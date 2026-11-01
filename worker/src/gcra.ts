// Generic Cell Rate Algorithm: one timestamp (the theoretical arrival time) per key.

export interface Gcra {
  /** Milliseconds between requests at the sustained rate. */
  interval: number;
  /** Requests allowed at once on top of the sustained rate. */
  burst: number;
}

export type GcraResult = { ok: true } | { ok: false; retryAfterMs: number };

export class GcraLimiter {
  private tat = new Map<string, number>();

  constructor(private readonly limit: Gcra) {}

  take(key: string, now: number): GcraResult {
    const { interval, burst } = this.limit;
    const tat = Math.max(this.tat.get(key) ?? now, now);
    const allowAt = tat - interval * burst;
    // Reject once the backlog reaches `burst` intervals, so exactly `burst` requests pass at once.
    if (now <= allowAt) return { ok: false, retryAfterMs: allowAt - now + 1 };
    this.tat.set(key, tat + interval);
    if (this.tat.size > 1024) this.prune(now);
    return { ok: true };
  }

  private prune(now: number) {
    for (const [key, tat] of this.tat) if (tat <= now) this.tat.delete(key);
  }
}

/** 20 requests/s sustained, burst 60 (docs/protocol.md "Limits"). */
export const REQUEST_LIMIT: Gcra = { interval: 50, burst: 60 };
/** Pairing-code attempts per caller IP: 5 at once, then one every 2 minutes. */
export const PAIR_LIMIT: Gcra = { interval: 120_000, burst: 5 };
/** Pairing-code attempts for a whole service, from every IP together: 30 at once, then one every 20 seconds. */
export const PAIR_GLOBAL_LIMIT: Gcra = { interval: 20_000, burst: 30 };
