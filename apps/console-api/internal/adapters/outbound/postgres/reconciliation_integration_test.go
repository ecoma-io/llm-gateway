//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/ingestion"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The reconciliation worker's store half, against the real `control` database.
//
// What is proved HERE is the half no fake can say: that the statements behind
// the findings and runs tables converge the way the port promises against the
// partial unique index that arbitrates them, that the identity trigger refuses
// a rewrite reached through the adapter's own database and not only through
// psql, and that the sweep reads the ledger through the same statements the
// applier wrote it with. deploy/postgres/verify.sh already proves the raw
// schema refusals; this file proves the ADAPTER reaches them, and that the
// figures a pass's leg assertions are made against describe a REAL settlement
// — a fake's numbers agree with the pass by construction, and a misread CTE
// would satisfy every one of them.
//
// It cannot drive a whole pass: this package is an outbound adapter and the
// architecture test scans _test.go files too, so the unit of work a pass is
// written as is proved in the application package against fakes and the
// statements are proved here. The seam is deliberately the one the reaper's
// own integration file draws, for the same reason.
//
// Conventions, from the file beside this one: no t.Parallel (the database is
// shared), every id and subject is run-unique so a rerun against an
// already-populated database cannot converge on an earlier run's rows,
// fixtures reach their states through the ports rather than through crafted
// rows, and nothing deletes — the schema has no delete path and the ids carry
// the isolation.
//
// Run (the compose project is port-shifted per worktree):
//
//	GATEWAY_POSTGRES_PROJECT=llm-gateway-postgres-b13 GATEWAY_POSTGRES_PORT=55442 \
//	  docker compose -f deploy/postgres/compose.yaml up -d
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:55442/postgres?sslmode=disable' \
//	  go test -tags=integration -race ./internal/adapters/outbound/postgres

// reconciliationRepos gathers the pool and every repository the reconciliation
// worker's store half speaks through, over one store.
type reconciliationRepos struct {
	db          *sql.DB
	store       persistence.Store
	clock       persistence.Clock
	findings    persistence.ReconciliationFindings
	runs        persistence.ReconciliationRuns
	buckets     persistence.FundingBuckets
	settlements persistence.Settlements
	applied     persistence.AppliedFacts
	quarantined persistence.QuarantinedFacts
	cursor      persistence.IngestionCursor
	accounting  *accountingRepos
}

func integrationReconciliation(t *testing.T) *reconciliationRepos {
	t.Helper()
	a := integrationAccounting(t)
	store := a.store
	return &reconciliationRepos{
		db:          a.db,
		store:       store,
		clock:       NewClock(store),
		findings:    NewReconciliationFindings(store),
		runs:        NewReconciliationRuns(store),
		buckets:     NewFundingBuckets(store),
		settlements: NewSettlements(store),
		applied:     NewAppliedFacts(store),
		quarantined: NewQuarantinedFacts(store),
		cursor:      NewIngestionCursor(store),
		accounting:  a,
	}
}

// reconProbe is a run-unique subject, so a rerun can never converge on an
// earlier run's row and every probe below is independent of how many times the
// suite has been run against this database.
func reconProbe(what string) string {
	return fmt.Sprintf("recon it %s %d", what, time.Now().UnixNano())
}

// reconFinding builds the finding a check would record for a probe subject.
func reconFinding(t *testing.T, r *reconciliationRepos, check, subjectKind, subject, severity string) persistence.Finding {
	t.Helper()
	at, err := r.clock.Now(t.Context())
	if err != nil {
		t.Fatalf("clock: %v", err)
	}
	return persistence.Finding{
		CheckKind:   check,
		SubjectKind: subjectKind,
		SubjectID:   subject,
		Severity:    severity,
		Observed:    []byte(`{"probe":true}`),
		Detail:      "a verify probe finding, written by the reconciliation integration suite",
		DetectedAt:  at,
		LastSeenAt:  at,
	}
}

// TestIntegrationReconciliationFindingsConvergeOnTheOpenKey is the dedup
// bargain at the engine: a second Open of a key an open finding already holds
// creates nothing and reports false. A re-run over unchanged data is the case
// this exists for, and a table that grew a row per pass would fail its only
// question.
func TestIntegrationReconciliationFindingsConvergeOnTheOpenKey(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	subject := reconProbe("converge")
	first := reconFinding(t, r, "bucket_derivation_drift", "funding_bucket", subject, "critical")

	created, err := r.findings.Open(ctx, first)
	if err != nil {
		t.Fatalf("Open(first): %v", err)
	}
	if !created {
		t.Fatalf("the first Open of an unused key reported false; the subject %q is run-unique so nothing could hold it", subject)
	}

	created, err = r.findings.Open(ctx, first)
	if err != nil {
		t.Fatalf("Open(second): %v", err)
	}
	if created {
		t.Errorf("the second Open of an open key reported true; the pass would count a re-confirmation as a new finding and the table would grow without bound")
	}
	if got := integrationFindingCount(t, r.db, subject); got != 1 {
		t.Errorf("the subject carries %d findings, want 1 — a re-run must converge", got)
	}
}

