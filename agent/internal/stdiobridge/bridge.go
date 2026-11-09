// Package stdiobridge launches a stdio MCP server and re-exposes it over Streamable HTTP (stateless, so MCP
// 2026-07-28 is offered) and the legacy SSE transport, with only the allowed tools.
package stdiobridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
	"github.com/harikanthl/toprelay/agent/internal/redact"
	"github.com/harikanthl/toprelay/agent/internal/reqctx"
	"github.com/harikanthl/toprelay/agent/internal/shield"
)

// ErrNotRunning is returned by forwarded calls while the child is (re)starting.
var ErrNotRunning = errors.New("the local MCP server is restarting; try again in a moment")

// Options configures a Bridge.
type Options struct {
	Service string
	Command []string
	Env     map[string]string
	Cwd     string
	// Allowed is the current tool policy: the tools some client may use (they're mirrored). AllowedFor, if set,
	// narrows it per client for each request.
	Allowed    mcpproxy.Allowed
	AllowedFor func(client string) mcpproxy.Allowed
	// OnHints receives the child's tool annotations before the policy is applied to them.
	OnHints func([]mcpproxy.ToolHint)
	// OnToolsChanged is called when the set of exposed tools changes.
	OnToolsChanged func()
	// Held reports tools held for review (pins); Allowed already refuses them. Shield returns the live shield
	// settings (nil: no shield).
	Held   func(tool string) bool
	Shield func() shield.Config
	Log    *slog.Logger
	// MinRestart and MaxRestart bound the restart backoff (defaults 1 s and 30 s).
	MinRestart, MaxRestart time.Duration
	// SSEKeepAlive is the interval of SSE comment keepalives (default 25 s).
	SSEKeepAlive time.Duration
}

// Bridge is a supervised stdio child plus the MCP server that re-exposes it.
type Bridge struct {
	o Options

	session  atomic.Pointer[mcp.ClientSession]
	server   atomic.Pointer[mcp.Server]
	ready    chan struct{}
	readyOne sync.Once
	refresh  chan struct{}

	mu        sync.Mutex
	childAll  []*mcp.Tool
	exposed   []string
	resources []string
	templates []string
	prompts   []string
	lastErr   error
	starts    int

	streamable http.Handler
	sse        http.Handler
	proxy      *mcpproxy.Proxy
}

// New prepares a bridge; call Run to start the child.
func New(o Options) *Bridge {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.MinRestart <= 0 {
		o.MinRestart = time.Second
	}
	if o.MaxRestart <= 0 {
		o.MaxRestart = 30 * time.Second
	}
	if o.SSEKeepAlive <= 0 {
		o.SSEKeepAlive = 25 * time.Second
	}
	if o.Allowed == nil {
		o.Allowed = func(string) bool { return false }
	}
	b := &Bridge{o: o, ready: make(chan struct{}), refresh: make(chan struct{}, 1)}
	get := func(*http.Request) *mcp.Server { return b.server.Load() }
	// These handlers are never bound to a network listener: requests arrive in-process from the tunnel, so
	// go-sdk's DNS-rebinding guard (which keys off the listener address) doesn't apply.
	b.streamable = mcp.NewStreamableHTTPHandler(get, &mcp.StreamableHTTPOptions{Stateless: true, DisableLocalhostProtection: true,
		MaxRequestBodyBytes: mcpproxy.MaxBody})
	b.sse = mcp.NewSSEHandler(get, &mcp.SSEOptions{DisableLocalhostProtection: true, MaxRequestBodyBytes: mcpproxy.MaxBody})
	b.proxy = &mcpproxy.Proxy{Service: o.Service, Target: &url.URL{Scheme: "http", Host: "stdio.bridge", Path: "/mcp"},
		Transport: mcpproxy.HandlerTransport{Handler: http.HandlerFunc(b.serveStreamable)}, Allowed: o.Allowed, AllowedFor: o.AllowedFor,
		Held: o.Held, Shield: o.Shield, Log: o.Log}
	return b
}

// Exposed returns the allowed tool names, in the child's order.
func (b *Bridge) Exposed() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.exposed)
}

// ChildTools returns every tool the child lists (allowed or not).
func (b *Bridge) ChildTools() []*mcp.Tool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.childAll)
}

// LastError returns the last child failure, if any.
func (b *Bridge) LastError() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastErr
}

// Starts returns how many times the child was started.
func (b *Bridge) Starts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starts
}

// WaitReady waits until the child has been connected once.
func (b *Bridge) WaitReady(ctx context.Context) error {
	select {
	case <-b.ready:
		return nil
	case <-ctx.Done():
		if err := b.LastError(); err != nil {
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
		}
		return ctx.Err()
	}
}

