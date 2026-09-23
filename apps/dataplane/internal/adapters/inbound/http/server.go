// Package http is the dataplane's inbound HTTP adapter: the runtime's public
// surface, and the one everything on the LLM request path arrives through.
//
// It owns transport concerns — routing, middleware, request identifiers and
// wire errors — then calls the application for use-case work. Routing and
// provider behaviour do not exist yet: health and readiness remain
// infrastructure-level, GET /version proves the HTTP → application → response
// path every domain endpoint will follow, and POST /v1/chat/completions is
// contracted and answers 501 — the inference surface's address, held open
// before anything behind it is built. The public contract is
// api/openapi/runtime.yaml; it changes before this package does, never after.
//
// Nothing here decides a business rule. A handler translates a request into a
// call, and the application's answer or typed error back into a response; the
// one thing this package owns outright is the shape of the wire — status,
// envelope, headers — which is what an inbound adapter is for.
//
// This is the runtime's own surface and nobody else's: the Data Plane's
// management transport is a different application (apps/dataplane-api), and
// console or management routes appearing here would put the Control Plane back
// on the hot path this split exists to remove. `internal/arch` fails the build
// if one does.
package http

import (
	"encoding/json"
	"errors"
	"log"
	stdhttp "net/http"
	"path"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
)

const (
	// serviceName names this application in the log lines below. Each Go
	// application in this repository carries its own copy of this transport
	// kit, and the name is what a reader of a shared log stream gets instead
	// of three identical unlabelled lines.
	serviceName = "dataplane"

	// RequestIDHeader is the one request-correlation header this application
	// accepts and returns. Its spelling matches api/openapi/runtime.yaml
	// exactly.
	RequestIDHeader = "X-Request-Id"

	// internalErrorMessage is intentionally less specific than any operational
	// cause. A client can act on the status and request ID; it must not learn
	// about a database, a provider, a path, or a stack frame it cannot fix.
	internalErrorMessage = "internal error"
)

// New returns the dataplane's HTTP handler: middleware first, routes after.
//
// The surface itself is declared in routes.go and mounted here; this function
// owns everything around it — the middleware, the two fallbacks below, and the
// order they are composed in.
func New(app *application.App) stdhttp.Handler {
	mux := stdhttp.NewServeMux()

	for _, rt := range routes(app) {
		register(mux, rt)
	}

	// The fallback every other path and method lands on. Unmatched paths are
	// not an operation in api/openapi/runtime.yaml, but their envelope is
	// contracted in its description: JSON, code "not_found", request ID.
	mux.HandleFunc("/", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		writeError(w, r, notFoundError{})
	})

	// ServeMux rewrites a non-canonical path — a doubled slash, a "." or ".."
	// segment — into an HTML 301/307 before routing, a response shape the
	// contract does not have. The path as requested matches no operation, so
	// the transport answers it here instead of letting the redirect through,
	// and every reachable response stays inside api/openapi/runtime.yaml.
	handler := stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if r.URL.Path != cleanPath(r.URL.Path) {
			writeError(w, r, notFoundError{})
			return
		}
		mux.ServeHTTP(w, r)
	})

	return requestID(handler)
}

// cleanPath returns the canonical form ServeMux would rewrite to, mirroring
// net/http's own helper: path.Clean with any original trailing slash restored.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np
}

// writeStatus is the one definition of what a health endpoint's body looks
// like, shared by /healthz and /readyz so the two cannot drift apart; the
// shape and trailing newline are exactly what api/openapi/runtime.yaml
// documents.
func writeStatus(w stdhttp.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(stdhttp.StatusOK)
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}

// versionResponse is the one wire shape for a version answer, mirroring the
// shared Version schema (api/openapi/shared/probes.yaml) field for field. The
// application returns the bare value; serialization stays on this side of the
// boundary, exactly as it does for the error envelope below.
type versionResponse struct {
	Version string `json:"version"`
}

