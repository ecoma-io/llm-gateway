# Contributing to the Ecoma LLM Gateway

Thank you for being here. This document is the short version of everything a
pull request is judged on, so nothing about the process is a surprise.

By contributing you agree that your work is licensed under the
[Apache License 2.0](LICENSE), and that you have the right to grant that
license — see [Ownership of what you contribute](#ownership-of-what-you-contribute).

The rules an agent or contributor is held to beyond the process are in
[AGENTS.md](AGENTS.md) — read it before your first change.

## Setting up

Requirements: **Node ≥ 24** (`.node-version` pins the major) and **pnpm ≥ 11**
(pinned via `packageManager`, so Corepack fetches the right one). The three Go
modules' targets additionally need **Go** (the version directive in each
module's `go.mod` is that module's floor) and **golangci-lint** for their
`lint` targets.

```bash
git clone https://github.com/ecoma-io/llm-gateway.git
cd llm-gateway
pnpm install
```

`pnpm install` runs `lefthook install`, which is what puts the Git hooks in
place. If you have ever wondered why a repository's hooks did not run for you: it
is because that step was skipped. Do not skip it.

## The commands

| Command                  | What it does                                                                                                       |
| ------------------------ | ------------------------------------------------------------------------------------------------------------------ |
| `pnpm format`            | Prettier, in place                                                                                                 |
| `pnpm format:check`      | Prettier, read-only — what CI runs                                                                                 |
| `pnpm lint`              | Every project's `lint` target through Moon — ESLint for the console, gofmt + golangci-lint for the Go applications |
| `pnpm test`              | Every project's `test` target through Moon — Vitest for the console, `go test` for the Go applications             |
| `pnpm typecheck`         | Every project's `typecheck` target through Moon — `vue-tsc --noEmit` and `go build ./...`                          |
| `pnpm build`             | Every project's `build` target through Moon — `vite build` and the Go binaries                                     |
| `pnpm check-projects`    | Asserts every `apps/*` and `packages/*` directory is a project Moon can see, with the four targets                 |
| `pnpm openapi:check`     | Spectral at `--fail-severity=warn`, read-only — lints three `api/openapi/*.yaml`, resolving `shared/` by `$ref`    |
| `pnpm dev:console`       | The console's Vite dev server                                                                                      |
| `pnpm dev:console-api`   | The Control Plane API (`go run ./cmd/console-api` on :8080)                                                        |
| `pnpm dev:dataplane`     | The Data Plane runtime (`go run ./cmd/dataplane` on :8081)                                                         |
| `pnpm dev:dataplane-api` | The Data Plane management API (`go run ./cmd/dataplane-api` on :8082)                                              |

A `Makefile` at the root spells the same commands as `make` targets
(`make lint`, `make go-test`, …) — aliases, not a second definition. And a
single project's targets run directly:

```bash
pnpm exec moon run console:lint console:test console:typecheck console:build
pnpm exec moon run console-api-client:lint console-api-client:test console-api-client:typecheck console-api-client:build
pnpm exec moon run console-api:lint console-api:test console-api:typecheck console-api:build
pnpm exec moon run dataplane:lint dataplane:test dataplane:typecheck dataplane:build
pnpm exec moon run dataplane-api:lint dataplane-api:test dataplane-api:typecheck dataplane-api:build
```

Those rosters are exactly what CI runs
([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) — one definition of
green, in one place.

### Why `check-projects` exists, and what it would catch

Three states of `apps/` or `packages/` produce an identical exit 0 out of
`moon run`:

1. **Nothing is there.** Moon prints nothing to run and exits 0.
2. **A project is there but declares none of the four targets.** It is skipped
   in silence.
3. **A directory is there with sources but no `moon.yml`.** It is invisible to
   Moon entirely.

States 2 and 3 are the ones that cost you: the build stays green and nobody is
told an app is not being checked. `scripts/check-projects.mjs` turns both into
a red check, in CI and in the pre-commit hook. If you add an app, you will
meet it — it is telling you Moon cannot see what you just added.

## What the hooks do

| hook         | commands it runs, in order                                                                         |
| ------------ | -------------------------------------------------------------------------------------------------- |
| `pre-commit` | `prettier` over the staged files, re-staging what it rewrote; `eslint` over them; `check-projects` |
| `commit-msg` | `commitlint`, checking the message shape                                                           |
| `pre-push`   | `pnpm test`; `moon projects` to prove the graph still computes                                     |

Bypassing a hook with `--no-verify` is occasionally the right call during a
rebase. It is never the right way to land a change.

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org/), enforced by
commitlint.

