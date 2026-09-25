package accounting

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// Every test here pins the hold's contract: formed together with the
// waterfall legs that say where its money came from, refusing any shape that
// cannot be re-derived from those legs, never aliasing the caller's slice,
// and closing exactly once. The settlement reads this aggregate to build the
// fact the Control Plane derives its ledger legs from — a hold that cannot
// be re-derived from its legs is a leak, not a memory.

var (
	openedAt     = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	leaseExpires = openedAt.Add(30 * time.Second)
	holdExpires  = openedAt.Add(5 * time.Minute)
	closedAt     = openedAt.Add(4 * time.Minute)
)

// waterfall builds a fresh two-leg split totalling 5000: the shape most
// reservations take, and the one the returned copy is compared against.
func waterfall() []Allocation {
	return []Allocation{
		{FundingBucketID: "bucket-entitlement", Amount: 3000, Ordinal: 1},
		{FundingBucketID: "bucket-payg", Amount: 2000, Ordinal: 2},
	}
}

// openReservation builds the fixture every close test starts from, by way of
// the constructor: a constructor refusal here fails the test — the refusal
// tables below own those cases.
func openReservation(t *testing.T, amount int64, allocations []Allocation) *Reservation {
	t.Helper()
	reservation, err := NewReservation("res-0001", "req-0001", "rev-2026-09", 15, 60, 512, 1024, amount, allocations, openedAt, holdExpires, "admission@host-1", leaseExpires)
	if err != nil {
		t.Fatalf("NewReservation() error = %v", err)
	}
	return &reservation
}

// TestNewReservationOpensALeasedHoldWithItsWaterfall pins the open hold: the
// amount, the pricing basis and the bounds carried whole, the legs stored in
// order as given, the state open, and ClosedAt still zero — a hold that has
// not closed has no close to name. A zero hold with no legs is pinned too:
// it is the one legal hold without a split, and its Allocations must still
// be a usable (empty) slice, never a caller-owned nil.
func TestNewReservationOpensALeasedHoldWithItsWaterfall(t *testing.T) {
	t.Run("a funded hold carries its legs in order", func(t *testing.T) {
		reservation := openReservation(t, 5000, waterfall())

		if reservation.ID != identity.ReservationID("res-0001") || reservation.RequestID != identity.RequestID("req-0001") || reservation.PriceRevision != "rev-2026-09" {
			t.Errorf("identity/basis = (%q, %q, %q), want the constructor's arguments back", reservation.ID, reservation.RequestID, reservation.PriceRevision)
		}
		if reservation.InputUnitPrice != 15 || reservation.OutputUnitPrice != 60 || reservation.InputTokens != 512 || reservation.MaxOutputTokens != 1024 || reservation.ReservedAmount != 5000 {
			t.Errorf("hold figures = (%d, %d, %d, %d, %d), want (15, 60, 512, 1024, 5000)", reservation.InputUnitPrice, reservation.OutputUnitPrice, reservation.InputTokens, reservation.MaxOutputTokens, reservation.ReservedAmount)
		}
		if reservation.State != StateOpen || !reservation.Open() || !reservation.ClosedAt.IsZero() {
			t.Errorf("state = %q, Open = %v, ClosedAt = %v, want open/true/zero", reservation.State, reservation.Open(), reservation.ClosedAt)
		}
		if reservation.LeaseOwner != "admission@host-1" || !reservation.LeaseExpiresAt.Equal(leaseExpires) {
			t.Errorf("lease = (%q, %v), want (admission@host-1, %v) — an open hold is leased to whoever opened it", reservation.LeaseOwner, reservation.LeaseExpiresAt, leaseExpires)
		}
		want := waterfall()
		if len(reservation.Allocations) != len(want) {
			t.Fatalf("Allocations = %+v, want %v", reservation.Allocations, want)
		}
		for i, leg := range want {
			if reservation.Allocations[i] != leg {
				t.Errorf("Allocations[%d] = %+v, want %+v — the waterfall is stored in the order given", i, reservation.Allocations[i], leg)
			}
		}
	})

	t.Run("a zero hold is legal with no legs", func(t *testing.T) {
		reservation := openReservation(t, 0, nil)
		if !reservation.Open() {
			t.Error("Open() = false, want true — a zero hold still opens")
		}
		if len(reservation.Allocations) != 0 {
			t.Errorf("Allocations = %v, want none — a hold of zero has nothing to split", reservation.Allocations)
		}
	})
}

