package ingestion

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// This file pins Interpret against the contract's normative sentences
// (api/openapi/shared/usage-facts.yaml): the version-1 envelope, the leg
// grammar, the per-kind pairing rules, the settled amount's re-derivation,
// and the waterfall the derivation walks. Every refusal a fact can meet is
// named here, and the disposition split — quarantinable versus page stop —
// is pinned beside them, because the applier branches on that split and a
// sentinel in the wrong list is a fact applied that should have been
// recorded, or a page stopped that should have advanced.

// envelope serialises legs into the version-1 payload the way the writer
// does — through the same field names, in one flat object.
func envelope(t *testing.T, legs ...wireLeg) []byte {
	t.Helper()
	raw, err := json.Marshal(payloadV1{Allocations: legs})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return raw
}

// Two funding buckets in the version-7 uuid shape the runtime mints — the
// leg grammar's own word (bucketIDForm), exercised by every coherent fact
// below.
const (
	bucketA = "0b000000-0000-7000-8000-00000000000a"
	bucketB = "0b000000-0000-7000-8000-00000000000b"
)

// settledFact builds a coherent settled fact the derivation accepts, with
// the figures priced by the hold formula: 1_500_000 input at 200/M plus
// 500_000 output at 800/M is 300_000_000 + 400_000_000 raw, one ceiling
// over the sum, settled_amount 700.
func settledFact(t *testing.T, legs ...wireLeg) Fact {
	t.Helper()
	amount := int64(700)
	attempt := "att-1"
	capture := "reported"
	revision := "rev-1"
	inPrice := int64(200)
	outPrice := int64(800)
	inTokens := int64(1_500_000)
	outTokens := int64(500_000)
	delivery := int64(4096)
	return Fact{
		AppendSeq:            7,
		RequestID:            "d9000000-0000-7000-8000-0000000000d1",
		Kind:                 KindSettled,
		SchemaVersion:        SchemaVersion,
		Payload:              envelope(t, legs...),
		CaptureMethod:        &capture,
		CommittedAttemptID:   &attempt,
		ProviderInputTokens:  &inTokens,
		ProviderOutputTokens: &outTokens,
		DeliveryTokens:       &delivery,
		PriceRevisionID:      &revision,
		InputUnitPrice:       &inPrice,
		OutputUnitPrice:      &outPrice,
		SettledAmount:        &amount,
	}
}

func TestASettledFactDerivesTheWaterfall(t *testing.T) {
	// The contract's derivation, normatively: consume min(remaining,
	// amount) per leg in ascending ordinal order — the boundary leg may be
	// partially consumed — and release each leg's unconsumed remainder.
	// The tail sums to 850; the settlement consumes 700 of it greedy from
	// the head, so the boundary leg is the second one and its tail goes
	// back.
	fact := settledFact(t,
		wireLeg{FundingBucketID: bucketA, Amount: 450, Ordinal: 1},
		wireLeg{FundingBucketID: bucketB, Amount: 400, Ordinal: 2},
	)
	out, err := Interpret(fact)
	if err != nil {
		t.Fatalf("Interpret() error = %v, want nil", err)
	}
	if out.Class != ClassSettlement || out.Kind != KindSettled || out.RequestID != "d9000000-0000-7000-8000-0000000000d1" {
		t.Fatalf("Interpret() identity = (%s, %s, %s), want the settled settlement class for d9000000-0000-7000-8000-0000000000d1", out.RequestID, out.Kind, out.Class)
	}
	if out.SettledAmount != 700 {
		t.Errorf("SettledAmount = %d, want 700: one ceiling over 700_000_000 raw", out.SettledAmount)
	}
	if out.Price.RevisionID != "rev-1" || out.Price.InputUnitPrice != 200 || out.Price.OutputUnitPrice != 800 {
		t.Errorf("Price = %+v, want the fact's pricing basis copied by value", out.Price)
	}
	want := []Leg{
		{Bucket: bucketA, Held: 450, Consumed: 450, Released: 0},
		{Bucket: bucketB, Held: 400, Consumed: 250, Released: 150},
	}
	if len(out.Legs) != len(want) {
		t.Fatalf("Legs = %+v, want %d legs", out.Legs, len(want))
	}
	for i, leg := range want {
		if out.Legs[i] != leg {
			t.Errorf("Legs[%d] = %+v, want %+v", i, out.Legs[i], leg)
		}
	}
}

