package accounting

import (
	"fmt"
	"time"
)

// Kind is one of the six movements of money the ledger records. The kind
// fixes the leg's delta algebra — the one table ADR 0004 states and the one
// table the constructors below encode, the schema pins, and nothing may
// extend silently:
//
//	kind       | amount    | settled_delta | held_delta
//	grant      | granted   | +amount       | 0
//	topup      | topped    | +amount       | 0
//	hold       | held      | 0             | +amount
//	release    | released  | 0             | −amount
//	consume    | consumed  | −amount       | −amount
//	adjustment | |delta|   | stated        | stated (exactly one non-zero)
//
// The row worth internalising is consume: it subtracts from BOTH balances by
// the consumed amount, because consumption both spends settled money and
// vacates the hold it was secured by. Release exists as its own kind because
// returning the unconsumed tail is a different fact from spending — a
// settlement writes its consume legs first, then a release leg per
// allocation tail, and the two are never merged into one "net" row.
type Kind string

const (
	// KindGrant is capacity issued for an entitlement cycle, written by the
	// cycle roll that materialises the entitlement.
	KindGrant Kind = "grant"
	// KindTopup is funding added to a PAYG bucket: operator-run today, the
	// payment provider's webhook later (B15).
	KindTopup Kind = "topup"
	// KindHold is capacity occupied by a reservation, booked from the
	// reservation as a fact after the runtime's admission decided it.
	KindHold Kind = "hold"
	// KindRelease is occupied capacity returned: a settlement's unconsumed
	// tail, a compensation with no candidate to serve, or a reaper's
	// expiry booking.
	KindRelease Kind = "release"
	// KindConsume is capacity actually spent, written from the usage fact
	// at settlement, carrying its price snapshot by value.
	KindConsume Kind = "consume"
	// KindAdjustment is an explicit operator correction with a reason, an
	// original-entry reference and stated deltas — the only leg whose
	// deltas are free values, and never an overdraw or credit mechanism: a
	// stated delta that would drive any balance below zero is refused.
	KindAdjustment Kind = "adjustment"
)

// PriceSnapshot is the consume leg's price provenance, copied by value from
// the revision the consume was priced against. The B12 catalog owns
// revisions — this is a textual reference and two copied unit prices, not a
// foreign key, because the settlement must be derivable from the fact alone
// and a price later re-derived from a moving table is not provenance.
type PriceSnapshot struct {
	RevisionID      PriceRevisionID
	InputUnitPrice  Amount
	OutputUnitPrice Amount
}

// LedgerEntry is one immutable bucket leg: the ledger's unit of record. A
// leg is born through one of the kind constructors below — never through
// struct literal, because the delta algebra is the constructor's to state —
// and is stamped with its bucket sequence by the persistence adapter inside
// the transaction that writes it. Sequence zero means "not yet written".
type LedgerEntry struct {
	ID               LedgerEntryID
	FundingBucketID  FundingBucketID
	Kind             Kind
	Amount           Amount
	SettledDelta     Delta
	HeldDelta        Delta
	SettlementID     SettlementID // "" = none
	ReservationID    ReservationID
	CommandKey       CommandKey
	Price            *PriceSnapshot // nil on every non-consume kind
	AdjustmentReason string
	OriginalEntryID  LedgerEntryID
	OperatorID       OperatorID
	Sequence         int64
	CreatedAt        time.Time
}

// EntryIDMinter mints a fresh ledger-leg identity. NewLedgerEntryID is the
// production minter; BuildSettle takes one as a parameter because a single
// settlement writes several legs and their identities come from the same
// source the header's did.
type EntryIDMinter func() (LedgerEntryID, error)

