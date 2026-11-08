package cli

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/harikanthl/toprelay/agent/internal/caller"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/httpfwd"
	"github.com/harikanthl/toprelay/agent/internal/names"
	"github.com/harikanthl/toprelay/agent/internal/redact"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/service"
	"github.com/harikanthl/toprelay/agent/internal/upstream"
)

const addHelp = `
Expose a local app through your relay. Exactly one source:

  --mcp-url URL          a Streamable HTTP MCP server, e.g. http://127.0.0.1:7879/mcp, or one on a Unix socket:
                         unix:///path/app.sock (HTTP path /mcp; another with unix:///path/app.sock?path=/rpc)
  --stdio -- cmd args…   a stdio MCP server; TopRelay launches it and bridges it to Streamable HTTP and SSE.
                         It runs with YOUR user's permissions: only add commands you trust.
  --http URL             a plain HTTP service (raw forwarding, no tool discovery); limit it with --allow-path

If the app checks a token, give TopRelay the header it expects, in one of these ways:

  --header-file Name=/path   the agent reads the value from that file on every request, so the app can rotate it
                             (the file must be yours, mode 0600). Best for apps.
  --header-stdin             read "Name: value" from the first line of stdin; the value goes to the keyring
  --header "Name: value"     discouraged: the value is visible to other processes (ps) while add runs

Change it later, live, with ` + "`toprelay set-header <name>`" + `. Every relayed request also carries a signed
X-TopRelay-Caller header the app can verify (` + "`toprelay caller-key <name>`" + `, MIGRATION.md).

For a TCP app on this Mac, --pin-team-id TEAMID [--pin-bundle-id ID] makes the agent check the code signature of
the process listening on the port before it connects, so another program squatting the port gets nothing.
A unix:// socket must be yours with mode 0600; the agent checks the peer's user on every connection.

The first add deploys the relay Worker into your Cloudflare account (run ` + "`toprelay login`" + ` first).
For MCP sources, tools marked readOnlyHint are exposed by default; you're asked about the rest (y/N), or pick
with --tools-default. Change it later with ` + "`toprelay tools <name>`" + `.

Examples:
  toprelay add notes --mcp-url http://127.0.0.1:7879/mcp
  toprelay add notes --mcp-url http://127.0.0.1:7879/mcp --header-file X-Notes-Token=$HOME/Library/Application\ Support/Notes/token
  echo "X-Notes-Token: $TOKEN" | toprelay add notes --mcp-url unix://$HOME/.notes/mcp.sock --header-stdin
  toprelay add files --stdio --env ROOT=/Users/me/Docs -- npx -y @modelcontextprotocol/server-filesystem /Users/me/Docs
  toprelay add web --http http://127.0.0.1:8080 --allow-path /api`

// AddResult is the --json output of add.
type AddResult struct {
	Service     config.Service `json:"service"`
	URL         string         `json:"url"`
	SSEURL      string         `json:"sseUrl,omitempty"`
	Tools       []ToolView     `json:"tools"`
	Warnings    []string       `json:"warnings"`
	FirstDeploy bool           `json:"firstDeploy"`
	Subdomain   string         `json:"subdomain"`
}

