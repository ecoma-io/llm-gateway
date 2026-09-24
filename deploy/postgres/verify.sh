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
# the lane it acts on. Each lane proves its pipeline against its own
# database — the Data Plane's proofs run the fuller pipeline (apply,
# validate, fail, recover); the Control Plane's run apply, re-apply and full
# rollback on its foundation migration — and each lane then proves the other
# plane's database untouched.
#
# The proofs, in order:
#   1. both lanes' directories hold exactly what the runner will read —
#      versions 1..N, one .up.sql and one .down.sql per version, nothing
#      else — and the failing-migration fixture is in neither lane;
#   2. both plane databases start and accept connections;
#   3. the image actually ships the timescaledb extension;
#   4. the Data Plane's migrations apply;
#   5. the recorded version is the newest migration in that lane, and not
#      dirty;
#   6. re-applying is a no-op, not an error;
#   7. the Control Plane's foundation migration applies into its own
#      database — the ownership namespace and its comment, re-applied as a
#      no-op — and the two lanes are two databases: no history and no schema
#      crosses the boundary, and one fixture credential opens both;
#   8. PostgreSQL transaction semantics hold (rolled-back work leaves
#      nothing behind, committed work survives);
#   9. a migration that fails mid-file rolls back whole, records its target
#      version dirty, refuses further runs, and is recovered with force;
#  10. a full down roll returns both schemas to clean;
#  11. the suite leaves both databases up and fully migrated.
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

