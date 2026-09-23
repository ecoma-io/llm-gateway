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
// error body, headers — which is what an inbound adapter is for. That error
// body is the runtime's own, not the gateway's shared envelope, and wireerrors.go
// is where the two were separated and why.
//
// This is the runtime's own surface and nobody else's: the Data Plane's
// management transport is a different application (apps/dataplane-api), and
// console or management routes appearing here would put the Control Plane back
// on the hot path this split exists to remove. routes_test.go fails if one
// does.
package http

import (
	"encoding/json"
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
	// not an operation in api/openapi/runtime.yaml, but their response is
	// contracted in the descriptions there: JSON, the runtime error body, and
	// the request ID in the header.
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
// boundary, exactly as it does for the error body in wireerrors.go.
type versionResponse struct {
	Version string `json:"version"`
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
