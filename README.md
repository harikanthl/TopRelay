# TopRelay

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
./toprelay add notes --mcp-url http://127.0.0.1:7879/mcp
./toprelay add tools --stdio -- node ./server.js
./toprelay up                                      # keep everything connected (up --install for login start)
./toprelay pair notes                              # code for the claude.ai consent page
./toprelay key notes --client claude-api           # bearer key + snippet for APIs
./toprelay doctor                                  # what's wrong and how to fix it
./toprelay test notes                              # one harmless call end to end, with latency
```

Not tested yet against real claude.ai, ChatGPT, Grok, Gemini or Qwen accounts. The compatibility table in
`docs/brief.md` is filled only from real tests.

## Layout

| Path | What |
|---|---|
| `worker/` | Relay Worker and `Relay` Durable Object (TypeScript). See [worker/README.md](worker/README.md) |
| `agent/` | The `toprelay` Go CLI and daemon. See [agent/README.md](agent/README.md) |
| `docs/protocol.md` | The exact contract between the two: frames, keys, routes, limits, test vectors |
| `docs/brief.md` | Product brief and build plan |
| `docs/research/` | Research notes behind the design |
| `spike/oauth/` | Throwaway OAuth test against claude.ai |

## License

[Apache-2.0](LICENSE)
