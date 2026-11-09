package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
	"github.com/harikanthl/toprelay/agent/internal/names"
	"github.com/harikanthl/toprelay/agent/internal/pins"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
	"github.com/harikanthl/toprelay/agent/internal/shield"
	"github.com/harikanthl/toprelay/agent/internal/stdiobridge"
	"github.com/harikanthl/toprelay/agent/internal/upstream"
)

// ToolView is one tool as shown by `add` and `tools`.
type ToolView struct {
	Name        string   `json:"name"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Allowed     bool     `json:"allowed"`
	ReadOnly    bool     `json:"readOnly"`
	Destructive bool     `json:"destructive"`
	Idempotent  bool     `json:"idempotent"`
	OpenWorld   bool     `json:"openWorld"`
	Warnings    []string `json:"warnings,omitempty"`
	// Held is set when the tool's definition changed since it was approved: it is hidden and refused until
	// `toprelay pins <service> --approve` (Allowed is then false).
	Held bool `json:"held,omitempty"`
	// Findings are passages in the description that try to instruct the model (shield).
	Findings []shield.Finding `json:"findings,omitempty"`
}

// reviewTools pins the listed tools (a tool seen for the first time is pinned as it is; one that changed is
// held), then marks held tools and suspicious descriptions on views. reset first forgets every pin: what the
// owner sees while adding the service is what they approve.
func (a *App) reviewTools(s *config.Service, tools []*mcp.Tool, views []ToolView, reset bool) ([]ToolView, error) {
	st, err := pins.Open(pins.Path(a.Dir, s.Name))
	if err != nil {
		return views, wrap("pins", "delete "+pins.Path(a.Dir, s.Name)+" to pin every tool again as it is now", err)
	}
	if reset {
		if _, err := st.Forget(nil); err != nil {
			return views, wrap("pins", "check permissions on "+a.Dir, err)
		}
	}
	obs := make([]pins.Tool, 0, len(tools))
	for _, t := range tools {
		fp, err := pins.FingerprintTool(t)
		if err != nil {
			continue
		}
		obs = append(obs, pins.Tool{Name: t.Name, Fingerprint: fp, Description: t.Description})
	}
	if _, err := st.Observe(obs); err != nil {
		return views, wrap("pins", "check permissions on "+a.Dir, err)
	}
	scan := s.ShieldConfig().InjectionMode() != shield.InjectionOff
	byName := map[string]*mcp.Tool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	for i := range views {
		v := &views[i]
		if st.IsHeld(v.Name) {
			v.Held, v.Allowed = true, false
			v.Warnings = append(v.Warnings, fmt.Sprintf("tool %q changed since it was approved, so it is held: review it with `toprelay pins %s`", v.Name, s.Name))
		}
		if t := byName[v.Name]; scan && t != nil {
			if fs := shield.ScanText(t.Description); len(fs) > 0 {
				v.Findings = fs
				v.Warnings = append(v.Warnings, fmt.Sprintf("tool %q has text in its description that tries to instruct the model (%s)", v.Name, strings.Join(shield.Rules(fs), ", ")))
			}
		}
	}
	return views, nil
}

func readOnly(t *mcp.Tool) bool { return t.Annotations != nil && t.Annotations.ReadOnlyHint }

func toolViews(service string, tools []*mcp.Tool, p config.ToolPolicy) []ToolView {
	out := make([]ToolView, 0, len(tools))
	for _, t := range tools {
		v := ToolView{Name: t.Name, Title: t.Title, Description: t.Description, Allowed: p.Base(t.Name, hintOf(t)),
			ReadOnly: readOnly(t), Destructive: !readOnly(t), OpenWorld: true, Warnings: names.ToolWarnings(service, t.Name)}
		if an := t.Annotations; an != nil {
			if an.Title != "" && v.Title == "" {
				v.Title = an.Title
			}
			v.Idempotent = an.IdempotentHint
			if an.DestructiveHint != nil && !an.ReadOnlyHint {
				v.Destructive = *an.DestructiveHint
			}
			if an.ReadOnlyHint {
				v.Destructive = false
			}
			if an.OpenWorldHint != nil {
				v.OpenWorld = *an.OpenWorldHint
			}
		}
		out = append(out, v)
	}
	return out
}

// listLocalTools connects to the service's local MCP server and lists its tools. env is the stdio child's
// environment (values from the keyring, or from --env during add).
func (a *App) listLocalTools(s *config.Service, up *upstream.Upstream, env map[string]string) ([]*mcp.Tool, error) {
	ctx, cancel := context.WithTimeout(a.Ctx, 30*time.Second)
	defer cancel()
	switch s.Source.Type {
	case config.SourceMCPURL:
		tools, err := mcpproxy.ListTools(ctx, up.Endpoint.URL.String(), up.Client())
		if err != nil {
			return nil, errf("local_unreachable", "start the app that serves "+s.Source.URL+", then try again", "%v", err)
		}
		return tools, nil
	case config.SourceStdio:
		tools, err := stdiobridge.ListChildTools(ctx, s.Source.Command, env, s.Source.Cwd)
		if err != nil {
			return nil, errf("local_unreachable", "check that the command runs on its own and speaks MCP over stdio", "%v", err)
		}
		return tools, nil
	}
	return nil, errors.New("not an MCP service")
}

// stdioEnv reads a stdio service's environment values from the keyring.
func (a *App) stdioEnv(s *config.Service) (map[string]string, error) {
	env, err := secrets.LoadEnv(a.Store, s.Name, s.Source.EnvNames)
	if err != nil {
		return nil, wrap("keyring", "remove and re-add the service with its --env values", err)
	}
	return env, nil
}

// hintOf reads a tool's annotations as the policy sees them.
func hintOf(t *mcp.Tool) config.Hint {
	h := mcpproxy.HintsFromTools([]*mcp.Tool{t})[0]
	return config.Hint{Known: true, ReadOnly: h.ReadOnly, Destructive: h.Destructive}
}

// defaultPolicy honours MCP annotations: read-only tools are exposed, now and when the server adds more;
// everything else waits for an explicit allow.
func defaultPolicy(tools []*mcp.Tool) config.ToolPolicy {
	p := config.ToolPolicy{Default: config.DefaultReadOnly}
	p.Normalize()
	return p
}
