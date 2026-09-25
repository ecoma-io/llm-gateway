// Package accounting is the domain of the runtime's own money memory: the
// reservation a request opens on future spend, the waterfall legs that
// reservation was drawn down against, the lockable quota projection each leg
// names, and the usage fact the whole of it eventually becomes.
//
// None of it is a financial record. The Control Plane's ledger owns money;
// this package owns the runtime's memory of what it held and what it did with
// it — memory exact enough that the fact derived from it settles the same
// request the same way no matter who reads it, and never authoritative past
// that (ADR 0004). A Reservation is not a Ledger Entry; a quota projection is
// a lock, not a balance; a fact is the boundary where the runtime stops
// deciding and the Control Plane starts deriving.
//
// The package mirrors its tables the way package execution does: every
// invariant here has a twin constraint in migration 000002_runtime_storage,
// the database as the final guard and this package as the legible refusal.
package accounting

import (
	"errors"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// State is a reservation's life, and it has exactly one way out of open:
// settled (the request completed and the hold became usage), released (the
// request finalised without settleable usage and the hold went back), expired
// (the reaper closed a hold whose lease and window both lapsed). Each state is
// terminal; there is no path between terminal states, and the database's
// trigger behind the CAS refuses any attempt to drive one.
type State string

const (
	StateOpen     State = "open"
	StateSettled  State = "settled"
	StateReleased State = "released"
	StateExpired  State = "expired"
)

// ErrReservationClosed is every close attempted on a reservation that already
// closed. A reservation closes exactly once, and the second closer loses to
// the first by design — the CAS in the repository decides the winner, and the
// loser reads this sentinel to know its decision was not the one that stood.
var ErrReservationClosed = errors.New("accounting: reservation is already closed")

// ErrAllocationShape marks a hold whose waterfall legs do not add up: legs
// that do not sum to the reserved amount, an ordinal list with a hole, an
// empty leg list on a non-zero hold, or a hold of zero. The runtime's memory
// of its own drawdown is the settlement's input; a hold that cannot be
// re-derived from its legs is not a memory, it is a leak.
var ErrAllocationShape = errors.New("accounting: reservation allocations do not form the hold")

// ErrNegativeAmount marks a reserved amount below zero. A hold is a number of
// minor units set aside; a negative one is a refund wearing a hold's name, and
// refunds are nobody's decision at admission.
var ErrNegativeAmount = errors.New("accounting: reserved amount must not be negative")

// Reservation is one request's hold on future spend: the amount drawn down at
// admission, the legs that say which funding buckets carried it and in what
// order, and the pricing basis it was derived from.
//
// The pricing snapshot rides on the reservation for the reason the request's
// does: reserved_amount was derived from exactly these numbers, and the
// reservation outlives any catalog change that would make a join lie.
type Reservation struct {
	ID              identity.ReservationID
	RequestID       identity.RequestID
	PriceRevision   string
	InputUnitPrice  int64
	OutputUnitPrice int64
	InputTokens     int
	MaxOutputTokens int
	ReservedAmount  int64
	// Allocations are the waterfall split, ordered by Ordinal from 1 with no
	// holes: the order the buckets were drawn from, which is the order any
	// release must return to. They are part of the aggregate, not a child
	// table's concern, because a hold without its split cannot be settled.
	Allocations []Allocation

	State          State
	CreatedAt      time.Time
	ExpiresAt      time.Time
	LeaseOwner     string
	LeaseExpiresAt time.Time
	ClosedAt       time.Time
}

// Allocation is one leg of the waterfall split: how much of the hold came
// from one funding bucket, and in which position of the order.
type Allocation struct {
	FundingBucketID string
	Amount          int64
	Ordinal         int
}

// NewReservation opens the hold admission draws down: amount and legs are
// formed together, and a hold whose legs disagree with its amount is refused
// before it exists.
func NewReservation(id identity.ReservationID, requestID identity.RequestID, priceRevision string, inputUnitPrice, outputUnitPrice int64, inputTokens, maxOutputTokens int, reservedAmount int64, allocations []Allocation, createdAt, expiresAt time.Time, leaseOwner string, leaseExpiresAt time.Time) (Reservation, error) {
	if id == "" || requestID == "" {
		return Reservation{}, errors.New("accounting: a reservation needs its id and its request")
	}
	if priceRevision == "" {
		return Reservation{}, errors.New("accounting: a reservation carries its pricing basis")
	}
	if inputUnitPrice < 0 || outputUnitPrice < 0 {
		return Reservation{}, errors.New("accounting: unit prices must not be negative")
	}
	if inputTokens < 0 || maxOutputTokens <= 0 {
		return Reservation{}, errors.New("accounting: token bounds must be a non-negative input and a positive ceiling")
	}
	if reservedAmount < 0 {
		return Reservation{}, ErrNegativeAmount
	}
	if !expiresAt.After(createdAt) {
		return Reservation{}, errors.New("accounting: a reservation must outlive its creation")
	}
	if leaseExpiresAt.Before(createdAt) {
		return Reservation{}, errors.New("accounting: the lease cannot expire before the reservation exists")
	}
	if leaseOwner == "" {
		return Reservation{}, errors.New("accounting: an open reservation is leased to whoever opened it")
	}
	legs, err := validateAllocations(allocations, reservedAmount)
	if err != nil {
		return Reservation{}, err
	}
	return Reservation{
		ID:              id,
		RequestID:       requestID,
		PriceRevision:   priceRevision,
		InputUnitPrice:  inputUnitPrice,
		OutputUnitPrice: outputUnitPrice,
		InputTokens:     inputTokens,
		MaxOutputTokens: maxOutputTokens,
		ReservedAmount:  reservedAmount,
		Allocations:     legs,
		State:           StateOpen,
		CreatedAt:       createdAt,
		ExpiresAt:       expiresAt,
		LeaseOwner:      leaseOwner,
		LeaseExpiresAt:  leaseExpiresAt,
	}, nil
}

// validateAllocations enforces the leg shape the settlement re-derives the
// hold from: contiguous ordinals from 1, positive amounts, distinct buckets,
// and a sum equal to the hold. It returns a normalised copy — the aggregate
// never aliases the caller's slice, because a caller that later sorts its own
// slice must not sort a reservation's legs out from under it.
func validateAllocations(allocations []Allocation, reservedAmount int64) ([]Allocation, error) {
	if reservedAmount == 0 {
		if len(allocations) != 0 {
			return nil, ErrAllocationShape
		}
		return []Allocation{}, nil
	}
	if len(allocations) == 0 {
		return nil, ErrAllocationShape
	}
	legs := make([]Allocation, len(allocations))
	copy(legs, allocations)
	var sum int64
	seen := make(map[string]bool, len(legs))
	for i, leg := range legs {
		if leg.Amount <= 0 {
			return nil, ErrAllocationShape
		}
		if leg.Ordinal != i+1 {
			return nil, ErrAllocationShape
		}
		if leg.FundingBucketID == "" || seen[leg.FundingBucketID] {
			return nil, ErrAllocationShape
		}
		seen[leg.FundingBucketID] = true
		sum += leg.Amount
	}
	if sum != reservedAmount {
		return nil, ErrAllocationShape
	}
	return legs, nil
}

// Settle closes the hold the way a completed request does: the hold became
// usage, the usage became a settled fact, and the fact — not the reservation —
// is what the Control Plane will derive its ledger legs from. The close and
// the fact append are one unit of work at the repository, the append being
// that unit's last statement.
func (r *Reservation) Settle(closedAt time.Time) error {
	return r.close(StateSettled, closedAt)
}

// Release closes the hold the way a finalisation without settleable usage
// does: everything drawn down goes back. Nothing here computes the return —
// the projection's available is the repository's conditional UPDATE — but the
// fact this close produces carries the legs in its payload, which is where the
// return derives them from.
func (r *Reservation) Release(closedAt time.Time) error {
	return r.close(StateReleased, closedAt)
}

// Expire closes the hold the way the reaper does. The reaper's claim is
// conditional in the repository — state open, lease lapsed — and unconditional
// here, because the caller that invokes it has already checked; the CAS is
// what makes the claim safe, this method is what makes it legible.
func (r *Reservation) Expire(closedAt time.Time) error {
	return r.close(StateExpired, closedAt)
}

// close is the one transition out of open, and it runs once.
func (r *Reservation) close(state State, closedAt time.Time) error {
	if r.State != StateOpen {
		return ErrReservationClosed
	}
	r.State = state
	r.ClosedAt = closedAt
	return nil
}

// Open reports whether the hold is still held. The settlement path asks this
// before it builds its fact; the reaper asks it through its CAS.
func (r Reservation) Open() bool {
	return r.State == StateOpen
}
