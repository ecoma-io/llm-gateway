package main

// This file is the end-to-end regression for the one way this flow could lose
// money: a response that is not a page moving the durable position across
// facts nobody saw. Everything else in the module tests one side of the seam —
// the outbound adapter's decoder refuses malformed bodies, the application's
// replay keeps position and work together — and neither test can see the
// other. The dependency rule keeps it that way: the application may not import
// its adapter and the adapter may not import the application, so a test that
// wires the real decoder to the real use case has exactly one home, and it is
// the composition root's. That is fitting rather than incidental: wiring the
// two together is this package's whole job.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	dataplaneadapter "github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	dataplaneport "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// seamCredential is distinctive so the leak assertion below fires on what it
// is written for rather than on a coincidence.
const seamCredential = "management-credential-e2e-44b1"

// malformedPageBody is the page that would once have cost facts: a 200 that
// carries a position to advance to and a fact whose request_id is null. Zero-
// value decoding turned the null into "" and the page crossed as well-formed;
// the cursor then advanced across a fact no applier ever saw.
const malformedPageBody = `{"events":[{"append_seq":7,"request_id":null,"kind":"settled","schema_version":1,"occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":true}`

// validPageBody answers the same range with the fact the malformed page hid.
const validPageBody = `{"events":[{"append_seq":7,"request_id":"req-1","kind":"settled","schema_version":1,"occurred_at":"2026-09-24T10:11:12Z","payload":{"amount":"4200"}}],"next_cursor":"cursor-2","has_more":false}`

// feedServer stands in for dataplane-api: it answers every read with the body
// the test last set, and records what it was asked for. The fields are guarded
// because the handler runs on the server's goroutine while the test reads them
// from its own — the request has completed on the wire by then, but only a
// lock makes that ordering visible to the race detector.
type feedServer struct {
	mu     sync.Mutex
	body   string
	afters []string
	server *httptest.Server
}

func newFeedServer(t *testing.T, body string) *feedServer {
	t.Helper()
	feed := &feedServer{body: body}
	feed.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		feed.mu.Lock()
		defer feed.mu.Unlock()
		feed.afters = append(feed.afters, r.URL.Query().Get("after"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(feed.body))
	}))
	t.Cleanup(feed.server.Close)
	return feed
}

func (f *feedServer) setBody(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body = body
}

func (f *feedServer) askedFor() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.afters)
}

// seamWorld is the Control Plane's durable state in miniature: the position,
// the applier's effects, and the order things happened in. A rollback restores
// the state and keeps the record, because what the assertions need to see is
// both.
type seamWorld struct {
	position string
	effects  map[string]persistence.Fact
	log      []string
}

func newSeamWorld(position string) *seamWorld {
	return &seamWorld{position: position, effects: map[string]persistence.Fact{}}
}

// seamStore is the unit of work. It snapshots the durable state at begin and
// restores it when the callback fails, which is what makes the assertion
// "nothing was durably applied" mean the same thing a database would mean.
type seamStore struct {
	persistence.Store
	world *seamWorld
}

func (s seamStore) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	s.world.log = append(s.world.log, "begin")
	position := s.world.position
	effects := make(map[string]persistence.Fact, len(s.world.effects))
	for requestID, fact := range s.world.effects {
		effects[requestID] = fact
	}

	if err := fn(context.WithValue(ctx, seamTxKey{}, true)); err != nil {
		s.world.position = position
		s.world.effects = effects
		s.world.log = append(s.world.log, "rollback")
		return err
	}
	s.world.log = append(s.world.log, "commit")
	return nil
}

// seamTxKey marks the context a unit of work handed its callback, the way the
// real store's transaction travels; InUnitOfWork reads it back.
type seamTxKey struct{}

// InUnitOfWork answers the marker WithinTx marks with — the fake of the port
// member the unit-of-work-shaped repositories ask before refusing a call.
func (s seamStore) InUnitOfWork(ctx context.Context) bool {
	marked, ok := ctx.Value(seamTxKey{}).(bool)
	return ok && marked
}

type seamCursor struct{ world *seamWorld }

