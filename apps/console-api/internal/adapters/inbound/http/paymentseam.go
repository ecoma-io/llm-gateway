package http

import (
	"context"
	"errors"
	"log"
	"time"
)

// The port, as a choice this seam made once and states here because a reader
// meets the seam before the handler that branches on it.
//
// internal/arch/imports_test.go allows an inbound adapter to name
// internal/ports — the rule's own words are "the port vocabulary is what the
// application is written against and what the adapters implement, so it points
// inward from both" — so the two refusals a delivery can carry are the PORT's
// sentinels rather than copies of them, and paymenthandlers.go is where they are
// imported and compared. That is the deliberate choice between the two this
// transport could have made: the adapter that implements the port returns
// `payments.ErrBadSignature` and `payments.ErrMalformedEvent` already wrapped in
// its own detail, so `errors.Is` reaches them with no translation layer in
// cmd/console-api that could be forgotten the day a third refusal is added. A
// second set of sentinels declared here would be a mapping table, and the failure
// mode of a mapping table is a case nobody mapped answering 500.
//
// The one boundary this choice does NOT cross is the adapter itself:
// internal/adapters/outbound/paymentprovider may be imported by cmd and by
// nothing else, so the freshness refusal its verifier exports
// (`paymentprovider.ErrStaleDelivery`) cannot be named in this package at all.
// That is why ErrStaleDelivery below is declared HERE, and why the composition
// root is what translates the adapter's refusal into it.

// The payment operations, as the narrow seam their handlers call. Four
// operations, and the fourth is the price list rather than a fourth way to move
// money: a console that rendered a top-up chooser without GET /top-up-offers
// would be asking a customer to name an offer they could not see.
//
// The verifier is its OWN interface rather than a fifth method here, and the
// reason is the reason ConsoleReadUseCases is separate from SessionUseCases:
// what a nil means. A process with no payment
// use cases has a console whose top-up screen cannot open a payment; a process
// with no verifier has an endpoint that cannot authenticate a delivery — and
// those are two different missing features with two different failure modes. A
// webhook handler that reached a nil verifier would answer 500 to every
// delivery, forever, and a top-up handler that reached a nil use-case would
// create no payment while its own row still existed. Keeping the two apart lets
// each be refused on its own terms.
//
// It is also separate from SessionUseCases for that same reason: the session
// surface is this service's authentication boundary, and the payment surface is
// product work performed BEHIND one. A nil here is a feature that is absent, not
// a boundary that is open.
//
// Every field and argument is PLAIN — strings, an int, a *int64, a time.Time.
// No domain type crosses this boundary, for the import-rule reason at the top of
// wire.go: internal/arch/imports_test.go forbids this package from reaching
// internal/domain, so an event's kind, a payment's status and an amount are all
// values the transport carries and never a grammar it interprets. The
// consequence is intentional: nothing here can decide what a payment is, and the
// one layer that could — the application — is the only one that does.
//
// The ACCOUNT IS ALWAYS A PARAMETER and never a field of a request type. It is
// the session's: resolveSession resolved it and the handler passes it as this
// seam's first argument. There is therefore no request body on this surface a
// caller could name an account in, and no way to thread a different one in
// without editing the call. That is ADR 0012 §2's rule stated as a signature
// rather than as a review habit, and it matters more here than anywhere else on
// the surface: a payment is money, and "which account funds itself" is not a
// question the browser gets to answer.
type PaymentUseCases interface {
	// BeginTransfer opens a payment for the account against one of this
	// deployment's top-up offers, and answers with it — durable, and priced by
	// the server. The offer is an opaque identifier the application resolves
	// into an amount and a currency; the request never carries either.
	//
	// idempotencyKey is the caller's identity for this logical top-up and is
	// required. Every attempt at the same top-up carries the same value, so a
	// retry after a dropped response converges on the payment the key already
	// names rather than opening a second one. A converged call returns the SAME
	// payment, possibly in a later state than this call's request would have
	// opened, and the handler renders it identically either way.
	BeginTransfer(ctx context.Context, accountID, offerID, idempotencyKey string) (PaymentIntentResult, error)

	// ApplyProviderEvent records one verified delivery and reports what became
	// of it. It is the ONLY operation on this surface that can move money, and
	// it is called by nothing a browser can reach.
	//
	// The delivery carries the event AND the raw bytes it was verified from, and
	// both halves are load-bearing: the event says what the delivery claims and
	// the bytes are what the signature covered. A caller that re-serialised the
	// event, or read the body a second time, would hand this method a different
	// message than the one the provider signed — see WebhookDelivery.
	ApplyProviderEvent(ctx context.Context, delivery WebhookDelivery) (WebhookOutcome, error)

	// ListPayments returns one page of the account's payments, newest first.
	// The account is the port's predicate, so another account's payment is a row
	// the statement never returned rather than a 403 a filter produced.
	ListPayments(ctx context.Context, accountID, after string, limit int) (PaymentPageResult, error)

	// ListTopUpOffers returns the top-up offers this deployment sells: the price
	// list a chooser renders, and the vocabulary POST /payment-intents is called
	// with.
	//
	// It takes NO ACCOUNT, and the absence is the point rather than an omission:
	// an offer is a price this deployment decided, not a fact about a customer,
	// so there is no account for the predicate to name — and the parameter is
	// omitted rather than accepted and ignored, because a parameter that is
	// always ignored is a parameter the next reader will assume means something.
	// It is not paged either: a fixed list of a handful of prices has no position
	// for a cursor to name. It is still behind a session, because publishing a
	// price list to anonymous callers is a decision this surface has not made,
	// and the session is resolved by the handler rather than passed here.
	ListTopUpOffers(ctx context.Context) (TopUpOfferListResult, error)
}

