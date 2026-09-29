package payments

import (
	"fmt"
	"strings"
	"time"
)

// The legal edges of the state machine, stated once and read by every move.
//
// It is a table rather than a switch inside a method for the reason ADR 0004
// states its own kind table: a rule this important belongs somewhere a reader
// can see whole, in one place, rather than spread across the branches of the
// code that happens to consult it. It is also what makes an out-of-order rule
// VISIBLE, and there are TWO of those rather than one: a move BACKWARD, from
// `requires_action` to `awaiting_transfer`, and a move forward into a state this
// platform had already stopped waiting for, from `expired` to `succeeded`.
// Both are legible here, each on a line of its own, rather than buried in a
// conditional nobody would find.
//
// The edges below are the table's, all NINETEEN of them, grouped the way the
// table groups them — by the state the payment is IN — and the completeness is
// the point rather than a courtesy. A diagram that showed fifteen and read as
// the whole machine would teach a reader that the four it dropped cannot
// happen, which is the opposite of what a diagram is for; and the four a
// shortened version loses are exactly the ones nobody can guess, because they
// are the two surprising edges above plus the pair that leaves `created`
// without a destination ever being recorded.
//
//	created ──────────────▶ awaiting_transfer
//	created ──────────────▶ failed              a provider answered in the crash
//	created ──────────────▶ cancelled           window — see the table below
//	awaiting_transfer ────▶ requires_action
//	awaiting_transfer ────▶ succeeded           the credit
//	awaiting_transfer ────▶ failed
//	awaiting_transfer ────▶ cancelled
//	awaiting_transfer ────▶ expired
//	requires_action ──────▶ awaiting_transfer   BACKWARD — the customer did it
//	requires_action ──────▶ succeeded           the credit
//	requires_action ──────▶ failed
//	requires_action ──────▶ cancelled
//	expired ──────────────▶ succeeded           late but paid — legal, see below
//	cancelled ────────────▶ succeeded           ditto
//	succeeded ────────────▶ partially_refunded
//	succeeded ────────────▶ refunded
//	partially_refunded ───▶ refunded
//	partially_refunded ───▶ partially_refunded
//	refunded ─────────────▶ refunded            more of the same refund arriving
//
// FOUR of these need defending, and each is one a reader should question if
// they ever move.
//
// `requires_action → awaiting_transfer` is the BACKWARD edge, and it is there
// because `requires_action` is not a state a payment leaves the customer
// behind in. It is where the provider has asked them for something — a
// challenge, an authentication step — and the customer answering it puts the
// payment back in their hands: waiting on their transfer again, with the
// challenge satisfied. A table without this edge would refuse the provider's
// own description of its own transaction, the payment would sit in
// `requires_action` for the rest of its life, and the console would go on
// telling a customer to complete a step they had already completed.
//
// `created → failed` and `created → cancelled` are the crash-window edges, and
// the table below carries the argument in full: an intent is durable before the
// provider is called, so a provider that answered before this platform recorded
// its answer has a payment for the event to land on. Refusing it would lose the
// record of a payment the customer reached.
//
// `expired → succeeded` and `cancelled → succeeded` are the late-payment edges.
// Expiry and cancellation are LOCAL decisions: this platform stopped waiting,
// or the customer walked away. Neither is a statement about the money, and the
// provider is the only party that gets to make that statement. A customer who
// sent the transfer thirty seconds after this platform's patience ran out
// has PAID, and refusing to fund them because a local timer fired first is the
// failure mode this edge exists to prevent. The price is that expiry is
// therefore not final, and any code that treats it as final — a sweep that
// forgets un-settled payments, a report that counts them as lost revenue — is
// describing a set of payments that may still arrive. That is stated here
// because the alternative is a rule nobody knows to distrust.
//
// `refunded → refunded` is there because partial refunds arrive out of order,
// and a full refund arriving after a partial one is not a second full refund: it
// is the same one, complete. The state is idempotent on itself for exactly
// that reason. Note what is NOT there is `refunded → succeeded` and
// `succeeded → succeeded`: a payment that has been fully refunded and then
// reports captured again is not a payment being confirmed twice, it is a
// provider contradicting itself, and the caller quarantines it.
var legalEdges = map[Status][]Status{
	StatusCreated: {
		StatusAwaitingTransfer,
		// A payment that is never given a destination can still be reported as
		// failed or cancelled by a provider that was called before this
		// platform's response was written — the crash window between the
		// provider's call and the recording of its answer. The payment exists,
		// so the event has somewhere to land; refusing it would lose the record
		// of a payment the customer reached.
		StatusFailed,
		StatusCancelled,
	},
	StatusAwaitingTransfer: {
		StatusRequiresAction,
		StatusSucceeded,
		StatusFailed,
		StatusCancelled,
		StatusExpired,
	},
	StatusRequiresAction: {
		StatusAwaitingTransfer,
		StatusSucceeded,
		StatusFailed,
		StatusCancelled,
	},
	// The late-payment edges. See above.
	StatusExpired:   {StatusSucceeded},
	StatusCancelled: {StatusSucceeded},
	StatusSucceeded: {StatusPartiallyRefunded, StatusRefunded},
	StatusPartiallyRefunded: {
		StatusRefunded,
		StatusPartiallyRefunded,
	},
	StatusRefunded: {StatusRefunded},
}

