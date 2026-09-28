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
    // origin in production, and in development every path the console asks for
    // is proxied to the Control Plane API on :8080 — so `pnpm dev:console-api`
    // + `pnpm dev:console` work together with no CORS on the API and no env
    // override. The runtime's :8081 is deliberately absent from this list: an
    // inference request is not a browser call, and a console that could reach
    // `/v1/*` through its own origin would be the hop the plane split removes
    // (ADR 0006 §4). Point the console at a console-api elsewhere with
    // `VITE_API_BASE_URL`.
    //
    // The list is the console-api's route table, not a guess: the three probes,
    // `/auth` (sign-in, the session read, sign-out), `/account`, `/users`,
    // `/api-keys`, `/plans`, `/subscriptions`, `/entitlements`,
    // `/funding-buckets` (whose prefix also covers that bucket's ledger) and
    // `/reconciliation`. A path added to api/openapi/console.yaml without a
    // rule here is a dev-server 404 that looks exactly like a backend failure.
    proxy: {
      "/healthz": "http://localhost:8080",
      "/readyz": "http://localhost:8080",
      "/version": "http://localhost:8080",
      "/auth": "http://localhost:8080",
      "/account": "http://localhost:8080",
      "/users": "http://localhost:8080",
      "/api-keys": "http://localhost:8080",
      "/plans": "http://localhost:8080",
      "/subscriptions": "http://localhost:8080",
      "/entitlements": "http://localhost:8080",
      "/funding-buckets": "http://localhost:8080",
      "/reconciliation": "http://localhost:8080",
    },
  },
  test: {
    environment: "jsdom",
  },
});
