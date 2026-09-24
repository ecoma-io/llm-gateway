package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// Every test here pins one clause of the port's contract, phrased in
// storage.go: what commits, what rolls back, what joins, which pool a join
// belongs to, what a repository's Querier resolves to, and what a caller is
// told when the driver refuses. The driver is the fake in
// fakedriver_test.go; what these tests prove is the adapter's orchestration,
// not PostgreSQL's — that belongs to the suite in deploy/postgres.

func TestNewHandsBackThePortNotTheAdapter(t *testing.T) {
	_, db := registerFake(t)

	// New's return type is the port, so `store` here is a persistence.Store by
	// construction and application code never names the adapter — everything
	// else this file pins runs through exactly this shape.
	store := New(db)
	if err := store.Ping(context.Background()); err != nil {
		t.Fatalf("ping through the port: %v", err)
	}
}

func TestPingSucceedsWhileTheDriverAnswers(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)

	if err := store.Ping(context.Background()); err != nil {
		t.Fatalf("Ping through an answering driver: %v", err)
	}
	if got := of(f.recorded(), evPing); len(got) != 1 {
		t.Fatalf("Ping reached the driver %d times, want exactly 1: %v", len(got), got)
	}
}

func TestPingCarriesAFailingDriverToTheCaller(t *testing.T) {
	f, db := registerFake(t)
	refused := errors.New("connection refused")
	f.failOn(evPing, refused)
	store := New(db)

	err := store.Ping(context.Background())
	if !errors.Is(err, refused) {
		t.Fatalf("Ping returned %v, want the driver's refusal wrapped, not swallowed or replaced", err)
	}
}

func TestWithinTxCommitsWhenTheWorkSucceeds(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)

	err := store.WithinTx(context.Background(), func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("WithinTx of successful work: %v", err)
	}
	got := of(f.recorded(), evBegin, evCommit, evRollback)
	want := []event{evBegin, evCommit}
	if !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — success must commit and never roll back", got, want)
	}
}

func TestWithinTxRollsBackWhenTheWorkFails(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	boom := errors.New("unit of work failed")

	err := store.WithinTx(context.Background(), func(context.Context) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("WithinTx returned %v, want the unit's own error handed back unchanged", err)
	}
	got := of(f.recorded(), evBegin, evCommit, evRollback)
	want := []event{evBegin, evRollback}
	if !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — failure must roll back and never commit", got, want)
	}
}

func TestWithinTxRollsBackWhenTheWorkPanics(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)

	panicked := false
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("WithinTx swallowed the panic — the caller must see it, not the pool")
			}
			panicked = true
		}()
		_ = store.WithinTx(context.Background(), func(context.Context) error {
			panic("mid-work panic")
		})
	}()
	if !panicked {
		t.Fatal("the panic guard never ran")
	}
	got := of(f.recorded(), evBegin, evCommit, evRollback)
	want := []event{evBegin, evRollback}
	if !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — a panic must leave the transaction rolled back", got, want)
	}
}

func TestNestedWithinTxJoinsTheTransactionAlreadyInFlight(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)

	// The inner scope fails; the owner of the transaction interprets that
	// failure, carries on, and commits the unit. Joining means exactly one
	// begin and one commit for the whole unit — a nested scope neither opens
	// its own transaction nor rolls the owner's back behind its back.
	err := store.WithinTx(context.Background(), func(ctx context.Context) error {
		_ = store.WithinTx(ctx, func(context.Context) error {
			return errors.New("inner scope failed")
		})
		return nil
	})
	if err != nil {
		t.Fatalf("WithinTx returned %v, want the owner's verdict (nil)", err)
	}
	got := of(f.recorded(), evBegin, evCommit, evRollback)
	want := []event{evBegin, evCommit}
	if !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — a nested scope must neither begin nor roll back on its own", got, want)
	}
}

