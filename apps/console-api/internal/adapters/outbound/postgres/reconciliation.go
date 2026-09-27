package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The reconciliation repositories: translation between the persistence port's
// findings and runs and the `control` database's reconciliation tables
// (control migration 000009).
//
// This file is where the detect-and-report design stops, and it is worth
// naming what that means mechanically: there is no statement here that writes
// a ledger entry, moves a balance, or records an adjustment, because the port
// this file implements has no method that could ask for one. That is not a
// restraint exercised at each call site — it is a shape the interface cannot
// be bent out of. A correction to an append-only ledger is a NEW adjustment
// leg requiring an operator_id, and there is no operator here but a human; a
// machine principal would be a grammar decision about operators that has not
// been made, and the migration's comment says the ledger must not guess at its
// grammar. So the worker finds, records, and stops, and everything below is
// either a read or a write to a findings/runs row.
//
// The mechanics that matter, in the order they appear:
//
//   - Open is ON CONFLICT DO NOTHING against the partial unique index over
//     status = 'open', and its answer is the rows-affected count. That is the
//     same mechanic the settlement header's exactly-once edge uses, and it is
//     what makes a re-run converge instead of duplicating: a pass that sees
//     the same divergence twice opens one row and reports it twice as
//     "unchanged", never two rows.
//   - The re-confirmation of an already-open finding is a single narrow UPDATE
//     of last_seen_at through the conflict arm, guarded by the same partial
//     index. The identity trigger on the table allows that one column and
//     refuses every other, so the re-confirmation cannot smuggle in an evidence
//     rewrite even by accident.
//   - Finish is compare-and-set on the status the caller read, for the same
//     reason the bucket close and the cursor advance are: a verdict already
//     recorded is not something a second finisher may overwrite.
//   - Every window read here is bounded by an explicit LIMIT, in the adapter
//     and not only at the call site, because a pass that overran its deadline
//     with an unbounded read is a pass that is still running when the process
//     is asked to stop.

const (
	// maxDetailRunes is the detail length the schema records
	// (reconciliation_findings_detail_grammar). The detail is one sentence for
	// the operator who resolves the finding, not evidence, so a check that
	// writes one longer than the column takes is truncated at the record rather
	// than losing the whole finding to a CHECK failure.
	maxDetailRunes = 2048
)

// NewReconciliationFindings returns the persistence port's findings
// repository, backed by store. It panics on a nil store for the reason every
// constructor in this package does: the failure a nil dependency produces
// later is strictly worse than a loud one here.
func NewReconciliationFindings(store persistence.Store) persistence.ReconciliationFindings {
	if store == nil {
		panic("postgres: NewReconciliationFindings requires a non-nil persistence.Store")
	}
	return &reconciliationFindingsRepo{store: store}
}

// NewReconciliationRuns returns the persistence port's runs repository,
// backed by store, and panics on a nil store for the same reason.
func NewReconciliationRuns(store persistence.Store) persistence.ReconciliationRuns {
	if store == nil {
		panic("postgres: NewReconciliationRuns requires a non-nil persistence.Store")
	}
	return &reconciliationRunsRepo{store: store}
}

// Compile-time proof that the repositories satisfy the port's contracts.
var (
	_ persistence.ReconciliationFindings = (*reconciliationFindingsRepo)(nil)
	_ persistence.ReconciliationRuns     = (*reconciliationRunsRepo)(nil)
)

// ---------------------------------------------------------------------------
// reconciliation_findings — the durable record of what was found.
// ---------------------------------------------------------------------------

type reconciliationFindingsRepo struct {
	store persistence.Store
}

