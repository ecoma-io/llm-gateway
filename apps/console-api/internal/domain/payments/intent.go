// Package payments is the Control Plane's payment aggregate: what a customer
// asked to pay, what the provider said happened, and the rules by which one
// becomes the other.
//
// The package holds a vocabulary and a state machine and nothing else. It does
// not know HTTP, does not know SQL, and — the rule that shapes every decision
// in it — does not know how money is MOVED. Crediting a funding bucket is
// Accounting's business, through TopUp, and this package's contribution to that
// is one string: the command key that says which leg this payment is. This
// package never mints a balance, never holds a balance, and never claims a
// payment is worth anything to a bucket; the aggregate that can is in
// internal/domain/accounting, and an architecture that let this one answer that
// question would have two authorities for one figure.
//
// Three things are fixed here rather than in the application, because each of
// them is a rule about what a payment IS and an implementation that could differ
// between two callers would be two different products:
//
//   - the statuses a payment can hold, and which move between them are legal;
//   - the command key a funding leg is keyed on, and why it is derived from the
//     provider's PAYMENT reference rather than from the event that reported it;
//   - the ceiling on what a payment may have been refunded, and the fact that
//     reaching it stops the refunds rather than reversing the payment.
//
// The currency question is deliberately present and deliberately narrow. B6 has
// no per-row currency because ADR 0004 recorded that as a consequence, and
// issue #63 is where the platform-wide answer lives. A payment cannot wait for
// that answer to be un-ambiguous, so this package carries the ONE currency a
// payment was denominated in, on the intent, and the application refuses any
// event that disagrees with it. There is no conversion here and no per-leg
// currency: a payment is denominated once, or it is refused.
package payments

import (
	"fmt"
)

// Status is where a payment stands, as this Control Plane understands it.
//
// The names are the contract's — api/openapi/console.yaml's PaymentIntentState
// — and they are deliberately a plain string set rather than a closed Go enum
// mirrored from a document this package does not own. The same reasoning
// ports/outbound/dataplane gives for the fact feed's Kind applies here: a Go
// copy of a list the contract owns is a copy that goes stale, and a build that
// cannot name a status the contract has since added should say so rather than
// render it as one it knows.
type Status string

const (
	// StatusCreated is a payment this platform opened and for which no
	// destination has been recorded. It exists so the intent is durable BEFORE the
	// provider is called: the provider's idempotency key is derived from the
	// intent's identity, and a key derived from something that does not exist
	// until after the call is a key that cannot make a retry the same request.
	StatusCreated Status = "created"

	// StatusAwaitingTransfer is a payment whose customer has been given the
	// instructions that will settle it and has not yet sent the money. It is
	// the state a top-up spends most of its life in, and the name says what is
	// actually true: this platform is waiting on a bank transfer to arrive at
	// a destination it asked the provider for, and nothing the customer does in
	// a browser changes that.
	StatusAwaitingTransfer Status = "awaiting_transfer"

	// StatusRequiresAction is a payment the provider says needs something from
	// the customer before it can settle — a challenge, an authentication step.
	// It is a separate status rather than a flag on awaiting_transfer because the
	// customer's next action is different and a console that rendered the two
	// identically would be telling a customer to do something that is not what
	// the provider asked for.
	StatusRequiresAction Status = "requires_action"

	// StatusSucceeded is a payment the provider says was captured and this
	// platform has credited. It is the ONLY status from which a funding leg is
	// written, and reaching it is the single irreversible move in this state
	// machine.
	StatusSucceeded Status = "succeeded"

	// StatusFailed is a payment the provider will not capture. No money moved,
	// so there is nothing to reverse.
	StatusFailed Status = "failed"

	// StatusCancelled is a payment the customer abandoned. Same money sentence
	// as failed, and a separate status only so a console can say which it was
	// — "you cancelled this" and "this could not be paid" are different things
	// to read, and the difference is the customer's own doing.
	StatusCancelled Status = "cancelled"

	// StatusExpired is a payment whose customer did not transfer in time.
	// It is NOT terminal in the way failed is, and the difference is the whole
	// reason this status exists: a provider-authoritative event may arrive
	// after this platform has given up waiting, and a customer who paid must
	// be funded. See terminalFor for how that is reconciled.
	StatusExpired Status = "expired"

	// StatusPartiallyRefunded is a succeeded payment some of whose money the
	// provider has returned. The funded amount does not change, and the
	// outstanding claim is carried on the intent.
	StatusPartiallyRefunded Status = "partially_refunded"

	// StatusRefunded is a succeeded payment all of whose money the provider has
	// returned.
	StatusRefunded Status = "refunded"

	// StatusQuarantined is not a payment status at all. It is the disposition
	// of a DELIVERY: an event that authenticated and could not be interpreted
	// against any payment, or against this one, is recorded against the
	// payment when there is one and is the operator's work item until somebody
	// decides what it meant. It is spelled as a status so the console renders
	// one state vocabulary rather than two, and it is never a state an intent
	// TRANSITIONS INTO — a payment that was succeeded and then met an
	// unreadable event has not become quarantined, and saying so would lose the
	// fact that the money is in the bucket.
	StatusQuarantined Status = "quarantined"
)

