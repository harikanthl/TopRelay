# TopRelay v1 protocol

The contract between the relay Worker (`worker/`) and the laptop agent (`agent/`). Both sides must follow this file
exactly, and both test suites check the test vectors at the end. Change it only with a version bump.

## Names

- **Service name**: `^[a-z0-9]{1,32}$`. Lowercase letters and digits only, because the Gemini API rejects `-` and
  Gemini CLI rejects `_`. Reserved: `health`, `authorize`, `oauth`, `well`, `admin`, `connect`, `mcp`, `toprelay`, `narrow`, `revoke`
  (the last two are `toprelay key` subcommands).
- **Service kind**: `mcp`, for Streamable HTTP MCP (an MCP URL or a `stdio` bridge), or `http`, for raw HTTP
  forwarding.
- **Generation** (`gen`): a positive integer per service, starting at 1. `toprelay rotate` increments it, which
  invalidates that service's agent key and every bearer key minted for it.

## Worker configuration (bindings)

The agent deploys the Worker as script `toprelay`, with these bindings:

| Binding | Type | Value |
|---|---|---|
| `RELAY` | `durable_object_namespace` | class `Relay` (SQLite-backed) |
| `OAUTH_KV` | `kv_namespace` | the account's `toprelay-oauth` namespace |
| `PUBLIC_URL` | `plain_text` | `https://toprelay.<subdomain>.workers.dev` (no trailing slash) |
| `SERVICES` | `plain_text` | JSON: `[{"name":"notes","kind":"mcp","gen":1}, …]` |
| `AGENT_ROOT` | `secret_text` | 32 random bytes, base64url without padding |
| `BEARER_ROOT` | `secret_text` | 32 random bytes, base64url without padding |

The deployment also sets:
- `main_module: "relay.js"`
- `compatibility_date: "2026-10-01"`
- `compatibility_flags: ["global_fetch_strictly_public", "enable_request_signal"]` (CIMD fetch safety; `request.signal`
  aborts when the bot disconnects, so the relay can send `cancel`)
- `observability: {"enabled": true, "logs": {"enabled": true, "invocation_logs": false}}`

