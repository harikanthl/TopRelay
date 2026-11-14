# Saving tokens and learning from tool calls in TopRelay (as of 2026-11)

Research date: 2026-11-14. The question: which caching and algorithmic tricks (papers, open-source repos, provider
features) can TopRelay use to **(a)** save tokens and latency, and **(b)** learn from past tool calls so later ones get
cheaper and more reliable? Which are worth building, where, and with what data structures?

TopRelay has two places where it can save tokens, and they have different levers:

1. **The proxy** (`agent/internal/mcpproxy`) sits in front of every bot. We don't control the bots' prompts, only what
   they see from us: the tool list, the descriptions and the results.
2. **The planned assistant** (`agent/internal/brain`) runs on the user's own key or on a local model. There we control
   the whole prompt.

This note builds on three earlier ones and doesn't repeat them:

- `agent-ecosystem-2026-10.md` covers the MCP 2026-07-28 changes, the tool-count limits and the response-size caps.
- `algorithms-for-linking-2026-10.md` §5a has the BM25 `search_tools` design.
- `agent-harnesses-2026-11.md` covers the brain loop, memory and skills, and the Apple model as a router.

Every claim has a source number (see Sources). Repo facts (license, latest release, last push) come from the GitHub
API on 2026-11-14 [54]. **[unverified]** marks anything not confirmed on a primary page. Savings figures are the
sources' own numbers on their own benchmarks, not TopRelay measurements.

## 1. Recommendation

The biggest lever for bots is **not sending tokens at all**: fewer tool definitions and smaller results. Caching tool
*results* in the proxy cuts latency and load on the apps, but **it saves the bot no tokens**, because a cached result
still lands in the bot's context. The biggest lever for the assistant is a **cache-stable prompt prefix**, since
provider cache reads cost 0.1× of base input or less [1][7]. Learned behaviour is worth building only as **ranking
and proposals**. It must never grant anything.

| # | Build | Where | Estimated saving (source) | Effort | Risk |
|---|---|---|---|---|---|
| 1 | **Cache-stable tool lists.** Deterministic order, byte-stable descriptions, correct `ttlMs`/`cacheScope`, and changes to the allow-list or profile batched instead of trickled | Proxy (every bot) | Keeps the bots' own prompt caches warm. Changing tool definitions invalidates Anthropic's whole cache (tools, system, messages) [2], and OpenAI's cache breaks on any change to tool names, descriptions, schemas or order [7]. Caching gave 41–80% cost savings and 13–31% better TTFT on an agent benchmark [28] | S | Low |
| 2 | **Aggregated endpoint with `search_tools`** (already planned for v1.2) plus **learned per-bot priors** for ranking and for picking the 3–5 always-loaded tools | Proxy | Anthropic: tool search "typically reduces this by over 85 percent" (about 55k tokens of definitions) [3]; accuracy went from 49% to 74% (Opus 4) and from 79.5% to 88.1% (Opus 4.5) [4]. RAG-MCP: more than 50% fewer prompt tokens and selection accuracy of 43.13% vs 13.62% [29]. MCP-Zero: 98% fewer tokens on APIBank [30] | M | Low to medium: a bot that can't search loses tools |
| 3 | **Cache-aware prompt layout for the brain**: tools → system → skills index → frozen memory → conversation, with breakpoints, a 1 h TTL for sporadic phone commands, per-provider cache keys, native tool search and inline tool additions, and tool-result clearing with `clear_at_least` | Assistant | Cached input costs 0.1× (0.05× on Opus 5.5 and Sonnet 5.5) on Anthropic [1] and 0.1× on OpenAI [7]. A DeepSeek cache hit costs 2% of a miss (`deepseek-flash`: $0.003 vs $0.15 per MTok off-peak) [13] | M | Low |
| 4 | **Result budget**: cap large tool results, return the rest through a `resource_link` or a continuation handle, never truncate silently | Proxy | Claude Code warns above 10,000 tokens and cuts at 25,000 by default [17]. Anthropic's code-execution post filters a 10,000-row sheet down to 5 rows and cites 50,000 tokens for one transcript passing through twice [5]. Saving depends on the apps (test) | M | Medium: bots that ignore links lose data |
| 5 | **Read-only result cache with request coalescing**, invalidated by a per-service generation counter on any write | Proxy | Latency only. TVCache, which keys on the whole call history, reports hit rates up to 70% and median tool time down to 1/6.9 in RL rollouts [41]. Real-world hit rate for a single user is unknown (test) | M | Medium: stale reads. Opt in per tool |
| 6 | **Local-model settings**: Ollama `keep_alive` and `num_ctx`, llama.cpp prompt cache and slot save/restore, Apple `prewarm(promptPrefix:)` | Assistant (local only) | Skips the re-prefill of the stable prefix. No numbers for Apple Silicon found (test with llama.cpp `timings.cache_n`) [18][19][25] | S | Low |
| 7 | **Parallel execution of independent read-only calls** in the brain loop | Assistant | LLMCompiler: up to 3.7× lower latency and 6.7× lower cost than ReAct [51]. AsyncLM: 1.6×–5.4× faster task completion [52] | S–M | Low if limited to read-only calls the user marked as safe |
| 8 | **Loop and duplicate detection**: exact hash of (tool, canonical args) per client in a sliding window, SimHash of results | Proxy | Stops waste. MCPMark found that failed runs loop through repeated, exploratory calls (`agent-ecosystem-2026-10.md`). No token figure | S | Low (log and warn only) |
| 9 | **Trajectory store and SKILL.md proposals** (workflow and plan caching) | Assistant (mines the call log too) | APC: 50.31% lower cost and 27.28% lower latency [46]. AWM: +24.6% and +51.1% relative success, with fewer steps [42]. ASI: 10.7–15.3% fewer steps [47] | L | Medium: poisoning. Proposals need user approval |

**Not worth building (now):**

- **Semantic caching of answers or tool results.**
  - False hits are a correctness bug.
  - Collisions are an attack surface: CacheAttack reached an 86% hijack rate [39].
  - The best "verified" design, vCache, is licensed CC BY-NC-ND [37][54].
- **LLMLingua-style compression of results.** It's Python, it changes tokens inside facts, and its last release was
  2024-04 [33][34][54].
