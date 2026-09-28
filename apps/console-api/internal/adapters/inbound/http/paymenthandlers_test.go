package http

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	stdhttp "net/http"
	"net/http/httptest"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	paymentport "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/payments"
)

// The payment surface's tests, and the two fakes they run on.
//
// The fakes live in this file for the reason the session and read fakes live in
// theirs: a fake of a narrow seam needs no cluster, no provider and no signing
// secret, and it is what keeps these tests in CI's unit tier. Both are recorded
// rather than merely answered — every call is appended to a list a test reads
// back — because the assertions that matter on this surface are about what a
// handler ASKED FOR (the account came from the session, the bytes were the ones
// verified, the delivery never reached the application) and not only about the
// status it wrote.

// The compile-time assertions that keep the fakes honest: a seam that grew a
// method would fail to build here rather than pass a test that no longer
// exercises it.
var (
	_ PaymentUseCases = (*fakePaymentUseCases)(nil)
	_ WebhookVerifier = (*fakeWebhookVerifier)(nil)
)

// checkoutCall is one BeginCheckout as it reached the seam. The three fields are
// the whole request, and the account is its own field because "the account came
// from the session" is the claim this fake exists to make.
type checkoutCall struct {
	AccountID      string
	OfferID        string
	IdempotencyKey string
}

// paymentPageCall is one ListPayments as it reached the seam: whose account,
// which cursor and which bound.
type paymentPageCall struct {
	AccountID string
	After     string
	Limit     int
}

type fakePaymentUseCases struct {
	mu sync.Mutex

	// The configured answers. A zero-value fake answers nothing useful, which is
	// why every test starts from newFakePayments.
	checkout        PaymentIntentResult
	checkoutErr     error
	appliedOutcome  WebhookOutcome
	applyErr        error
	paymentsPage    PaymentPageResult
	paymentsPageErr error
	offers          TopUpOfferListResult
	offersErr       error

	// The calls received, in order.
	checkoutCalls    []checkoutCall
	applyCalls       []WebhookDelivery
	paymentsPageCall []paymentPageCall
	offersCallCount  int
}

func (f *fakePaymentUseCases) BeginCheckout(_ context.Context, accountID, offerID, idempotencyKey string) (PaymentIntentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkoutCalls = append(f.checkoutCalls, checkoutCall{AccountID: accountID, OfferID: offerID, IdempotencyKey: idempotencyKey})
	if f.checkoutErr != nil {
		return PaymentIntentResult{}, f.checkoutErr
	}
	return f.checkout, nil
}

func (f *fakePaymentUseCases) ApplyProviderEvent(_ context.Context, delivery WebhookDelivery) (WebhookOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applyCalls = append(f.applyCalls, delivery)
	if f.applyErr != nil {
		return WebhookOutcome{}, f.applyErr
	}
	return f.appliedOutcome, nil
}

func (f *fakePaymentUseCases) ListPayments(_ context.Context, accountID, after string, limit int) (PaymentPageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paymentsPageCall = append(f.paymentsPageCall, paymentPageCall{AccountID: accountID, After: after, Limit: limit})
	if f.paymentsPageErr != nil {
		return PaymentPageResult{}, f.paymentsPageErr
	}
	return f.paymentsPage, nil
}

func (f *fakePaymentUseCases) ListTopUpOffers(_ context.Context) (TopUpOfferListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offersCallCount++
	if f.offersErr != nil {
		return TopUpOfferListResult{}, f.offersErr
	}
	return f.offers, nil
}

// The accessors copy under the lock, so a test reads a consistent list and the
// handler goroutine never shares the slice with the assertion.
func (f *fakePaymentUseCases) checkouts() []checkoutCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]checkoutCall(nil), f.checkoutCalls...)
}

func (f *fakePaymentUseCases) deliveries() []WebhookDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]WebhookDelivery(nil), f.applyCalls...)
}

func (f *fakePaymentUseCases) paymentPages() []paymentPageCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]paymentPageCall(nil), f.paymentsPageCall...)
}

func (f *fakePaymentUseCases) offerCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.offersCallCount
}

// verifyCall is one Verify as it reached the seam: the whole header map with its
// multiplicity, and the exact slice the handler read.
type verifyCall struct {
	Headers map[string][]string
	RawBody []byte
}

type fakeWebhookVerifier struct {
	mu sync.Mutex

	event VerifiedEvent
	err   error

	calls []verifyCall
}

func (f *fakeWebhookVerifier) Verify(headers map[string][]string, rawBody []byte) (VerifiedEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, verifyCall{Headers: headers, RawBody: rawBody})
	if f.err != nil {
		return VerifiedEvent{}, f.err
	}
	return f.event, nil
}

func (f *fakeWebhookVerifier) verifications() []verifyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]verifyCall(nil), f.calls...)
}

// livePaymentIntent is a payment in the one state the fixture's checkout URL
// belongs to. The amount is 500 minor units in a currency with a THOUSAND of
// them — the shape a client would multiply out, which is the reason the exponent
// travels beside the payment rather than being assumed.
//
// The exponent is deliberately not 2. A fixture priced in a hundred-unit
// currency is the one a render that wrote a literal `2` would pass, and the
// field exists precisely so a reader does not have to know which currency it is
// looking at; three decimal places is a real currency (KWD, BHD) and is the
// smallest fixture that makes the distinction visible.
func livePaymentIntent() PaymentIntentRecord {
	return PaymentIntentRecord{
		ID:                "44444444-4444-4444-8444-444444444444",
		Status:            "created",
		AmountMinorUnits:  500,
		Currency:          "KWD",
		MinorUnitExponent: 3,
		CheckoutURL:       "https://checkout.example/session/abc",
		CreatedAt:         fixtureTime,
		ExpiresAt:         fixtureTime,
	}
}

// liveTopUpOffers is a deployment's price list as the application would hand it
// over, and each of the three axes on which its two rows differ is deliberate.
//
// Declaration order is the deployment's own, because that ordering is the one
// lever an operator has over how their price list reads.
//
// The first carries a label and the second carries none, because the contract
// makes `label` optional and the omitted case is the one a render that always
// wrote the field would get wrong.
//
// And the two are in DIFFERENT currencies with DIFFERENT exponents, which is
// the axis a fixture of two USD offers cannot test at all: with every row priced
// the same way, a render that wrote its own `USD` and its own `2` passes every
// assertion this file makes. A yen offer is exponent 0 and has no minor unit at
// all, so a client that assumed hundredths would read it as a hundredth of one.
func liveTopUpOffers() []TopUpOfferRecord {
	return []TopUpOfferRecord{
		{ID: "starter", AmountMinorUnits: 500, Currency: "USD", MinorUnitExponent: 2, Label: "Starter"},
		{ID: "operators", AmountMinorUnits: 50000, Currency: "JPY", MinorUnitExponent: 0},
	}
}

// paymentSessionAccount is the account the live session resolves to. It is read
// off the session fixture rather than written out again, so a test that asserts
// "the account came from the session" cannot pass against a stale literal.
func paymentSessionAccount() string { return liveSessionResult().Principal.AccountID }

