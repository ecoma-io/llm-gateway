package application

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// occurredAt is the instant every test fact carries. Its value is arbitrary;
// what the tests pin is that it survives the trip untouched, which is why it
// carries a non-zero offset-free instant rather than time.Now().
var occurredAt = time.Date(2026, 9, 24, 10, 11, 12, 0, time.UTC)

// factEvent builds one event as the seam carries it.
func factEvent(requestID, kind string) dataplane.Event {
	return dataplane.Event{
		RequestID:     requestID,
		Kind:          kind,
		SchemaVersion: 3,
		OccurredAt:    occurredAt,
		Payload:       json.RawMessage(`{"amount":"4200"}`),
	}
}

// TestThePageSizeTheFlowAsksForIsOneTheContractAnswers is the one rule
// FactPageSize has to satisfy, and it is a rule about the wire rather than about
// this flow.
//
// The constant is a consumer's choice — how much one pass can hold — and not a
// copy of the contract's default, so this test deliberately does not assert that
// it equals 100. What it asserts is that the choice lies inside the range the
// contract will answer: `limit` below 1 or above 1000 is refused with
// `400 invalid_request` by the façade, so a page size that drifted outside those
// bounds would not be a consumer paging differently, it would be a consumer
// whose every pass fails — and it would fail as a rejected request, with nothing
// anywhere saying the number was the reason.
//
// The bounds are spelled as literals here, and they are the contract's rather
// than this package's: the same two numbers are read out of
// api/openapi/shared/usage-facts.yaml by
// internal/adapters/outbound/dataplane/contract_test.go, which is what holds
// them to the document. A bound that moved there fails there, loudly, instead of
// being silently accommodated here.
func TestThePageSizeTheFlowAsksForIsOneTheContractAnswers(t *testing.T) {
	const (
		contractMinimum = 1
		contractMaximum = 1000
	)

	if FactPageSize < contractMinimum || FactPageSize > contractMaximum {
		t.Errorf("FactPageSize is %d and the contract's limit is %d..%d; a page size outside that range is refused on every pass rather than answered", FactPageSize, contractMinimum, contractMaximum)
	}
}

// TestAFirstPageIsAppliedBeforeTheCursorAdvances is the happy path and the
// ordering claim in one assertion: the position is read, the page is fetched,
// every fact is applied, the cursor advances to the page's next_cursor, and the
// transaction commits. The full sequence is compared rather than the endpoints,
// because "the cursor advanced to the right value" is also true of a pass that
// advanced before it applied anything — the bug that would lose a page on the
// first crash.
func TestAFirstPageIsAppliedBeforeTheCursorAdvances(t *testing.T) {
	world := newWorld()
	world.pages[""] = dataplane.Page{
		Events:     []dataplane.Event{factEvent("req-1", "settled"), factEvent("req-2", "released")},
		NextCursor: "cursor-2",
		HasMore:    true,
	}

	result, err := newIngestion(world).Replay(context.Background())
	if err != nil {
		t.Fatalf("Replay() error = %v, want nil", err)
	}

	if got, want := result.Applied, 2; got != want {
		t.Errorf("Applied = %d, want %d", got, want)
	}
	if !result.HasMore {
		t.Error("HasMore = false, want true: the page said more facts follow it")
	}
	if got, want := world.position, "cursor-2"; got != want {
		t.Errorf("position = %q, want %q — the page's next_cursor, never one computed here", got, want)
	}
	if got, want := world.reads, []string{""}; !slices.Equal(got, want) {
		t.Errorf("the Data Plane was asked for %q, want %q — a consumer with no stored position reads from the beginning", got, want)
	}
	if got, want := world.limits, []int{FactPageSize}; !slices.Equal(got, want) {
		t.Errorf("page limits = %v, want %v", got, want)
	}
	want := []string{"begin", "apply:req-1", "apply:req-2", "advance:cursor-2", "commit"}
	if !slices.Equal(world.order, want) {
		t.Errorf("the flow observed %v, want %v", world.order, want)
	}
	if world.outsideTx != 0 {
		t.Errorf("%d port call(s) ran outside the unit of work; the applier and the cursor must be covered by one transaction", world.outsideTx)
	}
}

