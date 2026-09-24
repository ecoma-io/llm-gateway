package postgres

import (
	"context"
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

// fakeDSN is a well-formed DSN for this application's own database. The fake
// driver ignores its content, but Open reads it far enough to enforce the
// plane binding, so it names the database this adapter serves for the same
// reason a real call would.
func fakeDSN() string {
	return "postgres://gateway:not-the-fixture-password@127.0.0.1:5432/dataplane?sslmode=disable"
}

func TestOpenValidatesItsOptionsBeforeTouchingADriver(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		wantErr string
	}{
		{
			name:    "rejects an empty DSN",
			options: Options{MaxOpenConns: 25, MaxIdleConns: 5},
			wantErr: "DSN is required",
		},
		{
			name: "rejects a zero max open conns",
			options: Options{
				DSN: fakeDSN(), MaxOpenConns: 0, MaxIdleConns: 5,
				ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
			},
			wantErr: "MaxOpenConns 0 must be at least 1",
		},
		{
			name: "rejects a negative max open conns",
			options: Options{
				DSN: fakeDSN(), MaxOpenConns: -1, MaxIdleConns: 5,
				ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
			},
			wantErr: "MaxOpenConns -1 must be at least 1",
		},
		{
			name: "rejects a negative max idle conns",
			options: Options{
				DSN: fakeDSN(), MaxOpenConns: 25, MaxIdleConns: -1,
				ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
			},
			wantErr: "MaxIdleConns -1 must not be negative",
		},
		{
			name: "rejects a max idle conns greater than max open conns",
			options: Options{
				DSN: fakeDSN(), MaxOpenConns: 5, MaxIdleConns: 6,
				ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
			},
			wantErr: "MaxIdleConns 6 must not be greater than MaxOpenConns 5",
		},
		{
			name: "rejects a negative conn max lifetime",
			options: Options{
				DSN: fakeDSN(), MaxOpenConns: 25, MaxIdleConns: 5,
				ConnMaxLifetime: -time.Second, ConnMaxIdleTime: 5 * time.Minute,
			},
			wantErr: "ConnMaxLifetime -1s must not be negative",
		},
		{
			name: "rejects a negative conn max idle time",
			options: Options{
				DSN: fakeDSN(), MaxOpenConns: 25, MaxIdleConns: 5,
				ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: -time.Second,
			},
			wantErr: "ConnMaxIdleTime -1s must not be negative",
		},
		{
			name: "refuses a DSN naming another plane's database",
			options: Options{
				DSN:          "postgres://gateway:not-the-fixture-password@127.0.0.1:5432/control?sslmode=disable",
				MaxOpenConns: 25, MaxIdleConns: 5,
				ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
			},
			wantErr: "this adapter serves the dataplane database only",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Open is called with the real driver name on purpose: every case
			// above must be refused before sql.Open resolves a driver at all,
			// so the refusal is proven against the exported entry point
			// without registering anything.
			db, err := Open(context.Background(), tt.options)
			if err == nil {
				_ = db.Close()
				t.Fatal("Open() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Open() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestOpenAppliesThePoolSizesAndPingsTheDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The open-connections bound is the one size the pool states back as a
	// fact, and the ping is the startup validation itself: exactly one,
	// before Open returns.
	sizedF, sizedName := newFake(t)
	sizedDB, err := open(ctx, sizedName, Options{
		DSN: fakeDSN(), MaxOpenConns: 7, MaxIdleConns: 1,
		ConnMaxLifetime: time.Hour, ConnMaxIdleTime: time.Hour,
	})
	if err != nil {
		t.Fatalf("open() error = %v", err)
	}
	t.Cleanup(func() { _ = sizedDB.Close() })
	if got := sizedDB.Stats().MaxOpenConnections; got != 7 {
		t.Errorf("pool MaxOpenConnections = %d, want 7 — Open must apply the open-connections bound", got)
	}
	if got, want := of(sizedF.recorded(), evPing), []event{evPing}; !equal(got, want) {
		t.Fatalf("the driver saw %v pings, want exactly 1 — Open's ping is what makes the open verified", got)
	}

	// The idle bound is not exposed as a setting, so it is proven the way the
	// pool behaves: with MaxIdleConns 0 a released connection is closed
	// rather than kept — the ping's connection too, since Open's ping releases
	// before the statement runs — and with MaxIdleConns 1 the statement's
	// connection is kept for the next caller.
	spentF, spentName := newFake(t)
	spentDB, err := open(ctx, spentName, Options{
		DSN: fakeDSN(), MaxOpenConns: 2, MaxIdleConns: 0,
		ConnMaxLifetime: time.Hour, ConnMaxIdleTime: time.Hour,
	})
	if err != nil {
		t.Fatalf("open() with MaxIdleConns 0 error = %v", err)
	}
	t.Cleanup(func() { _ = spentDB.Close() })
	if _, err := spentDB.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatalf("ExecContext with MaxIdleConns 0: %v", err)
	}
	if got, want := of(spentF.recorded(), evConnect, evExec, evClose), []event{evConnect, evClose, evConnect, evExec, evClose}; !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — MaxIdleConns 0 must close every connection it releases", got, want)
	}

	keptF, keptName := newFake(t)
	keptDB, err := open(ctx, keptName, Options{
		DSN: fakeDSN(), MaxOpenConns: 2, MaxIdleConns: 1,
		ConnMaxLifetime: time.Hour, ConnMaxIdleTime: time.Hour,
	})
	if err != nil {
		t.Fatalf("open() with MaxIdleConns 1 error = %v", err)
	}
	t.Cleanup(func() { _ = keptDB.Close() })
	if _, err := keptDB.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatalf("ExecContext with MaxIdleConns 1: %v", err)
	}
	if got, want := of(keptF.recorded(), evConnect, evExec), []event{evConnect, evExec}; !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — MaxIdleConns 1 must keep the released connection, and the statement must reuse the ping's", got, want)
	}
	if got := keptDB.Stats().Idle; got != 1 {
		t.Errorf("pool idle connections = %d, want 1 — the bound the configuration names is the bound the pool enforces", got)
	}
}