// WebhookVerifier answers one question about a delivery: is it ours, and what
// does it claim?
//
// It is the port's own interface, method for method, and this package could
// have named `payments.WebhookVerifier` directly instead of declaring it again.
// It is redeclared because the two describe two different contracts: the port's
// says what an IMPLEMENTATION must do, and this one says what a HANDLER calls. A
// handler that took the port's interface would be a handler written against the
// port's package, and the day the port grew a member for a second caller's
// benefit this file would be edited for a reason that has nothing to do with
// transport. The shapes are identical today and the go vet-able assignment in
// cmd/console-api is what keeps them identical: an implementation of the port's
// interface satisfies this one without a shim, and a signature that drifts is a
// compile error at the composition root rather than a 400 nobody can explain.
type WebhookVerifier interface {
	// Verify checks the provider's signature over rawBody using the headers
	// supplied, and returns the event those bytes carry.
	//
	// The headers arrive UNPARSED and with their multiplicity intact — a
	// provider's signature header appearing twice is refused by the
	// implementation rather than resolved to either value, so this seam hands
	// over the whole map rather than one string. The body arrives as the bytes
	// that were read, and it is the SAME slice the handler then passes to
	// ApplyProviderEvent: a body read twice is not necessarily the body that was
	// verified.
	//
	// The errors are the port's, and the handler branches on exactly three:
	// payments.ErrBadSignature (refused, 400, nothing written),
	// payments.ErrMalformedEvent (authenticated and unnameable: logged, 200,
	// nothing written) and ErrStaleDelivery (authenticated and outside the
	// freshness tolerance: refused, 400, nothing written). Anything else is a
	// failure this transport cannot classify, and it answers 500 — the one
	// status that means "nothing was recorded, send it again".
	Verify(headers map[string][]string, rawBody []byte) (VerifiedEvent, error)
}

