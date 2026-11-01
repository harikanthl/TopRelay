# toprelay agent

The laptop side of TopRelay: one Go binary that deploys the relay Worker into your own Cloudflare account and
keeps local MCP servers and HTTP services reachable through it. The wire contract is `../docs/protocol.md`.

**Security, plainly:** the relay terminates TLS, so traffic is not end-to-end encrypted from the bot to your app:
it passes through Cloudflare on your own account. The relay logs nothing and stores only OAuth grants. `--stdio`
servers run with your user's permissions: an allowed tool call can do anything that command can do.

## Build

```sh
go build -o toprelay ./cmd/toprelay      # Go 1.27
```

The Worker bundle is embedded from `internal/deploy/relay.js` (`cd ../worker && npm run bundle` re-copies it).
Deploys refuse a bundle that doesn't export the `Relay` class.

## Commands

Every command has `--help`. Most take `--json` (see below).

| Command | What it does |
|---|---|
| `login [--token-stdin] [--account ID]` | Verify and store a Cloudflare API token (Workers Scripts Edit, Workers KV Storage Edit, Account Settings Read) and pick the account |
| `add <name> --mcp-url URL` | Expose a Streamable HTTP MCP server |
| `add <name> --stdio [--env K=V]… [--cwd DIR] -- cmd args…` | Launch a stdio MCP server and bridge it (Streamable HTTP at `/<name>/mcp`, legacy SSE at `/<name>/mcp/sse`) |
| `add <name> --http URL [--allow-path P]…` | Raw HTTP forwarding, limited to path prefixes |
| `add … [--header "Name: value"] [--tools-default allow-readonly\|deny\|allow-all]` | Upstream header (value kept in the keyring); non-interactive tool choice |
| `tools <name> [--allow a,b] [--deny a,b] [--allow-all] [--reset]` | Show tools (exposed or hidden, annotations) and change the policy; a running `up` applies it live |
| `up [--install \| --uninstall]` | Run every service; `--install` adds a launchd agent (`~/Library/LaunchAgents/dev.toprelay.agent.plist`) or a systemd user unit |
| `status` | Relay-side connection and today's usage per service, against the 100k requests/day free allowance |
| `list` | Services, local targets and public URLs |
| `logs <name> [-f] [-n N]` | Local call log (no bodies, no credentials) |
| `key <name> [--tools a,b] [--for 1h] [--client claude-api\|openai\|xai\|gemini\|qwen\|curl]` | Mint a bearer key locally and print snippets |
| `key narrow <key> [--tools …] [--for …]` | Narrow a key offline |
| `key revoke <name> <id>` | Revoke one key by id |
| `pair <name> [--ttl 10m]` | Pairing code for the OAuth consent page (claude.ai and other OAuth clients) |
| `rotate <name>` | gen+1, redeploy, revoke OAuth grants: every key for the service stops working |
| `remove <name>` | Revoke OAuth, drop from the relay, delete the service's secrets |
| `doctor` | Check everything, each failure with a fix; non-zero exit on failure |
| `test <name>` | One harmless call through the public URL with a 5-minute key; prints the relay's added latency |

Service names are 1-32 lowercase letters and digits. `narrow` and `revoke` are refused as names because they
are `key` subcommands.

## Environment

| Variable | Meaning |
|---|---|
| `TOPRELAY_CONFIG_DIR` | Config directory. Default `$XDG_CONFIG_HOME/toprelay`, else `~/.config/toprelay` |
| `TOPRELAY_KEYRING=file` | Keep secrets in `<config dir>/secrets.json` (0600) instead of the OS keyring |
| `TOPRELAY_RELAY_URL` | Use this relay base URL instead of the deployed one, e.g. `http://127.0.0.1:8787` for `wrangler dev` (`http` maps to `ws`). Also polled for `/health` after a deploy |
| `TOPRELAY_CF_API_URL` | Cloudflare API base URL (tests only) |

## Files

`<config dir>/config.json` (0600): no secrets.

```json
{
  "accountId": "…", "accountName": "…", "subdomain": "hari",
  "publicUrl": "https://toprelay.hari.workers.dev", "kvNamespaceId": "…",
  "services": [
    {"name": "notes", "kind": "mcp", "gen": 1,
     "source": {"type": "mcp-url", "url": "http://127.0.0.1:7879/mcp"},
     "tools": {"allow": ["search"], "deny": ["delete"], "default": "deny"},
     "upstreamHeader": "X-App-Token"},
    {"name": "tools", "kind": "mcp", "gen": 1,
     "source": {"type": "stdio", "command": ["node", "server.js"], "env": {"K": "V"}, "cwd": "/path"},
     "tools": {"allow": [], "deny": [], "default": "allow"}},
    {"name": "web", "kind": "http", "gen": 1, "source": {"type": "http", "url": "http://127.0.0.1:8080"},
     "allowPaths": ["/api"], "tools": {"allow": [], "deny": [], "default": "allow"}}
  ]
}
```

A tool is exposed if it's in `deny`: no; in `allow`: yes; otherwise `default` decides (`"allow"` or `"deny"`).

**Secrets.** OS keyring service `toprelay`, or with `TOPRELAY_KEYRING=file` a flat JSON object in
`<config dir>/secrets.json` (mode 0600):

```json
{
  "cloudflare-token": "…",
  "agent-root": "<32 random bytes, base64url, no padding>",
  "bearer-root": "<32 random bytes, base64url, no padding>",
  "upstream-header/notes": "<value of the service's upstreamHeader>"
}
```

The CLI creates the roots on the first `add`. They must match the Worker's `AGENT_ROOT` and `BEARER_ROOT`.

