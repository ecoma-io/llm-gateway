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
// SePay sends TWO headers where a card processor sends one, and both are
// required: the timestamp is not an element inside the signature header but a
// header of its own, and a verifier that read only the signature would have no
// idea which instant was signed. The scheme is
//
//	X-SePay-Signature: sha256={lowercase hexadecimal HMAC-SHA256}
//	X-SePay-Timestamp: {Unix time in SECONDS}
//
// where the signed message is `"{timestamp}.{rawBody}"` — the timestamp's
// decimal spelling, a ".", and the request body's exact bytes — keyed with the
// webhook's signing secret. The signature value carries a scheme prefix, so the
// hexadecimal digest is parsed out of the header rather than taken whole.
//
// THE PROVIDER'S OWN WINDOW IS 300 SECONDS. It states that a signature is valid
// for five minutes either side of the timestamp it carries, which is why the
// deployment's configured tolerance is refused above that figure at load: a
// tolerance this process would honour that the provider would not is a number
// that reads like a defence and is not one.
const (
	// signatureHeader is the header the delivery's signature arrives in.
	signatureHeader = "X-SePay-Signature"

	// timestampHeader is the header the signed instant arrives in.
	timestampHeader = "X-SePay-Timestamp"

	// signatureScheme is the prefix the signature's value carries. It is
	// required rather than stripped-if-present: the scheme is what says which
	// MAC the digest is, and a value that does not name one is a value this
	// build has not been told how to check.
	signatureScheme = "sha256"
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
	// signingSecret is the webhook's signing secret, held only to be used as an
	// HMAC key. It never reaches a log line or an error.
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

// NewVerifier returns the verifier described by a webhook's signing secret and
// the staleness bound. It panics on an empty secret or a non-positive tolerance
// for the reason New panics on an empty API token: both are wiring defects, and
// a verifier with either is a verifier that answers yes to every delivery or
// refuses every delivery.
//
// A tolerance above the provider's own 300-second window is refused for the
// same reason, and it is the one bound here that is the PROVIDER's rather than
// this process's: honouring a delivery the provider would consider expired is a
// decision this platform has no standing to make, and an operator who widened
// the window to paper over a clock fault would be widening the replay window
// for anybody who captured a delivery. Skew is fixed by fixing the clock.
func NewVerifier(signingSecret string, tolerance time.Duration) *Verifier {
	if signingSecret == "" {
		panic("paymentprovider: NewVerifier requires a webhook signing secret")
	}
	if tolerance <= 0 {
		panic("paymentprovider: NewVerifier requires a positive webhook tolerance")
	}
	if tolerance > maxProviderTolerance {
		panic("paymentprovider: the webhook tolerance must not exceed the provider's own 300-second window: a delivery this process would accept and the provider would not is not a delivery")
	}
	return &Verifier{signingSecret: signingSecret, tolerance: tolerance, now: time.Now}
}

// maxProviderTolerance is the window the provider's own documentation states as
// the validity of one of its signatures.
const maxProviderTolerance = 300 * time.Second

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
// The refusal order is deliberate: both headers, then the timestamp's spelling,
// then the signature's spelling, then the authenticity, then the freshness,
// then the content. An unauthenticated caller therefore learns nothing about
// this deployment's clock or its event vocabulary, and a delivery that fails
// freshness but not authentication is reported as stale rather than as
// unauthentic. The provider's window is symmetric, so a clock that runs fast is
// as acceptable as one that runs slow — see withinTolerance.
func (v *Verifier) Verify(headers map[string][]string, rawBody []byte) (payments.ProviderEvent, error) {
	signature, err := singleHeader(headers, signatureHeader)
	if err != nil {
		return payments.ProviderEvent{}, err
	}
	timestamp, err := singleHeader(headers, timestampHeader)
	if err != nil {
		return payments.ProviderEvent{}, err
	}

	// The timestamp is parsed BEFORE the MAC is computed, and the order is
	// harmless because the string that is signed is the one that arrived: a
	// caller that spelled the instant as something other than a whole number of
	// Unix seconds has sent a header this build cannot compare against a clock,
	// and computing a MAC over it first would only be a slower way to refuse it.
	// The RAW spelling is what goes into the MAC — see sign — so a leading zero
	// does not become a different string between verification and comparison.
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return payments.ProviderEvent{}, fmt.Errorf("%w: the %s header is not a Unix time in seconds", payments.ErrBadSignature, timestampHeader)
	}

	digest, err := parseSignature(signature)
	if err != nil {
		return payments.ProviderEvent{}, err
	}
	provided, err := hex.DecodeString(digest)
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
//
// With TWO required headers the hazard is the same and the stakes are higher
// than with one: a delivery carrying two timestamps and one signature could be
// verified against whichever timestamp the implementation happened to pick, so
// the pair a deployment accepted would depend on map iteration order.
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

// parseSignature takes the hexadecimal digest out of the signature header's
// value, requiring the scheme prefix that names which digest it is.
//
// The prefix is compared case-insensitively and the digest is not: a scheme
// spelled `SHA256=` is the same scheme and refusing it would be a refusal of a
// provider's typography rather than of its message, while the digest's case is
// the decoder's business — hex.DecodeString reads both — and folding it here
// would be this function inventing a normalisation of signed material. Only the
// digest is signed, so only the digest is compared, byte for byte, after
// decoding.
func parseSignature(header string) (string, error) {
	scheme, digest, ok := strings.Cut(header, "=")
	if !ok {
		return "", fmt.Errorf("%w: the signature header does not name a scheme, so this build has not been told which digest it carries", payments.ErrBadSignature)
	}
	if !strings.EqualFold(strings.TrimSpace(scheme), signatureScheme) {
		// The scheme is named rather than echoed. An unknown scheme is a
		// provider that has moved to something this build does not implement,
		// and echoing its spelling into an error line would be this package
		// repeating attacker-chosen text back into a log.
		return "", fmt.Errorf("%w: the signature header names a scheme this build does not verify", payments.ErrBadSignature)
	}
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return "", fmt.Errorf("%w: the signature header carries an empty digest", payments.ErrBadSignature)
	}
	return digest, nil
}

