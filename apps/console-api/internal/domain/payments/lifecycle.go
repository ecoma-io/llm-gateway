package payments

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// The lifecycle: how a payment comes into existence, how an open checkout
// records where it sends the customer, how a provider's capture is matched
// against what this platform sent, and how a refund is recognised.
//
// Everything here is a decision made BEFORE a statement runs. That is the
// division of labour the migration mirrors in plpgsql — the domain refuses,
// the CHECK states the value set, the trigger enforces the transition, and the
// WHERE clause arbitrates — and a refusal that arrives as a driver error is a
// refusal some caller will not recognise as one, because `errors.Is` does not
// match a PostgreSQL string. The rules below exist so that the code which
// READS them is the code that writes them.

// NewPayment is everything the Control Plane must decide, on its own, before a
// customer is charged anything.
//
// Note what is absent: no account id from the request body, no amount the
// browser sent, no provider identifier, no event id. Every field is either
// this platform's own choice or a figure it priced. The browser named a top-up
// OFFER, and an offer is a server-owned price for a server-owned amount — a
// client-supplied amount of minor units is not a smaller version of that, it
// is the absence of the policy, and the whole reason a top-up is offered
// rather than accepted.
type NewPayment struct {
	// AccountID and FundingBucketID are the pair this payment funds. They are
	// resolved together and never separately, because a bucket belonging to
	// another account is not a bucket a topup may target — the pairing is
	// enforced again by a trigger (payment_intents_funding_bucket_owner) and
	// again by the schema's own owner XOR on the bucket, and this validation
	// is the first of the three rather than the last.
	AccountID       string
	FundingBucketID string

	// AmountMinorUnits is the canonical figure: what the ledger will be told,
	// in integer minor units, in Currency. Strictly positive; a zero or
	// negative topup is refused here rather than becoming a leg the ledger
	// would have to treat as a withdrawal.
	AmountMinorUnits int64

	// Currency is the ISO 4217 code the amount is denominated in, uppercase.
	Currency string

	// MinorUnitExponent is that currency's number of decimal places.
	MinorUnitExponent int

	// Provider is the provider that will be asked to take the money.
	Provider string

	// IdempotencyKey is the caller's stable key for this logical topup, unique
	// per account. Two clicks of one button are one payment.
	IdempotencyKey string

	// CheckoutTTL is how long this platform waits for the customer before
	// giving up. It is a local deadline and NOT a statement about the money —
	// see the expired → succeeded edge in state.go.
	CheckoutTTL time.Duration

	// Now is the caller's clock reading. The adapter passes the DATABASE's
	// clock, never the process's, and that is a rule rather than a
	// preference: two replicas of this plane minting payments with their own
	// clocks produce two payment timelines that no reconciliation can
	// interleave, and a checkout whose expiry is judged against a clock that
	// runs a minute fast expires a minute early for everyone.
	Now time.Time

	// MintedID and MintedAt are the identity the database assigns. They are
	// inputs rather than outputs because the identifier is generated on the
	// insert — a column the schema declares GENERATED ALWAYS, and a column
	// this package has no right to also invent.
	MintedID IntentID
	MintedAt time.Time
}

// maxCheckoutTTL bounds how long a checkout may be waited on.
//
// It is a product policy expressed as a constant because a payment integration
// has no table to read it from, and a bound nobody wrote down is not a policy.
// Thirty days is far past any provider's own session lifetime and comfortably
// past any customer's patience; a longer one means a checkout this platform has
// abandoned is still in the console's live list.
const maxCheckoutTTL = 30 * 24 * time.Hour

