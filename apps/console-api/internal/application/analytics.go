package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/analytics"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// AnalyticsReadTimeout is the deadline one usage read runs under, and it is
// equal to the statement timeout the adapter sets (analyticsReadBound in
// adapters/outbound/postgres/analytics.go).
//
// The two being equal is the whole design. The server-side bound is what
// releases the connection; the application-side one is what the caller is
// told. A server bound LONGER than this would be dead weight — the context is
// cancelled first and the connection is held until PostgreSQL notices — and a
// server bound SHORTER would make a read fail as a timeout while the caller
// still believed it had time left.
//
// A report that cannot finish in five seconds fails CLOSED: it is a refusal,
// not a partial answer. The alternative — returning the series without the
// money, or the money without the balances — is an answer whose parts were
// read at different instants and cannot be reconciled by whoever receives it.
const AnalyticsReadTimeout = 5 * time.Second

// Usage is the one read the analytics surface serves, and it is a use case
// rather than a passthrough for four reasons that are all visible only here:
//
//   - THE SCOPE IS RESOLVED HERE, from a credential, and never taken as a
//     parameter. Nothing above this line can name an account, so nothing above
//     this line can be handed a request to read somebody else's spend. The
//     account goes into the query, and the query is the only thing that ever
//     sees it.
//   - THE BOUNDS ARE ENFORCED HERE, on the domain type, before any statement
//     is built. A range that is a year, a grain this build does not serve, a
//     zone with no calendar — each is refused with a message naming the bound,
//     because a caller who asked for a year and was refused has something to
//     do about it.
//   - THE STORE'S TWO HALVES BECOME ONE ANSWER. The store returns a series of
//     counts and a set of money figures; the domain's constructor is the only
//     place that sums them, so the envelope and the series cannot disagree.
//   - A READ FOR AN ACCOUNT THIS PLANE HOLDS NO DERIVED ROW FOR IS
//     NOT_AVAILABLE, and is assembled here rather than by the store, so "this
//     plane has never ingested this account" is a domain answer with a domain
//     type and never a zero-filled series that a caller has to guess the
//     meaning of. What makes it possible is that the store reports its own
//     coverage beside the freshness instant; see the decision in Usage.
type Usage struct {
	analytics persistence.Analytics
	scoper    persistence.Scoper
}

// NewUsageUseCase builds the read around its two ports. It panics on a nil
// port for the reason every constructor in this package does: a port promised
// and not delivered is a wiring defect, and learning about it in the middle of
// an HTTP request is a strictly worse place than learning about it at start.
func NewUsageUseCase(readModel persistence.Analytics, scoper persistence.Scoper) *Usage {
	switch {
	case readModel == nil:
		panic("application: NewUsageUseCase requires the analytics read model")
	case scoper == nil:
		panic("application: NewUsageUseCase requires a scope resolver")
	}
	return &Usage{analytics: readModel, scoper: scoper}
}

// UsageRequest is one caller's question: the range, the grain, and the zone to
// display it in. It deliberately has NO field that could carry an account.
//
// The account is not absent by accident and it is not "added later": a field
// here would be a filter a caller could tamper with, and a statement that
// accepted it would be indistinguishable in review from one that could not.
// The scope arrives from the credential and from nowhere else.
//
// From, To and Granularity are required rather than defaulted, and the zero
// values are how a missing one arrives. The contract's reason is the one this
// type follows: a range that defaults to "the last thirty days" answers a
// question nobody asked, and a caller cannot tell a default from a choice.
type UsageRequest struct {
	// From and To are the half-open range [From, To), both RFC 3339 instants
	// in the transport. The zero value is a MISSING bound, not an instant
	// before the calendar — a handler that parsed "" into the zero time and
	// passed it here would produce a range of forty years and a refusal the
	// caller could not act on.
	From time.Time
	To   time.Time
	// Granularity is the raw grain string from the request. It is parsed here
	// rather than in the transport so that the refusal names the vocabulary the
	// caller could have used, in the use case that owns the answer.
	Granularity string
	// Timezone is the display zone. An empty value means UTC, and is recorded
	// as UTC in the answer so a caller that never asked for a zone is not left
	// guessing which one its buckets were cut in. This is the ONLY defaulted
	// field, because UTC is not an answer nobody asked for: it is where every
	// figure is stored, so a caller that named no zone named the store.
	Timezone string
}

