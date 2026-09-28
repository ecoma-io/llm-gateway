package http

import (
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"strings"

	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/payments"
)

const (
	// maxWebhookBody bounds how much of a delivery this endpoint reads. It is
	// well above any real event — a provider's payload is a couple of kilobytes
	// — and it exists so a caller cannot make this process buffer an unbounded
	// body in order to be told the body was the wrong length.
	//
	// The bound is applied to the READ rather than to the decoder, and the
	// difference from decodeJSONBody is the whole reason this endpoint does not
	// use it. stdhttp.MaxBytesReader refuses by poisoning the connection as well
	// as the body: net/http cannot return a connection it has not drained to the
	// pool, so it closes it after the response. This endpoint is called by a
	// provider that batches deliveries, over a connection it keeps — so refusing
	// one oversized delivery with MaxBytesReader would cost every other delivery
	// that connection was carrying. A limit on the reader refuses the delivery
	// and nothing else.
	//
	// Reading the body directly is also the only option that produces the BYTES
	// the signature covers. A decoder could not be used here at all: a signature
	// is computed over bytes, and a re-encoded object has lost the whitespace,
	// key order and number spellings the provider signed.
	maxWebhookBody = 64 << 10
)

// handleProviderWebhook is POST /payment-webhooks/{provider}: the payment
// provider's own delivery endpoint, and the only operation on this surface no
// browser may reach.
//
// It is authenticated by a signature over the raw body and NOT by a session, so
// it resolves no Principal, takes no account and reads no cookie. Its row in
// routes.go is marked guardServerToServer for that reason: the browser guards
// (origin, content-type, double-submit) would refuse every real delivery, and a
// provider has no origin, no CSRF token and no session to present. It also sets
// no CORS header, ever: a browser has no business reading this response, and a
// response that admitted one would be advertising an endpoint whose every
// authentication fact is a secret the browser would then be handling.
//
// The status contract is one sentence — 2xx means "this event needs no further
// delivery from you" — and the branches below are that sentence made total:
//
//   - The path names a provider THIS PROCESS DOES NOT SERVE: 404, and the
//     delivery is not read at all. The endpoint is a provider's own, so it is
//     addressed to a provider by name, and a name this deployment does not
//     answer for is a URL mistake rather than a delivery that failed to
//     authenticate. A 4xx keeps it out of a retry budget either way; the 404 is
//     what stops the operator from diagnosing a typo in an endpoint URL as a
//     signature problem.
//   - The delivery MAY NOT BE ACTED ON, AND NEVER WILL BE: 400, NOTHING
//     written. Five things earn it, and every one of them is a fact about the
//     DELIVERY rather than a verdict on its contents — which is the property
//     that makes the status safe to hand a provider, because a 4xx says "do not
//     send this again" and the provider's next attempt carries these same bytes.
//     In the order the branches below run: the body declared a Content-Encoding
//     this endpoint will not decode; it declared a Content-Type that cannot
//     carry the JSON it verifies; it is larger than the read bound; the delivery
//     did not authenticate — the signature header absent, appearing more than
//     once, or unequal to what this deployment computes over these exact bytes;
//     or it authenticated and is STALE, arriving outside the freshness
//     tolerance, which is a replay rather than a delivery. Each has its own log
//     line, so an operator can tell a forged delivery from a replayed one from a
//     malformed one. An unverified body is an attacker's free text, and a
//     quarantine row keyed on it would make the quarantine table writable by
//     anyone who can reach this port.
//   - The delivery authenticated and this build cannot read an event id out of
//     it: 200, and still NOTHING written. A row keyed on an absent event id
//     would swallow the NEXT unreadable delivery as a duplicate of this one, and
//     reporting a real event as already-seen is worse than reporting nothing.
//     The operator reads the log line; the provider is told to stop.
//   - The delivery authenticated and named an event: the application applies it
//     and answers 200 for all three dispositions — applied, duplicate and
//     quarantined — with one identical body. A kind this build has no rule for
//     is NOT this handler's business: the verifier returns a populated event and
//     the APPLICATION quarantines it with a reason, so nothing here inspects or
//     classifies a kind.
//   - The delivery was verified and could not be recorded: the seam's error,
//     which is the one case a redelivery can change, and therefore the only 5xx.
//
// The handler makes no outbound call of any kind. It verifies locally and hands
// the event to a seam; opening a checkout, calling the provider back to ask what
// it meant, or fetching a payment from it are all operations this path does not
// perform, because a delivery endpoint that calls out is an endpoint whose
// availability depends on the thing it is trying to talk about.
func handleProviderWebhook(surface PaymentSurface) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		// The path segment is checked FIRST, before a header is read or a byte of
		// the body is, and it is the only check on this row that is about the
		// address rather than the message. A delivery to a provider this process
		// does not serve is answered 404, which is the one status that says
		// "there is nothing here" rather than "what you sent was wrong" — and the
		// distinction is worth a branch, because the alternative is what this row
		// did before it had one: every delivery to any path was verified against
		// the single configured secret, so a mistyped endpoint looked exactly
		// like a forged delivery and the provider read 400 "did not
		// authenticate" for a URL that was never served.
		//
		// Nothing is recorded either way. This is a routing refusal, and the
		// bytes have not been read yet.
		if provider := r.PathValue("provider"); !surface.servesProvider(provider) {
			logWebhook(r, "the delivery is addressed to provider %q, which this deployment does not serve; nothing was read and nothing was recorded", provider)
			writeError(w, r, notFoundError{})
			return
		}

		// Both header checks run before a byte of the body is read. A compressed
		// body is not the body that was signed, and this endpoint refuses to
		// interpret one rather than gambling that the provider's signature was
		// computed over the compressed bytes; a body whose declared type cannot
		// be JSON cannot be a signed event this build reads.
		if encoding := nonIdentityEncoding(r); encoding != "" {
			logWebhook(r, "the delivery declared Content-Encoding %q, so these are not the bytes that were signed; nothing was recorded", encoding)
			writeError(w, r, &badRequestError{cause: fmt.Errorf("the delivery declared Content-Encoding %q", encoding)})
			return
		}
		if !webhookContentTypeIsJSON(r) {
			// EVERY declared value is logged, not the first, because the check
			// above refuses on every one of them and the line has to say what
			// was actually refused. `application/json` followed by `text/plain`
			// is refused BY the second value, and a line quoting only the first
			// would read as this endpoint refusing the one type it accepts.
			declared := strings.Join(r.Header.Values("Content-Type"), ", ")
			logWebhook(r, "the delivery declared Content-Type %q, which cannot carry the JSON this endpoint verifies; nothing was recorded", declared)
			writeError(w, r, &badRequestError{cause: fmt.Errorf("the delivery declared Content-Type %q", declared)})
			return
		}

		// The body is read ONCE, and this slice is what is verified and then what
		// the application is handed. A second read would return whatever is left
		// of the stream, and the bytes that were verified and the bytes that were
		// interpreted would no longer be the same message.
		rawBody, oversized, err := readWebhookBody(r)
		if err != nil {
			// The body could not be read at all. Nothing was recorded, and a
			// redelivery is the correct next step, so this is a 5xx.
			logWebhook(r, "the delivery body could not be read: %T", err)
			writeError(w, r, application.Internal(err))
			return
		}
		if oversized {
			// The provider's next attempt carries the same oversized bytes, so a
			// retry could only repeat this — which is why this is a refusal and
			// not a 5xx. The body is deliberately not read to its end.
			//
			// The line names the BOUND and not the size that arrived, and the
			// absence is the bound working rather than an omission: the one
			// number an operator would rather have is how big the delivery was,
			// and producing it would mean reading a body precisely because it
			// is too large to read. `readWebhookBody` stops one byte past the
			// cap, so "larger than 65536" is everything this endpoint is
			// entitled to know.
			logWebhook(r, "the delivery body is larger than the %d bytes this endpoint reads; nothing was recorded", maxWebhookBody)
			writeError(w, r, &badRequestError{cause: fmt.Errorf("the body is larger than %d bytes", maxWebhookBody)})
			return
		}

		event, err := surface.Verifier.Verify(webhookHeaders(r), rawBody)
		switch {
		case err == nil:
			// Authenticated, and the claims are the application's to resolve.
		case errors.Is(err, payments.ErrBadSignature):
			logWebhook(r, "the delivery did not authenticate; nothing was recorded, and the same bytes will not authenticate on a retry")
			writeError(w, r, &badRequestError{cause: err})
			return
		case errors.Is(err, ErrStaleDelivery):
			logWebhook(r, "the delivery authenticated but arrived outside the freshness tolerance; nothing was recorded, and a redelivery carries the same timestamp")
			writeError(w, r, &badRequestError{cause: err})
			return
		case errors.Is(err, payments.ErrMalformedEvent):
			// Authenticated, and unnameable. Answering 200 rather than 4xx is the
			// application's contract and the port's own comment carries the
			// argument: an unreadable delivery is not an attack and not a
			// provider bug this build can act on, and the provider's next attempt
			// would carry these very bytes.
			logWebhook(r, "the delivery authenticated and carries no readable event id, so there is nothing to record it under; nothing was written and the provider is told to stop")
			writeJSON(w, stdhttp.StatusOK, webhookAcknowledgement{Received: true})
			return
		default:
			// A verification failure this transport cannot classify. Nothing has
			// been recorded — that is what makes this answerable — and a 5xx is
			// the only status that asks for the delivery again.
			logWebhook(r, "the delivery could not be verified: %T", err)
			writeError(w, r, application.Internal(err))
			return
		}

		// The event AND the bytes travel together, and the bytes are the same
		// slice that was just verified: the application records what the provider
		// signed rather than this build's re-encoding of it.
		//
		// The outcome is deliberately discarded. All three dispositions answer
		// with one identical body — the contract's rule, not a simplification —
		// and the disposition itself is durable in the application's own record,
		// so a handler that rendered or logged it would be the first place a
		// branch on it could be added.
		if _, err := surface.Payments.ApplyProviderEvent(r.Context(), WebhookDelivery{Event: event, RawBody: rawBody}); err != nil {
			// The seam's answer decides the status, as it does on every other row:
			// an internal cause is a 500, which is the one case in which nothing
			// was durably recorded and a redelivery can change the outcome.
			logWebhook(r, "the delivery was verified and could not be recorded: %T", err)
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, webhookAcknowledgement{Received: true})
	}
}

