# Algorithms and data structures for TopRelay (2026-10)

Research for `docs/brief.md`, written 2026-10-31. The question: which data structures and algorithms unlock concrete
features as TopRelay grows from "a local MCP server gets a URL" toward "link any app to any bot, device or other
app". Each entry names the feature, why it fits the platform limits, and a sketch. Citations are in the Sources
list at the end; Cloudflare numbers were re-checked against the docs on 2026-10-31.

## The top 8

| # | Technique | Feature it unlocks | Runs in |
|---|---|---|---|
| 1 | **Credit-based flow control, frame coalescing and deficit round robin** over the one WebSocket | Large and streamed replies without blowing the Durable Object's memory, fewer billed messages, no single slow stream stalling the rest | DO + agent |
| 2 | **GCRA rate limiting plus write-behind counters** | Per-service and per-client quotas, and a live "you've used 41% of today's free tier" in `toprelay status`, without spending the free row-write budget | DO |
| 3 | **Macaroon-style HMAC-chain capability tokens** | Hand any bot a token scoped to one service, a few tools, an expiry, made offline from the CLI and narrowed further by whoever holds it. Checked in the Worker, no KV write, no DO call | Worker + agent |
| 4 | **Request-outcome table with sequence-numbered chunks** (resume, not retry) | A tool call survives the laptop's socket dropping mid-call: the agent replays the result instead of the app running it twice | Agent + DO |
| 5 | **BM25 inverted index over tool metadata** | One URL for all of a person's local apps, with a `search_tools` meta-tool, so bots without native tool search can still use hundreds of tools | Agent |
| 6 | **Priority queue over the single Durable Object alarm** (SQLite table indexed on due time) | Scheduled calls ("every morning, call tool X and send the result to bot Y") and store-and-forward delivery of webhooks to a sleeping laptop | DO |
| 7 | **RFC 6962 Merkle tree log** with checkpoints anchored in the DO | A tamper-evident call log: the owner can prove what a bot called, and spot a local log edited or truncated later | Agent (+ DO for checkpoints) |
| 8 | **Noise KK sessions after CPace or SAS pairing** | End-to-end encrypted links between the owner's own devices and apps (laptop to laptop, phone to laptop) that the relay cannot read | Agent + TopRelayKit |

Below the top 8: hybrid logical clocks with a last-writer-wins map (config sync across devices), DAG checks and a
hop limit (app-to-app pipelines), range-based set reconciliation (merging logs across devices), exponential backoff
with full jitter (reconnects), Aho-Corasick (secret redaction), Hungarian matching (field mapping), DDSketch
(latency percentiles). The "Skipped" section lists what was considered and dropped.

## Constraints that decide the choices

Verified against Cloudflare docs, 2026-10-31:

- **Worker:** 10 ms CPU per request on Free, 128 MB per isolate, 50 subrequests per request. Anything per request
  in the Worker must be a few hashes and a map lookup.
- **SQLite-backed DO, Free plan:** 5 M rows read/day, **100 k rows written/day**, 5 GB stored, 100 k requests/day.
  "Every row update to an index counts as an additional row written." So one log row per request with one index
  uses 2 of the 100 k daily writes: per-request writes in the DO would hit the write cap before the request cap.
- Max row/string/BLOB 2 MB, 100 bound parameters, 100 KB statement. FTS5 (with `bm25()`), JSON functions and math
  functions are available.
- **One alarm per DO**; `setAlarm` overwrites; failed handlers retry with backoff (2 s start, 6 retries).
- WebSocket attachment (`serializeAttachment`) max 16,384 bytes.
- **KV Free:** 100 k reads/day but only **1,000 writes/day** and 1 write/s per key. Don't put counters, nonces or
  per-request state in KV.
- Rate Limiting binding: per Cloudflare location, approximate ("can exceed this threshold"), periods of 10 or
  60 s only. The docs point to Durable Objects for a strict global limit. Free-plan availability not stated: check.
- Workers WebCrypto has HMAC, Ed25519 sign/verify, X25519 `deriveBits`, and the non-standard
  `crypto.subtle.timingSafeEqual`.
- From the spec: incoming WebSocket messages bill at 20:1, outgoing ones are free. Relay→agent frames (credits,
  cancels) cost nothing; agent→relay frames (chunks) do.

Design rule from the brief, kept throughout: the Worker stays protocol-blind. Anything that reads MCP goes in the
agent.

---

## 1. Streams over one WebSocket