- **Code mode inside the proxy.** It needs a sandbox, and Anthropic's API and Cloudflare already offer it to the bots
  that can use it [4][27].
- **Speculative execution of tool calls.** Up to 20% lower latency at 55% prediction accuracy [50]. The proxy can't
  guess arguments, and annotations are untrusted [15].
- **LMCache and CacheBlend.** They are GPU-server KV layers for vLLM and SGLang [23][24], not a single Mac.

## 2. Provider prompt and KV caching (Nov 2026)

### 2a. What each provider does

| Provider | How you turn it on | Minimum | Price of a cached read | Write cost / TTL | Cache key / routing | Breaks the cache |
|---|---|---|---|---|---|---|
| **Anthropic** [1][2] | `cache_control: {type: "ephemeral"}` per block (up to 4 breakpoints), or a top-level one that moves forward ("automatic caching", not on Bedrock `bedrock-runtime`) | 512 tokens (Opus 5.5, Opus 5, Sonnet 5.5, Haiku 5.5, Fable/Mythos 5.x); 1,024 (Opus 4.8, Sonnet 5, Sonnet 4.5/4.6, Opus 4/4.1, Sonnet 4); 2,048 (Opus 4.7, Mythos Preview); 4,096 (Opus 4.5/4.6, Haiku 4.5). Shorter prompts are just not cached, with no error | 0.1× base; 0.05× on Opus 5.5 and Sonnet 5.5; 0.025× on Fable/Mythos 5.1–5.2 | 5 min at 1.25×; 1 h at 2×. A read refreshes the TTL at no cost | Exact prefix in the order `tools` → `system` → `messages`. Each breakpoint looks back at most 20 positions | Any change to a tool definition invalidates everything. `tool_choice`, images, `disable_parallel_tool_use`, thinking and effort settings invalidate messages. Web search and citations toggles invalidate system and messages |
| **OpenAI** [7] | Automatic (implicit breakpoints) | GPT-5.6+: 1,024 visible input tokens; earlier models vary | 0.1×; 0.05× on GPT-6.1 Sol/Terra | GPT-5.6+: 1.25× (30 min) or 2.5× (12 h, Responses API only). Earlier models: no write charge, `in_memory` (5–10 min) or `24h` | `prompt_cache_key`. Above about 15 requests per minute per prefix, overflow routing can spill to other machines | Changing tools (names, descriptions, schemas, *order*), `parallel_tool_calls`, `reasoning.effort`, `text.format`, compaction, or anything before a breakpoint |
| **xAI (Grok)** [9][10] | Prefix reuse; the docs don't say whether it's opt-in **[unverified]** | Not stated | "Substantially lower" than regular input; no figure on the caching pages | Not stated; entries can be evicted under memory pressure | `x-grok-conv-id` header (Chat Completions) or `prompt_cache_key` (Responses) to pin to a server | Editing earlier messages |
| **Google Gemini** [11] | Implicit caching "enabled by default for all Gemini 2.5 and newer models" | 4,096 (Gemini 4 family, 3.8/3.6 Flash, 3.1 Pro Preview); 2,048 (2.5 Flash/Pro) | Not stated on the page | Explicit caching only on Gemini 3 and 2.5 via `generateContent`; not for Gemini 4 or the Interactions API | — | — |
| **DeepSeek** [12][13] | On by default, on disk | Prefix units, stored at message ends and at fixed intervals (interval not stated) | `deepseek-flash`: $0.003 vs $0.15 per MTok off-peak (2%); `deepseek-v4-pro`: $0.022 vs $0.66 (3.3%) | Best effort; cleared "within a few hours to a few days" | — | Any change to the prefix |

### 2b. How dynamic tool lists break caching, and the sanctioned ways around it

- **Anthropic.** Deferred tools (`defer_loading: true`) "are not included in the system-prompt prefix". A discovered
  tool is appended inline as a `tool_reference` block, "so prompt caching is preserved" [2][3]. A deferred tool can't
  carry `cache_control`; put the breakpoint on a non-deferred tool [3].
  - **Custom tool search.** Any client tool can return `tool_reference` blocks, so TopRelay's own BM25 can drive
    Claude's deferred loading [3].
  - **Mid-conversation tools.** The `inline-tools-2026-09-15` beta adds a tool through a `tool_addition` block in a
    mid-conversation system message and leaves `tools` unchanged [1].
- **OpenAI.**
  - Keep `tools` stable. Use `allowed_tools` or `tool_choice: "none"` instead of removing tools.
  - Add tools mid-thread with a developer-role `additional_tools` item.
  - Tool search (`{"type":"tool_search"}`, GPT-5.4+, hosted or `execution: "client"`) appends discovered tools at the
    end "to preserve the model's cache". Keep each namespace under 10 functions [7][8].
- **Measured.** On DeepResearch Bench, caching cut cost by 41–80% across OpenAI, Anthropic and Google. Naive
  full-context caching "can paradoxically increase latency". The authors recommend dynamic content at the end of the
  system prompt, no dynamic function lists, and no caching of dynamic tool results [28].
- **MCP's part.** Servers SHOULD return `tools/list` "in a deterministic order … [to] improve LLM prompt cache hit
  rates" [14][15]. **TopRelay's proxy is the server the bot sees**, so this is our job.
  - Today our allow-list filter drops tools from the app's list. That keeps the app's order, but the bot's view still
    changes whenever the allow-list changes.

### 2c. Prompt-layout rules for the brain

Order, from the most stable to the least:

```
[tools]        sorted by namespaced name; always-loaded set (≤5) + search tool; the rest deferred
               ── breakpoint 1 (last non-deferred tool)
[system]       fixed instructions, provider quirks, the tool-category line ("you can search for tools for Notes, Mail…") [3]
[skills index] name + one-line description per skill, sorted; bodies load on demand
               ── breakpoint 2
[memory]       frozen USER.md + MEMORY.md snapshot, taken once per session (agent-harnesses-2026-11.md §3)
               ── breakpoint 3
[conversation] the phone command, tool calls, results; the automatic (moving) breakpoint
```

Rules:

1. **Never change `tools` inside a session.** New tools come in through tool search or inline additions [1][7][8].
   The per-session tool set is the content hash of the sorted definitions. Log it, so a cache miss can be explained.
2. **Nothing time-varying above the conversation.** No "today is …", no unread counts. Put the time in the user turn.
   This follows the "dynamic content at the end" finding [28].
