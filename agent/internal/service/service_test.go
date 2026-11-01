package service

import (
	"strings"
	"testing"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/config"
)

func TestHello(t *testing.T) {
	svc := config.Service{Name: "notes", Kind: "mcp", Gen: 1, Source: config.Source{Type: config.SourceMCPURL, URL: "http://127.0.0.1:1/mcp"},
		Tools: config.ToolPolicy{Allow: []string{"search"}, Default: config.DefaultDeny}}
	rt, err := Build(svc, "", nil, nil)
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

	h, err := Build(config.Service{Name: "web", Kind: "http", Source: config.Source{Type: config.SourceHTTP, URL: "http://127.0.0.1:1"}, AllowPaths: []string{"/"}}, "", nil, nil)
	if err != nil || !h.Hello().AllowAll || h.Hello().Kind != "http" {
		t.Fatal("http hello")
	}
	for _, bad := range []config.Service{
		{Name: "x", Source: config.Source{Type: config.SourceMCPURL, URL: "ftp://x"}},
		{Name: "x", Source: config.Source{Type: config.SourceHTTP, URL: "::"}},
		{Name: "x", Source: config.Source{Type: "nope"}},
	} {
		if _, err := Build(bad, "", nil, nil); err == nil {
			t.Errorf("%+v accepted", bad.Source)
		}
	}
}
