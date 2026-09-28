// Package payments is the Control Plane's outbound port to an external payment
// provider: the seam through which a customer is sent to a hosted checkout and
// through which money is recognised.
//
// It is a port in this module for the reason ports/outbound/dataplane is one and
// for a different reason. There, the port existed because the peer was a
// separate Go module and an import path could not cross. Here the provider is a
// third party's HTTPS API, and the reason is the same in the end: the provider's
// vocabulary — its endpoints, its header names, its event shapes, its
// idempotency header, its amount encoding — is a fact about THEM, and a fact
// about them does not belong in the domain that decides what a payment is. The
// domain says "open a checkout for this amount in this currency" and "here is a
// signed event, tell me what it claims". The adapter says "POST /v1/checkout and
// here is how you read the signature".
//
// This port is LOCAL INFRASTRUCTURE, not a cross-plane seam. That distinction is
// asserted mechanically in internal/arch/packages_test.go and it is the whole
// reason the name here is `payments` and not `provider`: the one seam between
// the planes is `dataplane`, and a payment processor reached over the internet
// does not make this application and the runtime into two applications that
// share state. It shares nothing with the runtime. It has its own database, its
// own credentials, its own failure modes, and this plane can be down for a week
// without the inference path noticing.
//
// Two rules run through every member below, and both are load-bearing.
//
// FIRST: nothing here moves money. A Checkout creates a checkout; it does not
// take one, and this port has no method that could. The credential is a hosted
// checkout the customer completes on the provider's own surface; this process
// never sees a card number, a CVV, or a cardholder field, because there is no
// type here for one to arrive in. That is the PCI posture, and it is structural
// rather than promised: a DTO without a card field cannot be made to accept
// one, and a reader can see that without being told.
//
// SECOND: a verified event is not a payment, it is a CLAIM about a payment. The
// port's verification step answers one question — "did our provider, over a
// trusted server-to-server channel, produce these exact bytes?" — and returns
// what the bytes say. It does not decide what happens next, does not look at a
// database, and does not know the word intent. The decision is the application
// layer's, and it is the layer that must resolve the claim against a payment
// this platform created, because only that layer can know which account was
// meant. An adapter that "resolves" an event to an account would be an adapter
// with the authority to move a customer's money, and the port below refuses to
// be shaped that way.
package payments

import (
	"context"
	"errors"
	"time"
)

// Checkout opens a hosted checkout and returns the URL the customer is sent to.
//
// It is one operation rather than a provider object because the port is the
// shape the APPLICATION needs, and the application needs exactly one thing: a
// place to send a customer who has decided to fund their account. A richer
// session abstraction would be a shape invented before a second caller arrived,
// which is the failure mode ports/outbound/dataplane's own header warns about —
// a port member with no caller is a shape invented twice.
//
// The URL is returned VERBATIM and is never parsed by anything on this side. It
// is a promise the provider makes and this process keeps on the provider's
// behalf; a consumer that url.Parse'd it to extract a session id would be
// interpreting a value whose only promise is that it may be handed to a
// browser, and would break silently the first time the provider changed the
// shape of its own URLs. The type says `string` for the same reason the fact
// feed's payload is json.RawMessage: the wire owns the meaning.
type Checkout interface {
	// OpenCheckout creates a checkout for the given amount in the given
	// currency and returns where to send the customer.
	//
	// The amount is integer MINOR UNITS — the same int64 money.go defines and
	// the ledger stores — and the currency is an ISO 4217 code. The provider
	// may send amounts in a different unit of its own choosing; the adapter
	// converts, and a conversion that is not exact is an error rather than a
	// rounding, because a rounded amount is a charge for a figure nobody agreed
	// to.
	//
	// A failure wraps ErrProviderUnavailable when the provider did not answer
	// at all, or answered that it is unwell, because the caller's response is
	// different and the difference is not a detail: one is a condition that
	// clears and the other is a defect. Everything else this method reports —
	// a refused credential, a parameter the provider would not accept, an
	// answer that does not decode — is a failure a retry cannot repair.
	OpenCheckout(ctx context.Context, in CheckoutRequest) (CheckoutSession, error)
}

