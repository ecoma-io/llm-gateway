package main

import (
	"context"
	"errors"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/inbound/http"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
)

// The usage read's one meeting point with the handler that calls it, and it is
// HERE for the reason consoleReads is: the import rule forbids the inbound
// surface from reaching internal/domain, so the answer has to become wire fields
// at the one place allowed to know both — the composition root. usage.go states
// the seam in full; this file is the other end of it.
//
// It is a smaller file than consoleReads because there is one answer rather than
// ten pages, and it is otherwise the same discipline: copy, convert, keep the
// shape. Nothing here is computed. A total is copied from the total the domain
// summed, a bucket edge is copied from the edge the domain cut, and a basis is
// copied from the pass that produced it. If a figure could be derived two ways,
// this is the wrong file for it — and the domain's own tests, not these lines,
// are where the figure's arithmetic is pinned.

// consoleUsage adapts *application.Usage to the http package's UsageUseCases
// seam.
//
// The seam's method is exported and the type is a value rather than a pointer,
// for the reason consoleReads is: Go lets any package satisfy an interface as
// long as every method is exported, and the value carries the pointer the
// application hands back so a nil is caught by newConsoleUsage below rather
// than reaching a request.
type consoleUsage struct {
	useCase *application.Usage
}

// errConsoleUsageUnwired is what a nil use case produces. It is named here
// rather than imported, so the composition root's refusal and the http
// package's own nil check read as the two things they are: a wiring mistake at
// start-up, and a request-time nil the transport refuses to build a server
// around.
var errConsoleUsageUnwired = errors.New("console-api: the usage use case is required and was nil")

// newConsoleUsage returns the transport's view of the usage use case.
//
// The nil is refused at this boundary rather than answered around: a surface
// mounted on a nil use case panics on the first request it serves, and the
// stack of that panic names a request goroutine rather than the wiring that
// caused it. A process that refuses to start is an operator's five minutes; a
// process that panics on the console's usage screen is a page five.
func newConsoleUsage(useCase *application.Usage) (http.UsageUseCases, error) {
	if useCase == nil {
		return nil, errConsoleUsageUnwired
	}
	return consoleUsage{useCase: useCase}, nil
}

// Usage answers one range and renders it into the transport's own vocabulary.
//
// The refusal travels unchanged, and that is deliberate: which credentials
// resolve, which bounds this plane serves and whether a figure exists yet are
// decisions below this line, and this adapter is not entitled to a second
// opinion on any of them. A conversion that turned one refusal into another
// would be a place a caller could learn something the use case did not say.
func (c consoleUsage) Usage(ctx context.Context, token string, request application.UsageRequest) (http.UsageAnswer, error) {
	answer, err := c.useCase.Usage(ctx, token, request)
	if err != nil {
		return http.UsageAnswer{}, err
	}

	// Non-nil so the response carries an array rather than null: the contract
	// types the series as an array, and `null` is not an array.
	series := make([]http.UsageAnswerBucket, 0, len(answer.Series))
	for _, point := range answer.Series {
		series = append(series, http.UsageAnswerBucket{
			Start:                  point.Start,
			End:                    point.End,
			RequestsWithUsageFacts: point.WithUsageFacts,
			RequestsSettled:        point.Settled,
		})
	}

	return http.UsageAnswer{
		Availability: string(answer.Availability),
		Granularity:  string(answer.Range.Granularity),
		From:         answer.Range.From,
		To:           answer.Range.To,
		// The zone the answer was cut in, as the domain resolved it. The
		// transport echoes this field and derives no name of its own.
		Timezone:           answer.Range.Timezone,
		FinalBucketPartial: answer.FinalBucketPartial,
		Series:             series,

		RequestsWithUsageFacts: answer.RequestsWithUsageFacts,
		RequestsSettled:        answer.RequestsSettled,

		SettledMinorUnits:    answer.SettledMinorUnits,
		ReleasedMinorUnits:   answer.ReleasedMinorUnits,
		FundsAddedMinorUnits: answer.FundsAddedMinorUnits,
		HeldMinorUnits:       answer.HeldMinorUnits,
		AvailableMinorUnits:  answer.AvailableMinorUnits,

		Capture: http.UsageAnswerCapture{
			Reported:         answer.Capture.Reported,
			GatewayObserved:  answer.Capture.GatewayObserved,
			ReservationFloor: answer.Capture.ReservationFloor,
		},
		Freshness: http.UsageAnswerFreshness{
			DataThrough: answer.Freshness.DataThrough,
			Basis:       string(answer.Freshness.Basis),
		},
	}, nil
}
