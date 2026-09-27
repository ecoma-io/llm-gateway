package application

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The reaper's tests. The fakes beside them carry a world where a hold can be
// stranded and a clock can be moved, so what these assert is the reaper's own
// discipline rather than the engine's: what it reclaims, what it declines to
// reclaim, what it tolerates, and what it refuses.
//
// The shape of the argument is deliberately narrow. The reaper is a second
// door into a composition the release already owns, and its only two
// departures from the release's discipline — a finalise that lost the race and
// a duplicate fact — are departures the release could not afford. A test that
// only proved the happy path would not touch either, and the departures are
// the whole risk.

// The reclaim itself: a stranded hold is closed, its capacity is given back,
// its request and its replay record are stated, and an expired fact is the
// last thing written.
func TestReapClosesTheStrandedHoldAndStatesTheEnding(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	f.stranded()

	report, err := f.reaper.Reap(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if report.Scanned != 1 || report.Recovered != 1 || report.Skipped != 0 {
		t.Fatalf("report = %+v, want one victim scanned and recovered with no skip", report)
	}

	// The hold is terminal, and terminal as expired — the sweep's own
	// transition, not a release the reaper improvised.
	reservation := f.reservation(f.hold)
	if reservation.State != accounting.StateExpired {
		t.Fatalf("hold state = %q, want %q", reservation.State, accounting.StateExpired)
	}
	if reservation.ClosedAt.IsZero() {
		t.Fatal("the reclaimed hold has no closing instant; a close with no instant is not a close")
	}

	// The capacity came back to the bucket it left, by exactly the amount the
	// leg carried. This is the return the release also makes and the settle
	// deliberately does not: a reclaimed hold spent nothing.
	if got := f.world.available(reaperTestBucket); got != f.capacity+103 {
		t.Fatalf("bucket available = %d, want %d (the seed plus the returned leg)", got, f.capacity+103)
	}

	// The request is failed through the abandoned door, naming no attempt —
	// which is the schema's own pairing, and the reason the reaper reaches
	// FailAbandoned rather than the release's switch.
	request := f.requestRow(f.request)
	if request.Status != execution.StatusFailed {
		t.Fatalf("request status = %q, want %q", request.Status, execution.StatusFailed)
	}
	if request.FailureReason != execution.FailedGatewayAbandoned {
		t.Fatalf("request failure reason = %q, want %q", request.FailureReason, execution.FailedGatewayAbandoned)
	}
	if request.CommittedAttemptID != "" {
		t.Fatalf("request named attempt %q; an abandoned ending names none, and the schema refuses one", request.CommittedAttemptID)
	}

	// The replay record took the same pointer, so a later replay under this
	// key is answered 409 rather than drawing a second hold.
	record := f.intake(f.account, f.key)
	if record.FinalStatus == nil || *record.FinalStatus != execution.FinalFailed {
		t.Fatalf("replay record final status = %v, want %q", record.FinalStatus, execution.FinalFailed)
	}
	if record.FinalFailureReason != execution.FailedGatewayAbandoned {
		t.Fatalf("replay record failure reason = %q, want %q", record.FinalFailureReason, execution.FailedGatewayAbandoned)
	}
}

// The expired fact is the unit's last statement, and it says what the release's
// does: the hold's own legs, no usage, no amount. The reaper prices nothing —
// the money half of the arc is the other plane's derivation from this fact.
func TestReapAppendsTheExpiredFactWithTheHoldsOwnLegs(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	f.stranded()

	if _, err := f.reaper.Reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if f.factCount() != 1 {
		t.Fatalf("facts = %d, want exactly the one the reclaim published", f.factCount())
	}
	fact := f.factAt(0)
	if fact.Kind != accounting.KindExpired {
		t.Fatalf("fact kind = %q, want %q", fact.Kind, accounting.KindExpired)
	}
	if fact.RequestID != f.request {
		t.Fatalf("fact names request %q, want %q", fact.RequestID, f.request)
	}
	if fact.SettledAmount != nil {
		t.Fatalf("fact carries a settled amount of %d; an expiry spent nothing and the reaper prices nothing", *fact.SettledAmount)
	}
	if fact.PriceRevision != "" || fact.InputUnitPrice != nil || fact.OutputUnitPrice != nil {
		t.Fatalf("fact carries a pricing basis (%q, %v, %v); an expiry has no usage to price", fact.PriceRevision, fact.InputUnitPrice, fact.OutputUnitPrice)
	}
	if fact.CommittedAttemptID != "" {
		t.Fatalf("fact names attempt %q; an expiry names none, and the fact contract refuses one", fact.CommittedAttemptID)
	}
	// The legs travel in the payload, which is the domain's own field, and the
	// reaper does not reach into it. So the reaper's authorship of the tail is
	// witnessed from outside: the capacity that came back came back by exactly
	// the leg the fact carries.
	leg := accounting.Allocation{FundingBucketID: reaperTestBucket, Amount: 103, Ordinal: 1}
	if len(f.world.returned[0]) != 1 || f.world.returned[0][0] != leg {
		t.Fatalf("returned legs = %+v, want %+v verbatim", f.world.returned[0], []accounting.Allocation{leg})
	}
}

// The order of the unit is the order the feed reads facts in, so the append is
// last and a sweep never runs beside it inside the same unit. A drain runs one
// more sweep after its last victim — the one that finds nothing and ends the
// cycle — so the order is read up to the append that completes the unit.
func TestReapWritesTheFactLast(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	f.stranded()

	if _, err := f.reaper.Reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	var order []string
	f.world.mu.Lock()
	for _, event := range f.world.events {
		order = append(order, event)
		if event == "fact.append" {
			break
		}
	}
	f.world.mu.Unlock()

	// A closed hold with no deciding fact behind it is the orphan the doctrine
	// forbids, so every step before the append is load-bearing and the append
	// is the word that completes them. The commit is the unit's, not a step in
	// the tail's order, and the test that follows is the one that counts it.
	want := []string{"begin", "reservation.sweep", "ledger.return", "request.finalise", "intake.finalise", "fact.append"}
	if !slices.Equal(order, want) {
		t.Fatalf("unit order = %v, want %v", order, want)
	}
}

// The whole unit is one unit: a hold may not be closed by one transaction and
// tailed by another, which is the tear ErrExpireOutsideUnitOfWork's own
// documentation describes. The fake refuses a bare sweep, so a reaper that
// swept outside a unit would fail here rather than quietly commit.
func TestReapRunsTheSweepAndItsTailInOneUnitOfWork(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	f.stranded()

	if _, err := f.reaper.Reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if got := f.world.outsideTx; got != 0 {
		t.Fatalf("%d ports were reached outside a unit of work, want none", got)
	}
	// One unit per sweep, every one of them committed. A drain that opened a
	// second unit to tail what a first one swept would show two begins to one
	// commit, and a unit that gave up mid-tail would show a rollback.
	if begins, commits, rollbacks := f.world.count("begin"), f.world.count("commit"), f.world.count("rollback"); begins != commits || rollbacks != 0 {
		t.Fatalf("units: %d began, %d committed, %d rolled back; want every unit committed and no rollback", begins, commits, rollbacks)
	}
	if begins, sweeps := f.world.count("begin"), f.world.count("reservation.sweep"); begins != sweeps {
		t.Fatalf("units opened = %d for %d sweeps; a sweep outside a unit is the tear this guards", begins, sweeps)
	}
}

// An empty queue is a cycle's ordinary end, not a failure, and it must not be
// dressed up as one: a reaper that cannot tell "nothing to do" from "broken"
// is a reaper an operator cannot read.
func TestReapOnAnEmptySweepDoesNothingAndIsNotAFailure(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)

	report, err := f.reaper.Reap(context.Background())
	if err != nil {
		t.Fatalf("reap on an empty queue: %v", err)
	}
	if report != (ReapReport{}) {
		t.Fatalf("report = %+v, want a zero report", report)
	}
	if f.factCount() != 0 {
		t.Fatal("an empty sweep published a fact; there was no hold to state")
	}
}

