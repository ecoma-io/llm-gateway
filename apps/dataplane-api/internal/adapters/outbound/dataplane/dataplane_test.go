package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// testCredential is distinctive so that the leak assertions below fire on what
// they are written for rather than on a coincidence.
const testCredential = "management-credential-9c17b0"

// opaqueCursor is deliberately nothing like a number: an adapter that parsed
// the cursor, compared it to a previous one or formatted it would pass a test
// written with "1234".
const opaqueCursor = "cur:9f2 &=<not-a-number>/+=="

// settledPageBody is a well-formed answer: one fact with a payload, one cursor,
// one has_more. The timestamp is deliberately a whole second so that the
// assertion below is about the instant rather than about a format choice.
const settledPageBody = `{"events":[{"request_id":"req_01HZ","kind":"settled","schema_version":1,"occurred_at":"2026-09-23T10:00:00Z","payload":{"allocation_id":"alloc-1"}}],"next_cursor":"` + opaqueCursor + `","has_more":true}`

// recordedCall is one request as the Data Plane's listener saw it. The query is
// kept as the parsed parameters rather than as the request, because a missing
// parameter and an empty one are different answers and half of what the test
// below pins is that difference.
type recordedCall struct {
	authorization string
	path          string
	after         []string
	limit         []string
}

// upstream stands in for the Data Plane's management listener: it answers with
// whatever the test configured and records what it received. The record is
// guarded because the handler runs on the server's goroutine while the test
// reads it from its own — the request has completed on the wire by then, but
// only a lock makes that ordering visible to the race detector.
type upstream struct {
	mu     sync.Mutex
	status int
	body   string
	calls  []recordedCall
}

func (u *upstream) record(r *stdhttp.Request) recordedCall {
	u.mu.Lock()
	defer u.mu.Unlock()

	query := r.URL.Query()
	call := recordedCall{
		authorization: r.Header.Get("Authorization"),
		path:          r.URL.Path,
		after:         query["after"],
		limit:         query["limit"],
	}
	u.calls = append(u.calls, call)
	return call
}

func (u *upstream) recorded() []recordedCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.calls)
}

// server starts the listener and returns a client wired to it. The client is
// the server's own, so its connections are torn down with the test.
func (u *upstream) server(t *testing.T) *Client {
	t.Helper()
	server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		u.record(r)
		status := u.status
		if status == 0 {
			status = stdhttp.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(u.body))
	}))
	t.Cleanup(server.Close)
	return New(server.Client(), server.URL, testCredential)
}

// TestReadUsageEventsForwardsTheCursorAndThePageSizeUntouched is the relay
// claim, asserted on what the Data Plane actually received: the cursor travels
// as the opaque string it was given — no parsing, no trimming, no
// reformatting — and an absent one is absent rather than empty.
func TestReadUsageEventsForwardsTheCursorAndThePageSizeUntouched(t *testing.T) {
	tests := []struct {
		name      string
		after     string
		limit     int
		wantAfter []string
	}{
		{
			name:      "an opaque cursor crosses as the string it was",
			after:     opaqueCursor,
			limit:     7,
			wantAfter: []string{opaqueCursor},
		},
		{
			name:      "an absent cursor is sent as an absent parameter",
			after:     "",
			limit:     100,
			wantAfter: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := &upstream{body: settledPageBody}
			client := up.server(t)

			if _, err := client.ReadUsageEvents(context.Background(), tt.after, tt.limit); err != nil {
				t.Fatalf("ReadUsageEvents() error = %v", err)
			}
			calls := up.recorded()
			if len(calls) != 1 {
				t.Fatalf("the Data Plane received %d calls, want 1", len(calls))
			}
			if !slices.Equal(calls[0].after, tt.wantAfter) {
				t.Errorf("the Data Plane received after = %v, want %v", calls[0].after, tt.wantAfter)
			}
			if !slices.Equal(calls[0].limit, []string{strconv.Itoa(tt.limit)}) {
				t.Errorf("the Data Plane received limit = %v, want [%d]", calls[0].limit, tt.limit)
			}
			if want := "Bearer " + testCredential; calls[0].authorization != want {
				t.Errorf("the Data Plane received Authorization = %q, want %q", calls[0].authorization, want)
			}
			if want := "/internal/usage-events"; calls[0].path != want {
				t.Errorf("the Data Plane received a call to %q, want %q", calls[0].path, want)
			}
		})
	}
}

// TestReadUsageEventsKeepsNoPositionBetweenCalls is the "owns no state" claim
// as a behaviour rather than as an absence: the Data Plane answers a first call
// with a cursor, and the second call — which named none — still names none. An
// adapter that remembered next_cursor and resumed from it would be the façade
// holding a position, which is the one thing it may not do: the consumer owns
// its position, and a position advanced by the hop in the middle could skip
// facts the consumer never applied.
func TestReadUsageEventsKeepsNoPositionBetweenCalls(t *testing.T) {
	up := &upstream{body: settledPageBody}
	client := up.server(t)

	for range 2 {
		if _, err := client.ReadUsageEvents(context.Background(), "", 100); err != nil {
			t.Fatalf("ReadUsageEvents() error = %v", err)
		}
	}
	calls := up.recorded()
	if len(calls) != 2 {
		t.Fatalf("the Data Plane received %d calls, want 2", len(calls))
	}
	for i, call := range calls {
		if call.after != nil {
			t.Errorf("call %d carried after = %v; the adapter remembered a position", i+1, call.after)
		}
	}
}

