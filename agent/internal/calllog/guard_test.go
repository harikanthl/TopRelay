package calllog

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
)

func TestMiddlewareRecordsNamelessCallAsRefused(t *testing.T) {
	l, err := Open(Path(t.TempDir(), "notes"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var got []Record
	l.OnRecord = func(r Record) { got = append(got, r) }
	target, _ := url.Parse("http://127.0.0.1:1/mcp") // never reached: the call is refused locally
	h := l.Middleware(&mcpproxy.Proxy{Service: "notes", Target: target, Allowed: func(string) bool { return false }})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`)))
	if len(got) != 1 || !got[0].Refused {
		t.Fatalf("records %+v, want one refused", got)
	}
}

func TestMiddlewareIgnoresCaseVariantToolName(t *testing.T) {
	l, err := Open(Path(t.TempDir(), "notes"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var got []Record
	l.OnRecord = func(r Record) { got = append(got, r) }
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_all","Name":"search"}}`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/mcp", strings.NewReader(body)))
	if len(got) != 1 || got[0].Tool == "search" {
		t.Fatalf("records %+v: logged the smuggled alias as the tool", got)
	}
}
