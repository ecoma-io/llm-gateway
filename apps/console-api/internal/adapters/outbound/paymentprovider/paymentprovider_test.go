package paymentprovider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	payments "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/payments"
)

// testSigningSecret and testAPIKey are the two credentials the fixtures use.
//
// Both are ASSEMBLED from fragments and neither is shaped like the provider's
// own credentials. This repository is scanned by gitleaks and pushed through
// GitHub's push protection, and both refuse a secret-shaped string — correctly,
// because a fixture needs to be a string the code treats as a credential and
// never a value that could be mistaken for a live one. The values here are
// low-entropy repeats for the same reason.
func testSigningSecret() string { return "sig-" + strings.Repeat("w", 32) }

func testAPIKey() string { return "console-api-" + strings.Repeat("k", 32) }

// newTLSServer stands up the fake provider over TLS, and the scheme is not
// incidental: `New` refuses an `http` base URL because every request it makes
// carries the deployment's API secret as a bearer credential, so a test that
// pointed a client at a cleartext `httptest.NewServer` would be asserting against
// a wiring the constructor deliberately refuses to accept. Running the fake
// provider over TLS lets these tests exercise the real request path — the
// form encoding, the Authorization header, the status handling — against a
// client the production composition root could actually construct.
func newTLSServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	return server
}

// The fake provider's own URL is already the `https://` one `New` requires —
// httptest's StartTLS sets it — so the fixtures hand `server.URL` straight over.
// What they have to add is the trust store, and trustServerTLS is the only
// place in this package that does.
//
// trustServerTLS points a client at httptest's self-signed certificate, and it
// MUTATES the transport on the client `New` built rather than replacing the
// client.
//
// The mutation is the whole point. `New` sets a Timeout and a CheckRedirect that
// refuses to follow a payment redirect, and a fixture that swapped in a fresh
// &http.Client{} would silently drop the second — which is precisely what
// happened the first time this file was written: the redirect test started
// failing because the client had started following redirects, and the only
// correct fix was in the fixture. A test helper that can quietly remove the
// behaviour it was not asked to test is a defect in the helper.
func trustServerTLS(t *testing.T, server *httptest.Server, client *Client) {
	t.Helper()
	client.httpClient.Transport = server.Client().Transport
}

// testNow is the instant every verifier in this file believes it is. Staleness
// is a comparison against a clock, and a test that read the real one would be
// asserting how long the machine took to get there.
var testNow = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func testVerifier() *Verifier {
	verifier := NewVerifier(testSigningSecret(), 5*time.Minute)
	verifier.now = func() time.Time { return testNow }
	return verifier
}

// signFixture computes the provider's signature the way the provider's
// documentation states it: HMAC-SHA256 over the string "{t}.{rawBody}", keyed
// with the signing secret's own bytes.
//
// It is written here as an INDEPENDENT construction rather than by calling the
// package's own sign, and the difference is the whole value of the fixture: a
// test that signed through the same function it verifies with could not see a
// wrong separator, a wrong key, or a body that was re-encoded on its way into
// the MAC. This one spells out the concatenation in a single string.
func signFixture(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + string(body)))
	return hex.EncodeToString(mac.Sum(nil))
}

// signedHeaders is the one signature header a correctly signed delivery
// carries, stamped at the given instant.
func signedHeaders(t *testing.T, signedAt time.Time, body []byte) map[string][]string {
	t.Helper()
	stamp := strconv.FormatInt(signedAt.Unix(), 10)
	return map[string][]string{
		signatureHeader: {"t=" + stamp + ",v1=" + signFixture(testSigningSecret(), stamp, body)},
	}
}

// withSignatureHeader replaces the signature header of a header map, keeping
// every other key, so a case can build a malformed header without repeating the
// whole map.
func withSignatureHeader(headers map[string][]string, values ...string) map[string][]string {
	headers[signatureHeader] = values
	return headers
}

// eventBody renders a session delivery of the given kind as the provider would
// send it, with the fields this build reads. An entry in object whose value is
// nil REMOVES that field, which is how a case states "the provider sent no
// amount" rather than "the provider sent a zero".
func eventBody(t *testing.T, kind string, object map[string]any) []byte {
	t.Helper()
	return envelopeJSON(t, kind, overrides(map[string]any{
		"id":             "cs_test_session_reference",
		"amount_total":   int64(2500),
		"currency":       "usd",
		"payment_status": "paid",
	}, object))
}

// chargeEventBody renders the other shape this adapter reads: a CHARGE object
// rather than a checkout session, which is what the provider's refund deliveries
// carry. Its fields are the refund's own — the payment intent it belongs to, and
// how much of the charge has gone back.
func chargeEventBody(t *testing.T, kind string, object map[string]any) []byte {
	t.Helper()
	return envelopeJSON(t, kind, overrides(map[string]any{
		"id":              "ch_test_charge_reference",
		"payment_intent":  "pi_test_payment_reference",
		"amount_refunded": int64(2500),
		"currency":        "usd",
	}, object))
}

// overrides applies a case's fields to a set of defaults: a value of nil means
// "the provider did not send this member", which is a different sentence from
// "the provider sent a zero".
func overrides(defaults, object map[string]any) map[string]any {
	fields := make(map[string]any, len(defaults)+len(object))
	for key, value := range defaults {
		fields[key] = value
	}
	for key, value := range object {
		if value == nil {
			delete(fields, key)
			continue
		}
		fields[key] = value
	}
	return fields
}

// envelopeJSON wraps an object in the provider's event envelope.
func envelopeJSON(t *testing.T, kind string, fields map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id":      "evt_test_delivery",
		"type":    kind,
		"created": int64(1735689600),
		"data":    map[string]any{"object": fields},
	})
	if err != nil {
		t.Fatalf("rendering the fixture: %v", err)
	}
	return raw
}