// The insert names every identity and evidence column, and leaves the two
// lifecycle columns to their defaults (status 'open', resolved_at NULL). The
// conflict target is the dedup key the port states: one open finding per
// (check_kind, subject_kind, subject_id), and the partial unique index is the
// arbiter of it.
//
// DO NOTHING, and the answer is the RETURNING row's presence — not the
// statement's rows-affected count. That distinction is the whole design and
// the integration suite is what proved it: DO UPDATE reports one row whether
// it inserted or updated, so an adapter reading RowsAffected would report
// every re-confirmation as a brand-new finding, and the pass's opened/
// unchanged split would be inverted with the table still holding one row. The
// one-statement upsert can only be discriminated by a system column
// (RETURNING xmax = 0), which is a fact about storage internals; the two
// statements below say the same thing in the vocabulary the port itself uses,
// and at a cost the write path of a background worker cannot feel: the
// conflicting case carries one index probe on the identity of a row that is
// already in the index either way.
const insertFinding = `
INSERT INTO control.reconciliation_findings
    (check_kind, subject_kind, subject_id, severity, detected_at, last_seen_at, observed, detail)
VALUES ($1, $2, $3, $4, $5, $5, $6, $7)
ON CONFLICT (check_kind, subject_kind, subject_id) WHERE status = 'open'
DO NOTHING
RETURNING id`

// The re-confirmation, issued only when the insert above returned no row —
// that is, when an open finding already holds this key.
//
// It is the only UPDATE this repository ever issues other than the status
// move, it touches last_seen_at and nothing else, and the absence of the
// update arm from the insert above is deliberate rather than missed: without
// it, a second Open of an unchanged divergence would report false and touch
// nothing, so last_seen_at would freeze at the first sighting and a finding
// open for a year would carry a year-old last_seen_at. That is worse than
// useless — it is a field whose whole meaning is "and it is still true" and
// which would say otherwise.
//
// The identity trigger is what keeps the two statements honest together: this
// one cannot smuggle in an evidence rewrite, so the evidence a reader sees is
// always the evidence the finding was opened with, however many times it has
// been re-confirmed since.
const touchFindingLastSeen = `
UPDATE control.reconciliation_findings
SET last_seen_at = $4::timestamptz
WHERE check_kind = $1
  AND subject_kind = $2
  AND subject_id = $3
  AND status = 'open'`

// The one sanctioned lifecycle UPDATE, narrow twice over: the statement sets
// only the lifecycle columns, and the table's trigger refuses any change to the
// identity or evidence however it is written. The resolved_at companion is
// set or cleared from the target status inside the statement rather than
// passed, so a caller cannot write a status and a resolved_at that disagree —
// the shape CHECK would catch it, and catching it there would turn a caller's
// arithmetic mistake into a failed pass.
//
// The two casts are load-bearing and the reason is easy to get wrong. A CASE's
// result type is fixed from its branches, and the ELSE arm's bare NULL is
// untyped — so $4 appears in that arm first and is coerced to the CASE's
// resolved type, which is TEXT. The same parameter then reaches
// last_seen_at, a timestamptz column, as text, and the statement is refused:
// "column last_seen_at is of type timestamp with time zone but expression is of
// type text". A transition to 'open' or 'acknowledged' is a status-only move
// that could never commit, and a caller reading that as a defect in its own
// status vocabulary would be wrong. $4::timestamptz in the CASE is what pins
// the CASE's type, and the cast on the other arm is what makes that fix reach
// the column.
//
// Note what RowsAffected does and does not mean on the two statements that
// use it. HERE, n == 1 is the compare-and-set's answer: the WHERE carries the
// status the caller believed it was moving from, so one row means the
// transition won. It is not the same measurement as the one Open declines to
// use — ON CONFLICT DO UPDATE reports one row for both of its arms and cannot
// tell an insert from an update, which is why Open reads a RETURNING row's
// presence instead.
const updateFindingStatus = `
UPDATE control.reconciliation_findings
SET status = $3::text,
    resolved_at = CASE WHEN $3::text IN ('resolved', 'ignored') THEN $4::timestamptz ELSE NULL END,
    last_seen_at = $4::timestamptz
WHERE id = $1 AND status = $2::text`

const countOpenFindings = `
SELECT count(*)
FROM control.reconciliation_findings
WHERE status = 'open'`