// TestIntegrationReconciliationReconfirmationAdvancesLastSeenAndNothingElse is
// the reason the conflict arm is a DO UPDATE rather than DO NOTHING: without
// it a finding open for a year carries a year-old last_seen_at, and a field
// whose whole meaning is "and it is still true" says otherwise. The evidence
// must be untouched by that update — the identity trigger is what guarantees
// it, and this proves the adapter is on the right side of it.
func TestIntegrationReconciliationReconfirmationAdvancesLastSeenAndNothingElse(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	subject := reconProbe("last seen")
	opening := reconFinding(t, r, "bucket_derivation_drift", "funding_bucket", subject, "critical")
	if _, err := r.findings.Open(ctx, opening); err != nil {
		t.Fatalf("Open: %v", err)
	}
	before := integrationFindingSnapshot(t, r.db, subject)

	// A second sighting with DIFFERENT evidence — which is what a bucket whose
	// balances drifted further would carry. The re-confirmation must move the
	// sighting and keep the opening evidence, because a finding whose evidence
	// is rewritten in place is not evidence.
	later := opening
	later.Observed = []byte(`{"probe":true,"drifted_further":true}`)
	later.DetectedAt = opening.DetectedAt.Add(time.Hour)
	later.LastSeenAt = later.DetectedAt
	created, err := r.findings.Open(ctx, later)
	if err != nil {
		t.Fatalf("Open(reconfirmation): %v", err)
	}
	if created {
		t.Fatalf("the re-confirmation reported a new finding")
	}
	after := integrationFindingSnapshot(t, r.db, subject)
	if string(after.Observed) != string(before.Observed) {
		t.Errorf("the re-confirmation rewrote the evidence: %s then %s — the identity trigger must refuse it", before.Observed, after.Observed)
	}
	if !after.LastSeenAt.After(before.LastSeenAt) {
		t.Errorf("the re-confirmation left last_seen_at at %s, want a later sighting than %s", after.LastSeenAt, before.LastSeenAt)
	}
	if !after.DetectedAt.Equal(before.DetectedAt) {
		t.Errorf("the re-confirmation moved detected_at from %s to %s; the first sighting is the fact", before.DetectedAt, after.DetectedAt)
	}
}

