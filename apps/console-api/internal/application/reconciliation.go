package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/ingestion"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// Reconciliation is the Control Plane's reconciliation worker (B13): a pass
// that looks for ways this plane's own state contradicts itself, records what
// it finds, and stops.
//
// It is DETECT-AND-REPORT, and that is not a phase to be finished later — it
// is the shape the design has and cannot be bent out of. Seven adversarial
// reviews against the merged B6 foundation established that automatic repair
// is structurally impossible here, and the two reasons are worth carrying
// into every read of this file, because they are why there is no method on this
// type that fixes anything:
//
//   - The ledger is append-only. control.ledger_entries and
//     control.settlements carry BEFORE UPDATE OR DELETE triggers, so a
//     correction cannot be an edit: it must be a NEW adjustment leg, and an
//     adjustment leg requires an operator_id. "system:reaper" is a machine
//     principal, and what a principal may be in the ledger's grammar is a
//     decision the founder has not made — the schema's own comment says the
//     ledger must not guess at its grammar. Minting a principal to get a refund
//     out is how an audit trail learns to lie.
//   - A reversal is not representable as one adjustment.
//     ledger_entries_leg_algebra makes an adjustment single-axis: exactly one
//     of its two deltas is non-zero. A consume moves settled AND held by the
//     same amount, so undoing one needs both axes, and the two-adjustment
//     workaround is blocked by the no-credit rule — the axis that would have to
//     be credited first is one the account cannot be credited past. Over-
//     settlement therefore escalates to an operator, which is the correct
//     outcome and not a gap in this worker.
//
// Every read below is a read, and every write lands in a findings row or a run
// row. If you are looking for the place a divergence gets fixed, it is not here
// and it must not be: the fix is an operator's decision made through the
// ledger's own door, and this worker's job is to have made the divergence
// legible enough for that decision to be quick.
//
// ---------------------------------------------------------------------------
// The check table
// ---------------------------------------------------------------------------
//
// Six checks in four families. They share one discipline — re-read before
// recording, see checkBucketDerivation — and they differ in what they can see.
//
//	 F1 bucket_derivation_drift      funding_bucket  the cached three balances
//	                                                  disagree with what the
//	                                                  bucket's own legs derive
//	 F2 settlement_total_mismatch    settlement      the header's total and the
//	                                                  sum of its consume legs
//	                                                  disagree
//	 F3 settlement_leg_shape         settlement      the legs are not a shape
//	                                                  any settle plan writes
//	 F4 settled_without_settlement   request         a settled effect with no
//	                                                  settlement behind it, or
//	                                                  with a settlement that
//	                                                  names another request
//	 F5 disposition_conflict         request         one request's applied rows
//	                                                  disagree about which
//	                                                  terminal state it reached
//	 F6 cursor_missing               control_plane   the position is absent
//	                                                  while effects exist
//
// Every one of them is falsifiable against this plane's own rows. That is the
// line the pass draws, and the classes it CANNOT cross are named at the end of
// this comment rather than approximated, because a pass that quietly guessed
// at them would be worse than one that admits the gap.

// The check kinds. Each is part of a finding's identity, so they are a closed
// vocabulary this plane owns rather than free text, and two checks with
// different names are different findings even about the same subject.
const (
	checkBucketDerivationDrift   = "bucket_derivation_drift"
	checkSettlementTotalMismatch = "settlement_total_mismatch"
	checkSettlementLegShape      = "settlement_leg_shape"
	checkSettledWithoutSettle    = "settled_without_settlement"
	checkDispositionConflict     = "disposition_conflict"
	checkCursorMissing           = "cursor_missing"

	// The three F4 sub-keys, separately keyed because they are three different
	// defects with three different remedies, and an operator who resolves one
	// must not have the other follow it closed.
	//
	// The first two are shapes the schema's own constraints forbid
	// (applied_facts_settled_shape pins a settled row's settlement_id to
	// non-NULL, applied_facts_settlement_fkey pins the reference). Finding one
	// is therefore a fact about the constraint, not about the money: a row that
	// landed under an older schema, a restored dump, or a hand-written insert.
	// The finding is still worth recording, because the charge the row
	// represents may exist with nothing behind it and nothing else in the plane
	// will say so.
	findingNullSettlementID   = checkSettledWithoutSettle + ".null_settlement_id"
	findingAbsentSettlement   = checkSettledWithoutSettle + ".absent_settlement"
	findingCrossWiredSettleme = checkSettledWithoutSettle + ".cross_wired_settlement"

	// The two F5 sub-keys, and why they differ in severity is written at the
	// check that raises each.
	findingTerminalSplit     = checkDispositionConflict + ".terminal_split"
	findingChargedDisclaimed = checkDispositionConflict + ".charged_and_disclaimed"

	// The two F3 sub-keys. Both carry the leg multiset they were found with in
	// the key, and that is load-bearing: a settlement corrected from carrying
	// one consume leg to carrying two is a DIFFERENT divergence, and keying both
	// under the bare check kind would make the correction re-confirm the
	// finding it was meant to answer. A settlement found healthy generates no
	// row at all, so a healthy settlement can never collide with a shape key.
	findingForeignLegKind   = checkSettlementLegShape + ".foreign_leg_kind"
	findingConsumeNoRelease = checkSettlementLegShape + ".consume_without_release"
)

// The subject kinds, which with the check kind and the subject id form a
// finding's identity. They are this worker's vocabulary and not the wire's:
// control_plane is a subject in its own right, for the one check whose subject
// is the plane rather than a row.
const (
	subjectBucket       = "funding_bucket"
	subjectSettlement   = "settlement"
	subjectRequest      = "request"
	subjectControlPlane = "control_plane"
)

// The severities, in the migration's three words. They are written once, at
// detection, and never moved — a finding whose severity could be re-graded
// would let a reader renegotiate the evidence instead of resolving it.
const (
	severityInfo     = "info"
	severityWarning  = "warning"
	severityCritical = "critical"
)

// ReconciliationScope is the scope string every run row carries. One value
// today: the whole lane. It is a column and not a constant folded into the
// adapter so that a second scope is a new value with its own checks rather than
// an ambiguity inside this one.
const ReconciliationScope = "control_plane"

// LedgerReader is the slice of the accounting use cases the pass reads
// through: a bucket's cached and derived balances, and a settlement's own
// legs.
//
// It is an interface rather than the Accounting use case because the pass must
// not inherit the one thing on that type which moves money. Taking all of
// Accounting would put Adjust, Settle, Hold and ReleaseHold one call away from
// a background loop, and the only thing standing between this worker and an
// automatic refund would be that nobody has written the call yet. The
// interface IS the guarantee: a repair path cannot be added here without
// editing this declaration, which is a reviewable event rather than a silent
// one.
//
// The two members it does have are reads that return what they found and judge
// nothing: ReconcileBucket reports both sides plus the domain's own verdict,
// and SettlementLedger reports the legs as they exist. A member that fixed
// something would be a member this interface does not have.
type LedgerReader interface {
	// ReconcileBucket compares a bucket's cached balances against what its legs
	// alone derive, and reports both sides plus whether they agree.
	ReconcileBucket(ctx context.Context, bucketID accounting.FundingBucketID) (ReconcileReport, error)

	// SettlementLedger returns a settlement's header beside the consume-leg
	// sum, the leg count, the per-kind multiset and the two bucket counts of
	// its own legs.
	SettlementLedger(ctx context.Context, settlementID accounting.SettlementID) (persistence.SettlementLedger, error)
}

