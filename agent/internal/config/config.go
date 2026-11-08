// Package config loads and saves the agent's JSON configuration. Secrets never live here; see package secrets.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/harikanthl/toprelay/agent/internal/secrets"
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
	// DefaultReadOnly exposes a tool in neither list only if its MCP annotations say it's read-only and not
	// destructive. Destructive tools (destructiveHint, which MCP defaults to true for non-read-only tools) need an
	// explicit allow.
	DefaultReadOnly = "readonly"
)

// Hint is what a tool's MCP annotations say about it.
type Hint struct {
	Known       bool // the tool has been listed, so the hints below are real
	ReadOnly    bool
	Destructive bool
}

// ClientRule changes the policy for one client (or a group of clients): Deny hides tools, Allow exposes them,
// both on top of the service's policy.
type ClientRule struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// Config is the content of config.json.
type Config struct {
	AccountID     string `json:"accountId,omitempty"`
	AccountName   string `json:"accountName,omitempty"`
	Subdomain     string `json:"subdomain,omitempty"`
	PublicURL     string `json:"publicUrl,omitempty"`
	KVNamespaceID string `json:"kvNamespaceId,omitempty"`
	// LogRetentionDays drops call-log records older than this (0: keep until the size cap rotates them out).
	LogRetentionDays int       `json:"logRetentionDays,omitempty"`
	Services         []Service `json:"services"`
}

// Service is one exposed local app.
type Service struct {
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Source     Source     `json:"source"`
	Gen        int        `json:"gen"`
	Tools      ToolPolicy `json:"tools"`
	AllowPaths []string   `json:"allowPaths,omitempty"`
	// UpstreamHeader is the name of the header that carries the app's credential. Its value is in the keyring
	// (secrets.UpstreamHeader), or, when UpstreamHeaderFile is set, read from that file on every request.
	UpstreamHeader     string `json:"upstreamHeader,omitempty"`
	UpstreamHeaderFile string `json:"upstreamHeaderFile,omitempty"`
	// UpstreamHeaderRev goes up on every `set-header`, so a running `up` reloads the value without a restart.
	UpstreamHeaderRev int `json:"upstreamHeaderRev,omitempty"`
	// CallerKeyFile is an app-supplied key file for X-TopRelay-Caller; empty uses
	// <config dir>/caller-keys/<name>.key.
	CallerKeyFile string `json:"callerKeyFile,omitempty"`
	// Pin requires the local app to be signed by a team (macOS).
	Pin *Pin `json:"pin,omitempty"`
	// Limits are the service's own rate limits, enforced in the agent.
	Limits *Limits `json:"limits,omitempty"`
	// FilesDir is the service's outbox: files the app writes there can be linked as toprelay-file:///<name>.
	FilesDir string `json:"filesDir,omitempty"`
}

// Limits are rate limits as "N/s", "N/m", "N/h" or "N/d": for the whole service, for each client, and per tool
// (all clients together). Empty strings mean no limit.
type Limits struct {
	Service string            `json:"service,omitempty"`
	Client  string            `json:"client,omitempty"`
	Tools   map[string]string `json:"tools,omitempty"`
}

// Empty reports whether no limit is set.
func (l Limits) Empty() bool { return l.Service == "" && l.Client == "" && len(l.Tools) == 0 }

// Pin is a code-signature requirement for the process serving the app.
type Pin struct {
	TeamID   string `json:"teamId"`
	BundleID string `json:"bundleId,omitempty"`
}

// Source says where the local app is.
type Source struct {
	Type    string   `json:"type"`
	URL     string   `json:"url,omitempty"`
	Command []string `json:"command,omitempty"`
	// EnvNames are the stdio child's extra environment variables; their values live in the keyring
	// (secrets.StdioEnv).
	EnvNames []string `json:"envNames,omitempty"`
	// Env holds values only in memory (read from the keyring for a run) or in a config written by an older
	// version, which MigrateEnv moves to the keyring. Save refuses to write it.
	Env map[string]string `json:"env,omitempty"`
	Cwd string            `json:"cwd,omitempty"`
}

// ToolPolicy decides which MCP tools are exposed: explicit allow and deny sets, plus a default for tools in
// neither set (tools that appear after `add`).
type ToolPolicy struct {
	Allow   []string `json:"allow"`
	Deny    []string `json:"deny"`
	Default string   `json:"default"`
	// Clients are per-client rules, keyed by selector: an exact client id (an OAuth client id, or "key:<id>" for
	// a bearer key), a prefix ending in "*", or a domain ("claude.ai") that matches OAuth client ids which are
	// URLs on that host or its subdomains.
	Clients map[string]ClientRule `json:"clients,omitempty"`
}

// Base reports whether the service policy exposes a tool, before client rules.
func (p ToolPolicy) Base(tool string, h Hint) bool {
	if slices.Contains(p.Deny, tool) {
		return false
	}
	if slices.Contains(p.Allow, tool) {
		return true
	}
	switch p.Default {
	case DefaultAllow:
		return true
	case DefaultReadOnly:
		return h.Known && h.ReadOnly && !h.Destructive
	}
	return false
}

// Allowed reports whether a tool is exposed, ignoring annotations and client rules (callers without hints).
func (p ToolPolicy) Allowed(tool string) bool { return p.Base(tool, Hint{}) }

// AllowedFor reports whether a tool is exposed to one client.
func (p ToolPolicy) AllowedFor(tool, client string, h Hint) bool {
	if r, ok := p.RuleFor(client); ok {
		if slices.Contains(r.Deny, tool) {
			return false
		}
		if slices.Contains(r.Allow, tool) {
			return true
		}
	}
	return p.Base(tool, h)
}

