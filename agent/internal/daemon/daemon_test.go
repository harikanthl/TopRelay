package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/harikanthl/toprelay/agent/internal/calllog"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/keys"
	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/testutil/fakerelay"
)

func upstreamServer(t *testing.T) *httptest.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "1"}, nil)
	for _, n := range []string{"search", "delete", "read"} {
		s.AddTool(&mcp.Tool{Name: n, InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ran " + r.Params.Name}}}, nil
		})
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func wait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestUpEndToEnd(t *testing.T) {
	dir := t.TempDir()
	relay := fakerelay.New(t, fakerelay.Service{Name: "notes", Kind: "mcp", Gen: 1})
	store := &secrets.File{Path: dir + "/secrets.json"}
	store.Set(secrets.AgentRoot, relay.AgentRoot)
	store.Set(secrets.BearerRoot, relay.BearerRoot)
	up := upstreamServer(t)
	cfg := &config.Config{PublicURL: "https://unused.example", Services: []config.Service{{Name: "notes", Kind: "mcp", Gen: 1,
		Source: config.Source{Type: config.SourceMCPURL, URL: up.URL},
		Tools:  config.ToolPolicy{Allow: []string{"search"}, Deny: []string{"delete"}, Default: config.DefaultDeny}}}}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	reload := make(chan struct{}, 1)
	d := &Daemon{Dir: dir, Store: store, RelayURL: relay.URL(), Reload: reload, PollInterval: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	relay.WaitConnected(t, "notes", 1)

	wait(t, "state connected", func() bool {
		s, err := ReadState(StatePath(dir))
		return err == nil && s.Running && s.Services["notes"] != nil && s.Services["notes"].Connected && s.Services["notes"].Since != nil
	})
	st, _ := os.Stat(StatePath(dir))
	if st.Mode().Perm() != 0o600 {
		t.Fatal("state.json mode")
	}
	if h := relay.Hellos("notes"); h[0].Tools == nil || strings.Join(*h[0].Tools, ",") != "search" || h[0].Agent != Agent {
		t.Fatalf("hello %+v", h[0])
	}

	// A bot call through the relay; the refused call is counted too.
	id, _ := keys.NewKeyID()
	b, _ := keys.Mint(relay.BearerRoot, id, keys.Caveats("notes", 1, nil, time.Time{}))
	cs, err := mcpproxy.Connect(ctx, relay.URL()+"/notes/mcp", mcpproxy.HTTPClientWithHeaders(nil, map[string]string{"Authorization": "Bearer " + b.String()}))
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := mcpproxy.ListAllTools(ctx, cs)
	if strings.Join(mcpproxy.ToolNames(tools), ",") != "search" {
		t.Fatalf("tools %v", mcpproxy.ToolNames(tools))
	}
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "delete"})
	if err != nil || !r.IsError {
		t.Fatalf("delete %v %v", r, err)
	}
	cs.Close()
	wait(t, "requests counted", func() bool {
		s, _ := ReadState(StatePath(dir))
		return s != nil && s.Services["notes"].Requests >= 3 && s.Services["notes"].Refused == 1
	})
	lines, _ := calllog.Tail(calllog.Path(dir, "notes"), 0)
	if len(lines) < 3 || !strings.Contains(strings.Join(lines, "\n"), `"client":"key:`+id+`"`) {
		t.Fatalf("call log %v", lines)
	}

	// Policy change in the config file: picked up without a reconnect, hello resent.
	cfg.Services[0].Tools = config.ToolPolicy{Default: config.DefaultAllow, Deny: []string{"delete"}}
	config.Save(dir, cfg)
	wait(t, "hello resent", func() bool {
		h := relay.Hellos("notes")
		last := h[len(h)-1]
		return len(h) >= 2 && last.Tools != nil && slices.Equal(*last.Tools, []string{"search", "read"}) || len(h) >= 2 && last.Tools != nil && slices.Contains(*last.Tools, "read") && !slices.Contains(*last.Tools, "delete")
	})
	if relay.Connects("notes") != 1 {
		t.Fatal("policy change must not reconnect")
	}
	// Allow all, via the reload signal: tools null.
	cfg.Services[0].Tools = config.ToolPolicy{Default: config.DefaultAllow}
	config.Save(dir, cfg)
	reload <- struct{}{}
	wait(t, "allow-all hello", func() bool {
		h := relay.Hellos("notes")
		return h[len(h)-1].Tools == nil
	})

	// Rotation (gen 2): the service restarts with the new key.
	relay.SetService(fakerelay.Service{Name: "notes", Kind: "mcp", Gen: 2})
	cfg.Services[0].Gen = 2
	config.Save(dir, cfg)
	relay.WaitConnected(t, "notes", 2)

	// Removal: the tunnel stops and the state entry goes.
	cfg.Services = nil
	config.Save(dir, cfg)
	wait(t, "removed", func() bool {
		s, _ := ReadState(StatePath(dir))
		return s != nil && s.Services["notes"] == nil && !relay.Connected("notes")
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s, _ := ReadState(StatePath(dir))
	if s.Running {
		t.Fatal("state must say stopped after exit")
	}
}

func TestUpNeedsRoots(t *testing.T) {
	dir := t.TempDir()
	config.Save(dir, &config.Config{PublicURL: "https://x", Services: []config.Service{{Name: "a", Kind: "http", Gen: 1, Source: config.Source{Type: "http", URL: "http://127.0.0.1:1"}}}})
	d := &Daemon{Dir: dir, Store: &secrets.File{Path: dir + "/secrets.json"}}
	err := d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "agent-root") {
		t.Fatalf("err %v", err)
	}
}
