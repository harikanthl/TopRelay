// OAuth for bots: one authorization server (issuer PUBLIC_URL), one protected resource per service, and a
// consent page where the owner types a pairing code from `toprelay pair <service>` (docs/protocol.md "OAuth").

import {
  AuthorizationError,
  CimdFetchError,
  OAuthAuthorizationServer,
  OAuthResourceServer,
  insufficientScope,
} from "@cloudflare/workers-oauth-provider";
import type { ConsentDescription, OAuthResourceContext } from "@cloudflare/workers-oauth-provider";
import type { Config, Env, Service } from "./config";
import { resourceFor } from "./config";
import { INTERNAL } from "./headers";
import { verifyBearer } from "./keys";

export interface RelayProps {
  service: string;
  /** Bearer key id, when the caller used a key rather than OAuth. */
  keyId?: string;
  /** Tool restriction from the key's caveats. */
  tools?: string[];
}

export type Forward = (request: Request, env: Env, service: Service, ctx: OAuthResourceContext<RelayProps>) => Promise<Response>;

export interface OAuthSetup {
  authServer: OAuthAuthorizationServer<Env> | null;
  resources: Map<string, OAuthResourceServer<Env, RelayProps>>;
}

export const SCOPES = ["mcp", "offline_access"];

let cached: { config: Config; setup: OAuthSetup } | undefined;

export function oauthFor(config: Config, forward: Forward): OAuthSetup {
  if (cached?.config === config) return cached.setup;
  const issuer = config.publicUrl;
  const services = [...config.services.values()];
  const authServer =
    services.length === 0
      ? null
      : new OAuthAuthorizationServer<Env>({
          issuer,
          resources: services.map((s) => resourceFor(issuer, s)),
          scopesSupported: SCOPES,
          clientIdMetadataDocumentEnabled: true,
          clientRegistrationEndpoint: "/oauth/register",
          accessTokenTTL: 3600,
          refreshTokenTTL: 30 * 86400,
          refreshTokenIdleTTL: 7 * 86400,
        });

  const resources = new Map<string, OAuthResourceServer<Env, RelayProps>>();
  for (const service of services) {
    const resource = resourceFor(issuer, service);
    resources.set(
      service.name,
      new OAuthResourceServer<Env, RelayProps>({
        resourceMetadata: {
          resource,
          authorization_servers: [issuer],
          bearer_methods_supported: ["header"],
          resource_name: `TopRelay: ${service.name}`,
        },
        requiredScopes: ["mcp"],
        validateToken: (env) => async (res, token) => {
          if (token.startsWith("trk1.")) {
            const result = await verifyBearer(env.BEARER_ROOT, token, service.name, service.gen, Math.floor(Date.now() / 1000));
            if (!result.ok) return null;
            const { id, expiresAt, tools } = result.claims;
            return { props: { service: service.name, keyId: id, tools }, audience: res, expiresAt, scope: ["mcp"], clientId: `key:${id}` };
          }
          const validated = await authServer!.validateToken<RelayProps>(res, token, env);
          if (!validated || validated.props?.service !== service.name) return null;
          return {
            props: { service: service.name },
            audience: validated.audience,
            expiresAt: validated.expiresAt,
            scope: validated.scope,
            userId: validated.userId,
            clientId: validated.clientId,
          };
        },
        handler: {
          fetch(request, env, ctx) {
            if (!ctx.auth.scope.includes("mcp")) return insufficientScope(ctx.auth, ["mcp"]);
            return forward(request, env, service, ctx);
          },
        },
      }),
    );
  }

  const setup = { authServer, resources };
  cached = { config, setup };
  return setup;
}

/** Finds the service an authorization request's resource names. */
export function serviceFromResource(config: Config, resource: string | undefined): Service | undefined {
  if (!resource) return undefined;
  for (const service of config.services.values()) if (resourceFor(config.publicUrl, service) === resource) return service;
  return undefined;
}

