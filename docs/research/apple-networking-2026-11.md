# Apple networking and IPC for TopRelay, November 2026

Research date: 2026-11-14. It asks which Apple frameworks and open-source libraries TopRelay can build on so that a
Mac app with a TopRelay integration talks to the agent without friction, and is reachable by cloud bots. Primary
sources only: developer.apple.com documentation (read through its JSON data endpoints), WWDC session pages, the macOS
26.6 SDK headers and man pages on the build Mac, Apple's GitHub repos, the MCP spec, and the official SDK repos. Each
claim has a source number; see the list at the end. Anything marked **[unverified]** comes from a secondary source
or couldn't be confirmed on a primary page. "Observed" means checked by hand on this Mac (macOS 26.6.2, Xcode SDK
of Nov 2025).

This note doesn't repeat the bot and vendor side (`agent-ecosystem-2026-10.md`), tunnels and tsnet
(`tailscale-and-tunnels-2026-10.md`), Noise/CPace pairing (`algorithms-for-linking-2026-10.md` §7a), or the product
directions (`directions-2026-10.md`). It covers only the Mac side: how an app and the agent find each other, talk,
and prove who they are.

## Summary

**Apple has not shipped MCP for third-party apps.** In September 2025, 9to5Mac reported "very incipient" MCP code
tied to App Intents in the 26.1 betas [27] **[unverified]**. Since then, nothing on Apple's pages makes App Intents
reachable from outside agents:
- In WWDC26 material, MCP appears only in Xcode, for coding agents [23].
- App Intents still feed only Siri, Spotlight, Shortcuts, widgets and the Action button [22][23][24][26].
- The macOS 27 guide doesn't mention MCP [24].

A third-party claim of "system-wide MCP in macOS 27" isn't backed by any Apple page I could find. TopRelay's bridge
role is still open.

Ranked by user value against effort:

| # | Feature | Value | Effort | Verdict |
|---|---|---|---|---|
| 1 | **TopRelayKit `MCPServer`**: a drop-in, spec-current MCP server that listens on a unix socket in the app's container and serves tools declared in Swift (including Foundation Models `Tool`s) | High: removes the hand-written HTTP parser, the TCP port, the token file and the caller check that every app writes today (ezpdf has ~1,650 lines of this) | M | **Build first** |
| 2 | **Code-signature identity on every transport**, done in-process by audit token instead of `lsof` + `codesign` on a PID | High (security), invisible to users | S–M | **Build** |
| 3 | **Discovery by manifest file, not Bonjour**: apps drop a small JSON file into a known directory; the agent watches it and offers "ezpdf wants to be reachable" | High: zero-config | S | **Build**; Bonjour rejected (below) |
| 4 | **Bundled agent through `SMAppService`**: the app ships `toprelay` as a login-item LaunchAgent, with no separate install | High for normal users | M (signing, updates) | **Build**; TopRelayKit README already describes it |
| 5 | **Wake on call**: launchd socket activation, or `open -g`, so that a bot call starts the app or its helper instead of failing | Medium–High | S–M | **Build after 1** |
| 6 | **App Intents bridge for apps without MCP**, through `shortcuts run` | Medium: few third-party apps ship intents | M, brittle | **Opt-in beta**, built on an existing MIT project's approach |
| 7 | **Apple Events (sdef) bridge** for scriptable apps | Medium for power users | M | **Later**, curated tools only |
| 8 | **HPKE end-to-end encryption** | Low for bots (the Worker must end TLS); Medium for an iPhone companion | M | **Only for the device-link path** |

Rejected or deferred: XPC as the agent↔app transport (the Go agent has no XPC without cgo; it fits only an app
that bundles its own agent), Bonjour discovery (prompts, carries no identity, not needed on one Mac), TLS-PSK on
loopback (Go `crypto/tls` has no PSK [63]), Network Extension (overkill), Wi-Fi Aware (not on macOS [43]).

Surprising findings:
- `NWParameters.acceptLocalOnly` means **local link, not loopback** [5].
- Apple's own Foundation Models utilities package encodes a `GenerationSchema` straight into a JSON-Schema tool
  `parameters` field [35]. A Foundation Models `Tool` is therefore one encode away from an MCP tool.
- **launchd agents don't get the automatic Local Network exemption that daemons and Terminal tools get** [8].
- `XPCPeerRequirement` (macOS 26) and `LightweightCodeRequirements` (macOS 14.4) give a typed, audit-token-based
  peer check [13][14].
- The official MCP Swift SDK (0.12.1, May 2026) **still targets 2025-11-25**. Stateless 2026-07-28 is an open
  issue [54]. The Go SDK has negotiated 2026-07-28 since v1.7.0 [55].

## Pain points seen in a real integration (ezpdf)

`ezpdf/Packages/EzPDFCore/Sources/EzPDFCore/MCP/` (observed):
- `LocalServer.swift` (424 lines) hand-writes an HTTP/1.1 parser on `NWListener` bound to `127.0.0.1:7337`. It
  refuses chunked bodies, checks `Host` against DNS rebinding, limits connections, and retries 20 times when the
  port is still held after a relaunch.
- A `mcp-token` file is created and rotated by hand.
- `isRemote` is guessed from `X-Forwarded-For` / `CF-Connecting-IP` / `X-Toprelay-Client`, the weak signals that
  MIGRATION.md §2 says to replace with `X-TopRelay-Caller`.
- `MCPHandler.swift` supports versions up to `2025-11-25` and expects `initialize`. The agent's proxy forwards
  bodies as the bot sent them, so a 2026-07-28 client (stateless, no `initialize` [53]) reaches the app unchanged.
