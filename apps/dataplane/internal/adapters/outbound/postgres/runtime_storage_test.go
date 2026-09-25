//go:build integration

package postgres

// The runtime-storage integration suite: the B7 repositories and the fact
// reader against a real PostgreSQL carrying the 000002_runtime_storage schema.
// The unit suites beside this file pin the adapters' orchestration against the
// hand-written driver fake; what these tests prove is the other half — that
// the conditional drawdown, the dedup partial uniques, the stream's
// commit-order allocation, the publication algebra and the terminal-row
// triggers behave the way the migration's header comment promises, which no
// fake can say.
//
// Run (from apps/dataplane):
//
//	docker compose -f deploy/postgres/compose.yaml up -d --wait
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:5432/postgres?sslmode=disable' \
//	  go test -v -tags=integration ./internal/adapters/outbound/postgres
//
// Two pieces of infrastructure are specific to this suite:
//
//   - integrationRuntimeSchema applies the migration FILES under
//     migrations/dataplane itself — golang-migrate's own table and versioning
//     discipline, serialised across `go test` processes by a session advisory
//     lock — so the suite runs against a fresh CI database as readily as
//     against the already-migrated local fixture. One test proves the
//     from-scratch path on a throwaway database instead of assuming it.
//
//   - every test mints a unique account id (integrationRuntimeAccount) and
//     every multi-write unit runs inside store.WithinTx. No test truncates a
//     shared table and no test reads another's rows: the fixture database is
//     retained forever by design and carries rows from every earlier run, so
//     the suite must be safe to run twice against one server. The one
//     sanctioned exception is the cursor test's simulated lost stream, which
//     deletes the feed and its stream row; the migration header documents why
//     no trigger guards those (append-only is a writer discipline, not a DB
//     guard), and that test is where the discipline's failure mode is proved.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/usagefacts"
)

// Two advisory-lock keys, one per serialisation this suite needs. Both are
// arbitrary constants; what matters is that they are stable across processes,
// because a lock whose key varied would serialise nothing.
const (
	// runtimeSchemaLockKey guards schema application: two `go test` processes
	// started against one database must not interleave their applies of the
	// same migration file.
	runtimeSchemaLockKey = 700002

	// throwawayDatabaseLockKey guards the throwaway-database tests. Their
	// databases have fixed names and their teardown is DROP DATABASE
	// WITH (FORCE), which kills the other holder's connections — so two
	// processes must never run such tests at once, even mid-development when
	// one run targets a single test by name.
	throwawayDatabaseLockKey = 700003
)

// integrationRepositoryRoot walks up from the process working directory until
// it finds the repository root, recognised by the one directory this suite
// reads at run time: migrations/dataplane. The suite applies the migration
// files themselves (integrationRuntimeSchema) rather than carrying a copy of
// the schema, so a migration edited for review is the migration the suite
// proves — and a checkout is the only place those files exist.
func integrationRepositoryRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("finding the working directory: %v", err)
	}
	for {
		if info, statErr := os.Stat(filepath.Join(dir, "migrations", "dataplane")); statErr == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no ancestor of %s contains migrations/dataplane — the runtime-storage suite applies the migration files themselves and cannot run outside a repository checkout", dir)
		}
		dir = parent
	}
}