// NewIntent mints a time-ordered payment identifier.
//
// RFC 9562 version 7 for the reason commerce's ids.go gives, which is also the
// reason persistence.md pins v7 for the commerce identifiers: a payment's id
// orders it, and the console's list, the reconciliation's pass and this
// package's own keyset all order by that id. A v4 would order nothing and
// every one of those readers would fall back to a timestamp that two
// concurrent inserts can share.
//
// The minter reads the caller's clock, and the caller is the application layer
// passing the DATABASE's reading — the same rule NewPayment.Now carries. Minting
// here rather than in the application exists because the version and variant
// bits are this aggregate's business: a payment id that is not a v7 is not a
// payment id this plane will order correctly, and a caller that could mint one
// anyway would be able to break the ordering without any signal.
func NewIntent(now time.Time) (IntentID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("payments: read random bytes: %w", err)
	}
	// The Unix epoch in milliseconds fits the forty-eight bits this layout
	// gives it until far past any year a gateway will meet, so the widening
	// conversion loses nothing.
	ms := uint64(now.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	h := make([]byte, 36)
	hex.Encode(h[0:8], b[0:4])
	h[8] = '-'
	hex.Encode(h[9:13], b[4:6])
	h[13] = '-'
	hex.Encode(h[14:18], b[6:8])
	h[18] = '-'
	hex.Encode(h[19:23], b[8:10])
	h[23] = '-'
	hex.Encode(h[24:36], b[10:16])
	return IntentID(h), nil
}

// New builds a payment in its birth state.
//
// The ordering inside this function is the argument, and it is deliberate:
// structural validity first (which payment, whose money, how much, in what
// unit), then the amount, then the identity the platform chose. Nothing here
// is derived from anything a third party said, because nothing a third party
// said exists yet — a payment is created BEFORE the provider is called, and
// that order is not incidental. The provider's idempotency key is derived from
// the payment's own id, so the payment has to be durable before the call;
// a key derived from something that does not exist until the provider answers
// cannot make a retry the same request as the first attempt, and a retry that
// is a different request is a customer charged twice.
func New(in NewPayment) (Intent, error) {
	if in.AccountID == "" {
		return Intent{}, fmt.Errorf("%w: a payment names the account it funds", ErrInvalidReference)
	}
	if in.FundingBucketID == "" {
		return Intent{}, fmt.Errorf("%w: a payment names the funding bucket it credits", ErrInvalidReference)
	}
	if !validProviderReference(in.FundingBucketID) {
		return Intent{}, fmt.Errorf("%w: %q is not a funding bucket reference this build carries", ErrInvalidReference, in.FundingBucketID)
	}
	if in.AmountMinorUnits <= 0 {
		// A topup of nothing, and a topup of a negative figure, are both
		// refused here. A negative topup is not a withdrawal: B6's
		// adjustments are how money leaves, and admitting one through the
		// funding path would give a caller a debit primitive dressed as a
		// deposit, with none of the ceiling a withdrawal has to pass.
		return Intent{}, fmt.Errorf("%w: a top-up of %d minor units is not a top-up", ErrInvalidReference, in.AmountMinorUnits)
	}
	currency := currencyCodeUpper(in.Currency)
	if !validCurrencyCode(currency) {
		// Refused rather than repaired. A currency code is three letters and
		// guessing which three is a guess about how much money a customer
		// is sending; the failure mode of a lenient check is not a rejected
		// request, it is a funded bucket whose unit is a guess.
		return Intent{}, fmt.Errorf("%w: %q is not an ISO 4217 alphabetic code", ErrInvalidReference, in.Currency)
	}
	if !validMinorUnitExponent(in.MinorUnitExponent) {
		// See validMinorUnitExponent: the unit is the difference between a
		// thousand yen and ten dollars, so an exponent this build does not
		// know is an amount whose size is unknown.
		return Intent{}, fmt.Errorf("%w: %d is not a currency exponent this build knows", ErrInvalidReference, in.MinorUnitExponent)
	}
	if in.Provider == "" {
		return Intent{}, fmt.Errorf("%w: a payment names the provider that will take it", ErrInvalidReference)
	}
	// The key is carried VERBATIM — not folded, not trimmed — and the rule lives
	// in CheckIdempotencyKey so the caller and this constructor cannot come to
	// different conclusions about the same bytes. Case is preserved on purpose:
	// the key is the caller's own identifier and the uniqueness that catches a
	// duplicate click is byte-exact (payment_intents_idempotency_key is a UNIQUE
	// over the column as stored), so folding here would merge two keys a caller
	// considers distinct without any of the three places that match on the key
	// agreeing to it.
	if err := CheckIdempotencyKey(in.IdempotencyKey); err != nil {
		return Intent{}, fmt.Errorf("%w: %w", ErrInvalidReference, err)
	}
	if in.CheckoutTTL <= 0 {
		return Intent{}, fmt.Errorf("%w: a checkout must be given a lifetime", ErrInvalidReference)
	}
	if in.CheckoutTTL > maxCheckoutTTL {
		return Intent{}, fmt.Errorf("%w: a checkout lifetime of %s is over the %s ceiling", ErrInvalidReference, in.CheckoutTTL, maxCheckoutTTL)
	}
	if in.MintedID == "" {
		return Intent{}, fmt.Errorf("%w: a payment has an identity", ErrInvalidReference)
	}
	if in.MintedAt.IsZero() {
		return Intent{}, fmt.Errorf("%w: a payment has a creation time", ErrInvalidReference)
	}

	return Intent{
		ID:                 in.MintedID,
		AccountID:          in.AccountID,
		FundingBucketID:    in.FundingBucketID,
		AmountMinorUnits:   in.AmountMinorUnits,
		Currency:           currency,
		MinorUnitExponent:  in.MinorUnitExponent,
		Provider:           in.Provider,
		IdempotencyKey:     in.IdempotencyKey,
		Status:             StatusCreated,
		RefundedMinorUnits: 0,
		StateVersion:       0,
		CreatedAt:          in.MintedAt,
		UpdatedAt:          in.MintedAt,
		ExpiresAt:          in.MintedAt.Add(in.CheckoutTTL),
	}, nil
}

