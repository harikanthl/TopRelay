package limits

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRate(t *testing.T) {
	for in, want := range map[string]Rate{"60/m": {60, time.Minute}, "10/s": {10, time.Second}, "5/h": {5, time.Hour}, "1/d": {1, 24 * time.Hour}} {
		r, err := ParseRate(in)
		if err != nil || r != want || r.String() != in {
			t.Errorf("%s: %+v %v", in, r, err)
		}
	}
	for _, bad := range []string{"", "60", "0/m", "-1/m", "x/m", "10/w", "10/"} {
		if _, err := ParseRate(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCheck(t *testing.T) {
	now := time.Unix(1793500000, 0)
	l, err := New(Config{Client: "3/m", Tools: map[string]string{"render": "2/m"}})
	if err != nil {
		t.Fatal(err)
	}
	l.Now = func() time.Time { return now }
	for i := range 2 {
		if ok, _, _ := l.Check("a", []string{"render"}); !ok {
			t.Fatalf("render %d refused within the burst", i)
		}
	}
	ok, wait, what := l.Check("b", []string{"render"})
	if ok || what != "the tool render" || wait <= 0 || wait > 30*time.Second {
		t.Fatalf("third render: %v %s %q", ok, wait, what)
	}
	// The refusal charged nothing: client b still has its full burst for other calls.
	for i := range 3 {
		if ok, _, _ := l.Check("b", nil); !ok {
			t.Fatalf("b call %d", i)
		}
	}
	if ok, _, what := l.Check("b", nil); ok || what != "this client" {
		t.Fatal("client limit")
	}
	if ok, _, _ := l.Check("a", nil); !ok {
		t.Fatal("a has its own client budget")
	}
	now = now.Add(30 * time.Second) // one render slot back (2/m = one per 30 s)
	if ok, _, _ := l.Check("c", []string{"render"}); !ok {
		t.Fatal("render after its interval")
	}
	l.Set(Config{})
	for range 100 {
		if ok, _, _ := l.Check("a", []string{"render"}); !ok {
			t.Fatal("no limits after clearing")
		}
	}
	if Validate(Config{Tools: map[string]string{"x": "fast"}}) == nil {
		t.Fatal("bad tool rate accepted")
	}
}

func TestMiddleware(t *testing.T) {
	l, _ := New(Config{Service: "1/m"})
	reached := 0
	h := l.Middleware("notes", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "tools/call") {
			t.Error("the body didn't survive the limiter")
		}
		reached++
	}))
	call := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"x"}}`)))
		return w
	}
	call()
	w := call()
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || m["id"] != float64(42) || reached != 1 {
		t.Fatalf("%d %v %s reached=%d", w.Code, w.Header(), w.Body, reached)
	}
	if !strings.Contains(m["error"].(map[string]any)["message"].(string), "try again in") {
		t.Fatal(m)
	}
}

func TestCheckConcurrentAndBatch(t *testing.T) {
	l, _ := New(Config{Tools: map[string]string{"x": "5/h"}})
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a, _, _ := l.Check("c", []string{"x"}); a {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 5 {
		t.Fatalf("%d admitted under a 5/h limit", ok.Load())
	}
	l2, _ := New(Config{Tools: map[string]string{"x": "1/h"}})
	if a, _, _ := l2.Check("c", []string{"x", "x"}); a {
		t.Fatal("a batch calling x twice passed a 1/h limit")
	}
	if a, _, _ := l2.Check("c", []string{"x"}); !a {
		t.Fatal("the refused batch charged the limit")
	}
}
