package management

import (
	"encoding/json"
	"go/ast"
	"go/token"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	stdhttp "net/http"
	"net/http/httptest"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/usagefacts"
)

// grammarValidBatchBody is a delivered batch that passes the grammar and
// reaches the application, for the rows that assert on how an application
// answer is translated. Its numbers mean nothing; its shape means everything.
const grammarValidBatchBody = `{"protocol_version":1,` +
	`"epoch":"0b6fd7a1-3f6e-4a55-9a21-5c8f2e7d1b90","from_revision":10,` +
	`"changes":[{"revision":11,"resource_kind":"account",` +
	`"resource_id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",` +
	`"recorded_at":"2026-09-24T10:00:00Z","payload":{"state":"active"}}]}`

// contractPath is the façade's contract, read from here because the two hops
// have to agree about one thing and this is where that agreement is checked.
//
// The six levels back are: management, inbound, adapters, internal, dataplane,
// apps. `go test` runs a package with its own directory as the working
// directory, so this constant is the whole navigation.
const contractPath = "../../../../../../api/openapi/dataplane.yaml"

// This file pins the private protocol — the hop between dataplane-api and this
// listener — rather than the contract this listener does not implement.
//
// The two are easy to conflate and the conflation is the defect this file
// exists to catch: `dataplane.yaml` is the façade's document, the façade is what
// a contracted caller reaches, and what this process serves is a protocol
// between two processes of one product, defined in
// docs/architecture/cross-plane-protocols.md. `routes_test.go` pins what this
// surface is against a literal list; the tests below pin the parts of it that
// the *other* side of the hop depends on, so that a change here that the façade
// would have to follow is a red test on this side rather than a discovery at a
// deployment.
//
// The counterpart is apps/dataplane-api/internal/adapters/outbound/dataplane/
// protocol_test.go. Neither file can import the other — the applications are
// separate Go modules (ADR 0006 §1) — so each states the same literals and each
// fails when its own side moves away from them.

// TestThePathIsTheOneTheFacadeContracts pins the deliberate collision of the two
// hops' path strings.
//
// This listener's path is the private protocol's, and the private protocol is
// not `dataplane.yaml`. The two are nevertheless the same string, and that is a
// decision rather than an accident: the façade carries the caller's values
// across rather than rewriting the request, and a path rewritten hop by hop
// would be a translation step the protocol has to describe and could get wrong. What
// makes the sameness safe is that both sides assert it — the façade's
// `usageEventsPath` against this same document, this constant against the same
// document — so renaming the operation on the façade's side without renaming it
// here is a failing test and not a 404 discovered in an environment.
func TestThePathIsTheOneTheFacadeContracts(t *testing.T) {
	declared := contractPaths(t)

	found := false
	for _, rt := range routes(newTestApp(t)) {
		if !strings.HasPrefix(rt.path, "/internal/") {
			continue
		}
		found = true
		if !slices.Contains(declared, rt.path) {
			t.Errorf("this listener serves %s %s, and %s declares no such path (%v); the private protocol's path and the façade's are the same string on purpose, and this is the test that keeps them so", rt.method, rt.path, contractPath, declared)
		}
	}
	if !found {
		t.Fatal("no internal route is declared; the assertion above would hold vacuously")
	}
}

// contractPaths reads the `paths:` keys of the façade's contract.
//
// It is a line scan rather than a parse, for the reason the façade's own
// contract test gives: the standard library has no YAML parser, the shape being
// scanned is narrow and fixed, and a document that stopped matching the scan
// yields too few paths and fails the comparison above rather than passing it
// silently. The vacuity guard is there too, in the caller.
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
			// The section ends at the next top-level key, so a path that appears
			// in a description later in the document cannot be mistaken for one.
			return paths
		}
	}
	return paths
}

