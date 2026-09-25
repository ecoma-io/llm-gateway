package management

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
)

// runtimeSurface names the paths that belong to the runtime's public surface
// and to nobody else. None may appear in this table: an inference route on the
// private listener would be an LLM endpoint on an address that is not built,
// scaled or exposed for LLM traffic — a second front door nobody declared.
var runtimeSurface = []string{
	"/v1/",
}

// TestTheSurfaceIsTheDeclaredSet pins what this listener serves. It fails on an
// endpoint added without a decision: the table and this list move together, and
// a new row is one edit in each. It is deliberately an equality rather than a
// subset — a route silently dropped is as much a defect as one silently added,
// because a management surface that stops answering is a Control Plane that
// stops reconciling.
//
// This listener is not the surface api/openapi/dataplane.yaml contracts, and it
// is therefore not checked against that document by any test here. The document
// is the façade's — dataplane-api — and contract_test.go lives in that module,
// comparing the façade's routes against it. What the two hops share is the page
// they carry, which is why the private protocol is pinned on each side by
// protocol_test.go (this package's) and stated in
// docs/architecture/cross-plane-protocols.md. A reader looking for "does this
// process serve what the contract says" is one module over; the question here is
// "does this listener serve exactly what the private protocol declares", and the
// literal list below is that question asked.
func TestTheSurfaceIsTheDeclaredSet(t *testing.T) {
	got := []string{}
	for _, rt := range routes(newTestApp(t)) {
		got = append(got, rt.method+" "+rt.path)
	}
	sort.Strings(got)

	want := []string{
		"GET /healthz",
		"GET /internal/alias-groups/{group_name}/versions/current",
		"GET /internal/usage-events",
		"GET /readyz",
		"GET /version",
	}
	sort.Strings(want)

	if !slices.Equal(got, want) {
		t.Fatalf("the route table serves %v, want %v", got, want)
	}
}

// TestTheManagementSurfaceCarriesNoRuntimeRoute is the other half of the
// runtime's foreign-surface test. That one fails if a management path appears
// on the address that serves LLM traffic; this one fails if an inference path
// appears here, which is the same mistake made from the other side.
func TestTheManagementSurfaceCarriesNoRuntimeRoute(t *testing.T) {
	for _, rt := range routes(newTestApp(t)) {
		for _, foreign := range runtimeSurface {
			if strings.HasPrefix(rt.path, foreign) {
				t.Errorf("the management listener declares %s %s: %s belongs to the runtime's surface and to a different address (ADR 0006 §4, §11)", rt.method, rt.path, foreign)
			}
		}
	}
}

// TestEveryInternalRouteIsAuthenticated is the security claim of this surface
// stated as a rule rather than as a property of the one route that exists.
//
// The boundary ADR 0006 §9 draws is that a management call identifies itself as
// a service. A `/internal/` route added without the credential check would be an
// administrative operation open to anything that can reach the port, and — this
// is the point of checking it here rather than in a test of the row — it would
// be added by someone who copied the row above it and deleted a field they did
// not understand. The rule is stated over the prefix so the next route inherits
// the protection rather than having to remember it.
func TestEveryInternalRouteIsAuthenticated(t *testing.T) {
	internalRoutes := 0
	for _, rt := range routes(newTestApp(t)) {
		if !strings.HasPrefix(rt.path, "/internal/") {
			continue
		}
		internalRoutes++
		if !rt.authenticated {
			t.Errorf("%s %s is an internal route and is not authenticated; every management operation authenticates as a service (ADR 0006 §9)", rt.method, rt.path)
		}
	}

	// Without this the test passes on a table whose internal routes were all
	// deleted, which is the one state it exists to rule out.
	if internalRoutes == 0 {
		t.Fatal("no internal route is declared; the rule above would hold vacuously")
	}
}

// TestTheProbesNeedNoCredential is the other half of the contract's per-operation
// security: /healthz, /readyz and /version declare `security: []`, because an
// orchestrator asking whether this process is alive should not have to hold a
// deployment secret to be told "ok". A guard added to them by reflex would make
// a liveness check fail closed on a missing secret, which is how a process that
// is perfectly healthy gets restarted.
func TestTheProbesNeedNoCredential(t *testing.T) {
	for _, rt := range routes(newTestApp(t)) {
		if strings.HasPrefix(rt.path, "/internal/") {
			continue
		}
		if rt.authenticated {
			t.Errorf("%s %s requires a credential; the contract declares the probes as unauthenticated, and an orchestrator has no service identity to present", rt.method, rt.path)
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
	for _, rt := range routes(newTestApp(t)) {
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

// newTestApp returns the application this package's tables are built against.
// The reader is the stub in management_test.go and the catalog the stub world
// in groupversions_test.go: building a route table reads neither, so both stay
// untouched, and the tests that do read record what they saw there.
func newTestApp(t *testing.T) *application.App {
	t.Helper()
	return application.New("test", &stubFacts{}, newStubCatalog(&stubVersions{}))
}
