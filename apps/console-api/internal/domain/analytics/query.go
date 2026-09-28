package analytics

import (
	"fmt"
	"time"
)

// The bounds this surface will answer for, and the reason each is a refusal.
//
// The numbers are not round numbers chosen for taste; each is the natural
// consequence of the grain vocabulary and the storage decision, and stating
// where they come from is what stops the next person from raising one and
// lowering the other without noticing what they did.
const (
	// MaxRange is the longest range this surface will answer for. Ninety
	// days is the longest window any of these figures is useful at, and at
	// the finest grain it is also exactly the longest series MaxSeriesPoints
	// admits — the two bounds coincide, which is what makes the finest grain
	// safe to serve at all.
	MaxRange = 90 * 24 * time.Hour

	// MaxSeriesPoints bounds the points in one series. The contract declares
	// the same number, and the two are one promise written twice — the domain
	// enforces it, the contract states it, and a test below fails if either
	// moves without the other.
	//
	// It is 2161 rather than 2160 because MaxRange is ninety days and
	// NINETY-DAY PLUS ONE HOUR is the longest series the bounds admit, not
	// ninety days: the first bucket of a range is the bucket CONTAINING the
	// range's start, so a caller who asks for 09:00 on 1 June to 09:00 on 30
	// August has 2161 hour-buckets, the first of them holding thirty minutes.
	// A bound of 2160 would refuse that range — and would refuse every
	// unaligned range in the last hour of the maximum window, which is a
	// caller shortening their range by an hour to get past a bound they were
	// never told about. The contract's own reasoning ("90 days at hourly
	// granularity is the longest series this surface will produce") is the
	// error; the arithmetic is what follows from the walk.
	//
	// The coarser grains stay well inside it: ninety days is ninety points at
	// the day grain and four at the month grain, so the number is a real
	// ceiling on the response and not a formality.
	MaxSeriesPoints = 2161
)

// Query is one request for one account's usage over one range at one grain,
// with the account derived rather than named.
//
// There is no AccountID field to set, and that is the security core rather than
// a missing convenience. The account is established by the principal before a
// Query exists, so there is no request-side field to tamper with and no code
// path where a filter is assembled without the scope in it.
type Query struct {
	// From is the INCLUSIVE lower bound of the range, as an absolute
	// instant. Half-open with To, so two consecutive ranges tile the
	// timeline with neither overlap nor gap.
	From time.Time
	// To is the EXCLUSIVE upper bound of the range, as an absolute instant.
	To time.Time
	// Granularity is the width of one point in the series.
	Granularity Granularity
	// Timezone is the IANA zone the bucket edges are cut in. It moves where
	// a bucket BEGINS and nothing else: every figure is a sum of UTC
	// instants, so the same range costs the same amount at every zone.
	//
	// Empty means UTC, which is the only default this surface takes — and
	// the difference between that and the default of a query parameter is
	// that a caller who named no zone asked for UTC, while a caller who
	// named a zone that does not resolve has asked something this plane
	// cannot answer (ErrInvalidTimezone).
	Timezone string
}