// amount is a pointer to an int64 for a table's expectations: the port's money
// fields are pointers precisely so that an absent amount and an amount of zero
// are different sentences, and a table that could not state which of the two it
// expects would not be testing that.
func amount(minorUnits int64) *int64 { return &minorUnits }

func TestVerifyAcceptsACorrectlySignedDelivery(t *testing.T) {
	// The session names the payment it was paid through, which is the shape a
	// real completion arrives in and the reason the port has two reference
	// fields rather than one: the checkout reference is what this platform
	// stored and seeks by, and the payment reference is what a later refund
	// delivery will name instead.
	body := eventBody(t, kindCheckoutCompleted, map[string]any{"payment_intent": "pi_test_payment_reference"})

	event, err := testVerifier().Verify(signedHeaders(t, testNow, body), body)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	// The provider's `checkout.session.completed` with payment_status paid is
	// this platform's capture, and the MAPPING is what the application above is
	// written against: it switches on the port's own vocabulary, so the
	// provider's spelling must not reach it.
	if event.Kind != payments.KindCaptured {
		t.Errorf("Kind = %q, want %q — a paid completion is this platform's capture", event.Kind, payments.KindCaptured)
	}
	if event.EventID != "evt_test_delivery" {
		t.Errorf("EventID = %q, want the delivery's own id", event.EventID)
	}
	if event.CheckoutRef != "cs_test_session_reference" {
		t.Errorf("CheckoutRef = %q, want the session reference the platform stored", event.CheckoutRef)
	}
	if event.PaymentRef != "pi_test_payment_reference" {
		t.Errorf("PaymentRef = %q, want the payment the session was paid through", event.PaymentRef)
	}
	if event.AmountMinorUnits == nil || *event.AmountMinorUnits != 2500 {
		t.Errorf("AmountMinorUnits = %v, want 2500", event.AmountMinorUnits)
	}
	if event.Currency != "USD" {
		t.Errorf("Currency = %q, want the uppercase ISO 4217 form the domain stores", event.Currency)
	}
	if want := time.Unix(1735689600, 0).UTC(); !event.OccurredAt.Equal(want) {
		t.Errorf("OccurredAt = %s, want %s", event.OccurredAt, want)
	}
}

// TestVerifySignsTheRawBytesAndNotTheirReSerialisation is the property the port
// calls its most important one, proved rather than asserted: the fixture is
// valid JSON with insignificant whitespace and a key order no encoder would
// choose, and the signature is computed over those exact octets.
func TestVerifySignsTheRawBytesAndNotTheirReSerialisation(t *testing.T) {
	const raw = `{
  "data": {"object": {"payment_status": "paid",

      "amount_total": 2500, "id": "cs_test_session_reference",
      "currency": "usd"}},
  "type": "checkout.session.completed",
  "id": "evt_test_delivery",
  "created": 1735689600
}`
	body := []byte(raw)

	// The same JSON VALUE, in different bytes. Decoding and re-encoding is
	// exactly what a verifier that parsed the body before checking the
	// signature would be signing.
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	reserialised, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-serialising the fixture: %v", err)
	}
	if bytes.Equal(reserialised, body) {
		t.Fatal("the fixture must not already be in the encoder's canonical form, or this test proves nothing")
	}

	// The provider's own bytes, signed as they arrived, verify.
	if _, err := testVerifier().Verify(signedHeaders(t, testNow, body), body); err != nil {
		t.Fatalf("Verify() over the raw bytes = %v, want the delivery accepted", err)
	}

	// The same delivery, signed over the re-serialised form, does not. A
	// verifier that decoded first would accept this one — and would accept a
	// forged body whose only difference from a real one was its whitespace.
	if _, err := testVerifier().Verify(signedHeaders(t, testNow, reserialised), body); !errors.Is(err, payments.ErrBadSignature) {
		t.Fatalf("Verify() over the raw bytes with a signature of the re-serialised form = %v, want %v", err, payments.ErrBadSignature)
	}
}

