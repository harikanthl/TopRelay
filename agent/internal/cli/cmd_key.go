package cli

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/keys"
)

// Clients that `key --client` knows.
var Clients = []string{"claude-api", "openai", "xai", "gemini", "qwen", "curl"}

// KeyResult is key --json.
type KeyResult struct {
	Key      string            `json:"key"`
	ID       string            `json:"id"`
	Service  string            `json:"service,omitempty"`
	Gen      string            `json:"gen,omitempty"`
	Tools    []string          `json:"tools"`
	Expires  int64             `json:"expires,omitempty"`
	URL      string            `json:"url,omitempty"`
	Snippets map[string]string `json:"snippets,omitempty"`
	Warnings []string          `json:"warnings"`
}

func cmdKey(a *App, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "narrow":
			return cmdKeyNarrow(a, args[1:])
		case "revoke":
			return cmdKeyRevoke(a, args[1:])
		}
	}
	fs := a.newFlags("key", "key <name> [--tools a,b] [--for 1h] [--client claude-api|openai|xai|gemini|qwen|curl]\n       toprelay key narrow <key> [--tools a,b] [--for 1h]\n       toprelay key revoke <name> <id>", `
Mint a bearer key for bots that take a fixed header (Claude API, OpenAI, xAI, Gemini, Qwen, SDKs). It is made
locally, needs no redeploy, and is printed once: store it in the bot's secret settings.

  --tools a,b   the key can only list and call these tools
  --for 1h      the key expires (30m, 1h, 7d …); without it the key lasts until revoked or rotated
  --client X    print the snippet for one client (default: all)

`+"`key narrow`"+` restricts an existing key further, offline (no secret needed). `+"`key revoke`"+` blocks one key by id;
`+"`toprelay rotate`"+` voids them all.`)
	toolsS := fs.String("tools", "", "comma-separated tools the key may use")
	forS := fs.String("for", "", "lifetime, e.g. 1h or 7d")
	client := fs.String("client", "", "one of "+strings.Join(Clients, ", "))
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("key needs a service name (or `key narrow <key>` / `key revoke <name> <id>`)")
	}
	if *client != "" && !slices.Contains(Clients, *client) {
		return usageErr("--client must be one of %s", strings.Join(Clients, ", "))
	}
	cfg, s, err := a.loadService(pos[0])
	if err != nil {
		return err
	}
	var tools []string
	var warnings []string
	if *toolsS != "" {
		if s.Kind != config.KindMCP {
			return usageErr("--tools only applies to mcp services")
		}
		tools = splitList(*toolsS)
		for _, t := range tools {
			if !s.Tools.Allowed(t) {
				warnings = append(warnings, fmt.Sprintf("%q isn't exposed by the service's policy, so this key can't use it either (toprelay tools %s --allow %s)", t, s.Name, t))
			}
		}
	}
	var exp time.Time
	if *forS != "" {
		d, err := parseDuration(*forS)
		if err != nil {
			return usageErr("%v", err)
		}
		exp = a.Now().Add(d)
	} else {
		warnings = append(warnings, "this key never expires; consider --for 30d, and revoke it with `toprelay key revoke "+s.Name+" <id>`")
	}
	b, err := a.keyFor(s, tools, exp)
	if err != nil {
		return err
	}
	res := keyResult(b)
	res.URL = serviceURL(cfg, s)
	res.Warnings = nonNil(warnings)
	res.Snippets = snippets(cfg, s, b.String(), *client)
	return a.emit(res, func() {
		a.say("Bearer key for %s (id %s), shown once:\n\n  %s\n\n", s.Name, b.ID, b.String())
		describeKey(a, res)
		for _, c := range Clients {
			if sn, ok := res.Snippets[c]; ok {
				a.say("── %s ──\n%s\n\n", c, sn)
			}
		}
		for _, w := range warnings {
			a.say("warning: %s\n", w)
		}
	})
}

func keyResult(b keys.Bearer) KeyResult {
	d := keys.Describe(b)
	r := KeyResult{Key: b.String(), ID: b.ID, Service: d.Service, Gen: d.Gen, Tools: d.Tools}
	if !d.Expires.IsZero() {
		r.Expires = d.Expires.Unix()
	}
	r.Warnings = []string{}
	return r
}

func describeKey(a *App, r KeyResult) {
	if r.Tools != nil {
		a.say("Tools: %s\n", strings.Join(r.Tools, ", "))
	} else {
		a.say("Tools: every exposed tool\n")
	}
	if r.Expires != 0 {
		a.say("Expires: %s\n\n", time.Unix(r.Expires, 0).Local().Format(time.RFC1123))
	} else {
		a.say("Expires: never (until revoked or rotated)\n\n")
	}
}

