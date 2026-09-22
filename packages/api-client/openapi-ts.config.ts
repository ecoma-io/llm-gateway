// The client is generated, never hand-written: this config plus
// ../../api/openapi/openapi.yaml is the entire definition of src/generated,
// and the `build` target re-runs the generator and fails on any diff so a
// contract change cannot land without its regenerated client (AGENTS.md,
// "The rules", 3). The generator version is pinned exactly in package.json —
// output bytes are a function of (spec, config, versions), and a floating
// caret would let a release rewrite the tree overnight.
import { defineConfig } from "@hey-api/openapi-ts";

export default defineConfig({
  input: "../../api/openapi/openapi.yaml",
  output: {
    path: "src/generated",
    // Formatting runs in package scripts instead of here, with an explicit
    // ../../.prettierrc path. hey-api resolves post-processors from the output
    // folder, so a temporary freshness output would otherwise fall back to
    // Prettier defaults and falsely differ from the committed source. ESLint
    // stays in this package's lint target: its embedded runner disagrees with
    // the repository's ESLint 10 flat config, while the target uses CI's
    // exact config — one definition of clean, not two.
  },
  plugins: [
    // Types and operations from the contract, plus a fetch-based client with
    // no runtime beyond the platform's fetch — the console bundles source, so
    // there is no artifact to ship and no axios-shaped dependency to justify.
    "@hey-api/typescript",
    "@hey-api/sdk",
    "@hey-api/client-fetch",
  ],
});
