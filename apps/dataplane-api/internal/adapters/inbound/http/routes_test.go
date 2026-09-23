package http

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/application"
)

// inferenceSurface names the OpenAI-compatible paths that belong to the
// runtime, in the spelling ADR 0002 fixes for the one of them that exists. A
// management route matching any of these would put this application on the LLM
// request path — the second half of the thing ADR 0006 §11 forbids, alongside
// the Control Plane hop — and it would do it quietly, because the route would
// work.
var inferenceSurface = []string{
	"/v1/chat/completions",
	"/v1/completions",
	"/v1/embeddings",
	"/v1/messages",
	"/v1/responses",
}

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

// TestTheManagementTransportServesNoInferenceRoute is the half of the boundary
// this application owns: the LLM request path never lands on a management
// process. A management API is sized and scaled for administrative traffic and
// reaches Data Plane state over a seam the runtime does not use, so a `/v1`
// route here would be both a performance and an availability defect that a
// scaffold-sized test suite would not otherwise notice.
func TestTheManagementTransportServesNoInferenceRoute(t *testing.T) {
	for _, rt := range routes(application.New("test")) {
		for _, inference := range inferenceSurface {
			if rt.path == inference {
				t.Errorf("the management API declares %s %s: the inference surface belongs to the Data Plane runtime (ADR 0006 §11)", rt.method, rt.path)
			}
		}
	}
}

// TestTheManagementTransportServesNoConsoleRoute is the other half: the
// internal surface does not become a public one. This application has no
// generated client, no browser reachability and no public contract, and a
// route named for the console's API is how it acquires all three.
func TestTheManagementTransportServesNoConsoleRoute(t *testing.T) {
	for _, rt := range routes(application.New("test")) {
		for _, console := range consoleSurface {
			if strings.HasPrefix(rt.path, console) {
				t.Errorf("the management API declares %s %s: %s is the Control Plane's public surface (ADR 0006 §11)", rt.method, rt.path, console)
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
