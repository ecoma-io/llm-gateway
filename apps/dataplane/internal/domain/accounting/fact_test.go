package accounting

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// Every test here pins the fact's contract: one immutable statement about a
// request's ending, whose kind and figures agree, whose payload is the v1
// envelope with the allocation tail inside it, and whose usage-bearing kinds
// name the attempt their usage came from. The Control Plane derives its
// ledger legs from this row and nothing else, so the field names in the
// payload and the pointer-vs-nil distinction on the prices are pinned as
// wire contract, not implementation detail.

var occurredAt = time.Date(2026, 9, 25, 12, 0, 4, 0, time.UTC)

// factWaterfall builds a fresh two-leg allocation tail totalling 5000 — the
// legs the reservation held, as the fact closes them out.
func factWaterfall() []AllocationLeg {
	return []AllocationLeg{
		{FundingBucketID: "bucket-entitlement", Amount: 3000, Ordinal: 1},
		{FundingBucketID: "bucket-payg", Amount: 2000, Ordinal: 2},
	}
}

// usageReport builds a fresh report of the three token counts, so no test
// can alias another's pointers.
func usageReport() (*int64, *int64, *int64) {
	input, output, delivery := int64(511), int64(137), int64(12)
	return &input, &output, &delivery
}

// payloadOctets is the exact serialised size setPayload produces for legs,
// computed from the same marshalled legs: the flat envelope
// `{"allocations":[...]}` with commas between entries. The payload-cap test
// stands on this arithmetic, so it is asserted against the real writer's
// output before the boundary cases run.
func payloadOctets(legs []AllocationLeg) int {
	total := len(`{"allocations":[`) + len(`]}`)
	for i, leg := range legs {
		encoded, err := json.Marshal(leg)
		if err != nil {
			panic("payloadOctets: " + err.Error())
		}
		if i > 0 {
			total++
		}
		total += len(encoded)
	}
	return total
}

// legsTotalling builds an allocation tail whose v1 payload serialises to
// exactly target octets, by growing the last leg's bucket id one octet at a
// time — each growth adds exactly one octet, so the walk terminates exactly
// on the boundary it is aiming for.
func legsTotalling(t *testing.T, target int) []AllocationLeg {
	t.Helper()
	legs := factWaterfall()
	legs[len(legs)-1].FundingBucketID = "bucket-payg"
	for payloadOctets(legs) < target {
		legs[len(legs)-1].FundingBucketID += "0"
	}
	if got := payloadOctets(legs); got != target {
		t.Fatalf("legsTotalling(%d) built a payload of %d octets", target, got)
	}
	return legs
}

// settledFact builds the fixture every settled-shape test starts from: a
// reported settle of the whole hold under the standard snapshot. The amount
// is the hold formula's own answer for the report below —
// ceil((511·15 + 137·60)/1_000_000) = ceil(15 885/1 000 000) = 1 — because
// the constructor binds the amount to the figures and refuses anything else;
// the binding itself is TestNewSettledBindsTheAmountToTheHoldFormula's
// subject.
func settledFact(t *testing.T, legs []AllocationLeg) Fact {
	t.Helper()
	input, output, delivery := usageReport()
	fact, err := NewSettled("req-0001", "att-0001", CaptureReported, input, output, delivery, "rev-2026-09", 15, 60, 1, legs, occurredAt)
	if err != nil {
		t.Fatalf("NewSettled() error = %v", err)
	}
	return fact
}

