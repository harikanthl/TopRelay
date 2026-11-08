import { afterEach, describe, expect, it } from "vitest";
import { agentKey } from "../src/keys";
import { fileSig } from "../src/keys";
import { AGENT_ROOT, BEARER_ROOT, FakeAgent, b64, call, keyId, mcpHeaders, mintKey, rpc, unb64 } from "./helpers";

const agents: FakeAgent[] = [];
async function agent(service: string, opts: Parameters<typeof FakeAgent.connect>[1] = {}) {
  const a = await FakeAgent.connect(service, opts);
  agents.push(a);
  return a;
}
afterEach(() => {
  for (const a of agents.splice(0)) if (!a.closed) a.close();
});

const notesKey = () => mintKey(["svc=notes", "g=1"]);
const adminHeaders = async (service: string, gen = 1) => ({ authorization: `Bearer ${await agentKey(AGENT_ROOT, service, gen)}`, "content-type": "application/json" });

describe("routing", () => {
  it("answers health, refuses OPTIONS and unknown services", async () => {
    expect(await (await call("/health")).text()).toBe("ok");
    expect((await call("/notes/mcp", { method: "OPTIONS" })).status).toBe(405);
    expect((await call("/nope/mcp")).status).toBe(404);
    expect((await call("/notes/other")).status).toBe(404); // mcp kind serves only /mcp
  });

  it("publishes OAuth metadata per service", async () => {
    const as = await (await call("/.well-known/oauth-authorization-server")).json<any>();
    expect(as.issuer).toBe("http://localhost");
    expect(as.protected_resources).toEqual(
      expect.arrayContaining(["http://localhost/notes/mcp", "http://localhost/files", "http://localhost/rotated/mcp"]),
    );
    expect(as.client_id_metadata_document_supported).toBe(true);
    const prm = await (await call("/.well-known/oauth-protected-resource/notes/mcp")).json<any>();
    expect(prm.resource).toBe("http://localhost/notes/mcp");
    expect(prm.scopes_supported).toEqual(["mcp"]);
  });

  it("challenges a call with no credential, and rejects a bad one", async () => {
    const res = await call("/notes/mcp", { method: "POST", body: rpc(1, "tools/list") });
    expect(res.status).toBe(401);
    expect(res.headers.get("www-authenticate")).toContain("resource_metadata=");
    expect((await call("/notes/mcp", { method: "POST", headers: mcpHeaders("trk1.bad.key"), body: "{}" })).status).toBe(401);
    expect((await call("/notes/mcp", { method: "POST", headers: mcpHeaders("not-a-token"), body: "{}" })).status).toBe(401);
  });

  it("rejects a key for another service or an old generation", async () => {
    const forFiles = await mintKey(["svc=files", "g=1"]);
    expect((await call("/notes/mcp", { method: "POST", headers: mcpHeaders(forFiles), body: "{}" })).status).toBe(401);
    const oldGen = await mintKey(["svc=rotated", "g=1"]);
    expect((await call("/rotated/mcp", { method: "POST", headers: mcpHeaders(oldGen), body: "{}" })).status).toBe(401);
  });

  it("requires the agent key on connect and admin", async () => {
    expect((await call("/notes/connect", { headers: { upgrade: "websocket" } })).status).toBe(401);
    expect((await call("/notes/connect", { headers: { upgrade: "websocket", authorization: "Bearer tra1.wrong" } })).status).toBe(401);
    const wrongGen = await agentKey(AGENT_ROOT, "rotated", 1);
    expect((await call("/rotated/admin/status", { headers: { authorization: `Bearer ${wrongGen}` } })).status).toBe(401);
    expect((await call("/notes/connect", { headers: await adminHeaders("notes") })).status).toBe(426);
  });
});

