// Package paymentprovider is the console-api's outbound HTTP adapter for the
// payment port in internal/ports/outbound/payments: it obtains bank-transfer
// destinations from a third party's API and verifies that third party's webhook
// deliveries.
//
// It speaks to ONE provider, named here so a reader does not have to derive it
// from the header names: SEPAY, over its virtual-account order API and its
// webhook signing scheme. The provider's vocabulary is the whole reason this
// package exists — its endpoints, its `X-SePay-Signature` and
// `X-SePay-Timestamp` headers, its order-code grammar and its event shape are
// facts about SePay, and a fact about SePay does not belong in the domain that
// decides what a payment is. When a second processor is onboarded this package
// grows a sibling and the port does not move.
//
// The two operations are two types rather than one, because the port declares
// them as two interfaces and the two have exactly one caller each: Client opens
// transfer destinations and holds the API token, Verifier verifies deliveries
// and holds the webhook signing secret. Nothing here holds both, so the
// outbound path cannot reach the webhook secret and the webhook path cannot
// reach the API token.
//
// What the adapter does NOT do is decide anything about money. It converts the
// amount it was given into the provider's encoding exactly, or it refuses; it
// returns the provider's own description of a destination — the account, its
// bank, its holder, and the provider's image of the transfer — verbatim and
// unparsed; and it reports what a verified delivery claims without resolving
// that claim against any payment, because resolving it is the application
// layer's job and the layer that could get it wrong would also be the layer
// with the authority to move money.
//
// It is built against the HTTP API directly, with the standard library, and
// deliberately not against a vendor SDK: obtaining a destination is one POST
// and a signature is one HMAC, and a dependency for that would be a version to
// pin, a second vocabulary for the same header names, and a library that
// decides on this package's behalf which API version it speaks.
package paymentprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	// The port's package is named `payments` and so is the domain package this
	// adapter never imports; the alias here keeps every reference below
	// unambiguous.
	payments "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/payments"
)

const (
	// orderPath is the operation this adapter reaches: POST
	// /v2/bank-accounts/{ba_xid}/orders creates one virtual-account order and
	// answers with the account the customer must pay into. It is a constant
	// because it is the provider's contract, not the caller's — nothing in the
	// Control Plane chooses which path an order lives at — and the account
	// segment is filled from configuration.
	orderPath = "/bank-accounts/%s/orders"

	// authorizationScheme is the credential scheme the provider's API expects.
	// The token travels as `Bearer {token}`, and the scheme is written here
	// rather than composed at the call site so that the one place a credential
	// meets a request header is readable in a single line.
	authorizationScheme = "Bearer "

	// orderAnswerLimit bounds what this adapter reads of any one answer.
	//
	// The same bound the dataplane adapter puts on a projection answer, and for
	// the same reason: this process does not rely on its peers being well
	// behaved, it relies on their being unable to choose this process's memory
	// ceiling. An order object is a couple of kilobytes, and the provider's own
	// answer also carries the QR image it drew; a body that cannot state itself
	// inside 64 KiB is not an order answer.
	orderAnswerLimit = 1 << 16

	// The provider's own grammar for an order code, as its documentation
	// states it: alphanumeric, at least six characters, at most fifty. It is
	// the identifier this platform's request is known by at the provider — the
	// thing a duplicate is refused against, and the thing an operator reads off
	// the provider's dashboard.
	minOrderCodeLength = 6
	maxOrderCodeLength = 50
)

