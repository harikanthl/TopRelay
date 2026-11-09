// Package daemon runs every configured service (`toprelay up`): one tunnel per service, a local handler per
// kind, live reload of the config, and state.json for the menu-bar app.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/caller"
	"github.com/harikanthl/toprelay/agent/internal/calllog"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/files"
	"github.com/harikanthl/toprelay/agent/internal/keys"
	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
	"github.com/harikanthl/toprelay/agent/internal/pins"
	"github.com/harikanthl/toprelay/agent/internal/redact"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/service"
	"github.com/harikanthl/toprelay/agent/internal/tunnel"
	"github.com/harikanthl/toprelay/agent/internal/upstream"
)

// Agent is the agent identifier sent in hello.
const Agent = "toprelay/0.2.0"

// Daemon is `toprelay up`.
type Daemon struct {
	Dir   string
	Store secrets.Store
	// RelayURL overrides the configured public URL (TOPRELAY_RELAY_URL).
	RelayURL string
	Log      *slog.Logger
	// Reload triggers a config reload (SIGHUP); the config file is also polled.
	Reload       <-chan struct{}
	PollInterval time.Duration
	// TunnelOptions, if set, adjusts each tunnel's options (tests).
	TunnelOptions func(*tunnel.Options)
	// Tuning adjusts long-call timings (tests; zero: defaults).
	Tuning service.Tuning
	// State, if set, is the state file to use (the control socket subscribes to it); otherwise Run makes one.
	State *StateFile
	// Files holds offered files and download links (shared with the control socket); Run makes one if nil.
	Files *files.Registry

	state         *StateFile
	retentionDays atomic.Int64
	mu            sync.Mutex
	running       map[string]*runner
}

type runner struct {
	sig string
	// cred identifies the credential settings, which are applied live (no restart, no reconnect).
	cred   string
	limits string
	cancel context.CancelFunc
	done   chan struct{}
	rt     *service.Runtime
}

// signature identifies the parts of a service that need a restart when they change.
func signature(base string, s config.Service) string {
	b, _ := json.Marshal(struct {
		Base   string
		Kind   string
		Source config.Source
		Gen    int
		Paths  []string
		Pin    *config.Pin
		Files  string
	}{base, s.Kind, s.Source, s.Gen, s.AllowPaths, s.Pin, s.FilesDir})
	return string(b)
}

// credSignature identifies the credential settings; `set-header` bumps UpstreamHeaderRev when only the keyring
// value changed.
func credSignature(s config.Service) string {
	b, _ := json.Marshal([]any{s.UpstreamHeader, s.UpstreamHeaderFile, s.UpstreamHeaderRev, s.CallerKeyFile})
	return string(b)
}

func fileStamp(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", st.ModTime().UnixNano(), st.Size())
}

// Run runs until ctx ends.
func (d *Daemon) Run(ctx context.Context) error {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.PollInterval <= 0 {
		d.PollInterval = 2 * time.Second
	}
	d.running = map[string]*runner{}
	if d.Files == nil {
		d.Files = files.NewRegistry()
	}
	d.state = d.State
	if d.state == nil {
		d.state = NewStateFile(StatePath(d.Dir), nil)
	}
	if err := d.state.Write(); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	defer d.state.Stop()

	cfg, err := config.LoadMigrated(d.Dir, d.Store)
	if err != nil {
		return err
	}
	if len(cfg.Services) == 0 {
		d.Log.Warn("no services configured yet; waiting for `toprelay add`")
	}
	if err := d.reconcile(ctx, cfg); err != nil {
		return err
	}
	stamp := fileStamp(config.Path(d.Dir))
	t := time.NewTicker(d.PollInterval)
	defer t.Stop()
	defer d.stopAll()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-d.Reload:
		case <-t.C:
			d.refreshPins()
			if s := fileStamp(config.Path(d.Dir)); s == stamp {
				continue
			}
		}
		stamp = fileStamp(config.Path(d.Dir))
		cfg, err := config.LoadMigrated(d.Dir, d.Store)
		if err != nil {
			d.Log.Error("config reload failed; keeping the running services", "err", err)
			continue
		}
		if err := d.reconcile(ctx, cfg); err != nil {
			d.Log.Error("config reload failed", "err", err)
		}
	}
}

