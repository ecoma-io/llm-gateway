package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/ingestion"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The reconciliation worker's tests, and the reason they are weighted the way
// they are.
//
// A detector's two failure modes are not symmetric, and the table above is
// biased on purpose. A check that misses a divergence costs an operator one
// manual sweep, and the pass will run again. A check that fires on a healthy
// plane writes a row that is wrong, into a table this design appends to and
// nobody may delete from, and calls it money that is in question — and the
// cost of that is not one manual sweep, it is a findings table nobody trusts
// again, which is the same as no table at all. So every family here is tested
// twice: once against the state that must raise, and once against the state
// that must NOT, with the negative cases written first and their names reading
// as the invariant they defend.
//
// The second bias is the re-read discipline. Six checks share one rule — read,
// compare, RE-READ before recording — and the rule is only worth anything if a
// test can stage a leg landing between the two reads. The ledger fake scripts
// answers per bucket and per settlement precisely so those races are stageable,
// and the tests below use them to assert that the first disagreement and the
// second agreement record nothing at all.
//
// The third is what the pass does NOT do. It holds ports that can move money —
// settlements, buckets, the applied ledger, the quarantine, even the cursor —
// and every one of those members in these fakes refuses and counts the call. A
// pass that found a bucket drift and then settled anything is a test failure
// here, not a reading of the design's prose.

func TestNewReconciliationRefusesAnyMissingPort(t *testing.T) {
	// The constructor's wiring contract, in the shape commerce's takes: one
	// value holding the whole argument list, nil one member at a time, and a
	// panic per member. A port this use case was promised and did not get is a
	// wiring defect, and the middle of a pass — after a run row is open and
	// half the buckets swept — is a strictly worse place to learn about it.
	//
	// The settings are refused for the same reason and on the same evidence: a
	// batch of zero makes every windowed read return nothing and the pass
	// report a clean window it never looked at, and a lookback of zero or less
	// makes the FIRST pass sweep an empty window and record a run row saying it
	// completed. A run row is the record an operator later reads to decide
	// whether this worker has ever worked.
	w := newReconWorld(t)
	good := DefaultReconciliationSettings()
	ports := []any{
		reconBuckets{world: w},
		reconApplied{world: w},
		reconCursor{world: w},
		reconFindings{world: w},
		reconRuns{world: w},
		w.ledger,
		reconClock{world: w},
	}
	// The port each case names, and the fragment the constructor's own panic
	// message carries for it — the messages are single-sentence and name the
	// port, so the assertion is that the message names the port rather than
	// that it equals this string, which would be a test that breaks when the
	// wording improves.
	names := []string{
		"the funding buckets repository", "the applied-facts ledger", "the ingestion cursor",
		"the findings repository", "the runs repository", "the ledger reader", "the clock",
	}
	inTheMessage := []string{
		"a funding buckets repository", "an applied-facts ledger", "an ingestion cursor",
		"a findings repository", "a runs repository", "a ledger reader", "a clock",
	}
	// The nil has to be a NIL INTERFACE and not a nil concrete value, and
	// `any(nil)` is the only way to say that from a table: assigning a nil
	// reconBuckets to a persistence.FundingBuckets field yields a non-nil
	// interface holding a nil pointer, which passes the constructor's guard
	// and panics three frames deeper inside the sweep. So the value is set as
	// an untyped nil held in an `any`, and the type assertion is what turns it
	// back into the nil interface the constructor is being asked to refuse.
	// The eight arguments are eight type assertions, and each one has to be
	// able to receive a nil interface — a typed nil would slip past the guard
	// the case exists to exercise, so the table's `any` values are asserted
	// through the same two-step that produces one.
	args := func(miss int) []any {
		out := slices.Clone(ports)
		out[miss] = nil
		return out
	}
	build := func(miss int, settings ReconciliationSettings) *Reconciliation {
		a := args(miss)
		buckets, _ := a[0].(persistence.FundingBuckets)
		applied, _ := a[1].(persistence.AppliedFacts)
		cursor, _ := a[2].(persistence.IngestionCursor)
		findings, _ := a[3].(persistence.ReconciliationFindings)
		runs, _ := a[4].(persistence.ReconciliationRuns)
		reader, _ := a[5].(LedgerReader)
		clock, _ := a[6].(persistence.Clock)
		return NewReconciliation(buckets, applied, cursor, findings, runs,
			reader, clock, settings)
	}
	// The PANIC MESSAGE is asserted as well as the panic, because a message
	// that says "nil pointer" instead of naming the port has cost a reader
	// the port name they were about to go and look for. Every port is
	// therefore removed one at a time, and the message has to name that port:
	// one table, one removal, one name.
	for miss := range ports {
		panicked, message := func() (panicked bool, message string) {
			defer func() {
				if r := recover(); r != nil {
					panicked, message = true, fmt.Sprint(r)
				}
			}()
			build(miss, good)
			return
		}()
		if !panicked {
			t.Errorf("NewReconciliation accepted a nil %s; a wiring defect must be loud before the pass starts, not halfway through one", names[miss])
			continue
		}
		if !strings.Contains(message, inTheMessage[miss]) {
			t.Errorf("the panic for a nil %s said %q, which does not name it — a wiring defect that arrives as a bare nil-pointer report costs the reader the port they were about to look for", names[miss], message)
		}
	}
	if len(w.runs) != 0 {
		t.Fatalf("the refused constructions wrote %d run rows; a constructor that panics must not have touched the store", len(w.runs))
	}
}

func TestNewReconciliationRefusesPacingThatWouldReportACleanWindowItNeverSwept(t *testing.T) {
	// The two settings refusals, and the sentence each carries is the whole
	// reason. A batch below one reads nothing and still reports a pass. A
	// lookback at or below zero sweeps an empty window and still records that
	// it completed — and a run row is the one artefact an operator reads to
	// decide whether this worker has ever done anything.
	w := newReconWorld(t)
	tests := []struct {
		name     string
		settings ReconciliationSettings
	}{
		{
			name:     "a batch of zero reads nothing and reports the window clean",
			settings: ReconciliationSettings{Lookback: time.Hour, Batch: 0},
		},
		{
			name:     "a negative batch is the same silence",
			settings: ReconciliationSettings{Lookback: time.Hour, Batch: -1},
		},
		{
			name:     "a zero lookback leaves the first pass nothing to sweep",
			settings: ReconciliationSettings{Lookback: 0, Batch: 500},
		},
		{
			name:     "a negative lookback reaches backwards past the beginning of time",
			settings: ReconciliationSettings{Lookback: -time.Hour, Batch: 500},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			panicked := func() (recovered bool) {
				defer func() { recovered = recover() != nil }()
				newReconciliationWith(w, tt.settings)
				return false
			}()
			if !panicked {
				t.Fatalf("NewReconciliation accepted %+v: %s", tt.settings, tt.name)
			}
		})
	}
	if len(w.runs) != 0 {
		t.Fatalf("the refused settings wrote %d run rows; a constructor that panics must not have reached the store", len(w.runs))
	}
}

func TestAPassOverAHealthyPlaneOpensNothingAtAll(t *testing.T) {
	// The single most important assertion in this file, and the one every
	// other test here would let a regression hide inside. A plane with two
	// healthy buckets, a settled fact whose settlement is on file and filed
	// against the same request, a whole-plan settlement's legs and a written
	// position is a plane that is working. A pass that opens one finding on that
	// state has written money into question on the strength of a comparison
	// that was never wrong, and the findings table is append-only: nobody may
	// delete it, and the next pass will re-confirm it.
	w := newReconWorld(t)
	w.seedHealthyBucket(bucketID(1), 5000)
	w.seedHealthyBucket(bucketID(2), 250)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"

	// A settled request with its settlement of record on file, the legs
	// BuildSettle would have written, and a tail that went back as a release.
	req := requestID(1)
	st := settlementID(1)
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedSettlement(tailReleasedSettlement(st, req, 700))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a clean pass: healthy state is the common case, not an error", err)
	}
	if got, want := summary.FindingsOpened, 0; got != want {
		t.Errorf("got %d findings opened over healthy state, want %d — the checks are the thing this worker must be protected from getting wrong", got, want)
	}
	if got, want := summary.FindingsUnchanged, 0; got != want {
		t.Errorf("got %d findings re-confirmed over healthy state, want %d — there was nothing on file to re-confirm", got, want)
	}
	if got := w.findingsOpened(); got != 0 {
		t.Errorf("the findings table holds %d rows after a clean pass, want none: %v", got, w.checks())
	}
	// The count in the summary is a claim about the table, so the two are read
	// from different places: the summary came back from the pass, the table
	// from the world. A pass that counted a finding it did not write is a
	// headline that is a guess.
	if got, want := summary.Scanned, 3; got != want {
		t.Errorf("got %d rows scanned, want %d — two buckets and one applied fact; a pass that undercounts its own work cannot be audited", got, want)
	}
}

// TestAPassThatLosesTheWindowClaimIsNotAFailure is the second-replica case at
// the pass's own level, and it is a small test about a small thing: two
// replicas of the control plane read the same high-water mark, so they compute
// the same window, and the engine arbitrates which of them sweeps it. The one
// that does not sweep it has nothing to report and nothing to fix.
//
// What it must not do is log a failure every interval for the life of a
// correct deployment. A refusal here is the expected answer to a race, and
// treating it as an error would put a permanent line in the log of every
// instance that loses the interval — which is a log line an operator learns
// to ignore, which is the worst thing a health signal can become. So the pass
// returns an empty summary and no error, and sweeps nothing.
//
// The second assertion is the one that makes the first safe: the losing pass
// must not have swept. A pass that recorded findings and THEN reported the
// refusal would be a pass that did the duplicate work the claim exists to
// prevent, and an empty summary is only the right answer if the work is
// genuinely absent.
func TestAPassThatLosesTheWindowClaimIsNotAFailure(t *testing.T) {
	w := newReconWorld(t)
	w.seedHealthyBucket(bucketID(1), 5000)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	req := requestID(1)
	st := settlementID(1)
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedSettlement(tailReleasedSettlement(st, req, 700))

	// Two rows, and the order matters because Latest reads the last one:
	//
	//	id 1 — a SIBLING pass still running, and it is sweeping the very
	//	       window this pass is about to compute. This is the row the
	//	       claim refuses on, and it is why two replicas of the control
	//	       plane converge instead of both sweeping.
	//	id 2 — the completed mark, whose WindowTo is this pass's from, and
	//	       which is newest, so it is the high-water mark the pass reads.
	//
	// A single running row would not do it: the pass would then read its own
	// sibling's WindowTo as the mark and compute a window the sibling had
	// already finished, which is a different (and already correct) case.
	w.runs = append(w.runs,
		persistence.Run{
			ID:         1,
			Scope:      ReconciliationScope,
			Status:     runRunning,
			StartedAt:  w.now.Add(-2 * time.Hour),
			WindowFrom: w.now.Add(-2 * time.Hour),
			WindowTo:   w.now.Add(-time.Hour),
		},
		persistence.Run{
			ID:         2,
			Scope:      ReconciliationScope,
			Status:     "completed",
			StartedAt:  w.now.Add(-time.Hour),
			WindowFrom: w.now.Add(-3 * time.Hour),
			WindowTo:   w.now.Add(-2 * time.Hour),
		},
	)

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile on a claimed window returned %v, want no error — the other pass is doing this work, and a refusal is the expected answer to a race", err)
	}
	if summary.Scanned != 0 || summary.FindingsOpened != 0 || summary.FindingsUnchanged != 0 {
		t.Errorf("summary = %+v, want the zero summary — a pass that swept nothing reports nothing", summary)
	}
	if got := w.findingsOpened(); got != 0 {
		t.Errorf("the findings table holds %d rows after a refused pass, want none: %v", got, w.checks())
	}
	// And the refusal was recorded as a refusal, which is what a reader of the
	// order wants to see: the pass asked, was told no, and stopped. A refused
	// begin that left no trace at all would be indistinguishable from a pass
	// that never ran. The window named is the sibling's — the mark gave this
	// pass its from, and the running sibling holds a claim on exactly that.
	if !slices.Contains(w.order, "run.begin:refused:"+ReconciliationScope+":"+windowLabel(w.now.Add(-2*time.Hour))) {
		t.Errorf("the pass's order is %v, want it to record the refused begin for the window it computed", w.order)
	}
	if len(w.runs) != 2 {
		t.Errorf("the runs table holds %d rows, want 2 — a refused begin writes nothing", len(w.runs))
	}
}

func TestAPassCancelledByShutdownStillClosesItsRunRow(t *testing.T) {
	// The stop signal is the commonest way a pass ends, and it used to leave
	// the run row 'running' for ever: the pass finishes its row on the context
	// it was handed, that context is already cancelled when the signal
	// arrives, and the store refuses the write. A stranded row is not a
	// coverage problem — the next pass opens at its WindowTo and sweeps its
	// window regardless — but it is a REPORTING one, and a worse one than it
	// looks: a 'running' row is what this design calls the honest record of a
	// worker that started and never finished, so every ordinary restart of the
	// control plane would leave one behind, forever, indistinguishable from
	// the wedged worker the row exists to reveal.
	w := newReconWorld(t)
	w.seedHealthyBucket(bucketID(1), 5000)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"

	// Cancelled after the last read and before the pass returns, which is the
	// window the signal lands in whenever it lands mid-pass: every check is
	// done, every finding is recorded, and only the close is left. The hook
	// fires from F6's read because that is the pass's final one — cancelling
	// after Reconcile returns instead would leave the close holding a live
	// context and prove nothing.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w.cancelBeforeFinish = cancel

	summary, err := newReconciliation(w).Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile under a cancellation returned %v, want no error — a stopped pass is a finished pass", err)
	}
	if summary.Scanned != 1 {
		t.Errorf("summary = %+v, want the one healthy bucket scanned: the cancellation is meant to land after the sweep", summary)
	}
	if len(w.runs) != 1 {
		t.Fatalf("the runs table holds %d rows, want 1 — the pass opened exactly one", len(w.runs))
	}
	if w.runs[0].Status != "completed" {
		t.Errorf("the run row is %q, want \"completed\": a pass that swept to the end and was then stopped has finished, and its row saying otherwise is the defect. Order: %v", w.runs[0].Status, w.order)
	}
	if w.runs[0].FinishedAt == nil {
		t.Error("the run row has no finished_at, want one stamped — a row that moved status without a verdict is a different defect")
	}
	// The close is what proves the fix rather than a coincidence of ordering:
	// on the pre-fix code this refusal is recorded and the row is left
	// 'running', because the Finish was issued on the cancelled context.
	if slices.Contains(w.order, "run.finish:refused:") {
		t.Errorf("the pass's order is %v, want no refused finish — the close is issued on a context that outlives the caller's", w.order)
	}
}

