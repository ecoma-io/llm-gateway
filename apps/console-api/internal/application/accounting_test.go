package application

import (
	"context"
	"errors"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The accounting use cases, pinned against the fake world: every movement is
// one unit of work, every redelivery converges or is named, a settlement
// header never outlives its legs, and no path anywhere drives a balance
// negative. The concurrency the fake cannot stage — two writers, one row —
// is the integration suite's; what is staged here is every decision the use
// cases make around the statements.

func TestNewAccountingRequiresEveryPort(t *testing.T) {
	world := newAccountingWorld(t)
	build := func(miss int) *Accounting {
		ports := []any{
			fakeAccountingStore{world: world},
			fakeFundingBuckets{world: world},
			fakeFundingLedger{world: world},
			fakeAccountingSettlements{world: world},
			fakeFundingProjections{world: world},
			fakeAccountingPayg{world: world},
			fakeAccountingClock{world: world},
		}
		ports[miss] = nil
		return NewAccounting(
			ports[0].(persistence.Store),
			ports[1].(persistence.FundingBuckets),
			ports[2].(persistence.FundingLedger),
			ports[3].(persistence.Settlements),
			ports[4].(persistence.FundingProjections),
			ports[5].(persistence.PaygAccounts),
			ports[6].(persistence.Clock),
		)
	}
	for miss := 0; miss < 7; miss++ {
		if func() (panicked bool) {
			defer func() { panicked = recover() != nil }()
			build(miss)
			return panicked
		}() {
			continue
		}
		t.Fatalf("NewAccounting accepted a nil port at position %d; wiring defects must be loud", miss)
	}
}

func TestFundEntitlementRefusesToRunOutsideAUnitOfWork(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)

	entitlementID, err := commerce.NewEntitlementID()
	if err != nil {
		t.Fatalf("mint entitlement id: %v", err)
	}
	if err := use.FundEntitlement(t.Context(), entitlementID, 5000); !errors.Is(err, ErrUnitOfWorkRequired) {
		t.Fatalf("FundEntitlement on a bare context = %v, want ErrUnitOfWorkRequired", err)
	}
	if len(world.buckets) != 0 || len(world.legs) != 0 {
		t.Fatalf("the refused funding left state behind: %d buckets, %d legs", len(world.buckets), len(world.legs))
	}
}

func TestFundEntitlementGrantsOnceAndConvergesOnARetry(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)

	entitlementID, err := commerce.NewEntitlementID()
	if err != nil {
		t.Fatalf("mint entitlement id: %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := use.store.WithinTx(t.Context(), func(ctx context.Context) error {
			return use.FundEntitlement(ctx, entitlementID, 5000)
		}); err != nil {
			t.Fatalf("fund attempt %d: %v", attempt, err)
		}
	}
	if len(world.legs) != 1 {
		t.Fatalf("two rolls wrote %d grant legs, want 1 — the retry converged", len(world.legs))
	}
	bucketID := world.bucketByEntitlement[accounting.EntitlementID(entitlementID)]
	bucket := world.buckets[bucketID]
	if bucket.Settled != accounting.Balance(5000) || bucket.Available != accounting.Balance(5000) {
		t.Fatalf("bucket after funding: settled %d, available %d, want 5000 each",
			bucket.Settled, bucket.Available)
	}
}

func TestFundEntitlementRefusesAGrantlessAmount(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)

	entitlementID, err := commerce.NewEntitlementID()
	if err != nil {
		t.Fatalf("mint entitlement id: %v", err)
	}
	err = use.store.WithinTx(t.Context(), func(ctx context.Context) error {
		return use.FundEntitlement(ctx, entitlementID, 0)
	})
	if !errors.Is(err, accounting.ErrInvalidAmount) {
		t.Fatalf("a grantless definition = %v, want ErrInvalidAmount: funding silence is a roll defect", err)
	}
	if len(world.buckets) != 0 {
		t.Fatalf("the failed funding left %d buckets behind; the roll must roll back", len(world.buckets))
	}
}

