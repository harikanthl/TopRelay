// End-to-end OAuth flow in workerd: discovery → DCR → consent with pairing code → token → MCP call.
// CIMD can't be exercised here (it fetches a public https document); that part is tested from claude.ai.
import { describe, it, expect } from "vitest";
import { exports } from "cloudflare:workers";

const BASE = "http://localhost";
const RESOURCE = `${BASE}/mcp`;
const REDIRECT = "http://127.0.0.1:9999/callback";

// Real clients always send Host; the MCP handler's Host check needs it.
const call = (path: string, init: RequestInit = {}) => {
  const headers = new Headers(init.headers);
  headers.set("Host", new URL(BASE).host);
  return (exports as unknown as { default: Fetcher }).default.fetch(new Request(`${BASE}${path}`, { ...init, headers }));
};

const b64url = (bytes: ArrayBuffer | Uint8Array) =>
  btoa(String.fromCharCode(...new Uint8Array(bytes))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

async function pkce() {
  const verifier = b64url(crypto.getRandomValues(new Uint8Array(32)));
  const challenge = b64url(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier)));
  return { verifier, challenge };
}

// Keep only name=value from each Set-Cookie, as a browser would send them back.
const cookiesFrom = (res: Response) => res.headers.getSetCookie().map((c) => c.split(";")[0]).join("; ");

async function readMcp(res: Response): Promise<any> {
  const text = await res.text();
  if (res.headers.get("content-type")?.includes("text/event-stream")) {
    const data = text.split("\n").filter((l) => l.startsWith("data:")).map((l) => l.slice(5).trim());
    return JSON.parse(data[data.length - 1]);
  }
  return JSON.parse(text);
}

async function registerClient() {
  const res = await call("/oauth/register", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ client_name: "Spike Test Client", redirect_uris: [REDIRECT], token_endpoint_auth_method: "none" }),
  });
  expect(res.status).toBe(201);
  return (await res.json<{ client_id: string }>()).client_id;
}

async function openConsent(clientId: string, challenge: string) {
  const q = new URLSearchParams({
    response_type: "code",
    client_id: clientId,
    redirect_uri: REDIRECT,
    code_challenge: challenge,
    code_challenge_method: "S256",
    state: "state-123",
    scope: "mcp:tools",
    resource: RESOURCE,
  });
  const res = await call(`/authorize?${q}`);
  const html = await res.text();
  expect(res.status, html).toBe(200);
  const handle = html.match(/name="handle" value="([^"]+)"/)?.[1];
  expect(handle).toBeTruthy();
  return { handle: handle!, cookie: cookiesFrom(res), html };
}

const submit = (handle: string, cookie: string, fields: Record<string, string>) =>
  call("/authorize", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded", Cookie: cookie },
    body: new URLSearchParams({ handle, scope: "mcp:tools", ...fields }),
    redirect: "manual",
  });

describe("discovery", () => {
  it("publishes protected-resource metadata for /mcp", async () => {
    const res = await call("/.well-known/oauth-protected-resource/mcp");
    expect(res.status).toBe(200);
    const meta = await res.json<any>();
    expect(meta.resource).toBe(RESOURCE);
    expect(meta.scopes_supported).toEqual(["mcp:tools"]);
  });

  it("publishes authorization-server metadata with DCR and PKCE", async () => {
    const meta = await (await call("/.well-known/oauth-authorization-server")).json<any>();
    expect(meta.registration_endpoint).toBe(`${BASE}/oauth/register`);
    expect(meta.code_challenge_methods_supported).toContain("S256");
    // Advertised only when the flag and the option are both on.
    expect(meta.client_id_metadata_document_supported).toBe(true);
  });

  it("challenges an unauthenticated MCP call", async () => {
    const res = await call("/mcp", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
    expect(res.status).toBe(401);
    expect(res.headers.get("www-authenticate")).toContain("resource_metadata=");
  });
});

describe("consent and tokens", () => {
  it("shows the client and redirect host, and refuses a wrong pairing code", async () => {
    const clientId = await registerClient();
    const { challenge } = await pkce();
    const { handle, cookie, html } = await openConsent(clientId, challenge);
    expect(html).toContain("Spike Test Client");
    expect(html).toContain("not verified");
    expect(html).toContain("127.0.0.1");
    const res = await submit(handle, cookie, { decision: "approve", code: "wrong" });
    expect(res.status).toBe(403);
  });

  it("escapes a hostile client name", async () => {
    const res = await call("/oauth/register", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ client_name: '<script>alert(1)</script>', redirect_uris: [REDIRECT], token_endpoint_auth_method: "none" }),
    });
    const { client_id } = await res.json<{ client_id: string }>();
    const { html } = await openConsent(client_id, (await pkce()).challenge);
    expect(html).not.toContain("<script>alert(1)</script>");
  });

  it("issues a token after the right code, and the token works on /mcp", async () => {
    const clientId = await registerClient();
    const { verifier, challenge } = await pkce();
    const { handle, cookie } = await openConsent(clientId, challenge);

    const approved = await submit(handle, cookie, { decision: "approve", code: "test-pair-code" });
    expect(approved.status).toBe(302);
    const location = new URL(approved.headers.get("location")!);
    expect(location.origin + location.pathname).toBe(REDIRECT);
    expect(location.searchParams.get("state")).toBe("state-123");
    const code = location.searchParams.get("code")!;
    expect(code).toBeTruthy();

    const tokenRes = await call("/oauth/token", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({
        grant_type: "authorization_code",
        code,
        redirect_uri: REDIRECT,
        client_id: clientId,
        code_verifier: verifier,
        resource: RESOURCE,
      }),
    });
    expect(tokenRes.status).toBe(200);
    const { access_token } = await tokenRes.json<{ access_token: string }>();

    const listRes = await call("/mcp", {
      method: "POST",
      headers: {
        Authorization: `Bearer ${access_token}`,
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        "MCP-Protocol-Version": "2025-11-25",
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list", params: {} }),
    });
    const body = await readMcp(listRes);
    expect(listRes.status, JSON.stringify(body)).toBe(200);
    const names = body.result.tools.map((t: { name: string }) => t.name);
    expect(names).toEqual(expect.arrayContaining(["hello", "whoami", "confirm_probe"]));

    const helloRes = await call("/mcp", {
      method: "POST",
      headers: {
        Authorization: `Bearer ${access_token}`,
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        "MCP-Protocol-Version": "2025-11-25",
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "hello", arguments: { name: "test" } } }),
    });
    const hello = await readMcp(helloRes);
    expect(hello.result.content[0].text).toContain("Hello, test");
  });

  it("rejects a forged token on /mcp", async () => {
    const res = await call("/mcp", {
      method: "POST",
      headers: { Authorization: "Bearer not-a-real-token", "Content-Type": "application/json" },
      body: "{}",
    });
    expect(res.status).toBe(401);
  });
});