// TestAFactIsTranslatedIntoTheStoresOwnVocabulary is the port boundary made
// visible: the applier receives a persistence.Fact whose fields came from the
// event, not the event itself. A field dropped in the translation is a
// settlement derived from nothing, which is why every field is asserted.
func TestAFactIsTranslatedIntoTheStoresOwnVocabulary(t *testing.T) {
	world := newWorld()
	world.pages[""] = dataplane.Page{
		Events:     []dataplane.Event{factEvent("req-1", "settled")},
		NextCursor: "cursor-2",
	}

	if _, err := newIngestion(world).Replay(context.Background()); err != nil {
		t.Fatalf("Replay() error = %v, want nil", err)
	}

	fact, applied := world.effect("req-1", "settled")
	if !applied {
		t.Fatal("the applier recorded no effect for req-1's settlement class")
	}
	want := persistence.Fact{
		RequestID:     "req-1",
		Kind:          "settled",
		SchemaVersion: 3,
		OccurredAt:    occurredAt,
		Payload:       []byte(`{"amount":"4200"}`),
	}
	if fact.RequestID != want.RequestID || fact.Kind != want.Kind || fact.SchemaVersion != want.SchemaVersion {
		t.Errorf("applied fact = %+v, want the event's identity and kind %+v", fact, want)
	}
	if !fact.OccurredAt.Equal(want.OccurredAt) {
		t.Errorf("OccurredAt = %s, want %s", fact.OccurredAt, want.OccurredAt)
	}
	if got := string(fact.Payload); got != string(want.Payload) {
		t.Errorf("Payload = %s, want %s", got, want.Payload)
	}
}

// TestAFailedApplyLeavesThePositionAndRefetchesTheSamePage is the crash-safety
// contract. The first pass must apply nothing durably, leave the position
// exactly where it was and never advance, and the second pass must ask for the
// same page — because a position that moved past a page that failed is a page
// nobody will ever read again.
func TestAFailedApplyLeavesThePositionAndRefetchesTheSamePage(t *testing.T) {
	world := newWorld()
	world.pages[""] = dataplane.Page{
		Events:     []dataplane.Event{factEvent("req-1", "settled"), factEvent("req-2", "settled")},
		NextCursor: "cursor-2",
		HasMore:    true,
	}
	world.failOn["req-2"] = errApply

	_, err := newIngestion(world).Replay(context.Background())
	if !errors.Is(err, errApply) {
		t.Fatalf("Replay() error = %v, want it to wrap %v", err, errApply)
	}

	if got, want := world.position, ""; got != want {
		t.Errorf("position = %q, want %q — a failed pass must leave the position untouched", got, want)
	}
	if len(world.effects) != 0 {
		t.Errorf("the applier recorded %d effect(s) after a rollback, want 0: the facts applied before the failure roll back with it", len(world.effects))
	}
	if slices.ContainsFunc(world.order, func(entry string) bool { return strings.HasPrefix(entry, "advance:") }) {
		t.Errorf("the flow observed %v; the cursor must never advance when applying failed", world.order)
	}
	if last := world.order[len(world.order)-1]; last != "rollback" {
		t.Errorf("the flow ended with %q, want a rollback", last)
	}

	// The next pass reads the same position and gets the same page, which is
	// the whole point of not advancing: the facts are delivered again, and the
	// applier's idempotency is what makes that free.
	world.failOn = map[string]error{}
	result, err := newIngestion(world).Replay(context.Background())
	if err != nil {
		t.Fatalf("second Replay() error = %v, want nil", err)
	}
	if got, want := world.reads, []string{"", ""}; !slices.Equal(got, want) {
		t.Errorf("the Data Plane was asked for %q, want %q — the same page is refetched after a failure", got, want)
	}
	if got, want := result.Applied, 2; got != want {
		t.Errorf("Applied = %d, want %d", got, want)
	}
	if got, want := world.position, "cursor-2"; got != want {
		t.Errorf("position = %q, want %q after the retry succeeded", got, want)
	}
}

