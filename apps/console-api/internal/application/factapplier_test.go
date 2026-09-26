package application

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/ingestion"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// This file pins the real applier's behaviour against the contract's two
// laws: a derived fact lands exactly one effect per (request_id, kind class),
// and a refusal lands either in the quarantine (nil returned, page advances)
// or nowhere (error returned, page stops). The accounting primitives are
// faked at the holdSettler seam — their own suites pin their algebra, and
// the postgres integration suite pins the whole path against the real
// engine; what a fake belongs to here is the applier's own decisions, which
// are ordering and disposition, not arithmetic.

const (
	bucketA          = "10000000-0000-7000-8000-00000000000a"
	bucketB          = "10000000-0000-7000-8000-00000000000b"
	applierRequestID = "d9000000-0000-7000-8000-0000000000d1"
)

// fakeHoldSettler records the movements it is asked for, in order, and
// fails on demand. A converged settle reports the re-acknowledgement path —
// the settlement already on file, nothing moved again.
type fakeHoldSettler struct {
	moves       []string
	failOn      string
	converge    bool
	settlements map[accounting.RequestID]accounting.Settlement
}

func (f *fakeHoldSettler) Hold(_ context.Context, bucketID accounting.FundingBucketID, reservationID accounting.ReservationID, amount int64) (accounting.Bucket, error) {
	if f.failOn == "hold:"+string(bucketID) {
		return accounting.Bucket{}, errors.New("bucket refused the hold")
	}
	f.moves = append(f.moves, "hold:"+string(bucketID)+":"+string(reservationID)+":"+i64(amount))
	return accounting.Bucket{}, nil
}

func (f *fakeHoldSettler) ReleaseHold(_ context.Context, bucketID accounting.FundingBucketID, reservationID accounting.ReservationID, amount int64) (accounting.Bucket, error) {
	f.moves = append(f.moves, "release:"+string(bucketID)+":"+string(reservationID)+":"+i64(amount))
	return accounting.Bucket{}, nil
}

func (f *fakeHoldSettler) Settle(_ context.Context, requestID accounting.RequestID, allocations []accounting.Allocation) (SettlementResult, error) {
	if f.failOn == "settle" {
		return SettlementResult{}, accounting.ErrSettlementConflict
	}
	var total int64
	for _, a := range allocations {
		f.moves = append(f.moves, "settle:"+string(a.BucketID)+":"+i64(a.HeldBooked.Int64())+":"+i64(a.Consumed.Int64()))
		total += a.Consumed.Int64()
	}
	settlement, ok := f.settlements[requestID]
	if !ok {
		settlement = accounting.Settlement{ID: "e1000000-0000-7000-8000-0000000000e1", RequestID: requestID, SettledTotal: accounting.Amount(total)}
		f.settlements[requestID] = settlement
	}
	return SettlementResult{Settlement: settlement, Converged: f.converge}, nil
}

func i64(v int64) string {
	return strconv.FormatInt(v, 10)
}

// fakeAppliedLedger is the idempotency ledger: Find reads the map, Record
// writes it, and the primary key refuses a second row for a pair.
type fakeAppliedLedger struct {
	rows       map[factKey]*persistence.AppliedFact
	failFind   bool
	failRecord bool
}

func (f *fakeAppliedLedger) Find(_ context.Context, requestID, kindClass string) (*persistence.AppliedFact, error) {
	if f.failFind {
		return nil, errors.New("ledger read failed")
	}
	return f.rows[factKey{requestID, kindClass}], nil
}

func (f *fakeAppliedLedger) Record(_ context.Context, applied persistence.AppliedFact) error {
	if f.failRecord {
		return errors.New("ledger write failed")
	}
	key := factKey{applied.RequestID, applied.KindClass}
	if _, taken := f.rows[key]; taken {
		return errors.New("duplicate applied fact")
	}
	f.rows[key] = &applied
	return nil
}

// fakeQuarantine records what it is given.
type fakeQuarantine struct {
	rows []persistence.QuarantinedFact
	fail bool
}

func (f *fakeQuarantine) Record(_ context.Context, quarantined persistence.QuarantinedFact) error {
	if f.fail {
		return errors.New("quarantine write failed")
	}
	f.rows = append(f.rows, quarantined)
	return nil
}

// applierWorld is the fixture: the applier over fakes, with the settled
// fact whose derivation the assertions below lean on — tail (450 on A,
// 400 on B), settled amount 700, boundary partial on B.
type applierWorld struct {
	t          *testing.T
	settler    *fakeHoldSettler
	ledger     *fakeAppliedLedger
	quarantine *fakeQuarantine
	applier    *FactApplier
}