// newEntry is the one construction path: every kind constructor validates its
// references, states its deltas, and hands the leg to the caller fully
// formed. The schema's leg_algebra CHECK pins the same table, so a leg the
// domain would refuse is one the database would refuse too — the refusal
// just arrives in this package's own words, before any statement runs.
func newEntry(id LedgerEntryID, bucketID FundingBucketID, kind Kind, amount Amount,
	settled, held Delta, createdAt time.Time) (LedgerEntry, error) {
	if err := validateMintedEntryID(id); err != nil {
		return LedgerEntry{}, err
	}
	if bucketID == "" {
		return LedgerEntry{}, fmt.Errorf("accounting: %w: blank funding bucket id", ErrInvalidReference)
	}
	if _, err := NewAmount(amount.Int64()); err != nil {
		return LedgerEntry{}, fmt.Errorf("accounting: new %s entry: %w", kind, err)
	}
	return LedgerEntry{
		ID:              LedgerEntryID(id),
		FundingBucketID: bucketID,
		Kind:            kind,
		Amount:          amount,
		SettledDelta:    settled,
		HeldDelta:       held,
		CreatedAt:       createdAt.UTC(),
	}, nil
}

// NewGrantEntry states a grant: capacity issued to an entitlement-cycle
// bucket by the cycle roll, settled balance in, held untouched.
func NewGrantEntry(id LedgerEntryID, bucketID FundingBucketID, granted Amount, createdAt time.Time) (LedgerEntry, error) {
	return newEntry(id, bucketID, KindGrant, granted, Delta(granted.Int64()), 0, createdAt)
}

// NewTopupEntry states a topup: funding added to a PAYG bucket, keyed by the
// caller's idempotency key so a retried command converges on its original
// leg instead of moving money twice.
func NewTopupEntry(id LedgerEntryID, bucketID FundingBucketID, topped Amount, commandKey CommandKey, createdAt time.Time) (LedgerEntry, error) {
	if err := validateCommandKey(commandKey); err != nil {
		return LedgerEntry{}, fmt.Errorf("accounting: new topup entry: %w", err)
	}
	entry, err := newEntry(id, bucketID, KindTopup, topped, Delta(topped.Int64()), 0, createdAt)
	if err != nil {
		return LedgerEntry{}, err
	}
	entry.CommandKey = commandKey
	return entry, nil
}

// NewHoldEntry states a hold: capacity occupied by a reservation, booked
// from the reservation as a fact after the runtime's admission decided it.
func NewHoldEntry(id LedgerEntryID, bucketID FundingBucketID, held Amount, reservationID ReservationID, createdAt time.Time) (LedgerEntry, error) {
	if err := validateReservationID(reservationID); err != nil {
		return LedgerEntry{}, fmt.Errorf("accounting: new hold entry: %w", err)
	}
	entry, err := newEntry(id, bucketID, KindHold, held, 0, Delta(held.Int64()), createdAt)
	if err != nil {
		return LedgerEntry{}, err
	}
	entry.ReservationID = reservationID
	return entry, nil
}

// NewReleaseEntry states a release: occupied capacity returned. The
// reservation is always named — the hold being returned is the reservation's
// — and the settlement is named too when the release is a settlement's
// unconsumed tail (reaper and compensation releases book from terminal facts
// with no settlement behind them).
func NewReleaseEntry(id LedgerEntryID, bucketID FundingBucketID, released Amount, reservationID ReservationID, settlementID SettlementID, createdAt time.Time) (LedgerEntry, error) {
	if err := validateReservationID(reservationID); err != nil {
		return LedgerEntry{}, fmt.Errorf("accounting: new release entry: %w", err)
	}
	entry, err := newEntry(id, bucketID, KindRelease, released, 0, -Delta(released.Int64()), createdAt)
	if err != nil {
		return LedgerEntry{}, err
	}
	entry.ReservationID = reservationID
	entry.SettlementID = settlementID
	return entry, nil
}

