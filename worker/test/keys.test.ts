import { describe, expect, it } from "vitest";
import { agentKey, checkAgentKey, verifyBearer } from "../src/keys";
import { AGENT_ROOT, BEARER_ROOT, mintKey } from "./helpers";
import { GcraLimiter } from "../src/gcra";

// docs/protocol.md "Test vectors"
const AGENT_VECTOR = "tra1.TA3-AhWB6Ka9MRN31Tk1D5C0kgDAgB4eoiJpZWwq7Aw";
const BEARER_BASE =
  "trk1.eyJpIjoiMDEyMzQ1Njc4OWFiY2RlZiIsImMiOlsic3ZjPW5vdGVzIiwiZz0xIl19.ByA-aoTDoDDxKP2uBxEVX5f_pNZRme7o3HaKWh6kn3I";
const BEARER_NARROWED =
  "trk1.eyJpIjoiMDEyMzQ1Njc4OWFiY2RlZiIsImMiOlsic3ZjPW5vdGVzIiwiZz0xIiwidG9vbHM9c2VhcmNoLHJlYWQiLCJleHA9NDEwMjQ0NDgwMCJdfQ.x26sPAObXFuxuW6XUmyku29lI8HiecbJgLrwd32DRxg";
const NOW = 1_793_500_000;

describe("agent key", () => {
  it("matches the protocol vector", async () => {
    expect(await agentKey(AGENT_ROOT, "notes", 1)).toBe(AGENT_VECTOR);
    expect(await checkAgentKey(AGENT_ROOT, "notes", 1, AGENT_VECTOR)).toBe(true);
  });

  it("is bound to the service and generation", async () => {
    expect(await checkAgentKey(AGENT_ROOT, "files", 1, AGENT_VECTOR)).toBe(false);
    expect(await checkAgentKey(AGENT_ROOT, "notes", 2, AGENT_VECTOR)).toBe(false);
    expect(await checkAgentKey("", "notes", 1, AGENT_VECTOR)).toBe(false);
  });
});

describe("bearer key", () => {
  it("verifies the base vector", async () => {
    const r = await verifyBearer(BEARER_ROOT, BEARER_BASE, "notes", 1, NOW);
    expect(r).toEqual({ ok: true, claims: { id: "0123456789abcdef", service: "notes", expiresAt: undefined, tools: undefined } });
  });

  it("verifies the narrowed vector and reports its limits", async () => {
    const r = await verifyBearer(BEARER_ROOT, BEARER_NARROWED, "notes", 1, NOW);
    expect(r.ok && r.claims).toEqual({ id: "0123456789abcdef", service: "notes", expiresAt: 4102444800, tools: ["search", "read"] });
  });

  it("our minter reproduces the vectors", async () => {
    expect(await mintKey(["svc=notes", "g=1"], "0123456789abcdef")).toBe(BEARER_BASE);
  });

  it("rejects the wrong service, a rotated generation, expiry and tampering", async () => {
    expect(await verifyBearer(BEARER_ROOT, BEARER_BASE, "files", 1, NOW)).toMatchObject({ ok: false, reason: "wrong service" });
    expect(await verifyBearer(BEARER_ROOT, BEARER_BASE, "notes", 2, NOW)).toMatchObject({ ok: false, reason: "rotated" });
    expect(await verifyBearer(BEARER_ROOT, BEARER_NARROWED, "notes", 1, 4102444800)).toMatchObject({ ok: false, reason: "expired" });
    const [p, payload, sig] = BEARER_NARROWED.split(".");
    const widened = `${p}.${btoa(JSON.stringify({ i: "0123456789abcdef", c: ["svc=notes", "g=1"] })).replace(/=+$/, "")}.${sig}`;
    expect(await verifyBearer(BEARER_ROOT, widened, "notes", 1, NOW)).toMatchObject({ ok: false });
    expect(await verifyBearer(BEARER_ROOT, `${p}.${payload}.${sig.slice(0, -2)}AA`, "notes", 1, NOW)).toMatchObject({ ok: false });
    expect(await verifyBearer(BEARER_ROOT, "trk1.garbage", "notes", 1, NOW)).toMatchObject({ ok: false, reason: "malformed" });
  });

  it("fails closed on unknown caveats and intersects tools", async () => {
    const unknown = await mintKey(["svc=notes", "g=1", "admin=1"]);
    expect(await verifyBearer(BEARER_ROOT, unknown, "notes", 1, NOW)).toMatchObject({ ok: false, reason: "unknown caveat admin" });
    const two = await mintKey(["svc=notes", "g=1", "tools=a,b,c", "tools=b,c,d"]);
    const r = await verifyBearer(BEARER_ROOT, two, "notes", 1, NOW);
    expect(r.ok && r.claims.tools).toEqual(["b", "c"]);
  });

  it("requires svc then g as the first two caveats", async () => {
    const swapped = await mintKey(["g=1", "svc=notes"]);
    expect(await verifyBearer(BEARER_ROOT, swapped, "notes", 1, NOW)).toMatchObject({ ok: false });
  });
});

describe("GCRA", () => {
  it("allows exactly the burst, then the sustained rate", () => {
    const limiter = new GcraLimiter({ interval: 100, burst: 3 });
    const t = 1_000_000;
    expect([1, 2, 3, 4].map(() => limiter.take("k", t).ok)).toEqual([true, true, true, false]);
    const denied = limiter.take("k", t);
    expect(denied.ok === false && denied.retryAfterMs).toBeGreaterThan(0);
    expect(limiter.take("k", t + 100).ok).toBe(true);
    expect(limiter.take("k", t + 100).ok).toBe(false);
    expect(limiter.take("other", t).ok).toBe(true);
  });
});
