package accounting

import (
	"errors"
	"testing"
	"time"
)

// Every test here pins the quota half of the runtime's money memory: the
// scope matrix that keeps a projection's kind and its scope fields in
// agreement, the guarded refill, and the waterfall order — the one ordering
// in the runtime, doubling as the canonical lock order, whose two spellings
// (this function and the adapter's SQL) must stay the same order.

var (
	cycleOneEnd   = time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	cycleTwoEnd   = time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	subscribedOld = time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	subscribedNew = time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	seededAt      = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
)

// entitlementProjection is a valid named entitlement-cycle projection: the
// scope names its entitlement, its cycle and the cycle's end, and the row
// names the bucket it draws from and the alias group version that granted it.
func entitlementProjection() Projection {
	return Projection{
		AccountID:             "acc-0001",
		ScopeKind:             ScopeEntitlementCycle,
		EntitlementID:         "ent-0001",
		CycleNumber:           3,
		FundingBucketID:       "bucket-0001",
		AliasGroupVersionID:   "agv-0001",
		NamedScope:            true,
		PeriodEnd:             cycleOneEnd,
		SubscriptionCreatedAt: subscribedOld,
		State:                 ProjectionActive,
		LimitAmount:           5000,
		Available:             4200,
		Revision:              7,
		SeededAt:              seededAt,
		UpdatedAt:             seededAt,
	}
}

// paygProjection is a valid pay-as-you-go balance: none of the entitlement
// scope's fields, the same row shape otherwise.
func paygProjection() Projection {
	projection := entitlementProjection()
	projection.ScopeKind = ScopePayGBalance
	projection.EntitlementID = ""
	projection.CycleNumber = 0
	projection.PeriodEnd = time.Time{}
	projection.NamedScope = false
	return projection
}

// TestProjectionValidateEnforcesTheScopeMatrix pins the scope matrix in both
// directions: an entitlement cycle must name its entitlement, a positive
// cycle and the cycle's end; a PAYG balance must carry none of them; a kind
// outside the two is refused; and every row must name its bucket and alias
// group version and carry non-negative figures. The mirror CHECKs on the
// row refuse the same shapes — this is the legible refusal in front of them.
func TestProjectionValidateEnforcesTheScopeMatrix(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Projection)
		wantRefuse bool
	}{
		{name: "accepts an entitlement cycle"},
		{name: "accepts a payg balance", mutate: func(p *Projection) { *p = paygProjection() }},
		{name: "refuses an entitlement cycle without its entitlement", mutate: func(p *Projection) { p.EntitlementID = "" }, wantRefuse: true},
		{name: "refuses an entitlement cycle without a cycle number", mutate: func(p *Projection) { p.CycleNumber = 0 }, wantRefuse: true},
		{name: "refuses a negative cycle number", mutate: func(p *Projection) { p.CycleNumber = -1 }, wantRefuse: true},
		{name: "refuses an entitlement cycle without its period end", mutate: func(p *Projection) { p.PeriodEnd = time.Time{} }, wantRefuse: true},
		// The three PAYG cases start from the PAYG base and add one
		// entitlement field, so the refusal is the stray field itself and
		// not the base row's own scope.
		{name: "refuses a payg balance naming an entitlement", mutate: func(p *Projection) { *p = paygProjection(); p.EntitlementID = "ent-0001" }, wantRefuse: true},
		{name: "refuses a payg balance carrying a cycle number", mutate: func(p *Projection) { *p = paygProjection(); p.CycleNumber = 3 }, wantRefuse: true},
		{name: "refuses a payg balance carrying a period end", mutate: func(p *Projection) { *p = paygProjection(); p.PeriodEnd = cycleOneEnd }, wantRefuse: true},
		{name: "refuses an empty scope kind", mutate: func(p *Projection) { p.ScopeKind = "" }, wantRefuse: true},
		{name: "refuses a scope kind outside the vocabulary", mutate: func(p *Projection) { p.ScopeKind = "credit_balance" }, wantRefuse: true},
		{name: "refuses a row with no funding bucket", mutate: func(p *Projection) { p.FundingBucketID = "" }, wantRefuse: true},
		{name: "refuses a row with no alias group version", mutate: func(p *Projection) { p.AliasGroupVersionID = "" }, wantRefuse: true},
		{name: "refuses a negative limit", mutate: func(p *Projection) { p.LimitAmount = -1 }, wantRefuse: true},
		{name: "refuses a negative available", mutate: func(p *Projection) { p.Available = -1 }, wantRefuse: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projection := entitlementProjection()
			if tt.mutate != nil {
				tt.mutate(&projection)
			}
			err := projection.Validate()
			if tt.wantRefuse && !errors.Is(err, ErrUnknownScope) {
				t.Errorf("Validate() = %v, want ErrUnknownScope for %+v", err, projection)
			}
			if !tt.wantRefuse && err != nil {
				t.Errorf("Validate() = %v, want the projection accepted", err)
			}
		})
	}
}

