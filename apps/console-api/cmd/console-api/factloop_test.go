package main

// This file pins the loop's own decisions, against the same seam the
// end-to-end regression uses: the real replay use case over the fake feed,
// store, cursor and applier. What only the loop can prove is its cadence —
// that a feed answering has_more is drained under the pass's deadline without
// waiting for the next tick, that the tick is what bounds an idle consumer,
// and that a position the feed can no longer replay is logged and survived
// rather than crashed on. The drain and the deadline are the loop's whole
// arithmetic; everything beneath them belongs to the files below.

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dataplaneadapter "github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
)

// queuedFeed answers each read with the next body in its queue and the last
// one forever after — a feed that produced a burst and then went quiet. The
// queue is what lets a test hand the loop a has_more page and observe, from
// the feed's own request log, whether the next page was pulled inside the
// same pass or on a tick that never came.
type queuedFeed struct {
	mu     sync.Mutex
	bodies []string
	afters []string
	server *httptest.Server
}

func newQueuedFeed(t *testing.T, bodies ...string) *queuedFeed {
	t.Helper()
	feed := &queuedFeed{bodies: bodies}
	feed.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		feed.mu.Lock()
		defer feed.mu.Unlock()
		feed.afters = append(feed.afters, r.URL.Query().Get("after"))
		body := feed.bodies[len(feed.bodies)-1]
		if len(feed.bodies) > 1 {
			body = feed.bodies[0]
			feed.bodies = feed.bodies[1:]
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(feed.server.Close)
	return feed
}

func (f *queuedFeed) askedFor() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.afters...)
}

// goneFeed answers 410 to everything: the stored position is no longer
// replayable, which is the loop's one specially-handled failure.
type goneFeed struct {
	server *httptest.Server
}

func newGoneFeed(t *testing.T) *goneFeed {
	t.Helper()
	feed := &goneFeed{}
	feed.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"gone"}`, http.StatusGone)
	}))
	t.Cleanup(feed.server.Close)
	return feed
}

// runLoop starts the loop the way main does — first pass immediate, ticks at
// interval — and returns a stop that cancels and waits for the goroutine.
func runLoop(t *testing.T, ctx context.Context, ingestion *application.FactIngestion, interval, timeout time.Duration) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	ingestionWG.Add(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runIngestionLoop(ctx, ingestion, interval, timeout)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the loop did not stop after its context was cancelled")
		}
	}
}

// await polls until want() holds or the deadline passes — the loop runs on
// its own goroutine, so an assertion that watches it polls rather than
// sleeps.
func await(t *testing.T, want func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if want() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A burst on the wire — a page that says more follows — is drained inside the
// same pass, under the pass's deadline, with an interval the test proves
// never fires. Two pages, one per class of one request's neighbours, both
// applied and both advanced past, while a tick at an hour could never have
// carried the second.
func TestTheLoopDrainsAFeedThatSaysMoreInsideOnePass(t *testing.T) {
	pageOne := `{"events":[{"append_seq":3,"request_id":"req-1","kind":"unbillable_orphaned","schema_version":1,"occurred_at":"2026-09-24T10:11:12Z","payload":{"allocations":[]}}],"next_cursor":"cursor-2","has_more":true}`
	pageTwo := `{"events":[{"append_seq":4,"request_id":"req-2","kind":"settled","schema_version":1,"occurred_at":"2026-09-24T10:11:13Z","payload":{"allocations":[]}}],"next_cursor":"cursor-3","has_more":false}`
	feed := newQueuedFeed(t, pageOne, pageTwo)
	client := dataplaneadapter.New(feed.server.Client(), feed.server.URL, seamCredential)
	world := newSeamWorld("cursor-1")
	ingestion := application.NewFactIngestion(client, seamStore{world: world}, seamCursor{world: world}, seamApplier{world: world})

	stop := runLoop(t, context.Background(), ingestion, time.Hour, 5*time.Second)
	defer stop()

	await(t, func() bool {
		world.mu.Lock()
		defer world.mu.Unlock()
		return world.position == "cursor-3" && len(world.effects) == 2
	})
	world.mu.Lock()
	position, effects := world.position, len(world.effects)
	world.mu.Unlock()
	if position != "cursor-3" || effects != 2 {
		t.Fatalf("position = %q with %d effect(s), want cursor-3 with both pages applied", position, effects)
	}
	if asked := feed.askedFor(); len(asked) < 2 || asked[0] != "cursor-1" || asked[1] != "cursor-2" {
		t.Fatalf("the feed was asked for %v, want the second page pulled inside the first pass", asked)
	}
}

// A position the feed can no longer replay is the loop's one special
// failure: named as its own line and survived — the loop keeps ticking, the
// position stays where it was, and nothing crashes, because the state is an
// operator's to resolve and the loop's job is to say so and stand by.
func TestTheLoopNamesAnUnreplayablePositionAndStandsBy(t *testing.T) {
	feed := newGoneFeed(t)
	client := dataplaneadapter.New(feed.server.Client(), feed.server.URL, seamCredential)
	world := newSeamWorld("cursor-1")
	ingestion := application.NewFactIngestion(client, seamStore{world: world}, seamCursor{world: world}, seamApplier{world: world})

	var logs syncBuffer
	previous := log.Default().Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	stop := runLoop(t, context.Background(), ingestion, 5*time.Millisecond, 5*time.Second)
	defer stop()

	await(t, func() bool {
		return strings.Contains(logs.String(), "no longer replayable")
	})
	if !strings.Contains(logs.String(), "no longer replayable") {
		t.Fatalf("logs = %q, want the unreplayable position named", logs.String())
	}
	world.mu.Lock()
	position := world.position
	world.mu.Unlock()
	if position != "cursor-1" {
		t.Fatalf("position = %q, want it where the refusal left it", position)
	}
}

// syncBuffer is the captured log: the loop writes it from its own goroutine
// and the test reads it from this one, so the guard is the buffer's, not the
// parties'.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
