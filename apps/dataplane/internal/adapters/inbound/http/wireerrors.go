package http

import (
	"encoding/json"
	"errors"
	"log"
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
)

// The runtime's error protocol, in one file, because it is one decision.
//
// The two other applications in this repository answer their callers with the
// ErrorEnvelope declared in api/openapi/shared/errors.yaml — a gateway-designed
// shape for a caller that is the gateway's own console. This application does
// not. Its caller is an OpenAI-compatible client written somewhere else,
// parsing `error.type` and `error.message` because that is what the API it was
// written against sends; handing that client a gateway envelope is not a
// cosmetic difference, it is a client that cannot report the failure. The two
// vocabularies are therefore independent contracts with independent documents
// (api/openapi/runtime.yaml and shared/runtime-errors.yaml versus
// api/openapi/shared/errors.yaml), and this file is the runtime's side of that
// split. See ADR 0006 §11.
//
// There are three ways a runtime failure reaches — or fails to reach — a
// client, and they are distinguished by one moment: the first content-bearing
// byte written to the client.
//
//   - Before that moment the failure is ordinary HTTP: a status code and the
//     JSON body below. Nothing has been sent, so the status is still the
//     client's to act on, and a caller that tries one model and then another
//     (ADR 0002) still sees exactly one error.
//   - After it, no status can be revised. The failure is written into the
//     stream instead: one `data:` frame carrying the same JSON body, then the
//     terminal `data: [DONE]` frame, then close. A client already rendering
//     content learns the answer ended in failure rather than waiting for a
//     connection that will never finish.
//   - A client that disconnected is told nothing, because there is nothing left
//     to tell it. The observation is a usage fact, not a wire response.
//
// X-Request-Id is unaffected by all of this: it is a header on every response
// this application writes, including the ones written here, and it stays the
// correlation handle an operator has for a request whose failure the client
// could not read.

// The caller-facing failure classes this runtime can produce, in the
// vocabulary RuntimeErrorBody declares in shared/runtime-errors.yaml. The
// schema's enum is wider than this list on purpose — it is the compatibility
// surface other callers already branch on — but an error type this runtime
// cannot produce has no constant here, so that adding one is a visible edit.
const (
	// typeInvalidRequest says the request itself is the problem: a path this
	// surface does not serve, or a method the path does not accept. A client
	// should not retry it unchanged.
	typeInvalidRequest = "invalid_request_error"

	// typeNotFound says the addressed resource does not exist. The runtime
	// distinguishes it from typeInvalidRequest because a client that resolves a
	// name before using it — a model alias, a project — branches on the
	// difference.
	typeNotFound = "not_found_error"

	// typeAPIError says the runtime failed at something the request was entitled
	// to ask for. It is the bucket for an unclassifiable implementation failure
	// and for a contracted operation that is not built, because in both cases
	// the caller's request was not what failed.
	typeAPIError = "api_error"
)

// codeNotImplemented is the one machine-readable refinement this runtime
// produces today; the schema's `code` is otherwise null. It exists so a caller
// can tell "this surface owns the path and the capability does not exist yet"
// from "this surface does not own the path" — both of which are non-retryable,
// and only one of which is worth a bug report.
const codeNotImplemented = "not_implemented"

// runtimeErrorBody is the JSON body of every runtime failure, on either
// channel: `error.message` and `error.type` are required by the schema, `param`
// and `code` are always serialized and null when the failure is not about one
// parameter or has no refinement. The nulls are emitted rather than omitted
// because an OpenAI-compatible client reads these keys positionally as often as
// by name.
type runtimeErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// runtimeErrorResponse is the top-level body: the error object under one key,
// and nothing beside it. The request identifier travels in X-Request-Id and
// deliberately not here, so the body a client parses is exactly the one its
// OpenAI client already knows.
type runtimeErrorResponse struct {
	Error runtimeErrorBody `json:"error"`
}

// stringCode returns a pointer to a non-empty code, or nil so the field
// serializes as null.
func stringCode(code string) *string {
	if code == "" {
		return nil
	}
	return &code
}

// wireFailure is one failure, fully classified: the status, the body a client
// parses, and whether the runtime could classify it at all. The last field is
// not a fourth status — it is the difference between a failure this
// application understood well enough to describe and one it did not, and only
// the second is worth an operator's log line.
type wireFailure struct {
	status   int
	body     runtimeErrorBody
	internal bool
}

// writeError is the one place a runtime failure becomes bytes. Every handler in
// this package reaches it, and nothing else in the package decides a status
// from an error.
//
// It sets the response's request identifier itself rather than assuming the
// middleware already did. Through New the two write the same value and the
// second write is invisible — but the contract promises X-Request-Id on every
// response this application produces, and a guarantee that only holds when
// another function ran first is not one this function can make. It is also what
// lets a test exercise the error path without composing the whole server.
//
// An unclassified failure is logged as a correlation fact — the request
// identifier and nothing else. Its operational cause is not logged here, and
// the request identifier is bounded and grammar-checked by requestID while a
// cause is neither.
func writeError(w stdhttp.ResponseWriter, r *stdhttp.Request, err error) {
	failure := errorResponse(err)
	requestID, ok := RequestIDFromContext(r.Context())
	if !ok || requestID == "" {
		// Reachable only for a handler called without the middleware — the
		// contract's header guarantee still has to hold, so an identifier is
		// minted here rather than left empty.
		requestID = newRequestID()
	}
	w.Header().Set(RequestIDHeader, requestID)
	if failure.internal {
		log.Printf("%s request_id=%s internal error", serviceName, requestID)
	}
	writeJSON(w, failure.status, runtimeErrorResponse{Error: failure.body})
}

