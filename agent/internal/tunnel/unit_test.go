package tunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWSURL(t *testing.T) {
	cases := map[string]string{
		"https://toprelay.hari.workers.dev":  "wss://toprelay.hari.workers.dev/notes/connect",
		"https://toprelay.hari.workers.dev/": "wss://toprelay.hari.workers.dev/notes/connect",
		"http://127.0.0.1:8787":              "ws://127.0.0.1:8787/notes/connect",
		"http://127.0.0.1:8787/base":         "ws://127.0.0.1:8787/base/notes/connect",
	}
	for in, want := range cases {
		got, err := WSURL(in, "notes")
		if err != nil || got != want {
			t.Errorf("%s → %s %v, want %s", in, got, err, want)
		}
	}
	if _, err := WSURL("ftp://x", "notes"); err == nil {
		t.Error("ftp accepted")
	}
}

func TestBackoffBounds(t *testing.T) {
	maxJitter := func(d time.Duration) time.Duration { return d }
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second}
	for n, w := range want {
		if got := Backoff(n, maxJitter); got != w {
			t.Errorf("attempt %d ceiling %v, want %v", n, got, w)
		}
	}
	if Backoff(1000, maxJitter) != 30*time.Second || Backoff(-1, maxJitter) != 500*time.Millisecond {
		t.Error("extreme attempts")
	}
	if Backoff(5, func(time.Duration) time.Duration { return 0 }) != 0 {
		t.Error("jitter floor")
	}
	// The default jitter stays within [0, ceiling].
	c := New(Options{})
	for n := 0; n < 200; n++ {
		d := Backoff(n%8, c.o.Jitter)
		if d < 0 || d > 30*time.Second || d > 500*time.Millisecond<<min(n%8, 10) {
			t.Fatalf("attempt %d delay %v out of bounds", n%8, d)
		}
	}
}

func TestWakeDetector(t *testing.T) {
	d := &WakeDetector{Interval: 5 * time.Second, Threshold: 15 * time.Second}
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	if d.Check(t0) {
		t.Fatal("first tick")
	}
	if d.Check(t0.Add(5 * time.Second)) {
		t.Fatal("normal tick")
	}
	if d.Check(t0.Add(25 * time.Second)) { // 20 s gap: 15 s over the interval, not more
		t.Fatal("exactly the threshold must not count")
	}
	if !d.Check(t0.Add(25*time.Second + 21*time.Second)) {
		t.Fatal("21 s gap over a 5 s ticker should be a wake")
	}
	if d.Check(t0.Add(51 * time.Second)) {
		t.Fatal("after a wake, normal ticks resume")
	}
}

// fakeTimers is a manual clock for the merge timer.
type fakeTimers struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool { s := !t.stopped; t.stopped = true; return s }

func (ft *fakeTimers) after(d time.Duration, f func()) stopper {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	t := &fakeTimer{d: d, f: f}
	ft.timers = append(ft.timers, t)
	return t
}

func (ft *fakeTimers) fire() int {
	ft.mu.Lock()
	ts := ft.timers
	ft.timers = nil
	ft.mu.Unlock()
	n := 0
	for _, t := range ts {
		if !t.stopped {
			t.stopped = true
			t.f()
			n++
		}
	}
	return n
}

func (ft *fakeTimers) armed() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	n := 0
	for _, t := range ft.timers {
		if !t.stopped {
			n++
		}
	}
	return n
}

type recorder struct {
	mu     sync.Mutex
	frames []any
}

func (r *recorder) send(_ context.Context, v any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, v)
	return nil
}

func (r *recorder) chunks() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]byte
	for _, f := range r.frames {
		if c, ok := f.(chunkFrame); ok {
			b, _ := base64.StdEncoding.DecodeString(c.Data)
			out = append(out, b)
		}
	}
	return out
}

func TestChunkMerging(t *testing.T) {
	ft := &fakeTimers{}
	rec := &recorder{}
	w := newResponseWriter(context.Background(), rec, "r1", ft.after)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Secret", "dropped")
	w.WriteHeader(200)
	// The head goes out at once for SSE, before any data.
	if len(rec.frames) != 1 {
		t.Fatalf("frames after head: %d", len(rec.frames))
	}
	head := rec.frames[0].(responseFrame)
	if !head.Stream || head.Headers["content-type"] != "text/event-stream" || head.Headers["x-secret"] != "" {
		t.Fatalf("head %+v", head)
	}
	// Small writes are merged until the 10 ms timer fires.
	w.Write([]byte("data: a\n\n"))
	w.Flush()
	w.Write([]byte("data: b\n\n"))
	if len(rec.chunks()) != 0 || ft.armed() != 1 {
		t.Fatalf("chunks %d armed %d", len(rec.chunks()), ft.armed())
	}
	if ft.timers[0].d != 10*time.Millisecond {
		t.Fatalf("merge delay %v", ft.timers[0].d)
	}
	ft.fire()
	if c := rec.chunks(); len(c) != 1 || string(c[0]) != "data: a\n\ndata: b\n\n" {
		t.Fatalf("merged chunk %q", c)
	}
	// 64 KiB flushes immediately, without the timer, and cancels the armed one.
	w.Write([]byte("x"))
	if ft.armed() != 1 {
		t.Fatal("timer not armed for the first unflushed byte")
	}
	w.Write(bytes.Repeat([]byte("y"), MergeBytes))
	if c := rec.chunks(); len(c) != 2 || len(c[1]) != MergeBytes+1 {
		t.Fatalf("64 KiB flush: %d chunks", len(c))
	}
	if ft.armed() != 0 {
		t.Fatal("timer still armed after a size flush")
	}
	// A big write is split into chunks of at most 1 MiB.
	w.Write(bytes.Repeat([]byte("z"), 2*MaxChunk+5))
	c := rec.chunks()
	if len(c) != 5 || len(c[2]) != MaxChunk || len(c[4]) != 5 {
		t.Fatalf("split: %d chunks", len(c))
	}
	w.Write([]byte("tail"))
	w.finish("")
	frames := rec.frames
	if end, ok := frames[len(frames)-1].(endFrame); !ok || end.Error != "" {
		t.Fatalf("last frame %+v", frames[len(frames)-1])
	}
	if c := rec.chunks(); string(c[len(c)-1]) != "tail" {
		t.Fatal("pending bytes not flushed at the end")
	}
}