func newFakePayments() *fakePaymentUseCases {
	return &fakePaymentUseCases{
		checkout:     PaymentIntentResult{Intent: livePaymentIntent()},
		paymentsPage: PaymentPageResult{Items: []PaymentIntentRecord{livePaymentIntent()}},
		offers:       TopUpOfferListResult{Items: liveTopUpOffers()},
	}
}

func newFakeWebhookVerifier() *fakeWebhookVerifier {
	amount := int64(500)
	return &fakeWebhookVerifier{event: VerifiedEvent{
		Kind:             "payment.succeeded",
		EventID:          "evt-1",
		CheckoutRef:      "cs_1",
		PaymentRef:       "pi_1",
		AmountMinorUnits: &amount,
		Currency:         "USD",
		OccurredAt:       time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC),
	}}
}

// newPaymentServer builds the real handler over these fakes: the whole surface,
// so the guards, the error translation and the route table are the ones a
// deployment serves rather than a handler assembled for the test.
//
// Provider is set because a WIRED surface has to name one — New panics on an
// empty name, since the delivery path is addressed by it — and the name matches
// the path webhookRequest posts to. A test that left it empty would not be
// testing an unwired deployment; it would be testing a surface that cannot
// exist. For the genuinely unwired shape there is a route of its own — the
// four-argument constructor, which builds unwiredPaymentSurface itself.
func newPaymentServer(payments *fakePaymentUseCases, verifier *fakeWebhookVerifier) stdhttp.Handler {
	return New(application.New("test"), &answeringPinger{}, newFakeSessionUseCases(),
		newFakeConsoleReadUseCases(), stubUsage(), PaymentSurface{
			Payments: payments,
			Verifier: verifier,
			Provider: "stripe",
		})
}

// webhookRequest is one provider delivery as a provider's server sends it: no
// cookie, no Origin, no CSRF token, a JSON content type and a signature header.
// That absence is the point — the route's class says a browser's guards must not
// be applied to it, and this is the shape that proves they are not.
func webhookRequest(body string) *stdhttp.Request {
	req := httptest.NewRequest(stdhttp.MethodPost, "/payment-webhooks/stripe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Payment-Signature", "t=1759000000,v1=deadbeef")
	req.Header.Set(RequestIDHeader, "webhook-request")
	return req
}

func deliver(handler stdhttp.Handler, req *stdhttp.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// captureLog runs one request with the process log redirected, so a test can
// assert what an operator would read. The lines on these paths are the ONLY
// record of most refusals — nothing is stored for an unauthenticated body or for
// a delivery with no readable event id — so the log line is a behaviour and not
// a decoration.
func captureLog(t *testing.T, run func()) string {
	t.Helper()
	var captured bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&captured)
	defer log.SetOutput(previous)
	run()
	return captured.String()
}

// errorCode reads the envelope's code, which is the part of a refusal a client
// branches on and the part these tests assert. The message is asserted where the
// contract promises WHICH one it was.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	envelope, ok := decode(t, rec)["error"].(map[string]any)
	if !ok {
		t.Fatalf("the body carries no error envelope: %q", rec.Body.String())
	}
	code, _ := envelope["code"].(string)
	return code
}

// TestTheWebhookRefusesAnUnauthenticatedDeliveryWithoutWritingAnything is the
// first of the 400's two meanings, and the assertion is as much about what did
// NOT happen as about the status.
//
// An unverified body is an attacker's free text: a quarantine row keyed on it
// would make that table writable by anyone who can reach this port, and would do
// it with a payload this build never authenticated. So the seam that writes is
// never reached, and a test that only checked the 400 would not notice.
func TestTheWebhookRefusesAnUnauthenticatedDeliveryWithoutWritingAnything(t *testing.T) {
	payments := newFakePayments()
	verifier := newFakeWebhookVerifier()
	verifier.err = paymentport.ErrBadSignature
	handler := newPaymentServer(payments, verifier)

	rec := deliver(handler, webhookRequest(`{"id":"evt-1"}`))

	if rec.Code != stdhttp.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != string(application.CodeInvalidRequest) {
		t.Errorf("code = %q, want %q", got, application.CodeInvalidRequest)
	}
	if got := len(payments.deliveries()); got != 0 {
		t.Errorf("the application was handed %d deliveries for a body that did not authenticate; want none", got)
	}
	if got := len(verifier.verifications()); got != 1 {
		t.Errorf("the verifier was asked %d times, want once; the signature is the only authentication here", got)
	}
}

// TestTheWebhookRefusesAStaleDeliveryWithItsOwnLogLine is the 400's second
// meaning, and the reason the two are separate branches rather than one: both
// are permanently unusable, and an operator has to be able to tell a forged
// delivery from a replayed one.
//
// The two log lines are compared to each other as well as each to its own
// subject, because "the stale line exists" would pass for a branch that logged
// the signature message twice.
func TestTheWebhookRefusesAStaleDeliveryWithItsOwnLogLine(t *testing.T) {
	forged := newFakePayments()
	forgedVerifier := newFakeWebhookVerifier()
	forgedVerifier.err = paymentport.ErrBadSignature
	var forgedRec *httptest.ResponseRecorder
	forgedLog := captureLog(t, func() {
		forgedRec = deliver(newPaymentServer(forged, forgedVerifier), webhookRequest(`{"id":"evt-1"}`))
	})

	stale := newFakePayments()
	staleVerifier := newFakeWebhookVerifier()
	staleVerifier.err = ErrStaleDelivery
	var staleRec *httptest.ResponseRecorder
	staleLog := captureLog(t, func() {
		staleRec = deliver(newPaymentServer(stale, staleVerifier), webhookRequest(`{"id":"evt-1"}`))
	})

	for name, rec := range map[string]*httptest.ResponseRecorder{"a forged delivery": forgedRec, "a stale delivery": staleRec} {
		if rec.Code != stdhttp.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; a redelivery carries the same signature or the same timestamp, so neither becomes usable on a retry", name, rec.Code)
		}
	}
	if got := len(stale.deliveries()); got != 0 {
		t.Errorf("the application was handed %d deliveries for a stale body; want none", got)
	}
	if !strings.Contains(forgedLog, "did not authenticate") {
		t.Errorf("a forged delivery logged %q, which does not say it did not authenticate", forgedLog)
	}
	if !strings.Contains(staleLog, "freshness tolerance") {
		t.Errorf("a stale delivery logged %q, which does not name the freshness tolerance", staleLog)
	}
	if forgedLog == staleLog {
		t.Errorf("a forged delivery and a replayed one logged the same line %q; an operator cannot tell them apart", forgedLog)
	}
}

// TestTheWebhookAcknowledgesAnUnreadableDeliveryWithoutWritingAnything is the
// case that must NOT share the 400.
//
// A delivery this build authenticated and cannot read is a provider bug rather
// than an attack, and the row a quarantine would write is keyed by an event id
// that is absent: it would swallow the NEXT unreadable delivery as a duplicate.
// So the answer is 200 — the provider is told to stop sending these very bytes —
// and nothing is stored.
func TestTheWebhookAcknowledgesAnUnreadableDeliveryWithoutWritingAnything(t *testing.T) {
	payments := newFakePayments()
	verifier := newFakeWebhookVerifier()
	verifier.err = paymentport.ErrMalformedEvent
	handler := newPaymentServer(payments, verifier)

	var rec *httptest.ResponseRecorder
	logged := captureLog(t, func() {
		rec = deliver(handler, webhookRequest(`{"no":"event id"}`))
	})

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want 200; a 4xx here would put a permanent refusal into the provider's retry budget (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "{\"received\":true}\n" {
		t.Errorf("body = %q, want the same acknowledgement every 2xx carries", got)
	}
	if got := len(payments.deliveries()); got != 0 {
		t.Errorf("the application was handed %d deliveries for a body with no readable event id; want none", got)
	}
	if !strings.Contains(logged, "no readable event id") {
		t.Errorf("an unreadable delivery logged %q, which does not say why nothing was written", logged)
	}
}

