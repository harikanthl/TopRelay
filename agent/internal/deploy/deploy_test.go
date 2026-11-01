package deploy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/cfapi"
	"github.com/harikanthl/toprelay/agent/internal/testutil/cffake"
)

const bundle = "export class Relay {}\nexport default { fetch() {} }\n"

func newDeployer(t *testing.T, f *cffake.Fake, health *httptest.Server) *Deployer {
	api := cfapi.New(f.Token, f.URL())
	api.Sleep = func(context.Context, time.Duration) error { return nil }
	return &Deployer{API: api, Script: []byte(bundle), PollInterval: time.Millisecond, PollTimeout: 2 * time.Second,
		PublicURLFor: func(string) string { return health.URL }}
}

func healthServer(t *testing.T, failFirst int32) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("health path %s", r.URL.Path)
		}
		if n.Add(1) <= failFirst {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte("ok"))
	}))
	t.Cleanup(s.Close)
	return s, &n
}

func TestDeployFirstThenRedeploy(t *testing.T) {
	f := cffake.New(t)
	f.TakenNames["hari"] = true // account name "Hari's Account" → "hari"; taken, so a suffix is tried
	f.Fail429Once = true
	h, hits := healthServer(t, 2)
	d := newDeployer(t, f, h)
	in := Input{AccountID: "acc1", AccountName: "Hari's Account",
		Services:  []ServiceEntry{{Name: "notes", Kind: "mcp", Gen: 1}},
		AgentRoot: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", BearerRoot: "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8"}
	res, err := d.Deploy(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.FirstDeploy || !strings.HasPrefix(res.Subdomain, "hari-") || res.KVNamespaceID != "kv1" {
		t.Fatalf("result %+v", res)
	}
	if hits.Load() != 3 {
		t.Fatalf("health polled %d times", hits.Load())
	}
	up := f.LastUpload()
	if !reflect.DeepEqual(up.PartOrder, []string{"metadata", "relay.js"}) {
		t.Fatalf("parts %v", up.PartOrder)
	}
	if p := up.Parts["relay.js"]; p.ContentType != "application/javascript+module" || string(p.Body) != bundle || p.Filename != "relay.js" {
		t.Fatalf("module part %+v", p)
	}
	if up.Parts["metadata"].ContentType != "application/json" {
		t.Fatal("metadata content type")
	}
	want := map[string]any{
		"main_module":         "relay.js",
		"compatibility_date":  "2026-10-01",
		"compatibility_flags": []any{"global_fetch_strictly_public", "enable_request_signal"},
		"observability":       map[string]any{"enabled": true, "logs": map[string]any{"enabled": true, "invocation_logs": false}},
		"migrations":          map[string]any{"new_tag": "v1", "new_sqlite_classes": []any{"Relay"}},
		"bindings": []any{
			map[string]any{"type": "durable_object_namespace", "name": "RELAY", "class_name": "Relay"},
			map[string]any{"type": "kv_namespace", "name": "OAUTH_KV", "namespace_id": "kv1"},
			map[string]any{"type": "plain_text", "name": "PUBLIC_URL", "text": h.URL},
			map[string]any{"type": "plain_text", "name": "SERVICES", "text": `[{"name":"notes","kind":"mcp","gen":1}]`},
			map[string]any{"type": "secret_text", "name": "AGENT_ROOT", "text": in.AgentRoot},
			map[string]any{"type": "secret_text", "name": "BEARER_ROOT", "text": in.BearerRoot},
		},
	}
	if !reflect.DeepEqual(up.Metadata, want) {
		g, _ := json.MarshalIndent(up.Metadata, "", " ")
		t.Fatalf("metadata:\n%s", g)
	}
	if len(f.Enables) != 1 || f.Enables[0].Header.Get("Cloudflare-Workers-Script-Api-Date") != "2025-08-01" ||
		strings.TrimSpace(string(f.Enables[0].Body)) != `{"enabled":true}` {
		t.Fatalf("enable call %+v", f.Enables)
	}

	// Redeploy: no migrations, KV reused, subdomain kept, no new namespace.
	in.Services = append(in.Services, ServiceEntry{Name: "tools", Kind: "mcp", Gen: 1})
	res2, err := d.Deploy(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res2.FirstDeploy || res2.KVNamespaceID != "kv1" || res2.Subdomain != res.Subdomain || len(f.KV) != 1 {
		t.Fatalf("redeploy %+v kv %v", res2, f.KV)
	}
	up2 := f.LastUpload()
	if _, has := up2.Metadata["migrations"]; has {
		t.Fatal("migrations sent on a redeploy")
	}
	if up2.Binding("SERVICES")["text"] != `[{"name":"notes","kind":"mcp","gen":1},{"name":"tools","kind":"mcp","gen":1}]` {
		t.Fatalf("services %v", up2.Binding("SERVICES"))
	}
	// The token never appears anywhere but the Authorization header.
	for _, r := range f.Requests {
		if strings.Contains(string(r.Body), f.Token) || strings.Contains(r.Path, f.Token) {
			t.Fatal("token in body or path")
		}
	}
}

func TestDeployRefusesMissingBundle(t *testing.T) {
	f := cffake.New(t)
	h, _ := healthServer(t, 0)
	d := newDeployer(t, f, h)
	d.Script = []byte("// nothing here\n")
	_, err := d.Deploy(context.Background(), Input{AccountID: "acc1", AgentRoot: "x", BearerRoot: "y"})
	if err == nil || !strings.Contains(err.Error(), "Relay") {
		t.Fatalf("err %v", err)
	}
	if len(f.Requests) != 0 {
		t.Fatal("API called with a bad bundle")
	}
	d.Script = []byte("class Relay{}\nexport { Relay, worker as default };")
	if !exportsRelay.Match(d.Script) {
		t.Fatal("bundler-style export not recognised")
	}
}

func TestHealthTimeout(t *testing.T) {
	f := cffake.New(t)
	f.Subdomain = "hari"
	h, _ := healthServer(t, 1<<30)
	d := newDeployer(t, f, h)
	d.PollTimeout = 50 * time.Millisecond
	_, err := d.Deploy(context.Background(), Input{AccountID: "acc1", AgentRoot: "x", BearerRoot: "y"})
	if err == nil || !strings.Contains(err.Error(), "doctor") {
		t.Fatalf("err %v", err)
	}
}

func TestSubdomainFromName(t *testing.T) {
	cases := map[string]string{
		"Hari's Account":        "hari",
		"ACME Corp":             "acme-corp",
		"  --  ":                "toprelay",
		"me@example.com":        "me-example-com",
		strings.Repeat("a", 80): strings.Repeat("a", 63),
	}
	for in, want := range cases {
		if got := SubdomainFromName(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestMetadataEmptyServices(t *testing.T) {
	b, err := Metadata("https://x/", "kv", nil, "a", "b", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"text":"[]"`) || !strings.Contains(string(b), `"text":"https://x"`) {
		t.Fatalf("%s", b)
	}
}
