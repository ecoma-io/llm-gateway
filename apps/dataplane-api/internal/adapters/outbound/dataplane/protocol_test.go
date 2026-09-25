package dataplane

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	stdhttp "net/http"
)

// contractPath is this application's contract document, relative to this
// package's directory — `go test` runs each package with that directory as its
// working directory. The six levels back are: dataplane, outbound, adapters,
// internal, dataplane-api, apps.
//
// It is read here for one narrow purpose: the private listener's path is the
// same string as this contract's operation, deliberately, and both ends of the
// hop assert it against the same file. The façade's own route table is compared
// against the whole document in internal/adapters/inbound/http/contract_test.go;
// this is the other end of the wire pointing at the same source of truth.
const contractPath = "../../../../../../api/openapi/dataplane.yaml"

// This file pins the private protocol — the hop this adapter makes to the Data
// Plane's own listener — and the counterpart is
// apps/dataplane/internal/adapters/inbound/management/protocol_test.go. Neither
// can import the other: the applications are separate Go modules (ADR 0006 §1),
// so each states the same literals and each fails when its own side moves away
// from them. The protocol itself, and why it is a page of documentation rather
// than a fourth OpenAPI file, is in docs/architecture/cross-plane-protocols.md.

// TestTheRequestIsThePrivateProtocols pins the three things about the request
// that the other side of the hop depends on, none of which is visible from
// either file alone.
//
// The path is asserted against the façade's contract because the two hops use
// the same string on purpose — the façade carries the caller's values across
// rather than rewriting the request — and a rename on either side has to be a
// red test on the other. The parameter set is asserted as an *equality* rather
// than as "after and limit are present": a third parameter added here would be
// one the listener has not agreed to, and the listener refuses parameters it
// does not declare, so the extra would turn every read into a 400 discovered in
// an environment rather than a failure here.
func TestTheRequestIsThePrivateProtocols(t *testing.T) {
	declared := contractPaths(t)
	if !slices.Contains(declared, "/internal/usage-events") {
		t.Fatalf("%s declares no /internal/usage-events operation (%v); the façade's contract and this adapter have drifted apart", contractPath, declared)
	}
	if !slices.Contains(declared, usageEventsPath) {
		t.Errorf("this adapter calls %q, which %s does not declare among %v", usageEventsPath, contractPath, declared)
	}

	up := &upstream{body: settledPageBody}
	client := up.server(t)

	if _, err := client.ReadUsageEvents(context.Background(), opaqueCursor, 100); err != nil {
		t.Fatalf("ReadUsageEvents() error = %v", err)
	}
	calls := up.recorded()
	if len(calls) != 1 {
		t.Fatalf("the listener received %d calls, want 1", len(calls))
	}
	if got, want := calls[0].path, usageEventsPath; got != want {
		t.Errorf("the listener was called at %q, want %q", got, want)
	}

	// The recorded call keeps the parameters it received, so the names are read
	// back off the wire rather than off the code that wrote them.
	request, err := client.request(context.Background(), opaqueCursor, 100)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	names := make([]string, 0, 2)
	for name := range request.URL.Query() {
		names = append(names, name)
	}
	slices.Sort(names)
	if want := []string{"after", "limit"}; !slices.Equal(names, want) {
		t.Errorf("the request carries the parameters %v, want exactly %v — the private protocol declares two, and the listener refuses anything else", names, want)
	}
}

