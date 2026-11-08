package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/harikanthl/toprelay/agent/internal/caller"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/redact"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/service"
	"github.com/harikanthl/toprelay/agent/internal/upstream"
)

// headerSpec is an upstream header from --header, --header-stdin or --header-file.
type headerSpec struct {
	name  string
	value string // "" for a file
	file  string
	// discouraged is set for --header (the value was on the command line).
	discouraged bool
}

func (h *headerSpec) source() string {
	if h == nil {
		return "none"
	}
	if h.file != "" {
		return "file"
	}
	return "keyring"
}

// parseHeaderLine parses "Name: value".
func parseHeaderLine(line, flag string) (*headerSpec, error) {
	hn, hv, ok := strings.Cut(line, ":")
	hn, hv = strings.TrimSpace(hn), strings.TrimSpace(hv)
	if !ok || hn == "" || hv == "" {
		return nil, usageErr("%s wants \"Name: value\"", flag)
	}
	if err := upstream.ValidHeaderName(hn); err != nil {
		return nil, usageErr("%s: %v", flag, err)
	}
	if !upstream.SafeHeaderValue(hv) {
		return nil, usageErr("%s: the value has control characters or line breaks", flag)
	}
	redact.Add(hv)
	return &headerSpec{name: hn, value: hv}, nil
}

// readStdinLine reads the first line of stdin (at most 8 KiB).
func (a *App) readStdinLine(flag string) (string, error) {
	line, err := a.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", wrap("stdin", "pipe the value in, e.g. `printf 'Name: value\\n' | toprelay …`", err)
	}
	if len(line) > upstream.MaxSecretFile {
		return "", usageErr("%s: the line on stdin is longer than %d bytes", flag, upstream.MaxSecretFile)
	}
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return "", usageErr("%s: nothing on stdin; pipe \"Name: value\" in", flag)
	}
	return line, nil
}

