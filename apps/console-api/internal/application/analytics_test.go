package application

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/analytics"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The usage read's boundary, which is four claims in one use case and is the
// only place any of them can be made.
//
// The account is not a parameter, the bounds are refused before a statement is
// built, the store's two halves become one domain answer, and a read that
// finds no derived rows is NOT_AVAILABLE rather than an empty success. None of
// those is visible from inside the domain, and none of them is visible from
// outside the transport either — a handler that handed every request to the
// store would produce the same responses for the requests that are legal. So
// they are asserted here, against ports that record what they were asked and
// refuse to read a database.
//
// The driver below is SCRIPTED rather than a mock with expectations. The claims
// are about what the read asked for — the account it carried, the bounds it
// checked, the deadline it ran under — and a scripted port that records the
// question can assert them exactly, while a mock asserts only that a call
// happened in the order the author already expected.

const (
	analyticsToken   = "console-analytics-token"
	analyticsAccount = identity.AccountID("018f0000-0000-7000-8000-00000000000a")
	// The one token that does not resolve. It is a separate constant rather
	// than a second string literal so that a test cannot accidentally present
	// the working token in a case about an unresolved one.
	unknownToken = "not-a-credential"
)

// errUnresolvedCredential is what a resolver returns for a token it cannot
// place. It carries no account and no hint about what a different token might
// have resolved to, and the use case's job is to refuse rather than to describe.
var errUnresolvedCredential = errors.New("the credential does not resolve to an account")

// scriptedStore is the fake read model. It answers from figures a test wrote
// down, and it records the ONE query it was asked — the account, the bounds and
// the buckets — because every claim below is a claim about that query.
//
// A call that arrives with no deadline is a test failure rather than a zero
// value, and the fake says so by panicking: a fake that ignored a missing
// deadline would pass every test in this file while the bound the whole use
// case exists to state went unenforced. A fake may lie about what it returns;
// it may not lie about what it was handed.
type scriptedStore struct {
	// stored and fresh are the two halves the read is assembled from: the
	// series with the money beside it, and the freshness statement to read it
	// with.
	stored persistence.Usage
	fresh  persistence.Freshness
	err    error

	// asked records the query, and calls how many arrived, so a test can say
	// what the read asked as well as what it returned.
	asked []persistence.UsageQuery
	calls int
}

// Usage implements persistence.Analytics.
func (s *scriptedStore) Usage(ctx context.Context, query persistence.UsageQuery) (persistence.Usage, persistence.Freshness, error) {
	s.asked = append(s.asked, query)
	s.calls++
	if _, ok := ctx.Deadline(); !ok {
		// The read must run under a deadline of its own, because the adapter
		// bounds the same read server-side and the two being equal is the
		// design: a caller told it has five seconds while the connection is
		// held for longer is a caller that believes it has time it does not.
		panic("application: the analytics read reached the store on a context with no deadline; AnalyticsReadTimeout is the bound the caller is told and the adapter's is the one that releases the connection")
	}
	if s.err != nil {
		return persistence.Usage{}, persistence.Freshness{}, s.err
	}
	return s.stored, s.fresh, nil
}

// scriptedScoper resolves exactly one credential and refuses every other one,
// which is the real resolver's shape: a static table, a comparison, and one
// refusal that describes nothing. A scoper that accepted whatever it was given
// would let every other assertion in this file pass while the one property
// that matters — the account is derived from the credential and from nowhere
// else — was never under test.
type scriptedScoper struct {
	account identity.AccountID
	asked   []string
}

var scoped = persistence.RequestScope{AccountID: analyticsAccount}

// ScopeOf implements persistence.Scoper.
func (s *scriptedScoper) ScopeOf(_ context.Context, presented string) (persistence.RequestScope, error) {
	s.asked = append(s.asked, presented)
	if presented != analyticsToken {
		return persistence.RequestScope{}, errUnresolvedCredential
	}
	return scoped, nil
}