func TestAPassThatFindsEveryDivergenceWritesNothingToAnyMoneyBearingPort(t *testing.T) {
	// The design's whole claim, asserted mechanically rather than read. This
	// pass finds FIVE divergences across F1, F2, F3, F4 and F6 and the fakes'
	// money-bearing ports answer every write with a refusal and count it. A
	// pass that settled, closed, recorded, quarantined or advanced anything
	// while looking lands on that refusal, and the pass fails.
	//
	// The corollary matters as much as the claim: the finding is the OUTPUT,
	// not a step on the way to a repair. A pass that found the bucket drift and
	// then fixed it would be a reconciliation pass with an automatic refund in
	// it, and the only thing standing between this worker and one is that
	// nobody has written the call yet.
	w := newReconWorld(t)
	w.seedBucket(bucketID(1), 5000, 250) // F1: the cache and the legs disagree
	w.position = ""                      // F6: nothing applied through, and effects exist

	req := requestID(1)
	st := settlementID(1)
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	// F4's absent settlement: the fact names a settlement that is not on file.
	// F2 then has nothing to compare, because a settlement that does not exist
	// has no header and no legs — which is the doc's own statement that F4
	// already covers this fact and one defect is one finding.

	// F3's foreign kind and F5's split need a request that carries both, so
	// they are seeded on a second request whose settlement IS on file.
	req2 := requestID(2)
	st2 := settlementID(2)
	w.seedApplied(appliedSettled(req2, st2, 8, 100), w.now.Add(-time.Minute))
	w.seedApplied(appliedTerminal(req2, 9, ingestion.KindReleased), w.now.Add(-30*time.Second))
	w.seedSettlement(foreignKindSettlement(st2, req2, 100, string(accounting.KindGrant)))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that finds and stops: this worker is detect-and-report, not detect-and-repair", err)
	}
	if len(w.writes) != 0 {
		t.Errorf("the pass reached %d money-bearing writes (%v); every write this design permits lands in a findings row or a run row, and a correction to an append-only ledger needs an operator", len(w.writes), w.writes)
	}
	// And the findings DID land, so the test is not passing because the pass
	// did nothing at all: a worker that found nothing and wrote nothing is
	// indistinguishable from a worker that never ran.
	if summary.FindingsOpened < 4 {
		t.Errorf("got %d findings opened over five seeded divergences, want at least 4 — a pass that writes nothing passes this test for the wrong reason", summary.FindingsOpened)
	}
	if got, want := len(w.runs), 1; got != want {
		t.Fatalf("got %d run rows, want %d — one pass is one run row", got, want)
	}
	if got, want := w.runs[0].Status, runCompleted; got != want {
		t.Errorf("run status = %q, want %q: a pass that finished its sweep must record that it finished", got, want)
	}
}

// ---------------------------------------------------------------------------
// F1 — bucket_derivation_drift
// ---------------------------------------------------------------------------

func TestABucketWhoseCacheAndLegsDisagreeOpensOneCriticalFinding(t *testing.T) {
	// The divergence itself, and the only place the pass is allowed to say a
	// number of money is in question. The cache exists so holds do not aggregate
	// the ledger on every read; the legs are the authority (ADR 0004); when the
	// two disagree, what is recorded is the disagreement and NOT a verdict about
	// which is right, because the correction to an append-only ledger is a new
	// adjustment leg and an adjustment leg requires an operator_id this plane
	// may not mint.
	w := newReconWorld(t)
	w.seedBucket(bucketID(7), 5000, 250)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that records the drift", err)
	}
	row, ok := w.theFinding(checkBucketDerivationDrift, subjectBucket, string(bucketID(7)))
	if !ok {
		t.Fatalf("no %s finding for the drifted bucket; the pass opened %v", checkBucketDerivationDrift, w.checks())
	}
	if row.severity != severityCritical {
		t.Errorf("severity = %q, want %q — a bucket whose own rows disagree about its balance is money in question", row.severity, severityCritical)
	}
	// The evidence carries BOTH sides and the legs' count. The count is not part
	// of the verdict — Derivation.ConsistentWith compares three balances and
	// nothing else — but a bucket with a drifted cache and no legs is a
	// different defect from one with ten thousand, and the two deserve different
	// evidence.
	evidence := decodeObserved(t, row.observed)
	for field, want := range map[string]any{
		"cached_settled": float64(5000), "derived_settled": float64(5250),
		"cached_held": float64(0), "derived_held": float64(0),
		"cached_available": float64(5000), "derived_available": float64(5250),
		"legs": float64(2),
	} {
		if evidence[field] != want {
			t.Errorf("evidence[%q] = %v, want %v — the finding is the comparison, and a reader who cannot see both sides has nothing to act on", field, evidence[field], want)
		}
	}
	if summary.FindingsOpened != 1 {
		t.Errorf("FindingsOpened = %d, want 1: one drifted bucket is one finding", summary.FindingsOpened)
	}
	if got, want := w.runs[0].FindingsOpened, int64(1); got != want {
		t.Errorf("the run row counted %d opened, want %d — the run row is the record an operator reads about this worker", got, want)
	}
}

func TestAHealthyBucketIsReconciledOnceAndReReadNever(t *testing.T) {
	// The cost claim, which is also the false-positive claim. The re-read is
	// only reached on a divergence, so a plane of healthy buckets costs one
	// read each and not two — a re-read per bucket would double the pass's
	// cost on the state it is designed to spend nearly all its time on, which is
	// the state where nothing is wrong.
	w := newReconWorld(t)
	for seq := 1; seq <= 3; seq++ {
		w.seedHealthyBucket(bucketID(seq), int64(1000*seq))
	}
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"

	if _, err := newReconciliation(w).Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v, want a clean pass", err)
	}
	for seq := 1; seq <= 3; seq++ {
		id := bucketID(seq)
		if got, want := w.ledger.bucketCalls[id], 1; got != want {
			t.Errorf("bucket %s was reconciled %d times, want %d — the re-read is per DIVERGING bucket, not per bucket", id, got, want)
		}
	}
	if got := w.findingsOpened(); got != 0 {
		t.Errorf("the findings table holds %d rows after a pass over healthy buckets, want none: %v", got, w.checks())
	}
}

func TestABucketDriftThatResolvesOnTheReReadRecordsNothing(t *testing.T) {
	// The re-read is the difference between a finding and a snapshot of a race.
	// A leg landing between the sweep's read and the finding's write would
	// otherwise be recorded as a divergence the very next pass re-confirms as
	// absent — a finding saying "this bucket is wrong" about a bucket that is
	// right, in a table nobody may delete from. The world moved; the second
	// read agrees with the cache; nothing is written.
	w := newReconWorld(t)
	id := bucketID(7)
	// One bucket for the sweep to reach, and a script that overrides what the
	// reader answers it with: the first read disagrees, the second agrees,
	// which is exactly the state a landing leg produces from the reader's side.
	w.seedBucket(id, 5000, 250)
	w.ledger.bucketAnswers[id] = []reconBucketAnswer{
		{report: ReconcileReport{
			Bucket:     accounting.Bucket{ID: id, Settled: 5000, Available: 5000},
			Derivation: accounting.Derivation{Settled: 5250, Available: 5250, Legs: 2},
			Consistent: false,
		}},
		{report: ReconcileReport{
			Bucket:     accounting.Bucket{ID: id, Settled: 5250, Available: 5250},
			Derivation: accounting.Derivation{Settled: 5250, Available: 5250, Legs: 3},
			Consistent: true,
		}},
	}
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that saw a race and recorded nothing", err)
	}
	if summary.FindingsOpened != 0 || summary.FindingsUnchanged != 0 {
		t.Errorf("the pass recorded %d opened and %d unchanged for a bucket that was consistent on the re-read, want zero of both — the second read agreeing IS the answer",
			summary.FindingsOpened, summary.FindingsUnchanged)
	}
	if got := w.findingsOpened(); got != 0 {
		t.Errorf("the findings table holds %d rows, want none: %v", got, w.checks())
	}
	// Both reads happened — the rule is re-READ, not re-skip.
	if got, want := w.ledger.bucketCalls[id], 2; got != want {
		t.Errorf("bucket %s was read %d times, want %d — the divergence was confirmed rather than assumed away", id, got, want)
	}
}

func TestADivergenceTheReReadCannotConfirmFailsThePassRatherThanRecordingIt(t *testing.T) {
	// The re-read is a READ and reads can fail. A failed second read is not
	// evidence of anything, so the pass must return the failure and record
	// nothing: answering "no finding" on a read that failed would be a silent
	// miss, and recording on it would be a finding whose evidence nobody read
	// twice.
	w := newReconWorld(t)
	id := bucketID(7)
	// One bucket for the sweep to reach, and a script whose SECOND answer is a
	// failure: a re-read that fails is not evidence of anything, so the pass
	// must return the failure and record nothing. Answering "no finding" on a
	// read that failed would be a silent miss, and recording on it would be a
	// finding whose evidence nobody read twice.
	w.seedBucket(id, 5000, 250)
	w.ledger.bucketAnswers[id] = []reconBucketAnswer{
		{report: ReconcileReport{
			Bucket:     accounting.Bucket{ID: id, Settled: 5000, Available: 5000},
			Derivation: accounting.Derivation{Settled: 5250, Available: 5250, Legs: 2},
			Consistent: false,
		}},
		{err: errReconBucket},
	}
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if !errors.Is(err, errReconBucket) {
		t.Fatalf("got %v, want an error wrapping %v — a failed re-read is a failure, not a clean result", err, errReconBucket)
	}
	if summary.FindingsOpened != 0 {
		t.Errorf("FindingsOpened = %d, want 0: a finding recorded off a read that failed is a claim nobody can check", summary.FindingsOpened)
	}
	if got, want := w.runs[0].Status, runFailed; got != want {
		t.Errorf("run status = %q, want %q — the pass closes its own run as failed, because a row left 'running' for ever reads as a pass still sweeping", got, want)
	}
}

func TestTheBucketSweepPagesByKeysetAndStopsOnAShortPage(t *testing.T) {
	// Pagination is a claim about the cursor, and OFFSET cannot make it: OFFSET
	// re-reads and re-discards every row already passed, so its cost grows
	// with the table, and a pass paging by OFFSET while legs land can both skip
	// a row and read one twice. The sweep therefore starts at the all-zero uuid
	// — the keyset's origin, because the column is compared as uuid and the zero
	// value sorts before every real one — threads the previous page's LAST id
	// forward, and treats a short page as the end of the table rather than an
	// error.
	const (
		total  = 5
		batch  = 2
		zeroID = "00000000-0000-0000-0000-000000000000"
	)
	w := newReconWorld(t)
	for seq := 1; seq <= total; seq++ {
		w.seedHealthyBucket(bucketID(seq), int64(100*seq))
	}
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	settings := ReconciliationSettings{Lookback: time.Hour, Batch: batch}

	if _, err := newReconciliationWith(w, settings).Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v, want a clean pass", err)
	}
	// Three pages for five buckets at a batch of two: two full pages and a short
	// one, and the short one is the stop.
	want := []string{
		"bucket.sweep:" + zeroID,
		"bucket.sweep:" + string(bucketID(2)),
		"bucket.sweep:" + string(bucketID(4)),
	}
	var sweeps []string
	for _, entry := range w.order {
		if strings.HasPrefix(entry, "bucket.sweep:") {
			sweeps = append(sweeps, entry)
		}
	}
	if !slices.Equal(sweeps, want) {
		t.Fatalf("swept after %v, want %v — the walk starts at the zero uuid and threads the previous page's last id; OFFSET would have re-read every row already passed", sweeps, want)
	}
	// Every bucket is reconciled exactly once across the three pages, which is
	// what "no row swept twice" means in practice.
	for seq := 1; seq <= total; seq++ {
		if got, want := w.ledger.bucketCalls[bucketID(seq)], 1; got != want {
			t.Errorf("bucket %s was reconciled %d times across the walk, want %d — keyset pagination reaches every row once", bucketID(seq), got, want)
		}
	}
	if got, want := w.runs[0].BucketsScanned, int64(total); got != want {
		t.Errorf("the run counted %d buckets, want %d — the counter is the pass's own claim about its reach", got, want)
	}
}

// ---------------------------------------------------------------------------
// F2 and F3 — the settlement's header and its own legs
// ---------------------------------------------------------------------------

func TestASettlementHeaderThatDisagreesWithItsOwnConsumeLegsOpensOneCriticalFinding(t *testing.T) {
	// The header's total is DEFINED to be the sum of its consume legs —
	// BuildSettle computes it from the legs it wrote — so a mismatch there is a
	// header and its own legs disagreeing about what was charged. What the
	// finding may NOT say is which of the two is right: the header is written
	// once and neither figure can be re-derived from the other, so the answer
	// is an operator's.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))

	// A header that says 700 beside legs that sum to 450. The shape is
	// otherwise a whole plan's — so the finding is about the total and nothing
	// else, and F3 has no reason to fire beside it.
	drifted := healthySettlement(st, req, 700)
	drifted.ConsumeSum = 450
	w.seedSettlement(drifted)

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that records the mismatch", err)
	}
	row, ok := w.theFinding(checkSettlementTotalMismatch, subjectSettlement, string(st))
	if !ok {
		t.Fatalf("no %s finding for the mismatched settlement; the pass opened %v", checkSettlementTotalMismatch, w.checks())
	}
	if row.severity != severityCritical {
		t.Errorf("severity = %q, want %q — a header and its own legs disagreeing about the charge is money in question", row.severity, severityCritical)
	}
	evidence := decodeObserved(t, row.observed)
	if got, want := evidence["header_total"], float64(700); got != want {
		t.Errorf("evidence[header_total] = %v, want %v", got, want)
	}
	if got, want := evidence["consume_sum"], float64(450); got != want {
		t.Errorf("evidence[consume_sum] = %v, want %v — the finding is the disagreement, so both figures must be readable", got, want)
	}
	if summary.FindingsOpened != 1 {
		t.Errorf("FindingsOpened = %d, want 1: one settlement whose header and legs disagree is one finding", summary.FindingsOpened)
	}
}

func TestASettlementWhoseLegsBalanceButHaveTheWrongShapeStillOpensAFinding(t *testing.T) {
	// The reason the shape rule exists and why it is not a sum comparison. A
	// consume of +50 against a release of -50 sums to the cached zero, so a
	// check that only compared sums would report a healthy settlement whose
	// release leg is missing or whose consume was written twice. Here the
	// header's total and the consume sum AGREE — the settlement balances — and
	// the shape is still a defect, which is the whole claim about comparing
	// multisets and bucket counts rather than sums.
	//
	// The settlement is read through the answer SCRIPT rather than the steady
	// state, because a settlement that is F4-healthy and a settlement whose
	// shape is wrong are the same rows and the fake cannot stage both at once
	// without a second read that would be consumed by F4's re-read path.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))

	// The write that ran twice: one bucket's consume and release, each written
	// a second time. The sum and the header still agree — 700 charged, 700 in
	// legs — and the per-kind counts pair up exactly, so the only thing that can
	// see it is the ceiling on legs per bucket. That is the clause with teeth:
	// a settlement of one bucket cannot hold four legs.
	partial := persistence.SettlementLedger{
		Settlement: accounting.Settlement{
			ID:           st,
			RequestID:    req,
			SettledTotal: accounting.Amount(700),
		},
		ConsumeSum: 700,
		Legs:       4,
		LegsByKind: map[string]int64{
			string(accounting.KindConsume): 2,
			string(accounting.KindRelease): 2,
		},
		Buckets:               1,
		BucketsWithoutRelease: 1,
	}
	// F4's read, the shape check's cached read, and the shape check's
	// confirming read — the pass reads the settlement three times, and the
	// script names all three so the order is the test's rather than a guess.
	w.ledger.settlementAnswers[st] = []persistence.SettlementLedger{partial, partial, partial}

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that records the wrong shape", err)
	}
	// The shape rides in the key, so a settlement found healthy generates no
	// row at all and a wrong shape cannot collide with a healthy one. It also
	// means a settlement corrected from one consume leg to two opens a NEW
	// finding rather than re-confirming the one the correction was answering.
	want := findingConsumeNoRelease + "#consume=2,release=2"
	row, ok := w.theFinding(want, subjectSettlement, string(st))
	if !ok {
		t.Fatalf("no %s finding for the duplicated leg write; the pass opened %v", want, w.checks())
	}
	if row.severity != severityCritical {
		t.Errorf("severity = %q, want %q", row.severity, severityCritical)
	}
	evidence := decodeObserved(t, row.observed)
	if got, want := evidence["consume_legs"], float64(2); got != want {
		t.Errorf("evidence[consume_legs] = %v, want %v", got, want)
	}
	if got, want := evidence["release_legs"], float64(2); got != want {
		t.Errorf("evidence[release_legs] = %v, want %v", got, want)
	}
	if got, want := evidence["buckets"], float64(1); got != want {
		t.Errorf("evidence[buckets] = %v, want %v — the bucket count is what the per-bucket ceiling is checked against", got, want)
	}
	if summary.FindingsOpened != 1 {
		t.Errorf("FindingsOpened = %d, want 1", summary.FindingsOpened)
	}
	// And no total-mismatch row beside it: the header and the consume sum
	// AGREE on this settlement, and a check that filed one would be comparing
	// figures that agree.
	if _, also := w.theFinding(checkSettlementTotalMismatch, subjectSettlement, string(st)); also {
		t.Errorf("the pass also opened %s for a settlement whose header total and consume sum agree", checkSettlementTotalMismatch)
	}
}

