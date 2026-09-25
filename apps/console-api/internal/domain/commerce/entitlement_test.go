package commerce

import (
	"errors"
	"testing"
	"time"
)

func mustDefinition(t *testing.T, scope AliasGroupName, amount int64) *GrantDefinition {
	t.Helper()
	d, err := NewGrantDefinition("def-1", "v-1", scope, DimensionCost, amount)
	if err != nil {
		t.Fatalf("NewGrantDefinition returned error: %v", err)
	}
	return d
}

func mustEntitlement(t *testing.T, subID SubscriptionID, cycle int, d *GrantDefinition, scope AliasGroupVersionID, periodStart, periodEnd, now time.Time) *Entitlement {
	t.Helper()
	e, err := NewEntitlement("ent-1", subID, cycle, *d, scope, periodStart, periodEnd, now)
	if err != nil {
		t.Fatalf("NewEntitlement returned error: %v", err)
	}
	return e
}

func TestNewEntitlementCopiesTheDefinitionTerms(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	definition := mustDefinition(t, "anthropic", 2000)

	e := mustEntitlement(t, "sub-1", 1, definition, "0198f0a4-3f6c-7000-8000-000000000001", start, end, clock)

	if e.State != EntitlementActive {
		t.Fatalf("birth state = %q, want active", e.State)
	}
	// The copy is the point: the entitlement stands alone as history even
	// though the definition row is immutable too.
	if e.Dimension != definition.Dimension || e.GrantedAmount != definition.GrantedAmount {
		t.Fatalf("entitlement terms %+v do not copy the definition %+v", e, definition)
	}
	if e.GrantDefinitionID != definition.ID {
		t.Fatalf("GrantDefinitionID = %q, want %q", e.GrantDefinitionID, definition.ID)
	}
	if !e.CreatedAt.Equal(clock) || !e.UpdatedAt.Equal(clock) {
		t.Fatalf("stamps %v/%v are not the roll instant %v", e.CreatedAt, e.UpdatedAt, clock)
	}
}

func TestNewEntitlementRefusals(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	definition := mustDefinition(t, "anthropic", 2000)
	scope := AliasGroupVersionID("0198f0a4-3f6c-7000-8000-000000000001")

	cases := []struct {
		name        string
		cycle       int
		scope       AliasGroupVersionID
		periodStart time.Time
		periodEnd   time.Time
		want        error
	}{
		{"cycle zero", 0, scope, start, end, ErrInvalidPeriod},
		{"empty bounds", 1, scope, start, start, ErrInvalidPeriod},
		{"inverted bounds", 1, scope, end, start, ErrInvalidPeriod},
		{"scope outside the v7 form", 1, AliasGroupVersionID("not-a-uuid"), start, end, ErrInvalidAliasGroupVersionID},
		{"scope pinned with a v4 form", 1, AliasGroupVersionID("0198f0a4-3f6c-4000-8000-000000000001"), start, end, ErrInvalidAliasGroupVersionID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewEntitlement("ent-1", "sub-1", tc.cycle, *definition, tc.scope, tc.periodStart, tc.periodEnd, clock)
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewEntitlement error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEntitlementCoverageIsHalfOpenAndRequiresActiveState(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	e := mustEntitlement(t, "sub-1", 1, mustDefinition(t, "anthropic", 2000),
		"0198f0a4-3f6c-7000-8000-000000000001", start, end, clock)

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"one nanosecond before start", start.Add(-time.Nanosecond), false},
		{"exactly start", start, true},
		{"one nanosecond before end", end.Add(-time.Nanosecond), true},
		{"exactly end", end, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.Covers(tc.at); got != tc.want {
				t.Fatalf("Covers(%v) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
	t.Run("an expired entitlement covers nothing even inside its bounds", func(t *testing.T) {
		if err := e.Expire(end); err != nil {
			t.Fatalf("Expire returned error: %v", err)
		}
		if e.Covers(start.Add(24 * time.Hour)) {
			t.Fatal("an expired entitlement still answers Covers")
		}
	})
}

func TestEntitlementExpiry(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	t.Run("expiring before the end is refused", func(t *testing.T) {
		e := mustEntitlement(t, "sub-1", 1, mustDefinition(t, "anthropic", 2000),
			"0198f0a4-3f6c-7000-8000-000000000001", start, end, clock)
		if err := e.Expire(end.Add(-time.Nanosecond)); !errors.Is(err, ErrNotDue) {
			t.Fatalf("early expire error = %v, want ErrNotDue", err)
		}
		if e.State != EntitlementActive {
			t.Fatalf("state after refused expire = %q, want active", e.State)
		}
	})
	t.Run("expiring at exactly the end is due", func(t *testing.T) {
		e := mustEntitlement(t, "sub-1", 1, mustDefinition(t, "anthropic", 2000),
			"0198f0a4-3f6c-7000-8000-000000000001", start, end, clock)
		if err := e.Expire(end); err != nil {
			t.Fatalf("Expire at end returned error: %v", err)
		}
		if e.State != EntitlementExpired || !e.UpdatedAt.Equal(end) {
			t.Fatalf("after expire: state=%q updated_at=%v", e.State, e.UpdatedAt)
		}
	})
	t.Run("re-expiring is a no-op that rewrites nothing", func(t *testing.T) {
		e := mustEntitlement(t, "sub-1", 1, mustDefinition(t, "anthropic", 2000),
			"0198f0a4-3f6c-7000-8000-000000000001", start, end, clock)
		if err := e.Expire(end); err != nil {
			t.Fatalf("Expire returned error: %v", err)
		}
		stamp := e.UpdatedAt
		if err := e.Expire(end.Add(time.Hour)); err != nil {
			t.Fatalf("re-expire returned error: %v", err)
		}
		if !e.UpdatedAt.Equal(stamp) {
			t.Fatalf("re-expire moved updated_at to %v, want %v", e.UpdatedAt, stamp)
		}
	})
}