// VerifiedEvent is one verified delivery, normalized, in plain fields.
//
// The fields are the port's ProviderEvent field for field, and the type is
// redeclared here for the same reason the interface is: the transport renders
// and forwards values and never interprets them. Note what is NOT here — there
// is no disposition, no status and no account. A verified event is a CLAIM about
// a payment, and which payment it is about is a question only the application
// can answer, because only the application can resolve the destination
// reference this build recorded.
//
// The field-for-field mirror is deliberately mechanical, so a reference the port
// grows — a refund resolves against a payment reference a capture does not name
// — is one more plain field here and nothing else: no handler in this package
// reads any of these fields, and a transport that had begun to would have to be
// taught what the new one means.
//
// AmountMinorUnits is a pointer and nil is a refusal rather than a zero: an
// absent amount and an amount of zero are different sentences, and the port's
// own comment says so. The transport never reads it — it forwards the event —
// and its plain shape is what lets the composition root rebuild the provider's
// struct without this package knowing that struct exists.
type VerifiedEvent struct {
	// Kind is the provider's own vocabulary, a plain string. This package never
	// compares it to anything: whether a kind is one this build interprets is
	// the APPLICATION's decision, and a handler that classified it would be a
	// second decision-maker with no record of what it decided.
	Kind string
	// EventID is the provider's delivery identity, and the key a delivery is
	// recorded under. Empty only when the implementation could not read one out
	// of the bytes at all, which is the case it reports as
	// payments.ErrMalformedEvent instead of returning.
	EventID string
	// TransferRef is the provider's identifier for the DESTINATION it issued
	// for this platform's payment — the virtual account the money arrived at —
	// or empty when the delivery names none.
	//
	// It is the reference a capture is resolved by, because it is the one this
	// plane recorded before the customer was ever shown it. A refund names the
	// payment instead, which is why the two are separate fields rather than one
	// reference with a footnote.
	TransferRef string
	// PaymentRef is the provider's identifier for the PAYMENT rather than the
	// delivery, or empty when the provider named none. It is the value a
	// refund resolves through, and the one a capture stores.
	PaymentRef string
	// AmountMinorUnits is the provider's reported amount, or nil when the event
	// reports none.
	AmountMinorUnits *int64
	// Currency is the provider's ISO 4217 code, or empty.
	Currency string
	// OccurredAt is when the provider observed the outcome.
	OccurredAt time.Time
}

// WebhookDelivery is one verified delivery as the application receives it: the
// event, and the bytes it was verified from.
//
// The raw bytes travel WITH the event rather than being read again by the layer
// below, and that is not a convenience. A signature is computed over bytes, and
// the bytes that were verified must be the bytes that are interpreted: a second
// read of the request body returns whatever is left of the stream — in practice,
// nothing at all — and an application that re-encoded the event into JSON would
// be recording a message the provider never sent, with whitespace, key order and
// number spellings of this build's choosing. Carrying the slice makes the
// mistake unrepresentable at this boundary rather than merely discouraged.
//
// The bytes are the SAME slice Verify was given, not a copy of it: the handler
// reads the body exactly once, verifies that slice, and hands that slice on.
type WebhookDelivery struct {
	// Event is what the delivery claims, as the verifier read it.
	Event VerifiedEvent
	// RawBody is the exact body those claims were verified from.
	RawBody []byte
}

// WebhookOutcome is what became of a recorded delivery, in plain fields.
//
// Disposition is the application's word, one of `applied`, `duplicate` or
// `quarantined`; Reason is set only for a quarantine and names why an operator
// is being asked to look; IntentID is the payment the delivery resolved to when
// it resolved to one.
//
// The handler renders NONE of this on the wire, and that is deliberate rather
// than an oversight — see the acknowledgement constant at the bottom of this
// file.
// The outcome exists so a test can assert what a delivery did and so a future
// log line can name it; a response that carried it would invite the provider's
// implementation to branch on a decision it has no business making.
type WebhookOutcome struct {
	Disposition string
	Reason      string
	IntentID    string
}

// PaymentIntentRecord is the seam's payment row: what this platform asked the
// provider to collect, and what became of it.
//
// Status is the plain string the contract's own enum spells — `created`,
// `awaiting_transfer`, `requires_action`, `succeeded`, `failed`, `cancelled`,
// `expired`, `partially_refunded`, `refunded`, `quarantined` — and this package
// never compares it to anything. A handler that rendered a payment as settled
// made that up; the only thing that can move a payment's status is a signed
// delivery the application applied.
//
// TransferInstructions is nil exactly while no destination has been recorded,
// and the wire renders that absence as an explicit null rather than as an empty
// object — the contract types the field `[string, "null"]` and the schema says
// null is the value of a payment whose destination does not exist yet. There is
// no derived status here: nothing in this package infers "created" from an
// absent destination, and a payment whose destination has been recorded keeps
// carrying it in every later state.
//
// MinorUnitExponent is copied from the payment rather than looked up in the
// live catalogue, and the difference is visible to a customer: the amount is
// integer minor units, and the exponent is what tells a reader whether 2500 is
// twenty-five dollars or twenty-five hundred yen. A payment carries its own
// because it outlives the offer that priced it — an offer withdrawn from the
// price list would otherwise change how a historical payment reads.
type PaymentIntentRecord struct {
	ID                   string
	Status               string
	AmountMinorUnits     int64
	Currency             string
	MinorUnitExponent    int
	TransferInstructions *PaymentTransferInstructions
	CreatedAt            string
	ExpiresAt            string
}

