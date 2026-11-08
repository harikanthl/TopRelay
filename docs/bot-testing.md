# Testing TopRelay against real bots

The compatibility tables in `README.md`, `agent/README.md` and `docs/brief.md` may only say "tested" after a run
of these steps, with the evidence kept. Nothing here has been run yet (as of 0.2): every bot is **not tested**.

## Setup (once)

1. A Cloudflare account on the Free plan and an API token (Workers Scripts Edit, Workers KV Storage Edit, Account
   Settings Read). `toprelay login`.
2. A test MCP server with one read-only tool, one destructive tool, one 60-second tool that sends progress, and
   one tool that returns a `resource_link`. The agent's test servers (`internal/daemon/longcall_test.go`,
   `internal/daemon/files_test.go`) show each shape.
3. `toprelay add probe --mcp-url http://127.0.0.1:<port>/mcp --files-dir <dir>` then `toprelay up`.
4. Keep `toprelay logs probe -f --json` running and save its output for every run: it is the evidence (client id,
   tool, status, duration, bytes, reason).

## For every bot, record

| Check | How |
|---|---|
| Connects | the bot lists tools; `logs` shows `tools/list` from its client id |
| Policy | only the read-only tool is listed; calling the destructive one is refused (`reason: tool_not_allowed`) |
| Call | the read-only tool runs and the answer reaches the model |
| Long call | the 60 s tool completes; note whether progress is shown anywhere |
| File link | what the bot does with the `resource_link` (shows it, fetches it: a fetch appears in `logs` as `GET /files/…`) |
| Cancel | stop the bot mid-call; `logs` shows the call ended, the app saw its request end |
| Client id | the exact `client` value, for `toprelay tools --client` rules |

Write down the exact UI path or API request used, the date, the bot's version or model, and anything that
surprised you.

## claude.ai custom connector (OAuth + pairing)

Settings › Connectors › Add custom connector › paste `https://toprelay.<you>.workers.dev/probe/mcp`. When the
consent page asks for a code, run `toprelay pair probe` and type it. Record whether claude.ai used CIMD ("published
identity") or dynamic registration (the `client` value in `logs` is a URL for CIMD), and whether reconnecting
after `toprelay rotate probe` asks for a new code.

## ChatGPT connectors

Whether a custom MCP connector is available depends on the plan and workspace settings; record which. Use the
service URL; OAuth with a pairing code as for claude.ai. Record whether it accepts a bearer key instead
(`toprelay key probe --client openai`), which the brief lists as an open question.

## Grok (consumer connectors) and the xAI API

Consumer: add the URL as a connector if the account offers it; OAuth or bearer as offered. API: the Responses API
remote MCP tool with the URL and a bearer key from `toprelay key probe --client xai`. Per the request that
prompted this runbook, xAI loads every tool into the model's context unless the request names `allowed_tools`:
pass `allowed_tools` with exactly the tools you want, and record the token counts with and without it.

## Claude API `mcp_servers`

`toprelay key probe --client claude-api` prints the request (the MCP connector beta header, `mcp_servers[].url`,
`authorization_token`, and the `mcp_toolset` entry). Record the beta header version you used.

## After a run

Fill in the "Tested" column with the date and a link to the saved log, and add any gotchas here, under the bot.
