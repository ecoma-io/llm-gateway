package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The routing stage's endings: the two ways an admitted request finalises,
// each in one unit of work, written before it is answered.
//
// A release states that the request ended without settleable usage — the
// whole hold goes back, the request row finalises failed or rejected, the
// replay record takes its terminal pointer, and the released fact is the
// feed's last word. A settle states that an answer was committed — the hold
// becomes the request's cost, the committed attempt's usage is priced, and
// the settled fact is the feed's last word. The shapes are deliberately
// twins: CAS first (who owns the ending), then the rows, the fact LAST,
// because the feed's append holds its row lock to the commit and the order
// facts are allocated must be the order they become visible.
//
// The budget is the ending budget, not a backoff ladder: these units run
// after the caller's answer has effectively been decided, so a few quick
// retries are all a request's lifetime affords. When the budget runs out the
// error is the answer — the hold stays stranded (open reservation, executing
// request, the reaper's to reclaim) and the transport answers the internal
// failure, never anything that invites a retry, because a retry would draw a
// second hold.
const (
	// endingMaxAttempts bounds every ending unit's retries, and
	// endingRetryBackoff is the pause between them.
	endingMaxAttempts  = 3
	endingRetryBackoff = 10 * time.Millisecond

	// endingBudget bounds how long one ending's whole retry ladder may run
	// once detached from the caller. The budget's floors and ceilings make it
	// a rounding error against a request's lifetime; the point of the bound
	// is that a detached ending still ends, rather than waiting on a
	// connection that is never coming back.
	endingBudget = 30 * time.Second
)

// endingContext detaches an ending from the caller's request context. The
// caller may be gone — a stream the client closed after its commitment, a
// body the proxy cut — and the ending is exactly the work that must survive
// the going: settlement of the usage that arrived proceeds either way, and a
// hold whose release died with the connection strands until the reaper
// claims it, unbilled. WithoutCancel keeps the values the store's querier
// resolution rides on while dropping the cancellation and the deadline; the
// budget's own timeout takes their place, so a detached ending can still
// fail rather than hang. Only the bookkeeping outlives the connection: the
// provider call itself, and the walk's abandoned check, stay on the caller's
// context.
func endingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), endingBudget)
}

// release finalises an admitted request without settleable usage. Exactly one
// of rejection and failure is set: a rejection is the no-candidate answer's
// two shapes, a failure is a surfaced upstream refusal — and the pairing each
// implies (request row reason, replay record pointer) travels with it, so the
// ending reads as one decision everywhere it is later read.
func (r *ChatRouting) release(ctx context.Context, in ChatInput, admitted *Admission, rejection execution.RejectionReason, failure execution.FailureReason) error {
	ctx, cancel := endingContext(ctx)
	defer cancel()
	for attempt := 0; attempt < endingMaxAttempts; attempt++ {
		// A unit that lost its CAS settled too — someone else owns the
		// ending — and an error is the only thing left to retry.
		_, err := r.releaseOnce(ctx, in, admitted, rejection, failure)
		if err == nil {
			return nil
		}
		if !isRetryableStoreFailure(err) {
			return err
		}
		if attempt+1 < endingMaxAttempts {
			select {
			case <-ctx.Done():
				return fmt.Errorf("application: release request %s: %w", admitted.RuntimeRequestID, ctx.Err())
			case <-time.After(endingRetryBackoff << attempt):
			}
		}
	}
	return fmt.Errorf("application: release request %s: the release unit did not settle after %d attempts", admitted.RuntimeRequestID, endingMaxAttempts)
}