// Reconciliation is the worker. Its fields are its dependencies and nothing
// else: it holds no cache, no position of its own, and no state carried
// between passes, because a pass that remembers something in memory holds
// findings that disappear with the process. Everything durable is a row.
type Reconciliation struct {
	buckets  persistence.FundingBuckets
	applied  persistence.AppliedFacts
	cursor   persistence.IngestionCursor
	findings persistence.ReconciliationFindings
	runs     persistence.ReconciliationRuns
	ledger   LedgerReader
	clock    persistence.Clock
	settings ReconciliationSettings
}

// ReconciliationSettings is the pass's pacing, all of it supplied by the
// composition root from configuration. It carries no staleness threshold, and
// the absence is a decision rather than an omission — see
// defaultReconciliationSettings for why, at length, because a lookback knob
// and a "too old" knob look identical and mean opposite things.
type ReconciliationSettings struct {
	// Lookback is how far back a pass reaches when it has no high-water mark to
	// continue from: the first pass, or the first after an operator truncated
	// the run history. Every later pass opens at the previous pass's WindowTo,
	// so this bounds the FIRST pass's reach and is not a per-pass window.
	Lookback time.Duration

	// Batch is the bound on every windowed read in the pass: buckets per page,
	// and rows per window read. One number, because one number is a pass's cost
	// per statement, and four numbers an operator must relate to each other is
	// four ways to configure a pass that outruns its own deadline.
	Batch int
}

// DefaultReconciliationSettings is the pacing a deployment gets when it names
// none.
//
// The lookback is an hour because it is the FIRST pass's reach and the first
// pass is the only one with no history to continue from: an hour of newly
// created state is enough for the first pass to say something, and a larger
// number would be a first pass scanning a table whose size nobody has
// measured. It is deliberately NOT a staleness threshold, and the difference is
// the whole reason there is no second knob. A lookback says "when there is
// nothing to continue from, start here"; a staleness threshold would say "a row
// older than this is a defect", and that assertion cannot be made from this
// plane's own rows — a settled fact can arrive minutes after it happened, a
// released one hours, and an orphan whenever the runtime gives up on it, with
// nothing in Control-Plane state recording how long any of those SHOULD have
// taken. A detector built on that number would be reporting a policy assertion
// as a detection, and the finding table is the one place in this plane where a
// reader must be able to trust that what is written down was observed.
//
// The batch is 500 because a background pass over a human-paced plane's tables
// has no workload that would justify measuring it, and 500 keeps one
// statement's result set inside whatever a pass's deadline can absorb.
func DefaultReconciliationSettings() ReconciliationSettings {
	return ReconciliationSettings{
		Lookback: time.Hour,
		Batch:    500,
	}
}

// NewReconciliation builds the worker around the ports it needs. It panics on
// a nil port for the reason every constructor in this package does: a port this
// use case was promised and did not get is a wiring defect, and the middle of a
// pass — after a run row is open and half the buckets swept — is a strictly
// worse place to learn about it.
//
// The signature is deliberately short. A first draft took the store, the
// settlements repository and the quarantine as well, and all three went
// unused: the store because a pass is deliberately not one unit of work (see
// Reconcile), and the other two because the checks that would have read them
// are two this plane may not ship — the missing-settlement sweep, which needs
// the Data Plane's request population, and the unreleased-hold detector, whose
// timeout is the runtime's policy and not this plane's to set. A port held
// here and read nowhere is a promise the composition root makes and the worker
// does not keep, and a future check wiring itself to one of them would be
// reaching for a dependency the worker's shape does not actually have. When a
// check lands that needs one, the parameter comes back with the check.
//
// The settings are refused rather than corrected. A batch of zero would make
// every windowed read return nothing and every pass report a clean window it
// never looked at, which is the one outcome a reconciliation pass must never
// produce. A lookback of zero or less would make the first pass sweep an empty
// window and record a run row saying it completed — and a run row is the record
// an operator later reads to decide whether this worker has ever worked.
func NewReconciliation(
	buckets persistence.FundingBuckets,
	applied persistence.AppliedFacts,
	cursor persistence.IngestionCursor,
	findings persistence.ReconciliationFindings,
	runs persistence.ReconciliationRuns,
	ledger LedgerReader,
	clock persistence.Clock,
	settings ReconciliationSettings,
) *Reconciliation {
	switch {
	case buckets == nil:
		panic("application: NewReconciliation requires a funding buckets repository")
	case applied == nil:
		panic("application: NewReconciliation requires an applied-facts ledger")
	case cursor == nil:
		panic("application: NewReconciliation requires an ingestion cursor")
	case findings == nil:
		panic("application: NewReconciliation requires a findings repository")
	case runs == nil:
		panic("application: NewReconciliation requires a runs repository")
	case ledger == nil:
		panic("application: NewReconciliation requires a ledger reader")
	case clock == nil:
		panic("application: NewReconciliation requires a clock")
	}
	if settings.Batch < 1 {
		panic("application: NewReconciliation requires a batch of at least 1, a pass that reads nothing reports a clean window it never swept")
	}
	if settings.Lookback <= 0 {
		panic("application: NewReconciliation requires a positive lookback, the first pass sweeps nothing without one")
	}
	return &Reconciliation{
		buckets:  buckets,
		applied:  applied,
		cursor:   cursor,
		findings: findings,
		runs:     runs,
		ledger:   ledger,
		clock:    clock,
		settings: settings,
	}
}

// ReconcileSummary is one pass's headline. It carries no subject, no request
// id and no settlement id, and that restraint IS the observability contract
// this plane writes by: a log line is a place where a request id would
// eventually become a metric label and start grouping on, so the pass reports
// counts and the findings table holds the identities.
type ReconcileSummary struct {
	// Scanned is how many rows the pass looked at, over every family.
	Scanned int

	// FindingsOpened is how many findings this pass created, and
	// FindingsUnchanged how many divergences it re-confirmed against findings
	// that were already open. The second is the one that says a divergence has
	// been true for more than one pass, which is the difference between a blip
	// and a standing condition.
	FindingsOpened    int
	FindingsUnchanged int
}

