// Package management is the dataplane's private inbound HTTP adapter: the
// surface the Data Plane's management traffic arrives on, served from a
// listener of its own.
//
// A second listener rather than a route on the runtime's mux, and that is the
// whole reason this package exists. The runtime's surface is reached by whoever
// holds an API key and is reached constantly; this one is reached by the
// Control Plane's management façade, on a port an operator does not publish.
// Mounting these routes on the runtime would put an administrative surface on
// the address that serves LLM traffic — same process, same port, same exposure
// — and the two are separated precisely so that they cannot be exposed
// together by accident. routes_test.go in the runtime's package still fails if
// `/internal/` appears on its table; this package is where that path lives
// instead.
//
// It owns no state, and the shape of the code says so: every handler here
// translates a request into a call on the application and the answer back into
// the management envelope. There is no cache, no cursor, no store, and no
// second copy of a fact. The one thing this package does own outright is the
// boundary's authentication — who may call a management operation at all — and
// it owns it in front of the application rather than inside it, because a
// caller that has not identified itself as a service must not reach use-case
// code even by accident.
//
// The public contract is api/openapi/dataplane.yaml, which this surface and the
// management façade both implement: it changes before this package does, never
// after.
package management

import (
	"encoding/json"
	"log"
	stdhttp "net/http"
	"path"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
)

const (
	// serviceName names this process in the log lines below, matching the
	// runtime surface's own value: this is one process with two listeners, and
	// a reader of a shared log stream is looking for the process first.
	serviceName = "dataplane"

	// RequestIDHeader is the one request-correlation header this surface
	// accepts and returns. Its spelling matches api/openapi/shared/probes.yaml
	// exactly, as the runtime's does.
	RequestIDHeader = "X-Request-Id"

	// internalErrorMessage is deliberately less specific than any operational
	// cause. A management caller can act on the status and the request ID; it
	// must not learn about a database, a connection string or a stack frame it
	// cannot fix.
	internalErrorMessage = "internal error"
)

// New returns the management listener's handler: middleware first, routes
// after.
//
// The credential is the shared secret deployment configured for this surface,
// and it is a required argument rather than something a handler looks up. An
// empty credential authenticates nobody (see auth.go), so a composition root
// that forgets to pass one gets a listener that refuses every request instead
// of a listener that accepts every request — the failure is loud and closed.
func New(app *application.App, credential string) stdhttp.Handler {
	mux := stdhttp.NewServeMux()

	for _, rt := range routes(app) {
		register(mux, rt, credential)
	}

	// The fallback every other path lands on. An unmatched path is not an
	// operation in api/openapi/dataplane.yaml, but its response is contracted
	// in the descriptions there: JSON, the management envelope, and the request
	// ID in the header.
	mux.HandleFunc("/", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		writeFailure(w, r, notFoundFailure())
	})

	// ServeMux rewrites a non-canonical path — a doubled slash, a "." or ".."
	// segment — into an HTML 301/307 before routing, a response shape the
	// contract does not have. The path as requested matches no operation, so
	// the transport answers it here instead of letting the redirect through,
	// and every reachable response stays inside api/openapi/dataplane.yaml.
	handler := stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if r.URL.Path != cleanPath(r.URL.Path) {
			writeFailure(w, r, notFoundFailure())
			return
		}
		mux.ServeHTTP(w, r)
	})

	return requestID(handler)
}

// cleanPath returns the canonical form ServeMux would rewrite to, mirroring
// net/http's own helper: path.Clean with any original trailing slash restored.
//
// It is a copy of the runtime surface's helper rather than an import of it. The
// kit each inbound surface carries — this, the request-ID middleware, the JSON
// writer — is deliberately per-surface: the runtime answers untrusted LLM
// clients and this answers peer services, and the day one of those two needs a
// different treatment of a caller-supplied path or identifier, it must not be
// the other one's behaviour that changes with it. The duplication is a
// paragraph, not a library; a third surface here would be the moment to
// revisit it.
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

// writeJSON is this surface's one response writer. Keeping it a function rather
// than a method on a wrapper type is what keeps the streaming interfaces a
// future management operation might need — Flusher above all — reachable
// through the handler chain.
func writeJSON(w stdhttp.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		// Status and headers are already on the wire; a retry cannot repair a
		// half-written response. Log the fact without echoing the payload.
		log.Printf("%s management response JSON write failed: %T", serviceName, err)
	}
}

// writeStatus is the one definition of what a probe body looks like, shared by
// /healthz and /readyz so the two cannot drift apart; the shape and trailing
// newline are exactly what api/openapi/shared/probes.yaml documents.
func writeStatus(w stdhttp.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(stdhttp.StatusOK)
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}

// versionResponse is the one wire shape for a version answer, mirroring the
// shared Version schema field for field. The application returns the bare
// value; serialization stays on this side of the boundary.
type versionResponse struct {
	Version string `json:"version"`
}