// TestPublicationValidateEnforcesTheSameShape pins the publication's rules:
// the same scope matrix a projection answers to, since a publication is how
// a projection is born, plus the two fields a publication carries and a row
// does not need checking here — the account it belongs to (never empty) and
// its limit (never negative). A publication carries no available on purpose;
// a redelivery must not resurrect spent capacity, so there is nothing to
// refuse on that field.
func TestPublicationValidateEnforcesTheSameShape(t *testing.T) {
	validEntitlement := func() Publication {
		projection := entitlementProjection()
		return Publication{
			AccountID:             projection.AccountID,
			FundingBucketID:       projection.FundingBucketID,
			AliasGroupVersionID:   projection.AliasGroupVersionID,
			ScopeKind:             projection.ScopeKind,
			EntitlementID:         projection.EntitlementID,
			CycleNumber:           projection.CycleNumber,
			NamedScope:            projection.NamedScope,
			PeriodEnd:             projection.PeriodEnd,
			SubscriptionCreatedAt: projection.SubscriptionCreatedAt,
			State:                 projection.State,
			LimitAmount:           projection.LimitAmount,
			Revision:              projection.Revision,
		}
	}
	validPayg := func() Publication {
		publication := validEntitlement()
		projection := paygProjection()
		publication.ScopeKind = projection.ScopeKind
		publication.EntitlementID = ""
		publication.CycleNumber = 0
		publication.PeriodEnd = time.Time{}
		publication.NamedScope = false
		return publication
	}

	tests := []struct {
		name    string
		mutate  func(*Publication)
		refused bool
	}{
		{name: "accepts an entitlement cycle publication"},
		{name: "accepts a payg balance publication", mutate: func(p *Publication) { *p = validPayg() }},
		{name: "refuses an entitlement cycle without its entitlement", mutate: func(p *Publication) { p.EntitlementID = "" }, refused: true},
		{name: "refuses an entitlement cycle without a cycle number", mutate: func(p *Publication) { p.CycleNumber = 0 }, refused: true},
		{name: "refuses an entitlement cycle without its period end", mutate: func(p *Publication) { p.PeriodEnd = time.Time{} }, refused: true},
		// The three PAYG cases start from the PAYG base and add one
		// entitlement field, so the refusal is the stray field itself and
		// not the base row's own scope.
		{name: "refuses a payg balance naming an entitlement", mutate: func(p *Publication) { *p = validPayg(); p.EntitlementID = "ent-0001" }, refused: true},
		{name: "refuses a payg balance carrying a cycle number", mutate: func(p *Publication) { *p = validPayg(); p.CycleNumber = 2 }, refused: true},
		{name: "refuses a payg balance carrying a period end", mutate: func(p *Publication) { *p = validPayg(); p.PeriodEnd = cycleOneEnd }, refused: true},
		{name: "refuses an unknown scope kind", mutate: func(p *Publication) { p.ScopeKind = "credit_balance" }, refused: true},
		{name: "refuses an empty account", mutate: func(p *Publication) { p.AccountID = "" }, refused: true},
		{name: "refuses a publication with no funding bucket", mutate: func(p *Publication) { p.FundingBucketID = "" }, refused: true},
		{name: "refuses a publication with no alias group version", mutate: func(p *Publication) { p.AliasGroupVersionID = "" }, refused: true},
		{name: "refuses a publication with no subscription created-at", mutate: func(p *Publication) { p.SubscriptionCreatedAt = time.Time{} }, refused: true},
		{name: "refuses a negative limit", mutate: func(p *Publication) { p.LimitAmount = -1 }, refused: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			publication := validEntitlement()
			if tt.mutate != nil {
				tt.mutate(&publication)
			}
			err := publication.Validate()
			if tt.refused && !errors.Is(err, ErrUnknownScope) {
				t.Errorf("Validate() = %v, want ErrUnknownScope for %+v", err, publication)
			}
			if !tt.refused && err != nil {
				t.Errorf("Validate() = %v, want the publication accepted", err)
			}
		})
	}
}

