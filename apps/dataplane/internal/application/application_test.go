package application

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/usagefacts"
)

func TestVersionReturnsTheBuildVersion(t *testing.T) {
	app := New("v0.1.0", &stubFacts{})

	if got, want := app.Version(), "v0.1.0"; got != want {
		t.Errorf("Version() = %q, want %q", got, want)
	}
}

// stubFacts is the fact reader the tests below drive the use case with. It
// records the arguments it was called with, because half of what the use case
// does is decide what reaches the port and the assertions need to see that
// decision rather than only its result.
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

func TestReadUsageEventsPassesTheCursorThroughUntouched(t *testing.T) {
	facts := &stubFacts{}
	app := New("v0.1.0", facts)

	// A cursor is opaque, so the use case must not care what is in it — an
	// awkward string with a space in it included. A use case that trimmed,
	// validated or re-encoded this value would be one the Data Plane's own
	// encoding could never change.
	const cursor = "opaque position 42/not-a-number"
	if _, err := app.ReadUsageEvents(context.Background(), cursor, 10); err != nil {
		t.Fatalf("ReadUsageEvents() error = %v", err)
	}

	if facts.after != cursor {
		t.Errorf("the port was read after %q, want %q unchanged", facts.after, cursor)
	}
}

// TestReadUsageEventsPassesThePageSizeItIsGiven is the page-size half of the
// boundary claim, and its shape is the decision: the use case neither defaults
// nor clamps, because the management surface has already applied the contract's
// default and its bounds before calling in. A use case that adjusted the value
// here would be a second place a page size is decided, and the adjustment would
// be invisible to the caller it was made for.
//
// The out-of-range rows are the ones that make this a test of "neither defaults
// nor clamps" rather than of arithmetic. An in-range table alone is satisfied by
// a use case that clamps, or that substitutes the default whenever the value
// looks wrong: every row it feeds in is one a clamp would leave alone, so the
// clamp is invisible. Values the contract refuses are exactly the ones where
// those rewrites would show, and the answer here is that they travel to the port
// unchanged — the surface above refuses them before this code runs, and this
// code's part of that sentence is to have no opinion about them.
func TestReadUsageEventsPassesThePageSizeItIsGiven(t *testing.T) {
	for _, limit := range []int{1, 250, usagefacts.DefaultLimit, usagefacts.MaxLimit, 0, -1, usagefacts.MaxLimit + 1} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			facts := &stubFacts{}
			app := New("v0.1.0", facts)

			if _, err := app.ReadUsageEvents(context.Background(), "", limit); err != nil {
				t.Fatalf("ReadUsageEvents() error = %v", err)
			}
			if facts.limit != limit {
				t.Errorf("the port was read with limit %d, want %d unchanged", facts.limit, limit)
			}
		})
	}
}

func TestReadUsageEventsReturnsThePortsPageUnchanged(t *testing.T) {
	// The page is the store's, in the store's order, with the store's position.
	// The use case is a boundary and not a filter: anything it did to this
	// value would be applied to every consumer at once, from the process that
	// serves LLM traffic.
	page := usagefacts.Page{
		Events: []usagefacts.Event{{
			RequestID:     "req_1",
			Kind:          "settled",
			SchemaVersion: 1,
			OccurredAt:    time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
			Payload:       json.RawMessage(`{"tokens":7}`),
		}},
		NextCursor: "position-7",
		HasMore:    true,
	}

	app := New("v0.1.0", &stubFacts{page: page})
	got, err := app.ReadUsageEvents(context.Background(), "", usagefacts.DefaultLimit)
	if err != nil {
		t.Fatalf("ReadUsageEvents() error = %v", err)
	}

	if len(got.Events) != 1 || got.Events[0].RequestID != "req_1" || string(got.Events[0].Payload) != `{"tokens":7}` {
		t.Errorf("ReadUsageEvents() events = %+v, want the port's page unchanged", got.Events)
	}
	if got.NextCursor != page.NextCursor || got.HasMore != page.HasMore {
		t.Errorf("ReadUsageEvents() = %q/%v, want %q/%v", got.NextCursor, got.HasMore, page.NextCursor, page.HasMore)
	}
}

func TestReadUsageEventsReportsASourceFailureRatherThanAnEmptyPage(t *testing.T) {
	// An empty page and an unreadable source are different answers, and the
	// difference is a consumer's next move: advance, or retry. Collapsing the
	// error into an empty page here would tell a consumer this Data Plane has
	// recorded nothing — a claim no adapter is entitled to make.
	app := New("v0.1.0", &stubFacts{err: usagefacts.ErrSourceUnavailable})

	_, err := app.ReadUsageEvents(context.Background(), "", usagefacts.DefaultLimit)

	if !errors.Is(err, usagefacts.ErrSourceUnavailable) {
		t.Errorf("ReadUsageEvents() error = %v, want ErrSourceUnavailable", err)
	}
}

func TestApplicationErrorsPreserveTheirMeaning(t *testing.T) {
	cause := errors.New("the storage implementation failed")
	tests := []struct {
		name        string
		err         *Error
		wantCode    Code
		wantMessage string
		wantCause   error
	}{
		{
			name:        "not found retains a safe public message",
			err:         NotFound("version was not found"),
			wantCode:    CodeNotFound,
			wantMessage: "version was not found",
		},
		{
			name:      "internal retains its cause for server-side inspection",
			err:       Internal(cause),
			wantCode:  CodeInternal,
			wantCause: cause,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Code; got != tt.wantCode {
				t.Errorf("Code = %q, want %q", got, tt.wantCode)
			}
			if got := tt.err.Message; got != tt.wantMessage {
				t.Errorf("Message = %q, want %q", got, tt.wantMessage)
			}
			if tt.wantCause != nil && !errors.Is(tt.err, tt.wantCause) {
				t.Errorf("error does not unwrap to %v", tt.wantCause)
			}
		})
	}
}