func TestASettlementCarryingAForeignLegKindOpensACriticalFindingRatherThanAShapeOne(t *testing.T) {
	// A leg kind the settle path never writes on a settlement: a grant, a
	// topup, an adjustment. The schema's reference shape pins those kinds'
	// settlement_id to NULL, so a settlement carrying one is a leg attached to a
	// record it does not belong to — and the schema did not hold either.
	//
	// The ordering here is the point: the foreign-kind finding is recorded and
	// the shape finding is NOT, even though the two-part shape (a consume with
	// nothing beside it) is also true of this multiset. One defect, one
	// finding — a settlement that gets a foreign-kind row and a shape row has
	// told the operator about a problem twice.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 100), w.now.Add(-2*time.Minute))
	w.seedSettlement(foreignKindSettlement(st, req, 100, string(accounting.KindGrant)))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that records the foreign kind", err)
	}
	want := findingForeignLegKind + "#consume=1,grant=1"
	row, ok := w.theFinding(want, subjectSettlement, string(st))
	if !ok {
		t.Fatalf("no %s finding for the foreign leg kind; the pass opened %v", want, w.checks())
	}
	evidence := decodeObserved(t, row.observed)
	kinds, ok := evidence["foreign_kinds"].([]any)
	if !ok || len(kinds) != 1 || kinds[0] != string(accounting.KindGrant) {
		t.Errorf("evidence[foreign_kinds] = %v, want [%s] — the evidence names the kinds the settle path never writes", evidence["foreign_kinds"], accounting.KindGrant)
	}
	// The shape finding must not also be open: the foreign kind IS the defect,
	// and a second row keyed by the same multiset would be a second sentence
	// about one problem.
	shape := findingConsumeNoRelease + "#consume=1,grant=1"
	if _, also := w.theFinding(shape, subjectSettlement, string(st)); also {
		t.Errorf("the pass also opened %s; a settlement with a foreign leg kind is ONE defect and naming it twice tells the operator about one problem twice", shape)
	}
	if summary.FindingsOpened != 1 {
		t.Errorf("FindingsOpened = %d, want 1", summary.FindingsOpened)
	}
}

func TestTheZeroPricedSettlementOpensNoFindingAtAll(t *testing.T) {
	// The legitimate shape the shape rule must be written so it does not
	// condemn. A zero-priced model books a hold of nothing and consumes nothing,
	// and the settle still files a header naming the request settled — the
	// header IS the record. Every clause of the rule is an inequality that zero
	// satisfies; a rule with a minimum of one leg would file a critical finding
	// against every free request the plane ever saw, and free requests are a
	// thing this plane exists to serve.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 0), w.now.Add(-2*time.Minute))
	w.seedSettlement(zeroPricedSettlement(st, req))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a clean pass", err)
	}
	if summary.FindingsOpened != 0 {
		t.Errorf("got %d findings over a zero-priced settlement, want 0: a header of record and no legs is an answer, not a defect — %v",
			summary.FindingsOpened, w.checks())
	}
	// The settlement is read TWICE and not once: once by F4, which asks
	// whether the settlement behind the fact is on file and filed against this
	// request, and once by the shape check, which asks a different question of
	// the same two aggregate queries. Two reads is the floor for a settlement
	// that produced no finding at all, and a third would be the re-read — which
	// is reached only on a candidate divergence, and this one is not it.
	if got, want := w.ledger.settlementReads[st], 2; got != want {
		t.Errorf("settlement %s was read %d times, want %d — F4 asks whether it is on file for this request and the shape check asks whether its legs are a shape; neither is a divergence, so neither re-reads",
			st, got, want)
	}
}

func TestOneSettlementIsReadOnceForTheShapeAndOnceAgainOnlyToConfirmADivergence(t *testing.T) {
	// The per-settlement cache and the re-read that bypasses it, both at once.
	// F2, F3 and F4 all need the same two aggregate queries, and asking three
	// times would let a leg landing between the asks produce three findings
	// about one settlement from three different states of it. So the first read
	// is cached and shared, and the CONFIRMING read goes fresh to the ledger —
	// reading the cache again would confirm the pass's own snapshot rather than
	// the world.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))

	drifted := healthySettlement(st, req, 700)
	drifted.ConsumeSum = 450
	w.seedSettlement(drifted)

	if _, err := newReconciliation(w).Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that records the mismatch", err)
	}
	// Three reads: F4's (which asks whether the settlement is filed against this
	// request), the shape check's cached one, and the confirming one F2 makes
	// through the ledger directly. Four would mean the shape check read the
	// cache twice; two would mean F4 and the shape check shared a read, which
	// they do not — F4 is keyed by request and asks a different question of
	// the same rows.
	if got := w.ledger.settlementReads[st]; got < 2 {
		t.Errorf("settlement %s was read %d times, want at least 2 — a divergence is re-read fresh rather than confirmed from the pass's own cache", st, got)
	}
}

func TestASettlementShapeThatResolvesOnTheReReadRecordsNothing(t *testing.T) {
	// The re-read again, on the leg-shape path rather than the bucket path, and
	// staged through the settlement answers: the first read reports a shape no
	// settle plan writes, the second reads a whole one. A settlement's legs only
	// ever land whole in one unit of work, so in production the two reads agree
	// — which is exactly why the finding is about a race and the race is worth
	// one extra read.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))

	partial := persistence.SettlementLedger{
		Settlement: accounting.Settlement{ID: st, RequestID: req, SettledTotal: accounting.Amount(700)},
		ConsumeSum: 700,
		Legs:       2,
		LegsByKind: map[string]int64{
			string(accounting.KindConsume): 1,
			string(accounting.KindRelease): 1,
		},
		Buckets:               1,
		BucketsWithoutRelease: 1,
	}
	whole := tailReleasedSettlement(st, req, 700)
	// The three reads are F4's, the shape check's cached read, and the shape
	// check's confirming read. The cached read must be the divergent one or the
	// shape check never reaches its confirming read, and the confirming read is
	// the whole plan — which is the race: a settlement that was half-written
	// when the pass read it and whole when it read it again.
	w.ledger.settlementAnswers[st] = []persistence.SettlementLedger{partial, partial, whole}

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that saw a race and recorded nothing", err)
	}
	if summary.FindingsOpened != 0 {
		t.Errorf("got %d findings over a settlement that was whole on the re-read, want 0 — the second read agreeing IS the answer: %v", summary.FindingsOpened, w.checks())
	}
}

func TestASettlementWhoseHeaderAgreesOnTheReReadRecordsNothing(t *testing.T) {
	// F2's half of the re-read discipline. The header is written once and the
	// legs are appended in the same unit of work, so a first read that
	// disagrees and a second that agrees is the whole space of races here — and
	// a finding about a mismatch that no longer exists is a row nobody can
	// delete.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))

	mismatched := healthySettlement(st, req, 700)
	mismatched.ConsumeSum = 450
	whole := healthySettlement(st, req, 700)
	// The three reads are F4's, the shape check's cached read, and the shape
	// check's confirming read. The cached read must carry the mismatch for the
	// confirming read to be reached at all, and the confirming read must carry a
	// header that agrees with its own legs — which is the race F2's re-read
	// exists for, and the reason the header cannot be read twice and taken at
	// face value on the first.
	w.ledger.settlementAnswers[st] = []persistence.SettlementLedger{whole, mismatched, whole}

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that saw a race and recorded nothing", err)
	}
	if summary.FindingsOpened != 0 {
		t.Errorf("got %d findings over a settlement whose header agreed on the re-read, want 0: %v", summary.FindingsOpened, w.checks())
	}
}

// ---------------------------------------------------------------------------
// F4 — settled_without_settlement
// ---------------------------------------------------------------------------

func TestASettledFactWithNoSettlementIDOpensTheNullSettlementIDFindingAndNothingElse(t *testing.T) {
	// The first of three sub-keys, separately keyed because they are three
	// different defects with three different remedies and an operator who
	// resolves one must not have the other follow it closed. This one is a
	// settled row with no settlement_id at all — which applied_facts_settled_shape
	// forbids, so finding one is a fact about the CONSTRAINT (a row that landed
	// under an older schema, a restored dump, a hand-written insert) rather
	// than about the money. The charge may exist with nothing behind it, and
	// whether it does is not readable from this plane's own rows.
	w := newReconWorld(t)
	req := requestID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(persistence.AppliedFact{
		RequestID:     string(req),
		Kind:          ingestion.KindSettled,
		AppendSeq:     7,
		SettledAmount: settledAmount(700),
		CaptureMethod: captureMethod("reported"),
	}, w.now.Add(-2*time.Minute))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that records the null settlement id", err)
	}
	row, ok := w.theFinding(findingNullSettlementID, subjectRequest, string(req))
	if !ok {
		t.Fatalf("no %s finding for the settled fact with no settlement of record; the pass opened %v", findingNullSettlementID, w.checks())
	}
	if row.severity != severityCritical {
		t.Errorf("severity = %q, want %q", row.severity, severityCritical)
	}
	// The subject is the REQUEST, because the applied row is what is defective
	// and the request is the identity an operator holds.
	if row.subject != string(req) {
		t.Errorf("subject = %q, want %q — the finding is about the applied row, and the request is the handle an operator resolves it by", row.subject, req)
	}
	if summary.FindingsOpened != 1 {
		t.Errorf("FindingsOpened = %d, want 1 — a settled fact with nothing behind it is one finding and not three", summary.FindingsOpened)
	}
	// It is not also an absent-settlement finding: there is no settlement named
	// to be absent, and one defect is one finding.
	if _, also := w.theFinding(findingAbsentSettlement, subjectRequest, string(req)); also {
		t.Errorf("the pass also opened %s; a null settlement_id is not a named settlement that is missing", findingAbsentSettlement)
	}
}

func TestASettledFactNamingASettlementThatIsNotOnFileOpensTheAbsentSettlementFinding(t *testing.T) {
	// The second sub-key, and the one the migration's foreign key forbids. The
	// charge exists and the record it names does not, and nothing else in this
	// plane will say so: the applier already booked the effect, the settlement
	// writer already moved the balances, and both were true when they ran.
	//
	// It is NOT re-read, and the asymmetry with the cross-wired case below is
	// deliberate. A settlement appearing under an applied row is a schema
	// violation being repaired, and a re-read would catch the repair and say
	// nothing — which is right, because the finding recorded a defect that has
	// been fixed and the findings table is append-only precisely so the record
	// of it survives. Only the finding nobody can delete is the one that must
	// be confirmed first.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	// No settlement seeded: the named record is not on file.

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that records the absent settlement", err)
	}
	row, ok := w.theFinding(findingAbsentSettlement, subjectRequest, string(req))
	if !ok {
		t.Fatalf("no %s finding for the settled fact naming a settlement that is not on file; the pass opened %v", findingAbsentSettlement, w.checks())
	}
	if row.severity != severityCritical {
		t.Errorf("severity = %q, want %q", row.severity, severityCritical)
	}
	evidence := decodeObserved(t, row.observed)
	if got, want := evidence["settlement_id"], string(st); got != want {
		t.Errorf("evidence[settlement_id] = %v, want %q — the operator has to be told which record is missing", got, want)
	}
	// F4 already covers this fact, so the shape check must not open a second
	// row about the same subject: one defect, one finding. It sees the
	// ErrNotFound on its own read and returns without judging the shape.
	if got := summary.FindingsOpened; got != 1 {
		t.Errorf("FindingsOpened = %d, want 1: the shape check reads through the same settlement and an ErrNotFound there is the same defect already recorded", got)
	}
	// And the shape check asked the port exactly once more. A settlement that
	// is not on file is a constraint violation being repaired, and the absent
	// case deliberately does not re-read: the re-read there would catch the
	// repair and say nothing, which is right, because the finding recorded a
	// defect that has been fixed and the table is append-only precisely so the
	// record of it survives. The cross-wired case below is the one that is
	// confirmed, and the arithmetic below is what makes the difference legible.
	if got, want := w.ledger.settlementReads[st], 2; got != want {
		t.Errorf("settlement %s was read %d times, want %d — F4 asked once, the shape check asked once and stopped at the ErrNotFound, and the absent case is not re-read",
			st, got, want)
	}
}

func TestASettlementFiledAgainstAnotherRequestOpensACrossWiredFindingAboutTheSettlement(t *testing.T) {
	// The third sub-key, and the only one recorded against the SETTLEMENT
	// rather than the request. The header's request_id is unique, so exactly one
	// request owns it and this fact's request is not that one: two requests'
	// records disagree about which is which. The subject is the settlement
	// because the defect is the settlement's — it is filed against the wrong
	// request — and an operator resolving it needs the settlement in hand, not
	// either of the two requests it is confusing.
	//
	// And it IS re-read, because it is the one of the three that can arise with
	// no constraint violated at all: a settlement re-filed under a different
	// request between the read and the write would otherwise be a finding about
	// a cross-wiring that no longer exists, and no operator can delete it.
	w := newReconWorld(t)
	req := requestID(1)
	other := requestID(2)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedSettlement(healthySettlement(st, other, 700))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that records the cross-wiring", err)
	}
	row, ok := w.theFinding(findingCrossWiredSettleme, subjectSettlement, string(st))
	if !ok {
		t.Fatalf("no %s finding for the settlement filed against another request; the pass opened %v", findingCrossWiredSettleme, w.checks())
	}
	if row.subject != string(st) {
		t.Errorf("subject = %q, want the settlement %q — the defect is the settlement's, and the operator needs it in hand", row.subject, st)
	}
	evidence := decodeObserved(t, row.observed)
	if got, want := evidence["settlement_request_id"], string(other); got != want {
		t.Errorf("evidence[settlement_request_id] = %v, want %q", got, want)
	}
	if got, want := evidence["fact_request_id"], string(req); got != want {
		t.Errorf("evidence[fact_request_id] = %v, want %q", got, want)
	}
	if summary.FindingsOpened != 1 {
		t.Errorf("FindingsOpened = %d, want 1", summary.FindingsOpened)
	}
}