// TestThePageThisListenerWritesIsThePageTheContractDescribes pins the shape the
// two hops share, field by field, without pinning a single byte of a value.
//
// The façade's decoder reads exactly these keys and re-encodes exactly them, so
// a field renamed here is a column that silently stops arriving at the Control
// Plane; a field added here is one the façade drops without a word. Both are
// invisible in a diff that only shows the handler, which is why the key sets are
// asserted rather than the bytes: the byte-level tests belong to
// management_test.go, where a wrong value is the subject, and this one is about
// the envelope both sides decode.
//
// The expected key sets are read out of api/openapi/shared/usage-facts.yaml
// rather than written twice in Go, and that is the difference between this pin
// and a restatement. A literal list here would pin the handler to itself: the
// same edit that renamed `next_cursor` in the handler would rename it in the
// list, every module would stay green, and the contract both hops claim to
// implement would be the one file nobody consulted. Reading the document means
// the rename is red here, on the side that produces the page, at the moment it
// is made.
func TestThePageThisListenerWritesIsThePageTheContractDescribes(t *testing.T) {
	facts := &stubFacts{page: usagefacts.Page{
		Events: []usagefacts.Event{{
			RequestID:     "req_1",
			Kind:          "settled",
			SchemaVersion: 1,
			OccurredAt:    time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
			Payload:       json.RawMessage(`{"tokens":7}`),
		}},
		NextCursor: "position-7",
		HasMore:    true,
	}}

	rec := serve(t, facts, authed(t, "/internal/usage-events"))
	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, stdhttp.StatusOK, rec.Body.String())
	}

	document := scanContract(t, usageFactsPath)

	var page map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding the page from %s: %v", rec.Body.String(), err)
	}
	wantPage := declaredFields(t, document, "components.schemas.UsageFactPage.properties")
	if got := sortedKeys(page); !slices.Equal(got, wantPage) {
		t.Errorf("the page's keys are %v and %s declares %v — this is the envelope the façade decodes, and a key added or renamed here is one it would drop in silence", got, usageFactsPath, wantPage)
	}

	var events []map[string]json.RawMessage
	if err := json.Unmarshal(page["events"], &events); err != nil {
		t.Fatalf("decoding events from %s: %v", rec.Body.String(), err)
	}
	if len(events) != 1 {
		t.Fatalf("the page carries %d event(s), want 1", len(events))
	}
	wantEvent := declaredFields(t, document, "components.schemas.UsageEvent.properties")
	if got := sortedKeys(events[0]); !slices.Equal(got, wantEvent) {
		t.Errorf("the event's keys are %v and %s declares %v — the façade's decoder names exactly these, and a fact's field that stops being written here stops reaching the Control Plane", got, usageFactsPath, wantEvent)
	}
}

// declaredFields returns the property names a schema declares, sorted, and
// refuses to return an empty set.
//
// The guard is the whole reason this is a function: a scan that lost its
// footing would return nothing, and an assertion against nothing passes for
// every handler — including one that writes no page at all. A field set this
// pin cannot read is a pin that proves nothing, so it fails instead.
func declaredFields(t *testing.T, document contractDocument, path string) []string {
	t.Helper()
	fields := document.children[path]
	if len(fields) == 0 {
		t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", usageFactsPath, path)
	}
	sorted := slices.Clone(fields)
	sort.Strings(sorted)
	return sorted
}