func cmdKeyNarrow(a *App, args []string) error {
	fs := a.newFlags("key narrow", "key narrow <key> [--tools a,b] [--for 1h]", `
Restrict an existing bearer key further, offline: the result can do at most what the original can. Useful for
handing a short-lived, single-tool key to someone else.`)
	toolsS := fs.String("tools", "", "comma-separated tools the narrowed key may use")
	forS := fs.String("for", "", "lifetime of the narrowed key")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("key narrow needs the key to narrow")
	}
	b, err := keys.Parse(pos[0])
	if err != nil {
		return wrap("bad_key", "pass a key printed by `toprelay key`", err)
	}
	var tools []string
	if *toolsS != "" {
		tools = splitList(*toolsS)
	}
	var exp time.Time
	if *forS != "" {
		d, err := parseDuration(*forS)
		if err != nil {
			return usageErr("%v", err)
		}
		exp = a.Now().Add(d)
	}
	extra := keys.ExtraCaveats(tools, exp)
	if len(extra) == 0 {
		return usageErr("give --tools and/or --for")
	}
	n := keys.Narrow(b, extra...)
	res := keyResult(n)
	return a.emit(res, func() {
		a.say("Narrowed key (id %s):\n\n  %s\n\n", n.ID, n.String())
		describeKey(a, res)
	})
}

var keyIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

func cmdKeyRevoke(a *App, args []string) error {
	fs := a.newFlags("key revoke", "key revoke <name> <id>", `
Block one bearer key (and every key narrowed from it) by its 16-character id, at once, without a redeploy.`)
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErr("key revoke needs a service name and a key id")
	}
	id := pos[1]
	if strings.HasPrefix(id, "trk1.") {
		b, err := keys.Parse(id)
		if err != nil {
			return wrap("bad_key", "pass the key id or the key", err)
		}
		id = b.ID
	}
	if !keyIDRe.MatchString(id) {
		return usageErr("a key id is 16 lowercase hex characters")
	}
	cfg, s, err := a.loadService(pos[0])
	if err != nil {
		return err
	}
	key, err := a.agentKey(s)
	if err != nil {
		return err
	}
	rc, err := a.relay(cfg)
	if err != nil {
		return err
	}
	n, err := rc.RevokeKey(a.Ctx, s.Name, key, id)
	if err != nil {
		return wrap("relay", "run `toprelay doctor`", err)
	}
	return a.emit(map[string]any{"service": s.Name, "id": id, "revokedKeys": n}, func() {
		a.say("✓ key %s revoked for %s (%d revoked keys on the list)\n", id, s.Name, n)
	})
}

// snippets returns copy-paste configuration per client (one client, or all when client is "").
func snippets(cfg *config.Config, s *config.Service, key, client string) map[string]string {
	u := serviceURL(cfg, s)
	all := map[string]string{}
	if s.Kind == config.KindHTTP {
		all["curl"] = fmt.Sprintf("curl -sS %s -H 'Authorization: Bearer %s'", u, key)
	} else {
		all["claude-api"] = fmt.Sprintf(`curl https://api.anthropic.com/v1/messages \
  -H "x-api-key: $ANTHROPIC_API_KEY" -H "anthropic-version: 2023-06-01" \
  -H "anthropic-beta: mcp-client-2025-11-20" -H "content-type: application/json" \
  -d '{"model":"claude-opus-5-5","max_tokens":1024,
       "mcp_servers":[{"type":"url","url":"%s","name":"%s","authorization_token":"%s"}],
       "tools":[{"type":"mcp_toolset","mcp_server_name":"%s"}],
       "messages":[{"role":"user","content":"Which %s tools can you use?"}]}'`, u, s.Name, key, s.Name, s.Name)
		all["openai"] = fmt.Sprintf(`curl https://api.openai.com/v1/responses \
  -H "Authorization: Bearer $OPENAI_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"<model>","input":"Which %s tools can you use?",
       "tools":[{"type":"mcp","server_label":"%s","server_url":"%s","authorization":"%s","require_approval":"never"}]}'`, s.Name, s.Name, u, key)
		all["xai"] = fmt.Sprintf(`curl https://api.x.ai/v1/responses \
  -H "Authorization: Bearer $XAI_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"<model>","input":"Which %s tools can you use?",
       "tools":[{"type":"mcp","server_label":"%s","server_url":"%s","authorization":"%s"}]}'`, s.Name, s.Name, u, key)
		all["gemini"] = fmt.Sprintf(`Gemini CLI (~/.gemini/settings.json):
{"mcpServers":{"%s":{"httpUrl":"%s","headers":{"Authorization":"Bearer %s"}}}}`, s.Name, u, key)
		if su := sseURL(cfg, s); su != "" {
			all["qwen"] = fmt.Sprintf(`Model Studio MCP server (SSE):
  URL:     %s
  Headers: {"Authorization":"Bearer %s"}`, su, key)
		}
		all["curl"] = fmt.Sprintf(`curl -sS %s \
  -H 'Authorization: Bearer %s' \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}'`, u, key)
	}
	if client == "" {
		return all
	}
	if sn, ok := all[client]; ok {
		return map[string]string{client: sn}
	}
	if client == "qwen" {
		return map[string]string{"qwen": "Qwen / Model Studio needs the legacy SSE transport, which TopRelay serves only for --stdio services."}
	}
	return map[string]string{client: "not applicable to an http service; use curl"}
}