// TestEveryDispositionOfARecordedDeliveryAnswersOneIdenticalBody is the
// contract's rule with no exceptions: 2xx means "this event needs no further
// delivery from you", so applied, duplicate and quarantined are one response.
//
// The bodies are compared to EACH OTHER rather than each to a literal, because
// the failure this test exists for is a handler that renders the disposition —
// which would leave every per-case assertion green and leak a decision the
// provider's implementation has no business branching on.
func TestEveryDispositionOfARecordedDeliveryAnswersOneIdenticalBody(t *testing.T) {
	outcomes := []WebhookOutcome{
		{Disposition: "applied", IntentID: "44444444-4444-4444-8444-444444444444"},
		{Disposition: "duplicate", IntentID: "44444444-4444-4444-8444-444444444444"},
		{Disposition: "quarantined", Reason: "an event kind this build has no rule for"},
	}

	bodies := map[string]string{}
	for _, outcome := range outcomes {
		payments := newFakePayments()
		payments.appliedOutcome = outcome
		verifier := newFakeWebhookVerifier()

		rec := deliver(newPaymentServer(payments, verifier), webhookRequest(`{"id":"evt-1"}`))
		if rec.Code != stdhttp.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %q)", outcome.Disposition, rec.Code, rec.Body.String())
		}
		if got := len(payments.deliveries()); got != 1 {
			t.Fatalf("%s: the application was handed %d deliveries, want exactly one", outcome.Disposition, got)
		}
		bodies[outcome.Disposition] = rec.Body.String()
	}

	for disposition, body := range bodies {
		if body != "{\"received\":true}\n" {
			t.Errorf("%s: body = %q, want one identical acknowledgement for every outcome", disposition, body)
		}
		for _, other := range outcomes {
			if other.Disposition == disposition {
				continue
			}
			if bodies[other.Disposition] != body {
				t.Errorf("applied, duplicate and quarantined deliveries answer differently (%q vs %q); a provider's implementation can branch on that",
					body, bodies[other.Disposition])
				break
			}
		}
		if strings.Contains(body, disposition) {
			t.Errorf("%s: the body names the disposition %q", disposition, disposition)
		}
	}
}

// TestTheWebhookAnswersAnUnclassifiedVerificationFailureWithARetryable500 pins
// the fourth branch: an error this transport cannot place is the one case in
// which nothing was recorded for a reason a redelivery could change.
func TestTheWebhookAnswersAnUnclassifiedVerificationFailureWithARetryable500(t *testing.T) {
	payments := newFakePayments()
	verifier := newFakeWebhookVerifier()
	verifier.err = errors.New("the signing secret could not be read")
	handler := newPaymentServer(payments, verifier)

	rec := deliver(handler, webhookRequest(`{"id":"evt-1"}`))

	if rec.Code != stdhttp.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; nothing was recorded and the provider's next attempt is the correct next step (body %q)", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != string(application.CodeInternal) {
		t.Errorf("code = %q, want %q", got, application.CodeInternal)
	}
	if got := len(payments.deliveries()); got != 0 {
		t.Errorf("the application was handed %d deliveries that were never verified; want none", got)
	}
}

// TestTheWebhookAnswersAnUnrecordableDeliveryWithARetryable500 is the last
// branch, and the only 5xx on this endpoint that a real delivery can produce:
// the event was verified, the application could not record it, and a redelivery
// is exactly what the provider should do.
func TestTheWebhookAnswersAnUnrecordableDeliveryWithARetryable500(t *testing.T) {
	payments := newFakePayments()
	payments.applyErr = application.Internal(errors.New("the intent store is not answering"))
	handler := newPaymentServer(payments, newFakeWebhookVerifier())

	rec := deliver(handler, webhookRequest(`{"id":"evt-1"}`))

	if rec.Code != stdhttp.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != string(application.CodeInternal) {
		t.Errorf("code = %q, want %q", got, application.CodeInternal)
	}
	if strings.Contains(rec.Body.String(), "the intent store is not answering") {
		t.Errorf("the operational cause reached the wire: %q", rec.Body.String())
	}
	if got := len(payments.deliveries()); got != 1 {
		t.Errorf("the application was handed %d deliveries, want exactly one — the bytes reached it and the recording failed", got)
	}
}

// TestTheWebhookVerifiesAndForwardsTheSameBytes is the assertion behind carrying
// the raw body on the seam.
//
// The identity check is on the SLICE, not on the bytes: two equal byte slices
// from two reads of a body would satisfy a comparison and still be the defect
// this is about — a body read twice is not necessarily the body that was
// verified, and a signature covers bytes rather than a message. Comparing the
// first element's address is what distinguishes "the same message" from "a
// message that happens to look like it".
func TestTheWebhookVerifiesAndForwardsTheSameBytes(t *testing.T) {
	payments := newFakePayments()
	verifier := newFakeWebhookVerifier()
	handler := newPaymentServer(payments, verifier)

	const body = `{"id":"evt-1","amount":500}`
	deliver(handler, webhookRequest(body))

	verified := verifier.verifications()
	if len(verified) != 1 {
		t.Fatalf("the verifier was asked %d times, want once", len(verified))
	}
	if got := string(verified[0].RawBody); got != body {
		t.Fatalf("the verifier saw %q, want the bytes that were sent %q", got, body)
	}

	delivered := payments.deliveries()
	if len(delivered) != 1 {
		t.Fatalf("the application was handed %d deliveries, want exactly one", len(delivered))
	}
	if got := string(delivered[0].RawBody); got != body {
		t.Fatalf("the application was handed %q, want the bytes the provider sent %q", got, body)
	}
	if &verified[0].RawBody[0] != &delivered[0].RawBody[0] {
		t.Errorf("the bytes that were verified and the bytes that were interpreted are different slices; the application re-read or re-encoded the message")
	}
	if got := delivered[0].Event.EventID; got != "evt-1" {
		t.Errorf("the event that reached the application carries the id %q, want the verifier's answer", got)
	}
}

