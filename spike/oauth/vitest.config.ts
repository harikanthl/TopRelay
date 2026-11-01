import { defineConfig } from "vitest/config";
import { cloudflareTest } from "@cloudflare/vitest-plugin";

export default defineConfig({
  plugins: [
    cloudflareTest({
      wrangler: { configPath: "./wrangler.jsonc" },
      miniflare: {
        bindings: { MCP_RESOURCE: "http://localhost/mcp", PAIR_CODE: "test-pair-code" },
      },
    }),
  ],
});