describe("offline", () => {
  it("answers tools/call with a tool error the model can read", async () => {
    const res = await call("/files/x", { headers: { authorization: `Bearer ${await mintKey(["svc=files", "g=1"])}` } });
    expect(res.status).toBe(503);
    expect(await res.text()).toContain("files is offline");
  });

  it("answers MCP calls by method when nobody is connected", async () => {
    // rotated has never had an agent connect
    const key = await mintKey(["svc=rotated", "g=2"]);
    const call1 = await call("/rotated/mcp", { method: "POST", headers: mcpHeaders(key, { "mcp-method": "tools/call", "mcp-name": "x" }), body: rpc(7, "tools/call", { name: "x" }) });
    expect(call1.status).toBe(200);
    const body = await call1.json<any>();
    expect(body.id).toBe(7);
    expect(body.result.isError).toBe(true);
    expect(body.result.content[0].text).toContain("rotated is offline");

    const list = await call("/rotated/mcp", { method: "POST", headers: mcpHeaders(key), body: rpc(8, "tools/list") });
    expect(list.status).toBe(503);
    expect((await list.json<any>()).error).toMatchObject({ code: -32000 });
  });
});

describe("relaying", () => {
  it("forwards an allow-listed request and returns the agent's JSON answer", async () => {
    const a = await agent("notes");
    expect(a.welcome).toMatchObject({ type: "welcome", maxBody: 20 * 1024 * 1024 });
    const key = await notesKey();
    const pending = call("/notes/mcp?x=1", {
      method: "POST",
      headers: { ...mcpHeaders(key, { "mcp-method": "tools/list", cookie: "secret=1", "x-evil": "1" }), "cf-connecting-ip": "203.0.113.9" },
      body: rpc(1, "tools/list"),
    });
    const req = await a.nextRequest();
    expect(req).toMatchObject({ method: "POST", path: "/mcp", query: "x=1" });
    expect(JSON.parse(unb64(req.body!))).toMatchObject({ method: "tools/list" });
    expect(req.headers).toMatchObject({
      "content-type": "application/json",
      "mcp-protocol-version": "2026-07-28",
      "mcp-method": "tools/list",
      "x-forwarded-proto": "https",
      "x-toprelay-client": `key:${keyId(key)}`,
    });
    expect(req.headers.authorization).toBeUndefined();
    expect(req.headers.cookie).toBeUndefined();
    expect(req.headers["x-evil"]).toBeUndefined();
    expect(req.tools).toBeUndefined();

    a.respondJson(req.id, { jsonrpc: "2.0", id: 1, result: { tools: [] } }, 200, { "set-cookie": "a=b", "mcp-session-id": "s1" });
    const res = await pending;
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ jsonrpc: "2.0", id: 1, result: { tools: [] } });
    expect(res.headers.get("mcp-session-id")).toBe("s1");
    expect(res.headers.get("set-cookie")).toBeNull();
  });

  it("never lets a bot set the headers apps trust (x-toprelay-caller, x-toprelay-client, forwarded host)", async () => {
    const a = await agent("notes");
    const key = await notesKey();
    const pending = call("/notes/mcp", {
      method: "POST",
      headers: {
        ...mcpHeaders(key, { "mcp-method": "tools/list" }),
        "x-toprelay-caller": "v1; client=forged; ts=1; nonce=00; sig=00",
        "x-toprelay-client": "claude.ai",
        "x-forwarded-host": "127.0.0.1",
        "x-toprelay-service": "other",
        host: "relay.test",
      },
      body: rpc(1, "tools/list"),
    });
    const req = await a.nextRequest();
    expect(req.headers["x-toprelay-caller"]).toBeUndefined();
    expect(req.headers["x-toprelay-client"]).toBe(`key:${keyId(key)}`);
    expect(req.headers["x-forwarded-host"]).not.toBe("127.0.0.1");
    expect(req.headers["x-toprelay-service"]).toBeUndefined();
    a.respondJson(req.id, { jsonrpc: "2.0", id: 1, result: { tools: [] } });
    expect((await pending).status).toBe(200);
  });

  it("streams an SSE reply chunk by chunk", async () => {
    const a = await agent("notes");
    const pending = call("/notes/mcp", { method: "POST", headers: mcpHeaders(await notesKey()), body: rpc(2, "tools/call", { name: "slow" }) });
    const req = await a.nextRequest();
    a.send({ type: "response", id: req.id, status: 200, headers: { "content-type": "text/event-stream" }, stream: true });
    const res = await pending;
    expect(res.headers.get("content-type")).toBe("text/event-stream");
    a.send({ type: "chunk", id: req.id, data: b64("event: message\ndata: {\"progress\":1}\n\n") });
    a.send({ type: "chunk", id: req.id, data: b64("event: message\ndata: {\"done\":true}\n\n") });
    a.send({ type: "end", id: req.id });
    const text = await res.text();
    expect(text).toContain('{"progress":1}');
    expect(text).toContain('{"done":true}');
  });

  it("keeps concurrent requests apart by id", async () => {
    const a = await agent("files");
    const key = await mintKey(["svc=files", "g=1"]);
    const p1 = call("/files/one", { headers: { authorization: `Bearer ${key}` } });
    const p2 = call("/files/two", { headers: { authorization: `Bearer ${key}` } });
    await a.until(() => a.requests.length >= 2);
    const [r1, r2] = a.requests;
    expect(r1.id).not.toBe(r2.id);
    // answer in reverse order
    for (const r of [r2, r1]) a.send({ type: "response", id: r.id, status: 200, headers: { "content-type": "text/plain" }, body: b64(`got ${r.path}`) });
    expect(await (await p1).text()).toBe("got /one");
    expect(await (await p2).text()).toBe("got /two");
  });

  it("tells the agent when the bot stops reading a stream", async () => {
    const a = await agent("notes");
    const res = call("/notes/mcp", { method: "POST", headers: mcpHeaders(await notesKey()), body: rpc(3, "tools/call", { name: "long" }) });
    const req = await a.nextRequest();
    a.send({ type: "response", id: req.id, status: 200, headers: { "content-type": "text/event-stream" }, stream: true });
    a.send({ type: "chunk", id: req.id, data: b64("data: 1\n\n") });
    const reader = (await res).body!.getReader();
    await reader.read();
    await reader.cancel();
    a.send({ type: "chunk", id: req.id, data: b64("data: 2\n\n") });
    await a.until(() => a.cancels.includes(req.id));
  });

  it("tells the agent when the bot hangs up before any answer (a slow tool call)", async () => {
    const a = await agent("notes");
    const ctl = new AbortController();
    const res = call("/notes/mcp", { method: "POST", headers: mcpHeaders(await notesKey()), body: rpc(4, "tools/call", { name: "ocr" }), signal: ctl.signal });
    const req = await a.nextRequest();
    ctl.abort();
    await res.catch(() => undefined);
    await a.until(() => a.cancels.includes(req.id));
  });

  it("passes progress events through as they arrive, not at the end", async () => {
    const a = await agent("notes");
    const res = call("/notes/mcp", { method: "POST", headers: mcpHeaders(await notesKey()), body: rpc(5, "tools/call", { name: "render" }) });
    const req = await a.nextRequest();
    a.send({ type: "response", id: req.id, status: 200, headers: { "content-type": "text/event-stream" }, stream: true });
    const reader = (await res).body!.getReader();
    const decoder = new TextDecoder();
    for (const event of [": keepalive\n\n", 'event: message\ndata: {"method":"notifications/progress"}\n\n']) {
      a.send({ type: "chunk", id: req.id, data: b64(event) });
      const { value } = await reader.read();
      expect(decoder.decode(value)).toBe(event); // delivered before the next one is even sent
    }
    a.send({ type: "chunk", id: req.id, data: b64('event: message\ndata: {"id":5,"result":{}}\n\n') });
    a.send({ type: "end", id: req.id });
    const { value } = await reader.read();
    expect(decoder.decode(value)).toContain('"id":5');
    expect((await reader.read()).done).toBe(true);
  });

  it("fails pending requests at once when the agent disconnects", async () => {
    const a = await agent("notes");
    const pending = call("/notes/mcp", { method: "POST", headers: mcpHeaders(await notesKey(), { "mcp-method": "tools/call", "mcp-name": "x" }), body: rpc(9, "tools/call", { name: "x" }) });
    await a.nextRequest();
    a.close();
    const body = await (await pending).json<any>();
    expect(body.id).toBe(9);
    expect(body.result.content[0].text).toContain("notes is offline");
  });

  it("replaces an older connection with close code 4000", async () => {
    const first = await agent("notes");
    const second = await agent("notes");
    await first.until(() => first.closed !== undefined);
    expect(first.closed!.code).toBe(4000);
    const pending = call("/notes/mcp", { method: "POST", headers: mcpHeaders(await notesKey()), body: rpc(1, "ping") });
    const req = await second.nextRequest();
    second.respondJson(req.id, { jsonrpc: "2.0", id: 1, result: {} });
    expect((await pending).status).toBe(200);
  });

  it("refuses paths with empty segments or encoded slashes", async () => {
    const key = await mintKey(["svc=files", "g=1"]);
    for (const path of ["/files//host/x", "/files/a%2f..%2fetc", "/files/a%5Cb"]) {
      expect((await call(path, { headers: { authorization: `Bearer ${key}` } })).status, path).toBe(400);
    }
  });

  it("cuts off a stream the bot isn't reading before it fills memory", async () => {
    const a = await agent("files");
    const pending = call("/files/big", { headers: { authorization: `Bearer ${await mintKey(["svc=files", "g=1"])}` } });
    const req = await a.nextRequest();
    a.send({ type: "response", id: req.id, status: 200, headers: { "content-type": "application/octet-stream" }, stream: true });
    const res = await pending;
    const chunk = btoa("x".repeat(1024 * 1024));
    for (let i = 0; i < 24; i++) a.send({ type: "chunk", id: req.id, data: chunk });
    await a.until(() => a.cancels.includes(req.id), 10000);
    // The bot gets a broken stream, not a clean end: a truncated reply must never look complete.
    let received = 0;
    let failed = false;
    try {
      const reader = res.body!.getReader();
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        received += value.byteLength;
      }
    } catch {
      failed = true;
    }
    expect(failed).toBe(true);
    expect(received).toBeLessThan(24 * 1024 * 1024);
  }, 20000);

  it("drops response headers workerd can't accept instead of hanging", async () => {
    const a = await agent("files");
    const pending = call("/files/x", { headers: { authorization: `Bearer ${await mintKey(["svc=files", "g=1"])}` } });
    const req = await a.nextRequest();
    a.send({ type: "response", id: req.id, status: 200, headers: { etag: "a\u0000b", "content-type": "text/plain" }, body: b64("ok") });
    const res = await pending;
    expect(res.status).toBe(200);
    expect(res.headers.get("etag")).toBeNull();
    expect(await res.text()).toBe("ok");
  });

  it("rejects oversized bodies before forwarding", async () => {
    const a = await agent("files");
    const big = new Uint8Array(20 * 1024 * 1024 + 1);
    const res = await call("/files/upload", { method: "POST", headers: { authorization: `Bearer ${await mintKey(["svc=files", "g=1"])}` }, body: big });
    expect(res.status).toBe(413);
    expect(a.requests).toHaveLength(0);
  });
});

