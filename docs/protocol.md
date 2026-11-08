# TopRelay protocol, version 1.1

The contract between the relay Worker (`worker/`) and the laptop agent (`agent/`). Both sides must follow this file
exactly, and both test suites check the test vectors at the end. Change it only with a version bump.

**Versions.** 1.1 (2026-11) adds file links: the `files` capability, the `internal` field of `request` frames, and
the `GET /<s>/files/<id>` route. It is wire-compatible with 1.0: frames still carry `"v": 1`, unknown fields are
ignored, and each side uses 1.1 features only when the other advertised `files`. A 1.0 agent never receives an
internal request; a 1.1 agent on a 1.0 relay tells the model to have the owner run `toprelay deploy`.

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
| `GET /<s>/files/<id>?e=<exp>&s=<sig>` | the signature | `mcp` kind only: a single-use file download (see Files) |
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
{"v":1,"type":"hello","agent":"toprelay/0.1.0","caps":["files"],"kind":"mcp","tools":["search","read"]}
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
{"v":1,"type":"welcome","caps":["files"],"maxBody":20971520}
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

`internal` (1.1) marks a request the agent answers itself, never forwarding it to the app. The only value is
`"file"` (a download, see Files). An agent must answer an `internal` value it doesn't know with `404`, never pass
it to the app. The relay sends `internal` requests only to an agent whose hello had the `files` capability.

`cancel` is sent when the bot goes away. The agent aborts the upstream request, sends nothing more for that id, and
needn't send `end`.

### Lifecycle

- A new connection for a service closes the old one with close code `4000` ("replaced").
- When the socket closes, every pending request fails at once with `503` (JSON-RPC body for MCP paths). Pending
  requests are never retried, because they may already have changed the user's data.
- The agent reconnects with exponential backoff and full jitter: a delay of `random(0, min(30 s, 0.5 s × 2^n))`.
  It reconnects immediately after the computer wakes from sleep, which it detects as a wall-clock jump of more than
  15 s over a 5 s ticker.

## Files (1.1)