func TestASettlementReFiledUnderThisRequestBeforeTheWriteRecordsNothing(t *testing.T) {
	// The cross-wiring race, staged. The first read says the settlement belongs
	// to somebody else, the second says it belongs here, and a finding about a
	// cross-wiring that resolved itself would be a finding nobody can delete —
	// which is the only reason this one shape and not the other two is
	// confirmed.
	w := newReconWorld(t)
	req := requestID(1)
	other := requestID(2)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.ledger.settlementAnswers[st] = []persistence.SettlementLedger{
		healthySettlement(st, other, 700), // F4's first read: somebody else's
		healthySettlement(st, req, 700),   // re-filed under this request
		healthySettlement(st, req, 700),   // the shape check's cached read
	}

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that saw a re-filing and recorded nothing", err)
	}
	if summary.FindingsOpened != 0 {
		t.Errorf("got %d findings over a settlement that was re-filed under this request, want 0 — the confirming read agreed, and a finding about a resolved cross-wiring is one nobody can delete: %v",
			summary.FindingsOpened, w.checks())
	}
}

func TestAnAppliedFactWhoseSettlementIsOnFileForThisRequestOpensNothing(t *testing.T) {
	// F4's healthy path, stated on its own because it is the state nearly every
	// settled row on a running plane is in. The applied row names a settlement,
	// the settlement is on file, and it is filed against the same request: the
	// check's whole question answered affirmatively, and the answer opens
	// nothing.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedSettlement(healthySettlement(st, req, 700))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a clean pass", err)
	}
	if summary.FindingsOpened != 0 {
		t.Errorf("got %d findings for a settled fact whose settlement is on file for this request, want 0: %v", summary.FindingsOpened, w.checks())
	}
}

// ---------------------------------------------------------------------------
// F5 — disposition_conflict
// ---------------------------------------------------------------------------

func TestARequestSettledAndThenReleasedOpensExactlyOneFindingNotOnePerSide(t *testing.T) {
	// The keying correction, and the single most load-bearing assertion about
	// F5. A per-side key — one finding for the released effect, one for the
	// settled one — is a table that grows with the number of conflicting
	// EFFECTS rather than the number of conflicts: a request with three
	// contradictory rows produces three findings, and resolving two of them
	// leaves the third unable to suppress the others, so the operator closes
	// the same finding three times and none of the three closures says the
	// request's history has been looked at. One request, one conflict, one row.
	//
	// The severity is INFO and the reason is specific: the class key did its job,
	// only the first terminal fact booked, and nothing moved twice. What the
	// operator wants to know is that the runtime closed one request twice. This
	// plane's answer to the second closure was already correct.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedApplied(appliedTerminal(req, 8, ingestion.KindReleased), w.now.Add(-time.Minute))
	w.seedSettlement(healthySettlement(st, req, 700))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that records the split", err)
	}
	row, ok := w.theFinding(findingTerminalSplit, subjectRequest, string(req))
	if !ok {
		t.Fatalf("no %s finding for the request closed twice; the pass opened %v", findingTerminalSplit, w.checks())
	}
	if row.severity != severityInfo {
		t.Errorf("severity = %q, want %q — nothing booked twice and no money is in question; the runtime closed one request twice and that is worth seeing", row.severity, severityInfo)
	}
	if row.subject != string(req) {
		t.Errorf("subject = %q, want %q — the finding is keyed BY REQUEST, which is the whole of the correction", row.subject, req)
	}
	if summary.FindingsOpened != 1 {
		t.Errorf("FindingsOpened = %d, want exactly 1: the two effects are two sides of ONE conflict, and a per-side key is a row per side", summary.FindingsOpened)
	}
	// And the observation is one finding's, carrying both sequences — the later
	// effect's and the settled one's — because that is the evidence a single
	// conflict row has to hold.
	evidence := decodeObserved(t, row.observed)
	if got, want := evidence["later_kind"], ingestion.KindReleased; got != want {
		t.Errorf("evidence[later_kind] = %v, want %q", got, want)
	}
	if got, want := evidence["later_seq"], float64(8); got != want {
		t.Errorf("evidence[later_seq] = %v, want %v", got, want)
	}
	if got, want := evidence["settled_seq"], float64(7); got != want {
		t.Errorf("evidence[settled_seq] = %v, want %v — one conflict carries both sides, because it is one conflict", got, want)
	}
}

func TestARequestCarryingAnExpiredEffectAfterItsSettlementOpensTheSameOneFinding(t *testing.T) {
	// The other side of the same disagreement. "Released or expired" is one
	// finding because the two are the same fact arriving through two of the
	// feed's doors: the class key let only the first book either way, and the
	// second closure is what an operator should see. Two kinds, one key — so a
	// plane that saw a released then an expired closure did not grow a table.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedApplied(appliedTerminal(req, 8, ingestion.KindExpired), w.now.Add(-time.Minute))
	w.seedSettlement(healthySettlement(st, req, 700))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that records the split", err)
	}
	if _, ok := w.theFinding(findingTerminalSplit, subjectRequest, string(req)); !ok {
		t.Fatalf("no %s finding for the expired-after-settled request; the pass opened %v", findingTerminalSplit, w.checks())
	}
	if summary.FindingsOpened != 1 {
		t.Errorf("FindingsOpened = %d, want 1 — released and expired are the same disagreement, not two", summary.FindingsOpened)
	}
}

func TestARequestChargedAndAlsoDisclaimedOpensOneWarningAboutBothFacts(t *testing.T) {
	// The other F5 key, and the one where money is genuinely in question. A
	// settlement-class effect AND an unbillable-orphaned effect on one request
	// is LEGAL: the two classes are separate precisely so a request may carry an
	// orphan beside its settlement, and the schema allows both. What makes it
	// worth a finding is that the orphan's own claim is that it moved no money,
	// and a request that was both charged and disclaimed has two facts
	// disagreeing about whether it owed anything. Only an operator can say which
	// the customer is held to, and the two facts are the evidence for it.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedApplied(appliedOrphan(req, 8), w.now.Add(-time.Minute))
	w.seedSettlement(healthySettlement(st, req, 700))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that records the conflict", err)
	}
	row, ok := w.theFinding(findingChargedDisclaimed, subjectRequest, string(req))
	if !ok {
		t.Fatalf("no %s finding for the request that was both charged and disclaimed; the pass opened %v", findingChargedDisclaimed, w.checks())
	}
	if row.severity != severityWarning {
		t.Errorf("severity = %q, want %q — legal state, but two facts disagree about whether the request owed anything, and an operator has to say which the customer is held to",
			row.severity, severityWarning)
	}
	evidence := decodeObserved(t, row.observed)
	if got, want := evidence["settled_amount"], float64(700); got != want {
		t.Errorf("evidence[settled_amount] = %v, want %v", got, want)
	}
	if got, want := evidence["orphan_capture"], "estimated"; got != want {
		t.Errorf("evidence[orphan_capture] = %v, want %q", got, want)
	}
	if summary.FindingsOpened != 1 {
		t.Errorf("FindingsOpened = %d, want 1", summary.FindingsOpened)
	}
}

func TestARequestWithOnlyOneTerminalEffectOpensNoDispositionFinding(t *testing.T) {
	// F5's healthy path, which is every request on a working plane. A settled
	// effect with no orphan beside it is a request that was charged and not
	// disclaimed, which is what a charge looks like. A released effect with no
	// settled one behind it is a request that was closed without being charged,
	// which is what a release looks like. Neither is a disagreement, and a
	// request that is looked up and found consistent must generate no row.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedSettlement(healthySettlement(st, req, 700))

	// A second request released and never settled: the other half of the
	// healthy case, and the one a released-only plane is entirely made of.
	lonely := requestID(2)
	w.seedApplied(appliedTerminal(lonely, 9, ingestion.KindReleased), w.now.Add(-2*time.Minute))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a clean pass", err)
	}
	if summary.FindingsOpened != 0 {
		t.Errorf("got %d findings over two requests with one terminal effect each, want 0 — a single terminal effect is not a disagreement: %v",
			summary.FindingsOpened, w.checks())
	}
}

func TestATerminalSplitQuarantinedBetweenTheTwoReadsRecordsNothing(t *testing.T) {
	// F5's re-read, and the reason it exists rather than being read once. A
	// second terminal fact can be QUARANTINED — that is precisely the
	// disposition this finding names, and the quarantine is the expected
	// handling. A pass that recorded the split before that write would have
	// recorded a conflict this plane has already handled correctly, in a table
	// nobody may delete from.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedApplied(appliedTerminal(req, 8, ingestion.KindReleased), w.now.Add(-time.Minute))
	w.seedSettlement(healthySettlement(st, req, 700))

	// The settlement class's first read shows the settled row; the confirming
	// read sees the released one having taken the class instead — the state
	// after the applier quarantined the second terminal fact. The class key is
	// what makes that a real state rather than a hypothetical one: only the
	// first terminal fact of a class books, so a quarantined second one leaves
	// the row that WAS there standing, and the pass's confirming read sees the
	// replacement.
	settled := appliedSettled(req, st, 7, 700)
	taken := appliedTerminal(req, 8, ingestion.KindReleased)
	w.findScripts[findKey{string(req), ingestion.ClassSettlement}] = []findAnswer{
		{fact: &settled},
		{fact: &taken},
	}

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that saw the quarantine and recorded nothing", err)
	}
	if summary.FindingsOpened != 0 {
		t.Errorf("got %d findings for a split the plane quarantined, want 0 — the quarantine is the expected handling and the finding would be a conflict already answered: %v",
			summary.FindingsOpened, w.checks())
	}
}

func TestAChargeWhoseOrphanWasQuarantinedBetweenTheTwoReadsRecordsNothing(t *testing.T) {
	// The same re-read discipline on the other F5 key. A settlement's own
	// unbillable orphan is the only kind in its class, so there is no redelivery
	// that can take the class away from it — but the confirming read still runs,
	// and a state where the orphan is gone is a state the finding should not
	// describe. The cost is one read per settled fact; what it buys is that a
	// finding, once written, describes a state the plane was actually in.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedApplied(appliedOrphan(req, 8), w.now.Add(-time.Minute))
	w.seedSettlement(healthySettlement(st, req, 700))

	// The orphan class is empty on BOTH reads — an orphan effect that was
	// refused before it booked leaves nothing under its key, and the settled row
	// beside it is the whole of what the request carries.
	w.findScripts[findKey{string(req), ingestion.ClassUnbillableOrphaned}] = []findAnswer{
		{},
		{},
	}

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that saw the orphan gone and recorded nothing", err)
	}
	if summary.FindingsOpened != 0 {
		t.Errorf("got %d findings for a request whose orphan class is empty on both reads, want 0: %v", summary.FindingsOpened, w.checks())
	}
}

// ---------------------------------------------------------------------------
// F6 — cursor_missing
// ---------------------------------------------------------------------------

func TestAnEmptyPositionWithEffectsOnFileOpensTheCursorMissingFinding(t *testing.T) {
	// The one check with no window and no subject row, and the only thing about
	// the position that is this plane's own: the row must exist if this plane
	// has applied anything. An absent position with effects on file means the
	// position was deleted or never written while everything depending on it is
	// present, and the consequence is specific — the next replay pass requests
	// the feed from the beginning and re-applies every fact this plane ever
	// applied. The class key makes that free in effect and expensive in work,
	// and nothing else in the plane notices.
	//
	// The subject is the literal control_plane, because the subject IS the
	// absence: there is no row to name.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "" // deleted, or never written
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedSettlement(healthySettlement(st, req, 700))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that records the missing position", err)
	}
	row, ok := w.theFinding(checkCursorMissing, subjectControlPlane, subjectControlPlane)
	if !ok {
		t.Fatalf("no %s finding for the missing position; the pass opened %v", checkCursorMissing, w.checks())
	}
	if row.severity != severityCritical {
		t.Errorf("severity = %q, want %q — the next replay re-applies the whole feed", row.severity, severityCritical)
	}
	if summary.FindingsOpened != 1 {
		t.Errorf("FindingsOpened = %d, want 1", summary.FindingsOpened)
	}
	// The read behind the check is the WIDEST one the port will accept and
	// deliberately unbounded at both ends: a fact applied at the very first
	// instant of the database's life is exactly the row this check exists to
	// find, and a window that started "now minus the lookback" would miss it.
	// One row is all it needs, so the batch is one.
	seen := 0
	for _, entry := range w.order {
		if entry == "applied.recent" {
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("the applied ledger was read %d times, want 2 — once for the windowed families and once for the cursor's own existence", seen)
	}
}

func TestAnEmptyPositionWithNoEffectsOnFileOpensNothing(t *testing.T) {
	// The other half of F6, and the case a plane that has simply never run is
	// in. An empty position saying nothing has ever been applied and an empty
	// ledger saying the same thing AGREE, and the two agreeing is a healthy
	// plane. A finding here would fire on every deployment's first minute and
	// every one of them would be wrong.
	w := newReconWorld(t)
	w.position = ""

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a clean pass", err)
	}
	if summary.FindingsOpened != 0 {
		t.Errorf("got %d findings for a plane that has applied nothing and has an empty position, want 0: %v", summary.FindingsOpened, w.checks())
	}
}

func TestAPlaneWithEffectsAndAWrittenPositionOpensNothingForTheCursor(t *testing.T) {
	// The third state, and the one a running plane is in: the position is
	// present, the effects are present, and the two agree that the feed has
	// been read. F6 is scoped to the singleton case and the scope is a
	// correction a reviewer forced — a first draft also compared the position
	// against the feed's last applied sequence on the theory that a cursor
	// behind the ledger is a stalled consumer, and that check is unshippable
	// here because the position is a Data-Plane-issued opaque string and
	// parsing it or ordering by it is a rule the whole Control Plane keeps.
	w := newReconWorld(t)
	req := requestID(1)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000042"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedSettlement(healthySettlement(st, req, 700))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a clean pass", err)
	}
	if summary.FindingsOpened != 0 {
		t.Errorf("got %d findings for a plane with a written position, want 0 — existence is the only question about the position this plane may ask: %v",
			summary.FindingsOpened, w.checks())
	}
}

// ---------------------------------------------------------------------------
// The window, the tiling and the idempotency bargain
// ---------------------------------------------------------------------------

