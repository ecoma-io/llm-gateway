package http

import (
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
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
const contractPath = "../../../../../../api/openapi/dataplane.yaml"

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
	for _, rt := range routes(testApp()) {
		served = append(served, rt.method+" "+rt.path)
	}
	sort.Strings(served)

	if !slices.Equal(served, declared) {
		t.Fatalf("the route table serves %v and %s declares %v; the code and the contract have drifted apart", served, contractPath, declared)
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

// usageFactsPath is the fragment this application and the Data Plane both
// implement, relative to this package's directory. The façade's page size and
// cursor bounds are declared there and nowhere else, so this is the document
// the constants below are pinned against.
const usageFactsPath = "../../../../../../api/openapi/shared/usage-facts.yaml"

// TestThePageThisSurfaceWritesIsThePageTheContractDescribes pins the field names
// this surface serializes to the ones the contract declares, and it is the half
// of the pin the numbers test below cannot reach.
//
// The two are different failures. A number drifting is this surface answering a
// question differently from the contract; a *name* drifting is this surface
// answering a different question — the Control Plane's decoder reads the names,
// and a renamed field does not arrive as a wrong value but as an absent one. It
// is the worse of the two, because a Go struct tag and the document that
// describes it are two spellings of one string that nothing compiles together,
// and a rename in either is invisible in a diff of the other.
//
// The names come from the YAML rather than from a second Go literal, for the
// reason the listener's copy of this test gives: a literal list here would be
// updated by the same edit that renamed the tag, and the document — the thing
// both hops claim to implement — would never be consulted.
//
// The Data Plane's listener carries its own copy of this pin over its own
// response type, and neither can check the other: the applications are separate
// Go modules (ADR 0006 §1).
func TestThePageThisSurfaceWritesIsThePageTheContractDescribes(t *testing.T) {
	document := scanContract(t, usageFactsPath)

	tests := []struct {
		name   string
		path   string
		sample any
	}{
		{
			name:   "the page",
			path:   "components.schemas.UsageFactPage.properties",
			sample: usageEventsResponse{},
		},
		{
			name:   "the event",
			path:   "components.schemas.UsageEvent.properties",
			sample: usageEventResponse{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared := document.children[tt.path]
			if len(declared) == 0 {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", usageFactsPath, tt.path)
			}
			sort.Strings(declared)

			if got := jsonFieldNames(t, tt.sample); !slices.Equal(got, declared) {
				t.Errorf("this surface serializes %v and %s declares %v; a caller decoding this response reads the contract's names, and one that is only on one side arrives as an absent field rather than a wrong one", got, usageFactsPath, declared)
			}
		})
	}
}

// jsonFieldNames returns the JSON names a response type serializes, sorted.
//
// It is a reflection over the struct's tags rather than a reading of the encoder
// that runs, which is the closest a test can get to the wire without calling
// the handler — and it is enough, because `encoding/json` takes a field's name
// from exactly this tag and from nowhere else.
func jsonFieldNames(t *testing.T, sample any) []string {
	t.Helper()
	typ := reflect.TypeOf(sample)
	if typ.Kind() != reflect.Struct {
		t.Fatalf("jsonFieldNames was given a %s, which serializes no named fields", typ.Kind())
	}

	names := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("%s.%s carries no JSON name (%q); every field of a response type is one the contract declares, and an unnamed one serializes as its Go name", typ.Name(), field.Name, field.Tag.Get("json"))
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestTheFactContractsNumbersAreTheConstants pins the numbers this surface
// enforces — the page size's bounds and default, and the cursor's maximum and
// minimum length — to the document that declares them.
//
// It is the pin the other tests cannot provide. Every other cursor and limit
// test in this package derives its expectations from the same Go constant it is
// testing, so the constant could be changed to 4000 and the whole package would
// still pass while the surface quietly disagreed with the contract it publishes.
// Here the expectation comes from the YAML.
//
// Both hops carry the same numbers, and each module pins them against the same
// file rather than importing the other: the applications are separate Go modules
// (ADR 0006 §1), so each one's test is the only thing that can hold its own side
// to the document.
func TestTheFactContractsNumbersAreTheConstants(t *testing.T) {
	numbers := scanContract(t, usageFactsPath).numbers

	tests := []struct {
		name     string
		key      string
		constant int
	}{
		{name: "the page size a caller may not go below", key: "components.parameters.UsageEventsLimit.schema.minimum", constant: minUsageEventsLimit},
		{name: "the page size a caller may not exceed", key: "components.parameters.UsageEventsLimit.schema.maximum", constant: maxUsageEventsLimit},
		{name: "the page size used when a caller names none", key: "components.parameters.UsageEventsLimit.schema.default", constant: defaultUsageEventsLimit},
		{name: "the cursor's maximum length", key: "components.schemas.UsageCursor.maxLength", constant: maxUsageEventsCursorLength},
		{name: "the cursor's minimum length, which is why `after=` is refused", key: "components.schemas.UsageCursor.minLength", constant: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared, ok := numbers[tt.key]
			if !ok {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", usageFactsPath, tt.key)
			}
			if declared != tt.constant {
				t.Errorf("%s says %s is %d and this surface enforces %d; the code and the contract have drifted apart", usageFactsPath, tt.key, declared, tt.constant)
			}
		})
	}
}

// projectionPath is the fragment both hops of the projection protocol speak,
// relative to this package's directory. The two delivery bodies cross as the
// producer's bytes and this surface renders the two closed answers from the
// schemas declared there, so this is the document the pins below are read
// from. It is shared by the façade and the private listener, and neither
// module can hold the other to it — each carries its own copy of the pin
// (ADR 0006 §1).
const projectionPath = "../../../../../../api/openapi/shared/projection.yaml"

// TestTheProjectionShapesThisSurfaceWritesAreTheSchemasTheFragmentDeclares is
// the field-name pin for the projection answers, the same pin the usage-fact
// page carries. The names matter more here, not less: the producer's cycle
// decodes the position to decide whether to bootstrap, and a name that exists
// only on one side of this surface does not arrive as a wrong value but as an
// absent one — which the producer would read as a mirror that never
// bootstrapped, and answer with a snapshot nobody needed.
func TestTheProjectionShapesThisSurfaceWritesAreTheSchemasTheFragmentDeclares(t *testing.T) {
	document := scanContract(t, projectionPath)

	tests := []struct {
		name   string
		path   string
		sample any
	}{
		{
			name:   "the position",
			path:   "components.schemas.ProjectionPosition.properties",
			sample: projectionPositionResponse{},
		},
		{
			name:   "the acknowledgement",
			path:   "components.schemas.ProjectionAppliedAck.properties",
			sample: projectionAckResponse{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared := document.children[tt.path]
			if len(declared) == 0 {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", projectionPath, tt.path)
			}
			sort.Strings(declared)

			if got := jsonFieldNames(t, tt.sample); !slices.Equal(got, declared) {
				t.Errorf("this surface serializes %v and %s declares %v; the producer reads the contract's names, and one that is only on one side arrives as an absent field rather than a wrong one", got, projectionPath, declared)
			}
		})
	}
}

// TestTheProjectionContractsNumbersAreTheConstants pins the one number this
// surface enforces on the projection path — the body bound both delivery
// operations declare — to the document that declares it, and the document's
// two declarations to each other.
//
// The bound is the transport's whole contribution to the delivery path, and it
// is the number a well-meaning edit would most easily let drift: sized into
// the code so a full snapshot fits, declared in the contract so a caller can
// rely on it, and enforced by the listener a hop behind — all three must move
// together, and each pin here is what notices one of them moving alone.
func TestTheProjectionContractsNumbersAreTheConstants(t *testing.T) {
	numbers := scanContract(t, contractPath).numbers

	tests := []struct {
		name     string
		key      string
		constant int
	}{
		{
			name:     "the snapshot delivery's body bound",
			key:      "paths./internal/projection/snapshot.post.x-max-body-bytes",
			constant: maxProjectionBodyBytes,
		},
		{
			name:     "the changes delivery's body bound",
			key:      "paths./internal/projection/changes.post.x-max-body-bytes",
			constant: maxProjectionBodyBytes,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared, ok := numbers[tt.key]
			if !ok {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", contractPath, tt.key)
			}
			if declared != tt.constant {
				t.Errorf("%s says %s is %d and this surface enforces %d; the code and the contract have drifted apart", contractPath, tt.key, declared, tt.constant)
			}
		})
	}

	t.Run("the fragment's array bounds are the ones the bound was sized for", func(t *testing.T) {
		fragment := scanContract(t, projectionPath).numbers

		tests := []struct {
			name string
			key  string
		}{
			{name: "the snapshot's api_keys array", key: "components.schemas.ProjectionSnapshot.properties.api_keys.maxItems"},
			{name: "the snapshot's accounts array", key: "components.schemas.ProjectionSnapshot.properties.accounts.maxItems"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				declared, ok := fragment[tt.key]
				if !ok {
					t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", projectionPath, tt.key)
				}
				// The two arrays at their bound must fit inside the body bound the
				// deliveries declare. The assertion is against the constant rather
				// than recomputed, because the arithmetic is the design: 5000
				// records of a bounded credential row, twice, inside ten mebibytes.
				if declared != 5000 {
					t.Errorf("%s says %s is %d, want 5000; the body bound was sized against this number and the two moving apart is a reviewed decision, not a rounding one", projectionPath, tt.key, declared)
				}
			})
		}
	})
}

