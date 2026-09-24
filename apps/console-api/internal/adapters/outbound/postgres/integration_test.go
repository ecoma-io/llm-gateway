//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// Integration tests deliberately require an explicit server rather than
// starting Docker from go test: process tests that own a Docker socket are
// brittle (pull latency, daemon permissions, port races) and conceal which
// server they actually exercised. `deploy/postgres/compose.yaml` provides the
// one pinned disposable cluster; its README has the exact commands.
//
// Run (from the repository root):
//
//	docker compose -f deploy/postgres/compose.yaml up -d --wait
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:5432/postgres?sslmode=disable' \
//		go test -tags=integration ./internal/adapters/outbound/postgres
//
// The variable names an ADMIN connection — a URL to the cluster's `postgres`
// maintenance database, whose role may create databases. The plane database
// every test below actually runs against is derived from it by swapping the
// path, so one variable moves the whole suite to another cluster unchanged:
// CI's admin DSN names a superuser, the local fixture's names `gateway`, and
// this file does not care which.

// adminDSN returns the environment's admin DSN, failing the test with the fix
// when the variable is absent — the same contract the moon test-integration
// gate states before go test is even invoked.
func adminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_ADMIN_DSN")
	if dsn == "" {
		t.Fatal("POSTGRES_TEST_ADMIN_DSN is required for integration tests; start deploy/postgres first (from the repository root: docker compose -f deploy/postgres/compose.yaml up -d --wait) and set it to postgres://gateway:gateway-dev-only@127.0.0.1:5432/postgres?sslmode=disable")
	}
	return dsn
}

// planeDSN ensures the plane's database exists on the target cluster and
// returns the DSN for it: the admin URL with the path swapped to the plane
// database, query string preserved (net/url, so sslmode rides along).
//
// The existence check and the CREATE run on a raw database/sql pool rather
// than through Open, and that is deliberate: Open is the startup contract
// under test — it refuses any database but this plane's — while this helper's
// job is to reach the maintenance database that contract is derived from.
// The CREATE DATABASE is the fixture's fallback, not its normal path: both
// plane databases are built by deploy/postgres's initdb on an empty volume.
// A role without CREATEDB makes the fallback fail loudly; the fix is creating
// the database once by hand (the command is in the failure message), and the
// suite then only ever verifies.
func planeDSN(t *testing.T) string {
	t.Helper()

	admin, err := sql.Open("pgx", adminDSN(t))
	if err != nil {
		t.Fatalf("sql.Open on the admin DSN: %v", err)
	}
	defer func() { _ = admin.Close() }()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping on the admin DSN: %v", err)
	}

	var present bool
	switch err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, ownedDatabase).Scan(&present); {
	case err != nil:
		t.Fatalf("pg_database lookup for %q: %v", ownedDatabase, err)
	case !present:
		if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+ownedDatabase); err != nil {
			t.Fatalf("CREATE DATABASE %s: %v (does the admin role have CREATEDB? create it once by hand: docker compose -f deploy/postgres/compose.yaml exec postgres psql -U gateway -d postgres -c 'CREATE DATABASE %s')", ownedDatabase, err, ownedDatabase)
		}
	}

	parsed, err := url.Parse(adminDSN(t))
	if err != nil {
		t.Fatalf("POSTGRES_TEST_ADMIN_DSN: %v", err)
	}
	parsed.Path = "/" + ownedDatabase
	return parsed.String()
}

// integrationPool dials the plane database through the adapter's own Open —
// the same call the process makes at startup, plane binding included — and
// closes the pool at cleanup. The store is handed back beside the pool
// because the port is what the behaviour tests speak through.
func integrationPool(t *testing.T) (*sql.DB, persistence.Store) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	db, err := Open(ctx, Options{
		DSN:             planeDSN(t),
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: time.Minute,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, New(db)
}

// integrationProbeTable is the one object the transaction tests write through:
// a plain table in the plane database, created IF NOT EXISTS by each test that
// uses it and dropped by that test's cleanup — the same nature as the DDL
// probes in deploy/postgres/verify.sh, and no more schema than that. The
// Control Plane's migration lane owns real schema; nothing here pretends to.
const integrationProbeTable = "public.integration_tx_probe_control_api"

// ensureProbeTable creates the probe table through the pool, outside any unit
// of work, and empties it, so every transaction test starts from the same
// blank state and none depends on another having cleaned up; dropProbeTable
// removes it again. Both take context.Background rather than t.Context on
// purpose: dropProbeTable runs from t.Cleanup, by which point t.Context has
// been cancelled, and a cleanup that cannot reach the database is a cleanup
// that proves nothing.
func ensureProbeTable(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+integrationProbeTable+" (id int)"); err != nil {
		t.Fatalf("CREATE TABLE IF NOT EXISTS %s: %v", integrationProbeTable, err)
	}
	if _, err := db.ExecContext(ctx, "TRUNCATE "+integrationProbeTable); err != nil {
		t.Fatalf("TRUNCATE %s: %v", integrationProbeTable, err)
	}
}