// TestTheFailureVocabularyIsThisSurfacesOwn is the error-semantics half of the
// façade/private separation, asserted by making this listener produce every
// failure it can and reading what it says.
//
// The point is not that the codes are nice. It is that the set is closed and
// that it is *this* set: the façade answers a caller under its own contract, and
// one of its statuses — 502 `upstream_unavailable` — is its word for "the Data
// Plane did not answer", which is not a sentence this process could say about
// itself. If that code were producible here, the façade's 502 would have stopped
// meaning "I could not reach the Data Plane" and started meaning "something went
// wrong somewhere", and an operator would lose the one fact that tells them
// which side to look at.
//
// The last two rows are the same underlying condition — the fact source is
// unreadable — reached from both surfaces. Here it is this process's own
// storage, so it is a 500; at the façade it is a peer it could not reach, so
// it is a 502. Two answers to one condition is exactly what "the two sides must
// not accidentally share a wire-error semantics" means, and it is why the
// façade translates rather than relays.
func TestTheFailureVocabularyIsThisSurfacesOwn(t *testing.T) {
	tests := []struct {
		name       string
		facts      *stubFacts
		applier    *stubProjection
		authorized bool
		method     string
		target     string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "a parameter this operation cannot read",
			facts:      &stubFacts{},
			authorized: true,
			method:     stdhttp.MethodGet,
			target:     "/internal/usage-events?limit=0",
			wantStatus: stdhttp.StatusBadRequest,
			wantCode:   codeInvalidRequest,
		},
		{
			name:       "a caller that did not identify itself",
			facts:      &stubFacts{},
			method:     stdhttp.MethodGet,
			target:     "/internal/usage-events",
			wantStatus: stdhttp.StatusUnauthorized,
			wantCode:   codeUnauthenticated,
		},
		{
			name:       "a path this listener does not serve",
			facts:      &stubFacts{},
			authorized: true,
			method:     stdhttp.MethodGet,
			target:     "/not-a-listener-path",
			wantStatus: stdhttp.StatusNotFound,
			wantCode:   codeNotFound,
		},
		{
			name:       "a method this path does not accept",
			facts:      &stubFacts{},
			authorized: true,
			method:     stdhttp.MethodDelete,
			target:     "/internal/usage-events",
			wantStatus: stdhttp.StatusMethodNotAllowed,
			wantCode:   codeMethodNotAllowed,
		},
		{
			name:       "a position this Data Plane can no longer replay",
			facts:      &stubFacts{err: usagefacts.ErrCursorExpired},
			authorized: true,
			method:     stdhttp.MethodGet,
			target:     "/internal/usage-events",
			wantStatus: stdhttp.StatusGone,
			wantCode:   codeCursorExpired,
		},
		{
			// Where the façade answers 502 with upstream_unavailable. This
			// process has no upstream to blame and says so.
			name:       "a fact source this process cannot read",
			facts:      &stubFacts{err: usagefacts.ErrSourceUnavailable},
			authorized: true,
			method:     stdhttp.MethodGet,
			target:     "/internal/usage-events",
			wantStatus: stdhttp.StatusInternalServerError,
			wantCode:   codeInternal,
		},
		{
			// The protocol fails closed: a version this plane does not speak is
			// refused before anything is read or applied.
			name:       "a message in a protocol version this plane does not speak",
			authorized: true,
			method:     stdhttp.MethodPost,
			target:     "/internal/projection/changes",
			body:       `{"protocol_version":99}`,
			wantStatus: stdhttp.StatusBadRequest,
			wantCode:   codeUnsupportedVersion,
		},
		{
			name:       "a batch that cannot join the applied position",
			applier:    &stubProjection{err: projection.ErrRevisionGap},
			authorized: true,
			method:     stdhttp.MethodPost,
			target:     "/internal/projection/changes",
			body:       grammarValidBatchBody,
			wantStatus: stdhttp.StatusConflict,
			wantCode:   codeRevisionGap,
		},
		{
			name:       "a stored position that cannot join the producer timeline",
			applier:    &stubProjection{err: projection.ErrSnapshotRequired},
			authorized: true,
			method:     stdhttp.MethodPost,
			target:     "/internal/projection/changes",
			body:       grammarValidBatchBody,
			wantStatus: stdhttp.StatusConflict,
			wantCode:   codeSnapshotRequired,
		},
		{
			// A batch that would move a terminal state backwards shares the
			// shape refusal's status and code: the message is at fault, and
			// no retry of it can succeed.
			name:       "a batch that would move a terminal state backwards",
			applier:    &stubProjection{err: projection.ErrTerminalRegression},
			authorized: true,
			method:     stdhttp.MethodPost,
			target:     "/internal/projection/changes",
			body:       grammarValidBatchBody,
			wantStatus: stdhttp.StatusBadRequest,
			wantCode:   codeInvalidRequest,
		},
	}

	produced := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
			if tt.authorized {
				request.Header.Set(authorizationHeader, credentialScheme+" "+serviceCredential)
			}
			var rec *httptest.ResponseRecorder
			if tt.applier != nil {
				rec = serveProjection(t, tt.applier, request)
			} else {
				rec = serve(t, tt.facts, request)
			}

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := envelopeCode(t, rec); got != tt.wantCode {
				t.Fatalf("error code = %q, want %q", got, tt.wantCode)
			}
			if strings.Contains(rec.Body.String(), "upstream_unavailable") {
				t.Errorf("body = %s: this listener has no upstream, so it must never answer with the façade's word for one it could not reach", rec.Body.String())
			}
			produced[tt.wantCode] = true
		})
	}

	// The two lists are the same list, and asserting it is what keeps the table
	// above from being a sample rather than a census: a failure added to this
	// surface without a row here, or a row deleted with its failure left in the
	// code, shows up as a difference rather than as nothing.
	//
	// The declared half is read out of errors.go rather than written again here,
	// which is the difference between a census and a restatement. A literal list
	// in this test would be edited by the same change that added the constant, so
	// a code added, renamed or removed would move both sides of the comparison at
	// once and the vocabulary would grow with nothing holding it to the rows
	// above. Reading the file means a new code is red here on the commit that
	// introduces it — for having no row — which is what makes the table complete
	// rather than merely correct.
	declared := declaredCodes(t)

	got := make([]string, 0, len(produced))
	for code := range produced {
		got = append(got, code)
	}
	sort.Strings(got)
	sort.Strings(declared)
	if !slices.Equal(got, declared) {
		t.Errorf("this surface can produce %v and errors.go declares %v; the two lists are one list, so a code with no row above is a status nothing has ever asserted — and a row with no code is a table of a failure that no longer exists", got, declared)
	}
	if slices.Contains(declared, "upstream_unavailable") {
		t.Error("upstream_unavailable is in this surface's vocabulary; it names a reach beyond this process that does not exist")
	}
}

