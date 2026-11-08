package cli

import (
	"maps"
	"strings"

	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/limits"
)

const limitsHelp = `
Set a service's own rate limits, enforced on this computer before a call reaches the app. Rates are N/s, N/m,
N/h or N/d; a limit allows bursts of up to N. Over a limit the bot gets HTTP 429 with Retry-After (and a
JSON-RPC error saying when to retry), and the call is logged with reason rate_limited.

  --service RATE      all calls to the service together
  --client RATE       each client separately (each OAuth client, each bearer key)
  --tool NAME=RATE    one tool, all clients together (repeatable)
  --clear             remove every limit
  Use "off" as a RATE to remove that one limit. With no flags, shows the limits.

The relay also limits every service to 20 requests/s (burst 60). A running ` + "`toprelay up`" + ` applies changes
live. ` + "`toprelay status`" + ` shows how many calls each service refused this way.

Example: toprelay limits notes --client 60/m --tool render=10/m`

func cmdLimits(a *App, args []string) error {
	fs := a.newFlags("limits", "limits <name> [--service RATE] [--client RATE] [--tool NAME=RATE]… [--clear]", limitsHelp)
	svcRate := fs.String("service", "", "limit for the whole service")
	clientRate := fs.String("client", "", "limit for each client")
	var tools multi
	fs.Var(&tools, "tool", "NAME=RATE limit for one tool (repeatable)")
	clear := fs.Bool("clear", false, "remove every limit")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("limits needs a service name")
	}
	cfg, s, err := a.loadService(pos[0])
	if err != nil {
		return err
	}
	l := config.Limits{}
	if s.Limits != nil {
		l = *s.Limits
		l.Tools = maps.Clone(l.Tools)
	}
	changed := *clear || *svcRate != "" || *clientRate != "" || len(tools) > 0
	if *clear {
		l = config.Limits{}
	}
	set := func(dst *string, v string) {
		if v == "off" {
			*dst = ""
		} else if v != "" {
			*dst = v
		}
	}
	set(&l.Service, *svcRate)
	set(&l.Client, *clientRate)
	for _, tv := range tools {
		name, rate, ok := strings.Cut(tv, "=")
		if !ok || name == "" || rate == "" {
			return usageErr("--tool wants NAME=RATE, e.g. render=10/m")
		}
		if rate == "off" {
			delete(l.Tools, name)
			continue
		}
		if l.Tools == nil {
			l.Tools = map[string]string{}
		}
		l.Tools[name] = rate
	}
	if len(l.Tools) == 0 {
		l.Tools = nil
	}
	if err := limits.Validate(l); err != nil {
		return usageErr("%v", err)
	}
	if changed {
		if l.Empty() {
			s.Limits = nil
		} else {
			s.Limits = &l
		}
		if err := config.Save(a.Dir, cfg); err != nil {
			return wrap("config", "check permissions on "+a.Dir, err)
		}
	}
	out := map[string]any{"service": s.Name, "limits": l, "changed": changed}
	return a.emit(out, func() {
		if l.Empty() {
			a.say("%s has no limits of its own (the relay's 20/s per service still applies)\n", s.Name)
			return
		}
		if l.Service != "" {
			a.say("service  %s\n", l.Service)
		}
		if l.Client != "" {
			a.say("client   %s each\n", l.Client)
		}
		for t, r := range l.Tools {
			a.say("tool     %s %s\n", t, r)
		}
		if changed {
			a.say("✓ saved; a running `toprelay up` applies it now\n")
		}
	})
}