func (r *reconciliationFindingsRepo) Open(ctx context.Context, finding persistence.Finding) (bool, error) {
	// The empty object, never nil. observed is NOT NULL with a '{}' default,
	// and the insert names the column — so the default does not apply to a
	// bound NULL, and a finding recorded without evidence fails the whole pass
	// on a NOT NULL rather than on anything about the divergence. The port
	// permits an empty Observed, so an empty one has to mean an empty object.
	observed := "{}"
	if len(finding.Observed) > 0 {
		observed = string(finding.Observed)
	}
	var id int64
	err := r.store.Querier(ctx).QueryRowContext(ctx, insertFinding,
		finding.CheckKind, finding.SubjectKind, finding.SubjectID, finding.Severity,
		finding.DetectedAt, observed, truncateDetail(finding.Detail)).Scan(&id)
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, sql.ErrNoRows):
		// Nothing here is a race the caller could converge on: the one
		// uniqueness constraint the insert can lose to is the partial open-key
		// index, and DO NOTHING absorbs it. Whatever else fires is the
		// database refusing a finding on its own grammar — a check_kind over
		// 64 runes, a severity outside the three words — and that is a defect
		// in the check, not a conflict to retry.
		return false, conflictOf(fmt.Errorf("postgres: open reconciliation finding %s/%s/%s: %w",
			finding.CheckKind, finding.SubjectKind, finding.SubjectID, err))
	}

	// An open finding already holds this key. The re-confirmation is issued
	// unconditionally rather than on a row count it cannot get: a zero count
	// means one of two things — the finding was resolved between the two
	// statements, so there is nothing to re-confirm, or the identity of the
	// open key moved — and both leave exactly the same record, which is the
	// one this call is reporting.
	if _, err := r.store.Querier(ctx).ExecContext(ctx, touchFindingLastSeen,
		finding.CheckKind, finding.SubjectKind, finding.SubjectID, finding.DetectedAt); err != nil {
		return false, fmt.Errorf("postgres: re-confirm reconciliation finding %s/%s/%s: %w",
			finding.CheckKind, finding.SubjectKind, finding.SubjectID, err)
	}
	return false, nil
}

// findingStatuses is the findings' lifecycle vocabulary, restated from the
// migration's CHECK so a caller's word is checked against one list. The schema
// would accept a differently-cased spelling of the same word, and the rest of
// the table — the open count, the re-confirmation, the partial unique index —
// would not, so the check has to be the case-sensitive one.
var findingStatuses = []string{"open", "acknowledged", "resolved", "ignored"}

func validFindingStatus(status string) bool {
	for _, known := range findingStatuses {
		if status == known {
			return true
		}
	}
	return false
}

