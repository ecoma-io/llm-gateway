package paymentprovider

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	payments "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/payments"
)

// The provider's webhook signing scheme, as its documentation states it.
//
// Stripe sends one header, named below, whose value is a comma-separated list
// of key=value elements: `t` is the Unix timestamp in seconds at which the
// delivery was signed, `v1` is the HMAC-SHA256 of the string formed by
// concatenating that timestamp's decimal spelling, a ".", and the request body
// — `"{t}.{rawBody}"` — keyed with the endpoint's signing secret (the value
// the provider's dashboard calls its webhook secret, used as its own bytes).
// The `v0` scheme and any further element the provider adds are not part of
// this build's vocabulary and are ignored; a `v1` is required.
const (
	// signatureHeader is the header the delivery's signature arrives in.
	signatureHeader = "Stripe-Signature"

	// timestampElement and signatureElement are the two elements this build
	// reads out of that header, spelled as the provider spells them.
	timestampElement = "t"
	signatureElement = "v1"
)

// ErrStaleDelivery reports that a delivery authenticated but its signed
// timestamp is outside the configured webhook tolerance.
//
// It is a sentinel of THIS package and not one of the port's, because the port
// is frozen and its two sentinels answer two different questions: a bad
// signature means "this did not come from our provider" and a malformed event
// means "this came from our provider and this build cannot read it". "This
// came from our provider, this build can read it, and it is too old to act on"
// is a third answer, and a caller that could not tell it apart from the first
// would be unable to say whether an operator should be looking at the endpoint
// secret or at a clock.
//
// It deliberately does NOT wrap ErrBadSignature. A stale delivery is authentic
// — the signature verified — so a caller that treated it as an authentication
// failure would be answering the wrong question.
var ErrStaleDelivery = errors.New("paymentprovider: the delivery's signed timestamp is outside the configured webhook tolerance")

// Verifier verifies the provider's webhook deliveries and reports what they
// claim.
type Verifier struct {
	// signingSecret is the endpoint's signing secret, held only to be used as
	// an HMAC key. It never reaches a log line or an error.
	signingSecret string

	// tolerance is how far the delivery's signed timestamp may sit from now,
	// in either direction.
	tolerance time.Duration

	// now is the clock the freshness check reads. It is a field rather than a
	// direct call to time.Now so the check can be exercised at a fixed instant,
	// which is the difference between a test of the tolerance and a test of
	// how fast the machine running it happens to be.
	now func() time.Time
}

// NewVerifier returns the verifier described by an endpoint's signing secret
// and the staleness bound. It panics on an empty secret or a non-positive
// tolerance for the reason New panics on an empty API secret: both are wiring
// defects, and a verifier with either is a verifier that answers yes to every
// delivery or refuses every delivery.
func NewVerifier(signingSecret string, tolerance time.Duration) *Verifier {
	if signingSecret == "" {
		panic("paymentprovider: NewVerifier requires a webhook signing secret")
	}
	if tolerance <= 0 {
		panic("paymentprovider: NewVerifier requires a positive webhook tolerance")
	}
	return &Verifier{signingSecret: signingSecret, tolerance: tolerance, now: time.Now}
}

// Compile-time proof that the verifier satisfies the port's verification half.
var _ payments.WebhookVerifier = (*Verifier)(nil)