// refreshPins picks up tool approvals made by `toprelay pins` (also from the Mac app).
func (d *Daemon) refreshPins() {
	d.mu.Lock()
	rs := make(map[string]*runner, len(d.running))
	for n, r := range d.running {
		rs[n] = r
	}
	d.mu.Unlock()
	for name, r := range rs {
		changed, err := r.rt.RefreshPins()
		if err != nil {
			d.Log.Warn("can't read the tool pins", "service", name, "err", err)
			continue
		}
		if changed {
			held := r.rt.Held()
			d.state.Update(name, func(st *ServiceState) { st.Held = held })
		}
	}
}

func (d *Daemon) stopAll() {
	d.mu.Lock()
	rs := d.running
	d.running = map[string]*runner{}
	d.mu.Unlock()
	for _, r := range rs {
		r.cancel()
	}
	for _, r := range rs {
		<-r.done
	}
}

func (d *Daemon) base(cfg *config.Config) (string, error) {
	if d.RelayURL != "" {
		return d.RelayURL, nil
	}
	if cfg.PublicURL == "" {
		return "", errors.New("the relay isn't deployed yet: run `toprelay add` (or set TOPRELAY_RELAY_URL for a local relay)")
	}
	return cfg.PublicURL, nil
}

// reconcile starts, stops, restarts or updates services to match cfg.
func (d *Daemon) reconcile(ctx context.Context, cfg *config.Config) error {
	if len(cfg.Services) == 0 {
		d.mu.Lock()
		names := make([]string, 0, len(d.running))
		for n := range d.running {
			names = append(names, n)
		}
		d.mu.Unlock()
		d.stopAll()
		for _, n := range names {
			d.state.Remove(n)
			d.Log.Info("service removed", "service", n)
		}
		return nil
	}
	base, err := d.base(cfg)
	if err != nil {
		return err
	}
	d.retentionDays.Store(int64(cfg.LogRetentionDays))
	agentRoot, err := secrets.Require(d.Store, secrets.AgentRoot, "run `toprelay add` to create it (or seed it for a local relay; see README)")
	if err != nil {
		return err
	}
	want := map[string]config.Service{}
	for _, s := range cfg.Services {
		want[s.Name] = s
	}
	d.mu.Lock()
	var stop []*runner
	var removed []string
	for name, r := range d.running {
		s, ok := want[name]
		if !ok || r.sig != signature(base, s) {
			stop = append(stop, r)
			delete(d.running, name)
			if !ok {
				removed = append(removed, name)
			}
		}
	}
	d.mu.Unlock()
	for _, r := range stop {
		r.cancel()
		<-r.done
	}
	// Only after the runners stopped: their last state callbacks would re-create the entries.
	for _, name := range removed {
		d.state.Remove(name)
		d.Log.Info("service removed", "service", name)
	}
	for _, s := range cfg.Services {
		d.mu.Lock()
		r := d.running[s.Name]
		d.mu.Unlock()
		if r != nil {
			if cs := credSignature(s); cs != r.cred && s.HasUpstream() {
				if err := d.applyCredential(r, s); err != nil {
					d.Log.Error("can't apply the new upstream credential; keeping the old one", "service", s.Name, "err", err)
					d.state.Update(s.Name, func(st *ServiceState) { st.LastError = redact.Log(err.Error()) })
				} else {
					r.cred = cs
					d.Log.Info("upstream credential updated", "service", s.Name)
				}
			}
			if ls := limitsSig(s); ls != r.limits {
				if err := r.rt.SetLimits(s.Limits); err != nil {
					d.Log.Error("bad rate limits; keeping the old ones", "service", s.Name, "err", err)
				} else {
					r.limits = ls
					d.Log.Info("rate limits changed", "service", s.Name)
				}
			}
			if !samePolicy(r.rt.Policy(), s.Tools) {
				d.Log.Info("tool policy changed", "service", s.Name)
				r.rt.SetPolicy(s.Tools)
			}
			if sc := s.ShieldConfig(); sc != r.rt.ShieldConfig() {
				d.Log.Info("shield changed", "service", s.Name, "leaks", sc.LeakMode(), "injection", sc.InjectionMode())
				r.rt.SetShield(sc)
			}
			continue
		}
		if err := d.start(ctx, base, agentRoot, s); err != nil {
			d.Log.Error("can't start service", "service", s.Name, "err", err)
			d.state.Update(s.Name, func(st *ServiceState) { st.Connected, st.LastError = false, redact.Log(err.Error()) })
		}
	}
	return nil
}

