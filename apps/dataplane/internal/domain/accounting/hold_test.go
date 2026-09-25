package accounting

import (
	"errors"
	"math"
	"math/big"
	"testing"
	"time"
)

// The hold tests pin ADR 0003's formula from its two load-bearing corners:
// the ONE ceiling over the summed raw product (never per leg), and checked
// arithmetic that reports overflow instead of wrapping. Every expected value
// below is computed a second time in arbitrary precision (math/big), so the
// test pins the formula, not an implementation.

func TestHoldAppliesOneCeilingOverTheSummedProduct(t *testing.T) {
	tests := []struct {
		name      string
		inTokens  int
		outTokens int
		inPrice   int64
		outPrice  int64
		want      int64
		wantErr   error
	}{
		{name: "exact multiples on both legs", inTokens: 1_000_000, outTokens: 2_000_000, inPrice: 1500, outPrice: 6000, want: 13500},
		{name: "a fractional ceiling over the sum", inTokens: 999_999, outTokens: 0, inPrice: 1, outPrice: 0, want: 1},
		{
			// The flagship single-ceiling case: two legs each just under the
			// scale. Two per-leg ceilings would charge 2; the one ceiling
			// charges 1.
			name:     "two fractional legs ceiling once, not per leg",
			inTokens: 499_999, outTokens: 500_000, inPrice: 1, outPrice: 1, want: 1,
		},
		{
			// The same case three tokens over the scale, where the ceiling
			// finally moves: the sum, not a leg, decides. (Exactly at the
			// scale the ceiling is still 1 — no remainder, no rounding.)
			name:     "the ceiling moves only when the sum crosses the scale",
			inTokens: 499_999, outTokens: 500_002, inPrice: 1, outPrice: 1, want: 2,
		},
		{name: "a zero-priced input leg drops out of the sum", inTokens: 5_000_000, outTokens: 1, inPrice: 0, outPrice: 2_000_000, want: 2},
		{name: "a free model holds nothing", inTokens: 10_000, outTokens: 10_000, inPrice: 0, outPrice: 0, want: 0},
		{name: "no tokens hold nothing", inTokens: 0, outTokens: 0, inPrice: 1500, outPrice: 6000, want: 0},
		{
			// 128-bit territory: each product alone is far past 64 bits, the
			// sum of the two legs still divides to a legal hold.
			name:     "legs whose raw products exceed 64 bits sum to a legal hold",
			inTokens: 1_000_000_000, outTokens: 1_000_000_000, inPrice: 4_000_000_000, outPrice: 5_000_000_000, want: 9_000_000_000_000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Hold(tt.inTokens, tt.outTokens, tt.inPrice, tt.outPrice)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Hold() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Hold() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("Hold() = %d, want %d per the arbitrary-precision derivation", got, tt.want)
			}
			if want := bigCeiling(t, tt.inTokens, tt.outTokens, tt.inPrice, tt.outPrice); got != want {
				t.Errorf("Hold() = %d, want %d from math/big", got, want)
			}
		})
	}
}

// bigCeiling re-derives the expected hold in arbitrary precision: the same
// formula, computed with no overflow possible, so a wrapped or per-leg
// implementation cannot agree with it by accident.
func bigCeiling(t *testing.T, inTokens, outTokens int, inPrice, outPrice int64) int64 {
	t.Helper()
	scale := big.NewInt(1_000_000)
	sum := new(big.Int).Mul(big.NewInt(int64(inTokens)), big.NewInt(inPrice))
	sum.Add(sum, new(big.Int).Mul(big.NewInt(int64(outTokens)), big.NewInt(outPrice)))
	// ceil(sum/scale) = (sum + scale - 1) / scale, on non-negative values.
	sum.Add(sum, new(big.Int).Sub(scale, big.NewInt(1)))
	sum.Div(sum, scale)
	if !sum.IsInt64() {
		t.Fatalf("derived hold %s does not fit an int64; the case belongs in the overflow table", sum)
	}
	return sum.Int64()
}

func TestHoldRefusesNegativeInputs(t *testing.T) {
	tests := []struct {
		name      string
		inTokens  int
		outTokens int
		inPrice   int64
		outPrice  int64
	}{
		{name: "negative input tokens", inTokens: -1, outTokens: 1, inPrice: 1, outPrice: 1},
		{name: "negative output tokens", inTokens: 1, outTokens: -1, inPrice: 1, outPrice: 1},
		{name: "negative input price", inTokens: 1, outTokens: 1, inPrice: -1, outPrice: 1},
		{name: "negative output price", inTokens: 1, outTokens: 1, inPrice: 1, outPrice: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hold, err := Hold(tt.inTokens, tt.outTokens, tt.inPrice, tt.outPrice)
			if !errors.Is(err, ErrHoldInputs) {
				t.Fatalf("Hold() error = %v, want ErrHoldInputs", err)
			}
			if hold != 0 {
				t.Errorf("Hold() = %d, want 0 beside the error", hold)
			}
		})
	}
}