// The compile-time proofs that the two fakes stand in for the PORTS and not for
// something narrower, so a port that grew a member fails here rather than in a
// test that quietly stopped covering it.
var (
	_ persistence.Analytics = (*scriptedStore)(nil)
	_ persistence.Scoper    = (*scriptedScoper)(nil)
)

// usageRange is the range every case below asks for: three whole hours at the
// hour grain in UTC, so the answer's final bucket is complete. The figures are
// uneven on purpose — a range every case answered with zeros would leave the
// sum assertions unable to tell a real total from a dropped one.
func usageRange() UsageRequest {
	return UsageRequest{
		From:        time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
		To:          time.Date(2026, time.September, 1, 3, 0, 0, 0, time.UTC),
		Granularity: "hour",
	}
}

// hourBuckets is the store's own series: three buckets whose counts are the
// ones the domain's constructor is expected to sum.
func hourBuckets() []persistence.Bucket {
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	return []persistence.Bucket{
		{Start: start, End: start.Add(time.Hour), WithUsageFacts: 2, Settled: 1},
		{Start: start.Add(time.Hour), End: start.Add(2 * time.Hour), WithUsageFacts: 5, Settled: 2},
		{Start: start.Add(2 * time.Hour), End: start.Add(3 * time.Hour), WithUsageFacts: 2, Settled: 0},
	}
}

// dataThrough is the instant the store's ingestion pass last completed. It is a
// record-time of this plane's own loop and never a completeness claim about the
// runtime, which is why every case below asserts the BASIS beside it.
var dataThrough = time.Date(2026, time.September, 1, 0, 30, 0, 0, time.UTC)

// TestUsageRefusesUnauthenticated is the tenancy rule at the use case, and the
// refusal is the whole claim.
//
// A credential that does not resolve is 401, never 403 and never 404, and the
// message is this package's FIXED one: a credential's failure must not describe
// the accounts it might have named, or a caller learns the credential space by
// probing it. So the assertion covers all three — the code, the fixed message
// and the store never being reached, because an unresolved credential must not
// spend a database round trip to learn nothing.
func TestUsageRefusesUnauthenticated(t *testing.T) {
	store := &scriptedStore{stored: persistence.Usage{Series: hourBuckets()}}
	scoper := &scriptedScoper{account: analyticsAccount}
	use := NewUsageUseCase(store, scoper)

	answer, err := use.Usage(t.Context(), unknownToken, usageRange())

	if err == nil {
		t.Fatalf("Usage() with a credential that does not resolve returned %+v and no error, want a refusal", answer)
	}
	applicationError, ok := As(err)
	if !ok {
		t.Fatalf("Usage() error = %v (%T), want an application Error", err, err)
	}
	if applicationError.Code != CodeUnauthenticated {
		t.Errorf("Code = %q, want %q — the two mean opposite things to a caller: one says \"this exists and is not yours\", the other says \"we could not tell who you are\"",
			applicationError.Code, CodeUnauthenticated)
	}
	// The message is this package's own, byte for byte. An error that carried
	// the resolver's text would carry whatever the resolver said about the
	// token, and that text is the transport's to keep.
	if want := UnresolvedCredential().Error(); applicationError.Error() != want {
		t.Errorf("Error() = %q, want the fixed refusal %q", applicationError.Error(), want)
	}
	if store.calls != 0 {
		t.Errorf("the read reached the store %d times for a credential that does not resolve, want none: an unresolved credential must not spend a database round trip", store.calls)
	}
	if !reflect.DeepEqual(answer, analytics.Usage{}) {
		t.Errorf("Usage() returned %+v beside its refusal, want the zero value: a refused read is not a partial answer", answer)
	}
}

