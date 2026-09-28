package persistence

import (
	"context"
	"time"
)

// The reconciliation worker's durable surface, in the store's vocabulary:
// the findings a pass records for the divergences it found, and the runs that
// say when a pass happened (control migration 000009).
//
// What this port is NOT, and cannot be made into by a future change to it, is
// a repair path. There is deliberately no method here that writes a ledger
// entry, moves a balance, or records an adjustment — and that absence is the
// design rather than an omission, for the reason the migration header states
// and repeats here because a reader of the interface is exactly the reader who
// needs it: a correction to an append-only ledger is a NEW adjustment leg
// requiring an operator_id, and a machine principal would be a grammar
// decision about operators that has not been made. The worker finds, records
// and stops. An operator decides, and the ledger's word is the only door.
//
// The two identities that matter here, restated because every method's
// behaviour follows from them:
//
//   - A finding's identity is (CheckKind, SubjectKind, SubjectID) and nothing
//     else. It is not a message string, not a timestamp, and not a run id, and
//     a finding whose key carried any of those would be a new row on every
//     pass — a table that grows without bound while failing its only question,
//     which is what is open right now. Open converges on the open row; a
//     resolution frees the key for a genuine later recurrence.
//   - A run's identity is its own row, and its window is half-open: from is
//     inclusive, to exclusive. Two passes tile the timeline with no overlap
//     and no gap, which is the only shape in which a row cannot be swept
//     twice or skipped between two windows.

// Finding is one recorded divergence: the entity a check named, the invariant
// it violated, the evidence the check compared, and how much it matters.
//
// The evidence is opaque to this port on purpose. It is structured jsonb —
// what a given check puts in it is that check's business, and a new check is a
// new evidence shape rather than a migration. It is bounded by nothing, for
// the reason the migration header gives: every writer is one of this plane's
// own checks, never a provider blob, never a raw prompt, never a fact payload.
type Finding struct {
	// CheckKind names the invariant that was violated. It is part of the
	// finding's identity, so it is a closed vocabulary this plane owns rather
	// than free text, and two checks with different names are different
	// findings even about the same subject.
	CheckKind string

	// SubjectKind and SubjectID name the entity the invariant was violated on:
	// a funding bucket, a settlement, a request, or the literal control_plane
	// for a feed-wide signal. Together with CheckKind they are the dedup key.
	SubjectKind string
	SubjectID   string

	// Severity is how much the divergence matters: info (recorded for the
	// record), warning (something is inconsistent and an operator should look)
	// or critical (money or the feed's ability to bill is in question). It is
	// written once and never moved — a finding whose severity is editable
	// would let a reader re-grade the evidence instead of resolving it.
	Severity string

	// Observed is the evidence the check compared, as this plane's checks
	// wrote it. Empty is an honest value (a singleton check with no figures
	// to compare), never a placeholder.
	Observed []byte

	// Detail is one sentence for the human who resolves the finding. It is
	// prose, not evidence, and it never carries a payload: the figures belong
	// in Observed, where they can be compared.
	Detail string

	// DetectedAt is the instant the first pass saw this divergence, and
	// LastSeenAt the instant the most recent pass saw it still true. Both are
	// supplied by the caller's database clock so the two agree with the pass's
	// window to the microsecond — and neither is part of the identity.
	DetectedAt time.Time
	LastSeenAt time.Time
}

