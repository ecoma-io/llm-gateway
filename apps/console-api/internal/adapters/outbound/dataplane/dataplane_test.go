package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	port "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
)

// The tests drive the adapter through an injected *http.Client whose transport
// is a function, so every request is an in-process value and every response a
// literal: no socket, no server and no test double of the Data Plane. What is
// asserted is the adapter's whole job — the request it builds and the page it
// makes of the answer.

const (
	// baseURL carries a path prefix on purpose: a deployment behind a gateway
	// serves the management surface under one, and an adapter that dropped it
	// would send every read to a path the deployment does not serve.
	baseURL    = "https://dataplane.example.test:8443/management"
	credential = "service-credential-not-for-logs"

	// anOpaqueCursor is deliberately hostile. It contains separator characters,
	// an escape, spaces and something that looks like a query parameter of its
	// own — everything a cursor would need to escape in order to be sent
	// byte-for-byte, and the shape an adapter that concatenated strings would
	// get wrong.
	anOpaqueCursor = "  not-a-number &limit=9999 =%2F +  spaces"
)

// roundTripperFunc is a transport that is a function: the smallest injection
// point that lets a test answer a request without a socket.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

// jsonResponse builds the response a non-200 status would carry; only the
// status and the body matter to the adapter, which never inspects an error
// envelope.
func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// capturingClient returns a client that records the request it was handed and
// answers with response.
func capturingClient(seen **http.Request, response *http.Response) *http.Client {
	return &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		*seen = request
		return response, nil
	})}
}

// TestReadUsageEventsSendsTheCursorVerbatim is the opacity rule on the wire
// side: the position is escaped into the query and arrives at the peer
// unchanged, and its contents cannot become parameters of their own. The limit
// is asserted beside it because a cursor carrying `&limit=9999` is exactly what
// a hand-built query string would have let through.
func TestReadUsageEventsSendsTheCursorVerbatim(t *testing.T) {
	var seen *http.Request
	client := New(
		capturingClient(&seen, jsonResponse(http.StatusOK, `{"events":[],"next_cursor":"c1","has_more":false}`)),
		baseURL,
		credential,
	)

	if _, err := client.ReadUsageEvents(context.Background(), anOpaqueCursor, 100); err != nil {
		t.Fatalf("ReadUsageEvents() error = %v, want nil", err)
	}
	if seen == nil {
		t.Fatal("the transport was never called")
	}

	query := seen.URL.Query()
	if got := query.Get("after"); got != anOpaqueCursor {
		t.Errorf("after = %q, want %q byte for byte", got, anOpaqueCursor)
	}
	if got, want := query.Get("limit"), "100"; got != want {
		t.Errorf("limit = %q, want %q — the cursor's contents must not become query parameters", got, want)
	}
	if got, want := strings.Count(seen.URL.RawQuery, "limit="), 1; got != want {
		t.Errorf("the raw query %q mentions limit %d time(s), want %d", seen.URL.RawQuery, got, want)
	}
	if got, want := seen.URL.Path, "/management/internal/usage-events"; got != want {
		t.Errorf("path = %q, want %q — the base URL's prefix is kept", got, want)
	}
	if got, want := seen.Header.Get("Authorization"), "Bearer "+credential; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got, want := seen.Method, http.MethodGet; got != want {
		t.Errorf("method = %s, want %s", got, want)
	}
}

// TestReadUsageEventsOmitsAnEmptyPosition is the one thing the adapter decides
// about a cursor: a consumer that has never applied anything has the empty
// string for a position, and the contract spells "from the beginning of what is
// retained" as an absent parameter. Sending `after=` empty would be a value the
// schema refuses, and defaulting it to anything else would be inventing a
// position.
func TestReadUsageEventsOmitsAnEmptyPosition(t *testing.T) {
	var seen *http.Request
	client := New(
		capturingClient(&seen, jsonResponse(http.StatusOK, `{"events":[],"next_cursor":"c1","has_more":false}`)),
		baseURL,
		credential,
	)

	if _, err := client.ReadUsageEvents(context.Background(), "", 50); err != nil {
		t.Fatalf("ReadUsageEvents() error = %v, want nil", err)
	}
	if _, present := seen.URL.Query()["after"]; present {
		t.Errorf("the request carried after=%q, want no after at all", seen.URL.Query().Get("after"))
	}
	if got, want := seen.URL.Query().Get("limit"), "50"; got != want {
		t.Errorf("limit = %q, want %q", got, want)
	}
}

