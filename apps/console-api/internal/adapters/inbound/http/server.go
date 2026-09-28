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

	// upstreamUnavailableMessage is what a caller is told when a dependency of
	// this plane did not answer. It is a different sentence from
	// internalErrorMessage because the two ask for different things — this one
	// is worth retrying — and it stays a sentence about the DEPENDENCY rather
	// than about the request, which is the distinction the contract draws:
	// nothing about the caller's request was wrong.
	upstreamUnavailableMessage = "the payment provider is unavailable"

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
// sessions is the four session operations, reads is the ten product reads and
// usage is the credential-authenticated one, behind the three narrow seams in
// wire.go, readwire_seam.go and usage.go. All three are refused for the same
// reason and with different reasoning behind it. A nil sessions is a server
// that would answer every product operation as though every caller were signed
// in — which, with the origin, content-type and double-submit guards all
// passing, is exactly the state a CSRF attack is trying to produce. A nil reads
// is a console whose ten screens render nothing, which is a missing feature
// rather than a wrong answer. A nil usage is one product screen missing, and it
// is refused on the same terms: a missing feature answers 404 at worst, while a
// nil seam panics on the floor of a request goroutine, where the stack names a
// request rather than the wiring that caused it. This is where the stack would
// have said which screen is missing.
//
// The surface itself is declared in routes.go and mounted here; this function
// owns everything around it — the middleware, the two fallbacks below, and the
// order they are composed in.
// The surface itself is declared in routes.go and mounted here; this function
// owns everything around it — the middleware, the two fallbacks below, and the
// order they are composed in.
//
// surface is the payment integration: the four payment use cases and the
// provider's delivery verifier, travelling as one optional argument. It is
// optional in the SIGNATURE and required in every process that serves a console
// — cmd/console-api passes it — and the two are not in tension: the four
// payment rows are part of the declared surface, so a server built without them
// has a complete route table and handlers that refuse, rather than a table with
// a hole in it. See unwiredPaymentSurface for why that is the shape, and why a
// request to an unwired payment operation is a logged 500 rather than a panic.
//
// A caller that supplies a surface must supply BOTH halves: a process that can
// open payments but cannot verify a delivery would create checkouts it can never
// settle, and that is refused here rather than discovered by a customer. More
// than one surface is a wiring mistake with no meaning, so it is refused too.
//
// The surface is the one argument that is not a required positional parameter,
// and the reason is a fact about who calls this function: routes_test.go and
// contract_test.go build a table with nothing but fakes, and the four-argument
// form is what they call. Making the surface a fifth required parameter would
// edit every one of those tests to say "and no payments", which is a line about
// the test rather than about the server.
func New(app *application.App, readiness persistence.Pinger, sessions SessionUseCases, reads ConsoleReadUseCases, usage UsageUseCases, surface ...PaymentSurface) stdhttp.Handler {
	if readiness == nil {
		panic("http: New requires a readiness Pinger; /readyz has nothing to gate on without one")
	}
	if sessions == nil {
		panic("http: New requires the session use cases; the session surface is this service's authentication boundary")
	}
	if reads == nil {
		panic("http: New requires the console read use cases; ten product screens have nothing to render without them")
	}
	if usage == nil {
		panic("http: New requires the usage use cases; the /usage surface has no use case to serve without one")
	}
	if len(surface) > 1 {
		panic("http: New takes one payment surface at most")
	}
	payments := unwiredPaymentSurface()
	if len(surface) == 1 {
		if surface[0].Payments == nil || surface[0].Verifier == nil {
			panic("http: New requires both halves of the payment surface; a server that can open payments but not verify a delivery would open checkouts it can never settle")
		}
		if surface[0].Provider == "" {
			// An empty name is not a missing feature like the two nil halves
			// above — it is a wire defect, and it fails CLOSED but silently:
			// the delivery path's own segment is never empty, so no path would
			// match and every delivery would be answered 404 with the process
			// reporting itself healthy. A wired surface with no name is a
			// composition root that forgot one argument, and the only place
			// that can be caught cheaply is here.
			panic("http: New requires the wired payment surface to name its provider; the delivery path is addressed by that name and an unnamed surface serves no path at all")
		}
		payments = surface[0]
	}
	mux := stdhttp.NewServeMux()

	table := routesWithPayments(app, readiness, sessions, reads, usage, payments)
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
// Principal and no one-time secret, so caching it is harmless. Sending
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
//
// The application's code vocabulary is CLOSED and every member of it has an arm
// below. That is a rule with a test behind it — TestEveryApplicationCodeHasAStatus
// enumerates the codes the application package declares and fails on one this
// table does not answer — because the alternative is what this function used to
// be: a three-arm switch over five codes, where a code with no arm answered 500
// `internal` and the omission was invisible. A status is the one thing a client
// acts on, so a missing arm is not a missing detail; it is a caller doing the
// wrong thing on purpose.
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
	return applicationResponse(applicationError)
}