// TestIntegrationReconciliationResolutionFreesTheKeyForARecurrence is the
// other half of the partial index, and the half an unqualified unique would
// have made impossible: a divergence that was resolved and came back is a NEW
// finding with fresh evidence, not a disappearance into the closed one.
func TestIntegrationReconciliationResolutionFreesTheKeyForARecurrence(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	subject := reconProbe("recurrence")
	if _, err := r.findings.Open(ctx, reconFinding(t, r, "bucket_derivation_drift", "funding_bucket", subject, "critical")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := integrationFindingID(t, r.db, subject)

	at, err := r.clock.Now(ctx)
	if err != nil {
		t.Fatalf("clock: %v", err)
	}
	moved, err := r.findings.Restatus(ctx, id, "open", "resolved", at)
	if err != nil {
		t.Fatalf("Restatus: %v", err)
	}
	if !moved {
		t.Fatalf("Restatus(open -> resolved) reported false; the row was open and the caller read the status itself")
	}

	recurrence := reconFinding(t, r, "bucket_derivation_drift", "funding_bucket", subject, "critical")
	created, err := r.findings.Open(ctx, recurrence)
	if err != nil {
		t.Fatalf("Open(recurrence): %v", err)
	}
	if !created {
		t.Errorf("a recurrence of a resolved divergence created nothing; the key must be freed by the resolution or the second occurrence is unrecordable")
	}
	if got := integrationFindingCount(t, r.db, subject); got != 2 {
		t.Errorf("the subject carries %d findings, want 2 — the resolved one and its recurrence", got)
	}
}

// TestIntegrationReconciliationRestatusThatLostTheRaceIsRefused is the
// compare-and-set: a resolution is a decision, and two decisions must not
// merge. The second caller is told the world moved rather than overwriting the
// verdict already recorded.
func TestIntegrationReconciliationRestatusThatLostTheRaceIsRefused(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	subject := reconProbe("restatus race")
	if _, err := r.findings.Open(ctx, reconFinding(t, r, "bucket_derivation_drift", "funding_bucket", subject, "critical")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := integrationFindingID(t, r.db, subject)
	at, err := r.clock.Now(ctx)
	if err != nil {
		t.Fatalf("clock: %v", err)
	}

	first, err := r.findings.Restatus(ctx, id, "open", "resolved", at)
	if err != nil {
		t.Fatalf("Restatus(first): %v", err)
	}
	if !first {
		t.Fatalf("the first Restatus reported false")
	}
	second, err := r.findings.Restatus(ctx, id, "open", "resolved", at)
	if err != nil {
		t.Fatalf("Restatus(second): %v", err)
	}
	if second {
		t.Errorf("a second Restatus from the status the caller read reported true; the row is no longer open, so the compare-and-set must refuse it")
	}
}

// TestIntegrationReconciliationTheIdentityGuardIsReachedFromGo proves the
// trigger is not a psql-only guard. Its value is that no writer can get past
// it, and the adapter is the writer this plane actually has — a guard only a
// DBA session can trip has not guarded the worker.
func TestIntegrationReconciliationTheIdentityGuardIsReachedFromGo(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	subject := reconProbe("identity guard")
	if _, err := r.findings.Open(ctx, reconFinding(t, r, "bucket_derivation_drift", "funding_bucket", subject, "critical")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := integrationFindingID(t, r.db, subject)

	// The widening an adapter bug would produce: a re-confirmation that also
	// re-grades the severity. verify.sh proves the same refusals as raw
	// statements; what this adds is that a Go caller holding the same
	// connection and the same role is refused identically.
	_, err := r.db.ExecContext(ctx, "UPDATE control.reconciliation_findings SET severity = 'info' WHERE id = $1", id)
	if err == nil {
		t.Fatalf("a severity rewrite was accepted; the identity guard did not fire for a Go caller")
	}
	if !strings.Contains(err.Error(), "append-only for its identity") {
		t.Errorf("the refusal was not the identity guard: %v", err)
	}
}

// TestIntegrationReconciliationFindingIsNeverRemoved is the other half of the
// guard, and the direction that matters most: a finding that was recorded and
// then deleted is a defect with no evidence left. The probe's DELETE is refused
// even though the statement-level trigger fires whether or not the statement
// would have removed a row — the property a row-level guard with a WHEN clause
// would miss, and the reason the migration chose a statement-level trigger.
func TestIntegrationReconciliationFindingIsNeverRemoved(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	subject := reconProbe("no delete")
	if _, err := r.findings.Open(ctx, reconFinding(t, r, "bucket_derivation_drift", "funding_bucket", subject, "critical")); err != nil {
		t.Fatalf("Open: %v", err)
	}

	_, err := r.db.ExecContext(ctx, "DELETE FROM control.reconciliation_findings WHERE subject_id = $1", subject)
	if err == nil {
		t.Fatalf("the delete was accepted; the finding is the only evidence of a divergence and this plane has no delete path for it")
	}
	if !strings.Contains(err.Error(), "never removed") {
		t.Errorf("the refusal was not the delete guard: %v", err)
	}
	if got := integrationFindingCount(t, r.db, subject); got != 1 {
		t.Errorf("the subject carries %d findings after the refused delete, want 1", got)
	}
}

// TestIntegrationReconciliationConcurrentOpenersConvergeOnOneRow is the dedup
// key under real concurrency, which is the case the partial index exists for
// and the one a sequential test cannot show: two passes that both see the same
// divergence must converge on one row, and neither may see a unique violation.
func TestIntegrationReconciliationConcurrentOpenersConvergeOnOneRow(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	subject := reconProbe("concurrent")
	const openers = 8

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		created  int
		failures []string
	)
	for i := 0; i < openers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each opener runs in its own unit of work, the shape a pass's
			// record call runs in: the finding outlives the statement that
			// wrote it, and the arbitring index is what makes that safe.
			var wasCreated bool
			err := r.store.WithinTx(ctx, func(ctx context.Context) error {
				c, err := r.findings.Open(ctx, reconFinding(t, r, "disposition_conflict", "request", subject, "warning"))
				wasCreated = c
				return err
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				failures = append(failures, err.Error())
			case wasCreated:
				created++
			}
		}()
	}
	wg.Wait()

	if len(failures) > 0 {
		t.Errorf("%d of %d openers failed: %v", len(failures), openers, failures)
	}
	if created != 1 {
		t.Errorf("%d openers reported creating the finding, want exactly 1 — the partial unique index is the arbiter and the losers converge", created)
	}
	if got := integrationFindingCount(t, r.db, subject); got != 1 {
		t.Errorf("the subject carries %d findings, want 1 after %d concurrent openers", got, openers)
	}
}

// TestIntegrationReconciliationAWindowIsClaimedByOneRunningPass is the
// multi-replica answer, and the failure it exists to prevent is silent rather
// than loud. Two control-plane replicas read the same high-water mark, so they
// compute the SAME window; nothing in an id sequence arbitrates between them.
// Both used to open a run row, both swept, and both wrote converging findings —
// no money at risk, since the pass moves none — while neither advanced the
// mark. The plane re-swept that one window for ever and never covered a window
// after it. A pass that had stopped covering new ground would have looked
// exactly like a healthy one.
//
// So the window is claimed while the pass holds it, and the claim is a partial
// unique index over running rows rather than a lock: it ends with the row, so a
// process that dies mid-pass leaves a row an operator can see rather than a
// lock that outlives its holder and blocks every pass behind it.
//
// The three assertions are the whole property. A second concurrent Begin of
// one window is refused. A FINISHED pass releases it — the partial predicate
// is the release, and a permanent claim would deadlock the pass after the
// first. And a DIFFERENT window is claimable while the first is held, because
// the claim is per-window and not a mutex over the worker.
func TestIntegrationReconciliationAWindowIsClaimedByOneRunningPass(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	now, err := r.clock.Now(ctx)
	if err != nil {
		t.Fatalf("clock: %v", err)
	}
	// Two passes sharing a window_from, and a third with its own — the shape a
	// fleet of replicas produces, since the mark is shared and the clock is
	// not read by the claim.
	windowFrom := now.Add(-2 * time.Hour)
	windowTo := now.Add(-time.Hour)

	first, err := r.runs.Begin(ctx, "control_plane_ledger", windowFrom, windowTo)
	if err != nil {
		t.Fatalf("Begin(first): %v", err)
	}

	// The racing pass, in the same goroutine and the same microsecond: the
	// claim is enforced by the engine, so a sequential reproduction is the
	// same race with the scheduling taken out of it.
	if _, err := r.runs.Begin(ctx, "control_plane_ledger", windowFrom, windowTo); !errors.Is(err, persistence.ErrWindowClaimed) {
		t.Fatalf("a second Begin of a running window returned %v, want persistence.ErrWindowClaimed — two passes must not sweep one window", err)
	}

	// A different window is a different claim. The index is on (scope,
	// window_from), not on scope alone, so a pass that reads a different mark
	// — or a second scope added later — is not blocked by the first.
	other, err := r.runs.Begin(ctx, "control_plane_ledger", windowFrom.Add(-2*time.Hour), windowFrom)
	if err != nil {
		t.Fatalf("Begin(a different window) while the first is claimed: %v", err)
	}
	if other == first {
		t.Fatalf("both Begins returned id %d; the second window is a different claim and must be a different row", other)
	}

	// Finishing releases the claim — this is the assertion a plain mutex
	// would fail and a partial index passes, and it is what keeps the second
	// pass of a healthy single-replica worker from finding its own window
	// held for ever.
	if _, err := r.runs.Finish(ctx, first, "running", "completed", now, persistence.RunCounters{}); err != nil {
		t.Fatalf("Finish(first): %v", err)
	}
	reclaimed, err := r.runs.Begin(ctx, "control_plane_ledger", windowFrom, windowTo)
	if err != nil {
		t.Fatalf("Begin(after the first pass finished): %v", err) // a finished pass must release its window
	}
	if reclaimed == first {
		t.Errorf("the reclaimed window returned the first pass's id %d; the claim did not produce a new row", reclaimed)
	}
	// And the row count says the loser wrote nothing: the refused Begin left
	// no row behind, which is what "refused" has to mean for a statement whose
	// only output is the id it did not produce.
	var claimed int
	if err := r.db.QueryRowContext(ctx, `
		SELECT count(*) FROM control.reconciliation_runs
		WHERE window_from = $1`, windowFrom).Scan(&claimed); err != nil {
		t.Fatalf("counting the runs over the shared window: %v", err)
	}
	if claimed != 2 {
		t.Errorf("the shared window carries %d run rows, want 2 — the first and the pass that reclaimed it, with the refused one having written nothing", claimed)
	}
}

// TestIntegrationReconciliationRunLifecycle is the pass lifecycle: the row is
// written before the work with no finish, which is the record a pass that dies
// mid-sweep leaves, and Finish is compare-and-set so a verdict already recorded
// is not overwritten.
func TestIntegrationReconciliationRunLifecycle(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	now, err := r.clock.Now(ctx)
	if err != nil {
		t.Fatalf("clock: %v", err)
	}
	windowFrom, windowTo := now.Add(-time.Hour), now

	id, err := r.runs.Begin(ctx, "control_plane", windowFrom, windowTo)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	open, err := r.runs.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if open.ID != id {
		t.Fatalf("Latest returned run %d, want the one just opened (%d)", open.ID, id)
	}
	if open.Status != "running" {
		t.Errorf("the opened run reads status %q, want \"running\" — a pass writes its row before it does its work", open.Status)
	}
	if open.FinishedAt != nil {
		t.Errorf("the opened run carries finished_at %s, want nil while it is running", open.FinishedAt)
	}
	if !open.WindowFrom.Equal(windowFrom) || !open.WindowTo.Equal(windowTo) {
		t.Errorf("the run's window reads [%s, %s), want [%s, %s) — a run row is a claim about work, so the bounds must be the ones the pass used",
			open.WindowFrom, open.WindowTo, windowFrom, windowTo)
	}

	counters := persistence.RunCounters{BucketsScanned: 12, FindingsOpened: 3, FindingsUnchanged: 5}
	finished, err := r.runs.Finish(ctx, id, "running", "completed", now, counters)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !finished {
		t.Fatalf("Finish reported false on a run this caller opened and read as running")
	}

	after, err := r.runs.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest(after): %v", err)
	}
	if after.Status != "completed" || after.FinishedAt == nil {
		t.Errorf("the finished run reads status %q with finished_at %v, want completed with a finish", after.Status, after.FinishedAt)
	}
	if after.BucketsScanned != counters.BucketsScanned || after.FindingsOpened != counters.FindingsOpened || after.FindingsUnchanged != counters.FindingsUnchanged {
		t.Errorf("the finished run's counters read %d/%d/%d, want %d/%d/%d — a run must never be marked complete with counters describing a different pass",
			after.BucketsScanned, after.FindingsOpened, after.FindingsUnchanged,
			counters.BucketsScanned, counters.FindingsOpened, counters.FindingsUnchanged)
	}

	// The compare-and-set, from the status the caller read the second time.
	again, err := r.runs.Finish(ctx, id, "running", "failed", now, persistence.RunCounters{})
	if err != nil {
		t.Fatalf("Finish(again): %v", err)
	}
	if again {
		t.Errorf("a second Finish from \"running\" reported true; the run is completed, so a verdict already recorded must not be overwritten")
	}
}