// Verify checks the provider's signature over rawBody and returns the event
// those bytes carry.
//
// It signs and compares the RAW BYTES it was handed, and that is the single
// most important property of this method. The signature covers the provider's
// own octets: whitespace, key order, duplicate keys, number spellings and all.
// A verifier that decoded the body into a struct and re-encoded it to check the
// signature would authenticate a body the provider never sent — the re-encoded
// form is a different byte string, and while the two happen to agree for a
// canonical encoder today, "happens to agree" is not a security property. The
// port takes []byte rather than an *http.Request precisely so that the bytes
// which are verified are the bytes which are interpreted; this method keeps
// that promise by never constructing a second representation of the body
// before the HMAC is compared.
//
// The refusal order is deliberate: the header, then the signature, then the
// freshness, then the content. An unauthenticated caller therefore learns
// nothing about this deployment's clock or its event vocabulary, and a delivery
// that fails freshness but not authentication is reported as stale rather than
// as unauthentic.
func (v *Verifier) Verify(headers map[string][]string, rawBody []byte) (payments.ProviderEvent, error) {
	header, err := singleHeader(headers, signatureHeader)
	if err != nil {
		return payments.ProviderEvent{}, err
	}
	timestamp, signature, err := parseSignatureHeader(header)
	if err != nil {
		return payments.ProviderEvent{}, err
	}
	provided, err := hex.DecodeString(signature)
	if err != nil {
		return payments.ProviderEvent{}, fmt.Errorf("%w: the signature is not a hexadecimal digest", payments.ErrBadSignature)
	}

	// hmac.Equal rather than bytes.Equal: the comparison is between a value an
	// attacker chose and a value derived from a secret, and a byte-by-byte
	// comparison that stops at the first difference is a timing oracle for
	// forging the rest.
	expected := sign(v.signingSecret, timestamp, rawBody)
	if !hmac.Equal(expected, provided) {
		return payments.ProviderEvent{}, fmt.Errorf("%w: the signature does not match these bytes", payments.ErrBadSignature)
	}

	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		// Unreachable through parseSignatureHeader, which parses the element
		// before returning it; kept because a signed timestamp this build
		// cannot read is not a timestamp to compare against, and the branch
		// that cannot happen is cheaper than the assumption that it cannot.
		return payments.ProviderEvent{}, fmt.Errorf("%w: the signed timestamp is not a Unix time in seconds", payments.ErrBadSignature)
	}
	if !withinTolerance(time.Unix(seconds, 0).UTC(), v.now(), v.tolerance) {
		return payments.ProviderEvent{}, fmt.Errorf("%w: the delivery was signed %s ago and the tolerance is %s", ErrStaleDelivery, v.now().Sub(time.Unix(seconds, 0).UTC()), v.tolerance)
	}

	return normalise(rawBody)
}

// singleHeader returns the one value of the named header, matching the name
// case-insensitively and refusing any multiplicity.
//
// net/http lowercases header keys on the way in, so in production this loop
// matches the same key it would match with an exact comparison — but the map
// is a map, and a caller constructing one by hand (a test, a proxy shim, a
// future transport that preserves case) can put two entries in it that name
// the same header. Two entries, or one entry with two values, are the same
// hazard: an implementation that picks "the first of several" lets a request
// carry one signature the deployment accepts and another it does not, which is
// header smuggling, and the port's own comment names this as the thing the
// multiplicity is preserved to prevent. The inbound requestid middleware
// refuses a repeated request id on exactly this reasoning.
func singleHeader(headers map[string][]string, name string) (string, error) {
	values := []string{}
	for key, many := range headers {
		if strings.EqualFold(key, name) {
			values = append(values, many...)
		}
	}
	switch len(values) {
	case 0:
		return "", fmt.Errorf("%w: the delivery carried no %s header", payments.ErrBadSignature, name)
	case 1:
		return values[0], nil
	default:
		return "", fmt.Errorf("%w: the delivery carried %d %s headers, and a request that authenticates under more than one is smuggling", payments.ErrBadSignature, len(values), name)
	}
}

