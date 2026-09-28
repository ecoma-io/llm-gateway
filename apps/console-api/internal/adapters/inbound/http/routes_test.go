package http

import (
	"slices"
	"sort"
	"strings"
	"testing"
)

// runtimeNamespace names the OpenAI-compatible namespace that belongs to the
// Data Plane's runtime. A Control Plane route under it would put the LLM
// request path behind the console's backend — the single thing ADR 0006
// forbids — and it would do it quietly, because the route would work.
//
// The check is this prefix rather than the handful of paths that exist today
// (`/v1/chat/completions` and its siblings, ADR 0002's spelling). The defect
// is the arrival of the *surface*, not of one endpoint of it, and
// `/v1/chat/completions/stream` is that defect while matching none of the
// listed spellings.
//
// It is also the whole prefix rather than a list of the runtime's own paths
// because the split is total in both directions: `/v1/*` is the runtime's
// namespace and this plane serves nothing under it (docs/architecture/
// planes.md, "Exposure"). The usage read model is the operation most likely to
// be argued out of that — a report of the Control Plane's own books reads like
// a console concern — and it is served at `/usage`, at the root, with the rest
// of the console surface.
const runtimeNamespace = "/v1/"

// TestTheSurfaceIsTheDeclaredSet pins what this application serves. It fails
// on an endpoint added without a decision: the table and this list move
// together, and a new row is one edit in each. It is deliberately an equality
// rather than a subset — a route silently dropped is as much a defect as one
// silently added.
//
// The document is the other half of that decision and is checked by
// contract_test.go, which compares this same table against
// api/openapi/console.yaml in both directions. This list makes adding an
// endpoint a deliberate act; that test is what notices the document moving
// alone.
func TestTheSurfaceIsTheDeclaredSet(t *testing.T) {
	got := []string{}
	for _, rt := range routeTableForTest() {
		got = append(got, rt.method+" "+rt.path)
	}
	sort.Strings(got)

	want := []string{
		"GET /healthz",
		"GET /readyz",
		"GET /version",
		"POST /auth/sign-in",
		"GET /auth/session",
		"DELETE /auth/session",
		"POST /api-keys",
		// The reads. Seven are account-scoped and take their account from the
		// session, never from the path; four — the plan catalogue, the top-up
		// price list, the findings and the reconciliation runs — are whole-plane
		// and are account-scoped by nothing, which is the contract's own
		// statement about them rather than an omission here.
		"GET /account/overview",
		"GET /users",
		"GET /api-keys",
		"GET /plans",
		"GET /subscriptions",
		"GET /entitlements",
		"GET /funding-buckets",
		"GET /funding-buckets/{funding_bucket_id}/ledger",
		"GET /reconciliation/findings",
		"GET /reconciliation/runs",
		// The usage read model, and the only row whose scope is a credential
		// rather than a session. It is listed here for the same reason as the
		// rest: a route that appears without a line in this list is a surface
		// nobody reviewed.
		"GET /usage",
		// The payment surface: the price list and the account's payments are
		// reads, and the two writes are the top-up itself and the provider's
		// delivery endpoint — the last of which is the one row on this surface
		// no browser may call.
		"GET /top-up-offers",
		"GET /payment-intents",
		"POST /payment-intents",
		"POST /payment-webhooks/{provider}",
	}

	sort.Strings(want)

	if !slices.Equal(got, want) {
		t.Fatalf("the route table serves %v, want %v", got, want)
	}
}

// TestTheControlPlaneServesNoInferenceRoute is the boundary this repository
// exists to hold, in the one place it is mechanically checkable: whatever the
// Control Plane API grows, it does not grow the LLM request path.
func TestTheControlPlaneServesNoInferenceRoute(t *testing.T) {
	for _, rt := range routeTableForTest() {
		if strings.HasPrefix(rt.path, runtimeNamespace) {
			t.Errorf("the Control Plane API declares %s %s: %s belongs to the Data Plane runtime (ADR 0006)", rt.method, rt.path, runtimeNamespace)
		}
	}
}

// TestEveryUnsafeRouteIsGuarded is the mechanical form of ADR 0012 §2's
// "four layers, because SameSite is a browser control and not a boundary".
//
// The guard class is derived from the method in routes() for every row that does
// not declare one, so this is a regression pin rather than a check of a hand-set
// field: it turns red the moment someone changes the derivation, and it is the
// assertion that states what the derivation must be. An unsafe method whose row
// is not guarded is a money-moving write reachable by a cross-origin page, which
// is the failure mode the whole session surface exists to prevent.
//
// There are exactly two right answers for an unsafe row and they are not
// interchangeable: guardBrowserUnsafe is the browser's three guards, and
// guardServerToServer is a write that authenticates ITSELF — the provider's
// webhook, which has no origin, no cookie and no session to present, and which
// browser guards would refuse every time. The one value an unsafe row may not
// carry is guardNone, and that is what the first branch below asserts. A class
// rather than a boolean is what makes that a distinction a reader can see in the
// table and a test can require, instead of a `false` that says both "no guards"
// and "not the browser's guards".
//
// The reverse direction matters as much. A safe method marked guarded would be a
// read that requires an origin and a double-submit token, which is not stricter
// — it is unreachable, because a browser does not send Origin on a same-origin
// GET. So the rule has a wrong answer in each direction and this asserts both,
// with the safe direction requiring guardNone exactly.
func TestEveryUnsafeRouteIsGuarded(t *testing.T) {
	for _, rt := range routeTableForTest() {
		if isUnsafeMethod(rt.method) {
			if rt.guard != guardBrowserUnsafe && rt.guard != guardServerToServer {
				t.Errorf("%s %s: guard = %s, want %s or %s; an unsafe method must clear the origin, content-type and double-submit guards, or declare that it authenticates itself",
					rt.method, rt.path, rt.guard, guardBrowserUnsafe, guardServerToServer)
			}
			continue
		}
		if rt.guard != guardNone {
			t.Errorf("%s %s: guard = %s, want %s; a safe method is guarded by the session cookie inside its handler and by nothing at the mount, because a browser sends no Origin on a same-origin GET",
				rt.method, rt.path, rt.guard, guardNone)
		}
	}
}

