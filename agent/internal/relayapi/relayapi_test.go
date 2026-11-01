package relayapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte("ok"))
			return
		}
		if r.Header.Get("Authorization") != "Bearer tra1.good" {
			http.Error(w, "nope", 401)
			return
		}
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		switch r.URL.Path {
		case "/notes/admin/status":
			w.Write([]byte(`{"service":"notes","connected":true,"requests":5}`))
		case "/notes/admin/pair":
			if in["code"] != "ABCDEFGHJK" || in["ttl"] != float64(600) {
				t.Errorf("pair body %v", in)
			}
			w.Write([]byte(`{"expires":1793500600}`))
		case "/notes/admin/revoke-key":
			w.Write([]byte(`{"revoked":3}`))
		case "/notes/admin/revoke-oauth":
			w.Write([]byte(`{"revoked":2}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL + "/")
	ctx := context.Background()
	st, err := c.Status(ctx, "notes", "tra1.good")
	if err != nil || !st.Connected || st.Requests != 5 {
		t.Fatal(st, err)
	}
	exp, err := c.Pair(ctx, "notes", "tra1.good", "ABCDEFGHJK", 10*time.Minute)
	if err != nil || exp.Unix() != 1793500600 {
		t.Fatal(exp, err)
	}
	if n, err := c.RevokeKey(ctx, "notes", "tra1.good", "0123456789abcdef"); err != nil || n != 3 {
		t.Fatal(n, err)
	}
	if n, err := c.RevokeOAuth(ctx, "notes", "tra1.good"); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if d, err := c.Health(ctx); err != nil || d.IsZero() {
		t.Fatal(d, err)
	}
	_, err = c.Status(ctx, "notes", "tra1.bad")
	if err == nil || !strings.Contains(err.Error(), "rejected the agent key") {
		t.Fatal(err)
	}
	_, err = c.Status(ctx, "other", "tra1.good")
	if err == nil || !strings.Contains(err.Error(), "doesn't know this service") {
		t.Fatal(err)
	}
	srv.Close()
	if _, err := c.Health(ctx); err == nil || !strings.Contains(err.Error(), "can't reach the relay") {
		t.Fatal(err)
	}
}
