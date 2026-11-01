package cli

import (
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
	fs := a.newFlags("tools", "tools <name> [--allow a,b] [--deny a,b] [--allow-all] [--reset]", `
Show a service's tools with their status (exposed or hidden) and annotations, and change which are exposed.
A running `+"`toprelay up`"+` picks the change up within seconds and tells the relay.

  --allow a,b   expose these tools
  --deny a,b    hide these tools
  --allow-all   expose every tool, including ones the server adds later
  --reset       back to the default: read-only tools exposed, the rest hidden, new tools hidden`)
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
	hv, err := a.upstreamValue(s)
	if err != nil {
		return err
	}
	res := ToolsResult{Service: s.Name}
	tools, lerr := a.listLocalTools(s, hv)
	if lerr == nil {
		res.Reachable = true
	} else {
		res.Error = lerr.Error()
	}
	p := s.Tools
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
			for _, n := range append(splitList(*allow), splitList(*deny)...) {
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
	return a.emit(res, func() {
		if lerr != nil {
			a.say("warning: can't list tools right now: %v\n", lerr)
		}
		for _, v := range res.Tools {
			status := "hidden "
			if v.Allowed {
				status = "exposed"
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
			a.say("  %s  %-30s %s\n", status, v.Name, strings.Join(hints, ", "))
		}
		def := "hidden"
		if p.Default == config.DefaultAllow {
			def = "exposed"
		}
		a.say("New tools the server adds later: %s\n", def)
		if len(unknown) > 0 {
			a.say("note: the server doesn't list %s right now; the setting is kept for when it does\n", strings.Join(unknown, ", "))
		}
		if res.Changed {
			a.say("✓ saved; a running `toprelay up` applies it within a few seconds\n")
		}
	})
}
