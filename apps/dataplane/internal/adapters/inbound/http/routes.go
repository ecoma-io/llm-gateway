package http

import (
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
)

// route is one endpoint of this application's HTTP surface.
//
// The table in routes is that surface's single source of truth: New registers
// from it, and routes_test.go reads it, so an endpoint cannot exist in the
// server and be missing from the inventory a reviewer reads. The scaffold has
// three probes and no runtime endpoint; the table is here because the shape of
// a surface is worth stating in one place before there are twenty rows to
// state it about, and because that is what makes the surface testable as data
// rather than as a series of calls.
//
// For this application the table carries a second weight. The runtime's
// surface is the one place a Control Plane or management endpoint could appear
// and still work — the runtime has an HTTP server, so a console route mounted
// on it would answer — and the table is what makes that a red build instead.
type route struct {
	method  string
	path    string
	handler stdhttp.HandlerFunc
}

// routes returns this application's HTTP surface, in the order a reader meets
// it. Each handler owns the response *shape*; what the answer is belongs to
// the application, and how it reaches the wire belongs to the kit in
// server.go.
func routes(app *application.App) []route {
	return []route{
		// Liveness: the process is up and its loop is turning. Anything about
		// whether the gateway could do useful work — a dependency reachable, a
		// cache warm, an upstream answering — is readiness's job and never
		// appears here, so an orchestrator restarting on /healthz never kills a
		// runtime for a downstream outage it cannot fix.
		{
			method:  stdhttp.MethodGet,
			path:    "/healthz",
			handler: func(w stdhttp.ResponseWriter, _ *stdhttp.Request) { writeStatus(w) },
		},
		// Readiness: the scaffold has no dependencies, so it is always ready.
		// The checks that will gate this endpoint later — a database ping, a
		// cache ping, an egress probe — hang off here, and only here.
		{
			method:  stdhttp.MethodGet,
			path:    "/readyz",
			handler: func(w stdhttp.ResponseWriter, _ *stdhttp.Request) { writeStatus(w) },
		},
		// Version flows through the application rather than reading main's stamp
		// directly: cmd/dataplane owns the one ldflags version source and hands it
		// to application.New, and this handler reads the result across the same
		// boundary every future runtime endpoint will use.
		{
			method: stdhttp.MethodGet,
			path:   "/version",
			handler: func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
				writeJSON(w, stdhttp.StatusOK, versionResponse{Version: app.Version()})
			},
		},
		// The inference surface, contracted and not built. It is registered
		// rather than left unrouted because the difference matters to a
		// caller: 501 with `not_implemented` says the path belongs to this
		// application and the capability does not exist yet, while the 404 an
		// unrouted path would produce says the gateway has no such endpoint —
		// and the second is a lie that costs someone an afternoon.
		//
		// The body is not read, parsed or forwarded. Reading it would be the
		// first line of an implementation, and this endpoint's whole content
		// today is the fact that there is not one.
		{
			method: stdhttp.MethodPost,
			path:   "/v1/chat/completions",
			handler: func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
				writeError(w, r, notImplementedError{})
			},
		},
	}
}

// register mounts one route along with its method-agnostic companion, which
// owns every other method on the path. ServeMux answers a mismatched method
// with a plain-text 405, so the companion answers with the contract's JSON
// envelope instead — an ordinary handler, not a ResponseWriter wrapper that
// would silently strip streaming interfaces such as Flusher from every
// response this service will ever write.
//
// That last point is why this application registers routes one at a time
// rather than wrapping the mux: the runtime's whole reason to exist is the
// streaming response, and a wrapper that hides Flusher would break it while
// every scaffold test stayed green.
//
// GET patterns already match HEAD, so a GET route's companion answers exactly
// the methods the path refuses; that is why the Allow header names HEAD for a
// GET and only the method itself otherwise.
func register(mux *stdhttp.ServeMux, rt route) {
	allowed := rt.method
	if rt.method == stdhttp.MethodGet {
		allowed = stdhttp.MethodGet + ", " + stdhttp.MethodHead
	}
	mux.HandleFunc(rt.method+" "+rt.path, rt.handler)
	mux.HandleFunc(rt.path, func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Allow", allowed)
		writeError(w, r, methodNotAllowedError{})
	})
}
