# Agent guidance

This file is the contract every contributor — human or agentic — is held to in
this repository. It is deliberately short: it states the rules, and
[CONTRIBUTING.md](CONTRIBUTING.md) states the process. When the two overlap,
this file wins on rules and that one wins on mechanics.

## What this repository is

The Ecoma LLM Gateway: four applications in two planes, released as one unit.
The **Control Plane** (`apps/console`, `apps/console-api`) is where people sign
in, subscribe and inspect; the **Data Plane** (`apps/dataplane`,
`apps/dataplane-api`) is what serves LLM traffic. The split, its rules and its
open questions are in [ADR 0006](docs/adr/0006-control-plane-and-data-plane.md),
which wins over any summary here.

Today it is a scaffold — the foundation below is real, the product domains are
not built. Do not invent them speculatively: a domain arrives as a designed
change (contract first, see below), never as scaffolding someone left around.

## Layout

| Path                          | What lives there                                                                                                                                                                |
| ----------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `apps/console`                | The Vue 3 console — the Control Plane's presentation layer, and the only browser-reachable application.                                                                         |
| `apps/console-api`            | The Control Plane API. `cmd/console-api` is the entry point; `internal/` is the implementation.                                                                                 |
| `apps/dataplane`              | The Data Plane runtime — the OpenAI-compatible gateway. `cmd/dataplane` is the entry point.                                                                                     |
| `apps/dataplane-api`          | The Data Plane's management API. `cmd/dataplane-api` is the entry point; it holds no state of its own.                                                                          |
| `go.work`                     | The Go workspace over the three Go modules. Committed, so CI and contributors run the same commands.                                                                            |
| `packages/console-api-client` | The TypeScript client the console consumes, generated from `api/openapi/console.yaml`.                                                                                          |
| `api/openapi/`                | The three contracts, one per boundary — `console.yaml`, `dataplane.yaml`, `runtime.yaml` — with the wire shapes they share in `shared/`.                                        |
| `migrations/`                 | Database migrations — one lane per plane, `migrations/<plane>/`, holding ordered, reviewed `.up.sql`/`.down.sql` pairs (the Data Plane's bootstrap pair enables TimescaleDB).   |
| `deploy/`                     | Local development and integration fixtures for the backing infrastructure: `postgres/` (compose, migration runner, `verify.sh` suite) and `redis/` (disposable Valkey fixture). |
| `docs/`                       | Long-form documentation — `adr/` decision records and `architecture/` reference pages, indexed in `docs/README.md`.                                                             |
| `scripts/`                    | Repository gates.                                                                                                                                                               |
| `.github/workflows/`          | `ci.yml`, `analysis.yml`, `release.yml`.                                                                                                                                        |

## The rules

1. **Inspect before modifying.** Read the file you are about to change and the
   files around it before writing. A change that contradicts its neighbours is
   a defect even when it is green.
2. **The API contract is the OpenAPI document.** The contract for the surface
   you are changing — `api/openapi/console.yaml`, `dataplane.yaml` or
   `runtime.yaml` — changes first, the implementation follows. An endpoint that
   exists in code and not in the contract is a bug; a contract entry with no
   implementation is a bug. Each application's route table is compared against
   its own document by a test, in both directions, so neither can move alone.
   `dataplane.yaml` is the contract of **`dataplane-api`**, the façade; the
   listener `dataplane` opens is not that surface and does not implement that
   document.
   The private transport between the two is the one exception, and it is
   deliberate: it is an **implementation protocol**, stated in full in
   [docs/architecture/cross-plane-protocols.md](docs/architecture/cross-plane-protocols.md)
   and pinned on each side by its own protocol test plus that package's own
   route test, not a fourth OpenAPI document. It is a protocol between two
   processes of the same product that no external client and no browser
   reaches, so a document for it would be one nobody generates a client from
   and everybody forgets to update. `api/openapi/` therefore still holds
   exactly three documents: `console.yaml`, `dataplane.yaml` and
   `runtime.yaml`.
3. **The console consumes the contract, not the implementation.** Where it is
   practical, the frontend binds to types generated from the OpenAPI document
   rather than hand-writing shapes that mirror it. A hand-written copy is drift
   waiting to happen.
4. **The planes do not talk sideways.** An LLM request goes to the runtime and
   nowhere else: the Data Plane never calls the Control Plane, never imports
   its module, never reads its database, and never requires it to be running.
   The Control Plane reaches the Data Plane through `dataplane-api`, the
   management façade, which holds no state of its own: it carries each call
   across to the Data Plane's private management listener and answers in its own
   vocabulary rather than passing anything through — translating, not relaying
   (ADR 0006 §9). The Control Plane also reads what the Data Plane has already
   recorded, by pull, over that same façade.
   Every one of those calls is a management call the Data Plane can refuse, and
   none of them is on the runtime's request path. `internal/arch` in each Go
   module fails its `test` target when this is broken — the rule is enforced,
   not asked for, and the target is part of the required checks.
5. **Infrastructure stays behind explicit boundaries.** Database access,
   external providers, queues: each lives behind a port in
   `internal/ports/outbound/`, named for what it does, not for what it is, and
   is implemented under `internal/adapters/outbound/`. A change that reaches
   for an infrastructure client from application code widens that boundary —
   widen it on purpose, in the diff, or not at all.
6. **Behavioural changes arrive with tests.** A change to what an application
   or the console does lands with a test that fails without it. A green suite
   that cannot go red is not coverage.
7. **Database changes are migrations.** Schema arrives as a file in the lane
   of the plane that owns it — `migrations/control/` or
   `migrations/dataplane/` — ordered and reviewable. The lane is the
   database, so a file's directory is also the deployment decision about
   where it runs; there is no shared lane and no cross-plane migration.
   Never an ad-hoc edit, never a change only applied by hand.
8. **No direct commits to `main`.** The branch is protected; the ruleset
   enforces it. If you find a way to push to `main` directly, that is a defect
   to report, not a shortcut to use.
9. **Changes land through pull requests.** Branch, pull request, required
   checks green, merge through the queue. See CONTRIBUTING.md for the flow.
10. **Conventional Commits are required.** `type(scope): subject`, enforced by
    commitlint on every commit and on every pull request title. Scopes:
    `console`, `console-api`, `dataplane`, `dataplane-api`, `openapi`,
    `workspace`, `docs`, `deps`, `ci`.
11. **No dependency without justification.** A new import — npm or Go module —
    arrives with a reason in the diff: what it does, why the standard library
    and the existing tree cannot. "It is popular" is not a reason.

## Commands

The root `package.json` is the roster; `pnpm <script>` is the form.
`format`, `format:check`, `openapi:check`, `lint`, `test`, `typecheck`,
`build`, `check-projects`, `dev:console`, `dev:console-api`, `dev:dataplane`,
`dev:dataplane-api`. The Moon tasks behind `lint`/`test`/`typecheck`/`build`
live in `apps/*/moon.yml` and run per project; a single project's targets run
as `pnpm exec moon run console-api:lint` (every application declares the same
four names). The hooks (`lefthook.yml`) run format, lint and the projects gate
on commit, tests and the graph on push, commitlint on the message.

## Commits

[Conventional Commits](https://www.conventionalcommits.org/), with the scope
naming where the change lands (`console`, `console-api`, `dataplane`,
`dataplane-api`, `openapi`, `workspace`, `docs`, `deps`, `ci` — optional when
the change owns no surface). The pull request
title becomes the squash commit's subject, so it is held to the same rule.
AI-assisted commits carry a trailer — `Assisted-by: <tool>` or
`Generated-by: <tool>` — one per pull request, on the last commit.

## Execution order for coding agents

1. Read this file, then CONTRIBUTING.md, then the files your change touches.
2. Make the change: contract first when an API surface moves, tests beside
   the behaviour, migrations for schema.
3. Run the gates — `pnpm format:check && pnpm lint && pnpm test && pnpm typecheck && pnpm build` — and the Go
   tests directly if you touched a Go module (`cd apps/console-api && go test ./...`,
   and the same in the other two; `make go-test` loops over all three).
   `go vet` is not listed: golangci-lint in `pnpm lint` already runs govet
   (each module's `.golangci.yml` argues the roster).
4. Commit with a conventional message, push the branch, open the pull request
   against `main`, and let the required checks judge it.

A scope expansion you noticed along the way is a new issue, not a quiet
addition to the current pull request.
