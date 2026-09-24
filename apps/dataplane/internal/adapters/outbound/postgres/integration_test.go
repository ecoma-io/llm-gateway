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

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// Integration tests deliberately require an explicit server rather than
// starting Docker from go test: process tests that own a Docker socket are
// brittle (pull latency, daemon permissions, port races) and conceal which
// server they actually exercised. `deploy/postgres/compose.yaml` provides the
// one pinned disposable cluster; its README has the exact commands.
//
// The variable is an *admin* DSN rather than the plane's own, because the
// suite derives the `dataplane` database from it — creating that database when
// it is missing, so a fresh cluster needs no manual step. CI's admin DSN
// authenticates as a role that may create databases; the local fixture's
// `gateway` role may too.
//
// Run (from the repository root):
//
//	docker compose -f deploy/postgres/compose.yaml up -d --wait
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:5432/postgres?sslmode=disable' \
//		go test -v -tags=integration ./internal/adapters/outbound/postgres
//
// The unit suite beside this file pins the adapter's orchestration against the
// hand-written driver fake; what these tests prove is the other half — that
// the orchestration is right about a real PostgreSQL, which no fake can say.
func integrationAdminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_ADMIN_DSN")
	if dsn == "" {
		t.Fatal("POSTGRES_TEST_ADMIN_DSN is required for integration tests; start the fixture (from the repository root: docker compose -f deploy/postgres/compose.yaml up -d --wait) and set it to postgres://gateway:gateway-dev-only@127.0.0.1:5432/postgres?sslmode=disable (see deploy/postgres/README.md)")
	}
	return dsn
}

// integrationPlaneDSN returns the DSN of the database this application owns,
// derived from the admin DSN by swapping its path — its query string and
// credentials ride along unchanged. The database is created first if the
// cluster does not have it, because a suite that needs a manual step before
// its first run is a suite that does not run.
//
// The connection that does the ensuring is opened on the driver directly, not
// through Open: Open refuses any DSN not naming `dataplane`, and the admin DSN
// names the cluster's maintenance database by design. That refusal is the
// adapter doing its job, not a gap in the helper.
func integrationPlaneDSN(t *testing.T) string {
	t.Helper()
	admin, err := url.Parse(integrationAdminDSN(t))
	if err != nil {
		t.Fatalf("POSTGRES_TEST_ADMIN_DSN is not a parsable URL: %v", err)
	}

	adminDB, err := sql.Open(driverName, admin.String())
	if err != nil {
		t.Fatalf("sql.Open on the admin DSN: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
			t.Fatalf("CREATE DATABASE %s: %v — CI's admin DSN uses a role that may create databases; locally: docker compose -f deploy/postgres/compose.yaml exec postgres psql -U gateway -d postgres -c 'CREATE DATABASE %s'", ownedDatabase, err, ownedDatabase)
		}
	}

	plane := *admin
	plane.Path = "/" + ownedDatabase
	return plane.String()
}

