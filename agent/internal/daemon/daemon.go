// Package daemon runs every configured service (`toprelay up`): one tunnel per service, a local handler per
// kind, live reload of the config, and state.json for the menu-bar app.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/calllog"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/keys"
	"github.com/harikanthl/toprelay/agent/internal/redact"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/service"
	"github.com/harikanthl/toprelay/agent/internal/tunnel"
)

// Agent is the agent identifier sent in hello.
const Agent = "toprelay/0.1.0"

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

	state   *StateFile
	mu      sync.Mutex
	running map[string]*runner
}

type runner struct {
	sig    string
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
		Header string
	}{base, s.Kind, s.Source, s.Gen, s.AllowPaths, s.UpstreamHeader})
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
	d.state = NewStateFile(StatePath(d.Dir), nil)
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
			if !samePolicy(r.rt.Policy(), s.Tools) {
				d.Log.Info("tool policy changed", "service", s.Name)
				r.rt.SetPolicy(s.Tools)
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
	upstream := ""
	if s.UpstreamHeader != "" {
		v, err := secrets.Require(d.Store, secrets.UpstreamHeader(s.Name), "re-add the service with --header")
		if err != nil {
			return err
		}
		upstream = v
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
	logw.OnRecord = func(r calllog.Record) {
		d.state.Update(s.Name, func(st *ServiceState) {
			st.Requests++
			if r.Refused {
				st.Refused++
			}
		})
	}
	log := d.Log // every component logs the service name itself
	rt, err := service.Build(bs, upstream, logw, log)
	if err != nil {
		logw.Close()
		return err
	}
	o := tunnel.Options{BaseURL: base, Service: s.Name, AgentKey: key, Agent: Agent, Handler: rt.Handler,
		Hello: rt.Hello, HelloChanged: rt.HelloChanged(), Log: log,
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
	r := &runner{sig: signature(base, s), cancel: cancel, done: make(chan struct{}), rt: rt}
	d.state.Update(s.Name, func(st *ServiceState) { *st = ServiceState{} })
	d.mu.Lock()
	d.running[s.Name] = r
	d.mu.Unlock()
	go func() {
		defer close(r.done)
		defer logw.Close()
		var wg sync.WaitGroup
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
