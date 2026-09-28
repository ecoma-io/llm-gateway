package analytics

import (
	"reflect"
	"testing"
	"time"
)

// The two halves of the answer, and the arithmetic that keeps them from
// drifting apart.
//
// NewUsage is the ONLY construction path for an available answer, and the
// reason it exists is that two independent totals for one question are two
// numbers that can disagree. Nothing else in the plane may hand a caller a
// series and an envelope figure that were computed separately, so the claims
// below are about the constructor's two promises: the counts ARE the sum of the
// series, and the balances are NOT in the series at all.

// usageQuery is the range every case below answers for: three whole hours at
// the hour grain in UTC, so the series is three buckets and the final bucket is
// NOT partial. Whether a range's end lands inside its last bucket is a fact
// about the range and is query_test.go's claim; nothing here is about it.
func usageQuery(t *testing.T) Query {
	t.Helper()
	query, err := NewQuery(at(2026, time.September, 1, 0, 0), at(2026, time.September, 1, 3, 0), GranularityHour, "")
	if err != nil {
		t.Fatalf("NewQuery() error = %v, want nil", err)
	}
	return query
}

// hourPoints is three hour-long buckets carrying figures chosen so that a
// partial sum cannot be mistaken for the whole.
//
// WithUsageFacts rises while Settled does not, which is the ordinary shape of a
// window holding requests that were recorded and requests that were merely
// priced. The SECOND bucket is the one that matters: a constructor that reset
// its accumulator per point, or that read only the first figure off each point,
// lands on 2 and 1 here rather than on 9 and 3.
func hourPoints() []Point {
	start := at(2026, time.September, 1, 0, 0)
	return []Point{
		{Start: start, End: start.Add(time.Hour), WithUsageFacts: 2, Settled: 1},
		{Start: start.Add(time.Hour), End: start.Add(2 * time.Hour), WithUsageFacts: 5, Settled: 2},
		{Start: start.Add(2 * time.Hour), End: start.Add(3 * time.Hour), WithUsageFacts: 2, Settled: 0},
	}
}

var hourCapture = Capture{Reported: 4, GatewayObserved: 2, ReservationFloor: 1}

var hourFreshness = Freshness{
	DataThrough: at(2026, time.September, 1, 0, 30),
	Basis:       FreshnessFactFeedPass,
}

// TestNewUsageSumsSeriesIntoTopLevelFields is the constructor's central claim:
// the envelope's counts are the sum of the series, computed once, so a chart
// drawn from the series and a figure read off the envelope cannot disagree.
//
// The sum is asserted against figures computed HERE rather than against literals
// written beside them. A literal per point and a literal for the total are two
// sets of numbers a future edit to either can be made to match by accident; a
// sum the test itself performs cannot be wrong that way.
func TestNewUsageSumsSeriesIntoTopLevelFields(t *testing.T) {
	query := usageQuery(t)
	series := hourPoints()

	answer := NewUsage(query, series, hourCapture, hourFreshness)

	var wantWithFacts, wantSettled int64
	for _, point := range series {
		wantWithFacts += point.WithUsageFacts
		wantSettled += point.Settled
	}
	if answer.RequestsWithUsageFacts != wantWithFacts {
		t.Errorf("RequestsWithUsageFacts = %d, want the sum of the series, %d", answer.RequestsWithUsageFacts, wantWithFacts)
	}
	if answer.RequestsSettled != wantSettled {
		t.Errorf("RequestsSettled = %d, want the sum of the series, %d", answer.RequestsSettled, wantSettled)
	}
	// And both sums are non-zero, because a sum that is zero because the
	// constructor summed nothing is precisely the defect this file exists to
	// catch and a pair of zero assertions would not have noticed.
	if answer.RequestsWithUsageFacts == 0 || answer.RequestsSettled == 0 {
		t.Errorf("the series sums to %d requests with usage facts and %d settled, want figures a caller can read: a constructor that dropped the sum entirely would satisfy two zero comparisons",
			answer.RequestsWithUsageFacts, answer.RequestsSettled)
	}

	// The rest of the answer is carried, not recomputed: the query it answers
	// and the two halves it was handed.
	if answer.Range != query {
		t.Errorf("Range = %+v, want the query it answers, %+v", answer.Range, query)
	}
	if answer.Capture != hourCapture {
		t.Errorf("Capture = %+v, want %+v carried through", answer.Capture, hourCapture)
	}
	if answer.Freshness != hourFreshness {
		t.Errorf("Freshness = %+v, want %+v carried through", answer.Freshness, hourFreshness)
	}
	if len(answer.Series) != len(series) {
		t.Errorf("Series carries %d points, want the %d it was given — the constructor sums the series and never rebuilds it", len(answer.Series), len(series))
	}
	for i, point := range answer.Series {
		if point.Start != series[i].Start || point.End != series[i].End {
			t.Errorf("point %d = [%s, %s), want [%s, %s)", i, point.Start, point.End, series[i].Start, series[i].End)
		}
	}
}

