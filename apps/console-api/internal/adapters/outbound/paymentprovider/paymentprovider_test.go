package paymentprovider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	payments "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/payments"
)

// testSigningSecret and testAPIToken are the two credentials the fixtures use.
//
// Both are ASSEMBLED from fragments and neither is shaped like the provider's
// own credentials. This repository is scanned by gitleaks and pushed through
// GitHub's push protection, and both refuse a secret-shaped string — correctly,
// because a fixture needs to be a string the code treats as a credential and
// never a value that could be mistaken for a live one. The values here are
// low-entropy repeats for the same reason.
func testSigningSecret() string { return "sig-" + strings.Repeat("w", 32) }

func testAPIToken() string { return "sepay-" + strings.Repeat("k", 32) }

// newTLSServer stands up the fake provider over TLS, and the scheme is not
// incidental: `New` refuses an `http` base URL because every request it makes
// carries the deployment's API token as a bearer credential, so a test that
// pointed a client at a cleartext `httptest.NewServer` would be asserting
// against a wiring the constructor deliberately refuses to accept. Running the
// fake provider over TLS lets these tests exercise the real request path — the
// JSON body, the Authorization header, the status handling — against a client
// the production composition root could actually construct.
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
// happened the first time this suite was written against an earlier provider:
// the redirect test started failing because the client had started following
// redirects, and the only correct fix was in the fixture. A test helper that can
// quietly remove the behaviour it was not asked to test is a defect in the
// helper.
func trustServerTLS(t *testing.T, server *httptest.Server, client *Client) {
	t.Helper()
	client.httpClient.Transport = server.Client().Transport
}

// testNow is the instant every verifier in this file believes it is. Staleness
// is a comparison against a clock, and a test that read the real one would be
// asserting how long the machine took to get there.
var testNow = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// testVerifier returns a verifier whose clock is pinned at testNow.
//
// Its tolerance is the provider's own 300-second window, stated in the unit the
// provider states it in, which makes the staleness boundary read directly in
// seconds below rather than through an arithmetic of this file's invention.
func testVerifier() *Verifier {
	verifier := NewVerifier(testSigningSecret(), maxProviderTolerance)
	verifier.now = func() time.Time { return testNow }
	return verifier
}

// signFixture computes the provider's signature the way the provider's
// documentation states it: HMAC-SHA256 over the string "{timestamp}.{rawBody}",
// keyed with the signing secret's own bytes.
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

// signatureHeaderFor renders the signature header's value the provider would
// send for a body signed at an instant with a given secret, so a case can state
// a wrong secret or a wrong instant without repeating the whole header map.
func signatureHeaderFor(secret string, signedAt time.Time, body []byte) string {
	stamp := strconv.FormatInt(signedAt.Unix(), 10)
	return signatureScheme + "=" + signFixture(secret, stamp, body)
}

// signedHeaders is the pair of headers a correctly signed delivery carries,
// stamped at the given instant.
//
// The provider's scheme is TWO headers rather than one, so the fixture returns
// both: a delivery authenticated by a single value is a shape this build has
// never been told how to check.
func signedHeaders(t *testing.T, signedAt time.Time, body []byte) map[string][]string {
	t.Helper()
	return map[string][]string{
		signatureHeader: {signatureHeaderFor(testSigningSecret(), signedAt, body)},
		timestampHeader: {strconv.FormatInt(signedAt.Unix(), 10)},
	}
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

// deliveryJSON marshals one delivery body exactly as the fields state it.
func deliveryJSON(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("rendering the delivery fixture: %v", err)
	}
	return raw
}

// eventBody renders a delivery as the provider would send it, with the members
// this build reads. An entry whose value is nil REMOVES that member, which is
// how a case states "the provider sent no amount" rather than "the provider
// sent a zero".
//
// The default id is a JSON NUMBER, because the provider's own documentation
// states it that way; the string spelling this build also accepts is a case of
// its own rather than the fixture's default.
func eventBody(t *testing.T, object map[string]any) []byte {
	t.Helper()
	return deliveryJSON(t, overrides(map[string]any{
		"id":             int64(92704),
		"subAccount":     "va_test_destination",
		"transferType":   transferTypeIn,
		"transferAmount": int64(5000000),
	}, object))
}

// amount is a pointer to an int64 for a table's expectations: the port's money
// fields are pointers precisely so that an absent amount and an amount of zero
// are different sentences, and a table that could not state which of the two it
// expects would not be testing that.
func amount(minorUnits int64) *int64 { return &minorUnits }

const (
	// testBankAccountXID is the provider's identifier for the merchant account
	// orders are issued under. Its shape is the provider's own, and it is spelled
	// only with characters urlSafeSegment admits because `New` refuses one that
	// is not — a value that has to be escaped to become a path segment is a value
	// this adapter declines to build an order URL out of.
	testBankAccountXID = "ba_test_account"

	// testVAHolderName is the name the virtual account is held in: the
	// beneficiary a customer's banking app may show them, and therefore the one
	// order field here that a person sees.
	testVAHolderName = "ECOMA TEST COMPANY LIMITED"

	// testQRCodeTemplate names the layout of the image the provider draws. It is
	// the provider's vocabulary and this build asks for an image on every order.
	testQRCodeTemplate = "compact"

	// testOrderAmount is the amount the client fixtures ask for, and the amount
	// the answer fixtures state the destination was issued for. The two agree
	// because the adapter checks that they do.
	testOrderAmount = 2500
)

// testOrderSettings is one merchant's order configuration, with the two optional
// fields a bank may or may not require left unset so that a case which needs
// them sets them explicitly.
func testOrderSettings() OrderSettings {
	return OrderSettings{
		BankAccountXID: testBankAccountXID,
		VAHolderName:   testVAHolderName,
		QRCodeTemplate: testQRCodeTemplate,
	}
}

// testTransferRequest is one transfer the client may open: a stable identity, a
// positive amount in the currency this provider settles in, and a window the
// console would have shown a customer.
func testTransferRequest() payments.TransferRequest {
	return payments.TransferRequest{
		IdempotencyKey:   "topup-intent-0001",
		AmountMinorUnits: testOrderAmount,
		Currency:         settlementCurrency,
		ExpiresIn:        30 * time.Minute,
	}
}

// orderAnswer renders the provider's answer to one order: the `data` member its
// contract wraps every answer in, with every field this build reads. An entry
// whose value is nil REMOVES that member, so a case can state an absence — which
// is the only way to state "the provider echoed no amount", since a zero would
// be a claim.
func orderAnswer(t *testing.T, object map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"data": overrides(map[string]any{
		"va_number":      "va_test_destination",
		"va_holder_name": testVAHolderName,
		"bank_name":      "Test Bank",
		"qr_code_url":    "https://qr.example/va_test_destination",
		"amount":         int64(testOrderAmount),
		"order_code":     "TOPUPINTENT0001",
	}, object)})
	if err != nil {
		t.Fatalf("rendering the answer fixture: %v", err)
	}
	return string(raw)
}

// openAgainst stands up a fake provider that answers one order with the given
// status and body and opens one transfer against it, so a case about reading an
// answer is one line rather than a server, a client and a trust store.
func openAgainst(t *testing.T, status int, answer string) (payments.TransferInstructions, error) {
	t.Helper()
	server := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	client := New(server.URL, testAPIToken(), testOrderSettings(), 5*time.Second)
	trustServerTLS(t, server, client)
	return client.OpenTransfer(context.Background(), testTransferRequest())
}

// orderLog is the fake provider's record of the orders it was asked for, with a
// lock of its own: the handler runs on the server's goroutine and the test reads
// the slice from its own.
type orderLog struct {
	mu      sync.Mutex
	ordered []orderRequest
}

func (l *orderLog) record(body orderRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ordered = append(l.ordered, body)
}

// codes is the order code of every request the provider was asked for, in the
// order they arrived.
func (l *orderLog) codes() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	codes := make([]string, 0, len(l.ordered))
	for _, body := range l.ordered {
		codes = append(codes, body.OrderCode)
	}
	return codes
}

// orderHandler answers every order with a complete destination and records what
// the provider was asked for.
func orderHandler(t *testing.T, log *orderLog) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body orderRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("the order request is not JSON: %v", err)
		}
		log.record(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, orderAnswer(t, nil))
	})
}

// panicMessage runs call and returns the message it panicked with, failing the
// test if it returned at all.
//
// The constructor refusals below are panics rather than errors because each is a
// composition-root defect, and a test that only asserted "it panicked" would not
// be able to check the one thing a panic message must not contain.
func panicMessage(t *testing.T, call func()) string {
	t.Helper()
	var message string
	func() {
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("the constructor accepted a wiring defect")
			}
			text, ok := recovered.(string)
			if !ok {
				t.Fatalf("the panic carried %T rather than a message", recovered)
			}
			message = text
		}()
		call()
	}()
	return message
}

// assertTextCarriesNeitherEndpointNorSecret is the error-hygiene rule applied to
// any text this package produces, a panic's message included: a payment endpoint
// is configuration and an API token is a token, and each would otherwise end up
// in a log line written by a process that holds the other.
func assertTextCarriesNeitherEndpointNorSecret(t *testing.T, text, endpoint string) {
	t.Helper()
	if endpoint != "" && strings.Contains(text, endpoint) {
		t.Errorf("the text %q carries the configured endpoint", text)
	}
	if strings.Contains(text, testAPIToken()) {
		t.Errorf("the text %q carries the provider API token", text)
	}
}

