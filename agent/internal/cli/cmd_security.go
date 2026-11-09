package cli

import (
	"crypto/ed25519"
	"encoding/base64"
	"slices"
	"strings"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/calllog"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/keys"
	"github.com/harikanthl/toprelay/agent/internal/pins"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/shield"
)

// HeldView is a tool held for review: its approved definition and the one the server lists now.
type HeldView struct {
	Tool string `json:"tool"`
	// ApprovedSHA256 and NowSHA256 fingerprint the whole definition (description, schemas, annotations).
	ApprovedSHA256 string    `json:"approvedSha256"`
	NowSHA256      string    `json:"nowSha256"`
	Before         string    `json:"before"`
	After          string    `json:"after"`
	SeenAt         time.Time `json:"seenAt"`
	// DescriptionChanged is false when only the schemas or annotations changed.
	DescriptionChanged bool             `json:"descriptionChanged"`
	Findings           []shield.Finding `json:"findings,omitempty"`
}

// PinsResult is the --json output of pins.
type PinsResult struct {
	Service   string     `json:"service"`
	Pinned    int        `json:"pinned"`
	Held      []HeldView `json:"held"`
	Approved  []string   `json:"approved,omitempty"`
	Forgotten []string   `json:"forgotten,omitempty"`
}

func cmdPins(a *App, args []string) error {
	fs := a.newFlags("pins", "pins <name> [--approve a,b | --approve-all] [--forget a,b | --forget-all]", `
TopRelay pins every MCP tool's definition (description, input and output schemas, annotations) the first time it
sees it. If the server later changes a definition, the tool is held: bots don't see it and calls to it are
refused (reason tool_changed in the logs), until you review the change here. This stops a server from changing
what a tool says or does after you approved it (a "rug pull").

  --approve a,b   accept the new definitions of these held tools
  --approve-all   accept every held tool
  --forget a,b    drop these pins: the tools are pinned again, as they are, the next time they're listed
  --forget-all    drop every pin

With no flags, shows what is held and how it changed. A running `+"`toprelay up`"+` applies approvals within
seconds.`)
	approve := fs.String("approve", "", "comma-separated held tools to approve")
	approveAll := fs.Bool("approve-all", false, "approve every held tool")
	forget := fs.String("forget", "", "comma-separated tools to unpin")
	forgetAll := fs.Bool("forget-all", false, "unpin every tool")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("pins needs a service name")
	}
	if (*approve != "" && *approveAll) || (*forget != "" && *forgetAll) {
		return usageErr("use either a list or --…-all, not both")
	}
	_, s, err := a.loadService(pos[0])
	if err != nil {
		return err
	}
	if s.Kind != config.KindMCP {
		return errf("not_mcp", "", "%s is an http service: it has no tools to pin", s.Name)
	}
	st, err := pins.Open(pins.Path(a.Dir, s.Name))
	if err != nil {
		return wrap("pins", "delete "+pins.Path(a.Dir, s.Name)+" to pin every tool again as it is now", err)
	}
	res := PinsResult{Service: s.Name, Held: []HeldView{}}
	if *approve != "" || *approveAll {
		want := splitList(*approve)
		for _, n := range want {
			if !st.IsHeld(n) {
				return errf("not_held", "run `toprelay pins "+s.Name+"` to see the held tools", "%s is not held", n)
			}
		}
		if res.Approved, err = st.Approve(want); err != nil {
			return wrap("pins", "check permissions on "+a.Dir, err)
		}
	}
	if *forget != "" || *forgetAll {
		if res.Forgotten, err = st.Forget(splitList(*forget)); err != nil {
			return wrap("pins", "check permissions on "+a.Dir, err)
		}
	}
	snap := st.Snapshot()
	res.Pinned = len(snap.Tools)
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range snap.Held {
			if !yield(n) {
				return
			}
		}
	}) {
		c, p := snap.Held[name], snap.Tools[name]
		v := HeldView{Tool: name, ApprovedSHA256: p.SHA256, NowSHA256: c.SHA256, Before: p.Description, After: c.Description,
			SeenAt: c.SeenAt, DescriptionChanged: p.Description != c.Description}
		if s.ShieldConfig().InjectionMode() != shield.InjectionOff {
			v.Findings = shield.ScanText(c.Description)
		}
		res.Held = append(res.Held, v)
	}
	return a.emit(res, func() {
		for _, n := range res.Approved {
			a.say("✓ approved the new definition of %s\n", n)
		}
		for _, n := range res.Forgotten {
			a.say("✓ unpinned %s\n", n)
		}
		if len(res.Held) == 0 {
			a.say("%s: %d tools pinned, none changed since approval.\n", s.Name, res.Pinned)
			return
		}
		a.say("%s: %d tools pinned; %d changed since approval and are held (hidden from bots, calls refused):\n\n", s.Name, res.Pinned, len(res.Held))
		for _, h := range res.Held {
			a.say("  %s  (changed %s)\n", h.Tool, h.SeenAt.Local().Format("2006-01-02 15:04"))
			if h.DescriptionChanged {
				for _, l := range strings.Split(h.Before, "\n") {
					a.say("    - %s\n", l)
				}
				for _, l := range strings.Split(h.After, "\n") {
					a.say("    + %s\n", l)
				}
			} else {
				a.say("    description unchanged; its input/output schema or annotations changed\n")
			}
			for _, f := range h.Findings {
				a.say("    ⚠ %s: %s\n", f.Rule, f.Excerpt)
			}
			a.say("\n")
		}
		a.say("Approve with: toprelay pins %s --approve %s   (or --approve-all)\n", s.Name, res.Held[0].Tool)
	})
}

