package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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

// A config.json written by an older version carries stdio env values. The daemon moves them to the keyring,
// rewrites the config with names only, and still starts the child with the values.
func TestLegacyStdioEnvMovesToKeyring(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "tinyserver")
	if out, err := exec.Command("go", "build", "-o", bin, "../stdiobridge/testdata/tinyserver").CombinedOutput(); err != nil {
		t.Fatalf("build tinyserver: %v\n%s", err, out)
	}
	dir := t.TempDir()
	relay := fakerelay.New(t, fakerelay.Service{Name: "tools", Kind: "mcp", Gen: 1})
	store := &secrets.File{Path: filepath.Join(dir, "secrets.json")}
	store.Set(secrets.AgentRoot, relay.AgentRoot)
	store.Set(secrets.BearerRoot, relay.BearerRoot)
	const value = "greeting-s3cret-value"
	legacy, _ := json.Marshal(map[string]any{"publicUrl": "https://unused.example", "services": []any{map[string]any{
		"name": "tools", "kind": "mcp", "gen": 1,
		"source": map[string]any{"type": "stdio", "command": []string{bin}, "env": map[string]string{"TINY_GREETING": value}},
		"tools":  map[string]any{"allow": []string{}, "deny": []string{}, "default": "allow"}}}})
	if err := os.WriteFile(config.Path(dir), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Dir: dir, Store: store, RelayURL: relay.URL(), PollInterval: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() { cancel(); <-done }()
	relay.WaitConnected(t, "tools", 1)

	raw, _ := os.ReadFile(config.Path(dir))
	if strings.Contains(string(raw), value) || !strings.Contains(string(raw), `"TINY_GREETING"`) {
		t.Fatalf("config.json after migration:\n%s", raw)
	}
	if v, err := store.Get("stdio-env/tools/TINY_GREETING"); err != nil || v != value {
		t.Fatalf("keyring value %q %v", v, err)
	}

	id, _ := keys.NewKeyID()
	bearer, _ := keys.Mint(relay.BearerRoot, id, keys.Caveats("tools", 1, nil, time.Now().Add(time.Hour)))
	wctx, wcancel := context.WithTimeout(ctx, 20*time.Second)
	defer wcancel()
	cs, err := mcpproxy.Connect(wctx, relay.URL()+"/tools/mcp", mcpproxy.HTTPClientWithHeaders(nil, map[string]string{"Authorization": "Bearer " + bearer.String()}))
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	pr, err := cs.GetPrompt(wctx, &mcp.GetPromptParams{Name: "greet"})
	if err != nil || pr.Messages[0].Content.(*mcp.TextContent).Text != "hi "+value {
		t.Fatalf("child env: %v %v", pr, err)
	}
}
