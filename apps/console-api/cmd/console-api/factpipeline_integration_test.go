//go:build integration

package main

// The composition root's integration tier: the whole fact pipeline wired the
// way main() wires it — the real outbound use cases and adapters over the
// real `control` database — driven by a feed that stands in for dataplane-api
// the way the outbound decoder's own seam tests stand in for it. Everything
// else in the module tests one link; this file is the chain.
//
// What only the whole chain can answer: that a replay pass opens one real
// unit of work whose commit carries the settlement, its legs, the applied
// ledger row and the cursor advance together; that a fact the accounting
// layer refuses stops the page with nothing of the earlier facts' work left
// behind (the whole-page law against the real engine, not a fake's
// imitation of it); that a quarantinable refusal is recorded and the page
// still advances; and that the second delivery of a page the first one
// committed books nothing twice.
//
// Conventions are the storage suites': no t.Parallel, run-unique ids minted
// by the domain's v7 minters, fixtures reached through the ports, nothing
// deleted. Run it the way the storage suites are run — the fixture and the
// variables are the same ones; see internal/adapters/outbound/postgres's
// integration header.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/outbound/postgres"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/ingestion"
	dataplaneport "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// pipelinePool dials the plane database through the adapter's Open — the same
// startup call main() makes — and hands back the pool and the store everything
// else resolves from. The admin-DSN dance lives in the postgres suite; this
// file names the plane database the way that suite's derived DSN does.
func pipelinePool(t *testing.T) (*sql.DB, persistence.Store) {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_ADMIN_DSN")
	if dsn == "" {
		t.Fatal("POSTGRES_TEST_ADMIN_DSN is required for integration tests; start deploy/postgres first (from the repository root: docker compose -f deploy/postgres/compose.yaml up -d --wait) and set it to postgres://gateway:gateway-dev-only@127.0.0.1:55441/postgres?sslmode=disable")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("POSTGRES_TEST_ADMIN_DSN: %v", err)
	}
	parsed.Path = "/control"

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	db, err := postgres.Open(ctx, postgres.Options{
		DSN:             parsed.String(),
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: time.Minute,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, postgres.New(db)
}

// pipelineWorld is the process in miniature: the ports and use cases wired
// exactly as main() wires them, over one pool, with the feed a canned
// implementation of the seam's UsageFacts. feedAfters records the positions
// the consumer asked for, the observable the cursor's secrecy leaves a test —
// the Data Plane never learns the position, so the test reads the read, not
// the write.
type pipelineWorld struct {
	store      persistence.Store
	accounting *application.Accounting
	applied    persistence.AppliedFacts
	feedAfters []string
	ingestion  *application.FactIngestion
	page       dataplaneport.Page
}

func newPipelineWorld(t *testing.T) *pipelineWorld {
	t.Helper()
	_, store := pipelinePool(t)

	accounting := application.NewAccounting(store,
		postgres.NewFundingBuckets(store),
		postgres.NewFundingLedger(store),
		postgres.NewSettlements(store),
		postgres.NewFundingProjections(store),
		postgres.NewPaygAccounts(store),
		postgres.NewClock(store),
	)
	applier := application.NewFactApplier(accounting,
		postgres.NewAppliedFacts(store),
		postgres.NewQuarantinedFacts(store),
	)

	world := &pipelineWorld{
		store:      store,
		accounting: accounting,
		applied:    postgres.NewAppliedFacts(store),
	}
	world.ingestion = application.NewFactIngestion(world, store, postgres.NewIngestionCursor(store), applier)
	return world
}

// ReadUsageEvents implements the seam's UsageFacts with the world's canned
// page, recording every position it is asked for.
func (w *pipelineWorld) ReadUsageEvents(_ context.Context, after string, _ int) (dataplaneport.Page, error) {
	w.feedAfters = append(w.feedAfters, after)
	return w.page, nil
}

// fundedBucket opens a PAYG bucket through the ports and grants it, in one
// committed unit of work — the fixture path: identity account, bucket, grant
// leg. The balances it leaves are the assertion baseline everything below
// reads.
func (w *pipelineWorld) fundedBucket(t *testing.T, raw int64) accounting.Bucket {
	t.Helper()
	ctx := t.Context()

	accountID, err := identity.NewAccountID()
	if err != nil {
		t.Fatalf("NewAccountID: %v", err)
	}
	account, err := identity.NewAccount(accountID, "it-pipeline-"+string(accountID), time.Now().UTC())
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	if err := postgres.NewAccounts(w.store).Create(ctx, *account); err != nil {
		t.Fatalf("create account: %v", err)
	}

	bucketID, err := accounting.NewFundingBucketID()
	if err != nil {
		t.Fatalf("NewFundingBucketID: %v", err)
	}
	bucket, err := accounting.NewAccountBucket(bucketID, accounting.AccountID(account.ID), time.Now().UTC())
	if err != nil {
		t.Fatalf("NewAccountBucket: %v", err)
	}
	if err := postgres.NewFundingBuckets(w.store).Create(ctx, bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	topupID, err := accounting.NewLedgerEntryID()
	if err != nil {
		t.Fatalf("NewLedgerEntryID: %v", err)
	}
	// A PAYG bucket is funded by a topup, not a grant — the grant is the
	// cycle bucket's funding leg, and the ledger's transition guard says so.
	// The command key is the topup's idempotency key; the minted v7 makes it
	// run-unique like everything else here.
	topup, err := accounting.NewTopupEntry(topupID, bucket.ID, acctAmount(t, raw),
		accounting.CommandKey("topup-"+string(topupID)), time.Now().UTC())
	if err != nil {
		t.Fatalf("NewTopupEntry: %v", err)
	}
	if err := w.store.WithinTx(ctx, func(txCtx context.Context) error {
		_, _, err := postgres.NewFundingLedger(w.store).Append(txCtx, topup)
		return err
	}); err != nil {
		t.Fatalf("fund the bucket: %v", err)
	}
	return bucket
}

// settledFact is the coherent settled fact for requestID over bucket: one
// leg naming the bucket for the whole settled amount, the figures that
// re-derive it (1M input at 1500, 2M output at 6000, one ceiling over the
// summed raw product — 13_500), and the provenance columns the pairing rules
// demand.
func settledFact(t *testing.T, requestID string, bucket accounting.FundingBucketID) dataplaneport.Event {
	t.Helper()
	const amount = int64(13_500)
	capture := "reported"
	attempt := "0c000000-0000-7000-8000-000000000000"
	revision := "rev-pipeline"
	inputPrice, outputPrice := int64(1500), int64(6000)
	inputTokens, outputTokens := int64(1_000_000), int64(2_000_000)
	delivery := int64(4096)
	settledAmount := amount

	payload, err := json.Marshal(struct {
		Allocations []struct {
			FundingBucketID string `json:"funding_bucket_id"`
			Amount          int64  `json:"amount"`
			Ordinal         int    `json:"ordinal"`
		} `json:"allocations"`
	}{Allocations: []struct {
		FundingBucketID string `json:"funding_bucket_id"`
		Amount          int64  `json:"amount"`
		Ordinal         int    `json:"ordinal"`
	}{{
		FundingBucketID: string(bucket),
		Amount:          amount,
		Ordinal:         1,
	}}})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	return dataplaneport.Event{
		AppendSeq:            7,
		RequestID:            requestID,
		Kind:                 ingestion.KindSettled,
		SchemaVersion:        ingestion.SchemaVersion,
		OccurredAt:           time.Now().UTC(),
		Payload:              payload,
		CaptureMethod:        &capture,
		CommittedAttemptID:   &attempt,
		ProviderInputTokens:  &inputTokens,
		ProviderOutputTokens: &outputTokens,
		DeliveryTokens:       &delivery,
		PriceRevision:        &revision,
		InputUnitPrice:       &inputPrice,
		OutputUnitPrice:      &outputPrice,
		SettledAmount:        &settledAmount,
	}
}

// pipelineRequestID mints the fact's request id: canonical-lowercase-uuid is
// the grammar Interpret pins (the reservation identity), and the domain's v7
// minter produces one.
func pipelineRequestID(t *testing.T) string {
	t.Helper()
	id, err := accounting.NewSettlementID()
	if err != nil {
		t.Fatalf("mint request id: %v", err)
	}
	return string(id)
}

// acctAmount builds the fixture amounts the grant and the balance reads use.
func acctAmount(t *testing.T, raw int64) accounting.Amount {
	t.Helper()
	amount, err := accounting.NewAmount(raw)
	if err != nil {
		t.Fatalf("amount %d: %v", raw, err)
	}
	return amount
}

// readBalances is the projection read the storage suites assert through: the
// bucket's cached balances after whatever the pass booked.
func (w *pipelineWorld) readBalances(t *testing.T, id accounting.FundingBucketID) (settled, held, available int64) {
	t.Helper()
	bucket, err := postgres.NewFundingBuckets(w.store).ByID(t.Context(), id)
	if err != nil {
		t.Fatalf("read bucket %s: %v", id, err)
	}
	return int64(bucket.Settled), int64(bucket.Held), int64(bucket.Available)
}

// quarantinedReason reads the quarantine's reason column for one fact
// directly — the port's own read face is deliberately Find-shaped, and the
// row's value here is that it exists and says why.
func (w *pipelineWorld) quarantinedReason(t *testing.T, requestID string, appendSeq int64) (string, bool) {
	t.Helper()
	var reason string
	err := w.store.Querier(t.Context()).QueryRowContext(t.Context(),
		`SELECT reason FROM control.quarantined_facts WHERE request_id = $1 AND append_seq = $2`,
		requestID, appendSeq).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read the quarantine: %v", err)
	}
	return reason, true
}

// TestAReplayPassSettlesAppliesAndAdvancesAsOneUnit drives the whole
// pipeline through the happy path and then through the same page again. The
// first pass must leave settlement, ledger legs, applied row and position in
// one committed state; the second must re-read the same range, book nothing,
// and leave every number where the first pass left it — replay is a read
// before it is ever a second write, on the real engine and not a fake of it.
func TestAReplayPassSettlesAppliesAndAdvancesAsOneUnit(t *testing.T) {
	world := newPipelineWorld(t)
	ctx := t.Context()

	bucket := world.fundedBucket(t, 50_000)
	requestID := pipelineRequestID(t)
	world.page = dataplaneport.Page{
		Events:     []dataplaneport.Event{settledFact(t, requestID, bucket.ID)},
		NextCursor: "cursor-pipeline-2",
		HasMore:    false,
	}

	result, err := world.ingestion.Replay(ctx)
	if err != nil {
		t.Fatalf("Replay() error = %v, want nil", err)
	}
	if result.Applied != 1 || result.HasMore {
		t.Fatalf("Replay() = %+v, want one applied fact and no more", result)
	}

	// The settlement of record, with its lineage row and the position that
	// claims it: the three durable halves of one derivation.
	settlement, err := postgres.NewSettlements(world.store).ByRequestID(ctx, accounting.RequestID(requestID))
	if err != nil {
		t.Fatalf("ByRequestID() error = %v, want the settled fact's settlement", err)
	}
	if settlement.SettledTotal != acctAmount(t, 13_500) {
		t.Errorf("settled total = %d, want 13_500 — one ceiling over the fact's own figures", settlement.SettledTotal)
	}
	applied, err := world.applied.Find(ctx, requestID, ingestion.ClassSettlement)
	if err != nil || applied == nil {
		t.Fatalf("Find() = (%v, %v), want the applied row", applied, err)
	}
	if applied.SettlementID != string(settlement.ID) {
		t.Errorf("applied row names settlement %q, want the settlement of record %q", applied.SettlementID, string(settlement.ID))
	}
	if applied.SettledAmount == nil || *applied.SettledAmount != 13_500 {
		t.Errorf("applied row settled amount = %v, want 13_500", applied.SettledAmount)
	}
	if position, err := postgres.NewIngestionCursor(world.store).Position(ctx); err != nil || position != "cursor-pipeline-2" {
		t.Fatalf("Position() = (%q, %v), want the page's own next_cursor — the pass committed its claim", position, err)
	}

	// The bucket's projection: the full hold consumed — the spent 13_500 is
	// gone from both settled and available, and nothing is left held.
	if settled, held, available := world.readBalances(t, bucket.ID); settled != 36_500 || held != 0 || available != 36_500 {
		t.Errorf("balances after the settle = (settled %d, held %d, available %d), want (36_500, 0, 36_500)", settled, held, available)
	}

	// The redelivery: the same page arrives again, as a restored cursor or a
	// concurrent worker would deliver it. The pass applies the fact again —
	// deliveries are not effects — and books nothing: one settlement still,
	// one applied row, balances unmoved.
	world.page.NextCursor = "cursor-pipeline-3"
	result, err = world.ingestion.Replay(ctx)
	if err != nil {
		t.Fatalf("replayed Replay() error = %v, want nil", err)
	}
	if result.Applied != 1 {
		t.Fatalf("replayed Replay() applied = %d, want the delivery counted", result.Applied)
	}
	replayed, err := postgres.NewSettlements(world.store).ByRequestID(ctx, accounting.RequestID(requestID))
	if err != nil {
		t.Fatalf("ByRequestID() after the replay: %v", err)
	}
	if replayed.ID != settlement.ID || replayed.SettledTotal != settlement.SettledTotal {
		t.Errorf("replayed settlement = (%s, %d), want the first pass's (%s, %d) unchanged",
			replayed.ID, replayed.SettledTotal, settlement.ID, settlement.SettledTotal)
	}
	if settled, held, available := world.readBalances(t, bucket.ID); settled != 36_500 || held != 0 || available != 36_500 {
		t.Errorf("balances after the replay = (settled %d, held %d, available %d), want the first pass's — a replay moves no money", settled, held, available)
	}

	// The two reads bracket the advance: the first pass asked from wherever
	// the singleton stood before it — the fixture is shared, so the seed is
	// whatever earlier passes left — and the second asked from the position
	// the first pass stored. The read, not the write, is the observable the
	// cursor's secrecy leaves.
	if len(world.feedAfters) != 2 {
		t.Fatalf("the feed was asked %d times, want two reads: %v", len(world.feedAfters), world.feedAfters)
	}
	if world.feedAfters[1] != "cursor-pipeline-2" {
		t.Errorf("the second read asked from %q, want the position the first pass stored", world.feedAfters[1])
	}
}

// TestAPageThatFailsHalfwayLeavesNothingBehind is the whole-page law on the
// real engine: the page's first fact books a full settlement, its second
// names a bucket the ledger does not know, and the pass must come back as an
// error with nothing committed — not the settlement, not its applied row,
// not the position. The next pass re-reads both facts; what the first one
// derived is free to derive again, and what the second one refused is still
// there for a human.
func TestAPageThatFailsHalfwayLeavesNothingBehind(t *testing.T) {
	world := newPipelineWorld(t)
	ctx := t.Context()

	// The cursor is a shared singleton on the fixture, so the baseline is
	// read, not assumed: the law under test is that a failed page leaves the
	// position where this pass found it.
	before, err := postgres.NewIngestionCursor(world.store).Position(ctx)
	if err != nil {
		t.Fatalf("Position() before the pass: %v", err)
	}

	bucket := world.fundedBucket(t, 50_000)
	good := pipelineRequestID(t)
	bad := pipelineRequestID(t)
	world.page = dataplaneport.Page{
		Events: []dataplaneport.Event{
			settledFact(t, good, bucket.ID),
			settledFact(t, bad, "00000000-0000-7000-8000-0000000000ff"), // never created
		},
		NextCursor: "cursor-pipeline-2",
	}

	result, err := world.ingestion.Replay(ctx)
	if err == nil {
		t.Fatalf("Replay() = %+v with a fact the ledger cannot apply, want the page stopped", result)
	}
	if result.Applied != 0 {
		t.Errorf("Replay() applied = %d on a failed page, want zero — deliveries a rollback undid are not deliveries", result.Applied)
	}

	if _, err := postgres.NewSettlements(world.store).ByRequestID(ctx, accounting.RequestID(good)); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("the page's first fact = %v, want no settlement of its own — its work rolled back with the page", err)
	}
	if applied, err := world.applied.Find(ctx, good, ingestion.ClassSettlement); err != nil || applied != nil {
		t.Errorf("Find(good) = (%v, %v), want nil — an effect that did not commit recorded no applied row", applied, err)
	}
	if applied, err := world.applied.Find(ctx, bad, ingestion.ClassSettlement); err != nil || applied != nil {
		t.Errorf("Find(bad) = (%v, %v), want nil — a refusal the accounting layer owns is not quarantined away", applied, err)
	}
	if reason, quarantined := world.quarantinedReason(t, bad, 7); quarantined {
		t.Errorf("the refused fact was quarantined with %q; an accounting refusal is a state of the plane, not the fact's disposition", reason)
	}
	if position, err := postgres.NewIngestionCursor(world.store).Position(ctx); err != nil || position != before {
		t.Fatalf("Position() = (%q, %v), want %q — nothing moved", position, err, before)
	}
	if settled, held, available := world.readBalances(t, bucket.ID); settled != 50_000 || held != 0 || available != 50_000 {
		t.Errorf("balances after the failed page = (settled %d, held %d, available %d), want the topup's own — the hold legs rolled back too", settled, held, available)
	}

	// The retry the loop would make: the same page, the same refusal, the
	// same held position. The stop is a state, not a skip.
	if _, err := world.ingestion.Replay(ctx); err == nil {
		t.Fatal("the retried pass applied a page it must refuse forever until the plane changes")
	}
}

// TestAQuarantinableRefusalIsRecordedAndThePageAdvances drives the other
// disposition through the real store: a kind this build does not implement
// is recorded verbatim — with its reason — inside the pass's unit of work,
// the page advances past it, and the next fact on the page still applies.
func TestAQuarantinableRefusalIsRecordedAndThePageAdvances(t *testing.T) {
	world := newPipelineWorld(t)
	ctx := t.Context()

	bucket := world.fundedBucket(t, 50_000)
	refused := pipelineRequestID(t)
	applied := pipelineRequestID(t)

	unimplemented := settledFact(t, refused, bucket.ID)
	unimplemented.Kind = "refunded"
	world.page = dataplaneport.Page{
		Events: []dataplaneport.Event{
			unimplemented,
			settledFact(t, applied, bucket.ID),
		},
		NextCursor: "cursor-pipeline-2",
	}

	result, err := world.ingestion.Replay(ctx)
	if err != nil {
		t.Fatalf("Replay() error = %v, want nil — the refusal is the record, not an error", err)
	}
	if result.Applied != 2 {
		t.Fatalf("Replay() applied = %d, want both deliveries", result.Applied)
	}

	reason, quarantined := world.quarantinedReason(t, refused, 7)
	if !quarantined {
		t.Fatal("the refused fact recorded no quarantine row; a skipped fact with no record is a silent loss")
	}
	if !strings.Contains(reason, "unknown fact kind") {
		t.Errorf("quarantine reason = %q, want the interpretation's own words", reason)
	}
	if appliedRow, err := world.applied.Find(ctx, applied, ingestion.ClassSettlement); err != nil || appliedRow == nil {
		t.Fatalf("Find(the next fact) = (%v, %v), want it applied — the page advanced past the refusal, not through it", appliedRow, err)
	}
	if position, err := postgres.NewIngestionCursor(world.store).Position(ctx); err != nil || position != "cursor-pipeline-2" {
		t.Fatalf("Position() = (%q, %v), want the advance the recorded refusal made true", position, err)
	}
}