func assertNoEndpointOrSecret(t *testing.T, err error, endpoint string) {
	t.Helper()
	assertTextCarriesNeitherEndpointNorSecret(t, err.Error(), endpoint)
}

func TestVerifyAcceptsACorrectlySignedDelivery(t *testing.T) {
	// The destination the delivery names is the virtual account the provider
	// issued for this platform's payment, echoed back, and it is the value a
	// capture resolves by. The delivery id is the provider's own name for the
	// transfer that arrived, which is what a redelivery repeats — and the two are
	// different references because they resolve against different stored columns,
	// which is why the port carries them in two fields.
	body := eventBody(t, nil)

	event, err := testVerifier().Verify(signedHeaders(t, testNow, body), body)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if event.Kind != payments.KindCaptured {
		t.Errorf("Kind = %q, want %q — money arriving is this platform's capture", event.Kind, payments.KindCaptured)
	}
	if event.EventID != "92704" {
		t.Errorf("EventID = %q, want the delivery's own id", event.EventID)
	}
	if event.TransferRef != "va_test_destination" {
		t.Errorf("TransferRef = %q, want the virtual account the money arrived at, which is a capture's resolution key", event.TransferRef)
	}
	if event.PaymentRef != "92704" {
		t.Errorf("PaymentRef = %q, want the transaction's own id: it is the economic event a funding leg's key is derived from", event.PaymentRef)
	}
	if event.AmountMinorUnits == nil || *event.AmountMinorUnits != 5000000 {
		t.Errorf("AmountMinorUnits = %v, want 5000000", event.AmountMinorUnits)
	}
	if event.Currency != settlementCurrency {
		t.Errorf("Currency = %q, want %q: the provider names no currency on a delivery and this one settles in exactly one", event.Currency, settlementCurrency)
	}
	// The provider states no instant this build can place — its transactionDate
	// carries no offset — and the port's own spelling of "not said" is the zero
	// time rather than this process's clock.
	if !event.OccurredAt.IsZero() {
		t.Errorf("OccurredAt = %s, want no claim at all", event.OccurredAt)
	}
}

// TestVerifySignsTheRawBytesAndNotTheirReSerialisation is the property the port
// calls its most important one, proved rather than asserted: the fixture is
// valid JSON with insignificant whitespace and a key order no encoder would
// choose, and the signature is computed over those exact octets.
func TestVerifySignsTheRawBytesAndNotTheirReSerialisation(t *testing.T) {
	const raw = `{
  "transferType": "in",

      "transferAmount": 5000000, "id": 92704,
  "subAccount": "va_test_destination"
}`
	body := []byte(raw)

	// The same JSON VALUE, in different bytes. Decoding and re-encoding is
	// exactly what a verifier that parsed the body before checking the signature
	// would be signing.
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
		t.Fatalf("Verify() over the bytes the provider sent = %v, want the delivery accepted", err)
	}

	// The same delivery under that signature, delivered in other bytes, does
	// not. A verifier that decoded first would accept this one — and would accept
	// a forged body whose only difference from a real one was its whitespace.
	if _, err := testVerifier().Verify(signedHeaders(t, testNow, body), reserialised); !errors.Is(err, payments.ErrBadSignature) {
		t.Fatalf("Verify() over re-serialised bytes under the original's signature = %v, want %v", err, payments.ErrBadSignature)
	}

	// And the other direction, so the property is pinned rather than one
	// accidental difference being observed: the padded body's own signature is
	// accepted, which is what makes the refusal above a statement about bytes
	// rather than about this build refusing one particular spelling.
	event, err := testVerifier().Verify(signedHeaders(t, testNow, reserialised), reserialised)
	if err != nil {
		t.Fatalf("Verify() over the re-serialised bytes under their own signature = %v, want the delivery accepted", err)
	}
	if event.TransferRef != "va_test_destination" {
		t.Errorf("TransferRef = %q, want the destination either spelling names", event.TransferRef)
	}
}

func TestVerifyRefusesADeliveryThatIsNotSignedByOurSecret(t *testing.T) {
	body := eventBody(t, nil)
	stamp := strconv.FormatInt(testNow.Unix(), 10)
	signature := signatureHeaderFor(testSigningSecret(), testNow, body)
	tampered := eventBody(t, map[string]any{"transferAmount": int64(5000001)})

	tests := []struct {
		name    string
		headers map[string][]string
		// body is the delivered bytes; nil means the untampered fixture above.
		body []byte
	}{
		{
			name:    "no signature header at all",
			headers: map[string][]string{timestampHeader: {stamp}},
		},
		{
			name:    "no timestamp header at all",
			headers: map[string][]string{signatureHeader: {signature}},
		},
		{
			name: "a signature made with another secret",
			headers: map[string][]string{
				signatureHeader: {signatureHeaderFor("not-our-secret", testNow, body)},
				timestampHeader: {stamp},
			},
		},
		{
			// A scheme this build has not been told how to check is a provider
			// that has moved to something else, and the digest is not compared
			// against anything.
			name: "a scheme this build does not verify",
			headers: map[string][]string{
				signatureHeader: {"sha512=" + signFixture(testSigningSecret(), stamp, body)},
				timestampHeader: {stamp},
			},
		},
		{
			name: "a signature header that names no scheme",
			headers: map[string][]string{
				signatureHeader: {signFixture(testSigningSecret(), stamp, body)},
				timestampHeader: {stamp},
			},
		},
		{
			name: "an empty digest",
			headers: map[string][]string{
				signatureHeader: {signatureScheme + "="},
				timestampHeader: {stamp},
			},
		},
		{
			name: "a digest that is not hexadecimal",
			headers: map[string][]string{
				signatureHeader: {"sha256=zzzz"},
				timestampHeader: {stamp},
			},
		},
		{
			// The signed message is the timestamp's OWN SPELLING, so a header
			// this build cannot read as an instant is one it cannot compare
			// against a clock at all.
			name: "a timestamp that is not a whole number of seconds",
			headers: map[string][]string{
				signatureHeader: {signature},
				timestampHeader: {"1735689600.5"},
			},
		},
		{
			name: "a timestamp that is not a number",
			headers: map[string][]string{
				signatureHeader: {signatureHeaderFor(testSigningSecret(), testNow, body)},
				timestampHeader: {"sooner"},
			},
		},
		{
			// One extra unit on the amount, and nothing else: the delivery this
			// build would otherwise fund is a different figure.
			name:    "a tampered body",
			headers: signedHeaders(t, testNow, body),
			body:    tampered,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delivered := tt.body
			if delivered == nil {
				delivered = body
			}
			_, err := testVerifier().Verify(tt.headers, delivered)
			if !errors.Is(err, payments.ErrBadSignature) {
				t.Fatalf("Verify() error = %v, want %v", err, payments.ErrBadSignature)
			}
		})
	}
}

// TestVerifyRefusesARepeatedHeaderRatherThanChoosingAmongThem is the header
// smuggling case, stated on its own because the provider's scheme makes it
// worse than it is for a provider with one signature header: with TWO required
// headers, an implementation that took "the first of several" would let a
// delivery carry two timestamps and one signature and be verified against
// whichever of the two it happened to pick. The pair a deployment accepted would
// then depend on map iteration order, which is not a configuration anybody can
// reason about, not one an operator could audit, and not one a signature can
// defend. The port preserves the multiplicity so this is refusable at all.
func TestVerifyRefusesARepeatedHeaderRatherThanChoosingAmongThem(t *testing.T) {
	body := eventBody(t, nil)
	stamp := strconv.FormatInt(testNow.Unix(), 10)
	signature := signatureHeaderFor(testSigningSecret(), testNow, body)

	tests := []struct {
		name    string
		headers map[string][]string
	}{
		{
			name: "two signatures in one value list",
			headers: map[string][]string{
				signatureHeader: {signature, signature},
				timestampHeader: {stamp},
			},
		},
		{
			// The same hazard, stated the way a map can also state it: one value
			// under two spellings of the same name. net/http lowercases keys on
			// the way in, so this shape cannot arrive from the wire today — which
			// is exactly why the lookup is written to refuse it rather than to
			// depend on that.
			name: "one signature under two spellings of the header name",
			headers: map[string][]string{
				signatureHeader:     {signature},
				"x-sepay-signature": {signature},
				timestampHeader:     {stamp},
			},
		},
		{
			name: "a good signature and a forged one beside it",
			headers: map[string][]string{
				signatureHeader: {signature, signatureScheme + "=" + strings.Repeat("0", 64)},
				timestampHeader: {stamp},
			},
		},
		{
			name: "two timestamps in one value list",
			headers: map[string][]string{
				signatureHeader: {signature},
				timestampHeader: {stamp, stamp},
			},
		},
		{
			name: "one timestamp under two spellings of the header name",
			headers: map[string][]string{
				signatureHeader:     {signature},
				timestampHeader:     {stamp},
				"X-SEPAY-TIMESTAMP": {stamp},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := testVerifier().Verify(tt.headers, body)
			if !errors.Is(err, payments.ErrBadSignature) {
				t.Fatalf("Verify() error = %v, want %v", err, payments.ErrBadSignature)
			}
		})
	}

	// The control: the same signature and the same timestamp, carried once each,
	// is accepted — so every refusal above is the multiplicity and not the
	// fixture.
	if _, err := testVerifier().Verify(signedHeaders(t, testNow, body), body); err != nil {
		t.Fatalf("Verify() over one of each header = %v, want the delivery accepted", err)
	}
}

