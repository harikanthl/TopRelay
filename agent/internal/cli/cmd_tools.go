package cli

import (
	"maps"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/harikanthl/toprelay/agent/internal/config"
)

// ToolsResult is the --json output of tools.
type ToolsResult struct {
	Service   string            `json:"service"`
	Policy    config.ToolPolicy `json:"policy"`
	Reachable bool              `json:"reachable"`
	Error     string            `json:"error,omitempty"`
	Tools     []ToolView        `json:"tools"`
	Changed   bool              `json:"changed"`
}

func cmdTools(a *App, args []string) error {
	fs := a.newFlags("tools", "tools <name> [--client SELECTOR] [--allow a,b] [--deny a,b] [--allow-all] [--reset]", `
Show a service's tools with their status (exposed or hidden) and annotations, and change which are exposed.
A running `+"`toprelay up`"+` picks the change up within seconds and tells the relay.

  --allow a,b   expose these tools
  --deny a,b    hide these tools
  --allow-all   expose every tool, including ones the server adds later
  --reset       back to the default: tools whose MCP annotations say read-only are exposed (now and later);
                destructive ones, and ones without annotations, only if you --allow them

  --client SELECTOR   apply --allow/--deny/--reset to one client instead, on top of the service's policy:
                      a domain (claude.ai, chatgpt.com, x.ai) matching OAuth client ids on that host,
                      key:<id> for a bearer key, an exact client id, or a prefix ending in *.
                      The client ids that called are in `+"`toprelay logs`"+`.`)
	client := fs.String("client", "", "change the rule for one client (domain, key:<id>, client id, or prefix*)")
	allow := fs.String("allow", "", "comma-separated tools to expose")
	deny := fs.String("deny", "", "comma-separated tools to hide")
	all := fs.Bool("allow-all", false, "expose every tool, present and future")
	reset := fs.Bool("reset", false, "reset to the default policy")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("tools needs a service name")
	}
	cfg, s, err := a.loadService(pos[0])
	if err != nil {
		return err
	}
	if s.Kind != config.KindMCP {
		return errf("not_mcp", "use --allow-path on `add` for http services", "%s is an http service: it has no tools", s.Name)
	}
	up, err := a.upstreamFor(s)
	if err != nil {
		return err
	}
	env, err := a.stdioEnv(s)
	if err != nil {
		return err
	}
	res := ToolsResult{Service: s.Name}
	tools, lerr := a.listLocalTools(s, up, env)
	if lerr == nil {
		res.Reachable = true
	} else {
		res.Error = lerr.Error()
	}
	p := s.Tools
	var clientNames []string
	if *client != "" {
		if *all {
			return usageErr("--allow-all is for the whole service; for a client, --allow the tools")
		}
		if strings.ContainsAny(*client, " \t,") {
			return usageErr("bad --client selector %q", *client)
		}
		r := p.Clients[*client]
		if *reset {
			r = config.ClientRule{}
		}
		for _, t := range splitList(*allow) {
			r.Deny = slices.DeleteFunc(r.Deny, func(x string) bool { return x == t })
			if !slices.Contains(r.Allow, t) {
				r.Allow = append(r.Allow, t)
			}
		}
		for _, t := range splitList(*deny) {
			r.Allow = slices.DeleteFunc(r.Allow, func(x string) bool { return x == t })
			if !slices.Contains(r.Deny, t) {
				r.Deny = append(r.Deny, t)
			}
		}
		if p.Clients == nil {
			p.Clients = map[string]config.ClientRule{}
		} else {
			p.Clients = maps.Clone(p.Clients)
		}
		p.Clients[*client] = r
		res.Changed = *reset || *allow != "" || *deny != ""
		clientNames = append(splitList(*allow), splitList(*deny)...)
		*reset, *allow, *deny = false, "", "" // applied to the client, not the service
		if !res.Changed {
			return usageErr("--client needs --allow, --deny or --reset")
		}
	}
	if *reset {
		if lerr != nil {
			return wrap("local_unreachable", "start the local server so its read-only tools can be found, then rerun --reset", lerr)
		}
		p = defaultPolicy(tools)
		res.Changed = true
	}
	if *all {
		p.Default, p.Deny = config.DefaultAllow, []string{}
		res.Changed = true
	}
	if *allow != "" {
		p.SetAllowed(splitList(*allow)...)
		res.Changed = true
	}
	if *deny != "" {
		p.SetDenied(splitList(*deny)...)
		res.Changed = true
	}
	p.Normalize()
	var unknown []string
	if res.Changed {
		if lerr == nil {
			known := map[string]bool{}
			for _, t := range tools {
				known[t.Name] = true
			}
			for _, n := range append(append(splitList(*allow), splitList(*deny)...), clientNames...) {
				if !known[n] {
					unknown = append(unknown, n)
				}
			}
		}
		s.Tools = p
		if err := config.Save(a.Dir, cfg); err != nil {
			return wrap("config", "check permissions on "+a.Dir, err)
		}
	}
	res.Policy = p
	if tools == nil {
		tools = []*mcp.Tool{}
	}
	res.Tools = toolViews(s.Name, tools, p)
	if lerr == nil {
		if res.Tools, err = a.reviewTools(s, tools, res.Tools, false); err != nil {
			return err
		}
	}
	return a.emit(res, func() {
		if lerr != nil {
			a.say("warning: can't list tools right now: %v\n", lerr)
		}
		for _, v := range res.Tools {
			status := "hidden "
			if v.Allowed {
				status = "exposed"
			}
			if v.Held {
				status = "HELD   "
			}
			var hints []string
			if v.ReadOnly {
				hints = append(hints, "read-only")
			} else if v.Destructive {
				hints = append(hints, "destructive")
			}
			if v.Idempotent {
				hints = append(hints, "idempotent")
			}
			if !v.OpenWorld {
				hints = append(hints, "closed-world")
			}
			if len(v.Findings) > 0 {
				hints = append(hints, "⚠ suspicious description")
			}
			a.say("  %s  %-30s %s\n", status, v.Name, strings.Join(hints, ", "))
		}
		for _, v := range res.Tools {
			for _, w := range v.Warnings {
				if v.Held || len(v.Findings) > 0 {
					a.say("warning: %s\n", w)
				}
			}
		}
		def := "hidden"
		switch p.Default {
		case config.DefaultAllow:
			def = "exposed"
		case config.DefaultReadOnly:
			def = "exposed if read-only, hidden otherwise"
		}
		a.say("New tools the server adds later: %s\n", def)
		for sel, r := range p.Clients {
			a.say("Client %s: also exposed %s; hidden %s\n", sel, orNone(r.Allow), orNone(r.Deny))
		}
		if len(unknown) > 0 {
			a.say("note: the server doesn't list %s right now; the setting is kept for when it does\n", strings.Join(unknown, ", "))
		}
		if res.Changed {
			a.say("✓ saved; a running `toprelay up` applies it within a few seconds\n")
		}
	})
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}