// PaymentTransferInstructions is where the customer sends the money, in the
// provider's own words: the destination the provider issued for this payment,
// the bank and holder it sits with, and the provider's drawing of the same.
//
// The four fields are the domain's own four, and none of them is parsed here.
// The seam carries them so the transport can render a destination a customer can
// check before sending anything; nothing in this package reads a bank out of the
// image, or re-derives the code from the other three, and a transport that began
// to would be interpreting an encoding the provider owns.
//
// QRURL is empty when the provider drew no image, which is an ordinary value
// rather than a failure — the contract types it nullable, and a destination with
// a code, a bank name and a holder is one a customer can pay into without ever
// scanning anything.
type PaymentTransferInstructions struct {
	TransferCode  string
	BankName      string
	AccountHolder string
	QRURL         string
}

// PaymentIntentResult is BeginTransfer's answer: the payment, and whether this
// call was the one that opened it.
//
// Converged carries no wire meaning and the handler does not render it. The
// contract states why at length: both the first call and the repeat answer 201
// with the same payment, because the client asked for a payment and now holds
// one, and which of the two happened is not a difference a page may act on. It
// is on the struct for the assertion a test can make about which path ran — and
// for the day the application itself wants to log it.
type PaymentIntentResult struct {
	Intent    PaymentIntentRecord
	Converged bool
}

// PaymentPageResult is one page of payments: the rows, whether more follow, and
// the opaque cursor that names where the next page starts.
//
// The three fields are the same three every list on this surface answers with,
// and the cursor is passed through untouched — this transport never decodes one,
// never compares one and never synthesises one.
type PaymentPageResult struct {
	Items      []PaymentIntentRecord
	HasMore    bool
	NextCursor string
}

// TopUpOfferRecord is the seam's price-list row: one offer this deployment
// sells.
//
// It carries no account and no state, because an offer is configuration rather
// than a row: there is nothing about it that varies per customer and nothing
// about it that changes while a reader reads it.
//
// Label is presentation and not authority — the string a chooser shows, which a
// deployment may leave empty — and AmountMinorUnits with Currency and
// MinorUnitExponent are the price. A client that preferred a number parsed out
// of the label to AmountMinorUnits would be pricing a top-up itself, which is the
// one thing the request schema exists to make impossible.
type TopUpOfferRecord struct {
	ID                string
	AmountMinorUnits  int64
	Currency          string
	MinorUnitExponent int
	Label             string
}

// TopUpOfferListResult is the whole price list.
//
// There is no cursor and no total, and their absence is the contract's decision
// rather than a smaller version of one: a cursor names a position in a history
// and a price list has no history, so the list arrives whole. The order is the
// deployment's — the order an operator declared them — which is why this is a
// slice the application ordered rather than something this transport sorts.
type TopUpOfferListResult struct {
	Items []TopUpOfferRecord
}

// ErrStaleDelivery is this transport's word for a delivery that authenticated
// but arrived outside the deployment's freshness tolerance.
//
// It is declared HERE and not in the port, and the asymmetry with the two
// sentinels above is the import rule rather than a preference: the freshness
// refusal is the ADAPTER's (`paymentprovider.ErrStaleDelivery`), and
// internal/adapters/outbound may be imported by cmd and by nothing else, so this
// package has no way to name it. Declaring the transport's own sentinel is the
// documented alternative to a mapping table, and the composition root is what
// connects the two: its verifier wrapper returns this value for a stale
// delivery, and this handler then answers the same 400 a bad signature gets with
// a different log line.
//
// Why a 400 and not a 500, given that nothing is recorded on either path: both a
// forged delivery and a stale one are permanently unusable, and a redelivery
// carries the SAME signed timestamp — a retry could only repeat the refusal. A
// 4xx keeps that out of the provider's retry budget, which is the same argument
// the contract makes for answering a bad signature 400 rather than 5xx. If the
// composition root does not translate the adapter's refusal, a stale delivery
// reaches the default arm and answers 500 — a retry, which is wrong but not
// dangerous, and the log line names the case.
var ErrStaleDelivery = errors.New("console-api http: the delivery authenticated but arrived outside the freshness tolerance")