func newApplierWorld(t *testing.T) *applierWorld {
	t.Helper()
	world := &applierWorld{
		t:          t,
		settler:    &fakeHoldSettler{settlements: map[accounting.RequestID]accounting.Settlement{}},
		ledger:     &fakeAppliedLedger{rows: map[factKey]*persistence.AppliedFact{}},
		quarantine: &fakeQuarantine{},
	}
	world.applier = NewFactApplier(world.settler, world.ledger, world.quarantine)
	return world
}

// settledFact is the coherent settled fact, verbatim on the wire: two legs,
// one ceiling over 700_000_000 raw, amount 700, boundary partial on the
// second leg.
func settledFact(t *testing.T) persistence.Fact {
	t.Helper()
	amount := int64(700)
	attempt := "0c000000-0000-7000-8000-00000000000c"
	capture := "reported"
	revision := "rev-1"
	inPrice := int64(200)
	outPrice := int64(800)
	inTokens := int64(1_500_000)
	outTokens := int64(500_000)
	delivery := int64(4096)
	return persistence.Fact{
		AppendSeq:            7,
		RequestID:            applierRequestID,
		Kind:                 ingestion.KindSettled,
		SchemaVersion:        ingestion.SchemaVersion,
		Payload:              envelopeFor(t, legFor{FundingBucketID: bucketA, Amount: 450, Ordinal: 1}, legFor{FundingBucketID: bucketB, Amount: 400, Ordinal: 2}),
		CaptureMethod:        &capture,
		CommittedAttemptID:   &attempt,
		ProviderInputTokens:  &inTokens,
		ProviderOutputTokens: &outTokens,
		DeliveryTokens:       &delivery,
		PriceRevision:        &revision,
		InputUnitPrice:       &inPrice,
		OutputUnitPrice:      &outPrice,
		SettledAmount:        &amount,
	}
}

type legFor struct {
	FundingBucketID string `json:"funding_bucket_id"`
	Amount          int64  `json:"amount"`
	Ordinal         int    `json:"ordinal"`
}

