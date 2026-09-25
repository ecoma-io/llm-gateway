package management

import (
	"context"
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/usagefacts"
)

// stubFacts is the fact reader these tests drive the surface with. It records
// what the application passed down, because half of what this surface does is
// decide what reaches the port and the assertions need to see that decision
// rather than only its result.
type stubFacts struct {
	page  usagefacts.Page
	err   error
	after string
	limit int
	calls int
}

// Read implements usagefacts.Reader.
func (stub *stubFacts) Read(_ context.Context, after string, limit int) (usagefacts.Page, error) {
	stub.calls++
	stub.after = after
	stub.limit = limit
	return stub.page, stub.err
}

// stubProjection is the applier these tests drive the surface with. It
// records every snapshot and batch the handler handed over, because what the
// tests assert about the projection operations is mostly the handover — that
// a refused body never reaches the port, that a batch's entries arrive parsed
// rather than as bytes — and the answer it gives is one knob, `err`, plus the
// acknowledgement and position each test wants the surface to report.
type stubProjection struct {
	position    projection.Position
	positionErr error
	ack         uint64
	err         error

	snapshots []projection.Snapshot
	batches   []projection.Batch
}

// Position implements persistence.ProjectionApplier.
func (stub *stubProjection) Position(_ context.Context) (projection.Position, error) {
	return stub.position, stub.positionErr
}

// ApplySnapshot implements persistence.ProjectionApplier.
func (stub *stubProjection) ApplySnapshot(_ context.Context, snapshot projection.Snapshot, _ time.Time) (uint64, error) {
	stub.snapshots = append(stub.snapshots, snapshot)
	return stub.ack, stub.err
}

// ApplyChanges implements persistence.ProjectionApplier.
func (stub *stubProjection) ApplyChanges(_ context.Context, batch projection.Batch) (uint64, error) {
	stub.batches = append(stub.batches, batch)
	return stub.ack, stub.err
}

// The credential these tests configure and present. It is one value so that a
// test which presents the wrong one has to say so in its own case.
const (
	serviceCredential = "a-service-credential-for-tests"
	otherCredential   = "a-different-credential"
)

// serve drives one request through the whole handler chain — middleware,
// routing, authentication, handler — because that chain is what the surface is.
// Testing the handler directly would leave the guard, the envelope and the
// request ID unexercised, and those are the parts a change is most likely to
// break. The catalog sits over the empty stub world in groupversions_test.go
// and the projection applier is an empty stub: the fact-feed and catalog tests
// deliver nothing, and the projection tests build their own surface through
// serveProjection below.
func serve(t *testing.T, facts *stubFacts, request *stdhttp.Request) *httptest.ResponseRecorder {
	t.Helper()
	return serveApp(t, application.New("v0.1.0", facts, newStubCatalog(&stubVersions{}), &stubProjection{}), request)
}

// serveApp is serve with the application made explicit, for the tests that
// need to name the stub one of the ports answers through.
func serveApp(t *testing.T, app *application.App, request *stdhttp.Request) *httptest.ResponseRecorder {
	t.Helper()
	handler := New(app, serviceCredential)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	return rec
}

// serveProjection drives one request through a surface whose projection
// applier is the given stub, so a projection test controls both what the
// operations read and what their applies answer.
func serveProjection(t *testing.T, applier *stubProjection, request *stdhttp.Request) *httptest.ResponseRecorder {
	t.Helper()
	return serveApp(t, application.New("v0.1.0", &stubFacts{}, newStubCatalog(&stubVersions{}), applier), request)
}

// authed builds a GET for the fact feed carrying the configured credential.
func authed(t *testing.T, target string) *stdhttp.Request {
	t.Helper()
	request := httptest.NewRequest(stdhttp.MethodGet, target, nil)
	request.Header.Set(authorizationHeader, credentialScheme+" "+serviceCredential)
	return request
}

