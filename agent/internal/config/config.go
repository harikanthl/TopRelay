// Package config loads and saves the agent's JSON configuration. Secrets never live here; see package secrets.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Service kinds and source types.
const (
	KindMCP  = "mcp"
	KindHTTP = "http"

	SourceMCPURL = "mcp-url"
	SourceStdio  = "stdio"
	SourceHTTP   = "http"

	DefaultAllow = "allow"
	DefaultDeny  = "deny"
)

// Config is the content of config.json.
type Config struct {
	AccountID     string    `json:"accountId,omitempty"`
	AccountName   string    `json:"accountName,omitempty"`
	Subdomain     string    `json:"subdomain,omitempty"`
	PublicURL     string    `json:"publicUrl,omitempty"`
	KVNamespaceID string    `json:"kvNamespaceId,omitempty"`
	Services      []Service `json:"services"`
}

// Service is one exposed local app.
type Service struct {
	Name           string     `json:"name"`
	Kind           string     `json:"kind"`
	Source         Source     `json:"source"`
	Gen            int        `json:"gen"`
	Tools          ToolPolicy `json:"tools"`
	AllowPaths     []string   `json:"allowPaths,omitempty"`
	UpstreamHeader string     `json:"upstreamHeader,omitempty"`
}

// Source says where the local app is.
type Source struct {
	Type    string            `json:"type"`
	URL     string            `json:"url,omitempty"`
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
}

// ToolPolicy decides which MCP tools are exposed: explicit allow and deny sets, plus a default for tools in
// neither set (tools that appear after `add`).
type ToolPolicy struct {
	Allow   []string `json:"allow"`
	Deny    []string `json:"deny"`
	Default string   `json:"default"`
}

// Allowed reports whether a tool is exposed.
func (p ToolPolicy) Allowed(tool string) bool {
	if slices.Contains(p.Deny, tool) {
		return false
	}
	if slices.Contains(p.Allow, tool) {
		return true
	}
	return p.Default == DefaultAllow
}

// AllowsAll reports whether every tool, present and future, is exposed (hello `tools: null`).
func (p ToolPolicy) AllowsAll() bool {
	return p.Default == DefaultAllow && len(p.Deny) == 0
}

// AllowList returns the allowed names among the known tools, in their order.
func (p ToolPolicy) AllowList(known []string) []string {
	out := []string{}
	for _, t := range known {
		if p.Allowed(t) && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// SetAllowed moves tools into the allow set (out of deny).
func (p *ToolPolicy) SetAllowed(tools ...string) {
	for _, t := range tools {
		p.Deny = slices.DeleteFunc(p.Deny, func(s string) bool { return s == t })
		if !slices.Contains(p.Allow, t) {
			p.Allow = append(p.Allow, t)
		}
	}
}

// SetDenied moves tools into the deny set (out of allow).
func (p *ToolPolicy) SetDenied(tools ...string) {
	for _, t := range tools {
		p.Allow = slices.DeleteFunc(p.Allow, func(s string) bool { return s == t })
		if !slices.Contains(p.Deny, t) {
			p.Deny = append(p.Deny, t)
		}
	}
}

// Normalize fills nil slices and the default so the JSON is stable.
func (p *ToolPolicy) Normalize() {
	if p.Allow == nil {
		p.Allow = []string{}
	}
	if p.Deny == nil {
		p.Deny = []string{}
	}
	if p.Default != DefaultAllow {
		p.Default = DefaultDeny
	}
}

// Dir returns the configuration directory from the environment.
func Dir(getenv func(string) string) (string, error) {
	if d := getenv("TOPRELAY_CONFIG_DIR"); d != "" {
		return d, nil
	}
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "toprelay"), nil
	}
	home := getenv("HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("can't find the home directory; set TOPRELAY_CONFIG_DIR: %w", err)
		}
		home = h
	}
	return filepath.Join(home, ".config", "toprelay"), nil
}

// Path returns the config.json path in dir.
func Path(dir string) string { return filepath.Join(dir, "config.json") }

// ErrNotFound means config.json doesn't exist yet.
var ErrNotFound = errors.New("no TopRelay configuration yet: run `toprelay login`, then `toprelay add`")

// Load reads config.json from dir.
func Load(dir string) (*Config, error) {
	b, err := os.ReadFile(Path(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config %s is not valid JSON (fix or delete it): %w", Path(dir), err)
	}
	for i := range c.Services {
		c.Services[i].Tools.Normalize()
	}
	return &c, nil
}

// LoadOrEmpty reads config.json, returning an empty config when it doesn't exist.
func LoadOrEmpty(dir string) (*Config, error) {
	c, err := Load(dir)
	if errors.Is(err, ErrNotFound) {
		return &Config{}, nil
	}
	return c, err
}

// Save writes config.json atomically with mode 0600.
func Save(dir string, c *Config) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if c.Services == nil {
		c.Services = []Service{}
	}
	for i := range c.Services {
		c.Services[i].Tools.Normalize()
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return os.Rename(name, Path(dir))
}

// Find returns the service with name, or nil.
func (c *Config) Find(name string) *Service {
	for i := range c.Services {
		if c.Services[i].Name == name {
			return &c.Services[i]
		}
	}
	return nil
}

// MustFind returns the service or an actionable error.
func (c *Config) MustFind(name string) (*Service, error) {
	if s := c.Find(name); s != nil {
		return s, nil
	}
	var have []string
	for _, s := range c.Services {
		have = append(have, s.Name)
	}
	if len(have) == 0 {
		return nil, fmt.Errorf("no service named %q (no services yet: add one with `toprelay add`)", name)
	}
	return nil, fmt.Errorf("no service named %q (have: %s)", name, strings.Join(have, ", "))
}

// Remove deletes a service by name and reports whether it existed.
func (c *Config) Remove(name string) bool {
	n := len(c.Services)
	c.Services = slices.DeleteFunc(c.Services, func(s Service) bool { return s.Name == name })
	return len(c.Services) != n
}

// LocalTarget describes the service's local endpoint for display.
func (s Service) LocalTarget() string {
	switch s.Source.Type {
	case SourceStdio:
		return "stdio: " + strings.Join(s.Source.Command, " ")
	default:
		return s.Source.URL
	}
}

// PublicPath is the path under PUBLIC_URL that bots use.
func (s Service) PublicPath() string {
	if s.Kind == KindMCP {
		return "/" + s.Name + "/mcp"
	}
	return "/" + s.Name + "/"
}