// TestVerifyRefusesAStaleDeliveryDistinguishablyFromABadSignature pins the
// package's own sentinel, and the separation is the whole reason it exists: this
// delivery AUTHENTICATED, so an operator looking at it should be looking at a
// clock rather than at the endpoint's signing secret.
//
// The boundary is tested from both sides and in both directions. At exactly the
// tolerance the delivery is still inside the window, which is what makes the
// refusal one second later a statement about the window rather than about this
// machine's clock.
func TestVerifyRefusesAStaleDeliveryDistinguishablyFromABadSignature(t *testing.T) {
	body := eventBody(t, nil)

	// One second outside the window, in the past.
	_, err := testVerifier().Verify(signedHeaders(t, testNow.Add(-(maxProviderTolerance+time.Second)), body), body)
	if !errors.Is(err, ErrStaleDelivery) {
		t.Fatalf("Verify() error = %v, want %v", err, ErrStaleDelivery)
	}
	if errors.Is(err, payments.ErrBadSignature) {
		t.Errorf("Verify() error = %v, and a stale delivery must not be reported as an authentication failure: its signature verified", err)
	}

	// And the boundary from the inside.
	if _, err := testVerifier().Verify(signedHeaders(t, testNow.Add(-maxProviderTolerance), body), body); err != nil {
		t.Fatalf("Verify() at exactly the tolerance = %v, want the delivery accepted", err)
	}

	// The same boundary in the other direction, because the tolerance is
	// symmetric and a one-sided test would not notice a one-sided comparison.
	_, err = testVerifier().Verify(signedHeaders(t, testNow.Add(maxProviderTolerance+time.Second), body), body)
	if !errors.Is(err, ErrStaleDelivery) {
		t.Fatalf("Verify() one second beyond the tolerance in the future = %v, want %v", err, ErrStaleDelivery)
	}
	if errors.Is(err, payments.ErrBadSignature) {
		t.Errorf("Verify() error = %v, and a delivery stamped in the future authenticated just as a stale one did", err)
	}
	if _, err := testVerifier().Verify(signedHeaders(t, testNow.Add(maxProviderTolerance), body), body); err != nil {
		t.Fatalf("Verify() at exactly the tolerance in the future = %v, want the delivery accepted", err)
	}
}

// TestVerifyAcceptsADeliveryFromAClockRunningAhead pins the symmetry of the
// tolerance: a provider whose clock is a little fast is a provider that is still
// sending real money, and a one-sided bound would refuse it.
func TestVerifyAcceptsADeliveryFromAClockRunningAhead(t *testing.T) {
	body := eventBody(t, nil)

	event, err := testVerifier().Verify(signedHeaders(t, testNow.Add(30*time.Second), body), body)
	if err != nil {
		t.Fatalf("Verify() error = %v, want a delivery stamped slightly in the future accepted", err)
	}
	if event.TransferRef != "va_test_destination" {
		t.Errorf("TransferRef = %q, want the destination", event.TransferRef)
	}
}

// TestVerifyNeverReadsAnOutgoingTransferAsARefund is the single most expensive
// mistake available in this integration, so it is written to fail loudly rather
// than to agree quietly.
//
// The provider's "out" says money LEFT the merchant's provider account. It might
// be a payout to the operator's own bank, a fee, a settlement sweep, or a
// transfer an operator made by hand — and every one of those says nothing about
// a customer. Reading it as a refund would attach a real debit to whichever
// payment happened to match the delivery's reference, move that payment's status,
// and write a refund projection for money that was never returned. Nothing later
// would catch it: the delivery is authentic, it names a real destination and a
// real amount, and the ledger would be told that money went back to a customer
// who never asked for it.
func TestVerifyNeverReadsAnOutgoingTransferAsARefund(t *testing.T) {
	body := eventBody(t, map[string]any{"transferType": transferTypeOut})

	event, err := testVerifier().Verify(signedHeaders(t, testNow, body), body)
	if err != nil {
		t.Fatalf("Verify() error = %v, want the delivery read rather than refused: an outgoing transfer is still a delivery this build can name", err)
	}

	switch event.Kind {
	case payments.KindRefunded:
		t.Fatalf("Kind = %q, and reading an outgoing transfer as a refund would book a real debit against a payment nobody refunded — this is the one reading of this body that must never happen", event.Kind)
	case payments.KindCaptured:
		t.Fatalf("Kind = %q, and an outgoing transfer is not money arriving either: it would credit a customer whose money left the account", event.Kind)
	case transferTypeOut:
		// The answer this build means: the provider's own name for a direction
		// it has no rule for, which the application records and does not act on.
	default:
		t.Fatalf("Kind = %q, want the provider's own %q", event.Kind, transferTypeOut)
	}

	// The delivery is still named and still readable. What changes is what the
	// application does with it, not whether this build can record it.
	if event.EventID != "92704" {
		t.Errorf("EventID = %q, want the delivery's own id", event.EventID)
	}
}

// TestVerifyReadsEveryDeliveryItCanNameAndLeavesTheRefusalToTheApplication is
// the contract the port's second sentinel rests on: an event this build cannot
// USE is not an event it cannot READ.
//
// Each case is a well-signed delivery returned as a populated event — with the
// provider's own kind where no port kind is true of it, with a nil amount where
// the amount could not be read exactly, and with an empty reference where the
// delivery names no destination. The application records each one under its real
// event id and quarantines it with a reason an operator resolves
// (unknown_kind, amount_mismatch, unknown_payment); a 5xx or a silent 2xx here
// would instead be this adapter deciding something it cannot know.
func TestVerifyReadsEveryDeliveryItCanNameAndLeavesTheRefusalToTheApplication(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		// wantKind is the port's kind where the adapter maps one, and the
		// provider's own spelling everywhere else.
		wantKind string
		// wantTransferRef is the destination the delivery names, or empty where
		// it names none.
		wantTransferRef string
		wantAmount      *int64
	}{
		{
			name:            "money arriving is this platform's capture",
			body:            eventBody(t, nil),
			wantKind:        payments.KindCaptured,
			wantTransferRef: "va_test_destination",
			wantAmount:      amount(5000000),
		},
		{
			name:            "an outgoing transfer is not a refund and crosses under the provider's own name",
			body:            eventBody(t, map[string]any{"transferType": transferTypeOut}),
			wantKind:        transferTypeOut,
			wantTransferRef: "va_test_destination",
			wantAmount:      amount(5000000),
		},
		{
			name:            "a direction this build has no rule for crosses unchanged",
			body:            eventBody(t, map[string]any{"transferType": "reversal"}),
			wantKind:        "reversal",
			wantTransferRef: "va_test_destination",
			wantAmount:      amount(5000000),
		},
		{
			// No direction at all: the port's zero kind is not a valid event, and
			// the application reads an empty kind as one it does not know — a
			// quarantine rather than a refusal, because the delivery still has an
			// id the caller can record it under.
			name:            "no direction at all",
			body:            eventBody(t, map[string]any{"transferType": nil}),
			wantKind:        "",
			wantTransferRef: "va_test_destination",
			wantAmount:      amount(5000000),
		},
		{
			// A direction spelled as something that is not a string is a
			// direction this build has no rule for rather than an error: it is
			// reported the same way an absent one is, and the delivery keeps its
			// id.
			name:            "a direction this build cannot read as text",
			body:            eventBody(t, map[string]any{"transferType": int64(5)}),
			wantKind:        "",
			wantTransferRef: "va_test_destination",
			wantAmount:      amount(5000000),
		},
		{
			// An absent amount is not a zero, and it is not a refusal either: the
			// port reports no amount and the application refuses the capture.
			name:            "an amount the provider stated as null",
			body:            eventBody(t, map[string]any{"transferAmount": nil}),
			wantKind:        payments.KindCaptured,
			wantTransferRef: "va_test_destination",
			wantAmount:      nil,
		},
		{
			// Zero IS readable — it is an integer — and it crosses as a zero
			// rather than as an absence, because the two are different sentences
			// and the domain refuses the zero on its own terms.
			name:            "an amount of zero",
			body:            eventBody(t, map[string]any{"transferAmount": int64(0)}),
			wantKind:        payments.KindCaptured,
			wantTransferRef: "va_test_destination",
			wantAmount:      amount(0),
		},
		{
			// The shape a provider takes when it changes an integer into a
			// decimal. Truncating 12.5 to 12 is a payment nobody agreed to, so
			// the amount is reported as ABSENT and the application quarantines it
			// as a mismatch.
			name: "an amount spelled as a decimal",
			body: []byte(`{"id":92704,"subAccount":"va_test_destination",` +
				`"transferType":"in","transferAmount":12.5}`),
			wantKind:        payments.KindCaptured,
			wantTransferRef: "va_test_destination",
			wantAmount:      nil,
		},
		{
			name: "an amount spelled with an exponent",
			body: []byte(`{"id":92704,"subAccount":"va_test_destination",` +
				`"transferType":"in","transferAmount":5e6}`),
			wantKind:        payments.KindCaptured,
			wantTransferRef: "va_test_destination",
			wantAmount:      nil,
		},
		{
			// A QUOTED integer is refused too, and it is the refusal most likely
			// to be argued with, because `"5000000"` plainly means five million.
			// It is refused because the port's amount is a NUMBER and a provider
			// that quotes one has changed the type of a money field — and the day
			// a quoted amount arrives it may arrive with a separator, a currency
			// word or a decimal, and the reading that accepts the quotes today is
			// the reading that has to decide what to do with those tomorrow.
			// Refusing costs a quarantine an operator resolves; the tolerant
			// reading costs a guess about money.
			name: "an amount spelled as a quoted integer",
			body: []byte(`{"id":92704,"subAccount":"va_test_destination",` +
				`"transferType":"in","transferAmount":"5000000"}`),
			wantKind:        payments.KindCaptured,
			wantTransferRef: "va_test_destination",
			wantAmount:      nil,
		},
		{
			// Beyond what an int64 states. The port's amount is an int64 and a
			// figure it cannot hold exactly is one it must not round.
			name: "an amount larger than this build can hold exactly",
			body: []byte(`{"id":92704,"subAccount":"va_test_destination",` +
				`"transferType":"in","transferAmount":99999999999999999999}`),
			wantKind:        payments.KindCaptured,
			wantTransferRef: "va_test_destination",
			wantAmount:      nil,
		},
		{
			// No destination: the delivery names no payment this platform can
			// resolve, which the application quarantines as an unknown payment
			// rather than answering 5xx for.
			name:            "no destination named",
			body:            eventBody(t, map[string]any{"subAccount": nil}),
			wantKind:        payments.KindCaptured,
			wantTransferRef: "",
			wantAmount:      amount(5000000),
		},
		{
			name:            "a destination whose spelling this build cannot read as text",
			body:            eventBody(t, map[string]any{"subAccount": map[string]any{"account": "va_test_destination"}}),
			wantKind:        payments.KindCaptured,
			wantTransferRef: "",
			wantAmount:      amount(5000000),
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
			if event.TransferRef != tt.wantTransferRef {
				t.Errorf("TransferRef = %q, want %q", event.TransferRef, tt.wantTransferRef)
			}
			switch {
			case tt.wantAmount == nil && event.AmountMinorUnits != nil:
				t.Errorf("AmountMinorUnits = %d, want none: the amount could not be read exactly, and a rounded one is a payment nobody agreed to", *event.AmountMinorUnits)
			case tt.wantAmount != nil && event.AmountMinorUnits == nil:
				t.Errorf("AmountMinorUnits = none, want %d", *tt.wantAmount)
			case tt.wantAmount != nil && *event.AmountMinorUnits != *tt.wantAmount:
				t.Errorf("AmountMinorUnits = %d, want %d", *event.AmountMinorUnits, *tt.wantAmount)
			}
			// The payload names no currency, and this provider settles in exactly
			// one, so the delivery contributes the same code on every path.
			if event.Currency != settlementCurrency {
				t.Errorf("Currency = %q, want %q", event.Currency, settlementCurrency)
			}
			// The provider states no instant this build can place, on every path.
			if !event.OccurredAt.IsZero() {
				t.Errorf("OccurredAt = %s, want no claim at all", event.OccurredAt)
			}
		})
	}
}