func TestOpenAccountFundingCreatesTheBucketAndFilesTheReferenceOnce(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)
	accountID := commerce.AccountID("account-payg-1")

	first, err := use.OpenAccountFunding(t.Context(), accountID)
	if err != nil {
		t.Fatalf("open account funding: %v", err)
	}
	second, err := use.OpenAccountFunding(t.Context(), accountID)
	if err != nil {
		t.Fatalf("re-open account funding: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("two calls produced buckets %s and %s; the account has one PAYG bucket, ever",
			first.ID, second.ID)
	}
	if len(world.buckets) != 1 {
		t.Fatalf("%d buckets on file, want 1", len(world.buckets))
	}
	if world.paygRows[accountID].FundingBucketID != commerce.FundingBucketID(first.ID) {
		t.Fatalf("payg reference = %s, want the bucket the use case created", world.paygRows[accountID].FundingBucketID)
	}
}

func TestOpenAccountFundingNamesAConflictingReference(t *testing.T) {
	world := newAccountingWorld(t)
	accountID := commerce.AccountID("account-payg-2")

	// The row already points elsewhere — the defect the write-once guard
	// exists to stop. No bucket answers the account lookup, so the use case
	// walks the create path, loses the reference filing, re-reads, and names
	// what landed instead of absorbing it.
	world.paygRows[accountID] = commerce.AccountPayg{
		AccountID:       accountID,
		FundingBucketID: "0198f0a4-3f6c-7000-8000-0000000000c1",
	}

	use := newAccounting(world)
	_, err := use.OpenAccountFunding(t.Context(), accountID)
	if !errors.Is(err, accounting.ErrInvalidTransition) {
		t.Fatalf("open funding against a conflicting reference = %v, want ErrInvalidTransition", err)
	}
	if len(world.buckets) != 0 {
		t.Fatalf("the failed opening left %d buckets behind; the rollback must erase the fresh bucket", len(world.buckets))
	}
}

func TestTopUpConvergesAndConflictsOnTheCommandKey(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)
	bucket := world.seedAccountFunding(t, "account-payg-1")

	if _, err := use.TopUp(t.Context(), bucket.ID, 5000, "cmd-1"); err != nil {
		t.Fatalf("top up: %v", err)
	}
	after, err := use.TopUp(t.Context(), bucket.ID, 5000, "cmd-1")
	if err != nil {
		t.Fatalf("re-run of the same top up: %v", err)
	}
	if after.Settled != accounting.Balance(5000) {
		t.Fatalf("settled after the converged retry = %d, want 5000 — the key moved money once", after.Settled)
	}
	if len(world.legs) != 1 {
		t.Fatalf("%d legs on file after a retried top up, want 1", len(world.legs))
	}

	if _, err := use.TopUp(t.Context(), bucket.ID, 6000, "cmd-1"); !errors.Is(err, accounting.ErrDuplicateCommand) {
		t.Fatalf("the same key with another amount = %v, want ErrDuplicateCommand", err)
	}
}

func TestTopUpRefusesAClosedBucket(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)
	bucket := world.seedAccountFunding(t, "account-payg-1")

	if err := use.CloseBucket(t.Context(), bucket.ID); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := use.TopUp(t.Context(), bucket.ID, 5000, "cmd-1"); !errors.Is(err, accounting.ErrBucketClosed) {
		t.Fatalf("top up a closed bucket = %v, want ErrBucketClosed", err)
	}
}

func TestTopUpRefusesACycleBucket(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)
	entitlementID, err := commerce.NewEntitlementID()
	if err != nil {
		t.Fatalf("mint entitlement id: %v", err)
	}
	bucket := world.seedEntitlementFunding(t, entitlementID, 100)

	// A cycle is granted by its roll, never topped up: the ownership rule
	// is part of the transition the use case runs, so the funder for the
	// other owner is refused in the transition's own words.
	if _, err := use.TopUp(t.Context(), bucket.ID, 5000, "cmd-1"); !errors.Is(err, accounting.ErrInvalidTransition) {
		t.Fatalf("top up a cycle bucket = %v, want ErrInvalidTransition", err)
	}
	if len(world.legs) != 1 {
		t.Fatalf("%d legs on file after the refused top up, want 1 — the grant only", len(world.legs))
	}
}

