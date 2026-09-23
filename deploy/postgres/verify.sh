#!/usr/bin/env bash
# The persistence integration suite of the Ecoma LLM Gateway.
#
# One definition of green for the whole database foundation, run by
# contributors (bash deploy/postgres/verify.sh) and — verbatim — by whatever
# integration lane the workflow-owning change wires up, against the same
# compose project. Every step asserts something the gateway's migration
# safety model depends on, and every assertion fails the suite loudly — a
# step that cannot fail is a step that proves nothing, and none is allowed
# in here.
#
# The cluster holds two databases, one per plane, and every step below names
# the lane it acts on. The Data Plane's lane is the one with migrations to
# apply, so it carries the migration proofs; the Control Plane's lane is
# asserted from the outside — that its database exists, that it answers, and
# that the other plane's lane has put nothing in it.
#
# The proofs, in order:
#   1. both plane databases start and accept connections;
#   2. the image actually ships the timescaledb extension;
#   3. the Data Plane's migrations apply;
#   4. the recorded version is the newest migration in that lane, and not
#      dirty;
#   5. re-applying is a no-op, not an error;
#   6. the two lanes are two databases — the Data Plane's applied history is
#      in the Data Plane's database and nowhere else — and one fixture
#      credential opens both;
#   7. PostgreSQL transaction semantics hold (rolled-back work leaves
#      nothing behind, committed work survives);
#   8. a migration that fails mid-file rolls back whole, records its target
#      version dirty, refuses further runs, and is recovered with force;
#   9. a full down roll returns the schema to clean;
#   10. the suite leaves the database up and fully migrated.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
compose_file="$script_dir/compose.yaml"

# The two lanes, as `migrations/README.md` defines them: the directory under
# migrations/ and the database it is applied to carry the same name, which is
# why one word names both everywhere below.
control_db=control
dataplane_db=dataplane

compose() { docker compose -f "$compose_file" "$@"; }

# psql_scalar runs one SQL statement in one lane's database and prints its
# scalar result. The flags are the quiet ones: no headers, no row count,
# unaligned, one field — output that can be compared byte-for-byte.
# ON_ERROR_STOP makes a failed statement a failed psql, which (under set -e)
# is a failed suite rather than an empty string quietly compared against
# something.
psql_scalar() {
	local database="$1" statement="$2"
	compose exec -T postgres psql -U gateway -d "$database" -v ON_ERROR_STOP=1 -qtAX -c "$statement"
}

psql_script() {
	local database="$1"
	compose exec -T postgres psql -U gateway -d "$database" -v ON_ERROR_STOP=1 -q
}

# migrate_lane runs the pinned runner against one lane. The lane variable is
# what the compose service reads for both `-path` and `-database`
# (compose.yaml), so this is the only place a lane is named.
migrate_lane() {
	local lane="$1"
	shift
	GATEWAY_MIGRATE_LANE="$lane" compose run --rm migrate "$@"
}

# migrate_probe runs the runner against a temporary migrations tree — a
# lane's real files plus the deliberately-failing fixture — which is the one
# invocation that carries an extra `run` option. The option sits between
# `run` and the service name because that is the order the compose CLI
# accepts, which is why this is a second function rather than an argument to
# the one above.
migrate_probe() {
	local lane="$1" dir="$2"
	shift 2
	GATEWAY_MIGRATE_LANE="$lane" compose run --rm -v "$dir":/migrations:ro migrate "$@"
}

step() { printf '\n=== %s\n' "$1"; }

assert_equals() {
	local label="$1" got="$2" want="$3"
	if [ "$got" != "$want" ]; then
		printf 'FAIL: %s — got %q, want %q\n' "$label" "$got" "$want" >&2
		exit 1
	fi
	printf 'ok: %s (%s)\n' "$label" "$got"
}

assert_nonempty() {
	local label="$1" got="$2"
	if [ -z "$got" ]; then
		printf 'FAIL: %s — got an empty string, want a value\n' "$label" >&2
		exit 1
	fi
	printf 'ok: %s (%s)\n' "$label" "$got"
}

# expect_failure runs a command that must exit non-zero and fails the suite
# if it does not. The failure paths are load-bearing here: a migration run
# that cannot fail is a migration run that proves nothing.
expect_failure() {
	local label="$1"
	shift
	if "$@"; then
		printf 'FAIL: %s — the command that had to fail exited zero\n' "$label" >&2
		exit 1
	fi
	printf 'ok: %s (failed as required)\n' "$label"
}

