package management

import (
	"context"
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
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
// break.
func serve(t *testing.T, facts *stubFacts, request *stdhttp.Request) *httptest.ResponseRecorder {
	t.Helper()
	handler := New(application.New("v0.1.0", facts), serviceCredential)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	return rec
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
	// The cursor is opaque: this surface is where it was minted, and even here
	// the handler does not look inside it. A value with spaces and slashes in it
	// is exactly the kind of string a format-aware implementation would mangle.
	const cursor = "opaque position 42/not-a-number"

	facts := &stubFacts{}
	serve(t, facts, authed(t, "/internal/usage-events?after="+strings.ReplaceAll(cursor, " ", "%20")))

	if facts.after != cursor {
		t.Errorf("the port was read after %q, want %q unchanged", facts.after, cursor)
	}
}

func TestTheFactFeedAppliesThePageSizeBounds(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantLimit int
	}{
		{name: "an omitted limit is served at the port's default", query: "", wantLimit: usagefacts.DefaultLimit},
		{name: "a limit within the bounds is passed on", query: "?limit=250", wantLimit: 250},
		{name: "a limit above the maximum is served at the maximum", query: "?limit=1000000", wantLimit: usagefacts.MaxLimit},
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
		{name: "a limit supplied twice", query: "?limit=10&limit=20"},
		{name: "a cursor supplied twice", query: "?after=one&after=two"},
		{name: "a cursor past the declared length", query: "?after=" + strings.Repeat("c", maxCursorLength+1)},
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
