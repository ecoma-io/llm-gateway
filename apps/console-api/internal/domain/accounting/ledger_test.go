package accounting

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// The one table this package exists to state is ADR 0004's delta algebra.
// The tests below are that table, row by row, plus every guard ApplyTo can
// answer and the waterfall a settlement's legs write in. If a constructor's
// deltas drift from the table, or a guard's direction stops being the safe
// one, a test here fails before any statement runs.

var legNow = time.Date(2026, time.September, 25, 11, 0, 0, 0, time.UTC)

func mustEntryID(t *testing.T) LedgerEntryID {
	t.Helper()
	id, err := NewLedgerEntryID()
	if err != nil {
		t.Fatalf("mint ledger entry id: %v", err)
	}
	return id
}

func mustAmount(t *testing.T, raw int64) Amount {
	t.Helper()
	a, err := NewAmount(raw)
	if err != nil {
		t.Fatalf("NewAmount(%d): %v", raw, err)
	}
	return a
}

func activeBucket(t *testing.T) Bucket {
	t.Helper()
	b, err := NewEntitlementBucket(mustBucketID(t), EntitlementID(validV7), bucketNow)
	if err != nil {
		t.Fatalf("open bucket: %v", err)
	}
	return b
}

func activeAccountBucket(t *testing.T) Bucket {
	t.Helper()
	b, err := NewAccountBucket(mustBucketID(t), "account-7", bucketNow)
	if err != nil {
		t.Fatalf("open account bucket: %v", err)
	}
	return b
}

func snapshotPrice() PriceSnapshot {
	return PriceSnapshot{
		RevisionID:      "rev-2026-09",
		InputUnitPrice:  3,
		OutputUnitPrice: 5,
	}
}

// TestTheDeltaAlgebraTable is ADR 0004's table as assertions: one test per
// kind, each naming the row it pins.
func TestTheDeltaAlgebraTable(t *testing.T) {
	bucket := mustBucketID(t)

	grant, err := NewGrantEntry(mustEntryID(t), bucket, mustAmount(t, 100), legNow)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if grant.SettledDelta != Delta(100) || grant.HeldDelta != 0 {
		t.Fatalf("grant deltas = (%d, %d), want (+100, 0): settled in, held untouched",
			grant.SettledDelta, grant.HeldDelta)
	}

	topup, err := NewTopupEntry(mustEntryID(t), bucket, mustAmount(t, 50), "cmd-1", legNow)
	if err != nil {
		t.Fatalf("topup: %v", err)
	}
	if topup.SettledDelta != Delta(50) || topup.HeldDelta != 0 || topup.CommandKey != "cmd-1" {
		t.Fatalf("topup = (settled %d, held %d, key %q), want (+50, 0, cmd-1)",
			topup.SettledDelta, topup.HeldDelta, topup.CommandKey)
	}

	hold, err := NewHoldEntry(mustEntryID(t), bucket, mustAmount(t, 40), ReservationID(validV4), legNow)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if hold.SettledDelta != 0 || hold.HeldDelta != Delta(40) || hold.ReservationID != ReservationID(validV4) {
		t.Fatalf("hold = (settled %d, held %d, reservation %q), want (0, +40, the reservation)",
			hold.SettledDelta, hold.HeldDelta, hold.ReservationID)
	}

	release, err := NewReleaseEntry(mustEntryID(t), bucket, mustAmount(t, 15), ReservationID(validV4), "", legNow)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if release.SettledDelta != 0 || release.HeldDelta != Delta(-15) {
		t.Fatalf("release deltas = (%d, %d), want (0, −15)", release.SettledDelta, release.HeldDelta)
	}

	consume, err := NewConsumeEntry(mustEntryID(t), bucket, mustAmount(t, 25), SettlementID(validV7), snapshotPrice(), legNow)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if consume.SettledDelta != Delta(-25) || consume.HeldDelta != Delta(-25) {
		t.Fatalf("consume deltas = (%d, %d), want (−25, −25): spending and vacating are one fact",
			consume.SettledDelta, consume.HeldDelta)
	}
	if consume.Price == nil || *consume.Price != snapshotPrice() {
		t.Fatalf("consume price = %v, want the snapshot by value", consume.Price)
	}

	adjustment, err := NewAdjustmentEntry(mustEntryID(t), bucket, Delta(-10), 0,
		"goodwill credit", LedgerEntryID(validV7), "ops-1", "", legNow)
	if err != nil {
		t.Fatalf("adjustment: %v", err)
	}
	if adjustment.SettledDelta != Delta(-10) || adjustment.HeldDelta != 0 || adjustment.Amount != Amount(10) {
		t.Fatalf("adjustment = (settled %d, held %d, amount %d), want (−10, 0, 10): the amount is the one non-zero delta's magnitude",
			adjustment.SettledDelta, adjustment.HeldDelta, adjustment.Amount)
	}
}

