package commerce

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

// scopeVersionOf mints the catalog version id a roll would have pinned for
// a definition: deterministic per definition id (so assertions can compare
// across calls) and always in the v7 form the entitlement constructor
// validates.
func scopeVersionOf(defID string) AliasGroupVersionID {
	sum := sha256.Sum256([]byte(defID))
	return AliasGroupVersionID(fmt.Sprintf("0198f0a4-3f6c-7000-8000-%x", sum[:6]))
}

// The waterfall suite replays ADR 0003's worked example and then attacks
// each of the four keys at its exact tie. The construction helpers keep to
// real transitions: every entitlement is minted the way a roll would.

func derivationSubscription(t *testing.T, id string, account AccountID, anchor time.Time, createdAt time.Time) *Subscription {
	t.Helper()
	s, err := NewSubscription(SubscriptionID(id), account, "v-1", anchor, true, createdAt)
	if err != nil {
		t.Fatalf("NewSubscription(%s) returned error: %v", id, err)
	}
	return s
}

func derivationEntitlement(t *testing.T, s *Subscription, defID string, scope AliasGroupName, amount int64, cycle int) *Entitlement {
	t.Helper()
	definition, err := NewGrantDefinition(GrantDefinitionID(defID), s.PlanVersionID, scope, DimensionCost, amount)
	if err != nil {
		t.Fatalf("NewGrantDefinition(%s) returned error: %v", defID, err)
	}
	ps, pe, err := CycleBounds(s.StartAt, cycle)
	if err != nil {
		t.Fatalf("CycleBounds(%d) returned error: %v", cycle, err)
	}
	e, err := NewEntitlement(EntitlementID(defID+"-ent"), s.ID, cycle, *definition, scopeVersionOf(defID), ps, pe, createdAtOf(s))
	if err != nil {
		t.Fatalf("NewEntitlement(%s) returned error: %v", defID, err)
	}
	return e
}

func createdAtOf(s *Subscription) time.Time { return s.CreatedAt }

func candidate(e *Entitlement, s *Subscription) CandidateGrant {
	return CandidateGrant{Entitlement: e, SubscriptionCreatedAt: s.CreatedAt, AliasGroupName: AliasGroupName("anthropic")}
}

// TestWaterfallWorkedExample is ADR 0003's allocation example: at Sep 10,
// S1 (anthropic, ends Sep 15, grants 20), S2 (anthropic, ends Oct 1, grants
// 45) and S3 (wildcard, ends Oct 5, grants 80) all cover, and the order —
// named-before-wildcard, then soonest end — is the one 60 units walk: 20 to
// S1, 40 to S2, nothing to S3.
func TestWaterfallWorkedExample(t *testing.T) {
	aug15 := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	sep1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	sep5 := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)

	s1 := derivationSubscription(t, "s1", "acc-1", aug15, aug15)
	s2 := derivationSubscription(t, "s2", "acc-1", sep1, sep1)
	s3 := derivationSubscription(t, "s3", "acc-1", sep5, sep5)

	e1 := derivationEntitlement(t, s1, "d1", "anthropic", 20, 1)
	e2 := derivationEntitlement(t, s2, "d2", "anthropic", 45, 1)
	e3 := derivationEntitlement(t, s3, "d3", WildcardGroupName, 80, 1)

	// The wildcard candidate carries the wildcard NAME — the repository
	// joins it from the grant definition.
	c3 := candidate(e3, s3)
	c3.AliasGroupName = WildcardGroupName

	got := EffectiveEntitlements([]CandidateGrant{c3, candidate(e2, s2), candidate(e1, s1)}, at)
	if len(got) != 3 {
		t.Fatalf("derivation returned %d entitlements, want 3", len(got))
	}
	wantOrder := []EntitlementID{e1.ID, e2.ID, e3.ID}
	for i, want := range wantOrder {
		if got[i].EntitlementID != want {
			t.Fatalf("position %d = %s, want %s (full order: %s, %s, %s)",
				i, got[i].EntitlementID, want, got[0].EntitlementID, got[1].EntitlementID, got[2].EntitlementID)
		}
	}
	if got[0].GrantedAmount != 20 || got[1].GrantedAmount != 45 || got[2].GrantedAmount != 80 {
		t.Fatalf("amounts drifted: %d, %d, %d", got[0].GrantedAmount, got[1].GrantedAmount, got[2].GrantedAmount)
	}
	if got[2].IsWildcard != true || got[0].IsWildcard != false {
		t.Fatalf("wildcard flags drifted: %v, %v, %v", got[0].IsWildcard, got[1].IsWildcard, got[2].IsWildcard)
	}
	if got[0].AliasGroupName != "anthropic" || got[2].AliasGroupName != WildcardGroupName {
		t.Fatalf("scope names drifted: %q, %q", got[0].AliasGroupName, got[2].AliasGroupName)
	}
}