// maxIdempotencyKeyLength bounds the caller's key for a repeat of one topup.
//
// It is generous, and deliberately so: the key is generated by a client, and
// the cost of refusing a long one is a customer's topup button that does not
// work, while the cost of accepting a long one is a column holding what a
// client chose to put in it. The bound is the schema's column width, and it is
// a refusal rather than a truncation for the same reason a provider reference
// is refused rather than truncated — a cut identifier is a different
// identifier that looks like the right one.
const maxIdempotencyKeyLength = 128

// CheckIdempotencyKey reports whether key is one this domain can carry as the
// caller's stable key for a repeat of one top-up, and returns the reason when
// it is not.
//
// It is exported, and it is a function rather than the constant above, because
// the caller's key is the ONE field of NewPayment that carries bytes a client
// chose: every other field is read from this deployment's own configuration, is
// derived from a row this plane already holds, or is minted here. That makes it
// the only input that can make New fail on a real request, and the difference
// between the two layers refusing it is not cosmetic — an application that let
// it through and relied on New would answer a client's own input with a 500,
// telling an operator to page whoever is on call about a top-up button that was
// holding a key one byte too long. Asking this function first lets the
// application answer 400 with this sentence, and there is no second copy of the
// bound to drift from: New calls the same function on the same bytes.
//
// Case and surrounding whitespace are preserved. The key is an identifier
// rather than a phrase, and the three places that match on it — the pre-read by
// (account, key), the UNIQUE index over the stored column, and the re-read on a
// collision — all compare bytes; normalising here would move one of the three
// rather than all of them, and a key a caller treats as distinct would converge
// on someone else's payment.
func CheckIdempotencyKey(key string) error {
	if key == "" {
		return fmt.Errorf("a payment names the key that makes a repeat of it one payment")
	}
	if len(key) > maxIdempotencyKeyLength {
		return fmt.Errorf("an idempotency key of %d characters is over the %d this build carries", len(key), maxIdempotencyKeyLength)
	}
	return nil
}