// ShieldResult is the --json output of shield.
type ShieldResult struct {
	Service   string `json:"service"`
	Leaks     string `json:"leaks"`
	Injection string `json:"injection"`
	Changed   bool   `json:"changed"`
	// LeakRules and InjectionRules are the built-in rule names.
	LeakRules      []string `json:"leakRules"`
	InjectionRules []string `json:"injectionRules"`
	// Flagged are listed tools whose descriptions contain instruction-like text (when the server answered).
	Flagged   []FlaggedTool `json:"flagged"`
	Reachable bool          `json:"reachable"`
	Error     string        `json:"error,omitempty"`
}

// FlaggedTool is a tool whose description the injection scan flagged.
type FlaggedTool struct {
	Tool     string           `json:"tool"`
	Findings []shield.Finding `json:"findings"`
}

func cmdShield(a *App, args []string) error {
	fs := a.newFlags("shield", "shield <name> [--leaks redact|block|off] [--injection warn|off]", `
The shield inspects every answer from the app on this computer, before it reaches the relay or any bot.

  --leaks redact      replace credentials (API keys, tokens, private keys, passwords in URLs, and every secret
                      TopRelay itself holds, such as the app's own credential) with [REDACTED:<kind>] (default)
  --leaks block       withhold the whole answer instead, with an error the model can relay
  --leaks off         don't look
  --injection warn    flag text that tries to instruct the model (hidden Unicode, "ignore previous
                      instructions", requests for ~/.ssh, …) in the logs; the answer is unchanged (default)
  --injection off     don't look

With no flags, shows the settings and scans the current tool descriptions. Findings appear in `+"`toprelay logs`"+`
and as counters in `+"`toprelay status`"+`. A running `+"`toprelay up`"+` applies changes live.`)
	leaks := fs.String("leaks", "", "redact, block or off")
	injection := fs.String("injection", "", "warn or off")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("shield needs a service name")
	}
	cfg, s, err := a.loadService(pos[0])
	if err != nil {
		return err
	}
	if s.Kind != config.KindMCP {
		return errf("not_mcp", "", "%s is an http service: the shield inspects MCP answers", s.Name)
	}
	sc := s.ShieldConfig()
	res := ShieldResult{Service: s.Name, LeakRules: shield.LeakRules(), InjectionRules: shield.InjectionRules(), Flagged: []FlaggedTool{}}
	if *leaks != "" || *injection != "" {
		if *leaks != "" {
			sc.Leaks = *leaks
		}
		if *injection != "" {
			sc.Injection = *injection
		}
		if err := sc.Validate(); err != nil {
			return usageErr("%v", err)
		}
		// Defaults are stored as empty, so a default config leaves no trace in config.json.
		if sc.Leaks == shield.LeaksRedact {
			sc.Leaks = ""
		}
		if sc.Injection == shield.InjectionWarn {
			sc.Injection = ""
		}
		if sc == (shield.Config{}) {
			s.Shield = nil
		} else {
			s.Shield = &sc
		}
		if err := config.Save(a.Dir, cfg); err != nil {
			return wrap("config", "check permissions on "+a.Dir, err)
		}
		res.Changed = true
	}
	res.Leaks, res.Injection = sc.LeakMode(), sc.InjectionMode()
	up, err := a.upstreamFor(s)
	if err != nil {
		return err
	}
	env, err := a.stdioEnv(s)
	if err != nil {
		return err
	}
	if tools, lerr := a.listLocalTools(s, up, env); lerr != nil {
		res.Error = lerr.Error()
	} else {
		res.Reachable = true
		for _, t := range tools {
			if f := shield.ScanText(t.Description); len(f) > 0 {
				res.Flagged = append(res.Flagged, FlaggedTool{Tool: t.Name, Findings: f})
			}
		}
	}
	return a.emit(res, func() {
		a.say("%s shield: credentials %s, instruction-like text %s\n", s.Name,
			map[string]string{shield.LeaksRedact: "redacted", shield.LeaksBlock: "withheld (whole answer)", shield.LeaksOff: "not checked"}[res.Leaks],
			map[string]string{shield.InjectionWarn: "flagged in the logs", shield.InjectionOff: "not checked"}[res.Injection])
		switch {
		case !res.Reachable:
			a.say("warning: can't scan tool descriptions right now: %s\n", res.Error)
		case len(res.Flagged) == 0:
			a.say("Tool descriptions: nothing suspicious.\n")
		default:
			for _, f := range res.Flagged {
				for _, x := range f.Findings {
					a.say("  ⚠ %s: %s: %s\n", f.Tool, x.Rule, x.Excerpt)
				}
			}
			a.say("Hide a tool with: toprelay tools %s --deny %s\n", s.Name, res.Flagged[0].Tool)
		}
		if res.Changed {
			a.say("✓ saved; a running `toprelay up` applies it within a few seconds\n")
		}
	})
}

