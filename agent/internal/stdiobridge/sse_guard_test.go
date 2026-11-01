package stdiobridge_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
	"github.com/harikanthl/toprelay/agent/internal/stdiobridge"
)

func TestSSEPostWithoutToolNameIsRefused(t *testing.T) {
	bin := buildTiny(t)
	b := stdiobridge.New(stdiobridge.Options{Service: "tools", Command: []string{bin}, Allowed: func(string) bool { return true }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)
	wctx, wcancel := context.WithTimeout(ctx, 20*time.Second)
	defer wcancel()
	if err := b.WaitReady(wctx); err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]int{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`:                                        http.StatusForbidden,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","Name":"wipe"}}`: http.StatusBadRequest,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/mcp/sse?sessionid=x", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		b.ServeHTTP(rec, req)
		if rec.Code != want || rec.Header().Get(mcpproxy.RefusedHeader) == "" {
			t.Errorf("%s: status %d refused=%q, want %d and marked refused", body, rec.Code, rec.Header().Get(mcpproxy.RefusedHeader), want)
		}
	}
}
