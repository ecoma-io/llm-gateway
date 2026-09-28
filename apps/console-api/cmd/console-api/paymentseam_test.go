package main

// The payment seam's crossing, tested where it happens: the composition root.
//
// Everything this file asserts is about the ONE thing that only exists here. The
// adapter's own tests prove that a signature is checked over the raw bytes and
// that a stale delivery is refused distinguishably; the http package's tests
// prove which status each refusal answers. Neither can see the other, and the
// dependency rule keeps it that way — the transport may not import the outbound
// adapter, so the two halves are joined by exactly one function,
// webhookVerifier.Verify, and by nothing else in this module.
//
// That join has a concrete failure mode worth a test rather than a review: if
// the translation is dropped, a stale delivery reaches the handler's default arm
// and answers 500. A 500 tells the provider to send it again, and every
// redelivery carries the SAME signed timestamp, so the loop never ends and the
// operator sees a retry storm instead of the clock this case is about. The two
// authentication refusals have the mirror-image failure: a mapping table in
// between is a place for a case nobody mapped, and a mis-mapped
// ErrBadSignature is a forged delivery answered 2xx.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/inbound/http"
	paymentprovideradapter "github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/outbound/paymentprovider"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/config"
	// The two packages named `payments` are both in play here, and the aliases
	// are what keeps them apart: `payments` is the PORT, whose sentinels the
	// refusal assertions below compare against, and `paymentsdomain` is the
	// aggregate a stored row is — the type the page's items carry.
	paymentsdomain "github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/payments"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/payments"
)

// seamSigningSecret is assembled from fragments rather than written out, for
// the reason the adapter's own fixtures are: this repository is scanned by
// gitleaks and pushed through GitHub's push protection, and both correctly
// refuse a secret-shaped string. A fixture only has to be a string the code
// treats as a credential.
func seamSigningSecret() string { return "seam-" + strings.Repeat("s", 32) }

// seamTolerance is wide enough that a delivery stamped now is never stale, so
// the freshness assertions below fail on the translation rather than on the
// machine's scheduler.
const seamTolerance = 5 * time.Minute

// seamSignatureHeader is the provider's own header name, spelled here rather
// than imported: the adapter keeps it unexported, and a fixture that signed with
// whatever the implementation happened to call the header could not see the
// header being renamed on the wire.
const seamSignatureHeader = "Stripe-Signature"

// signSeamDelivery computes the provider's signature the way the provider
// documents it, as an INDEPENDENT construction: HMAC-SHA256 over
// "{t}.{rawBody}", keyed with the signing secret's own bytes. A test that signed
// through the implementation it verifies could not see a wrong separator or a
// body re-encoded on its way into the MAC.
func signSeamDelivery(signedAt time.Time, body []byte) map[string][]string {
	stamp := strconv.FormatInt(signedAt.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(seamSigningSecret()))
	mac.Write([]byte(stamp + "." + string(body)))
	signed := "t=" + stamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	return map[string][]string{seamSignatureHeader: {signed}}
}

// seamEventBody is a delivery the adapter can read: a completed, paid checkout
// session naming both the checkout this platform opened and the payment it was
// paid through. It carries insignificant whitespace and a key order no encoder
// would choose, which is what makes the raw-bytes assertion below meaningful —
// the same event in a canonical encoding is a DIFFERENT byte string, and the
// signature covers the one that was sent.
const seamEventBody = `{ "id" : "evt_seam_delivery" ,` +
	`"type":"checkout.session.completed","created":1735689600,` +
	`"data":{"object":{"id":"cs_seam_checkout","payment_intent":"pi_seam_payment",` +
	`"amount_total":4200,"currency":"usd","payment_status":"paid"}}}`

func seamVerifier() webhookVerifier {
	return webhookVerifier{
		verifier: paymentprovideradapter.NewVerifier(seamSigningSecret(), seamTolerance),
	}
}

// TestPaymentSurfacesIsAbsentUntilADeploymentConfiguresAProvider pins the state
// a deployment that sells nothing is in, and the state it is NOT in.
//
// A nil here is not a failure to wire: config.Payments turns the group off
// unless it names a provider, and buildPaymentsSurface answers nil for exactly
// that state. What must not happen is the opposite — a process that configured
// no provider being handed a surface whose use cases are nil pointers, which
// would be a panic on the customer's first top-up rather than a startup
// refusal. The second case is why the constructor is guarded rather than the
// caller: the guard is the one that holds for a caller that never read the
// comment.
func TestPaymentSurfacesIsAbsentUntilADeploymentConfiguresAProvider(t *testing.T) {
	if surfaces := paymentSurfaces(nil, config.Payments{}); len(surfaces) != 0 {
		t.Fatalf("paymentSurfaces(nil, ...) = %d surfaces, want none: a deployment with no provider serves no payment surface, and the four rows answer fail-closed rather than half-wired", len(surfaces))
	}
}