// validCurrencyCode reports whether code is an ISO 4217 ALPHABETIC code.
//
// The check is the shape and not membership of a list, and the reason is
// enumerated in validMinorUnitExponent: a list of currencies in Go would be a
// copy the world would move past, and a payment in a currency this build has
// never heard of is a payment this plane should be able to CARRY — it funds a
// bucket in the deployment's settlement currency or it does not, and a
// deployment whose settlement currency is the one being offered is the case
// that must work. What must never be accepted is a shape that is not a
// currency, because a three-character string that is not a code would sail
// through every later comparison — the stored value and the provider's value
// would be the same wrong string, and the mismatch check would find them in
// agreement.
func validCurrencyCode(code string) bool {
	if len(code) != 3 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return false
		}
	}
	return true
}

// validMinorUnitExponent reports whether n is a currency exponent this build
// knows how to place a decimal point at.
//
// The bound is 0..4 and it is a bound on PRECISION, not a list of currencies:
// ISO 4217's exponents run from 0 (JPY, KRW — the yen has no minor unit) to 4
// (CLF, UYW — the unidad de Fomento is written to four places), and nothing in
// the standard goes beyond. A build that meets an exponent of 5 is looking at
// something that is not a currency amount, and a build that meets a NEGATIVE
// exponent is looking at a value smaller than the unit it claims to be
// denominated in, which is a quantum and not a payment.
//
// The alternative — deriving the exponent from a table — is what this refuses,
// and issue #63 is where the platform-wide answer belongs. A payment cannot
// wait for it, because refusing every currency the table has not caught up with
// would mean refusing every payment in a new currency forever. So the exponent
// travels with the payment, stated by the deployment that offers it, and the
// consistency this build can check — the amount this platform sent equals the
// amount the provider reports, in the same unit — does not depend on knowing
// which currencies exist.
func validMinorUnitExponent(n int) bool {
	return n >= 0 && n <= 4
}

// OpenedCheckout is what a provider gave this platform when a hosted checkout
// was created: where to send the customer, and what the provider calls it.
//
// The domain carries its own two-field type rather than the outbound port's
// CheckoutSession, and the reason is the import rule this module already
// enforces everywhere else: a domain package that names a port's type is a
// domain that has decided it knows what a provider protocol looks like. The
// dependency runs the other way — the port's type is convertible into this one
// by the adapter that owns both — and the day a second provider appears with a
// richer session shape, the port grows and this file does not.
//
// The URL is carried as a string and is NEVER parsed. It is a promise the
// provider makes and this process keeps on the provider's behalf; a consumer
// that url.Parse'd it to pull out a session id would be interpreting a value
// whose only contract is that a browser may be sent to it, and would break
// silently the first time the provider changed the shape of its own URLs.
type OpenedCheckout struct {
	// URL is the hosted checkout, verbatim.
	URL string
	// ProviderRef is the provider's identifier for the checkout, and the
	// thing a later delivery names. It is stored durably and is the link
	// between a provider's event and a payment this platform created.
	ProviderRef string
}

// OpenCheckout records that a hosted checkout has been opened, and returns the
// payment as it now stands.
//
// The provider's reference and URL are the only two things that change, and
// neither is interpreted: the URL is stored because a customer who comes back
// to a payment needs to be sent to the same checkout, and the reference is
// stored because it is the only thing a later delivery can be MATCHED on. A
// value this function does not have a rule for is refused rather than stored,
// because a blank reference means the payment can never be matched and a blank
// URL means the console will render a button that goes nowhere — both are
// states that look like a working feature.
//
// The move itself is the caller's compare-and-swap, not this function's: the
// returned payment still shows the state this function validated, and the
// caller writes it under that expectation. A caller that skips the swap and
// writes unconditionally would race its own second attempt, and the two
// checkouts that came back would have one winner with no way to tell which.
func (i Intent) OpenCheckout(checkout OpenedCheckout, now time.Time) (Intent, error) {
	if !canMove(i.Status, StatusCheckoutOpen) {
		return Intent{}, fmt.Errorf("%w: a payment at %s cannot open a checkout", ErrInvalidTransition, i.Status)
	}
	if checkout.URL == "" {
		return Intent{}, fmt.Errorf("%w: an opened checkout has a URL to send the customer to", ErrInvalidReference)
	}
	if !validProviderReference(checkout.ProviderRef) {
		return Intent{}, fmt.Errorf("%w: %q is not a checkout reference this build carries", ErrInvalidReference, checkout.ProviderRef)
	}
	next := i
	next.ProviderCheckoutRef = checkout.ProviderRef
	next.CheckoutURL = checkout.URL
	next.Status = StatusCheckoutOpen
	next.UpdatedAt = now
	return next, nil
}

