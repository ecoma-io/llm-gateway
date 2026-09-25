package commerce

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var clock = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func TestNewPlanTrimsAndBoundsItsName(t *testing.T) {
	t.Run("surrounding whitespace is trimmed before persisting", func(t *testing.T) {
		p, err := NewPlan("plan-1", "  pro  ", clock)
		if err != nil {
			t.Fatalf("NewPlan returned error: %v", err)
		}
		if p.Name != "pro" {
			t.Fatalf("Name = %q, want %q", p.Name, "pro")
		}
		if !p.CreatedAt.Equal(clock) {
			t.Fatalf("CreatedAt = %v, want %v", p.CreatedAt, clock)
		}
	})
	t.Run("a blank name is refused", func(t *testing.T) {
		for _, name := range []string{"", "   ", "\t\n"} {
			if _, err := NewPlan("plan-1", name, clock); !errors.Is(err, ErrInvalidPlanName) {
				t.Fatalf("NewPlan(%q) error = %v, want ErrInvalidPlanName", name, err)
			}
		}
	})
	t.Run("the name bound is a rune count", func(t *testing.T) {
		exactly := strings.Repeat("é", 256)
		if _, err := NewPlan("plan-1", exactly, clock); err != nil {
			t.Fatalf("NewPlan with 256 runes returned error: %v", err)
		}
		over := strings.Repeat("é", 257)
		if _, err := NewPlan("plan-1", over, clock); !errors.Is(err, ErrInvalidPlanName) {
			t.Fatalf("NewPlan with 257 runes error = %v, want ErrInvalidPlanName", err)
		}
	})
}

func TestNewPlanVersionIsADraftWithValidatedTerms(t *testing.T) {
	t.Run("birth state is draft with no stamps", func(t *testing.T) {
		v, err := NewPlanVersion("v-1", "plan-1", 1, RecurringPeriodCalendarMonth, 1999, clock)
		if err != nil {
			t.Fatalf("NewPlanVersion returned error: %v", err)
		}
		if v.State != PlanVersionDraft {
			t.Fatalf("State = %q, want draft", v.State)
		}
		if v.PublishedAt != nil || v.RetiredAt != nil {
			t.Fatalf("a newborn draft carries stamps: published=%v retired=%v", v.PublishedAt, v.RetiredAt)
		}
		if v.AcceptsSubscriptions() {
			t.Fatal("a draft accepts subscriptions")
		}
	})
	t.Run("a zero price is a free plan and is legal", func(t *testing.T) {
		v, err := NewPlanVersion("v-1", "plan-1", 1, RecurringPeriodCalendarMonth, 0, clock)
		if err != nil {
			t.Fatalf("NewPlanVersion with zero price returned error: %v", err)
		}
		if v.RecurringPriceMinorUnits != 0 {
			t.Fatalf("RecurringPriceMinorUnits = %d, want 0", v.RecurringPriceMinorUnits)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		cases := []struct {
			name    string
			version int
			period  RecurringPeriod
			price   int64
			want    error
		}{
			{"version zero", 0, RecurringPeriodCalendarMonth, 100, ErrInvalidPeriod},
			{"negative price", 1, RecurringPeriodCalendarMonth, -1, ErrInvalidPrice},
			{"unknown period", 1, "fortnight", 100, nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := NewPlanVersion("v-1", "plan-1", tc.version, tc.period, tc.price, clock)
				if tc.want == nil {
					if err == nil {
						t.Fatalf("NewPlanVersion returned nil error, want a refusal")
					}
					return
				}
				if !errors.Is(err, tc.want) {
					t.Fatalf("NewPlanVersion error = %v, want %v", err, tc.want)
				}
			})
		}
	})
}