**Running against `wrangler dev`** with no Cloudflare account:

```sh
cd worker && npx wrangler dev --port 8787 \
  --var AGENT_ROOT:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8 \
  --var BEARER_ROOT:ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8
mkdir -p /tmp/tr && cd /tmp/tr
cat > config.json <<'EOF'
{"publicUrl":"http://127.0.0.1:8787","services":[{"name":"notes","kind":"mcp","gen":1,
 "source":{"type":"mcp-url","url":"http://127.0.0.1:7879/mcp"},"tools":{"allow":[],"deny":[],"default":"allow"}}]}
EOF
echo '{"agent-root":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8","bearer-root":"ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8"}' > secrets.json
chmod 600 config.json secrets.json
export TOPRELAY_CONFIG_DIR=/tmp/tr TOPRELAY_KEYRING=file TOPRELAY_RELAY_URL=http://127.0.0.1:8787
toprelay up &            # services must also be in the Worker's SERVICES var (wrangler.jsonc)
toprelay test notes      # or: toprelay key notes --for 1h --client curl
```

`<config dir>/state.json` (0600, written atomically by `up` on every change; for the menu-bar app):

```json
{"pid": 4242, "startedAt": "…", "updatedAt": "…", "running": true,
 "services": {"notes": {"connected": true, "since": "2026-11-01T07:01:22Z", "lastError": "",
                        "reconnectAttempt": 0, "nextRetryMs": 0, "requests": 5, "refused": 1}}}
```

`since` is `null` while disconnected. `running` turns `false` when `up` exits. `requests` and `refused` count
calls in this `up` session.

`<config dir>/logs/<service>.jsonl`: one line per call, rotated at 10 MB (one `.1` file kept). With `up
--install`, the daemon's own log is `logs/agent.log`.

## JSON output (`--json`)

With `--json`, `list`, `status`, `tools`, `pair`, `key`, `add`, `remove`, `rotate`, `doctor`, `test`, `login`,
`version` and `up --install/--uninstall` print exactly one JSON object on stdout and nothing else. They never
prompt. A failure prints the following and exits 1 (2 for usage errors):

```json
{"error": {"code": "not_logged_in", "message": "no Cloudflare token stored", "fix": "run `toprelay login`"}}
```

`doctor` exits 1 with its normal report when a check fails. `logs --json [-f]` prints one call record per line
instead.

| Command | Object |
|---|---|
| `login` | `{"account":{"id","name"},"tokenStatus":"active"}` |
| `add` | `{"service":<config service>,"url","sseUrl"?,"tools":[ToolView],"warnings":[…],"firstDeploy","subdomain"}` |
| `tools` | `{"service","policy":{"allow","deny","default"},"reachable","error"?,"tools":[ToolView],"changed"}` |
| `list` | `{"accountId","publicUrl","services":[{"name","kind","source","local","url","sseUrl"?,"gen","allowedTools","deniedTools","newTools","allowPaths"?,"upstreamHeader"?}]}` |
| `status` | `{"day","requestsToday","freeLimit":100000,"percentOfLimit","daemon":<state.json>?,"services":[{"name","kind","url","connected","connectedAt"?,"agent"?,"requests","rejected","bytesIn","bytesOut","revokedKeys","error"?,"local":<state entry>?}]}` |
| `key` | `{"key","id","service","gen","tools":[…]\|null,"expires"?,"url","snippets":{client:text},"warnings"}` |
| `key narrow` | `{"key","id","service","gen","tools","expires"?,"warnings"}` |
| `key revoke` | `{"service","id","revokedKeys"}` |
| `pair` | `{"service","code","expires":<unix s>,"expiresAt":<RFC 3339>}` |
| `rotate` | `{"service","gen","oauthRevoked","warnings"}` |
| `remove` | `{"service","removed":true,"oauthRevoked","warnings"}` |
| `doctor` | `{"ok","checks":[{"name","status":"ok\|warn\|fail\|skip","detail"?,"fix"?}]}` |
| `test` | `{"service","call","ok","status"?,"tools"?,"remoteMs","localMs","relayAddedMs","protocol"?}` |
| `up --install` | `{"installed":true\|false,"path"}` |
| `version` | `{"version"}` |

`ToolView` is `{"name","title"?,"description"?,"allowed","readOnly","destructive","idempotent","openWorld","warnings"?}`.
A call log line is `{"time","method","path","mcpMethod"?,"tool"?,"status","durationMs","client"?,"refused"?}`.

## Bot compatibility

Fill this from real end-to-end tests only. The `key` snippets for the OpenAI, xAI, Gemini and Qwen APIs follow
their documented shapes but haven't been tested end to end yet.

| Bot | How it connects | Tested |
|---|---|---|
| Generic MCP client (go-sdk, 2026-07-28 and 2025-11-25) | Streamable HTTP + bearer key | yes (fake relay and `wrangler dev`) |
| Legacy SSE client (go-sdk) | `/<name>/mcp/sse` + bearer key | yes (fake relay); endpoint event checked on `wrangler dev` |
| claude.ai, Claude API, ChatGPT/OpenAI, xAI, Gemini, Qwen | see `toprelay add` output | not yet |

## Testing

```sh
go vet ./...
go test -race ./...
```

The tests use no network or Cloudflare account. They run against an in-process Cloudflare API fake that asserts
the exact multipart upload, metadata, migrations and headers (`internal/testutil/cffake`), and an in-process relay
speaking the protocol.md frames (`internal/testutil/fakerelay`). They also build and launch a real stdio MCP
server (`internal/stdiobridge/testdata/tinyserver`) and use real go-sdk clients and servers. Key derivation is
checked against the protocol.md test vectors.
