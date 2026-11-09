// Package service builds the local handler and hello for one configured service.
package service

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/calllog"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/httpfwd"
	"github.com/harikanthl/toprelay/agent/internal/limits"
	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
	"github.com/harikanthl/toprelay/agent/internal/pins"
	"github.com/harikanthl/toprelay/agent/internal/shield"
	"github.com/harikanthl/toprelay/agent/internal/stdiobridge"
	"github.com/harikanthl/toprelay/agent/internal/tunnel"
	"github.com/harikanthl/toprelay/agent/internal/upstream"
)

// Runtime is one service's local side: its handler, its policy and the hello it announces.
type Runtime struct {
	Svc     config.Service
	Handler http.Handler

	policy       atomic.Pointer[config.ToolPolicy]
	helloChanged chan struct{}
	bridge       *stdiobridge.Bridge
	proxy        *mcpproxy.Proxy
	limiter      *limits.Limiter
	// Upstream reaches the app (nil for stdio).
	Upstream *upstream.Upstream
	log      *slog.Logger

	mu        sync.Mutex
	known     []string
	hints     map[string]config.Hint
	lastHello *tunnel.Hello
	// descFindings are injection findings in each listed tool's description.
	descFindings map[string][]shield.Finding

	// pins holds tools whose definition changed since approval (nil: no pinning). shieldCfg is the live shield.
	pins      atomic.Pointer[pins.Store]
	shieldCfg atomic.Pointer[shield.Config]
	// OnHeld, if set, is called with the tools that a listing newly held for review.
	OnHeld func([]string)
}

// Build creates the runtime. up reaches the app (from NewUpstream; nil for stdio). logw, if non-nil, records every
// call.
func Build(svc config.Service, up *upstream.Upstream, logw *calllog.Log, log *slog.Logger) (*Runtime, error) {
	return BuildWith(svc, up, logw, log, Tuning{})
}

// Tuning adjusts long-call timings; zero values use the mcpproxy defaults.
type Tuning struct {
	EarlySSE, KeepAlive time.Duration
}

// BuildWith is Build with timings.
func BuildWith(svc config.Service, up *upstream.Upstream, logw *calllog.Log, log *slog.Logger, tun Tuning) (*Runtime, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	rt := &Runtime{Svc: svc, helloChanged: make(chan struct{}, 1), log: log, Upstream: up}
	if svc.HasUpstream() && up == nil {
		return nil, fmt.Errorf("service %s: no upstream", svc.Name)
	}
	p := svc.Tools
	rt.policy.Store(&p)
	sc := svc.ShieldConfig()
	rt.shieldCfg.Store(&sc)
	// Some client may use it: what's mirrored, listed to the relay, and the ceiling for every client. A tool
	// held for review is no one's.
	allowed := func(tool string) bool { return !rt.IsHeld(tool) && rt.policy.Load().AllowedAny(tool, rt.hint(tool)) }
	allowedFor := func(client string) mcpproxy.Allowed {
		p := rt.policy.Load()
		return func(tool string) bool { return !rt.IsHeld(tool) && p.AllowedFor(tool, client, rt.hint(tool)) }
	}
	var h http.Handler
	switch svc.Source.Type {
	case config.SourceMCPURL:
		rt.proxy = &mcpproxy.Proxy{Service: svc.Name, Target: up.Endpoint.URL, Transport: up.Transport(), Allowed: allowed,
			AllowedFor: allowedFor, OnHints: rt.learnHints, Held: rt.IsHeld, Shield: rt.ShieldConfig,
			Prepare: up.PrepareRelayed, OnTools: rt.addKnown, Log: log, EarlySSE: tun.EarlySSE, KeepAlive: tun.KeepAlive}
		h = rt.proxy
	case config.SourceStdio:
		rt.bridge = stdiobridge.New(stdiobridge.Options{Service: svc.Name, Command: svc.Source.Command, Env: svc.Source.Env,
			Cwd: svc.Source.Cwd, Allowed: allowed, AllowedFor: allowedFor, OnHints: rt.learnHints, OnToolsChanged: rt.notify,
			Held: rt.IsHeld, Shield: rt.ShieldConfig, Log: log})
		h = rt.bridge
	case config.SourceHTTP:
		f := httpfwd.New(up.Endpoint.URL, svc.AllowPaths, up.PrepareRelayed)
		f.Transport, f.Log = up.Transport(), log
		h = f
	default:
		return nil, fmt.Errorf("service %s: unknown source type %q", svc.Name, svc.Source.Type)
	}
	var lc config.Limits
	if svc.Limits != nil {
		lc = *svc.Limits
	}
	lim, err := limits.New(lc)
	if err != nil {
		return nil, fmt.Errorf("service %s: limits: %w", svc.Name, err)
	}
	rt.limiter = lim
	h = lim.Middleware(svc.Name, h)
	if logw != nil {
		h = logw.Middleware(h)
	}
	rt.Handler = h
	return rt, nil
}

