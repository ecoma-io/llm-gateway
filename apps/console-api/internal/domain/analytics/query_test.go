package analytics

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// analyticsContractPath is the fragment this package answers, relative to this
// package's directory — `go test` runs each package with that directory as its
// working directory. The five levels back are: analytics, domain, internal,
// console-api, apps.
//
// The agreement cannot be typed across the seam: the document is what a
// generated client and any other consumer reads, while this package enforces
// the bound in Go and has no way to be compiled against the yaml. The number
// therefore exists twice, and nothing in either language notices when the two
// copies drift — which is why every behaviour test in this file stays green
// through the drift, because they all measure the same Go constant. The pin
// below is the one test that measures the CODE against the DOCUMENT, and the
// constant's own comment is the promise it keeps.
const analyticsContractPath = "../../../../../api/openapi/shared/analytics.yaml"

// at is a UTC instant written the way a test reads it, because every assertion
// in this file is about where a bucket edge FALLS and a literal built from
// time.Date in the default location would put the expected edge in the test
// runner's zone rather than in the one under test.
func at(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

// midnightIn is a zoned midnight, which is the only form a range can take
// that ends exactly on a day boundary in that zone. The three cases below that
// are about a whole day or a whole day-RANGE need it, and each of them needs it
// for its own reason: two of them are in Europe/London, whose days are 23 and 25
// hours long twice a year, and one is in Asia/Ho_Chi_Minh, whose days are seven
// hours from UTC. A range written in UTC instants is a whole number of days
// long only in UTC, and a case whose name says "a whole day" while its range is
// not one is a case that proves the opposite of what it claims.
//
// The failure message names the zone rather than the library: the standard
// library's zone database is not always installed, and a test that failed on
// its ABSENCE should not read as a defect in the code under test.
func midnightIn(t *testing.T, zone string, year int, month time.Month, day int) time.Time {
	t.Helper()
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("time.LoadLocation(%s) error = %v; the zone database this test reads is not present", zone, err)
	}
	return time.Date(year, month, day, 0, 0, 0, 0, location)
}

// mustQuery builds the query every case below is about, and fails the test
// rather than returning a zero Query: a case that silently ran against an
// unbuilt query would assert on zero bounds and pass.
func mustQuery(t *testing.T, from, to time.Time, granularity Granularity, timezone string) Query {
	t.Helper()
	query, err := NewQuery(from, to, granularity, timezone)
	if err != nil {
		t.Fatalf("NewQuery(%s, %s, %q, %q) error = %v, want nil", from, to, granularity, timezone, err)
	}
	return query
}

// TestNewQueryRefusesTheRangesItCannotServe is the whole bound table in one
// place, and it is the layer's front door: everything else in this package
// assumes a Query exists, so what this refuses is what no query can be.
//
// The three grains are the vocabulary, and each refusal below is about a
// RANGE rather than about a grain, because a grain this build does not know is
// a different error and a different test.
func TestNewQueryRefusesTheRangesItCannotServe(t *testing.T) {
	tests := []struct {
		name    string
		from    time.Time
		to      time.Time
		grain   Granularity
		zone    string
		wantErr error
	}{
		{
			name:    "an end at the start is not a range",
			from:    at(2026, time.September, 1, 0, 0),
			to:      at(2026, time.September, 1, 0, 0),
			grain:   GranularityHour,
			wantErr: ErrInvalidRange,
		},
		{
			name:    "an end before the start is inverted",
			from:    at(2026, time.September, 2, 0, 0),
			to:      at(2026, time.September, 1, 0, 0),
			grain:   GranularityHour,
			wantErr: ErrInvalidRange,
		},
		{
			// One nanosecond past the maximum. A bound tested only at its
			// boundary proves nothing about the half-open decision, and an
			// off-by-one here is a caller refused for a range one nanosecond
			// inside the promise.
			name:    "a range one nanosecond past the maximum",
			from:    at(2026, time.June, 1, 0, 0),
			to:      at(2026, time.June, 1, 0, 0).Add(MaxRange + time.Nanosecond),
			grain:   GranularityHour,
			wantErr: ErrRangeTooLong,
		},
		{
			name:  "a grain outside the vocabulary",
			from:  at(2026, time.September, 1, 0, 0),
			to:    at(2026, time.September, 2, 0, 0),
			grain: Granularity("week"),
			// ErrInvalidGranularity, checked separately below because a grain
			// that does not parse has no bucket arithmetic to run.
			wantErr: ErrInvalidGranularity,
		},
		{
			// The range is legal and the zone is not, and which of the two is
			// reported matters: a caller who named a zone this build cannot
			// resolve has to be told that, because shortening the range would
			// not help them.
			name:    "a zone that does not resolve",
			from:    at(2026, time.September, 1, 0, 0),
			to:      at(2026, time.September, 3, 0, 0),
			grain:   GranularityDay,
			zone:    "Mars/Olympus_Mons",
			wantErr: ErrInvalidTimezone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewQuery(tt.from, tt.to, tt.grain, tt.zone)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("NewQuery(%s, %s, %q, %q) error = %v, want one wrapping %v",
					tt.from, tt.to, tt.grain, tt.zone, err, tt.wantErr)
			}
		})
	}
}