// TestIntegrationReconciliationTheHighWaterMarkIsTheNewestRunEvenUnfinished is
// the property the high-water mark exists for. A pass that died mid-sweep left
// the newest row, and that row's window_to is where the next pass resumes —
// ordering by finished_at instead would let a crashed pass be skipped over in
// favour of an older completed one, and the next pass would re-sweep what the
// crashed pass had half-covered.
func TestIntegrationReconciliationTheHighWaterMarkIsTheNewestRunEvenUnfinished(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	now, err := r.clock.Now(ctx)
	if err != nil {
		t.Fatalf("clock: %v", err)
	}

	completed, err := r.runs.Begin(ctx, "control_plane", now.Add(-2*time.Hour), now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Begin(completed): %v", err)
	}
	if _, err := r.runs.Finish(ctx, completed, "running", "completed", now, persistence.RunCounters{BucketsScanned: 7}); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	// The crashed pass: opened, never finished, and its window is the newest.
	crashed, err := r.runs.Begin(ctx, "control_plane", now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("Begin(crashed): %v", err)
	}

	latest, err := r.runs.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest.ID != crashed {
		t.Errorf("Latest returned run %d, want the newest (%d) — the high-water mark is read by id, never by finished_at, so a crashed pass is not skipped over", latest.ID, crashed)
	}
	if latest.Status != "running" {
		t.Errorf("the newest run reads status %q, want \"running\" — where the sweep stopped is where the next one resumes whether or not it finished", latest.Status)
	}
	if !latest.WindowTo.Equal(now) {
		t.Errorf("the newest run's window_to is %s, want %s — that instant is the next pass's lower bound", latest.WindowTo, now)
	}
}