// ErrInvalidReference is the sentinel for a reference this domain cannot carry:
// a blank provider reference, a status it does not know, a refund above what
// the payment can carry. It is the domain's own error and never crosses the
// wire; the application translates it into whatever its surface says.
var ErrInvalidReference = fmt.Errorf("payments: invalid payment reference")

// ErrInvalidTransition is the sentinel for a move the state machine does not
// have. A payment that succeeded does not become cancelled, and a payment that
// expired does not become failed; both of those are the provider telling this
// platform about a payment it has already had the final word on.
//
// The out-of-order case is NOT this error, and keeping the two apart is the
// point. A succeeded event arriving for a payment this platform had given up on
// is a legitimate late delivery, and the provider is right and this platform's
// impatience was the wrong. That is a LEGAL move — expired or cancelled to
// succeeded — and it is the one move into StatusSucceeded that is not a
// compare-and-swap from a single expected predecessor. A move that names no
// legal edge at all is this error, and the caller quarantines rather than
// guessing.
var ErrInvalidTransition = fmt.Errorf("payments: that payment cannot move to that status")

// ErrDuplicateEvent is the sentinel for a delivery this platform has already
// recorded. It is not a failure: the event's effect, if it had one, is already
// durable, and the caller answers it as a success. It exists as a named
// outcome because "already applied" and "applied" are different facts and a
// caller that cannot tell them apart will either retry forever or apply twice.
var ErrDuplicateEvent = fmt.Errorf("payments: that provider event is already recorded")

// ErrUnknownPayment is the sentinel for a delivery naming a payment this
// platform never opened, or one that is not the payment it was matched to.
//
// It is the answer a provider must be able to get for a real payment: a
// webhook can arrive for a payment whose intent write this platform has not
// seen — a delivery racing the response that recorded its destination, a replay
// of a payment from a previous deployment, a provider that emits an event for a
// destination this account was issued outside this surface. Refusing is correct in every one of
// those cases, because accepting an event for an unknown payment is exactly the
// shape of "credit whatever the payload says", and a signature proves who sent
// a message, not what the message is allowed to do.
var ErrUnknownPayment = fmt.Errorf("payments: that delivery names no payment this platform opened")

// ErrDuplicatePayment is the sentinel for a second payment for one
// (account, idempotency key) pair.
//
// It is NOT an error to answer as one. The key exists so that a client which
// retried the call that opens a top-up — after a dropped response, a refreshed
// tab, an impatient double click — gets the payment it already has instead of a
// second one, and the caller's correct response is to converge: re-read the
// payment the key names and return it. A caller that surfaced this as a
// failure would make the retry that the key was invented to make safe into an
// error, which is the opposite of the key's purpose.
var ErrDuplicatePayment = fmt.Errorf("payments: that account already opened a payment under that idempotency key")
