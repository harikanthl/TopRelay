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
	"time"

	"github.com/harikanthl/toprelay/agent/internal/httpfwd"
	"github.com/harikanthl/toprelay/agent/internal/reqctx"
	"github.com/harikanthl/toprelay/agent/internal/shield"
)

// MaxBody is the largest request body accepted (the relay's cap).
const MaxBody = 20 << 20

// CallerHeader is the signed caller header (package caller); the proxy never forwards one it didn't make.
const CallerHeader = "X-Toprelay-Caller"

// RefusedHeader marks locally refused calls for the call log. It isn't on the relay's response allow-list, so it
// never leaves the agent.
const RefusedHeader = "X-Toprelay-Refused"

// ReasonHeader carries why a call was refused or failed locally, for the call log (never leaves the agent).
const ReasonHeader = "X-Toprelay-Reason"

// Reasons in the call log.
const (
	ReasonToolNotAllowed = "tool_not_allowed"
	ReasonInvalid        = "invalid_request"
	ReasonCredential     = "credential_unavailable"
	ReasonUnreachable    = "app_unreachable"
	ReasonRateLimited    = "rate_limited"
	// ReasonToolChanged: the tool's definition changed since it was approved (pins), so it is held.
	ReasonToolChanged = "tool_changed"
)

// Proxy is a filtering reverse proxy to a Streamable HTTP MCP endpoint. It serves /mcp and /mcp/… .
type Proxy struct {
	Service string
	Target  *url.URL
	// Transport reaches the upstream; nil uses a dedicated http.Transport.
	Transport http.RoundTripper
	// Allowed is the current tool policy. AllowedFor, if set, replaces it with a per-client policy (client is the
	// relay's X-Toprelay-Client).
	Allowed    Allowed
	AllowedFor func(client string) Allowed
	// OnHints receives the annotations of every tool in a tools/list result, before the result is filtered.
	OnHints func([]ToolHint)
	// Prepare, if set, readies each upstream request: the app's credential and the X-TopRelay-Caller signature
	// (upstream.Upstream.PrepareRelayed). body is the exact body sent.
	Prepare func(out *http.Request, body []byte) error
	// OnTools receives the names in every tools/list result (before filtering).
	OnTools func([]string)
	Log     *slog.Logger
	// EarlySSE is how long a tools/call waits for the app's headers before the agent starts an SSE reply itself
	// (0: DefaultEarlySSE; negative: never). KeepAlive is the longest an SSE reply goes without bytes
	// (0: DefaultKeepAlive).
	EarlySSE, KeepAlive time.Duration
	// Links, if set, turns toprelay-file URIs in tools/call results into public links. client is the relay's
	// X-Toprelay-Client.
	Links func(client, uri string) (Link, error)
	// Held, if set, reports tools held for review because their definition changed (Allowed already refuses
	// them); their refusals say so.
	Held func(tool string) bool
	// Shield, if set, returns the current shield settings, applied to every message from the app.
	Shield func() shield.Config
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
	// The tunnel decodes %2F and %2E, so clean the decoded path and refuse dot segments: /mcp/..%2Fadmin must
	// not become /mcp/../admin upstream.
	clean, ok := httpfwd.CleanPath(r.URL.Path)
	if !ok {
		return nil, false
	}
	sub, ok := strings.CutPrefix(clean, "/mcp")
	if !ok || (sub != "" && sub[0] != '/') {
		return nil, false
	}
	u := *p.Target
	u.Path = strings.TrimRight(p.Target.Path, "/") + sub
	if sub == "" {
		u.Path = p.Target.Path
	}
	u.RawPath = ""
	// The configured URL's own query (e.g. ?tenant=a) stays; the bot's is added after it.
	switch {
	case p.Target.RawQuery == "":
		u.RawQuery = r.URL.RawQuery
	case r.URL.RawQuery != "":
		u.RawQuery = p.Target.RawQuery + "&" + r.URL.RawQuery
	}
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
	policy := p.Allowed
	if p.AllowedFor != nil {
		policy = p.AllowedFor(r.Header.Get("X-Toprelay-Client"))
	}
	allowed := Restrict(policy, restriction, restricted)
	headerName := ""
	if strings.EqualFold(r.Header.Get("Mcp-Method"), "tools/call") {
		headerName = r.Header.Get("Mcp-Name")
	}
	var d Decision
	if r.Method == http.MethodPost {
		d = DecideHeld(body, headerName, p.Service, allowed, p.Held)
	} else {
		d = Decision{ListIDs: map[string]bool{}}
	}
	if d.Refuse {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(RefusedHeader, d.RefusedLabel())
		w.Header().Set(ReasonHeader, d.Reason())
		w.Header().Set("Content-Length", strconv.Itoa(len(d.Body)))
		w.WriteHeader(d.Status)
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
	// Only the agent may sign X-TopRelay-Caller; whatever arrived from the bot side is dropped.
	out.Header.Del(CallerHeader)
	if p.Prepare != nil {
		if err := p.Prepare(out, body); err != nil {
			if p.Log != nil {
				p.Log.Error("can't prepare the upstream request", "service", p.Service, "err", err)
			}
			w.Header().Set(ReasonHeader, ReasonCredential)
			p.answerUnavailable(w, d, fmt.Sprintf("%s: TopRelay can't get the credential for the local app right now. Ask its owner to run `toprelay doctor`.", p.Service), http.StatusBadGateway)
			return
		}
	}
	onSeen := func(seen []string) {
		if p.OnTools != nil && len(seen) > 0 {
			p.OnTools(seen)
		}
	}
	// The shield sees what the bot would (after the list filter) and runs before file links are minted.
	xf := Chain(ListTransformLearn(d.ListIDs, allowed, p.OnHints), p.shieldTransform(r), p.linkTransform(r, d))
	ch := make(chan roundTrip, 1)
	go func() {
		resp, err := p.transport().RoundTrip(out)
		ch <- roundTrip{resp, err}
	}()
	var rt roundTrip
	if after := p.earlyAfter(); after > 0 && earlyEligible(r, d) {
		t := time.NewTimer(after)
		select {
		case rt = <-ch:
			t.Stop()
		case <-t.C:
			p.serveEarly(w, r, d, ch, xf, onSeen)
			return
		}
	} else {
		rt = <-ch
	}
	resp, err := rt.resp, rt.err
	if err != nil {
		if r.Context().Err() != nil {
			panic(http.ErrAbortHandler)
		}
		// The bot gets no details: dial errors carry local paths, ports, pids and pin settings. They go to the log.
		msg := fmt.Sprintf("%s: the local MCP server isn't answering. Ask its owner to check that the app is running.", p.Service)
		if p.Log != nil {
			p.Log.Warn("upstream unreachable", "service", p.Service, "err", err)
		}
		w.Header().Set(ReasonHeader, ReasonUnreachable)
		p.answerUnavailable(w, d, msg, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = append([]string(nil), v...)
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case xf != nil && mt == "application/json":
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		fb, seen := TransformBody(b, xf)
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
		ka := newKeepAlive(w, p.keepAliveEvery())
		err := TransformSSE(ka, resp.Body, xf, onSeen)
		ka.stop()
		if err != nil && r.Context().Err() == nil {
			panic(http.ErrAbortHandler)
		}
	default:
		w.WriteHeader(resp.StatusCode)
		if err := copyFlush(w, resp.Body); err != nil && r.Context().Err() == nil {
			panic(http.ErrAbortHandler)
		}
	}
}

// answerUnavailable answers a single tools/call with a tool error the model can relay, and anything else with a
// JSON-RPC error.
func (p *Proxy) answerUnavailable(w http.ResponseWriter, d Decision, msg string, status int) {
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
	jsonRPCError(w, status, id, msg)
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

// shieldTransform applies the shield to each message from the app and records findings for the call log.
func (p *Proxy) shieldTransform(r *http.Request) Transform {
	if p.Shield == nil {
		return nil
	}
	cfg := p.Shield()
	if cfg.LeakMode() == shield.LeaksOff && cfg.InjectionMode() == shield.InjectionOff {
		return nil
	}
	found := reqctx.FindingsFrom(r.Context())
	return func(msg []byte) ([]byte, bool, []string) {
		out, changed, rep := cfg.InspectMessage(msg, p.Service)
		RecordShield(found, rep, p.Log, p.Service)
		return out, changed, nil
	}
}

// RecordShield adds a shield report to the request's findings and the agent log.
func RecordShield(found *reqctx.Findings, rep shield.Report, log *slog.Logger, service string) {
	if rep.Empty() {
		return
	}
	found.AddLeaks(rep.Leaks, rep.Blocked)
	for _, f := range rep.Injection {
		found.AddWarnings("injection:" + f.Rule)
	}
	if log == nil {
		return
	}
	if len(rep.Leaks) > 0 {
		action := "redacted"
		if rep.Blocked {
			action = "withheld"
		}
		log.Warn("credentials in the app's answer were "+action, "service", service, "rules", shield.Summary(rep.Leaks))
	}
	if len(rep.Injection) > 0 {
		log.Warn("the app's answer contains text that tries to instruct the model", "service", service, "rules", shield.Rules(rep.Injection))
	}
}