// CheckoutRequest is one checkout, in this port's own vocabulary.
//
// Every field is set by the Control Plane and none of them comes from the
// customer's browser. The browser names a thing (see the top-up offer the
// application resolves); the server prices it, and this struct is what it
// priced. A port request with a caller-set amount and no policy behind it would
// be a way to say "the customer may charge themselves whatever they typed",
// which is the opposite of what a payment is.
//
// IdempotencyKey is not optional and not decorative. It is the SAME value on
// every attempt at the same logical checkout, and the reason is stated on the
// method: a lost response is a normal event, not an exception, and a retry
// that mints a new key is a second charge. The application derives it from the
// payment intent's own identity, which is durable before the first call, so
// "retry" and "first attempt" are the same request as far as the provider is
// concerned. An implementation that ignores it has not implemented this port.
type CheckoutRequest struct {
	// IdempotencyKey is the caller's stable identity for this logical
	// checkout, and the only thing standing between a network timeout and a
	// customer charged twice. It must be identical on every retry.
	IdempotencyKey string

	// AmountMinorUnits is the canonical funding amount in integer minor units,
	// strictly positive. It is what the ledger will be told to credit, and it
	// is the figure the event must later agree with.
	AmountMinorUnits int64

	// Currency is the ISO 4217 code the amount is denominated in. Uppercase.
	Currency string

	// Reference is this platform's own identifier for the payment — the intent
	// id. It is sent so the provider echoes it on the event, which lets the
	// application match an event to a payment it created rather than trusting a
	// metadata field to identify one.
	Reference string

	// ReturnURL is where the provider sends the customer after checkout. It is
	// built by this application from a configured base, never from a request
	// parameter: a checkout API that accepts an attacker's absolute redirect
	// URL is an open redirect on a payment page, and the URL is not merely
	// cosmetic here — it is where the customer lands holding a session.
	ReturnURL string
}

// CheckoutSession is where the customer goes, and what the provider calls it.
type CheckoutSession struct {
	// URL is the hosted checkout, returned verbatim. Never parsed here.
	URL string
	// ProviderRef is the provider's own identifier for the checkout, the
	// thing the later event will name. It is stored durably and is the link
	// between a provider's event and a payment this platform created.
	ProviderRef string
}

// WebhookVerifier answers one question about a delivery: is it ours, and what
// does it claim?
//
// It is its own interface beside Checkout rather than a member of one because
// the two have exactly one caller each and no common reason: a use case that
// opens checkouts should not have to name the verification it never performs,
// and the verification is called by a transport handler, not by a use case.
//
// It takes the RAW BYTES as an argument rather than reading a request, and that
// is the port's most important single property. A signature is computed over
// bytes, and the bytes that are verified must be the bytes that are interpreted.
// A verifier handed a *http.Request would have to read and parse it, and the
// path from "read" to "verify" to "parse again" is exactly where a re-serialised
// JSON body gets verified instead of the provider's own bytes: a decoded struct
// has lost its whitespace, its key order, its duplicate keys and its number
// spellings, and an HMAC over it authenticates something the provider never
// sent. Taking []byte makes that mistake unrepresentable — this port does not
// import net/http and has no way to be handed a parsed body at all.
type WebhookVerifier interface {
	// Verify checks the provider's signature over rawBody using the headers
	// supplied, and returns the event those bytes carry.
	//
	// A failure here means the delivery did not come from the configured
	// provider. It is not a retryable condition and must not be turned into
	// one: the caller answers a refusal and stops, because a wrong signature
	// will be wrong again. headers is the provider's signature headers as they
	// arrived, unparsed, with their multiplicity preserved — an implementation
	// that accepts "the first of several" has made header smuggling possible,
	// and the multiplicity is preserved HERE precisely so it cannot.
	Verify(headers map[string][]string, rawBody []byte) (ProviderEvent, error)
}

