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
	dimensions  persistence.FactDimensions
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

// NewFactApplier builds the applier around its four ports. It panics on a
// nil port for the reason every constructor in this package does: a port
// promised and not delivered is a wiring defect, and the middle of a page —
// after three facts applied and a hold booked — is a strictly worse place
// to learn about it.
//
// The dimensions port is not optional and is not a reporting concern bolted on
// at the edge. It is the only reason an account-scoped report can exist, and it
// is written here, inside the same unit of work that books the effect: an
// attribution committed apart from the fact it attributes is a row describing a
// derivation that rolled back, and a fact whose attribution is missing is a
// request that has silently vanished from its customer's report.
func NewFactApplier(accounting holdSettler, applied persistence.AppliedFacts, quarantined persistence.QuarantinedFacts, dimensions persistence.FactDimensions) *FactApplier {
	switch {
	case accounting == nil:
		panic("application: NewFactApplier requires the accounting use cases")
	case applied == nil:
		panic("application: NewFactApplier requires an applied-facts ledger")
	case quarantined == nil:
		panic("application: NewFactApplier requires a quarantine")
	case dimensions == nil:
		panic("application: NewFactApplier requires the analytics fact dimensions")
	}
	return &FactApplier{accounting: accounting, applied: applied, quarantined: quarantined, dimensions: dimensions}
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

	// The account the fact belongs to, derived from the buckets its own
	// allocation tail names, recorded inside the same unit of work that booked
	// the effect above. It comes after the effect and before the applied row
	// because all three are one transaction and the order is the applier's own:
	// money first, attribution second, disposition last.
	if err := applier.attribute(ctx, fact, outcome); err != nil {
		return err
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

// attribute records the accounts a fact's allocation tail draws on, so the
// analytics read model can scope to an account at all.
//
// Three properties of it are decisions, not mechanics:
//
//   - THE ACCOUNT IS DERIVED, NEVER SUPPLIED. It comes from the buckets the
//     fact's own tail names, joined to their owners. The fact contract carries
//     no account, no plane's usage_events table has an account column, and the
//     runtime's requests table that does is unreachable from this plane. A
//     caller-supplied account here would be a caller choosing whose report a
//     request lands in, and this plane has no caller that could be trusted with
//     that.
//
//   - IT IS A SET, AND EVERY MEMBER IS RECORDED. A settlement can draw on an
//     entitlement bucket and a PAYG bucket at once, and in the general shape the
//     schema allows those can belong to more than one account. Recording only
//     the first would attribute the request to one of the accounts that paid
//     for it and drop the other, and the dropped account's report would be
//     short by exactly the requests it funded — a figure wrong in the
//     direction that looks like good news.
//
//   - A FACT THAT DRAWS ON NO BUCKET BELONGS TO NO ACCOUNT. The account is
//     DERIVED from the buckets the tail names, and a tail of no legs derives
//     nothing to derive from. So an orphan — a request no bucket could pay
//     for — and a zero-priced settle — a real settlement of record that
//     moved nothing, whose tail is empty by the same rule that makes a
//     positive amount carry legs (interpret.go:331) — both record no
//     attribution, and both are absent from every account's report rather than
//     counted in one of them.
//
//     The second case is the one worth being careful about, because the
//     contract could have promised either answer. A zero-priced settle IS a
//     settlement of record, so counting it is defensible; but attributing it
//     would mean inventing an owner, and the only thing that could supply one
//     is the API key — which this plane's fact contract does not carry and
//     which would be a caller-supplied account, the one thing an attribution
//     must never be. So the request is counted in a platform-wide view and
//     in no account's, and the openapi says so in those words rather than
//     promising a figure the tenancy rule cannot produce.
func (applier *FactApplier) attribute(ctx context.Context, fact persistence.Fact, outcome ingestion.Outcome) error {
	if len(outcome.Legs) == 0 {
		return nil
	}

	// The tail's buckets, in the tail's order, so the attribution is
	// deterministic on a page that is itself deterministic. The map is
	// belt-and-braces: checkLegs already refuses a tail that names one bucket
	// twice, so no fact reaching here can repeat a bucket. It stays because the
	// cost is nil and the alternative is a duplicate row per duplicate bucket
	// if a future contract ever permits one — a duplicate the applier would
	// have written and the store's primary key would have refused, turning a
	// report's accounting detail into a page-stopping error.
	buckets := make([]string, 0, len(outcome.Legs))
	seen := make(map[string]struct{}, len(outcome.Legs))
	for _, leg := range outcome.Legs {
		id := string(leg.Bucket)
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		buckets = append(buckets, id)
	}

	accounts, err := applier.dimensions.AccountsOf(ctx, buckets)
	if err != nil {
		return fmt.Errorf("application: resolve the accounts for fact %d request %s: %w",
			fact.AppendSeq, outcome.RequestID, err)
	}

	// One row per account, under the same (request_id, kind_class) key the
	// applied-facts ledger enforces, so a redelivery converges to a no-op
	// exactly as it does there — the same replay reaching both.
	for _, accountID := range accounts {
		if err := applier.dimensions.Record(ctx, persistence.FactDimension{
			RequestID: outcome.RequestID,
			KindClass: outcome.Class,
			AccountID: accountID,
			AppendSeq: fact.AppendSeq,
		}); err != nil {
			return fmt.Errorf("application: attribute fact %d request %s to account %s: %w",
				fact.AppendSeq, outcome.RequestID, accountID, err)
		}
	}
	return nil
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
