// Package payments is the Control Plane's outbound port to an external payment
// provider: the seam through which a customer is told where to send money and
// through which money is recognised.
//
// It is a port in this module for the reason ports/outbound/dataplane is one and
// for a different reason. There, the port existed because the peer was a
// separate Go module and an import path could not cross. Here the provider is a
// third party's HTTPS API, and the reason is the same in the end: the provider's
// vocabulary — its endpoints, its header names, its event shapes, its amount
// encoding — is a fact about THEM, and a fact about them does not belong in the
// domain that decides what a payment is. The domain says "obtain a destination
// for this amount in this currency" and "here is a signed event, tell me what it
// claims". The adapter says "POST this order endpoint and here is how you read
// the signature".
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
// FIRST: nothing here moves money. OpenTransfer asks the provider to issue a
// DESTINATION and answers with its own description of that destination; it does
// not take a payment, and this port has no method that could. The instrument is
// a bank transfer arriving at an account the provider issued for this one
// payment, and the moment of payment is a bank's, not this process's. That is a
// structural posture rather than a promised one: there is no type here for a
// card number, a CVV or a cardholder field to arrive in, because money never
// travels through this port in the outbound direction at all — it only ever
// arrives as a claim, on the inbound side below.
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

// Transfers obtains, from the provider, the destination a customer's money
// should be sent to.
//
// It is one operation rather than a provider object because the port is the
// shape the APPLICATION needs, and the application needs exactly one thing: a
// destination for a customer who has decided to fund their account. A richer
// account abstraction would be a shape invented before a second caller arrived,
// which is the failure mode ports/outbound/dataplane's own header warns about —
// a port member with no caller is a shape invented twice.
//
// It is named for the movement of money and not for the provider's object,
// because the provider's object is exactly the thing this port exists to keep
// out of the application: one provider calls this an order, another calls it a
// payment link, and both are the same fact — a place money can arrive that names
// one payment.
type Transfers interface {
	// OpenTransfer asks the provider for a destination for the given amount in
	// the given currency, and answers with the provider's own description of
	// it.
	//
	// The amount is integer MINOR UNITS — the same int64 money.go defines and
	// the ledger stores — and the currency is an ISO 4217 code. The provider
	// may send amounts in a different unit of its own choosing; the adapter
	// converts, and a conversion that is not exact is an error rather than a
	// rounding, because a rounded amount is a request for a figure nobody
	// agreed to.
	//
	// It is NOT idempotent in the sense a caller might hope for, and the port
	// is honest about that rather than promising the impossible: a provider
	// that keys a destination on a caller-supplied identity answers a repeat
	// with a conflict, not with the destination it already issued, and a
	// provider that offers no way to read that destination back leaves nothing
	// to converge on. Such a refusal is reported as ErrOrderCodeTaken, and the
	// caller's answer is to abandon the attempt — see that sentinel.
	//
	// A failure wraps ErrProviderUnavailable when the provider did not answer
	// at all, or answered that it is unwell, because the caller's response is
	// different and the difference is not a detail: one is a condition that
	// clears and the other is a defect. Everything else this method reports —
	// a refused credential, a parameter the provider would not accept, an
	// answer that does not decode — is a failure a retry cannot repair.
	OpenTransfer(ctx context.Context, in TransferRequest) (TransferInstructions, error)
}

// TransferRequest is one transfer destination, in this port's own vocabulary.
//
// Every field is set by the Control Plane and none of them comes from the
// customer's browser. The browser names a thing (see the top-up offer the
// application resolves); the server prices it, and this struct is what it
// priced. A port request with a caller-set amount and no policy behind it would
// be a way to say "the customer may pay themselves whatever they typed", which
// is the opposite of what a payment is.
//
// IdempotencyKey is not optional and not decorative. It is the SAME value on
// every attempt at the same logical transfer, and it is derived from the
// payment intent's own identity, which is durable before the first call — so
// "retry" and "first attempt" name the same thing on the wire, and a provider
// that refuses a duplicate is refusing a duplicate of a request this platform
// genuinely already made. An implementation that ignores it has not implemented
// this port.
type TransferRequest struct {
	// IdempotencyKey is the caller's stable identity for this logical
	// transfer, and the only thing standing between a repeated call and a
	// customer handed two destinations for one payment. It must be identical
	// on every retry.
	IdempotencyKey string

	// AmountMinorUnits is the canonical funding amount in integer minor units,
	// strictly positive. It is what the ledger will be told to credit, and it
	// is the figure the event must later agree with.
	AmountMinorUnits int64

	// Currency is the ISO 4217 code the amount is denominated in. Uppercase.
	Currency string

	// ExpiresIn is how long the destination should stay valid, as this
	// platform's own decision about its own patience.
	//
	// It is here rather than in the adapter's configuration because the window
	// the console TELLS a customer and the window the provider ENFORCES are one
	// fact, and two authorities for one fact drift. This process already
	// decides how long it waits before giving up on a payment locally; handing
	// that same value to the provider is what makes the printed deadline and
	// the issued account agree. An implementation encodes it in whatever unit
	// its provider states, exactly, and refuses a value it cannot state exactly
	// rather than rounding it: a rounded lifetime is an account that expires at
	// a moment nobody chose.
	ExpiresIn time.Duration
}

