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
        // One entry per application under `apps/`, plus the surfaces that
        // name a change owning no app. The four applications are listed
        // individually rather than by plane: a reviewer reading a log wants to
        // know which one moved, and a scope that said only "control plane"
        // would leave the console's change and its backend's change looking
        // alike.
        "console",
        "console-api",
        "dataplane",
        "dataplane-api",
        // The OpenAPI contracts at `api/openapi/` — a contract change is not
        // an implementation change, and the scope is how a reader tells them
        // apart. One scope covers all three documents: they are one boundary
        // decision, and splitting the scope would invite them to drift.
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