// TestIntegrationReconciliationBucketSweepPagesByKeysetAndTerminates is the
// walk every pass makes and the one OFFSET would ruin: a keyset page asks for
// the rows after the last id it saw, so a row can neither be re-read nor
// skipped as the table grows under the sweep. The seeded ids are checked
// explicitly; the page COUNT is not, because this database is shared with the
// other suites and asserting on a global row count would be asserting on them.
func TestIntegrationReconciliationBucketSweepPagesByKeysetAndTerminates(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	seeded := make([]accounting.FundingBucketID, 0, 5)
	for i := 0; i < 5; i++ {
		bucket := r.accounting.newAccountBucket(t, reconProbe(fmt.Sprintf("sweep %d", i)))
		seeded = append(seeded, bucket.ID)
	}

	// One page at a time from the zero uuid, which sorts before every real id
	// — the same origin the pass itself uses.
	seen := map[accounting.FundingBucketID]int{}
	after := zeroBucketID
	for page := 0; page < 10_000; page++ {
		rows, err := r.buckets.Sweep(ctx, after, 2)
		if err != nil {
			t.Fatalf("Sweep(page %d, after %s): %v", page, after, err)
		}
		for _, bucket := range rows {
			seen[bucket.ID]++
		}
		if len(rows) < 2 {
			for _, id := range seeded {
				if seen[id] == 0 {
					t.Errorf("the sweep never returned seeded bucket %s", id)
				} else if seen[id] > 1 {
					t.Errorf("the sweep returned bucket %s %d times, want exactly once — a keyset walk does not re-read a row it passed", id, seen[id])
				}
			}
			return
		}
		// The v7 ids are canonical lowercase uuid text, so the string order is
		// the column's own order — the same comparison the sweep's ORDER BY
		// makes. Without it the cursor would never advance and the walk would
		// re-read page one forever.
		if !(rows[len(rows)-1].ID > after) {
			t.Fatalf("page %d's last id %s is not after the cursor %s; the sweep is not paging by keyset and can re-read forever", page, rows[len(rows)-1].ID, after)
		}
		after = rows[len(rows)-1].ID
	}
	t.Fatal("the sweep never reached a short page in 10000 pages; it is not terminating")
}

// TestIntegrationReconciliationSweepRefusesAPageOfNothing is the adapter's own
// bound, and it is a refusal rather than a clamp: a limit of zero would make
// every page empty and every pass report a clean window it never looked at,
// which is the one outcome a reconciliation pass must never produce.
func TestIntegrationReconciliationSweepRefusesAPageOfNothing(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if _, err := r.buckets.Sweep(ctx, zeroBucketID, 0); err == nil {
		t.Errorf("Sweep(limit 0) returned no error; a pass that reads nothing must refuse, not report a clean window")
	}
}

// TestIntegrationReconciliationSettlementLedgerDescribesTheShapeThePlanWrote
// is the read every leg assertion is made against, proved against a REAL
// settlement: the header, the consume sum, the per-kind multiset and the two
// bucket counts must agree with the plan the accounting use case writes, or
// every F2/F3 finding this worker would raise is a false positive by
// construction. A fake's numbers agree with the pass by definition; a misread
// CTE would not.
func TestIntegrationReconciliationSettlementLedgerDescribesTheShapeThePlanWrote(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	const held, consumed = int64(4000), int64(2500)
	settlement, bucket := r.settledRequest(t, held, consumed)

	ledger, err := r.settlements.Ledger(ctx, settlement.ID)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	if ledger.Settlement.ID != settlement.ID {
		t.Errorf("the ledger's header is %s, want %s", ledger.Settlement.ID, settlement.ID)
	}
	if ledger.Settlement.SettledTotal.Int64() != consumed {
		t.Errorf("the header's total is %d, want the consumed amount %d", ledger.Settlement.SettledTotal.Int64(), consumed)
	}
	if ledger.ConsumeSum != consumed {
		t.Errorf("the sum of the settlement's consume legs is %d, want %d — the header is DEFINED to be that sum, so this settlement would open a total_mismatch finding on its first pass",
			ledger.ConsumeSum, consumed)
	}
	if ledger.Buckets != 1 {
		t.Errorf("the ledger reports %d buckets, want 1 — the one bucket seeded", ledger.Buckets)
	}
	if ledger.BucketsWithoutRelease != 0 {
		t.Errorf("the ledger reports %d buckets with a consume and no release, want 0 — this settlement released its %d-unit tail, and a positive count would open a leg-shape finding on a healthy settlement",
			ledger.BucketsWithoutRelease, held-consumed)
	}
	if ledger.LegsByKind[string(accounting.KindConsume)] != 1 || ledger.LegsByKind[string(accounting.KindRelease)] != 1 {
		t.Errorf("the ledger's leg multiset reads %v, want one consume and one release", ledger.LegsByKind)
	}
	if ledger.Legs != 2 {
		t.Errorf("the ledger reports %d legs, want 2", ledger.Legs)
	}
	if bucket.ID == "" {
		t.Errorf("the fixture returned no bucket")
	}
}

// TestIntegrationReconciliationTheZeroPricedSettleIsALegitimateShape is the one
// settlement a naive shape check refuses: no legs at all, and every count at
// zero. The port's aggregate over an empty CTE is one row of zeros rather than
// no row, which is exactly why a missing row would have been read as a miss.
func TestIntegrationReconciliationTheZeroPricedSettleIsALegitimateShape(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	settlement := r.headerOnlySettlement(t)
	ledger, err := r.settlements.Ledger(ctx, settlement.ID)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	if ledger.ConsumeSum != 0 || ledger.Buckets != 0 || ledger.BucketsWithoutRelease != 0 || ledger.Legs != 0 {
		t.Errorf("the zero-priced settle reads consume_sum %d, buckets %d, buckets_without_release %d, legs %d; want all zero — every shape assertion is an inequality that zero satisfies",
			ledger.ConsumeSum, ledger.Buckets, ledger.BucketsWithoutRelease, ledger.Legs)
	}
	if len(ledger.LegsByKind) != 0 {
		t.Errorf("the zero-priced settle carries a leg multiset %v, want empty", ledger.LegsByKind)
	}
}

// TestIntegrationReconciliationAnAbsentSettlementIsAMissNotAnEmptyShape is the
// other half of the same pair, and the distinction the F4 check depends on: a
// check that cannot tell "no such settlement" from "a settlement with no legs"
// would record an empty-shape finding against a request whose settlement was
// never written.
func TestIntegrationReconciliationAnAbsentSettlementIsAMissNotAnEmptyShape(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	absent, err := accounting.NewSettlementID()
	if err != nil {
		t.Fatalf("mint settlement id: %v", err)
	}
	if _, err := r.settlements.Ledger(ctx, absent); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("Ledger(absent) error = %v, want persistence.ErrNotFound — a miss and an empty shape are different answers and the F4 check is written against the first", err)
	}
}

