package http

import (
	"errors"
	"io"
	"log"
	stdhttp "net/http"
	"strconv"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// The chat completion handler: the endpoint on the LLM request path, and the
// one route whose work is admission.
//
// The handler's own share of that work is deliberately small, and every step
// of it is ordered by cost: shape the credential header, verify the
// credential, then read the body, then admit. A request refused early costs
// the runtime nothing it did not already spend, and a request that never
// reaches admission writes nothing — the order is the cost model, stated as
// control flow. What the handler owns outright is the translation: requests
// in, admission outcomes out, the wire answer from the table in wiremap.go
// and never from a decision made here. Nothing in this file formats a
// business verdict, and nothing in it logs a secret: the credential, its
// digest and the request body cross no log line, because a shared log stream
// is exactly where they must not land.
//
// The route carries its authentication inside itself rather than in a mux
// wrapper. A wrapper would answer every route's authentication questions
// from one place that only the chat route asked, and it would wrap the
// ResponseWriter — which the register() doctrine forbids, because a wrapper
// that hides Flusher breaks every streamed response this runtime will ever
// write. Per-route is also the honest scope: the probes are not customer
// surfaces and present no credential.

// idempotencyKeyHeader is the request header that makes a retry safe, in the
// spelling the contract declares.
const idempotencyKeyHeader = "Idempotency-Key"

// maxChatBodyBytes is the body bound this endpoint enforces while reading:
// 10485760 bytes, the number the contract documents beside the prose that an
// over-bound body answers 400 and never 413. It is a constant rather than a
// knob on purpose — the bound is part of the request's contract, and an
// answer that depends on how a process was configured is an answer the
// contract cannot promise.
const maxChatBodyBytes = 10485760

// newChatCompletionHandler builds the route's handler over the admission use
// cases the composition root wired. Both are required at the route, not
// checked nowhere: a wiring that left either nil means the endpoint cannot
// do its one job, and the honest answer to a request that arrives is the
// internal failure — never a fabricated refusal that would tell a caller
// their key was bad when the defect is theirs-not.
func newChatCompletionHandler(wiring wiring) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		// The correlation identifier is decided before any work runs and set
		// before any byte is written — a streamed answer's headers freeze at
		// its first content byte, so a guarantee that waits for the answer's
		// writer is no guarantee at all. Every answer this handler produces,
		// on either channel, inherits it by having been set here first.
		requestID, ok := RequestIDFromContext(r.Context())
		if !ok || requestID == "" {
			requestID = newRequestID()
		}
		w.Header().Set(RequestIDHeader, requestID)

		// The credential's header shape, before anything reads a mirror or a
		// body: the cheapest refusal first, and the one that needs nothing
		// from the application at all.
		credential, ok := parseCredential(r)
		if !ok {
			writeChatAnswer(w, r, requestID, unauthenticatedAnswer(unauthenticatedRequest{label: "authorization_header"}))
			return
		}

		// Verification. A refusal is an answer, not a failure; every other
		// error is the mirror's own and lands on the internal one.
		if wiring.auth == nil {
			writeChatAnswer(w, r, requestID, chatAnswer{failure: internalFailure()})
			return
		}
		authenticated, err := authenticate(wiring.auth, r, credential)
		if err != nil {
			writeChatError(w, r, requestID, err, "")
			return
		}

		// The idempotency key's header shape: one header, or none — absent
		// is admission's refusal to make, because the request row it writes
		// is a decision the use case owns. More than one is undecidable at
		// the transport — there is no single key to store — and is refused
		// here, having presented no request the runtime could record.
		keyValues := r.Header.Values(idempotencyKeyHeader)
		if len(keyValues) > 1 {
			writeChatAnswer(w, r, requestID, chatAnswer{
				failure: wireFailure{
					status: stdhttp.StatusBadRequest,
					body: runtimeErrorBody{
						Message: idempotencyKeyMessage,
						Type:    typeInvalidRequest,
						Param:   stringPtr(application.DetailIdempotencyKey),
						Code:    stringCode(codeInvalidRequest),
					},
				},
				reason: codeInvalidRequest,
			})
			return
		}
		idempotencyKey := ""
		if len(keyValues) == 1 {
			idempotencyKey = keyValues[0]
		}

		// The body, read last: every refusal above was cheaper than reading
		// it, and a refusal below is the first one that cost a byte. With it
		// the request acquires its runtime identity — minted here, once, and
		// carried by every answer from here on, because an operator reading a
		// decision must be able to find the request row it decided. With it,
		// too, the request acquires its reply: the channel over which an
		// admitted request's answer travels, built here over this
		// connection's writer and handed to the use case inside the input.
		body := readChatBody(w, r)
		runtimeID := identity.NewRequestID()

		outcome, err := wiring.chat.Serve(r.Context(), application.ChatInput{
			RequestID:      runtimeID,
			Credential:     authenticated.KeyID,
			AccountID:      authenticated.AccountID,
			AccountState:   authenticated.AccountState,
			IdempotencyKey: idempotencyKey,
			Body:           body,
			Reply:          newChatReply(w),
		})
		if err != nil {
			// Admission could not reach a decision. That is never the
			// caller's answer to give: the cause stays behind the boundary
			// and the wire carries the one internal failure. The identity
			// still rides the log — the use case may have left a row behind.
			writeChatError(w, r, requestID, err, runtimeID)
			return
		}
		answer := chatWireCell(outcome)
		// The log names the identity the decision was reached under when the
		// use case names one — the unit's attempt id for anything decided
		// inside a unit of work — and falls back to this arrival's own mint
		// for the answers decided before one opened. The wire body is never
		// touched: the runtime request id is a log fact, not a contract one.
		answer.runtimeID = runtimeID
		if outcome.RuntimeRequestID != "" {
			answer.runtimeID = outcome.RuntimeRequestID
		}
		writeChatAnswer(w, r, requestID, answer)
	}
}