// PolicyChanged re-applies the tool policy to the exposed server.
func (b *Bridge) PolicyChanged() {
	select {
	case b.refresh <- struct{}{}:
	default:
	}
}

// Command builds the child's exec.Cmd.
func Command(ctx context.Context, argv []string, env map[string]string, cwd string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("no stdio command configured")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Dir = cwd
	cmd.WaitDelay = 5 * time.Second
	return cmd, nil
}

// lineLogger forwards the child's stderr to the log, one line at a time.
func lineLogger(log *slog.Logger, service string) io.WriteCloser {
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			log.Info("stdio server stderr", "service", service, "line", redact.Log(sc.Text()))
		}
		io.Copy(io.Discard, pr)
	}()
	return pw
}

// Run supervises the child until ctx ends, restarting it with backoff when it exits.
func (b *Bridge) Run(ctx context.Context) error {
	delay := b.o.MinRestart
	for {
		started := time.Now()
		err := b.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.mu.Lock()
		b.lastErr = err
		b.mu.Unlock()
		if time.Since(started) > time.Minute {
			delay = b.o.MinRestart
		}
		b.o.Log.Warn("stdio server stopped; restarting", "service", b.o.Service, "err", err, "in", delay)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		delay = min(delay*2, b.o.MaxRestart)
	}
}

func (b *Bridge) runOnce(ctx context.Context) error {
	cmd, err := Command(ctx, b.o.Command, b.o.Env, b.o.Cwd)
	if err != nil {
		return err
	}
	stderr := lineLogger(b.o.Log, b.o.Service)
	defer stderr.Close()
	cmd.Stderr = stderr
	changed := func() {
		select {
		case b.refresh <- struct{}{}:
		default:
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "toprelay", Version: "0.2.0"}, &mcp.ClientOptions{
		Capabilities:               &mcp.ClientCapabilities{},
		ToolListChangedHandler:     func(context.Context, *mcp.ToolListChangedRequest) { changed() },
		ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) { changed() },
		PromptListChangedHandler:   func(context.Context, *mcp.PromptListChangedRequest) { changed() },
	})
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	cs, err := client.Connect(cctx, &mcp.CommandTransport{Command: cmd}, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("start %s: %w", strings.Join(b.o.Command, " "), err)
	}
	defer cs.Close()
	b.mu.Lock()
	b.starts++
	b.mu.Unlock()
	if b.server.Load() == nil {
		b.server.Store(b.newServer(cs.InitializeResult()))
	}
	b.session.Store(cs)
	defer b.session.CompareAndSwap(cs, nil)
	if err := b.sync(ctx, cs); err != nil {
		return err
	}
	b.readyOne.Do(func() { close(b.ready) })
	b.o.Log.Info("stdio server running", "service", b.o.Service, "tools", len(b.Exposed()))

	done := make(chan error, 1)
	go func() { done <- cs.Wait() }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			if err == nil {
				err = errors.New("the stdio server exited")
			}
			return err
		case <-b.refresh:
			if err := b.sync(ctx, cs); err != nil {
				b.o.Log.Warn("refresh from stdio server failed", "service", b.o.Service, "err", err)
			}
		}
	}
}

func (b *Bridge) newServer(init *mcp.InitializeResult) *mcp.Server {
	opts := &mcp.ServerOptions{HasTools: true, HasResources: true, HasPrompts: true,
		CompletionHandler: func(ctx context.Context, r *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
			cs := b.session.Load()
			if cs == nil {
				return nil, ErrNotRunning
			}
			return cs.Complete(ctx, r.Params)
		}}
	name, version := b.o.Service, "0.1.0"
	if init != nil {
		opts.Instructions = init.Instructions
		if init.ServerInfo != nil {
			name, version = init.ServerInfo.Name, init.ServerInfo.Version
		}
	}
	return mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, opts)
}

func objectSchema(s any) (any, bool) {
	if s == nil {
		return json.RawMessage(`{"type":"object"}`), true
	}
	m, ok := s.(map[string]any)
	if !ok {
		return s, true
	}
	switch m["type"] {
	case nil:
		m["type"] = "object"
		return m, true
	case "object":
		return m, true
	default:
		return nil, false
	}
}