func cmdAdd(a *App, args []string) error {
	fs := a.newFlags("add", "add <name> (--mcp-url URL | --stdio [--env K=V]… [--cwd DIR] -- cmd args… | --http URL [--allow-path P]…) [--header-file Name=/path | --header-stdin] [--pin-team-id T [--pin-bundle-id B]] [--caller-key-file PATH] [--tools-default allow-readonly|deny|allow-all]", addHelp)
	mcpURL := fs.String("mcp-url", "", "Streamable HTTP MCP endpoint of the local app")
	stdio := fs.Bool("stdio", false, "launch a stdio MCP server (command after --)")
	httpURL := fs.String("http", "", "base URL of a plain HTTP service")
	var envs, paths multi
	fs.Var(&envs, "env", "environment variable for the stdio command, K=V (repeatable)")
	cwd := fs.String("cwd", "", "working directory for the stdio command")
	fs.Var(&paths, "allow-path", "path prefix the relay may reach on an --http service (repeatable; default: all)")
	header := fs.String("header", "", "DISCOURAGED (visible in ps): upstream header \"Name: value\"; use --header-file or --header-stdin")
	headerStdin := fs.Bool("header-stdin", false, "read the upstream header \"Name: value\" from stdin (the value goes to the keyring)")
	headerFile := fs.String("header-file", "", "upstream header Name=/path: the value is read from the file on every request")
	pinTeam := fs.String("pin-team-id", "", "macOS: only connect if the app listening is signed by this Apple team id")
	pinBundle := fs.String("pin-bundle-id", "", "macOS: and has this bundle id (needs --pin-team-id)")
	filesDir := fs.String("files-dir", "", "outbox directory: files in it can be shared as toprelay-file:///<name> links (--mcp-url only)")
	callerKeyFile := fs.String("caller-key-file", "", "use the app's own key file (0600, ≥ 32 chars) to sign X-TopRelay-Caller")
	toolsDefault := fs.String("tools-default", "", "which tools to expose without asking: allow-readonly (default), deny, allow-all")
	pos, rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("add needs exactly one service name, e.g. `toprelay add notes --mcp-url …`")
	}
	name := pos[0]
	if err := names.ValidateService(name); err != nil {
		return wrap("bad_name", "service names are 1-32 lowercase letters and digits", err)
	}
	sources := 0
	for _, set := range []bool{*mcpURL != "", *stdio, *httpURL != ""} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return usageErr("give exactly one of --mcp-url, --stdio or --http")
	}
	if len(rest) > 0 && !*stdio {
		return usageErr("arguments after -- are only for --stdio")
	}
	switch *toolsDefault {
	case "", "allow-readonly", "deny", "allow-all":
	default:
		return usageErr("--tools-default must be allow-readonly, deny or allow-all")
	}

	cfg, err := config.LoadOrEmptyMigrated(a.Dir, a.Store)
	if err != nil {
		return wrap("config", "fix or delete "+config.Path(a.Dir), err)
	}
	if cfg.Find(name) != nil {
		return errf("exists", "pick another name, or `toprelay remove "+name+"` first", "a service named %q already exists", name)
	}
	if _, err := a.cfClient(); err != nil {
		return err
	}
	if cfg.AccountID == "" {
		return errf("no_account", "run `toprelay login`", "no Cloudflare account selected")
	}

	s := config.Service{Name: name, Gen: 1}
	var warnings []string
	var envValues map[string]string
	switch {
	case *mcpURL != "":
		if err := checkURL(*mcpURL, "/mcp"); err != nil {
			return wrap("bad_url", "use a URL like http://127.0.0.1:7879/mcp or unix:///path/app.sock", err)
		}
		s.Kind, s.Source = config.KindMCP, config.Source{Type: config.SourceMCPURL, URL: *mcpURL}
	case *stdio:
		if len(rest) == 0 {
			return usageErr("--stdio needs the command after --, e.g. `toprelay add tools --stdio -- node server.js`")
		}
		env := map[string]string{}
		for _, kv := range envs {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return usageErr("--env wants K=V, got %q", kv)
			}
			env[k] = v
		}
		dir := *cwd
		if dir == "" {
			if wd, err := os.Getwd(); err == nil {
				dir = wd
			}
		} else if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		// Values go to the keyring (after the deploy); config.json and --json output only carry the names.
		s.Kind, s.Source = config.KindMCP, config.Source{Type: config.SourceStdio, Command: rest, Cwd: dir}
		for _, k := range slices.Sorted(maps.Keys(env)) {
			s.Source.EnvNames = append(s.Source.EnvNames, k)
			redact.Add(env[k])
		}
		envValues = env
		warnings = append(warnings, "stdio servers run with your user's permissions: everything the command can do on this computer, an allowed tool call can trigger")
	case *httpURL != "":
		if err := checkURL(*httpURL, "/"); err != nil {
			return wrap("bad_url", "use a URL like http://127.0.0.1:8080 or unix:///path/app.sock", err)
		}
		s.Kind, s.Source = config.KindHTTP, config.Source{Type: config.SourceHTTP, URL: strings.TrimRight(*httpURL, "/")}
		s.AllowPaths = []string{}
		for _, p := range paths {
			c, ok := httpfwd.CleanPath("/" + strings.TrimLeft(p, "/"))
			if !ok {
				return usageErr("bad --allow-path %q", p)
			}
			s.AllowPaths = append(s.AllowPaths, c)
		}
		if len(s.AllowPaths) == 0 {
			s.AllowPaths = []string{"/"}
			warnings = append(warnings, "no --allow-path given: every path of "+s.Source.URL+" is reachable through the relay (with a valid key)")
		}
	}
	hs, err := a.headerFlags(*header, *headerStdin, *headerFile)
	if err != nil {
		return err
	}
	if hs != nil {
		if !s.HasUpstream() {
			return usageErr("--header* flags are for --mcp-url and --http services; give a stdio server its secrets with --env")
		}
		s.UpstreamHeader, s.UpstreamHeaderFile = hs.name, hs.file
		if hs.discouraged {
			warnings = append(warnings, "--header puts the secret on the command line, where other processes can read it (ps); use --header-file or --header-stdin")
		}
	}
	if *pinBundle != "" && *pinTeam == "" {
		return usageErr("--pin-bundle-id needs --pin-team-id: a bundle id alone proves nothing, anyone can sign with it")
	}
	if *pinTeam != "" {
		if !s.HasUpstream() {
			return usageErr("--pin-team-id is for --mcp-url and --http services")
		}
		s.Pin = &config.Pin{TeamID: *pinTeam, BundleID: *pinBundle}
	}
	if *callerKeyFile != "" {
		if !s.HasUpstream() {
			return usageErr("--caller-key-file is for --mcp-url and --http services")
		}
		if err := checkCallerKeyFile(*callerKeyFile); err != nil {
			return err
		}
		s.CallerKeyFile = *callerKeyFile
	}
	if *filesDir != "" {
		if s.Source.Type != config.SourceMCPURL {
			return usageErr("--files-dir is for --mcp-url services")
		}
		d, err := checkFilesDir(*filesDir)
		if err != nil {
			return err
		}
		s.FilesDir = d
	}
	var up *upstream.Upstream
	if s.HasUpstream() {
		// Built from the flags: the value isn't in the keyring until the deploy succeeds.
		ep, err := service.Endpoint(s)
		if err != nil {
			return wrap("bad_url", "check the URL", err)
		}
		var cred *upstream.Credential
		if hs != nil {
			cred = &upstream.Credential{Name: hs.name, Value: hs.value, File: hs.file}
		}
		if up, err = upstream.New(upstream.Options{Endpoint: ep, Pin: service.Pin(s), Credential: cred}); err != nil {
			return wrap("bad_upstream", "check --pin-team-id and the URL", err)
		}
	}

	var views []ToolView
	if s.Kind == config.KindMCP {
		a.note("Connecting to %s …\n", s.LocalTarget())
		tools, err := a.listLocalTools(&s, up, envValues)
		if err != nil {
			return err
		}
		s.Tools = a.choosePolicy(name, tools, *toolsDefault, &warnings)
		views = toolViews(name, tools, s.Tools)
		allowed := 0
		for _, v := range views {
			warnings = append(warnings, v.Warnings...)
			if v.Allowed {
				allowed++
			}
		}
		if allowed > 20 {
			warnings = append(warnings, fmt.Sprintf("%d tools are exposed; models choose worse past 10-20, so consider `toprelay tools %s --deny …`", allowed, name))
		}
	} else {
		s.Tools = config.ToolPolicy{Default: config.DefaultAllow}
	}
	s.Tools.Normalize()

	cfg.Services = append(cfg.Services, s)
	a.note("Deploying the relay …\n")
	res, err := a.deployConfig(cfg)
	if err != nil {
		return err
	}
	if hs != nil && hs.file == "" {
		if err := a.Store.Set(secrets.UpstreamHeader(name), hs.value); err != nil {
			return wrap("keyring", "check keyring access", err)
		}
	}
	if s.HasUpstream() && s.CallerKeyFile == "" {
		if _, err := caller.EnsureKeyFile(caller.DefaultKeyPath(a.Dir, name)); err != nil {
			return wrap("caller_key", "check permissions on "+a.Dir, err)
		}
	}
	for _, k := range s.Source.EnvNames {
		if err := a.Store.Set(secrets.StdioEnv(name, k), envValues[k]); err != nil {
			return wrap("keyring", "check keyring access", err)
		}
	}
	if err := config.Save(a.Dir, cfg); err != nil {
		return wrap("config", "check permissions on "+a.Dir, err)
	}
	sp := cfg.Find(name)
	out := AddResult{Service: *sp, URL: serviceURL(cfg, sp), SSEURL: sseURL(cfg, sp), Tools: views, Warnings: warnings,
		FirstDeploy: res.FirstDeploy, Subdomain: res.Subdomain}
	if out.Tools == nil {
		out.Tools = []ToolView{}
	}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	return a.emit(out, func() {
		a.say("✓ relay deployed to your Cloudflare account\n✓ %s   ← give this to your bot\n", out.URL)
		for _, v := range views {
			mark := "hidden "
			if v.Allowed {
				mark = "exposed"
			}
			a.say("  %s  %s\n", mark, v.Name)
		}
		for _, w := range warnings {
			a.say("warning: %s\n", w)
		}
		a.say("\n%s\nThen keep it connected: toprelay up   (or toprelay up --install)\n", connectionSteps(cfg, sp))
	})
}

