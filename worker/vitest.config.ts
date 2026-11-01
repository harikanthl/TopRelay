import { defineConfig } from "vitest/config";
import { cloudflareTest } from "@cloudflare/vitest-plugin";

// Roots are the docs/protocol.md test vectors.
export default defineConfig({
  plugins: [
    cloudflareTest({
      wrangler: { configPath: "./wrangler.jsonc" },
      miniflare: {
        bindings: {
          PUBLIC_URL: "http://localhost",
          SERVICES: JSON.stringify([
            { name: "notes", kind: "mcp", gen: 1 },
            { name: "files", kind: "http", gen: 1 },
            { name: "rotated", kind: "mcp", gen: 2 },
          ]),
          AGENT_ROOT: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
          BEARER_ROOT: "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8",
        },
      },
    }),
  ],
});