func TestTheFactFeedReturnsThePageThePortGave(t *testing.T) {
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

	// The body is pinned as bytes rather than decoded and inspected: this
	// surface is a contract, and a test that decoded the response would pass
	// just as happily against a field renamed to something no consumer expects.
	want := `{"events":[{"request_id":"req_1","kind":"settled","schema_version":1,` +
		`"occurred_at":"2026-09-24T10:00:00Z","payload":{"tokens":7}}],` +
		`"next_cursor":"position-7","has_more":true}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get(RequestIDHeader); got == "" {
		t.Errorf("%s is missing; every response carries one", RequestIDHeader)
	}
	if rec.Code != stdhttp.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, stdhttp.StatusOK)
	}
}

func TestTheFactFeedPassesTheCursorThroughUntouched(t *testing.T) {
	tests := []struct {
		name   string
		cursor string
	}{
		{
			// A value with spaces and slashes in it is exactly the kind of string
			// a format-aware implementation would mangle.
			name:   "a cursor with characters a format-aware implementation would escape",
			cursor: "opaque position 42/not-a-number",
		},
		{
			// The bound is a count of characters, and the schema says so: a
			// cursor of exactly maxCursorLength code points is one this feed is
			// entitled to issue, whatever it costs in bytes. Counting bytes here
			// would refuse it — and the refusal would be invisible for the ASCII
			// cursor above, which is why this row exists.
			name:   "a cursor of the declared length in characters and more than that in bytes",
			cursor: strings.Repeat("é", maxCursorLength),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := &stubFacts{}
			serve(t, facts, authed(t, "/internal/usage-events?after="+url.QueryEscape(tt.cursor)))

			if facts.after != tt.cursor {
				t.Errorf("the port was read after %q, want %q unchanged", facts.after, tt.cursor)
			}
		})
	}
}

// TestTheFactFeedAppliesThePageSizeBounds pins the accepting half of the
// contract's `limit` rule (shared/usage-facts.yaml): an omitted parameter is the
// declared default, and every value between the minimum and the maximum travels
// to the port exactly as the caller wrote it. The refusing half is in the table
// below, and the two together are the whole rule — a page size is either served
// as asked or refused, never adjusted.
func TestTheFactFeedAppliesThePageSizeBounds(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantLimit int
	}{
		{name: "an omitted limit is served at the port's default", query: "", wantLimit: usagefacts.DefaultLimit},
		{name: "the smallest limit the contract allows is passed on", query: "?limit=1", wantLimit: 1},
		{name: "a limit within the bounds is passed on", query: "?limit=250", wantLimit: 250},
		{name: "the largest limit the contract allows is passed on", query: "?limit=" + strconv.Itoa(usagefacts.MaxLimit), wantLimit: usagefacts.MaxLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := &stubFacts{}
			serve(t, facts, authed(t, "/internal/usage-events"+tt.query))

			if facts.limit != tt.wantLimit {
				t.Errorf("the port was read with limit %d, want %d", facts.limit, tt.wantLimit)
			}
		})
	}
}

func TestMalformedQueryParametersAreRefused(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "a limit that is not a number", query: "?limit=lots"},
		{name: "a limit below the declared minimum", query: "?limit=0"},
		{name: "a negative limit", query: "?limit=-1"},
		{name: "a limit above the declared maximum", query: "?limit=" + strconv.Itoa(usagefacts.MaxLimit+1)},
		{name: "a limit far above the declared maximum", query: "?limit=1000000"},
		{name: "a limit larger than an integer", query: "?limit=99999999999999999999"},
		{name: "a limit supplied twice", query: "?limit=10&limit=20"},
		{name: "a cursor supplied twice", query: "?after=one&after=two"},
		{name: "a cursor past the declared length", query: "?after=" + strings.Repeat("c", maxCursorLength+1)},
		{
			// The length is counted in characters and this value is over it
			// whichever way you count — but it is the row that fails if the
			// count becomes bytes in the other direction: two bytes per code
			// point here meant a 256-character cursor would have been refused
			// as if it were 512.
			name:  "a cursor past the declared length in characters",
			query: "?after=" + url.QueryEscape(strings.Repeat("é", maxCursorLength+1)),
		},
		{
			// Present and empty is the one value refused for what it would
			// otherwise be read as. The protocol defines the beginning by the
			// parameter's absence, so `after=` is a caller with a broken
			// position, not a caller asking for everything retained.
			name:  "a cursor supplied empty",
			query: "?after=",
		},
		{name: "a parameter this operation does not declare", query: "?position=abc"},
		{name: "a parameter the caller capitalised differently", query: "?After=abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := &stubFacts{}
			rec := serve(t, facts, authed(t, "/internal/usage-events"+tt.query))

			if rec.Code != stdhttp.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, stdhttp.StatusBadRequest)
			}
			if got := envelopeCode(t, rec); got != codeInvalidRequest {
				t.Errorf("error code = %q, want %q", got, codeInvalidRequest)
			}
			// The request never reached the source. A parameter this surface
			// refused must not have been half-applied on its way past.
			if facts.calls != 0 {
				t.Errorf("the port was read %d times; a refused request must not reach it", facts.calls)
			}
		})
	}
}

func TestTheFactFeedMapsTheSourcesFailures(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name:        "an unreplayable cursor is a decision, not a retry",
			err:         usagefacts.ErrCursorExpired,
			wantStatus:  stdhttp.StatusGone,
			wantCode:    codeCursorExpired,
			wantMessage: "the requested position is no longer replayable",
		},
		{
			name:        "an unavailable source is this process's own failure",
			err:         usagefacts.ErrSourceUnavailable,
			wantStatus:  stdhttp.StatusInternalServerError,
			wantCode:    codeInternal,
			wantMessage: internalErrorMessage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, &stubFacts{err: tt.err}, authed(t, "/internal/usage-events"))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := envelopeCode(t, rec); got != tt.wantCode {
				t.Errorf("error code = %q, want %q", got, tt.wantCode)
			}
			if got := envelopeMessage(t, rec); got != tt.wantMessage {
				t.Errorf("error message = %q, want %q", got, tt.wantMessage)
			}
			// The cause is the port's error text, and it is logged, never
			// returned. A management caller can act on a status and a request
			// ID; it cannot act on a store's internal wording.
			if strings.Contains(rec.Body.String(), tt.err.Error()) {
				t.Errorf("body = %s, leaks the source's own error text", rec.Body.String())
			}
		})
	}
}

func TestAPageWithNoPositionIsRefused(t *testing.T) {
	// A page the protocol cannot use. A consumer that stored an empty cursor
	// would ask from the beginning of retained history on every cycle and never
	// advance, re-reading the same facts forever; refusing here turns a source
	// that cannot count into a loud failure at the surface that promised a
	// position.
	rec := serve(t, &stubFacts{page: usagefacts.Page{NextCursor: ""}}, authed(t, "/internal/usage-events"))

	if rec.Code != stdhttp.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, stdhttp.StatusInternalServerError)
	}
	if got := envelopeCode(t, rec); got != codeInternal {
		t.Errorf("error code = %q, want %q", got, codeInternal)
	}
}

func TestAnEmptyPageSerializesAsAnEmptyArray(t *testing.T) {
	// `events` is typed as an array and required, so `null` would be a response
	// no consumer could parse against the document it was written from — and
	// every consumer would write the same nil check to discover that.
	rec := serve(t, &stubFacts{page: usagefacts.Page{NextCursor: "position-0"}}, authed(t, "/internal/usage-events"))

	want := `{"events":[],"next_cursor":"position-0","has_more":false}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestTheFeedWritesAmpersandsAndAngleBracketsLiterally(t *testing.T) {
	// The cursor and the payload are the two values the feed's consumers treat
	// as opaque bytes, and neither may be HTML-escaped on the wire: the cursor
	// is decoded back by dataplane-api and re-emitted, and the chain test at
	// the composition root pins the whole page byte for byte. `&` and `<` are
	// the two runes encoding/json would escape by default, and a payload that
	// contains them — a reason string, a provider's body — is exactly the kind
	// of content this surface will be the head of.
	facts := &stubFacts{page: usagefacts.Page{
		Events: []usagefacts.Event{{
			RequestID:     "req_amp",
			Kind:          "settled",
			SchemaVersion: 1,
			OccurredAt:    time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
			Payload:       json.RawMessage(`{"reason":"a<b & c"}`),
		}},
		NextCursor: "cur:9f2&=<",
		HasMore:    true,
	}}

	rec := serve(t, facts, authed(t, "/internal/usage-events"))

	want := `{"events":[{"request_id":"req_amp","kind":"settled","schema_version":1,` +
		`"occurred_at":"2026-09-24T10:00:00Z","payload":{"reason":"a<b & c"}}],` +
		`"next_cursor":"cur:9f2&=<","has_more":true}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %s\nwant %s\nthe write must not HTML-escape the cursor or the payload", got, want)
	}
}

func TestAFactWithNoBodySerializesAsAnEmptyObject(t *testing.T) {
	facts := &stubFacts{page: usagefacts.Page{
		Events:     []usagefacts.Event{{RequestID: "req_2", Kind: "released", SchemaVersion: 1}},
		NextCursor: "position-8",
	}}

	rec := serve(t, facts, authed(t, "/internal/usage-events"))

	want := `"payload":{}`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body = %s, want it to contain %s", rec.Body.String(), want)
	}
}

func TestAFactWithALiteralNullBodySerializesAsAnEmptyObject(t *testing.T) {
	// The second spelling of no body (#43). A fact source that surfaces a
	// JSON null column hands back the four bytes `null`, and a guard that
	// checked only emptiness would put them on the wire as `"payload":null` —
	// a page the contract does not describe, and one the façade in front of
	// this listener answers with 502 on every retry of the position rather
	// than serve. The bytes are not nothing, so no length check can catch
	// them; only their spelling can.
	facts := &stubFacts{page: usagefacts.Page{
		Events: []usagefacts.Event{{
			RequestID:     "req_3",
			Kind:          "released",
			SchemaVersion: 1,
			Payload:       json.RawMessage(`null`),
		}},
		NextCursor: "position-9",
	}}

	rec := serve(t, facts, authed(t, "/internal/usage-events"))

	want := `"payload":{}`
	if got := rec.Body.String(); !strings.Contains(got, want) {
		t.Errorf("body = %s, want it to contain %s; the second spelling of no body must reach the wire as the first one does", got, want)
	}
}

// TestTheFeedNormalisesOnlyTheTwoSpellingsOfAnAbsentBody pins the boundary of
// the substitution byte for byte, at the one level where all of it can be said
// honestly. Two of the four rows cannot be driven through the handler above:
// the empty slice is the test before this one's subject, and the JSON string
// `"null"` is a value and not an absence — served through the handler it would
// pin a page the contract forbids, asserting the violation a guard exists to
// prevent. What only this table states is the charter itself: absence, in
// either of its two spellings, becomes `{}`, and everything else leaves as the
// bytes it arrived as, because this surface is a reader of facts and not their
// interpreter — the object shape behind a body is the store's CHECK on the way
// in and the fact source's own decoding on the way out, not a judgement this
// guard repeats.
func TestTheFeedNormalisesOnlyTheTwoSpellingsOfAnAbsentBody(t *testing.T) {
	tests := []struct {
		name    string
		payload json.RawMessage
		want    string
	}{
		{
			// The spelling of a column that never had bytes: an empty slice,
			// which no length can distinguish from any other emptiness.
			name:    "no bytes at all",
			payload: json.RawMessage(nil),
			want:    `{}`,
		},
		{
			name:    "the four bytes of a literal null",
			payload: json.RawMessage(`null`),
			want:    `{}`,
		},
		{
			// Four bytes wrapped in quotes: a JSON string whose content
			// happens to spell null. It is a value, and the guard that
			// judges values is a different guard in a different place.
			name:    "the json string null, which is a value and not an absence",
			payload: json.RawMessage(`"null"`),
			want:    `"null"`,
		},
		{
			name:    "a body, whatever is in it",
			payload: json.RawMessage(`{"tokens":7}`),
			want:    `{"tokens":7}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(payloadOrEmpty(tt.payload)); got != tt.want {
				t.Errorf("payloadOrEmpty(%s) = %s, want %s; absence is normalised and nothing else is", tt.payload, got, tt.want)
			}
		})
	}
}

// envelopeCode and envelopeMessage decode just enough of the management
// envelope to assert on a field. They are deliberately not used for the success
// path, whose bytes are pinned exactly.
func envelopeCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return envelopeField(t, rec, "code")
}

func envelopeMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return envelopeField(t, rec, "message")
}

func envelopeField(t *testing.T, rec *httptest.ResponseRecorder, field string) string {
	t.Helper()
	var envelope struct {
		Error map[string]string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding the error envelope from %s: %v", rec.Body.String(), err)
	}
	return envelope.Error[field]
}