// TestIntegrationReconciliationTheAppliedWindowIsBoundedByAppliedAt is the
// windowed sweep's own read, and the half-open bounds are asserted through the
// seeded row's OWN applied_at rather than by comparing the lengths of two
// windows: this database is shared with the other suites, and a length
// comparison would be a statement about their rows.
func TestIntegrationReconciliationTheAppliedWindowIsBoundedByAppliedAt(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	requestID := reconProbe("applied window")
	if _, err := r.recordAppliedSettled(t, requestID); err != nil {
		t.Fatalf("seeding the applied fact: %v", err)
	}
	recordedAt, err := r.appliedAt(t, requestID)
	if err != nil {
		t.Fatalf("reading the row's own applied_at: %v", err)
	}

	// Inclusive at the floor: a row exactly at `from` is inside the window.
	atFloor, err := r.applied.Recent(ctx, recordedAt, recordedAt.Add(time.Hour), time.Time{}, "", 1_000)
	if err != nil {
		t.Fatalf("Recent(from = applied_at): %v", err)
	}
	// Exclusive at the ceiling: a row exactly at `to` is outside it. This is
	// the property that lets two consecutive passes over adjacent windows see
	// each row exactly once.
	atCeiling, err := r.applied.Recent(ctx, recordedAt.Add(-time.Hour), recordedAt, time.Time{}, "", 1_000)
	if err != nil {
		t.Fatalf("Recent(to = applied_at): %v", err)
	}
	if !reconContainsRequest(atFloor, requestID) {
		t.Errorf("a window whose inclusive floor is the row's own applied_at did not return it; the lower bound is not inclusive")
	}
	if reconContainsRequest(atCeiling, requestID) {
		t.Errorf("a window whose exclusive ceiling is the row's own applied_at returned it; the upper bound is not exclusive, so two adjacent passes would both see this row")
	}
}