// TestUsageRefusesInvalidRequest is the bounds enforcement, which is a claim
// about the ORDER as much as about the code: the store must not be reached for
// a request whose bounds were refused, so a caller cannot learn what this plane
// holds by asking for a range the plane will not serve.
//
// The four cases are the four ways a request arrives broken, and each one is a
// bound the contract requires: no start, no end, a grain outside the
// vocabulary, and a zone with no calendar. The third and fourth are the domain
// refusals this use case TRANSLATES, so they are the cases that would fail if
// the translation were dropped — a caller would then be told "invalid range" for
// a range that is fine and has a zone that is not.
func TestUsageRefusesInvalidRequest(t *testing.T) {
	tests := []struct {
		name    string
		request func() UsageRequest
		// names is a fragment the message must carry, so the refusal names
		// WHICH bound was broken rather than only that something was.
		names string
	}{
		{
			// A zero time is the year one, so a bound that checked only the
			// range would answer with a forty-thousand-year refusal whose
			// message names a duration nobody asked for. The zero value is a
			// MISSING bound, and it is refused as one.
			name:    "a missing start",
			request: func() UsageRequest { r := usageRange(); r.From = time.Time{}; return r },
			names:   "from",
		},
		{
			// To is the EXCLUSIVE upper bound, so two consecutive ranges tile
			// the timeline with neither overlap nor gap; a caller that leaves it
			// out is asking for a range with no end.
			name:    "a missing end",
			request: func() UsageRequest { r := usageRange(); r.To = time.Time{}; return r },
			names:   "to",
		},
		{
			name:    "a grain outside the vocabulary",
			request: func() UsageRequest { r := usageRange(); r.Granularity = "week"; return r },
			names:   "week",
		},
		{
			// The refusal names the zone rather than the range: a caller who
			// asked for a zone this build cannot resolve cannot repair it by
			// shortening the range.
			name:    "a zone with no calendar",
			request: func() UsageRequest { r := usageRange(); r.Timezone = "Mars/Olympus_Mons"; return r },
			names:   "timezone",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &scriptedStore{stored: persistence.Usage{Series: hourBuckets()}}
			scoper := &scriptedScoper{account: analyticsAccount}
			use := NewUsageUseCase(store, scoper)

			_, err := use.Usage(t.Context(), analyticsToken, tt.request())

			if err == nil {
				t.Fatal("Usage() returned no error for a request whose bounds it does not serve")
			}
			applicationError, ok := As(err)
			if !ok {
				t.Fatalf("Usage() error = %v (%T), want an application Error", err, err)
			}
			if applicationError.Code != CodeInvalidRequest {
				t.Errorf("Code = %q, want %q", applicationError.Code, CodeInvalidRequest)
			}
			if !strings.Contains(applicationError.Error(), tt.names) {
				t.Errorf("Error() = %q, want it to name %q: a caller who was refused has something to act on only if the refusal says which bound to change",
					applicationError.Error(), tt.names)
			}
			if store.calls != 0 {
				t.Errorf("the read reached the store %d times for a request it refused, want none: the bounds are enforced before any statement is built",
					store.calls)
			}
		})
	}
}