func (r *reconciliationFindingsRepo) Restatus(ctx context.Context, id int64, from, to string, at time.Time) (bool, error) {
	// Both statuses are checked here, and a status outside the vocabulary is
	// refused rather than written. The schema's CHECK is `status IN (…)`,
	// which is case-SENSITIVE, so a caller writing "Open" produces a row that
	// satisfies the constraint and matches nothing else: the open count misses
	// it, the re-confirmation's WHERE misses it, and the partial unique index
	// the whole dedup story rests on is built on a different spelling of the
	// same word. A finding in that state cannot be resolved, re-confirmed or
	// found, and it holds no key that frees — so the error is here, before the
	// statement, where a caller can read what word it got wrong.
	//
	// from = to is refused for the same class of reason and not the schema's:
	// it sets the status to itself, moves last_seen_at backwards, and reports
	// a transition that did not happen. A caller that means "I saw it again"
	// wants the touch, which the pass's own re-confirmation already issues.
	if !validFindingStatus(from) || !validFindingStatus(to) {
		return false, conflictOf(fmt.Errorf("postgres: restatus reconciliation finding %d: status %q -> %q is outside the vocabulary %v",
			id, from, to, findingStatuses))
	}
	if from == to {
		return false, conflictOf(fmt.Errorf("postgres: restatus reconciliation finding %d: status %q -> %q is not a transition",
			id, from, to))
	}
	res, err := r.store.Querier(ctx).ExecContext(ctx, updateFindingStatus, id, from, to, at)
	if err != nil {
		return false, conflictOf(fmt.Errorf("postgres: restatus reconciliation finding %d to %s: %w", id, to, err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: restatus reconciliation finding %d to %s: read rows affected: %w", id, to, err)
	}
	// False means the world moved: another finisher changed the status this
	// call was told it was moving from, or the row is gone. The caller re-reads
	// rather than overwriting a verdict already recorded — a resolution is a
	// decision, and two decisions must not merge.
	return n == 1, nil
}

func (r *reconciliationFindingsRepo) OpenCount(ctx context.Context) (int, error) {
	var count int
	if err := r.store.Querier(ctx).QueryRowContext(ctx, countOpenFindings).Scan(&count); err != nil {
		return 0, fmt.Errorf("postgres: count open reconciliation findings: %w", err)
	}
	return count, nil
}

// truncateDetail bounds the finding's detail to the length the schema records,
// by runes — the column's CHECK counts characters, not octets, and the
// truncation should give up exactly as much as the column would refuse. The
// discipline is truncateReason's, applied to a different column and for a
// different reason: a reason is a diagnostic the consumer writes about a
// refused fact, while a detail is prose the plane writes about its own finding.
func truncateDetail(detail string) string {
	return clampEvidence(detail, maxDetailRunes)
}

// ---------------------------------------------------------------------------
// reconciliation_runs — the pass history, and the high-water mark with it.
// ---------------------------------------------------------------------------

type reconciliationRunsRepo struct {
	store persistence.Store
}

// The run row is written before the pass does any work, in status 'running'
// with no finished_at — the schema's own shape CHECK makes those two facts
// inseparable, so a row where they disagree cannot be written at all. A pass
// that dies leaves exactly this row behind, and that is the right record: a
// pass that started and never finished is a fact, and hiding it would make a
// wedged worker indistinguishable from an idle one.
//
// started_at is NOT in the column list. The migration defaults it from the
// clock, and 000008's lane convention says why: the worker's own lifecycle
// instants are the engine's facts about its rows, not the caller's claims. A
// first draft named the column and bound it to window_from, which is the
// PREVIOUS pass's window_to — and a pass's start instant is not that. The
// consequence was not cosmetic and not visible in any test: a worker wedged
// for forty minutes wrote started_at one interval back, so the row that exists
// precisely to make a wedged worker legible read as a pass that started a
// minute ago and is still running. The column's own default is the answer, and
// letting the engine fill it is both the lane's doctrine and the only statement
// that cannot be wrong about when the pass began.
//
// window_from and window_to ARE named: they are the pass's decision about what
// it swept, which is a claim rather than a fact about a row, and the migration
// records that they are deliberately not defaulted for the same reason.
//
// RETURNING is not decoration and its id is never a value the caller supplied:
// allocation order is commit order, so the id is where this pass sits in the
// sequence — which is what makes it the one cursor Latest can order by. A
// sequence allocated at a different instant, or a caller-supplied id, would
// make "the newest run" an opinion rather than a fact.
const beginRun = `
INSERT INTO control.reconciliation_runs (scope, status, window_from, window_to)
VALUES ($1, 'running', $2, $3)
RETURNING id`

// Finish is compare-and-set on the status the caller read. The counters are
// written from the same statement that completes the pass, so a run is never
// marked complete with counters that describe a different pass.
//
// The $1 and $2 casts are load-bearing and the first is invisible. WHERE id =
// $1 resolves $1 against the primary key's bigint, and the scan Go binds to
// that column's Go type, so the id must be a typed argument rather than the
// untyped one a []any built with a bare int literal would carry.
const finishRun = `
UPDATE control.reconciliation_runs
SET status = $2::text,
    finished_at = $3::timestamptz,
    buckets_scanned = $4,
    findings_opened = $5,
    findings_unchanged = $6
WHERE id = $1::bigint AND status = $7::text`

// Latest orders by id, never by finished_at. A pass that started and never
// finished has no finished_at at all, and it is still the high-water mark the
// next pass must not move backwards from: the newest row is where the sweep
// stopped, whether or not the pass that stopped there finished. Ordering by
// finished_at instead would let a crashed pass be silently skipped over in
// favour of an older completed one, and the next pass would re-sweep a window
// the crashed pass had already half-covered.
const latestRun = `
SELECT id, scope, status, started_at, finished_at, window_from, window_to,
       buckets_scanned, findings_opened, findings_unchanged
FROM control.reconciliation_runs
ORDER BY id DESC
LIMIT 1`

func (r *reconciliationRunsRepo) Begin(ctx context.Context, scope string, windowFrom, windowTo time.Time) (int64, error) {
	var id int64
	err := r.store.Querier(ctx).QueryRowContext(ctx, beginRun, scope, windowFrom, windowTo).
		Scan(&id)
	if err != nil {
		// The claim losing is its own answer, and it is the ONLY unique
		// violation this statement can raise — the run row's identity is a
		// sequence and the caller never supplies it. So a violation here is
		// exactly the window another running pass holds, and translating it
		// to a sentinel is what lets the caller treat "someone else is doing
		// this window" as the ordinary outcome it is rather than as a failure
		// to log every tick.
		if state, ok := sqlStateOf(err); ok && state == pgUniqueViolation {
			return 0, fmt.Errorf("postgres: begin reconciliation run over [%s, %s): %w",
				windowFrom.UTC().Format(time.RFC3339Nano), windowTo.UTC().Format(time.RFC3339Nano),
				persistence.ErrWindowClaimed)
		}
		return 0, conflictOf(fmt.Errorf("postgres: begin reconciliation run over [%s, %s): %w",
			windowFrom.UTC().Format(time.RFC3339Nano), windowTo.UTC().Format(time.RFC3339Nano), err))
	}
	return id, nil
}

func (r *reconciliationRunsRepo) Finish(ctx context.Context, id int64, from, to string, finishedAt time.Time, counters persistence.RunCounters) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, finishRun,
		id, to, finishedAt, counters.BucketsScanned, counters.FindingsOpened, counters.FindingsUnchanged, from)
	if err != nil {
		return false, conflictOf(fmt.Errorf("postgres: finish reconciliation run %d as %s: %w", id, to, err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: finish reconciliation run %d as %s: read rows affected: %w", id, to, err)
	}
	return n == 1, nil
}

func (r *reconciliationRunsRepo) Latest(ctx context.Context) (persistence.Run, error) {
	var run persistence.Run
	var finishedAt sql.NullTime
	err := r.store.Querier(ctx).QueryRowContext(ctx, latestRun).Scan(
		&run.ID, &run.Scope, &run.Status, &run.StartedAt, &finishedAt,
		&run.WindowFrom, &run.WindowTo,
		&run.BucketsScanned, &run.FindingsOpened, &run.FindingsUnchanged)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistence.Run{}, fmt.Errorf("postgres: latest reconciliation run: %w", persistence.ErrNotFound)
		}
		return persistence.Run{}, fmt.Errorf("postgres: latest reconciliation run: %w", err)
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		run.FinishedAt = &t
	}
	return run, nil
}

