// vitest's defineConfig re-exports vite's with the `test` block typed, so one
// config serves both runners — `vite build` ignores `test`, `vitest` reads it.
// jsdom, not happy-dom: the shell's tests mount real components, and jsdom is
// the DOM the wider Vue ecosystem tests against.
import { fileURLToPath, URL } from "node:url";

import vue from "@vitejs/plugin-vue";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  test: {
    environment: "jsdom",
  },
});
