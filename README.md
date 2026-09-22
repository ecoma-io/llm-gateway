# Ecoma LLM Gateway

A gateway for LLM traffic — routing requests to providers, and governing what
flows through — with a Vue 3 console for the people operating it. It is part of
the [Ecoma](https://github.com/ecoma-io/ecoma) organisation's product line.

**This repository is a scaffold.** The engineering foundation is real and
enforced — workspace, CI, static analysis, release automation, pull-request
governance — but the product domains (providers, routing, API keys, usage
accounting, billing) are deliberately not built yet. They land as ordinary
pull requests against the structure below, without reorganising the
repository.

## What is here today

| Path                       | What it is                                                                                                                                  |
| -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `apps/api`                 | The Go API service. Serves `GET /healthz` and `GET /readyz` from the standard library, with graceful SIGTERM/SIGINT shutdown. Nothing else. |
| `apps/web`                 | The Vue 3 + TypeScript console: Vite, Vue Router, Pinia. A minimal shell that boots, routes and builds — no product UI.                     |
| `api/openapi/openapi.yaml` | The API contract. Today it documents exactly the two health endpoints the service implements.                                               |
| `migrations/`              | Empty. Database changes, when they arrive, land here as migrations — never as out-of-band schema edits.                                     |
| `scripts/`                 | Repository gates (`check-projects.mjs`).                                                                                                    |
| `.github/workflows/`       | `ci.yml`, `analysis.yml`, `release.yml`.                                                                                                    |

## Working in the repository

Requirements: **Node ≥ 24** and **pnpm ≥ 11** (Corepack fetches the pinned
version via `packageManager`), and **Go 1.26+** for the API's targets.
`pnpm install` installs dependencies and the Git hooks.

```bash
pnpm format         # Prettier, in place
pnpm format:check   # Prettier, read-only — what CI runs
pnpm lint           # every project's lint target (ESLint; gofmt + golangci-lint)
pnpm test           # every project's test target (Vitest; go test)
pnpm typecheck      # every project's typecheck target (vue-tsc; go build)
pnpm build          # every project's build target (vite build; go build -o bin/gateway)
pnpm dev:web        # the console's Vite dev server
pnpm dev:api        # the API server via go run
```

The full contributor flow — hooks, commit conventions, how a pull request
lands — is in [CONTRIBUTING.md](CONTRIBUTING.md); the rules agents and
contributors are held to are in [AGENTS.md](AGENTS.md). A `Makefile` at the
root spells the same commands as `make` targets.

## Release

The repository is one release unit: [release-please](https://github.com/googleapis/release-please)
keeps a release pull request open against `main`, one version covers the whole
tree, and the tag is `v<version>`. Nothing publishes to any registry at
scaffold stage — the tag is the release.

## License

[Apache License 2.0](LICENSE).
