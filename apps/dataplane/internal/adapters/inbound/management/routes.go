package management

import (
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
)

// route is one endpoint of this surface.
//
// The authentication decision belongs to the route rather than to the listener,
// because the contract makes it per operation: the probes declare `security: []`
// — an orchestrator asking whether this process is alive should not have to hold
// a deployment secret to be told "ok" — while the fact feed declares the service
// credential. A listener-wide guard would contradict the document in one
// direction or the other, and the document is the source.
type route struct {
	method        string
	path          string
	handler       stdhttp.HandlerFunc
	authenticated bool
}

// routes returns this surface's HTTP surface, in the order a reader meets it.
//
// The table is the surface's single source of truth: New registers from it and
// routes_test.go pins it by literal, so an endpoint cannot exist in the server
// and be missing from the inventory a reviewer reads — and, more to the point
// here, nobody can add a second management operation to this application without
// editing the line that says what the surface is.
//
// The table is also where the split from the runtime surface stays visible: the
// three probes are the shared convention every application in this repository
// serves, and everything under /internal/ is this listener's alone.
func routes(app *application.App) []route {
	return []route{
		// Liveness of the process. The runtime's listener answers the same
		// question on its own port; both are the same process, and an
		// orchestrator may ask it on either.
		{
			method:  stdhttp.MethodGet,
			path:    "/healthz",
			handler: func(w stdhttp.ResponseWriter, _ *stdhttp.Request) { writeStatus(w) },
		},
		// Readiness of the management surface. The scaffold has no dependencies,
		// so it is always ready; the check that will gate this endpoint later —
		// the fact store reachable, the accounting schema migrated — hangs off
		// here and only here.
		{
			method:  stdhttp.MethodGet,
			path:    "/readyz",
			handler: func(w stdhttp.ResponseWriter, _ *stdhttp.Request) { writeStatus(w) },
		},
		// Version flows through the application rather than reading main's stamp
		// directly: cmd/dataplane owns the one ldflags version source and hands
		// it to application.New.
		{
			method: stdhttp.MethodGet,
			path:   "/version",
			handler: func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
				writeJSON(w, stdhttp.StatusOK, versionResponse{Version: app.Version()})
			},
		},
		// The fact feed: the first cross-plane operation, and the first one on
		// this surface that a caller authenticates for. The
		// path string is the one the façade contracts, and it is the same
		// string here on purpose rather than by coincidence — the façade names
		// the path itself rather than rewriting one it was given, so there is
		// no translation step for the two ends to disagree about. What the
		// façade does rewrite is the answer: it builds its own envelope and its
		// own statuses from what this listener said. The protocol itself,
		// and the reason it is a page in a document rather than a fourth
		// OpenAPI file, are in docs/architecture/cross-plane-protocols.md.
		{
			method:        stdhttp.MethodGet,
			path:          "/internal/usage-events",
			handler:       usageEvents(app),
			authenticated: true,
		},
		// The catalog read the Control Plane's commerce roll makes: for one
		// alias-group name, which catalog version is currently current, so an
		// entitlement can pin that version's immutable id as its scope. It is
		// the second authenticated row, and the guard is written so that a
		// future unauthenticated row stays a visible decision on the row rather
		// than an omission nobody reads — a catalog fact is exactly the thing
		// ADR 0006 §7 says never leaves except to the other plane's service
		// identity.
		//
		// The same string-not-rewritten rule holds: groupVersionPath is what
		// the façade calls, and the path template lives in
		// api/openapi/dataplane.yaml because from the Control Plane's side of
		// the seam this operation is ordinary contract surface. Here it is the
		// private protocol's path — the segment the caller names travels
		// through it unmodified, and what this listener answers is a catalog
		// fact, not a page of a feed: three fields, one of them the id the
		// control database stores.
		{
			method:        stdhttp.MethodGet,
			path:          groupVersionPath,
			handler:       currentGroupVersion(app),
			authenticated: true,
		},
	}
}

// register mounts one route along with its method-agnostic companion, which
// owns every other method on the path. ServeMux answers a mismatched method with
// a plain-text 405, so the companion answers with the contract's JSON envelope
// instead — an ordinary handler, not a ResponseWriter wrapper that would strip
// streaming interfaces from every response this surface will ever write.
//
// An authenticated route's companion is authenticated too. The alternative —
// letting an unauthenticated caller learn that a path exists but not that it
// accepts another method — would be a distinction without a difference, since
// the path is published in a contract, and it would make the guard depend on
// which method the caller chose.
func register(mux *stdhttp.ServeMux, rt route, credential string) {
	allowed := rt.method
	if rt.method == stdhttp.MethodGet {
		allowed = stdhttp.MethodGet + ", " + stdhttp.MethodHead
	}

	mux.HandleFunc(rt.method+" "+rt.path, guard(rt, credential))
	mux.HandleFunc(rt.path, guard(route{authenticated: rt.authenticated, handler: func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Allow", allowed)
		writeFailure(w, r, methodNotAllowedFailure())
	}}, credential))
}

// guard wraps a handler in the service-credential check.
//
// It runs before the handler, so a caller that has not identified itself as a
// service never reaches use-case code — not even the route's own argument
// parsing. The check is a wrapper rather than a step inside each handler
// because the failure mode of the second is one handler that forgets it, and
// that handler would be the only one with no protection while every test of the
// others stayed green.
func guard(rt route, credential string) stdhttp.HandlerFunc {
	if !rt.authenticated {
		return rt.handler
	}
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if !authorised(r, credential) {
			writeFailure(w, r, unauthenticatedFailure())
			return
		}
		rt.handler(w, r)
	}
}
