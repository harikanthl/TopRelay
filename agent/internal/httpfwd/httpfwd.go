// Package httpfwd forwards raw HTTP requests to a local base URL, with a path prefix allow-list.
package httpfwd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
)

// MaxBody is the largest request body forwarded (the relay's cap).
const MaxBody = 20 << 20

// callerHeader is X-TopRelay-Caller: only the agent may set it.
const callerHeader = "X-Toprelay-Caller"

// Forwarder is the http-kind handler.
type Forwarder struct {
	Base       *url.URL
	AllowPaths []string
	// Prepare, if set, readies each upstream request (credential, X-TopRelay-Caller); body is the exact body.
	Prepare func(out *http.Request, body []byte) error
	// Transport reaches the app; nil uses http.DefaultTransport.
	Transport http.RoundTripper
	Log       *slog.Logger
	proxy     *httputil.ReverseProxy
}

type bodyKey struct{}

var errPrepare = errors.New("prepare upstream request")

// prepareTransport runs Prepare on the outgoing request just before it is sent.
type prepareTransport struct{ f *Forwarder }

func (t prepareTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.f.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	r.Header.Del(callerHeader)
	if t.f.Prepare != nil {
		body, _ := r.Context().Value(bodyKey{}).([]byte)
		if err := t.f.Prepare(r, body); err != nil {
			if r.Body != nil {
				r.Body.Close()
			}
			if t.f.Log != nil {
				t.f.Log.Error("can't prepare the upstream request", "err", err)
			}
			return nil, errors.Join(errPrepare, err)
		}
	}
	return base.RoundTrip(r)
}

// New builds a forwarder.
func New(base *url.URL, allow []string, prepare func(*http.Request, []byte) error) *Forwarder {
	f := &Forwarder{Base: base, AllowPaths: allow, Prepare: prepare}
	f.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(f.Base)
			// The app sees its own host (e.g. 127.0.0.1:8080), never the relay's: apps that block DNS rebinding
			// require it. The relay's host is in X-Forwarded-Host.
			pr.Out.Host = f.Base.Host
			// Rewrite drops X-Forwarded-*; the relay's values (from the frame) are the real ones, so keep them.
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v := pr.In.Header.Values(h); len(v) > 0 {
					pr.Out.Header[h] = v
				}
			}
		},
		Transport:     prepareTransport{f},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			// No URL here: the body goes to the remote caller, and the base URL may carry credentials.
			msg := "toprelay: the local service isn't answering. Ask its owner to check that it is running."
			if errors.Is(err, errPrepare) {
				msg = "toprelay: TopRelay can't get the credential for the local service right now. Ask its owner to run `toprelay doctor`."
			}
			http.Error(w, msg, http.StatusBadGateway)
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
	// The body is needed whole for the signature. It is already in memory: the relay sends it in one frame.
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
		if err != nil {
			http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(b) > MaxBody {
			http.Error(w, "request body over 20 MiB", http.StatusRequestEntityTooLarge)
			return
		}
		body = b
	}
	r2 := r.Clone(context.WithValue(r.Context(), bodyKey{}, body))
	r2.URL.Path, r2.URL.RawPath = p, ""
	r2.Header.Del(callerHeader)
	r2.ContentLength = int64(len(body))
	if len(body) > 0 {
		r2.Body = io.NopCloser(bytes.NewReader(body))
	} else {
		r2.Body = http.NoBody
	}
	f.proxy.ServeHTTP(w, r2)
}