// NewQuery binds a validated query. Every bound is enforced here rather than
// at the adapter, so there is exactly one place a caller can be refused and
// the refusal does not depend on which surface asked.
func NewQuery(from, to time.Time, granularity Granularity, timezone string) (Query, error) {
	if _, known := knownGrains[granularity]; !known {
		return Query{}, errInvalidGranularity(string(granularity))
	}
	// The instants are compared in UTC because they ARE UTC instants; a
	// comparison in some presentation zone would be a comparison of two
	// instants through a lens that can move between them, which is the
	// whole class of bug the UTC-storage decision exists to prevent.
	if !to.After(from) {
		return Query{}, fmt.Errorf("%w: [%s, %s) is empty or inverted", ErrInvalidRange,
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	}
	if to.Sub(from) > MaxRange {
		return Query{}, fmt.Errorf("%w: %s is longer than the maximum %s", ErrRangeTooLong,
			to.Sub(from), MaxRange)
	}

	location, err := resolveLocation(timezone)
	if err != nil {
		return Query{}, err
	}
	// The point count is bounded before the walk runs, computed AT THIS
	// GRAIN rather than at the finest one. The two disagree about a range
	// whose hours are not a whole number of buckets — a 90-day range
	// beginning at 09:30 has 2161 of them, and a finest-grain count would
	// refuse a range the contract promises to serve, so the count that gates
	// the answer has to be the count the answer will actually have.
	// WalkBounds measures that number: the same walk BucketBounds performs,
	// counted instead of collected, and the day and month walks cut on the
	// display zone's calendar, so their counts follow the calendar rather
	// than a division by 24.
	//
	// It is a real bound and not a formality, because the zone moves it: at
	// the HOUR grain the width is an hour in every zone, and a range of
	// exactly ninety days yields 2160 buckets everywhere. At the coarser
	// grains the same range yields 90 or 4, and a caller reaching for a
	// wider window is refused by the RANGE bound rather than by this one —
	// which is the message naming which of the two constants to shorten
	// against.
	//
	// Both bounds are constants, so this cannot grow without MaxRange growing
	// with it, and ErrRangeTooLong has already refused anything past
	// MaxRange. The check is still made rather than assumed, because the
	// series is bounded by the contract and a bound no code enforces is a
	// promise only the transport keeps.
	if points := CountBuckets(from, to, granularity, location); points > MaxSeriesPoints {
		return Query{}, fmt.Errorf("%w: a %s range of %s in %s cuts into %d points, more than the %d this surface serves",
			ErrSeriesTooLong, granularity, to.Sub(from), canonicalTimezone(timezone, location), points, MaxSeriesPoints)
	}
	return Query{From: from.UTC(), To: to.UTC(), Granularity: granularity, Timezone: canonicalTimezone(timezone, location)}, nil
}

// Location is the zone the query's buckets are cut in. It is resolved once,
// at construction, so the walk below never has to handle an unresolvable zone
// and an adapter never has to choose one.
func (q Query) Location() *time.Location {
	location, err := resolveLocation(q.Timezone)
	if err != nil {
		// Unreachable: NewQuery resolved the zone and stored its canonical
		// name, and resolveLocation is a pure function of that name. It is
		// here rather than a panic so that a future edit to the canonical
		// form cannot turn a stored string into a crash on the request path.
		return time.UTC
	}
	return location
}

// FinalBucketPartial reports whether the range's end falls INSIDE the last
// bucket, so that bucket has not finished filling and its figures will grow.
//
// It is a fact about the RANGE and not about the data, which is why it is
// computed here rather than inferred from an empty bucket: an empty partial
// bucket is still a partial bucket, and a caller drawing it as a complete zero
// would be drawing a number this plane never claimed. It is also the difference
// between a report a caller may bill from and one they may not — see the
// freshness object's note that nothing in this system closes a period.
func (q Query) FinalBucketPartial() bool {
	bounds := q.BucketBounds()
	if len(bounds) == 0 {
		return false
	}
	// The half-open range is closed at the top when its end IS the last
	// bucket's end, and partial when it falls anywhere inside it. A range
	// that ends exactly on a boundary is the one case that is complete,
	// and it is the case a naive "end != bucket end" test would get wrong
	// by refusing the most finished answer this surface can give.
	return q.To.Before(bounds[len(bounds)-1].End)
}

// Bucket is one point of a series: a half-open interval in the query's zone
// and the figures derived into it.
type Bucket struct {
	Start time.Time
	End   time.Time
}

// BucketBounds cuts the query's range into its buckets, in ascending order,
// with no bucket omitted — including a bucket that has no activity in it, which
// is present with no bounds to fill rather than absent, so a caller renders a
// gap-free series without having to invent a rule for which gaps to fill.
func (q Query) BucketBounds() []Bucket {
	return WalkBounds(q.From, q.To, q.Granularity, q.Location())
}

// WalkBounds is the walk itself, given its four inputs rather than a Query.
//
// It is separate because the number of buckets is needed TWICE and must be the
// same number both times: once before the series exists, to refuse a request
// whose answer would break the contract's bound, and once to build the series
// that answer is made of. Two implementations of one walk would be two
// numbers that can disagree, and a caller that was refused for a range the
// series would have fit — or served a series one point longer than the bound it
// was checked against — would be told something the other number disagrees
// with. There is one walk, called twice, and it is WalkBounds.
//
// The walk is over the DISPLAY zone's calendar and terminates on a check
// rather than on a counter: a counter would have to agree with the calendar
// about how many days a month has, and a calendar month over ninety days is
// between three and four buckets while a day at an hour is ninety times
// twenty-four. The check is the only rule that holds for all three grains
// without each one carrying its own arithmetic.
func WalkBounds(from, to time.Time, granularity Granularity, location *time.Location) []Bucket {
	// The walk starts at the beginning of the bucket CONTAINING the range's
	// start, because a caller who asks for 09:30 wants the 09:00 bucket
	// that contains it. Clipping the first bucket to the range's own start
	// would renumber the series against the calendar, and a chart whose
	// first bar is a half-hour would be a chart of something other than
	// the day.
	first := truncate(from, granularity, location)
	bounds := make([]Bucket, 0, 16)
	for start := first; start.Before(to); {
		end := next(start, granularity, location)
		bounds = append(bounds, Bucket{Start: start, End: end})
		if !end.After(start) {
			// Every arm of next advances — see its own note — so this is
			// expected to be unreachable, and it is a break rather than a
			// continue because a walk that cannot advance must not spin for
			// the life of the process. What it must NOT be is the silent
			// truncation it used to be: an arm that stopped advancing used to
			// end the series at a daylight-saving transition and report the
			// rest of the range as a range with nothing in it, which is why
			// the domain tests now walk every transition the zone database
			// knows about rather than trusting this line.
			break
		}
		start = end
	}
	return bounds
}

// CountBuckets is how many points this range carries at this grain, which is
// the number the contract's series bound is written against. It is a COUNT and
// not a call to WalkBounds for one reason: a request about to be refused should
// not have built the series it is about to refuse, and the walk is the only
// allocation either of them makes.
//
// It is still the same walk, step for step — truncate, then next, until the
// range's end — and the two are held together by the test that asserts the two
// functions agree over every grain and every range a caller may ask for. That
// test is the reason the duplication is safe and not merely small: a walk that
// changed in one place and not the other would be a refusal computed from one
// rule and an answer built from another.
func CountBuckets(from, to time.Time, granularity Granularity, location *time.Location) int {
	points := 0
	for start := truncate(from, granularity, location); start.Before(to); {
		end := next(start, granularity, location)
		points++
		if !end.After(start) {
			break
		}
		start = end
	}
	return points
}

// truncate moves an instant back to the start of the bucket of this grain
// that contains it, in the display zone. A function rather than a Query method
// because the walk that produces the bounds is shared with the count that
// gates them, and neither is a thing a caller asks a query to do.
func truncate(at time.Time, granularity Granularity, location *time.Location) time.Time {
	local := at.In(location)
	switch granularity {
	case GranularityHour:
		// Truncated through the zone rather than by dividing an absolute
		// instant: an hour is an hour long everywhere, but the HOUR a
		// clock reads is a local reading, and a zone whose offset is a
		// half hour would place the boundary at :30 rather than at :00 if
		// the arithmetic were done on the UTC instant instead.
		//
		// It is the instant REWOUND BY THE MINUTES it stands past its local
		// hour rather than a wall clock rebuilt through time.Date, and the
		// difference is the whole reason this is written the long way. An
		// hour that occurs twice — the one a fall-back repeats — makes
		// time.Date's answer a CHOICE between two instants an hour apart, and
		// the one it picks is not necessarily the one `at` fell in: asked for
		// 01:00 on the day London goes back, it answers the second occurrence,
		// so the boundary would land AFTER the instant being truncated and the
		// bucket that contains `at` would be missing from the series. A rewind
		// cannot pass the instant it started from, and it keeps the offset
		// `at` was actually read in, so the bucket it names is the one `at` is
		// in rather than the one it would be in under another offset's clock.
		return at.Add(-time.Duration(local.Minute())*time.Minute -
			time.Duration(local.Second())*time.Second -
			time.Duration(local.Nanosecond()))
	case GranularityDay:
		// The local day's own first instant, asked of the zone through the
		// wall clock that names it and NOT rewound by arithmetic — the
		// opposite choice from the hour arm, and the width is why. A day is
		// long enough for a transition to fall inside the span a rewind takes
		// back, and when one does the rewind lands in the PREVIOUS local day:
		// an instant at 02:05 on the morning London springs forward rewinds by
		// two local hours into 23:00 of the day before, which is a boundary
		// that does not contain the instant it was asked for. The fast path is
		// taken only when the rebuilt midnight is BOTH at or before `at` and
		// the first instant of the date it names — the second test is what
		// separates a midnight that is really where the date begins from one
		// the zone skipped (which resolves to an instant still in the previous
		// local day, an hour short of where the date begins) and from one a
		// fall-back names twice (where the rebuild may answer with the second
		// occurrence).
		onDate := onSameDate(at, location)
		midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
		if !midnight.After(at) && startsSpan(midnight, onDate) {
			return midnight
		}
		// The zone's own answer it is, then: the first instant whose local date
		// is this one, found from the instant being truncated, which is on that
		// date by definition.
		return firstInside(at, 30*time.Hour, onDate)
	case GranularityCalendarMonth:
		// The same shape as the day arm, one month wide, and the same two-part
		// fast path: the first of the month is a midnight a zone can skip or
		// repeat exactly as any other is.
		onMonth := onSameMonth(at, location)
		first := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, location)
		if !first.After(at) && startsSpan(first, onMonth) {
			return first
		}
		return firstInside(at, 32*24*time.Hour, onMonth)
	default:
		return local.UTC()
	}
}

