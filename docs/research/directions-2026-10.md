# What TopRelay could become (2026-10-31)

A synthesis of three research notes in this folder. Each claim below is sourced there:
- `tailscale-and-tunnels-2026-10.md`: Tailscale Funnel, tsnet, tsidp, Aperture, and the other tunnels.
- `agent-ecosystem-2026-10.md`: OpenAI, Anthropic, Google, xAI, Mistral, Qwen, DeepSeek, Kimi, GLM and MiniMax,
  plus the MCP spec and the A2A, Skills and AGENTS.md standards.
- `algorithms-for-linking-2026-10.md`: data structures and algorithms mapped to features.

## What the research changed

1. **The gap is real and still open.** OpenAI's Secure MCP Tunnel and Anthropic's MCP tunnels each work only with
   their own vendor's products.
   - Anthropic's tunnel is Enterprise-only and in research preview.
   - OpenAI's is limited to 120 s per request and 600 requests a minute.
   - Both are aimed at company networks.
   - Both leave auth to the MCP server.

   Nobody gives one person a single endpoint, usable from every bot, with auth in front of a server that has none.
2. **Tailscale isn't the answer for cloud bots.**
   - Funnel requests carry no caller identity.
   - Funnel only gives `*.ts.net` names and is still in beta.
   - The free plan is for non-commercial use.
   - tsidp is experimental.
   - Aperture only works inside a tailnet.

   Tailscale *is* the right answer for a private mode reaching the user's own devices.
3. **MCP 2026-07-28 helps a protocol-blind relay.** The required `Mcp-Method` / `Mcp-Name` headers let the Worker
   refuse a disallowed tool from headers alone. Sessions are gone, so the relay has less state to keep.
4. **The number of tools is the main limit on quality, more than access.**
   - Vendors say to keep it to 10–20 tools (Anthropic: accuracy drops past 30–50).
   - The best scores on multi-app MCP benchmarks are 39–53%.
   - Failures come from unfamiliar tools and long contexts, not from auth.
5. **The free tier's tight limits are on writes, not requests.** KV allows 1,000 writes a day. Durable Object
   SQLite allows 100k rows written a day, and every index update counts as an extra write. This shapes the design:
   tokens checked by signature instead of KV lookups, and counters that are saved in batches.

## Six directions

| # | Direction | One line | Verdict |
|---|---|---|---|
| A | **Personal MCP endpoint** (the current brief) | One local server → one stable URL, any bot | Foundation. Build it, with the corrections below |
| B | **One URL for all my apps** | Every local app behind one endpoint, with tool search and a separate tool set per bot | **Strongest product.** Solves the real quality problem |
| C | **A leash for agents** | Approve writes from your phone, give a bot a narrowed token, keep a tamper-evident log | **The thing no vendor has.** Pair it with B |
| D | **Link my devices** | Laptop ↔ phone ↔ home server apps, private, end-to-end encrypted | v1.x as `--private` (tsnet); Tailscale is strong here, don't fight it head-on |
| E | **Local Zapier** | App A's output feeds app B; queued webhooks while the laptop sleeps | Later. Big scope, and Zapier/n8n/Composio already own it |
| F | **Agent inbox (A2A)** | Other people's agents send tasks to yours | Watch. A2A v1.0 fits the same relay, but demand is unproven |

### B + C together: "your personal agent gateway"

> One URL that gives any AI the apps on your computer: only the tools you chose, only for as long as you
> allow, writes approved from your phone, and every call on a record that can't be quietly changed.

What makes it work:
- **Aggregated endpoint with `search_tools`.** BM25 runs over tool names and descriptions in the agent, and the
  bot sees ~10 core tools plus search. Tool search works on every bot, including ChatGPT, Grok and Qwen, which
  can't search their tool lists themselves.
- **Per-bot profiles.** `--profile chatgpt` gets its own key or URL and its own tool set of no more than 20 tools.
- **Phone approval for write calls** through MRTR or URL elicitation (in the 2026-07-28 spec). The call waits for
  the approval page the relay hosts. If the client doesn't support it, the bot gets an error result explaining
  that approval is needed. *Client support per vendor is unverified: test it first.*
- **Macaroon-style tokens.** `toprelay key notes --tools search,read --for 1h` creates a token offline that can
  only be narrowed further. The Worker checks it in microseconds with no KV write.
- **A tamper-evident call log.** It uses the scheme from Certificate Transparency (RFC 6962), with signed
  checkpoints in the Durable Object; Go's `x/mod/sumdb/tlog` provides it.
- **Errors the model can explain.** For example: "The user's Mac is asleep; ask them to open it". These come
  back as a tool result, so the bot can tell the user what to do instead of failing.
- **Generated `SKILL.md` / AGENTS.md** for each endpoint, so coding agents know how to use it.

Why it beats the alternatives:
- A vendor tunnel only reaches one vendor's bots, and a plain tunnel adds no control over callers or tools.
- Composio and docker/mcp-gateway manage MCP servers for developers, not a person's own apps.
- No vendor tunnel offers phone approval or tokens a user can narrow.

## Corrections to `docs/brief.md` before building

1. claude.ai accepts more than OAuth or no auth. It now recommends **CIMD** (Client ID Metadata Documents); DCR
   still works. Static headers, including `Authorization`, are in beta for some organizations. Turn on
   `clientIdMetadataDocumentEnabled` in workers-oauth-provider and keep DCR as a fallback.
2. Add **Anthropic MCP tunnels** to the competitors, and update the Tailscale Funnel row: TLS ends on the device,
   there's no custom domain, and requests carry no caller identity.
3. **Worker:** refuse disallowed tools based on `Mcp-Name`. The agent keeps the body-level check and must keep the
   `tools/list` order and the `ttlMs`/`cacheScope` cache hints intact.
4. **stdio bridge:** add a legacy SSE endpoint, because Alibaba Model Studio only accepts SSE. Gemini only accepts
   Streamable HTTP. Add both to the compatibility table, plus Mistral Le Chat.
5. **Wire protocol v1:** reserve binary frames, per-stream sequence numbers, credit frames, and request ids unique
   across reconnects. Without these, resume-instead-of-retry and flow control can't be added later without
   breaking the protocol.
6. **Budgets:** use GCRA rate limiting in the Durable Object, save counters in batches rather than on every
   request, and reconnect with full jitter. Replace per-key bearer secrets with macaroons, so new keys don't need
   a redeploy.
7. **Name lint at `add`.** Use lowercase alphanumeric service names (Gemini rejects `-` and `_`) and tool names of
   at most 63 characters from `[A-Za-z0-9_-]`, with an optional prefix.
8. Keep the agent's MCP handler an `http.Handler` that doesn't care how requests arrive, so a tsnet listener can
   be added later at no cost.

## Suggested path

1. **v1:** A with the corrections above. Run the OAuth/CIMD spike against claude.ai first, as planned.
2. **v1.1:** C-lite: macaroon keys, errors the model can explain, a spike on phone approval.
3. **v1.2:** B: an aggregated endpoint with `search_tools` and per-bot profiles. This is where the product
   changes from "a tunnel" to "a gateway".
4. **v1.3:** a tamper-evident log, and `--private` over tsnet (the first step toward D).
5. **v2:** pick D or E based on what users actually connect.

## Open questions this raises

- Which clients actually support URL elicitation or MRTR today? Phone approval depends on it.
- Is the Rate Limiting binding available on Workers Free? (Not in the docs; GCRA in the Durable Object is the
  fallback.)
- Name: "TopRelay" fits A. It fits B + C less well, since those make it a gateway rather than a relay. Decide
  before buying a domain.