// sign computes the provider's signature over the timestamp's own spelling and
// the raw body: HMAC-SHA256(key = signing secret, message = "{timestamp}.{rawBody}").
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
// is fast. The provider's own window is symmetric too, so this matches the rule
// the signature is issued under rather than a stricter one of this build's
// invention.
func withinTolerance(signed, now time.Time, tolerance time.Duration) bool {
	skew := now.Sub(signed)
	return skew <= tolerance && skew >= -tolerance
}

// The provider's field values this adapter reads, spelled exactly as the
// provider spells them.
//
// There is ONE value here that changes what the platform does, and its smallness
// is the design rather than an early stage of it: this is not a list of
// everything the provider can send, it is the list of everything whose meaning
// this platform has decided IN ADVANCE. Every other value — including every
// value of transferType this build has not been told the meaning of — crosses
// to the application under the provider's own spelling and is quarantined there
// with a reason an operator can act on.
const (
	// transferTypeIn is the provider's word for money ARRIVING. It is the only
	// value that funds anything.
	transferTypeIn = "in"

	// transferTypeOut is the provider's word for money LEAVING the account.
	//
	// It is named here so that the one thing this build must never do with it
	// can be said in one place: it is NOT a refund. Reading it as one would be
	// the single most expensive mistake available in this file. The platform
	// recognises refunds so that a customer's money going back is recorded
	// rather than quarantined as noise, and it recognises them on a delivery
	// that says a PAYMENT was refunded. An outgoing transfer says only that
	// money left the merchant's account — it may be a payout to the operator's
	// own bank, a fee, a settlement sweep, or a transfer an operator made by
	// hand — and booking any of those against a customer's payment would attach
	// a real debit to whichever payment happened to match, moving a status and
	// writing a refund projection for money that was never returned. It
	// therefore crosses to the application under the provider's own name and is
	// recorded as a kind this build does not act on, which is what it is.
	transferTypeOut = "out"
)

