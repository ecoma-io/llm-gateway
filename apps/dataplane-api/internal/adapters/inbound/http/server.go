// Package http is the dataplane-api's inbound HTTP adapter: the Data Plane's
// management surface, which is internal-only. It is not reachable from a
// browser and is never documented in the console's contract (ADR 0006 §11).
//
// It owns transport concerns — routing, middleware, request identifiers, the
// service-caller check and wire errors — then calls the application for
// use-case work. Health and readiness remain infrastructure-level; GET /version
// proves the HTTP → application → response path every domain endpoint follows;
// and GET /internal/usage-events is the first operation that answers from the
// Data Plane rather than from this process, reached through the outbound port
// rather than read here. The public contract is api/openapi/dataplane.yaml; it
// changes before this package does, never after.
//
// Nothing here decides a business rule. A handler translates a request into a
// call, and the application's answer or typed error back into a response; the
// one thing this package owns outright is the shape of the wire — status,
// envelope, headers — which is what an inbound adapter is for. Two consequences
// are worth naming, because both are easy to get wrong in a way that looks like
// a shortcut and is not: nothing here reads Data Plane state directly (there is
// no client in this package, only a port), and nothing here remembers a page,
// a cursor or a fact between requests.
//
// Nor does anything here serve a runtime request. The OpenAI-compatible
// surface belongs to apps/dataplane: a `/v1/...` route registered in this
// application would put a management process on the LLM path, and
// routes_test.go fails if one appears.
package http

import (
	"encoding/json"
	"errors"
	"log"
	stdhttp "net/http"
	"path"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

const (
	// serviceName names this application in the log lines below. Each Go
	// application in this repository carries its own copy of this transport
	// kit, and the name is what a reader of a shared log stream gets instead
	// of three identical unlabelled lines.
	serviceName = "dataplane-api"

	// RequestIDHeader is the one request-correlation header this application
	// accepts and returns. Its spelling matches api/openapi/dataplane.yaml
	// exactly.
	RequestIDHeader = "X-Request-Id"

	// internalErrorMessage is intentionally less specific than any operational
	// cause. A client can act on the status and request ID; it must not learn
	// about a database, a provider, a path, or a stack frame it cannot fix.
	internalErrorMessage = "internal error"
)

// New returns the dataplane-api's HTTP handler: middleware first, routes after.
//
// The surface itself is declared in routes.go and mounted here; this function
// owns everything around it — the middleware, the two fallbacks below, and the
// order they are composed in. The authenticator is passed through to the route
// table rather than held here: which rows are protected is a column of that
// table, and a mux-level check would have to decide which paths are exempt —
// the probes — from a list kept somewhere other than the rows themselves.
func New(app *application.App, authenticator dataplane.Authenticator) stdhttp.Handler {
	mux := stdhttp.NewServeMux()

	for _, rt := range routes(app) {
		register(mux, rt, authenticator)
	}

	// The fallback every other path and method lands on. Unmatched paths are
	// not an operation in api/openapi/dataplane.yaml, but their envelope is
	// contracted in its description: JSON, code "not_found", request ID.
	mux.HandleFunc("/", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		writeError(w, r, notFoundError{})
	})

	// ServeMux rewrites a non-canonical path — a doubled slash, a "." or ".."
	// segment — into an HTML 301/307 before routing, a response shape the
	// contract does not have. The path as requested matches no operation, so
	// the transport answers it here instead of letting the redirect through,
	// and every reachable response stays inside api/openapi/dataplane.yaml.
	handler := stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		// The structure is judged on the escaped path, not the decoded one:
		// %2F is a character inside one segment, and judging the decoded form
		// would refuse names whose encoding merely looks like a doubled
		// slash or a dot segment — names the contract promises travel as one
		// segment and the group grammar admits.
		escaped := r.URL.EscapedPath()
		if escaped != cleanPath(escaped) {
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
// shape and trailing newline are exactly what api/openapi/dataplane.yaml
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
	case application.CodeCursorExpired:
		// 410 rather than 404 or 500: the position existed and the Data Plane
		// has aged it out, which is a state the caller can act on — its stored
		// cursor is unusable and reconciliation is the only way forward. The
		// message is the application's, and it is fixed there; only the status
		// and the machine-readable code are decided here.
		return stdhttp.StatusGone, string(application.CodeCursorExpired), applicationError.Message
	case application.CodeUpstreamUnavailable:
		// 502: the gateway reached for an answer and did not get one. The
		// alternative — a 500 — would blame this process for a failure that is
		// not its own, and would tell the caller to look in the wrong place.
		return stdhttp.StatusBadGateway, string(application.CodeUpstreamUnavailable), applicationError.Message
	default:
		return stdhttp.StatusInternalServerError, string(application.CodeInternal), internalErrorMessage
	}
}

func writeJSON(w stdhttp.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	// HTML escaping is off, and on this surface that is a correctness setting
	// rather than a preference. The default rewrites `<`, `>` and `&` inside
	// every string into < and friends — sensible for a JSON document
	// embedded in a page, wrong for a value travelling between two services.
	// The usage-fact cursor is opaque and must cross byte for byte, and a
	// cursor containing an `&` is a legitimate cursor: escaping it would be
	// this transport rewriting a value it is forbidden to interpret. The
	// setting applies to every body this application writes, which is
	// consistent — none of them is HTML, and valid JSON is what the contract
	// asks for either way.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
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