func TestNestedWithinTxOnADifferentStoreRunsItsOwnIndependentUnit(t *testing.T) {
	fA, dbA := registerFake(t)
	fB, dbB := registerFake(t)
	storeA, storeB := New(dbA), New(dbB)

	// Two stores over two pools. Store B's nested scope must not find store
	// A's transaction — a transaction on pool A can no more carry B's
	// writes than B's queries can reach pool A — so B begins its own unit
	// and owns its own commit: BEGIN(A), BEGIN(B), COMMIT(B), COMMIT(A).
	// The package-scoped key this test's predecessor once relied on made B
	// silently join A's transaction; per-driver streams are what go red
	// under that shape — B would record nothing at all.
	err := storeA.WithinTx(context.Background(), func(ctx context.Context) error {
		return storeB.WithinTx(ctx, func(context.Context) error { return nil })
	})
	if err != nil {
		t.Fatalf("nested WithinTx across two stores: %v", err)
	}
	if got, want := of(fA.recorded(), evBegin, evCommit, evRollback), []event{evBegin, evCommit}; !equal(got, want) {
		t.Fatalf("pool A saw %v, want %v — the outer store must begin and commit its own unit untouched", got, want)
	}
	if got, want := of(fB.recorded(), evBegin, evCommit, evRollback), []event{evBegin, evCommit}; !equal(got, want) {
		t.Fatalf("pool B saw %v, want %v — the inner store must begin and commit its own unit, never join pool A's transaction", got, want)
	}
}

func TestNestedWithinTxFailureOnADifferentStoreLeavesTheOuterVerdictToItsOwner(t *testing.T) {
	fA, dbA := registerFake(t)
	fB, dbB := registerFake(t)
	storeA, storeB := New(dbA), New(dbB)

	// The inner store's unit fails on its own pool and rolls its own work
	// back; the outer owner interprets that failure and still commits its
	// own. Neither transaction's fate may depend on the other's.
	boom := errors.New("inner store's unit failed")
	err := storeA.WithinTx(context.Background(), func(ctx context.Context) error {
		_ = storeB.WithinTx(ctx, func(context.Context) error { return boom })
		return nil
	})
	if err != nil {
		t.Fatalf("WithinTx returned %v, want the outer owner's verdict (nil)", err)
	}
	if got, want := of(fA.recorded(), evBegin, evCommit, evRollback), []event{evBegin, evCommit}; !equal(got, want) {
		t.Fatalf("pool A saw %v, want %v — store B's failure must not touch store A's unit", got, want)
	}
	if got, want := of(fB.recorded(), evBegin, evCommit, evRollback), []event{evBegin, evRollback}; !equal(got, want) {
		t.Fatalf("pool B saw %v, want %v — store B's failed unit must roll back on its own pool", got, want)
	}
}

func TestAStoreNeverHandsOutAnotherPoolsTransaction(t *testing.T) {
	fA, dbA := registerFake(t)
	fB, dbB := registerFake(t)
	storeA, storeB := New(dbA), New(dbB)

	// The leak this test forbids: store B's repository, running inside
	// store A's unit, resolving its query surface and finding A's
	// transaction there. Two probes pin the opposite, both at the driver:
	//
	//   - inside A's unit but outside any unit of B's own, B's Querier is
	//     B's pool — the exec runs as a plain pool statement on pool B, and
	//     pool A's stream shows no exec at all (it would if A's transaction
	//     had answered);
	//   - inside B's own nested unit, B's Querier is B's transaction — one
	//     connect on B for the whole nested run, because the exec rode the
	//     connection that transaction holds.
	err := storeA.WithinTx(context.Background(), func(ctx context.Context) error {
		if _, err := storeB.Querier(ctx).ExecContext(ctx, "SELECT 1"); err != nil {
			return err
		}
		return storeB.WithinTx(ctx, func(bctx context.Context) error {
			_, err := storeB.Querier(bctx).ExecContext(bctx, "UPDATE probe SET ok = true")
			return err
		})
	})
	if err != nil {
		t.Fatalf("WithinTx: %v", err)
	}
	if got, want := of(fA.recorded(), evConnect, evBegin, evExec, evCommit), []event{evConnect, evBegin, evCommit}; !equal(got, want) {
		t.Fatalf("pool A saw %v, want %v — store A's transaction must execute nothing that store B sent", got, want)
	}
	// Pool B's stream tells the whole story in order: an exec with no begin
	// before it (the pool answering outside any unit of work), then B's own
	// begin, exec, commit. One connect only — the first exec returned its
	// connection to the pool and BeginTx reused it — so here the sequence,
	// not the connect count, is the proof; the unit-bound test above is
	// where the count carries the argument.
	if got, want := of(fB.recorded(), evConnect, evBegin, evExec, evCommit), []event{evConnect, evExec, evBegin, evExec, evCommit}; !equal(got, want) {
		t.Fatalf("pool B saw %v, want %v — the first exec is B's pool answering outside any unit, the begin/exec pair is B's own unit", got, want)
	}
}