// Specificity beats expiry: a wildcard whose cycle ends sooner is still
// consumed after a named grant whose cycle ends later.
func TestSpecificityBeatsExpiry(t *testing.T) {
	sep1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)

	namedSub := derivationSubscription(t, "sn", "acc-1", sep1, sep1)
	wildSub := derivationSubscription(t, "sw", "acc-1", sep1, sep1)
	named := derivationEntitlement(t, namedSub, "dn", "anthropic", 20, 1) // [Sep1, Oct1)
	wild := derivationEntitlement(t, wildSub, "dw", WildcardGroupName, 80, 1)

	// Shrink the wildcard's stored cycle so its end comes first — the roll
	// cannot produce this pair from one anchor, and the derivation must not
	// care.
	wild.PeriodEnd = named.PeriodEnd.Add(-24 * time.Hour)

	cw := candidate(wild, wildSub)
	cw.AliasGroupName = WildcardGroupName
	got := EffectiveEntitlements([]CandidateGrant{cw, candidate(named, namedSub)}, at)
	if len(got) != 2 || got[0].EntitlementID != named.ID || got[1].EntitlementID != wild.ID {
		t.Fatalf("order = [%s, %s], want named [%s] before wildcard [%s]",
			got[0].EntitlementID, got[1].EntitlementID, named.ID, wild.ID)
	}
}

func TestWaterfallTieBreaks(t *testing.T) {
	sep1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	aug1 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)

	t.Run("soonest period_end wins between equal scopes", func(t *testing.T) {
		// Anchored Aug 15, the cycle ends Sep 15; anchored Sep 1, Oct 1 —
		// both cover Sep 10, so key 2 decides.
		soonSub := derivationSubscription(t, "ss", "acc-1", aug15(), aug15())
		farSub := derivationSubscription(t, "sf", "acc-1", sep1, sep1)
		soon := derivationEntitlement(t, soonSub, "ds", "anthropic", 20, 1) // ends Sep 15
		far := derivationEntitlement(t, farSub, "df", "anthropic", 20, 1)   // ends Oct 1

		got := EffectiveEntitlements([]CandidateGrant{candidate(far, farSub), candidate(soon, soonSub)}, at)
		if got[0].EntitlementID != soon.ID {
			t.Fatalf("first = %s, want the sooner-ending %s", got[0].EntitlementID, soon.ID)
		}
	})
	t.Run("oldest subscription wins when ends tie", func(t *testing.T) {
		oldSub := derivationSubscription(t, "so", "acc-1", sep1, aug1)
		newSub := derivationSubscription(t, "snew", "acc-1", sep1, sep1)
		oldE := derivationEntitlement(t, oldSub, "do", "anthropic", 20, 1)
		newE := derivationEntitlement(t, newSub, "dnew", "anthropic", 20, 1)

		got := EffectiveEntitlements([]CandidateGrant{candidate(newE, newSub), candidate(oldE, oldSub)}, at)
		if got[0].EntitlementID != oldE.ID {
			t.Fatalf("first = %s, want the older subscription's %s", got[0].EntitlementID, oldE.ID)
		}
		if got[0].SourceSubscriptionCreatedAt != aug1 {
			t.Fatalf("carried created_at = %v, want %v", got[0].SourceSubscriptionCreatedAt, aug1)
		}
	})
	t.Run("entitlement id is the final, total tie-break", func(t *testing.T) {
		subA := derivationSubscription(t, "sa", "acc-1", sep1, aug1)
		subB := derivationSubscription(t, "sb", "acc-1", sep1, aug1)
		eA := derivationEntitlement(t, subA, "da", "anthropic", 20, 1)
		eB := derivationEntitlement(t, subB, "db", "anthropic", 20, 1)

		got := EffectiveEntitlements([]CandidateGrant{candidate(eB, subB), candidate(eA, subA)}, at)
		first, second := got[0].EntitlementID, got[1].EntitlementID
		if first >= second {
			t.Fatalf("order = [%s, %s], want ascending id", first, second)
		}
	})
}

