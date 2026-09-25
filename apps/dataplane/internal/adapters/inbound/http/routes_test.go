package http

import (
	"slices"
	"sort"
	"strings"
	"testing"
)

// inferenceSurface names the OpenAI-compatible paths this runtime serves or
// will serve, in the spelling ADR 0002 fixes for the one of them that exists.
// Exactly one is contracted today — POST /v1/chat/completions, which answers
// 501 — and the rest are named so the surface is legible as a whole: this list
// is this application's to serve, and the check that it stays out of the other
// two applications lives in their own route tests.
var inferenceSurface = []string{
	"/v1/chat/completions",
	"/v1/completions",
	"/v1/embeddings",
	"/v1/messages",
	"/v1/responses",
}

// foreignSurface names the paths that belong to the Control Plane's public
// API and to the Data Plane's management API. Neither may appear here: a
// console route on the runtime puts the Control Plane's vocabulary on the hot
// path, and a management route puts an administrative surface on it — both are
// reachable and both would work, which is why the check is mechanical.
//
// The prefixes are matched rather than the exact paths the sibling
// applications serve today, because the runtime's defect is the arrival of the
// *surface*, not of one endpoint of it.
var foreignSurface = []string{
	"/api/",
	"/admin/",
	"/internal/",
	"/console/",
	"/manage/",
	"/management/",
}

// TestTheSurfaceIsTheDeclaredSet pins what this application serves. It fails
// on an endpoint added without a decision: the table and this list move
// together, and a new row is one edit in each. It is deliberately an equality
// rather than a subset — a route silently dropped is as much a defect as one
// silently added.
//
// The document is the other half of that decision and is checked by
// contract_test.go, which compares this same table against
// api/openapi/runtime.yaml in both directions. This list makes adding an
// endpoint a deliberate act; that test is what notices the document moving
// alone.
func TestTheSurfaceIsTheDeclaredSet(t *testing.T) {
	got := []string{}
	for _, rt := range routes(newTestApp(t, "test"), wiring{}) {
		got = append(got, rt.method+" "+rt.path)
	}
	sort.Strings(got)

	want := []string{"GET /healthz", "GET /readyz", "GET /version", "POST /v1/chat/completions"}
	sort.Strings(want)

	if !slices.Equal(got, want) {
		t.Fatalf("the route table serves %v, want %v", got, want)
	}
}

// TestTheRuntimeServesNoForeignRoute is the runtime's half of the boundary
// ADR 0006 states: nothing on this surface is a console route and nothing on
// it is a management route. The runtime is the process the Control Plane is
// not allowed to be a hop on, and the way that fails in practice is a route
// someone added here because it was the application already running.
func TestTheRuntimeServesNoForeignRoute(t *testing.T) {
	for _, rt := range routes(newTestApp(t, "test"), wiring{}) {
		for _, foreign := range foreignSurface {
			if strings.HasPrefix(rt.path, foreign) {
				t.Errorf("the runtime declares %s %s: %s belongs to another application's surface (ADR 0006 §4, §11)", rt.method, rt.path, foreign)
			}
		}
	}
}

// TestTheInferenceSurfaceIsTheRuntimes names the endpoints that are this
// application's to serve. It asserts membership rather than presence, because
// the list is deliberately wider than what exists: it is the surface, and the
// assertion that will matter as each endpoint lands is that it lands *here* —
// the Control Plane's route test asserts the same list stays out of its table.
func TestTheInferenceSurfaceIsTheRuntimes(t *testing.T) {
	if len(inferenceSurface) == 0 {
		t.Fatal("the inference surface is empty; the claim below would be vacuous")
	}
	for _, inference := range inferenceSurface {
		if !strings.HasPrefix(inference, "/v1/") {
			t.Errorf("%s is not an OpenAI-compatible path; the surface this application serves is versioned under /v1", inference)
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
	for _, rt := range routes(newTestApp(t, "test"), wiring{}) {
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
