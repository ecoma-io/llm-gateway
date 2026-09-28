package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/inbound/http"
	paymentprovideradapter "github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/outbound/paymentprovider"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/config"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/payments"
	paymentprovider "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/payments"
)

// The payment integration at the composition root, which is where the two
// things this process may not say anywhere else are said: which adapter
// implements the port, and how the transport's plain vocabulary maps onto the
// application's.
//
// It is the same division consolereads.go keeps, and for the same reason.
// internal/arch/imports_test.go forbids the inbound surface from importing
// internal/domain, so the http package declares four operations, one verifier
// and a handful of result types in plain strings and integers, and SOMEONE who
// holds both grammars has to write the crossing down. cmd is on the rule's
// allow-list precisely so that someone is here — in a file a reader can find,
// rather than smeared across the handlers that call it.
//
// Everything below is mechanical: call, convert, convert back. It computes
// nothing, derives nothing and filters nothing. An amount is copied from the
// payment it was stored on; a status is copied from the delivery the
// application applied; the three figures of a page are copied from the three
// the use case resolved. A value that could be computed two ways belongs
// somewhere else, and the direction of that "somewhere else" is inward: this
// file is the outermost layer this process has.

// paymentSurfaces is the payment surface as the handler constructor takes it:
// one element when this deployment configured a provider, and NOTHING when it
// did not.
//
// The empty answer is a state rather than a failure — config.Payments turns the
// whole group off unless it names a provider, and application.NewPayments is
// built only in that case — and the shape of it is what keeps the handler
// constructor honest. The constructor takes an OPTIONAL surface and serves
// fail-closed handlers for the four payment rows when it is given none (see
// unwiredPaymentSurface); returning a nil slice here is how this process says
// "this deployment sells no top-up surface" without a second constructor, a
// boolean, or a handler that has to know which of the two it was given.
//
// The verifier is composed HERE rather than inside buildPaymentsSurface, and
// the split follows the two secrets: that function is handed the API secret,
// because calling the provider needs it, and this one is handed the signing
// secret, because authenticating a delivery needs that. Neither function ever
// holds both, and neither logs either — config.Payments' own doc comment says
// the same thing from the other side, and the redaction is in that type's
// String method so that a `%v` on the config cannot print one.
//
// The panic inside NewVerifier is unreachable from here for the reason
// buildPaymentsSurface's panics are: config.Payments.Validate refuses an empty
// signing secret and a non-positive tolerance while the group is on, and this
// function is only reached past that validation.
func paymentSurfaces(useCases *application.Payments, cfg config.Payments) []http.PaymentSurface {
	if useCases == nil {
		return nil
	}
	return []http.PaymentSurface{{
		Payments: paymentUseCases{payments: useCases},
		Verifier: webhookVerifier{
			verifier: paymentprovideradapter.NewVerifier(cfg.WebhookSigningSecret, cfg.WebhookTolerance),
		},
		// The name the delivery path is addressed to, and the same value the
		// application namespaces its command keys with — one config field, read
		// here and read there, so the path a provider posts to and the namespace
		// its payments are keyed in cannot drift apart in a deployment template.
		Provider: cfg.Provider,
	}}
}

// paymentUseCases adapts *application.Payments to the http package's seam.
type paymentUseCases struct {
	payments *application.Payments
}