// parseSignatureHeader reads the `t` and `v1` elements out of the signature
// header and refuses every way it can fail to state exactly one of each.
//
// The timestamp is returned as the STRING the header carried, not as a number,
// because the string is what the provider signed: `"{t}.{rawBody}"`. Parsing it
// into an integer and formatting it back would be a normalisation of the signed
// material, and a header spelling the timestamp with a leading zero would then
// be signed under one string and verified under another. The decimal value is
// parsed separately, and only to compare against the clock.
//
// A repeated `v1` is refused rather than matched against any of them. The
// provider documents that two signatures appear during a secret rotation, and
// that is exactly the shape that makes accepting any of several unsafe: this
// deployment has ONE configured secret, so a delivery carrying two v1 values
// carries one it expects this endpoint to ignore, and a verifier that ignores
// it cannot say which secret authenticated the delivery. The cost is that a
// rotation must be done in a step, which is a deployment decision an operator
// makes deliberately; the alternative is a permanent open door.
func parseSignatureHeader(header string) (timestamp, signature string, err error) {
	var (
		haveTimestamp, haveSignature bool
	)
	for _, element := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(element), "=")
		if !ok {
			return "", "", fmt.Errorf("%w: the signature header carries an element that is not a key=value pair", payments.ErrBadSignature)
		}
		switch key = strings.TrimSpace(key); key {
		case timestampElement:
			if haveTimestamp {
				return "", "", fmt.Errorf("%w: the signature header carries the timestamp more than once", payments.ErrBadSignature)
			}
			haveTimestamp = true
			timestamp = strings.TrimSpace(value)
		case signatureElement:
			if haveSignature {
				return "", "", fmt.Errorf("%w: the signature header carries more than one %s signature", payments.ErrBadSignature, signatureElement)
			}
			haveSignature = true
			signature = strings.TrimSpace(value)
		default:
			// Unknown elements — the provider's `v0` scheme, and whatever it
			// adds later — are ignored rather than refused: they are not part
			// of this build's verification, and a scheme the provider stops
			// sending must not be what stops this endpoint accepting its
			// deliveries.
		}
	}
	if !haveTimestamp {
		return "", "", fmt.Errorf("%w: the signature header carries no %s timestamp", payments.ErrBadSignature, timestampElement)
	}
	if !haveSignature {
		return "", "", fmt.Errorf("%w: the signature header carries no %s signature", payments.ErrBadSignature, signatureElement)
	}
	if timestamp == "" || signature == "" {
		return "", "", fmt.Errorf("%w: the signature header carries an empty %s or %s value", payments.ErrBadSignature, timestampElement, signatureElement)
	}
	if _, err := strconv.ParseInt(timestamp, 10, 64); err != nil {
		return "", "", fmt.Errorf("%w: the signed timestamp is not a Unix time in seconds", payments.ErrBadSignature)
	}
	return timestamp, signature, nil
}

// sign computes the provider's signature over the timestamp's own spelling and
// the raw body: HMAC-SHA256(key = signing secret, message = "{t}.{rawBody}").
//
// The concatenation is written out as three writes rather than by building the
// signed string, so the body is never copied and never re-encoded on its way
// into the MAC.
func sign(signingSecret, timestamp string, rawBody []byte) []byte {
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(rawBody)
	return mac.Sum(nil)
}

// withinTolerance reports whether a signed instant is close enough to now, in
// either direction.
//
// The comparison is symmetric and the sign of the test matters: a provider's
// clock running slightly ahead of this platform's is common and benign, and a
// one-sided tolerance would refuse real deliveries from a provider whose clock
// is fast. The domain carries the same function — occurredWithinTolerance in
// the payments domain — for the same reason, and the two are stated the same
// way round so a reader who has seen one recognises the other.
func withinTolerance(signed, now time.Time, tolerance time.Duration) bool {
	skew := now.Sub(signed)
	return skew <= tolerance && skew >= -tolerance
}

