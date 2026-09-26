//go:build integration

package postgres

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The fact consumer's effect repositories against the real `control`
// database. The unit tests pin the applier's decisions and the fake-driver
// tests pin which handle a statement runs on, so what only PostgreSQL can
// answer lives here: that the applied row round-trips whole with its
// settlement foreign key, that the (request_id, kind_class) primary key is
// the exactly-once arbiter it exists to be, that both writes refuse to run
// autocommitted, and that a quarantined fact is recorded verbatim — bounded
// reason, converged duplicate — by the schema the refusal depends on.
//
// What is deliberately NOT re-proven here is the full applier-to-ledger
// path: the accounting suite pins Hold, ReleaseHold and Settle against this
// same engine, the application suite pins the ordering between them, and
// the composition root's seam test pins the whole replay. This file is the
// two new tables' own tier.
//
// Conventions are the accounting suite's: no t.Parallel, run-unique ids
// minted by acctRequestID and acctSettlementID (nothing deletes, so a
// literal id from a previous run would collide with its own row), fixtures
// reached through the ports, nothing deleted.

func integrationIngestion(t *testing.T) (persistence.Store, persistence.AppliedFacts, persistence.QuarantinedFacts, persistence.Settlements) {
	t.Helper()
	_, store := integrationPool(t)
	return store, NewAppliedFacts(store), NewQuarantinedFacts(store), NewSettlements(store)
}

func TestAppliedFactsRecordAndFindRoundTrip(t *testing.T) {
	store, applied, _, settlements := integrationIngestion(t)
	ctx := t.Context()

	requestID := string(acctRequestID(t))
	settledAmount := int64(700)
	capture := "reported"
	settlementID := acctSettlementID(t)

	err := store.WithinTx(ctx, func(txCtx context.Context) error {
		// The settlement the applied row points at, reached through the
		// settlements port — the foreign key is real, so the fixture is.
		created, err := settlements.Create(txCtx, accounting.Settlement{
			ID:           settlementID,
			RequestID:    accounting.RequestID(requestID),
			SettledTotal: acctAmount(t, 700),
			CreatedAt:    time.Now().UTC(),
		})
		if err != nil {
			return err
		}
		if !created {
			t.Fatal("the settlement fixture was not created")
		}
		return applied.Record(txCtx, persistence.AppliedFact{
			RequestID:     requestID,
			KindClass:     "settlement",
			Kind:          "settled",
			AppendSeq:     41,
			SettledAmount: &settledAmount,
			CaptureMethod: &capture,
			SettlementID:  string(settlementID),
		})
	})
	if err != nil {
		t.Fatalf("record inside the unit of work: %v", err)
	}

	row, err := applied.Find(ctx, requestID, "settlement")
	if err != nil {
		t.Fatalf("Find() error = %v, want the recorded row", err)
	}
	if row == nil {
		t.Fatal("Find() = nil, want the recorded row")
	}
	if row.Kind != "settled" || row.AppendSeq != 41 {
		t.Errorf("row identity = (%s, %d), want (settled, 41)", row.Kind, row.AppendSeq)
	}
	if row.SettledAmount == nil || *row.SettledAmount != 700 {
		t.Errorf("row settled amount = %v, want 700", row.SettledAmount)
	}
	if row.CaptureMethod == nil || *row.CaptureMethod != "reported" {
		t.Errorf("row capture = %v, want reported", row.CaptureMethod)
	}
	if row.SettlementID != string(settlementID) {
		t.Errorf("row settlement = %q, want %q", row.SettlementID, string(settlementID))
	}

	// A different class is a different row and finds nothing: the pair is
	// the key, and a settlement on file must not shadow an orphan.
	absent, err := applied.Find(ctx, requestID, "unbillable_orphaned")
	if err != nil || absent != nil {
		t.Fatalf("Find(orphan class) = (%v, %v), want nil, nil", absent, err)
	}
}

func TestAppliedFactsRecordAReleasedShapeWithNoSettledColumns(t *testing.T) {
	store, applied, _, _ := integrationIngestion(t)
	ctx := t.Context()

	requestID := string(acctRequestID(t))
	err := store.WithinTx(ctx, func(txCtx context.Context) error {
		return applied.Record(txCtx, persistence.AppliedFact{
			RequestID: requestID,
			KindClass: "settlement",
			Kind:      "released",
			AppendSeq: 42,
		})
	})
	if err != nil {
		t.Fatalf("record the released shape: %v", err)
	}
	row, err := applied.Find(ctx, requestID, "settlement")
	if err != nil || row == nil {
		t.Fatalf("Find() = (%v, %v), want the released row", row, err)
	}
	if row.SettledAmount != nil || row.CaptureMethod != nil || row.SettlementID != "" {
		t.Errorf("released row = %+v, want the shape-free row the schema pins", row)
	}
}

