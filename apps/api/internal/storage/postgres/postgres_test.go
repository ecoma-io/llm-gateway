package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// Every test here pins one clause of the port's contract, phrased in
// storage.go: what commits, what rolls back, what joins, and what a caller
// is told when the driver refuses. The driver is the fake in
// fakedriver_test.go; what these tests prove is the adapter's orchestration,
// not PostgreSQL's — that belongs to the suite in deploy/postgres.

func TestNewHandsBackThePortNotTheAdapter(t *testing.T) {
	_, db := registerFake(t)

	// New's return type is the port, so `store` here is a storage.Store by
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

func TestWithinTxCarriesTheOpenTransactionInTheWorkContext(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)

	// The port's query surface belongs to the repositories built on it, and
	// none exist yet — so no query can be routed here. What can and must be
	// pinned now is the mechanism those repositories will resolve: the work
	// context carries the open *sql.Tx itself, under the key the adapter
	// owns, from the moment fn starts. A repository that reads its
	// transaction from the context — never from the pool, as the port
	// requires — therefore holds the real, in-flight transaction: the same
	// one this test watches the driver begin exactly once for the unit.
	var carried *sql.Tx
	err := store.WithinTx(context.Background(), func(ctx context.Context) error {
		tx, ok := ctx.Value(txKey{}).(*sql.Tx)
		if !ok || tx == nil {
			t.Fatal("the work context carries no live *sql.Tx — a repository resolving its transaction from the context would silently query outside the unit of work")
		}
		carried = tx
		return nil
	})
	if err != nil {
		t.Fatalf("WithinTx: %v", err)
	}
	if carried == nil {
		t.Fatal("the work function never ran")
	}
	got := of(f.recorded(), evBegin, evCommit, evRollback)
	want := []event{evBegin, evCommit}
	if !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — the carried transaction must be the unit's one and only", got, want)
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
