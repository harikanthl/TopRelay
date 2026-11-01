// Package httpfwd forwards raw HTTP requests to a local base URL, with a path prefix allow-list.
package httpfwd

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
)

// Forwarder is the http-kind handler.
type Forwarder struct {
	Base       *url.URL
	AllowPaths []string
	// Upstream header (from the keyring), added when Name is set.
	HeaderName, HeaderValue string
	Transport               http.RoundTripper
	proxy                   *httputil.ReverseProxy
}

// New builds a forwarder.
func New(base *url.URL, allow []string, headerName, headerValue string) *Forwarder {
	f := &Forwarder{Base: base, AllowPaths: allow, HeaderName: headerName, HeaderValue: headerValue}
	f.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(f.Base)
			pr.Out.Host = f.Base.Host
			// Rewrite drops X-Forwarded-*; the relay's values (from the frame) are the real ones, so keep them.
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v := pr.In.Header.Values(h); len(v) > 0 {
					pr.Out.Header[h] = v
				}
			}
			if f.HeaderName != "" {
				pr.Out.Header.Set(f.HeaderName, f.HeaderValue)
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			// No URL here: the body goes to the remote caller, and the base URL may carry credentials.
			http.Error(w, "toprelay: the local service isn't answering. Ask its owner to check that it is running.", http.StatusBadGateway)
		},
	}
	return f
}

// CleanPath normalises a request path, keeping a trailing slash; ok is false for paths with dot segments.
func CleanPath(p string) (string, bool) {
	if p == "" {
		p = "/"
	}
	if p[0] != '/' {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return "", false
		}
	}
	c := path.Clean(p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c, true
}

// Allowed reports whether a clean path falls under one of the prefixes, on a segment boundary.
func Allowed(p string, prefixes []string) bool {
	for _, pre := range prefixes {
		if pre == "" || pre == "/" {
			return true
		}
		pre = "/" + strings.Trim(pre, "/")
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := CleanPath(r.URL.Path)
	if !ok || !Allowed(p, f.AllowPaths) {
		http.NotFound(w, r)
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path, r2.URL.RawPath = p, ""
	if f.Transport != nil {
		f.proxy.Transport = f.Transport
	}
	f.proxy.ServeHTTP(w, r2)
}