// integrationPool opens and pings a pool on the plane database the way the
// composition root does, and hands back both handles the tests need: the pool
// to run probes through, and the store under test.
func integrationPool(t *testing.T) (*sql.DB, persistence.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	db, err := Open(ctx, Options{
		DSN:             integrationPlaneDSN(t),
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

// integrationProbe creates the transaction probe table, empties it, and drops
// it at cleanup. The table is a test probe of the same nature as the DDL
// probes in deploy/postgres/verify.sh — permanent, idempotent, empty of
// anything but probe rows, and never part of the database's schema:
// migrations own that, and this suite is not one. The TRUNCATE is what makes
// the table idempotent for concurrent runs and for the run after an aborted
// one: a commit this suite leaves behind on a killed run is not allowed to
// fail the next run's duplicate-key insert.
func integrationProbe(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	const probe = "CREATE TABLE IF NOT EXISTS public.integration_tx_probe_dataplane (id integer NOT NULL PRIMARY KEY)"
	if _, err := db.ExecContext(ctx, probe); err != nil {
		t.Fatalf("creating the probe table: %v", err)
	}
	if _, err := db.ExecContext(ctx, "TRUNCATE public.integration_tx_probe_dataplane"); err != nil {
		t.Fatalf("TRUNCATE public.integration_tx_probe_dataplane: %v", err)
	}
	dropCtx, cancelDrop := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(func() {
		defer cancelDrop()
		_, _ = db.ExecContext(dropCtx, "DROP TABLE IF EXISTS public.integration_tx_probe_dataplane")
	})
}

// probeRowCount counts the probe rows carrying one id. Each test claims its
// own id, so tests running against one shared probe table never read another
// test's rows.
func probeRowCount(t *testing.T, db *sql.DB, id int) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM public.integration_tx_probe_dataplane WHERE id = $1", id).Scan(&count); err != nil {
		t.Fatalf("counting probe row %d: %v", id, err)
	}
	return count
}

func TestIntegrationOpenServesThePlaneDatabase(t *testing.T) {
	_, store := integrationPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := store.Ping(ctx); err != nil {
		t.Fatalf("Ping() through the store error = %v", err)
	}
	var name string
	if err := store.Querier(ctx).QueryRowContext(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatalf("SELECT current_database(): %v", err)
	}
	if name != ownedDatabase {
		t.Errorf("current_database() = %q, want %q — the pool must point at the database this application owns", name, ownedDatabase)
	}
}

func TestIntegrationRefusesToStartAgainstAnotherPlanesDatabase(t *testing.T) {
	admin, err := url.Parse(integrationAdminDSN(t))
	if err != nil {
		t.Fatalf("POSTGRES_TEST_ADMIN_DSN is not a parsable URL: %v", err)
	}
	// `postgres` is the cluster's always-present maintenance database: a DSN
	// naming it is a DSN a real deployment could hand over by mistake, and the
	// server would answer it. Open must refuse it anyway — the plane binding
	// is not a reachability question but an ownership one, and the refusal
	// happens before the driver dials anything.
	foreign := *admin
	foreign.Path = "/postgres"
	foreignDSN := foreign.String()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	db, err := Open(ctx, Options{
		DSN:             foreignDSN,
		MaxOpenConns:    2,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
	})
	if err == nil {
		_ = db.Close()
		t.Fatal("Open() on the postgres database error = nil, want an ownership refusal")
	}
	if !strings.Contains(err.Error(), "owns only the dataplane database") {
		t.Errorf("Open() error = %q, want the ownership boundary named", err)
	}
	if strings.Contains(err.Error(), foreignDSN) {
		t.Errorf("Open() error = %q, want it to carry no DSN — its userinfo is a credential", err)
	}
}

func TestIntegrationWithinTxCommitsWhenTheWorkSucceeds(t *testing.T) {
	db, store := integrationPool(t)
	integrationProbe(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	const probeID = 301
	err := store.WithinTx(ctx, func(ctx context.Context) error {
		_, err := store.Querier(ctx).ExecContext(ctx, "INSERT INTO public.integration_tx_probe_dataplane (id) VALUES ($1)", probeID)
		return err
	})
	if err != nil {
		t.Fatalf("WithinTx() error = %v", err)
	}
	if got := probeRowCount(t, db, probeID); got != 1 {
		t.Errorf("probe rows after commit = %d, want 1 — a committed unit of work must be visible to a second read", got)
	}
}

func TestIntegrationWithinTxRollsBackWhenTheWorkFails(t *testing.T) {
	db, store := integrationPool(t)
	integrationProbe(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	boom := errors.New("unit of work failed")
	const probeID = 401
	err := store.WithinTx(ctx, func(ctx context.Context) error {
		if _, err := store.Querier(ctx).ExecContext(ctx, "INSERT INTO public.integration_tx_probe_dataplane (id) VALUES ($1)", probeID); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithinTx() error = %v, want the unit's own error handed back", err)
	}
	if got := probeRowCount(t, db, probeID); got != 0 {
		t.Errorf("probe rows after rollback = %d, want 0 — a failed unit of work must leave nothing behind", got)
	}
}

func TestIntegrationWithinTxRollsBackWhenTheWorkPanics(t *testing.T) {
	db, store := integrationPool(t)
	integrationProbe(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	const probeID = 501
	panicked := false
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("WithinTx swallowed the panic — the caller must see it, not the pool")
			}
			panicked = true
		}()
		_ = store.WithinTx(ctx, func(ctx context.Context) error {
			if _, err := store.Querier(ctx).ExecContext(ctx, "INSERT INTO public.integration_tx_probe_dataplane (id) VALUES ($1)", probeID); err != nil {
				return err
			}
			panic("mid-work panic")
		})
	}()
	if !panicked {
		t.Fatal("the panic guard never ran")
	}
	if got := probeRowCount(t, db, probeID); got != 0 {
		t.Errorf("probe rows after the panic = %d, want 0 — a panic must leave the transaction rolled back", got)
	}
}

func TestIntegrationCancellationMidTransactionLeavesThePoolHealthy(t *testing.T) {
	_, store := integrationPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// A statement that would sleep five seconds, given a context that gives
	// up at a hundred milliseconds: the caller must get its context error
	// back promptly — not after the sleep — and the pool must answer
	// afterwards, because a runtime that cancelled one slow query has not
	// lost its database.
	sleepCtx, cancelSleep := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancelSleep()
	started := time.Now()
	err := store.WithinTx(sleepCtx, func(ctx context.Context) error {
		_, err := store.Querier(ctx).ExecContext(ctx, "SELECT pg_sleep(5)")
		return err
	})
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WithinTx() error = %v, want the context's deadline", err)
	}
	if elapsed >= 4*time.Second {
		t.Errorf("WithinTx() returned after %s, want a prompt failure — the cancellation must reach the query, not wait out the sleep", elapsed)
	}

	if err := store.Ping(ctx); err != nil {
		t.Fatalf("Ping() after a cancelled statement error = %v, want a healthy pool", err)
	}
	var one int
	if err := store.Querier(ctx).QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1 after a cancelled statement: %v", err)
	}
	if one != 1 {
		t.Errorf("SELECT 1 = %d, want 1", one)
	}
}