// TestNewUsageKeepsTheBalancesOutOfTheSeries is the half of the design a
// "helpful" arithmetic change breaks.
//
// Held and Available are POINT-IN-TIME balances as at the instant the READ
// ran, and Point carries no field for either. That omission IS the claim: a
// constructor that grew a per-bucket balance, or that multiplied the account's
// total across the points, would put the same as-at-the-read figure on every
// bucket with the implication that it was true of each — a per-bucket reading
// of a per-account figure, and the most likely way a caller would misread one
// (the instant is the read's and not the range's, which is §10.5 of the
// architecture note: the projection caches the present and keeps no history).
// So the point's own shape is pinned: it is two instants and two counts, and a
// fifth money field would be invisible to every assertion above.
func TestNewUsageKeepsTheBalancesOutOfTheSeries(t *testing.T) {
	answer := NewUsage(usageQuery(t), hourPoints(), hourCapture, hourFreshness)

	// Every money figure the constructor was NOT given is zero: it sums the
	// counts and nothing else, so a constructor that also produced money could
	// only have invented it.
	if answer.SettledMinorUnits != 0 || answer.ReleasedMinorUnits != 0 ||
		answer.FundsAddedMinorUnits != 0 || answer.HeldMinorUnits != 0 || answer.AvailableMinorUnits != 0 {
		t.Errorf("NewUsage() produced money figures it was not given: settled %d, released %d, funds added %d, held %d, available %d — the constructor sums counts; the money is read from the store and assigned to the answer it built",
			answer.SettledMinorUnits, answer.ReleasedMinorUnits, answer.FundsAddedMinorUnits, answer.HeldMinorUnits, answer.AvailableMinorUnits)
	}

	fields := reflect.TypeFor[Point]()
	var names []string
	for i := range fields.NumField() {
		names = append(names, fields.Field(i).Name)
	}
	if got, want := len(names), 4; got != want {
		t.Errorf("analytics.Point carries %d fields (%v), want %d (Start, End, WithUsageFacts, Settled): the balances are point-in-time figures as at the read's own instant and belong in exactly one place in the answer, so a per-bucket copy of them is the misreading the type exists to prevent",
			got, names, want)
	}
}

// TestNewUsageAvailableForNonEmptySeries is the other half of the two-axis
// design, and the case that separates "there is an answer" from "the answer is
// a failure".
//
// A series that holds a bucket whose figures are ALL ZERO is still an available
// answer, and it is the case a naive implementation gets wrong: a range that
// was quiet and a range this plane holds nothing for would both come back
// empty, and only the field tells them apart. So the second point below is
// deliberately a zero figure rather than a non-zero one.
func TestNewUsageAvailableForNonEmptySeries(t *testing.T) {
	start := at(2026, time.September, 1, 0, 0)
	series := []Point{
		{Start: start, End: start.Add(time.Hour), WithUsageFacts: 3, Settled: 2},
		{Start: start.Add(time.Hour), End: start.Add(2 * time.Hour)},
	}

	answer := NewUsage(usageQuery(t), series, hourCapture, hourFreshness)

	if answer.Availability != AvailabilityAvailable {
		t.Errorf("Availability = %q, want %q — a series is an answer even when every figure in it is zero", answer.Availability, AvailabilityAvailable)
	}
	if len(answer.Series) != 2 {
		t.Fatalf("Series carries %d points, want the 2 it was given", len(answer.Series))
	}
	// The quiet bucket contributes to the totals and does not make the answer
	// unavailable, so the totals are the first bucket's figures alone.
	if answer.RequestsWithUsageFacts != 3 || answer.RequestsSettled != 2 {
		t.Errorf("the answer counts %d requests with usage facts and %d settled, want 3 and 2 — a quiet bucket contributes zero and does not make the range unavailable",
			answer.RequestsWithUsageFacts, answer.RequestsSettled)
	}
}

// TestNewUsageNotAvailableForEmptySeries is the OTHER construction path, and
// the two properties that make it a COMPLETE answer rather than an empty
// successful one.
//
// A caller has to be able to tell "this plane holds nothing for that range" from
// "that range was quiet", and Availability is the only thing that tells them.
// So both halves are checked together: the series is empty AND the field says
// why, because an empty series under an available Availability is exactly the
// answer this design rules out.
func TestNewUsageNotAvailableForEmptySeries(t *testing.T) {
	query := usageQuery(t)

	answer := NotAvailable(query, hourFreshness)

	if answer.Availability != AvailabilityNotAvailable {
		t.Errorf("Availability = %q, want %q — a caller cannot tell a range this plane holds nothing for from one that was simply quiet unless the field says so", answer.Availability, AvailabilityNotAvailable)
	}
	// An EMPTY SERIES, not a nil one. The two encode differently — a nil slice
	// is a JSON null — and the contract's series is an array.
	if answer.Series == nil {
		t.Error("Series is nil, want an empty series: the contract declares an array, and a nil slice is a null in every encoding of it")
	}
	if len(answer.Series) != 0 {
		t.Errorf("Series carries %d points, want none", len(answer.Series))
	}

	// Every figure is zero, and zero here means ABSENCE rather than a count of
	// zero activity — which is the whole reason the field exists.
	if answer.RequestsWithUsageFacts != 0 || answer.RequestsSettled != 0 {
		t.Errorf("the not-available answer counts %d requests with usage facts and %d settled, want zero: the absence of derived rows is not a count of nothing having happened",
			answer.RequestsWithUsageFacts, answer.RequestsSettled)
	}
	if answer.SettledMinorUnits != 0 || answer.ReleasedMinorUnits != 0 ||
		answer.FundsAddedMinorUnits != 0 || answer.HeldMinorUnits != 0 || answer.AvailableMinorUnits != 0 {
		t.Error("the not-available answer carries money figures, want none: this plane holds no derived rows for the range, which is not a statement that nothing was spent in it")
	}
	if answer.Capture.Total() != 0 {
		t.Errorf("the not-available answer splits %d settlements that captured, want none: a capture rate over a range this plane holds nothing for would divide by a zero that is an absence rather than a measurement", answer.Capture.Total())
	}

	// The range and the freshness are still carried. A caller told "not
	// available" must still know WHICH range it asked about and how far this
	// plane has got, or its next question cannot be a later one.
	if answer.Range != query {
		t.Errorf("Range = %+v, want the query it answers, %+v", answer.Range, query)
	}
	if answer.Freshness != hourFreshness {
		t.Errorf("Freshness = %+v, want %+v", answer.Freshness, hourFreshness)
	}
}