// parseHeaderFile parses "Name=/path" and checks the file can be used now.
func parseHeaderFile(arg, flag string) (*headerSpec, error) {
	hn, p, ok := strings.Cut(arg, "=")
	hn = strings.TrimSpace(hn)
	if !ok || hn == "" || p == "" {
		return nil, usageErr("%s wants Name=/path/to/file", flag)
	}
	if err := upstream.ValidHeaderName(hn); err != nil {
		return nil, usageErr("%s: %v", flag, err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, usageErr("%s: %v", flag, err)
	}
	if _, err := upstream.ReadSecretFile(abs); err != nil {
		return nil, wrap("header_file", "the file must exist, be yours and have mode 0600 (chmod 600 "+abs+")", err)
	}
	return &headerSpec{name: hn, file: abs}, nil
}

// headerFlags turns add's three header flags into one spec (nil when none was given).
func (a *App) headerFlags(header string, stdin bool, file string) (*headerSpec, error) {
	n := 0
	for _, set := range []bool{header != "", stdin, file != ""} {
		if set {
			n++
		}
	}
	switch {
	case n > 1:
		return nil, usageErr("give only one of --header, --header-stdin and --header-file")
	case header != "":
		h, err := parseHeaderLine(header, "--header")
		if err != nil {
			return nil, err
		}
		h.discouraged = true
		return h, nil
	case stdin:
		line, err := a.readStdinLine("--header-stdin")
		if err != nil {
			return nil, err
		}
		return parseHeaderLine(line, "--header-stdin")
	case file != "":
		return parseHeaderFile(file, "--header-file")
	}
	return nil, nil
}

// checkCallerKeyFile checks an app-supplied caller key file.
func checkCallerKeyFile(path string) error {
	if !filepath.IsAbs(path) {
		return usageErr("--caller-key-file wants an absolute path")
	}
	k, err := upstream.ReadSecretFile(path)
	if err != nil {
		return wrap("caller_key", "the file must exist, be yours and have mode 0600", err)
	}
	if len(k) < caller.MinKeyLen {
		return errf("caller_key", "write at least 32 random characters, e.g. `openssl rand -base64 32`", "the key in %s is shorter than %d characters", path, caller.MinKeyLen)
	}
	return nil
}

// upstreamFor builds the way to reach a configured service's app (credential from the keyring or file).
func (a *App) upstreamFor(s *config.Service) (*upstream.Upstream, error) {
	if !s.HasUpstream() {
		return nil, nil
	}
	up, err := service.NewUpstream(*s, a.Dir, a.Store)
	if err != nil {
		return nil, wrap("upstream", "run `toprelay doctor`; set the credential again with `toprelay set-header "+s.Name+" --stdin`", err)
	}
	return up, nil
}

const setHeaderHelp = `
Change the header that carries the app's credential, live: a running ` + "`toprelay up`" + ` uses it for the next
request (within 2 s). Nothing is redeployed and no OAuth grant or bearer key is touched. Exactly one of:

  --stdin             read "Name: value" from the first line of stdin; the value goes to the keyring
  --file Name=/path   read the value from the file on every request (yours, mode 0600); the app can then rotate
                      its token by rewriting the file, without calling TopRelay at all
  --clear             stop sending a credential header

Examples:
  printf 'X-Notes-Token: %s\n' "$TOKEN" | toprelay set-header notes --stdin
  toprelay set-header notes --file X-Notes-Token=$HOME/.notes/token`

// SetHeaderResult is set-header --json.
type SetHeaderResult struct {
	Service string `json:"service"`
	Header  string `json:"header"`
	Updated bool   `json:"updated"`
	// Source is "keyring", "file" or "none".
	Source string `json:"source"`
	File   string `json:"file,omitempty"`
}

func cmdSetHeader(a *App, args []string) error {
	fs := a.newFlags("set-header", "set-header <name> (--stdin | --file Name=/path | --clear)", setHeaderHelp)
	stdin := fs.Bool("stdin", false, "read \"Name: value\" from stdin")
	file := fs.String("file", "", "Name=/path: read the value from this file on every request")
	clear := fs.Bool("clear", false, "send no credential header")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("set-header needs a service name")
	}
	n := 0
	for _, set := range []bool{*stdin, *file != "", *clear} {
		if set {
			n++
		}
	}
	if n != 1 {
		return usageErr("give exactly one of --stdin, --file Name=/path and --clear")
	}
	cfg, s, err := a.loadService(pos[0])
	if err != nil {
		return err
	}
	if !s.HasUpstream() {
		return errf("no_upstream", "give a stdio server its secrets with --env when you add it", "%s is a stdio service: it has no HTTP upstream to send a header to", s.Name)
	}
	var h *headerSpec
	switch {
	case *stdin:
		line, err := a.readStdinLine("--stdin")
		if err != nil {
			return err
		}
		if h, err = parseHeaderLine(line, "--stdin"); err != nil {
			return err
		}
	case *file != "":
		if h, err = parseHeaderFile(*file, "--file"); err != nil {
			return err
		}
	}
	// The keyring is written before the config, so a running `up` that sees the new rev finds the new value.
	if h != nil && h.file == "" {
		if err := a.Store.Set(secrets.UpstreamHeader(s.Name), h.value); err != nil {
			return wrap("keyring", "check keyring access", err)
		}
	}
	if h == nil {
		s.UpstreamHeader, s.UpstreamHeaderFile = "", ""
	} else {
		s.UpstreamHeader, s.UpstreamHeaderFile = h.name, h.file
	}
	s.UpstreamHeaderRev++
	if err := config.Save(a.Dir, cfg); err != nil {
		return wrap("config", "check permissions on "+a.Dir, err)
	}
	if h == nil || h.file != "" {
		// The value no longer lives in the keyring.
		if err := a.Store.Delete(secrets.UpstreamHeader(s.Name)); err != nil {
			a.note("warning: couldn't delete the old value from the keyring: %v\n", err)
		}
	}
	res := SetHeaderResult{Service: s.Name, Updated: true, Source: h.source()}
	if h != nil {
		res.Header, res.File = h.name, h.file
	}
	return a.emit(res, func() {
		switch res.Source {
		case "none":
			a.say("✓ %s no longer sends a credential header\n", s.Name)
		case "file":
			a.say("✓ %s sends %s, read from %s on every request\n", s.Name, res.Header, res.File)
		default:
			a.say("✓ %s sends %s (value in the keyring)\n", s.Name, res.Header)
		}
		a.say("  A running `toprelay up` uses it within 2 s. Nothing was redeployed; keys and OAuth grants still work.\n")
	})
}

