package service

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
)

// build makes a runtime the way the daemon does, with a temporary config dir and file keyring.
func build(t testing.TB, svc config.Service) (*Runtime, error) {
	t.Helper()
	dir := t.TempDir()
	if !svc.HasUpstream() {
		return Build(svc, nil, nil, nil)
	}
	up, err := NewUpstream(svc, dir, &secrets.File{Path: filepath.Join(dir, "secrets.json")})
	if err != nil {
		return nil, err
	}
	return Build(svc, up, nil, nil)
}

func TestHello(t *testing.T) {
	svc := config.Service{Name: "notes", Kind: "mcp", Gen: 1, Source: config.Source{Type: config.SourceMCPURL, URL: "http://127.0.0.1:1/mcp"},
		Tools: config.ToolPolicy{Allow: []string{"search"}, Default: config.DefaultDeny}}
	rt, err := build(t, svc)
	if err != nil {
		t.Fatal(err)
	}
	if h := rt.Hello(); h.AllowAll || strings.Join(h.Tools, ",") != "search" {
		t.Fatalf("deny default %+v", h)
	}
	// Default allow with a deny: the hello lists known tools minus the denied ones.
	rt.SetPolicy(config.ToolPolicy{Default: config.DefaultAllow, Deny: []string{"delete"}})
	select {
	case <-rt.HelloChanged():
	case <-time.After(time.Second):
		t.Fatal("no hello change signal")
	}
	rt.addKnown([]string{"search", "delete", "read"})
	if h := rt.Hello(); strings.Join(h.Tools, ",") != "search,read" {
		t.Fatalf("known %+v", h)
	}
	rt.SetPolicy(config.ToolPolicy{Default: config.DefaultAllow})
	if h := rt.Hello(); !h.AllowAll {
		t.Fatalf("allow all %+v", h)
	}
	// Same hello twice: no second signal.
	<-rt.HelloChanged()
	rt.notify()
	select {
	case <-rt.HelloChanged():
		t.Fatal("unchanged hello signalled")
	default:
	}

	h, err := build(t, config.Service{Name: "web", Kind: "http", Source: config.Source{Type: config.SourceHTTP, URL: "http://127.0.0.1:1"}, AllowPaths: []string{"/"}})
	if err != nil || !h.Hello().AllowAll || h.Hello().Kind != "http" {
		t.Fatal("http hello")
	}
	for _, bad := range []config.Service{
		{Name: "x", Source: config.Source{Type: config.SourceMCPURL, URL: "ftp://x"}},
		{Name: "x", Source: config.Source{Type: config.SourceHTTP, URL: "::"}},
		{Name: "x", Source: config.Source{Type: "nope"}},
	} {
		if _, err := build(t, bad); err == nil {
			t.Errorf("%+v accepted", bad.Source)
		}
	}
	// An HTTP upstream service can't be built without its upstream.
	if _, err := Build(svc, nil, nil, nil); err == nil {
		t.Error("mcp-url service built without an upstream")
	}
}
