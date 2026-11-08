package upstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/caller"
)

func TestParseEndpoint(t *testing.T) {
	good := []struct {
		raw, def, url, sock string
	}{
		{"http://127.0.0.1:7879/mcp", "/mcp", "http://127.0.0.1:7879/mcp", ""},
		{"https://localhost/x?y=1", "/mcp", "https://localhost/x?y=1", ""},
		{"unix:///tmp/app.sock", "/mcp", "http://localhost/mcp", "/tmp/app.sock"},
		{"unix:///tmp/app.sock", "/", "http://localhost/", "/tmp/app.sock"},
		{"unix:///tmp/app.sock?path=/rpc", "/mcp", "http://localhost/rpc", "/tmp/app.sock"},
		{"unix:///tmp/app.sock?path=%2Frpc%3Fa%3D1", "/mcp", "http://localhost/rpc?a=1", "/tmp/app.sock"},
	}
	for _, g := range good {
		e, err := ParseEndpoint(g.raw, g.def)
		if err != nil {
			t.Errorf("%s: %v", g.raw, err)
			continue
		}
		if e.URL.String() != g.url || e.Socket != g.sock {
			t.Errorf("%s: %s %q", g.raw, e.URL, e.Socket)
		}
	}
	for _, bad := range []string{"ftp://x", "http://", "unix://host/tmp/a.sock", "unix://tmp/a.sock", "unix:///tmp/../a.sock",
		"unix:///tmp/a.sock?x=1", "unix:///tmp/a.sock?path=rpc", "unix:///tmp/a.sock?path=//evil/x", "http://u:p@127.0.0.1/mcp", "::"} {
		if _, err := ParseEndpoint(bad, "/mcp"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	lb := map[string]bool{"http://127.0.0.1:1/": true, "http://127.9.9.9:1/": true, "http://[::1]:1/": true, "http://LOCALHOST:1/": true,
		"http://192.168.1.2:1/": false, "http://example.com/": false, "unix:///tmp/a.sock": false}
	for raw, want := range lb {
		e, _ := ParseEndpoint(raw, "/")
		if e.IsLoopback() != want {
			t.Errorf("%s loopback %v", raw, !want)
		}
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestReadSecretFile(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "token")
	writeFile(t, ok, "  tok-123\n", 0o600)
	if v, err := ReadSecretFile(ok); err != nil || v != "tok-123" {
		t.Fatalf("%q %v", v, err)
	}
	link := filepath.Join(dir, "link")
	os.Symlink(ok, link)
	if v, err := ReadSecretFile(link); err != nil || v != "tok-123" {
		t.Fatalf("symlink to a good file: %q %v", v, err)
	}
	cases := map[string]func() string{
		"group readable": func() string { p := filepath.Join(dir, "g"); writeFile(t, p, "x", 0o640); return p },
		"world readable": func() string { p := filepath.Join(dir, "w"); writeFile(t, p, "x", 0o604); return p },
		"empty":          func() string { p := filepath.Join(dir, "e"); writeFile(t, p, " \n", 0o600); return p },
		"line break":     func() string { p := filepath.Join(dir, "n"); writeFile(t, p, "a\r\nb", 0o600); return p },
		"too big": func() string {
			p := filepath.Join(dir, "b")
			writeFile(t, p, strings.Repeat("a", MaxSecretFile+1), 0o600)
			return p
		},
		"directory": func() string { p := filepath.Join(dir, "d"); os.Mkdir(p, 0o700); return p },
		"missing":   func() string { return filepath.Join(dir, "nope") },
		"relative":  func() string { return "token" },
	}
	for name, mk := range cases {
		if _, err := ReadSecretFile(mk()); !errors.Is(err, ErrSecretFile) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestHeaderNames(t *testing.T) {
	for _, n := range []string{"X-App-Token", "Authorization", "x_token"} {
		if err := ValidHeaderName(n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	for _, n := range []string{"", "X Token", "X:T", "Host", "x-toprelay-caller", "X-Forwarded-For", "Mcp-Session-Id"} {
		if ValidHeaderName(n) == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

// sockDir is a short private directory: macOS limits socket paths to 104 bytes, and t.TempDir is long.
func sockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "trs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func serveUnix(t *testing.T, path string, mode os.FileMode, h http.Handler) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
}

func TestUnixSocketUpstream(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Unix socket upstreams need macOS or Linux")
	}
	dir := sockDir(t)
	sock := filepath.Join(dir, "app.sock")
	var host, path atomic.Value
	serveUnix(t, sock, 0o600, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host.Store(r.Host)
		path.Store(r.URL.Path)
		io.WriteString(w, "hi")
	}))
	ep, err := ParseEndpoint("unix://"+sock, "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	u, err := New(Options{Endpoint: ep})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := u.Client().Get(ep.URL.String())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hi" || host.Load() != "localhost" || path.Load() != "/mcp" {
		t.Fatalf("%s host=%v path=%v", b, host.Load(), path.Load())
	}
	if err := u.Dial(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A socket others may connect to is refused, before any byte is sent.
	open := filepath.Join(dir, "open.sock")
	serveUnix(t, open, 0o666, http.NotFoundHandler())
	if err := CheckSocket(open); err == nil || !strings.Contains(err.Error(), "other users") {
		t.Fatalf("0666 socket: %v", err)
	}
	ep2, _ := ParseEndpoint("unix://"+open, "/mcp")
	u2, _ := New(Options{Endpoint: ep2})
	if _, err := u2.Client().Get(ep2.URL.String()); err == nil {
		t.Fatal("request to a 0666 socket went through")
	}
	// Not a socket.
	plain := filepath.Join(dir, "plain")
	writeFile(t, plain, "x", 0o600)
	if err := CheckSocket(plain); err == nil {
		t.Fatal("regular file accepted as a socket")
	}
	// A directory others can write to lets them swap the socket.
	shared := sockDir(t)
	os.Chmod(shared, 0o777)
	ssock := filepath.Join(shared, "a.sock")
	serveUnix(t, ssock, 0o600, http.NotFoundHandler())
	if err := CheckSocket(ssock); err == nil || !strings.Contains(err.Error(), "write") {
		t.Fatalf("world-writable dir: %v", err)
	}
	if err := CheckSocket(filepath.Join(dir, "missing.sock")); err == nil {
		t.Fatal("missing socket accepted")
	}
}

func TestPinLogic(t *testing.T) {
	var calls []string
	lsofOut := "p4242\nf9\nn127.0.0.1:7879->127.0.0.1:50000\np777\nf3\nn127.0.0.1:50000->127.0.0.1:7879\n"
	codesignErr := error(nil)
	p := &Pin{TeamID: "ABCDE12345", BundleID: "com.example.app"}
	p.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if strings.HasSuffix(name, "lsof") {
			return []byte(lsofOut), nil
		}
		return nil, codesignErr
	}
	if p.Requirement() != `anchor apple generic and certificate leaf[subject.OU] = "ABCDE12345" and identifier "com.example.app"` {
		t.Fatal(p.Requirement())
	}
	ctx := context.Background()
	// pid 777 is us (the client end); 4242 accepted the connection.
	pid, err := p.acceptorPID(ctx, 50000, 7879)
	if err != nil || pid != 4242 {
		t.Fatal(pid, err)
	}
	if !strings.Contains(calls[0], "-iTCP:50000 -sTCP:ESTABLISHED") {
		t.Fatalf("lsof call %q", calls[0])
	}
	if err := p.check(ctx, pid); err != nil {
		t.Fatal(err)
	}
	if err := p.check(ctx, pid); err != nil { // cached: no second codesign
		t.Fatal(err)
	}
	n := 0
	for _, c := range calls {
		if strings.Contains(c, "codesign") {
			n++
			if !strings.Contains(c, "--verify -R=anchor apple generic") || !strings.HasSuffix(c, " 4242") {
				t.Errorf("codesign call %q", c)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d codesign runs, want 1 (cache)", n)
	}
	codesignErr = errors.New("exit status 3")
	if err := p.check(ctx, 778); !errors.Is(err, ErrPin) {
		t.Fatalf("unsigned process accepted: %v", err)
	}
	// IPv6 form, and another connection's sockets on the same app port are ignored.
	lsofOut = "p5\nn[::1]:7879->[::1]:50000\np6\nn[::1]:7879->[::1]:50001\n"
	if pid, err := p.acceptorPID(ctx, 50000, 7879); err != nil || pid != 5 {
		t.Fatal("ipv6", pid, err)
	}
	// Nobody visible holds the other end (another user's process): refused after retries.
	lsofOut = "p777\nn127.0.0.1:50000->127.0.0.1:7879\n"
	if _, err := p.acceptorPID(ctx, 50000, 7879); !errors.Is(err, ErrPin) {
		t.Fatalf("invisible acceptor: %v", err)
	}
	lsofOut = "p1\nn127.0.0.1:7879->127.0.0.1:50000\np2\nn127.0.0.1:7879->127.0.0.1:50000\n"
	if _, err := p.acceptorPID(ctx, 50000, 7879); !errors.Is(err, ErrPin) {
		t.Fatalf("two acceptors: %v", err)
	}
	lsofOut = "pabc\n"
	if _, err := p.acceptorPID(ctx, 50000, 7879); !errors.Is(err, ErrPin) {
		t.Fatalf("garbage: %v", err)
	}
	for _, bad := range []*Pin{{TeamID: "abc"}, {TeamID: "ABCDE12345", BundleID: `x" or true or "`}} {
		if runtime.GOOS == "darwin" && bad.Validate() == nil {
			t.Errorf("%+v valid", bad)
		}
	}
	if runtime.GOOS != "darwin" {
		if (&Pin{TeamID: "ABCDE12345"}).Validate() == nil {
			t.Error("pin accepted off macOS")
		}
	}
}

// On macOS, the real lsof and codesign: the process that accepted the connection (this test process) is found,
// refused by a team pin (test binaries are ad-hoc signed), and accepted by a requirement naming its own identifier.
func TestPinRealDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer srv.Close()
	ep, _ := ParseEndpoint(srv.URL+"/mcp", "/mcp")
	port, _ := ep.Port()
	p := &Pin{TeamID: "ABCDE12345"}
	ctx := context.Background()
	c, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	appPort, _ := strconv.Atoi(port)
	pid, err := p.acceptorPID(ctx, c.LocalAddr().(*net.TCPAddr).Port, appPort)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("acceptor pid %d %v (want %d)", pid, err, os.Getpid())
	}
	u, err := New(Options{Endpoint: ep, Pin: p})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Client().Get(ep.URL.String()); err == nil || !errors.Is(err, ErrPin) {
		t.Fatalf("an unsigned app passed a team pin: %v", err)
	}
	out, _ := defaultRun(ctx, "/usr/bin/codesign", "-dv", strconv.Itoa(pid))
	// codesign -dv prints to stderr; defaultRun only returns stdout, so ask again with the error text.
	if len(out) == 0 {
		var errb bytes.Buffer
		cmd := exec.Command("/usr/bin/codesign", "-dv", strconv.Itoa(pid))
		cmd.Stderr = &errb
		cmd.Run()
		out = errb.Bytes()
	}
	m := regexp.MustCompile(`(?m)^Identifier=(\S+)`).FindSubmatch(out)
	if m == nil {
		t.Skipf("can't read this binary's identifier: %s", out)
	}
	if err := p.verify(ctx, pid, `identifier "`+string(m[1])+`"`); err != nil {
		t.Fatalf("own identifier refused: %v", err)
	}
}

func TestPrepare(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	writeFile(t, tokFile, "first\n", 0o600)
	keyFile := filepath.Join(dir, "caller.key")
	if _, err := caller.EnsureKeyFile(keyFile); err != nil {
		t.Fatal(err)
	}
	ep, _ := ParseEndpoint("http://127.0.0.1:1/mcp", "/mcp")
	now := time.Unix(1793500000, 0)
	u, err := New(Options{Endpoint: ep, Credential: &Credential{Name: "X-App-Token", File: tokFile}, CallerKeyFile: keyFile,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call"}`)
	r, _ := http.NewRequest("POST", "http://127.0.0.1:1/mcp?a=b", bytes.NewReader(body))
	r.Header.Set("X-Toprelay-Client", "key:0123456789abcdef")
	r.Header.Set(caller.Header, "v1; client=forged; ts=1; nonce=x; sig=y")
	if err := u.PrepareRelayed(r, body); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("X-App-Token") != "first" || len(r.Header.Values(caller.Header)) != 1 {
		t.Fatalf("headers %v", r.Header)
	}
	key, _ := ReadSecretFile(keyFile)
	c, err := caller.Verify([]byte(key), r.Header.Get(caller.Header), "POST", "/mcp?a=b", body, now)
	if err != nil || c.Client != "key:0123456789abcdef" {
		t.Fatalf("%+v %v", c, err)
	}

	// The app rotates its token by renaming a new file over the old one: the next request uses it.
	tmp := filepath.Join(dir, "token.new")
	writeFile(t, tmp, "second", 0o600)
	os.Rename(tmp, tokFile)
	r2, _ := http.NewRequest("GET", "http://127.0.0.1:1/mcp", nil)
	if err := u.PrepareLocal(r2); err != nil || r2.Header.Get("X-App-Token") != "second" {
		t.Fatalf("after rotation %q %v", r2.Header.Get("X-App-Token"), err)
	}
	if r2.Header.Get(caller.Header) != "" {
		t.Fatal("a local request was signed")
	}

	// Live swap to a keyring value, then the token file disappears: errors are ErrCredential, never a request
	// without the credential.
	u.SetCredential(&Credential{Name: "X-App-Token", Value: "kv"})
	r3, _ := http.NewRequest("GET", "http://127.0.0.1:1/mcp", nil)
	if err := u.PrepareLocal(r3); err != nil || r3.Header.Get("X-App-Token") != "kv" {
		t.Fatal("swap")
	}
	u.SetCredential(&Credential{Name: "X-App-Token", File: filepath.Join(dir, "gone")})
	if err := u.PrepareRelayed(r3, nil); !errors.Is(err, ErrCredential) {
		t.Fatalf("missing file: %v", err)
	}
	u.SetCredential(nil)
	os.Remove(keyFile)
	if err := u.PrepareRelayed(r3, nil); !errors.Is(err, ErrCredential) {
		t.Fatalf("missing caller key: %v", err)
	}
	short := filepath.Join(dir, "short.key")
	writeFile(t, short, "too-short", 0o600)
	u.SetCallerKeyFile(short)
	if err := u.PrepareRelayed(r3, nil); !errors.Is(err, ErrCredential) {
		t.Fatalf("short caller key: %v", err)
	}
	u.SetCallerKeyFile("")
	if err := u.PrepareRelayed(r3, nil); err != nil {
		t.Fatal("signing off", err)
	}

	// A pin on a non-local URL makes no sense.
	far, _ := ParseEndpoint("http://example.com/mcp", "/mcp")
	if runtime.GOOS == "darwin" {
		if _, err := New(Options{Endpoint: far, Pin: &Pin{TeamID: "ABCDE12345"}}); err == nil {
			t.Fatal("pin on a remote URL accepted")
		}
	}
}