func TestAFullyConsumedTailReleasesNothing(t *testing.T) {
	fact := settledFact(t, wireLeg{FundingBucketID: bucketA, Amount: 700, Ordinal: 1})
	out, err := Interpret(fact)
	if err != nil {
		t.Fatalf("Interpret() error = %v, want nil", err)
	}
	if out.Legs[0] != (Leg{Bucket: bucketA, Held: 700, Consumed: 700, Released: 0}) {
		t.Fatalf("Legs = %+v, want the whole leg consumed", out.Legs)
	}
}

func TestAZeroPricedSettleIsAHeaderWithNoLegs(t *testing.T) {
	// Zero is a value on both shores: a hold that prices out to zero draws
	// no legs, and the settlement derived from it is still a settlement of
	// record — an outcome with no amount, no legs and no consume to price.
	// Both arms priced at zero is the free model; the counts stay real and
	// price to nothing.
	fact := settledFact(t)
	zero := int64(0)
	fact.SettledAmount = &zero
	*fact.InputUnitPrice = 0
	*fact.OutputUnitPrice = 0
	fact.Payload = envelope(t)
	out, err := Interpret(fact)
	if err != nil {
		t.Fatalf("Interpret() error = %v, want nil", err)
	}
	if out.SettledAmount != 0 || len(out.Legs) != 0 {
		t.Fatalf("outcome = (%d, %d legs), want a header of record with no legs", out.SettledAmount, len(out.Legs))
	}
}

func TestReleasedAndExpiredFactsReleaseEveryLegInFull(t *testing.T) {
	for _, kind := range []string{KindReleased, KindExpired} {
		fact := Fact{
			AppendSeq:     8,
			RequestID:     "d9000000-0000-7000-8000-0000000000d2",
			Kind:          kind,
			SchemaVersion: SchemaVersion,
			Payload: envelope(t,
				wireLeg{FundingBucketID: bucketA, Amount: 40, Ordinal: 1},
				wireLeg{FundingBucketID: bucketB, Amount: 10, Ordinal: 2},
			),
		}
		out, err := Interpret(fact)
		if err != nil {
			t.Fatalf("Interpret(%s) error = %v, want nil", kind, err)
		}
		if out.Class != ClassSettlement {
			t.Fatalf("class = %s, want %s", out.Class, ClassSettlement)
		}
		want := []Leg{
			{Bucket: bucketA, Held: 40, Consumed: 0, Released: 40},
			{Bucket: bucketB, Held: 10, Consumed: 0, Released: 10},
		}
		for i, leg := range want {
			if out.Legs[i] != leg {
				t.Errorf("%s: Legs[%d] = %+v, want %+v — the whole hold goes back", kind, i, out.Legs[i], leg)
			}
		}
	}
}

func TestAnOrphanDerivesNoLegsAtAll(t *testing.T) {
	// The tail rides along as telemetry of what was held; the orphan's
	// effect is none of it — an applied_facts row and nothing else.
	capture := "gateway_observed"
	attempt := "att-9"
	inTokens := int64(1234)
	fact := Fact{
		AppendSeq:           9,
		RequestID:           "d9000000-0000-7000-8000-0000000000d3",
		Kind:                KindUnbillableOrphaned,
		SchemaVersion:       SchemaVersion,
		Payload:             envelope(t, wireLeg{FundingBucketID: bucketA, Amount: 40, Ordinal: 1}),
		CaptureMethod:       &capture,
		CommittedAttemptID:  &attempt,
		ProviderInputTokens: &inTokens,
	}
	out, err := Interpret(fact)
	if err != nil {
		t.Fatalf("Interpret() error = %v, want nil", err)
	}
	if out.Class != ClassUnbillableOrphaned || out.CaptureMethod != "gateway_observed" {
		t.Fatalf("outcome = (%s, %s), want the orphan class with its capture provenance", out.Class, out.CaptureMethod)
	}
	if out.SettledAmount != 0 || len(out.Legs) != 0 {
		t.Fatalf("orphan derived (%d, %d legs), want no legs and no amount: no money moved", out.SettledAmount, len(out.Legs))
	}
}