func TestOpenAppliesTheConnectionLifetimes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A long lifetime reuses one connection for everything: the ping's
	// connection answers both statements, and nothing is retired.
	keptF, keptName := newFake(t)
	keptDB, err := open(ctx, keptName, Options{
		DSN: fakeDSN(), MaxOpenConns: 2, MaxIdleConns: 1,
		ConnMaxLifetime: time.Hour, ConnMaxIdleTime: time.Hour,
	})
	if err != nil {
		t.Fatalf("open() with an hour-long ConnMaxLifetime error = %v", err)
	}
	t.Cleanup(func() { _ = keptDB.Close() })
	for i := 0; i < 2; i++ {
		if _, err := keptDB.ExecContext(ctx, "SELECT 1"); err != nil {
			t.Fatalf("ExecContext %d: %v", i, err)
		}
	}
	if got, want := of(keptF.recorded(), evConnect, evExec), []event{evConnect, evExec, evExec}; !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — a long lifetime must reuse the connection the ping opened", got, want)
	}

	// A one-nanosecond lifetime is already spent when the connection is
	// released, so the pool retires it at once: the ping and each statement
	// stand up their own connection, and the pool counts every retirement as
	// a lifetime close.
	spentF, spentName := newFake(t)
	spentDB, err := open(ctx, spentName, Options{
		DSN: fakeDSN(), MaxOpenConns: 2, MaxIdleConns: 1,
		ConnMaxLifetime: time.Nanosecond, ConnMaxIdleTime: time.Hour,
	})
	if err != nil {
		t.Fatalf("open() with a nanosecond ConnMaxLifetime error = %v", err)
	}
	t.Cleanup(func() { _ = spentDB.Close() })
	for i := 0; i < 2; i++ {
		if _, err := spentDB.ExecContext(ctx, "SELECT 1"); err != nil {
			t.Fatalf("ExecContext %d: %v", i, err)
		}
	}
	if got, want := of(spentF.recorded(), evConnect, evExec), []event{evConnect, evConnect, evExec, evConnect, evExec}; !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — a spent ConnMaxLifetime must stand up a fresh connection per statement, never reuse one", got, want)
	}
	stats := spentDB.Stats()
	if got, want := stats.MaxLifetimeClosed, int64(3); got != want {
		t.Errorf("pool MaxLifetimeClosed = %d, want %d — Open must apply the connection lifetime the configuration names", got, want)
	}

	// The idle time is enforced by the pool's own background cleaner, so its
	// effect is a matter of when, not whether: with a one-nanosecond idle time
	// the released connection is retired within the cleaner's first pass,
	// which the pool counts as an idle-time close. The wait is bounded; the
	// cleaner's interval with this configuration is nanoseconds.
	_, idleName := newFake(t)
	idleDB, err := open(ctx, idleName, Options{
		DSN: fakeDSN(), MaxOpenConns: 2, MaxIdleConns: 1,
		ConnMaxLifetime: time.Hour, ConnMaxIdleTime: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("open() with a nanosecond ConnMaxIdleTime error = %v", err)
	}
	t.Cleanup(func() { _ = idleDB.Close() })
	if _, err := idleDB.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatalf("ExecContext with a nanosecond ConnMaxIdleTime: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for idleDB.Stats().MaxIdleTimeClosed == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no connection was ever closed for idle time — Open must apply the idle time the configuration names")
		}
		time.Sleep(time.Millisecond)
	}
	if got := idleDB.Stats().Idle; got != 0 {
		t.Errorf("pool idle connections = %d, want 0 — a connection past its idle time must not sit in the pool", got)
	}
}