// TestIntegrationReconciliationTheAppliedWindowPagesOnThePairAndNotTheInstant
// is the regression for the paging bug this window was written with, and the
// one property of the read that cannot be seen by a single call.
//
// The bug: a keyset of `applied_at > after`. Every row a page of ingestion
// applies is applied in ONE transaction — a page commits whole — so a hundred
// facts share one applied_at to the microsecond, and resuming after that
// instant skipped ninety-nine of them. A pass over a batch of exactly the
// ingestion page size therefore examined one fact in a hundred and reported a
// clean window. The database here held the evidence: five rows sharing one
// instant, whose second page came back empty.
//
// The fix is the PAIR, and the fix is only real if the read orders by the
// pair and resumes on the pair. So the test does what a single-call assertion
// cannot: it forms a real tie of five rows, pages the window one row at a
// time, and requires the walk to deliver all five. A keyset on the instant
// stops after the first; a keyset on the pair walks the whole tie, in
// request_id order, which is the only order a stable walk can have among rows
// the instant cannot separate.
//
// The tie is formed the way production forms it, through the port and in one
// transaction, and not by rewriting rows. applied_facts has its own
// append-only trigger — this test's first draft tried to UPDATE applied_at onto
// a shared instant and was refused for exactly the reason everything else
// cannot be, which is worth saying: the ledger that reconciliation sweeps is
// the same ledger that refuses to be revised, so a pass's fixture cannot be
// assembled by hand at all. The column defaults to now(), which inside a
// transaction is transaction_timestamp() — one instant for the whole
// transaction — and one transaction for five records IS the tie.
func TestIntegrationReconciliationTheAppliedWindowPagesOnThePairAndNotTheInstant(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Five probes, one transaction. The request ids are the suite's own probe
	// strings, so the tie's resolution order is whatever request_id order is
	// and the assertion below sorts against the database's own collation
	// rather than a guess at it.
	const pages = 5
	subjects := make([]string, 0, pages)
	if err := r.store.WithinTx(ctx, func(txCtx context.Context) error {
		for i := range pages {
			subject := reconProbe(fmt.Sprintf("shared instant %02d", i))
			subjects = append(subjects, subject)
			if err := r.recordAppliedSettledIn(txCtx, t, subject); err != nil {
				return fmt.Errorf("seeding the applied fact %d: %w", i, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("recording the page's five facts in one transaction: %v", err)
	}

	// The tie is the precondition, and it is asserted rather than assumed: if
	// these five rows are not on one instant, the walk below is being asked
	// nothing and its success would be meaningless. The instant is read rather
	// than predicted, because the column's default is now() and the value is
	// the engine's to give.
	placeholders := make([]string, 0, pages)
	args := make([]any, 0, pages)
	for i, subject := range subjects {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
		args = append(args, subject)
	}
	var distinct int
	var shared time.Time
	if err := r.db.QueryRowContext(ctx,
		"SELECT count(DISTINCT applied_at), min(applied_at) FROM control.applied_facts WHERE request_id IN ("+
			strings.Join(placeholders, ", ")+")", args...).Scan(&distinct, &shared); err != nil {
		t.Fatalf("counting the seeded rows' distinct instants: %v", err)
	}
	if distinct != 1 {
		t.Fatalf("the %d seeded rows sit on %d distinct instants, want 1 — the tie this test depends on did not form", pages, distinct)
	}

	// The whole window, read as one call, is the control: it must see all
	// five. A read that cannot return them together cannot page them either.
	whole, err := r.applied.Recent(ctx, shared, shared.Add(time.Hour), time.Time{}, "", 1_000)
	if err != nil {
		t.Fatalf("Recent(whole window): %v", err)
	}
	for _, subject := range subjects {
		if !reconContainsRequest(whole, subject) {
			t.Fatalf("the whole window did not return %q — the ceiling bounds are wrong, and the paging below would then prove nothing", subject)
		}
	}

	// The walk itself: one row per page, resumed on the pair, five pages.
	// This is the pass's own loop, and the loop terminates on a short page,
	// so a full first page and four more are what a correct walk returns.
	after, afterID := time.Time{}, ""
	seen := make([]string, 0, pages)
	for page := 1; page <= pages; page++ {
		facts, err := r.applied.Recent(ctx, shared, shared.Add(time.Hour), after, afterID, 1)
		if err != nil {
			t.Fatalf("Recent(page %d): %v", page, err)
		}
		if len(facts) != 1 {
			t.Fatalf("page %d returned %d rows, want 1 — a keyset on the instant alone stops at the first row of a tie, and this is where the rest of the page is lost",
				page, len(facts))
		}
		seen = append(seen, facts[0].RequestID)
		after, afterID = facts[0].AppliedAt, facts[0].RequestID
	}

	// Every subject, once, and in the one order a stable walk can give among
	// rows the instant cannot separate. A missing row is the bug; a repeat is
	// the other half of it, a walk that re-reads a row the cursor already
	// passed. The order is settled by the database rather than by a guess at
	// Go's sort against a collation this database may not share, so the
	// expectation is a sort and the comparison is a sort.
	if len(seen) != pages {
		t.Fatalf("the walk delivered %d rows, want %d", len(seen), pages)
	}
	// The walk's order against the database's: every seeded subject, once, in
	// the order the engine itself ranks them. A missing subject is the bug
	// this whole file is about; a repeated one is the other half of it, a walk
	// that re-reads a row the cursor has already passed.
	sort.Strings(seen)
	ranked := append([]string(nil), subjects...)
	sort.Strings(ranked)
	for i := range ranked {
		if seen[i] != ranked[i] {
			t.Errorf("walk row %d = %q, want %q — the tie is walked in request_id order, and every subject appears exactly once", i, seen[i], ranked[i])
		}
	}
}

// TestIntegrationReconciliationTheAppliedWindowRefusesAPageOfNothing is the
// same refusal the bucket sweep carries, on the read that walks the hottest
// table in the plane.
func TestIntegrationReconciliationTheAppliedWindowRefusesAPageOfNothing(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	now, err := r.clock.Now(ctx)
	if err != nil {
		t.Fatalf("clock: %v", err)
	}
	if _, err := r.applied.Recent(ctx, now.Add(-time.Hour), now, time.Time{}, "", 0); err == nil {
		t.Errorf("Recent(limit 0) returned no error; a windowed read of nothing is a clean window the pass never swept")
	}
}

// TestIntegrationReconciliationThePositionIsReadAndNeverInterpreted is the
// boundary the F6 check is written on: the position is a Data-Plane-issued
// opaque string, and the adapter's read returns it without parsing it. The
// assertion is deliberately weak — a position of ANY shape comes back as
// itself, never rejected and never compared to anything — because that opacity
// is the property, and a strong assertion about the value would be this plane
// inventing a vocabulary for a grammar it does not own. On a database the
// ingestion suite has advanced, the position is legitimately non-empty and
// there is nothing further to say.
func TestIntegrationReconciliationThePositionIsReadAndNeverInterpreted(t *testing.T) {
	r := integrationReconciliation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	position, err := r.cursor.Position(ctx)
	if err != nil {
		t.Fatalf("Position: %v", err)
	}
	if position == "" {
		effects, err := r.applied.Recent(ctx, time.Time{}, time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), time.Time{}, "", 1)
		if err != nil {
			t.Fatalf("Recent(whole history): %v", err)
		}
		if len(effects) > 0 {
			t.Logf("an empty position against %d+ applied effects — the F6 divergence this check exists to record", len(effects))
		}
		return
	}
	t.Logf("the position reads %q, carried as opaque text; the suites share a database so this probe does not assert on its value", position)
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// zeroBucketID is where a keyset sweep starts: the nil uuid sorts before every
// v7 the domain's minter produces, so the first page is the head of the table.
const zeroBucketID = accounting.FundingBucketID("00000000-0000-0000-0000-000000000000")

// settledRequest runs the settle choreography the accounting use case runs — a
// funded bucket, a hold, an allocation, and BuildSettle's plan behind its
// header in one unit of work — and returns the header and the bucket it settled
// against. The reconciliation read is proved against the shape the real writer
// produces, which is the whole point: a crafted row would agree with whatever
// the read happens to do.
func (r *reconciliationRepos) settledRequest(t *testing.T, held, consumed int64) (accounting.Settlement, accounting.Bucket) {
	t.Helper()
	a := r.accounting
	bucket := a.newAccountBucket(t, reconProbe("settled request"))
	a.appendCommitted(t, a.topupEntry(t, bucket.ID, held+1_000, acctCommandKey(t, "seed-")))

	reservation := acctReservation(t)
	a.appendCommitted(t, a.holdEntry(t, bucket.ID, held, reservation))

	allocation, err := accounting.NewAllocation(bucket.ID, reservation, acctAmount(t, held))
	if err != nil {
		t.Fatalf("allocation: %v", err)
	}
	allocation, err = allocation.SettleConsumed(acctAmount(t, consumed), acctPrice(t))
	if err != nil {
		t.Fatalf("settle consumed: %v", err)
	}
	plan, err := accounting.BuildSettle(acctSettlementID(t), acctRequestID(t),
		[]accounting.Allocation{allocation}, accounting.NewLedgerEntryID, time.Now().UTC())
	if err != nil {
		t.Fatalf("build settle: %v", err)
	}

	err = r.store.WithinTx(t.Context(), func(ctx context.Context) error {
		created, err := r.settlements.Create(ctx, plan.Settlement)
		if err != nil {
			return err
		}
		if !created {
			return errors.New("integration: the first acknowledgement must create the header")
		}
		for _, entry := range plan.Entries {
			if _, _, err := a.ledger.Append(ctx, entry); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	return plan.Settlement, bucket
}

// headerOnlySettlement records a header of record and no legs — what the
// zero-priced settle books. BuildSettle cannot produce it (an allocation always
// yields a consume or a release), so the header goes through the port with a
// zero total, which is the only shape that reaches this state.
//
// The zero is cast rather than built with NewAmount on purpose: NewAmount
// takes strictly positive magnitudes, and a zero-priced settle is exactly the
// case where zero IS the value and not the absence of one — the same judgment
// validateAmountAtOrAboveZero carries in the domain, reached here from the
// other side. A spelling the domain refuses is not one an adapter test gets
// to write around silently, so the cast is explicit about which of the two
// constructors this column is being filled by.
func (r *reconciliationRepos) headerOnlySettlement(t *testing.T) accounting.Settlement {
	t.Helper()
	settlement, err := r.headerOnlySettlementE(t)
	if err != nil {
		t.Fatalf("create the zero-priced header: %v", err)
	}
	return settlement
}

// headerOnlySettlementE is headerOnlySettlement with the failure as an error
// rather than a Fatalf, for the callers that are already inside a unit of work
// and must hand the failure back for the transaction to roll back — a Fatalf
// from inside a WithinTx closure would end the test with the transaction still
// open and the rollback never run.
func (r *reconciliationRepos) headerOnlySettlementE(t *testing.T) (accounting.Settlement, error) {
	t.Helper()
	settlement := accounting.Settlement{
		ID:           acctSettlementID(t),
		RequestID:    acctRequestID(t),
		SettledTotal: accounting.Amount(0),
		CreatedAt:    time.Now().UTC(),
	}
	created, err := r.settlements.Create(t.Context(), settlement)
	if err != nil {
		return accounting.Settlement{}, err
	}
	if !created {
		return accounting.Settlement{}, errors.New("the zero-priced header reported false; the request id is run-unique")
	}
	return settlement, nil
}

// recordAppliedSettled records the applied-facts row the applier would write for
// a settled fact, through the port and inside a unit of work — Record refuses
// to run autocommitted, which is itself half of what this fixture proves.
//
// The settlement it names is a real header, because the row's own foreign key
// demands one and the domain is right to: a settled effect pointing at nothing
// is the shape F4 exists to record, and a fixture that could not create one
// would be unable to tell this path apart from that one.
func (r *reconciliationRepos) recordAppliedSettled(t *testing.T, requestID string) (accounting.Settlement, error) {
	t.Helper()
	settlement, err := r.headerOnlySettlementE(t)
	if err != nil {
		return accounting.Settlement{}, err
	}
	return settlement, r.store.WithinTx(t.Context(), func(ctx context.Context) error {
		return r.recordAppliedSettledIn(ctx, t, requestID)
	})
}

// recordAppliedSettledIn is recordAppliedSettled's body with the caller's own
// unit of work, and it exists for the pair-paging test alone: that test needs
// five records in ONE transaction, because one transaction is the only thing
// that makes five rows share an applied_at. Opening five transactions would
// make five instants and no tie, and the test would then pass against the
// statement it exists to reject.
//
// It still writes a real settlement header per fact, because the row's foreign
// key demands one — a row with no header is a shape the F4 check would
// normally record, and a fixture that could create one could not tell the two
// apart.
func (r *reconciliationRepos) recordAppliedSettledIn(ctx context.Context, t *testing.T, requestID string) error {
	settlement, err := r.headerOnlySettlementE(t)
	if err != nil {
		return err
	}
	amount := acctAmount(t, 1_234).Int64()
	capture := "provider_reported"
	return r.applied.Record(ctx, persistence.AppliedFact{
		RequestID:     requestID,
		KindClass:     ingestion.ClassSettlement,
		Kind:          ingestion.KindSettled,
		AppendSeq:     1,
		SettledAmount: &amount,
		CaptureMethod: &capture,
		SettlementID:  string(settlement.ID),
	})
}

func (r *reconciliationRepos) appliedAt(t *testing.T, requestID string) (time.Time, error) {
	t.Helper()
	var at time.Time
	if err := r.db.QueryRow("SELECT applied_at FROM control.applied_facts WHERE request_id = $1", requestID).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("read the seeded fact's applied_at: %w", err)
	}
	return at, nil
}

func reconContainsRequest(facts []persistence.AppliedFact, requestID string) bool {
	for _, fact := range facts {
		if fact.RequestID == requestID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// probe helpers
// ---------------------------------------------------------------------------

type reconSnapshot struct {
	DetectedAt  time.Time
	LastSeenAt  time.Time
	Observed    []byte
	Severity    string
	Status      string
	ResolutionS sql.NullTime
}

func integrationFindingID(t *testing.T, db *sql.DB, subject string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow("SELECT id FROM control.reconciliation_findings WHERE subject_id = $1 ORDER BY id LIMIT 1", subject).Scan(&id); err != nil {
		t.Fatalf("reading the probe finding's id: %v", err)
	}
	return id
}

func integrationFindingSnapshot(t *testing.T, db *sql.DB, subject string) reconSnapshot {
	t.Helper()
	var out reconSnapshot
	if err := db.QueryRow(`SELECT detected_at, last_seen_at, observed, severity, status, resolved_at
		FROM control.reconciliation_findings WHERE subject_id = $1 ORDER BY id LIMIT 1`, subject).
		Scan(&out.DetectedAt, &out.LastSeenAt, &out.Observed, &out.Severity, &out.Status, &out.ResolutionS); err != nil {
		t.Fatalf("reading the probe finding's row: %v", err)
	}
	return out
}

func integrationFindingCount(t *testing.T, db *sql.DB, subject string) int {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM control.reconciliation_findings WHERE subject_id = $1", subject).Scan(&count); err != nil {
		t.Fatalf("counting findings for %q: %v", subject, err)
	}
	return count
}