// TestNewReservationRefusesAnUnusableHold pins each constructor refusal in
// isolation: identity, the pricing basis, the non-negative prices, the token
// bounds, the amount, the window the hold outlives its creation within, the
// lease, and the lease's owner. Every refusal has a twin CHECK on the row;
// the point of the table is that the caller reads the reason while forming
// the hold instead of at the write.
func TestNewReservationRefusesAnUnusableHold(t *testing.T) {
	tests := []struct {
		name       string
		id         identity.ReservationID
		requestID  identity.RequestID
		revision   string
		inPrice    int64
		outPrice   int64
		input      int
		maxOut     int
		amount     int64
		expiresAt  time.Time
		leaseOwner string
		leaseEnd   time.Time
		wantErr    error
		message    string
	}{
		{
			name:      "rejects an empty reservation id",
			requestID: "req-0001", revision: "rev-2026-09", inPrice: 15, outPrice: 60,
			input: 512, maxOut: 1024, amount: 5000, expiresAt: holdExpires, leaseOwner: "admission@host-1", leaseEnd: leaseExpires,
			message: "needs its id and its request",
		},
		{
			name: "rejects an empty request id",
			id:   "res-0001", revision: "rev-2026-09", inPrice: 15, outPrice: 60,
			input: 512, maxOut: 1024, amount: 5000, expiresAt: holdExpires, leaseOwner: "admission@host-1", leaseEnd: leaseExpires,
			message: "needs its id and its request",
		},
		{
			name: "rejects a missing pricing basis",
			id:   "res-0001", requestID: "req-0001", inPrice: 15, outPrice: 60,
			input: 512, maxOut: 1024, amount: 5000, expiresAt: holdExpires, leaseOwner: "admission@host-1", leaseEnd: leaseExpires,
			message: "carries its pricing basis",
		},
		{
			name: "rejects a negative input price",
			id:   "res-0001", requestID: "req-0001", revision: "rev-2026-09", inPrice: -1, outPrice: 60,
			input: 512, maxOut: 1024, amount: 5000, expiresAt: holdExpires, leaseOwner: "admission@host-1", leaseEnd: leaseExpires,
			message: "unit prices must not be negative",
		},
		{
			name: "rejects a negative output price",
			id:   "res-0001", requestID: "req-0001", revision: "rev-2026-09", inPrice: 15, outPrice: -1,
			input: 512, maxOut: 1024, amount: 5000, expiresAt: holdExpires, leaseOwner: "admission@host-1", leaseEnd: leaseExpires,
			message: "unit prices must not be negative",
		},
		{
			name: "rejects negative input tokens",
			id:   "res-0001", requestID: "req-0001", revision: "rev-2026-09", inPrice: 15, outPrice: 60,
			input: -1, maxOut: 1024, amount: 5000, expiresAt: holdExpires, leaseOwner: "admission@host-1", leaseEnd: leaseExpires,
			message: "non-negative input and a positive ceiling",
		},
		{
			name: "rejects a zero output ceiling",
			id:   "res-0001", requestID: "req-0001", revision: "rev-2026-09", inPrice: 15, outPrice: 60,
			input: 512, maxOut: 0, amount: 5000, expiresAt: holdExpires, leaseOwner: "admission@host-1", leaseEnd: leaseExpires,
			message: "non-negative input and a positive ceiling",
		},
		{
			name: "rejects a negative amount",
			id:   "res-0001", requestID: "req-0001", revision: "rev-2026-09", inPrice: 15, outPrice: 60,
			input: 512, maxOut: 1024, amount: -1, expiresAt: holdExpires, leaseOwner: "admission@host-1", leaseEnd: leaseExpires,
			wantErr: ErrNegativeAmount,
		},
		{
			name: "rejects a hold that does not outlive its creation",
			id:   "res-0001", requestID: "req-0001", revision: "rev-2026-09", inPrice: 15, outPrice: 60,
			input: 512, maxOut: 1024, amount: 5000, expiresAt: openedAt, leaseOwner: "admission@host-1", leaseEnd: leaseExpires,
			message: "must outlive its creation",
		},
		{
			name: "rejects an empty lease owner",
			id:   "res-0001", requestID: "req-0001", revision: "rev-2026-09", inPrice: 15, outPrice: 60,
			input: 512, maxOut: 1024, amount: 5000, expiresAt: holdExpires, leaseEnd: leaseExpires,
			message: "leased to whoever opened it",
		},
		{
			name: "rejects a lease that expires before the hold exists",
			id:   "res-0001", requestID: "req-0001", revision: "rev-2026-09", inPrice: 15, outPrice: 60,
			input: 512, maxOut: 1024, amount: 5000, expiresAt: holdExpires, leaseOwner: "admission@host-1", leaseEnd: openedAt.Add(-time.Second),
			message: "cannot expire before the reservation exists",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reservation, err := NewReservation(tt.id, tt.requestID, tt.revision, tt.inPrice, tt.outPrice, tt.input, tt.maxOut, tt.amount, waterfall(), openedAt, tt.expiresAt, tt.leaseOwner, tt.leaseEnd)
			if err == nil {
				t.Fatalf("NewReservation() = %+v, want an error", reservation)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("NewReservation() error = %v, want it to wrap %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && !strings.Contains(err.Error(), tt.message) {
				t.Errorf("NewReservation() error = %q, want it to contain %q", err, tt.message)
			}
		})
	}
}

