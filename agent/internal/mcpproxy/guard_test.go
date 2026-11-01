package mcpproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// smuggled are bodies that Go's encoding/json would read differently from an exact-key upstream (JS, Python,
// go-sdk), or that the guard can't parse at all.
var smuggled = map[string]string{
	"case-variant name":   `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_all","Name":"search"}}`,
	"case-variant method": `{"jsonrpc":"2.0","id":1,"method":"tools/call","Method":"ping","params":{"name":"delete_all"}}`,
	"case-variant id":     `{"jsonrpc":"2.0","id":1,"ID":2,"method":"tools/list"}`,
	"case-variant params": `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"},"PARAMS":{"name":"delete_all"}}`,
	"long-s params":       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_all"},"paramſ":{"name":"search"}}`,
	"duplicate name":      `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","name":"delete_all"}}`,
	"duplicate method":    `{"jsonrpc":"2.0","id":1,"method":"ping","method":"tools/call","params":{"name":"delete_all"}}`,
	"duplicate argument":  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"q":"a","q":"b"}}}`,
	"batch case variant":  `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_all","NAME":"search"}}]`,
	"byte order mark":     "\xef\xbb\xbf" + `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_all"}}`,
	"trailing data":       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_all"}} x`,
	"not json":            `not json`,
	"empty":               ``,
}

func TestDecideRejectsSmuggledBodies(t *testing.T) {
	allowed := func(n string) bool { return n == "search" }
	for name, body := range smuggled {
		d := Decide([]byte(body), "", "notes", allowed)
		if !d.Refuse || !strings.Contains(string(d.Body), "-32700") {
			t.Errorf("%s: refuse=%v body=%s, want a -32700 parse error", name, d.Refuse, d.Body)
		}
	}
	// Exact keys still work, and other fields' case is irrelevant.
	if d := Decide([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"Name":"x","name":"y"}}}`), "", "notes", allowed); d.Refuse {
		t.Fatalf("plain allowed call refused: %s", d.Body)
	}
}

func TestProxyDoesNotForwardSmuggledBodies(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL + "/mcp")
	p := &Proxy{Service: "notes", Target: u, Allowed: func(n string) bool { return n == "search" }}
	for name, body := range smuggled {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "-32700") {
			t.Errorf("%s: status %d body %s, want 400 with -32700", name, rec.Code, rec.Body.String())
		}
		if rec.Header().Get(RefusedHeader) == "" {
			t.Errorf("%s: not marked refused for the call log", name)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d smuggled bodies reached the upstream", n)
	}
}

func TestDecideRefusesCallWithoutName(t *testing.T) {
	allowAll := func(string) bool { return true }
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":5}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`,
		`[{"jsonrpc":"2.0","id":1,"method":"tools/call"}]`,
	} {
		d := Decide([]byte(body), "", "notes", allowAll)
		if !d.Refuse || len(d.Tools) != 1 || d.Tools[0] != "(missing name)" {
			t.Errorf("%s: refuse=%v tools=%q", body, d.Refuse, d.Tools)
		}
	}
}

func TestProxyRefusesDotSegments(t *testing.T) {
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		io.WriteString(w, "ok")
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL + "/mcp")
	p := &Proxy{Service: "notes", Target: u, Allowed: func(string) bool { return true }}
	for _, target := range []string{"/mcp/..%2Fadmin", "/mcp/%2e%2e/admin", "/mcp/./x", "/mcp/a/..%2F..%2Fadmin"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", target, rec.Code)
		}
	}
	if len(paths) != 0 {
		t.Fatalf("dot segments reached the upstream: %v", paths)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/mcp/sub//x", nil))
	if rec.Code != 200 || len(paths) != 1 || paths[0] != "/mcp/sub/x" {
		t.Fatalf("subpath: %d %v", rec.Code, paths)
	}
}
