// The Relay Durable Object: one per service. Holds the laptop agent's WebSocket (hibernatable), relays requests
// to it as frames, and keeps the service's small state: revoked bearer key ids, the pairing code, usage counters.
// Frames and limits: docs/protocol.md.

import { DurableObject } from "cloudflare:workers";
import type { Env } from "./config";
import { MAX_BODY } from "./config";
import { GcraLimiter, PAIR_LIMIT, REQUEST_LIMIT } from "./gcra";
import { filterResponseHeaders, forwardedRequestHeaders, INTERNAL } from "./headers";
import { stringsEqual } from "./keys";
import { disallowedText, offlineText, parseRpc, rpcError, toolError } from "./mcp";

export const START_TIMEOUT_MS = 120_000;
export const STREAM_IDLE_MS = 300_000;
export const TOTAL_TIMEOUT_MS = 840_000;
const MAX_REVOKED = 10_000;
const FLUSH_DELAY_MS = 60_000;

interface Hello {
  agent: string;
  caps: string[];
  kind: string;
  tools: string[] | null;
}

interface Attachment {
  connectedAt: number;
  hello?: Hello;
}

interface Pending {
  ws: WebSocket;
  service: string;
  kind: string;
  body: Uint8Array;
  contentType: string | null;
  mcpMethod: string | null;
  started: boolean;
  settled: boolean;
  resolve: (response: Response) => void;
  writer?: WritableStreamDefaultWriter<Uint8Array>;
  startTimer?: number;
  idleTimer?: number;
  totalTimer?: number;
}

interface Usage {
  day: string;
  requests: number;
  rejected: number;
  bytesIn: number;
  bytesOut: number;
}

const json = (body: unknown, status = 200, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json", "cache-control": "no-store", ...headers } });

const stopTimer = (timer: number | undefined) => {
  if (timer !== undefined) clearTimeout(timer);
};

const today = (now: number) => new Date(now).toISOString().slice(0, 10);

function b64ToBytes(text: string): Uint8Array {
  const bin = atob(text);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function bytesToB64(bytes: Uint8Array): string {
  let s = "";
  const step = 0x8000;
  for (let i = 0; i < bytes.length; i += step) s += String.fromCharCode(...bytes.subarray(i, i + step));
  return btoa(s);
}

/** Pairing codes: uppercase, spaces and dashes removed (docs/protocol.md "Pairing codes"). */
export const normalizeCode = (code: string) => code.toUpperCase().replace(/[\s-]+/g, "");

async function sha256Hex(text: string): Promise<string> {
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text)));
  return [...digest].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** Reads a body up to `limit` bytes; returns null when it is larger. */
async function readCapped(body: ReadableStream<Uint8Array> | null, limit: number): Promise<Uint8Array | null> {
  if (!body) return new Uint8Array(0);
  const reader = body.getReader();
  const parts: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > limit) {
      await reader.cancel();
      return null;
    }
    parts.push(value);
  }
  const out = new Uint8Array(size);
  let offset = 0;
  for (const part of parts) {
    out.set(part, offset);
    offset += part.byteLength;
  }
  return out;
}