describe("tool allow-lists", () => {
  it("refuses a tool the agent didn't allow, from the Mcp-Name header alone", async () => {
    const a = await agent("notes", { tools: ["hello"] });
    const res = await call("/notes/mcp", {
      method: "POST",
      headers: mcpHeaders(await notesKey(), { "mcp-method": "tools/call", "mcp-name": "delete_all" }),
      body: rpc(4, "tools/call", { name: "delete_all" }),
    });
    const body = await res.json<any>();
    expect(body).toMatchObject({ id: 4, result: { isError: true } });
    expect(body.result.content[0].text).toBe('The tool "delete_all" is not available on notes. Its owner hasn\'t allowed it.');
    expect(a.requests).toHaveLength(0);
  });

  it("applies a key's tools caveat and passes it to the agent", async () => {
    const a = await agent("notes");
    const narrowed = await mintKey(["svc=notes", "g=1", "tools=hello"]);
    const refused = await call("/notes/mcp", {
      method: "POST",
      headers: mcpHeaders(narrowed, { "mcp-method": "tools/call", "mcp-name": "other" }),
      body: rpc(5, "tools/call", { name: "other" }),
    });
    expect((await refused.json<any>()).result.isError).toBe(true);
    const allowed = call("/notes/mcp", {
      method: "POST",
      headers: mcpHeaders(narrowed, { "mcp-method": "tools/call", "mcp-name": "hello" }),
      body: rpc(6, "tools/call", { name: "hello" }),
    });
    const req = await a.nextRequest();
    expect(req.tools).toEqual(["hello"]);
    a.respondJson(req.id, { jsonrpc: "2.0", id: 6, result: { content: [] } });
    expect((await allowed).status).toBe(200);
  });

  it("refuses an expired key", async () => {
    const expired = await mintKey(["svc=notes", "g=1", `exp=${Math.floor(Date.now() / 1000) - 1}`]);
    expect((await call("/notes/mcp", { method: "POST", headers: mcpHeaders(expired), body: "{}" })).status).toBe(401);
  });
});

