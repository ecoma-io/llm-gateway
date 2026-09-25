package http

import (
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// route is one endpoint of this application's HTTP surface.
//
// The table in routes is that surface's single source of truth: New registers
// from it, and routes_test.go reads it, so an endpoint cannot exist in the
// server and be missing from the inventory a reviewer reads. The scaffold has
// three probes and no product endpoints; the table is here because the shape
// of a surface is worth stating in one place before there are twenty rows to
// state it about, and because that is what makes the surface testable as data
// rather than as a series of calls.
type route struct {
	method  string
	path    string
	handler stdhttp.HandlerFunc
}

// routes returns this application's HTTP surface, in the order a reader meets
// it. Each handler owns the response *shape*; what the answer is belongs to
// the application, and how it reaches the wire belongs to the kit in
// server.go. readiness is the one dependency the table carries beside the
// application, because the readiness probe's answer is a fact about this
// process's own wiring rather than about a resource any use case owns.
func routes(app *application.App, readiness persistence.Pinger) []route {
	return []route{
		// Liveness: the process is up and its loop is turning. Anything about
		// whether the gateway could do useful work — a dependency reachable, a
		// cache warm — is readiness's job and never appears here, so an
		// orchestrator restarting on /healthz never kills a process for a
		// downstream outage it cannot fix.
		{
			method:  stdhttp.MethodGet,
			path:    "/healthz",
			handler: func(w stdhttp.ResponseWriter, _ *stdhttp.Request) { writeStatus(w) },
		},
		// Readiness: gated on this process's own dependency — the database
		// answering, over the port's Pinger. What the answer is belongs to
		// readyz; the check hangs off here, and only here, so an orchestrator
		// restarting on /healthz never kills this process for a dependency it
		// is on its way to reach.
		{
			method:  stdhttp.MethodGet,
			path:    "/readyz",
			handler: readyz(readiness),
		},
		// Version flows through the application rather than reading main's stamp
		// directly: cmd/console-api owns the one ldflags version source and hands it
		// to application.New, and this handler reads the result across the same
		// boundary every future domain endpoint will use.
		{
			method: stdhttp.MethodGet,
			path:   "/version",
			handler: func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
				writeJSON(w, stdhttp.StatusOK, versionResponse{Version: app.Version()})
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
