package accounting

import (
	"errors"
	"math"
	"testing"
)

// A settlement is exactly-once by request id, and its plan is legs in the
// caller's waterfall order with the header's total computed from the legs it
// wrote. This file pins the builder against ADR 0004's worked example and
// every input a settlement must refuse.

func mustSettlementID(t *testing.T) SettlementID {
	t.Helper()
	id, err := NewSettlementID()
	if err != nil {
		t.Fatalf("mint settlement id: %v", err)
	}
	return id
}

func mustAllocation(t *testing.T, bucketID FundingBucketID, held int64) Allocation {
	t.Helper()
	a, err := NewAllocation(bucketID, ReservationID(validV4), mustAmount(t, held))
	if err != nil {
		t.Fatalf("new allocation: %v", err)
	}
	return a
}

func TestAllocationValidation(t *testing.T) {
	if _, err := NewAllocation("", ReservationID(validV4), mustAmount(t, 10)); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("blank bucket allocation = %v, want ErrInvalidReference", err)
	}
	if _, err := NewAllocation(mustBucketID(t), "not-a-uuid", mustAmount(t, 10)); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("malformed reservation allocation = %v, want ErrInvalidReference", err)
	}
	// Zero is a value: a model priced at nothing books a hold of nothing, and
	// the allocation is still an allocation. Below zero is not — no slice of
	// a hold runs backwards.
	if _, err := NewAllocation(mustBucketID(t), ReservationID(validV4), Amount(0)); err != nil {
		t.Fatalf("zero-held allocation = %v, want nil: a zero-priced model books a hold of nothing", err)
	}
	if _, err := NewAllocation(mustBucketID(t), ReservationID(validV4), Amount(-1)); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("negative-held allocation = %v, want ErrInvalidAmount", err)
	}
}

// TestAZeroPricedSettlementIsStillASettlement is the zero-price story end to
// end: a model priced at nothing books a hold of nothing, consumes nothing,
// and settles for nothing — a header of record with no legs, because no
// money moved. The refusal that survives is the once-only one: a settled-
// zero allocation is settled, and a second SettleConsumed is a defect even
// though its amounts are indistinguishable from the first's.
func TestAZeroPricedSettlementIsStillASettlement(t *testing.T) {
	a, err := NewAllocation(mustBucketID(t), ReservationID(validV4), Amount(0))
	if err != nil {
		t.Fatalf("new zero-held allocation: %v", err)
	}

	freePrice := PriceSnapshot{RevisionID: "rev-free", InputUnitPrice: 0, OutputUnitPrice: 0}
	settled, err := a.SettleConsumed(Amount(0), freePrice)
	if err != nil {
		t.Fatalf("settle nothing for nothing: %v", err)
	}
	if settled.Consumed != Amount(0) {
		t.Fatalf("consumed = %d, want 0", settled.Consumed)
	}

	if _, err := settled.SettleConsumed(Amount(0), freePrice); !errors.Is(err, ErrInvalidSettlement) {
		t.Fatalf("a second zero SettleConsumed = %v, want ErrInvalidSettlement — the allocation settled once, at zero", err)
	}

	plan, err := BuildSettle(mustSettlementID(t), "req-zero-priced", []Allocation{settled}, NewLedgerEntryID, legNow)
	if err != nil {
		t.Fatalf("build settle of a zero-priced allocation: %v", err)
	}
	if plan.Settlement.SettledTotal != Amount(0) {
		t.Fatalf("settled total = %d, want 0 — nothing was spent", plan.Settlement.SettledTotal)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("the plan writes %d legs, want none — a settlement of nothing moves no money", len(plan.Entries))
	}
}

func TestSettleConsumedRecordsOnceWithItsPrice(t *testing.T) {
	a := mustAllocation(t, mustBucketID(t), 40)

	settled, err := a.SettleConsumed(mustAmount(t, 25), snapshotPrice())
	if err != nil {
		t.Fatalf("settle 25 of 40: %v", err)
	}
	if settled.Consumed != Amount(25) || settled.Price != (snapshotPrice()) {
		t.Fatalf("settled allocation = (consumed %d, price %v), want (25, the snapshot)",
			settled.Consumed, settled.Price)
	}

	if _, err := settled.SettleConsumed(mustAmount(t, 5), snapshotPrice()); !errors.Is(err, ErrInvalidSettlement) {
		t.Fatalf("a second SettleConsumed = %v, want ErrInvalidSettlement: an allocation settles once", err)
	}
}

