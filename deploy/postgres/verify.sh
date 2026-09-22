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
# The proofs, in order:
#   1. the pinned TimescaleDB image starts and accepts connections;
#   2. the image actually ships the timescaledb extension;
#   3. the migrations apply;
#   4. the recorded version is the newest migration file, and not dirty;
#   5. re-applying is a no-op, not an error;
#   6. PostgreSQL transaction semantics hold (rolled-back work leaves
#      nothing behind, committed work survives);
#   7. a migration that fails mid-file rolls back whole, records its target
#      version dirty, refuses further runs, and is recovered with force;
#   8. a full down roll returns the schema to clean;
#   9. the suite leaves the database up and fully migrated.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
compose_file="$script_dir/compose.yaml"

compose() { docker compose -f "$compose_file" "$@"; }

# psql_scalar runs one SQL statement in the database and prints its scalar
# result. The flags are the quiet ones: no headers, no row count, unaligned,
# one field — output that can be compared byte-for-byte. ON_ERROR_STOP makes
# a failed statement a failed psql, which (under set -e) is a failed suite
# rather than an empty string quietly compared against something.
psql_scalar() {
	compose exec -T postgres psql -U gateway -d gateway -v ON_ERROR_STOP=1 -qtAX -c "$1"
}

psql_script() {
	compose exec -T postgres psql -U gateway -d gateway -v ON_ERROR_STOP=1 -q
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

# migration_version returns the newest migration version present in the
# repository — the version a fully-migrated database must record. Leading
# zeros are stripped because golang-migrate stores the version as an integer
# (000001 is 1 in schema_migrations).
newest_migration_version() {
	# `|| true` guards the no-match case: under pipefail, ls exiting non-zero
	# would abort the suite inside the substitution, before the empty check
	# below ever ran.
	local newest_file
	newest_file="$(ls "$repo_root"/migrations/*.up.sql 2>/dev/null | sort | tail -n1 || true)"
	if [ -z "$newest_file" ]; then
		echo 'FAIL: no *.up.sql files under migrations/ — there is nothing to prove the pipeline with' >&2
		exit 1
	fi
	local base
	base="$(basename "$newest_file")"
	echo $((10#"${base%%_*}"))
}

# recorded_version reads the version the migration tool last recorded, 0 when
# nothing is applied — the two shapes (no table yet, table at zero) are one
# answer so callers need not care which they hit.
recorded_version() {
	if [ "$(psql_scalar "SELECT to_regclass('public.schema_migrations') IS NULL")" = "t" ]; then
		echo 0
		return
	fi
	psql_scalar 'SELECT COALESCE(max(version), 0) FROM schema_migrations'
}

newest="$(newest_migration_version)"

step "1/9 the database starts and accepts connections"
# --wait blocks on the healthcheck: a container whose port answers but whose
# PostgreSQL rejects transactions is not up, and this step refuses to call it
# up.
compose up -d --wait
assert_equals "SELECT 1 answers" "$(psql_scalar 'SELECT 1')" "1"

step "2/9 the pinned image ships the timescaledb extension"
assert_equals "timescaledb appears in pg_available_extensions" \
	"$(psql_scalar "SELECT count(*) FROM pg_available_extensions WHERE name = 'timescaledb'")" "1"

step "3/9 the migrations apply"
compose run --rm migrate up

step "4/9 the recorded version is the newest migration, and clean"
assert_equals "schema_migrations.version" \
	"$(psql_scalar 'SELECT version FROM schema_migrations')" \
	"$newest"
assert_equals "schema_migrations.dirty" \
	"$(psql_scalar 'SELECT dirty FROM schema_migrations')" \
	"f"
assert_nonempty "timescaledb is installed in the database" \
	"$(psql_scalar "SELECT extversion FROM pg_extension WHERE extname = 'timescaledb'")"

step "5/9 re-applying is a no-op, not an error"
compose run --rm migrate up
assert_equals "version is unchanged after a no-op apply" "$(recorded_version)" "$newest"

step "6/9 PostgreSQL transaction semantics hold"
# Two probes, because the migration safety model rests on both: DDL rolled
# back leaves nothing (migrations run inside transactions and a failed one
# must leave no half-applied schema), and DML rolled back leaves nothing
# while committed DML survives. The probe table is created and dropped by
# this suite alone — it is a probe, not schema, and never a business table.
# The leading DROP TABLE IF EXISTS makes the step honest on any leftover
# state: the probe proves rollback, it does not depend on a clean slate.
psql_script <<'SQL'
DROP TABLE IF EXISTS public._verify_tx_probe;
BEGIN;
CREATE TABLE public._verify_tx_probe (id integer NOT NULL PRIMARY KEY);
ROLLBACK;
SQL
assert_equals "a rolled-back CREATE TABLE leaves no table" \
	"$(psql_scalar "SELECT to_regclass('public._verify_tx_probe') IS NULL")" "t"

psql_script <<'SQL'
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
	"$(psql_scalar "SELECT string_agg(id::text, ' ' ORDER BY id) FROM public._verify_tx_probe")" \
	"1 3"
psql_script <<'SQL'
DROP TABLE public._verify_tx_probe;
SQL

step "7/9 a migration that fails fails whole, loudly, and stops the world"
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
# real migrations plus the probe, mounted over the runner's read-only source
# for this one step and deleted on the way out. The probe is renamed to one
# past the newest real migration, so the fixture never collides with the
# migration that arrives next.
probe_version=$((newest + 1))
probe_pair="$(printf '%06d' "$probe_version")_should_fail"
fixture_dir="$(mktemp -d)"
trap 'rm -rf "$fixture_dir"' EXIT
cp "$repo_root"/migrations/*.sql "$fixture_dir"/
# Copy the probe straight onto its runtime name: when the real migrations
# reach 000002 the fixture's own name is taken, and a rename onto itself is
# an error — so the fixture's committed name is never load-bearing.
cp "$script_dir"/fixtures/failing-migration/*.up.sql "$fixture_dir/$probe_pair.up.sql"
cp "$script_dir"/fixtures/failing-migration/*.down.sql "$fixture_dir/$probe_pair.down.sql"
expect_failure "the deliberately-failing migration exits non-zero" \
	compose run --rm -v "$fixture_dir":/migrations:ro migrate up </dev/null
assert_equals "the failed migration's target version is recorded dirty" \
	"$(psql_scalar 'SELECT version, dirty FROM schema_migrations')" "$probe_version|t"
assert_equals "the failed migration's first statement left no table" \
	"$(psql_scalar "SELECT to_regclass('public._verify_failed_probe') IS NULL")" "t"
expect_failure "a dirty database refuses another migration run" \
	compose run --rm migrate up </dev/null
assert_equals "the refusal changed nothing" \
	"$(psql_scalar 'SELECT version, dirty FROM schema_migrations')" "$probe_version|t"
# Recovery is exactly the move the README documents: force the version that
# is actually applied — the previous one, since the failure rolled back —
# then let the real migrations converge.
compose run --rm migrate force "$newest" </dev/null
assert_equals "force restores the actually-applied version, clean" \
	"$(psql_scalar 'SELECT version, dirty FROM schema_migrations')" "$newest|f"

step "8/9 a full down roll returns the schema to clean"
# </dev/null pins the non-interactive contract: the suite must never depend
# on who is holding a terminal. This is the same move a contributor makes;
# the suite just proves it ends where the safety model promises.
compose run --rm migrate down -all </dev/null
assert_equals "the recorded version is zero after a full roll-back" \
	"$(recorded_version)" "0"
assert_equals "the timescaledb extension is gone after a full roll-back" \
	"$(psql_scalar "SELECT count(*) FROM pg_extension WHERE extname = 'timescaledb'")" "0"

step "9/9 the suite leaves the database migrated, not half-torn-down"
compose run --rm migrate up
assert_equals "final recorded version" "$(recorded_version)" "$newest"

printf '\npersistence suite: green (database up and migrated on %s)\n' \
	"$(psql_scalar 'SELECT version()')"
