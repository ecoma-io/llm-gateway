// The Control Plane API: the console's backend, and the only application that
// owns Control Plane state.
//
// The module path is spelled in full because Go's import path IS the name;
// living under apps/console-api of the ecoma-io/llm-gateway monorepo is the
// accepted cost of the monorepo decision — the same arrangement every module
// in this organisation carries. The path is also what makes the plane
// boundary mechanical: the runtime lives in a different module, so an import
// of this one's internals from there does not compile.
module github.com/ecoma-io/llm-gateway/apps/console-api

go 1.26

// valkey-go is the sole RESP client. deploy/redis/README.md records why it is
// the smallest reasonable dependency and why Valkey is selected; the Go
// standard library has no Redis-compatible client.
require github.com/valkey-io/valkey-go v1.0.78

// pgx is the PostgreSQL driver behind the persistence adapter. The standard
// library has no PostgreSQL driver of its own, lib/pq is in maintenance mode,
// and the port is spelled in database/sql — so pgx's stdlib wrapper is the
// one dependency that fits. internal/adapters/outbound/postgres/postgres.go
// states the justification in full, at the blank import that carries it. The
// identity integration tier's real-database tests (//go:build integration)
// import it directly for the same reason; every other import lives in a
// _test.go file, and no other production code links pgx.
require github.com/jackc/pgx/v5 v5.11.0

// golang-migrate is the schema-change tool deploy/postgres chose (its README
// records why, and the pinned runner image is this exact version) — required
// here only so the identity integration tier can ensure its own lane's
// migration history from the admin DSN, with bookkeeping identical to the
// runner's. Test imports only (//go:build integration); no production code
// links it, and the deploy path stays the pinned Docker image.
require github.com/golang-migrate/migrate/v4 v4.20.1

require (
	github.com/jackc/pgerrcode v0.0.0-20220416144525-469b46aa5efa // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)