// Reconcile runs one pass and records what it finds.
//
// `to` is this method's to read, from the database's own clock and never
// time.Now(): a pass that bounded its window by a process wall clock while
// every row it stamps is written by the engine's clock would be comparing two
// clocks about one fact. `from` is the high-water mark — the previous run's
// WindowTo when there is one, and only when there is not the lookback. A
// recomputed now()-minus-lookback bound would re-derive its own tail forever
// whenever a pass outran its own interval, so the tail of a busy minute would
// be swept on every pass after it and every finding in that tail would say
// "seen again" for a window the pass had already covered.
//
// The window is half-open, from inclusive and to exclusive, and it is the run
// row's own: the same two instants bound every read the pass makes. That is
// what lets two consecutive passes tile the timeline, with no row swept twice
// and none skipped between two windows.
//
// The pass is NOT one unit of work, and that is a decision. A transaction
// spanning a whole window would turn a background sweep into a long-lived unit
// of work holding row locks for a duration bounded only by the population,
// and a pass that failed at its end would lose the findings it had
// legitimately found. Findings outliving the pass that found them is the
// correct durability story: each Open is its own commit, and the pass's own
// record of having run is the run row — written BEFORE the sweep and finished
// AFTER it, so a pass that dies mid-sweep leaves a 'running' row. That row is
// the honest record of a worker that started and did not finish, and it is
// still read as the next pass's high-water mark, because where the sweep
// stopped is where the next one resumes whether or not the pass finished.
//
// A failure mid-pass records the run as 'failed' and returns the error. The
// next pass reads the newest row by id — including one that never finished —
// and continues from its window, so work the failed pass did not reach is
// reached, and work it did reach is re-confirmed, which is free.
func (r *Reconciliation) Reconcile(ctx context.Context) (ReconcileSummary, error) {
	to, err := dbNow(ctx, r.clock, "reconcile")
	if err != nil {
		return ReconcileSummary{}, err
	}
	from, err := r.windowFrom(ctx, to)
	if err != nil {
		return ReconcileSummary{}, err
	}

	// The run row is opened before the sweep, on the pool rather than inside a
	// unit of work, for the reason the pass has none: a run row that waited for
	// a transaction that only opens once the sweep is done would be a row
	// written after the thing it records.
	//
	// A window another pass is already sweeping is not a failure and not a
	// retry: that pass is doing the work this one would have done, so the
	// right answer is to sweep nothing and let the next tick read a mark this
	// pass never wrote. Treating the refusal as an error would log a line
	// every interval for the life of a second replica, which is the noise a
	// correct deployment produces most.
	runID, err := r.runs.Begin(ctx, ReconciliationScope, from, to)
	if err != nil {
		if errors.Is(err, persistence.ErrWindowClaimed) {
			return ReconcileSummary{}, nil
		}
		return ReconcileSummary{}, fmt.Errorf("application: reconcile: begin the run over [%s, %s): %w",
			windowLabel(from), windowLabel(to), err)
	}

	summary, err := r.sweep(ctx, from, to)
	if err != nil {
		// The pass closes its own run row as failed before returning, and that
		// is what the 'failed' status is for. The findings it already recorded
		// stand — each Open was its own commit and each one was true when it
		// was written — so the run row is the only thing left to close, and a
		// row left 'running' for ever would read as a pass still sweeping.
		//
		// Its own failure is not masked by the sweep's: a failed Finish is
		// wrapped onto the sweep's error rather than dropped, because the two
		// mean different things and an operator reading this one wants both.
		//
		// The close runs on a context that outlives the caller's, and that is
		// the difference between a worker that was stopped and a plane with a
		// stranded pass. The signal's own context is already cancelled, so a
		// Finish issued on it is refused by the store for the reason it should
		// be — and the row it was meant to close stays 'running' for ever, read
		// by every future operator as a worker still sweeping, on every
		// deployment that restarts, which is to say on every deployment. The
		// grace is bounded (runCloseGrace) because the goroutine is waited on
		// by the drain, and a process that will not stop is a worse failure
		// than the row it was added to prevent; and the next pass re-claims and
		// sweeps that window regardless, so a close that misses its grace
		// costs the reader one stale row rather than any coverage.
		closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), runCloseGrace)
		defer closeCancel()
		if _, finishErr := r.runs.Finish(closeCtx, runID, runRunning, runFailed, to, persistence.RunCounters{
			BucketsScanned:    int64(summary.Scanned),
			FindingsOpened:    int64(summary.FindingsOpened),
			FindingsUnchanged: int64(summary.FindingsUnchanged),
		}); finishErr != nil {
			return summary, errors.Join(err, fmt.Errorf(
				"application: reconcile: record the pass over [%s, %s) as failed: %w",
				windowLabel(from), windowLabel(to), finishErr))
		}
		return summary, err
	}

	// A refused compare-and-set here is a pass whose run row is no longer
	// 'running' — a finisher that raced this one. It is not an error, because
	// the row already carries a verdict and the pass's own work is unaffected;
	// it is worth saying so rather than discarding the bool in silence, since
	// a silent discard is how a second accounting of the same pass appears.
	//
	// The same context rule as the failure path above: a successful sweep
	// finished under a signal that arrived just after its last read, and a
	// Finish issued on the cancelled context would leave a completed pass
	// recorded as still running.
	closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), runCloseGrace)
	defer closeCancel()
	if _, err := r.runs.Finish(closeCtx, runID, runRunning, runCompleted, to, persistence.RunCounters{
		BucketsScanned:    int64(summary.Scanned),
		FindingsOpened:    int64(summary.FindingsOpened),
		FindingsUnchanged: int64(summary.FindingsUnchanged),
	}); err != nil {
		return summary, fmt.Errorf("application: reconcile: finish the run over [%s, %s): %w",
			windowLabel(from), windowLabel(to), err)
	}
	return summary, nil
}

// runCloseGrace bounds the one write a cancelled context must not strand:
// closing the run row of the pass that was interrupted. It is a constant
// because there is nothing to tune — the write is one UPDATE of one row the
// pass already holds by id, and every value here is far longer than a
// statement that short has any reason to take. It is bounded rather than
// open because this runs under a drain that waits for the worker, and a
// process that will not stop is a worse failure than a stranded row.
const runCloseGrace = 5 * time.Second

// The run statuses the pass itself writes, restated from the migration's CHECK
// so a typo here is a compile-adjacent failure rather than a constraint
// violation at three in the morning.
const (
	runRunning   = "running"
	runCompleted = "completed"
	runFailed    = "failed"
)

// windowFrom is the high-water mark, and this is its only rule.
//
// The newest run by id — not by finished_at, because a crashed pass has no
// finish and is still where the sweep stopped — supplies the next window's
// lower bound. A pass whose window ended at `to` has covered everything up to
// `to`, so the next one starts there; anything else either re-sweeps the tail
// or skips it.
//
// The ErrNotFound branch is the first pass, and the lookback is what it falls
// back to. That fallback is bounded rather than total on purpose: a first pass
// over the whole table is a first pass whose cost nobody has measured, and the
// truncation case is better served by re-checking an hour of recent state than
// by a sweep that holds the loop until its deadline.
//
// The one deferral is worth stating, because the pass deliberately does NOT do
// the obvious thing here. A cursor that has moved BACKWARDS — a restored
// position, an operator's edit — is a real defect, and it is also the one
// cursor question this plane may not answer: the position is a Data-Plane
// issued opaque string, and this port's own doc says parsing it, ordering by
// it, or validating its shape is a rule the whole Control Plane keeps, not a
// rule about the wire alone. A "the cursor went backwards" check would be a
// second reader of the position's grammar, in the one place that must not have
// one. It is deferred to the day the feed contract exposes a comparable
// position — a watermark, or a sequence this plane may order by — which is the
// day the question acquires an answer instead of a guess. Until then the only
// thing about the position this pass reads is whether it exists, and F6 is
// that check.
func (r *Reconciliation) windowFrom(ctx context.Context, to time.Time) (time.Time, error) {
	latest, err := r.runs.Latest(ctx)
	switch {
	case err == nil:
		return latest.WindowTo, nil
	case !errors.Is(err, persistence.ErrNotFound):
		return time.Time{}, fmt.Errorf("application: reconcile: read the high-water mark: %w", err)
	}
	return to.Add(-r.settings.Lookback), nil
}

// sweep runs the four families and returns whatever it recorded before any of
// them failed.
//
// The order is the order of cost per unit of coverage. F1 comes first because
// its subject set is decided before the sweep starts — every bucket on the
// plane, windowed or not — so it is the only family whose reach does not
// depend on a window and the only one whose findings are about state that is
// not time-bounded. The windowed families follow, sharing one read of the
// applied-facts window rather than four. F6 comes last: it is one query for a
// cursor row and one for a single applied row, and the value it returns does
// not decay while the rest of the pass runs.
//
// A family that fails stops the pass. The families are not independent enough
// for a partial result to mean much, and the run row is left 'running' — which
// is the record that this pass did not finish.
func (r *Reconciliation) sweep(ctx context.Context, from, to time.Time) (ReconcileSummary, error) {
	var summary ReconcileSummary

	scanned, err := r.sweepBuckets(ctx, &summary)
	if err != nil {
		return summary, err
	}
	summary.Scanned += scanned

	if err := r.sweepWindow(ctx, &summary, from, to); err != nil {
		return summary, err
	}
	if err := r.checkCursorExists(ctx, &summary); err != nil {
		return summary, err
	}
	return summary, nil
}

// ---------------------------------------------------------------------------
// F1 — bucket_derivation_drift
// ---------------------------------------------------------------------------