// integrationRuntimeSchema brings the pool's database to the lane's newest
// version, by hand, the way golang-migrate would: one row in
// public.schema_migrations naming the applied version and its dirty flag, one
// transaction per applied file.
//
// Doing this in the suite rather than requiring a pre-migrated database is
// what makes one target serve both runners: the local fixture (and CI's
// service container) is already at the newest version, so the helper applies
// nothing and costs one lock and one read — while a fresh CI database is
// brought up whole, migration 000001's timescaledb bootstrap included, with
// no out-of-band step a forgotten README line could skip.
//
// The versioning discipline is golang-migrate's, mirrored: its table is
// created if missing (a database migrated by no one yet has none), its single
// row is read as version 0 / not dirty when absent, more than one row is a
// corrupted history this suite refuses to guess at, and a dirty flag stops
// the run — a dirty flag means a migration half-applied when its process
// died, and silently migrating over one is how a schema ends up nowhere in
// particular. Recovery is a human decision (golang-migrate's `force`, or a
// drop and re-apply); the suite only refuses to be the thing that skips it.
func integrationRuntimeSchema(t testing.TB, db *sql.DB) {
	t.Helper()
	lane := filepath.Join(integrationRepositoryRoot(t), "migrations", "dataplane")
	entries, err := os.ReadDir(lane)
	if err != nil {
		t.Fatalf("reading the migration lane %s: %v", lane, err)
	}
	var files []string
	for _, entry := range entries {
		// .up.sql only: the .down.sql half of each pair is the rollback's
		// business, and the suite has no rollback to run — it never un-applies
		// a shared database's schema.
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".up.sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("taking a connection for the schema lock: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// The lock is session-level and the session is this connection, so it is
	// held until the cleanup unlocks and the pool reclaims the connection:
	// long enough to serialise concurrent processes through a whole apply,
	// never across tests. pg_advisory_lock waits rather than fails — the
	// second process's suite simply starts a moment later.
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", runtimeSchemaLockKey); err != nil {
		t.Fatalf("taking the schema advisory lock: %v", err)
	}
	t.Cleanup(func() {
		// The cleanup's context cannot ride t.Context(): it is cancelled
		// before cleanups run. The unlock must reach the server for the next
		// process's lock not to wait out its own timeout.
		unlockCtx, cancelUnlock := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelUnlock()
		_, _ = conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", runtimeSchemaLockKey)
	})

	if _, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS public.schema_migrations (version bigint NOT NULL, dirty boolean NOT NULL)"); err != nil {
		t.Fatalf("ensuring public.schema_migrations: %v", err)
	}
	rows, err := conn.QueryContext(ctx, "SELECT version, dirty FROM public.schema_migrations")
	if err != nil {
		t.Fatalf("reading public.schema_migrations: %v", err)
	}
	var (
		current  int64
		dirty    bool
		recorded int
	)
	for rows.Next() {
		recorded++
		if err := rows.Scan(&current, &dirty); err != nil {
			rows.Close()
			t.Fatalf("scanning public.schema_migrations: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("reading public.schema_migrations: %v", err)
	}
	rows.Close()
	if recorded > 1 {
		t.Fatalf("public.schema_migrations carries %d rows — golang-migrate keeps exactly one, and a history with more is corrupted past what this suite may guess at", recorded)
	}
	if recorded == 0 {
		// A database no runner has touched: version 0, clean.
		current, dirty = 0, false
	}
	if dirty {
		t.Fatalf("public.schema_migrations is dirty at version %d — recover it before running this suite, never over the flag: golang-migrate's force (migrate -path migrations/dataplane -database $DSN force <clean-version>) or a drop and re-apply", current)
	}

	for _, name := range files {
		if len(name) < 7 {
			t.Fatalf("migration file %q does not carry the leading six-digit version golang-migrate's naming requires", name)
		}
		version, err := strconv.Atoi(name[:6])
		if err != nil {
			t.Fatalf("migration file %q does not carry the leading six-digit version golang-migrate's naming requires", name)
		}
		if int64(version) <= current {
			continue
		}
		content, err := os.ReadFile(filepath.Join(lane, name))
		if err != nil {
			t.Fatalf("reading migration %s: %v", name, err)
		}
		// One transaction per file, the runner's own atomicity: a migration
		// that fails mid-file leaves the version it recorded behind and the
		// dirty flag it deserves, which the check above will refuse on the
		// next run. (The file is executed whole: the driver uses the simple
		// query protocol for a zero-argument Exec, which is the one protocol
		// PostgreSQL parses as multiple statements.)
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("beginning the transaction for %s: %v", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(content)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("applying %s: %v", name, err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM public.schema_migrations"); err != nil {
			_ = tx.Rollback()
			t.Fatalf("recording %s: %v", name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO public.schema_migrations (version, dirty) VALUES ($1, false)", version); err != nil {
			_ = tx.Rollback()
			t.Fatalf("recording %s: %v", name, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("committing %s: %v", name, err)
		}
		current = int64(version)
	}
}

// integrationAdminDSNTB and integrationPlaneDSNTB are the testing.TB shapes of
// integrationAdminDSN and integrationPlaneDSN in integration_test.go, which
// this suite may not edit; the benchmarks need them because *testing.B does
// not satisfy *testing.T. The derivation and the refusal to run without an
// explicit server are identical — a duplicated derivation is the price of not
// touching a file another lane owns, and the pool builder below asserts the
// two stay honest by using the same Options.
func integrationAdminDSNTB(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_ADMIN_DSN")
	if dsn == "" {
		t.Fatal("POSTGRES_TEST_ADMIN_DSN is required for integration tests; start the fixture (from the repository root: docker compose -f deploy/postgres/compose.yaml up -d --wait) and set it to postgres://gateway:gateway-dev-only@127.0.0.1:5432/postgres?sslmode=disable (see deploy/postgres/README.md)")
	}
	return dsn
}

// integrationPlaneDSNTB returns the DSN of the database this application owns,
// derived from the admin DSN, creating the database when the cluster lacks it.
// The ensuring connection is opened on the driver directly: Open refuses any
// DSN not naming `dataplane`, and the admin DSN names the cluster's
// maintenance database by design — the refusal is the adapter doing its job.
func integrationPlaneDSNTB(t testing.TB) string {
	t.Helper()
	admin, err := url.Parse(integrationAdminDSNTB(t))
	if err != nil {
		t.Fatalf("POSTGRES_TEST_ADMIN_DSN is not a parsable URL: %v", err)
	}
	adminDB, err := sql.Open(driverName, admin.String())
	if err != nil {
		t.Fatalf("sql.Open on the admin DSN: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := adminDB.PingContext(ctx); err != nil {
		t.Fatalf("the admin DSN could not reach the server: %v — start the fixture (from the repository root: docker compose -f deploy/postgres/compose.yaml up -d --wait)", err)
	}
	var exists bool
	if err := adminDB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", ownedDatabase).Scan(&exists); err != nil {
		t.Fatalf("looking up the %s database: %v", ownedDatabase, err)
	}
	if !exists {
		if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+ownedDatabase); err != nil {
			t.Fatalf("CREATE DATABASE %s: %v", ownedDatabase, err)
		}
	}
	plane := *admin
	plane.Path = "/" + ownedDatabase
	return plane.String()
}

// integrationPoolTB is integrationPool's testing.TB shape: the same Options —
// notably the same MaxOpenConns, because the contended benchmarks must measure
// the pool the contended tests measure — the same production Open, the same
// cleanup discipline.
func integrationPoolTB(t testing.TB) (*sql.DB, persistence.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := Open(ctx, Options{
		DSN:             integrationPlaneDSNTB(t),
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Open() on the %s database error = %v", ownedDatabase, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, New(db)
}

// integrationThrowawaySerialise takes the cluster-side advisory lock under
// which every throwaway-database test runs. The lock is taken on a dedicated
// admin connection and held to the test's end: two processes running such
// tests concurrently would otherwise have the second's DROP DATABASE
// WITH (FORCE) kill the first's connections mid-test and turn a clean
// isolation mechanism into a heisenbug. It is a different key from the schema
// lock — the two guards serialise different things — and a different lock
// space from anything inside the throwaway database, because advisory locks
// are per-database and the throwaway does not exist yet when this one is
// taken.
func integrationThrowawaySerialise(t testing.TB) {
	t.Helper()
	adminDB, err := sql.Open(driverName, integrationAdminDSNTB(t))
	if err != nil {
		t.Fatalf("sql.Open on the admin DSN: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := adminDB.Conn(ctx)
	if err != nil {
		_ = adminDB.Close()
		t.Fatalf("taking an admin connection for the throwaway lock: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", throwawayDatabaseLockKey); err != nil {
		_ = conn.Close()
		_ = adminDB.Close()
		t.Fatalf("taking the throwaway-database advisory lock: %v", err)
	}
	t.Cleanup(func() {
		unlockCtx, cancelUnlock := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelUnlock()
		_, _ = conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", throwawayDatabaseLockKey)
		_ = conn.Close()
		_ = adminDB.Close()
	})
}

// integrationThrowawayDatabase creates a database on the fixture cluster that
// exists for exactly one test, and hands back a pool on it. The pool is opened
// on the driver directly, not through Open: the plane-binding guard is the
// production adapter's door for production DSNs, and these test databases are
// named dataplane_* precisely so nobody can mistake one for the plane's.
//
// The name is a test-owned literal, never user input, and it is fixed per test
// rather than minted — a rerun must find and drop the leftover of a run that
// was killed before its cleanup, which WITH (FORCE) does by terminating any
// connection still attached. Between processes the throwaway lock, not the
// name, is what keeps two tests from dropping each other's database.
func integrationThrowawayDatabase(t testing.TB, name string) *sql.DB {
	t.Helper()
	admin, err := url.Parse(integrationAdminDSNTB(t))
	if err != nil {
		t.Fatalf("POSTGRES_TEST_ADMIN_DSN is not a parsable URL: %v", err)
	}
	adminDB, err := sql.Open(driverName, admin.String())
	if err != nil {
		t.Fatalf("sql.Open on the admin DSN: %v", err)
	}
	// Registered first so it runs last: the drop below needs this handle alive
	// after the pool is gone.
	t.Cleanup(func() { _ = adminDB.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := adminDB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
		t.Fatalf("dropping a leftover of %s: %v", name, err)
	}
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("CREATE DATABASE %s: %v", name, err)
	}

	plane := *admin
	plane.Path = "/" + name
	db, err := sql.Open(driverName, plane.String())
	if err != nil {
		t.Fatalf("sql.Open on the throwaway database %s: %v", name, err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Minute)
	db.SetConnMaxIdleTime(30 * time.Second)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping the throwaway database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		dropCtx, cancelDrop := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancelDrop()
		if _, err := adminDB.ExecContext(dropCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("dropping throwaway database %s: %v (a leftover is dropped by the next run's own setup)", name, err)
		}
	})
	return db
}

// integrationRuntimeAccount mints the unique account id one test carries, and
// uniqueness is the suite's whole isolation story: every row a test writes
// names its account or a funding bucket minted from it, so two tests — or two
// runs, or two concurrent `go test` processes — never touch one another's
// rows, and no shared table is ever truncated. The test-name fragment keeps a
// failed assertion legible in the database itself; the random suffix keeps a
// rerun's funding buckets off the UNIQUE constraint the previous run's rows
// still hold. The suffix is the tail of a version-7 uuid because the tail is
// its random half — the head is a timestamp and repeats within a millisecond.
func integrationRuntimeAccount(t testing.TB, fragment string) string {
	t.Helper()
	var cleaned strings.Builder
	for _, r := range strings.ToLower(fragment) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cleaned.WriteRune(r)
			continue
		}
		cleaned.WriteByte('-')
	}
	name := strings.Trim(cleaned.String(), "-")
	if len(name) > 32 {
		name = name[:32]
	}
	suffix := string(identity.NewRequestID())
	return "b7it-" + name + "-" + suffix[len(suffix)-8:]
}

// integrationRepositories is the five runtime repositories over one store.
// Every repository resolves its query surface from the context through that
// store, so repositories built over one store share one unit of work exactly
// when a context carries one — which is the composition the runtime wires,
// and the property the atomicity tests below depend on.
type integrationRepositories struct {
	store    persistence.Store
	requests persistence.RequestRepository
	attempts persistence.AttemptRepository
	intakes  persistence.IntakeRepository
	reserves persistence.ReservationRepository
	quota    persistence.QuotaProjectionRepository
	facts    persistence.FactRepository
}

func integrationRepos(t testing.TB, store persistence.Store) integrationRepositories {
	t.Helper()
	return integrationRepositories{
		store:    store,
		requests: NewRequestRepository(store),
		attempts: NewAttemptRepository(store),
		intakes:  NewIntakeRepository(store),
		reserves: NewReservationRepository(store),
		quota:    NewQuotaProjectionRepository(store),
		facts:    NewFactRepository(store),
	}
}

// integrationSubscriptionCreatedAt is the one subscription timestamp every
// seeded projection carries. It is deliberately shared: the waterfall's
// subscription tiebreak must never be what separates a suite's buckets, so
// any ordering the tests assert is being decided by the inputs the test
// chose — named scope and period end — and not by an accident of seeding.
var integrationSubscriptionCreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// integrationPublish delivers one grant through the repository — the same
// statement the Control Plane's publications arrive through. A zero period
// end publishes the PAYG scope, a non-zero one an entitlement cycle; the
// revision is the caller's because the publication algebra is ordered by it
// and the algebra test redelivers and supersedes on purpose.
func integrationPublish(t testing.TB, ctx context.Context, repos integrationRepositories, account, bucket string, namedScope bool, periodEnd time.Time, limit, revision int64, state accounting.ProjectionState) accounting.PublicationOutcome {
	t.Helper()
	publication := accounting.Publication{
		AccountID:             account,
		FundingBucketID:       bucket,
		AliasGroupVersionID:   "b7it-alias-group-1",
		ScopeKind:             accounting.ScopePayGBalance,
		NamedScope:            namedScope,
		PeriodEnd:             periodEnd,
		SubscriptionCreatedAt: integrationSubscriptionCreatedAt,
		State:                 state,
		LimitAmount:           limit,
		Revision:              revision,
	}
	if !periodEnd.IsZero() {
		publication.ScopeKind = accounting.ScopeEntitlementCycle
		publication.EntitlementID = "b7it-entitlement-" + bucket
		publication.CycleNumber = 1
	}
	outcome, err := repos.quota.ApplyPublication(ctx, publication)
	if err != nil {
		t.Fatalf("ApplyPublication(%s, revision %d): %v", bucket, revision, err)
	}
	return outcome
}

// integrationSeedProjection mints the account's one funding bucket and
// publishes its grant, asserting the seed — every call site seeds a bucket it
// has just minted, so any outcome but "seeded" is a bug in the suite, and one
// loud failure beats every later assertion confounding a stale row with a
// fresh one.
func integrationSeedProjection(t testing.TB, ctx context.Context, repos integrationRepositories, account string, namedScope bool, periodEnd time.Time, limit int64) string {
	t.Helper()
	bucket := account + "-bucket"
	if outcome := integrationPublish(t, ctx, repos, account, bucket, namedScope, periodEnd, limit, 1, accounting.ProjectionActive); outcome != accounting.PublicationSeeded {
		t.Fatalf("ApplyPublication() on a fresh bucket = %q, want %q", outcome, accounting.PublicationSeeded)
	}
	return bucket
}

// integrationPrice is the one price snapshot every suite request is priced
// under. Nothing here needs it to vary: the settlement re-derives its amount
// from the row's own snapshot, and one constant keeps every fact's arithmetic
// checkable by eye — 120 input tokens at 2 and 45 output tokens at 3 is 375
// minor units, the settled amount every helper writes.
func integrationPrice() execution.PriceSnapshot {
	return execution.PriceSnapshot{RevisionID: "b7it-price-revision-1", InputUnitPrice: 2, OutputUnitPrice: 3}
}

// int64Ptr is the suite's spelling of "a known count": a pointer, because the
// domain distinguishes a nil count (nobody reported any) from a zero one (the
// provider said none were used).
func int64Ptr(v int64) *int64 { return &v }

// integrationFactLegs renders a reservation's waterfall split as the fact
// payload's allocation tail. The two shapes are deliberately distinct types
// in the domain — the reservation remembers its own drawdown, the fact
// publishes it — and this is the one place the suite converts between them.
func integrationFactLegs(allocations []accounting.Allocation) []accounting.AllocationLeg {
	legs := make([]accounting.AllocationLeg, 0, len(allocations))
	for _, allocation := range allocations {
		legs = append(legs, accounting.AllocationLeg{
			FundingBucketID: allocation.FundingBucketID,
			Amount:          allocation.Amount,
			Ordinal:         allocation.Ordinal,
		})
	}
	return legs
}

// integrationSettlement runs the settlement unit of work the ports' doctrine
// describes: the request insert, the attempt insert, the drawdown, the
// reservation open, the reservation close(settled) and the fact append — one
// WithinTx, the append its last statement. The unit is deliberately wider
// than any one production path (admission and settlement are separate
// transactions in the runtime) so one helper can stand for the whole chain a
// feed page is derived from.
//
// It returns the request, the fact it appended, and the sequence the store
// allocated, so the caller can assert on both sides of the unit: the rows and
// the feed.
func integrationSettlement(t testing.TB, ctx context.Context, repos integrationRepositories, account string) (execution.Request, accounting.Fact, int64) {
	t.Helper()
	price := integrationPrice()
	now := time.Now().UTC()
	request, err := execution.NewRequest(identity.NewRequestID(), account, account+"-api-key", "bench/alias", 120, 4096, price, now)
	if err != nil {
		t.Fatalf("building a request: %v", err)
	}
	attempt, err := execution.NewAttempt(identity.NewAttemptID(), request.ID, 0, 0, "b7it-backend", "b7it/model", execution.OutcomeSucceeded, "", now, now)
	if err != nil {
		t.Fatalf("building an attempt: %v", err)
	}
	integrationSeedProjection(t, ctx, repos, account, true, now.Add(24*time.Hour), 1000)

	var (
		fact accounting.Fact
		seq  int64
	)
	err = repos.store.WithinTx(ctx, func(ctx context.Context) error {
		if err := repos.requests.Insert(ctx, request); err != nil {
			return err
		}
		if err := repos.attempts.Insert(ctx, attempt); err != nil {
			return err
		}
		drawn, err := repos.quota.Drawdown(ctx, account, 250)
		if err != nil {
			return err
		}
		reservation, err := accounting.NewReservation(
			identity.NewReservationID(), request.ID, price.RevisionID,
			price.InputUnitPrice, price.OutputUnitPrice,
			request.InputTokens, request.MaxOutputTokens,
			250, drawn, now, now.Add(time.Hour), "b7it-runtime", now.Add(30*time.Minute),
		)
		if err != nil {
			return err
		}
		if err := repos.reserves.Insert(ctx, reservation); err != nil {
			return err
		}
		closed, err := repos.reserves.Close(ctx, reservation.ID, accounting.StateSettled, now)
		if err != nil {
			return err
		}
		if !closed {
			return errors.New("reservation.Close() = false inside the unit that opened it")
		}
		fact, err = accounting.NewSettled(request.ID, attempt.ID, accounting.CaptureReported,
			int64Ptr(120), int64Ptr(45), int64Ptr(45),
			price.RevisionID, price.InputUnitPrice, price.OutputUnitPrice, 375,
			integrationFactLegs(reservation.Allocations), now)
		if err != nil {
			return err
		}
		seq, err = repos.facts.Append(ctx, fact)
		return err
	})
	if err != nil {
		t.Fatalf("settlement unit of work: %v", err)
	}
	return request, fact, seq
}

// integrationFormSettled builds — writes nothing — one settled fact for a
// fresh request and attempt. It is split from the write below because the
// concurrency tests need the forming done on the test goroutine: t.Fatal
// belongs to the goroutine running the test, and a helper that forms and
// writes in one would be failing from inside a spawned one. The legs
// parameter is the payload's whole content, so a test that needs
// distinguishable facts mints distinguishable legs and a test that needs
// identical ones passes the same value twice.
func integrationFormSettled(t testing.TB, account string, legs []accounting.AllocationLeg, occurredAt time.Time) (execution.Request, execution.Attempt, accounting.Fact) {
	t.Helper()
	price := integrationPrice()
	request, err := execution.NewRequest(identity.NewRequestID(), account, account+"-api-key", "bench/alias", 120, 4096, price, occurredAt)
	if err != nil {
		t.Fatalf("building a request: %v", err)
	}
	attempt, err := execution.NewAttempt(identity.NewAttemptID(), request.ID, 0, 0, "b7it-backend", "b7it/model", execution.OutcomeSucceeded, "", occurredAt, occurredAt)
	if err != nil {
		t.Fatalf("building an attempt: %v", err)
	}
	fact, err := accounting.NewSettled(request.ID, attempt.ID, accounting.CaptureReported,
		int64Ptr(120), int64Ptr(45), int64Ptr(45),
		price.RevisionID, price.InputUnitPrice, price.OutputUnitPrice, 375,
		legs, occurredAt)
	if err != nil {
		t.Fatalf("building a settled fact: %v", err)
	}
	return request, attempt, fact
}

// integrationWriteSettled writes a formed request, attempt and fact inside one
// unit of work — the smallest honest unit that exercises the stream's sequence
// allocation — and returns the sequence the store allocated.
func integrationWriteSettled(ctx context.Context, repos integrationRepositories, request execution.Request, attempt execution.Attempt, fact accounting.Fact) (int64, error) {
	seq := int64(0)
	err := repos.store.WithinTx(ctx, func(ctx context.Context) error {
		if err := repos.requests.Insert(ctx, request); err != nil {
			return err
		}
		if err := repos.attempts.Insert(ctx, attempt); err != nil {
			return err
		}
		allocated, err := repos.facts.Append(ctx, fact)
		seq = allocated
		return err
	})
	return seq, err
}

// integrationAppendSettled forms and writes one settled fact in its own unit
// of work, and returns the request, the fact and the sequence the store
// allocated. The serial callers' shape; the concurrent ones split it.
func integrationAppendSettled(t testing.TB, ctx context.Context, repos integrationRepositories, account string, legs []accounting.AllocationLeg, occurredAt time.Time) (identity.RequestID, accounting.Fact, int64) {
	t.Helper()
	request, attempt, fact := integrationFormSettled(t, account, legs, occurredAt)
	seq, err := integrationWriteSettled(ctx, repos, request, attempt, fact)
	if err != nil {
		t.Fatalf("appending one settled fact: %v", err)
	}
	return request.ID, fact, seq
}

// integrationReservation opens one zero-amount hold for a fresh request: the
// reaper's subject is the lease vocabulary, not the allocation tail, so the
// hold carries the pricing basis and token bounds the sweep reads back and no
// legs at all — the one shape NewReservation accepts without any.
func integrationReservation(t testing.TB, ctx context.Context, repos integrationRepositories, account string, createdAt, expiresAt, leaseExpiresAt time.Time) (identity.ReservationID, identity.RequestID) {
	t.Helper()
	price := integrationPrice()
	request, err := execution.NewRequest(identity.NewRequestID(), account, account+"-api-key", "bench/alias", 10, 20, price, createdAt)
	if err != nil {
		t.Fatalf("building a request: %v", err)
	}
	if err := repos.requests.Insert(ctx, request); err != nil {
		t.Fatalf("inserting the reaper's request: %v", err)
	}
	reservation, err := accounting.NewReservation(identity.NewReservationID(), request.ID,
		price.RevisionID, price.InputUnitPrice, price.OutputUnitPrice,
		request.InputTokens, request.MaxOutputTokens,
		0, nil, createdAt, expiresAt, "b7it-runtime", leaseExpiresAt)
	if err != nil {
		t.Fatalf("building a reservation: %v", err)
	}
	if err := repos.reserves.Insert(ctx, reservation); err != nil {
		t.Fatalf("inserting the reaper's reservation: %v", err)
	}
	return reservation.ID, request.ID
}

// integrationStreamRow reads the feed's identity row: the epoch cursors are
// minted under and the newest allocated sequence. A database that has never
// appended has no row — exists comes back false and both values come back
// zero, the caller deciding what an unborn stream means for it.
func integrationStreamRow(t testing.TB, db *sql.DB) (epoch string, lastSeq int64, exists bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := db.QueryRowContext(ctx, "SELECT epoch::text, last_seq FROM public.usage_events_stream WHERE singleton").Scan(&epoch, &lastSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false
	}
	if err != nil {
		t.Fatalf("reading the fact stream identity: %v", err)
	}
	return epoch, lastSeq, true
}

// integrationStreamCursor returns the cursor naming the stream's current end:
// the genesis position when nothing has ever been appended, the canonical
// position otherwise. The tests are in the adapter's own package, so the
// cursor's format is in scope, and using it is how a test on the shared
// fixture scopes its reads to the facts it appended itself — the stream
// carries every earlier test's and every earlier run's rows, and only a
// positioned read tells them apart. A test that needs a literal empty-feed
// genesis read mints its own database (integrationThrowawayDatabase) instead
// of standing on the accidents of run order.
func integrationStreamCursor(t testing.TB, db *sql.DB) string {
	t.Helper()
	epoch, lastSeq, exists := integrationStreamRow(t, db)
	if !exists {
		return genesisCursor
	}
	return encodeCursor(epoch, lastSeq)
}

// integrationProjectionRow reads one projection back the way the tests assert
// on it: the runtime-owned number (available) beside the Control-Plane-owned
// fields a publication moves (limit, state, revision), straight off the row —
// never derived from what a Drawdown returned, which is a decision's output,
// not the state it left.
func integrationProjectionRow(t testing.TB, db *sql.DB, bucket string) (available, limitAmount, revision int64, state string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := db.QueryRowContext(ctx, "SELECT available, limit_amount, revision, state FROM public.quota_projections WHERE funding_bucket_id = $1", bucket).
		Scan(&available, &limitAmount, &revision, &state)
	if err != nil {
		t.Fatalf("reading projection %s: %v", bucket, err)
	}
	return available, limitAmount, revision, state
}

// integrationReservationState reads one hold's state by its own id.
func integrationReservationState(t testing.TB, db *sql.DB, id identity.ReservationID) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var state string
	if err := db.QueryRowContext(ctx, "SELECT state FROM public.reservations WHERE id = $1", string(id)).Scan(&state); err != nil {
		t.Fatalf("reading reservation %s: %v", id, err)
	}
	return state
}

// integrationReservationStateForRequest reads one hold's state by the request
// it holds for — the UNIQUE(request_id) the schema guarantees, which makes the
// request a usable handle on the hold when the test never kept the id.
func integrationReservationStateForRequest(t testing.TB, db *sql.DB, requestID identity.RequestID) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var state string
	if err := db.QueryRowContext(ctx, "SELECT state FROM public.reservations WHERE request_id = $1", string(requestID)).Scan(&state); err != nil {
		t.Fatalf("reading the reservation of request %s: %v", requestID, err)
	}
	return state
}

// integrationRequestStatus reads one request row's status.
func integrationRequestStatus(t testing.TB, db *sql.DB, requestID identity.RequestID) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var status string
	if err := db.QueryRowContext(ctx, "SELECT status FROM public.requests WHERE id = $1", string(requestID)).Scan(&status); err != nil {
		t.Fatalf("reading the status of request %s: %v", requestID, err)
	}
	return status
}

// integrationIntakeRow counts the replay records one (account, key) carries —
// the engine's unique key means the only honest answers are 0 and 1.
func integrationIntakeRow(t testing.TB, db *sql.DB, account, key string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM public.request_intake WHERE account_id = $1 AND idempotency_key = $2", account, key).Scan(&count); err != nil {
		t.Fatalf("counting the replay records of (%s, %s): %v", account, key, err)
	}
	return count
}

// integrationFactCount counts the facts one request carries.
func integrationFactCount(t testing.TB, db *sql.DB, requestID identity.RequestID) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM public.usage_events WHERE request_id = $1", string(requestID)).Scan(&count); err != nil {
		t.Fatalf("counting the facts of request %s: %v", requestID, err)
	}
	return count
}