- The same four operations are defined twice: as `AppIntent` structs in `App/Intents/Intents.swift` and as MCP tools
  in `MCPTools.swift` (655 lines).

Every TopRelay-integrated app will repeat this. Feature 1 is a library that does it once.

## Per framework and library

### Network.framework, classic API (`NWListener`, `NWConnection`, `NWBrowser`)

- **What.** Available since macOS 10.14:
  - `NWEndpoint.unix(path:)`, a UNIX-domain endpoint [4]
  - `NWEndpoint.service(name:type:domain:interface:)`, a Bonjour endpoint [4]
  - `NWParameters.requiredLocalEndpoint`, which pins a listener to an address and port [64]
  - `NWParameters.acceptLocalOnly`, documented as "restricts listeners to only accepting connections from the local
    link" [5]. That means the LAN link, not loopback.
  - WebSocket as a protocol in the stack (`NWProtocolWebSocket`, macOS 10.15) [6]
  - TLS pre-shared keys through `sec_protocol_options_add_pre_shared_key` [64]
- **`NWListener.init(launchd:using:)` and `init(launchdSocketKey:parameters:)`** exist (macOS 13). Their pages have
  no description text, but the names match launchd socket activation [7][21].
- **How TopRelay uses it.** A TopRelayKit server listens on `.unix(path:)` inside the app's container or app-group
  container. Then no TCP port can be squatted, no port number has to be chosen, and the agent's existing
  `LOCAL_PEERCRED` check applies (agent/README "Port squatting").
- **Risks.**
  - Don't use `acceptLocalOnly` as a loopback guard. Bind to `127.0.0.1`, or better, use a unix socket.
  - The launchd initializers have no documentation text, so test them before relying on them.

### Network.framework, Swift-concurrency API (`NetworkConnection`, `NetworkListener`, `NetworkBrowser`)

- **What.** New in macOS 26 / iOS 26 (WWDC25 session 250) [1][2][3]:
  - Protocol stacks are declared with result builders: `TLS { TCP { IP() } }`, plus `TLV {}` and
    `Coder(T.self, using: .json)` framers [1].
  - `NetworkListener(...).run { connection in … }` starts a child task per connection [1].
  - `NetworkConnection` has QUIC streams (`openStream`, `inboundStreams`), datagrams, and `negotiatedALPN` [2].
  - `NetworkListener` has `init(for:using:)`, which "advertises a service", plus `newConnectionLimit` and
    `onServiceRegistrationUpdate` [3].
- **WWDC26** had no Network framework session. The System Services list shows none [65], and the State of the Union
  mentions only WebKit's QUIC and SwiftNIO [23].
- **How TopRelay uses it.** It's a natural base for TopRelayKit's server loop on macOS 26+. The Kit targets
  macOS 13+ today, so it needs `@available` gating or a classic `NWListener` fallback.
- **Risks.**
  - The talk doesn't show a unix-socket example [1]. The pages list `init(to:using:)` overloads that take an
    endpoint [2], and `NWEndpoint.unix` is an endpoint [4], but **I couldn't confirm `NetworkListener` binding a
    unix path**. Test it.
  - There's no HTTP layer. Either way, TopRelayKit needs an HTTP/1.1 parser (its own, or SwiftNIO's).

### Bonjour / DNS-SD

- **What.**
  - On macOS 15+, every Bonjour operation (register, browse, resolve) needs the Local Network privilege [8].
  - The privilege is granted automatically to launchd **daemons**, root, and command-line tools run from Terminal
    or SSH. It is **not** granted to launchd **agents** [8].
  - An app must list its service types in `NSBonjourServices` [8][10].
  - `dns_sd.h` has `kDNSServiceInterfaceIndexLocalOnly`. A service registered with it "will be found *only* by other
    local clients on the same machine" [9].
- **How TopRelay could use it.** Apps advertise `_mcp._tcp` (or a TopRelay type) with `LocalOnly`, and the agent
  browses for them.
- **Verdict: reject for the Mac itself.**
  1. `toprelay up --install` is a LaunchAgent, so browsing would raise a Local Network prompt for the agent (and
     for each app that registers). TN3179 doesn't say whether `LocalOnly` registrations are exempt
     (**open question**).
  2. A Bonjour record carries no identity. Any process can advertise "ezpdf", so the agent would still need the
     code-signature check.
  3. Everything is on one Mac, so a file in a known directory does the same job with no prompt (Feature 3).

  Keep Bonjour in mind for a LAN or home-server mode later, where it's the right tool.

### XPC (`XPCListener`, `XPCSession`, peer requirements) and launchd Mach services

- **What.**
  - The Swift `XPCListener` and `XPCSession` arrived in macOS 14 [11][12]. They can take a peer requirement at
    creation (`init(service:…requirement:…)`) or later (`setPeerRequirement(_:)`) [11][12].
  - `XPCPeerRequirement` (macOS 26) has `isFromSameTeam(andMatchesSigningIdentifier:)`, `hasEntitlement`,
    `entitlement(_:matches:)`, `isPlatformCode`, and `codeRequirement(_:)`, which takes a `ProcessCodeRequirement` [13].
  - Older C equivalents: `xpc_connection_set_peer_code_signing_requirement` (macOS 12) and
    `xpc_connection_set_peer_lightweight_code_requirement` [15].
  - `XPCSession(machService:)` reaches a launch agent or daemon by name [12][16]. The name comes from the
    `MachServices` key in launchd.plist [20].
  - A sandboxed app can reach a Mach or XPC service named `<app group id>.<name>`, including one in a non-sandboxed
    process of the same group [17].