func TestAdjustmentStatesExactlyOneNonZeroDelta(t *testing.T) {
	bucket := mustBucketID(t)
	original := LedgerEntryID(validV7)

	if _, err := NewAdjustmentEntry(mustEntryID(t), bucket, 0, 0, "no move", original, "ops-1", "", legNow); !errors.Is(err, ErrInvalidAdjustment) {
		t.Fatalf("adjustment (0, 0) = %v, want ErrInvalidAdjustment", err)
	}
	if _, err := NewAdjustmentEntry(mustEntryID(t), bucket, Delta(5), Delta(5), "both move", original, "ops-1", "", legNow); !errors.Is(err, ErrInvalidAdjustment) {
		t.Fatalf("adjustment (5, 5) = %v, want ErrInvalidAdjustment", err)
	}
	if _, err := NewAdjustmentEntry(mustEntryID(t), bucket, 0, Delta(5), "held only", original, "ops-1", "", legNow); err != nil {
		t.Fatalf("adjustment (0, +5) must pass: %v", err)
	}
	// The one delta whose magnitude does not fit an Amount is refused where
	// the leg's amount is derived, not left to wrap or to the schema.
	if _, err := NewAdjustmentEntry(mustEntryID(t), bucket, Delta(math.MinInt64), 0, "floor correction", original, "ops-1", "", legNow); !errors.Is(err, ErrAmountRange) {
		t.Fatalf("adjustment with min int64 settled delta = %v, want ErrAmountRange", err)
	}
	if _, err := NewAdjustmentEntry(mustEntryID(t), bucket, 0, Delta(math.MinInt64), "floor correction", original, "ops-1", "", legNow); !errors.Is(err, ErrAmountRange) {
		t.Fatalf("adjustment with min int64 held delta = %v, want ErrAmountRange", err)
	}
}

func TestAdjustmentValidationCoversReasonOriginalOperatorAndOptionalKey(t *testing.T) {
	bucket := mustBucketID(t)
	original := LedgerEntryID(validV7)

	if _, err := NewAdjustmentEntry(mustEntryID(t), bucket, Delta(5), 0, "", original, "ops-1", "", legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("reasonless adjustment = %v, want ErrInvalidReference", err)
	}
	if _, err := NewAdjustmentEntry(mustEntryID(t), bucket, Delta(5), 0, "reason", "", "ops-1", "", legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("orphan adjustment = %v, want ErrInvalidReference", err)
	}
	if _, err := NewAdjustmentEntry(mustEntryID(t), bucket, Delta(5), 0, "reason", original, "", "", legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("unattributed adjustment = %v, want ErrInvalidReference", err)
	}
	if _, err := NewAdjustmentEntry(mustEntryID(t), bucket, Delta(5), 0, "reason", original, "ops-1", CommandKey(strings.Repeat("k", maxCommandKeyLength+1)), legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("oversized optional key = %v, want ErrInvalidReference", err)
	}
}