// sweepBuckets walks the bucket table by keyset and compares each bucket's
// cached balances against what its own legs derive.
//
// The walk is keyset-paginated and never OFFSET, for the port's stated reason:
// OFFSET re-reads and re-discards every row already passed, so its cost grows
// with the table, and a pass paging by OFFSET while legs land can both skip a
// row and read one twice. A short page is the end of the sweep and never an
// error.
//
// The walk is unbounded in pages and bounded in each page, and the asymmetry is
// safe for one reason: a bucket created mid-sweep sorts at or after the cursor
// by uuid v7's mint ordering, so the next pass's sweep of the same keyspace
// reaches it. No bucket is missed for being new, and the pass's own cost is
// bounded by the deadline the loop gives it.
func (r *Reconciliation) sweepBuckets(ctx context.Context, summary *ReconcileSummary) (int, error) {
	// The all-zero uuid is the keyset's origin: a bucket id is a uuid and the
	// column is compared as uuid, so the zero value sorts before every real
	// one and the first page is the whole table's beginning. An empty string
	// would be a type error and a "no cursor yet" sentinel would be a second
	// meaning on a column that has one.
	const zeroBucketID = "00000000-0000-0000-0000-000000000000"

	after := accounting.FundingBucketID(zeroBucketID)
	scanned := 0
	for {
		page, err := r.buckets.Sweep(ctx, after, r.settings.Batch)
		if err != nil {
			return scanned, fmt.Errorf("application: reconcile: sweep funding buckets after %s: %w", after, err)
		}
		for _, bucket := range page {
			scanned++
			if err := r.checkBucketDerivation(ctx, summary, bucket.ID); err != nil {
				return scanned, err
			}
			after = bucket.ID
		}
		if len(page) < r.settings.Batch {
			return scanned, nil
		}
	}
}

// checkBucketDerivation is F1 for one bucket, and the shape every check in
// this file follows: read, compare, and on a divergence RE-READ before
// recording anything.
//
// The re-read is the difference between a finding and a snapshot of a race. A
// leg landing between the sweep's read and the finding's write would otherwise
// be recorded as a divergence that the next pass immediately re-confirms as
// absent — a finding saying "this bucket is wrong" about a bucket that is
// right, sitting in a table no one can delete from. Re-reading through the same
// reader the first read came from, and recording only when the second read
// disagrees too, turns that race into a non-event: the leg landed, the second
// read agrees with the cache, nothing is written, and the next pass sees the
// same agreement.
//
// It costs one extra read per diverging bucket — not per bucket, since the
// re-read is only reached on a divergence — on the pass that is already the
// background one and runs on its own interval. It buys a findings table with no
// false positives in it, which is the only property that makes the table worth
// reading at all.
//
// The legs' count rides along as evidence and is NOT part of the verdict:
// Derivation.ConsistentWith compares the three balances and nothing else, by
// design, because a bucket with a drifted cache and no legs is a different
// defect from one with ten thousand and the two deserve different evidence.
func (r *Reconciliation) checkBucketDerivation(ctx context.Context, summary *ReconcileSummary, bucketID accounting.FundingBucketID) error {
	report, err := r.ledger.ReconcileBucket(ctx, bucketID)
	if err != nil {
		return fmt.Errorf("application: reconcile: check bucket %s: %w", bucketID, err)
	}
	if report.Consistent {
		return nil
	}
	confirmed, err := r.ledger.ReconcileBucket(ctx, bucketID)
	if err != nil {
		return fmt.Errorf("application: reconcile: re-check bucket %s: %w", bucketID, err)
	}
	if confirmed.Consistent {
		return nil // the world moved under the first read; there was no divergence
	}
	return r.record(ctx, summary, persistence.Finding{
		CheckKind:   checkBucketDerivationDrift,
		SubjectKind: subjectBucket,
		SubjectID:   string(bucketID),
		Severity:    severityCritical,
		Observed: observed(map[string]any{
			"cached_settled":    int64(confirmed.Bucket.Settled),
			"derived_settled":   int64(confirmed.Derivation.Settled),
			"cached_held":       int64(confirmed.Bucket.Held),
			"derived_held":      int64(confirmed.Derivation.Held),
			"cached_available":  int64(confirmed.Bucket.Available),
			"derived_available": int64(confirmed.Derivation.Available),
			"legs":              confirmed.Derivation.Legs,
		}),
		Detail: "the bucket's cached balances disagree with what its own ledger legs derive; the legs are the authority (ADR 0004), and the adjustment that would correct this is an operator's decision with no automatic form. Every figure is from the confirming read, so the cached and derived sides are one snapshot rather than two instants spliced together",
	})
}

// ---------------------------------------------------------------------------
// F2, F3, F4, F5 — the windowed families
// ---------------------------------------------------------------------------

// sweepWindow runs the four checks that share the applied-facts window, and
// reads that window once. Each check is a comparison over a set of applied
// facts, and reading the same window four times to run four comparisons over it
// is four times the cost for one pass's answer.
//
// The window bounds WHICH rows are read and the batch bounds HOW MANY per
// statement, so a window whose population exceeds the batch is paged — and it
// is paged inside the window it was given, because the run row has already
// committed `to` as the next pass's lower bound. A page that is still full
// after it has been checked is not the end of anything, so the next page's
// lower bound is that page's own last applied_at: the keyset is the same
// (applied_at, request_id) order the read is written in, which is what makes
// the walk skip no row and repeat none even while the ledger is still growing
// at the window's far edge.
//
// Growth at the far edge is what bounds the loop, and the arithmetic is worth
// stating because the alternative is worse than unbounded. The upper bound was
// taken once, at the start of the pass, from the plane's own clock; a row
// applied after it is outside the window whatever this pass does, and belongs
// to the next one. So the pages cannot outrun their own window indefinitely —
// at worst they page to the moment `to` was read and stop. What the loop does
// NOT do is extend `to` to chase a growing tail: that would make a slow
// window's length grow with its own slowness, and a pass that can never
// finish is a pass whose window never advances. The loop's own budget is the
// context the caller gave it, and a pass that runs out of it fails and leaves
// its window unadvanced, which is a recordable pass rather than a silent gap.
//
// A silent gap is what this shape exists to prevent, and the failure it would
// have been is worth naming: reading one page, advancing the high-water mark
// past the rest of the window, and stamping the run 'completed' reports a
// window swept and clean for rows nobody read. Those rows are then unreachable
// for ever, because the mark has moved past them and no pass revisits a window
// it has already covered.
//
// The per-settlement cache is the other cost decision. A settlement's own legs
// are two aggregate queries, and F2, F3 and F4 all need them; asking three
// times would let a leg landing between the asks produce three findings about
// one settlement from three different states of it. The cache lives for one
// pass and is rebuilt every pass — a finding's identity does not depend on it,
// so a pass that found the same divergence last time reaches the same
// conclusion from a fresh read.
func (r *Reconciliation) sweepWindow(ctx context.Context, summary *ReconcileSummary, from, to time.Time) error {
	ledgers := map[accounting.SettlementID]persistence.SettlementLedger{}
	settlementOf := func(ctx context.Context, id accounting.SettlementID) (persistence.SettlementLedger, error) {
		if cached, ok := ledgers[id]; ok {
			return cached, nil
		}
		ledger, err := r.ledger.SettlementLedger(ctx, id)
		if err != nil {
			return persistence.SettlementLedger{}, err
		}
		ledgers[id] = ledger
		return ledger, nil
	}

	// after and afterID are the keyset's open bound, on the pair. The zero
	// time and the empty string open the window: no applied row carries
	// either, so the first page is the window's whole beginning.
	after, afterID := time.Time{}, ""
	for {
		facts, err := r.applied.Recent(ctx, from, to, after, afterID, r.settings.Batch)
		if err != nil {
			return fmt.Errorf("application: reconcile: read applied facts over [%s, %s) after %s: %w",
				windowLabel(from), windowLabel(to), windowLabel(after), err)
		}
		summary.Scanned += len(facts)

		checked := map[accounting.SettlementID]struct{}{}
		for _, fact := range facts {
			if err := r.checkSettledWithoutSettlement(ctx, summary, fact); err != nil {
				return err
			}
			if err := r.checkDispositionConflict(ctx, summary, fact); err != nil {
				return err
			}
			if fact.Kind != ingestion.KindSettled || fact.SettlementID == "" {
				continue
			}
			id := accounting.SettlementID(fact.SettlementID)
			if _, dup := checked[id]; dup {
				continue
			}
			checked[id] = struct{}{}
			if err := r.checkSettlementShape(ctx, summary, fact, settlementOf); err != nil {
				return err
			}
		}

		if len(facts) < r.settings.Batch {
			return nil
		}
		// The next page's open bound is the last row this one read, on the pair
		// the read is ordered by. The read hands both back for exactly this: a
		// keyset a caller cannot reconstruct is not a cursor, and a keyset on
		// the instant alone would drop every other row that shares it — which,
		// because a whole ingestion page commits in one transaction, is most of
		// a window.
		last := facts[len(facts)-1]
		after, afterID = last.AppliedAt, last.RequestID
	}
}

