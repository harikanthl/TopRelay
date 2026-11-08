// Package cli implements the toprelay commands. cmd/toprelay is a thin wrapper around Main.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/redact"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
)

// Version is the agent version.
const Version = "0.2.0"

// App holds the process's I/O and environment, so tests can drive commands.
type App struct {
	Ctx    context.Context
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	// IsTTY reports whether stdin and stdout are a terminal (prompts allowed).
	IsTTY func() bool
	// ReadSecret reads a line without echo.
	ReadSecret func(prompt string) (string, error)
	Now        func() time.Time

	// Filled by Main from the environment.
	Dir      string
	Store    secrets.Store
	CFBase   string
	RelayURL string
	JSON     bool

	// Test hooks.
	DeployPoll   time.Duration
	DeployHealth *http.Client
	Exe          string
	Home         string
	Installer    func() installer
	// Exit ends the process when a command overruns its --timeout and ignores cancellation (cmd/toprelay sets
	// os.Exit). TimeoutGrace overrides TimeoutGrace (tests).
	Exit         func(int)
	TimeoutGrace time.Duration

	in           *bufio.Reader
	emittedError bool
	// emitted is set once a command printed its result (emit); the watchdog reads it from another goroutine.
	emitted atomic.Bool
	// lingering, after Main, is closed when a command that overran its timeout finally returns (in process only).
	lingering <-chan struct{}
}

// Error is a command failure with a machine code and a fix.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
	// Accounts lists the choices when the code is choose_account, so a GUI can offer a picker.
	Accounts []AccountChoice `json:"accounts,omitempty"`
	usage    bool
}

// AccountChoice is one Cloudflare account a token can see.
type AccountChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (e *Error) Error() string { return e.Message }

func errf(code, fix, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...), Fix: fix}
}

func usageErr(format string, a ...any) *Error {
	return &Error{Code: "usage", Message: fmt.Sprintf(format, a...), Fix: "run `toprelay help` or add --help to the command", usage: true}
}

func wrap(code, fix string, err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: code, Message: err.Error(), Fix: fix}
}

type command struct {
	name    string
	summary string
	run     func(a *App, args []string) error
}

func commands() []command {
	return []command{
		{"login", "store a Cloudflare API token and pick the account", cmdLogin},
		{"add", "expose a local MCP server or HTTP service", cmdAdd},
		{"tools", "show and choose which MCP tools are exposed", cmdTools},
		{"up", "keep every service connected (--install for launchd/systemd)", cmdUp},
		{"status", "connection and today's usage per service", cmdStatus},
		{"list", "list services and their URLs", cmdList},
		{"logs", "show a service's call log", cmdLogs},
		{"key", "mint, narrow or revoke bearer keys", cmdKey},
		{"pair", "make a pairing code for the OAuth consent page", cmdPair},
		{"set-header", "change the app's credential header, live (no redeploy)", cmdSetHeader},
		{"caller-key", "show or rotate the key apps use to verify X-TopRelay-Caller", cmdCallerKey},
		{"limits", "rate limits per service, client and tool (429 + Retry-After)", cmdLimits},
		{"files-dir", "let a service share files with bots from an outbox directory", cmdFilesDir},
		{"deploy", "redeploy the relay Worker with this agent's version (keeps keys and grants)", cmdDeploy},
		{"rotate", "invalidate a service's keys and OAuth grants", cmdRotate},
		{"remove", "remove a service and revoke everything for it", cmdRemove},
		{"doctor", "check the whole setup and say how to fix problems", cmdDoctor},
		{"test", "one harmless call end to end through the public URL", cmdTest},
		{"version", "print the version", cmdVersion},
	}
}

func (a *App) usage(w io.Writer) {
	fmt.Fprintf(w, `toprelay %s: reach local MCP servers and HTTP services from any cloud bot, through a relay on your own
Cloudflare account.

Usage: toprelay <command> [flags]   (add --json to most commands for machine-readable output)

Commands:
`, Version)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
	fmt.Fprint(w, `
Start with:  toprelay login
             toprelay add notes --mcp-url http://127.0.0.1:7879/mcp
             toprelay up

Global flags: --json (one JSON object on stdout, never prompts), --timeout D (default 5m for one-shot commands;
none for up, logs -f and status --watch). In --json mode at most 4 KiB goes to stderr.

Environment: TOPRELAY_CONFIG_DIR, TOPRELAY_KEYRING=file, TOPRELAY_RELAY_URL (see README).
`)
}

// stripJSON removes the global --json flag (before any "--").
func stripJSON(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	found := false
	for i, x := range args {
		if x == "--" {
			out = append(out, args[i:]...)
			break
		}
		if x == "--json" || x == "-json" {
			found = true
			continue
		}
		out = append(out, x)
	}
	return out, found
}

