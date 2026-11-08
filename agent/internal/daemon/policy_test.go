package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/keys"
	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/testutil/fakerelay"
)

// Annotations decide by default, and a client rule changes what one client gets.
func TestAnnotationsAndClientPolicy(t *testing.T) {
	no := false
	s := mcp.NewServer(&mcp.Implementation{Name: "app", Version: "1"}, nil)
	for _, tl := range []*mcp.Tool{
		{Name: "search", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		{Name: "read", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		{Name: "append", Annotations: &mcp.ToolAnnotations{DestructiveHint: &no}},
		{Name: "delete"}, // no annotations: MCP says destructive
	} {
		tl.InputSchema = json.RawMessage(`{"type":"object"}`)
		s.AddTool(tl, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ran " + r.Params.Name}}}, nil
		})
	}
	app := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer app.Close()

	dir := t.TempDir()
	relay := fakerelay.New(t, fakerelay.Service{Name: "app", Kind: "mcp", Gen: 1})
	store := &secrets.File{Path: filepath.Join(dir, "secrets.json")}
	store.Set(secrets.AgentRoot, relay.AgentRoot)
	store.Set(secrets.BearerRoot, relay.BearerRoot)
	idA, _ := keys.NewKeyID()
	idB, _ := keys.NewKeyID()
	config.Save(dir, &config.Config{PublicURL: "https://unused.example", Services: []config.Service{{Name: "app", Kind: "mcp", Gen: 1,
		Source: config.Source{Type: config.SourceMCPURL, URL: app.URL},
		Tools: config.ToolPolicy{Default: config.DefaultReadOnly, Clients: map[string]config.ClientRule{
			"key:" + idA: {Allow: []string{"delete"}, Deny: []string{"search"}},
		}}}}})
	d := &Daemon{Dir: dir, Store: store, RelayURL: relay.URL(), PollInterval: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	relay.WaitConnected(t, "app", 1)

	session := func(id string) *mcp.ClientSession {
		b, _ := keys.Mint(relay.BearerRoot, id, keys.Caveats("app", 1, nil, time.Time{}))
		cs, err := mcpproxy.Connect(ctx, relay.URL()+"/app/mcp", mcpproxy.HTTPClientWithHeaders(nil, map[string]string{"Authorization": "Bearer " + b.String()}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cs.Close() })
		return cs
	}
	listed := func(cs *mcp.ClientSession) []string {
		tools, err := mcpproxy.ListAllTools(ctx, cs)
		if err != nil {
			t.Fatal(err)
		}
		n := mcpproxy.ToolNames(tools)
		slices.Sort(n)
		return n
	}
	called := func(cs *mcp.ClientSession, tool string) bool {
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool})
		return err == nil && !r.IsError
	}
	a, b := session(idA), session(idB)
	if got := listed(b); !slices.Equal(got, []string{"read", "search"}) {
		t.Fatalf("default client sees %v (want only the read-only tools)", got)
	}
	if got := listed(a); !slices.Equal(got, []string{"delete", "read"}) {
		t.Fatalf("client A sees %v", got)
	}
	if called(b, "delete") || called(b, "append") || !called(b, "search") {
		t.Fatal("default client calls")
	}
	if !called(a, "delete") || called(a, "search") {
		t.Fatal("client A calls")
	}
	// The relay's coarse list is the union.
	wait(t, "hello with the union", func() bool {
		h := relay.Hellos("app")
		last := h[len(h)-1]
		if last.Tools == nil {
			return false
		}
		got := slices.Clone(*last.Tools)
		slices.Sort(got)
		return slices.Equal(got, []string{"delete", "read", "search"})
	})
}

// Limits through the relay: 429 with Retry-After for the bot, applied live, counted in state.json.
func TestRateLimitsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	relay := fakerelay.New(t, fakerelay.Service{Name: "notes", Kind: "mcp", Gen: 1})
	store := &secrets.File{Path: filepath.Join(dir, "secrets.json")}
	store.Set(secrets.AgentRoot, relay.AgentRoot)
	store.Set(secrets.BearerRoot, relay.BearerRoot)
	up := upstreamServer(t)
	cfg := &config.Config{PublicURL: "https://unused.example", Services: []config.Service{{Name: "notes", Kind: "mcp", Gen: 1,
		Source: config.Source{Type: config.SourceMCPURL, URL: up.URL}, Tools: config.ToolPolicy{Default: config.DefaultAllow}}}}
	config.Save(dir, cfg)
	d := &Daemon{Dir: dir, Store: store, RelayURL: relay.URL(), PollInterval: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	relay.WaitConnected(t, "notes", 1)
	id, _ := keys.NewKeyID()
	b, _ := keys.Mint(relay.BearerRoot, id, keys.Caveats("notes", 1, nil, time.Time{}))
	post := func() *http.Response {
		req, _ := http.NewRequest("POST", relay.URL()+"/notes/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{}}}`))
		req.Header.Set("Authorization", "Bearer "+b.String())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	if r := post(); r.StatusCode != 200 {
		t.Fatalf("no limits yet: %d", r.StatusCode)
	}
	cfg.Services[0].Limits = &config.Limits{Tools: map[string]string{"search": "1/h"}}
	config.Save(dir, cfg)
	var limited *http.Response
	wait(t, "the limit to apply", func() bool {
		limited = post()
		return limited.StatusCode == 429
	})
	if ra := limited.Header.Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After %q", ra)
	}
	if relay.Connects("notes") != 1 {
		t.Fatal("a limits change reconnected")
	}
	wait(t, "limited counted", func() bool {
		s, _ := ReadState(StatePath(dir))
		return s != nil && s.Services["notes"] != nil && s.Services["notes"].Limited >= 1
	})
}
