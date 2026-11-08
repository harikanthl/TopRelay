# Relay Worker

The public side of TopRelay. The `toprelay` agent deploys it to the owner's own Cloudflare account as the Worker
`toprelay`. Contract: [`docs/protocol.md`](../docs/protocol.md).

| File | What |
|---|---|
| `src/index.ts` | Routing and credential checks, including signed file-download links. Bodies are streamed to the Durable Object, never read here |
| `src/relay.ts` | `Relay` Durable Object, one per service: the agent's hibernatable WebSocket, request/stream relaying, cancellation, timeouts, offline and disallowed-tool answers, single-use file downloads, GCRA rate limit, pairing codes, revoked keys, usage counters |
| `src/oauth.ts` | One OAuth authorization server (CIMD + DCR), one protected resource per service, consent page with pairing code |
| `src/keys.ts` | Agent keys and macaroon-style bearer keys |
| `src/gcra.ts`, `src/headers.ts`, `src/mcp.ts`, `src/config.ts` | Rate limiting, header allow-lists, JSON-RPC answers, bindings |

## Develop

```sh
npm install
npm test            # workerd: key vectors, routing, relaying with a fake agent, streaming, cancel, OAuth flow
npm run typecheck
npm run bundle      # dist/relay.js, copied to agent/internal/deploy/relay.js (embedded in the Go binary)
```

Run `npm run bundle` after every change in `src/`, and commit the updated `agent/internal/deploy/relay.js`.
CI fails if they differ.

For `wrangler dev`, put the two roots in `.dev.vars` (any 32 random bytes, base64url):

```
AGENT_ROOT=…
BEARER_ROOT=…
```

`wrangler.jsonc` is for local development only. Real deployments get their bindings from the agent.

## Free-plan budget

- Each relayed call is 1 Worker request plus 1 Durable Object request.
- The agent's streamed chunks count as incoming WebSocket messages, billed at 20:1.
- Keepalive pings are answered automatically, without waking the object.
- Usage counters are written at most once a minute.
- KV is only touched by OAuth (sign-in and token refresh), never on each call.