func TestVerifyRefusesADeliveryThatIsNotSignedByOurSecret(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string][]string
		body    []byte
	}{
		{
			name:    "a missing signature header",
			headers: map[string][]string{},
			body:    eventBody(t, kindCheckoutCompleted, nil),
		},
		{
			// Multiplicity is the hazard the port preserves the headers for:
			// an implementation that took "the first of several" would let a
			// request carry one signature this deployment accepts and one it
			// does not.
			name: "a repeated signature header",
			headers: func() map[string][]string {
				body := eventBody(t, kindCheckoutCompleted, nil)
				header := signedHeaders(t, testNow, body)[signatureHeader][0]
				return map[string][]string{signatureHeader: {header, header + "x"}}
			}(),
			body: eventBody(t, kindCheckoutCompleted, nil),
		},
		{
			// The same hazard, stated the way a map can also state it: one
			// value under two spellings of the same name. net/http lowercases
			// keys on the way in, so this shape cannot arrive from the wire
			// today — which is exactly why the lookup is written to refuse it
			// rather than to depend on that.
			name: "one signature under two spellings of the header name",
			headers: func() map[string][]string {
				body := eventBody(t, kindCheckoutCompleted, nil)
				header := signedHeaders(t, testNow, body)[signatureHeader][0]
				return map[string][]string{"stripe-signature": {header}, "Stripe-Signature": {header}}
			}(),
			body: eventBody(t, kindCheckoutCompleted, nil),
		},
		{
			name:    "a tampered body",
			headers: signedHeaders(t, testNow, eventBody(t, kindCheckoutCompleted, nil)),
			// One extra digit on the amount, and nothing else: the delivery
			// this build would otherwise fund is a different figure.
			body: eventBody(t, kindCheckoutCompleted, map[string]any{"amount_total": int64(25000)}),
		},
		{
			name:    "a signature made with another secret",
			headers: map[string][]string{signatureHeader: {"t=" + strconv.FormatInt(testNow.Unix(), 10) + ",v1=" + signFixture("not-our-secret", strconv.FormatInt(testNow.Unix(), 10), eventBody(t, kindCheckoutCompleted, nil))}},
			body:    eventBody(t, kindCheckoutCompleted, nil),
		},
		{
			name:    "an empty signature header",
			headers: withSignatureHeader(signedHeaders(t, testNow, eventBody(t, kindCheckoutCompleted, nil)), ""),
			body:    eventBody(t, kindCheckoutCompleted, nil),
		},
		{
			name: "a header carrying no timestamp",
			headers: func() map[string][]string {
				body := eventBody(t, kindCheckoutCompleted, nil)
				return map[string][]string{signatureHeader: {"v1=" + signFixture(testSigningSecret(), "", body)}}
			}(),
			body: eventBody(t, kindCheckoutCompleted, nil),
		},
		{
			name: "a header carrying no v1 signature",
			headers: map[string][]string{
				signatureHeader: {"t=" + strconv.FormatInt(testNow.Unix(), 10)},
			},
			body: eventBody(t, kindCheckoutCompleted, nil),
		},
		{
			// The provider documents that two v1 values appear during a secret
			// rotation. Accepting either is the smuggling shape, so the second
			// one is refused rather than matched.
			name: "a header carrying two v1 signatures",
			headers: func() map[string][]string {
				body := eventBody(t, kindCheckoutCompleted, nil)
				stamp := strconv.FormatInt(testNow.Unix(), 10)
				signature := signFixture(testSigningSecret(), stamp, body)
				return map[string][]string{signatureHeader: {"t=" + stamp + ",v1=" + signature + ",v1=" + signature}}
			}(),
			body: eventBody(t, kindCheckoutCompleted, nil),
		},
		{
			name: "a header carrying a signature that is not hexadecimal",
			headers: map[string][]string{
				signatureHeader: {"t=" + strconv.FormatInt(testNow.Unix(), 10) + ",v1=zzzz"},
			},
			body: eventBody(t, kindCheckoutCompleted, nil),
		},
		{
			name: "a header carrying an element that is not a pair",
			headers: map[string][]string{
				signatureHeader: {"t=" + strconv.FormatInt(testNow.Unix(), 10) + ",garbage"},
			},
			body: eventBody(t, kindCheckoutCompleted, nil),
		},
		{
			name: "a header whose timestamp is not a number",
			headers: map[string][]string{
				signatureHeader: {"t=sooner,v1=" + signFixture(testSigningSecret(), "sooner", eventBody(t, kindCheckoutCompleted, nil))},
			},
			body: eventBody(t, kindCheckoutCompleted, nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := testVerifier().Verify(tt.headers, tt.body)
			if !errors.Is(err, payments.ErrBadSignature) {
				t.Fatalf("Verify() error = %v, want %v", err, payments.ErrBadSignature)
			}
		})
	}
}