// ---------------------------------------------------------------------------
// The sweep reads — every member B13 added to the other repositories.
// ---------------------------------------------------------------------------

// The bucket walk. The exclusive lower bound on the uuid is the keyset, and
// the ORDER BY is not decoration: without it PostgreSQL is free to return the
// qualifying rows in any order, and a caller paging on the last id it saw
// would be paging on an arbitrary one. The LIMIT is in the statement because
// an unbounded read of an unbounded table is how a pass outruns its deadline.
const sweepFundingBuckets = `
SELECT ` + fundingBucketColumns + `
FROM control.funding_buckets
WHERE id > $1
ORDER BY id
LIMIT $2`

func (r *bucketRepo) Sweep(ctx context.Context, after accounting.FundingBucketID, limit int) ([]accounting.Bucket, error) {
	if limit < 1 {
		return nil, fmt.Errorf("postgres: sweep funding buckets: limit must be at least 1, got %d", limit)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, sweepFundingBuckets, string(after), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: sweep funding buckets after %s: %w", after, err)
	}
	defer func() { _ = rows.Close() }()

	buckets := []accounting.Bucket{}
	for rows.Next() {
		b, err := scanFundingBucketRow(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: sweep funding buckets after %s: %w", after, err)
		}
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: sweep funding buckets after %s: %w", after, err)
	}
	return buckets, nil
}

