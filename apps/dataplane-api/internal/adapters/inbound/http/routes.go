package http

import (
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/application"
)

// route is one endpoint of this application's HTTP surface.
//
// The table in routes is that surface's single source of truth: New registers
// from it, and routes_test.go reads it, so an endpoint cannot exist in the
// server and be missing from the inventory a reviewer reads. The scaffold has
// three probes and no management endpoint; the table is here because the shape
// of a surface is worth stating in one place before there are twenty rows to
// state it about, and because that is what makes the surface testable as data
// rather than as a series of calls.
//
// For this application the table is the whole surface, permanently. The
// management API is internal — the Control Plane calls it and nobody else on
// this side of the network does — so it has no public contract to grow into
// and no runtime endpoint to add: the two ways it could be wrong are a
// `/v1/...` route and a console route, and both are red builds in
// routes_test.go.
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
		// Liveness: the process is up and its loop is turning. A management
		// transport holds no state of its own, so this is a statement about
		// the process and nothing else.
		{
			method:  stdhttp.MethodGet,
			path:    "/healthz",
			handler: func(w stdhttp.ResponseWriter, _ *stdhttp.Request) { writeStatus(w) },
		},
		// Readiness: the scaffold has no dependencies, so it is always ready.
		// When this application does reach Data Plane state (ADR 0006 §9), the
		// check for that reaches the Data Plane — it does not read a database
		// from here.
		{
			method:  stdhttp.MethodGet,
			path:    "/readyz",
			handler: func(w stdhttp.ResponseWriter, _ *stdhttp.Request) { writeStatus(w) },
		},
		// Version flows through the application rather than reading main's stamp
		// directly: cmd/dataplane-api owns the one ldflags version source and hands
		// it to application.New, and this handler reads the result across the same
		// boundary every future management endpoint will use.
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