// The event kinds this adapter reads, spelled exactly as the provider spells
// them.
//
// The provider emits hundreds of these and this build reads five. That small
// number is the design rather than an early stage of it: what follows is not a
// list of everything that can arrive, it is the list of everything whose
// meaning this platform has decided IN ADVANCE. Everything else is passed
// through to the application under the provider's own name — see portKind —
// and quarantined there with a reason an operator can act on.
const (
	// kindCheckoutCompleted is the primary delivery: the customer finished the
	// checkout. With a card it arrives paid; with a delayed-notification method
	// it may arrive UNPAID, which is why payment_status is read below.
	kindCheckoutCompleted = "checkout.session.completed"
	// kindCheckoutAsyncPaymentSucceeded is the delayed notification that the
	// money arrived, and it follows an unpaid completion.
	kindCheckoutAsyncPaymentSucceeded = "checkout.session.async_payment_succeeded"
	// kindCheckoutAsyncPaymentFailed is the delayed notification that the money
	// did not arrive.
	kindCheckoutAsyncPaymentFailed = "checkout.session.async_payment_failed"
	// kindCheckoutExpired is the session this platform's own TTL outlived,
	// announced.
	kindCheckoutExpired = "checkout.session.expired"
	// kindChargeRefunded is the provider reporting that money went BACK to the
	// customer. It is a CHARGE event rather than a session event, and the
	// difference is load-bearing: the object it carries is a Charge, and a
	// Charge's identifiers are not the checkout reference this platform stored.
	// See the reference discussion in normalise.
	kindChargeRefunded = "charge.refunded"
)

// eventEnvelope is the wire shape of a provider event, as much of it as this
// build reads: the delivery's identity, its kind, when the provider observed
// it, and the object it is about.
//
// Every field is a RAW JSON TOKEN rather than a typed value, and that is the
// one decision this file's tolerance rests on. With typed fields, a single
// unreadable field fails the decode of the WHOLE envelope: a body whose
// `amount_total` is spelled `12.5`, or quoted as a string, or turned into an
// object by a schema change, would take the event id down with it — and the
// event id is the one thing the caller needs to record the delivery at all.
// Reading each field as its own token means a field this build cannot read
// costs exactly that field: the port reports it as an ABSENT value and the
// APPLICATION quarantines the delivery with a reason a human resolves
// (amount_mismatch, unknown_kind, unknown_payment). The port's
// ErrMalformedEvent comment carries the argument in full — "we could not read
// your message" and "we read it and could not use it" are different things to
// tell a provider, and only the first of them is this function's refusal.
//
// api_version is deliberately NOT read. The provider versions its API and this
// build does not gate on the version string, because the version is not what
// gives a field its meaning — the field does. A release that renamed
// amount_total would produce a delivery with no readable amount, which is a
// quarantine the application records with evidence; gating on the version
// instead would refuse every delivery on the day the provider shipped an
// unrelated field, and the refusal would be permanent.
type eventEnvelope struct {
	ID      json.RawMessage `json:"id"`
	Type    json.RawMessage `json:"type"`
	Created json.RawMessage `json:"created"`
	Data    *eventData      `json:"data"`
}

// eventData is the envelope's data member, read only far enough to reach the
// object inside it.
type eventData struct {
	Object json.RawMessage `json:"object"`
}

// eventObject is the wire shape of the object a delivery is about, across BOTH
// shapes this adapter reads: a Checkout Session (the session events) and a
// Charge (the refund event). The field names do not collide between the two, so
// one struct reads both, and each kind below reads only the members that belong
// to its own shape.
type eventObject struct {
	// ID is the object's own identifier: the Checkout Session's id on a session
	// event — which is exactly the reference this platform stored when it
	// opened the checkout — and the Charge's id on a charge event.
	ID json.RawMessage `json:"id"`

	// AmountTotal is the session's total, in the currency's smallest unit: the
	// same unit the port's minor units are.
	AmountTotal json.RawMessage `json:"amount_total"`

	// AmountRefunded is how much of a charge the provider has refunded. It is
	// the charge's RUNNING total, not one refund's increment.
	AmountRefunded json.RawMessage `json:"amount_refunded"`

	// PaymentIntent is the payment intent a charge belongs to, which is the
	// provider's identifier for the PAYMENT rather than for the checkout. It is
	// null on charges old enough to predate payment intents, which is why the
	// reader below falls back to the charge's own id.
	PaymentIntent json.RawMessage `json:"payment_intent"`

	// Currency is the provider's lowercase ISO 4217 code.
	Currency json.RawMessage `json:"currency"`

	// PaymentStatus distinguishes a completed session whose money has arrived
	// ("paid") from one whose money has not ("unpaid").
	PaymentStatus json.RawMessage `json:"payment_status"`
}

