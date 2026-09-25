package application

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The accounting fakes: one world, in the same spirit as the commerce and
// replay worlds, because the property under test is once more a relationship
// — legs and bucket move inside one unit of work, a settlement header never
// lands without its legs, and a redelivery converges on what is already on
// file. The ledger fake is not a stub: it runs the same guard, the same
// uniqueness mappings and the same sequence stamp the PostgreSQL statement
// pair runs, over the world's state, which is what lets the tests watch the
// converge-or-refuse bargain end to end.
type accountingWorld struct {
	order []string

	buckets             map[accounting.FundingBucketID]accounting.Bucket
	bucketByEntitlement map[accounting.EntitlementID]accounting.FundingBucketID
	bucketByAccount     map[accounting.AccountID]accounting.FundingBucketID
	legs                []accounting.LedgerEntry
	settlements         map[accounting.RequestID]accounting.Settlement
	paygRows            map[commerce.AccountID]commerce.AccountPayg

	now time.Time

	// failOn refuses one operation by key ("ledger.append", "settlements.create").
	failOn map[string]error

	// racingLeg models the other writer that lands the winning leg between a
	// caller's pre-read and its append: the first append that names the same
	// bucket and key lands the racer's leg instead of the caller's and answers
	// the duplicate sentinel — the exact shape of the port's savepoint story.
	racingLeg  *accounting.LedgerEntry
	raceLanded bool

	// appendFailOnCall makes that absolute n-th append fail with a
	// persistence error — the mechanical way to break a settlement mid-plan
	// and watch the header die with it.
	appendFailOnCall int
	appendCalls      int

	// closeLoses refuses that many compare-and-swaps first — legs landing
	// under the closer's feet — without moving the row, the mechanical way
	// to walk the close loop's retry and exhaustion paths.
	closeLoses int

	outsideTx int
}

func newAccountingWorld(t *testing.T) *accountingWorld {
	t.Helper()
	return &accountingWorld{
		now:                 time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		buckets:             map[accounting.FundingBucketID]accounting.Bucket{},
		bucketByEntitlement: map[accounting.EntitlementID]accounting.FundingBucketID{},
		bucketByAccount:     map[accounting.AccountID]accounting.FundingBucketID{},
		settlements:         map[accounting.RequestID]accounting.Settlement{},
		paygRows:            map[commerce.AccountID]commerce.AccountPayg{},
		failOn:              map[string]error{},
	}
}

// newAccounting wires the use cases over one world, the way every accounting
// test builds them: all seven ports answer from the same state.
func newAccounting(w *accountingWorld) *Accounting {
	return NewAccounting(
		fakeAccountingStore{world: w},
		fakeFundingBuckets{world: w},
		fakeFundingLedger{world: w},
		fakeAccountingSettlements{world: w},
		fakeFundingProjections{world: w},
		fakeAccountingPayg{world: w},
		fakeAccountingClock{world: w},
	)
}

// seedAccountFunding opens a PAYG bucket through the use case itself and
// returns it — the same choreography production takes.
func (w *accountingWorld) seedAccountFunding(t *testing.T, accountID commerce.AccountID) accounting.Bucket {
	t.Helper()
	use := newAccounting(w)
	bucket, err := use.OpenAccountFunding(t.Context(), accountID)
	if err != nil {
		t.Fatalf("seed account funding: %v", err)
	}
	return bucket
}

// seedEntitlementFunding funds an entitlement cycle through the use case
// itself, inside the unit of work the roll would provide.
func (w *accountingWorld) seedEntitlementFunding(t *testing.T, entitlementID commerce.EntitlementID, granted int64) accounting.Bucket {
	t.Helper()
	use := newAccounting(w)
	if err := use.store.WithinTx(t.Context(), func(ctx context.Context) error {
		return use.FundEntitlement(ctx, entitlementID, granted)
	}); err != nil {
		t.Fatalf("seed entitlement funding: %v", err)
	}
	bucket, err := use.BucketByEntitlement(t.Context(), accounting.EntitlementID(entitlementID))
	if err != nil {
		t.Fatalf("seed entitlement funding: read bucket: %v", err)
	}
	return bucket
}

// derived recomputes a bucket's balances from its legs alone — the fake of
// what the projections port reads out of the database, and what Reconcile is
// judged against.
func (w *accountingWorld) derived(bucketID accounting.FundingBucketID) accounting.Derivation {
	var d accounting.Derivation
	for _, leg := range w.legs {
		if leg.FundingBucketID != bucketID {
			continue
		}
		d.Settled += accounting.Balance(leg.SettledDelta)
		d.Held += accounting.Balance(leg.HeldDelta)
		d.Legs++
	}
	d.Available = d.Settled - d.Held
	return d
}

func (w *accountingWorld) record(entry string) {
	w.order = append(w.order, entry)
}

