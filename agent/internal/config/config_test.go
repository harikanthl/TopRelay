package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing config: %v", err)
	}
	c := &Config{
		AccountID: "acc", Subdomain: "hari", PublicURL: "https://toprelay.hari.workers.dev", KVNamespaceID: "kv1",
		Services: []Service{
			{Name: "notes", Kind: KindMCP, Gen: 2, Source: Source{Type: SourceMCPURL, URL: "http://127.0.0.1:7879/mcp"},
				Tools: ToolPolicy{Allow: []string{"search"}, Deny: []string{"delete"}, Default: DefaultDeny}, UpstreamHeader: "X-Token"},
			{Name: "tools", Kind: KindMCP, Gen: 1, Source: Source{Type: SourceStdio, Command: []string{"node", "s.js"}, EnvNames: []string{"A"}, Cwd: "/tmp"}},
			{Name: "web", Kind: KindHTTP, Gen: 1, Source: Source{Type: SourceHTTP, URL: "http://127.0.0.1:8080"}, AllowPaths: []string{"/api"}},
		},
	}
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, c)
	}
	if got.Find("tools") == nil || got.Find("nope") != nil {
		t.Fatal("Find")
	}
	if _, err := got.MustFind("nope"); err == nil {
		t.Fatal("MustFind")
	}
	if !got.Remove("web") || got.Remove("web") || len(got.Services) != 2 {
		t.Fatal("Remove")
	}
}

func TestDir(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	env["HOME"] = "/h"
	if d, _ := Dir(get); d != "/h/.config/toprelay" {
		t.Fatal(d)
	}
	env["XDG_CONFIG_HOME"] = "/x"
	if d, _ := Dir(get); d != "/x/toprelay" {
		t.Fatal(d)
	}
	env["TOPRELAY_CONFIG_DIR"] = "/c"
	if d, _ := Dir(get); d != "/c" {
		t.Fatal(d)
	}
}

func TestPolicy(t *testing.T) {
	p := ToolPolicy{Allow: []string{"a"}, Deny: []string{"b"}, Default: DefaultDeny}
	if !p.Allowed("a") || p.Allowed("b") || p.Allowed("new") {
		t.Fatal("deny default")
	}
	if p.AllowsAll() {
		t.Fatal("AllowsAll")
	}
	p.Default = DefaultAllow
	if !p.Allowed("new") || p.Allowed("b") || p.AllowsAll() {
		t.Fatal("allow default with deny")
	}
	if got := p.AllowList([]string{"c", "b", "a", "c"}); !reflect.DeepEqual(got, []string{"c", "a"}) {
		t.Fatalf("AllowList = %v", got)
	}
	p.SetAllowed("b")
	if !p.Allowed("b") || len(p.Deny) != 0 || !p.AllowsAll() {
		t.Fatalf("SetAllowed: %+v", p)
	}
	p.SetDenied("a")
	if p.Allowed("a") || len(p.Allow) != 1 {
		t.Fatalf("SetDenied: %+v", p)
	}
	var z ToolPolicy
	z.Normalize()
	if z.Default != DefaultDeny || z.Allow == nil || z.Deny == nil {
		t.Fatal("Normalize")
	}
	// An explicit allow wins only when not denied.
	both := ToolPolicy{Allow: []string{"x"}, Deny: []string{"x"}, Default: DefaultAllow}
	if both.Allowed("x") {
		t.Fatal("deny must win")
	}
}

func TestBadJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(Path(dir), []byte("{"), 0o600)
	if _, err := Load(dir); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

func TestSaveRefusesEnvValues(t *testing.T) {
	c := &Config{Services: []Service{{Name: "tools", Kind: KindMCP, Gen: 1,
		Source: Source{Type: SourceStdio, Command: []string{"x"}, Env: map[string]string{"TOKEN": "v"}}}}}
	if err := Save(t.TempDir(), c); err == nil || !strings.Contains(err.Error(), "keyring") {
		t.Fatalf("Save with env values: %v", err)
	}
}