// applyCredential swaps a running service's credential and caller key file.
func (d *Daemon) applyCredential(r *runner, s config.Service) error {
	cred, err := service.Credential(s, d.Store)
	if err != nil {
		return err
	}
	keyPath := service.CallerKeyPath(d.Dir, s)
	if s.CallerKeyFile == "" {
		if _, err := caller.EnsureKeyFile(keyPath); err != nil {
			return err
		}
	}
	r.rt.SetCredential(cred)
	r.rt.Upstream.SetCallerKeyFile(keyPath)
	return nil
}

// linker turns a toprelay-file URI from the service's app into a signed, single-use relay URL.
func (d *Daemon) linker(base, bearerRoot string, s config.Service, relayFiles *atomic.Bool) func(client, uri string) (mcpproxy.Link, error) {
	return func(client, uri string) (mcpproxy.Link, error) {
		if !relayFiles.Load() {
			return mcpproxy.Link{}, errors.New("the relay on Cloudflare is older than this agent; the owner should run `toprelay deploy`")
		}
		e, err := d.Files.Resolve(s.Name, s.FilesDir, uri)
		if err != nil {
			return mcpproxy.Link{}, err
		}
		id, exp, err := d.Files.NewDownload(e, client, 0)
		if err != nil {
			return mcpproxy.Link{}, err
		}
		u, err := keys.FileURL(base, bearerRoot, s.Name, s.Gen, id, exp)
		if err != nil {
			return mcpproxy.Link{}, err
		}
		return mcpproxy.Link{URL: u, Name: e.Name, MimeType: e.ContentType, Size: e.Size}, nil
	}
}

// downloads serves the relay's internal file requests: GET /files/<id>, each id once.
func (d *Daemon) downloads(service string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := strings.CutPrefix(r.URL.Path, "/files/")
		if !ok || !keys.FileIDRE.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		e, _, err := d.Files.Take(service, id)
		if err != nil {
			http.Error(w, "toprelay: "+err.Error()+". Ask for the file again.", http.StatusGone)
			return
		}
		files.Serve(w, e)
	})
}

func limitsSig(s config.Service) string {
	b, _ := json.Marshal(s.Limits)
	return string(b)
}