The first deploy only (when the script doesn't exist yet) also adds
`migrations: {"new_tag": "v1", "new_sqlite_classes": ["Relay"]}`.

Adding, removing or rotating a service changes `SERVICES` and redeploys. A redeploy drops every agent socket, and
the agent reconnects.

## Credentials

### Agent key (laptop agent ↔ relay)

```
agent_key(service, gen) = "tra1." + b64url( HMAC-SHA256( b64url_decode(AGENT_ROOT), "toprelay/agent/v1/" + service + "/" + gen ) )
```

The agent sends it as `Authorization: Bearer <agent_key>` on `/<service>/connect` and `/<service>/admin/*`. The
Worker recomputes it from `SERVICES` and compares in constant time.

### Bearer key (bot ↔ relay), macaroon-style

```
token     = "trk1." + b64url(payload) + "." + b64url(sig)
payload   = UTF-8 JSON {"i": <key id>, "c": [<caveat>, …]}      (key id: 16 lowercase hex chars)
sig       = fold over caveats: s0 = HMAC-SHA256(b64url_decode(BEARER_ROOT), "toprelay/bearer/v1/" + i)
                               s(n) = HMAC-SHA256(s(n-1), caveat(n))
```

The signature covers the key id and the caveats in order, not the JSON bytes. The verifier parses the payload,
recomputes `sig` and compares in constant time.

**Narrowing** a key needs no root secret: append caveat `c'` to `c`, and set `sig' = HMAC-SHA256(sig, c')`.

Caveats. Every caveat must hold; an unknown caveat makes the key invalid, so it fails closed:

| Caveat | Rule |
|---|---|
| `svc=<name>` | Required, and must be the first caveat. Must equal the service in the request path. |
| `g=<gen>` | Required, and must be the second caveat. Must equal that service's current `gen`. |
| `exp=<unix seconds>` | Optional, repeatable. The request time must be before every `exp`. |
| `tools=<a,b,…>` | Optional, repeatable. Allowed tools are the intersection of every `tools` caveat. |

A key is also invalid if its id is on the service's revocation list (see `admin/revoke-key`).

Keys are base64url without padding everywhere. b64url uses the URL-safe alphabet (`-`, `_`).

### OAuth (bot ↔ relay)

There is one authorization server, with issuer `PUBLIC_URL`. Each service is its own protected resource:
- `mcp` kind: `PUBLIC_URL + "/" + name + "/mcp"`
- `http` kind: `PUBLIC_URL + "/" + name`

Scopes: `scopesSupported = ["mcp", "offline_access"]` and `requiredScopes = ["mcp"]`. CIMD is on, DCR is on at
`/oauth/register`, and the token endpoint is `/oauth/token`.

The grant's `userId` is `svc-<name>` (the OAuth library forbids `:`), and its props are `{"service": "<name>"}`. The handler refuses a token whose
props name another service, as a second check on top of the audience.

**Pairing codes** are normalised before hashing, both when set and when typed: uppercase, with spaces and `-`
removed. The agent generates 12 characters from `ABCDEFGHJKMNPQRSTUVWXYZ23456789` (no 0/O, 1/I/L), and may show
them grouped (`ABCD-EFGH-JKMN`). A code works once.

The consent page at `/authorize` finds the service from the authorization request's resource. It asks for a
**pairing code**, which the Durable Object checks (see `admin/pair`).

## HTTP routes (Worker)

| Route | Auth | Behaviour |
|---|---|---|
| `GET /health` | none | `200 ok` |
| `GET /.well-known/oauth-authorization-server` | none | RFC 8414 metadata |
| `GET /.well-known/oauth-protected-resource/<resource path>` | none | RFC 9728 metadata for that service |
| `GET, POST /authorize` | none | Consent page with pairing code |
| `POST /oauth/token`, `POST /oauth/register` | none | OAuth provider |
| `GET /<s>/connect` | agent key | WebSocket upgrade to the service's Durable Object |
| `GET /<s>/admin/status` | agent key | JSON status (below) |
| `POST /<s>/admin/pair` | agent key | Body `{"code": "<≥ 8 chars after normalising>", "ttl": <seconds, 60–3600, default 600>}` → `{"expires": <unix s>}` |
| `POST /<s>/admin/revoke-key` | agent key | Body `{"id": "<16 hex>"}` → `{"revoked": n}` (size of the list) |
| `POST /<s>/admin/revoke-oauth` | agent key | Revokes every OAuth grant for `svc-<s>` → `{"revoked": n}` |
| `* /<s>/mcp`, `* /<s>/mcp/…` | OAuth or bearer | `mcp` kind: forwarded to the agent |
| `* /<s>/…` (except `connect`, `admin`) | OAuth or bearer | `http` kind: forwarded to the agent |

An unknown service, or a path the service's kind doesn't serve, returns `404`. A missing or invalid credential
returns `401` with the RFC 9728 `WWW-Authenticate` challenge. Credentials are checked in the Worker, before any
Durable Object call. The relay sends no CORS headers and answers `OPTIONS` with `405`.

Admin status body:

```json
{"service":"notes","connected":true,"connectedAt":1793500000,"agent":"toprelay/0.1.0",
 "day":"2026-10-31","requests":123,"rejected":4,"bytesIn":5678,"bytesOut":91011,"revokedKeys":0}
```

### Request forwarding (Worker → Durable Object → agent)

**Request headers** forwarded to the agent (an allow-list, names lowercased):
`accept`, `content-type`, `user-agent`, `mcp-protocol-version`, `mcp-session-id`, `mcp-method`, `mcp-name`,
`last-event-id`. Everything else is dropped, `authorization` and `cookie` included.

The relay adds:
- `x-forwarded-for` and `cf-connecting-ip`: the caller's IP
- `x-forwarded-proto: https`
- `x-forwarded-host`: the relay's host
- `x-toprelay-client`: the OAuth client id, or `key:<id>` for a bearer key

**Response headers** returned from the agent (an allow-list): `content-type`, `cache-control`, `mcp-session-id`,
`location`, `etag`, `last-modified`, `content-disposition`, `retry-after`, `mcp-protocol-version`.

Limits:
- Request bodies: up to **20 MiB**. Larger ones get `413` before forwarding.
- A response whose headers don't arrive within **120 s** gets `504`.
- A streamed response with no chunk for **300 s** is ended.
- No request lasts longer than **840 s** in total.
- Rate limit, per service: GCRA at **20 requests/s, burst 60**. Over the limit gets `429` with `Retry-After`.
- A streamed reply may queue at most about **8 MiB** for a bot that reads slowly. Past that the stream is
  aborted and the agent gets `cancel` (v1 has no flow-control credit).
- A stream cut short by a timeout or the queue limit is **aborted, never closed cleanly**, so a truncated reply
  never looks complete.
- Paths containing `//`, `%2F` or `%5C` (any case) get `400`, since they would reach the agent as a different
  path than the one the relay routed.
- Pairing attempts: **5 per caller IP**, then 1 every 2 minutes; and **30 per service** from all IPs, then 1
  every 20 s. A `POST /authorize` is checked against its consent session (handle and cookie) **before** the code
  is, so a request without a real consent page uses up neither attempts nor the code. A wrong code shows the
  page again with a fresh handle.

### MCP-aware answers in the Durable Object

The relay reads a request body only in these two cases, and only to find the JSON-RPC `id`:

- **Agent offline.** A `tools/call` (from the `Mcp-Method` header, or the body's `method` when the header is
  absent) gets `200` with the JSON-RPC result
  `{"content":[{"type":"text","text":"<service> is offline: the computer running it is asleep or disconnected. Ask its owner to open it, then try again."}],"isError":true}`.
  Any other method gets `503` with the JSON-RPC error `{"code":-32000,"message":"<same text>"}`, and a non-MCP
  path gets `503` with that text as plain text.
- **Disallowed tool.** When `Mcp-Method` is `tools/call` and `Mcp-Name` is not allowed, the call gets `200` with
  the result `{"content":[{"type":"text","text":"The tool \"<name>\" is not available on <service>. Its owner hasn't allowed it."}],"isError":true}`.
  Two lists apply: the agent's allow-list (from `hello`) and the bearer key's `tools` caveats. A tool must be
  allowed by both.

If the body has no usable `id`, it is answered with `"id": null`.

## WebSocket frames (Durable Object ↔ agent)

Text frames, each one JSON object with `"v": 1` and a `"type"`. Unknown types and unknown fields are ignored, so
either side can be newer. Bodies and chunks are base64 (standard alphabet, with padding). Maximum frame size:
**32 MiB**.

Keepalive: the agent sends the text `ping` every 30 s, and the Durable Object answers `pong` automatically
(without waking from hibernation). If no `pong` arrives within 10 s, the agent reconnects.

### agent → relay

```json
{"v":1,"type":"hello","agent":"toprelay/0.1.0","caps":[],"kind":"mcp","tools":["search","read"]}
```
This must be the first frame. `tools` is the agent's allow-list, or `null` for "allow all" (always `null` for the
`http` kind). The relay keeps it on the socket, so it survives hibernation. The agent resends `hello` whenever the
allow-list changes.