// readChatBody reads the request body under the endpoint's bound, handing
// admission either the bytes or the reason there are none. The bound is
// enforced by the reader itself — a body that crosses it stops the read
// there, rather than being buffered whole and refused after — and the two
// refusals are distinguished because they are different facts for admission
// to record: a body over the bound is the caller's request being too large,
// an unreadable one is the request not arriving whole. Neither body is ever
// logged; a refusal is a fact about length and arrival, not content.
func readChatBody(w stdhttp.ResponseWriter, r *stdhttp.Request) application.RequestBody {
	r.Body = stdhttp.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err == nil {
		return application.BodyBytes(raw)
	}
	var tooLarge *stdhttp.MaxBytesError
	if errors.As(err, &tooLarge) {
		return application.BodyRefused{Reason: application.BodyTooLarge}
	}
	return application.BodyRefused{Reason: application.BodyUnreadable}
}

// unauthenticatedAnswer lifts a credential refusal into the answer the table
// writes, carrying the refusal's log label with it. Every 401 this endpoint
// produces passes through here, which is what keeps them byte-identical:
// the transport's shape refusals and the application's verification verdicts
// share one wire cell by construction, not by convention.
func unauthenticatedAnswer(refusal unauthenticatedRequest) chatAnswer {
	return chatAnswer{
		failure: refusal.wireFailure(),
		reason:  "unauthenticated",
		label:   refusal.label,
	}
}

// writeChatAnswer is the one place a table answer becomes bytes and the log
// line that answers for it. The request identifier is not set here — the
// handler set it before any work ran, because a streamed answer's headers
// freeze at its first content byte, and the answers written through the
// reply channel inherit it by having been set first. A silent answer — one
// whose bytes already travelled through the reply, or whose caller is gone —
// becomes the log line only: its wire was the reply's to shape, and a second
// write here would be a second answer for one request.
//
// The log line is built from the same answer the wire carries, so the two
// renderings cannot disagree; the routing trace, when the answer names one,
// rides the log beside them.
func writeChatAnswer(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID string, answer chatAnswer) {
	line := serviceName + " request_id=" + requestID
	if answer.runtimeID != "" {
		line += " runtime_request_id=" + string(answer.runtimeID)
	}
	if answer.replay {
		line += " replayed=true"
	}
	if answer.label != "" {
		line += " " + answer.label
	}
	if answer.reason != "" {
		line += " reason=" + answer.reason
	}
	if answer.routing != nil {
		line += " alias=" + answer.routing.Alias
		line += " routing_attempts=" + strconv.Itoa(answer.routing.Attempts)
		if answer.routing.LastPosition > 0 {
			line += " last_position=" + strconv.Itoa(answer.routing.LastPosition)
		}
		if answer.routing.LastErrorClass != "" {
			line += " last_error_class=" + string(answer.routing.LastErrorClass)
		}
		if answer.routing.Committed {
			line += " committed=true"
		}
	}
	if answer.failure.internal {
		line += " internal error"
	}
	log.Print(line)

	if answer.silent {
		return
	}
	if answer.retryAfter != "" {
		w.Header().Set(retryAfterHeader, answer.retryAfter)
	}
	if answer.replay {
		w.Header().Set(idempotentReplayHeader, idempotentReplayValue)
		w.Header().Set(originalRequestIDHeader, string(answer.original))
	}
	writeJSON(w, answer.failure.status, runtimeErrorResponse{Error: answer.failure.body})
}

// writeChatError reports a failure that is not an answer: verification's
// store would not answer, or admission could not reach a decision. A
// verification refusal is the one exception it lifts back out — it is an
// answer, the fixed 401, and it goes through unauthenticatedAnswer so that
// its log tokens survive the trip. Everything else goes through the same
// writer with the one internal failure so that even the endpoint's failures
// carry the request identifier and the same log line shape as its answers.
// The runtime identity rides along when the failure happened after one was
// minted — a use case that failed midway may still have left a row behind,
// and the log line is what finds it.
func writeChatError(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID string, err error, runtimeID identity.RequestID) {
	var refusal unauthenticatedRequest
	if errors.As(err, &refusal) {
		writeChatAnswer(w, r, requestID, unauthenticatedAnswer(refusal))
		return
	}
	writeChatAnswer(w, r, requestID, chatAnswer{failure: errorResponse(err), runtimeID: runtimeID})
}
