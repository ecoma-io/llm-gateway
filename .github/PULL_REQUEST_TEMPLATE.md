## Description

<!-- What changes, and why. Link the issue this closes. -->

Closes #

## Type of change

- [ ] Bug fix (non-breaking change fixing an issue)
- [ ] New capability in the gateway API
- [ ] Console (web) change
- [ ] OpenAPI contract change
- [ ] Breaking change (a consumer must edit configuration or code to upgrade)
- [ ] Documentation
- [ ] Build, CI, or repository tooling

## Contract impact

<!-- The OpenAPI document (api/openapi/openapi.yaml) is the contract. Say what
a consumer of the API sees after this change. Write "none" explicitly rather
than leaving it out. -->

- [ ] No API surface changes
- [ ] The API surface changes, and the change is described above and reflected in `api/openapi/openapi.yaml`

## How this was verified

<!-- What you actually ran and saw, not what should happen. -->

**Steps:**

1.
2.

- [ ] Tests added or updated for behavioural changes, and I watched the new one fail before it passed
- [ ] `pnpm format:check`, `pnpm lint`, `pnpm test`, `pnpm typecheck` and `pnpm build` all pass locally

## Checklist

- [ ] I have self-reviewed this diff
- [ ] Documentation is updated in the same pass as the behaviour it describes
- [ ] No unrelated changes are included
- [ ] I have the right to contribute this work under the Apache License 2.0

## AI-assisted development

- [ ] This pull request is AI-assisted (drafted or substantially written by an AI coding agent)
- [ ] The pull request carries exactly one disclosure trailer, on its last commit: `Assisted-by: <tool>`, or `Generated-by: <tool>` where the tool produced substantially the whole commit

<!-- Name the tool and model, e.g. "Claude Code". A description can be edited
later and no clone carries it — the commit trailer travels with the code. -->
