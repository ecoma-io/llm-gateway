package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
)

// The commerce repositories' tests over the fake driver, on the same terms
// as the identity suite: the driver refuses statements (Prepare and query
// are "not part of this contract") and always answers zero rows affected, so
// what these tests pin is exactly what this layer's contract is — which
// handle a statement runs on, how a driver-reported condition becomes the
// port's vocabulary (here, the constraint name a uniqueness violation
// carries), and that a guarded move whose WHERE clause matched nothing is
// the verdict false and not an error. Reading rows back, the clock's
// transaction stability and the whole single-statement due-work story are
// PostgreSQL behaviour and belong to the integration tier in this package.

var commerceClock = time.Date(2026, 9, 25, 15, 0, 0, 0, time.UTC)

// pgUniqueViolationErr is the test's stand-in for the driver's own error
// type — the one shape that carries a constraint name alongside its
// SQLSTATE. The real driver reports exactly this struct through
// database/sql; building one by hand is the honest way to pin the mapping
// without a live database deciding which rule to break.
func pgUniqueViolationErr(constraint string) *pgconn.PgError {
	return &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: constraint}
}

func TestCreateSubscriptionRunsInsideTheCallersUnitOfWork(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	subscriptions := NewSubscriptions(store)

	subscription := mustSubscriptionFor(t)
	err := store.WithinTx(context.Background(), func(ctx context.Context) error {
		return subscriptions.Create(ctx, *subscription)
	})
	if err != nil {
		t.Fatalf("create subscription inside a unit of work: %v", err)
	}
	got := of(f.recorded(), evConnect, evBegin, evExec, evCommit, evRollback)
	want := []event{evConnect, evBegin, evExec, evCommit}
	if !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — the insert must ride the unit's transaction, with no second connection", got, want)
	}
}

func TestCreatePlanOutsideAUnitOfWorkRunsOnThePool(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	plans := NewPlans(store)

	if err := plans.Create(context.Background(), *mustPlanFor(t)); err != nil {
		t.Fatalf("create plan outside a unit of work: %v", err)
	}
	got := of(f.recorded(), evConnect, evBegin, evExec, evCommit, evRollback)
	want := []event{evConnect, evExec}
	if !equal(got, want) {
		t.Fatalf("driver calls were %v, want %v — with no unit of work the insert runs on the pool, bare", got, want)
	}
}

func TestSetEnabledWritesWithoutAPriorRead(t *testing.T) {
	_, db := registerFake(t)
	store := New(db)
	payg := NewPaygAccounts(store)

	// The flag's whole state is one boolean: the insert-or-update is the one
	// place this port writes without reading first, so the statement must go
	// out as a single exec and no query may precede it.
	if err := payg.SetEnabled(context.Background(), "a0000000-0000-0000-0000-0000000000a1", true, commerceClock); err != nil {
		t.Fatalf("set payg enabled: %v", err)
	}
}

// TestCreatePlanMapsTheNameUniquenessConstraintToItsSentinel and the two
// tests after it pin the constraint-name → sentinel translation for each of
// the three constraints a commerce insert can lose to: the name travels as
// the driver's own error field and becomes the domain's word for the rule
// here, and nowhere above this file.
func TestCreatePlanMapsTheNameUniquenessConstraintToItsSentinel(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	plans := NewPlans(store)
	f.failOn(evExec, pgUniqueViolationErr("plans_name_key"))

	err := plans.Create(context.Background(), *mustPlanFor(t))
	if !errors.Is(err, commerce.ErrPlanNameTaken) {
		t.Fatalf("create plan under plans_name_key returned %v, want commerce.ErrPlanNameTaken", err)
	}
}

func TestCreatePlanVersionMapsTheVersionNumberConstraintToItsSentinel(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	versions := NewPlanVersions(store)
	f.failOn(evExec, pgUniqueViolationErr("plan_versions_version_number_key"))

	err := versions.Create(context.Background(), *mustPlanVersionFor(t))
	if !errors.Is(err, commerce.ErrPlanVersionNumberTaken) {
		t.Fatalf("create plan version under plan_versions_version_number_key returned %v, want commerce.ErrPlanVersionNumberTaken", err)
	}
}

func TestAddGrantDefinitionMapsTheScopeConstraintToItsSentinel(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	versions := NewPlanVersions(store)
	f.failOn(evExec, pgUniqueViolationErr("plan_grant_definitions_scope_dimension_key"))

	err := versions.AddGrantDefinition(context.Background(), *mustGrantDefinitionFor(t))
	if !errors.Is(err, commerce.ErrGrantScopeTaken) {
		t.Fatalf("add grant definition under plan_grant_definitions_scope_dimension_key returned %v, want commerce.ErrGrantScopeTaken", err)
	}
}

