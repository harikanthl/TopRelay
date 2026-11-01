// Worker configuration, parsed from bindings (docs/protocol.md "Worker configuration").

export type ServiceKind = "mcp" | "http";

export interface Service {
  name: string;
  kind: ServiceKind;
  gen: number;
}

export interface Env {
  RELAY: DurableObjectNamespace;
  OAUTH_KV: KVNamespace;
  PUBLIC_URL: string;
  SERVICES: string;
  AGENT_ROOT: string;
  BEARER_ROOT: string;
}

export const NAME_RE = /^[a-z0-9]{1,32}$/;
export const RESERVED = new Set(["health", "authorize", "oauth", "well", "admin", "connect", "mcp", "toprelay", "narrow", "revoke"]);

export const MAX_BODY = 20 * 1024 * 1024;

export interface Config {
  publicUrl: string;
  services: Map<string, Service>;
}

let cached: { key: string; config: Config } | undefined;

/** Parses PUBLIC_URL and SERVICES once per isolate (they only change with a redeploy). Throws on bad config. */
export function loadConfig(env: Env): Config {
  const key = `${env.PUBLIC_URL}\n${env.SERVICES}`;
  if (cached?.key === key) return cached.config;

  const publicUrl = String(env.PUBLIC_URL ?? "").replace(/\/+$/, "");
  if (!/^https?:\/\/[^/]+$/.test(publicUrl)) throw new Error("PUBLIC_URL must be an origin like https://toprelay.example.workers.dev");

  let parsed: unknown;
  try {
    parsed = JSON.parse(env.SERVICES || "[]");
  } catch {
    throw new Error("SERVICES is not valid JSON");
  }
  if (!Array.isArray(parsed)) throw new Error("SERVICES must be a JSON array");

  const services = new Map<string, Service>();
  for (const entry of parsed) {
    const { name, kind, gen } = (entry ?? {}) as Record<string, unknown>;
    if (typeof name !== "string" || !NAME_RE.test(name) || RESERVED.has(name)) throw new Error(`invalid service name: ${String(name)}`);
    if (kind !== "mcp" && kind !== "http") throw new Error(`invalid kind for ${name}`);
    if (typeof gen !== "number" || !Number.isInteger(gen) || gen < 1) throw new Error(`invalid gen for ${name}`);
    if (services.has(name)) throw new Error(`duplicate service ${name}`);
    services.set(name, { name, kind, gen });
  }

  const config = { publicUrl, services };
  cached = { key, config };
  return config;
}

/** The OAuth protected-resource identifier for a service. */
export function resourceFor(publicUrl: string, service: Service): string {
  return service.kind === "mcp" ? `${publicUrl}/${service.name}/mcp` : `${publicUrl}/${service.name}`;
}
