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
// The page size is not decided here, and its absence from this file is the
// decision. The contract's bounds and its default are applied by the surface
// that receives the parameter, before this use case is called, so a limit
// arriving here is one the contract allows and there is nothing left to enforce
// — while a limit this function quietly adjusted would be a page the caller
// never asked for, silently delivered. What the use case does own is the
// contract's *shape* for `after`: an empty string means the port's own "from
// the beginning of what is retained", and a non-empty one is passed through
// byte for byte, because a cursor is opaque here as it is everywhere else.
func (app *App) ReadUsageEvents(ctx context.Context, after string, limit int) (usagefacts.Page, error) {
	return app.facts.Read(ctx, after, limit)
}