func TestTheGrammarRefusesEveryMalformedFact(t *testing.T) {
	t.Run("an unknown kind is recorded, not guessed", func(t *testing.T) {
		fact := settledFact(t)
		fact.Kind = "refunded"
		_, err := Interpret(fact)
		if !errors.Is(err, ErrUnknownKind) || !Quarantinable(err) {
			t.Fatalf("error = %v, want quarantinable ErrUnknownKind", err)
		}
	})
	t.Run("an unknown schema version is refused, never guessed", func(t *testing.T) {
		fact := settledFact(t)
		fact.SchemaVersion = 2
		_, err := Interpret(fact)
		if !errors.Is(err, ErrUnknownSchemaVersion) || !Quarantinable(err) {
			t.Fatalf("error = %v, want quarantinable ErrUnknownSchemaVersion", err)
		}
	})
	t.Run("a correction names a path this build has not built", func(t *testing.T) {
		fact := settledFact(t)
		corrects := int64(3)
		fact.CorrectsAppendSeq = &corrects
		_, err := Interpret(fact)
		if !errors.Is(err, ErrCorrectionUnsupported) || !Quarantinable(err) {
			t.Fatalf("error = %v, want quarantinable ErrCorrectionUnsupported", err)
		}
	})
	t.Run("a request id outside the grammar is malformed", func(t *testing.T) {
		empty := settledFact(t)
		empty.RequestID = ""
		if _, err := Interpret(empty); !errors.Is(err, ErrMalformedFact) {
			t.Fatalf("empty request id: error = %v, want ErrMalformedFact", err)
		}
		long := settledFact(t)
		long.RequestID = strings.Repeat("r", 257)
		_, err := Interpret(long)
		if !errors.Is(err, ErrMalformedFact) || !Quarantinable(err) {
			t.Fatalf("257-octet request id: error = %v, want quarantinable ErrMalformedFact", err)
		}
	})
	t.Run("an append seq below one is malformed", func(t *testing.T) {
		fact := settledFact(t)
		fact.AppendSeq = 0
		if _, err := Interpret(fact); !errors.Is(err, ErrMalformedFact) {
			t.Fatalf("error = %v, want ErrMalformedFact", err)
		}
	})
	t.Run("a request id outside the reservation grammar is malformed", func(t *testing.T) {
		// The contract makes the request id the reservation's identity, and
		// the derived legs book the reservation through primitives that
		// validate the canonical uuid form. A fact carrying anything else
		// was not written by the runtime this feed belongs to, and refusing
		// it here is a recorded disposition rather than a hold that stops
		// the page mid-derivation.
		fact := settledFact(t)
		fact.RequestID = "req-not-a-uuid"
		if _, err := Interpret(fact); !errors.Is(err, ErrMalformedFact) || !Quarantinable(err) {
			t.Fatalf("error = %v, want quarantinable ErrMalformedFact", err)
		}
		upper := settledFact(t)
		upper.RequestID = "D9000000-0000-7000-8000-0000000000D1"
		if _, err := Interpret(upper); !errors.Is(err, ErrMalformedFact) {
			t.Fatalf("uppercase request id: error = %v, want ErrMalformedFact", err)
		}
	})
}