func TestEveryKindRefusesAnUnformedLeg(t *testing.T) {
	bucket := mustBucketID(t)

	if _, err := NewGrantEntry(mustEntryID(t), bucket, Amount(0), legNow); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("zero grant = %v, want ErrInvalidAmount", err)
	}
	if _, err := NewGrantEntry("", bucket, mustAmount(t, 1), legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("unminted entry id = %v, want ErrInvalidReference", err)
	}
	if _, err := NewGrantEntry(mustEntryID(t), "", mustAmount(t, 1), legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("blank bucket = %v, want ErrInvalidReference", err)
	}
	if _, err := NewTopupEntry(mustEntryID(t), bucket, mustAmount(t, 1), "", legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("keyless topup = %v, want ErrInvalidReference: a topup is keyed by definition", err)
	}
	if _, err := NewHoldEntry(mustEntryID(t), bucket, mustAmount(t, 1), "not-a-uuid", legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("hold with a malformed reservation = %v, want ErrInvalidReference", err)
	}
	if _, err := NewReleaseEntry(mustEntryID(t), bucket, mustAmount(t, 1), "not-a-uuid", "", legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("release with a malformed reservation = %v, want ErrInvalidReference", err)
	}
	if _, err := NewConsumeEntry(mustEntryID(t), bucket, mustAmount(t, 1), "", snapshotPrice(), legNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("settlementless consume = %v, want ErrInvalidReference", err)
	}
	// A zero price is a real price — the free model — so the refusal that
	// survives is the negative one.
	negative := PriceSnapshot{RevisionID: "rev", InputUnitPrice: 3, OutputUnitPrice: -1}
	if _, err := NewConsumeEntry(mustEntryID(t), bucket, mustAmount(t, 1), SettlementID(validV7), negative, legNow); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("consume priced below zero on one side = %v, want ErrInvalidAmount", err)
	}
}

func TestApplyToRefusesALegForADifferentOrClosedBucket(t *testing.T) {
	b := activeBucket(t)
	other := mustBucketID(t)
	grant, err := NewGrantEntry(mustEntryID(t), other, mustAmount(t, 10), legNow)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := grant.ApplyTo(b); err == nil {
		t.Fatal("a leg for another bucket must be refused")
	}

	closed, err := b.Close(bucketNow)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	mine, err := NewGrantEntry(mustEntryID(t), b.ID, mustAmount(t, 10), legNow)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := mine.ApplyTo(closed); !errors.Is(err, ErrBucketClosed) {
		t.Fatalf("apply to a closed bucket = %v, want ErrBucketClosed", err)
	}
}

func TestApplyToRefusesAFunderForTheOtherOwner(t *testing.T) {
	// A grant funds a cycle bucket and a topup funds an account's; the
	// matching kind against the other owner is the transition it is not,
	// refused before any balance moves.
	cycle := activeBucket(t)
	strayTopup, err := NewTopupEntry(mustEntryID(t), cycle.ID, mustAmount(t, 10), "cmd-stray", legNow)
	if err != nil {
		t.Fatalf("topup: %v", err)
	}
	if _, err := strayTopup.ApplyTo(cycle); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("topup onto entitlement bucket %s = %v, want ErrInvalidTransition", cycle.ID, err)
	}

	account := activeAccountBucket(t)
	strayGrant, err := NewGrantEntry(mustEntryID(t), account.ID, mustAmount(t, 10), legNow)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := strayGrant.ApplyTo(account); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("grant onto account bucket %s = %v, want ErrInvalidTransition", account.ID, err)
	}

	// The guard is about the pair, not the kind: each funder lands on the
	// owner it exists for.
	ownedTopup, err := NewTopupEntry(mustEntryID(t), account.ID, mustAmount(t, 10), "cmd-account", legNow)
	if err != nil {
		t.Fatalf("topup: %v", err)
	}
	if _, err := ownedTopup.ApplyTo(account); err != nil {
		t.Fatalf("topup onto its own account bucket: %v", err)
	}
	ownedGrant, err := NewGrantEntry(mustEntryID(t), cycle.ID, mustAmount(t, 10), legNow)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := ownedGrant.ApplyTo(cycle); err != nil {
		t.Fatalf("grant onto its own cycle bucket: %v", err)
	}
}