An app hands a file to a bot by returning an MCP `resource_link` content item in a `tools/call` result whose
`uri` is `toprelay-file:///<relative path>` (a file in the service's outbox directory) or `toprelay-file://<id>`
(a file registered on the agent's control socket). Only `mcp-url` services rewrite links. The agent replaces the
`uri` with:

```
https://<public host>/<service>/files/<id>?e=<exp>&s=<sig>

id       = 32 lowercase hex (random, a fresh one per link)
exp      = unix seconds, at most 15 minutes ahead (the agent uses 10)
file_key = HMAC-SHA256(b64url_decode(BEARER_ROOT), "toprelay/files/v1")
sig      = b64url(HMAC-SHA256(file_key, "toprelay/file/v1/" + service + "/" + gen + "/" + id + "/" + exp))
```

It also sets `mimeType` and `name` if the app didn't, and `size`. A link that can't be made (outside the outbox,
missing, over 256 MiB, not the agent user's, the relay too old) becomes a `text` item saying why.

The Worker serves `GET /<s>/files/<id>` without OAuth (the signature is the credential): it checks the id
format, the signature, that `exp` is in the future and no more than 15 minutes (plus 60 s skew) ahead, all before
any Durable Object call (`404` bad form, `403` bad signature, `410` expired). The Durable Object needs an agent with
`files` (`503` otherwise) and makes the link single-use: it records the id (until its `exp`) when it sends the
request to the agent, and forgets it again if the request ends before the agent's response head arrives
(offline, timeout, bot gone), so such a link can be retried. A fetch of a recorded id is `410`. It sends:

```json
{"v":1,"type":"request","id":"…","method":"GET","path":"/files/<id>","query":"","internal":"file","headers":{…}}
```

The agent serves each id once, only for its own service, and only if the file still has the device, inode, size
and modification time it had when the link was made (`410` otherwise). For an offered file that identity is taken
when the link is made, not when it was offered. Outbox paths are percent-decoded. A file that can't be linked
becomes a text item without local paths in it. It answers with `content-type` and
`content-disposition: attachment; filename=…; filename*=UTF-8''…`, streamed like any response. The relay then
forces `content-disposition: attachment`, `x-content-type-options: nosniff`,
`content-security-policy: default-src 'none'; sandbox`, `cache-control: no-store` and `referrer-policy: no-referrer`,
because the file is served from the relay's own origin, next to the OAuth consent page.

Uploads are not part of 1.1. A safe upload needs request bodies streamed in frames (today a body is one base64
frame of at most 20 MiB), so it is reserved as the `upload` capability for a later version.

## Agent → local app

What a local app receives for a relayed request. This is the contract apps code against; it doesn't change the
frames, so it has no frame version of its own.

**Endpoint.** `--mcp-url` and `--http` take `http://`, `https://` or `unix:///absolute/path.sock`. A socket URL's
HTTP path is `/mcp` for `--mcp-url` and `/` for `--http`, or `?path=/other` (the only query parameter allowed).
Before every new connection to a socket the agent checks that it is a socket owned by the agent's user with no
group or other permission bits, in a directory owned by that user (or root) that group and others can't write to;
after connecting it checks the peer's user with `LOCAL_PEERCRED` (macOS) or `SO_PEERCRED` (Linux). Sockets are
refused on other systems.

**Pin (macOS, optional).** With a pin `{teamId, bundleId?}`, for every new connection, before sending on it,
the agent finds the process on the other end (TCP: `lsof -nP -iTCP:<the connection's local port>
-sTCP:ESTABLISHED -F pn`, keeping the socket whose local port is the app's port and whose remote port is the
connection's; none visible within 0.5 s, or several, is refused; socket: `LOCAL_PEERPID`) and runs
`codesign --verify -R='anchor apple generic and certificate leaf[subject.OU] = "<teamId>" [and identifier "<bundleId>"]' <pid>`.
A passing pid is cached for 5 minutes. A bundle id alone is refused: anyone can sign with any identifier.

**Host.** The `Host` header is the app's own: the URL's `host[:port]` exactly as configured (e.g.
`127.0.0.1:7879`), or `localhost` for a socket. Never the relay's host, so apps that block DNS rebinding by
requiring a loopback `Host` keep working.

**Headers.** The request frame's headers (the relay's allow-list plus what it adds), so the app gets
`x-forwarded-for`, `cf-connecting-ip`, `x-forwarded-proto: https`, `x-forwarded-host: <relay host>` and
`x-toprelay-client`. Hop-by-hop headers are removed. Then:

- the service's **credential header**, if configured: a value from the keyring, or the trimmed content of a file
  read on every request (an absolute path to a regular file owned by the agent's user, mode without group/other
  bits, at most 8 KiB, no control characters). If it can't be read, the request is not sent: a single `tools/call`
  gets a tool error, anything else `502`.
- **`X-TopRelay-Caller`**, on every relayed request to an `--mcp-url` or `--http` service. Any incoming header of
  that name is removed first (the Worker never forwards it either). The agent's own requests (tool discovery,
  `doctor`, `test`) carry the credential but never this header.

```
X-TopRelay-Caller: v1; client=<enc>; ts=<unix seconds>; nonce=<32 hex>; sig=<64 hex>

sig = lowercase hex HMAC-SHA256(key, "toprelay-caller-v1" \n METHOD \n request-target \n <enc> \n <ts> \n <nonce> \n hex(SHA-256(body)))
```

- `key` is the bytes of the service's caller key: the trimmed content of `<config dir>/caller-keys/<service>.key`
  (43 characters, base64url of 32 random bytes, mode 0600), or of an app-supplied file (at least 32 characters).
  `toprelay caller-key <service>` prints it.
- `request-target` is the path and query exactly as on the HTTP request line.
- `<enc>` is the `x-toprelay-client` value (an OAuth client id, or `key:<id>`) with every byte outside
  `A-Z a-z 0-9 - . _ ~ : / @` written as `%XX` (uppercase hex). It is signed in this encoded form.
- `body` is the exact request body (empty for none).

A verifier must: split on `;`, require `v1` first, require `ts` to be digits within 60 s of its clock, recompute
`sig` and compare in constant time, then percent-decode `client`. A verifier that wants strict single use can
remember `nonce`s for 2 minutes. Verifiers in Go, Swift, Python and Node: `docs/verifiers/` and `MIGRATION.md`.

`stdio` services are not HTTP and get no header.

## Reserved for later versions

Not built in v1. Neither side may use these names for anything else:

- caps `"upload"` (streamed request bodies for uploads), `"binary"` (binary frames instead of base64), `"credit"` (frames carrying per-stream flow-control credit),
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

file link, BEARER_ROOT as above, service "notes", gen 1, id "00112233445566778899aabbccddeeff", exp 1793500900:
  sig = tBkr0_7eqyIbMb0KVCASzymzU0--l_J_ECGXjWMnLlY

X-TopRelay-Caller, key = "tr-caller-test-key-0123456789abcdefghijkl" (its UTF-8 bytes),
POST "/mcp?x=1", client "https://claude.ai/oauth/mcp client;x", ts 1793500000,
nonce "000102030405060708090a0b0c0d0e0f", body {"jsonrpc":"2.0","id":1,"method":"tools/list"}:
  v1; client=https://claude.ai/oauth/mcp%20client%3Bx; ts=1793500000; nonce=000102030405060708090a0b0c0d0e0f; sig=d42479b952efe9b90dfb5336b0cf2074c0e6fe4b578a9feb3b5c68fef7058f13
```

The payload JSON in these vectors is compact (`{"i":…,"c":[…]}`, with no spaces). Verifiers must accept any valid
JSON. Minters must produce this compact form.
