// Full OAuth flow through the relay: DCR → consent with pairing code → token → MCP call reaching the agent.
import { afterEach, describe, expect, it } from "vitest";
import { agentKey } from "../src/keys";
import { AGENT_ROOT, FakeAgent, call, mcpHeaders, rpc } from "./helpers";

const REDIRECT = "http://127.0.0.1:9999/callback";
const RESOURCE = "http://localhost/notes/mcp";
const agents: FakeAgent[] = [];
afterEach(() => {
  for (const a of agents.splice(0)) if (!a.closed) a.close();
});

const b64url = (bytes: ArrayBuffer | Uint8Array) =>
  btoa(String.fromCharCode(...new Uint8Array(bytes))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

async function pkce() {
  const verifier = b64url(crypto.getRandomValues(new Uint8Array(32)));
  const challenge = b64url(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier)));
  return { verifier, challenge };
}

const cookiesFrom = (res: Response) => res.headers.getSetCookie().map((c) => c.split(";")[0]).join("; ");

async function pair(service: string, code: string) {
  const res = await call(`/${service}/admin/pair`, {
    method: "POST",
    headers: { authorization: `Bearer ${await agentKey(AGENT_ROOT, service, 1)}`, "content-type": "application/json" },
    body: JSON.stringify({ code }),
  });
  expect(res.status).toBe(200);
}

async function register() {
  const res = await call("/oauth/register", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ client_name: "Test Bot", redirect_uris: [REDIRECT], token_endpoint_auth_method: "none" }),
  });
  expect(res.status).toBe(201);
  return (await res.json<{ client_id: string }>()).client_id;
}

async function openConsent(clientId: string, challenge: string, resource = RESOURCE) {
  const q = new URLSearchParams({
    response_type: "code",
    client_id: clientId,
    redirect_uri: REDIRECT,
    code_challenge: challenge,
    code_challenge_method: "S256",
    state: "st",
    scope: "mcp offline_access",
    resource,
  });
  const res = await call(`/authorize?${q}`);
  const html = await res.text();
  expect(res.status, html).toBe(200);
  return { html, cookie: cookiesFrom(res), handle: html.match(/name="handle" value="([^"]+)"/)![1] };
}

const submit = (cookie: string, fields: Record<string, string>) =>
  call("/authorize", {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded", cookie },
    body: new URLSearchParams(fields),
    redirect: "manual",
  });

async function fullFlow(code: string) {
  const clientId = await register();
  const { verifier, challenge } = await pkce();
  const consent = await openConsent(clientId, challenge);
  const approved = await submit(consent.cookie, { handle: consent.handle, service: "notes", code, decision: "approve" });
  expect(approved.status).toBe(302);
  const location = new URL(approved.headers.get("location")!);
  const tokenRes = await call("/oauth/token", {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "authorization_code",
      code: location.searchParams.get("code")!,
      redirect_uri: REDIRECT,
      client_id: clientId,
      code_verifier: verifier,
      resource: RESOURCE,
    }),
  });
  expect(tokenRes.status).toBe(200);
  const tokens = await tokenRes.json<{ access_token: string; refresh_token?: string; scope: string }>();
  return { clientId, tokens };
}

