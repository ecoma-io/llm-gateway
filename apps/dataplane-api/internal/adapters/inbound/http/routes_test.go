package http

import (
	stdhttp "net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
)

// runtimeNamespace names the OpenAI-compatible namespace that belongs to the
// runtime. A management route under it would put this application on the LLM
// request path — the second half of the thing ADR 0006 §11 forbids, alongside
// the Control Plane hop — and it would do it quietly, because the route would
// work.
//
// The check is this prefix rather than the handful of paths that exist today
// (`/v1/chat/completions` and its siblings, ADR 0002's spelling). The defect
// is the arrival of the *surface*, not of one endpoint of it, and
// `/v1/chat/completions/stream` is that defect while matching none of the
// listed spellings.
const runtimeNamespace = "/v1/"

// consoleSurface names the paths that belong to the Control Plane's public
// API. The management transport is internal and the console is a browser
// application; a console route here would be a public surface reachable at an
// address the public surface is not, and the mistake that produces one is that
// the two applications look alike while being written.
var consoleSurface = []string{"/api/", "/console/"}

// TestTheSurfaceIsTheDeclaredSet pins what this application serves. It fails
// on an endpoint added without a decision: the table and this list move
// together, and a new row is one edit in each. It is deliberately an equality
// rather than a subset — a route silently dropped is as much a defect as one
// silently added.
//
// The document is the other half of that decision and is checked by
// contract_test.go, which compares this same table against
// api/openapi/dataplane.yaml in both directions. This list makes adding an
// endpoint a deliberate act; that test is what notices the document moving
// alone.
func TestTheSurfaceIsTheDeclaredSet(t *testing.T) {
	got := []string{}
	for _, rt := range routes(testApp()) {
		got = append(got, rt.method+" "+rt.path)
	}
	sort.Strings(got)

	want := []string{
		"GET /healthz",
		"GET /internal/alias-groups/{group_name}/versions/current",
		"GET /internal/usage-events",
		"GET /internal/projection/position",
		"GET /readyz",
		"GET /version",
		"POST /internal/projection/changes",
		"POST /internal/projection/snapshot",
	}
	sort.Strings(want)

	if !slices.Equal(got, want) {
		t.Fatalf("the route table serves %v, want %v", got, want)
	}
}

// TestTheManagementTransportServesNoInferenceRoute is the half of the boundary
// this application owns: the LLM request path never lands on a management
// process. A management API is sized and scaled for administrative traffic and
// reaches Data Plane state over a seam the runtime does not use, so a `/v1`
// route here would be both a performance and an availability defect that a
// scaffold-sized test suite would not otherwise notice.
func TestTheManagementTransportServesNoInferenceRoute(t *testing.T) {
	for _, rt := range routes(testApp()) {
		if strings.HasPrefix(rt.path, runtimeNamespace) {
			t.Errorf("the management API declares %s %s: %s belongs to the Data Plane runtime (ADR 0006 §11)", rt.method, rt.path, runtimeNamespace)
		}
	}
}

// TestTheManagementTransportServesNoConsoleRoute is the other half: the
// internal surface does not become a public one. This application has no
// generated client, no browser reachability and no public contract, and a
// route named for the console's API is how it acquires all three.
func TestTheManagementTransportServesNoConsoleRoute(t *testing.T) {
	for _, rt := range routes(testApp()) {
		for _, console := range consoleSurface {
			if strings.HasPrefix(rt.path, console) {
				t.Errorf("the management API declares %s %s: %s is the Control Plane's public surface (ADR 0006 §11)", rt.method, rt.path, console)
			}
		}
	}
}

// probePaths are the rows a caller may reach without a credential. Which
// routes are unprotected is the half of the table nobody re-reads: a row that
// lost its `authenticated` column looks exactly like a row that serves
// something harmless, and only a list written out says which rows are allowed
// to be that.
var probePaths = []string{"/healthz", "/readyz", "/version"}

// TestOnlyTheProbesAreUnprotected pins which rows are reachable without a
// service credential, and it is deliberately an equality rather than a subset:
// a new unauthenticated route is as much a defect as a protected one that lost
// its flag, and both would be a management surface answering callers this
// deployment does not trust.
func TestOnlyTheProbesAreUnprotected(t *testing.T) {
	unprotected := []string{}
	protected := 0
	for _, rt := range routes(testApp()) {
		if rt.authenticated {
			protected++
			continue
		}
		unprotected = append(unprotected, rt.path)
	}

	// Without this the equality below would hold for a table where nothing is
	// protected at all: an empty list is equal to nothing.
	if protected == 0 {
		t.Fatal("no route is authenticated; this check would pass on a surface that protects nothing")
	}

	sort.Strings(unprotected)
	want := slices.Clone(probePaths)
	sort.Strings(want)
	if !slices.Equal(unprotected, want) {
		t.Errorf("the unprotected routes are %v, want the probes %v — a row outside that list must carry the credential", unprotected, want)
	}
}

// TestAnUnauthenticatedCallerIsRefusedOnEveryProtectedRoute is the other half,
// and the one that would notice the flag being declared and not applied: each
// protected row is driven through the composed handler with no credential at
// all, and must answer 401 rather than reaching its use case.
//
// The probes are driven the same way and must not answer 401, because a
// liveness probe behind a credential reports a deployment's broken secret as
// an unhealthy process.
func TestAnUnauthenticatedCallerIsRefusedOnEveryProtectedRoute(t *testing.T) {
	handler := testHandler(testApp())
	probes := map[string]bool{}
	for _, path := range probePaths {
		probes[path] = true
	}

	for _, rt := range routes(testApp()) {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, nil))

			if rt.authenticated && rec.Code != stdhttp.StatusUnauthorized {
				t.Errorf("%s %s answered %d to a caller with no credential, want %d", rt.method, rt.path, rec.Code, stdhttp.StatusUnauthorized)
			}
			if !rt.authenticated && rec.Code == stdhttp.StatusUnauthorized {
				t.Errorf("%s %s answered 401; %s is a probe and a probe behind a credential reports a broken secret as an unhealthy process", rt.method, rt.path, rt.path)
			}
			if probes[rt.path] == rt.authenticated {
				t.Errorf("%s is declared protected=%v and listed as a probe=%v; the two disagree", rt.path, rt.authenticated, probes[rt.path])
			}
		})
	}
}

// TestEveryRouteIsMountable is the invariant the mux relies on: two rows with
// the same method and path would panic in ServeMux at wiring time, and a path
// that is not absolute and canonical would be served as something other than
// what the table says — the mux's own path cleaning is suppressed deliberately
// in server.go, so nothing downstream would correct it.
func TestEveryRouteIsMountable(t *testing.T) {
	seen := map[string]bool{}
	for _, rt := range routes(testApp()) {
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
