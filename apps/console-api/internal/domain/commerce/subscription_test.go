package commerce

import (
	"errors"
	"testing"
	"time"
)

func mustSubscription(t *testing.T, account AccountID, version PlanVersionID, startAt time.Time, renewal bool) *Subscription {
	t.Helper()
	s, err := NewSubscription("sub-1", account, version, startAt, renewal, startAt.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("NewSubscription returned error: %v", err)
	}
	return s
}

// mustActivate promotes a subscription at exactly its start_at, the scan's
// own first due instant, and returns the cycle-1 end for follow-on asserts.
func mustActivate(t *testing.T, s *Subscription, now time.Time) time.Time {
	t.Helper()
	_, end, err := s.CycleBoundsFor(1)
	if err != nil {
		t.Fatalf("CycleBoundsFor(1) returned error: %v", err)
	}
	if err := s.ActivateWithCycle(1, s.StartAt, end, now); err != nil {
		t.Fatalf("ActivateWithCycle returned error: %v", err)
	}
	return end
}

func TestNewSubscriptionIsPendingWithNullCycleFields(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s := mustSubscription(t, "acc-1", "v-1", start, true)

	if s.State != SubscriptionPending {
		t.Fatalf("birth state = %q, want pending", s.State)
	}
	if s.CycleNumber != nil || s.PeriodStart != nil || s.PeriodEnd != nil {
		t.Fatalf("a pending subscription carries cycle state: %v %v %v", s.CycleNumber, s.PeriodStart, s.PeriodEnd)
	}
	if s.CancelAt != nil || s.CancellationMode != "" {
		t.Fatalf("a newborn subscription carries a cancellation instruction: %v %q", s.CancelAt, s.CancellationMode)
	}
	if s.IsDueForStart(start.Add(-time.Nanosecond)) {
		t.Fatal("a subscription is due before its start_at")
	}
	if !s.IsDueForStart(start) {
		t.Fatal("a subscription is due exactly at its start_at")
	}
}

func TestActivation(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	t.Run("activation before start_at is refused", func(t *testing.T) {
		s := mustSubscription(t, "acc-1", "v-1", start, true)
		_, end, _ := s.CycleBoundsFor(1)
		err := s.ActivateWithCycle(1, s.StartAt, end, start.Add(-time.Nanosecond))
		if !errors.Is(err, ErrNotDue) {
			t.Fatalf("early activation error = %v, want ErrNotDue", err)
		}
		if s.State != SubscriptionPending {
			t.Fatalf("state after refused activation = %q, want pending", s.State)
		}
	})
	t.Run("activation at start_at hands over cycle 1", func(t *testing.T) {
		s := mustSubscription(t, "acc-1", "v-1", start, true)
		end := mustActivate(t, s, start)
		if s.State != SubscriptionActive || s.CycleNumber == nil || *s.CycleNumber != 1 {
			t.Fatalf("after activation: state=%q cycle=%v", s.State, s.CycleNumber)
		}
		if s.PeriodStart == nil || !s.PeriodStart.Equal(start) || !s.PeriodEnd.Equal(end) {
			t.Fatalf("cycle bounds %v..%v, want %v..%v", s.PeriodStart, s.PeriodEnd, start, end)
		}
		if !s.CycleCovers(start) || !s.CycleCovers(end.Add(-time.Nanosecond)) || s.CycleCovers(end) {
			t.Fatal("the activated cycle is not half-open over its own bounds")
		}
	})
	t.Run("the first cycle is 1 and only 1", func(t *testing.T) {
		s := mustSubscription(t, "acc-1", "v-1", start, true)
		_, end, _ := s.CycleBoundsFor(1)
		if err := s.ActivateWithCycle(2, s.StartAt, end, start); !errors.Is(err, ErrInvalidPeriod) {
			t.Fatalf("activation with cycle 2 error = %v, want ErrInvalidPeriod", err)
		}
	})
	t.Run("a second activation is refused", func(t *testing.T) {
		s := mustSubscription(t, "acc-1", "v-1", start, true)
		mustActivate(t, s, start)
		_, end2, _ := s.CycleBoundsFor(2)
		if err := s.ActivateWithCycle(1, s.StartAt, end2, start); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("second activation error = %v, want ErrInvalidTransition", err)
		}
	})
	t.Run("activation refuses empty bounds", func(t *testing.T) {
		s := mustSubscription(t, "acc-1", "v-1", start, true)
		if err := s.ActivateWithCycle(1, start, start, start); !errors.Is(err, ErrInvalidPeriod) {
			t.Fatalf("activation with empty bounds error = %v, want ErrInvalidPeriod", err)
		}
	})
}

