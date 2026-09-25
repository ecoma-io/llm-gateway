package management

import (
	"errors"
	"log"
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/usagefacts"
)

// The failure vocabulary of the private protocol this listener speaks, stated in
// docs/architecture/cross-plane-protocols.md.
//
// It overlaps the façade's rather than being derived from it, and the overlap is
// not inheritance. Both surfaces answer in the same envelope — the two are hops
// of one chain, and an envelope per hop would mean a translation whose only
// content was a rename — but the set of failures is each surface's own. Nothing
// in api/openapi/shared/errors.yaml describes this listener: that enum is the
// façade's, the façade is what a contracted caller reaches, and this process is
// one it reaches in turn. `upstream_unavailable` is the clearest case and the
// reason the two sets are written down separately: it is the façade's word for
// "the Data Plane did not answer", which is a sentence this Data Plane could
// only tell about itself.
//
// They are spelled as literals rather than imported from anywhere for the
// reason every constant on a wire boundary is: the definition is the document,
// and a constant shared across two processes would be a second definition
// wearing a name.
const (
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
	codeInvalidRequest   = "invalid_request"
	codeUnauthenticated  = "unauthenticated"
	codeCursorExpired    = "cursor_expired"
	codeInternal         = "internal"
)

// errorEnvelope is the whole failure shape of this surface.
//
// It is the gateway's own envelope and not the runtime's OpenAI-compatible
// error: a management caller is a peer application inside the deployment, and
// the OpenAI shape exists for a client this surface never has. The runtime
// answers with the other one from the other listener, in the same process,
// about the same gateway — which is why the split is worth stating rather than
// assuming.
type errorEnvelope struct {
	Error     errorBody `json:"error"`
	RequestID string    `json:"request_id"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// failure is one complete, decided wire failure. Every path out of this package
// that is not a success builds one of these, so the status, the code and the
// message are chosen together and cannot drift into a combination the contract
// does not declare.
type failure struct {
	status  int
	code    string
	message string
	// internal marks a failure whose cause exists but must not be serialized.
	// It is what makes the log line below possible without putting the cause on
	// the wire.
	internal bool
	// cause is what actually went wrong, for the log line only. It rides the
	// failure instead of being logged at the mapping site because the request
	// ID — the one handle that ties the log line to the response an operator
	// is holding — is resolved where the failure is written, and a cause
	// logged without its request ID is a second mystery.
	cause error
}

func notFoundFailure() failure {
	return failure{
		status:  stdhttp.StatusNotFound,
		code:    codeNotFound,
		message: "the requested path is not served by this management API",
	}
}

func methodNotAllowedFailure() failure {
	return failure{
		status:  stdhttp.StatusMethodNotAllowed,
		code:    codeMethodNotAllowed,
		message: "this path does not accept the request method",
	}
}

func invalidRequestFailure(message string) failure {
	return failure{status: stdhttp.StatusBadRequest, code: codeInvalidRequest, message: message}
}

func unauthenticatedFailure() failure {
	return failure{
		status:  stdhttp.StatusUnauthorized,
		code:    codeUnauthenticated,
		message: "the request did not identify itself as a service this surface accepts",
	}
}

// internalFailureWithCause is the internal failure carrying what went wrong
// for the log line. The wire answer is fixed — 500, the internal code, the
// one message — because a management caller can act on a status and a request
// ID and cannot act on a driver error; the cause is what the operator reading
// the log line needs, and it never reaches the envelope. Every internal
// failure this surface decides has a cause, so there is no cause-less
// constructor to forget one with.
func internalFailureWithCause(cause error) failure {
	return failure{
		status:   stdhttp.StatusInternalServerError,
		code:     codeInternal,
		message:  internalErrorMessage,
		internal: true,
		cause:    cause,
	}
}

// writeFailure serializes a decided failure and logs it when its cause is not
// for the wire.
//
// The request ID is resolved here rather than trusted from the context. The
// middleware installs one on every request this handler can see, but a
// guarantee that only holds because another function ran first is not one this
// function can make, and the contract promises the header on every response
// including the ones produced before routing.
//
// The cause, when there is one, is logged against that request ID — the full
// error chain, driver prose and all, because the log line is where the
// operator's investigation starts and the wire is where it must never end up.
func writeFailure(w stdhttp.ResponseWriter, r *stdhttp.Request, f failure) {
	requestID, ok := RequestIDFromContext(r.Context())
	if !ok || requestID == "" {
		requestID = newRequestID()
	}
	w.Header().Set(RequestIDHeader, requestID)
	if f.internal {
		if f.cause != nil {
			log.Printf("%s management request_id=%s internal error: %v", serviceName, requestID, f.cause)
		} else {
			log.Printf("%s management request_id=%s internal error", serviceName, requestID)
		}
	}
	writeJSON(w, f.status, errorEnvelope{Error: errorBody{Code: f.code, Message: f.message}, RequestID: requestID})
}

// failureFor maps an error the application returned onto this surface's
// vocabulary.
//
// The two fact-feed errors are the only ones that carry meaning across the
// boundary, and they do not share an answer: an expired cursor is a position
// this Data Plane will never be able to replay and a human has to decide what
// happens to the gap, while an unavailable source is a read that will succeed
// later and should be retried. Collapsing them into one status would tell a
// consumer either to retry forever or to give up on a transient failure.
//
// Anything unrecognised — including an internal application error — becomes the
// generic 500 with the fixed public message. The cause is logged against the
// request ID and never serialized: a management caller can act on a status and
// a request ID, and cannot act on a query, a path or a driver error.
func failureFor(err error) failure {
	switch {
	case errors.Is(err, usagefacts.ErrCursorExpired):
		return failure{
			status:  stdhttp.StatusGone,
			code:    codeCursorExpired,
			message: "the requested position is no longer replayable",
		}
	case errors.Is(err, usagefacts.ErrSourceUnavailable):
		// This listener answers from its own process, so an unavailable source
		// is this Data Plane's own storage and not a peer it could not reach:
		// there is no upstream to blame and the 502 the facade returns would be
		// a lie told here.
		return internalFailureWithCause(err)
	}

	if applicationError, ok := application.As(err); ok {
		switch applicationError.Code {
		case application.CodeNotFound:
			return failure{
				status:  stdhttp.StatusNotFound,
				code:    codeNotFound,
				message: applicationError.Message,
			}
		case application.CodeInternal:
			return internalFailureWithCause(err)
		}
	}
	return internalFailureWithCause(err)
}