func TestHoldGuardsTheAvailableBalanceAndTheReservationKey(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)
	bucket := world.seedAccountFunding(t, "account-payg-1")
	if _, err := use.TopUp(t.Context(), bucket.ID, 100, "cmd-1"); err != nil {
		t.Fatalf("top up: %v", err)
	}

	held, err := use.Hold(t.Context(), bucket.ID, "3d6f8a20-93e1-4c2b-9a7d-5f1e2d3c4b5a", 80)
	if err != nil {
		t.Fatalf("hold 80: %v", err)
	}
	if held.Held != accounting.Balance(80) || held.Available != accounting.Balance(20) {
		t.Fatalf("after the hold: held %d, available %d, want 80, 20", held.Held, held.Available)
	}

	retried, err := use.Hold(t.Context(), bucket.ID, "3d6f8a20-93e1-4c2b-9a7d-5f1e2d3c4b5a", 80)
	if err != nil {
		t.Fatalf("redelivered hold: %v", err)
	}
	if retried.Held != accounting.Balance(80) {
		t.Fatalf("held after the redelivery = %d, want 80 — the reservation moved money once", retried.Held)
	}

	if _, err := use.Hold(t.Context(), bucket.ID, "3d6f8a20-93e1-4c2b-9a7d-5f1e2d3c4b5a", 30); !errors.Is(err, accounting.ErrDuplicateMovement) {
		t.Fatalf("the same reservation at another amount = %v, want ErrDuplicateMovement", err)
	}
	if _, err := use.Hold(t.Context(), bucket.ID, "3d6f8a20-93e1-4c2b-9a7d-5f1e2d3c4b5b", 30); !errors.Is(err, accounting.ErrInsufficientAvailable) {
		t.Fatalf("hold 30 against available 20 = %v, want ErrInsufficientAvailable", err)
	}
	if len(world.legs) != 2 {
		t.Fatalf("%d legs on file, want 2 — refusals never move money", len(world.legs))
	}
}

func TestARacingRedeliveryConvergesThroughTheCollision(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)
	bucket := world.seedAccountFunding(t, "account-payg-1")

	// The other writer's topup lands between this caller's pre-read and its
	// append: the port's savepoint keeps the unit of work alive, the duplicate
	// sentinel comes back, and the re-read finds the winner — same key, same
	// payload, so the caller converges on it instead of refusing.
	entryID, err := accounting.NewLedgerEntryID()
	if err != nil {
		t.Fatalf("mint the racer's entry id: %v", err)
	}
	amount, err := accounting.NewAmount(5000)
	if err != nil {
		t.Fatalf("amount: %v", err)
	}
	racer, err := accounting.NewTopupEntry(entryID, bucket.ID, amount, "cmd-1", world.now)
	if err != nil {
		t.Fatalf("build the racer's leg: %v", err)
	}
	world.racingLeg = &racer

	after, err := use.TopUp(t.Context(), bucket.ID, 5000, "cmd-1")
	if err != nil {
		t.Fatalf("the racing redelivery must converge, got %v", err)
	}
	if after.Settled != accounting.Balance(5000) {
		t.Fatalf("settled after converging on the racer = %d, want 5000", after.Settled)
	}
	if len(world.legs) != 1 {
		t.Fatalf("%d legs on file, want 1 — the loser's leg never lands", len(world.legs))
	}
	if world.legs[0].ID != racer.ID {
		t.Fatalf("the leg on file is %s, want the racer's %s", world.legs[0].ID, racer.ID)
	}
}

func TestARacingRedeliveryWithADifferentPayloadIsNamed(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)
	bucket := world.seedAccountFunding(t, "account-payg-1")

	// Same shape as the convergence race, but the winner moved a different
	// amount: the re-read finds a differing original and the caller keeps the
	// defect, not the money.
	entryID, err := accounting.NewLedgerEntryID()
	if err != nil {
		t.Fatalf("mint the racer's entry id: %v", err)
	}
	amount, err := accounting.NewAmount(9000)
	if err != nil {
		t.Fatalf("amount: %v", err)
	}
	racer, err := accounting.NewTopupEntry(entryID, bucket.ID, amount, "cmd-1", world.now)
	if err != nil {
		t.Fatalf("build the racer's leg: %v", err)
	}
	world.racingLeg = &racer

	if _, err := use.TopUp(t.Context(), bucket.ID, 5000, "cmd-1"); !errors.Is(err, accounting.ErrDuplicateCommand) {
		t.Fatalf("the collision with a different payload = %v, want ErrDuplicateCommand", err)
	}
	if len(world.legs) != 1 || world.legs[0].Amount != accounting.Amount(9000) {
		t.Fatalf("the ledger after the refused call = %+v, want only the racer's 9000 leg", world.legs)
	}
}