// TestAFailedAdvanceRollsBackTheAppliedFacts is the same unit of work seen from
// the other end: the cursor write is the last thing the transaction does, and a
// failure there must take the effects with it. If it did not, the Control Plane
// would hold facts it has no recorded position for, and the next pass would
// derive them again — free, because the applier is idempotent, but only because
// the facts and the position really did move together.
func TestAFailedAdvanceRollsBackTheAppliedFacts(t *testing.T) {
	world := newWorld()
	world.pages[""] = dataplane.Page{
		Events:     []dataplane.Event{factEvent("req-1", "settled")},
		NextCursor: "cursor-2",
	}
	world.advanceErr = errAdvance

	_, err := newIngestion(world).Replay(context.Background())
	if !errors.Is(err, errAdvance) {
		t.Fatalf("Replay() error = %v, want it to wrap %v", err, errAdvance)
	}
	if len(world.effects) != 0 {
		t.Errorf("the applier recorded %d effect(s), want 0: an advance that failed rolls the applied facts back with it", len(world.effects))
	}
	if got, want := world.position, ""; got != want {
		t.Errorf("position = %q, want %q unchanged", got, want)
	}
	if last := world.order[len(world.order)-1]; last != "rollback" {
		t.Errorf("the flow ended with %q, want a rollback", last)
	}
}

// TestAReplayedPageHasNoSecondEffect is kind-classed idempotency seen from
// outside the applier: the same range read twice delivers every fact twice, and
// the applier's state still holds one effect for the fact's class. The use case
// must not filter the redelivery itself — the applier owns idempotency, and a
// consumer that skipped facts it believed it had seen would be ordering by
// something it does not own.
func TestAReplayedPageHasNoSecondEffect(t *testing.T) {
	world := newWorld()
	world.pages[""] = dataplane.Page{
		Events:     []dataplane.Event{factEvent("req-1", "settled")},
		NextCursor: "cursor-2",
	}
	ingestion := newIngestion(world)

	if _, err := ingestion.Replay(context.Background()); err != nil {
		t.Fatalf("first Replay() error = %v, want nil", err)
	}
	if got, want := len(world.effects), 1; got != want {
		t.Fatalf("effects after the first pass = %d, want %d", got, want)
	}

	world.rewind()
	if _, err := ingestion.Replay(context.Background()); err != nil {
		t.Fatalf("second Replay() error = %v, want nil", err)
	}

	if got, want := len(world.reads), 2; got != want {
		t.Fatalf("reads = %d, want %d: the same range was requested twice", got, want)
	}
	deliveries := 0
	for _, entry := range world.order {
		if entry == "apply:req-1" {
			deliveries++
		}
	}
	if got, want := deliveries, 2; got != want {
		t.Errorf("req-1 was delivered %d time(s), want %d — replay is normal and the applier is what absorbs it", got, want)
	}
	if got, want := len(world.effects), 1; got != want {
		t.Errorf("effects after the replay = %d, want %d: delivering the same fact class twice is a no-op", got, want)
	}
	if got, want := world.position, "cursor-2"; got != want {
		t.Errorf("position = %q, want %q", got, want)
	}
}

// TestTheCursorTravelsVerbatim is the opacity rule at the application level.
// The position is a value that looks nothing like a number — spaces, separators,
// an escape, non-ASCII — and the assertion is byte-for-byte in both directions:
// what the reader was asked for is exactly what the cursor held, and what the
// cursor was advanced to is exactly the page's next_cursor. A trim, a default,
// a shape check or a locally computed position all break this test.
func TestTheCursorTravelsVerbatim(t *testing.T) {
	const position = "  not a number: é {\"a\":1} & =%2F\t"
	const nextCursor = "cursor two / 100% &limit=9 \U0001f680"

	world := newWorld()
	world.position = position
	world.pages[position] = dataplane.Page{
		Events:     []dataplane.Event{factEvent("req-1", "settled")},
		NextCursor: nextCursor,
		HasMore:    false,
	}

	result, err := newIngestion(world).Replay(context.Background())
	if err != nil {
		t.Fatalf("Replay() error = %v, want nil", err)
	}

	if got := world.reads; len(got) != 1 || got[0] != position {
		t.Errorf("the Data Plane was asked for %q, want %q byte for byte", got, position)
	}
	if world.position != nextCursor {
		t.Errorf("position = %q, want the page's next_cursor %q byte for byte", world.position, nextCursor)
	}
	if result.HasMore {
		t.Error("HasMore = true, want false: the page said the feed had nothing after it")
	}
}