describe("consent page", () => {
  it("shows the service, the unverified client and the loopback warning, and escapes names", async () => {
    const res = await call("/oauth/register", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ client_name: "<img src=x onerror=alert(1)>", redirect_uris: [REDIRECT], token_endpoint_auth_method: "none" }),
    });
    const { client_id } = await res.json<{ client_id: string }>();
    const { html } = await openConsent(client_id, (await pkce()).challenge);
    expect(html).toContain("<code>notes</code>");
    expect(html).toContain("not verified");
    expect(html).toContain("app on a computer");
    expect(html).not.toContain("<img src=x");
  });

  it("refuses a wrong code and then succeeds with the right one (same page)", async () => {
    await pair("notes", "right-code-1234");
    const clientId = await register();
    const consent = await openConsent(clientId, (await pkce()).challenge);
    const wrong = await submit(consent.cookie, { handle: consent.handle, service: "notes", code: "wrong-code", decision: "approve" });
    expect(wrong.status).toBe(403);
    expect(await wrong.text()).toContain("pairing code is wrong");
    const right = await submit(consent.cookie, { handle: consent.handle, service: "notes", code: "right-code-1234", decision: "approve" });
    expect(right.status).toBe(302);
    expect(new URL(right.headers.get("location")!).searchParams.get("code")).toBeTruthy();
  });

  it("accepts a code typed in lowercase, with spaces or dashes", async () => {
    await pair("notes", "ABCD-EFGH-JKMN");
    const clientId = await register();
    const consent = await openConsent(clientId, (await pkce()).challenge);
    const res = await submit(consent.cookie, { handle: consent.handle, service: "notes", code: " abcd efgh-jkmn ", decision: "approve" });
    expect(res.status).toBe(302);
  });

  it("treats the code as single use", async () => {
    await pair("notes", "single-use-code");
    await fullFlow("single-use-code");
    const clientId = await register();
    const consent = await openConsent(clientId, (await pkce()).challenge);
    const again = await submit(consent.cookie, { handle: consent.handle, service: "notes", code: "single-use-code", decision: "approve" });
    expect(again.status).toBe(403);
    expect(await again.text()).toContain("no active pairing code");
  });

  it("refuses a code from another service's form field", async () => {
    // A valid code for files must not approve a request whose resource is notes.
    const filesKey = await agentKey(AGENT_ROOT, "files", 1);
    await call("/files/admin/pair", { method: "POST", headers: { authorization: `Bearer ${filesKey}`, "content-type": "application/json" }, body: '{"code":"files-code-123"}' });
    const clientId = await register();
    const consent = await openConsent(clientId, (await pkce()).challenge);
    const res = await submit(consent.cookie, { handle: consent.handle, service: "files", code: "files-code-123", decision: "approve" });
    expect(res.status).toBe(400);
  });

  it("denies cleanly", async () => {
    const clientId = await register();
    const consent = await openConsent(clientId, (await pkce()).challenge);
    const res = await submit(consent.cookie, { handle: consent.handle, decision: "deny" });
    expect(res.status).toBe(302);
    expect(new URL(res.headers.get("location")!).searchParams.get("error")).toBe("access_denied");
  });
});

describe("tokens", () => {
  it("reach the agent for their own service only, and stop after revoke-oauth", async () => {
    await pair("notes", "flow-code-5678");
    const { clientId, tokens } = await fullFlow("flow-code-5678");
    expect(tokens.scope.split(" ")).toContain("mcp");
    expect(tokens.refresh_token).toBeTruthy();

    const a = await FakeAgent.connect("notes");
    agents.push(a);
    const pending = call("/notes/mcp", { method: "POST", headers: mcpHeaders(tokens.access_token), body: rpc(1, "tools/list") });
    const req = await a.nextRequest();
    expect(req.headers["x-toprelay-client"]).toBe(clientId);
    a.respondJson(req.id, { jsonrpc: "2.0", id: 1, result: { tools: [] } });
    expect((await pending).status).toBe(200);

    // Audience-bound: no use at another service.
    const elsewhere = await call("/rotated/mcp", { method: "POST", headers: mcpHeaders(tokens.access_token), body: rpc(1, "tools/list") });
    expect(elsewhere.status).toBe(401);

    const revoke = await call("/notes/admin/revoke-oauth", { method: "POST", headers: { authorization: `Bearer ${await agentKey(AGENT_ROOT, "notes", 1)}` } });
    expect((await revoke.json<any>()).revoked).toBeGreaterThanOrEqual(1);
    const after = await call("/notes/mcp", { method: "POST", headers: mcpHeaders(tokens.access_token), body: rpc(2, "tools/list") });
    expect(after.status).toBe(401);
  });
});