// fakeAccountingStore is the unit of work: it snapshots the durable state at
// begin and restores it on error, so a leg that fails mid-plan demonstrably
// takes its settlement header down with it.
type fakeAccountingStore struct {
	persistence.Store
	world *accountingWorld
}

func (s fakeAccountingStore) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	w := s.world
	w.record("begin")

	buckets := copyMap(w.buckets)
	byEntitlement := copyMap(w.bucketByEntitlement)
	byAccount := copyMap(w.bucketByAccount)
	legs := append([]accounting.LedgerEntry(nil), w.legs...)
	settlements := copyMap(w.settlements)
	paygRows := copyMap(w.paygRows)
	raceBefore := w.raceLanded

	if err := fn(context.WithValue(ctx, txMarkerKey{}, true)); err != nil {
		w.buckets, w.bucketByEntitlement, w.bucketByAccount = buckets, byEntitlement, byAccount
		w.legs, w.settlements, w.paygRows = legs, settlements, paygRows
		// The racing leg models another transaction's commit, which this
		// unit of work's rollback cannot reach: re-land it on the restored
		// state, exactly as the winner's committed echo would stand.
		if w.raceLanded && !raceBefore && w.racingLeg != nil {
			_, _, _ = w.land(*w.racingLeg)
		}
		w.record("rollback")
		return err
	}
	w.record("commit")
	return nil
}

func (s fakeAccountingStore) InUnitOfWork(ctx context.Context) bool {
	return inTransaction(ctx)
}

type fakeFundingBuckets struct {
	persistence.FundingBuckets
	world *accountingWorld
}

func (f fakeFundingBuckets) Create(_ context.Context, bucket accounting.Bucket) error {
	if err := f.world.failOn["buckets.create"]; err != nil {
		return err
	}
	f.world.buckets[bucket.ID] = bucket
	if bucket.EntitlementID != "" {
		f.world.bucketByEntitlement[bucket.EntitlementID] = bucket.ID
	}
	if bucket.AccountID != "" {
		f.world.bucketByAccount[bucket.AccountID] = bucket.ID
	}
	return nil
}

func (f fakeFundingBuckets) ByID(_ context.Context, id accounting.FundingBucketID) (accounting.Bucket, error) {
	bucket, ok := f.world.buckets[id]
	if !ok {
		return accounting.Bucket{}, persistence.ErrNotFound
	}
	return bucket, nil
}

func (f fakeFundingBuckets) ByEntitlementID(_ context.Context, id accounting.EntitlementID) (accounting.Bucket, error) {
	bucketID, ok := f.world.bucketByEntitlement[id]
	if !ok {
		return accounting.Bucket{}, persistence.ErrNotFound
	}
	return f.world.buckets[bucketID], nil
}

func (f fakeFundingBuckets) ByAccountID(_ context.Context, id accounting.AccountID) (accounting.Bucket, error) {
	bucketID, ok := f.world.bucketByAccount[id]
	if !ok {
		return accounting.Bucket{}, persistence.ErrNotFound
	}
	return f.world.buckets[bucketID], nil
}

// Close is the CAS: the predicate — active, the caller's version, nothing
// held — is the statement's WHERE clause, and the bool is its verdict. A
// world primed with closeLoses loses that many swaps first, the row untouched,
// exactly as a leg landing between the read and the write presents.
func (f fakeFundingBuckets) Close(_ context.Context, id accounting.FundingBucketID, fromVersion int64, updatedAt time.Time) (bool, error) {
	bucket, ok := f.world.buckets[id]
	if !ok || bucket.Status != accounting.BucketActive || bucket.Version != fromVersion || bucket.Held != 0 {
		return false, nil
	}
	if f.world.closeLoses > 0 {
		f.world.closeLoses--
		return false, nil
	}
	bucket.Status = accounting.BucketClosed
	bucket.UpdatedAt = updatedAt
	f.world.buckets[id] = bucket
	return true, nil
}

type fakeFundingLedger struct {
	persistence.FundingLedger
	world *accountingWorld
}

