package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"log/slog"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/caller"
	"github.com/harikanthl/toprelay/agent/internal/calllog"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/daemon"
	"github.com/harikanthl/toprelay/agent/internal/files"
	"github.com/harikanthl/toprelay/agent/internal/keys"
	"github.com/harikanthl/toprelay/agent/internal/pins"
	"github.com/harikanthl/toprelay/agent/internal/relayapi"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/service"
)

// FreeRequestsPerDay is the Workers Free request allowance.
const FreeRequestsPerDay = 100000

type installer interface {
	Install() (string, error)
	Uninstall() (string, error)
}

func (a *App) installer() installer {
	if a.Installer != nil {
		return a.Installer()
	}
	exe := a.Exe
	if exe == "" {
		if e, err := os.Executable(); err == nil {
			if r, err := filepath.EvalSymlinks(e); err == nil {
				e = r
			}
			exe = e
		}
	}
	home := a.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	env := map[string]string{"TOPRELAY_CONFIG_DIR": a.Dir}
	for _, k := range []string{"TOPRELAY_KEYRING", "TOPRELAY_RELAY_URL"} {
		if v := a.Getenv(k); v != "" {
			env[k] = v
		}
	}
	env["PATH"] = daemon.ServicePath(a.Getenv("PATH"), home)
	return daemon.NewInstaller(home, exe, filepath.Join(a.Dir, "logs"), env)
}

