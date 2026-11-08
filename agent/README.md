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
| `add <name> --mcp-url URL` | Expose a Streamable HTTP MCP server. URL is `http(s)://…` or `unix:///path/app.sock[?path=/mcp]` |
| `add <name> --stdio [--env K=V]… [--cwd DIR] -- cmd args…` | Launch a stdio MCP server and bridge it (Streamable HTTP at `/<name>/mcp`, legacy SSE at `/<name>/mcp/sse`) |
| `add <name> --http URL [--allow-path P]…` | Raw HTTP forwarding, limited to path prefixes (URL as for `--mcp-url`) |
| `add … --header-file Name=/path` | The app's credential header, read from that file (yours, mode 0600) on **every request**, so the app can rotate it |
| `add … --header-stdin` | The credential as `Name: value` on the first line of stdin; the value goes to the keyring |
| `add … --header "Name: value"` | **Discouraged**: the value is in argv, visible to other processes (`ps`). Kept for compatibility; `add` warns |
| `add … --pin-team-id T [--pin-bundle-id B]` | macOS: only connect if the process serving the app is signed by Apple team `T` (and has bundle id `B`) |
| `add … --caller-key-file PATH` | Sign `X-TopRelay-Caller` with the app's own key file instead of one TopRelay makes |
| `add … --tools-default allow-readonly\|deny\|allow-all` | Non-interactive tool choice |
| `add … --files-dir DIR` / `files-dir <name> (DIR \| --off)` | An outbox: files the app writes there can be handed to bots as `resource_link`s (see [Files](#files-for-bots)) |
| `deploy` | Redeploy the relay with this agent's Worker code (after an update); keeps every key and grant |
| `set-header <name> (--stdin \| --file Name=/path \| --clear)` | Change the credential header **live**: no redeploy, no reconnect, keys and OAuth grants untouched |
| `caller-key <name> [--rotate \| --file PATH \| --default]` | Print (or rotate, or switch) the key the app uses to verify `X-TopRelay-Caller` |
| `tools <name> [--allow a,b] [--deny a,b] [--allow-all] [--reset]` | Show tools (exposed or hidden, annotations) and change the policy; a running `up` applies it live |
| `tools <name> --client SELECTOR (--allow … \| --deny … \| --reset)` | A per-client rule: `claude.ai`-style domain, `key:<id>`, exact client id, or `prefix*` |
| `limits <name> [--service R] [--client R] [--tool NAME=R]… [--clear]` | Rate limits in the agent (`60/m`, `10/s`, …): 429 + `Retry-After` |
| `up [--install \| --uninstall]` | Run every service; `--install` adds a launchd agent (`~/Library/LaunchAgents/dev.toprelay.agent.plist`) or a systemd user unit |
| `status` | Relay-side connection and today's usage per service, against the 100k requests/day free allowance |
| `status --watch` | Follow the local agent (no relay calls): the state now, then again on every change; with `--json` one line each |
| `list` | Services, local targets and public URLs |
| `logs <name> [-f] [-n N]` (or `--service name`) | Local call log: client, tool, status, duration, bytes, refusal reason (no bodies, no credentials) |
| `logs --retention DAYS` | Keep that many days of call log for every service (0: only the 10 MB size cap) |
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
    {"name": "ezpdf", "kind": "mcp", "gen": 1,
     "source": {"type": "mcp-url", "url": "unix:///Users/me/Library/Containers/app.ezpdf/Data/mcp.sock"},
     "tools": {"allow": [], "deny": [], "default": "deny"},
     "upstreamHeader": "X-EzPDF-Token", "upstreamHeaderFile": "/Users/me/Library/Application Support/ezpdf/mcp-token",
     "upstreamHeaderRev": 2, "pin": {"teamId": "ABCDE12345", "bundleId": "app.ezpdf"}},
    {"name": "tools", "kind": "mcp", "gen": 1,
     "source": {"type": "stdio", "command": ["node", "server.js"], "env": {"K": "V"}, "cwd": "/path"},
     "tools": {"allow": [], "deny": [], "default": "allow"}},
    {"name": "web", "kind": "http", "gen": 1, "source": {"type": "http", "url": "http://127.0.0.1:8080"},
     "allowPaths": ["/api"], "tools": {"allow": [], "deny": [], "default": "allow"}}
  ]
}
```

A tool is exposed if it's in `deny`: no; in `allow`: yes; otherwise `default` decides: `"allow"`, `"deny"`, or
`"readonly"` (what `add` sets): exposed only if the server's MCP annotations say `readOnlyHint: true` and it isn't
destructive. MCP treats a tool without `readOnlyHint` as destructive unless it says `destructiveHint: false`; under
`"readonly"` such tools, and every non-read-only tool, need an explicit allow. New read-only tools the server adds
later are exposed; new write tools aren't.

`tools.clients` holds per-client rules, `{"<selector>": {"allow": […], "deny": […]}}`, applied on top: a rule's
`deny` hides a tool from that client, its `allow` exposes one (even one the service denies). A selector is an exact
client id (`key:<id>` for a bearer key, or an OAuth client id), a prefix ending in `*`, or a domain such as
`claude.ai` that matches OAuth client ids that are URLs on that host or a subdomain. Exact beats prefix beats
domain. The agent enforces rules per request (listing and calling); the relay is told the union, as a first check.
A bearer key's own `--tools` restriction applies on top of all of this.

`limits` holds the service's rate limits (`{"service":"600/h","client":"60/m","tools":{"render":"10/m"}}`), each
GCRA with a burst of N. A call over any of them gets `429` with `Retry-After` (and a JSON-RPC error with
`data.retryAfter`) without reaching the app, and charges none of the others. The relay's own limit (20/s, burst 60
per service) still applies first. `logRetentionDays` (top level) is the call-log retention.

`upstreamHeader` is the credential header's name. Its value is in the keyring, unless `upstreamHeaderFile` names a
file to read on every request. `set-header` bumps `upstreamHeaderRev`, which tells a running `up` to reload the
value without restarting the service. `callerKeyFile`, when set, is an app-owned key for `X-TopRelay-Caller`.
`pin` is a macOS code-signature requirement (see [What the app receives](#what-the-app-receives)).

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

`since` is `null` while disconnected. `running` turns `false` when `up` exits; readers (`status`, `status --watch`,
TopRelayKit) also report `false` when `pid` no longer exists, so a crashed agent doesn't look alive. `requests` and
`refused` count calls in this `up` session.

**Schema.** `version` is `1`. It goes up only for an incompatible change; new fields may appear at any time, so
ignore what you don't know. A file without `version` is version 1. The file is replaced atomically (written to a
temporary file in the same directory, then renamed), so a reader never sees half of it; watch the directory, not
the file. Fields: `version` int, `pid` int, `startedAt`/`updatedAt` RFC 3339, `running` bool, `services` map of
name → `{connected bool, since RFC 3339|null, lastError string, reconnectAttempt int, nextRetryMs int,
requests int, refused int, limited int}` (`limited`: calls over the service's own rate limits).

`<config dir>/agent.sock` (0600): the control socket of a running `up`, for TopRelayKit. See
[Control socket](#control-socket). Only one `up` runs per config dir: a second one exits with `already_running`.

`<config dir>/caller-keys/<service>.key` (0600, directory 0700): the key apps use to verify `X-TopRelay-Caller`.
Made by `add` (or by `up` for older services), replaced by `caller-key --rotate`, deleted by `remove`.

`<config dir>/logs/<service>.jsonl`: one line per call, rotated at 10 MB (one `.1` file kept). With `up
--install`, the daemon's own log is `logs/agent.log`.

## JSON output (`--json`)

With `--json`, `list`, `status`, `tools`, `pair`, `key`, `add`, `remove`, `rotate`, `doctor`, `test`, `login`,
`version` and `up --install/--uninstall` print exactly one JSON object on stdout and nothing else. They never
prompt. A failure prints the following and exits 1 (2 for usage errors):

```json
{"error": {"code": "not_logged_in", "message": "no Cloudflare token stored", "fix": "run `toprelay login`"}}
```

`doctor` exits 1 with its normal report when a check fails. `logs --json [-f]` and `status --watch --json` print
one object per line instead.

**Bounded.** Every one-shot command finishes within `--timeout` (default 5 minutes; a first deploy is the slowest
step). At the deadline it is cancelled and prints `{"error":{"code":"timeout",…}}`; if it still hasn't returned 5
seconds later, the process prints that error itself (unless it already printed its result) and exits. `up`,
`logs -f` and `status --watch` have no default deadline; an explicit `--timeout` ends them normally. Commands that
start helpers (`up --install` runs `launchctl`/`systemctl`) never hand them your stdout or stderr and stop waiting
for them after 30 s, so a caller reading toprelay's output to EOF always gets EOF.

**Quiet.** In `--json` mode at most 4 KiB goes to stderr, so an app that never reads stderr can't block toprelay
on a full pipe. `up --json` logs to `<config dir>/logs/agent.log` instead of stderr.

| Command | Object |
|---|---|
| `login` | `{"account":{"id","name"},"tokenStatus":"active"}` |
| `add` | `{"service":<config service>,"url","sseUrl"?,"tools":[ToolView],"warnings":[…],"firstDeploy","subdomain"}` |
| `tools` | `{"service","policy":{"allow","deny","default","clients"?},"reachable","error"?,"tools":[ToolView],"changed"}` |
| `limits` | `{"service","limits":{"service"?,"client"?,"tools"?},"changed"}` |
| `logs --retention` | `{"logRetentionDays"}` |
| `list` | `{"accountId","publicUrl","services":[{"name","kind","source","local","url","sseUrl"?,"gen","allowedTools","deniedTools","newTools","allowPaths"?,"upstreamHeader"?,"upstreamHeaderSource"?:"keyring\|file","upstreamHeaderFile"?,"callerKeyFile"?,"pin"?}]}` |
| `status` | `{"day","requestsToday","freeLimit":100000,"percentOfLimit","daemon":<state.json>?,"services":[{"name","kind","url","connected","connectedAt"?,"agent"?,"requests","rejected","bytesIn","bytesOut","revokedKeys","error"?,"local":<state entry>?}]}` |
| `key` | `{"key","id","service","gen","tools":[…]\|null,"expires"?,"url","snippets":{client:text},"warnings"}` |
| `key narrow` | `{"key","id","service","gen","tools","expires"?,"warnings"}` |
| `key revoke` | `{"service","id","revokedKeys"}` |
| `pair` | `{"service","code","expires":<unix s>,"expiresAt":<RFC 3339>}` |
| `set-header` | `{"service","header":"<name>","updated":true,"source":"keyring\|file\|none","file"?}` (`header` is `""` after `--clear`) |
| `files-dir` | `{"service","filesDir"}` (`""` after `--off`) |
| `deploy` | `{"deployed":true,"publicUrl","subdomain"}` |
| `caller-key` | `{"service","header":"X-TopRelay-Caller","key","path","source":"toprelay\|app","rotated"}` |
| `rotate` | `{"service","gen","oauthRevoked","warnings"}` |
| `remove` | `{"service","removed":true,"oauthRevoked","warnings"}` |
| `doctor` | `{"ok","checks":[{"name","status":"ok\|warn\|fail\|skip","detail"?,"fix"?}]}` |
| `test` | `{"service","call","ok","status"?,"tools"?,"remoteMs","localMs","relayAddedMs","protocol"?}` |
| `up --install` | `{"installed":true\|false,"path"}` |
| `version` | `{"version"}` |
| `status --watch` | one state.json object per line (below) |

`ToolView` is `{"name","title"?,"description"?,"allowed","readOnly","destructive","idempotent","openWorld","warnings"?}`.
A call log line is `{"time","method","path","mcpMethod"?,"tool"?,"status","durationMs","client"?,"refused"?,"reason"?,"bytesIn","bytesOut"}`.
`reason` is `tool_not_allowed`, `invalid_request`, `rate_limited`, `credential_unavailable` or `app_unreachable`.
Bodies, arguments, results and credentials are never logged.

## What the app receives

The full contract is in [`docs/protocol.md`](../docs/protocol.md#agent--local-app); for app authors, see
[`MIGRATION.md`](../MIGRATION.md). In short:

- **Host** is the app's own (`127.0.0.1:<port>` as configured, or `localhost` over a socket), never the relay's, so
  a DNS-rebinding check on `Host` keeps working. The relay's host is in `X-Forwarded-Host`.
- **`X-Forwarded-For`, `CF-Connecting-IP`, `X-Forwarded-Proto`, `X-Toprelay-Client`** come from the relay. Any local
  process can also send them, so treat them as hints only.
- **`X-TopRelay-Caller`** is signed by the agent with the service's caller key and covers the method, path and
  query, client, time and body. It is the one header that proves a request came through TopRelay. The agent
  removes any copy that arrives from the bot side.
- **The credential header** comes from the keyring or a file, read fresh for every request.

**Port squatting.** Whatever listens on the app's TCP port gets the relayed requests and the credential. Two
defenses:

1. **A Unix socket** (`unix:///path/app.sock`): before each connection the agent checks that the socket is yours
   with mode 0600, that its directory is yours (or root's) and not writable by others, and that the peer process
   runs as you (`LOCAL_PEERCRED` / `SO_PEERCRED`). macOS and Linux.
2. **A code-signature pin** for TCP on macOS (`--pin-team-id`, optional `--pin-bundle-id`): after each new
   connection is made, and before anything is sent on it, the agent finds the process holding the accepting end
   of *that* connection (`lsof` on the connection's own port pair) and checks it with `codesign --verify -R=…`.
   Checking who listens on the port wouldn't be enough: another user could listen on the same port on another
   address (`::1` next to `127.0.0.1`), and the connection might go there. Limits: it costs an `lsof` and a
   `codesign` run per new connection (a passing process is cached for 5 minutes, and connections are reused);
   `lsof` only sees your own processes, so if the accepting process isn't yours, or doesn't accept within half a
   second, the connection is refused; it proves who answered, not that the app is free of bugs, and another
   process of yours can still inject into an app that doesn't use the hardened runtime. A bundle id without a team
   id is refused, because anyone can sign with any bundle id. Off macOS, use a socket.

## Control socket

A running `up` serves `<config dir>/agent.sock` (mode 0600, in the 0700 config dir; connections from other users
are dropped after a peer-credential check). It is how [TopRelayKit](../TopRelayKit/README.md) works without
starting processes. HTTP/1.x over the socket:

| Request | Answer |
|---|---|
| `GET /v1/state` | the state.json object |
| `GET /v1/state/watch` | NDJSON: the state now, then a line on every change (pushed, not polled); an empty line every 30 s when idle |
| `POST /v1/cli` `{"args":["add","notes",…],"stdin":"…"}` | exactly the object `toprelay <args> --json` prints; HTTP 200 for exit 0, 422 for exit 1, 400 for a usage error |
| `POST /v1/files` `{"service","path","name"?,"contentType"?,"ttl"?}` | `{"service","id","uri":"toprelay-file://<id>","expires"}`: a file the app may return in a `resource_link` |

`/v1/cli` runs one command at a time, in the agent, with the same code as the CLI. Allowed: `add`, `set-header`,
`caller-key`, `files-dir`, `deploy`, `limits`, `tools`, `list`, `status`, `pair`, `key`, `rotate`, `remove`,
`doctor`, `test`, `version`, `login`. A command that overruns its `--timeout` gets a timeout answer, and later
commands wait until it has really finished.
`up`, `logs -f` and `status --watch` are refused. Put secrets in `stdin` (`--header-stdin`, `set-header --stdin`,
`login --token-stdin`), never in `args`.

The socket path must fit in 104 bytes (macOS). If it doesn't, `up` runs without it and logs a warning.

## Files for bots

A tool can hand a bot a file without base64 in JSON: return an MCP `resource_link` whose `uri` is

- `toprelay-file:///<name>` for a file the app wrote into the service's outbox (`--files-dir`), or
- `toprelay-file://<id>` for a file registered with `POST /v1/files` (TopRelayKit: `offerFile`).

The agent replaces the `uri` with `https://<relay>/<service>/files/<id>?e=…&s=…`: signed (no OAuth needed, so
any bot or script can fetch it), single-use, valid 10 minutes, and served only if the file hasn't changed since.
It fills in `mimeType`, `name` and `size`. A link is minted for the call (and client) that produced it, but the
fetch itself carries no credential, so whoever holds the link can use it, once, within its 10 minutes: treat it
like the file's content. Files are at most 256 MiB, must be yours, and outbox paths can't leave
the outbox (`..` and symlinks out are refused). The relay always serves them as attachments with `nosniff` and a
sandboxing CSP. Details: [`docs/protocol.md`](../docs/protocol.md#files-11). Only `--mcp-url` services; a relay
deployed by an older agent needs `toprelay deploy` once.

A link is spent only when the relay hands the request to the agent; if the agent is offline, too old, or drops
before it starts sending, the same link works again later. Once bytes flow it is spent, and a download the relay
cuts short can't be retried with it. The relay has no flow control yet and stops a download when more than 8 MiB
is waiting for a slow reader, so large files need a bot that reads about as fast as your upload. Ask the tool
again for a fresh link.

Uploads aren't supported yet: see the protocol's Files section for why and what's reserved.

## Long tool calls

The relay answers `504` if a reply's headers take more than 120 s, and ends a stream with no bytes for 300 s.
The agent keeps slow tools inside both:

- **Early SSE.** A single `tools/call` from a client that accepts `text/event-stream` (Streamable HTTP clients must)
  that hasn't got the app's headers after 20 s gets an SSE reply started by the agent. When the app answers with
  JSON, it is delivered as one `event: message`; when it answers with SSE, its events follow. If the app fails, the
  call ends with a tool error the model can read. Other requests (`initialize`, `tools/list`, notifications) are
  never upgraded, so their headers (`Mcp-Session-Id`) reach the bot.
- **Keep-alives.** An SSE reply goes at most about 18 s without bytes (15 s, checked every 3 s): the agent writes an SSE comment (`: keepalive`)
  between events. Clients ignore comments.
- **Progress** (`notifications/progress`) and every other event pass through as they arrive.
- **Cancellation.** When the bot disconnects, the relay sends `cancel`, the agent cancels its request to the app,
  and the app's HTTP request context ends (tested end to end). Whether your tool stops is up to your server: the
  Go SDK v1.8's stateless Streamable HTTP handler keeps running the tool after the request ends (measured in
  `internal/daemon/longcall_test.go`), so check `r.Context()` yourself if you use it.
- No call lasts more than 840 s in total (the relay's cap).

`TOPRELAY_LONG_TESTS=1 go test -run TestThreeMinuteToolCalls ./internal/daemon` runs a real 3-minute streaming tool
and a 3-minute JSON tool with these timings.

## Bot compatibility

Fill this from real end-to-end tests only. The `key` snippets for the OpenAI, xAI, Gemini and Qwen APIs follow
their documented shapes but haven't been tested end to end yet.

| Bot | How it connects | Tested |
|---|---|---|
| Generic MCP client (go-sdk, 2026-07-28 and 2025-11-25) | Streamable HTTP + bearer key | yes (fake relay and `wrangler dev`) |
| Legacy SSE client (go-sdk) | `/<name>/mcp/sse` + bearer key | yes (fake relay); endpoint event checked on `wrangler dev` |
| claude.ai, Claude API, ChatGPT/OpenAI, xAI, Gemini, Qwen | see `toprelay add` output | not yet: runbook in [`docs/bot-testing.md`](../docs/bot-testing.md) |

Not built yet: per-call approval of destructive tools on a phone or the menu bar (planned for v1.1). Until then,
keep destructive tools hidden, or allow them only for the clients that need them (`tools --client`).

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
