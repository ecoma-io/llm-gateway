package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
)

// The ledger adapter's unit-of-work rule and its savepoint bracket, pinned
// against the fake driver. What the adapter does ABOVE the statements —
// refusing an autocommitted append, closing the bracket it opened when a
// statement refuses — is orchestration these tests can observe; what the
// statements themselves do is PostgreSQL's, and belongs to the suite in
// deploy/postgres.

func TestALedgerAppendRefusesToRunOutsideAUnitOfWork(t *testing.T) {
	f, db := registerFake(t)
	ledger := NewFundingLedger(New(db))

	entry := grantLeg(t)
	_, _, err := ledger.Append(context.Background(), entry)
	if !errors.Is(err, errAppendOutsideUnitOfWork) {
		t.Fatalf("Append outside a unit of work = %v, want the refusal sentinel", err)
	}
	if got := f.recorded(); len(got) != 0 {
		t.Fatalf("the refusal must precede every driver call, got events %v", got)
	}
}

func TestALedgerAppendBracketsAFailedEchoInItsSavepoint(t *testing.T) {
	f, db := registerFake(t)
	store := New(db)
	ledger := NewFundingLedger(store)

	// The fake driver refuses statements (its conn has no query path), so the
	// echo inside the savepoint fails. What this proves is the bracket: the
	// adapter opened a savepoint, rolled back to it rather than leaving the
	// unit of work aborted, and let the failure surface — the caller's
	// transaction dies by its own verdict, not under the adapter's leg.
	err := store.WithinTx(context.Background(), func(ctx context.Context) error {
		_, _, appendErr := ledger.Append(ctx, grantLeg(t))
		return appendErr
	})
	if err == nil {
		t.Fatal("Append against a driver that refuses statements must surface the failure")
	}
	events := f.recorded()
	if got := of(events, evExec); len(got) != 2 {
		t.Fatalf("savepoint bracket = %d exec events, want exactly 2 (open, roll back to): %v", len(got), events)
	}
	if got := of(events, evRollback); len(got) != 1 {
		t.Fatalf("the failed unit of work must roll back exactly once: %v", events)
	}
}

func grantLeg(t *testing.T) accounting.LedgerEntry {
	t.Helper()
	bucketID, err := accounting.NewFundingBucketID()
	if err != nil {
		t.Fatalf("mint bucket id: %v", err)
	}
	entryID, err := accounting.NewLedgerEntryID()
	if err != nil {
		t.Fatalf("mint entry id: %v", err)
	}
	granted, err := accounting.NewAmount(100)
	if err != nil {
		t.Fatalf("amount: %v", err)
	}
	entry, err := accounting.NewGrantEntry(entryID, bucketID, granted,
		time.Date(2026, time.September, 25, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("grant entry: %v", err)
	}
	return entry
}