// SetLimits swaps the rate limits live.
func (rt *Runtime) SetLimits(l *config.Limits) error {
	var c config.Limits
	if l != nil {
		c = *l
	}
	return rt.limiter.Set(c)
}

// SetCredential swaps the app credential live (after `toprelay set-header`).
func (rt *Runtime) SetCredential(c *upstream.Credential) {
	if rt.Upstream != nil {
		rt.Upstream.SetCredential(c)
	}
}

// SetLinks installs the file-link minter (before the tunnel starts). Only MCP URL services rewrite links.
func (rt *Runtime) SetLinks(fn func(client, uri string) (mcpproxy.Link, error)) bool {
	if rt.proxy == nil {
		return false
	}
	rt.proxy.Links = fn
	return true
}

// SetPins turns on tool pinning with store (before Run). Tools already held stay hidden from the start.
func (rt *Runtime) SetPins(store *pins.Store) {
	rt.pins.Store(store)
	rt.pinsChanged()
}

// IsHeld reports whether a tool is held for review because its definition changed.
func (rt *Runtime) IsHeld(tool string) bool {
	st := rt.pins.Load()
	return st != nil && st.IsHeld(tool)
}

// Held returns the tools held for review.
func (rt *Runtime) Held() []string {
	if st := rt.pins.Load(); st != nil {
		return st.Held()
	}
	return nil
}

// RefreshPins picks up approvals made by the CLI (or the Mac app through it). It reports whether the held set
// changed.
func (rt *Runtime) RefreshPins() (bool, error) {
	st := rt.pins.Load()
	if st == nil {
		return false, nil
	}
	changed, err := st.Refresh()
	if changed {
		rt.log.Info("tool review changed", "service", rt.Svc.Name, "held", st.Held())
		rt.pinsChanged()
	}
	return changed, err
}

// pinsChanged re-applies the policy where it is mirrored (the stdio bridge) and re-announces the hello.
func (rt *Runtime) pinsChanged() {
	if rt.bridge != nil {
		rt.bridge.PolicyChanged()
	}
	rt.notify()
}

// ShieldConfig returns the live shield settings.
func (rt *Runtime) ShieldConfig() shield.Config { return *rt.shieldCfg.Load() }

// SetShield swaps the shield settings live.
func (rt *Runtime) SetShield(c shield.Config) { rt.shieldCfg.Store(&c) }

// DescriptionFindings returns injection findings in listed tools' descriptions, by tool.
func (rt *Runtime) DescriptionFindings() map[string][]shield.Finding {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return maps.Clone(rt.descFindings)
}

// HelloChanged signals that Hello() changed.
func (rt *Runtime) HelloChanged() <-chan struct{} { return rt.helloChanged }

// Policy returns the current tool policy.
func (rt *Runtime) Policy() config.ToolPolicy { return *rt.policy.Load() }

// SetPolicy swaps the tool policy; the next hello reflects it.
func (rt *Runtime) SetPolicy(p config.ToolPolicy) {
	rt.policy.Store(&p)
	if rt.bridge != nil {
		rt.bridge.PolicyChanged() // the bridge calls notify once its exposed set changes
	}
	rt.notify()
}

