package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/harikanthl/toprelay/agent/internal/cfapi"
	"github.com/harikanthl/toprelay/agent/internal/config"
	"github.com/harikanthl/toprelay/agent/internal/redact"
	"github.com/harikanthl/toprelay/agent/internal/secrets"
)

// TokenURL opens the dashboard's token page with the right permissions template.
const TokenURL = "https://dash.cloudflare.com/profile/api-tokens?permissionGroupKeys=" +
	`[{"key":"workers_scripts","type":"edit"},{"key":"workers_kv_storage","type":"edit"},{"key":"account_settings","type":"read"}]` +
	"&name=TopRelay&accountId=*&zoneId=all"

func cmdLogin(a *App, args []string) error {
	fs := a.newFlags("login", "login [--token-stdin] [--account ID]", `
Store a Cloudflare API token for deploying the relay into your account.

Create a token at:
  `+TokenURL+`
with these permissions: Account › Workers Scripts › Edit, Workers KV Storage › Edit, Account Settings › Read.
The token is verified, then stored in the OS keyring (never in config.json).`)
	stdin := fs.Bool("token-stdin", false, "read the token from stdin instead of a hidden prompt")
	account := fs.String("account", "", "account id to use when the token sees several accounts")
	if _, _, err := parse(fs, args); err != nil {
		return err
	}
	var token string
	switch {
	case *stdin:
		line, err := a.in.ReadString('\n')
		if err != nil && err != io.EOF {
			return wrap("input", "pipe the token into stdin", err)
		}
		token = strings.TrimSpace(line)
	case a.interactive() && a.ReadSecret != nil:
		a.say("Create a Cloudflare API token here (Workers Scripts Edit, Workers KV Storage Edit, Account Settings Read):\n  %s\n\n", TokenURL)
		t, err := a.ReadSecret("Paste the token (hidden): ")
		if err != nil {
			return wrap("input", "use --token-stdin", err)
		}
		token = strings.TrimSpace(t)
	default:
		return errf("no_tty", "pipe the token in: echo \"$CF_TOKEN\" | toprelay login --token-stdin", "no terminal to prompt for the token")
	}
	if token == "" {
		return errf("empty_token", "create a token at "+TokenURL, "the token is empty")
	}
	redact.Add(token)
	api := cfapi.New(token, a.CFBase)
	st, err := api.VerifyToken(a.Ctx)
	if err != nil {
		return wrap("token_invalid", "create a new token at "+TokenURL, err)
	}
	accts, err := api.ListAccounts(a.Ctx)
	if err != nil {
		return wrap("accounts", "the token needs Account Settings › Read", err)
	}
	if len(accts) == 0 {
		return errf("no_accounts", "give the token access to your account (Account Resources › Include › your account)", "the token can't see any Cloudflare account")
	}
	var pick *cfapi.Account
	switch {
	case *account != "":
		for i := range accts {
			if accts[i].ID == *account {
				pick = &accts[i]
			}
		}
		if pick == nil {
			return errf("bad_account", "use one of: "+accountList(accts), "the token can't see account %s", *account)
		}
	case len(accts) == 1:
		pick = &accts[0]
	case a.interactive():
		a.say("The token sees %d accounts:\n", len(accts))
		for i, ac := range accts {
			a.say("  %d) %s (%s)\n", i+1, ac.Name, ac.ID)
		}
		for pick == nil {
			ans, err := a.prompt("Use which account? ")
			if err != nil {
				return wrap("input", "use --account ID", err)
			}
			n, err := strconv.Atoi(ans)
			if err == nil && n >= 1 && n <= len(accts) {
				pick = &accts[n-1]
			}
		}
	default:
		e := errf("choose_account", "rerun with --account ID, one of: "+accountList(accts), "the token sees %d accounts", len(accts))
		for _, ac := range accts {
			e.Accounts = append(e.Accounts, AccountChoice{ID: ac.ID, Name: ac.Name})
		}
		return e
	}
	if err := a.Store.Set(secrets.CloudflareToken, token); err != nil {
		return wrap("keyring", "set TOPRELAY_KEYRING=file to use a file instead", err)
	}
	cfg, err := config.LoadOrEmpty(a.Dir)
	if err != nil {
		return wrap("config", "fix or delete "+config.Path(a.Dir), err)
	}
	if cfg.AccountID != "" && cfg.AccountID != pick.ID && len(cfg.Services) > 0 {
		a.note("warning: services were deployed to account %s; the next deploy goes to %s\n", cfg.AccountID, pick.ID)
	}
	if cfg.AccountID != pick.ID {
		cfg.Subdomain, cfg.PublicURL, cfg.KVNamespaceID = "", "", ""
	}
	cfg.AccountID, cfg.AccountName = pick.ID, pick.Name
	if err := config.Save(a.Dir, cfg); err != nil {
		return wrap("config", "check permissions on "+a.Dir, err)
	}
	return a.emit(map[string]any{"account": map[string]string{"id": pick.ID, "name": pick.Name}, "tokenStatus": st.Status}, func() {
		a.say("✓ token verified and stored in the keyring\n✓ account: %s (%s)\nNext: toprelay add <name> --mcp-url http://127.0.0.1:<port>/mcp\n", pick.Name, pick.ID)
	})
}

func accountList(accts []cfapi.Account) string {
	var s []string
	for _, a := range accts {
		s = append(s, fmt.Sprintf("%s (%s)", a.ID, a.Name))
	}
	return strings.Join(s, ", ")
}
