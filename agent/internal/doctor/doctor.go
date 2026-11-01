// Package doctor runs the `toprelay doctor` checks, each with a clear fix.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/harikanthl/toprelay/agent/internal/cfapi"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/deploy"
	"github.com/harikanthl/toprelay/agent/internal/keys"
	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
	"github.com/harikanthl/toprelay/agent/internal/redact"
	"github.com/harikanthl/toprelay/agent/internal/relayapi"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/stdiobridge"
)

// Status of a check.
const (
	OK   = "ok"
	Warn = "warn"
	Fail = "fail"
	Skip = "skip"
)

// Check is one result.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// Report is the whole run.
type Report struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// Env is what the checks need.
type Env struct {
	Dir      string
	Store    secrets.Store
	CFBase   string
	RelayURL string // TOPRELAY_RELAY_URL override
	Now      func() time.Time
	// Resolve looks up a host (DNS); nil uses net.DefaultResolver.
	Resolve func(ctx context.Context, host string) error
	// MaxSkew is the clock skew that triggers a warning (default 30 s).
	MaxSkew time.Duration
}

type runner struct {
	r Report
}

func (r *runner) add(c Check) {
	c.Detail = redact.Log(c.Detail)
	r.r.Checks = append(r.r.Checks, c)
}

// Run runs every check in order; later checks are skipped when their prerequisites fail.
func Run(ctx context.Context, e Env) Report {
	rep := run(ctx, e)
	rep.OK = !slices.ContainsFunc(rep.Checks, func(c Check) bool { return c.Status == Fail })
	return rep
}

