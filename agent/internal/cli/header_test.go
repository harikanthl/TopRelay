package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/harikanthl/toprelay/agent/internal/caller"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
)

func (e *env) login() {
	e.t.Helper()
	if code, out, errs := e.run(e.cf.Token+"\n", "login", "--token-stdin", "--json"); code != 0 {
		e.t.Fatalf("login: %s %s", out, errs)
	}
}

// runStdinJSON is runJSON with stdin.
func (e *env) runStdinJSON(stdin string, want int, v any, args ...string) string {
	e.t.Helper()
	code, out, errs := e.run(stdin, append([]string{"--json"}, args...)...)
	if code != want {
		e.t.Fatalf("%v: exit %d, want %d\nstdout: %s\nstderr: %s", args, code, want, out, errs)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(v); err != nil || dec.More() {
		e.t.Fatalf("%v: stdout is not one JSON object: %v\n%s", args, err, out)
	}
	return out
}

func TestHeaderFlagsAndSetHeader(t *testing.T) {
	e := newEnv(t)
	e.login()

	// --header-stdin: the value goes to the keyring and never into config.json or the output.
	var added AddResult
	out := e.runStdinJSON("X-App-Token: app-secret-123\n", 0, &added, "add", "notes", "--mcp-url", e.up.URL, "--header-stdin")
	if strings.Contains(out, "app-secret-123") || added.Service.UpstreamHeader != "X-App-Token" || added.Service.UpstreamHeaderFile != "" {
		t.Fatalf("add --header-stdin: %s", out)
	}
	if len(added.Tools) == 0 {
		t.Fatal("the tools weren't listed with the stdin header")
	}
	if v, _ := e.store.Get(secrets.UpstreamHeader("notes")); v != "app-secret-123" {
		t.Fatalf("keyring %q", v)
	}
	for _, w := range added.Warnings {
		if strings.Contains(w, "command line") {
			t.Fatal("--header-stdin warned about the command line")
		}
	}
	// add creates the service's caller key, 0600 in a 0700 directory.
	keyPath := caller.DefaultKeyPath(e.dir, "notes")
	st, err := os.Stat(keyPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("caller key %v %v", st, err)
	}

	// --header-file: checked now, read later; nothing in the keyring.
	tok := filepath.Join(e.dir, "app-token")
	os.WriteFile(tok, []byte("app-secret-123\n"), 0o644)
	var eo errorOut
	e.runJSON(1, &eo, "add", "notes2", "--mcp-url", e.up.URL, "--header-file", "X-App-Token="+tok)
	if eo.Error.Code != "header_file" || !strings.Contains(eo.Error.Fix, "chmod 600") {
		t.Fatalf("0644 token file: %+v", eo)
	}
	os.Chmod(tok, 0o600)
	e.runJSON(0, &added, "add", "notes2", "--mcp-url", e.up.URL, "--header-file", "X-App-Token="+tok)
	if added.Service.UpstreamHeaderFile != tok || len(added.Tools) == 0 {
		t.Fatalf("add --header-file %+v", added)
	}
	if _, err := e.store.Get(secrets.UpstreamHeader("notes2")); err == nil {
		t.Fatal("a file header was copied into the keyring")
	}

	// --header still works, with a warning; two header flags are a usage error.
	e.runJSON(0, &added, "add", "notes3", "--mcp-url", e.up.URL, "--header", "X-App-Token: app-secret-123")
	if !strings.Contains(strings.Join(added.Warnings, "\n"), "--header-file or --header-stdin") {
		t.Fatalf("no warning for --header: %v", added.Warnings)
	}
	code, _, _ := e.run("X-A: b\n", "--json", "add", "notes4", "--mcp-url", e.up.URL, "--header-stdin", "--header", "X-A: b")
	if code != 2 {
		t.Fatalf("two header flags: exit %d", code)
	}
	for _, bad := range []string{"Host: x", "X-TopRelay-Caller: x", "Bad Name: x", "NoColon"} {
		code, _, _ := e.run(bad+"\n", "--json", "add", "notes4", "--mcp-url", e.up.URL, "--header-stdin")
		if code != 2 {
			t.Errorf("header %q: exit %d", bad, code)
		}
	}
	code, out, _ = e.run("", "--json", "add", "notes4", "--mcp-url", e.up.URL, "--header-stdin")
	if code != 2 || !strings.Contains(out, "nothing on stdin") {
		t.Fatalf("empty stdin: %d %s", code, out)
	}
	e.runStdinJSON("X-A: b\n", 2, &eo, "add", "x", "--stdio", "--header-stdin", "--", "true")
	if !strings.Contains(eo.Error.Message, "--env") {
		t.Fatalf("stdio with a header: %+v", eo)
	}
	e.runJSON(2, &eo, "add", "notes4", "--mcp-url", e.up.URL, "--pin-bundle-id", "com.example.app")
	if !strings.Contains(eo.Error.Message, "proves nothing") {
		t.Fatalf("bundle id alone: %+v", eo)
	}

	// set-header: live, no redeploy, exact JSON.
	uploads := e.cf.UploadCount()
	var sh map[string]any
	out = e.runStdinJSON("X-App-Token: rotated-456\n", 0, &sh, "set-header", "notes", "--stdin")
	if sh["service"] != "notes" || sh["header"] != "X-App-Token" || sh["updated"] != true || sh["source"] != "keyring" || strings.Contains(out, "rotated-456") {
		t.Fatalf("set-header %s", out)
	}
	if v, _ := e.store.Get(secrets.UpstreamHeader("notes")); v != "rotated-456" {
		t.Fatal("keyring not updated")
	}
	cfg, _ := config.Load(e.dir)
	if s := cfg.Find("notes"); s.UpstreamHeaderRev != 1 {
		t.Fatalf("rev %d", s.UpstreamHeaderRev)
	}
	e.runJSON(0, &sh, "set-header", "notes", "--file", "X-App-Token="+tok)
	if sh["source"] != "file" || sh["file"] != tok {
		t.Fatalf("set-header --file %v", sh)
	}
	if _, err := e.store.Get(secrets.UpstreamHeader("notes")); err == nil {
		t.Fatal("the keyring value outlived the switch to a file")
	}
	e.runJSON(0, &sh, "set-header", "notes", "--clear")
	if sh["source"] != "none" || sh["updated"] != true {
		t.Fatalf("clear %v", sh)
	}
	cfg, _ = config.Load(e.dir)
	if s := cfg.Find("notes"); s.UpstreamHeader != "" || s.UpstreamHeaderFile != "" || s.UpstreamHeaderRev != 3 {
		t.Fatalf("after clear %+v", s)
	}
	if e.cf.UploadCount() != uploads {
		t.Fatal("set-header redeployed the Worker")
	}
	e.runJSON(2, &eo, "set-header", "notes", "--stdin", "--clear")
	e.runJSON(2, &eo, "set-header", "notes")

	// list shows where each credential comes from.
	var list struct {
		Services []ListService `json:"services"`
	}
	e.runJSON(0, &list, "list")
	src := map[string]string{}
	for _, s := range list.Services {
		src[s.Name] = s.UpstreamHeaderSource
		if s.CallerKeyFile != caller.DefaultKeyPath(e.dir, s.Name) {
			t.Errorf("%s caller key %q", s.Name, s.CallerKeyFile)
		}
	}
	if src["notes"] != "" || src["notes2"] != "file" || src["notes3"] != "keyring" {
		t.Fatalf("sources %v", src)
	}
}

func TestCallerKeyCommand(t *testing.T) {
	e := newEnv(t)
	e.login()
	var added AddResult
	e.runJSON(0, &added, "add", "notes", "--mcp-url", e.up.URL, "--header", "X-App-Token: app-secret-123")
	var ck CallerKeyResult
	e.runJSON(0, &ck, "caller-key", "notes")
	b, _ := os.ReadFile(caller.DefaultKeyPath(e.dir, "notes"))
	if ck.Key != strings.TrimSpace(string(b)) || len(ck.Key) != 43 || ck.Header != "X-TopRelay-Caller" || ck.Source != "toprelay" || ck.Rotated {
		t.Fatalf("caller-key %+v", ck)
	}
	old := ck.Key
	e.runJSON(0, &ck, "caller-key", "notes", "--rotate")
	if !ck.Rotated || ck.Key == old {
		t.Fatalf("rotate %+v", ck)
	}

	// An app-owned key file (e.g. in a sandboxed app's container).
	appKey := filepath.Join(e.dir, "app-caller.key")
	os.WriteFile(appKey, []byte("short"), 0o600)
	var eo errorOut
	e.runJSON(1, &eo, "caller-key", "notes", "--file", appKey)
	if eo.Error.Code != "caller_key" {
		t.Fatalf("short key %+v", eo)
	}
	os.WriteFile(appKey, []byte(strings.Repeat("k", 40)+"\n"), 0o600)
	e.runJSON(0, &ck, "caller-key", "notes", "--file", appKey)
	if ck.Source != "app" || ck.Path != appKey || ck.Key != strings.Repeat("k", 40) {
		t.Fatalf("app key %+v", ck)
	}
	e.runJSON(1, &eo, "caller-key", "notes", "--rotate")
	if eo.Error.Code != "app_key" {
		t.Fatalf("rotating an app key %+v", eo)
	}
	e.runJSON(0, &ck, "caller-key", "notes", "--default")
	if ck.Source != "toprelay" || ck.Key != strings.TrimSpace(readKey(t, caller.DefaultKeyPath(e.dir, "notes"))) {
		t.Fatalf("back to default %+v", ck)
	}

	// remove deletes the key TopRelay made.
	var rm map[string]any
	e.runJSON(0, &rm, "remove", "notes")
	if _, err := os.Stat(caller.DefaultKeyPath(e.dir, "notes")); !os.IsNotExist(err) {
		t.Fatal("caller key left behind after remove")
	}
}

func readKey(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAddUnixSocket(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Unix socket upstreams need macOS or Linux")
	}
	e := newEnv(t)
	e.login()
	dir, err := os.MkdirTemp("/tmp", "trc") // short: macOS caps socket paths at 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	serve := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		srv := &http.Server{Handler: e.up.Config.Handler}
		go srv.Serve(l)
		t.Cleanup(func() { srv.Close() })
		return p
	}
	good := serve("ok.sock", 0o600)
	var added AddResult
	e.runStdinJSON("X-App-Token: app-secret-123\n", 0, &added, "add", "sock", "--mcp-url", "unix://"+good+"?path=/", "--header-stdin")
	if len(added.Tools) == 0 || added.Service.Source.URL != "unix://"+good+"?path=/" {
		t.Fatalf("add over a socket %+v", added)
	}
	open := serve("open.sock", 0o666)
	var eo errorOut
	e.runStdinJSON("X-App-Token: app-secret-123\n", 1, &eo, "add", "sock2", "--mcp-url", "unix://"+open+"?path=/", "--header-stdin")
	if eo.Error.Code != "local_unreachable" || !strings.Contains(eo.Error.Message, "other users") {
		t.Fatalf("0666 socket %+v", eo)
	}
	e.runJSON(1, &eo, "add", "sock3", "--mcp-url", "unix://relative.sock")
	if eo.Error.Code != "bad_url" {
		t.Fatalf("bad socket URL %+v", eo)
	}
}