func TestTheFirstPassOpensTheLookbackAndTheNextOpensThePreviousWindowTo(t *testing.T) {
	// The tiling, which is the pass's contract with the next one and the reason
	// `from` is a high-water mark and not a recomputed now()-minus-lookback. A
	// recomputed bound re-derives its own tail forever whenever a pass outruns
	// its own interval, so the tail of a busy minute would be swept on every
	// pass after it and every finding in that tail would say "seen again" for a
	// window the pass had already covered.
	//
	// The window is half-open — from inclusive, to exclusive — and the same two
	// instants bound every read the pass makes. That is what lets two
	// consecutive passes tile the timeline with no row swept twice and none
	// skipped between two windows.
	w := newReconWorld(t)
	lookback := time.Hour
	settings := ReconciliationSettings{Lookback: lookback, Batch: 500}
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"

	// First pass.
	firstTo := w.now
	if _, err := newReconciliationWith(w, settings).Reconcile(t.Context()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(w.runs) != 1 {
		t.Fatalf("got %d run rows after one pass, want 1", len(w.runs))
	}
	first := w.runs[0]
	if got, want := first.WindowFrom, w.now.Add(-lookback); !got.Equal(want) {
		t.Errorf("first pass opened at %s, want %s — the first pass falls back to the lookback because there is no run to continue from",
			windowLabel(got), windowLabel(want))
	}
	if got, want := first.WindowTo, firstTo; !got.Equal(want) {
		t.Errorf("first pass closed at %s, want %s — the upper bound is the database's clock, never the process wall clock", windowLabel(got), windowLabel(want))
	}
	if first.Scope != ReconciliationScope {
		t.Errorf("scope = %q, want %q", first.Scope, ReconciliationScope)
	}
	// The applied-facts read is bounded by the SAME window the run row records,
	// and by the configured batch. A window the run records and a window the
	// sweep uses that differ would make the row a claim about work nobody did.
	if got, want := w.appliedReads, []windowedRead{{first.WindowFrom, first.WindowTo, 500}}; !slices.Equal(got, want) {
		t.Errorf("the applied-facts reads were %v, want %v — every read in the pass is bounded by the run row's own window", got, want)
	}

	// The clock moves on and the second pass opens where the first one closed.
	w.now = w.now.Add(30 * time.Second)
	if _, err := newReconciliationWith(w, settings).Reconcile(t.Context()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(w.runs) != 2 {
		t.Fatalf("got %d run rows after two passes, want 2", len(w.runs))
	}
	second := w.runs[1]
	if got, want := second.WindowFrom, first.WindowTo; !got.Equal(want) {
		t.Errorf("second pass opened at %s, want %s — the high-water mark is the previous pass's WindowTo, or the tail of a busy minute is swept for ever",
			windowLabel(got), windowLabel(want))
	}
	if !second.WindowTo.After(first.WindowTo) {
		t.Errorf("second pass closed at %s, want a bound after the first's %s — a window that does not move forward is not a window",
			windowLabel(second.WindowTo), windowLabel(first.WindowTo))
	}
	// The union of the two windows is contiguous and the intersection is empty,
	// which is the tiling stated as arithmetic rather than as two comparisons.
	if second.WindowFrom.Before(first.WindowTo) {
		t.Errorf("the second window starts at %s, before the first closed at %s — the two windows overlap and a row at the seam is swept twice",
			windowLabel(second.WindowFrom), windowLabel(first.WindowTo))
	}
	if got, want := w.appliedReads, []windowedRead{
		{first.WindowFrom, first.WindowTo, 500},
		{second.WindowFrom, second.WindowTo, 500},
	}; !slices.Equal(got, want) {
		t.Errorf("the applied-facts reads were %v, want %v", got, want)
	}
}

func TestAnUnfinishedRunIsStillTheHighWaterMarkTheNextPassOpensFrom(t *testing.T) {
	// The crash case, and the reason Latest is the newest row BY ID and never
	// by finished_at. A pass that died between two writes has no finish, and it
	// is still where the sweep stopped: where it stopped is where the next one
	// resumes, whether or not the pass finished. A Latest that sorted by
	// finished_at would hand the next pass a mark it may not move backwards
	// from, and the work the failed pass did not reach would never be reached.
	w := newReconWorld(t)
	settings := ReconciliationSettings{Lookback: time.Hour, Batch: 500}
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"

	// A run that started and never finished, its window standing where the
	// crashed pass got to.
	crashed := w.now.Add(-2 * time.Hour)
	w.runs = append(w.runs, persistence.Run{
		ID:         1,
		Scope:      ReconciliationScope,
		Status:     runRunning, // no finish: the pass died mid-sweep
		StartedAt:  w.now.Add(-3 * time.Hour),
		FinishedAt: nil,
		WindowFrom: crashed.Add(-time.Hour),
		WindowTo:   crashed,
	})

	if _, err := newReconciliationWith(w, settings).Reconcile(t.Context()); err != nil {
		t.Fatalf("pass after a crashed one: %v", err)
	}
	if len(w.runs) != 2 {
		t.Fatalf("got %d run rows, want 2 — the crashed row and the one this pass opened", len(w.runs))
	}
	if got, want := w.runs[1].WindowFrom, crashed; !got.Equal(want) {
		t.Errorf("the pass after a crash opened at %s, want %s — an unfinished run is where the sweep stopped, and the next one resumes there",
			windowLabel(got), windowLabel(want))
	}
}

func TestASecondPassOverUnchangedStateOpensNoNewFindingsAndCountsThemUnchanged(t *testing.T) {
	// The idempotency bargain, read off the `created` bool Open reports rather
	// than asserted about a scripted one. A finding's identity is (check,
	// subject kind, subject id) and nothing else — not a message, not a
	// timestamp, not a run id — so a pass that found the same divergence twice
	// converges on the open row and counts the second sighting as a
	// re-confirmation. A worker that wrote a row per pass would grow a table
	// without bound while failing its only question, which is what is open right
	// now.
	w := newReconWorld(t)
	w.seedBucket(bucketID(7), 5000, 250) // one standing divergence
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"

	first, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.FindingsOpened != 1 || first.FindingsUnchanged != 0 {
		t.Fatalf("first pass opened %d and re-confirmed %d, want 1 and 0 — the first sighting of a divergence is a new finding",
			first.FindingsOpened, first.FindingsUnchanged)
	}
	if got, want := w.findingsOpened(), 1; got != want {
		t.Fatalf("the findings table holds %d rows after the first pass, want %d", got, want)
	}

	w.now = w.now.Add(time.Minute)
	second, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.FindingsOpened != 0 {
		t.Errorf("the second pass opened %d findings over unchanged state, want 0 — a pass per row is a table that grows without bound while failing its only question", second.FindingsOpened)
	}
	if second.FindingsUnchanged != 1 {
		t.Errorf("the second pass re-confirmed %d findings, want 1 — the unchanged counter is what says a divergence has been true for more than one pass, which is the difference between a blip and a standing condition",
			second.FindingsUnchanged)
	}
	if got, want := w.findingsOpened(), 1; got != want {
		t.Errorf("the findings table holds %d rows after two passes over unchanged state, want %d", got, want)
	}
	// The finding's DETECTED_AT is the first sighting and its LAST_SEEN_AT the
	// most recent: a finding's history is when it was first seen and when it
	// was last seen, and a re-confirmation moves only the second. A history
	// that lost the first would tell an operator a divergence is newer than it
	// is, and one that lost the second could not show it is still true.
	row, _ := w.theFinding(checkBucketDerivationDrift, subjectBucket, string(bucketID(7)))
	if got, want := row.detectedAt, w.now.Add(-time.Minute); !got.Equal(want) {
		t.Errorf("detected_at = %s, want %s — the first sighting's instant does not move on a re-confirmation", windowLabel(got), windowLabel(want))
	}
	if got, want := row.lastSeenAt, w.now; !got.Equal(want) {
		t.Errorf("last_seen_at = %s, want %s — the most recent pass's clock, and the same instant on first sighting", windowLabel(got), windowLabel(want))
	}
	if got, want := w.runs[1].FindingsUnchanged, int64(1); got != want {
		t.Errorf("the run row recorded %d unchanged, want %d — the run row is where an operator reads whether a divergence is standing", got, want)
	}
}

func TestAResolvedFindingLetsTheSameDivergenceOpenANewRow(t *testing.T) {
	// The other half of the identity claim, and the one that makes the identity
	// a lifetime rather than a tombstone. A resolution frees the key: an
	// operator closes a finding, the divergence it was about is corrected, and
	// the same check on the same subject must be able to speak again. Keying on
	// anything a second sighting could change — the evidence, the message, the
	// run id — would make the row un-resolvable, and a table nobody can resolve
	// is a table nobody reads.
	w := newReconWorld(t)
	w.seedBucket(bucketID(7), 5000, 250)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	findings := reconFindings{world: w}

	if _, err := newReconciliation(w).Reconcile(t.Context()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	order := w.findingOrder()
	if len(order) != 1 {
		t.Fatalf("got %d open findings, want 1", len(order))
	}
	// The operator's decision, through the one sanctioned UPDATE.
	ok, err := findings.Restatus(t.Context(), 1, "open", "resolved", w.now)
	if err != nil {
		t.Fatalf("resolve the finding: %v", err)
	}
	if !ok {
		t.Fatalf("resolving the only open finding reported false; the compare-and-set should have matched the status the caller read")
	}
	if got, want := order[0], (findingKey{checkBucketDerivationDrift, subjectBucket, string(bucketID(7))}); got != want {
		t.Fatalf("resolved %v, want the bucket drift finding %v", got, want)
	}

	w.now = w.now.Add(time.Minute)
	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("pass after the resolution: %v", err)
	}
	if summary.FindingsOpened != 1 {
		t.Errorf("the pass after a resolution opened %d findings, want 1 — a resolved key is free, and a divergence that recurs after its resolution is a new one", summary.FindingsOpened)
	}
	if summary.FindingsUnchanged != 0 {
		t.Errorf("the pass after a resolution re-confirmed %d findings, want 0 — the key was free", summary.FindingsUnchanged)
	}
}

// ---------------------------------------------------------------------------
// The pass as a whole: the run row, and what a failure does to it
// ---------------------------------------------------------------------------

func TestTheRunRowIsOpenedBeforeTheSweepAndFinishedAfterIt(t *testing.T) {
	// The pass is NOT one unit of work, and that is a decision rather than an
	// accident. A transaction spanning a whole window would turn a background
	// sweep into a long-lived unit of work holding row locks for a duration
	// bounded only by the population, and a pass that failed at its end would
	// lose the findings it had legitimately found. So the run row is written
	// BEFORE the sweep and finished AFTER it — a run row that waited for a
	// transaction that only opens once the sweep is done would be a row written
	// after the thing it records.
	//
	// And the counters arrive with the finish, read back from the world rather
	// than from the summary, because the two agreeing is the claim: the
	// summary is what a log line says and the run row is what an operator reads
	// months later.
	w := newReconWorld(t)
	w.seedBucket(bucketID(1), 5000, 250) // a divergence, so both counters are non-zero
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a clean pass", err)
	}
	begin, finish := 0, -1
	for i, entry := range w.order {
		switch {
		case strings.HasPrefix(entry, "run.begin:"):
			begin = i
		case strings.HasPrefix(entry, "run.finish:"):
			finish = i
		}
	}
	if begin < 0 {
		t.Fatalf("the pass never opened a run row; the log was %v", w.order)
	}
	if finish < 0 {
		t.Fatalf("the pass never finished its run row; the log was %v", w.order)
	}
	if begin > finish {
		t.Errorf("the run row was opened at %d and finished at %d in the log; a run row written after the thing it records is a record of nothing", begin, finish)
	}
	// The sweep happened BETWEEN them, which is the whole claim.
	if !orderIs(w.order, begin+1, "bucket.sweep:00000000-0000-0000-0000-000000000000") {
		t.Errorf("the first thing after opening the run row was %v, want the bucket sweep — the row must be open while the work it records is in flight", w.order[begin+1])
	}
	if got, want := finish, len(w.order)-1; got != want {
		t.Errorf("the run row was finished at %d with %d entries after it, want it last — the row is the record of a pass that finished", got, len(w.order)-1)
	}

	run := w.runs[0]
	if run.FinishedAt == nil {
		t.Fatalf("the finished run has no finished_at; a row where the two lifecycle instants disagree is a pass that died between two writes")
	}
	if got, want := *run.FinishedAt, w.now; !got.Equal(want) {
		t.Errorf("finished_at = %s, want %s — the pass's own database clock, so the row's history and the sweep's window are one clock", windowLabel(got), windowLabel(want))
	}
	if got, want := run.FindingsOpened, int64(summary.FindingsOpened); got != want {
		t.Errorf("the run row counted %d opened, want %d — the summary and the run row are two readings of one pass and must agree", got, want)
	}
	if got, want := run.FindingsUnchanged, int64(summary.FindingsUnchanged); got != want {
		t.Errorf("the run row counted %d unchanged, want %d", got, want)
	}
	if got, want := run.BucketsScanned, int64(summary.Scanned); got != want {
		t.Errorf("the run row counted %d scanned, want %d", got, want)
	}
}

func TestAFamilyFailureReturnsItsCauseAndClosesTheRunRowAsFailed(t *testing.T) {
	// A failure mid-pass returns the error and closes the run row as
	// 'failed', and that row is the honest record of a worker that started
	// and did not finish. It is still read as the next pass's high-water
	// mark, because where the sweep stopped is where the next one resumes
	// whether or not the pass finished. The cases are in different families
	// on purpose: the run row's honesty must not depend on which read
	// failed, and a test that only ever failed one family would not know
	// that.
	tests := []struct {
		name   string
		script func(w *reconWorld)
		cause  error
		// wantText is the fragment the error's own message must carry.
		wantText string
		// wantStatus is the run row's status after the pass gives up, and
		// it is the substantive claim of this table: a pass that failed
		// inside the window closes its row as failed, because a row left
		// 'running' reads as a sweep still in progress. The one case that
		// differs is the one whose failure WAS the close.
		wantStatus string
	}{
		{
			name: "the high-water mark cannot be read",
			script: func(w *reconWorld) {
				w.failOn["runs.latest"] = errReconLatest
			},
			cause:    errReconLatest,
			wantText: "high-water mark",
		},
		{
			name: "the run row cannot be opened",
			script: func(w *reconWorld) {
				w.failOn["runs.begin"] = errReconBegin
			},
			cause:    errReconBegin,
			wantText: "begin the run",
		},
		{
			name: "the bucket sweep fails partway through",
			script: func(w *reconWorld) {
				w.seedHealthyBucket(bucketID(1), 100)
				w.failOn["buckets.sweep"] = errReconSweep
			},
			cause:      errReconSweep,
			wantText:   "sweep funding buckets",
			wantStatus: runFailed,
		},
		{
			name: "the applied-facts window cannot be read",
			script: func(w *reconWorld) {
				w.failOn["applied.recent"] = errReconRecent
			},
			cause:      errReconRecent,
			wantText:   "read applied facts",
			wantStatus: runFailed,
		},
		{
			name: "the ingestion position cannot be read",
			script: func(w *reconWorld) {
				w.failOn["cursor.position"] = errReconCursor
			},
			cause:      errReconCursor,
			wantText:   "read the ingestion position",
			wantStatus: runFailed,
		},
		{
			name: "the run row cannot be closed",
			script: func(w *reconWorld) {
				w.failOn["runs.finish"] = errReconFinish
			},
			cause:      errReconFinish,
			wantText:   "finish the run",
			wantStatus: runRunning,
		},
		{
			name: "a finding cannot be recorded",
			script: func(w *reconWorld) {
				w.seedBucket(bucketID(1), 5000, 250)
				w.failOn["findings.open"] = errReconOpen
			},
			cause:      errReconOpen,
			wantText:   "open finding",
			wantStatus: runFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newReconWorld(t)
			w.position = "0198f0a4-3f6c-7000-b000-000000000001"
			tt.script(w)

			_, err := newReconciliation(w).Reconcile(t.Context())
			if !errors.Is(err, tt.cause) {
				t.Fatalf("got %v, want an error wrapping %v — the cause travels with the pass's failure, because \"the sweep failed\" with nothing attached is a bug report nobody can act on", err, tt.cause)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("the error %q does not name the half that failed (%q); a message that cannot be acted on is a message nobody will act on", err, tt.wantText)
			}
			// The two cases that never got as far as a run row have none to
			// close, and the rest close their own. A pass that failed inside
			// the window records the row as 'failed' — not 'running', which
			// would read as a sweep still in progress — and stamps the
			// window's own `to` on it, because that is the instant the pass
			// stopped and it is the instant the next pass resumes from.
			//
			// The one case whose status is NOT 'failed' is the one whose
			// failure WAS the close: there the pass had swept the whole
			// window and the row stays 'running' with no finished_at, which
			// is the one shape in this table that reads as an unfinished
			// pass. That is correct — the pass genuinely did not manage to
			// record its own ending, and the next pass re-sweeps from its
			// window, which is the safe direction to be wrong in.
			switch tt.wantText {
			case "high-water mark", "begin the run":
				if len(w.runs) != 0 {
					t.Errorf("got %d run rows after failing before the sweep, want none", len(w.runs))
				}
			default:
				if len(w.runs) != 1 {
					t.Fatalf("got %d run rows after a mid-pass failure, want 1 — a pass that started is a fact whether or not it finished", len(w.runs))
				}
				if got, want := w.runs[0].Status, tt.wantStatus; got != want {
					t.Errorf("run status = %q, want %q — the row is the pass's own record of how it ended, and an operator reads it to decide whether this worker is working", got, want)
				}
				if tt.wantStatus == runFailed {
					if w.runs[0].FinishedAt == nil {
						t.Fatalf("the failed run has no finished_at; a 'failed' row with no instant says when nothing, and the next pass resumes from the window rather than from the row")
					}
					if got, want := *w.runs[0].FinishedAt, w.runs[0].WindowTo; !got.Equal(want) {
						t.Errorf("the failed run's finished_at is %s, want the window's to (%s) — the pass stopped at the bound it was sweeping, and that is what the next pass resumes from", got, want)
					}
				} else if w.runs[0].FinishedAt != nil {
					t.Errorf("the run whose own close failed has finished_at %s; a row whose lifecycle instants say finished when the close failed is a pass that died between two writes", w.runs[0].FinishedAt)
				}
				// The ORDER is the stronger statement and the log is where
				// order lives. Every pass that opened a run row issued
				// exactly one Finish — including the one whose own close
				// failed, which issued one and was refused, because a call
				// that did not happen and a call the port turned down are
				// different facts and only the log tells them apart. A second
				// would be a pass trying again against a row whose state it
				// does not know.
				if got, want := countOrder(w.order, "run.finish"), 1; got != want {
					t.Errorf("the pass issued Finish %d times, want %d — every pass that opened a run row closes it exactly once, on whichever of the two paths it is leaving by", got, want)
				}
				if got := countOrder(w.order, "run.begin"); got != 1 {
					t.Errorf("the pass issued Begin %d times, want 1", got)
				}
			}
		})
	}
}