// The roll tests walk a subscription anchored Sep 15 12:00 UTC: cycle 1 is
// [Sep 15, Oct 15), cycle 2 [Oct 15, Nov 15), cycle 3 [Nov 15, Dec 15).
func TestCycleRoll(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	newActive := func(t *testing.T, renewal bool) (*Subscription, time.Time) {
		t.Helper()
		s := mustSubscription(t, "acc-1", "v-1", start, renewal)
		end := mustActivate(t, s, start)
		return s, end
	}
	mustRoll := func(t *testing.T, s *Subscription, now time.Time) time.Time {
		t.Helper()
		cycle := *s.CycleNumber + 1
		ps, pe, err := s.CycleBoundsFor(cycle)
		if err != nil {
			t.Fatalf("CycleBoundsFor(%d) returned error: %v", cycle, err)
		}
		if err := s.RollCycle(cycle, ps, pe, now); err != nil {
			t.Fatalf("RollCycle to cycle %d returned error: %v", cycle, err)
		}
		return pe
	}

	t.Run("rolling before the period ends is refused", func(t *testing.T) {
		s, end := newActive(t, true)
		err := s.RollCycle(2, start.Add(30*24*time.Hour), end.Add(30*24*time.Hour), end.Add(-time.Nanosecond))
		if !errors.Is(err, ErrNotDue) {
			t.Fatalf("early roll error = %v, want ErrNotDue", err)
		}
		if *s.CycleNumber != 1 {
			t.Fatalf("cycle advanced to %d on a refused roll", *s.CycleNumber)
		}
	})
	t.Run("rolling at exactly the period end opens the next cycle", func(t *testing.T) {
		s, end := newActive(t, true)
		newEnd := mustRoll(t, s, end)
		if *s.CycleNumber != 2 {
			t.Fatalf("cycle = %d, want 2", *s.CycleNumber)
		}
		if !s.PeriodStart.Equal(end) || !s.PeriodEnd.Equal(newEnd) {
			t.Fatalf("cycle 2 bounds %v..%v, want %v..%v", s.PeriodStart, s.PeriodEnd, end, newEnd)
		}
	})
	t.Run("a fixed-term subscription never rolls", func(t *testing.T) {
		s, end := newActive(t, false)
		_, next, _ := s.CycleBoundsFor(2)
		err := s.RollCycle(2, end, next, end)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("roll of a fixed-term subscription error = %v, want ErrInvalidTransition", err)
		}
	})
	t.Run("the roll never skips a cycle", func(t *testing.T) {
		s, end := newActive(t, true)
		_, next, _ := s.CycleBoundsFor(3)
		if err := s.RollCycle(3, end, next, end); !errors.Is(err, ErrInvalidPeriod) {
			t.Fatalf("skip-roll error = %v, want ErrInvalidPeriod", err)
		}
	})
	t.Run("a cancellation due at or before the new cycle's end suppresses the roll", func(t *testing.T) {
		s, end := newActive(t, true)
		cycle3Start := mustRoll(t, s, end) // cycle 2 rolled at Oct 15; cycle 3: [Nov 15, Dec 15)
		_, cycle3End, _ := s.CycleBoundsFor(3)

		// cancel_at exactly at cycle 3's end: the cycle would not survive past
		// its own cancellation, so the roll is refused even though cancel_at
		// lies after "now".
		if err := s.ScheduleCancellation(cycle3End, end); err != nil {
			t.Fatalf("ScheduleCancellation returned error: %v", err)
		}
		err := s.RollCycle(3, cycle3Start, cycle3End, cycle3Start)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("roll past cancel_at==period_end error = %v, want ErrInvalidTransition", err)
		}
		if *s.CycleNumber != 2 {
			t.Fatalf("cycle advanced to %d on a suppressed roll", *s.CycleNumber)
		}

		// One nanosecond later and the cycle survives the whole period: the
		// roll is allowed.
		if err := s.ScheduleCancellation(cycle3End.Add(time.Nanosecond), end); err != nil {
			t.Fatalf("ScheduleCancellation returned error: %v", err)
		}
		if err := s.RollCycle(3, cycle3Start, cycle3End, cycle3Start); err != nil {
			t.Fatalf("roll with surviving cancel_at returned error: %v", err)
		}
		if *s.CycleNumber != 3 {
			t.Fatalf("cycle = %d, want 3", *s.CycleNumber)
		}
	})
	t.Run("a suspended subscription cannot roll", func(t *testing.T) {
		s, end := newActive(t, true)
		if err := s.Suspend(end.Add(-24 * time.Hour)); err != nil {
			t.Fatalf("Suspend returned error: %v", err)
		}
		_, next, _ := s.CycleBoundsFor(2)
		if err := s.RollCycle(2, end, next, end); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("roll of a suspended subscription error = %v, want ErrInvalidTransition", err)
		}
	})
}

