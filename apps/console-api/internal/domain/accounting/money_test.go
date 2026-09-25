package accounting

import (
	"errors"
	"math"
	"testing"
)

// Money is the package's first invariant: fixed-point integer minor units,
// refused at the boundaries this file pins. Every test here is one clause of
// that invariant — positivity, no wrap, no negative magnitude, signed
// balances that only an adjustment can push below zero.

func TestNewAmountRefusesNonPositiveMagnitudes(t *testing.T) {
	for _, raw := range []int64{0, -1, math.MinInt64} {
		if _, err := NewAmount(raw); !errors.Is(err, ErrInvalidAmount) {
			t.Fatalf("NewAmount(%d) = %v, want ErrInvalidAmount: zero and negatives are not movements", raw, err)
		}
	}
	if a, err := NewAmount(1); err != nil || a != Amount(1) {
		t.Fatalf("NewAmount(1) = %d, %v; the smallest magnitude must be accepted", a, err)
	}
	if a, err := NewAmount(math.MaxInt64); err != nil || a != Amount(math.MaxInt64) {
		t.Fatalf("NewAmount(max int64) = %d, %v; the ceiling itself is a legal magnitude", a, err)
	}
}

func TestAmountAddRefusesTheOverflowInsteadOfWrapping(t *testing.T) {
	a, _ := NewAmount(math.MaxInt64)
	b, _ := NewAmount(1)

	got, err := a.Add(b)
	if !errors.Is(err, ErrAmountRange) {
		t.Fatalf("max + 1 = %d, %v; want ErrAmountRange, never a wrapped amount", got, err)
	}
}

func TestAmountAddSumsOrdinaryMagnitudes(t *testing.T) {
	a, _ := NewAmount(80)
	b, _ := NewAmount(20)

	got, err := a.Add(b)
	if err != nil || got != Amount(100) {
		t.Fatalf("80 + 20 = %d, %v; want 100", got, err)
	}
}

func TestAmountSubRefusesGoingBelowZero(t *testing.T) {
	a, _ := NewAmount(20)
	b, _ := NewAmount(80)

	if _, err := a.Sub(b); !errors.Is(err, ErrAmountRange) {
		t.Fatalf("20 - 80 = %v; want ErrAmountRange: a magnitude cannot be negative", err)
	}
}

func TestAmountSubTakesExactDifference(t *testing.T) {
	a, _ := NewAmount(80)
	b, _ := NewAmount(20)

	got, err := a.Sub(b)
	if err != nil || got != Amount(60) {
		t.Fatalf("80 - 20 = %d, %v; want 60", got, err)
	}
}

func TestDeltaAbsIsTheMagnitudeOfTheStatedDirection(t *testing.T) {
	if got := Delta(-30).Abs(); got != Amount(30) {
		t.Fatalf("Abs(-30) = %d, want 30", got)
	}
	if got := Delta(30).Abs(); got != Amount(30) {
		t.Fatalf("Abs(30) = %d, want 30", got)
	}
	if got := Delta(0).Abs(); got != Amount(0) {
		t.Fatalf("Abs(0) = %d, want 0", got)
	}
}

func TestBalanceMoveRefusesEitherOverflow(t *testing.T) {
	up := Balance(math.MaxInt64)
	if _, err := up.Move(1); !errors.Is(err, ErrAmountRange) {
		t.Fatalf("max balance + 1 = %v; want ErrAmountRange", err)
	}

	down := Balance(math.MinInt64)
	if _, err := down.Move(-1); !errors.Is(err, ErrAmountRange) {
		t.Fatalf("min balance - 1 = %v; want ErrAmountRange", err)
	}
}

func TestBalanceMoveAppliesOrdinaryDeltasBothDirections(t *testing.T) {
	b := BalanceOf(Amount(100))
	if got, err := b.Move(-30); err != nil || got != Balance(70) {
		t.Fatalf("100 - 30 = %d, %v; want 70", got, err)
	}
	if got, err := Balance(70).Move(30); err != nil || got != Balance(100) {
		t.Fatalf("70 + 30 = %d, %v; want 100", got, err)
	}
}

func TestBalanceSubRefusesOverflowInEitherDirection(t *testing.T) {
	floor := Balance(math.MinInt64)
	if _, err := floor.Sub(Balance(1)); !errors.Is(err, ErrAmountRange) {
		t.Fatalf("min - 1 = %v; want ErrAmountRange", err)
	}
	ceiling := Balance(math.MaxInt64)
	if _, err := ceiling.Sub(Balance(-1)); !errors.Is(err, ErrAmountRange) {
		t.Fatalf("max - (-1) = %v; want ErrAmountRange", err)
	}
}

func TestBalanceIsNegativeNamesTheOnlyLegalNegative(t *testing.T) {
	if !(Balance(-1)).IsNegative() {
		t.Fatal("Balance(-1).IsNegative() = false; a negative balance must be nameable")
	}
	if (Balance(0)).IsNegative() || (Balance(1)).IsNegative() {
		t.Fatal("zero and positive balances must not read as negative")
	}
}