### 1a. Credit-based flow control (top 8, #1)

**Problem.** Every request for a service shares one WebSocket. If a bot reads a large SSE stream slowly, the agent
keeps sending chunks and the DO buffers them in memory (128 MB isolate). One slow consumer can take down every
stream for that service.

**Technique.** Per-stream credit windows, as in HTTP/2 (RFC 9113 §5.2, `WINDOW_UPDATE`): the receiver advertises how
many bytes it will accept, and the sender never exceeds it. Add a connection-level window too.

**Sketch.**
- Each stream starts with `W = 256 KiB` of credit. The agent subtracts the payload size for each chunk and stops
  reading from the local app when credit reaches 0 (in Go, just stop calling `Read` on the upstream body; TCP
  pushes the backpressure back into the app).
- The DO writes chunks into the response `TransformStream`. After `await writer.ready` resolves (the bot's side has
  drained), it sends `{id, credit: n}` back. Relay→agent messages are free.
- Memory bound in the DO: `W × open streams + connection window`. Fixed, not driven by the bot.

### 1b. Coalescing and binary frames

**Problem.** Agent→relay messages bill at 20:1, and the v1 protocol base64-encodes bodies inside JSON (33% size
overhead, plus JSON parse CPU in the DO).

**Technique.** Nagle-style coalescing (RFC 896): hold small chunks briefly and flush on a size or time threshold.
Use binary WebSocket frames with a tiny header.

**Sketch.** Flush a stream's buffer at ≥ 32 KiB, 20 ms after its first unflushed byte, or at end of stream. Several
streams' frames can share one WebSocket message. Frame: `type u8 | stream-id uvarint | seq uvarint | len uvarint |
payload`. Keep JSON for control frames if that helps debugging. Never hold an SSE event past the timer: MCP
progress notifications must stay prompt.

### 1c. Fair scheduling between streams: deficit round robin

**Problem.** A 20 MiB response going out as consecutive frames delays a small `tools/list` reply queued behind it
(head-of-line blocking at the sender).

**Technique.** Deficit round robin (Shreedhar & Varghese, SIGCOMM 1995): each active stream gets a quantum of bytes
per round; O(1) work per frame, near-perfect byte fairness.

**Sketch.** In the agent's writer goroutine: a ring of streams with pending data, each with a `deficit` counter.
Per turn, `deficit += quantum (16 KiB)`, send frames while `len(frame) ≤ deficit`, move on. Combine with 1a: a
stream with no credit leaves the ring until credit arrives. Applies equally in the DO for relay→agent request
bodies.

**Decide now.** Credits, sequence numbers and binary framing change the wire protocol. Reserve `v:2` fields before
v1 ships, even if the agent uses fixed windows at first.

### 1d. Resume instead of retry (top 8, #4)

**Problem.** The brief says never retry a relayed request, which is right. But laptops drop sockets all the time
(wake, Wi-Fi change, deploys), and today any call in flight at that moment fails with 503 even if the app finished
the work.

**Technique.** An idempotency table keyed by request id, which is how Stripe-style idempotency keys and TCP
retransmission work: the receiver records outcomes, so a lost acknowledgement doesn't re-run the operation. It
doesn't rely on MCP's `idempotentHint`. The spec calls annotations hints that clients "should never make tool use
decisions" on from untrusted servers.

**Sketch.**
- The DO assigns ids that are unique across reconnects: `incarnation (random 64-bit at DO start) || counter`.
- The agent keeps `map[id] → {state: running|done, status, headers, chunks ring buffer, expires}` for 2 minutes
  (bounded by bytes, LRU eviction).
- On reconnect the agent sends `{resume: [{id, state, lastSeq}]}`. The DO still holds its pending requests
  (in-flight I/O keeps it in memory). For each one: `done` → the agent replays the stored response; `running` →
  the agent re-attaches the stream from `lastSeq+1`; unknown to the agent → fail with 503 as today (it never ran).
- An id is never executed twice by the agent: on a duplicate request it looks the id up first.
- Same idea as SSE's `Last-Event-ID`, one layer down.

---

## 2. Quotas and usage

### 2a. GCRA in the Durable Object (top 8, #2)

**Feature.** Exact per-service and per-client rate limits ("this bot: 60 calls/min, burst 10"), a pairing-code
attempt limiter, and clear `429` errors with `Retry-After`.

**Technique.** Generic Cell Rate Algorithm (ATM Forum; write-up by Brandur Leach). It's equivalent to a token bucket
but stores **one number per key**, the theoretical arrival time (TAT).

**Sketch.**
```
T   = period / limit                 // emission interval
tau = T * (burst - 1)                // burst tolerance
allow(now): if now < tat - tau → deny, retryAfter = tat - tau - now
            else tat = max(tat, now) + T; allow