// unwiredPaymentSurface is the surface a process that did not wire the payment
// integration serves: all four payment rows exist in the route table — they are
// part of the declared surface, and the contract test reads that table — and
// every one of them answers 500 with a log line naming the wiring gap.
//
// It is here because the four-argument New has to serve SOMETHING for those
// rows. The alternative shapes are worse: a route missing from the table would
// make the contract test fail for a process whose only fault is not serving
// payments, and a nil seam reached at request time would panic on the floor of a
// request goroutine, where the stack names a request rather than the wiring. A
// fail-closed 500 is the contract's own answer for "nothing was durably
// recorded", which a process in this state has recorded exactly nothing, and the
// log line is what makes it an operator's fact rather than a client's.
//
// It is not a state production reaches: cmd/console-api passes the real surface,
// and the panic-free refusal here exists so the route table is complete without
// one.
func unwiredPaymentSurface() PaymentSurface {
	return PaymentSurface{Payments: unwiredPayments{}, Verifier: unwiredVerifier{}}
}

// PaymentSurface is the seams the payment rows are built from, travelling as one
// argument because they are one feature: a process that serves the top-up screen
// but cannot verify a delivery would open payments it can never settle.
type PaymentSurface struct {
	Payments PaymentUseCases
	Verifier WebhookVerifier

	// Provider is the name the delivery endpoint answers for, and it is the
	// deployment's configured provider — the same value the application
	// namespaces its ledger command keys with and the adapter speaks the
	// vocabulary of. The delivery path carries it as a segment, and the handler
	// compares the two before it reads a byte of the body.
	//
	// It is a plain string because everything crossing this boundary is, and it
	// is HERE rather than inside the verifier because the verifier is the port's
	// own interface — the adapter implements it on its own terms, and a
	// deployment detail like "which path this process answers on" is not the
	// adapter's to know.
	//
	// Empty is the unwired surface and is deliberately not an error: see
	// servesProvider.
	Provider string
}

// servesProvider reports whether this surface answers for the provider named in
// the delivery path.
//
// An unwired surface answers for everything and serves nothing: its Provider is
// empty, and skipping the comparison is what lets its own refusal — a 500 saying
// the payment integration was never wired — be what a provider sees. Answering
// 404 there would be the wrong fact twice over: the operator would go looking for
// a route that exists, and the one line that says the feature is absent would
// never be reached.
//
// The comparison is exact rather than case-folded. The path is a route segment,
// not a value a caller is invited to spell loosely: the name is validated at
// startup into the alphabet the domain's own fold already fixes, so a request
// spelling it differently is addressed to a resource this process does not serve
// rather than to the same one in another case.
func (s PaymentSurface) servesProvider(name string) bool {
	return s.Provider == "" || s.Provider == name
}

// errPaymentSurfaceUnwired is what every unwired method reports. The message is
// for the server's own log line, never the wire: writeError renders the fixed
// `internal error` and the correlation fact, and nothing here reaches a client.
func errPaymentSurfaceUnwired(operation string) error {
	log.Printf("%s the payment integration is not wired: %s has no implementation behind it", serviceName, operation)
	return errors.New("console-api http: the payment surface was not wired: " + operation)
}

type unwiredPayments struct{}

func (unwiredPayments) BeginTransfer(context.Context, string, string, string) (PaymentIntentResult, error) {
	return PaymentIntentResult{}, errPaymentSurfaceUnwired("POST /payment-intents")
}

func (unwiredPayments) ApplyProviderEvent(context.Context, WebhookDelivery) (WebhookOutcome, error) {
	return WebhookOutcome{}, errPaymentSurfaceUnwired("POST /payment-webhooks/{provider}")
}

func (unwiredPayments) ListPayments(context.Context, string, string, int) (PaymentPageResult, error) {
	return PaymentPageResult{}, errPaymentSurfaceUnwired("GET /payment-intents")
}

func (unwiredPayments) ListTopUpOffers(context.Context) (TopUpOfferListResult, error) {
	return TopUpOfferListResult{}, errPaymentSurfaceUnwired("GET /top-up-offers")
}

type unwiredVerifier struct{}

func (unwiredVerifier) Verify(map[string][]string, []byte) (VerifiedEvent, error) {
	return VerifiedEvent{}, errPaymentSurfaceUnwired("POST /payment-webhooks/{provider} verification")
}