func aug15() time.Time { return time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC) }

func TestDerivationFiltersCoverage(t *testing.T) {
	sep1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	oct1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)

	liveSub := derivationSubscription(t, "sl", "acc-1", sep1, sep1)
	live := derivationEntitlement(t, liveSub, "dl", "anthropic", 20, 1)

	deadSub := derivationSubscription(t, "sd", "acc-1", sep1, sep1)
	dead := derivationEntitlement(t, deadSub, "dd", "anthropic", 20, 1)
	if err := dead.Expire(oct1); err != nil {
		t.Fatalf("Expire returned error: %v", err)
	}

	got := EffectiveEntitlements([]CandidateGrant{candidate(dead, deadSub), candidate(live, liveSub),
		{Entitlement: nil, SubscriptionCreatedAt: sep1, AliasGroupName: "anthropic"}}, at)
	if len(got) != 1 || got[0].EntitlementID != live.ID {
		t.Fatalf("derivation kept %d entitlements, want exactly the live one", len(got))
	}
	t.Run("the coverage boundary is the same half-open rule", func(t *testing.T) {
		if got := EffectiveEntitlements([]CandidateGrant{candidate(live, liveSub)}, oct1); len(got) != 0 {
			t.Fatal("an entitlement covers at exactly its period_end")
		}
	})
}

func TestEffectiveForAliasAppliesMembershipFirst(t *testing.T) {
	sep1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)

	anthropicSub := derivationSubscription(t, "sa", "acc-1", sep1, sep1)
	anthropic := derivationEntitlement(t, anthropicSub, "da", "anthropic", 45, 1)
	wildSub := derivationSubscription(t, "sw", "acc-1", sep1, sep1)
	wild := derivationEntitlement(t, wildSub, "dw", WildcardGroupName, 80, 1)

	candidates := []CandidateGrant{candidate(anthropic, anthropicSub), func() CandidateGrant {
		c := candidate(wild, wildSub)
		c.AliasGroupName = WildcardGroupName
		return c
	}()}

	t.Run("a known alias is served by its named grant first, wildcard second", func(t *testing.T) {
		membership := func(v AliasGroupVersionID) bool { return true }
		got := EffectiveForAlias(candidates, membership, at)
		if len(got) != 2 || got[0].EntitlementID != anthropic.ID || got[1].EntitlementID != wild.ID {
			t.Fatalf("order = %v, want anthropic then wildcard", got)
		}
	})
	t.Run("an unknown alias falls through to the wildcard alone", func(t *testing.T) {
		anthropicVersion := anthropic.AliasGroupVersionID
		membership := func(v AliasGroupVersionID) bool { return v != anthropicVersion }
		got := EffectiveForAlias(candidates, membership, at)
		if len(got) != 1 || got[0].EntitlementID != wild.ID || !got[0].IsWildcard {
			t.Fatalf("fallthrough = %v, want the wildcard alone", got)
		}
	})
	t.Run("an alias no version contains is served by nothing", func(t *testing.T) {
		membership := func(AliasGroupVersionID) bool { return false }
		if got := EffectiveForAlias(candidates, membership, at); len(got) != 0 {
			t.Fatalf("membership-first filtering kept %d entitlements, want none", len(got))
		}
	})
}