/** Revokes every OAuth grant for a service. Returns how many were revoked. */
export async function revokeServiceGrants(setup: OAuthSetup, env: Env, service: string): Promise<number> {
  if (!setup.authServer) return 0;
  const helpers = setup.authServer.getOAuthApi(env);
  const userId = `svc-${service}`;
  let revoked = 0;
  let cursor: string | undefined;
  do {
    const page = await helpers.listUserGrants(userId, cursor ? { cursor } : undefined);
    for (const grant of page.items) {
      await helpers.revokeGrant(grant.id, userId);
      revoked++;
    }
    cursor = page.cursor;
  } while (cursor);
  return revoked;
}

// ── consent page ──────────────────────────────────────────────────────────────────────────────────────────────

const escape = (value: string) => value.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);

function page(title: string, body: string, status = 200, headers = new Headers()): Response {
  headers.set("content-type", "text/html; charset=utf-8");
  headers.set("cache-control", "no-store");
  headers.set("x-frame-options", "DENY");
  headers.set("content-security-policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'");
  headers.set("referrer-policy", "no-referrer");
  return new Response(
    `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>${escape(title)}</title>
<style>
:root{--fg:#1b1b1f;--muted:#5c5c66;--bg:#fafafa;--card:#fff;--line:#e3e3e8;--accent:#1f5eff;--warn:#b25e00}
@media (prefers-color-scheme:dark){:root{--fg:#ececf1;--muted:#a0a0ab;--bg:#111114;--card:#1a1a1f;--line:#2c2c33;--accent:#6e95ff;--warn:#f0a040}}
*{box-sizing:border-box}body{margin:0;font:16px/1.5 system-ui,-apple-system,sans-serif;color:var(--fg);background:var(--bg)}
main{max-width:30rem;margin:3rem auto;padding:0 16px}.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:1.5rem}
h1{font-size:1.25rem;margin:0 0 1rem}p{margin:.5rem 0}.muted{color:var(--muted);font-size:.9rem}.warn{color:var(--warn)}
label{display:block;margin-top:1rem;font-weight:600}input[type=text]{width:100%;font:inherit;padding:.6rem;margin-top:.35rem;border:1px solid var(--line);border-radius:8px;background:var(--bg);color:var(--fg);letter-spacing:.08em}
.row{display:flex;gap:.5rem;margin-top:1.25rem}button{font:inherit;padding:.55rem 1.1rem;border-radius:8px;border:1px solid var(--line);background:var(--card);color:var(--fg);cursor:pointer}
button.primary{background:var(--accent);border-color:var(--accent);color:#fff}code{font-size:.9em}
</style>
<main><div class="card">${body}</div><p class="muted" style="text-align:center">TopRelay</p></main></html>`,
    { status, headers },
  );
}

function consentBody(service: Service, details: ConsentDescription, handle: string, error?: string): string {
  const name = escape(details.clientName);
  const origin = details.clientDomain
    ? `Published by <strong>${escape(details.clientDomain)}</strong>.`
    : `<span class="warn">This app registered itself; its name is not verified.</span>`;
  return `<h1>Allow ${name} to use <code>${escape(service.name)}</code>?</h1>
<p>${origin}</p>
<p>Access will be sent to <strong>${escape(details.redirectHost)}</strong>.</p>
${details.redirectIsLoopback ? `<p class="warn">This sends access to an app on a computer. Continue only if you just started connecting from it.</p>` : ""}
<p class="muted">It will be able to call the tools you allowed for <code>${escape(service.name)}</code>.</p>
${error ? `<p class="warn" role="alert">${escape(error)}</p>` : ""}
<form method="post">
  <input type="hidden" name="handle" value="${escape(handle)}">
  <input type="hidden" name="service" value="${escape(service.name)}">
  <label for="code">Pairing code</label>
  <input type="text" id="code" name="code" autocomplete="off" autocapitalize="off" spellcheck="false" autofocus>
  <p class="muted">Run <code>toprelay pair ${escape(service.name)}</code> on your computer to get one.</p>
  <div class="row"><button class="primary" name="decision" value="approve">Allow</button><button name="decision" value="deny">Deny</button></div>
</form>`;
}

