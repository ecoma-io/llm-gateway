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
    // The console talks to its backend same-origin (src/lib/api.ts): one
    // origin in production, and in development the dev server proxies the two
    // contract probes to the Control Plane API on :8080 — so `pnpm
    // dev:console-api` + `pnpm dev:console` work together with no CORS on the
    // API and no env override. The runtime's :8081 is deliberately absent from
    // this list: an inference request is not a browser call, and a console
    // that could reach `/v1/*` through its own origin would be the hop the
    // plane split removes (ADR 0006 §4). Point the console at a console-api
    // elsewhere with `VITE_API_BASE_URL`.
    proxy: {
      "/healthz": "http://localhost:8080",
      "/readyz": "http://localhost:8080",
    },
  },
  test: {
    environment: "jsdom",
  },
});