func TestReleaseHoldReturnsExactlyWhatWasHeld(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)
	bucket := world.seedAccountFunding(t, "account-payg-1")
	if _, err := use.TopUp(t.Context(), bucket.ID, 100, "cmd-1"); err != nil {
		t.Fatalf("top up: %v", err)
	}
	reservation := accounting.ReservationID("3d6f8a20-93e1-4c2b-9a7d-5f1e2d3c4b5a")
	if _, err := use.Hold(t.Context(), bucket.ID, reservation, 40); err != nil {
		t.Fatalf("hold: %v", err)
	}

	if _, err := use.ReleaseHold(t.Context(), bucket.ID, reservation, 50); !errors.Is(err, accounting.ErrInsufficientHeld) {
		t.Fatalf("release 50 against held 40 = %v, want ErrInsufficientHeld", err)
	}

	after, err := use.ReleaseHold(t.Context(), bucket.ID, reservation, 40)
	if err != nil {
		t.Fatalf("release 40: %v", err)
	}
	if after.Held != 0 || after.Available != accounting.Balance(100) {
		t.Fatalf("after the release: held %d, available %d, want 0, 100", after.Held, after.Available)
	}

	if _, err := use.ReleaseHold(t.Context(), bucket.ID, reservation, 40); err != nil {
		t.Fatalf("re-run of the same release must converge: %v", err)
	}
	if after.Held != 0 || after.Available != accounting.Balance(100) {
		t.Fatalf("the ledger after the converged re-release: held %d, available %d, want 0, 100",
			after.Held, after.Available)
	}
}

func TestAdjustRefusesToOverdrawAndConvergesWhenKeyed(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)
	bucket := world.seedAccountFunding(t, "account-payg-1")
	if _, err := use.TopUp(t.Context(), bucket.ID, 100, "cmd-1"); err != nil {
		t.Fatalf("top up: %v", err)
	}
	original := world.legs[0].ID

	// The no-credit rule at the use case's edge: a correction that would take
	// settled below zero is refused, never recorded.
	if _, err := use.Adjust(t.Context(), bucket.ID, -150, 0, "misposted grant",
		original, "ops-1", ""); !errors.Is(err, accounting.ErrInvalidAdjustment) {
		t.Fatalf("settled 100 − 150 = %v, want ErrInvalidAdjustment", err)
	}

	after, err := use.Adjust(t.Context(), bucket.ID, -30, 0, "misposted grant", original, "ops-1", "fix-1")
	if err != nil {
		t.Fatalf("adjust −30: %v", err)
	}
	if after.Settled != accounting.Balance(70) || after.Available != accounting.Balance(70) {
		t.Fatalf("after the correction: settled %d, available %d, want 70, 70", after.Settled, after.Available)
	}

	retried, err := use.Adjust(t.Context(), bucket.ID, -30, 0, "misposted grant", original, "ops-1", "fix-1")
	if err != nil {
		t.Fatalf("re-run of the keyed correction: %v", err)
	}
	if retried.Settled != accounting.Balance(70) || len(world.legs) != 2 {
		t.Fatalf("the retried correction converged to settled %d over %d legs, want 70 over 2",
			retried.Settled, len(world.legs))
	}

	if _, err := use.Adjust(t.Context(), bucket.ID, -10, 0, "different correction", original, "ops-1", "fix-1"); !errors.Is(err, accounting.ErrDuplicateCommand) {
		t.Fatalf("the same key with other deltas = %v, want ErrDuplicateCommand", err)
	}

	// Unkeyed corrections run without a convergence lookup — an operator's
	// one-off is still a legal fact.
	if _, err := use.Adjust(t.Context(), bucket.ID, -10, 0, "second mispost", original, "ops-1", ""); err != nil {
		t.Fatalf("unkeyed adjustment: %v", err)
	}
	if world.legs[len(world.legs)-1].SettledDelta != accounting.Delta(-10) {
		t.Fatalf("the unkeyed correction did not land as stated: %+v", world.legs[len(world.legs)-1])
	}
}