// canMove reports whether a payment may move from one status to another.
//
// It is the whole of the state machine, and the application layer calls it
// through Move rather than consulting the table itself — the table is unexported
// precisely so there is one reader, because a second reader is a second
// interpretation and this table is the difference between funding a customer
// twice and funding them once.
func canMove(from, to Status) bool {
	for _, candidate := range legalEdges[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

// knownStatus reports whether a status is one this build has a word for.
//
// It is what an event carrying an unfamiliar status is checked against, and the
// answer "no" is a refusal rather than a pass: a provider that adds a status is
// describing an outcome this build has no rule for, and the alternative —
// storing it and rendering it as something it is not — would put a status the
// state machine has never considered into the same vocabulary as one it has.
func knownStatus(s Status) bool {
	switch s {
	case StatusCreated, StatusAwaitingTransfer, StatusRequiresAction, StatusSucceeded,
		StatusFailed, StatusCancelled, StatusExpired,
		StatusPartiallyRefunded, StatusRefunded, StatusQuarantined:
		return true
	}
	return false
}

// currencyCodeUpper normalizes a currency to the uppercase ISO 4217 form the
// schema pins and the comparisons use.
//
// Case is folded rather than refused because a provider that sends "usd" is a
// provider whose message this platform can still understand unambiguously —
// ISO 4217 is defined in uppercase and there is exactly one reading of "usd".
// That is a different case from an AMOUNT, where "100" and "100.00" are the
// same figure and a provider sending a decimal string is sending something this
// port will refuse; the tolerance is for spelling, not for precision.
func currencyCodeUpper(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// TopUpCommandKey returns the ledger command key a funding leg for this payment
// carries.
//
// It is the single most consequential string in this package, and every part of
// its construction answers a specific failure someone would otherwise ship.
//
// The namespace comes first. B6's uniqueness on (funding_bucket_id,
// command_key) is ONE SCOPE for every source of topups — an operator's manual
// funding, a future automatic one, and this — and a raw provider reference
// landing in that scope unnamespaced is a coin flip against a key somebody else
// minted. docs/architecture/commerce.md says each source namespaces its own
// keys into that one scope; this is that namespace, made mechanical rather than
// remembered. The segment is fixed-length and pinned, so a provider that used
// our prefix as its own prefix values could not collide with us.
//
// Then the PAYMENT, not the event. This is the finding a double credit hides
// in, and it is worth being exact about: a provider's event id identifies a
// DELIVERY, and a payment has several. A provider may name one capture under
// more than one event type, a re-notification arrives under a fresh delivery id,
// and a partial refund and a charge update are separate deliveries of one
// economic fact. Keying the credit on the delivery would
// write one leg per delivery, and each of those legs is individually legitimate
// — right bucket, right amount, right key scope — so nothing downstream would
// object. The only thing standing between that and a customer funded four times
// is this function. The payment reference is per-payment, so every delivery of
// the same capture derives the SAME key, and B6's own convergence collapses
// them: the second call finds the original leg with the same payload and
// returns the bucket as it stands rather than moving it again.
//
// The reference itself is bounded before it goes in, and bounded in a way that
// cannot collide: a digest, not a truncation. B6 accepts a command key of 1 to
// 256 characters, and a provider reference is unbounded, so SOMETHING has to
// give. Truncating would be the obvious choice and it is the wrong one — two
// references sharing a prefix would produce one key and one credit where two
// were owed, and the failure would be silent and permanent. A digest is fixed
// length, and the full reference is stored on the payment row beside it, so
// reconciliation can still say which payment a leg belongs to; the key is
// derived, never the record.
func TopUpCommandKey(provider, providerPaymentRef string) (string, error) {
	if provider == "" {
		return "", fmt.Errorf("%w: a command key names the provider that issued the payment", ErrInvalidReference)
	}
	if providerPaymentRef == "" {
		return "", fmt.Errorf("%w: a command key names the payment the provider captured", ErrInvalidReference)
	}
	// provider is folded and the payment ref is not, and the asymmetry is
	// deliberate. The provider is a configuration value this deployment
	// chose, so there is exactly one spelling of it and folding only stops a
	// name that differs from the declared one by case or spacing from being
	// two scopes. The payment ref is the provider's own value and is carried
	// byte for byte: two references that differ in case are two references to
	// the provider, and this platform is not entitled to decide they are the
	// same one.
	return fmt.Sprintf("pay:%s:topup:%s", foldProvider(provider), digestReference(providerPaymentRef)), nil
}

// RefundCommandKey returns the ledger command key a refund correction leg would
// carry, namespaced separately from the topup's.
//
// The purpose segment is load-bearing. A refund adjustment and the topup it
// reverses would otherwise share one key if they were derived from the same
// value, and the first adjustment would collide with the topup on
// ledger_entries_bucket_command_key and be reported as a duplicate command — an
// error that reads like a redelivery and is not one.
//
// WHAT THE KEY NAMES is the payment's refunded STATE, not one refund, and that
// is a statement about the deliveries this platform receives rather than a
// preference. The refund delivery this build consumes is a charge-level one: its
// object is a charge, it reports the charge's cumulative refunded TOTAL, and it
// names NO individual refund — there is no refund identifier in it to key on.
// So the identity a refund leg can be derived from is the payment TOGETHER WITH
// the total that has gone back, which is exactly the pair this function takes.
// A payment refunded 400 and then 1000 produces two different keys; a redelivery
// of either produces the key it already had; and a key derived from the payment
// alone would collide across the two partial refunds, making the second leg read
// as a duplicate of the first — the failure this function exists to prevent,
// reintroduced by the shape of its input.
//
// The consequence is recorded rather than hidden: a platform that needed one
// ledger leg per REFUND, rather than per refunded state, would have to consume a
// delivery that carries the provider's own refund object with its own id. That
// is a change to the port — one more reference on the event — and not a change
// to this function, which can only key on what a delivery actually names.
func RefundCommandKey(provider, providerPaymentRef string, refundedTotal int64) (string, error) {
	if provider == "" {
		return "", fmt.Errorf("%w: a refund key names the provider that issued the refund", ErrInvalidReference)
	}
	if providerPaymentRef == "" {
		return "", fmt.Errorf("%w: a refund key names the payment the provider refunded", ErrInvalidReference)
	}
	if refundedTotal <= 0 {
		return "", fmt.Errorf("%w: a refund key names a total that has gone back, and %d has not gone anywhere", ErrInvalidReference, refundedTotal)
	}
	// The reference is digested and the total is appended in the clear, rather
	// than the two being digested as one concatenation. A digest over a
	// concatenation would make the boundary between its parts a matter of
	// spelling: the reference "a@1" with a total of 0 and the reference "a" with
	// a total of 10 are the same eleven characters, so two different refunded
	// states would derive one key and the second leg would be swallowed as a
	// duplicate. digestReference returns a fixed-length hex digest, so the digit
	// after the separator cannot be mistaken for part of it.
	return fmt.Sprintf("pay:%s:refund:%s.%d", foldProvider(provider), digestReference(providerPaymentRef), refundedTotal), nil
}

// foldProvider normalizes a provider name into the namespace segment of a
// command key.
//
// Only unreserved URL characters survive, and the set is a denylist rather than
// an allowlist on purpose: an allowlist would need a character to be ADDED each
// time a provider is onboarded, and the failure mode of forgetting is a command
// key containing the separator and a second key forged from the same string. A
// denylist fails safe — an unexpected character becomes an underscore, and the
// worst outcome is a provider whose namespace is uglier than it needed to be,
// which is a cosmetic defect rather than a financial one.
func foldProvider(provider string) string {
	folded := strings.ToLower(strings.TrimSpace(provider))
	var b strings.Builder
	b.Grow(len(folded))
	for _, r := range folded {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		// A provider whose name is entirely unreserved-able would produce an
		// EMPTY namespace segment, and "pay::topup:x" is a key that another
		// provider with the same problem would also produce. This is
		// unreachable for any real provider name and cheap to make impossible.
		return "unnamed"
	}
	return b.String()
}

// RefundCeiling is the total a payment may have been refunded, in minor units.
//
// It is a function of the CAPTURED amount and is deliberately the only way this
// package will answer a refund question, because the alternative — a mutable
// running total on the intent, checked by the caller — is a check that two
// concurrent refunds can both pass. Both read the same running total, both see
// room, both write. Here the running total lives in the ledger, which has a
// unique index behind it, and the intent's own refunded figure is a projection
// of what the legs say rather than a second place the answer is written.
//
// The ceiling is the captured amount and not the funded amount, and those are
// the same number here by construction: this platform credits the intent's
// canonical amount and refuses any event that disagrees, so a payment's funded
// amount IS what the provider says it captured. A deployment that ever credits
// a different figure would have to revisit this, and the comment is where that
// decision would be made.
func RefundCeiling(captured int64) (int64, error) {
	if captured <= 0 {
		return 0, fmt.Errorf("%w: a refund ceiling is the captured amount, which is %d", ErrInvalidReference, captured)
	}
	return captured, nil
}

// CanRefund reports whether adding `requested` more to `already` would stay
// under the captured amount.
//
// Its two returns answer two DIFFERENT questions, and the difference is worth
// stating because a caller that read the second one as "how much room is left"
// would be reading a refusal's figure off a success. On the success path the
// second return is `requested` — the amount that was accepted, which is what a
// caller computing a new total needs. On the refusal path it is `remaining`, the
// room that was there, because that is the number that turns "refused" into a
// question an operator can answer. A caller that wants the room WITHOUT asking
// for more of it wants RefundableRemaining, which answers it directly and does
// not have to refuse to do so.
//
// The subtraction is checked rather than computed-then-compared. A refund
// larger than the capture is refused, not clamped: a clamp would book a
// correction for a figure the provider did not report, and the ledger's job is
// to record what happened rather than to make it arithmetically pleasant.
func CanRefund(already, requested, captured int64) (int64, error) {
	ceiling, err := RefundCeiling(captured)
	if err != nil {
		return 0, err
	}
	if requested <= 0 {
		return 0, fmt.Errorf("%w: a refund of %d minor units is not a refund", ErrInvalidReference, requested)
	}
	if already < 0 || already > ceiling {
		// An already-refunded figure outside [0, ceiling] is a state this
		// platform cannot have reached through this package's own rules, so
		// it is a data defect rather than a business refusal, and it is
		// refused loudly instead of being used as the base for a decision.
		return 0, fmt.Errorf("%w: a payment refunded %d of %d is not a state this build reaches", ErrInvalidReference, already, ceiling)
	}
	remaining := ceiling - already
	if requested > remaining {
		// A distinct sentinel rather than a bare ErrInvalidReference, and the
		// reason is that a caller MUST be able to tell the two apart. Both are
		// refusals, so the umbrella sentinel answers "is this a refusal"; what
		// the caller above this layer has to report is which figure disagreed
		// and why, and a ceiling and a reference are told apart by nothing but
		// the message text if they share one sentinel. A caller that folded
		// them would file a delivery whose amount exceeded the capture as
		// "named something this payment is not", which sends an operator to
		// look at a provider's reference naming when the money is the fault.
		return remaining, fmt.Errorf("%w: a refund of %d exceeds the %d still refundable of a capture of %d",
			ErrRefundCeiling, requested, remaining, ceiling)
	}
	return requested, nil
}

// ErrRefundCeiling reports a refund that, taken together with what the payment
// has already been refunded, exceeds what the payment captured.
//
// It is a distinct sentinel rather than an ErrInvalidReference because the two
// want different words in an operator's queue. An invalid reference is a
// delivery that named something this payment is not, and the fault is in the
// naming. A ceiling is arithmetic: the provider refunded more than it took, and
// reconciling the two figures is the work. The house rule for the sentinels in
// this package is that each names a situation the operator can only resolve one
// way, and folding these two together produces a report whose remedy is
// ambiguous.
var ErrRefundCeiling = fmt.Errorf("payments: a refund exceeds what the payment captured")

// RefundableRemaining reports how much of a capture may still be refunded after
// `refunded` minor units of it have gone back.
//
// It exists because the question "how much room is left" has no answer through
// CanRefund at the moment the answer matters most, and the reason is the probe.
// CanRefund can only report the room by being asked for something that does not
// fit, so the one case where the room is ZERO — a payment refunded exactly to
// its capture — is refused by the very request that asks for it. A caller
// reading that refusal as a ceiling breach reports the full refund as an
// over-refund, which is the opposite of what happened, and the refunded status
// that only a full refund reaches is then unreachable through the method that
// writes it.
//
// So the room is computed here instead of probed: zero at the ceiling, the
// difference below it, and a loud refusal for a total outside [0, captured],
// which is a state this package's own rules cannot have produced.
func RefundableRemaining(refunded, captured int64) (int64, error) {
	ceiling, err := RefundCeiling(captured)
	if err != nil {
		return 0, err
	}
	if refunded < 0 || refunded > ceiling {
		return 0, fmt.Errorf("%w: a payment refunded %d of %d is not a state this build reaches", ErrInvalidReference, refunded, ceiling)
	}
	return ceiling - refunded, nil
}

// maxProviderReferenceLength bounds a provider reference this domain will
// carry.
//
// The bound is generous — no provider's identifiers approach it — and its
// purpose is to keep an unbounded third-party string out of a database column
// and out of a command key, not to police the provider. A reference longer than
// this is refused rather than truncated, because truncating an IDENTIFIER
// produces a different identifier that looks like the right one, and two
// references differing past the cut would become one payment.
const maxProviderReferenceLength = 255

// validProviderReference reports whether a provider reference is one this
// domain will carry.
func validProviderReference(ref string) bool {
	return ref != "" && len(ref) <= maxProviderReferenceLength
}

// ValidProviderReference is validProviderReference for callers outside this
// package — the application, which has to answer "can this delivery even name
// a payment" before it asks a repository a question that would otherwise be
// answered by a miss indistinguishable from a real one.
//
// It is exported rather than the rule being restated, because a second copy of
// a bound is a second bound: the day this one moves, the copy in the caller
// does not, and the two disagree about which references exist.
func ValidProviderReference(ref string) bool { return validProviderReference(ref) }

// CurrencyCode is currencyCodeUpper for callers outside this package.
//
// Exporting the NORMALISATION rather than leaving each caller to fold case is
// what keeps a comparison honest. The application has to decide whether a
// provider's reported currency disagrees with the payment's, and a caller that
// compared "usd" against "USD" with `==` would report a disagreement that is
// not one — quarantining a real customer's real payment over its spelling.
func CurrencyCode(code string) string { return currencyCodeUpper(code) }

// occurredWithinTolerance reports whether a signed event's timestamp is close
// enough to now to be accepted as FRESH.
//
// The sign of the test matters and is easy to get backwards: a provider's clock
// running slightly ahead of this platform's is common and benign, and a
// tolerance that refused it would reject real payments. The bound is therefore
// symmetric, and a caller that wants to distinguish the two must compare
// against now rather than ask this function.
//
// This constrains how long a CAPTURED delivery stays replayable, which is all a
// timestamp can do. It is not the replay defence and cannot be: an attacker who
// captures a delivery can resend it inside any window a timestamp admits, for
// as long as the window is open. The defence is durable — the event is recorded
// with its effect, in one unit of work, and a second delivery of a recorded
// event is a duplicate — and this function narrows the window rather than
// closing it. Stating that is the point: a timestamp is a bound on staleness,
// and a system that treated it as a bound on replay would be relying on a
// secret the attacker does not need to possess, in a scheme the provider
// re-delivers by design.
func occurredWithinTolerance(occurred, now time.Time, tolerance time.Duration) bool {
	if tolerance <= 0 {
		return true
	}
	skew := now.Sub(occurred)
	return skew <= tolerance && skew >= -tolerance
}