// contractDocument is what scanning a YAML file yields: the integer scalars it
// declares, and the child keys of every mapping. Both are keyed by dotted path,
// so a `minimum` under one schema and a `minimum` under another are two
// different keys and neither is confused for the other.
type contractDocument struct {
	numbers  map[string]int
	children map[string][]string
}

// scanContract reads a YAML document's indentation-nested keys.
//
// It is a scanner rather than a parser, for the reason contractOperations is:
// the standard library has no YAML parser, the shape being read is narrow —
// indentation-nested scalar keys — and this repository writes these documents by
// hand. A document that stopped matching the scan yields nothing, and every
// caller above treats an empty result as a failure rather than a skip, so the
// scan going blind is loud instead of vacuous.
//
// Keys are pushed and popped by indentation, so `minimum` under one schema and
// `minimum` under another are two different paths. Sequence entries and folded
// description text fall out on their own: a line is only read as a key when it
// reads exactly as `indent key: value` with no whitespace inside the key, and
// prose lines containing a colon have spaces before it.
func scanContract(t *testing.T, path string) contractDocument {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	document := contractDocument{numbers: map[string]int{}, children: map[string][]string{}}
	type frame struct {
		indent int
		key    string
		path   string
	}
	stack := []frame{}

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "- ") {
			continue
		}

		indent := len(line) - len(strings.TrimLeft(line, " "))
		key, value, found := strings.Cut(trimmed, ":")
		if !found || key == "" || strings.ContainsAny(key, " \t") {
			continue
		}

		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}

		parts := make([]string, 0, len(stack)+1)
		for _, parent := range stack {
			parts = append(parts, parent.key)
		}
		parts = append(parts, key)
		path := strings.Join(parts, ".")

		parentPath := ""
		if len(stack) > 0 {
			parentPath = stack[len(stack)-1].path
		}
		document.children[parentPath] = append(document.children[parentPath], key)

		if value == "" {
			// A mapping whose children follow at a deeper indent.
			stack = append(stack, frame{indent: indent, key: key, path: path})
			continue
		}

		if number, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			document.numbers[path] = number
		}
	}

	if len(document.numbers) == 0 || len(document.children) == 0 {
		t.Fatalf("%s yielded no keys; every pin against it would prove nothing", path)
	}
	return document
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