// TestVerifyReadsTheDeliveryIDFromEitherSpellingTheProviderUses pins the second
// reading rawIdentifier makes, and the third case is why it is a reading rather
// than a parse: the provider's id is the text its bytes state, so a figure beyond
// what an int64 holds still names a delivery.
//
// What this adapter deliberately does NOT decide is whether two spellings of one
// number are the SAME delivery: a provider that changed its spelling mid-flight
// would record two deliveries under two ids, and the payment's own state machine
// refuses to turn that into two credits — exactly as it refuses any second
// delivery of one transaction.
func TestVerifyReadsTheDeliveryIDFromEitherSpellingTheProviderUses(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{
			name: "the number's own decimal spelling",
			body: eventBody(t, map[string]any{"id": int64(92704)}),
			want: "92704",
		},
		{
			name: "a JSON string",
			body: eventBody(t, map[string]any{"id": "92704"}),
			want: "92704",
		},
		{
			name: "a figure larger than an int64 states, which is read rather than parsed",
			body: eventBody(t, map[string]any{"id": json.Number("12345678901234567890")}),
			want: "12345678901234567890",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, err := testVerifier().Verify(signedHeaders(t, testNow, tt.body), tt.body)
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			if event.EventID != tt.want {
				t.Errorf("EventID = %q, want %q", event.EventID, tt.want)
			}
		})
	}
}

// TestVerifyNamesADeliveryItCannotOtherwiseRead is the json.RawMessage envelope's
// whole point, in one body: three of the four members are things this build
// cannot convert, and the delivery is still named and still recorded.
//
// With typed fields a single unreadable member would fail the decode of the WHOLE
// envelope and take the id down with it — and the id is the one thing the caller
// needs in order to record the delivery at all. Every unreadable member is
// reported as an absent value instead, and the application turns each into a
// quarantine with a reason an operator can act on.
func TestVerifyNamesADeliveryItCannotOtherwiseRead(t *testing.T) {
	body := []byte(`{"id":92704,"subAccount":{"unreadable":true},"transferType":["in"],"transferAmount":"5000000"}`)

	event, err := testVerifier().Verify(signedHeaders(t, testNow, body), body)
	if err != nil {
		t.Fatalf("Verify() error = %v, want the delivery named and read", err)
	}
	if event.EventID != "92704" {
		t.Errorf("EventID = %q, want the delivery's own id: an unreadable member costs that member, not the whole delivery", event.EventID)
	}
	if event.TransferRef != "" {
		t.Errorf("TransferRef = %q, want none, because the destination could not be read as text", event.TransferRef)
	}
	if event.Kind != "" {
		t.Errorf("Kind = %q, want none, because the direction could not be read as text", event.Kind)
	}
	if event.AmountMinorUnits != nil {
		t.Errorf("AmountMinorUnits = %d, want none", *event.AmountMinorUnits)
	}
	if event.Currency != settlementCurrency {
		t.Errorf("Currency = %q, want %q", event.Currency, settlementCurrency)
	}
}

// TestVerifyReportsAnUnstorableDestinationAsAbsentRatherThanTruncated is the one
// transformation this adapter may never make. A truncated identifier is a
// DIFFERENT identifier: it looks like the right value in every log line and every
// operator screen that reads it, so the delivery would resolve to a payment
// nobody asked about or silently fail to resolve one that arrived. Empty is this
// port's spelling of "not said", and the application answers it with an
// unknown-payment quarantine, which a person resolves.
//
// The reason the bound exists at all is the storage layer: a value longer than
// the column would raise a constraint violation that surfaces as a 5xx, asking
// the provider to redeliver bytes this build can never store.
func TestVerifyReportsAnUnstorableDestinationAsAbsentRatherThanTruncated(t *testing.T) {
	body := eventBody(t, map[string]any{"subAccount": strings.Repeat("v", maxStorableReferenceLength+1)})

	event, err := testVerifier().Verify(signedHeaders(t, testNow, body), body)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if event.TransferRef != "" {
		t.Errorf("TransferRef = %q, want none rather than the first %d bytes of it", event.TransferRef, maxStorableReferenceLength)
	}
	// The delivery is still named, which is what lets the caller record it and
	// quarantine it rather than answer 5xx.
	if event.EventID != "92704" {
		t.Errorf("EventID = %q, want the delivery's own id", event.EventID)
	}
	if event.Kind != payments.KindCaptured {
		t.Errorf("Kind = %q, want %q: only the destination was unusable", event.Kind, payments.KindCaptured)
	}

	// And the boundary from the inside, so the refusal above is the bound rather
	// than any long value.
	atLimit := eventBody(t, map[string]any{"subAccount": strings.Repeat("v", maxStorableReferenceLength)})
	event, err = testVerifier().Verify(signedHeaders(t, testNow, atLimit), atLimit)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if event.TransferRef != strings.Repeat("v", maxStorableReferenceLength) {
		t.Errorf("TransferRef = %q, want the destination stated at exactly the bound", event.TransferRef)
	}
}

