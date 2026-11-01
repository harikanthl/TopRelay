# TopRelay

**Status: early development. Nothing here is ready to use yet.**

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

## Layout

| Path | What |
|---|---|
| `spike/oauth/` | Throwaway test: OAuth (CIMD + DCR) in front of a hello-world MCP server, to try from claude.ai |
| `worker/` | The relay Worker and Durable Object (coming) |
| `agent/` | The `toprelay` Go CLI and daemon (coming) |
| `docs/brief.md` | Product brief and build plan |
| `docs/research/` | Research notes behind the design |

## License

[Apache-2.0](LICENSE)
