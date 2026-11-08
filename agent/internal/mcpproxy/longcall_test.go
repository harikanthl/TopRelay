package mcpproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func longProxy(t *testing.T, up http.HandlerFunc) *Proxy {
	t.Helper()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL + "/mcp")
	return &Proxy{Service: "notes", Target: u, Allowed: func(string) bool { return true },
		EarlySSE: 100 * time.Millisecond, KeepAlive: 40 * time.Millisecond}
}

// sseEvents splits an SSE body into events (comments included as their own entries).
func sseEvents(body string) []string {
	var out []string
	for _, e := range strings.Split(body, "\n\n") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

const slowCall = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"render"}}`

func TestEarlySSEForSlowJSON(t *testing.T) {
	p := longProxy(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{\n  \"jsonrpc\": \"2.0\", \"id\": 7,\n  \"result\": {\"content\": [{\"type\":\"text\",\"text\":\"done\"}]}\n}")
	})
	start := time.Now()
	w := post(p, context.Background(), slowCall, nil)
	if w.Header().Get("Content-Type") != "text/event-stream" || w.Code != 200 {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	ev := sseEvents(w.Body.String())
	if len(ev) < 3 || ev[0] != ": keepalive" {
		t.Fatalf("want keep-alives before the answer: %q", ev)
	}
	last := ev[len(ev)-1]
	data, ok := strings.CutPrefix(last, "event: message\ndata: ")
	if !ok || strings.Contains(data, "\n") {
		t.Fatalf("last event %q", last)
	}
	var m map[string]any
	if json.Unmarshal([]byte(data), &m) != nil || m["id"] != float64(7) || m["result"] == nil {
		t.Fatalf("answer %s", data)
	}
	if time.Since(start) < 400*time.Millisecond {
		t.Fatal("returned before the app answered")
	}
}

func TestEarlySSEOnlyWhenAllowed(t *testing.T) {
	p := longProxy(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":7,"result":{}}`)
	})
	// A client that doesn't accept SSE waits for plain JSON.
	w := post(p, context.Background(), slowCall, map[string]string{"Accept": "application/json"})
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("JSON-only client got %v", w.Header())
	}
	// Not a tools/call (session setup must keep the app's headers, like Mcp-Session-Id).
	w = post(p, context.Background(), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil)
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("initialize got %v", w.Header())
	}
	// A notification (no id) has no answer to deliver.
	w = post(p, context.Background(), `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"x"}}`, nil)
	if w.Header().Get("Content-Type") == "text/event-stream" {
		t.Fatal("notification upgraded")
	}
	// Turned off.
	p.EarlySSE = -1
	w = post(p, context.Background(), slowCall, nil)
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("EarlySSE < 0 still upgraded")
	}
}

func TestEarlySSEAppFailures(t *testing.T) {
	p := longProxy(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	w := post(p, context.Background(), slowCall, nil)
	ev := sseEvents(w.Body.String())
	last := ev[len(ev)-1]
	if !strings.Contains(last, `"isError":true`) || !strings.Contains(last, "HTTP 500") || !strings.Contains(last, `"id":7`) {
		t.Fatalf("app error: %q", last)
	}

	// The app dies mid-call: a tool error, not a hang.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		c.Close()
	}))
	defer srv.Close()
	p.Target, _ = url.Parse(srv.URL + "/mcp")
	w = post(p, context.Background(), slowCall, nil)
	ev = sseEvents(w.Body.String())
	if last := ev[len(ev)-1]; !strings.Contains(last, `"isError":true`) || !strings.Contains(last, "stopped answering") {
		t.Fatalf("app died: %q", last)
	}
}

// An SSE reply with long gaps gets comments between events, never inside one.
func TestKeepAliveBetweenEvents(t *testing.T) {
	p := longProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := range 3 {
			// Half an event, a pause, then the rest: a comment must not land in the middle.
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":%d}}\n", i)
			fl.Flush()
			time.Sleep(120 * time.Millisecond)
			io.WriteString(w, "\n")
			fl.Flush()
		}
		io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}\n\n")
	})
	p.EarlySSE = -1
	w := post(p, context.Background(), slowCall, nil)
	ev := sseEvents(w.Body.String())
	progress, comments := 0, 0
	for _, e := range ev {
		switch {
		case e == ": keepalive":
			comments++
		case strings.HasPrefix(e, "event: message\ndata: ") && !strings.Contains(e, ":keepalive") && !strings.Contains(e, ": keepalive"):
			if strings.Contains(e, "progress") {
				progress++
			}
		default:
			t.Fatalf("corrupted event %q", e)
		}
	}
	if progress != 3 || comments == 0 || !strings.Contains(ev[len(ev)-1], `"id":7`) {
		t.Fatalf("progress %d comments %d: %q", progress, comments, ev)
	}
}

// The bot goes away: the app's request is cancelled.
func TestCancelReachesApp(t *testing.T) {
	var mu sync.Mutex
	cancelled := make(chan struct{})
	p := longProxy(t, func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body) // Go's server notices a dropped client only once the body is read
		<-r.Context().Done()
		mu.Lock()
		close(cancelled)
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	func() {
		defer func() { recover() }() // the proxy aborts the handler
		post(p, ctx, slowCall, nil)
	}()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the app never saw the cancellation")
	}
}