func TestHoldReportsOverflowInsteadOfWrapping(t *testing.T) {
	tests := []struct {
		name      string
		inTokens  int
		outTokens int
		inPrice   int64
		outPrice  int64
	}{
		{
			// A true hold just past 2^63: the wrapped int64 arithmetic would
			// answer a small positive number here, which is precisely the
			// invented money the check exists to refuse. (MaxInt32 tokens at
			// 5e15 minor units per million prices the input alone past the
			// representable hold.)
			name:     "a hold past MaxInt64",
			inTokens: math.MaxInt32, outTokens: 0, inPrice: 5_000_000_000_000_000, outPrice: 0,
		},
		{
			// A raw sum past 128 bits: both legs saturate their high words.
			name:     "a raw sum past 128 bits",
			inTokens: math.MaxInt32, outTokens: math.MaxInt32, inPrice: math.MaxInt64, outPrice: math.MaxInt64,
		},
		{
			// The floor quotient is exactly MaxInt64 — representable — and
			// the one ceiling step for the remainder pushes it out. The
			// wrapped arithmetic would answer MinInt64 here.
			name:     "the ceiling itself overflows",
			inTokens: 0, outTokens: math.MaxInt64, inPrice: 0, outPrice: 1_000_001,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hold, err := Hold(tt.inTokens, tt.outTokens, tt.inPrice, tt.outPrice)
			if !errors.Is(err, ErrHoldOverflow) {
				t.Fatalf("Hold() = (%d, %v), want ErrHoldOverflow", hold, err)
			}
			if hold != 0 {
				t.Errorf("Hold() = %d, want 0 beside the error", hold)
			}
		})
	}
}

// TestHoldNeverUnderstatesASmallerExecution pins the direction property the
// reservation depends on: a hold computed from the reserved bounds is at
// least the settled price of any execution that stayed inside them. Actual
// usage never exceeds the reserved input and output counts, the prices are
// the frozen snapshot's, and the ceiling is monotone — so a settlement can
// never price above its hold.
func TestHoldNeverUnderstatesASmallerExecution(t *testing.T) {
	const inPrice, outPrice = 3711, 15_003
	reservedIn, reservedOut := 9_000, 4_000
	reserved, err := Hold(reservedIn, reservedOut, inPrice, outPrice)
	if err != nil {
		t.Fatalf("Hold() error = %v", err)
	}
	for actualIn := 0; actualIn <= reservedIn; actualIn += 137 {
		for actualOut := 0; actualOut <= reservedOut; actualOut += 91 {
			settled, err := Hold(actualIn, actualOut, inPrice, outPrice)
			if err != nil {
				t.Fatalf("Hold(%d, %d) error = %v, want nil below the reserved bounds", actualIn, actualOut, err)
			}
			if settled > reserved {
				t.Fatalf("settled %d exceeds the reserved hold %d at (%d, %d) tokens", settled, reserved, actualIn, actualOut)
			}
		}
	}
}

// TestReDerivedHoldMatchesTheReservedAmount pins the client-side half of the
// re-derivation invariant: a reservation built through the domain constructor
// from Hold's own answer re-derives to itself, and a reservation whose stored
// amount does not follow from its own columns re-derives to the difference —
// which is exactly the mismatch the application's assertion exists to catch
// before the write.
func TestReDerivedHoldMatchesTheReservedAmount(t *testing.T) {
	const inPrice, outPrice = 1500, 6000
	hold, err := Hold(2_000_000, 1_000_000, inPrice, outPrice)
	if err != nil {
		t.Fatalf("Hold() error = %v", err)
	}
	if hold != 9_000 {
		t.Fatalf("Hold() = %d, want 9000 for the fixture to be legible", hold)
	}
	reservation, err := NewReservation(
		"res-0001", "req-0001", "rev-0001", inPrice, outPrice, 2_000_000, 1_000_000, hold,
		[]Allocation{{FundingBucketID: "bucket-0001", Amount: hold, Ordinal: 1}},
		time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 25, 12, 5, 0, 0, time.UTC),
		"owner-0001",
		time.Date(2026, 9, 25, 12, 1, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("NewReservation() error = %v", err)
	}
	reDerived, err := reservation.ReDerivedHold()
	if err != nil {
		t.Fatalf("ReDerivedHold() error = %v", err)
	}
	if reDerived != reservation.ReservedAmount {
		t.Errorf("ReDerivedHold() = %d, want the reserved %d", reDerived, reservation.ReservedAmount)
	}

	// A stored amount its own columns do not support: the constructor accepts
	// it (the legs match the amount it was given), and the re-derivation is
	// the assertion that refuses it.
	wrong, err := NewReservation(
		"res-0002", "req-0002", "rev-0001", inPrice, outPrice, 2_000_000, 1_000_000, 9_999,
		[]Allocation{{FundingBucketID: "bucket-0002", Amount: 9_999, Ordinal: 1}},
		time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 25, 12, 5, 0, 0, time.UTC),
		"owner-0002",
		time.Date(2026, 9, 25, 12, 1, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("NewReservation() error = %v", err)
	}
	reDerived, err = wrong.ReDerivedHold()
	if err != nil {
		t.Fatalf("ReDerivedHold() error = %v", err)
	}
	if reDerived == wrong.ReservedAmount {
		t.Errorf("ReDerivedHold() = %d, want the mismatched derivation the assertion exists to catch", reDerived)
	}
}