// declaredCodes reads the string values of the `code*` constants errors.go
// declares, sorted.
//
// It fails rather than skips on a constant it cannot read, because the census
// above treats this list as the surface's complete vocabulary: a constant
// assigned something other than a literal — a concatenation, a variable — would
// be one this census had no opinion about while still claiming to be complete.
//
// What it still does not catch, and what the test below is for: a failure
// written where no constant is involved. A `writeFailure(w, r, failure{status:
// 403, code: "forbidden"})` inline in a handler adds a code to the surface
// without adding a constant here, so this census would be comparing the same
// two lists as before while the wire had grown a third.
func declaredCodes(t *testing.T) []string {
	t.Helper()
	file, _ := parseSource(t, "errors.go")

	codes := []string{}
	for _, declaration := range file.Decls {
		group, ok := declaration.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if !strings.HasPrefix(name.Name, "code") {
					continue
				}
				if i >= len(value.Values) {
					t.Fatalf("errors.go declares %s with no value; the census above would be comparing against a constant it never read", name.Name)
				}
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("errors.go declares %s as something other than a string literal; every code on the wire is spelled here, and the census above reads this file to know which ones exist", name.Name)
				}
				code, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("errors.go declares %s as %s, which is not a quoted string: %v", name.Name, literal.Value, err)
				}
				codes = append(codes, code)
			}
		}
	}

	if len(codes) == 0 {
		t.Fatal("errors.go declares no code* constants; the census above would compare this surface's answers against nothing")
	}
	return codes
}

// TestEveryFailureIsDeclaredInOneFile closes the hole declaredCodes leaves: a
// failure built inline, in a handler, out of a literal code.
//
// The census above proves that the surface produces exactly the codes errors.go
// declares. That sentence is only as strong as "every failure comes from
// errors.go", and nothing about the census establishes it — an inline failure in
// a handler produces a code no constant names, so the census sees the same two
// lists it saw before and passes. This is the missing half: every `failure`
// composite literal in the package's non-test files lives in errors.go.
//
// It is a restriction rather than an observation, and it is a small one. A
// failure is a status, a code and a message decided together; errors.go is where
// that decision is written down, and the file is one screen of constructors. A
// handler that built its own would be a second place the wire vocabulary is
// spelled, which is the thing this pair of tests exists to refuse.
func TestEveryFailureIsDeclaredInOneFile(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	read := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "errors.go" {
			continue
		}
		read++
		file, positions := parseSource(t, name)
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			ident, ok := literal.Type.(*ast.Ident)
			if ok && ident.Name == "failure" {
				t.Errorf("%s:%d builds a failure of its own; every failure this surface can produce is declared in errors.go, where the census above can read its code", name, positions.Position(literal.Pos()).Line)
			}
			return true
		})
	}

	// A directory walk that found nothing would make the loop above vacuous, and
	// this package has non-test files whatever names they carry.
	if read == 0 {
		t.Fatal("no source files were read; every check above would pass vacuously")
	}
}

// usageFactsPath is the fragment both ends of this hop implement, relative to
// this package's directory. Its numbers — the page size's bounds and default,
// and the cursor's maximum and minimum length — are declared there and nowhere
// else, and this listener enforces all five of them.
const usageFactsPath = "../../../../../../api/openapi/shared/usage-facts.yaml"