// ReconciliationFindings is where a detected divergence is recorded, and it is
// the only durable state this design produces besides the runs.
type ReconciliationFindings interface {
	// Open records finding as the one open finding for its
	// (CheckKind, SubjectKind, SubjectID), and reports whether this call
	// created it. It MUST be idempotent under re-run: a second Open for a
	// key an open finding already holds creates nothing and reports false,
	// and only touches that row's last_seen_at — which is the whole reason a
	// re-run over unchanged data opens zero findings and merely re-confirms
	// the ones already there.
	//
	// It deliberately does not refuse when the key is taken. A refusal would
	// make the caller treat a re-seen divergence as an error, and the caller
	// would have to distinguish "new" from "known" anyway; returning whether
	// the row is new is the answer, and the partial unique index over
	// status = 'open' is the arbiter (the same ON CONFLICT DO NOTHING
	// mechanic the settlement header's exactly-once edge uses).
	//
	// It is NOT unit-of-work-shaped: a finding outlives the pass that found
	// it by design, and a pass that failed after recording findings has
	// still recorded them truthfully. It does not resolve, and it does not
	// move money.
	Open(ctx context.Context, finding Finding) (created bool, err error)

	// Restatus moves one finding from its current status to `to`, and reports
	// whether it did. It is the ONE sanctioned UPDATE on this table, and it is
	// narrow twice over: the statement sets only the lifecycle columns
	// (status, resolved_at, last_seen_at), and the table's own trigger refuses
	// any change to the identity or the evidence however it is written. The
	// move is compare-and-set on the status the caller read, so a transition
	// that raced another operator's refuses rather than overwrites — a
	// resolution is a decision, and two decisions must not merge.
	//
	// at is the instant from the caller's database clock, and the same value
	// stamps resolved_at and last_seen_at: a finding closed at the moment the
	// decision was made, and re-confirmed at the same instant if the status
	// moved back towards open. The caller supplies it rather than the
	// repository, for the reason every other timestamp in this port does — a
	// process wall clock deciding a durable record's history is two clocks
	// disagreeing about one fact.
	//
	// It deliberately cannot be a repair. Nothing here reaches a balance, a
	// leg or a settlement: acknowledging that a customer was over-settled is
	// a record, and the adjustment that would actually refund them is a
	// separate, operator-authorised act this port has no method for.
	Restatus(ctx context.Context, id int64, from, to string, at time.Time) (bool, error)

	// OpenCount returns how many findings are open. It is the pass's headline
	// number and the only one the worker logs, because it is the only figure
	// here that is a fact about the whole plane rather than about one subject.
	OpenCount(ctx context.Context) (int, error)
}

// AccountFindings is the console's read of what the worker found, and it is
// the read half of a port that has no repair half.
//
// Every member here is a SELECT. That is not a restraint exercised at each
// call site — the interface has no method that could write one — and the
// absence is the design the package header states: a finding is evidence, not
// a repair path. Acknowledging one records an operator's decision, and a
// correction to an append-only ledger is a NEW adjustment leg carrying an
// operator_id, whose grammar is a decision that has not been made
// (accounting.OperatorID). Nothing on this page resolves, refunds or
// adjusts, and a member added later that did would be a repair path wearing a
// read's name.
//
// No total. OpenCount above is the worker's own headline, a fact about the
// whole plane; the list below returns a page and never a count of it.
type AccountFindings interface {
	// List returns at most page.Limit findings, newest first, keyed on id.
	//
	// There is no account predicate here and there cannot be one: a
	// finding's subject may be a funding bucket, a settlement, a request, or
	// the literal control_plane for a feed-wide signal, and findings are not
	// scoped to one account. What the console's operator may read is a
	// question about the operator's CLASS, and it is answered above this port
	// — the findings are a whole-plane fact in the same way open_finding_count
	// is, and the contract says so in those words.
	//
	// The id is the keyset rather than detected_at: it is unique, and
	// detected_at is two passes' clocks colliding on one divergence.
	List(ctx context.Context, page FindingPage) ([]Finding, error)
}

// FindingPage is the findings list's request: the optional status and
// severity filters, the keyset position, and a page size.
type FindingPage struct {
	// Status is one finding status, or empty for every finding open and
	// closed.
	Status string

	// Severity is one severity, or empty for every severity.
	Severity string

	// After is the exclusive lower bound on the id. The zero value is the
	// beginning of the table.
	After int64

	// Limit is the number of rows wanted; the adapter asks for one more.
	Limit int
}