// TestUsageRefusesInternalOnDeadline is the read's fail-closed promise, and it
// has two halves that have to be checked separately because a test that only
// checked one would pass through a change that broke the other.
//
// A read that ran out of time is not a zero and not a partial answer: a chart
// drawn from a truncated series would show real activity having stopped at
// whatever instant the deadline landed on. So the refusal carries the budget
// that was exceeded, which is the sentence a server log needs and a client
// never sees. And the deadline must be the one this package declares rather
// than whatever context arrived, because the adapter sets the SAME bound
// server-side and the two being equal is what stops a caller being told it has
// five seconds while its connection is held for longer.
func TestUsageRefusesInternalOnDeadline(t *testing.T) {
	store := &scriptedStore{
		stored: persistence.Usage{Series: hourBuckets()},
		err:    fmt.Errorf("canceling statement due to statement timeout: %w", context.DeadlineExceeded),
	}
	scoper := &scriptedScoper{account: analyticsAccount}
	use := NewUsageUseCase(store, scoper)

	answer, err := use.Usage(t.Context(), analyticsToken, usageRange())

	if err == nil {
		t.Fatalf("Usage() with a read that ran out of time returned %+v and no error, want a refusal", answer)
	}
	applicationError, ok := As(err)
	if !ok {
		t.Fatalf("Usage() error = %v (%T), want an application Error", err, err)
	}
	if applicationError.Code != CodeInternal {
		t.Errorf("Code = %q, want %q — a read that cannot finish in its budget is a failure, and a partial series would read as activity that stopped", applicationError.Code, CodeInternal)
	}
	// The cause is kept, because the server log is where the budget and the
	// instant belong. It is not part of the public message: Internal carries
	// the cause and no Message, so the transport substitutes its own.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the refusal does not unwrap to the deadline that caused it: %v", err)
	}
	if !strings.Contains(err.Error(), AnalyticsReadTimeout.String()) {
		t.Errorf("Error() = %q, want it to name the %s budget the read ran under", err.Error(), AnalyticsReadTimeout)
	}
	if !reflect.DeepEqual(answer, analytics.Usage{}) {
		t.Errorf("Usage() returned %+v beside its refusal, want the zero value: a timed-out read is not a partial answer", answer)
	}
}

// TestUsageReturnsNotAvailableForEmptySeries is the decision the use case owns
// and the store does not, and the case that separates it from an empty success.
//
// A store that returned no buckets at all is one that holds no derived rows for
// that account over that range, which is an ANSWER with a domain type behind it
// and not a failure. The check is on the COUNT and not on the figures, and it
// is made here rather than in the adapter, so "this plane holds nothing for
// that range" is never a bare empty slice a caller has to guess the meaning of.
//
// The freshness still rides along: a caller told "not available" must know how
// far this plane has got, or its next question cannot be a later one.
func TestUsageReturnsNotAvailableForEmptySeries(t *testing.T) {
	store := &scriptedStore{
		stored: persistence.Usage{Series: nil},
		fresh:  persistence.Freshness{DataThrough: dataThrough},
	}
	scoper := &scriptedScoper{account: analyticsAccount}
	use := NewUsageUseCase(store, scoper)

	answer, err := use.Usage(t.Context(), analyticsToken, usageRange())

	if err != nil {
		t.Fatalf("Usage() error = %v, want nil — a range this plane holds no derived rows for is an answer, not a failure", err)
	}
	if answer.Availability != analytics.AvailabilityNotAvailable {
		t.Errorf("Availability = %q, want %q — a caller cannot tell a range this plane holds nothing for from one that was simply quiet unless the field says so",
			answer.Availability, analytics.AvailabilityNotAvailable)
	}
	if answer.Series == nil {
		t.Error("Series is nil, want an empty series: the contract declares an array, and a nil slice is a null in every encoding of it")
	}
	if len(answer.Series) != 0 {
		t.Errorf("Series carries %d points, want none", len(answer.Series))
	}
	if answer.RequestsWithUsageFacts != 0 || answer.RequestsSettled != 0 {
		t.Errorf("the not-available answer counts %d requests with usage facts and %d settled, want zero: the absence of derived rows is not a count of nothing having happened",
			answer.RequestsWithUsageFacts, answer.RequestsSettled)
	}
	if !answer.Freshness.DataThrough.Equal(dataThrough) {
		t.Errorf("Freshness.DataThrough = %s, want the store's %s", answer.Freshness.DataThrough, dataThrough)
	}
	// The store WAS asked: this is an answer, so the refusal-shaped assertions
	// above do not apply and the read has to have happened.
	if store.calls != 1 {
		t.Errorf("the read reached the store %d times, want 1", store.calls)
	}
	assertScopeCarried(t, store, scoper)
}