func TestSettleConsumedRefusesOverspendAndBadPrices(t *testing.T) {
	a := mustAllocation(t, mustBucketID(t), 40)

	if _, err := a.SettleConsumed(mustAmount(t, 50), snapshotPrice()); !errors.Is(err, ErrInvalidSettlement) {
		t.Fatalf("consume 50 of held 40 = %v, want ErrInvalidSettlement", err)
	}
	// A zero price is a real price — the free model — so the refusal that
	// survives is the negative one.
	negativePrice := PriceSnapshot{RevisionID: "rev", InputUnitPrice: 3, OutputUnitPrice: -1}
	if _, err := a.SettleConsumed(mustAmount(t, 10), negativePrice); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("consume priced below zero on one side = %v, want ErrInvalidAmount", err)
	}
	if _, err := a.SettleConsumed(mustAmount(t, 10), PriceSnapshot{RevisionID: "rev", InputUnitPrice: 0, OutputUnitPrice: 3}); err != nil {
		t.Fatalf("consume priced at zero on one side = %v, want nil — a zero price is a price", err)
	}
}

// TestBuildSettleWritesTheWorkedExample is the two-bucket waterfall ADR 0004
// states: S1 books a hold of 20 and is consumed in full; S2 books 40, spends
// 10 and returns the 30 tail. The plan is three legs in waterfall order — S1
// consume, S2 consume, S2 release — with the header's total the sum of the
// consume legs: 30.
func TestBuildSettleWritesTheWorkedExample(t *testing.T) {
	s1 := mustBucketID(t)
	s2 := mustBucketID(t)
	settlement := mustSettlementID(t)

	first := mustAllocation(t, s1, 20)
	first, err := first.SettleConsumed(mustAmount(t, 20), snapshotPrice())
	if err != nil {
		t.Fatalf("settle S1: %v", err)
	}
	second := mustAllocation(t, s2, 40)
	second, err = second.SettleConsumed(mustAmount(t, 10), snapshotPrice())
	if err != nil {
		t.Fatalf("settle S2: %v", err)
	}

	plan, err := BuildSettle(settlement, "req-2026-09-25-0001", []Allocation{first, second}, NewLedgerEntryID, legNow)
	if err != nil {
		t.Fatalf("build settle: %v", err)
	}

	if plan.Settlement.ID != settlement || plan.Settlement.RequestID != "req-2026-09-25-0001" {
		t.Fatalf("header identity = (%s, %s), want the minted settlement and the request key",
			plan.Settlement.ID, plan.Settlement.RequestID)
	}
	if plan.Settlement.SettledTotal != Amount(30) {
		t.Fatalf("settled total = %d, want 30 — the sum of the consume legs, computed not stated", plan.Settlement.SettledTotal)
	}
	if len(plan.Entries) != 3 {
		t.Fatalf("plan has %d legs, want 3: S1 consume, S2 consume, S2 release", len(plan.Entries))
	}

	firstLeg, secondLeg, thirdLeg := plan.Entries[0], plan.Entries[1], plan.Entries[2]
	if firstLeg.Kind != KindConsume || firstLeg.FundingBucketID != s1 || firstLeg.Amount != Amount(20) {
		t.Fatalf("leg 1 = (%s on %s for %d), want S1's consume of 20 first", firstLeg.Kind, firstLeg.FundingBucketID, firstLeg.Amount)
	}
	if secondLeg.Kind != KindConsume || secondLeg.FundingBucketID != s2 || secondLeg.Amount != Amount(10) {
		t.Fatalf("leg 2 = (%s on %s for %d), want S2's consume of 10", secondLeg.Kind, secondLeg.FundingBucketID, secondLeg.Amount)
	}
	if thirdLeg.Kind != KindRelease || thirdLeg.FundingBucketID != s2 || thirdLeg.Amount != Amount(30) {
		t.Fatalf("leg 3 = (%s on %s for %d), want S2's release of the 30 tail", thirdLeg.Kind, thirdLeg.FundingBucketID, thirdLeg.Amount)
	}
	if thirdLeg.SettlementID != settlement || thirdLeg.ReservationID != second.ReservationID {
		t.Fatalf("release leg names (settlement %s, reservation %q), want the settlement and S2's reservation",
			thirdLeg.SettlementID, thirdLeg.ReservationID)
	}
	for i, leg := range plan.Entries {
		if leg.SettlementID != settlement {
			t.Errorf("leg %d names settlement %q, want %s", i, leg.SettlementID, settlement)
		}
		if leg.Sequence != 0 {
			t.Errorf("leg %d carries sequence %d, want 0 — the adapter stamps it inside the write", i, leg.Sequence)
		}
	}
}

