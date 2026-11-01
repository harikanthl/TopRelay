// Test helpers: call the Worker, mint bearer keys, and play the laptop agent over a real WebSocket.
import { exports } from "cloudflare:workers";
import { agentKey, b64urlDecode, b64urlEncode } from "../src/keys";

export const AGENT_ROOT = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8";
export const BEARER_ROOT = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8";
export const BASE = "http://localhost";

const worker = () => (exports as unknown as { default: Fetcher }).default;

export function call(path: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  if (!headers.has("host")) headers.set("host", "localhost");
  return worker().fetch(new Request(`${BASE}${path}`, { ...init, headers }));
}

async function hmac(key: Uint8Array, message: string): Promise<Uint8Array> {
  const k = await crypto.subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await crypto.subtle.sign("HMAC", k, new TextEncoder().encode(message)));
}

/** Mints a trk1 key exactly as docs/protocol.md describes (the agent does this in Go). */
export async function mintKey(caveats: string[], id = randomKeyId()): Promise<string> {
  let s = await hmac(b64urlDecode(BEARER_ROOT), `toprelay/bearer/v1/${id}`);
  for (const c of caveats) s = await hmac(s, c);
  return `trk1.${b64urlEncode(new TextEncoder().encode(JSON.stringify({ i: id, c: caveats })))}.${b64urlEncode(s)}`;
}

export function randomKeyId(): string {
  return [...crypto.getRandomValues(new Uint8Array(8))].map((b) => b.toString(16).padStart(2, "0")).join("");
}

export const keyId = (token: string) => JSON.parse(new TextDecoder().decode(b64urlDecode(token.split(".")[1]))).i as string;

export const mcpHeaders = (token: string, extra: Record<string, string> = {}) => ({
  authorization: `Bearer ${token}`,
  "content-type": "application/json",
  accept: "application/json, text/event-stream",
  "mcp-protocol-version": "2026-07-28",
  ...extra,
});

export const rpc = (id: number, method: string, params: unknown = {}) => JSON.stringify({ jsonrpc: "2.0", id, method, params });

export const b64 = (text: string) => btoa(text);
export const unb64 = (data: string) => atob(data);

export interface RequestFrame {
  v: 1;
  type: "request";
  id: string;
  method: string;
  path: string;
  query: string;
  headers: Record<string, string>;
  body?: string;
  tools?: string[];
}

type Frame = Record<string, unknown> & { type: string };

/** A fake laptop agent: connects, says hello, and lets the test answer each request frame. */
export class FakeAgent {
  readonly requests: RequestFrame[] = [];
  readonly cancels: string[] = [];
  closed?: { code: number; reason: string };
  private waiters: Array<() => void> = [];

  private constructor(readonly ws: WebSocket, readonly welcome: Frame) {
    ws.addEventListener("message", (event) => {
      if (typeof event.data !== "string" || event.data === "pong") return;
      const frame = JSON.parse(event.data) as Frame;
      if (frame.type === "request") this.requests.push(frame as unknown as RequestFrame);
      if (frame.type === "cancel") this.cancels.push(frame.id as string);
      this.wake();
    });
    ws.addEventListener("close", (event) => {
      this.closed = { code: event.code, reason: event.reason };
      this.wake();
    });
  }

  static async connect(service: string, opts: { tools?: string[] | null; gen?: number; key?: string } = {}): Promise<FakeAgent> {
    const key = opts.key ?? (await agentKey(AGENT_ROOT, service, opts.gen ?? 1));
    const res = await call(`/${service}/connect`, { headers: { upgrade: "websocket", authorization: `Bearer ${key}` } });
    if (res.status !== 101 || !res.webSocket) throw new Error(`connect failed: ${res.status} ${await res.text()}`);
    const ws = res.webSocket;
    ws.accept();
    const welcome = new Promise<Frame>((resolve) => {
      const onMessage = (event: MessageEvent) => {
        if (typeof event.data !== "string") return;
        const frame = JSON.parse(event.data) as Frame;
        if (frame.type === "welcome") {
          ws.removeEventListener("message", onMessage);
          resolve(frame);
        }
      };
      ws.addEventListener("message", onMessage);
    });
    ws.send(JSON.stringify({ v: 1, type: "hello", agent: "toprelay/test", caps: [], kind: "mcp", tools: opts.tools ?? null }));
    return new FakeAgent(ws, await welcome);
  }

  private wake() {
    const waiters = this.waiters;
    this.waiters = [];
    for (const w of waiters) w();
  }

  /** Waits until `predicate` holds, re-checking on every frame. */
  async until(predicate: () => boolean, timeoutMs = 3000): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    while (!predicate()) {
      if (Date.now() > deadline) throw new Error("timed out waiting for the agent");
      await new Promise<void>((resolve) => {
        this.waiters.push(resolve);
        setTimeout(resolve, 50);
      });
    }
  }

  async nextRequest(): Promise<RequestFrame> {
    const index = this.requests.length;
    await this.until(() => this.requests.length > index);
    return this.requests[index];
  }

  send(frame: Record<string, unknown>) {
    this.ws.send(JSON.stringify({ v: 1, ...frame }));
  }

  respondJson(id: string, body: unknown, status = 200, headers: Record<string, string> = {}) {
    this.send({ type: "response", id, status, headers: { "content-type": "application/json", ...headers }, body: b64(JSON.stringify(body)) });
  }

  close(code = 1000) {
    this.ws.close(code, "test");
  }
}