const callerKeyHelp = `
Every request the agent relays to the app carries a signed header:

  X-TopRelay-Caller: v1; client=<client id>; ts=<unix s>; nonce=<hex>; sig=<hex HMAC-SHA256>

Only the agent can make it, so the app can trust "this call came from a remote bot, as this client" and give it
fewer powers. The app verifies it with the service's caller key, printed here and kept (mode 0600) at
<config dir>/caller-keys/<name>.key. Verifiers in Go, Swift, Python and Node: MIGRATION.md.

  --rotate          make a new key (the old one stops verifying at once)
  --file PATH       use a key file the app owns instead (e.g. inside a sandboxed app's container; mode 0600,
                    at least 32 characters); the app rotates it by rewriting the file
  --default         go back to the key file TopRelay manages`

// CallerKeyResult is caller-key --json.
type CallerKeyResult struct {
	Service string `json:"service"`
	Header  string `json:"header"`
	Key     string `json:"key"`
	Path    string `json:"path"`
	// Source is "toprelay" (the default file) or "app" (--file).
	Source  string `json:"source"`
	Rotated bool   `json:"rotated"`
}

func cmdCallerKey(a *App, args []string) error {
	fs := a.newFlags("caller-key", "caller-key <name> [--rotate | --file PATH | --default]", callerKeyHelp)
	rotate := fs.Bool("rotate", false, "replace the key with a new one")
	file := fs.String("file", "", "use the app's own key file")
	def := fs.Bool("default", false, "use the key file TopRelay manages")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("caller-key needs a service name")
	}
	n := 0
	for _, set := range []bool{*rotate, *file != "", *def} {
		if set {
			n++
		}
	}
	if n > 1 {
		return usageErr("give at most one of --rotate, --file and --default")
	}
	cfg, s, err := a.loadService(pos[0])
	if err != nil {
		return err
	}
	if !s.HasUpstream() {
		return errf("no_upstream", "", "%s is a stdio service: requests to it are not HTTP, so there is no header to sign", s.Name)
	}
	changed := false
	switch {
	case *file != "":
		if err := checkCallerKeyFile(*file); err != nil {
			return err
		}
		s.CallerKeyFile, changed = *file, true
	case *def:
		s.CallerKeyFile, changed = "", s.CallerKeyFile != ""
	}
	if changed {
		if err := config.Save(a.Dir, cfg); err != nil {
			return wrap("config", "check permissions on "+a.Dir, err)
		}
	}
	res := CallerKeyResult{Service: s.Name, Header: caller.Header, Path: service.CallerKeyPath(a.Dir, *s), Source: "toprelay"}
	if s.CallerKeyFile != "" {
		res.Source = "app"
		if *rotate {
			return errf("app_key", "the app owns "+s.CallerKeyFile+": rotate by rewriting that file", "this service uses the app's own key file")
		}
	} else if *rotate {
		if _, err := caller.WriteKeyFile(res.Path); err != nil {
			return wrap("caller_key", "check permissions on "+filepath.Dir(res.Path), err)
		}
		res.Rotated = true
	} else if _, err := caller.EnsureKeyFile(res.Path); err != nil {
		return wrap("caller_key", "check permissions on "+filepath.Dir(res.Path), err)
	}
	if res.Key, err = upstream.ReadSecretFile(res.Path); err != nil {
		return wrap("caller_key", "chmod 600 "+res.Path, err)
	}
	return a.emit(res, func() {
		if res.Rotated {
			a.say("✓ new caller key for %s; the old one no longer verifies\n", s.Name)
		}
		a.say("Header: %s\nKey:    %s\nFile:   %s\n", res.Header, res.Key, res.Path)
		a.say("Verify it in the app (MIGRATION.md); requests without a valid signature didn't come through TopRelay.\n")
	})
}

// describeHeader is the credential for list output.
func describeHeader(s *config.Service) string {
	switch {
	case s.UpstreamHeader == "":
		return ""
	case s.UpstreamHeaderFile != "":
		return fmt.Sprintf("%s (file %s)", s.UpstreamHeader, s.UpstreamHeaderFile)
	default:
		return s.UpstreamHeader + " (keyring)"
	}
}