// TestHasMoreIsTheFeedsAnswerAndNotThePageLength pins the one distinction a
// paging loop cannot get wrong: a page shorter than the limit does not mean the
// feed is drained, and an empty page does not mean the flow is over. HasMore is
// the Data Plane's answer and nothing here derives it.
func TestHasMoreIsTheFeedsAnswerAndNotThePageLength(t *testing.T) {
	world := newWorld()
	world.pages[""] = dataplane.Page{
		Events:     []dataplane.Event{factEvent("req-1", "settled")},
		NextCursor: "cursor-2",
		HasMore:    true,
	}
	world.pages["cursor-2"] = dataplane.Page{
		Events:     nil,
		NextCursor: "cursor-2",
		HasMore:    false,
	}
	ingestion := newIngestion(world)

	short, err := ingestion.Replay(context.Background())
	if err != nil {
		t.Fatalf("Replay() error = %v, want nil", err)
	}
	if !short.HasMore {
		t.Error("HasMore = false on a short page, want true: a page under the limit has a concurrent writer too, and only the feed says whether more follows")
	}

	caughtUp, err := ingestion.Replay(context.Background())
	if err != nil {
		t.Fatalf("second Replay() error = %v, want nil", err)
	}
	if caughtUp.HasMore {
		t.Error("HasMore = true on an empty page that said the feed was drained, want false")
	}
	if caughtUp.Applied != 0 {
		t.Errorf("Applied = %d on an empty page, want 0", caughtUp.Applied)
	}
	if got, want := world.position, "cursor-2"; got != want {
		t.Errorf("position = %q, want %q: an empty page carries the position the request did", got, want)
	}
}

// TestAnExpiredCursorReachesTheCallerAndOpensNoUnitOfWork is the one failure
// the Control Plane must be able to tell apart from a transport failure. It
// also asserts that the error carries no cursor value: a position in a log line
// is a position in a place nobody audits, and the caller can match the sentinel
// through errors.Is without the value ever being printed.
func TestAnExpiredCursorReachesTheCallerAndOpensNoUnitOfWork(t *testing.T) {
	const position = "a-position-the-feed-no-longer-retains"

	world := newWorld()
	world.position = position
	world.readErr = dataplane.ErrCursorExpired

	_, err := newIngestion(world).Replay(context.Background())
	if !errors.Is(err, dataplane.ErrCursorExpired) {
		t.Fatalf("Replay() error = %v, want it to wrap %v", err, dataplane.ErrCursorExpired)
	}
	if len(world.order) != 0 {
		t.Errorf("the flow observed %v, want nothing: no unit of work is opened for a page that could not be read", world.order)
	}
	if got, want := world.position, position; got != want {
		t.Errorf("position = %q, want %q unchanged", got, want)
	}
	if strings.Contains(err.Error(), position) {
		t.Errorf("the error %q carries the cursor value; an opaque position must not be echoed into a log line", err)
	}
}

// TestAPageWithNoPositionIsRefusedBeforeAnythingIsWritten is the second gate on
// the malformed page, and it is deliberately not a duplicate of the adapter's.
//
// The adapter refuses the wire as it arrives; this asserts the property that
// matters to the durable state the Control Plane owns — a page the flow cannot
// advance from must leave that state exactly as it was. The reader here breaks
// its port's contract and returns such a page anyway, which is the only way to
// reach this path at all, and the pass must refuse it rather than store a
// position that is really "never applied anything": an empty position is
// indistinguishable from the state a consumer starts in, so the flow would
// re-read the same page on every cycle and never advance — a stall with nothing
// to observe and nothing to alert on.
func TestAPageWithNoPositionIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	world := newWorld()
	world.position = "a-position"
	world.pages["a-position"] = dataplane.Page{
		Events:     []dataplane.Event{factEvent("req-1", "settled")},
		NextCursor: "",
	}

	_, err := newIngestion(world).Replay(context.Background())
	if !errors.Is(err, dataplane.ErrMalformedPage) {
		t.Fatalf("Replay() error = %v, want it to wrap %v", err, dataplane.ErrMalformedPage)
	}

	if got, want := world.position, "a-position"; got != want {
		t.Errorf("position = %q, want %q unchanged: a malformed page must not move the consumer's position", got, want)
	}
	if len(world.effects) != 0 {
		t.Errorf("the applier recorded %d effect(s), want 0: a page that cannot be advanced from must not be applied either", len(world.effects))
	}
	if len(world.order) != 0 {
		t.Errorf("the flow observed %v, want nothing: the refusal happens before the unit of work opens", world.order)
	}
}