func TestSettleIsExactlyOnceAndAtomic(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)

	firstID, err := commerce.NewEntitlementID()
	if err != nil {
		t.Fatalf("mint entitlement id: %v", err)
	}
	secondID, err := commerce.NewEntitlementID()
	if err != nil {
		t.Fatalf("mint entitlement id: %v", err)
	}
	first := world.seedEntitlementFunding(t, firstID, 20)
	second := world.seedEntitlementFunding(t, secondID, 40)

	reservation := accounting.ReservationID("3d6f8a20-93e1-4c2b-9a7d-5f1e2d3c4b5a")
	for _, bucket := range []accounting.Bucket{first, second} {
		if _, err := use.Hold(t.Context(), bucket.ID, reservation, bucket.Settled.Int64()); err != nil {
			t.Fatalf("hold on %s: %v", bucket.ID, err)
		}
	}

	firstAlloc, err := accounting.NewAllocation(first.ID, reservation, 20)
	if err != nil {
		t.Fatalf("allocation: %v", err)
	}
	firstAlloc, err = firstAlloc.SettleConsumed(20, accounting.PriceSnapshot{RevisionID: "rev-1", InputUnitPrice: 1, OutputUnitPrice: 1})
	if err != nil {
		t.Fatalf("settle S1: %v", err)
	}
	secondAlloc, err := accounting.NewAllocation(second.ID, reservation, 40)
	if err != nil {
		t.Fatalf("allocation: %v", err)
	}
	secondAlloc, err = secondAlloc.SettleConsumed(10, accounting.PriceSnapshot{RevisionID: "rev-1", InputUnitPrice: 1, OutputUnitPrice: 1})
	if err != nil {
		t.Fatalf("settle S2: %v", err)
	}

	result, err := use.Settle(t.Context(), "req-1", []accounting.Allocation{firstAlloc, secondAlloc})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if result.Converged {
		t.Fatal("the first acknowledgement must not read as converged")
	}
	if result.Settlement.SettledTotal != accounting.Amount(30) {
		t.Fatalf("settled total = %d, want 30", result.Settlement.SettledTotal)
	}
	drained := world.buckets[first.ID]
	if drained.Settled != 0 || drained.Held != 0 || drained.Available != 0 {
		t.Fatalf("S1 after the settle: (%d, %d, %d), want all zero", drained.Settled, drained.Held, drained.Available)
	}
	tail := world.buckets[second.ID]
	if tail.Settled != accounting.Balance(30) || tail.Held != 0 || tail.Available != accounting.Balance(30) {
		t.Fatalf("S2 after the settle: (%d, %d, %d), want 30, 0, 30 — the consume spent 10 and vacated its hold, the release returned the rest",
			tail.Settled, tail.Held, tail.Available)
	}

	// The re-acknowledgement: same request, same total — converged, nothing
	// moved again.
	replayed, err := use.Settle(t.Context(), "req-1", []accounting.Allocation{firstAlloc, secondAlloc})
	if err != nil {
		t.Fatalf("re-acknowledgement: %v", err)
	}
	if !replayed.Converged || replayed.Settlement.SettledTotal != accounting.Amount(30) {
		t.Fatalf("re-acknowledgement = (converged %v, total %d), want (true, 30)",
			replayed.Converged, replayed.Settlement.SettledTotal)
	}
	if len(world.legs) != 7 { // 2 grants + 2 holds + 2 consume + 1 release
		t.Fatalf("%d legs on file after the re-acknowledgement, want 7", len(world.legs))
	}

	// The same request at a different total is the conflict the exactly-once
	// key exists to name.
	secondAlloc.Consumed = 15
	if _, err := use.Settle(t.Context(), "req-1", []accounting.Allocation{firstAlloc, secondAlloc}); !errors.Is(err, accounting.ErrSettlementConflict) {
		t.Fatalf("re-acknowledgement at total 35 = %v, want ErrSettlementConflict", err)
	}
}