// sync lists the child's tools, resources and prompts and mirrors them (tools filtered) on the server.
func (b *Bridge) sync(ctx context.Context, cs *mcp.ClientSession) error {
	srv := b.server.Load()
	caps := &mcp.ServerCapabilities{}
	if ir := cs.InitializeResult(); ir != nil && ir.Capabilities != nil {
		caps = ir.Capabilities
	}
	var tools []*mcp.Tool
	if caps.Tools != nil {
		t, err := mcpproxy.ListAllTools(ctx, cs)
		if err != nil {
			return err
		}
		tools = t
	}
	if b.o.OnHints != nil && len(tools) > 0 {
		hs := mcpproxy.HintsFromTools(tools)
		b.o.OnHints(hs)
	}
	var exposed []string
	for _, t := range tools {
		if !b.o.Allowed(t.Name) {
			continue
		}
		schema, ok := objectSchema(t.InputSchema)
		if !ok {
			b.o.Log.Warn("skipping tool with a non-object input schema", "service", b.o.Service, "tool", t.Name)
			continue
		}
		copyT := *t
		copyT.InputSchema = schema
		name := t.Name
		srv.AddTool(&copyT, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			cs := b.session.Load()
			if cs == nil {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.o.Service + ": " + ErrNotRunning.Error()}}, IsError: true}, nil
			}
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Meta: r.Params.Meta, Name: name, Arguments: r.Params.Arguments})
			if err != nil || res == nil {
				return res, err
			}
			return b.shieldResult(ctx, res), nil
		})
		exposed = append(exposed, name)
	}

	var resources, templates, prompts []string
	if caps.Resources != nil {
		for r, err := range cs.Resources(ctx, nil) {
			if err != nil {
				return fmt.Errorf("list resources: %w", err)
			}
			srv.AddResource(r, b.forwardRead)
			resources = append(resources, r.URI)
		}
		for rt, err := range cs.ResourceTemplates(ctx, nil) {
			if err != nil {
				return fmt.Errorf("list resource templates: %w", err)
			}
			srv.AddResourceTemplate(rt, b.forwardRead)
			templates = append(templates, rt.URITemplate)
		}
	}
	if caps.Prompts != nil {
		for p, err := range cs.Prompts(ctx, nil) {
			if err != nil {
				return fmt.Errorf("list prompts: %w", err)
			}
			srv.AddPrompt(p, func(ctx context.Context, r *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				cs := b.session.Load()
				if cs == nil {
					return nil, ErrNotRunning
				}
				res, err := cs.GetPrompt(ctx, r.Params)
				if err != nil || res == nil {
					return res, err
				}
				return shieldValue(ctx, b, res, func() *mcp.GetPromptResult { return &mcp.GetPromptResult{} })
			})
			prompts = append(prompts, p.Name)
		}
	}

	b.mu.Lock()
	gone := func(old, now []string) []string {
		var out []string
		for _, o := range old {
			if !slices.Contains(now, o) {
				out = append(out, o)
			}
		}
		return out
	}
	rmTools := gone(b.exposed, exposed)
	rmRes := gone(b.resources, resources)
	rmTpl := gone(b.templates, templates)
	rmPrompts := gone(b.prompts, prompts)
	changed := !slices.Equal(b.exposed, exposed)
	b.childAll, b.exposed, b.resources, b.templates, b.prompts = tools, exposed, resources, templates, prompts
	b.mu.Unlock()
	if len(rmTools) > 0 {
		srv.RemoveTools(rmTools...)
	}
	if len(rmRes) > 0 {
		srv.RemoveResources(rmRes...)
	}
	if len(rmTpl) > 0 {
		srv.RemoveResourceTemplates(rmTpl...)
	}
	if len(rmPrompts) > 0 {
		srv.RemovePrompts(rmPrompts...)
	}
	if changed && b.o.OnToolsChanged != nil {
		b.o.OnToolsChanged()
	}
	return nil
}

func (b *Bridge) forwardRead(ctx context.Context, r *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	cs := b.session.Load()
	if cs == nil {
		return nil, ErrNotRunning
	}
	res, err := cs.ReadResource(ctx, r.Params)
	if err != nil || res == nil {
		return res, err
	}
	return shieldValue(ctx, b, res, func() *mcp.ReadResourceResult { return &mcp.ReadResourceResult{} })
}

// shieldResult redacts (or withholds) credentials in a tool result before it reaches either transport. The
// Streamable HTTP path is also inspected by the proxy, which scans for injection text; the legacy SSE path
// gets only this pass.
func (b *Bridge) shieldResult(ctx context.Context, res *mcp.CallToolResult) *mcp.CallToolResult {
	out, err := shieldValue(ctx, b, res, func() *mcp.CallToolResult { return &mcp.CallToolResult{} })
	if err != nil {
		// Never pass on an answer the shield couldn't check.
		b.o.Log.Error("can't inspect a tool result; withholding it", "service", b.o.Service, "err", err)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.o.Service + ": TopRelay couldn't check this result, so it withheld it."}}, IsError: true}
	}
	return out
}

