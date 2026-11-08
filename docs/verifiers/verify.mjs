// Verifies X-TopRelay-Caller (docs/protocol.md "Agent → local app"). Node 18+, no dependencies.
// BEGIN verifier
import { createHash, createHmac, timingSafeEqual } from "node:crypto";

/** Returns the caller's client id if the header is valid, else null. target is the raw request path + query (req.url). */
export function verifyTopRelayCaller(key, header, method, target, body, now = Date.now() / 1000) {
  const [v, ...rest] = String(header ?? "").split(";").map((p) => p.trim());
  const f = Object.fromEntries(rest.filter((p) => p.includes("=")).map((p) => [p.slice(0, p.indexOf("=")), p.slice(p.indexOf("=") + 1)]));
  if (v !== "v1" || !/^\d+$/.test(f.ts ?? "") || [f.client, f.nonce, f.sig].includes(undefined)) return null;
  if (Math.abs(now - Number(f.ts)) > 60) return null;
  const bodyHash = createHash("sha256").update(body).digest("hex");
  const msg = ["toprelay-caller-v1", method, target, f.client, f.ts, f.nonce, bodyHash].join("\n");
  const want = Buffer.from(createHmac("sha256", key).update(msg).digest("hex"));
  const got = Buffer.from(f.sig);
  if (got.length !== want.length || !timingSafeEqual(got, want)) return null;
  try { return decodeURIComponent(f.client); } catch { return null; }
}
// END verifier
if (process.argv[1]?.endsWith("verify.mjs")) { // the protocol.md test vector
  const [key, header, method, target, body, now] = process.argv.slice(2);
  console.log(verifyTopRelayCaller(key, header, method, target, Buffer.from(body), Number(now)));
}