// OrderSettings is the merchant's own configuration for the provider's order
// API: which bank account orders are issued under, and the handful of optional
// fields the provider's own documentation makes bank-dependent.
//
// It is a struct rather than six constructor parameters because the six travel
// together and a caller that supplied five and forgot the sixth would get an
// order that the provider refuses for a reason this process cannot read out of
// its answer — see OpenTransfer on why the provider's error prose is never
// parsed.
type OrderSettings struct {
	// BankAccountXID is the provider's identifier for the bank account orders
	// are issued under. It is a PATH SEGMENT and not a body field, so it is
	// validated on construction rather than sent as a value: a string that
	// needs escaping is a string this adapter refuses to build a URL out of.
	BankAccountXID string

	// VAHolderName is the name the virtual account is held in, and it is sent
	// on every order. It is the beneficiary name a customer's banking app may
	// show them when they enter the account number, and the provider echoes it
	// back on the answer this adapter turns into transfer instructions — so it
	// is the one field here that a customer sees, and it is configuration
	// rather than anything a request parameter can choose.
	VAHolderName string

	// TID and VAPrefix are the provider's optional order fields for the banks
	// that require them — the terminal identifier a bank issues, and the prefix
	// a virtual account number must carry. Which bank requires which is the
	// provider's own documentation's business and it changes; this adapter
	// sends each one when it is set and omits it entirely when it is not, so
	// the deployment's answer to "does our bank need this" is configuration and
	// not a build.
	TID      string
	VAPrefix string

	// QRCodeTemplate names the layout of the image the provider draws for the
	// transfer. It is the provider's vocabulary and it is required: this build
	// asks for an image on every order, because a destination a customer can
	// scan is the whole affordance, and a template is what the provider needs
	// to draw one.
	QRCodeTemplate string
}

// Client obtains transfer destinations from the provider's order API.
//
// It is stateless apart from its configuration: no order cache, no retry
// counter, no learned anything. One call builds one request from its arguments
// and reads one answer.
type Client struct {
	// httpClient is built by New rather than injected, and the difference from
	// the dataplane adapter is the whole reason this comment exists. That
	// adapter's client is the composition root's, with a bare &http.Client{}
	// and no timeout — a posture its own construction comment justifies by the
	// per-cycle deadlines the projection and ingestion loops wrap every call
	// in. Those deadlines do not exist here. A destination is obtained from a
	// BROWSER-FACING request path: there is no loop above it, no cycle timeout,
	// and no ambient deadline at all, so a client with no timeout would hold a
	// customer's HTTP request open for as long as a provider that has stopped
	// answering keeps a connection alive. The bound therefore lives on the
	// client, where the deployment configured it.
	//
	// CheckRedirect is the other half of that posture. A redirect on this
	// request would re-send the API token AND the order identity to whatever
	// host the response named — a host no operator configured — and the order
	// identity is the one value that makes a duplicate destination impossible
	// to mint silently. The provider's own API answers no redirects, so a 3xx
	// here is a defect on the path or a proxy that should not be there, and
	// both are reported rather than followed.
	httpClient *http.Client

	// endpoint is the API base URL, parsed once at construction: it is
	// configuration, its shape is fixed for the life of the client, and the
	// only thing a call appends is its own operation's path.
	endpoint url.URL

	// token is the provider's API token, held only to be put in the
	// Authorization header. It never reaches a log line or an error.
	token string

	// orders is the merchant's own order configuration, validated at
	// construction.
	orders OrderSettings
}