func (c seamCursor) Position(context.Context) (string, error) {
	return c.world.position, nil
}

func (c seamCursor) Advance(_ context.Context, next string) error {
	c.world.log = append(c.world.log, "advance:"+next)
	c.world.position = next
	return nil
}

type seamApplier struct{ world *seamWorld }

func (a seamApplier) Apply(_ context.Context, fact persistence.Fact) error {
	a.world.log = append(a.world.log, "apply:"+fact.RequestID)
	a.world.effects[fact.RequestID] = fact
	return nil
}

// TestAMalformedPageOnTheWireNeverMovesTheDurablePosition is the original
// P1, asserted end to end: the real outbound decoder reading a real 200 whose
// required fields do not arrive, feeding the real replay use case, over a
// position the fakes hold the way the store will.
//
// The chain is deliberately the whole chain. A decoder that refused in its own
// package and a use case that never advanced on an error are each true alone
// and worthless together if the seam between them dropped either property; a
// caller that ignored the error, a Page returned beside it, a convenience
// short-circuit added in the application — any of those would pass both
// packages' own tests and still lose facts. Here the failure has to survive
// the composition root to count.
//
// The second half is the property the first half exists to protect: after the
// refused page, the same range is still there to read, and a valid page for it
// applies and advances. A refusal is a delay, never a skip.
func TestAMalformedPageOnTheWireNeverMovesTheDurablePosition(t *testing.T) {
	const initial = "cursor-1"

	feed := newFeedServer(t, malformedPageBody)
	client := dataplaneadapter.New(feed.server.Client(), feed.server.URL, seamCredential)
	world := newSeamWorld(initial)
	ingestion := application.NewFactIngestion(client, seamStore{world: world}, seamCursor{world: world}, seamApplier{world: world})

	_, err := ingestion.Replay(context.Background())
	if !errors.Is(err, dataplaneport.ErrMalformedPage) {
		t.Fatalf("Replay() error = %v, want it to wrap %v — the decoder's refusal must reach the caller as the seam's own classification", err, dataplaneport.ErrMalformedPage)
	}
	if got, want := world.position, initial; got != want {
		t.Errorf("position = %q, want %q — the durable position must not move on a page nobody read", got, want)
	}
	if len(world.effects) != 0 {
		t.Errorf("the applier recorded %d effect(s), want 0", len(world.effects))
	}
	if slices.ContainsFunc(world.log, func(entry string) bool { return strings.HasPrefix(entry, "advance:") }) {
		t.Errorf("the flow observed %v; a malformed page must never reach an advance", world.log)
	}
	if got, want := feed.askedFor(), []string{initial}; !slices.Equal(got, want) {
		t.Errorf("the feed was asked for %v, want [%v]", got, want)
	}
	for _, secret := range []string{feed.server.URL, seamCredential, url.QueryEscape(initial)} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q carries %q; a refusal must not name the endpoint, the credential or the position", err.Error(), secret)
		}
	}

	// The feed now answers the same range with the fact the malformed page
	// carried. The pass that was refused is retried from the position it was
	// refused at, and the fact that page hid is the fact this one applies.
	feed.setBody(validPageBody)
	result, err := ingestion.Replay(context.Background())
	if err != nil {
		t.Fatalf("second Replay() error = %v, want nil", err)
	}
	if got, want := result.Applied, 1; got != want {
		t.Errorf("Applied = %d, want %d", got, want)
	}
	if _, applied := world.effects["req-1"]; !applied {
		t.Error("the applier recorded no effect for req-1; the range the malformed page hid was never processed")
	}
	if got, want := world.position, "cursor-2"; got != want {
		t.Errorf("position = %q, want %q", got, want)
	}
	if got, want := world.log, []string{"begin", "apply:req-1", "advance:cursor-2", "commit"}; !slices.Equal(got, want) {
		t.Errorf("the flow observed %v, want %v", got, want)
	}
	if got, want := feed.askedFor(), []string{initial, initial}; !slices.Equal(got, want) {
		t.Errorf("the feed was asked for %v, want %v", got, want)
	}
}