// TestVerifyRefusesOnlyABodyItCannotName covers the port's second sentinel, and
// the narrowness of it is the whole test: ErrMalformedEvent means "I could not
// read a delivery id out of these bytes", which is the one case where the caller
// can record NOTHING and answers 2xx. Every other refusal this adapter makes is a
// populated event with no error, and the test above is that half.
//
// The reason the id is the boundary: the application records a delivery under
// (provider, provider account, event id), so a row keyed on an id this adapter
// invented would swallow the NEXT unreadable delivery as a duplicate of this one
// — reporting a real event as already-seen, which is worse than reporting
// nothing.
func TestVerifyRefusesOnlyABodyItCannotName(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "a body that is not JSON at all",
			body: []byte("this is not a delivery"),
		},
		{
			// Valid JSON, and not an object: there is no body to read an id out
			// of.
			name: "a body that is a JSON string",
			body: []byte(`"money arrived"`),
		},
		{
			name: "a body that is a JSON array",
			body: []byte(`[92704]`),
		},
		{
			name: "a body with no delivery id",
			body: eventBody(t, map[string]any{"id": nil}),
		},
		{
			name: "a body whose delivery id is null",
			body: []byte(`{"id":null,"subAccount":"va_test_destination"}`),
		},
		{
			name: "a body whose delivery id is empty",
			body: []byte(`{"id":"","subAccount":"va_test_destination"}`),
		},
		{
			// A decimal is not an identifier: it names nothing this build can key
			// a record on, and inventing the reading would be this adapter
			// deciding what the provider meant.
			name: "a body whose delivery id is a decimal",
			body: []byte(`{"id":12.5}`),
		},
		{
			name: "a body whose delivery id is an exponent",
			body: []byte(`{"id":1e6}`),
		},
		{
			name: "a body whose delivery id is a boolean",
			body: []byte(`{"id":true}`),
		},
		{
			// Longer than the column the id is written to. This is the same
			// answer as an absent id rather than a fourth outcome, and the
			// alternative is what this case exists to prevent: the id is
			// recorded first and unconditionally, so an unrecordable one would
			// reach a bounded column, raise a constraint violation, and surface
			// as a 5xx — a provider retrying forever against a delivery this
			// build can never store.
			name: "a body whose delivery id is longer than this build can record",
			body: []byte(`{"id":"` + strings.Repeat("e", maxProviderEventIDLength+1) + `"}`),
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

// TestNewVerifierRefusesWiringDefects pins the verifier's own panics, and the
// tolerance above the provider's window is the one bound here that is the
// PROVIDER's rather than this process's: honouring a delivery the provider would
// consider expired is a decision this platform has no standing to make, and an
// operator who widened the window to paper over a clock fault would be widening
// the replay window for anybody who captured a delivery.
func TestNewVerifierRefusesWiringDefects(t *testing.T) {
	tests := []struct {
		name string
		call func()
	}{
		{name: "no signing secret", call: func() { NewVerifier("", time.Minute) }},
		{name: "a zero tolerance", call: func() { NewVerifier(testSigningSecret(), 0) }},
		{name: "a negative tolerance", call: func() { NewVerifier(testSigningSecret(), -time.Second) }},
		{
			name: "a tolerance wider than the provider's own window",
			call: func() { NewVerifier(testSigningSecret(), maxProviderTolerance+time.Second) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			panicMessage(t, tt.call)
		})
	}

	// A startup panic is a log line and the signing secret is the one value in
	// this package that must never be in one.
	secret := testSigningSecret()
	for _, tolerance := range []time.Duration{0, -time.Second, maxProviderTolerance + time.Second} {
		message := panicMessage(t, func() { NewVerifier(secret, tolerance) })
		if strings.Contains(message, secret) {
			t.Errorf("the panic message %q quotes the signing secret", message)
		}
	}

	// And the provider's own window is accepted rather than refused: the bound is
	// "not wider than what the provider honours", not "narrower".
	if verifier := NewVerifier(testSigningSecret(), maxProviderTolerance); verifier == nil {
		t.Fatal("NewVerifier() returned no verifier at exactly the provider's own tolerance")
	}
}

// TestProviderNameNamesTheProviderThisAdapterImplements pins the value a
// composition root compares its configured namespace against. It is a fact about
// this CODE rather than about the deployment: a process that named a provider it
// has no adapter for would start healthy, log that it is ready, and answer 400
// "did not authenticate" to every genuine delivery — while sending the operator
// to look at the endpoint secret, which is the one thing that is certainly not
// wrong.
func TestProviderNameNamesTheProviderThisAdapterImplements(t *testing.T) {
	name, ok := payments.ProviderName("sepay")
	if !ok {
		t.Fatal("payments.ProviderName() reports that this build does not implement the provider this adapter is the adapter for")
	}
	if name != "sepay" {
		t.Errorf("payments.ProviderName() = %q, want %q", name, "sepay")
	}
}

// TestOpenTransferSendsTheProviderItsOwnRequest is the order half's contract in
// one call: the operation, the path the merchant's account is named in, the
// credential, the order code derived from the idempotency key, the provider's
// amount and duration encodings, and the image the customer will scan.
func TestOpenTransferSendsTheProviderItsOwnRequest(t *testing.T) {
	const (
		tid      = "TID_TEST_TERMINAL"
		vaPrefix = "9704"
	)

	var got struct {
		method        string
		path          string
		authorization string
		contentType   string
		accept        string
		body          orderRequest
	}
	server := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.authorization = r.Header.Get("Authorization")
		got.contentType = r.Header.Get("Content-Type")
		got.accept = r.Header.Get("Accept")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the order request: %v", err)
		}
		if err := json.Unmarshal(raw, &got.body); err != nil {
			t.Errorf("the order request is not JSON: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, orderAnswer(t, nil))
	}))

	settings := testOrderSettings()
	settings.TID = tid
	settings.VAPrefix = vaPrefix
	client := New(server.URL, testAPIToken(), settings, 5*time.Second)
	trustServerTLS(t, server, client)

	if _, err := client.OpenTransfer(context.Background(), testTransferRequest()); err != nil {
		t.Fatalf("OpenTransfer() error = %v", err)
	}

	if got.method != http.MethodPost {
		t.Errorf("the provider saw %s, want POST", got.method)
	}
	// The merchant account is a PATH SEGMENT rather than a body field, so the
	// account the order is issued under is the one the URL names.
	if want := fmt.Sprintf(orderPath, testBankAccountXID); got.path != want {
		t.Errorf("the provider saw %s, want %s", got.path, want)
	}
	if got.authorization != authorizationScheme+testAPIToken() {
		t.Errorf("the provider saw Authorization %q, want the configured API token", got.authorization)
	}
	if got.contentType != "application/json" {
		t.Errorf("the provider saw Content-Type %q", got.contentType)
	}
	if got.accept != "application/json" {
		t.Errorf("the provider saw Accept %q", got.accept)
	}

	// The order code is this platform's own name for the request, derived from
	// the caller's stable identity: uppercased, with every character the
	// provider's grammar does not admit deleted. One key yields one code, which
	// is what makes the provider's conflict mean "this exact request was already
	// made" rather than "somebody used this name".
	if want := "TOPUPINTENT0001"; got.body.OrderCode != want {
		t.Errorf("the provider saw order_code = %q, want %q", got.body.OrderCode, want)
	}
	if got.body.Amount != testOrderAmount {
		t.Errorf("the provider saw amount = %d, want %d", got.body.Amount, testOrderAmount)
	}
	if got.body.VAHolderName != testVAHolderName {
		t.Errorf("the provider saw va_holder_name = %q, want the configured holder name", got.body.VAHolderName)
	}
	// The provider states this field in SECONDS, and the unit is the whole
	// content of the conversion: read as minutes, a half-hour destination would
	// be issued for thirty hours, and nothing anywhere would raise.
	if want := int64(1800); got.body.Duration != want {
		t.Errorf("the provider saw duration = %d, want %d seconds", got.body.Duration, want)
	}
	// The image is always requested: a destination a customer can scan is the
	// affordance this whole integration exists to produce.
	if !got.body.WithQRCode {
		t.Error("the provider saw with_qrcode = false, and every order this build opens asks for an image")
	}
	if got.body.QRCodeTemplate != testQRCodeTemplate {
		t.Errorf("the provider saw qrcode_template = %q, want %q", got.body.QRCodeTemplate, testQRCodeTemplate)
	}
	if got.body.TID != tid {
		t.Errorf("the provider saw tid = %q, want the configured %q", got.body.TID, tid)
	}
	if got.body.VAPrefix != vaPrefix {
		t.Errorf("the provider saw va_prefix = %q, want the configured %q", got.body.VAPrefix, vaPrefix)
	}
}

// TestOpenTransferOmitsTheOptionalOrderFieldsWhenTheDeploymentHasNotSetThem pins
// the other half of the two optional fields: which bank needs a terminal
// identifier and which needs a virtual-account prefix is the provider's own
// documentation's business and it changes, so a deployment's answer to "does our
// bank need this" is configuration. An unset field is one the provider never
// sees rather than one it sees as the empty string — sending an empty one would
// be this platform asserting something about the bank it does not know.
func TestOpenTransferOmitsTheOptionalOrderFieldsWhenTheDeploymentHasNotSetThem(t *testing.T) {
	called := false
	var fields map[string]json.RawMessage
	server := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Errorf("the order request is not JSON: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, orderAnswer(t, nil))
	}))

	client := New(server.URL, testAPIToken(), testOrderSettings(), 5*time.Second)
	trustServerTLS(t, server, client)
	if _, err := client.OpenTransfer(context.Background(), testTransferRequest()); err != nil {
		t.Fatalf("OpenTransfer() error = %v", err)
	}
	if !called {
		t.Fatal("no order reached the provider, so the body below was never observed")
	}

	for _, absent := range []string{"tid", "va_prefix"} {
		if _, present := fields[absent]; present {
			t.Errorf("the order request carries %q, and this deployment has not set it", absent)
		}
	}
	// The required fields are all still there: an omission test that passed
	// because the body was empty would prove nothing.
	for _, required := range []string{"order_code", "amount", "va_holder_name", "duration", "with_qrcode", "qrcode_template"} {
		if _, present := fields[required]; !present {
			t.Errorf("the order request omits %q, which the provider's contract requires", required)
		}
	}
}