func TestApplyToHoldGuardIsAvailable(t *testing.T) {
	b := activeBucket(t)
	b.Settled, b.Held, b.Available = Balance(100), Balance(90), Balance(10)

	hold, err := NewHoldEntry(mustEntryID(t), b.ID, mustAmount(t, 20), ReservationID(validV4), legNow)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, err := hold.ApplyTo(b); !errors.Is(err, ErrInsufficientAvailable) {
		t.Fatalf("hold 20 against available 10 = %v, want ErrInsufficientAvailable", err)
	}
}

func TestApplyToReleaseGuardIsHeld(t *testing.T) {
	b := activeBucket(t)
	b.Settled, b.Held, b.Available = Balance(100), Balance(10), Balance(90)

	release, err := NewReleaseEntry(mustEntryID(t), b.ID, mustAmount(t, 20), ReservationID(validV4), "", legNow)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := release.ApplyTo(b); !errors.Is(err, ErrInsufficientHeld) {
		t.Fatalf("release 20 against held 10 = %v, want ErrInsufficientHeld", err)
	}
}

func TestApplyToConsumeGuardsHeldFirstThenSettled(t *testing.T) {
	b := activeBucket(t)
	b.Settled, b.Held, b.Available = Balance(100), Balance(10), Balance(90)

	consume, err := NewConsumeEntry(mustEntryID(t), b.ID, mustAmount(t, 20), SettlementID(validV7), snapshotPrice(), legNow)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, err := consume.ApplyTo(b); !errors.Is(err, ErrInsufficientHeld) {
		t.Fatalf("consume 20 against held 10 = %v, want ErrInsufficientHeld first", err)
	}

	short := activeBucket(t)
	short.Settled, short.Held, short.Available = Balance(10), Balance(20), Balance(-10)
	shortConsume, err := NewConsumeEntry(mustEntryID(t), short.ID, mustAmount(t, 20), SettlementID(validV7), snapshotPrice(), legNow)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, err := shortConsume.ApplyTo(short); !errors.Is(err, ErrInsufficientSettled) {
		t.Fatalf("consume 20 against settled 10 (held 20) = %v, want ErrInsufficientSettled", err)
	}
}

func TestApplyToAdjustmentNeverDrivesAnyBalanceNegative(t *testing.T) {
	b := activeBucket(t)
	b.Settled, b.Held, b.Available = Balance(100), Balance(40), Balance(60)

	heldDown, err := NewAdjustmentEntry(mustEntryID(t), b.ID, 0, Delta(-50), "reaper correction", LedgerEntryID(validV7), "ops-1", "", legNow)
	if err != nil {
		t.Fatalf("adjustment: %v", err)
	}
	if _, err := heldDown.ApplyTo(b); !errors.Is(err, ErrInvalidAdjustment) {
		t.Fatalf("held 40 − 50 = %v, want ErrInvalidAdjustment", err)
	}

	availableDown, err := NewAdjustmentEntry(mustEntryID(t), b.ID, Delta(-100), 0, "mispost correction", LedgerEntryID(validV7), "ops-1", "", legNow)
	if err != nil {
		t.Fatalf("adjustment: %v", err)
	}
	if _, err := availableDown.ApplyTo(b); !errors.Is(err, ErrInvalidAdjustment) {
		t.Fatalf("settled 100 − 100 with 40 held (available → −40) = %v, want ErrInvalidAdjustment", err)
	}

	// The no-credit rule: settled itself never goes below zero. The schema
	// pins this transitively (settled = available + held, both pinned); the
	// domain guard pins it directly, so a correction is refused in domain
	// words before any statement runs.
	settledDown, err := NewAdjustmentEntry(mustEntryID(t), b.ID, Delta(-150), 0, "goodwill debit", LedgerEntryID(validV7), "ops-1", "", legNow)
	if err != nil {
		t.Fatalf("adjustment: %v", err)
	}
	if _, err := settledDown.ApplyTo(b); !errors.Is(err, ErrInvalidAdjustment) {
		t.Fatalf("settled 100 − 150 = %v, want ErrInvalidAdjustment: no credit semantics are built", err)
	}

	fits, err := NewAdjustmentEntry(mustEntryID(t), b.ID, Delta(-30), 0, "mispost correction", LedgerEntryID(validV7), "ops-1", "", legNow)
	if err != nil {
		t.Fatalf("adjustment: %v", err)
	}
	after, err := fits.ApplyTo(b)
	if err != nil {
		t.Fatalf("settled 100 − 30: %v", err)
	}
	if after.Settled != Balance(70) || after.Available != Balance(30) || after.Held != Balance(40) {
		t.Fatalf("after a fitting correction: (%d, %d, %d), want (70, 40, 30)",
			after.Settled, after.Held, after.Available)
	}
}