// A reclaim is idempotent. The second cycle finds the hold terminal, the
// sweep's predicate does not match it again, and no capacity moves twice —
// the property the return's own lack of idempotency makes worth proving.
func TestReapIsIdempotentAcrossCycles(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	f.stranded()

	first, err := f.reaper.Reap(context.Background())
	if err != nil {
		t.Fatalf("first reap: %v", err)
	}
	after := f.world.available(reaperTestBucket)

	second, err := f.reaper.Reap(context.Background())
	if err != nil {
		t.Fatalf("second reap: %v", err)
	}
	if second != (ReapReport{}) {
		t.Fatalf("second report = %+v, want nothing reclaimed the second time", second)
	}
	if got := f.world.available(reaperTestBucket); got != after {
		t.Fatalf("bucket available moved on a second cycle: %d, want %d — a return is not idempotent and must only ever run behind the sweep's close", got, after)
	}
	if f.factCount() != 1 {
		t.Fatalf("facts = %d after two cycles, want the one the first published", f.factCount())
	}
	if first.Recovered != 1 {
		t.Fatalf("first report = %+v, want one recovered", first)
	}
}

// The two horizons. A hold is eligible only when BOTH clocks have run out:
// the lease says the owner died, the window says the caller walked away.
// Either one alone is not a verdict, and a reaper that re-derived eligibility
// from its own clock instead of trusting the sweep's predicate would be
// deciding this on its own authority.
func TestReapRequiresBothClocksToHaveLapsed(t *testing.T) {
	now := time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)

	tests := []struct {
		name         string
		windowOffset time.Duration
		leaseOffset  time.Duration
		wantReaped   bool
	}{
		{"both lapsed", -time.Hour, -time.Hour, true},
		{"window still open, lease lapsed", time.Hour, -time.Hour, false},
		{"window lapsed, lease still held", -time.Hour, time.Hour, false},
		{"both still live", time.Hour, time.Hour, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newReaperFixture(t, 10, time.Second)
			f.world.now = now
			f.world.seedStrandedHold(f.hold, f.request, f.account, f.key, f.legs, tc.windowOffset, tc.leaseOffset)

			report, err := f.reaper.Reap(context.Background())
			if err != nil {
				t.Fatalf("reap: %v", err)
			}
			if reaped := report.Scanned == 1; reaped != tc.wantReaped {
				t.Fatalf("scanned = %d (reaped %v), want reaped %v", report.Scanned, reaped, tc.wantReaped)
			}
			// The capacity is the second witness: a hold the reaper declined
			// is a hold whose capacity is still drawn, and a hold it took is
			// a hold whose capacity is back.
			want := f.capacity
			if tc.wantReaped {
				want += 103
			}
			if got := f.world.available(reaperTestBucket); got != want {
				t.Fatalf("bucket available = %d, want %d", got, want)
			}
		})
	}
}