func TestWithinTxFailingWorkCannotHideARollbackFailure(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	boom := errors.New("unit of work failed")
	f.failOn(evRollback, errors.New("connection died"))

	err := store.WithinTx(context.Background(), func(context.Context) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("WithinTx returned %v, want the unit's error — a dead connection's rollback noise must not replace it", err)
	}
}

func TestWithinTxSurfacesABeginFailureUnmuted(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	refused := errors.New("cannot connect now")
	f.failOn(evBegin, refused)

	err := store.WithinTx(context.Background(), func(context.Context) error { return nil })
	if !errors.Is(err, refused) {
		t.Fatalf("WithinTx returned %v, want the driver's refusal preserved for the caller to inspect", err)
	}
}

func TestWithinTxSurfacesACommitFailureWithoutTouchingThePoolAgain(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	refused := errors.New("commit refused")
	f.failOn(evCommit, refused)

	err := store.WithinTx(context.Background(), func(context.Context) error { return nil })
	if !errors.Is(err, refused) {
		t.Fatalf("WithinTx returned %v, want the commit failure", err)
	}
	// Since Go 1.20, a failed Commit leaves database/sql assuming the
	// connection is in an unknown state: the pool discards it, the deferred
	// Rollback answers ErrTxDone, and no further driver call is made. The
	// server rolls the open transaction back when that connection dies —
	// which is why the driver seeing only a begin here is the correct end
	// state, not an unfinished one.
	got := of(f.recorded(), evBegin, evCommit, evRollback)
	want := []event{evBegin}
	if !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — after a refused commit the adapter must not drive the discarded connection further", got, want)
	}
}

func TestQuerierInsideAUnitOfWorkRunsOnTheUnitsTransaction(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)

	// A repository inside the unit resolves its query surface from the work
	// context and runs its statement through it. The exec must land on the
	// connection the transaction holds: exactly one connect for the whole
	// run, because a pool exec would find no idle connection — the
	// transaction holds the only one the fake has opened — and be forced to
	// open a second. One connect is the proof the statement went around no
	// unit of work: it went through it.
	err := store.WithinTx(context.Background(), func(ctx context.Context) error {
		_, err := store.Querier(ctx).ExecContext(ctx, "UPDATE probe SET ok = true")
		return err
	})
	if err != nil {
		t.Fatalf("WithinTx: %v", err)
	}
	got := of(f.recorded(), evConnect, evBegin, evExec, evCommit, evRollback)
	want := []event{evConnect, evBegin, evExec, evCommit}
	if !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — the resolved Querier must be the unit's transaction, not a second connection the pool found", got, want)
	}
}

func TestQuerierOutsideAUnitOfWorkRunsOnThePool(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)

	// With no unit of work in flight the same resolution answers with the
	// pool: the statement runs on its own connection, with no begin and no
	// commit around it. The two Querier tests together pin both halves of
	// the resolution — the context decides, and the storage layer is the
	// only place that reads it.
	if _, err := store.Querier(context.Background()).ExecContext(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("ExecContext through the pool: %v", err)
	}
	got := of(f.recorded(), evConnect, evBegin, evExec, evCommit, evRollback)
	want := []event{evConnect, evExec}
	if !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — outside a unit of work the Querier is the pool, not a transaction", got, want)
	}
}