// TestUsageReturnsAvailableForNonEmptySeries is the other branch, and the case
// that would notice a use case which never took the available path at all.
//
// The series is placed into the DOMAIN's bucket walk rather than the store's, so
// the bounds the contract promised and the bounds the query used are the same
// rule; and the store's two halves — a series of counts and a set of money
// figures — become ONE answer through the domain's constructor, so the envelope
// and the series cannot disagree. Every figure below is therefore checked: a
// total that stayed zero, a balance that was never read, or a point that kept
// the store's instants rather than the domain's would each be a separate answer
// a caller could not reconcile.
func TestUsageReturnsAvailableForNonEmptySeries(t *testing.T) {
	store := &scriptedStore{
		stored: persistence.Usage{
			Series:               hourBuckets(),
			SettledMinorUnits:    4200,
			ReleasedMinorUnits:   700,
			FundsAddedMinorUnits: 100000,
			// Point-in-time, and read rather than re-derived: a balance is a
			// running accumulation of signed leg deltas and never a sum of
			// rounded per-request figures.
			Balances: persistence.Balances{Held: 1500, Available: 97000},
			Capture:  analytics.Capture{Reported: 4, GatewayObserved: 2, ReservationFloor: 1},
		},
		fresh: persistence.Freshness{DataThrough: dataThrough, Basis: analytics.FreshnessFactFeedPass},
	}
	scoper := &scriptedScoper{account: analyticsAccount}
	use := NewUsageUseCase(store, scoper)

	answer, err := use.Usage(t.Context(), analyticsToken, usageRange())

	if err != nil {
		t.Fatalf("Usage() error = %v, want nil", err)
	}
	if answer.Availability != analytics.AvailabilityAvailable {
		t.Fatalf("Availability = %q, want %q for a read that returned a series", answer.Availability, analytics.AvailabilityAvailable)
	}

	// The series, in the order and with the bounds the query used.
	want := hourBuckets()
	if len(answer.Series) != len(want) {
		t.Fatalf("Series carries %d points, want %d", len(answer.Series), len(want))
	}
	for i, point := range answer.Series {
		if point.Start != want[i].Start || point.End != want[i].End {
			t.Errorf("point %d = [%s, %s), want [%s, %s)", i, point.Start, point.End, want[i].Start, want[i].End)
		}
		if point.WithUsageFacts != want[i].WithUsageFacts || point.Settled != want[i].Settled {
			t.Errorf("point %d counts %d with usage facts and %d settled, want %d and %d",
				i, point.WithUsageFacts, point.Settled, want[i].WithUsageFacts, want[i].Settled)
		}
	}
	// The point instants are UTC, because the query stores UTC and a point carrying
	// an instant in the caller's display zone would be a second presentation of a
	// figure that is already an instant.
	for i, point := range answer.Series {
		if point.Start.Location() != time.UTC || point.End.Location() != time.UTC {
			t.Errorf("point %d carries its instants in %s and %s, want UTC for both", i, point.Start.Location(), point.End.Location())
		}
	}

	// The envelope totals are the sum of the series, computed by the domain's
	// constructor rather than here. The expected figures are written out from
	// hourBuckets by hand so that a constructor which stopped summing would be
	// caught by these numbers and not by a restatement of the same sum.
	if answer.RequestsWithUsageFacts != 9 {
		t.Errorf("RequestsWithUsageFacts = %d, want 9 (2 + 5 + 2), the sum of the series", answer.RequestsWithUsageFacts)
	}
	if answer.RequestsSettled != 3 {
		t.Errorf("RequestsSettled = %d, want 3 (1 + 2 + 0), the sum of the series", answer.RequestsSettled)
	}

	// The money and the balances, each read from the store's own half and
	// placed on the answer rather than re-derived from it.
	if answer.SettledMinorUnits != 4200 || answer.ReleasedMinorUnits != 700 || answer.FundsAddedMinorUnits != 100000 {
		t.Errorf("the money came back as settled %d, released %d, funds added %d, want 4200, 700, 100000",
			answer.SettledMinorUnits, answer.ReleasedMinorUnits, answer.FundsAddedMinorUnits)
	}
	if answer.HeldMinorUnits != 1500 || answer.AvailableMinorUnits != 97000 {
		t.Errorf("the balances came back as held %d, available %d, want 1500 and 97000",
			answer.HeldMinorUnits, answer.AvailableMinorUnits)
	}
	if answer.Capture.Total() != 7 {
		t.Errorf("Capture.Total() = %d, want 7 (4 reported + 2 gateway-observed + 1 reservation floor)", answer.Capture.Total())
	}

	// The range is echoed and the freshness is LIFTED rather than invented:
	// the instant is the store's, and the basis beside it is what tells a
	// reader that it is a liveness signal and not a completeness one.
	if answer.Range.From != usageRange().From || answer.Range.To != usageRange().To {
		t.Errorf("Range = [%s, %s), want the range that was asked for, [%s, %s)",
			answer.Range.From, answer.Range.To, usageRange().From, usageRange().To)
	}
	if !answer.Freshness.DataThrough.Equal(dataThrough) {
		t.Errorf("Freshness.DataThrough = %s, want the store's %s", answer.Freshness.DataThrough, dataThrough)
	}
	if answer.Freshness.Basis != analytics.FreshnessFactFeedPass {
		t.Errorf("Freshness.Basis = %q, want %q — a freshness object that did not say which would be read as \"everything up to here is accounted for\" by every caller who saw a date",
			answer.Freshness.Basis, analytics.FreshnessFactFeedPass)
	}

	assertScopeCarried(t, store, scoper)
}