// handleBeginCheckout is POST /payment-intents: a customer funding their account
// against one of this deployment's top-up offers.
//
// The account is the SESSION's — resolveSession resolved it and it is passed as
// this seam's first argument — and there is no account field in the request body
// for a caller to set, which is ADR 0012 §2 stated as a signature. A payment is
// money, and "which account funds itself" is the last question a browser should
// be able to answer about a payment.
//
// The body is decoded with the surface's own decoder, so a malformed body is the
// same 400 with the same envelope as every other write here. A blank offer and a
// blank idempotency key are NOT refused here: an offer's vocabulary is the
// deployment's configuration and a key's meaning is the application's, so the
// layer that knows what either should have been is the layer that refuses, with
// a message that names it.
//
// 201 is the answer on both the first call and a converged repeat, and the
// handler renders the same body either way: the contract states why — the client
// asked for a payment and now holds one, and which of the two happened is not a
// difference a page may act on. The 503 for a provider that did not answer is
// not this handler's to produce either; it arrives as an error from the seam and
// renders through the one error translation point.
func handleBeginCheckout(sessions SessionUseCases, useCases PaymentUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		sessionPrincipal, ok := resolveSession(w, r, sessions)
		if !ok {
			return
		}
		var in beginCheckoutRequest
		if err := decodeJSONBody(w, r, &in); err != nil {
			writeError(w, r, err)
			return
		}
		result, err := useCases.BeginCheckout(r.Context(), accountOf(sessionPrincipal), in.Offer, in.IdempotencyKey)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusCreated, renderPaymentIntent(result.Intent))
	}
}

