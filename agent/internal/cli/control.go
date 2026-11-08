package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/daemon"
	"github.com/harikanthl/toprelay/agent/internal/files"
	"github.com/harikanthl/toprelay/agent/internal/upstream"
)

// The control socket lets an app drive TopRelay without starting processes (TopRelayKit). `toprelay up` serves
// it at <config dir>/agent.sock (mode 0600; only connections from this user are accepted). It speaks HTTP/1.x:
//
//	GET  /v1/state         the state.json object
//	GET  /v1/state/watch   one state object per line (NDJSON): now, then on every change
//	POST /v1/cli           {"args":["add","notes",…],"stdin":"…"} → exactly what `toprelay <args> --json` prints;
//	                       HTTP 200 for exit 0, 422 for exit 1, 400 for a usage error (exit 2)
//	POST /v1/files         {"service","path","name"?,"contentType"?,"ttl"?} → {"service","id","uri","expires"}:
//	                       a file the app may return as a resource_link with that uri
//
// Commands run one at a time, in this process, with the same code as the CLI.

// ControlPath is the control socket in the config dir.
func ControlPath(dir string) string { return filepath.Join(dir, "agent.sock") }

// controlCommands may run over the socket. `up` (and every long-running form) is refused.
var controlCommands = []string{"add", "set-header", "caller-key", "files-dir", "deploy", "limits", "tools", "list", "status", "pair",
	"key", "rotate", "remove", "doctor", "test", "version", "login"}

// lockedBuffer is a bytes.Buffer safe for one writer and one reader.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// MaxControlBody caps a /v1/cli request.
const MaxControlBody = 1 << 20

type control struct {
	a     *App
	state *daemon.StateFile
	files *files.Registry
	log   *slog.Logger
	mu    sync.Mutex
}

// ErrAgentRunning means another `toprelay up` answers on the control socket.
var ErrAgentRunning = errors.New("another `toprelay up` is already running for this config dir")

// listenControl claims the control socket. A socket nobody answers on is stale and is replaced.
func listenControl(dir string) (net.Listener, error) {
	path := ControlPath(dir)
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return nil, ErrAgentRunning
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		os.Remove(path)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return &peerListener{Listener: l}, nil
}

// peerListener drops connections from other users.
type peerListener struct{ net.Listener }

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			c.Close()
			continue
		}
		p, err := upstream.PeerOf(uc)
		if err != nil || p.UID != os.Getuid() {
			c.Close()
			continue
		}
		return c, nil
	}
}

func (c *control) serve(ctx context.Context, l net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/state", c.getState)
	mux.HandleFunc("GET /v1/state/watch", c.watchState)
	mux.HandleFunc("POST /v1/cli", c.runCLI)
	mux.HandleFunc("POST /v1/files", c.offerFile)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		c.log.Error("control socket stopped", "err", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func (c *control) getState(w http.ResponseWriter, r *http.Request) {
	s := c.state.Snapshot()
	writeJSON(w, http.StatusOK, s)
}

func (c *control) watchState(w http.ResponseWriter, r *http.Request) {
	ch, unsub := c.state.Subscribe()
	defer unsub()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	last := ""
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		s := c.state.Snapshot()
		key := s
		key.UpdatedAt = time.Time{}
		kb, _ := json.Marshal(key)
		if string(kb) != last {
			last = string(kb)
			b, _ := json.Marshal(s)
			if _, err := w.Write(append(b, '\n')); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-ch:
		case <-ping.C:
			// An empty line keeps idle connections from looking dead; readers skip it.
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

type cliRequest struct {
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
}

func (c *control) runCLI(w http.ResponseWriter, r *http.Request) {
	var req cliRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxControlBody+1))
	if err != nil || len(body) > MaxControlBody || json.Unmarshal(body, &req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": usageErr("the body must be {\"args\":[…],\"stdin\":\"…\"} under 1 MiB")})
		return
	}
	args := slices.DeleteFunc(slices.Clone(req.Args), func(s string) bool { return s == "--json" || s == "-json" })
	if len(args) == 0 || !slices.Contains(controlCommands, args[0]) || longRunning(args[0], args[1:]) {
		name := ""
		if len(args) > 0 {
			name = args[0]
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": usageErr("%q can't run over the control socket (allowed: %s)", name, strings.Join(controlCommands, ", "))})
		return
	}
	c.mu.Lock()
	unlock := true
	defer func() {
		if unlock {
			c.mu.Unlock()
		}
	}()
	var out bytes.Buffer
	var errb lockedBuffer
	sub := &App{Ctx: r.Context(), Stdin: strings.NewReader(req.Stdin), Stdout: &out, Stderr: &errb, Getenv: c.a.Getenv,
		IsTTY: func() bool { return false }, Now: c.a.Now, Dir: c.a.Dir, Store: c.a.Store, CFBase: c.a.CFBase,
		RelayURL: c.a.RelayURL, DeployPoll: c.a.DeployPoll, DeployHealth: c.a.DeployHealth, Exe: c.a.Exe, Home: c.a.Home,
		Installer: c.a.Installer}
	// No Exit hook: a stuck command must never take the agent down; its timeout error is returned instead.
	code := Main(append([]string{"--json"}, args...), sub)
	if sub.lingering != nil {
		// It overran its timeout and is still running: hold the lock until it ends, so the next command can't
		// race it on the config file. Its stderr is still being written; leave it alone.
		unlock = false
		go func() {
			<-sub.lingering
			c.mu.Unlock()
		}()
		c.log.Warn("control command overran its timeout; later commands wait for it", "command", args[0])
	} else if s := errb.String(); s != "" {
		c.log.Debug("control command stderr", "command", args[0], "stderr", s)
	}
	status := http.StatusOK
	switch code {
	case 0:
	case 2:
		status = http.StatusBadRequest
	default:
		status = http.StatusUnprocessableEntity
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(out.Bytes())
}
