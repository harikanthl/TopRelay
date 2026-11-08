package service

import (
	"fmt"

	"github.com/harikanthl/toprelay/agent/internal/caller"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/upstream"
)

// DefaultHTTPPath is the HTTP path of a unix:// URL without ?path, per kind.
func DefaultHTTPPath(kind string) string {
	if kind == config.KindMCP {
		return "/mcp"
	}
	return "/"
}

// Endpoint parses the service's local URL.
func Endpoint(s config.Service) (*upstream.Endpoint, error) {
	ep, err := upstream.ParseEndpoint(s.Source.URL, DefaultHTTPPath(s.Kind))
	if err != nil {
		return nil, fmt.Errorf("service %s: %w", s.Name, err)
	}
	return ep, nil
}

// CallerKeyPath is the X-TopRelay-Caller key file of a service.
func CallerKeyPath(dir string, s config.Service) string {
	if s.CallerKeyFile != "" {
		return s.CallerKeyFile
	}
	return caller.DefaultKeyPath(dir, s.Name)
}

// Credential returns the service's credential: a file, or the keyring value. nil when it has none.
func Credential(s config.Service, store secrets.Store) (*upstream.Credential, error) {
	if s.UpstreamHeader == "" {
		return nil, nil
	}
	if s.UpstreamHeaderFile != "" {
		return &upstream.Credential{Name: s.UpstreamHeader, File: s.UpstreamHeaderFile}, nil
	}
	v, err := secrets.Require(store, secrets.UpstreamHeader(s.Name), "set it again with `toprelay set-header "+s.Name+" --stdin`")
	if err != nil {
		return nil, err
	}
	return &upstream.Credential{Name: s.UpstreamHeader, Value: v}, nil
}

// Pin converts the config pin.
func Pin(s config.Service) *upstream.Pin {
	if s.Pin == nil {
		return nil
	}
	return &upstream.Pin{TeamID: s.Pin.TeamID, BundleID: s.Pin.BundleID}
}

// NewUpstream builds how the agent reaches the service's app: endpoint checks, pin, credential, and the caller
// key (created at the default path if it doesn't exist yet; an app-supplied file must exist).
func NewUpstream(s config.Service, dir string, store secrets.Store) (*upstream.Upstream, error) {
	if !s.HasUpstream() {
		return nil, fmt.Errorf("service %s has no HTTP upstream", s.Name)
	}
	ep, err := Endpoint(s)
	if err != nil {
		return nil, err
	}
	cred, err := Credential(s, store)
	if err != nil {
		return nil, err
	}
	keyPath := CallerKeyPath(dir, s)
	if s.CallerKeyFile == "" {
		if _, err := caller.EnsureKeyFile(keyPath); err != nil {
			return nil, fmt.Errorf("service %s: create the caller key %s: %w", s.Name, keyPath, err)
		}
	}
	return upstream.New(upstream.Options{Endpoint: ep, Pin: Pin(s), Credential: cred, CallerKeyFile: keyPath})
}
