package calllog

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
)

const appReply = `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`

func TestAuditFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var got []Record
	l.OnRecord = func(r Record) { got = append(got, r) }
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, appReply)
	}))
	defer app.Close()
	u, _ := url.Parse(app.URL + "/mcp")
	h := l.Middleware(&mcpproxy.Proxy{Service: "notes", Target: u, Allowed: func(n string) bool { return n == "search" }})
	call := func(tool string) {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":{"secret":"do-not-log"}}}`
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Toprelay-Client", "https://claude.ai/x")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	call("search")
	call("delete")
	if len(got) != 2 {
		t.Fatalf("%d records", len(got))
	}
	ok, refused := got[0], got[1]
	if ok.Tool != "search" || ok.Client != "https://claude.ai/x" || ok.BytesIn == 0 || ok.BytesOut != int64(len(appReply)) || ok.Refused || ok.Reason != "" {
		t.Fatalf("ok %+v", ok)
	}
	if !refused.Refused || refused.Reason != mcpproxy.ReasonToolNotAllowed || refused.Tool != "delete" || refused.BytesOut == 0 {
		t.Fatalf("refused %+v", refused)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "do-not-log") {
		t.Fatal("a body reached the call log")
	}
}

func TestPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.jsonl")
	old, recent := time.Now().Add(-40*24*time.Hour).UTC(), time.Now().UTC()
	os.WriteFile(path+".1", []byte(`{"time":"`+old.Format(time.RFC3339Nano)+`","status":200}`+"\n"), 0o600)
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.Write(Record{Time: old, Status: 200})
	l.Write(Record{Time: recent, Status: 201})
	n, err := l.Prune(time.Now().Add(-30 * 24 * time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("pruned %d %v", n, err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatal("an all-old rotated file should be deleted")
	}
	// The writer still works after pruning, and appends after the kept line.
	l.Write(Record{Time: recent, Status: 202})
	lines, _ := Tail(path, 0)
	if len(lines) != 2 || !strings.Contains(lines[0], `"status":201`) || !strings.Contains(lines[1], `"status":202`) {
		t.Fatalf("%v", lines)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatal("mode after prune")
	}
}

// A prune while `logs -f` runs must not print the kept records again.
func TestFollowAcrossPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.jsonl")
	l, _ := Open(path)
	defer l.Close()
	old := time.Now().Add(-40 * 24 * time.Hour).UTC()
	l.Write(Record{Time: old, Status: 200})
	l.Write(Record{Time: time.Now().UTC(), Status: 201})
	var mu sync.Mutex
	var got []string
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Follow(ctx, path, 10*time.Millisecond, func(s string) { mu.Lock(); got = append(got, s); mu.Unlock() })
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	l.Write(Record{Time: time.Now().UTC(), Status: 202})
	time.Sleep(50 * time.Millisecond)
	l.Prune(time.Now().Add(-30 * 24 * time.Hour))
	time.Sleep(50 * time.Millisecond)
	l.Write(Record{Time: time.Now().UTC(), Status: 203})
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || !strings.Contains(got[0], `"status":202`) || !strings.Contains(got[1], `"status":203`) {
		t.Fatalf("followed %v", got)
	}
}