func TestASettlementThatFailsMidPlanLeavesNothingBehind(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)

	entitlementID, err := commerce.NewEntitlementID()
	if err != nil {
		t.Fatalf("mint entitlement id: %v", err)
	}
	bucket := world.seedEntitlementFunding(t, entitlementID, 100)
	reservation := accounting.ReservationID("3d6f8a20-93e1-4c2b-9a7d-5f1e2d3c4b5a")
	if _, err := use.Hold(t.Context(), bucket.ID, reservation, 40); err != nil {
		t.Fatalf("hold: %v", err)
	}

	alloc, err := accounting.NewAllocation(bucket.ID, reservation, 40)
	if err != nil {
		t.Fatalf("allocation: %v", err)
	}
	alloc, err = alloc.SettleConsumed(25, accounting.PriceSnapshot{RevisionID: "rev-1", InputUnitPrice: 1, OutputUnitPrice: 1})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}

	// The consume leg lands, the release tail is refused — a persistence
	// failure arrives from below — and the whole unit of work rolls back:
	// no header, no legs, no balance moved.
	world.appendFailOnCall = world.appendCalls + 2
	if _, err := use.Settle(t.Context(), "req-broken", []accounting.Allocation{alloc}); err == nil {
		t.Fatal("a settlement whose tail cannot land must fail")
	}
	if len(world.settlements) != 0 {
		t.Fatalf("settlements on file after the failed plan: %d, want 0 — the header dies with its legs", len(world.settlements))
	}
	if len(world.legs) != 2 {
		t.Fatalf("%d legs on file after the failed plan, want 2 (the grant and the hold)", len(world.legs))
	}
	after := world.buckets[bucket.ID]
	if after.Settled != accounting.Balance(100) || after.Held != accounting.Balance(40) {
		t.Fatalf("bucket after the failed plan: settled %d, held %d, want 100, 40", after.Settled, after.Held)
	}
}

func TestCloseBucketIsBlockedWhileFundsAreHeld(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)
	bucket := world.seedAccountFunding(t, "account-payg-1")
	if _, err := use.TopUp(t.Context(), bucket.ID, 100, "cmd-1"); err != nil {
		t.Fatalf("top up: %v", err)
	}
	reservation := accounting.ReservationID("3d6f8a20-93e1-4c2b-9a7d-5f1e2d3c4b5a")
	if _, err := use.Hold(t.Context(), bucket.ID, reservation, 40); err != nil {
		t.Fatalf("hold: %v", err)
	}

	if err := use.CloseBucket(t.Context(), bucket.ID); !errors.Is(err, accounting.ErrBucketCloseBlocked) {
		t.Fatalf("close with 40 held = %v, want ErrBucketCloseBlocked", err)
	}
	if world.buckets[bucket.ID].Status != accounting.BucketActive {
		t.Fatal("the refused close must leave the bucket active")
	}

	if _, err := use.ReleaseHold(t.Context(), bucket.ID, reservation, 40); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := use.CloseBucket(t.Context(), bucket.ID); err != nil {
		t.Fatalf("close after the release: %v", err)
	}
	if world.buckets[bucket.ID].Status != accounting.BucketClosed {
		t.Fatal("the bucket must read closed after the close")
	}

	// Closing a closed bucket is the converged no-op.
	if err := use.CloseBucket(t.Context(), bucket.ID); err != nil {
		t.Fatalf("close of a closed bucket: %v", err)
	}
}