- **Could TopRelay offer an XPC transport?** Only in one shape.
  - The Go agent can't speak XPC without cgo or a dynamic-call shim, and it has no cgo today (observed: no
    `import "C"` in `agent/`). purego [62] can call C functions without cgo, but libxpc is block- and
    dispatch-based, so that would be fragile.
  - An app-group Mach name works only when the agent is signed into the app's group, which means it's the app's
    bundled copy (Feature 4).
  - XPC carries messages, not an HTTP byte stream, so MCP's Streamable HTTP would need re-framing.
- **Verdict.** Use unix sockets, which give the same benefits (no port, a kernel peer credential, the sandbox
  app-group rule also covers them [17]). XPC is worth it only for a future Swift-only helper.

### SMAppService and launchd

- **What.**
  - `SMAppService.agent(plistName:)`, `.daemon(plistName:)`, `.loginItem(identifier:)`, `register()`,
    `unregister()` and `status` (macOS 13) [18].
  - The plist lives inside the signed app bundle, and users see the providing app under Login Items [19].
  - For a launchd agent installed through `SMAppService`, macOS already knows the "responsible code" for Local
    Network prompts. A legacy plist needs `AssociatedBundleIdentifiers` instead [8][20].
  - launchd `Sockets` with `SockPathName` and `SockPathMode` give launch-on-demand unix sockets, which the job
    collects with `launch_activate_socket(3)` [20][21].
- **How TopRelay uses it.**
  - Feature 4: bundle the agent.
  - Feature 5: a helper LaunchAgent that launchd starts when the agent connects to its socket.
- **Risks.**
  - The user has to approve the item in Login Items. `status` reports `.requiresApproval` [18].
  - A bundled agent is signed by the app's team, so two apps could each bundle a different `toprelay` version.
    Only one `up` may run per config dir (agent/README).
  - Whether a Mac App Store app may ship a non-sandboxed `SMAppService` agent is **[unverified]**. A sandboxed agent
    couldn't read `~/.config/toprelay`.

### App Intents, App Shortcuts, Shortcuts

- **What.**
  - App Intents make "content and actions discoverable by Apple Intelligence and support system experiences like
    Siri, Spotlight, Shortcuts, and widgets" (macOS 13+) [22].
  - WWDC26 added intent and entity schemas for Siri, View Annotations, an App Intents Testing framework,
    `LongRunningIntent`, `CancellableIntent`, and `ExecutionTargets` (which process runs an intent) [24][26].
  - WWDC26 "What's new in Shortcuts" added triggers, a Use Model action and Storage. It **had no new way for
    another process or agent to run an intent** [25].
- **MCP status.**
  - A 26.1-beta code finding was reported by 9to5Mac [27] **[unverified]**.
  - A December 2025 developer-forums question got no Apple answer [28].
  - WWDC26 mentions MCP only for Xcode [23].
  - **Conclusion:** there's no public MCP surface for App Intents as of 2026-11-14.
- **Ways to bridge them today.**
  1. **Outside the app.**
     - `shortcuts run <name|id> --input-path … --output-path …` [30].
     - Intent metadata is in `<App>.app/Contents/Resources/Metadata.appintents/extract.actionsdata`, a JSON file
       Xcode generates at build time with `actions`, `parameters`, titles, descriptions and types (observed; ezpdf
       ships 4 actions) [31]. Its format is **undocumented**.
     - VladUZH/intents-mcp (MIT) already does this. It writes a Shortcut per action, signs it with
       `shortcuts sign --mode people-who-know-me`, and runs it. It needs one "Add Shortcut" click per wrapper. It
       doesn't support entity actions (657 of 945 actions on the author's Mac). Only 8 of 50 third-party apps on
       the author's Mac shipped intents [29].
  2. **Inside the app (better).** An intent is a Swift struct with `perform()`, so the app can run its own intents
     directly. What it lacks is a documented runtime way to turn `@Parameter`s into a JSON Schema (**not found**).
- **Verdict.**
  - Inside the app: TopRelayKit takes a schema from a `@Generable` argument type (next section), not by reflecting
    on intents.
  - Outside the app: an opt-in `toprelay add --intents <bundle id>` beta that follows intents-mcp's approach.

### Foundation Models: `Tool`, `GenerationSchema`, `LanguageModel`

- **What.**
  - `Tool` (macOS 26) has `name`, `description`, `parameters` (a `GenerationSchema`), and
    `call(arguments:)` [32].
  - `GenerationSchema` is `Encodable` and `Decodable` [33].
  - `GeneratedContent` has `init(json:)` and `jsonString` [33].
  - Apple's `foundation-models-utilities` (Apache-2.0) sends a tool's `parameters: GenerationSchema` directly as the
    JSON-Schema `parameters` of an OpenAI-style function definition [35]. That makes the encoded schema usable as
    an MCP `inputSchema`.
  - In macOS 27 the framework takes any model through a `LanguageModel` protocol (Claude, Gemini, any provider) [24][36].
  - WWDC26 "Build agentic app experiences" shows dynamic profiles, skills and sub-sessions, but **no MCP client** [34].
- **How TopRelay uses it.** It's the cheapest way to get a schema. An app that already wrote `Tool`s for Apple
  Intelligence passes them to `MCPServer(tools: [...])`, and TopRelayKit:
  - maps `name` and `description` to the MCP tool,
  - maps `try JSONEncoder().encode(tool.parameters)` to `inputSchema`,
  - calls `tool.call(arguments: GeneratedContent(json: argsJSON))` and returns the output as text or structured
    content.

  One definition then serves Siri/Apple Intelligence and every cloud bot.