// New returns the order client described by the provider's API base URL, the
// API token this process authenticates with, the merchant's order settings, and
// the bound on one call.
//
// It panics on a wiring defect — a base URL that is not an absolute https URL,
// a URL carrying userinfo, a query or a fragment, an empty token, an order
// configuration missing a field the API requires, a non-positive timeout — for
// the reason postgres.New panics on a nil pool and dataplane.New panics on an
// empty credential: each is a defect in the composition root that would
// otherwise appear far from the line that could have said so, as a request to
// nowhere, a transfer that can only ever be refused, or a client with no bound
// at all.
//
// HTTPS AND NOT HTTP, and this is the second of two refusals rather than a
// second opinion on the first: the configuration layer holds the same rule, and
// either alone would leave a path open. Config refuses at Load, so an operator
// cannot start the process with a cleartext base URL; this refuses at New, so a
// caller that constructs a client without going through config cannot either.
// The reason the URL is secret-bearing rather than merely public is the
// credential: every request this client makes carries the API token as a bearer
// credential, and over cleartext that token is readable by anything on the path
// — a token that can open orders and read the account's transactions, with no
// permission scoping to limit the damage. A destination obtained over a
// MITM-able connection is also a MITM's own account number, which this plane
// would then store and show a customer as the place to send their money.
//
// No panic message quotes the base URL or the token. url.Parse's own error text
// quotes the string it rejected, credentials included, and a startup panic is a
// log line.
func New(apiBaseURL, token string, orders OrderSettings, requestTimeout time.Duration) *Client {
	base, err := url.Parse(apiBaseURL)
	if err != nil {
		panic("paymentprovider: the API base URL is not a URL")
	}
	if base.Scheme != "https" {
		panic("paymentprovider: the API base URL must use the https scheme: every request this client makes carries the deployment's payment API token as a bearer credential, and over cleartext it is readable by anything on the path")
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
	if token == "" {
		panic("paymentprovider: New requires a provider API token")
	}
	if requestTimeout <= 0 {
		panic("paymentprovider: New requires a positive request timeout")
	}
	if orders.BankAccountXID == "" {
		panic("paymentprovider: New requires the bank account orders are issued under")
	}
	if !urlSafeSegment(orders.BankAccountXID) {
		panic("paymentprovider: the bank account identifier must be a URL-safe path segment: this adapter refuses to build an order URL out of a value that would need escaping, because an escaped segment the provider decodes differently from this process is an order issued under the wrong account")
	}
	if orders.VAHolderName == "" {
		panic("paymentprovider: New requires the name the virtual account is held in: it is the beneficiary a customer's bank shows them, and an order without one is an account nobody can check before sending money to it")
	}
	if orders.QRCodeTemplate == "" {
		panic("paymentprovider: New requires the QR code template the provider draws the transfer's image with")
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
		endpoint: *base,
		token:    token,
		orders:   orders,
	}
}

// urlSafeSegment reports whether a value can be placed in a URL path segment
// with no escaping, which is the only kind of value this adapter will build an
// order URL from.
//
// The set is deliberately narrow — unreserved characters, which are never
// escaped and never percent-decoded differently by anybody — and it is a
// refusal rather than an escaping call because the two failures are not
// equivalent. url.PathEscape would produce a URL, and whether the provider
// decodes that segment back to this value is a fact about the provider's
// framework that this build has not verified; an order issued under a
// mis-decoded account identifier is a destination for money that belongs to
// somebody else's merchant account, which no later check in this platform can
// notice. A value outside the set is an operator-visible startup panic naming
// the variable, which is a strictly better failure.
func urlSafeSegment(value string) bool {
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// Compile-time proof that the client satisfies the port's transfer half, so a
// signature drifting from the seam is a build failure rather than a discovery
// at the composition root.
var _ payments.Transfers = (*Client)(nil)

// OpenTransfer asks the provider for a destination for one payment and returns
// the provider's own description of it.
//
// THE PROVIDER HAS NO SEPARATE IDEMPOTENCY HEADER, and that absence is the
// single most important thing to know about this method. The order code IS the
// request's identity: it travels as a body field rather than as a header, and
// the provider answers a second order under a code it already holds with a
// conflict instead of the destination it issued the first time. The
// consequences run through everything below.
//
//   - The code is derived from the port's IdempotencyKey, which the port
//     already defines as the caller's stable identity for one logical
//     transfer, so a retry of one attempt carries one code — which is what
//     makes the provider's refusal MEAN something rather than being noise.
//   - A conflict cannot be recovered from, because the destination the
//     provider issued under that code is not returned to this process and the
//     provider documents no way to read it back by the code that names it. It
//     is reported as ErrOrderCodeTaken and the attempt is abandoned: the port
//     says why, and the application's answer is a new payment with a new key
//     rather than a guess at an account number nobody was ever told.
//   - The refusal is not a retryable condition, so it is not reported as one.
//     The bytes of the next attempt would be the same bytes and the answer
//     would be the same answer.
func (c *Client) OpenTransfer(ctx context.Context, in payments.TransferRequest) (payments.TransferInstructions, error) {
	// Everything below is checked before a byte goes on the wire, because each
	// of these is a request the provider would refuse — or, worse, accept.
	if in.IdempotencyKey == "" {
		// A call the provider cannot recognise as a repeat of another call is
		// the definition of a second destination for one payment. A lost
		// response is a normal event and the retry that follows it carries the
		// same key; a request that carries none makes the retry a NEW order,
		// and the customer is shown an account the platform is not waiting on.
		return payments.TransferInstructions{}, errors.New("paymentprovider: open transfer requires an idempotency key: a lost response is a normal event, and a retry the provider cannot recognise as a repeat is a second destination")
	}
	if in.Currency == "" {
		return payments.TransferInstructions{}, errors.New("paymentprovider: open transfer requires a currency")
	}
	if in.Currency != settlementCurrency {
		// The provider settles domestic transfers in exactly one currency, so a
		// request for another is a deployment that has priced an offer in a
		// currency this provider cannot move. Refusing HERE is what keeps that
		// a startup-adjacent configuration fault with a clear message rather
		// than a destination that is issued, shown to a customer, paid into,
		// and only then refused by the delivery's own currency check — with
		// real money already at the provider.
		return payments.TransferInstructions{}, fmt.Errorf("paymentprovider: this provider settles in %s and this deployment asked for %s, so no destination could hold the amount the payment is denominated in", settlementCurrency, in.Currency)
	}
	code, err := orderCode(in.IdempotencyKey)
	if err != nil {
		return payments.TransferInstructions{}, err
	}
	amount, err := orderAmount(in.AmountMinorUnits)
	if err != nil {
		return payments.TransferInstructions{}, err
	}
	duration, err := orderDuration(in.ExpiresIn)
	if err != nil {
		return payments.TransferInstructions{}, err
	}

	body := orderRequest{
		OrderCode:    code,
		Amount:       amount,
		VAHolderName: c.orders.VAHolderName,
		Duration:     duration,
		// The image is always requested: a destination a customer can scan is
		// the affordance this whole integration exists to produce, and the
		// template is what the provider draws it with.
		WithQRCode:     true,
		QRCodeTemplate: c.orders.QRCodeTemplate,
		TID:            c.orders.TID,
		VAPrefix:       c.orders.VAPrefix,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		// Unreachable: every field above is a string, an integer or a boolean.
		// Kept because a caller that received a half-built request body would
		// have no way to tell that from a provider that refused one.
		return payments.TransferInstructions{}, fmt.Errorf("paymentprovider: encode the order request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.orderURL(), bytes.NewReader(encoded))
	if err != nil {
		return payments.TransferInstructions{}, fmt.Errorf("paymentprovider: build the order request: %w", err)
	}
	request.Header.Set("Authorization", authorizationScheme+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

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
		return payments.TransferInstructions{}, fmt.Errorf("paymentprovider: open transfer: %w: %w",
			payments.ErrProviderUnavailable, transportCause(err))
	}
	defer func() { _ = response.Body.Close() }()

	switch {
	case response.StatusCode == http.StatusCreated || response.StatusCode == http.StatusOK:
		// Both mean the order exists. Two statuses rather than one because a
		// creation answer is spelled both ways across HTTP APIs and the
		// provider's own documentation states the created one; treating a
		// 200 as a failure would refuse a working order for its status line,
		// and the destination that came back with it is the thing this method
		// is for.
	case response.StatusCode == http.StatusConflict:
		// The provider holds an order under this code. See the method's own
		// comment for why this is unrecoverable rather than a lookup, and why
		// the mapping is by status rather than by the provider's error prose:
		// this endpoint's only documented conflict is a duplicate order code,
		// the body is not read (see below), and the direction this can be
		// wrong in is an abandoned attempt with a clear message rather than a
		// wrong destination.
		return payments.TransferInstructions{}, fmt.Errorf("%w: the provider already holds an order under this payment's code", payments.ErrOrderCodeTaken)
	case retryableProviderStatus(response.StatusCode):
		// A 5xx or a 429 is the provider saying its own service is unwell —
		// the one class of provider answer that is worth repeating — and it is
		// reported as such rather than as an internal fault, because the two
		// send a client in opposite directions. The provider's documented rate
		// limit arrives as a 429 and is deliberately in this class: opening
		// transfers bursts when a customer clicks through the console's offers,
		// and waiting is the correct answer to it.
		return payments.TransferInstructions{}, fmt.Errorf("paymentprovider: open transfer: %w: the provider answered %d",
			payments.ErrProviderUnavailable, response.StatusCode)
	default:
		// The status is the whole message, and the provider's error body is
		// deliberately not read. Error prose quotes what was rejected — a
		// credential included — and an error this package returns is a log line
		// written by a process that holds the credential. The status is what an
		// operator acts on; a 401 and a 400 need different responses, and both
		// are legible without the provider's wording. A 3xx lands here too:
		// CheckRedirect refuses to follow it, and the status says why.
		return payments.TransferInstructions{}, fmt.Errorf("paymentprovider: open transfer: unexpected status %d", response.StatusCode)
	}

	var answer orderResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, orderAnswerLimit)).Decode(&answer); err != nil {
		// An answer whose body will not decode is the provider breaking its own
		// contract, and it fails closed: no destination is returned, so no
		// customer is shown an account to send money to.
		return payments.TransferInstructions{}, fmt.Errorf("paymentprovider: open transfer: the answer would not decode as an order: %w", err)
	}
	instructions, err := answer.instructions(amount)
	if err != nil {
		return payments.TransferInstructions{}, fmt.Errorf("paymentprovider: open transfer: %w", err)
	}
	return instructions, nil
}

// retryableProviderStatus reports whether a provider's own HTTP status says
// the provider is unwell rather than that it refused this request.
//
// The split is the whole of the port's ErrProviderUnavailable doctrine, stated
// once so that the arms above and any later operation cannot disagree about
// where the line is. 5xx is the provider's own service failing; 429 is the
// provider asking to be called again more slowly, which is the same instruction
// — wait and retry — expressed by the one status that carries it.
//
// Everything else that is not a creation status is a refusal: 400 for a request
// it would not parse, 401 and 403 for a credential it does not accept, the 409
// the caller above has already taken, and the 3xx that CheckRedirect refused to
// follow. Repeating any of those sends the same bytes to the same answer, which
// is the definition of a failure a client must not be told to retry.
func retryableProviderStatus(status int) bool {
	return status >= http.StatusInternalServerError || status == http.StatusTooManyRequests
}

// orderURL is the client's endpoint with this operation's path appended.
func (c *Client) orderURL() string {
	joined := c.endpoint
	joined.Path = strings.TrimSuffix(joined.Path, "/") + fmt.Sprintf(orderPath, c.orders.BankAccountXID)
	return joined.String()
}

// orderRequest is the wire shape of one order, as the provider's API declares
// it: the identity the request is known by, the amount the account will be
// issued for, the holder's name, how long the account stays valid, and the
// image to draw.
//
// The optional fields carry `omitempty` and the required ones do not, which is
// the whole of how "which bank needs this" stays a configuration question: an
// unset terminal or prefix is a field the provider never sees, rather than one
// it sees as an empty string and has to interpret.
type orderRequest struct {
	OrderCode      string `json:"order_code"`
	Amount         int64  `json:"amount"`
	VAHolderName   string `json:"va_holder_name"`
	Duration       int64  `json:"duration"`
	WithQRCode     bool   `json:"with_qrcode"`
	QRCodeTemplate string `json:"qrcode_template"`
	TID            string `json:"tid,omitempty"`
	VAPrefix       string `json:"va_prefix,omitempty"`
}

// orderResponse is the wire shape of one created order, as much of it as this
// adapter reads. The provider wraps its answers in a `data` member.
//
// Every field is a pointer for the reason every required field on this seam is
// one: with a plain value, a body that omits the field reads as the empty
// string and the omission passes as an answer — and an empty destination is a
// customer with nowhere to send money, while an empty holder name is an account
// nobody can check.
type orderResponse struct {
	Data *orderData `json:"data"`
}

// orderData is the order itself.
type orderData struct {
	// VANumber is the account the provider issued for this payment alone: the
	// destination, and the reference a later delivery names. It is the single
	// most load-bearing string in this file.
	VANumber *string `json:"va_number"`

	// VAHolderName and AccountHolderName both name the destination's holder,
	// and the preference between them is deliberate — see instructions.
	VAHolderName      *string `json:"va_holder_name"`
	AccountHolderName *string `json:"account_holder_name"`

	// BankName is the provider's name for the institution the destination sits
	// at.
	BankName *string `json:"bank_name"`

	// QRCodeURL is the provider's own image of the transfer to make, or absent
	// when it drew none. The raw image data is deliberately NOT read: the URL
	// is what the console renders, and a second copy of the same bytes in a
	// column would be a second thing to keep correct.
	QRCodeURL *string `json:"qr_code_url"`

	// Amount is the amount the destination was issued for, as the provider
	// states it. It is read to be CHECKED, not to be trusted — see
	// instructions.
	Amount *int64 `json:"amount"`

	// OrderCode is the identity this request carried. It is not read: the value
	// is this platform's own, it was sent rather than received, and comparing
	// the provider's echo against it would tell this build nothing it did not
	// already know.
	OrderCode *string `json:"order_code"`
}

// The bounds below are the schema's, not the provider's, and they are here for
// the same reason the webhook path applies storableProviderText before it
// records: a provider string that cannot be stored must not be written raw.
//
// A destination identifier longer than the grammar CHECK allows, or an image
// URL longer than the evidence CHECK allows, would fail a check violation that
// nothing translates — so the whole transfer would roll back and every later
// attempt for that payment would fail the same way, a permanent 500 against a
// working provider, discovered by a customer. Neither is a refusal of the
// payment, so neither is reported as one: a transfer whose answer this build
// cannot keep is an error the use case surfaces, and the payment stays
// `created` with no destination recorded — the state the contract already
// describes for a provider that never answered.
const (
	maxStorableQRURLLength = 2048
	maxStorableRefLength   = 255
)

// instructions translates the provider's answer into the port's description of
// a destination, and returns every string BYTE FOR BYTE.
//
// Nothing here parses any of them, and that is a rule rather than an oversight:
// the port returns them verbatim and its own comment says why — they are
// promises the provider makes and this process keeps on the provider's behalf,
// and a consumer that decomposed one would be interpreting a value whose only
// promise is that it describes a destination to a human. The one transformation
// between the provider's bytes and these fields is the JSON string decode that
// reading any body requires.
//
// THE HOLDER'S NAME is taken from the virtual account's own field when the
// provider states one, and from the underlying bank account's field when it
// does not. Both name the holder of a destination and only one of them names
// the destination the customer is about to pay into, so the preference is
// specificity rather than a fallback between unrelated values; an answer that
// states neither is refused, because a destination this platform cannot name a
// holder for is one the domain refuses to record anyway, and failing here says
// so in the provider's own context.
//
// THE AMOUNT IS CHECKED AND NOT TRUSTED. The provider echoes the amount the
// destination was issued for, and a destination issued for a figure other than
// the one the payment is denominated in is a payment no customer can settle
// correctly: the money would arrive, the delivery would name it, and the
// application's own amount check would quarantine it against a payment whose
// amount is the one this platform priced. Refusing to show the destination at
// all is the only point at which that is still recoverable, which is why the
// check lives here.
//
// It is strict about DISAGREEMENT and tolerant of ABSENCE, and the asymmetry is
// deliberate rather than lazy. An absent amount is independently covered — the
// webhook's own amount is checked against the payment before any credit, so an
// unverified figure can never reach the ledger — while a PRESENT and different
// figure is a contradiction between the provider's answer and this platform's
// record, and a build that shrugged at it would be showing a customer an
// account that cannot settle the payment it belongs to. Under a check whose
// absence is covered elsewhere, refusing the absent case as well would trade a
// real capability for no additional safety.
func (r orderResponse) instructions(requested int64) (payments.TransferInstructions, error) {
	if r.Data == nil {
		return payments.TransferInstructions{}, errors.New("the order answer carried no data member, which the provider's contract requires")
	}
	data := r.Data

	switch {
	case data.VANumber == nil:
		return payments.TransferInstructions{}, errors.New("the order answer carried no virtual account number, which the provider's contract requires")
	case *data.VANumber == "":
		return payments.TransferInstructions{}, errors.New("the order answer carried an empty virtual account number, so there is no destination and nothing for a later delivery to name")
	case len(*data.VANumber) > maxStorableRefLength:
		return payments.TransferInstructions{}, fmt.Errorf("the order answer carried a virtual account number of %d bytes, which is longer than the %d this build can record; a truncated reference would be a different reference, and the delivery naming it could not be matched back to this payment",
			len(*data.VANumber), maxStorableRefLength)
	case data.BankName == nil || *data.BankName == "":
		return payments.TransferInstructions{}, errors.New("the order answer named no bank, so there is nowhere to say the money should go")
	case len(*data.BankName) > maxStorableRefLength:
		return payments.TransferInstructions{}, fmt.Errorf("the order answer carried a bank name of %d bytes, which is longer than the %d this build can record", len(*data.BankName), maxStorableRefLength)
	}

	holder := data.VAHolderName
	if holder == nil || *holder == "" {
		holder = data.AccountHolderName
	}
	switch {
	case holder == nil || *holder == "":
		return payments.TransferInstructions{}, errors.New("the order answer named no account holder, so a customer cannot check who they are sending money to")
	case len(*holder) > maxStorableRefLength:
		return payments.TransferInstructions{}, fmt.Errorf("the order answer carried an account holder name of %d bytes, which is longer than the %d this build can record", len(*holder), maxStorableRefLength)
	}

	if data.Amount != nil && *data.Amount != requested {
		return payments.TransferInstructions{}, fmt.Errorf("the order answer was issued for %d where this payment is denominated in %d, so the destination could not settle it", *data.Amount, requested)
	}

	// The image is optional in exactly one direction: absent, or stated as the
	// empty string, are both "the provider drew none" — which the port carries
	// as the empty string and the console renders as an account a customer can
	// type. A destination is complete without it, and refusing a payment over a
	// decoration would be refusing money.
	qrURL := ""
	if data.QRCodeURL != nil {
		qrURL = *data.QRCodeURL
	}
	if len(qrURL) > maxStorableQRURLLength {
		return payments.TransferInstructions{}, fmt.Errorf("the order answer carried a QR image url of %d bytes, which is longer than the %d this build can keep for a customer to scan",
			len(qrURL), maxStorableQRURLLength)
	}

	return payments.TransferInstructions{
		TransferCode:  *data.VANumber,
		BankName:      *data.BankName,
		AccountHolder: *holder,
		QRURL:         qrURL,
	}, nil
}

// orderCode derives the provider's order code from the port's idempotency key,
// and refuses a key that cannot become one.
//
// The transformation is uppercasing and deleting every character the provider's
// grammar does not admit — this platform's keys are spelled with separators
// that carry meaning to a human reading them and none to the provider — and it
// is applied to the CALLER'S key rather than to something minted here, which is
// what keeps the property the whole method depends on: one attempt, one key,
// one code, and therefore a conflict that means "this exact request was already
// made".
//
// The code is a NEW identifier this adapter mints rather than a provider value
// being stored, and the difference is what makes the truncation below
// admissible where truncating a provider's identifier is not. Nothing is
// resolved by an order code; it is this platform's name for its own request at
// the provider. A collision would be refused by the provider as a duplicate —
// an abandoned attempt with a clear operator-facing message — and can never
// become a wrong credit, because the destination that could fund a payment is
// still resolved through the virtual account the provider issued.
//
// The length floor is the provider's and it is enforced rather than padded: a
// key too short to become a code is a caller that has not supplied the stable
// identity this method requires, and padding it would be manufacturing the
// identity the caller was supposed to provide.
func orderCode(idempotencyKey string) (string, error) {
	var code strings.Builder
	for _, r := range strings.ToUpper(idempotencyKey) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			code.WriteRune(r)
		default:
			// Deleted rather than replaced: a separator is this platform's
			// spelling and the provider's grammar has no room for it. The
			// mapping from text to code is not injective — two keys differing
			// only in their separators become one code — and that is deliberate
			// and safe, because the caller supplies one key per attempt and the
			// provider refuses a second order under a code it already holds.
		}
	}
	derived := code.String()
	if len(derived) < minOrderCodeLength {
		return "", fmt.Errorf("paymentprovider: the idempotency key yields a %d-character order code and the provider requires at least %d, so it cannot name this request at the provider", len(derived), minOrderCodeLength)
	}
	if len(derived) > maxOrderCodeLength {
		// A prefix of an already-unique string is still unique enough for a name
		// nothing resolves by: see the comment above on why truncation is
		// admissible for a code this adapter mints. The prefix is taken from the
		// front, which is where a versioned identifier's distinguishing bits
		// are.
		derived = derived[:maxOrderCodeLength]
	}
	return derived, nil
}

