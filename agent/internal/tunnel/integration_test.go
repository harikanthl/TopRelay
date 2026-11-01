package tunnel_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/keys"
	"github.com/harikanthl/toprelay/agent/internal/reqctx"
	"github.com/harikanthl/toprelay/agent/internal/testutil/fakerelay"
	"github.com/harikanthl/toprelay/agent/internal/tunnel"
)

type harness struct {
	relay  *fakerelay.Relay
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, h http.Handler, mod func(*tunnel.Options)) *harness {
	t.Helper()
	r := fakerelay.New(t, fakerelay.Service{Name: "notes", Kind: "mcp", Gen: 1})
	key, err := keys.AgentKey(r.AgentRoot, "notes", 1)
	if err != nil {
		t.Fatal(err)
	}
	o := tunnel.Options{BaseURL: r.URL(), Service: "notes", AgentKey: key, Handler: h,
		Hello:  func() tunnel.Hello { return tunnel.Hello{Kind: "mcp", Tools: []string{"search"}} },
		Jitter: func(d time.Duration) time.Duration { return d / 50 }}
	if mod != nil {
		mod(&o)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hs := &harness{relay: r, cancel: cancel, done: make(chan error, 1)}
	c := tunnel.New(o)
	go func() { hs.done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-hs.done:
		case <-time.After(5 * time.Second):
			t.Error("tunnel didn't stop")
		}
	})
	r.WaitConnected(t, "notes", 1)
	return hs
}

func TestJSONAndSSE(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Mcp-Session-Id", "s1")
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, "data: %d\n\n", i)
				w.(http.Flusher).Flush()
				time.Sleep(30 * time.Millisecond)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "no=1")
		fmt.Fprintf(w, `{"echo":%q,"q":%q,"ip":%q,"host":%q}`, body, r.URL.RawQuery, r.RemoteAddr, r.Host)
	})
	hs := start(t, mux, nil)
	h := hs.relay.Hellos("notes")
	if len(h) != 1 || h[0].Tools == nil || strings.Join(*h[0].Tools, ",") != "search" || h[0].Kind != "mcp" {
		t.Fatalf("hello %+v", h)
	}
	resp, err := hs.relay.Do("notes", fakerelay.Request{Path: "/mcp", Query: "a=1", Body: []byte(`{"x":1}`),
		Headers: map[string]string{"content-type": "application/json", "cf-connecting-ip": "203.0.113.9", "x-forwarded-host": "relay.example"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Stream || resp.Status != 200 || resp.Headers["content-type"] != "application/json" || resp.Headers["set-cookie"] != "" {
		t.Fatalf("json resp %+v", resp)
	}
	if string(resp.Body) != `{"echo":"{\"x\":1}","q":"a=1","ip":"203.0.113.9:0","host":"relay.example"}` {
		t.Fatalf("body %s", resp.Body)
	}

	resp, err = hs.relay.Do("notes", fakerelay.Request{Path: "/mcp", Method: "GET", Headers: map[string]string{"accept": "text/event-stream"}})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Stream || resp.Headers["mcp-session-id"] != "s1" || string(resp.Body) != "data: 0\n\ndata: 1\n\ndata: 2\n\n" {
		t.Fatalf("sse %+v %q", resp, resp.Body)
	}
	if len(resp.Chunks) < 2 {
		t.Fatalf("events 30 ms apart should not be merged into one chunk: %d", len(resp.Chunks))
	}
}

func TestConcurrent(t *testing.T) {
	var inflight, peak atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
		inflight.Add(-1)
		io.WriteString(w, r.URL.Query().Get("n"))
	})
	hs := start(t, h, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	begin := time.Now()
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := hs.relay.Do("notes", fakerelay.Request{Path: "/mcp", Query: fmt.Sprintf("n=%d", i)})
			if err != nil {
				errs <- err
				return
			}
			if string(resp.Body) != fmt.Sprint(i) {
				errs <- fmt.Errorf("request %d got %q", i, resp.Body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if peak.Load() < 10 || time.Since(begin) > 1500*time.Millisecond {
		t.Fatalf("not concurrent: peak %d, took %v", peak.Load(), time.Since(begin))
	}
}

func TestCancelReachesUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	started := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	hs := start(t, h, nil)
	x, err := hs.relay.Send("notes", fakerelay.Request{Path: "/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := x.Cancel(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel didn't reach the handler")
	}
}

func TestToolsRestrictionInContext(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tools, ok := reqctx.Tools(r.Context())
		fmt.Fprintf(w, "%v %s", ok, strings.Join(tools, ","))
	})
	hs := start(t, h, nil)
	resp, _ := hs.relay.Do("notes", fakerelay.Request{Path: "/mcp"})
	if string(resp.Body) != "false " {
		t.Fatalf("unrestricted: %q", resp.Body)
	}
	tools := []string{"a", "b"}
	resp, _ = hs.relay.Do("notes", fakerelay.Request{Path: "/mcp", Tools: &tools})
	if string(resp.Body) != "true a,b" {
		t.Fatalf("restricted: %q", resp.Body)
	}
}