func TestThePayloadGrammarRefusesWhatNoWriterWrote(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"body that is not JSON", []byte("not json")},
		{"a body that is not the envelope", []byte(`{"allocation":[]}`)},
		{"a field the envelope does not carry", []byte(`{"allocations":[],"note":"hi"}`)},
		{"more than one JSON value", []byte(`{"allocations":[]} {"allocations":[]}`)},
		{"an empty body", []byte("")},
		{"a bare null document — the decode that succeeds against anything", []byte("null")},
		{"an array document", []byte(`[]`)},
		{"a string document", []byte(`"settled"`)},
		{"an ordinal hole", envelope(t, wireLeg{FundingBucketID: bucketA, Amount: 1, Ordinal: 2})},
		{"ordinals out of drawdown order", envelope(t,
			wireLeg{FundingBucketID: bucketA, Amount: 1, Ordinal: 1},
			wireLeg{FundingBucketID: bucketB, Amount: 1, Ordinal: 1},
		)},
		{"a non-positive amount", envelope(t, wireLeg{FundingBucketID: bucketA, Amount: 0, Ordinal: 1})},
		{"a blank bucket", envelope(t, wireLeg{FundingBucketID: "", Amount: 1, Ordinal: 1})},
		{"a repeated bucket", envelope(t,
			wireLeg{FundingBucketID: bucketA, Amount: 1, Ordinal: 1},
			wireLeg{FundingBucketID: bucketA, Amount: 1, Ordinal: 2},
		)},
		{"a bucket outside the uuid grammar", envelope(t, wireLeg{FundingBucketID: "prod-bucket-7", Amount: 1, Ordinal: 1})},
		{"a v4-shaped bucket", envelope(t, wireLeg{FundingBucketID: "0b000000-0000-4000-8000-00000000000a", Amount: 1, Ordinal: 1})},
		{"an uppercase bucket", envelope(t, wireLeg{FundingBucketID: "0B000000-0000-7000-8000-00000000000A", Amount: 1, Ordinal: 1})},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			fact := settledFact(t)
			fact.Payload = tt.payload
			_, err := Interpret(fact)
			if !errors.Is(err, ErrMalformedPayload) || !Quarantinable(err) {
				t.Fatalf("error = %v, want quarantinable ErrMalformedPayload", err)
			}
		})
	}
}

func TestAnOversizePayloadStopsThePage(t *testing.T) {
	// A payload the quarantine cannot record verbatim is not a quarantine:
	// a truncated copy of the evidence is not a record. This refusal is the
	// one this package makes that stops the page instead of advancing past
	// it.
	fact := settledFact(t)
	fact.Payload = make([]byte, MaxPayloadOctets+1)
	_, err := Interpret(fact)
	if !errors.Is(err, ErrPayloadOversize) {
		t.Fatalf("error = %v, want ErrPayloadOversize", err)
	}
	if Quarantinable(err) {
		t.Fatal("an unrecordable payload is not quarantinable: the page stops")
	}
}

func TestTheSettledPairingRules(t *testing.T) {
	// The tail sums to 850, so only the mutation under test breaks the
	// fact: each case below changes one sentence the settled shape states
	// and pins the refusal that names it.
	tail := func(t *testing.T) Fact {
		t.Helper()
		return settledFact(t,
			wireLeg{FundingBucketID: bucketA, Amount: 450, Ordinal: 1},
			wireLeg{FundingBucketID: bucketB, Amount: 400, Ordinal: 2},
		)
	}
	cases := []struct {
		name   string
		mutate func(*Fact)
	}{
		{"no capture method", func(f *Fact) { f.CaptureMethod = nil }},
		{"an unknown capture method", func(f *Fact) { *f.CaptureMethod = "guessed" }},
		{"no committed attempt", func(f *Fact) { f.CommittedAttemptID = nil }},
		{"no price revision", func(f *Fact) { f.PriceRevisionID = nil }},
		{"no unit prices", func(f *Fact) { f.InputUnitPrice = nil }},
		{"no settled amount", func(f *Fact) { f.SettledAmount = nil }},
		{"a negative count", func(f *Fact) { negative := int64(-1); f.ProviderInputTokens = &negative }},
		{"a negative price", func(f *Fact) { *f.OutputUnitPrice = -1 }},
		{"a negative settled amount", func(f *Fact) { *f.SettledAmount = -1 }},
		{"an amount the formula cannot re-derive", func(f *Fact) { *f.SettledAmount = 701 }},
		{"a positive amount over an empty tail", func(f *Fact) { f.Payload = envelope(t) }},
		{"a zero amount over a real tail", func(f *Fact) { zero := int64(0); f.SettledAmount = &zero }},
		{"a tail that cannot absorb the amount", func(f *Fact) {
			// The legs sum to 70; the amount prices to 700.
			f.Payload = envelope(t, wireLeg{FundingBucketID: bucketA, Amount: 70, Ordinal: 1})
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			fact := tail(t)
			tt.mutate(&fact)
			_, err := Interpret(fact)
			if !errors.Is(err, ErrIncoherentFact) || !Quarantinable(err) {
				t.Fatalf("error = %v, want quarantinable ErrIncoherentFact", err)
			}
		})
	}
}