// Append runs the statement pair's contract over the world: the guard is
// ApplyTo — the same algebra the echo's WHERE clause states — the uniqueness
// constraints answer the duplicate sentinels the adapter maps them to, and
// the sequence is allocated, never guessed.
func (f fakeFundingLedger) Append(ctx context.Context, entry accounting.LedgerEntry) (accounting.LedgerEntry, accounting.Bucket, error) {
	w := f.world
	if !inTransaction(ctx) {
		w.outsideTx++
	}
	w.record("append:" + string(entry.Kind))
	if err := w.failOn["ledger.append"]; err != nil {
		return accounting.LedgerEntry{}, accounting.Bucket{}, err
	}
	w.appendCalls++
	if w.appendFailOnCall > 0 && w.appendCalls == w.appendFailOnCall {
		return accounting.LedgerEntry{}, accounting.Bucket{}, persistence.ErrConflict
	}

	bucket, ok := w.buckets[entry.FundingBucketID]
	if !ok {
		return accounting.LedgerEntry{}, accounting.Bucket{}, persistence.ErrNotFound
	}
	if bucket.Status != accounting.BucketActive {
		return accounting.LedgerEntry{}, accounting.Bucket{}, accounting.ErrBucketClosed
	}

	// The other writer won the race the caller's pre-read missed: its leg
	// lands, the caller's is refused with the constraint's sentinel, and the
	// unit of work stays alive for the re-read — the savepoint's whole story.
	if w.racingLeg != nil && !w.raceLanded &&
		w.racingLeg.FundingBucketID == entry.FundingBucketID && sameLedgerKey(*w.racingLeg, entry) {
		w.raceLanded = true
		if _, _, err := w.land(*w.racingLeg); err != nil {
			return accounting.LedgerEntry{}, accounting.Bucket{}, err
		}
		if entry.CommandKey != "" {
			return accounting.LedgerEntry{}, accounting.Bucket{}, accounting.ErrDuplicateCommand
		}
		return accounting.LedgerEntry{}, accounting.Bucket{}, accounting.ErrDuplicateMovement
	}

	for _, leg := range w.legs {
		if leg.FundingBucketID != entry.FundingBucketID {
			continue
		}
		if entry.CommandKey != "" && leg.CommandKey == entry.CommandKey {
			return accounting.LedgerEntry{}, accounting.Bucket{}, accounting.ErrDuplicateCommand
		}
		if entry.ReservationID != "" && leg.Kind == entry.Kind && leg.ReservationID == entry.ReservationID {
			return accounting.LedgerEntry{}, accounting.Bucket{}, accounting.ErrDuplicateMovement
		}
		if entry.SettlementID != "" && leg.Kind == entry.Kind &&
			leg.SettlementID == entry.SettlementID && leg.FundingBucketID == entry.FundingBucketID {
			return accounting.LedgerEntry{}, accounting.Bucket{}, accounting.ErrDuplicateMovement
		}
	}

	stamped, after, err := w.land(entry)
	if err != nil {
		return accounting.LedgerEntry{}, accounting.Bucket{}, err
	}
	return stamped, after, nil
}

// land is the insert plus the echo: ApplyTo is the guarded UPDATE over the
// world's row, the sequence is allocated from the row's counter, and both
// facts land together. The provenance conditions the real echo's WHERE
// clause carries are repeated here against the world's legs — a release
// names a hold on file, a correction cites its own bucket's leg — so the
// fake refuses what the statement pair refuses.
func (w *accountingWorld) land(entry accounting.LedgerEntry) (accounting.LedgerEntry, accounting.Bucket, error) {
	bucket, ok := w.buckets[entry.FundingBucketID]
	if !ok {
		return accounting.LedgerEntry{}, accounting.Bucket{}, persistence.ErrNotFound
	}
	if entry.Kind == accounting.KindRelease && !w.holdOnFile(entry.FundingBucketID, entry.ReservationID) {
		return accounting.LedgerEntry{}, accounting.Bucket{}, fmt.Errorf("fakes: release names reservation %s on bucket %s, which has no hold leg on file here: %w",
			entry.ReservationID, entry.FundingBucketID, accounting.ErrInvalidReference)
	}
	if entry.Kind == accounting.KindAdjustment && !w.correctsOwnBucket(entry.OriginalEntryID, entry.FundingBucketID) {
		return accounting.LedgerEntry{}, accounting.Bucket{}, fmt.Errorf("fakes: adjustment on bucket %s cites entry %s, which is not this bucket's: %w",
			entry.FundingBucketID, entry.OriginalEntryID, accounting.ErrInvalidReference)
	}
	after, err := entry.ApplyTo(bucket)
	if err != nil {
		return accounting.LedgerEntry{}, accounting.Bucket{}, err
	}
	stamped := entry
	stamped.Sequence = bucket.LastSequence + 1
	after.LastSequence = stamped.Sequence
	after.Version = bucket.Version + 1
	after.UpdatedAt = w.now
	w.legs = append(w.legs, stamped)
	w.buckets[after.ID] = after
	return stamped, after, nil
}

// holdOnFile answers the release echo's provenance condition over the
// world's legs: did this bucket book the named reservation's hold?
func (w *accountingWorld) holdOnFile(bucketID accounting.FundingBucketID, reservationID accounting.ReservationID) bool {
	for _, leg := range w.legs {
		if leg.FundingBucketID == bucketID && leg.ReservationID == reservationID && leg.Kind == accounting.KindHold {
			return true
		}
	}
	return false
}

