package dataplane

import (
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// usageFactsPath is the fragment this adapter decodes, relative to this
// package's directory — `go test` runs each package with that directory as its
// working directory. The six levels back are: dataplane, outbound, adapters,
// internal, console-api, apps.
//
// This is the third module to point at this document, and the three are not
// redundant. The applications are separate Go modules (ADR 0006 §1), so no test
// can hold two of them at once; each module's test is the only thing that can
// hold its own side to the file, and a contract no side is held to is a
// document rather than an agreement.
const usageFactsPath = "../../../../../../api/openapi/shared/usage-facts.yaml"

// This file is the Control Plane's end of the fact contract, pinned against the
// document rather than against the constants beside it.
//
// Everything else in this package derives its expectations from the same Go
// values it exercises: the cursor bound is asserted with the constant the check
// uses, and the decoded fields are asserted with a JSON literal written to match
// the struct tags. That is the right way to test behaviour and the wrong way to
// test agreement — a constant renamed, a bound widened or a tag re-spelled stays
// green in every one of those tests, because the expectation moved with the
// code. What follows reads the contract instead, so the one file both planes
// claim to implement is the thing the code is measured against.

// TestThePageThisAdapterDecodesIsThePageTheContractDescribes pins the field
// names this adapter reads to the ones the contract declares.
//
// A decode is the place where a renamed field is most quietly wrong. `events`,
// `next_cursor` and `has_more` are three strings in a struct tag, and so are
// the five fields of a fact. The decoder refuses a required field that arrives
// absent or null, so a rename on one side no longer lands on a usable zero
// value — but that refusal is measured against the tags themselves, and tags
// that moved together would agree with each other and with nothing else. What
// follows reads the contract instead, so the one file both planes claim to
// implement is the thing the code is measured against.
func TestThePageThisAdapterDecodesIsThePageTheContractDescribes(t *testing.T) {
	document := scanContract(t, usageFactsPath)

	tests := []struct {
		name   string
		path   string
		sample any
	}{
		{
			name:   "the page",
			path:   "components.schemas.UsageFactPage.properties",
			sample: pageResponse{},
		},
		{
			name:   "the event",
			path:   "components.schemas.UsageEvent.properties",
			sample: eventResponse{},
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
				t.Errorf("this adapter decodes %v and %s declares %v; a name on one side and not the other decodes to a zero value rather than failing, so the disagreement would arrive as an empty position rather than as an error", got, usageFactsPath, declared)
			}
		})
	}
}

// jsonFieldNames returns the JSON names a response type reads, sorted.
//
// It reflects over the struct's tags rather than over the decoder that runs,
// which is the closest a test can get to the wire without a server — and it is
// enough, because `encoding/json` takes a field's name from exactly this tag
// and from nowhere else.
func jsonFieldNames(t *testing.T, sample any) []string {
	t.Helper()
	typ := reflect.TypeOf(sample)
	if typ.Kind() != reflect.Struct {
		t.Fatalf("jsonFieldNames was given a %s, which reads no named fields", typ.Kind())
	}

	names := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("%s.%s carries no JSON name (%q); every field of a response type is one the contract declares, and an unnamed one is read from the Go name", typ.Name(), field.Name, field.Tag.Get("json"))
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestTheFactContractsNumbersAreTheConstants pins this adapter's copy of the
// contract's numbers to the document that declares them.
//
// The adapter holds one number of its own — `usageCursorMaxLength` — and it is
// the one that decides whether a page the Data Plane issued is a page the
// Control Plane can store. Widening it here while the contract kept 512 would
// let this hop accept a position neither HTTP surface will ever hand back, and
// narrowing it would refuse one the contract permits: both are disagreements
// with the document, both are invisible to every behaviour test in this package
// because those tests use the same constant, and both are red here.
//
// The page size the Control Plane asks for is deliberately not on this list. It
// is not a copy of the contract's default — a consumer chooses its own page size
// and the contract only bounds it — so pinning it to 100 would turn a choice
// into an agreement. It is held to its own rule in
// internal/application/factingestion_test.go, against these same bounds.
func TestTheFactContractsNumbersAreTheConstants(t *testing.T) {
	numbers := scanContract(t, usageFactsPath).numbers

	tests := []struct {
		name     string
		key      string
		constant int
	}{
		{name: "the cursor's maximum length", key: "components.schemas.UsageCursor.maxLength", constant: usageCursorMaxLength},
		{name: "the cursor's minimum length, which is why an empty position is refused", key: "components.schemas.UsageCursor.minLength", constant: 1},
		{name: "the page size a caller may not go below", key: "components.parameters.UsageEventsLimit.schema.minimum", constant: 1},
		{name: "the page size a caller may not exceed", key: "components.parameters.UsageEventsLimit.schema.maximum", constant: 1000},
		{name: "the page size used when a caller names none", key: "components.parameters.UsageEventsLimit.schema.default", constant: 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared, ok := numbers[tt.key]
			if !ok {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", usageFactsPath, tt.key)
			}
			if declared != tt.constant {
				t.Errorf("%s says %s is %d and this adapter is written against %d; the code and the contract have drifted apart", usageFactsPath, tt.key, declared, tt.constant)
			}
		})
	}
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
// It is a scanner rather than a parser because Go's standard library has no YAML
// parser and the shape being read is narrow: indentation-nested scalar keys, in
// documents this repository writes by hand. A document that stopped matching the
// scan yields nothing, and every caller above treats an empty result as a
// failure rather than a skip, so the scan going blind is loud instead of
// vacuous.
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
