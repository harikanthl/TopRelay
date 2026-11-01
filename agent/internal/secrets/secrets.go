// Package secrets stores credentials in the OS keyring (service "toprelay"), or in a 0600 JSON file when
// TOPRELAY_KEYRING=file.
package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/zalando/go-keyring"

	"github.com/harikanthl/toprelay/agent/internal/redact"
)

// KeyringService is the keyring service name for every TopRelay secret.
const KeyringService = "toprelay"

// Well-known secret names.
const (
	CloudflareToken = "cloudflare-token"
	AgentRoot       = "agent-root"
	BearerRoot      = "bearer-root"
)

// UpstreamHeader is the secret name for a service's upstream header value.
func UpstreamHeader(service string) string { return "upstream-header/" + service }

// StdioEnv is the secret name for one environment variable of a stdio service.
func StdioEnv(service, name string) string { return "stdio-env/" + service + "/" + name }

// LoadEnv reads a stdio service's environment variables from the store.
func LoadEnv(s Store, service string, names []string) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(names))
	for _, n := range names {
		v, err := Require(s, StdioEnv(service, n), "re-add the service with --env "+n+"=…")
		if err != nil {
			return nil, err
		}
		env[n] = v
	}
	return env, nil
}

// DeleteEnv removes a stdio service's environment variables from the store.
func DeleteEnv(s Store, service string, names []string) error {
	var errs []error
	for _, n := range names {
		errs = append(errs, s.Delete(StdioEnv(service, n)))
	}
	return errors.Join(errs...)
}

// ErrNotFound means the secret isn't stored.
var ErrNotFound = errors.New("secret not found")

// Store gets, sets and deletes secrets.
type Store interface {
	Get(name string) (string, error)
	Set(name, value string) error
	Delete(name string) error
}

// Open picks the store from TOPRELAY_KEYRING ("file" uses dir/secrets.json; anything else the OS keyring).
func Open(getenv func(string) string, dir string) Store {
	if getenv("TOPRELAY_KEYRING") == "file" {
		return &File{Path: filepath.Join(dir, "secrets.json")}
	}
	return Keyring{}
}

// Keyring is the OS keyring (macOS Keychain, libsecret, Windows Credential Manager).
type Keyring struct{}

func (Keyring) Get(name string) (string, error) {
	v, err := keyring.Get(KeyringService, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read %s from the OS keyring: %w (set TOPRELAY_KEYRING=file to use a file instead)", name, err)
	}
	redact.Add(v)
	return v, nil
}

func (Keyring) Set(name, value string) error {
	redact.Add(value)
	if err := keyring.Set(KeyringService, name, value); err != nil {
		return fmt.Errorf("store %s in the OS keyring: %w (set TOPRELAY_KEYRING=file to use a file instead)", name, err)
	}
	return nil
}

func (Keyring) Delete(name string) error {
	err := keyring.Delete(KeyringService, name)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("delete %s from the OS keyring: %w", name, err)
	}
	return nil
}

// File is a 0600 JSON file of name → value.
type File struct {
	Path string
	mu   sync.Mutex
}

func (f *File) read() (map[string]string, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read secrets file: %w", err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("secrets file %s is corrupt: %w", f.Path, err)
	}
	return m, nil
}

func (f *File) write(m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write secrets file: %w", err)
	}
	return os.Rename(tmp, f.Path)
}

func (f *File) Get(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return "", err
	}
	v, ok := m[name]
	if !ok {
		return "", ErrNotFound
	}
	redact.Add(v)
	return v, nil
}

func (f *File) Set(name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	redact.Add(value)
	m, err := f.read()
	if err != nil {
		return err
	}
	m[name] = value
	return f.write(m)
}

func (f *File) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return err
	}
	if _, ok := m[name]; !ok {
		return nil
	}
	delete(m, name)
	return f.write(m)
}

// Require reads a secret, turning ErrNotFound into an actionable error.
func Require(s Store, name, fix string) (string, error) {
	v, err := s.Get(name)
	if errors.Is(err, ErrNotFound) {
		return "", fmt.Errorf("%s is missing from the keyring: %s", name, fix)
	}
	return v, err
}
