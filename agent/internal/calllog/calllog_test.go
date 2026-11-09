package calllog

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
)

func TestMiddleware(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(Path(dir, "notes"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var got []Record
	l.OnRecord = func(r Record) { got = append(got, r) }
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "secret-arg") {
			t.Error("body not restored for the handler")
		}
		w.Header().Set(mcpproxy.RefusedHeader, "wipe")
		w.WriteHeader(200)
	}))
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wipe","arguments":{"x":"secret-arg"}}}`))
	r.Header.Set("X-Toprelay-Client", "key:0123456789abcdef")
	r.Header.Set("Authorization", "Bearer should-not-log")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if len(got) != 1 {
		t.Fatal(got)
	}
	g := got[0]
	if g.Method != "POST" || g.Path != "/mcp" || g.MCPMethod != "tools/call" || g.Tool != "wipe" || g.Status != 200 || g.Client != "key:0123456789abcdef" || !g.Refused {
		t.Fatalf("%+v", g)
	}
	raw, _ := os.ReadFile(Path(dir, "notes"))
	if strings.Contains(string(raw), "secret-arg") || strings.Contains(string(raw), "should-not-log") {
		t.Fatal("body or credential logged")
	}
	var back Record
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &back); err != nil || back.Tool != "wipe" {
		t.Fatalf("line %s", raw)
	}
	st, _ := os.Stat(Path(dir, "notes"))
	if st.Mode().Perm() != 0o600 {
		t.Fatal("mode")
	}
}

func TestRotateAndTail(t *testing.T) {
	dir := t.TempDir()
	p := Path(dir, "s")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	l.maxSize = 1000
	for i := 0; i < 50; i++ {
		l.Write(Record{Method: "GET", Path: "/" + strings.Repeat("x", 20), Status: i})
	}
	l.Close()
	st, _ := os.Stat(p)
	old, err := os.Stat(p + ".1")
	if err != nil || st.Size() > 1000 || old.Size() > 1000 {
		t.Fatalf("rotation: %v %v %v", st.Size(), old, err)
	}
	lines, err := Tail(p, 5)
	if err != nil || len(lines) != 5 {
		t.Fatal(lines, err)
	}
	var last Record
	json.Unmarshal([]byte(lines[4]), &last)
	if last.Status != 49 {
		t.Fatalf("last %+v", last)
	}
}

func TestFollow(t *testing.T) {
	dir := t.TempDir()
	p := Path(dir, "s")
	l, _ := Open(p)
	l.maxSize = 600            // about two chained lines per file
	l.Write(Record{Status: 1}) // history: not followed
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var seen []string
	done := make(chan struct{})
	go func() {
		Follow(ctx, p, 5*time.Millisecond, func(s string) { mu.Lock(); seen = append(seen, s); mu.Unlock() })
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	for i := 2; i <= 6; i++ {
		l.Write(Record{Status: i})
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	l.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 4 || strings.Contains(seen[0], `"status":1,`) {
		t.Fatalf("followed %v", seen)
	}
}
