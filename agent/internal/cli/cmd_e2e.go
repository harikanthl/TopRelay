package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
	"github.com/harikanthl/toprelay/agent/internal/stdiobridge"
)

// errSilent exits non-zero without printing (the command already printed its result).
var errSilent = errors.New("silent failure")

// TestResult is test --json.
type TestResult struct {
	Service      string   `json:"service"`
	Call         string   `json:"call"`
	OK           bool     `json:"ok"`
	Status       int      `json:"status,omitempty"`
	Tools        []string `json:"tools,omitempty"`
	RemoteMs     float64  `json:"remoteMs"`
	LocalMs      float64  `json:"localMs"`
	RelayAddedMs float64  `json:"relayAddedMs"`
	Protocol     string   `json:"protocol,omitempty"`
}

const testRuns = 3

func median(d []time.Duration) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[len(s)/2]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func cmdTest(a *App, args []string) error {
	fs := a.newFlags("test", "test <name>", `
Make one harmless call end to end through the public URL, with a freshly minted bearer key that expires in 5
minutes: tools/list for MCP services, a GET of the first allowed path for http services. Prints the latency, and
the relay's added latency (compared with calling the local service directly). Needs `+"`toprelay up`"+` running.`)
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("test needs a service name")
	}
	cfg, s, err := a.loadService(pos[0])
	if err != nil {
		return err
	}
	base, err := a.relayBase(cfg)
	if err != nil {
		return err
	}
	b, err := a.keyFor(s, nil, a.Now().Add(5*time.Minute))
	if err != nil {
		return err
	}
	auth := map[string]string{"Authorization": "Bearer " + b.String(), "User-Agent": "toprelay-test/" + Version}
	ctx, cancel := a.waitCtx(90 * time.Second)
	defer cancel()
	res := TestResult{Service: s.Name}
	var remote, local []time.Duration
	if s.Kind == config.KindMCP {
		res.Call = "tools/list"
		cs, err := mcpproxy.Connect(ctx, strings.TrimRight(base, "/")+s.PublicPath(), mcpproxy.HTTPClientWithHeaders(nil, auth))
		if err != nil {
			return errf("remote_failed", "run `toprelay doctor` (is `toprelay up` running?)", "through the relay: %v", err)
		}
		defer cs.Close()
		res.Protocol = cs.InitializeResult().ProtocolVersion
		for range testRuns {
			t0 := time.Now()
			tools, err := mcpproxy.ListAllTools(ctx, cs)
			if err != nil {
				return errf("remote_failed", "run `toprelay doctor`", "tools/list through the relay: %v", err)
			}
			remote = append(remote, time.Since(t0))
			res.Tools = mcpproxy.ToolNames(tools)
		}
		lcs, cleanup, err := a.localSession(ctx, s)
		if err != nil {
			return err
		}
		defer cleanup()
		for range testRuns {
			t0 := time.Now()
			if _, err := mcpproxy.ListAllTools(ctx, lcs); err != nil {
				return errf("local_failed", "check the local server", "tools/list on the local server: %v", err)
			}
			local = append(local, time.Since(t0))
		}
		res.OK = true
	} else {
		res.Call = "GET " + s.AllowPaths[0]
		up, err := a.upstreamFor(s)
		if err != nil {
			return err
		}
		get := func(client *http.Client, u string, h map[string]string) (int, time.Duration, error) {
			req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
			if err != nil {
				return 0, 0, err
			}
			for k, v := range h {
				req.Header.Set(k, v)
			}
			t0 := time.Now()
			resp, err := client.Do(req)
			if err != nil {
				return 0, 0, err
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return resp.StatusCode, time.Since(t0), nil
		}
		path := strings.TrimLeft(s.AllowPaths[0], "/")
		for range testRuns {
			st, d, err := get(http.DefaultClient, strings.TrimRight(base, "/")+"/"+s.Name+"/"+path, auth)
			if err != nil {
				return errf("remote_failed", "run `toprelay doctor`", "through the relay: %v", err)
			}
			res.Status = st
			remote = append(remote, d)
			_, ld, err := get(up.Client(), up.Endpoint.Join("/"+path).String(), nil)
			if err != nil {
				return errf("local_failed", "start the service at "+s.Source.URL, "local service: %v", err)
			}
			local = append(local, ld)
		}
		res.OK = res.Status < 500
	}
	rm, lm := median(remote), median(local)
	res.RemoteMs, res.LocalMs = ms(rm), ms(lm)
	res.RelayAddedMs = ms(max(rm-lm, 0))
	err = a.emit(res, func() {
		if res.OK {
			a.say("✓ %s %s through %s\n", s.Name, res.Call, base)
		} else {
			a.say("✗ %s %s through %s: HTTP %d\n", s.Name, res.Call, base, res.Status)
		}
		if res.Tools != nil {
			a.say("  tools: %s\n", strings.Join(res.Tools, ", "))
		}
		if res.Protocol != "" {
			a.say("  protocol: %s\n", res.Protocol)
		}
		a.say("  latency: %.1f ms through the relay, %.1f ms direct → the relay adds %.1f ms (median of %d)\n",
			res.RemoteMs, res.LocalMs, res.RelayAddedMs, testRuns)
	})
	if err == nil && !res.OK {
		return errSilent
	}
	return err
}

// localSession opens an MCP session straight to the local server (a fresh child for stdio).
func (a *App) localSession(ctx context.Context, s *config.Service) (*mcp.ClientSession, func(), error) {
	switch s.Source.Type {
	case config.SourceMCPURL:
		up, err := a.upstreamFor(s)
		if err != nil {
			return nil, nil, err
		}
		cs, err := mcpproxy.Connect(ctx, up.Endpoint.URL.String(), up.Client())
		if err != nil {
			return nil, nil, errf("local_failed", "start the app that serves "+s.Source.URL, "%v", err)
		}
		return cs, func() { cs.Close() }, nil
	default:
		env, err := a.stdioEnv(s)
		if err != nil {
			return nil, nil, err
		}
		cmd, err := stdiobridge.Command(ctx, s.Source.Command, env, s.Source.Cwd)
		if err != nil {
			return nil, nil, err
		}
		c := mcp.NewClient(&mcp.Implementation{Name: "toprelay-test", Version: Version}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
		cs, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 2 * time.Second}, nil)
		if err != nil {
			return nil, nil, errf("local_failed", "check that the command runs on its own", "%v", fmt.Errorf("start %s: %w", strings.Join(s.Source.Command, " "), err))
		}
		return cs, func() { cs.Close() }, nil
	}
}