// TestPaymentSurfacesIsExactlyOneCompletePair is the shape the handler
// constructor requires: one payments seam AND one verifier, never one of the
// two.
//
// The application value is a zero value and is deliberately never called — this
// test asserts the SHAPE of what the composition root builds, and reaching a use
// case would need a store, a clock and a provider adapter for no assertion this
// test makes. The zero value is what makes that explicit: nothing below can
// depend on the application working.
func TestPaymentSurfacesIsExactlyOneCompletePair(t *testing.T) {
	cfg := config.Payments{WebhookSigningSecret: seamSigningSecret(), WebhookTolerance: seamTolerance}
	surfaces := paymentSurfaces(&application.Payments{}, cfg)
	if len(surfaces) != 1 {
		t.Fatalf("paymentSurfaces(...) = %d surfaces, want exactly one", len(surfaces))
	}
	if surfaces[0].Payments == nil {
		t.Error("the payment use cases are nil, so every top-up row is a fail-closed 500 in a process that configured a provider")
	}
	if surfaces[0].Verifier == nil {
		t.Error("the verifier is nil, so every delivery is a fail-closed 500 in a process that configured a signing secret")
	}
}

// TestTheWebhookVerifierTranslatesTheAdaptersStaleRefusal is the translation
// this file exists for, asserted from both sides: the transport's own sentinel
// is reachable through the returned error, AND the adapter's cause is still
// under it.
//
// The second half is not decoration. The handler logs this error, and the
// adapter's message is the one that says how far outside the tolerance the
// delivery was — which is what an operator looking at a clock needs. A
// translation that replaced the cause instead of wrapping it would answer the
// right status and leave the log line saying only that something was stale.
func TestTheWebhookVerifierTranslatesTheAdaptersStaleRefusal(t *testing.T) {
	// An hour old against a five-minute tolerance. The signature is valid: this
	// is an AUTHENTIC delivery that is too old to act on, which is the whole
	// reason it must not be reported as an authentication failure.
	body := []byte(seamEventBody)
	headers := signSeamDelivery(time.Now().Add(-time.Hour), body)

	_, err := seamVerifier().Verify(headers, body)
	if !errors.Is(err, http.ErrStaleDelivery) {
		t.Fatalf("Verify() error = %v, want %v: an untranslated refusal reaches the handler's default arm and answers 500, and every redelivery carries the same signed timestamp", err, http.ErrStaleDelivery)
	}
	if !errors.Is(err, paymentprovideradapter.ErrStaleDelivery) {
		t.Errorf("Verify() error = %v, and the adapter's own refusal must remain in the chain so the log line can say how stale the delivery was", err)
	}
	// A stale delivery is authentic, and a caller that treated it as a forgery
	// would be answering the wrong question with the wrong log line.
	if errors.Is(err, payments.ErrBadSignature) {
		t.Errorf("Verify() error = %v, and a stale delivery must not be reported as an authentication failure", err)
	}
}

// TestTheWebhookVerifierLeavesThePortsOwnRefusalsUntranslated is the other half
// of the asymmetry: the two sentinels the transport CAN name cross untouched.
//
// Both cases are reachable from a real request. An altered body under someone
// else's signature is a forgery, or a byte that rotted in transit. An
// undecodable body under a correct signature is a provider that changed its
// wire format — authentic and unreadable, which is an operator's problem rather
// than a retry's.
func TestTheWebhookVerifierLeavesThePortsOwnRefusalsUntranslated(t *testing.T) {
	t.Run("a signature over different bytes", func(t *testing.T) {
		signed := []byte(seamEventBody)
		headers := signSeamDelivery(time.Now(), signed)
		// One digit of the amount changed. The bytes no longer hash to the
		// signature, and nothing about the header distinguishes this from a
		// forged delivery.
		altered := []byte(strings.Replace(seamEventBody, "4200", "9200", 1))

		_, err := seamVerifier().Verify(headers, altered)
		if !errors.Is(err, payments.ErrBadSignature) {
			t.Fatalf("Verify() error = %v, want %v", err, payments.ErrBadSignature)
		}
		if errors.Is(err, http.ErrStaleDelivery) {
			t.Errorf("Verify() error = %v, and a wrong signature is not a freshness question", err)
		}
	})

	t.Run("a correct signature over bytes that name no delivery", func(t *testing.T) {
		body := []byte("this is not a provider event")
		headers := signSeamDelivery(time.Now(), body)

		_, err := seamVerifier().Verify(headers, body)
		if !errors.Is(err, payments.ErrMalformedEvent) {
			t.Fatalf("Verify() error = %v, want %v: an authenticated body that names nothing is a delivery the caller records as unreadable rather than refuses", err, payments.ErrMalformedEvent)
		}
		if errors.Is(err, http.ErrStaleDelivery) || errors.Is(err, payments.ErrBadSignature) {
			t.Errorf("Verify() error = %v, and a well-signed unreadable body is neither a forgery nor a clock problem", err)
		}
	})
}