// TestVerifyRefusesOnlyABodyItCannotName covers the port's second sentinel, and
// the narrowness of it is the whole test: ErrMalformedEvent means "I could not
// read an event id out of these bytes", which is the one case where the caller
// can record NOTHING and answers 2xx. Every other refusal this adapter makes is
// a populated event with no error, and the test below is that half.
//
// The reason the id is the boundary: the application records a delivery under
// (provider, provider account, event id), so a row keyed on an id this adapter
// invented would swallow the NEXT unreadable delivery as a duplicate of this
// one — reporting a real event as already-seen, which is worse than reporting
// nothing.
func TestVerifyRefusesOnlyABodyItCannotName(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "a body that is not JSON",
			body: []byte("this is not an event"),
		},
		{
			// Valid JSON, and not an object: there is no envelope to read an id
			// out of.
			name: "a body that is a JSON string",
			body: []byte(`"checkout.session.completed"`),
		},
		{
			name: "a body with no event id",
			body: []byte(`{"type":"checkout.session.completed","data":{"object":{"id":"cs_test_session_reference"}}}`),
		},
		{
			name: "a body whose event id is empty",
			body: []byte(`{"id":"","type":"checkout.session.completed","data":{"object":{"id":"cs_test_session_reference"}}}`),
		},
		{
			name: "a body whose event id is null",
			body: []byte(`{"id":null,"type":"checkout.session.completed"}`),
		},
		{
			// A number where the provider's contract says a string is not a
			// string to coerce: it is an id this build cannot read, and a
			// coerced one would be an id the provider never sent.
			name: "a body whose event id is a number",
			body: []byte(`{"id":42,"type":"checkout.session.completed"}`),
		},
		{
			// Longer than the column the id is written to. This is the same
			// answer as an absent id rather than a fourth outcome, and the
			// alternative is what this case exists to prevent: the id is
			// recorded first and unconditionally, so an unrecordable one would
			// reach a bounded column, raise a constraint violation, and surface
			// as a 5xx — a provider retrying forever against a delivery this
			// build can never store.
			name: "a body whose event id is longer than this build can record",
			body: []byte(`{"id":"` + strings.Repeat("e", maxProviderEventIDLength+1) + `","type":"checkout.session.completed"}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, err := testVerifier().Verify(signedHeaders(t, testNow, tt.body), tt.body)
			if !errors.Is(err, payments.ErrMalformedEvent) {
				t.Fatalf("Verify() error = %v, want %v", err, payments.ErrMalformedEvent)
			}
			if event != (payments.ProviderEvent{}) {
				t.Errorf("Verify() event = %+v, want the zero event alongside the refusal: a delivery the caller cannot record is not one it may act on", event)
			}
		})
	}
}

// TestVerifyReadsEveryDeliveryItCanNameAndLeavesTheRefusalToTheApplication is
// the other half of that contract, and it is the one that took a decision: an
// event this build cannot USE is not an event it cannot READ.
//
// Each case below is a well-signed delivery that some earlier shape of this
// adapter would have refused with ErrMalformedEvent, and each is now returned as
// a populated event — with the provider's own kind where no port kind is true of
// it, with a nil amount where the amount could not be read exactly, and with an
// empty reference where the delivery names no object. The application records
// each one under its real event id and quarantines it with a reason an operator
// resolves (unknown_kind, amount_mismatch, unknown_payment); a 5xx or a silent
// 2xx here would instead be this adapter deciding something it cannot know.
func TestVerifyReadsEveryDeliveryItCanNameAndLeavesTheRefusalToTheApplication(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		// wantKind is the port's kind where the adapter maps one, and the
		// provider's own spelling everywhere else.
		wantKind        string
		wantCheckoutRef string
		// wantPaymentRef is the second reference a delivery may name — the id
		// of the money rather than of the checkout it was taken through. It is
		// empty wherever the body names no payment intent, which is the
		// ordinary shape of a session-object delivery.
		wantPaymentRef string
		wantAmount     *int64
		wantCurrency   string
		// wantNoTimestamp states that the body made no claim about when the
		// provider observed the outcome, and the port reports exactly that.
		wantNoTimestamp bool
	}{
		{
			// A real delivery about real money that names a payment intent this
			// platform never stored. It crosses under the provider's own name;
			// the application records it and quarantines it as a kind this build
			// does not act on.
			name:            "an event kind this build does not interpret",
			body:            eventBody(t, "payment_intent.succeeded", nil),
			wantKind:        "payment_intent.succeeded",
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      amount(2500),
			wantCurrency:    "USD",
		},
		{
			// The delayed-notification shape: the session completed and the
			// money has not arrived. Mapping it to a capture would credit a
			// customer whose transfer may still fail, so it crosses as the
			// provider's own kind rather than as either of the port's two.
			name:            "a completed session that is not paid",
			body:            eventBody(t, kindCheckoutCompleted, map[string]any{"payment_status": "unpaid"}),
			wantKind:        kindCheckoutCompleted,
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      amount(2500),
			wantCurrency:    "USD",
		},
		{
			name:            "a delayed payment that failed",
			body:            eventBody(t, kindCheckoutAsyncPaymentFailed, nil),
			wantKind:        kindCheckoutAsyncPaymentFailed,
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      amount(2500),
			wantCurrency:    "USD",
		},
		{
			name:            "a session that expired",
			body:            eventBody(t, kindCheckoutExpired, nil),
			wantKind:        kindCheckoutExpired,
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      amount(2500),
			wantCurrency:    "USD",
		},
		{
			// The delayed success IS this platform's capture: the money arrived,
			// and it is the same economic event as a paid completion.
			name:            "a delayed payment that succeeded",
			body:            eventBody(t, kindCheckoutAsyncPaymentSucceeded, nil),
			wantKind:        payments.KindCaptured,
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      amount(2500),
			wantCurrency:    "USD",
		},
		{
			// An absent amount is not a zero, and it is not a refusal either:
			// the port reports no amount and the domain refuses the capture.
			name:            "an absent amount",
			body:            eventBody(t, kindCheckoutCompleted, map[string]any{"amount_total": nil}),
			wantKind:        payments.KindCaptured,
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      nil,
			wantCurrency:    "USD",
		},
		{
			// The shape a provider changes an integer into. Truncating 12.5 to
			// 12 is a charge nobody agreed to, so the amount is reported as
			// ABSENT and the application quarantines it as a mismatch.
			name: "an amount that is not an exact integer",
			body: []byte(`{"id":"evt_test_delivery","type":"checkout.session.completed","created":1735689600,` +
				`"data":{"object":{"id":"cs_test_session_reference","amount_total":12.5,"currency":"usd","payment_status":"paid"}}}`),
			wantKind:        payments.KindCaptured,
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      nil,
			wantCurrency:    "USD",
		},
		{
			name: "an amount spelled as a string",
			body: []byte(`{"id":"evt_test_delivery","type":"checkout.session.completed","created":1735689600,` +
				`"data":{"object":{"id":"cs_test_session_reference","amount_total":"2500","currency":"usd","payment_status":"paid"}}}`),
			wantKind:        payments.KindCaptured,
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      nil,
			wantCurrency:    "USD",
		},
		{
			// Zero IS readable — it is an integer — and it crosses as a zero
			// rather than as an absence, because the two are different sentences
			// and the domain refuses the zero on its own terms.
			name:            "an amount of zero",
			body:            eventBody(t, kindCheckoutCompleted, map[string]any{"amount_total": int64(0)}),
			wantKind:        payments.KindCaptured,
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      amount(0),
			wantCurrency:    "USD",
		},
		{
			name:            "an absent currency",
			body:            eventBody(t, kindCheckoutCompleted, map[string]any{"currency": nil}),
			wantKind:        payments.KindCaptured,
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      amount(2500),
			wantCurrency:    "",
		},
		{
			// No object at all: the delivery names no payment this platform can
			// resolve, which the application quarantines as an unknown payment
			// rather than answering 5xx for. The kind is the provider's own
			// because a completion is only this platform's capture when the
			// object says the money arrived — an object this build cannot read
			// states no payment_status, so the capture reading is not available.
			name:            "no object",
			body:            envelopeJSON(t, kindCheckoutCompleted, nil),
			wantKind:        kindCheckoutCompleted,
			wantCheckoutRef: "",
			wantAmount:      nil,
			wantCurrency:    "",
		},
		{
			name:            "an absent session reference",
			body:            eventBody(t, kindCheckoutCompleted, map[string]any{"id": nil}),
			wantKind:        payments.KindCaptured,
			wantCheckoutRef: "",
			wantAmount:      amount(2500),
			wantCurrency:    "USD",
		},
		{
			// No type at all: the port's zero-kind event is not valid, and the
			// application reads an empty kind as one it does not know — a
			// quarantine rather than a refusal, because the delivery still has an
			// id the caller can record it under.
			name:            "no event type",
			body:            []byte(`{"id":"evt_test_delivery","created":1735689600,"data":{"object":{"id":"cs_test_session_reference","amount_total":2500,"currency":"usd"}}}`),
			wantKind:        "",
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      amount(2500),
			wantCurrency:    "USD",
		},
		{
			// The three ways provider text this build cannot STORE is reported
			// as text it could not read. Each of these would otherwise reach a
			// bounded column and come back as a constraint violation, which the
			// provider reads as a 5xx and retries forever — so each is reduced
			// to the port's spelling of "not said", and the application answers
			// it with a quarantine (unknown kind, unknown payment, currency
			// mismatch) rather than with a page.
			//
			// The value is reported as absent rather than TRUNCATED, and that is
			// the whole point: a truncated identifier is a different identifier
			// that looks like the right one in every log line and operator
			// screen that reads it.
			name:            "a session reference longer than this build can record",
			body:            eventBody(t, kindCheckoutCompleted, map[string]any{"id": strings.Repeat("c", maxStorableReferenceLength+1)}),
			wantKind:        payments.KindCaptured,
			wantCheckoutRef: "",
			wantAmount:      amount(2500),
			wantCurrency:    "USD",
		},
		{
			name:            "an event kind longer than this build can record",
			body:            eventBody(t, strings.Repeat("k", maxStorableKindLength+1), nil),
			wantKind:        "",
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      amount(2500),
			wantCurrency:    "USD",
		},
		{
			name:            "a payment reference longer than this build can record",
			body:            eventBody(t, kindCheckoutCompleted, map[string]any{"payment_intent": strings.Repeat("p", maxStorableReferenceLength+1)}),
			wantKind:        payments.KindCaptured,
			wantCheckoutRef: "cs_test_session_reference",
			wantPaymentRef:  "",
			wantAmount:      amount(2500),
			wantCurrency:    "USD",
		},
		{
			// Not three letters: it cannot be compared with a payment's currency
			// in any way that means anything, and it is not a spelling this
			// build repairs by guessing which three letters were meant.
			name:            "a currency that is not three letters",
			body:            eventBody(t, kindCheckoutCompleted, map[string]any{"currency": "usdollar"}),
			wantKind:        payments.KindCaptured,
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      amount(2500),
			wantCurrency:    "",
		},
		{
			name:            "a currency that is too short",
			body:            eventBody(t, kindCheckoutCompleted, map[string]any{"currency": "us"}),
			wantKind:        payments.KindCaptured,
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      amount(2500),
			wantCurrency:    "",
		},
		{
			name:            "a currency that is not letters",
			body:            eventBody(t, kindCheckoutCompleted, map[string]any{"currency": "u5d"}),
			wantKind:        payments.KindCaptured,
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      amount(2500),
			wantCurrency:    "",
		},
		{
			// The provider's own timestamp, absent. The port makes no claim
			// rather than substituting the signature header's time (when the
			// delivery was sent) or this process's clock.
			name:            "no creation time",
			body:            []byte(`{"id":"evt_test_delivery","type":"checkout.session.completed","data":{"object":{"id":"cs_test_session_reference","amount_total":2500,"currency":"usd","payment_status":"paid"}}}`),
			wantKind:        payments.KindCaptured,
			wantCheckoutRef: "cs_test_session_reference",
			wantAmount:      amount(2500),
			wantCurrency:    "USD",
			wantNoTimestamp: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, err := testVerifier().Verify(signedHeaders(t, testNow, tt.body), tt.body)
			if err != nil {
				t.Fatalf("Verify() error = %v, want the delivery read and the refusal left to the application", err)
			}
			// The corollary the port's rule rests on: every path that returns no
			// error carries an event id, because the caller records the delivery
			// under it.
			if event.EventID == "" {
				t.Error("EventID is empty on a path that returned no error, so the caller has no key to record the delivery under")
			}
			if event.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", event.Kind, tt.wantKind)
			}
			if event.CheckoutRef != tt.wantCheckoutRef {
				t.Errorf("CheckoutRef = %q, want %q", event.CheckoutRef, tt.wantCheckoutRef)
			}
			if event.PaymentRef != tt.wantPaymentRef {
				t.Errorf("PaymentRef = %q, want %q", event.PaymentRef, tt.wantPaymentRef)
			}
			switch {
			case tt.wantAmount == nil && event.AmountMinorUnits != nil:
				t.Errorf("AmountMinorUnits = %d, want none: the amount could not be read exactly, and a rounded one is a charge nobody agreed to", *event.AmountMinorUnits)
			case tt.wantAmount != nil && event.AmountMinorUnits == nil:
				t.Errorf("AmountMinorUnits = none, want %d", *tt.wantAmount)
			case tt.wantAmount != nil && *event.AmountMinorUnits != *tt.wantAmount:
				t.Errorf("AmountMinorUnits = %d, want %d", *event.AmountMinorUnits, *tt.wantAmount)
			}
			if event.Currency != tt.wantCurrency {
				t.Errorf("Currency = %q, want %q", event.Currency, tt.wantCurrency)
			}
			if tt.wantNoTimestamp {
				if !event.OccurredAt.IsZero() {
					t.Errorf("OccurredAt = %s, want no claim at all", event.OccurredAt)
				}
			} else if want := time.Unix(1735689600, 0).UTC(); !event.OccurredAt.Equal(want) {
				t.Errorf("OccurredAt = %s, want %s", event.OccurredAt, want)
			}
		})
	}
}

// TestVerifyReadsARefundAsARefundAndNamesThePaymentItKnows is the refund shape,
// which is the one place this adapter's two readings disagree.
//
// A refund is a CHARGE event: its object is a charge, and the only identifier on
// it that names the payment is the payment intent (or, on a charge old enough to
// predate intents, the charge's own id). That is NOT the checkout reference this
// platform stored when it opened the checkout, and there is no field in this
// delivery that carries one. So the adapter must report it in the port's PAYMENT
// reference field and leave CheckoutRef empty: an adapter that put it in
// CheckoutRef would hand the application a payment id to seek a checkout column
// with, and every refund would be filed as a payment this platform cannot find.
//
// The two lines this adapter may not cross are unchanged and are why the answer
// is a correctly-labelled reference rather than a resolved payment: verification
// is not an operation that may call the provider, and it may not write a
// reference the provider never sent. Resolution is the application's, and it is
// able to make it now that the reference is labelled with what it is.
func TestVerifyReadsARefundAsARefundAndNamesThePaymentItKnows(t *testing.T) {
	body := chargeEventBody(t, kindChargeRefunded, nil)

	event, err := testVerifier().Verify(signedHeaders(t, testNow, body), body)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if event.Kind != payments.KindRefunded {
		t.Errorf("Kind = %q, want %q — the port has a kind for a refund and this delivery is one", event.Kind, payments.KindRefunded)
	}
	if event.PaymentRef != "pi_test_payment_reference" {
		t.Errorf("PaymentRef = %q, want the payment intent, which is the provider's identifier for the payment", event.PaymentRef)
	}
	// And NOT the checkout field: a refund names no checkout, and a reference
	// filed under that name is one the application will seek a checkout column
	// with and never find.
	if event.CheckoutRef != "" {
		t.Errorf("CheckoutRef = %q, want empty — this delivery names no checkout, and a payment id in this field is a lookup that cannot succeed", event.CheckoutRef)
	}
	if event.AmountMinorUnits == nil || *event.AmountMinorUnits != 2500 {
		t.Errorf("AmountMinorUnits = %v, want the amount the provider reports refunded", event.AmountMinorUnits)
	}
	if event.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", event.Currency)
	}

	t.Run("falls back to the charge's own id when the intent is absent", func(t *testing.T) {
		// A charge old enough to predate payment intents states null there, and
		// the charge's own id is then the only payment reference it carries.
		body := chargeEventBody(t, kindChargeRefunded, map[string]any{"payment_intent": nil})
		event, err := testVerifier().Verify(signedHeaders(t, testNow, body), body)
		if err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
		if event.PaymentRef != "ch_test_charge_reference" {
			t.Errorf("PaymentRef = %q, want the charge's own id", event.PaymentRef)
		}
	})

	t.Run("reads a refund whose amount is absent without refusing it", func(t *testing.T) {
		body := chargeEventBody(t, kindChargeRefunded, map[string]any{"amount_refunded": nil})
		event, err := testVerifier().Verify(signedHeaders(t, testNow, body), body)
		if err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
		if event.AmountMinorUnits != nil {
			t.Errorf("AmountMinorUnits = %d, want none: a refund that states no amount is not a refund of zero", *event.AmountMinorUnits)
		}
		if event.EventID == "" {
			t.Error("EventID is empty, so the application has no key to record this refund under")
		}
	})
}

func TestVerifyRefusesAStaleDeliveryDistinguishablyFromABadSignature(t *testing.T) {
	body := eventBody(t, kindCheckoutCompleted, nil)

	// An hour old against a five-minute tolerance.
	_, err := testVerifier().Verify(signedHeaders(t, testNow.Add(-time.Hour), body), body)
	if !errors.Is(err, ErrStaleDelivery) {
		t.Fatalf("Verify() error = %v, want %v", err, ErrStaleDelivery)
	}
	// The distinction is the point of the sentinel: an operator looking at a
	// stale delivery is looking at a clock, and one looking at a bad signature
	// is looking at a secret.
	if errors.Is(err, payments.ErrBadSignature) {
		t.Errorf("Verify() error = %v, and a stale delivery must not be reported as an authentication failure", err)
	}
}

// TestVerifyAcceptsADeliveryFromAClockRunningAhead pins the symmetry of the
// tolerance: a provider whose clock is a little fast is a provider that is
// still sending real money, and a one-sided bound would refuse it.
func TestVerifyAcceptsADeliveryFromAClockRunningAhead(t *testing.T) {
	body := eventBody(t, kindCheckoutCompleted, nil)

	event, err := testVerifier().Verify(signedHeaders(t, testNow.Add(30*time.Second), body), body)
	if err != nil {
		t.Fatalf("Verify() error = %v, want a delivery stamped slightly in the future accepted", err)
	}
	if event.CheckoutRef != "cs_test_session_reference" {
		t.Errorf("CheckoutRef = %q, want the session reference", event.CheckoutRef)
	}
}

func TestVerifyRefusesARepeatedSignatureHeaderCaseInsensitively(t *testing.T) {
	// Stated on its own as well as in the table above, because the case that
	// matters is the one where the two spellings disagree in case only: a
	// lookup written against the map's keys would find one and miss the other.
	body := eventBody(t, kindCheckoutCompleted, nil)
	header := signedHeaders(t, testNow, body)[signatureHeader][0]

	_, err := testVerifier().Verify(map[string][]string{signatureHeader: {header}, "stripe-signature": {header}}, body)
	if !errors.Is(err, payments.ErrBadSignature) {
		t.Fatalf("Verify() error = %v, want %v", err, payments.ErrBadSignature)
	}
}

// TestOpenCheckoutSendsTheProviderItsOwnRequest is the checkout half's contract
// in one call: the operation, the credential, the idempotency key, the
// provider's amount encoding, the reference the platform will match against,
// and no card field anywhere.
func TestOpenCheckoutSendsTheProviderItsOwnRequest(t *testing.T) {
	const (
		sessionID     = "cs_test_session_reference"
		returnURL     = "https://console.internal.example/billing/top-up"
		idempotency   = "topup-intent-0001"
		reference     = "pi_intent_reference"
		sessionAmount = 2500
	)
	// The URL carries everything a canonicaliser would touch — a percent
	// escape, a query, a fragment — because the port's promise is that it
	// crosses byte for byte and a test only proves that with a URL whose bytes
	// are not already canonical.
	const sessionURL = "https://checkout.example/c/pay/cs_test%2Fsession?a=1&b=two#fragment"

	var got struct {
		method        string
		path          string
		idempotency   string
		authorization string
		contentType   string
		form          url.Values
	}
	server := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.idempotency = r.Header.Get(idempotencyHeader)
		got.authorization = r.Header.Get("Authorization")
		got.contentType = r.Header.Get("Content-Type")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
		}
		got.form, err = url.ParseQuery(string(raw))
		if err != nil {
			t.Errorf("the request body is not form-encoded: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"`+sessionID+`","url":"`+sessionURL+`"}`)
	}))

	client := New(server.URL, testAPIKey(), 5*time.Second)
	trustServerTLS(t, server, client)
	session, err := client.OpenCheckout(context.Background(), payments.CheckoutRequest{
		IdempotencyKey:   idempotency,
		AmountMinorUnits: sessionAmount,
		Currency:         "USD",
		Reference:        reference,
		ReturnURL:        returnURL,
	})
	if err != nil {
		t.Fatalf("OpenCheckout() error = %v", err)
	}

	if session.URL != sessionURL {
		t.Errorf("CheckoutSession.URL = %q, want %q returned verbatim", session.URL, sessionURL)
	}
	if session.ProviderRef != sessionID {
		t.Errorf("CheckoutSession.ProviderRef = %q, want %q", session.ProviderRef, sessionID)
	}

	if got.method != http.MethodPost {
		t.Errorf("the provider saw %s, want POST", got.method)
	}
	if got.path != checkoutSessionsPath {
		t.Errorf("the provider saw %s, want %s", got.path, checkoutSessionsPath)
	}
	// A retry that mints a new key is a second charge; this is the header that
	// stops it.
	if got.idempotency != idempotency {
		t.Errorf("the provider saw Idempotency-Key %q, want %q", got.idempotency, idempotency)
	}
	if got.authorization != "Bearer "+testAPIKey() {
		t.Errorf("the provider saw Authorization %q, want the configured API secret", got.authorization)
	}
	if got.contentType != "application/x-www-form-urlencoded" {
		t.Errorf("the provider saw Content-Type %q", got.contentType)
	}

	want := map[string]string{
		"mode":                                   "payment",
		"success_url":                            returnURL,
		"cancel_url":                             returnURL,
		"line_items[0][quantity]":                "1",
		"line_items[0][price_data][currency]":    "usd",
		"line_items[0][price_data][unit_amount]": strconv.Itoa(sessionAmount),
		"line_items[0][price_data][product_data][name]": checkoutProductName,
		"metadata[reference]":                           reference,
		"payment_intent_data[metadata][reference]":      reference,
	}
	for key, value := range want {
		if got.form.Get(key) != value {
			t.Errorf("the provider saw %s = %q, want %q", key, got.form.Get(key), value)
		}
	}

	// The PCI posture, checked on the wire rather than promised in a comment:
	// there is no type in this adapter for a card field to arrive in, and this
	// is the assertion that the request carries none either.
	for key := range got.form {
		for _, forbidden := range []string{"card", "cvc", "cvv", "pan", "exp_"} {
			if strings.Contains(strings.ToLower(key), forbidden) {
				t.Errorf("the checkout request carries %q, and this process never sends card material", key)
			}
		}
	}
}