// CaptureMatch is the verdict that a delivery really is the capture of this
// payment, and — when it is — the key the funding leg is keyed on.
//
// It exists as a type rather than as a bare bool and a key because the two
// answers must not be separable. A caller that could ask the first question
// without the second would be a caller that can decide to fund a payment
// without having a key to fund it under, and a key minted at the credit site
// rather than derived from the payment is a key that a later code change can
// derive differently — which is a double credit wearing a refactor's clothes.
// A capture that matches therefore CARRIES its own idempotency key, computed
// once, here, from the payment the event named.
type CaptureMatch struct {
	// Matched is the whole of the verdict. False means this delivery is not
	// the capture of this payment, and the caller records it as a quarantine
	// rather than crediting anything.
	Matched bool
	// CommandKey is the ledger command key for the funding leg, derived from
	// the provider's PAYMENT reference. Meaningful only when Matched; the
	// zero value otherwise, so a caller that ignores Matched and uses the
	// key anyway submits an empty key and B6 refuses it.
	CommandKey string
}

// MatchCapture decides whether a provider's delivery is the capture of this
// payment, and derives the key the funding leg will carry if so.
//
// Four things have to agree, and each of them closes a specific way this
// platform would fund a customer who did not pay, or pay a customer the wrong
// amount:
//
//  1. The reference must be one this platform STORED. An event naming a
//     reference this platform never issued is refused here rather than
//     adopted, and this is the check that makes a signature irrelevant to
//     ownership: the signature answers "our provider sent this", and only a
//     stored reference answers "about a payment of ours".
//
//  2. The payment must be in a state the state machine can move to
//     succeeded. A payment that is already refunded has had its final word
//     and a capture arriving after it is a provider contradicting itself; a
//     payment that is `created` cannot be captured because no checkout was
//     ever sent to anybody. canMove is consulted rather than a switch here
//     so the two copies of the rule cannot drift — the Go one and the
//     plpgsql trigger, which is the one that actually holds when a second
//     writer arrives from a path this build did not write.
//
//  3. The AMOUNT must be present, positive, and equal. Absent is not zero
//     (the port's money fields are pointers for exactly this), and unequal
//     is refused rather than trusted in either direction: a provider that
//     reports less than the platform sent has under-charged a customer who
//     expected to be funded at the price they were shown, and a provider
//     that reports more has charged them more than the platform asked. This
//     plane cannot tell which, so it funds neither and records the claim.
//
//  4. The CURRENCY must match, and it is checked before the amount on
//     purpose. An amount is not a figure until the unit it counts in is
//     known, and 1000 is a thousand yen or ten dollars depending entirely on
//     which of the two was meant. Comparing the two integers first would let
//     a capture in a different currency through on the off chance that the
//     numbers happened to agree, and the ledger would then hold ten dollars
//     under a promise of a thousand yen. There is no conversion in this
//     package and no authority to make one: issue #63 owns that question and
//     it is not answered.
func (i Intent) MatchCapture(providerPaymentRef string, amount *int64, currency string) (CaptureMatch, error) {
	if !validProviderReference(providerPaymentRef) {
		return CaptureMatch{}, fmt.Errorf("%w: %q is not a payment reference this build carries", ErrInvalidReference, providerPaymentRef)
	}
	if amount == nil {
		return CaptureMatch{}, fmt.Errorf("%w: a capture states an amount; this one states none", ErrInvalidReference)
	}
	if *amount <= 0 {
		return CaptureMatch{}, fmt.Errorf("%w: a capture of %d minor units is not a capture", ErrInvalidReference, *amount)
	}
	if i.ProviderPaymentRef != "" && i.ProviderPaymentRef != providerPaymentRef {
		// The payment already has a capture, and this delivery names a
		// DIFFERENT one. A provider that renames a charge between the
		// capture and a later delivery would land here, and the answer is
		// still a refusal: a payment has one capture, and a second
		// reference is either a provider that does not keep its own
		// identifiers stable or a delivery that was not about this
		// payment. Neither is something to fund on.
		return CaptureMatch{}, fmt.Errorf("%w: payment %s is already the capture of %q, not %q",
			ErrInvalidReference, i.ID, i.ProviderPaymentRef, providerPaymentRef)
	}
	if !canMove(i.Status, StatusSucceeded) {
		return CaptureMatch{}, fmt.Errorf("%w: a payment at %s cannot be captured", ErrInvalidTransition, i.Status)
	}
	if currencyCodeUpper(currency) != i.Currency {
		// Compared AFTER the shape is known to be a currency and BEFORE the
		// amount. See above.
		return CaptureMatch{}, fmt.Errorf("%w: a capture in %q is not a payment denominated in %q",
			ErrInvalidReference, currency, i.Currency)
	}
	if *amount != i.AmountMinorUnits {
		return CaptureMatch{}, fmt.Errorf("%w: a capture of %d minor units is not the %d this payment was opened for",
			ErrInvalidReference, *amount, i.AmountMinorUnits)
	}

	key, err := TopUpCommandKey(i.Provider, providerPaymentRef)
	if err != nil {
		return CaptureMatch{}, err
	}
	return CaptureMatch{Matched: true, CommandKey: key}, nil
}