func TestPingHonoursTheCallerContext(t *testing.T) {
	_, db := registerFake(t)
	store := New(db)

	// A cancelled context must reach the pool: Ping is a readiness answer,
	// and answering readiness for a request already gone is a lie.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Ping(ctx); err == nil {
		t.Fatal("Ping with a cancelled context succeeded, want the context's cancellation surfaced")
	}
}

// equal is a two-line helper rather than slices.Equal because the compared
// elements are a named string type and the failure message above already
// prints both sides — what a mismatch means lives in the test, not here.
func equal(got, want []event) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// openTestDSN is the DSN every Open test hands the fake driver, which parses
// none of it. It carries a password on purpose: the tests that print an error
// out of Open assert the password never rides along with it.
const openTestDSN = "postgres://gateway:unit-test-password@127.0.0.1:5432/control?sslmode=disable"

// openTestOptions returns the smallest Options Open accepts, so each invalid
// case below changes exactly one field.
func openTestOptions() Options {
	return Options{
		DSN:             openTestDSN,
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
	}
}

func TestOpenRejectsInvalidOptions(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(*Options)
		wantErr string
	}{
		{
			name:    "requires a DSN",
			edit:    func(o *Options) { o.DSN = "" },
			wantErr: "postgres: DSN is required",
		},
		{
			name:    "rejects a zero MaxOpenConns",
			edit:    func(o *Options) { o.MaxOpenConns = 0 },
			wantErr: "postgres: MaxOpenConns must be greater than zero",
		},
		{
			name:    "rejects a negative MaxOpenConns",
			edit:    func(o *Options) { o.MaxOpenConns = -1 },
			wantErr: "postgres: MaxOpenConns must be greater than zero",
		},
		{
			name:    "rejects a negative MaxIdleConns",
			edit:    func(o *Options) { o.MaxIdleConns = -1 },
			wantErr: "postgres: MaxIdleConns must not be negative",
		},
		{
			name:    "rejects a MaxIdleConns above MaxOpenConns",
			edit:    func(o *Options) { o.MaxIdleConns = 9 },
			wantErr: "postgres: MaxIdleConns (9) must not exceed MaxOpenConns (4)",
		},
		{
			name:    "rejects a negative ConnMaxLifetime",
			edit:    func(o *Options) { o.ConnMaxLifetime = -time.Second },
			wantErr: "postgres: ConnMaxLifetime must not be negative",
		},
		{
			name:    "rejects a negative ConnMaxIdleTime",
			edit:    func(o *Options) { o.ConnMaxIdleTime = -time.Second },
			wantErr: "postgres: ConnMaxIdleTime must not be negative",
		},
		{
			name: "rejects a DSN that names another plane's database",
			edit: func(o *Options) {
				o.DSN = "postgres://gateway:unit-test-password@127.0.0.1:5432/dataplane?sslmode=disable"
			},
			wantErr: "DSN names database \"dataplane\", but this application owns only the control database",
		},
		{
			name:    "rejects a DSN with no database in its path",
			edit:    func(o *Options) { o.DSN = "postgres://gateway:unit-test-password@127.0.0.1:5432/?sslmode=disable" },
			wantErr: "postgres: DSN must name exactly one database in its path",
		},
		{
			// A keyword-value DSN parses as a URL with no scheme and the
			// whole string for a path. The refusal must come from the scheme
			// check, quoting nothing — an error that quoted the "database"
			// this DSN seems to name would quote its password.
			name: "rejects a keyword-value DSN without quoting it",
			edit: func(o *Options) {
				o.DSN = "host=127.0.0.1 port=5432 dbname=dataplane user=gateway password=unit-test-password"
			},
			wantErr: "postgres: DSN must be a postgres:// or postgresql:// URL",
		},
		{
			// One leading slash is the path's separator; a second one is
			// part of the database name this door reads, while the driver
			// trims it away. The two readings must never both pass.
			name:    "rejects a double-slash database path",
			edit:    func(o *Options) { o.DSN = "postgres://gateway:unit-test-password@127.0.0.1:5432//control" },
			wantErr: "postgres: DSN must name exactly one database in its path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := openTestOptions()
			tt.edit(&opts)
			// The driver name is one that was never registered, so a
			// validation failure that came after the dial attempt would
			// answer "unknown driver" and miss every wantErr below — the
			// only way these pass is by rejecting before anything is opened.
			db, err := open(context.Background(), "unregistered-driver", opts)
			if err == nil {
				_ = db.Close()
				t.Fatal("open() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("open() error = %q, want it to contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "unit-test-password") {
				t.Errorf("open() error = %q, must not carry the DSN's password", err)
			}
		})
	}
}