func TestScheduledCancellation(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	t.Run("only an active subscription takes the instruction", func(t *testing.T) {
		pending := mustSubscription(t, "acc-1", "v-1", start, true)
		if err := pending.ScheduleCancellation(start, start); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("scheduling on pending error = %v, want ErrInvalidTransition", err)
		}
	})
	t.Run("an unset instant is refused", func(t *testing.T) {
		s := mustSubscription(t, "acc-1", "v-1", start, true)
		mustActivate(t, s, start)
		if err := s.ScheduleCancellation(time.Time{}, start); !errors.Is(err, ErrInvalidCancellation) {
			t.Fatalf("zero cancel_at error = %v, want ErrInvalidCancellation", err)
		}
	})
	t.Run("the subscription stays active and usable until cancel_at", func(t *testing.T) {
		s := mustSubscription(t, "acc-1", "v-1", start, true)
		mustActivate(t, s, start)
		cancelAt := start.Add(10 * 24 * time.Hour)
		if err := s.ScheduleCancellation(cancelAt, start); err != nil {
			t.Fatalf("ScheduleCancellation returned error: %v", err)
		}
		if s.State != SubscriptionActive {
			t.Fatalf("state after scheduling = %q, want active", s.State)
		}
		if s.CancelAt == nil || !s.CancelAt.Equal(cancelAt) || s.CancellationMode != CancellationScheduled {
			t.Fatalf("instruction drifted: cancel_at=%v mode=%q", s.CancelAt, s.CancellationMode)
		}
		if s.IsDueForCancellation(cancelAt.Add(-time.Nanosecond)) {
			t.Fatal("the cancellation is due before its instant")
		}
		if !s.IsDueForCancellation(cancelAt) {
			t.Fatal("the cancellation is due exactly at its instant")
		}
	})
	t.Run("completion keeps the instruction's original instant", func(t *testing.T) {
		s := mustSubscription(t, "acc-1", "v-1", start, true)
		mustActivate(t, s, start)
		cancelAt := start.Add(10 * 24 * time.Hour)
		if err := s.ScheduleCancellation(cancelAt, start); err != nil {
			t.Fatalf("ScheduleCancellation returned error: %v", err)
		}
		completedAt := cancelAt.Add(time.Hour)
		if err := s.CompleteDueCancellation(completedAt); err != nil {
			t.Fatalf("CompleteDueCancellation returned error: %v", err)
		}
		if s.State != SubscriptionCancelled {
			t.Fatalf("state after completion = %q, want cancelled", s.State)
		}
		if !s.CancelAt.Equal(cancelAt) {
			t.Fatalf("cancel_at moved to %v, want the instructed %v", s.CancelAt, cancelAt)
		}
		if s.CancellationMode != CancellationScheduled {
			t.Fatalf("mode = %q, want scheduled", s.CancellationMode)
		}
		if !s.UpdatedAt.Equal(completedAt) {
			t.Fatalf("updated_at = %v, want %v", s.UpdatedAt, completedAt)
		}
	})
	t.Run("completion before the instant is refused", func(t *testing.T) {
		s := mustSubscription(t, "acc-1", "v-1", start, true)
		mustActivate(t, s, start)
		if err := s.ScheduleCancellation(start.Add(24*time.Hour), start); err != nil {
			t.Fatalf("ScheduleCancellation returned error: %v", err)
		}
		if err := s.CompleteDueCancellation(start); !errors.Is(err, ErrNotDue) {
			t.Fatalf("early completion error = %v, want ErrNotDue", err)
		}
	})
	t.Run("an immediate cancellation is not a scheduled one", func(t *testing.T) {
		s := mustSubscription(t, "acc-1", "v-1", start, true)
		mustActivate(t, s, start)
		now := start.Add(24 * time.Hour)
		if err := s.Cancel(now); err != nil {
			t.Fatalf("Cancel returned error: %v", err)
		}
		if s.State != SubscriptionCancelled || s.CancellationMode != CancellationImmediate || !s.CancelAt.Equal(now) {
			t.Fatalf("immediate cancel drifted: state=%q mode=%q cancel_at=%v", s.State, s.CancellationMode, s.CancelAt)
		}
		if err := s.CompleteDueCancellation(now.Add(time.Hour)); !errors.Is(err, ErrNotDue) {
			t.Fatalf("completion on cancelled error = %v, want ErrNotDue", err)
		}
	})
	t.Run("cancelling a pending subscription is refused", func(t *testing.T) {
		pending := mustSubscription(t, "acc-1", "v-1", start, true)
		if err := pending.Cancel(start); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("cancel of pending error = %v, want ErrInvalidTransition", err)
		}
	})
}