// TestNewReservationRefusesWaterfallsThatDoNotFormTheHold pins every shape
// validateAllocations exists for, each refused with ErrAllocationShape:
// legs that do not sum to the hold (over or under), a hole in the ordinal
// list, an ordinal list not starting at 1, a non-positive leg, an empty or
// duplicated bucket. The runtime's memory of its drawdown is the
// settlement's input, so a hold that cannot be re-derived from its legs is
// refused before it exists rather than discovered missing at settlement.
func TestNewReservationRefusesWaterfallsThatDoNotFormTheHold(t *testing.T) {
	tests := []struct {
		name        string
		amount      int64
		allocations []Allocation
	}{
		{
			name:   "a funded hold with no legs",
			amount: 5000,
		},
		{
			name:        "a zero hold carrying legs",
			amount:      0,
			allocations: waterfall(),
		},
		{
			name:        "a leg of zero",
			amount:      5000,
			allocations: []Allocation{{FundingBucketID: "bucket-a", Amount: 0, Ordinal: 1}},
		},
		{
			name:        "a negative leg",
			amount:      5000,
			allocations: []Allocation{{FundingBucketID: "bucket-a", Amount: -5, Ordinal: 1}},
		},
		{
			name:        "an ordinal list not starting at one",
			amount:      5000,
			allocations: []Allocation{{FundingBucketID: "bucket-a", Amount: 5000, Ordinal: 2}},
		},
		{
			name:   "a hole in the ordinal list",
			amount: 5000,
			allocations: []Allocation{
				{FundingBucketID: "bucket-a", Amount: 3000, Ordinal: 1},
				{FundingBucketID: "bucket-b", Amount: 2000, Ordinal: 3},
			},
		},
		{
			name:   "an empty bucket id",
			amount: 5000,
			allocations: []Allocation{
				{FundingBucketID: "", Amount: 5000, Ordinal: 1},
			},
		},
		{
			name:   "a duplicated bucket",
			amount: 5000,
			allocations: []Allocation{
				{FundingBucketID: "bucket-a", Amount: 3000, Ordinal: 1},
				{FundingBucketID: "bucket-a", Amount: 2000, Ordinal: 2},
			},
		},
		{
			name:        "legs over the hold",
			amount:      4000,
			allocations: waterfall(), // 3000 + 2000
		},
		{
			name:        "legs under the hold",
			amount:      6000,
			allocations: waterfall(), // 3000 + 2000
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewReservation("res-0001", "req-0001", "rev-2026-09", 15, 60, 512, 1024, tt.amount, tt.allocations, openedAt, holdExpires, "admission@host-1", leaseExpires)
			if !errors.Is(err, ErrAllocationShape) {
				t.Errorf("NewReservation() error = %v, want ErrAllocationShape", err)
			}
		})
	}
}

