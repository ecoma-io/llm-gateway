# Thin aliases over the pnpm and Go commands. The canonical definitions live in
# package.json, apps/*/moon.yml and CONTRIBUTING.md — this file adds no command
# of its own, it only spells the common ones for the `make <target>` habit.
# Note make cannot spell `:` inside a target name, so `format:check` becomes
# `format-check` here.

.DEFAULT_GOAL := help
.PHONY: help install format format-check lint test typecheck build check-projects \
	dev-web dev-api go-fmt go-vet go-test

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

check-projects: ## Assert every apps/* directory is a project Moon can see
	pnpm check-projects

dev-web: ## Run the Vue console dev server
	pnpm dev:web

dev-api: ## Run the Go API server (go run)
	pnpm dev:api

go-fmt: ## gofmt -l over the Go module — empty output means clean
	cd apps/api && gofmt -l .

go-vet: ## go vet over the Go module
	cd apps/api && go vet ./...

go-test: ## go test over the Go module
	cd apps/api && go test ./...
