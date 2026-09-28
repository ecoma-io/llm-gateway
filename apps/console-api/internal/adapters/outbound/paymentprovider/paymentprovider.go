// Package paymentprovider is the console-api's outbound HTTP adapter for the
// payment port in internal/ports/outbound/payments: it opens hosted checkouts
// at a third party's API and verifies that third party's webhook deliveries.
//
// It speaks to ONE provider, named here so a reader does not have to derive it
// from the header names: STRIPE, over its Checkout Sessions API and its
// webhook signing scheme. The provider's vocabulary is the whole reason this
// package exists — its endpoint, its `Idempotency-Key` request header, its
// amount encoding, its `Stripe-Signature` header and its event shapes are
// facts about Stripe, and a fact about Stripe does not belong in the domain
// that decides what a payment is. When a second processor is onboarded this
// package grows a sibling and the port does not move.
//
// The two operations are two types rather than one, because the port declares
// them as two interfaces and the two have exactly one caller each: Client
// opens checkouts and holds the API secret, Verifier verifies deliveries and
// holds the signing secret. Nothing here holds both, so the checkout path
// cannot reach the webhook secret and the webhook path cannot reach the API
// secret.
//
// What the adapter does NOT do is decide anything about money. It converts the
// amount it was given into the provider's encoding exactly, or it refuses; it
// returns the provider's checkout URL byte for byte; and it reports what a
// verified delivery claims without resolving that claim against any payment,
// because resolving it is the application layer's job and the layer that could
// get it wrong would also be the layer with the authority to move money.
//
// It is built against the HTTP API directly, with the standard library, and
// deliberately not against the provider's Go SDK: a checkout is one POST and a
// signature is one HMAC, and a dependency for that would be a version to pin,
// a second vocabulary for the same header names, and a library that decides on
// this package's behalf which API version it speaks.
package paymentprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	// The port's package is named `payments` and so is the domain package this
	// adapter never imports; the alias here keeps every reference below
	// unambiguous.
	payments "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/payments"
)

const (
	// checkoutSessionsPath is the operation this adapter reaches, as Stripe's
	// API reference declares it: POST /v1/checkout/sessions creates a hosted
	// Checkout Session. It is a constant because it is the contract's, not the
	// caller's — nothing in the Control Plane chooses which path a session
	// lives at.
	checkoutSessionsPath = "/v1/checkout/sessions"

	// idempotencyHeader is the request header Stripe recognises a repeated
	// creation by. The name is the provider's, and it is spelled once, here,
	// because it is the single most load-bearing string in this file.
	idempotencyHeader = "Idempotency-Key"

	// checkoutAnswerLimit bounds what this adapter reads of any one answer.
	//
	// The same bound the dataplane adapter puts on a projection answer, and for
	// the same reason: this process does not rely on its peers being well
	// behaved, it relies on their being unable to choose this process's memory
	// ceiling. A session object is a couple of kilobytes; a body that cannot
	// state itself inside 64 KiB is not a session.
	checkoutAnswerLimit = 1 << 16

	// checkoutProductName is the line item's product name, which Stripe
	// requires and which the customer sees on the provider's own page. The
	// port carries no description field, so this is a constant rather than a
	// caller's string: a customer-controlled product name is a field on a
	// payment page that the platform did not choose, and the platform chooses
	// this one.
	checkoutProductName = "Account top-up"
)

// stripeUnitAmountMax is the largest amount Stripe's integer amount encoding
// carries: 99,999,999.99 in a two-decimal currency. The bound is the
// provider's, not this package's policy.
const stripeUnitAmountMax = 99_999_999

// Client opens hosted checkouts at the provider's API.
//
// It is stateless apart from its configuration: no session cache, no retry
// counter, no learned anything. One call builds one request from its arguments
// and reads one answer.
type Client struct {
	// httpClient is built by New rather than injected, and the difference from
	// the dataplane adapter is the whole reason this comment exists. That
	// adapter's client is the composition root's, with a bare &http.Client{}
	// and no timeout — a posture its own construction comment justifies by the
	// per-cycle deadlines the projection and ingestion loops wrap every call
	// in. Those deadlines do not exist here. A checkout is opened from a
	// BROWSER-FACING request path: there is no loop above it, no cycle
	// timeout, and no ambient deadline at all, so a client with no timeout
	// would hold a customer's HTTP request open for as long as a provider that
	// has stopped answering keeps a connection alive. The bound therefore
	// lives on the client, where the deployment configured it.
	//
	// CheckRedirect is the other half of that posture. A redirect on this
	// request would re-send the API secret AND the idempotency key to whatever
	// host the response named — a host no operator configured — and the
	// idempotency key is the one value that makes a second charge
	// impossible. The provider's own API answers no redirects, so a 3xx here
	// is a defect on the path or a proxy that should not be there, and both
	// are reported rather than followed.
	httpClient *http.Client

	// endpoint is the API base URL, parsed once at construction: it is
	// configuration, its shape is fixed for the life of the client, and the
	// only thing a call appends is its own operation's path.
	endpoint url.URL

	// secretKey is the provider's API secret, held only to be put in the
	// Authorization header. It never reaches a log line or an error.
	secretKey string
}

