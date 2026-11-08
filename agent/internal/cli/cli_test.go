package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/harikanthl/toprelay/agent/internal/calllog"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/daemon"
	"github.com/harikanthl/toprelay/agent/internal/keys"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/testutil/cffake"
	"github.com/harikanthl/toprelay/agent/internal/testutil/fakerelay"
)

type env struct {
	t     *testing.T
	dir   string
	store *secrets.File
	cf    *cffake.Fake
	relay *fakerelay.Relay
	up    *httptest.Server
	vars  map[string]string
}

func upstreamMCP(t *testing.T) *httptest.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "notes-app", Version: "1"}, nil)
	add := func(name string, ro bool) {
		s.AddTool(&mcp.Tool{Name: name, Description: name + " notes", InputSchema: json.RawMessage(`{"type":"object"}`),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: ro}},
			func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ran " + r.Params.Name}}}, nil
			})
	}
	add("search", true)
	add("read", true)
	add("delete", false)
	add("bad.name", false)
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		if r.Header.Get("X-App-Token") != "app-secret-123" {
			return nil
		}
		return s
	}, &mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(srv.Close)
	return srv
}

func newEnv(t *testing.T) *env {
	dir := t.TempDir()
	e := &env{t: t, dir: dir, store: &secrets.File{Path: filepath.Join(dir, "secrets.json")}, cf: cffake.New(t),
		relay: fakerelay.New(t), up: upstreamMCP(t), vars: map[string]string{}}
	return e
}

func (e *env) app(stdin string) (*App, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &App{Ctx: context.Background(), Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb,
		Getenv: func(k string) string { return e.vars[k] }, Dir: e.dir, Store: e.store, CFBase: e.cf.URL(),
		RelayURL: e.relay.URL(), DeployPoll: 5 * time.Millisecond}, &out, &errb
}

func (e *env) run(stdin string, args ...string) (int, string, string) {
	e.t.Helper()
	a, out, errb := e.app(stdin)
	code := Main(args, a)
	return code, out.String(), errb.String()
}

