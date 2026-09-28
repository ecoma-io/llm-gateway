package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The console's reconciliation reads: what the worker found, and the passes
// it made.
//
// Every statement here is a SELECT, and that is the whole of the design
// rather than a restraint exercised per call site. The port these implement
// has no method that could write a repair — no resolve, no refund, no
// adjust — and a member added later that did would be a repair path wearing
// a read's name. A finding is evidence; a correction to an append-only ledger
// is a NEW adjustment leg carrying an operator_id, whose grammar is a
// decision that has not been made (accounting.OperatorID), and it is not a
// button on a table.
//
// Neither list carries an account predicate, and that is the contract's own
// statement rather than a gap: a finding's subject may be a funding bucket, a
// settlement, a request, or the literal control_plane for a feed-wide signal,
// and a pass sweeps the plane. The dashboard says the same of
// open_finding_count — it is "the one number here that is not the account's
// alone". What an operator may read is a question about the operator's class,
// and it is answered above this port.
//
// The keyset on both is the identity column, descending. It is unique, and it
// is allocation order, so "newest first" is total and repeatable: ordering
// findings by detected_at instead would be two passes' clocks colliding on
// one divergence, which is exactly the non-total key the rest of this plane
// refuses to page on.

// NewAccountFindings returns the console's findings-list repository and
// NewAccountRuns its run-history repository. Both panic on a nil store for
// the reason every constructor in this package does.
func NewAccountFindings(store persistence.Store) persistence.AccountFindings {
	if store == nil {
		panic("postgres: NewAccountFindings requires a non-nil persistence.Store")
	}
	return &accountFindingsRepo{store: store}
}

func NewAccountRuns(store persistence.Store) persistence.AccountRuns {
	if store == nil {
		panic("postgres: NewAccountRuns requires a non-nil persistence.Store")
	}
	return &accountRunsRepo{store: store}
}

// Compile-time proof that the repositories satisfy the port's contracts.
var (
	_ persistence.AccountFindings = (*accountFindingsRepo)(nil)
	_ persistence.AccountRuns     = (*accountRunsRepo)(nil)
)

// ---------------------------------------------------------------------------
// reconciliation_findings
// ---------------------------------------------------------------------------

type accountFindingsRepo struct {
	store persistence.Store
}

// The two filters are parameters of the same statement, and the keyset is
// the descending id. `observed` is selected as the jsonb it is and is
// returned as the bytes the check wrote: it is structured evidence whose
// shape is that check's business, and nothing on this path interprets it.
//
// The zero-`after` case is `id < 0` against a GENERATED ALWAYS AS IDENTITY
// column that starts at 1, so the first page is the newest finding on the
// table.
const listFindings = `
SELECT id, check_kind, subject_kind, subject_id, severity, status, detected_at, last_seen_at, resolved_at, observed, detail
FROM control.reconciliation_findings
WHERE id < $1
  AND ($2 = '' OR status = $2)
  AND ($3 = '' OR severity = $3)
ORDER BY id DESC
LIMIT $4`

func (r *accountFindingsRepo) List(ctx context.Context, page persistence.FindingPage) ([]persistence.Finding, error) {
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list reconciliation findings: %w", err)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, listFindings,
		page.After, page.Status, page.Severity, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list reconciliation findings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	findings := make([]persistence.Finding, 0, limit)
	for rows.Next() {
		var finding persistence.Finding
		var resolvedAt sql.NullTime
		if err := rows.Scan(&finding.ID, &finding.CheckKind, &finding.SubjectKind,
			&finding.SubjectID, &finding.Severity, &finding.Status, &finding.DetectedAt,
			&finding.LastSeenAt, &resolvedAt, &finding.Observed, &finding.Detail); err != nil {
			return nil, fmt.Errorf("postgres: list reconciliation findings: %w", err)
		}
		if resolvedAt.Valid {
			instant := resolvedAt.Time
			finding.ResolvedAt = &instant
		}
		findings = append(findings, finding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list reconciliation findings: %w", err)
	}
	return findings, nil
}

// ---------------------------------------------------------------------------
// reconciliation_runs
// ---------------------------------------------------------------------------

type accountRunsRepo struct {
	store persistence.Store
}

// The keyset is the descending identity, for the same reason Latest reads the
// newest row by id and never by finished_at: a crashed pass has no finish, so
// an order keyed on finished_at is not an order over the rows at all.
//
// The window is returned as its two halves, and the contract says why: a
// window rendered as an inclusive pair is one an operator cannot tell from a
// window that overlapped the last. `finished_at` is selected as stored and
// stays NULL for a pass that started and never ended, which is a fact worth
// rendering rather than hiding.
const listRuns = `
SELECT id, scope, started_at, finished_at, status, window_from, window_to, buckets_scanned, findings_opened, findings_unchanged
FROM control.reconciliation_runs
WHERE id < $1
ORDER BY id DESC
LIMIT $2`

func (r *accountRunsRepo) List(ctx context.Context, page persistence.RunPage) ([]persistence.Run, error) {
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list reconciliation runs: %w", err)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, listRuns, page.After, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list reconciliation runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	runs := make([]persistence.Run, 0, limit)
	for rows.Next() {
		var run persistence.Run
		var finishedAt sql.NullTime
		if err := rows.Scan(&run.ID, &run.Scope, &run.StartedAt, &finishedAt, &run.Status,
			&run.WindowFrom, &run.WindowTo, &run.BucketsScanned, &run.FindingsOpened,
			&run.FindingsUnchanged); err != nil {
			return nil, fmt.Errorf("postgres: list reconciliation runs: %w", err)
		}
		if finishedAt.Valid {
			instant := finishedAt.Time
			run.FinishedAt = &instant
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list reconciliation runs: %w", err)
	}
	return runs, nil
}