// clientAllows lists the tools client rules allow by name (sorted, so the hello is stable).
func clientAllows(p config.ToolPolicy) []string {
	var out []string
	for _, r := range p.Clients {
		out = append(out, r.Allow...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// hint returns what a tool's annotations said when it was last listed.
func (rt *Runtime) hint(tool string) config.Hint {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.hints[tool]
}

// learnHints records listed tools' annotations, pins their definitions (holding changed ones), and scans
// their descriptions; a change may change the hello. It runs before the listing is filtered, so a tool that
// changes is hidden from the very listing that shows the change.
func (rt *Runtime) learnHints(hs []mcpproxy.ToolHint) {
	rt.observePins(hs)
	rt.scanDescriptions(hs)
	rt.mu.Lock()
	if rt.hints == nil {
		rt.hints = map[string]config.Hint{}
	}
	changed := false
	names := make([]string, 0, len(hs))
	for _, h := range hs {
		nh := config.Hint{Known: true, ReadOnly: h.ReadOnly, Destructive: h.Destructive}
		if rt.hints[h.Name] != nh {
			rt.hints[h.Name], changed = nh, true
		}
		names = append(names, h.Name)
	}
	rt.mu.Unlock()
	rt.addKnown(names)
	if changed {
		rt.notify()
	}
}

func (rt *Runtime) observePins(hs []mcpproxy.ToolHint) {
	st := rt.pins.Load()
	if st == nil {
		return
	}
	tools := make([]pins.Tool, 0, len(hs))
	for _, h := range hs {
		tools = append(tools, pins.Tool{Name: h.Name, Fingerprint: h.Fingerprint, Description: h.Description})
	}
	held, err := st.Observe(tools)
	if err != nil {
		rt.log.Error("can't record tool pins", "service", rt.Svc.Name, "err", err)
		return
	}
	if len(held) == 0 {
		return
	}
	rt.log.Warn("tool definitions changed since they were approved; holding them for review (toprelay pins)",
		"service", rt.Svc.Name, "tools", held)
	if rt.OnHeld != nil {
		rt.OnHeld(held)
	}
	rt.notify()
}

func (rt *Runtime) scanDescriptions(hs []mcpproxy.ToolHint) {
	if rt.ShieldConfig().InjectionMode() == shield.InjectionOff {
		return
	}
	found := map[string][]shield.Finding{}
	for _, h := range hs {
		if fs := shield.ScanText(h.Description); len(fs) > 0 {
			found[h.Name] = fs
		}
	}
	rt.mu.Lock()
	if rt.descFindings == nil {
		rt.descFindings = map[string][]shield.Finding{}
	}
	var fresh []string
	for name, fs := range found {
		if _, ok := rt.descFindings[name]; !ok {
			fresh = append(fresh, name)
		}
		rt.descFindings[name] = fs
	}
	for _, h := range hs {
		if _, ok := found[h.Name]; !ok {
			delete(rt.descFindings, h.Name)
		}
	}
	rt.mu.Unlock()
	slices.Sort(fresh)
	for _, name := range fresh {
		rt.log.Warn("a tool description contains text that tries to instruct the model", "service", rt.Svc.Name,
			"tool", name, "rules", shield.Rules(found[name]))
	}
}

func (rt *Runtime) addKnown(names []string) {
	rt.mu.Lock()
	changed := false
	for _, n := range names {
		if !slices.Contains(rt.known, n) {
			rt.known = append(rt.known, n)
			changed = true
		}
	}
	rt.mu.Unlock()
	if changed {
		rt.notify()
	}
}

// Hello is the hello announced for this service.
func (rt *Runtime) Hello() tunnel.Hello {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.helloLocked()
}

// helloLocked computes the hello; rt.mu must be held. (It may take the bridge's lock: the order is always rt.mu,
// then the bridge's, and the bridge calls notify only after releasing its own.)
func (rt *Runtime) helloLocked() tunnel.Hello {
	p := rt.Policy()
	if rt.Svc.Kind == config.KindHTTP {
		return tunnel.Hello{Kind: config.KindHTTP, AllowAll: true}
	}
	if p.AllowsAll() {
		return tunnel.Hello{Kind: config.KindMCP, AllowAll: true}
	}
	if rt.bridge != nil {
		return tunnel.Hello{Kind: config.KindMCP, Tools: rt.bridge.Exposed()}
	}
	// Explicitly allowed tools are always listed, even before the upstream has been seen; held ones never are.
	tools := p.AllowListHints(append(append(slices.Clone(rt.known), p.Allow...), clientAllows(p)...), func(t string) config.Hint { return rt.hints[t] })
	return tunnel.Hello{Kind: config.KindMCP, Tools: slices.DeleteFunc(tools, rt.IsHeld)}
}

// notify signals a hello change when the hello differs from the last one. Computing and comparing under one lock
// keeps lastHello equal to the newest hello, so concurrent callers can't leave a stale one that would suppress a
// later, needed signal.
func (rt *Runtime) notify() {
	rt.mu.Lock()
	h := rt.helloLocked()
	same := rt.lastHello != nil && rt.lastHello.AllowAll == h.AllowAll && slices.Equal(rt.lastHello.Tools, h.Tools)
	if !same {
		rt.lastHello = &h
	}
	rt.mu.Unlock()
	if same {
		return
	}
	select {
	case rt.helloChanged <- struct{}{}:
	default:
	}
}

// Run runs the service's background work (the stdio child, or tool discovery for an MCP URL) until ctx ends.
func (rt *Runtime) Run(ctx context.Context) error {
	if rt.bridge != nil {
		return rt.bridge.Run(ctx)
	}
	if rt.Svc.Source.Type != config.SourceMCPURL {
		<-ctx.Done()
		return ctx.Err()
	}
	// Learn the upstream's tool names so a default-allow policy can be announced as a list; refresh every 5 min.
	delay := 30 * time.Second
	for {
		lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		tools, err := mcpproxy.ListTools(lctx, rt.Upstream.Endpoint.URL.String(), rt.Upstream.Client())
		cancel()
		next := 5 * time.Minute
		if err != nil {
			rt.log.Warn("can't list tools from the local MCP server", "service", rt.Svc.Name, "err", err)
			next = delay
		} else {
			rt.learnHints(mcpproxy.HintsFromTools(tools))
		}
		t := time.NewTimer(next)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// WaitReady waits for a stdio child to be up (no-op for other kinds).
func (rt *Runtime) WaitReady(ctx context.Context) error {
	if rt.bridge != nil {
		return rt.bridge.WaitReady(ctx)
	}
	return nil
}