```
A DO is single-threaded, so this is atomic without locks. Keep `Map<key, tat>` in memory and don't persist it:
losing it on eviction only resets limits, which is fine for abuse control. Cost is O(1) and a few bytes per key.

**Where.** Rejecting in the Worker saves a DO request, but Worker isolates don't share memory. Layers:
(1) Rate Limiting binding in the Worker for coarse flood control (approximate, per location), if the Free plan
has it; (2) GCRA in the DO for the exact limit; (3) `golang.org/x/time/rate` (a token bucket) in the agent for
per-tool limits the owner sets ("`send_email`: 5/hour").

### 2b. Write-behind counters for the free-tier meter

**Feature.** `toprelay status` shows requests, DO rows written and streamed messages against the daily free limits,
and the relay returns a clear "daily budget reached" error before Cloudflare starts failing operations.

**Technique.** Aggregate in memory, flush periodically: the standard write-behind cache. Pair it with a ring buffer
of per-minute buckets and a running sum, for O(1) sliding-24h totals.

**Sketch.** The DO keeps `counts[metric]` in memory. An alarm every 60 s (through the scheduler in §4a) upserts one
row per metric for the current UTC day: at most 1,440 × metrics rows per day instead of one per request. On
eviction you lose at most a minute of counts, and Cloudflare's own dashboard is the authority anyway. Agent side:
`[1440]uint32` per-minute buckets plus a running total; advancing a minute subtracts the bucket that drops out.

---

## 3. Credentials and policy

### 3a. Macaroon-style attenuable tokens (top 8, #3)

**Feature.** `toprelay key notes --tools search,read --expires 24h` makes a token for a friend's bot or a CI job.
Anyone holding a token can narrow it further (fewer tools, shorter expiry) offline, without the root secret, and
it can never be widened. No KV write per token (KV Free allows 1,000 writes/day) and no redeploy per key (the
current plan adds a `secret_text` binding per bearer key hash).

**Technique.** Macaroons (Birgisson et al., NDSS 2014): a chained HMAC over caveats.
`sig₀ = HMAC(K, id)`, `sigᵢ = HMAC(sigᵢ₋₁, caveatᵢ)`. Adding a caveat needs only the current signature.

**Sketch.**
- `K` = one per-service root secret (Worker secret). Token =
  `base64url(id | caveats[] | sig)`, caveats as short strings: `svc=notes`, `exp=1798675200`,
  `tools=search,read`, `client=ci`.
- Worker: recompute the chain with `crypto.subtle` HMAC (n caveats = n HMACs, microseconds),
  `timingSafeEqual` on the result, check `svc` and `exp` itself (headers only). Fits 10 ms easily.
- **Tool caveats are enforced by the agent**, not the Worker (the Worker can't see tool names in bodies). The
  Worker forwards the verified caveat list in a relay-only header (stripped from inbound requests, never passed to
  the app). The agent takes the intersection with the service allow-list.
- Revocation needs state: keep expiries short and keep a small deny-list of token ids. A secret binding updated on
  `rotate` costs no KV writes; a KV key read per request also works within 100 k reads/day.
- Agent-side, represent tool sets as bitsets over the service's tool index (`[]uint64`): effective permission =
  `allowList & tokenMask`, O(n/64). Use names, not indexes, in tokens, so catalog changes don't silently remap.

**Later: Biscuit** (Eclipse Biscuit spec). Ed25519 signature chain plus Datalog policies; verifiable with only the
public key, with third-party blocks. Worth it when someone other than the owner must verify tokens (another
person's relay, a partner app). Today the Worker holds the secret anyway, so the HMAC version is simpler and
cheaper. The Biscuit runtime would ship as WASM: measure bundle size and CPU against 10 ms first.

---

## 4. Scheduling and app-to-app links

### 4a. Priority queue over the single alarm (top 8, #6)

**Feature.** Scheduled tool calls, periodic counter flushes (§2b), token expiry sweeps, and **store-and-forward**:
an incoming webhook (GitHub, Stripe, a phone shortcut) for an app on a sleeping laptop is queued and delivered on
reconnect. This is the first "app → app" feature that works when the laptop is off.

**Technique.** A min-priority queue keyed by due time. SQLite's B-tree index on `due_at` is the heap: `ORDER BY
due_at LIMIT 1` is O(log n), and the DO's one alarm always points at the minimum.

**Sketch.**
```sql
CREATE TABLE jobs(id INTEGER PRIMARY KEY, due_at INTEGER, kind TEXT, payload BLOB, attempts INTEGER);
CREATE INDEX jobs_due ON jobs(due_at);
```
After any insert or delete, `setAlarm((SELECT min(due_at) FROM jobs))`. In `alarm()`, in one
`transactionSync`, take the jobs with `due_at ≤ now`, run them, then reschedule the recurring ones and retry the
failed ones with jittered backoff (§6). Re-arm before returning, and catch errors so Cloudflare's 6 automatic
retries aren't used up during an outage. Each insert costs 2 rows written (row + index). Store-and-forward
bodies: 2 MB row cap, 5 GB total. **Opt-in only**: it breaks the "relay stores nothing but OAuth grants" promise,
so the README must say so. Only for async callers (webhooks), never for a bot waiting on a reply.

### 4b. Pipelines: DAG check plus a hop limit

**Feature.** "When app A produces X, call app B" links, without infinite ping-pong between apps or bots.

**Technique.** Kahn's topological sort (cp-algorithms) at link-creation time, O(V+E): reject a link that creates a
cycle and run each pipeline in topological order. Static checks can't see loops that go *through* a bot (a bot calls
tool A, A's output prompts the bot to call B, B triggers A), so also add a runtime hop budget like HTTP's
`Max-Forwards` (RFC 9110 §7.6.2) and IP TTL.

**Sketch.** The agent attaches `TopRelay-Hops: n` (default 8) and a trace id to every call it makes on a
pipeline's behalf. It decrements on each relay hop and refuses at 0 with a clear error. The trace id goes in the
call log so loops are debuggable.

### 4c. Field mapping between apps: schema check plus Hungarian assignment

**Feature.** Linking A's output to B's input. MCP tools may declare `outputSchema` (with `structuredContent`), so
TopRelay can check compatibility before running.

**Technique.** First a structural check: every `required` field of B's `inputSchema` must be produced, with
compatible types, recursing into objects. Then propose a one-to-one field mapping by maximum-weight bipartite
matching (Hungarian algorithm, Kuhn 1955; cp-algorithms), with weights from name-token similarity plus type match.
O(n³) with n under 50 fields takes microseconds.

**Sketch.** Show the proposal and let the owner or an LLM confirm. Hungarian guarantees a consistent one-to-one
assignment; similarity scores only rank candidates. Medium value: build this when pipelines exist.

---

## 5. Tool discovery

### 5a. BM25 over tool metadata (top 8, #5)

**Feature.** `toprelay add` across many local apps, then expose **one aggregated endpoint**
(`/all/mcp`) whose `tools/list` returns a small set of always-on tools plus `search_tools(query)` (and
`call_tool(name, args)` for clients that won't re-list). Bots without native tool search (ChatGPT, Grok, generic
clients) get the same benefit Anthropic's tool search gives Claude.

**Why BM25.** Anthropic's own tool search tool ships two variants, regex and BM25, searching tool names,
descriptions, argument names and argument descriptions, returning 5 tools by default. Anthropic's docs say tool
selection accuracy "degrades once you exceed 30–50 available tools". BM25 (Robertson & Zaragoza 2009) needs no
model and no training, and for 10³ tools the index is tiny.

**Sketch (agent, Go, in memory).**
- Documents = tools from every service, namespaced `svc.tool` (MCP allows `.` in tool names).
- Fields with weights: name ×3 (split on `_`, `-`, `.` and camelCase), title ×2, description ×1, parameter
  names and descriptions ×1. Standard `k1 = 1.2`, `b = 0.75`.
- Inverted index `map[term][]posting{doc, tf}`; query cost is the sum of posting-list lengths. Microseconds.
- Rebuild on `notifications/tools/list_changed`. Only tools that pass the allow-list and the caller's token caveats
  are indexed or returned.
- Alternative: FTS5 with `bm25(t, 3.0, 2.0, 1.0)` if the agent already embeds SQLite. Both DO SQLite and SQLite
  support it.
- **For Claude API users,** say in the docs that the MCP connector's `defer_loading` on `mcp_toolset` already does
  this server-side. TopRelay's search is for everyone else.

**Embeddings, if ever.** Brute-force cosine over 1,000 tools × 384 dimensions is ~4×10⁵ multiply-adds, well under
1 ms. HNSW (Malkov & Yashunin) only pays off far above 10⁵ vectors. Anthropic's custom tool search returns
`tool_reference` blocks, so an embedding search can plug in later without changing the endpoint.

---

## 6. Audit, logs and diagnostics

### 6a. Merkle tree log with anchored checkpoints (top 8, #7)

**Feature.** `toprelay audit verify` and `toprelay audit prove <call>`: a call log nobody can quietly edit,
truncate or reorder, and a portable proof that "bot X called `delete_note` at 14:02" (or didn't) for a teammate or
for incident review.

**Technique.** Crosby & Wallach's tree-based tamper-evident log (USENIX Security 2009), refined in Certificate
Transparency (RFC 6962/9162). It gives O(log n) proofs that a record is in the log and that a later log extends an
earlier one. A plain hash chain makes auditing O(n). **`golang.org/x/mod/sumdb/tlog`** implements it
(`StoredHashes`, `TreeHash`, `ProveRecord`/`CheckRecord`, `ProveTree`/`CheckTree`, tiles), so there's nothing to
write from scratch.

**Sketch.**
- Leaf = canonical encoding of the existing call-log record (time, service, method, tool, status, duration, token
  id). Still no bodies, no credentials.
- Every 5 minutes or 1,000 records, sign `(size, root)` with the device's Ed25519 key and send the checkpoint to
  the DO: 1 row written per checkpoint.
- Tampering with local history after a checkpoint can't produce a valid consistency proof against the cloud copy.
  This doesn't stop a compromised laptop from lying *before* the checkpoint. The DO can add a second witness by
  keeping its own leaf hash of what it saw (id, time, status), cheap with `crypto.subtle.digest`.

### 6b. Secret redaction with Aho-Corasick

**Feature.** "Redact secrets in every log" enforced mechanically, including in `doctor` output and bug-report
bundles.

**Technique.** Aho-Corasick (1975; cp-algorithms) matches all known secret strings in one pass, O(text + matches),
whatever the number of patterns. The agent *knows* its exact secrets: agent keys, bearer tokens, upstream
credentials, OAuth tokens it saw in headers. Add their base64 and URL-encoded forms as extra patterns.
`github.com/cloudflare/ahocorasick` exists. Rebuild the automaton on `rotate`. Generic patterns
(`sk-…`, JWT shapes) are a separate regex pass.

### 6c. Latency percentiles: exact first, DDSketch if needed

For `status` p50/p99 per service: the local log holds at most ~100 k calls/day, so exact percentiles from SQLite or
a sorted slice are fine. Use DDSketch (Masson, Rim & Lee, PVLDB 2019: relative-error and fully mergeable; Go:
`DataDog/sketches-go`) only for per-minute rollups or for sending latency stats to the DO for a dashboard. Choose
it over t-digest because guarantees on relative error suit long-tailed latencies.

---

## 7. Device linking and sync

### 7a. Noise sessions after PAKE or SAS pairing (top 8, #8)

**Feature.** Links where both ends run TopRelay code: laptop ↔ laptop, iPhone (TopRelayKit) ↔ laptop, app on
machine A ↔ app on machine B. The relay forwards ciphertext and **cannot read the traffic**, a stronger promise
than the TLS-terminating bot path. Scope this honestly: cloud bots speak HTTPS to the Worker, so bot traffic can't
be end-to-end encrypted this way.

**Technique.** Noise Protocol Framework (rev. 34). After pairing, both sides know each other's static keys: use
`Noise_KK_25519_ChaChaPoly_SHA256` (1 round trip, mutual authentication, forward secrecy). Go: `flynn/noise`.
Swift: built on CryptoKit primitives. Bind application auth to the handshake hash `h` (Noise §11.2) to rule out
session splicing.

**Pairing.** A relay in the middle can swap public keys during first contact, so pairing must authenticate with a
short code. Two good options:
- **Code typed on one device → CPace** (CFRG's balanced PAKE, `draft-irtf-cfrg-cpace`): an attacker gets one
  online guess per run. With a 6-digit code and the GCRA limiter from §2a on attempts, that's 10⁻⁶ per try.
  Prefer it to SPAKE2: RFC 9382 says SPAKE2 "was not selected" in the CFRG PAKE competition. Caveat: Go CPace
  libraries are immature (`filippo.io/cpace` is experimental and follows a 2020 draft), so budget for review.
- **Both screens visible → short authentication string** (Vaudenay, CRYPTO 2005): run Noise XX, both devices show
  6 digits derived from `h`, the user confirms they match. Use a commit-before-reveal step (as Bluetooth numeric
  comparison does), or a man-in-the-middle can grind ephemeral keys for a colliding string.
- **Don't** use the short code as a Noise `psk`: a passive relay could brute-force a 6-digit PSK offline from one
  recorded handshake.

### 7b. Config sync across the owner's devices: HLC plus a last-writer-wins map

**Feature.** Two laptops (or laptop and phone) share services, allow-lists, links and schedules. Edits made offline
merge without conflicts.

**Technique.** A state-based CRDT (Shapiro et al. 2011) of the simplest kind: a map of LWW registers, keyed by
config path, with tombstones for deletes. Timestamps come from hybrid logical clocks (Kulkarni, Demirbas et al.
2014): close to wall-clock time, still causally ordered when clocks skew.
```
send/local:  l' = max(l, pt); c = (l' == l) ? c+1 : 0; l = l'
receive m:   l' = max(l, m.l, pt); c = per HLC rules; l = l'
pack:        48-bit ms | 16-bit counter → uint64; tiebreak by device id
```
Merge = per-key max of `(hlc, device)`. The DO keeps a copy (a few hundred rows). Devices sync with "changes since
HLC watermark w", which is simple because the DO is a central point. Full sequence CRDTs (Automerge, Yjs/YATA) are
only worth it if TopRelay adds shared documents (a scratchpad shared by a bot and an app). Not now.

### 7c. Merging sets without a central log: range-based set reconciliation

**Feature.** Merging call logs (or any append-only record set) from several devices into one audit view, or
rebuilding a DO's copy after it lost state, when there's no single ordered changelog.

**Technique.** Range-based set reconciliation (Meyer, 2022/23): recursively split the key range and compare
fingerprints, so communication grows with the *difference*, not the set size. **Negentropy** implements it (C++, JS,
Rust, Go and more; used for Nostr sync, NIP-77), with an optional frame-size limit that fits WebSocket message
caps. Keys = `(hlc, record-hash)`. The Merkle log in §6a and a Merkle search tree (Auvolat & Taïani, SRDS 2019)
are alternatives. Negentropy is the most practical: it exists in Go and is protocol-specified. Lower priority:
build it when multi-device audit is a real request.

---

## 8. Reconnect: exponential backoff with full jitter

Small, but it decides how the relay behaves after a Cloudflare deploy drops every socket at once.
`sleep = random(0, min(cap, base · 2^attempt))` (Brooker, AWS Architecture Blog 2015) spreads the reconnect
storm. Use `base = 250 ms`, `cap = 30 s`. Reset on a successful connection, and skip the backoff after a wake or
network-change event, as the brief already says. One function in the agent. Reuse it for job retries in §4a.

---

## Skipped (considered and dropped)

| Candidate | Why not |
|---|---|
| HyperLogLog for unique callers | A single owner has a handful of OAuth clients and tokens. An exact set is smaller than the sketch. |
| Count-min sketch for per-caller budgets | Exact counters are cheap with write-behind (§2b). Overcounting from collisions would mean wrongly blocking calls. |
| Radix tree for URL routing | The Worker routes on the first path segment: one split and a map lookup. Path allow-lists for raw HTTP are dozens of prefixes. A sorted list or linear scan is enough. |
| Tries for tool names | A map plus BM25 covers lookup and search. Prefix queries (`github_*`) are a sorted slice and binary search. |
| MinHash/LSH | Near-duplicate tool detection across 10³ tools is 10⁶ pairwise Jaccard comparisons, done once, which is trivial. LSH pays off at millions of items. |
| HNSW | Brute force under ~10⁵ vectors (§5a). |
| Timer wheels (Varghese & Lauck 1987) | Go's runtime timers are heap-based and handle thousands of timers. The DO has one alarm, and per-request timeouts are `Promise.race`. Timer wheels matter at millions of timers. |
| Consistent, rendezvous or jump hashing | `idFromName(service)` already maps a service to its DO. Sharding one service across several DOs would only matter far beyond 100 k requests/day. Revisit if a hot service needs N DOs: jump hash (Lamping & Veach 2014) needs no state. |
| Invertible Bloom lookup tables | Range-based reconciliation (§7c) has maintained implementations and no tuning for difference size. |
| BDDs, interval trees for policy | Policy sets are tens of rules. Time-window rules are two comparisons. |
| Fenwick or segment trees for usage stats | An indexed SQLite time column plus a minute ring buffer answer every range query `status` needs. |
| t-digest | DDSketch fits latency better (§6c), and exact percentiles fit even better at this scale. |
| Union-find for entity resolution across apps | Real (same contact in CRM and notes), but it's a product of its own, with no near-term TopRelay feature behind it. |

---

## Build order

1. **Before v1 ships (protocol decisions that are expensive to change later):** reserve binary framing, per-stream
   sequence numbers and credit frames in the wire protocol (§1a–1c), and DO-generated incarnation-scoped request
   ids (§1d). Implement GCRA in the DO plus the pairing-attempt limiter (§2a), and full-jitter reconnects (§8).
2. **v1 or v1.0.x:** make bearer keys macaroons (§3a), replacing per-key secret bindings, with tool caveats
   enforced in the agent. Add write-behind usage counters and the free-tier meter in `status` (§2b). Turn on the
   Aho-Corasick redactor (§6b).
3. **v1.1:** resume-not-retry outcome table (§1d), DRR plus coalescing (§1b, 1c) once streaming load is measured.
4. **v1.2, "one URL for all my apps":** aggregated endpoint with BM25 `search_tools` (§5a).
5. **v1.3, trust:** Merkle call log with DO checkpoints (§6a).
6. **v2, "link app to app":** alarm-backed job queue and opt-in store-and-forward webhooks (§4a), DAG check and
   hop limit (§4b), schema check and Hungarian field mapping (§4c).
7. **v2, "link my devices":** CPace/SAS pairing and Noise KK sessions (§7a), HLC + LWW config sync (§7b).
8. **On demand:** range-based reconciliation for multi-device logs (§7c), DDSketch rollups (§6c), Biscuit for
   delegation to third parties (§3a).

---

## Sources

Cloudflare (checked 2026-10-31)
- Durable Objects limits: https://developers.cloudflare.com/durable-objects/platform/limits/
- Durable Objects pricing (Free plan rows read/written): https://developers.cloudflare.com/durable-objects/platform/pricing/
- SQLite storage API (rows billing, index writes, FTS5/JSON, PITR, `transactionSync`): https://developers.cloudflare.com/durable-objects/api/sqlite-storage-api/
- Alarms (one per object, retries): https://developers.cloudflare.com/durable-objects/api/alarms/
- WebSockets and hibernation (`serializeAttachment` 16 KiB): https://developers.cloudflare.com/durable-objects/best-practices/websockets/
- Workers limits (10 ms CPU Free, 128 MB): https://developers.cloudflare.com/workers/platform/limits/
- KV limits (1,000 writes/day Free): https://developers.cloudflare.com/kv/platform/limits/
- Rate Limiting binding: https://developers.cloudflare.com/workers/runtime-apis/bindings/rate-limit/
- Web Crypto in Workers (`timingSafeEqual`, Ed25519, X25519, HMAC): https://developers.cloudflare.com/workers/runtime-apis/web-crypto/

MCP and Anthropic
- MCP 2025-11-25 tools (outputSchema, structuredContent, list_changed): https://modelcontextprotocol.io/specification/2025-11-25/server/tools
- MCP 2025-11-25 schema, `ToolAnnotations` (`idempotentHint`, "hints"): https://modelcontextprotocol.io/specification/2025-11-25/schema
- Anthropic tool search tool (regex/BM25 variants, `defer_loading`, custom `tool_reference`): https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool

Streams and scheduling
- RFC 9113, HTTP/2 (§5.2 flow control): https://www.rfc-editor.org/rfc/rfc9113
- RFC 896, Nagle, "Congestion Control in IP/TCP Internetworks": https://www.rfc-editor.org/rfc/rfc896
- Shreedhar & Varghese, "Efficient Fair Queueing using Deficit Round Robin", SIGCOMM 1995, doi:10.1145/217391.217453: https://courses.cs.duke.edu/cps214/spring09/papers/drr.pdf
- RFC 9110, HTTP Semantics (§7.6.2 Max-Forwards): https://www.rfc-editor.org/rfc/rfc9110
- Varghese & Lauck, "Hashed and Hierarchical Timing Wheels", SOSP 1987: http://www.cs.columbia.edu/~nahum/w6998/papers/sosp87-timing-wheels.pdf
- Brooker, "Exponential Backoff And Jitter", AWS Architecture Blog, 2015: https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/

Rate limiting and sketches
- GCRA explained (Brandur Leach): https://brandur.org/rate-limiting
- Go token bucket, `golang.org/x/time/rate`: https://pkg.go.dev/golang.org/x/time/rate
- Masson, Rim & Lee, "DDSketch", PVLDB 12(12), 2019: https://arxiv.org/abs/1908.10693 ; Go: https://github.com/DataDog/sketches-go

Capabilities
- Birgisson et al., "Macaroons: Cookies with Contextual Caveats for Decentralized Authorization in the Cloud", NDSS 2014: https://research.google/pubs/macaroons-cookies-with-contextual-caveats-for-decentralized-authorization-in-the-cloud/
- Biscuit specification: https://github.com/eclipse-biscuit/biscuit/blob/main/SPECIFICATIONS.md

Search
- SQLite FTS5 (`bm25()`, trigram, prefix indexes): https://www.sqlite.org/fts5.html
- Robertson & Zaragoza, "The Probabilistic Relevance Framework: BM25 and Beyond", Foundations and Trends in IR 3(4), 2009
- Malkov & Yashunin, HNSW: https://arxiv.org/abs/1603.09320

Logs and audit
- Crosby & Wallach, "Efficient Data Structures for Tamper-Evident Logging", USENIX Security 2009: https://www.usenix.org/conference/usenixsecurity09/technical-sessions/presentation/efficient-data-structures-tamper-evident
- RFC 9162, Certificate Transparency 2.0 (RFC 6962 successor): https://www.rfc-editor.org/rfc/rfc9162
- Go `golang.org/x/mod/sumdb/tlog`: https://pkg.go.dev/golang.org/x/mod/sumdb/tlog
- Aho & Corasick, "Efficient string matching", CACM 18(6), 1975; https://cp-algorithms.com/string/aho_corasick.html ; Go: https://github.com/cloudflare/ahocorasick

Graphs and matching (cp-algorithms)
- Topological sort (Kahn/DFS): https://cp-algorithms.com/graph/topological-sort.html
- Cycle detection: https://cp-algorithms.com/graph/finding-cycle.html
- Hungarian algorithm (Kuhn 1955): https://cp-algorithms.com/graph/hungarian-algorithm.html
- Disjoint set union: https://cp-algorithms.com/data_structures/disjoint_set_union.html
- Fenwick tree: https://cp-algorithms.com/data_structures/fenwick.html

Secure channels and pairing
- Noise Protocol Framework, rev. 34 (2018-07-11): https://noiseprotocol.org/noise.html ; Go: https://github.com/flynn/noise
- CPace, draft-irtf-cfrg-cpace: https://datatracker.ietf.org/doc/draft-irtf-cfrg-cpace/ ; Go (experimental): https://pkg.go.dev/filippo.io/cpace
- RFC 9382, SPAKE2 (2023): https://www.rfc-editor.org/rfc/rfc9382.html
- Vaudenay, "Secure Communications over Insecure Channels Based on Short Authenticated Strings", CRYPTO 2005
- Go `crypto/mlkem` (post-quantum KEM, if Noise sessions later need a hybrid): https://pkg.go.dev/crypto/mlkem

Sync
- Kulkarni, Demirbas et al., "Logical Physical Clocks" (hybrid logical clocks), 2014: https://www.cse.buffalo.edu/~demirbas/publications/hlc.pdf
- Shapiro, Preguiça, Baquero, Zawirski, "Conflict-free Replicated Data Types", SSS 2011
- Meyer, "Range-Based Set Reconciliation", 2022/23: https://arxiv.org/abs/2212.13567 ; Negentropy: https://github.com/hoytech/negentropy
- Auvolat & Taïani, "Merkle Search Trees", SRDS 2019, doi:10.1109/SRDS.2019.00032
- Nicolaescu et al., YATA/Yjs, GROUP 2016, doi:10.1145/2957276.2957310
- Lamping & Veach, "A Fast, Minimal Memory, Consistent Hash Algorithm", 2014: https://arxiv.org/abs/1406.2294
- Goodrich & Mitzenmacher, "Invertible Bloom Lookup Tables", 2011: https://arxiv.org/abs/1101.2245