func TestOpenCheckoutRefusesWhatWouldNotBeTheAgreedCharge(t *testing.T) {
	called := false
	server := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = io.WriteString(w, `{"id":"cs_test_session_reference","url":"https://checkout.example/c/pay"}`)
	}))

	client := New(server.URL, testAPIKey(), 5*time.Second)
	trustServerTLS(t, server, client)
	request := payments.CheckoutRequest{
		IdempotencyKey:   "topup-intent-0001",
		AmountMinorUnits: 2500,
		Currency:         "USD",
		Reference:        "pi_intent_reference",
		ReturnURL:        "https://console.internal.example/billing/top-up",
	}

	tests := []struct {
		name   string
		mutate func(*payments.CheckoutRequest)
	}{
		{
			// The amount the provider cannot encode exactly: a clamp or a
			// rounding would be a charge for a figure nobody agreed to.
			name:   "an amount the provider's encoding cannot carry",
			mutate: func(in *payments.CheckoutRequest) { in.AmountMinorUnits = stripeUnitAmountMax + 1 },
		},
		{
			name:   "an amount of nothing",
			mutate: func(in *payments.CheckoutRequest) { in.AmountMinorUnits = 0 },
		},
		{
			name:   "a negative amount",
			mutate: func(in *payments.CheckoutRequest) { in.AmountMinorUnits = -1 },
		},
		{
			// Without a key the provider cannot recognise a retry as a repeat.
			name:   "no idempotency key",
			mutate: func(in *payments.CheckoutRequest) { in.IdempotencyKey = "" },
		},
		{
			name:   "no return URL",
			mutate: func(in *payments.CheckoutRequest) { in.ReturnURL = "" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := request
			tt.mutate(&in)
			if _, err := client.OpenCheckout(context.Background(), in); err == nil {
				t.Fatal("OpenCheckout() error = nil, want the request refused")
			}
		})
	}

	if called {
		t.Error("a refused checkout reached the provider, and every refusal above is a request that must never be sent")
	}
}