// checkSettlementShape is F2 and F3 together, over one settlement.
//
// Two assertions, and the second is not implied by the first. The header's
// total is DEFINED to be the sum of the consume legs — BuildSettle computes it
// from the legs it wrote — so a mismatch there is a header and its own legs
// disagreeing about what was charged. And the leg multiset is asserted
// separately because a PARTIAL leg write can still balance: a consume of +50
// against a release of -50 sums to the cached zero, so a check that only
// compared sums would report a healthy settlement whose release leg is missing
// or whose consume leg was written twice. The bucket counts exist for the same
// reason and are what the multiset is checked against: BuildSettle writes, per
// bucket, a consume if the spend was positive and a release if the tail was,
// so consume-count minus release-count equals the number of buckets whose
// consume took their whole hold, and the same equality computed from distinct
// buckets is an invariant a partial write cannot satisfy by accident.
//
// The EXPECTED shape is recomputed from the settlement's own buckets and its
// own total, and NOT from the fact's payload. That is the load-bearing choice
// in this check. The payload is the Data Plane's, its schema is the contract's,
// and re-deriving the expected multiset from it would make this check a second
// interpreter running beside the applier — the kind of second opinion the
// ledger is supposed not to need, and one that would need re-implementing
// every time the fact contract grows a field. The legs are this plane's own
// record of what it did; the header is this plane's own writer's total for the
// same plan; comparing those two is a statement about this plane's internal
// consistency, and it needs no data from another plane to make.
//
// What it therefore CANNOT say is which of the two figures is right, and it
// does not try: the total_mismatch finding records the disagreement, and the
// correction is an operator's.
//
// The shape is in the finding's key for the leg-shape assertions, so a
// settlement whose legs are corrected opens a NEW finding rather than
// re-confirming the one the correction was answering. A settlement found
// healthy generates no row at all, so there is nothing for a wrong shape to
// collide with.
func (r *Reconciliation) checkSettlementShape(ctx context.Context, summary *ReconcileSummary, fact persistence.AppliedFact,
	settlementOf func(context.Context, accounting.SettlementID) (persistence.SettlementLedger, error),
) error {
	id := accounting.SettlementID(fact.SettlementID)
	ledger, err := settlementOf(ctx, id)
	switch {
	case err == nil:
	case errors.Is(err, persistence.ErrNotFound):
		// F4's absent-settlement finding already covers this fact, keyed by the
		// request. One defect, one finding.
		return nil
	default:
		return fmt.Errorf("application: reconcile: read the legs of settlement %s: %w", id, err)
	}

	shapeOK := legShapeOK(ledger)
	foreign := foreignLegKinds(ledger.LegsByKind)

	// Both figures come from the RE-READ, never one from each pass. The whole
	// point of the re-read is to assert against a state that survived a
	// concurrent write, and evidence that splices the first read's header with
	// the second read's legs is a pair of numbers that never coexisted — which
	// is the one thing an operator adjudicating this finding must not be handed,
	// and the identity trigger makes it permanently unrewritable. The same
	// holds for the bucket check below, and the reason is the same.
	if ledger.ConsumeSum != ledger.Settlement.SettledTotal.Int64() {
		confirmed, err := r.ledger.SettlementLedger(ctx, id)
		if err != nil {
			return fmt.Errorf("application: reconcile: re-read the legs of settlement %s: %w", id, err)
		}
		if confirmed.ConsumeSum != confirmed.Settlement.SettledTotal.Int64() {
			if err := r.record(ctx, summary, persistence.Finding{
				CheckKind:   checkSettlementTotalMismatch,
				SubjectKind: subjectSettlement,
				SubjectID:   fact.SettlementID,
				Severity:    severityCritical,
				Observed: observed(map[string]any{
					"header_total": int64(confirmed.Settlement.SettledTotal),
					"consume_sum":  confirmed.ConsumeSum,
					"legs":         confirmed.Legs,
				}),
				Detail: "the settlement header's total and the sum of its own consume legs disagree, and the header is defined to be that sum: which of the two is right is an operator's answer, because the header is written once and neither figure can be re-derived from the other",
			}); err != nil {
				return err
			}
		}
	}

	// One re-read covers both leg-shape assertions, because they are read from
	// the same rows and a re-read that confirmed one and not the other would
	// mean the settlement changed between two reads of one statement — which
	// the table's own append-only rules make impossible, since a settlement's
	// legs only ever land whole in one unit of work.
	if shapeOK && len(foreign) == 0 {
		return nil
	}
	confirmed, err := r.ledger.SettlementLedger(ctx, id)
	if err != nil {
		return fmt.Errorf("application: reconcile: re-read the legs of settlement %s: %w", id, err)
	}
	confirmedShapeOK, confirmedForeign := legShapeOK(confirmed), foreignLegKinds(confirmed.LegsByKind)

	if len(confirmedForeign) > 0 {
		return r.record(ctx, summary, persistence.Finding{
			CheckKind:   findingForeignLegKind + "#" + shapeKey(confirmed.LegsByKind),
			SubjectKind: subjectSettlement,
			SubjectID:   fact.SettlementID,
			Severity:    severityCritical,
			Observed: observed(map[string]any{
				"foreign_kinds": confirmedForeign,
				"legs_by_kind":  confirmed.LegsByKind,
				"legs":          confirmed.Legs,
			}),
			Detail: "the settlement carries a leg of a kind the settle path never writes on a settlement: the schema's reference shape pins those kinds' settlement_id to NULL, so this is a leg attached to a settlement it does not belong to",
		})
	}
	if !confirmedShapeOK {
		return r.record(ctx, summary, persistence.Finding{
			CheckKind:   findingConsumeNoRelease + "#" + shapeKey(confirmed.LegsByKind),
			SubjectKind: subjectSettlement,
			SubjectID:   fact.SettlementID,
			Severity:    severityCritical,
			Observed: observed(map[string]any{
				"consume_legs": confirmed.LegsByKind[string(accounting.KindConsume)],
				"release_legs": confirmed.LegsByKind[string(accounting.KindRelease)],
				"buckets":      confirmed.Buckets,
			}),
			Detail: "the settlement's legs are not a shape any settle plan writes: each bucket contributes at most one consume and one release, so a settlement of N buckets holds at most N of each and at most 2N legs, and the leg count is the consume count plus the release count with nothing unaccounted for",
		})
	}
	return nil
}