# newest_migration_version returns the newest migration version present in
# one lane — the version a fully-migrated database in that lane must record.
# Per lane, not per repository: golang-migrate records versions in the
# database it migrates, so each lane numbers its own files from 000001.
# Leading zeros are stripped because the tool stores the version as an
# integer (000001 is 1 in schema_migrations).
newest_migration_version() {
	local lane="$1" newest_file
	# `|| true` guards the no-match case: under pipefail, ls exiting non-zero
	# would abort the suite inside the substitution, before the empty check
	# below ever ran.
	newest_file="$(ls "$repo_root"/migrations/"$lane"/*.up.sql 2>/dev/null | sort | tail -n1 || true)"
	if [ -z "$newest_file" ]; then
		echo "FAIL: no *.up.sql files under migrations/$lane/ — there is nothing to prove the pipeline with" >&2
		exit 1
	fi
	local base
	base="$(basename "$newest_file")"
	echo $((10#"${base%%_*}"))
}

# recorded_version reads the version a lane's database last recorded, 0 when
# nothing is applied — the two shapes (no table yet, table at zero) are one
# answer so callers need not care which they hit.
recorded_version() {
	local database="$1"
	if [ "$(psql_scalar "$database" "SELECT to_regclass('public.schema_migrations') IS NULL")" = "t" ]; then
		echo 0
		return
	fi
	psql_scalar "$database" 'SELECT COALESCE(max(version), 0) FROM schema_migrations'
}

newest="$(newest_migration_version "$dataplane_db")"

step "1/10 both plane databases start and accept connections"
# --wait blocks on the healthcheck: a container whose port answers but whose
# PostgreSQL rejects transactions is not up, and this step refuses to call it
# up. The two databases are created by the cluster's own initdb script
# (initdb/), which runs once on an empty volume — so a volume created before
# this split has neither, and the fix is the `down -v` reset the README
# documents rather than anything this script could guess.
compose up -d --wait
assert_equals "the cluster serves two databases" \
	"$(psql_scalar postgres "SELECT count(*) FROM pg_database WHERE datname IN ('$control_db', '$dataplane_db')")" "2"
assert_equals "the Control Plane's database answers" "$(psql_scalar "$control_db" 'SELECT 1')" "1"
assert_equals "the Data Plane's database answers" "$(psql_scalar "$dataplane_db" 'SELECT 1')" "1"

step "2/10 the pinned image ships the timescaledb extension"
assert_equals "timescaledb appears in pg_available_extensions" \
	"$(psql_scalar "$dataplane_db" "SELECT count(*) FROM pg_available_extensions WHERE name = 'timescaledb'")" "1"

step "3/10 the Data Plane's migrations apply"
migrate_lane "$dataplane_db" up

step "4/10 the recorded version is the newest migration in that lane, and clean"
assert_equals "schema_migrations.version" \
	"$(psql_scalar "$dataplane_db" 'SELECT version FROM schema_migrations')" \
	"$newest"
assert_equals "schema_migrations.dirty" \
	"$(psql_scalar "$dataplane_db" 'SELECT dirty FROM schema_migrations')" \
	"f"
assert_nonempty "timescaledb is installed in the database" \
	"$(psql_scalar "$dataplane_db" "SELECT extversion FROM pg_extension WHERE extname = 'timescaledb'")"

step "5/10 re-applying is a no-op, not an error"
migrate_lane "$dataplane_db" up
assert_equals "version is unchanged after a no-op apply" "$(recorded_version "$dataplane_db")" "$newest"

step "6/10 the two lanes are two databases, and one credential opens both"
# Two databases are a database ownership boundary (ADR 0006 §7): separate
# namespaces, separate connection targets, independent transactions and
# independent migration history, with no ordinary SQL statement spanning them.
# They are not a security/credential boundary — that is a production decision
# the README states, and this fixture does not demonstrate it. What this step
# pins, and all it pins, is that the configuration actually puts the lanes in
# two databases: the Data Plane's applied history is recorded in the Data
# Plane's database and the other lane's database has been migrated by nobody.
# The control-side assertion is deliberately shaped as an absence, and that is
# what it pins until the Control Plane's lane lands its first migration — the
# day that file exists, this line is the one that says so, and it gets
# replaced rather than silenced.
assert_equals "the Data Plane's applied history is in the Data Plane's database" \
	"$(psql_scalar "$dataplane_db" "SELECT to_regclass('public.schema_migrations') IS NOT NULL")" "t"
assert_equals "the Control Plane's database has no applied history" \
	"$(psql_scalar "$control_db" "SELECT to_regclass('public.schema_migrations') IS NULL")" "t"
# The credential side of that boundary, asserted rather than described: this
# fixture's one role reaches both databases, so "the fixture does not
# demonstrate credential isolation" is a fact the suite proves and the
# documentation cannot drift back into a stronger claim.
assert_equals "one fixture credential opens both plane databases" \
	"$(psql_scalar "$control_db" 'SELECT current_user')|$(psql_scalar "$dataplane_db" 'SELECT current_user')" \
	"gateway|gateway"

step "7/10 PostgreSQL transaction semantics hold"
# Two probes, because the migration safety model rests on both: DDL rolled
# back leaves nothing (migrations run inside transactions and a failed one
# must leave no half-applied schema), and DML rolled back leaves nothing
# while committed DML survives. The probe table is created and dropped by
# this suite alone — it is a probe, not schema, and never a business table.
# The leading DROP TABLE IF EXISTS makes the step honest on any leftover
# state: the probe proves rollback, it does not depend on a clean slate.
psql_script "$dataplane_db" <<'SQL'
DROP TABLE IF EXISTS public._verify_tx_probe;
BEGIN;
CREATE TABLE public._verify_tx_probe (id integer NOT NULL PRIMARY KEY);
ROLLBACK;
SQL
assert_equals "a rolled-back CREATE TABLE leaves no table" \
	"$(psql_scalar "$dataplane_db" "SELECT to_regclass('public._verify_tx_probe') IS NULL")" "t"

psql_script "$dataplane_db" <<'SQL'
CREATE TABLE public._verify_tx_probe (id integer NOT NULL PRIMARY KEY);
INSERT INTO public._verify_tx_probe (id) VALUES (1);
BEGIN;
INSERT INTO public._verify_tx_probe (id) VALUES (2);
ROLLBACK;
BEGIN;
INSERT INTO public._verify_tx_probe (id) VALUES (3);
COMMIT;
SQL
assert_equals "committed rows survive, rolled-back rows do not" \
	"$(psql_scalar "$dataplane_db" "SELECT string_agg(id::text, ' ' ORDER BY id) FROM public._verify_tx_probe")" \
	"1 3"
psql_script "$dataplane_db" <<'SQL'
DROP TABLE public._verify_tx_probe;
SQL

step "8/10 a migration that fails fails whole, loudly, and stops the world"
# The safety model's central claims, executed rather than asserted:
#
#   Atomicity  — the probe pair's up file has a succeeding CREATE TABLE and
#                then a failing INSERT; one-transaction delivery must leave
#                neither.
#   Dirty      — the tool records the target version with dirty=true, where
#                it stays until a human reconciles.
#   Refusal    — with the state dirty, another run must not march on.
#   Recovery   — `force <the version actually applied>` clears the flag, and
#                the real migrations converge.
#
# The probe pair lives in fixtures/failing-migration/ — applied history is
# immutable — and reaches the runner as a temporary directory holding the
# whole migrations tree with the probe added to one lane, mounted over the
# runner's read-only source for this one step and deleted on the way out.
# The whole tree, not one lane: the runner is pointed at a path inside it, so
# a mount holding only the probe's lane would prove something the real layout
# does not do. The probe is renamed to one past the newest real migration in
# its lane, so the fixture never collides with the migration that arrives
# next.
probe_version=$((newest + 1))
probe_pair="$(printf '%06d' "$probe_version")_should_fail"
fixture_dir="$(mktemp -d)"
trap 'rm -rf "$fixture_dir"' EXIT
cp -R "$repo_root"/migrations/. "$fixture_dir"/
# Copy the probe straight onto its runtime name: when the real migrations
# reach 000002 the fixture's own name is taken, and a rename onto itself is
# an error — so the fixture's committed name is never load-bearing.
cp "$script_dir"/fixtures/failing-migration/*.up.sql "$fixture_dir/$dataplane_db/$probe_pair.up.sql"
cp "$script_dir"/fixtures/failing-migration/*.down.sql "$fixture_dir/$dataplane_db/$probe_pair.down.sql"
expect_failure "the deliberately-failing migration exits non-zero" \
	migrate_probe "$dataplane_db" "$fixture_dir" up </dev/null
assert_equals "the failed migration's target version is recorded dirty" \
	"$(psql_scalar "$dataplane_db" 'SELECT version, dirty FROM schema_migrations')" "$probe_version|t"
assert_equals "the failed migration's first statement left no table" \
	"$(psql_scalar "$dataplane_db" "SELECT to_regclass('public._verify_failed_probe') IS NULL")" "t"
expect_failure "a dirty database refuses another migration run" \
	migrate_lane "$dataplane_db" up </dev/null
assert_equals "the refusal changed nothing" \
	"$(psql_scalar "$dataplane_db" 'SELECT version, dirty FROM schema_migrations')" "$probe_version|t"
# Recovery is exactly the move the README documents: force the version that
# is actually applied — the previous one, since the failure rolled back —
# then let the real migrations converge.
migrate_lane "$dataplane_db" force "$newest" </dev/null
assert_equals "force restores the actually-applied version, clean" \
	"$(psql_scalar "$dataplane_db" 'SELECT version, dirty FROM schema_migrations')" "$newest|f"

step "9/10 a full down roll returns the schema to clean"
# </dev/null pins the non-interactive contract: the suite must never depend
# on who is holding a terminal. This is the same move a contributor makes;
# the suite just proves it ends where the safety model promises.
migrate_lane "$dataplane_db" down -all </dev/null
assert_equals "the recorded version is zero after a full roll-back" \
	"$(recorded_version "$dataplane_db")" "0"
assert_equals "the timescaledb extension is gone after a full roll-back" \
	"$(psql_scalar "$dataplane_db" "SELECT count(*) FROM pg_extension WHERE extname = 'timescaledb'")" "0"

step "10/10 the suite leaves the database migrated, not half-torn-down"
migrate_lane "$dataplane_db" up
assert_equals "final recorded version" "$(recorded_version "$dataplane_db")" "$newest"

printf '\npersistence suite: green (database up and migrated on %s)\n' \
	"$(psql_scalar "$dataplane_db" 'SELECT version()')"
