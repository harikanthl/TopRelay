package cfapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTest(t *testing.T, h http.HandlerFunc) (*Client, *[]time.Duration) {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("tok-secret-123", srv.URL)
	var slept []time.Duration
	c.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	return c, &slept
}

func TestRetryOn429And5xx(t *testing.T) {
	var n atomic.Int32
	c, slept := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-secret-123" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		switch n.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
		case 2:
			w.WriteHeader(502)
		default:
			io.WriteString(w, `{"success":true,"errors":[],"result":[{"id":"a1","name":"Hari"}],"result_info":{"page":1,"total_pages":1}}`)
		}
	})
	accts, err := c.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 1 || accts[0].ID != "a1" {
		t.Fatalf("accounts %+v", accts)
	}
	if len(*slept) != 2 || (*slept)[0] != 7*time.Second {
		t.Fatalf("slept %v", *slept)
	}
}

func TestRetryGivesUp(t *testing.T) {
	c, slept := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		io.WriteString(w, `{"success":false,"errors":[{"code":1,"message":"down"}]}`)
	})
	_, err := c.VerifyToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err %v", err)
	}
	if len(*slept) != c.MaxRetries {
		t.Fatalf("retries %d", len(*slept))
	}
	if strings.Contains(err.Error(), "tok-secret-123") {
		t.Fatal("token leaked into error")
	}
}

func TestErrorsAndCodes(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"success":false,"errors":[{"code":10031,"message":"taken"}]}`)
	})
	_, err := c.PutSubdomain(context.Background(), "a", "x")
	if !IsCode(err, CodeSubdomainTaken) || IsCode(err, 1) {
		t.Fatalf("err %v", err)
	}
	c2, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		io.WriteString(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`)
	})
	_, err = c2.ListKVNamespaces(context.Background(), "a")
	if err == nil || !strings.Contains(err.Error(), "Workers KV Storage") {
		t.Fatalf("403 hint missing: %v", err)
	}
}

func TestRetryDelay(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	if d := RetryDelay(0, "3", now); d != 3*time.Second {
		t.Fatal(d)
	}
	if d := RetryDelay(0, now.Add(10*time.Second).Format(http.TimeFormat), now); d != 10*time.Second {
		t.Fatal(d)
	}
	for n := 0; n < 10; n++ {
		d := RetryDelay(n, "", now)
		base := time.Second << min(n, 5)
		if d < base/2 || d > base {
			t.Fatalf("attempt %d delay %v", n, d)
		}
	}
}

func TestPaginationAndScripts(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/storage/kv/namespaces"):
			if r.URL.Query().Get("page") == "1" {
				io.WriteString(w, `{"success":true,"result":[{"id":"1","title":"a"}],"result_info":{"page":1,"total_pages":2}}`)
			} else {
				io.WriteString(w, `{"success":true,"result":[{"id":"2","title":"toprelay-oauth"}],"result_info":{"page":2,"total_pages":2}}`)
			}
		case strings.HasSuffix(r.URL.Path, "/workers/scripts/toprelay"):
			w.WriteHeader(404)
			io.WriteString(w, `{"success":false,"errors":[{"code":10007,"message":"workers.api.error.script_not_found"}]}`)
		case strings.HasSuffix(r.URL.Path, "/workers/scripts/other"):
			io.WriteString(w, "export default {}")
		case strings.HasSuffix(r.URL.Path, "/workers/scripts"):
			io.WriteString(w, `{"success":true,"result":[{"id":"toprelay"}]}`)
		}
	})
	ns, err := c.ListKVNamespaces(context.Background(), "a")
	if err != nil || len(ns) != 2 || ns[1].Title != "toprelay-oauth" {
		t.Fatalf("%+v %v", ns, err)
	}
	ok, err := c.ScriptExists(context.Background(), "a", "toprelay")
	if err != nil || ok {
		t.Fatal(ok, err)
	}
	ok, err = c.ScriptExists(context.Background(), "a", "other")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	names, err := c.ListScripts(context.Background(), "a")
	if err != nil || len(names) != 1 {
		t.Fatal(names, err)
	}
}