// assertScopeCarried is the tenancy rule, and it is shared by both answer cases
// because it is a claim about the QUERY rather than about the answer.
//
// The account is a result of the use case and never an input, so the only thing
// that can put an account into a statement is the scope the credential
// resolved to. The bounds travel as the query's own, and the buckets are the
// domain's walk rather than the store's: a store that returned them in another
// order could not produce an answer that disagreed with itself, and the bucket
// count is the contract's series bound checked against the walk that built it.
func assertScopeCarried(t *testing.T, store *scriptedStore, scoper *scriptedScoper) {
	t.Helper()

	if len(store.asked) != 1 {
		t.Fatalf("the store was asked %d times, want 1", len(store.asked))
	}
	query := store.asked[0]
	if query.AccountID != string(analyticsAccount) {
		t.Errorf("the read's query carried AccountID %q, want %q — nothing above the use case can name an account, so the credential is the only source of it",
			query.AccountID, analyticsAccount)
	}
	if len(scoper.asked) != 1 || scoper.asked[0] != analyticsToken {
		t.Errorf("the resolver was asked %v, want exactly one call with %q", scoper.asked, analyticsToken)
	}
	// The bounds the caller asked for, and the buckets the DOMAIN cut. A store
	// that received a different range, or a different number of buckets than
	// the walk produces, would answer a question nobody asked.
	if !query.From.Equal(usageRange().From) || !query.To.Equal(usageRange().To) {
		t.Errorf("the read's query carried [%s, %s), want [%s, %s)", query.From, query.To, usageRange().From, usageRange().To)
	}
	walked := analytics.WalkBounds(query.From, query.To, analytics.GranularityHour, time.UTC)
	if len(query.Buckets) != len(walked) {
		t.Fatalf("the read's query carried %d buckets, want the %d the domain's own walk produces", len(query.Buckets), len(walked))
	}
	for i, bucket := range query.Buckets {
		if !bucket.Start.Equal(walked[i].Start) || !bucket.End.Equal(walked[i].End) {
			t.Errorf("bucket %d = [%s, %s), want [%s, %s)", i, bucket.Start, bucket.End, walked[i].Start, walked[i].End)
		}
	}
}
