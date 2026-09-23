// Package http is the dataplane's inbound HTTP adapter: the runtime's public
// surface, and the one everything on the LLM request path arrives through.
//
// It owns transport concerns — routing, middleware, request identifiers and
// wire errors — then calls the application for use-case work. Product endpoints
// do not exist yet: health and readiness remain infrastructure-level, while
// GET /version proves the HTTP → application → response path every domain
// endpoint will follow. The public contract is api/openapi/openapi.yaml; it
// changes before this package does, never after.
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
	// accepts and returns. Its spelling matches api/openapi/openapi.yaml
	// exactly.
	RequestIDHeader = "X-Request-Id"

	// internalErrorMessage is intentionally less specific than any operational
	// cause. A client can act on the status and request ID; it must not learn
	// about a database, a provider, a path, or a stack frame it cannot fix.
	internalErrorMessage = "internal error"
)

// New returns the dataplane's HTTP handler: middleware first, routes after.
//
// The mux's method patterns ("GET /healthz") mean the method is part of the
// route. ServeMux answers a mismatched method with a plain-text 405, so every
// route also carries a method-agnostic companion pattern below that answers
// with the contract's JSON envelope instead — an ordinary handler, not a
// ResponseWriter wrapper that would silently strip streaming interfaces such
// as Flusher from every response this service will ever write.
func New(app *application.App) stdhttp.Handler {
	mux := stdhttp.NewServeMux()

	// Liveness: the process is up and its loop is turning. Anything about
	// whether the gateway could do useful work — a dependency reachable, a
	// cache warm — is readiness's job and never appears here, so an
	// orchestrator restarting on /healthz never kills a process for a
	// downstream outage it cannot fix.
	registerGET(mux, "/healthz", func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		writeStatus(w)
	})

	// Readiness: the scaffold has no dependencies, so it is always ready.
	// The checks that will gate this endpoint later — a database ping, an
	// upstream probe — hang off here, and only here.
	registerGET(mux, "/readyz", func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		writeStatus(w)
	})

	// Version flows through the application rather than reading main's stamp
	// directly: cmd/dataplane owns the one ldflags version source and hands it
	// to application.New, and this handler reads the result across the same
	// boundary every future domain endpoint will use.
	registerGET(mux, "/version", func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		writeJSON(w, stdhttp.StatusOK, versionResponse{Version: app.Version()})
	})

	// The fallback every other path and method lands on. Unmatched paths are
	// not an operation in api/openapi/openapi.yaml, but their envelope is
	// contracted in its description: JSON, code "not_found", request ID.
	mux.HandleFunc("/", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		writeError(w, r, notFoundError{})
	})

	// ServeMux rewrites a non-canonical path — a doubled slash, a "." or ".."
	// segment — into an HTML 301/307 before routing, a response shape the
	// contract does not have. The path as requested matches no operation, so
	// the transport answers it here instead of letting the redirect through,
	// and every reachable response stays inside api/openapi/openapi.yaml.
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

// registerGET mounts a GET route with its method-agnostic companion, which
// owns every other method on the path. GET patterns already match HEAD, so
// the companion answers exactly the methods the resource refuses.
func registerGET(mux *stdhttp.ServeMux, path string, handler stdhttp.HandlerFunc) {
	mux.HandleFunc("GET "+path, handler)
	mux.HandleFunc(path, func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, r, methodNotAllowedError{})
	})
}

// writeStatus is the one definition of what a health endpoint's body looks
// like, shared by /healthz and /readyz so the two cannot drift apart; the
// shape and trailing newline are exactly what api/openapi/openapi.yaml
// documents.
func writeStatus(w stdhttp.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(stdhttp.StatusOK)
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}

// versionResponse is the one wire shape for a version answer, mirroring
// api/openapi/openapi.yaml's Version schema field for field. The application
// returns the bare value; serialization stays on this side of the boundary,
// exactly as it does for the error envelope below.
type versionResponse struct {
	Version string `json:"version"`
}

// errorEnvelope is the one wire shape for failures, mirroring
// api/openapi/openapi.yaml's ErrorEnvelope field for field.
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
