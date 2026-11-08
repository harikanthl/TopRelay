// Agent keys and macaroon-style bearer keys (docs/protocol.md "Credentials").

const enc = new TextEncoder();

export function b64urlEncode(bytes: ArrayBuffer | Uint8Array): string {
  const u8 = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  let s = "";
  for (let i = 0; i < u8.length; i++) s += String.fromCharCode(u8[i]);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function b64urlDecode(text: string): Uint8Array {
  if (!/^[A-Za-z0-9_-]*$/.test(text)) throw new Error("not base64url");
  const padded = text.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((text.length + 3) % 4);
  const bin = atob(padded);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

async function hmac(key: Uint8Array, message: string): Promise<Uint8Array> {
  const k = await crypto.subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await crypto.subtle.sign("HMAC", k, enc.encode(message)));
}

/** Constant-time equality for byte arrays of possibly different length. */
export function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  return crypto.subtle.timingSafeEqual(a, b);
}

/** Constant-time equality for strings of any length (hashed first so lengths match). */
export async function stringsEqual(a: string, b: string): Promise<boolean> {
  const [x, y] = await Promise.all([crypto.subtle.digest("SHA-256", enc.encode(a)), crypto.subtle.digest("SHA-256", enc.encode(b))]);
  return crypto.subtle.timingSafeEqual(x, y);
}

export async function agentKey(agentRoot: string, service: string, gen: number): Promise<string> {
  return "tra1." + b64urlEncode(await hmac(b64urlDecode(agentRoot), `toprelay/agent/v1/${service}/${gen}`));
}

export async function checkAgentKey(agentRoot: string, service: string, gen: number, presented: string): Promise<boolean> {
  if (!agentRoot) return false;
  return stringsEqual(await agentKey(agentRoot, service, gen), presented);
}

export interface BearerClaims {
  id: string;
  service: string;
  /** Earliest expiry across exp caveats, unix seconds; undefined when none. */
  expiresAt?: number;
  /** Intersection of tools caveats; undefined when unrestricted. */
  tools?: string[];
}

export type BearerResult = { ok: true; claims: BearerClaims } | { ok: false; reason: string };

const KEY_ID_RE = /^[0-9a-f]{16}$/;

/**
 * Verifies a trk1 bearer key for one service. `now` is unix seconds.
 * Revocation is checked separately (the Durable Object holds the list).
 */
export async function verifyBearer(bearerRoot: string, token: string, service: string, gen: number, now: number): Promise<BearerResult> {
  if (!bearerRoot) return { ok: false, reason: "no bearer root configured" };
  const parts = token.split(".");
  if (parts.length !== 3 || parts[0] !== "trk1") return { ok: false, reason: "malformed" };

  let payload: { i?: unknown; c?: unknown };
  let sig: Uint8Array;
  try {
    payload = JSON.parse(new TextDecoder("utf-8", { fatal: true, ignoreBOM: false }).decode(b64urlDecode(parts[1])));
    sig = b64urlDecode(parts[2]);
  } catch {
    return { ok: false, reason: "malformed" };
  }
  const { i, c } = payload ?? {};
  if (typeof i !== "string" || !KEY_ID_RE.test(i)) return { ok: false, reason: "bad key id" };
  if (!Array.isArray(c) || c.length < 2 || c.length > 64 || !c.every((x) => typeof x === "string" && x.length <= 4096)) {
    return { ok: false, reason: "bad caveats" };
  }
  const caveats = c as string[];

  let s = await hmac(b64urlDecode(bearerRoot), `toprelay/bearer/v1/${i}`);
  for (const caveat of caveats) s = await hmac(s, caveat);
  if (!bytesEqual(s, sig)) return { ok: false, reason: "bad signature" };

  if (caveats[0] !== `svc=${service}`) return { ok: false, reason: "wrong service" };
  if (caveats[1] !== `g=${gen}`) return { ok: false, reason: "rotated" };

  let expiresAt: number | undefined;
  let tools: Set<string> | undefined;
  for (const caveat of caveats.slice(2)) {
    const eq = caveat.indexOf("=");
    const name = eq < 0 ? caveat : caveat.slice(0, eq);
    const value = eq < 0 ? "" : caveat.slice(eq + 1);
    if (name === "exp") {
      if (!/^\d{1,12}$/.test(value)) return { ok: false, reason: "bad exp" };
      const exp = Number(value);
      if (now >= exp) return { ok: false, reason: "expired" };
      expiresAt = expiresAt === undefined ? exp : Math.min(expiresAt, exp);
    } else if (name === "tools") {
      const list = new Set(value.split(",").filter(Boolean));
      tools = tools === undefined ? list : new Set([...tools].filter((t) => list.has(t)));
    } else {
      return { ok: false, reason: `unknown caveat ${name}` };
    }
  }
  return { ok: true, claims: { id: i, service, expiresAt, tools: tools && [...tools] } };
}

/** Longest a signed file URL may be valid, plus clock skew allowance (docs/protocol.md "Files"). */
export const MAX_FILE_URL_TTL = 15 * 60;
const FILE_SKEW = 60;
export const FILE_ID_RE = /^[0-9a-f]{32}$/;

/** sig = b64url(HMAC(HMAC(BEARER_ROOT, "toprelay/files/v1"), "toprelay/file/v1/<service>/<gen>/<id>/<exp>")). */
export async function fileSig(bearerRoot: string, service: string, gen: number, id: string, exp: number): Promise<string> {
  const fileKey = await hmac(b64urlDecode(bearerRoot), "toprelay/files/v1");
  return b64urlEncode(await hmac(fileKey, `toprelay/file/v1/${service}/${gen}/${id}/${exp}`));
}

export type FileUrlResult = { ok: true; exp: number } | { ok: false; status: number; reason: string };

/** Checks a download URL's id, expiry and signature. */
export async function verifyFileUrl(bearerRoot: string, service: string, gen: number, id: string, e: string | null, s: string | null, now: number): Promise<FileUrlResult> {
  if (!FILE_ID_RE.test(id) || !e || !/^\d{1,12}$/.test(e) || !s) return { ok: false, status: 404, reason: "not found" };
  const exp = Number(e);
  if (exp > now + MAX_FILE_URL_TTL + FILE_SKEW) return { ok: false, status: 403, reason: "bad link" };
  if (!(await stringsEqual(await fileSig(bearerRoot, service, gen, id, exp), s))) return { ok: false, status: 403, reason: "bad link" };
  if (exp <= now) return { ok: false, status: 410, reason: "This download link expired. Ask for the file again." };
  return { ok: true, exp };
}