3. **Memory is a snapshot.** Writes during a session go to daily notes and show up next session, as Hermes' frozen
   snapshot does (`agent-harnesses-2026-11.md`).
4. **Pick the TTL from the traffic.** A phone command arrives every few minutes or hours, and inside one command the
   loop makes several calls seconds apart.
   - Per Anthropic's multipliers [1], with a prefix of P tokens and k commands spaced more than 5 min apart within an
     hour:
     - 5 min TTL: 1.25·k·P
     - 1 h TTL: 2·P + 0.1·(k−1)·P (0.05 on the 5.5 models)
   - So **the 1 h TTL wins from k = 2**: 2.1·P vs 2.5·P.
   - A 5 min cache on a lone command costs 25% *more* than no cache for the first call. It pays off only through the
     reads in that command's later rounds.
   - Use 1 h on breakpoints 1–2 and 5 min on the moving breakpoint. OpenAI GPT-5.6+ gives the same choice between
     30 min at 1.25× and 12 h at 2.5× [7].
5. **Set the routing key.**
   - OpenAI `prompt_cache_key`: one per brain session [7].
   - xAI `x-grok-conv-id`: the session id [9].
   - Anthropic and DeepSeek need none.
6. **Clear tool results with `clear_at_least`.** Anthropic's `clear_tool_uses_20250919` (beta
   `context-management-2025-06-27`) invalidates the cached prefix when it clears. Set `clear_at_least` so each clear
   is worth a cache write [6]. On other providers, do the same in our own loop: clear old results in large batches,
   rarely.
7. **Meet the minimum.** On Haiku 4.5 or Opus 4.5/4.6 a prefix under 4,096 tokens isn't cached at all [1]. Read
   `cache_read_input_tokens` / `cached_tokens` and show a "cache hit %" in the brain's debug view.

## 3. Local KV cache reuse (only if the user runs a local model)

| Runtime | What exists | What the brain should do |
|---|---|---|
| **llama.cpp server** [18] | Prompt cache on by default (`--cache-prompt`, per-request `cache_prompt: true`); a host-memory prompt cache (`--cache-ram`, 8,192 MiB default); `--cache-reuse N` (reuse through KV shifting); slot routing by prompt similarity (`--slot-prompt-similarity`, 0.10); `POST /slots/{id}?action=save\|restore` with `--slot-save-path`; usage reports `timings.cache_n` and `prompt_tokens_details.cached_tokens` | Keep the §2c layout, so the stable prefix is the shared prefix. Optionally save the slot after the first prefill and restore it after a server restart. Note the doc's warning that cached prompts can make output nondeterministic |
| **Ollama** [19] | `keep_alive` defaults to 5 min (`OLLAMA_KEEP_ALIVE`, or per request; negative keeps the model loaded); **default context 4,096 tokens** (`OLLAMA_CONTEXT_LENGTH` / `num_ctx`); `OLLAMA_NUM_PARALLEL` 1; RAM grows with `NUM_PARALLEL × CONTEXT_LENGTH`. The FAQ says nothing about prompt KV reuse **[unverified]** | Send `keep_alive` (e.g. `"30m"`) on every brain request, so a phone command doesn't pay for a model load. **Set `num_ctx` explicitly**: 4,096 can't hold a useful tool list plus memory. Measure prefix reuse |
| **MLX (`mlx-lm`)** [20] | `mlx_lm.cache_prompt` and `make_prompt_cache`/`save_prompt_cache` (safetensors files); the server has an in-memory LRU prompt cache. A PR to persist it to disk was closed, not merged **[secondary sources]** | Only matters if we ever drive `mlx_lm.server`. Same layout rule |
| **vLLM / SGLang** [21][22] | vLLM hashes each full KV block (parent hash + tokens + extras, SHA-256 by default since v0.11) and evicts LRU. SGLang's RadixAttention keeps a radix tree of prefixes (up to 6.4× throughput in its paper) | Server engines. Only relevant if the user points the brain at one. Nothing to build |
| **LMCache / CacheBlend** [23][24] | KV offload and sharing across vLLM/SGLang (up to 15× throughput); CacheBlend reuses non-prefix chunk KVs, 2.2–3.3× lower TTFT. LMCache notes that context truncation "can greatly reduce prefix cache hit ratio by half" | Not for a Mac. The truncation finding backs rule 6 above: clear rarely, in large batches |
| **Apple Foundation Models** [25] | `LanguageModelSession.prewarm(promptPrefix:)` loads resources and, if given the start of the coming prompt, lets the next `respond` benefit. No guarantee it runs. No KV persistence API found. The 4,096-token context is covered in `agent-harnesses-2026-11.md` | Call `prewarm` with the router's fixed instructions as soon as a phone command arrives, before the classification turn. Pass at most 3–5 tool schemas, pre-selected by our BM25 |

**How much it matters:** local reuse saves *prefill time*, not money. I found no primary benchmark for prefill
speed on Apple Silicon at a few thousand tokens, so this is a test item (§7). If the stable prefix (tools, system,
skills index, memory) is about 3–6k tokens and is re-read on every round of the loop, prefix reuse changes the time
to first token for every round after the first. Ollama's 5 min unload can change it for the *first* round, since
the whole model has to load again.

## 4. Tool-result caching in the proxy

### 4a. What MCP says (2026-07-28)

- **Caching hints cover only `server/discover`, `tools/list`, `prompts/list`, `resources/list`,
  `resources/templates/list` and `resources/read`.** `tools/call` results carry no `ttlMs` [16].
- The cache key is "the request method together with the request parameters that affect the result". Results of MRTR
  retries (with `inputResponses` or `requestState`) "MUST NOT be cached" [16].
- `cacheScope: "private"`: caches "MUST NOT be shared across authorization contexts". A notification that arrives
  while an entry is fresh invalidates it [16].
- Tool annotations (`readOnlyHint`, `idempotentHint`, …) MUST be treated as "untrusted unless they come from trusted
  servers" [15].
- Our pins record the annotations as part of the approved definition. That proves the definition didn't change. It
  doesn't prove the tool behaves as annotated.
