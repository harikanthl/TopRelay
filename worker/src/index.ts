// TopRelay relay Worker. Deployed by the toprelay agent to the owner's own Cloudflare account.
// Authenticates and routes; the Relay Durable Object does the relaying. Contract: docs/protocol.md.

import type { OAuthResourceContext } from "@cloudflare/workers-oauth-provider";
import type { Config, Env, Service } from "./config";
import { loadConfig } from "./config";
import { INTERNAL, REQUEST_HEADERS } from "./headers";
import { checkAgentKey, verifyFileUrl } from "./keys";
import type { RelayProps } from "./oauth";
import { authorize, oauthFor, revokeServiceGrants } from "./oauth";

export { Relay } from "./relay";

const text = (body: string, status: number, headers: Record<string, string> = {}) =>
  new Response(body, { status, headers: { "content-type": "text/plain; charset=utf-8", "cache-control": "no-store", ...headers } });

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json", "cache-control": "no-store" } });

const relayStub = (env: Env, service: Service) => env.RELAY.get(env.RELAY.idFromName(service.name));

/** Forwards an authenticated bot request to the service's Durable Object. The body is streamed, never read. */
async function forward(request: Request, env: Env, service: Service, ctx: OAuthResourceContext<RelayProps>): Promise<Response> {
  const url = new URL(request.url);
  const headers = new Headers();
  for (const name of REQUEST_HEADERS) {
    const value = request.headers.get(name);
    if (value !== null) headers.set(name, value);
  }
  const length = request.headers.get("content-length");
  if (length !== null) headers.set("content-length", length);
  headers.set(INTERNAL.service, service.name);
  headers.set(INTERNAL.kind, service.kind);
  headers.set(INTERNAL.method, request.method);
  headers.set(INTERNAL.path, url.pathname.slice(1 + service.name.length) || "/");
  headers.set(INTERNAL.query, url.search.replace(/^\?/, ""));
  headers.set(INTERNAL.ip, request.headers.get("cf-connecting-ip") ?? "");
  headers.set(INTERNAL.host, url.host);
  headers.set(INTERNAL.client, ctx.auth.clientId ?? "");
  if (ctx.props.keyId) headers.set(INTERNAL.keyId, ctx.props.keyId);
  if (ctx.props.tools) headers.set(INTERNAL.tools, JSON.stringify(ctx.props.tools));

  const hasBody = request.method !== "GET" && request.method !== "HEAD";
  return relayStub(env, service).fetch("https://relay/relay", {
    method: "POST",
    headers,
    body: hasBody ? request.body : null,
    signal: request.signal,
  });
}

function bearerFrom(request: Request): string {
  const auth = request.headers.get("authorization") ?? "";
  return auth.toLowerCase().startsWith("bearer ") ? auth.slice(7).trim() : "";
}

async function agentRoute(request: Request, env: Env, config: Config, service: Service, rest: string[]): Promise<Response> {
  if (!(await checkAgentKey(env.AGENT_ROOT, service.name, service.gen, bearerFrom(request)))) {
    return text("invalid agent key", 401, { "www-authenticate": 'Bearer realm="toprelay-agent"' });
  }
  const stub = relayStub(env, service);
  const internal = new Headers({ [INTERNAL.service]: service.name, "content-type": "application/json" });

  if (rest[0] === "connect" && rest.length === 1) {
    if (request.method !== "GET") return text("method not allowed", 405);
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") return text("expected a WebSocket upgrade", 426);
    const upgrade = new Headers(request.headers);
    upgrade.delete("authorization");
    upgrade.set(INTERNAL.service, service.name);
    return stub.fetch("https://relay/connect", { method: "GET", headers: upgrade });
  }

  if (rest[0] === "admin" && rest.length === 2) {
    const op = rest[1];
    if (op === "status" && request.method === "GET") return stub.fetch("https://relay/admin/status", { headers: internal });
    if ((op === "pair" || op === "revoke-key") && request.method === "POST") {
      return stub.fetch(`https://relay/admin/${op}`, { method: "POST", headers: internal, body: await request.text() });
    }
    if (op === "revoke-oauth" && request.method === "POST") {
      return json({ revoked: await revokeServiceGrants(oauthFor(config, forward), env, service.name) });
    }
    return text("unknown admin operation", 404);
  }
  return text("not found", 404);
}

/** A signed, single-use download link (docs/protocol.md "Files"). The signature is the credential, so bots can
 * fetch it without OAuth; it's checked here, before any Durable Object call. */
async function fileRoute(request: Request, env: Env, service: Service, id: string): Promise<Response> {
  if (request.method !== "GET") return text("method not allowed", 405);
  const url = new URL(request.url);
  const v = await verifyFileUrl(env.BEARER_ROOT, service.name, service.gen, id, url.searchParams.get("e"), url.searchParams.get("s"), Math.floor(Date.now() / 1000));
  if (!v.ok) return text(v.reason, v.status);
  const headers = new Headers({
    [INTERNAL.service]: service.name,
    [INTERNAL.fileId]: id,
    [INTERNAL.fileExp]: String(v.exp),
    [INTERNAL.ip]: request.headers.get("cf-connecting-ip") ?? "",
    [INTERNAL.host]: url.host,
  });
  return relayStub(env, service).fetch("https://relay/file", { method: "POST", headers, signal: request.signal });
}

export default {
  async fetch(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
    const url = new URL(request.url);
    if (url.pathname === "/health") return text("ok", 200);
    if (request.method === "OPTIONS") return text("method not allowed", 405);

    let config: Config;
    try {
      config = loadConfig(env);
    } catch (error) {
      console.error(JSON.stringify({ error: "config", message: (error as Error).message }));
      return text("This relay is misconfigured. Run `toprelay doctor` on the owner's computer.", 500);
    }
    const setup = oauthFor(config, forward);
    const segments = url.pathname.split("/").filter(Boolean);

    if (url.pathname === "/authorize") return authorize(request, env, config, setup);
    if (url.pathname === "/.well-known/oauth-authorization-server" || segments[0] === "oauth") {
      return setup.authServer ? setup.authServer.fetch(request, env, ctx) : text("not found", 404);
    }
    if (segments[0] === ".well-known" && segments[1] === "oauth-protected-resource") {
      const resource = setup.resources.get(segments[2] ?? "");
      return resource ? resource.fetch(request, env, ctx) : text("not found", 404);
    }

    // Empty segments and encoded slashes or backslashes would reach the agent as a different path than the one
    // routed here (`//host/x` even parses as a host), so they're refused outright.
    if (url.pathname.includes("//") || /%2f|%5c/i.test(url.pathname)) return text("bad path", 400);

    const service = config.services.get(segments[0] ?? "");
    if (!service) return text("not found", 404);
    const rest = segments.slice(1);
    if (rest[0] === "connect" || rest[0] === "admin") return agentRoute(request, env, config, service, rest);
    // Only mcp services: under an http service every path is the app's.
    if (service.kind === "mcp" && rest[0] === "files" && rest.length === 2) return fileRoute(request, env, service, rest[1]);

    if (service.kind === "mcp" && rest[0] !== "mcp") return text("not found", 404);
    return setup.resources.get(service.name)!.fetch(request, env, ctx);
  },
} satisfies ExportedHandler<Env>;