// TestTheGuardCoversEveryUnsafeMethodTheSurfaceDeclares is the half of the
// previous test that says the surface HAS unsafe methods at all.
//
// Without it, an empty product surface would satisfy TestEveryUnsafeRouteIsGuarded
// vacuously — no unsafe row, no violated rule, a green test over a guard that
// guards nothing. The writes this surface ships are counted here, by class, so
// removing one without deciding anything turns this red and names what is
// missing.
//
// The counts are per class rather than one total, and the split is the assertion:
// four browser writes are the session's three (POST /auth/sign-in,
// DELETE /auth/session, POST /api-keys) plus POST /payment-intents, and the ONE
// server-to-server write is the provider's delivery endpoint
// (POST /payment-webhooks/{provider}). A fifth browser write arriving without a
// decision, or the webhook quietly losing its class and inheriting the browser
// guards, both turn this red — and the second is the one that would otherwise be
// invisible, because a provider refused by an origin check answers 403 and no
// test that only counted total guarded routes would notice.
func TestTheGuardCoversEveryUnsafeMethodTheSurfaceDeclares(t *testing.T) {
	browser, serverToServer := 0, 0
	for _, rt := range routeTableForTest() {
		switch rt.guard {
		case guardBrowserUnsafe:
			browser++
		case guardServerToServer:
			serverToServer++
		}
	}
	if browser != 4 {
		t.Errorf("the surface declares %d browser-guarded routes, want 4; the session surface's three writes and the top-up are the ones the browser guards exist for", browser)
	}
	if serverToServer != 1 {
		t.Errorf("the surface declares %d server-to-server routes, want 1; POST /payment-webhooks/{provider} is the one write here that authenticates itself with a signature over the raw body", serverToServer)
	}
}

// TestNoRouteCarriesAnAccountID is the account-scope rule as a mechanical
// check rather than a review habit (ADR 0012 §2: "account_id is taken from the
// session, never from the request, and never from a path").
//
// The defect it catches is arrival rather than spelling: a path segment is
// written into every proxy's access log, every Referer on every outbound
// navigation and every history entry, so an account-scoped path records which
// customer each operator was reading in a place none of them chose.
//
// The rule is about ACCOUNT segments specifically, not about path parameters as
// such, because the contract has one path parameter of its own —
// /funding-buckets/{funding_bucket_id}/ledger — and a blanket "no parameters"
// rule would fail a path the contract deliberately promises. An account is the
// one identifier that must never appear in a path on this surface, so that is
// the one this names.
//
// The predicate matches a segment that CARRIES an account identifier, not one
// that merely contains the word. Those are different things, and the first
// draft of this test conflated them: `strings.Contains(segment, "account")`
// fails `GET /account/overview`, whose "account" is a fixed word in a
// parameterless path that identifies no customer and appears in no log as
// one. A rule that fires on a path the contract requires is a rule that gets
// deleted rather than fixed, which loses the check entirely.
//
// What must not exist is a segment that could hold a value: a brace
// placeholder, or a literal value standing where a placeholder belongs. A
// path carrying one is the defect; a path whose segment is a constant noun is
// not, however unfortunate the noun.
func TestNoRouteCarriesAnAccountID(t *testing.T) {
	for _, rt := range routeTableForTest() {
		for _, segment := range strings.Split(strings.Trim(rt.path, "/"), "/") {
			holdsAValue := strings.Contains(segment, "{")
			namesAnAccount := strings.Contains(strings.ToLower(segment), "account")
			if holdsAValue && namesAnAccount {
				t.Errorf("%s %s: the segment %q puts an account identifier in a path, where every proxy access log and every outbound Referer will record it",
					rt.method, rt.path, segment)
			}
		}
	}
}

// TestEveryRouteIsMountable is the invariant the mux relies on: two rows with
// the same method and path would panic in ServeMux at wiring time, and a path
// that is not absolute and canonical would be served as something other than
// what the table says — the mux's own path cleaning is suppressed deliberately
// in server.go, so nothing downstream would correct it.
func TestEveryRouteIsMountable(t *testing.T) {
	seen := map[string]bool{}
	for _, rt := range routeTableForTest() {
		if !strings.HasPrefix(rt.path, "/") {
			t.Errorf("%s %s: a route path must begin with /", rt.method, rt.path)
		}
		if rt.method == "" {
			t.Errorf("%s: a route path needs a method", rt.path)
		}
		if rt.handler == nil {
			t.Errorf("%s %s: a route needs a handler", rt.method, rt.path)
		}
		key := rt.method + " " + rt.path
		if seen[key] {
			t.Errorf("%s is declared twice; the mux would panic on the second", key)
		}
		seen[key] = true
	}
}
