package http

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
)

// inferenceSurface names the OpenAI-compatible paths that belong to the Data
// Plane's runtime, in the spelling ADR 0002 fixes for the one of them that
// exists. A Control Plane route matching any of these would put the LLM
// request path behind the console's backend — the single thing ADR 0006
// forbids — and it would do it quietly, because the route would work.
var inferenceSurface = []string{
	"/v1/chat/completions",
	"/v1/completions",
	"/v1/embeddings",
	"/v1/messages",
	"/v1/responses",
}

// TestTheSurfaceIsTheDeclaredSet pins what this application serves. It fails
// on an endpoint added without a decision: the table, this list and
// api/openapi/openapi.yaml move together, and a new row is one edit in each.
// It is deliberately an equality rather than a subset — a route silently
// dropped is as much a defect as one silently added.
func TestTheSurfaceIsTheDeclaredSet(t *testing.T) {
	got := []string{}
	for _, rt := range routes(application.New("test")) {
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
	for _, rt := range routes(application.New("test")) {
		for _, inference := range inferenceSurface {
			if rt.path == inference {
				t.Errorf("the Control Plane API declares %s %s: the inference surface belongs to the Data Plane runtime (ADR 0006)", rt.method, rt.path)
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
	for _, rt := range routes(application.New("test")) {
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