// TestNewReservationKeepsPositionOrderAndItsOwnCopy pins what the code does
// with order, both directions: legs whose ordinals do not match their
// positions are refused rather than silently reordered (the constructor is a
// shape check, not a normaliser — a reordered waterfall would change which
// bucket a release returns to first), and legs in position order are stored
// in a slice the aggregate owns, so the caller sorting or rewriting its own
// slice afterwards cannot move the reservation's legs out from under it.
func TestNewReservationKeepsPositionOrderAndItsOwnCopy(t *testing.T) {
	t.Run("refuses out-of-order legs instead of reordering them", func(t *testing.T) {
		reversed := []Allocation{
			{FundingBucketID: "bucket-payg", Amount: 2000, Ordinal: 2},
			{FundingBucketID: "bucket-entitlement", Amount: 3000, Ordinal: 1},
		}
		_, err := NewReservation("res-0001", "req-0001", "rev-2026-09", 15, 60, 512, 1024, 5000, reversed, openedAt, holdExpires, "admission@host-1", leaseExpires)
		if !errors.Is(err, ErrAllocationShape) {
			t.Errorf("NewReservation() with position-reversed legs error = %v, want ErrAllocationShape — order is refused, never repaired", err)
		}
	})

	t.Run("mutating the caller's slice afterwards leaves the hold intact", func(t *testing.T) {
		legs := waterfall()
		reservation := openReservation(t, 5000, legs)
		frozen := append([]Allocation(nil), reservation.Allocations...)

		legs[0] = Allocation{FundingBucketID: "mutated", Amount: 1, Ordinal: 99}
		legs[1] = Allocation{FundingBucketID: "mutated-too", Amount: 2, Ordinal: 98}

		if len(reservation.Allocations) != len(frozen) {
			t.Fatalf("Allocations shrank or grew to %+v, want the two legs as formed", reservation.Allocations)
		}
		for i, leg := range frozen {
			if reservation.Allocations[i] != leg {
				t.Errorf("Allocations[%d] = %+v, want %+v — the caller's mutation must not reach the aggregate", i, reservation.Allocations[i], leg)
			}
		}
	})
}

// closers names the three ways out of open, uniformly, so the close-once
// matrix below can drive each against each.
func closers() map[string]func(*Reservation, time.Time) error {
	return map[string]func(*Reservation, time.Time) error{
		"settle":  func(r *Reservation, at time.Time) error { return r.Settle(at) },
		"release": func(r *Reservation, at time.Time) error { return r.Release(at) },
		"expire":  func(r *Reservation, at time.Time) error { return r.Expire(at) },
	}
}

// wantState names the state each closer lands on.
func wantState(closer string) State {
	switch closer {
	case "settle":
		return StateSettled
	case "release":
		return StateReleased
	default:
		return StateExpired
	}
}

// TestAReservationClosesExactlyOnce pins the one transition out of open over
// the full 3x3 matrix: each closer lands its own terminal state and stamps
// the close time once, Open() turns false with it, and every later close —
// by any of the three verbs, including the one that just succeeded — answers
// ErrReservationClosed and leaves the first close standing. The settlement's
// fact and the reaper's fact both derive from this close; a silent second
// close would rewrite which one the consumer reads.
func TestAReservationClosesExactlyOnce(t *testing.T) {
	for closerName, closer := range closers() {
		t.Run(closerName, func(t *testing.T) {
			reservation := openReservation(t, 5000, waterfall())

			if !reservation.Open() {
				t.Fatal("Open() = false before any close, want true")
			}
			if err := closer(reservation, closedAt); err != nil {
				t.Fatalf("%s() error = %v", closerName, err)
			}
			if reservation.State != wantState(closerName) || !reservation.ClosedAt.Equal(closedAt) {
				t.Errorf("state = %q, ClosedAt = %v, want %q at %v", reservation.State, reservation.ClosedAt, wantState(closerName), closedAt)
			}
			if reservation.Open() {
				t.Error("Open() = true after a close, want false")
			}

			for secondName, second := range closers() {
				if err := second(reservation, closedAt.Add(time.Minute)); !errors.Is(err, ErrReservationClosed) {
					t.Errorf("second %s after %s = %v, want ErrReservationClosed", secondName, closerName, err)
				}
			}
			if reservation.State != wantState(closerName) || !reservation.ClosedAt.Equal(closedAt) {
				t.Errorf("state = %q, ClosedAt = %v, want the first close standing (%q at %v)", reservation.State, reservation.ClosedAt, wantState(closerName), closedAt)
			}
		})
	}
}