func TestMergeTimingReal(t *testing.T) {
	rec := &recorder{}
	w := newResponseWriter(context.Background(), rec, "r1", nil)
	w.Header().Set("Content-Type", "text/event-stream")
	start := time.Now()
	w.Write([]byte("a"))
	deadline := time.Now().Add(time.Second)
	for len(rec.chunks()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	el := time.Since(start)
	if len(rec.chunks()) != 1 || el < 9*time.Millisecond || el > 500*time.Millisecond {
		t.Fatalf("chunk after %v (%d chunks)", el, len(rec.chunks()))
	}
	w.finish("")
}

func TestWholeAndSwitchToStream(t *testing.T) {
	rec := &recorder{}
	w := newResponseWriter(context.Background(), rec, "r1", (&fakeTimers{}).after)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
	w.finish("")
	if len(rec.frames) != 1 {
		t.Fatal(rec.frames)
	}
	f := rec.frames[0].(responseFrame)
	b, _ := base64.StdEncoding.DecodeString(f.Body)
	if f.Stream || f.Status != 200 || string(b) != `{"ok":true}` {
		t.Fatalf("whole %+v", f)
	}

	// No Content-Length and more than 1 MiB: switches to streaming.
	rec2 := &recorder{}
	w2 := newResponseWriter(context.Background(), rec2, "r2", (&fakeTimers{}).after)
	w2.Header().Set("Content-Type", "application/octet-stream")
	w2.Write(bytes.Repeat([]byte("a"), WholeMaxBytes))
	if len(rec2.frames) != 0 {
		t.Fatal("exactly 1 MiB must still be buffered")
	}
	w2.Write([]byte("b"))
	if h, ok := rec2.frames[0].(responseFrame); !ok || !h.Stream {
		t.Fatalf("no stream head: %+v", rec2.frames)
	}
	w2.finish("")
	total := 0
	for _, c := range rec2.chunks() {
		total += len(c)
	}
	if total != WholeMaxBytes+1 {
		t.Fatalf("streamed %d bytes", total)
	}

	// An empty 204 omits the body.
	rec3 := &recorder{}
	w3 := newResponseWriter(context.Background(), rec3, "r3", nil)
	w3.WriteHeader(http.StatusNoContent)
	w3.finish("")
	if f := rec3.frames[0].(responseFrame); f.Status != 204 || f.Body != "" {
		t.Fatalf("204 %+v", f)
	}

	// A panic before any output becomes a 502.
	rec4 := &recorder{}
	w4 := newResponseWriter(context.Background(), rec4, "r4", nil)
	w4.finish("internal error")
	if f := rec4.frames[0].(responseFrame); f.Status != 502 {
		t.Fatalf("failure %+v", f)
	}
}

func TestCancelledWriterSendsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recorder{}
	w := newResponseWriter(ctx, rec, "r1", nil)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Write([]byte("a"))
	n := len(rec.frames)
	cancel()
	if _, err := w.Write([]byte("b")); err == nil {
		t.Fatal("write after cancel succeeded")
	}
	w.finish("")
	if len(rec.frames) != n {
		t.Fatalf("frames sent after cancel: %d → %d", n, len(rec.frames))
	}
}

func TestHelloFrame(t *testing.T) {
	c := New(Options{Hello: func() Hello { return Hello{Kind: "mcp", Tools: nil} }})
	f := c.helloFrame()
	if f.Tools == nil || len(*f.Tools) != 0 {
		t.Fatal("empty allow-list must be [] not null")
	}
	c = New(Options{Hello: func() Hello { return Hello{Kind: "mcp", AllowAll: true} }})
	if c.helloFrame().Tools != nil {
		t.Fatal("allow-all must be null")
	}
	c = New(Options{Hello: func() Hello { return Hello{Kind: "http", Tools: []string{"x"}} }})
	if c.helloFrame().Tools != nil {
		t.Fatal("http kind must be null")
	}
	if !strings.HasPrefix(c.helloFrame().Agent, "toprelay/") {
		t.Fatal("agent")
	}
}
