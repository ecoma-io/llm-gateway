package main

import (
	"testing"
	"time"
)

// The cadence arithmetic, tested directly rather than through a running loop.
// A loop test would have to assert on wall-clock instants to catch a
// backoff that does not double, and a test that sleeps for sixteen minutes to
// observe sixteen doublings is a test nobody keeps. The arithmetic is pure,
// so it is tested pure — and the loop's own use of it is one line that the
// build already covers.
//
// What is being pinned is the SHAPE, because every part of it is a choice
// someone could later change by accident:
//
//   - a failure doubles and a success resets to the configured interval, with
//     no middle state a reader has to interpret
//   - the ceiling is a multiple of the CONFIGURED interval, so it does not
//     drift with each doubling
//   - a long streak is clamped rather than wrapped, because a wrapped shift
//     would produce a NEGATIVE gap and a loop that then sweeps hot
//   - jitter only ever grows a gap, and only by a bounded fraction of it

func TestTheBackoffDoublesThenResetsOnTheFirstSuccess(t *testing.T) {
	const interval = time.Minute

	// The streak is the loop's whole state, and the table below is the whole
	// story of it: what a run of failures waits, and what the success after
	// them waits. Every entry is read off the rule rather than measured from a
	// running loop, because the rule is what a later change would alter.
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

	// The ceiling, approached from below and then not: an operator reading a
	// stalled worker's cadence needs a number that stops meaning something new
	// every cycle.
	_, ceiling := nextBackoff(reconBackoffCeiling-1, true, interval)
	if ceiling != reconBackoffCeiling*interval {
		t.Errorf("gap at the ceiling = %s, want %s", ceiling, reconBackoffCeiling*interval)
	}
	for _, streak := range []int{reconBackoffCeiling, 64, 1000, 1 << 20} {
		_, gap := nextBackoff(streak, true, interval)
		if gap > reconBackoffCeiling*interval {
			t.Errorf("a streak of %d waits %s, want at most the ceiling %s", streak, gap, reconBackoffCeiling*interval)
		}
		if gap <= 0 {
			t.Errorf("a streak of %d waits %s, want a positive gap — a wrapped shift produces a negative duration, which is a hot loop rather than a backoff", streak, gap)
		}
	}

	// And the ceiling is measured against the CONFIGURED interval, not
	// against the gap it produced, so it is the same number on every worker
	// whatever its own history was. This is the property that makes the
	// ceiling readable rather than merely bounded.
	_, wide := nextBackoff(10, true, 10*time.Minute)
	_, narrow := nextBackoff(10, true, time.Minute)
	if wide/narrow != 10 {
		t.Errorf("the ceiling tracked the failing worker's own history: %s and %s for a tenfold difference in configured interval, want one proportional to the configuration", wide, narrow)
	}
}

func TestTheJitterOnlyEverGrowsAGapAndOnlyByItsFraction(t *testing.T) {
	const base = time.Minute

	// Both ends of the range, and the middle: the spread is rand.Int64N over
	// [0, span], so a fake that returns the bottom of the range must add
	// NOTHING (a spread that could also shrink a gap would let a fleet
	// re-synchronise) and one that returns the top must add exactly the
	// fraction.
	bottom := func(int64) int64 { return 0 }

	if got := jittered(base, reconJitterFraction, bottom); got != base {
		t.Errorf("gap with the jitter at its bottom = %s, want the bare %s — the spread is an addition and nothing else", got, base)
	}
	// The top is `n-1` rather than `n` because Int64N is half-open: the largest
	// value it can return is span-1, and a fake that answered `n` would prove a
	// top the real generator can never draw.
	top := func(n int64) int64 { return n - 1 }
	wantTop := base + time.Duration(float64(base)*reconJitterFraction)
	if got := jittered(base, reconJitterFraction, top); got != wantTop {
		t.Errorf("gap with the jitter at its top = %s, want %s", got, wantTop)
	}

	// The function it is handed is asked for a range one wider than the
	// fraction, because Int64N is half-open. A caller that passed the exact
	// span would never draw its top value, and a test that did not notice
	// would leave that off-by-one in the code.
	var asked int64
	jittered(base, reconJitterFraction, func(n int64) int64 { asked = n; return 0 })
	if want := int64(float64(base)*reconJitterFraction) + 1; asked != want {
		t.Errorf("the jitter was offered a range of %d, want %d — Int64N is half-open, so the span itself would never be drawn", asked, want)
	}
}

func TestTheJitterLeavesAGapItCannotSpread(t *testing.T) {
	// A gap too short to spread by a whole nanosecond is returned unchanged.
	// Rounding up instead would turn a one-nanosecond gap into a two-
	// nanosecond one — a hundred percent jitter, which is not a spread but a
	// doubling by another name.
	if got := jittered(time.Nanosecond, reconJitterFraction, func(n int64) int64 { return n }); got != time.Nanosecond {
		t.Errorf("gap = %s, want the unspreadeable %s returned unchanged", got, time.Nanosecond)
	}
	// A zero gap and a zero fraction are both no-ops rather than a divide or
	// a draw from an empty range: a misconfigured interval that reached here
	// should be a plain zero wait, not a panic in a background goroutine.
	if got := jittered(0, reconJitterFraction, func(int64) int64 { return 99 }); got != 0 {
		t.Errorf("gap = %s for a zero interval, want 0", got)
	}
	if got := jittered(time.Minute, 0, func(int64) int64 { return 99 }); got != time.Minute {
		t.Errorf("gap = %s for a zero fraction, want the bare interval", got)
	}
}
