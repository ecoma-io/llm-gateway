package commerce

import (
	"sort"
	"time"
)

// The entitlement derivation: the deterministic application-level view over
// an account's active subscriptions and their entitlements at one instant —
// the canonical answer to "what may this account consume, from which grant,
// until when" that the quota projection and admission consume downstream.
// It is a reading, never a state: nothing here writes, and the output says
// nothing about what has been consumed — that is the quota projection's
// business, and the ledger's, not the entitlement's.
//
// What the domain can and cannot see: catalog membership lives in the Data
// Plane's database, so "does this scope contain alias a" is not answerable
// here. The seam is a caller-supplied membership predicate — the
// projection's own snapshot answers it, in its own database, and the
// waterfall's order is exactly the same either way.

// CandidateGrant is one derivation input: an entitlement plus the two facts
// the waterfall reads from neighbouring rows — the subscription's creation
// instant (the third tie-break, never normalised to start_at; the two
// diverge the first time a cycle rolls) and the grant definition's scope
// NAME, which is both the specificity key and the seam the membership
// predicate closes over. The repository query feeds these joined; the
// subscription-active and period-coverage filters are the query's, and the
// derivation re-checks coverage defensively because a total function costs
// nothing and a wrong answer here is money.
type CandidateGrant struct {
	Entitlement           *Entitlement
	SubscriptionCreatedAt time.Time
	AliasGroupName        AliasGroupName
}

// EffectiveEntitlement is the derivation's output unit — the canonical
// representation of one grant's contribution to what an account may
// consume. It is the projection seed's vocabulary: every field the runtime
// needs to rank, match and enforce is here, and nothing about quota state.
type EffectiveEntitlement struct {
	EntitlementID       EntitlementID
	SubscriptionID      SubscriptionID
	CycleNumber         int
	AliasGroupVersionID AliasGroupVersionID
	// The scope NAME the grant definition carried and the version the roll
	// pinned — the name ranks specificity, the version answers membership.
	AliasGroupName AliasGroupName
	IsWildcard     bool
	Dimension      Dimension
	GrantedAmount  int64
	PeriodStart    time.Time
	PeriodEnd      time.Time
	// The third tie-break's input, carried verbatim from the subscription.
	SourceSubscriptionCreatedAt time.Time
}

// MembershipOfAlias is the derivation's injected seam: whether one
// alias-group version contains the alias in question. The projection phase
// supplies it from the catalog snapshot; in tests it is a table. It is a
// predicate, not a lookup, because the waterfall needs the answer and
// nothing else about the group.
type MembershipOfAlias func(versionID AliasGroupVersionID) bool

// EffectiveEntitlements derives the account's effective entitlements at at:
// the candidates whose cycle covers the instant, ordered by the single
// allocation waterfall ADR 0003 fixes and no other:
//
//  1. scope specificity — a named group before the wildcard, so the
//     wildcard survives for aliases without dedicated entitlement;
//  2. soonest period_end — capacity about to expire is spent first;
//  3. oldest subscription — ties broken by commercial seniority;
//  4. entitlement id ascending — the total, replay-stable last word.
//
// The order is total: no two candidates can compare equal, because no two
// entitlements share an id. Every call site that consumes grants in a
// sequence — this derivation, the projection's ranking, any future
// settlement leg ordering — must use this one function and add no
// "reasonable" order of its own.
func EffectiveEntitlements(candidates []CandidateGrant, at time.Time) []EffectiveEntitlement {
	effective := make([]EffectiveEntitlement, 0, len(candidates))
	for _, c := range candidates {
		if c.Entitlement == nil || !c.Entitlement.Covers(at) {
			continue
		}
		effective = append(effective, EffectiveEntitlement{
			EntitlementID:               c.Entitlement.ID,
			SubscriptionID:              c.Entitlement.SubscriptionID,
			CycleNumber:                 c.Entitlement.CycleNumber,
			AliasGroupVersionID:         c.Entitlement.AliasGroupVersionID,
			AliasGroupName:              c.AliasGroupName,
			IsWildcard:                  c.AliasGroupName == WildcardGroupName,
			Dimension:                   c.Entitlement.Dimension,
			GrantedAmount:               c.Entitlement.GrantedAmount,
			PeriodStart:                 c.Entitlement.PeriodStart,
			PeriodEnd:                   c.Entitlement.PeriodEnd,
			SourceSubscriptionCreatedAt: c.SubscriptionCreatedAt,
		})
	}
	sort.SliceStable(effective, func(i, j int) bool {
		return waterfallLess(effective[i], effective[j])
	})
	return effective
}

// EffectiveForAlias derives the entitlements that can serve one alias at
// at: the same derivation, with the membership seam applied first. It is
// the waterfall's match-and-order steps exactly — the reservation and spill
// steps that follow are the runtime's transaction, not this function's.
func EffectiveForAlias(candidates []CandidateGrant, contains MembershipOfAlias, at time.Time) []EffectiveEntitlement {
	matching := make([]CandidateGrant, 0, len(candidates))
	for _, c := range candidates {
		if c.Entitlement == nil {
			continue
		}
		if contains(c.Entitlement.AliasGroupVersionID) {
			matching = append(matching, c)
		}
	}
	return EffectiveEntitlements(matching, at)
}

// waterfallLess is the waterfall, one comparison. Each key is checked in
// turn; the ids make the order total, so sort stability is irrelevant to
// the result and SliceStable's choice is only about nerve calm.
func waterfallLess(a, b EffectiveEntitlement) bool {
	// 1. Specificity: named scope first. A wildcard grant is the broader
	// promise and is consumed last, saved for aliases the named grants
	// do not cover.
	if a.IsWildcard != b.IsWildcard {
		return !a.IsWildcard
	}
	// 2. Soonest period_end first.
	if !a.PeriodEnd.Equal(b.PeriodEnd) {
		return a.PeriodEnd.Before(b.PeriodEnd)
	}
	// 3. Oldest subscription first.
	if !a.SourceSubscriptionCreatedAt.Equal(b.SourceSubscriptionCreatedAt) {
		return a.SourceSubscriptionCreatedAt.Before(b.SourceSubscriptionCreatedAt)
	}
	// 4. The stable last word.
	return a.EntitlementID < b.EntitlementID
}