func TestAddGrantDefinitionRefusesAVersionThatLeftDraft(t *testing.T) {
	_, db := registerFake(t)
	store := New(db)
	versions := NewPlanVersions(store)

	// The fake answers zero rows affected: the insert's SELECT found no
	// version still in draft, which is the guard's own verdict — the edit
	// lost to a publication, and the domain's word for it is
	// ErrVersionNotEditable. A verdict is not an error at the driver.
	err := versions.AddGrantDefinition(context.Background(), *mustGrantDefinitionFor(t))
	if !errors.Is(err, commerce.ErrVersionNotEditable) {
		t.Fatalf("add grant definition against a zero-rows answer returned %v, want commerce.ErrVersionNotEditable", err)
	}
}

func TestCreateEntitlementWrapsTheOncePerCycleRefusalUnmapped(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	entitlements := NewEntitlements(store)
	f.failOn(evExec, pgUniqueViolationErr("entitlements_grant_once_per_cycle"))

	err := entitlements.Create(context.Background(), *mustEntitlementFor(t))
	if err == nil {
		t.Fatal("create entitlement under entitlements_grant_once_per_cycle returned nil, want the refusal wrapped")
	}
	for _, sentinel := range []error{
		commerce.ErrPlanNameTaken,
		commerce.ErrPlanVersionNumberTaken,
		commerce.ErrGrantScopeTaken,
	} {
		if errors.Is(err, sentinel) {
			t.Fatalf("the once-per-cycle refusal was mapped to %v — only a buggy roll can fire it, and the guarded cycle advance is the real guard", sentinel)
		}
	}
}

func TestAUniquenessViolationWithoutAConstraintNamePropagatesUnmapped(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	plans := NewPlans(store)
	// The structural seam identity classifies on carries the SQLSTATE but no
	// constraint name; the commerce mapping refuses to guess which business
	// rule fired and lets the failure travel wrapped instead.
	f.failOn(evExec, &sqlStateError{state: pgUniqueViolation})

	err := plans.Create(context.Background(), *mustPlanFor(t))
	if err == nil || errors.Is(err, commerce.ErrPlanNameTaken) {
		t.Fatalf("create plan under an unnamed 23505 returned %v, want the failure wrapped and unmapped", err)
	}
}