func TestIntegrationPoolCloseIsSafeAndFinal(t *testing.T) {
	// No goroutine-leak assertion here, on purpose: counting goroutines
	// against a real server in CI is flaky in both directions, and the
	// contract a leak check would guard is pinned by the assertions this test
	// already makes — a pool that closes cleanly, twice, and refuses work
	// afterwards is a pool that let go of everything it held.
	db, _ := integrationPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("second Close() error = %v, want nil — Close is safe to repeat", err)
	}
	if _, err := db.QueryContext(ctx, "SELECT 1"); err == nil {
		t.Fatal("QueryContext() after Close() error = nil, want a closed-pool error")
	} else {
		// The standard library's sentinel for a closed pool is the
		// unexported errDBClosed ("sql: database is closed"); sql.ErrConnDone
		// belongs to reusing a single Conn or Tx, which is not this call's
		// path. The text is what pins it.
		if !strings.Contains(err.Error(), "database is closed") {
			t.Errorf("QueryContext() after Close() error = %v, want the closed-pool error", err)
		}
	}
}

func TestIntegrationStartupValidationFailsFastAgainstAClosedPort(t *testing.T) {
	admin, err := url.Parse(integrationAdminDSN(t))
	if err != nil {
		t.Fatalf("POSTGRES_TEST_ADMIN_DSN is not a parsable URL: %v", err)
	}
	// Port 1 on the loopback is the cheap way to name a server that is not
	// there: the connection is refused immediately, which is exactly the
	// condition startup validation exists to catch.
	closed := *admin
	closed.Host = "127.0.0.1:1"
	closed.Path = "/" + ownedDatabase
	closedDSN := closed.String()
	credential, _ := admin.User.Password()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	started := time.Now()
	db, err := Open(ctx, Options{
		DSN:             closedDSN,
		MaxOpenConns:    2,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
	})
	elapsed := time.Since(started)
	if err == nil {
		_ = db.Close()
		t.Fatal("Open() against a closed port error = nil, want a startup failure")
	}
	if elapsed >= 2*time.Second {
		t.Errorf("Open() returned after %s, want a fast failure — an unreachable database must not be waited out at boot", elapsed)
	}
	if !strings.Contains(err.Error(), "ping") {
		t.Errorf("Open() error = %q, want it wrapped as the open's ping failure", err)
	}
	// The credential half needs a credential to look for: an admin DSN
	// without a password (trust auth, a URL with no userinfo) yields an
	// empty string, and Contains(anything, "") is always true.
	if strings.Contains(err.Error(), closedDSN) || (credential != "" && strings.Contains(err.Error(), credential)) {
		t.Errorf("Open() error = %q, want it to carry neither the DSN nor its credential", err)
	}
}
