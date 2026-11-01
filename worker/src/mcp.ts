// JSON-RPC answers the relay gives on its own (docs/protocol.md "MCP-aware answers").

export function offlineText(service: string): string {
  return `${service} is offline: the computer running it is asleep or disconnected. Ask its owner to open it, then try again.`;
}

export function disallowedText(service: string, tool: string): string {
  return `The tool "${tool}" is not available on ${service}. Its owner hasn't allowed it.`;
}

export interface RpcInfo {
  id: string | number | null;
  method?: string;
  toolName?: string;
}

/** Reads the JSON-RPC id, method and tool name from a request body, tolerating anything. */
export function parseRpc(body: Uint8Array, contentType: string | null): RpcInfo {
  if (body.length === 0 || body.length > 1024 * 1024) return { id: null };
  if (contentType && !contentType.toLowerCase().includes("json")) return { id: null };
  try {
    const msg = JSON.parse(new TextDecoder().decode(body));
    const one = Array.isArray(msg) ? msg[0] : msg;
    if (!one || typeof one !== "object") return { id: null };
    const id = typeof one.id === "string" || typeof one.id === "number" ? one.id : null;
    const method = typeof one.method === "string" ? one.method : undefined;
    const toolName = method === "tools/call" && typeof one.params?.name === "string" ? one.params.name : undefined;
    return { id, method, toolName };
  } catch {
    return { id: null };
  }
}

const json = (body: unknown, status: number) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json", "cache-control": "no-store" } });

/** A tool result with isError: true, which models can read and explain to the user. */
export function toolError(id: RpcInfo["id"], text: string): Response {
  return json({ jsonrpc: "2.0", id, result: { content: [{ type: "text", text }], isError: true } }, 200);
}

export function rpcError(id: RpcInfo["id"], code: number, message: string, status: number): Response {
  return json({ jsonrpc: "2.0", id, error: { code, message } }, status);
}