func TestAppliedFactsRecordRefusesToRunAutocommitted(t *testing.T) {
	_, applied, _, _ := integrationIngestion(t)
	err := applied.Record(t.Context(), persistence.AppliedFact{
		RequestID: string(acctRequestID(t)),
		KindClass: "settlement",
		Kind:      "expired",
		AppendSeq: 43,
	})
	if err == nil || !strings.Contains(err.Error(), "unit of work") {
		t.Fatalf("Record() outside a unit of work = %v, want the refusal", err)
	}
}

func TestTheAppliedLedgerPrimaryKeyIsTheExactlyOnceArbiter(t *testing.T) {
	// Two deliveries of the same class, different kinds, different seqs —
	// the second worker's Find runs after the first committed and sees the
	// row, so the second Record never happens. Only the first writer's row
	// stands: the class books one effect, whichever kind arrived first.
	store, applied, _, _ := integrationIngestion(t)
	ctx := t.Context()

	requestID := string(acctRequestID(t))
	first := persistence.AppliedFact{RequestID: requestID, KindClass: "settlement", Kind: "released", AppendSeq: 44}
	second := persistence.AppliedFact{RequestID: requestID, KindClass: "settlement", Kind: "expired", AppendSeq: 45}

	derive := func(row persistence.AppliedFact) error {
		return store.WithinTx(ctx, func(txCtx context.Context) error {
			existing, err := applied.Find(txCtx, row.RequestID, row.KindClass)
			if err != nil {
				return err
			}
			if existing != nil {
				return nil
			}
			return applied.Record(txCtx, row)
		})
	}

	if err := derive(first); err != nil {
		t.Fatalf("the first worker's page: %v", err)
	}
	if err := derive(second); err != nil {
		t.Fatalf("the second worker's page: %v", err)
	}
	row, err := applied.Find(ctx, requestID, "settlement")
	if err != nil || row == nil {
		t.Fatalf("Find() = (%v, %v), want the first worker's row", row, err)
	}
	if row.Kind != "released" || row.AppendSeq != 44 {
		t.Errorf("surviving row = (%s, %d), want the first writer's (released, 44)", row.Kind, row.AppendSeq)
	}
}