// The exact boundary. The sweep's predicate is a strict `<` against the
// engine's own clock, so an instant that equals `now` is not yet eligible and
// one nanosecond either side of it is a different answer. The reaper does not
// second-guess this: the sweep decides, and the reaper states what it was
// given. A test that only checked "an hour ago" would not notice a reaper
// that had learned to re-derive the boundary itself, which is the mistake
// this exists to catch.
func TestReapLeavesTheExpiryBoundaryToTheSweep(t *testing.T) {
	now := time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)

	tests := []struct {
		name        string
		offset      time.Duration
		wantReaped  bool
		explanation string
	}{
		{"a nanosecond past", -time.Nanosecond, true, "strictly before now, so the predicate matches"},
		{"exactly now", 0, false, "the predicate is a strict <, so an instant equal to now has not lapsed"},
		{"a nanosecond ahead", time.Nanosecond, false, "after now, so the hold is not eligible"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newReaperFixture(t, 10, time.Second)
			f.world.now = now
			// The window is an hour past so that the lease alone decides, which
			// is the horizon this table is about.
			f.world.seedStrandedHold(f.hold, f.request, f.account, f.key, f.legs, -time.Hour, tc.offset)

			report, err := f.reaper.Reap(context.Background())
			if err != nil {
				t.Fatalf("reap: %v", err)
			}
			if reaped := report.Scanned == 1; reaped != tc.wantReaped {
				t.Fatalf("scanned = %d (reaped %v), want reaped %v: %s", report.Scanned, reaped, tc.wantReaped, tc.explanation)
			}
		})
	}
}

