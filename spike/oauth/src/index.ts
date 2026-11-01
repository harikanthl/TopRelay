// TopRelay OAuth spike. Throwaway: answers build step 2 of docs/brief.md, then the real relay is built in worker/.
//
//   GET  /                          → what this is
//   GET  /health                    → "ok"
//   GET  /authorize                 → consent page (owner types the pairing code)
//   POST /authorize                 → approve / deny
//   *    /mcp                       → hello-world MCP server, behind OAuth
//   /.well-known/…, /oauth/token, /oauth/register → served by workers-oauth-provider
//
// Logs (no bodies, no tokens): method, MCP-Protocol-Version, Mcp-Method, Mcp-Name, user agent, client id, so we can
// see what each bot actually sends.

import { OAuthProvider, AuthorizationError, CimdFetchError } from "@cloudflare/workers-oauth-provider";
import type { ConsentDescription, OAuthHelpers } from "@cloudflare/workers-oauth-provider";
import { McpServer, inputRequired, acceptedContent } from "@modelcontextprotocol/server";
import { createMcpHandler, getMcpAuthContext } from "agents/mcp/server";
import { env } from "cloudflare:workers";
import { z } from "zod";

export interface Env {
  OAUTH_KV: KVNamespace;
  MCP_RESOURCE: string;
  PAIR_CODE: string;
  OAUTH_PROVIDER: OAuthHelpers;
}

const OWNER = "owner"; // single owner: no user accounts

// ── MCP server ──────────────────────────────────────────────────────────────────────────────────────────────────

function createServer() {
  const server = new McpServer({ name: "toprelay-spike", version: "0.0.1" });

  server.registerTool(
    "hello",
    {
      title: "Say hello",
      description: "Returns a greeting. Harmless; use it to check the connection works.",
      inputSchema: z.object({ name: z.string().optional().describe("Who to greet") }),
      annotations: { readOnlyHint: true },
    },
    async ({ name }) => ({ content: [{ type: "text", text: `Hello, ${name ?? "world"}, from TopRelay!` }] }),
  );

  server.registerTool(
    "whoami",
    {
      title: "Show the OAuth client",
      description: "Shows which OAuth client and scopes this call was made with.",
      inputSchema: z.object({}),
      annotations: { readOnlyHint: true },
    },
    async (_args, ctx) => {
      const auth = getMcpAuthContext();
      const info = { userId: auth?.props?.userId, clientId: ctx.http?.authInfo?.clientId, scopes: ctx.http?.authInfo?.scopes };
      return { content: [{ type: "text", text: JSON.stringify(info) }] };
    },
  );

  // Probe for the phone-approval idea (v1.1): does the client support MCP 2026-07-28 input requests (MRTR)?
  // A client that supports it asks the user to confirm and retries; one that doesn't gets the fallback text.
  server.registerTool(
    "confirm_probe",
    {
      title: "Approval probe",
      description: "Asks the user to confirm before answering. Used to test whether this client supports approval prompts.",
      inputSchema: z.object({}),
      annotations: { readOnlyHint: true },
    },
    async (_args, ctx) => {
      const schema = z.object({ confirm: z.boolean() });
      const answer = acceptedContent(ctx.mcpReq.inputResponses, "confirm", schema);
      if (answer === undefined) {
        console.log(JSON.stringify({ probe: "confirm_probe", step: "input_required" }));
        return inputRequired({
          inputRequests: {
            confirm: inputRequired.elicit({ message: "TopRelay spike: allow this test call?", requestedSchema: schema }),
          },
        });
      }
      console.log(JSON.stringify({ probe: "confirm_probe", step: "answered", confirm: answer.confirm }));
      return { content: [{ type: "text", text: answer.confirm ? "Approved: this client supports approval prompts." : "Declined by the user." }] };
    },
  );

  return server;
}

const mcpHandler = createMcpHandler(createServer);

const apiHandler = {
  async fetch(request: Request, env: Env, ctx: ExecutionContext) {
    const h = request.headers;
    console.log(
      JSON.stringify({
        mcp: true,
        method: request.method,
        protocolVersion: h.get("mcp-protocol-version"),
        mcpMethod: h.get("mcp-method"),
        mcpName: h.get("mcp-name"),
        userAgent: h.get("user-agent"),
        clientId: (ctx as ExecutionContext & { auth?: { clientId?: string } }).auth?.clientId,
      }),
    );
    return mcpHandler(request, env, ctx);
  },
};

// ── Consent page with a pairing code ────────────────────────────────────────────────────────────────────────────

const escape = (value: string) => value.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);

const page = (title: string, body: string, status = 200, headers = new Headers()) => {
  headers.set("Content-Type", "text/html; charset=utf-8");
  headers.set("Cache-Control", "no-store");
  return new Response(
    `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>${escape(title)}</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:2rem auto;padding:0 1rem;color:#1a1a1a;background:#fff}
@media (prefers-color-scheme:dark){body{color:#eee;background:#141414}input{background:#222;color:#eee}}
input[type=text]{font:inherit;padding:.4rem;width:100%;box-sizing:border-box}button{font:inherit;padding:.4rem 1rem;margin-right:.5rem}
.warn{border-left:3px solid #c80;padding-left:.75rem}</style>
${body}`,
    { status, headers },
  );
};

