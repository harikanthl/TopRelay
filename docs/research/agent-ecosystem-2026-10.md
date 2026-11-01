# Agent ecosystem survey for TopRelay (mid-2025 to 2026-10)

Written 2026-10-31. Covers what the major labs and the MCP project shipped about agents calling tools and connecting
to apps, and what that means for a relay/connector product. Every claim has a source number (see Sources). Items
marked **[unverified]** come from third-party pages or couldn't be confirmed on a primary page. Where a primary page
gives no date, it says so.

## Summary

- **Both big vendors now ship their own tunnel.** OpenAI has Secure MCP Tunnel (`tunnel-client`, long-poll, works
  for ChatGPT, Codex and the Responses API) [5]. Anthropic has **MCP tunnels** (research preview, Enterprise or by
  request, built on Cloudflare `cloudflared` plus an Anthropic `mcp-proxy`, hostnames under `*.tunnel.anthropic.com`)
  [11][12]. Both serve only their own vendor's products, both target company networks (Docker/Helm, RFC 1918 targets),
  and both need the MCP server to do its own auth. Neither one serves a single person's laptop across every bot.
  That gap is still open.
- **MCP 2026-07-28 is stateless** [1]. There is no `initialize` handshake and no `Mcp-Session-Id`. A new
  `server/discover` method is mandatory. **`Mcp-Method` / `Mcp-Name` HTTP headers are now required**, so a
  protocol-blind Worker can see which tool is being called without reading the body. SSE resumability is gone,
  `ttlMs`/`cacheScope` cache hints are new, and DCR is deprecated in favour of CIMD.
- **The auth target has moved to CIMD.** claude.ai's recommended option is now "Use Claude's published identity"
  (a Client ID Metadata Document Anthropic hosts). DCR and your own client ID still work [10]. ChatGPT also supports
  CIMD, DCR and static credentials [7]. The Cloudflare Workers OAuth Provider has a
  `clientIdMetadataDocumentEnabled` option [36].
- **claude.ai can now send static headers** (up to four, including `Authorization`) on custom connectors. This is
  beta and only some organizations have it [10]. Until it's everywhere, OAuth is still the safe default for claude.ai.
- **Every developer API takes a bearer token**: Claude API `authorization_token` [3], OpenAI `authorization` [6],
  xAI `authorization`/`headers` [8], Gemini Interactions `headers`/`credential` [13][14], Alibaba Model Studio
  `headers` [20]. So does Mistral Le Chat (bearer/basic/OAuth/none) [16]. A per-service bearer key covers every API.
- **Transport support is uneven.** Gemini accepts **Streamable HTTP only, no SSE** [15]. Alibaba Model Studio's
  Responses API accepts **SSE only** (docs updated 2026-10-11) [20]. Everyone else takes both. A relay that bridges
  stdio has to serve both the old and the new transport.
- **Tool count is the main limit on quality.** Anthropic says selection gets worse past 30–50 tools [2]. OpenAI says
  "aim for fewer than 20" [22]. Google says keep 10–20 active [15]. Tool search with deferred loading now exists at
  Anthropic [2], OpenAI (gpt-5.4+) [4] and Kimi (K3/K3.1 dynamic tool loading) [26].
- **Code mode is going mainstream.** Anthropic's "code execution with MCP" [30] and programmatic tool calling [29],
  and Cloudflare's Code Mode server (2,500 endpoints exposed as two tools in about 1,000 tokens) [35]. The open
  `@cloudflare/codemode` package can wrap any existing MCP server in a single `code()` tool [35].
- **Reasoning models break if a proxy edits their history.** DeepSeek V4 returns a 400 error if `reasoning_content`
  isn't passed back when tools are present [23]. Kimi K3 needs the full assistant message back unchanged [27].
  MiniMax measured τ² dropping from 87 to 64 when thinking is dropped [32]. Self-hosted Kimi K2 tool calls failed
  over 80% of the time because of chat-template and tool-call-ID issues [28].
- **Benchmarks are still low** on real multi-app MCP work: MCPMark best pass@1 52.6% (pass^4 33.9%) [38],
  MCP-Universe 43.7% [39], Toolathlon 38.6% at release [40]. The common failures are long context, unfamiliar tools,
  and repetitive exploratory calls.
- **The protocols beyond MCP have settled.** A2A v1.0 (signed Agent Cards, `tenant`, device-code OAuth) moved into
  the Agentic AI Foundation in Aug 2026 [42]. AGENTS.md and MCP are founding projects of AAIF (Dec 2025) [43].
  Agent Skills (`SKILL.md`) is an open spec [44]. MCP Apps (interactive UI) became the first official MCP extension
  in Jan 2026 [45]. Enterprise-Managed Authorization (ID-JAG) has been stable since June 2026 [47] **[unverified
  date]**.
- **For TopRelay**, the most useful moves are:
  - enforce the allow-list in the Worker from `Mcp-Name`
  - support CIMD in the OAuth server
  - serve a legacy SSE endpoint for Qwen
  - lint service and tool names per client
  - offer per-bot tool profiles
  - use MRTR/URL elicitation for phone approval of write calls
  - position against the two vendor tunnels as "one personal endpoint for every bot"

## Per-vendor

### MCP project (spec, registry, extensions)

**2025-11-25 revision** [9]:
- OpenID Connect discovery for authorization servers.
- Incremental scope consent via `WWW-Authenticate` (SEP-835).
- Tool name guidance (SEP-986).
- URL-mode elicitation (SEP-1036).
- Tool calling inside sampling (SEP-1577).
- **CIMD as the recommended client registration** (SEP-991).
- Experimental **tasks** (SEP-1686).
- Input validation errors are returned as tool execution errors, so the model can correct itself (SEP-1303).
- Invalid `Origin` must get HTTP 403.
- JSON Schema 2020-12 is the default dialect.

