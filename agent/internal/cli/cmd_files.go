package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/files"
)

const filesDirHelp = `
Give an MCP service an outbox for sharing files with bots. When a tool result contains an MCP resource_link
whose uri is toprelay-file:///<name>, and <name> is a file in this directory, the agent replaces the uri with a
signed https link on your relay. It works once and expires in 10 minutes; bots fetch it without OAuth.

The directory must be yours and not writable by others. Paths that leave it (.., symlinks out) are refused.
--off removes the outbox. A running ` + "`toprelay up`" + ` restarts the service to apply it.`

func cmdFilesDir(a *App, args []string) error {
	fs := a.newFlags("files-dir", "files-dir <name> (DIR | --off)", filesDirHelp)
	off := fs.Bool("off", false, "stop sharing files from an outbox")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 || (len(pos) == 2) == *off {
		return usageErr("files-dir needs a service name and either a directory or --off")
	}
	cfg, s, err := a.loadService(pos[0])
	if err != nil {
		return err
	}
	if s.Source.Type != config.SourceMCPURL {
		return errf("not_mcp_url", "", "%s isn't an --mcp-url service; file links work only there", s.Name)
	}
	dir := ""
	if !*off {
		if dir, err = checkFilesDir(pos[1]); err != nil {
			return err
		}
	}
	s.FilesDir = dir
	if err := config.Save(a.Dir, cfg); err != nil {
		return wrap("config", "check permissions on "+a.Dir, err)
	}
	return a.emit(map[string]any{"service": s.Name, "filesDir": dir}, func() {
		if dir == "" {
			a.say("✓ %s no longer shares files from an outbox\n", s.Name)
		} else {
			a.say("✓ %s shares files from %s: return a resource_link with uri toprelay-file:///<name>\n", s.Name, dir)
		}
	})
}

func checkFilesDir(d string) (string, error) {
	abs, err := filepath.Abs(d)
	if err != nil {
		return "", usageErr("bad directory %q", d)
	}
	if err := files.CheckOutbox(abs); err != nil {
		return "", wrap("files_dir", "use a directory you own, e.g. inside your app's support folder", err)
	}
	return abs, nil
}

func cmdDeploy(a *App, args []string) error {
	fs := a.newFlags("deploy", "deploy", `
Redeploy the relay Worker with this agent's version of it, without changing any service, key or OAuth grant.
Run it after updating toprelay so the relay gets new features (such as file links). Connected agents reconnect
by themselves.`)
	if _, _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	a.note("Deploying the relay …\n")
	res, err := a.deployConfig(cfg)
	if err != nil {
		return err
	}
	if err := config.Save(a.Dir, cfg); err != nil {
		return wrap("config", "check permissions on "+a.Dir, err)
	}
	return a.emit(map[string]any{"deployed": true, "publicUrl": cfg.PublicURL, "subdomain": res.Subdomain}, func() {
		a.say("✓ relay deployed: %s\n", cfg.PublicURL)
	})
}

// offerRequest is POST /v1/files on the control socket.
type offerRequest struct {
	Service     string `json:"service"`
	Path        string `json:"path"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	TTLSeconds  int    `json:"ttl"`
}

// offerFile registers a file an app wants to hand to a bot; the app puts the returned uri in a resource_link.
func (c *control) offerFile(w http.ResponseWriter, r *http.Request) {
	var req offerRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil || json.Unmarshal(body, &req) != nil || req.Service == "" || req.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": usageErr(`the body must be {"service","path","name"?,"contentType"?,"ttl"?}`)})
		return
	}
	cfg, err := config.Load(c.a.Dir)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": wrap("config", "", err)})
		return
	}
	s, err := cfg.MustFind(req.Service)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": wrap("no_service", "run `toprelay list`", err)})
		return
	}
	if s.Source.Type != config.SourceMCPURL {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": errf("not_mcp_url", "", "file links work only for --mcp-url services")})
		return
	}
	id, exp, err := c.files.Offer(s.Name, req.Path, req.Name, req.ContentType, time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": wrap("file", "offer a regular file you own, at most 256 MiB", err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": s.Name, "id": id, "uri": "toprelay-file://" + id,
		"expires": exp.Unix()})
}