```json
{"v":1,"type":"response","id":"…","status":200,"headers":{"content-type":"application/json"},"body":"<base64>"}
{"v":1,"type":"response","id":"…","status":200,"headers":{"content-type":"text/event-stream"},"stream":true}
{"v":1,"type":"chunk","id":"…","data":"<base64>"}
{"v":1,"type":"end","id":"…"}
{"v":1,"type":"end","id":"…","error":"upstream closed"}
```
A response is either whole (`body`, which may be omitted when empty), or `stream: true` followed by `chunk`
frames and one `end`. The agent merges small chunks: it flushes at 64 KiB or 10 ms after the first unflushed
byte, whichever comes first. A chunk's data is at most 1 MiB before encoding.

### relay → agent

```json
{"v":1,"type":"welcome","caps":[],"maxBody":20971520}
{"v":1,"type":"request","id":"k3x9-17","method":"POST","path":"/mcp","query":"","headers":{…},"body":"<base64>","tools":["search"]}
{"v":1,"type":"cancel","id":"k3x9-17"}
```

`welcome` answers `hello`.

In a `request` frame:
- `path` is the part after `/<service>`: `/mcp` for the MCP endpoint, or `/` and below for `http` services.
- `query` is the query string without its `?`.
- `tools` is present only when a bearer key restricts tools. The agent must also refuse any tool outside it,
  checking the body, so clients without `Mcp-Name` are covered too.
- `id` is generated by the Durable Object: a random incarnation prefix plus a counter. It is never reused while
  the object is alive, so a reconnect can't mix up two requests.

`cancel` is sent when the bot goes away. The agent aborts the upstream request, sends nothing more for that id, and
needn't send `end`.

### Lifecycle

- A new connection for a service closes the old one with close code `4000` ("replaced").
- When the socket closes, every pending request fails at once with `503` (JSON-RPC body for MCP paths). Pending
  requests are never retried, because they may already have changed the user's data.
- The agent reconnects with exponential backoff and full jitter: a delay of `random(0, min(30 s, 0.5 s × 2^n))`.
  It reconnects immediately after the computer wakes from sleep, which it detects as a wall-clock jump of more than
  15 s over a 5 s ticker.

## Reserved for later versions

Not built in v1. Neither side may use these names for anything else:

- caps `"binary"` (binary frames instead of base64), `"credit"` (frames carrying per-stream flow-control credit),
  `"resume"` (outcome replay by id after a reconnect)
- frame types `credit`, `resume`, `seq`

## Test vectors

```
AGENT_ROOT  = AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8          (bytes 0x00..0x1f)
BEARER_ROOT = ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8          (bytes 0x20..0x3f)

agent_key("notes", 1) =
  tra1.TA3-AhWB6Ka9MRN31Tk1D5C0kgDAgB4eoiJpZWwq7Aw

bearer, i = "0123456789abcdef", c = ["svc=notes","g=1"]:
  trk1.eyJpIjoiMDEyMzQ1Njc4OWFiY2RlZiIsImMiOlsic3ZjPW5vdGVzIiwiZz0xIl19.ByA-aoTDoDDxKP2uBxEVX5f_pNZRme7o3HaKWh6kn3I

the same key narrowed with "tools=search,read", then "exp=4102444800":
  trk1.eyJpIjoiMDEyMzQ1Njc4OWFiY2RlZiIsImMiOlsic3ZjPW5vdGVzIiwiZz0xIiwidG9vbHM9c2VhcmNoLHJlYWQiLCJleHA9NDEwMjQ0NDgwMCJdfQ.x26sPAObXFuxuW6XUmyku29lI8HiecbJgLrwd32DRxg
```

The payload JSON in these vectors is compact (`{"i":…,"c":[…]}`, with no spaces). Verifiers must accept any valid
JSON. Minters must produce this compact form.