// The settlement's own legs, aggregated in the database rather than read out
// one row at a time. Two statements rather than one is a deliberate split: the
// header is a miss-or-one read (ErrNotFound is a real answer a check needs),
// while the multiset is a group-by that returns no row for a settlement with no
// legs — which is a legitimate shape, the zero-priced settle, and not a miss.

const selectSettlementByID = `
SELECT id, request_id, settled_total, created_at
FROM control.settlements
WHERE id = $1`

// The per-kind multiset and the two bucket counts. The bucket counts are
// computed in a CTE first because PostgreSQL has no FILTER over a window
// function and no subquery inside an aggregate FILTER, and the honest
// formulation of "buckets with a consume and no release" is a per-bucket
// grouping: the CTE collapses the settlement's legs to one row per bucket
// carrying whether that bucket has a consume, whether it has a release, and
// how much it consumed, and the outer aggregate turns those rows into the two
// counts and the consume sum.
//
// SUM is cast back to bigint so an overflow is a loud error at the driver
// rather than a silently widened numeric scan — the same cast DerivedBalances
// makes and the same reason.
//
// A settlement with no legs makes the CTE empty, and an aggregate over an
// empty set is one row of zeros, so the zero-priced settle needs no special
// case here at all. That is why the HAVING is not present: it would only be
// needed to suppress a row, and suppressing it would turn a legitimate answer
// into a miss.
const settlementLegMultiset = `
WITH per_bucket AS (
    SELECT funding_bucket_id,
           bool_or(kind = 'consume') AS has_consume,
           bool_or(kind = 'release') AS has_release,
           sum(CASE WHEN kind = 'consume' THEN amount ELSE 0 END) AS consume_sum
    FROM control.ledger_entries
    WHERE settlement_id = $1
    GROUP BY funding_bucket_id
)
SELECT count(*)::bigint,
       count(*) FILTER (WHERE has_consume AND NOT has_release)::bigint,
       COALESCE(sum(consume_sum), 0)::bigint
FROM per_bucket`

// The per-kind counts, and the total leg count beside them. Separate from the
// aggregate above because a caller that wants the multiset wants the SHAPE, and
// the shape is exact here where the filtered bucket count above is
// deliberately conservative.
//
// The kind literals are the domain's own, restated as SQL. A store statement
// cannot import the Go enum, and the alternative — deriving the kinds from a
// table of them — would be a second source of truth for a six-value vocabulary
// that a CHECK constraint already pins on the column: a kind outside this
// vocabulary cannot be in the table for the statement to miss. The names are
// the seam between the domain and the store, and they are written out where a
// reader can see them rather than hidden behind a parameter the engine has no
// way to validate.
const settlementLegKinds = `
SELECT kind, count(*)
FROM control.ledger_entries
WHERE settlement_id = $1
GROUP BY kind`

