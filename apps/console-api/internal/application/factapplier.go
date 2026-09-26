package application

import (
	"context"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/ingestion"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// FactApplier is the persistence port's applier made real: it turns one fact
// into the ledger effect the fact contract derives, or into the recorded
// refusal the whole-page rule stands on.
//
// The shape of the unit is the replay's, not this type's: Apply runs inside
// the unit of work the FactIngestion opened for the page, and every write it
// makes — hold legs, settlement, the applied row, the quarantine — commits
// or rolls back with the page's cursor advance. That is the whole
// atomicity story: a fact is applied exactly when its position is claimed,
// and any failure leaves both where they were, so the next pass re-reads the
// same page and the read-before-write below makes it free.
//
// The disposition split is Interpret's, and Apply only honours it:
//
//   - a quarantinable refusal is recorded verbatim and swallowed — nil
//     returns, the page advances past the fact, and the refusal row is the
//     operator-visible state a reconciliation pass resolves. A fact this
//     build cannot interpret is conservatively uncharged: quarantine is
//     always the under-charge, never a guess at a charge;
//   - anything else — an unrecordable payload, a bucket the ledger does not
//     know, a settlement that conflicts with the one on file — is returned.
//     Those are states a retry does not conjure away, and the page stops on
//     them: the contract's own word is that a fact a consumer cannot apply
//     stops the consumer at that fact, never skips past it.
//
// The read-before-write is what makes a redelivered fact a no-op rather than
// a walk through the accounting primitives' convergence paths: Find on the
// (request_id, kind class) pair answers "derived from already?" in one read,
// and only a first arrival reaches the ledger. The pair — never the kind
// alone, never the request_id alone — is the contract's idempotency key,
// which is what lets a request's orphan fact book beside its settlement
// while its second settled fact books nothing.
type FactApplier struct {
	accounting  holdSettler
	applied     persistence.AppliedFacts
	quarantined persistence.QuarantinedFacts
}

// holdSettler is the slice of the accounting use cases a derived outcome
// moves money through. The applier is the first caller of these primitives
// that did not build them, and the interface is what keeps it that way: a
// consumer of the money grammar, not a second implementation of it.
type holdSettler interface {
	Hold(ctx context.Context, bucketID accounting.FundingBucketID, reservationID accounting.ReservationID, amountMinorUnits int64) (accounting.Bucket, error)
	ReleaseHold(ctx context.Context, bucketID accounting.FundingBucketID, reservationID accounting.ReservationID, amountMinorUnits int64) (accounting.Bucket, error)
	Settle(ctx context.Context, requestID accounting.RequestID, allocations []accounting.Allocation) (SettlementResult, error)
}

// NewFactApplier builds the applier around its three ports. It panics on a
// nil port for the reason every constructor in this package does: a port
// promised and not delivered is a wiring defect, and the middle of a page —
// after three facts applied and a hold booked — is a strictly worse place
// to learn about it.
func NewFactApplier(accounting holdSettler, applied persistence.AppliedFacts, quarantined persistence.QuarantinedFacts) *FactApplier {
	switch {
	case accounting == nil:
		panic("application: NewFactApplier requires the accounting use cases")
	case applied == nil:
		panic("application: NewFactApplier requires an applied-facts ledger")
	case quarantined == nil:
		panic("application: NewFactApplier requires a quarantine")
	}
	return &FactApplier{accounting: accounting, applied: applied, quarantined: quarantined}
}

// Apply derives fact and lands its effect, or records the refusal.
func (applier *FactApplier) Apply(ctx context.Context, fact persistence.Fact) error {
	outcome, err := ingestion.Interpret(ingestion.Fact{
		AppendSeq:            fact.AppendSeq,
		RequestID:            fact.RequestID,
		Kind:                 fact.Kind,
		SchemaVersion:        fact.SchemaVersion,
		OccurredAt:           fact.OccurredAt,
		Payload:              fact.Payload,
		CaptureMethod:        fact.CaptureMethod,
		CommittedAttemptID:   fact.CommittedAttemptID,
		ProviderInputTokens:  fact.ProviderInputTokens,
		ProviderOutputTokens: fact.ProviderOutputTokens,
		DeliveryTokens:       fact.DeliveryTokens,
		PriceRevisionID:      fact.PriceRevision,
		InputUnitPrice:       fact.InputUnitPrice,
		OutputUnitPrice:      fact.OutputUnitPrice,
		SettledAmount:        fact.SettledAmount,
		CorrectsAppendSeq:    fact.CorrectsAppendSeq,
	})
	if err != nil {
		if ingestion.Quarantinable(err) {
			return applier.quarantine(ctx, fact, err)
		}
		// Not recordable: the page stops, the transaction rolls back, and
		// the position holds for a human — the contract's stopped-feed
		// state, reached through the one refusal that cannot file itself.
		return fmt.Errorf("application: apply fact %d for request %s (%s): %w",
			fact.AppendSeq, fact.RequestID, fact.Kind, err)
	}

	// Replay is a read before it is ever a second write.
	existing, err := applier.applied.Find(ctx, outcome.RequestID, outcome.Class)
	if err != nil {
		return fmt.Errorf("application: read the applied ledger for request %s class %s: %w",
			outcome.RequestID, outcome.Class, err)
	}
	if existing != nil {
		// A redelivery is a read before it is ever a second write. The same
		// kind for the same class is the same logical outcome — the runtime
		// re-appends it, a replayed range re-reads it — and the append seq
		// identifies the wire row, not the fact, so it stays a no-op
		// whatever seq the redelivery rides. A different kind for the same
		// class is two terminal states claiming one request: applying it
		// would bury the disagreement in a silent nil, and refusing it
		// outright would wedge the feed on the writer's defect. It is
		// recorded instead, like any other contradiction between a fact and
		// the books.
		if existing.Kind == outcome.Kind {
			return nil
		}
		return applier.quarantine(ctx, fact, fmt.Errorf("%w: request %s already carries a %s effect from append seq %d; this delivery claims the same class as %s at seq %d",
			ingestion.ErrIncoherentFact, outcome.RequestID, existing.Kind, existing.AppendSeq, outcome.Kind, fact.AppendSeq))
	}

	var settlementID string
	switch outcome.Kind {
	case ingestion.KindSettled:
		settlementID, err = applier.applySettlement(ctx, outcome)
	case ingestion.KindReleased, ingestion.KindExpired:
		err = applier.applyRelease(ctx, outcome)
	case ingestion.KindUnbillableOrphaned: // no money moved.
		err = nil
	default:
		// Unreachable — Interpret refuses every kind it does not name, so
		// nothing derivable reaches this switch unnamed. Kept as the page
		// stop it would be, so exhaustiveness never rests on that accident.
		err = fmt.Errorf("ingestion: unknown fact kind %q", outcome.Kind)
	}
	if err != nil {
		// The accounting primitives' refusals are the ledger's word: an
		// unknown bucket, a conflicting settlement, a movement that does not
		// converge. None is this fact's disposition — they are states the
		// plane is in — so none is quarantined away. The page stops.
		return fmt.Errorf("application: apply fact %d for request %s (%s): %w",
			fact.AppendSeq, fact.RequestID, fact.Kind, err)
	}

	return applier.applied.Record(ctx, persistence.AppliedFact{
		RequestID:     outcome.RequestID,
		KindClass:     outcome.Class,
		Kind:          outcome.Kind,
		AppendSeq:     fact.AppendSeq,
		SettledAmount: fact.SettledAmount,
		CaptureMethod: fact.CaptureMethod,
		SettlementID:  settlementID,
	})
}

// applySettlement books a settled fact's derivation: the hold legs the tail
// names — this plane's hold rows are derived from the fact, and the fact is
// the first thing to carry the tail — then the settlement over them, whose
// plan consumes greedy down the waterfall and releases each tail, one
// consume and at most one release leg per bucket.
func (applier *FactApplier) applySettlement(ctx context.Context, outcome ingestion.Outcome) (string, error) {
	reservationID := accounting.ReservationID(outcome.RequestID)
	allocations := make([]accounting.Allocation, 0, len(outcome.Legs))
	for _, leg := range outcome.Legs {
		if _, err := applier.accounting.Hold(ctx, leg.Bucket, reservationID, leg.Held); err != nil {
			return "", fmt.Errorf("book the hold leg on bucket %s: %w", leg.Bucket, err)
		}
		held, err := accounting.NewAmount(leg.Held)
		if err != nil {
			return "", fmt.Errorf("hold leg on bucket %s: %w", leg.Bucket, err)
		}
		allocation, err := accounting.NewAllocation(leg.Bucket, reservationID, held)
		if err != nil {
			return "", fmt.Errorf("hold leg on bucket %s: %w", leg.Bucket, err)
		}
		if leg.Consumed > 0 {
			settled, err := allocation.SettleConsumed(accounting.Amount(leg.Consumed), outcome.Price)
			if err != nil {
				return "", fmt.Errorf("consume %d of bucket %s's hold: %w", leg.Consumed, leg.Bucket, err)
			}
			allocation = settled
		}
		allocations = append(allocations, allocation)
	}
	// Empty allocations are the zero-priced settle: a header of record with
	// no legs, because nothing moved. Settle takes it as it takes any other.
	result, err := applier.accounting.Settle(ctx, accounting.RequestID(outcome.RequestID), allocations)
	if err != nil {
		return "", fmt.Errorf("settle the derivation: %w", err)
	}
	if result.Converged {
		// The ledger already holds this request's settlement while the
		// consumer's applied ledger has no row for it — the request was
		// closed through another door, or a page that settled it once
		// failed without its own atomicity. The matched total means no
		// double charge is possible, but the state is the plane's, not the
		// fact's disposition to paper over: the page stops, the same
		// doctrine as a conflicting total.
		return "", fmt.Errorf("settle the derivation: request %s is already settled on the ledger as %s with no applied fact on file",
			outcome.RequestID, result.Settlement.ID)
	}
	return string(result.Settlement.ID), nil
}

// applyRelease books a released or expired fact: every leg of the tail goes
// back in full. The hold legs are booked from the same fact — they are this
// plane's memory of a hold the runtime closed elsewhere — and the release
// legs return exactly what they name, a zero-tail leg being skipped because
// money that moves is money that exists and a zero movement is not one.
func (applier *FactApplier) applyRelease(ctx context.Context, outcome ingestion.Outcome) error {
	reservationID := accounting.ReservationID(outcome.RequestID)
	for _, leg := range outcome.Legs {
		if _, err := applier.accounting.Hold(ctx, leg.Bucket, reservationID, leg.Held); err != nil {
			return fmt.Errorf("book the hold leg on bucket %s: %w", leg.Bucket, err)
		}
		if leg.Released > 0 {
			if _, err := applier.accounting.ReleaseHold(ctx, leg.Bucket, reservationID, leg.Released); err != nil {
				return fmt.Errorf("release the hold leg on bucket %s: %w", leg.Bucket, err)
			}
		}
	}
	return nil
}

// quarantine records the refusal verbatim, beside its reason, inside the
// page's unit of work. The nil it returns is not a silent skip — the record
// is the disposition, and the position that advances past the fact in the
// same transaction is the other half of it.
func (applier *FactApplier) quarantine(ctx context.Context, fact persistence.Fact, cause error) error {
	err := applier.quarantined.Record(ctx, persistence.QuarantinedFact{
		RequestID:            fact.RequestID,
		AppendSeq:            fact.AppendSeq,
		Kind:                 fact.Kind,
		SchemaVersion:        fact.SchemaVersion,
		OccurredAt:           fact.OccurredAt,
		Payload:              fact.Payload,
		CaptureMethod:        fact.CaptureMethod,
		CommittedAttemptID:   fact.CommittedAttemptID,
		ProviderInputTokens:  fact.ProviderInputTokens,
		ProviderOutputTokens: fact.ProviderOutputTokens,
		DeliveryTokens:       fact.DeliveryTokens,
		PriceRevision:        fact.PriceRevision,
		InputUnitPrice:       fact.InputUnitPrice,
		OutputUnitPrice:      fact.OutputUnitPrice,
		SettledAmount:        fact.SettledAmount,
		CorrectsAppendSeq:    fact.CorrectsAppendSeq,
		Reason:               cause.Error(),
	})
	if err != nil {
		return fmt.Errorf("application: quarantine fact %d for request %s: %w",
			fact.AppendSeq, fact.RequestID, err)
	}
	return nil
}
