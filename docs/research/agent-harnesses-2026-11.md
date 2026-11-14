# Agent harnesses, memory and skills for a TopRelay "brain" (as of 2026-11)

Written 2026-11-14. The question: TopRelay may add a small built-in assistant. The user brings an API key for any
model, or uses Apple's on-device Foundation Models. They send commands from an inbox (the phone first, maybe messaging
apps later). The assistant runs tool calls against the user's local Mac apps through TopRelay, and has memory and
skills (`SKILL.md`) the user can edit. This note asks which open-source harnesses, memory systems and skill formats
TopRelay should build on, copy from, or study.

It builds on `agent-ecosystem-2026-10.md`, which covers the MCP 2026-07-28 spec, AAIF, tool-count limits and the
Agent Skills and AGENTS.md standards. Those points are not repeated here. Every claim has a source number (see
Sources). Repo facts (license, stars, latest release, last push) come from the GitHub API on 2026-11-14 [52].
**[unverified]** marks anything not confirmed on a primary page.

## 1. Recommendation

**The base: our own code, moved into the Go daemon.** Build the brain as `agent/internal/brain`. Port the loop
shape of Operator's `AgentLoop.swift` to Go (about 200 lines). Call every tool with the go-sdk MCP client, which is
already a dependency (`go-sdk v1.8.0`), through `mcpproxy.HandlerTransport`. That way allow-lists, tool pins, the
shield and the signed call log apply to the brain exactly as they apply to a cloud bot. No outside harness fits all
three constraints: Go or Swift, Apache-compatible, and tool calls through our proxy. Every serious candidate is
TypeScript, Python or Rust. Each one also brings its own shell and file tools and trusts its own process:

- Pi says it "does not include a built-in permission system" [1][6].
- OpenClaw runs tools "on the host for the main session unless you configure sandboxing" [10].

**Is OperatorLLM + OKF memory the better base? Yes, as a design. No, as code to copy unchanged.**

- **OperatorLLM** has the three loop rules that matter, taken from Hermes:
  - the assistant's tool-call turn is echoed back into the transcript
  - a bounded budget, with one last call that offers no tools
  - every tool result is a string

  It also has a fail-closed parallel-safe classifier, Anthropic and OpenAI-compatible clients, Keychain storage
  and an on-device tool bridge.

  It is not the daemon base, for four reasons:
  - It is `@MainActor` Swift.
  - It has no MCP client, no session store and no compaction.
  - Its own manifest records that its HTTP clients don't build on Linux (`URLSession.bytes(for:)`).
  - Its default budget is 4 rounds, tuned for a chat bubble. A command sent from the phone needs more.

  Keep it for the Swift app's model settings and the Apple on-device provider.
- **OKF memory** has the best write rules of anything surveyed:
  - provenance: a preference counts only if the user's own message states it
  - facts, not instructions
  - no secrets and no plumbing
  - caps (300 facts, 6,000 characters of recall)
  - revision tokens for writes
  - a read-only wake file with a provenance header

  Copy the rules and the notebook format, not the `.okf/` storage dependency.
- **Licensing.** The Operator repo has no LICENSE file. Moving its code into Apache-2.0 TopRelay is the owner's
  call. Keep the MIT notices for the parts it took from Hermes (`AgentLoop.swift`) and from `yc-software/qm`
  (`MemoryNotebook`, `MemoryCurator` and `MemoryExtraction`, per their file headers).

**Copy small, licence-compatible pieces** (table in §6):

- **Pi:** the compaction trigger and summary format, before/after tool-call hooks, and per-tool `sequential`
  mode [3][7].
- **Hermes:** memory size caps, the frozen memory snapshot, staged memory writes, pairing parameters and
  context-file injection scanning [21][22].
- **OpenClaw:** the memory file layout, memory provenance quarantine, external-content wrapping and chat-template
  token stripping [14][16].
- **Anthropic's memory tool:** the command set [42].
- **Apple `foundation-models-utilities`:** the Swift history-trimming modifiers (Apache-2.0) [50].

**Study, but don't embed:**

- **OpenClaw** is the reference for a messaging-channel inbox, and also the cautionary tale: 722 published
  advisories since 2026-01-31, 14 of them critical [17].
- **Hermes** is the reference for pairing and approvals.
- **Codex** is the reference for skill budgets and the macOS Seatbelt sandbox [24].

**Don't use:**

- **Claude Agent SDK.** It drives a bundled Claude Code CLI, and the TypeScript package is under Anthropic's
  Commercial Terms [26].
- **Letta.** Letta Cloud is its default backend [37].
- **mem0, Graphiti and Cognee** for v1. They are Python services with vector or graph stores, more than one user on
  one Mac needs [38][39][40].

**Skills:** adopt the Agent Skills spec as it stands [43]. Discover skills in `~/.agents/skills/`, the shared path
Pi and Codex already read [4][24], plus a TopRelay folder. Keep TopRelay-only fields under `metadata`. v1 does not
run a skill's `scripts/`.

