// Package accounting is the money domain: funding buckets, the ledger and
// its six leg kinds, settlements of record, and the reserve-and-settle
// primitives the Control Plane's half of ADR 0004 is written in.
//
// The package holds the algebra and nothing else. It does not know SQL, it
// does not know HTTP, and it does not know which currency the deployment
// settles in — amounts are integer minor units everywhere, and the single
// platform-wide settlement currency is configuration that lives above this
// package (ADR 0004's recorded consequence: no per-row currency column
// exists, so no per-row currency type does either).
package accounting

import (
	"errors"
	"fmt"
	"math"
)

// The money sentinels. Everything a caller can branch on is here; the
// constructors wrap them with the context that names the rule.
var (
	// ErrInvalidAmount is a magnitude that is not strictly positive: a grant
	// of zero, a hold of a negative amount, a price of nothing.
	ErrInvalidAmount = errors.New("invalid amount")
	// ErrAmountRange is arithmetic that leaves the int64 range amounts are
	// defined on. Money never wraps: a balance move that would overflow is
	// refused, not carried.
	ErrAmountRange = errors.New("amount out of range")
)

// Amount is a strictly positive number of minor units — the magnitude of one
// movement of money. Direction never lives in an Amount: the ledger leg's
// kind states it, and the signed counterpart is Delta. An int64 of minor
// units is far past any balance a gateway will hold, which is exactly why
// overflow is a refused error rather than a wrap.
type Amount int64

// NewAmount validates raw as a movement magnitude: strictly positive.
func NewAmount(raw int64) (Amount, error) {
	if raw <= 0 {
		return 0, fmt.Errorf("accounting: %w: %d is not a positive minor-unit amount", ErrInvalidAmount, raw)
	}
	return Amount(raw), nil
}

// validateAmountAtOrAboveZero accepts a magnitude of zero where zero is a
// value and not the absence of one: a zero-priced model books a hold of
// nothing, settles for nothing, and is still a settlement of record. Every
// leg that moves money keeps NewAmount's strictly-positive rule — money that
// moves is money that exists — while the figures that describe a price or a
// slice of a hold may legitimately be nothing at all.
func validateAmountAtOrAboveZero(raw int64) error {
	if raw < 0 {
		return fmt.Errorf("accounting: %w: %d is not a minor-unit amount", ErrInvalidAmount, raw)
	}
	return nil
}

// Int64 returns the amount as the plain integer the database stores.
func (a Amount) Int64() int64 { return int64(a) }

// Add returns a plus b, refusing to wrap past the int64 ceiling. b is
// required to be non-negative: Amount models magnitudes, and a negative
// addend would turn a sum of movements into a difference without ever
// touching the overflow guards that only look upward.
func (a Amount) Add(b Amount) (Amount, error) {
	if b < 0 {
		return 0, fmt.Errorf("accounting: %w: %d is not a magnitude to add", ErrInvalidAmount, b)
	}
	if b > 0 && a > Amount(math.MaxInt64)-b {
		return 0, fmt.Errorf("accounting: %w: %d + %d overflows int64", ErrAmountRange, a, b)
	}
	return a + b, nil
}

// Sub returns a minus b, refusing to go below zero: the magnitudes this type
// models are balances and movements, and a negative one is either a bug or —
// deliberately — an adjustment, which travels as a Delta instead. b is
// required to be non-negative for the same reason Add requires it: a
// negative subtrahend would negate into a negative result, and the
// `b > a` guard above it cannot see a wrapped sum.
func (a Amount) Sub(b Amount) (Amount, error) {
	if b < 0 {
		return 0, fmt.Errorf("accounting: %w: %d is not a magnitude to subtract", ErrInvalidAmount, b)
	}
	if b > a {
		return 0, fmt.Errorf("accounting: %w: %d - %d would go below zero", ErrAmountRange, a, b)
	}
	return a - b, nil
}

// Delta is a signed number of minor units: how one ledger leg moves one of a
// bucket's two cached balances. Positive is into the balance, negative is
// out. Adjustments state their deltas directly; every other kind's deltas
// are the algebra's, fixed by the kind (ADR 0004).
type Delta int64

// Int64 returns the delta as the plain signed integer the database stores.
func (d Delta) Int64() int64 { return int64(d) }

// Abs returns the delta's magnitude, refusing the one value whose magnitude
// does not fit an Amount: negating math.MinInt64 wraps back to itself, and a
// wrapped magnitude would file an adjustment leg under an amount the caller
// never stated. An adjustment states exactly one non-zero delta, so its leg
// amount is the magnitude of that one — and the schema's amount > 0 would
// have caught the wrap downstream; this is the same refusal one layer up,
// where the error can name the delta that caused it.
func (d Delta) Abs() (Amount, error) {
	if d == Delta(math.MinInt64) {
		return 0, fmt.Errorf("accounting: %w: %d has no representable magnitude", ErrAmountRange, int64(d))
	}
	if d < 0 {
		return Amount(-d), nil
	}
	return Amount(d), nil
}

// Balance is a signed number of minor units — what a bucket holds in one of
// its balance columns. Every persisted balance is non-negative by the
// algebra and the schema (the projection equality plus available ≥ 0 keeps
// settled ≥ 0 too, and no credit semantics are built: a correction that
// would drive a balance negative is refused, not recorded), but the type is
// signed where Amount is not because it carries derived comparisons — a
// settled-minus-held computed on the way to a verdict — that must never wrap
// to reach a guard.
type Balance int64

// BalanceOf returns b as a Balance — the balance a zero-history bucket holds.
func BalanceOf(b Amount) Balance { return Balance(b) }

// Move returns the balance after the delta lands, refusing to wrap past the
// int64 range balances are defined on.
func (b Balance) Move(d Delta) (Balance, error) {
	if d > 0 && b > Balance(math.MaxInt64)-Balance(d) {
		return 0, fmt.Errorf("accounting: %w: %d + %d overflows int64", ErrAmountRange, b, d)
	}
	if d < 0 && b < Balance(math.MinInt64)-Balance(d) {
		return 0, fmt.Errorf("accounting: %w: %d - %d overflows int64", ErrAmountRange, b, -d)
	}
	return b + Balance(d), nil
}

// Sub returns the balance minus another balance, refusing to wrap.
func (b Balance) Sub(other Balance) (Balance, error) {
	if other > 0 && b < Balance(math.MinInt64)+other {
		return 0, fmt.Errorf("accounting: %w: %d - %d overflows int64", ErrAmountRange, b, other)
	}
	if other < 0 && b > Balance(math.MaxInt64)+other {
		return 0, fmt.Errorf("accounting: %w: %d - %d overflows int64", ErrAmountRange, b, other)
	}
	return b - other, nil
}

// IsNegative reports whether the balance is below zero. No legal bucket
// state reaches one — the guard reads it on the way to a verdict, on values
// a refused move would have produced.
func (b Balance) IsNegative() bool { return b < 0 }

// Int64 returns the balance as the plain signed integer the database stores.
func (b Balance) Int64() int64 { return int64(b) }