// New returns the checkout client described by the provider's API base URL, the
// API secret this process authenticates with, and the bound on one call.
//
// It panics on a wiring defect — a base URL that is not an absolute http(s)
// URL, a URL carrying userinfo, a query or a fragment, an empty secret, a
// non-positive timeout — for the reason postgres.New panics on a nil pool and
// dataplane.New panics on an empty credential: each is a defect in the
// composition root that would otherwise appear far from the line that could
// have said so, as a request to nowhere, a checkout that can only ever be
// refused, or a client with no bound at all.
//
// No panic message quotes the base URL. url.Parse's own error text quotes the
// string it rejected, credentials included, and a startup panic is a log line.
func New(apiBaseURL, secretKey string, requestTimeout time.Duration) *Client {
	base, err := url.Parse(apiBaseURL)
	if err != nil {
		panic("paymentprovider: the API base URL is not a URL")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		panic("paymentprovider: the API base URL must be http or https")
	}
	if base.Host == "" {
		panic("paymentprovider: the API base URL must name a host")
	}
	if base.User != nil {
		panic("paymentprovider: the API base URL must not carry userinfo")
	}
	if base.RawQuery != "" || base.Fragment != "" {
		panic("paymentprovider: the API base URL must not carry a query or a fragment")
	}
	if secretKey == "" {
		panic("paymentprovider: New requires a provider API secret")
	}
	if requestTimeout <= 0 {
		panic("paymentprovider: New requires a positive request timeout")
	}

	base.Path = strings.TrimSuffix(base.Path, "/")
	return &Client{
		httpClient: &http.Client{
			Timeout: requestTimeout,
			// A payment POST is never redirected: see the field's comment.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		endpoint:  *base,
		secretKey: secretKey,
	}
}

// Compile-time proof that the client satisfies the port's checkout half, so a
// signature drifting from the seam is a build failure rather than a discovery
// at the composition root.
var _ payments.Checkout = (*Client)(nil)

// OpenCheckout creates a hosted Checkout Session and returns where to send the
// customer, verbatim.
func (c *Client) OpenCheckout(ctx context.Context, in payments.CheckoutRequest) (payments.CheckoutSession, error) {
	// Everything below is checked before a byte goes on the wire, because each
	// of these is a request the provider would refuse — or, worse, accept.
	if in.IdempotencyKey == "" {
		// A call the provider cannot recognise as a repeat of another call is
		// the definition of a second charge. A lost response is a normal event
		// and the retry that follows it carries the same key; a request that
		// carries none makes the retry a NEW checkout, and the customer pays
		// twice. Refusing here is the only place this can be caught.
		return payments.CheckoutSession{}, errors.New("paymentprovider: open checkout requires an idempotency key: a lost response is a normal event, and a retry the provider cannot recognise as a repeat is a second charge")
	}
	if in.Currency == "" {
		return payments.CheckoutSession{}, errors.New("paymentprovider: open checkout requires a currency")
	}
	if in.Reference == "" {
		return payments.CheckoutSession{}, errors.New("paymentprovider: open checkout requires a reference the provider can echo back")
	}
	if in.ReturnURL == "" {
		// The return URL is built by this application from configuration and
		// never from a request parameter; an empty one would be a customer
		// left on the provider's page with no way back.
		return payments.CheckoutSession{}, errors.New("paymentprovider: open checkout requires a return URL")
	}
	amount, err := stripeUnitAmount(in.AmountMinorUnits)
	if err != nil {
		return payments.CheckoutSession{}, err
	}

	// The body is Stripe's form-encoded parameter syntax: the field names are
	// the provider's, and url.Values does the escaping, which matters because
	// the return URL and the reference travel in it and a value with an `&` in
	// it must not become a second parameter. url.Values also escapes the
	// brackets in the nested keys; the encoding is the same string to any form
	// parser that decodes before it parses, which is what the provider's own
	// libraries rely on.
	form := url.Values{}
	form.Set("mode", "payment")
	// Both URLs are the same configured value. The port carries one return
	// URL, and both outcomes send the customer back to the console, where the
	// payment's own state — not the URL — says what happened.
	form.Set("success_url", in.ReturnURL)
	form.Set("cancel_url", in.ReturnURL)
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", strings.ToLower(in.Currency))
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(amount, 10))
	form.Set("line_items[0][price_data][product_data][name]", checkoutProductName)
	// The reference goes on the session AND on the payment intent it creates,
	// so whichever delivery the provider sends first carries the identifier
	// this platform stored. It is metadata rather than anything the provider
	// acts on: the application matches a delivery to its own payment, and the
	// provider has no opinion about the value.
	form.Set("metadata[reference]", in.Reference)
	form.Set("payment_intent_data[metadata][reference]", in.Reference)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.checkoutURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return payments.CheckoutSession{}, fmt.Errorf("paymentprovider: build the checkout request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.secretKey)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set(idempotencyHeader, in.IdempotencyKey)

	response, err := c.httpClient.Do(request)
	if err != nil {
		// The transport cause, not the wrapper: http.Client's *url.Error
		// embeds the full request URL, and the wrapper would be this package
		// putting a configured endpoint into an error line.
		//
		// The port's ErrProviderUnavailable wraps it, because a failure to
		// reach the provider at all is the condition that clears on its own.
		// This package cannot tell a provider that is down from a DNS entry
		// that is wrong, and it does not guess: the caller's answer for both is
		// a 503 an operator can see, which is the right answer for one of them
		// and a harmless one for the other.
		return payments.CheckoutSession{}, fmt.Errorf("paymentprovider: open checkout: %w: %w",
			payments.ErrProviderUnavailable, transportCause(err))
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		// A 5xx or a 429 is the provider saying its own service is unwell —
		// the one class of provider answer that is worth repeating — and it is
		// reported as such rather than as an internal fault, because the two
		// send a client in opposite directions.
		if retryableProviderStatus(response.StatusCode) {
			return payments.CheckoutSession{}, fmt.Errorf("paymentprovider: open checkout: %w: the provider answered %d",
				payments.ErrProviderUnavailable, response.StatusCode)
		}
		// The status is the whole message, and the provider's error body is
		// deliberately not read. Stripe's error MESSAGES quote the credential
		// they rejected — "Invalid API Key provided: sk_..." is its own
		// wording — and an error this package returns is a log line written by
		// a process that has the key. The status is what an operator acts on;
		// a 401 and a 402 need different responses, and both are legible
		// without the provider's prose. A 3xx lands here too: CheckRedirect
		// refuses to follow it, and the status says why.
		return payments.CheckoutSession{}, fmt.Errorf("paymentprovider: open checkout: unexpected status %d", response.StatusCode)
	}

	var body checkoutSessionResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, checkoutAnswerLimit)).Decode(&body); err != nil {
		// A 200 whose body will not decode is the provider breaking its own
		// contract, and it fails closed: no URL is returned, so no customer is
		// sent anywhere.
		return payments.CheckoutSession{}, fmt.Errorf("paymentprovider: open checkout: the answer would not decode as a session: %w", err)
	}
	session, err := body.session()
	if err != nil {
		return payments.CheckoutSession{}, fmt.Errorf("paymentprovider: open checkout: %w", err)
	}
	return session, nil
}

