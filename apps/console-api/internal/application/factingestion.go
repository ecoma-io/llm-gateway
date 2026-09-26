package application

import (
	"context"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// FactPageSize is the number of facts one replay pass asks the Data Plane for.
//
// It is the wire contract's own default (shared/usage-facts.yaml,
// UsageEventsLimit: 100, within 1..1000) rather than a tuned number: the
// workload that would justify a different page size does not exist yet, and a
// page size chosen now would be a guess with a retry policy attached to it.
// The day the loop wants to page faster or slower, it takes this as a
// parameter, and the call sites that exist then are what decide it.
const FactPageSize = 100

// FactIngestion is the replay use case: it moves the Control Plane's position
// through the Data Plane's fact feed by applying pages of facts to the
// Control Plane's own store.
//
// This is the Control Plane's half of the pull-with-replay delivery model
// (ADR 0006 §5, and shared/usage-facts.yaml for the wire): the runtime records
// facts durably and never notifies anyone; this use case reads them, applies
// them, and records how far it has got. The properties that make that safe are
// all in the order of the four calls below, and none of them is visible in the
// signatures —
//
//  1. the position is read from the Control Plane's own store, so the restart
//     point is the consumer's and not the Data Plane's;
//  2. the page is fetched outside any transaction, because holding a database
//     transaction open across a network call is how a slow management API
//     becomes a database problem;
//  3. every fact in the page is applied inside one unit of work, through the
//     applier, whose idempotency by request_id is what makes a redelivered
//     page free;
//  4. the cursor advances inside that same unit of work and only after the last
//     fact has been applied. The position is a claim about work already done,
//     so it must never move ahead of the work: if any apply fails, the
//     transaction rolls back, the position stays where it was, and the next
//     pass reads the same page again. That is the whole crash-safety story, and
//     it is why the advance is the last statement in the transaction rather
//     than its own write.
//
// The page's NextCursor is never computed here. It comes from the Data Plane —
// a position in an order only the Data Plane can name — and this use case
// stores it verbatim.
//
// Nothing constructs this type yet, and nothing schedules it: there is no
// worker loop, no ticker and no background goroutine, because the loop that
// calls Replay belongs to the pull the schema PR builds, beside the table the
// cursor is stored in. The postgres and valkey adapters sit unwired for the
// same reason — their use case has not landed either — and a scheduler wired
// ahead of its schema would be a process that polls a feed into nothing.
type FactIngestion struct {
	facts   dataplane.UsageFacts
	store   persistence.Store
	cursor  persistence.IngestionCursor
	applier persistence.FactApplier
}

// NewFactIngestion builds the replay use case around the four ports it needs.
// It panics on a nil port for the reason postgres.New panics on a nil pool: a
// port this use case was promised and did not get is a wiring defect, and a nil
// dereference in the middle of an ingest — after a page has been read and a
// transaction opened — is a strictly worse place to learn about it.
func NewFactIngestion(facts dataplane.UsageFacts, store persistence.Store, cursor persistence.IngestionCursor, applier persistence.FactApplier) *FactIngestion {
	switch {
	case facts == nil:
		panic("application: NewFactIngestion requires a usage fact reader")
	case store == nil:
		panic("application: NewFactIngestion requires a store")
	case cursor == nil:
		panic("application: NewFactIngestion requires an ingestion cursor")
	case applier == nil:
		panic("application: NewFactIngestion requires a fact applier")
	}
	return &FactIngestion{facts: facts, store: store, cursor: cursor, applier: applier}
}

// FactIngestionResult reports what one replay pass did, and it carries nothing
// else on purpose — no cursor, no page. The position is durable in the store by
// the time this is returned, and a caller holding a copy of it would be one
// step away from comparing positions locally, which is the one thing no package
// outside the Data Plane may do with a cursor.
type FactIngestionResult struct {
	// Applied is the number of facts this pass handed to the applier. It counts
	// deliveries and not effects: a replayed fact is delivered and applied
	// again, and the applier's idempotency is what keeps the second one free.
	Applied int

	// HasMore reports whether the Data Plane said the feed holds more facts
	// after the page just applied. It is the stopping condition of the loop
	// that does not exist yet, and it is deliberately not derived from Applied:
	// a short page and a drained feed are different states, because a
	// concurrent writer produces a short page too.
	HasMore bool
}

// Replay reads at most one page of facts beyond the stored position and applies
// it, advancing the cursor, in a single unit of work.
//
// A failure leaves the Control Plane exactly where it was: the position
// untouched and the page unapplied, so calling Replay again re-reads the same
// page. It returns ErrCursorExpired — which the caller can match through
// errors.Is, alongside any other cause from the ports — when the Data Plane can
// no longer replay the stored position; that is not retried, it is a state the
// Control Plane has to be brought out of on purpose, and the error carries no
// cursor value so a log line cannot become a position store of its own.
func (ingestion *FactIngestion) Replay(ctx context.Context) (FactIngestionResult, error) {
	position, err := ingestion.cursor.Position(ctx)
	if err != nil {
		return FactIngestionResult{}, fmt.Errorf("application: read the usage fact position: %w", err)
	}

	// The empty string is the cursor's own "never applied anything", and the
	// adapter is what turns it into an absent `after`; the use case passes the
	// position through without looking at it, which keeps it the one string
	// this flow handles without opinion.
	page, err := ingestion.facts.ReadUsageEvents(ctx, position, FactPageSize)
	if err != nil {
		return FactIngestionResult{}, fmt.Errorf("application: read usage facts: %w", err)
	}

	// A page with no position is a page this pass cannot act on, and the check
	// is here — before the unit of work, and not only in the adapter that read
	// the wire — because advancing is the one step in this flow that writes
	// durable state outside this process.
	//
	// The adapter refuses a malformed response as it arrives, so a port that
	// behaves as the seam documents never hands this one back. This second gate
	// is not that check repeated: it guards the write rather than the read. An
	// empty position stored here is indistinguishable from "never applied
	// anything", so the consumer would re-read the same page on every cycle and
	// never advance — a silent stall with nothing to observe, which is why it is
	// worth refusing at the last point where it can still be refused.
	if page.NextCursor == "" {
		return FactIngestionResult{}, fmt.Errorf("application: read usage facts: %w", dataplane.ErrMalformedPage)
	}

	if err := ingestion.store.WithinTx(ctx, func(txCtx context.Context) error {
		for _, event := range page.Events {
			// The seam's vocabulary stops here: the applier is given the
			// store's Fact, so the derivation from a payload — and whether this
			// build understands the fact's schema_version at all — stays
			// behind the port that owns the schema. Refusing an unknown
			// version, rather than guessing at it, is that side's decision to
			// make: it is the only side that knows which shapes it can settle.
			fact := persistence.Fact{
				AppendSeq:            event.AppendSeq,
				RequestID:            event.RequestID,
				Kind:                 event.Kind,
				SchemaVersion:        event.SchemaVersion,
				OccurredAt:           event.OccurredAt,
				Payload:              []byte(event.Payload),
				CaptureMethod:        event.CaptureMethod,
				CommittedAttemptID:   event.CommittedAttemptID,
				ProviderInputTokens:  event.ProviderInputTokens,
				ProviderOutputTokens: event.ProviderOutputTokens,
				DeliveryTokens:       event.DeliveryTokens,
				PriceRevision:        event.PriceRevision,
				InputUnitPrice:       event.InputUnitPrice,
				OutputUnitPrice:      event.OutputUnitPrice,
				SettledAmount:        event.SettledAmount,
				CorrectsAppendSeq:    event.CorrectsAppendSeq,
			}
			if err := ingestion.applier.Apply(txCtx, fact); err != nil {
				// Returning here ends the unit of work, so the effects of the
				// facts already applied roll back with it. Nothing has been
				// advanced, and nothing will be: the next pass reads the same
				// page, and the applier's idempotency makes the facts that did
				// land free to deliver again.
				return err
			}
		}

		// Last, and inside the transaction that applied the facts above. The
		// context here is the transaction's — txCtx, not ctx — because a store
		// resolves its query surface from the context it is handed, and an
		// advance written through the pool would commit on its own, outside
		// the rollback that exists to keep position and work moving together.
		return ingestion.cursor.Advance(txCtx, page.NextCursor)
	}); err != nil {
		return FactIngestionResult{}, fmt.Errorf("application: apply usage facts: %w", err)
	}

	return FactIngestionResult{Applied: len(page.Events), HasMore: page.HasMore}, nil
}