// TestTheFactContractsNumbersAreTheConstants pins this listener's numbers to the
// document that declares them.
//
// Without it the numbers are unfalsifiable from inside this module: every other
// cursor and limit test here derives its expectation from the same constant it
// tests, so `maxCursorLength` could be widened to 4000 and the package would
// stay green while this hop accepted values the façade in front of it refuses —
// the disagreement the constant's own comment exists to prevent. The
// expectation comes from the YAML, so the two hops cannot drift apart without
// one of the two modules going red.
//
// The façade and the outbound adapter each carry their own copy of this pin
// against the same file, because the applications are separate Go modules
// (ADR 0006 §1) and no import exists through which one could be held to the
// other's constants.
func TestTheFactContractsNumbersAreTheConstants(t *testing.T) {
	numbers := scanContract(t, usageFactsPath).numbers

	tests := []struct {
		name     string
		key      string
		constant int
	}{
		{name: "the page size a caller may not go below", key: "components.parameters.UsageEventsLimit.schema.minimum", constant: 1},
		{name: "the page size a caller may not exceed", key: "components.parameters.UsageEventsLimit.schema.maximum", constant: usagefacts.MaxLimit},
		{name: "the page size used when a caller names none", key: "components.parameters.UsageEventsLimit.schema.default", constant: usagefacts.DefaultLimit},
		{name: "the cursor's maximum length", key: "components.schemas.UsageCursor.maxLength", constant: maxCursorLength},
		{name: "the cursor's minimum length, which is why `after=` is refused", key: "components.schemas.UsageCursor.minLength", constant: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared, ok := numbers[tt.key]
			if !ok {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", usageFactsPath, tt.key)
			}
			if declared != tt.constant {
				t.Errorf("%s says %s is %d and this listener enforces %d; the code and the contract have drifted apart", usageFactsPath, tt.key, declared, tt.constant)
			}
		})
	}
}

// projectionFactsPath is the projection fragment both ends of this hop
// implement, relative to this package's directory, alongside the façade
// document that carries the two delivered operations.
//
// Its numbers — the protocol version, the per-array snapshot bound, the batch
// bound and the delivered-body ceiling — are declared there and nowhere else,
// and this listener enforces all of them.
const projectionFactsPath = "../../../../../../api/openapi/shared/projection.yaml"

// TestTheProjectionContractsNumbersAreTheConstants pins this listener's
// projection numbers to the documents that declare them, for the same reason
// its sibling pins the fact feed's: a constant widened here while the document
// stood still is a façade that refuses bodies this listener accepts, and the
// disagreement would be invisible from inside either module alone.
//
// The body ceiling is read out of dataplane.yaml rather than the fragment,
// because the ceiling is declared on the operations and the operations are the
// façade's — the fragment deliberately describes shapes only. Both delivered
// operations carry it, and both are pinned: a ceiling raised on one operation
// and not the other is exactly the kind of edit this test exists to make red.
func TestTheProjectionContractsNumbersAreTheConstants(t *testing.T) {
	facade := scanContract(t, contractPath).numbers
	fragment := scanContract(t, projectionFactsPath).numbers

	tests := []struct {
		name     string
		document map[string]int
		key      string
		constant int
	}{
		{name: "the protocol version this plane speaks", document: fragment, key: "components.schemas.ProjectionProtocolVersion.const", constant: projection.ProtocolVersion},
		{name: "the api keys a snapshot may carry", document: fragment, key: "components.schemas.ProjectionSnapshot.properties.api_keys.maxItems", constant: projection.MaxSnapshotRecords},
		{name: "the accounts a snapshot may carry", document: fragment, key: "components.schemas.ProjectionSnapshot.properties.accounts.maxItems", constant: projection.MaxSnapshotRecords},
		{name: "the changes one delivery may carry", document: fragment, key: "components.schemas.ProjectionChangesBatch.properties.changes.maxItems", constant: projection.MaxChangesPerBatch},
		{name: "the snapshot body's ceiling in bytes", document: facade, key: "paths./internal/projection/snapshot.post.x-max-body-bytes", constant: maxProjectionBodyBytes},
		{name: "the batch body's ceiling in bytes", document: facade, key: "paths./internal/projection/changes.post.x-max-body-bytes", constant: maxProjectionBodyBytes},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared, ok := tt.document[tt.key]
			if !ok {
				t.Fatalf("the document declares no %s; this pin proves nothing until the scan finds it", tt.key)
			}
			if declared != tt.constant {
				t.Errorf("the contract says %s is %d and this listener enforces %d; the code and the contract have drifted apart", tt.key, declared, tt.constant)
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
// A scanner rather than a parser, for the reason the path scan above gives:
// indentation-nested keys are the whole shape being read, the documents are
// written by hand, and a scan that stopped matching yields nothing — which
// every assertion built on it reports as a failure rather than skipping, since
// each one refuses an empty set. Sequence entries and folded description text
// fall out on their own: a line is only read as a key when it reads exactly as
// `indent key: value` with no whitespace inside the key, and prose lines
// containing a colon have spaces before it.
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

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