func TestADriverFailureThatIsNotAUniquenessViolationPropagatesUnmapped(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	versions := NewPlanVersions(store)
	// A foreign-key refusal: an impossible graph, not a business rule, under
	// a constraint name no insert in this file loses to.
	boom := &pgconn.PgError{Code: "23503", ConstraintName: "plans_pkey"}
	f.failOn(evExec, boom)

	err := versions.AddGrantDefinition(context.Background(), *mustGrantDefinitionFor(t))
	if errors.Is(err, commerce.ErrGrantScopeTaken) || errors.Is(err, commerce.ErrVersionNotEditable) {
		t.Fatalf("a foreign-key refusal was mapped to a domain sentinel: %v", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("add grant definition returned %v, want the driver failure wrapped and unmapped", err)
	}
}

// TestEveryGuardedMoveReportsALostSwapAsNotApplied walks each of the
// port's guarded moves against the fake's zero-rows answer: the statement is
// the verdict, matched-nothing is false, and false is never an error — the
// caller re-reads and the next pass settles it.
func TestEveryGuardedMoveReportsALostSwapAsNotApplied(t *testing.T) {
	_, db := registerFake(t)
	store := New(db)
	versions := NewPlanVersions(store)
	subscriptions := NewSubscriptions(store)
	entitlements := NewEntitlements(store)
	payg := NewPaygAccounts(store)

	versionID := commerce.PlanVersionID("v0000000-0000-0000-0000-0000000000v1")
	subscriptionID := commerce.SubscriptionID("s0000000-0000-0000-0000-0000000000s1")
	entitlementID := commerce.EntitlementID("e0000000-0000-0000-0000-0000000000e1")
	accountID := commerce.AccountID("a0000000-0000-0000-0000-0000000000a1")
	past := commerceClock.Add(-time.Hour)
	future := commerceClock.Add(time.Hour)

	moves := map[string]func() (bool, error){
		"publish plan version": func() (bool, error) {
			return versions.Publish(context.Background(), versionID, commerce.PlanVersionDraft, commerceClock)
		},
		"retire plan version": func() (bool, error) {
			return versions.Retire(context.Background(), versionID, commerce.PlanVersionPublished, commerceClock)
		},
		"transition subscription": func() (bool, error) {
			return subscriptions.TransitionState(context.Background(), subscriptionID, commerce.SubscriptionActive, commerce.SubscriptionSuspended, commerceClock)
		},
		"schedule cancellation": func() (bool, error) {
			return subscriptions.ScheduleCancellation(context.Background(), subscriptionID, future, commerceClock)
		},
		"cancel subscription": func() (bool, error) {
			return subscriptions.Cancel(context.Background(), subscriptionID, commerceClock)
		},
		"activate subscription": func() (bool, error) {
			return subscriptions.Activate(context.Background(), subscriptionID, past, future, commerceClock)
		},
		"roll subscription": func() (bool, error) {
			return subscriptions.Roll(context.Background(), subscriptionID, 1, past, future, commerceClock)
		},
		"expire subscription": func() (bool, error) {
			return subscriptions.Expire(context.Background(), subscriptionID, commerceClock)
		},
		"expire entitlement": func() (bool, error) {
			return entitlements.Expire(context.Background(), entitlementID, commerceClock)
		},
		"assign funding bucket": func() (bool, error) {
			return payg.AssignFundingBucket(context.Background(), accountID, "f0000000-0000-0000-7000-000000000001", commerceClock)
		},
	}
	for name, move := range moves {
		applied, err := move()
		if err != nil {
			t.Errorf("%s against a zero-rows answer failed: %v — losing a swap is a verdict, never an error", name, err)
			continue
		}
		if applied {
			t.Errorf("%s reported a swap applied that the driver said matched no rows", name)
		}
	}
}

func TestCommerceRepositoriesRefuseANilStore(t *testing.T) {
	for name, build := range map[string]func(){
		"clock":         func() { NewClock(nil) },
		"plans":         func() { NewPlans(nil) },
		"plan versions": func() { NewPlanVersions(nil) },
		"subscriptions": func() { NewSubscriptions(nil) },
		"entitlements":  func() { NewEntitlements(nil) },
		"payg accounts": func() { NewPaygAccounts(nil) },
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

// TestNowWrapsTheReadsFailure proves the one thing the fake driver can
// honestly answer about the clock: that a failed read leaves this file
// wrapped around it, in this package's voice, and does not leak the driver's
// raw text upward. That the instant is transaction_timestamp() at all — and
// stable inside a unit of work — is the database's answer, and the
// integration tier asks it there.
func TestNowWrapsTheReadsFailure(t *testing.T) {
	_, db := registerFake(t)
	store := New(db)
	clock := NewClock(store)

	_, err := clock.Now(context.Background())
	if err == nil {
		t.Fatal("Now against a driver that refuses statements returned nil, want the read's failure")
	}
	if !strings.Contains(err.Error(), "postgres: read the database clock") {
		t.Fatalf("Now returned %v, want the failure wrapped as this adapter's own", err)
	}
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// mustPlanFor returns a valid plan aggregate for adapter tests.
func mustPlanFor(t *testing.T) *commerce.Plan {
	t.Helper()
	plan, err := commerce.NewPlan("p0000000-0000-7000-8000-0000000000p1", "adapter probe", commerceClock)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	return plan
}

// mustPlanVersionFor returns a valid plan version in its birth state for
// adapter tests.
func mustPlanVersionFor(t *testing.T) *commerce.PlanVersion {
	t.Helper()
	version, err := commerce.NewPlanVersion("v0000000-0000-7000-8000-0000000000v1",
		"p0000000-0000-7000-8000-0000000000p1", 1, commerce.RecurringPeriodCalendarMonth, 4900, commerceClock)
	if err != nil {
		t.Fatalf("NewPlanVersion: %v", err)
	}
	return version
}

// mustGrantDefinitionFor returns a valid grant definition for adapter tests.
func mustGrantDefinitionFor(t *testing.T) *commerce.GrantDefinition {
	t.Helper()
	definition, err := commerce.NewGrantDefinition("g0000000-0000-7000-8000-0000000000g1",
		"v0000000-0000-7000-8000-0000000000v1", commerce.WildcardGroupName, commerce.DimensionCost, 500)
	if err != nil {
		t.Fatalf("NewGrantDefinition: %v", err)
	}
	return definition
}

// mustSubscriptionFor returns a valid subscription in its birth state,
// pending with the cycle fields unset, for adapter tests.
func mustSubscriptionFor(t *testing.T) *commerce.Subscription {
	t.Helper()
	subscription, err := commerce.NewSubscription("s0000000-0000-7000-8000-0000000000s1",
		"a0000000-0000-0000-0000-0000000000a1", "v0000000-0000-7000-8000-0000000000v1",
		commerceClock.Add(-time.Minute), true, commerceClock)
	if err != nil {
		t.Fatalf("NewSubscription: %v", err)
	}
	return subscription
}

// mustEntitlementFor returns a valid entitlement for adapter tests. The
// scope version is a blind reference to another plane's row, so the domain
// only ever validates its v7 form — minted here from a real commerce minter.
func mustEntitlementFor(t *testing.T) *commerce.Entitlement {
	t.Helper()
	scopeVersion, err := commerce.NewEntitlementID()
	if err != nil {
		t.Fatalf("NewEntitlementID: %v", err)
	}
	entitlement, err := commerce.NewEntitlement("e0000000-0000-7000-8000-0000000000e1",
		"s0000000-0000-7000-8000-0000000000s1", 1, *mustGrantDefinitionFor(t),
		commerce.AliasGroupVersionID(scopeVersion),
		commerceClock.Add(-time.Hour), commerceClock.Add(29*24*time.Hour), commerceClock)
	if err != nil {
		t.Fatalf("NewEntitlement: %v", err)
	}
	return entitlement
}