// orderAmount converts the port's integer minor units into the integer the
// provider's `amount` field carries.
//
// The provider's amount is an integer in the currency's own smallest unit, and
// the port's AmountMinorUnits is an integer in exactly that unit — the same
// quantity the ledger stores. For this provider's settlement currency the
// exponent is zero, so the smallest unit IS the currency unit and this
// conversion is the identity. Saying that plainly is not a shortcut: there is
// no arithmetic here that COULD round, which is the point. A conversion that
// scaled the figure — to major units, to a hundredth of the stored unit, to a
// decimal string with a fixed number of places — is where an amount stops being
// the amount, and that conversion would live in this function and nowhere else.
// A rounded amount is a request for a figure nobody agreed to, and the ledger
// would then be credited a different figure than the customer paid.
//
// There is deliberately NO upper bound here, and its absence is a decision
// worth stating because the provider this adapter replaced carried one. That
// bound was a property of the card processor's amount ENCODING — a field with a
// fixed number of digits — and it was wrong for this provider in the direction
// that matters: a domestic transfer's amount is stated in whole units of a
// currency with no minor unit, so the largest top-up this platform sells is a
// eight- or nine-digit figure that a card processor's encoding would have
// refused. What bounds a top-up here is a POLICY — the largest amount this
// deployment sells — and a policy belongs in configuration, where an operator
// changes it, rather than in the adapter that encodes it.
//
// A non-positive amount is refused, mirroring the domain, which refuses a
// top-up of nothing before it ever reaches this adapter.
func orderAmount(minorUnits int64) (int64, error) {
	if minorUnits <= 0 {
		return 0, fmt.Errorf("paymentprovider: an order for %d minor units is not a payment", minorUnits)
	}
	return minorUnits, nil
}

