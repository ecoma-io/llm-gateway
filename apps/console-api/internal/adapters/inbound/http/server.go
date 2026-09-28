// Package http is the console-api's inbound HTTP adapter: the Control Plane's
// public surface, and the only one a browser reaches.
//
// It owns transport concerns — routing, middleware, request identifiers and
// wire errors — then calls the application for use-case work. The three probes
// stay infrastructure-level: liveness is ungated, readiness gates on the
// Control Plane's own store through the narrow port the probe needs, and
// GET /version proves the HTTP → application → response path. The session
// surface is the product half and it ships before any other (ADR 0012 §2): a
// management operation that lands ahead of sign-in mints live credentials for
// anyone who asks. The public contract is api/openapi/console.yaml; it changes
// before this package does, never after.
//
// Nothing here decides a business rule. A handler translates a request into a
// call, and the application's answer or typed error back into a response; the
// one thing this package owns outright is the shape of the wire — status,
// envelope, headers — which is what an inbound adapter is for. What it also owns
// is the four request guards an unsafe method must clear, which are transport
// facts about where a request came from and never use-case decisions; see
// session.go.
package http

import (
	"encoding/json"
	"errors"
	"log"
	stdhttp "net/http"
	"path"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

const (
	// serviceName names this application in the log lines below. Each Go
	// application in this repository carries its own copy of this transport
	// kit, and the name is what a reader of a shared log stream gets instead
	// of three identical unlabelled lines.
	serviceName = "console-api"

	// RequestIDHeader is the one request-correlation header this application
	// accepts and returns. Its spelling matches api/openapi/console.yaml
	// exactly.
	RequestIDHeader = "X-Request-Id"

	// internalErrorMessage is intentionally less specific than any operational
	// cause. A client can act on the status and request ID; it must not learn
	// about a database, a provider, a path, or a stack frame it cannot fix.
	internalErrorMessage = "internal error"

	// notReadyMessage is the same discipline for the readiness probe's one
	// refusal: the client learns that this service cannot receive traffic yet,
	// and nothing about which dependency is missing or why it is not answering.
	notReadyMessage = "the service is not ready to receive traffic"
)

// New returns the console-api's HTTP handler: middleware first, routes after.
//
// readiness is the persistence.Pinger the readiness probe gates on, carried
// alongside the application rather than through it: the probe asks a fact
// about this process's wiring — can the pool answer — and no use case produces
// that, so the narrow port the store already satisfies is the whole of what
// this package is handed for it. A nil readiness is refused here rather than
// answered around, because a probe with no store behind it is exactly the
// static /readyz this replaces.
//
// sessions is the four session operations and reads is the ten product reads,
// behind the two narrow seams in wire.go and readwire_seam.go. Both are refused
// for the same reason and with different reasoning behind it. A nil sessions
// is a server that would answer every product operation as though every caller
// were signed in — which, with the origin, content-type and double-submit
// guards all passing, is exactly the state a CSRF attack is trying to produce.
// A nil reads is a console whose ten screens render nothing, which is a
// missing feature rather than a wrong answer. Neither is worth continuing past,
// and neither is worth answering at request time either: a handler that reached
// a nil seam would panic on the floor of a request goroutine, where the stack
// names a request rather than the wiring that caused it. This is where the
// stack would have said which screen is missing.
//
// The surface itself is declared in routes.go and mounted here; this function
// owns everything around it — the middleware, the two fallbacks below, and the
// order they are composed in.
func New(app *application.App, readiness persistence.Pinger, sessions sessionUseCases, reads consoleReadUseCases) stdhttp.Handler {
	if readiness == nil {
		panic("http: New requires a readiness Pinger; /readyz has nothing to gate on without one")
	}
	if sessions == nil {
		panic("http: New requires the session use cases; the session surface is this service's authentication boundary")
	}
	if reads == nil {
		panic("http: New requires the console read use cases; ten product screens have nothing to render without them")
	}
	mux := stdhttp.NewServeMux()

	table := routes(app, readiness, sessions, reads)
	for _, rt := range table {
		register(mux, rt)
	}
	mountCompanions(mux, table)

	// The fallback every other path and method lands on. Unmatched paths are
	// not an operation in api/openapi/console.yaml, but their envelope is
	// contracted in its description: JSON, code "not_found", request ID.
	mux.HandleFunc("/", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		writeError(w, r, notFoundError{})
	})

	// ServeMux rewrites a non-canonical path — a doubled slash, a "." or ".."
	// segment — into an HTML 301/307 before routing, a response shape the
	// contract does not have. The path as requested matches no operation, so
	// the transport answers it here instead of letting the redirect through,
	// and every reachable response stays inside api/openapi/console.yaml.
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
// shape and trailing newline are exactly what api/openapi/console.yaml
// documents.
//
// It deliberately does NOT set `Cache-Control: no-store`, which is the one
// deliberate divergence from writeJSON. The three probes are the surface's
// documented exemption from the no-store rule (the contract's NoStore header
// says so, and names them by enumeration): a probe carries no account data, no
// principal and no one-time secret, so caching it is harmless. Sending
// no-store on a probe would be a small, defensible over-approximation — but it
// would also mean the probes no longer match the contract's headers, and a
// header that contradicts the document is a defect a client-side test cannot
// distinguish from a bug. The exemption lives in one function, so the three
// probes take it together and a fourth, product endpoint added later cannot
// inherit it by accident — product responses go through writeJSON, which always
// sets no-store.
// The exemption is expressed as a HEADER CLEAR rather than as the absence of a
// call: a probe that reached a writeJSON-shaped handler for any reason — a
// future refactor, a route that forgets which writer it uses — would otherwise
// silently acquire a header the contract does not promise. Clearing it here
// makes the exemption the property of the PROBE, not of the writer that happens
// to be called today, so a fourth probe inherits it and no product response can
// inherit it by accident.
func writeStatus(w stdhttp.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Del("Cache-Control")
	w.WriteHeader(stdhttp.StatusOK)
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}

// writeProbeJSON is the probe-shaped writer for /version, which the contract
// tags as a probe and declares with no Cache-Control header for the same reason
// /healthz and /readyz are exempt: a build version is not account data. It
// renders a value rather than a fixed status body, so it cannot be writeStatus,
// and it shares writeStatus's exemption for the same reason.
func writeProbeJSON(w stdhttp.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Del("Cache-Control")
	w.WriteHeader(stdhttp.StatusOK)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("%s probe JSON write failed: %T", serviceName, err)
	}
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
	// The readiness refusal is not this branch: its dependency name is the one
	// operational fact the operator needs, and notReady writes that line
	// itself before getting here.
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

// writeJSON is the one place a response body is written, and it marks every one
// of them uncacheable.
//
// `Cache-Control: no-store` is the reason the file has a session surface at
// all (ADR 0012 §3): a minted credential is returned exactly once, and a
// response a browser or a shared proxy was allowed to keep turns that once into
// a durable copy — in a disk cache, in the back-forward cache, in a corporate
// proxy's store. There is no way to revoke a copy nobody told the server about,
// so the response that must not be kept is marked uncacheable at the only place
// that can guarantee it. Setting it here rather than per handler is what makes
// it a property of the surface instead of a discipline every future handler has
// to remember.
//
// The three probes are the one exemption, by enumeration, and they are exempt in
// writeStatus rather than here — see there for why an exemption is safer
// written once than filtered per call.
func writeJSON(w stdhttp.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
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

// notReadyError is the transport fact that the process is up and one of its
// own dependencies is not: the readiness probe's one refusal. It maps itself
// like its siblings, because readiness is a fact about this process's wiring
// and no use case produced it. The code is service_unavailable rather than
// internal because the condition is expected to clear — a caller retries
// later rather than differently — and the message is fixed because which
// dependency is missing is the operator's fact, carried by the log line in
// notReady, never the client's.
type notReadyError struct{}

func (notReadyError) Error() string {
	return "not ready"
}

func (notReadyError) response() (int, string, string) {
	return stdhttp.StatusServiceUnavailable, "service_unavailable", notReadyMessage
}
