package reqctx

import (
	"context"
	"slices"
	"sync"
)

// Findings collects what the shield noticed while one request was answered, for the call log. Handlers may
// add from several goroutines (an SSE stream and its keepalive, a stdio tool handler).
type Findings struct {
	mu       sync.Mutex
	leaks    []string
	blocked  bool
	warnings []string
}

type findingsKey struct{}

// WithFindings attaches a new collector to ctx.
func WithFindings(ctx context.Context) (context.Context, *Findings) {
	f := &Findings{}
	return context.WithValue(ctx, findingsKey{}, f), f
}

// FindingsFrom returns the request's collector, or nil (adding to nil is a no-op).
func FindingsFrom(ctx context.Context) *Findings {
	f, _ := ctx.Value(findingsKey{}).(*Findings)
	return f
}

// AddLeaks records leak rule hits (one entry per match) and whether the answer was withheld.
func (f *Findings) AddLeaks(hits []string, blocked bool) {
	if f == nil || len(hits) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaks = append(f.leaks, hits...)
	f.blocked = f.blocked || blocked
}

// AddWarnings records warnings, each once.
func (f *Findings) AddWarnings(ws ...string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range ws {
		if !slices.Contains(f.warnings, w) {
			f.warnings = append(f.warnings, w)
		}
	}
}

// Snapshot returns the leak hits, whether anything was blocked, and the warnings.
func (f *Findings) Snapshot() (leaks []string, blocked bool, warnings []string) {
	if f == nil {
		return nil, false, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.leaks), f.blocked, slices.Clone(f.warnings)
}
