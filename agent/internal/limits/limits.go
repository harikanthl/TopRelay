// Package limits enforces a service's own rate limits in the agent: per service, per client and per tool, on top
// of the relay's fixed per-service limit. Over a limit, the call gets 429 with Retry-After and never reaches the app.
package limits

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
)

// Rate is n requests per period, with a burst of n.
type Rate struct {
	N      int
	Period time.Duration
}

// ParseRate reads "60/m", "10/s", "1000/h" or "500/d".
func ParseRate(s string) (Rate, error) {
	n, unit, ok := strings.Cut(strings.TrimSpace(s), "/")
	count, err := strconv.Atoi(n)
	if !ok || err != nil || count <= 0 || count > 1_000_000 {
		return Rate{}, fmt.Errorf("bad rate %q: use N/s, N/m, N/h or N/d, e.g. 60/m", s)
	}
	periods := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}
	p, ok := periods[unit]
	if !ok {
		return Rate{}, fmt.Errorf("bad rate %q: the unit is s, m, h or d", s)
	}
	return Rate{N: count, Period: p}, nil
}

func (r Rate) String() string {
	for u, p := range map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour} {
		if r.Period == p {
			return fmt.Sprintf("%d/%s", r.N, u)
		}
	}
	return fmt.Sprintf("%d/%s", r.N, r.Period)
}

// Config is a service's limits (config.Limits).
type Config = config.Limits

type compiled struct {
	service, client *Rate
	tools           map[string]Rate
}

func compile(c Config) (compiled, error) {
	var out compiled
	if c.Service != "" {
		r, err := ParseRate(c.Service)
		if err != nil {
			return out, err
		}
		out.service = &r
	}
	if c.Client != "" {
		r, err := ParseRate(c.Client)
		if err != nil {
			return out, err
		}
		out.client = &r
	}
	for t, v := range c.Tools {
		r, err := ParseRate(v)
		if err != nil {
			return out, fmt.Errorf("tool %s: %w", t, err)
		}
		if out.tools == nil {
			out.tools = map[string]Rate{}
		}
		out.tools[t] = r
	}
	return out, nil
}

// Validate checks a config.
func Validate(c Config) error {
	_, err := compile(c)
	return err
}

// gcra holds a generic cell rate limiter's state: one "theoretical arrival time" per key.
type gcra struct {
	mu  sync.Mutex
	tat map[string]time.Time
}

// Limiter is one service's limits, swappable live.
type Limiter struct {
	cfg atomic.Pointer[compiled]
	g   gcra
	Now func() time.Time
}

// New makes a limiter.
func New(c Config) (*Limiter, error) {
	l := &Limiter{Now: time.Now}
	return l, l.Set(c)
}

// Set replaces the limits (counters for unchanged keys carry over).
func (l *Limiter) Set(c Config) error {
	cc, err := compile(c)
	if err != nil {
		return err
	}
	l.cfg.Store(&cc)
	return nil
}

// Check admits a request from client, calling tools (none for a non-tool request). Every limit that applies must
// have room; when one doesn't, nothing is charged and the wait is returned.
func (l *Limiter) Check(client string, tools []string) (bool, time.Duration, string) {
	c := l.cfg.Load()
	now := l.Now()
	type want struct {
		key, what string
		r         Rate
	}
	var ws []want
	if c.service != nil {
		ws = append(ws, want{"s", "this service", *c.service})
	}
	if c.client != nil {
		ws = append(ws, want{"c:" + client, "this client", *c.client})
	}
	for _, t := range tools {
		if r, ok := c.tools[t]; ok {
			ws = append(ws, want{"t:" + t, "the tool " + t, r})
		}
	}
	// Check and charge under one lock, against a working copy of the arrival times: concurrent calls can't both
	// pass, a batch calling one tool twice is charged twice, and a refused call charges nothing.
	l.g.mu.Lock()
	defer l.g.mu.Unlock()
	if l.g.tat == nil {
		l.g.tat = map[string]time.Time{}
	}
	pending := map[string]time.Time{}
	for _, w := range ws {
		tat, ok := pending[w.key]
		if !ok {
			tat = l.g.tat[w.key]
		}
		if tat.Before(now) {
			tat = now
		}
		interval := w.r.Period / time.Duration(w.r.N)
		if allowAt := tat.Add(-(w.r.Period - interval)); now.Before(allowAt) {
			return false, allowAt.Sub(now), w.what
		}
		pending[w.key] = tat.Add(interval)
	}
	for k, t := range pending {
		l.g.tat[k] = t
	}
	if len(l.g.tat) > 10_000 {
		for k, t := range l.g.tat {
			if t.Before(now) {
				delete(l.g.tat, k)
			}
		}
	}
	return true, 0, ""
}

// Middleware refuses calls over a limit with 429 and Retry-After, before they reach next.
func (l *Limiter) Middleware(service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var tools []string
		var id json.RawMessage
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/mcp") && r.Body != nil {
			body, err := io.ReadAll(io.LimitReader(r.Body, mcpproxy.MaxBody+1))
			if err != nil {
				http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if msgs, _, err := mcpproxy.Parse(body); err == nil {
				for _, m := range msgs {
					if m.Method == "tools/call" {
						tools = append(tools, m.ToolName())
					}
				}
				if len(msgs) == 1 {
					id = msgs[0].ID
				}
			}
		}
		ok, wait, what := l.Check(r.Header.Get("X-Toprelay-Client"), tools)
		if ok {
			next.ServeHTTP(w, r)
			return
		}
		secs := int(math.Ceil(wait.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		w.Header().Set(mcpproxy.ReasonHeader, mcpproxy.ReasonRateLimited)
		msg := fmt.Sprintf("%s: too many calls for %s; try again in %d s.", service, what, secs)
		if strings.HasPrefix(r.URL.Path, "/mcp") {
			if len(id) == 0 {
				id = json.RawMessage("null")
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32000, "message": msg,
				"data": map[string]any{"retryAfter": secs}}})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write(b)
			return
		}
		http.Error(w, msg, http.StatusTooManyRequests)
	})
}