// retryableProviderStatus reports whether a provider's own HTTP status says
// the provider is unwell rather than that it refused this request.
//
// The split is the whole of the port's ErrProviderUnavailable doctrine, stated
// once so that the two arms above and any later operation cannot disagree about
// where the line is. 5xx is the provider's own service failing; 429 is the
// provider asking to be called again more slowly, which is the same instruction
// — wait and retry — expressed by the one status that carries it.
//
// Everything else that is not 200 is a refusal: 400 for a request it would not
// parse, 401 and 403 for a credential it does not accept, 402 for something
// about the account of the credential, and the 3xx that CheckRedirect refused
// to follow. Repeating any of those sends the same bytes to the same answer,
// which is the definition of a failure a client must not be told to retry.
func retryableProviderStatus(status int) bool {
	return status >= http.StatusInternalServerError || status == http.StatusTooManyRequests
}

// checkoutURL is the client's endpoint with this operation's path appended.
func (c *Client) checkoutURL() string {
	joined := c.endpoint
	joined.Path = strings.TrimSuffix(joined.Path, "/") + checkoutSessionsPath
	return joined.String()
}

// checkoutSessionResponse is the wire shape of one created session, as much of
// it as this adapter reads: the provider's identifier for the checkout and
// where the customer is sent.
//
// Both fields are pointers for the reason every required field on this seam is
// one: with a plain value, a body that omits the field reads as the empty
// string and the omission passes as an answer, and an empty URL is a customer
// sent nowhere while an empty reference is a payment that can never be matched
// to the delivery that reports it.
type checkoutSessionResponse struct {
	ID  *string `json:"id"`
	URL *string `json:"url"`
}