// integrationStoredPayload returns the payload bytes exactly as the database
// stores them for one request's fact. That is not the same byte string the
// writer marshalled: jsonb normalises its text form (keys by length then
// bytewise, whitespace gone), so the reader's byte-level contract is against
// the stored form — the bytes that came back are the bytes that were stored.
func integrationStoredPayload(t testing.TB, db *sql.DB, requestID identity.RequestID) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var payload []byte
	if err := db.QueryRowContext(ctx, "SELECT payload FROM public.usage_events WHERE request_id = $1", string(requestID)).Scan(&payload); err != nil {
		t.Fatalf("reading back the stored payload of request %s: %v", requestID, err)
	}
	return payload
}

// assertTerminalUpdateRefused drives one raw UPDATE at a terminal row and
// asserts the BEFORE UPDATE trigger refuses it. The CAS in every repository
// WHERE clause is the real guard; the trigger is the bug detector behind it,
// and this is where the suite proves the detector is actually armed — that a
// write which reaches a terminal row by any path other than the CAS fails
// loudly instead of landing.
func assertTerminalUpdateRefused(t *testing.T, db *sql.DB, statement string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, statement, args...)
	if err == nil {
		t.Fatalf("UPDATE on a terminal row error = nil, want the terminal-immutability trigger's exception (%s)", statement)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("UPDATE on a terminal row error = %v, want the trigger's raise_exception (P0001)", err)
	}
	if !strings.Contains(pgErr.Message, "cannot be updated") {
		t.Errorf("trigger message = %q, want the terminal-row refusal named", pgErr.Message)
	}
}

// TestIntegrationRuntimeSchemaAppliesOnAFreshDatabase proves the path CI
// exercises and the local fixture never shows: a database with nothing in it
// comes up whole under the helper — every runtime table present, the lane's
// history recorded as exactly one clean row, and the fact reader answering a
// genesis read. It runs against a throwaway database precisely so the
// fixture's already-migrated dataplane proves nothing about this path: an
// already-applied schema proves the helper can skip, never that it can apply.
func TestIntegrationRuntimeSchemaAppliesOnAFreshDatabase(t *testing.T) {
	integrationThrowawaySerialise(t)
	db := integrationThrowawayDatabase(t, "dataplane_b7_schema_probe")
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Every table the runtime schema owns, by name, through to_regclass: the
	// function answers NULL for a relation that does not exist, which is the
	// one verdict a fresh database must never give about its own schema.
	for _, table := range []string{
		"requests", "request_attempts", "request_intake",
		"reservations", "reservation_allocations", "quota_projections",
		"usage_events", "usage_events_stream",
	} {
		var regclass *string
		if err := db.QueryRowContext(ctx, "SELECT to_regclass($1)::text", "public."+table).Scan(&regclass); err != nil {
			t.Fatalf("to_regclass(public.%s): %v", table, err)
		}
		if regclass == nil {
			t.Errorf("to_regclass(public.%s) = NULL — the fresh database must carry every table the lane's migrations create", table)
		}
	}

	// The history the helper recorded: exactly one row, at the lane's newest
	// version, not dirty — the state golang-migrate itself would leave, so the
	// real runner can take over from the helper with nothing to repair.
	entries, err := os.ReadDir(filepath.Join(integrationRepositoryRoot(t), "migrations", "dataplane"))
	if err != nil {
		t.Fatalf("reading the migration lane: %v", err)
	}
	newest := int64(0)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".up.sql") && len(entry.Name()) >= 6 {
			if version, err := strconv.Atoi(entry.Name()[:6]); err == nil && int64(version) > newest {
				newest = int64(version)
			}
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT version, dirty FROM public.schema_migrations")
	if err != nil {
		t.Fatalf("reading public.schema_migrations: %v", err)
	}
	defer rows.Close()
	var history []struct {
		version int64
		dirty   bool
	}
	for rows.Next() {
		var one struct {
			version int64
			dirty   bool
		}
		if err := rows.Scan(&one.version, &one.dirty); err != nil {
			t.Fatalf("scanning public.schema_migrations: %v", err)
		}
		history = append(history, one)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading public.schema_migrations: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("public.schema_migrations carries %d rows, want exactly one — golang-migrate's single-row history", len(history))
	}
	if history[0].version != newest || history[0].dirty {
		t.Errorf("public.schema_migrations = (version %d, dirty %t), want (version %d, dirty false) — the newest migration in the lane, cleanly applied", history[0].version, history[0].dirty, newest)
	}

	// And the reader the schema serves: on a database that has never appended
	// a fact there is no stream row at all, which is an empty feed and not a
	// broken one — the genesis position, no error, no events.
	reader := NewUsageFacts(db)
	page, err := reader.Read(ctx, "", usagefacts.DefaultLimit)
	if err != nil {
		t.Fatalf("Read() on a feed that never appended: %v", err)
	}
	if page.NextCursor != genesisCursor {
		t.Errorf("Read() NextCursor = %q, want the genesis position %q", page.NextCursor, genesisCursor)
	}
	if len(page.Events) != 0 || page.HasMore {
		t.Errorf("Read() on an empty feed returned %d events (HasMore %t), want none", len(page.Events), page.HasMore)
	}
}

