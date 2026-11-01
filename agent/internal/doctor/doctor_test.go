package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/testutil/cffake"
)

func find(r Report, name string) Check {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return Check{}
}

func TestNoConfig(t *testing.T) {
	r := Run(context.Background(), Env{Dir: t.TempDir(), Store: &secrets.File{Path: t.TempDir() + "/s.json"}})
	if r.OK || len(r.Checks) != 1 || r.Checks[0].Status != Fail || !strings.Contains(r.Checks[0].Fix, "toprelay login") {
		t.Fatalf("%+v", r)
	}
}

func TestFailuresHaveFixes(t *testing.T) {
	dir := t.TempDir()
	store := &secrets.File{Path: filepath.Join(dir, "s.json")}
	// A relay whose clock is 5 minutes ahead, a local MCP URL that's down, and no token.
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().Add(5*time.Minute-30*time.Second).UTC().Format(http.TimeFormat))
		w.Write([]byte("ok"))
	}))
	defer relay.Close()
	config.Save(dir, &config.Config{AccountID: "acc1", PublicURL: "https://toprelay.nowhere.workers.dev",
		Services: []config.Service{{Name: "notes", Kind: "mcp", Gen: 1, Source: config.Source{Type: config.SourceMCPURL, URL: "http://127.0.0.1:1/mcp"}}}})
	r := Run(context.Background(), Env{Dir: dir, Store: store, RelayURL: relay.URL})
	if r.OK {
		t.Fatal("should fail")
	}
	for _, name := range []string{"cloudflare token", "root secrets", "local service: notes"} {
		c := find(r, name)
		if c.Status != Fail || c.Fix == "" {
			t.Errorf("%s: %+v", name, c)
		}
	}
	if c := find(r, "clock skew"); c.Status != Warn || !strings.Contains(c.Detail, "clock is 4m") {
		t.Errorf("skew: %+v", c)
	}
	if c := find(r, "agent connected: notes"); c.Status != Skip {
		t.Errorf("agent: %+v", c)
	}
}

func TestPermissionsAndDNS(t *testing.T) {
	dir := t.TempDir()
	store := &secrets.File{Path: filepath.Join(dir, "s.json")}
	cf := cffake.New(t)
	store.Set(secrets.CloudflareToken, cf.Token)
	config.Save(dir, &config.Config{AccountID: "acc1", PublicURL: "https://toprelay.hari.workers.dev"})
	r := Run(context.Background(), Env{Dir: dir, Store: store, CFBase: cf.URL(),
		Resolve: func(ctx context.Context, host string) error {
			if host != "toprelay.hari.workers.dev" {
				t.Errorf("resolved %s", host)
			}
			return context.DeadlineExceeded
		}})
	if c := find(r, "cloudflare permissions"); c.Status != OK {
		t.Errorf("perms %+v", c)
	}
	if c := find(r, "worker deployed"); c.Status != Fail || !strings.Contains(c.Fix, "toprelay add") {
		t.Errorf("worker %+v", c)
	}
	if c := find(r, "subdomain DNS"); c.Status != Fail {
		t.Errorf("dns %+v", c)
	}
	if c := find(r, "root secrets"); c.Status != Skip {
		t.Errorf("roots with no services %+v", c)
	}
}
