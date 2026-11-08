package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pin requires the local app to be signed by an Apple developer team (and optionally to have a bundle id), checked
// against the running process before every new connection. macOS only.
//
// For TCP the process is the one holding the accepting end of the agent's connection (lsof); for a Unix socket it
// is the peer process the kernel reports. Limits, also in the README: a pin proves who is listening, not that the code is
// free of bugs; a process of the same user can still debug or inject into the app unless it uses the hardened
// runtime; and the TCP lookup costs an lsof and a codesign run per new connection (results are cached per process
// for PinCacheTTL; connections are reused).
type Pin struct {
	TeamID   string `json:"teamId"`
	BundleID string `json:"bundleId,omitempty"`

	// run runs a command and returns stdout; tests replace it.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
	mu  sync.Mutex
	ok  map[int]time.Time
}

// PinCacheTTL is how long a verified process stays verified.
const PinCacheTTL = 5 * time.Minute

var (
	teamRE   = regexp.MustCompile(`^[A-Z0-9]{10}$`)
	bundleRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,254}$`)
)

// Validate checks the pin's fields and that this system can enforce it.
func (p *Pin) Validate() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("a code-signature pin needs macOS (this is %s); use a Unix socket upstream instead, which checks the app's user", runtime.GOOS)
	}
	if !teamRE.MatchString(p.TeamID) {
		return fmt.Errorf("team id %q must be 10 capital letters or digits (see `codesign -dv --verbose=2 /Applications/App.app`, TeamIdentifier)", p.TeamID)
	}
	if p.BundleID != "" && !bundleRE.MatchString(p.BundleID) {
		return fmt.Errorf("bundle id %q has characters that aren't allowed", p.BundleID)
	}
	return nil
}

// Requirement is the code-signing requirement the process must satisfy.
func (p *Pin) Requirement() string {
	r := `anchor apple generic and certificate leaf[subject.OU] = "` + p.TeamID + `"`
	if p.BundleID != "" {
		r += ` and identifier "` + p.BundleID + `"`
	}
	return r
}

func (p *Pin) String() string {
	if p.BundleID != "" {
		return p.BundleID + " (team " + p.TeamID + ")"
	}
	return "team " + p.TeamID
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return out.Bytes(), fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return out.Bytes(), nil
}

func (p *Pin) runner() func(ctx context.Context, name string, args ...string) ([]byte, error) {
	if p.run != nil {
		return p.run
	}
	return defaultRun
}

// ErrPin wraps every pin failure.
var ErrPin = errors.New("code-signature pin")

// check verifies one process against the requirement, using the cache.
func (p *Pin) check(ctx context.Context, pid int) error {
	now := time.Now()
	p.mu.Lock()
	if t, ok := p.ok[pid]; ok && now.Sub(t) < PinCacheTTL {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	if err := p.verify(ctx, pid, p.Requirement()); err != nil {
		return fmt.Errorf("%w: the process (pid %d) serving this service is not %s: %v", ErrPin, pid, p, err)
	}
	p.mu.Lock()
	if p.ok == nil {
		p.ok = map[int]time.Time{}
	}
	for k, t := range p.ok {
		if now.Sub(t) >= PinCacheTTL {
			delete(p.ok, k)
		}
	}
	p.ok[pid] = now
	p.mu.Unlock()
	return nil
}

// verify checks a running process against a code requirement (dynamic validation: the code in memory).
func (p *Pin) verify(ctx context.Context, pid int, requirement string) error {
	_, err := p.runner()(ctx, "/usr/bin/codesign", "--verify", "-R="+requirement, strconv.Itoa(pid))
	return err
}

// acceptorPID finds the process holding the other end of our TCP connection: the established socket whose local
// port is the app's port and whose remote port is our connection's local port. Checking the listening sockets
// instead isn't enough: another user can listen on the same port on another address (::1 next to 127.0.0.1), and
// lsof doesn't show other users' processes. A socket we can't see, or one not accepted yet, is retried briefly and
// then refused.
func (p *Pin) acceptorPID(ctx context.Context, localPort, appPort int) (int, error) {
	var last error
	for attempt := 0; attempt < 20; attempt++ {
		if attempt > 0 {
			t := time.NewTimer(25 * time.Millisecond)
			select {
			case <-ctx.Done():
				t.Stop()
				return 0, ctx.Err()
			case <-t.C:
			}
		}
		out, _ := p.runner()(ctx, "/usr/sbin/lsof", "-nP", "-iTCP:"+strconv.Itoa(localPort), "-sTCP:ESTABLISHED", "-F", "pn")
		pids, err := parseAcceptors(out, localPort, appPort)
		if err != nil {
			return 0, err
		}
		switch len(pids) {
		case 1:
			for pid := range pids {
				return pid, nil
			}
		case 0:
			last = fmt.Errorf("%w: can't see which process accepted the connection on port %d (it isn't yours, or it isn't accepting)", ErrPin, appPort)
			continue
		default:
			return 0, fmt.Errorf("%w: several processes hold the connection on port %d", ErrPin, appPort)
		}
	}
	return 0, last
}

// parseAcceptors reads `lsof -F pn` output: "p<pid>" lines, then "n<local>-><remote>" lines for that pid.
func parseAcceptors(out []byte, localPort, appPort int) (map[int]bool, error) {
	port := func(addr string) int {
		i := strings.LastIndexByte(addr, ':')
		if i < 0 {
			return -1
		}
		n, err := strconv.Atoi(addr[i+1:])
		if err != nil {
			return -1
		}
		return n
	}
	pids := map[int]bool{}
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			n, err := strconv.Atoi(line[1:])
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%w: unexpected lsof output %q", ErrPin, line)
			}
			pid = n
		case 'n':
			l, r, ok := strings.Cut(line[1:], "->")
			if ok && pid > 0 && port(l) == appPort && port(r) == localPort {
				pids[pid] = true
			}
		}
	}
	return pids, nil
}