// TestNewQueryServesTheEdgesOfEveryBound is the other half of the table above,
// and it is the half that would catch a bound tightened by accident: the
// maximum range, the empty range's smallest legal size, and the finest grain
// over the longest window are all things the contract promises to serve, and
// each is here as a case that must come back nil.
//
// The maximum range at the finest grain is the one that matters most and the
// one a hand-written guard gets wrong: its hours are a whole number of buckets
// only when it starts on an hour boundary, so a count computed as
// `duration/hours + 2` refuses a range the contract says is servable. The
// unaligned start below is the case that proves the count follows the walk.
func TestNewQueryServesTheEdgesOfEveryBound(t *testing.T) {
	tests := []struct {
		name  string
		from  time.Time
		to    time.Time
		grain Granularity
		zone  string
	}{
		{
			name:  "one nanosecond is a range",
			from:  at(2026, time.September, 1, 0, 0),
			to:    at(2026, time.September, 1, 0, 0).Add(time.Nanosecond),
			grain: GranularityHour,
		},
		{
			name:  "the maximum range at the finest grain, on an hour boundary",
			from:  at(2026, time.June, 1, 0, 0),
			to:    at(2026, time.June, 1, 0, 0).Add(MaxRange),
			grain: GranularityHour,
		},
		{
			// The same maximum, off the hour. This is the case a finest-grain
			// count refuses and the walk does not, and the contract promises it:
			// 90 days is 2160 hours is 2161 hour-buckets when the first one is
			// a partial.
			name:  "the maximum range at the finest grain, unaligned",
			from:  at(2026, time.June, 1, 9, 30),
			to:    at(2026, time.June, 1, 9, 30).Add(MaxRange),
			grain: GranularityHour,
		},
		{
			name:  "the maximum range at the day grain in a zone without daylight saving",
			from:  at(2026, time.June, 1, 9, 30),
			to:    at(2026, time.June, 1, 9, 30).Add(MaxRange),
			grain: GranularityDay,
			zone:  "Asia/Ho_Chi_Minh",
		},
		{
			name:  "the maximum range at the month grain",
			from:  at(2026, time.June, 1, 9, 30),
			to:    at(2026, time.June, 1, 9, 30).Add(MaxRange),
			grain: GranularityCalendarMonth,
		},
		{
			// A fixed offset has a midnight. A zone of the form "Etc/GMT-5"
			// carries no DST RULES, so its offset is the same all year — but
			// that is not the same as having no calendar, and refusing a day
			// bucket in it would refuse the contract's own example
			// (`timezone: UTC` over a month) for no reason a caller could
			// repair. The buckets are exactly 24 hours long, which is what a
			// zone with no transitions can promise.
			name:  "a day in a fixed-offset zone",
			from:  at(2026, time.September, 1, 0, 0),
			to:    at(2026, time.September, 3, 0, 0),
			grain: GranularityDay,
			zone:  "Etc/GMT-5",
		},
		{
			name:  "a calendar month in a fixed-offset zone",
			from:  at(2026, time.September, 1, 0, 0),
			to:    at(2026, time.October, 1, 0, 0),
			grain: GranularityCalendarMonth,
			zone:  "Etc/GMT-5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The acceptance IS the assertion. NewQuery counts the buckets with
			// CountBuckets and refuses over MaxSeriesPoints before it returns,
			// so a nil error says a range whose walk is at most the ceiling was
			// served — and the walk it counted is the walk BucketBounds performs,
			// which the tiling test below holds step for step against the count.
			// Re-deriving the length here from the constructed query would
			// re-assert the same constant twice and prove nothing the acceptance
			// does not already prove.
			if _, err := NewQuery(tt.from, tt.to, tt.grain, tt.zone); err != nil {
				t.Fatalf("NewQuery(%s, %s, %q, %q) error = %v, want nil — the contract promises this range",
					tt.from, tt.to, tt.grain, tt.zone, err)
			}
		})
	}
}

// TestNewQueryNormalisesWhatItStores is the small set of normalisations the
// constructor performs, each of which a caller sees in the echoed range.
//
// A query stores UTC instants whatever zone it was handed in, because the
// instants ARE instants and a comparison of two of them through a presentation
// lens is the whole class of bug the storage decision exists to prevent; and a
// query that named no zone stores the zone it was cut in, because a contract
// that promises a zone and echoes a blank is a contract the caller has to
// guess about.
func TestNewQueryNormalisesWhatItStores(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatalf("time.LoadLocation(Asia/Ho_Chi_Minh) error = %v; the zone database this test reads is not present", err)
	}

	t.Run("instants are stored as UTC whatever zone they arrived in", func(t *testing.T) {
		// Built in the zone rather than in UTC, so a query that stored the
		// instant as it arrived would hold a different time.Time with the
		// same wall clock and a different instant.
		from := time.Date(2026, time.September, 1, 0, 0, 0, 0, loc)
		to := time.Date(2026, time.September, 2, 0, 0, 0, 0, loc)

		query := mustQuery(t, from, to, GranularityHour, "Asia/Ho_Chi_Minh")

		if !query.From.Equal(from) || !query.To.Equal(to) {
			t.Errorf("the query stored [%s, %s), want the same instants as [%s, %s)",
				query.From, query.To, from, to)
		}
		if query.From.Location() != time.UTC || query.To.Location() != time.UTC {
			t.Errorf("the query stored its bounds in %s and %s, want both in UTC", query.From.Location(), query.To.Location())
		}
	})

	t.Run("an unnamed zone is stored as the zone it was cut in", func(t *testing.T) {
		query := mustQuery(t, at(2026, time.September, 1, 0, 0), at(2026, time.September, 2, 0, 0), GranularityHour, "")
		if query.Timezone != "UTC" {
			t.Errorf("Query.Timezone = %q, want the canonical UTC the answer's buckets were cut in", query.Timezone)
		}
		if query.Location() != time.UTC {
			t.Errorf("Query.Location() = %s, want UTC", query.Location())
		}
	})

	t.Run("a named zone is echoed as it was named", func(t *testing.T) {
		query := mustQuery(t, at(2026, time.September, 1, 0, 0), at(2026, time.September, 2, 0, 0), GranularityHour, "Asia/Ho_Chi_Minh")
		if query.Timezone != "Asia/Ho_Chi_Minh" {
			t.Errorf("Query.Timezone = %q, want the caller's own spelling — the response echoes the zone so a reader renders the instants it was handed", query.Timezone)
		}
	})
}

