# Agent guidance

This file is the contract every contributor — human or agentic — is held to in
this repository. It is deliberately short: it states the rules, and
[CONTRIBUTING.md](CONTRIBUTING.md) states the process. When the two overlap,
this file wins on rules and that one wins on mechanics.

## What this repository is

The Ecoma LLM Gateway: a Go API service and a Vue 3 console for routing and
governing LLM traffic, released as one unit. Today it is a scaffold — the
foundation below is real, the product domains are not built. Do not invent
them speculatively: a domain arrives as a designed change (contract first, see
below), never as scaffolding someone left around.

## Layout

| Path                       | What lives there                                                                     |
| -------------------------- | ------------------------------------------------------------------------------------ |
| `apps/api`                 | The Go service. `cmd/gateway` is the entry point; `internal/` is the implementation. |
| `apps/web`                 | The Vue 3 console.                                                                   |
| `api/openapi/openapi.yaml` | The API contract.                                                                    |
| `migrations/`              | Database migrations. Empty until a database exists.                                  |
| `scripts/`                 | Repository gates.                                                                    |
| `.github/workflows/`       | `ci.yml`, `analysis.yml`, `release.yml`.                                             |

## The rules

1. **Inspect before modifying.** Read the file you are about to change and the
   files around it before writing. A change that contradicts its neighbours is
   a defect even when it is green.
2. **The API contract is the OpenAPI document.** `api/openapi/openapi.yaml`
   changes first, the implementation follows. An endpoint that exists in code
   and not in the contract is a bug; a contract entry with no implementation is
   a bug.
3. **The console consumes the contract, not the implementation.** Where it is
   practical, the frontend binds to types generated from the OpenAPI document
   rather than hand-writing shapes that mirror it. A hand-written copy is drift
   waiting to happen.
4. **The Go service stays console-agnostic.** Nothing under `apps/api` imports
   or references Vue concepts, DOM shapes, or console presentation. Coupling
   the domain to the frontend's implementation details forecloses every other
   client.
5. **Infrastructure stays behind explicit boundaries.** Database access,
   external providers, queues: each lives behind an interface in `internal/`,
   named for what it does, not for what it is. A change that reaches for an
   infrastructure client from application code widens that boundary — widen it
   on purpose, in the diff, or not at all.
6. **Behavioural changes arrive with tests.** A change to what the service or
   the console does lands with a test that fails without it. A green suite
   that cannot go red is not coverage.
7. **Database changes are migrations.** Schema arrives as a file under
   `migrations/`, ordered and reviewable. Never an ad-hoc edit, never a
   change only applied by hand.
8. **No direct commits to `main`.** The branch is protected; the ruleset
   enforces it. If you find a way to push to `main` directly, that is a defect
   to report, not a shortcut to use.
9. **Changes land through pull requests.** Branch, pull request, required
   checks green, merge through the queue. See CONTRIBUTING.md for the flow.
10. **Conventional Commits are required.** `type(scope): subject`, enforced by
    commitlint on every commit and on every pull request title. Scopes: `web`,
    `api`, `openapi`, `workspace`, `docs`, `deps`, `ci`.
11. **No dependency without justification.** A new import — npm or Go module —
    arrives with a reason in the diff: what it does, why the standard library
    and the existing tree cannot. "It is popular" is not a reason.

## Commands

The root `package.json` is the roster; `pnpm <script>` is the form.
`format`, `format:check`, `lint`, `test`, `typecheck`, `build`,
`check-projects`, `dev:web`, `dev:api`. The Moon tasks behind `lint`/`test`/
`typecheck`/`build` live in `apps/*/moon.yml` and run per project; a single
project's targets run as `pnpm exec moon run web:lint` (or `api:...`). The
hooks (`lefthook.yml`) run format, lint and the projects gate on commit, tests
and the graph on push, commitlint on the message.

## Commits

[Conventional Commits](https://www.conventionalcommits.org/), with the scope
naming where the change lands (`web`, `api`, `openapi`, `workspace`, `docs`,
`deps`, `ci` — optional when the change owns no surface). The pull request
title becomes the squash commit's subject, so it is held to the same rule.
AI-assisted commits carry a trailer — `Assisted-by: <tool>` or
`Generated-by: <tool>` — one per pull request, on the last commit.

## Execution order for coding agents

1. Read this file, then CONTRIBUTING.md, then the files your change touches.
2. Make the change: contract first when the API surface moves, tests beside
   the behaviour, migrations for schema.
3. Run the gates — `pnpm format:check && pnpm lint && pnpm test && pnpm typecheck && pnpm build` — and the Go
   checks directly if you touched `apps/api` (`cd apps/api && go vet ./... && go test ./...`).
4. Commit with a conventional message, push the branch, open the pull request
   against `main`, and let the required checks judge it.

A scope expansion you noticed along the way is a new issue, not a quiet
addition to the current pull request.