// The payments surface's wire shapes, mirroring api/openapi/shared/console.yaml
// field for field. They are plain strings and plain integers for the reason
// readwire.go states at length: this package renders shapes, and a handler
// holding a grammar would be deciding what a client should see.

// paymentIntentRecord mirrors the contract's PaymentIntent, whose seven fields
// are all required.
//
// TransferInstructions is a pointer because the schema types it
// `[string, "null"]` and null is the value of a payment whose destination does
// not exist yet. The null is rendered from the seam's nil by
// renderTransferInstructions, and no handler derives the payment's status from
// it — the two facts travel separately because the schema declares both, and a
// client that inferred one from the other would be inferring a state the
// provider's own delivery is the only authority on.
type paymentIntentRecord struct {
	ID                   string                       `json:"id"`
	Status               string                       `json:"status"`
	AmountMinorUnits     int64                        `json:"amount_minor_units"`
	Currency             string                       `json:"currency"`
	MinorUnitExponent    int                          `json:"minor_unit_exponent"`
	TransferInstructions *paymentTransferInstructions `json:"transfer_instructions"`
	CreatedAt            string                       `json:"created_at"`
	ExpiresAt            string                       `json:"expires_at"`
}

// paymentTransferInstructions mirrors the contract's PaymentTransferInstructions,
// whose four fields are all required and one of which is nullable.
//
// QRURL is a *string because the schema types it `[string, "null"]`: a provider
// that drew no image answered a real destination, and the null is that answer
// rather than a failure. The other three are plain strings and are always
// written, because a destination the store recorded always has them — the
// domain refuses to record one that does not.
type paymentTransferInstructions struct {
	TransferCode  string  `json:"transfer_code"`
	BankName      string  `json:"bank_name"`
	AccountHolder string  `json:"account_holder"`
	QRURL         *string `json:"qr_url"`
}

// beginTransferRequest mirrors the contract's CreatePaymentIntentRequest.
//
// Both fields are required by the schema and NEITHER is validated here beyond
// being decodable. An offer is an opaque identifier whose vocabulary belongs to
// the deployment's configuration and an idempotency key's meaning belongs to the
// application — so a blank or unknown one is refused by the layer that knows what
// it should have been, with a 400 the contract promises. The one thing this
// transport owns is that the body is a single well-formed JSON object.
//
// There is deliberately no amount and no currency field, and their absence is
// the design rather than an omission: a client-supplied amount is not a smaller
// version of a policy, it is the absence of one.
type beginTransferRequest struct {
	Offer          string `json:"offer"`
	IdempotencyKey string `json:"idempotency_key"`
}

// topUpOfferRecord mirrors the contract's TopUpOffer, four required fields and
// one optional one.
//
// Label is `omitempty` because the schema makes it optional and a deployment may
// publish an offer with no name of its own — the absence is a value the contract
// has, not a value this build invents, and rendering an empty string where the
// document describes an absent field would be a client rendering a blank name
// instead of the amount. The other four are required and are always written,
// including a minor_unit_exponent of zero, which is the exponent of a currency
// with no minor unit and not an absence.
type topUpOfferRecord struct {
	ID                string `json:"id"`
	AmountMinorUnits  int64  `json:"amount_minor_units"`
	Currency          string `json:"currency"`
	MinorUnitExponent int    `json:"minor_unit_exponent"`
	Label             string `json:"label,omitempty"`
}

// topUpOfferList mirrors the contract's TopUpOfferList: `items`, and nothing
// else — no cursor, no total, no `has_more`. It is deliberately not the page
// envelope every other list on this surface answers with, and the schema states
// why at length: a cursor names a position, and a price list has no position to
// name.
type topUpOfferList struct {
	Items []topUpOfferRecord `json:"items"`
}

// webhookAcknowledgementBody is the exact body every 2xx from the delivery
// endpoint carries, and it is a raw BYTE STRING rather than a marshalled value
// for a reason that would otherwise look like a style preference.
//
// The body is a third party's contract to the byte. The provider accepts a
// delivery only when the response is 200 or 201 within thirty seconds AND the
// body is exactly the JSON document it documents — one member, spelled
// `success`, with the space after the colon that the provider's own example
// shows. Go's encoding/json emits `{"success":true}`: no space, but the same
// document as far as every JSON parser is concerned and a DIFFERENT byte string
// as far as a provider comparing what it received against what it published is
// concerned. This platform's JSON discipline — always marshal a value, never
// hand-write a body — is exactly what would produce the other string, which is
// why this one exception has to be deliberate, spelled out here, and tested at
// the byte.
//
// The sameness across outcomes is the contract's other rule: 2xx means "this
// event needs no further delivery from you" and nothing more, so applied,
// duplicate and quarantined all get these same bytes. A disposition rendered
// here would invite the provider's implementation to branch on a decision it has
// no business making — and the provider is the one party this plane cannot
// correct afterwards. A quarantined delivery is an operator's work item,
// recorded where it can be; telling the provider about it would put a permanent
// refusal into its retry budget.
const webhookAcknowledgementBody = `{"success": true}`