// TestAnUnresolvableZoneIsRefusedRatherThanDefaulted is the asymmetry between
// a zone that was not named and a zone that does not exist.
//
// The first asked for UTC and gets it. The second asked a question this plane
// cannot answer, and answering it in UTC anyway would be a number moving under
// a label the caller chose — the failure the display zone exists to prevent.
func TestAnUnresolvableZoneIsRefusedRatherThanDefaulted(t *testing.T) {
	_, err := NewQuery(at(2026, time.September, 1, 0, 0), at(2026, time.September, 2, 0, 0), GranularityHour, "Mars/Olympus_Mons")
	if !errors.Is(err, ErrInvalidTimezone) {
		t.Errorf("NewQuery() with a zone that does not resolve: error = %v, want one wrapping ErrInvalidTimezone", err)
	}
	// And the same query with a REAL zone returns nil, so the case is about
	// the zone and not about the range or the grain: without this a future
	// edit that refused every zone would pass the test above.
	if _, err := NewQuery(at(2026, time.September, 1, 0, 0), at(2026, time.September, 2, 0, 0), GranularityHour, "Asia/Ho_Chi_Minh"); err != nil {
		t.Errorf("NewQuery() with a zone that does resolve: error = %v, want nil", err)
	}
}

// TestTheProcessZoneIsRefusedRatherThanResolved is the one name in this
// surface's vocabulary that RESOLVES and is still not a zone.
//
// time.LoadLocation accepts "Local" and answers with the zone the server
// PROCESS is running in, read from the host. That is the failure the refusal
// exists to prevent, and it is worse than an unresolvable name rather than
// milder: a caller who sent Mars/Olympus_Mons gets an error, while a caller who
// sent `Local` would have been served a chart whose hour boundaries depend on
// the host's TZ, under a label they cannot reproduce from the answer — the
// response echoes the name it was given, and "Local" resolves to a different
// zone in the caller's own process than in this one.
//
// The premise is asserted rather than assumed, because it is the whole case:
// the loader ACCEPTS this name, so what the test below is about is this
// surface's decision and not the standard library's.
func TestTheProcessZoneIsRefusedRatherThanResolved(t *testing.T) {
	if _, err := time.LoadLocation("Local"); err != nil {
		t.Fatalf("time.LoadLocation(%q) error = %v: this case is about a name the loader accepts and this surface refuses, so a build where the loader rejects it is not exercising the same decision", "Local", err)
	}

	_, err := NewQuery(at(2026, time.September, 1, 0, 0), at(2026, time.September, 2, 0, 0), GranularityHour, "Local")
	if !errors.Is(err, ErrInvalidTimezone) {
		t.Errorf("NewQuery() with the process zone: error = %v, want one wrapping %v; the host's own zone is not a name a caller can resolve back to the same zone, and accepting it would cut the buckets on whichever zone the binary happened to be deployed under", err, ErrInvalidTimezone)
	}
	// And the refusal is about the NAME rather than about zones failing to
	// resolve in general: the same query under a real IANA name still serves.
	if _, err := NewQuery(at(2026, time.September, 1, 0, 0), at(2026, time.September, 2, 0, 0), GranularityHour, "Europe/London"); err != nil {
		t.Errorf("NewQuery() with a real IANA zone: error = %v, want nil", err)
	}
}