// onSameDate reports whether an instant falls on the same local calendar date
// as the reference instant, in the display zone. It is the definition of the
// day grain's span, written once and passed to the boundary helpers rather than
// recomputed beside them, because a search and its fast-path test disagreeing
// about what a day IS would produce a bucket whose start and end are days
// under two different rules.
func onSameDate(reference time.Time, location *time.Location) func(time.Time) bool {
	at := reference.In(location)
	return func(candidate time.Time) bool {
		other := candidate.In(location)
		return other.Year() == at.Year() && other.Month() == at.Month() && other.Day() == at.Day()
	}
}

// onSameMonth is onSameDate for the month grain: the span is the local calendar
// month, which is the day rule lifted one field.
func onSameMonth(reference time.Time, location *time.Location) func(time.Time) bool {
	at := reference.In(location)
	return func(candidate time.Time) bool {
		other := candidate.In(location)
		return other.Year() == at.Year() && other.Month() == at.Month()
	}
}

// startsSpan reports whether an instant is the FIRST instant of the span it
// falls in — whether it is a calendar boundary rather than a moment inside one.
//
// The test is one nanosecond deep and that is the whole of it: an instant opens
// a span exactly when the instant before it belongs to a different one. It is
// here because a rebuilt wall clock cannot answer the question by itself. The
// zone database resolves a local midnight that does not exist to some instant
// near it and a midnight that exists twice to one of the two, and neither
// answer is a statement about which instant the date BEGINS at; asking the
// clock what it read a nanosecond earlier is.
func startsSpan(at time.Time, inside func(time.Time) bool) bool {
	return inside(at) && !inside(at.Add(-time.Nanosecond))
}

