// Flat ESLint config. Prettier owns formatting — `eslint-config-prettier` is
// last so it switches off every stylistic rule the two would otherwise fight
// over, leaving ESLint to judge correctness only.
//
// The repository's JavaScript has three homes: the Vue console under
// `apps/web` (typed TypeScript in `.ts` and `.vue`, type-checked by
// `vue-tsc` through the typecheck target), the API client under
// `packages/api-client` (its generated sources get their own block below),
// and the plain-Node scripts and root configs (`.mjs`).
// `typescript-eslint`'s non-type-checked recommended set covers all of them:
// it is a strict superset of `eslint-js.recommended` carrying
// correctness-and-safety rules without demanding type information.
// `eslint-plugin-vue`'s
// flat/recommended adds the Vue-specific set for `.vue` files, with
// `vue-eslint-parser` reading the SFC and the TypeScript parser reading the
// `<script lang="ts">` blocks inside it.
//
// What is deliberately absent: Go files. ESLint cannot read them; `gofmt` and
// `golangci-lint` are the half of the lint target that owns them
// (`apps/api/moon.yml`).
import js from "@eslint/js";
import pluginVue from "eslint-plugin-vue";
import globals from "globals";
import prettier from "eslint-config-prettier";
import tseslint from "typescript-eslint";

export default [
  {
    ignores: [
      "node_modules/**",
      "**/dist/**",
      "**/coverage/**",
      "**/bin/**",
      ".moon/**",
      // Claude Code worktrees — full clones of this repository. Prettier
      // ignores them through `.gitignore`; ESLint reads only its own
      // `ignores`, so the same exclusion is stated here for its own reader.
      ".claude/worktrees/**",
    ],
  },

  js.configs.recommended,
  // The non-type-checked recommended set: every `js.configs.recommended`
  // rule with TypeScript-aware spelling and correctness, on plain syntax.
  ...tseslint.configs.recommended,
  ...pluginVue.configs["flat/recommended"],

  {
    // The console's sources, the shared scripts, and this config: everything
    // ESLint reads in this repository.
    files: ["**/*.{mjs,cjs,js,ts}"],
    languageOptions: {
      ecmaVersion: "latest",
      sourceType: "module",
      globals: { ...globals.browser, ...globals.node },
    },
    rules: {
      "no-unused-vars": "off",
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_", caughtErrorsIgnorePattern: "^_" },
      ],
      eqeqeq: ["error", "always", { null: "ignore" }],
      "prefer-const": "error",
      "no-var": "error",
    },
  },

  {
    // Vue single-file components: the plugin's config above registered
    // `vue-eslint-parser` for them; what it hands the `<script lang="ts">`
    // block is named here, so TypeScript inside a component is parsed by the
    // TypeScript parser rather than read as plain script.
    files: ["**/*.vue"],
    languageOptions: {
      globals: { ...globals.browser },
      parserOptions: {
        parser: tseslint.parser,
        sourceType: "module",
      },
    },
    rules: {
      "no-unused-vars": "off",
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_", caughtErrorsIgnorePattern: "^_" },
      ],
    },
  },

  {
    // Generated client sources (packages/api-client/src/generated) are the
    // bytes of @hey-api/openapi-ts — regenerated, never edited, when the
    // OpenAPI contract moves. Correctness rules that assume a human chose
    // every type (`no-explicit-any` in the vendored fetch/SSE core) would
    // have the lint gate fight the generator over code nobody writes here.
    // Prettier still owns their formatting, and everything outside this block
    // — including the curated surface in packages/api-client/src — keeps the
    // full rule set.
    files: ["packages/api-client/src/generated/**/*.ts"],
    rules: {
      "@typescript-eslint/no-explicit-any": "off",
      "@typescript-eslint/no-unused-vars": "off",
    },
    linterOptions: {
      reportUnusedDisableDirectives: "off",
    },
  },

  prettier,
];