**2026-07-28 revision** [1] (final; Cloudflare's 2026-08-06 post says it shipped "last week" [36]).
Major changes:
- Sessions and `Mcp-Session-Id` removed. Cross-call state goes in server-minted handles passed as tool arguments
  (SEP-2567).
- The `initialize` handshake is removed. Each request carries its protocol version, client capabilities and client
  info in `_meta`. Version mismatch returns `UnsupportedProtocolVersionError` (SEP-2575).
- **`server/discover` is required** on every server.
- The GET stream and `resources/subscribe` are replaced by **`subscriptions/listen`**, one long-lived POST response
  stream. Progress notifications still flow on the stream of the request they belong to.
- `ping` and `logging/setLevel` are removed. The log level is set per request in `_meta`.
- **Tasks moved to the `io.modelcontextprotocol/tasks` extension.** Results are polled with `tasks/get`, client input
  goes through `tasks/update`, `tasks/list` is gone, and servers may return task handles without being asked
  (SEP-2663).
- **Multi Round-Trip Requests (MRTR)**: instead of sending its own request, the server returns
  `resultType: "input_required"` with `inputRequests`, and the client retries the original request with
  `inputResponses`. This replaces server-initiated elicitation, sampling and roots (SEP-2322).
- **SSE resumability (`Last-Event-ID`) is removed.** If a stream breaks, the client must re-issue the request with a
  new ID.

Minor changes:
- `extensions` capability field.
- OpenTelemetry trace context in `_meta`.
- **Deterministic `tools/list` order (SHOULD)**, for prompt-cache hits.
- **Required `Mcp-Method` and `Mcp-Name` headers**, and `x-mcp-header` to map tool parameters into headers
  (SEP-2243).
- **Required `ttlMs` + `cacheScope` on list and read results** (SEP-2549).
- RFC 9207 `iss` check is MUST for clients.
- Credentials are bound to their issuer (SEP-2352).
- `inputSchema`/`outputSchema` may use any JSON Schema 2020-12 keyword, including `$ref` (SEP-2106).
- `elicitationId` and `notifications/elicitation/complete` are removed.
- New error code range: `HeaderMismatch` −32020, `MissingRequiredClientCapability` −32021,
  `UnsupportedProtocolVersion` −32022.

Deprecated (at least 12-month window): Roots, Sampling, Logging, HTTP+SSE transport, **DCR** (in favour of CIMD).
Cloudflare says DCR is "slated for removal after summer 2027" [36].

**Extensions and other projects**:
- **MCP Apps** (SEP-1865). `ui://` resources, tools point at them through `_meta.ui.resourceUri`, MIME type
  `text/html;profile=mcp-app`, CSP and sandbox metadata. Spec in `ext-apps/specification/2026-01-26` [45]. Described
  as co-developed by Anthropic and OpenAI and the first official extension **[secondary: SDK issue]**. Sources
  disagree on the extension ID (`io.modelcontextprotocol/ui` vs `/apps`).
- **Enterprise-Managed Authorization** (`io.modelcontextprotocol/enterprise-managed-authorization`, from SEP-990,
  ID-JAG / Okta "Cross App Access") [47]. Stable 2026-06-18 per secondary sources **[unverified date]**.
- **MCP Registry**: preview since 2025-09-08 [46]. The API froze at v0.1 on 2025-10-24. I found no GA announcement.
- **MCPB** (MCP Bundles): local servers packaged for Claude Desktop [10].

### Anthropic

- **MCP connector (Messages API)** [3]:
  - Beta header `mcp-client-2026-09-15` (or `mcp-client-2025-11-20`).
  - `mcp_servers[].url` and `authorization_token`, plus an `mcp_toolset` entry for allow/deny lists and
    `defer_loading`.
  - **Tools only.** The server must be public over HTTP; Streamable HTTP or SSE both work.
  - The OAuth flow is the caller's job.
  - The **2026-09-15 header records and pins each server's tool list**, so a server that changes its tools
    mid-conversation doesn't change what Claude sees. You can also pass a pinned `tools` copy.
  - With `inline-tools-2026-09-15` you can add a server mid-conversation.
  - Not ZDR-eligible.
- **claude.ai custom connectors** [10]:
  - All plans; Free gets one connector. On Team/Enterprise an Owner adds it.
  - Authentication: "Sign in now", "Sign in when needed", or "No sign-in".
  - OAuth client: **"Use Claude's published identity" (CIMD, recommended)**, "Register automatically" (DCR), or your
    own client ID.
  - **Request headers (beta, limited organizations)**: up to four approved header names, value sent verbatim
    (include `Bearer `), can be combined with OAuth (then `Authorization` isn't allowed). Anthropic approves custom
    header names.
  - Auth settings can't be edited after you add the connector; you remove it and add it again.
  - Calls come from Anthropic's cloud, so the URL must be public.
- **MCP tunnels** [11][12]:
  - Research preview, by request. claude.ai side needs Enterprise.
  - Messages API and Managed Agents through the Console/Tunnels API (`/v1/tunnels`).
  - Stack: `cloudflared` plus Anthropic's `mcp-proxy`. Inner TLS with a customer-held CA, so Cloudflare sees only
    ciphertext. Hostnames under `<sub>.<id>.tunnel.anthropic.com`, which accept connections only from Claude.
  - Limits: 10 tunnels per organization, 90-day server certificate, upstream must be IPv4 RFC 1918, Streamable HTTP
    only.
  - "The tunnel … does not authenticate to [the server]": OAuth on each MCP server is the customer's job.
  - No launch date given.
- **Tool search tool** [2]:
  - `tool_search_tool_regex_20251119` and `_bm25_20251119`.
  - Up to 10,000 deferred tools. A search returns 5 by default.
  - Selection accuracy degrades past 30–50 tools. Tool search cuts definition tokens by over 85%.
  - Keep the 3–5 most-used tools non-deferred and namespace tool names (`github_`, `slack_`).
  - Prompt cache is preserved. `defer_loading` can't be combined with `cache_control` on the same tool.
  - Stable. Not available in Managed Agents.
- **Advanced tool use** (2025-11-24) [29]:
  - Tool search, **programmatic tool calling** (Claude writes code that calls tools in a sandbox; −37% tokens) and
    **tool use examples** (`input_examples`; 72% to 90% on complex parameters).
  - MCP eval accuracy with tool search: Opus 4.5 went from 79.5% to 88.1%.
- **"Code execution with MCP"** (2025-11-04) [30]:
  - MCP servers presented as a code API on a filesystem. One workflow dropped from 150k to 2k tokens.
  - PII can be tokenized before the model sees it.
  - Working code is saved as `SKILL.md` skills.
- **"Writing effective tools for agents"** (2025-09-11) [31]:
  - Build a few consolidated tools rather than wrapping every endpoint.
  - Namespace tools.
  - Return names, not UUIDs, and offer a `response_format` option (concise/detailed).
  - Paginate and truncate with guidance. Claude Code caps tool responses at **25,000 tokens**.
  - Write actionable errors and use unambiguous parameter names.
- **Agent Skills** became an open standard at agentskills.io in Dec 2025 [44] **[date from secondary sources]**.

### OpenAI

- **Remote MCP in Responses** [6]:
  - Fields: `server_url` or **`tunnel_id`**, `authorization` (an OAuth access token, never stored, so you resend it
    on every request), `allowed_tools`, `require_approval` (approval is requested by default), `defer_loading`.
  - `connector_id` is legacy: deprecated for models released after 2026-09-01.
  - Streamable HTTP or HTTP/SSE.
  - Keep the `mcp_list_tools` item in context so tools aren't fetched again every turn.
  - Remote MCP is Responses-only.
- **Agents API** [4]: MCP tools are **deferred automatically**, and MCP connections are configured separately from
  Responses.
- **Tool search** [4]:
  - Supported on gpt-5.4 and later.
  - Hosted mode, or client mode (`execution: "client"`) where your code runs the lookup.
  - Namespaces and MCP servers are recommended over individual deferred functions.
  - **Fewer than 10 functions per namespace.**
  - Changing the loaded set invalidates the cache from that point on.
- **Function calling guide** [22]:
  - "Aim for fewer than 20 functions."
  - Strict mode needs `additionalProperties:false` and every field required; make a field optional by allowing
    `null`. A non-conforming schema is rejected.
  - On Responses, omitting `strict` means "normalize, else non-strict".
  - Custom tools with Lark or regex grammars.
- **ChatGPT custom MCP** ("plugins") [7]:
  - Server URL **or Secure MCP Tunnel**. SSE or streaming HTTP.
  - Auth: OAuth (static credentials if given, otherwise CIMD or DCR), none, or mixed. In mixed mode, `initialize` and
    `tools/list` are unauthenticated and each tool declares its own security scheme.
  - **Tools without `readOnlyHint` are treated as writes** and need confirmation by default.
  - Keep the first 512 characters of server instructions self-contained.
  - Plan availability: the help article (403 to the fetcher) says Business/Enterprise/Edu on the web. A third-party
    page says Plus/Pro get read/fetch-only **[unverified]**.
- **Secure MCP Tunnel** [5]:
  - `tunnel-client` long-polls `api.openai.com:443` (optionally mTLS) and forwards JSON-RPC to the private server;
    SSE can be forwarded.
  - Limits: 1,000 tunnels per workspace or org, **600 requests/min per tunnel per app**, **10 MiB per message**,
    **120 s deadline**, 1,000 queued requests.
  - **Public plugins still need a public HTTPS endpoint.** The docs suggest putting a public proxy in front.
  - Launch 2026-05-27 per a third-party guide **[unverified]**.
- **Apps SDK / app directory**: built on MCP. Submissions opened late 2025 and rolled out in early 2026. ChatGPT
  supports MCP Apps UIs **[secondary sources]**.
- **AGENTS.md** (Aug 2025), donated to AAIF [43]. **ACP** (Agentic Commerce Protocol, with Stripe, Sep 2025)
  **[secondary sources]** [48].

### Google (Gemini, A2A)

- **Gemini Interactions API / Managed Agents** [13][14][15]:
  - `mcp_server` tool with `name`, `url`, `headers`, `allowed_tools`.
  - **Streamable HTTP only, no SSE.** Server names in snake_case, no hyphens.
  - Newer docs add a **`credential`** field: a stored secret that an egress proxy injects as a header (default
    `Authorization: Bearer`; `header_name` and `prefix` can be changed). OAuth2 credentials refresh automatically.
    Docs updated 2026-10-07.
  - The July 2026 blog post adds `background: true` async interactions, and says remote MCP removes the need for
    "custom proxy middleware".
  - Docs conflict on whether Gemini 3 models support remote MCP **[unverified]**.
  - Best practice: "keep the active set of tools to 10–20 maximum".
- **Gemini CLI** [17]:
  - Transports: stdio, SSE (`url`) and Streamable HTTP (`httpUrl`).
  - Auth: OAuth with discovery and DCR (validates `iss`), custom headers, Google ADC, service-account impersonation
    for IAP.
  - Tool filtering with `includeTools`/`excludeTools`.
  - **Tool names are sanitized to `[A-Za-z0-9_.:-]`, shortened past 63 characters, and registered as
    `mcp_{server}_{tool}`. Server names must not contain `_`.**
- **Gemini Enterprise** custom MCP data store: preview, admin-only, HTTPS, StreamableHTTP only. Needs an OAuth client
  registered explicitly; redirect `https://vertexaisearch.cloud.google.com/oauth-redirect` [18].
- **Consumer Gemini app**: third-party pages say "Settings > Connected apps > Add a custom app" since June 2026,
  possibly Ultra-only **[unverified]** [19].
- **A2A v1.0** [42]:
  - Method names in PascalCase (`SendMessage`, `GetTask` …) and a new `ListTasks`.
  - `a2a.proto` is normative. Bindings: JSON-RPC, gRPC, HTTP+JSON.
  - **Signed Agent Cards** (JWS + JCS).
  - **`supportedInterfaces[]`** replaces `preferredTransport`.
  - **`tenant` field**, so one endpoint can serve several agents.
  - OAuth: implicit and password flows removed, **device code flow added**, `pkceRequired`, mTLS scheme.
  - Joined the Agentic AI Foundation 2026-08-27. The JS SDK 1.0 GA was 2026-07-22 [41].
- **AP2** (agent payments, signed mandates, x402 extension) **[secondary sources]** [48].

### xAI

- **Remote MCP** in the native SDK, the OpenAI-compatible Responses API and the Voice/Speech API [8]:
  - Fields: `server_url`, `server_label`, `allowed_tools`, `authorization`, `headers`.
  - Streamable HTTP or SSE only.
  - `require_approval` and `connector_id` are **not supported**.
  - Without `allowed_tools`, every tool goes into context.
- **Grok consumer custom connectors** (grok.com/connectors, paid plans) are described only by third parties
  **[unverified]** [21].

### Mistral

- **Le Chat custom MCP connectors** [16]:
  - Auth: none, HTTP Bearer or Basic, or OAuth 2.1 with DCR. The auth type is detected from the URL.
  - Streamable HTTP recommended.
  - **No dynamic tool discovery, no resources, no prompt templates.**
  - Admin-only to add; on Free/Pro/Student the account owner is admin.
  - One vendor reports that an empty client secret makes Le Chat try DCR **[unverified]**.
- **Studio API Connectors**: registered MCP servers that can be used as tools in conversations and Agents. Public
  preview. The overview page doesn't document auth [33].

### Alibaba (Qwen)

- **Model Studio Responses API MCP** [20]:
  - Fields: `type:"mcp"`, **`server_protocol:"sse"`** (SSE is the only protocol listed), `server_label`,
    `server_url`, `server_description`, **`headers`** for bearer auth.
  - **Max 10 MCP servers per request.** Responses API only.
  - Models: Qwen3.5–3.8 Max/Plus/Flash and the open-source series.
  - Page updated 2026-10-11.
- **Qwen-Agent** [24]:
  - MCP config shown only as stdio `command`/`args`.
  - The default function-calling template is `nous` (Hermes style), recommended for Qwen3, and supports parallel
    calls.
  - For QwQ/Qwen3, don't enable vLLM's `--tool-call-parser hermes` (Qwen-Agent parses tool calls itself). For
    Qwen3-Coder, use vLLM's built-in parser with `use_raw_api`.
  - Qwen3.5 open-sourced 2026-02-16.
- **Qwen Code** (CLI) supports remote HTTP MCP with OAuth **[secondary page]** [25].

### DeepSeek

- **V3.2-Exp** (2025-09-29): DeepSeek Sparse Attention for long context, and API prices cut by 50%+ [34].
- **V3.2** (2025-12-01): the first DeepSeek model with "thinking integrated into tool use", trained on 1,800+
  synthetic agent environments [37].
- **Current API models** [23][37b]: `deepseek-flash` (V4.1-Flash-1017) and `deepseek-v4-pro`. There is an
  **Anthropic-compatible base URL** (`/anthropic`), and the docs point to Claude Code, Codex and OpenCode
  integration.
- **Thinking mode with tools**: you **must pass `reasoning_content` back on every later request or get a 400**.
  Temperature and penalties are ignored. `reasoning_effort` is low/high/max.
- I found no remote MCP support in DeepSeek's own API.
- V4 preview 2026-04 and V4-Pro GA 2026-08 per third parties **[unverified dates]**.

### Moonshot (Kimi)

- **K2 tool-calling accuracy on vLLM** (vLLM blog 2025-10-28) [28]. Self-hosted K2 initially parsed fewer than 20%
  of tool calls. Causes:
  - a dropped `add_generation_prompt`
  - empty `content` printed as a Python list
  - tool-call IDs not in the `functions.name:idx` form
  Moonshot's API rewrites IDs and uses constrained decoding ("Enforcer"). K2-Vendor-Verifier measures how closely
  third-party hosts match.
- **K2.5** (2026-01-27, Agent Swarm), **K2.6** (2026-04-20), **K3** (2026-07-16, open weights 2026-07-27, 1M
  context), **K3.1 Pro** (2026-10-23, current flagship) [27][26] (K2.x dates from a help page via search).
- K3 needs the full assistant message, including `reasoning_content` and `tool_calls`, passed back unchanged [27].
- **Dynamically loaded tools** (K3/K3.1 only) [26]:
  - Inject a `role:"system"` message that carries `tools` and no `content`.
  - Definitions aren't kept server-side; append-only to protect the cache.
  - There's no built-in search; you build one with a `search_tools` function.
- Function names: letters, digits, `-`, `_` [26b].
- I found no MCP support in the Kimi API.
- Third-party docs say `tool_choice:"required"` is downgraded to `auto` on K2.x **[unverified]**.

### Zhipu (Z.ai GLM)

- **Streaming tool-call arguments** (`stream=True` + `tool_stream=True`) on GLM-4.6/4.7, and possibly GLM-5. The
  page is inconsistent about GLM-5 [49].
- GLM-5 released around 2026-02-11/12 **[third-party date]**.
- Z.ai **ships MCP servers** (vision and web search) for GLM Coding Plan users as a local npm package [50].
- I found no remote MCP support in the GLM API.

### MiniMax

- **Interleaved thinking** (2025-11-03) [32]: keep thinking blocks (Anthropic-compatible API) or `reasoning_details`
  (OpenAI-compatible API) in history.
- Dropping them cost Tau² 87 → 64, BrowseComp 44.0 → 31.4, GAIA 75.7 → 67.9.
- M2 technical report on arXiv [51].
- M2.5 (2026-02), M2.7 and M3 (2026-06) are reported by third parties **[unverified]**.
- I found no remote MCP support in the MiniMax API.

## Cross-cutting: tricks that matter for a connector or relay

1. **Header-level routing is now in the spec.** `Mcp-Method` and `Mcp-Name` on every Streamable HTTP POST
   (2026-07-28) [1][36] let a Worker apply the tool allow-list, per-tool rate limits and logging without parsing
   JSON. The server has to check that the headers match the body (`HeaderMismatch` −32020). Older clients
   (2025-11-25) won't send these headers, so the agent-side filter is still needed.
2. **The stateless transport means no session affinity.** There's no `Mcp-Session-Id`, no SSE resume and no ping.
   A dropped stream loses the request and the client re-issues it under a new ID [1]. "Never retry" in the relay
   still holds, because the client does the retry.
3. **`subscriptions/listen` is a long-lived POST stream** [1]. A relay has to hold it open, and its notifications
   count against request budgets.
4. **Cache hints.** `ttlMs`/`cacheScope` on `tools/list` and deterministic list order [1]. Anthropic pins the tool
   list per conversation [3], and OpenAI recommends keeping `mcp_list_tools` in context [6]. Changing the allow-list
   mid-conversation may not reach the model until the next conversation.
5. **Tool budget.** Keep 10–20 tools exposed per bot [15][22]. Past about 30–50, use tool search [2]. Namespacing
   and keyword-rich descriptions make search work [2][4]. Fewer than 10 functions per namespace [4].
6. **Code mode.** Expose `search` + `execute` instead of N tools. That gives large savings for big APIs [30][35],
   but you need a sandbox. `@cloudflare/codemode`'s `codeMcpServer` wraps an existing server [35].
7. **Response size.** 25k-token cap in Claude Code [31], 10 MiB per message in OpenAI's tunnel [5]. Paginate,
   truncate with a hint, and offer a `response_format` option.
8. **Errors the model can act on.** Bad input should come back as a tool execution error (`isError`), not a
   protocol error [9]. Write error text that tells the model what to do next [31].
9. **Annotations drive the confirmation UI.** ChatGPT treats any tool without `readOnlyHint` as a write [7].
   Anthropic recommends denylisting destructive tools [3].
10. **Naming rules across clients.**
    - Gemini CLI: tool names `[A-Za-z0-9_.:-]`, at most 63 characters, server names without `_` [17].
    - Gemini API: server names without `-` [15].
    - Kimi: function names letters, digits, `-`, `_` [26b].
    The safe intersection for service names is lowercase alphanumeric only.
11. **Schemas.** MCP now allows any JSON Schema 2020-12 keyword, including `$ref` [1], but OpenAI strict mode needs
    a narrow subset [22]. Expect clients to reject or flatten complex schemas **[per-client behaviour
    unverified]**.
12. **Reasoning passback.** DeepSeek, Kimi K3 and MiniMax all lose quality or return errors if a proxy drops or
    edits reasoning or tool-call IDs [23][27][28][32]. This only matters if TopRelay ever sits between a model and
    its client (model routing, a hosted agent loop).
13. **Long tasks.** The tasks extension (polling `tasks/get`) [1], Gemini `background:true` [14], and the OpenAI
    tunnel's 120 s deadline [5]. Slow local tools need a task handle or progress notifications, not one blocking
    call.
14. **MRTR and URL elicitation** [1][9]. A server can answer `input_required` and point the user at a URL (consent,
    OAuth to a third party). The client retries once the user is done. That is a standard hook for human approval.

### What benchmarks say fails

- **MCPMark** (127 tasks; arXiv 2509.24002, ICLR 2026) [38]: best 52.56% pass@1, 33.86% pass^4. Reliability across
  repeated runs is much worse than single-run success. Successful runs use fewer, more targeted calls; failed runs
  loop through repeated, exploratory calls.
- **MCP-Universe** (Salesforce, 231 tasks, 11 servers; arXiv 2508.14704) [39]: GPT-5 43.72%. The named failure modes
  are **long context** (token counts grow quickly with each step) and **unknown tools**.
- **Toolathlon** (32 apps, 604 tools, 108 tasks, about 20 turns; arXiv 2510.25726) [40]: Claude 4.5 Sonnet 38.6% and
  DeepSeek-V3.2-Exp 20.1% at release. Later leaderboard numbers come from third parties **[unverified]**.
- **MCP-Atlas** (1,000 tasks, 36 servers; arXiv 2602.00933) [52]. **BFCL v4** added agentic, memory and
  format-sensitivity tests (2026-04) **[secondary]**. **τ²-bench** is the conversational, policy-bound benchmark
  MiniMax reports on [32].
- None of these papers singles out auth or pagination as a failure category, as far as their abstracts and
  summaries go. In practice, auth failures show up as setup friction, not model errors. Pagination counts as part of
  the "long context" problem.

## Vendor × remote MCP support × auth accepted

| Vendor / surface | Remote MCP? | Transports | Auth accepted | Notes |
|---|---|---|---|---|
| Claude API (Messages) | Yes, beta [3] | Streamable HTTP, SSE | `authorization_token` (bearer); caller runs OAuth | Tools only; tool list pinned with the 2026-09-15 header; MCP tunnels for private servers (preview) [12] |
| claude.ai connectors | Yes, all plans (Free: 1) [10] | Streamable HTTP, SSE (`/sse`) | OAuth via CIMD (recommended), DCR or own client; none; **request headers beta, limited orgs** | Auth can't be edited after add; Enterprise tunnels [11] |
| OpenAI Responses | Yes [6] | Streamable HTTP, HTTP/SSE | `authorization` (OAuth token, resent each call); `tunnel_id` | Approval by default; `connector_id` legacy |
| ChatGPT custom MCP | Yes, plan-gated [7] | SSE, streaming HTTP | OAuth (static creds / CIMD / DCR), none, mixed per tool | No `readOnlyHint` means write + confirm; tunnel option |
| xAI API | Yes [8] | Streamable HTTP, SSE | `authorization`, `headers` | No `require_approval`; use `allowed_tools` |
| Grok app | Reported [21] | Remote only | OAuth reported | **[unverified]** |
| Gemini Interactions / Managed Agents | Yes [13][15] | **Streamable HTTP only** | `headers`; stored `credential` injected by egress proxy (bearer/OAuth2 refresh) [14] | Server name without `-`; Gemini 3 support unclear |
| Gemini CLI | Yes [17] | stdio, SSE, Streamable HTTP | OAuth (DCR), headers, Google ADC, SA impersonation | Name sanitization, 63-character limit |
| Gemini Enterprise | Yes, preview, admin [18] | StreamableHTTP only | Pre-registered OAuth client | Fixed redirect URI |
| Gemini app | Reported [19] | Remote only | OAuth (DCR) reported | **[unverified]**, maybe Ultra-only |
| Mistral Le Chat | Yes, admin [16] | Streamable HTTP (recommended) | None, Bearer/Basic, OAuth 2.1 + DCR | No dynamic tool discovery, no resources/prompts |
| Mistral API Connectors | Yes, preview [33] | Not documented | Not documented | Registered servers |
| Alibaba Model Studio (Qwen) | Yes, Responses only [20] | **SSE only** | `headers` (bearer) | Max 10 servers per request |
| DeepSeek API | Not found | — | — | Anthropic-compatible endpoint exists [23] |
| Kimi API | Not found | — | — | Dynamic tool loading (K3+) [26] |
| Z.ai GLM API | Not found | — | — | Ships its own MCP servers [50] |
| MiniMax API | Not found | — | — | **[not checked on a primary page]** |

## Product implications for TopRelay

Positioning:

- **Against the vendor tunnels.**
  - OpenAI's tunnel is OpenAI-only, behind an API key, 120 s, 600 rpm [5].
  - Anthropic's is Enterprise-only, Docker/Helm, RFC 1918 targets, Claude-only, and runs on Cloudflare [11][12].
  - Neither gives a single person one URL for every bot, and neither adds auth in front of an auth-less server.
  - TopRelay's line: "one personal endpoint, every bot, auth and allow-list built in, on your own Cloudflare". Say
    that Anthropic's own tunnel also runs on Cloudflare, so "Cloudflare in the path" is normal.
- **The remote-client credential is the product.** Both vendor tunnels leave server auth to the customer. TopRelay
  doing OAuth and bearer for an auth-less stdio server is the main difference. Keep it front and centre.

Concrete feature ideas, roughly in priority order:

1. **Allow-list enforced in the Worker from `Mcp-Name`.** With a 2026-07-28 client, the Worker rejects a disallowed
   `tools/call` before the Durable Object runs: cheaper, and still protocol-blind. The agent keeps the full
   body-level check, `HeaderMismatch` validation and the `tools/list` filter. That filter must **preserve list
   order** and pass `ttlMs`/`cacheScope` through.
2. **CIMD in the relay's OAuth server.** Turn on `clientIdMetadataDocumentEnabled` [36] so claude.ai's recommended
   "published identity" option and ChatGPT's CIMD path work. Keep DCR as the fallback until 2027. Implement the
   `iss` response parameter (RFC 9207) and `resource` binding (RFC 8707), and key grants by issuer.
3. **Bearer key as a first-class path.** Every developer API and Le Chat take one. claude.ai is adding request
   headers [10]. In `toprelay key`, print the exact snippet per client: `authorization_token`, `authorization`,
   `headers`, Gemini `credential`. Remind users to include `Bearer ` for claude.ai headers.
4. **Legacy SSE endpoint for the stdio bridge** (`/<service>/sse`), so Alibaba Model Studio (SSE only) [20] and
   older clients work. Streamable HTTP stays the default. Add a test row for Qwen in the compatibility table.
5. **Name lint at `add`.**
   - Service names lowercase alphanumeric only (no `-` for the Gemini API, no `_` for Gemini CLI).
   - Warn on tool names over 63 characters or outside `[A-Za-z0-9_-]`.
   - Offer a prefix option (`notes_search`) so names stay unique and searchable across servers.
6. **Per-bot tool profiles.** `toprelay tools notes --profile chatgpt`, with a separate URL or key per profile. That
   keeps each bot at 10–20 tools. Warn at more than 20 exposed tools and point users to tool search / defer_loading
   in the snippet for Claude and OpenAI.
7. **Annotation editor.** Let the owner mark a tool read-only or destructive locally, but only to make it stricter,
   never to turn a write into a read. This feeds ChatGPT's confirmation UI [7] and the default allow-list.
8. **Approve write calls from a phone (MRTR / URL elicitation).** For tools marked "ask", the agent returns
   `input_required` with a URL to a relay-hosted approval page and holds the call until the owner approves. Works on
   2026-07-28 clients that support elicitation. Fall back to an `isError` result explaining that approval is
   needed. This is a feature neither vendor tunnel has. **[Client support per vendor unverified; spike first.]**
9. **Errors the model can explain.** When the laptop is offline or a tool is disallowed, answer `tools/call` with a
   tool result (`isError: true`, plain text such as "The user's Mac is asleep; ask them to open it") rather than only
   a JSON-RPC error [9][31]. Keep HTTP 503 for non-MCP paths.
10. **Long-running local tools.** Support the tasks extension in the stdio bridge (return a task handle past N
    seconds) and stream progress notifications on the request stream. The DO's 15-minute pending I/O limit and
    clients' own deadlines (120 s for OpenAI's tunnel [5]) make one blocking call fragile.
11. **Response guard in the agent.** An optional cap on result size, with a truncation note telling the model to
    narrow its query [31]. Off by default: it changes the app's answers.
12. **`doctor` checks per client.** Transport (SSE vs Streamable), header presence (`Mcp-Method`), `server/discover`
    answered, auth mode, name lint, tool count, schema features that strict-mode clients reject (`$ref`, `oneOf`)
    **[strict-mode rejection behaviour unverified per vendor]**.
13. **Optional code mode (later).** `toprelay add big --stdio … --code-mode` exposes `search`/`execute` over the
    allowed tools, using `@cloudflare/codemode` [35]. It needs a sandbox (Dynamic Worker Loader availability on the
    free plan **unverified**), and it weakens per-tool allow-listing unless `execute` enforces it too. Only for
    servers with more than 50 tools.
14. **Skills and AGENTS.md output.** For each service, generate a `SKILL.md` [44] and an AGENTS.md snippet [43]
    describing the endpoint, its tools and the auth step. Agents that load skills (Claude, Codex, Gemini CLI
    **[adoption per secondary sources]**) then know how to use the user's app.
15. **Broader "link apps" pivot.**
    - **A2A over the same relay**: the transport is just HTTPS + JSON-RPC/SSE. Its `tenant` field and signed Agent
      Cards map onto `/<service>/`, and device-code OAuth fits CLI pairing [42].
    - **Device-to-device**: one person's laptop MCP server reachable by their phone's agent through the same Worker.
    - Keep payments (ACP/AP2/x402) and AG-UI out of scope. They are about commerce and front ends, not reachability.
16. **Watchlist changes.**
    - Add **Anthropic MCP tunnels** to the competitor list and correct the brief: claude.ai is no longer
      "OAuth or no auth only", since request headers exist in beta [10].
    - Update the bot-compatibility table with Gemini (Streamable only), Alibaba (SSE only), and Mistral Le Chat.

## Sources

1. MCP spec 2026-07-28 changelog. https://modelcontextprotocol.io/specification/2026-07-28/changelog
2. Anthropic, Tool search tool docs. https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool
3. Anthropic, MCP connector docs. https://platform.claude.com/docs/en/agents-and-tools/mcp-connector
4. OpenAI, Tool search guide. https://developers.openai.com/api/docs/guides/tools-tool-search
5. OpenAI, Secure MCP Tunnels guide. https://developers.openai.com/api/docs/guides/secure-mcp-tunnels ; repo https://github.com/openai/tunnel-client
6. OpenAI, Connectors and MCP servers guide. https://developers.openai.com/api/docs/guides/tools-connectors-mcp
7. OpenAI, Add a custom MCP server (ChatGPT). https://developers.openai.com/api/docs/guides/custom-mcp-server.md ; help https://help.openai.com/en/articles/12584461-developer-mode-and-mcp-apps-in-chatgpt (403 to fetcher)
8. xAI, Remote MCP tools. https://docs.x.ai/developers/tools/remote-mcp
9. MCP spec 2025-11-25 changelog. https://modelcontextprotocol.io/specification/2025-11-25/changelog
10. Anthropic, Add a connector that isn't in the directory. https://claude.com/docs/connectors/custom
11. Anthropic, MCP tunnels (claude.ai). https://claude.com/docs/connectors/mcp-tunnels/overview
12. Anthropic, MCP tunnels (Platform). https://platform.claude.com/docs/en/agents-and-tools/mcp-tunnels/overview
13. Google, Gemini Interactions API. https://ai.google.dev/api/interactions-api
14. Google, Credentials in managed agents (updated 2026-10-07). https://ai.google.dev/gemini-api/docs/agent-credentials ; blog (Jul 2026) https://blog.google/innovation-and-ai/technology/developers-tools/expanding-managed-agents-gemini-api/
15. Google, Function calling with the Gemini API. https://ai.google.dev/gemini-api/docs/function-calling
16. Mistral, Le Chat MCP connectors. https://docs.mistral.ai/le-chat/knowledge-integrations/connectors/mcp-connectors ; help https://help.mistral.ai/en/articles/393572-configuring-a-custom-connector ; empty-secret note (vendor) https://docs.bko.finologee.com/docs/mcp-mistral
17. Gemini CLI, MCP servers. https://geminicli.com/docs/tools/mcp-server/
18. Google Cloud, Gemini Enterprise custom MCP server. https://docs.cloud.google.com/gemini/enterprise/docs/connectors/custom-mcp-server/set-up-custom-mcp-server
19. Third-party: Gemini custom apps. https://skillsplayground.com/integrations/gemini/ **[unverified]**
20. Alibaba Cloud Model Studio, MCP (updated 2026-10-11). https://www.alibabacloud.com/help/en/model-studio/mcp
21. Third-party: Grok custom connectors. https://skillsplayground.com/integrations/grok/ **[unverified]**
22. OpenAI, Function calling guide. https://developers.openai.com/api/docs/guides/function-calling
23. DeepSeek, Thinking mode guide. https://api-docs.deepseek.com/guides/thinking_mode ; (37b) quick start / models https://api-docs.deepseek.com/guides/function_calling
24. QwenLM, Qwen-Agent. https://github.com/QwenLM/Qwen-Agent
25. Qwen Code MCP docs. https://qwenlm.github.io/qwen-code-docs/de/users/features/mcp/
26. Kimi, Dynamically loaded tools. https://platform.kimi.ai/docs/guide/use-dynamic-tool-loading.md ; model list https://platform.kimi.ai/docs/models ; quickstart https://platform.kimi.ai/docs/guide/kimi-k2-quickstart ; (26b) tool calls guide https://platform.kimi.ai/docs/guide/use-kimi-api-to-complete-tool-calls.md ; K3.1 Pro launch https://forum.moonshot.ai/t/kimi-k3-1-pro-has-launched-start-exploring-now/629
27. MoonshotAI, Kimi-K3 repo. https://github.com/MoonshotAI/Kimi-K3 ; announcement https://forum.moonshot.ai/t/kimi-k3-is-here-our-most-capable-model/480
28. vLLM blog, Kimi K2 tool-calling accuracy (2025-10-28). https://vllm.ai/blog/Kimi-K2-Accuracy ; verifier https://evalscope.readthedocs.io/en/latest/benchmarks/k2_verifier.html
29. Anthropic Engineering, Advanced tool use (2025-11-24). https://www.anthropic.com/engineering/advanced-tool-use
30. Anthropic Engineering, Code execution with MCP (2025-11-04). https://www.anthropic.com/engineering/code-execution-with-mcp
31. Anthropic Engineering, Writing effective tools for agents (2025-09-11). https://www.anthropic.com/engineering/writing-tools-for-agents
32. MiniMax, Why interleaved thinking matters for M2 (2025-11-03). https://www.minimax.io/news/why-is-interleaved-thinking-important-for-m2
33. Mistral, Connectors (Studio API). https://docs.mistral.ai/studio-api/knowledge-rag/connectors
34. DeepSeek, V3.2-Exp release (2025-09-29). https://api-docs.deepseek.com/news/news250929
35. Cloudflare, Code Mode MCP (2026-02-20). https://blog.cloudflare.com/code-mode-mcp/ ; original https://blog.cloudflare.com/code-mode ; enterprise portals https://blog.cloudflare.com/enterprise-mcp/
36. Cloudflare, The next generation of MCP (2026-08-06). https://blog.cloudflare.com/mcp-v2/
37. DeepSeek, V3.2 release (2025-12-01). https://api-docs.deepseek.com/news/news251201
38. MCPMark (arXiv 2509.24002). https://arxiv.org/abs/2509.24002
39. MCP-Universe (arXiv 2508.14704). https://arxiv.org/abs/2508.14704 ; https://www.salesforce.com/blog/mcp-universe/
40. Toolathlon (arXiv 2510.25726). https://arxiv.org/abs/2510.25726
41. a2a-js v1.0.0 release (2026-07-22). https://github.com/a2aproject/a2a-js/releases/tag/v1.0.0
42. A2A, What's new in v1.0. https://a2a-protocol.org/latest/whats-new-v1/
43. Linux Foundation, Agentic AI Foundation formation (2025-12-09). https://www.linuxfoundation.org/press/linux-foundation-announces-the-formation-of-the-agentic-ai-foundation
44. Agent Skills specification. https://agentskills.io/specification
45. MCP Apps, SEP-1865 and spec. https://modelcontextprotocol.io/seps/1865-mcp-apps-interactive-user-interfaces-for-mcp ; https://github.com/modelcontextprotocol/ext-apps/blob/main/specification/2026-01-26/apps.mdx
46. MCP Registry. https://blog.modelcontextprotocol.io/posts/2025-09-08-mcp-registry-preview/ ; https://github.com/modelcontextprotocol/registry
47. MCP blog, Enterprise-managed auth. https://blog.modelcontextprotocol.io/posts/enterprise-managed-auth/ (date from secondary sources)
48. Third-party comparison of ACP / AP2 / x402. https://crossmint.com/learn/agentic-payments-protocols-compared **[secondary]**
49. Z.ai, Stream tool call. https://docs.bigmodel.cn/cn/guide/capabilities/stream-tool
50. Z.ai, Vision MCP server. https://docs.z.ai/devpack/mcp/vision-mcp-server
51. MiniMax-M2 series technical report (arXiv 2605.26494). https://arxiv.org/pdf/2605.26494
52. MCP-Atlas (arXiv 2602.00933). https://www.arxiv.org/pdf/2602.00933
