package http

import (
	stdhttp "net/http"
	"strings"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// route is one endpoint of this application's HTTP surface.
//
// The table in routes is that surface's single source of truth: New registers
// from it, and routes_test.go reads it, so an endpoint cannot exist in the
// server and be missing from the inventory a reviewer reads. The shape
// of a surface is worth stating in one place before there are twenty rows to
// state it about, and because that is what makes the surface testable as data
// rather than as a series of calls.
//
// guard names the row's cross-cutting guards rather than repeating them at each
// handler. It is not decoration: an unsafe method that arrives without them is
// the defect the whole session surface is built to prevent, and stating the
// requirement as data lets the tests assert that EVERY unsafe row carries it
// without restating the rule per handler.
type route struct {
	method  string
	path    string
	handler stdhttp.HandlerFunc
	// guard is DERIVED, not authored: routes() sets it from the row's method, so
	// a row cannot claim to be guarded while its handler is not, and an unsafe
	// row cannot be added without inheriting the guards. That is the whole
	// reason it is a field rather than a habit — the alternative, a handler
	// that calls the guard itself, is a line a future edit can forget on exactly
	// the operation where forgetting it costs the most.
	guard bool
}

// routes returns this application's HTTP surface, in the order a reader meets
// it. Each handler owns the response *shape*; what the answer is belongs to
// the application, and how it reaches the wire belongs to the kit in
// server.go. readiness is the one dependency the table carries beside the
// application, because the readiness probe's answer is a fact about this
// process's own wiring rather than about a resource any use case owns.
//
// The session use cases travel as the narrow seam in wire.go rather than as the
// application type, for the reason in that file: the import rule forbids this
// package from reaching internal/domain, so the seam speaks in the plain fields
// the wire itself needs.
func routes(app *application.App, readiness persistence.Pinger, sessions sessionUseCases) []route {
	product := []route{
		// Sign-in: the only unauthenticated write, and the only way a session
		// comes into existence. It carries the origin, content-type and
		// double-submit guards even though it has no session of its own
		// behind them, because the attack those stop is login CSRF — a
		// cross-origin page that silently signs a victim into an account the
		// attacker controls — and SameSite=Strict does not help against it.
		{
			method:  stdhttp.MethodPost,
			path:    "/auth/sign-in",
			handler: handleSignIn(sessions),
		},
		// The principal this session belongs to. The console calls it on every
		// load, before it renders anything; a 200 is the only proof of
		// authentication the client ever has, and it is the cookie guard doing
		// that work. No cross-cutting guards, because GET is safe by definition
		// and the cookie is the only thing standing between a caller and
		// another account's data here.
		{
			method:  stdhttp.MethodGet,
			path:    "/auth/session",
			handler: handleSession(sessions),
		},
		// Sign-out is idempotent: a second call, or a call with nothing to end,
		// still answers 204 with the cookies cleared. A sign-out that failed
		// would leave a live session on a machine the user believes they left.
		{
			method:  stdhttp.MethodDelete,
			path:    "/auth/session",
			handler: handleSignOut(sessions),
		},
		// The one write on this surface that produces a secret, and so the one
		// where every guard is load-bearing: the response is the credential
		// itself, returned once, with no idempotency key so a replay mints a
		// second key rather than re-serving the first one's secret.
		{
			method:  stdhttp.MethodPost,
			path:    "/api-keys",
			handler: handleMintAPIKey(sessions),
		},
	}

	probe := []route{
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
				writeProbeJSON(w, versionResponse{Version: app.Version()})
			},
		},
	}

	// The cross-cutting guards are attached HERE, from the method, rather than
	// authored per row. Every unsafe method on this surface clears the origin,
	// content-type and double-submit checks before its handler runs, and a row
	// added next year with a POST inherits them by existing — the failure mode
	// this closes is a money-moving write whose author forgot a check that was
	// never in the handler to begin with.
	table := append(probe, product...)
	for i := range table {
		table[i].guard = isUnsafeMethod(table[i].method)
	}
	return table
}

// register mounts one route's method-specific pattern.
//
// The method-agnostic companion is NOT registered here: it belongs to the
// PATH, not to a row, and a path with two operations — /auth/session serves a
// GET and a DELETE — would otherwise register it twice and panic the mux. The
// companion is mounted once per distinct path by mountCompanions, which is the
// one place that knows the path's full method set.
func register(mux *stdhttp.ServeMux, rt route) {
	handler := rt.handler
	if rt.guard {
		handler = guardUnsafeRequest(handler)
	}
	mux.HandleFunc(rt.method+" "+rt.path, handler)
}

// mountCompanions mounts the method-agnostic companion for every distinct path
// in the table, once each.
//
// ServeMux answers a mismatched method with a plain-text 405, so the companion
// answers with the contract's JSON envelope instead — an ordinary handler, not a
// ResponseWriter wrapper that would silently strip streaming interfaces such
// as Flusher from every response this service will ever write.
//
// The Allow header names every method the path accepts, which is why the
// companion is built per PATH rather than per row: a path with a GET and a
// DELETE must advertise both, and a path with a GET advertises HEAD as well
// because Go's GET patterns already match HEAD.
func mountCompanions(mux *stdhttp.ServeMux, table []route) {
	allowed := map[string][]string{}
	var order []string
	for _, rt := range table {
		methods := allowed[rt.path]
		if methods == nil {
			order = append(order, rt.path)
		}
		// HEAD is served by a GET pattern, so it is allowed on any path that has
		// a GET and needs no row of its own.
		if rt.method == stdhttp.MethodGet {
			allowed[rt.path] = append(methods, stdhttp.MethodGet, stdhttp.MethodHead)
			continue
		}
		allowed[rt.path] = append(methods, rt.method)
	}
	for _, path := range order {
		advertised := strings.Join(allowed[path], ", ")
		mux.HandleFunc(path, func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
			w.Header().Set("Allow", advertised)
			writeError(w, r, methodNotAllowedError{})
		})
	}
}