// TestOpenTransferDerivesOneOrderCodePerIdempotencyKey is the property the whole
// method depends on: one attempt, one key, one code, and therefore a conflict
// that means "this exact request was already made".
func TestOpenTransferDerivesOneOrderCodePerIdempotencyKey(t *testing.T) {
	log := &orderLog{}
	server := newTLSServer(t, orderHandler(t, log))
	client := New(server.URL, testAPIToken(), testOrderSettings(), 5*time.Second)
	trustServerTLS(t, server, client)

	open := func(t *testing.T, key string) {
		t.Helper()
		in := testTransferRequest()
		in.IdempotencyKey = key
		if _, err := client.OpenTransfer(context.Background(), in); err != nil {
			t.Fatalf("OpenTransfer(%q) error = %v", key, err)
		}
	}

	// One key, two attempts: the SAME code. A retry of one attempt carries one
	// identity, so a provider that refuses a duplicate is refusing a request this
	// platform genuinely already made rather than a second destination for one
	// payment.
	open(t, "topup-intent-0001")
	open(t, "topup-intent-0001")
	codes := log.codes()
	if len(codes) != 2 {
		t.Fatalf("the provider was asked for %d orders, want 2", len(codes))
	}
	if codes[0] != codes[1] {
		t.Errorf("two attempts under one key saw codes %q and %q, want one code: a retry that mints a new identity is a second destination for one payment", codes[0], codes[1])
	}
	if want := "TOPUPINTENT0001"; codes[0] != want {
		t.Errorf("the provider saw order_code = %q, want %q", codes[0], want)
	}

	// Two keys, two codes: the derivation is of the CALLER's key, so two logical
	// transfers are two orders at the provider.
	open(t, "topup-intent-0002")
	codes = log.codes()
	if codes[2] == codes[0] {
		t.Errorf("two different keys both yielded %q, so a second payment would be refused as a duplicate of the first", codes[2])
	}

	// And the provider's own grammar, on every code this build mints: alphanumeric
	// from six to fifty characters.
	for _, code := range codes {
		if len(code) < minOrderCodeLength || len(code) > maxOrderCodeLength {
			t.Errorf("the provider saw order_code %q of %d characters, and its grammar admits %d to %d", code, len(code), minOrderCodeLength, maxOrderCodeLength)
		}
		for _, r := range code {
			switch {
			case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			default:
				t.Errorf("the provider saw order_code %q, and %q is not a character its grammar admits", code, r)
			}
		}
	}

	t.Run("a key too short to become a code is refused before anything is sent", func(t *testing.T) {
		// The floor is the provider's and it is enforced rather than padded:
		// padding would be manufacturing the identity the caller was supposed to
		// supply, and the caller's key is what makes a retry recognisable.
		in := testTransferRequest()
		in.IdempotencyKey = "ab-1"
		if _, err := client.OpenTransfer(context.Background(), in); err == nil {
			t.Fatal("OpenTransfer() error = nil, want a key too short for the provider's grammar refused")
		}
		if got := len(log.codes()); got != 3 {
			t.Errorf("the provider was asked for %d orders, want 3: a key that cannot name the request is refused before a byte goes on the wire", got)
		}
	})

	t.Run("a key longer than the grammar is truncated rather than refused", func(t *testing.T) {
		// A prefix of an already-unique string is still unique enough for a name
		// nothing resolves by: the code is this platform's name for its own
		// request, and the destination that funds a payment is resolved through
		// the virtual account the provider issued, never through this.
		in := testTransferRequest()
		in.IdempotencyKey = strings.Repeat("k", maxOrderCodeLength+10)
		if _, err := client.OpenTransfer(context.Background(), in); err != nil {
			t.Fatalf("OpenTransfer() error = %v, want a long key truncated rather than refused", err)
		}
		codes := log.codes()
		if got, want := codes[len(codes)-1], strings.Repeat("K", maxOrderCodeLength); got != want {
			t.Errorf("the provider saw order_code = %q, want the first %d characters %q", got, maxOrderCodeLength, want)
		}
	})
}

// TestOpenTransferReadsTheProvidersAnswer pins the translation of the provider's
// answer into the port's description of a destination. Every string crosses BYTE
// FOR BYTE: the bank's name and the holder's are a promise the provider makes and
// this process keeps on the provider's behalf, and the reference is the value a
// later delivery will name.
func TestOpenTransferReadsTheProvidersAnswer(t *testing.T) {
	instructions, err := openAgainst(t, http.StatusCreated, orderAnswer(t, map[string]any{
		"va_number":   "va_test_destination",
		"bank_name":   "Vietcombank",
		"qr_code_url": "https://qr.example/va_test_destination",
	}))
	if err != nil {
		t.Fatalf("OpenTransfer() error = %v", err)
	}
	if instructions.TransferCode != "va_test_destination" {
		t.Errorf("TransferCode = %q, want the provider's own identifier for the destination, returned verbatim", instructions.TransferCode)
	}
	if instructions.BankName != "Vietcombank" {
		t.Errorf("BankName = %q, want the provider's own name for the institution", instructions.BankName)
	}
	if instructions.AccountHolder != testVAHolderName {
		t.Errorf("AccountHolder = %q, want the virtual account's own holder name", instructions.AccountHolder)
	}
	if instructions.QRURL != "https://qr.example/va_test_destination" {
		t.Errorf("QRURL = %q, want the provider's own image of the transfer", instructions.QRURL)
	}

	t.Run("prefers the virtual account's holder over the bank account's", func(t *testing.T) {
		// Both fields name the holder of a destination, and only one of them
		// names the destination the customer is about to pay into, so the
		// preference is specificity rather than a fallback between unrelated
		// values.
		instructions, err := openAgainst(t, http.StatusCreated, orderAnswer(t, map[string]any{
			"va_holder_name":      "THE VIRTUAL ACCOUNT HOLDER",
			"account_holder_name": "THE UNDERLYING ACCOUNT HOLDER",
		}))
		if err != nil {
			t.Fatalf("OpenTransfer() error = %v", err)
		}
		if instructions.AccountHolder != "THE VIRTUAL ACCOUNT HOLDER" {
			t.Errorf("AccountHolder = %q, want the virtual account's own holder name", instructions.AccountHolder)
		}
	})

	t.Run("falls back to the underlying account's holder when the virtual account states none", func(t *testing.T) {
		instructions, err := openAgainst(t, http.StatusCreated, orderAnswer(t, map[string]any{
			"va_holder_name":      nil,
			"account_holder_name": "THE UNDERLYING ACCOUNT HOLDER",
		}))
		if err != nil {
			t.Fatalf("OpenTransfer() error = %v", err)
		}
		if instructions.AccountHolder != "THE UNDERLYING ACCOUNT HOLDER" {
			t.Errorf("AccountHolder = %q, want the account holder the answer does state", instructions.AccountHolder)
		}
	})

	t.Run("accepts an answer that drew no image", func(t *testing.T) {
		// Empty is an ordinary value rather than a failure: the destination is
		// complete without it, a customer can type an account number into a
		// banking app, and a build that refused to open a payment because an
		// image was missing would be refusing money over a decoration.
		instructions, err := openAgainst(t, http.StatusCreated, orderAnswer(t, map[string]any{"qr_code_url": nil}))
		if err != nil {
			t.Fatalf("OpenTransfer() error = %v, want an answer with no image accepted", err)
		}
		if instructions.QRURL != "" {
			t.Errorf("QRURL = %q, want the empty string the port carries for an image the provider did not draw", instructions.QRURL)
		}
		if instructions.TransferCode != "va_test_destination" {
			t.Errorf("TransferCode = %q, want the destination: the three non-empty members are always enough to pay", instructions.TransferCode)
		}
	})

	t.Run("accepts a 200 as well as a 201", func(t *testing.T) {
		// A creation answer is spelled both ways across HTTP APIs, and treating a
		// 200 as a failure would refuse a working order for its status line.
		instructions, err := openAgainst(t, http.StatusOK, orderAnswer(t, nil))
		if err != nil {
			t.Fatalf("OpenTransfer() error = %v, want a 200 answer accepted", err)
		}
		if instructions.TransferCode != "va_test_destination" {
			t.Errorf("TransferCode = %q, want the destination", instructions.TransferCode)
		}
	})
}

// TestOpenTransferRefusesAnAnswerIssuedForAnotherAmount is the one check the
// answer's own amount exists for, and the asymmetry it makes is deliberate: a
// PRESENT and different figure is a contradiction between the provider's answer
// and this platform's record, while an ABSENT one is independently covered — the
// webhook's own amount is checked against the payment before any credit, so an
// unverified figure can never reach the ledger.
func TestOpenTransferRefusesAnAnswerIssuedForAnotherAmount(t *testing.T) {
	// A destination issued for a figure other than the one the payment is
	// denominated in is a payment no customer can settle correctly: the money
	// would arrive, the delivery would name it, and the application's own amount
	// check would quarantine it against a payment priced at the other figure.
	// Refusing to show the destination is the last point at which that is still
	// recoverable.
	instructions, err := openAgainst(t, http.StatusCreated, orderAnswer(t, map[string]any{"amount": int64(testOrderAmount + 1)}))
	if err == nil {
		t.Fatal("OpenTransfer() error = nil, want an answer issued for another amount refused")
	}
	if instructions != (payments.TransferInstructions{}) {
		t.Errorf("OpenTransfer() instructions = %+v, want none alongside the refusal: a destination this build cannot vouch for must not be shown to a customer", instructions)
	}

	// Absence is not disagreement, and the second half of this test is what keeps
	// the first half from being a blanket "the answer must state an amount".
	instructions, err = openAgainst(t, http.StatusCreated, orderAnswer(t, map[string]any{"amount": nil}))
	if err != nil {
		t.Fatalf("OpenTransfer() error = %v, want an answer stating no amount accepted", err)
	}
	if instructions.TransferCode != "va_test_destination" {
		t.Errorf("TransferCode = %q, want the destination", instructions.TransferCode)
	}
}