func TestTheNoUsagePairingRules(t *testing.T) {
	// A released or expired fact claims no usage, and a claim of usage on a
	// no-usage fact is not extra telemetry but a contradiction. Each column
	// the contract makes null on these kinds is pinned: carry one and the
	// fact is incoherent.
	withUsage := func(t *testing.T, mutate func(*Fact)) Fact {
		t.Helper()
		fact := Fact{
			AppendSeq:     11,
			RequestID:     "d9000000-0000-7000-8000-0000000000d4",
			Kind:          KindReleased,
			SchemaVersion: SchemaVersion,
			Payload:       envelope(t, wireLeg{FundingBucketID: bucketA, Amount: 40, Ordinal: 1}),
		}
		mutate(&fact)
		return fact
	}
	capture := "reported"
	attempt := "att-2"
	count := int64(5)
	revision := "rev-1"
	price := int64(200)
	amount := int64(700)
	for name, mutate := range map[string]func(*Fact){
		"a capture method":    func(f *Fact) { f.CaptureMethod = &capture },
		"a committed attempt": func(f *Fact) { f.CommittedAttemptID = &attempt },
		"an input count":      func(f *Fact) { f.ProviderInputTokens = &count },
		"a delivery count":    func(f *Fact) { f.DeliveryTokens = &count },
		"a price revision":    func(f *Fact) { f.PriceRevisionID = &revision },
		"a unit price":        func(f *Fact) { f.InputUnitPrice = &price },
		"a settled amount":    func(f *Fact) { f.SettledAmount = &amount },
		"a negative count":    func(f *Fact) { negative := int64(-5); f.ProviderInputTokens = &negative },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Interpret(withUsage(t, mutate))
			if !errors.Is(err, ErrIncoherentFact) || !Quarantinable(err) {
				t.Fatalf("released fact with %s: error = %v, want quarantinable ErrIncoherentFact", name, err)
			}
		})
	}
}

func TestTheOrphanPairingRules(t *testing.T) {
	// The orphan names who observed what; it prices nothing. A pricing
	// column on an orphan is a charge smuggled through a fact that derives
	// none, and the missing provenance columns are a telemetry fact with no
	// telemetry.
	capture := "reported"
	attempt := "att-3"
	revision := "rev-1"
	price := int64(200)
	amount := int64(700)
	base := func(t *testing.T) Fact {
		t.Helper()
		return Fact{
			AppendSeq:          12,
			RequestID:          "d9000000-0000-7000-8000-0000000000d5",
			Kind:               KindUnbillableOrphaned,
			SchemaVersion:      SchemaVersion,
			Payload:            envelope(t),
			CaptureMethod:      &capture,
			CommittedAttemptID: &attempt,
		}
	}
	for name, mutate := range map[string]func(*Fact){
		"no capture method":    func(f *Fact) { f.CaptureMethod = nil },
		"an unknown capture":   func(f *Fact) { *f.CaptureMethod = "guessed" },
		"no committed attempt": func(f *Fact) { f.CommittedAttemptID = nil },
		"a price revision":     func(f *Fact) { f.PriceRevisionID = &revision },
		"a unit price":         func(f *Fact) { f.InputUnitPrice = &price },
		"a settled amount":     func(f *Fact) { f.SettledAmount = &amount },
		"a negative count":     func(f *Fact) { negative := int64(-2); f.ProviderOutputTokens = &negative },
	} {
		t.Run(name, func(t *testing.T) {
			fact := base(t)
			mutate(&fact)
			_, err := Interpret(fact)
			if !errors.Is(err, ErrIncoherentFact) || !Quarantinable(err) {
				t.Fatalf("orphan with %s: error = %v, want quarantinable ErrIncoherentFact", name, err)
			}
		})
	}
}

