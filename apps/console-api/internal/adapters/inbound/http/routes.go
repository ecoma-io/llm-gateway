package http

import (
	stdhttp "net/http"
	"strconv"
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
type guardClass uint8

const (
	guardNone guardClass = iota
	guardBrowserUnsafe
	guardServerToServer
)

// String names the class where a number would be unreadable: a test failure that
// said `guarded = 1, want 1 or 2` would send its reader to this file to find out
// which class was which, and the failure message is the whole value of the test.
// An unnamed value prints as its number rather than as a name this package did
// not give it.
func (g guardClass) String() string {
	switch g {
	case guardNone:
		return "guardNone"
	case guardBrowserUnsafe:
		return "guardBrowserUnsafe"
	case guardServerToServer:
		return "guardServerToServer"
	default:
		return "guardClass(" + strconv.FormatUint(uint64(g), 10) + ")"
	}
}

type route struct {
	method  string
	path    string
	handler stdhttp.HandlerFunc
	// guard is DERIVED, not authored: routes() sets it from the row's method, so
	// a row cannot claim to be guarded while its handler is not, and an unsafe
	// row cannot be added without inheriting the guards. That is the whole
	// reason it is a field rather than a habit — the alternative, a handler that
	// calls the guard itself, is a line a future edit can forget on exactly the
	// operation where forgetting it costs the most.
	//
	// The one class a row CAN declare is guardServerToServer, which exists
	// because the derivation would give the provider's delivery endpoint the
	// browser guards — and those would refuse every real delivery. It is a class
	// rather than a boolean so the exception is one named value a reader can see
	// in the table and a test can require.
	guard guardClass
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
// the wire itself needs. The ten read use cases travel as their own seam beside
// them rather than as members of the session one, because the two have
// different consequences when they are absent — see ConsoleReadUseCases.
//
// usage is the fourth and the same shape as the two above: it is authenticated
// by a CREDENTIAL rather than by a session cookie, so it does not go through
// resolveSession and belongs to neither seam, and it is a seam of its own rather
// than a member of either because it is wired from a different port — see
// UsageUseCases.
//
// The payment surface travels as PaymentSurface, its own pair of seams, for the
// same reason again: a console whose top-up screen has no implementation is a
// missing feature, and a delivery endpoint that cannot authenticate is a
// boundary that is absent.
//
// routes is routesWithPayments with the payment integration unwired, and the
// split is about what a caller can say rather than about what a route is. The
// four payment rows are part of the DECLARED surface — the contract test reads
// this table and requires them — so the table cannot be built without something
// behind them. A caller that wires a payment surface gets the real thing; a
// caller that does not gets handlers that refuse, loudly and closed, rather than
// a table with a hole in it. See unwiredPaymentSurface.
func routes(app *application.App, readiness persistence.Pinger, sessions SessionUseCases, reads ConsoleReadUseCases, usage UsageUseCases) []route {
	return routesWithPayments(app, readiness, sessions, reads, usage, unwiredPaymentSurface())
}

// routesWithPayments is the surface itself, over the seams it was handed.
func routesWithPayments(app *application.App, readiness persistence.Pinger, sessions SessionUseCases, reads ConsoleReadUseCases, usage UsageUseCases, surface PaymentSurface) []route {
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
		// The Principal this session belongs to. The console calls it on every
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
		// The top-up: the second write on this surface that moves money, and the
		// one where the guards are load-bearing for a different reason than the
		// mint's. The account comes from the session and the request names an
		// OFFER, never an amount — a browser that could name a price could charge
		// itself one minor unit or a billion. It carries no idempotency of its
		// own on the wire, which is why the guards matter here too: a
		// cross-origin page that could POST this would be opening payments
		// against a victim's account, and the double-click that the idempotency
		// key exists for is the caller's own retry rather than this one.
		//
		// It sits after POST /api-keys rather than beside its own GET because the
		// table's order is the reader's, not the wire's: the reads below are
		// appended to this slice, so the two operations on one path are together
		// only in the inventory the tests sort.
		{
			method:  stdhttp.MethodPost,
			path:    "/payment-intents",
			handler: handleBeginTransfer(sessions, surface.Payments),
		},
	}

	// The reads the contract declares, in the order api/openapi/console.yaml
	// declares them: the dashboard's composition, then identity, then commerce,
	// then accounting, then reconciliation. Every one of them is a GET, so
	// `isUnsafeMethod` below attaches no cross-cutting guards to any of them —
	// the origin, content-type and double-submit guards exist for a request that
	// CHANGES something, and a read that changed something would be a different
	// defect. The session cookie, applied by resolveSession inside each handler,
	// is the only guard a read needs, and it is applied by the handler rather
	// than the table because a read is not "unguarded" until a live Principal
	// is behind it: the three probes above are the only rows on this surface
	// that are reachable without one.
	//
	// Reads sit in their own slice rather than appended to the product rows so
	// that the two halves of the surface — the session operations that establish
	// and end a caller, and the screens that need one — stay legible in the
	// table as the contract states them.
	readsOnly := []route{
		// The dashboard's one server-side composition. Its account is the
		// session's, and nothing here computes a figure: the counts are stored
		// columns and the two lists are bounded rows.
		{
			method:  stdhttp.MethodGet,
			path:    "/account/overview",
			handler: handleGetAccountOverview(sessions, reads),
		},
		// The account's console users, keyset-paged, with the account predicate
		// in the statement rather than applied to rows already fetched.
		{
			method:  stdhttp.MethodGet,
			path:    "/users",
			handler: handleListUsers(sessions, reads),
		},
		// The account's keys, keyset-paged. Every row is an ownership record;
		// the POST above is the only place a credential exists.
		{
			method:  stdhttp.MethodGet,
			path:    "/api-keys",
			handler: handleListAPIKeys(sessions, reads),
		},
		// The plan catalogue. Not account-scoped — a plan is what every account
		// buys from — so this is the one list that takes no account at all,
		// while still requiring a session like every other product read.
		{
			method:  stdhttp.MethodGet,
			path:    "/plans",
			handler: handleListPlans(sessions, reads),
		},
		// What the account has bought. A scheduled cancellation is data beside
		// an unchanged state, never a state of its own.
		{
			method:  stdhttp.MethodGet,
			path:    "/subscriptions",
			handler: handleListSubscriptions(sessions, reads),
		},
		// The grants the account's subscriptions materialised. No balance here:
		// what remains of a grant is drawn on a funding bucket.
		{
			method:  stdhttp.MethodGet,
			path:    "/entitlements",
			handler: handleListEntitlements(sessions, reads),
		},
		// The account's buckets with their three cached balances, rendered and
		// never derived by the client. Both bucket kinds are on this one list,
		// and `kind` is what tells them apart.
		{
			method:  stdhttp.MethodGet,
			path:    "/funding-buckets",
			handler: handleListFundingBuckets(sessions, reads),
		},
		// One bucket's ledger. The only path parameter on the whole surface, and
		// it names a bucket rather than an account: a bucket the session's
		// account does not own is a row the query did not return, so the
		// question a caller asked of a foreign bucket and of a bucket that does
		// not exist is the same question and gets the same empty page.
		{
			method:  stdhttp.MethodGet,
			path:    "/funding-buckets/{funding_bucket_id}/ledger",
			handler: handleListLedgerEntries(sessions, reads),
		},
		// The top-up price list: what this deployment sells, and the vocabulary
		// POST /payment-intents below is called with. It is a read of
		// CONFIGURATION rather than of account data — it takes no account, is
		// not paged and arrives whole — and it precedes the payments themselves
		// in this list because a chooser is what a customer meets first: the
		// offer they pick is the one the payment below names.
		{
			method:  stdhttp.MethodGet,
			path:    "/top-up-offers",
			handler: handleListTopUpOffers(sessions, surface.Payments),
		},
		// The account's payments, newest first. The cursor is a payment's own
		// id, so a page can neither skip a payment nor read one twice, and the
		// account predicate travels in the query rather than being applied to
		// rows already fetched.
		//
		// Nothing on this row decides anything: a status was written by a signed
		// delivery the provider sent to the endpoint below, and the console
		// re-reads a payment rather than assuming anything from what the
		// customer did on the page — the customer's own act is not evidence,
		// and there is no redirect in this instrument to read an answer from.
		{
			method:  stdhttp.MethodGet,
			path:    "/payment-intents",
			handler: handleListPayments(sessions, surface.Payments),
		},
		// The reconciliation worker's recorded divergences, whole-plane and
		// newest first. Evidence is rendered as text and never interpreted, and
		// nothing here repairs anything: a finding is a record, not a path.
		{
			method:  stdhttp.MethodGet,
			path:    "/reconciliation/findings",
			handler: handleListFindings(sessions, reads),
		},
		// The pass history, whole-plane and newest first. A pass with no
		// finished_at is a pass that started and never finished, rendered as
		// such because a wedged worker and an idle one look identical otherwise.
		{
			method:  stdhttp.MethodGet,
			path:    "/reconciliation/runs",
			handler: handleListReconciliationRuns(sessions, reads),
		},
		// The account's usage read model, and the only row on this surface
		// authenticated by something other than a session cookie. It resolves
		// a bearer credential to an account on the server, so the account this
		// row answers about is never a value the caller sent. Every figure in
		// its answer is derived from state this plane already holds, and none
		// of them is an authority: the ledger remains the money record.
		//
		// It is a root path like every other product operation, and never
		// `/v1/usage`: `/v1/*` is the Data Plane runtime's namespace, and a
		// console read on it would be indistinguishable in a URL from a model
		// call (ADR 0006 §5, `docs/architecture/planes.md`).
		{
			method:  stdhttp.MethodGet,
			path:    "/usage",
			handler: handleUsage(usage),
		},
	}
	product = append(product, readsOnly...)

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

	// The payment provider's delivery endpoint, on its own rather than in
	// either slice above, because it is the one row on this surface that is not
	// a console operation: no browser calls it, no session reaches it, and no
	// account is named in it. Its path is deliberately outside `/api` and free
	// of the runtime's `/v1/` namespace — it is a third party's endpoint that
	// happens to be served by this process, and the two things a generator of
	// this surface must never do with it are expose it to browser code and route
	// it through the inference path.
	//
	// Its guard is EXPLICIT and not derived, because the derivation would give
	// it guardBrowserUnsafe — POST is unsafe by definition, and that is the
	// right default for every other row. Here it would be exactly wrong: a
	// provider has no Origin, no CSRF token and no session cookie, so a browser
	// guard would refuse every real delivery. The class is stated rather than
	// inferred so that a reader sees which row takes the exception and why, and
	// so the test that requires every unsafe row to carry one of the two named
	// classes has something to require.
	providerDeliveries := []route{
		{
			method:  stdhttp.MethodPost,
			path:    "/payment-webhooks/{provider}",
			handler: handleProviderWebhook(surface),
			guard:   guardServerToServer,
		},
	}

	// The cross-cutting guards are attached HERE, from the method, rather than
	// authored per row. Every unsafe method on this surface clears the origin,
	// content-type and double-submit checks before its handler runs, and a row
	// added next year with a POST inherits them by existing — the failure mode
	// this closes is a money-moving write whose author forgot a check that was
	// never in the handler to begin with.
	//
	// The one exception is a row that declares guardServerToServer, which is a
	// server-to-server write (the payment provider's webhook) that carries its own
	// authentication — a cryptographic signature over the raw request body — and
	// must NOT carry the browser guards: a provider has no Origin, no CSRF cookie,
	// and no session. That exception is a class rather than a bool false so a
	// reviewer reading the table sees which row it is and why, and so
	// TestEveryUnsafeRouteIsGuarded can require the class to be one of the two
	// named values rather than silently false.
	table := append(probe, product...)
	table = append(table, providerDeliveries...)
	for i := range table {
		if table[i].guard != guardNone {
			continue
		}
		if isUnsafeMethod(table[i].method) {
			table[i].guard = guardBrowserUnsafe
		}
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
	if rt.guard == guardBrowserUnsafe {
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