// RecordRefund recognises a refund and reports what became of it.
//
// The word is RECOGNISE, and the distinction it draws is the most consequential
// boundary in this package: a refund is the provider saying money went BACK to
// the customer, and the honest response to that is to record it, refuse to
// book a debit B6 cannot represent, and hand the case to a human. B15 does NOT
// book a correction, and the reason is not caution — it is that the correction
// is not expressible in the ledger this platform has.
//
// A refund is a debit that leaves settled. B6's ApplyTo refuses any adjustment
// driving a bucket below zero, and it refuses it in three places on purpose:
// the code, the persisted echo's WHERE clause, and the schema's own projection
// CHECK. So consider the ordinary case rather than the tidy one. A customer
// tops up 10000, spends 9000, and is then refunded the whole payment. The
// customer is owed 10000 and holds 1000. Both facts cannot live in a balance,
// and the balance has to be the one that wins, because the alternative is an
// account that reads -9000. The withdrawal pattern B6 does model — one
// adjustment moving settled AND held together — is a pattern for SPENDING money
// from a bucket, and here the held side is a reservation against a different
// entitlement's grant; a refund has no business releasing it. And the way
// around it, booking a held debit with an operator id, was the route the
// repository already refused once: minting a machine principal to get a
// correction out of the ledger is how an audit trail learns to lie. So this
// package records the claim, exposes the ceiling, and leaves the accounting
// decision to whoever is entitled to make it.
//
// What the record carries is therefore three figures, not one. The refund
// itself, which the provider reported. The amount the ledger could have taken
// and did not, because the balance did not hold it. And the amount that
// remains refundable under the ceiling, which is what stops a second refund
// from quietly re-deriving the first one's arithmetic off a total that moved.
//
// The ceiling is a REFUSAL, not a clamp. A refund above what the capture took
// is refused whole: clamping would book a correction for a figure the provider
// never reported, and the ledger's job is to record what happened rather than to
// make it arithmetically pleasant. CanRefund returns the remaining room in the
// error so an operator can see by how much it was refused, which is the
// difference between a question an operator can answer and one they have to go
// and compute.
type RefundRecord struct {
	// RefundRef is the provider reference this record is about, and it is the
	// PAYMENT's: the refund delivery this build consumes is a charge-level one
	// that reports the charge's cumulative refunded figure and names no
	// individual refund, so the finest identity available to it is the payment
	// itself. The out-of-order case it has to survive is still the one that
	// matters — a full refund arriving after a partial one is not a second full
	// refund — and what survives it is not this field but CommandKey below.
	RefundRef string

	// AmountMinorUnits is the INCREMENT this record adds to the payment's
	// refunded total: what this delivery reported that the platform had not
	// already recognised. It is deliberately not the provider's own figure,
	// because the provider reports a CUMULATIVE total for the charge — so a
	// caller that passed that total through would count the first refund twice
	// on the second delivery, and the second legitimate partial refund would be
	// refused as a ceiling breach.
	AmountMinorUnits int64

	// Status is where the payment now stands: partially_refunded or
	// refunded, and only those two.
	Status Status

	// TotalRefunded is the running total after this refund, which is what the
	// next refund's ceiling is measured against.
	TotalRefunded int64

	// RemainingRefundable is what may still be refunded of this capture.
	RemainingRefundable int64

	// CommandKey is the key a refund leg WOULD carry, derived and returned
	// even though this package writes no leg. It is computed here so that
	// whoever books the correction derives the same key this package's
	// ceiling arithmetic already assumed — a key invented at the booking
	// site is a key that can disagree with the total beside it.
	//
	// It names the refunded STATE, which is the pair of the payment and
	// TotalRefunded: see RefundCommandKey for why that pair and not one
	// refund's own identifier. Two things follow for a booking site, and both
	// are properties rather than caveats. The key is stable across
	// redeliveries of one refunded state, so a leg booked from a replayed
	// delivery converges instead of doubling. And two distinct partial refunds
	// carry two keys, each naming the total that produced it.
	CommandKey string
}