// applicationResponse is the code-to-status table: one arm per code, and the arm
// is chosen from what the code MEANS rather than from how it is spelled.
//
// It is a function of its own so the test can call it for every code the
// application declares without constructing an error value, and so the reader
// can see the whole mapping in one screen.
//
// The `default` arm is not a fallback for a code this build knows: it is the
// arrival of a code it does not, and it LOGS the code it could not place. That
// line is the difference between a missing arm and a broken server — without it
// the two are one symptom, and the one that gets fixed is the wrong one.
func applicationResponse(applicationError *application.Error) (status int, code string, message string) {
	switch applicationError.Code {
	case application.CodeNotFound:
		return stdhttp.StatusNotFound, string(application.CodeNotFound), applicationError.Message
	case application.CodeInvalidRequest:
		// 400 invalid_request, and the message is forwarded rather than replaced
		// by a fixed one. CodeInvalidRequest says the request is well-formed HTTP
		// and its inputs do not satisfy the contract — a limit outside the
		// bounds, a cursor this surface cannot place, a cursor carried under
		// filters the request no longer makes — and the contract promises the
		// caller which one it was. It has already been reduced to that
		// category by the layer below, which is the only layer that could name
		// an account, and the answers themselves name no account: a page size
		// and a cursor.
		//
		// This branch did not exist and the omission was a live defect rather
		// than a gap: a `limit=abc` on any of the nine lists classified as
		// CodeInvalidRequest, fell to the default, and answered 500 `internal`
		// — a server fault for a value the server was told about and chose not
		// to read, on a surface whose contract states plainly that "not an
		// integer at all" is `400 invalid_request`. A generated client reads 500
		// as retryable and a malformed page size is not retryable.
		return stdhttp.StatusBadRequest, string(application.CodeInvalidRequest), applicationError.Message
	case application.CodeUnauthenticated:
		// 401 with a message that describes THIS credential and no other. The
		// use case writes a fixed sentence on purpose, so every unresolved
		// credential — absent, unknown, retired — is refused identically and
		// the response is not an oracle over the credential space.
		//
		// There is deliberately no WWW-Authenticate challenge: this scheme is a
		// deployment-supplied bearer token (console.yaml's securitySchemes), and
		// naming a realm would tell a caller where to look for credentials
		// without telling this surface anything it would act on.
		//
		// The session surface answers its own 401 through a transport error
		// rather than through this branch (session.go's refusal and the sign-in
		// failure), because a session's refusal has its own message and its own
		// decision about the challenge. This branch is for an APPLICATION error
		// — the usage read's unresolved credential — and its absence was a live
		// defect once that read existed: a credential this surface could not
		// resolve answered 500 `internal`, which a client reads as a server
		// fault and retries, when the honest answer is that the caller must
		// present a credential that resolves.
		//
		// Which is the general shape of this arm, and the defect this table was
		// written for. The code means the caller did not identify itself as a
		// service this plane accepts, the contract declares 401 for it on every
		// operation that can produce it, and it used to fall through to the
		// default: a caller who had not authenticated was told the server had a
		// fault. 500 is the status a generated client retries, and a retry with
		// the same credential fails the same way — so the answer was not merely
		// wrong, it was the one wrong answer that produces traffic.
		return stdhttp.StatusUnauthorized, string(application.CodeUnauthenticated), applicationError.Message
	case application.CodeConflict:
		// 409, and the message is forwarded for the reason InvalidRequest's is:
		// the application has already reduced this to a statement about the
		// request's relationship to the server's state — an account that may not
		// fund itself — and that statement is what the contract promises the
		// caller. It names no account, because the answer does not need one: the
		// caller's own request said which resource it was about.
		return stdhttp.StatusConflict, string(application.CodeConflict), applicationError.Message
	case application.CodeUpstreamUnavailable:
		// 503, and this arm is the closure between the contract and the
		// implementation rather than a nicety: api/openapi/shared/errors.yaml
		// names `upstream_unavailable` as a code "produced by the Console API's
		// payment operations", and api/openapi/console.yaml declares the 503 on
		// POST /payment-intents with a paragraph explaining what it means —
		// "the condition is the provider's and is expected to clear, so the
		// caller retries later rather than differently". Without this arm the
		// code existed in the document and nowhere else: a provider outage
		// arrived as CodeInternal and answered 500, which is the one status a
		// generated client must NOT retry, on the one failure that is safe to.
		//
		// The message is fixed rather than forwarded, for internalErrorMessage's
		// reason: the cause here is a third party's, and a provider's own error
		// prose is written for someone holding a credential.
		return stdhttp.StatusServiceUnavailable, string(application.CodeUpstreamUnavailable), upstreamUnavailableMessage
	case application.CodeInternal:
		// The cause is deliberately not forwarded and never serialized: a client
		// can act on the status and the request identifier, and an operational
		// cause is the operator's fact — writeError logs the correlation line.
		return stdhttp.StatusInternalServerError, string(application.CodeInternal), internalErrorMessage
	default:
		// Unreachable through the application's own vocabulary, and reachable
		// the moment somebody adds a member to it. A code this build cannot place
		// is a 500 — it is not a refusal a caller could act on, and it is not a
		// success — and the log line is what makes it a wiring defect an operator
		// reads rather than a server error a client reports.
		log.Printf("%s unrecognised application code %q: answering %d %s",
			serviceName, applicationError.Code, stdhttp.StatusInternalServerError, application.CodeInternal)
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

// readTimeoutMessage is the public sentence for a read that outran its budget.
// It is fixed and names no figure, no range and no account: a caller learns
// that the answer was not computed and can retry the same request, and learns
// nothing about what it would have said.
const readTimeoutMessage = "the service could not complete this read within its time budget"

// readTimeoutError is the transport fact that a read exceeded its deadline. It
// is 503 rather than 500 for the reason notReadyError is: the condition is
// expected to clear, so a caller retries later rather than differently, and the
// contract promises exactly this for the analytics surface ("A caller that
// exceeds it receives HTTP 503 with `service_unavailable`, which is a
// retry-later answer rather than a differently-shaped one").
//
// It exists as a transport error rather than an application code because the
// use case reports the overrun as an internal failure — it cannot know whether
// the caller can retry — and the RETRY decision is this layer's, alongside
// every other status it decides.
type readTimeoutError struct{}

func (readTimeoutError) Error() string {
	return "read timed out"
}

func (readTimeoutError) response() (int, string, string) {
	return stdhttp.StatusServiceUnavailable, "service_unavailable", readTimeoutMessage
}