// Usage returns the account the credential speaks for. The account is a
// RESULT of this method, never an input: the caller learns which account it
// was answered for, and cannot have asked for another one.
func (use *Usage) Usage(ctx context.Context, token string, request UsageRequest) (analytics.Usage, error) {
	scope, err := use.scoper.ScopeOf(ctx, token)
	if err != nil {
		// Every unresolved credential is one refusal, and the transport turns
		// it into 401 without a body that describes the credential space.
		return analytics.Usage{}, UnresolvedCredential()
	}

	query, err := newAnalyticsQuery(request)
	if err != nil {
		return analytics.Usage{}, err
	}

	// The deadline is applied AFTER the scope resolves and the query
	// validates, because neither of those is a database operation and neither
	// should spend a report's time budget on work a report does not need.
	readCtx, cancel := context.WithTimeout(ctx, AnalyticsReadTimeout)
	defer cancel()

	stored, freshness, err := use.analytics.Usage(readCtx, persistence.UsageQuery{
		AccountID: string(scope.AccountID),
		From:      query.From,
		To:        query.To,
		Buckets:   query.BucketBounds(),
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// A read that ran out of time is not a zero and not a partial
			// answer. It is a failure, and the failure is the honest one: a
			// chart drawn from a truncated series would show real activity
			// having stopped at whatever instant the deadline landed on.
			return analytics.Usage{}, Internal(fmt.Errorf("the analytics read exceeded its %s budget: %w", AnalyticsReadTimeout, err))
		}
		return analytics.Usage{}, Internal(fmt.Errorf("read the analytics usage: %w", err))
	}

	// The series is placed into the domain's own bucket walk rather than the
	// store's: the bounds the contract promised and the bounds the query used
	// are then the same rule, and a store that returned them in a different
	// order could not produce an answer that disagreed with itself.
	series := make([]analytics.Point, 0, len(stored.Series))
	for _, bucket := range stored.Series {
		series = append(series, analytics.Point{
			Start:          bucket.Start.UTC(),
			End:            bucket.End.UTC(),
			WithUsageFacts: bucket.WithUsageFacts,
			Settled:        bucket.Settled,
		})
	}

	// The coverage decision, and the only thing that makes not_available
	// reachable at all.
	//
	// It is NOT decided on the series, and that is the point of the store's
	// coverage column: the series holds one row per requested bucket whether or
	// not anything happened in it (the store's LATERAL join is a LEFT one, so a
	// bucket with no facts comes back as a zero rather than as a gap), which
	// means a zero-filled series is the same shape for an account that was
	// quiet and for one this plane has never ingested. Deciding on the series
	// would answer not_available for every quiet range — the collapse the
	// domain's NotAvailable doc rules out — and deciding on the range would be
	// a second way to compute what the series already reports.
	//
	// So the question this branch asks is the account's, not the range's: has
	// this plane EVER ingested a derived fact for it? If it has not, the answer
	// is an ANSWER rather than a failure — the nothing is the figure — and the
	// freshness rides along so the caller's next question can be a later range.
	if !stored.AccountHasDerivations {
		return analytics.NotAvailable(query, freshnessOf(freshness)), nil
	}

	answer := analytics.NewUsage(query, series, analytics.Capture{
		Reported:         stored.Capture.Reported,
		GatewayObserved:  stored.Capture.GatewayObserved,
		ReservationFloor: stored.Capture.ReservationFloor,
	}, freshnessOf(freshness))
	answer.SettledMinorUnits = stored.SettledMinorUnits
	answer.ReleasedMinorUnits = stored.ReleasedMinorUnits
	answer.FundsAddedMinorUnits = stored.FundsAddedMinorUnits
	answer.HeldMinorUnits = stored.Balances.Held
	answer.AvailableMinorUnits = stored.Balances.Available
	return answer, nil
}

// freshnessOf lifts the store's freshness statement into the domain's. The
// basis is carried rather than assumed: the store read the ingestion cursor's
// last completed pass, which is a LIVENESS signal and not a completeness one,
// and a freshness object that did not say which would be read as "everything up
// to here is accounted for" by every caller who saw a date.
func freshnessOf(stored persistence.Freshness) analytics.Freshness {
	basis := stored.Basis
	if basis == "" {
		basis = analytics.FreshnessFactFeedPass
	}
	return analytics.Freshness{DataThrough: stored.DataThrough, Basis: basis}
}

// newAnalyticsQuery requires the bounds the contract requires and then defers
// to the domain's own validation, translating a refusal into the code the
// transport knows.
//
// The required bounds are checked HERE rather than in the transport, for the
// same reason the transport still parses them: the use case is what a future
// caller reaches, and a bound enforced only by a handler is a bound a
// non-HTTP caller does not have. A zero time is refused as a MISSING bound
// rather than passed to the domain, because time.Time's zero is the year one:
// handing it to a range check would produce a forty-thousand-year refusal whose
// message names a duration the caller never asked for.
func newAnalyticsQuery(request UsageRequest) (analytics.Query, error) {
	if request.From.IsZero() {
		return analytics.Query{}, InvalidRequest("from is required: a range that defaults to a window answers a question nobody asked, and a caller cannot tell a default from a choice")
	}
	if request.To.IsZero() {
		return analytics.Query{}, InvalidRequest("to is required, and is the EXCLUSIVE upper bound of the range so that two consecutive ranges tile the timeline with neither overlap nor gap")
	}

	grain, err := analytics.ParseGranularity(request.Granularity)
	if err != nil {
		return analytics.Query{}, InvalidRequest(err.Error())
	}

	query, err := analytics.NewQuery(request.From, request.To, grain, request.Timezone)
	if err != nil {
		return analytics.Query{}, invalidRange(err)
	}
	return query, nil
}

// invalidRange is the one place a domain refusal becomes a transport code. It
// names the bound that was broken, because the domain's sentinels are the
// vocabulary and this is the sentence: a caller who was refused for asking for
// 400 days needs to know it was the 90.
func invalidRange(err error) error {
	switch {
	case errors.Is(err, analytics.ErrInvalidRange):
		return InvalidRequest("the range is not a range: from must be strictly before to")
	case errors.Is(err, analytics.ErrRangeTooLong):
		return InvalidRequest(fmt.Sprintf("the range is longer than the %s this surface serves", analytics.MaxRange))
	case errors.Is(err, analytics.ErrSeriesTooLong):
		return InvalidRequest(fmt.Sprintf("the range and grain would produce more than %d points", analytics.MaxSeriesPoints))
	case errors.Is(err, analytics.ErrInvalidTimezone):
		return InvalidRequest("the timezone is not a zone this process can resolve")
	case errors.Is(err, analytics.ErrInvalidGranularity):
		return InvalidRequest(err.Error())
	case errors.Is(err, analytics.ErrUnresolvable):
		return Internal(err)
	default:
		return InvalidRequest(err.Error())
	}
}
