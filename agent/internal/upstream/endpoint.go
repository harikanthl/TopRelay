// Package upstream is how the agent reaches a local app: the endpoint (TCP URL or Unix socket), the checks that
// make sure it is the right app (socket owner and permissions, peer credentials, a macOS code-signature pin), the
// app's credential header (from the keyring or a file read on every request) and the X-TopRelay-Caller signature.
package upstream

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

// Endpoint is a parsed local target.
type Endpoint struct {
	// Raw is the configured URL.
	Raw string
	// URL is where HTTP requests go. For a Unix socket it is http://localhost<path>.
	URL *url.URL
	// Socket is the Unix socket path, or "" for TCP.
	Socket string
}

// ParseEndpoint parses an http(s) URL or unix:///abs/path.sock[?path=/http/path]. defaultPath is the HTTP path
// for a socket URL without ?path (e.g. "/mcp").
func ParseEndpoint(raw, defaultPath string) (*Endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			return nil, fmt.Errorf("%q has no host", raw)
		}
		if u.User != nil {
			return nil, fmt.Errorf("%q has credentials in the URL; use --header-file or --header-stdin instead", raw)
		}
		return &Endpoint{Raw: raw, URL: u}, nil
	case "unix":
		if u.Host != "" {
			return nil, fmt.Errorf("%q: a socket URL is unix:///absolute/path.sock (three slashes)", raw)
		}
		if u.User != nil || u.Fragment != "" {
			return nil, fmt.Errorf("%q: unexpected parts in a socket URL", raw)
		}
		sock := u.Path
		if !filepath.IsAbs(sock) || filepath.Clean(sock) != sock {
			return nil, fmt.Errorf("%q: the socket path must be absolute and clean", raw)
		}
		q := u.Query()
		for k := range q {
			if k != "path" {
				return nil, fmt.Errorf("%q: the only query parameter of a socket URL is ?path=/http/path", raw)
			}
		}
		p := defaultPath
		if v := q.Get("path"); v != "" {
			p = v
		}
		if !strings.HasPrefix(p, "/") {
			return nil, fmt.Errorf("%q: ?path must start with /", raw)
		}
		pu, err := url.Parse(p)
		if err != nil || pu.Host != "" || pu.Scheme != "" {
			return nil, fmt.Errorf("%q: bad ?path", raw)
		}
		// Host "localhost": apps that block DNS rebinding accept it, and nothing resolves it here.
		return &Endpoint{Raw: raw, Socket: sock, URL: &url.URL{Scheme: "http", Host: "localhost", Path: pu.Path, RawQuery: pu.RawQuery}}, nil
	default:
		return nil, fmt.Errorf("%q: want http://, https:// or unix://", raw)
	}
}

// IsLoopback reports whether a TCP endpoint's host is 127.0.0.0/8, ::1 or localhost.
func (e *Endpoint) IsLoopback() bool {
	if e.Socket != "" {
		return false
	}
	h := e.URL.Hostname()
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Port is the TCP port (with the scheme's default).
func (e *Endpoint) Port() (string, error) {
	if e.Socket != "" {
		return "", errors.New("a socket has no port")
	}
	if p := e.URL.Port(); p != "" {
		return p, nil
	}
	if e.URL.Scheme == "https" {
		return "443", nil
	}
	return "80", nil
}

// Join returns the endpoint's URL with sub appended to its path (for http services: base + request path).
func (e *Endpoint) Join(sub string) *url.URL {
	u := *e.URL
	u.Path = strings.TrimRight(e.URL.Path, "/") + sub
	u.RawPath = ""
	return &u
}

// Display is the endpoint for humans and logs (no credentials ever appear in it; ParseEndpoint refuses them).
func (e *Endpoint) Display() string { return e.Raw }