func TestLogsFlags(t *testing.T) {
	e := newEnv(t)
	e.login()
	var added AddResult
	e.runJSON(0, &added, "add", "notes", "--mcp-url", e.up.URL, "--header", "X-App-Token: app-secret-123")
	var m map[string]any
	e.runJSON(0, &m, "logs", "--retention", "30")
	if m["logRetentionDays"] != float64(30) {
		t.Fatal(m)
	}
	cfg, _ := config.Load(e.dir)
	if cfg.LogRetentionDays != 30 {
		t.Fatal("not saved")
	}
	var eo errorOut
	e.runJSON(2, &eo, "logs", "notes", "--retention", "3")
	if code, out, _ := e.run("", "logs", "--service", "notes", "--json"); code != 0 || out != "" {
		t.Fatalf("logs --service: %d %q", code, out)
	}
}

func TestLimitsCommand(t *testing.T) {
	e := newEnv(t)
	e.login()
	var added AddResult
	e.runJSON(0, &added, "add", "notes", "--mcp-url", e.up.URL, "--header", "X-App-Token: app-secret-123")
	var m map[string]any
	e.runJSON(0, &m, "limits", "notes", "--client", "60/m", "--tool", "render=10/m", "--tool", "ocr=1/s")
	cfg, _ := config.Load(e.dir)
	l := cfg.Find("notes").Limits
	if l == nil || l.Client != "60/m" || l.Tools["render"] != "10/m" || l.Tools["ocr"] != "1/s" || m["changed"] != true {
		t.Fatalf("%+v %v", l, m)
	}
	e.runJSON(0, &m, "limits", "notes", "--tool", "ocr=off", "--client", "off")
	cfg, _ = config.Load(e.dir)
	if l := cfg.Find("notes").Limits; l.Client != "" || len(l.Tools) != 1 {
		t.Fatalf("off: %+v", l)
	}
	var eo errorOut
	e.runJSON(2, &eo, "limits", "notes", "--service", "lots")
	e.runJSON(2, &eo, "limits", "notes", "--tool", "render")
	e.runJSON(0, &m, "limits", "notes", "--clear")
	cfg, _ = config.Load(e.dir)
	if cfg.Find("notes").Limits != nil {
		t.Fatal("clear")
	}
	e.runJSON(0, &m, "limits", "notes")
	if m["changed"] != false {
		t.Fatal("show changed something")
	}
}