// TestOpenCheckoutDoesNotFollowARedirect pins the client's redirect posture. A
// followed redirect would re-send the API secret and the idempotency key to a
// host no operator configured, and the key is the one value that makes a
// second charge impossible.
func TestOpenCheckoutDoesNotFollowARedirect(t *testing.T) {
	followed := false
	// The redirect TARGET is a plain-http server deliberately. Nothing reaches
	// it, and that is the assertion: a followed redirect would carry the API
	// secret and the idempotency key to a host no operator configured, over
	// cleartext, to a URL the client would have refused as a base. Pointing the
	// target at a scheme the client rejects is the second line of defence
	// showing itself — the first is that no redirect is followed at all.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed = true
	}))
	defer target.Close()

	server := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+checkoutSessionsPath, http.StatusTemporaryRedirect)
	}))

	client := New(server.URL, testAPIKey(), 5*time.Second)
	trustServerTLS(t, server, client)
	_, err := client.OpenCheckout(context.Background(), payments.CheckoutRequest{
		IdempotencyKey:   "topup-intent-0001",
		AmountMinorUnits: 2500,
		Currency:         "USD",
		Reference:        "pi_intent_reference",
		ReturnURL:        "https://console.internal.example/billing/top-up",
	})
	if err == nil {
		t.Fatal("OpenCheckout() error = nil, want the redirect reported rather than followed")
	}
	if !strings.Contains(err.Error(), "unexpected status 307") {
		t.Errorf("OpenCheckout() error = %q, want the redirect's status", err)
	}
	if followed {
		t.Error("the client followed a redirect and re-sent the credential and the idempotency key to another host")
	}
}