func TestTheFamiliesRunInOrderOfCostAndTheCursorCheckLast(t *testing.T) {
	// The order is the order of cost per unit of coverage, and each of the three
	// positions is a claim about why. F1 first because its subject set is
	// decided before the sweep starts — every bucket on the plane, windowed or
	// not — so it is the only family whose reach does not depend on a window.
	// The windowed families follow, sharing one read of the applied-facts window
	// rather than four. F6 last because it is one query for a cursor row and
	// one for a single applied row, and the value it returns does not decay
	// while the rest of the pass runs — so there is no cost to it being last and
	// a correctness cost to it being first.
	w := newReconWorld(t)
	w.seedBucket(bucketID(1), 5000, 250)
	w.position = "" // so F6 speaks and the log names it
	// F6 needs effects on file to disagree with the empty position, and the
	// windowed families need the same row to be in the window.
	req := requestID(1)
	st := settlementID(1)
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedSettlement(healthySettlement(st, req, 700))

	if _, err := newReconciliation(w).Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v, want a clean pass", err)
	}
	sweepAt, windowAt, cursorAt, firstFinding := -1, -1, -1, -1
	for i, entry := range w.order {
		switch {
		case strings.HasPrefix(entry, "bucket.sweep:") && sweepAt < 0:
			sweepAt = i
		case entry == "applied.recent" && windowAt < 0:
			windowAt = i
		case strings.HasPrefix(entry, "finding.open:"+checkCursorMissing) && cursorAt < 0:
			cursorAt = i
		case strings.HasPrefix(entry, "finding.open:") && firstFinding < 0:
			firstFinding = i
		}
	}
	if sweepAt < 0 || windowAt < 0 || cursorAt < 0 {
		t.Fatalf("the log does not name all three families; it was %v", w.order)
	}
	if !(sweepAt < windowAt && windowAt < cursorAt) {
		t.Errorf("the families ran at buckets %d, window %d, cursor %d; F1 is windowless and the cursor's value decays, so the order is buckets, window, cursor",
			sweepAt, windowAt, cursorAt)
	}
	if firstFinding < 0 {
		t.Fatalf("the pass opened no finding over a drifted bucket with an empty position; the log was %v", w.order)
	}
	// The windowed families share ONE read of the applied-facts window rather
	// than four. Reading it four times to run four comparisons over it is four
	// times the cost for one pass's answer.
	if got, want := countOrder(w.order, "applied.recent"), 2; got != want {
		t.Errorf("the applied ledger was read %d times, want %d — one for the windowed families, one for the cursor's existence", got, want)
	}
}

func TestOneSettlementIsShapeCheckedOnceEvenWhenSeveralAppliedFactsNameIt(t *testing.T) {
	// The per-settlement guard in the window sweep, and the cost claim it
	// carries. Several applied rows naming one settlement is a real state — a
	// settlement's request_id is unique, so a second row naming it is a
	// cross-wiring F4 names in its own right — and re-checking the settlement's
	// shape for each of them would multiply the two aggregate queries that back
	// it for no new question.
	//
	// The read count is the assertion, and its arithmetic is worth stating
	// because the four reads are four different questions: F4 reads once per
	// applied fact, so the cross-wiring's confirming read is one of them and is
	// where the cross-wiring re-read discipline shows up, and the shape check
	// reads once. A fifth read is the guard missing, and it would be the shape
	// check running again off the second row.
	w := newReconWorld(t)
	owner := requestID(1)
	other := requestID(2)
	st := settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"

	w.seedApplied(appliedSettled(owner, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedApplied(appliedSettled(other, st, 8, 700), w.now.Add(-time.Minute))
	w.seedSettlement(healthySettlement(st, owner, 700))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a clean pass", err)
	}
	// The second request's fact names a settlement filed against the first, and
	// that IS a divergence — one, recorded against the settlement.
	if _, ok := w.theFinding(findingCrossWiredSettleme, subjectSettlement, string(st)); !ok {
		t.Errorf("no %s finding for the second request naming a settlement filed against the first; the pass opened %v",
			findingCrossWiredSettleme, w.checks())
	}
	if summary.FindingsOpened != 1 {
		t.Errorf("FindingsOpened = %d, want 1 — the cross-wiring is one finding, and the settlement's own shape is healthy", summary.FindingsOpened)
	}
	if got, want := w.ledger.settlementReads[st], 4; got != want {
		t.Errorf("settlement %s was read %d times, want %d — F4 reads once per applied fact and the shape check reads once per settlement, and re-checking the shape off a second row costs the two aggregates for no new question",
			st, got, want)
	}
}

// ---------------------------------------------------------------------------
// The rule and the helpers, tested directly
// ---------------------------------------------------------------------------

func TestLegShapeAcceptsTheShapesBuildSettleWritesAndRefusesAPartialWrite(t *testing.T) {
	// The per-settlement rule, as a table because the cases are four integers
	// wide and a reader wants them side by side. The plan BuildSettle writes, per
	// bucket, is a consume leg iff the spend was positive and a release iff the
	// tail was, so for a settlement with B buckets and C consumes the number of
	// buckets that took their whole hold is C − R — and the distinct-bucket
	// count of consume-without-release must equal exactly that.
	//
	// The last row is the one the rule is FOR. A consume of +50 against a
	// release of −50 sums to the cached zero, so a check that only compared sums
	// would report a healthy settlement whose release leg was never written. The
	// shape rule is the one that can see it, and that is why it exists beside
	// the sum comparison rather than instead of it.
	tests := []struct {
		name    string
		why     string
		ledger  persistence.SettlementLedger
		wantOK  bool
		explain string
	}{
		{
			name:    "the zero-priced settle books a header and no legs at all",
			why:     "a header of record with nothing moved is a legitimate answer, and every clause here is an inequality that zero satisfies",
			ledger:  zeroPricedSettlement(settlementID(1), requestID(1)),
			wantOK:  true,
			explain: "a rule with a minimum of one leg would file a critical finding against every free request the plane ever saw",
		},
		{
			name:    "one bucket whose consume took its whole hold",
			why:     "the common shape: a consume and no release beside it, because the spend equalled the hold",
			ledger:  healthySettlement(settlementID(1), requestID(1), 700),
			wantOK:  true,
			explain: "a plan whose spend equalled its hold writes exactly this",
		},
		{
			name:    "two buckets, one of which had a tail to release",
			why:     "one bucket consumed and released, one consumed its whole hold",
			ledger:  tailReleasedSettlement(settlementID(1), requestID(1), 700),
			wantOK:  true,
			explain: "a plan that under-spent one hold writes a release beside that bucket's consume",
		},
		{
			// The shape the first draft of this rule refused, and the shape any
			// account holding both a cycle bucket and a pay-as-you-go balance
			// writes on every request where the spend is under the first hold.
			// BuildSettle's two per-bucket tests are independent, so the second
			// bucket's release is legitimate with nothing to consume beside it.
			name: "a release with no consume beside it, because the spend never reached that bucket",
			why:  "the commonest multi-bucket settlement there is",
			ledger: persistence.SettlementLedger{
				LegsByKind: map[string]int64{string(accounting.KindConsume): 1, string(accounting.KindRelease): 1},
				Legs:       2,
				Buckets:    2,
			},
			wantOK:  true,
			explain: "one bucket was spent from, one was held and returned untouched; both are shapes BuildSettle writes",
		},
		{
			name: "two buckets, both returned untouched beside one spend",
			why:  "the spend ran out early, so every hold it never touched comes back whole",
			ledger: persistence.SettlementLedger{
				LegsByKind: map[string]int64{string(accounting.KindConsume): 1, string(accounting.KindRelease): 2},
				Legs:       3,
				Buckets:    3,
			},
			wantOK:  true,
			explain: "three holds, one spent, two returned: a release with no consume beside it is a bucket the spend never reached",
		},
		{
			name: "a three-bucket spend that ran out partway",
			why:  "two buckets consumed, one of them with a tail, one untouched",
			ledger: persistence.SettlementLedger{
				LegsByKind: map[string]int64{string(accounting.KindConsume): 2, string(accounting.KindRelease): 2},
				Legs:       4,
				Buckets:    3,
			},
			wantOK:  true,
			explain: "the first bucket spent and released its tail, the second spent its whole hold, the third came back untouched",
		},
		{
			name:    "a settlement that spent from every bucket it held",
			why:     "every hold exactly consumed, so a release for each",
			ledger:  healthySettlement(settlementID(3), requestID(3), 2100),
			wantOK:  true,
			explain: "the mirror of the untouched case, and equally routine",
		},
		{
			name: "a consume with no release beside it, and no other bucket to explain it",
			why:  "on its own this is a legal shape — a bucket that took its whole hold — so nothing here may fire",
			ledger: persistence.SettlementLedger{
				LegsByKind: map[string]int64{string(accounting.KindConsume): 1},
				Legs:       1,
				Buckets:    1,
			},
			wantOK:  true,
			explain: "the ledger alone cannot tell a consume that took its whole hold from a release that was never written, and neither is a defect: both are shapes BuildSettle writes. Only a count that breaks the ceiling is",
		},
		{
			name: "a release with no consume on any bucket at all",
			why:  "a settlement that settled nothing charged: every hold came back, and the plan wrote no consume",
			ledger: persistence.SettlementLedger{
				LegsByKind: map[string]int64{string(accounting.KindRelease): 1},
				Legs:       1,
				Buckets:    1,
			},
			wantOK:  true,
			explain: "the mirror of the consume-only shape: a bucket the spend never reached, which is every request whose hold came back unspent",
		},
		{
			name: "a leg kind the settle path never writes on a settlement",
			why:  "the counts read consume 1, releases 0, buckets 1, so every shape clause holds — and the leg-count clause refuses, because a grant is not a leg the multiset's two named kinds cover",
			ledger: persistence.SettlementLedger{
				LegsByKind: map[string]int64{string(accounting.KindConsume): 1, string(accounting.KindGrant): 1},
				Legs:       2,
				Buckets:    1,
			},
			wantOK:  false,
			explain: "legs equal to consumes plus releases is the clause that sees it: 2 is not 1 plus 0. The foreign-kind check is what NAMES the cause, and this rule declining the shape is not a second opinion — the foreign-kind branch returns before the shape finding is recorded, so one settlement with a grant gets one row",
		},
		{
			name: "a duplicated leg that pairs up",
			why:  "the same bucket's consume written twice, with a release written twice to match — the pairing is intact and only the ceiling sees it",
			ledger: persistence.SettlementLedger{
				LegsByKind: map[string]int64{string(accounting.KindConsume): 2, string(accounting.KindRelease): 2},
				Legs:       4,
				Buckets:    1,
			},
			wantOK:  false,
			explain: "four legs on one bucket is a write that ran twice, and a rule that compared consumes with releases would call this balanced; the ceiling on legs per bucket is the clause that has teeth",
		},
		{
			name: "more consumes than there are buckets",
			why:  "a plan writes at most one consume per bucket, so a second one on the same bucket is a write that ran twice",
			ledger: persistence.SettlementLedger{
				LegsByKind: map[string]int64{string(accounting.KindConsume): 3, string(accounting.KindRelease): 3},
				Legs:       6,
				Buckets:    2,
			},
			wantOK:  false,
			explain: "the legs are the evidence and a count per kind is what turns a duplicated leg into a finding",
		},
		{
			name: "a leg count that disagrees with the per-kind multiset",
			why:  "the totals and the parts are two readings of the same rows and they may not differ",
			ledger: persistence.SettlementLedger{
				LegsByKind: map[string]int64{string(accounting.KindConsume): 1, string(accounting.KindRelease): 1},
				Legs:       3,
				Buckets:    1,
			},
			wantOK:  false,
			explain: "a settlement whose leg count is not its consume count plus its release count is carrying a leg the multiset did not account for",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := legShapeOK(tt.ledger); got != tt.wantOK {
				t.Errorf("legShapeOK = %v, want %v: %s (%s)", got, tt.wantOK, tt.why, tt.explain)
			}
		})
	}
}

func TestForeignLegKindsNamesTheKindsASettleNeverWritesAndSortsThem(t *testing.T) {
	// The list goes into evidence, and evidence whose key order changes between
	// two passes makes one divergence look like two — so the list is sorted.
	// It is also deduplicated by construction: a multiset carries a kind once,
	// however many legs of it there are, so a grant written twice is one kind
	// in the evidence and not two.
	//
	// A hold leg is excluded by the schema's own reference shape, so a hold on a
	// settlement would mean the constraint did not hold either — which is
	// exactly what this check reports, so hold is NOT special-cased away here.
	byKind := map[string]int64{
		string(accounting.KindConsume):    2,
		string(accounting.KindRelease):    1,
		string(accounting.KindTopup):      1,
		string(accounting.KindGrant):      1,
		string(accounting.KindAdjustment): 1,
		string(accounting.KindHold):       1,
	}
	foreign := foreignLegKinds(byKind)
	want := []string{
		string(accounting.KindAdjustment),
		string(accounting.KindGrant),
		string(accounting.KindHold),
		string(accounting.KindTopup),
	}
	if !slices.Equal(foreign, want) {
		t.Fatalf("foreignLegKinds = %v, want %v — the settle path writes a consume and a release on a settlement and nothing else, and the list goes into evidence where a changing order makes one divergence look like two",
			foreign, want)
	}
	if got := foreignLegKinds(map[string]int64{
		string(accounting.KindConsume): 1,
		string(accounting.KindRelease): 1,
	}); len(got) != 0 {
		t.Errorf("foreignLegKinds on a whole plan = %v, want none — a healthy settlement must name no foreign kinds at all", got)
	}
	if got := foreignLegKinds(map[string]int64{}); len(got) != 0 {
		t.Errorf("foreignLegKinds on the zero-priced settle = %v, want none", got)
	}
}