func TestTheAppliedLedgerPrimaryKeyRefusesTheTrueRace(t *testing.T) {
	// The race Find cannot close — both reads before either write — ends at
	// the constraint: exactly one insert commits, the other names the
	// violation, and the store's transaction discipline leaves the loser's
	// unit of work to roll back whole. The rows are both released-shaped so
	// the only thing that can decide between them is the primary key.
	store, applied, _, _ := integrationIngestion(t)
	ctx := t.Context()

	requestID := string(acctRequestID(t))
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, seq := range []int64{46, 47} {
		wg.Add(1)
		go func(slot int, seq int64) {
			defer wg.Done()
			<-release
			results[slot] = store.WithinTx(ctx, func(txCtx context.Context) error {
				return applied.Record(txCtx, persistence.AppliedFact{
					RequestID: requestID,
					KindClass: "settlement",
					Kind:      "released",
					AppendSeq: seq,
				})
			})
		}(i, seq)
	}
	close(release)
	wg.Wait()

	failures, successes := 0, 0
	for _, err := range results {
		if err != nil {
			failures++
		} else {
			successes++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("two racing records = %d commits, %d refusals, want exactly one of each: %v", successes, failures, results)
	}
}

func TestQuarantinedFactsRecordVerbatimAndConverge(t *testing.T) {
	store, _, quarantined, _ := integrationIngestion(t)
	ctx := t.Context()

	requestID := string(acctRequestID(t))
	occurred := time.Now().UTC().Truncate(time.Microsecond)
	price := "rev-very-long-revision-identifier"
	amount := int64(12345)
	inTokens := int64(9)
	payload := `{"allocations":[{"funding_bucket_id":"b","amount":5,"ordinal":1}]}`
	reason := strings.Repeat("r", 600) + " — the tail this record exists to keep"

	err := store.WithinTx(ctx, func(txCtx context.Context) error {
		return quarantined.Record(txCtx, persistence.QuarantinedFact{
			RequestID:           requestID,
			AppendSeq:           48,
			Kind:                "refunded",
			SchemaVersion:       2,
			OccurredAt:          occurred,
			Payload:             []byte(payload),
			PriceRevision:       &price,
			ProviderInputTokens: &inTokens,
			SettledAmount:       &amount,
			Reason:              reason,
		})
	})
	if err != nil {
		t.Fatalf("record the quarantine: %v", err)
	}

	// The read-back is raw SQL on purpose: the record's value is that the
	// columns match the feed, and the port under test has no read path to
	// round-trip through.
	var kind, storedPayload, storedReason string
	var schemaVersion int
	var storedPrice *string
	if err := store.Querier(ctx).QueryRowContext(ctx, `
		SELECT kind, payload, reason, schema_version, price_revision_id
		FROM control.quarantined_facts WHERE request_id = $1 AND append_seq = 48
	`, requestID).Scan(&kind, &storedPayload, &storedReason, &schemaVersion, &storedPrice); err != nil {
		t.Fatalf("read the quarantine back: %v", err)
	}
	if kind != "refunded" || schemaVersion != 2 || storedPayload != payload {
		t.Errorf("verbatim columns = (%s, v%d, %.60q), want the fact whole", kind, schemaVersion, storedPayload)
	}
	if storedPrice == nil || *storedPrice != price {
		t.Errorf("price revision = %v, want %q", storedPrice, price)
	}
	if storedReason != strings.Repeat("r", 512) {
		t.Errorf("reason = %q, want the first 512 of the original — the tail is the one thing the bound may give up", storedReason)
	}

	// The same fact refused again — a restored cursor replaying the range —
	// converges: the original record stands, and a second row never
	// appears.
	err = store.WithinTx(ctx, func(txCtx context.Context) error {
		return quarantined.Record(txCtx, persistence.QuarantinedFact{
			RequestID:     requestID,
			AppendSeq:     48,
			Kind:          "refunded",
			SchemaVersion: 2,
			OccurredAt:    occurred,
			Payload:       []byte(payload),
			Reason:        "a different sentence about the same fact",
		})
	})
	if err != nil {
		t.Fatalf("the converged re-record = %v, want nil", err)
	}
	var rows int
	if err := store.Querier(ctx).QueryRowContext(ctx, `
		SELECT count(*) FROM control.quarantined_facts WHERE request_id = $1
	`, requestID).Scan(&rows); err != nil {
		t.Fatalf("count the quarantine: %v", err)
	}
	if rows != 1 {
		t.Fatalf("quarantine rows for the fact = %d, want the one", rows)
	}
}

func TestQuarantinedFactsRecordRefusesToRunAutocommitted(t *testing.T) {
	_, _, quarantined, _ := integrationIngestion(t)
	err := quarantined.Record(t.Context(), persistence.QuarantinedFact{
		RequestID:     string(acctRequestID(t)),
		AppendSeq:     49,
		Kind:          "refunded",
		SchemaVersion: 2,
		OccurredAt:    time.Now().UTC(),
		Payload:       []byte("{}"),
		Reason:        "refused outside a unit of work",
	})
	if err == nil || !strings.Contains(err.Error(), "unit of work") {
		t.Fatalf("Record() outside a unit of work = %v, want the refusal", err)
	}
}

func TestQuarantinedFactsRecordRefusesAPayloadTheSchemaCannotTake(t *testing.T) {
	// 32768 octets is the column's whole grammar — the same cap the domain
	// pins as MaxPayloadOctets, which is what stops an oversize payload one
	// layer up. This probe is the schema's own word beneath it.
	store, _, quarantined, _ := integrationIngestion(t)
	ctx := t.Context()
	err := store.WithinTx(ctx, func(txCtx context.Context) error {
		return quarantined.Record(txCtx, persistence.QuarantinedFact{
			RequestID:     string(acctRequestID(t)),
			AppendSeq:     50,
			Kind:          "settled",
			SchemaVersion: 1,
			OccurredAt:    time.Now().UTC(),
			Payload:       []byte(strings.Repeat("x", 32769)),
			Reason:        "over the column's cap",
		})
	})
	if err == nil {
		t.Fatal("a 32769-octet payload was recorded, want the CHECK's refusal")
	}
}
