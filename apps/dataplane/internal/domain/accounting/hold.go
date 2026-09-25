package accounting

import (
	"errors"
	"math"
	"math/bits"
)

// The hold arithmetic. A hold is what admission sets aside before a provider
// is called, and ADR 0003 pins both its formula and its ceiling:
//
//	hold = ceil((inputTokens·inputUnitPrice + outputTokens·outputUnitPrice) / 1_000_000)
//
// The prices are integer minor units per 1M tokens, so the raw product of
// each leg is tokens × price-per-million; the ONE ceiling is taken over the
// summed raw product, never per leg. The distinction is worth a sentence: two
// ceilings would round each leg up independently and charge the client up to
// one minor unit more than the request costs, and a hold that routinely
// overstates by construction is a small lie the settlement would have to
// correct every time. One ceiling over the sum rounds once, on the number the
// request actually prices to.
const priceScale = 1_000_000

// ErrHoldOverflow marks a hold whose true value does not fit an int64. The
// check is arithmetic, not a guard against garbage: the inputs are the
// client's own token counts and the catalog's prices, and the product of two
// legal values can simply be too large. Overflow is therefore the over-cap
// verdict at its extreme — a hold of 2^63 minor units or more exceeds any
// reservation_cap the catalog can name — and the caller answers it with
// invalid_request, before any capacity is taken. It is deliberately not a
// 500: a server fault is something the gateway did wrong, while a hold too
// large to reserve is a request the gateway cannot price, refused the same
// way and at the same step as any other over-cap hold.
var ErrHoldOverflow = errors.New("accounting: hold exceeds the largest reservable amount")

// ErrHoldInputs marks negative token counts or negative unit prices. Neither
// is a legal hold input — NewRequest and NewReservation each refuse their own
// negatives, and this check keeps the pure function honest if a caller ever
// reaches it before those — but unlike overflow it names a caller bug, not a
// client's request.
var ErrHoldInputs = errors.New("accounting: hold inputs must not be negative")

// Hold derives the hold a request opens on future spend: one ceiling over the
// summed raw product, in integer minor units. It is the only place the
// formula lives — the waterfall's verdicts and the settlement both re-derive
// through it — so the number admission reserves, the number the schema's
// trigger re-derives on the row, and the number a replayed audit computes can
// only disagree if this function is wrong, which is a defect one test can
// pin.
//
// Every path is checked arithmetic in 128 bits (math/bits.Mul64 /
// bits.Add64 / bits.Div64): a wrapped product would silently understate a
// hold, and an understated hold is the one way a runtime invents money. The
// function refuses nothing about magnitude short of overflow — a huge but
// representable hold is a legitimate over-cap refusal the caller makes
// against the alias's reservation_cap, and this function does not know the
// cap.
func Hold(inputTokens, outputTokens int, inputUnitPrice, outputUnitPrice int64) (int64, error) {
	if inputTokens < 0 || outputTokens < 0 || inputUnitPrice < 0 || outputUnitPrice < 0 {
		return 0, ErrHoldInputs
	}
	// Each leg's raw cost can exceed 64 bits, so it is formed in 128 bits and
	// the legs are summed there. inputs are non-negative, so the unsigned
	// words are the values.
	hiIn, loIn := bits.Mul64(uint64(inputTokens), uint64(inputUnitPrice))
	hiOut, loOut := bits.Mul64(uint64(outputTokens), uint64(outputUnitPrice))
	lo, carry := bits.Add64(loIn, loOut, 0)
	hi, overflow := bits.Add64(hiIn, hiOut, carry)
	if overflow != 0 {
		// The true sum already exceeds 128 bits: the hold is past any cap.
		return 0, ErrHoldOverflow
	}
	// Divide the 128-bit sum by 1M with one ceiling. bits.Div64 panics when
	// the high word is not below the divisor — the quotient would not fit 64
	// bits — and that condition here is exactly the overflow verdict, so it is
	// refused by hand before the division rather than recovered after.
	const divisor = uint64(priceScale)
	if hi >= divisor {
		return 0, ErrHoldOverflow
	}
	quotient, remainder := bits.Div64(hi, lo, divisor)
	if remainder != 0 {
		if quotient == math.MaxUint64 {
			return 0, ErrHoldOverflow
		}
		quotient++
	}
	// A hold of 2^63 or more minor units is not representable — and is
	// nothing any reservation_cap could cover. Same verdict, same refusal.
	if quotient > math.MaxInt64 {
		return 0, ErrHoldOverflow
	}
	return int64(quotient), nil
}

// ReDerivedHold re-derives the hold from the reservation's own stored columns
// — the client-side half of the re-derivation invariant. The requests and
// reservations rows re-derive the hold in a database trigger (the server-side
// half, migration 000003), and the settlement re-derives it through Hold; this
// method is what an application calls before the write to assert the same
// equality where the decision is formed, so a caller that reserved an amount
// these prices and counts do not support fails at admission instead of at the
// row. NewReservation deliberately does not run this check — it validates the
// legs against the amount it is given, not the amount against the formula —
// because the formula's inputs arrive with the reservation and the assertion
// is the caller's sentence to make.
func (r Reservation) ReDerivedHold() (int64, error) {
	return Hold(r.InputTokens, r.MaxOutputTokens, r.InputUnitPrice, r.OutputUnitPrice)
}