func TestReconcileKeepsTheCacheHonest(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)
	bucket := world.seedAccountFunding(t, "account-payg-1")
	if _, err := use.TopUp(t.Context(), bucket.ID, 100, "cmd-1"); err != nil {
		t.Fatalf("top up: %v", err)
	}
	reservation := accounting.ReservationID("3d6f8a20-93e1-4c2b-9a7d-5f1e2d3c4b5a")
	if _, err := use.Hold(t.Context(), bucket.ID, reservation, 40); err != nil {
		t.Fatalf("hold: %v", err)
	}

	report, err := use.ReconcileBucket(t.Context(), bucket.ID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !report.Consistent {
		t.Fatalf("a healthy bucket must reconcile: %+v against %+v", report.Derivation, report.Bucket)
	}
	if report.Derivation.Legs != 2 || report.Derivation.Held != accounting.Balance(40) {
		t.Fatalf("derivation = %+v, want held 40 over 2 legs", report.Derivation)
	}

	// A tampered cache: the legs win, and the report says so.
	tampered := world.buckets[bucket.ID]
	tampered.Settled = accounting.Balance(999)
	world.buckets[bucket.ID] = tampered

	report, err = use.ReconcileBucket(t.Context(), bucket.ID)
	if err != nil {
		t.Fatalf("reconcile after tampering: %v", err)
	}
	if report.Consistent {
		t.Fatal("a tampered cache must read inconsistent")
	}
	if report.Derivation.Settled != accounting.Balance(100) {
		t.Fatalf("the derivation still says settled %d, want the legs' 100", report.Derivation.Settled)
	}
}

func TestTheWaterfallRunsEndToEndThroughTheUseCases(t *testing.T) {
	world := newAccountingWorld(t)
	use := newAccounting(world)
	bucket := world.seedAccountFunding(t, "account-payg-1")

	// Grant → hold → consume-in-settle → the balances the ADR's example names,
	// with the sequence stamped in land order and the version counting the
	// bucket's writers.
	if _, err := use.TopUp(t.Context(), bucket.ID, 100, "cmd-1"); err != nil {
		t.Fatalf("top up: %v", err)
	}
	reservation := accounting.ReservationID("3d6f8a20-93e1-4c2b-9a7d-5f1e2d3c4b5a")
	if _, err := use.Hold(t.Context(), bucket.ID, reservation, 40); err != nil {
		t.Fatalf("hold: %v", err)
	}
	alloc, err := accounting.NewAllocation(bucket.ID, reservation, 40)
	if err != nil {
		t.Fatalf("allocation: %v", err)
	}
	alloc, err = alloc.SettleConsumed(25, accounting.PriceSnapshot{RevisionID: "rev-1", InputUnitPrice: 2, OutputUnitPrice: 3})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, err := use.Settle(t.Context(), "req-1", []accounting.Allocation{alloc}); err != nil {
		t.Fatalf("settle: %v", err)
	}

	after := world.buckets[bucket.ID]
	if after.Settled != accounting.Balance(75) || after.Held != 0 || after.Available != accounting.Balance(75) {
		t.Fatalf("after the waterfall: (%d, %d, %d), want 75, 0, 75", after.Settled, after.Held, after.Available)
	}
	if len(world.legs) != 4 { // topup, hold, consume, release
		t.Fatalf("%d legs on file, want 4", len(world.legs))
	}
	for i, leg := range world.legs {
		if leg.Sequence != int64(i+1) {
			t.Fatalf("leg %d carries sequence %d; a bucket's order is its counter, 1..%d",
				i, leg.Sequence, len(world.legs))
		}
	}
	if world.legs[2].Kind != accounting.KindConsume || world.legs[3].Kind != accounting.KindRelease {
		t.Fatalf("settlement legs landed as (%s, %s), want consume before release",
			world.legs[2].Kind, world.legs[3].Kind)
	}
	if world.legs[2].Price == nil || world.legs[2].Price.RevisionID != "rev-1" {
		t.Fatalf("the consume leg lost its price snapshot: %+v", world.legs[2].Price)
	}
	if world.legs[2].SettledDelta != accounting.Delta(-25) || world.legs[2].HeldDelta != accounting.Delta(-25) {
		t.Fatalf("consume deltas = (%d, %d), want (−25, −25)",
			world.legs[2].SettledDelta, world.legs[2].HeldDelta)
	}
	if world.legs[3].SettledDelta != 0 || world.legs[3].HeldDelta != accounting.Delta(-15) {
		t.Fatalf("release deltas = (%d, %d), want (0, −15)",
			world.legs[3].SettledDelta, world.legs[3].HeldDelta)
	}
}