func dropProbeTable(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+integrationProbeTable); err != nil {
		t.Errorf("DROP TABLE IF EXISTS %s: %v", integrationProbeTable, err)
	}
}

func countProbeRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+integrationProbeTable).Scan(&count); err != nil {
		t.Fatalf("count(*) on %s: %v", integrationProbeTable, err)
	}
	return count
}

func TestIntegrationOpenServesThePlaneDatabaseThroughThePort(t *testing.T) {
	_, store := integrationPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// current_database() is the answer that cannot be faked by a pool
	// pointed somewhere else, and it is asked through the store — the port,
	// not the pool — so the whole path is the one application code takes.
	var database string
	if err := store.Querier(ctx).QueryRowContext(ctx, "SELECT current_database()").Scan(&database); err != nil {
		t.Fatalf("current_database() through the port: %v", err)
	}
	if database != ownedDatabase {
		t.Errorf("current_database() = %q, want %q — the port answered, but not from the database this plane owns", database, ownedDatabase)
	}
}

func TestIntegrationOpenRefusesTheOtherPlanesDatabase(t *testing.T) {
	parsed, err := url.Parse(adminDSN(t))
	if err != nil {
		t.Fatalf("POSTGRES_TEST_ADMIN_DSN: %v", err)
	}
	password, _ := parsed.User.Password()
	parsed.Path = "/postgres"

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	db, err := Open(ctx, Options{
		DSN:             parsed.String(),
		MaxOpenConns:    2,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
	})
	if err == nil {
		_ = db.Close()
		t.Fatal("Open() against the postgres maintenance database error = nil, want the plane-binding refusal")
	}
	if !strings.Contains(err.Error(), "owns only the control database") {
		t.Errorf("Open() error = %q, want it to state the ownership boundary", err)
	}
	if password != "" && strings.Contains(err.Error(), password) {
		t.Errorf("Open() error = %q, must not carry the DSN's password", err)
	}
}

func TestIntegrationWithinTxCommitsAndASecondPoolSeesTheRow(t *testing.T) {
	db, store := integrationPool(t)
	ensureProbeTable(t, db)
	t.Cleanup(func() { dropProbeTable(t, db) })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := store.WithinTx(ctx, func(ctx context.Context) error {
		_, err := store.Querier(ctx).ExecContext(ctx, "INSERT INTO "+integrationProbeTable+" (id) VALUES (1)")
		return err
	})
	if err != nil {
		t.Fatalf("WithinTx: %v", err)
	}

	// A second pool, dialled after the commit, is the proof the commit
	// reached the server: it shares nothing with the first but the database,
	// so what it reads is what the server kept.
	secondDB, _ := integrationPool(t)
	if got := countProbeRows(t, secondDB); got != 1 {
		t.Errorf("count(*) through a second pool = %d, want 1 — the committed unit of work must survive and be visible", got)
	}
}

func TestIntegrationWithinTxRollsBackWhenTheWorkFails(t *testing.T) {
	db, store := integrationPool(t)
	ensureProbeTable(t, db)
	t.Cleanup(func() { dropProbeTable(t, db) })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	boom := errors.New("the unit of work failed")
	err := store.WithinTx(ctx, func(ctx context.Context) error {
		if _, err := store.Querier(ctx).ExecContext(ctx, "INSERT INTO "+integrationProbeTable+" (id) VALUES (1)"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithinTx returned %v, want the unit's own error handed back unchanged", err)
	}
	if got := countProbeRows(t, db); got != 0 {
		t.Errorf("count(*) after the failed unit = %d, want 0 — the insert must not have survived its own rollback", got)
	}
}

func TestIntegrationWithinTxRollsBackWhenTheWorkPanics(t *testing.T) {
	db, store := integrationPool(t)
	ensureProbeTable(t, db)
	t.Cleanup(func() { dropProbeTable(t, db) })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	panicked := false
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("WithinTx swallowed the panic — the caller must see it, not the pool")
			}
			panicked = true
		}()
		_ = store.WithinTx(ctx, func(ctx context.Context) error {
			if _, err := store.Querier(ctx).ExecContext(ctx, "INSERT INTO "+integrationProbeTable+" (id) VALUES (1)"); err != nil {
				return err
			}
			panic("mid-work panic")
		})
	}()
	if !panicked {
		t.Fatal("the panic guard never ran")
	}
	if got := countProbeRows(t, db); got != 0 {
		t.Errorf("count(*) after the panicked unit = %d, want 0 — the insert must not have survived the rollback the panic triggered", got)
	}
}

