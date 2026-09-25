package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// cursor is deliberately nothing like a number: this layer may hand a cursor
// over and nothing else, and a value an offset-shaped implementation could
// parse is the wrong fixture for proving that.
const cursor = "cur:9f2 &=<not-a-number>/+=="

// TestUsageEventsReturnsTheDataPlanesPageUnchanged is the pass-through claim:
// the page comes back exactly as the port produced it — same order, same
// cursor, same payload bytes — and the cursor and page size go down exactly as
// they arrived. Anything this layer did to either would be the façade holding
// an opinion the protocol does not give it.
func TestUsageEventsReturnsTheDataPlanesPageUnchanged(t *testing.T) {
	occurredAt := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	page := dataplane.Page{
		Events: []dataplane.Event{
			{RequestID: "req_2", Kind: "released", SchemaVersion: 1, OccurredAt: occurredAt, Payload: []byte(`{"b":1}`)},
			{RequestID: "req_1", Kind: "settled", SchemaVersion: 1, OccurredAt: occurredAt, Payload: []byte(`{"a":1}`)},
		},
		NextCursor: cursor,
		HasMore:    true,
	}
	usage := &fakeUsageFacts{page: page}
	app := New("test", usage, &fakeCatalog{}, &fakeProjection{})

	got, err := app.UsageEvents(context.Background(), cursor, 17)
	if err != nil {
		t.Fatalf("UsageEvents() error = %v", err)
	}
	if got.NextCursor != cursor || !got.HasMore {
		t.Errorf("UsageEvents() = %+v, want the page the port produced", got)
	}
	if len(got.Events) != len(page.Events) {
		t.Fatalf("UsageEvents() returned %d events, want %d", len(got.Events), len(page.Events))
	}
	for i := range page.Events {
		if got.Events[i].RequestID != page.Events[i].RequestID {
			t.Errorf("event %d = %q, want %q — the use case reordered the page", i, got.Events[i].RequestID, page.Events[i].RequestID)
		}
		if string(got.Events[i].Payload) != string(page.Events[i].Payload) {
			t.Errorf("event %d payload = %q, want %q", i, got.Events[i].Payload, page.Events[i].Payload)
		}
	}
	if usage.after != cursor {
		t.Errorf("the port was asked for after = %q, want %q", usage.after, cursor)
	}
	if usage.limit != 17 {
		t.Errorf("the port was asked for limit = %d, want 17", usage.limit)
	}
}

// TestUsageEventsKeepsThePortsFailureAsItsCause separates the two failures the
// seam can produce from everything else, and pins that each carries a code the
// transport can map and a cause errors.Is can still reach. Losing the cause
// would leave a caller unable to tell an aged-out position from an unreachable
// Data Plane once the error had crossed the boundary.
func TestUsageEventsKeepsThePortsFailureAsItsCause(t *testing.T) {
	tests := []struct {
		name     string
		portErr  error
		wantCode Code
		wantIs   error
	}{
		{
			name:     "an unreplayable position keeps its own category",
			portErr:  dataplane.ErrCursorExpired,
			wantCode: CodeCursorExpired,
			wantIs:   dataplane.ErrCursorExpired,
		},
		{
			name:     "an unreachable Data Plane keeps its own category",
			portErr:  dataplane.ErrUpstreamUnavailable,
			wantCode: CodeUpstreamUnavailable,
			wantIs:   dataplane.ErrUpstreamUnavailable,
		},
		{
			name:     "a failure the port does not describe is this application's own",
			portErr:  errors.New("the adapter returned something the port does not define"),
			wantCode: CodeInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := New("test", &fakeUsageFacts{err: tt.portErr}, &fakeCatalog{}, &fakeProjection{})

			_, err := app.UsageEvents(context.Background(), cursor, 100)

			applicationError, ok := As(err)
			if !ok {
				t.Fatalf("UsageEvents() error = %v, want an application error", err)
			}
			if applicationError.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", applicationError.Code, tt.wantCode)
			}
			if applicationError.Message == "" && tt.wantCode != CodeInternal {
				t.Errorf("Code %q carries no message a transport could serialize", tt.wantCode)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("the error does not unwrap to %v", tt.wantIs)
			}
		})
	}
}