// RecordRefund checks a refund against the payment's captured amount and
// returns the record a caller would write.
//
// `amount` is the INCREMENT the delivery added to the payment's refunded total,
// not the total the provider reported — see the field's own comment for why the
// difference is the whole of the second-partial-refund case. `refundRef` is the
// provider's reference for the payment this claim is about.
//
// It refuses rather than returning a record for an over-refund, an unknown
// refund reference, a non-positive amount, or a payment that was never
// captured. The state it moves to is decided HERE and not left to the caller,
// because "partially" versus "fully" is a comparison against the ceiling and a
// caller that recomputed it could disagree with the arithmetic the ceiling
// check just performed.
func (i Intent) RecordRefund(refundRef string, amount *int64) (RefundRecord, error) {
	if !validProviderReference(refundRef) {
		return RefundRecord{}, fmt.Errorf("%w: %q is not a refund reference this build carries", ErrInvalidReference, refundRef)
	}
	if amount == nil {
		return RefundRecord{}, fmt.Errorf("%w: a refund states an amount; this one states none", ErrInvalidReference)
	}
	if i.ProviderPaymentRef == "" {
		// The refund arrived before the capture this platform has not
		// recorded. A provider is within its rights to deliver them in
		// either order, and the right answer is to record it and wait rather
		// than to book a correction against a leg that does not exist.
		return RefundRecord{}, fmt.Errorf("%w: a refund for %s arrived before its capture", ErrRefundAheadOfCapture, i.ID)
	}
	if !canMove(i.Status, StatusPartiallyRefunded) {
		return RefundRecord{}, fmt.Errorf("%w: a payment at %s cannot be refunded", ErrInvalidTransition, i.Status)
	}
	requested, err := CanRefund(i.RefundedMinorUnits, *amount, i.AmountMinorUnits)
	if err != nil {
		return RefundRecord{}, err
	}
	total := i.RefundedMinorUnits + requested
	key, err := RefundCommandKey(i.Provider, refundRef, total)
	if err != nil {
		return RefundRecord{}, err
	}
	next := StatusPartiallyRefunded
	if total == i.AmountMinorUnits {
		next = StatusRefunded
	}
	// The room is computed, not probed. Asking whether ONE more unit fits was
	// the shape this used to have, and it refused a refund that reached the
	// capture exactly — the room is zero there, so the probe could not fit and
	// answered with its own refusal, which the caller read as an over-refund.
	// A full refund is the one case where this figure is zero, and it is also
	// the only path to StatusRefunded, so the probe made the refunded state
	// unreachable. See RefundableRemaining.
	remaining, err := RefundableRemaining(total, i.AmountMinorUnits)
	if err != nil {
		return RefundRecord{}, err
	}
	return RefundRecord{
		RefundRef:           refundRef,
		AmountMinorUnits:    requested,
		Status:              next,
		TotalRefunded:       total,
		RemainingRefundable: remaining,
		CommandKey:          key,
	}, nil
}

