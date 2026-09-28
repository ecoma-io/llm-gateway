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
			// Unreachable for a real zone: hour, day and month all
			// advance. It is here because the walk's only exit that is not
			// the range's end is this check, and a walk that cannot advance
			// would otherwise spin for the life of the process.
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
		return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, location)
	case GranularityDay:
		return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	case GranularityCalendarMonth:
		return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, location)
	default:
		return local.UTC()
	}
}

// next returns the start of the bucket after the one starting at start, at
// this grain, in the display zone. The calendar arithmetic is done on the LOCAL reading and
// the result is re-zoned by time.Date's own resolution of the location, which
// is what makes a daylight-saving transition produce a 23- or 25-hour day
// rather than a day that is silently 24 hours of the wrong length.
func next(start time.Time, granularity Granularity, location *time.Location) time.Time {
	local := start.In(location)
	switch granularity {
	case GranularityHour:
		return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, location).Add(time.Hour)
	case GranularityDay:
		// A new calendar day, not "24 hours later": the two differ by an
		// hour twice a year and the difference is the entire reason the
		// bucket carries an end rather than a width.
		return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, location)
	case GranularityCalendarMonth:
		// Day zero of the next month, which time.Date normalises to that
		// month's first, and the start day is always the first so there is
		// nothing to clip: a bucket never starts mid-month.
		return time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, location)
	default:
		return start.Add(time.Hour)
	}
}
