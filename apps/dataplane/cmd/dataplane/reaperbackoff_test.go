package main

import (
	"testing"
	"time"
)

// The cadence arithmetic, tested directly rather than through a running loop.
// A loop test would have to assert on wall-clock instants to catch a backoff
// that does not double, and a test that sleeps to observe eight doublings is
// a test nobody keeps. The arithmetic is pure, so it is tested pure.
//
// The shape pinned here is the same one the control plane's worker pins, with
// two differences that are the point of testing both: this loop's ceiling is
// LOWER, because the work it delays is expiring capacity rather than a
// background read, and that is a claim about this loop alone. If a later
// change copies the other number over, these tests are what notice.

func TestTheBackoffDoublesThenResetsOnTheFirstSuccess(t *testing.T) {
	const interval = 5 * time.Second

	tests := []struct {
		name       string
		streak     int
		failedNow  bool
		wantStreak int
		wantGap    time.Duration
	}{
		{"a first failure waits one interval", 0, true, 1, interval},
		{"a second failure doubles", 1, true, 2, 2 * interval},
		{"a third failure doubles again", 2, true, 3, 4 * interval},
		{"a success drops straight back to the configured cadence", 4, false, 0, interval},
		{"a success after one failure is still a reset", 1, false, 0, interval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			streak, gap := nextBackoff(tt.streak, tt.failedNow, interval)
			if streak != tt.wantStreak {
				t.Errorf("streak = %d, want %d", streak, tt.wantStreak)
			}
			if gap != tt.wantGap {
				t.Errorf("gap = %s, want %s", gap, tt.wantGap)
			}
		})
	}
}

func TestTheBackoffStopsAtItsCeilingAndNeverWraps(t *testing.T) {
	const interval = time.Second

	// The ceiling is lower here than on the control plane's worker, and the
	// reason is the work: a slow reconciliation pass delays a detection,
	// while a slow reaper widens the very window it exists to close. Pinned
	// as a literal so the two numbers cannot drift into each other.
	const wantCeiling = 8
	if reaperBackoffCeiling != wantCeiling {
		t.Errorf("reaperBackoffCeiling = %d, want %d — a reaper that backs off further than this leaves holds waiting that only this loop would have reclaimed", reaperBackoffCeiling, wantCeiling)
	}

	_, ceiling := nextBackoff(reaperBackoffCeiling-1, true, interval)
	if ceiling != reaperBackoffCeiling*interval {
		t.Errorf("gap at the ceiling = %s, want %s", ceiling, reaperBackoffCeiling*interval)
	}
	for _, streak := range []int{reaperBackoffCeiling, 64, 1000, 1 << 20} {
		_, gap := nextBackoff(streak, true, interval)
		if gap > reaperBackoffCeiling*interval {
			t.Errorf("a streak of %d waits %s, want at most the ceiling %s", streak, gap, reaperBackoffCeiling*interval)
		}
		if gap <= 0 {
			t.Errorf("a streak of %d waits %s, want a positive gap — a wrapped shift produces a negative duration, which is a hot drain rather than a backoff", streak, gap)
		}
	}
}

func TestTheJitterOnlyEverGrowsAGapAndOnlyByItsFraction(t *testing.T) {
	const base = 5 * time.Second

	// The spread is an addition and nothing else: a jitter that could also
	// SHRINK a gap would let a fleet of reapers re-synchronise a short gap,
	// which is the thing the spread exists to prevent.
	if got := jittered(base, reaperJitterFraction, func(int64) int64 { return 0 }); got != base {
		t.Errorf("gap with the jitter at its bottom = %s, want the bare %s", got, base)
	}
	top := func(n int64) int64 { return n - 1 }
	wantTop := base + time.Duration(float64(base)*reaperJitterFraction)
	if got := jittered(base, reaperJitterFraction, top); got != wantTop {
		t.Errorf("gap with the jitter at its top = %s, want %s", got, wantTop)
	}

	// Int64N is half-open, so the range is one wider than the span; passing
	// the exact span would leave the top value undrawable.
	var asked int64
	jittered(base, reaperJitterFraction, func(n int64) int64 { asked = n; return 0 })
	if want := int64(float64(base)*reaperJitterFraction) + 1; asked != want {
		t.Errorf("the jitter was offered a range of %d, want %d", asked, want)
	}
}

func TestTheJitterLeavesAGapItCannotSpread(t *testing.T) {
	if got := jittered(time.Nanosecond, reaperJitterFraction, func(n int64) int64 { return n }); got != time.Nanosecond {
		t.Errorf("gap = %s, want the unspreadeable %s returned unchanged", got, time.Nanosecond)
	}
	if got := jittered(0, reaperJitterFraction, func(int64) int64 { return 99 }); got != 0 {
		t.Errorf("gap = %s for a zero interval, want 0", got)
	}
	if got := jittered(time.Minute, 0, func(int64) int64 { return 99 }); got != time.Minute {
		t.Errorf("gap = %s for a zero fraction, want the bare interval", got)
	}
}
