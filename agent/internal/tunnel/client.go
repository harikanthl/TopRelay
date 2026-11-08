// Package tunnel is the agent side of the relay WebSocket (protocol.md "WebSocket frames" and "Lifecycle").
package tunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/harikanthl/toprelay/agent/internal/reqctx"
)

// CloseReplaced is the close code the relay uses when another connection takes over the service.
const CloseReplaced = 4000

// Hello is what the agent announces in its hello frame.
type Hello struct {
	Kind string
	// AllowAll sends tools:null. Otherwise Tools is the allow-list.
	AllowAll bool
	Tools    []string
}

// State is reported to Options.OnState.
type State struct {
	Connected bool
	Err       error
	// Attempt is the number of consecutive failed connections (0 once connected for a while).
	Attempt int
	// Retry is the delay before the next attempt, when disconnected.
	Retry time.Duration
}

// Options configures a Client.
type Options struct {
	// BaseURL is the relay's public URL (https://… or http://… for wrangler dev).
	BaseURL  string
	Service  string
	AgentKey string
	Agent    string // e.g. "toprelay/0.2.0"
	Handler  http.Handler
	Hello    func() Hello
	// HelloChanged signals that Hello() changed; the client resends hello.
	HelloChanged <-chan struct{}
	Log          *slog.Logger
	OnState      func(State)
	// Files answers internal "file" requests (downloads). When set, hello offers the "files" capability.
	Files http.Handler
	// OnWelcome receives the relay's capabilities after each connect.
	OnWelcome func(caps []string)

	PingInterval     time.Duration // default 30 s
	PongTimeout      time.Duration // default 10 s
	HandshakeTimeout time.Duration // default 20 s
	WakeInterval     time.Duration // default 5 s
	WakeThreshold    time.Duration // default 15 s
	// Now is the wall clock for wake detection; default time.Now.
	Now func() time.Time
	// Jitter returns a random duration in [0, max]; default uniform.
	Jitter func(max time.Duration) time.Duration
	// HTTPClient dials the WebSocket; default http.DefaultClient.
	HTTPClient *http.Client
	after      afterFunc
}

// Client keeps one service connected to the relay.
type Client struct {
	o    Options
	wake chan struct{}
}

// New returns a client with defaults filled in.
func New(o Options) *Client {
	if o.PingInterval <= 0 {
		o.PingInterval = 30 * time.Second
	}
	if o.PongTimeout <= 0 {
		o.PongTimeout = 10 * time.Second
	}
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = 20 * time.Second
	}
	if o.WakeInterval <= 0 {
		o.WakeInterval = 5 * time.Second
	}
	if o.WakeThreshold <= 0 {
		o.WakeThreshold = 15 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Jitter == nil {
		o.Jitter = func(max time.Duration) time.Duration { return rand.N(max + 1) }
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Agent == "" {
		o.Agent = "toprelay/0.2.0"
	}
	if o.Hello == nil {
		o.Hello = func() Hello { return Hello{Kind: "mcp", AllowAll: true} }
	}
	return &Client{o: o, wake: make(chan struct{}, 1)}
}

// WSURL maps the relay base URL to the service's /connect WebSocket URL.
func WSURL(base, service string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return "", fmt.Errorf("bad relay URL %q: %w", base, err)
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("bad relay URL %q: want https:// or http://", base)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + service + "/connect"
	u.RawQuery = ""
	return u.String(), nil
}

// Backoff is the reconnect delay for attempt n: random(0, min(30 s, 0.5 s × 2^n)).
func Backoff(n int, jitter func(time.Duration) time.Duration) time.Duration {
	ceiling := 30 * time.Second
	d := 500 * time.Millisecond << min(max(n, 0), 10)
	return jitter(min(d, ceiling))
}

// WakeDetector reports a wall-clock jump larger than the threshold between ticks.
type WakeDetector struct {
	Interval  time.Duration
	Threshold time.Duration
	last      time.Time
}

// Check takes the wall-clock time of a tick and reports whether the computer probably slept since the last.
func (d *WakeDetector) Check(now time.Time) bool {
	now = now.Round(0) // wall clock only: the monotonic clock may stop during sleep
	defer func() { d.last = now }()
	if d.last.IsZero() {
		return false
	}
	return now.Sub(d.last)-d.Interval > d.Threshold
}

func (c *Client) watchWake(ctx context.Context) {
	det := &WakeDetector{Interval: c.o.WakeInterval, Threshold: c.o.WakeThreshold}
	det.Check(c.o.Now())
	t := time.NewTicker(c.o.WakeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if det.Check(c.o.Now()) {
				c.o.Log.Info("wake from sleep detected", "service", c.o.Service)
				c.Wake()
			}
		}
	}
}

// Wake makes the client reconnect now if it is waiting, or check its socket now if connected.
func (c *Client) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Client) state(s State) {
	if c.o.OnState != nil {
		c.o.OnState(s)
	}
}

