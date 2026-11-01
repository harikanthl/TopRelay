package mcpproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/harikanthl/toprelay/agent/internal/reqctx"
)

func allow(names ...string) Allowed { return func(t string) bool { return slices.Contains(names, t) } }

const listResult = `{"jsonrpc":"2.0","id":7,"result":{"tools":[{"name":"search","inputSchema":{"type":"object"}},{"name":"delete","inputSchema":{"type":"object"}},{"name":"read","annotations":{"readOnlyHint":true}}],"ttlMs":60000,"cacheScope":"private","nextCursor":"c2"}}`

func TestFilterBodyKeepsOrderAndFields(t *testing.T) {
	out, seen := FilterBody([]byte(listResult), map[string]bool{"7": true}, allow("read", "search"))
	var m struct {
		ID     int `json:"id"`
		Result struct {
			Tools      []map[string]any `json:"tools"`
			TTLMs      int              `json:"ttlMs"`
			CacheScope string           `json:"cacheScope"`
			NextCursor string           `json:"nextCursor"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Result.Tools) != 2 || m.Result.Tools[0]["name"] != "search" || m.Result.Tools[1]["name"] != "read" {
		t.Fatalf("tools %v", m.Result.Tools)
	}
	if m.Result.TTLMs != 60000 || m.Result.CacheScope != "private" || m.Result.NextCursor != "c2" || m.ID != 7 {
		t.Fatalf("fields lost: %s", out)
	}
	if strings.Join(seen, ",") != "search,delete,read" {
		t.Fatalf("seen %v", seen)
	}
	// A response to another id passes through untouched.
	out, _ = FilterBody([]byte(listResult), map[string]bool{"8": true}, allow())
	if string(out) != listResult {
		t.Fatal("unrelated response modified")
	}
	// String ids match too, in a batch.
	batch := `[{"jsonrpc":"2.0","id":"a","result":{"tools":[{"name":"x"},{"name":"y"}]}},{"jsonrpc":"2.0","id":2,"result":{}}]`
	out, _ = FilterBody([]byte(batch), map[string]bool{`"a"`: true}, allow("y"))
	if !strings.Contains(string(out), `"tools":[{"name":"y"}]`) || !strings.Contains(string(out), `"id":2`) {
		t.Fatalf("batch %s", out)
	}
}

func TestFilterSSE(t *testing.T) {
	in := "event: message\nid: e1\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
		": comment\n\n" +
		"id: e2\r\ndata: " + listResult + "\r\n\r\n" +
		"data: {\"jsonrpc\":\"2.0\",\n" + "data: \"id\":9,\"result\":{\"tools\":[{\"name\":\"delete\"}]}}\n\n" +
		"data: partial"
	var out bytes.Buffer
	var seen []string
	err := FilterSSE(&out, strings.NewReader(in), map[string]bool{"7": true, "9": true}, allow("search"), func(s []string) { seen = append(seen, s...) })
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "event: message\nid: e1\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n: comment\n\n") {
		t.Fatalf("passthrough changed:\n%s", got)
	}
	if !strings.Contains(got, "id: e2\r\ndata: {") || strings.Contains(got, `"name":"delete"`) || !strings.Contains(got, `"name":"search"`) {
		t.Fatalf("filtered event:\n%s", got)
	}
	if !strings.Contains(got, `"ttlMs":60000`) || !strings.HasSuffix(got, "data: partial") {
		t.Fatalf("fields or tail lost:\n%s", got)
	}
	if !strings.Contains(got, `"result":{"tools":[]}`) {
		t.Fatalf("multi-line data event not filtered:\n%s", got)
	}
	if len(seen) != 4 {
		t.Fatalf("seen %v", seen)
	}
}

func TestDecide(t *testing.T) {
	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"delete","arguments":{}}}`
	d := Decide([]byte(call), "", "notes", allow("search"))
	if !d.Refuse {
		t.Fatal("not refused")
	}
	var r struct {
		ID     int `json:"id"`
		Result struct {
			Content []struct{ Type, Text string } `json:"content"`
			IsError bool                          `json:"isError"`
		} `json:"result"`
	}
	json.Unmarshal(d.Body, &r)
	want := `The tool "delete" is not available on notes. Its owner hasn't allowed it.`
	if r.ID != 3 || !r.Result.IsError || r.Result.Content[0].Text != want || r.Result.Content[0].Type != "text" {
		t.Fatalf("refusal %s", d.Body)
	}
	// Allowed call passes.
	if d := Decide([]byte(strings.Replace(call, "delete", "search", 1)), "", "notes", allow("search")); d.Refuse {
		t.Fatal("allowed call refused")
	}
	// Header names a disallowed tool even though the body is unparseable: refused, id null.
	d = Decide([]byte("not json"), "delete", "notes", allow("search"))
	if !d.Refuse || !strings.Contains(string(d.Body), `"id":null`) {
		t.Fatalf("header refusal %s", d.Body)
	}
	// Header and body disagree, header disallowed: refused.
	if d := Decide([]byte(strings.Replace(call, "delete", "search", 1)), "delete", "notes", allow("search")); !d.Refuse {
		t.Fatal("header mismatch accepted")
	}
	// tools/list ids are recorded.
	d = Decide([]byte(`{"jsonrpc":"2.0","id":"x","method":"tools/list"}`), "", "notes", allow())
	if d.Refuse || !d.ListIDs[`"x"`] {
		t.Fatalf("list ids %v", d.ListIDs)
	}
	// Batch with one bad call: answered whole, locally.
	batch := `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"delete"}},{"jsonrpc":"2.0","method":"notifications/x"}]`
	d = Decide([]byte(batch), "", "notes", allow("search"))
	var arr []map[string]any
	json.Unmarshal(d.Body, &arr)
	if !d.Refuse || len(arr) != 2 || arr[0]["error"] == nil || arr[1]["result"] == nil {
		t.Fatalf("batch %s", d.Body)
	}
}

func TestRestrict(t *testing.T) {
	a := Restrict(allow("a", "b"), []string{"b", "c"}, true)
	if a("a") || !a("b") || a("c") {
		t.Fatal("restriction must intersect")
	}
	if !Restrict(allow("a"), nil, false)("a") {
		t.Fatal("unrestricted")
	}
}

// fakeUpstream is a minimal MCP-ish server recording calls.
type fakeUpstream struct {
	calls   atomic.Int32
	mu      sync.Mutex
	headers []http.Header
	sse     bool
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	f.mu.Lock()
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var m Message
	json.Unmarshal(body, &m)
	w.Header().Set("Mcp-Session-Id", "sess-1")
	var reply string
	switch m.Method {
	case "tools/list":
		reply = strings.Replace(listResult, `"id":7`, `"id":`+string(m.ID), 1)
	default:
		reply = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ran %s"}]}}`, m.ID, m.ToolName())
	}
	if f.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "id: 1\ndata: %s\n\n", reply)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, reply)
}