// shieldValue applies the shield's leak pass to a result value: it is encoded as a JSON-RPC response, inspected,
// and decoded back into a fresh value from mk. The original is returned when nothing changed.
func shieldValue[T any](ctx context.Context, b *Bridge, v *T, mk func() *T) (*T, error) {
	if b.o.Shield == nil {
		return v, nil
	}
	cfg := b.o.Shield()
	if cfg.LeakMode() == shield.LeaksOff {
		return v, nil
	}
	res, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 0, "result": json.RawMessage(res)})
	cfg.Injection = shield.InjectionOff // the proxy pass reports injection, once
	out, changed, rep := cfg.InspectMessage(msg, b.o.Service)
	mcpproxy.RecordShield(reqctx.FindingsFrom(ctx), rep, b.o.Log, b.o.Service)
	if !changed {
		return v, nil
	}
	var back struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &back); err != nil {
		return nil, err
	}
	if back.Error != nil {
		return nil, errors.New(back.Error.Message)
	}
	nv := mk()
	if err := json.Unmarshal(back.Result, nv); err != nil {
		return nil, err
	}
	return nv, nil
}

func (b *Bridge) serveStreamable(w http.ResponseWriter, r *http.Request) {
	if b.server.Load() == nil {
		unavailable(w, b.o.Service)
		return
	}
	b.streamable.ServeHTTP(w, r)
}

func unavailable(w http.ResponseWriter, service string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "2")
	w.WriteHeader(http.StatusServiceUnavailable)
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32000,
		"message": service + ": the local MCP server is starting; try again in a moment."}})
	w.Write(b)
}

// ServeHTTP serves /mcp (Streamable HTTP, through the guard and filter) and /mcp/sse (legacy SSE).
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/mcp":
		b.proxy.ServeHTTP(w, r)
	case "/mcp/sse":
		b.serveSSE(w, r)
	default:
		http.NotFound(w, r)
	}
}

// serveSSE serves the legacy HTTP+SSE transport. Its endpoint event must carry the public path, so the request
// path is prefixed with /<service>. Disallowed calls posted to it get 403: SSE answers travel on the stream, not
// in the POST, so the tool-result refusal can't be used there.
func (b *Bridge) serveSSE(w http.ResponseWriter, r *http.Request) {
	if b.server.Load() == nil {
		unavailable(w, b.o.Service)
		return
	}
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, mcpproxy.MaxBody+1))
		if err != nil || len(body) > mcpproxy.MaxBody {
			http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
			return
		}
		restriction, restricted := reqctx.Tools(r.Context())
		policy := b.o.Allowed
		if b.o.AllowedFor != nil {
			policy = b.o.AllowedFor(r.Header.Get("X-Toprelay-Client"))
		}
		allowed := mcpproxy.Restrict(policy, restriction, restricted)
		if d := mcpproxy.Decide(body, "", b.o.Service, allowed); d.Refuse {
			w.Header().Set(mcpproxy.RefusedHeader, d.RefusedLabel())
			w.Header().Set(mcpproxy.ReasonHeader, d.Reason())
			if d.Invalid {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(d.Status)
				w.Write(d.Body)
				return
			}
			http.Error(w, mcpproxy.RefusalText(d.Tools[0], b.o.Service), http.StatusForbidden)
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		r.ContentLength = int64(len(body))
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + b.o.Service + r.URL.Path
	r2.URL.RawPath = ""
	if r.Method == http.MethodGet {
		kw := &keepAliveWriter{ResponseWriter: w}
		stop := make(chan struct{})
		go kw.loop(b.o.SSEKeepAlive, stop, r.Context())
		defer close(stop)
		b.sse.ServeHTTP(kw, r2)
		return
	}
	b.sse.ServeHTTP(w, r2)
}

// keepAliveWriter interleaves SSE comment lines so idle streams outlive the relay's 300 s chunk timeout.
type keepAliveWriter struct {
	http.ResponseWriter
	mu      sync.Mutex
	started bool
}

func (k *keepAliveWriter) WriteHeader(code int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.started = true
	k.ResponseWriter.WriteHeader(code)
}

func (k *keepAliveWriter) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.started = true
	return k.ResponseWriter.Write(p)
}

func (k *keepAliveWriter) Flush() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if f, ok := k.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (k *keepAliveWriter) loop(every time.Duration, stop chan struct{}, ctx context.Context) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			k.mu.Lock()
			if k.started {
				io.WriteString(k.ResponseWriter, ": keepalive\n\n")
				if f, ok := k.ResponseWriter.(http.Flusher); ok {
					f.Flush()
				}
			}
			k.mu.Unlock()
		}
	}
}

// ListChildTools starts the command once, lists its tools and stops it (for `add` and `tools`).
func ListChildTools(ctx context.Context, argv []string, env map[string]string, cwd string) ([]*mcp.Tool, error) {
	cmd, err := Command(ctx, argv, env, cwd)
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "toprelay", Version: "0.2.0"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 2 * time.Second}, nil)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", strings.Join(argv, " "), err)
	}
	defer cs.Close()
	return mcpproxy.ListAllTools(ctx, cs)
}