// maxProviderEventIDLength is how long an event id this build can RECORD is.
//
// It is the width of the column the id is stored in — provider_event_id on
// control.payment_events, and the matching column on control.payment_quarantine
// — and it is stated here rather than left to the database because the two
// layers would give different answers to the same delivery. The column's answer
// is a constraint violation, which reaches the provider as a 5xx asking it to
// retry bytes that can never be stored; this adapter's answer is the port's
// ErrMalformedEvent, which the handler turns into the 2xx it gives every
// delivery this build cannot record, with the reason in the log.
//
// It is generous by two orders of magnitude against the ids real providers
// mint, so it is not a limit on any delivery that exists — it is the bound that
// keeps a schema constraint from being the thing a provider discovers.
const maxProviderEventIDLength = 255

// normalise reads a verified body into the port's event.
//
// There is exactly ONE refusal in this function and it is narrow on purpose: a
// body whose event id cannot be read at all. The caller records a delivery
// under (provider, provider account, event id), so a delivery it cannot name is
// one it cannot record — and an id invented here would be worse than no record,
// because the row would swallow the NEXT unreadable delivery as a duplicate of
// this one. The port's ErrMalformedEvent comment states the whole argument.
//
// Everything else is read into the event and left to the application, including
// everything this build does not understand. An unfamiliar kind crosses under
// the provider's own name; an amount whose spelling cannot be converted exactly
// is reported as an ABSENT amount rather than a rounded one, which is the one
// way an amount can go quietly wrong; a delivery naming no object at all is
// reported with no reference. Each of those becomes a quarantine the
// application writes with an operator-actionable reason, and each is a better
// outcome than a 2xx that recorded nothing while the provider's real delivery
// went unseen.
//
// The body has already been authenticated when this runs. That is what makes a
// named-but-unreadable delivery the application's problem rather than an
// authentication one.
func normalise(rawBody []byte) (payments.ProviderEvent, error) {
	var envelope eventEnvelope
	if err := json.NewDecoder(bytes.NewReader(rawBody)).Decode(&envelope); err != nil {
		// The one decode failure that IS a refusal: bytes that are not a JSON
		// object name nothing, so there is no delivery id to record and no
		// field to read out of them.
		return payments.ProviderEvent{}, fmt.Errorf("%w: the body would not decode as a provider event: %w", payments.ErrMalformedEvent, err)
	}

	eventID, ok := rawString(envelope.ID)
	if !ok || eventID == "" {
		// The delivery's own identity, and the idempotency key for THIS
		// delivery. Absent, empty, or spelled as something other than a string
		// are one answer: this body cannot be named, so the caller writes
		// nothing and answers 2xx rather than keying a row on an identity it
		// made up.
		return payments.ProviderEvent{}, fmt.Errorf("%w: the body carries no event id, so the caller has no key to record it under", payments.ErrMalformedEvent)
	}
	if len(eventID) > maxProviderEventIDLength {
		// The same answer as an absent id, for the same reason and by the same
		// route: this build cannot RECORD a delivery under this id, and the
		// caller's contract is that it either records the delivery or answers
		// 2xx having recorded nothing. It is not a fourth outcome, and the
		// alternative is worse than it looks — the id is written to a bounded
		// column, so refusing it is not a choice this adapter is making, it is
		// one the storage layer would make by raising a constraint violation
		// that surfaces as a 5xx, asking the provider to retry a delivery whose
		// bytes can never be stored. A 200 tells the provider to stop and puts
		// the reason in this plane's log, which is the one place an operator
		// can act on it.
		return payments.ProviderEvent{}, fmt.Errorf("%w: the body's event id is %d characters and this build records at most %d, so the caller has no key to record it under",
			payments.ErrMalformedEvent, len(eventID), maxProviderEventIDLength)
	}

	object := decodeObject(envelope.Data)
	providerKind, _ := rawString(envelope.Type)

	// What the delivery names, and for how much. The provider's two shapes
	// state the same facts under different names, so the branch is by kind and
	// never by guess: reading `amount_total` off a charge would be reading a
	// field that is not there and calling its absence "no amount".
	//
	// TWO REFERENCES COME OUT OF THIS BRANCH and they are not interchangeable.
	// A Checkout Session's own id is the reference this platform wrote when it
	// opened the checkout, and the session's `payment_intent` is the provider's
	// id for the money. A capture delivery carries BOTH — the session is the
	// object and the payment intent is a field on it. A refund delivery carries
	// only the second: a `charge.refunded` names a charge, whose `payment_intent`
	// is the payment, and it never mentions the session the customer paid
	// through. Each is read into the field that names what it is, and the
	// application resolves each against the column that holds it.
	var (
		checkoutRef string
		paymentRef  string
		amount      *int64
		currency    string
	)
	if providerKind == kindChargeRefunded {
		// The payment this charge belongs to, with the charge's own id as the
		// fallback for a charge that predates payment intents. There is no
		// checkout reference to read: the delivery does not carry one, and
		// inventing the session id here — even by looking it up — would be
		// putting a value into the message that the provider never signed.
		paymentRef, _ = rawString(object.PaymentIntent)
		if paymentRef == "" {
			paymentRef, _ = rawString(object.ID)
		}
		paymentRef = storableProviderText(paymentRef, maxStorableReferenceLength)
		amount = readableAmount(object.AmountRefunded)
	} else {
		// Every other delivery this build reads is about a Checkout Session.
		// The session's id is the reference this platform stored; its payment
		// intent is the money's own id, and it is what a later refund will
		// name — so it is read here, at the one moment the provider states
		// both, and stored by the application as the payment's economic
		// identity. It is absent only from a session that carries no payment
		// at all, which is a session this build never captures anyway.
		checkoutRef, _ = rawString(object.ID)
		checkoutRef = storableProviderText(checkoutRef, maxStorableReferenceLength)
		paymentRef, _ = rawString(object.PaymentIntent)
		paymentRef = storableProviderText(paymentRef, maxStorableReferenceLength)
		amount = readableAmount(object.AmountTotal)
	}
	if code, ok := rawString(object.Currency); ok {
		currency = storableCurrency(code)
	}

	return payments.ProviderEvent{
		// The port's vocabulary where this build has one, and the provider's own
		// string everywhere else — empty only if the provider left the type out
		// entirely, which the application treats as a kind it does not know.
		//
		// Bounded to what the ledger's column carries, and reduced to nothing
		// when the provider's name is longer: see storableProviderText. An
		// unrecordable kind must not become the storage layer's 5xx.
		Kind: storableProviderText(portKind(providerKind, object), maxStorableKindLength),
		// The delivery, not the payment. A provider emits several distinct
		// events for one payment, and keying a credit on this would fund that
		// payment once per event.
		EventID: eventID,
		// The checkout this platform opened, when the delivery names one.
		CheckoutRef: checkoutRef,
		// The payment the money moved for, when the delivery names one.
		PaymentRef: paymentRef,
		// The provider's amount in the unit the port's minor units are, or nil
		// when this build could not read one exactly. A nil is a refusal the
		// application makes, not a zero it might credit.
		AmountMinorUnits: amount,
		Currency:         currency,
		// The provider's own `created`, or the zero time when it stated none.
		OccurredAt: readableInstant(envelope.Created),
	}, nil
}