// DEVIATION FROM THE RELEASE, and the first of the two: a finalise that lost
// its race is a skip, not a rollback.
//
// In the release a false is fatal because that unit owns the ending through
// the reservation's compare-and-set. The reaper is a second door, and the
// sweep's exclusive claim is what makes tolerating the first door safe — a
// settled request's hold was already terminal, so the sweep would never have
// selected it. A false can therefore only be another release-class ending
// that got there first, and rolling the unit back over that would reopen
// nothing and re-derive the same victim next cycle.
func TestReapToleratesAFinaliseLostToTheEndingThatWonTheRace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*reapWorld)
	}{
		{"the request row was already written", func(w *reapWorld) { w.requestFinaliseLost = true }},
		{"the replay record was already pointed", func(w *reapWorld) { w.intakeFinaliseLost = true }},
		{"both were already written", func(w *reapWorld) { w.requestFinaliseLost = true; w.intakeFinaliseLost = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReaperFixture(t, 10, time.Second)
			f.stranded()
			tc.setup(f.world)

			report, err := f.reaper.Reap(context.Background())
			if err != nil {
				t.Fatalf("reap: a finalise lost to the ending that won is a skip, not a failure: %v", err)
			}
			if report.Skipped != 1 || report.Recovered != 0 {
				t.Fatalf("report = %+v, want the victim counted as skipped", report)
			}
			// The hold is still reclaimed, which is the point: a skip is not a
			// failure to close, it is a failure to restate what another ending
			// had already stated.
			if got := f.reservation(f.hold).State; got != accounting.StateExpired {
				t.Fatalf("hold state = %q, want %q — a tolerated skip still closes the hold", got, accounting.StateExpired)
			}
			if f.factCount() != 1 {
				t.Fatalf("facts = %d, want the expiry still published: a hold this unit closed must have a word on the feed", f.factCount())
			}
		})
	}
}

// DEVIATION FROM THE RELEASE, and the second: a duplicate fact is "done, not
// an error". The release names that sentinel a bug, and correctly — the
// compare-and-set it holds guarantees exactly one settlement-relevant fact per
// request. The reaper's claim is a different claim on a different table, and
// SKIP LOCKED skips a contended row rather than waiting for it, so a live
// release through the other door can genuinely land this request's fact
// first. The precedent is the orphan tail's, which already reads the sentinel
// as the twin having won.
func TestReapToleratesADuplicateFactAsTheOtherDoorHavingWon(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	f.stranded()
	f.world.factDuplicate = true

	report, err := f.reaper.Reap(context.Background())
	if err != nil {
		t.Fatalf("reap: a duplicate fact means the other door won, not that this one broke: %v", err)
	}
	if report.Skipped != 1 {
		t.Fatalf("report = %+v, want the victim counted as skipped", report)
	}
	if f.factCount() != 0 {
		t.Fatalf("facts = %d, want none appended — the twin's fact is already on the feed", f.factCount())
	}
	// And the reclaim still stands: capacity returned, hold terminal.
	if got := f.world.available(reaperTestBucket); got != f.capacity+103 {
		t.Fatalf("bucket available = %d, want %d — rolling this back would undo a return for no end", got, f.capacity+103)
	}
	if got := f.reservation(f.hold).State; got != accounting.StateExpired {
		t.Fatalf("hold state = %q, want %q", got, accounting.StateExpired)
	}
}

// A contended hold is not a claim. SKIP LOCKED leaves a row another writer
// holds alone, and with a limit of one there is nothing behind it to fall
// through to — so a cycle whose only victim is locked ends with nothing
// reclaimed, and must say so by reporting nothing rather than by failing.
func TestReapReportsNothingWhenTheOnlyHoldIsLockedByAnotherWriter(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	f.stranded()
	f.world.sweepSkipsOne = true

	report, err := f.reaper.Reap(context.Background())
	if err != nil {
		t.Fatalf("reap: a locked row is not a failure: %v", err)
	}
	if report != (ReapReport{}) {
		t.Fatalf("report = %+v, want a zero report", report)
	}
	// The hold is untouched, so the next cycle can still take it.
	if got := f.reservation(f.hold).State; got != accounting.StateOpen {
		t.Fatalf("hold state = %q, want it still %q", got, accounting.StateOpen)
	}
}

