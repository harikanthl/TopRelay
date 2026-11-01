# OAuth spike

Throwaway Worker for build step 2 of [`docs/brief.md`](../../docs/brief.md). It puts
`@cloudflare/workers-oauth-provider` (CIMD + DCR) in front of a hello-world MCP server, with a consent page that
asks for a pairing code. Once the questions below are answered, the real relay is built in `worker/` and this
folder can be deleted.

## Questions it answers

1. Does claude.ai finish the OAuth flow with **CIMD** ("Use Claude's published identity")? With **DCR**
   ("Register automatically")?
2. Is a pairing code on the consent page enough for one owner with no user accounts?
3. Does a token check stay under Workers Free's **10 ms CPU** per request?
4. Which headers do bots really send (`MCP-Protocol-Version`, `Mcp-Method`, `Mcp-Name`)? The Worker allow-list in
   the brief depends on `Mcp-Name`.
5. Does claude.ai support MCP input requests (MRTR)? Phone approval in v1.1 depends on it. The `confirm_probe`
   tool tests it.

## Run the tests

```sh
npm install
npm test          # 7 tests in workerd: discovery, DCR, consent, wrong code, escaping, token, MCP call
```

CIMD can't be tested locally (it fetches the client's public metadata document), so it's tested from claude.ai.

## Deploy to your Cloudflare account

```sh
npx wrangler kv namespace create OAUTH_KV     # paste the id into wrangler.jsonc
npx wrangler deploy                           # prints https://toprelay-oauth-spike.<subdomain>.workers.dev
# put that URL + /mcp into MCP_RESOURCE in wrangler.jsonc, then:
npx wrangler deploy
openssl rand -base64 18                       # a pairing code; keep it handy
npx wrangler secret put PAIR_CODE             # paste it
curl https://toprelay-oauth-spike.<subdomain>.workers.dev/health   # → ok
```

## Test from claude.ai

1. In claude.ai, add a custom connector with the URL `https://toprelay-oauth-spike.<subdomain>.workers.dev/mcp`.
   For the OAuth client, choose **Use Claude's published identity** (CIMD).
2. On the consent page, check that it shows Claude's verified domain. Enter the pairing code and click **Allow**.
3. In a chat, ask Claude to:
   - call `hello`;
   - call `whoami`;
   - call `confirm_probe`. If claude.ai asks you to confirm, MRTR works. If Claude says approval prompts aren't
     supported, it doesn't.
4. Remove the connector, add it again with **Register automatically** (DCR), and repeat.
5. Watch the logs while you do this: `npx wrangler tail --format pretty`. Every `/mcp` call logs the protocol
   version, `Mcp-Method`, `Mcp-Name` and the user agent. Bodies and tokens are never logged.
6. For CPU time, open the Cloudflare dashboard → Workers & Pages → `toprelay-oauth-spike` → Metrics → CPU time.
   Note the median and the 99th percentile.

Optional: the same URL works from the Claude API MCP connector and ChatGPT, if the account allows custom MCP.

## Results

Fill this in from real runs, then copy what matters into the brief's compatibility table.

| Check | CIMD | DCR |
|---|---|---|
| claude.ai finishes OAuth | | |
| Consent page shows the right client | | |
| `hello` / `whoami` work | | |
| `confirm_probe` prompts the user (MRTR) | | |
| Headers seen (`MCP-Protocol-Version`, `Mcp-Method`, `Mcp-Name`) | | |
| CPU time, `/mcp` call (median / p99) | | |
| CPU time, `/oauth/token` (median / p99) | | |

## Known gaps (fine for a spike, not for the relay)

- Wrong pairing codes aren't rate-limited. The relay will limit attempts with GCRA in the Durable Object.
- A single fixed pairing code. The relay will issue a short-lived one with `toprelay pair <service>`.
- No revocation UI.