// TestOpenTransferRefusesAnAnswerThatIsNotADestination covers the refusals on the
// reading side. Each is a provider breaking its own contract, and each fails
// closed: no destination is returned, so no customer is shown an account to send
// money to.
func TestOpenTransferRefusesAnAnswerThatIsNotADestination(t *testing.T) {
	tests := []struct {
		name   string
		answer string
	}{
		{
			name:   "an answer with no data member",
			answer: `{}`,
		},
		{
			name:   "an answer with no virtual account number",
			answer: orderAnswer(t, map[string]any{"va_number": nil}),
		},
		{
			name:   "an answer with an empty virtual account number",
			answer: orderAnswer(t, map[string]any{"va_number": ""}),
		},
		{
			name:   "an answer with a virtual account number longer than this build can record",
			answer: orderAnswer(t, map[string]any{"va_number": strings.Repeat("v", maxStorableRefLength+1)}),
		},
		{
			name:   "an answer naming no bank",
			answer: orderAnswer(t, map[string]any{"bank_name": nil}),
		},
		{
			name:   "an answer naming no holder at all",
			answer: orderAnswer(t, map[string]any{"va_holder_name": nil}),
		},
		{
			// The three bounds below are the schema's rather than the provider's,
			// and each exists for the same reason the webhook path applies its own
			// bound before recording: a string that cannot be stored must not be
			// written raw. The column's answer to one would be a constraint
			// violation nothing translates, so the transfer would roll back and
			// every later attempt for that payment would fail identically — a
			// permanent 500 against a working provider, discovered by a customer.
			// None of these is a refusal of the payment, so each is reported as an
			// error the use case surfaces while the payment stays as it was.
			name:   "an answer naming a bank longer than this build can record",
			answer: orderAnswer(t, map[string]any{"bank_name": strings.Repeat("b", maxStorableRefLength+1)}),
		},
		{
			name:   "an answer naming a holder longer than this build can record",
			answer: orderAnswer(t, map[string]any{"va_holder_name": strings.Repeat("h", maxStorableRefLength+1)}),
		},
		{
			// An image the provider drew but this build cannot keep is refused,
			// and that is not the same question as an image the provider did not
			// draw: the absent one is an ordinary value (see the test above),
			// while an unkeepable one would be a column violation on a value the
			// customer cannot use anyway.
			name:   "an answer carrying an image url longer than this build can keep",
			answer: orderAnswer(t, map[string]any{"qr_code_url": "https://qr.example/" + strings.Repeat("q", maxStorableQRURLLength+1)}),
		},
		{
			name:   "an answer that is not JSON",
			answer: "not an order",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instructions, err := openAgainst(t, http.StatusCreated, tt.answer)
			if err == nil {
				t.Fatal("OpenTransfer() error = nil, want the answer refused")
			}
			if instructions != (payments.TransferInstructions{}) {
				t.Errorf("OpenTransfer() instructions = %+v, want none alongside the refusal", instructions)
			}
		})
	}
}

// TestOpenTransferMapsAProviderRefusalToThePortsOwnSentinels pins where the line
// between "wait" and "stop" is drawn, because the two send a client in opposite
// directions and only one of them is a condition that clears.
func TestOpenTransferMapsAProviderRefusalToThePortsOwnSentinels(t *testing.T) {
	t.Run("a conflict is an order code the provider already holds", func(t *testing.T) {
		// The provider answers a second order under a code it already holds with
		// a conflict, and the destination it issued the first time is not
		// returned to this process — nor is there any way to read it back by the
		// code that names it. The attempt is therefore abandoned rather than
		// retried: the bytes of the next attempt would be the same bytes and the
		// answer would be the same answer.
		_, err := openAgainst(t, http.StatusConflict, `{"error":{"message":"order_code already exists"}}`)
		if !errors.Is(err, payments.ErrOrderCodeTaken) {
			t.Fatalf("OpenTransfer() error = %v, want %v", err, payments.ErrOrderCodeTaken)
		}
		if errors.Is(err, payments.ErrProviderUnavailable) {
			t.Errorf("OpenTransfer() error = %v, and a conflict is a refusal no retry can repair", err)
		}
	})

	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusTooManyRequests,
	} {
		t.Run(fmt.Sprintf("status %d is a condition that clears", status), func(t *testing.T) {
			// A 5xx is the provider's own service failing, and a 429 is the
			// provider asking to be called again more slowly — which is the same
			// instruction, expressed by the one status that carries it. Opening
			// transfers bursts when a customer clicks through the console's
			// offers, so waiting is the correct answer to both.
			_, err := openAgainst(t, status, `{"error":"unwell"}`)
			if !errors.Is(err, payments.ErrProviderUnavailable) {
				t.Fatalf("OpenTransfer() error = %v, want %v", err, payments.ErrProviderUnavailable)
			}
			if errors.Is(err, payments.ErrOrderCodeTaken) {
				t.Errorf("OpenTransfer() error = %v, and a provider that is unwell has not refused this request's identity", err)
			}
		})
	}

	t.Run("a refused request is neither sentinel", func(t *testing.T) {
		// A 4xx is the provider rejecting something about the request — a
		// credential, a parameter, a permission — and no amount of retrying
		// supplies what it refused. That is this plane's defect or this plane's
		// configuration, and the honest answer is an internal error rather than a
		// 503 asking for a retry storm.
		_, err := openAgainst(t, http.StatusForbidden, `{"error":{"message":"permission denied"}}`)
		if err == nil {
			t.Fatal("OpenTransfer() error = nil, want the refusal reported")
		}
		if errors.Is(err, payments.ErrOrderCodeTaken) || errors.Is(err, payments.ErrProviderUnavailable) {
			t.Errorf("OpenTransfer() error = %v, want a refusal this build reports as its own fault", err)
		}
		// The status is the whole message an operator acts on: a 401 and a 400
		// need different responses, and both are legible without the provider's
		// wording.
		if !strings.Contains(err.Error(), "unexpected status 403") {
			t.Errorf("OpenTransfer() error = %q, want the status", err)
		}
	})
}

// TestOpenTransferDoesNotFollowARedirect pins the client's redirect posture. A
// followed redirect would re-send the API token AND the order identity to
// whatever host the response named — a host no operator configured — and the
// order identity is the one value that makes a duplicate destination impossible
// to mint silently.
func TestOpenTransferDoesNotFollowARedirect(t *testing.T) {
	followed := false
	// The redirect TARGET is a plain-http server deliberately. Nothing reaches
	// it, and that is the assertion: a followed redirect would carry the token
	// and the order identity to a host no operator configured, over cleartext, to
	// a URL the client would have refused as a base. Pointing the target at a
	// scheme the client rejects is the second line of defence showing itself —
	// the first is that no redirect is followed at all.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed = true
	}))
	defer target.Close()

	server := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+fmt.Sprintf(orderPath, testBankAccountXID), http.StatusTemporaryRedirect)
	}))

	client := New(server.URL, testAPIToken(), testOrderSettings(), 5*time.Second)
	trustServerTLS(t, server, client)
	_, err := client.OpenTransfer(context.Background(), testTransferRequest())
	if err == nil {
		t.Fatal("OpenTransfer() error = nil, want the redirect reported rather than followed")
	}
	if !strings.Contains(err.Error(), "unexpected status 307") {
		t.Errorf("OpenTransfer() error = %q, want the redirect's status", err)
	}
	if followed {
		t.Error("the client followed a redirect and re-sent the credential and the order identity to another host")
	}
}

// TestOpenTransferErrorsCarryNeitherTheEndpointNorTheSecret is the error-hygiene
// half of the adapter's posture: a payment endpoint is configuration and an API
// token is a token, and an error line is where both would otherwise end up.
func TestOpenTransferErrorsCarryNeitherTheEndpointNorTheSecret(t *testing.T) {
	// A refusal that echoes the credential in its body, which is what a
	// provider's own error wording does, must still not put it in our error. This
	// package never reads an error body: the status is what an operator acts on,
	// and a body is where an attacker-chosen or credential-bearing string would
	// travel into a log written by the process that holds the credential.
	refusing := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid API token provided: `+testAPIToken()+`"}}`)
	}))

	client := New(refusing.URL, testAPIToken(), testOrderSettings(), 5*time.Second)
	trustServerTLS(t, refusing, client)
	_, err := client.OpenTransfer(context.Background(), testTransferRequest())
	if err == nil {
		t.Fatal("OpenTransfer() error = nil, want the refusal reported")
	}
	if !strings.Contains(err.Error(), "unexpected status 402") {
		t.Errorf("OpenTransfer() error = %q, want the status", err)
	}
	if strings.Contains(err.Error(), "Invalid API token provided") {
		t.Errorf("OpenTransfer() error = %q, and this build must not repeat the provider's own wording about the credential it rejected", err)
	}
	assertNoEndpointOrSecret(t, err, refusing.URL)

	// And the transport failure: http.Client wraps it in a *url.Error whose text
	// embeds the whole request URL, which is the value this adapter strips.
	//
	// The server is TLS for the same reason every other fake provider here is:
	// `New` refuses a cleartext base URL, so an `http://` endpoint could not reach
	// this client at all and a test that used one would be asserting against a
	// wiring the constructor rejects. It is closed before the client is built, so
	// the port is bound and nothing is listening on it — the connection is
	// refused, which is the transport failure being tested.
	unreachable := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := unreachable.URL
	unreachable.Close()

	client = New(endpoint, testAPIToken(), testOrderSettings(), 5*time.Second)
	_, err = client.OpenTransfer(context.Background(), testTransferRequest())
	if err == nil {
		t.Fatal("OpenTransfer() error = nil, want the transport failure reported")
	}
	assertNoEndpointOrSecret(t, err, endpoint)
	// This package cannot tell a provider that is down from a DNS entry that is
	// wrong, and it does not guess: the caller's answer for both is a 503, which
	// is the right answer for one of them and a harmless one for the other.
	if !errors.Is(err, payments.ErrProviderUnavailable) {
		t.Errorf("OpenTransfer() error = %v, want %v: a failure to reach the provider at all is the condition that clears on its own", err, payments.ErrProviderUnavailable)
	}
}