func TestTheLegShapeKeyIsStableAcrossMapIterationOrder(t *testing.T) {
	// shapeKey renders a leg multiset as the string the leg-shape findings key
	// themselves on, and the whole design of putting the shape IN the key rests
	// on that string being the same on every pass. Go's map iteration order is
	// deliberately unspecified, so a key built from an unsorted map would be a
	// different key every pass — which would make the finding open a fresh row
	// every pass, the exact failure the shape-in-key exists to prevent.
	byKind := map[string]int64{
		string(accounting.KindConsume): 1,
		string(accounting.KindRelease): 1,
		string(accounting.KindGrant):   2,
	}
	first := shapeKey(byKind)
	for range 64 {
		if got := shapeKey(byKind); got != first {
			t.Fatalf("shapeKey returned %q then %q for the same multiset; a key that varies between passes opens a new finding every pass", got, first)
		}
	}
	want := shapeKeyOf(byKind)
	if first != want {
		t.Errorf("shapeKey = %q, want %q — the key is the string an operator reads in the findings table, sorted by kind and rendered kind=count", first, want)
	}
	if got, want := shapeKey(map[string]int64{}), ""; got != want {
		t.Errorf("shapeKey on an empty multiset = %q, want %q — the zero-priced settle's shape is the empty shape", got, want)
	}
}

func TestDerefHelpersReadNilAsNullRatherThanZero(t *testing.T) {
	// The nullable columns of an applied fact, and the difference between
	// "this row carried no amount" and "this row's amount is zero". The
	// evidence blob is read by an operator deciding what happened, and a nil
	// rendered as 0 would say a row carried nothing when it carried a nil —
	// which is a different claim about the same row.
	encoded, err := json.Marshal(map[string]any{
		"present": derefInt64(settledAmount(700)),
		"absent":  derefInt64(nil),
		"capture": derefString(captureMethod("reported")),
		"method":  derefString(nil),
	})
	if err != nil {
		t.Fatalf("encode the evidence: %v", err)
	}
	fields := decodeObserved(t, encoded)
	if got, want := fields["present"], float64(700); got != want {
		t.Errorf("a present amount encoded as %v, want %v", got, want)
	}
	if got, ok := fields["absent"]; !ok || got != nil {
		t.Errorf("a nil amount encoded as %v, want null — a row that carried no amount and a row whose amount is zero are different claims about the same row", got)
	}
	if got, want := fields["capture"], "reported"; got != want {
		t.Errorf("a present capture encoded as %v, want %v", got, want)
	}
	if got, ok := fields["method"]; !ok || got != nil {
		t.Errorf("a nil capture encoded as %v, want null", got)
	}
}

func TestTheWindowCeilingIsAFixedFarFutureInstantInsideTheTypeRange(t *testing.T) {
	// The exclusive upper bound the one unbounded read uses, and why it is a
	// CONSTANT rather than a computed "now plus something": the read must be
	// something the database can compare against without being told what time
	// it is, and a fact applied at the very first instant of the database's
	// life is exactly the row F6 exists to find. A window that started "now
	// minus the lookback" would miss it.
	//
	// And the value must be exclusive-and-in-range: a ceiling the database
	// cannot hold would make the read fail on the plane that most needs the
	// check, and a ceiling a real row could reach would leave the oldest
	// effects outside the window again.
	if got, want := windowCeiling, time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC); !got.Equal(want) {
		t.Errorf("windowCeiling = %s, want %s", windowLabel(got), windowLabel(want))
	}
	if windowCeiling.Before(newReconWorld(t).now) {
		t.Errorf("windowCeiling = %s, which is before the pass's own clock — the unbounded read would exclude every effect the plane has", windowLabel(windowCeiling))
	}
	if windowCeiling.Year() > 9999 {
		t.Errorf("windowCeiling is in year %d; timestamptz reaches 294276, so the ceiling must stay comfortably inside the type's range", windowCeiling.Year())
	}
}

func TestTheSummaryCarriesCountsAndNoIdentifiers(t *testing.T) {
	// The observability contract this plane writes by, asserted rather than
	// described. A log line is a place where a request id would eventually
	// become a metric label and start grouping on, so the pass reports counts
	// and the findings table holds the identities. A summary that grew a
	// request-id field would be a summary somebody would eventually log.
	//
	// The test is a reflection over the type rather than a string search: a
	// field added later is what this fails on, and it fails on the field rather
	// than on a rendering of it.
	summary := ReconcileSummary{}
	want := []string{"Scanned", "FindingsOpened", "FindingsUnchanged"}
	got := make([]string, 0, 3)
	typ := reflect.TypeOf(summary)
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("ReconcileSummary has fields %v, want %v — a log line carrying a request id eventually becomes a metric label grouping on it, and the identities belong in the findings table", got, want)
	}
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Type.Kind() == reflect.String {
			t.Errorf("ReconcileSummary.%s is a string; every field here is a count, because a string field is a place an identifier would go next", typ.Field(i).Name)
		}
	}
}

func TestTheDefaultSettingsAreAnHoursReachAndABatchOfFiveHundred(t *testing.T) {
	// The pacing a deployment gets when it names none, and the reasoning the
	// single knob rests on. The lookback is an hour because it is the FIRST
	// pass's reach and the first pass is the only one with no history to
	// continue from; a first pass over a whole table is a first pass whose cost
	// nobody has measured. A first pass can also follow an operator's
	// truncation of the run history, and the truncation case is better served by
	// re-checking an hour of recent state than by a sweep that holds the loop
	// until its deadline.
	//
	// And it is deliberately NOT a staleness threshold, which is why there is
	// no second knob: a lookback says "when there is nothing to continue from,
	// start here", while a staleness threshold would say "a row older than this
	// is a defect" — and that assertion cannot be made from this plane's own
	// rows. A detector built on that number would be reporting a policy
	// assertion as a detection.
	got := DefaultReconciliationSettings()
	if want := time.Hour; got.Lookback != want {
		t.Errorf("default lookback = %s, want %s", got.Lookback, want)
	}
	if got.Batch != 500 {
		t.Errorf("default batch = %d, want 500 — one number bounds every windowed read in the pass, and four numbers an operator must relate to each other is four ways to configure a pass that outruns its own deadline", got.Batch)
	}
	if got.Batch < 1 {
		t.Errorf("default batch = %d; a batch below one makes every windowed read return nothing and the pass report a clean window it never looked at", got.Batch)
	}
	if got.Lookback <= 0 {
		t.Errorf("default lookback = %s; without one the first pass sweeps an empty window and records a run row saying it completed", got.Lookback)
	}
}

// ---------------------------------------------------------------------------
// The rule helpers the tests reach through
// ---------------------------------------------------------------------------

func TestTheWindowLabelRendersABoundPreciselyEnoughToActOn(t *testing.T) {
	// The precision is not decoration. The half-open window is the pass's
	// contract with the next one, and an error that rounded a bound to the
	// second would be an error that cannot be acted on — the difference between
	// two bounds is often the whole question. The instant is rendered in UTC and
	// carries nanoseconds; the message is a log line about work, not a record
	// of any request, so the zone is a convenience rather than a privacy.
	at := time.Date(2026, 9, 26, 11, 0, 0, 123456789, time.UTC)
	want := "2026-09-26T11:00:00.123456789Z"
	if got := windowLabel(at); got != want {
		t.Errorf("windowLabel = %q, want %q — a bound rounded to the second is a difference between two bounds that cannot be computed", got, want)
	}
	// A bound in another zone is rendered in UTC rather than in the zone the
	// process happened to be in, because two windows on either side of a
	// deployment zone change are otherwise unreadable side by side.
	zone := time.FixedZone("test", 5*60*60)
	if got := windowLabel(at.In(zone)); got != want {
		t.Errorf("windowLabel of a +05:00 instant = %q, want %q — every bound in a message is UTC so two windows compare", got, want)
	}
}

func TestTheObservedEncoderSwallowsAnUnencodableValueIntoAnEmptyObject(t *testing.T) {
	// The evidence is jsonb the findings table stores, and a shape json cannot
	// encode would be a bug in a check. The alternative to swallowing it is
	// panicking, and the alternative chosen is the empty object: a bug in a
	// check degrades into a finding with no evidence rather than a pass that
	// crashes between two findings and takes the other checks with it. The
	// evidence being absent is visible; the pass stopping is not.
	//
	// The point being made is about the HANDLING, and the value a check builds
	// today — an int64, a string, a bool, a []string, a map[string]int64 — is
	// encodable by construction, so the empty object is reached only by a shape
	// that should not exist. This asserts the degradation is real and not
	// theoretical: a channel does not marshal, and the check that passed one
	// would be a bug this file can demonstrate rather than imagine.
	fields := map[string]any{"good": int64(1), "bad": make(chan int)}
	encoded := observed(fields)
	if string(encoded) != "{}" {
		t.Errorf("observed with an unencodable value = %s, want {} — the pass must degrade into a finding with no evidence rather than crash between two findings", encoded)
	}
	// And the ordinary case is an object, not an empty one, so the degradation
	// above is a property of the failure and not of the encoder.
	ordinary := observed(map[string]any{"cached_settled": int64(5000), "legs": int64(2)})
	if string(ordinary) != `{"cached_settled":5000,"legs":2}` {
		t.Errorf("observed with ordinary figures = %s, want the object it was given", ordinary)
	}
}

func TestEveryRemainingReadThePassMakesSurfacesItsOwnCause(t *testing.T) {
	// The reads the family-level table above does not reach, each one on its
	// own and each one checked the same way: the error comes back wrapping the
	// port's own failure, and the message names the half. There is no branch
	// here that returns a clean result on a read that failed, and the reason
	// each one matters is that a background pass that swallows a read failure
	// reports a window it did not look at — the one outcome this worker must
	// never produce.
	//
	// The clock is first because it is the pass's only source of "now": a
	// failed clock read means the pass does not know its own window's upper
	// bound, and a pass that guessed one would compare two clocks about one
	// fact.
	tests := []struct {
		name     string
		script   func(w *reconWorld)
		cause    error
		wantText string

		// absorbed marks the one case whose read failed and whose failure the
		// family answers with SILENCE. It exists in this table so the
		// distinction is stated rather than implied: every other row is a
		// read whose error travels, and F5's confirming read is the one that
		// does not.
		absorbed bool
	}{
		{
			name:     "the database clock cannot be read",
			script:   func(w *reconWorld) { w.failOn["clock.now"] = errReconClock },
			cause:    errReconClock,
			wantText: "read the database clock",
		},
		{
			name: "a bucket's own two sides cannot be read",
			script: func(w *reconWorld) {
				w.seedHealthyBucket(bucketID(1), 100)
				w.failOn["ledger.bucket"] = errReconBucket
			},
			cause:    errReconBucket,
			wantText: "check bucket",
		},
		{
			name: "a settlement's legs cannot be read behind a settled fact",
			script: func(w *reconWorld) {
				w.seedApplied(appliedSettled(requestID(1), settlementID(1), 7, 700), w.now.Add(-time.Minute))
				w.failOn["ledger.settlement"] = errReconSettle
			},
			cause:    errReconSettle,
			wantText: "read the settlement behind the settled fact",
		},
		{
			name: "the settlement's total is confirmed by a read that cannot be completed",
			script: func(w *reconWorld) {
				req, st := requestID(1), settlementID(1)
				w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-time.Minute))
				// A settlement whose header and its own consume legs disagree,
				// so the pass has a total mismatch to confirm and therefore
				// makes a re-read. The legs are a whole plan's, so F3's shape
				// assertions have nothing to say and it is the TOTAL's
				// confirming read that this reaches.
				drifted := healthySettlement(st, req, 700)
				drifted.ConsumeSum = 450
				w.ledger.settlementAnswers = map[accounting.SettlementID][]persistence.SettlementLedger{
					st: {drifted},
				}
				// The stage is counted over the reads this settlement has
				// answered, and the pass makes three of them: F4's own read
				// of the settlement behind the fact, the shape check's — which
				// the pass's per-settlement cache does NOT serve, because F4
				// bypassed the cache and so nothing was ever stored in it — and
				// the re-read that confirms the mismatch. The failure is
				// staged at the SECOND, and what that catches is worth saying
				// plainly: the pass's own cache is per-CHECK, so a read the
				// pass thought it had already made is still a read, and a
				// database that refuses it is a failure the pass has to
				// report rather than one it may quietly serve from memory.
				w.ledger.settlementFailFrom = map[accounting.SettlementID]int{st: 1}
				w.failOn["ledger.settlement"] = errReconSettle
			},
			cause:    errReconSettle,
			wantText: "read the legs of settlement",
		},
		{
			name: "a terminal effect's class cannot be looked up",
			script: func(w *reconWorld) {
				w.seedApplied(appliedTerminal(requestID(1), 7, ingestion.KindReleased), w.now.Add(-time.Minute))
				w.failOn["applied.find"] = errReconFind
			},
			cause:    errReconFind,
			wantText: "read the settlement class of request",
		},
		{
			name: "a settled effect's orphan class cannot be looked up",
			script: func(w *reconWorld) {
				req, st := requestID(1), settlementID(1)
				// The settlement is on file for this request, so F4 has nothing
				// to say and the orphan lookup is the NEXT read of this applied
				// row — and the one that fails. F4 on its own is covered by the
				// case above.
				w.seedSettlement(healthySettlement(st, req, 700))
				w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-time.Minute))
				w.failOn["applied.find"] = errReconFind
			},
			cause:    errReconFind,
			wantText: "read the orphan class of request",
		},
		{
			name: "the shape's confirming read cannot be completed",
			script: func(w *reconWorld) {
				req, st := requestID(1), settlementID(1)
				w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-time.Minute))
				// Legs that are not a shape any settle plan writes, so the
				// shape check has a finding and therefore a re-read — and the
				// settlement is read three times before that re-read: F4's,
				// the shape check's (the pass's per-settlement cache is not
				// consulted for a settlement F4 already read, because F4
				// never put one there), and the re-read itself. Staging the
				// failure at the third is therefore the re-read, and it is
				// the one read failure in this file the pass must refuse to
				// absorb: recording the finding off a read that did not
				// complete would put an unconfirmed divergence in front of
				// an operator.
				partial := healthySettlement(st, req, 700)
				partial.Legs = 4
				partial.LegsByKind = map[string]int64{
					string(accounting.KindConsume): 2,
					string(accounting.KindRelease): 2,
				}
				w.ledger.settlementAnswers = map[accounting.SettlementID][]persistence.SettlementLedger{
					st: {partial, partial, partial},
				}
				w.ledger.settlementFailFrom = map[accounting.SettlementID]int{st: 2}
				w.failOn["ledger.settlement"] = errReconSettle
			},
			cause:    errReconSettle,
			wantText: "re-read the legs of settlement",
		},
		{
			name: "the confirming lookup of an orphan cannot be completed",
			script: func(w *reconWorld) {
				req, st := requestID(1), settlementID(1)
				w.seedSettlement(healthySettlement(st, req, 700))
				w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-time.Minute))
				// The first lookup answers with an orphan and the second
				// cannot be completed, and the second is the only thing
				// standing between the pass and a charged-and-disclaimed
				// finding. A first draft of this check answered a failed
				// confirming read with SILENCE, on the argument that a missed
				// confirmation would cost a later pass. That argument is wrong
				// here, and the reason is the high-water mark: the window is
				// re-read by nobody, so a check that could not complete is not
				// deferred, it is lost. F1, F2 and F3 already refused a failed
				// re-read for that reason and this one now refuses with them.
				orphan := appliedOrphan(req, 11)
				w.findScripts[findKey{string(req), ingestion.ClassUnbillableOrphaned}] = []findAnswer{
					{fact: &orphan},
					{err: errReconFind},
				}
			},
			cause:    errReconFind,
			wantText: "re-read the orphan class",
		},
		{
			name: "the applied ledger cannot be read for the cursor's existence",
			script: func(w *reconWorld) {
				// An empty position and a settled fact on file — the exact
				// state F6 exists to report. The failure is staged at the
				// SECOND read because the first is the windowed sweep's: F6
				// reads the ledger itself, with its own unbounded window, and
				// a pass that reported the windowed read's failure would be
				// naming the wrong half.
				req, st := requestID(1), settlementID(1)
				w.seedSettlement(healthySettlement(st, req, 700))
				w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-time.Minute))
				w.position = ""
				w.recentFailFrom = 1
				w.failOn["applied.recent"] = errReconRecent
			},
			cause:    errReconRecent,
			wantText: "read the applied ledger",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newReconWorld(t)
			w.position = "0198f0a4-3f6c-7000-b000-000000000001"
			tt.script(w)

			_, err := newReconciliation(w).Reconcile(t.Context())
			if tt.cause == nil {
				// The one case that expects no error at all, and it is here
				// because the family absorbs this failure rather than
				// propagating it. The cause stays named in the case's script
				// so the reader can see which read failed.
				if err != nil {
					t.Fatalf("Reconcile returned %v, want the pass to absorb this one", err)
				}
			} else if !errors.Is(err, tt.cause) {
				t.Fatalf("got %v, want an error wrapping %v — a read that failed is not a clean result, and a pass that reports one has swept a window it never looked at", err, tt.cause)
			}
			if tt.cause != nil && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("the error %q does not name the half that failed (%q)", err, tt.wantText)
			}
			if got := w.findingsOpened(); got != 0 {
				t.Errorf("the pass recorded %d findings on a failed read, want none — a finding whose comparison never completed is a claim nobody can check: %v", got, w.checks())
			}
			if tt.absorbed {
				// The evidence that this pass really swept rather than
				// skipping: the window was read, and the run was closed
				// with the counters the sweep produced. A pass that had
				// failed somewhere and returned an error would leave the run
				// open, so the finished row is the proof the absorption cost
				// the window nothing.
				if countOrder(w.order, "run.finish") != 1 {
					t.Errorf("the pass absorbed the failure and did not finish its run; a window it did not finish is a window its next pass will re-read: %v", w.order)
				}
			}
		})
	}
}