func TestIntegrationWithinTxCancelledMidwayRollsBackAndLeavesThePoolHealthy(t *testing.T) {
	db, store := integrationPool(t)

	// pg_sleep(5) against a 100ms context: the server is genuinely asked to
	// cancel, the error is the context's own, and the return is prompt — the
	// five seconds must not be waited out.
	txCtx, cancelTx := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelTx()
	start := time.Now()
	err := store.WithinTx(txCtx, func(ctx context.Context) error {
		_, err := store.Querier(ctx).ExecContext(ctx, "SELECT pg_sleep(5)")
		return err
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("WithinTx error = nil, want the cancelled query's error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WithinTx error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("WithinTx returned after %s, want a prompt cancellation, not a waited-out pg_sleep", elapsed)
	}

	// The pool did nothing wrong: its next caller is answered, on a fresh
	// context, by a connection the cancelled transaction did not poison.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("Ping after a cancelled query: %v", err)
	}
	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1 after a cancelled query: %v", err)
	} else if one != 1 {
		t.Errorf("SELECT 1 = %d, want 1", one)
	}
}

// TestIntegrationPoolClosesIdempotentlyAndRefusesFurtherQueries pins the
// lifecycle contract the process relies on at shutdown: Close is idempotent,
// and a query through a closed pool fails with the standard library's own
// closed answer rather than a hang or a panic.
//
// There is no goroutine-leak assertion here on purpose: NumGoroutine deltas
// are flaky in shared CI, where unrelated finalizers and pool janitors move
// the count for free, so the contract is pinned by close-safety and by
// ErrConnDone — the observables, and the ones that do not flake.
func TestIntegrationPoolClosesIdempotentlyAndRefusesFurtherQueries(t *testing.T) {
	db, _ := integrationPool(t)

	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want an idempotent no-op", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	// A closed pool refuses work with the standard library's own closed
	// answer — errDBClosed, spelled "sql: database is closed". That sentinel
	// is unexported, so the exact text is what this asserts; sql.ErrConnDone
	// is the neighbouring contract of a closed checked-out *sql.Conn, not of
	// a closed pool, and would make this assertion pass for the wrong reason
	// (a nil comparison) or fail outright.
	if _, err := db.QueryContext(ctx, "SELECT 1"); err == nil || err.Error() != "sql: database is closed" {
		t.Errorf("QueryContext after Close error = %v, want \"sql: database is closed\"", err)
	}
}

// TestIntegrationConnMaxIdleTimeRetiresIdleConnections pins the one pool
// setting the fake-driver suite cannot witness: database/sql enforces
// ConnMaxIdleTime on its connection cleaner, whose first pass is never sooner
// than one second — real time, which only this suite spends. The pool's own
// MaxIdleTimeClosed counter is the proof a connection was retired for idleness
// and not discarded some other way.
func TestIntegrationConnMaxIdleTimeRetiresIdleConnections(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	db, err := Open(ctx, Options{
		DSN:             planeDSN(t),
		MaxOpenConns:    2,
		MaxIdleConns:    2,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatalf("ExecContext: %v", err)
	}
	// The cleaner's first pass comes at ~1s (its minimum interval), so the
	// counter is polled rather than assumed — but it must move within the
	// context above, or the setting did not reach the pool.
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if stats := db.Stats(); stats.MaxIdleTimeClosed == 1 && stats.Idle == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the idle connection was never retired for its idle time: stats = %+v, want MaxIdleTimeClosed=1 and Idle=0", db.Stats())
}

func TestIntegrationOpenAgainstADeadPortFailsFastWithoutLeakingTheDSN(t *testing.T) {
	// A closed loopback port answers with a refusal, so Open's startup
	// validation must come back promptly with the failure named — and with
	// nothing of the DSN in it, which is the assertion the synthetic
	// password makes possible.
	dsn := "postgres://gateway:suite-only-password@127.0.0.1:1/control?sslmode=disable"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	db, err := Open(ctx, Options{
		DSN:             dsn,
		MaxOpenConns:    2,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
	})
	elapsed := time.Since(start)
	if err == nil {
		_ = db.Close()
		t.Fatal("Open() against a closed port error = nil, want a startup failure")
	}
	if elapsed >= 3*time.Second {
		t.Errorf("Open() returned after %s, want a prompt failure against a port that refuses, not a hang on the context", elapsed)
	}
	if !strings.Contains(err.Error(), "postgres: connect") {
		t.Errorf("Open() error = %q, want it to name the connection that failed startup validation", err)
	}
	if strings.Contains(err.Error(), "suite-only-password") || strings.Contains(err.Error(), dsn) {
		t.Errorf("Open() error = %q, must not carry the DSN or its credential", err)
	}
}