func (r *settlementRepo) Ledger(ctx context.Context, settlementID accounting.SettlementID) (persistence.SettlementLedger, error) {
	var out persistence.SettlementLedger
	q := r.store.Querier(ctx)
	err := q.QueryRowContext(ctx, selectSettlementByID, string(settlementID)).
		Scan(&out.Settlement.ID, &out.Settlement.RequestID, &out.Settlement.SettledTotal, &out.Settlement.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistence.SettlementLedger{}, fmt.Errorf("postgres: settlement ledger of %s: %w", settlementID, persistence.ErrNotFound)
		}
		return persistence.SettlementLedger{}, fmt.Errorf("postgres: settlement ledger of %s: %w", settlementID, err)
	}

	if err := q.QueryRowContext(ctx, settlementLegMultiset, string(settlementID)).
		Scan(&out.Buckets, &out.BucketsWithoutRelease, &out.ConsumeSum); err != nil {
		return persistence.SettlementLedger{}, fmt.Errorf("postgres: settlement ledger of %s: aggregate legs: %w", settlementID, err)
	}

	rows, err := q.QueryContext(ctx, settlementLegKinds, string(settlementID))
	if err != nil {
		return persistence.SettlementLedger{}, fmt.Errorf("postgres: settlement ledger of %s: read leg kinds: %w", settlementID, err)
	}
	defer func() { _ = rows.Close() }()

	out.LegsByKind = map[string]int64{}
	for rows.Next() {
		var kind string
		var count int64
		if err := rows.Scan(&kind, &count); err != nil {
			return persistence.SettlementLedger{}, fmt.Errorf("postgres: settlement ledger of %s: scan leg kind: %w", settlementID, err)
		}
		out.LegsByKind[kind] = count
		out.Legs += count
	}
	if err := rows.Err(); err != nil {
		return persistence.SettlementLedger{}, fmt.Errorf("postgres: settlement ledger of %s: read leg kinds: %w", settlementID, err)
	}
	return out, nil
}

// The applied-fact window. The bound is applied_at — when this plane recorded
// the derivation — and not the fact's occurred_at, which is the Data Plane's
// clock: ordering one plane's records by another plane's timestamps is how a
// modest clock skew becomes a fact applied outside the window it was applied
// in. The (applied_at, request_id) order is a tiebreak and not decoration:
// applied_at alone is not unique, and a caller paging on the last row it saw
// needs a total order to page on.
const recentAppliedFacts = `
SELECT request_id, kind_class, kind, append_seq, settled_amount, capture_method, settlement_id, applied_at
FROM control.applied_facts
WHERE applied_at >= $1 AND applied_at < $2
  AND (applied_at, request_id) > ($3, $4)
ORDER BY applied_at, request_id
LIMIT $5`

// The keyset is a ROW comparison on the pair, which is the one form that
// PostgreSQL can use as a single index condition against
// applied_facts_applied_at_idx (applied_at, request_id) — the index is the
// ordering and the bound is the same ordering, so a page is a walk rather than
// a filter over a scan.
//
// A bound on applied_at alone would be simpler and would be wrong: a whole
// ingestion page commits in one transaction, so all of its rows carry one
// applied_at, and resuming strictly after that instant skips every row of the
// page but the last one the read saw. With a batch of 500 and pages of 100
// that is most of a window's rows, silently, with the run row stamped
// completed.
//
// $3 is a bare comparison against a timestamptz column, so its type is fixed
// on first appearance; $4 is compared to a text column in the same row
// expression. The zero time and the empty string are the keyset's origin: no
// applied row carries either, so the first page is the window's whole
// beginning and neither value needs a sentinel column to mean "nothing yet".
func (r *appliedFactsRepo) Recent(ctx context.Context, from, to, after time.Time, afterID string, limit int) ([]persistence.AppliedFact, error) {
	if limit < 1 {
		return nil, fmt.Errorf("postgres: recent applied facts: limit must be at least 1, got %d", limit)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, recentAppliedFacts, from, to, after, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: recent applied facts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	facts := []persistence.AppliedFact{}
	for rows.Next() {
		var fact persistence.AppliedFact
		var settlementID sql.NullString
		if err := rows.Scan(&fact.RequestID, &fact.KindClass, &fact.Kind, &fact.AppendSeq,
			&fact.SettledAmount, &fact.CaptureMethod, &settlementID, &fact.AppliedAt); err != nil {
			return nil, fmt.Errorf("postgres: recent applied facts: scan: %w", err)
		}
		fact.SettlementID = settlementID.String
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: recent applied facts: %w", err)
	}
	return facts, nil
}