func TestOpenClosesThePoolAndCarriesThePingFailureToTheCaller(t *testing.T) {
	f, name := newFake(t)
	refused := errors.New("connection refused")
	f.failOn(evPing, refused)

	dsn := fakeDSN()
	_, err := open(context.Background(), name, Options{
		DSN: dsn, MaxOpenConns: 25, MaxIdleConns: 5,
		ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
	})
	if !errors.Is(err, refused) {
		t.Fatalf("open() returned %v, want the ping failure wrapped for the caller to inspect", err)
	}
	if strings.Contains(err.Error(), dsn) {
		t.Errorf("open() error = %q, want it to carry no DSN — its userinfo is a credential", err)
	}
	// Whether the ping's own connection was discarded by the pool or the
	// half-built pool's Close reaped it, the connection the ping opened is
	// closed by the time open returns. Either path is correct; both are the
	// same promise.
	if got, want := of(f.recorded(), evClose), []event{evClose}; !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — a failed open must close the pool it built", got, want)
	}
}

func TestOpenFailsFastWithACancelledContext(t *testing.T) {
	f, name := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := open(ctx, name, Options{
		DSN: fakeDSN(), MaxOpenConns: 25, MaxIdleConns: 5,
		ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("open() returned %v, want the caller's cancellation surfaced", err)
	}
	// Failing fast means failing before the database is contacted at all —
	// the same branch of the code the ping-failure test above proves closes
	// the half-built pool, reached here before any connection exists.
	if got, want := of(f.recorded(), evConnect, evPing), []event{}; !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — a cancelled open must not reach the database", got, want)
	}
}

func TestWithinTxFailsAtBeginTxWithACancelledContext(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.WithinTx(ctx, func(context.Context) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithinTx returned %v, want the caller's cancellation surfaced", err)
	}
	if !strings.Contains(err.Error(), "begin transaction") {
		t.Errorf("WithinTx error = %q, want the wrapped begin error the adapter attaches", err)
	}
	// The failure happened before a transaction existed, so the driver saw
	// no begin, no commit and no rollback: there was nothing to undo.
	if got, want := of(f.recorded(), evBegin, evCommit, evRollback), []event{}; !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — a begin that never happened leaves no transaction behind", got, want)
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
