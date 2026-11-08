package mcpproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Defaults for long calls. The relay gives up on a reply whose headers haven't arrived in 120 s and ends a
// stream with no bytes for 300 s (docs/protocol.md "Limits"); these keep a slow tool well inside both.
const (
	// DefaultEarlySSE is how long a tools/call may wait for the app's response headers before the agent starts
	// an SSE reply itself (when the client accepts SSE).
	DefaultEarlySSE = 20 * time.Second
	// DefaultKeepAlive is the longest an SSE reply goes without bytes: the agent writes an SSE comment.
	DefaultKeepAlive = 15 * time.Second
	// MaxLateJSON caps a JSON answer delivered as an SSE event after an early start.
	MaxLateJSON = 20 << 20
)

// keepAlive serialises writes to an SSE response and writes ": keepalive" comments when nothing else has been
// written for a while. Callers must write whole events: comments only ever go between writes.
type keepAlive struct {
	mu    sync.Mutex
	w     http.ResponseWriter
	last  time.Time
	err   error
	stopc chan struct{}
	done  chan struct{}
}

func newKeepAlive(w http.ResponseWriter, every time.Duration) *keepAlive {
	k := &keepAlive{w: w, last: time.Now(), stopc: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(k.done)
		// Checked every fifth of the interval: the longest silence is every × 1.2.
		t := time.NewTicker(max(every/5, 10*time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-k.stopc:
				return
			case <-t.C:
				k.mu.Lock()
				if k.err == nil && time.Since(k.last) >= every {
					k.writeLocked([]byte(": keepalive\n\n"))
				}
				k.mu.Unlock()
			}
		}
	}()
	return k
}

func (k *keepAlive) writeLocked(p []byte) (int, error) {
	if k.err != nil {
		return 0, k.err
	}
	n, err := k.w.Write(p)
	if err != nil {
		k.err = err
		return n, err
	}
	if f, ok := k.w.(http.Flusher); ok {
		f.Flush()
	}
	k.last = time.Now()
	return n, nil
}

func (k *keepAlive) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.writeLocked(p)
}

// Flush is a no-op: every write is flushed.
func (k *keepAlive) Flush() {}

// stop ends the comments; no write happens after it returns.
func (k *keepAlive) stop() {
	close(k.stopc)
	<-k.done
}

// earlyEligible reports whether a request may get an early SSE reply: a POST of one tools/call request (with an
// id) from a client that accepts SSE, as Streamable HTTP requires clients to.
func earlyEligible(r *http.Request, d Decision) bool {
	if r.Method != http.MethodPost || len(d.Msgs) != 1 || d.Msgs[0].Method != "tools/call" {
		return false
	}
	id := bytes.TrimSpace(d.Msgs[0].ID)
	if len(id) == 0 || string(id) == "null" {
		return false
	}
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		if mt, _, err := mime.ParseMediaType(strings.TrimSpace(part)); err == nil && mt == "text/event-stream" {
			return true
		}
	}
	return false
}

type roundTrip struct {
	resp *http.Response
	err  error
}

func sseEvent(w io.Writer, payload []byte) error {
	_, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
	return err
}

// serveEarly answers a slow tools/call as SSE before the app has answered: headers now, keep-alive comments
// while waiting, then the app's answer as an SSE message event (or its SSE stream, passed through).
func (p *Proxy) serveEarly(w http.ResponseWriter, r *http.Request, d Decision, ch <-chan roundTrip, xf Transform, onSeen func([]string)) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Del("Content-Length")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	ka := newKeepAlive(w, p.keepAliveEvery())
	defer ka.stop()
	if p.Log != nil {
		p.Log.Debug("slow tool call: started an SSE reply early", "service", p.Service)
	}
	id := d.Msgs[0].ID
	toolErr := func(msg string) {
		b, _ := json.Marshal(response{JSONRPC: "2.0", ID: idOrNull(id), Result: toolResult{Content: []textContent{{Type: "text", Text: msg}}, IsError: true}})
		sseEvent(ka, b)
	}
	res := <-ch
	if res.err != nil {
		if r.Context().Err() != nil {
			panic(http.ErrAbortHandler)
		}
		if p.Log != nil {
			p.Log.Warn("upstream failed during a long call", "service", p.Service, "err", res.err)
		}
		w.Header().Set(ReasonHeader, ReasonUnreachable)
		toolErr(fmt.Sprintf("%s: the local MCP server stopped answering. Ask its owner to check that the app is running.", p.Service))
		return
	}
	defer res.resp.Body.Close()
	mt, _, _ := mime.ParseMediaType(res.resp.Header.Get("Content-Type"))
	switch mt {
	case "text/event-stream":
		if err := TransformSSE(ka, res.resp.Body, xf, onSeen); err != nil && r.Context().Err() == nil {
			panic(http.ErrAbortHandler)
		}
	case "application/json":
		b, err := io.ReadAll(io.LimitReader(res.resp.Body, MaxLateJSON+1))
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		var compact bytes.Buffer
		if len(b) > MaxLateJSON || json.Compact(&compact, b) != nil {
			toolErr(fmt.Sprintf("%s: the local app's answer was not valid JSON or was over 20 MiB.", p.Service))
			return
		}
		out, _ := TransformBody(compact.Bytes(), xf)
		sseEvent(ka, out)
	default:
		toolErr(fmt.Sprintf("%s: the local app answered HTTP %d (%s) instead of a JSON-RPC result.", p.Service, res.resp.StatusCode, mt))
	}
}

func (p *Proxy) earlyAfter() time.Duration {
	if p.EarlySSE != 0 {
		return p.EarlySSE
	}
	return DefaultEarlySSE
}

func (p *Proxy) keepAliveEvery() time.Duration {
	if p.KeepAlive > 0 {
		return p.KeepAlive
	}
	return DefaultKeepAlive
}

// linkTransform is the file-link rewrite for this request's tools/call ids, or nil.
func (p *Proxy) linkTransform(r *http.Request, d Decision) Transform {
	if p.Links == nil {
		return nil
	}
	client := r.Header.Get("X-Toprelay-Client")
	return LinkTransform(d.CallIDs, func(uri string) (Link, error) { return p.Links(client, uri) })
}