// TransferInstructions is where the money goes, in the provider's own words.
//
// Every string in it is returned VERBATIM and is never parsed by anything on
// this side. The bank's name and the account holder's are a promise the
// provider makes and this process keeps on the provider's behalf, and the
// reference is the value a later delivery will name — a consumer that tried to
// decompose any of them would be interpreting values whose only promise is that
// they describe a destination to a human, and would break silently the first
// time the provider changed the shape of its own identifiers. The fields say
// `string` for the same reason the fact feed's payload is json.RawMessage: the
// wire owns the meaning.
type TransferInstructions struct {
	// TransferCode is the provider's own identifier for this destination — the
	// account it issued for this payment alone. It is stored durably and is the
	// link between a provider's delivery and a payment this platform created:
	// money can only arrive at an account the provider issued, so a delivery
	// naming it is a claim about money that really moved, and not about a
	// customer's free text that a bank might have rewritten.
	TransferCode string

	// BankName is the provider's name for the institution the destination sits
	// at, and AccountHolder is the name it is held in. Both are EVIDENCE FOR
	// THE CUSTOMER — the console renders them so a person can check the
	// destination before sending anything — and neither is ever compared
	// against a delivery.
	BankName      string
	AccountHolder string

	// QRURL is the provider's own image of the transfer to make, or empty when
	// the provider issued none.
	//
	// Empty is an ordinary value rather than a failure: the destination above
	// is complete without it, a customer can type an account number into a
	// banking app, and a build that refused to open a payment because an image
	// was missing would be refusing money over a decoration. The image is
	// returned verbatim and never composed here, which is deliberate — see the
	// ADR that chose this provider: an image this platform drew would be an
	// encoding this platform has to get right, for a value whose only reader is
	// somebody's camera.
	QRURL string
}

// WebhookVerifier answers one question about a delivery: is it ours, and what
// does it claim?
//
// It is its own interface beside Transfers rather than a member of one because
// the two have exactly one caller each and no common reason: a use case that
// opens transfers should not have to name the verification it never performs,
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

	// TransferRef is the provider's identifier for the DESTINATION it issued
	// for this platform's payment, as the delivery stated it, or empty when the
	// delivery names no destination.
	//
	// It is the reference a CAPTURE is resolved by, because it is the one this
	// plane wrote down before the customer was ever shown it: the destination's
	// id is stored when the transfer is opened, so a delivery naming it
	// resolves against a row that already exists rather than against one the
	// delivery itself would have to create. It is also the reason a capture
	// needs no trust in the payload beyond the signature — the value is an
	// account the provider issued for this payment, so money reaching it is
	// money this platform asked for, which a transfer memo could never prove.
	//
	// Empty is an ordinary value and not a failure. A provider's refund
	// delivery names the payment, not the destination it arrived at — the
	// destination is not part of an event about money going back — so an
	// implementation that had only this field would be unable to report a
	// refund it had perfectly well authenticated. See PaymentRef.
	TransferRef string

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
	// into a lookup that only knew transfer ids, so every refund this platform
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

// ErrOrderCodeTaken reports that the provider already holds a destination under
// the identity this request offered it, and will not issue another.
//
// It is a distinct sentinel because the caller's answer is neither a retry nor
// an internal error: the attempt is UNRECOVERABLE and must be abandoned. The
// destination the provider issued under that identity exists, but nothing on
// this side has ever seen it — it was never returned to this process, and the
// provider documents no way to read it back by the identity that names it — so
// there is no value to show a customer and no way to obtain one. Retrying
// changes nothing (the provider will refuse identically), and retrying under a
// fresh identity is precisely what the caller must choose DELIBERATELY rather
// than stumble into.
//
// The condition is reachable without a fault anywhere, and that is why it has a
// name of its own. An identity derived from a payment's own id is refused on a
// second attempt at that payment, and a second attempt is the ordinary
// consequence of a lost response, a crashed process between the provider's call
// and the durable write, or an operator replaying a failure. Every one of those
// leaves a destination at the provider that no customer was ever told about,
// which is money nobody can send and nobody is waiting for.
//
// The name is the provider's own words for the thing it refused, and that is
// deliberate rather than a leak: this sentinel is the ONE place the port admits
// that a provider's identity for a destination is something a caller can
// collide with, and a neutral name like ErrAlreadyOpened would hide from the
// adapter author exactly which condition they are translating.
var ErrOrderCodeTaken = errors.New("payments: the provider already holds a destination under that order identity")

// The two delivery kinds this build interprets, in this port's own vocabulary.
//
// They are constants of the PORT rather than of a provider, and that is what
// the adapter's normalisation is FOR: two providers' spellings of "the money
// arrived" are two spellings of one thing this platform does, and the
// application above must not have to learn either spelling. An adapter maps the
// provider's own kind onto one of these two or refuses with ErrMalformedEvent.
//
// The set is deliberately tiny, and its smallness is the design rather than a
// stage of it. `captured` and `refunded` are the only two events that can
// change what a customer's balance is owed, and every other thing a provider
// emits — a transfer expiring, a dispute opening, a receipt being mailed — is
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

// ProviderName is the name a provider is KNOWN BY — the one that says which
// vocabulary its events speak and which signature its webhook carries — as
// opposed to the name a deployment gives its ledger namespace, which says
// nothing about the protocol on the wire.
//
// The two being separate is the reason this function exists rather than a
// switch in the composition root. The ledger namespace is per-DEPLOYMENT: one
// operator may run this build against two providers, and their payments live in
// two namespaces on purpose. The provider name is per-ADAPTER: it is a fact
// about the code, and a deployment that names a provider it has no adapter for
// has configured a namespace no signature can ever satisfy.
//
// A composition root that ignores the return value and builds the only adapter
// it has produces a process that starts healthy, logs that it is ready for
// that provider, and answers 400 "did not authenticate" to every genuine
// delivery — while the operator is sent to look at the endpoint secret, which
// is the one thing that is certainly not wrong. That is the same no-symptom
// failure shape the configuration's placeholder-secret check exists to prevent,
// one layer up: here the value is not a weak secret but a name no code
// implements.
func ProviderName(string) (string, bool) {
	return "sepay", true
}