// runJSON runs a --json command and decodes its single JSON object.
func (e *env) runJSON(want int, v any, args ...string) {
	e.t.Helper()
	code, out, errs := e.run("", append([]string{"--json"}, args...)...)
	if code != want {
		e.t.Fatalf("%v: exit %d, want %d\nstdout: %s\nstderr: %s", args, code, want, out, errs)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(v); err != nil {
		e.t.Fatalf("%v: stdout is not JSON: %v\n%s", args, err, out)
	}
	if dec.More() {
		e.t.Fatalf("%v: more than one JSON object on stdout:\n%s", args, out)
	}
}

// syncRelay copies the CLI's roots and services into the fake relay.
func (e *env) syncRelay() {
	ar, _ := e.store.Get(secrets.AgentRoot)
	br, _ := e.store.Get(secrets.BearerRoot)
	e.relay.AgentRoot, e.relay.BearerRoot = ar, br
	cfg, _ := config.Load(e.dir)
	for _, s := range cfg.Services {
		e.relay.SetService(fakerelay.Service{Name: s.Name, Kind: s.Kind, Gen: s.Gen})
	}
}

type errorOut struct {
	Error struct {
		Code, Message, Fix string
	} `json:"error"`
}

func TestCLIEndToEnd(t *testing.T) {
	e := newEnv(t)

	// Errors before login are JSON with code, message and fix.
	var eo errorOut
	e.runJSON(1, &eo, "list")
	if eo.Error.Code != "no_config" || eo.Error.Fix == "" {
		t.Fatalf("%+v", eo)
	}
	e.runJSON(1, &eo, "add", "notes", "--mcp-url", e.up.URL)
	if eo.Error.Code != "not_logged_in" {
		t.Fatalf("%+v", eo)
	}

	// login: several accounts need --account without a TTY.
	e.cf.Accounts = append(e.cf.Accounts, map[string]string{"id": "acc2", "name": "Work"})
	code, out, _ := e.run(e.cf.Token+"\n", "login", "--token-stdin", "--json")
	if code != 1 || !strings.Contains(out, `"code":"choose_account"`) || strings.Contains(out, e.cf.Token) {
		t.Fatalf("login without --account: %d %s", code, out)
	}
	var choose struct {
		Error struct {
			Accounts []AccountChoice `json:"accounts"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &choose); err != nil || len(choose.Error.Accounts) != 2 || choose.Error.Accounts[1] != (AccountChoice{ID: "acc2", Name: "Work"}) {
		t.Fatalf("choose_account should list the accounts for a picker: %v %s", err, out)
	}
	code, out, _ = e.run(e.cf.Token+"\n", "login", "--token-stdin", "--account", "acc1", "--json")
	var login struct {
		Account     map[string]string `json:"account"`
		TokenStatus string            `json:"tokenStatus"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &login) != nil || login.Account["id"] != "acc1" || login.TokenStatus != "active" {
		t.Fatalf("login: %d %s", code, out)
	}
	if tok, _ := e.store.Get(secrets.CloudflareToken); tok != e.cf.Token {
		t.Fatal("token not stored")
	}
	code, out, _ = e.run("bad-token-000000\n", "login", "--token-stdin", "--json")
	if code != 1 || !strings.Contains(out, "token_invalid") {
		t.Fatalf("bad token: %s", out)
	}

	// add: bad name, then a real mcp-url service with an upstream header.
	e.runJSON(1, &eo, "add", "my-notes", "--mcp-url", e.up.URL)
	if eo.Error.Code != "bad_name" {
		t.Fatalf("%+v", eo)
	}
	var added AddResult
	e.runJSON(0, &added, "add", "notes", "--mcp-url", e.up.URL, "--header", "X-App-Token: app-secret-123")
	if added.URL != "https://toprelay.hari.workers.dev/notes/mcp" || !added.FirstDeploy || added.Service.Gen != 1 {
		t.Fatalf("add %+v", added)
	}
	allowed := map[string]bool{}
	for _, v := range added.Tools {
		allowed[v.Name] = v.Allowed
	}
	if !allowed["search"] || !allowed["read"] || allowed["delete"] || allowed["bad.name"] {
		t.Fatalf("default policy %+v", added.Tools)
	}
	ws := strings.Join(added.Warnings, "\n")
	if !strings.Contains(ws, "bad.name") || !strings.Contains(ws, "toprelay tools notes --allow") {
		t.Fatalf("warnings %v", added.Warnings)
	}
	up := e.cf.LastUpload()
	if up.Binding("SERVICES")["text"] != `[{"name":"notes","kind":"mcp","gen":1}]` {
		t.Fatalf("SERVICES %v", up.Binding("SERVICES"))
	}
	ar, _ := e.store.Get(secrets.AgentRoot)
	if up.Binding("AGENT_ROOT")["text"] != ar || ar == "" {
		t.Fatal("AGENT_ROOT not the stored root")
	}
	if v, _ := e.store.Get(secrets.UpstreamHeader("notes")); v != "app-secret-123" {
		t.Fatal("upstream header not in keyring")
	}
	raw, _ := readFile(config.Path(e.dir))
	if strings.Contains(raw, "app-secret-123") || strings.Contains(raw, ar) {
		t.Fatal("secret in config.json")
	}
	e.runJSON(1, &eo, "add", "notes", "--mcp-url", e.up.URL)
	if eo.Error.Code != "exists" {
		t.Fatalf("%+v", eo)
	}

	// A stdio service (the tiny server) with --tools-default allow-all, and an http service.
	tiny := buildTiny(t)
	var addedS AddResult
	e.runJSON(0, &addedS, "add", "tools", "--stdio", "--tools-default", "allow-all", "--env", "TINY_GREETING=greeting-s3cret-value", "--", tiny)
	if addedS.SSEURL != "https://toprelay.hari.workers.dev/tools/mcp/sse" || addedS.Service.Tools.Default != "allow" || len(addedS.Tools) != 4 {
		t.Fatalf("stdio add %+v", addedS)
	}
	if !strings.Contains(strings.Join(addedS.Warnings, " "), "permissions") {
		t.Fatal("stdio permission warning missing")
	}
	// --env values go to the keyring: config.json and the --json output only carry the names.
	if addedS.Service.Source.Env != nil {
		t.Fatalf("env values in the --json output: %+v", addedS.Service.Source)
	}
	if raw, _ := readFile(config.Path(e.dir)); strings.Contains(raw, "greeting-s3cret-value") || !strings.Contains(raw, `"envNames": [`) {
		t.Fatalf("env value in config.json, or names missing:\n%s", raw)
	}
	if v, err := e.store.Get("stdio-env/tools/TINY_GREETING"); err != nil || v != "greeting-s3cret-value" {
		t.Fatalf("env value in keyring: %q %v", v, err)
	}
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "web %s", r.URL.Path) }))
	defer web.Close()
	var addedH AddResult
	e.runJSON(0, &addedH, "add", "web", "--http", web.URL, "--allow-path", "api")
	if addedH.URL != "https://toprelay.hari.workers.dev/web/" || strings.Join(addedH.Service.AllowPaths, ",") != "/api" {
		t.Fatalf("http add %+v", addedH)
	}
	if e.cf.UploadCount() != 3 {
		t.Fatal("each add redeploys")
	}
	if _, has := e.cf.LastUpload().Metadata["migrations"]; has {
		t.Fatal("migrations on a redeploy")
	}
	e.syncRelay()

	// list
	var list struct {
		PublicURL string        `json:"publicUrl"`
		Services  []ListService `json:"services"`
	}
	e.runJSON(0, &list, "list")
	if len(list.Services) != 3 || list.Services[0].Name != "notes" || list.Services[2].Kind != "http" {
		t.Fatalf("list %+v", list)
	}

	// tools: show, change, reset.
	var tr ToolsResult
	e.runJSON(0, &tr, "tools", "notes")
	if !tr.Reachable || len(tr.Tools) != 4 || tr.Changed {
		t.Fatalf("tools %+v", tr)
	}
	e.runJSON(0, &tr, "tools", "notes", "--allow", "delete")
	if !tr.Changed || !tr.Policy.Allowed("delete") {
		t.Fatalf("tools allow %+v", tr)
	}
	e.runJSON(0, &tr, "tools", "notes", "--deny", "delete", "--allow-all")
	if tr.Policy.Default != "allow" || tr.Policy.Allowed("delete") {
		t.Fatalf("tools allow-all+deny %+v", tr)
	}
	e.runJSON(0, &tr, "tools", "notes", "--reset")
	// Reset honours annotations: read-only tools exposed (now and later), the rest need an explicit allow.
	exposed := map[string]bool{}
	for _, v := range tr.Tools {
		exposed[v.Name] = v.Allowed
	}
	if tr.Policy.Default != "readonly" || exposed["delete"] || !exposed["search"] || !exposed["read"] {
		t.Fatalf("tools reset %+v", tr)
	}
	e.runJSON(1, &eo, "tools", "web")
	if eo.Error.Code != "not_mcp" {
		t.Fatalf("%+v", eo)
	}

	// key: mint, verify against the root, narrow, revoke.
	var kr KeyResult
	e.runJSON(0, &kr, "key", "notes", "--tools", "search", "--for", "1h", "--client", "claude-api")
	br, _ := e.store.Get(secrets.BearerRoot)
	b, err := keys.Parse(kr.Key)
	if err != nil {
		t.Fatal(err)
	}
	vt, err := keys.Verify(br, b, keys.Request{Service: "notes", Gen: 1, Now: time.Now()})
	if err != nil || strings.Join(vt, ",") != "search" || kr.Expires == 0 {
		t.Fatalf("minted key: %v %v %+v", vt, err, kr)
	}
	sn := kr.Snippets["claude-api"]
	if len(kr.Snippets) != 1 || !strings.Contains(sn, `"authorization_token":"`+kr.Key+`"`) || !strings.Contains(sn, "mcp_toolset") || !strings.Contains(sn, "mcp-client-2025-11-20") {
		t.Fatalf("snippet %v", kr.Snippets)
	}
	e.runJSON(0, &kr, "key", "tools")
	for _, c := range Clients {
		if kr.Snippets[c] == "" {
			t.Fatalf("missing snippet %s", c)
		}
	}
	if !strings.Contains(kr.Snippets["qwen"], "/tools/mcp/sse") || len(kr.Warnings) == 0 {
		t.Fatalf("qwen/warnings %+v", kr)
	}
	var nk KeyResult
	e.runJSON(0, &nk, "key", "narrow", kr.Key, "--tools", "echo", "--for", "10m")
	nb, _ := keys.Parse(nk.Key)
	if nb.ID != kr.ID || strings.Join(nk.Tools, ",") != "echo" {
		t.Fatalf("narrow %+v", nk)
	}
	if _, err := keys.Verify(br, nb, keys.Request{Service: "tools", Gen: 1, Now: time.Now()}); err != nil {
		t.Fatal("narrowed key invalid:", err)
	}
	var rv map[string]any
	e.runJSON(0, &rv, "key", "revoke", "notes", kr.ID)
	if rv["revokedKeys"] != float64(1) || e.relay.Revoked("notes")[0] != kr.ID {
		t.Fatalf("revoke %v", rv)
	}
	e.runJSON(2, &eo, "key", "revoke", "notes", "xyz")
	if eo.Error.Code != "usage" {
		t.Fatalf("%+v", eo)
	}

	// pair
	var pr map[string]any
	e.runJSON(0, &pr, "pair", "notes", "--ttl", "5m")
	codeS, _ := pr["code"].(string)
	if len(codeS) < 10 || strings.ContainsAny(codeS, "01OIL") || e.relay.Pairs("notes")[0] != codeS || pr["expires"].(float64) < float64(time.Now().Unix()+290) {
		t.Fatalf("pair %v", pr)
	}
	e.runJSON(2, &eo, "pair", "notes", "--ttl", "2h")

	// status before up: not connected.
	var st StatusResult
	e.runJSON(0, &st, "status")
	if len(st.Services) != 3 || st.Services[0].Connected || st.FreeLimit != 100000 || st.RequestsToday != 3*123 {
		t.Fatalf("status %+v", st)
	}

	// up: everything connects; state.json follows.
	ctx, cancel := context.WithCancel(context.Background())
	upDone := make(chan int, 1)
	var upErr bytes.Buffer
	var upMu sync.Mutex
	go func() {
		a, _, _ := e.app("")
		a.Ctx = ctx
		a.Stderr = &lockedWriter{w: &upErr, mu: &upMu}
		upDone <- Main([]string{"up"}, a)
	}()
	for _, s := range []string{"notes", "tools", "web"} {
		e.relay.WaitConnected(t, s, 1)
	}
	waitFor(t, func() bool {
		s, err := daemon.ReadState(daemon.StatePath(e.dir))
		return err == nil && len(s.Services) == 3 && s.Services["notes"].Connected && s.Services["tools"].Connected
	})
	e.runJSON(0, &st, "status")
	if !st.Services[0].Connected || st.Daemon == nil || !st.Daemon.Running || st.Services[0].Local == nil || !st.Services[0].Local.Connected {
		t.Fatalf("status with up %+v", st)
	}

	// test: through the relay, for each kind.
	var tres TestResult
	e.runJSON(0, &tres, "test", "notes")
	if !tres.OK || strings.Join(tres.Tools, ",") != "read,search" || tres.RemoteMs <= 0 || tres.Protocol != "2026-07-28" {
		t.Fatalf("test notes %+v", tres)
	}
	e.runJSON(0, &tres, "test", "tools")
	if !tres.OK || len(tres.Tools) != 4 {
		t.Fatalf("test tools %+v", tres)
	}
	e.runJSON(0, &tres, "test", "web")
	if !tres.OK || tres.Status != 200 || tres.Call != "GET /api" {
		t.Fatalf("test web %+v", tres)
	}

	// logs --json: one JSON object per line, with the test key as client.
	code, out, _ = e.run("", "logs", "notes", "--json", "-n", "0")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) < 3 {
		t.Fatalf("logs %d %q", code, out)
	}
	var rec calllog.Record
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil || !strings.HasPrefix(rec.Client, "key:") || rec.MCPMethod == "" {
		t.Fatalf("log line %q %v", lines[len(lines)-1], err)
	}
	// logs -f --json streams new calls.
	fctx, fcancel := context.WithCancel(context.Background())
	var fout bytes.Buffer
	var fmu sync.Mutex
	fdone := make(chan struct{})
	go func() {
		a, _, _ := e.app("")
		a.Ctx = fctx
		a.Stdout = &lockedWriter{w: &fout, mu: &fmu}
		Main([]string{"logs", "notes", "-f", "-n", "1", "--json"}, a)
		close(fdone)
	}()
	time.Sleep(100 * time.Millisecond)
	e.runJSON(0, &tres, "test", "notes")
	waitFor(t, func() bool { fmu.Lock(); defer fmu.Unlock(); return strings.Count(fout.String(), "\n") >= 3 })
	fcancel()
	<-fdone

	// doctor: all green against the fakes.
	var rep struct {
		OK     bool `json:"ok"`
		Checks []struct{ Name, Status, Detail, Fix string }
	}
	e.runJSON(0, &rep, "doctor")
	if !rep.OK {
		t.Fatalf("doctor %+v", rep)
	}
	names := ""
	for _, c := range rep.Checks {
		names += c.Name + ";"
	}
	for _, want := range []string{"config", "cloudflare token", "cloudflare permissions", "root secrets", "worker deployed", "relay reachable", "clock skew", "local service: notes", "agent connected: tools"} {
		if !strings.Contains(names, want) {
			t.Fatalf("doctor lacks %q: %s", want, names)
		}
	}

	// rotate: gen 2, OAuth revoked with the new key, the running up reconnects.
	e.relay.SetService(fakerelay.Service{Name: "notes", Kind: "mcp", Gen: 2})
	var rot map[string]any
	e.runJSON(0, &rot, "rotate", "notes")
	if rot["gen"] != float64(2) || e.relay.OAuthRevocations("notes") != 1 {
		t.Fatalf("rotate %v", rot)
	}
	if !strings.Contains(e.cf.LastUpload().Binding("SERVICES")["text"].(string), `{"name":"notes","kind":"mcp","gen":2}`) {
		t.Fatal("rotate didn't redeploy gen 2")
	}
	e.relay.WaitConnected(t, "notes", 2)

	// remove: OAuth revoked, redeployed without it, secrets gone; up drops it.
	var rm map[string]any
	e.runJSON(0, &rm, "remove", "notes")
	if rm["removed"] != true || e.relay.OAuthRevocations("notes") != 2 {
		t.Fatalf("remove %v", rm)
	}
	if strings.Contains(e.cf.LastUpload().Binding("SERVICES")["text"].(string), "notes") {
		t.Fatal("notes still deployed")
	}
	if _, err := e.store.Get(secrets.UpstreamHeader("notes")); err == nil {
		t.Fatal("upstream secret kept")
	}
	waitForMsg(t, func() string { upMu.Lock(); defer upMu.Unlock(); return upErr.String() }, func() bool {
		s, _ := daemon.ReadState(daemon.StatePath(e.dir))
		return s != nil && s.Services["notes"] == nil
	})

	cancel()
	select {
	case c := <-upDone:
		if c != 0 {
			t.Fatalf("up exit %d: %s", c, upErr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("up didn't stop")
	}
	upMu.Lock()
	logs := upErr.String()
	upMu.Unlock()
	for _, secret := range []string{ar, br, e.cf.Token, "app-secret-123", "tra1."} {
		if strings.Contains(logs, secret) {
			t.Fatalf("secret %q in up logs", secret[:5])
		}
	}
}

func TestHumanOutputAndHelp(t *testing.T) {
	e := newEnv(t)
	code, out, _ := e.run("")
	if code != 0 || !strings.Contains(out, "Commands:") {
		t.Fatal(out)
	}
	for _, c := range commands() {
		code, _, errs := e.run("", c.name, "--help")
		if code != 0 || !strings.Contains(errs, "Usage: toprelay "+c.name) {
			t.Fatalf("%s --help: %d %s", c.name, code, errs)
		}
	}
	code, _, errs := e.run("", "list")
	if code != 1 || !strings.Contains(errs, "error: no TopRelay configuration") || !strings.Contains(errs, "fix: run `toprelay login`") {
		t.Fatalf("human error: %s", errs)
	}
	code, out, _ = e.run(e.cf.Token+"\n", "login", "--token-stdin")
	if code != 0 || !strings.Contains(out, "✓ account: Hari's Account") || strings.Contains(out, e.cf.Token) {
		t.Fatalf("human login: %s", out)
	}
	code, out, _ = e.run("", "version", "--json")
	if code != 0 || strings.TrimSpace(out) != `{"version":"`+Version+`"}` {
		t.Fatal(out)
	}
	code, _, errs = e.run("", "login")
	if code != 1 || !strings.Contains(errs, "--token-stdin") {
		t.Fatalf("login without tty: %s", errs)
	}
}

func TestInteractiveAdd(t *testing.T) {
	e := newEnv(t)
	e.run(e.cf.Token+"\n", "login", "--token-stdin")
	a, outb, _ := e.app("y\nn\n")
	a.IsTTY = func() bool { return true }
	code := Main([]string{"add", "notes", "--mcp-url", e.up.URL, "--header", "X-App-Token: app-secret-123"}, a)
	out := outb.String()
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	cfg, _ := config.Load(e.dir)
	p := cfg.Services[0].Tools
	// Non-read-only tools are asked in server order (sorted): bad.name (y), delete (n).
	ro := config.Hint{Known: true, ReadOnly: true}
	rw := config.Hint{Known: true, Destructive: true}
	if !p.Base("bad.name", rw) || p.Base("delete", rw) || !p.Base("search", ro) || p.Default != config.DefaultReadOnly {
		t.Fatalf("policy %+v\n%s", p, out)
	}
	if !strings.Contains(out, "claude.ai") || !strings.Contains(out, "toprelay pair notes") || !strings.Contains(out, "Qwen") {
		t.Fatalf("connection steps missing:\n%s", out)
	}
}

func TestInstallJSON(t *testing.T) {
	e := newEnv(t)
	config.Save(e.dir, &config.Config{})
	var calls []string
	a, out, _ := e.app("")
	home := t.TempDir()
	a.Installer = func() installer {
		return &daemon.Installer{GOOS: "linux", Home: home, Exe: "/bin/toprelay", LogDir: e.dir + "/logs",
			Run: func(n string, args ...string) error { calls = append(calls, n); return nil }}
	}
	if code := Main([]string{"up", "--install", "--json"}, a); code != 0 {
		t.Fatal(out.String())
	}
	var r map[string]any
	json.Unmarshal(out.Bytes(), &r)
	if r["installed"] != true || !strings.HasSuffix(r["path"].(string), "toprelay.service") || len(calls) == 0 {
		t.Fatalf("%v %v", r, calls)
	}
}

func TestParseHelpers(t *testing.T) {
	for in, want := range map[string]time.Duration{"1h": time.Hour, "7d": 7 * 24 * time.Hour, "30m": 30 * time.Minute} {
		if d, err := parseDuration(in); err != nil || d != want {
			t.Errorf("%s: %v %v", in, d, err)
		}
	}
	for _, bad := range []string{"", "x", "-1h", "0d", "1.5d"} {
		if _, err := parseDuration(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	args, j := stripJSON([]string{"add", "x", "--json", "--stdio", "--", "cmd", "--json"})
	if !j || strings.Join(args, " ") != "add x --stdio -- cmd --json" {
		t.Fatalf("stripJSON %v", args)
	}
	c, err := PairCode(12)
	if err != nil || len(c) != 12 || strings.ContainsAny(c, "01OIL") {
		t.Fatal(c, err)
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func waitForMsg(t *testing.T, msg func() string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	t.Fatalf("condition not met in time\n%s\n%s", msg(), buf[:n])
}

func buildTiny(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tinyserver")
	out, err := exec.Command("go", "build", "-o", bin, "../stdiobridge/testdata/tinyserver").CombinedOutput()
	if err != nil {
		t.Fatalf("build tinyserver: %v\n%s", err, out)
	}
	return bin
}

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}