func TestApplyToAnswersAnUnknownKindWithAnError(t *testing.T) {
	b := activeBucket(t)
	unknown := LedgerEntry{ID: mustEntryID(t), FundingBucketID: b.ID, Kind: Kind("revoke"),
		Amount: mustAmount(t, 1)}

	if _, err := unknown.ApplyTo(b); err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("apply an unknown kind = %v, want an unknown-kind error", err)
	}
}

// TestTheWaterfallReplaysADR0004sExample is the worked example: a bucket
// granted 100, holding 40 for a reservation, settles 25 consumed and releases
// the 15 tail — consume leg first, release leg second, available computed at
// every step and equal to settled − held throughout.
func TestTheWaterfallReplaysADR0004sExample(t *testing.T) {
	b := activeBucket(t)

	grant, err := NewGrantEntry(mustEntryID(t), b.ID, mustAmount(t, 100), legNow)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	b, err = grant.ApplyTo(b)
	if err != nil {
		t.Fatalf("apply grant: %v", err)
	}
	if b.Settled != Balance(100) || b.Available != Balance(100) {
		t.Fatalf("after grant: settled %d, available %d; want 100, 100", b.Settled, b.Available)
	}

	hold, err := NewHoldEntry(mustEntryID(t), b.ID, mustAmount(t, 40), ReservationID(validV4), legNow)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	b, err = hold.ApplyTo(b)
	if err != nil {
		t.Fatalf("apply hold: %v", err)
	}
	if b.Held != Balance(40) || b.Available != Balance(60) {
		t.Fatalf("after hold: held %d, available %d; want 40, 60", b.Held, b.Available)
	}

	consume, err := NewConsumeEntry(mustEntryID(t), b.ID, mustAmount(t, 25), SettlementID(validV7), snapshotPrice(), legNow)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	b, err = consume.ApplyTo(b)
	if err != nil {
		t.Fatalf("apply consume: %v", err)
	}
	if b.Settled != Balance(75) || b.Held != Balance(15) || b.Available != Balance(60) {
		t.Fatalf("after consume: settled %d, held %d, available %d; want 75, 15, 60 — available unchanged",
			b.Settled, b.Held, b.Available)
	}

	release, err := NewReleaseEntry(mustEntryID(t), b.ID, mustAmount(t, 15), ReservationID(validV4), "", legNow)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	b, err = release.ApplyTo(b)
	if err != nil {
		t.Fatalf("apply release: %v", err)
	}
	if b.Settled != Balance(75) || b.Held != 0 || b.Available != Balance(75) {
		t.Fatalf("after release: settled %d, held %d, available %d; want 75, 0, 75",
			b.Settled, b.Held, b.Available)
	}
}

func TestDerivationConsistencyIsTheWholeReconcileVerdict(t *testing.T) {
	b := activeBucket(t)
	b.Settled, b.Held, b.Available = Balance(75), Balance(0), Balance(75)

	derived := Derivation{Settled: Balance(75), Held: 0, Available: Balance(75), Legs: 4}
	if !derived.ConsistentWith(b) {
		t.Fatal("a matching derivation must read consistent")
	}

	drifted := derived
	drifted.Available = Balance(74)
	if drifted.ConsistentWith(b) {
		t.Fatal("a one-unit drift in any column must read inconsistent")
	}
}