**Apple model: router, not worker.** The on-device model has 4,096 tokens per session, and tool definitions count
against that [45][46]. Use it to classify commands, pick a skill, fill the arguments of single-tool commands offline,
and summarize. Send multi-step work to the user's key. On macOS 27, the Private Cloud Compute model (32,000 tokens,
with reasoning) is worth adding as a provider. It needs a managed entitlement and network access, so it doesn't
count as offline [48][49].

## 2. Harness comparison

Stars, release tags and dates come from the GitHub API on 2026-11-14 [52]. Rows are grouped by relevance.

| Harness | Lang | License | Latest release | Stars | MCP client | Providers | Sessions / compaction | Sandbox / approval | Embed in Go daemon / Swift app |
|---|---|---|---|---|---|---|---|---|---|
| **Pi** (`earendil-works/pi`; `badlogic/pi-mono` now redirects here) [1] | TS (Node ≥ 22.19) | MIT [9] | v1.5.2, 2026-11-10 | 122k | Yes: stdio + Streamable HTTP, `pi mcp add`, `~/.pi/agent/mcp.json` [5] | `pi-ai`: many vendors plus "any OpenAI-compatible API: Ollama, vLLM, LM Studio" [1] | JSONL session **tree** (branches); auto-compaction when `contextTokens > contextWindow − reserveTokens` (16,384), keeps 20k recent tokens, structured summary [2][3] | None built in; "does not ask for approval before every tool call"; project trust gates resource loading; containerize for isolation [6] | Only as a Node child process over RPC (JSONL on stdin/stdout) [2]. Copy ideas, not code |
| **OpenClaw** [10] | TS (Node 24/26) | MIT | v2026.11.5-beta.2, 2026-11-12 | 394k | Yes, `mcp.servers`, through tool policy [10] | Hosted and local; runtimes `openclaw`, `codex`, `claude-cli`, `copilot`, `muse` [13] | SQLite state, per-agent sessions [13] | Tools on host unless sandboxing is configured; exec approvals and allowlists; DM pairing [10][15] | No: a whole gateway product |
| **Hermes Agent** (NousResearch) [20] | Python | MIT | v0.23.1+canary, 2026-11-14 | 263k | Yes [20] | Nous Portal, OpenRouter, OpenAI, custom endpoint, others [20] | SQLite FTS5 session search, `/compress` [20][21] | `approvals.mode` smart (default) / manual / off; hard blocklist; container backends skip pattern checks [22] | No (Python process). Source of OperatorLLM's loop |
| **Goose** (`aaif-goose/goose`, in AAIF) [23] | Rust | Apache-2.0 | v1.59.1, 2026-11-14 | 57k | Yes, MCP-native "extensions" [23] | 15+, including Ollama; subscriptions via ACP [23] | not checked | not checked | Crates (`goose-sdk`, `goose-mcp`) and an API [23]; Rust FFI or subprocess only |
| **Codex CLI** [24] | Rust | Apache-2.0 | rust-v0.169.0, 2026-11-13 | 135k | Yes (`rmcp-client` crate) | OpenAI; local through `ollama` and `lmstudio` crates [24] | Rollouts plus a two-phase background **memories** pipeline [24] | `read-only` / `workspace-write` / `danger-full-access`; Seatbelt on macOS; approvals `on-request` / `never` (`untrusted` retired) [24] | `app-server` JSON-RPC subprocess. Code can be copied (Apache) |
| **OpenAI Agents SDK** [25] | Python / JS | MIT | py v0.24.0-alpha.7 (2026-11-08); js 0.22.0 (2026-11-12) | 31k / 4k | Yes [25] | "provider-agnostic … 100+ other LLMs" [25] | Sessions (Redis extra) [25] | Guardrails, human in the loop, sandbox agents [25] | No |
| **Claude Agent SDK** [26] | Python / TS | Python repo MIT; TS: "Use is subject to Anthropic's Commercial Terms" | py v0.2.182, ts v0.3.325 (2026-11-13) | 9k / 2k | Yes, including in-process SDK MCP servers [26] | Claude (other providers **[unverified]**) | Claude Code's | Claude Code's permissions and hooks | No: wraps a bundled proprietary CLI |
| **opencode** (`anomalyco/opencode`; `sst/opencode` redirects) | TS | MIT | v2.3.0, 2026-11-13 | 227k | **[not checked]** | Many | Has `server` and `sdk` packages [27] | `plan` agent asks before bash [27] | Server mode. Past critical advisory: an unauthenticated HTTP server allowed command execution [27] |
| **Kimi Code** (`MoonshotAI/kimi-code`) [28] | TS | MIT | 2.6.2, 2026-11-13 | 8k | Yes, `/mcp-config` [28] | Kimi plus compatible providers [28] | — | Approvals guide [28] | ACP subprocess. The Python predecessor `kimi-cli` (Apache-2.0) was archived 2026-09 [52] |
| **OpenHands** [29] | Python SDK + TS UI | MIT | v1.40.0, 2026-11-12 | 93k | **[not checked]** | Many | Agent Server REST API [29] | Docker/VM backends [29] | No |
| **smolagents** | Python | Apache-2.0 | v1.26.0, **2026-05-29** | 30k | **[not checked]** | Many | — | — | No. No release in 5½ months |
| **Mastra** [31] | TS | Apache-2.0 except `ee/` [31] | @mastra/core 1.83.0, 2026-11-13 | 30k | Authors MCP servers [31] | Router to 40+ providers [31] | Conversation history, "Observational Memory" [31] | Suspend/resume for approval [31] | No (Node) |
| **Vercel AI SDK** [32] | TS | Apache-2.0 | ai@7.0.173, 2026-11-13 | 28k | **[not checked]** | Unified providers; defaults to Vercel AI Gateway [32] | — | — | No (Node). `ToolLoopAgent` [32] |
| **Go:** `google/adk-go` [33] | Go | Apache-2.0 | v2.7.1, 2026-11-04 | 9k | **[not checked]** | "optimized for Gemini … model-agnostic" [33] | Sessions **[unverified]** | — | Yes, but a big framework built around Gemini |
| **Go:** `cloudwego/eino` [34] | Go | Apache-2.0 | v0.9.23, 2026-11-09 | 14k | Through `eino-ext` **[unverified]** | OpenAI, Ollama and more through `eino-ext` [34] | Interrupt/resume, context management [34] | — | Yes. ReAct `ChatModelAgent`; graph framework |
| **Go:** `tmc/langchaingo` | Go | MIT | v0.1.15, 2026-10-05 | 10k | — | Many | — | — | Yes, but LangChain-shaped |
| **Swift:** Apple Foundation Models + `apple/foundation-models-utilities` [47][50] | Swift | System framework; utilities Apache-2.0 | utilities 1.0.0-beta6 (README) | 0.6k | No | macOS 27: a `LanguageModel` protocol lets any model back a `LanguageModelSession`; `ChatCompletionsLanguageModel` talks to any chat-completions server [47][50] | History modifiers: drop finished tool calls, rolling window, summarize [50] | — | Swift app only; macOS 26+ (27 for `LanguageModel`/PCC). TopRelay targets macOS 14 |
| **Swift:** `huggingface/AnyLanguageModel` [36] | Swift | Apache-2.0 | 1.3.0, 2026-11-10 | 1k | No | "drop-in replacement" for the Foundation Models API across providers [36] | — | `ToolExecutionDelegate` can stop or replace a call [36] | Swift app only |
| **In-house:** OperatorLLM | Swift | none (owner's) | — | — | No | on-device, Anthropic, OpenAI, Gemini, OpenRouter, custom OpenAI-compatible, "This Mac" | None | Fail-closed `parallelSafe` | Swift `@MainActor`; not Linux |

**Takeaways.**

- **Pi is the cleanest design to learn from.**
  - Four default coding tools (`read`, `bash`, `edit`, `write`), with read-only variants (`read`, `grep`, `find`,
    `ls`) [8].
  - Skills loaded on demand [4].
  - A session tree with summary entries that never delete history [2][3].
  - Plain `beforeToolCall`/`afterToolCall` hooks in `agent-loop.ts` [7].

  Its whole security stance is "isolate the process" [6]. That is the wrong shape for TopRelay, which already
  *is* the policy layer.
- **OpenClaw was built on Pi but no longer runs on it.** Its changelogs bump `@mariozechner/pi-agent-core`,
  `pi-ai` and `pi-coding-agent` for the "embedded Pi runner" up to 0.58.0 (2026.3.13) [12]. Today it says only that
  "portions of OpenClaw were adapted from Pi / pi-mono" and that it depends on `pi-tui` [11]. Its built-in runtime
  is called `openclaw`, and others plug in beside it [13].
- **No Go-native or Swift-native harness is close to what we need.** The Go frameworks (ADK-Go, Eino) are
  multi-agent and graph frameworks. Using one would mean wrapping our MCP proxy in their abstractions to save about
  200 lines of loop. On the Swift side, Apple's macOS 27 `LanguageModel` protocol and `foundation-models-utilities`
  are the most interesting new pieces. They are tied to macOS 27, though, and the brain has to run when the app
  isn't in front.

## 3. Memory

| System | Storage | Who writes | Offline | Inspectable / editable | Fit for one user on one Mac |
|---|---|---|---|---|---|
| **Letta** (MemGPT) [37] | Memory blocks; "MemFS: all context … tracked via git" (Letta Code) | The agent rewrites its own memory, plus "dreaming" | Local backend exists; **Letta Cloud is the default** | Git-backed | Interesting idea (git history of memory). Letta V1 server retired to an `archive` branch; active code is `letta-code` (TS, Apache-2.0, v0.34.61) |
| **mem0** [38] | Library, or self-hosted server with Docker Compose; vector store | LLM extraction | With local models | Through an API | Heavy for one user; v2.2.1, 2025-09-25 |
| **Zep / Graphiti** [39] | Temporal knowledge graph (facts with validity windows) | LLM extraction | With a local graph DB and LLM | Graph queries | Over-built for v1; the temporal-validity idea is worth keeping |
| **Cognee** [40] | Knowledge graph; "runs locally for free — no API key required" | Pipeline | Yes | Graph | Python service; overkill |
| **LangMem** [41] | LangGraph `BaseStore` | Background manager | Depends on store | Through store | Tied to LangGraph |
| **Pi** | **No memory.** Context files (`AGENTS.md`, `CLAUDE.md`, `SYSTEM.md`) and sessions only [2] | — | — | — | Minimal by design |
| **OpenClaw** [14] | `USER.md` (directives), `MEMORY.md` (curated facts), `memory/YYYY-MM-DD.md` (daily notes), `DREAMS.md`; SQLite FTS5 + optional vectors (local GGUF through llama.cpp) | The agent, plus a background "dreaming" sweep | Yes (keyword search without embeddings) | Plain Markdown | Good layout. Has **provenance quarantine**: an untrusted `MEMORY.md` is left out of context and listed by `doctor` [14] |
| **Hermes** [21] | `MEMORY.md` (2,200 characters) + `USER.md` (1,375 characters); SQLite FTS5 over all sessions | The agent through a `memory` tool (`add`/`patch`/`remove`) plus a review after each turn | Yes | Markdown | Hard caps (writes past the cap return an error), a **frozen snapshot** per session (keeps the prompt cache), optional `write_approval` staging |
| **Claude memory tool** [42] | Client-side `/memories` directory; you implement `view`, `create`, `str_replace`, `insert`, `delete`, `rename` | The model, through `memory_20250818` | Storage is yours (the model still needs the API) | Files | Command set Claude models already use. Docs require path-traversal checks, size caps, expiry |
| **Codex memories** [24] | Two-phase background pipeline: extract per thread, then consolidate | Background job | — | Files | Same split as OKF's curator |
| **Operator OKF** | `knowledge/memory/<scope>.md`, dated bullets `- (YYYY-MM-DD) fact`, `<!-- consolidated: -->` marker, SHA-256 revision token | **Only the curator** ("agents still never write memory"); nightly review | Yes | Markdown | Strongest rules. Read side (`MemoryWake`) is a read-only staged file with a provenance header |

**Recommended design (`~/Library/Application Support/TopRelay/brain/`):**

1. **Files, in Markdown, always visible in the app.**
   - `USER.md`: preferences, written as facts (OKF rule), not orders.
   - `MEMORY.md`: durable facts, using OKF's dated-bullet format.
   - `memory/YYYY-MM-DD.md`: daily notes, the OpenClaw layout [14].

   The user edits them in the app or any editor. Their edits are trusted.
2. **Separate writing from remembering.**
   - The model gets a memory tool with Claude's command names [42], confined to the `memory/` tree.
   - Its writes land in `memory/` daily notes only, never in `USER.md` or `MEMORY.md` directly.
   - A curator pass promotes notes into `MEMORY.md` using OKF's extraction prompt. That pass is the qm-derived
     `MemoryExtraction` rules ported to Go: provenance, no secrets, no plumbing.
   - Promotion from any turn that read untrusted content is **held for approval** on the phone. This is Hermes'
     `write_approval` [21], made the default.
3. **Tag provenance on every line.** Record whether a turn came from a paired phone, a channel, or a background
   job, and whether any tool result was read. A background curator never gets more authority than the least-trusted
   input it is folding. This exact gap was OpenClaw GHSA-62qm-6fjj-6g23 [17].
4. **Inject a frozen snapshot.** Load `USER.md` and `MEMORY.md` once per session, under caps (Hermes 2,200/1,375
   characters [21]; OKF 6,000 characters of recall), and wrap them with a provenance header (OKF `MemoryWake`).
5. **Search.** Use SQLite FTS5 over sessions and daily notes, which is offline and needs no embeddings [21][14].
   In Go, a pure-Go SQLite driver keeps the static binary (driver choice **[not checked]**). Add vectors later.
6. **Audit.** Write each memory change to the signed call log, so a planted fact can be traced.

## 4. Skills format

**Adopt the Agent Skills spec unchanged** [43]:

- A directory containing `SKILL.md`.
- YAML frontmatter with required `name` (≤ 64 characters, lowercase and hyphens, matching the folder) and
  `description` (≤ 1,024 characters).
- Optional `license`, `compatibility` (≤ 500 characters), `metadata` (string to string) and `allowed-tools`
  (experimental).
- Three-stage loading: about 100 tokens of metadata, then a body under 5,000 tokens and 500 lines, then
  resources [43].

Pi implements it directly [4], Hermes and Codex build on it [20][24], and OpenClaw and Letta load `SKILL.md`
folders [10][37]. AGENTS.md stays the format for always-on instructions [44]. Pi, Codex and OpenClaw all read it. Use
it for a user's standing instructions to the brain, not for skills.

TopRelay specifics:

- **Locations.**
  - `~/.agents/skills/`, already shared by Pi [4], Codex [24] and Letta (project `.agents/skills`) [37].
  - `~/Library/Application Support/TopRelay/brain/skills/` for skills the app creates.
  - Per-service generated skills, from `agent-ecosystem-2026-10.md` item 14.
- **Budget.** Copy Codex's rule: the skill list uses at most 2% of the context window, or 8,000 characters when the
  window size is unknown [24]. On the Apple model, list at most a handful of skills.
- **TopRelay fields go under `metadata`.** For example `toprelay.services: "notes calendar"` and
  `toprelay.writes: "ask"`. That keeps the files portable, since the spec only allows string values there [43].
- **`allowed-tools` can only narrow.** A skill can reduce the tools offered while it is active. It can never widen
  the per-service allow-list. OpenClaw shipped two flaws on this path: skill dispatch that skipped owner-only policy
  (GHSA-7cp7-87pj-p32v) and skill commands that skipped tool policy (GHSA-mhm4-93fw-4qr2) [17].
- **No `scripts/` execution in v1.** The brain has no shell tool. A skill that needs scripts gets a clear
  "not supported here". This takes away the whole ClawHavoc payload class (§5).
- **Pin skills like tools.** Store the SHA-256 of each skill folder. On change, hold the skill and show a diff,
  using the same UI as `toprelay pins`. Run the shield's injection scan on `SKILL.md`, as Hermes does for
  `AGENTS.md`/`SOUL.md` [22].
- **Apple path.** Apple's `Skills`/`Skill` types in `foundation-models-utilities` are built in code and don't load
  files [50]. The app would parse `SKILL.md` and build `Skill(name:description:instructions:)` itself.

## 5. Inbox and channel security

**How others take commands from phones.**

- **OpenClaw:** one Gateway serves 20+ channels (WhatsApp, Telegram, iMessage, Signal, Slack…) [10]. Unknown DM
  senders get an 8-character code that expires after 1 hour, with at most 3 pending per channel account. The
  message is "not processed" until the owner approves. Approval grants DM access only, not group access [15].
- **Hermes:** default deny ("if no allowlists are configured, all users are denied"). Allowlists per platform.
  `pair`, `ignore` or `decline` for unknown senders. Pairing codes have a 1-hour expiry, one request per user per
  10 minutes, and a 1-hour lockout after 5 failed approvals. Codes are never logged [22].
- **Letta Code:** Slack, Telegram, Discord and custom channels [37].

**Documented incidents (OpenClaw, published GitHub advisories [17]).** 722 advisories from 2026-01-31 to
2026-11-14: 14 critical, 249 high, 390 medium, 69 low. 220 were published in February alone. The classes that matter
for a TopRelay inbox:

| Class | Example | Lesson for TopRelay |
|---|---|---|
| Sender allowlist bypass | Voice-call allowlist used **suffix matching** and accepted **empty caller IDs** (GHSA-4rj2-gpmh-qq5x, critical); WhatsApp **group IDs** satisfied sender allowlists (GHSA-fh38-965w-f6c3); Slack group DMs skipped allowlists (GHSA-58qx-6m8p-wh2j) | Identify senders by a key, never by a phone number or display ID. Exact match. Empty means deny. No groups |
| Pairing → admin escalation | Pairing-scoped device tokens could mint `operator.admin` and reach node RCE (GHSA-4jpw-hj22-2xmc and four more critical advisories in March) | A paired inbox device gets one scope ("send commands, approve writes") that it can't raise itself. New scopes only from the Mac UI |
| Untrusted text promoted to trusted | Background output injected as trusted `System:` events (GHSA-gfmx-pph7-g46x); dreaming dropped the sender's restrictions (GHSA-62qm-6fjj-6g23); heartbeat inherited `senderIsOwner` (GHSA-g5cg-8x5w-7jpm, critical) | Provenance travels with every message, memory line and scheduled job. Background runs get the *least* authority of their inputs |
| Local control UI trusting the URL | Control UI took `gatewayUrl` from the query string and auto-sent its token: 1-click RCE "even on instances configured to listen on loopback only" (GHSA-g8p2-7wf7-98mq) | The app and `agent.sock` must never take endpoints or tokens from links. (opencode had the same class: unauthenticated local HTTP server → RCE, GHSA-vxw4-wv6m-9hhh [27]) |
| Skill supply chain | Koi Security reported 341 malicious ClawHub skills out of 2,857, 335 from one "ClawHavoc" campaign delivering the AMOS macOS stealer through fake prerequisites [19] **[secondary]**; Antiy CERT counts at least 1,184 historically [19]; OpenClaw's own advisory found ClawHub downloads lacked integrity checks (GHSA-3vvq-q2qc-7rmp) | No marketplace. Local skills only, pinned, scanned, no scripts. OpenClaw added VirusTotal scanning and warns that "a clean scan doesn't mean a skill is safe" because instructions in natural language don't match signatures [18] |
| Environment injection | Workspace `.env` overrode runtime controls (GHSA-8rh7-6779-cjqq, critical; GHSA-7wv4-cc7p-jhxc) | The brain doesn't read env files and doesn't spawn shells |

OpenClaw's own guidance [16] is worth adopting as policy:

- Weak or small models must not run tool-enabled agents on untrusted inboxes.
- Use a "reader" agent with no tools to summarize untrusted content.
- Wrap external content in boundary markers.
- Strip chat-template special tokens (`<|im_start|>` and similar) from tool output before it reaches self-hosted
  models.

**TopRelay inbox design:**

1. **v1 inbox = paired devices only.** Commands go through the user's own Worker and Durable Object. The Worker
   stays blind to content.
   - Each command is signed by the phone's device key: Ed25519 over (device, nonce, time, body).
   - The daemon verifies it, enforces replay and clock windows, and logs it to the signed call log.
   - This reuses the pairing and approval path already planned in `directions-2026-10.md` (C, "a leash").
2. **Writes need approval.** Any tool without `readOnlyHint: true` asks on the phone before it runs. That includes
   tools an allowed skill names.
3. **Messaging channels (later) are lower trust by construction.**
   - Identify senders by numeric ID only (exact match, empty means deny), with DMs only and pairing parameters
     like Hermes'.
   - A channel message can run read tools. Writes still need approval on the paired phone.
4. **Tool output is untrusted.** It is already scanned by the shield. The brain also wraps it in boundary markers
   and never lets it reach memory without the provenance tag.

## 6. Lift / reuse table

| Source (license) | What to take | Where it goes in TopRelay |
|---|---|---|
| Operator `OperatorLLM/AgentLoop.swift` (owner; Hermes-derived, MIT notice) | The loop: echo the tool-call turn, results in call order, string results, a final call with no tools, fail-closed `parallelSafe`, usage totals. Raise the round budget for inbox commands | `agent/internal/brain/loop.go` (port) |
| Operator `OperatorLLM/{AnthropicMessagesClient,OpenAICompatibleClient,LLMProvider}.swift` | Provider presets and wire details (OpenRouter, Gemini OpenAI-compatible base URL, custom) | `agent/internal/brain/providers/` (Go port), with Swift settings UI in `TopRelay/` |
| Operator `OnDeviceLLM.swift`, `OnDeviceTools.swift` | Availability messages; JSON-schema → `DynamicGenerationSchema` bridge that drops tools it can't describe | `TopRelay/` app as the "apple" provider, served to the daemon over `agent.sock` |
| Operator `OKF/Memory/{MemoryExtraction,MemoryNotebook,MemoryCurator,MemoryWake}.swift` (owner; qm-derived, MIT notice) | Extraction prompt (provenance, facts-not-instructions, no secrets/plumbing), dated-bullet notebook, caps (300 facts / 6,000 characters), revision-token writes, read-only wake file with provenance header | `agent/internal/brain/memory/` (Go port) |
| Pi `packages/coding-agent/src/core/compaction/compaction.ts`, docs `compaction.md` (MIT) [3] | Trigger `contextTokens > contextWindow − reserveTokens`, keep the last ~20k tokens, summary sections *Goal / Constraints & Preferences / Progress (Done/In Progress/Blocked) / Key Decisions / Next Steps / Critical Context*, carry the previous summary forward | `agent/internal/brain/compact.go` |
| Pi `packages/agent/src/agent-loop.ts` (MIT) [7] | `beforeToolCall`/`afterToolCall` hook points; per-tool `executionMode: "sequential"`; steering vs. follow-up message queues [2] | `brain/loop.go` (the hooks are where approval and provenance go) |
| Pi session format (`how-pi-works.md`, `session-format.md`) (MIT) [2] | Append-only JSONL with entry IDs and parent IDs; compaction as an appended entry, history never deleted | `brain/session.go` (linear first, tree later) |
| Hermes memory and security docs (MIT) [21][22] | Character caps with an error on overflow, frozen snapshot per session, staged `write_approval`, pairing numbers (8 characters, 1 hour, 1 per 10 minutes, lockout after 5), context-file injection scan | `brain/memory/`, inbox pairing in `agent/internal/daemon` |
| OpenClaw docs (MIT) [14][16] | Memory file layout; provenance quarantine of `MEMORY.md`; `<<<EXTERNAL_UNTRUSTED_CONTENT>>>` wrapping; stripping chat-template tokens from external text | `brain/memory/`, `brain/untrusted.go`; token stripping also fits `agent/internal/shield` |
| Anthropic memory tool (docs) [42] | Command names and result strings for `view`/`create`/`str_replace`/`insert`/`delete`/`rename`; path-traversal rules | `brain/memory/tool.go` (offered to every provider, not only Claude) |
| Codex skills docs (Apache-2.0 repo) [24] | Skill-list budget (2% of context / 8,000 characters), the `allow_implicit_invocation` idea | `brain/skills/` |
| Agent Skills `skills-ref` (Apache-2.0) [43] | Validation rules for frontmatter | `brain/skills/validate.go` (re-implement in Go) |
| Apple `foundation-models-utilities` (Apache-2.0) [50] | `droppingCompletedToolCalls`, `rollingWindow`, `summarizeHistory` modifiers; `ChatCompletionsLanguageModel` | `TopRelay/` app, macOS 27 path only |
| `modelcontextprotocol/go-sdk` (MIT → Apache-2.0 transition) [35] | MCP client, already a dependency | `brain` → `mcpproxy.HandlerTransport` |

## 7. Apple Foundation Models: router vs. worker

- **Tool calling.**
  - The `Tool` protocol has a name, a description, `Arguments` (Generable) and `call`.
  - "The framework puts the tool definitions — name, description, parameter information — in the prompt."
  - Tools must be `Sendable` "so the framework can run them concurrently."
  - Output of one tool feeds the next through "back-to-back tool calls" [45].
  - `includesSchemaInInstructions` defaults to `true` and "should only be `false` if the model has been trained to
    have innate knowledge of this tool" [51].
  - June 2026 added `GenerationOptions.ToolCallingMode`: `allowed`, `required`, `disallowed` [51].
- **Context.**
  - "Apple's on-device foundation model has a context window of 4096 tokens per language model session". This
    covers instructions, prompts, tool schemas, tool input and output, and responses.
  - Going over throws `exceededContextWindowSize`.
  - TN3193 was last updated 2026-03-31 [46].
  - `contextSize` and `tokenCount(for:)` arrived with the 26.4 model update [47].
- **macOS 27 (June 2026).**
  - New on-device model; Apple says to re-test prompts [47].
  - A `LanguageModel` protocol so "nearly any language model" can back a session [47][49].
  - `PrivateCloudComputeLanguageModel`: "a 32,000 token context window" with reasoning [49]. It needs a managed
    entitlement and reports quota usage [48].
  - Apple open-sourced utilities, `CoreAILanguageModel` and MLX bindings [47].
- **Verdict.** With 4k tokens, the on-device model can hold perhaps 3–5 small tool schemas plus a short command
  (an estimate, not measured). Use it as a **router**:
  - classify the intent and the read/write risk
  - pick the service and skill
  - fill arguments for one read call ("what's on my calendar today")
  - summarize daily notes into memory candidates
  - work offline when no key is set

  Don't use it as a **worker** for multi-step plans across apps. Run those on the user's key, or on PCC on
  macOS 27. Because Foundation Models exists only in Swift (plus Apple's Python SDK [47]), the Go daemon reaches it
  through the Mac app over `agent.sock`. When the app isn't running, the "apple" provider is unavailable.

## 8. Open questions

- Is the brain only for paired-phone commands, or also an MCP tool cloud bots can call ("ask my Mac")? The second
  turns it into a confused-deputy risk.
- Go SQLite: pure-Go driver vs. cgo. Size and licence for the static binary **[not checked]**.
- How much of Pi's session tree is worth it? Branching helps coding, and probably not an inbox.
- PCC eligibility for a non-App-Store, Developer ID app **[unverified]**.
- Should OKF's "agents never write memory" rule stay absolute (only the curator writes)? Or should the memory tool
  be allowed to write daily notes as proposed here?
- Which MCP clients support MRTR / URL elicitation (carried over from the brief). The brain can do phone approval
  itself without client support.
- Licensing the Operator code into an Apache-2.0 repo (owner's decision) and carrying the Hermes and qm MIT notices.

## Sources

1. Pi monorepo README (`badlogic/pi-mono` → `earendil-works/pi`). https://github.com/earendil-works/pi
2. Pi, How Pi works; Sessions. https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/how-pi-works.md ; https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/sessions.md
3. Pi, Compaction reference. https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/compaction.md
4. Pi, Skills. https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/skills.md
5. Pi, MCP servers. https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/mcp.md
6. Pi, Run Pi safely. https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/security.md
7. Pi, `pi-agent-core` README and `agent-loop.ts`. https://github.com/earendil-works/pi/tree/main/packages/agent
8. Pi, built-in tools `src/core/tools/index.ts`. https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/tools/index.ts
9. Pi LICENSE (MIT, Mario Zechner); release v1.5.2. https://github.com/earendil-works/pi/releases
10. OpenClaw README; tools/MCP and skills docs. https://github.com/openclaw/openclaw ; https://github.com/openclaw/openclaw/blob/main/docs/tools/mcp.md ; https://github.com/openclaw/openclaw/blob/main/docs/tools/skills.md
11. OpenClaw THIRD_PARTY_NOTICES.md. https://github.com/openclaw/openclaw/blob/main/THIRD_PARTY_NOTICES.md
12. OpenClaw changelogs 2026.2.21 and 2026.3.13. https://github.com/openclaw/openclaw/tree/main/CHANGELOG
13. OpenClaw, Agent runtimes; agent runtime workflow. https://github.com/openclaw/openclaw/blob/main/docs/concepts/agent-runtimes.md ; https://github.com/openclaw/openclaw/blob/main/docs/openclaw-agent-runtime.md
14. OpenClaw, Memory overview; builtin memory engine. https://github.com/openclaw/openclaw/blob/main/docs/concepts/memory.md ; https://github.com/openclaw/openclaw/blob/main/docs/concepts/memory-builtin.md
15. OpenClaw, Pairing. https://github.com/openclaw/openclaw/blob/main/docs/channels/pairing.md
16. OpenClaw, Prompt injection. https://github.com/openclaw/openclaw/blob/main/docs/gateway/security/prompt-injection.md
17. OpenClaw security advisories (counted through the GitHub API, 2026-11-14), including GHSA-g8p2-7wf7-98mq, GHSA-4rj2-gpmh-qq5x, GHSA-fh38-965w-f6c3, GHSA-58qx-6m8p-wh2j, GHSA-4jpw-hj22-2xmc, GHSA-gfmx-pph7-g46x, GHSA-62qm-6fjj-6g23, GHSA-g5cg-8x5w-7jpm, GHSA-7cp7-87pj-p32v, GHSA-mhm4-93fw-4qr2, GHSA-3vvq-q2qc-7rmp, GHSA-8rh7-6779-cjqq, GHSA-7wv4-cc7p-jhxc. https://github.com/openclaw/openclaw/security/advisories
18. OpenClaw blog, VirusTotal partnership. https://www.openclaw.ai/blog/virustotal-partnership
19. **[secondary]** The Hacker News, 341 malicious ClawHub skills (Koi Security findings). https://thehackernews.com/2026/02/researchers-find-341-malicious-clawhub.html ; Antiy CERT, ClawHavoc analysis. https://www.antiy.net/p/clawhavoc-analysis-of-large-scale-poisoning-campaign-targeting-the-openclaw-skill-market-for-ai-agents/
20. Hermes Agent README. https://github.com/NousResearch/hermes-agent
21. Hermes, Memory. https://hermes-agent.nousresearch.com/docs/user-guide/features/memory
22. Hermes, Security. https://hermes-agent.nousresearch.com/docs/user-guide/security
23. Goose README and crates. https://github.com/aaif-goose/goose
24. Codex repo (`codex-rs/` crates, `codex-rs/memories/README.md`); Build skills; Sandboxing. https://github.com/openai/codex ; https://learn.chatgpt.com/docs/build-skills ; https://learn.chatgpt.com/docs/sandboxing
25. OpenAI Agents SDK README. https://github.com/openai/openai-agents-python ; https://github.com/openai/openai-agents-js
26. Claude Agent SDK: Python README; TypeScript LICENSE.md. https://github.com/anthropics/claude-agent-sdk-python ; https://github.com/anthropics/claude-agent-sdk-typescript/blob/main/LICENSE.md
27. opencode README and advisories GHSA-vxw4-wv6m-9hhh, GHSA-c83v-7274-4vgp, GHSA-632h-h47v-g4x4. https://github.com/anomalyco/opencode ; https://github.com/anomalyco/opencode/security/advisories
28. Kimi Code README. https://github.com/MoonshotAI/kimi-code
29. OpenHands README. https://github.com/OpenHands/OpenHands
30. smolagents. https://github.com/huggingface/smolagents
31. Mastra README and LICENSE. https://github.com/mastra-ai/mastra
32. Vercel AI SDK README and LICENSE. https://github.com/vercel/ai
33. ADK for Go README. https://github.com/google/adk-go
34. Eino README. https://github.com/cloudwego/eino
35. MCP Go SDK LICENSE. https://github.com/modelcontextprotocol/go-sdk
36. AnyLanguageModel README. https://github.com/huggingface/AnyLanguageModel
37. Letta README (V1 archived); Letta Code README. https://github.com/letta-ai/letta ; https://github.com/letta-ai/letta-code
38. mem0 README. https://github.com/mem0ai/mem0
39. Graphiti README. https://github.com/getzep/graphiti
40. Cognee README. https://github.com/topoteretes/cognee
41. LangMem README. https://github.com/langchain-ai/langmem
42. Anthropic, Memory tool. https://platform.claude.com/docs/en/agents-and-tools/tool-use/memory-tool
43. Agent Skills specification. https://agentskills.io/specification ; https://github.com/agentskills/agentskills
44. AGENTS.md. https://agents.md ; https://github.com/agentsmd/agents.md (and `agent-ecosystem-2026-10.md` [43])
45. Apple, `Tool`. https://developer.apple.com/documentation/foundationmodels/tool
46. Apple, TN3193: Managing the on-device foundation model's context window. https://developer.apple.com/documentation/technotes/tn3193-managing-the-on-device-foundation-model-s-context-window
47. Apple, Foundation Models updates. https://developer.apple.com/documentation/updates/foundationmodels
48. Apple, `PrivateCloudComputeLanguageModel`. https://developer.apple.com/documentation/foundationmodels/privatecloudcomputelanguagemodel
49. Apple, WWDC26 session 241 "What's new in the Foundation Models framework". https://developer.apple.com/videos/play/wwdc2026/241/
50. Apple, Foundation Models framework utilities README and `Sources/FoundationModelsUtilities/Skills/`. https://github.com/apple/foundation-models-utilities
51. Apple, `includesSchemaInInstructions`; `GenerationOptions.ToolCallingMode`; `LanguageModel`. https://developer.apple.com/documentation/foundationmodels/tool/includesschemaininstructions ; https://developer.apple.com/documentation/foundationmodels/generationoptions/toolcallingmode-swift.struct ; https://developer.apple.com/documentation/foundationmodels/languagemodel
52. GitHub REST API (`/repos/{owner}/{repo}`, `/releases`, `/security-advisories`), queried 2026-11-14.
