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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/daemon"
)

func TestTimeoutFlagAndLongRunning(t *testing.T) {
	args, d, set, err := stripTimeout([]string{"pair", "--timeout", "2s", "notes", "--", "--timeout", "x"})
	if err != nil || !set || d != 2*time.Second || strings.Join(args, " ") != "pair notes -- --timeout x" {
		t.Fatalf("%v %s %v %v", args, d, set, err)
	}
	if _, d, _, err := stripTimeout([]string{"--timeout=1m", "list"}); err != nil || d != time.Minute {
		t.Fatal(d, err)
	}
	for _, bad := range [][]string{{"--timeout"}, {"--timeout", "soon"}, {"--timeout=-1s"}} {
		if _, _, _, err := stripTimeout(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	long := map[string]bool{"up": true, "up --install": false, "up --uninstall": false, "logs notes -f": true, "logs notes": false,
		"status --watch": true, "status": false, "add x --stdio -- up --install": false}
	for line, want := range long {
		f := strings.Fields(line)
		if got := longRunning(f[0], f[1:]); got != want {
			t.Errorf("%q long-running %v", line, got)
		}
	}
}

// A command that respects cancellation ends with a timeout error object, quickly.
func TestTimeoutSoft(t *testing.T) {
	e := newEnv(t)
	e.login()
	var added AddResult
	e.runJSON(0, &added, "add", "notes", "--mcp-url", e.up.URL, "--header", "X-App-Token: app-secret-123")
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer hang.Close()
	defer close(release) // runs first: the handler returns, so Close doesn't wait on it
	a, out, _ := e.app("")
	a.RelayURL = hang.URL
	start := time.Now()
	code := Main([]string{"--json", "--timeout", "300ms", "pair", "notes"}, a)
	var eo errorOut
	if code != 1 || json.Unmarshal(out.Bytes(), &eo) != nil || eo.Error.Code != "timeout" || time.Since(start) > 3*time.Second {
		t.Fatalf("exit %d after %s: %s", code, time.Since(start), out)
	}
}

// A command that ignores cancellation: after the grace period the process prints the timeout error and exits.
func TestTimeoutHard(t *testing.T) {
	var out bytes.Buffer
	var gotExit atomic.Int32
	gotExit.Store(-1)
	a := &App{Ctx: context.Background(), JSON: true, Stderr: &bytes.Buffer{}, TimeoutGrace: 50 * time.Millisecond,
		Exit: func(c int) { gotExit.Store(int32(c)) }}
	g := &guardWriter{w: &out}
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	err := a.runBounded("add", 100*time.Millisecond, g, func() error { <-block; return nil })
	if err != exitCode(1) || gotExit.Load() != 1 || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v exit %d after %s", err, gotExit.Load(), time.Since(start))
	}
	var eo errorOut
	if json.Unmarshal(out.Bytes(), &eo) != nil || eo.Error.Code != "timeout" || !strings.Contains(eo.Error.Message, "add didn't finish within 100ms") {
		t.Fatalf("%s", out.String())
	}
	// A late write from the stuck command never reaches the caller.
	fmt.Fprint(g, `{"late":true}`)
	if strings.Contains(out.String(), "late") {
		t.Fatal("write after the timeout")
	}

	// The result was printed, then cleanup hung: exit 0 and no second object.
	out.Reset()
	g2 := &guardWriter{w: &out}
	a.emittedError = false
	block2 := make(chan struct{})
	defer close(block2)
	a.runBounded("up", 100*time.Millisecond, g2, func() error {
		fmt.Fprintln(g2, `{"installed":true}`)
		a.emitted.Store(true) // as emit does
		<-block2
		return nil
	})
	if gotExit.Load() != 0 || strings.TrimSpace(out.String()) != `{"installed":true}` {
		t.Fatalf("exit %d: %s", gotExit.Load(), out.String())
	}

	// A prompt is output but not a result: a hang after it still exits 1.
	out.Reset()
	g3 := &guardWriter{w: &out}
	a3 := &App{Ctx: context.Background(), Stderr: &bytes.Buffer{}, TimeoutGrace: 50 * time.Millisecond, Exit: func(c int) { gotExit.Store(int32(c)) }}
	block3 := make(chan struct{})
	defer close(block3)
	a3.runBounded("add", 100*time.Millisecond, g3, func() error { fmt.Fprint(g3, "Expose delete? [y/N] "); <-block3; return nil })
	if gotExit.Load() != 1 {
		t.Fatalf("prompt counted as a result: exit %d", gotExit.Load())
	}

	// In process (no Exit hook): the code comes back, and lingering says when the command really ends.
	a4 := &App{Ctx: context.Background(), JSON: true, Stderr: &bytes.Buffer{}, TimeoutGrace: 50 * time.Millisecond}
	release := make(chan struct{})
	err = a4.runBounded("add", 100*time.Millisecond, &guardWriter{w: &bytes.Buffer{}}, func() error { <-release; return nil })
	if err != exitCode(1) || a4.lingering == nil {
		t.Fatalf("in process: %v %v", err, a4.lingering)
	}
	select {
	case <-a4.lingering:
		t.Fatal("lingering closed while the command still runs")
	default:
	}
	close(release)
	select {
	case <-a4.lingering:
	case <-time.After(2 * time.Second):
		t.Fatal("lingering never closed")
	}
}

// A command that printed its report, then failed after its deadline, keeps its own result: one JSON object.
func TestTimeoutAfterReport(t *testing.T) {
	var out bytes.Buffer
	a := &App{Ctx: context.Background(), JSON: true, Stderr: &bytes.Buffer{}}
	g := &guardWriter{w: &out}
	err := a.runBounded("doctor", 50*time.Millisecond, g, func() error {
		fmt.Fprintln(g, `{"ok":false,"checks":[]}`)
		<-a.Ctx.Done()
		return errSilent
	})
	if err != errSilent || strings.Count(out.String(), "{") != 1 {
		t.Fatalf("%v %q", err, out.String())
	}
}

func TestBoolFlag(t *testing.T) {
	for args, want := range map[string]bool{"--watch": true, "-watch=1": true, "--watch=t": true, "--watch=TRUE": true,
		"--watch=false": false, "--watch=0": false, "--watch=junk": true, "--watchx": false, "": false, "-- --watch": false} {
		if got := boolFlag(strings.Fields(args), "watch"); got != want {
			t.Errorf("%q: %v", args, got)
		}
	}
	if !longRunning("status", []string{"--watch=1"}) || !longRunning("logs", []string{"x", "-f=t"}) || longRunning("up", []string{"--install=1"}) {
		t.Fatal("longRunning")
	}
}

func TestJSONStderrCap(t *testing.T) {
	var errb bytes.Buffer
	c := &capWriter{w: &errb, left: MaxJSONStderr}
	for range 10 {
		n, err := c.Write(bytes.Repeat([]byte("x"), 1000))
		if n != 1000 || err != nil {
			t.Fatal("cap writer must claim every byte, or the logger would retry or fail")
		}
	}
	if errb.Len() > MaxJSONStderr+100 || !strings.Contains(errb.String(), "truncated") {
		t.Fatalf("stderr %d bytes", errb.Len())
	}
}

func TestUpJSONLogsToFile(t *testing.T) {
	e := newEnv(t)
	e.login()
	var added AddResult
	e.runJSON(0, &added, "add", "notes", "--mcp-url", e.up.URL, "--header", "X-App-Token: app-secret-123")
	e.syncRelay()
	a, _, errb := e.app("")
	ctx, cancel := context.WithCancel(context.Background())
	a.Ctx = ctx
	done := make(chan int, 1)
	go func() { done <- Main([]string{"--json", "up"}, a) }()
	e.relay.WaitConnected(t, "notes", 1)
	cancel()
	<-done
	if errb.Len() != 0 {
		t.Fatalf("up --json wrote to stderr: %s", errb)
	}
	b, err := os.ReadFile(filepath.Join(e.dir, "logs", "agent.log"))
	if err != nil || !strings.Contains(string(b), "toprelay up") {
		t.Fatalf("agent.log: %v %s", err, b)
	}
}

func TestStatusWatch(t *testing.T) {
	dir := t.TempDir()
	var out syncBuffer
	a := &App{Stdout: &out, Stderr: &bytes.Buffer{}, Dir: dir}
	ctx, cancel := context.WithCancel(context.Background())
	a.Ctx = ctx
	done := make(chan int, 1)
	go func() { done <- Main([]string{"--json", "status", "--watch"}, a) }()
	lines := func() []string {
		return strings.FieldsFunc(out.String(), func(r rune) bool { return r == '\n' })
	}
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for len(lines()) < n && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if len(lines()) < n {
			t.Fatalf("want %d lines, have %v", n, lines())
		}
	}
	waitFor(1) // no agent yet: running false at once
	var st daemon.State
	if json.Unmarshal([]byte(lines()[0]), &st) != nil || st.Running || st.Version != daemon.StateVersion {
		t.Fatalf("first line %s", lines()[0])
	}
	sf := daemon.NewStateFile(daemon.StatePath(dir), nil)
	sf.Update("notes", func(s *daemon.ServiceState) { s.Connected = true })
	waitFor(2)
	json.Unmarshal([]byte(lines()[1]), &st)
	if !st.Running || !st.Services["notes"].Connected || st.PID != os.Getpid() {
		t.Fatalf("second line %s", lines()[1])
	}
	// A rewrite that changes nothing but updatedAt prints nothing.
	sf.Write()
	time.Sleep(600 * time.Millisecond)
	if len(lines()) != 2 {
		t.Fatalf("updatedAt-only change printed: %v", lines())
	}
	sf.Update("notes", func(s *daemon.ServiceState) { s.Requests = 3 })
	waitFor(3)
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// A state.json left by a crashed agent (running, but its pid is gone) reads as not running.
func TestStaleStateFile(t *testing.T) {
	dir := t.TempDir()
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(daemon.StatePath(dir), []byte(fmt.Sprintf(`{"pid":%d,"running":true,"services":{"notes":{"connected":true}}}`, gone.Process.Pid)), 0o600)
	st, err := daemon.ReadState(daemon.StatePath(dir))
	if err != nil || st.Running || st.Services["notes"].Connected || st.Version != 1 {
		t.Fatalf("%+v %v", st, err)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