// A retryable store failure is retried on the store's own classification, and
// a retry re-runs the sweep rather than replaying the victims the failed
// attempt read. The sweep judges against the engine's clock, so a replayed
// victim list would be a list this cycle no longer has a claim to.
func TestReapRetriesARetryableStoreFailure(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	f.stranded()
	f.world.sweepFailures = 1
	f.world.sweepFailure = persistence.ErrCommitOutcomeUnknown

	report, err := f.reaper.Reap(context.Background())
	if err != nil {
		t.Fatalf("reap: a retryable failure is retried, not returned: %v", err)
	}
	if report.Recovered != 1 {
		t.Fatalf("report = %+v, want the victim recovered on the retry", report)
	}
	// One sweep more than the retry itself: the failed one, the retry that
	// re-derived the victim, and the one that found the queue dry and ended the
	// cycle. A ladder that replayed the first sweep's victims instead would end
	// with the same victim recovered, so the count is the only witness that the
	// retry re-ran the sweep.
	if got := f.world.count("reservation.sweep"); got != 3 {
		t.Fatalf("sweeps = %d, want 3 — the failed one, the retry that re-ran it, and the dry one", got)
	}
}

// A failure the store does not classify as retryable is the reaper's answer,
// not its input. Retrying it would spend the ladder on something the ladder
// cannot fix.
func TestReapReturnsANonRetryableFailureWithoutRetrying(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	f.stranded()
	f.world.sweepFailures = 5
	f.world.sweepFailure = errors.New("the catalog is not the shape this build reads")

	if _, err := f.reaper.Reap(context.Background()); err == nil {
		t.Fatal("reap returned no error for a failure the ladder cannot fix")
	}
	if got := f.world.count("reservation.sweep"); got != 1 {
		t.Fatalf("sweeps = %d, want 1 — a failure outside the retryable classes is not retried", got)
	}
}

// A ladder that never settles leaves the hold open, because every attempt
// rolled its whole unit back, the sweep's close included. A stranded-but-open
// hold is the ending ladder's posture too, and it is the reason a later cycle
// exists.
func TestReapLeavesTheHoldOpenWhenTheLadderIsExhausted(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	f.stranded()
	f.world.sweepFailures = 99
	f.world.sweepFailure = persistence.ErrCommitOutcomeUnknown

	if _, err := f.reaper.Reap(context.Background()); err == nil {
		t.Fatal("reap returned no error after an exhausted ladder")
	}
	if got := f.reservation(f.hold).State; got != accounting.StateOpen {
		t.Fatalf("hold state = %q, want it still %q — a rolled-back unit must not leave a closed hold behind", got, accounting.StateOpen)
	}
	if f.factCount() != 0 {
		t.Fatal("a rolled-back ladder published a fact")
	}
}