// firstInside returns the earliest instant in [limit-span, limit] that
// inside reports true for, and is the backward half of the boundary arithmetic:
// it answers "where did this span BEGIN", where boundaryAfter answers "where did
// it END". The caller guarantees the predicate holds at limit — it is the
// instant whose own span is being found — and the window is generous by
// construction, so the search is only ever reached for the transitions.
func firstInside(limit time.Time, span time.Duration, inside func(time.Time) bool) time.Time {
	lo, hi := limit.Add(-span), limit
	for hi.Sub(lo) > time.Nanosecond {
		mid := lo.Add(hi.Sub(lo) / 2)
		if inside(mid) {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi
}

// next returns the start of the bucket after the one starting at start, at
// this grain, in the display zone.
//
// EVERY ARM ADVANCES, and that is a property the walk depends on rather than a
// happy accident: the walk's loop exits on the range's end, and its only other
// exit is the guard against a step that does not move. An arm that returned
// `start` would therefore not merely add a degenerate bucket — it would END the
// series there, and a fall-back is exactly where a naive calendar step stops
// moving.
//
// The hour arm is absolute and the day and month arms are the zone's own
// boundary, and the difference is not an inconsistency: a clock hour is a fixed
// span in every zone, so the hour's end is arithmetic, while a calendar day is
// a set of instants the ZONE decides the edges of. Asking a zone database
// "which instant begins the next date" by rebuilding a wall clock is only
// correct when that wall clock names exactly one instant, and midnight is
// precisely where it may not: it can be skipped by a spring-forward, and it can
// be named twice by a fall-back. So the calendar arms ask the zone instead —
// see boundaryAfter — and time.Date's answer is used only as the fast path when
// it is already provably on the far side of the edge.
func next(start time.Time, granularity Granularity, location *time.Location) time.Time {
	local := start.In(location)
	switch granularity {
	case GranularityHour:
		// An hour is an hour long in every zone, so the step is absolute and
		// the calendar is consulted only where the walk is anchored. Deriving
		// it from the wall clock instead would ask time.Date to resolve an
		// hour that a fall-back makes ambiguous, and its answer is as likely
		// to be the occurrence BEFORE the one stepped from as the one after —
		// which is a step of zero, a series that ends at the transition, and a
		// range whose remaining days are reported as nothing happening.
		return start.Add(time.Hour)
	case GranularityDay:
		// A new calendar day, not "24 hours later": the two differ by an hour
		// twice a year and the difference is the entire reason the bucket
		// carries an end rather than a width.
		onDate := onSameDate(start, location)
		candidate := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, location)
		if candidate.After(start) && startsSpan(candidate, onDate) {
			return candidate
		}
		// The fast path is unavailable, which means the wall clock naming the
		// next midnight resolved to an instant that is not where the next date
		// begins: Santiago and Havana spring forward at midnight, and there
		// time.Date answers with an instant still inside the day being closed,
		// an hour short of the transition — a day that ends before it is over,
		// whose last hour is counted in no bucket at all. A whole day is not
		// the answer either: 24 hours from here is right by accident where the
		// day really is 23 or 25 hours long. The boundary belongs to the zone,
		// so the zone is asked for it.
		return boundaryAfter(start, 30*time.Hour, onDate)
	case GranularityCalendarMonth:
		// Day zero of the next month, which time.Date normalises to that
		// month's first, and the start day is always the first so there is
		// nothing to clip: a bucket never starts mid-month.
		onMonth := onSameMonth(start, location)
		candidate := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, location)
		if candidate.After(start) && startsSpan(candidate, onMonth) {
			return candidate
		}
		// The same two ways a rebuilt first-of-the-month can fail as a
		// midnight can — a zone can skip it or name it twice — and the same
		// answer, because a month is a calendar span like any other. The
		// window is a month plus two days, which no month length reaches.
		return boundaryAfter(start, 32*24*time.Hour, onMonth)
	default:
		return start.Add(time.Hour)
	}
}

// boundaryAfter returns the first instant after start whose local reading the
// zone places outside the calendar span `inside` describes — the end of the day
// or of the month start is in.
//
// It exists because a calendar boundary is a fact about a zone and not a
// formula. The span is asked of the zone's own clock, one instant at a time,
// and the answer is exact to the nanosecond rather than to whatever offset a
// rebuilt wall clock happened to resolve to. `span` bounds the search: a local
// date is at most a transition's own width away, and the widest a zone has ever
// moved a calendar is the day Samoa skipped, so the two callers' windows are
// generous by construction. Only the transitions reach this at all — every
// ordinary day and every ordinary month takes its caller's fast path — so the
// search's cost is off the series' hot path by design.
func boundaryAfter(start time.Time, span time.Duration, inside func(time.Time) bool) time.Time {
	// `inside(start)` is true by the caller's own definition and the window's
	// far end is outside the span by construction, so the search is bracketed
	// before it takes a single step: the invariant below is that the predicate
	// is true at lo and false at hi.
	lo, hi := start, start.Add(span)
	for hi.Sub(lo) > time.Nanosecond {
		mid := lo.Add(hi.Sub(lo) / 2)
		if inside(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi
}