// session translates Stripe's answer into the port's, and returns the URL
// BYTE FOR BYTE.
//
// Nothing here parses the URL, and that is a rule rather than an oversight:
// the port returns it verbatim and its own comment says why — the URL is a
// promise the provider makes and this process keeps on the provider's behalf,
// and a consumer that url.Parse'd it to pull out a session id would be
// interpreting a value whose only promise is that it may be handed to a
// browser. The one transformation between the provider's bytes and this field
// is the JSON string decode that reading any body requires.
func (r checkoutSessionResponse) session() (payments.CheckoutSession, error) {
	switch {
	case r.ID == nil:
		return payments.CheckoutSession{}, errors.New("the checkout answer carried no id, which the provider's contract requires")
	case *r.ID == "":
		return payments.CheckoutSession{}, errors.New("the checkout answer carried an empty id, so there is no reference to match a later delivery against")
	case r.URL == nil:
		return payments.CheckoutSession{}, errors.New("the checkout answer carried no url, which the provider's contract requires")
	case *r.URL == "":
		return payments.CheckoutSession{}, errors.New("the checkout answer carried an empty url, so there is nowhere to send the customer")
	}
	return payments.CheckoutSession{URL: *r.URL, ProviderRef: *r.ID}, nil
}

// stripeUnitAmount converts the port's integer minor units into the integer
// Stripe's `unit_amount` field carries, and refuses every amount that encoding
// cannot hold exactly.
//
// Stripe's `unit_amount` is "a positive integer in cents (or the currency's
// own smallest unit)", and the port's AmountMinorUnits is an integer in
// exactly that unit — the same quantity the ledger stores. So the conversion
// this function performs is the identity, and saying that plainly is not a
// shortcut: there is no arithmetic here that COULD round, which is the point.
// A conversion that scaled the figure — to major units, to a hundredth of the
// stored unit, to a decimal string with a fixed number of places — is where an
// amount stops being the amount, and that conversion would live in this
// function and nowhere else. A rounded amount is a charge for a figure nobody
// agreed to, and the ledger would then be credited a different figure than the
// customer paid.
//
// What the refusals below are is the "not exact" arm of that rule: an amount
// this encoding cannot represent is an error rather than a clamp, because a
// clamped amount is the same defect as a rounded one. A non-positive amount is
// refused here too, mirroring the domain, which refuses a top-up of nothing
// before it ever reaches this adapter.
func stripeUnitAmount(minorUnits int64) (int64, error) {
	if minorUnits <= 0 {
		return 0, fmt.Errorf("paymentprovider: a checkout for %d minor units is not a checkout", minorUnits)
	}
	if minorUnits > stripeUnitAmountMax {
		return 0, fmt.Errorf("paymentprovider: a checkout for %d minor units is more than the %d the provider's amount encoding carries exactly", minorUnits, stripeUnitAmountMax)
	}
	return minorUnits, nil
}

// transportCause strips the URL net/http attaches to a failed request.
//
// http.Client wraps every transport failure in a *url.Error whose message
// embeds the full request URL. Here that URL is the provider's API endpoint —
// configuration, and a value that would end up in an error line every time the
// provider was unreachable. The cause underneath is what says what went wrong
// (a refused connection, a TLS failure, an expired context), so the cause is
// what this package reports and the wrapper is dropped. Everything else is
// returned as it came.
//
// The dataplane adapter carries the same function for the same reason; it is
// duplicated rather than shared because an adapter may not import another
// adapter, and a shared helper package would be a new root in this module's
// architecture for six lines.
func transportCause(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}
