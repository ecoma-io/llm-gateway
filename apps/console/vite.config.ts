// vitest's defineConfig re-exports vite's with the `test` block typed, so one
// config serves both runners — `vite build` ignores `test`, `vitest` reads it.
// jsdom, not happy-dom: the shell's tests mount real components, and jsdom is
// the DOM the wider Vue ecosystem tests against.
import { fileURLToPath, URL } from "node:url";

import tailwindcss from "@tailwindcss/vite";
import vue from "@vitejs/plugin-vue";
import { defineConfig } from "vitest/config";

export default defineConfig({
  // Loom's published global stylesheet is authored in Tailwind CSS 4's
  // CSS-first syntax (`@theme static`, `@source`); this is the official Vite
  // integration that compiles those tokens and the console's utility classes.
  plugins: [vue(), tailwindcss()],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  server: {
    // The console talks to the gateway same-origin (src/lib/api.ts): one
    // origin in production, and in development the dev server proxies the two
    // contract probes to the Go process on :8080 — so `pnpm dev:api` +
    // `pnpm dev:web` work together with no CORS on the API and no env
    // override. Point the console at a gateway elsewhere with
    // `VITE_API_BASE_URL`.
    proxy: {
      "/healthz": "http://localhost:8080",
      "/readyz": "http://localhost:8080",
    },
  },
  test: {
    environment: "jsdom",
  },
});
