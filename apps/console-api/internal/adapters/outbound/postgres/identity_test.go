package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
)

// The identity repositories' tests over the fake driver. The driver refuses
// statements (Prepare and Query are "not part of this contract"), so what
// these tests can pin is exactly what the adapter's contract is at this
// layer: which handle a statement runs on, what commits and rolls back, and
// how a driver-reported condition becomes the port's vocabulary. Reading
// rows back is PostgreSQL behaviour and belongs to the real-database tiers:
// deploy/postgres/verify.sh at the SQL level, the integration-tagged tests
// in this package at the repository level.

var identityClock = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// sqlStateError is the test's stand-in for a driver error that reports a
// SQLSTATE — the structural seam the adapter classifies, satisfied for real
// by pgconn's error type and satisfied here by a hand-written one.
type sqlStateError struct{ state string }

func (e *sqlStateError) Error() string    { return "driver reported condition " + e.state }
func (e *sqlStateError) SQLState() string { return e.state }

func TestCreateAccountRunsInsideTheCallersUnitOfWork(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	accounts := NewAccounts(store)

	account := mustAccountFor(t)
	err := store.WithinTx(context.Background(), func(ctx context.Context) error {
		return accounts.Create(ctx, *account)
	})
	if err != nil {
		t.Fatalf("create account inside a unit of work: %v", err)
	}
	got := of(f.recorded(), evConnect, evBegin, evExec, evCommit, evRollback)
	want := []event{evConnect, evBegin, evExec, evCommit}
	if !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — the insert must ride the unit's transaction, with no second connection", got, want)
	}
}

func TestCreateAccountOutsideAUnitOfWorkRunsOnThePool(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	accounts := NewAccounts(store)

	if err := accounts.Create(context.Background(), *mustAccountFor(t)); err != nil {
		t.Fatalf("create account outside a unit of work: %v", err)
	}
	got := of(f.recorded(), evConnect, evBegin, evExec, evCommit, evRollback)
	want := []event{evConnect, evExec}
	if !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — with no unit of work the insert runs on the pool, bare", got, want)
	}
}

func TestCreateUserMapsAUniqueViolationToTheEmailTakenSentinel(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	users := NewUsers(store)
	f.failOn(evExec, &sqlStateError{state: "23505"})

	user := mustUserFor(t)
	err := users.Create(context.Background(), *user)
	if !errors.Is(err, identity.ErrUserEmailTaken) {
		t.Fatalf("create user under a 23505 returned %v, want identity.ErrUserEmailTaken", err)
	}
}

func TestCreateUserCarriesOtherDriverFailuresUnmapped(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	users := NewUsers(store)
	boom := &sqlStateError{state: "08006"} // connection_failure — nothing to do with uniqueness
	f.failOn(evExec, boom)

	err := users.Create(context.Background(), *mustUserFor(t))
	if errors.Is(err, identity.ErrUserEmailTaken) {
		t.Fatalf("a non-uniqueness driver failure was mapped to ErrUserEmailTaken")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("create user returned %v, want the driver failure wrapped and unmapped", err)
	}
}

func TestRevokeReportsALostSwapAsNotApplied(t *testing.T) {
	_, db := registerFake(t)
	store := New(db)
	keys := NewAPIKeys(store)

	// The fake's ExecContext always answers zero rows affected: the one
	// outcome the adapter must not lie about is the swap that matched
	// nothing — applied is false and the error is nil, because "no row
	// matched" is a verdict, not a failure.
	applied, err := keys.Revoke(context.Background(), "c0000000-0000-0000-0000-0000000000c1", identityClock)
	if err != nil {
		t.Fatalf("revoke against a zero-rows answer: %v", err)
	}
	if applied {
		t.Fatal("revoke reported a swap applied that the driver said matched no rows")
	}
}

func TestTransitionStateReportsALostSwapAsNotApplied(t *testing.T) {
	_, db := registerFake(t)
	store := New(db)
	accounts := NewAccounts(store)

	applied, err := accounts.TransitionState(context.Background(),
		"a0000000-0000-0000-0000-0000000000a1", identity.AccountActive, identity.AccountSuspended, identityClock)
	if err != nil {
		t.Fatalf("transition against a zero-rows answer: %v", err)
	}
	if applied {
		t.Fatal("transition reported a swap applied that the driver said matched no rows")
	}
}

func TestIdentityRepositoriesRefuseANilStore(t *testing.T) {
	for name, build := range map[string]func(){
		"accounts": func() { NewAccounts(nil) },
		"users":    func() { NewUsers(nil) },
		"api keys": func() { NewAPIKeys(nil) },
	} {
		panicked := false
		func() {
			defer func() {
				if recover() != nil {
					panicked = true
				}
			}()
			build()
		}()
		if !panicked {
			t.Fatalf("%s: NewX(nil) did not panic — a nil store is a wiring defect, not a runtime surprise", name)
		}
	}
}

// mustAccountFor returns a valid account aggregate for adapter tests.
func mustAccountFor(t *testing.T) *identity.Account {
	t.Helper()
	a, err := identity.NewAccount("a0000000-0000-0000-0000-0000000000a1", "adapter probe", identityClock)
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	return a
}

// mustUserFor returns a valid user aggregate for adapter tests.
func mustUserFor(t *testing.T) *identity.User {
	t.Helper()
	u, err := identity.NewUser("b0000000-0000-0000-0000-0000000000b1",
		"a0000000-0000-0000-0000-0000000000a1", "adapter@example.com", identityClock)
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	return u
}