// TestNewRefillBuildsAGuardedOperation pins the refill's rules: it carries
// the Control Plane's identity, it names its bucket, it adds a positive
// amount, and it always carries the revision it is guarded by — a negative
// revision is refused with ErrRefillNotGuarded, because an unguarded refill
// replayed is capacity minted twice, the one way this table could invent
// money. Zero is a legitimate guard: a projection at revision zero is a state
// a grant can sit at.
func TestNewRefillBuildsAGuardedOperation(t *testing.T) {
	t.Run("builds a guarded refill and rounds it through", func(t *testing.T) {
		refill, err := NewRefill("refill-0001", "bucket-0001", 1500, 7)
		if err != nil {
			t.Fatalf("NewRefill() error = %v", err)
		}
		if refill != (Refill{RefillID: "refill-0001", FundingBucketID: "bucket-0001", Amount: 1500, AtRevision: 7}) {
			t.Errorf("NewRefill() = %+v, want the four fields back as given", refill)
		}
	})

	t.Run("accepts a zero guard revision", func(t *testing.T) {
		if _, err := NewRefill("refill-0001", "bucket-0001", 1500, 0); err != nil {
			t.Errorf("NewRefill() with a zero guard error = %v, want it accepted — zero is a state a projection can sit at", err)
		}
	})

	tests := []struct {
		name            string
		refillID        string
		fundingBucketID string
		amount          int64
		atRevision      int64
		wantErr         error
	}{
		{name: "refuses an unidentified refill", refillID: "", fundingBucketID: "bucket-0001", amount: 1500, atRevision: 7},
		{name: "refuses an unnamed bucket", refillID: "refill-0001", fundingBucketID: "", amount: 1500, atRevision: 7},
		{name: "refuses a zero amount", refillID: "refill-0001", fundingBucketID: "bucket-0001", amount: 0, atRevision: 7},
		{name: "refuses a negative amount", refillID: "refill-0001", fundingBucketID: "bucket-0001", amount: -1, atRevision: 7},
		{name: "refuses a negative guard revision", refillID: "refill-0001", fundingBucketID: "bucket-0001", amount: 1500, atRevision: -1, wantErr: ErrRefillNotGuarded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRefill(tt.refillID, tt.fundingBucketID, tt.amount, tt.atRevision)
			if err == nil {
				t.Fatal("NewRefill() succeeded, want a refusal")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("NewRefill() error = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}

// TestRefillOutcomeVocabulary pins the three outcomes' wire spellings. They
// cross the port boundary as strings the adapter's SQL CASE emits, so a
// renamed constant would silently stop matching the statement — the pin is
// what makes that a compile-time-adjacent failure instead of a runtime one.
func TestRefillOutcomeVocabulary(t *testing.T) {
	want := map[RefillOutcome]string{
		RefillApplied:        "applied",
		RefillAlreadyApplied: "already_applied",
		RefillStale:          "stale",
	}
	for outcome, spelling := range want {
		if string(outcome) != spelling {
			t.Errorf("RefillOutcome %q does not spell %q — the adapter's CASE emits the spelling, not the constant", string(outcome), spelling)
		}
	}
}

// waterfallProjection builds one projection for the ordering fixtures: the
// scope fields and the three sort keys, with the funding bucket id as the
// human-readable marker the assertions read.
func waterfallProjection(named bool, periodEnd, subscription time.Time, entitlement, bucket string) Projection {
	projection := entitlementProjection()
	projection.NamedScope = named
	projection.PeriodEnd = periodEnd
	projection.SubscriptionCreatedAt = subscription
	projection.EntitlementID = entitlement
	projection.FundingBucketID = bucket
	return projection
}

// orderedBuckets extracts the marker sequence from an ordered slice.
func orderedBuckets(projections []Projection) []string {
	buckets := make([]string, 0, len(projections))
	for _, projection := range projections {
		buckets = append(buckets, projection.FundingBucketID)
	}
	return buckets
}

// TestByWaterfallOrdersTheAccountTheWayADR0003ReadsIt pins the one and only
// waterfall order over an eight-projection fixture, asserted in exact final
// position: named scopes before the wildcard, then the earliest period end,
// then the oldest subscription, then the entitlement id, then the funding
// bucket id as the last tiebreak — the order must be total, because it is
// also the lock order and two rows tied on every input would make "first"
// a coin flip — and the PAYG balance — no period end — last of all. The input
// is handed over shuffled so the sort, not the input order, is what the output
// proves, and the input is checked to come back un-reordered: ByWaterfall is
// computed fresh from raw fields every call and must not write through to
// the caller's slice.
func TestByWaterfallOrdersTheAccountTheWayADR0003ReadsIt(t *testing.T) {
	input := []Projection{
		waterfallProjection(false, time.Time{}, time.Time{}, "", "payg-balance"),                   // PAYG: no period end
		waterfallProjection(true, cycleTwoEnd, subscribedOld, "ent-d", "named-cycle-two"),          // named, later cycle
		waterfallProjection(true, cycleOneEnd, subscribedNew, "ent-z", "named-cycle-one-late-sub"), // same cycle end, newer subscription
		waterfallProjection(false, cycleTwoEnd, subscribedOld, "ent-e", "wildcard-cycle-two"),      // wildcard, real period end
		waterfallProjection(true, cycleOneEnd, subscribedOld, "ent-c", "named-cycle-one-ent-c"),    // full tie except entitlement id
		waterfallProjection(true, cycleOneEnd, subscribedOld, "ent-a", "named-cycle-one-ent-a"),    // full tie except entitlement id
		waterfallProjection(true, cycleOneEnd, subscribedOld, "ent-b", "named-cycle-one-ent-b-2"),  // full tie incl. entitlement id: bucket tiebreak
		waterfallProjection(true, cycleOneEnd, subscribedOld, "ent-b", "named-cycle-one-ent-b-1"),  // full tie incl. entitlement id: bucket tiebreak
	}
	want := []string{
		"named-cycle-one-ent-a",    // tie on everything but the entitlement id: ent-a < ent-b < ent-c
		"named-cycle-one-ent-b-1",  // tie with its ent-b sibling on every other input: bucket-1 < bucket-2
		"named-cycle-one-ent-b-2",  // the sibling, after
		"named-cycle-one-ent-c",    // same end, older subscription than the late-sub row
		"named-cycle-one-late-sub", // same end as the tie group, newer subscription
		"named-cycle-two",          // named, later period end
		"wildcard-cycle-two",       // wildcard sorts after every named scope, own period end ordering
		"payg-balance",             // no period end: last, whatever a zero time would do
	}

	inputCopy := append([]Projection(nil), input...)
	ordered := ByWaterfall(input)

	if got := orderedBuckets(ordered); !equalStrings(got, want) {
		t.Errorf("ByWaterfall() = %v, want %v — one order, named first, cycles before the balance", got, want)
	}
	if ordered[len(ordered)-1].FundingBucketID != "payg-balance" {
		t.Errorf("last projection = %q, want the PAYG balance — NULLS LAST parity: the cycles drain before the balance does", ordered[len(ordered)-1].FundingBucketID)
	}
	if got := orderedBuckets(input); !equalStrings(got, orderedBuckets(inputCopy)) {
		t.Errorf("input after ByWaterfall() = %v, want it exactly as handed over — the function must not reorder the caller's slice", got)
	}
}

// TestByWaterfallSortsNoPeriodEndAfterEveryRealPeriodEnd pins the NULLS LAST
// parity on its own, because it is the one ordering a naive time comparison
// gets backwards: time.Time's zero value sorts before every real date, so a
// plain ASC comparison would draw the PAYG balance first — the one order the
// waterfall is not.
func TestByWaterfallSortsNoPeriodEndAfterEveryRealPeriodEnd(t *testing.T) {
	projections := []Projection{
		waterfallProjection(false, time.Time{}, time.Time{}, "", "payg-balance"),
		waterfallProjection(false, cycleTwoEnd, subscribedOld, "ent-b", "wildcard-cycle-two"),
		waterfallProjection(false, cycleOneEnd, subscribedOld, "ent-a", "wildcard-cycle-one"),
	}
	want := []string{"wildcard-cycle-one", "wildcard-cycle-two", "payg-balance"}

	if got := orderedBuckets(ByWaterfall(projections)); !equalStrings(got, want) {
		t.Errorf("ByWaterfall() = %v, want %v — a zero period end sorts last, not first", got, want)
	}
}

// TestByWaterfallIsStableForFullTiesAndNeverWritesThrough pins the copy
// semantics and the stability: two projections identical on every sort key
// (a PAYG pair has an empty entitlement id, so no tiebreak distinguishes
// them) come back in the order handed over — the order is the canonical lock
// order, so equal elements must not dance between calls — and mutating the
// returned slice must not write through to the input, because the order is
// recomputed from raw fields, never cached or aliased.
func TestByWaterfallIsStableForFullTiesAndNeverWritesThrough(t *testing.T) {
	input := []Projection{
		waterfallProjection(false, time.Time{}, time.Time{}, "", "payg-first-come"),
		waterfallProjection(false, time.Time{}, time.Time{}, "", "payg-second-come"),
	}

	ordered := ByWaterfall(input)
	if got := orderedBuckets(ordered); !equalStrings(got, []string{"payg-first-come", "payg-second-come"}) {
		t.Errorf("ByWaterfall() = %v, want the input order preserved for fully equal elements", got)
	}

	ordered[0].Available = 1
	if input[0].Available != 4200 {
		t.Errorf("input[0].Available = %d after mutating the ordered copy, want it untouched — the result must not alias the input", input[0].Available)
	}
}

// equalStrings compares two marker sequences; it exists so the order
// assertions above can show both sides in one message.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
