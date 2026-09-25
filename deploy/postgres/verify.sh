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
# rollback on their lane, whose identity step additionally exercises the
# identity schema's own rules at the SQL level — a constraint that has never
# refused a row is a comment, not a constraint — and each lane then proves
# the other plane's database untouched.
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
#   7. the Control Plane's lane applies into its own database — the
#      ownership namespace with the identity schema inside it, re-applied as
#      a no-op — and the two lanes are two databases: no history and no
#      schema crosses the boundary, and one fixture credential opens both;
#   8. the Control Plane's identity schema enforces its rules: lifecycle
#      states, the live-email uniqueness and its re-invite exception, the
#      revocation/state pairing, the token prefix grammar, and every foreign
#      key;
#   9. PostgreSQL transaction semantics hold (rolled-back work leaves
#      nothing behind, committed work survives);
#  10. a migration that fails mid-file rolls back whole, records its target
#      version dirty, refuses further runs, and is recovered with force;
#  11. a full down roll returns both schemas to clean;
#  12. the suite leaves both databases up and fully migrated.
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

# expect_lane_constraint_failure is the probe's core: one SQL batch against
# one lane's database that must fail, and by one particular constraint: the
# batch is wrapped in an explicit transaction whose ROLLBACK a successful run
# would reach (so even a probe that fails to fail leaves nothing behind), and
# the captured error output must name the constraint under test. A refusal
# for any other reason — a stale row's duplicate key, a mis-aimed seed —
# fails the suite with psql's actual output attached, because a step that
# cannot say which rule refused the row has not proven that rule.
expect_lane_constraint_failure() {
	local database="$1" label="$2" constraint="$3" batch="$4" output status=0
	output="$(compose exec -T postgres psql -U gateway -d "$database" \
		-v ON_ERROR_STOP=1 -q -c "BEGIN;
$batch;
ROLLBACK;" 2>&1)" || status=$?
	if [ "$status" -eq 0 ]; then
		printf 'FAIL: %s — the batch exited zero; the constraint never fired\n' "$label" >&2
		exit 1
	fi
	if ! printf '%s' "$output" | grep -qF -- "$constraint"; then
		printf 'FAIL: %s — the batch failed, but not by constraint %s; psql said:\n%s\n' \
			"$label" "$constraint" "$output" >&2
		exit 1
	fi
	printf 'ok: %s (refused by %s)\n' "$label" "$constraint"
}

# expect_constraint_failure probes the Control Plane's database — the lane
# every identity and commerce rule lives in.
expect_constraint_failure() {
	expect_lane_constraint_failure "$control_db" "$@"
}

