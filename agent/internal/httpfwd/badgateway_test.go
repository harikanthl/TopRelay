package httpfwd

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBadGatewayHidesBaseURL(t *testing.T) {
	base, _ := url.Parse("http://user:pa55word@127.0.0.1:1/app?token=s3cret")
	f := New(base, []string{"/"}, nil)
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != 502 {
		t.Fatalf("status %d", rec.Code)
	}
	for _, leak := range []string{"s3cret", "pa55word", "127.0.0.1", "/app"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("502 body leaks %q: %s", leak, rec.Body.String())
		}
	}
}