// handleListTopUpOffers is GET /top-up-offers: the prices this deployment sells,
// which is what a chooser renders and what POST /payment-intents is called with.
//
// It resolves a session and passes NO account to the seam, and both halves of
// that are deliberate. The session is required because publishing a price list to
// anonymous callers is not a decision this surface has made — every product
// operation here is behind the cookie. The account is absent because an offer is
// not a fact about a customer: it is configuration, the same list answers every
// session that may fund itself, and taking an account in order to ignore it would
// imply a scoping this read does not have.
//
// There is no paging and no filter, and the handler therefore reads neither. A
// fixed list of a handful of prices has no cursor to parse, no position to name
// and no total worth maintaining, and the contract's schema says all of that at
// length rather than leaving it to a client to discover.
func handleListTopUpOffers(sessions SessionUseCases, useCases PaymentUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if _, ok := resolveSession(w, r, sessions); !ok {
			return
		}
		result, err := useCases.ListTopUpOffers(r.Context())
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, renderTopUpOfferList(result))
	}
}

// handleListPayments is GET /payment-intents: the account's payments, newest
// first.
//
// It is the same shape as every other list on this surface — resolve the
// session, read the two paging parameters, ask the seam, render the page — and
// the account is the session's, carried into the port's own predicate rather
// than applied to rows already fetched. Nothing here decides anything about a
// payment: a status was written by a signed delivery the provider sent, and this
// handler renders what the application recorded.
func handleListPayments(sessions SessionUseCases, useCases PaymentUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		sessionPrincipal, ok := resolveSession(w, r, sessions)
		if !ok {
			return
		}
		ask, err := paging(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		result, err := useCases.ListPayments(r.Context(), accountOf(sessionPrincipal), ask.After, ask.Limit)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, newPage(result.Items, result.HasMore, result.NextCursor, renderPaymentIntent))
	}
}