- **Consequence:** there is no standard hint saying a tool *result* is cacheable. A result cache is TopRelay policy.
  It should be **opt-in per tool**. The app can *suggest* it for tools that are pinned and approved and annotated
  `readOnlyHint && idempotentHint && !openWorldHint`.

### 4b. Exact-key caching is safe; "stateful" is the trap

- **TVCache** [41] shows the trap. Tool outputs depend on the state left by earlier calls, so its lookup is a
  longest-prefix match over the **whole tool-call history**. In the authors' setting that reached hit rates up to
  70% and cut median tool time to 1/6.9.
- **For the proxy, the equivalent is cheaper:** a per-service **generation counter** that any non-cacheable call to
  that service bumps. Any write to Notes makes every cached Notes read unreachable. Reads from other services are
  unaffected.
- **What it saves:**
  - app latency and work
  - nothing in the bot's context

### 4c. Semantic caching: don't

- **GPTCache** (MIT, last release 0.1.44 on 2024-08-01) [35][54] and **Redis LangCache** (preview, managed or
  licensed self-managed; it caches *LLM responses* keyed by prompt embeddings) [36] target chatbot answers, not tool
  results.
- **False hits are built in.**
  - GPT Semantic Cache reports a ">97%" positive-hit rate [38]. That still means a few percent of hits are wrong.
  - vCache shows that static similarity thresholds give "unexpected error rates". It learns a threshold per entry
    under a user-set error bound [37]. Its repo is CC BY-NC-ND 3.0 [54], so we can't reuse the code.
- **It's an attack surface.**
  - CacheAttack gets an 86% response-hijack rate with crafted suffixes, and it "can induce malicious behaviors in LLM
    agent" [39].
  - An NDSS 2026 paper (Wu et al.) demonstrates semantic cache poisoning, and its proposed defence is partial
    **[secondary: NDSS listing]** [40].
- **The verdict:** a tool argument like `{"query":"invoices march"}` vs `{"query":"invoices april"}` is "semantically
  similar" and has a different answer. Exact keys only.

## 5. Shrinking what bots see

| Technique | Who applies it | Can the proxy do it without breaking bots? |
|---|---|---|
| **Provider tool search / deferred loading** (Anthropic `tool_search_tool_{regex,bm25}_20251119`, up to 10,000 deferred tools, 5 results by default; `mcp_toolset` `default_config.defer_loading`) [3]. OpenAI `tool_search` [8]. Claude Code defers MCP tools by default [17] | The bot's client | No need. These bots already defer, so a huge raw list costs them little. Our aggregated endpoint matters for bots that don't (ChatGPT connectors, Grok, generic clients **[per-client behaviour unverified]**) |
| **Proxy `search_tools` + `call_tool`** on `/all/mcp` (designed in `algorithms-for-linking-2026-10.md` §5a) | Proxy | Yes. It's an ordinary tool. Keep 3–5 hot tools listed directly, as Anthropic's guidance suggests [3]. Tool-retrieval papers back the idea: RAG-MCP [29], MCP-Zero (hierarchical server → tool routing) [30], Toolshed [31], Re-Invoke (rewrites the query into likely tool docs) [32] |
| **Code mode** (Cloudflare: two tools, `search()` and `execute()`, cover 2,500+ endpoints in about 1,000 tokens, vs about 1.17M) [27][26]. Anthropic: 150,000 → 2,000 tokens on a Drive → Salesforce example [5]. Programmatic tool calling: 37% fewer tokens [4] | Bot runtime with a sandbox | Not in the proxy: it needs a JS/WASM sandbox and turns the agent into a code-execution host. Claude API users get programmatic tool calling server-side [4] |
| **Description compression** | Proxy | Risky. Tool-use examples raised accuracy from 72% to 90% [4], so descriptions carry accuracy. Instead, **lint** them: flag descriptions over N tokens and duplicated schema text, and let the user override per tool. Overrides go through pins, so the bot's view stays byte-stable |
| **Result truncation with a link or continuation** | Proxy | Yes, if it's explicit. MCP results can carry `resource_link` items [15], and the spec's "Stateful Tools" guidance (server-minted handles as arguments) fits a `read_more(handle, offset)` tool [15]. The first page ends with one line: "N more items; call read_more with handle=… or open the link". Claude Code itself saves text results over 50,000 characters to a file [17] |
| **LLMLingua compression** (up to 20× [33]; LLMLingua-2 2×–5× at 1.6×–2.9× end-to-end speed-up [34]) | — | No. It's Python, and it drops tokens from data the bot may quote back (IDs, amounts). The last release was 2024-04 [54] |

## 6. Learning from tool calls

### 6a. What the literature offers

