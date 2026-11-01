// Package deploy uploads the relay Worker into the user's Cloudflare account, idempotently.
package deploy

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/cfapi"
)

//go:embed relay.js
var relayJS []byte

// Bundle returns the embedded relay Worker bundle.
func Bundle() []byte { return relayJS }

const (
	// ScriptName is the Worker script name.
	ScriptName = "toprelay"
	// KVTitle is the title of the OAuth KV namespace.
	KVTitle = "toprelay-oauth"
	// CompatibilityDate is the Worker compatibility date.
	CompatibilityDate = "2026-10-01"
)

// CompatibilityFlags are the Worker compatibility flags from protocol.md.
var CompatibilityFlags = []string{"global_fetch_strictly_public", "enable_request_signal"}

var exportsRelay = regexp.MustCompile(`export\s+class\s+Relay\b|export\s*\{[^}]*\bRelay\b[^}]*\}`)

// ServiceEntry is one element of the SERVICES binding.
type ServiceEntry struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Gen  int    `json:"gen"`
}

// Input is everything a deploy needs.
type Input struct {
	AccountID   string
	AccountName string
	// Known values from a previous deploy; empty means "look it up".
	Subdomain     string
	KVNamespaceID string
	Services      []ServiceEntry
	AgentRoot     string
	BearerRoot    string
}

// Result is what a deploy learned or created.
type Result struct {
	Subdomain     string
	PublicURL     string
	KVNamespaceID string
	FirstDeploy   bool
}

// Deployer runs the REST steps of brief.md "Deploy".
type Deployer struct {
	API    *cfapi.Client
	Script []byte
	// Health polls PUBLIC_URL/health; nil uses a plain client.
	Health       *http.Client
	PollInterval time.Duration
	PollTimeout  time.Duration
	// PublicURLFor overrides the derived https://toprelay.<sub>.workers.dev (for tests).
	PublicURLFor func(subdomain string) string
	// HealthBase, if set, is polled instead of PUBLIC_URL (TOPRELAY_RELAY_URL, e.g. wrangler dev).
	HealthBase string
	Log        *slog.Logger
	// Progress, if set, receives one line per step for the CLI.
	Progress func(string)
}

func (d *Deployer) progress(format string, a ...any) {
	if d.Progress != nil {
		d.Progress(fmt.Sprintf(format, a...))
	}
}

// PublicURL is the relay's URL for a workers.dev subdomain.
func PublicURL(subdomain string) string {
	return "https://" + ScriptName + "." + subdomain + ".workers.dev"
}

// Deploy runs every step and returns the result.
func (d *Deployer) Deploy(ctx context.Context, in Input) (*Result, error) {
	script := d.Script
	if script == nil {
		script = relayJS
	}
	if !exportsRelay.Match(script) {
		return nil, errors.New("the embedded relay.js doesn't export the Relay Durable Object class: this build of toprelay has no Worker bundle (build worker/ and copy its bundle to agent/internal/deploy/relay.js)")
	}
	if in.AccountID == "" {
		return nil, errors.New("no Cloudflare account selected: run `toprelay login`")
	}
	if in.AgentRoot == "" || in.BearerRoot == "" {
		return nil, errors.New("root secrets are missing")
	}
	res := &Result{}

	// Step 2: the workers.dev subdomain.
	sub, err := d.ensureSubdomain(ctx, in)
	if err != nil {
		return nil, err
	}
	res.Subdomain = sub
	if d.PublicURLFor != nil {
		res.PublicURL = d.PublicURLFor(sub)
	} else {
		res.PublicURL = PublicURL(sub)
	}
	d.progress("workers.dev subdomain: %s", sub)

	// Step 3: the KV namespace.
	kv, err := d.ensureKV(ctx, in)
	if err != nil {
		return nil, err
	}
	res.KVNamespaceID = kv

	// Step 4: first deploy?
	exists, err := d.API.ScriptExists(ctx, in.AccountID, ScriptName)
	if err != nil {
		return nil, err
	}
	res.FirstDeploy = !exists

	// Step 5: upload.
	meta, err := Metadata(res.PublicURL, kv, in.Services, in.AgentRoot, in.BearerRoot, res.FirstDeploy)
	if err != nil {
		return nil, err
	}
	if err := d.API.PutScript(ctx, in.AccountID, ScriptName, meta, []cfapi.Module{
		{Name: "relay.js", ContentType: "application/javascript+module", Content: script},
	}); err != nil {
		return nil, fmt.Errorf("upload the relay Worker: %w", err)
	}
	d.progress("relay Worker uploaded (%d services)", len(in.Services))

	// Step 6: enable the workers.dev route.
	if err := d.API.EnableScriptSubdomain(ctx, in.AccountID, ScriptName); err != nil {
		return nil, fmt.Errorf("enable the workers.dev route: %w", err)
	}

	// Step 7: wait for /health.
	healthBase := res.PublicURL
	if d.HealthBase != "" {
		healthBase = strings.TrimRight(d.HealthBase, "/")
	}
	if err := d.waitHealthy(ctx, healthBase); err != nil {
		return nil, err
	}
	d.progress("relay is up at %s", res.PublicURL)
	return res, nil
}