// ProviderEvent is one verified provider delivery, normalized.
//
// The zero value is not a valid event: Kind is empty. That is deliberate, and it
// is why the normalization below is where an implementation must be strict
// rather than where the application must be careful. A provider that adds a
// field, renames one, or changes an amount from an integer to a decimal string
// produces an event this build does not understand, and the answer to that must
// be a refusal — a recorded, operator-visible one — rather than a zero that
// flows into a money decision. An absent amount and an amount of zero are
// different sentences and a struct with plain values cannot tell them apart,
// so the money-bearing fields are pointers and a nil is a refusal.
type ProviderEvent struct {
	// Kind is the provider's own vocabulary, as a plain string. A Go enum here
	// would be a copy of a list the provider owns, and a copy goes stale the
	// day the provider adds a member; the application is what decides whether
	// it recognises a kind, exactly as the fact applier decides. An
	// unrecognised kind is a refusal, never a no-op.
	Kind string

	// EventID is the provider's delivery identity and the idempotency key for
	// this delivery. It is NOT the payment's identity: a provider routinely
	// emits several distinct events for one payment, and keying a credit on it
	// would fund that payment once per event. What the credit is keyed on is
	// PaymentRef below.
	EventID string

	// CheckoutRef is the provider's identifier for the CHECKOUT this platform
	// opened, as the delivery stated it, or empty when the delivery names no
	// checkout.
	//
	// It is the reference a CAPTURE is resolved by, because it is the one this
	// plane wrote down before the customer was ever sent anywhere: the
	// checkout's id is stored when the session is created, so a delivery
	// naming it resolves against a row that already exists rather than against
	// one the delivery itself would have to create.
	//
	// Empty is an ordinary value and not a failure. A provider's refund
	// delivery names the payment, not the session it was taken through — the
	// session is not part of an event about money going back — so an
	// implementation that had only this field would be unable to report a
	// refund it had perfectly well authenticated. See PaymentRef.
	CheckoutRef string

	// PaymentRef is the provider's identifier for the PAYMENT — the economic
	// event — as distinct from the delivery that reported it, or empty when
	// the provider did not name one.
	//
	// It is the reference a REFUND is resolved by, and it is what a funding
	// leg's idempotency key is derived from: an idempotency key has to name
	// the ECONOMIC event, because two deliveries of one capture must derive
	// one key — keying on the delivery's own id would fund a payment once per
	// event — and a refund that names this value is naming the same thing the
	// capture did.
	//
	// The two fields are separate rather than one "reference" with a footnote
	// because they are genuinely different identifiers from different provider
	// objects, and the application resolves them against different stored
	// columns. Collapsing them into one field was the shape this port used to
	// have, and its consequence was concrete: a refund carried a payment id
	// into a lookup that only knew checkout ids, so every refund this platform
	// received was recorded as a payment it could not find.
	PaymentRef string

	// AmountMinorUnits is the provider's reported amount. Non-nil only when
	// the event is one that reports an amount; nil is a refusal, not a zero.
	AmountMinorUnits *int64

	// Currency is the provider's reported ISO 4217 code, or empty for an event
	// that reports none. It is checked for EQUALITY against what the platform
	// sent, before any amount comparison, because an amount is not a figure
	// until the unit it counts in is known.
	Currency string

	// OccurredAt is when the provider observed the outcome. It is compared
	// against a clock tolerance on the RECEIPT, which constrains how long a
	// captured delivery stays useful and does not constrain how long a
	// delivered effect may be applied.
	OccurredAt time.Time
}

// ErrBadSignature reports that a delivery did not authenticate: the signature
// header was absent, repeated, malformed, or did not match these exact bytes.
//
// It is a distinct sentinel rather than a generic error because the CALLER's
// response differs by cause and the difference is the whole point. A bad
// signature means "this is not our provider" and the delivery is refused
// permanently — retrying a wrong signature is not an act of optimism, it is a
// loop. A provider outage, a malformed-but-authentic body, and a database that
// was briefly unavailable all arrive as OTHER errors, and the caller answers
// those differently.
var ErrBadSignature = errors.New("payments: the delivery's signature did not verify against its signing secret")