| Work | Learns | From | Reported effect | Fit |
|---|---|---|---|---|
| **Agent Workflow Memory** (arXiv 2409.07429) [42] | Reusable workflows (sub-routines) | Past trajectories, offline or online | +24.6% / +51.1% relative success on Mind2Web / WebArena, fewer steps | Brain: workflows → skill proposals |
| **Agentic Plan Caching** (arXiv 2506.14852) [46] | Plan templates, matched by keywords, adapted by a small model | Finished runs | −50.31% cost, −27.28% latency | Brain: keyword match against skill descriptions; the Apple model adapts the template |
| **Agent Skill Induction** (arXiv 2504.06821) [47] | Program skills, **verified** before reuse | Online | +23.5% success, 10.7–15.3% fewer steps | Verification idea: a skill is kept only if its replay succeeded |
| **SkillWeaver** (arXiv 2504.07079) [48] | Skills as APIs | Exploration | +31.8% / +39.8% relative success | Too exploratory: it runs actions to practise. Not for a user's real apps |
| **SpeedRunner** (arXiv 2608.11338) [49] | A coding agent refactors skills from trajectories | Logs, no replay | Best cost reduction (no single number in the abstract) | Later: "improve this skill" as a brain command |
| **Voyager** (arXiv 2305.16291) [43] | Code skill library | Self-verification | 3.3× more items, 15.3× faster milestones (Minecraft) | The library shape |
| **ExpeL** (arXiv 2308.10144) [44] | Natural-language insights | Training tasks | Gains grow with experience (qualitative) | Memory facts like "Notes search needs the folder name" |
| **Agent KB** (arXiv 2507.06229) [45] | Cross-framework experience base | Many agents | smolagents 55.2% → 73.9% pass@3 | Its "disagreement gate" is the useful bit: retrieved experience must not override current evidence |
| **Speculative Actions** (arXiv 2510.04371) [50] | Predicts the next action with a fast model | Live | Up to 55% next-action accuracy, up to 20% lower latency | Only for side-effect-free calls. Low value for us |
| **LLMCompiler** [51] / **AsyncLM** [52] | Parallel / asynchronous function calls | — | 3.7× / 1.6–5.4× lower latency | Brain loop (rec. #7) |

### 6b. What TopRelay can learn, and from what data

- **The proxy's call log** (`agent/internal/calllog`) records time, MCP method, tool, client, status, duration,
  bytes, refusal, reason, leaks and warnings. It records no bodies and **no tool-level `isError`**. So from cloud
  bots we can learn **which tools each bot uses, how often, and in what order**. We can't learn arguments, and
  "success" is only HTTP-level.
- **The brain's own sessions** have the full trajectories: arguments, results and the user's reaction. All
  argument-level learning (templates, defaults) has to come from there.

Concretely:

1. **Usage priors per bot.** Decayed counts per (client, tool) and first-order transitions (client, prev → next)
   over **sessions** split on a 10-minute idle gap. The 2026-07-28 spec dropped `Mcp-Session-Id` [14], so the gap is
   the only boundary.
   - Use them to boost `search_tools` ranking and to choose a bot's 3–5 always-loaded tools.
   - A Markov chain is enough. Bandits (Thompson sampling over which tools to expose) aren't worth it: feedback is
     too sparse to learn from and too noisy.
2. **Frequent sequences → SKILL.md proposals.** Count tool n-grams (n = 2–5) per session in a trie. A sequence seen
   in ≥ 3 sessions on ≥ 2 distinct days becomes a *proposal*: a draft SKILL.md listing the steps and tools. The
   argument templates come from the brain's trajectories where it has them. Following ASI [47], only propose a
   sequence whose replay in the brain succeeded.
3. **Argument defaults per user** (for example "calendar = Work"). These are memory facts with provenance, under the
   OKF rule that only the user's own message makes a preference (`agent-harnesses-2026-11.md` §3). They are shown
   while the brain fills arguments, and never silently applied to a write call.

### 6c. Safety rules for anything learned

- **Learned state may reorder, never permit.** Priors act only *inside* the caller's `Allowed` set and token caveats.
  A proposed skill grants no tool. Running it goes through the proxy, the pins, the shield and phone approval step by
  step, like any call.
- **Resist poisoning from tool output.** A malicious result can steer a bot into calling tools, so it can inflate
  counts.
  - Count only calls that weren't refused, returned 2xx and carried no shield `Warnings` or `Leaks`.
  - Count each tool **at most once per session** toward frequency.
  - Ignore sessions that hit the loop detector.
  - Decay with a 14-day half-life (a starting value to tune).
  - Learn sequences that will become skills only from brain sessions started by the paired phone, never from
    background or channel turns. This is the provenance-tagging rule in `agent-harnesses-2026-11.md` §3.
- **Proposals are text the user approves**, shown with diffs, and written to the signed call log when accepted.
  Agent KB's disagreement gate [45] maps to a prompt rule: a skill is a suggestion, and the current tool results win.
- **Learning must not break caching (§2).** Recomputing a bot's always-loaded set reorders its `tools/list`. So
  recompute at most daily, apply on the next `tools/list_changed`, and only when the set really differs.

## 7. Data structures and Go libraries

| Need | Use | Library (license, latest release) | Notes |
|---|---|---|---|
| Content-addressed keys | SHA-256 over canonical JSON (keys sorted, no whitespace). `encoding/json` already sorts map keys when it marshals `map[string]any` | stdlib (`crypto/sha256`, already used by `pins`) | Watch number formatting: decode with `UseNumber` so `1` and `1.0` stay distinct |
| Result cache (bounded, TTL, cost-aware) | W-TinyLFU [53] | **otter v2** (`maypok86/otter`, Apache-2.0, v2.4.0 2026-10-19; Go ≥ 1.24; expiry, loaders that dedupe concurrent gets, stats, persistence) [55] | Preferred. Alternatives: **ristretto** (`dgraph-io/ristretto`, Apache-2.0, v2.4.2 2026-07-07; TinyLFU admission + SampledLFU, cost-based) [56]; **theine-go** (MIT, v0.6.3 2026-11-04); **golang-lru** (MPL-2.0, last release v2.0.7 in 2023-09) [54]. At hundreds of entries a `map` + `container/list` LRU would also do |
| Coalescing identical in-flight calls | singleflight | `golang.org/x/sync/singleflight` (BSD-3; `x/sync` is already an indirect dependency in `agent/go.mod`) | Or otter's loader |
| Tool search | BM25 inverted index (`map[term][]posting`), as in `algorithms-for-linking-2026-10.md` §5a | Our own (~150 lines). **bleve** (Apache-2.0, v2.6.1 2026-08-24) has BM25 since v2.5.0 [58], but it's a full search engine | Our own: zero dependencies, microseconds at 10³ tools |
| Optional embeddings | Exact cosine (no ANN below about 10⁵ vectors) | **kelindar/search** (MIT, v0.5.0 2026-09-19; needs Go 1.27, matching our `go 1.27.2`; core has no native deps; the `llama` submodule uses purego for GGUF embeddings) [57] | Later, behind Anthropic's `tool_reference` [3]. `chromem-go` is MPL-2.0 and last released 2024-09 [54] |
| Session/trajectory store and FTS | SQLite FTS5 | **ncruces/go-sqlite3** (MIT, v0.37.0 2026-11-07, "cgo-free", SQLite as Wasm turned into Go) [59]; or `modernc.org/sqlite` (BSD-3-Clause, tag v1.62.0) [54] | Settles the "Go SQLite driver" open question in `agent-harnesses-2026-11.md` in favour of a cgo-free one |
| Sequence mining | Trie of tool-name n-grams with counts per node (like TVCache's tree of call sequences [41]) | Our own `map[string]*node` | Radix compression isn't needed: depth ≤ 5, and the alphabet is about 10³ tools |
| Usage counts | Exact maps with decay | stdlib | **Count-min sketch: skip.** Exact counts for 10³ tools × a few bots fit in kilobytes |
| Membership ("seen this args hash?") | Exact `map` with a time window | stdlib | **Bloom filter: skip** at this scale. `bits-and-blooms/bloom` (BSD-2, v3.7.1) if ever needed [54] |
| Near-duplicate results (loop detection, memory dedupe) | 64-bit SimHash (Charikar, STOC 2002; not re-read for this note) over shingles; Hamming distance ≤ 3 | Our own (~40 lines) | Go SimHash/MinHash repos are stale (`mfonda/simhash` last pushed 2015, `ekzhu/minhash-lsh` 2019) [54] |

## 8. Design sketches

### 8a. Cache-stable lists plus `search_tools` with learned priors (recs. #1 and #2)

**Packages:** `agent/internal/toolindex` (new: BM25, the stable list, priors) and `agent/internal/usage` (new: reads
the call log and keeps the priors). The proxy calls `toolindex` from the `tools/list` transform for `/all/mcp`.

- **The stable list.** For the aggregated endpoint, build the bot's list as sort-by-`svc.tool` over the
  allowed, unheld tools, serialised once. Keep the bytes in `listCache[client]` along with
  `etag = sha256(bytes)`.
  - Rebuild only on `tools/list_changed` from an app, a pin change, an allow-list or profile change, or the daily
    prior recompute.
  - Emit `ttlMs` (e.g. 300000) and `cacheScope: "private"`. The list depends on the caller's token caveats, and the
    spec says filtered per-user lists are private [16].
  - For per-app endpoints, keep the app's order but strip only denied tools, so the order is still deterministic.
- **Batching.** Changes to the allow-list from the Mac app are coalesced over 2 s. Then one `list_changed` goes out
  instead of several.
- **The `search_tools` score:**

  ```
  score(t | q, c) = BM25(q, t)                       // fields: name×3, title×2, description, params
                  + α · log(1 + n_c(t))              // n_c: decayed count of tool t for client c
                  + β · P_c(t | last_c)              // transition from client c's last tool, if within 10 min
  only tools with BM25(q,t) > 0 are eligible;        // priors re-rank, never surface unrelated tools
  α, β small (start 0.3, 0.5); top 5 returned by default (matches Anthropic's default [3])
  ```

- **The always-loaded set** per bot: the top 3–5 by `n_c`, recomputed daily (§6c). Until there's data, use the
  user's pinned favourites.
- **Data:**
  - `n_c(t)` and `P_c(·|·)` live in `usage.json`, rebuilt from the call log at start-up by streaming the JSONL.
  - The log is append-only and hash-chained, so the counts can always be recomputed from it.
  - Decay: `n ← n · 2^(−Δt/14d)` on read.

### 8b. Read-only result cache (rec. #5)

**Package:** `agent/internal/resultcache`. A new `Proxy.Cache` field, checked in the `tools/call` path *after* the
allow-list, pin-hold and rate-limit checks. A hit counts as a call for rate limiting.

```go
type Key [32]byte // sha256 of the fields below, length-prefixed
// service, tool, pinHash(tool), canonicalArgs, client, gen[service]

type Entry struct {
    Body      []byte    // JSON-RPC result after the shield, BEFORE LinkTransform
    StoredAt  time.Time
    TTL       time.Duration
}
```

- **Eligible** only when all of these hold:
  - the user opted the tool in, in the Mac app (suggested for pinned, approved,
    `readOnlyHint && idempotentHint && !openWorldHint` tools)
  - the request has no `inputResponses` or `requestState` (MRTR retries MUST NOT be cached [16])
  - the result has `resultType: "complete"`, `isError: false`, and no shield `Blocked` or `Leaks`
- **TTL:** per tool, 30 s by default and at most 10 min. Freshness is checked on read, never polled (the spec's TTL
  rule [16]).
- **Invalidation:**
  - **A per-service generation `gen[svc]`, an atomic counter.** It's bumped *before forwarding* any `tools/call` to
    that service whose tool isn't opted in, so a write can't race a read into the cache. It's also bumped on
    `tools/list_changed`, on a pin change, on an upstream reconnect or app restart, and on an allow-list change.
  - Old keys just become unreachable, and otter's TinyLFU evicts them.
  - A "Clear cache" button sets every `gen` forward.
- **Coalescing:** concurrent identical calls share one upstream request (singleflight on `Key`).
- **Ordering matters for links.** `LinkTransform` mints *single-use* public URLs (`mcpproxy/transform.go`). The cache
  stores the result before that transform and re-runs it on every hit, so a cached result never serves a used-up
  link. The shield runs before storing, so a hit returns already-checked content. Re-running the shield on a hit is
  cheap and optional.
- **Per client.** `client` is in the key. Sharing across bots would add few hits, and bots can have different
  policies.
- **Call log:** add `cached: true` (and `coalesced: true`) to the entry, so the user can see hits. That's a calllog
  schema change, so it's listed in §9.
- **Size:** otter with a cost of `len(Body)`, 32 MiB total by default. Entries over 1 MiB are never cached.

### 8c. The brain's prompt assembler (rec. #3)

**Package:** `agent/internal/brain/prompt`. A pure function `Assemble(session, turn) → ProviderRequest`, with one
adapter per provider:

- **Inputs, frozen at session start:**
  - `toolset` (sorted, with a content hash)
  - `system` (fixed text plus the categories line)
  - `skillsIndex` (sorted name and description)
  - `memorySnapshot`
- **Anthropic adapter.**
  - Breakpoints on the last non-deferred tool (1 h), the end of the skills index (1 h) and the end of the memory
    (1 h), plus top-level automatic caching for the moving conversation breakpoint (5 min). Explicit breakpoints and
    the automatic one share the limit of 4 [1].
  - The deferred tools are the rest. TopRelay's `search_tools` result returns `tool_reference` blocks [3].
  - `context_management` with `clear_tool_uses_20250919`, `clear_at_least` ≥ 5k tokens, and `exclude_tools` for the
    memory tool [6].
- **OpenAI adapter.** Stable `tools` with `defer_loading` namespaces of fewer than 10 functions each, plus
  `tool_search` with `execution: "client"` backed by our index [8]. `prompt_cache_key = session id`; on GPT-5.6+,
  `prompt_cache_options.ttl = "12h"` only when the user's command rate justifies it (§2c rule 4) [7].
- **xAI adapter.** `x-grok-conv-id = session id` [9].
- **OpenAI-compatible and local adapter.** The same byte order, `keep_alive` and an explicit `num_ctx` for Ollama
  [19]; `cache_prompt: true` for llama.cpp [18]. Report `cached_tokens` / `timings.cache_n`.
- **Apple router.** Pass only BM25's top 3–5 schemas and call `prewarm(promptPrefix:)` with the router instructions
  [25].
- **Invariant test.** For every adapter, two consecutive turns of one session must produce byte-identical request
  prefixes up to the last fixed breakpoint. This is a golden test in `brain/prompt`.

## 9. Lift / reuse table

| Source | License | What to take | Where it goes |
|---|---|---|---|
| Anthropic tool search, custom `tool_reference` [3] | API | Our BM25 returns `tool_reference` blocks, and the 3–5 always-loaded rule | `brain/prompt` Anthropic adapter; `toolindex` defaults |
| OpenAI `tool_search` with `execution: "client"` [8] | API | A client-run search backed by our index | `brain/prompt` OpenAI adapter |
| MCP caching utility [16] | Spec | `ttlMs`/`cacheScope` semantics, MRTR exclusion, private per auth context | `mcpproxy` list results; `resultcache` eligibility |
| "Don't Break the Cache" (arXiv 2601.06007) [28] | Paper | Dynamic content last, no dynamic function lists, cache the system prompt first | §2c rules |
| TVCache (arXiv 2602.10986) [41] | Paper | State-aware invalidation, done as a per-service generation counter; tree of call sequences | `resultcache`; `usage` trie |
| RAG-MCP [29], MCP-Zero [30], Re-Invoke [32] | Papers | Server → tool two-stage routing; rewriting the query into tool-doc vocabulary (later, with the Apple model) | `toolindex` |
| AWM [42], APC [46], ASI [47] | Papers; AWM code Apache-2.0 (`zorazrw/agent-workflow-memory`) [54] | Workflow induction, keyword-matched plan templates, verify before keeping | `brain/skills/propose` (new) |
| Agent KB [45] | Paper; code Apache-2.0 | The "disagreement gate" as a prompt rule | brain system prompt |
| LLMCompiler [51] | MIT | A DAG of independent calls run in parallel | brain loop (with Operator's fail-closed `parallelSafe` classifier, per `agent-harnesses-2026-11.md`) |
| otter v2 [55] | Apache-2.0 | W-TinyLFU cache with TTL and loaders | `resultcache` |
| `x/sync/singleflight` | BSD-3 | Coalescing | `resultcache` |
| ncruces/go-sqlite3 [59] | MIT | cgo-free SQLite + FTS5 | brain session and trajectory store |
| kelindar/search [57] | MIT | Exact cosine index (later) | `toolindex` (optional) |
| llama.cpp slot save/restore [18], Ollama `keep_alive`/`num_ctx` [19], Apple `prewarm` [25] | MIT / MIT / system | Request settings | `brain/prompt` local adapters |
| **Don't take:** vCache code [37] | CC BY-NC-ND 3.0 | — | — |
| **Don't take:** GPTCache [35], LLMLingua [33] | MIT | Python, wrong layer | — |

## 10. Open questions to test

1. **Do the bots' MCP clients honour `ttlMs` and react to `list_changed` mid-conversation?** Check claude.ai
   connectors, ChatGPT, Grok and Claude Code. Claude Code refreshes on `list_changed` [17], but its discovery cache
   is off by default since v2.1.238. Anthropic pins the tool list per conversation (`agent-ecosystem-2026-10.md`).
   The others are **[unverified]**.
2. **Which bots still load all MCP tools up front?** That decides how much the aggregated endpoint saves for each
   bot. Claude Code defers by default [17]. ChatGPT and Grok connectors: **[unverified]**.
3. **Result-cache hit rate for one real user.** Replay a week of call logs: count (tool, args-hash) repeats within 30
   s, 2 min and 10 min with no write in between. That needs an args hash in the call log. Add `argsHash` (SHA-256 of
   the canonical args, which isn't a body and leaks nothing readable) and `toolError` (the result's `isError`). Both
   are calllog schema changes for the owner to decide.
4. **Do bots send both `structuredContent` and its text duplicate to the model?** The spec says a server SHOULD
   return both [15]. If a client passes both on, every structured result costs about twice its tokens. Then the
   proxy could drop the text copy for clients known to read `structuredContent` **[per-client behaviour
   unverified]**.
5. **Prefill time on Apple Silicon** with and without a warm prefix for 2k, 4k and 8k tokens. Test llama.cpp
   (`timings`), Ollama (after unload vs warm) and Apple `prewarm`.
6. **Is the 1 h TTL worth it?** Count real command spacing from the brain's sessions. It wins only at ≥ 2 commands
   per hour that are more than 5 min apart (§2c).
7. **Do learned priors improve `search_tools`?** Build an offline eval: hold out the last week of sessions and ask
   whether the tool called next is in the top 5 with and without priors.
8. **Can a malicious tool result inflate priors** despite the once-per-session cap? Red-team with a test app whose
   output tells the bot to call a tool repeatedly.

## Sources

1. Anthropic, Prompt caching. https://platform.claude.com/docs/en/build-with-claude/prompt-caching
2. Anthropic, Tool use with prompt caching. https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-use-with-prompt-caching
3. Anthropic, Tool search tool. https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool
4. Anthropic Engineering, advanced tool use post (2025-11-24). https://www.anthropic.com/engineering/advanced-tool-use
5. Anthropic Engineering, "Code execution with MCP" (2025-11-04). https://www.anthropic.com/engineering/code-execution-with-mcp
6. Anthropic, Context editing. https://platform.claude.com/docs/en/build-with-claude/context-editing
7. OpenAI, Prompt caching. https://developers.openai.com/api/docs/guides/prompt-caching
8. OpenAI, Tool search. https://developers.openai.com/api/docs/guides/tools-tool-search
9. xAI, Prompt caching: how it works / maximizing cache hits. https://docs.x.ai/developers/advanced-api-usage/prompt-caching/how-it-works ; https://docs.x.ai/developers/advanced-api-usage/prompt-caching/maximizing-cache-hits
10. xAI, Prompt caching: usage and pricing. https://docs.x.ai/developers/advanced-api-usage/prompt-caching/usage-and-pricing
11. Google, Gemini API caching. https://aistudio.google.com/docs/caching
12. DeepSeek, Context caching (KV cache). https://api-docs.deepseek.com/guides/kv_cache
13. DeepSeek, Models and pricing. https://api-docs.deepseek.com/quick_start/pricing
14. MCP specification 2026-07-28, Key changes. https://modelcontextprotocol.io/specification/2026-07-28/changelog
15. MCP specification 2026-07-28, Tools. https://modelcontextprotocol.io/specification/2026-07-28/server/tools
16. MCP specification 2026-07-28, Caching. https://modelcontextprotocol.io/specification/2026-07-28/server/utilities/caching
17. Claude Code, MCP (tool search, output limits, discovery cache, `list_changed`). https://code.claude.com/docs/en/mcp
18. llama.cpp server README. https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md
19. Ollama FAQ. https://docs.ollama.com/faq
20. mlx-lm repo and PR #1405 (closed). https://github.com/ml-explore/mlx-lm ; https://github.com/ml-explore/mlx-lm/pull/1405 (prompt-cache API details from secondary pages, e.g. DeepWiki) **[secondary]**
21. vLLM, Automatic prefix caching design. https://docs.vllm.ai/en/latest/design/prefix_caching.html
22. Zheng et al., "SGLang: Efficient Execution of Structured Language Model Programs", arXiv 2312.07104.
23. "LMCache: An Efficient KV Cache Layer for Enterprise-Scale LLM Inference", arXiv 2510.09665.
24. Yao et al., "CacheBlend: Fast Large Language Model Serving for RAG with Cached Knowledge Fusion", arXiv 2405.16444.
25. Apple, `LanguageModelSession.prewarm(promptPrefix:)`. https://developer.apple.com/documentation/foundationmodels/languagemodelsession/prewarm(promptprefix:)
26. Cloudflare blog, "Code Mode" (2025-09-26). https://blog.cloudflare.com/code-mode/
27. Cloudflare, "Code Mode: give agents an entire API in 1,000 tokens" (2026-02-20). https://blog.cloudflare.com/code-mode-mcp/
28. "Don't Break the Cache: An Evaluation of Prompt Caching for Long-Horizon Agentic Tasks", arXiv 2601.06007 (v2, 2026-01-31).
29. "RAG-MCP: Mitigating Prompt Bloat in LLM Tool Selection via Retrieval-Augmented Generation", arXiv 2505.03275.
30. "MCP-Zero: Active Tool Discovery for Autonomous LLM Agents", arXiv 2506.01056.
31. "Toolshed: Scale Tool-Equipped Agents with Advanced RAG-Tool Fusion and Tool Knowledge Bases", arXiv 2410.14594.
32. "Re-Invoke: Tool Invocation Rewriting for Zero-Shot Tool Retrieval", arXiv 2408.01875.
33. Jiang et al., "LLMLingua: Compressing Prompts for Accelerated Inference of Large Language Models", arXiv 2310.05736.
34. Pan et al., "LLMLingua-2: Data Distillation for Efficient and Faithful Task-Agnostic Prompt Compression", arXiv 2403.12968.
35. GPTCache. https://github.com/zilliztech/GPTCache
36. Redis, LangCache docs. https://redis.io/docs/latest/develop/ai/langcache/
37. "vCache: Verified Semantic Prompt Caching", arXiv 2502.03771; code https://github.com/vcache-project/vCache (LICENSE: CC BY-NC-ND 3.0).
38. "GPT Semantic Cache: Reducing LLM Costs and Latency via Semantic Embedding Caching", arXiv 2411.05276.
39. "From Similarity to Vulnerability: Key Collision Attack on LLM Semantic Caching", arXiv 2601.23088.
40. Wu et al., "When Cache Poisoning Meets LLM Systems: Semantic Cache Poisoning and Its Countermeasures", NDSS 2026. https://www.ndss-symposium.org/ndss-paper/when-cache-poisoning-meets-llm-systems-semantic-cache-poisoning-and-its-countermeasures/ **[listing only; paper not read]**
41. "TVCache: A Stateful Tool-Value Cache for Post-Training LLM Agents", arXiv 2602.10986.
42. Wang et al., "Agent Workflow Memory", arXiv 2409.07429; code https://github.com/zorazrw/agent-workflow-memory
43. Wang et al., "Voyager: An Open-Ended Embodied Agent with Large Language Models", arXiv 2305.16291.
44. Zhao et al., "ExpeL: LLM Agents Are Experiential Learners", arXiv 2308.10144.
45. "Agent KB: Leveraging Cross-Domain Experience for Agentic Problem Solving", arXiv 2507.06229; code https://github.com/OPPO-PersonalAI/Agent-KB
46. "Agentic Plan Caching: Test-Time Memory for Fast and Cost-Efficient LLM Agents", arXiv 2506.14852.
47. Wang et al., "Inducing Programmatic Skills for Agentic Tasks" (ASI), arXiv 2504.06821.
48. "SkillWeaver: Web Agents can Self-Improve by Discovering and Honing Skills", arXiv 2504.07079.
49. "Better, Faster, Stronger: Programmatic Skill Learning Best Reduces Agent Cost" (SpeedRunner), arXiv 2608.11338.
50. "Speculative Actions: A Lossless Framework for Faster Agentic Systems", arXiv 2510.04371.
51. Kim et al., "An LLM Compiler for Parallel Function Calling", arXiv 2312.04511; https://github.com/SqueezeAILab/LLMCompiler
52. "Asynchronous LLM Function Calling" (AsyncLM), arXiv 2412.07017.
53. Einziger, Friedman, Manes, "TinyLFU: A Highly Efficient Cache Admission Policy", arXiv 1512.00727.
54. GitHub REST API (`/repos/{owner}/{repo}`, `/releases/latest`, `/tags`, `/contents/LICENSE`), queried 2026-11-14.
55. otter README. https://github.com/maypok86/otter
56. ristretto README. https://github.com/dgraph-io/ristretto
57. kelindar/search README. https://github.com/kelindar/search
58. bleve v2.5.0 release notes (BM25 scoring). https://github.com/blevesearch/bleve/releases/tag/v2.5.0
59. ncruces/go-sqlite3 README. https://github.com/ncruces/go-sqlite3