// Run is one reconciliation pass: the half-open window it swept, when it
// started and how it ended, and the three counters that say what it did.
type Run struct {
	// ID is the pass's own identity, allocated by the store — never minted by
	// a caller, for the same reason the projection's revision is never an
	// argument: allocation order is commit order, and a caller-supplied id
	// could only be a guess.
	ID int64

	// Scope names what the pass swept. One value today.
	Scope string

	// StartedAt and FinishedAt are the pass's lifecycle instants from the
	// database's own clock. FinishedAt is nil exactly while the pass is
	// running, and a row where the two disagree is a pass that died between
	// two writes.
	StartedAt  time.Time
	FinishedAt *time.Time

	// Status is running, completed or failed.
	Status string

	// WindowFrom is inclusive and WindowTo exclusive — the pass's half-open
	// window. WindowTo is also the high-water mark the NEXT pass opens from,
	// read through Latest: a recomputed now()-minus-lookback bound would
	// re-derive its own tail forever whenever a pass outran its own interval.
	WindowFrom time.Time
	WindowTo   time.Time

	// BucketsScanned, FindingsOpened and FindingsUnchanged are the counters.
	// Unchanged is the one a re-run shows: the same divergence seen again is
	// a re-confirmation, not a new finding.
	BucketsScanned    int64
	FindingsOpened    int64
	FindingsUnchanged int64
}

// ReconciliationRuns is the pass history, and the high-water mark with it.
type ReconciliationRuns interface {
	// Begin opens a run row for a pass about to cover [windowFrom, windowTo)
	// and returns its id. The row is written BEFORE the pass does its work and
	// is left in status 'running' if the process dies mid-pass — which is the
	// right record: a pass that started and never finished is a fact, and
	// hiding it would make a wedged worker indistinguishable from an idle one.
	//
	// The window halves are the caller's because they are the pass's decision
	// about what it swept, and they must be exactly the ones every read in the
	// pass is bounded by — a window the run records and a window the sweep
	// uses that differ would make the row a claim about work nobody did.
	//
	// Begin is also the WINDOW CLAIM, and the claim is the answer this method
	// has to be able to give: a pass that computes the same window another
	// running pass is already covering is refused, and refused with
	// ErrWindowClaimed rather than a unique-violation string. The caller does
	// not treat that as a failure — there is nothing wrong with it, and the
	// other pass is doing exactly the work this one would have done. It is an
	// error only because a method that can decline has to say so in its
	// signature, and a bool the caller must remember to check is a bool that
	// some caller will not.
	Begin(ctx context.Context, scope string, windowFrom, windowTo time.Time) (int64, error)

	// Finish completes a run, compare-and-set on the status the caller read:
	// status becomes `to`, finished_at the supplied instant, and the three
	// counters are written. False means the world moved — another finisher
	// won, or the row is gone — and the caller re-reads through Latest rather
	// than overwriting a verdict already recorded.
	Finish(ctx context.Context, id int64, from, to string, finishedAt time.Time, counters RunCounters) (bool, error)

	// Latest returns the most recent run, or ErrNotFound when no pass has ever
	// run. It is the high-water mark's only reader: the next pass's window
	// opens at the newest run's WindowTo, so a pass that outruns its own
	// interval still advances instead of re-deriving its own tail. The newest
	// row by id, never by finished_at — a crashed pass has no finish, and a
	// pass that started and never ended is still the high-water mark the
	// next pass must not move backwards from.
	Latest(ctx context.Context) (Run, error)
}

// AccountRuns is the console's read of the pass history.
//
// Read-only, for the same reason AccountFindings is: a run is a record of a
// detection pass, and nothing an operator looking at one may do to it is a
// repair. A run whose FinishedAt is nil is returned as it stands rather than
// hidden — a wedged worker and an idle one are otherwise indistinguishable.
//
// The id is both the identity and the keyset. It is a GENERATED ALWAYS AS
// IDENTITY column, so id order is allocation order, and Latest above already
// orders by it for the reason the high-water mark needs to.
type AccountRuns interface {
	// List returns at most page.Limit runs, newest first, keyed on id. No
	// account predicate, for the reason AccountFindings has none: a pass
	// sweeps the plane.
	List(ctx context.Context, page RunPage) ([]Run, error)
}

// RunPage is the runs list's request: a keyset position and a page size.
type RunPage struct {
	After int64
	Limit int
}

// RunCounters is one pass's tally, kept to its own type so Finish's signature
// states what it writes rather than taking three loose int64s a caller could
// reorder. Every counter is non-negative and the schema refuses a negative one.
type RunCounters struct {
	BucketsScanned    int64
	FindingsOpened    int64
	FindingsUnchanged int64
}
