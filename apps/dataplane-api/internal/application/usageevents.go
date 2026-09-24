package application

import (
	"context"
	"errors"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// UsageEvents returns one page of the Data Plane's usage-fact feed and nothing
// else. It is the use case behind GET /internal/usage-events.
//
// It is a pass-through, and that is the design rather than an unfinished
// version of one. The page is not filtered, reordered, deduplicated or
// trimmed, and the cursor is not inspected: the Data Plane owns the feed's
// order and the cursor's encoding, the Control Plane owns the position it has
// applied through, and a decision made in this process would be a third party
// rewriting the protocol both of them depend on (ADR 0006 §5, §9).
//
// There is also no acknowledgement anywhere on this path — no consume, no
// delete, no mark-as-delivered — and its absence is what makes the seam safe to
// retry: a call that fails leaves the feed exactly as it was, and the same page
// can be asked for again.
func (app *App) UsageEvents(ctx context.Context, after string, limit int) (dataplane.Page, error) {
	page, err := app.usage.ReadUsageEvents(ctx, after, limit)
	if err == nil {
		return page, nil
	}

	// The port's two failures become this package's codes, because the
	// transport maps codes and has no business knowing which outbound adapter
	// is behind the port. Both keep their cause, so errors.Is still reaches the
	// port's sentinel for a caller that needs to distinguish them
	// programmatically; neither cause is serialized, and Error.Error() returns
	// the fixed message above it rather than the cause.
	if errors.Is(err, dataplane.ErrCursorExpired) {
		return dataplane.Page{}, &Error{
			Code:    CodeCursorExpired,
			Message: "the usage fact cursor is no longer replayable",
			cause:   err,
		}
	}
	if errors.Is(err, dataplane.ErrUpstreamUnavailable) {
		return dataplane.Page{}, &Error{
			Code:    CodeUpstreamUnavailable,
			Message: "the data plane is unavailable",
			cause:   err,
		}
	}

	// An adapter that failed in a way the port does not describe is this
	// process's own defect, and it is classified as such rather than reported
	// as the Data Plane's problem. The cause stays server-side: Internal's
	// message is replaced with a fixed one at the wire.
	return dataplane.Page{}, Internal(err)
}