func run(ctx context.Context, e Env) Report {
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.MaxSkew <= 0 {
		e.MaxSkew = 30 * time.Second
	}
	r := &runner{}

	cfg, err := config.Load(e.Dir)
	if err != nil {
		r.add(Check{Name: "config", Status: Fail, Detail: err.Error(), Fix: "run `toprelay login`, then `toprelay add <name> …`"})
		return r.r
	}
	r.add(Check{Name: "config", Status: OK, Detail: fmt.Sprintf("%s (%d services)", config.Path(e.Dir), len(cfg.Services))})

	// Cloudflare token and permissions.
	token, err := e.Store.Get(secrets.CloudflareToken)
	var api *cfapi.Client
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		r.add(Check{Name: "cloudflare token", Status: Fail, Detail: "no token stored", Fix: "run `toprelay login`"})
	case err != nil:
		r.add(Check{Name: "cloudflare token", Status: Fail, Detail: err.Error(), Fix: "check keyring access, or set TOPRELAY_KEYRING=file"})
	default:
		api = cfapi.New(token, e.CFBase)
		api.MaxRetries = 1
		if _, err := api.VerifyToken(ctx); err != nil {
			r.add(Check{Name: "cloudflare token", Status: Fail, Detail: err.Error(), Fix: "create a new token and run `toprelay login`"})
			api = nil
		} else {
			r.add(Check{Name: "cloudflare token", Status: OK, Detail: "active"})
		}
	}
	if api != nil {
		if cfg.AccountID == "" {
			r.add(Check{Name: "cloudflare permissions", Status: Fail, Detail: "no account selected", Fix: "run `toprelay login`"})
		} else {
			var missing []string
			if _, err := api.ListAccounts(ctx); err != nil {
				missing = append(missing, "Account Settings › Read")
			}
			if _, err := api.ListKVNamespaces(ctx, cfg.AccountID); err != nil {
				missing = append(missing, "Workers KV Storage › Edit")
			}
			if _, err := api.ListScripts(ctx, cfg.AccountID); err != nil {
				missing = append(missing, "Workers Scripts › Edit")
			}
			if len(missing) > 0 {
				r.add(Check{Name: "cloudflare permissions", Status: Fail, Detail: "missing: " + strings.Join(missing, ", "),
					Fix: "create a token with Account › Workers Scripts › Edit, Workers KV Storage › Edit, Account Settings › Read, then `toprelay login`"})
			} else {
				r.add(Check{Name: "cloudflare permissions", Status: OK, Detail: "read access confirmed for accounts, KV and Workers (edit access is only proven by a deploy)"})
			}
		}
	} else {
		r.add(Check{Name: "cloudflare permissions", Status: Skip, Detail: "needs a valid token"})
	}

	// Roots.
	agentRoot, aerr := e.Store.Get(secrets.AgentRoot)
	_, berr := e.Store.Get(secrets.BearerRoot)
	rootsOK := aerr == nil && berr == nil
	if rootsOK {
		if _, err := keys.DecodeRoot(agentRoot); err != nil {
			rootsOK = false
			aerr = err
		}
	}
	switch {
	case rootsOK:
		r.add(Check{Name: "root secrets", Status: OK, Detail: "AGENT_ROOT and BEARER_ROOT present"})
	case len(cfg.Services) == 0:
		r.add(Check{Name: "root secrets", Status: Skip, Detail: "created with the first `toprelay add`"})
	default:
		r.add(Check{Name: "root secrets", Status: Fail, Detail: fmt.Sprintf("agent root: %v; bearer root: %v", errText(aerr), errText(berr)),
			Fix: "the keyring lost them: copy them from the computer that ran `toprelay add`, or remove and re-add every service"})
	}

	// Worker deployed.
	if api != nil && cfg.AccountID != "" {
		exists, err := api.ScriptExists(ctx, cfg.AccountID, deploy.ScriptName)
		switch {
		case err != nil:
			r.add(Check{Name: "worker deployed", Status: Fail, Detail: err.Error(), Fix: "check the token's Workers Scripts permission"})
		case !exists:
			r.add(Check{Name: "worker deployed", Status: Fail, Detail: "no `toprelay` Worker in the account", Fix: "run `toprelay add <name> …` to deploy it"})
		default:
			r.add(Check{Name: "worker deployed", Status: OK, Detail: "script `toprelay` exists"})
		}
	} else {
		r.add(Check{Name: "worker deployed", Status: Skip, Detail: "needs a valid token and account"})
	}

	// Subdomain reachable, /health and clock skew.
	base := cfg.PublicURL
	if e.RelayURL != "" {
		base = e.RelayURL
	}
	var relay *relayapi.Client
	if base == "" {
		r.add(Check{Name: "relay reachable", Status: Fail, Detail: "no public URL yet", Fix: "run `toprelay add <name> …` to deploy the relay"})
	} else {
		u, _ := url.Parse(base)
		resolve := e.Resolve
		if resolve == nil {
			resolve = func(ctx context.Context, host string) error {
				_, err := net.DefaultResolver.LookupHost(ctx, host)
				return err
			}
		}
		if u != nil && e.RelayURL == "" {
			if err := resolve(ctx, u.Hostname()); err != nil {
				r.add(Check{Name: "subdomain DNS", Status: Fail, Detail: err.Error(), Fix: "new workers.dev subdomains can take a few minutes; try again shortly"})
			} else {
				r.add(Check{Name: "subdomain DNS", Status: OK, Detail: u.Hostname()})
			}
		}
		relay = relayapi.New(base)
		sent := e.Now()
		date, err := relay.Health(ctx)
		if err != nil {
			r.add(Check{Name: "relay reachable", Status: Fail, Detail: err.Error(), Fix: "run `toprelay doctor` again in a minute; if it persists, redeploy with `toprelay rotate <name>`"})
			relay = nil
		} else {
			r.add(Check{Name: "relay reachable", Status: OK, Detail: base + "/health answered 200"})
			if date.IsZero() {
				r.add(Check{Name: "clock skew", Status: Skip, Detail: "the relay sent no Date header"})
			} else {
				skew := sent.Sub(date)
				if skew < 0 {
					skew = -skew
				}
				// Date has 1 s resolution; allow for that.
				if skew > e.MaxSkew+time.Second {
					r.add(Check{Name: "clock skew", Status: Warn, Detail: fmt.Sprintf("this computer's clock is %s off the relay's", skew.Round(time.Second)),
						Fix: "turn on automatic time setting; bearer key expiry (--for) depends on it"})
				} else {
					r.add(Check{Name: "clock skew", Status: OK, Detail: fmt.Sprintf("within %s", skew.Round(time.Second)+time.Second)})
				}
			}
		}
	}

	// Each local service, and the relay's view of its agent.
	for _, s := range cfg.Services {
		r.localCheck(ctx, e, s)
		name := "agent connected: " + s.Name
		switch {
		case relay == nil || !rootsOK:
			r.add(Check{Name: name, Status: Skip, Detail: "needs a reachable relay and the root secrets"})
		default:
			key, _ := keys.AgentKey(agentRoot, s.Name, s.Gen)
			st, err := relay.Status(ctx, s.Name, key)
			switch {
			case err != nil:
				r.add(Check{Name: name, Status: Fail, Detail: err.Error(), Fix: "if the key is rejected, the deployed Worker has other roots or gen: run `toprelay rotate " + s.Name + "`"})
			case !st.Connected:
				r.add(Check{Name: name, Status: Fail, Detail: "the relay has no socket from this service", Fix: "run `toprelay up` (or `toprelay up --install` to keep it running)"})
			default:
				r.add(Check{Name: name, Status: OK, Detail: fmt.Sprintf("connected since %s (%s)", time.Unix(st.ConnectedAt, 0).Format(time.RFC3339), st.Agent)})
			}
		}
	}
	return r.r
}