func TestPlanVersionLifecycle(t *testing.T) {
	publish := func(t *testing.T) *PlanVersion {
		t.Helper()
		v, err := NewPlanVersion("v-1", "plan-1", 1, RecurringPeriodCalendarMonth, 100, clock)
		if err != nil {
			t.Fatalf("NewPlanVersion returned error: %v", err)
		}
		if err := v.Publish(clock); err != nil {
			t.Fatalf("Publish returned error: %v", err)
		}
		return v
	}
	t.Run("publishing freezes the terms and is idempotent", func(t *testing.T) {
		v := publish(t)
		if v.State != PlanVersionPublished || v.PublishedAt == nil {
			t.Fatalf("after publish: state=%q published_at=%v", v.State, v.PublishedAt)
		}
		if !v.AcceptsSubscriptions() {
			t.Fatal("a published version refuses subscriptions")
		}
		publishedAt := *v.PublishedAt
		if err := v.Publish(clock.Add(time.Hour)); err != nil {
			t.Fatalf("re-publish returned error: %v", err)
		}
		if !v.PublishedAt.Equal(publishedAt) {
			t.Fatalf("re-publish moved published_at to %v, want %v", v.PublishedAt, publishedAt)
		}
	})
	t.Run("retiring stops new subscriptions and is idempotent", func(t *testing.T) {
		v := publish(t)
		if err := v.Retire(clock.Add(time.Hour)); err != nil {
			t.Fatalf("Retire returned error: %v", err)
		}
		if v.State != PlanVersionRetired || v.RetiredAt == nil {
			t.Fatalf("after retire: state=%q retired_at=%v", v.State, v.RetiredAt)
		}
		if v.AcceptsSubscriptions() {
			t.Fatal("a retired version accepts subscriptions")
		}
		retiredAt := *v.RetiredAt
		if err := v.Retire(clock.Add(2 * time.Hour)); err != nil {
			t.Fatalf("re-retire returned error: %v", err)
		}
		if !v.RetiredAt.Equal(retiredAt) {
			t.Fatalf("re-retire moved retired_at to %v, want %v", v.RetiredAt, retiredAt)
		}
		if err := v.Publish(clock.Add(2 * time.Hour)); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("publish of a retired version error = %v, want ErrInvalidTransition", err)
		}
	})
	t.Run("a draft is abandoned, not retired", func(t *testing.T) {
		v, err := NewPlanVersion("v-1", "plan-1", 1, RecurringPeriodCalendarMonth, 100, clock)
		if err != nil {
			t.Fatalf("NewPlanVersion returned error: %v", err)
		}
		if err := v.Retire(clock); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("retire of a draft error = %v, want ErrInvalidTransition", err)
		}
		if v.State != PlanVersionDraft {
			t.Fatalf("State = %q after refused retire, want draft", v.State)
		}
	})
}

func TestGrantDefinitionValidation(t *testing.T) {
	t.Run("terms are accepted verbatim", func(t *testing.T) {
		d, err := NewGrantDefinition("d-1", "v-1", "anthropic", DimensionCost, 2000)
		if err != nil {
			t.Fatalf("NewGrantDefinition returned error: %v", err)
		}
		if d.IsWildcard() {
			t.Fatal("a named scope reports wildcard")
		}
		if d.Dimension != DimensionCost || d.GrantedAmount != 2000 {
			t.Fatalf("definition terms drifted: %+v", d)
		}
	})
	t.Run("the wildcard scope is recognised", func(t *testing.T) {
		d, err := NewGrantDefinition("d-1", "v-1", WildcardGroupName, DimensionCost, 2000)
		if err != nil {
			t.Fatalf("NewGrantDefinition with wildcard returned error: %v", err)
		}
		if !d.IsWildcard() {
			t.Fatal("the wildcard scope does not report wildcard")
		}
	})
	t.Run("refusals", func(t *testing.T) {
		cases := []struct {
			name   string
			scope  AliasGroupName
			dim    Dimension
			amount int64
			want   error
		}{
			{"zero grant purchases nothing", "anthropic", DimensionCost, 0, ErrInvalidGrantAmount},
			{"negative grant", "anthropic", DimensionCost, -5, ErrInvalidGrantAmount},
			{"unknown dimension", "anthropic", "tokens", 5, nil},
			{"group name outside the grammar", "bad name!", DimensionCost, 5, ErrInvalidAliasGroupName},
			{"empty group name", "", DimensionCost, 5, ErrInvalidAliasGroupName},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := NewGrantDefinition("d-1", "v-1", tc.scope, tc.dim, tc.amount)
				if tc.want == nil {
					if err == nil {
						t.Fatal("NewGrantDefinition returned nil error, want a refusal")
					}
					return
				}
				if !errors.Is(err, tc.want) {
					t.Fatalf("NewGrantDefinition error = %v, want %v", err, tc.want)
				}
			})
		}
	})
}