describe("admin", () => {
  it("revokes a key at once", async () => {
    const a = await agent("files");
    const key = await mintKey(["svc=files", "g=1"]);
    const revoke = await call("/files/admin/revoke-key", { method: "POST", headers: await adminHeaders("files"), body: JSON.stringify({ id: keyId(key) }) });
    expect(await revoke.json()).toMatchObject({ revoked: 1 });
    const res = await call("/files/x", { headers: { authorization: `Bearer ${key}` } });
    expect(res.status).toBe(401);
    expect(a.requests).toHaveLength(0);
    expect((await call("/files/admin/revoke-key", { method: "POST", headers: await adminHeaders("files"), body: '{"id":"nothex"}' })).status).toBe(400);
  });

  it("reports status and usage", async () => {
    const a = await agent("files");
    const key = await mintKey(["svc=files", "g=1"]);
    const p = call("/files/a", { headers: { authorization: `Bearer ${key}` } });
    const req = await a.nextRequest();
    a.send({ type: "response", id: req.id, status: 200, headers: {}, body: b64("hello") });
    await p;
    const status = await (await call("/files/admin/status", { headers: await adminHeaders("files") })).json<any>();
    expect(status).toMatchObject({ service: "files", connected: true, agent: "toprelay/test" });
    expect(status.requests).toBeGreaterThanOrEqual(1);
    expect(status.bytesOut).toBeGreaterThanOrEqual(5);
    expect(status.day).toMatch(/^\d{4}-\d{2}-\d{2}$/);
  });

  it("validates pairing input", async () => {
    const headers = await adminHeaders("notes");
    expect((await call("/notes/admin/pair", { method: "POST", headers, body: '{"code":"short"}' })).status).toBe(400);
    expect((await call("/notes/admin/pair", { method: "POST", headers, body: '{"code":"long-enough-code","ttl":5}' })).status).toBe(400);
    const ok = await (await call("/notes/admin/pair", { method: "POST", headers, body: '{"code":"long-enough-code"}' })).json<any>();
    expect(ok.expires).toBeGreaterThan(Date.now() / 1000);
  });
});