func (u paymentUseCases) BeginCheckout(ctx context.Context, accountID, offerID, idempotencyKey string) (http.PaymentIntentResult, error) {
	result, err := u.payments.BeginCheckout(ctx, application.BeginCheckoutRequest{
		AccountID:      accountID,
		OfferID:        offerID,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return http.PaymentIntentResult{}, err
	}
	// Converged crosses as it is and the handler never renders it: both the
	// first call and a repeat answer with the same payment, because the client
	// asked for a payment and now holds one. It is carried for the handler
	// test's assertion, and this layer neither reads nor re-derives it.
	return http.PaymentIntentResult{
		Intent:    seamPaymentIntent(result.Intent),
		Converged: result.Converged,
	}, nil
}

// ApplyProviderEvent is the one operation on this seam that can move money, and
// the conversion is the whole of the adapter's job — except for the body.
//
// RawBody crosses UNTOUCHED and as the SAME slice the handler verified. It is
// copied into the application's delivery rather than re-read, re-encoded or
// summarised, because it is the evidence of what the provider signed: the
// application stores it beside the delivery so an operator can answer "what did
// the provider actually send" without opening the provider's dashboard, and a
// re-serialisation of the parsed event would be this build's whitespace, key
// order and number spellings rather than the provider's. Nothing in this
// function may look inside it, and nothing does.
func (u paymentUseCases) ApplyProviderEvent(ctx context.Context, delivery http.WebhookDelivery) (http.WebhookOutcome, error) {
	outcome, err := u.payments.ApplyProviderEvent(ctx, application.ProviderDelivery{
		// The two structs are field for field the same — that is what the seam's
		// own comment claims and what the CONVERSION proves: Go permits it only
		// between identical field names, types and order, so a field either type
		// grows turns this line into a compile error at the composition root
		// rather than into a value silently dropped on its way inward. A
		// hand-written literal would compile through exactly that change, and
		// the field it forgot would be a payment funded from an event the
		// application read as empty.
		Event:   paymentprovider.ProviderEvent(delivery.Event),
		RawBody: delivery.RawBody,
	})
	if err != nil {
		return http.WebhookOutcome{}, err
	}
	// The three words cross as strings. The application's disposition is a
	// closed vocabulary of its own and this layer does not spell the members
	// out: a switch here would be a second, later place a new disposition has
	// to be added, and the one that forgot would answer a client with an empty
	// word rather than failing to compile.
	return http.WebhookOutcome{
		Disposition: string(outcome.Disposition),
		Reason:      string(outcome.Reason),
		IntentID:    string(outcome.IntentID),
	}, nil
}

func (u paymentUseCases) ListPayments(ctx context.Context, accountID, after string, limit int) (http.PaymentPageResult, error) {
	page, err := u.payments.ListPayments(ctx, accountID, after, limit)
	if err != nil {
		return http.PaymentPageResult{}, err
	}
	return seamPaymentPage(page), nil
}

// seamPaymentPage renders one resolved page, and it is a function of its own
// rather than three lines inside ListPayments so that the rendering can be
// tested without a store: what a page means to a client is the three figures it
// carries, and two of them — a has_more that disagrees with the rows, a cursor
// this layer re-encoded — are wrong in ways only a page-level test can see. It
// is the same shape seamPaymentIntent has, one level up.
func seamPaymentPage(page application.PaymentPage) http.PaymentPageResult {
	result := http.PaymentPageResult{
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		Items:      make([]http.PaymentIntentRecord, 0, len(page.Items)),
	}
	for _, intent := range page.Items {
		result.Items = append(result.Items, seamPaymentIntent(intent))
	}
	return result
}

// ListTopUpOffers takes no account and ignores the context, and both are worth
// a sentence because both look like mistakes.
//
// The context is required by the seam and discarded here: the list is a slice
// of a catalogue this process built at startup, so there is no statement to
// cancel, no lock to release and no peer to stop waiting for. A method that
// pretended otherwise would be a cancellation point that cancels nothing.
//
// The account is absent from the seam's own signature, so there is none to
// ignore: an offer is a price this deployment decided and not a fact about a
// customer, and a parameter that is always ignored is a parameter the next
// reader will assume means something. The handler still resolves a session
// first, because a price list is not published to anonymous callers.
func (u paymentUseCases) ListTopUpOffers(ctx context.Context) (http.TopUpOfferListResult, error) {
	offers := u.payments.Offers()
	result := http.TopUpOfferListResult{Items: make([]http.TopUpOfferRecord, 0, len(offers))}
	for _, offer := range offers {
		result.Items = append(result.Items, http.TopUpOfferRecord{
			ID:                offer.ID,
			AmountMinorUnits:  offer.AmountMinorUnits,
			Currency:          offer.Currency,
			MinorUnitExponent: offer.MinorUnitExponent,
			Label:             offer.Label,
		})
	}
	return result, nil
}

// seamPaymentIntent renders one stored payment as the wire row the contract
// declares.
//
// Every field is a COPY and none is derived, and the two that could tempt a
// derivation are the two that matter. CheckoutURL is copied, empty included,
// rather than being inferred from the status: the contract types it
// `[string, "null"]`, the seam renders the empty string as null, and a payment
// whose checkout does not exist yet is one fact rather than two. And ExpiresAt
// is copied from the intent's own deadline rather than computed from CreatedAt
// and a configured window — the deadline is a stored fact, and a client
// comparing a computed one against what the provider's checkout page shows
// would be comparing this build's arithmetic against the provider's.
//
// The timestamps go through wireTimestamp, which renders the zero instant as
// the empty string. That is the same rendering every other record on this
// surface uses, and the contract makes both fields required strings, so an
// absent instant is the empty string rather than an omitted key.
func seamPaymentIntent(intent payments.Intent) http.PaymentIntentRecord {
	return http.PaymentIntentRecord{
		ID:                string(intent.ID),
		Status:            string(intent.Status),
		AmountMinorUnits:  intent.AmountMinorUnits,
		Currency:          intent.Currency,
		MinorUnitExponent: intent.MinorUnitExponent,
		CheckoutURL:       intent.CheckoutURL,
		CreatedAt:         wireTimestamp(intent.CreatedAt),
		ExpiresAt:         wireTimestamp(intent.ExpiresAt),
	}
}

// webhookVerifier adapts the provider adapter's verifier to the http package's
// seam.
//
// The adapter is the ONLY implementation of the port's verification half in
// this build, and it is the only thing in the process that has ever seen the
// signing secret. Its vocabulary is the port's, so the event crosses by
// conversion for the reason ApplyProviderEvent's does — a field either struct
// grows is a compile error here. What does NOT cross is the signing secret, the
// signature header, or anything the adapter read out of the request other than
// the fields the port declares.
type webhookVerifier struct {
	verifier *paymentprovideradapter.Verifier
}

func (v webhookVerifier) Verify(headers map[string][]string, rawBody []byte) (http.VerifiedEvent, error) {
	event, err := v.verifier.Verify(headers, rawBody)
	if err != nil {
		// The freshness refusal is TRANSLATED and the two authentication
		// refusals are not, and the asymmetry is the import rule rather than a
		// preference. internal/adapters/outbound may be imported by cmd and by
		// nothing else, so the http package cannot name
		// paymentprovider.ErrStaleDelivery at all and declares its own
		// sentinel for it; the two it CAN name are the port's own, which the
		// adapter already returns wrapped in its own detail, so they cross
		// untouched and errors.Is reaches them with no mapping table in
		// between.
		//
		// The translation keeps the adapter's cause: the handler logs this
		// value, and the adapter's message is the one that says how far outside
		// the tolerance the delivery was — which is what an operator looking at
		// a clock needs. Nothing in it is a secret or a signature; the adapter
		// builds its refusal from the two timestamps, never from the header.
		if errors.Is(err, paymentprovideradapter.ErrStaleDelivery) {
			return http.VerifiedEvent{}, fmt.Errorf("%w: %w", http.ErrStaleDelivery, err)
		}
		return http.VerifiedEvent{}, err
	}
	return http.VerifiedEvent(event), nil
}