// ErrMalformedEvent reports that a delivery authenticated but this build
// cannot interpret it: an event kind it does not know, a schema version ahead
// of it, a money field absent where the provider's contract requires one, or an
// amount that is not an exact integer in the unit the currency implies.
//
// It is a distinct sentinel for the same reason ErrBadSignature is. A refusal
// of a well-signed event is an operator's problem, not a retry's: the provider
// will keep sending whatever shape it sends, and a build that guessed at an
// unfamiliar shape would be deciding how much money a customer paid on the
// strength of a type it invented.
//
// WHAT THE CALLER DOES WITH IT is narrower than it looks, and the narrowing is
// the whole reason this sentinel is not a general "I did not like that body".
// The application records a delivery under the key (provider, provider account,
// event id); it can only do that for a delivery it can NAME. An implementation
// therefore returns this error only when it could not read an event id out of
// the bytes at all, and in that case the caller writes NOTHING and answers 2xx:
// a row keyed on an absent event id would swallow the next unreadable delivery
// as a duplicate of this one, and reporting a real event as already-seen is a
// worse failure than reporting nothing.
//
// A delivery that HAS an event id is not malformed in this sense even when its
// kind is unfamiliar or its amount is spelled in a unit this build cannot
// convert exactly. Those are read into the event below and refused by the
// APPLICATION, which quarantines them with a reason an operator can act on —
// because "we understood your message and cannot use it" and "we could not read
// your message" are different things to tell a provider, and different things
// for a human to be woken up about.
var ErrMalformedEvent = errors.New("payments: the delivery is well-signed but this build cannot name or read it")

// ErrProviderUnavailable reports that the provider did not answer, or answered
// that it is unwell: a transport failure, a 5xx, or a 429.
//
// It is a distinct sentinel because the CALLER's answer differs, and the
// difference is what a client does next rather than a nuance of logging. The
// condition is the provider's and is expected to clear, so the caller answers
// the contract's `503 upstream_unavailable` — "retry later, not differently" —
// and a generated client that retries is behaving correctly. Every other
// failure this port reports is a refusal that the same request would earn
// again, and answering one of those 5xx would ask for a retry storm against a
// condition no retry can change.
//
// A 4xx is deliberately NOT one of these, and the line is drawn there rather
// than at "any non-2xx" because the two mean opposite things: a provider that
// rejected the request rejected something about it — a credential, a
// parameter, a permission — and no amount of retrying supplies what it
// refused. That is this plane's defect or this plane's configuration, and the
// honest answer is an internal error.
var ErrProviderUnavailable = errors.New("payments: the provider did not answer, or answered that it is unavailable")

// The two delivery kinds this build interprets, in this port's own vocabulary.
//
// They are constants of the PORT rather than of a provider, and that is what
// the adapter's normalisation is FOR: a provider's `payment_intent.succeeded`
// and another's `charge.completed` are two spellings of one thing this platform
// does, and the application above must not have to learn either spelling. An
// adapter maps the provider's own kind onto one of these two or refuses with
// ErrMalformedEvent.
//
// The set is deliberately tiny, and its smallness is the design rather than a
// stage of it. `captured` and `refunded` are the only two events that can
// change what a customer's balance is owed, and every other thing a provider
// emits — a checkout expiring, a dispute opening, a receipt being mailed — is
// either a local decision this platform makes for itself or an operational fact
// a reconciliation reads rather than a state machine consumes. A port with a
// kind for each of those would be a port that grows every time a provider ships
// a feature, and the application would grow with it.
//
// An unrecognised kind is NOT a no-op: the application records it and answers
// 2xx (see the disposition contract), because refusing it loudly would page
// someone every time the provider added a type — and the provider's event is
// not going to fix itself by being sent again.
const (
	// KindCaptured reports that the provider took the money. It is the only
	// kind that funds anything.
	KindCaptured = "captured"

	// KindRefunded reports that the provider returned money to the customer.
	//
	// It is interpreted and RECORDED and it does not move a ledger leg, and
	// the reason is stated at length in the application's refund path: a
	// refund is a debit the accounting algebra cannot represent once the
	// money has been spent. The kind exists so the event is understood and
	// answerable rather than quarantined as noise, because "we did not
	// understand your message" and "we understood it and recorded it" are
	// different things to tell a provider.
	KindRefunded = "refunded"
)