// readWebhookBody reads one delivery, bounded, and reports whether it exceeded
// the bound.
//
// The limit is maxWebhookBody + 1 so that a body of exactly the cap is read
// whole and a body one byte over it is detected without reading the rest: a
// bound checked as "read the cap, then ask if there is more" needs the same
// extra read, and a bound checked after an unbounded read is not a bound.
//
// It never returns a partially read body as a success. A delivery that arrived
// truncated is not a shorter delivery — its signature covers bytes that did not
// arrive — so a read error is the caller's error, not a shorter slice.
func readWebhookBody(r *stdhttp.Request) (body []byte, oversized bool, err error) {
	if r.Body == nil {
		return nil, false, errors.New("the delivery carried no body")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		return nil, false, err
	}
	if len(raw) > maxWebhookBody {
		return nil, true, nil
	}
	return raw, false, nil
}

// webhookHeaders copies the request's headers for the verifier.
//
// The multiplicity is preserved — every value of every name, not the first, and
// not a joined string — because a signature header that appears twice is a
// request that is refused rather than one that is resolved to either value, and
// a verifier handed one string could not tell. The map is a COPY: the request's
// own header map is live server state, and a verifier that appended to a slice
// it was handed would be editing the request.
func webhookHeaders(r *stdhttp.Request) map[string][]string {
	headers := make(map[string][]string, len(r.Header))
	for name, values := range r.Header {
		headers[name] = append([]string(nil), values...)
	}
	return headers
}

