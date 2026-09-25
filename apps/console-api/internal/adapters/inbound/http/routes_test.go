package http

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
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
	for _, rt := range routes(application.New("test"), &answeringPinger{}) {
		got = append(got, rt.method+" "+rt.path)
	}
	sort.Strings(got)

	want := []string{"GET /healthz", "GET /readyz", "GET /version"}
	sort.Strings(want)

	if !slices.Equal(got, want) {
		t.Fatalf("the route table serves %v, want %v", got, want)
	}
}

// TestTheControlPlaneServesNoInferenceRoute is the boundary this repository
// exists to hold, in the one place it is mechanically checkable: whatever the
// Control Plane API grows, it does not grow the LLM request path.
func TestTheControlPlaneServesNoInferenceRoute(t *testing.T) {
	for _, rt := range routes(application.New("test"), &answeringPinger{}) {
		if strings.HasPrefix(rt.path, runtimeNamespace) {
			t.Errorf("the Control Plane API declares %s %s: %s belongs to the Data Plane runtime (ADR 0006)", rt.method, rt.path, runtimeNamespace)
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
	for _, rt := range routes(application.New("test"), &answeringPinger{}) {
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