// TestReadUsageEventsDecodesAPage pins the mapping from the contract's field
// names onto the port's Page: snake_case on the wire, Go names in the port, and
// has_more carried through as the caller's stopping condition.
func TestReadUsageEventsDecodesAPage(t *testing.T) {
	body := `{
		"events": [
			{"append_seq":41,"request_id":"req-1","kind":"settled","schema_version":3,"occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}
		],
		"next_cursor":"opaque-cursor-2",
		"has_more":true
	}`

	var seen *http.Request
	client := New(capturingClient(&seen, jsonResponse(http.StatusOK, body)), baseURL, credential)

	page, err := client.ReadUsageEvents(context.Background(), "", 100)
	if err != nil {
		t.Fatalf("ReadUsageEvents() error = %v, want nil", err)
	}

	if got, want := len(page.Events), 1; got != want {
		t.Fatalf("len(Events) = %d, want %d", got, want)
	}
	event := page.Events[0]
	if got, want := event.AppendSeq, int64(41); got != want {
		t.Errorf("AppendSeq = %d, want %d — the fact's place in the feed crosses as its own field", got, want)
	}
	if got, want := event.RequestID, "req-1"; got != want {
		t.Errorf("RequestID = %q, want %q", got, want)
	}
	if got, want := event.Kind, "settled"; got != want {
		t.Errorf("Kind = %q, want %q", got, want)
	}
	if got, want := event.SchemaVersion, 3; got != want {
		t.Errorf("SchemaVersion = %d, want %d", got, want)
	}
	if want := time.Date(2026, 9, 24, 10, 11, 12, 0, time.UTC); !event.OccurredAt.Equal(want) {
		t.Errorf("OccurredAt = %s, want %s", event.OccurredAt, want)
	}
	if got, want := string(event.Payload), `{"amount":"4200"}`; got != want {
		t.Errorf("Payload = %s, want %s", got, want)
	}
	if got, want := page.NextCursor, "opaque-cursor-2"; got != want {
		t.Errorf("NextCursor = %q, want %q byte for byte", got, want)
	}
	if !page.HasMore {
		t.Error("HasMore = false, want true")
	}

	empty := New(
		capturingClient(&seen, jsonResponse(http.StatusOK, `{"events":[],"next_cursor":"c1","has_more":false}`)),
		baseURL,
		credential,
	)
	drained, err := empty.ReadUsageEvents(context.Background(), "c1", 100)
	if err != nil {
		t.Fatalf("ReadUsageEvents() error = %v, want nil", err)
	}
	if drained.HasMore {
		t.Error("HasMore = true for a page that said the feed was drained, want false")
	}
	if len(drained.Events) != 0 {
		t.Errorf("len(Events) = %d on an empty page, want 0", len(drained.Events))
	}
}