// releaseOnce runs the release unit once. Every step past the CAS is guarded
// by the one before it: the CAS decides who owns the hold's ending, and a
// lost CAS ends the unit with nothing written — the winner's ending stands,
// and this caller's answer is unchanged.
func (r *ChatRouting) releaseOnce(ctx context.Context, in ChatInput, admitted *Admission, rejection execution.RejectionReason, failure execution.FailureReason) (bool, error) {
	var completed bool
	err := r.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := r.clock.TransactionTimestamp(txCtx)
		if err != nil {
			return err
		}
		// The CAS is the whole claim to the ending: only the writer that
		// moves the hold out of open may return its legs and state it.
		closed, err := r.reservations.Close(txCtx, admitted.ReservationID, accounting.StateReleased, now)
		if err != nil {
			return fmt.Errorf("application: release request %s: release the hold: %w", admitted.RuntimeRequestID, err)
		}
		if !closed {
			return nil
		}
		// The return, in the legs' stored ordinal order — the waterfall order
		// every writer of the projection walks, and the order the fact
		// publishes. The port's return is unconditional on the balances, so a
		// publication that shrank a ceiling cannot turn a release into a
		// failure.
		if _, err := r.ledger.Return(txCtx, admitted.Legs); err != nil {
			return fmt.Errorf("application: release request %s: return the hold: %w", admitted.RuntimeRequestID, err)
		}
		request := execution.Request{ID: admitted.RuntimeRequestID, Status: execution.StatusExecuting}
		switch {
		case failure == execution.FailedGatewayAbandoned:
			// The abandoned word names the request through its own door.
			// FailBeforeCommitment refuses it — that refusal is the domain's
			// law that a surfaced refusal and an abandonment are different
			// shapes of ending — and FailAbandoned is the finalisation the
			// vocabulary gives the word: no attempt named, because nothing
			// was committed. The reaper reaches it for the process it cannot
			// ask; the walk reaches it here, where the evidence is its own:
			// the caller left before any candidate answered, so this process
			// is the evidence, and the hold it opened comes back now instead
			// of stranding until the sweep.
			err = request.FailAbandoned(now)
		case failure != "":
			err = request.FailBeforeCommitment(failure, now)
		default:
			err = request.Reject(rejection, now)
		}
		if err != nil {
			return err
		}
		finalised, err := r.requests.Finalise(txCtx, request)
		if err != nil {
			return fmt.Errorf("application: release request %s: finalise the request: %w", admitted.RuntimeRequestID, err)
		}
		if !finalised {
			// The CAS gave this unit the ending, and the request row would
			// not take it. That is not a quiet no-op: a closed hold with no
			// deciding fact behind it is exactly the orphan the doctrine
			// forbids (no closed hold without its fact). The unit errors and
			// rolls back — the CAS's close included — leaving the hold open
			// for a whole retry or, at exhaustion, for the reaper.
			return fmt.Errorf("application: release request %s: the request row did not finalise", admitted.RuntimeRequestID)
		}
		decided, err := r.finaliseIntake(txCtx, in, failure, rejection)
		if err != nil {
			return fmt.Errorf("application: release request %s: finalise the replay record: %w", admitted.RuntimeRequestID, err)
		}
		if !decided {
			// Same verdict one step later: the record would not take its
			// terminal pointer, so the release fact must not be appended.
			// The unit rolls back whole and stays whole for its retry.
			return fmt.Errorf("application: release request %s: the replay record did not finalise", admitted.RuntimeRequestID)
		}
		fact, err := accounting.NewReleased(admitted.RuntimeRequestID, factLegs(admitted.Legs), now)
		if err != nil {
			return err
		}
		// The append is the unit's LAST statement: the feed's row lock is
		// held to the commit, so the order facts are allocated is the order
		// they become visible, and this ending reads as one fact.
		if _, err := r.facts.Append(txCtx, fact); err != nil {
			return fmt.Errorf("application: release request %s: append the release: %w", admitted.RuntimeRequestID, err)
		}
		completed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return completed, nil
}

// finaliseIntake takes the replay record's terminal pointer for a release
// ending: rejected with its reason, or failed with the surfaced refusal —
// the pointer a future replay re-derives the original answer from.
func (r *ChatRouting) finaliseIntake(txCtx context.Context, in ChatInput, failure execution.FailureReason, rejection execution.RejectionReason) (bool, error) {
	if failure != "" {
		return r.intakes.Finalise(txCtx, in.AccountID, in.IdempotencyKey, execution.FinalFailed, "", failure)
	}
	return r.intakes.Finalise(txCtx, in.AccountID, in.IdempotencyKey, execution.FinalRejected, rejection, "")
}

