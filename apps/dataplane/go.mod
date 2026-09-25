// The Data Plane runtime: the OpenAI-compatible gateway that serves LLM
// traffic, and the only application on the request hot path.
//
// The module path is spelled in full because Go's import path IS the name;
// living under apps/dataplane of the ecoma-io/llm-gateway monorepo is the
// accepted cost of the monorepo decision — the same arrangement every module
// in this organisation carries. The path is also what makes the plane
// boundary mechanical: the Control Plane lives in a different module, so an
// import of this one's internals from there does not compile, and this
// module's go.mod carries no require of any sibling.
//
// Nothing in this module may depend on the Control Plane being reachable. A
// runtime request is served — or refused — from state this process already
// holds; the Control Plane is a publisher of configuration and a consumer of
// usage facts, never a hop on the path (ADR 0006 §4).
module github.com/ecoma-io/llm-gateway/apps/dataplane

go 1.26

// valkey-go is the sole RESP client. deploy/redis/README.md records why it is
// the smallest reasonable dependency and why Valkey is selected; the Go
// standard library has no Redis-compatible client.
//
// pgx is the PostgreSQL driver behind the persistence port's database/sql
// shape. There is no PostgreSQL driver in the standard library, and
// database/sql is the port's decided vocabulary — a transaction must be
// expressible — so the driver must speak it: lib/pq is in maintenance mode,
// and pgx's stdlib package is the maintained database/sql driver, registered
// under the name "pgx" by the one blank import in
// internal/adapters/outbound/postgres. Nothing else in this module names the
// driver or any of the indirect modules below.
require (
	github.com/jackc/pgx/v5 v5.11.0
	github.com/valkey-io/valkey-go v1.0.78
)

// golang-migrate is the schema-change tool deploy/postgres chose (its README
// records why, and the pinned runner image is this exact version) — required
// here only so the catalog integration tier can ensure the dataplane lane's
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