func TestBuildSettleWritesAZeroConsumptionAsReleaseOnly(t *testing.T) {
	s1 := mustBucketID(t)

	plan, err := BuildSettle(mustSettlementID(t), "req-x", []Allocation{mustAllocation(t, s1, 40)}, NewLedgerEntryID, legNow)
	if err != nil {
		t.Fatalf("build settle: %v", err)
	}
	if plan.Settlement.SettledTotal != 0 {
		t.Fatalf("settled total = %d, want 0 for a settlement that consumed nothing", plan.Settlement.SettledTotal)
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Kind != KindRelease || plan.Entries[0].Amount != Amount(40) {
		t.Fatalf("plan legs = %v, want exactly one release of 40", plan.Entries)
	}
}

func TestBuildSettleRefusesAnUnformedSettlement(t *testing.T) {
	if _, err := BuildSettle("", "req-x", []Allocation{mustAllocation(t, mustBucketID(t), 10)}, NewLedgerEntryID, legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("blank settlement id = %v, want ErrInvalidReference", err)
	}
	if _, err := BuildSettle(mustSettlementID(t), "", []Allocation{mustAllocation(t, mustBucketID(t), 10)}, NewLedgerEntryID, legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("blank request id = %v, want ErrInvalidReference", err)
	}
	if _, err := BuildSettle(mustSettlementID(t), "req-x", nil, NewLedgerEntryID, legNow); !errors.Is(err, ErrInvalidSettlement) {
		t.Fatalf("no allocations = %v, want ErrInvalidSettlement", err)
	}
	if _, err := BuildSettle(mustSettlementID(t), "req-x", []Allocation{mustAllocation(t, mustBucketID(t), 10)}, nil, legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("nil minter = %v, want ErrInvalidReference", err)
	}
}

func TestBuildSettleRefusesLiteralStructShortcuts(t *testing.T) {
	overspent := mustAllocation(t, mustBucketID(t), 10)
	overspent.Consumed = mustAmount(t, 20)
	if _, err := BuildSettle(mustSettlementID(t), "req-x", []Allocation{overspent}, NewLedgerEntryID, legNow); !errors.Is(err, ErrInvalidSettlement) {
		t.Fatalf("consumed 20 against held 10 = %v, want ErrInvalidSettlement", err)
	}

	unformed := mustAllocation(t, mustBucketID(t), 10)
	unformed.Consumed = mustAmount(t, 5) // price never recorded
	if _, err := BuildSettle(mustSettlementID(t), "req-x", []Allocation{unformed}, NewLedgerEntryID, legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("a consumed allocation without its price = %v, want the price refusal", err)
	}
}

func TestBuildSettleRefusesANegativeConsumption(t *testing.T) {
	// The exported fields let a shortcut write a negative "consumed". Left
	// unchecked, the tail arithmetic runs backwards: a small negative grew
	// the release past the hold, and math.MinInt64 wrapped the tail negative
	// so the plan came back as a header with NO legs and a total of zero —
	// a request recorded as settled while its hold stayed booked forever.
	for _, consumed := range []Amount{Amount(-1), Amount(math.MinInt64)} {
		alloc := mustAllocation(t, mustBucketID(t), 10)
		alloc.Consumed = consumed
		if _, err := BuildSettle(mustSettlementID(t), "req-x", []Allocation{alloc}, NewLedgerEntryID, legNow); !errors.Is(err, ErrInvalidAmount) {
			t.Fatalf("consumed %d against held 10 = %v, want ErrInvalidAmount: a settlement cannot spend less than nothing",
				consumed, err)
		}
	}
}

func TestBuildSettleRefusesTwoAllocationsForOneBucket(t *testing.T) {
	bucket := mustBucketID(t)
	first := mustAllocation(t, bucket, 10)
	second := mustAllocation(t, bucket, 20)

	if _, err := BuildSettle(mustSettlementID(t), "req-x", []Allocation{first, second}, NewLedgerEntryID, legNow); !errors.Is(err, ErrInvalidSettlement) {
		t.Fatalf("two allocations on one bucket = %v, want ErrInvalidSettlement: the schema's (settlement, bucket, kind) uniqueness is this rule", err)
	}
}