// TestTheWebhookBoundsTheBodyItReads pins the bound at both edges. A body of
// exactly the cap must be read whole — an off-by-one here refuses a legitimate
// delivery — and a body one byte over must be refused WITHOUT being read to its
// end, which is what makes the bound a bound.
//
// The refusal is a 400 rather than a 5xx: the provider's next attempt carries
// the same oversized bytes, so a retry could only repeat it.
func TestTheWebhookBoundsTheBodyItReads(t *testing.T) {
	atCap := newFakePayments()
	atCapVerifier := newFakeWebhookVerifier()
	rec := deliver(newPaymentServer(atCap, atCapVerifier), webhookRequest(strings.Repeat("a", maxWebhookBody)))

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("a body of exactly %d bytes: status = %d, want 200 (body %q)", maxWebhookBody, rec.Code, rec.Body.String())
	}
	if got := len(atCapVerifier.verifications()); got != 1 {
		t.Fatalf("a body of exactly %d bytes was verified %d times, want once", maxWebhookBody, got)
	}
	if got := len(atCapVerifier.verifications()[0].RawBody); got != maxWebhookBody {
		t.Errorf("the verifier saw %d bytes for a body of exactly %d", got, maxWebhookBody)
	}

	overCap := newFakePayments()
	overCapVerifier := newFakeWebhookVerifier()
	rec = deliver(newPaymentServer(overCap, overCapVerifier), webhookRequest(strings.Repeat("a", maxWebhookBody+1)))

	if rec.Code != stdhttp.StatusBadRequest {
		t.Fatalf("a body of %d bytes: status = %d, want 400 (body %q)", maxWebhookBody+1, rec.Code, rec.Body.String())
	}
	if got := len(overCapVerifier.verifications()); got != 0 {
		t.Errorf("an oversized body was handed to the verifier %d times; want none — a signature over a truncated body authenticates nothing", got)
	}
	if got := len(overCap.deliveries()); got != 0 {
		t.Errorf("an oversized body reached the application %d times; want none", got)
	}
}

// TestTheWebhookRefusesACompressedBodyBeforeReadingIt is the check that runs
// before a byte of the body is read.
//
// A signature covers bytes, and no provider signs the compressed form of what it
// sent: a build that decompressed first would be verifying a message it produced
// itself. Both header spellings are exercised because the check is over the
// header's values rather than its first one — `identity, gzip` is a gzip body.
func TestTheWebhookRefusesACompressedBodyBeforeReadingIt(t *testing.T) {
	for name, encodings := range map[string][]string{
		"gzip":              {"gzip"},
		"identity and gzip": {"identity", "gzip"},
	} {
		payments := newFakePayments()
		verifier := newFakeWebhookVerifier()
		req := webhookRequest(`{"id":"evt-1"}`)
		for _, encoding := range encodings {
			req.Header.Add("Content-Encoding", encoding)
		}

		var rec *httptest.ResponseRecorder
		logged := captureLog(t, func() { rec = deliver(newPaymentServer(payments, verifier), req) })

		if rec.Code != stdhttp.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %q)", name, rec.Code, rec.Body.String())
		}
		if got := len(verifier.verifications()); got != 0 {
			t.Errorf("%s: a compressed body was verified %d times; want none", name, got)
		}
		if !strings.Contains(logged, "Content-Encoding") {
			t.Errorf("%s: the refusal logged %q, which does not name the encoding", name, logged)
		}
	}

	// And the pair: `identity` is the spelling of "the body is the body", so a
	// delivery that declares it is admitted. Without this half the check above
	// would pass for a handler that refused every body with the header at all.
	payments := newFakePayments()
	verifier := newFakeWebhookVerifier()
	req := webhookRequest(`{"id":"evt-1"}`)
	req.Header.Set("Content-Encoding", "identity")

	if rec := deliver(newPaymentServer(payments, verifier), req); rec.Code != stdhttp.StatusOK {
		t.Errorf("a body declaring Content-Encoding: identity answered %d, want 200; the header's identity spelling is not a compression (body %q)", rec.Code, rec.Body.String())
	}
}

