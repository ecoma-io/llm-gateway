package commerce

import (
	"fmt"
	"time"
)

// The cycle clock. ADR 0003 fixes the arithmetic so no implementation
// invents it: a period is a calendar month anchored at the subscription's
// start_at — period 1 runs start_at → the same day-of-month one month later
// (clipped to the month's end when the day does not exist), and each later
// period continues from the previous one's end. Cycle membership is
// evaluated against the database clock; the bounds themselves are computed
// here, from whatever instant the roll transaction read inside itself.

// CycleBounds returns the half-open interval [start, end) of one cycle of a
// subscription anchored at startAt. Cycle numbering starts at 1; the bounds
// of cycle n are the clipped month-additions of startAt by n-1 and n, so
// each cycle's start is exactly the previous cycle's end and the intervals
// tile the timeline without gap or overlap.
func CycleBounds(startAt time.Time, cycle int) (time.Time, time.Time, error) {
	if cycle < 1 {
		return time.Time{}, time.Time{}, fmt.Errorf("commerce: cycle bounds: %w: cycle %d starts below 1", ErrInvalidPeriod, cycle)
	}
	start := startAt.UTC()
	periodStart := addCalendarMonthsClipped(start, cycle-1)
	periodEnd := addCalendarMonthsClipped(start, cycle)
	return periodStart, periodEnd, nil
}

// CoversPeriod reports whether at falls inside the half-open interval
// [periodStart, periodEnd). The boundaries are the rule, not an accident of
// comparison: an admission at exactly period_start is in the cycle, an
// admission at exactly period_end is in the next one or in none — capacity
// stops being available to new admissions at period_end (ADR 0003), while
// an allocation reserved before it stays settlement-eligible after it.
func CoversPeriod(periodStart, periodEnd, at time.Time) bool {
	return !at.Before(periodStart) && at.Before(periodEnd)
}

// addCalendarMonthsClipped moves anchor forward by months calendar months,
// keeping the time-of-day and clipping the day-of-month to the target
// month's length: January 31 plus one month is February 28 in a common
// year, February 29 in a leap year, and January 31 plus three months is
// April 30 — never a spill into the month that follows. Everything is
// computed in UTC: the schema's instants are absolute, and a wall-clock
// reading in some operator's zone must never move a commercial boundary.
func addCalendarMonthsClipped(anchor time.Time, months int) time.Time {
	year, month, day := anchor.Date()
	hour, min, sec := anchor.Clock()
	nanosec := anchor.Nanosecond()

	// Day 0 of the month after the target is the target month's last day —
	// time.Date normalizes the overflow, so this is the month's length.
	target := time.Date(year, month+time.Month(months), 1, hour, min, sec, nanosec, time.UTC)
	lastDay := time.Date(target.Year(), target.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(target.Year(), target.Month(), day, hour, min, sec, nanosec, time.UTC)
}