```
<type>(<scope>): <subject>
```

**Types:** `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`,
`ci`, `chore`, `revert`.

**Scope is optional**, and when used it names where the change lands: `console`
(the Vue app), `console-api`, `dataplane`, `dataplane-api` (the three Go
applications, one scope each), `openapi` (the contracts), `workspace` (the
repository and its tooling), `docs`, `deps`, `ci`.

```
feat(dataplane): proxy a completion request to a provider
fix(console): keep the token field masked after a failed submit
chore(workspace): scaffold llm gateway monorepo
```

A breaking change is marked with `!` after the type or scope, and explained in
a `BREAKING CHANGE:` footer.

### If your commit was AI-assisted

Add a trailer naming the tool: `Assisted-by: <tool>`, or `Generated-by: <tool>`
where the tool produced substantially the whole commit. A pull request
description can be edited later and no clone carries it; the commit trailer
travels with the code.

**One trailer per pull request, on the last commit** — not one per commit.
Squashing concatenates the full message of every commit on the branch into the
body of the single commit that lands, trailers and all.

## Tests

Tests live beside the code they test: `*.spec.ts` under `apps/console/src` run on
Vitest, `_test.go` under each Go module run on the standard `go test`. No mocking
library is in the tree — the surface is small enough that tests drive real
handlers and real stores, and the day that stops being true is the day a test
double is justified in the diff that introduces it.

Two things a reviewer will check:

- **A test pins intent, not just current output.** If the logic that matters
  could change without failing your test, the test is not doing its job.
- **A test is titled by the behaviour it pins**, never by the phase of work
  that added it. `readyz answers ok while the scaffold has no dependencies`,
  not `health test fix round 2`.

Before trusting a new check, break the thing it checks and watch the test go
red. A test nobody has seen fail is a test nobody knows can.

## The contract

There are three contracts under `api/openapi/`, one per boundary:
`console.yaml` for the console's API, `dataplane.yaml` for the management
surface `dataplane-api` serves, and `runtime.yaml` for the OpenAI-compatible
runtime. `dataplane.yaml` is the management surface only — the
OpenAI-compatible paths are in `runtime.yaml` and nowhere else. Shapes two of
them genuinely share — the request ID, the probe schemas — live once in
`shared/` and are referenced by relative `$ref`, so a caller who learns one on
one surface has learned it on the other.

Errors are not shared, and that is deliberate. The console and the management
API answer with the `ErrorEnvelope` in `shared/errors.yaml` —
`{"error":{"code","message"},"request_id"}`, the console's envelope and the
management surface's. `runtime.yaml` references no envelope of ours and carries
instead the OpenAI-compatible `{"error":{"message","type","param","code"}}`
from `shared/runtime-errors.yaml`. The two vocabularies are independent, and a
change to one must not reach a client of the other: an OpenAI-compatible client
parses `error.type` and `error.message` because that is what the API it was
written against sends, and renaming those keys into a gateway envelope is not a
cosmetic difference — it is a client that can no longer read the failure
([ADR 0006](docs/adr/0006-control-plane-and-data-plane.md) §11).

Two checks stand behind the contracts, and neither subsumes the other:

- `pnpm openapi:check` — Spectral, at `--fail-severity=warn`, resolves every
  `$ref` and validates each document against the OpenAPI specification. A
  dangling reference, a self-contradicting schema or a malformed document is a
  non-zero exit. It is a step of its own in the console verify job, on every
  event including the merge queue, so a required check depends on it.
- the `contract_test.go` in each application — a route table compared against
  the text of its own document, in both directions. A document that is valid but
  describes an endpoint the process does not serve (or the reverse) fails here.

