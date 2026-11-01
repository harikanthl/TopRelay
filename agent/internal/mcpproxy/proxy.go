package mcpproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/harikanthl/toprelay/agent/internal/reqctx"
)

// MaxBody is the largest request body accepted (the relay's cap).
const MaxBody = 20 << 20

// RefusedHeader marks locally refused calls for the call log. It isn't on the relay's response allow-list, so it
// never leaves the agent.
const RefusedHeader = "X-Toprelay-Refused"

// Proxy is a filtering reverse proxy to a Streamable HTTP MCP endpoint. It serves /mcp and /mcp/… .
type Proxy struct {
	Service string
	Target  *url.URL
	// Transport reaches the upstream; nil uses a dedicated http.Transport.
	Transport http.RoundTripper
	// Allowed is the current tool policy.
	Allowed Allowed
	// Upstream header added to every request (from the keyring), if Name is set.
	HeaderName, HeaderValue string
	// OnTools receives the names in every tools/list result (before filtering).
	OnTools func([]string)
	Log     *slog.Logger
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *Proxy) transport() http.RoundTripper {
	if p.Transport != nil {
		return p.Transport
	}
	p.Transport = &http.Transport{Proxy: nil, DisableCompression: true, MaxIdleConnsPerHost: 16}
	return p.Transport
}

func (p *Proxy) upstreamURL(r *http.Request) (*url.URL, bool) {
	sub, ok := strings.CutPrefix(r.URL.Path, "/mcp")
	if !ok || (sub != "" && sub[0] != '/') {
		return nil, false
	}
	u := *p.Target
	u.Path = strings.TrimRight(p.Target.Path, "/") + sub
	if sub == "" {
		u.Path = p.Target.Path
	}
	u.RawPath = ""
	u.RawQuery = r.URL.RawQuery
	return &u, true
}

func jsonRPCError(w http.ResponseWriter, status int, id json.RawMessage, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(response{JSONRPC: "2.0", ID: idOrNull(id), Error: map[string]any{"code": -32000, "message": msg}})
	w.Write(b)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target, ok := p.upstreamURL(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
		if err != nil {
			http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(b) > MaxBody {
			http.Error(w, "request body over 20 MiB", http.StatusRequestEntityTooLarge)
			return
		}
		body = b
	}
	restriction, restricted := reqctx.Tools(r.Context())
	allowed := Restrict(p.Allowed, restriction, restricted)
	headerName := ""
	if strings.EqualFold(r.Header.Get("Mcp-Method"), "tools/call") {
		headerName = r.Header.Get("Mcp-Name")
	}
	var d Decision
	if r.Method == http.MethodPost {
		d = Decide(body, headerName, p.Service, allowed)
	} else {
		d = Decision{ListIDs: map[string]bool{}}
	}
	if d.Refuse {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(RefusedHeader, strings.Join(d.Tools, ","))
		w.Header().Set("Content-Length", strconv.Itoa(len(d.Body)))
		w.WriteHeader(http.StatusOK)
		w.Write(d.Body)
		return
	}

	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for k, v := range r.Header {
		out.Header[k] = append([]string(nil), v...)
	}
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	out.Header.Del("Accept-Encoding") // the filter needs plain bodies
	out.ContentLength = int64(len(body))
	if len(body) == 0 && r.Method != http.MethodPost && r.Method != http.MethodPut {
		out.Body = http.NoBody
	}
	if p.HeaderName != "" {
		out.Header.Set(p.HeaderName, p.HeaderValue)
	}
	resp, err := p.transport().RoundTrip(out)
	if err != nil {
		if r.Context().Err() != nil {
			panic(http.ErrAbortHandler)
		}
		msg := fmt.Sprintf("%s: the local MCP server isn't answering (%v). Ask its owner to check that the app is running.", p.Service, err)
		if p.Log != nil {
			p.Log.Warn("upstream unreachable", "service", p.Service, "err", err)
		}
		var id json.RawMessage
		isCall := false
		if len(d.Msgs) == 1 {
			id, isCall = d.Msgs[0].ID, d.Msgs[0].Method == "tools/call"
		}
		if isCall {
			b, _ := json.Marshal(response{JSONRPC: "2.0", ID: idOrNull(id), Result: toolResult{Content: []textContent{{Type: "text", Text: msg}}, IsError: true}})
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
			return
		}
		jsonRPCError(w, http.StatusBadGateway, id, msg)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = append([]string(nil), v...)
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	onSeen := func(seen []string) {
		if p.OnTools != nil && len(seen) > 0 {
			p.OnTools(seen)
		}
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case len(d.ListIDs) > 0 && mt == "application/json":
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		fb, seen := FilterBody(b, d.ListIDs, allowed)
		onSeen(seen)
		w.Header().Set("Content-Length", strconv.Itoa(len(fb)))
		w.WriteHeader(resp.StatusCode)
		w.Write(fb)
	case mt == "text/event-stream":
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if err := FilterSSE(w, resp.Body, d.ListIDs, allowed, onSeen); err != nil && r.Context().Err() == nil {
			panic(http.ErrAbortHandler)
		}
	default:
		w.WriteHeader(resp.StatusCode)
		if err := copyFlush(w, resp.Body); err != nil && r.Context().Err() == nil {
			panic(http.ErrAbortHandler)
		}
	}
}

func copyFlush(w io.Writer, r io.Reader) error {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