// TestAMalformedPageLeavesThePositionAndRefetchesTheSameRange is the
// ingestion-level half of the malformed-page invariant: whatever the reader's
// reason for refusing a page — and after the outbound adapter's hardening, a
// 200 whose required fields are absent or null arrives as exactly this error —
// the durable position must not move and nothing may be applied. The second
// pass asks for the same range and, the feed now answering, applies it: a
// refused page delays the flow, it never skips a fact.
func TestAMalformedPageLeavesThePositionAndRefetchesTheSameRange(t *testing.T) {
	const position = "cursor-1"

	world := newWorld()
	world.position = position
	world.readErr = dataplane.ErrMalformedPage

	_, err := newIngestion(world).Replay(context.Background())
	if !errors.Is(err, dataplane.ErrMalformedPage) {
		t.Fatalf("Replay() error = %v, want it to wrap %v", err, dataplane.ErrMalformedPage)
	}
	if got, want := world.position, position; got != want {
		t.Errorf("position = %q, want %q unchanged: a page the reader refused must not move the consumer's position", got, want)
	}
	if len(world.effects) != 0 {
		t.Errorf("the applier recorded %d effect(s), want 0: a refused page is not applied either", len(world.effects))
	}
	if len(world.order) != 0 {
		t.Errorf("the flow observed %v, want nothing: no unit of work is opened for a page that could not be read", world.order)
	}

	// The next pass asks for the same range and gets a page this time, which
	// is the whole point of not advancing: the range is still there to read.
	world.readErr = nil
	world.pages[position] = dataplane.Page{
		Events:     []dataplane.Event{factEvent("req-1", "settled")},
		NextCursor: "cursor-2",
		HasMore:    false,
	}
	result, err := newIngestion(world).Replay(context.Background())
	if err != nil {
		t.Fatalf("second Replay() error = %v, want nil", err)
	}
	if got, want := result.Applied, 1; got != want {
		t.Errorf("Applied = %d, want %d", got, want)
	}
	if got, want := world.position, "cursor-2"; got != want {
		t.Errorf("position = %q, want %q after the valid page", got, want)
	}
	if got, want := world.reads, []string{position, position}; !slices.Equal(got, want) {
		t.Errorf("the Data Plane was asked for %v, want %v", got, want)
	}
}

// TestAFailedPositionReadOpensNoUnitOfWork is the same guard one step earlier:
// if the Control Plane cannot read where it is, nothing else in the flow runs.
func TestAFailedPositionReadOpensNoUnitOfWork(t *testing.T) {
	world := newWorld()
	world.positionErr = errPosition

	_, err := newIngestion(world).Replay(context.Background())
	if !errors.Is(err, errPosition) {
		t.Fatalf("Replay() error = %v, want it to wrap %v", err, errPosition)
	}
	if len(world.reads) != 0 {
		t.Errorf("the Data Plane was asked for %q, want no read: the position was never established", world.reads)
	}
	if len(world.order) != 0 {
		t.Errorf("the flow observed %v, want nothing", world.order)
	}
}

// TestTheFlowNeverAcknowledgesTheDataPlane is ADR 0006 §5's
// no-acknowledgement rule asserted as a property of the seam rather than
// described in prose. Two halves: the port has exactly one method, and it is a
// read — there is no function in this flow that could tell the runtime a fact
// was consumed — and the fake, which is the whole of the Data Plane as far as
// these tests are concerned, saw exactly the reads the flow declares and
// nothing else. A second call appearing in world.reads, or a second method
// appearing on the interface, is the acknowledgement this design has no
// endpoint for.
func TestTheFlowNeverAcknowledgesTheDataPlane(t *testing.T) {
	seam := reflect.TypeOf((*dataplane.UsageFacts)(nil)).Elem()
	if got, want := seam.NumMethod(), 1; got != want {
		t.Fatalf("the usage fact seam has %d method(s), want %d: the facts are durable history and the consumer's position is the consumer's, so the seam has no way to acknowledge anything", got, want)
	}
	if got, want := seam.Method(0).Name, "ReadUsageEvents"; got != want {
		t.Fatalf("the usage fact seam's only method is %s, want %s", got, want)
	}

	world := newWorld()
	world.pages[""] = dataplane.Page{
		Events:     []dataplane.Event{factEvent("req-1", "settled")},
		NextCursor: "cursor-2",
	}

	if _, err := newIngestion(world).Replay(context.Background()); err != nil {
		t.Fatalf("Replay() error = %v, want nil", err)
	}

	if got, want := len(world.reads), 1; got != want {
		t.Errorf("the fake Data Plane saw %d call(s), want %d — the flow's only cross-plane call is the read", got, want)
	}
}
