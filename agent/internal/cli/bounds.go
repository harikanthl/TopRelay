package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout bounds every one-shot command. A deploy (first `add`) is the slowest: a new workers.dev subdomain
// can take a minute or two to answer.
const DefaultTimeout = 5 * time.Minute

// TimeoutGrace is how long a command gets, after its deadline cancels its context, to return on its own before
// the process prints a timeout error and exits anyway.
const TimeoutGrace = 5 * time.Second

// MaxJSONStderr caps what reaches stderr in --json mode, so an app that never reads stderr can't make toprelay
// block on a full pipe.
const MaxJSONStderr = 4 << 10

// longRunning reports whether a command runs until stopped, so the default timeout doesn't apply (an explicit
// --timeout still does).
func longRunning(cmd string, args []string) bool {
	switch cmd {
	case "up":
		return !boolFlag(args, "install") && !boolFlag(args, "uninstall")
	case "logs":
		return boolFlag(args, "f")
	case "status":
		return boolFlag(args, "watch")
	}
	return false
}

// boolFlag reports whether a boolean flag is set the way the flag package reads it: -name, --name, or
// -name=<anything strconv.ParseBool accepts as true>. A value it can't parse counts as set (the command would
// refuse it anyway), so the answer errs towards "long-running".
func boolFlag(args []string, name string) bool {
	set := false
	for _, a := range args {
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		k, v, hasV := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if k != name {
			continue
		}
		if !hasV {
			set = true
			continue
		}
		b, err := strconv.ParseBool(v)
		set = b || err != nil
	}
	return set
}

// stripTimeout removes the global --timeout flag (before any "--").
func stripTimeout(args []string) ([]string, time.Duration, bool, error) {
	out := make([]string, 0, len(args))
	var d time.Duration
	set := false
	for i := 0; i < len(args); i++ {
		x := args[i]
		if x == "--" {
			out = append(out, args[i:]...)
			break
		}
		var v string
		switch {
		case x == "--timeout" || x == "-timeout":
			if i+1 >= len(args) {
				return nil, 0, false, usageErr("--timeout needs a duration, e.g. --timeout 30s")
			}
			v = args[i+1]
			i++
		case strings.HasPrefix(x, "--timeout=") || strings.HasPrefix(x, "-timeout="):
			v = x[strings.IndexByte(x, '=')+1:]
		default:
			out = append(out, x)
			continue
		}
		pd, err := parseDuration(v)
		if err != nil {
			return nil, 0, false, usageErr("--timeout: %v", err)
		}
		d, set = pd, true
	}
	return out, d, set, nil
}

// guardWriter serialises writes to stdout and can be sealed, so the watchdog's error and a late write from the
// command never both reach the caller.
type guardWriter struct {
	mu      sync.Mutex
	w       io.Writer
	sealed  bool
	written bool
}

func (g *guardWriter) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return len(p), nil
	}
	g.written = g.written || len(p) > 0
	return g.w.Write(p)
}

func (g *guardWriter) wrote() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.written
}

// seal stops further writes and reports whether anything was written before.
func (g *guardWriter) seal(final func(io.Writer)) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	was := g.written
	if !g.sealed && !was && final != nil {
		final(g.w)
	}
	g.sealed = true
	return was
}

// capWriter passes through the first n bytes, then one notice, then nothing.
type capWriter struct {
	mu   sync.Mutex
	w    io.Writer
	left int
	note bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if c.left > 0 {
		k := min(len(p), c.left)
		c.w.Write(p[:k])
		c.left -= k
		p = p[k:]
	}
	if len(p) > 0 && !c.note {
		c.note = true
		io.WriteString(c.w, "\n…(stderr truncated in --json mode)\n")
	}
	return n, nil
}

var errTimeout = errors.New("timeout")

// runBounded runs fn with a deadline. When the deadline passes, the context is cancelled; if fn hasn't returned
// TimeoutGrace later, the timeout error is printed (unless the command already printed its result) and the
// process exits through a.Exit.
func (a *App) runBounded(cmd string, d time.Duration, out *guardWriter, fn func() error) error {
	if d <= 0 {
		return fn()
	}
	ctx, cancel := context.WithTimeoutCause(a.Ctx, d, errTimeout)
	defer cancel()
	a.Ctx = ctx // before the goroutine starts: the command reads it
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() { done <- fn(); close(finished) }()
	grace := a.TimeoutGrace
	if grace <= 0 {
		grace = TimeoutGrace
	}
	timeoutErr := func() *Error {
		return errf("timeout", "raise it with --timeout, or run `toprelay doctor` to find what is slow",
			"toprelay %s didn't finish within %s", cmd, d)
	}
	// A command that already printed its result (doctor and test print a report, then return errSilent) keeps
	// its own error: replacing it would print a second JSON object.
	asTimeout := func(err error) error {
		if err != nil && !out.wrote() {
			return timeoutErr()
		}
		return err
	}
	select {
	case err := <-done:
		if errors.Is(context.Cause(ctx), errTimeout) {
			return asTimeout(err)
		}
		return err
	case <-ctx.Done():
	}
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case err := <-done:
		return asTimeout(err)
	case <-t.C:
	}
	// The command ignores its context: answer for it and leave.
	wrote := out.seal(func(w io.Writer) {
		if a.JSON {
			fmt.Fprintf(w, `{"error":{"code":"timeout","message":%q,"fix":%q}}`+"\n", timeoutErr().Message, timeoutErr().Fix)
		}
	})
	if !a.JSON {
		fmt.Fprintf(a.Stderr, "error: %s\n", timeoutErr().Message)
	}
	code := 1
	if wrote && a.emitted.Load() {
		code = 0 // the result was already printed; only the cleanup hung
	}
	if a.Exit == nil {
		// In process (the control socket): the command is still running. The caller must not start another one
		// until it ends.
		a.lingering = finished
		return exitCode(code)
	}
	a.exit(code)
	return exitCode(code)
}

// exitCode carries a process exit code out of a command that can't print anything more.
type exitCode int

func (e exitCode) Error() string { return "exit " + strconv.Itoa(int(e)) }

func (a *App) exit(code int) {
	if a.Exit != nil {
		a.Exit(code)
	}
}