// TestTheWebhookRequiresABodyThatCanBeJSON is the content-type guard, which is
// deliberately LOOSE in one direction and closed in the other.
//
// `application/json; charset=utf-8` is what most providers send through most
// proxies, and a `+json` suffix is a legal spelling of a JSON body; refusing
// either would refuse deliveries whose bytes are exactly what was signed. A
// media type that cannot be JSON at all is refused instead, because a signature
// over such a body is a signature over something this build will not interpret.
func TestTheWebhookRequiresABodyThatCanBeJSON(t *testing.T) {
	cases := []struct {
		contentType string
		admitted    bool
	}{
		{"", true}, // a delivery that declared nothing declared nothing wrong
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"application/vnd.stripe+json", true},
		{"text/plain", false},
		{"text/html", false},
		{"; charset=utf-8", false},
	}

	for _, tc := range cases {
		name := tc.contentType
		if name == "" {
			name = "no content type"
		}
		t.Run(name, func(t *testing.T) {
			payments := newFakePayments()
			verifier := newFakeWebhookVerifier()
			req := webhookRequest(`{"id":"evt-1"}`)
			req.Header.Del("Content-Type")
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}

			rec := deliver(newPaymentServer(payments, verifier), req)

			if tc.admitted {
				if rec.Code != stdhttp.StatusOK {
					t.Fatalf("Content-Type %q: status = %d, want 200 (body %q)", tc.contentType, rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != stdhttp.StatusBadRequest {
				t.Fatalf("Content-Type %q: status = %d, want 400 (body %q)", tc.contentType, rec.Code, rec.Body.String())
			}
			if got := len(verifier.verifications()); got != 0 {
				t.Errorf("Content-Type %q was verified %d times; want none", tc.contentType, got)
			}
		})
	}
}

// TestTheWebhookReadsEveryDeclaredContentTypeAndNotOnlyTheFirst is the
// multiplicity half of the rule above, and it is the same rule the signature and
// the content encoding already follow.
//
// `Header.Get` answers the first value, so a delivery declaring
// `application/json` and then `text/plain` would be admitted by a check that
// read only the first — the reader would be picking a winner between two
// incompatible claims instead of noticing that there are two. Every declared
// value is checked; a delivery that contradicts itself is refused.
func TestTheWebhookReadsEveryDeclaredContentTypeAndNotOnlyTheFirst(t *testing.T) {
	payments := newFakePayments()
	verifier := newFakeWebhookVerifier()
	req := webhookRequest(`{"id":"evt-1"}`)
	// Set then Add: two values, the first of which on its own would be admitted.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add("Content-Type", "text/plain")

	var rec *httptest.ResponseRecorder
	logged := captureLog(t, func() {
		rec = deliver(newPaymentServer(payments, verifier), req)
	})

	if rec.Code != stdhttp.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if got := len(verifier.verifications()); got != 0 {
		t.Errorf("a delivery declaring two content types was verified %d times; want none", got)
	}
	if got := len(payments.deliveries()); got != 0 {
		t.Errorf("a delivery declaring two content types reached the use case %d times; want none", got)
	}
	// And the line NAMES what was refused. The refusal ran on the SECOND
	// declared value, so a line quoting only the first — which is what
	// `Header.Get` returns, and what this line used to write — would read as
	// this endpoint refusing `application/json`, the one type it accepts. An
	// operator following that line looks for a bug that is not there.
	if !strings.Contains(logged, "application/json") || !strings.Contains(logged, "text/plain") {
		t.Errorf("the log line reads %q, want both declared values named; the value the check refused on is the second one", logged)
	}
}

// TestTheWebhookHandsTheVerifierEveryHeaderValue is what makes "the signature
// header appears twice is a refusal" implementable at all.
//
// A verifier handed a single joined string could not tell one header from two,
// and accepting "the first of several" is how a smuggled value wins. The whole
// map travels, multiplicity intact, and the copy the handler makes is what keeps
// a verifier from editing live request state.
func TestTheWebhookHandsTheVerifierEveryHeaderValue(t *testing.T) {
	payments := newFakePayments()
	verifier := newFakeWebhookVerifier()
	req := webhookRequest(`{"id":"evt-1"}`)
	req.Header.Add("X-Payment-Signature", "t=1759000000,v1=second")

	deliver(newPaymentServer(payments, verifier), req)

	verifications := verifier.verifications()
	if len(verifications) != 1 {
		t.Fatalf("the verifier was asked %d times, want once", len(verifications))
	}
	got := verifications[0].Headers["X-Payment-Signature"]
	if len(got) != 2 {
		t.Fatalf("the verifier saw %d values for the signature header, want 2; a repeated signature is a refusal and the verifier is the only layer that can see it", len(got))
	}
	if got[0] != "t=1759000000,v1=deadbeef" || got[1] != "t=1759000000,v1=second" {
		t.Errorf("the verifier saw %q, want both values in order", got)
	}
}

// TestTheWebhookIsNotABrowserOperation is the route's class asserted from both
// ends: structurally, that its row declares guardServerToServer rather than
// inheriting the browser guards an unsafe method gets by default; and
// behaviourally, that a delivery shaped exactly like a cross-site browser
// request is still admitted.
//
// The second half is the one that would catch the class being lost, and it is
// the failure that is otherwise invisible: a provider refused by an origin check
// answers 403, and every test that only counted guarded routes would stay green.
func TestTheWebhookIsNotABrowserOperation(t *testing.T) {
	found := false
	for _, rt := range routeTableForTest() {
		if rt.method == stdhttp.MethodPost && rt.path == "/payment-webhooks/{provider}" {
			found = true
			if rt.guard != guardServerToServer {
				t.Errorf("the delivery endpoint carries guard %s, want %s; the browser guards would refuse every real delivery", rt.guard, guardServerToServer)
			}
		}
	}
	if !found {
		t.Fatalf("the delivery endpoint is not in the route table")
	}

	payments := newFakePayments()
	verifier := newFakeWebhookVerifier()
	req := webhookRequest(`{"id":"evt-1"}`)
	// Everything a browser would send that a provider's server never does: a
	// cross-site fetch, no cookie, no double-submit token.
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")

	rec := deliver(newPaymentServer(payments, verifier), req)

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("a cross-site-shaped delivery answered %d, want 200; the browser guards must not reach this row (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("the delivery endpoint answered Access-Control-Allow-Origin: %q; a browser has no business reading this response", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("the delivery endpoint answered Access-Control-Allow-Credentials: %q, want nothing", got)
	}
	if got := rec.Header().Get(RequestIDHeader); got == "" {
		t.Errorf("the response carries no %s; a delivery leaves no row on most refusal paths, so the identifier is how an operator finds its log line", RequestIDHeader)
	}
}

// TestBeginCheckoutTakesTheAccountFromTheSession is ADR 0012 §2 as a signature.
//
// The request names another account in its query and in a body field, and the
// assertion is about what reached the use case: the session's account. A payment
// is money, and "which account funds itself" is the last question a browser
// should be able to answer about one.
func TestBeginCheckoutTakesTheAccountFromTheSession(t *testing.T) {
	payments := newFakePayments()
	handler := newPaymentServer(payments, newFakeWebhookVerifier())
	foreign := "99999999-9999-4999-8999-999999999999"

	rec := callOn(handler, stdhttp.MethodPost,
		"/payment-intents?account_id="+foreign,
		`{"offer":"starter","idempotency_key":"topup-1","account_id":"`+foreign+`"}`,
		requestOptions{})

	if rec.Code != stdhttp.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	calls := payments.checkouts()
	if len(calls) != 1 {
		t.Fatalf("the use case was called %d times, want once", len(calls))
	}
	if calls[0].AccountID != paymentSessionAccount() {
		t.Errorf("the account that reached the use case was %q, want the session's %q; no query parameter and no body field may replace it",
			calls[0].AccountID, paymentSessionAccount())
	}
	if calls[0].OfferID != "starter" {
		t.Errorf("the offer forwarded was %q, want %q — the request names an offer and never an amount", calls[0].OfferID, "starter")
	}
	if calls[0].IdempotencyKey != "topup-1" {
		t.Errorf("the idempotency key forwarded was %q, want %q", calls[0].IdempotencyKey, "topup-1")
	}
}

// TestBeginCheckoutRendersThePaymentAndConverges asserts the wire shape on both
// halves of the 201: the first call and the converged repeat are the same
// response.
//
// A repeat that rendered "this already existed" differently would be the first
// place a page could branch on which of the two happened — and the contract
// states that a converged answer may carry a payment in a LATER state, so a
// client that rendered a checkout link from the first call's answer would be
// sending a customer to pay for something they already paid for.
func TestBeginCheckoutRendersThePaymentAndConverges(t *testing.T) {
	payments := newFakePayments()
	handler := newPaymentServer(payments, newFakeWebhookVerifier())

	first := callOn(handler, stdhttp.MethodPost, "/payment-intents", `{"offer":"starter","idempotency_key":"topup-1"}`, requestOptions{})
	if first.Code != stdhttp.StatusCreated {
		t.Fatalf("the first call answered %d, want 201 (body %q)", first.Code, first.Body.String())
	}

	payments.mu.Lock()
	payments.checkout = PaymentIntentResult{Intent: livePaymentIntent(), Converged: true}
	payments.mu.Unlock()

	repeat := callOn(handler, stdhttp.MethodPost, "/payment-intents", `{"offer":"starter","idempotency_key":"topup-1"}`, requestOptions{})
	if repeat.Code != stdhttp.StatusCreated {
		t.Fatalf("the converged repeat answered %d, want the same 201 (body %q)", repeat.Code, repeat.Body.String())
	}
	if first.Body.String() != repeat.Body.String() {
		t.Errorf("the first call answered %q and the converged repeat %q; which of the two happened is not a difference a page may act on",
			first.Body.String(), repeat.Body.String())
	}

	body := decode(t, first)
	want := []string{"amount_minor_units", "checkout_url", "created_at", "currency", "expires_at", "id", "minor_unit_exponent", "status"}
	got := topLevelKeys(body)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the payment carries %v, want exactly %v", got, want)
	}
	if body["status"] != "created" {
		t.Errorf("status = %#v, want the state the application recorded", body["status"])
	}
	// The exponent crosses untouched, and the fixture's is 3 rather than the
	// 2 a currency with a hundred minor units has: a render that wrote its own
	// constant, or that dropped the field as "the obvious default", is what
	// this asserts against — the amount is meaningless without it.
	if body["minor_unit_exponent"] != float64(3) {
		t.Errorf("minor_unit_exponent = %#v, want 3; the amount is integer minor units and this is the only thing that says how many of them make one unit",
			body["minor_unit_exponent"])
	}
	if body["checkout_url"] != "https://checkout.example/session/abc" {
		t.Errorf("checkout_url = %#v, want the URL the application stored", body["checkout_url"])
	}

	// And the pair: a payment with no checkout yet renders an explicit null
	// rather than an empty string. The schema types the field `[string, "null"]`
	// and null is the value of a payment whose checkout does not exist — the two
	// are different values on the wire and a client acts differently on them.
	payments.mu.Lock()
	absent := livePaymentIntent()
	absent.CheckoutURL = ""
	payments.checkout = PaymentIntentResult{Intent: absent}
	payments.mu.Unlock()

	fresh := callOn(handler, stdhttp.MethodPost, "/payment-intents", `{"offer":"starter","idempotency_key":"topup-2"}`, requestOptions{})
	if value, present := decode(t, fresh)["checkout_url"]; !present || value != nil {
		t.Errorf("a payment with no checkout answered checkout_url = %#v, want an explicit null", value)
	}
}