func samePolicy(a, b config.ToolPolicy) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func (d *Daemon) start(ctx context.Context, base, agentRoot string, s config.Service) error {
	key, err := keys.AgentKey(agentRoot, s.Name, s.Gen)
	if err != nil {
		return err
	}
	redact.Add(key)
	var up *upstream.Upstream
	if s.HasUpstream() {
		if up, err = service.NewUpstream(s, d.Dir, d.Store); err != nil {
			return err
		}
	}
	// The stdio child's environment comes from the keyring, into a copy only: it never reaches config.json.
	bs := s
	if bs.Source.Env, err = secrets.LoadEnv(d.Store, s.Name, s.Source.EnvNames); err != nil {
		return err
	}
	logw, err := calllog.Open(calllog.Path(d.Dir, s.Name))
	if err != nil {
		return fmt.Errorf("open call log: %w", err)
	}
	auditKey, err := keys.AuditKey(agentRoot)
	if err != nil {
		logw.Close()
		return err
	}
	logw.SetSigner(s.Name, auditKey)
	logw.OnRecord = func(r calllog.Record) {
		d.state.Update(s.Name, func(st *ServiceState) {
			st.Requests++
			if r.Refused {
				st.Refused++
			}
			if r.Reason == mcpproxy.ReasonRateLimited {
				st.Limited++
			}
			if len(r.Leaks) > 0 {
				st.Leaks++
			}
			if len(r.Warnings) > 0 {
				st.Warnings++
			}
		})
	}
	log := d.Log // every component logs the service name itself
	rt, err := service.BuildWith(bs, up, logw, log, d.Tuning)
	if err != nil {
		logw.Close()
		return err
	}
	if s.Kind == config.KindMCP {
		// A damaged pins file stops the service rather than forgetting what was approved.
		ps, err := pins.Open(pins.Path(d.Dir, s.Name))
		if err != nil {
			logw.Close()
			return err
		}
		rt.OnHeld = func([]string) {
			held := rt.Held()
			d.state.Update(s.Name, func(st *ServiceState) { st.Held = held })
		}
		rt.SetPins(ps)
	}
	var relayFiles atomic.Bool
	var filesHandler http.Handler
	if bearerRoot, berr := d.Store.Get(secrets.BearerRoot); berr == nil && rt.SetLinks(d.linker(base, bearerRoot, s, &relayFiles)) {
		filesHandler = logw.Middleware(d.downloads(s.Name))
	}
	o := tunnel.Options{BaseURL: base, Service: s.Name, AgentKey: key, Agent: Agent, Handler: rt.Handler, Files: filesHandler,
		OnWelcome: func(caps []string) { relayFiles.Store(slices.Contains(caps, "files")) },
		Hello:     rt.Hello, HelloChanged: rt.HelloChanged(), Log: log,
		OnState: func(ts tunnel.State) {
			d.state.Update(s.Name, func(st *ServiceState) {
				if ts.Connected {
					now := time.Now().UTC()
					st.Connected, st.Since, st.LastError, st.ReconnectAttempt, st.NextRetryMs = true, &now, "", 0, 0
					return
				}
				st.Connected, st.Since = false, nil
				st.ReconnectAttempt, st.NextRetryMs = ts.Attempt, ts.Retry.Milliseconds()
				if ts.Err != nil {
					st.LastError = redact.Log(ts.Err.Error())
				}
			})
		}}
	if d.TunnelOptions != nil {
		d.TunnelOptions(&o)
	}
	sctx, cancel := context.WithCancel(ctx)
	// Retention is read on every pass, so a change applies within the hour; 0 means no age limit.
	prune := func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if days := d.retentionDays.Load(); days > 0 {
				if n, err := logw.Prune(time.Now().Add(-time.Duration(days) * 24 * time.Hour)); err != nil {
					log.Warn("can't prune the call log", "service", s.Name, "err", err)
				} else if n > 0 {
					log.Info("pruned the call log", "service", s.Name, "records", n)
				}
			}
			select {
			case <-sctx.Done():
				return
			case <-t.C:
			}
		}
	}
	r := &runner{sig: signature(base, s), cred: credSignature(s), limits: limitsSig(s), cancel: cancel, done: make(chan struct{}), rt: rt}
	held := rt.Held()
	d.state.Update(s.Name, func(st *ServiceState) { *st = ServiceState{Held: held} })
	d.mu.Lock()
	d.running[s.Name] = r
	d.mu.Unlock()
	go func() {
		defer close(r.done)
		defer logw.Close()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); prune() }() // stops before logw.Close runs
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rt.Run(sctx); err != nil && sctx.Err() == nil {
				log.Error("service stopped", "service", s.Name, "err", err)
			}
		}()
		// Give a stdio child a moment so the first hello carries its tools; connect regardless.
		wctx, wcancel := context.WithTimeout(sctx, 15*time.Second)
		rt.WaitReady(wctx)
		wcancel()
		tunnel.New(o).Run(sctx)
		wg.Wait()
	}()
	log.Info("service started", "service", s.Name, "local", s.LocalTarget())
	return nil
}