// legShapeOK is the per-settlement shape rule, in one place so the first read
// and the re-read cannot judge the same rows differently.
//
// What BuildSettle writes, per bucket, is a consume leg IFF the spend was
// positive and a release leg IFF the tail was positive — two independent
// tests, not one. So the shape a settlement of B buckets can have is exactly:
// buckets paired (one of each leg), buckets consumed-and-exhausted (a consume
// alone), buckets untouched (a release alone), and buckets touched (both). All
// four are legitimate, and the third is the common one: a reservation that
// held from two buckets but spent out of the first leaves the second with a
// release and nothing to consume from it, and any account with both a
// subscription cycle and a pay-as-you-go balance settles that way on every
// request where the spend is under the first hold.
//
// Which makes the ceiling the whole of it. Each bucket writes at most one leg
// of each kind, so a settlement of B buckets writes at most 2B legs, and
// consumes and releases each number at most B. That is what catches a
// duplicated leg — a second consume on the same bucket, however the counts
// pair up — and it is the only thing a count can say, because the pairing
// itself is not a property the aggregate can recover: a consume with no
// release beside it has two explanations the legs cannot tell apart, the
// consume genuinely taking its whole hold or a release that was never written,
// and both are shapes the multiset is compatible with.
//
// The first draft of this rule asserted releases <= consumes, which is the
// same ceiling read the other way round and which BuildSettle's per-bucket
// independence refutes: it fired on the majority of ordinary multi-bucket
// settlements, at critical severity, with a detail sentence describing the
// shape as unnameable. A findings table that cries critical on healthy money
// is not read. A consume/available ratio is deliberately NOT checked here — the
// accounting domain's own guard already refuses a consume above a bucket's
// available, and re-deriving it would put a balance rule in a file that is not
// allowed to know about balances.
//
// The zero-priced settle books a header and no legs at all: zero buckets, zero
// consumes, zero releases, and every clause below holds at zero. That shape is
// a legitimate answer and must not open a finding, which is why the clauses are
// inequalities zero satisfies rather than a minimum of one leg.
func legShapeOK(ledger persistence.SettlementLedger) bool {
	consumes := ledger.LegsByKind[string(accounting.KindConsume)]
	releases := ledger.LegsByKind[string(accounting.KindRelease)]
	return consumes <= ledger.Buckets &&
		releases <= ledger.Buckets &&
		consumes+releases <= 2*ledger.Buckets &&
		ledger.Legs == consumes+releases
}

// foreignLegKinds is the leg kinds a settlement carries that the settle path
// never writes on one, sorted. A grant, a topup, an adjustment OR A HOLD leg
// naming a settlement is a leg attached to a record it does not belong to, and
// the hold is the one worth naming: the schema pins settlement_id to NULL for
// every kind outside {consume, release}
//
//	ledger_entries_leg_algebra: WHEN 'hold' THEN settlement_id IS NULL
//
// so a hold leg carrying a settlement is a fact about the constraint having
// failed, not an impossible row. It lands in this same bucket because both are
// "a leg of a kind the settle path never writes, attached to a settlement it
// does not belong to", and a check that excluded hold would leave the one the
// schema most clearly forbids unexamined. The list is sorted and deduplicated
// because it goes into evidence, and an evidence blob whose key order changes
// between two passes makes one divergence look like two.
func foreignLegKinds(byKind map[string]int64) []string {
	foreign := make([]string, 0, len(byKind))
	for kind := range byKind {
		switch kind {
		case string(accounting.KindConsume), string(accounting.KindRelease):
			continue
		default:
			foreign = append(foreign, kind)
		}
	}
	sort.Strings(foreign)
	return foreign
}

// checkSettledWithoutSettlement is F4, and it is the check that walks the
// applied ledger BACKWARDS from each settled fact to the settlement it claims to
// have produced.
//
// The direction is the check's whole point: the applied_facts row names a
// settlement, and the question is whether the settlement behind that name
// exists and is filed against the same request. Three shapes, each separately
// keyed because each is a different defect with a different remedy, and the
// separation is not cosmetic: keyed as one finding, an operator who resolved the
// absent-settlement case would have the cross-wired case follow it closed, and
// a cross-wired settlement is the more serious of the two.
//
//	null_settlement_id    the row is a settled fact with no settlement_id at
//	                      all. applied_facts_settled_shape forbids it, so
//	                      finding one is a fact about the constraint: a row
//	                      that landed before the constraint did, a restored
//	                      dump, a hand-written insert. The charge the row
//	                      represents may exist with nothing behind it.
//	absent_settlement     the row names a settlement and the settlement is not
//	                      on file. The foreign key forbids it, same reasoning.
//	                      Critical: the charge exists and the record it names
//	                      does not, and nothing else in the plane will say so.
//	cross_wired_settlement  the settlement is on file and belongs to another
//	                      request. The header's request_id is unique, so
//	                      exactly one request owns it and this fact's request
//	                      is not that one: two requests' money and records
//	                      disagree about which is which. Recorded against the
//	                      SETTLEMENT, because the defect is the settlement's —
//	                      it is filed against the wrong request — and an
//	                      operator resolving it needs the settlement in hand,
//	                      not either of the two requests it is confusing.
//
// The re-read applies to the cross-wired shape and not to the two absent ones,
// and the asymmetry is deliberate. A settlement appearing or disappearing under
// an applied row is a schema violation being repaired, and a re-read would
// simply catch the repair and say nothing — which is right: the finding
// recorded a defect that has been fixed, and the findings table is
// append-only precisely so the record of it survives. A settlement changing
// REQUEST is a different thing: it can happen with no constraint violated at
// all, and a finding about a cross-wiring that resolved itself would be a
// finding nobody can delete. So the one shape that can arise from a race
// rather than a violation is the one that gets re-read.
func (r *Reconciliation) checkSettledWithoutSettlement(ctx context.Context, summary *ReconcileSummary, fact persistence.AppliedFact) error {
	if fact.Kind != ingestion.KindSettled {
		return nil
	}
	if fact.SettlementID == "" {
		return r.record(ctx, summary, persistence.Finding{
			CheckKind:   findingNullSettlementID,
			SubjectKind: subjectRequest,
			SubjectID:   fact.RequestID,
			Severity:    severityCritical,
			Observed: observed(map[string]any{
				"kind":           fact.Kind,
				"kind_class":     fact.KindClass,
				"append_seq":     fact.AppendSeq,
				"settled_amount": derefInt64(fact.SettledAmount),
			}),
			Detail: "a settled fact is recorded as applied with no settlement of record, which the schema's own settled-shape check forbids: the charge it represents may exist with nothing behind it, and whether it does is not readable from Control-Plane state alone",
		})
	}

	ledger, err := r.ledger.SettlementLedger(ctx, accounting.SettlementID(fact.SettlementID))
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		return r.record(ctx, summary, persistence.Finding{
			CheckKind:   findingAbsentSettlement,
			SubjectKind: subjectRequest,
			SubjectID:   fact.RequestID,
			Severity:    severityCritical,
			Observed: observed(map[string]any{
				"settlement_id":  fact.SettlementID,
				"append_seq":     fact.AppendSeq,
				"settled_amount": derefInt64(fact.SettledAmount),
			}),
			Detail: "a settled fact is recorded as applied against a settlement that is not on file, which the foreign key forbids: the charge exists and the record it names does not",
		})
	case err != nil:
		return fmt.Errorf("application: reconcile: read the settlement behind the settled fact of request %s: %w", fact.RequestID, err)
	}

	if string(ledger.Settlement.RequestID) == fact.RequestID {
		return nil
	}
	// Re-read, fresh from the ledger and not from the pass's cache: a
	// settlement re-filed under a different request between the read and the
	// write would otherwise be a finding about a cross-wiring that no longer
	// exists. A re-read that could not be completed fails the pass rather than
	// answering it — a window this check could not look at is a window the
	// high-water mark will move past regardless of what is decided here.
	confirmed, err := r.ledger.SettlementLedger(ctx, accounting.SettlementID(fact.SettlementID))
	if err != nil {
		return fmt.Errorf("application: reconcile: re-read the settlement behind the settled fact of request %s: %w", fact.RequestID, err)
	}
	if string(confirmed.Settlement.RequestID) == fact.RequestID {
		return nil
	}
	return r.record(ctx, summary, persistence.Finding{
		CheckKind:   findingCrossWiredSettleme,
		SubjectKind: subjectSettlement,
		SubjectID:   fact.SettlementID,
		Severity:    severityCritical,
		Observed: observed(map[string]any{
			"settlement_request_id": confirmed.Settlement.RequestID,
			"fact_request_id":       fact.RequestID,
			"append_seq":            fact.AppendSeq,
		}),
		Detail: "the settlement a settled fact named is filed against a different request: the header's request_id is unique, so exactly one of the two requests can own it, and two requests' records disagree about which",
	})
}