// AllowedAny reports whether some client may use a tool: the set the relay is told about.
func (p ToolPolicy) AllowedAny(tool string, h Hint) bool {
	if p.Base(tool, h) {
		return true
	}
	for _, r := range p.Clients {
		if slices.Contains(r.Allow, tool) {
			return true
		}
	}
	return false
}

// RuleFor finds the rule for a client: an exact selector first, then the longest prefix, then the longest
// matching domain.
func (p ToolPolicy) RuleFor(client string) (ClientRule, bool) {
	if len(p.Clients) == 0 || client == "" {
		return ClientRule{}, false
	}
	if r, ok := p.Clients[client]; ok {
		return r, true
	}
	best, bestLen, bestDomain := ClientRule{}, -1, false
	host := ""
	if u, err := url.Parse(client); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		host = strings.ToLower(u.Hostname())
	}
	for sel, r := range p.Clients {
		if pre, ok := strings.CutSuffix(sel, "*"); ok {
			if strings.HasPrefix(client, pre) && (len(pre) > bestLen || bestDomain) {
				best, bestLen, bestDomain = r, len(pre), false
			}
			continue
		}
		if host == "" || strings.Contains(sel, "://") || strings.HasPrefix(sel, "key:") || bestLen >= 0 && !bestDomain {
			continue
		}
		d := strings.ToLower(sel)
		if (host == d || strings.HasSuffix(host, "."+d)) && len(d) > bestLen {
			best, bestLen, bestDomain = r, len(d), true
		}
	}
	return best, bestLen >= 0
}

// AllowsAll reports whether every tool, present and future, is exposed to some client (hello `tools: null`).
func (p ToolPolicy) AllowsAll() bool {
	return p.Default == DefaultAllow && len(p.Deny) == 0
}

// AllowList returns the names among the known tools that some client may use, in their order.
func (p ToolPolicy) AllowList(known []string) []string {
	return p.AllowListHints(known, func(string) Hint { return Hint{} })
}

// AllowListHints is AllowList with each tool's annotations.
func (p ToolPolicy) AllowListHints(known []string, hint func(string) Hint) []string {
	out := []string{}
	for _, t := range known {
		if p.AllowedAny(t, hint(t)) && !slices.Contains(out, t) {
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
	if p.Default != DefaultAllow && p.Default != DefaultReadOnly {
		p.Default = DefaultDeny
	}
	for k, r := range p.Clients {
		if len(r.Allow) == 0 && len(r.Deny) == 0 {
			delete(p.Clients, k)
		}
	}
	if len(p.Clients) == 0 {
		p.Clients = nil
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

// LoadMigrated is Load followed by MigrateEnv.
func LoadMigrated(dir string, store secrets.Store) (*Config, error) {
	c, err := Load(dir)
	if err != nil {
		return nil, err
	}
	if err := MigrateEnv(dir, c, store); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadOrEmptyMigrated is LoadOrEmpty followed by MigrateEnv.
func LoadOrEmptyMigrated(dir string, store secrets.Store) (*Config, error) {
	c, err := LoadOrEmpty(dir)
	if err != nil {
		return nil, err
	}
	if err := MigrateEnv(dir, c, store); err != nil {
		return nil, err
	}
	return c, nil
}

// MigrateEnv moves stdio environment values that an older version wrote into config.json to the keyring, keeps
// only their names, and rewrites the config. It does nothing when no service has values.
func MigrateEnv(dir string, c *Config, store secrets.Store) error {
	changed := false
	for i := range c.Services {
		s := &c.Services[i]
		if len(s.Source.Env) == 0 {
			if s.Source.Env != nil {
				s.Source.Env, changed = nil, true
			}
			continue
		}
		for _, k := range slices.Sorted(maps.Keys(s.Source.Env)) {
			if err := store.Set(secrets.StdioEnv(s.Name, k), s.Source.Env[k]); err != nil {
				return fmt.Errorf("move %s's environment variable %s to the keyring: %w", s.Name, k, err)
			}
			if !slices.Contains(s.Source.EnvNames, k) {
				s.Source.EnvNames = append(s.Source.EnvNames, k)
			}
		}
		slices.Sort(s.Source.EnvNames)
		s.Source.Env, changed = nil, true
	}
	if !changed {
		return nil
	}
	if err := Save(dir, c); err != nil {
		return fmt.Errorf("rewrite the config without environment values: %w", err)
	}
	return nil
}

// ErrEnvValues means a config with stdio environment values was about to be written.
var ErrEnvValues = errors.New("stdio environment values belong in the keyring, not config.json")

// Save writes config.json atomically with mode 0600. It refuses a config that carries stdio environment values.
func Save(dir string, c *Config) error {
	for _, s := range c.Services {
		if len(s.Source.Env) > 0 {
			return fmt.Errorf("service %s: %w", s.Name, ErrEnvValues)
		}
	}
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

// HasUpstream reports whether the service reaches its app over HTTP (an MCP URL or an http service).
func (s Service) HasUpstream() bool {
	return s.Source.Type == SourceMCPURL || s.Source.Type == SourceHTTP
}

// PublicPath is the path under PUBLIC_URL that bots use.
func (s Service) PublicPath() string {
	if s.Kind == KindMCP {
		return "/" + s.Name + "/mcp"
	}
	return "/" + s.Name + "/"
}