// settlementCurrency is the currency this provider settles in, and therefore
// the currency its deliveries' amounts are stated in.
//
// The provider's webhook names no currency, and this constant is the answer to
// that rather than a default chosen here. SePay moves domestic Vietnamese bank
// transfers: there is exactly one currency an amount from it can be in, and a
// platform that accepted its deliveries has configured its offers in that
// currency or has configured a top-up nobody can pay. Reporting the empty
// string instead — the port's spelling of "the provider stated none" — would
// quarantine every delivery this integration will ever receive on the
// application's currency check, which is a working integration reported as a
// mismatch.
//
// It is a fact about the PROVIDER and it lives in the adapter for that reason.
// It is also not a rubber stamp: the application still compares it against the
// currency the payment was denominated in, so a deployment that priced an offer
// in anything else is refused at the delivery rather than credited as though
// the figures agreed — which is where a currency mistake belongs, one layer
// before money moves.
const settlementCurrency = "VND"

// eventEnvelope is the wire shape of a provider delivery, as much of it as this
// build reads: the transaction's identity, the destination the money arrived
// at, which way it moved, and for how much.
//
// Every field is a RAW JSON TOKEN rather than a typed value, and that is the
// one decision this file's tolerance rests on. With typed fields, a single
// unreadable field fails the decode of the WHOLE envelope: a body whose
// `transferAmount` is spelled `"5000000"`, or as a decimal, or turned into an
// object by a schema change, would take the event id down with it — and the
// event id is the one thing the caller needs to record the delivery at all.
// Reading each field as its own token means a field this build cannot read
// costs exactly that field: the port reports it as an ABSENT value and the
// APPLICATION quarantines the delivery with a reason a human resolves
// (amount_mismatch, unknown_payment, currency_mismatch). The port's
// ErrMalformedEvent comment carries the argument in full — "we could not read
// your message" and "we read it and could not use it" are different things to
// tell a provider, and only the first of them is this function's refusal.
//
// The provider's own `transactionDate` and `accountNumber` are deliberately NOT
// read. The first carries no timezone and would have to be given one to become
// an instant, which is this adapter inventing a fact; the second is the payer's
// or the merchant's account depending on a reading this build has not verified,
// and it is evidence rather than a key — nothing here would compare it against
// anything. The port's OccurredAt is therefore the zero time on every delivery
// this provider sends, which is the port's own spelling of "the provider stated
// none this build can place".
//
// Neither field is a LOSS, and it is worth being exact about where they end up,
// because "we do not read it" and "we keep it anyway" are different claims. What
// this build keeps is the authenticated BODY, and the port keeps that only on a
// delivery it could not apply: an applied delivery needs no evidence, because
// the ledger leg and the payment's own state are the evidence, while a
// quarantined one is the case where an operator has to answer "what did the
// provider actually send". So an operator reading a quarantine sees these two
// fields inside the raw bytes, unparsed and compared against nothing; an applied
// delivery's bytes are not stored at all, and the fields in them were never read
// on the way past.
type eventEnvelope struct {
	// ID is the transaction's identity, and the provider repeats it on every
	// redelivery of this transaction — which is what makes it the dedup key.
	// The provider spells it as an integer, so the reader below accepts the
	// number's own decimal spelling as well as a string.
	ID json.RawMessage `json:"id"`

	// SubAccount is the virtual account the money arrived at: the destination
	// this platform asked the provider for, echoed back. It is the reference a
	// capture resolves by, and it is an account the provider issued for one
	// payment rather than a memo a customer typed, which is the whole reason
	// this integration is built on it.
	SubAccount json.RawMessage `json:"subAccount"`

	// TransferType says which way the money moved: arriving, or leaving.
	TransferType json.RawMessage `json:"transferType"`

	// TransferAmount is the transaction's amount, in the settlement currency's
	// smallest unit — which for a currency with no minor unit is the amount
	// itself. It is read as an exact integer or reported as absent, never
	// rounded.
	TransferAmount json.RawMessage `json:"transferAmount"`
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
// everything this build does not understand. A transfer moving in a direction
// this build has no rule for crosses under the provider's own name; an amount
// whose spelling cannot be converted exactly is reported as an ABSENT amount
// rather than a rounded one, which is the one way an amount can go quietly
// wrong; a delivery naming no destination is reported with no reference. Each of
// those becomes a quarantine the application writes with an operator-actionable
// reason, and each is a better outcome than a 2xx that recorded nothing while
// the provider's real delivery went unseen.
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

	eventID, ok := rawIdentifier(envelope.ID)
	if !ok || eventID == "" {
		// The delivery's own identity, and the idempotency key for THIS
		// delivery. Absent, empty, or spelled as something this build cannot
		// read are one answer: this body cannot be named, so the caller writes
		// nothing and answers 2xx rather than keying a row on an identity it
		// made up.
		return payments.ProviderEvent{}, fmt.Errorf("%w: the body carries no delivery id, so the caller has no key to record it under", payments.ErrMalformedEvent)
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
		return payments.ProviderEvent{}, fmt.Errorf("%w: the body's delivery id is %d characters and this build records at most %d, so the caller has no key to record it under",
			payments.ErrMalformedEvent, len(eventID), maxProviderEventIDLength)
	}

	// TWO REFERENCES COME OUT OF THIS BODY and only one of them is a destination.
	// The virtual account is what the money arrived at and is what a capture
	// resolves by; the transaction's own id is the economic event, and it is
	// what the funding leg's command key is derived from — two deliveries of one
	// transaction therefore derive one key, and a second transaction into the
	// same account derives another, which is exactly the distinction the ledger
	// needs. They are separate fields for the reason the port states: the
	// application resolves them against different stored columns, and a refund,
	// were this provider to report one, would name the second and never the
	// first.
	transferRef, _ := rawString(envelope.SubAccount)
	transferRef = storableProviderText(transferRef, maxStorableReferenceLength)

	return payments.ProviderEvent{
		// The port's vocabulary where this build has one, and the provider's own
		// value everywhere else — empty only if the provider left the direction
		// out entirely, which the application treats as a kind it does not know.
		//
		// Bounded to what the ledger's column carries, and reduced to nothing
		// when the provider's value is longer: see storableProviderText. An
		// unrecordable kind must not become the storage layer's 5xx.
		Kind: storableProviderText(portKind(rawStringOrEmpty(envelope.TransferType)), maxStorableKindLength),
		// The delivery, not the payment. The provider repeats it on a
		// redelivery, and keying the dedup record on it is what makes the second
		// delivery a duplicate rather than a second credit.
		EventID: eventID,
		// The destination the provider issued for this platform's payment, when
		// the delivery names one.
		TransferRef: transferRef,
		// The transaction's own id — the economic event — carried under the
		// identifier the port reserves for it.
		PaymentRef: storableProviderText(eventID, maxStorableReferenceLength),
		// The provider's amount in the unit the port's minor units are, or nil
		// when this build could not read one exactly. A nil is a refusal the
		// application makes, not a zero it might credit.
		AmountMinorUnits: readableAmount(envelope.TransferAmount),
		// The provider names no currency on a delivery, and this provider
		// settles in exactly one. See settlementCurrency.
		Currency: settlementCurrency,
		// The provider states no instant this build can place. See eventEnvelope.
		OccurredAt: time.Time{},
	}, nil
}