// TestBeginCheckoutIsGuardedLikeEveryOtherBrowserWrite asserts the derived guard
// reaches the new write. The row is a POST and inherits the origin, content-type
// and double-submit guards by existing; this is the request that would otherwise
// open payments against a victim's account from a cross-origin page.
func TestBeginCheckoutIsGuardedLikeEveryOtherBrowserWrite(t *testing.T) {
	payments := newFakePayments()
	handler := newPaymentServer(payments, newFakeWebhookVerifier())
	const body = `{"offer":"starter","idempotency_key":"topup-1"}`

	rec := callOn(handler, stdhttp.MethodPost, "/payment-intents", body, requestOptions{crossSite: true})
	if rec.Code != stdhttp.StatusForbidden {
		t.Fatalf("a cross-site top-up answered %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if got := len(payments.checkouts()); got != 0 {
		t.Errorf("a cross-site top-up reached the use case %d times; want none", got)
	}

	// The pair: the same request with the three guards' evidence restored is
	// admitted, so the refusal above is about the cross-site request rather than
	// about a door welded shut.
	if rec := callOn(handler, stdhttp.MethodPost, "/payment-intents", body, requestOptions{}); rec.Code != stdhttp.StatusCreated {
		t.Errorf("the same top-up from this console answered %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
}

// TestBeginCheckoutRefusesWhatTheTransportCannotReadAndNothingMore pins the
// boundary of this layer's judgement.
//
// An offer's vocabulary is the deployment's configuration and an idempotency
// key's meaning belongs to the application, so a blank one is NOT refused here —
// the layer that knows what either should have been is the layer that refuses,
// with a message naming it. A body that is not a well-formed JSON object is the
// transport's own refusal, and it never reaches the use case.
func TestBeginCheckoutRefusesWhatTheTransportCannotReadAndNothingMore(t *testing.T) {
	payments := newFakePayments()
	handler := newPaymentServer(payments, newFakeWebhookVerifier())

	rec := callOn(handler, stdhttp.MethodPost, "/payment-intents", `{"offer":`, requestOptions{})
	if rec.Code != stdhttp.StatusBadRequest {
		t.Fatalf("a malformed body answered %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if got := len(payments.checkouts()); got != 0 {
		t.Errorf("a malformed body reached the use case %d times; want none", got)
	}

	if rec := callOn(handler, stdhttp.MethodPost, "/payment-intents", `{}`, requestOptions{}); rec.Code != stdhttp.StatusCreated {
		t.Fatalf("an empty top-up request answered %d, want 201; the transport does not validate the offer's vocabulary or the key's meaning (body %q)", rec.Code, rec.Body.String())
	}
	calls := payments.checkouts()
	if len(calls) != 1 {
		t.Fatalf("the use case was called %d times, want exactly once — only the well-formed empty body reached it", len(calls))
	}
	if calls[0].OfferID != "" || calls[0].IdempotencyKey != "" {
		t.Errorf("an empty request reached the use case as (%q, %q), want the empty strings it sent; refusing it here would be this layer inventing a vocabulary",
			calls[0].OfferID, calls[0].IdempotencyKey)
	}
}

// TestListPaymentsForwardsTheSessionAccountAndThePaging is the account predicate
// and the cursor, which are the two things this handler is allowed to forward.
//
// A query parameter claiming another account is ignored — the WHERE clause is
// the session's, as the contract states — and the cursor travels untouched
// because this transport never decodes one.
func TestListPaymentsForwardsTheSessionAccountAndThePaging(t *testing.T) {
	payments := newFakePayments()
	payments.paymentsPage = PaymentPageResult{
		Items:      []PaymentIntentRecord{livePaymentIntent()},
		HasMore:    true,
		NextCursor: "44444444-4444-4444-8444-444444444444",
	}
	handler := newPaymentServer(payments, newFakeWebhookVerifier())
	foreign := "99999999-9999-4999-8999-999999999999"

	rec := callOn(handler, stdhttp.MethodGet, "/payment-intents?limit=2&after=cursor-9&account_id="+foreign, "", requestOptions{})
	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	calls := payments.paymentPages()
	if len(calls) != 1 {
		t.Fatalf("the use case was called %d times, want once", len(calls))
	}
	if calls[0].AccountID != paymentSessionAccount() {
		t.Errorf("the account forwarded was %q, want the session's %q; a query parameter may not replace it", calls[0].AccountID, paymentSessionAccount())
	}
	if calls[0].After != "cursor-9" || calls[0].Limit != 2 {
		t.Errorf("the paging forwarded was (after %q, limit %d), want (after %q, limit 2)", calls[0].After, calls[0].Limit, "cursor-9")
	}

	body := decode(t, rec)
	want := []string{"has_more", "items", "next_cursor"}
	if got := topLevelKeys(body); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the page carries %v, want exactly %v", got, want)
	}
	if body["next_cursor"] != "44444444-4444-4444-8444-444444444444" {
		t.Errorf("next_cursor = %#v, want the cursor the application resolved", body["next_cursor"])
	}
	if body["has_more"] != true {
		t.Errorf("has_more = %#v, want the application's answer", body["has_more"])
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want the one payment the fake answered", body["items"])
	}
}

// TestListPaymentsRefusesAMalformedLimitWithoutCallingTheSeam is the 400 the
// contract promises for a page size that is not a whole number.
//
// It is asserted here and not only in the shared paging tests because the
// refusal must arrive before the account is used: a handler that called the use
// case and then refused would have read an account's rows to answer a request it
// had already decided was malformed.
func TestListPaymentsRefusesAMalformedLimitWithoutCallingTheSeam(t *testing.T) {
	payments := newFakePayments()
	handler := newPaymentServer(payments, newFakeWebhookVerifier())

	rec := callOn(handler, stdhttp.MethodGet, "/payment-intents?limit=abc", "", requestOptions{})

	if rec.Code != stdhttp.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != string(application.CodeInvalidRequest) {
		t.Errorf("code = %q, want %q", got, application.CodeInvalidRequest)
	}
	if got := len(payments.paymentPages()); got != 0 {
		t.Errorf("a malformed page size reached the use case %d times; want none", got)
	}
}

// TestListTopUpOffersRendersTheWholePriceListAndNothingElse is the read that is
// not a page and not account-scoped.
//
// It answers `{items: [...]}` with no cursor, no total and no `has_more`, in the
// deployment's own order — the order an operator declared, which is the one
// lever they have over how their price list reads. It takes no account, which is
// not an omission: an offer is a price this deployment decided rather than a
// fact about a customer, and it is still behind a session, because publishing a
// price list to anonymous callers is not a decision this surface has made.
func TestListTopUpOffersRendersTheWholePriceListAndNothingElse(t *testing.T) {
	payments := newFakePayments()
	handler := newPaymentServer(payments, newFakeWebhookVerifier())

	// The paging parameters are sent and ignored: a price list has no position
	// for a cursor to name, and a handler that forwarded them would be promising
	// a page the schema does not have.
	rec := callOn(handler, stdhttp.MethodGet, "/top-up-offers?limit=1&after=cursor-9", "", requestOptions{})
	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := payments.offerCalls(); got != 1 {
		t.Fatalf("the use case was called %d times, want once", got)
	}

	body := decode(t, rec)
	if got := topLevelKeys(body); strings.Join(got, ",") != "items" {
		t.Errorf("the price list carries %v, want exactly [items]; a cursor names a position and a fixed price list has none", got)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items = %#v, want an array", body["items"])
	}
	if len(items) != 2 {
		t.Fatalf("items carries %d offers, want the 2 the deployment declared", len(items))
	}
	first, _ := items[0].(map[string]any)
	second, _ := items[1].(map[string]any)
	if first["id"] != "starter" || second["id"] != "operators" {
		t.Errorf("the offers are rendered as %v and %v, want the order the deployment declared them in", first["id"], second["id"])
	}
	if first["label"] != "Starter" {
		t.Errorf("label = %#v, want the label the deployment declared", first["label"])
	}
	if _, present := second["label"]; present {
		t.Errorf("an offer with no label rendered one anyway: %#v; the schema makes label optional and an empty string is a name, not an absence", second["label"])
	}
	if first["amount_minor_units"] != float64(500) || first["currency"] != "USD" || first["minor_unit_exponent"] != float64(2) {
		t.Errorf("the price rendered as %v %v with exponent %v, want 500 USD with exponent 2", first["amount_minor_units"], first["currency"], first["minor_unit_exponent"])
	}
	// The second row is what makes the pair an assertion rather than a
	// coincidence: it is priced in a currency with NO minor unit, so a render
	// that echoed a constant would have to echo two different constants to pass
	// both rows, and every field is copied from the deployment's own record.
	if second["amount_minor_units"] != float64(50000) || second["currency"] != "JPY" || second["minor_unit_exponent"] != float64(0) {
		t.Errorf("the second price rendered as %v %v with exponent %v, want 50000 JPY with exponent 0", second["amount_minor_units"], second["currency"], second["minor_unit_exponent"])
	}

	// And the pair for the session: the same read without a cookie is a 401 and
	// never reaches the use case.
	payments.mu.Lock()
	payments.offersCallCount = 0
	payments.mu.Unlock()

	rec = callOn(handler, stdhttp.MethodGet, "/top-up-offers", "", requestOptions{noSession: true})
	if rec.Code != stdhttp.StatusUnauthorized {
		t.Fatalf("an anonymous price list answered %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}
	if got := payments.offerCalls(); got != 0 {
		t.Errorf("an anonymous request reached the use case %d times; want none", got)
	}
}

// TestAnEmptyPriceListIsAnEmptyArrayNotAnUnavailableSurface is the deployment
// that sells nothing.
//
// A catalogue with no offers is legal configuration and a process with one must
// still start, so this route answers 200 with an empty array — not a 500, and
// not `null`. `unwiredPaymentSurface` is for a process that wired no payments at
// all, which is a different thing from a deployment that configured none.
func TestAnEmptyPriceListIsAnEmptyArrayNotAnUnavailableSurface(t *testing.T) {
	payments := newFakePayments()
	payments.offers = TopUpOfferListResult{}
	handler := newPaymentServer(payments, newFakeWebhookVerifier())

	var rec *httptest.ResponseRecorder
	logged := captureLog(t, func() {
		rec = callOn(handler, stdhttp.MethodGet, "/top-up-offers", "", requestOptions{})
	})

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("an empty price list answered %d, want 200 (body %q, log %q)", rec.Code, rec.Body.String(), logged)
	}
	if got := rec.Body.String(); got != "{\"items\":[]}\n" {
		t.Errorf("an empty price list answered %q, want an empty array; null is not an array", got)
	}
}

// TestEveryApplicationCodeHasAStatus is the test server.go's errorResponse names
// in its own comment, and it is the closure between the application's code
// vocabulary and the transport's status table.
//
// The codes are SCANNED from internal/application's source rather than listed
// here, and the direction that matters is the failing one: a code added to the
// application without an arm in applicationResponse would otherwise answer 500
// `internal` through the default branch — a server fault for a decision the
// application made deliberately, which is the exact defect the 401 and the 400
// arms were added to fix. A list on both sides could only catch that by someone
// remembering to edit two files; the scan cannot be forgotten.
func TestEveryApplicationCodeHasAStatus(t *testing.T) {
	want := map[application.Code]int{
		application.CodeNotFound:            stdhttp.StatusNotFound,
		application.CodeInvalidRequest:      stdhttp.StatusBadRequest,
		application.CodeUnauthenticated:     stdhttp.StatusUnauthorized,
		application.CodeConflict:            stdhttp.StatusConflict,
		application.CodeInternal:            stdhttp.StatusInternalServerError,
		application.CodeUpstreamUnavailable: stdhttp.StatusServiceUnavailable,
	}

	declared := declaredApplicationCodes(t)
	if len(declared) < len(want) {
		t.Fatalf("the scan found %d application codes (%v) and this test expects at least %d; every assertion below would pass vacuously",
			len(declared), declared, len(want))
	}
	seen := map[application.Code]bool{}
	for _, code := range declared {
		seen[code] = true
		status, known := want[code]
		if !known {
			t.Errorf("internal/application declares the code %q and errorResponse has no arm for it; add one and a status here", code)
			continue
		}
		gotStatus, gotCode, _ := applicationResponse(&application.Error{Code: code, Message: "the message"})
		if gotStatus != status {
			t.Errorf("the code %q maps to %d, want %d", code, gotStatus, status)
		}
		if gotCode != string(code) {
			t.Errorf("the code %q renders as %q on the wire, want the code itself", code, gotCode)
		}
	}
	for code := range want {
		if !seen[code] {
			t.Errorf("this test expects a status for %q, which internal/application no longer declares; the arm is dead", code)
		}
	}
}

// declaredApplicationCodes reads the codes out of internal/application's
// non-test source.
//
// The path is relative to this package's directory, which is how `go test` runs
// it — the same convention contract_test.go's contractPath uses. The three
// levels back are http, inbound and adapters.
//
// The scan is narrow on purpose — a declaration line whose name ends in Code and
// whose value is a quoted lower_snake string — because the alternative is a YAML
// or Go parser bought to assert a handful of constants, the same trade
// contract_test.go makes when it scans the OpenAPI document line by line. A
// declaration this pattern missed would fail the vacuity guard above rather than
// pass silently.
func declaredApplicationCodes(t *testing.T) []application.Code {
	t.Helper()
	const dir = "../../../application"

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	declaration := regexp.MustCompile(`^[ \t]*Code[A-Za-z]*[ \t]+Code[ \t]*=[ \t]*"([a-z_]+)"`)

	var codes []application.Code
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, line := range strings.Split(string(source), "\n") {
			if match := declaration.FindStringSubmatch(line); match != nil {
				codes = append(codes, application.Code(match[1]))
			}
		}
	}
	return codes
}

// TestAnUnrecognisedApplicationCodeIsLoggedAndAnswers500 is the default arm,
// which exists for the day the vocabulary grows.
//
// A code this build cannot place is not a refusal a caller could act on and not
// a success, so it is a 500 — and the log line is what makes it a wiring defect
// an operator reads rather than a server error a client reports.
func TestAnUnrecognisedApplicationCodeIsLoggedAndAnswers500(t *testing.T) {
	var status int
	var code string
	logged := captureLog(t, func() {
		status, code, _ = errorResponse(&application.Error{Code: application.Code("a_code_from_the_future"), Message: "unplaceable"})
	})

	if status != stdhttp.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
	if code != string(application.CodeInternal) {
		t.Errorf("code = %q, want %q; a code outside the contract's enum may not escape through it", code, application.CodeInternal)
	}
	if !strings.Contains(logged, "a_code_from_the_future") {
		t.Errorf("the refusal logged %q, which does not name the code that produced it", logged)
	}
}

// TestErrorMessagesFollowTheStatusTheyMapTo is the message policy, which is the
// half of the mapping a status test cannot see.
//
// A 4xx message is forwarded because the contract promises WHICH input was
// refused and the layer below has already reduced it to a statement that names
// no account. An internal cause is never forwarded and never serialized: a
// client can act on the status and the request identifier, and an operational
// cause is the operator's fact. An error that is neither an application error
// nor a transport error is the same answer as an unplaceable code.
func TestErrorMessagesFollowTheStatusTheyMapTo(t *testing.T) {
	const message = "the limit parameter is not a whole number of rows"

	for _, code := range []application.Code{application.CodeNotFound, application.CodeInvalidRequest, application.CodeUnauthenticated, application.CodeConflict} {
		_, _, got := applicationResponse(&application.Error{Code: code, Message: message})
		if got != message {
			t.Errorf("the code %q rendered the message %q, want the application's own %q", code, got, message)
		}
	}

	if _, _, got := applicationResponse(&application.Error{Code: application.CodeInternal}); got != internalErrorMessage {
		t.Errorf("an internal error rendered the message %q, want the fixed %q", got, internalErrorMessage)
	}

	// An operational cause leaking through the seam is the failure this asserts
	// against: the message must not carry it whichever branch produced it.
	status, code, got := errorResponse(errors.New("pq: relation \"payment_intents\" does not exist"))
	if status != stdhttp.StatusInternalServerError || code != string(application.CodeInternal) {
		t.Errorf("an unclassified error mapped to %d %s, want 500 %s", status, code, application.CodeInternal)
	}
	if strings.Contains(got, "payment_intents") {
		t.Errorf("the operational cause reached the message: %q", got)
	}

	// A transport error maps itself, and it does so ahead of the application's
	// vocabulary: the transport fact that no route matched is not an application
	// code, and the two mappings are separate tables for that reason.
	if status, code, _ := errorResponse(notFoundError{}); status != stdhttp.StatusNotFound || code != string(application.CodeNotFound) {
		t.Errorf("the transport's own not-found mapped to %d %s, want 404 %s", status, code, application.CodeNotFound)
	}
}

// TestAnUnwiredPaymentSurfaceFailsClosed is the decision behind the optional
// surface, asserted on all four rows.
//
// The four payment rows are part of the DECLARED surface — the contract test
// reads the route table and requires them — so a process that wired no payments
// still serves them. What it must not do is serve them as a 404 nobody can
// distinguish from a typo, or panic on the floor of a request goroutine where
// the stack names a request rather than the wiring: it answers a logged 500,
// which is the contract's own answer for "nothing was durably recorded".
func TestAnUnwiredPaymentSurfaceFailsClosed(t *testing.T) {
	// The four-argument constructor is the unwired surface, which is the form
	// the route-table and contract tests call.
	handler := New(application.New("test"), &answeringPinger{}, newFakeSessionUseCases(), newFakeConsoleReadUseCases(), stubUsage())

	cases := []struct {
		name    string
		method  string
		path    string
		body    string
		webhook bool
	}{
		{name: "begin a checkout", method: stdhttp.MethodPost, path: "/payment-intents", body: `{"offer":"starter","idempotency_key":"topup-1"}`},
		{name: "list the payments", method: stdhttp.MethodGet, path: "/payment-intents"},
		{name: "list the offers", method: stdhttp.MethodGet, path: "/top-up-offers"},
		{name: "receive a delivery", method: stdhttp.MethodPost, path: "/payment-webhooks/stripe", body: `{"id":"evt-1"}`, webhook: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec *httptest.ResponseRecorder
			logged := captureLog(t, func() {
				if tc.webhook {
					rec = deliver(handler, webhookRequest(tc.body))
					return
				}
				rec = callOn(handler, tc.method, tc.path, tc.body, requestOptions{})
			})

			if rec.Code != stdhttp.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; an unwired payment operation is a wiring defect and a logged refusal, not a 404 and not a panic (body %q)", rec.Code, rec.Body.String())
			}
			if got := errorCode(t, rec); got != string(application.CodeInternal) {
				t.Errorf("code = %q, want %q", got, application.CodeInternal)
			}
			if !strings.Contains(logged, "not wired") {
				t.Errorf("the refusal logged %q, which does not name the wiring gap; the log line is the whole point of failing closed", logged)
			}
		})
	}
}

// TestNewRefusesAPaymentSurfaceThatIsNotExactlyOneCompletePair is the
// constructor's own refusal, and it is the moment the missing half is diagnosed.
//
// A process that can open payments but cannot verify a delivery would create
// checkouts it can never settle — a customer pays and nothing records it — so
// the two halves travel together and either one alone is refused here, where the
// stack names the wiring, rather than in a request goroutine.
func TestNewRefusesAPaymentSurfaceThatIsNotExactlyOneCompletePair(t *testing.T) {
	cases := []struct {
		name     string
		surfaces []PaymentSurface
	}{
		{name: "payments without a verifier", surfaces: []PaymentSurface{{Payments: newFakePayments()}}},
		{name: "a verifier without payments", surfaces: []PaymentSurface{{Verifier: newFakeWebhookVerifier()}}},
		{name: "two surfaces", surfaces: []PaymentSurface{
			{Payments: newFakePayments(), Verifier: newFakeWebhookVerifier()},
			{Payments: newFakePayments(), Verifier: newFakeWebhookVerifier()},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Errorf("New built a handler from %s; the missing half is a wiring defect and a customer would find it first", tc.name)
				}
			}()
			New(application.New("test"), &answeringPinger{}, newFakeSessionUseCases(), newFakeConsoleReadUseCases(), stubUsage(), tc.surfaces...)
		})
	}

	// The pair: one complete surface is accepted, so the refusals above are
	// about the missing half rather than about the argument existing at all.
	if handler := newPaymentServer(newFakePayments(), newFakeWebhookVerifier()); handler == nil {
		t.Errorf("a complete payment surface built no handler")
	}
}
