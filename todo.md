# TopRelay build order

Each phase depends on the one before it. Research behind it: `docs/research/apple-networking-2026-11.md`,
`docs/research/agent-harnesses-2026-11.md`, `docs/research/token-saving-and-tool-learning-2026-11.md`.

## Open decisions

- [ ] Add `argsHash` (HMAC with a key that never leaves the Mac) and `toolError` to the call log. Needed before
      Phase 3.
- [ ] Phone alerts: Web Push (VAPID, stays in the user's account) or native APNs (needs a tiny relay we run).
      Needed before Phase 5.

## Phase 0: ship what's done (today)

- [ ] Commit the ezpdf fixes as 1.0.3: Reconnect updates in place with `set-header`, and the token is read from a
      file (`--header-file`).
- [ ] Commit the three research notes.
- [ ] Real-bot test: ezpdf from Grok, claude.ai and ChatGPT, following `docs/bot-testing.md`. Fill in the
      compatibility table in `docs/brief.md`.
- [ ] Fix whatever the test breaks. Likely suspects: Grok's OAuth flow, and ezpdf only speaking MCP 2025-11-25
      (not the stateless 2026-07-28 version).

## Phase 1: finish v1 (about 1 week)

- [ ] Byte-stable tool lists in `mcpproxy`: fixed order, fixed descriptions, correct `ttlMs`/`cacheScope`, policy
      changes applied in one go, so bots' prompt caches survive.
- [ ] Result size cap, with the rest reachable through a `resource_link` or `read_more`. Never cut silently.
- [ ] Loop detection (the same tool with the same arguments repeated): log a warning.
- [ ] App identity from audit tokens (`LOCAL_PEERTOKEN` plus Security.framework via purego) instead of PID +
      `codesign`, which closes the PID-reuse race in `upstream/pin.go`.
- [ ] Test whether connecting to a socket under `~/Library/Containers/<app>/Data/` triggers the macOS 14+
      app-data prompt. If it does, change the recommended socket location in `agent/README.md`.
- [ ] Pass the v1 release gate in `docs/brief.md` and release.

## Phase 2: effortless app integration (about 2 weeks)

- [ ] `TopRelayMCP` Swift product, separate so TopRelayKit stays dependency-free. It:
  - [ ] listens on a unix socket in the app's container;
  - [ ] checks the signed `X-TopRelay-Caller` header;
  - [ ] speaks MCP 2026-07-28 and 2025-11-25;
  - [ ] optionally takes tool definitions shared with Foundation Models `Tool`. Test the JSON Schema form with
        each bot.
- [ ] Move ezpdf onto TopRelayMCP, deleting its hand-written server (about 1,650 lines).
- [ ] Register apps with a manifest file plus a code-signature check, not Bonjour, so TopRelay finds apps
      without `add` commands.
- [ ] Bundle the agent inside an app with `SMAppService`, so users never install the CLI by hand.

## Phase 3: one endpoint for all apps (about 1 week)

- [ ] `/all/mcp` with `search_tools` (BM25 in the agent), plus each bot's usual tools ranked from the call log
      (use counts that fade over time, and which tool follows which).
- [ ] Call-log fields `argsHash` and `toolError`, if approved above.

## Phase 4: built-in assistant (about 2 weeks)

- [ ] `agent/internal/brain`, ported from Operator's `AgentLoop`:
  - [ ] round budget plus a final no-tools grace call;
  - [ ] models through the official Go SDKs (check licenses) or OpenAI-compatible endpoints;
  - [ ] tool calls through the in-process proxy, so allow-lists, pins, shield and audit apply;
  - [ ] independent read-only calls run in parallel.
- [ ] Prompt order providers can cache: tools → system → skills index → memory snapshot frozen per session →
      conversation. Choose 5-minute or 1-hour caching by usage pattern.
- [ ] Memory: port OKF's notebook, extraction rules and store to Go. Keep the MIT notices for Hermes and
      `yc-software/qm`.
- [ ] Skills: the Agent Skills `SKILL.md` spec in `~/.agents/skills/`.
  - [ ] TopRelay-only fields go under `metadata`.
  - [ ] Skills are pinned and scanned like tools, and can only narrow the tool allow-list.
  - [ ] Skill scripts don't run.
- [ ] Sessions in SQLite (`ncruces/go-sqlite3`).
- [ ] `toprelay ask` plus a chat pane in the Mac app. Apple's model routes (4,096-token limit), the user's key
      does the work.
- [ ] Local models: Ollama `keep_alive` and explicit `num_ctx`, llama.cpp prompt cache, Apple `prewarm`.

## Phase 5: phone and inbox (about 2–3 weeks)

- [ ] `/_control` route plus a control Durable Object:
  - [ ] pairing in person (QR code or 6-digit code), then Noise end-to-end encryption between phone and Mac;
  - [ ] the relay only passes opaque bytes;
  - [ ] a fixed list of allowed actions, never the full `/v1/cli`;
  - [ ] a signature from a paired device on every command.
- [ ] Encrypted last-known status and queued alerts stored in the Durable Object.
- [ ] Web page served by the relay: status, live call feed, kill switch, held-pin approval, write approval from
      the phone.
- [ ] Inbox: phone commands start assistant runs. Then scheduled jobs and a nightly memory review.
- [ ] Proposed skills learned from repeated successful tool sequences; the user approves each one, and none can
      widen permissions.

## Later, only if users ask

- [ ] Native iOS app (push alerts, Face ID approval, widgets, Shortcuts).
- [ ] App Intents bridge through `shortcuts run`, for apps without MCP.
- [ ] Opt-in cache for read-only results (otter v2 plus `singleflight`), cleared by any write to the same app.
- [ ] Apple Events bridge with hand-written adapters per app.

## Don't build

- XPC transport.
- Bonjour discovery.
- Network Extension.
- TLS-PSK.
- Semantic caching.
- An assistant that runs shell commands.