describe("file downloads", () => {
  const fileUrl = async (id: string, service = "notes", ttl = 600) => {
    const exp = Math.floor(Date.now() / 1000) + ttl;
    return `/${service}/files/${id}?e=${exp}&s=${await fileSig(BEARER_ROOT, service, 1, id, exp)}`;
  };
  const randomId = () => [...crypto.getRandomValues(new Uint8Array(16))].map((b) => b.toString(16).padStart(2, "0")).join("");

  it("streams the agent's file once, as an attachment that can't run on the relay's origin", async () => {
    const a = await agent("notes", { caps: ["files"] });
    expect(a.welcome.caps).toContain("files");
    const id = randomId();
    const url = await fileUrl(id);
    const pending = call(url); // no Authorization: the signed URL is the credential
    const req = await a.nextRequest();
    expect(req).toMatchObject({ method: "GET", path: `/files/${id}`, internal: "file" });
    a.send({ type: "response", id: req.id, status: 200, stream: true,
      headers: { "content-type": "text/html", "content-disposition": "inline", "set-cookie": "x=1" } });
    a.send({ type: "chunk", id: req.id, data: b64("<script>alert(1)</script>") });
    a.send({ type: "end", id: req.id });
    const res = await pending;
    expect(res.status).toBe(200);
    expect(await res.text()).toBe("<script>alert(1)</script>");
    expect(res.headers.get("content-disposition")).toBe("attachment");
    expect(res.headers.get("x-content-type-options")).toBe("nosniff");
    expect(res.headers.get("content-security-policy")).toBe("default-src 'none'; sandbox");
    expect(res.headers.get("cache-control")).toBe("no-store");
    expect(res.headers.get("set-cookie")).toBeNull();

    const again = await call(url);
    expect(again.status).toBe(410);
    expect(a.requests.length).toBe(1); // the second try never reached the agent
  });

  it("refuses bad, expired and foreign links before the Durable Object", async () => {
    const a = await agent("notes", { caps: ["files"] });
    const id = randomId();
    const good = await fileUrl(id);
    expect((await call(good.replace(/s=[^&]+/, "s=AAAA"))).status).toBe(403);
    expect((await call(await fileUrl(id, "notes", -5))).status).toBe(410);
    expect((await call(good.replace("/notes/", "/rotated/"))).status).toBe(403);
    expect((await call(good, { method: "POST" })).status).toBe(405);
    expect((await call(`/files/files/${id}?e=1&s=x`)).status).toBe(401); // http service: /files is the app's, behind its credential
    expect(a.requests.length).toBe(0);
  });

  it("doesn't spend a link on a download that never reached an agent", async () => {
    const id = randomId();
    const url = await fileUrl(id);
    expect((await call(url)).status).toBe(503); // nobody connected: retry later, as the 503 says
    const old = await agent("notes"); // connected, but can't share files
    expect((await call(url)).status).toBe(503);
    old.close();
    const a = await agent("notes", { caps: ["files"] });
    const pending = call(url);
    const req = await a.nextRequest();
    a.send({ type: "response", id: req.id, status: 200, headers: { "content-type": "text/plain" }, body: b64("ok") });
    expect(await (await pending).text()).toBe("ok");
    expect((await call(url)).status).toBe(410); // now it's spent
  });

  it("gives the link back if the agent drops before answering", async () => {
    const a = await agent("notes", { caps: ["files"] });
    const url = await fileUrl(randomId());
    const pending = call(url);
    await a.nextRequest();
    a.close();
    expect((await pending).status).toBe(503);
    const b = await agent("notes", { caps: ["files"] });
    const again = call(url);
    const req = await b.nextRequest();
    b.send({ type: "response", id: req.id, status: 200, headers: { "content-type": "text/plain" }, body: b64("ok") });
    expect((await again).status).toBe(200);
  });

  it("needs an agent that can share files", async () => {
    await agent("notes"); // no "files" cap: an old agent would forward the path to the app
    const res = await call(await fileUrl(randomId()));
    expect(res.status).toBe(503);
    expect(await res.text()).toContain("too old");
  });
});
