package accounting

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// ErrDerivationOverflow reports a settled amount whose true value cannot fit
// the widths the derivation passes through. It is the Control-side twin of
// the Data Plane's hold-overflow refusal, and it names the same situation
// from the other shore: a figure this large was never reserved by any
// reservation_cap, so a fact carrying it is not a charge this ledger can
// book but a defect to refuse.
var ErrDerivationOverflow = errors.New("accounting: the derived settled amount exceeds every representable bound")

// ErrDerivationInputs marks negative token counts or negative unit prices
// handed to the derivation. The fact contract forbids both shapes — the
// engine's own columns refuse a negative price, and a negative count is a
// claim nothing can settle — so a derivation that receives one is looking at
// a fact that was never legal, which the caller records rather than books.
var ErrDerivationInputs = errors.New("accounting: derivation inputs must not be negative")

// priceScale is the per-million denominator the price grammar is written
// against: unit prices are integer minor units per 1M tokens.
const priceScale = 1_000_000

// SettledAmount re-derives the only settled amount a fact's own figures can
// support: one ceiling over the summed raw product of each priced arm's
// tokens and unit price, in integer minor units. It is the Control-side
// twin of the Data Plane's hold formula — the same arithmetic, checked the
// same way — and it is what the fact consumer runs over a settled fact's
// counts and prices before any of it reaches the ledger. The runtime binds
// the fact's settled_amount to this formula at write time; the consumer
// re-derives it at read time; the two can disagree only if one side's
// arithmetic is wrong, which is a defect one test pins on each shore.
//
// The formula is the contract's, stated normatively in
// api/openapi/shared/usage-facts.yaml: one ceiling over the SUM, never a
// ceiling per arm — a per-arm ceiling rounds twice, and the half-minor-unit
// difference would be a charge the reservation never held. Null counts
// price at zero (the caller resolves nil before calling), delivery tokens
// never price at all (they are the transport's share, not the model's), and
// a zero unit price is a real price — a free arm contributes nothing and is
// not an absence.
//
// Every path is checked arithmetic in 128 bits (math/bits.Mul64 /
// bits.Add64 / bits.Div64): a wrapped product would silently understate a
// charge, and an understated charge is the one way a consumer of this
// formula loses an operator money.
func SettledAmount(inputTokens, outputTokens int64, inputUnitPrice, outputUnitPrice int64) (Amount, error) {
	if inputTokens < 0 || outputTokens < 0 || inputUnitPrice < 0 || outputUnitPrice < 0 {
		return 0, ErrDerivationInputs
	}
	// Each arm's raw cost can exceed 64 bits, so it is formed in 128 bits
	// and the arms are summed there. The inputs are non-negative, so the
	// unsigned words are the values.
	hiIn, loIn := bits.Mul64(uint64(inputTokens), uint64(inputUnitPrice))
	hiOut, loOut := bits.Mul64(uint64(outputTokens), uint64(outputUnitPrice))
	lo, carry := bits.Add64(loIn, loOut, 0)
	hi, overflow := bits.Add64(hiIn, hiOut, carry)
	if overflow != 0 {
		return 0, fmt.Errorf("%w: the summed raw cost passes 128 bits", ErrDerivationOverflow)
	}
	// One ceiling over the whole sum. bits.Div64 panics when the high word
	// is not below the divisor — the quotient would not fit 64 bits — and
	// that condition here is exactly the overflow verdict, so it is refused
	// by hand before the division rather than recovered after.
	if hi >= priceScale {
		return 0, fmt.Errorf("%w: the quotient passes 64 bits", ErrDerivationOverflow)
	}
	quotient, remainder := bits.Div64(hi, lo, priceScale)
	if remainder != 0 {
		if quotient == math.MaxUint64 {
			return 0, fmt.Errorf("%w: the ceiling passes 64 bits", ErrDerivationOverflow)
		}
		quotient++
	}
	// An amount of 2^63 or more minor units is not representable — and is
	// nothing any bucket could have held.
	if quotient > math.MaxInt64 {
		return 0, fmt.Errorf("%w: the ceiling passes the signed width", ErrDerivationOverflow)
	}
	return Amount(quotient), nil
}