// Run keeps the service connected until ctx ends.
func (c *Client) Run(ctx context.Context) error {
	wsURL, err := WSURL(c.o.BaseURL, c.o.Service)
	if err != nil {
		return err
	}
	go c.watchWake(ctx)
	attempt := 0
	for {
		up, err := c.session(ctx, wsURL)
		if ctx.Err() != nil {
			c.state(State{})
			return ctx.Err()
		}
		code := websocket.CloseStatus(err)
		switch {
		case code == CloseReplaced:
			c.o.Log.Warn("relay replaced this connection: another computer connected the same service; retrying with backoff", "service", c.o.Service)
		case err != nil:
			c.o.Log.Warn("relay connection ended", "service", c.o.Service, "err", err)
		}
		if up >= 30*time.Second && code != CloseReplaced {
			attempt = 0
		}
		delay := Backoff(attempt, c.o.Jitter)
		attempt++
		c.state(State{Err: err, Attempt: attempt, Retry: delay})
		c.o.Log.Debug("reconnecting", "service", c.o.Service, "in", delay)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-c.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// DialError explains a failed WebSocket upgrade.
type DialError struct {
	Status int
	Err    error
}

func (e *DialError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "the relay rejected the agent key (the service was rotated or redeployed with other roots): run `toprelay doctor`"
	case http.StatusNotFound:
		return "the relay doesn't know this service (it isn't in the deployed SERVICES): run `toprelay doctor`"
	case 0:
		return fmt.Sprintf("can't reach the relay: %v", e.Err)
	default:
		return fmt.Sprintf("relay refused the connection: HTTP %d", e.Status)
	}
}

func (e *DialError) Unwrap() error { return e.Err }

// WriteTimeout bounds one frame write; a socket that can't take a frame in this time is dropped.
const WriteTimeout = 30 * time.Second

type conn struct {
	ws *websocket.Conn
	// life is the socket's own context. Writes use it, never a request's: coder/websocket closes the whole
	// connection when a write's context ends mid-write, so a cancel for one request would drop every other.
	life context.Context
}

// send writes one frame unless ctx (the request's, or the socket's) has already ended.
func (c *conn) send(ctx context.Context, v any) error {
	if ctx.Err() != nil {
		return errCancelled
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(c.life, WriteTimeout)
	defer cancel()
	return c.ws.Write(wctx, websocket.MessageText, b)
}

type inflight struct {
	cancel context.CancelFunc
}

// session runs one connection and returns how long it was up after welcome.
func (c *Client) session(ctx context.Context, wsURL string) (time.Duration, error) {
	dctx, dcancel := context.WithTimeout(ctx, c.o.HandshakeTimeout)
	defer dcancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+c.o.AgentKey)
	hdr.Set("User-Agent", c.o.Agent)
	ws, resp, err := websocket.Dial(dctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr, HTTPClient: c.o.HTTPClient})
	if err != nil {
		st := 0
		if resp != nil {
			st = resp.StatusCode
		}
		return 0, &DialError{Status: st, Err: err}
	}
	defer ws.CloseNow()
	ws.SetReadLimit(MaxFrame + 1<<16)
	cn := &conn{ws: ws, life: ctx}

	// The first hello carries the current allow-list, so drop change signals that arrived before it.
	if c.o.HelloChanged != nil {
		select {
		case <-c.o.HelloChanged:
		default:
		}
	}
	if err := cn.send(dctx, c.helloFrame()); err != nil {
		return 0, fmt.Errorf("send hello: %w", err)
	}
	for {
		typ, msg, err := ws.Read(dctx)
		if err != nil {
			return 0, fmt.Errorf("waiting for welcome: %w", err)
		}
		if typ != websocket.MessageText || string(msg) == "pong" {
			continue
		}
		var f inFrame
		if json.Unmarshal(msg, &f) == nil && f.Type == "welcome" {
			if c.o.OnWelcome != nil {
				c.o.OnWelcome(f.Caps)
			}
			break
		}
	}
	dcancel()
	start := time.Now()
	c.o.Log.Info("connected to relay", "service", c.o.Service)
	c.state(State{Connected: true})

	sctx, scancel := context.WithCancelCause(ctx)
	defer scancel(nil)
	cn.life = sctx // before any goroutine writes
	pong := make(chan struct{}, 1)
	var wg sync.WaitGroup
	defer wg.Wait()

	wg.Add(2)
	go func() { defer wg.Done(); c.pinger(sctx, scancel, cn, pong) }()
	go func() { defer wg.Done(); c.helloWatcher(sctx, scancel, cn) }()

	var mu sync.Mutex
	pending := map[string]*inflight{}
	defer func() {
		mu.Lock()
		for _, p := range pending {
			p.cancel()
		}
		mu.Unlock()
	}()

	readErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			typ, msg, err := ws.Read(sctx)
			if err != nil {
				readErr <- err
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			if string(msg) == "pong" {
				select {
				case pong <- struct{}{}:
				default:
				}
				continue
			}
			var f inFrame
			if err := json.Unmarshal(msg, &f); err != nil {
				c.o.Log.Warn("ignoring a malformed frame from the relay", "service", c.o.Service)
				continue
			}
			switch f.Type {
			case "request":
				rctx, rcancel := context.WithCancel(sctx)
				inf := &inflight{cancel: rcancel}
				mu.Lock()
				if old, dup := pending[f.ID]; dup {
					old.cancel()
				}
				pending[f.ID] = inf
				mu.Unlock()
				wg.Add(1)
				go func() {
					defer wg.Done()
					c.handle(rctx, cn, f)
					rcancel()
					mu.Lock()
					if pending[f.ID] == inf {
						delete(pending, f.ID)
					}
					mu.Unlock()
				}()
			case "cancel":
				mu.Lock()
				if p, ok := pending[f.ID]; ok {
					p.cancel()
					delete(pending, f.ID)
				}
				mu.Unlock()
			default:
				// welcome repeats and unknown types are ignored (protocol.md: either side can be newer).
			}
		}
	}()

	var result error
	select {
	case err := <-readErr:
		result = err
	case <-sctx.Done():
		result = context.Cause(sctx)
	}
	scancel(result)
	ws.CloseNow()
	return time.Since(start), result
}

