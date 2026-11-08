# TopRelay: reach any local app from any cloud bot

A product brief and build guide for a separate repo. First written 2026-10-25 from the relay built into ezRecord
(`ezRecord/ezRecord/Remote/`) and the Cloudflare docs research in
`ezRecord/docs/research/cloudflare-relay-2026-10.md`. Revised 2026-10-28 after a competitive review (OpenAI Secure
MCP Tunnel, MCP 2026-07-28, claude.ai connector auth). Revised 2026-10-31 from the research in `docs/research/`
(`agent-ecosystem-2026-10.md`, `tailscale-and-tunnels-2026-10.md`, `algorithms-for-linking-2026-10.md`,
`directions-2026-10.md`). Read the research files before building; every Cloudflare limit below comes from them.

## The problem

More and more desktop apps run a local server for agents: MCP servers, HTTP tool APIs, dev servers. Cloud bots
(Grok, ChatGPT, claude.ai custom connectors, agents in someone's backend) can only call a **public HTTPS URL**.
A laptop has none: it sits behind a home router, often behind carrier NAT (CGNAT), and changes networks.

Today's options are poor for normal people:
- **Port forwarding / UPnP**: fails under CGNAT, exposes the home IP, needs a domain and a TLS certificate.
- **ngrok / cloudflared quick tunnels**: account per user, URL changes every run, quick tunnels don't support SSE,
  2026 "protected" quick tunnels need an email PIN a bot can't pass.
- **Tailscale Funnel**: still beta (on all plans, including free). Needs a Tailscale account, HTTPS and the
  `funnel` attribute in the tailnet policy, ports 443/8443/10000 only, `*.ts.net` names only (no custom domains),
  unpublished bandwidth caps. TLS ends on the device, which is more private than our relay, but public requests
  carry **no caller identity**, and the free plan is non-commercial. Tailscale itself advises against Funnel for
  agents with personal data.
- **A VPS running frp / rathole / bore**: works, but someone has to rent and run a server.
- **OpenAI Secure MCP Tunnel** (2026-05, `openai/tunnel-client`): connects a localhost MCP server to ChatGPT, Codex
  and the Responses API with an outbound-only client. Good, but traffic goes through OpenAI, it only serves OpenAI
  products, and OpenAI's docs say public plugins still need a public HTTPS endpoint. 120 s per request,
  600 requests a minute.
- **Anthropic MCP tunnels** (research preview, Enterprise or by request): `cloudflared` plus an Anthropic proxy,
  hostnames under `*.tunnel.anthropic.com`. Claude products only, aimed at company networks (Docker/Helm).

Both vendor tunnels leave auth to the MCP server, and each serves only its own vendor's bots.

Reaching the app is only half of it. A tunnel delivers a request; it doesn't control **who** may call **which**
tools. Most local MCP servers (especially `stdio` ones) have no auth at all, so a plain tunnel makes them
publicly callable.

## The product

**TopRelay gives a local MCP server a stable, private HTTPS endpoint that any agent can use, on the person's own
free Cloudflare account, with them in control of which tools are exposed.**
No server for us to run or pay for, no traffic through us or an AI vendor, nothing to rent.

```
Cloud bot ──HTTPS + OAuth/bearer──▶ https://toprelay.<their-subdomain>.workers.dev/<service>/mcp
                              │   Worker: auth check, routing. Never reads bodies. (their account, free plan)
                              │   Durable Object per service: holds the socket, pending requests
                              ▼
      laptop agent ◀── one outbound WebSocket per service (no inbound ports, works behind any NAT)
          │   MCP-aware: tool allow-list, call log, stdio bridge
          ├──▶ http://127.0.0.1:<port>/mcp   (ezRecord on :7879, any Streamable HTTP MCP server)
          ├──▶ stdio child process            (any stdio MCP server, launched by the agent)
          └──▶ http://127.0.0.1:<port>/…     (raw HTTP forwarding, no tool discovery)
```

**Design rule:** the Worker stays protocol-blind (headers only). Everything that understands MCP lives in the
laptop agent. That keeps the Worker inside 10 ms CPU, keeps the "relay never reads your traffic" promise as close
to true as a TLS-terminating relay can be, and means MCP spec changes only touch the agent.

Two things ship:
1. **The relay Worker** (`relay-worker.js`), deployed by the agent into the user's account.
2. **The laptop agent**, in two forms:
   - `toprelay` CLI and daemon: one static Go binary. macOS and Linux supported; Windows binary built but best
     effort until keychain and service install are tested there.
   - `TopRelayKit`: a thin Swift package for Mac apps (ezRecord, Musedrop, ezPrompter) over the same protocol and
     credential handling. Built after the CLI is proven, not as a second networking implementation.

### What v1 supports

| The local app has | TopRelay v1 |
|---|---|
| A Streamable HTTP MCP server | Expose its MCP endpoint (`--mcp-url`) |
| A `stdio` MCP server | Launch it and bridge it to Streamable HTTP (`--stdio`) |
| A plain HTTP service | Raw forwarding (`--http`). A public URL, not tool discovery: a bot still needs a tool contract |
| A REST API with an OpenAPI spec | **v1.1** (`--openapi`), see Deferred |
| Only a GUI | Not supported |

### The user experience

```sh
toprelay login                      # Cloudflare API token (v1); Sign in with Cloudflare later
toprelay add notes --mcp-url http://127.0.0.1:7879/mcp
toprelay add tools --stdio -- node ./server.js
#   ✓ relay deployed to your Cloudflare account
#   ✓ https://toprelay.hari.workers.dev/notes/mcp   ← give this to your bot
#   → connection steps for claude.ai, ChatGPT, Grok, Claude API
toprelay tools notes                # list tools, choose which are exposed (default: all read-only hints, confirm rest)
toprelay up                         # keep all services connected; `up --install` for launchd/systemd
toprelay doctor                     # local service, relay, auth, transport, remote endpoint
toprelay test notes                 # one harmless call end to end (tools/list, or a tool marked safe)
toprelay status | list | logs <name> | rotate <name> | remove <name>
```

Inside a Mac app it's one switch: **Reach from anywhere** → sign in → copyable URL with steps per bot.

## How it works

### Worker (public side)
- `GET /health` → `ok`.
- `GET /<service>/connect` (WebSocket upgrade) → the laptop's socket. Auth: `Authorization: Bearer <agent key>`,
  compared in constant time against a per-service secret.
- OAuth endpoints for remote clients (see Credentials): `/.well-known/oauth-protected-resource/<service>/…`,
  `/.well-known/oauth-authorization-server`, `/authorize`, `/token`, `/register`.
- `* /<service>/…` → **check the remote-client credential first**. For MCP 2026-07-28 clients, also refuse a
  disallowed tool from the required `Mcp-Method` / `Mcp-Name` headers (`tools/call` + a name not on the allow-list)
  without reading the body; the agent still does the full body check. Then forward to that service's Durable Object
  (`idFromName(service)`). Rejected calls never cost a Durable Object request. No socket → **503 at once** with a
  clear message (JSON-RPC error body for MCP paths), never a hang.
- Forward only an allow-list of headers. **Strip the remote-client credential** before forwarding, so it never
  reaches the app.
- The Worker only authenticates and routes (Workers Free: **10 ms CPU** per request; measure the OAuth token check
  against this). The Durable Object does the work (30 s CPU per request).

### Durable Object
- Accept the laptop socket with `ctx.acceptWebSocket(ws, [tag])` (hibernation). **Never `ws.accept()`**: it bills
  duration for the whole connection.
- `ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair("ping","pong"))`: keepalives don't wake it and
  aren't billed.
- A new connection for the same service replaces the old one.
- Pending requests live in an in-memory `Map` keyed by request id. Safe, because an in-flight request keeps the
  object in memory (with compatibility_date ≥ 2026-10-01, pending I/O holds it for up to 15 min).
- No `setInterval` in the object (it blocks hibernation). Timeouts via `Promise.race` per request.
- Many requests in flight at once per service; the id keeps them apart.
- Client disconnects (request `signal` aborts) → send a `cancel` frame to the agent.
- On socket close or error: fail every pending request with 503. **Never retry** a relayed request: it may already
  have changed the user's data.
- Per-service rate limit.

### Wire protocol (v1)
**Reserved now, built later** (adding them after v1 would break every installed agent):
- a binary frame type (no base64), negotiated with a `caps` list in a hello frame;
- per-stream sequence numbers and `credit` frames (relay → agent) for flow control;
- request ids generated by the Durable Object and scoped to its incarnation, so a reconnect never reuses one.
  The agent keeps a short table of outcomes by id, so a call cut off by a dropped socket can resume instead of
  running twice.

The v1 encoding: JSON text frames. Bodies are base64. Streaming and cancellation are in v1: MCP's Streamable HTTP answers either
with JSON or with a request-scoped SSE stream, and both must work.

```
relay → agent  {"v":1, "id", "method", "path", "headers": {…}, "body"}
relay → agent  {"v":1, "id", "cancel": true}                       client went away
agent → relay  {"v":1, "id", "status", "headers": {…}, "body"}       whole response, or
agent → relay  {"v":1, "id", "status", "headers": {…}, "stream": true}
agent → relay  {"v":1, "id", "chunk": base64} …  {"v":1, "id", "end": true}
agent → relay  "ping"   (every 30 s)   relay → agent  "pong"
agent → relay  {"v":1, "hello": true, "caps": [...]}                first frame; relay answers with its caps
```

Unknown fields and unknown frame types are ignored, so either side can be newer.

For a streamed reply the object returns a `Response` whose body is a `TransformStream` and writes chunks into it
as they arrive. Worker ↔ Durable Object calls carry streams with flow control.

### The laptop agent
- One goroutine per service; reconnect with backoff, and immediately after wake or a network change. Code deploys
  and rotations drop sockets: recovering from that is normal operation, not an error.
- **MCP versions:** clients will speak both 2025-11-25 (stateful, `initialize` handshake, `Mcp-Session-Id`) and
  2026-07-28 (stateless, `server/discover`, no handshake) for a while. For `--mcp-url`, forward as-is and keep
  session headers intact. For `--stdio`, serve with `modelcontextprotocol/go-sdk` ≥ v1.7.0, with `Stateless: true`
  on Streamable HTTP so 2026-07-28 is offered (without it the SDK negotiates down to 2025-11-25).
- **Transport-agnostic handler:** the agent's MCP layer is an `http.Handler` that doesn't care how requests arrive
  (the Durable Object socket today; a `tsnet` listener for a later `--private` mode).
- **Legacy SSE endpoint** (`/<service>/mcp/sse`) for the `stdio` bridge: Alibaba Model Studio (Qwen) accepts only SSE.
  Gemini accepts only Streamable HTTP. Streamable HTTP stays the default.
- **Tool allow-list:** filter `tools/list` results (JSON and SSE), keeping the server's order and its
  `ttlMs`/`cacheScope` cache hints, and answer a disallowed `tools/call` without touching the app. Answer it, and
  "service offline", as a tool result with `isError: true` and plain text the model can relay ("The owner's Mac is
  asleep; ask them to open it"), not only a JSON-RPC error.
- **Name lint at `add`:** service names lowercase alphanumeric (the Gemini API rejects `-`, Gemini CLI rejects `_`);
  warn on tool names over 63 characters or outside `[A-Za-z0-9_-]`; offer a prefix (`notes_search`). Warn when
  more than 20 tools are exposed: every vendor says quality drops past 10–20. Default: expose tools marked `readOnlyHint`, ask about the rest at `add`.
- **Call log:** local only. Time, service, method, tool name, status, duration. No bodies, no credentials.
- `stdio` processes run with the user's own permissions. Say so in `add` and in the README.
- Config in `~/.config/toprelay/config.json`; every secret in the OS keychain (Keychain, libsecret).

### Credentials

Three roles, never mixed:

| Credential | Between | Stored |
|---|---|---|
| **Agent key** | laptop agent ↔ relay | Worker secret + OS keychain |
| **Remote-client credential** | bot ↔ relay | Worker (OAuth tokens in KV, or a bearer key hash) |
| **Upstream credential** | agent ↔ app, if the app has its own auth | OS keychain only; the agent adds it locally |

The remote-client credential is required on every service, even when the local app has no auth. An agent that
authenticated to the relay must never make an unauthenticated server publicly callable.

Two ways for a bot to authenticate, both in v1:
- **MCP OAuth 2.1** via `@cloudflare/workers-oauth-provider` (≥ 1.2), with protected-resource metadata,
  authorization-server metadata, PKCE, **CIMD** (`clientIdMetadataDocumentEnabled`, plus the
  `global_fetch_strictly_public` compatibility flag) and DCR as a fallback (MCP 2026-07-28 deprecates DCR;
  Cloudflare plans to remove it after summer 2027). claude.ai's recommended connector option is now "Use Claude's
  published identity" (CIMD); ChatGPT supports CIMD too. claude.ai can also send static request headers, including
  `Authorization`, but that is beta for some organizations only, so OAuth stays the default for claude.ai. Needs a
  KV binding. The owner approves a client on the
  consent page with a short pairing code from `toprelay pair <service>` (single owner; no user accounts).
- **Generated bearer key** (`toprelay key <service>`), for clients that take a fixed header: the Claude API MCP
  connector (`authorization_token`), the OpenAI Responses API (`authorization`), xAI, Gemini (`headers` /
  `credential`), Alibaba Model Studio (`headers`), Mistral Le Chat, SDKs. `key` prints the exact snippet per
  client. Keys are **macaroon-style HMAC chains** rooted in one Worker secret: checked in microseconds, narrowed
  offline (`--tools search,read --for 1h`), and adding a key needs no redeploy. Revocation is a short deny-list.

Never put a credential in the URL. Redact secrets in every log.

### Deploy (what the agent does with the user's Cloudflare credential)
1. `GET /accounts` → account id (a pasted token may see several; let the user pick).
2. `PUT /accounts/{id}/workers/scripts/toprelay` as multipart: `metadata` +
   `relay-worker.js` (`application/javascript+module`). Metadata:
   `main_module`, **`compatibility_date` (always set it; default is 2021-11-02; use ≥ 2026-10-01)**,
   bindings `durable_object_namespace` (`RELAY` → class `Relay`), `kv_namespace` (`OAUTH_KV`, created first with
   `POST /accounts/{id}/storage/kv/namespaces` if missing), one `secret_text` per agent key and bearer key hash,
   and on the first deploy only `migrations: {new_tag:"v1", new_sqlite_classes:["Relay"]}`.
   (Newer alternative since 2026-07: `exports: {"Relay":{"type":"durable-object","storage":"sqlite"}}`. The two
   are mutually exclusive, and moving to `exports` is one-way. Pick one at the start.)
3. `POST /accounts/{id}/workers/scripts/toprelay/subdomain {"enabled":true}` (POST, not PUT;
   header `Cloudflare-Workers-Script-Api-Date: 2025-08-01`).
4. `GET /accounts/{id}/workers/subdomain`; if none (error 10007), `PUT` one (10031 = taken, try another).
5. Poll `https://toprelay.<sub>.workers.dev/health` until 200 (new subdomains take a moment).

Idempotent: safe to re-run. Adding a service = add secret bindings and redeploy (no migration). Rotating = new
secret + redeploy; the agent reconnects with the new key. Removing = delete the secrets, revoke its OAuth grants,
close the socket.

### Signing in to Cloudflare
This is the user's Cloudflare credential for deploying, not the OAuth that bots use to call the relay.
- **Pasted API token** (v1): Account › Workers Scripts › Edit, Workers KV Storage › Edit, Account Settings › Read.
  Link to the dashboard's token page with the right template.
- **Sign in with Cloudflare** (later; OAuth 2, PKCE, no client secret): `dash.cloudflare.com/oauth2/auth` and
  `/oauth2/token`. **New OAuth clients are private** (only members of the owning account can authorize). Making
  one public needs a logo, a client URL and a DNS TXT domain verification, and is permanent. Exact scope names:
  list with `GET /oauth/scopes`. Loopback redirect URIs and token lifetimes aren't documented: test first.
- **Temporary accounts** (no signup, API live since 2026-07-14): `POST /provisioning/previews/challenge`, solve the
  proof-of-work, `POST /provisioning/previews`. Durable Objects work there. The user must claim within 60 minutes or
  it's deleted, and after claiming we lose deploy access. Good for a "try it now" demo, not for the real setup.

## Limits to design around (Workers Free, from the research)
- 100k Worker requests/day; 100k Durable Object requests/day; 13,000 GB-s duration/day; 5 GB storage.
- **Writes run out first.** KV: 1,000 writes/day (OAuth grants and refreshes only; never per request). Durable
  Object SQLite: 100k rows written/day, and every index update counts as an extra row. Keep usage counters in
  memory and flush them about once a minute; rate-limit with GCRA in the Durable Object (one timestamp per key).
- Incoming WebSocket messages count as requests at 20:1; outgoing messages and incoming pings are free. Streamed
  chunks are agent → relay, so they count: batch small chunks.
- WebSocket messages up to 32 MiB. Cap request bodies (e.g. 20 MiB) and say so in errors.
- Code deploys and Cloudflare restarts drop every socket: the agent must always reconnect.
- No documented WebSocket idle timeout: heartbeat every ~30 s anyway.
- Show usage against these limits in `toprelay status`, and return a clear error when a limit is hit. Don't
  promise unlimited free use.

## Security
- The relay terminates TLS, so traffic is **not** end-to-end encrypted from the bot to the app: it passes through
  Cloudflare on the user's own account. The relay logs nothing and stores only OAuth grants. Say this plainly in
  the README.
- Remote-client auth on every service (see Credentials); rejected before any Durable Object call.
- Apps should treat relayed calls as remote: TopRelay sets `cf-connecting-ip` / `x-forwarded-for`, and signs every
  relayed request with `X-TopRelay-Caller` (HMAC, per-service key) so the app can trust it and apply its remote
  rules (ezRecord: remote token, no deletes, library-only paths). See `MIGRATION.md`.
- Tool allow-list in the agent; path allow-list for raw HTTP services.
- Body cap, per-request timeout, per-service rate limit, header allow-list.
- Don't add `Origin` handling that lets browsers drive local apps through the relay.
- `toprelay rotate` for compromised keys; `toprelay remove` revokes everything for a service.

## Bot compatibility
One endpoint shape (Streamable HTTP MCP) for every bot; only the auth and setup steps differ. Fill this table only
from real end-to-end tests, and ship it in the README. Don't claim a bot works until it's tested.

| Bot | How it connects | Auth | Tested |
|---|---|---|---|
| claude.ai custom connector | Remote MCP URL, Streamable HTTP or SSE | OAuth (CIMD recommended, DCR); static headers in beta for some orgs | |
| Claude API MCP connector | `mcp_servers[].url` | `authorization_token` (bearer) | |
| ChatGPT custom MCP app | Public HTTPS MCP URL (plan/workspace permitting) | OAuth or none | |
| OpenAI Responses API | `server_url` | `headers` / bearer | |
| xAI / Grok API | Remote MCP URL, Streamable HTTP or SSE | bearer header | |
| Gemini API / Gemini CLI | Streamable HTTP only | `headers` / `credential` | |
| Alibaba Model Studio (Qwen) | SSE only | `headers` | |
| Mistral Le Chat | Remote MCP URL | bearer, basic, OAuth or none | |
| Generic MCP client (go-sdk, TS SDK) | Streamable HTTP | either | |

ChatGPT users can also use OpenAI's Secure MCP Tunnel. Say so: TopRelay is for when you want one endpoint for every
bot, or a public endpoint OpenAI's tunnel can't provide.

## Build plan
1. **Worker, generic** (1 day): `/<service>/connect` + `/<service>/…`, agent key check, bearer key check, 503
   offline, timeouts, body cap, header allow-list, streaming chunks, cancel. Tests with vitest + workerd (copy
   KekasaiRelay's setup).
2. **OAuth spike first** (½ day, before step 3; in `spike/oauth/`): `workers-oauth-provider` with CIMD and DCR in
   front of a hello-world MCP server on a Worker, connected from claude.ai. Confirms the auth model and the 10 ms CPU budget before the rest is built on it.
3. **OAuth in the relay** (1 day): metadata endpoints, DCR, consent page with pairing code, KV, revoke on remove.
4. **Go agent** (1½ days): `login` (API token), `add --mcp-url | --stdio | --http`, `up` (reconnect, wake
   detection), `list`, `status`, `logs`, `remove`, `rotate`, `key`, `pair`. Keychain storage.
5. **Deploy from the agent** (½ day): the five REST steps plus KV namespace; idempotent.
6. **MCP layer in the agent** (1 day): `stdio` bridge on `go-sdk` (stateless on), tool allow-list over JSON and
   SSE, `tools` command, call log.
7. **`doctor` and `test`** (½ day), with an actionable message for every failure (local service down, relay not
   deployed, bad key, expired token, limit reached).
8. **Release gate** (1 day): the checks below, and the compatibility table filled in.
9. **TopRelayKit** (½ day): the Swift client as a package; swap ezRecord's `RelayClient` over to it.
10. **Polish**: `up --install` (launchd/systemd), Homebrew tap, Sign in with Cloudflare, Windows.

## v1 release gate
1. A local `stdio` MCP server and a local Streamable HTTP MCP server, both reached through the relay.
2. Discovery and a harmless tool call from a generic MCP client (2025-11-25 and 2026-07-28), from claude.ai over
   OAuth, from the Claude API and xAI with a bearer key, and from ChatGPT where the account allows.
3. Local app offline → immediate, clear error. Reconnects on its own after sleep, a network change and a redeploy.
4. Bad remote credential rejected before the Durable Object is called; `rotate` and `remove` take effect at once.
5. Concurrent calls, JSON replies, SSE streams, client cancellation, oversized bodies, timeouts, header filtering.
6. A disallowed tool is hidden from `tools/list` and refused on `tools/call`.
7. Logs carry useful metadata and no credentials or bodies.

## Deferred (not v1)
- **OpenAPI → MCP tools** (v1.1): `toprelay add crm --openapi ./openapi.yaml --base-url http://127.0.0.1:8080`.
  A documented subset of JSON APIs, operations chosen by the user, upstream auth from the keychain, write operations
  off by default. Study agentgateway's OpenAPI example first. Build it when a real user brings a spec.
- GUI automation for apps with no API.
- A marketplace or catalogue of integrations (Composio's territory).
- A2A gateway, model routing, workflows, enterprise registry (agentgateway and ContextForge cover these).
- Relay backends other than Cloudflare.
- Team accounts, multi-user policies, enterprise SSO.
- SDKs beyond TopRelayKit until the protocol and security model are stable.

## What to lift from existing code
| Need | Source (our repos) |
|---|---|
| Worker + Durable Object relay | `ezRecord/ezRecord/Remote/relay-worker.js` (single service; generalise to `/<service>/…`) |
| Deploy over the REST API | `ezRecord/ezRecord/Remote/RelayDeployer.swift`; Musedrop `MuseDrop/Services/CloudflareDeploy/WorkersAssetsDeployer.swift` |
| API client, retries, Keychain credential | `ezRecord/ezRecord/Remote/CloudflareAPI.swift` (from Musedrop `CloudflareAPI.swift`) |
| OAuth (PKCE, loopback redirect) | Musedrop `MuseDrop/Services/CloudflareDeploy/CloudflareOAuth.swift` |
| No-signup trial | Musedrop `MuseDrop/Services/CloudflareDeploy/CloudflareTemporaryAccount.swift` |
| Laptop client (Swift) | `ezRecord/ezRecord/Remote/RelayClient.swift` |
| Durable Object WebSocket room + tests | Musedrop `MuseDrop/Resources/Slides/live-worker.js`; `KekasaiRelay/` (vitest + workerd tests) |
| Research | `ezRecord/docs/research/cloudflare-relay-2026-10.md` |

## Outside repos
| Repo | Use |
|---|---|
| `modelcontextprotocol/go-sdk` (≥ v1.7.0) | Dependency: `stdio` bridge, JSON-RPC, both spec versions |
| `cloudflare/workers-oauth-provider` | Dependency: OAuth in front of the relay |
| `sparfenyuk/mcp-proxy` | Reference: stdio ↔ HTTP/SSE bridging and its interop tests |
| `openai/tunnel-client` | Competitor: study its onboarding, diagnostics and readiness checks |
| `agentgateway/agentgateway` | Reference: OpenAPI → MCP and authorization policy (for v1.1) |
| `docker/mcp-gateway` | Reference: local server management, profiles, secrets UX |
| `IBM/mcp-context-forge` | Long-term reference: registry, federation, governance. Don't rebuild it |
| `open-webui/mcpo` | Reference only: converts MCP → OpenAPI, the opposite direction |
| frp, rathole, bore | Tunnel protocol ideas; all need a VPS |

Competitive watchlist: OpenAI Secure MCP Tunnel, Anthropic MCP tunnels, ngrok, cloudflared, Tailscale Funnel
(and its custom-domain request, FR #11563), NetBird `expose`, Composio. TopRelay's
difference: your own Cloudflare account, free, stable URL, every bot, auth and tool control built in, no vendor in
the traffic path.

## Open questions
- Name and domain (needed for a public Cloudflare OAuth client and the consent page).
- OAuth consent for a single owner: is a pairing code from the CLI enough, or does claude.ai's flow need something
  else? Answered by build step 2.
- Does the OAuth token check fit in 10 ms CPU on Workers Free? Answered by build step 2.
- One Worker per user for all services (simple, current plan) vs. one per service (isolation).
- Free-plan request budget for chatty bots and streamed replies: 100k/day is plenty for one person; confirm with a
  measured session and show it in `toprelay status`.
- Does ChatGPT accept a bearer header for custom MCP apps, or only OAuth? The docs list static credentials; test.
- Which clients support URL elicitation / MRTR? Phone approval of write calls (v1.1) depends on it.
- Is the Rate Limiting binding on Workers Free? Not documented; GCRA in the Durable Object is the fallback.

## After v1 (see `docs/research/directions-2026-10.md`)
- **v1.1, a leash:** macaroon key narrowing, errors the model can explain, a phone-approval spike.
- **v1.2, one URL for all my apps:** an aggregated endpoint with BM25 `search_tools` in the agent and per-bot tool
  profiles of at most 20 tools.
- **v1.3, trust and privacy:** an RFC 6962 Merkle call log (`golang.org/x/mod/sumdb/tlog`) with checkpoints in the
  Durable Object; `--private` over `tsnet` (pin ≥ v1.104.4).