func errText(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, secrets.ErrNotFound) {
		return "missing"
	}
	return err.Error()
}

func (r *runner) localCheck(ctx context.Context, e Env, s config.Service) {
	name := "local service: " + s.Name
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	headers := map[string]string{}
	if s.UpstreamHeader != "" {
		v, err := e.Store.Get(secrets.UpstreamHeader(s.Name))
		if err != nil {
			r.add(Check{Name: name, Status: Fail, Detail: "upstream header value missing from the keyring", Fix: "remove and re-add the service with --header"})
			return
		}
		headers[s.UpstreamHeader] = v
	}
	switch s.Source.Type {
	case config.SourceMCPURL:
		tools, err := mcpproxy.ListTools(ctx, s.Source.URL, headers)
		if err != nil {
			r.add(Check{Name: name, Status: Fail, Detail: err.Error(), Fix: "start the app that serves " + s.Source.URL})
			return
		}
		r.add(Check{Name: name, Status: OK, Detail: toolSummary(tools, s.Tools)})
	case config.SourceStdio:
		tools, err := stdiobridge.ListChildTools(ctx, s.Source.Command, s.Source.Env, s.Source.Cwd)
		if err != nil {
			r.add(Check{Name: name, Status: Fail, Detail: err.Error(), Fix: "check the command runs on its own: " + strings.Join(s.Source.Command, " ")})
			return
		}
		r.add(Check{Name: name, Status: OK, Detail: toolSummary(tools, s.Tools)})
	case config.SourceHTTP:
		d := net.Dialer{Timeout: 5 * time.Second}
		u, err := url.Parse(s.Source.URL)
		if err != nil {
			r.add(Check{Name: name, Status: Fail, Detail: err.Error(), Fix: "re-add the service with a valid --http URL"})
			return
		}
		host := u.Host
		if u.Port() == "" {
			if u.Scheme == "https" {
				host += ":443"
			} else {
				host += ":80"
			}
		}
		c, err := d.DialContext(ctx, "tcp", host)
		if err != nil {
			r.add(Check{Name: name, Status: Fail, Detail: err.Error(), Fix: "start the service at " + s.Source.URL})
			return
		}
		c.Close()
		r.add(Check{Name: name, Status: OK, Detail: s.Source.URL + " accepts connections"})
	}
}

func toolSummary(tools []*mcp.Tool, p config.ToolPolicy) string {
	names := mcpproxy.ToolNames(tools)
	allowed := p.AllowList(names)
	return fmt.Sprintf("%d tools, %d exposed: %s", len(names), len(allowed), strings.Join(allowed, ", "))
}
