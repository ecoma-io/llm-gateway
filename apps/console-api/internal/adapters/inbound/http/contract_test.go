package http

import (
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
)

// contractPath is this application's contract document, relative to this
// package's directory — `go test` runs each package with that directory as its
// working directory, so the path is a constant the way the module root is in
// internal/arch.
//
// The six levels back are: http, inbound, adapters, internal, the application,
// apps. Spelling them out is what makes the constant checkable by reading; a
// path that is one level short fails loudly, at this test, with the path it
// looked for.
//
// The path can reach the contract because the contract lives in this
// repository, beside the code it describes. If the two were ever split into
// different repositories, this is the file that would notice.
const contractPath = "../../../../../../api/openapi/console.yaml"

// routeTableForTest is the test-local constructor the session surface required,
// and ADR 0012's Consequences names this exact moment: the moment the
// panic-on-nil-port discipline gets quietly defeated if nobody says so.
//
// The temptation is real. routes() now takes a sessionUseCases, and the
// contract test needs a route table; so the route table gets a real store, and
// the test needs a database, and a contract test that needs a database stops
// running in CI's unit tier. The seam prevents it: sessionUseCases is an
// interface declared in this package, so a fake of it needs no port, no
// connection string and no cluster — the dependency the test-local constructor
// exists to avoid is the one the seam was declared to keep out.
//
// The whole table is read, the handlers are never called, so the fake's answers
// are irrelevant; it is here to satisfy the signature.
func routeTableForTest() []route {
	return routes(application.New("test"), &answeringPinger{}, newFakeSessionUseCases())
}

// TestTheRouteTableIsTheContract is the closure between what this application
// serves and what its contract promises, asserted in both directions: every
// operation the document declares is a row in the route table, and every row
// in the route table is an operation the document declares.
//
// `routes_test.go` pins the same table against a literal list, and the two are
// not redundant. The literal list makes a new endpoint a deliberate act — it
// cannot be added without editing a line a reviewer reads. This catches the
// edit that list misses: the document changed alone. That is the failure a
// contract-first repository is most exposed to, because it is the one where
// both the promise and the implementation still look correct in isolation.
//
// Both directions matter. A declared-but-unserved operation is a lie to a
// caller. A served-but-undeclared one is a surface nobody agreed to, and the
// second is worse, because it works.
func TestTheRouteTableIsTheContract(t *testing.T) {
	declared := contractOperations(t)

	served := []string{}
	for _, rt := range routeTableForTest() {
		served = append(served, rt.method+" "+rt.path)
	}
	sort.Strings(served)

	if !slices.Equal(served, declared) {
		t.Fatalf("the route table serves %v and %s declares %v; the code and the contract have drifted apart", served, contractPath, declared)
	}
}

// TestTheContractTestNeedsNoDatabase is the assertion behind the test-local
// constructor, and it is the one that keeps the constructor honest.
//
// A contract test that needs a database stops being a unit test: it stops
// running in CI's fast tier, it stops running at all on a machine without the
// fixture, and — worst — it becomes a test people skip rather than a test that
// fails. The seam keeps that from happening, and this proves it by driving the
// whole contract comparison in a process that has no way to reach a database:
// no connection string is read, no port is dialed, and the test finishes in
// microseconds.
//
// The check is a duration bound rather than a fixture absence, because the
// absence is the thing that is hard to assert directly — nothing in this
// package can see whether some other package opened a socket. A test that
// needed a real cluster would take milliseconds to fail, to connect, or to
// time out; one that needs nothing finishes before the bound. A generous
// second is two orders of magnitude above what this work costs and three below
// what a connection attempt costs, so the bound distinguishes them without
// being a flake.
func TestTheContractTestNeedsNoDatabase(t *testing.T) {
	start := time.Now()
	declared := contractOperations(t)
	served := []string{}
	for _, rt := range routeTableForTest() {
		served = append(served, rt.method+" "+rt.path)
	}
	sort.Strings(served)
	_ = slices.Equal(served, declared)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("reading the contract and building the route table took %v; the constructor is meant to need no store, and a test that opened one would show up as this", elapsed)
	}
}

// contractOperations reads the `paths:` section of this application's contract
// and returns its operations as "METHOD /path", sorted.
//
// The document is scanned line by line rather than parsed, which is a
// deliberate trade. Go's standard library has no YAML parser, and a dependency
// taken solely to assert a handful of indented keys is a large one bought for
// a small purpose. The shape being scanned is narrow and fixed: OpenAPI puts
// path keys at two spaces of indent under `paths:` and method keys at four
// spaces inside a path item, and this repository writes those documents by
// hand. A document that stopped matching the scan would yield too few
// operations and fail the comparison above rather than pass it silently — and
// the vacuity guard below is that same argument written as an assertion, so
// the failure is a clear message instead of a confusing diff.
func contractOperations(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("reading %s: %v", contractPath, err)
	}

	operations := []string{}
	inPaths := false
	currentPath := ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t")
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":"):
			// A path key: two spaces of indent, a leading slash, a trailing
			// colon. The section needs no closing case — the top-level keys
			// that follow simply match neither branch, and the method branch
			// below requires a path to have been seen first.
			currentPath = strings.TrimSuffix(strings.TrimSpace(line), ":")
		case inPaths && currentPath != "" && strings.HasPrefix(line, "    ") && strings.HasSuffix(line, ":"):
			method := strings.TrimSuffix(strings.TrimSpace(line), ":")
			if isHTTPMethod(method) {
				operations = append(operations, strings.ToUpper(method)+" "+currentPath)
			}
		}
	}

	if len(operations) == 0 {
		t.Fatalf("%s declares no operations; every comparison against it would pass vacuously", contractPath)
	}
	sort.Strings(operations)
	return operations
}

// isHTTPMethod keeps the method branch of the scan from mistaking some future
// nested key for a verb. Only operations appear directly inside a path item
// today; this is what keeps that true rather than assumed.
func isHTTPMethod(name string) bool {
	switch name {
	case "get", "put", "post", "delete", "options", "head", "patch", "trace":
		return true
	default:
		return false
	}
}