// TestBucketBoundsCutTheRangeInTheDisplayZone is the calendar arithmetic, and
// the cases are the three that a division-based implementation gets wrong.
//
// Each one states an edge a caller would chart, in the zone it was asked in.
func TestBucketBoundsCutTheRangeInTheDisplayZone(t *testing.T) {
	tests := []struct {
		name  string
		from  time.Time
		to    time.Time
		grain Granularity
		zone  string
		want  []Bucket
	}{
		{
			name:  "hours are cut on the hour in the display zone",
			from:  at(2026, time.September, 1, 2, 30),
			to:    at(2026, time.September, 1, 5, 0),
			grain: GranularityHour,
			want: []Bucket{
				{Start: at(2026, time.September, 1, 2, 0), End: at(2026, time.September, 1, 3, 0)},
				{Start: at(2026, time.September, 1, 3, 0), End: at(2026, time.September, 1, 4, 0)},
				{Start: at(2026, time.September, 1, 4, 0), End: at(2026, time.September, 1, 5, 0)},
			},
		},
		{
			// The first bucket is the one CONTAINING 02:30, not one clipped to
			// it. A chart whose first bar is half an hour long is a chart of
			// something other than an hour, and a caller summing the widths of
			// the series would be summing something that is not the range they
			// asked for.
			name:  "the first bucket contains the range's start and is not clipped to it",
			from:  at(2026, time.September, 1, 2, 30),
			to:    at(2026, time.September, 1, 3, 30),
			grain: GranularityHour,
			want: []Bucket{
				{Start: at(2026, time.September, 1, 2, 0), End: at(2026, time.September, 1, 3, 0)},
				{Start: at(2026, time.September, 1, 3, 0), End: at(2026, time.September, 1, 4, 0)},
			},
		},
		{
			// 09:30 UTC is 16:30 in this zone, so the bucket CONTAINING it is
			// the local day that began at 17:00 UTC on 1 September — and the
			// walk starts there rather than at the range's start. A day cut by
			// dividing an absolute instant by 24 hours would instead put the
			// first edge at 00:00 UTC, which is 07:00 here: a day that is not a
			// day.
			//
			// The range spans three local midnights, so it yields THREE
			// buckets, the first and last of them partial. That is the
			// behaviour a caller charting three days expects to see, and the
			// reason a range whose hours are not a whole number of days is
			// still rendered as days rather than as hours.
			name:  "a day begins at local midnight, and the range's partial first day is still a day",
			from:  time.Date(2026, time.September, 1, 9, 30, 0, 0, time.UTC),
			to:    time.Date(2026, time.September, 3, 9, 30, 0, 0, time.UTC),
			grain: GranularityDay,
			zone:  "Asia/Ho_Chi_Minh",
			want: []Bucket{
				{Start: at(2026, time.August, 31, 17, 0), End: at(2026, time.September, 1, 17, 0)},
				{Start: at(2026, time.September, 1, 17, 0), End: at(2026, time.September, 2, 17, 0)},
				{Start: at(2026, time.September, 2, 17, 0), End: at(2026, time.September, 3, 17, 0)},
			},
		},
		{
			// February has twenty-eight days and this range crosses it, which
			// is the whole reason the month grain is a calendar and not thirty
			// days. The final bucket is one day long; a thirty-day month would
			// have made it eleven.
			name:  "a calendar month is the zone's month, not thirty days",
			from:  at(2026, time.February, 25, 0, 0),
			to:    at(2026, time.March, 2, 0, 0),
			grain: GranularityCalendarMonth,
			want: []Bucket{
				{Start: at(2026, time.February, 1, 0, 0), End: at(2026, time.March, 1, 0, 0)},
				{Start: at(2026, time.March, 1, 0, 0), End: at(2026, time.April, 1, 0, 0)},
			},
		},
		{
			// A range inside one month yields exactly one bucket, and the
			// bucket is the month. The walk starts at the bucket CONTAINING the
			// start, so a mid-month request gets the whole month — which is what
			// a caller charting a month expects to see.
			name:  "a range inside one month yields that month",
			from:  at(2026, time.March, 10, 0, 0),
			to:    at(2026, time.March, 20, 0, 0),
			grain: GranularityCalendarMonth,
			want: []Bucket{
				{Start: at(2026, time.March, 1, 0, 0), End: at(2026, time.April, 1, 0, 0)},
			},
		},
		{
			// The zone is fixed at +07:00, so an hour boundary in local time is
			// an hour boundary in UTC as well and the two spellings agree —
			// which is why this case is here: it is the CONTROL for the zone
			// arithmetic, and a walk that ignored the zone entirely would
			// produce exactly these bounds. The case that catches the zone
			// being ignored is the one below it.
			name:  "hours in a whole-hour-offset zone are cut on the hour",
			from:  time.Date(2026, time.September, 1, 9, 30, 0, 0, time.UTC),
			to:    time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC),
			grain: GranularityHour,
			zone:  "Asia/Ho_Chi_Minh",
			want: []Bucket{
				{Start: at(2026, time.September, 1, 9, 0), End: at(2026, time.September, 1, 10, 0)},
				{Start: at(2026, time.September, 1, 10, 0), End: at(2026, time.September, 1, 11, 0)},
				{Start: at(2026, time.September, 1, 11, 0), End: at(2026, time.September, 1, 12, 0)},
			},
		},
		{
			// Asia/Kolkata is +05:30 with no daylight saving, so a local hour
			// begins at :30 past the UTC hour. An hour cut by dividing the
			// absolute instant — which is what a UTC-based walk does — puts
			// every edge at :00 UTC and every label reads half an hour off.
			//
			// The bounds below are stated in UTC and look "odd"; that is the
			// point, and the comment beside each says what local hour it is.
			name:  "hours in a half-hour-offset zone are cut on the local hour",
			from:  time.Date(2026, time.September, 1, 9, 30, 0, 0, time.UTC),
			to:    time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC),
			grain: GranularityHour,
			zone:  "Asia/Kolkata",
			want: []Bucket{
				// 15:00 local.
				{Start: at(2026, time.September, 1, 9, 30), End: at(2026, time.September, 1, 10, 30)},
				// 16:00 local.
				{Start: at(2026, time.September, 1, 10, 30), End: at(2026, time.September, 1, 11, 30)},
				// 17:00 local, which is the range's end.
				{Start: at(2026, time.September, 1, 11, 30), End: at(2026, time.September, 1, 12, 30)},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := mustQuery(t, tt.from, tt.to, tt.grain, tt.zone)
			got := query.BucketBounds()

			if len(got) != len(tt.want) {
				t.Fatalf("BucketBounds() = %d buckets, want %d:\n%s", len(got), len(tt.want), renderBuckets(got))
			}
			for i := range got {
				if !got[i].Start.Equal(tt.want[i].Start) || !got[i].End.Equal(tt.want[i].End) {
					t.Errorf("bucket %d = [%s, %s), want [%s, %s)\nall buckets:\n%s",
						i, got[i].Start.UTC().Format(time.RFC3339), got[i].End.UTC().Format(time.RFC3339),
						tt.want[i].Start.UTC().Format(time.RFC3339), tt.want[i].End.UTC().Format(time.RFC3339), renderBuckets(got))
				}
			}
		})
	}
}

// TestADayUnderADaylightTransitionIsTwentyThreeOrTwentyFiveHours is why every
// bucket carries an end rather than a width.
//
// The two days either side of a transition are not 24 hours long, and a bucket
// model that stored a start and a nominal width would report both of them as
// 24. The assertion is on the LENGTH, because that is the number a caller
// would divide a rate by.
func TestADayUnderADaylightTransitionIsTwentyThreeOrTwentyFiveHours(t *testing.T) {
	if _, err := time.LoadLocation("Europe/London"); err != nil {
		t.Fatalf("time.LoadLocation(Europe/London) error = %v; the zone database this test reads is not present", err)
	}

	tests := []struct {
		name       string
		year       int
		month      time.Month
		day        int
		wantLength time.Duration
	}{
		{
			// Spring forward: at 01:00 GMT the clock jumps to 02:00 BST, so
			// the hour from 01:00 to 02:00 does not happen and this London
			// day is 23 hours long.
			name:       "the day the clocks go forward is 23 hours",
			year:       2026,
			month:      time.March,
			day:        29,
			wantLength: 23 * time.Hour,
		},
		{
			// Fall back: at 02:00 BST the clock returns to 01:00 GMT, so the
			// hour from 01:00 to 02:00 happens twice and this day is 25.
			name:       "the day the clocks go back is 25 hours",
			year:       2026,
			month:      time.October,
			day:        25,
			wantLength: 25 * time.Hour,
		},
		{
			name:       "an ordinary day between them is 24 hours",
			year:       2026,
			month:      time.July,
			day:        1,
			wantLength: 24 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The range is ONE LONDON DAY, built as a London date — a range
			// from 00:00Z to 00:00Z is not a day in this zone at all: in July
			// it is one hour of the first of the month and twenty-three of the
			// second, and the bucket it yields is not the day the case is
			// about. AddDate on a zoned instant adds a CALENDAR day, so the
			// range ends on the next London midnight whether that is 23, 24 or
			// 25 hours of real time after it began.
			midnight := midnightIn(t, "Europe/London", tt.year, tt.month, tt.day)
			query := mustQuery(t, midnight, midnight.AddDate(0, 0, 1), GranularityDay, "Europe/London")
			bounds := query.BucketBounds()
			if len(bounds) != 1 {
				t.Fatalf("BucketBounds() over one day returned %d buckets:\n%s", len(bounds), renderBuckets(bounds))
			}
			if got := bounds[0].End.Sub(bounds[0].Start); got != tt.wantLength {
				t.Errorf("the bucket is %s long, want %s — a bucket's length is what a caller divides a rate by, and a nominal 24 hours would be wrong here by an hour",
					got, tt.wantLength)
			}
		})
	}
}