// TestIntegrationReplayServesTheSameFactsOnEveryRead is the spec's
// non-negotiable, walked end to end: settle three requests, then page the
// feed from genesis — the same read twice after a simulated consumer crash
// returns the same facts and the same cursor (at-least-once; a read never
// advances a position), the pages strictly after the previous cursor, an
// empty tail that restates a stable non-empty position, and payloads byte-
// identical to what was stored.
//
// It runs on a throwaway database because its assertions are about the feed
// from genesis: the shared fixture's stream carries every earlier run's rows,
// and a genesis read there would measure the suite's own run order rather
// than the reader.
func TestIntegrationReplayServesTheSameFactsOnEveryRead(t *testing.T) {
	integrationThrowawaySerialise(t)
	db := integrationThrowawayDatabase(t, "dataplane_b7_replay_probe")
	integrationRuntimeSchema(t, db)
	repos := integrationRepos(t, New(db))
	reader := NewUsageFacts(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Genesis before anything exists, so the test's later pages are provably
	// the only things the feed ever carried.
	first, err := reader.Read(ctx, "", 2)
	if err != nil {
		t.Fatalf("Read() from genesis: %v", err)
	}
	if page := (usagefacts.Page{}); first.NextCursor != genesisCursor || len(first.Events) != 0 || first.HasMore != page.HasMore {
		t.Fatalf("Read() from an unborn feed = %+v, want the genesis position and no events", first)
	}

	// Three settlements, one unit of work each, with payload-carrying buckets
	// named per request so every fact's payload is distinguishable on the feed.
	const settled = 3
	requests := make([]execution.Request, 0, settled)
	facts := make([]accounting.Fact, 0, settled)
	seqs := make([]int64, 0, settled)
	for i := 0; i < settled; i++ {
		request, fact, seq := integrationSettlement(t, ctx, repos, integrationRuntimeAccount(t, fmt.Sprintf("replay-f%d", i+1)))
		requests = append(requests, request)
		facts = append(facts, fact)
		seqs = append(seqs, seq)
	}

	page, err := reader.Read(ctx, "", 2)
	if err != nil {
		t.Fatalf("Read() from genesis: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("page one carried %d events, want 2 (limit 2)", len(page.Events))
	}
	if !page.HasMore {
		t.Errorf("page one HasMore = false, want true — the third fact is still behind the cursor")
	}
	if got := []string{page.Events[0].RequestID, page.Events[1].RequestID}; got[0] != string(requests[0].ID) || got[1] != string(requests[1].ID) {
		t.Errorf("page one = %v, want facts one and two in append order", got)
	}
	for i, event := range page.Events {
		if event.Kind != string(accounting.KindSettled) || event.SchemaVersion != accounting.SchemaVersion {
			t.Errorf("page one event %d = (kind %q, schema %d), want (settled, %d)", i, event.Kind, event.SchemaVersion, accounting.SchemaVersion)
		}
		// Byte equality against the stored form is the reader's payload
		// contract (see integrationStoredPayload for why the writer's bytes
		// and the stored bytes differ).
		if !bytes.Equal(event.Payload, integrationStoredPayload(t, db, requests[i].ID)) {
			t.Errorf("page one event %d payload = %s, want the stored bytes %s", i, event.Payload, integrationStoredPayload(t, db, requests[i].ID))
		}
	}
	cursorAfterTwo := page.NextCursor
	if cursorAfterTwo == "" {
		t.Fatal("page one NextCursor is empty — a page that delivered events must name where it stopped")
	}

	// The consumer crash: it read page one and died before storing the
	// cursor, so its position is still genesis. The same read again must
	// return the same two facts and the same cursor — replay is normal, and
	// nothing about reading consumes or advances anything.
	replay, err := reader.Read(ctx, "", 2)
	if err != nil {
		t.Fatalf("Read() again from genesis: %v", err)
	}
	if replay.NextCursor != cursorAfterTwo || len(replay.Events) != 2 || !replay.HasMore {
		t.Fatalf("the repeated read = (cursor %q, %d events, HasMore %t), want page one restated exactly", replay.NextCursor, len(replay.Events), replay.HasMore)
	}
	for i := range replay.Events {
		if replay.Events[i].RequestID != page.Events[i].RequestID || !bytes.Equal(replay.Events[i].Payload, page.Events[i].Payload) {
			t.Errorf("repeated read event %d differs from the first read — the same range must return the same facts", i)
		}
	}

	// Continuing from the stored cursor: strictly after it, so fact three —
	// and nothing already delivered.
	second, err := reader.Read(ctx, cursorAfterTwo, 2)
	if err != nil {
		t.Fatalf("Read(cursor, 2): %v", err)
	}
	if len(second.Events) != 1 || second.Events[0].RequestID != string(requests[2].ID) {
		t.Fatalf("the page after cursor %q = %v, want exactly fact three", cursorAfterTwo, second.Events)
	}
	if second.HasMore {
		t.Errorf("the page after the last fact HasMore = true, want false — the hint must not promise work that is not there")
	}
	cursorAfterThree := second.NextCursor

	// The caught-up read: an empty page whose position is the one it asked
	// about, restated canonically — non-empty (a consumer that applied nothing
	// still has a defined position) and stable across repeats.
	tail, err := reader.Read(ctx, cursorAfterThree, 2)
	if err != nil {
		t.Fatalf("Read(past-the-end, 2): %v", err)
	}
	if len(tail.Events) != 0 {
		t.Errorf("the caught-up read returned %d events, want none", len(tail.Events))
	}
	if tail.NextCursor == "" || tail.NextCursor != cursorAfterThree {
		t.Errorf("the caught-up read NextCursor = %q, want the asked-for position %q restated — stable and non-empty", tail.NextCursor, cursorAfterThree)
	}
}

// TestIntegrationAppendOrderIsCommitOrderUnderContention fires six settlement
// appends concurrently and asserts the one property the stream's single-row
// allocation exists for: the facts land at exactly the next six positions,
// contiguous, one global order, no duplicates — the allocation order being
// the commit order, because the stream row's lock is held to each
// transaction's commit. It then appends two facts whose occurred_at values
// are deliberately inverted and asserts the page order still follows
// append_seq: the feed's order is a position, never a timestamp.
func TestIntegrationAppendOrderIsCommitOrderUnderContention(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The burst starts where the stream currently ends — the fixture carries
	// every earlier run's facts, and "the next six positions" is relative to
	// that, not to zero.
	baseEpoch, baseSeq, baseExists := integrationStreamRow(t, db)
	baseCursor := integrationStreamCursor(t, db)

	// One account carries the whole burst: the contention the test needs is
	// between writers, not between accounts, and every row it mints still
	// names an account no other test will ever hold.
	burstAccount := integrationRuntimeAccount(t, "burst")

	const contenders = 6
	// The units are formed here, on the test goroutine — t.Fatal may only be
	// called from it — and written concurrently, each in its own unit of work.
	type burstUnit struct {
		request execution.Request
		attempt execution.Attempt
		fact    accounting.Fact
	}
	units := make([]burstUnit, contenders)
	for i := range units {
		request, attempt, fact := integrationFormSettled(t, burstAccount, nil, time.Now().UTC())
		units[i] = burstUnit{request: request, attempt: attempt, fact: fact}
	}
	type appended struct {
		request identity.RequestID
		seq     int64
		err     error
	}
	results := make(chan appended, contenders)
	var wg sync.WaitGroup
	for _, unit := range units {
		wg.Add(1)
		go func(unit burstUnit) {
			defer wg.Done()
			seq, err := integrationWriteSettled(ctx, repos, unit.request, unit.attempt, unit.fact)
			results <- appended{request: unit.request.ID, seq: seq, err: err}
		}(unit)
	}
	wg.Wait()
	close(results)

	seen := make(map[int64]identity.RequestID, contenders)
	for result := range results {
		if result.err != nil {
			t.Fatalf("a contended append failed: %v", result.err)
		}
		if other, dup := seen[result.seq]; dup {
			t.Fatalf("two facts claim append_seq %d (%s and %s) — the stream's allocation must be exclusive", result.seq, other, result.request)
		}
		seen[result.seq] = result.request
	}
	if len(seen) != contenders {
		t.Fatalf("%d facts appended, want %d", len(seen), contenders)
	}
	ordered := make([]int64, 0, contenders)
	for seq := range seen {
		ordered = append(ordered, seq)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for i, seq := range ordered {
		if want := baseSeq + int64(i) + 1; seq != want {
			t.Errorf("append_seq = %d at position %d, want %d — committed facts must occupy exactly the next positions in one global order", seq, i, want)
		}
	}

	// The stream row: still exactly one, its epoch the one the burst started
	// under (one stable identity, never re-minted mid-stream), its last_seq
	// the highest position the burst allocated.
	epoch, lastSeq, exists := integrationStreamRow(t, db)
	if !exists {
		t.Fatal("the stream row vanished during the burst")
	}
	if baseExists && epoch != baseEpoch {
		t.Errorf("the stream epoch moved from %s to %s during ordinary appends — the epoch is minted once, by the first append", baseEpoch, epoch)
	}
	if epoch == "" {
		t.Error("the stream epoch is empty — the server mints it on first append and never after")
	}
	if lastSeq != ordered[len(ordered)-1] {
		t.Errorf("stream last_seq = %d, want %d — the newest allocated position", lastSeq, ordered[len(ordered)-1])
	}

	// Reading the burst's range back: exactly its facts, in allocation order.
	page, err := NewUsageFacts(db).Read(ctx, baseCursor, 100)
	if err != nil {
		t.Fatalf("Read(the burst's range): %v", err)
	}
	if len(page.Events) != contenders {
		t.Fatalf("the burst's range carried %d events, want %d", len(page.Events), contenders)
	}
	for i, event := range page.Events {
		if want := seen[ordered[i]]; event.RequestID != string(want) {
			t.Errorf("position %d carried request %s, want %s — the page order is the append order", i, event.RequestID, want)
		}
	}

	// The clock is not the order: append a fact whose occurred_at is two
	// hours ahead, then one an hour ahead, and assert the page still reads in
	// append order — ordering by occurred_at would trust clocks that
	// disagree, and the feed exists so nobody has to.
	base := time.Now().UTC().Truncate(time.Second)
	firstID, _, firstSeq := integrationAppendSettled(t, ctx, repos, integrationRuntimeAccount(t, "clock-late"), nil, base.Add(2*time.Hour))
	secondID, _, secondSeq := integrationAppendSettled(t, ctx, repos, integrationRuntimeAccount(t, "clock-early"), nil, base.Add(1*time.Hour))
	if secondSeq != firstSeq+1 {
		t.Fatalf("the inverted pair allocated %d then %d, want consecutive positions", firstSeq, secondSeq)
	}
	inverted, err := NewUsageFacts(db).Read(ctx, encodeCursor(epoch, firstSeq-1), 10)
	if err != nil {
		t.Fatalf("Read(the inverted pair): %v", err)
	}
	if len(inverted.Events) != 2 {
		t.Fatalf("the inverted pair's range carried %d events, want 2", len(inverted.Events))
	}
	if inverted.Events[0].RequestID != string(firstID) || inverted.Events[1].RequestID != string(secondID) {
		t.Errorf("the pair read back as [%s, %s], want the append order [%s, %s] — later wall-clock, appended first, comes first",
			inverted.Events[0].RequestID, inverted.Events[1].RequestID, firstID, secondID)
	}
	if !inverted.Events[0].OccurredAt.After(inverted.Events[1].OccurredAt) {
		t.Errorf("the page's occurred_at values are not inverted (%v then %v) — the test's own setup stopped being what it asserts against",
			inverted.Events[0].OccurredAt, inverted.Events[1].OccurredAt)
	}
}

// TestIntegrationContendedDrawdownAdmitsExactlyTheCapacityThereIs puts eight
// concurrent drawdowns of 40 against one bucket seeded with 100 and asserts
// the whole point of the conditional update: exactly two win, exactly six are
// told the waterfall cannot cover them, nothing ever goes negative, and the
// winners' legs account for every unit that left. Contention is real here —
// the pool has four connections for eight units — and deadlock-free, because
// every writer walks the same waterfall order.
func TestIntegrationContendedDrawdownAdmitsExactlyTheCapacityThereIs(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	account := integrationRuntimeAccount(t, "drawdown")
	bucket := integrationSeedProjection(t, ctx, repos, account, true, time.Now().UTC().Add(24*time.Hour), 100)

	const contenders = 8
	const draw = int64(40)
	type outcome struct {
		err  error
		legs []accounting.Allocation
	}
	results := make(chan outcome, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var drawn []accounting.Allocation
			// The drawdown runs inside its own unit of work — the ports'
			// doctrine, and the property under test: the takes and the
			// giveback are one atomic unit, so a losing walk leaves the
			// bucket exactly as it found it.
			err := store.WithinTx(ctx, func(ctx context.Context) error {
				got, err := repos.quota.Drawdown(ctx, account, draw)
				if err != nil {
					return err
				}
				drawn = got
				return nil
			})
			results <- outcome{err: err, legs: drawn}
		}()
	}
	wg.Wait()
	close(results)

	winners := 0
	insufficient := 0
	var drawnTotal int64
	for result := range results {
		switch {
		case result.err == nil:
			winners++
			for _, leg := range result.legs {
				if leg.FundingBucketID != bucket {
					t.Errorf("a winner drew from %q, want only %q", leg.FundingBucketID, bucket)
				}
				drawnTotal += leg.Amount
			}
		case errors.Is(result.err, accounting.ErrInsufficientCapacity):
			insufficient++
			if len(result.legs) != 0 {
				t.Errorf("a losing walk returned %d legs — the giveback must leave nothing drawn behind", len(result.legs))
			}
		default:
			t.Fatalf("a contended drawdown failed with neither a win nor the capacity verdict: %v", result.err)
		}
	}
	if winners != 2 {
		t.Errorf("%d drawdowns of %d against %d capacity succeeded, want exactly 2", winners, draw, 100)
	}
	if insufficient != contenders-winners {
		t.Errorf("%d drawdowns were refused, want %d", insufficient, contenders-winners)
	}
	if drawnTotal != 80 {
		t.Errorf("the winners' legs sum to %d, want 80 — every unit that left must be accounted for", drawnTotal)
	}
	if available, _, _, _ := integrationProjectionRow(t, db, bucket); available != 20 {
		t.Errorf("available after the contention = %d, want 20 — the store granted 80 and nothing overdrawned it", available)
	}
}

// TestIntegrationSettlementDedupAllowsOneClosePerRequest races eight closes of
// one request and asserts the engine half of idempotency: the dedup partial
// unique admits exactly one settlement-relevant fact, refuses the duplicated
// settled close and a released close behind it (the settlement key spans
// settled, released and expired), admits the correction fact as the sanctioned
// exception, and keeps the orphan key separate — one unbillable_orphaned fact
// may coexist with the settlement, a second may not.
func TestIntegrationSettlementDedupAllowsOneClosePerRequest(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The request and its attempt exist before anyone closes: the fact's
	// foreign keys are the settlement's anchors, and the dedup is about the
	// fact, not about them.
	account := integrationRuntimeAccount(t, "dedup")
	request, attempt, fact := integrationFormSettled(t, account, nil, time.Now().UTC())
	err := store.WithinTx(ctx, func(ctx context.Context) error {
		if err := repos.requests.Insert(ctx, request); err != nil {
			return err
		}
		return repos.attempts.Insert(ctx, attempt)
	})
	if err != nil {
		t.Fatalf("inserting the request and its attempt: %v", err)
	}

	const closers = 8
	type closeResult struct {
		seq int64
		err error
	}
	results := make(chan closeResult, closers)
	var wg sync.WaitGroup
	for i := 0; i < closers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var (
				seq int64
				err error
			)
			// Each close is its own unit of work, the settlement shape: a
			// loser's sequence allocation rolls back with it, so a refused
			// close leaves no hole behind.
			err = store.WithinTx(ctx, func(ctx context.Context) error {
				allocated, err := repos.facts.Append(ctx, fact)
				seq = allocated
				return err
			})
			results <- closeResult{seq: seq, err: err}
		}()
	}
	wg.Wait()
	close(results)

	winners := 0
	var winnerSeq int64
	for result := range results {
		switch {
		case result.err == nil:
			winners++
			winnerSeq = result.seq
		case errors.Is(result.err, persistence.ErrDuplicateFact):
			// The one answer a duplicated close may get: the winner's fact
			// stands, this caller wrote nothing.
		default:
			t.Fatalf("a duplicated close failed with the wrong error: %v", result.err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d closes of one request succeeded, want exactly 1", winners)
	}

	// The settlement key spans every terminal close: a released fact for the
	// same request is the same second ending, whatever its kind.
	released, err := accounting.NewReleased(request.ID, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("building a released fact: %v", err)
	}
	if _, err := repos.facts.Append(ctx, released); !errors.Is(err, persistence.ErrDuplicateFact) {
		t.Errorf("Append(released for a settled request) error = %v, want persistence.ErrDuplicateFact — the settlement key spans settled, released and expired", err)
	}

	// The correction is the sanctioned exception: a NEW fact referencing the
	// original through CorrectsAppendSeq sits outside the partial index, so
	// the wire shape never forecloses corrections without admitting a second
	// settlement today.
	correction, err := accounting.NewReleased(request.ID, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("building the correction fact: %v", err)
	}
	correction.CorrectsAppendSeq = &winnerSeq
	if _, err := repos.facts.Append(ctx, correction); err != nil {
		t.Errorf("Append(corrects_append_seq set) error = %v, want nil — the correction is the one fact the dedup admits behind the original", err)
	}

	// The orphan key is a different key: one proven orphaned completion may
	// coexist with the settlement by design...
	orphan, err := accounting.NewUnbillableOrphaned(request.ID, attempt.ID, accounting.CaptureGatewayObserved,
		int64Ptr(120), int64Ptr(45), int64Ptr(45), nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("building an orphaned fact: %v", err)
	}
	if _, err := repos.facts.Append(ctx, orphan); err != nil {
		t.Errorf("Append(unbillable_orphaned beside a settlement) error = %v, want nil — the orphan key is separate by design", err)
	}
	// ...and a second orphan is the same duplicated ending a second time.
	if _, err := repos.facts.Append(ctx, orphan); !errors.Is(err, persistence.ErrDuplicateFact) {
		t.Errorf("Append(a second orphan) error = %v, want persistence.ErrDuplicateFact", err)
	}

	if got := integrationFactCount(t, db, request.ID); got != 3 {
		t.Errorf("the request carries %d facts, want 3 — one settlement, one correction naming it, one orphan", got)
	}
}

// TestIntegrationRefusedCursorsFailClosed walks the reader's cursor refusal
// table: every spelling this adapter never issued — malformed, mistyped,
// negative, non-canonical — is refused with ErrCursorExpired, the port's
// answer for a position it cannot place (which the management surface maps to
// 410, the protocol's stop-and-ask-a-human). It then proves the epoch half
// does its job: after a simulated lost stream the old cursor names an epoch
// that no longer exists, and the reader must fail closed rather than serve the
// new stream's facts to a consumer that believes it delivered them.
func TestIntegrationRefusedCursorsFailClosed(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	reader := NewUsageFacts(db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The position before the append, then one append, so the cursor under
	// test names a real page on a real stream — the position the reader hands
	// back afterwards is the evidence the read was scoped exactly here.
	account := integrationRuntimeAccount(t, "cursor")
	before := integrationStreamCursor(t, db)
	requestID, _, _ := integrationAppendSettled(t, ctx, repos, account, nil, time.Now().UTC())
	page, err := reader.Read(ctx, before, 10)
	if err != nil {
		t.Fatalf("Read(the appended range): %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].RequestID != string(requestID) {
		t.Fatalf("the appended range carried %v, want exactly this test's fact", page.Events)
	}
	valid := page.NextCursor
	parts := strings.Split(valid, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		t.Fatalf("the reader minted cursor %q, want the v1.<epoch>.<seq> spelling it is documented to issue", valid)
	}

	refusals := []string{
		"nonsense",               // not a cursor at all
		"v1.",                    // the prefix and nothing else
		"v2.1.2",                 // a version this adapter never issued
		"v1.abc.def",             // an epoch-shaped word and a non-number
		"v1." + parts[1] + ".-1", // a negative position
		// The uppercase epoch: a valid cursor re-encoded in a spelling this
		// adapter never mints. uuid spellings are lowercase here, and a cursor
		// that parses but does not round-trip byte for byte is refused rather
		// than normalised — canonical or refused, no halfway.
		"v1." + strings.ToUpper(parts[1]) + "." + parts[2],
		// The leading zero: parses to the same position, spells it
		// differently, and a spelling this adapter never issued must never
		// start working.
		"v1." + parts[1] + ".0" + parts[2],
	}
	for _, candidate := range refusals {
		_, err := reader.Read(ctx, candidate, 5)
		if err == nil {
			t.Errorf("Read(%q) error = nil, want a refusal", candidate)
			continue
		}
		if !errors.Is(err, usagefacts.ErrCursorExpired) {
			t.Errorf("Read(%q) error = %v, want it to wrap usagefacts.ErrCursorExpired", candidate, err)
		}
		if errors.Is(err, usagefacts.ErrSourceUnavailable) {
			t.Errorf("Read(%q) error = %v, want a cursor decision — a refused position is not a broken source", candidate, err)
		}
	}

	// The lost stream. No trigger guards either delete — append-only is a
	// writer discipline, not a DB guard — and this is the one place the suite
	// is allowed to break that discipline, because the discipline's failure
	// mode is exactly what has to be proved: a recreated database mints a new
	// epoch on its first append, and every cursor from the old stream must
	// fail closed instead of silently skipping the re-appended facts.
	if _, err := db.ExecContext(ctx, "DELETE FROM public.usage_events"); err != nil {
		t.Fatalf("simulating the lost feed: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM public.usage_events_stream WHERE singleton"); err != nil {
		t.Fatalf("simulating the lost stream identity: %v", err)
	}
	_, _, newSeq := integrationAppendSettled(t, ctx, repos, integrationRuntimeAccount(t, "cursor-reborn"), nil, time.Now().UTC())
	if newSeq != 1 {
		t.Errorf("the first append of the new stream allocated %d, want 1 — a new epoch counts from the beginning", newSeq)
	}
	_, err = reader.Read(ctx, valid, 10)
	if !errors.Is(err, usagefacts.ErrCursorExpired) {
		t.Errorf("Read(a cursor from the lost stream) error = %v, want usagefacts.ErrCursorExpired — the epoch half must fail the cursor closed, never serve the new stream", err)
	}
}

// TestIntegrationSettlementUnitIsAllOrNothing is the crash-consistency proof,
// in the shape the runtime actually runs: admission commits its unit (request,
// attempt, drawdown, open hold), then the settlement unit closes the hold and
// appends the fact and FAILS — and leaves neither behind, the hold open, no
// fact, the admission's rows exactly as they were. The failing unit's error is
// returned after both statements have run, so the rollback is the only thing
// that could have removed them. The successful twin commits both sides
// together, which is the whole promise.
func TestIntegrationSettlementUnitIsAllOrNothing(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// One account, one grant, enough capacity for both twins.
	account := integrationRuntimeAccount(t, "atomicity")
	integrationSeedProjection(t, ctx, repos, account, true, time.Now().UTC().Add(24*time.Hour), 5000)
	price := integrationPrice()
	now := time.Now().UTC()
	request, err := execution.NewRequest(identity.NewRequestID(), account, account+"-api-key", "bench/alias", 120, 4096, price, now)
	if err != nil {
		t.Fatalf("building a request: %v", err)
	}
	attempt, err := execution.NewAttempt(identity.NewAttemptID(), request.ID, 0, 0, "b7it-backend", "b7it/model", execution.OutcomeSucceeded, "", now, now)
	if err != nil {
		t.Fatalf("building an attempt: %v", err)
	}

	// The admission unit commits first, the way the runtime actually runs:
	// the request, its attempt, the drawdown and the open hold are durable
	// before any settlement begins. The grant is spent 250 of its 5000 by
	// what follows — the settlement unit's failure must not change that.
	drawn, err := repos.quota.Drawdown(ctx, account, 250)
	if err != nil {
		t.Fatalf("Drawdown(250): %v", err)
	}
	reservation, err := accounting.NewReservation(identity.NewReservationID(), request.ID,
		price.RevisionID, price.InputUnitPrice, price.OutputUnitPrice,
		request.InputTokens, request.MaxOutputTokens,
		250, drawn, now, now.Add(time.Hour), "b7it-runtime", now.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("building the hold: %v", err)
	}
	err = store.WithinTx(ctx, func(ctx context.Context) error {
		if err := repos.requests.Insert(ctx, request); err != nil {
			return err
		}
		if err := repos.attempts.Insert(ctx, attempt); err != nil {
			return err
		}
		return repos.reserves.Insert(ctx, reservation)
	})
	if err != nil {
		t.Fatalf("the admission unit: %v", err)
	}
	if available, _, _, _ := integrationProjectionRow(t, db, account+"-bucket"); available != 4750 {
		t.Fatalf("available after admission = %d, want 4750", available)
	}

	// The failing settlement unit: the close and the append BOTH run, then the
	// unit reports failure. Everything it did — the close, the fact, the
	// sequence it allocated — rolls back, and the admission's rows stand
	// exactly as they were.
	boom := errors.New("the settlement unit failed after both statements")
	err = store.WithinTx(ctx, func(ctx context.Context) error {
		closed, err := repos.reserves.Close(ctx, reservation.ID, accounting.StateSettled, now)
		if err != nil {
			return err
		}
		if !closed {
			return errors.New("reservation.Close() = false inside the settlement unit")
		}
		fact, err := accounting.NewSettled(request.ID, attempt.ID, accounting.CaptureReported,
			int64Ptr(120), int64Ptr(45), int64Ptr(45),
			price.RevisionID, price.InputUnitPrice, price.OutputUnitPrice, 375,
			integrationFactLegs(drawn), now)
		if err != nil {
			return err
		}
		if _, err := repos.facts.Append(ctx, fact); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithinTx() error = %v, want the unit's own failure handed back", err)
	}
	if got := integrationReservationState(t, db, reservation.ID); got != string(accounting.StateOpen) {
		t.Errorf("the hold's state after the failed unit = %q, want open — the close must roll back with the unit that failed", got)
	}
	if got := integrationFactCount(t, db, request.ID); got != 0 {
		t.Errorf("the request carries %d facts after the failed unit, want 0 — the append must roll back with the unit", got)
	}
	if status := integrationRequestStatus(t, db, request.ID); status != string(execution.StatusExecuting) {
		t.Errorf("the request's status after the failed unit = %q, want executing — nothing of the settlement's writes survived", status)
	}
	if available, _, _, _ := integrationProjectionRow(t, db, account+"-bucket"); available != 4750 {
		t.Errorf("available after the failed unit = %d, want 4750 — the admission's drawdown is durable, the settlement's is not", available)
	}

	// The successful twin: one unit, both sides visible afterwards — the
	// request that completed and the fact that counted it, or neither.
	twin, _, _ := integrationSettlement(t, ctx, repos, account+"-twin")
	if got := integrationReservationStateForRequest(t, db, twin.ID); got != string(accounting.StateSettled) {
		t.Errorf("the twin's hold = %q, want settled — the close and the fact commit together or not at all", got)
	}
	if got := integrationFactCount(t, db, twin.ID); got != 1 {
		t.Errorf("the twin carries %d facts, want exactly the one its close appended", got)
	}
}

// TestIntegrationPublicationAlgebraKeepsSpentCapacitySpent walks the
// publication algebra the migration header states, in order: a fresh
// publication seeds available at the limit; a redelivered revision is stale
// and must not resurrect what a drawdown spent; a newer revision moves the
// Control-Plane-owned fields and never available; a refill applies only at its
// guard revision; and a return puts back exactly what was drawn even when a
// publication has since shrunk the ceiling below outstanding capacity —
// available carries a floor, never a ceiling, or every shrunken grant would
// turn its own returns into failures.
func TestIntegrationPublicationAlgebraKeepsSpentCapacitySpent(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	account := integrationRuntimeAccount(t, "publication")
	bucket := account + "-bucket"
	periodEnd := time.Now().UTC().Add(24 * time.Hour)

	// Fresh: seeded, available starts at the limit — a new grant's whole
	// ceiling is offerable.
	if outcome := integrationPublish(t, ctx, repos, account, bucket, true, periodEnd, 500, 1, accounting.ProjectionActive); outcome != accounting.PublicationSeeded {
		t.Fatalf("the first publication = %q, want %q", outcome, accounting.PublicationSeeded)
	}
	if available, limit, revision, state := integrationProjectionRow(t, db, bucket); available != 500 || limit != 500 || revision != 1 || state != string(accounting.ProjectionActive) {
		t.Fatalf("the seeded row = (available %d, limit %d, revision %d, state %s), want (500, 500, 1, active)", available, limit, revision, state)
	}

	// Spend a fifth of it, so every later no-op has something it must not
	// resurrect.
	legs, err := repos.quota.Drawdown(ctx, account, 100)
	if err != nil {
		t.Fatalf("Drawdown(100): %v", err)
	}
	if len(legs) != 1 || legs[0].Amount != 100 {
		t.Fatalf("Drawdown(100) = %v, want one leg of 100", legs)
	}

	// The redelivery: same revision, arrived again after the drawdown. Stale,
	// and available must still be what the drawdown left.
	if outcome := integrationPublish(t, ctx, repos, account, bucket, true, periodEnd, 500, 1, accounting.ProjectionActive); outcome != accounting.PublicationStale {
		t.Errorf("the redelivered publication = %q, want %q — the revision decides, and this one already applied", outcome, accounting.PublicationStale)
	}
	if available, limit, _, _ := integrationProjectionRow(t, db, bucket); available != 400 || limit != 500 {
		t.Errorf("after the redelivery = (available %d, limit %d), want (400, 500) — a stale publication writes nothing", available, limit)
	}

	// The newer revision: the Control-Plane-owned fields move — limit and
	// state both — and available is deliberately not among them.
	if outcome := integrationPublish(t, ctx, repos, account, bucket, true, periodEnd, 800, 2, accounting.ProjectionInactive); outcome != accounting.PublicationUpdated {
		t.Errorf("the newer publication = %q, want %q", outcome, accounting.PublicationUpdated)
	}
	if available, limit, revision, state := integrationProjectionRow(t, db, bucket); available != 400 || limit != 800 || revision != 2 || state != string(accounting.ProjectionInactive) {
		t.Errorf("after the update = (available %d, limit %d, revision %d, state %s), want (400, 800, 2, inactive) — spent capacity must never be resurrected", available, limit, revision, state)
	}

	// The refill, guarded by the CURRENT revision: applies, available grows.
	refill, err := accounting.NewRefill(bucket, 50, 2)
	if err != nil {
		t.Fatalf("building the guarded refill: %v", err)
	}
	if applied, err := repos.quota.ApplyRefill(ctx, refill); err != nil || !applied {
		t.Fatalf("ApplyRefill(at revision 2) = (%t, %v), want (true, nil)", applied, err)
	}
	if available, _, _, _ := integrationProjectionRow(t, db, bucket); available != 450 {
		t.Errorf("available after the guarded refill = %d, want 450", available)
	}
	// The same increase replayed against the OLD revision: refused — an
	// unguarded refill replayed is capacity minted twice, the one way this
	// table could invent money.
	staleRefill, err := accounting.NewRefill(bucket, 50, 1)
	if err != nil {
		t.Fatalf("building the stale refill: %v", err)
	}
	if applied, err := repos.quota.ApplyRefill(ctx, staleRefill); err != nil || applied {
		t.Errorf("ApplyRefill(at revision 1) = (%t, %v), want (false, nil) — the guard revision has moved on", applied, err)
	}
	if available, _, _, _ := integrationProjectionRow(t, db, bucket); available != 450 {
		t.Errorf("available after the refused refill = %d, want 450", available)
	}

	// Back to active, and drain the whole projection, so the outstanding
	// capacity (450) is larger than the ceiling the next publication sets.
	if outcome := integrationPublish(t, ctx, repos, account, bucket, true, periodEnd, 800, 3, accounting.ProjectionActive); outcome != accounting.PublicationUpdated {
		t.Fatalf("reactivating = %q, want %q", outcome, accounting.PublicationUpdated)
	}
	if available, _, _, _ := integrationProjectionRow(t, db, bucket); available != 450 {
		t.Fatalf("the reactivation moved available to %d, want 450 — a publication never touches the runtime's number", available)
	}
	drained, err := repos.quota.Drawdown(ctx, account, 450)
	if err != nil {
		t.Fatalf("Drawdown(450): %v", err)
	}
	if outcome := integrationPublish(t, ctx, repos, account, bucket, true, periodEnd, 100, 4, accounting.ProjectionActive); outcome != accounting.PublicationUpdated {
		t.Fatalf("the shrinking publication = %q, want %q", outcome, accounting.PublicationUpdated)
	}
	if _, limit, _, _ := integrationProjectionRow(t, db, bucket); limit != 100 {
		t.Fatalf("the ceiling after the shrink = %d, want 100 — below the 450 outstanding", limit)
	}

	// The no-ceiling rule: the return lands whole. A return that failed
	// because the ceiling shrank would turn every capacity return of a
	// re-granted account into a failure, and the runtime's memory of what it
	// no longer holds is the truth the return states.
	returned, err := repos.quota.Return(ctx, drained)
	if err != nil {
		t.Fatalf("Return(the drained legs): %v", err)
	}
	if returned != len(drained) {
		t.Errorf("Return() = %d legs landed, want %d", returned, len(drained))
	}
	if available, _, _, _ := integrationProjectionRow(t, db, bucket); available != 450 {
		t.Errorf("available after the return = %d, want 450 — above the 100 ceiling, which is the point: available has a floor, never a ceiling", available)
	}
}

// TestIntegrationWaterfallDrawdownMatchesTheDomainOrder pins the one order the
// whole store shares: the SQL's `named_scope DESC, period_end ASC NULLS LAST,
// subscription_created_at ASC, entitlement_id ASC` and accounting.ByWaterfall
// must order the same rows identically, and a drawdown must walk that order.
// The rows are shaped so every comparator gets work to do — a named grant
// before a wildcard one despite a later period end, the earliest period end
// first, the PAYG balance last despite its infinite end — and the legs the
// drawdown returns name the buckets it touched, in the order it touched them.
func TestIntegrationWaterfallDrawdownMatchesTheDomainOrder(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	account := integrationRuntimeAccount(t, "waterfall")
	base := time.Now().UTC().Add(24 * time.Hour)
	// Four grants, one account, one subscription timestamp: the waterfall
	// inputs the tests vary are named scope and period end, and the tiebreaks
	// the order also carries (oldest subscription, entitlement id) are left
	// shared/random so they can never decide anything here.
	type grant struct {
		name   string
		bucket string
		named  bool
		endsIn time.Duration
		payg   bool
		limit  int64
		kept   int64
	}
	grantList := []grant{
		{name: "named-early", bucket: account + "-wf-early", named: true, endsIn: 12 * time.Hour, limit: 40, kept: 0},
		{name: "named-late", bucket: account + "-wf-late", named: true, endsIn: 48 * time.Hour, limit: 100, kept: 90},
		{name: "wildcard", bucket: account + "-wf-wild", named: false, endsIn: 24 * time.Hour, limit: 100, kept: 100},
		{name: "payg", bucket: account + "-wf-payg", named: false, payg: true, limit: 100, kept: 100},
	}
	for _, one := range grantList {
		end := base.Add(one.endsIn)
		if one.payg {
			end = time.Time{}
		}
		if outcome := integrationPublish(t, ctx, repos, account, one.bucket, one.named, end, one.limit, 1, accounting.ProjectionActive); outcome != accounting.PublicationSeeded {
			t.Fatalf("seeding %s = %q, want %q", one.name, outcome, accounting.PublicationSeeded)
		}
	}

	// The drawdown walks the order and records it in its legs: 50 against a
	// waterfall of 40, 100, 100, 100 must take the named-early grant whole
	// and finish on the named-late one.
	legs, err := repos.quota.Drawdown(ctx, account, 50)
	if err != nil {
		t.Fatalf("Drawdown(50): %v", err)
	}
	wantLegs := []accounting.Allocation{
		{FundingBucketID: grantList[0].bucket, Amount: 40, Ordinal: 1},
		{FundingBucketID: grantList[1].bucket, Amount: 10, Ordinal: 2},
	}
	if !reflect.DeepEqual(legs, wantLegs) {
		t.Errorf("Drawdown(50) legs = %+v, want %+v — the waterfall order, split where the first grant runs dry", legs, wantLegs)
	}
	for _, one := range grantList {
		if available, _, _, _ := integrationProjectionRow(t, db, one.bucket); available != one.kept {
			t.Errorf("%s available = %d, want %d", one.name, available, one.kept)
		}
	}

	// The domain half: the same rows read back unordered, built into the
	// domain's Projection, and ordered by accounting.ByWaterfall — the
	// function admission and return call. It must name the buckets in the
	// order the drawdown just walked.
	scanProjection := `SELECT account_id, scope_kind, entitlement_id, cycle_number,
	       funding_bucket_id, alias_group_version_id, named_scope,
	       period_end, subscription_created_at, state, limit_amount,
	       available, revision, seeded_at, updated_at
	FROM public.quota_projections
	WHERE account_id = $1`
	rows, err := db.QueryContext(ctx, scanProjection, account)
	if err != nil {
		t.Fatalf("reading the account's projections: %v", err)
	}
	defer rows.Close()
	var projections []accounting.Projection
	for rows.Next() {
		var (
			projection  accounting.Projection
			scope       string
			entitlement sql.NullString
			cycle       sql.NullInt64
			periodEnd   sql.NullTime
			state       string
		)
		if err := rows.Scan(&projection.AccountID, &scope, &entitlement, &cycle,
			&projection.FundingBucketID, &projection.AliasGroupVersionID, &projection.NamedScope,
			&periodEnd, &projection.SubscriptionCreatedAt, &state, &projection.LimitAmount,
			&projection.Available, &projection.Revision, &projection.SeededAt, &projection.UpdatedAt); err != nil {
			t.Fatalf("scanning a projection row: %v", err)
		}
		projection.ScopeKind = accounting.ScopeKind(scope)
		if entitlement.Valid {
			projection.EntitlementID = entitlement.String
		}
		if cycle.Valid {
			projection.CycleNumber = int(cycle.Int64)
		}
		if periodEnd.Valid {
			projection.PeriodEnd = periodEnd.Time
		}
		projection.State = accounting.ProjectionState(state)
		projections = append(projections, projection)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the account's projections: %v", err)
	}
	ordered := accounting.ByWaterfall(projections)
	wantOrder := []string{grantList[0].bucket, grantList[1].bucket, grantList[2].bucket, grantList[3].bucket}
	gotOrder := make([]string, 0, len(ordered))
	for _, projection := range ordered {
		gotOrder = append(gotOrder, projection.FundingBucketID)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Errorf("ByWaterfall ordered %v, want %v — named scope first, earliest period end next, the PAYG balance last", gotOrder, wantOrder)
	}

	// The SQL half: the adapter's own order by, spelled here exactly as
	// projectionWalk spells it, over the same rows. One order, two spellings,
	// one proof: this query and ByWaterfall disagreeing is the bug this test
	// exists to catch, and it is asserted against the domain's answer rather
	// than against a hardcoded list so the two halves are pinned to each
	// other, not merely to the test's expectations.
	sqlOrdered := `SELECT funding_bucket_id
	FROM public.quota_projections
	WHERE account_id = $1 AND state = 'active'
	ORDER BY named_scope DESC, period_end ASC NULLS LAST,
	         subscription_created_at ASC, entitlement_id ASC`
	sqlRows, err := db.QueryContext(ctx, sqlOrdered, account)
	if err != nil {
		t.Fatalf("reading the SQL waterfall order: %v", err)
	}
	defer sqlRows.Close()
	var sqlOrder []string
	for sqlRows.Next() {
		var bucket string
		if err := sqlRows.Scan(&bucket); err != nil {
			t.Fatalf("scanning the SQL waterfall order: %v", err)
		}
		sqlOrder = append(sqlOrder, bucket)
	}
	if err := sqlRows.Err(); err != nil {
		t.Fatalf("reading the SQL waterfall order: %v", err)
	}
	if !reflect.DeepEqual(sqlOrder, gotOrder) {
		t.Errorf("the SQL order by produced %v, want the domain's %v — the two spellings are one order or the waterfall is two waterfalls", sqlOrder, gotOrder)
	}
}

// TestIntegrationReaperExpiresOnlyLapsedLeases drives the reaper's batch CAS:
// a sweep closes exactly the holds whose leases have lapsed, oldest lease
// first, up to its limit, and returns them whole so the caller can append the
// expired fact in the same unit of work. A hold whose lease is still live
// survives every sweep, however many lapsed neighbours it sits between.
func TestIntegrationReaperExpiresOnlyLapsedLeases(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	now := time.Now().UTC()
	created := now.Add(-2 * time.Hour)
	expires := now.Add(time.Hour)
	// Three holds, one account: two leases lapsed (an hour ago and half an
	// hour ago), one still live. The reaper's order is lease_expires_at, so
	// the hour-old lease is the first victim a limited sweep takes.
	lapsedOld, oldRequest := integrationReservation(t, ctx, repos, integrationRuntimeAccount(t, "reaper-a"), created, expires, now.Add(-time.Hour))
	lapsedRecent, recentRequest := integrationReservation(t, ctx, repos, integrationRuntimeAccount(t, "reaper-b"), created, expires, now.Add(-30*time.Minute))
	fresh, freshRequest := integrationReservation(t, ctx, repos, integrationRuntimeAccount(t, "reaper-c"), created, expires, now.Add(30*time.Minute))

	victims, err := repos.reserves.ExpireLapsedLeases(ctx, 1)
	if err != nil {
		t.Fatalf("ExpireLapsedLeases(1): %v", err)
	}
	if len(victims) != 1 {
		t.Fatalf("the first sweep closed %d holds, want exactly the limit of 1", len(victims))
	}
	if victims[0].ID != lapsedOld || victims[0].RequestID != oldRequest {
		t.Errorf("the first sweep took hold %v for request %v, want the oldest lapsed lease %v for request %v", victims[0].ID, victims[0].RequestID, lapsedOld, oldRequest)
	}
	if victims[0].LeaseOwner != "b7it-runtime" || victims[0].PriceRevision != integrationPrice().RevisionID {
		t.Errorf("the victim carries owner %q under revision %q, want the hold's own lease owner and pricing basis — the expired fact derives from them", victims[0].LeaseOwner, victims[0].PriceRevision)
	}
	if got := integrationReservationState(t, db, lapsedOld); got != string(accounting.StateExpired) {
		t.Errorf("the first victim's state = %q, want expired", got)
	}
	if got := integrationReservationState(t, db, lapsedRecent); got != string(accounting.StateOpen) {
		t.Errorf("the half-hour lapsed hold = %q after a sweep of limit 1, want open — the limit bounds the sweep, not the lapse", got)
	}

	victims, err = repos.reserves.ExpireLapsedLeases(ctx, 1)
	if err != nil {
		t.Fatalf("the second ExpireLapsedLeases(1): %v", err)
	}
	if len(victims) != 1 || victims[0].ID != lapsedRecent || victims[0].RequestID != recentRequest {
		t.Errorf("the second sweep = %+v, want exactly the half-hour lapsed hold %v for request %v", victims, lapsedRecent, recentRequest)
	}

	victims, err = repos.reserves.ExpireLapsedLeases(ctx, 5)
	if err != nil {
		t.Fatalf("the sweeping ExpireLapsedLeases(5): %v", err)
	}
	if len(victims) != 0 {
		t.Errorf("the sweeping reaper closed %v, want none — the live lease is not the reaper's to take", victims)
	}
	if got := integrationReservationState(t, db, fresh); got != string(accounting.StateOpen) {
		t.Errorf("the fresh hold = %q after every sweep, want open", got)
	}

	// The reaper's whole contract is the pair, in one unit of work: the sweep
	// and the fact derived from what the sweep read back. A victim closed
	// without its fact would be a hold the runtime ended that no feed page
	// ever reports — the crash the settlement doctrine exists to prevent.
	lostVictim, lostRequest := integrationReservation(t, ctx, repos, integrationRuntimeAccount(t, "reaper-d"), created, expires, now.Add(-time.Hour))
	err = store.WithinTx(ctx, func(ctx context.Context) error {
		closed, err := repos.reserves.ExpireLapsedLeases(ctx, 1)
		if err != nil {
			return err
		}
		if len(closed) != 1 || closed[0].ID != lostVictim {
			return fmt.Errorf("the reaping unit closed %v, want exactly hold %v", closed, lostVictim)
		}
		fact, err := accounting.NewExpired(closed[0].RequestID, nil, time.Now().UTC())
		if err != nil {
			return err
		}
		_, err = repos.facts.Append(ctx, fact)
		return err
	})
	if err != nil {
		t.Fatalf("the reaping unit of work: %v", err)
	}
	if got := integrationFactCount(t, db, lostRequest); got != 1 {
		t.Errorf("the reaped request carries %d facts, want the one expired fact its sweep appended", got)
	}
	if got := integrationReservationState(t, db, lostVictim); got != string(accounting.StateExpired) {
		t.Errorf("the reaped hold = %q, want expired", got)
	}
	if got := integrationReservationState(t, db, fresh); got != string(accounting.StateOpen) {
		t.Errorf("the fresh hold after the reaping unit = %q, want open", got)
	}
	if got := integrationFactCount(t, db, freshRequest); got != 0 {
		t.Errorf("the live hold's request carries %d facts, want 0 — nothing the sweeps did touches it", got)
	}
}

// TestIntegrationIntakeAnswersReplaysFromOneRow races eight admissions of one
// (account, idempotency key) and asserts the engine's last word: the unique
// key admits exactly one row, the seven losers get the sentinel the replay
// path answers from, and the one row carries everything a replay needs — the
// digest it decided on, the request it named, and a pointer that says the
// original has not ended yet. Finalise then writes that pointer once; the
// second finalisation is answered false with no error, and a finalisation of
// a key nobody admitted is the caller's bug, not a no-op.
func TestIntegrationIntakeAnswersReplaysFromOneRow(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	account := integrationRuntimeAccount(t, "intake")
	key := "replay-key"
	digest := "b7it-digest-sha256-abc"
	requestID := identity.NewRequestID()
	intake, err := execution.NewIntake(account, key, digest, requestID, time.Now().UTC())
	if err != nil {
		t.Fatalf("building the replay record: %v", err)
	}

	const arrivals = 8
	type arrival struct {
		err error
	}
	results := make(chan arrival, arrivals)
	var wg sync.WaitGroup
	for i := 0; i < arrivals; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each arrival is its own unit of work: admission's insert rides
			// alone, and the engine's unique key is what turns the eighth
			// copy of the same decision into an answer instead of a row.
			err := store.WithinTx(ctx, func(ctx context.Context) error {
				return repos.intakes.Insert(ctx, intake)
			})
			results <- arrival{err: err}
		}()
	}
	wg.Wait()
	close(results)

	winners := 0
	for result := range results {
		switch {
		case result.err == nil:
			winners++
		case errors.Is(result.err, persistence.ErrDuplicateIntake):
			// The one answer a replayed admission may get: decided, this way,
			// by that original request — go read the row.
		default:
			t.Fatalf("a replayed admission failed with the wrong error: %v", result.err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d admissions of one key succeeded, want exactly 1", winners)
	}

	found, err := repos.intakes.Find(ctx, account, key)
	if err != nil {
		t.Fatalf("Find(the admitted key): %v", err)
	}
	if found.RequestID != requestID || found.RequestDigest != digest {
		t.Errorf("the row answers request %v with digest %q, want %v with %q", found.RequestID, found.RequestDigest, requestID, digest)
	}
	if found.Decided() {
		t.Errorf("Find() reports the record decided before its request finalised — the pointer must still be unset")
	}
	if got := integrationIntakeRow(t, db, account, key); got != 1 {
		t.Errorf("the key carries %d rows, want the one the engine admitted", got)
	}

	decided, err := repos.intakes.Finalise(ctx, account, key, execution.FinalFailed, "", execution.FailedGatewayAbandoned)
	if err != nil || !decided {
		t.Fatalf("Finalise(the pointer) = (%t, %v), want (true, nil)", decided, err)
	}
	again, err := repos.intakes.Finalise(ctx, account, key, execution.FinalSucceeded, "", "")
	if err != nil || again {
		t.Errorf("the second Finalise() = (%t, %v), want (false, nil) — the pointer was written once", again, err)
	}
	found, err = repos.intakes.Find(ctx, account, key)
	if err != nil {
		t.Fatalf("Find(the finalised key): %v", err)
	}
	if !found.Decided() || found.FinalStatus == nil || *found.FinalStatus != execution.FinalFailed || found.FinalFailureReason != execution.FailedGatewayAbandoned {
		t.Errorf("the finalised pointer reads back (%v, %q), want (failed, gateway_abandoned) — the first decision stands", found.FinalStatus, found.FinalFailureReason)
	}

	if _, err := repos.intakes.Find(ctx, account, "nobody-admitted-this"); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("Find(an unadmitted key) error = %v, want persistence.ErrNotFound", err)
	}
	if decided, err := repos.intakes.Finalise(ctx, account, "nobody-admitted-this", execution.FinalSucceeded, "", ""); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("Finalise(an unadmitted key) = (%t, %v), want persistence.ErrNotFound — a pointer with no record is a bug, not a no-op", decided, err)
	}
}

// TestIntegrationSucceededReplayPointerIsWritable asserts the one pointer
// shape the rest of the suite leaves out: a replay record whose original
// request SUCCEEDED. The domain's Finalise(succeeded) refuses any reason, and
// the engine's request_intake_final_succeeded_shape agrees — both reason
// columns NULL. The pointer must therefore be writable with a status and no
// reason at all, which is why request_intake_final_pairing is spelled "a
// reason exists only under a status" and not the reverse: the reverse pairing
// made this shape unwritable, and the succeeded pointer is the one terminal
// fate the replay record most needs to point at.
func TestIntegrationSucceededReplayPointerIsWritable(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	account := integrationRuntimeAccount(t, "intake-succeeded")
	key := "succeeded-key"
	intake, err := execution.NewIntake(account, key, "b7it-digest-sha256-abc", identity.NewRequestID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("building the replay record: %v", err)
	}
	if err := repos.intakes.Insert(ctx, intake); err != nil {
		t.Fatalf("inserting the replay record: %v", err)
	}

	wrote, err := repos.intakes.Finalise(ctx, account, key, execution.FinalSucceeded, "", "")
	if err == nil {
		if !wrote {
			t.Error("Finalise(a succeeded pointer) = false with no error, want true")
		}
		found, err := repos.intakes.Find(ctx, account, key)
		if err != nil {
			t.Fatalf("Find(the succeeded record): %v", err)
		}
		if !found.Decided() || found.FinalStatus == nil || *found.FinalStatus != execution.FinalSucceeded {
			t.Errorf("the succeeded pointer reads back %+v, want succeeded", found.FinalStatus)
		}
		if found.FinalRejectionReason != "" || found.FinalFailureReason != "" {
			t.Errorf("the succeeded pointer carries reasons (%q, %q), want none — succeeded names no reason", found.FinalRejectionReason, found.FinalFailureReason)
		}
		return
	}
	t.Fatalf("Finalise(a succeeded pointer) error = %v, want a write — the pairing constraint admits a status with no reason", err)
}

// TestIntegrationRequestFinaliseIsOnceOnly races eight finalisations of one
// executing request and asserts the CAS story end to end: one winner writes
// succeeded with its committed attempt, seven losers are told false with no
// error — read the winner's decision, never retry it — and any path that
// reaches the terminal row some other way meets the engine's trigger, which
// is the bug detector behind the CAS, not a second authority.
func TestIntegrationRequestFinaliseIsOnceOnly(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	account := integrationRuntimeAccount(t, "finalise")
	request, err := execution.NewRequest(identity.NewRequestID(), account, account+"-api-key", "bench/alias", 64, 128, integrationPrice(), time.Now().UTC())
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	attempt, err := execution.NewAttempt(identity.NewAttemptID(), request.ID, 0, 0, "b7it-backend", "b7it/model", execution.OutcomeSucceeded, "", time.Now().UTC(), time.Now().UTC())
	if err != nil {
		t.Fatalf("building the attempt: %v", err)
	}
	err = store.WithinTx(ctx, func(ctx context.Context) error {
		if err := repos.requests.Insert(ctx, request); err != nil {
			return err
		}
		return repos.attempts.Insert(ctx, attempt)
	})
	if err != nil {
		t.Fatalf("inserting the executing request and its attempt: %v", err)
	}

	// The winning decision, formed once and raced by all eight: succeed, on
	// this attempt, now.
	winner, err := execution.NewRequest(request.ID, request.AccountID, request.APIKeyID, request.Alias, request.InputTokens, request.MaxOutputTokens, request.Price, request.AdmittedAt)
	if err != nil {
		t.Fatalf("re-forming the executing request: %v", err)
	}
	if err := winner.Succeed(attempt.ID, time.Now().UTC()); err != nil {
		t.Fatalf("forming the winning decision: %v", err)
	}

	const closers = 8
	wins := make(chan bool, closers)
	var wg sync.WaitGroup
	for i := 0; i < closers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decided, err := repos.requests.Finalise(ctx, winner)
			if err != nil {
				t.Errorf("Finalise() error = %v, want nil — losing a once-only race is an answer, not a failure", err)
				wins <- false
				return
			}
			wins <- decided
		}()
	}
	wg.Wait()
	close(wins)

	winnersCount := 0
	for decided := range wins {
		if decided {
			winnersCount++
		}
	}
	if winnersCount != 1 {
		t.Errorf("%d of %d finalisations wrote the row, want exactly 1", winnersCount, closers)
	}

	var status string
	var committed sql.NullString
	var finished sql.NullTime
	ctxRow, cancelRow := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRow()
	if err := db.QueryRowContext(ctxRow, `SELECT status, committed_attempt_id::text, finished_at FROM public.requests WHERE id = $1`, string(request.ID)).
		Scan(&status, &committed, &finished); err != nil {
		t.Fatalf("reading the finalised row: %v", err)
	}
	if status != string(execution.StatusSucceeded) {
		t.Errorf("the row's status = %q, want succeeded", status)
	}
	if !committed.Valid || committed.String != string(attempt.ID) {
		t.Errorf("the row commits attempt %v, want %v — the decision names the usage the settlement will carry", committed, attempt.ID)
	}
	if !finished.Valid {
		t.Errorf("the row carries no finished_at — a finalisation without its timestamp is not a finalisation")
	}

	// The engine's half: the row is terminal, and no UPDATE reaches it — not
	// even one that writes the value it already has. The CAS's WHERE clause
	// is what loses the race cleanly; the trigger is what makes every other
	// path loud.
	assertTerminalUpdateRefused(t, db, `UPDATE public.requests SET status = 'failed' WHERE id = $1`, string(request.ID))
}

// TestIntegrationAttemptUsageUpdateKeepsTheFirstReport walks the one
// sanctioned update to an attempt row: a report writes the figures it carries
// and leaves the ones it does not exactly as they were. A later report that
// knows less must not displace the first one's telemetry — COALESCE is the
// whole discipline, and a report about an attempt that does not exist is
// answered false with no error, a miss and not a malfunction.
func TestIntegrationAttemptUsageUpdateKeepsTheFirstReport(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	account := integrationRuntimeAccount(t, "coalesce")
	request, err := execution.NewRequest(identity.NewRequestID(), account, account+"-api-key", "bench/alias", 64, 128, integrationPrice(), time.Now().UTC())
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	attempt, err := execution.NewAttempt(identity.NewAttemptID(), request.ID, 0, 0, "b7it-backend", "b7it/model", execution.OutcomeSucceeded, "", time.Now().UTC(), time.Now().UTC())
	if err != nil {
		t.Fatalf("building the attempt: %v", err)
	}
	// The row is born with the first report's input figure already in it —
	// the insert writes what the completion carried, and every later report
	// arrives through the update.
	attempt.ProviderInputTokens = int64Ptr(5)
	err = store.WithinTx(ctx, func(ctx context.Context) error {
		if err := repos.requests.Insert(ctx, request); err != nil {
			return err
		}
		return repos.attempts.Insert(ctx, attempt)
	})
	if err != nil {
		t.Fatalf("inserting the attempt with its first report: %v", err)
	}

	readFigures := func(where string) (input, output, delivery sql.NullInt64) {
		t.Helper()
		ctxRead, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelRead()
		if err := db.QueryRowContext(ctxRead, `SELECT provider_input_tokens, provider_output_tokens, delivery_tokens FROM public.request_attempts WHERE id = $1`, string(attempt.ID)).
			Scan(&input, &output, &delivery); err != nil {
			t.Fatalf("reading the attempt's figures %s: %v", where, err)
		}
		return input, output, delivery
	}

	written, err := repos.attempts.RecordProviderUsage(ctx, attempt.ID, nil, int64Ptr(7), nil)
	if err != nil || !written {
		t.Fatalf("RecordProviderUsage(output only) = (%t, %v), want (true, nil)", written, err)
	}
	input, output, delivery := readFigures("after the second report")
	if !input.Valid || input.Int64 != 5 {
		t.Errorf("provider_input_tokens = %v, want 5 — the second report knew nothing of input and must leave the first report's figure alone", input)
	}
	if !output.Valid || output.Int64 != 7 {
		t.Errorf("provider_output_tokens = %v, want 7 — the figure the second report carried", output)
	}
	if delivery.Valid {
		t.Errorf("delivery_tokens = %v, want NULL — a report that does not carry a figure does not write one", delivery)
	}

	written, err = repos.attempts.RecordProviderUsage(ctx, attempt.ID, int64Ptr(9), nil, int64Ptr(1))
	if err != nil || !written {
		t.Fatalf("RecordProviderUsage(the third report, which contradicts the first) = (%t, %v), want (true, nil)", written, err)
	}
	input, output, delivery = readFigures("after the third report")
	// A report that CARRIES a figure writes it — the update's contract is
	// "writes the figures it carries", and which claim stands when two
	// reports disagree is the caller's decision, not the store's. What
	// COALESCE protects is the figure the report does NOT carry: that one is
	// never displaced by a NULL.
	if !input.Valid || input.Int64 != 9 {
		t.Errorf("provider_input_tokens = %v, want 9 — the third report carried it, and a carried figure is written", input)
	}
	if !output.Valid || output.Int64 != 7 {
		t.Errorf("provider_output_tokens = %v, want 7 — the third report carried no output figure, and its NULL must not displace the second report's", output)
	}
	if !delivery.Valid || delivery.Int64 != 1 {
		t.Errorf("delivery_tokens = %v, want 1 — the one other figure the third report carried", delivery)
	}

	// The miss: a well-formed identity nobody inserted. An unparseable id
	// would be refused by the driver's uuid cast before the store could
	// answer; a real identity that matches no row is the port's false.
	written, err = repos.attempts.RecordProviderUsage(ctx, identity.AttemptID(identity.NewAttemptID()), int64Ptr(1), int64Ptr(1), int64Ptr(1))
	if err != nil {
		t.Errorf("RecordProviderUsage(a missing attempt) error = %v, want nil", err)
	}
	if written {
		t.Errorf("RecordProviderUsage(a missing attempt) = true, want false — zero rows is a miss, reported as one")
	}
}

// TestIntegrationFinaliseRefusesAnotherRequestsAttempt pins the composite
// foreign key that backs "the decision must carry the attempt it commits": a
// finalisation naming another request's attempt is refused with the domain
// sentinel that exists for exactly this, the abandoned close finalises
// without naming any attempt, and a row born terminal is untouchable — the
// trigger's WHEN clause skips every open-row write and stops exactly the
// terminal ones.
func TestIntegrationFinaliseRefusesAnotherRequestsAttempt(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	now := time.Now().UTC()
	ownerAccount := integrationRuntimeAccount(t, "fk-owner")
	otherAccount := integrationRuntimeAccount(t, "fk-other")

	// Two requests, one attempt each. The attempt belongs to its own request;
	// the composite key is (request_id, attempt_id), so no spelling of
	// "someone else's attempt" fits the constraint.
	owner, err := execution.NewRequest(identity.NewRequestID(), ownerAccount, ownerAccount+"-api-key", "bench/alias", 64, 128, integrationPrice(), now)
	if err != nil {
		t.Fatalf("building the owning request: %v", err)
	}
	other, err := execution.NewRequest(identity.NewRequestID(), otherAccount, otherAccount+"-api-key", "bench/alias", 64, 128, integrationPrice(), now)
	if err != nil {
		t.Fatalf("building the other request: %v", err)
	}
	othersAttempt, err := execution.NewAttempt(identity.NewAttemptID(), other.ID, 0, 0, "b7it-backend", "b7it/model", execution.OutcomeSucceeded, "", now, now)
	if err != nil {
		t.Fatalf("building the other request's attempt: %v", err)
	}
	err = store.WithinTx(ctx, func(ctx context.Context) error {
		if err := repos.requests.Insert(ctx, owner); err != nil {
			return err
		}
		if err := repos.requests.Insert(ctx, other); err != nil {
			return err
		}
		return repos.attempts.Insert(ctx, othersAttempt)
	})
	if err != nil {
		t.Fatalf("inserting the two requests and the other's attempt: %v", err)
	}

	// The owner decides, and decides wrongly: the attempt it names is the
	// other request's. The domain cannot see the database from where it
	// stands — checkFinalisable says so in terms — so the refusal happens
	// here, at the persist, and surfaces as the sentinel the caller maps.
	decided, err := execution.NewRequest(owner.ID, owner.AccountID, owner.APIKeyID, owner.Alias, owner.InputTokens, owner.MaxOutputTokens, owner.Price, owner.AdmittedAt)
	if err != nil {
		t.Fatalf("re-forming the executing owner: %v", err)
	}
	if err := decided.Succeed(othersAttempt.ID, now); err != nil {
		t.Fatalf("forming the wrong decision: %v", err)
	}
	finalised, err := repos.requests.Finalise(ctx, decided)
	if !errors.Is(err, persistence.ErrAttemptNotOfRequest) {
		t.Errorf("Finalise(another request's attempt) error = %v, want persistence.ErrAttemptNotOfRequest", err)
	}
	if finalised {
		t.Errorf("Finalise(another request's attempt) = true, want false — nothing was written")
	}
	if status := integrationRequestStatus(t, db, owner.ID); status != string(execution.StatusExecuting) {
		t.Errorf("the owner's status after the refused decision = %q, want executing — a refused finalisation writes nothing", status)
	}

	// The abandoned close: the reaper's finalisation names no attempt at all,
	// and the pairing CHECK on the row accepts exactly that shape.
	abandoned, err := execution.NewRequest(owner.ID, owner.AccountID, owner.APIKeyID, owner.Alias, owner.InputTokens, owner.MaxOutputTokens, owner.Price, owner.AdmittedAt)
	if err != nil {
		t.Fatalf("re-forming the executing owner: %v", err)
	}
	if err := abandoned.FailAbandoned(now.Add(time.Second)); err != nil {
		t.Fatalf("forming the abandoned close: %v", err)
	}
	finalised, err = repos.requests.Finalise(ctx, abandoned)
	if err != nil || !finalised {
		t.Fatalf("Finalise(the abandoned close) = (%t, %v), want (true, nil)", finalised, err)
	}
	var (
		status    string
		failure   sql.NullString
		committed sql.NullString
	)
	ctxRow, cancelRow := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRow()
	if err := db.QueryRowContext(ctxRow, `SELECT status, failure_reason, committed_attempt_id::text FROM public.requests WHERE id = $1`, string(owner.ID)).
		Scan(&status, &failure, &committed); err != nil {
		t.Fatalf("reading the abandoned row: %v", err)
	}
	if status != string(execution.StatusFailed) || !failure.Valid || failure.String != string(execution.FailedGatewayAbandoned) {
		t.Errorf("the abandoned row = (%s, %v), want (failed, %q)", status, failure, execution.FailedGatewayAbandoned)
	}
	if committed.Valid {
		t.Errorf("the abandoned row commits attempt %v, want NULL — a close the runtime cannot evidence names nobody", committed)
	}

	// Born terminal: rejection rows are written finished, and the trigger's
	// WHEN clause — which fires only on rows ALREADY terminal — must refuse
	// the first UPDATE anyone aims at it, whatever it writes.
	rejected, err := execution.RejectNew(identity.NewRequestID(), ownerAccount, ownerAccount+"-api-key", "bench/alias", execution.RejectedUnknownAlias, now)
	if err != nil {
		t.Fatalf("building the rejection row: %v", err)
	}
	if err := repos.requests.Insert(ctx, rejected); err != nil {
		t.Fatalf("inserting the rejection row: %v", err)
	}
	assertTerminalUpdateRefused(t, db, `UPDATE public.requests SET status = 'executing', rejection_reason = NULL, finished_at = NULL WHERE id = $1`, string(rejected.ID))
	if decided, err := repos.requests.Finalise(ctx, rejected); decided || err != nil {
		t.Errorf("Finalise(a row born terminal) = (%t, %v), want (false, nil) — the CAS matches no rows, the trigger never fires", decided, err)
	}
}

// TestIntegrationReaderDeliversTheStoredBytesAndNeverAliasesThem is the
// reader's byte-level contract: every event's payload is byte for byte what
// the database stores — which is not byte for byte what the writer
// marshalled, jsonb normalising its text form — and is a copy no other event
// and no later read can reach. A reader that aliased a reused scan buffer
// would pass every content check and corrupt the first consumer that held two
// pages at once, so the test holds page one open across a second read and
// writes into one event's buffer to prove the others stand alone.
func TestIntegrationReaderDeliversTheStoredBytesAndNeverAliasesThem(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	reader := NewUsageFacts(db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	account := integrationRuntimeAccount(t, "reader")
	// The position before any of this test's facts: the reads below must
	// deliver this test's appends and nothing from any earlier run.
	before := integrationStreamCursor(t, db)

	// Three facts with three leg shapes — none, one, two legs — so the
	// payloads are distinct JSON documents of distinct lengths, which is what
	// makes an aliasing bug visible rather than coincidental.
	first, factOne, seqOne := integrationAppendSettled(t, ctx, repos, account, nil, time.Now().UTC().Add(-3*time.Hour))
	second, factTwo, seqTwo := integrationAppendSettled(t, ctx, repos, account,
		[]accounting.AllocationLeg{{FundingBucketID: account + "-leg-a", Amount: 10, Ordinal: 1}},
		time.Now().UTC().Add(-2*time.Hour))
	third, factThree, seqThree := integrationAppendSettled(t, ctx, repos, account,
		[]accounting.AllocationLeg{
			{FundingBucketID: account + "-leg-b", Amount: 5, Ordinal: 1},
			{FundingBucketID: account + "-leg-c", Amount: 7, Ordinal: 2},
		},
		time.Now().UTC().Add(-time.Hour))

	// The append order is the sequence order, monotonic across the three —
	// the pages below are only as meaningful as that.
	if !(seqOne < seqTwo && seqTwo < seqThree) {
		t.Fatalf("sequences %d, %d, %d are not the append order", seqOne, seqTwo, seqThree)
	}

	assertStoredBytes := func(stage string, events []usagefacts.Event, facts map[identity.RequestID]accounting.Fact) {
		t.Helper()
		for _, event := range events {
			fact := facts[identity.RequestID(event.RequestID)]
			stored := integrationStoredPayload(t, db, identity.RequestID(event.RequestID))
			// Byte contract, against the stored form: the jsonb text
			// normalisation is the database's business, and what came out is
			// what is in there.
			if !bytes.Equal(event.Payload, stored) {
				t.Errorf("%s: event for %s carries %s, want the stored bytes %s", stage, event.RequestID, event.Payload, stored)
			}
			// Content contract, against the writer's bytes: the normalised
			// form and the marshalled one must be the same document, whatever
			// their spelling.
			var read, written any
			if err := json.Unmarshal(event.Payload, &read); err != nil {
				t.Fatalf("%s: the event's payload is not json: %v", stage, err)
			}
			if err := json.Unmarshal([]byte(fact.Payload), &written); err != nil {
				t.Fatalf("%s: the writer's payload is not json: %v", stage, err)
			}
			if !reflect.DeepEqual(read, written) {
				t.Errorf("%s: event payload %s decodes differently from the writer's %s", stage, event.Payload, fact.Payload)
			}
		}
	}

	pageOne, err := reader.Read(ctx, before, 2)
	if err != nil {
		t.Fatalf("Read(the first page): %v", err)
	}
	if len(pageOne.Events) != 2 || !pageOne.HasMore {
		t.Fatalf("the first page carried %d events (HasMore %t), want 2 and true", len(pageOne.Events), pageOne.HasMore)
	}
	if pageOne.Events[0].RequestID != string(first) || pageOne.Events[1].RequestID != string(second) {
		t.Fatalf("the first page delivered [%s, %s], want [%s, %s] in append order", pageOne.Events[0].RequestID, pageOne.Events[1].RequestID, first, second)
	}
	pageOneFacts := map[identity.RequestID]accounting.Fact{first: factOne, second: factTwo}
	assertStoredBytes("page one", pageOne.Events, pageOneFacts)

	// A second page while the first is still held — the exact shape a
	// buffer-aliasing bug would corrupt.
	fourth, factFour, _ := integrationAppendSettled(t, ctx, repos, account, nil, time.Now().UTC())
	fifth, factFive, _ := integrationAppendSettled(t, ctx, repos, account,
		[]accounting.AllocationLeg{{FundingBucketID: account + "-leg-d", Amount: 3, Ordinal: 1}},
		time.Now().UTC())
	pageTwo, err := reader.Read(ctx, pageOne.NextCursor, 2)
	if err != nil {
		t.Fatalf("Read(the second page): %v", err)
	}
	if len(pageTwo.Events) != 2 || !pageTwo.HasMore {
		t.Fatalf("the second page carried %d events (HasMore %t), want the third and fourth facts and true", len(pageTwo.Events), pageTwo.HasMore)
	}
	if pageTwo.Events[0].RequestID != string(third) || pageTwo.Events[1].RequestID != string(fourth) {
		t.Fatalf("the second page delivered [%s, %s], want [%s, %s] in append order", pageTwo.Events[0].RequestID, pageTwo.Events[1].RequestID, third, fourth)
	}
	pageTwoFacts := map[identity.RequestID]accounting.Fact{third: factThree, fourth: factFour}
	assertStoredBytes("page two", pageTwo.Events, pageTwoFacts)

	// The tail: one fact left, the cursor the second page minted resumes
	// exactly there, and the page that delivers nothing still names a
	// position to resume from.
	tail, err := reader.Read(ctx, pageTwo.NextCursor, 2)
	if err != nil {
		t.Fatalf("Read(the tail): %v", err)
	}
	if len(tail.Events) != 1 || tail.HasMore {
		t.Fatalf("the tail carried %d events (HasMore %t), want the fifth fact and false", len(tail.Events), tail.HasMore)
	}
	if tail.Events[0].RequestID != string(fifth) {
		t.Errorf("the tail delivered %s, want %s", tail.Events[0].RequestID, fifth)
	}
	assertStoredBytes("the tail", tail.Events, map[identity.RequestID]accounting.Fact{fifth: factFive})
	if _, err := reader.Read(ctx, tail.NextCursor, 2); err != nil {
		t.Errorf("Read(resuming from the tail's cursor): %v", err)
	}

	// Page one, still held, must be untouched by the second read: same bytes
	// as the database stores now.
	assertStoredBytes("page one after the second read", pageOne.Events, pageOneFacts)

	// And the events of one page share no backing bytes: a consumer writing
	// into one payload must not reach its neighbours. The write is the point
	// — a copy per event survives it, a shared scan buffer does not.
	if len(pageOne.Events[0].Payload) == 0 || len(pageOne.Events[1].Payload) == 0 {
		t.Fatalf("page one carries empty payloads; the aliasing probe needs bytes to write into")
	}
	probe := pageOne.Events[0].Payload[0]
	pageOne.Events[0].Payload[0] = ' '
	if !bytes.Equal(pageOne.Events[1].Payload, integrationStoredPayload(t, db, second)) {
		t.Errorf("writing into one event's payload disturbed its neighbour — the events alias one buffer")
	}
	if !bytes.Equal(pageOne.Events[0].Payload, replaceFirstByte(integrationStoredPayload(t, db, first), ' ')) {
		t.Errorf("the written event did not keep the write — the probe tested nothing: payload was %q, want the stored bytes with byte %d replaced", pageOne.Events[0].Payload, probe)
	}
}

// replaceFirstByte is the aliasing probe's expected value: the stored payload
// with its first byte replaced, which is exactly what a one-byte write into an
// unaliased copy produces.
func replaceFirstByte(payload []byte, replacement byte) []byte {
	out := append([]byte(nil), payload...)
	out[0] = replacement
	return out
}

// TestIntegrationReaderReportsAnUnavailableSource drives the reader's failure
// mapping from the query side: a database that cannot answer is the port's
// ErrSourceUnavailable — the retriable internal failure — and never
// ErrCursorExpired, which is a verdict about the caller's position, not about
// the store's health. The undecodable-row half of the mapping is deliberately
// not manufactured here: against real jsonb a stored payload cannot be
// invalid JSON, because the database refuses such an insert outright — the
// scan phase's json.Valid is drift armour for a corrupted store, not a path
// this suite can reach without corrupting the fixture it shares.
func TestIntegrationReaderReportsAnUnavailableSource(t *testing.T) {
	db, _ := integrationPool(t)
	integrationRuntimeSchema(t, db)
	reader := NewUsageFacts(db)

	// Close the pool before the read: every connection the reader could take
	// is gone, which is the query-phase failure — the database is there, the
	// runtime's route to it is not.
	if err := db.Close(); err != nil {
		t.Fatalf("closing the pool: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The genesis position: a well-formed cursor, so the refusal below cannot
	// be the cursor decoder's answer arriving ahead of the query.
	_, err := reader.Read(ctx, genesisCursor, 5)
	if err == nil {
		t.Fatalf("Read(against a closed pool) error = nil, want a refusal")
	}
	if !errors.Is(err, usagefacts.ErrSourceUnavailable) {
		t.Errorf("Read(against a closed pool) error = %v, want it to wrap usagefacts.ErrSourceUnavailable", err)
	}
	if errors.Is(err, usagefacts.ErrCursorExpired) {
		t.Errorf("Read(against a closed pool) error = %v, want nothing of the cursor vocabulary — a broken source is not an expired position", err)
	}
}