// portKind maps the provider's own direction onto the port's vocabulary, and
// passes every value this build has not decided the meaning of through under
// the provider's own spelling.
//
// ONE mapping is made here and the reason it is the only one is the reason this
// function is worth reading. Money arriving is the capture — that is what the
// port's KindCaptured means, and it is the only kind that funds anything. Money
// leaving is NOT the port's KindRefunded, however much it looks like one from a
// distance: see transferTypeOut for what reading it that way would cost. Every
// other value, including the empty one, crosses unchanged and is quarantined by
// the application as a kind this build does not act on, which is exactly the
// right treatment for an event that is known to be unusable rather than one that
// failed to arrive.
//
// The pass-through is the port's own contract ("Kind is the provider's own
// vocabulary, as a plain string") and it is NOT the same as ignoring the event:
// an unfamiliar kind still reaches the application with its id, its reference
// and whatever amount could be read.
func portKind(transferType string) string {
	switch transferType {
	case transferTypeIn:
		return payments.KindCaptured
	default:
		return transferType
	}
}

// rawStringOrEmpty reads a raw token as a string, reporting the empty string
// for a member that states something this build cannot read.
//
// It exists for the ONE call site where the empty spelling is the honest
// answer rather than a value to refuse: an unreadable transferType is a
// direction this build has no rule for, which is the same answer the absent
// member gets and is reported to the application as an unknown kind.
func rawStringOrEmpty(raw json.RawMessage) string {
	value, _ := rawString(raw)
	return value
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

// rawIdentifier reads a raw JSON token that NAMES something — a delivery id, a
// transaction id — as the text this build will record it under, and reports
// whether it could read one.
//
// It accepts the two spellings a provider actually uses for an identifier: a
// JSON string, and a JSON number. The number case is not a coercion, and the
// distinction is worth being exact about, because the same file refuses to
// coerce values elsewhere. Coercing a value would be inventing a spelling the
// provider did not send — reading `12.5` as a string, or a boolean as anything
// at all. An integer's decimal spelling is what the provider's bytes SAY: the
// token `92704` states the text "92704" exactly, and returning it is reading
// the body rather than interpreting it. Everything else — a decimal, an
// exponent, a boolean, an object, an array — is refused, because none of them
// names a thing this build can key a record on.
//
// Whether a string and a number spelling of the same identifier are the SAME
// delivery is a question this function deliberately does not answer: they are
// two byte strings, and a provider that changed its spelling mid-flight would
// record two deliveries under two ids — which the payment's own state machine
// refuses to turn into two credits, exactly as it refuses any second delivery
// of one transaction. Fabricating sameness here, by parsing the number and
// re-rendering it, would be this adapter deciding that two different tokens are
// one identity, which is a claim about the provider's behaviour rather than a
// reading of its bytes.
func rawIdentifier(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullToken) {
		return "", false
	}
	if trimmed[0] == '"' {
		return rawString(trimmed)
	}
	for i, b := range trimmed {
		if i == 0 && b == '-' {
			continue
		}
		if b < '0' || b > '9' {
			return "", false
		}
	}
	return string(trimmed), true
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
// is a refusal rather than a default: a destination this build cannot record is
// one it cannot resolve, so the delivery resolves to no payment and is
// quarantined as unknown; a kind it cannot record is one it has no rule for, so
// the delivery is quarantined as an unknown kind. Neither credits anything.
//
// The bound is in BYTES where the columns it mirrors are bounded in CHARACTERS,
// and the strictness is deliberate: a byte count is never smaller than a rune
// count, so this can only ever refuse a value the column would have held — and
// refusing is a failure an operator reads, while the alternative, measuring in
// runes and being wrong about the encoding, is the truncation this function
// exists to prevent. The values it is applied to are account and transaction
// identifiers, which are ASCII in every documented case.
func storableProviderText(value string, max int) string {
	if len(value) > max {
		return ""
	}
	return value
}

// readableAmount reads an amount exactly as the provider's integer encoding
// states it, and reports nothing when it states one this build cannot read.
//
// It refuses a decimal, an exponent, a quoted number, a null, an empty member
// and any other spelling that would need a conversion this package cannot make
// EXACTLY. The refusal is the point rather than a limitation: the port's
// AmountMinorUnits is nil on one, and a nil amount is what makes the
// application quarantine the delivery instead of crediting a figure this build
// guessed at. An amount of `12.5` truncated to `12` is a payment nobody agreed
// to, and it would be invisible in every log line that reported it.
//
// A QUOTED integer is refused too, and it is the refusal most likely to be
// argued with, because `"5000000"` plainly means five million. It is refused
// because the port's amount is a number and a provider that quotes one has
// changed the type of a money field — and the day a quoted amount arrives it
// may arrive with a separator, a currency word, or a decimal, and the reading
// that accepts the quotes today is the reading that has to decide what to do
// with those tomorrow. Refusing costs a quarantine an operator resolves; the
// tolerant reading costs a guess about money.
func readableAmount(raw json.RawMessage) *int64 {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullToken) {
		return nil
	}
	if trimmed[0] == '"' {
		return nil
	}
	amount, err := strconv.ParseInt(string(trimmed), 10, 64)
	if err != nil {
		return nil
	}
	return &amount
}