// settle finalises an admitted request whose answer was committed. The unit
// is detached on purpose — the provider call finished before it opens — and
// when it ends with someone else owning the reservation's close, the settle
// does not simply walk away: the orphan tail records what this unit saw,
// because a lost race is the one shape that would otherwise take the
// stream's only telemetry with it.
func (r *ChatRouting) settle(ctx context.Context, in ChatInput, admitted *Admission, attempt execution.Attempt, usage settleUsage, succeeded bool) error {
	ctx, cancel := endingContext(ctx)
	defer cancel()
	orphaned := false
	err := fmt.Errorf("application: settle request %s: the settle unit did not settle after %d attempts", admitted.RuntimeRequestID, endingMaxAttempts)
	for budget := 0; budget < endingMaxAttempts; budget++ {
		lost, onceErr := r.settleOnce(ctx, in, admitted, attempt, usage, succeeded)
		orphaned = orphaned || lost
		if onceErr == nil {
			err = nil
			break
		}
		err = onceErr
		if !isRetryableStoreFailure(onceErr) {
			return onceErr
		}
		if budget+1 < endingMaxAttempts {
			select {
			case <-ctx.Done():
				return fmt.Errorf("application: settle request %s: %w", admitted.RuntimeRequestID, ctx.Err())
			case <-time.After(endingRetryBackoff << budget):
			}
		}
	}
	if err != nil {
		return err
	}
	if orphaned {
		// The tail runs once, after the ladder — its writes have no
		// compare-and-set of their own to keep a retry from double-appending.
		return r.settleOrphanTail(ctx, in, admitted, attempt, usage)
	}
	return nil
}