# lane_drift_check proves one lane's directory holds exactly what the runner
# will read: files named `NNNNNN_<name>.up.sql` or `NNNNNN_<name>.down.sql`
# and nothing else, exactly one `.up.sql` and one `.down.sql` per version, and
# versions forming 1..N with no gap and no duplicate. golang-migrate records
# versions, not contents, and refuses neither a gap nor a stray file — a lane
# that drifted would still apply, and nothing else in this repository would
# notice. This check is the drift detection the pipeline relies on: the
# accepted "no checksum" gap in deploy/postgres/README.md's safety model,
# narrowed to what a directory listing can prove. Pure shell over
# migrations/ — it needs no database, so it runs before the cluster starts
# and fails before anything is pulled.
lane_drift_check() {
	local lane="$1"
	local lane_dir="$repo_root/migrations/$lane"
	local file name version count position
	local -a ups=() downs=()

	# The shape check doubles as the non-empty check: an empty (or missing)
	# lane leaves this glob unexpanded, the literal `*` matches nothing it
	# should, and the suite fails here rather than at the runner with
	# `first .: file does not exist`. The direction suffix is part of the
	# shape, not an afterthought: a bare `NNNNNN_<name>.sql` carries no
	# direction, lands in neither list below, and would drift through every
	# other check here while the runner's source silently skipped it.
	for file in "$lane_dir"/*; do
		name="$(basename "$file")"
		case "$name" in
		[0-9][0-9][0-9][0-9][0-9][0-9]_*.up.sql | [0-9][0-9][0-9][0-9][0-9][0-9]_*.down.sql) ;;
		*)
			printf 'FAIL: migrations/%s/ holds %q — a lane holds nothing but files named NNNNNN_<name>.up.sql and NNNNNN_<name>.down.sql\n' "$lane" "$name" >&2
			exit 1
			;;
		esac
	done

	for file in "$lane_dir"/*.up.sql; do
		name="$(basename "$file" .up.sql)"
		ups+=("${name%%_*}")
	done
	for file in "$lane_dir"/*.down.sql; do
		name="$(basename "$file" .down.sql)"
		downs+=("${name%%_*}")
	done

	if [ "${#ups[@]}" -ne "${#downs[@]}" ]; then
		printf 'FAIL: migrations/%s/ holds %d up files against %d down files — every version ships both\n' \
			"$lane" "${#ups[@]}" "${#downs[@]}" >&2
		exit 1
	fi

	for version in "${ups[@]}"; do
		count=0
		for name in "${downs[@]}"; do
			if [ "$name" = "$version" ]; then
				count=$((count + 1))
			fi
		done
		if [ "$count" -ne 1 ]; then
			printf 'FAIL: migrations/%s/ version %s has %d down files — exactly one pair member per direction\n' \
				"$lane" "$version" "$count" >&2
			exit 1
		fi
	done

	# The sequence, not just the set: 1..N, so a gap (a hole where applied
	# history was removed) and a duplicate (a re-used number) both land here,
	# as a failed suite, rather than in a database.
	position=0
	for version in $(printf '%s\n' "${ups[@]}" | sort -n); do
		position=$((position + 1))
		if [ "$((10#$version))" -ne "$position" ]; then
			printf 'FAIL: migrations/%s/ versions must run 1..%d with no gap and no duplicate — %s is where the sequence breaks\n' \
				"$lane" "${#ups[@]}" "$version" >&2
			exit 1
		fi
	done

	printf 'ok: migrations/%s/ holds versions %06d..%06d, one pair per version, nothing else\n' \
		"$lane" 1 "${#ups[@]}"
}

newest="$(newest_migration_version "$dataplane_db")"
control_newest="$(newest_migration_version "$control_db")"

step "1/11 both lanes hold exactly what the runner will read, and nothing else"
# Applied history under migrations/ is immutable, and the runner above all
# trusts its directory: these two checks are what stands between a drifted
# lane and a database that applies the drift. The fixture half is its own
# claim — the deliberately-failing pair lives in deploy/postgres/fixtures/,
# and a copy that ever landed inside a lane would be a migration the tool
# could apply for real.
lane_drift_check "$control_db"
lane_drift_check "$dataplane_db"
assert_equals "the failing-migration fixture is in no lane" \
	"$(find "$repo_root"/migrations -type d -name failing-migration | wc -l | tr -d ' ')" "0"

step "2/11 both plane databases start and accept connections"
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

step "3/11 the pinned image ships the timescaledb extension"
assert_equals "timescaledb appears in pg_available_extensions" \
	"$(psql_scalar "$dataplane_db" "SELECT count(*) FROM pg_available_extensions WHERE name = 'timescaledb'")" "1"

step "4/11 the Data Plane's migrations apply"
migrate_lane "$dataplane_db" up

step "5/11 the recorded version is the newest migration in that lane, and clean"
assert_equals "schema_migrations.version" \
	"$(psql_scalar "$dataplane_db" 'SELECT version FROM schema_migrations')" \
	"$newest"
assert_equals "schema_migrations.dirty" \
	"$(psql_scalar "$dataplane_db" 'SELECT dirty FROM schema_migrations')" \
	"f"
assert_nonempty "timescaledb is installed in the database" \
	"$(psql_scalar "$dataplane_db" "SELECT extversion FROM pg_extension WHERE extname = 'timescaledb'")"

step "6/11 re-applying is a no-op, not an error"
migrate_lane "$dataplane_db" up
assert_equals "version is unchanged after a no-op apply" "$(recorded_version "$dataplane_db")" "$newest"

step "7/11 the Control Plane's lane builds its namespace in its own database, and no lane crosses"
# Two databases are a database ownership boundary (ADR 0006 §7): separate
# namespaces, separate connection targets, independent transactions and
# independent migration history, with no ordinary SQL statement spanning them.
# They are not a security/credential boundary — that is a production decision
# the README states, and this fixture does not demonstrate it. What this step
# pins is both halves of that boundary at once: the Control Plane's lane
# applies its foundation migration into its own database and proves the same
# pipeline properties the Data Plane's steps proved — recorded version, clean
# state, idempotent re-apply — and neither lane is able to put an object in
# the other plane's database. The control-side assertions here replace the
# absence this step used to assert (no schema_migrations table) on the day
# the Control Plane's lane landed its first migration, exactly as that
# assertion's comment promised: replaced, not silenced.
migrate_lane "$control_db" up
assert_equals "the Control Plane's recorded version is its lane's newest" \
	"$(recorded_version "$control_db")" "$control_newest"
assert_equals "the Control Plane's recorded state is clean" \
	"$(psql_scalar "$control_db" 'SELECT dirty FROM schema_migrations')" "f"
assert_equals "the ownership namespace exists in the Control Plane's database" \
	"$(psql_scalar "$control_db" "SELECT count(*) FROM pg_namespace WHERE nspname = '$control_db'")" "1"
assert_equals "the namespace carries the ownership comment, byte for byte" \
	"$(psql_scalar "$control_db" "SELECT obj_description('$control_db'::regnamespace, 'pg_namespace')")" \
	"Control Plane ownership namespace (ADR 0006 §7); owned by apps/console-api."
assert_equals "the namespace holds no tables — foundation is a namespace, not schema" \
	"$(psql_scalar "$control_db" "SELECT count(*) FROM pg_tables WHERE schemaname = 'control'")" "0"

migrate_lane "$control_db" up
assert_equals "re-applying the Control Plane's lane is a no-op too" \
	"$(recorded_version "$control_db")" "$control_newest"

# Isolation, asserted from both sides: the Control Plane's migration created
# the namespace and nothing else — its own database's public schema holds the
# migration history and no product table — the Data Plane's database carries
# no `control` schema for the other lane to have leaked one into, and each
# database records its own applied history, because golang-migrate writes
# versions into the database it migrated.
assert_equals "the Control Plane's public schema holds only its migration history" \
	"$(psql_scalar "$control_db" "SELECT tablename FROM pg_tables WHERE schemaname = 'public'")" "schema_migrations"
assert_equals "the Control Plane's public schema holds no second table" \
	"$(psql_scalar "$control_db" "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'")" "1"
assert_equals "the Data Plane's database has no control namespace" \
	"$(psql_scalar "$dataplane_db" "SELECT count(*) FROM pg_namespace WHERE nspname = '$control_db'")" "0"
assert_equals "each plane records its own applied history" \
	"$(psql_scalar "$dataplane_db" "SELECT to_regclass('public.schema_migrations') IS NOT NULL")|$(psql_scalar "$control_db" "SELECT to_regclass('public.schema_migrations') IS NOT NULL")" \
	"t|t"
# The credential side of that boundary, asserted rather than described: this
# fixture's one role reaches both databases, so "the fixture does not
# demonstrate credential isolation" is a fact the suite proves and the
# documentation cannot drift back into a stronger claim.
assert_equals "one fixture credential opens both plane databases" \
	"$(psql_scalar "$control_db" 'SELECT current_user')|$(psql_scalar "$dataplane_db" 'SELECT current_user')" \
	"gateway|gateway"

step "8/11 PostgreSQL transaction semantics hold"
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

step "9/11 a migration that fails fails whole, loudly, and stops the world"
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

step "10/11 a full down roll returns both schemas to clean"
# </dev/null pins the non-interactive contract: the suite must never depend
# on who is holding a terminal. This is the same move a contributor makes;
# the suite just proves it ends where the safety model promises. Each lane
# rolls back through the same door against its own database, and each proof
# mirrors its lane's up-proof: the thing the migration created is gone, and
# the recorded version is zero. The migration-history table a full down
# leaves behind — truncated, not dropped — is the same state both proofs
# accept.
migrate_lane "$dataplane_db" down -all </dev/null
assert_equals "the Data Plane's recorded version is zero after a full roll-back" \
	"$(recorded_version "$dataplane_db")" "0"
assert_equals "the timescaledb extension is gone after a full roll-back" \
	"$(psql_scalar "$dataplane_db" "SELECT count(*) FROM pg_extension WHERE extname = 'timescaledb'")" "0"
migrate_lane "$control_db" down -all </dev/null
assert_equals "the Control Plane's recorded version is zero after a full roll-back" \
	"$(recorded_version "$control_db")" "0"
assert_equals "the ownership namespace and its comment are gone after a full roll-back" \
	"$(psql_scalar "$control_db" "SELECT count(*) FROM pg_namespace WHERE nspname = '$control_db'")" "0"
assert_equals "the Control Plane's public schema is back to its migration history alone" \
	"$(psql_scalar "$control_db" "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'")" "1"

step "11/11 the suite leaves both databases migrated, not half-torn-down"
migrate_lane "$dataplane_db" up
migrate_lane "$control_db" up
assert_equals "final recorded version, Data Plane" "$(recorded_version "$dataplane_db")" "$newest"
assert_equals "final recorded version, Control Plane" "$(recorded_version "$control_db")" "$control_newest"

printf '\npersistence suite: green (both plane databases up and migrated on %s)\n' \
	"$(psql_scalar "$dataplane_db" 'SELECT version()')"