// checkDispositionConflict is F5, and it replaces the "duplicate settlement"
// check a first draft of this table carried. The rename is the point.
//
// "Duplicate settlement" would have asserted that a request has at most one
// settlement — but the schema's unique index on request_id already makes that
// structurally true, so the check would have asserted what a constraint already
// guarantees and reported nothing, forever. The assertion worth making is a
// different one: does one request's applied ledger agree with itself about
// which terminal state it reached.
//
// The disagreement is keyed BY REQUEST and not per-side, and that is a
// correction a reviewer forced. A per-side key (one finding for the released
// effect, one for the settled one) is a table that grows with the number of
// conflicting EFFECTS rather than with the number of conflicts: a request with
// three contradictory rows produces three findings, and resolving two of them
// leaves the third unable to suppress the others — so the operator closes the
// same finding three times and none of the three closures means the request's
// history has been looked at. A request-keyed finding is one row per conflict,
// and closing it says what it means.
//
// The two disagreements differ in severity for a reason worth stating, because
// they are the same fact arriving through two of the feed's own doors:
//
//	terminal_split        a settlement-class effect says settled and another
//	                      says released or expired. The class key did its job —
//	                      only the first booked — so the second is precisely
//	                      what the applier quarantines as incoherent, and the
//	                      quarantine is the EXPECTED handling. INFO: no money
//	                      is in question, nothing booked twice, and what the
//	                      operator wants to know is that the runtime closed
//	                      one request twice. It is the runtime's defect, and
//	                      this plane's answer to it was already correct.
//	charged_and_disclaimed a settlement-class effect AND an
//	                      unbillable_orphaned effect on one request. The two
//	                      classes are separate precisely so a request MAY carry
//	                      an orphan beside its settlement, and the schema allows
//	                      both, so this is legal and not a defect. WARNING: the
//	                      orphan's own claim is that it moved no money, and a
//	                      request that was both charged and disclaimed has two
//	                      facts disagreeing about whether it owed anything. Only
//	                      an operator can say which the customer is held to, and
//	                      the two facts are the evidence for that decision.
func (r *Reconciliation) checkDispositionConflict(ctx context.Context, summary *ReconcileSummary, fact persistence.AppliedFact) error {
	switch fact.Kind {
	case ingestion.KindReleased, ingestion.KindExpired:
		settled, err := r.applied.Find(ctx, fact.RequestID, ingestion.ClassSettlement)
		if err != nil {
			return fmt.Errorf("application: reconcile: read the settlement class of request %s: %w", fact.RequestID, err)
		}
		if settled == nil || settled.Kind != ingestion.KindSettled {
			return nil
		}
		// Re-read, because a second terminal fact can be quarantined — the
		// disposition this finding names — and a pass that recorded the split
		// before that write would have recorded a conflict the plane has
		// already handled correctly.
		//
		// A re-read that FAILS is not the same answer as a re-read that found
		// the row changed, and this check refuses to confuse them. F1, F2 and F3
		// propagate a failed confirming read for the same reason: a pass that
		// reports "nothing to see" for a check it could not complete has
		// recorded a clean window it never looked at, and this window is not
		// re-read by anyone — the high-water mark advances past it whatever
		// this check decides. So the failure fails the pass, and the run row
		// keeps the window for the next one.
		confirmed, err := r.applied.Find(ctx, fact.RequestID, ingestion.ClassSettlement)
		if err != nil {
			return fmt.Errorf("application: reconcile: re-read the settlement class of request %s: %w", fact.RequestID, err)
		}
		if confirmed == nil || confirmed.Kind != ingestion.KindSettled {
			return nil
		}
		return r.record(ctx, summary, persistence.Finding{
			CheckKind:   findingTerminalSplit,
			SubjectKind: subjectRequest,
			SubjectID:   fact.RequestID,
			Severity:    severityInfo,
			Observed: observed(map[string]any{
				"later_kind":  fact.Kind,
				"later_seq":   fact.AppendSeq,
				"settled_seq": confirmed.AppendSeq,
			}),
			Detail: "a request carries both a settled effect and a released-or-expired one: the class key let only the first book, so nothing moved twice, but the runtime closed one request twice and the second closure is the one an operator should see",
		})

	case ingestion.KindSettled:
		orphan, err := r.applied.Find(ctx, fact.RequestID, ingestion.ClassUnbillableOrphaned)
		if err != nil {
			return fmt.Errorf("application: reconcile: read the orphan class of request %s: %w", fact.RequestID, err)
		}
		if orphan == nil {
			return nil
		}
		// As in the branch above: a confirming read that could not be completed
		// fails the pass rather than answering it. "I looked and found no
		// orphan" and "I could not look" are different states, and only one of
		// them is a clean window.
		confirmed, err := r.applied.Find(ctx, fact.RequestID, ingestion.ClassUnbillableOrphaned)
		if err != nil {
			return fmt.Errorf("application: reconcile: re-read the orphan class of request %s: %w", fact.RequestID, err)
		}
		if confirmed == nil {
			return nil
		}
		return r.record(ctx, summary, persistence.Finding{
			CheckKind:   findingChargedDisclaimed,
			SubjectKind: subjectRequest,
			SubjectID:   fact.RequestID,
			Severity:    severityWarning,
			Observed: observed(map[string]any{
				"settled_amount": derefInt64(fact.SettledAmount),
				"settled_seq":    fact.AppendSeq,
				"orphan_seq":     confirmed.AppendSeq,
				"orphan_capture": derefString(confirmed.CaptureMethod),
			}),
			Detail: "a request carries both a settlement effect and an unbillable-orphaned effect: the two classes are separate by design and the schema allows both, so this is legal, but two facts disagree about whether the request owed anything and only an operator can say which the customer is held to",
		})

	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// F6 — cursor_missing
// ---------------------------------------------------------------------------

// checkCursorExists is F6, and it is the one check with no window and no
// subject row: the cursor's own existence.
//
// It is scoped to the singleton case deliberately, and the scope is a
// correction a reviewer forced. A first draft also read the cursor's POSITION
// and compared it against the feed's last applied sequence, on the theory that
// a cursor behind the ledger is a stalled consumer. That check is unshippable
// here, and the reason is not whether the comparison is implementable: a
// cursor's position is a Data-Plane-issued opaque string, and this port's own
// doc says parsing it, ordering by it, or validating its shape is a rule the
// whole Control Plane keeps — not a rule about the wire alone. A cursor-lag
// check would be a second reader of that grammar, in the one place that must
// not have one, asserting a staleness this plane cannot measure. It is
// deferred to the day the feed contract exposes a watermark, which is the day
// "is the consumer behind" becomes a question with an answer.
//
// What remains is the one thing about the cursor that IS this plane's own: the
// row must exist if this plane has applied anything. Its absence with effects
// on file means the position was deleted or never written while the effects
// that depend on it are all present — a state in which the next replay pass
// requests the feed from the beginning and re-applies every fact this plane
// ever applied. The applier's class key makes that free in effect and expensive
// in work, and nothing else in the plane notices: this finding is the only
// thing that will.
//
// The finding is recorded against the control plane itself, not a row, because
// there is no row to name — the subject IS the absence.
func (r *Reconciliation) checkCursorExists(ctx context.Context, summary *ReconcileSummary) error {
	position, err := r.cursor.Position(ctx)
	if err != nil {
		return fmt.Errorf("application: reconcile: read the ingestion position: %w", err)
	}
	if position != "" {
		return nil
	}
	// The position says nothing has ever been applied. If the applied ledger
	// says otherwise, the two disagree about the plane's own history, and the
	// replay loop's next read is the consequence.
	//
	// The window is the widest one this port will accept, and deliberately
	// unbounded at both ends: a fact applied at the very first instant of the
	// database's life is exactly the row this check exists to find, and a
	// window that started "now minus the lookback" would miss it. One row is
	// all this needs, so the batch is one.
	effects, err := r.applied.Recent(ctx, time.Time{}, windowCeiling, time.Time{}, "", 1)
	if err != nil {
		return fmt.Errorf("application: reconcile: read the applied ledger: %w", err)
	}
	if len(effects) == 0 {
		return nil // nothing applied and nothing recorded: a plane that has not run yet
	}
	return r.record(ctx, summary, persistence.Finding{
		CheckKind:   checkCursorMissing,
		SubjectKind: subjectControlPlane,
		SubjectID:   subjectControlPlane,
		Severity:    severityCritical,
		Observed: observed(map[string]any{
			"position_present": false,
			"effects_present":  true,
		}),
		Detail: "the ingestion position is empty while the applied ledger holds effects: the next replay pass will request the whole feed from the beginning and re-apply every fact, which the class key makes free in effect and expensive in work, and nothing else in the plane notices it",
	})
}

// windowCeiling is the exclusive upper bound the one unbounded read above
// uses. It is a fixed far-future instant rather than a computed "now plus
// something", because the read must be a constant the database can compare
// against without being told what time it is: timestamptz reaches the year
// 294276, so this is inside the type's range by three centuries and no real
// row will ever be at or after it.
var windowCeiling = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)

// ---------------------------------------------------------------------------
// The two classes this pass cannot decide
// ---------------------------------------------------------------------------

// Neither of the following is implemented, and the reasons are the reason.
//
// "A request this plane has no usage fact for" is undecidable from this
// plane's own rows, and the only request-id population derivable here is
// applied_facts ∪ quarantined_facts. That population is self-fulfilling: a
// request with no fact produces no row in either table, so the query that
// "finds" it finds nothing, and a detector built on it would report on the set
// of requests this plane knows about and call it the set of requests. The
// population that would answer the question lives in the Data Plane, and
// reaching it is a cross-database statement the two-plane architecture (ADR
// 0006 §7) exists to prevent. A pass that computed a "requests seen" count
// and put it beside a "requests with facts" count would be a coverage figure
// that reads as coverage and measures nothing — so there is no such figure
// here, and its absence is the honest one.
//
// "An orphaned reservation or attempt" is undecidable for a different reason.
// This plane holds reservations only as a uuid on a hold leg and a release leg;
// it holds attempts not at all. A hold with no release anywhere is observable
// (this plane wrote both), and a first draft shipped that as
// orphan_hold_unreleased — but its subject would be a reservation whose
// correct resolution is nearly always "the runtime is still working", and this
// plane has no way to tell a live reservation from a dead one. Every honest
// version of the check is a timeout, and the timeout is a policy number this
// plane has no basis to set: the runtime's own admission and reaper horizons
// (B8) own that, and they live in another database. The check was cut rather
// than shipped as a number nobody here can defend.
//
// What the pass DOES do about a stuck hold is nothing, and that is worth
// stating too: a hold with no release is a fact this plane can see and cannot
// resolve, and the resolution — the runtime's reaper releasing the ceiling — is
// B8's job, running in the Data Plane where the reservation's state lives.
// A reconciliation finding is for states that are wrong in the books. A
// reservation in flight is not wrong; it is in flight.

// ---------------------------------------------------------------------------
// The recording path
// ---------------------------------------------------------------------------

// record is the one write every check funnels through, and the place the
// re-read-confirms rule and the idempotency bargain meet.
//
// The timestamps are the pass's database clock, read at the moment the finding
// is recorded: a finding's detected_at and last_seen_at are the same instant
// on its first sighting, and Open overwrites last_seen_at alone on every
// re-confirmation. A process wall clock stamping a durable record's history
// would be two clocks disagreeing about one fact — the same reason every
// writer in this plane reads transaction_timestamp().
//
// The created flag is the whole dedup story. Open converges on the open row
// for the key, so a pass that finds the same divergence twice records one row
// and counts the second sighting as unchanged — a re-confirmation, not a new
// finding. A worker that wrote a row per pass would grow a table without bound
// while failing its only question, which is what is open right now.
func (r *Reconciliation) record(ctx context.Context, summary *ReconcileSummary, finding persistence.Finding) error {
	at, err := r.clock.Now(ctx)
	if err != nil {
		return fmt.Errorf("application: reconcile: read the database clock: %w", err)
	}
	finding.DetectedAt = at
	finding.LastSeenAt = at
	created, err := r.findings.Open(ctx, finding)
	if err != nil {
		return fmt.Errorf("application: reconcile: open finding %s/%s: %w", finding.CheckKind, finding.SubjectKind, err)
	}
	if created {
		summary.FindingsOpened++
	} else {
		summary.FindingsUnchanged++
	}
	return nil
}

// observed encodes a check's evidence as the jsonb the findings table stores.
// The shape is a check's own business, which is why the column carries no
// schema and a new check is a new evidence shape rather than a migration.
//
// The encoding error is swallowed into an empty object, and the reason is that
// every value in these maps is an int64, a string, a bool, a []string or a
// map[string]int64 that this file built — all of which json.Marshal handles by
// construction. A shape json cannot encode would be a bug in the check, and
// swallowing it would hide it. The alternative — panicking — is the right
// shape for a value that cannot happen, and the alternative chosen here is the
// empty object, so that a bug in a check degrades into a finding with no
// evidence rather than a pass that crashes between two findings and takes the
// other checks with it. The evidence being absent is visible; the pass
// stopping is not.
func observed(fields map[string]any) []byte {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

// shapeKey renders a leg multiset as the stable string the leg-shape findings
// key themselves on. Sorted because Go map iteration order is deliberately
// unspecified, and a key built from an unsorted map would be a different key
// on every pass — which would make the finding open a fresh row on every pass,
// the exact failure the shape-in-key design exists to prevent.
func shapeKey(byKind map[string]int64) string {
	kinds := make([]string, 0, len(byKind))
	for kind := range byKind {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, kind+"="+strconv.FormatInt(byKind[kind], 10))
	}
	return strings.Join(parts, ",")
}

// windowLabel renders a window bound for an error message, in UTC and with
// nanosecond precision. The precision is not decoration: the half-open window
// is the pass's contract with the next one, and an error that rounded a bound
// to the second would be an error that cannot be acted on — the difference
// between two bounds is often the whole question. The instant itself is not
// sensitive: these are database clocks, and the message is a log line about
// work, not a record of any request.
func windowLabel(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}

func derefInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func derefString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}