// TestOpenCheckoutErrorsCarryNeitherTheEndpointNorTheSecret is the error-hygiene
// half of the adapter's posture: a payment endpoint is configuration and an API
// secret is a secret, and an error line is where both would otherwise end up.
func TestOpenCheckoutErrorsCarryNeitherTheEndpointNorTheSecret(t *testing.T) {
	request := payments.CheckoutRequest{
		IdempotencyKey:   "topup-intent-0001",
		AmountMinorUnits: 2500,
		Currency:         "USD",
		Reference:        "pi_intent_reference",
		ReturnURL:        "https://console.internal.example/billing/top-up",
	}

	// A refusal that echoes the credential in its body, which is what the
	// provider's own error wording does, must still not put it in our error.
	refusing := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid API Key provided: `+testAPIKey()+`"}}`)
	}))

	client := New(refusing.URL, testAPIKey(), 5*time.Second)
	trustServerTLS(t, refusing, client)
	_, err := client.OpenCheckout(context.Background(), request)
	if err == nil {
		t.Fatal("OpenCheckout() error = nil, want the refusal reported")
	}
	if !strings.Contains(err.Error(), "unexpected status 402") {
		t.Errorf("OpenCheckout() error = %q, want the status", err)
	}
	assertNoEndpointOrSecret(t, err, refusing.URL)

	// And the transport failure: http.Client wraps it in a *url.Error whose
	// text embeds the whole request URL, which is the value this adapter
	// strips.
	//
	// The server is TLS for the same reason every other fake provider here is:
	// `New` refuses a cleartext base URL, so an `http://` endpoint could not
	// reach this client at all and a test that used one would be asserting
	// against a wiring the constructor rejects. It is closed before the client
	// is built, so the port is bound and nothing is listening on it — the
	// connection is refused, which is the transport failure being tested.
	unreachable := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := unreachable.URL
	unreachable.Close()

	client = New(endpoint, testAPIKey(), 5*time.Second)
	_, err = client.OpenCheckout(context.Background(), request)
	if err == nil {
		t.Fatal("OpenCheckout() error = nil, want the transport failure reported")
	}
	assertNoEndpointOrSecret(t, err, endpoint)
}

