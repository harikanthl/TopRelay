// Builds the single-file Worker module the toprelay agent uploads (docs/protocol.md "Worker configuration"),
// and copies it to agent/internal/deploy/relay.js, where the Go binary embeds it.
import { build } from "esbuild";
import { copyFileSync, mkdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const out = join(root, "dist", "relay.js");
const agentCopy = join(root, "..", "agent", "internal", "deploy", "relay.js");

await build({
  entryPoints: [join(root, "src", "index.ts")],
  outfile: out,
  bundle: true,
  format: "esm",
  platform: "neutral",
  target: "es2024",
  conditions: ["workerd", "worker", "browser"],
  mainFields: ["module", "main"],
  external: ["cloudflare:*"],
  minify: false,
  legalComments: "inline",
  banner: { js: "// TopRelay relay Worker. Built from worker/src by worker/scripts/bundle.mjs. Apache-2.0." },
});

const code = readFileSync(out, "utf8");
const nodeImports = code.match(/from\s+["']node:[^"']+["']/g);
if (nodeImports) throw new Error(`bundle imports Node built-ins (needs nodejs_compat): ${nodeImports.join(", ")}`);
if (!/export\s*\{[^}]*\bRelay\b/.test(code)) throw new Error("bundle does not export the Relay class");

mkdirSync(dirname(agentCopy), { recursive: true });
copyFileSync(out, agentCopy);
console.log(`relay.js: ${(statSync(out).size / 1024).toFixed(1)} KiB → ${agentCopy}`);
