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
// The guard is derived from the method in routes(), so this is a regression pin
// rather than a check of a hand-set field: it turns red the moment someone
// changes the derivation, and it is the assertion that states what the
// derivation must be. An unsafe method whose row is not guarded is a
// money-moving write reachable by a cross-origin page, which is the failure
// mode the whole session surface exists to prevent.
//
// The reverse direction matters as much. A safe method marked guarded would be a
// read that requires an origin and a double-submit token, which is not stricter
// — it is unreachable, because a browser does not send Origin on a same-origin
// GET. So the rule has a wrong answer in each direction and this asserts both.
func TestEveryUnsafeRouteIsGuarded(t *testing.T) {
	for _, rt := range routeTableForTest() {
		unsafe := isUnsafeMethod(rt.method)
		if rt.guard != unsafe {
			t.Errorf("%s %s: guarded = %v, want %v; an unsafe method must clear the origin, content-type and double-submit guards",
				rt.method, rt.path, rt.guard, unsafe)
		}
	}
}

// TestTheGuardCoversEveryUnsafeMethodTheSurfaceDeclares is the half of the
// previous test that says the surface HAS unsafe methods at all.
//
// Without it, an empty product surface would satisfy TestEveryUnsafeRouteIsGuarded
// vacuously — no unsafe row, no violated rule, a green test over a guard that
// guards nothing. The two product writes this change ships are named here, so
// removing one without deciding anything turns this red and names what is
// missing.
func TestTheGuardCoversEveryUnsafeMethodTheSurfaceDeclares(t *testing.T) {
	guarded := 0
	for _, rt := range routeTableForTest() {
		if rt.guard {
			guarded++
		}
	}
	// Three: POST /auth/sign-in, DELETE /auth/session, POST /api-keys.
	if guarded != 3 {
		t.Errorf("the surface declares %d guarded routes, want 3; the session surface's three writes are the ones the guards exist for", guarded)
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
func TestNoRouteCarriesAnAccountID(t *testing.T) {
	for _, rt := range routeTableForTest() {
		for _, segment := range strings.Split(strings.Trim(rt.path, "/"), "/") {
			if strings.Contains(segment, "account") {
				t.Errorf("%s %s: the segment %q names an account in a path, where every proxy access log and every outbound Referer will record it",
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