// TestBucketBoundsTileTheRangeWithNoGapAndNoOverlap is the property every
// caller sums over, and it is a property of the SEQUENCE rather than of any
// one bucket.
//
// Two consecutive buckets sharing an edge is the half-open contract; a gap
// would make a caller double-count nothing and lose a bucket, and an overlap
// would make them count one twice. A series assembled by walking these bounds
// is only correct if the walk has this property.
func TestBucketBoundsTileTheRangeWithNoGapAndNoOverlap(t *testing.T) {
	grains := []Granularity{GranularityHour, GranularityDay, GranularityCalendarMonth}
	zones := []string{"", "UTC", "Asia/Ho_Chi_Minh", "Europe/London", "America/New_York"}
	// Ranges chosen to straddle things: a month boundary, a daylight
	// transition, a year boundary, and a range that starts mid-bucket.
	starts := []time.Time{
		at(2026, time.February, 27, 13, 45),
		at(2026, time.March, 29, 0, 0),
		at(2026, time.December, 31, 23, 30),
		at(2026, time.September, 1, 9, 30),
	}

	for _, grain := range grains {
		for _, zone := range zones {
			location, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatalf("time.LoadLocation(%q) error = %v", zone, err)
			}
			for _, start := range starts {
				name := string(grain) + " in " + zone + " from " + start.Format(time.RFC3339)
				t.Run(name, func(t *testing.T) {
					// A range a caller may actually ask for: five days at the
					// hour grain is 120 buckets, at the day grain five, and at
					// the month grain one or two.
					to := start.Add(5 * 24 * time.Hour)
					query := mustQuery(t, start, to, grain, zone)
					bounds := query.BucketBounds()

					if len(bounds) == 0 {
						t.Fatal("BucketBounds() returned no buckets for a non-empty range")
					}
					if !bounds[0].Start.Before(query.From) && !bounds[0].Start.Equal(query.From) {
						t.Errorf("the first bucket begins at %s, after the range's start %s", bounds[0].Start, query.From)
					}
					if bounds[len(bounds)-1].End.Before(query.To) {
						t.Errorf("the last bucket ends at %s, at or before the range's end %s", bounds[len(bounds)-1].End, query.To)
					}
					for i := 1; i < len(bounds); i++ {
						if !bounds[i-1].End.Equal(bounds[i].Start) {
							t.Errorf("buckets %d and %d do not share an edge: %d ends at %s and %d starts at %s",
								i-1, i, i-1, bounds[i-1].End.UTC().Format(time.RFC3339), i, bounds[i].Start.UTC().Format(time.RFC3339))
							break
						}
					}
					for i, bucket := range bounds {
						if !bucket.End.After(bucket.Start) {
							t.Errorf("bucket %d is [%s, %s), which is empty — the walk would not have produced it", i, bucket.Start, bucket.End)
							break
						}
					}
					// The count the contract's bound is written against must be
					// the count the walk produces. This is the assertion that
					// makes CountBuckets safe to use as a gate: it is checked
					// against the walk rather than against itself.
					if counted := CountBuckets(query.From, query.To, grain, location); counted != len(bounds) {
						t.Errorf("CountBuckets() = %d, but BucketBounds() produced %d — a request would be refused against one number and served the other:\n%s",
							counted, len(bounds), renderBuckets(bounds))
					}
				})
			}
		}
	}
}

// TestFinalBucketPartialIsAFactAboutTheRange is the field's whole claim, and
// the two cases either side of it are the ones a naive test would get wrong.
//
// A range ending exactly on a bucket edge is COMPLETE: the last bucket has
// finished filling and refusing to mark it partial would tell a caller the most
// finished answer this surface can give is unstable. A range ending one instant
// earlier is partial, and an empty partial bucket is still partial.
func TestFinalBucketPartialIsAFactAboutTheRange(t *testing.T) {
	tests := []struct {
		name  string
		from  time.Time
		to    time.Time
		grain Granularity
		zone  string
		want  bool
	}{
		{
			name:  "a range ending on the hour boundary is complete",
			from:  at(2026, time.September, 1, 0, 0),
			to:    at(2026, time.September, 1, 3, 0),
			grain: GranularityHour,
			want:  false,
		},
		{
			name:  "a range ending one instant before the boundary is partial",
			from:  at(2026, time.September, 1, 0, 0),
			to:    at(2026, time.September, 1, 3, 0).Add(-time.Nanosecond),
			grain: GranularityHour,
			want:  true,
		},
		{
			name:  "a range ending mid-bucket is partial",
			from:  at(2026, time.September, 1, 0, 0),
			to:    at(2026, time.September, 1, 1, 30),
			grain: GranularityHour,
			want:  true,
		},
		{
			// Two whole days in a zone that is not UTC, and so a range that
			// has to be written in the zone to be two days at all. The same
			// two instants written in UTC — 1 September to 3 September, both
			// at midnight — are not this case: Ho Chi Minh's days begin at
			// 17:00Z, so that range ends inside a third of them and the answer
			// is the one below, not this one.
			name:  "a whole number of days ending on the day boundary is complete",
			from:  midnightIn(t, "Asia/Ho_Chi_Minh", 2026, time.September, 1),
			to:    midnightIn(t, "Asia/Ho_Chi_Minh", 2026, time.September, 3),
			grain: GranularityDay,
			zone:  "Asia/Ho_Chi_Minh",
			want:  false,
		},
		{
			// The same two days written in UTC, in the same zone. A range
			// aligned to the UTC calendar and a range aligned to the display
			// zone's are different ranges, and which one a caller meant is
			// exactly what the zone parameter is for — so this answers
			// differently from the case above with no other difference
			// between them, which is the claim the field exists to make.
			name:  "a range aligned to UTC in a zone whose days are not is partial",
			from:  at(2026, time.September, 1, 0, 0),
			to:    at(2026, time.September, 2, 0, 0),
			grain: GranularityDay,
			zone:  "Asia/Ho_Chi_Minh",
			want:  true,
		},
		{
			// The same range one nanosecond short, in the same zone: the last
			// bucket has begun and has not finished, and an answer that had
			// called this whole would be one that changes retroactively.
			name:  "a whole number of days one instant short is partial",
			from:  midnightIn(t, "Asia/Ho_Chi_Minh", 2026, time.September, 1),
			to:    midnightIn(t, "Asia/Ho_Chi_Minh", 2026, time.September, 3).Add(-time.Nanosecond),
			grain: GranularityDay,
			zone:  "Asia/Ho_Chi_Minh",
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := mustQuery(t, tt.from, tt.to, tt.grain, tt.zone)
			if got := query.FinalBucketPartial(); got != tt.want {
				t.Errorf("FinalBucketPartial() = %v, want %v", got, tt.want)
			}
		})
	}
}

