package accounting

import (
	"fmt"
	"time"
)

// Settlement is the header of one settlement of record: one Data Plane
// request's financial closure, keyed exactly-once by its request id. The
// header carries only what the legs cannot say — the exactly-once key and
// the total — because the legs ARE the state: which buckets were touched,
// in what waterfall order, at which prices, is all derivable from the rows
// this header names. There is no status column on purpose; a settlement
// exists or the transaction that would have written it rolled back.
type Settlement struct {
	ID           SettlementID
	RequestID    RequestID
	SettledTotal Amount
	CreatedAt    time.Time
}

// Allocation is one bucket's slice of a settlement: how much the
// reservation had booked against that bucket, how much of it the usage fact
// actually spent, and the price the spend is provenance for. Held minus
// consumed is the tail that goes back — the builder below writes that
// arithmetic as legs, never as a field a caller could misstate.
type Allocation struct {
	BucketID      FundingBucketID
	ReservationID ReservationID
	HeldBooked    Amount
	Consumed      Amount
	Price         PriceSnapshot
}

// NewAllocation validates one allocation's parts before it can join a
// settlement: a real bucket, a canonical reservation reference, a positive
// booked hold.
func NewAllocation(bucketID FundingBucketID, reservationID ReservationID, heldBooked Amount) (Allocation, error) {
	if bucketID == "" {
		return Allocation{}, fmt.Errorf("accounting: new allocation: %w: blank funding bucket id", ErrInvalidReference)
	}
	if err := validateReservationID(reservationID); err != nil {
		return Allocation{}, fmt.Errorf("accounting: new allocation: %w", err)
	}
	if _, err := NewAmount(heldBooked.Int64()); err != nil {
		return Allocation{}, fmt.Errorf("accounting: new allocation: %w", err)
	}
	return Allocation{BucketID: bucketID, ReservationID: reservationID, HeldBooked: heldBooked}, nil
}

// SettleConsumed records what the usage fact spent on one allocation, with
// the price snapshot the charge is provenance for. A second call is a
// caller defect and is refused — an allocation settles once.
func (a Allocation) SettleConsumed(consumed Amount, price PriceSnapshot) (Allocation, error) {
	if a.Consumed != 0 {
		return Allocation{}, fmt.Errorf("accounting: allocation of bucket %s: %w: consumed already recorded", a.BucketID, ErrInvalidSettlement)
	}
	if _, err := NewAmount(consumed.Int64()); err != nil {
		return Allocation{}, fmt.Errorf("accounting: allocation of bucket %s: %w", a.BucketID, err)
	}
	if consumed > a.HeldBooked {
		return Allocation{}, fmt.Errorf("accounting: allocation of bucket %s: %w: %d consumed against %d held",
			a.BucketID, ErrInvalidSettlement, consumed, a.HeldBooked)
	}
	if err := validatePriceSnapshot(price); err != nil {
		return Allocation{}, fmt.Errorf("accounting: allocation of bucket %s: %w", a.BucketID, err)
	}
	a.Consumed = consumed
	a.Price = price
	return a, nil
}

// SettlePlan is what BuildSettle writes atomically: the settlement header
// and its legs, in the waterfall order the allocations were given. The
// persistence adapter appends the entries and inserts the header inside one
// control-plane transaction; a plan is a fact set, not a suggestion.
type SettlePlan struct {
	Settlement Settlement
	Entries    []LedgerEntry
}

