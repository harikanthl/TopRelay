package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/harikanthl/toprelay/agent/internal/cfapi"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/deploy"
	"github.com/harikanthl/toprelay/agent/internal/keys"
	"github.com/harikanthl/toprelay/agent/internal/relayapi"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
)

func (a *App) cfClient() (*cfapi.Client, error) {
	tok, err := a.Store.Get(secrets.CloudflareToken)
	if errors.Is(err, secrets.ErrNotFound) {
		return nil, errf("not_logged_in", "run `toprelay login`", "no Cloudflare token stored")
	}
	if err != nil {
		return nil, wrap("keyring", "set TOPRELAY_KEYRING=file to use a file instead of the OS keyring", err)
	}
	return cfapi.New(tok, a.CFBase), nil
}

// relayBase is where admin calls and tunnels go: TOPRELAY_RELAY_URL or the deployed public URL.
func (a *App) relayBase(cfg *config.Config) (string, error) {
	if a.RelayURL != "" {
		return a.RelayURL, nil
	}
	if cfg.PublicURL == "" {
		return "", errf("not_deployed", "run `toprelay add <name> …` to deploy the relay", "the relay isn't deployed yet")
	}
	return cfg.PublicURL, nil
}

func (a *App) agentKey(s *config.Service) (string, error) {
	root, err := secrets.Require(a.Store, secrets.AgentRoot, "it is created by the first `toprelay add`")
	if err != nil {
		return "", wrap("no_roots", "run `toprelay add` or restore the keyring", err)
	}
	return keys.AgentKey(root, s.Name, s.Gen)
}

func (a *App) relay(cfg *config.Config) (*relayapi.Client, error) {
	base, err := a.relayBase(cfg)
	if err != nil {
		return nil, err
	}
	return relayapi.New(base), nil
}

// ensureRoots creates AGENT_ROOT and BEARER_ROOT if they don't exist yet.
func (a *App) ensureRoots() (agentRoot, bearerRoot string, created bool, err error) {
	ar, aerr := a.Store.Get(secrets.AgentRoot)
	br, berr := a.Store.Get(secrets.BearerRoot)
	if aerr == nil && berr == nil {
		return ar, br, false, nil
	}
	if (aerr != nil && !errors.Is(aerr, secrets.ErrNotFound)) || (berr != nil && !errors.Is(berr, secrets.ErrNotFound)) {
		return "", "", false, wrap("keyring", "check keyring access", errors.Join(aerr, berr))
	}
	if aerr == nil || berr == nil {
		return "", "", false, errf("roots_partial", "restore the missing secret from your keyring backup, or remove every service and start over",
			"only one of the two root secrets is in the keyring")
	}
	if ar, err = keys.NewRoot(); err != nil {
		return "", "", false, err
	}
	if br, err = keys.NewRoot(); err != nil {
		return "", "", false, err
	}
	if err := a.Store.Set(secrets.AgentRoot, ar); err != nil {
		return "", "", false, wrap("keyring", "check keyring access", err)
	}
	if err := a.Store.Set(secrets.BearerRoot, br); err != nil {
		return "", "", false, wrap("keyring", "check keyring access", err)
	}
	return ar, br, true, nil
}

// deployConfig redeploys the Worker with cfg's services and records what the deploy learned in cfg.
func (a *App) deployConfig(cfg *config.Config) (*deploy.Result, error) {
	api, err := a.cfClient()
	if err != nil {
		return nil, err
	}
	if cfg.AccountID == "" {
		return nil, errf("no_account", "run `toprelay login`", "no Cloudflare account selected")
	}
	ar, br, _, err := a.ensureRoots()
	if err != nil {
		return nil, err
	}
	var svcs []deploy.ServiceEntry
	for _, s := range cfg.Services {
		svcs = append(svcs, deploy.ServiceEntry{Name: s.Name, Kind: s.Kind, Gen: s.Gen})
	}
	d := &deploy.Deployer{API: api, PollInterval: a.DeployPoll, Health: a.DeployHealth, HealthBase: a.RelayURL,
		Progress: func(s string) { a.note("  %s\n", s) }}
	res, err := d.Deploy(a.Ctx, deploy.Input{AccountID: cfg.AccountID, AccountName: cfg.AccountName, Subdomain: cfg.Subdomain,
		KVNamespaceID: cfg.KVNamespaceID, Services: svcs, AgentRoot: ar, BearerRoot: br})
	if err != nil {
		return nil, wrap("deploy", "run `toprelay doctor` to find the cause", err)
	}
	cfg.Subdomain, cfg.PublicURL, cfg.KVNamespaceID = res.Subdomain, res.PublicURL, res.KVNamespaceID
	return res, nil
}

// serviceURL is the public MCP or HTTP URL of a service.
func serviceURL(cfg *config.Config, s *config.Service) string {
	return strings.TrimRight(cfg.PublicURL, "/") + s.PublicPath()
}

func sseURL(cfg *config.Config, s *config.Service) string {
	if s.Source.Type != config.SourceStdio {
		return ""
	}
	return strings.TrimRight(cfg.PublicURL, "/") + "/" + s.Name + "/mcp/sse"
}

// connectionSteps explains how each bot connects (no secrets: the key comes from `toprelay key`).
func connectionSteps(cfg *config.Config, s *config.Service) string {
	u := serviceURL(cfg, s)
	var b strings.Builder
	if s.Kind == config.KindHTTP {
		fmt.Fprintf(&b, `Connect:
  Any HTTP client: send requests under %s with "Authorization: Bearer <key>".
    Get a key with: toprelay key %s --for 30d
`, u, s.Name)
		return b.String()
	}
	fmt.Fprintf(&b, `Connect a bot (URL: %s):
  claude.ai     Settings › Connectors › Add custom connector › paste the URL (OAuth).
                When the consent page asks for a pairing code, run: toprelay pair %s
  Claude API    mcp_servers[].url = the URL, authorization_token = a bearer key
                (beta header mcp-client-2025-11-20, plus a tools[] mcp_toolset entry):
                toprelay key %s --client claude-api
  OpenAI        Responses API tools[] {"type":"mcp","server_url":…,"authorization":<key>}:
                toprelay key %s --client openai
  xAI (Grok)    Remote MCP tool with the URL and the key: toprelay key %s --client xai
  Gemini        Streamable HTTP with headers {"Authorization":"Bearer <key>"}: toprelay key %s --client gemini
`, u, s.Name, s.Name, s.Name, s.Name, s.Name)
	if su := sseURL(cfg, s); su != "" {
		fmt.Fprintf(&b, "  Qwen / Model Studio  SSE URL %s with headers {\"Authorization\":\"Bearer <key>\"}: toprelay key %s --client qwen\n", su, s.Name)
	} else {
		b.WriteString("  Qwen / Model Studio  needs the legacy SSE transport, which TopRelay serves only for --stdio services.\n")
	}
	return b.String()
}
