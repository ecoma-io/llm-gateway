# Ecoma LLM Gateway

A gateway for LLM traffic — routing requests to providers, and governing what
flows through — with a Vue 3 console for the people operating it. It is part of
the [Ecoma](https://github.com/ecoma-io/ecoma) organisation's product line.

The repository is four applications in two planes, split because the two
workloads have opposite operational profiles. The **Control Plane**
(`apps/console`, `apps/console-api`) is where people sign in, subscribe and
inspect — ordinary request/response work where availability matters but
latency does not; the **Data Plane** (`apps/dataplane`, `apps/dataplane-api`)
serves LLM traffic and must stay up when everything else is down. An LLM
request never traverses the Control Plane, and the runtime never requires the
console to be running.
[ADR 0006](docs/adr/0006-control-plane-and-data-plane.md) is the decision
record.

**This repository is a scaffold.** The engineering foundation is real and
enforced — workspace, CI, static analysis, release automation, pull-request
governance — but the product domains (providers, routing, API keys, usage
accounting, billing) are deliberately not built yet. They land as ordinary
pull requests against the structure below, without reorganising the
repository.

## What is here today

| Path                          | What it is                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| ----------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `apps/console`                | The Vue 3 + TypeScript console: Vite, Vue Router, Pinia, styled by `@ecoma-io/loom`. Today it is the app shell — navigation, light/dark/system theming — plus one page surfacing the two contract probes.                                                                                                                                                                                                                                                                                                                                                    |
| `apps/console-api`            | The **Control Plane API** (port 8080): the console's backend, and the only application that owns Control Plane state. Serves `GET /healthz`, `GET /readyz` and `GET /version` from the standard library, with graceful SIGTERM/SIGINT shutdown.                                                                                                                                                                                                                                                                                                              |
| `apps/dataplane`              | The **Data Plane runtime** (port 8081): the OpenAI-compatible gateway LLM traffic arrives at. Serves the same three probes today; the request path itself is not built. It also serves a second, private management listener — `DATAPLANE_MANAGEMENT_ADDR`, opened only when deployment asks for it — which serves the usage-fact feed the management façade reads and answers from its own values.                                                                                                                                                          |
| `apps/dataplane-api`          | The **Data Plane management API** (port 8082): the Data Plane's administrative façade, internal-only and reachable from the Control Plane. It holds no state of its own — it carries each management call across to the runtime's private listener and answers from its own values — and the Control Plane reads what the Data Plane has already recorded by pulling through it.                                                                                                                                                                             |
| `go.work`                     | The Go workspace over those three modules, so a contributor runs `go build ./...` inside one without `go mod tidy` reaching for a sibling.                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `packages/console-api-client` | The TypeScript client generated from `api/openapi/console.yaml` by `@hey-api/openapi-ts` — the console's only API surface, with a build-time check that fails if the committed client drifts from that one contract.                                                                                                                                                                                                                                                                                                                                         |
| `api/openapi/`                | The three contracts, one per boundary: `console.yaml` (the console's API), `dataplane.yaml` (the management façade's API — `dataplane-api`, `/internal/usage-events` included) and `runtime.yaml` (the OpenAI-compatible runtime). Probes are shared in `shared/`; errors are not. Today each documents its own probes; `runtime.yaml` additionally declares `POST /v1/chat/completions`, which answers `501`.                                                                                                                                               |
| `migrations/`                 | Database migrations — ordered, hand-authored `NNNNNN_name.up.sql`/`.down.sql` pairs applied by golang-migrate, one lane per plane (`migrations/control/`, `migrations/dataplane/`), because one lane per plane is one database per plane. The Control Plane's lane has established its `control` namespace (`000001_control_foundation`) and its identity ownership root (`000002_identity_foundation`); the Data Plane's lane opens with the `000001_timescaledb_bootstrap` pair, which enables the `timescaledb` extension and creates no business schema. |
| `deploy/`                     | Local development and integration fixtures for the backing infrastructure: `postgres/` (the TimescaleDB compose project — one cluster, the `control` and `dataplane` databases — a database ownership boundary, not a credential one — the migration runner and the `verify.sh` verification suite) and `redis/` (the disposable Valkey fixture).                                                                                                                                                                                                            |
| `docs/`                       | Longer-form documentation: [`adr/`](docs/adr) (accepted architecture decision records) and [`architecture/`](docs/architecture) (reference pages for the designed domain model), indexed in [`docs/README.md`](docs/README.md).                                                                                                                                                                                                                                                                                                                              |
| `scripts/`                    | Repository gates (`check-projects.mjs`).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `.github/workflows/`          | `ci.yml`, `analysis.yml`, `release.yml`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |

`GET /internal/usage-events` is served by two processes in a chain of three.
`dataplane-api` is the one that **contracts** it — `dataplane.yaml` is that
façade's document, and it is the surface the Control Plane opens a socket to —
and the façade answers it from the runtime's private management listener, which
serves the feed in turn. That second hop, between two processes of the same
product, is an implementation protocol rather than a contract: it is stated in
[docs/architecture/cross-plane-protocols.md](docs/architecture/cross-plane-protocols.md)
and pinned on each side by a protocol test, it is not a fourth OpenAPI document,
and it is not a surface any browser or generated client reaches.

## Working in the repository

Requirements: **Node ≥ 24** and **pnpm ≥ 11** (Corepack fetches the pinned
version via `packageManager`), and **Go 1.26+** for the three modules' targets.
`pnpm install` installs dependencies and the Git hooks.

```bash
pnpm format         # Prettier, in place
pnpm format:check   # Prettier, read-only — what CI runs
pnpm lint           # every project's lint target (ESLint; gofmt + golangci-lint)
pnpm test           # every project's test target (Vitest; go test)
pnpm typecheck      # every project's typecheck target (vue-tsc; go build)
pnpm build          # every project's build target (vite build; go build -o bin/…)
pnpm dev:console    # the console's Vite dev server
pnpm dev:console-api   # the Control Plane API via go run (:8080)
pnpm dev:dataplane     # the Data Plane runtime via go run (:8081)
pnpm dev:dataplane-api # the Data Plane management API via go run (:8082)
```

All four run at once: each application binds its own port by default. The
runtime's private management listener is not one of those ports — it stays
closed until `DATAPLANE_MANAGEMENT_ADDR` names an address, so a contributor
gets the LLM surface without an administrative one.
`pnpm dev:console-api` serves the probes on :8080, and the dev server
`pnpm dev:console` proxies `/healthz` and `/readyz` to it — so the console's
status page works against the local Control Plane API with no configuration.
To point the console at a gateway on another origin instead, set
`VITE_API_BASE_URL`.

The full contributor flow — hooks, commit conventions, how a pull request
lands — is in [CONTRIBUTING.md](CONTRIBUTING.md); the rules agents and
contributors are held to are in [AGENTS.md](AGENTS.md). A `Makefile` at the
root spells the same commands as `make` targets.

`main` is governed: no direct pushes, pull requests only, squash-merged
through the merge queue once `ci-gate` and `analysis-gate` are green.

## Release

The repository is one release unit: [release-please](https://github.com/googleapis/release-please)
keeps a release pull request open against `main`, one version covers the whole
tree, and the tag is `v<version>`. Nothing publishes to any registry at
scaffold stage — the tag is the release.

## License

[Apache License 2.0](LICENSE).