// BuildSettle turns a request's allocations into the legs ADR 0004's
// settlement example names, preserving the caller's waterfall order: per
// allocation, the consume leg first (settled and held both down by what was
// spent, price snapshot by value), then a separate release leg for the
// unconsumed tail (held down only, reservation named, settlement named).
// Merging the two into one "net" row would hide the fact that spending and
// returning are different events, so the builder never does it.
//
// The header's settled_total is the sum of the consume legs by definition —
// the builder computes it from the legs it wrote, so caller arithmetic
// cannot disagree with it. Every allocation is re-validated here rather
// than trusted from construction: the fields are exported, and a settlement
// is exactly the place a shortcut must not survive.
//
// One leg pair at most per bucket: the ledger's (settlement_id,
// funding_bucket_id, kind) uniqueness is the schema pinning the one-call
// waterfall this builder exists to keep, so a caller handing two
// allocations for the same bucket is refused here in domain words.
func BuildSettle(settlementID SettlementID, requestID RequestID, allocations []Allocation,
	newEntryID EntryIDMinter, now time.Time) (SettlePlan, error) {
	if err := validateMintedSettlementID(settlementID); err != nil {
		return SettlePlan{}, err
	}
	if err := validateRequestID(requestID); err != nil {
		return SettlePlan{}, fmt.Errorf("accounting: build settle: %w", err)
	}
	if newEntryID == nil {
		return SettlePlan{}, fmt.Errorf("accounting: build settle: %w: nil entry id minter", ErrInvalidReference)
	}
	if len(allocations) == 0 {
		return SettlePlan{}, fmt.Errorf("accounting: build settle: %w: a settlement names at least one allocation", ErrInvalidSettlement)
	}

	seen := make(map[FundingBucketID]struct{}, len(allocations))
	total := Amount(0)
	entries := make([]LedgerEntry, 0, 2*len(allocations))
	for _, alloc := range allocations {
		if _, dup := seen[alloc.BucketID]; dup {
			return SettlePlan{}, fmt.Errorf("accounting: build settle: %w: two allocations name bucket %s",
				ErrInvalidSettlement, alloc.BucketID)
		}
		seen[alloc.BucketID] = struct{}{}

		if _, err := NewAmount(alloc.HeldBooked.Int64()); err != nil {
			return SettlePlan{}, fmt.Errorf("accounting: build settle: allocation of bucket %s: %w", alloc.BucketID, err)
		}
		if err := validateReservationID(alloc.ReservationID); err != nil {
			return SettlePlan{}, fmt.Errorf("accounting: build settle: allocation of bucket %s: %w", alloc.BucketID, err)
		}
		if alloc.Consumed > alloc.HeldBooked {
			return SettlePlan{}, fmt.Errorf("accounting: build settle: %w: allocation of bucket %s consumes %d against %d held",
				ErrInvalidSettlement, alloc.BucketID, alloc.Consumed, alloc.HeldBooked)
		}
		if alloc.Consumed > 0 {
			if err := validatePriceSnapshot(alloc.Price); err != nil {
				return SettlePlan{}, fmt.Errorf("accounting: build settle: allocation of bucket %s: %w", alloc.BucketID, err)
			}
			entryID, err := newEntryID()
			if err != nil {
				return SettlePlan{}, fmt.Errorf("accounting: build settle: new consume entry id: %w", err)
			}
			consumed, err := NewConsumeEntry(entryID, alloc.BucketID, alloc.Consumed, settlementID, alloc.Price, now)
			if err != nil {
				return SettlePlan{}, err
			}
			entries = append(entries, consumed)
			total, err = total.Add(alloc.Consumed)
			if err != nil {
				return SettlePlan{}, fmt.Errorf("accounting: build settle: settled total: %w", err)
			}
		}
		tail, err := alloc.HeldBooked.Sub(alloc.Consumed)
		if err != nil {
			// Unreachable after the Consumed > HeldBooked refusal above; kept
			// loud so a future edit that reorders the guards fails here first.
			return SettlePlan{}, fmt.Errorf("accounting: build settle: allocation tail of bucket %s: %w", alloc.BucketID, err)
		}
		if tail > 0 {
			entryID, err := newEntryID()
			if err != nil {
				return SettlePlan{}, fmt.Errorf("accounting: build settle: new release entry id: %w", err)
			}
			released, err := NewReleaseEntry(entryID, alloc.BucketID, tail, alloc.ReservationID, settlementID, now)
			if err != nil {
				return SettlePlan{}, err
			}
			entries = append(entries, released)
		}
	}

	return SettlePlan{
		Settlement: Settlement{
			ID:           settlementID,
			RequestID:    requestID,
			SettledTotal: total,
			CreatedAt:    now.UTC(),
		},
		Entries: entries,
	}, nil
}
