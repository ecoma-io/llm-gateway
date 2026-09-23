package dataplane

import (
	"context"
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
			{"request_id":"req-1","kind":"settled","schema_version":3,"occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}
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
