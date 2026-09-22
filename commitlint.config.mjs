// Conventional Commits, enforced by lefthook's commit-msg hook and — for the
// pull request title, which becomes the squash commit's subject — by CI.
//
// This repository is a workspace, so a scope carries real routing information:
// it names which app or surface a change lands in. It stays optional because a
// change to the toolchain itself belongs to no app. Rules and examples:
// CONTRIBUTING.md.
export default {
  extends: ["@commitlint/config-conventional"],
  rules: {
    "scope-enum": [
      2,
      "always",
      [
        // One entry per app under `apps/`, plus the surfaces that name a
        // change owning no app.
        "web",
        "api",
        // The OpenAPI contract at `api/openapi/` — a contract change is not an
        // implementation change, and the scope is how a reader tells them
        // apart.
        "openapi",
        "workspace",
        "docs",
        "deps",
        "ci",
      ],
    ],
    "body-max-line-length": [0],
  },
};