// TestNewSettledProducesTheV1EnvelopeAndThePricingPointers pins the settled
// fact's shape end to end: kind, schema version, capture method and attempt
// carried; the three pricing figures present as non-nil pointers holding
// exactly the values passed (the fact's whole pricing basis, present together
// or not at all); the usage pointers surviving as handed over; and the
// payload a flat v1 envelope whose entries carry the wire field names the
// Control Plane binds against, with each leg's bucket, amount and ordinal
// exactly as passed. AppendSeq stays zero: the store allocates it, at commit.
func TestNewSettledProducesTheV1EnvelopeAndThePricingPointers(t *testing.T) {
	legs := factWaterfall()
	fact := settledFact(t, legs)

	if fact.Kind != KindSettled {
		t.Errorf("Kind = %q, want %q", fact.Kind, KindSettled)
	}
	if fact.SchemaVersion != SchemaVersion || SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want the package constant 1", fact.SchemaVersion)
	}
	if fact.CaptureMethod != CaptureReported {
		t.Errorf("CaptureMethod = %q, want %q", fact.CaptureMethod, CaptureReported)
	}
	if fact.RequestID != identity.RequestID("req-0001") || fact.CommittedAttemptID != identity.AttemptID("att-0001") {
		t.Errorf("identity = (%q, %q), want the request and the attempt the usage came from", fact.RequestID, fact.CommittedAttemptID)
	}
	if fact.OccurredAt != occurredAt {
		t.Errorf("OccurredAt = %v, want %v", fact.OccurredAt, occurredAt)
	}
	if fact.AppendSeq != 0 {
		t.Errorf("AppendSeq = %d, want 0 — the store allocates it at commit, not the caller", fact.AppendSeq)
	}
	if fact.InputUnitPrice == nil || fact.OutputUnitPrice == nil || fact.SettledAmount == nil {
		t.Fatalf("pricing pointers = (%v, %v, %v), want all three present — a settled fact carries its pricing basis whole", fact.InputUnitPrice, fact.OutputUnitPrice, fact.SettledAmount)
	}
	if *fact.InputUnitPrice != 15 || *fact.OutputUnitPrice != 60 || *fact.SettledAmount != 1 {
		t.Errorf("pricing = (%d, %d, %d), want (15, 60, 1) — the amount the hold formula derives from the report", *fact.InputUnitPrice, *fact.OutputUnitPrice, *fact.SettledAmount)
	}
	if fact.PriceRevision != "rev-2026-09" {
		t.Errorf("PriceRevision = %q, want rev-2026-09", fact.PriceRevision)
	}
	if fact.ProviderInputTokens == nil || *fact.ProviderInputTokens != 511 || fact.ProviderOutputTokens == nil || *fact.ProviderOutputTokens != 137 || fact.DeliveryTokens == nil || *fact.DeliveryTokens != 12 {
		t.Errorf("usage = (%v, %v, %v), want the report carried as (511, 137, 12)", fact.ProviderInputTokens, fact.ProviderOutputTokens, fact.DeliveryTokens)
	}
	if fact.CorrectsAppendSeq != nil {
		t.Error("CorrectsAppendSeq is set, want nil — no fact this build writes corrects another")
	}

	// The helper's arithmetic is pinned against the real writer before the
	// cap boundary below may trust it.
	if len(fact.Payload) != payloadOctets(legs) {
		t.Fatalf("payload = %d octets, want %d — the boundary arithmetic must match the real writer", len(fact.Payload), payloadOctets(legs))
	}

	var envelope struct {
		Allocations []AllocationLeg `json:"allocations"`
	}
	if err := json.Unmarshal(fact.Payload, &envelope); err != nil {
		t.Fatalf("payload does not parse: %v", err)
	}
	if len(envelope.Allocations) != 2 {
		t.Fatalf("payload allocations = %v, want the two legs", envelope.Allocations)
	}
	for i, leg := range legs {
		if envelope.Allocations[i] != leg {
			t.Errorf("payload allocation %d = %+v, want %+v — the tail rides in the payload exactly as the hold drew it", i, envelope.Allocations[i], leg)
		}
	}

	// The struct round-trip above proves the values; the nameless map read
	// below proves the field names, which is the part a consumer's decoder
	// binds against and a struct tag change would silently break.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(fact.Payload, &raw); err != nil {
		t.Fatalf("payload does not parse as an object: %v", err)
	}
	entries, ok := raw["allocations"]
	if !ok {
		t.Fatal(`payload carries no "allocations" key — the envelope's one field name is the contract`)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(entries, &decoded); err != nil {
		t.Fatalf("allocations do not parse: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("decoded allocations = %v, want the two legs", decoded)
	}
	for _, key := range []string{"funding_bucket_id", "amount", "ordinal"} {
		if _, present := decoded[0][key]; !present {
			t.Errorf("allocation entry %v lacks the %q field — the wire name the Control Plane derives from", decoded[0], key)
		}
	}
}

// TestANewSettledFactCanPriceAtZero pins the reason the prices and the
// amount are pointers at all: zero is a real price and a real settled amount
// — a free model settles at zero — and nil is a different claim entirely.
// Zero must survive as a present zero, not collapse into absence.
func TestANewSettledFactCanPriceAtZero(t *testing.T) {
	input, output, delivery := usageReport()
	fact, err := NewSettled("req-0001", "att-0001", CaptureReported, input, output, delivery, "rev-free", 0, 0, 0, factWaterfall(), occurredAt)
	if err != nil {
		t.Fatalf("NewSettled() at zero prices error = %v", err)
	}
	if fact.InputUnitPrice == nil || *fact.InputUnitPrice != 0 || fact.OutputUnitPrice == nil || *fact.OutputUnitPrice != 0 || fact.SettledAmount == nil || *fact.SettledAmount != 0 {
		t.Errorf("pricing pointers = (%v, %v, %v), want three non-nil pointers at zero", fact.InputUnitPrice, fact.OutputUnitPrice, fact.SettledAmount)
	}
}

// TestNewSettledBindsTheAmountToTheHoldFormula pins the amount's binding to
// the figures the fact itself carries: the hold formula is the one place the
// arithmetic lives, so a settled amount the formula does not re-derive from
// the fact's own counts and prices is refused — an auditor must be able to
// take the row alone and reach the same number. The cases cover the non-zero
// ceiling (a fractional derivation rounds up), an exact derivation, the
// absent-count case that prices at zero, and the refusal naming both the
// amount offered and the amount derived.
func TestNewSettledBindsTheAmountToTheHoldFormula(t *testing.T) {
	tests := []struct {
		name              string
		input, output     *int64
		delivery          *int64
		inPrice, outPrice int64
		amount            int64
		want              int64 // the amount the formula derives and the fact must carry
		wantErr           bool  // the offered amount disagrees with the derivation
	}{
		{
			name:  "a fractional derivation rounds up",
			input: ptrInt64(40000), output: ptrInt64(20000),
			inPrice: 15, outPrice: 60,
			amount: 2, // ceil((40000·15 + 20000·60)/1_000_000) = ceil(1.8) = 2
			want:   2,
		},
		{
			name:  "the rounded-up amount one below is refused",
			input: ptrInt64(40000), output: ptrInt64(20000),
			inPrice: 15, outPrice: 60,
			amount:  1, // the formula says 2
			wantErr: true,
		},
		{
			name:  "an exact derivation needs no rounding",
			input: ptrInt64(40000), output: ptrInt64(20000),
			inPrice: 100, outPrice: 100,
			amount: 6, // (40000·100 + 20000·100)/1_000_000 = 6 exactly
			want:   6,
		},
		{
			name:  "an over-charged exact derivation is refused",
			input: ptrInt64(40000), output: ptrInt64(20000),
			inPrice: 100, outPrice: 100,
			amount:  7, // the formula says 6
			wantErr: true,
		},
		{
			name:  "absent counts price at zero and bind at zero",
			input: nil, output: nil,
			inPrice: 15, outPrice: 60,
			amount: 0,
			want:   0,
		},
		{
			name:  "absent counts with a claimed amount are refused",
			input: nil, output: nil,
			inPrice: 15, outPrice: 60,
			amount:  1, // the formula prices nothing at 0
			wantErr: true,
		},
		{
			name:  "delivery tokens never participate in the pricing",
			input: ptrInt64(0), output: ptrInt64(0), delivery: ptrInt64(999999),
			inPrice: 15, outPrice: 60,
			amount: 0, // the counts price to zero whatever was delivered
			want:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delivery := tt.delivery
			if tt.input == nil && tt.output == nil && delivery == nil {
				_, _, delivery = usageReport()
			}
			fact, err := NewSettled("req-0001", "att-0001", CaptureReported, tt.input, tt.output, delivery, "rev-2026-09", tt.inPrice, tt.outPrice, tt.amount, factWaterfall(), occurredAt)
			if tt.wantErr {
				if !errors.Is(err, ErrFactShape) || !strings.Contains(err.Error(), "disagrees") {
					t.Fatalf("NewSettled() error = %v, want the ErrFactShape disagreement naming both amounts", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewSettled() error = %v", err)
			}
			if fact.SettledAmount == nil || *fact.SettledAmount != tt.want {
				t.Errorf("SettledAmount = %v, want %d", fact.SettledAmount, tt.want)
			}
		})
	}
}

// TestNewSettledRefusesFiguresItCannotSettle pins every shape refusal the
// settled constructor makes, all of them wrapped in ErrFactShape: an unnamed
// attempt (usage with no attempt is a claim nothing traces), an unknown
// capture method, a missing pricing basis, a negative price or amount, and a
// negative usage count — usage is a count, not a balance, and a present
// negative is a claim nothing can settle.
func TestNewSettledRefusesFiguresItCannotSettle(t *testing.T) {
	valid := func() (identity.AttemptID, CaptureMethod, string, int64, int64, int64, *int64, *int64, *int64) {
		input, output, delivery := usageReport()
		return "att-0001", CaptureReported, "rev-2026-09", 15, 60, 1, input, output, delivery
	}
	type settledArgs struct {
		attempt  identity.AttemptID
		capture  CaptureMethod
		revision string
		inPrice  int64
		outPrice int64
		amount   int64
		input    *int64
		output   *int64
		delivery *int64
	}
	tests := []struct {
		name  string
		spoil func(*settledArgs)
	}{
		{name: "refuses an unnamed attempt", spoil: func(a *settledArgs) { a.attempt = "" }},
		{name: "refuses an unknown capture method", spoil: func(a *settledArgs) { a.capture = "guessed" }},
		{name: "refuses a missing pricing basis", spoil: func(a *settledArgs) { a.revision = "" }},
		{name: "refuses a negative input price", spoil: func(a *settledArgs) { a.inPrice = -1 }},
		{name: "refuses a negative output price", spoil: func(a *settledArgs) { a.outPrice = -1 }},
		{name: "refuses a negative settled amount", spoil: func(a *settledArgs) { a.amount = -1 }},
		{name: "refuses a negative input count", spoil: func(a *settledArgs) { a.input = ptrInt64(-1) }},
		{name: "refuses a negative output count", spoil: func(a *settledArgs) { a.output = ptrInt64(-1) }},
		{name: "refuses a negative delivery count", spoil: func(a *settledArgs) { a.delivery = ptrInt64(-1) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempt, capture, revision, inPrice, outPrice, amount, input, output, delivery := valid()
			args := settledArgs{attempt, capture, revision, inPrice, outPrice, amount, input, output, delivery}
			tt.spoil(&args)

			fact, err := NewSettled("req-0001", args.attempt, args.capture, args.input, args.output, args.delivery, args.revision, args.inPrice, args.outPrice, args.amount, factWaterfall(), occurredAt)
			if !errors.Is(err, ErrFactShape) {
				t.Errorf("NewSettled() error = %v, want ErrFactShape", err)
			}
			// Fact carries a json.RawMessage, so it is not comparable whole;
			// the fields that would betray a half-built fact are the kind,
			// the request and the payload.
			if fact.Kind != "" || fact.RequestID != "" || len(fact.Payload) != 0 {
				t.Errorf("NewSettled() = (%q, %q, %d payload octets), want nothing built — a refused shape builds no half-fact", fact.Kind, fact.RequestID, len(fact.Payload))
			}
		})
	}
}

// TestNewSettledEnforcesThePayloadCapAt16384Octets pins the writer-side cap
// on the serialised envelope, exactly on both sides of the boundary: a tail
// whose payload is 16384 octets builds, one octet more is refused with the
// cap error that names both numbers. The refusal is deliberately the writer
// cap's own error — setPayload's message carries the octet counts, parallel
// to the provider-error cap in package execution — and not the ErrFactShape
// shape sentinel, whose documented scope is a fact whose kind and figures
// disagree; this assertion pins the refusal without conflating the two.
func TestNewSettledEnforcesThePayloadCapAt16384Octets(t *testing.T) {
	input, output, delivery := usageReport()

	atCap, err := NewSettled("req-0001", "att-0001", CaptureReported, input, output, delivery, "rev-2026-09", 15, 60, 1, legsTotalling(t, maxPayloadOctets), occurredAt)
	if err != nil {
		t.Fatalf("NewSettled() at exactly %d octets error = %v", maxPayloadOctets, err)
	}
	if len(atCap.Payload) != maxPayloadOctets {
		t.Errorf("payload = %d octets, want exactly %d on the accepted side of the boundary", len(atCap.Payload), maxPayloadOctets)
	}

	_, err = NewSettled("req-0001", "att-0001", CaptureReported, input, output, delivery, "rev-2026-09", 15, 60, 1, legsTotalling(t, maxPayloadOctets+1), occurredAt)
	if err == nil {
		t.Fatal("NewSettled() one octet over the cap succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "16384") || !strings.Contains(err.Error(), "16385") {
		t.Errorf("NewSettled() error = %q, want it to name both the payload's octets and the cap", err)
	}
}

// TestReleasedAndExpiredFactsCarryNoSettlementFigures pins the two closes
// without settleable usage: their constructors take no figure parameters at
// all — the non-usage shape is enforced by the type, and this test pins the
// result, that the built fact carries no usage, no pricing, no amount, no
// attempt and no capture method, while its allocation tail rides in the
// payload for the Control Plane's return to derive from. The payload cap
// binds them the same as a settled fact.
func TestReleasedAndExpiredFactsCarryNoSettlementFigures(t *testing.T) {
	builders := map[string]func(legs []AllocationLeg) (Fact, error){
		string(KindReleased): func(legs []AllocationLeg) (Fact, error) { return NewReleased("req-0001", legs, occurredAt) },
		string(KindExpired):  func(legs []AllocationLeg) (Fact, error) { return NewExpired("req-0001", legs, occurredAt) },
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			legs := factWaterfall()
			fact, err := build(legs)
			if err != nil {
				t.Fatalf("builder error = %v", err)
			}
			if fact.Kind != Kind(name) {
				t.Errorf("Kind = %q, want %q", fact.Kind, name)
			}
			if fact.SchemaVersion != SchemaVersion {
				t.Errorf("SchemaVersion = %d, want %d", fact.SchemaVersion, SchemaVersion)
			}
			if fact.ProviderInputTokens != nil || fact.ProviderOutputTokens != nil || fact.DeliveryTokens != nil {
				t.Errorf("usage = (%v, %v, %v), want all nil — no usage is claimed by this ending", fact.ProviderInputTokens, fact.ProviderOutputTokens, fact.DeliveryTokens)
			}
			if fact.InputUnitPrice != nil || fact.OutputUnitPrice != nil || fact.SettledAmount != nil {
				t.Errorf("pricing = (%v, %v, %v), want all nil — this ending carries no pricing at all", fact.InputUnitPrice, fact.OutputUnitPrice, fact.SettledAmount)
			}
			if fact.CommittedAttemptID != "" || fact.CaptureMethod != "" || fact.PriceRevision != "" {
				t.Errorf("attempt = %q, capture = %q, revision = %q, want all empty", fact.CommittedAttemptID, fact.CaptureMethod, fact.PriceRevision)
			}
			if fact.CorrectsAppendSeq != nil {
				t.Error("CorrectsAppendSeq is set, want nil")
			}
			var envelope struct {
				Allocations []AllocationLeg `json:"allocations"`
			}
			if err := json.Unmarshal(fact.Payload, &envelope); err != nil {
				t.Fatalf("payload does not parse: %v", err)
			}
			if len(envelope.Allocations) != 2 || envelope.Allocations[1] != legs[1] {
				t.Errorf("payload allocations = %v, want the hold's legs — the return derives from them", envelope.Allocations)
			}

			_, err = build(legsTotalling(t, maxPayloadOctets+1))
			if err == nil || !strings.Contains(err.Error(), "16384") {
				t.Errorf("builder over the payload cap = %v, want the cap refusal naming the limit", err)
			}
		})
	}
}

// TestNewUnbillableOrphanedRecordsUsageWithoutAnAmount pins the orphaned
// completion: usage observed on a named attempt with a capture method, but
// no price, no revision and never a customer amount — the one usage-bearing
// fact that exists outside the settlement dedup by design. Its refusals are
// the settled fact's shape rules minus pricing: an unnamed attempt, an
// unknown capture method and a negative count are all ErrFactShape.
func TestNewUnbillableOrphanedRecordsUsageWithoutAnAmount(t *testing.T) {
	input, output, delivery := usageReport()
	fact, err := NewUnbillableOrphaned("req-0001", "att-0009", CaptureGatewayObserved, input, output, delivery, factWaterfall(), occurredAt)
	if err != nil {
		t.Fatalf("NewUnbillableOrphaned() error = %v", err)
	}
	if fact.Kind != KindUnbillableOrphaned || fact.SchemaVersion != SchemaVersion {
		t.Errorf("fact = (%q, v%d), want (%q, v%d)", fact.Kind, fact.SchemaVersion, KindUnbillableOrphaned, SchemaVersion)
	}
	if fact.CommittedAttemptID != identity.AttemptID("att-0009") || fact.CaptureMethod != CaptureGatewayObserved {
		t.Errorf("attempt = %q, capture = %q, want the observed attempt and its method", fact.CommittedAttemptID, fact.CaptureMethod)
	}
	if fact.ProviderInputTokens == nil || *fact.ProviderInputTokens != 511 || fact.ProviderOutputTokens == nil || *fact.ProviderOutputTokens != 137 || fact.DeliveryTokens == nil || *fact.DeliveryTokens != 12 {
		t.Errorf("usage = (%v, %v, %v), want the observed report carried", fact.ProviderInputTokens, fact.ProviderOutputTokens, fact.DeliveryTokens)
	}
	if fact.InputUnitPrice != nil || fact.OutputUnitPrice != nil || fact.SettledAmount != nil || fact.PriceRevision != "" {
		t.Errorf("pricing = (%v, %v, %v, %q), want none of it — an orphaned completion is never a charge", fact.InputUnitPrice, fact.OutputUnitPrice, fact.SettledAmount, fact.PriceRevision)
	}

	tests := []struct {
		name    string
		attempt identity.AttemptID
		capture CaptureMethod
		input   *int64
	}{
		{name: "refuses an unnamed attempt", capture: CaptureGatewayObserved},
		{name: "refuses an unknown capture method", attempt: "att-0009"},
		{name: "refuses a negative count", attempt: "att-0009", capture: CaptureGatewayObserved, input: ptrInt64(-1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, out, del := usageReport()
			if tt.input != nil {
				in = tt.input
			}
			_, err := NewUnbillableOrphaned("req-0001", tt.attempt, tt.capture, in, out, del, factWaterfall(), occurredAt)
			if !errors.Is(err, ErrFactShape) {
				t.Errorf("NewUnbillableOrphaned() error = %v, want ErrFactShape", err)
			}
		})
	}
}

// ptrInt64 takes an address for the refusal tables, which need distinct
// negative values per case; it asserts nothing.
func ptrInt64(value int64) *int64 {
	return &value
}
