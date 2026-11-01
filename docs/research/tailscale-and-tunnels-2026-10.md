# Tailscale (and other tunnels) for TopRelay, October 2026

Research date: 2026-10-31. Primary sources only (tailscale.com docs/blog/changelog, github.com/tailscale source,
pkg.go.dev, vendor docs for the other tools). Each claim has a source number; see the list at the end. Anything
marked **unverified** I couldn't confirm from a primary source. Tailscale moved its docs from `/kb/...` to
`/docs/...` in 2026; old `/kb/` links still work for some pages.

## Summary

- **Funnel is still beta, on all plans, ports 443/8443/10000 only, `*.ts.net` names only, with bandwidth limits
  that aren't published and can't be changed** [1]. Custom domains are an open feature request; the only `--domain`
  code is in a personal fork, not released [4].
- **Funnel is a TCP/TLS passthrough: TLS ends on the user's device**, not at Tailscale [1]. So SSE and
  Streamable HTTP depend only on the server on the device. That is stronger privacy than TopRelay's Cloudflare
  design, where the Worker terminates TLS.
- **The Go agent can embed `tsnet` and call `ListenFunnel` with no Tailscale app installed** [2][6]. The user still
  needs a Tailscale account, and the tailnet needs HTTPS and the `funnel` node attribute. `tsnet` can ask the
  control server for a one-click "enable Funnel" URL via LocalAPI `QueryFeature` (the same flow the CLI uses) [6].
- **There's no API call to turn Funnel on for a device.** Funnel is a node setting (`ServeConfig.AllowFunnel`) set
  through LocalAPI. The admin side is the policy file (`nodeAttrs`), which the API can write with the
  `policy_file` scope [1][6][13].
- **Funnel requests carry no caller identity** (no `Tailscale-User-*` or app-capability headers) [11][6]. Tailscale
  identity only helps for callers that are on the tailnet. Cloud bots never are.
- **Tailscale's 2026 AI work is about governing agents inside a tailnet, not reaching local apps from public
  bots.** Aperture (GA 2026-08-25/26) is an LLM and MCP gateway with no direct public internet access [3][8][9].
  Tailscale's own blog says not to Funnel an AI agent that holds personal data [17].
- **tsidp is experimental and is a "community project"**, but it implements the MCP authorization endpoints,
  including DCR, and has a `-funnel` flag for SaaS sign-in [7][10]. Only tailnet users can sign in through it, so
  it can't be the OAuth server for a single owner approving claude.ai without more work. Don't depend on it.
- **Free Personal plan: up to 6 users, unlimited user devices, 50 tagged resources, 1,000 ephemeral-node minutes per
  month, non-commercial use only** [5]. That non-commercial limit matters if TopRelay ever sells a Funnel backend.
- **Other 2026 options:** Cloudflare protected Quick Tunnels (`--allowed-mail`, 2026-10-02) still use an email PIN
  that bots can't pass [22]. NetBird (v0.65/v0.66) has a reverse proxy with custom domains and `netbird expose`
  [24][25]. iroh 1.0 (2026-07-09) gives P2P QUIC with hole punching [26]. Tailscale's `tailcat` (2026-08) uses
  Tailscale's data plane with no account [20].