- **Risks.**
  - macOS 26+ only.
  - Apple doesn't document the JSON form of `GenerationSchema`. It works as tool `parameters` in Apple's own
    client [35]. MCP 2026-07-28 accepts any JSON Schema 2020-12 [53]. Still, test it with each bot.
  - An app on macOS 13–25 needs a plain `Codable` + JSON-Schema path.

### Apple Events, OSA, Scripting Bridge

- **What.**
  - Scripting Bridge drives scriptable apps from their `sdef` [39].
  - `sdef <app>` prints the dictionary [40].
  - Sending Apple Events needs `NSAppleEventsUsageDescription` [37] and, under the hardened runtime, the
    `com.apple.security.automation.apple-events` entitlement. Neither is needed for events to yourself or to
    "processes signed with the same team ID" [38].
  - Apple's own wording on the risk: Mail "can be automated with Apple events, [so] other apps can use Mail to
    gain access to the data indirectly" [37].
- **How TopRelay uses it.** `toprelay add notes --applescript` with a **curated** tool list. Examples: "search
  Notes", "make a note", generated from the sdef's commands, read-only by default.
- **Risks.**
  - It's a broad capability. One Automation approval gives the agent everything the target app can script, and
    cloud bots would be steering it. The tool allow-list and pins are the only brake.
  - Which process TCC charges for a launchd agent sending events is **[unverified]**, and so is whether it prompts
    at all from the background (test it).
  - Generic sdef→MCP mapping makes bad tools (Notes' sdef lists 2 commands, observed). Hand-written adapters per
    app work better.

### Network Extension, Wi-Fi Aware, Multipeer Connectivity, Continuity

- `NERelayManager` (macOS 14) configures system relays for an app's traffic, and `NEAppProxyProvider` is a
  per-app proxy extension [41][42]. Both route **outbound** traffic. TopRelay only needs one outbound WebSocket,
  so **skip both**.
- Wi-Fi Aware is iOS/iPadOS 26 only, with **no macOS** [43]. It's no use for the Mac.
- Multipeer Connectivity runs on macOS 10.10+ but uses Bonjour, so it needs Local Network access and
  `NSBonjourServices` [8][44]. An iPhone companion should go through the relay, with the end-to-end encryption
  from `algorithms-for-linking-2026-10.md` §7a, rather than requiring the same Wi-Fi.
- Continuity: nothing found that a third party can use for this.

### CryptoKit, Secure Enclave, Keychain, App Attest

- `HPKE` (macOS 14) has `Sender` and `Recipient`, and suites including `Curve25519_SHA256_ChachaPoly` and the
  post-quantum `XWingMLKEM768X25519_SHA256_AES_GCM_256` [45].
- Go has `crypto/hpke` (RFC 9180) in the standard library [47], so the agent and the Kit can both implement it
  without third-party code.
- `SecureEnclave` has `P256`, `MLKEM768/1024` and `MLDSA65/87` [46].
- Keychain access groups share items only among apps "delivered by a single development team" [48]. TopRelay and a
  third-party app can't share a keychain item unless the app bundles the agent signed by its own team.
- `DCAppAttestService` (macOS 11) proves an app instance to **your server** [49]. A Worker could check it, but the
  agent already proves itself to the Worker, so it adds little.
- **For TopRelay:**
  - A Secure-Enclave P-256 key for the agent's identity to the relay (it can't be exported) is a good hardening
    step.
  - HPKE helps only for TopRelay-aware clients, such as an iPhone companion or the user's own scripts, where the
    Worker forwards ciphertext.
  - Cloud bots speak HTTPS to the Worker, so they **can't** use end-to-end encryption. This matches
    algorithms §7a.

### Code identity: audit tokens, `SecCode`, `LightweightCodeRequirements`, launch constraints

- **What.**
  - `sys/un.h` defines `LOCAL_PEERTOKEN`, which retrieves the peer's audit token, next to `LOCAL_PEERPID` [50].
  - `SecCode.h` has `kSecGuestAttributeAudit` (look up code by audit token) and `SecCodeCreateWithXPCMessage` [50].
  - `LightweightCodeRequirements` (macOS 14.4) has `ProcessCodeRequirement` with `TeamIdentifier`,
    `SigningIdentifier` and `ValidationCategory`, `SecCodeCheckValidityWithProcessRequirement`, and
    `SecTaskValidateForRequirement` [14].
  - Launch constraints can restrict who may start a binary (parent or responsible process), set in a launchd plist
    or at signing time. `Process.launchRequirement` (macOS 14.4) checks a child before it runs [51][14].
- **Why it matters.** The agent today does `lsof` → PID → `/usr/bin/codesign --verify -R=… <pid>` (observed:
  `agent/internal/upstream/pin.go`), and over a unix socket it uses `LOCAL_PEERPID`. A PID can be reused between
  the lookup and the check. An audit token includes a PID version, so it can't be.
  - **Go side:** read `LOCAL_PEERTOKEN` with `golang.org/x/sys/unix` (raw getsockopt), then call
    `SecCodeCopyGuestWithAttributes` + `SecCodeCheckValidity` through purego [62], which needs no cgo.
  - **Swift side** (TopRelayKit as the server, checking that the caller is the real agent): a
    `ProcessCodeRequirement` on the peer.
  - For TCP the agent still needs `lsof` to find the peer, so prefer unix sockets.