func (c *Client) helloFrame() helloFrame {
	h := c.o.Hello()
	f := helloFrame{V: 1, Type: "hello", Agent: c.o.Agent, Caps: []string{}, Kind: h.Kind}
	if c.o.Files != nil {
		f.Caps = append(f.Caps, "files")
	}
	if !h.AllowAll && h.Kind != "http" {
		tools := h.Tools
		if tools == nil {
			tools = []string{}
		}
		f.Tools = &tools
	}
	return f
}

var errPongTimeout = errors.New("no pong from the relay within the timeout")

func (c *Client) pinger(ctx context.Context, fail context.CancelCauseFunc, cn *conn, pong chan struct{}) {
	t := time.NewTicker(c.o.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.wake:
		}
		select {
		case <-pong:
		default:
		}
		wctx, cancel := context.WithTimeout(ctx, c.o.PongTimeout)
		err := cn.ws.Write(wctx, websocket.MessageText, []byte("ping"))
		cancel()
		if err != nil {
			fail(fmt.Errorf("send ping: %w", err))
			return
		}
		pt := time.NewTimer(c.o.PongTimeout)
		select {
		case <-ctx.Done():
			pt.Stop()
			return
		case <-pong:
			pt.Stop()
		case <-pt.C:
			fail(errPongTimeout)
			return
		}
	}
}

func (c *Client) helloWatcher(ctx context.Context, fail context.CancelCauseFunc, cn *conn) {
	if c.o.HelloChanged == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-c.o.HelloChanged:
			if !ok {
				return
			}
			if err := cn.send(ctx, c.helloFrame()); err != nil {
				fail(fmt.Errorf("resend hello: %w", err))
				return
			}
			c.o.Log.Info("allow-list changed; sent hello", "service", c.o.Service)
		}
	}
}

// handle turns a request frame into an http.Request for the local handler and streams the answer back.
func (c *Client) handle(ctx context.Context, out sender, f inFrame) {
	rw := newResponseWriter(ctx, out, f.ID, c.o.after)
	body, err := base64.StdEncoding.DecodeString(f.Body)
	if err != nil {
		rw.header.Set("Content-Type", "text/plain; charset=utf-8")
		rw.WriteHeader(http.StatusBadRequest)
		rw.Write([]byte("toprelay agent: request body is not valid base64\n"))
		rw.finish("")
		return
	}
	path := f.Path
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	u := &url.URL{Scheme: "http", Host: "toprelay.local", Path: path, RawQuery: f.Query}
	if pu, err := url.Parse(path); err == nil {
		u.Path, u.RawPath = pu.Path, pu.RawPath
	}
	if f.Tools != nil {
		ctx = reqctx.WithTools(ctx, *f.Tools)
	}
	method := f.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		rw.finish("")
		return
	}
	for k, v := range f.Headers {
		req.Header.Set(k, v)
	}
	req.ContentLength = int64(len(body))
	if h := f.Headers["x-forwarded-host"]; h != "" {
		req.Host = h
	}
	if ip := f.Headers["cf-connecting-ip"]; ip != "" {
		req.RemoteAddr = ip + ":0"
	}
	failure := ""
	func() {
		defer func() {
			if r := recover(); r != nil {
				if r == http.ErrAbortHandler {
					failure = "upstream closed"
					return
				}
				c.o.Log.Error("handler panic", "service", c.o.Service, "panic", fmt.Sprint(r))
				failure = "internal error"
			}
		}()
		switch f.Internal {
		case "":
			c.o.Handler.ServeHTTP(rw, req)
		case "file":
			if c.o.Files == nil {
				http.Error(rw, "toprelay agent: this agent doesn't share files", http.StatusNotFound)
				return
			}
			c.o.Files.ServeHTTP(rw, req)
		default:
			// An internal kind this agent doesn't know: never let it fall through to the app.
			http.Error(rw, "toprelay agent: unknown internal request", http.StatusNotFound)
		}
	}()
	rw.finish(failure)
}