- **Recommendation:** keep Cloudflare as the v1 backend. Treat Tailscale as two optional paths for later:
  `--backend funnel` (embedded tsnet, end-to-end TLS, no Cloudflare needed), and `--private` (tsnet Serve plus
  WhoIs-based tool grants for the user's own devices and agents). Neither changes the core design rule.

## 1. Tailscale Funnel, late 2026

| Question | Answer | Source |
|---|---|---|
| Status | Beta. Docs last validated 2026-01-20 | [1] |
| Plans | "Available on all plans", including the free Personal plan | [1][5] |
| Ports | 443, 8443, 10000 only (TCP). Enforced in code by the `funnel-ports` node attribute | [1][6] |
| Bandwidth | "Subject to non-configurable bandwidth limits". No figures published. Third-party reports are anecdotal | [1] |
| Protocol | TCP proxy through Funnel relay servers. TLS only, ended on your device. Relays can't decrypt | [1] |
| SSE / streaming | Not covered in the docs. Because Funnel passes raw TCP/TLS through, streaming depends on the server on the device. Serve's built-in proxy is Go's `httputil.ReverseProxy` with no custom flush settings [6]. Go flushes `text/event-stream` immediately (Go stdlib behaviour; I didn't test it through Serve). With `tsnet.ListenFunnel` the agent runs its own HTTP server, so streaming is under our control | [1][6] |
| Requirements | Tailscale ≥ 1.38.3, MagicDNS, HTTPS certificates enabled, `funnel` in `nodeAttrs`. The default policy grants it to `autogroup:member` | [1] |
| Domains | Only `<device>.<tailnet>.ts.net`. Custom domains are FR #11563: open, `needs-triage`. A 2026-09-26 comment links a `funnel --domain` patch in a personal fork. Not merged, not released | [1][4] |
| PROXY protocol | Supported for Funnel and Serve since v1.92.1 (2025-12-10) | [12] |
| Serve vs Funnel on one port | One or the other. The last command wins | [1] |
| Certificates | Let's Encrypt. Rate limits can block new certificates for up to 34 hours. New public DNS can take up to 10 minutes | [1] |
| macOS | Funnel in the GUI app needs the open-source or Standalone variant (not relevant to embedded tsnet; see §2) | [1] |
| Identity | Funnel requests get `Tailscale-Funnel-Request: ?1` and no user or app-capability headers | [6][11] |
| Recent fixes | v1.98.9 (2026-07-14) and v1.104.4 (2026-10-28) fixed security issues in Serve/Funnel. v1.102.2 (2026-08-04) fixed a regression that broke incoming Funnel connections. Pin a recent tsnet | [12][18] |

**Turning it on programmatically.** The Funnel docs describe only the CLI approval flow and the admin console's
"Add Funnel to policy" button [1]. Per-device Funnel is a LocalAPI `ServeConfig` with `AllowFunnel[host:port]=true`,
not a REST API resource. `tsnet.ListenFunnel` sets it for you and removes it on close [6]. On the tailnet side,
the API can write the policy file (`POST /api/v2/tailnet/:tailnet/acl`, scope `policy_file`) to add the `nodeAttrs`
entry [13]. I found no documented API call for the "Enable HTTPS" toggle (it's on the DNS page in the console)
(**unverified** whether `/tailnet/:tailnet/settings` exposes it) [13][14]. The simpler path is LocalAPI
`QueryFeature("funnel")`: it returns a control-server URL the user opens to enable the feature, and the CLI uses
it for its interactive enable [6].

**Free plan.** Personal: $0, up to 6 users, unlimited user devices, 50 tagged resources included ($1/month each
after that), 1,000 ephemeral minutes per month, 3 ACL groups, non-commercial use only [5]. A tsnet node signed in
as the user (not tagged, not ephemeral) uses none of the tagged or ephemeral allowance.

## 2. Embedding `tsnet` in the TopRelay Go agent

**What exists** (tsnet v1.104.x, published 2026-10-27/28) [2][6][12]:
- `tsnet.Server{Hostname, Dir, AuthKey, Ephemeral, ClientSecret, ClientID, IDToken, Audience, AdvertiseTags,
  ControlURL, Port, ...}`.
- `ListenFunnel("tcp", ":443", tsnet.FunnelOnly())` returns a TLS listener on the public internet, using the
  node's `*.ts.net` certificate. Without `FunnelOnly` it also listens on the tailnet. It fails with an explicit
  error if HTTPS isn't enabled, if the `funnel` attribute is missing, or if the port isn't allowed [6].
- `ListenTLS` / `Listen` (tailnet only), `ListenService(name, mode)` (Tailscale Services host; the node must be
  tagged and approved; tsnet support since v1.94.1, 2026-01-26) [2][12][15].
- `LocalClient()` gives `WhoIs`, `WhoIsForService`, `QueryFeature`, `GetServeConfig`/`SetServeConfig` [6].
- Auth order, from the package doc: `AuthKey` → `TS_AUTHKEY` → `TS_AUTH_KEY` → OAuth client secret (mints an auth
  key; tags required) → workload identity federation (needs `import _ "tailscale.com/feature/identityfederation"`)
  → interactive login URL printed to `UserLogf` [6]. Once enrolled, state in `Dir` is reused and the key is
  ignored [2][6].
- In-memory state is allowed only for ephemeral nodes [6].

**Does the user need to install Tailscale?** No. tsnet is a userspace node with its own netstack, its own identity
and its own MagicDNS name; Tailscale's August 2026 posts describe it as needing "no host daemon" [16][19].
(Inference: the macOS GUI variant limits in §1 don't apply, because tsnet doesn't use the system extension.
Running alongside an installed Tailscale app should be fine, because tsnet is a separate node.
**Unverified** by test.)

**Credentials, and which to use:**
- *Interactive login URL*: best for a single user. The node is owned by the user, never expires as a key, and isn't
  tagged or ephemeral. If device approval is on, an admin must approve it [21].
- *Auth keys*: one-off or reusable, optionally ephemeral, pre-approved or tagged. They expire after at most 90 days.
  Devices already joined stay joined until their node key expires (default 180 days) [23]. A user would have to
  generate one in the console and paste it. That's worse than the login URL.
- *OAuth client / trust credentials*: scoped API access. `devices:core` and `auth_keys` credentials must create
  **tagged** keys [13]. Tagged nodes get no user identity headers from Serve [11] and count against the 50 tagged
  resources [5]. Useful for headless fleets, not for a person's laptop.
- *Ephemeral*: removed 30–60 minutes after it goes offline, and gets a new IP next time [21]. A laptop that sleeps
  would lose its node, and maybe its stable hostname (hostname reuse **unverified**). Don't use ephemeral for the
  user's main agent.
- *Workload identity federation* (GA 2026-02-19): for CI and cloud workloads with an OIDC issuer [12]. Not for
  laptops.

**What onboarding could look like** (the design is mine; each step uses only APIs listed above):
```
toprelay add notes --mcp-url http://127.0.0.1:7879/mcp --backend funnel
  → tsnet.Server{Hostname:"toprelay", Dir:~/.config/toprelay/tsnet} .Up()
  → prints/open login URL           (user signs in or signs up to Tailscale with Google/GitHub/etc.)
  → LocalClient().QueryFeature("funnel") → if not enabled, open the returned URL (one click enables HTTPS + funnel attr)
  → ListenFunnel("tcp", ":443", FunnelOnly())
  ✓ https://toprelay.<tailnet>.ts.net/notes/mcp
```
Costs to the user: a Tailscale account, two browser steps, and a public URL that shows their tailnet name. Setup
needs no Cloudflare account or token at all.

**What stays the same in TopRelay.** The agent already owns MCP awareness. In Funnel mode the agent itself is the
HTTP server, so the tool allow-list, call log and stdio bridge carry over unchanged. What's missing is the Worker's
OAuth server: in Funnel mode the agent would have to host OAuth (PKCE, DCR, metadata, consent with a pairing code)
itself, in Go. claude.ai reaching a `*.ts.net` Funnel URL with OAuth is **unverified** and must be tested.

## 3. What Tailscale shipped for AI agents and MCP, 2025–2026

| Item | What it is | Date / status | Relevance to TopRelay | Source |
|---|---|---|---|---|
| **Aperture** | AI gateway on the tailnet: holds LLM provider keys, logs usage, guardrail hooks, MCP and HTTP API proxy ("connectors") with deny-by-default tool grants, Aperture Chat and agent sandboxes | Alpha 2026-02-17, beta 2026-04-23, GA 2026-08-25/26 | Inside the tailnet only. "Does not support direct public internet access"; outside clients need the Aperture CLI bridge or `ts-unplug`. Not a way for claude.ai to reach a laptop | [3][8][9][12] |
| Tailscale and Tailscale SSH MCP endpoints | MCP endpoints in Aperture that let coding agents add nodes and SSH to them, with user approval | 2026-08 (Aperture GA) | Agents managing a tailnet, not exposing apps | [8][18] |
| **tsidp** | OIDC/OAuth server using tailnet identity (WhoIs). MCP auth endpoints including DCR (gated by `allow_dcr` grant), RFC 8693 token exchange (`-enable-sts`), `-funnel` flag | Experimental, below 1.0, `TAILSCALE_USE_WIP_CODE=1` needed, "community project"; docs validated 2026-03-10 | See §4 | [7][10] |
| DCR-for-MCP blog | Plan to make tsidp support MCP DCR and STS; says the approach fits internal, not public, MCP servers | 2025-08-26 | Confirms Tailscale is aiming at private MCP | [27] |
| Lee Briggs MCP post | Local proxy forwards Tailscale identity to a tailnet MCP server, which checks grants per tool. Notes Anthropic's remote MCP "expects them to be on the public internet" | 2025-08-04 | Pattern for private mode (§5c) | [28] |
| **Tailscale Services** | Named services with a stable TailVIP and MagicDNS name; hosts must be tagged and approved; `ListenService` in tsnet | Beta 2025-10-28, GA 2026-01-27; docs validated 2026-10-07 | Stable private names for a service, independent of the host device. No Funnel mention | [12][15] |
| **Peer Relays** | Your own devices relay UDP traffic when direct connections fail, before falling back to DERP; grant `tailscale.com/cap/relay` | Beta 2025-10-29, GA 2026-02-18 | Helps device-to-device throughput behind hard NAT. Not public ingress | [12][29] |
| Workload identity federation | Join or call the API with an OIDC token instead of a secret | Beta 2025-10-30, GA 2026-02-19 | CI and cloud only | [12][13] |
| Tailnets API | Create API-only tailnets (tagged devices only) on demand; up to 10 per organization without a sales contract | Alpha; validated 2026-10-20 | Can't give each TopRelay user a tailnet at scale | [30] |
| ts-plug / ts-unplug | `ts-plug -hostname x -- cmd` puts a local server on the tailnet (`-public` for public access, whether that uses Funnel is **unverified**); `ts-unplug` brings a tailnet service to localhost | "Work in progress", no releases; open bug: request bodies over about one MTU hang | Closest Tailscale product to TopRelay's CLI shape, but immature | [31][32] |
| tailcat | Netcat over Tailscale's data plane (WireGuard, NAT traversal, DERP) with no account or control plane | Open-sourced 2026-08 | A P2P transport without a Tailscale account. Hosted DERP keeps metadata logs | [20][33] |
| tailscale-rs | Experimental Rust embedding library | 2026-08 | Possible future for Swift via C bindings; "more direction than production code" | [16] |
| App connectors | Route SaaS app traffic through a tailnet node | Existing; 2025 fixes | Not relevant | [12] |
| MagicRoute (beta) | Choose between redundant routers, connectors and Service hosts | 2026-10-19 | Not relevant | [12] |

Tailscale's own advice on Funnel and agents (2026-06-02): Funnel is for "small webapps... you need to expose to the
open web". It is not for "Anyone can reach my AI agent, with access to my personal data, stuffed with credentials,
with just a password". Tailscale now warns users with claw-named Funnel setups [17].

## 4. tsidp as the OAuth server for claude.ai

- It implements "all of the endpoints required & suggested by the MCP Authorization specification, including
  Dynamic Client Registration" [7][10].
- It identifies users with LocalAPI WhoIs, so a person has to be on the tailnet to sign in [7]. With `-funnel`,
  the endpoints are public "so it works with SaaS products" [10]. In claude.ai's flow, the user's browser hits
  `/authorize` and the claude.ai backend hits `/token` and `/register`. A browser on a tailnet device could pass
  WhoIs. A phone or laptop off the tailnet couldn't (inference). How Funnel callers are treated is not documented
  [10].
- DCR is denied by default; enabling it needs an `allow_dcr` grant. claude.ai's backend registers over Funnel, so
  it has no tailnet identity, and whether a grant can allow that is **unverified** [10].
- It's experimental, below 1.0, needs a WIP env flag, and its capability schema "may change at anytime" [10].

**Verdict:** not a fit as TopRelay's OAuth server. It would add a second public endpoint, a Tailscale dependency,
and an unstable schema, and it still wouldn't give the single-owner pairing-code consent TopRelay needs. Keep
`workers-oauth-provider` (Cloudflare backend) or a small Go OAuth server in the agent (Funnel backend). tsidp's
code is worth reading as a reference for MCP-compliant metadata and DCR in Go.

## 5. Hybrid design options for TopRelay

### (a) `--backend funnel` as an optional second backend
- **Real:** `tsnet.ListenFunnel` with `FunnelOnly()`. The agent serves MCP directly over TLS it terminates itself,
  and the URL is stable per device and tailnet [2][6].
- **Pros:** no Cloudflare account; end-to-end TLS to the laptop (no relay sees plaintext); no Workers request
  budget; no WebSocket wire protocol to maintain for this path; streaming is native HTTP.
- **Cons:** needs a Tailscale account and two enable steps; beta; unpublished bandwidth limits; only ports
  443/8443/10000; only `*.ts.net` names (shows the tailnet name); no offline 503 when the laptop sleeps (DNS
  resolves, the connection fails); the Personal plan is non-commercial [1][5]; OAuth must move into the agent;
  claude.ai and ChatGPT compatibility with `*.ts.net` Funnel is untested. Dependency weight: tsnet pulls in a lot
  of Tailscale code (binary size **unverified**, measure it).
- **Effort:** moderate. Mostly a Go OAuth server, and that's also what a future "no relay" mode would need.

### (b) Private mode: the user's own devices and agents, no public endpoint
- **Real:** `tsnet` `Listen`/`ListenTLS` on the tailnet, or `ListenService` for a stable service name (tagged host,
  approval needed) [2][15]. Claude Code, Codex and local agents on another of the user's devices can reach
  `https://toprelay.<tailnet>.ts.net/notes/mcp` directly.
- **Pros:** nothing public at all, which matches Tailscale's own advice for agents with personal data [17]. No OAuth
  needed when the caller is a tailnet identity. Works for home servers and other laptops.
- **Cons:** cloud bots (claude.ai, ChatGPT, Grok, API connectors) can't reach it. Anthropic's remote MCP expects
  public URLs [28]. iOS and Android devices need the Tailscale app to join.
- **Assessment:** cheap and useful. Probably the best first Tailscale feature, because it doesn't compete with the
  Cloudflare path and fills a gap the Cloudflare path can't fill.

### (c) Tailscale identity (WhoIs, grants) as the per-caller authorization layer
- **Real:** `LocalClient().WhoIs(remoteAddr)` returns the user or node and a `CapMap` of app capabilities from
  policy `grants`. `WhoIsForService` scopes this to a Service [6]. Serve forwards `Tailscale-User-Login` etc. and,
  with `--accept-app-caps`, `Tailscale-App-Capabilities` (v1.92+) [11]. The Briggs post does per-tool grants this
  way [28].
- **Limit:** none of this exists for Funnel traffic [6][11], and tagged nodes get no user headers [11]. So it only
  covers private mode (b).
- **Design:** in private mode, map an app capability like `toprelay.dev/cap/mcp: {"services":["notes"],
  "tools":["search_*"]}` onto the agent's existing allow-list. The policy then lives in the tailnet file and the
  owner can edit it with GitOps. Keep the agent's own allow-list as the default when no grant is present.

### (d) Device-to-device app linking (laptop ↔ phone ↔ home server)
- **Real:** any two tailnet nodes reach each other over WireGuard with NAT traversal, DERP fallback, and optionally
  Peer Relays [29]. tsnet gives Go processes this without the app; on phones the Tailscale app is needed.
  Swift has no tsnet; tailscale-rs is experimental [16].
- **Alternatives without accounts:** `tailcat` (Tailscale data plane, address exchanged out of band, fresh key per
  run unless saved) [20][33]; iroh 1.0 (QUIC dialed by Ed25519 endpoint ID, hole punching with relay fallback,
  bindings for "almost all major languages", Go not named) [26]; iroh's FAQ says hole punching works about 9 times
  in 10 [34].
- **Assessment:** speculative for TopRelay. It's a different product ("link my apps together") from "let cloud
  bots reach my app". If pursued, iroh or tailcat avoid making every user sign up for Tailscale. Tailscale is the
  better choice when the user already runs a tailnet.

## 6. Other 2026 options (primary-source facts)

- **Cloudflare Tunnel / cloudflared.** Protected Quick Tunnels: `cloudflared tunnel --url ... --allowed-mail
  alice@example.com` puts an email one-time PIN in front; no Cloudflare account needed; access ends when the
  process stops (2026-10-02) [22]. A bot can't pass an email PIN. `wrangler tunnel quick-start` (2026-03-19) starts
  an accountless Quick Tunnel; cloudflared 2026.5.2 added startup pre-checks; 2026.10.0 normalizes ingress URLs
  [35][36]. Named tunnels give stable hostnames but need a zone on the account (existing behaviour).
- **ngrok.** The MCP guide: run `ngrok http <port> --traffic-policy-file policy.yml`. Free plan includes a stable
  dev domain. Bearer, secret-path and IP-category rules work on free; rate-limit and url-rewrite need a paid plan.
  OAuth is left to the MCP server (ngrok can validate JWTs at the edge). SSE and Streamable HTTP work [37]. The AI
  Gateway moved to app.ngrok.ai, with ordered fallbacks added 2026-10-29 and provider keys in Agent 3.39.0
  [38][39].
- **NetBird.** v0.65 added a built-in reverse proxy with free `*.proxy.netbird.io` domains and custom domains
  (wildcard CNAME), protected by SSO, password, PIN or nothing [24][40]. v0.66 added `netbird expose 8080`
  (ephemeral, removed when the command stops; `--with-custom-domain`) [25]. You can also self-host the proxy. This
  is the closest thing to "Funnel with custom domains" today.
- **zrok (NetFoundry, on OpenZiti).** 2026 free tier: 5 GB per day, 25 environments, 50 share backends (ephemeral
  or reserved) [41]. v2 replaced reserved shares with namespaces and `zrok2 create name` [42].
- **Pinggy.** Free tunnels time out after 60 minutes with a new URL each time. Pro (about $3/month) gives
  persistent subdomains and custom domains (vendor pages; prices **unverified** against a pricing page) [43].
- **iroh (n0).** 1.0 shipped 2026-07-09: QUIC by endpoint ID, hole punching modelled on Tailscale's, relay fallback,
  multipath, pkarr/DHT address lookup [26]. It's a P2P library, not public HTTPS ingress, so it doesn't help cloud
  bots.
- **OpenAI Secure MCP Tunnel** is covered in `docs/brief.md`.

## What this means for TopRelay

1. **Keep Cloudflare as the only v1 backend.** Nothing Tailscale shipped in 2026 gives cloud bots a public endpoint
   with OAuth, a custom domain and per-tool control. Funnel is the closest, and it's still beta with unpublished
   limits and only `ts.net` names [1][4].
2. **Update the competitor row in `docs/brief.md`.** Funnel is "available on all plans", and TLS ends on the device
   (better privacy than TopRelay's relay). Say so honestly, and add: no custom domains, no caller identity on
   Funnel, the Personal plan is non-commercial, and Tailscale itself advises against Funnel for agents with
   personal data [1][5][17].
3. **Design the agent's MCP handler as an `http.Handler` that doesn't care how requests arrive.** Today that's the
   WebSocket from the Durable Object; later it could be a tsnet listener. This costs nothing now and keeps (a) and
   (b) open.
4. **v1.x: add `--private` (tsnet tailnet listener) before `--backend funnel`.** It's real, small, private by
   default, and the only way TopRelay can serve the user's own agents on other devices with no public exposure.
   Use `WhoIs` + an app-capability grant as an optional extra layer on top of the allow-list (§5c).
5. **Later, if users ask: `--backend funnel`.** It needs a Go OAuth server in the agent (PKCE, DCR, metadata,
   pairing-code consent). Build that once and reuse it for any non-Cloudflare backend. Use the interactive login
   (user-owned, non-ephemeral node) and `QueryFeature("funnel")` for one-click enabling. Test claude.ai, ChatGPT
   and the Claude API against a `*.ts.net` URL before promising anything.
6. **Don't adopt tsidp, Aperture or the Tailnets API.** tsidp is experimental and tied to tailnet identity.
   Aperture is tailnet-internal. The Tailnets API is alpha and capped at 10 tailnets per organization [10][9][30].
7. **Watch:** Funnel `--domain` (FR #11563) [4], Funnel leaving beta, published Funnel bandwidth numbers,
   tailscale-rs (a possible TopRelayKit path), NetBird's `expose` as a custom-domain competitor, and whether
   Anthropic or OpenAI ever let connectors join a tailnet [28].
8. **Pin tsnet ≥ v1.104.4** if it's adopted, because of the 2026 Serve/Funnel security fixes [12][18].

## Sources

1. Tailscale Funnel docs (validated 2026-01-20): https://tailscale.com/docs/features/tailscale-funnel (also https://tailscale.com/kb/1223/funnel)
2. tsnet package docs, v1.104.4 (2026-10-27): https://pkg.go.dev/tailscale.com/tsnet
3. What is Aperture (validated 2026-09-29): https://tailscale.com/docs/aperture/what-is-aperture
4. FR: custom domains for Funnel, issue #11563: https://github.com/tailscale/tailscale/issues/11563
5. Tailscale pricing: https://tailscale.com/pricing
6. Tailscale source (main, read 2026-10-31): https://github.com/tailscale/tailscale/blob/main/tsnet/tsnet.go , https://github.com/tailscale/tailscale/blob/main/ipn/serve.go , https://github.com/tailscale/tailscale/blob/main/ipn/ipnlocal/serve.go , https://github.com/tailscale/tailscale/blob/main/client/local/local.go , https://github.com/tailscale/tailscale/blob/main/cmd/tailscale/cli/serve_v2.go
7. tsidp docs (validated 2026-03-10): https://tailscale.com/docs/features/tsidp
8. Aperture GA blog (2026-08-26): https://tailscale.com/blog/aperture-ga
9. Aperture MCP server proxying: https://tailscale.com/docs/aperture/mcp-server ; connect outside tailnet: https://tailscale.com/docs/aperture/connect-outside-tailnet
10. tsidp repo README: https://github.com/tailscale/tsidp
11. Tailscale Serve docs (validated 2026-01-20): https://tailscale.com/docs/features/tailscale-serve
12. Tailscale changelog: https://tailscale.com/changelog
13. Trust credentials (validated 2026-01-30): https://tailscale.com/kb/1623/trust-credentials
14. Enabling HTTPS: https://tailscale.com/docs/how-to/set-up-https-certificates
15. Tailscale Services (validated 2026-10-07): https://tailscale.com/docs/features/tailscale-services
16. "Build with Tailscale using tsnet, APIs, and automated sharing" (2026-08-31): https://tailscale.com/blog/easier-building-with-tailscale
17. "Tailscale Funnel, Serve, and a tiny website for my fridge" (2026-06-02): https://tailscale.com/blog/funnel-fridge
18. Security bulletins: https://tailscale.com/security-bulletins
19. The 2026 TailscaleUp-date: https://tailscale.com/tailscaleup-date-week-26
20. tailcat page: https://tailscale.com/tailcat ; blog: https://tailscale.com/blog/tailcat
21. tsnet KB (validated 2026-07-24): https://tailscale.com/kb/1244/tsnet ; Ephemeral nodes (validated 2025-12-04): https://tailscale.com/kb/1111/ephemeral-nodes
22. Cloudflare changelog, Protected Quick Tunnels (2026-10-02): https://developers.cloudflare.com/changelog/post/2026-10-02-protected-quick-tunnels/ ; blog: https://blog.cloudflare.com/protected-quick-tunnels/
23. Auth keys (validated 2026-06-30): https://tailscale.com/kb/1085/auth-keys
24. NetBird Reverse Proxy docs: https://docs.netbird.io/manage/reverse-proxy ; custom domains: https://docs.netbird.io/manage/reverse-proxy/custom-domains
25. NetBird expose from CLI: https://docs.netbird.io/manage/reverse-proxy/expose-from-cli ; v0.66.0 release: https://github.com/netbirdio/netbird/releases/tag/v0.66.0
26. "The road to iroh 1.0" (2026-07-09): https://www.iroh.computer/blog/the-road-to-iroh-1-0
27. "Easy, Secure Dynamic Client Registration for MCP & AI" (2025-08-26): https://tailscale.com/blog/dynamic-client-registration-dcr-for-mcp-ai
28. "Making a Model Context Protocol server more robust, and much more private" (2025-08-04): https://tailscale.com/blog/model-for-mcp-connectivity-lee-briggs
29. Peer Relays docs (validated 2026-02-04): https://tailscale.com/docs/features/peer-relay
30. Tailnets API (alpha, validated 2026-10-20): https://tailscale.com/docs/features/tailnets-api
31. ts-plug repo: https://github.com/tailscale/ts-plug
32. ts-plug issue #11 (large bodies hang): https://github.com/tailscale/ts-plug/issues/11
33. tailcat repo: https://github.com/tailscale/tailcat
34. iroh FAQ: https://docs.iroh.computer/about/faq
35. Cloudflare Tunnel changelog: https://developers.cloudflare.com/changelog/product/tunnel/
36. cloudflared CHANGES.md: https://github.com/cloudflare/cloudflared/blob/master/CHANGES.md
37. ngrok, Using ngrok as your MCP gateway: https://ngrok.com/docs/using-ngrok-with/using-mcp
38. ngrok Agent changelog: https://ngrok.com/docs/gateway/agent/changelog
39. "The new ngrok.ai": https://ngrok.com/blog/new-ngrok-ai
40. NetBird v0.65 reverse proxy: https://netbird.io/knowledge-hub/reverse-proxy
41. zrok pricing 2026: https://zrok.io/pricing-2026/
42. zrok v1 → v2 migration: https://netfoundry.io/docs/zrok/how-tos/migration/migrate-v1-to-v2/
43. Pinggy compare / docs: https://pinggy.io/compare/pinggy-vs-ngrok/ , https://pinggy.io/docs/http_tunnels/