// TestReadUsageEventsDecodesAFullyPricedSettlement is the wire hop a real
// settlement takes: every typed figure the producer writes non-null crosses
// the seam non-null and equal — values AND null-fidelity together, the two
// things a decode can lose. The all-null pages the neighbouring tests decode
// pin the absence side; this pins the presence side, so a field that started
// decoding to its zero value would fail here rather than settle for nothing.
func TestReadUsageEventsDecodesAFullyPricedSettlement(t *testing.T) {
	body := `{
		"events": [
			{
				"append_seq": 87,
				"request_id": "req-priced",
				"kind": "settled",
				"schema_version": 3,
				"occurred_at": "2026-09-25T08:09:10Z",
				"payload": {"allocations":[{"funding_bucket_id":"f0e0c8a0-0000-4000-8000-000000000001","amount":21,"ordinal":1}]},
				"capture_method": "reservation_floor",
				"committed_attempt_id": "att-priced",
				"provider_input_tokens": 511,
				"provider_output_tokens": 137,
				"delivery_tokens": 12,
				"price_revision_id": "rev-priced-9",
				"input_unit_price": 1000000,
				"output_unit_price": 2000000,
				"settled_amount": 21
			}
		],
		"next_cursor": "opaque-cursor-priced",
		"has_more": false
	}`

	var seen *http.Request
	client := New(capturingClient(&seen, jsonResponse(http.StatusOK, body)), baseURL, credential)

	page, err := client.ReadUsageEvents(context.Background(), "", 100)
	if err != nil {
		t.Fatalf("ReadUsageEvents() error = %v, want nil", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("len(Events) = %d, want 1", len(page.Events))
	}
	event := page.Events[0]

	if event.CaptureMethod == nil || *event.CaptureMethod != "reservation_floor" {
		t.Errorf("CaptureMethod = %v, want reservation_floor", event.CaptureMethod)
	}
	if event.CommittedAttemptID == nil || *event.CommittedAttemptID != "att-priced" {
		t.Errorf("CommittedAttemptID = %v, want att-priced", event.CommittedAttemptID)
	}
	if event.ProviderInputTokens == nil || *event.ProviderInputTokens != 511 {
		t.Errorf("ProviderInputTokens = %v, want 511 — a priced figure crosses as the number it was", event.ProviderInputTokens)
	}
	if event.ProviderOutputTokens == nil || *event.ProviderOutputTokens != 137 {
		t.Errorf("ProviderOutputTokens = %v, want 137", event.ProviderOutputTokens)
	}
	if event.DeliveryTokens == nil || *event.DeliveryTokens != 12 {
		t.Errorf("DeliveryTokens = %v, want 12", event.DeliveryTokens)
	}
	if event.PriceRevision == nil || *event.PriceRevision != "rev-priced-9" {
		t.Errorf("PriceRevision = %v, want rev-priced-9", event.PriceRevision)
	}
	if event.InputUnitPrice == nil || *event.InputUnitPrice != 1_000_000 {
		t.Errorf("InputUnitPrice = %v, want 1000000 — per-million prices keep their magnitude on the wire", event.InputUnitPrice)
	}
	if event.OutputUnitPrice == nil || *event.OutputUnitPrice != 2_000_000 {
		t.Errorf("OutputUnitPrice = %v, want 2000000", event.OutputUnitPrice)
	}
	if event.SettledAmount == nil || *event.SettledAmount != 21 {
		t.Errorf("SettledAmount = %v, want 21", event.SettledAmount)
	}
	if event.CorrectsAppendSeq != nil {
		t.Errorf("CorrectsAppendSeq = %v, want nil — the correction path writes nothing in this build", event.CorrectsAppendSeq)
	}
}

// TestReadUsageEventsRefusesAPageItCannotAdvanceFrom is the consumer boundary
// the port's Page contract opens with: a page's position is the one value in
// this flow that becomes durable state in another database, and a response that
// is not shaped like a page must fail the read rather than be handed on.
//
// Both refusals are statements about the envelope — is the field there, and can
// the value be a cursor at all — and neither is a reading of the value. The
// test is written so that a future adapter that started parsing cursors would
// have to delete cases to keep passing.
func TestReadUsageEventsRefusesAPageItCannotAdvanceFrom(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "a page whose position is an empty string",
			body: `{"events":[],"next_cursor":"","has_more":false}`,
		},
		{
			name: "a page that carries no position field at all",
			body: `{"events":[],"has_more":false}`,
		},
		{
			name: "a page whose position is null",
			body: `{"events":[],"next_cursor":null,"has_more":false}`,
		},
		{
			name: "a page whose position is longer than a cursor can be",
			body: `{"events":[],"next_cursor":"` + strings.Repeat("c", 513) + `","has_more":false}`,
		},
		{
			// The refusing half of the character-versus-byte pair: 513 code
			// points, and so over the bound however it is counted. It is here so
			// that the accepted row above cannot be satisfied by a bound that
			// stopped refusing anything.
			name: "a page whose position is past the bound in characters",
			body: `{"events":[],"next_cursor":"` + strings.Repeat("é", 513) + `","has_more":false}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen *http.Request
			client := New(capturingClient(&seen, jsonResponse(http.StatusOK, tt.body)), baseURL, credential)

			page, err := client.ReadUsageEvents(context.Background(), "", 100)
			if !errors.Is(err, port.ErrMalformedPage) {
				t.Fatalf("ReadUsageEvents() error = %v, want it to wrap %v", err, port.ErrMalformedPage)
			}
			if page.NextCursor != "" || page.Events != nil || page.HasMore {
				t.Errorf("ReadUsageEvents() returned %+v beside an error, want the zero Page — a caller that ignored the error must not find a position in it", page)
			}
			// The refusal is made of the response and not of the request, so it
			// must not smuggle back the endpoint or the credential either.
			for _, secret := range []string{baseURL, credential} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q carries %q", err.Error(), secret)
				}
			}
		})
	}
}

// TestReadUsageEventsRefusesAPageMissingRequiredFields is the fail-closed
// table: every field the contract marks required — the page's three, the
// fact's six, append_seq first because a fact without its place in the feed
// cannot be ordered against the position it advances — is refused when the
// body omits it or carries it as null, because a page that must be filled in
// is not a page the contract describes.
//
// The danger each row stands against is the zero value: `encoding/json` used
// to answer a missing `has_more` with false, an absent `events` with an empty
// page, and a fact missing any field with the zero Event, and a page like that
// is one `Replay` away from advancing a durable position across facts nobody
// saw. Refused here, it never becomes a Page at all.
//
// One row per class, and each row's body differs from a valid page by the
// single field under test, so a green run cannot come from some other refusal
// firing instead.
func TestReadUsageEventsRefusesAPageMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "a page with no events field at all",
			body: `{"next_cursor":"cursor-2","has_more":false}`,
		},
		{
			name: "a page whose events field is null",
			body: `{"events":null,"next_cursor":"cursor-2","has_more":false}`,
		},
		{
			name: "a page with no next_cursor field at all",
			body: `{"events":[],"has_more":false}`,
		},
		{
			name: "a page whose next_cursor field is null",
			body: `{"events":[],"next_cursor":null,"has_more":false}`,
		},
		{
			name: "a page with no has_more field at all",
			body: `{"events":[],"next_cursor":"cursor-2"}`,
		},
		{
			name: "a page whose has_more field is null",
			body: `{"events":[],"next_cursor":"cursor-2","has_more":null}`,
		},
		{
			name: "a fact with no append_seq field at all",
			body: `{"events":[{"request_id":"req-1","kind":"settled","schema_version":1,"occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "a fact whose append_seq field is null",
			body: `{"events":[{"append_seq":null,"request_id":"req-1","kind":"settled","schema_version":1,"occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "a fact with no request_id field at all",
			body: `{"events":[{"append_seq":1,"kind":"settled","schema_version":1,"occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "a fact whose request_id field is null",
			body: `{"events":[{"append_seq":1,"request_id":null,"kind":"settled","schema_version":1,"occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "a fact with no kind field at all",
			body: `{"events":[{"append_seq":1,"request_id":"req-1","schema_version":1,"occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "a fact whose kind field is null",
			body: `{"events":[{"append_seq":1,"request_id":"req-1","kind":null,"schema_version":1,"occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "a fact with no schema_version field at all",
			body: `{"events":[{"append_seq":1,"request_id":"req-1","kind":"settled","occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "a fact whose schema_version field is null",
			body: `{"events":[{"append_seq":1,"request_id":"req-1","kind":"settled","schema_version":null,"occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "a fact with no occurred_at field at all",
			body: `{"events":[{"append_seq":1,"request_id":"req-1","kind":"settled","schema_version":1,"payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "a fact whose occurred_at field is null",
			body: `{"events":[{"append_seq":1,"request_id":"req-1","kind":"settled","schema_version":1,"occurred_at":null,"payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "a fact with no payload field at all",
			body: `{"events":[{"append_seq":1,"request_id":"req-1","kind":"settled","schema_version":1,"occurred_at":"2026-09-24T10:11:12Z"}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "a fact whose payload field is null",
			body: `{"events":[{"append_seq":1,"request_id":"req-1","kind":"settled","schema_version":1,"occurred_at":"2026-09-24T10:11:12Z","payload":null}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "a fact that is an empty object",
			body: `{"events":[{}],"next_cursor":"cursor-2","has_more":true}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen *http.Request
			client := New(capturingClient(&seen, jsonResponse(http.StatusOK, tt.body)), baseURL, credential)

			page, err := client.ReadUsageEvents(context.Background(), "", 100)
			if !errors.Is(err, port.ErrMalformedPage) {
				t.Fatalf("ReadUsageEvents() error = %v, want it to wrap %v", err, port.ErrMalformedPage)
			}
			if page.NextCursor != "" || page.Events != nil || page.HasMore {
				t.Errorf("ReadUsageEvents() returned %+v beside an error, want the zero Page — a caller that ignored the error must not find a position in it", page)
			}
			// The refusal is made of the response and not of the request, so it
			// must not smuggle back the endpoint or the credential either.
			for _, secret := range []string{baseURL, credential} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q carries %q", err.Error(), secret)
				}
			}
		})
	}
}

// TestReadUsageEventsRefusesABodyItCannotDecode covers the other half of "not
// a page the contract describes": a 200 whose body fails to decode at all —
// not JSON, truncated mid-page, or a required field carrying a value of the
// wrong type. The port's taxonomy has one name for an answer that arrived and
// is wrong, so these classify with the absent-and-null refusals above rather
// than surfacing as a bare decode error, the shape a failure of transport
// would take: an unknown answer is retryable bad luck, a body that lies about
// being a page is a peer breaking the contract.
func TestReadUsageEventsRefusesABodyItCannotDecode(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "a body that is not JSON at all",
			body: `<html>gateway error</html>`,
		},
		{
			name: "a body truncated mid-page",
			body: `{"events":[{"request_id":"req-1"`,
		},
		{
			name: "a has_more of the wrong type",
			body: `{"events":[],"next_cursor":"cursor-2","has_more":"true"}`,
		},
		{
			name: "a schema_version of the wrong type",
			body: `{"events":[{"append_seq":1,"request_id":"req-1","kind":"settled","schema_version":"1","occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "an occurred_at that is not a timestamp",
			body: `{"events":[{"append_seq":1,"request_id":"req-1","kind":"settled","schema_version":1,"occurred_at":"not-a-time","payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`,
		},
		{
			name: "an append_seq below the one the contract allows",
			body: `{"events":[{"append_seq":0,"request_id":"req-1","kind":"settled","schema_version":1,"occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen *http.Request
			client := New(capturingClient(&seen, jsonResponse(http.StatusOK, tt.body)), baseURL, credential)

			page, err := client.ReadUsageEvents(context.Background(), "", 100)
			if !errors.Is(err, port.ErrMalformedPage) {
				t.Fatalf("ReadUsageEvents() error = %v, want it to wrap %v", err, port.ErrMalformedPage)
			}
			if page.NextCursor != "" || page.Events != nil || page.HasMore {
				t.Errorf("ReadUsageEvents() returned %+v beside an error, want the zero Page — a caller that ignored the error must not find a position in it", page)
			}
			// The refusal is made of the response and not of the request, so it
			// must not smuggle back the endpoint or the credential either.
			for _, secret := range []string{baseURL, credential} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q carries %q", err.Error(), secret)
				}
			}
		})
	}
}

// TestAFactWithPresentButEmptyFieldsIsStillAPage is the boundary the table
// above must not erode: presence is the whole check. An empty request_id, a
// kind this build has never heard of and a schema_version of zero are values
// the body carried; deciding which of them can settle is the applier's, and an
// adapter that started refusing them would be taking that decision from it.
func TestAFactWithPresentButEmptyFieldsIsStillAPage(t *testing.T) {
	// append_seq carries the one value its bound admits rather than an empty
	// one: presence AND at least one are its whole check, so a zero is a
	// refusal above, not an emptiness this test is about.
	body := `{"events":[{"append_seq":1,"request_id":"","kind":"a-kind-nobody-knows","schema_version":0,"occurred_at":"0001-01-01T00:00:00Z","payload":[1,2]}],"next_cursor":"cursor-2","has_more":false}`

	var seen *http.Request
	client := New(capturingClient(&seen, jsonResponse(http.StatusOK, body)), baseURL, credential)

	page, err := client.ReadUsageEvents(context.Background(), "", 100)
	if err != nil {
		t.Fatalf("ReadUsageEvents() error = %v, want nil — every required field is present", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("len(Events) = %d, want 1", len(page.Events))
	}
	event := page.Events[0]
	if event.RequestID != "" || event.Kind != "a-kind-nobody-knows" || event.SchemaVersion != 0 {
		t.Errorf("event = %+v, want the values the body carried, empty or not", event)
	}
	if want := `[1,2]`; string(event.Payload) != want {
		t.Errorf("Payload = %s, want %s — a present payload crosses raw whatever it holds", event.Payload, want)
	}
}

// TestAPageMayCarryFieldsTheContractDoesNotName pins the decoder's forward
// compatibility: the Data Plane may add a field to the envelope or to a fact
// before this module ships, and an unfamiliar key is ignored, never refused.
// A consumer that refused one would be a version lock on the plane it reads.
func TestAPageMayCarryFieldsTheContractDoesNotName(t *testing.T) {
	body := `{"events":[{"append_seq":1,"request_id":"req-1","kind":"settled","schema_version":1,"occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"},"trace_id":"t-1"}],"next_cursor":"cursor-2","has_more":false,"published_at":"2026-09-24T10:11:12Z"}`

	var seen *http.Request
	client := New(capturingClient(&seen, jsonResponse(http.StatusOK, body)), baseURL, credential)

	page, err := client.ReadUsageEvents(context.Background(), "", 100)
	if err != nil {
		t.Fatalf("ReadUsageEvents() error = %v, want nil — unknown fields are tolerated", err)
	}
	if got, want := page.NextCursor, "cursor-2"; got != want {
		t.Errorf("NextCursor = %q, want %q", got, want)
	}
	if got, want := len(page.Events), 1; got != want {
		t.Fatalf("len(Events) = %d, want %d", got, want)
	}
}

// TestAnAcceptedPageKeepsItsCursorByteForByte is the other half of the
// boundary, and the half that keeps it from becoming enforcement for its own
// sake: every position the contract admits is still passed through untouched.
//
// The values are chosen to be ones a checking implementation would be tempted
// to normalise — a cursor with leading and trailing whitespace, one at exactly
// the declared maximum, and one holding characters that look like a query
// parameter of its own. None of them may be trimmed, rewritten or re-encoded.
func TestAnAcceptedPageKeepsItsCursorByteForByte(t *testing.T) {
	tests := []struct {
		name   string
		cursor string
	}{
		{name: "an opaque cursor in the shapes the Data Plane might issue", cursor: anOpaqueCursor},
		{name: "a cursor padded with whitespace on both ends", cursor: "  cur-2  "},
		{name: "a cursor exactly as long as the contract allows", cursor: strings.Repeat("c", 512)},
		// The same declared length in code points and twice that in bytes. The
		// schema's `maxLength` counts characters, and this is the row that fails
		// if this adapter counts them with `len()`: it would refuse a page the
		// contract admits, and refuse it as a malformed page — turning a value
		// the Control Plane is entitled to store into a permanent ingestion stop.
		{name: "a cursor of the declared length in characters and more in bytes", cursor: strings.Repeat("é", 512)},
		{name: "a single character", cursor: "x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"events":      []any{},
				"next_cursor": tt.cursor,
				"has_more":    false,
			})
			if err != nil {
				t.Fatalf("building the response body: %v", err)
			}

			var seen *http.Request
			client := New(capturingClient(&seen, jsonResponse(http.StatusOK, string(body))), baseURL, credential)

			page, err := client.ReadUsageEvents(context.Background(), tt.cursor, 100)
			if err != nil {
				t.Fatalf("ReadUsageEvents() error = %v, want nil", err)
			}
			if page.NextCursor != tt.cursor {
				t.Errorf("NextCursor = %q, want %q byte for byte", page.NextCursor, tt.cursor)
			}
			if got := seen.URL.Query().Get("after"); got != tt.cursor {
				t.Errorf("after = %q, want %q byte for byte", got, tt.cursor)
			}
		})
	}
}

// TestReadUsageEventsMapsAnExpiredCursor is the one status the port refines. A
// 410 is the Control Plane's cue that its stored position is unusable — it must
// re-establish it rather than retry — and the test matches the sentinel with
// errors.Is, which is the only way a caller is told.
func TestReadUsageEventsMapsAnExpiredCursor(t *testing.T) {
	client := New(
		&http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusGone, `{"error":{"code":"cursor_expired","message":"gone"},"request_id":"r-1"}`), nil
		})},
		baseURL,
		credential,
	)

	_, err := client.ReadUsageEvents(context.Background(), "a-stale-position", 100)
	if !errors.Is(err, port.ErrCursorExpired) {
		t.Fatalf("ReadUsageEvents() error = %v, want it to wrap %v", err, port.ErrCursorExpired)
	}
}

// TestReadUsageEventsReportsATransportFailureWithoutTheRequestInIt is the
// leak guard. net/http wraps a transport failure in a *url.Error whose message
// embeds the whole request URL — the endpoint, and the cursor in its query — so
// the assertions below are what a reader needs to see to believe that wrapper
// is dropped rather than passed along. The cause is asserted to survive, so a
// passing test cannot be a silently empty message.
func TestReadUsageEventsReportsATransportFailureWithoutTheRequestInIt(t *testing.T) {
	transportFailure := errors.New("dial tcp 10.0.0.5:8443: connect: connection refused")
	client := New(
		&http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, transportFailure
		})},
		baseURL,
		credential,
	)

	_, err := client.ReadUsageEvents(context.Background(), anOpaqueCursor, 100)
	if err == nil {
		t.Fatal("ReadUsageEvents() error = nil, want a transport failure")
	}
	message := err.Error()
	if !strings.Contains(message, "connection refused") {
		t.Errorf("error %q does not carry the transport cause; the URL must be dropped and not the reason", message)
	}
	for _, secret := range []string{baseURL, credential, anOpaqueCursor, url.QueryEscape(anOpaqueCursor)} {
		if strings.Contains(message, secret) {
			t.Errorf("error %q carries %q; a failure must not name the endpoint, the credential, or the position it asked for — in either its raw or its escaped form", message, secret)
		}
	}
}

// TestReadUsageEventsReportsAnUnexpectedStatus is the fallthrough: a status the
// port has no refinement for is reported with its number, because the number is
// the part an operator acts on, and nothing else about the request is.
func TestReadUsageEventsReportsAnUnexpectedStatus(t *testing.T) {
	client := New(
		&http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusBadGateway, `{"error":{"code":"upstream_unavailable","message":"unreachable"},"request_id":"r-2"}`), nil
		})},
		baseURL,
		credential,
	)

	_, err := client.ReadUsageEvents(context.Background(), "", 100)
	if err == nil {
		t.Fatal("ReadUsageEvents() error = nil, want an error for a 502")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error %q does not name the status", err)
	}
	if strings.Contains(err.Error(), baseURL) || strings.Contains(err.Error(), credential) {
		t.Errorf("error %q names the endpoint or the credential", err)
	}
	if errors.Is(err, port.ErrCursorExpired) {
		t.Error("a 502 was reported as an expired cursor")
	}
}

// TestNewRefusesACombinationItCannotServe pins the construction-time failures.
// Each is configuration that could only produce a broken call later — a nil
// transport, an endpoint that is not an absolute http(s) URL, a base URL whose
// query would be silently merged into every request, a credential that could
// only ever be refused — and each is a panic here rather than a mystery far
// from the wiring that caused it.
func TestNewRefusesACombinationItCannotServe(t *testing.T) {
	tests := []struct {
		name       string
		httpClient *http.Client
		baseURL    string
		credential string
	}{
		{name: "nil http client", httpClient: nil, baseURL: baseURL, credential: credential},
		{name: "relative base URL", httpClient: http.DefaultClient, baseURL: "/management", credential: credential},
		{name: "non-http scheme", httpClient: http.DefaultClient, baseURL: "ftp://dataplane.example.test", credential: credential},
		{name: "base URL without a host", httpClient: http.DefaultClient, baseURL: "https://", credential: credential},
		{name: "base URL with a query", httpClient: http.DefaultClient, baseURL: baseURL + "?token=leaked", credential: credential},
		{name: "empty credential", httpClient: http.DefaultClient, baseURL: baseURL, credential: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Errorf("New(%s, %q, %q) did not panic", tt.name, tt.baseURL, tt.credential)
				}
			}()
			New(tt.httpClient, tt.baseURL, tt.credential)
		})
	}
}

// TestCurrentGroupVersionWalksTheContractPath pins the read's whole wire
// shape: the contract's path with the group name as its one escaped segment —
// the wildcard group's "*" must arrive %2A-escaped, a segment of its own —
// the credential header, and the answer translated field for field.
func TestCurrentGroupVersionWalksTheContractPath(t *testing.T) {
	var seen *http.Request
	client := New(
		capturingClient(&seen, jsonResponse(http.StatusOK,
			`{"group_name":"*","version":1,"group_version_id":"0198f0a4-3f6c-7000-8000-000000000001"}`)),
		baseURL,
		credential,
	)

	groupVersion, err := client.CurrentGroupVersion(context.Background(), "*")
	if err != nil {
		t.Fatalf("CurrentGroupVersion(*) error = %v, want nil", err)
	}
	if seen == nil {
		t.Fatal("the transport was never called")
	}
	if got, want := seen.URL.Path, "/management/internal/alias-groups/*/versions/current"; got != want {
		t.Errorf("decoded path = %q, want %q", got, want)
	}
	// The wire line is the assertion that matters: an escape carried in the
	// decoded Path field alone is re-escaped by String() into %252A, and a
	// test that read only the decoded field would stay green while the
	// wildcard became unreachable.
	if got, want := seen.URL.RequestURI(), "/management/internal/alias-groups/%2A/versions/current"; got != want {
		t.Errorf("wire line = %q, want %q — the wildcard escapes exactly once", got, want)
	}
	if got, want := seen.URL.RawQuery, ""; got != want {
		t.Errorf("raw query = %q, want none", got)
	}
	if got, want := seen.Header.Get("Authorization"), "Bearer "+credential; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got, want := seen.Method, http.MethodGet; got != want {
		t.Errorf("method = %s, want %s", got, want)
	}
	if groupVersion.GroupName != "*" || groupVersion.Version != 1 ||
		groupVersion.GroupVersionID != "0198f0a4-3f6c-7000-8000-000000000001" {
		t.Fatalf("answer = %+v, want the three fields the contract carried", groupVersion)
	}
}

// TestCurrentGroupVersionDistinguishesAMissFromAFailure pins the one status
// this operation may act on: a 404 is the catalog's answer that the group has
// no version — the lane-stopping ErrGroupNotFound — while every other status
// is a transport failure that must not be mistaken for it.
func TestCurrentGroupVersionDistinguishesAMissFromAFailure(t *testing.T) {
	newClient := func(response *http.Response) *Client {
		var seen *http.Request
		return New(capturingClient(&seen, response), baseURL, credential)
	}

	if _, err := newClient(jsonResponse(http.StatusNotFound,
		`{"error":{"code":"not_found","message":"no version of the requested alias group exists"}}`)).
		CurrentGroupVersion(context.Background(), "api"); !errors.Is(err, port.ErrGroupNotFound) {
		t.Fatalf("404 error = %v, want port.ErrGroupNotFound", err)
	}
	if _, err := newClient(jsonResponse(http.StatusBadGateway, `{})`)).
		CurrentGroupVersion(context.Background(), "api"); err == nil || errors.Is(err, port.ErrGroupNotFound) {
		t.Fatalf("502 error = %v, want a transport failure that is not ErrGroupNotFound", err)
	}
	if _, err := newClient(jsonResponse(http.StatusOK,
		`{"group_name":"other","version":1,"group_version_id":"0198f0a4-3f6c-7000-8000-000000000001"}`)).
		CurrentGroupVersion(context.Background(), "api"); !errors.Is(err, port.ErrMalformedAnswer) {
		t.Fatalf("an answer about a different group = %v, want port.ErrMalformedAnswer", err)
	}
	if _, err := newClient(jsonResponse(http.StatusOK,
		`{"version":1,"group_version_id":"0198f0a4-3f6c-7000-8000-000000000001"}`)).
		CurrentGroupVersion(context.Background(), "api"); !errors.Is(err, port.ErrMalformedAnswer) {
		t.Fatalf("an answer with no group_name = %v, want port.ErrMalformedAnswer", err)
	}
}