// portKind maps the provider's own kind onto the port's vocabulary, and passes
// every kind this build has not decided the meaning of through under the
// provider's own name.
//
// The two mapped kinds are the only events that can change what a customer is
// owed, and the mapping is what keeps the provider's spellings out of the
// application: the application switches on `captured` and `refunded` and
// quarantines everything else as an unknown kind — which is exactly the right
// treatment for a kind this build has not interpreted, an event that is known
// to be unusable rather than one that failed to arrive.
//
// The pass-through is the port's own contract ("Kind is the provider's own
// vocabulary, as a plain string") and it is NOT the same as ignoring the event:
// an unfamiliar kind still reaches the application with its id, its reference
// and whatever amount could be read.
func portKind(providerKind string, object eventObject) string {
	switch providerKind {
	case kindCheckoutCompleted:
		// The completed session is the one shape whose kind does NOT by itself
		// mean the money arrived. The provider emits it for
		// delayed-notification methods with payment_status "unpaid", and the
		// money follows — or does not — as an async payment event. Funding the
		// unpaid form would credit a customer whose transfer may still fail, so
		// only the paid form is this platform's capture; the unpaid form
		// crosses under the provider's own name and is recorded as a kind this
		// build does not act on, which is what it is.
		if status, ok := rawString(object.PaymentStatus); ok && status == "paid" {
			return payments.KindCaptured
		}
		return providerKind
	case kindCheckoutAsyncPaymentSucceeded:
		// The delayed notification that the money did arrive: the same economic
		// event as a paid completion, in a second spelling. It is mapped to the
		// same kind, and the pair cannot fund the payment twice — the
		// application resolves both to one payment, and the payment's own state
		// machine refuses the second delivery's effect (the status CAS runs
		// before the credit), recording it as a state conflict instead.
		return payments.KindCaptured
	case kindChargeRefunded:
		// Money went back to the customer. The port has a kind for it, so it is
		// recognised rather than passed through as noise: "we understood your
		// message" and "we could not read your message" are different things to
		// tell a provider.
		return payments.KindRefunded
	case kindCheckoutAsyncPaymentFailed, kindCheckoutExpired:
		// Recognised, and deliberately NOT mapped to either kind: a delayed
		// payment that failed and a session that expired both report that no
		// money is coming, this platform has made no claim about either, and
		// neither of the port's two kinds is true of them. They cross as the
		// provider's own events, recorded and left alone.
		return providerKind
	default:
		return providerKind
	}
}