// TestReadUsageEventsReadsTheDataPlanesPage pins the success mapping field by
// field, including the payload's bytes: the port's Event carries the fact's
// body raw, and anything that decoded and re-encoded it here would be this
// module holding a copy of a shape it does not own.
func TestReadUsageEventsReadsTheDataPlanesPage(t *testing.T) {
	up := &upstream{body: settledPageBody}
	client := up.server(t)

	page, err := client.ReadUsageEvents(context.Background(), opaqueCursor, 100)
	if err != nil {
		t.Fatalf("ReadUsageEvents() error = %v", err)
	}
	if page.NextCursor != opaqueCursor {
		t.Errorf("NextCursor = %q, want %q", page.NextCursor, opaqueCursor)
	}
	if !page.HasMore {
		t.Error("HasMore = false, want true")
	}
	if len(page.Events) != 1 {
		t.Fatalf("len(Events) = %d, want 1", len(page.Events))
	}
	event := page.Events[0]
	if event.RequestID != "req_01HZ" {
		t.Errorf("RequestID = %q, want %q", event.RequestID, "req_01HZ")
	}
	if event.Kind != "settled" {
		t.Errorf("Kind = %q, want %q", event.Kind, "settled")
	}
	if event.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", event.SchemaVersion)
	}
	if want := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC); !event.OccurredAt.Equal(want) {
		t.Errorf("OccurredAt = %v, want %v", event.OccurredAt, want)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("the payload did not survive the read: %v", err)
	}
	if got := payload["allocation_id"]; got != "alloc-1" {
		t.Errorf("payload allocation_id = %v, want alloc-1", got)
	}
}

// TestReadUsageEventsClassifiesEveryFailureTheSeamCanProduce is the mapping
// table: a position the Data Plane has aged out is the one failure a caller can
// act on, and everything else — unreachable, refused, unreadable, or an
// unclassifiable status — is one condition, because in all of them the answer
// is unknown and this process has nothing to answer with instead.
//
// The Data Plane refusing this process's own credential is in the second group
// deliberately. It is a deployment error, not something the caller can repair,
// and reporting it as the caller's problem would send a consumer looking for a
// cursor bug in its own code.
//
// Every failure is also checked for the two values that must not travel with
// it: the listener's address and the shared secret.
func TestReadUsageEventsClassifiesEveryFailureTheSeamCanProduce(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		closed  bool
		wantErr error
	}{
		{
			name:    "a position past the retained history is a cursor the Data Plane refuses",
			status:  stdhttp.StatusGone,
			body:    `{"error":{"code":"cursor_expired","message":"..."}}`,
			wantErr: dataplane.ErrCursorExpired,
		},
		{
			name:    "the Data Plane refusing this process's own credential is not the caller's problem",
			status:  stdhttp.StatusUnauthorized,
			body:    `{"error":{"code":"unauthenticated","message":"..."}}`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "a request the Data Plane calls invalid is not something this façade can repair",
			status:  stdhttp.StatusBadRequest,
			body:    `{"error":{"code":"invalid_request","message":"..."}}`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "an implementation failure upstream is not this process's failure",
			status:  stdhttp.StatusInternalServerError,
			body:    `{"error":{"code":"internal","message":"..."}}`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "a success body this façade cannot read is a page it will not guess at",
			status:  stdhttp.StatusOK,
			body:    `{"events":[{"request_id":`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "a fact whose timestamp is not a timestamp is a page this façade cannot trust",
			status:  stdhttp.StatusOK,
			body:    `{"events":[{"request_id":"r","kind":"settled","schema_version":1,"occurred_at":"last Tuesday","payload":{}}],"next_cursor":"c","has_more":false}`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "a listener that is not there is the Data Plane being unavailable",
			closed:  true,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			if tt.closed {
				server.Close()
			} else {
				t.Cleanup(server.Close)
			}

			client := New(server.Client(), server.URL, testCredential)
			_, err := client.ReadUsageEvents(context.Background(), opaqueCursor, 100)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ReadUsageEvents() error = %v, want it to wrap %v", err, tt.wantErr)
			}
			// The address is the deployment's private topology and the
			// credential is a secret. Both are checked on the error text rather
			// than on the log line, because the error text is what a future
			// caller might choose to log.
			if strings.Contains(err.Error(), testCredential) {
				t.Errorf("the error text carries the credential: %q", err.Error())
			}
			if strings.Contains(err.Error(), server.URL) {
				t.Errorf("the error text carries the Data Plane's address: %q", err.Error())
			}
		})
	}
}
