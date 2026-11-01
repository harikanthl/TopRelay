// Package service builds the local handler and hello for one configured service.
package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/calllog"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/httpfwd"
	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
	"github.com/harikanthl/toprelay/agent/internal/stdiobridge"
	"github.com/harikanthl/toprelay/agent/internal/tunnel"
)

// Runtime is one service's local side: its handler, its policy and the hello it announces.
type Runtime struct {
	Svc     config.Service
	Handler http.Handler

	policy       atomic.Pointer[config.ToolPolicy]
	helloChanged chan struct{}
	bridge       *stdiobridge.Bridge
	mcpURL       string
	headers      map[string]string
	log          *slog.Logger

	mu        sync.Mutex
	known     []string
	lastHello *tunnel.Hello
}

// Build creates the runtime. upstreamValue is the keyring value of svc.UpstreamHeader. logw, if non-nil, records
// every call.
func Build(svc config.Service, upstreamValue string, logw *calllog.Log, log *slog.Logger) (*Runtime, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	rt := &Runtime{Svc: svc, helloChanged: make(chan struct{}, 1), log: log}
	p := svc.Tools
	rt.policy.Store(&p)
	allowed := func(tool string) bool { return rt.policy.Load().Allowed(tool) }
	var h http.Handler
	switch svc.Source.Type {
	case config.SourceMCPURL:
		u, err := url.Parse(svc.Source.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("service %s: bad MCP URL %q", svc.Name, svc.Source.URL)
		}
		rt.mcpURL = svc.Source.URL
		if svc.UpstreamHeader != "" {
			rt.headers = map[string]string{svc.UpstreamHeader: upstreamValue}
		}
		h = &mcpproxy.Proxy{Service: svc.Name, Target: u, Allowed: allowed, HeaderName: svc.UpstreamHeader, HeaderValue: upstreamValue,
			OnTools: rt.addKnown, Log: log}
	case config.SourceStdio:
		rt.bridge = stdiobridge.New(stdiobridge.Options{Service: svc.Name, Command: svc.Source.Command, Env: svc.Source.Env,
			Cwd: svc.Source.Cwd, Allowed: allowed, OnToolsChanged: rt.notify, Log: log})
		h = rt.bridge
	case config.SourceHTTP:
		u, err := url.Parse(svc.Source.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("service %s: bad base URL %q", svc.Name, svc.Source.URL)
		}
		h = httpfwd.New(u, svc.AllowPaths, svc.UpstreamHeader, upstreamValue)
	default:
		return nil, fmt.Errorf("service %s: unknown source type %q", svc.Name, svc.Source.Type)
	}
	if logw != nil {
		h = logw.Middleware(h)
	}
	rt.Handler = h
	return rt, nil
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
	rt.mu.Lock()
	known := slices.Clone(rt.known)
	rt.mu.Unlock()
	// Explicitly allowed tools are always listed, even before the upstream has been seen.
	return tunnel.Hello{Kind: config.KindMCP, Tools: p.AllowList(append(known, p.Allow...))}
}

// notify signals a hello change when the hello differs from the last one.
func (rt *Runtime) notify() {
	h := rt.Hello()
	rt.mu.Lock()
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
	if rt.mcpURL == "" {
		<-ctx.Done()
		return ctx.Err()
	}
	// Learn the upstream's tool names so a default-allow policy can be announced as a list; refresh every 5 min.
	delay := 30 * time.Second
	for {
		lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		tools, err := mcpproxy.ListTools(lctx, rt.mcpURL, rt.headers)
		cancel()
		next := 5 * time.Minute
		if err != nil {
			rt.log.Warn("can't list tools from the local MCP server", "service", rt.Svc.Name, "err", err)
			next = delay
		} else {
			rt.addKnown(mcpproxy.ToolNames(tools))
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