export class Relay extends DurableObject<Env> {
  private readonly incarnation = crypto.randomUUID().slice(0, 8);
  private seq = 0;
  private readonly pending = new Map<string, Pending>();
  private readonly requestLimiter = new GcraLimiter(REQUEST_LIMIT);
  private readonly pairLimiter = new GcraLimiter(PAIR_LIMIT);
  private revoked: Set<string>;
  private usage: Usage;
  private flushScheduled = false;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.ctx.storage.sql.exec("CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT NOT NULL)");
    this.revoked = new Set(this.getJson<string[]>("revoked") ?? []);
    this.usage = this.getJson<Usage>("usage") ?? { day: today(Date.now()), requests: 0, rejected: 0, bytesIn: 0, bytesOut: 0 };
    this.ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair("ping", "pong"));
  }

  // ── storage ───────────────────────────────────────────────────────────────────────────────────────────────────

  private getJson<T>(key: string): T | undefined {
    const row = this.ctx.storage.sql.exec<{ v: string }>("SELECT v FROM kv WHERE k = ?", key).toArray()[0];
    return row ? (JSON.parse(row.v) as T) : undefined;
  }

  private putJson(key: string, value: unknown) {
    this.ctx.storage.sql.exec("INSERT INTO kv (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v", key, JSON.stringify(value));
  }

  private deleteKey(key: string) {
    this.ctx.storage.sql.exec("DELETE FROM kv WHERE k = ?", key);
  }

  // ── usage counters: kept in memory, written at most once a minute (Free plan: 100k rows written/day) ──────────

  private count(field: "requests" | "rejected" | "bytesIn" | "bytesOut", amount = 1) {
    const now = Date.now();
    const day = today(now);
    if (this.usage.day !== day) this.usage = { day, requests: 0, rejected: 0, bytesIn: 0, bytesOut: 0 };
    this.usage[field] += amount;
    if (!this.flushScheduled) {
      this.flushScheduled = true;
      this.ctx.storage.setAlarm(now + FLUSH_DELAY_MS);
    }
  }

  private flushUsage() {
    this.putJson("usage", this.usage);
    this.flushScheduled = false;
  }

  async alarm() {
    this.flushUsage();
  }

  // ── entry point (called only by the Worker, after it checked credentials) ─────────────────────────────────────

  async fetch(request: Request): Promise<Response> {
    const { pathname } = new URL(request.url);
    switch (pathname) {
      case "/connect":
        return this.acceptAgent(request);
      case "/relay":
        return this.relay(request);
      case "/admin/status":
        return this.status(request);
      case "/admin/pair":
        return this.setPairing(request);
      case "/admin/revoke-key":
        return this.revokeKey(request);
      case "/pair-verify":
        return this.verifyPairing(request);
      default:
        return new Response("not found", { status: 404 });
    }
  }

  // ── agent socket ──────────────────────────────────────────────────────────────────────────────────────────────

  private acceptAgent(request: Request): Response {
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") return new Response("expected a WebSocket upgrade", { status: 426 });
    for (const old of this.ctx.getWebSockets("agent")) {
      this.failPendingFor(old);
      try {
        old.close(4000, "replaced");
      } catch {
        // already closing
      }
    }
    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);
    this.ctx.acceptWebSocket(server, ["agent"]);
    server.serializeAttachment({ connectedAt: Math.floor(Date.now() / 1000) } satisfies Attachment);
    return new Response(null, { status: 101, webSocket: client });
  }

  /** The open agent socket, newest first; undefined when the agent is offline. */
  private agentSocket(): { ws: WebSocket; attachment: Attachment } | undefined {
    let best: { ws: WebSocket; attachment: Attachment } | undefined;
    for (const ws of this.ctx.getWebSockets("agent")) {
      if (ws.readyState !== WebSocket.OPEN) continue;
      const attachment = (ws.deserializeAttachment() ?? { connectedAt: 0 }) as Attachment;
      if (!best || attachment.connectedAt >= best.attachment.connectedAt) best = { ws, attachment };
    }
    return best;
  }

  async webSocketMessage(ws: WebSocket, message: string | ArrayBuffer) {
    if (typeof message !== "string") return; // binary frames are reserved for a later version
    let frame: Record<string, unknown>;
    try {
      frame = JSON.parse(message);
    } catch {
      return;
    }
    if (!frame || typeof frame !== "object" || frame.v !== 1) return;

    switch (frame.type) {
      case "hello":
        return this.onHello(ws, frame);
      case "response":
        return this.onResponse(ws, frame);
      case "chunk":
        return this.onChunk(ws, frame);
      case "end":
        return this.onEnd(ws, frame);
      default:
        return; // unknown frame types are ignored
    }
  }

  async webSocketClose(ws: WebSocket, code: number, reason: string) {
    this.failPendingFor(ws);
    try {
      ws.close(code === 1005 || code === 1006 ? 1000 : code, reason);
    } catch {
      // already closed
    }
    if (this.flushScheduled) this.flushUsage();
  }

  async webSocketError(ws: WebSocket) {
    this.failPendingFor(ws);
  }

  private onHello(ws: WebSocket, frame: Record<string, unknown>) {
    const tools = frame.tools === null ? null : Array.isArray(frame.tools) ? frame.tools.filter((t): t is string => typeof t === "string") : null;
    const hello: Hello = {
      agent: typeof frame.agent === "string" ? frame.agent.slice(0, 64) : "unknown",
      caps: Array.isArray(frame.caps) ? frame.caps.filter((c): c is string => typeof c === "string").slice(0, 32) : [],
      kind: typeof frame.kind === "string" ? frame.kind : "",
      tools,
    };
    const attachment = (ws.deserializeAttachment() ?? { connectedAt: Math.floor(Date.now() / 1000) }) as Attachment;
    ws.serializeAttachment({ ...attachment, hello } satisfies Attachment);
    ws.send(JSON.stringify({ v: 1, type: "welcome", caps: [], maxBody: MAX_BODY }));
  }

  private pendingFor(ws: WebSocket, frame: Record<string, unknown>): Pending | undefined {
    const id = frame.id;
    if (typeof id !== "string") return undefined;
    const p = this.pending.get(id);
    return p && p.ws === ws && !p.settled ? p : undefined;
  }

  private onResponse(ws: WebSocket, frame: Record<string, unknown>) {
    const p = this.pendingFor(ws, frame);
    if (!p || p.started) return;
    const id = frame.id as string;
    const status = typeof frame.status === "number" && frame.status >= 200 && frame.status <= 599 ? frame.status : 502;
    const headers = filterResponseHeaders(frame.headers as Record<string, unknown>);
    p.started = true;
    stopTimer(p.startTimer);
    p.body = new Uint8Array(0);

    if (frame.stream === true) {
      const { readable, writable } = new TransformStream<Uint8Array, Uint8Array>();
      p.writer = writable.getWriter();
      // The bot went away: the readable side is cancelled and writes start failing.
      p.writer.closed.catch(() => this.cancel(id, true));
      this.resetIdle(id, p);
      p.resolve(new Response(readable, { status, headers }));
      return;
    }

    let body: Uint8Array = new Uint8Array(0);
    if (typeof frame.body === "string" && frame.body.length > 0) {
      try {
        body = b64ToBytes(frame.body);
      } catch {
        this.settle(id, p);
        p.resolve(new Response("bad response from agent", { status: 502 }));
        return;
      }
    }
    this.count("bytesOut", body.byteLength);
    this.settle(id, p);
    p.resolve(new Response(status === 204 || status === 304 ? null : body, { status, headers }));
  }

  private onChunk(ws: WebSocket, frame: Record<string, unknown>) {
    const p = this.pendingFor(ws, frame);
    if (!p?.writer || typeof frame.data !== "string") return;
    let bytes: Uint8Array;
    try {
      bytes = b64ToBytes(frame.data);
    } catch {
      return;
    }
    this.count("bytesOut", bytes.byteLength);
    this.resetIdle(frame.id as string, p);
    p.writer.write(bytes).catch(() => this.cancel(frame.id as string, true));
  }

  private onEnd(ws: WebSocket, frame: Record<string, unknown>) {
    const p = this.pendingFor(ws, frame);
    if (!p) return;
    const id = frame.id as string;
    if (!p.started) {
      // An end without a response: the agent gave up before answering.
      this.settle(id, p);
      p.resolve(this.offlineAnswer(p));
      return;
    }
    this.settle(id, p);
    if (typeof frame.error === "string") p.writer?.abort(new Error(frame.error)).catch(() => {});
    else p.writer?.close().catch(() => {});
  }

  private resetIdle(id: string, p: Pending) {
    stopTimer(p.idleTimer);
    p.idleTimer = setTimeout(() => this.cancel(id, false), STREAM_IDLE_MS);
  }

  /** Stops a request: tells the agent, and ends whatever the bot is waiting for. */
  private cancel(id: string, botGone: boolean) {
    const p = this.pending.get(id);
    if (!p || p.settled) return;
    this.settle(id, p);
    try {
      if (p.ws.readyState === WebSocket.OPEN) p.ws.send(JSON.stringify({ v: 1, type: "cancel", id }));
    } catch {
      // socket already gone
    }
    if (!p.started) p.resolve(new Response("the service took too long to answer", { status: botGone ? 499 : 504 }));
    else if (botGone) p.writer?.abort(new Error("cancelled")).catch(() => {});
    else p.writer?.close().catch(() => {});
  }

  private settle(id: string, p: Pending) {
    p.settled = true;
    stopTimer(p.startTimer);
    stopTimer(p.idleTimer);
    stopTimer(p.totalTimer);
    this.pending.delete(id);
  }

  private failPendingFor(ws: WebSocket) {
    for (const [id, p] of this.pending) {
      if (p.ws !== ws) continue;
      this.settle(id, p);
      if (p.started) p.writer?.abort(new Error("agent disconnected")).catch(() => {});
      else p.resolve(this.offlineAnswer(p));
    }
  }

  private offlineAnswer(p: Pick<Pending, "service" | "kind" | "body" | "contentType" | "mcpMethod">): Response {
    const text = offlineText(p.service);
    if (p.kind !== "mcp") return new Response(text, { status: 503, headers: { "content-type": "text/plain; charset=utf-8", "retry-after": "30" } });
    const rpc = parseRpc(p.body, p.contentType);
    const method = p.mcpMethod ?? rpc.method;
    return method === "tools/call" ? toolError(rpc.id, text) : rpcError(rpc.id, -32000, text, 503);
  }

  // ── relaying a request ────────────────────────────────────────────────────────────────────────────────────────

  private async relay(request: Request): Promise<Response> {
    const h = request.headers;
    const service = h.get(INTERNAL.service) ?? "";
    const kind = h.get(INTERNAL.kind) ?? "http";
    const now = Date.now();

    const limited = this.requestLimiter.take("req", now);
    if (!limited.ok) {
      this.count("rejected");
      return json({ error: "rate_limited", message: `Too many requests for ${service}. Slow down.` }, 429, {
        "retry-after": String(Math.ceil(limited.retryAfterMs / 1000)),
      });
    }

    const keyId = h.get(INTERNAL.keyId);
    if (keyId && this.revoked.has(keyId)) {
      this.count("rejected");
      return json({ error: "invalid_token", error_description: "This key was revoked." }, 401, {
        "www-authenticate": 'Bearer error="invalid_token", error_description="This key was revoked."',
      });
    }

    const declared = Number(h.get("content-length") ?? "0");
    const body = declared > MAX_BODY ? null : await readCapped(request.body, MAX_BODY);
    if (body === null) {
      this.count("rejected");
      return new Response(`Request body too large: the limit is ${MAX_BODY / 1024 / 1024} MiB.`, { status: 413 });
    }
    this.count("bytesIn", body.byteLength);

    const contentType = h.get("content-type");
    const mcpMethod = kind === "mcp" ? h.get("mcp-method") : null;
    const keyTools = h.get(INTERNAL.tools) ? (JSON.parse(h.get(INTERNAL.tools)!) as string[]) : undefined;
    const agent = this.agentSocket();

    // MCP 2026-07-28 clients name the tool in a header, so a disallowed call never reaches the laptop.
    const toolName = mcpMethod === "tools/call" ? h.get("mcp-name") : null;
    if (toolName) {
      const agentTools = agent?.attachment.hello?.tools;
      const allowedByAgent = agentTools === undefined || agentTools === null || agentTools.includes(toolName);
      const allowedByKey = keyTools === undefined || keyTools.includes(toolName);
      if (!allowedByAgent || !allowedByKey) {
        this.count("rejected");
        return toolError(parseRpc(body, contentType).id, disallowedText(service, toolName));
      }
    }

    if (!agent?.attachment.hello) {
      this.count("rejected");
      return this.offlineAnswer({ service, kind, body, contentType, mcpMethod });
    }
    this.count("requests");

    const id = `${this.incarnation}-${++this.seq}`;
    const frame = {
      v: 1,
      type: "request",
      id,
      method: h.get(INTERNAL.method) ?? "GET",
      path: h.get(INTERNAL.path) ?? "/",
      query: h.get(INTERNAL.query) ?? "",
      headers: forwardedRequestHeaders(h, h.get(INTERNAL.ip) ?? "", h.get(INTERNAL.host) ?? "", h.get(INTERNAL.client) ?? ""),
      ...(body.byteLength > 0 ? { body: bytesToB64(body) } : {}),
      ...(keyTools !== undefined ? { tools: keyTools } : {}),
    };

    return new Promise<Response>((resolve) => {
      const p: Pending = {
        ws: agent.ws,
        service,
        kind,
        body,
        contentType,
        mcpMethod,
        started: false,
        settled: false,
        resolve,
      };
      this.pending.set(id, p);
      p.startTimer = setTimeout(() => this.cancel(id, false), START_TIMEOUT_MS);
      p.totalTimer = setTimeout(() => this.cancel(id, false), TOTAL_TIMEOUT_MS);
      request.signal?.addEventListener("abort", () => this.cancel(id, true));
      try {
        agent.ws.send(JSON.stringify(frame));
      } catch {
        this.settle(id, p);
        resolve(this.offlineAnswer(p));
      }
    });
  }

  // ── admin ─────────────────────────────────────────────────────────────────────────────────────────────────────

  private status(request: Request): Response {
    const agent = this.agentSocket();
    const day = today(Date.now());
    const usage = this.usage.day === day ? this.usage : { day, requests: 0, rejected: 0, bytesIn: 0, bytesOut: 0 };
    return json({
      service: request.headers.get(INTERNAL.service) ?? "",
      connected: Boolean(agent?.attachment.hello),
      connectedAt: agent?.attachment.connectedAt ?? null,
      agent: agent?.attachment.hello?.agent ?? null,
      ...usage,
      revokedKeys: this.revoked.size,
    });
  }

  private async setPairing(request: Request): Promise<Response> {
    const body = (await request.json().catch(() => null)) as { code?: unknown; ttl?: unknown } | null;
    const code = typeof body?.code === "string" ? normalizeCode(body.code) : "";
    if (code.length < 8 || code.length > 128) return json({ error: "code must be 8 to 128 characters" }, 400);
    const ttl = body?.ttl === undefined ? 600 : body.ttl;
    if (typeof ttl !== "number" || !Number.isInteger(ttl) || ttl < 60 || ttl > 3600) return json({ error: "ttl must be 60 to 3600 seconds" }, 400);
    const expires = Math.floor(Date.now() / 1000) + ttl;
    this.putJson("pairing", { hash: await sha256Hex(code), expires });
    return json({ expires });
  }

  private async verifyPairing(request: Request): Promise<Response> {
    const limited = this.pairLimiter.take("pair", Date.now());
    if (!limited.ok) return json({ ok: false, reason: "too_many_attempts", retryAfter: Math.ceil(limited.retryAfterMs / 1000) });
    const body = (await request.json().catch(() => null)) as { code?: unknown } | null;
    const pairing = this.getJson<{ hash: string; expires: number }>("pairing");
    if (!pairing || pairing.expires <= Math.floor(Date.now() / 1000)) return json({ ok: false, reason: "no_code" });
    const code = typeof body?.code === "string" ? normalizeCode(body.code) : "";
    if (!(await stringsEqual(await sha256Hex(code), pairing.hash))) return json({ ok: false, reason: "wrong_code" });
    this.deleteKey("pairing"); // single use
    return json({ ok: true });
  }

  private async revokeKey(request: Request): Promise<Response> {
    const body = (await request.json().catch(() => null)) as { id?: unknown } | null;
    const id = body?.id;
    if (typeof id !== "string" || !/^[0-9a-f]{16}$/.test(id)) return json({ error: "id must be 16 lowercase hex characters" }, 400);
    if (!this.revoked.has(id)) {
      if (this.revoked.size >= MAX_REVOKED) return json({ error: "revocation list is full: rotate the service instead" }, 409);
      this.revoked.add(id);
      this.putJson("revoked", [...this.revoked]);
    }
    return json({ revoked: this.revoked.size });
  }
}