func checkURL(s, defaultPath string) error {
	_, err := upstream.ParseEndpoint(s, defaultPath)
	return err
}

// choosePolicy applies --tools-default, or asks on a TTY, or denies non-read-only tools.
func (a *App) choosePolicy(service string, tools []*mcp.Tool, mode string, warnings *[]string) config.ToolPolicy {
	p := defaultPolicy(tools)
	switch mode {
	case "deny":
		p = config.ToolPolicy{Default: config.DefaultDeny}
		for _, t := range tools {
			p.SetDenied(t.Name)
		}
		return p
	case "allow-all":
		return config.ToolPolicy{Default: config.DefaultAllow}
	case "allow-readonly":
		return p
	}
	var rest []*mcp.Tool
	for _, t := range tools {
		if !readOnly(t) {
			rest = append(rest, t)
		}
	}
	if len(rest) == 0 {
		return p
	}
	if !a.interactive() {
		var n []string
		for _, t := range rest {
			n = append(n, t.Name)
		}
		*warnings = append(*warnings, fmt.Sprintf("not exposed (no readOnlyHint): %s. To expose some: toprelay tools %s --allow %s",
			strings.Join(n, ", "), service, n[0]))
		return p
	}
	a.say("These tools may change data (no readOnlyHint). Expose each?\n")
	for _, t := range rest {
		desc := t.Description
		if len(desc) > 80 {
			desc = desc[:77] + "…"
		}
		if a.confirm(fmt.Sprintf("  %s: %s", t.Name, desc)) {
			p.SetAllowed(t.Name)
		}
	}
	return p
}