// NewConsumeEntry states a consume: capacity actually spent, with the price
// snapshot the charge is provenance for. Consume is the only kind that moves
// both balances and the only kind that carries a price snapshot.
func NewConsumeEntry(id LedgerEntryID, bucketID FundingBucketID, consumed Amount, settlementID SettlementID, price PriceSnapshot, createdAt time.Time) (LedgerEntry, error) {
	if settlementID == "" {
		return LedgerEntry{}, fmt.Errorf("accounting: new consume entry: %w: a consume names its settlement", ErrInvalidReference)
	}
	if err := validatePriceSnapshot(price); err != nil {
		return LedgerEntry{}, fmt.Errorf("accounting: new consume entry: %w", err)
	}
	entry, err := newEntry(id, bucketID, KindConsume, consumed, -Delta(consumed.Int64()), -Delta(consumed.Int64()), createdAt)
	if err != nil {
		return LedgerEntry{}, err
	}
	entry.SettlementID = settlementID
	snap := price
	entry.Price = &snap
	return entry, nil
}

// NewAdjustmentEntry states an operator correction: exactly one stated
// non-zero delta, the reason it was made, the entry it corrects and the
// operator who authorised it. This is the ledger's only correction path; it
// is never an overdraw or credit mechanism (ADR 0004) — a delta that would
// drive a balance negative is refused at ApplyTo, in the same words the
// schema's balance CHECKs would refuse the row. The command key is optional
// — an operator re-running a correction can key it, and the
// same-key-same-payload convergence applies.
func NewAdjustmentEntry(id LedgerEntryID, bucketID FundingBucketID, settledDelta, heldDelta Delta,
	reason string, originalEntryID LedgerEntryID, operatorID OperatorID, commandKey CommandKey, createdAt time.Time) (LedgerEntry, error) {
	if (settledDelta != 0) == (heldDelta != 0) {
		return LedgerEntry{}, fmt.Errorf("accounting: new adjustment entry: %w: exactly one of settled_delta/held_delta is non-zero", ErrInvalidAdjustment)
	}
	if err := validateAdjustmentReason(reason); err != nil {
		return LedgerEntry{}, fmt.Errorf("accounting: new adjustment entry: %w", err)
	}
	if originalEntryID == "" {
		return LedgerEntry{}, fmt.Errorf("accounting: new adjustment entry: %w: an adjustment names the entry it corrects", ErrInvalidReference)
	}
	if err := validateOperatorID(operatorID); err != nil {
		return LedgerEntry{}, fmt.Errorf("accounting: new adjustment entry: %w", err)
	}
	// Optional here, pinned where present: an unkeyed correction converges
	// through nothing, a keyed one through the same key-same-payload rule a
	// topup gets — but a key that could not live in the column is refused
	// before the statement, the same as a required one.
	if commandKey != "" {
		if err := validateCommandKey(commandKey); err != nil {
			return LedgerEntry{}, fmt.Errorf("accounting: new adjustment entry: %w", err)
		}
	}
	amount, err := Delta.Abs(settledDelta)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("accounting: new adjustment entry: settled_delta: %w", err)
	}
	if heldDelta != 0 {
		if amount, err = Delta.Abs(heldDelta); err != nil {
			return LedgerEntry{}, fmt.Errorf("accounting: new adjustment entry: held_delta: %w", err)
		}
	}
	entry, err := newEntry(id, bucketID, KindAdjustment, amount, settledDelta, heldDelta, createdAt)
	if err != nil {
		return LedgerEntry{}, err
	}
	entry.AdjustmentReason = reason
	entry.OriginalEntryID = originalEntryID
	entry.OperatorID = operatorID
	entry.CommandKey = commandKey
	return entry, nil
}

// validatePriceSnapshot pins the consume leg's price provenance: all three
// parts present, every price a magnitude of at least zero. A zero is a real
// price — a model the deployment gives away is priced at nothing, and the
// settlement that proves a nothing-charge still names the revision it was
// free under. What is refused is a price below zero: no movement of money
// runs backwards.
func validatePriceSnapshot(price PriceSnapshot) error {
	if err := validatePriceRevisionID(price.RevisionID); err != nil {
		return err
	}
	if err := validateAmountAtOrAboveZero(price.InputUnitPrice.Int64()); err != nil {
		return fmt.Errorf("input unit price: %w", err)
	}
	if err := validateAmountAtOrAboveZero(price.OutputUnitPrice.Int64()); err != nil {
		return fmt.Errorf("output unit price: %w", err)
	}
	return nil
}