// TestTheWebhookVerifierReadsTheRawBytesItWasHandedAndNothingElse is the
// property that must survive the crossing: the seam's fields are read from the
// bytes that were SIGNED, and a body that says the same thing in a different
// encoding does not verify.
//
// Both halves come from one fixture. The delivery carries insignificant
// whitespace, so a verifier that re-encoded the parsed event before hashing it
// would refuse a real delivery; and the CANONICAL form of the same event does
// not verify at all, which is the same defect seen from the other side — a
// re-serialising implementation would have accepted it. Neither assertion is
// possible without both bodies, which is why they are in one test.
func TestTheWebhookVerifierReadsTheRawBytesItWasHandedAndNothingElse(t *testing.T) {
	body := []byte(seamEventBody)
	event, err := seamVerifier().Verify(signSeamDelivery(time.Now(), body), body)
	if err != nil {
		t.Fatalf("Verify() error = %v, want the delivery read", err)
	}
	// The two references are the fields this seam grew, and they must arrive
	// populated and in the right slots: the application resolves each against a
	// different stored column, and a pair that crossed swapped would file every
	// capture against a payment reference this plane never wrote.
	if event.CheckoutRef != "cs_seam_checkout" {
		t.Errorf("CheckoutRef = %q, want the session this platform opened", event.CheckoutRef)
	}
	if event.PaymentRef != "pi_seam_payment" {
		t.Errorf("PaymentRef = %q, want the payment the session was paid through", event.PaymentRef)
	}
	if event.EventID != "evt_seam_delivery" {
		t.Errorf("EventID = %q, want the delivery's own id", event.EventID)
	}
	if event.AmountMinorUnits == nil || *event.AmountMinorUnits != 4200 {
		t.Errorf("AmountMinorUnits = %v, want 4200", event.AmountMinorUnits)
	}
	if event.Currency != "USD" {
		t.Errorf("Currency = %q, want the uppercase ISO 4217 form the domain stores", event.Currency)
	}

	t.Run("the same event in another encoding is a different delivery", func(t *testing.T) {
		// The same facts, re-encoded. Nothing about the change is visible in
		// the parsed event, so an implementation that hashed a re-encoding
		// would accept this against the original's signature.
		canonical := []byte(`{"id":"evt_seam_delivery","type":"checkout.session.completed","created":1735689600,` +
			`"data":{"object":{"id":"cs_seam_checkout","payment_intent":"pi_seam_payment",` +
			`"amount_total":4200,"currency":"usd","payment_status":"paid"}}}`)
		if _, err := seamVerifier().Verify(signSeamDelivery(time.Now(), body), canonical); !errors.Is(err, payments.ErrBadSignature) {
			t.Fatalf("Verify() error = %v, want %v: the signature covers the octets the provider sent, not this build's encoding of what they meant", err, payments.ErrBadSignature)
		}
	})
}

// TestTheWebhookVerifierTreatsAnExtraSignatureHeaderAsSmuggling is the header
// half of the same authority, and it crosses the seam as the map it arrived as:
// the handler hands the unparsed map over, and the implementation is what
// refuses multiplicity. The composition root must not flatten the two into one
// string on the way, which a wrapper that took the first value would do
// silently.
func TestTheWebhookVerifierTreatsAnExtraSignatureHeaderAsSmuggling(t *testing.T) {
	body := []byte(seamEventBody)
	headers := signSeamDelivery(time.Now(), body)
	// The same header spelled in another case, which is what a proxy shim or a
	// hand-built map produces. Both values are individually valid, and that is
	// the hazard: an implementation that picked one would accept a request
	// carrying a signature this deployment does not honour.
	lower := strings.ToLower(seamSignatureHeader)
	headers[lower] = append(headers[lower], headers[seamSignatureHeader]...)

	if _, err := seamVerifier().Verify(headers, body); !errors.Is(err, payments.ErrBadSignature) {
		t.Fatalf("Verify() error = %v, want %v: a delivery that authenticates under more than one signature is smuggling", err, payments.ErrBadSignature)
	}
}