func TestDropFailsPendingAndReconnects(t *testing.T) {
	handlerDone := make(chan struct{})
	started := make(chan struct{}, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			started <- struct{}{}
			<-r.Context().Done()
			close(handlerDone)
			return
		}
		io.WriteString(w, "ok")
	})
	var states []bool
	var smu sync.Mutex
	hs := start(t, h, func(o *tunnel.Options) {
		o.OnState = func(s tunnel.State) { smu.Lock(); states = append(states, s.Connected); smu.Unlock() }
	})
	x, err := hs.relay.Send("notes", fakerelay.Request{Path: "/slow"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	hs.relay.Drop("notes")
	resp, err := x.Collect(3 * time.Second)
	if err != nil || !resp.Dropped {
		t.Fatalf("pending request should fail on drop: %+v %v", resp, err)
	}
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the local request wasn't cancelled when the socket dropped")
	}
	hs.relay.WaitConnected(t, "notes", 2)
	resp, err = hs.relay.Do("notes", fakerelay.Request{Path: "/ok"})
	if err != nil || string(resp.Body) != "ok" {
		t.Fatalf("after reconnect: %+v %v", resp, err)
	}
	smu.Lock()
	defer smu.Unlock()
	if len(states) < 3 || !states[0] || states[1] || !states[2] {
		t.Fatalf("states %v", states)
	}
}

func TestReplaced4000(t *testing.T) {
	hs := start(t, http.NotFoundHandler(), nil)
	// A second agent (another computer) connects the same service: the first gets 4000 and keeps retrying.
	key, _ := keys.AgentKey(hs.relay.AgentRoot, "notes", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	other := tunnel.New(tunnel.Options{BaseURL: hs.relay.URL(), Service: "notes", AgentKey: key, Handler: http.NotFoundHandler(),
		Jitter: func(d time.Duration) time.Duration { return d / 50 }})
	go other.Run(ctx)
	hs.relay.WaitConnected(t, "notes", 3) // first, other, first again
	if hs.relay.Connects("notes") < 3 {
		t.Fatal("the replaced agent didn't retry")
	}
}

func TestPongTimeoutReconnects(t *testing.T) {
	hs := start(t, http.NotFoundHandler(), func(o *tunnel.Options) {
		o.PingInterval = 50 * time.Millisecond
		o.PongTimeout = 50 * time.Millisecond
	})
	hs.relay.NoPong.Store(true)
	hs.relay.WaitConnected(t, "notes", 2)
	hs.relay.NoPong.Store(false)
}

func TestHelloResent(t *testing.T) {
	var mu sync.Mutex
	tools := []string{"a"}
	changed := make(chan struct{}, 1)
	hs := start(t, http.NotFoundHandler(), func(o *tunnel.Options) {
		o.Hello = func() tunnel.Hello { mu.Lock(); defer mu.Unlock(); return tunnel.Hello{Kind: "mcp", Tools: tools} }
		o.HelloChanged = changed
	})
	mu.Lock()
	tools = []string{"a", "b"}
	mu.Unlock()
	changed <- struct{}{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h := hs.relay.Hellos("notes")
		if len(h) == 2 && strings.Join(*h[1].Tools, ",") == "a,b" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("hello not resent: %+v", hs.relay.Hellos("notes"))
}

func TestBadAgentKey(t *testing.T) {
	r := fakerelay.New(t, fakerelay.Service{Name: "notes", Kind: "mcp", Gen: 2})
	key, _ := keys.AgentKey(r.AgentRoot, "notes", 1)
	var last atomic.Value
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c := tunnel.New(tunnel.Options{BaseURL: r.URL(), Service: "notes", AgentKey: key, Handler: http.NotFoundHandler(),
		Jitter: func(time.Duration) time.Duration { return time.Millisecond },
		OnState: func(s tunnel.State) {
			if s.Err != nil {
				last.Store(s.Err.Error())
			}
		}})
	c.Run(ctx)
	if v, _ := last.Load().(string); !strings.Contains(v, "rejected the agent key") {
		t.Fatalf("last error %q", v)
	}
}