// renderPaymentIntent converts one seam record into the contract's shape.
//
// The two instants go through mustWireTime, so a malformed timestamp is a panic
// at the boundary rather than a field the contract types as a date-time
// rendering as something else. The destination goes through
// renderTransferInstructions, for the reason on that function.
func renderPaymentIntent(intent PaymentIntentRecord) paymentIntentRecord {
	return paymentIntentRecord{
		ID:                   intent.ID,
		Status:               intent.Status,
		AmountMinorUnits:     intent.AmountMinorUnits,
		Currency:             intent.Currency,
		MinorUnitExponent:    intent.MinorUnitExponent,
		TransferInstructions: renderTransferInstructions(intent.TransferInstructions),
		CreatedAt:            mustWireTime(intent.CreatedAt),
		ExpiresAt:            mustWireTime(intent.ExpiresAt),
	}
}

// renderTransferInstructions renders a recorded destination, or null when this
// payment has none.
//
// The nil is the whole reason this is a function rather than a field copy: the
// contract types `transfer_instructions` as an object OR null, and null is a
// value the contract has — the state of a payment this platform has recorded
// but not yet obtained a destination for. An empty object would be a different
// value, and a client that received one would have to guess whether four empty
// strings meant "no destination" or "a destination whose fields were lost".
//
// The QR URL is the one member that may be absent WITHIN a destination, and the
// two nulls mean different things: no destination at all, versus a destination
// the provider issued without drawing an image. Both are payable and only the
// second has a code, a bank name and a holder beside it.
func renderTransferInstructions(instructions *PaymentTransferInstructions) *paymentTransferInstructions {
	if instructions == nil {
		return nil
	}
	return &paymentTransferInstructions{
		TransferCode:  instructions.TransferCode,
		BankName:      instructions.BankName,
		AccountHolder: instructions.AccountHolder,
		QRURL:         nullableString(instructions.QRURL),
	}
}

// renderTopUpOffer converts one seam record into the contract's shape, field for
// field. There is no conversion to make beyond copying — no decimal point is
// placed here, no amount is scaled, and the currency is rendered as the
// deployment spelled it: a client that needs to place a decimal point does it
// with minor_unit_exponent, which is why that field travels.
// The two structs are field-for-field identical in name, order and type, which
// is what lets the conversion below be a type conversion rather than a
// field-by-field copy. A copy here would be a second place to forget a field:
// the first version of this function was a literal, and it went stale the moment
// a sixth field was added to one type and not the other — which is a wire
// contract silently losing a field, the worst shape this file could fail in.
// The conversion fails to COMPILE the moment the two drift, which is the
// outcome worth having, and the drift this file's own contract test is about.
func renderTopUpOffer(offer TopUpOfferRecord) topUpOfferRecord {
	return topUpOfferRecord(offer)
}

// renderTopUpOfferList renders the whole price list in the order the application
// gave it.
//
// The slice is normalised to non-nil for the reason newPage's is: the contract
// types `items` as an array, and `null` is not an array. The order is NOT
// touched — a price list's order is a presentation decision the deployment made,
// and a transport that sorted it would be choosing how a chooser renders.
func renderTopUpOfferList(result TopUpOfferListResult) topUpOfferList {
	offers := make([]topUpOfferRecord, 0, len(result.Items))
	for _, offer := range result.Items {
		offers = append(offers, renderTopUpOffer(offer))
	}
	return topUpOfferList{Items: offers}
}

// nullableString renders an absent value as an explicit null. It is the sibling
// of wireInstants for the one field on this surface whose absence is not an
// instant, and it exists for the same reason: the empty string and null are
// different values on the wire, and the contract chooses null for a destination
// the provider drew no image of.
func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
