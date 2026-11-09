<p align="center">
  <img src="design/app-icon-256.png" width="128" height="128" alt="TopRelay app icon: a glowing glass bridge from a local app to an AI">
</p>

<h1 align="center">TopRelay</h1>

**Status: early development. The engine works end to end locally; real-bot testing and the Mac app are next.**

TopRelay gives the apps on your computer a stable, private HTTPS address that any AI can use: claude.ai, ChatGPT,
Grok, the Claude and OpenAI APIs, or your own agents. It runs on your own free Cloudflare account. You choose which
tools each bot can use.

- **No server to rent.** The relay is deployed into your Cloudflare account. Nothing goes through us or an AI
  vendor.
- **Works behind any router.** Your computer makes one outbound connection, so nothing has to be opened on your
  network.
- **Auth built in.** Every service needs OAuth or a bearer key, even if the local app has no auth of its own.
- **You choose the tools.** Hide tools from the list, and refuse calls to tools you didn't allow.
- **Rug-pull proof.** Every tool's definition is pinned by hash. If a server later changes what a tool says or
  does, the tool is held, hidden and refused, until you review the change (`toprelay pins`).
- **Leak-proof answers.** Before anything leaves your computer, API keys, tokens, private keys and the app's own
  credentials are redacted. Text that tries to instruct the model (hidden Unicode, "ignore previous
  instructions") is flagged (`toprelay shield`).
- **Every call is provable.** The call log is hash-chained and signed with Ed25519, so `toprelay audit verify`
  shows whether a single line was edited, inserted or removed.
- **Apps can tell.** Every relayed call carries a signed `X-TopRelay-Caller` header, so an app knows for sure that
  a call is remote and which bot made it. Credentials come from a file or the keyring, never the command line, and
  can be changed live. Unix sockets and macOS code-signature pins stop another program from answering for the app.

```
Bot ──HTTPS + OAuth/bearer──▶ https://toprelay.<you>.workers.dev/<service>/mcp
                                  │  Worker + Durable Object (your account)
                                  ▼
          your computer ◀── one outbound WebSocket
              ├─▶ local MCP server (Streamable HTTP)
              ├─▶ local MCP server (stdio)
              └─▶ local HTTP service
```

## What Cloudflare can see

The relay ends TLS inside the Worker on **your** Cloudflare account, so traffic between the bot and your app is not
end-to-end encrypted. Cloudflare's systems handle it the way they handle any Worker request. TopRelay logs no
request bodies and stores only OAuth grants. See [SECURITY.md](SECURITY.md).

## Try it (developers)

```sh
cd agent && go build -o toprelay ./cmd/toprelay
./toprelay login                                   # Cloudflare API token
./toprelay add notes --mcp-url http://127.0.0.1:7879/mcp --header-file "X-Notes-Token=$HOME/.notes/token"
./toprelay add tools --stdio -- node ./server.js
./toprelay up                                      # keep everything connected (up --install for login start)
./toprelay pair notes                              # code for the claude.ai consent page
./toprelay key notes --client claude-api           # bearer key + snippet for APIs
./toprelay doctor                                  # what's wrong and how to fix it
./toprelay test notes                              # one harmless call end to end, with latency
```

Not tested yet against real claude.ai, ChatGPT, Grok, Gemini or Qwen accounts. The compatibility table in
`docs/brief.md` is filled only from real tests, following [`docs/bot-testing.md`](docs/bot-testing.md).

## Layout

| Path | What |
|---|---|
| `worker/` | Relay Worker and `Relay` Durable Object (TypeScript). See [worker/README.md](worker/README.md) |
| `agent/` | The `toprelay` Go CLI and daemon. See [agent/README.md](agent/README.md) |
| `TopRelayKit/` | Swift package for Mac apps: drive the agent over its control socket. See [TopRelayKit/README.md](TopRelayKit/README.md) |
| `docs/protocol.md` | The exact contract between the two (frames, keys, routes, limits, test vectors), and what a local app receives |
| `MIGRATION.md` | For app authors: credentials from a file, verifying `X-TopRelay-Caller`, sockets and pins |
| `docs/verifiers/` | `X-TopRelay-Caller` verifiers in Go, Swift, Python and Node, tested against the protocol vector |
| `docs/brief.md` | Product brief and build plan |
| `docs/research/` | Research notes behind the design |
| `spike/oauth/` | Throwaway OAuth test against claude.ai |

## License

[Apache-2.0](LICENSE)