// TestTheTranslationNeverRelaysTheListenersBody is the "translate, do not
// relay" rule asserted on the one thing that could break it: the error text.
//
// The two hops answer in the same envelope shape and hold different
// vocabularies, so a façade that passed the private listener's body along would
// look correct on almost every input — the codes coincide for the failures both
// surfaces name, and only the ones they do not name would show the difference.
// The bodies below are therefore built to be unmistakable: each carries a
// marker that appears nowhere in this module, and some are not JSON at all. Any
// of them surviving into the error text is the leak this test exists to catch.
//
// It is asserted on the error rather than on a response body because this
// package produces no response body. What it returns is a classification, and
// the classification is what the inbound adapter turns into the façade's own
// answer; the body cannot travel because there is no field for it to travel in,
// and this is the test that keeps that true if one is ever added.
func TestTheTranslationNeverRelaysTheListenersBody(t *testing.T) {
	const marker = "private-listener-body-marker-3a91"

	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "a refusal from a cursor this Data Plane aged out", status: stdhttp.StatusGone, body: `{"error":{"code":"cursor_expired","message":"` + marker + `"},"request_id":"r-1"}`},
		{name: "a refusal of this façade's own credential", status: stdhttp.StatusUnauthorized, body: `{"error":{"code":"unauthenticated","message":"` + marker + `"},"request_id":"r-2"}`},
		{name: "a request this listener called invalid", status: stdhttp.StatusBadRequest, body: `{"error":{"code":"invalid_request","message":"` + marker + `"},"request_id":"r-3"}`},
		{name: "a path this listener does not serve", status: stdhttp.StatusNotFound, body: `{"error":{"code":"not_found","message":"` + marker + `"},"request_id":"r-4"}`},
		{name: "an implementation failure in the listener", status: stdhttp.StatusInternalServerError, body: `{"error":{"code":"internal","message":"` + marker + `"},"request_id":"r-5"}`},
		{name: "a body that is not JSON at all", status: stdhttp.StatusInternalServerError, body: marker},
		{name: "an envelope shape this façade never declared", status: stdhttp.StatusBadGateway, body: `{"detail":"` + marker + `"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := &upstream{status: tt.status, body: tt.body}
			client := up.server(t)

			_, err := client.ReadUsageEvents(context.Background(), opaqueCursor, 100)
			if err == nil {
				t.Fatalf("ReadUsageEvents() error = nil, want a failure for status %d", tt.status)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("the error %q carries a value from the listener's body; the façade classifies what it read and answers from its own vocabulary", err.Error())
			}
		})
	}
}

// TestTheCatalogReadIsThePrivateProtocols is the same pin for the second
// operation the hop carries, and it exists for the same reason: the path this
// adapter calls and the path the listener serves are one string agreed in two
// modules, and only a test on each side can notice one of them moving.
//
// The three assertions are shaped differently on purpose. The path is checked
// against the contract's operation list, because the façade publishes this
// operation and the private hop carries it under the same spelling. The
// placeholder is checked to still be *in* that path, because the substitution
// below depends on it: a path edited to carry the group's name literally would
// make the replacement a no-op and every read ask for `{group_name}`. And the
// substituted form is driven through a live listener, because the two checks
// above read constants while this one reads what the wire carried.
func TestTheCatalogReadIsThePrivateProtocols(t *testing.T) {
	declared := contractPaths(t)
	if !slices.Contains(declared, currentGroupVersionPath) {
		t.Fatalf("%s declares no %s operation (%v); the facade's contract and this adapter have drifted apart", contractPath, currentGroupVersionPath, declared)
	}
	if !strings.Contains(currentGroupVersionPath, groupNamePlaceholder) {
		t.Fatalf("the path %s no longer carries %s; the substitution would silently ask for the placeholder itself", currentGroupVersionPath, groupNamePlaceholder)
	}

	up := &upstream{body: settledVersionBody}
	client := up.server(t)

	if _, err := client.CurrentGroupVersion(context.Background(), "frontier"); err != nil {
		t.Fatalf("CurrentGroupVersion() error = %v", err)
	}
	calls := up.recorded()
	if len(calls) != 1 {
		t.Fatalf("the listener received %d calls, want 1", len(calls))
	}
	if got, want := calls[0].rawPath, "/internal/alias-groups/frontier/versions/current"; got != want {
		t.Errorf("the listener was called at %q, want %q", got, want)
	}
	if calls[0].rawQuery != "" {
		t.Errorf("the request carried the query %q; the private protocol declares none for this operation", calls[0].rawQuery)
	}

	// The wildcard, substituted rather than assumed: `*` is the one group name
	// this seam is required to round-trip, and its escaped form is what the
	// listener's own route has to be proved to accept — that proof lives on the
	// other module, and this is the half this module owes.
	request, err := client.groupVersionRequest(context.Background(), "*")
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if got, want := request.URL.EscapedPath(), "/internal/alias-groups/%2A/versions/current"; got != want {
		t.Errorf("the wildcard's request path is %q, want %q — the name crosses as one escaped segment", got, want)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer "+testCredential {
		t.Errorf("the request carries Authorization = %q, want the service credential", got)
	}
}

// TestTheCatalogReadsBoundsAreTheContracts pins the three bounds this adapter
// enforces on the listener's answer to the document that declares them, for the
// reason the cursor bound below is pinned: the checks live in
// currentGroupVersionBody.groupVersion, the numbers live in the YAML, and
// nothing else in this module would notice the two drifting apart.
//
// `format: uuid` is deliberately absent from the pin and from the code: it is
// an annotation on the producer, not a bound this consumer keeps, and pinning
// it would suggest a check this adapter does not make.
func TestTheCatalogReadsBoundsAreTheContracts(t *testing.T) {
	numbers := contractNumbers(t, contractPath)

	tests := []struct {
		name string
		key  string
	}{
		{name: "the version's declared minimum", key: "components.schemas.CurrentAliasGroupVersion.properties.version.minimum"},
		{name: "the group name's declared minimum length", key: "components.schemas.CurrentAliasGroupVersion.properties.group_name.minLength"},
		{name: "the id's declared minimum length", key: "components.schemas.CurrentAliasGroupVersion.properties.group_version_id.minLength"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared, ok := numbers[tt.key]
			if !ok {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", contractPath, tt.key)
			}
			if declared != 1 {
				t.Errorf("%s says %s is %d and this adapter refuses anything below 1; the answer it accepts and the answer it contracts have drifted apart", contractPath, tt.key, declared)
			}
		})
	}
}

// usageFactsPath is the fragment both ends of this hop implement, relative to
// this package's directory. This adapter holds the fragment's cursor bound as a
// constant because the bound is what it enforces on the listener's answer, and
// the constant is pinned to the document below.
const usageFactsPath = "../../../../../../api/openapi/shared/usage-facts.yaml"

// TestTheFactContractsNumbersAreTheConstants pins this adapter's cursor bound to
// the document that declares it.
//
// The reason it is needed here rather than being covered by the inbound
// package's pin: the two are different constants in different packages with the
// same value, and this one guards the *other* direction. `page()` refuses a page
// whose `next_cursor` is longer than this number, so widening it alone would let
// this process write out a body that contradicts the contract it answers under —
// a page the consumer would then store, at a length nothing in front of it
// accepts on the way back. The inbound package's constant would still be 512 and
// the façade would still be publishing a document that says 512, so nothing else
// in this module would notice.
func TestTheFactContractsNumbersAreTheConstants(t *testing.T) {
	numbers := contractNumbers(t, usageFactsPath)

	declared, ok := numbers["components.schemas.UsageCursor.maxLength"]
	if !ok {
		t.Fatalf("%s declares no usage cursor maxLength; this pin proves nothing until the scan finds it", usageFactsPath)
	}
	if declared != usageCursorMaxLength {
		t.Errorf("%s says the cursor's maximum length is %d and this adapter enforces %d; the answer it accepts and the answer it contracts have drifted apart", usageFactsPath, declared, usageCursorMaxLength)
	}
}

// contractNumbers reads every `key: <integer>` line of a YAML document and
// returns them keyed by their dotted path. It is a scanner rather than a parser
// for the reason contractPaths is: indentation-nested scalar keys are the whole
// shape being read, and a scan that stopped matching yields no entry, which the
// assertion above reports as a failure rather than skipping.
func contractNumbers(t *testing.T, path string) map[string]int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	numbers := map[string]int{}
	type frame struct {
		indent int
		key    string
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

		if value == "" {
			stack = append(stack, frame{indent: indent, key: key})
			continue
		}

		if number, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			parts := make([]string, 0, len(stack)+1)
			for _, parent := range stack {
				parts = append(parts, parent.key)
			}
			parts = append(parts, key)
			numbers[strings.Join(parts, ".")] = number
		}
	}

	if len(numbers) == 0 {
		t.Fatalf("%s yielded no integers; every pin against it would prove nothing", path)
	}
	return numbers
}

// contractPaths reads the `paths:` keys of this application's contract. It is a
// line scan rather than a parse, for the reason the inbound package's
// contract test gives: the standard library has no YAML parser, the shape being
// scanned is narrow and fixed, and a document that stopped matching the scan
// yields too few paths and fails the comparison above rather than passing it
// silently.
func contractPaths(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("reading %s: %v", contractPath, err)
	}

	paths := []string{}
	inPaths := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t")
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":"):
			paths = append(paths, strings.TrimSuffix(strings.TrimSpace(line), ":"))
		case inPaths && line != "" && !strings.HasPrefix(line, " "):
			// The section ends at the next top-level key, so a path mentioned in
			// a description later in the document is not mistaken for one.
			return paths
		}
	}
	return paths
}
