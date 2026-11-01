// Header allow-lists (docs/protocol.md "Request forwarding").

export const REQUEST_HEADERS = [
  "accept",
  "content-type",
  "user-agent",
  "mcp-protocol-version",
  "mcp-session-id",
  "mcp-method",
  "mcp-name",
  "last-event-id",
] as const;

export const RESPONSE_HEADERS = new Set([
  "content-type",
  "cache-control",
  "mcp-session-id",
  "location",
  "etag",
  "last-modified",
  "content-disposition",
  "retry-after",
  "mcp-protocol-version",
]);

/** Internal headers the Worker sets on its call to the Durable Object. Never come from the client. */
export const INTERNAL = {
  service: "x-toprelay-service",
  kind: "x-toprelay-kind",
  path: "x-toprelay-path",
  query: "x-toprelay-query",
  method: "x-toprelay-method",
  client: "x-toprelay-client",
  keyId: "x-toprelay-key-id",
  tools: "x-toprelay-tools",
  ip: "x-toprelay-ip",
  host: "x-toprelay-host",
} as const;

export function forwardedRequestHeaders(source: Headers, ip: string, host: string, client: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const name of REQUEST_HEADERS) {
    const value = source.get(name);
    if (value !== null) out[name] = value;
  }
  if (ip) {
    out["x-forwarded-for"] = ip;
    out["cf-connecting-ip"] = ip;
  }
  out["x-forwarded-proto"] = "https";
  out["x-forwarded-host"] = host;
  out["x-toprelay-client"] = client;
  return out;
}

/** Tab, visible ASCII and bytes 0x80–0xFF: no NUL, no other control characters, no CR/LF. */
const SAFE_VALUE = /^[\t\x20-\x7e\x80-\xff]*$/;

export function filterResponseHeaders(source: Record<string, unknown> | undefined): Headers {
  const out = new Headers();
  if (!source || typeof source !== "object") return out;
  for (const [name, value] of Object.entries(source)) {
    const lower = name.toLowerCase();
    if (!RESPONSE_HEADERS.has(lower) || typeof value !== "string" || !SAFE_VALUE.test(value)) continue;
    try {
      out.set(lower, value);
    } catch {
      // workerd refused it: drop the header rather than the whole response
    }
  }
  return out;
}