// TestSeamPaymentIntentCopiesTheStoredPayment is the row-level crossing, and it
// asserts the two things a derivation would get wrong.
//
// The checkout URL is copied EMPTY and not inferred from the status: the
// contract types the field `[string, "null"]`, and a payment whose checkout does
// not exist yet states that as an absence rather than as a status this layer
// decided. The timestamps are copied from the instants the payment carries, not
// computed from a configured window — the deadline is a stored fact, and a client
// comparing this build's arithmetic against what the provider's page shows would
// be comparing two different things.
func TestSeamPaymentIntentCopiesTheStoredPayment(t *testing.T) {
	created := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	expires := created.Add(30 * time.Minute)

	record := seamPaymentIntent(seamStoredPayment(created, expires))
	if record.ID != "pi_local_1" {
		t.Errorf("ID = %q, want the platform's own payment id", record.ID)
	}
	if record.Status != "checkout_open" {
		t.Errorf("Status = %q, want the status the delivery wrote", record.Status)
	}
	if record.AmountMinorUnits != 4200 || record.Currency != "USD" {
		t.Errorf("amount = %d %s, want 4200 USD — the CANONICAL figure the ledger was told, not the provider's", record.AmountMinorUnits, record.Currency)
	}
	if record.CheckoutURL != "" {
		t.Errorf("CheckoutURL = %q, want empty: an absence is not a value this layer invents", record.CheckoutURL)
	}
	if record.CreatedAt != "2026-09-28T12:00:00Z" || record.ExpiresAt != "2026-09-28T12:30:00Z" {
		t.Errorf("timestamps = %s/%s, want the instants the payment carries rendered as RFC 3339 in UTC", record.CreatedAt, record.ExpiresAt)
	}
}

// TestThePaymentPageIsForwardedUnchanged pins the page's three figures, because
// the two that are easy to get wrong are the two a client acts on: a has_more
// that disagrees with the row count is a page that stops early and leaves rows
// the client was told about unseen, and a cursor is opaque — this layer may copy
// it and may not touch it. The fixture cursor is deliberately not URL-safe: a
// wrapper that ran it through an encoder would hand the client a position the
// application cannot resolve.
func TestThePaymentPageIsForwardedUnchanged(t *testing.T) {
	const cursor = "eyJ2IjoxfQ+/="
	page := application.PaymentPage{
		Items:      []paymentsdomain.Intent{seamStoredPayment(time.Now(), time.Now())},
		HasMore:    true,
		NextCursor: cursor,
	}

	forwarded := seamPaymentPage(page)
	if forwarded.NextCursor != cursor {
		t.Errorf("NextCursor = %q, want %q untouched: a cursor names a position in an order only the application knows", forwarded.NextCursor, cursor)
	}
	if !forwarded.HasMore {
		t.Error("HasMore = false, want the value the use case resolved: a client that stops here leaves rows it was told about unseen")
	}
	if len(forwarded.Items) != 1 || forwarded.Items[0].ID != "pi_local_1" {
		t.Errorf("Items = %+v, want the one row the page carried", forwarded.Items)
	}

	t.Run("an empty page is a page and not a missing one", func(t *testing.T) {
		// The rows are always an allocated slice, never nil: the contract
		// requires `items`, and a nil slice marshals as `null` — a client that
		// distinguished the two would be reading an absence the contract does
		// not have.
		empty := seamPaymentPage(application.PaymentPage{})
		if empty.Items == nil {
			t.Fatal("Items is nil, want an empty slice: the contract requires the key, and null is not an empty list")
		}
		if empty.HasMore || empty.NextCursor != "" {
			t.Errorf("empty page = %+v, want no has_more and no cursor", empty)
		}
	})
}

// seamStoredPayment is the stored payment the row-level assertions are made
// against: a payment whose checkout is open and whose checkout URL is NOT set,
// which is the state the contract renders as an explicit null and the state a
// derivation would be most tempted to paper over.
func seamStoredPayment(created, expires time.Time) paymentsdomain.Intent {
	return paymentsdomain.Intent{
		ID:               "pi_local_1",
		AccountID:        "acct_1",
		FundingBucketID:  "bucket_1",
		AmountMinorUnits: 4200,
		Currency:         "USD",
		Status:           "checkout_open",
		CreatedAt:        created,
		ExpiresAt:        expires,
	}
}