// decodeObject reads the delivery's object, and reports an object this build
// cannot read as an empty one.
//
// The failure is swallowed deliberately: an unreadable object is a delivery
// with no reference, no amount and no currency, which the application
// quarantines as an unknown payment — and it is NOT a reason to discard the
// event id that has already been read, because that id is what lets the
// delivery be recorded at all.
func decodeObject(data *eventData) eventObject {
	var object eventObject
	if data == nil || len(data.Object) == 0 {
		return object
	}
	_ = json.Unmarshal(data.Object, &object)
	return object
}

// nullToken is the JSON null as its own bytes, so that a member the provider
// set to null and a member the provider omitted are one answer — no claim —
// without a fresh allocation of the literal on every read.
var nullToken = []byte("null")

// rawString reads a raw JSON token as the string it states, and reports whether
// it stated one.
//
// null and an absent member are one answer here — no claim — and any other JSON
// type is the same answer. A value of the wrong type is not coerced: coercion
// would be this adapter inventing a spelling the provider did not send, on
// fields that decide which payment a delivery is about and how much it is for.
func rawString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullToken) {
		return "", false
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", false
	}
	return value, true
}

// maxStorableReferenceLength and maxStorableKindLength are how much of the
// provider's own text this build can RECORD: the widths of provider_payment_ref
// and kind on control.payment_events, and of the matching columns on
// control.payment_quarantine.
//
// They are stated here, in the adapter that reads the provider's bytes, and not
// left to the schema, because the two layers answer differently and only one of
// their answers is acceptable. A column's answer to text it cannot hold is a
// constraint violation, which surfaces as a 5xx — a status that asks the
// provider to send the same bytes again, forever, for a delivery this build can
// never store. This adapter's answer is to report the value as one it could not
// read, which the application turns into a quarantine with an
// operator-actionable reason or a refusal to credit, and which the handler
// answers 2xx. Nothing is lost when it triggers: the delivery's raw bytes are
// kept as the quarantine row's evidence, which is where an operator reads what
// the provider actually said.
//
// Both are two orders of magnitude above what real providers mint, so neither
// bounds a delivery that exists. They bound the schema, and they exist so that
// a provider shipping a longer identifier changes what this plane records
// rather than taking its webhook endpoint down.
const (
	maxStorableReferenceLength = 255
	maxStorableKindLength      = 128
)