// correctsOwnBucket answers the adjustment echo's provenance condition: is
// the cited entry this bucket's own leg?
func (w *accountingWorld) correctsOwnBucket(entryID accounting.LedgerEntryID, bucketID accounting.FundingBucketID) bool {
	for _, leg := range w.legs {
		if leg.ID == entryID && leg.FundingBucketID == bucketID {
			return true
		}
	}
	return false
}

// sameLedgerKey states the two uniqueness constraints the ledger enforces per
// bucket: one leg per command key, one leg per (reservation, kind).
func sameLedgerKey(a, b accounting.LedgerEntry) bool {
	if a.CommandKey != "" && b.CommandKey != "" {
		return a.CommandKey == b.CommandKey
	}
	if a.ReservationID != "" && b.ReservationID != "" {
		return a.Kind == b.Kind && a.ReservationID == b.ReservationID
	}
	if a.SettlementID != "" && b.SettlementID != "" {
		return a.Kind == b.Kind && a.SettlementID == b.SettlementID
	}
	return false
}

func (f fakeFundingLedger) ByBucketAndCommandKey(_ context.Context, bucketID accounting.FundingBucketID, commandKey accounting.CommandKey) (accounting.LedgerEntry, error) {
	for _, leg := range f.world.legs {
		if leg.FundingBucketID == bucketID && leg.CommandKey == commandKey {
			return leg, nil
		}
	}
	return accounting.LedgerEntry{}, persistence.ErrNotFound
}

func (f fakeFundingLedger) ByBucketReservationAndKind(_ context.Context, bucketID accounting.FundingBucketID, reservationID accounting.ReservationID, kind accounting.Kind) (accounting.LedgerEntry, error) {
	for _, leg := range f.world.legs {
		if leg.FundingBucketID == bucketID && leg.ReservationID == reservationID && leg.Kind == kind {
			return leg, nil
		}
	}
	return accounting.LedgerEntry{}, persistence.ErrNotFound
}

type fakeAccountingSettlements struct {
	persistence.Settlements
	world *accountingWorld
}

// Create is ON CONFLICT (request_id) DO NOTHING: the bool says whether this
// call's header is the one that landed.
func (f fakeAccountingSettlements) Create(_ context.Context, settlement accounting.Settlement) (bool, error) {
	if err := f.world.failOn["settlements.create"]; err != nil {
		return false, err
	}
	if _, ok := f.world.settlements[settlement.RequestID]; ok {
		return false, nil
	}
	f.world.settlements[settlement.RequestID] = settlement
	return true, nil
}

func (f fakeAccountingSettlements) ByRequestID(_ context.Context, requestID accounting.RequestID) (accounting.Settlement, error) {
	settlement, ok := f.world.settlements[requestID]
	if !ok {
		return accounting.Settlement{}, persistence.ErrNotFound
	}
	return settlement, nil
}

type fakeFundingProjections struct {
	persistence.FundingProjections
	world *accountingWorld
}

func (f fakeFundingProjections) DerivedBalances(_ context.Context, bucketID accounting.FundingBucketID) (accounting.Derivation, error) {
	if _, ok := f.world.buckets[bucketID]; !ok {
		return accounting.Derivation{}, persistence.ErrNotFound
	}
	return f.world.derived(bucketID), nil
}

type fakeAccountingClock struct {
	persistence.Clock
	world *accountingWorld
}

func (f fakeAccountingClock) Now(_ context.Context) (time.Time, error) {
	if err := f.world.failOn["clock.now"]; err != nil {
		return time.Time{}, err
	}
	return f.world.now, nil
}

type fakeAccountingPayg struct {
	persistence.PaygAccounts
	world *accountingWorld
}

func (f fakeAccountingPayg) ByAccount(_ context.Context, accountID commerce.AccountID) (commerce.AccountPayg, error) {
	payg, ok := f.world.paygRows[accountID]
	if !ok {
		return commerce.AccountPayg{}, persistence.ErrNotFound
	}
	return payg, nil
}

// AssignFundingBucket is the port's write-once verdict, faithful to the
// statement's WHERE clause: a row that already carries any reference — its
// own bucket re-offered included — matches nothing and answers false; only a
// rowless row or an empty reference takes the bucket and answers true.
func (f fakeAccountingPayg) AssignFundingBucket(_ context.Context, accountID commerce.AccountID, bucketID commerce.FundingBucketID, updatedAt time.Time) (bool, error) {
	payg, ok := f.world.paygRows[accountID]
	if ok && payg.FundingBucketID != "" {
		return false, nil
	}
	if !ok {
		payg = commerce.AccountPayg{AccountID: accountID, CreatedAt: updatedAt}
	}
	payg.FundingBucketID = bucketID
	payg.UpdatedAt = updatedAt
	f.world.paygRows[accountID] = payg
	return true, nil
}