const PAIR_ERRORS: Record<string, string> = {
  wrong_code: "That pairing code is wrong. Check it and try again.",
  no_code: "There's no active pairing code. Run toprelay pair on your computer, then try again.",
  too_many_attempts: "Too many attempts. Wait a few minutes, then try again.",
};

async function checkPairCode(env: Env, service: Service, code: string, ip: string): Promise<{ ok: boolean; reason?: string }> {
  const stub = env.RELAY.get(env.RELAY.idFromName(service.name));
  const res = await stub.fetch("https://relay/pair-verify", {
    method: "POST",
    headers: { "content-type": "application/json", [INTERNAL.ip]: ip },
    body: JSON.stringify({ code }),
  });
  return res.json();
}

export async function authorize(request: Request, env: Env, config: Config, setup: OAuthSetup): Promise<Response> {
  if (!setup.authServer) return page("Nothing to authorize", "<h1>Nothing to authorize</h1><p>No services are set up on this relay.</p>", 404);
  const oauth = setup.authServer.getOAuthApi(env);
  try {
    if (request.method === "GET") {
      const authRequest = await oauth.parseAuthRequest(request);
      const service = serviceFromResource(config, authRequest.resource);
      if (!service) return page("Unknown service", "<h1>Unknown service</h1><p>This relay doesn't serve that resource.</p>", 400);
      const details = await oauth.describeConsent(authRequest);
      const consent = await oauth.beginConsent(authRequest);
      return page(`Allow ${details.clientName}?`, consentBody(service, details, consent.handle), 200, consent.headers);
    }

    if (request.method === "POST") {
      const form = await request.formData();
      const handle = String(form.get("handle") ?? "");
      if (form.get("decision") !== "approve") {
        const denied = await oauth.denyConsent(request, handle);
        return new Response(null, { status: 302, headers: denied.headers });
      }
      // Validate the browser's consent session first (handle + cookie), so a request without a real consent page
      // can't spend pairing attempts or the owner's code. Approving consumes the handle; on a wrong code the page is
      // shown again with a fresh one.
      const approved = await oauth.approveConsent(request, handle, { scope: ["mcp", "offline_access"] });
      const service = serviceFromResource(config, approved.request.resource);
      if (!service || service.name !== String(form.get("service") ?? "")) {
        return page("Can't authorize", "<h1>Can't authorize</h1><p>The request doesn't match this service. Start connecting again.</p>", 400);
      }

      const check = await checkPairCode(env, service, String(form.get("code") ?? ""), request.headers.get("cf-connecting-ip") ?? "");
      if (!check.ok) {
        const message = PAIR_ERRORS[check.reason ?? ""] ?? "The pairing code couldn't be checked.";
        const details = await oauth.describeConsent(approved.request);
        const again = await oauth.beginConsent(approved.request);
        for (const [name, value] of approved.headers) if (name.toLowerCase() === "set-cookie") again.headers.append(name, value);
        return page(`Allow ${details.clientName}?`, consentBody(service, details, again.handle, message), 403, again.headers);
      }

      // Always grant mcp plus offline_access, so bots can refresh instead of asking the owner again every hour.
      const { redirectTo } = await oauth.completeAuthorization({
        request: approved.request,
        userId: `svc-${service.name}`,
        metadata: { service: service.name },
        scope: approved.request.scope,
        props: { service: service.name } satisfies RelayProps,
      });
      approved.headers.set("location", redirectTo);
      return new Response(null, { status: 302, headers: approved.headers });
    }

    return new Response("method not allowed", { status: 405, headers: { allow: "GET, POST" } });
  } catch (error) {
    if (error instanceof AuthorizationError && error.redirectTo) return Response.redirect(error.redirectTo, 302);
    if (error instanceof AuthorizationError || error instanceof CimdFetchError) {
      const message = error instanceof AuthorizationError ? (error.description ?? "This request isn't valid.") : "This app could not be verified.";
      return page("Can't authorize", `<h1>Can't authorize</h1><p>${escape(message)}</p>`, 400);
    }
    throw error;
  }
}