// storableProviderText returns the provider's own text when this build can
// record it, and the empty string when it cannot.
//
// Empty is this port's spelling of "not said" and it is the honest one here:
// the caller records the value or it does not have it, and a truncated
// identifier is a DIFFERENT identifier — one that looks like the right value in
// every log line, comparison and operator screen that reads it. Truncation is
// the one transformation that would turn "this build cannot read your message"
// into "this build read your message and it said something else", and that is
// unrecoverable.
//
// What an unreadable value means depends on which one it is, and every meaning
// is a refusal rather than a default: a payment reference this build cannot
// record is one it cannot resolve, so the delivery resolves to no payment and
// is quarantined as unknown; a kind it cannot record is one it has no rule for,
// so the delivery is quarantined as an unknown kind. Neither credits anything.
func storableProviderText(value string, max int) string {
	if len(value) > max {
		return ""
	}
	return value
}

// storableCurrency folds the provider's lowercase code to the uppercase ISO
// 4217 form the domain stores and compares, and reports the empty string when
// the result is not the three letters that form can hold.
//
// Folding a spelling is not the same act as converting an amount, which is why
// the case is repaired here; the shape is not, which is why it is refused. A
// code that is not three letters cannot be compared against a payment's
// currency in any way that means anything — and reporting it as unstated is
// what makes the application REFUSE the delivery (its currency comparison is
// against a payment whose currency is always three letters) rather than the
// storage layer answering the provider with a 5xx it will retry forever.
func storableCurrency(code string) string {
	folded := strings.ToUpper(code)
	if len(folded) != 3 {
		return ""
	}
	for _, r := range folded {
		if r < 'A' || r > 'Z' {
			return ""
		}
	}
	return folded
}

// readableAmount reads an amount exactly as the provider's integer encoding
// states it, and reports nothing when it states one this build cannot read.
//
// It refuses a decimal, an exponent, a quoted number, a null, an empty member
// and any other spelling that would need a conversion this package cannot make
// EXACTLY. The refusal is the point rather than a limitation: the port's
// AmountMinorUnits is nil on one, and a nil amount is what makes the
// application quarantine the delivery instead of crediting a figure this build
// guessed at. An amount of `12.5` truncated to `12` is a charge nobody agreed
// to, and it would be invisible in every log line that reported it.
func readableAmount(raw json.RawMessage) *int64 {
	amount, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil {
		return nil
	}
	return &amount
}

// readableInstant reads the provider's `created` member as the instant it
// states, and reports the zero time when it states none this build can read.
//
// A zero OccurredAt is a real answer and not an oversight. `created` is the
// provider's own claim about when it observed the outcome; a body that omits it
// makes no claim, and substituting the signature header's timestamp (when the
// delivery was SENT, a different fact) or this process's clock (which says
// nothing about the provider at all) would be this adapter composing a fact
// nobody stated.
func readableInstant(raw json.RawMessage) time.Time {
	seconds, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}