func TestSuspensionAndReinstatement(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	t.Run("the active-suspended round trip is reversible and idempotent", func(t *testing.T) {
		s := mustSubscription(t, "acc-1", "v-1", start, true)
		mustActivate(t, s, start)
		if err := s.Suspend(start.Add(time.Hour)); err != nil {
			t.Fatalf("Suspend returned error: %v", err)
		}
		if s.State != SubscriptionSuspended {
			t.Fatalf("state = %q, want suspended", s.State)
		}
		if err := s.Suspend(start.Add(2 * time.Hour)); err != nil {
			t.Fatalf("idempotent Suspend returned error: %v", err)
		}
		if err := s.Reinstate(start.Add(3 * time.Hour)); err != nil {
			t.Fatalf("Reinstate returned error: %v", err)
		}
		if s.State != SubscriptionActive {
			t.Fatalf("state = %q, want active", s.State)
		}
		if err := s.Reinstate(start.Add(4 * time.Hour)); err != nil {
			t.Fatalf("idempotent Reinstate returned error: %v", err)
		}
	})
	t.Run("terminal states refuse both directions", func(t *testing.T) {
		s := mustSubscription(t, "acc-1", "v-1", start, true)
		mustActivate(t, s, start)
		if err := s.Cancel(start.Add(time.Hour)); err != nil {
			t.Fatalf("Cancel returned error: %v", err)
		}
		if err := s.Suspend(start.Add(2 * time.Hour)); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("suspend of cancelled error = %v, want ErrInvalidTransition", err)
		}
		if err := s.Reinstate(start.Add(2 * time.Hour)); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("reinstate of cancelled error = %v, want ErrInvalidTransition", err)
		}
	})
}

func TestExpiry(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	expireCase := func(t *testing.T, renewal bool) (*Subscription, time.Time) {
		t.Helper()
		s := mustSubscription(t, "acc-1", "v-1", start, renewal)
		end := mustActivate(t, s, start)
		return s, end
	}

	t.Run("a fixed-term subscription expires when its cycle ends", func(t *testing.T) {
		s, end := expireCase(t, false)
		if s.IsDueForExpiry(end.Add(-time.Nanosecond)) {
			t.Fatal("expiry due before the period ends")
		}
		if !s.IsDueForExpiry(end) {
			t.Fatal("expiry not due exactly at the period end")
		}
		if err := s.Expire(end); err != nil {
			t.Fatalf("Expire returned error: %v", err)
		}
		if s.State != SubscriptionExpired {
			t.Fatalf("state = %q, want expired", s.State)
		}
	})
	t.Run("a suspended subscription still expires — suspension does not stop the clock", func(t *testing.T) {
		s, end := expireCase(t, false)
		if err := s.Suspend(end.Add(-24 * time.Hour)); err != nil {
			t.Fatalf("Suspend returned error: %v", err)
		}
		if err := s.Expire(end); err != nil {
			t.Fatalf("Expire of suspended returned error: %v", err)
		}
		if s.State != SubscriptionExpired {
			t.Fatalf("state = %q, want expired", s.State)
		}
	})
	t.Run("a renewing subscription is not due for expiry", func(t *testing.T) {
		s, end := expireCase(t, true)
		if s.IsDueForExpiry(end) {
			t.Fatal("a renewing subscription is due for expiry at the period end")
		}
	})
	t.Run("a cancelled subscription never expires", func(t *testing.T) {
		s, end := expireCase(t, true)
		if err := s.Cancel(end.Add(-24 * time.Hour)); err != nil {
			t.Fatalf("Cancel returned error: %v", err)
		}
		if s.IsDueForExpiry(end) {
			t.Fatal("a cancelled subscription is due for expiry")
		}
		if err := s.Expire(end); !errors.Is(err, ErrNotDue) {
			t.Fatalf("expire of cancelled error = %v, want ErrNotDue", err)
		}
	})
	t.Run("expiry preserves the last cycle as history", func(t *testing.T) {
		s, end := expireCase(t, false)
		if err := s.Expire(end.Add(time.Hour)); err != nil {
			t.Fatalf("Expire returned error: %v", err)
		}
		if *s.CycleNumber != 1 || !s.PeriodEnd.Equal(end) {
			t.Fatalf("history drifted: cycle=%d period_end=%v", *s.CycleNumber, s.PeriodEnd)
		}
	})
}

func TestNewSubscriptionRefusals(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	if _, err := NewSubscription("", "acc-1", "v-1", start, true, clock); err == nil {
		t.Fatal("NewSubscription with a blank id returned nil error")
	}
	if _, err := NewSubscription("sub-1", "", "v-1", start, true, clock); err == nil {
		t.Fatal("NewSubscription with a blank account returned nil error")
	}
	if _, err := NewSubscription("sub-1", "acc-1", "", start, true, clock); err == nil {
		t.Fatal("NewSubscription with a blank version returned nil error")
	}
	if _, err := NewSubscription("sub-1", "acc-1", "v-1", time.Time{}, true, clock); err == nil {
		t.Fatal("NewSubscription with an unset start returned nil error")
	}
}
