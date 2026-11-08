package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/caller"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/testutil/fakerelay"
	"github.com/harikanthl/toprelay/agent/internal/upstream"
)

// seen is one request as the local app received it.
type seen struct {
	Host, Target, Method string
	Header               http.Header
	Body                 []byte
}

// recorder is a local app that remembers the last relayed request (one with X-Forwarded-Host) and answers
// JSON-RPC.
type recorder struct {
	mu   sync.Mutex
	last *seen
	srv  *httptest.Server
}

func newRecorder(t *testing.T) *recorder {
	rec := &recorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Forwarded-Host") != "" {
			rec.mu.Lock()
			rec.last = &seen{Host: r.Host, Target: r.RequestURI, Method: r.Method, Header: r.Header.Clone(), Body: b}
			rec.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recorder) take() *seen {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.last
	r.last = nil
	return s
}

func writeSecret(t *testing.T, path, v string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// What the local app sees (docs/protocol.md "Agent → local app"): its own Host, the relay's forwarded headers,
// its credential, and one X-TopRelay-Caller that verifies with the service's caller key, even when the frame
// carried a forged one. Credential changes apply live, without a reconnect.
func TestUpstreamContract(t *testing.T) {
	dir := t.TempDir()
	relay := fakerelay.New(t, fakerelay.Service{Name: "notes", Kind: "mcp", Gen: 1}, fakerelay.Service{Name: "web", Kind: "http", Gen: 1})
	store := &secrets.File{Path: filepath.Join(dir, "secrets.json")}
	store.Set(secrets.AgentRoot, relay.AgentRoot)
	store.Set(secrets.BearerRoot, relay.BearerRoot)
	store.Set(secrets.UpstreamHeader("web"), "web-v1")
	app := newRecorder(t)
	appURL, _ := url.Parse(app.srv.URL)
	tokFile := filepath.Join(dir, "notes-token")
	writeSecret(t, tokFile, "notes-v1")
	cfg := &config.Config{PublicURL: "https://unused.example", Services: []config.Service{
		{Name: "notes", Kind: "mcp", Gen: 1, Source: config.Source{Type: config.SourceMCPURL, URL: app.srv.URL + "/mcp"},
			Tools: config.ToolPolicy{Default: config.DefaultAllow}, UpstreamHeader: "X-App-Token", UpstreamHeaderFile: tokFile},
		{Name: "web", Kind: "http", Gen: 1, Source: config.Source{Type: config.SourceHTTP, URL: app.srv.URL}, AllowPaths: []string{"/"},
			Tools: config.ToolPolicy{Default: config.DefaultAllow}, UpstreamHeader: "X-Web-Key"},
	}}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Dir: dir, Store: store, RelayURL: relay.URL(), PollInterval: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	relay.WaitConnected(t, "notes", 1)
	relay.WaitConnected(t, "web", 1)

	frameHeaders := func() map[string]string {
		return map[string]string{"content-type": "application/json", "accept": "application/json, text/event-stream",
			"x-forwarded-host": "toprelay.hari.workers.dev", "x-forwarded-proto": "https", "x-forwarded-for": "203.0.113.7",
			"cf-connecting-ip": "203.0.113.7", "x-toprelay-client": "https://claude.ai/oauth/client",
			"x-toprelay-caller": "v1; client=local-liar; ts=1; nonce=00; sig=00"}
	}
	verify := func(service string, s *seen, wantCred, credHeader string) {
		t.Helper()
		if s == nil {
			t.Fatalf("%s: the app saw no relayed request", service)
		}
		if s.Host != appURL.Host {
			t.Errorf("%s: Host %q, want the app's own %q", service, s.Host, appURL.Host)
		}
		if s.Header.Get("X-Forwarded-Host") != "toprelay.hari.workers.dev" || s.Header.Get("X-Forwarded-For") != "203.0.113.7" ||
			s.Header.Get("X-Forwarded-Proto") != "https" {
			t.Errorf("%s: forwarded headers %v", service, s.Header)
		}
		if got := s.Header.Get(credHeader); got != wantCred {
			t.Errorf("%s: credential %q, want %q", service, got, wantCred)
		}
		if v := s.Header.Values(caller.Header); len(v) != 1 {
			t.Fatalf("%s: %d caller headers", service, len(v))
		}
		key, err := upstream.ReadSecretFile(caller.DefaultKeyPath(dir, service))
		if err != nil {
			t.Fatal(err)
		}
		c, err := caller.Verify([]byte(key), s.Header.Get(caller.Header), s.Method, s.Target, s.Body, time.Now())
		if err != nil || c.Client != "https://claude.ai/oauth/client" {
			t.Fatalf("%s: caller header %q: %+v %v", service, s.Header.Get(caller.Header), c, err)
		}
	}

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	resp, err := relay.Do("notes", fakerelay.Request{Method: "POST", Path: "/mcp", Query: "v=2", Headers: frameHeaders(), Body: body})
	if err != nil || resp.Status != 200 {
		t.Fatalf("notes: %+v %v", resp, err)
	}
	s := app.take()
	verify("notes", s, "notes-v1", "X-App-Token")
	if s.Target != "/mcp?v=2" || string(s.Body) != string(body) {
		t.Fatalf("notes target %q body %q", s.Target, s.Body)
	}

	resp, err = relay.Do("web", fakerelay.Request{Method: "GET", Path: "/api/items", Query: "q=1", Headers: frameHeaders()})
	if err != nil || resp.Status != 200 {
		t.Fatalf("web: %+v %v", resp, err)
	}
	s = app.take()
	verify("web", s, "web-v1", "X-Web-Key")
	if s.Target != "/api/items?q=1" {
		t.Fatalf("web target %q", s.Target)
	}

	// The app rotates its token file: the next request carries the new value, no TopRelay call needed.
	writeSecret(t, tokFile, "notes-v2")
	relay.Do("notes", fakerelay.Request{Method: "POST", Path: "/mcp", Headers: frameHeaders(), Body: body})
	verify("notes", app.take(), "notes-v2", "X-App-Token")

	// set-header for web: keyring first, then the config's rev. Applied live: same socket, new value.
	store.Set(secrets.UpstreamHeader("web"), "web-v2")
	cfg.Services[1].UpstreamHeaderRev++
	config.Save(dir, cfg)
	wait(t, "new web credential", func() bool {
		relay.Do("web", fakerelay.Request{Method: "GET", Path: "/", Headers: frameHeaders()})
		s := app.take()
		return s != nil && s.Header.Get("X-Web-Key") == "web-v2"
	})
	// Switching to a file and renaming the header is live too.
	webFile := filepath.Join(dir, "web-token")
	writeSecret(t, webFile, "web-file")
	cfg.Services[1].UpstreamHeader, cfg.Services[1].UpstreamHeaderFile = "X-Web-Token", webFile
	cfg.Services[1].UpstreamHeaderRev++
	config.Save(dir, cfg)
	wait(t, "web credential from a file", func() bool {
		relay.Do("web", fakerelay.Request{Method: "GET", Path: "/", Headers: frameHeaders()})
		s := app.take()
		return s != nil && s.Header.Get("X-Web-Token") == "web-file" && s.Header.Get("X-Web-Key") == ""
	})
	if relay.Connects("web") != 1 || relay.Connects("notes") != 1 {
		t.Fatalf("credential changes reconnected: web %d, notes %d", relay.Connects("web"), relay.Connects("notes"))
	}

	// The token file vanishes: the call fails with a clear tool error, and the app never sees an
	// unauthenticated request.
	os.Remove(tokFile)
	call := []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"x"}}`)
	resp, err = relay.Do("notes", fakerelay.Request{Method: "POST", Path: "/mcp", Headers: frameHeaders(), Body: call})
	if err != nil || resp.Status != 200 || !containsAll(string(resp.Body), `"isError":true`, "credential") {
		t.Fatalf("missing token file: %+v %v", resp, err)
	}
	if app.take() != nil {
		t.Fatal("the app got a request without its credential")
	}
	cancel()
	<-done
}

func containsAll(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}