// Metadata builds the upload metadata exactly as protocol.md "Worker configuration" says.
func Metadata(publicURL, kvID string, services []ServiceEntry, agentRoot, bearerRoot string, first bool) ([]byte, error) {
	if services == nil {
		services = []ServiceEntry{}
	}
	svcJSON, err := json.Marshal(services)
	if err != nil {
		return nil, err
	}
	type binding struct {
		Type        string `json:"type"`
		Name        string `json:"name"`
		ClassName   string `json:"class_name,omitempty"`
		NamespaceID string `json:"namespace_id,omitempty"`
		Text        string `json:"text,omitempty"`
	}
	type migrations struct {
		NewTag           string   `json:"new_tag"`
		NewSqliteClasses []string `json:"new_sqlite_classes"`
	}
	type logs struct {
		Enabled        bool `json:"enabled"`
		InvocationLogs bool `json:"invocation_logs"`
	}
	type observability struct {
		Enabled bool `json:"enabled"`
		Logs    logs `json:"logs"`
	}
	type metadata struct {
		MainModule         string        `json:"main_module"`
		CompatibilityDate  string        `json:"compatibility_date"`
		CompatibilityFlags []string      `json:"compatibility_flags"`
		Bindings           []binding     `json:"bindings"`
		Observability      observability `json:"observability"`
		Migrations         *migrations   `json:"migrations,omitempty"`
	}
	m := metadata{
		MainModule:         "relay.js",
		CompatibilityDate:  CompatibilityDate,
		CompatibilityFlags: CompatibilityFlags,
		Bindings: []binding{
			{Type: "durable_object_namespace", Name: "RELAY", ClassName: "Relay"},
			{Type: "kv_namespace", Name: "OAUTH_KV", NamespaceID: kvID},
			{Type: "plain_text", Name: "PUBLIC_URL", Text: strings.TrimRight(publicURL, "/")},
			{Type: "plain_text", Name: "SERVICES", Text: string(svcJSON)},
			{Type: "secret_text", Name: "AGENT_ROOT", Text: agentRoot},
			{Type: "secret_text", Name: "BEARER_ROOT", Text: bearerRoot},
		},
		Observability: observability{Enabled: true, Logs: logs{Enabled: true, InvocationLogs: false}},
	}
	if first {
		m.Migrations = &migrations{NewTag: "v1", NewSqliteClasses: []string{"Relay"}}
	}
	return json.Marshal(m)
}

func (d *Deployer) ensureSubdomain(ctx context.Context, in Input) (string, error) {
	sub, err := d.API.GetSubdomain(ctx, in.AccountID)
	if err == nil {
		return sub, nil
	}
	if !cfapi.IsCode(err, cfapi.CodeNoSubdomain) {
		return "", fmt.Errorf("look up the workers.dev subdomain: %w", err)
	}
	base := SubdomainFromName(in.AccountName)
	candidates := []string{base}
	for range 5 {
		b := make([]byte, 2)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		suffix := "-" + hex.EncodeToString(b)
		candidates = append(candidates, trimLabel(base, 63-len(suffix))+suffix)
	}
	for _, c := range candidates {
		got, err := d.API.PutSubdomain(ctx, in.AccountID, c)
		if err == nil {
			d.progress("registered workers.dev subdomain %s", got)
			return got, nil
		}
		if !cfapi.IsCode(err, cfapi.CodeSubdomainTaken) {
			return "", fmt.Errorf("register a workers.dev subdomain: %w", err)
		}
	}
	return "", errors.New("every workers.dev subdomain tried was taken: choose one in the Cloudflare dashboard (Workers & Pages › Settings) and run the command again")
}

// SubdomainFromName derives a valid workers.dev subdomain label from an account name.
func SubdomainFromName(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	s = strings.TrimSuffix(s, "-s-account")
	s = strings.TrimSuffix(s, "-account")
	s = trimLabel(s, 63)
	if s == "" {
		return "toprelay"
	}
	return s
}

func trimLabel(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.Trim(s, "-")
}

func (d *Deployer) ensureKV(ctx context.Context, in Input) (string, error) {
	all, err := d.API.ListKVNamespaces(ctx, in.AccountID)
	if err != nil {
		return "", fmt.Errorf("list KV namespaces: %w", err)
	}
	for _, ns := range all {
		if ns.Title == KVTitle {
			return ns.ID, nil
		}
	}
	ns, err := d.API.CreateKVNamespace(ctx, in.AccountID, KVTitle)
	if err != nil {
		return "", fmt.Errorf("create the %s KV namespace: %w", KVTitle, err)
	}
	d.progress("created KV namespace %s", KVTitle)
	return ns.ID, nil
}

func (d *Deployer) waitHealthy(ctx context.Context, publicURL string) error {
	client := d.Health
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	interval, timeout := d.PollInterval, d.PollTimeout
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last string
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", publicURL+"/health", nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			last = err.Error()
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("the relay was uploaded but %s/health didn't answer 200 within %s (last: %s); new workers.dev subdomains can take a few minutes, so run `toprelay doctor` shortly", publicURL, timeout, last)
		case <-t.C:
		}
	}
}
