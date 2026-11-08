// Package service builds the local handler and hello for one configured service.
package service

import (
	"context"
	"fmt"
	"log/slog"
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
	// Some client may use it: what's mirrored, listed to the relay, and the ceiling for every client.
	allowed := func(tool string) bool { return rt.policy.Load().AllowedAny(tool, rt.hint(tool)) }
	allowedFor := func(client string) mcpproxy.Allowed {
		p := rt.policy.Load()
		return func(tool string) bool { return p.AllowedFor(tool, client, rt.hint(tool)) }
	}
	var h http.Handler
	switch svc.Source.Type {
	case config.SourceMCPURL:
		rt.proxy = &mcpproxy.Proxy{Service: svc.Name, Target: up.Endpoint.URL, Transport: up.Transport(), Allowed: allowed,
			AllowedFor: allowedFor, OnHints: rt.learnHints,
			Prepare: up.PrepareRelayed, OnTools: rt.addKnown, Log: log, EarlySSE: tun.EarlySSE, KeepAlive: tun.KeepAlive}
		h = rt.proxy
	case config.SourceStdio:
		rt.bridge = stdiobridge.New(stdiobridge.Options{Service: svc.Name, Command: svc.Source.Command, Env: svc.Source.Env,
			Cwd: svc.Source.Cwd, Allowed: allowed, AllowedFor: allowedFor, OnHints: rt.learnHints, OnToolsChanged: rt.notify, Log: log})
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

// learnHints records listed tools' annotations; a change may change the hello.
func (rt *Runtime) learnHints(hs []mcpproxy.ToolHint) {
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
	// Explicitly allowed tools are always listed, even before the upstream has been seen.
	return tunnel.Hello{Kind: config.KindMCP, Tools: p.AllowListHints(append(append(slices.Clone(rt.known), p.Allow...), clientAllows(p)...), func(t string) config.Hint { return rt.hints[t] })}
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
