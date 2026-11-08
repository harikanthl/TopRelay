package daemon

import (
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
	"github.com/harikanthl/toprelay/agent/internal/files"
	"github.com/harikanthl/toprelay/agent/internal/keys"
	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/testutil/fakerelay"
)

// A tool returns files as MCP resource_links; the bot gets signed relay URLs that work once.
func TestFileLinksEndToEnd(t *testing.T) {
	dir := t.TempDir()
	box := filepath.Join(dir, "outbox")
	os.Mkdir(box, 0o700)
	os.WriteFile(filepath.Join(box, "report.pdf"), []byte("%PDF-1.7 report"), 0o600)
	os.WriteFile(filepath.Join(dir, "outside.txt"), []byte("secret"), 0o600)
	offered := filepath.Join(dir, "scan.png")
	os.WriteFile(offered, []byte("PNG scan"), 0o600)

	reg := files.NewRegistry()
	var offerURI string
	s := mcp.NewServer(&mcp.Implementation{Name: "docs", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "export", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "here you go"},
				&mcp.ResourceLink{URI: "toprelay-file:///report.pdf", Name: "Q3 report.pdf"},
				&mcp.ResourceLink{URI: offerURI, Name: "scan.png"},
				&mcp.ResourceLink{URI: "toprelay-file:///../outside.txt", Name: "x"},
				&mcp.ResourceLink{URI: "https://example.com/untouched", Name: "web"},
			}}, nil
		})
	app := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer app.Close()

	relay := fakerelay.New(t, fakerelay.Service{Name: "docs", Kind: "mcp", Gen: 1})
	store := &secrets.File{Path: filepath.Join(dir, "secrets.json")}
	store.Set(secrets.AgentRoot, relay.AgentRoot)
	store.Set(secrets.BearerRoot, relay.BearerRoot)
	config.Save(dir, &config.Config{PublicURL: "https://unused.example", Services: []config.Service{{Name: "docs", Kind: "mcp", Gen: 1,
		Source: config.Source{Type: config.SourceMCPURL, URL: app.URL}, Tools: config.ToolPolicy{Default: config.DefaultAllow}, FilesDir: box}}})
	d := &Daemon{Dir: dir, Store: store, RelayURL: relay.URL(), PollInterval: 50 * time.Millisecond, Files: reg}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	relay.WaitConnected(t, "docs", 1)

	id, exp, err := reg.Offer("docs", offered, "", "", time.Minute)
	if err != nil || exp.IsZero() {
		t.Fatal(err)
	}
	offerURI = "toprelay-file://" + id

	kid, _ := keys.NewKeyID()
	b, _ := keys.Mint(relay.BearerRoot, kid, keys.Caveats("docs", 1, nil, time.Time{}))
	call := func() []mcp.Content {
		cs, err := mcpproxy.Connect(ctx, relay.URL()+"/docs/mcp", mcpproxy.HTTPClientWithHeaders(nil, map[string]string{"Authorization": "Bearer " + b.String()}))
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "export"})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content
	}
	content := call()
	if len(content) != 5 {
		t.Fatalf("content %d items", len(content))
	}
	report, ok1 := content[1].(*mcp.ResourceLink)
	scan, ok2 := content[2].(*mcp.ResourceLink)
	refused, ok3 := content[3].(*mcp.TextContent)
	web, ok4 := content[4].(*mcp.ResourceLink)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		t.Fatalf("content types %#v", content)
	}
	prefix := relay.URL() + "/docs/files/"
	if !strings.HasPrefix(report.URI, prefix) || report.Name != "Q3 report.pdf" || report.MIMEType != "application/pdf" || report.Size == nil || *report.Size != 15 {
		t.Fatalf("report link %+v", report)
	}
	if !strings.HasPrefix(scan.URI, prefix) || scan.MIMEType != "image/png" {
		t.Fatalf("offered link %+v", scan)
	}
	if !strings.Contains(refused.Text, "couldn't share") {
		t.Fatalf("escape attempt: %q", refused.Text)
	}
	if web.URI != "https://example.com/untouched" {
		t.Fatal("a normal link was rewritten")
	}

	get := func(u string) (int, string, http.Header) {
		resp, err := http.Get(u) // no credential: the link is the credential
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), resp.Header
	}
	code, body, h := get(report.URI)
	if code != 200 || body != "%PDF-1.7 report" || !strings.HasPrefix(h.Get("Content-Disposition"), `attachment; filename="report.pdf"`) || h.Get("Content-Type") != "application/pdf" {
		t.Fatalf("download %d %q %v", code, body, h)
	}
	if code, _, _ := get(report.URI); code != 410 {
		t.Fatalf("second download %d", code)
	}
	if code, body, _ := get(scan.URI); code != 200 || body != "PNG scan" {
		t.Fatalf("offered download %d %q", code, body)
	}
	if code, _, _ := get(strings.Replace(report.URI, "&s=", "&s=x", 1)); code != 403 {
		t.Fatalf("tampered link %d", code)
	}

	// A file changed after its link was made isn't served.
	content = call()
	link := content[1].(*mcp.ResourceLink).URI
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(filepath.Join(box, "report.pdf"), []byte("%PDF-1.7 changed!"), 0o600)
	if code, _, _ := get(link); code != 410 {
		t.Fatalf("changed file served: %d", code)
	}

	// An old relay (no "files" capability): the model is told to have the owner redeploy.
	relay.NoFiles.Store(true)
	relay.Drop("docs")
	relay.WaitConnected(t, "docs", 2)
	content = call()
	if txt, ok := content[1].(*mcp.TextContent); !ok || !strings.Contains(txt.Text, "toprelay deploy") {
		t.Fatalf("old relay: %#v", content[1])
	}
}