// offsetChanges lists the instants in [from, to) at which the zone's offset
// from UTC changed, found by reading the offset every hour and noting where it
// differs from the reading before. It is how the sweep below finds the
// transitions to walk across without the zone database having to publish them:
// time.Location exposes no transition table, and a test that hard-coded a list
// of dates would be a list of the transitions someone remembered rather than
// the ones the zone database actually holds.
//
// An hour's resolution is enough because the sweep brackets each instant it
// finds by fifty hours in both directions, so a reading that lands up to an
// hour late still straddles the transition it found. The offset is read from
// the instant's own reading rather than from a formatted clock, so a change
// that moves only the offset — a zone renaming itself, or a shift that keeps
// the same wall clock — is still a change here.
func offsetChanges(location *time.Location, from, to time.Time) []time.Time {
	var changes []time.Time
	_, previous := from.In(location).Zone()
	for instant := from.Add(time.Hour); instant.Before(to); instant = instant.Add(time.Hour) {
		if _, offset := instant.In(location).Zone(); offset != previous {
			changes = append(changes, instant.Add(-time.Hour))
			previous = offset
		}
	}
	return changes
}

// TestTheWalkSurvivesEveryTransitionTheZoneDatabaseKnows is the sweep the walk
// needs and the fixtures above could not give it: a range that crosses an
// offset change, in every zone that has one, at every grain.
//
// It exists because the defects it catches are invisible everywhere else. A
// fall-back repeats a local hour and an early spring-forward deletes one, and
// an instant truncated by rebuilding its wall clock through time.Date is a
// CHOICE between the two readings — a choice that can land at or before the
// instant being stepped from. The walk's loop stops when a step does not
// advance, so that is not a wrong bucket but a series that ends at the
// transition: every later bucket missing, the count and the money figures
// describing different windows, and the envelope still reporting the range it
// was asked for. Nothing but a walk across a real transition can see it, which
// is why the assertion is a property of the whole sequence rather than of a
// bucket anyone can write down.
//
// Each zone must contribute at least one transition or the sweep is vacuous
// for it, and the run must produce a four-figure number of buckets overall, so
// a walk that returned nothing at all cannot pass by having nothing to check.
func TestTheWalkSurvivesEveryTransitionTheZoneDatabaseKnows(t *testing.T) {
	// Zones chosen for the shapes a transition takes rather than for coverage
	// of the map: a whole-hour fall-back and spring-forward (London, New York),
	// a spring-forward whose DELETION lands on midnight (Santiago, Havana), a
	// zone whose offset moves by half an hour (Lord Howe) and one whose moves
	// by forty-five minutes (Chatham), and Apia, whose window is 2011 because
	// the transition worth walking there is the calendar day it skipped
	// outright — a date that did not exist at all, which is the extreme form of
	// the local midnight that does not exist.
	//
	// The window is per zone rather than one shared year for exactly that
	// reason: a skipped day is a historical event, and a sweep that could only
	// see this year's transitions could not walk it.
	windows := []struct {
		zone     string
		from, to time.Time
	}{
		{"Pacific/Chatham", at(2026, time.January, 1, 0, 0), at(2027, time.January, 1, 0, 0)},
		{"Europe/London", at(2026, time.January, 1, 0, 0), at(2027, time.January, 1, 0, 0)},
		{"America/New_York", at(2026, time.January, 1, 0, 0), at(2027, time.January, 1, 0, 0)},
		{"America/St_Johns", at(2026, time.January, 1, 0, 0), at(2027, time.January, 1, 0, 0)},
		{"America/Santiago", at(2026, time.January, 1, 0, 0), at(2027, time.January, 1, 0, 0)},
		{"America/Havana", at(2026, time.January, 1, 0, 0), at(2027, time.January, 1, 0, 0)},
		{"Australia/Lord_Howe", at(2026, time.January, 1, 0, 0), at(2027, time.January, 1, 0, 0)},
		{"Australia/Sydney", at(2026, time.January, 1, 0, 0), at(2027, time.January, 1, 0, 0)},
		{"Pacific/Apia", at(2011, time.December, 1, 0, 0), at(2012, time.January, 1, 0, 0)},
	}
	grains := []Granularity{GranularityHour, GranularityDay, GranularityCalendarMonth}
	bucketsWalked := 0

	for _, window := range windows {
		zone := window.zone
		location, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatalf("time.LoadLocation(%s) error = %v; the zone database this test reads is not present", zone, err)
		}
		transitions := offsetChanges(location, window.from, window.to)
		if len(transitions) == 0 {
			t.Errorf("%s changed its offset no times between %s and %s, so the sweep proves nothing about it; drop it from the list or widen the window",
				zone, window.from.Format(time.RFC3339), window.to.Format(time.RFC3339))
			continue
		}

		for _, transition := range transitions {
			// Fifty hours either side is wider than the longest grain of a
			// transition's own effect and narrower than the month grain's
			// buckets, so every grain meets the change with buckets on both
			// sides of it.
			from, to := transition.Add(-50*time.Hour), transition.Add(50*time.Hour)
			for _, grain := range grains {
				name := zone + " " + string(grain) + " across " + transition.In(location).Format(time.RFC3339)
				t.Run(name, func(t *testing.T) {
					bounds := WalkBounds(from, to, grain, location)
					if len(bounds) == 0 {
						t.Fatalf("WalkBounds() returned no buckets for [%s, %s)", from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
					}
					// The bucket CONTAINING the range's start opens the series
					// — the head is never dropped — and the last bucket
					// reaches the range's end, so the walk covers what it was
					// asked about.
					if bounds[0].Start.After(from) {
						t.Errorf("the first bucket begins at %s, after the range's start %s — the bucket containing the start is missing",
							bounds[0].Start.UTC().Format(time.RFC3339), from.UTC().Format(time.RFC3339))
					}
					if last := bounds[len(bounds)-1].End; last.Before(to) {
						t.Errorf("the last bucket ends at %s, before the range's end %s — the walk stopped short:\n%s",
							last.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), renderBuckets(bounds))
					}
					for i, bucket := range bounds {
						width := bucket.End.Sub(bucket.Start)
						if width <= 0 {
							t.Errorf("bucket %d is [%s, %s), which does not advance — a step of zero is where the walk stops, so every bucket after it is missing:\n%s",
								i, bucket.Start.UTC().Format(time.RFC3339), bucket.End.UTC().Format(time.RFC3339), renderBuckets(bounds))
							break
						}
						// At the hour grain a bucket is one absolute hour in
						// every zone, which is the whole reason the hour step
						// is absolute: it is the property that makes a step of
						// zero impossible rather than merely unlikely.
						if grain == GranularityHour && width != time.Hour {
							t.Errorf("bucket %d is %s wide, want exactly one hour in a zone where an hour is an hour:\n%s",
								i, width, renderBuckets(bounds))
							break
						}
						// A day is 23, 24 or 25 hours wherever a zone keeps
						// whole hours, and Lord Howe's half-hour shift puts its
						// 23½-hour day inside the same band.
						if grain == GranularityDay && (width < 22*time.Hour || width > 26*time.Hour) {
							t.Errorf("bucket %d is %s wide, outside the 22 to 26 hours a calendar day can be — a day bucket is a day, not a month or a minute:\n%s",
								i, width, renderBuckets(bounds))
							break
						}
					}
					for i := 1; i < len(bounds); i++ {
						if !bounds[i-1].End.Equal(bounds[i].Start) {
							t.Errorf("buckets %d and %d do not share an edge: %d ends at %s and %d starts at %s",
								i-1, i, i-1, bounds[i-1].End.UTC().Format(time.RFC3339), i, bounds[i].Start.UTC().Format(time.RFC3339))
							break
						}
					}
					if counted := CountBuckets(from, to, grain, location); counted != len(bounds) {
						t.Errorf("CountBuckets() = %d, but WalkBounds() produced %d — the gate and the answer disagree about how many points this range has",
							counted, len(bounds))
					}

					// The walk's own arithmetic, asked directly. Every one of
					// these buckets was TRUNCATED from an instant, and
					// truncation is a choice about which of a zone's readings
					// of one wall time to believe: a rebuilt clock asks
					// time.Date, and time.Date answers with one of the two
					// instants a repeated hour names and with some instant
					// near a skipped one. If the answer is not the instant
					// the bucket that CONTAINS `at` starts at, then the walk
					// above is not the walk this surface makes for any
					// instant in the range — the first bucket is open, so a
					// caller whose range begins mid-transition is served
					// buckets that do not include their own start. So each
					// instant walked above is put to truncate and to next
					// directly, and the pair must bracket it. This is the
					// assertion that failed when an earlier draft truncated
					// the day grain by rewinding a whole day: a rewound day
					// crosses a midnight transition inside its own rewind,
					// and the walk above never saw it because the walk
					// anchors on the truncated instant rather than on
					// every instant inside a bucket.
					for step := from; step.Before(to); step = step.Add(13 * time.Minute) {
						start := truncate(step, grain, location)
						if start.After(step) {
							t.Errorf("the bucket containing %s begins at %s, after the instant it was truncated from; a boundary the zone reads twice can be answered with the wrong one of the two",
								step.In(location).Format(time.RFC3339), start.In(location).Format(time.RFC3339))
							break
						}
						if end := next(start, grain, location); !end.After(step) {
							t.Errorf("the bucket [%s, %s) does not contain the instant %s it was truncated from",
								start.In(location).Format(time.RFC3339), end.In(location).Format(time.RFC3339), step.In(location).Format(time.RFC3339))
							break
						}
					}

					bucketsWalked += len(bounds)
				})
			}
		}
	}

	// The vacuity guard: a walk that produced nothing, or a transition scan
	// that found nothing, would satisfy every assertion above by having
	// nothing to assert about.
	if bucketsWalked < 1000 {
		t.Errorf("the sweep walked %d buckets in total, want the thousands that %d zones' transitions produce — the assertions above prove nothing about a walk this short",
			bucketsWalked, len(windows))
	}
}

