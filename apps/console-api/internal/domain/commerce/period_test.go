package commerce

import (
	"errors"
	"testing"
	"time"
)

// The boundary suite: cycle arithmetic and half-open coverage are the two
// places a "reasonable" implementation quietly diverges from ADR 0003, so
// every rule the ADR fixes is asserted at its exact edge.

func TestCycleBoundsAnchorAndClipAtMonthEnd(t *testing.T) {
	cases := []struct {
		name    string
		anchor  time.Time
		wantEnd time.Time
	}{
		{
			name:    "mid-month anchor is exact",
			anchor:  time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC),
			wantEnd: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC),
		},
		{
			name:    "january 31 clips to february 28 in a common year",
			anchor:  time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC),
			wantEnd: time.Date(2026, 2, 28, 23, 59, 59, 0, time.UTC),
		},
		{
			name:    "january 31 clips to february 29 in a leap year",
			anchor:  time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC),
			wantEnd: time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
		},
		{
			name:    "january 31 plus three months lands on april 30",
			anchor:  time.Date(2026, 1, 31, 6, 30, 0, 0, time.UTC),
			wantEnd: time.Date(2026, 2, 28, 6, 30, 0, 0, time.UTC),
		},
		{
			name:    "august 31 clips to september 30",
			anchor:  time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
			wantEnd: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		},
		{
			name:    "day 30 of a 31-day month never clips",
			anchor:  time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC),
			wantEnd: time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		},
		{
			name:    "sub-second precision survives the arithmetic",
			anchor:  time.Date(2026, 9, 15, 12, 0, 0, 123456789, time.UTC),
			wantEnd: time.Date(2026, 10, 15, 12, 0, 0, 123456789, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, end, err := CycleBounds(tc.anchor, 1)
			if err != nil {
				t.Fatalf("CycleBounds returned error: %v", err)
			}
			if !end.Equal(tc.wantEnd) {
				t.Fatalf("cycle 1 end = %v, want %v", end.UTC(), tc.wantEnd)
			}
		})
	}
	t.Run("three stacked cycles carry the january 31 anchor to april 30", func(t *testing.T) {
		anchor := time.Date(2026, 1, 31, 6, 30, 0, 0, time.UTC)
		start, _, err := CycleBounds(anchor, 4)
		if err != nil {
			t.Fatalf("CycleBounds(4) returned error: %v", err)
		}
		want := time.Date(2026, 4, 30, 6, 30, 0, 0, time.UTC)
		if !start.Equal(want) {
			t.Fatalf("cycle 4 starts %v, want %v — the clip spilled across a month", start, want)
		}
	})
}

func TestCycleBoundsTileWithoutGapOrOverlap(t *testing.T) {
	start := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	for cycle := 1; cycle <= 24; cycle++ {
		start_, end, err := CycleBounds(start, cycle)
		if err != nil {
			t.Fatalf("CycleBounds(%d) returned error: %v", cycle, err)
		}
		if !start_.Before(end) {
			t.Fatalf("cycle %d bounds [%v, %v) are empty or inverted", cycle, start_, end)
		}
		if cycle == 1 {
			continue
		}
		_, prevEnd, err := CycleBounds(start, cycle-1)
		if err != nil {
			t.Fatalf("CycleBounds(%d) returned error: %v", cycle-1, err)
		}
		if !prevEnd.Equal(start_) {
			t.Fatalf("cycle %d starts at %v, but cycle %d ends at %v — the tiling broke",
				cycle, start_, cycle-1, prevEnd)
		}
	}
}

func TestCycleBoundsRefuseCycleZero(t *testing.T) {
	if _, _, err := CycleBounds(clock, 0); !errors.Is(err, ErrInvalidPeriod) {
		t.Fatalf("CycleBounds cycle 0 error = %v, want ErrInvalidPeriod", err)
	}
}

// TestCycleCoverageIsHalfOpen is the boundary table the task names: exact
// start, end−ε, end, end+ε. A cycle is [start, end) — an admission at
// exactly start is in it, an admission at exactly end is in the next cycle
// or in none.
func TestCycleCoverageIsHalfOpen(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	end := start.Add(30 * 24 * time.Hour)
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"one nanosecond before start", start.Add(-time.Nanosecond), false},
		{"exactly start", start, true},
		{"mid-cycle", start.Add(15 * 24 * time.Hour), true},
		{"one nanosecond before end", end.Add(-time.Nanosecond), true},
		{"exactly end", end, false},
		{"one nanosecond after end", end.Add(time.Nanosecond), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CoversPeriod(start, end, tc.at); got != tc.want {
				t.Fatalf("CoversPeriod(%v) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

// The bounds are computed in UTC because they are absolute instants: an
// anchor created in UTC+7 must produce a boundary that is the same instant,
// not the same wall-clock reading in the operator's zone.
func TestCycleBoundsNormaliseToUTCPreservingTheInstant(t *testing.T) {
	ict := time.FixedZone("ICT", 7*3600)
	anchor := time.Date(2026, 1, 31, 23, 30, 0, 0, ict) // 16:30 UTC on the 31st
	_, end, err := CycleBounds(anchor, 1)
	if err != nil {
		t.Fatalf("CycleBounds returned error: %v", err)
	}
	want := time.Date(2026, 2, 28, 16, 30, 0, 0, time.UTC)
	if !end.Equal(want) {
		t.Fatalf("clipped end = %v, want %v — the anchor's absolute instant was not preserved", end.UTC(), want)
	}
	if _, offset := end.Zone(); offset != 0 {
		t.Fatalf("clipped end carries zone offset %d, want UTC", offset)
	}
}