func TestOpenConfiguresThePoolAndPingsThroughTheDriver(t *testing.T) {
	f, name := newFake(t)
	db, err := open(context.Background(), name, Options{
		DSN:             openTestDSN,
		MaxOpenConns:    7,
		MaxIdleConns:    3,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// MaxOpenConnections is the one configured limit the pool reports back,
	// so it is read from the pool rather than trusted from the code path;
	// the other three settings are proven behaviourally, in the two tests
	// below, because the fake driver is the only witness they have.
	if got := db.Stats().MaxOpenConnections; got != 7 {
		t.Errorf("MaxOpenConnections = %d, want 7", got)
	}
	// The ping is what makes the pool validated rather than merely opened:
	// it must have reached the driver exactly once before Open returned.
	if got := of(f.recorded(), evPing); len(got) != 1 {
		t.Fatalf("the driver was pinged %d times, want exactly 1: %v", len(got), got)
	}
}

func TestOpenAppliesMaxIdleConnsByClosingTheExcess(t *testing.T) {
	// Six connections are checked out of a pool that may open six and keep
	// four idle, held so they genuinely coexist, then all returned at once:
	// four go back to the idle pool and the two that come back to a full one
	// are closed on return — synchronously, by database/sql's own
	// accounting, so the count at the driver is exact and the proof does not
	// depend on a janitor's tick or on goroutine timing. Four is also what
	// makes this a proof of the setting rather than of the standard
	// library's default of two.
	f, name := newFake(t)
	db, err := open(context.Background(), name, Options{
		DSN:             openTestDSN,
		MaxOpenConns:    6,
		MaxIdleConns:    4,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const held = 6
	checked := make([]*sql.Conn, 0, held)
	for i := 0; i < held; i++ {
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatalf("db.Conn() %d of %d: %v", i+1, held, err)
		}
		checked = append(checked, conn)
	}
	for _, conn := range checked {
		_ = conn.Close()
	}

	if closed := len(of(f.recorded(), evClose)); closed != held-4 {
		t.Fatalf("the driver closed %d connections before the pool was closed, want %d — MaxIdleConns=4 must have turned the excess away", closed, held-4)
	}
}

func TestOpenAppliesConnMaxLifetimeByClosingExpiredConnectionsOnReturn(t *testing.T) {
	// A one-nanosecond lifetime expires every connection the instant it is
	// returned, so database/sql closes each at put time instead of pooling
	// it: ping and exec each cost the driver one connect and one close, and
	// nothing is ever reused. Had SetConnMaxLifetime not been applied, the
	// stream would show two connects and no closes at all.
	f, name := newFake(t)
	db, err := open(context.Background(), name, Options{
		DSN:             openTestDSN,
		MaxOpenConns:    2,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Nanosecond,
		ConnMaxIdleTime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("ExecContext on the expired-lifetime pool: %v", err)
	}
	got, want := of(f.recorded(), evConnect, evPing, evExec, evClose), []event{evConnect, evPing, evClose, evConnect, evExec, evClose}
	if !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — a connection past its lifetime must be closed on return, never pooled for reuse", got, want)
	}
	// ConnMaxIdleTime is the one setting this fake cannot witness: database/sql
	// enforces it on the connection cleaner, whose first pass is never sooner
	// than one second — real time this suite does not spend. Its enforcement
	// is pinned in the integration suite, against the real server.
}

func TestOpenSurfacesAPingFailureAndClosesThePool(t *testing.T) {
	f, name := newFake(t)
	refused := errors.New("connection refused")
	f.failOn(evPing, refused)

	db, err := open(context.Background(), name, openTestOptions())
	if err == nil {
		_ = db.Close()
		t.Fatal("open() error = nil, want the ping failure")
	}
	if !errors.Is(err, refused) {
		t.Fatalf("open() returned %v, want the driver's refusal wrapped, not swallowed or replaced", err)
	}
	if !strings.Contains(err.Error(), "postgres: connect") {
		t.Errorf("open() error = %q, want it to name the connection that failed startup validation", err)
	}
	if strings.Contains(err.Error(), "unit-test-password") {
		t.Errorf("open() error = %q, must not carry the DSN's password", err)
	}
	// The pool Open failed to validate is closed behind it: the connection
	// the ping opened is let go before the error leaves, so a failed startup
	// leaves nothing pooled for a caller who never received the handle.
	if got, want := of(f.recorded(), evConnect, evPing, evClose), []event{evConnect, evClose}; !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — the connection the failed ping opened must be closed, not pooled", got, want)
	}
}

func TestOpenFailsFastOnAnAlreadyCancelledContext(t *testing.T) {
	f, name := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	db, err := open(ctx, name, openTestOptions())
	if err == nil {
		_ = db.Close()
		t.Fatal("open() with a cancelled context error = nil, want the context's cancellation surfaced")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("open() returned %v, want context.Canceled wrapped", err)
	}
	if got := of(f.recorded(), evConnect, evPing); len(got) != 0 {
		t.Fatalf("driver calls were %v, want none — startup validation for a context already gone must not reach the driver", got)
	}
}

func TestWithoutDSNStrikesTheDSNOutOfADriverError(t *testing.T) {
	// A driver parse error quotes the connection string it was handed —
	// pgx's do, credentials and all — so the wrapper exists for exactly this
	// shape: the DSN struck out, the rest of the driver's text kept.
	leak := errors.New("cannot parse `" + openTestDSN + "`: invalid sslmode")
	got := withoutDSN(leak, openTestDSN)
	if strings.Contains(got.Error(), "unit-test-password") || strings.Contains(got.Error(), openTestDSN) {
		t.Fatalf("withoutDSN() = %q, want the DSN struck out", got)
	}
	if !strings.Contains(got.Error(), "[redacted dsn]") {
		t.Errorf("withoutDSN() = %q, want the redaction marker where the DSN was", got)
	}

	// An error that carries no DSN is handed back unchanged — the same
	// value, so errors.Is across the wrapper keeps working.
	kept := errors.New("connection refused")
	if returned := withoutDSN(kept, openTestDSN); returned != kept {
		t.Errorf("withoutDSN() returned a new error %v where the original carried no DSN", returned)
	}
}

func TestWithinTxWithAnAlreadyCancelledContextFailsAtBegin(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.WithinTx(ctx, func(context.Context) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithinTx returned %v, want the context's cancellation surfaced", err)
	}
	if !strings.Contains(err.Error(), "postgres: begin transaction") {
		t.Errorf("WithinTx error = %q, want it to name the begin that failed", err)
	}
	// The cancellation is answered before the driver is asked for anything:
	// no connection opened, no transaction begun, nothing to roll back.
	if got := of(f.recorded(), evConnect, evBegin, evCommit, evRollback); len(got) != 0 {
		t.Fatalf("driver calls were %v, want none — an already-gone context must not reach the driver", got)
	}
}