func renderBuckets(bounds []Bucket) string {
	out := ""
	for _, bucket := range bounds {
		out += "  [" + bucket.Start.UTC().Format(time.RFC3339) + ", " + bucket.End.UTC().Format(time.RFC3339) + ")\n"
	}
	if out == "" {
		return "  (none)\n"
	}
	return out
}

// TestTheContractSeriesCeilingIsWhatTheDomainEnforces is the pin the constant's
// own comment promises and this file did not have: MaxSeriesPoints is one
// number written twice, enforced here and declared in
// `components.schemas.AnalyticsUsageResponse.properties.series.maxItems`, and
// until this test nothing would have failed if either copy moved alone.
//
// The two copies are not the same document. The yaml is what a generated client
// is built from and what every consumer reads; the constant is what
// NewQuery counts against. A yaml that under-declared the ceiling describes a
// response the plane will produce and the client's own type would call
// impossible — and the drift is invisible in this package, because every other
// test here measures the constant against itself. A constant raised without the
// yaml fails in the other direction: the plane serves a series the contract
// promises cannot exist, and the client has no type to refuse it.
//
// The literal is spelled beside the constant for the reason
// `TestTheContractBoundsAndFloorsAreWhatThisPackageEnforces` spells it in the
// projection package: a reviewed contract change is a deliberate edit to both
// halves, and this row is the line a reviewer reads when deciding whether the
// ceiling may move. It is 2161 and not 2160 for the reason the constant gives —
// an unaligned ninety-day range has 2161 hour-buckets because the first of them
// is partial, and a 2160 bound would refuse every caller who asks for the
// maximum range at the finest grain.
func TestTheContractSeriesCeilingIsWhatTheDomainEnforces(t *testing.T) {
	const key = "components.schemas.AnalyticsUsageResponse.properties.series.maxItems"
	declared, ok := scanContract(t, analyticsContractPath).numbers[key]
	if !ok {
		t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", analyticsContractPath, key)
	}
	if declared != MaxSeriesPoints {
		t.Errorf("%s says %s is %d and this package enforces MaxSeriesPoints = %d; the domain and the contract have drifted, and every other test in this file stays green through the drift because they all measure the same Go constant",
			analyticsContractPath, key, declared, MaxSeriesPoints)
	}
	if MaxSeriesPoints != 2161 {
		t.Errorf("MaxSeriesPoints = %d, want 2161; the document spells the literal, and the number follows from the walk — ninety days is 2160 hours, and an unaligned start adds the partial bucket that contains it", MaxSeriesPoints)
	}
}

