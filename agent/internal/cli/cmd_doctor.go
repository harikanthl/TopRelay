package cli

import (
	"time"

	"github.com/harikanthl/toprelay/agent/internal/doctor"
)

func cmdDoctor(a *App, args []string) error {
	fs := a.newFlags("doctor", "doctor", `
Check everything, in order, each with a fix: config, Cloudflare token and permissions, root secrets, the Worker,
the workers.dev subdomain and /health, each local service (and its tools), the relay's view of each agent, and
clock skew against the relay. Exits non-zero when any check fails.`)
	if _, _, err := parse(fs, args); err != nil {
		return err
	}
	ctx, cancel := a.waitCtx(2 * time.Minute)
	defer cancel()
	rep := doctor.Run(ctx, doctor.Env{Dir: a.Dir, Store: a.Store, CFBase: a.CFBase, RelayURL: a.RelayURL, Now: a.Now})
	err := a.emit(rep, func() {
		for _, c := range rep.Checks {
			mark := map[string]string{doctor.OK: "✓", doctor.Warn: "!", doctor.Fail: "✗", doctor.Skip: "-"}[c.Status]
			a.say("%s %-28s %s\n", mark, c.Name, c.Detail)
			if c.Fix != "" && c.Status != doctor.OK {
				a.say("    fix: %s\n", c.Fix)
			}
		}
		if rep.OK {
			a.say("\nAll good.\n")
		}
	})
	if err != nil {
		return err
	}
	if !rep.OK {
		// The report is already printed; exit non-zero without a second error object in --json mode.
		return errSilent
	}
	return nil
}
