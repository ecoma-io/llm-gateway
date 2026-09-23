# Thin aliases over the pnpm and Go commands. The canonical definitions live in
# package.json, apps/*/moon.yml and CONTRIBUTING.md — this file adds no command
# of its own, it only spells the common ones for the `make <target>` habit.
# Note make cannot spell `:` inside a target name, so `format:check` becomes
# `format-check` here.

# The Go modules under apps/, in the order a reader meets them: the Control
# Plane, then the Data Plane, then the Data Plane's management surface. Every
# go-* target below loops over this list, so adding an application is one line
# here and a `moon.yml` beside it — not a fourth copy of `cd ... && go test`.
GO_APPS := console-api dataplane dataplane-api

.DEFAULT_GOAL := help
.PHONY: help install format format-check lint test typecheck build check-projects \
	dev-console dev-console-api dev-dataplane dev-dataplane-api \
	go-fmt go-vet go-test

help: ## List the available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'

install: ## pnpm install --frozen-lockfile (also installs the git hooks)
	pnpm install --frozen-lockfile

format: ## Prettier, in place
	pnpm format

format-check: ## Prettier, read-only — what CI runs
	pnpm format:check

lint: ## Every project's lint target through Moon
	pnpm lint

test: ## Every project's test target through Moon
	pnpm test

typecheck: ## Every project's typecheck target through Moon
	pnpm typecheck

build: ## Every project's build target through Moon
	pnpm build

check-projects: ## Assert every apps/* and packages/* directory is a Moon project
	pnpm check-projects

dev-console: ## Run the Vue console dev server
	pnpm dev:console

dev-console-api: ## Run the Control Plane API (go run, :8080)
	pnpm dev:console-api

dev-dataplane: ## Run the Data Plane runtime (go run, :8081)
	pnpm dev:dataplane

dev-dataplane-api: ## Run the Data Plane management API (go run, :8082)
	pnpm dev:dataplane-api

go-fmt: ## gofmt -l over every Go module — empty output means clean
	@failed=0; \
	for app in $(GO_APPS); do \
		out="$$(cd apps/$$app && gofmt -l .)"; \
		if [ -n "$$out" ]; then echo "apps/$$app:"; echo "$$out"; failed=1; fi; \
	done; \
	exit $$failed

go-vet: ## go vet over every Go module
	@failed=0; \
	for app in $(GO_APPS); do \
		( cd apps/$$app && go vet ./... ) || failed=1; \
	done; \
	exit $$failed

go-test: ## go test over every Go module
	@failed=0; \
	for app in $(GO_APPS); do \
		( cd apps/$$app && go test ./... ) || failed=1; \
	done; \
	exit $$failed