// ErrRefundAheadOfCapture reports a refund for a payment whose capture this
// platform has not recorded.
//
// It is a distinct sentinel rather than an ErrInvalidTransition because the
// two want opposite handling. A refused transition is a contradiction — the
// provider and this platform disagree about a payment that is finished, and
// that is a quarantine and a human. A refund ahead of its capture is a
// SEQUENCE the provider is entitled to produce, and the right answer is to
// record the delivery and let the capture arrive.
var ErrRefundAheadOfCapture = fmt.Errorf("payments: a refund was reported before the capture it refunds")

// SettleExpired moves a payment to expired once its checkout's deadline has
// passed, and reports whether the payment was in a state that could wait.
//
// The clock is the DATABASE's, and the reason is the one state.go gives for
// the late-payment edge: expiry is a local decision about patience, and a local
// decision made against a local clock is a decision two replicas make
// differently. The customer whose provider session has already expired sees
// `expired` in one replica's console and `checkout_open` in another's.
//
// The states it waits on are the two that are still WAITING — a checkout the
// customer has been sent to but not completed, and one the provider says needs
// something from them — but the ORDER of the two checks below is not the order
// that sentence suggests, and stating it is the point of this paragraph.
//
// The STATUS gate runs FIRST and the deadline SECOND. A payment whose status is
// not one of the two is not this sweep's payment at all: it is answered with no
// move and no error, and the clock is never consulted about it. And a payment
// at `requires_action` is refused AT THE GATE, before its deadline is read,
// because the wait list names it while legalEdges has no
// requires_action → expired edge for the loop to find. The refusal is therefore
// about the STATUS and never about the deadline: a requires_action payment
// gets ErrInvalidTransition an hour before its deadline and a day past it
// alike. That is exactly the shape of answer a caller can misread, so what it
// should conclude is stated here: the sweep is not the thing that settles a
// payment awaiting the customer. A customer mid-challenge is not someone who
// walked away, and refusing is what keeps a local timer from writing a terminal
// status onto a payment the customer can still complete. Settling one is a
// decision some later change owns; this one declines to make it.
//
// The gate and the wait list disagree about `requires_action`, and the loop
// refuses on that disagreement rather than resolving it — the error branch's
// own comment says so. A change that did mean to settle these payments would
// have to add the edge to the table rather than loosen the gate, because the
// table is what the trigger enforces and the two would then agree.
//
// The three states a completed or abandoned checkout leaves behind — succeeded,
// failed, cancelled — are not waiting for anybody and are not swept, and
// neither is any other status outside the pair above: a payment this sweep does
// not wait on gets the same "no move" as one its deadline has not reached, with
// no error, because there is nothing here for a caller to act on.
func (i Intent) SettleExpired(now time.Time) (Intent, bool, error) {
	waited := []Status{StatusCheckoutOpen, StatusRequiresAction}
	for _, from := range waited {
		if i.Status != from {
			continue
		}
		if !canMove(from, StatusExpired) {
			// The state machine and the wait list disagree, which is a defect
			// in this file rather than a condition any caller can cause.
			// Returning it beats sweeping a payment the table forbids.
			return Intent{}, false, fmt.Errorf("%w: %s waits but cannot expire", ErrInvalidTransition, from)
		}
	}
	if i.Status != StatusCheckoutOpen && i.Status != StatusRequiresAction {
		return Intent{}, false, nil
	}
	if now.Before(i.ExpiresAt) {
		return Intent{}, false, nil
	}
	next := i
	next.Status = StatusExpired
	next.UpdatedAt = now
	return next, true, nil
}
