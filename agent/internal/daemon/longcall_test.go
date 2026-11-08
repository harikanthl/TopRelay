package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/keys"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/service"
	"github.com/harikanthl/toprelay/agent/internal/testutil/fakerelay"
)

// longApps serves two slow tools: "render" (go-sdk, streams progress over SSE for dur) on /mcp, and a plain JSON
// app on /json that answers only after dur. cancelled receives once per call whose request was cancelled.
func longApps(t *testing.T, dur, every time.Duration, cancelled chan<- string) *httptest.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "slow", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "render", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			end := time.Now().Add(dur)
			for i := 0; time.Now().Before(end); i++ {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(every):
				}
				r.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: r.Params.GetProgressToken(),
					Progress: float64(i + 1), Total: float64(dur / every)})
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "rendered"}}}, nil
		})
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	mux := http.NewServeMux()
	// What the app sees at the HTTP level: the request's context ends when the bot hangs up.
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		finished := make(chan struct{})
		go func() {
			select {
			case <-r.Context().Done():
				select {
				case <-finished:
				default:
					report(cancelled, "render-http")
				}
			case <-finished:
			}
		}()
		sdk.ServeHTTP(w, r)
		close(finished)
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
			report(cancelled, "json")
			return
		case <-time.After(dur):
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"ocr done"}]}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// report sends without blocking: the agent's own tool discovery also gets cancelled now and then.
func report(ch chan<- string, what string) {
	select {
	case ch <- what:
	default:
	}
}

type timedStream struct {
	head    time.Duration // until the response headers
	maxGap  time.Duration // longest time with no bytes
	events  []string
	status  int
	ctype   string
	elapsed time.Duration
}

// botCall POSTs one JSON-RPC body through the relay as a bot would and times the reply.
func botCall(t *testing.T, ctx context.Context, url, key, body string) timedStream {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	ts := timedStream{head: time.Since(start), status: resp.StatusCode, ctype: resp.Header.Get("Content-Type")}
	last := time.Now()
	br := bufio.NewReader(resp.Body)
	var ev strings.Builder
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			ts.maxGap = max(ts.maxGap, time.Since(last))
			last = time.Now()
			if strings.TrimSpace(line) == "" {
				if ev.Len() > 0 {
					ts.events = append(ts.events, strings.TrimSpace(ev.String()))
					ev.Reset()
				}
			} else {
				ev.WriteString(line)
			}
		}
		if err != nil {
			if ev.Len() > 0 {
				ts.events = append(ts.events, strings.TrimSpace(ev.String()))
			}
			break
		}
	}
	ts.elapsed = time.Since(start)
	return ts
}

type longEnv struct {
	relay  *fakerelay.Relay
	key    string
	cancel context.CancelFunc
}

func startLong(t *testing.T, app *httptest.Server, tun service.Tuning) *longEnv {
	dir := t.TempDir()
	relay := fakerelay.New(t, fakerelay.Service{Name: "slow", Kind: "mcp", Gen: 1}, fakerelay.Service{Name: "ocr", Kind: "mcp", Gen: 1})
	store := &secrets.File{Path: filepath.Join(dir, "secrets.json")}
	store.Set(secrets.AgentRoot, relay.AgentRoot)
	store.Set(secrets.BearerRoot, relay.BearerRoot)
	all := config.ToolPolicy{Default: config.DefaultAllow}
	config.Save(dir, &config.Config{PublicURL: "https://unused.example", Services: []config.Service{
		{Name: "slow", Kind: "mcp", Gen: 1, Source: config.Source{Type: config.SourceMCPURL, URL: app.URL + "/mcp"}, Tools: all},
		{Name: "ocr", Kind: "mcp", Gen: 1, Source: config.Source{Type: config.SourceMCPURL, URL: app.URL + "/json"}, Tools: all},
	}})
	d := &Daemon{Dir: dir, Store: store, RelayURL: relay.URL(), PollInterval: 50 * time.Millisecond, Tuning: tun}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	relay.WaitConnected(t, "slow", 1)
	relay.WaitConnected(t, "ocr", 1)
	id, _ := keys.NewKeyID()
	b, _ := keys.Mint(relay.BearerRoot, id, keys.Caveats("slow", 1, nil, time.Time{}))
	return &longEnv{relay: relay, key: b.String(), cancel: cancel}
}

func (e *longEnv) keyFor(service string) string {
	id, _ := keys.NewKeyID()
	b, _ := keys.Mint(e.relay.BearerRoot, id, keys.Caveats(service, 1, nil, time.Time{}))
	return b.String()
}

const (
	renderCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"render","arguments":{},"_meta":{"progressToken":"p1"}}}`
	ocrCall    = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ocr","arguments":{}}}`
)