// errorResponse maps one error to its deterministic status and wire body. It is
// the whole status mapping for this surface.
//
// Two sources are consulted in order. A transport failure answers for itself —
// it is a fact about the request that no use case produced. Everything else is
// an application error, and an application error this code does not recognize —
// including one whose Code this package has never heard of — normalizes to a
// generic api_error rather than letting a future implementation category reach
// the wire through an enum with no member for it.
func errorResponse(err error) wireFailure {
	var transportFailure interface {
		error
		wireFailure() wireFailure
	}
	if errors.As(err, &transportFailure) {
		return transportFailure.wireFailure()
	}

	applicationError, ok := application.As(err)
	if !ok {
		return internalFailure()
	}
	if applicationError.Code == application.CodeNotFound {
		return wireFailure{
			status: stdhttp.StatusNotFound,
			body: runtimeErrorBody{
				Message: applicationError.Message,
				Type:    typeNotFound,
			},
		}
	}
	return internalFailure()
}

// internalFailure is the runtime's answer to a failure it cannot place: one
// fixed message, one fixed class, and the flag that sends the correlation fact
// to the log. Nothing from the cause crosses it.
func internalFailure() wireFailure {
	return wireFailure{
		status:   stdhttp.StatusInternalServerError,
		body:     runtimeErrorBody{Message: internalErrorMessage, Type: typeAPIError},
		internal: true,
	}
}

// notFoundError is the transport fact that no route matched. It maps itself
// rather than passing through the application, whose vocabulary is for
// resources its use-cases own.
//
// The message names the contract rather than echoing the requested path: a path
// is caller-shaped input, and the response is a shape an operator reads in a
// log after stripping identifiers. The path is in the access log, once.
type notFoundError struct{}

func (notFoundError) Error() string {
	return "resource not found"
}

func (notFoundError) wireFailure() wireFailure {
	return wireFailure{
		status: stdhttp.StatusNotFound,
		body: runtimeErrorBody{
			Message: "the requested path is not served by this runtime",
			Type:    typeNotFound,
		},
	}
}

// notImplementedError is the transport fact that the path is contracted and the
// operation behind it is not built. Like notFoundError it maps itself: the
// application has no use-case to refuse with, because there is no use-case —
// answering 501 is the transport telling the truth about a surface the contract
// describes and the code does not yet provide.
//
// It is a transport error rather than an application one for the same reason it
// exists: the day the runtime routes a completion, this disappears, and an
// application error code would have to be deleted along with it. The class is
// api_error rather than invalid_request_error because the request was not what
// failed — which is the difference between a caller retrying with a different
// body and a caller giving up, and the one distinction this error exists to
// make.
type notImplementedError struct{}

func (notImplementedError) Error() string {
	return "not implemented"
}

func (notImplementedError) wireFailure() wireFailure {
	return wireFailure{
		status: stdhttp.StatusNotImplemented,
		body: runtimeErrorBody{
			Message: "this operation is contracted and not implemented",
			Type:    typeAPIError,
			Code:    stringCode(codeNotImplemented),
		},
	}
}

// methodNotAllowedError is the transport fact that the path exists but the
// method does not. The Allow header, set by the companion route in register, is
// the machine-readable half of the answer; this body is the caller-facing one.
type methodNotAllowedError struct{}

func (methodNotAllowedError) Error() string {
	return "method not allowed"
}

func (methodNotAllowedError) wireFailure() wireFailure {
	return wireFailure{
		status: stdhttp.StatusMethodNotAllowed,
		body: runtimeErrorBody{
			Message: "this path does not accept the request method",
			Type:    typeInvalidRequest,
		},
	}
}

// streamDoneFrame is the terminal Server-Sent Event frame of an
// OpenAI-compatible stream. It closes every stream this runtime writes — a
// completed answer and a failed one alike — because a client that sees it knows
// the answer is over, while a client whose connection simply ends cannot tell
// an interrupted answer from a finished one.
var streamDoneFrame = []byte("data: [DONE]\n\n")

// streamErrorFrame renders a failure as the one Server-Sent Event frame that
// reports it on a stream that has already been committed.
//
// It is a pure function of the error, so the bytes can be asserted in a test
// without a socket, a handler or a flusher.
func streamErrorFrame(err error) []byte {
	return eventFrame(errorResponse(err).body)
}

// eventFrame renders one `data:` frame around a runtime error body.
func eventFrame(body runtimeErrorBody) []byte {
	payload, marshalErr := json.Marshal(runtimeErrorResponse{Error: body})
	if marshalErr != nil {
		// Unreachable: every field is a string or a nullable string pointer, so
		// there is nothing here for the encoder to reject. The fallback exists
		// because the alternative — writing a truncated frame — would leave a
		// committed stream with no parseable terminal event at all, which is the
		// failure this whole path exists to prevent.
		payload = []byte(`{"error":{"message":"internal error","type":"api_error","param":null,"code":null}}`)
	}
	frame := make([]byte, 0, len("data: ")+len(payload)+2)
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	return append(frame, '\n', '\n')
}

// writeStreamError reports a failure on a response that has already been
// committed: the error frame, the terminal [DONE] frame, and a flush so both
// leave the process rather than waiting in a buffer for content that will never
// come.
//
// It returns no status and cannot fail in the caller's terms. By the time it is
// called the status line is already on the wire, so there is nothing left to
// revise and nothing for a caller to branch on — which is exactly why the
// runtime treats post-commitment failure as a different problem from
// pre-commitment failure rather than as another error to be classified.
func writeStreamError(w stdhttp.ResponseWriter, err error) {
	_, _ = w.Write(streamErrorFrame(err))
	_, _ = w.Write(streamDoneFrame)
	if flusher, flushes := w.(stdhttp.Flusher); flushes {
		flusher.Flush()
	}
}