func TestQuarantinableCoversTheRecordedVocabulary(t *testing.T) {
	// The disposition split in one place: every sentinel the consumer
	// records verbatim is quarantinable, ErrPayloadOversize is not, and a
	// foreign error (a store failure) never is — a database outage is a
	// page stop, not a disposition about a fact.
	for _, err := range []error{
		ErrUnknownKind,
		ErrUnknownSchemaVersion,
		ErrMalformedFact,
		ErrMalformedPayload,
		ErrCorrectionUnsupported,
		ErrIncoherentFact,
	} {
		if !Quarantinable(err) {
			t.Errorf("%v should be quarantinable", err)
		}
	}
	if Quarantinable(ErrPayloadOversize) {
		t.Error("ErrPayloadOversize stops the page; it is not a recorded disposition")
	}
	if Quarantinable(errors.New("store: connection refused")) {
		t.Error("a foreign error is a page stop, never a disposition")
	}
}

func TestEveryKindClassOfKnowsTheSwitchNames(t *testing.T) {
	// The dispatch switch names its kinds outright and refuses the default,
	// so a kind ClassOf learns without the switch learning it arrives as a
	// recorded unknown kind instead of a silent fall-through. The proof is
	// per kind: the minimal coherent fact of each kind interprets with no
	// unknown-kind refusal and derives exactly its own class.
	build := map[string]func(t *testing.T) Fact{
		KindSettled: func(t *testing.T) Fact {
			return settledFact(t, wireLeg{FundingBucketID: bucketA, Amount: 700, Ordinal: 1})
		},
		KindReleased: noUsageFact(KindReleased),
		KindExpired:  noUsageFact(KindExpired),
		KindUnbillableOrphaned: func(t *testing.T) Fact {
			capture := "reported"
			attempt := "att-4"
			return Fact{
				AppendSeq:          14,
				RequestID:          "d9000000-0000-7000-8000-0000000000d7",
				Kind:               KindUnbillableOrphaned,
				SchemaVersion:      SchemaVersion,
				Payload:            envelope(t),
				CaptureMethod:      &capture,
				CommittedAttemptID: &attempt,
			}
		},
	}
	for kind, makeFact := range build {
		class, known := ClassOf(kind)
		if !known {
			t.Fatalf("ClassOf(%s) = unknown, want the kind the test builds a fact for", kind)
		}
		out, err := Interpret(makeFact(t))
		if errors.Is(err, ErrUnknownKind) {
			t.Fatalf("Interpret(%s) = %v, want the kind dispatched, not refused", kind, err)
		}
		if err != nil {
			t.Fatalf("Interpret(%s) error = %v, want nil", kind, err)
		}
		if out.Kind != kind || out.Class != class {
			t.Errorf("Interpret(%s) = (%s, %s), want the kind and its class %s", kind, out.Kind, out.Class, class)
		}
	}
}

// noUsageFact builds the minimal coherent released or expired fact: no
// usage columns, a one-leg tail.
func noUsageFact(kind string) func(t *testing.T) Fact {
	return func(t *testing.T) Fact {
		t.Helper()
		return Fact{
			AppendSeq:     13,
			RequestID:     "d9000000-0000-7000-8000-0000000000d6",
			Kind:          kind,
			SchemaVersion: SchemaVersion,
			Payload:       envelope(t, wireLeg{FundingBucketID: bucketA, Amount: 700, Ordinal: 1}),
		}
	}
}
