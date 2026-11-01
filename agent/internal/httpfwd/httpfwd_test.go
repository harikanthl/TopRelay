package httpfwd

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAllowed(t *testing.T) {
	pre := []string{"/api", "/static/"}
	for p, want := range map[string]bool{"/api": true, "/api/x": true, "/apix": false, "/static/a.css": true, "/": false, "/other": false} {
		if Allowed(p, pre) != want {
			t.Errorf("%s: want %v", p, want)
		}
	}
	if !Allowed("/anything", []string{"/"}) {
		t.Error("root prefix")
	}
	for p, want := range map[string]string{"/a//b/": "/a/b/", "/": "/", "": "/", "/a/b": "/a/b"} {
		if got, ok := CleanPath(p); !ok || got != want {
			t.Errorf("clean %q = %q %v", p, got, ok)
		}
	}
	for _, p := range []string{"/api/../secret", "/./x", "x"} {
		if _, ok := CleanPath(p); ok {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestForward(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s?%s token=%s xff=%s", r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Key"), r.Header.Get("X-Forwarded-For"))
	}))
	defer up.Close()
	base, _ := url.Parse(up.URL)
	f := New(base, []string{"/api"}, "X-Key", "k123")
	r := httptest.NewRequest("GET", "/api/items?x=1", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	w := httptest.NewRecorder()
	f.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "GET /api/items?x=1 token=k123 xff=203.0.113.7" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for _, p := range []string{"/secret", "/api/../secret"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		r.URL.Path = p
		f.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	up.Close()
	w = httptest.NewRecorder()
	f.ServeHTTP(w, httptest.NewRequest("GET", "/api", nil))
	b, _ := io.ReadAll(w.Body)
	if w.Code != 502 || !strings.Contains(string(b), "isn't answering") {
		t.Fatalf("down: %d %s", w.Code, b)
	}
}