func cmdUp(a *App, args []string) error {
	fs := a.newFlags("up", "up [--install | --uninstall] [--verbose]", `
Keep every service connected to the relay: one tunnel per service, reconnecting after sleep, network changes and
redeploys. Edits from `+"`toprelay add/tools/rotate/remove`"+` are picked up live (or send SIGHUP).
Live status for other programs is written to <config dir>/state.json, and apps can drive TopRelay through the
control socket <config dir>/agent.sock (TopRelayKit; see the README). Only one `+"`up`"+` runs per config dir.

  --install    run `+"`toprelay up`"+` at login: a launchd agent on macOS (~/Library/LaunchAgents/dev.toprelay.agent.plist),
               a systemd user unit on Linux; logs go to <config dir>/logs/agent.log
  --uninstall  remove it`)
	install := fs.Bool("install", false, "install and start the login service")
	uninstall := fs.Bool("uninstall", false, "stop and remove the login service")
	verbose := fs.Bool("verbose", false, "debug logging")
	if _, _, err := parse(fs, args); err != nil {
		return err
	}
	if *install && *uninstall {
		return usageErr("--install and --uninstall are exclusive")
	}
	if *install || *uninstall {
		in := a.installer()
		var p string
		var err error
		if *install {
			if _, cerr := a.loadConfig(); cerr != nil {
				return cerr
			}
			p, err = in.Install()
		} else {
			p, err = in.Uninstall()
		}
		if err != nil {
			return wrap("install", "check the message above; you can always run `toprelay up` in a terminal instead", err)
		}
		return a.emit(map[string]any{"installed": *install, "path": p}, func() {
			if *install {
				a.say("✓ installed %s; toprelay up now runs at login (logs: %s)\n", p, filepath.Join(a.Dir, "logs", "agent.log"))
			} else {
				a.say("✓ removed %s\n", p)
			}
		})
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if _, err := a.relayBase(cfg); err != nil && len(cfg.Services) > 0 {
		return err
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logOut := a.Stderr
	if a.JSON {
		// An app that runs `up --json` as a child gets nothing on stderr to drain; the log goes to a file.
		lf, err := openAgentLog(a.Dir)
		if err != nil {
			return wrap("log", "check permissions on "+a.Dir, err)
		}
		defer lf.Close()
		logOut = lf
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(a.Ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	reload := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				select {
				case reload <- struct{}{}:
				default:
				}
			}
		}
	}()
	state := daemon.NewStateFile(daemon.StatePath(a.Dir), nil)
	reg := files.NewRegistry()
	d := &daemon.Daemon{Dir: a.Dir, Store: a.Store, RelayURL: a.RelayURL, Log: log, Reload: reload, State: state, Files: reg}
	l, err := listenControl(a.Dir)
	switch {
	case errors.Is(err, ErrAgentRunning):
		return errf("already_running", "stop the other one first (`toprelay up --uninstall` if it was installed), or use it", "%v", err)
	case err != nil:
		log.Warn("no control socket (TopRelayKit can't connect); everything else works", "path", ControlPath(a.Dir), "err", err)
	default:
		defer os.Remove(ControlPath(a.Dir))
		defer l.Close()
		go (&control{a: a, state: state, files: reg, log: log}).serve(ctx, l)
	}
	log.Info("toprelay up", "version", Version, "services", len(cfg.Services), "state", daemon.StatePath(a.Dir), "control", ControlPath(a.Dir))
	if err := d.Run(ctx); err != nil {
		return wrap("up", "run `toprelay doctor`", err)
	}
	return nil
}

// openAgentLog opens <dir>/logs/agent.log for appending (0600), the same file `up --install` logs to.
func openAgentLog(dir string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, "logs", "agent.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// StatusService is one service in status --json.
type StatusService struct {
	Name        string               `json:"name"`
	Kind        string               `json:"kind"`
	URL         string               `json:"url"`
	Connected   bool                 `json:"connected"`
	ConnectedAt int64                `json:"connectedAt,omitempty"`
	Agent       string               `json:"agent,omitempty"`
	Requests    int64                `json:"requests"`
	Rejected    int64                `json:"rejected"`
	BytesIn     int64                `json:"bytesIn"`
	BytesOut    int64                `json:"bytesOut"`
	RevokedKeys int                  `json:"revokedKeys"`
	Error       string               `json:"error,omitempty"`
	Local       *daemon.ServiceState `json:"local,omitempty"`
}

// StatusResult is status --json.
type StatusResult struct {
	Day            string          `json:"day"`
	RequestsToday  int64           `json:"requestsToday"`
	FreeLimit      int64           `json:"freeLimit"`
	PercentOfLimit float64         `json:"percentOfLimit"`
	Daemon         *daemon.State   `json:"daemon,omitempty"`
	Services       []StatusService `json:"services"`
}

func cmdStatus(a *App, args []string) error {
	fs := a.newFlags("status", "status [--watch]", `
For each service: whether the relay has this computer's connection, and today's requests, rejected requests and
bytes, against the Workers Free allowance of 100,000 requests a day (counted per account, all services together).

--watch follows the local agent instead (no relay calls): it prints the current state.json at once, then again
every time it changes, until interrupted. With --json each one is a single line: {"version","pid","startedAt",
"updatedAt","running","services":{name:{"connected","since","lastError","reconnectAttempt","nextRetryMs",
"requests","refused"}}}. "running" is false when no `+"`toprelay up`"+` is running (also after a crash).`)
	watch := fs.Bool("watch", false, "print the local state on every change (one JSON line each with --json)")
	if _, _, err := parse(fs, args); err != nil {
		return err
	}
	if *watch {
		return a.watchStatus(250 * time.Millisecond)
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	res := StatusResult{FreeLimit: FreeRequestsPerDay, Services: []StatusService{}, Day: a.Now().UTC().Format("2006-01-02")}
	if st, err := daemon.ReadState(daemon.StatePath(a.Dir)); err == nil {
		res.Daemon = st
	}
	var rc *relayapi.Client
	if len(cfg.Services) > 0 {
		if rc, err = a.relay(cfg); err != nil {
			return err
		}
	}
	for i := range cfg.Services {
		s := &cfg.Services[i]
		ss := StatusService{Name: s.Name, Kind: s.Kind, URL: serviceURL(cfg, s)}
		if res.Daemon != nil && res.Daemon.Running {
			ss.Local = res.Daemon.Services[s.Name]
		}
		key, err := a.agentKey(s)
		if err != nil {
			return err
		}
		st, err := rc.Status(a.Ctx, s.Name, key)
		if err != nil {
			ss.Error = err.Error()
		} else {
			ss.Connected, ss.ConnectedAt, ss.Agent = st.Connected, st.ConnectedAt, st.Agent
			ss.Requests, ss.Rejected, ss.BytesIn, ss.BytesOut, ss.RevokedKeys = st.Requests, st.Rejected, st.BytesIn, st.BytesOut, st.RevokedKeys
			res.RequestsToday += st.Requests
			if st.Day != "" {
				res.Day = st.Day
			}
		}
		res.Services = append(res.Services, ss)
	}
	res.PercentOfLimit = float64(int(float64(res.RequestsToday)/FreeRequestsPerDay*10000)) / 100
	return a.emit(res, func() {
		if len(res.Services) == 0 {
			a.say("No services yet: toprelay add <name> …\n")
			return
		}
		for _, s := range res.Services {
			conn := "offline"
			if s.Connected {
				conn = "connected"
			}
			if s.Error != "" {
				a.say("%-12s error: %s\n", s.Name, s.Error)
				continue
			}
			a.say("%-12s %-9s  today: %d requests, %d rejected, %s in, %s out\n", s.Name, conn, s.Requests, s.Rejected, bytesHuman(s.BytesIn), bytesHuman(s.BytesOut))
			if s.Local != nil && (s.Local.Refused > 0 || s.Local.Limited > 0) {
				a.say("             this session: %d calls, %d refused by the tool policy, %d over your rate limits\n", s.Local.Requests, s.Local.Refused, s.Local.Limited)
			}
			if !s.Connected {
				a.say("             fix: run `toprelay up` on the computer with this service\n")
			}
		}
		a.say("\nAccount total %s: %d of %d free requests (%.1f%%)\n", res.Day, res.RequestsToday, FreeRequestsPerDay, res.PercentOfLimit)
		if res.PercentOfLimit >= 80 {
			a.say("warning: close to the Workers Free daily limit; requests fail once it's reached until 00:00 UTC\n")
		}
	})
}

// watchStatus prints state.json whenever its content changes (updatedAt alone doesn't count), and once at start.
func (a *App) watchStatus(poll time.Duration) error {
	ctx, stop := signal.NotifyContext(a.Ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	path := daemon.StatePath(a.Dir)
	last := ""
	for {
		st, err := daemon.ReadState(path)
		if err != nil {
			st = &daemon.State{Version: daemon.StateVersion, Services: map[string]*daemon.ServiceState{}}
		}
		key := *st
		key.UpdatedAt = time.Time{}
		kb, _ := json.Marshal(key)
		if string(kb) != last {
			last = string(kb)
			if a.JSON {
				b, _ := json.Marshal(st)
				fmt.Fprintf(a.Stdout, "%s\n", b)
			} else {
				a.printWatch(st)
			}
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil // an interrupt or --timeout ends a watch normally
		case <-t.C:
		}
	}
}

func (a *App) printWatch(st *daemon.State) {
	now := a.Now().Format("15:04:05")
	if !st.Running {
		fmt.Fprintf(a.Stdout, "%s  toprelay up is not running\n", now)
		return
	}
	names := make([]string, 0, len(st.Services))
	for n := range st.Services {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		s := st.Services[n]
		conn := "offline"
		if s.Connected {
			conn = "connected"
		}
		line := fmt.Sprintf("%s  %-12s %-9s %d requests, %d refused", now, n, conn, s.Requests, s.Refused)
		if s.LastError != "" && !s.Connected {
			line += "  (" + s.LastError + ")"
		}
		fmt.Fprintln(a.Stdout, line)
	}
}

func bytesHuman(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ListService is one service in list --json.
type ListService struct {
	Name           string   `json:"name"`
	Kind           string   `json:"kind"`
	Source         string   `json:"source"`
	Local          string   `json:"local"`
	URL            string   `json:"url"`
	SSEURL         string   `json:"sseUrl,omitempty"`
	Gen            int      `json:"gen"`
	AllowedTools   []string `json:"allowedTools"`
	DeniedTools    []string `json:"deniedTools"`
	NewTools       string   `json:"newTools"`
	AllowPaths     []string `json:"allowPaths,omitempty"`
	UpstreamHeader string   `json:"upstreamHeader,omitempty"`
	// UpstreamHeaderSource is "keyring" or "file" when UpstreamHeader is set.
	UpstreamHeaderSource string      `json:"upstreamHeaderSource,omitempty"`
	UpstreamHeaderFile   string      `json:"upstreamHeaderFile,omitempty"`
	CallerKeyFile        string      `json:"callerKeyFile,omitempty"`
	Pin                  *config.Pin `json:"pin,omitempty"`
	FilesDir             string      `json:"filesDir,omitempty"`
}

func cmdList(a *App, args []string) error {
	fs := a.newFlags("list", "list", "List the configured services, where they point locally, and their public URLs.")
	if _, _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	out := struct {
		AccountID string        `json:"accountId"`
		PublicURL string        `json:"publicUrl"`
		Services  []ListService `json:"services"`
	}{cfg.AccountID, cfg.PublicURL, []ListService{}}
	for i := range cfg.Services {
		s := &cfg.Services[i]
		out.Services = append(out.Services, ListService{Name: s.Name, Kind: s.Kind, Source: s.Source.Type, Local: s.LocalTarget(),
			URL: serviceURL(cfg, s), SSEURL: sseURL(cfg, s), Gen: s.Gen, AllowedTools: s.Tools.Allow, DeniedTools: s.Tools.Deny,
			NewTools: s.Tools.Default, AllowPaths: s.AllowPaths, UpstreamHeader: s.UpstreamHeader, UpstreamHeaderFile: s.UpstreamHeaderFile,
			Pin: s.Pin, FilesDir: s.FilesDir})
		ls := &out.Services[len(out.Services)-1]
		if s.UpstreamHeader != "" {
			ls.UpstreamHeaderSource = "keyring"
			if s.UpstreamHeaderFile != "" {
				ls.UpstreamHeaderSource = "file"
			}
		}
		if s.HasUpstream() {
			ls.CallerKeyFile = service.CallerKeyPath(a.Dir, *s)
		}
	}
	return a.emit(out, func() {
		if len(out.Services) == 0 {
			a.say("No services yet: toprelay add <name> …\n")
			return
		}
		for i, s := range out.Services {
			a.say("%-12s %-4s %s\n             → %s\n", s.Name, s.Kind, s.URL, s.Local)
			if h := describeHeader(&cfg.Services[i]); h != "" {
				a.say("             credential: %s\n", h)
			}
			if s.Pin != nil {
				a.say("             pinned to: %s\n", service.Pin(cfg.Services[i]).String())
			}
		}
	})
}

func cmdLogs(a *App, args []string) error {
	fs := a.newFlags("logs", "logs <name> [-f] [-n N] | logs --retention DAYS", `
Show the local call log of a service: time, method, path, MCP method, tool, status, duration, client, bytes in and
out, and why a call was refused. No bodies or credentials are ever logged. With --json, prints one JSON object
per call per line:
  {"time","method","path","mcpMethod"?,"tool"?,"status","durationMs","client"?,"refused"?,"reason"?,"bytesIn","bytesOut"}
reason is tool_not_allowed, invalid_request, rate_limited, credential_unavailable or app_unreachable.

--retention DAYS keeps records for that many days, for every service (0: until the 10 MB size cap rotates them
out); a running `+"`toprelay up`"+` applies it within the hour.`)
	follow := fs.Bool("f", false, "keep printing new calls")
	n := fs.Int("n", 20, "how many recent calls to show (0: all)")
	svcFlag := fs.String("service", "", "the service (instead of the positional name)")
	retention := fs.Int("retention", -1, "set how many days of call log to keep (0: no age limit)")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *retention >= 0 {
		if len(pos) > 0 || *svcFlag != "" || *follow {
			return usageErr("--retention applies to every service; give it alone")
		}
		cfg, err := a.loadConfig()
		if err != nil {
			return err
		}
		cfg.LogRetentionDays = *retention
		if err := config.Save(a.Dir, cfg); err != nil {
			return wrap("config", "check permissions on "+a.Dir, err)
		}
		return a.emit(map[string]any{"logRetentionDays": *retention}, func() {
			a.say("✓ call logs keep %d days (0: no age limit)\n", *retention)
		})
	}
	if *svcFlag != "" {
		pos = append(pos, *svcFlag)
	}
	if len(pos) != 1 {
		return usageErr("logs needs one service name")
	}
	if _, _, err := a.loadService(pos[0]); err != nil {
		return err
	}
	path := calllog.Path(a.Dir, pos[0])
	lines, err := calllog.Tail(path, *n)
	if err != nil {
		return wrap("logs", "check permissions on "+path, err)
	}
	print := func(line string) {
		if a.JSON {
			fmt.Fprintln(a.Stdout, line)
			return
		}
		var r calllog.Record
		if json.Unmarshal([]byte(line), &r) != nil {
			return
		}
		switch r.Type {
		case calllog.TypeCheckpoint:
			return // chain bookkeeping; `toprelay audit verify` checks it
		case calllog.TypePrune:
			fmt.Fprintf(a.Stdout, "%s  retention removed %d older records\n", r.Time.Local().Format("2006-01-02 15:04:05"), r.Removed)
			return
		}
		what := r.MCPMethod
		if r.Tool != "" {
			what += " " + r.Tool
		}
		if r.Refused || r.Reason != "" {
			what += " (" + map[bool]string{true: "refused", false: "failed"}[r.Refused]
			if r.Reason != "" {
				what += ": " + r.Reason
			}
			what += ")"
		}
		var flags string
		if len(r.Leaks) > 0 {
			verb := "redacted"
			if r.Blocked {
				verb = "withheld"
			}
			flags += "  ⚠ " + verb + " " + strings.Join(r.Leaks, ",")
		}
		if len(r.Warnings) > 0 {
			flags += "  ⚠ " + strings.Join(r.Warnings, ",")
		}
		fmt.Fprintf(a.Stdout, "%s  %-6s %-14s %-32s %3d %6dms  %s%s\n", r.Time.Local().Format("2006-01-02 15:04:05"), r.Method, r.Path, what, r.Status, r.DurationMs, r.Client, flags)
	}
	for _, l := range lines {
		print(l)
	}
	if len(lines) == 0 && !*follow {
		a.say("No calls logged for %s yet.\n", pos[0])
	}
	if !*follow {
		return nil
	}
	ctx, stop := signal.NotifyContext(a.Ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return calllog.Follow(ctx, path, 250*time.Millisecond, print)
}

func cmdRotate(a *App, args []string) error {
	fs := a.newFlags("rotate", "rotate <name>", `
Invalidate everything a bot holds for a service: its gen goes up, which changes the agent key and voids every bearer
key minted for it; the relay is redeployed and every OAuth grant for the service is revoked. A running
`+"`toprelay up`"+` reconnects with the new key by itself. Mint new keys with `+"`toprelay key`"+`.`)
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("rotate needs a service name")
	}
	cfg, s, err := a.loadService(pos[0])
	if err != nil {
		return err
	}
	s.Gen++
	a.note("Redeploying with %s gen %d …\n", s.Name, s.Gen)
	if _, err := a.deployConfig(cfg); err != nil {
		return err
	}
	if err := config.Save(a.Dir, cfg); err != nil {
		return wrap("config", "check permissions on "+a.Dir, err)
	}
	var warnings []string
	revoked := 0
	key, err := a.agentKey(s)
	if err == nil {
		var rc *relayapi.Client
		if rc, err = a.relay(cfg); err == nil {
			revoked, err = rc.RevokeOAuth(a.Ctx, s.Name, key)
		}
	}
	if err != nil {
		warnings = append(warnings, "couldn't revoke OAuth grants: "+err.Error()+"; run `toprelay rotate "+s.Name+"` again")
	}
	return a.emit(map[string]any{"service": s.Name, "gen": s.Gen, "oauthRevoked": revoked, "warnings": nonNil(warnings)}, func() {
		a.say("✓ %s rotated to gen %d: old bearer keys no longer work\n✓ %d OAuth grants revoked; claude.ai users must reconnect (toprelay pair %s)\n", s.Name, s.Gen, revoked, s.Name)
		for _, w := range warnings {
			a.say("warning: %s\n", w)
		}
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func cmdRemove(a *App, args []string) error {
	fs := a.newFlags("remove", "remove <name>", `
Remove a service: revoke its OAuth grants, drop it from the relay (redeploy) and delete its secrets from the
keyring. Its call log is kept in <config dir>/logs.`)
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("remove needs a service name")
	}
	cfg, s, err := a.loadService(pos[0])
	if err != nil {
		return err
	}
	name := s.Name
	envNames := slices.Clone(s.Source.EnvNames) // cfg.Remove below shifts the slice s points into
	ownCallerKey := s.HasUpstream() && s.CallerKeyFile == ""
	var warnings []string
	revoked := 0
	key, kerr := a.agentKey(s)
	if kerr == nil {
		rc, rerr := a.relay(cfg)
		if rerr == nil {
			revoked, rerr = rc.RevokeOAuth(a.Ctx, name, key)
		}
		if rerr != nil {
			warnings = append(warnings, "couldn't revoke OAuth grants first ("+rerr.Error()+"); removing the service from the relay makes them useless anyway")
		}
	}
	cfg.Remove(name)
	a.note("Redeploying without %s …\n", name)
	if _, err := a.deployConfig(cfg); err != nil {
		return err
	}
	if err := config.Save(a.Dir, cfg); err != nil {
		return wrap("config", "check permissions on "+a.Dir, err)
	}
	if err := a.Store.Delete(secrets.UpstreamHeader(name)); err != nil {
		warnings = append(warnings, "couldn't delete the upstream header secret: "+err.Error())
	}
	if err := secrets.DeleteEnv(a.Store, name, envNames); err != nil {
		warnings = append(warnings, "couldn't delete the stdio environment secrets: "+err.Error())
	}
	if ownCallerKey {
		if err := os.Remove(caller.DefaultKeyPath(a.Dir, name)); err != nil && !errors.Is(err, iofs.ErrNotExist) {
			warnings = append(warnings, "couldn't delete the caller key: "+err.Error())
		}
	}
	// A service added again later under this name starts with fresh pins.
	if err := os.Remove(pins.Path(a.Dir, name)); err != nil && !errors.Is(err, iofs.ErrNotExist) {
		warnings = append(warnings, "couldn't delete the tool pins: "+err.Error())
	}
	return a.emit(map[string]any{"service": name, "removed": true, "oauthRevoked": revoked, "warnings": nonNil(warnings)}, func() {
		a.say("✓ %s removed from the relay (%d OAuth grants revoked)\n", name, revoked)
		for _, w := range warnings {
			a.say("warning: %s\n", w)
		}
	})
}

// pairAlphabet has no 0/O, 1/I/L, so codes read back unambiguously.
const pairAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// PairCode returns a random code of n characters from pairAlphabet.
func PairCode(n int) (string, error) {
	var b strings.Builder
	max := big.NewInt(int64(len(pairAlphabet)))
	for range n {
		i, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(pairAlphabet[i.Int64()])
	}
	return b.String(), nil
}

func cmdPair(a *App, args []string) error {
	fs := a.newFlags("pair", "pair <name> [--ttl 10m]", `
Make a one-time pairing code for the relay's OAuth consent page (claude.ai, ChatGPT and other OAuth clients).
Add the service URL as a connector in the bot; when the consent page asks, type the code.`)
	ttlS := fs.String("ttl", "10m", "how long the code is valid (1m to 60m)")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("pair needs a service name")
	}
	ttl, err := parseDuration(*ttlS)
	if err != nil || ttl < time.Minute || ttl > time.Hour {
		return usageErr("--ttl must be between 1m and 60m")
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
	code, err := PairCode(12)
	if err != nil {
		return err
	}
	exp, err := rc.Pair(a.Ctx, s.Name, key, code, ttl)
	if err != nil {
		return wrap("relay", "run `toprelay doctor`", err)
	}
	return a.emit(map[string]any{"service": s.Name, "code": code, "expires": exp.Unix(), "expiresAt": exp.UTC().Format(time.RFC3339)}, func() {
		a.say("Pairing code for %s: %s\nValid until %s (%s). Type it on the consent page when you connect %s.\n",
			s.Name, code, exp.Local().Format("15:04:05"), ttl, serviceURL(cfg, s))
	})
}

// keyFor mints a bearer key for a service (used by key and test).
func (a *App) keyFor(s *config.Service, tools []string, expires time.Time) (keys.Bearer, error) {
	root, err := secrets.Require(a.Store, secrets.BearerRoot, "it is created by the first `toprelay add`")
	if err != nil {
		return keys.Bearer{}, wrap("no_roots", "run `toprelay add` or restore the keyring", err)
	}
	id, err := keys.NewKeyID()
	if err != nil {
		return keys.Bearer{}, err
	}
	return keys.Mint(root, id, keys.Caveats(s.Name, s.Gen, tools, expires))
}

// waitCtx is a.Ctx with a timeout.
func (a *App) waitCtx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(a.Ctx, d)
}