// checkLong checks a long call end to end: headers well before the relay's 120 s limit, no silence longer than
// maxGap (the relay ends streams idle for 300 s), progress delivered, and the final result last.
func checkLong(t *testing.T, name string, ts timedStream, headBy, maxGap time.Duration, wantProgress bool, final string) {
	t.Helper()
	if ts.status != 200 || ts.ctype != "text/event-stream" {
		t.Fatalf("%s: %d %s", name, ts.status, ts.ctype)
	}
	if ts.head > headBy {
		t.Errorf("%s: headers after %s (want ≤ %s)", name, ts.head, headBy)
	}
	if ts.maxGap > maxGap {
		t.Errorf("%s: %s without a byte (want ≤ %s)", name, ts.maxGap, maxGap)
	}
	progress := 0
	for _, e := range ts.events {
		if strings.Contains(e, "notifications/progress") {
			progress++
		}
	}
	if wantProgress && progress < 2 {
		t.Errorf("%s: %d progress notifications: %q", name, progress, ts.events)
	}
	if len(ts.events) == 0 || !strings.Contains(ts.events[len(ts.events)-1], final) {
		t.Errorf("%s: last event isn't the result: %q", name, ts.events)
	}
}

// Long calls, scaled down: a streaming tool and a JSON tool each take 3 s, with the early-SSE and keep-alive
// timings scaled to match.
func TestLongToolCalls(t *testing.T) {
	cancelled := make(chan string, 4)
	app := longApps(t, 3*time.Second, 400*time.Millisecond, cancelled)
	e := startLong(t, app, service.Tuning{EarlySSE: 300 * time.Millisecond, KeepAlive: 200 * time.Millisecond})
	ctx := context.Background()
	results := make(chan [2]any, 2)
	go func() {
		results <- [2]any{"render", botCall(t, ctx, e.relay.URL()+"/slow/mcp", e.keyFor("slow"), renderCall)}
	}()
	go func() { results <- [2]any{"ocr", botCall(t, ctx, e.relay.URL()+"/ocr/mcp", e.keyFor("ocr"), ocrCall)} }()
	for range 2 {
		r := <-results
		ts := r[1].(timedStream)
		if r[0] == "render" {
			checkLong(t, "render", ts, time.Second, 900*time.Millisecond, true, "rendered")
		} else {
			checkLong(t, "ocr", ts, time.Second, 900*time.Millisecond, false, "ocr done")
		}
		t.Logf("%s: headers %s, gap %s, total %s", r[0], ts.head, ts.maxGap, ts.elapsed)
		if ts.elapsed < 3*time.Second {
			t.Errorf("%s finished in %s, before the tool did", r[0], ts.elapsed)
		}
	}
}

// The bot hangs up: the app's request (and the go-sdk tool's context) is cancelled.
func TestLongCallCancelReachesApp(t *testing.T) {
	cancelled := make(chan string, 4)
	// go-sdk v1.8's stateless handler doesn't cancel a tool's context when the HTTP request ends, so keep the tool
	// short: cleanup waits for it.
	app := longApps(t, 4*time.Second, 200*time.Millisecond, cancelled)
	e := startLong(t, app, service.Tuning{EarlySSE: 200 * time.Millisecond, KeepAlive: 100 * time.Millisecond})
	for _, c := range []struct{ svc, body, want string }{{"ocr", ocrCall, "json"}, {"slow", renderCall, "render-http"}} {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(time.Second); cancel() }()
		req, _ := http.NewRequestWithContext(ctx, "POST", e.relay.URL()+"/"+c.svc+"/mcp", strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer "+e.keyFor(c.svc))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		select {
		case got := <-cancelled:
			if got != c.want {
				t.Errorf("cancelled %s, want %s", got, c.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the app never saw the bot hang up", c.svc)
		}
	}
}

// The real thing: 3-minute tools with the production timings. Slow, so only with TOPRELAY_LONG_TESTS=1.
func TestThreeMinuteToolCalls(t *testing.T) {
	if os.Getenv("TOPRELAY_LONG_TESTS") != "1" {
		t.Skip("set TOPRELAY_LONG_TESTS=1 to run the 3-minute tool test")
	}
	cancelled := make(chan string, 4)
	app := longApps(t, 3*time.Minute, 70*time.Second, cancelled) // progress rarer than the keep-alive
	e := startLong(t, app, service.Tuning{})
	ctx := context.Background()
	results := make(chan [2]any, 2)
	go func() {
		results <- [2]any{"render", botCall(t, ctx, e.relay.URL()+"/slow/mcp", e.keyFor("slow"), renderCall)}
	}()
	go func() { results <- [2]any{"ocr", botCall(t, ctx, e.relay.URL()+"/ocr/mcp", e.keyFor("ocr"), ocrCall)} }()
	for range 2 {
		r := <-results
		ts := r[1].(timedStream)
		// The relay's limits: headers within 120 s, no 300 s silence. Stay far inside them.
		checkLong(t, r[0].(string), ts, 25*time.Second, 20*time.Second, r[0] == "render", map[any]string{"render": "rendered", "ocr": "ocr done"}[r[0]])
		t.Logf("%s: headers after %s, longest silence %s, %d events, %s total", r[0], ts.head.Round(time.Millisecond), ts.maxGap.Round(time.Millisecond), len(ts.events), ts.elapsed.Round(time.Second))
	}
}