// Derivation is a bucket's balances recomputed from its legs alone — the
// read model Reconcile compares against the bucket's cached projection. The
// cached columns exist so admission reads do not aggregate the ledger on
// every hold; the derivation exists so the cache never gets the last word.
// They are computed in the database (SUM over the legs, cast to bigint so
// an overflow is a loud error rather than a silent numeric), and when the
// two disagree the legs win: the cache is rebuildable, the legs are the
// record.
type Derivation struct {
	Settled   Balance
	Held      Balance
	Available Balance
	Legs      int64
}

// ConsistentWith reports whether the derivation matches a bucket's cached
// projection — the whole of Reconcile's verdict, stated once so the
// application layer never re-derives it field by field.
func (d Derivation) ConsistentWith(b Bucket) bool {
	return d.Settled == b.Settled && d.Held == b.Held && d.Available == b.Available
}

// ApplyTo returns the bucket after this leg lands on it. It is the algebra's
// single execution site on the domain side — the persisted echo is the
// guarded statement the ledger adapter runs in the same transaction as the
// insert, whose WHERE clause repeats every guard below — so a caller that
// applies first and appends second gets a refusal in domain words when the
// read is already stale, and the statement's verdict when the world moved
// under it.
//
// Every guard has a direction, and the direction is always the safe one:
// nothing here can push held, settled or available below zero — including an
// adjustment, whose deltas are free only while the balances they land on
// stay non-negative.
func (e LedgerEntry) ApplyTo(b Bucket) (Bucket, error) {
	if b.ID != e.FundingBucketID {
		return Bucket{}, fmt.Errorf("accounting: apply %s entry to bucket: leg names bucket %s, not %s",
			e.Kind, e.FundingBucketID, b.ID)
	}
	if b.Status != BucketActive {
		return Bucket{}, fmt.Errorf("accounting: apply %s entry to bucket %s: %w", e.Kind, b.ID, ErrBucketClosed)
	}

	next := b
	take := Balance(e.Amount)
	switch e.Kind {
	case KindGrant, KindTopup:
		// Ownership is part of the algebra: a grant is how a cycle bucket is
		// funded and a topup is how an account's is, so a leg naming the
		// other owner is the transition it is not. No single row could pin
		// the pair — the leg and its bucket's owner live in two tables —
		// which is why this guard lives here and in the echo's WHERE clause
		// rather than in a schema CHECK.
		if e.Kind == KindGrant && b.OwnedByAccount() {
			return Bucket{}, fmt.Errorf("accounting: apply grant entry to bucket %s: %w: a grant funds a cycle bucket, and %s is account %s's",
				b.ID, ErrInvalidTransition, b.ID, b.AccountID)
		}
		if e.Kind == KindTopup && b.OwnedByEntitlement() {
			return Bucket{}, fmt.Errorf("accounting: apply topup entry to bucket %s: %w: a topup funds an account bucket, and %s is entitlement %s's cycle",
				b.ID, ErrInvalidTransition, b.ID, b.EntitlementID)
		}
		settled, err := b.Settled.Move(e.SettledDelta)
		if err != nil {
			return Bucket{}, fmt.Errorf("accounting: apply %s entry to bucket %s: %w", e.Kind, b.ID, err)
		}
		available, err := settled.Sub(b.Held)
		if err != nil {
			return Bucket{}, fmt.Errorf("accounting: apply %s entry to bucket %s: %w", e.Kind, b.ID, err)
		}
		next.Settled, next.Available = settled, available

	case KindHold:
		if b.Available < take {
			return Bucket{}, fmt.Errorf("accounting: apply hold entry to bucket %s: %w: %d available, %d requested",
				b.ID, ErrInsufficientAvailable, b.Available, e.Amount)
		}
		held, err := b.Held.Move(e.HeldDelta)
		if err != nil {
			return Bucket{}, fmt.Errorf("accounting: apply hold entry to bucket %s: %w", b.ID, err)
		}
		available, err := b.Available.Move(-e.HeldDelta)
		if err != nil {
			return Bucket{}, fmt.Errorf("accounting: apply hold entry to bucket %s: %w", b.ID, err)
		}
		next.Held, next.Available = held, available

	case KindRelease:
		if b.Held < take {
			return Bucket{}, fmt.Errorf("accounting: apply release entry to bucket %s: %w: %d held, %d released",
				b.ID, ErrInsufficientHeld, b.Held, e.Amount)
		}
		held, err := b.Held.Move(e.HeldDelta)
		if err != nil {
			return Bucket{}, fmt.Errorf("accounting: apply release entry to bucket %s: %w", b.ID, err)
		}
		available, err := b.Available.Move(-e.HeldDelta)
		if err != nil {
			return Bucket{}, fmt.Errorf("accounting: apply release entry to bucket %s: %w", b.ID, err)
		}
		next.Held, next.Available = held, available

	case KindConsume:
		if b.Held < take {
			return Bucket{}, fmt.Errorf("accounting: apply consume entry to bucket %s: %w: %d held, %d consumed",
				b.ID, ErrInsufficientHeld, b.Held, e.Amount)
		}
		if b.Settled < take {
			return Bucket{}, fmt.Errorf("accounting: apply consume entry to bucket %s: %w: %d settled, %d consumed",
				b.ID, ErrInsufficientSettled, b.Settled, e.Amount)
		}
		settled, err := b.Settled.Move(e.SettledDelta)
		if err != nil {
			return Bucket{}, fmt.Errorf("accounting: apply consume entry to bucket %s: %w", b.ID, err)
		}
		held, err := b.Held.Move(e.HeldDelta)
		if err != nil {
			return Bucket{}, fmt.Errorf("accounting: apply consume entry to bucket %s: %w", b.ID, err)
		}
		// Both balances drop by the same amount, so available is unchanged —
		// computed, not assumed, so an algebra bug here is a refusal here.
		available, err := settled.Sub(held)
		if err != nil {
			return Bucket{}, fmt.Errorf("accounting: apply consume entry to bucket %s: %w", b.ID, err)
		}
		next.Settled, next.Held, next.Available = settled, held, available

	case KindAdjustment:
		settled, err := b.Settled.Move(e.SettledDelta)
		if err != nil {
			return Bucket{}, fmt.Errorf("accounting: apply adjustment entry to bucket %s: %w", b.ID, err)
		}
		if settled.IsNegative() {
			return Bucket{}, fmt.Errorf("accounting: apply adjustment entry to bucket %s: %w: settled would go to %d",
				b.ID, ErrInvalidAdjustment, settled)
		}
		held, err := b.Held.Move(e.HeldDelta)
		if err != nil {
			return Bucket{}, fmt.Errorf("accounting: apply adjustment entry to bucket %s: %w", b.ID, err)
		}
		if held.IsNegative() {
			return Bucket{}, fmt.Errorf("accounting: apply adjustment entry to bucket %s: %w: held would go to %d",
				b.ID, ErrInvalidAdjustment, held)
		}
		available, err := settled.Sub(held)
		if err != nil {
			return Bucket{}, fmt.Errorf("accounting: apply adjustment entry to bucket %s: %w", b.ID, err)
		}
		if available.IsNegative() {
			return Bucket{}, fmt.Errorf("accounting: apply adjustment entry to bucket %s: %w: available would go to %d",
				b.ID, ErrInvalidAdjustment, available)
		}
		next.Settled, next.Held, next.Available = settled, held, available

	default:
		return Bucket{}, fmt.Errorf("accounting: apply entry to bucket %s: unknown kind %q", b.ID, e.Kind)
	}
	return next, nil
}