// Main runs the CLI and returns the exit code.
func Main(args []string, a *App) int {
	if a.Ctx == nil {
		a.Ctx = context.Background()
	}
	if a.Now == nil {
		a.Now = time.Now
	}
	if a.Getenv == nil {
		a.Getenv = os.Getenv
	}
	if a.IsTTY == nil {
		a.IsTTY = func() bool { return false }
	}
	if a.Stdin == nil {
		a.Stdin = strings.NewReader("")
	}
	a.in = bufio.NewReader(a.Stdin)
	args, a.JSON = stripJSON(args)
	// Never print a registered secret, on any stream. Stdout is guarded so a timeout error and a late result can't
	// both be printed; in --json mode stderr is capped, so an app that never reads it can't block us.
	out := &guardWriter{w: a.Stdout}
	a.Stdout = redact.Writer{W: out}
	if a.JSON {
		a.Stderr = &capWriter{w: a.Stderr, left: MaxJSONStderr}
	}
	a.Stderr = redact.Writer{W: a.Stderr, Keys: true}
	args, timeout, timeoutSet, terr := stripTimeout(args)
	if terr != nil {
		return a.fail(terr)
	}

	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) > 1 {
			args = []string{args[1], "--help"}
		} else {
			a.usage(a.Stdout)
			return 0
		}
	}
	dir, err := config.Dir(a.Getenv)
	if err != nil {
		return a.fail(wrap("config", "set TOPRELAY_CONFIG_DIR", err))
	}
	if a.Dir == "" {
		a.Dir = dir
	}
	if a.Store == nil {
		a.Store = secrets.Open(a.Getenv, a.Dir)
	}
	if a.CFBase == "" {
		a.CFBase = a.Getenv("TOPRELAY_CF_API_URL")
	}
	if a.RelayURL == "" {
		a.RelayURL = strings.TrimRight(a.Getenv("TOPRELAY_RELAY_URL"), "/")
	}
	for _, c := range commands() {
		if c.name == args[0] {
			d := timeout
			if !timeoutSet {
				d = DefaultTimeout
				if longRunning(c.name, args[1:]) {
					d = 0
				}
			}
			err := a.runBounded(c.name, d, out, func() error { return c.run(a, args[1:]) })
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			if errors.Is(err, errSilent) {
				return 1
			}
			var ec exitCode
			if errors.As(err, &ec) {
				return int(ec)
			}
			if err != nil {
				return a.fail(err)
			}
			return 0
		}
	}
	return a.fail(usageErr("unknown command %q", args[0]))
}

func (a *App) fail(err error) int {
	a.emittedError = true
	e := wrap("error", "", err)
	e.Message = redact.Log(e.Message)
	e.Fix = redact.Log(e.Fix)
	if a.JSON {
		enc := json.NewEncoder(a.Stdout)
		enc.SetEscapeHTML(false)
		enc.Encode(map[string]any{"error": e})
	} else {
		fmt.Fprintf(a.Stderr, "error: %s\n", e.Message)
		if e.Fix != "" {
			fmt.Fprintf(a.Stderr, "  fix: %s\n", e.Fix)
		}
	}
	if e.usage {
		return 2
	}
	return 1
}

// emit prints one JSON object in --json mode, or runs the human printer.
func (a *App) emit(v any, human func()) error {
	defer a.emitted.Store(true)
	if a.JSON {
		enc := json.NewEncoder(a.Stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(v)
	}
	human()
	return nil
}

// say prints human output (suppressed in --json mode).
func (a *App) say(format string, args ...any) {
	if !a.JSON {
		fmt.Fprintf(a.Stdout, format, args...)
	}
}

// note prints progress to stderr (suppressed in --json mode).
func (a *App) note(format string, args ...any) {
	if !a.JSON {
		fmt.Fprintf(a.Stderr, format, args...)
	}
}

func (a *App) interactive() bool { return !a.JSON && a.IsTTY() }

func (a *App) prompt(q string) (string, error) {
	fmt.Fprint(a.Stdout, q)
	line, err := a.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (a *App) confirm(q string) bool {
	ans, err := a.prompt(q + " [y/N] ")
	if err != nil {
		return false
	}
	ans = strings.ToLower(ans)
	return ans == "y" || ans == "yes"
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// newFlags makes a FlagSet with a usage text.
func (a *App) newFlags(name, synopsis, help string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(a.Stderr, "Usage: toprelay %s\n\n%s\n", synopsis, strings.TrimSpace(help))
		var has bool
		fs.VisitAll(func(*flag.Flag) { has = true })
		if has {
			fmt.Fprintln(a.Stderr, "\nFlags:")
			fs.PrintDefaults()
		}
		fmt.Fprintln(a.Stderr, "  -json\n    \tprint one JSON object instead of text")
	}
	return fs
}

// parse parses flags interleaved with positional arguments. Everything after "--" is returned as rest.
func parse(fs *flag.FlagSet, args []string) (pos, rest []string, err error) {
	for i, x := range args {
		if x == "--" {
			rest = args[i+1:]
			args = args[:i]
			break
		}
	}
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, nil, err
			}
			return nil, nil, usageErr("%v", err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, rest, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// parseDuration accepts Go durations plus a "d" (days) suffix.
func parseDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days int
		if _, err := fmt.Sscanf(n, "%d", &days); err != nil || days <= 0 || fmt.Sprint(days) != n {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad duration %q (use e.g. 30m, 1h, 7d)", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return d, nil
}

func (a *App) loadConfig() (*config.Config, error) {
	c, err := config.LoadMigrated(a.Dir, a.Store)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return nil, errf("no_config", "run `toprelay login`, then `toprelay add <name> …`", "no TopRelay configuration in %s", a.Dir)
		}
		return nil, wrap("config", "fix or delete "+config.Path(a.Dir), err)
	}
	return c, nil
}

func (a *App) loadService(name string) (*config.Config, *config.Service, error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return nil, nil, err
	}
	s, err := cfg.MustFind(name)
	if err != nil {
		return nil, nil, wrap("no_service", "run `toprelay list` to see the services", err)
	}
	return cfg, s, nil
}

func cmdVersion(a *App, args []string) error {
	fs := a.newFlags("version", "version", "Print the agent version.")
	if _, _, err := parse(fs, args); err != nil {
		return err
	}
	return a.emit(map[string]string{"version": Version}, func() { a.say("toprelay %s\n", Version) })
}