// settleOnce runs the settle unit once: the reservation closed settled, the
// committed attempt appended, the request and the replay record finalised,
// the settled fact last. There is deliberately no projection return here — a
// settlement keeps the drawdown, and the Control Plane's derivation reads the
// settled fact to learn what the spend became (data-implications.md). The
// boolean answers whether the close lost its race: the unit wrote nothing,
// and the caller owns what gets recorded about that.
func (r *ChatRouting) settleOnce(ctx context.Context, in ChatInput, admitted *Admission, attempt execution.Attempt, usage settleUsage, succeeded bool) (bool, error) {
	orphaned := false
	err := r.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := r.clock.TransactionTimestamp(txCtx)
		if err != nil {
			return err
		}
		// The CAS is the whole claim to the ending, exactly as a release's
		// is. A lost close settles the question of who owns the ending; the
		// unit writes nothing and the winner's fact is the one the feed
		// carries — what this unit observed is the orphan tail's to record.
		closed, err := r.reservations.Close(txCtx, admitted.ReservationID, accounting.StateSettled, now)
		if err != nil {
			return fmt.Errorf("application: settle request %s: close the hold: %w", admitted.RuntimeRequestID, err)
		}
		if !closed {
			orphaned = true
			return nil
		}
		// The committed attempt, appended inside the ending's own unit: no
		// transaction is ever open across the provider call — this one
		// opened after the call finished — and the row commits with the
		// ending it belongs to, so a crash between them leaves neither.
		if err := r.attempts.Insert(txCtx, attempt); err != nil {
			if !errors.Is(err, persistence.ErrAttemptAlreadyAppended) {
				return fmt.Errorf("application: settle request %s: append the attempt: %w", admitted.RuntimeRequestID, err)
			}
			// The append raced a writer that persisted the same row — the
			// lost-commit-ack shape the sentinel exists for. The row is on
			// disk; the append is done.
		}
		request := execution.Request{ID: admitted.RuntimeRequestID, Status: execution.StatusExecuting}
		if succeeded {
			err = request.Succeed(attempt.ID, now)
		} else {
			err = request.FailAfterCommitment(attempt.ID, now)
		}
		if err != nil {
			return err
		}
		finalised, err := r.requests.Finalise(txCtx, request)
		if err != nil {
			return fmt.Errorf("application: settle request %s: finalise the request: %w", admitted.RuntimeRequestID, err)
		}
		if !finalised {
			// The CAS gave this unit the ending, and the request row would
			// not take it — the same orphan the release refuses to leave
			// behind. The unit rolls back whole, the hold stays open, and a
			// whole retry may state the ending again.
			return fmt.Errorf("application: settle request %s: the request row did not finalise", admitted.RuntimeRequestID)
		}
		status := execution.FinalSucceeded
		failure := execution.FailureReason("")
		if !succeeded {
			status = execution.FinalFailed
			failure = execution.FailedStreamAfterCommitment
		}
		decided, err := r.intakes.Finalise(txCtx, in.AccountID, in.IdempotencyKey, status, "", failure)
		if err != nil {
			return fmt.Errorf("application: settle request %s: finalise the replay record: %w", admitted.RuntimeRequestID, err)
		}
		if !decided {
			return fmt.Errorf("application: settle request %s: the replay record did not finalise", admitted.RuntimeRequestID)
		}
		amount, err := settleAmount(usage, admitted.Price)
		if err != nil {
			return fmt.Errorf("application: settle request %s: price the settlement: %w", admitted.RuntimeRequestID, err)
		}
		fact, err := accounting.NewSettled(admitted.RuntimeRequestID, attempt.ID,
			usage.capture, usage.input, usage.output, usage.delivery,
			admitted.Price.RevisionID, admitted.Price.InputUnitPrice, admitted.Price.OutputUnitPrice,
			amount, factLegs(admitted.Legs), now)
		if err != nil {
			return err
		}
		// The append is the unit's LAST statement, the same order law the
		// release keeps.
		if _, err := r.facts.Append(txCtx, fact); err != nil {
			return fmt.Errorf("application: settle request %s: append the settlement: %w", admitted.RuntimeRequestID, err)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return orphaned, nil
}

// settleOrphanTail records what a settle owns when it does not own the
// ending: its close lost the race to another writer — the twin of itself
// whose first attempt committed past a lost ack, or the lapsed-lease sweep
// closing a hold whose lease died mid-stream — and without this tail the
// race would take the stream's only telemetry with it. Three steps, each in
// the shape its write has always had: the attempt row appended standalone,
// exactly as a pre-commitment observation is, reading an already-persisted
// twin as the done thing it is; then the replay record's verdict read, to
// learn whether an ending was stated for the request — a settle that
// finalised it already carried this usage into the feed through its own
// settled fact, and the tail adds nothing the feed needs twice; and, when
// the record still waits on an ending, the unbillable-orphaned fact, in one
// unit of its own with the append as its last statement — the feed's word
// that usage was observed on a named, committed attempt this process did
// not settle. No terminal row is written and no money moves: the winner's
// fact, settled or expired, stands beside this one, which is the
// coexistence the fact kind was designed for.
func (r *ChatRouting) settleOrphanTail(ctx context.Context, in ChatInput, admitted *Admission, attempt execution.Attempt, usage settleUsage) error {
	if err := r.attempts.Insert(ctx, attempt); err != nil && !errors.Is(err, persistence.ErrAttemptAlreadyAppended) {
		return fmt.Errorf("application: settle request %s: append the orphaned attempt: %w", admitted.RuntimeRequestID, err)
	}
	record, err := r.intakes.Find(ctx, in.AccountID, in.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("application: settle request %s: read the replay record for the orphan tail: %w", admitted.RuntimeRequestID, err)
	}
	if record.FinalStatus != nil {
		return nil
	}
	return r.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := r.clock.TransactionTimestamp(txCtx)
		if err != nil {
			return err
		}
		fact, err := accounting.NewUnbillableOrphaned(admitted.RuntimeRequestID, attempt.ID,
			usage.capture, usage.input, usage.output, usage.delivery, factLegs(admitted.Legs), now)
		if err != nil {
			return fmt.Errorf("application: settle request %s: form the orphaned fact: %w", admitted.RuntimeRequestID, err)
		}
		if _, err := r.facts.Append(txCtx, fact); err != nil {
			return fmt.Errorf("application: settle request %s: append the orphaned fact: %w", admitted.RuntimeRequestID, err)
		}
		return nil
	})
}

// settleAmount prices the settlement: the hold formula — the one place the
// arithmetic lives — over the very numbers the settled fact carries, so the
// fact's amount and its counts can never disagree, and a settlement re-derived
// by an auditor comes out the same number the feed published.
func settleAmount(usage settleUsage, price catalog.PriceSnapshot) (int64, error) {
	return accounting.Hold(int(deref0(usage.input)), int(deref0(usage.output)),
		price.InputUnitPrice, price.OutputUnitPrice)
}

// deref0 reads a nullable count as the number it prices at: nobody-knows and
// zero both contribute nothing to the amount, while the fact keeps the
// distinction between them.
func deref0(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