# expect_dataplane_constraint_failure probes the Data Plane's database — the
# lane the runtime mirror half lives in. Same transactional wrapper, same
# verdict rule; only the database differs.
expect_dataplane_constraint_failure() {
	expect_lane_constraint_failure "$dataplane_db" "$@"
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

	# The content check that rides along: a migration file carries no BEGIN,
	# COMMIT or ROLLBACK of its own (migrations/README.md) — the runner
	# delivers each file as one simple query, one implicit transaction, and
	# a file-managed transaction would replace that guarantee, not add to
	# it: anything after the file's COMMIT would auto-commit statement by
	# statement, outside the atomic unit the safety model promises. The
	# control lane's 000002 pair predates the rule — the rule was written
	# into the README after that file had landed, and applied history is
	# immutable — so it is named here as the one allowed exception, and the
	# roster shrinks only when a lane rewrite ever retires that pair.
	for file in "$lane_dir"/*.sql; do
		name="$(basename "$file")"
		case "$lane/$name" in
		control/000002_identity_foundation.up.sql | control/000002_identity_foundation.down.sql) continue ;;
		esac
		if grep -qiE '^[[:space:]]*(BEGIN|COMMIT|ROLLBACK|START[[:space:]]+TRANSACTION)([[:space:]]|;)' "$file"; then
			printf 'FAIL: migrations/%s/%s carries its own transaction control — one file is one implicit transaction (migrations/README.md)\n' "$lane" "$name" >&2
			exit 1
		fi
	done

	printf 'ok: migrations/%s/ holds versions %06d..%06d, one pair per version, nothing else\n' \
		"$lane" 1 "${#ups[@]}"
}

newest="$(newest_migration_version "$dataplane_db")"
control_newest="$(newest_migration_version "$control_db")"

step "1/12 both lanes hold exactly what the runner will read, and nothing else"
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

step "2/12 both plane databases start and accept connections"
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

step "3/12 the pinned image ships the timescaledb extension"
assert_equals "timescaledb appears in pg_available_extensions" \
	"$(psql_scalar "$dataplane_db" "SELECT count(*) FROM pg_available_extensions WHERE name = 'timescaledb'")" "1"

step "4/12 the Data Plane's migrations apply"
migrate_lane "$dataplane_db" up

step "5/12 the recorded version is the newest migration in that lane, and clean"
assert_equals "schema_migrations.version" \
	"$(psql_scalar "$dataplane_db" 'SELECT version FROM schema_migrations')" \
	"$newest"
assert_equals "schema_migrations.dirty" \
	"$(psql_scalar "$dataplane_db" 'SELECT dirty FROM schema_migrations')" \
	"f"
assert_nonempty "timescaledb is installed in the database" \
	"$(psql_scalar "$dataplane_db" "SELECT extversion FROM pg_extension WHERE extname = 'timescaledb'")"

step "6/12 re-applying is a no-op, not an error"
migrate_lane "$dataplane_db" up
assert_equals "version is unchanged after a no-op apply" "$(recorded_version "$dataplane_db")" "$newest"

step "7/12 the Control Plane's lane builds its namespace in its own database, and no lane crosses"
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
assert_equals "the namespace holds exactly the identity, commerce and projection schemas' thirteen tables" \
	"$(psql_scalar "$control_db" "SELECT count(*) FROM pg_tables WHERE schemaname = 'control'")" "13"
assert_equals "the control tables are the identity, commerce and projection foundations' set" \
	"$(psql_scalar "$control_db" "SELECT string_agg(tablename, ',' ORDER BY tablename COLLATE \"C\") FROM pg_tables WHERE schemaname = 'control'")" \
	"account_payg,accounts,api_keys,entitlements,plan_grant_definitions,plan_versions,plans,projection_accounts,projection_api_keys,projection_changes,projection_revision,subscriptions,users"
assert_equals "the projection counter is a seeded singleton with a timeline epoch" \
	"$(psql_scalar "$control_db" "SELECT count(*) FROM control.projection_revision WHERE id = 1 AND last_revision = (SELECT count(*) FROM control.accounts) AND epoch IS NOT NULL")" \
	"1"
assert_equals "the projection's backfilled log and mirror carry one entry per account" \
	"$(psql_scalar "$control_db" "SELECT (SELECT count(*) FROM control.projection_changes) || '|' || (SELECT count(*) FROM control.projection_accounts)")" \
	"$(psql_scalar "$control_db" "SELECT count(*) FROM control.accounts")|$(psql_scalar "$control_db" "SELECT count(*) FROM control.accounts")"

migrate_lane "$control_db" up
assert_equals "re-applying the Control Plane's lane is a no-op too" \
	"$(recorded_version "$control_db")" "$control_newest"

# Isolation, asserted from both sides: the Control Plane's migrations built
# the namespace and the identity schema inside it — its own database's public
# schema holds the migration history and no product table, because the
# identity tables live in the `control` namespace — the Data Plane's database
# carries no `control` schema for the other lane to have leaked one into, and
# each database records its own applied history, because golang-migrate
# writes versions into the database it migrated.
assert_equals "the Control Plane's public schema holds only its migration history" \
	"$(psql_scalar "$control_db" "SELECT tablename FROM pg_tables WHERE schemaname = 'public'")" "schema_migrations"
assert_equals "the Control Plane's public schema holds no second table" \
	"$(psql_scalar "$control_db" "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'")" "1"
assert_equals "the Data Plane's database has no control namespace" \
	"$(psql_scalar "$dataplane_db" "SELECT count(*) FROM pg_namespace WHERE nspname = '$control_db'")" "0"
# The isolation block's mirror of step 5's installed-half proof: timescaledb
# lives in the Data Plane's database and none other. The bootstrap migration
# is the only file in the repository that spells CREATE EXTENSION, so this
# count stays zero unless a future migration crosses the lane boundary — the
# exact violation migrations/README.md and persistence.md call out, caught
# here rather than trusted to the lane's directory layout.
assert_equals "the Control Plane's database carries no timescaledb extension" \
	"$(psql_scalar "$control_db" "SELECT count(*) FROM pg_extension WHERE extname = 'timescaledb'")" "0"
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

step "8/12 both lanes' schemas enforce their rules"
# Every refusal below names the constraint it proves, and every probe —
# refusal or positive — seeds what it needs inside its own explicit
# transaction and rolls it back, so the probes are self-contained,
# order-independent and re-runnable: no probe depends on a state a previous
# one left, and none leaves one for the next. The probe rows use fixed
# UUIDv4-form ids, matching the grammar the schema itself demands. The
# Control Plane's tables are named in the `control` namespace its lane's
# foundation migration built; the Data Plane's projection mirror lives in
# that plane's `public` schema and is probed against its own database.

expect_constraint_failure "an account state outside the lifecycle is refused" accounts_state_valid "
INSERT INTO control.accounts (id, name, state, created_at, updated_at)
VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'zombie', now(), now())"

expect_constraint_failure "a user email outside the grammar is refused" users_email_shape "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
)
INSERT INTO control.users (id, account_id, email, state, created_at, updated_at)
SELECT 'b0000000-0000-4000-8000-0000000000b1', id, 'not an email', 'invited', now(), now() FROM account"

expect_constraint_failure "a second live user may not hold the account's email" users_account_live_email_key "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
), first_user AS (
  INSERT INTO control.users (id, account_id, email, state, created_at, updated_at)
  SELECT 'b0000000-0000-4000-8000-0000000000b1', id, 'probe@example.com', 'invited', now(), now() FROM account
  RETURNING account_id, email
)
INSERT INTO control.users (id, account_id, email, state, created_at, updated_at)
SELECT 'b0000000-0000-4000-8000-0000000000b2', account_id, email, 'invited', now(), now() FROM first_user"

# The re-invite exception is the uniqueness rule's other half, asserted
# positively: seed, remove, re-invite, read the answer, roll back.
assert_equals "a removed user's email may be invited again" \
	"$(psql_scalar "$control_db" "
BEGIN;
INSERT INTO control.accounts (id, name, state, created_at, updated_at)
VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now());
INSERT INTO control.users (id, account_id, email, state, created_at, updated_at)
VALUES ('b0000000-0000-4000-8000-0000000000b1', 'a0000000-0000-4000-8000-0000000000a1', 'probe@example.com', 'invited', now(), now());
UPDATE control.users SET state = 'removed' WHERE id = 'b0000000-0000-4000-8000-0000000000b1';
INSERT INTO control.users (id, account_id, email, state, created_at, updated_at)
VALUES ('b0000000-0000-4000-8000-0000000000b2', 'a0000000-0000-4000-8000-0000000000a1', 'probe@example.com', 'invited', now(), now());
SELECT count(*) FROM control.users
WHERE account_id = 'a0000000-0000-4000-8000-0000000000a1'
  AND email = 'probe@example.com'
  AND state = 'invited';
ROLLBACK;")" "1"

expect_constraint_failure "an active key with a revocation stamp is refused" api_keys_revocation_consistency "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
)
INSERT INTO control.api_keys (id, account_id, created_by, display_name, prefix, state, created_at, updated_at, revoked_at)
SELECT 'c0000000-0000-4000-8000-0000000000c1', id, NULL, 'verify probe', 'gw_c0000000-0000-4000-8000-0000000000c1_', 'active', now(), now(), now() FROM account"

expect_constraint_failure "a revoked key without a revocation stamp is refused" api_keys_revocation_consistency "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
)
INSERT INTO control.api_keys (id, account_id, created_by, display_name, prefix, state, created_at, updated_at, revoked_at)
SELECT 'c0000000-0000-4000-8000-0000000000c1', id, NULL, 'verify probe', 'gw_c0000000-0000-4000-8000-0000000000c1_', 'revoked', now(), now(), NULL FROM account"

expect_constraint_failure "a key prefix outside the grammar is refused" api_keys_prefix_shape "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
)
INSERT INTO control.api_keys (id, account_id, created_by, display_name, prefix, state, created_at, updated_at, revoked_at)
SELECT 'c0000000-0000-4000-8000-0000000000c1', id, NULL, 'verify probe', 'garbage', 'active', now(), now(), NULL FROM account"

expect_constraint_failure "a key prefix with non-v4 uuid nibbles is refused" api_keys_prefix_shape "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
)
INSERT INTO control.api_keys (id, account_id, created_by, display_name, prefix, state, created_at, updated_at, revoked_at)
SELECT 'c0000000-0000-4000-8000-0000000000c1', id, NULL, 'verify probe', 'gw_c0000000-0000-1000-8000-0000000000c1_', 'active', now(), now(), NULL FROM account"

expect_constraint_failure "a user under an unknown account is refused" users_account_id_fkey "
INSERT INTO control.users (id, account_id, email, state, created_at, updated_at)
VALUES ('b0000000-0000-4000-8000-0000000000b1', 'e0000000-0000-4000-8000-0000000000e1', 'orphan@example.com', 'invited', now(), now())"

expect_constraint_failure "a key under an unknown account is refused" api_keys_account_id_fkey "
INSERT INTO control.api_keys (id, account_id, created_by, display_name, prefix, state, created_at, updated_at, revoked_at)
VALUES ('c0000000-0000-4000-8000-0000000000c1', 'e0000000-0000-4000-8000-0000000000e1', NULL, 'verify probe', 'gw_c0000000-0000-4000-8000-0000000000c1_', 'active', now(), now(), NULL)"

expect_constraint_failure "a key created by an unknown user is refused" api_keys_created_by_fkey "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
)
INSERT INTO control.api_keys (id, account_id, created_by, display_name, prefix, state, created_at, updated_at, revoked_at)
SELECT 'c0000000-0000-4000-8000-0000000000c1', id, 'f0000000-0000-4000-8000-0000000000f1', 'verify probe', 'gw_c0000000-0000-4000-8000-0000000000c1_', 'active', now(), now(), NULL FROM account"

assert_equals "a well-formed account, user and key all insert" \
	"$(psql_scalar "$control_db" "
BEGIN;
INSERT INTO control.accounts (id, name, state, created_at, updated_at)
VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now());
INSERT INTO control.users (id, account_id, email, state, created_at, updated_at)
VALUES ('b0000000-0000-4000-8000-0000000000b1', 'a0000000-0000-4000-8000-0000000000a1', 'probe@example.com', 'invited', now(), now());
INSERT INTO control.api_keys (id, account_id, created_by, display_name, prefix, state, created_at, updated_at, revoked_at)
VALUES ('c0000000-0000-4000-8000-0000000000c1', 'a0000000-0000-4000-8000-0000000000a1', 'b0000000-0000-4000-8000-0000000000b1', 'verify probe', 'gw_c0000000-0000-4000-8000-0000000000c1_', 'active', now(), now(), NULL);
SELECT (SELECT count(*) FROM control.accounts WHERE id = 'a0000000-0000-4000-8000-0000000000a1') || '|' ||
       (SELECT count(*) FROM control.users WHERE id = 'b0000000-0000-4000-8000-0000000000b1') || '|' ||
       (SELECT count(*) FROM control.api_keys WHERE id = 'c0000000-0000-4000-8000-0000000000c1');
ROLLBACK;")" "1|1|1"

# The commerce half of the step, in the same voice: every refusal names the
# constraint it proves, every probe seeds what it needs in its own rolled-back
# transaction, and the probe ids are fixed UUIDv7-form values matching the
# grammar the commerce tables themselves demand. One rule deliberately has no
# probe here: a subscription may only pin a *published* plan version — that
# guard lives in the subscribing use case, not in any constraint, and the Go
# integration tests are where it is proven.
expect_constraint_failure "a second plan may not take a held name" plans_name_key "
WITH plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING name
)
INSERT INTO control.plans (id, name, created_at)
SELECT 'b2000000-0000-7000-8000-0000000000b2', name, now() FROM plan"

expect_constraint_failure "a plan id outside the v7 grammar is refused" plans_id_uuid_v7 "
INSERT INTO control.plans (id, name, created_at)
VALUES ('b2000000-0000-4000-8000-0000000000b1', 'verify probe', now())"

expect_constraint_failure "a published version without a publication stamp is refused" plan_versions_state_stamps "
WITH plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
)
INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', NULL, NULL, now(), now() FROM plan"

expect_constraint_failure "a second version may not reuse a version number" plan_versions_version_number_key "
WITH plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING plan_id, version_number
)
INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
SELECT 'b3000000-0000-7000-8000-0000000000b2', plan_id, version_number, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM version"

expect_constraint_failure "a negative recurring price is refused" plan_versions_price_non_negative "
WITH plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
)
INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', -1, 'draft', NULL, NULL, now(), now() FROM plan"

expect_constraint_failure "a plan version under an unknown plan is refused" plan_versions_plan_id_fkey "
INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
VALUES ('b3000000-0000-7000-8000-0000000000b1', 'b2000000-0000-7000-8000-0000000000b1', 1, 'calendar_month', 4900, 'draft', NULL, NULL, now(), now())"

expect_constraint_failure "a grant definition outside the group grammar is refused" plan_grant_definitions_group_name_grammar "
WITH plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'draft', NULL, NULL, now(), now() FROM plan
  RETURNING id
)
INSERT INTO control.plan_grant_definitions (id, plan_version_id, alias_group_name, dimension, granted_amount, created_at)
SELECT 'b4000000-0000-7000-8000-0000000000b1', id, 'not a group!', 'cost', 100, now() FROM version"

expect_constraint_failure "a zero grant is refused" plan_grant_definitions_amount_positive "
WITH plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'draft', NULL, NULL, now(), now() FROM plan
  RETURNING id
)
INSERT INTO control.plan_grant_definitions (id, plan_version_id, alias_group_name, dimension, granted_amount, created_at)
SELECT 'b4000000-0000-7000-8000-0000000000b1', id, 'anthropic', 'cost', 0, now() FROM version"

expect_constraint_failure "one grant per scope and dimension per version" plan_grant_definitions_scope_dimension_key "
WITH plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'draft', NULL, NULL, now(), now() FROM plan
  RETURNING id
), definition AS (
  INSERT INTO control.plan_grant_definitions (id, plan_version_id, alias_group_name, dimension, granted_amount, created_at)
  SELECT 'b4000000-0000-7000-8000-0000000000b1', id, 'anthropic', 'cost', 100, now() FROM version
  RETURNING plan_version_id, alias_group_name, dimension
)
INSERT INTO control.plan_grant_definitions (id, plan_version_id, alias_group_name, dimension, granted_amount, created_at)
SELECT 'b4000000-0000-7000-8000-0000000000b2', plan_version_id, alias_group_name, dimension, 200, now() FROM definition"

expect_constraint_failure "a grant definition under an unknown version is refused" plan_grant_definitions_plan_version_id_fkey "
INSERT INTO control.plan_grant_definitions (id, plan_version_id, alias_group_name, dimension, granted_amount, created_at)
VALUES ('b4000000-0000-7000-8000-0000000000b1', 'b3000000-0000-7000-8000-0000000000b1', 'anthropic', 'cost', 100, now())"

expect_constraint_failure "a subscription state outside the lifecycle is refused" subscriptions_state_valid "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
), plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
)
INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
SELECT 'b5000000-0000-7000-8000-0000000000b1', account.id, version.id, 'paused', now(), true, NULL, NULL, NULL, NULL, NULL, now(), now() FROM account, version"

expect_constraint_failure "a cancellation instant without a mode is refused" subscriptions_cancellation_consistency "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
), plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
)
INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
SELECT 'b5000000-0000-7000-8000-0000000000b1', account.id, version.id, 'pending', now(), true, now(), NULL, NULL, NULL, NULL, now(), now() FROM account, version"

expect_constraint_failure "a pending subscription carrying a cycle is refused" subscriptions_pending_has_no_cycle "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
), plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
)
INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
SELECT 'b5000000-0000-7000-8000-0000000000b1', account.id, version.id, 'pending', now(), true, NULL, NULL, 1, NULL, NULL, now(), now() FROM account, version"

expect_constraint_failure "an active subscription without its cycle is refused" subscriptions_rolled_have_cycle "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
), plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
)
INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
SELECT 'b5000000-0000-7000-8000-0000000000b1', account.id, version.id, 'active', now(), true, NULL, NULL, NULL, NULL, NULL, now(), now() FROM account, version"

expect_constraint_failure "a cycle ending at or before its start is refused" subscriptions_period_bounds "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
), plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
)
INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
SELECT 'b5000000-0000-7000-8000-0000000000b1', account.id, version.id, 'active', now(), true, NULL, NULL, 1, now(), now(), now(), now() FROM account, version"

expect_constraint_failure "a subscription under an unknown plan version is refused" subscriptions_plan_version_id_fkey "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
)
INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
SELECT 'b5000000-0000-7000-8000-0000000000b1', id, 'b3000000-0000-7000-8000-0000000000b1', 'pending', now(), true, NULL, NULL, NULL, NULL, NULL, now(), now() FROM account"

expect_constraint_failure "a subscription under an unknown account is refused" subscriptions_account_id_fkey "
WITH plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
)
INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
SELECT 'b5000000-0000-7000-8000-0000000000b1', 'a0000000-0000-4000-8000-0000000000a1', version.id, 'pending', now(), true, NULL, NULL, NULL, NULL, NULL, now(), now() FROM version"

expect_constraint_failure "a cancellation mode outside its enum is refused" subscriptions_cancellation_mode_valid "
WITH plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
), account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
)
INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
SELECT 'b5000000-0000-7000-8000-0000000000b1', account.id, version.id, 'pending', now(), true, now(), 'whenever', NULL, NULL, NULL, now(), now() FROM account, version"

expect_constraint_failure "an immediate cancellation beside a live subscription is refused" subscriptions_cancellation_consistency "
WITH plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
), account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
)
INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
SELECT 'b5000000-0000-7000-8000-0000000000b1', account.id, version.id, 'active', now(), true, now(), 'immediate', 1, now(), now() + interval '1 month', now(), now() FROM account, version"

expect_constraint_failure "an entitlement state outside its machine is refused" entitlements_state_valid "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
), plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
), definition AS (
  INSERT INTO control.plan_grant_definitions (id, plan_version_id, alias_group_name, dimension, granted_amount, created_at)
  SELECT 'b4000000-0000-7000-8000-0000000000b1', id, 'anthropic', 'cost', 100, now() FROM version
  RETURNING id
), subscription AS (
  INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
  SELECT 'b5000000-0000-7000-8000-0000000000b1', account.id, version.id, 'active', now(), true, NULL, NULL, 1, now(), now() + interval '1 month', now(), now() FROM account, version
  RETURNING id
)
INSERT INTO control.entitlements (id, subscription_id, cycle_number, grant_definition_id, alias_group_version_id, dimension, granted_amount, state, period_start, period_end, created_at, updated_at)
SELECT 'b6000000-0000-7000-8000-0000000000b1', subscription.id, 1, definition.id, 'b7000000-0000-7000-8000-0000000000b1', 'cost', 100, 'suspended', now(), now() + interval '1 month', now(), now() FROM subscription, definition"

expect_constraint_failure "an entitlement scope version outside the v7 grammar is refused" entitlements_alias_group_version_uuid_v7 "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
), plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
), definition AS (
  INSERT INTO control.plan_grant_definitions (id, plan_version_id, alias_group_name, dimension, granted_amount, created_at)
  SELECT 'b4000000-0000-7000-8000-0000000000b1', id, 'anthropic', 'cost', 100, now() FROM version
  RETURNING id
), subscription AS (
  INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
  SELECT 'b5000000-0000-7000-8000-0000000000b1', account.id, version.id, 'active', now(), true, NULL, NULL, 1, now(), now() + interval '1 month', now(), now() FROM account, version
  RETURNING id
)
INSERT INTO control.entitlements (id, subscription_id, cycle_number, grant_definition_id, alias_group_version_id, dimension, granted_amount, state, period_start, period_end, created_at, updated_at)
SELECT 'b6000000-0000-7000-8000-0000000000b1', subscription.id, 1, definition.id, 'b7000000-0000-4000-8000-0000000000b1', 'cost', 100, 'active', now(), now() + interval '1 month', now(), now() FROM subscription, definition"

expect_constraint_failure "an entitlement cycle ending at or before its start is refused" entitlements_period_bounds "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
), plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
), definition AS (
  INSERT INTO control.plan_grant_definitions (id, plan_version_id, alias_group_name, dimension, granted_amount, created_at)
  SELECT 'b4000000-0000-7000-8000-0000000000b1', id, 'anthropic', 'cost', 100, now() FROM version
  RETURNING id
), subscription AS (
  INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
  SELECT 'b5000000-0000-7000-8000-0000000000b1', account.id, version.id, 'active', now(), true, NULL, NULL, 1, now(), now() + interval '1 month', now(), now() FROM account, version
  RETURNING id
)
INSERT INTO control.entitlements (id, subscription_id, cycle_number, grant_definition_id, alias_group_version_id, dimension, granted_amount, state, period_start, period_end, created_at, updated_at)
SELECT 'b6000000-0000-7000-8000-0000000000b1', subscription.id, 1, definition.id, 'b7000000-0000-7000-8000-0000000000b1', 'cost', 100, 'active', now(), now(), now(), now() FROM subscription, definition"

expect_constraint_failure "a cycle's grant may not be materialised twice" entitlements_grant_once_per_cycle "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
), plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
), definition AS (
  INSERT INTO control.plan_grant_definitions (id, plan_version_id, alias_group_name, dimension, granted_amount, created_at)
  SELECT 'b4000000-0000-7000-8000-0000000000b1', id, 'anthropic', 'cost', 100, now() FROM version
  RETURNING id
), subscription AS (
  INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
  SELECT 'b5000000-0000-7000-8000-0000000000b1', account.id, version.id, 'active', now(), true, NULL, NULL, 1, now(), now() + interval '1 month', now(), now() FROM account, version
  RETURNING id
), first_grant AS (
  INSERT INTO control.entitlements (id, subscription_id, cycle_number, grant_definition_id, alias_group_version_id, dimension, granted_amount, state, period_start, period_end, created_at, updated_at)
  SELECT 'b6000000-0000-7000-8000-0000000000b1', subscription.id, 1, definition.id, 'b7000000-0000-7000-8000-0000000000b1', 'cost', 100, 'active', now(), now() + interval '1 month', now(), now() FROM subscription, definition
  RETURNING subscription_id, cycle_number, grant_definition_id
)
INSERT INTO control.entitlements (id, subscription_id, cycle_number, grant_definition_id, alias_group_version_id, dimension, granted_amount, state, period_start, period_end, created_at, updated_at)
SELECT 'b6000000-0000-7000-8000-0000000000b2', subscription_id, cycle_number, grant_definition_id, 'b7000000-0000-7000-8000-0000000000b1', 'cost', 100, 'active', now(), now() + interval '1 month', now(), now() FROM first_grant"

expect_constraint_failure "an entitlement under an unknown subscription is refused" entitlements_subscription_id_fkey "
WITH plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
), definition AS (
  INSERT INTO control.plan_grant_definitions (id, plan_version_id, alias_group_name, dimension, granted_amount, created_at)
  SELECT 'b4000000-0000-7000-8000-0000000000b1', id, 'anthropic', 'cost', 100, now() FROM version
  RETURNING id
)
INSERT INTO control.entitlements (id, subscription_id, cycle_number, grant_definition_id, alias_group_version_id, dimension, granted_amount, state, period_start, period_end, created_at, updated_at)
SELECT 'b6000000-0000-7000-8000-0000000000b1', 'b5000000-0000-7000-8000-0000000000b1', 1, definition.id, 'b7000000-0000-7000-8000-0000000000b1', 'cost', 100, 'active', now(), now() + interval '1 month', now(), now() FROM definition"

expect_constraint_failure "an entitlement under an unknown grant definition is refused" entitlements_grant_definition_id_fkey "
WITH plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
), account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
), subscription AS (
  INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
  SELECT 'b5000000-0000-7000-8000-0000000000b1', account.id, version.id, 'active', now(), true, NULL, NULL, 1, now(), now() + interval '1 month', now(), now() FROM account, version
  RETURNING id
)
INSERT INTO control.entitlements (id, subscription_id, cycle_number, grant_definition_id, alias_group_version_id, dimension, granted_amount, state, period_start, period_end, created_at, updated_at)
SELECT 'b6000000-0000-7000-8000-0000000000b1', subscription.id, 1, 'b4000000-0000-7000-8000-0000000000b1', 'b7000000-0000-7000-8000-0000000000b1', 'cost', 100, 'active', now(), now() + interval '1 month', now(), now() FROM subscription"

expect_constraint_failure "an entitlement with a zero grant is refused" entitlements_amount_positive "
WITH plan AS (
  INSERT INTO control.plans (id, name, created_at)
  VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now())
  RETURNING id
), version AS (
  INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
  SELECT 'b3000000-0000-7000-8000-0000000000b1', id, 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now() FROM plan
  RETURNING id
), definition AS (
  INSERT INTO control.plan_grant_definitions (id, plan_version_id, alias_group_name, dimension, granted_amount, created_at)
  SELECT 'b4000000-0000-7000-8000-0000000000b1', id, 'anthropic', 'cost', 100, now() FROM version
  RETURNING id
), account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
), subscription AS (
  INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
  SELECT 'b5000000-0000-7000-8000-0000000000b1', account.id, version.id, 'active', now(), true, NULL, NULL, 1, now(), now() + interval '1 month', now(), now() FROM account, version
  RETURNING id
)
INSERT INTO control.entitlements (id, subscription_id, cycle_number, grant_definition_id, alias_group_version_id, dimension, granted_amount, state, period_start, period_end, created_at, updated_at)
SELECT 'b6000000-0000-7000-8000-0000000000b1', subscription.id, 1, definition.id, 'b7000000-0000-7000-8000-0000000000b1', 'cost', 0, 'active', now(), now() + interval '1 month', now(), now() FROM subscription, definition"

expect_constraint_failure "a PAYG row under an unknown account is refused" account_payg_account_id_fkey "
INSERT INTO control.account_payg (account_id, enabled, funding_bucket_id, created_at, updated_at)
VALUES ('a0000000-0000-4000-8000-0000000000a1', true, NULL, now(), now())"

expect_constraint_failure "a PAYG bucket reference outside the v7 grammar is refused" account_payg_funding_bucket_uuid_v7 "
WITH account AS (
  INSERT INTO control.accounts (id, name, state, created_at, updated_at)
  VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now())
  RETURNING id
)
INSERT INTO control.account_payg (account_id, enabled, funding_bucket_id, created_at, updated_at)
SELECT id, true, 'b8000000-0000-4000-8000-0000000000b1', now(), now() FROM account"

# The projection foundation's half of the step, in the same voice: the change
# log, the materialized mirrors and the counter singleton, each rule a
# refusal that names its constraint. Revisions here sit far past the
# backfill's 1..N so no probe can collide with the seeded history.
expect_constraint_failure "an api_key log entry without a digest is refused" projection_changes_payload_digest_shape "
INSERT INTO control.projection_changes (revision, resource_kind, resource_id, recorded_at, payload)
VALUES (99001, 'api_key', 'a1000000-0000-4000-8000-0000000000a2', now(), '{\"state\": \"active\"}')"

expect_constraint_failure "an api_key log entry with a malformed digest is refused" projection_changes_payload_digest_shape "
INSERT INTO control.projection_changes (revision, resource_kind, resource_id, recorded_at, payload)
VALUES (99002, 'api_key', 'a1000000-0000-4000-8000-0000000000a2', now(), '{\"digest\": \"XYZ\", \"state\": \"active\"}')"

expect_constraint_failure "a log entry of an unknown resource kind is refused" projection_changes_resource_kind_valid "
INSERT INTO control.projection_changes (revision, resource_kind, resource_id, recorded_at, payload)
VALUES (99003, 'plan', 'b2000000-0000-4000-8000-0000000000b1', now(), '{}')"

expect_constraint_failure "a log entry at revision zero is refused" projection_changes_revision_positive "
INSERT INTO control.projection_changes (revision, resource_kind, resource_id, recorded_at, payload)
VALUES (0, 'account', 'a0000000-0000-4000-8000-0000000000a1', now(), '{}')"

expect_constraint_failure "a mirror credential with a malformed digest is refused" projection_api_keys_digest_shape "
INSERT INTO control.projection_api_keys (key_id, account_id, digest, state, revoked_at, source_revision, updated_at)
VALUES ('a1000000-0000-4000-8000-0000000000a2', 'a0000000-0000-4000-8000-0000000000a1', 'NOPE', 'active', NULL, 99001, now())"

expect_constraint_failure "an active mirror credential carrying a revocation stamp is refused" projection_api_keys_revocation_consistency "
INSERT INTO control.projection_api_keys (key_id, account_id, digest, state, revoked_at, source_revision, updated_at)
VALUES ('a1000000-0000-4000-8000-0000000000a2', 'a0000000-0000-4000-8000-0000000000a1', '$(printf 'a%.0s' $(seq 1 64))', 'active', now(), 99001, now())"

expect_constraint_failure "a mirror credential outside its lifecycle is refused" projection_api_keys_state_valid "
INSERT INTO control.projection_api_keys (key_id, account_id, digest, state, revoked_at, source_revision, updated_at)
VALUES ('a1000000-0000-4000-8000-0000000000a2', 'a0000000-0000-4000-8000-0000000000a1', '$(printf 'a%.0s' $(seq 1 64))', 'suspended', NULL, 99001, now())"

expect_constraint_failure "a second revision-counter row is refused" projection_revision_id_singleton "
INSERT INTO control.projection_revision (id, epoch, last_revision)
VALUES (2, gen_random_uuid(), 0)"

# The backfill's positive proof: the counter's singleton exists, carries an
# epoch, and stands at exactly the number of backfilled accounts.
assert_equals "the projection counter is seeded to the backfilled account count" \
	"$(psql_scalar "$control_db" "
SELECT (SELECT last_revision FROM control.projection_revision WHERE id = 1) =
       (SELECT count(*) FROM control.accounts)
  AND (SELECT epoch IS NOT NULL FROM control.projection_revision WHERE id = 1)")" "t"

# The Data Plane's mirror half, in the same voice: the runtime-side tables
# the projection writes through the listener enforce the same grammar in
# their own database.
expect_dataplane_constraint_failure "a mirror credential digest outside the hex grammar is refused" api_key_credentials_digest_grammar "
INSERT INTO public.api_key_credentials (key_id, account_id, digest, state, revoked_at, source_revision, updated_at)
VALUES ('a1000000-0000-4000-8000-0000000000a2', 'a0000000-0000-4000-8000-0000000000a1', 'NOPE', 'active', NULL, 5, now())"

expect_dataplane_constraint_failure "an active runtime credential with a revocation stamp is refused" api_key_credentials_revocation_consistency "
INSERT INTO public.api_key_credentials (key_id, account_id, digest, state, revoked_at, source_revision, updated_at)
VALUES ('a1000000-0000-4000-8000-0000000000a2', 'a0000000-0000-4000-8000-0000000000a1', '$(printf 'a%.0s' $(seq 1 64))', 'active', now(), 5, now())"

expect_dataplane_constraint_failure "a runtime credential outside its lifecycle is refused" api_key_credentials_state_valid "
INSERT INTO public.api_key_credentials (key_id, account_id, digest, state, revoked_at, source_revision, updated_at)
VALUES ('a1000000-0000-4000-8000-0000000000a2', 'a0000000-0000-4000-8000-0000000000a1', '$(printf 'a%.0s' $(seq 1 64))', 'closed', NULL, 5, now())"

expect_dataplane_constraint_failure "a runtime account outside its lifecycle is refused" account_states_state_valid "
INSERT INTO public.account_states (account_id, state, source_revision, updated_at)
VALUES ('a0000000-0000-4000-8000-0000000000a1', 'revoked', 5, now())"

expect_dataplane_constraint_failure "a second runtime position row is refused" projection_state_id_singleton "
INSERT INTO public.projection_state (id, bootstrapped, applied_revision, producer_epoch)
VALUES (2, false, 0, NULL)"

# The positive proof of the whole chain, in one rolled-back transaction:
# account -> plan -> published version -> grant definition -> active
# subscription with its first cycle -> its entitlement -> its PAYG flag.
assert_equals "a well-formed commerce chain all inserts" \
	"$(psql_scalar "$control_db" "
BEGIN;
INSERT INTO control.accounts (id, name, state, created_at, updated_at)
VALUES ('a0000000-0000-4000-8000-0000000000a1', 'verify probe', 'active', now(), now());
INSERT INTO control.plans (id, name, created_at)
VALUES ('b2000000-0000-7000-8000-0000000000b1', 'verify probe', now());
INSERT INTO control.plan_versions (id, plan_id, version_number, period, recurring_price_minor_units, state, published_at, retired_at, created_at, updated_at)
VALUES ('b3000000-0000-7000-8000-0000000000b1', 'b2000000-0000-7000-8000-0000000000b1', 1, 'calendar_month', 4900, 'published', now(), NULL, now(), now());
INSERT INTO control.plan_grant_definitions (id, plan_version_id, alias_group_name, dimension, granted_amount, created_at)
VALUES ('b4000000-0000-7000-8000-0000000000b1', 'b3000000-0000-7000-8000-0000000000b1', 'anthropic', 'cost', 100, now());
INSERT INTO control.subscriptions (id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at)
VALUES ('b5000000-0000-7000-8000-0000000000b1', 'a0000000-0000-4000-8000-0000000000a1', 'b3000000-0000-7000-8000-0000000000b1', 'active', now(), true, NULL, NULL, 1, now(), now() + interval '1 month', now(), now());
INSERT INTO control.entitlements (id, subscription_id, cycle_number, grant_definition_id, alias_group_version_id, dimension, granted_amount, state, period_start, period_end, created_at, updated_at)
VALUES ('b6000000-0000-7000-8000-0000000000b1', 'b5000000-0000-7000-8000-0000000000b1', 1, 'b4000000-0000-7000-8000-0000000000b1', 'b7000000-0000-7000-8000-0000000000b1', 'cost', 100, 'active', now(), now() + interval '1 month', now(), now());
INSERT INTO control.account_payg (account_id, enabled, funding_bucket_id, created_at, updated_at)
VALUES ('a0000000-0000-4000-8000-0000000000a1', true, NULL, now(), now());
SELECT (SELECT count(*) FROM control.plans WHERE id = 'b2000000-0000-7000-8000-0000000000b1') || '|' ||
       (SELECT count(*) FROM control.plan_versions WHERE id = 'b3000000-0000-7000-8000-0000000000b1') || '|' ||
       (SELECT count(*) FROM control.plan_grant_definitions WHERE id = 'b4000000-0000-7000-8000-0000000000b1') || '|' ||
       (SELECT count(*) FROM control.subscriptions WHERE id = 'b5000000-0000-7000-8000-0000000000b1') || '|' ||
       (SELECT count(*) FROM control.entitlements WHERE id = 'b6000000-0000-7000-8000-0000000000b1') || '|' ||
       (SELECT count(*) FROM control.account_payg WHERE account_id = 'a0000000-0000-4000-8000-0000000000a1');
ROLLBACK;")" "1|1|1|1|1|1"

step "9/12 PostgreSQL transaction semantics hold"
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

step "10/12 a migration that fails fails whole, loudly, and stops the world"
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

step "11/12 a full down roll returns both schemas to clean"
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
assert_equals "the projection mirror is gone after a full roll-back" \
	"$(psql_scalar "$dataplane_db" "SELECT to_regclass('public.api_key_credentials') IS NULL AND to_regclass('public.account_states') IS NULL AND to_regclass('public.projection_state') IS NULL")" "t"
migrate_lane "$control_db" down -all </dev/null
assert_equals "the Control Plane's recorded version is zero after a full roll-back" \
	"$(recorded_version "$control_db")" "0"
assert_equals "the identity tables are gone after a full roll-back" \
	"$(psql_scalar "$control_db" "SELECT to_regclass('control.accounts') IS NULL AND to_regclass('control.users') IS NULL AND to_regclass('control.api_keys') IS NULL")" "t"
assert_equals "the commerce tables are gone after a full roll-back" \
	"$(psql_scalar "$control_db" "SELECT to_regclass('control.plans') IS NULL AND to_regclass('control.subscriptions') IS NULL AND to_regclass('control.entitlements') IS NULL AND to_regclass('control.account_payg') IS NULL")" "t"
assert_equals "the projection tables are gone after a full roll-back" \
	"$(psql_scalar "$control_db" "SELECT to_regclass('control.projection_revision') IS NULL AND to_regclass('control.projection_changes') IS NULL AND to_regclass('control.projection_api_keys') IS NULL AND to_regclass('control.projection_accounts') IS NULL")" "t"
assert_equals "the ownership namespace and its comment are gone after a full roll-back" \
	"$(psql_scalar "$control_db" "SELECT count(*) FROM pg_namespace WHERE nspname = '$control_db'")" "0"
assert_equals "the Control Plane's public schema is back to its migration history alone" \
	"$(psql_scalar "$control_db" "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'")" "1"

step "12/12 the suite leaves both databases migrated, not half-torn-down"
migrate_lane "$dataplane_db" up
migrate_lane "$control_db" up
assert_equals "final recorded version, Data Plane" "$(recorded_version "$dataplane_db")" "$newest"
assert_equals "final recorded version, Control Plane" "$(recorded_version "$control_db")" "$control_newest"

printf '\npersistence suite: green (both plane databases up and migrated on %s)\n' \
	"$(psql_scalar "$dataplane_db" 'SELECT version()')"