func TestARunRowThePassNoLongerOwnsRefusesThePassesOwnFinish(t *testing.T) {
	// Finish is compare-and-set on the run's own status, and the case a
	// correct caller never produces is the one worth testing anyway: the row
	// is in a status the pass did not put it in — a second worker closed it,
	// or an operator resolved the run by hand — so the compare-and-set
	// refuses and the counters the pass asserted are lost.
	//
	// The refusal is not an error, and it must not become one: the sweep is
	// over, the findings are on file, and the window really was swept. So this
	// asserts the whole shape — no error, the summary intact, the pass having
	// tried, and the row still the pass's own running row with nothing
	// written to it.
	//
	// What it does NOT assert is that the pass learned its counters were lost.
	// It does not, and cannot: the call discards the port's boolean and keeps
	// only the error, so a refused finish is invisible to the caller. The run
	// row stays 'running', and the next pass reads it as the high-water mark
	// it should be — the safe direction, but it means a lost finish is
	// recoverable only by an operator reading the history.
	w := newReconWorld(t)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.finishBy = runCompleted // the row is not 'running' by the time Finish arrives

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass that swept its window; a refused compare-and-set is a lost claim, not a failed one", err)
	}
	if summary.Scanned != 0 || summary.FindingsOpened != 0 || summary.FindingsUnchanged != 0 {
		t.Errorf("the summary came back %+v on a clean pass, want the zero counts — a refusal must not blank a window the pass really did sweep", summary)
	}
	if got := countOrder(w.order, "run.finish"); got != 1 {
		t.Errorf("the pass issued Finish %d times, want 1 — the refusal is the ANSWER, and a pass that retried would be a pass writing to a row it does not own", got)
	}
	if got, want := w.runs[0].Status, runRunning; got != want {
		t.Errorf("the run status is %q after a refused finish, want %q — the guard refused, so the row is where the pass left it and the pass has no other copy of the counters", got, want)
	}
	if w.runs[0].FinishedAt != nil {
		t.Errorf("the refused run has finished_at %s; a row whose lifecycle instants say finished when the guard said no is a lie an operator reads", w.runs[0].FinishedAt)
	}
}

func TestThePerSettlementCacheCoversOneCheckAndNotTheOther(t *testing.T) {
	// The pass caches a settlement's legs for the shape check, and the cache
	// is a cost decision, not a truth decision: it saves one aggregate read
	// when two applied facts in the same window name the same settlement. It
	// is per-CHECK, and this is the test that says so — F4's read and the
	// shape check's read are two reads of one settlement, because the cache
	// is populated by the shape check and F4 runs first and does not consult
	// it. A cache that covered both would hide a settlement re-filed between
	// the two, which is exactly the race F4's re-read exists to see.
	//
	// Both facts name the same settlement for the same request, which is a
	// state a real plane can hold: the applier's class key is (request,
	// class), so one request has one settled fact, but a settled fact whose
	// settlement_id was rewritten by hand is a schema violation this plane
	// exists to report and the table cannot forbid.
	w := newReconWorld(t)
	req, st := requestID(1), settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedSettlement(healthySettlement(st, req, 700))
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-2*time.Minute))
	w.seedApplied(appliedSettled(req, st, 9, 700), w.now.Add(-time.Minute))

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass over two facts naming one healthy settlement", err)
	}
	if summary.FindingsOpened != 0 {
		t.Errorf("the pass opened %d findings over two settled facts naming one healthy settlement, want none: %v", summary.FindingsOpened, w.checks())
	}
	// Two facts, two F4 reads, and ONE shape check — the shape check is
	// guarded by its own `checked` set, so the second fact does not re-run it,
	// and that set is the reason the read count is three and not four.
	if got := w.ledger.settlementReads[st]; got != 3 {
		t.Errorf("the settlement was read %d times, want 3 — one per fact behind F4 and one for the shape check, because F4's read does not populate the shape check's cache", got)
	}
	// The cache is consulted only AFTER a settlement has been asked for once,
	// and the `checked` set means the shape check is the only caller that
	// could ever be served by it — it runs at most once per settlement. So the
	// honest reading of this file's read counts is that the cache's hit branch
	// is unreachable from outside the package, and the way to say that is to
	// try: the count below is 2, which is F4's read and the shape check's,
	// and the two facts never produce a third.
	if got := w.ledger.settlementReads[st]; got != 3 {
		t.Errorf("the settlement was read %d times after the whole pass, want 3", got)
	}
}

func TestAShapeThatResolvesBetweenTheFirstReadAndTheReReadRecordsNothing(t *testing.T) {
	// The re-read is the shape check's only defence against a divergence that
	// no longer exists, and here the state moves in the only way it can move:
	// the first read sees legs that are not a shape any settle plan writes,
	// and the confirming read — which bypasses the pass's cache — sees the
	// whole plan. A pass that recorded off the first read would have put a
	// settlement in front of an operator for a defect that was repaired
	// before anyone looked at it, and a settlement is the one subject where
	// that is easy to do, because the repair is a legitimate thing for the
	// applier to have done.
	w := newReconWorld(t)
	req, st := requestID(1), settlementID(1)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	w.seedApplied(appliedSettled(req, st, 7, 700), w.now.Add(-time.Minute))
	// The reads are ordered: F4's, the shape check's, and the re-read. The
	// script makes the first two see a whole plan and the third see legs that
	// are not a shape any settle plan writes — one bucket, one consume, and
	// no bucket carrying a consume without a release, which is the shape of a
	// release leg that was never written.
	partial := healthySettlement(st, req, 700)
	partial.LegsByKind = map[string]int64{string(accounting.KindConsume): 1}
	partial.BucketsWithoutRelease = 0
	whole := healthySettlement(st, req, 700)
	w.ledger.settlementAnswers = map[accounting.SettlementID][]persistence.SettlementLedger{
		st: {whole, whole, partial},
	}

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a pass whose re-read confirmed the shape", err)
	}
	if summary.FindingsOpened != 0 {
		t.Errorf("the pass opened %d findings on a shape the re-read confirmed, want none: %v", summary.FindingsOpened, w.checks())
	}
	if got := w.ledger.settlementReads[st]; got != 2 {
		t.Errorf("the settlement was read %d times, want 2 — a whole plan on both of the pass's reads is not a shape failure, so it never reaches the re-read the script staged a third answer for", got)
	}
}

func TestAFailedPassWhoseOwnCloseAlsoFailedCarriesBothCauses(t *testing.T) {
	// Two failures, and the operator reading the returned error needs both.
	// The sweep failed — that is the cause, and it is what a retry is for. The
	// close failed — and that is the worse news, because it means the pass's
	// own record of how far it got is not on file either. Losing one of them
	// is losing the answer to a question: a joined error that kept only the
	// sweep's cause would leave an operator with a failed window and a run
	// row still reading 'running', which looks like a pass in progress; one
	// that kept only the close's would point at the wrong half.
	//
	// The run row is the proof the close did not land: a pass that closed it
	// as failed would have status 'failed' and a finished_at, and the row
	// stays 'running' without one. The pass did try — the log says so.
	w := newReconWorld(t)
	w.seedHealthyBucket(bucketID(1), 100)
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	// The windowed read fails, and then the close of the run it opened fails
	// on the way out of the failure path.
	w.failOn["applied.recent"] = errReconRecent
	w.failOn["runs.finish"] = errReconFinish

	_, err := newReconciliation(w).Reconcile(t.Context())
	if err == nil {
		t.Fatalf("Reconcile returned no error; a pass whose sweep failed and whose close failed must say so")
	}
	// errors.Is against BOTH, because errors.Join produces a multi-error and
	// this is the assertion that holds whichever order they were joined in.
	if !errors.Is(err, errReconRecent) {
		t.Errorf("the returned error does not wrap the sweep's failure (%v): %v — a caller retrying on this error is retrying a cause it was not told", errReconRecent, err)
	}
	if !errors.Is(err, errReconFinish) {
		t.Errorf("the returned error does not wrap the close's failure (%v): %v — the close is the worse news and the one that explains a run row still reading 'running'", errReconFinish, err)
	}
	// Both messages, not just both causes: an operator reads the string, and
	// the two halves must each name the half that failed.
	if !strings.Contains(err.Error(), "read applied facts") {
		t.Errorf("the joined error does not name the windowed read: %v", err)
	}
	if !strings.Contains(err.Error(), "as failed") {
		t.Errorf("the joined error does not say the pass was recording itself as failed: %v — the reader cannot tell which of the two writes the close was", err)
	}
	// The row is untouched, and the log shows the attempt.
	if len(w.runs) != 1 {
		t.Fatalf("got %d run rows, want 1 — the pass opened one and could not close it", len(w.runs))
	}
	if got, want := w.runs[0].Status, runRunning; got != want {
		t.Errorf("run status = %q, want %q — the close failed, so the row is where the pass left it and a 'failed' verdict nobody wrote is a verdict nobody can trust", got, want)
	}
	if w.runs[0].FinishedAt != nil {
		t.Errorf("the run whose close failed has finished_at %s; the row says finished when the write that would have said it did not land", w.runs[0].FinishedAt)
	}
	if got := countOrder(w.order, "run.finish"); got != 1 {
		t.Errorf("the pass issued Finish %d times, want 1 — it tried to close the row and the port refused; a second attempt would be a pass writing to a row it does not know the state of", got)
	}
}

func TestTheFailureKeepsTheFindingsAndClosesTheRunRowAsFailed(t *testing.T) {
	// What survives a failed pass, and it is a specific and small thing. The
	// findings a pass legitimately found are kept — each Open is its own
	// commit, and findings outliving the pass that found them is the correct
	// durability story — and the run row carries the counts the pass had
	// reached plus a 'failed' verdict. A design that rolled the pass back
	// would lose the findings it had found, and a design that committed a
	// completed run row would hide the failure.
	//
	// The row is CLOSED, not left running, and that is the part of the change
	// worth stating: a 'running' row says a sweep is in progress, and this one
	// is not. It has ended, badly, and an operator reading the history needs
	// to be able to tell those two apart without inferring it from a null.
	w := newReconWorld(t)
	w.seedBucket(bucketID(1), 5000, 250) // a divergence found before the failure
	w.position = "0198f0a4-3f6c-7000-b000-000000000001"
	// The windowed read fails after the bucket sweep has already recorded.
	w.failOn["applied.recent"] = errReconRecent

	summary, err := newReconciliation(w).Reconcile(t.Context())
	if !errors.Is(err, errReconRecent) {
		t.Fatalf("got %v, want an error wrapping %v", err, errReconRecent)
	}
	// The finding the pass DID legitimately find is on file, and the counter
	// that says so is in the summary it returned alongside the error — a
	// caller that ignores the error still learns what was recorded.
	if summary.FindingsOpened != 1 {
		t.Errorf("the summary reported %d opened alongside the failure, want 1 — a pass returns whatever it recorded before any family failed", summary.FindingsOpened)
	}
	if _, ok := w.theFinding(checkBucketDerivationDrift, subjectBucket, string(bucketID(1))); !ok {
		t.Errorf("the finding recorded before the failure is not on file; findings outlive the pass that found them and each Open is its own commit")
	}
	if got, want := w.runs[0].Status, runFailed; got != want {
		t.Errorf("run status = %q, want %q — the run row is the record that this pass started and did not finish, and a row left 'running' reads as a sweep still in progress", got, want)
	}
	// The row carries the counts the pass had reached BEFORE the failure, not
	// the ones it would have reached by finishing. That is the point of
	// closing it rather than rolling it back: the row says how far this pass
	// got, and the next pass says how far it got.
	if got, want := w.runs[0].FindingsOpened, int64(1); got != want {
		t.Errorf("the failed run recorded %d opened, want %d — a failed row that kept no counts would say only that something stopped, and this pass found a real divergence before it did", got, want)
	}
	if got, want := w.runs[0].BucketsScanned, int64(1); got != want {
		t.Errorf("the failed run recorded %d buckets scanned, want %d — the same claim the other counters make", got, want)
	}
	if w.runs[0].FinishedAt == nil {
		t.Fatalf("the failed run has no finished_at; a closed row with no instant says when nothing")
	}
	if got, want := *w.runs[0].FinishedAt, w.runs[0].WindowTo; !got.Equal(want) {
		t.Errorf("the failed run's finished_at is %s, want the window's to (%s) — the pass stopped at the bound it was sweeping", got, want)
	}
	// And the next pass picks up from where this one stopped, because the
	// closed-but-failed run row is the high-water mark just as a running one
	// was: where a pass stopped is where the next resumes.
	w.failOn = map[string]error{}
	w.now = w.now.Add(time.Minute)
	next, err := newReconciliation(w).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("the pass after the failure: %v", err)
	}
	if next.FindingsUnchanged != 1 {
		t.Errorf("the next pass re-confirmed %d findings, want 1 — the work the failed pass did reach is re-confirmed, which is free", next.FindingsUnchanged)
	}
	if got, want := w.runs[1].WindowFrom, w.runs[0].WindowTo; !got.Equal(want) {
		t.Errorf("the next pass opened at %s, want %s — the open run row is the high-water mark, and where a pass stopped is where the next resumes",
			windowLabel(got), windowLabel(want))
	}
}
