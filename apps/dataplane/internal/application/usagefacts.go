package application

import (
	"context"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/usagefacts"
)

// ReadUsageEvents returns one page of the runtime's recorded facts, in the
// order the Data Plane recorded them.
//
// This is the runtime's only use case that is not on the request path, and the
// difference is worth stating. Everything else the application does — admitting
// a request, choosing a candidate, recording an outcome — happens while a
// caller waits. This one happens because a consumer, somewhere in the Control
// Plane, decided it was time to reconcile, and nothing on the LLM request path
// waits on it (ADR 0006 §4).
//
// The use case is thin on purpose, and it is thin for a reason that is easy to
// get wrong later: the fact feed must not acquire a business rule. Quota,
// reservation and settlement semantics belong to whoever derives them from a
// fact, and they run in the Control Plane. A rule that crept in here — a
// filtered fact, a rewritten kind, a page re-sorted "helpfully" — would be
// applied to every consumer at once, in the process that serves traffic, where
// the latency is paid by an LLM caller.
//
// What it does own is the contract's bounds: the page size is clamped to what
// the port allows, because `limit` arrives from a caller and an application
// that passed it through verbatim would let that caller ask for the whole
// history in one response.
func (app *App) ReadUsageEvents(ctx context.Context, after string, limit int) (usagefacts.Page, error) {
	return app.facts.Read(ctx, after, clampLimit(limit))
}

// clampLimit applies the port's page-size bounds to a caller-supplied limit.
//
// Zero means "unspecified" rather than "none", and it is the value the
// management listener passes for a limit the caller omitted: an absent query
// parameter and a parsed zero are indistinguishable by the time they arrive
// here, and a caller asking for zero facts — the only other reading — is asking
// for nothing and would get an empty page forever. A negative never arrives: the
// listener refuses it, because unlike a limit that is merely too large there is
// no intent behind it to honour. Anything above the port's maximum is reduced
// rather than refused: the caller's intent is legible, the answer is still
// correct, and failing a reconciliation run over a page size would trade a
// request that is slightly smaller than asked for against a consumer that has
// stopped advancing.
//
// The bounds themselves are declared by the port — DefaultLimit and MaxLimit are
// its constants — and this is the function that applies them, so there is one
// place a page size can be decided and no second one a future caller could find
// instead.
func clampLimit(limit int) int {
	if limit <= 0 {
		return usagefacts.DefaultLimit
	}
	if limit > usagefacts.MaxLimit {
		return usagefacts.MaxLimit
	}
	return limit
}