- **Container access.** On macOS 14+, opening a file in **another app's** sandbox container triggers a permission
  prompt (`NSAppDataUsageDescription`) [52]. Whether `connect()` to a socket in `~/Library/Containers/<app>/Data/`
  counts is **untested** (open question). An app-group container socket, or a socket the app creates under the
  agent's directory, may avoid the prompt.

### Open-source libraries

| Library | License | Latest (as of 2026-11-14) | Maturity | Use for TopRelay |
|---|---|---|---|---|
| modelcontextprotocol/swift-sdk [54] | MIT → Apache-2.0 transition (mixed) | 0.12.1 (2026-05-07); main active (2026-11-10) | Pre-1.0; targets **2025-11-25**; open issues for stateless SEP-2575 (#245) and MRTR (#238) | Types and `Server` for the drop-in server (`StatelessHTTPServerTransport` takes a request and returns a response, so it needs your own HTTP layer). It doesn't speak 2026-07-28 yet. Pulls in swift-nio, swift-log, eventsource |
| modelcontextprotocol/go-sdk [55] | MIT → Apache-2.0 transition | v1.8.0 (2026-09-14) | Stable; 2026-07-28 since v1.7.0 | Already in `agent/go.mod`. Use it for the `--intents` / `--applescript` bridges as in-process servers |
| apple/swift-nio [56] | Apache-2.0 | 2.105.0 (2026-11-03) | Mature | HTTP/1.1 codec (`NIOHTTP1`) for the Kit server if Network.framework's lack of HTTP hurts |
| apple/swift-nio-transport-services [57] | Apache-2.0 | 1.28.0 (2026-04-23) | Mature | NIO on Network.framework; listener bootstrap binds a unix-domain path (source has `unixDomainSocketPath`) |
| hummingbird-project/hummingbird [61] | Apache-2.0 | 2.28.1 (2026-10-31) | Mature | Full HTTP server with unix-socket bind. Heavier than needed |
| apple/swift-nio-ssh [58] | Apache-2.0 | 0.15.1 (2026-10-26) | Pre-1.0 | Not needed |
| apple/swift-crypto [59] | Apache-2.0 | 5.1.1 (2026-11-03) | Mature | CryptoKit API on Linux; not needed on macOS |
| apple/swift-certificates [60] | Apache-2.0 | 1.21.1 (2026-10-26) | Mature | Not needed: no X.509 on loopback |
| apple/foundation-models-utilities [35] | Apache-2.0 | tag 1.1.0 | "Emerging and experimental" | Reference for `GenerationSchema` → JSON Schema; `ChatCompletionsLanguageModel` |
| ebitengine/purego [62] | Apache-2.0 | v0.11.3 (2026-11-03) | Widely used | Call Security.framework from Go without cgo (audit-token code check) |
| VladUZH/intents-mcp [29] | MIT | pushed 2026-09-27 | Small, 7 stars | Shortcut-wrapping approach for the App Intents bridge |
| Go `crypto/hpke` [47] | BSD-3-Clause (Go) | stdlib | Stable | HPKE in the agent, compatible with CryptoKit |
| cloudflare/circl [66] | BSD-3-Clause | v1.6.5 (2026-08-05) | Mature | Only if X-Wing or ML-KEM HPKE is needed in Go before the stdlib has it |

## Feature proposals

### F1. TopRelayKit `MCPServer`: a drop-in, spec-current MCP server

- **User sees:** nothing new; the app's "Reach from anywhere" toggle just works. **Developer sees:**
  ```swift
  let server = try MCPServer(name: "ezpdf", tools: [ConvertTool(), ExtractTool()])   // FoundationModels.Tool or TopRelayTool
  try await server.start()                                   // unix socket in the container, 0600
  try await relay.register(service: "ezpdf", server: server) // registers unix:// URL + pin + caller key
  ```
  Each handler gets a `CallContext` with `isRemote`, `client` (from a **verified** `X-TopRelay-Caller`), and the
  tool's annotations.
- **APIs:**
  - `NWListener` on `NWEndpoint.unix(path:)` [4] (or `NetworkListener` on macOS 26 [3]) with a strict HTTP/1.1
    parser, which is ezpdf's `RequestReader` hardened and moved into the Kit.
  - The Swift verifier in MIGRATION.md §2.
  - Foundation Models `Tool` → MCP mapping [32][33][35].
  - Implements 2026-07-28 (`server/discover`, `resultType`, `ttlMs`/`cacheScope`, deterministic `tools/list`
    order) **and** 2025-11-25 `initialize`, because the agent forwards bodies as the bot sent them [53].
- **Changes.**
  - **Kit:** a new product `TopRelayMCP`, so `TopRelayKit` stays dependency-free. Write the JSON-RPC layer in the
    Kit rather than depend on swift-sdk until it ships 2026-07-28 [54].
  - **Agent:** none required. Optionally, `register` accepts `pinTeamID` automatically from the calling app's
    signature.
  - **App:** delete its server code.
- **Security.**
  - No TCP port, so no squatting.
  - Socket mode 0600 in a directory only the user can write (the agent already checks this).
  - The app checks the agent's code signature on accept (F2), so a local process with the socket path still can't
    pose as TopRelay.
  - The credential header becomes optional, since the socket plus signature does the same job.

### F2. App identity by code signature on every transport

- **User sees:** `toprelay list` shows "ezpdf, signed by Team ABCDE12345" with no flags. A different program
  answering on that socket is refused, with a `doctor` hint.
- **APIs:**
  - `LOCAL_PEERTOKEN` [50] → `SecCodeCopyGuestWithAttributes(kSecGuestAttributeAudit)` → `SecCodeCheckValidity`
    with a requirement string.
  - Or `SecCodeCheckValidityWithProcessRequirement` (macOS 14.4) [14].
  - In Swift, `ProcessCodeRequirement` [14] for the reverse check.
- **Changes.**
  - **Agent:** a `peer_darwin.go` path using purego [62]. Keep `codesign` as the fallback. Pin on `add` by
    default when the socket's peer is signed (trust on first use).
  - **Kit:** checks that the connecting agent is `toprelay`, matching TopRelay's team id or the app's own team if
    bundled.
  - **App:** none.
- **Security.** Removes the PID-reuse race and the `lsof`/`codesign` cost per connection. It proves who answered,
  not that the app is free of bugs (unchanged).

### F3. Discovery by manifest file (instead of Bonjour)

- **User sees:** opening a TopRelay-aware app for the first time makes the agent (menu bar or `toprelay status`)
  offer "ezpdf can be reached from anywhere. Allow?", with the tool list and read-only defaults.
- **Mechanism.**
  - The app (through the Kit) writes `~/.config/toprelay/apps/<bundle-id>.json` with `{name, socket,
    teamId, bundleId, tools_default}`, or into its app-group container if sandboxed.
  - The agent watches the directory (kqueue / FSEvents), checks the socket's peer signature against `teamId` (F2),
    then asks the user. **Nothing is exposed until the user says yes.**
- **Why not Bonjour.** It prompts, it carries no identity, and it's pointless on a single machine [8][9].
- **Changes.**
  - **Agent:** a watcher plus a `pending` list in state.json, and `toprelay accept <name>`.
  - **Kit:** `relay.announce(...)`, usable even when the agent isn't running.
  - **App:** one call.
- **Security.** A malicious local app could announce "ezpdf". The signature check against the claimed team, and the
  user's approval naming the signer, defeat that. A sandboxed app can't write to `~/.config`, so the agent must
  also scan known group containers. **Open question:** does reading another team's group container prompt on
  macOS 15+?

### F4. Bundle the agent with `SMAppService`

- **User sees:** no Terminal step. The toggle asks for one Login Items approval.
- **APIs:** `SMAppService.agent(plistName:)`, `register()`, `status` [18]. The plist is in
  `Contents/Library/LaunchAgents` [19]. Add `MachServices` only if a Swift XPC endpoint is ever needed [20].
- **Changes.**
  - **Kit:** `relay.installAgent()` and `relay.agentStatus`.
  - **Agent:** a "managed by <bundle id>" mode. If another `up` already owns the config dir, the bundled one
    defers to it (an `already_running` handshake). Version check over `/v1/state`.
  - **Release:** a universal `toprelay` binary that apps re-sign with their team.
- **Security.**
  - The agent then runs with the app's team signature, so keychain items can be shared through an access group
    [48].
  - Several apps bundling copies means several signers of "TopRelay", so the Kit must check the version and team
    before trusting a running agent.

### F5. Wake on call

- **User sees:** a bot calls ezpdf while it's closed. The call works after a short delay, instead of "the app isn't
  running".
- **Two levels:**
  1. **Agent only:** on `ECONNREFUSED` or no socket for a service that has a `bundleId`, run `open -g -j -b <id>`,
     wait up to N seconds for the socket, then forward.
  2. **launchd socket activation:** the app ships a tiny LaunchAgent with `Sockets/SockPathName` [20]. launchd
     owns the socket, starts the helper on the first connect (`launch_activate_socket` [21], or
     `NWListener(launchdSocketKey:)` [7]), and the helper serves the request or forwards it to the app. Intents
     with `ExecutionTargets` can run in an extension without the app [26].
- **Changes.** Agent: a launch policy per service plus a timeout that fits MCP clients (OpenAI's tunnel allows 120 s
  per request; see `agent-ecosystem-2026-10.md`). Kit: helper template.
- **Security.** Only launches a bundle id that was pinned at `add`. Log each launch to the audit trail.

### F6. App Intents bridge for apps without MCP (opt-in beta)

- **User sees:** `toprelay add things --intents com.culturedcode.ThingsMac` lists the app's discoverable intents
  (title, description, parameters) as MCP tools, all off by default.
- **APIs:**
  - Read `Metadata.appintents/extract.actionsdata` (undocumented; observed).
  - Make one Shortcut per chosen action, sign it with `shortcuts sign`, and have the user add it (one click each,
    per intents-mcp [29]).
  - Call `shortcuts run <id> --input-path - --output-path -` [30].
- **Changes.** Agent: a new `--intents` source kind served in-process with go-sdk (like the stdio bridge). No Kit or
  app changes.
- **Security.**
  - Shortcuts run as the user, with their permissions.
  - `openAppWhenRun` intents bring the app to the front.
  - Entity-based intents (most of them) aren't supported [29].
  - A metadata format change in macOS 27/28 would break it silently, so pin per tool (already built) and run
    `doctor` checks.
- **Reject the bigger version.** Making this the main path gives fragile tools from a format Apple doesn't document.

### F7. Apple Events bridge (later)

- **User sees:** `toprelay add notes --applescript notes`, using a built-in adapter for Notes, Mail (read-only),
  Calendar, Things or OmniFocus.
- **APIs:** `osascript` or JXA templates per tool. The adapter is hand-written per app (an `sdef` [40] helps write
  it). Needs the Automation consent [37][38].
- **Changes.** Agent: an adapter registry run in-process. **Security:** read-only by default, write tools behind
  phone approval (directions C), and tools pinned. One Automation grant gives broad access [37], so say so in the
  consent text.

### F8. HPKE end-to-end encryption (device-link path only)

- **User sees:** an iPhone companion or the user's own script talks to a Mac app with the relay seeing only
  ciphertext. **Not available to claude.ai, ChatGPT or Grok**, because they need plain HTTPS at the Worker.
- **APIs:**
  - CryptoKit `HPKE.Sender` / `HPKE.Recipient` with `Curve25519_SHA256_ChachaPoly` [45], and Go `crypto/hpke` [47].
  - The Mac's static key could live in the Secure Enclave as P-256, using `P256_SHA256_AES_GCM_256` [45][46].
  - For sessions, prefer Noise KK after pairing (algorithms §7a). Use HPKE for single sealed messages, such as a
    queued request while the Mac sleeps.
- **Changes.** Agent: an `e2e` frame type (binary frames reserved in protocol v1). Kit: client. Worker: none, since
  it forwards bytes.

## Open questions to test

1. Does `kDNSServiceInterfaceIndexLocalOnly` register or browse trigger the Local Network prompt from a LaunchAgent
   on macOS 26/27? (Matters only if Bonjour is ever reconsidered.)
2. Does the agent's `connect()` to a unix socket under `~/Library/Containers/<other app>/Data/` raise the
   `NSAppDataUsageDescription` prompt [52]? Same question for `~/Library/Group Containers/<group>/`. Ezpdf's
   example path in agent/README depends on the answer.
3. Can `NetworkListener` (macOS 26) bind `NWEndpoint.unix(path:)`? Do `NWListener.init(launchdSocketKey:)` [7]
   and launchd `Sockets` work with an `SMAppService` agent?
4. Does an encoded `GenerationSchema` pass as MCP `inputSchema` in claude.ai, ChatGPT, Grok and Gemini (which are
   strict about schemas)?
5. Through purego, does `SecCodeCopyGuestWithAttributes` with an audit token from `LOCAL_PEERTOKEN` work for a
   sandboxed, hardened-runtime peer? What does it cost per connection compared with `codesign`?
6. Which process does TCC charge for Apple Events sent by `toprelay up` running as a LaunchAgent, and does it
   prompt from the background?
7. Will the Mac App Store accept an app that bundles a non-sandboxed `SMAppService` LaunchAgent? If not, can a
   sandboxed agent work at all?
8. Watch macOS 27.x betas and WWDC27 for an App Intents MCP surface [27]. If Apple ships one, F6 should switch to
   it, and F1 should export App Intents through it.

## Sources

Apple documentation (read 2026-11-14 via `developer.apple.com/tutorials/data/documentation/<path>.json`):

1. WWDC25 session 250, Use structured concurrency with Network framework. https://developer.apple.com/videos/play/wwdc2025/250/
2. NetworkConnection (iOS/macOS 26). https://developer.apple.com/documentation/network/networkconnection
3. NetworkListener (iOS/macOS 26). https://developer.apple.com/documentation/network/networklistener
4. NWEndpoint, incl. `unix(path:)` and `service(name:type:domain:interface:)`. https://developer.apple.com/documentation/network/nwendpoint
5. NWParameters.acceptLocalOnly. https://developer.apple.com/documentation/network/nwparameters/acceptlocalonly
6. NWProtocolWebSocket. https://developer.apple.com/documentation/network/nwprotocolwebsocket
7. NWListener (incl. `init(launchd:using:)`, `init(launchdSocketKey:parameters:)`, `service`). https://developer.apple.com/documentation/network/nwlistener
8. TN3179: Understanding local network privacy. https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy
9. `dns_sd.h`, macOS SDK (Xcode, Nov 2025), `kDNSServiceInterfaceIndexLocalOnly` comment, lines 926–957. Source also at https://github.com/apple-oss-distributions/mDNSResponder
10. NSBonjourServices. https://developer.apple.com/documentation/bundleresources/information-property-list/nsbonjourservices
11. XPCListener (macOS 14). https://developer.apple.com/documentation/xpc/xpclistener
12. XPCSession (macOS 14). https://developer.apple.com/documentation/xpc/xpcsession
13. XPCPeerRequirement (macOS 26). https://developer.apple.com/documentation/xpc/xpcpeerrequirement
14. LightweightCodeRequirements (macOS 14.4). https://developer.apple.com/documentation/lightweightcoderequirements ; Process.launchRequirement https://developer.apple.com/documentation/foundation/process/launchrequirement
15. xpc_connection_set_peer_code_signing_requirement (macOS 12) and related. https://developer.apple.com/documentation/xpc/xpc_connection_set_peer_code_signing_requirement(_:_:)
16. Creating XPC services. https://developer.apple.com/documentation/xpc/creating-xpc-services
17. App Groups entitlement (IPC naming rules for Mach/XPC and UNIX sockets). https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.security.application-groups
18. SMAppService (macOS 13). https://developer.apple.com/documentation/servicemanagement/smappservice
19. Updating your app package installer to use the new Service Management API. https://developer.apple.com/documentation/servicemanagement/updating-your-app-package-installer-to-use-the-new-service-management-api
20. `launchd.plist(5)` man page, macOS 26.6 (MachServices, Sockets/SockPathName/SockPathMode, AssociatedBundleIdentifiers).
21. `launch(3)` man page, macOS 26.6 (`launch_activate_socket`).
22. App Intents framework. https://developer.apple.com/documentation/appintents
23. WWDC26 Platforms State of the Union (session 102). https://developer.apple.com/videos/play/wwdc2026/102/
24. WWDC26 macOS guide. https://developer.apple.com/wwdc26/guides/macos/
25. WWDC26 What's new in Shortcuts (session 310). https://developer.apple.com/videos/play/wwdc2026/310/
26. WWDC26 App Intents new capabilities (session 345). https://developer.apple.com/videos/play/wwdc2026/345/
27. 9to5Mac, "Apple working on MCP support on Mac, iPhone, and iPad" (2025-09-22). https://9to5mac.com/2025/09/22/macos-tahoe-26-1-beta-1-mcp-integration/ **(secondary)**
28. Apple Developer Forums, "Is MCP supported on iOS/macOS?" (Dec 2025). https://developer.apple.com/forums/thread/809361
29. VladUZH/intents-mcp (MIT). https://github.com/VladUZH/intents-mcp
30. `shortcuts(1)` man page, macOS 26.6.
31. Observed: `Metadata.appintents/extract.actionsdata` and `version.json` in `/Applications/ezpdf.app` and `/System/Applications/*.app`, macOS 26.6.2.
32. Foundation Models `Tool` (macOS 26). https://developer.apple.com/documentation/foundationmodels/tool
33. `GenerationSchema` and `GeneratedContent` (`init(json:)`, `jsonString`). https://developer.apple.com/documentation/foundationmodels/generationschema , https://developer.apple.com/documentation/foundationmodels/generatedcontent
34. WWDC26 Build agentic app experiences (session 242). https://developer.apple.com/videos/play/wwdc2026/242/
35. apple/foundation-models-utilities, `Sources/FoundationModelsUtilities/LanguageModels/ChatCompletionsLanguageModel.swift` (tool `parameters: GenerationSchema`, `json_schema` response format). https://github.com/apple/foundation-models-utilities
36. Foundation Models `LanguageModel` protocol (macOS 27). https://developer.apple.com/documentation/foundationmodels/languagemodel
37. NSAppleEventsUsageDescription. https://developer.apple.com/documentation/bundleresources/information-property-list/nsappleeventsusagedescription
38. Apple Events entitlement. https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.security.automation.apple-events
39. Scripting Bridge. https://developer.apple.com/documentation/scriptingbridge
40. `sdef(1)` man page, macOS 26.6.
41. NERelayManager (macOS 14). https://developer.apple.com/documentation/networkextension/nerelaymanager
42. NEAppProxyProvider. https://developer.apple.com/documentation/networkextension/neappproxyprovider
43. Wi-Fi Aware (iOS/iPadOS 26 only). https://developer.apple.com/documentation/wifiaware
44. Multipeer Connectivity. https://developer.apple.com/documentation/multipeerconnectivity
45. CryptoKit HPKE and HPKE.Ciphersuite (macOS 14). https://developer.apple.com/documentation/cryptokit/hpke , https://developer.apple.com/documentation/cryptokit/hpke/ciphersuite
46. CryptoKit SecureEnclave. https://developer.apple.com/documentation/cryptokit/secureenclave
47. Go `crypto/hpke` package docs (`go doc crypto/hpke`, Go 1.27). https://pkg.go.dev/crypto/hpke
48. Sharing access to keychain items among a collection of apps. https://developer.apple.com/documentation/security/sharing-access-to-keychain-items-among-a-collection-of-apps
49. DCAppAttestService. https://developer.apple.com/documentation/devicecheck/dcappattestservice
50. macOS SDK headers: `sys/un.h` (`LOCAL_PEERTOKEN` 0x006), `Security/SecCode.h` (`kSecGuestAttributeAudit`, `SecCodeCreateWithXPCMessage`). Also https://developer.apple.com/documentation/security/seccodecreatewithxpcmessage(_:_:_:)
51. Defining launch environment and library constraints. https://developer.apple.com/documentation/security/defining-launch-environment-and-library-constraints
52. NSAppDataUsageDescription (macOS 14). https://developer.apple.com/documentation/bundleresources/information-property-list/nsappdatausagedescription

MCP and SDKs:

53. MCP specification 2026-07-28, Key Changes. https://modelcontextprotocol.io/specification/2026-07-28/changelog
54. modelcontextprotocol/swift-sdk: README (targets 2025-11-25; transports table), releases (0.12.1, 2026-05-07), issues #245 (SEP-2575 stateless) and #238 (MRTR), LICENSE (MIT→Apache-2.0 transition). https://github.com/modelcontextprotocol/swift-sdk
55. modelcontextprotocol/go-sdk v1.8.0 release notes and README version table. https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.8.0

Open-source libraries (GitHub API, read 2026-11-14):

56. apple/swift-nio. https://github.com/apple/swift-nio
57. apple/swift-nio-transport-services. https://github.com/apple/swift-nio-transport-services
58. apple/swift-nio-ssh. https://github.com/apple/swift-nio-ssh
59. apple/swift-crypto. https://github.com/apple/swift-crypto
60. apple/swift-certificates. https://github.com/apple/swift-certificates
61. hummingbird-project/hummingbird. https://github.com/hummingbird-project/hummingbird
62. ebitengine/purego. https://github.com/ebitengine/purego
63. golang/go issue #6379, "crypto/tls: add PSK support" (open, updated 2026-10-26). https://github.com/golang/go/issues/6379
64. `sec_protocol_options_add_pre_shared_key` and `NWParameters.requiredLocalEndpoint`. https://developer.apple.com/documentation/security/sec_protocol_options_add_pre_shared_key(_:_:_:) , https://developer.apple.com/documentation/network/nwparameters/requiredlocalendpoint
65. Apple Developer videos, System Services topic (WWDC26/WWDC25 lists). https://developer.apple.com/videos/system-services/
66. cloudflare/circl. https://github.com/cloudflare/circl
