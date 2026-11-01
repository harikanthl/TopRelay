package stdiobridge_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/harikanthl/toprelay/agent/internal/keys"
	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
	"github.com/harikanthl/toprelay/agent/internal/stdiobridge"
	"github.com/harikanthl/toprelay/agent/internal/testutil/fakerelay"
	"github.com/harikanthl/toprelay/agent/internal/tunnel"
)

// BuildTiny compiles testdata/tinyserver and returns the binary path.
func buildTiny(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tinyserver")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/tinyserver")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build tinyserver: %v\n%s", err, out)
	}
	return bin
}

func text(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if len(r.Content) == 0 {
		t.Fatal("empty result")
	}
	return r.Content[0].(*mcp.TextContent).Text
}

func TestBridgeThroughRelay(t *testing.T) {
	bin := buildTiny(t)
	var allowed atomic.Pointer[[]string]
	setAllowed := func(names ...string) { allowed.Store(&names) }
	setAllowed("echo", "add_tool", "crash", "late")
	var toolChanges atomic.Int32
	b := stdiobridge.New(stdiobridge.Options{Service: "tools", Command: []string{bin}, Env: map[string]string{"TINY_GREETING": "there"},
		Allowed: func(n string) bool { return slices.Contains(*allowed.Load(), n) }, OnToolsChanged: func() { toolChanges.Add(1) },
		MinRestart: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridgeDone := make(chan struct{})
	go func() { b.Run(ctx); close(bridgeDone) }()
	wctx, wcancel := context.WithTimeout(ctx, 20*time.Second)
	defer wcancel()
	if err := b.WaitReady(wctx); err != nil {
		t.Fatal(err)
	}

	relay := fakerelay.New(t, fakerelay.Service{Name: "tools", Kind: "mcp", Gen: 1})
	agentKey, _ := keys.AgentKey(relay.AgentRoot, "tools", 1)
	tc := tunnel.New(tunnel.Options{BaseURL: relay.URL(), Service: "tools", AgentKey: agentKey, Handler: b,
		Hello: func() tunnel.Hello { return tunnel.Hello{Kind: "mcp", Tools: b.Exposed()} }})
	go tc.Run(ctx)
	relay.WaitConnected(t, "tools", 1)
	if h := relay.Hellos("tools"); strings.Join(*h[0].Tools, ",") != "add_tool,crash,echo" {
		t.Fatalf("hello tools %v", *h[0].Tools)
	}

	id, _ := keys.NewKeyID()
	bearer, _ := keys.Mint(relay.BearerRoot, id, keys.Caveats("tools", 1, nil, time.Now().Add(time.Hour)))
	client := mcpproxy.HTTPClientWithHeaders(nil, map[string]string{"Authorization": "Bearer " + bearer.String()})
	cs, err := mcpproxy.Connect(wctx, relay.URL()+"/tools/mcp", client)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if v := cs.InitializeResult().ProtocolVersion; v != "2026-07-28" {
		t.Fatalf("protocol %s, want 2026-07-28", v)
	}
	tools, err := mcpproxy.ListAllTools(wctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(mcpproxy.ToolNames(tools), ","); got != "add_tool,crash,echo" {
		t.Fatalf("tools %s", got)
	}
	if tools[2].Annotations == nil || !tools[2].Annotations.ReadOnlyHint {
		t.Fatal("annotations not mirrored")
	}
	res, err := cs.CallTool(wctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]string{"msg": "hi"}})
	if err != nil || !strings.HasPrefix(text(t, res), "echo: hi pid=") {
		t.Fatalf("echo %v %v", res, err)
	}
	pid1 := text(t, res)
	res, err = cs.CallTool(wctx, &mcp.CallToolParams{Name: "wipe"})
	if err != nil || !res.IsError || text(t, res) != `The tool "wipe" is not available on tools. Its owner hasn't allowed it.` {
		t.Fatalf("wipe %v %v", res, err)
	}
	rr, err := cs.ReadResource(wctx, &mcp.ReadResourceParams{URI: "tiny://readme"})
	if err != nil || rr.Contents[0].Text != "hello from tiny" {
		t.Fatalf("resource %v %v", rr, err)
	}
	pr, err := cs.GetPrompt(wctx, &mcp.GetPromptParams{Name: "greet"})
	if err != nil || pr.Messages[0].Content.(*mcp.TextContent).Text != "hi there" {
		t.Fatalf("prompt %v %v", pr, err)
	}

	// A bearer key restricted to "add_tool" can't call echo, checked from the body by the agent.
	nk := keys.Narrow(bearer, "tools=add_tool")
	rcs, err := mcpproxy.Connect(wctx, relay.URL()+"/tools/mcp", mcpproxy.HTTPClientWithHeaders(nil, map[string]string{"Authorization": "Bearer " + nk.String()}))
	if err != nil {
		t.Fatal(err)
	}
	res, err = rcs.CallTool(wctx, &mcp.CallToolParams{Name: "echo"})
	if err != nil || !res.IsError || !strings.Contains(text(t, res), "not available") {
		t.Fatalf("restricted echo %v %v", res, err)
	}
	rtools, _ := mcpproxy.ListAllTools(wctx, rcs)
	if strings.Join(mcpproxy.ToolNames(rtools), ",") != "add_tool" {
		t.Fatalf("restricted list %v", mcpproxy.ToolNames(rtools))
	}
	rcs.Close()

	// list_changed from the child: "late" appears, and the hello is updated by the caller.
	before := toolChanges.Load()
	if _, err := cs.CallTool(wctx, &mcp.CallToolParams{Name: "add_tool"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return slices.Contains(b.Exposed(), "late") && toolChanges.Load() > before })
	res, err = cs.CallTool(wctx, &mcp.CallToolParams{Name: "late"})
	if err != nil || text(t, res) != "late ran" {
		t.Fatalf("late %v %v", res, err)
	}

	// Policy change: echo is removed without a restart.
	setAllowed("add_tool", "crash", "late")
	b.PolicyChanged()
	waitFor(t, func() bool { return !slices.Contains(b.Exposed(), "echo") })
	setAllowed("echo", "add_tool", "crash", "late")
	b.PolicyChanged()
	waitFor(t, func() bool { return slices.Contains(b.Exposed(), "echo") })

	// The child crashes: the bridge restarts it and calls work again (in a new process).
	cs.CallTool(wctx, &mcp.CallToolParams{Name: "crash"})
	waitFor(t, func() bool { return b.Starts() >= 2 })
	var res2 *mcp.CallToolResult
	waitFor(t, func() bool {
		r, err := cs.CallTool(wctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]string{"msg": "again"}})
		if err != nil || r.IsError {
			return false
		}
		res2 = r
		return true
	})
	if got := text(t, res2); !strings.HasPrefix(got, "echo: again pid=") || got[len("echo: again "):] == pid1[len("echo: hi "):] {
		t.Fatalf("after restart: %s (before %s)", got, pid1)
	}

	// Legacy SSE transport through the relay (what Qwen / Model Studio uses).
	sc := mcp.NewClient(&mcp.Implementation{Name: "sse-client", Version: "1"}, nil)
	sseCS, err := sc.Connect(wctx, &mcp.SSEClientTransport{Endpoint: relay.URL() + "/tools/mcp/sse", HTTPClient: client}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stools, err := mcpproxy.ListAllTools(wctx, sseCS)
	if err != nil || !slices.Contains(mcpproxy.ToolNames(stools), "echo") || slices.Contains(mcpproxy.ToolNames(stools), "wipe") {
		t.Fatalf("sse tools %v %v", mcpproxy.ToolNames(stools), err)
	}
	res, err = sseCS.CallTool(wctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]string{"msg": "sse"}})
	if err != nil || !strings.HasPrefix(text(t, res), "echo: sse") {
		t.Fatalf("sse echo %v %v", res, err)
	}
	if _, err := sseCS.CallTool(wctx, &mcp.CallToolParams{Name: "wipe"}); err == nil {
		t.Fatal("sse wipe should fail")
	}
	sseCS.Close()
	cs.Close()
	cancel()
	select {
	case <-bridgeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("bridge didn't stop")
	}
}

func TestListChildTools(t *testing.T) {
	bin := buildTiny(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tools, err := stdiobridge.ListChildTools(ctx, []string{bin}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(mcpproxy.ToolNames(tools), ",") != "add_tool,crash,echo,wipe" {
		t.Fatalf("%v", mcpproxy.ToolNames(tools))
	}
	if _, err := stdiobridge.ListChildTools(ctx, []string{filepath.Join(t.TempDir(), "missing")}, nil, ""); err == nil {
		t.Fatal("missing command accepted")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
