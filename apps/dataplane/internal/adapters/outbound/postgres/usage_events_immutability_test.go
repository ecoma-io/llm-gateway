//go:build integration

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
)

// TestIntegrationEngineRefusesAUsageEventRewrite proves the usage_events
// append-only guard at the engine: 000006 arms the BEFORE UPDATE OR DELETE
// trigger the persistence convention promises ("`ledger_entries` and
// `usage_events` have no `UPDATE` or `DELETE` path", ADR 0004 invariants 1–2),
// and this file is where the guard is seen firing. The insert that succeeds
// here is the domain's own settlement unit of work — the one write the table
// is for — and every refusal is driven as raw SQL, the way a writer that never
// heard of the port's discipline would arrive.
//
// Its own test file, not a case beside the terminal-row triggers in
// runtime_storage_test.go, because the subject is a different contract: those
// triggers let open rows move and freeze only the terminal ones, while a fact
// is born final — the reservation_allocations shape — and the refusal message
// this suite pins is the rewrite one, not the terminal one.
func TestIntegrationEngineRefusesAUsageEventRewrite(t *testing.T) {
	db, store := integrationPool(t)
	integrationRuntimeSchema(t, db)
	repos := integrationRepos(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The fact under test, written through the unit of work production uses.
	// Its allocation leg gives the payload real content, so the
	// unchanged-after-refusal assertions below mean something: a rewrite that
	// landed would be visible in what the reader is served.
	bucket := integrationRuntimeAccount(t, "fact-immutable-bucket")
	legs := []accounting.AllocationLeg{{FundingBucketID: bucket, Amount: 375, Ordinal: 1}}
	requestID, _, seq := integrationAppendSettled(t, ctx, repos,
		integrationRuntimeAccount(t, "fact-immutable"), legs, time.Now().UTC())

	// The trigger's reach, statement by statement. The rewritten payload is
	// even a deliverable one — '{"allocations": []}' passes the envelope
	// trigger's own shape check — which pins where the refusal comes from:
	// no UPDATE survives to be judged, whatever it carries.
	refusals := []struct {
		name      string
		statement string
		args      []any
	}{
		{
			name:      "a settled amount rewritten in place is a second settlement wearing the first's identity",
			statement: `UPDATE public.usage_events SET settled_amount = 1 WHERE append_seq = $1`,
			args:      []any{seq},
		},
		{
			name:      "a payload rewritten in place rewrites the only allocation tail the ledger may derive",
			statement: `UPDATE public.usage_events SET payload = '{"allocations": []}'::jsonb WHERE append_seq = $1`,
			args:      []any{seq},
		},
		{
			name:      "a kind rewritten in place turns a settled fact into one that never charged anyone",
			statement: `UPDATE public.usage_events SET kind = 'released' WHERE append_seq = $1`,
			args:      []any{seq},
		},
		{
			name:      "a deleted fact is a settlement the feed can never deliver",
			statement: `DELETE FROM public.usage_events WHERE append_seq = $1`,
			args:      []any{seq},
		},
	}
	for _, tt := range refusals {
		_, err := db.ExecContext(ctx, tt.statement, tt.args...)
		if err == nil {
			t.Errorf("%s: the statement succeeded, want the immutability trigger's refusal (%s)", tt.name, tt.statement)
			continue
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
			t.Errorf("%s: error = %v, want the trigger's raise_exception (P0001)", tt.name, err)
			continue
		}
		if !strings.Contains(pgErr.Message, "cannot be rewritten") {
			t.Errorf("%s: trigger message = %q, want the rewrite refusal named", tt.name, pgErr.Message)
		}
	}

	// The refusals were refusals, not damage: the fact is exactly as the
	// settlement wrote it — amount, kind, and the allocation tail naming the
	// bucket the settlement's waterfall recorded.
	var kind string
	var settledAmount int64
	if err := db.QueryRowContext(ctx, `SELECT kind, settled_amount FROM public.usage_events WHERE append_seq = $1`, seq).Scan(&kind, &settledAmount); err != nil {
		t.Fatalf("reading back the refused fact: %v", err)
	}
	if kind != "settled" || settledAmount != 375 {
		t.Errorf("fact after the refused rewrites = (%s, %d), want (settled, 375) — a refusal leaves the row as the settlement wrote it", kind, settledAmount)
	}
	if stored := string(integrationStoredPayload(t, db, requestID)); !strings.Contains(stored, bucket) {
		t.Errorf("payload after the refused rewrites = %s, want the settlement's own leg for %s still on file", stored, bucket)
	}

	// The guard is append-only, not a frozen table: a second settlement's
	// fact still lands whole after every refusal above, at a sequence the
	// stream keeps counting.
	_, _, secondSeq := integrationAppendSettled(t, ctx, repos,
		integrationRuntimeAccount(t, "fact-immutable-2"), nil, time.Now().UTC())
	if secondSeq <= seq {
		t.Fatalf("the second append allocated seq %d, want one past %d — appending must be untouched by the guard", secondSeq, seq)
	}

	// And the refusal is row-level, not a singular-statement accident: a
	// swept DELETE over both of this test's facts is refused whole, and both
	// facts are still there to be counted afterwards.
	_, err := db.ExecContext(ctx, `DELETE FROM public.usage_events WHERE append_seq IN ($1, $2)`, seq, secondSeq)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" || !strings.Contains(pgErr.Message, "cannot be rewritten") {
		t.Errorf("the swept DELETE of two facts error = %v, want the immutability trigger's exception", err)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM public.usage_events WHERE append_seq IN ($1, $2)`, seq, secondSeq).Scan(&remaining); err != nil {
		t.Fatalf("counting the facts the swept DELETE failed to take: %v", err)
	}
	if remaining != 2 {
		t.Errorf("facts surviving the swept DELETE = %d, want 2 — a refused statement takes none of its rows", remaining)
	}
}

// TestIntegrationUsageEventsTruncateIsOutsideTheTriggerStates the guard's
// boundary out loud, so a later reader of the migration does not mistake the
// trigger for a whole-table fence. A BEFORE UPDATE OR DELETE row trigger fires
// for the two statements the persistence convention refuses and for nothing
// else, so TRUNCATE still empties the table — the same reach
// reservation_allocations has under the leg trigger this one mirrors, and the
// same hole session_replication_role = replica opens beside it.
//
// What answers that hole is the privilege model, not the trigger: the
// production contract the deploy/postgres/README states gives an application
// role SELECT, INSERT, UPDATE and DELETE on its plane's tables and no DDL —
// TRUNCATE is a privilege the migration-owning role holds and the application
// role does not, and the fixture's single `gateway` role holds both because a
// local development database is not a privilege model.
//
// It runs on a throwaway database because what it proves is a WHOLE-TABLE
// verdict: the TRUNCATE empties public.usage_events entirely — every fact on
// file, not a selection — and the empty-table count that pins the boundary is
// meaningful only where the table holds this test's rows alone. On the shared
// fixture the table carries every earlier run's rows forever, and emptying it
// would destroy state other tests and later runs own, so the suite's
// throwaway-database rule is exactly what this test lives under: it mints a
// database, applies the lane, fills the table with three facts of its own,
// and measures the emptiness there.
func TestIntegrationUsageEventsTruncateIsOutsideTheTrigger(t *testing.T) {
	integrationThrowawaySerialise(t)
	db := integrationThrowawayDatabase(t, "dataplane_b7_truncate_probe")
	integrationRuntimeSchema(t, db)
	repos := integrationRepos(t, New(db))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	account := integrationRuntimeAccount(t, "fact-truncate")
	seqs := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		_, _, seq := integrationAppendSettled(t, ctx, repos, account, nil, time.Now().UTC())
		seqs = append(seqs, seq)
	}
	if _, err := db.ExecContext(ctx, "TRUNCATE public.usage_events"); err != nil {
		t.Fatalf("TRUNCATE public.usage_events error = %v, want nil — the guard's reach is UPDATE and DELETE, and this test says so where a reader of the trigger will look", err)
	}

	// And what remains is the whole table empty — not this test's three rows
	// less three, but zero facts on file. That totality is the boundary being
	// stated: a TRUNCATE is not a row-scoped statement the trigger could
	// narrow, and on a database of its own the test can afford to prove it.
	var remaining int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM public.usage_events").Scan(&remaining); err != nil {
		t.Fatalf("counting the facts after the TRUNCATE: %v", err)
	}
	if remaining != 0 {
		t.Errorf("facts after the TRUNCATE = %d, want 0 — the statement empties the table, and the boundary this test states is a real one", remaining)
	}

	// The stream is untouched by it, which is why a lost feed is emptied and
	// never re-numbered: the next append continues the sequence, and the
	// epoch stays the one the first append minted.
	epoch, _, _ := integrationStreamRow(t, db)
	if epoch == "" {
		t.Fatal("the stream lost its identity to a TRUNCATE of the facts — the epoch is minted by the first append, not by a migration")
	}
	_, _, nextSeq := integrationAppendSettled(t, ctx, repos, account, nil, time.Now().UTC())
	if nextSeq <= seqs[len(seqs)-1] {
		t.Errorf("the append after the TRUNCATE allocated seq %d, want one past %d — the stream is a separate table and keeps counting", nextSeq, seqs[len(seqs)-1])
	}
	_, lastSeq, _ := integrationStreamRow(t, db)
	if lastSeq != nextSeq {
		t.Errorf("stream last_seq = %d, want %d — the tail moved with the append", lastSeq, nextSeq)
	}
}