func newProxy(t *testing.T, up http.Handler) *Proxy {
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL + "/mcp")
	return &Proxy{Service: "notes", Target: u, Allowed: allow("search", "read"), HeaderName: "X-App-Token", HeaderValue: "app-secret"}
}

func post(p http.Handler, ctx context.Context, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, "POST", "/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}

func TestProxyJSONAndSSE(t *testing.T) {
	for _, sse := range []bool{false, true} {
		up := &fakeUpstream{sse: sse}
		p := newProxy(t, up)
		var seen []string
		p.OnTools = func(s []string) { seen = s }
		w := post(p, context.Background(), `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`, map[string]string{"Mcp-Session-Id": "sess-1", "Mcp-Protocol-Version": "2025-11-25"})
		if w.Code != 200 || strings.Contains(w.Body.String(), "delete") || !strings.Contains(w.Body.String(), "search") || !strings.Contains(w.Body.String(), `"id":5`) {
			t.Fatalf("sse=%v list: %d %s", sse, w.Code, w.Body)
		}
		if w.Header().Get("Mcp-Session-Id") != "sess-1" || len(seen) != 3 {
			t.Fatalf("session header / seen: %v %v", w.Header(), seen)
		}
		h := up.headers[0]
		if h.Get("Mcp-Session-Id") != "sess-1" || h.Get("Mcp-Protocol-Version") != "2025-11-25" || h.Get("X-App-Token") != "app-secret" {
			t.Fatalf("upstream headers %v", h)
		}
		// Allowed call reaches the app.
		w = post(p, context.Background(), `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"search"}}`, nil)
		if !strings.Contains(w.Body.String(), "ran search") {
			t.Fatalf("call: %s", w.Body)
		}
		// Disallowed call is answered locally.
		before := up.calls.Load()
		w = post(p, context.Background(), `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"delete"}}`, nil)
		if up.calls.Load() != before || !strings.Contains(w.Body.String(), `not available on notes`) || w.Header().Get(RefusedHeader) != "delete" {
			t.Fatalf("refusal reached the app or wrong: %s", w.Body)
		}
		// Tools outside the frame's restriction are refused too.
		ctx := reqctx.WithTools(context.Background(), []string{"read"})
		w = post(p, ctx, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"search"}}`, nil)
		if up.calls.Load() != before || !strings.Contains(w.Body.String(), `"search\" is not available`) {
			t.Fatalf("restriction: %s", w.Body)
		}
	}
}

func TestProxyUpstreamDown(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1/mcp")
	p := &Proxy{Service: "notes", Target: u, Allowed: allow("search")}
	w := post(p, context.Background(), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("call while down: %d %s", w.Code, w.Body)
	}
	w = post(p, context.Background(), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "-32000") {
		t.Fatalf("list while down: %d %s", w.Code, w.Body)
	}
	r := httptest.NewRequest("GET", "/other", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	if rec.Code != 404 {
		t.Fatal("non-mcp path")
	}
}

func TestInProcAndListTools(t *testing.T) {
	// A real go-sdk server behind HandlerTransport, reached with ListTools semantics through the proxy.
	s := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	for _, n := range []string{"search", "delete"} {
		s.AddTool(&mcp.Tool{Name: n, InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ran " + r.Params.Name}}}, nil
		})
	}
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true, DisableLocalhostProtection: true})
	u, _ := url.Parse("http://inproc/mcp")
	p := &Proxy{Service: "notes", Target: u, Transport: HandlerTransport{Handler: sdk}, Allowed: allow("search")}
	front := httptest.NewServer(p)
	defer front.Close()
	tools, err := ListTools(context.Background(), front.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ToolNames(tools), ",") != "search" {
		t.Fatalf("tools %v", ToolNames(tools))
	}
	cs, err := Connect(context.Background(), front.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "delete"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "not available on notes") {
		t.Fatalf("refusal via SDK client: %+v", res)
	}
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "search"})
	if err != nil || res.Content[0].(*mcp.TextContent).Text != "ran search" {
		t.Fatalf("allowed call: %+v %v", res, err)
	}
	if cs.InitializeResult().ProtocolVersion != "2026-07-28" {
		t.Fatalf("negotiated %s, want 2026-07-28 (stateless)", cs.InitializeResult().ProtocolVersion)
	}
}