// nonIdentityEncoding returns the first declared content encoding that is not
// one of the two spellings of "the body is the body" — absent and `identity` —
// or the empty string when the delivery declares none.
//
// A compressed body is refused because a signature covers BYTES: whatever a
// provider signed, it did not sign the compressed form of it, and a build that
// decompressed before verifying would be verifying a message it produced. The
// check is exhaustive over the header's values rather than reading the first,
// for the same reason the signature's multiplicity is preserved: `identity`
// followed by `gzip` is a gzip body.
func nonIdentityEncoding(r *stdhttp.Request) string {
	for _, value := range r.Header.Values("Content-Encoding") {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || strings.EqualFold(trimmed, "identity") {
			continue
		}
		return value
	}
	return ""
}

// webhookContentTypeIsJSON reports whether a delivery declares a content type
// that can carry the JSON this endpoint verifies.
//
// It is deliberately LOOSE, and the looseness is the requirement rather than a
// tolerance: a real delivery arrives as `application/json; charset=utf-8` from
// most providers and through most proxies, and a check that compared the header
// to `application/json` would refuse deliveries whose bytes are exactly what was
// signed. So the media type is parsed, its parameters are discarded, case is
// folded, and the `+json` suffix is accepted — the shapes a JSON body legally
// wears. An absent header is accepted too: a delivery that declared nothing
// declared nothing wrong, and the signature is the authentication here, not the
// label.
//
// What is refused is a media type that cannot be JSON at all, because a
// signature over such a body is a signature over something this build will not
// interpret, and refusing it keeps the delivery out of the provider's retry
// budget.
//
// EVERY declared value is checked, not the first, for the reason
// nonIdentityEncoding gives: this header is a refusal gate, and the two ways to
// read a header that appears twice are to take one of the two or to refuse the
// request. `Header.Get` is the first of those, and it is exactly as wrong here
// as it would be there — `application/json` followed by `text/plain` is a
// delivery that declares two incompatible things about itself, and answering
// "the first one said JSON, so this is JSON" is a reader picking a winner
// between two claims rather than noticing that there are two. A value that is
// present but empty declared nothing, and is skipped for the same reason an
// absent header is accepted: the signature is the authentication here.
func webhookContentTypeIsJSON(r *stdhttp.Request) bool {
	for _, declared := range r.Header.Values("Content-Type") {
		if strings.TrimSpace(declared) == "" {
			continue
		}
		mediaType, _, err := mime.ParseMediaType(declared)
		if err != nil {
			return false
		}
		mediaType = strings.ToLower(mediaType)
		if mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
			return false
		}
	}
	return true
}

// logWebhook writes this endpoint's one line about a delivery it refused or
// could not read, against the request identifier every other line about that
// request carries.
//
// It exists because a delivery leaves no row on most of the paths that reach it:
// an unauthenticated body is deliberately not stored, and a delivery with no
// readable event id has no key to be stored under, so the log line is the ONLY
// record of those events.
//
// What a line may carry is therefore worth being exact about, because the paths
// that log are exactly the paths where the input is attacker-shaped:
//
//   - Never a signature, a credential, an authorization header, or a byte of
//     the body. None of those is printed by any call site, and the rule is
//     stated here so that the next one does not become the first.
//   - A provider identifier appearing in the PATH, and the two refusal-shaped
//     header values (Content-Encoding, Content-Type), ARE quoted — they are the
//     subject of the line, and a line that refused a delivery without saying
//     what it declared is not an answer to "why was this refused". Both are
//     quoted with %q, which is load-bearing rather than cosmetic: %q escapes
//     newlines and control characters, so a header value cannot forge a second
//     log line or reopen the field structure around it.
func logWebhook(r *stdhttp.Request, format string, args ...any) {
	requestID, _ := RequestIDFromContext(r.Context())
	log.Printf("%s request_id=%s webhook: %s", serviceName, requestID, fmt.Sprintf(format, args...))
}
