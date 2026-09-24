package http

import (
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// route is one endpoint of this application's HTTP surface.
//
// The table in routes is that surface's single source of truth: New registers
// from it, and routes_test.go reads it, so an endpoint cannot exist in the
// server and be missing from the inventory a reviewer reads. The scaffold has
// three probes and one management operation; the table is here because the
// shape of a surface is worth stating in one place before there are twenty rows
// to state it about, and because that is what makes the surface testable as
// data rather than as a series of calls.
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
	// authenticated marks the rows whose caller must be a service this
	// deployment trusts. It is a column of the table rather than something a
	// handler looks up, because which routes are protected is a property of the
	// surface; register reads it, so the check cannot be reordered below the
	// work it guards — or omitted from a row — by an edit inside a handler.
	//
	// It covers the row's method-agnostic companion too. A path that needs an
	// identified caller needs one whatever verb arrives, so a request with the
	// wrong method to a protected path is answered 401 and not 405 (see
	// register).
	authenticated bool
}

// routes returns this application's HTTP surface, in the order a reader meets
// it. Each handler owns the response *shape*; what the answer is belongs to
// the application, and how it reaches the wire belongs to the kit in
// server.go.
//
// The table declares which rows are protected and register applies the check,
// rather than the table wrapping each handler itself. One column, read in one
// place, is what makes "every protected route is guarded" a property of the
// table rather than of every row's author remembering.
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
		// When this application reads Data Plane state (ADR 0006 §9), the check
		// for that reaches the Data Plane — it does not read a database from
		// here.
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
		// The usage-fact feed: the one row on this surface whose caller must
		// authenticate, and the one whose answer comes from another process.
		//
		// The check runs before anything else on this path, including the
		// method. A caller with no credential is not told which methods the path
		// accepts; it is told to identify itself, which it must do before any
		// verb would have been useful. The reverse order — answer 405, then ask
		// for a credential on the retry — would let an unauthenticated caller
		// enumerate this surface one method at a time, and would make the guard's
		// placement a property of the verb rather than of the path.
		{
			method:        stdhttp.MethodGet,
			path:          usageEventsPath,
			handler:       usageEventsHandler(app),
			authenticated: true,
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
// A protected row is guarded here rather than in the table, and its companion
// is guarded with it: the credential is checked once, in one place, before the
// mux has decided anything about the request. The companion of a public row —
// a probe — is not guarded, because there is no credential to check and a
// liveness probe that needed one would be a probe that reports a deployment's
// broken secret as an unhealthy process.
//
// GET patterns already match HEAD, so a GET route's companion answers exactly
// the methods the path refuses; that is why the Allow header names HEAD for a
// GET and only the method itself otherwise.
func register(mux *stdhttp.ServeMux, rt route, authenticator dataplane.Authenticator) {
	// guard is the row's one authentication decision, applied to both of the
	// handlers below so that a protected path cannot be half-protected: a
	// credential that satisfies the route satisfies its companion, and one that
	// does not is refused before either can answer.
	guard := func(handler stdhttp.HandlerFunc) stdhttp.HandlerFunc {
		if !rt.authenticated {
			return handler
		}
		return requireServiceCaller(authenticator, handler)
	}

	allowed := rt.method
	if rt.method == stdhttp.MethodGet {
		allowed = stdhttp.MethodGet + ", " + stdhttp.MethodHead
	}

	mux.HandleFunc(rt.method+" "+rt.path, guard(rt.handler))
	mux.HandleFunc(rt.path, guard(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Allow", allowed)
		writeError(w, r, methodNotAllowedError{})
	}))
}