The owning contract moves first: a new endpoint's shape is written there and
reviewed before (or with) the implementation, never discovered in code review
after it. Where practical, the console binds to types generated from
`console.yaml` rather than hand-written copies of it — see
[AGENTS.md](AGENTS.md), "The rules". No browser client is generated from the
other two: `packages/console-api-client` is generated from `console.yaml`
alone, and `openapi-ts.config.ts` is unchanged.

Database schema lands as migrations in the lane that owns it —
`migrations/control/` or `migrations/dataplane/`, one directory per
database, ordered `.up.sql`/`.down.sql` pairs a reviewer reads, applied by
the golang-migrate runner and proven against a real database by the suite in
[`deploy/postgres/README.md`](deploy/postgres/README.md), never as
out-of-band edits.

## Opening a pull request

1. Branch from `main`.
2. Make the change, with tests, and run the full command list above.
3. Fill in the pull request template honestly — especially the verification
   steps. Writing "not verified" is a conversation; leaving it blank is a
   rejection.
4. Keep it focused. Unrelated cleanup found along the way is welcome as its
   own pull request — mixed into this one it makes the real change
   unreviewable.

Reviews come from a maintainer.

### How a pull request lands

**Squash, always.** Merge commits and rebase merges are switched off in
repository settings and refused by the branch rules, so "Squash and merge" is
the only button. Three things follow:

- **The pull request title becomes the subject of the commit on `main`**, so
  the title itself must be a valid Conventional Commit. CI checks it with the
  same commitlint configuration the `commit-msg` hook uses, so a valid message
  has one definition rather than two. The cheap discipline: keep the title
  byte-identical to the branch's last commit subject, which the hook already
  judged.
- **One release-worthy change per pull request.** A pull request holding a
  `feat:` and an unrelated `fix:` gets one subject line, so it announces one of
  them. If you have two, send two.
- **You do not need to sign your commits.** `main` requires signatures, and
  GitHub signs the squash commit it creates — the commits on your branch are
  never the ones that land, so no key, no setup, nothing to configure.

Approved pull requests with green required checks go into the **merge queue**
immediately rather than waiting to be batched — the queue is what re-runs the
checks against `main` plus everything ahead of it, which is where two
independently-green pull requests' semantic conflict surfaces.

## How a release happens

Nothing you need to do — but worth knowing, because it explains a pull request
you will see open on `main` that nobody wrote.

[release-please](https://github.com/googleapis/release-please) reads the
Conventional Commit subjects since the last tag and keeps one pull request
open holding the next version and the changelog it derived. **That pull
request is the release proposal**: merging it tags, and the tag is the
release — `v<version>`, one version for the whole repository, nothing
published to any registry yet. So the subject line you write is what decides
the next version number — `feat:` moves the minor, `fix:` the patch, and a `!`
or a `BREAKING CHANGE:` footer the major.

Two details that are easy to trip over:

- **Do not hand-edit `CHANGELOG.md` or the version in `package.json`.**
  release-please owns both and rewrites them on the next run. `CHANGELOG.md`
  is in `.prettierignore` for the same reason: its generated layout and
  Prettier's preferred one disagree, and neither yields.
- **The release pull request's title is `chore(workspace): release <version>`**,
  not release-please's default. The default names the target branch as the
  scope (`chore(main): …`), and `main` is not in `commitlint.config.mjs`'s
  `scope-enum` — the release pull request would fail a required check and
  could never merge.

- **The version can be forced for one release.** A `Release-As: <version>`
  footer on any commit between the current tag and the next release forces
  exactly that version once.

## Reporting problems

- **Bugs and proposals** — the issue forms. The questions they ask are the
  ones that decide whether something is actionable.
- **Security vulnerabilities** — never a public issue. Use the repository's
  private security advisory (Security tab → report a vulnerability).

## Ownership of what you contribute

You keep the copyright in your contribution and license it to the project under
Apache-2.0, which includes the patent grant that license carries.

Please only send work you have the right to send. If you are employed as a
developer, your employment agreement may assign what you write to your employer
even on your own time and on your own hardware — in which case you need their
permission before contributing, not after. Anything you did not write yourself,
including substantial output from an AI tool, must be disclosed as described
above.