// errorEnvelope is the one wire shape for failures, mirroring the shared
// ErrorEnvelope (api/openapi/shared/errors.yaml) field for field. The two
// fragments are shared because all three contracts return the same envelope:
// a caller who learns this shape on one surface has learned it on all of
// them.
type errorEnvelope struct {
	Error     errorBody `json:"error"`
	RequestID string    `json:"request_id"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError is the one error translation point. An application error carries
// a typed code; a transport error maps itself; anything unknown — including a
// malformed application code — normalizes to the closed "internal" value
// rather than letting a future implementation category escape through the
// contract's enum.
func writeError(w stdhttp.ResponseWriter, r *stdhttp.Request, err error) {
	status, code, message := errorResponse(err)
	requestID, ok := RequestIDFromContext(r.Context())
	if !ok || requestID == "" {
		// Unreachable through New, whose middleware installs an identifier for
		// every request — but the envelope's contract guarantees a non-empty
		// request ID in both body and header, so writeError upholds that on its
		// own rather than trusting its caller.
		requestID = newRequestID()
		w.Header().Set(RequestIDHeader, requestID)
	}
	if code == string(application.CodeInternal) {
		// The request identifier is bounded and grammar-checked by requestID;
		// the operational cause is not, and a caller-shaped error string does
		// not belong in an operator's log line. Log the correlation fact only.
		log.Printf("%s request_id=%s internal error", serviceName, requestID)
	}
	writeJSON(w, status, errorEnvelope{
		Error:     errorBody{Code: code, Message: message},
		RequestID: requestID,
	})
}

// errorResponse maps one error to its deterministic status, wire code and
// public message. It is the whole status mapping; nothing else in the
// package decides a status from an error.
func errorResponse(err error) (status int, code string, message string) {
	var transportError interface {
		error
		response() (int, string, string)
	}
	if errors.As(err, &transportError) {
		return transportError.response()
	}

	applicationError, ok := application.As(err)
	if !ok {
		return stdhttp.StatusInternalServerError, string(application.CodeInternal), internalErrorMessage
	}
	switch applicationError.Code {
	case application.CodeNotFound:
		return stdhttp.StatusNotFound, string(application.CodeNotFound), applicationError.Message
	default:
		return stdhttp.StatusInternalServerError, string(application.CodeInternal), internalErrorMessage
	}
}

func writeJSON(w stdhttp.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		// Status and headers are already on the wire; a retry cannot repair a
		// half-written response. Log the fact without echoing the payload.
		log.Printf("%s response JSON write failed: %T", serviceName, err)
	}
}

// notFoundError is the transport fact that no route matched. It maps itself
// rather than passing through application, whose vocabulary is for resources
// its use-cases own.
type notFoundError struct{}

func (notFoundError) Error() string {
	return "resource not found"
}

func (notFoundError) response() (int, string, string) {
	return stdhttp.StatusNotFound, string(application.CodeNotFound), "resource not found"
}

// notImplementedError is the transport fact that the path is contracted and
// the operation behind it is not built. Like notFoundError it maps itself: the
// application has no use-case to refuse with, because there is no use-case —
// answering 501 is the transport telling the truth about a surface the
// contract describes and the code does not yet provide.
//
// It is a transport error rather than an application one for the same reason
// it exists: the day the runtime routes a completion, this disappears, and an
// application error code would have to be deleted along with it. The wire code
// `not_implemented` is declared in every contract's shared error vocabulary so
// that a caller can tell this apart from `not_found` — "belongs to the runtime
// and is not built" versus "is not a path here".
type notImplementedError struct{}

func (notImplementedError) Error() string {
	return "not implemented"
}

func (notImplementedError) response() (int, string, string) {
	return stdhttp.StatusNotImplemented, "not_implemented", "not implemented"
}

// methodNotAllowedError is the transport fact that the path exists but the
// method does not. The Allow header is set by the companion route, which is
// the one place that knows the methods the path accepts.
type methodNotAllowedError struct{}

func (methodNotAllowedError) Error() string {
	return "method not allowed"
}

func (methodNotAllowedError) response() (int, string, string) {
	return stdhttp.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"
}