// AuditKeyResult is the --json output of `audit key`.
type AuditKeyResult struct {
	PublicKey string `json:"publicKey"`
	KeyID     string `json:"keyId"`
}

func cmdAudit(a *App, args []string) error {
	fs := a.newFlags("audit", "audit verify <name> [--key PUBLIC_KEY] | audit key", `
Every call-log line carries a sequence number and the SHA-256 of the line before it, and checkpoint lines sign
the chain with an Ed25519 key derived from this agent's root secret. Editing, deleting, inserting or reordering
lines breaks the chain; only retention may drop the oldest lines, and it records that in the chain.

  audit verify <name>   check a service's call log (exit status 1 if it doesn't check out)
  --key PUBLIC_KEY      verify with an exported public key instead of this agent's (on another computer)
  audit key             print the public key, to give to whoever verifies the logs

Lines written after the last checkpoint are chained but not yet signed; a checkpoint is written every 100 lines,
every 5 minutes of activity, and when `+"`toprelay up`"+` stops.`)
	keyFlag := fs.String("key", "", "base64url Ed25519 public key (from `toprelay audit key`)")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageErr("audit needs verify <name> or key")
	}
	ownKey := func() (ed25519.PublicKey, error) {
		root, err := secrets.Require(a.Store, secrets.AgentRoot, "run `toprelay add` to create it")
		if err != nil {
			return nil, wrap("keyring", "", err)
		}
		k, err := keys.AuditKey(root)
		if err != nil {
			return nil, wrap("keyring", "", err)
		}
		return k.Public().(ed25519.PublicKey), nil
	}
	switch pos[0] {
	case "key":
		if len(pos) != 1 {
			return usageErr("audit key takes no arguments")
		}
		pub, err := ownKey()
		if err != nil {
			return err
		}
		res := AuditKeyResult{PublicKey: base64.RawURLEncoding.EncodeToString(pub), KeyID: calllog.KeyID(pub)}
		return a.emit(res, func() {
			a.say("%s\n", res.PublicKey)
			a.note("key id %s; verify elsewhere with: toprelay audit verify <name> --key %s\n", res.KeyID, res.PublicKey)
		})
	case "verify":
		if len(pos) != 2 {
			return usageErr("audit verify needs one service name")
		}
		name := pos[1]
		if _, _, err := a.loadService(name); err != nil {
			return err
		}
		var pub ed25519.PublicKey
		if *keyFlag != "" {
			b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(*keyFlag, "="))
			if err != nil || len(b) != ed25519.PublicKeySize {
				return usageErr("--key is not a base64url Ed25519 public key")
			}
			pub = b
		} else if pub, err = ownKey(); err != nil {
			return err
		}
		path := calllog.Path(a.Dir, name)
		rep, err := calllog.Verify(path, name, pub)
		if err != nil {
			return wrap("logs", "check permissions on "+path, err)
		}
		if err := a.emit(rep, func() {
			if rep.LastSeq == 0 && rep.Legacy == 0 {
				a.say("%s: the call log is empty.\n", name)
				return
			}
			if rep.OK {
				a.say("✓ %s: call log intact: lines %d–%d, %d calls, %d signed checkpoints\n", name, rep.FirstSeq, rep.LastSeq, rep.Calls, rep.Checkpoints)
			} else {
				a.say("✗ %s: the call log does NOT check out (%d problems):\n", name, len(rep.Problems))
				for _, p := range rep.Problems {
					a.say("  %s line %d (seq %d): %s\n", p.File, p.Line, p.Seq, p.What)
				}
			}
			if rep.FirstSeq > 1 {
				a.say("  lines before %d were rotated out or removed by retention (%d records in %d recorded prunes)\n", rep.FirstSeq, rep.Pruned, rep.Prunes)
			}
			if rep.Unsigned > 0 {
				a.say("  the last %d lines are chained but not signed yet (next checkpoint signs them)\n", rep.Unsigned)
			}
			if rep.Legacy > 0 {
				a.say("  %d older lines predate the chain and can't be checked\n", rep.Legacy)
			}
		}); err != nil {
			return err
		}
		if !rep.OK {
			// The report is already printed; exit non-zero without a second error object in --json mode.
			return errSilent
		}
		return nil
	default:
		return usageErr("unknown audit command %q (verify or key)", pos[0])
	}
}