// TestOpenTransferRefusesWhatWouldNotBeTheAgreedTransfer pins the requests this
// client refuses, and it refuses them BEFORE a byte goes on the wire. Each is a
// request the provider would refuse — or, worse, accept.
func TestOpenTransferRefusesWhatWouldNotBeTheAgreedTransfer(t *testing.T) {
	server := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a refused transfer reached the provider, and every refusal below is a request that must never be sent")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, orderAnswer(t, nil))
	}))
	client := New(server.URL, testAPIToken(), testOrderSettings(), 5*time.Second)
	trustServerTLS(t, server, client)

	tests := []struct {
		name   string
		mutate func(*payments.TransferRequest)
	}{
		{
			// The provider settles domestic transfers in exactly one currency, so
			// a request for another is a deployment that has priced an offer in a
			// currency this provider cannot move. Refusing here keeps that a
			// configuration fault with a clear message rather than a destination
			// that is issued, shown to a customer, paid into, and only then
			// refused by the delivery's own currency check.
			name:   "a currency this provider cannot settle in",
			mutate: func(in *payments.TransferRequest) { in.Currency = "USD" },
		},
		{
			name:   "no currency at all",
			mutate: func(in *payments.TransferRequest) { in.Currency = "" },
		},
		{
			// A call the provider cannot recognise as a repeat of another call is
			// the definition of a second destination for one payment.
			name:   "no idempotency key",
			mutate: func(in *payments.TransferRequest) { in.IdempotencyKey = "" },
		},
		{
			name:   "a key the provider's grammar cannot turn into an order code",
			mutate: func(in *payments.TransferRequest) { in.IdempotencyKey = "ab1" },
		},
		{
			name:   "an amount of nothing",
			mutate: func(in *payments.TransferRequest) { in.AmountMinorUnits = 0 },
		},
		{
			name:   "a negative amount",
			mutate: func(in *payments.TransferRequest) { in.AmountMinorUnits = -1 },
		},
		{
			// The provider states this field in whole seconds, so a lifetime that
			// is not a whole number of them is refused rather than rounded: a
			// rounded lifetime is an account that expires at a moment nobody
			// chose.
			name:   "a lifetime that is not a whole number of seconds",
			mutate: func(in *payments.TransferRequest) { in.ExpiresIn = 1500 * time.Millisecond },
		},
		{
			name:   "a lifetime of nothing",
			mutate: func(in *payments.TransferRequest) { in.ExpiresIn = 0 },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := testTransferRequest()
			tt.mutate(&in)
			if _, err := client.OpenTransfer(context.Background(), in); err == nil {
				t.Fatal("OpenTransfer() error = nil, want the request refused")
			}
		})
	}
}

// TestNewRefusesWiringDefects pins the constructor's panics. Each of these is a
// composition-root defect that would otherwise appear far from the line that
// could have said so: a request to nowhere, a transfer that can only ever be
// refused, or — for the timeout — a client with no bound at all on a path a
// customer's browser is waiting on.
//
// Every refusal is also checked for the two values a startup panic must never
// carry: the base URL, which may bear credentials, and the API token.
func TestNewRefusesWiringDefects(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		call     func()
	}{
		{
			name:     "an empty API base URL",
			endpoint: "",
			call:     func() { New("", testAPIToken(), testOrderSettings(), time.Second) },
		},
		{
			name:     "a base URL that is not absolute",
			endpoint: "api.sepay.example",
			call:     func() { New("api.sepay.example", testAPIToken(), testOrderSettings(), time.Second) },
		},
		{
			// Cleartext is refused, and the case is a LOOPBACK one so that the
			// refusal cannot be confused with a network question: nothing about
			// 127.0.0.1 is unsafe, and the rule still applies because the URL is
			// where the token is sent rather than what it is sent to. A
			// constructor that exempted loopback would be a constructor a
			// deployment could be misconfigured past in staging and not in
			// production — or worse, the other way round.
			name:     "a cleartext API base URL",
			endpoint: "http://127.0.0.1:8080",
			call:     func() { New("http://127.0.0.1:8080", testAPIToken(), testOrderSettings(), time.Second) },
		},
		{
			name:     "a base URL that is not a URL at all",
			endpoint: "https://api.sepay.example\x7f",
			call:     func() { New("https://api.sepay.example\x7f", testAPIToken(), testOrderSettings(), time.Second) },
		},
		{
			name:     "a base URL with userinfo",
			endpoint: "https://user:pass@api.sepay.example",
			call:     func() { New("https://user:pass@api.sepay.example", testAPIToken(), testOrderSettings(), time.Second) },
		},
		{
			name:     "a base URL with a query",
			endpoint: "https://api.sepay.example?version=2",
			call:     func() { New("https://api.sepay.example?version=2", testAPIToken(), testOrderSettings(), time.Second) },
		},
		{
			name:     "a base URL with a fragment",
			endpoint: "https://api.sepay.example#v2",
			call:     func() { New("https://api.sepay.example#v2", testAPIToken(), testOrderSettings(), time.Second) },
		},
		{
			name:     "a base URL naming no host",
			endpoint: "https:///bank-accounts/ba_test_account/orders",
			call: func() {
				New("https:///bank-accounts/ba_test_account/orders", testAPIToken(), testOrderSettings(), time.Second)
			},
		},
		{
			name:     "an empty API token",
			endpoint: "https://api.sepay.example",
			call:     func() { New("https://api.sepay.example", "", testOrderSettings(), time.Second) },
		},
		{
			name:     "a zero request timeout",
			endpoint: "https://api.sepay.example",
			call:     func() { New("https://api.sepay.example", testAPIToken(), testOrderSettings(), 0) },
		},
		{
			name:     "a negative request timeout",
			endpoint: "https://api.sepay.example",
			call:     func() { New("https://api.sepay.example", testAPIToken(), testOrderSettings(), -time.Second) },
		},
		{
			name:     "no bank account to issue orders under",
			endpoint: "https://api.sepay.example",
			call: func() {
				settings := testOrderSettings()
				settings.BankAccountXID = ""
				New("https://api.sepay.example", testAPIToken(), settings, time.Second)
			},
		},
		{
			// A path segment that needs escaping is refused rather than escaped:
			// whether the provider decodes it back to this value is a fact about
			// the provider's own framework, and an order issued under a
			// mis-decoded account identifier is a destination for money that
			// belongs to somebody else's merchant account.
			name:     "a bank account identifier that would need escaping",
			endpoint: "https://api.sepay.example",
			call: func() {
				settings := testOrderSettings()
				settings.BankAccountXID = "ba/account/../other"
				New("https://api.sepay.example", testAPIToken(), settings, time.Second)
			},
		},
		{
			name:     "no virtual account holder name",
			endpoint: "https://api.sepay.example",
			call: func() {
				settings := testOrderSettings()
				settings.VAHolderName = ""
				New("https://api.sepay.example", testAPIToken(), settings, time.Second)
			},
		},
		{
			name:     "no QR code template",
			endpoint: "https://api.sepay.example",
			call: func() {
				settings := testOrderSettings()
				settings.QRCodeTemplate = ""
				New("https://api.sepay.example", testAPIToken(), settings, time.Second)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := panicMessage(t, tt.call)
			assertTextCarriesNeitherEndpointNorSecret(t, message, tt.endpoint)
		})
	}
}

// TestNewPanicsWithoutQuotingTheBaseURL is the panic's own hygiene rule, stated
// on its own as well as in the table above: a startup panic is a log line, and a
// base URL is the value that may carry userinfo — a credential in configuration
// that url.Parse's own error text would quote straight back.
func TestNewPanicsWithoutQuotingTheBaseURL(t *testing.T) {
	const endpointWithCredentials = "https://user:pass@api.sepay.example"

	message := panicMessage(t, func() {
		New(endpointWithCredentials, testAPIToken(), testOrderSettings(), time.Second)
	})
	if strings.Contains(message, "user:pass") {
		t.Errorf("the panic message %q quotes the base URL, and a base URL can carry credentials", message)
	}
	if strings.Contains(message, testAPIToken()) {
		t.Errorf("the panic message %q quotes the API token", message)
	}
}