function consentPage(details: ConsentDescription, handle: string) {
  const name = escape(details.clientName);
  const origin = details.clientDomain
    ? `Published by <strong>${escape(details.clientDomain)}</strong> (verified: it hosts its own metadata document).`
    : "This app registered itself; its name is <strong>not verified</strong>.";
  const scopes = details.scope.map((s) => `<input type="hidden" name="scope" value="${escape(s)}">${escape(s)}`).join(", ");
  return `<h1>Allow ${name} to use your TopRelay spike?</h1>
<p>${origin}</p>
<p>Access will be sent to <strong>${escape(details.redirectHost)}</strong>.</p>
${details.redirectIsLoopback ? '<p class="warn"><strong>This sends access to an app on a computer.</strong> Continue only if you just started connecting from it.</p>' : ""}
<p>Scopes: ${scopes || "(none)"}</p>
<form method="post">
  <input type="hidden" name="handle" value="${escape(handle)}">
  <label>Pairing code<br><input type="text" name="code" autocomplete="off" autofocus required></label>
  <p><button name="decision" value="approve">Allow</button><button name="decision" value="deny" formnovalidate>Deny</button></p>
</form>`;
}

// Constant-time comparison of two strings of any length (hash first so lengths match).
async function sameSecret(a: string, b: string) {
  const enc = new TextEncoder();
  const [x, y] = await Promise.all([crypto.subtle.digest("SHA-256", enc.encode(a)), crypto.subtle.digest("SHA-256", enc.encode(b))]);
  return crypto.subtle.timingSafeEqual(x, y);
}

async function authorize(request: Request, env: Env): Promise<Response> {
  const oauth = env.OAUTH_PROVIDER;
  try {
    if (request.method === "GET") {
      const authRequest = await oauth.parseAuthRequest(request);
      const details = await oauth.describeConsent(authRequest); // first: a failed lookup leaves nothing in KV
      console.log(JSON.stringify({ consent: "shown", client: details.clientName, domain: details.clientDomain ?? null, redirectHost: details.redirectHost }));
      const consent = await oauth.beginConsent(authRequest);
      return page(`Authorize ${details.clientName}`, consentPage(details, consent.handle), 200, consent.headers);
    }
    if (request.method === "POST") {
      const form = await request.formData();
      const handle = String(form.get("handle") ?? "");
      if (form.get("decision") !== "approve") {
        const denied = await oauth.denyConsent(request, handle);
        return new Response(null, { status: 302, headers: denied.headers });
      }
      if (!env.PAIR_CODE || !(await sameSecret(String(form.get("code") ?? ""), env.PAIR_CODE))) {
        // The handle is still valid: the owner can go back and retry. The real relay rate-limits attempts (GCRA in the DO).
        console.log(JSON.stringify({ consent: "bad_code" }));
        return page("Wrong pairing code", "<h1>Wrong pairing code</h1><p>Go back and try again.</p>", 403);
      }
      const approved = await oauth.approveConsent(request, handle, { scope: form.getAll("scope").map(String) });
      const { redirectTo } = await oauth.completeAuthorization({
        request: approved.request, // from storage, not from the form
        userId: OWNER,
        metadata: {},
        scope: approved.request.scope,
        props: { userId: OWNER },
      });
      console.log(JSON.stringify({ consent: "approved", clientId: approved.request.clientId }));
      approved.headers.set("Location", redirectTo);
      return new Response(null, { status: 302, headers: approved.headers });
    }
    return new Response("Method not allowed", { status: 405 });
  } catch (error) {
    if (error instanceof AuthorizationError && error.redirectTo) return Response.redirect(error.redirectTo, 302);
    if (error instanceof AuthorizationError || error instanceof CimdFetchError) {
      const message = error instanceof AuthorizationError ? error.description : "This app could not be verified.";
      console.log(JSON.stringify({ consent: "error", kind: error.constructor.name, message }));
      return page("Can't authorize", `<h1>Can't authorize</h1><p>${escape(message ?? "Unknown error")}</p>`, 400);
    }
    throw error;
  }
}

const defaultHandler = {
  async fetch(request: Request, env: Env): Promise<Response> {
    const { pathname } = new URL(request.url);
    if (pathname === "/health") return new Response("ok");
    if (pathname === "/authorize") return authorize(request, env);
    if (pathname === "/") {
      return page("TopRelay OAuth spike", `<h1>TopRelay OAuth spike</h1><p>MCP endpoint: <code>${escape(env.MCP_RESOURCE)}</code></p>`);
    }
    return new Response("Not found", { status: 404 });
  },
};

export default new OAuthProvider<Env>({
  apiRoute: "/mcp",
  apiHandler,
  defaultHandler,
  authorizeEndpoint: "/authorize",
  tokenEndpoint: "/oauth/token",
  clientRegistrationEndpoint: "/oauth/register", // DCR fallback (deprecated in MCP 2026-07-28)
  clientIdMetadataDocumentEnabled: true, // CIMD: claude.ai's recommended "published identity"
  scopesSupported: ["mcp:tools", "offline_access"],
  requiredScopes: ["mcp:tools"],
  resourceMetadata: { resource: env.MCP_RESOURCE },
});