// TestTheWalkAndTheCountAgreeAtTheCeilingItself is the second half of what the
// pin above is a pin FOR. A pin that only compares two constants proves the two
// copies agree; it cannot prove the constant is reachable, and a ceiling
// nothing produces is a ceiling no caller is refused against.
//
// So the number is measured rather than assumed: the widest range the surface
// admits, at the finest grain, started OFF the hour, and both the count that
// gates the request and the walk that builds the answer are asked how many
// buckets it has. They must both say MaxSeriesPoints — one more than ninety
// days of hours, because the first bucket is the one CONTAINING 09:30 and
// holds only half an hour. A surface whose ceiling were 2160 would refuse this
// exact range, which is the caller shortening nothing and still being told no.
func TestTheWalkAndTheCountAgreeAtTheCeilingItself(t *testing.T) {
	from := at(2026, time.June, 1, 9, 30)
	to := from.Add(MaxRange)
	location := time.UTC

	if counted := CountBuckets(from, to, GranularityHour, location); counted != MaxSeriesPoints {
		t.Fatalf("CountBuckets() over the widest range the surface admits at the hour grain = %d, want %d — the bound the contract declares is written against this number, and a ceiling no range reaches is a ceiling nothing enforces",
			counted, MaxSeriesPoints)
	}
	query := mustQuery(t, from, to, GranularityHour, "UTC")
	if walked := len(query.BucketBounds()); walked != MaxSeriesPoints {
		t.Errorf("BucketBounds() over the same range = %d buckets, want %d — the walk is what the caller receives and the count is what refused, and one point apart is a response past the declared ceiling",
			walked, MaxSeriesPoints)
	}

	// And one hour shorter is not the ceiling, which is what makes the case
	// above a boundary rather than a formula: a count hard-wired to the
	// maximum range would return 2161 for this too.
	if counted := CountBuckets(from, to.Add(-time.Hour), GranularityHour, location); counted >= MaxSeriesPoints {
		t.Errorf("CountBuckets() over a range an hour shorter = %d, want fewer than %d — the ceiling counts BUCKETS and not hours, so a shorter range is a shorter series",
			counted, MaxSeriesPoints)
	}
}

// contractDocument is the fragment's integer scalars, keyed by dotted path —
// each one a separate key, so a `maxItems` under one schema and a `maxItems`
// under another can never be confused for one another.
type contractDocument struct {
	numbers map[string]int
}

// scanContract reads the fragment's indentation-nested keys.
//
// It is a scanner rather than a parser because Go's standard library has no
// YAML parser and the shape being read is narrow: one integer under one path.
// Keys are pushed and popped by indentation, so the path the `maxItems` is
// recorded against is the one it was written at. Folded description prose falls
// out on its own, since a line is only read as a key when it reads exactly as
// `key: value` with no whitespace inside the key, and a line of prose that
// happens to contain a colon is skipped by the same rule. A document that
// stopped matching the scan yields nothing, and the caller above treats an
// absent key as a failure rather than a skip, so a scanner that goes blind is
// loud instead of vacuous.
func scanContract(t *testing.T, path string) contractDocument {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	document := contractDocument{numbers: map[string]int{}}
	type frame struct {
		indent int
		path   string
	}
	stack := []frame{}

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		key, value, found := strings.Cut(trimmed, ":")
		if !found || key == "" || strings.ContainsAny(key, " \t") {
			continue
		}
		value = strings.TrimSpace(value)

		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parent := ""
		if len(stack) > 0 {
			parent = stack[len(stack)-1].path
		}
		joined := key
		if parent != "" {
			joined = parent + "." + key
		}

		if value == "" {
			// A mapping whose children follow at a deeper indent.
			stack = append(stack, frame{indent: indent, path: joined})
			continue
		}
		if number, err := strconv.Atoi(value); err == nil {
			document.numbers[joined] = number
		}
	}

	if len(document.numbers) == 0 {
		t.Fatalf("%s yielded no integer keys; every pin against it would prove nothing", path)
	}
	return document
}