// orderDuration converts how long the destination should stay valid into the
// provider's `duration` field, which its documentation states in SECONDS.
//
// The unit is the entire content of this function and it is worth stating
// because it is the one field on this seam where a plausible wrong answer is
// silent. A duration of 1800 read as minutes would issue an account valid for
// thirty hours instead of thirty minutes; read as milliseconds it would be
// under a second, and every customer would find their account already gone.
// Neither would raise anything anywhere — the provider would issue a valid
// account, a customer would transfer to it, and the money would be applied to a
// payment this platform had stopped waiting for, or would arrive at an account
// the provider had already expired.
//
// A duration that is not a whole number of seconds is refused rather than
// rounded, on the same rule the amount follows: a rounded lifetime is a
// lifetime nobody chose. The application's own window is a whole number of
// seconds, so this refusal is not reachable from the console.
func orderDuration(expiresIn time.Duration) (int64, error) {
	if expiresIn <= 0 {
		return 0, fmt.Errorf("paymentprovider: an order that expires after %s would issue an account nobody could pay", expiresIn)
	}
	if expiresIn%time.Second != 0 {
		return 0, fmt.Errorf("paymentprovider: a lifetime of %s is not a whole number of seconds, and the provider's duration field states seconds; this build refuses to round a lifetime rather than issue an account that expires at a moment nobody chose", expiresIn)
	}
	return int64(expiresIn / time.Second), nil
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
