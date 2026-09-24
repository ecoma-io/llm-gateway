package dataplane

import (
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// This file is the façade's outbound end of the fact contract, pinned against
// the document rather than against the JSON literals beside it.
//
// The page travels three wire vocabularies — the listener's write, this
// adapter's read, and the consumer's read — and the applications are separate
// Go modules (ADR 0006 §1), so each module's test can hold only its own side
// to the file. The consumer pins its read (console-api's contract_test.go) and
// the listener pins its write (the management package's protocol_test.go);
// nothing pinned this re-encoding, and a tag re-spelled here together with
// this package's literals would stay green in every behaviour test and
// surface only as this façade refusing every read at runtime.

// TestThePageThisAdapterDecodesIsThePageTheContractDescribes pins the field
// names this adapter reads to the ones the contract declares. The refusal this
// package's decoder makes for an absent or null field is measured against its
// own tags, so tags that moved together would agree with each other and with
// nothing else; what follows reads the contract instead, so the one file both
// sides of the hop claim to implement is the thing the code is measured
// against.
func TestThePageThisAdapterDecodesIsThePageTheContractDescribes(t *testing.T) {
	document := contractChildren(t, usageFactsPath)

	tests := []struct {
		name   string
		path   string
		sample any
	}{
		{
			name:   "the page",
			path:   "components.schemas.UsageFactPage.properties",
			sample: pageBody{},
		},
		{
			name:   "the event",
			path:   "components.schemas.UsageEvent.properties",
			sample: eventBody{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared := document[tt.path]
			if len(declared) == 0 {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", usageFactsPath, tt.path)
			}
			sort.Strings(declared)

			if got := jsonFieldNames(t, tt.sample); !slices.Equal(got, declared) {
				t.Errorf("this adapter decodes %v and %s declares %v; a name on one side and not the other is refused rather than decoded, so the disagreement would arrive as this façade answering 502 to every read", got, usageFactsPath, declared)
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

// contractChildren reads every mapping's child keys of a YAML document and
// returns them keyed by their dotted path, so `properties` under one schema
// and `properties` under another are two different entries. It is a scanner
// rather than a parser for the reason contractNumbers is: indentation-nested
// scalar keys are the whole shape being read, and a scan that stopped matching
// yields no entry, which the assertion above reports as a failure rather than
// skipping.
func contractChildren(t *testing.T, path string) map[string][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	children := map[string][]string{}
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
		keyPath := strings.Join(parts, ".")

		parentPath := ""
		if len(stack) > 0 {
			parentPath = stack[len(stack)-1].path
		}
		children[parentPath] = append(children[parentPath], key)

		if value == "" {
			// A mapping whose children follow at a deeper indent.
			stack = append(stack, frame{indent: indent, key: key, path: keyPath})
		}
	}

	if len(children) == 0 {
		t.Fatalf("%s yielded no keys; every pin against it would prove nothing", path)
	}
	return children
}
