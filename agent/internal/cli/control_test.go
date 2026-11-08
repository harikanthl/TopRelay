package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/daemon"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/testutil/cffake"
	"github.com/harikanthl/toprelay/agent/internal/testutil/fakerelay"
)

// newShortEnv is newEnv in a short directory: macOS caps socket paths at 104 bytes.
func newShortEnv(t *testing.T) *env {
	dir, err := os.MkdirTemp("/tmp", "trk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return &env{t: t, dir: dir, store: &secrets.File{Path: filepath.Join(dir, "secrets.json")}, cf: cffake.New(t),
		relay: fakerelay.New(t), up: upstreamMCP(t), vars: map[string]string{}}
}

func socketClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}}
}

func cliOver(t *testing.T, c *http.Client, stdin string, args ...string) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"args": args, "stdin": stdin})
	resp, err := c.Post("http://agent/v1/cli", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&m); err != nil || dec.More() {
		t.Fatalf("%v: not one JSON object: %v", args, err)
	}
	return resp.StatusCode, m
}

func TestControlSocket(t *testing.T) {
	e := newShortEnv(t)
	e.login()
	var added AddResult
	e.runJSON(0, &added, "add", "notes", "--mcp-url", e.up.URL, "--header", "X-App-Token: app-secret-123")
	e.syncRelay()
	a, _, _ := e.app("")
	ctx, cancel := context.WithCancel(context.Background())
	a.Ctx = ctx
	done := make(chan int, 1)
	go func() { done <- Main([]string{"up"}, a) }()
	sock := ControlPath(e.dir)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no control socket")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st, _ := os.Stat(sock); st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", st.Mode())
	}
	c := socketClient(sock)

	// The watch stream: a line now, and another once the service connects.
	wresp, err := c.Get("http://agent/v1/state/watch")
	if err != nil {
		t.Fatal(err)
	}
	defer wresp.Body.Close()
	lines := make(chan daemon.State, 16)
	go func() {
		sc := bufio.NewScanner(wresp.Body)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "" {
				continue
			}
			var s daemon.State
			if json.Unmarshal(sc.Bytes(), &s) == nil {
				lines <- s
			}
		}
		close(lines)
	}()
	connected := false
	for !connected {
		select {
		case s := <-lines:
			if s.Version != daemon.StateVersion || !s.Running {
				t.Fatalf("state %+v", s)
			}
			connected = s.Services["notes"] != nil && s.Services["notes"].Connected
		case <-time.After(10 * time.Second):
			t.Fatal("watch never showed the service connected")
		}
	}

	// Commands give exactly the CLI's --json objects.
	code, m := cliOver(t, c, "", "version")
	if code != 200 || m["version"] != Version {
		t.Fatalf("version %d %v", code, m)
	}
	code, m = cliOver(t, c, "X-App-Token: rotated\n", "set-header", "notes", "--stdin")
	if code != 200 || m["updated"] != true || m["header"] != "X-App-Token" {
		t.Fatalf("set-header %d %v", code, m)
	}
	if v, _ := e.store.Get(secrets.UpstreamHeader("notes")); v != "rotated" {
		t.Fatal("set-header over the socket didn't store the value")
	}
	code, m = cliOver(t, c, "", "pair", "nope")
	if code != 422 || m["error"].(map[string]any)["code"] != "no_service" {
		t.Fatalf("error %d %v", code, m)
	}
	code, m = cliOver(t, c, "", "list", "--bogus")
	if code != 400 || m["error"].(map[string]any)["code"] != "usage" {
		t.Fatalf("usage %d %v", code, m)
	}
	for _, refused := range [][]string{{"up"}, {"status", "--watch"}, {"status", "--watch=1"}, {"status", "-watch=t"}, {"logs", "notes", "-f"}, {}, {"help"}} {
		if code, _ := cliOver(t, c, "", refused...); code != 400 {
			t.Errorf("%v allowed over the socket", refused)
		}
	}
	// Offering a file: an id the app can return as toprelay-file://<id>.
	f := filepath.Join(e.dir, "offer.txt")
	os.WriteFile(f, []byte("hi"), 0o600)
	oresp, err := c.Post("http://agent/v1/files", "application/json", strings.NewReader(`{"service":"notes","path":"`+f+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	var offer map[string]any
	json.NewDecoder(oresp.Body).Decode(&offer)
	oresp.Body.Close()
	if oresp.StatusCode != 200 || offer["uri"] != "toprelay-file://"+offer["id"].(string) {
		t.Fatalf("offer %d %v", oresp.StatusCode, offer)
	}
	for _, bad := range []string{`{"service":"nope","path":"` + f + `"}`, `{"service":"notes","path":"relative"}`, `{}`} {
		r, _ := c.Post("http://agent/v1/files", "application/json", strings.NewReader(bad))
		r.Body.Close()
		if r.StatusCode == 200 {
			t.Errorf("offer %s accepted", bad)
		}
	}
	resp, err := c.Get("http://agent/v1/state")
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(err)
	}
	resp.Body.Close()

	// A second `up` on the same config dir is refused instead of fighting over the relay connection.
	a2, out2, _ := e.app("")
	if code := Main([]string{"--json", "up"}, a2); code != 1 || !strings.Contains(out2.String(), "already_running") {
		t.Fatalf("second up: %d %s", code, out2)
	}

	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("up exit %d", code)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("socket left behind")
	}
	// A stale socket (agent killed) doesn't block the next start.
	l, _ := net.Listen("unix", sock)
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	ll, err := listenControl(e.dir)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	ll.Close()
}

// TopRelayKit against a real `up`: builds the Swift smoke tool and runs it. Needs Swift; TOPRELAY_SWIFT_TESTS=1.
func TestTopRelayKitEndToEnd(t *testing.T) {
	if os.Getenv("TOPRELAY_SWIFT_TESTS") != "1" {
		t.Skip("set TOPRELAY_SWIFT_TESTS=1 to build and run TopRelayKit against a real agent")
	}
	pkg, _ := filepath.Abs("../../../TopRelayKit")
	build := exec.Command("swift", "build", "--product", "trkit-smoke", "--package-path", pkg)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("swift build: %v\n%s", err, out)
	}
	binDir, err := exec.Command("swift", "build", "--package-path", pkg, "--show-bin-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	smoke := filepath.Join(strings.TrimSpace(string(binDir)), "trkit-smoke")

	e := newShortEnv(t)
	e.login()
	var added AddResult
	e.runJSON(0, &added, "add", "notes", "--mcp-url", e.up.URL, "--header", "X-App-Token: app-secret-123")
	e.syncRelay()
	a, _, _ := e.app("")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Ctx = ctx
	done := make(chan int, 1)
	go func() { done <- Main([]string{"up"}, a) }()
	e.relay.WaitConnected(t, "notes", 1)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st, err := daemon.ReadState(daemon.StatePath(e.dir)); err == nil && st.Services["notes"] != nil && st.Services["notes"].Connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, err := exec.Command(smoke, e.dir, "notes").CombinedOutput()
	t.Logf("%s", out)
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "ok") {
		t.Fatalf("trkit-smoke: %v", err)
	}
	if v, _ := e.store.Get(secrets.UpstreamHeader("notes")); v != "from-kit" {
		t.Fatalf("credential set through the Kit: %q", v)
	}
	cancel()
	<-done
}