// The cycle takes at most its batch size, and a cycle that takes fewer has
// ended for a reason it can name. A drain with no bound is the long
// transaction the sweep's own limit exists to prevent.
func TestReapStopsAtItsBatchSize(t *testing.T) {
	f := newReaperFixture(t, 2, time.Second)
	oldest := f.addHold(1, -5*time.Hour)
	second := f.addHold(2, -4*time.Hour)
	later := f.addHold(3, -3*time.Hour)
	f.addHold(4, -2*time.Hour)
	f.addHold(5, -time.Hour)

	report, err := f.reaper.Reap(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if report.Scanned != 2 {
		t.Fatalf("scanned = %d, want the batch size of 2", report.Scanned)
	}
	// And the two it took are the two that had waited longest, which is the
	// sweep's own order: a cycle that reclaims in some other order would starve
	// the holds whose callers are already gone, which are the ones that will
	// never come back for them.
	for _, id := range []identity.ReservationID{oldest, second} {
		if got := f.reservation(id).State; got != accounting.StateExpired {
			t.Fatalf("hold %q state = %q, want %q — the sweep's order is the reaper's order", id, got, accounting.StateExpired)
		}
	}
	// And the three it did not reach are still open for the next cycle.
	for _, id := range []identity.ReservationID{later, identity.ReservationID("res-reaped-4"), identity.ReservationID("res-reaped-5")} {
		if got := f.reservation(id).State; got != accounting.StateOpen {
			t.Fatalf("hold %q state = %q, want it untouched by a batch that stopped at two", id, got)
		}
	}
}

// The order is the sweep's, and the reaper inherits it rather than imposing
// one: a cycle of one victim that is not the oldest is a starvation bug that
// the batch-size test above could not see, because that cycle drained the
// whole queue.
func TestReapTakesTheOldestLeaseFirst(t *testing.T) {
	f := newReaperFixture(t, 1, time.Second)
	oldest := f.addHold(1, -3*time.Hour)
	f.addHold(2, -2*time.Hour)
	f.addHold(3, -time.Hour)

	report, err := f.reaper.Reap(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if report.Recovered != 1 {
		t.Fatalf("report = %+v, want one victim", report)
	}
	if got := f.reservation(oldest).State; got != accounting.StateExpired {
		t.Fatalf("the oldest lease is %q, want %q — the sweep's order is the reaper's order", got, accounting.StateExpired)
	}
}

// The cycle budget is read BETWEEN iterations, which is the only place a
// reading bounds anything: a drain that checked once on its way out would
// have opened its whole batch under one budget and finished an unbounded
// amount of work after the budget had gone.
func TestReapStopsOnItsCycleBudget(t *testing.T) {
	f := newReaperFixture(t, 100, 60*time.Millisecond)
	for n := 1; n <= 10; n++ {
		f.addHold(n, -time.Duration(n)*time.Hour)
	}
	f.world.sweepLatency = 20 * time.Millisecond

	report, err := f.reaper.Reap(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if report.Scanned == 0 {
		t.Fatal("the budget stopped the cycle before it reclaimed anything; a budget is a bound, not a veto")
	}
	if report.Scanned >= 10 {
		t.Fatalf("scanned = %d, want the budget to have stopped the cycle short of the batch", report.Scanned)
	}
}

// A service handed a unit it did not open would nest its drain inside
// somebody else's ending — the very nesting the reaper's same-unit discipline
// is built to make impossible. The refusal is the routing stage's, in the same
// words and for the same reason.
func TestReapRefusesToRunInsideAUnitOfWork(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	f.stranded()

	ctx := context.WithValue(context.Background(), reapTxKey{}, true)
	report, err := f.reaper.Reap(ctx)
	if err == nil {
		t.Fatal("reap ran inside a unit of work it did not open")
	}
	if !strings.Contains(err.Error(), "inside a unit of work it did not open") {
		t.Fatalf("error = %v, want the refusal to name what it refused", err)
	}
	if report != (ReapReport{}) {
		t.Fatalf("report = %+v, want nothing reclaimed by a refused cycle", report)
	}
	if f.factCount() != 0 {
		t.Fatal("a refused cycle published a fact")
	}
}

// The observation is the operator's instrument, and it distinguishes the two
// shapes an operator cannot otherwise tell apart: the reaper ended this
// request, or the reaper found it already ended.
func TestReapObservesBothOutcomesSeparately(t *testing.T) {
	t.Run("the whole tail", func(t *testing.T) {
		f := newReaperFixture(t, 10, time.Second)
		f.stranded()
		var seen []ReapObservation
		f.reaper.ObserveReap = func(o ReapObservation) { seen = append(seen, o) }

		if _, err := f.reaper.Reap(context.Background()); err != nil {
			t.Fatalf("reap: %v", err)
		}
		if len(seen) != 1 {
			t.Fatalf("observations = %d, want one per reclaimed victim", len(seen))
		}
		if seen[0].Outcome != ReapOutcomeExpired {
			t.Fatalf("outcome = %q, want %q", seen[0].Outcome, ReapOutcomeExpired)
		}
		if seen[0].RequestID != string(f.request) {
			t.Fatalf("observation names request %q, want %q", seen[0].RequestID, f.request)
		}
		if seen[0].UsageEventID == 0 {
			t.Fatal("observation carries no feed position; the fact's own position is the one number an operator can correlate on")
		}
		if seen[0].ReclaimedLegs != 1 {
			t.Fatalf("reclaimed legs = %d, want 1", seen[0].ReclaimedLegs)
		}
		if seen[0].FinalStatus != string(execution.StatusFailed) {
			t.Fatalf("final status = %q, want %q", seen[0].FinalStatus, execution.StatusFailed)
		}
	})

	t.Run("the hold closed and the tail found done", func(t *testing.T) {
		f := newReaperFixture(t, 10, time.Second)
		f.stranded()
		f.world.factDuplicate = true
		var seen []ReapObservation
		f.reaper.ObserveReap = func(o ReapObservation) { seen = append(seen, o) }

		if _, err := f.reaper.Reap(context.Background()); err != nil {
			t.Fatalf("reap: %v", err)
		}
		if len(seen) != 1 {
			t.Fatalf("observations = %d, want the skipped victim still observed", len(seen))
		}
		if seen[0].Outcome != ReapOutcomeSkipped {
			t.Fatalf("outcome = %q, want %q", seen[0].Outcome, ReapOutcomeSkipped)
		}
	})
}

// The observation is optional, and a reaper wired with nothing observing it
// must behave identically. This is the nil-default the close's observer keeps,
// asserted here because a reaper that dereferenced it would only fail in the
// composition that forgot to wire one.
func TestReapRunsWithNoObserverWired(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	f.stranded()

	if _, err := f.reaper.Reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
}

// A port this service was promised and did not get is a wiring defect, and
// the start of the process is a far better place to learn about it than the
// middle of a drain.
func TestNewReaperPanicsOnAWiringDefect(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	store := fakeReapStore{world: f.world}
	reservations := fakeReapReservations{world: f.world}
	ledger := fakeReapLedger{world: f.world}
	requests := fakeReapRequests{world: f.world}
	intakes := fakeReapIntakes{world: f.world}
	facts := fakeReapFacts{world: f.world}
	config := ReaperConfig{BatchSize: 1, CycleBudget: time.Second}

	tests := []struct {
		name string
		call func()
	}{
		{"no store", func() { NewReaper(nil, reservations, ledger, requests, intakes, facts, config) }},
		{"no reservations", func() { NewReaper(store, nil, ledger, requests, intakes, facts, config) }},
		{"no ledger", func() { NewReaper(store, reservations, nil, requests, intakes, facts, config) }},
		{"no requests", func() { NewReaper(store, reservations, ledger, nil, intakes, facts, config) }},
		{"no intakes", func() { NewReaper(store, reservations, ledger, requests, nil, facts, config) }},
		{"no facts", func() { NewReaper(store, reservations, ledger, requests, intakes, nil, config) }},
		{"a batch that can close nothing", func() {
			NewReaper(store, reservations, ledger, requests, intakes, facts, ReaperConfig{BatchSize: 0, CycleBudget: time.Second})
		}},
		{"an unbounded drain", func() {
			NewReaper(store, reservations, ledger, requests, intakes, facts, ReaperConfig{BatchSize: 1, CycleBudget: 0})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("NewReaper did not refuse %s", tc.name)
				}
			}()
			tc.call()
		})
	}
}

// A cancelled context ends the cycle with what it had already reclaimed, not
// with a claim that it reclaimed nothing: each victim's unit is atomic, so
// what is committed is committed and the report says so.
func TestReapReturnsWhatItReclaimedWhenTheCallerIsCutShort(t *testing.T) {
	f := newReaperFixture(t, 10, time.Second)
	f.stranded()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report, err := f.reaper.Reap(ctx)
	if err == nil {
		t.Fatal("reap on a cancelled context returned no error")
	}
	if report != (ReapReport{}) {
		t.Fatalf("report = %+v, want a cycle cancelled before it began to be empty", report)
	}
	if f.factCount() != 0 {
		t.Fatal("a cancelled cycle published a fact")
	}
}