func envelopeFor(t *testing.T, legs ...legFor) []byte {
	t.Helper()
	type envelope struct {
		Allocations []legFor `json:"allocations"`
	}
	raw, err := json.Marshal(envelope{Allocations: legs})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

func TestAFactAppliesItsWholeDerivationAndRecordsOneRow(t *testing.T) {
	world := newApplierWorld(t)
	if err := world.applier.Apply(context.Background(), settledFact(t)); err != nil {
		t.Fatalf("Apply() error = %v, want nil", err)
	}

	// The hold legs first, in waterfall order, then the settlement's
	// consume-and-release plan per bucket: 450 consumed on A, 250 of B's
	// 400 consumed with 150 released. The moves are the applier's own
	// sentence about the fact; the algebra behind them is the primitives'.
	want := []string{
		"hold:" + bucketA + ":" + applierRequestID + ":450",
		"hold:" + bucketB + ":" + applierRequestID + ":400",
		"settle:" + bucketA + ":450:450",
		"settle:" + bucketB + ":400:250",
	}
	if len(world.settler.moves) != len(want) {
		t.Fatalf("moves = %v, want %v", world.settler.moves, want)
	}
	for i, move := range want {
		if world.settler.moves[i] != move {
			t.Errorf("moves[%d] = %s, want %s", i, world.settler.moves[i], move)
		}
	}

	// One applied row, with the lineage: the settled amount and capture
	// provenance from the fact, the settlement of record from the settle.
	row := world.ledger.rows[factKey{applierRequestID, ingestion.ClassSettlement}]
	if row == nil {
		t.Fatal("no applied row was recorded")
	}
	if row.Kind != ingestion.KindSettled || row.AppendSeq != 7 {
		t.Errorf("row identity = (%s, %d), want (settled, 7)", row.Kind, row.AppendSeq)
	}
	if row.SettledAmount == nil || *row.SettledAmount != 700 {
		t.Errorf("row settled amount = %v, want 700", row.SettledAmount)
	}
	if row.SettlementID != "e1000000-0000-7000-8000-0000000000e1" {
		t.Errorf("row settlement = %s, want the settlement the settle produced", row.SettlementID)
	}
	if len(world.quarantine.rows) != 0 {
		t.Errorf("quarantine recorded %d rows, want none", len(world.quarantine.rows))
	}
}

func TestAReplayedFactIsAReadAndNotASecondWrite(t *testing.T) {
	world := newApplierWorld(t)
	fact := settledFact(t)
	if err := world.applier.Apply(context.Background(), fact); err != nil {
		t.Fatalf("first Apply() error = %v, want nil", err)
	}
	world.settler.moves = nil
	if err := world.applier.Apply(context.Background(), fact); err != nil {
		t.Fatalf("replayed Apply() error = %v, want nil", err)
	}
	if len(world.settler.moves) != 0 {
		t.Fatalf("a replay moved money: %v", world.settler.moves)
	}
	if len(world.ledger.rows) != 1 {
		t.Fatalf("applied rows = %d, want the one", len(world.ledger.rows))
	}
	if len(world.quarantine.rows) != 0 {
		t.Fatalf("a true redelivery was quarantined: %d rows", len(world.quarantine.rows))
	}
}

func TestARedeliveryClaimingAClosedClassUnderADifferentKindIsQuarantined(t *testing.T) {
	// One class books one effect, so the second fact claiming the request's
	// settlement class is either the same outcome again — the same kind,
	// whatever position it rides, because the runtime re-appends and a
	// replayed range re-reads — or two terminal states claiming one request.
	// The contradiction is recorded rather than applied in silence or
	// wedged into a page stop: the disposition a fact-vs-books disagreement
	// earns.
	world := newApplierWorld(t)
	if err := world.applier.Apply(context.Background(), settledFact(t)); err != nil {
		t.Fatalf("the first settled fact: %v", err)
	}
	world.settler.moves = nil

	t.Run("same kind at a different position is the same outcome again", func(t *testing.T) {
		fact := settledFact(t)
		fact.AppendSeq = 8
		if err := world.applier.Apply(context.Background(), fact); err != nil {
			t.Fatalf("Apply() error = %v, want the no-op the class key promises", err)
		}
		if len(world.quarantine.rows) != 0 {
			t.Fatalf("a re-appended outcome was quarantined: %d rows", len(world.quarantine.rows))
		}
		if len(world.settler.moves) != 0 {
			t.Fatalf("a redelivery moved money: %v", world.settler.moves)
		}
	})
	t.Run("a different kind for the same class", func(t *testing.T) {
		fact := persistence.Fact{
			AppendSeq:     9,
			RequestID:     applierRequestID,
			Kind:          ingestion.KindExpired,
			SchemaVersion: ingestion.SchemaVersion,
			Payload:       envelopeFor(t, legFor{FundingBucketID: bucketA, Amount: 40, Ordinal: 1}),
		}
		if err := world.applier.Apply(context.Background(), fact); err != nil {
			t.Fatalf("Apply() error = %v, want the recorded disposition", err)
		}
		if len(world.quarantine.rows) != 1 {
			t.Fatalf("quarantine rows = %d, want the contradiction", len(world.quarantine.rows))
		}
		if reason := world.quarantine.rows[0].Reason; !strings.Contains(reason, "settled") || !strings.Contains(reason, "expired") {
			t.Errorf("reason = %q, want both kinds named", reason)
		}
	})
	t.Run("the contradiction moved no money and rewrote no row", func(t *testing.T) {
		if len(world.settler.moves) != 0 {
			t.Errorf("a quarantined contradiction moved money: %v", world.settler.moves)
		}
		row := world.ledger.rows[factKey{applierRequestID, ingestion.ClassSettlement}]
		if row == nil || row.Kind != ingestion.KindSettled || row.AppendSeq != 7 {
			t.Errorf("surviving row = %+v, want the first fact's (settled, 7)", row)
		}
	})
}

func TestAnOrphanBooksNothingButItsRow(t *testing.T) {
	world := newApplierWorld(t)
	capture := "gateway_observed"
	attempt := "0c000000-0000-7000-8000-00000000000c"
	inTokens := int64(1234)
	fact := persistence.Fact{
		AppendSeq:           9,
		RequestID:           applierRequestID,
		Kind:                ingestion.KindUnbillableOrphaned,
		SchemaVersion:       ingestion.SchemaVersion,
		Payload:             envelopeFor(t, legFor{FundingBucketID: bucketA, Amount: 40, Ordinal: 1}),
		CaptureMethod:       &capture,
		CommittedAttemptID:  &attempt,
		ProviderInputTokens: &inTokens,
	}
	if err := world.applier.Apply(context.Background(), fact); err != nil {
		t.Fatalf("Apply() error = %v, want nil", err)
	}
	if len(world.settler.moves) != 0 {
		t.Fatalf("an orphan moved money: %v", world.settler.moves)
	}
	row := world.ledger.rows[factKey{applierRequestID, ingestion.ClassUnbillableOrphaned}]
	if row == nil {
		t.Fatal("the orphan's row was not recorded")
	}
	if row.CaptureMethod == nil || *row.CaptureMethod != "gateway_observed" {
		t.Errorf("row capture = %v, want gateway_observed", row.CaptureMethod)
	}
	if row.SettledAmount != nil || row.SettlementID != "" {
		t.Errorf("row carries settled shape (%v, %s), want none", row.SettledAmount, row.SettlementID)
	}
}

func TestAnOrphanBooksBesideItsSettlement(t *testing.T) {
	// The two classes are independent rows: the orphan's arrival after the
	// settlement books the orphan beside it, which is the coexistence the
	// kind-classed key exists for.
	world := newApplierWorld(t)
	if err := world.applier.Apply(context.Background(), settledFact(t)); err != nil {
		t.Fatalf("settlement Apply() error = %v, want nil", err)
	}
	orphan := persistence.Fact{
		AppendSeq:     10,
		RequestID:     applierRequestID,
		Kind:          ingestion.KindUnbillableOrphaned,
		SchemaVersion: ingestion.SchemaVersion,
		Payload:       envelopeFor(t),
	}
	capture := "reported"
	attempt := "0c000000-0000-7000-8000-00000000000c"
	orphan.CaptureMethod = &capture
	orphan.CommittedAttemptID = &attempt
	if err := world.applier.Apply(context.Background(), orphan); err != nil {
		t.Fatalf("orphan Apply() error = %v, want nil", err)
	}
	if len(world.ledger.rows) != 2 {
		t.Fatalf("applied rows = %d, want one per class", len(world.ledger.rows))
	}
}

func TestAReleaseFactReturnsEveryLegInFull(t *testing.T) {
	world := newApplierWorld(t)
	fact := persistence.Fact{
		AppendSeq:     11,
		RequestID:     applierRequestID,
		Kind:          ingestion.KindReleased,
		SchemaVersion: ingestion.SchemaVersion,
		Payload: envelopeFor(t,
			legFor{FundingBucketID: bucketA, Amount: 40, Ordinal: 1},
			legFor{FundingBucketID: bucketB, Amount: 10, Ordinal: 2},
		),
	}
	if err := world.applier.Apply(context.Background(), fact); err != nil {
		t.Fatalf("Apply() error = %v, want nil", err)
	}
	want := []string{
		"hold:" + bucketA + ":" + applierRequestID + ":40",
		"release:" + bucketA + ":" + applierRequestID + ":40",
		"hold:" + bucketB + ":" + applierRequestID + ":10",
		"release:" + bucketB + ":" + applierRequestID + ":10",
	}
	if len(world.settler.moves) != len(want) {
		t.Fatalf("moves = %v, want %v", world.settler.moves, want)
	}
	for i, move := range want {
		if world.settler.moves[i] != move {
			t.Errorf("moves[%d] = %s, want %s", i, world.settler.moves[i], move)
		}
	}
	// The row records no settled shape: a release captured nothing.
	row := world.ledger.rows[factKey{applierRequestID, ingestion.ClassSettlement}]
	if row == nil {
		t.Fatal("no applied row was recorded")
	}
	if row.SettledAmount != nil || row.SettlementID != "" || row.CaptureMethod != nil {
		t.Errorf("release row = %+v, want a settled-shape-free row", row)
	}
}

func TestAZeroPricedSettleIsAHeaderOfRecord(t *testing.T) {
	// Zero is a value: a free model's settle is a header of record with no
	// legs — no hold legs, no consume, no release — and the applied row
	// still records the real zero amount, because the request happened.
	world := newApplierWorld(t)
	fact := settledFact(t)
	zero := int64(0)
	fact.SettledAmount = &zero
	*fact.InputUnitPrice = 0
	*fact.OutputUnitPrice = 0
	fact.Payload = envelopeFor(t)
	if err := world.applier.Apply(context.Background(), fact); err != nil {
		t.Fatalf("Apply() error = %v, want nil", err)
	}
	if len(world.settler.moves) != 0 {
		t.Fatalf("moves = %v, want a header-only settle with no legs", world.settler.moves)
	}
	row := world.ledger.rows[factKey{applierRequestID, ingestion.ClassSettlement}]
	if row == nil {
		t.Fatal("no applied row was recorded")
	}
	if row.SettledAmount == nil || *row.SettledAmount != 0 {
		t.Errorf("row settled amount = %v, want the real zero", row.SettledAmount)
	}
	if row.SettlementID == "" {
		t.Error("a zero settle is still a settlement of record")
	}
}

func TestAQuarantinableRefusalIsRecordedAndThePageAdvances(t *testing.T) {
	world := newApplierWorld(t)
	fact := settledFact(t)
	fact.Kind = "refunded" // a kind this build does not implement
	if err := world.applier.Apply(context.Background(), fact); err != nil {
		t.Fatalf("Apply() error = %v, want nil — the refusal is the record, not an error", err)
	}
	if len(world.quarantine.rows) != 1 {
		t.Fatalf("quarantine rows = %d, want the refusal", len(world.quarantine.rows))
	}
	recorded := world.quarantine.rows[0]
	if recorded.Kind != "refunded" || recorded.AppendSeq != 7 || recorded.RequestID != applierRequestID {
		t.Errorf("recorded identity = (%s, %d, %s), want the fact verbatim", recorded.RequestID, recorded.AppendSeq, recorded.Kind)
	}
	if !strings.Contains(recorded.Reason, "unknown fact kind") {
		t.Errorf("reason = %q, want the interpretation's own words", recorded.Reason)
	}
	if len(world.settler.moves) != 0 {
		t.Errorf("a quarantined fact moved money: %v", world.settler.moves)
	}
	if _, applied := world.ledger.rows[factKey{applierRequestID, ingestion.ClassSettlement}]; applied {
		t.Error("a quarantined fact recorded an applied row")
	}
}

func TestAnUnrecordableRefusalStopsThePage(t *testing.T) {
	world := newApplierWorld(t)
	fact := settledFact(t)
	fact.Payload = make([]byte, ingestion.MaxPayloadOctets+1)
	err := world.applier.Apply(context.Background(), fact)
	if err == nil {
		t.Fatal("Apply() = nil, want the oversize refusal — a payload the quarantine cannot record stops the page")
	}
	if len(world.quarantine.rows) != 0 {
		t.Errorf("the unrecordable refusal was recorded anyway: %d rows", len(world.quarantine.rows))
	}
}

func TestAnAccountingRefusalStopsThePage(t *testing.T) {
	world := newApplierWorld(t)
	world.settler.failOn = "settle"
	err := world.applier.Apply(context.Background(), settledFact(t))
	if !errors.Is(err, accounting.ErrSettlementConflict) {
		t.Fatalf("Apply() error = %v, want the settlement conflict upward", err)
	}
	if len(world.quarantine.rows) != 0 {
		t.Errorf("a ledger state was quarantined as if it were the fact's disposition: %d rows", len(world.quarantine.rows))
	}
	if len(world.ledger.rows) != 0 {
		t.Errorf("an effect that did not land recorded an applied row: %d rows", len(world.ledger.rows))
	}
}

func TestAConvergedSettleWithNoAppliedRowStopsThePage(t *testing.T) {
	// A settle that converges — the ledger already holds this request's
	// settlement, matched total — with no applied fact on file means the
	// books disagree about who closed the request: settled through another
	// door, or a once-applied page lost its own atomicity. No double charge
	// is possible, but the state is the plane's, not the fact's disposition
	// to paper over: the page stops, the same doctrine as a conflicting
	// total.
	world := newApplierWorld(t)
	world.settler.converge = true
	err := world.applier.Apply(context.Background(), settledFact(t))
	if err == nil || !strings.Contains(err.Error(), "already settled") {
		t.Fatalf("Apply() error = %v, want the divergence named", err)
	}
	if len(world.ledger.rows) != 0 {
		t.Errorf("the divergence recorded an applied row: %d rows", len(world.ledger.rows))
	}
	if len(world.quarantine.rows) != 0 {
		t.Errorf("a ledger state was quarantined as if it were the fact's disposition: %d rows", len(world.quarantine.rows))
	}
}

func TestALedgerReadFailureStopsThePage(t *testing.T) {
	world := newApplierWorld(t)
	world.ledger.failFind = true
	if err := world.applier.Apply(context.Background(), settledFact(t)); err == nil {
		t.Fatal("Apply() = nil, want the read failure — a replay check that cannot run must not guess")
	}
	if len(world.settler.moves) != 0 {
		t.Errorf("money moved without the replay check: %v", world.settler.moves)
	}
}

func TestANewFactApplierRefusesNilPorts(t *testing.T) {
	world := newApplierWorld(t)
	// The nils are interface nils on purpose: a typed-nil pointer would
	// satisfy the port and this test would prove nothing.
	cases := []struct {
		name        string
		accounting  holdSettler
		applied     persistence.AppliedFacts
		quarantined persistence.QuarantinedFacts
	}{
		{"no accounting", nil, world.ledger, world.quarantine},
		{"no ledger", world.settler, nil, world.quarantine},
		{"no quarantine", world.settler, world.ledger, nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewFactApplier accepted a missing port")
				}
			}()
			NewFactApplier(tt.accounting, tt.applied, tt.quarantined)
		})
	}
}