func assertNoEndpointOrSecret(t *testing.T, err error, endpoint string) {
	t.Helper()
	if strings.Contains(err.Error(), endpoint) {
		t.Errorf("error = %q, must not carry the configured endpoint", err)
	}
	if strings.Contains(err.Error(), testAPIKey()) {
		t.Errorf("error = %q, must not carry the provider API secret", err)
	}
}

// TestNewRefusesWiringDefects pins the constructor's panics. Each of these is a
// composition-root defect that would otherwise appear later as a request to
// nowhere, a checkout that can only ever be refused, or — for the timeout — a
// client with no bound on a browser-facing path.
func TestNewRefusesWiringDefects(t *testing.T) {
	tests := []struct {
		name string
		call func()
	}{
		{name: "an empty API base URL", call: func() { New("", testAPIKey(), time.Second) }},
		{name: "a base URL that is not absolute", call: func() { New("api.stripe.com", testAPIKey(), time.Second) }},
		// Cleartext is refused, and the case is a LOCALHOST one so that the
		// refusal cannot be confused with a network question: nothing about
		// 127.0.0.1 is unsafe, and the rule still applies because the URL is
		// where the secret is sent rather than what it is sent to. A
		// constructor that exempted loopback would be a constructor a
		// deployment could be misconfigured past in staging and not in
		// production — or worse, the other way round.
		{name: "a cleartext API base URL", call: func() { New("http://127.0.0.1:8080", testAPIKey(), time.Second) }},
		{name: "a base URL that is not a URL at all", call: func() { New("https://api.stripe.com\x7f", testAPIKey(), time.Second) }},
		{name: "a base URL with userinfo", call: func() { New("https://user:pass@api.stripe.com", testAPIKey(), time.Second) }},
		{name: "a base URL with a query", call: func() { New("https://api.stripe.com?v=1", testAPIKey(), time.Second) }},
		{name: "a base URL with a fragment", call: func() { New("https://api.stripe.com#v1", testAPIKey(), time.Second) }},
		{name: "an empty API secret", call: func() { New("https://api.stripe.com", "", time.Second) }},
		{name: "a zero request timeout", call: func() { New("https://api.stripe.com", testAPIKey(), 0) }},
		{name: "a verifier with no signing secret", call: func() { NewVerifier("", time.Minute) }},
		{name: "a verifier with no tolerance", call: func() { NewVerifier(testSigningSecret(), 0) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("the constructor accepted a wiring defect")
				}
			}()
			tt.call()
		})
	}
}

// TestNewPanicsWithoutQuotingTheBaseURL is the panic's own hygiene rule: a
// startup panic is a log line, and a base URL is the value that may carry
// userinfo.
func TestNewPanicsWithoutQuotingTheBaseURL(t *testing.T) {
	const endpointWithCredentials = "https://user:pass@api.stripe.com"
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("New() accepted a base URL carrying userinfo")
		}
		if message := recovered.(string); strings.Contains(message, "user:pass") {
			t.Errorf("the panic message %q quotes the base URL, and a base URL can carry credentials", message)
		}
	}()
	New(endpointWithCredentials, testAPIKey(), time.Second)
}
