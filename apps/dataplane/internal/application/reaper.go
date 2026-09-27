package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The reaper: the one door into a request that never had a caller left to end
// it.
//
// Every other ending in this package is written by the process that admitted
// the request, from evidence only that process holds: the walk that watched
// the caller leave, the settle that watched the usage arrive. When that
// process dies, none of them runs, and what it left behind is a hold with both
// its clocks run out — an open reservation nobody will close, an executing
// request row nothing will finalise, a replay record nothing will point, and
// capacity drawn from a grant that is not spending anything. The reaper is what
// says the ending anyway, from the two facts that outlive the process: the
// hold's own row, and the clock.
//
// It is a third door rather than a mode of the release, and that is the shape
// most of this file is arguing. One composition — return the whole hold, state
// the request, point the record, publish the fact — has two doors into it
// (data-implications.md), and the second one is not the first one with the
// caller's side removed. A release is written by the unit that owns the
// ending: its compare-and-set on the reservation is the whole claim, and every
// step after it is guarded by the one before, so a false anywhere is a defect
// and the unit rolls back. The reaper is a second door into the same request,
// and a second door has to tolerate the first: the two divergences from
// releaseOnce below are not loosened discipline, they are the discipline a
// unit that does not own the ending can actually keep. Both of them are about
// the same fact — somebody else already ended this request — and both of them
// are the reason the reaper cannot be the release with a different sweep in
// front of it.
//
// The reaper is not a second accounting engine. It returns capacity and closes
// runtime state. It prices nothing, it derives no amount, and it never infers a
// financial position: the expired fact it appends carries the hold's own legs
// and no usage and no amount, exactly as the released fact does, and the money
// half of the arc is another plane's derivation from that fact (ADR 0004,
// ADR 0006 §5). What the reaper decides is which requests are finished. What
// they cost is not its sentence.

// The reaper's ladder is the ending ladder's numbers, and for the ending
// ladder's reasons (route_endings.go): the unit being retried is the same kind
// of unit — all or nothing, every write re-judged by the sweep on the next
// run — and the classification that admits a retry at all
// (isRetryableStoreFailure) is one predicate shared by both ladders. They are
// named apart because the two budgets answer to different clocks: an ending's
// is the rest of one request's lifetime, and a cycle of this is a loop that
// will simply run again. The day those two need to differ is the day a reaper
// retry ladder should not be holding a drain hostage to a request's patience.
const (
	// reaperMaxAttempts bounds one victim's unit of work's retries, and
	// reaperRetryBackoff is the pause between them.
	reaperMaxAttempts  = endingMaxAttempts
	reaperRetryBackoff = endingRetryBackoff
)

// ReapReport is what one drain cycle reclaimed. The three counts are the same
// three things said three ways, and their sum is the invariant an operator
// reads first: Scanned counts the victims the cycle's sweeps returned — the
// holds this cycle found eligible and therefore closed, which is the work the
// cycle actually took off the open set. Recovered counts the victims whose
// whole tail committed with it. Skipped counts the victims whose hold this
// cycle closed and whose tail it then tolerated as already done, because
// another ending had got there first (the two tolerated skips below). So a
// cycle reads Scanned == Recovered + Skipped, and a Skipped is not a
// failure: it is a hold reclaimed whose ending somebody else had already
// written.
type ReapReport struct {
	Scanned   int
	Recovered int
	Skipped   int
}

// ReapOutcome names which of the two shapes one observation took. It is a
// reaper's own vocabulary rather than a request's status, because a skipped
// observation's request row reads whatever the ending that won it wrote and
// the reaper did not — an operator asking "did the reaper end this request, or
// find it already ended" is asking this question and no other.
const (
	// ReapOutcomeExpired is the whole tail: the hold closed, the request and
	// its replay record finalised, the expired fact appended.
	ReapOutcomeExpired = "expired"
	// ReapOutcomeSkipped is the hold closed and the tail tolerated as
	// already done.
	ReapOutcomeSkipped = "skipped"
)

// ReapObservation is the reclaim's one structured record. It is the close's
// sibling (CloseObservation) and as narrow as that one: the request it closed
// a hold for, the feed position its fact took, the terminal status its own
// transition carries, which of the two outcomes it was, how many legs went
// back, and how long the reclaim took. Nothing on it can carry a prompt, a
// credential or a provider response — the reaper never held one — and the
// identity it carries is the request id the close already carries, not
// anything of its own invention. UsageEventID is the feed position the fact
// took, and zero on a skipped observation whose fact was recorded by another
// writer; ReclaimedLegs is the count the projection's return reported landing,
// which is short of the hold's own leg count when a publication has not caught
// up with a bucket the hold was drawn from — the one number here that reports
// a gap rather than a claim.
type ReapObservation struct {
	RequestID     string
	UsageEventID  int64
	FinalStatus   string
	Outcome       string
	ReclaimedLegs int
	SweepDuration time.Duration
}

// ReapObserver receives one observation per reclaimed victim, after the unit
// that reclaimed it has committed. It runs on the reclaim's own path, which is
// the drain loop's goroutine and nothing a caller waits on: an observer that
// blocks delays the next sweep, and an observer that panics is the operator's
// miswiring. Nil is the wired-nothing default, for the reason the close's is.
type ReapObserver func(ReapObservation)

// Reaper is the application service that reclaims what a dead process
// stranded. It is a peer of ChatRouting rather than part of it: the routing
// stage serves a caller, and everything the reaper does happens with no caller
// at all. What it shares with that stage is its port surface — the same
// store, the same repositories, the same clock — and its unit-of-work
// discipline, down to the append that goes last.
type Reaper struct {
	store        persistence.Store
	reservations persistence.ReservationRepository
	ledger       persistence.QuotaProjectionRepository
	requests     persistence.RequestRepository
	intakes      persistence.IntakeRepository
	facts        persistence.FactRepository
	clock        txClock
	batchSize    int
	cycleBudget  time.Duration
	// ObserveReap receives one ReapObservation per reclaimed victim. Nil is
	// the wired-nothing default: the observation is the operator's
	// instrument, not the reclaim's.
	ObserveReap ReapObserver
}

// ReaperConfig is what the composition root tells the reaper about one drain
// cycle. Both bounds are cycle-local rather than per-request, which is what
// separates them from the horizons the routing stage is given: a cycle runs
// when there is nothing to reclaim, and a reaper that spent a request's worth
// of patience on an empty table would be the wrong shape of loop entirely.
type ReaperConfig struct {
	// BatchSize bounds how many victims one cycle may take. It is a bound on
	// units of work, not a page size: each victim is a whole unit of its own,
	// and a cycle that takes the fifty-first victim is a cycle that has been
	// holding the feed's serialisation point fifty-one times over.
	BatchSize int

	// CycleBudget bounds how long one cycle may keep opening units. The
	// caller's context already carries the loop's own deadline, and a deadline
	// kills the unit in flight; this bounds the next one from being started,
	// so a caller that hands this a context with no deadline of its own still
	// cannot hold a drain open past a bound it chose somewhere.
	CycleBudget time.Duration
}

// NewReaper builds the reaper over the ports it needs. It panics on a nil
// port, and on a cycle it could not run, for the reason its admission and
// routing siblings state: a port this service was promised and did not get is
// a wiring defect, and the middle of a drain — after one hold has been closed
// and before its tail has been written — is a strictly worse place to learn
// about it than the start of the process.
//
// The two configuration panics are the same argument about the knobs: a batch
// size below one is a reaper that can close nothing, and a cycle budget of
// zero is a drain with no bound, which is precisely the long transaction the
// sweep's own limit exists to prevent.
func NewReaper(
	store persistence.Store,
	reservations persistence.ReservationRepository,
	ledger persistence.QuotaProjectionRepository,
	requests persistence.RequestRepository,
	intakes persistence.IntakeRepository,
	facts persistence.FactRepository,
	config ReaperConfig,
) *Reaper {
	switch {
	case store == nil:
		panic("application: NewReaper requires a store")
	case reservations == nil:
		panic("application: NewReaper requires a reservation repository")
	case ledger == nil:
		panic("application: NewReaper requires a quota projection repository")
	case requests == nil:
		panic("application: NewReaper requires a request repository")
	case intakes == nil:
		panic("application: NewReaper requires an intake repository")
	case facts == nil:
		panic("application: NewReaper requires a fact repository")
	case config.BatchSize < 1:
		panic("application: NewReaper requires a batch size of at least one; a drain that can close nothing is not a reaper")
	case config.CycleBudget <= 0:
		panic("application: NewReaper requires a positive cycle budget; an unbounded drain is the long transaction the sweep's limit exists to prevent")
	}
	return &Reaper{
		store:        store,
		reservations: reservations,
		ledger:       ledger,
		requests:     requests,
		intakes:      intakes,
		facts:        facts,
		clock:        storeClock{store: store},
		batchSize:    config.BatchSize,
		cycleBudget:  config.CycleBudget,
	}
}

// Reap runs one drain cycle: it reclaims up to its batch size of the holds
// whose window and lease have both lapsed, one unit of work per victim, and
// reports what it took.
//
// The unit-of-work refusal is the routing stage's, in the same words and for
// the same reason: the loop opens units, and a service handed a unit it did
// not open would nest its drain inside somebody else's ending — the very
// nesting the reaper's own same-unit discipline is built to make impossible.
//
// A cycle ends when the sweep finds nothing, when the batch is spent, when
// the budget is spent, or when the caller's context is done. The budget is
// read BETWEEN iterations rather than at the end, which is the only place a
// reading bounds anything: a drain that checked once on its way out would
// have opened its whole batch under one budget and finished an unbounded
// amount of work after the budget it was given had already gone.
func (r *Reaper) Reap(ctx context.Context) (ReapReport, error) {
	if r.store.InUnitOfWork(ctx) {
		return ReapReport{}, fmt.Errorf("application: reaping refuses to run inside a unit of work it did not open")
	}
	var report ReapReport
	started := time.Now()
	for range r.batchSize {
		if err := ctx.Err(); err != nil {
			// The stop signal arriving between victims. The report is
			// returned with it, because what the cycle already reclaimed is
			// committed and is not un-reclaimed by the loop being cut short.
			return report, fmt.Errorf("application: reap: %w", err)
		}
		if time.Since(started) > r.cycleBudget {
			// The budget, spent. A cycle that stops here is a cycle that
			// stopped on purpose: whatever the sweep would have found next
			// is the next cycle's work, and the next cycle is one interval
			// away.
			return report, nil
		}
		// A retry re-runs the sweep and re-derives its victims, and it
		// must NEVER cache or replay an []ExpiredLease from a failed
		// attempt. ExpireLapsedLeases evaluates its predicate against
		// clock_timestamp(), so a retry is a fresh sweep against a later
		// clock and a possibly different victim set: a hold whose lease
		// was renewed a moment ago is not the victim it was, and a hold
		// behind it may have become one. Replaying the earlier answer would
		// close a hold this cycle no longer has any claim to.
		reclaimStarted := time.Now()
		done, result, err := r.reapWithLadder(ctx)
		if err != nil {
			// The cycle stops and the error travels out, which is the
			// ending ladder's posture rather than a page's: a victim whose
			// unit kept failing has left its hold open, and the next cycle
			// is where that gets picked up. There is no "skip this victim
			// and carry on" here, because the next victim is a later entry
			// in the same oldest-lease-first order and skipping ahead would
			// step over the row that is wedging the sweep.
			return report, err
		}
		if !done {
			// The sweep found nothing. This is the cycle's ordinary end —
			// an empty queue, not a failure — and the report says so.
			return report, nil
		}
		report.Scanned++
		outcome := ReapOutcomeExpired
		if result.skipped {
			report.Skipped++
			outcome = ReapOutcomeSkipped
		} else {
			report.Recovered++
		}
		r.observeReap(ReapObservation{
			RequestID:     result.requestID,
			UsageEventID:  result.eventSeq,
			FinalStatus:   result.finalStatus,
			Outcome:       outcome,
			ReclaimedLegs: result.reclaimedLegs,
			SweepDuration: time.Since(reclaimStarted),
		})
	}
	return report, nil
}

// observeReap hands one reclaimed victim to the wired observer, when one is.
func (r *Reaper) observeReap(o ReapObservation) {
	if r.ObserveReap != nil {
		r.ObserveReap(o)
	}
}

// reapResult is what one victim's unit of work reported back: the identity the
// observation carries, and the one boolean that separates the reaper's own
// ending from the one it found already written.
type reapResult struct {
	requestID     string
	finalStatus   string
	eventSeq      int64
	reclaimedLegs int
	skipped       bool
}

// reapWithLadder runs one victim's unit of work up to the reaper's budget,
// retrying whole on the store's own retryable classes. The shape is
// release's, and the two reasons the ending ladder's shape is what it is —
// re-judged by the sweep on every attempt, and a failure that is the only
// thing left to retry — are the same two here.
func (r *Reaper) reapWithLadder(ctx context.Context) (bool, reapResult, error) {
	for attempt := 0; attempt < reaperMaxAttempts; attempt++ {
		done, once, err := r.reapOnce(ctx)
		if err == nil {
			return done, once, nil
		}
		if !isRetryableStoreFailure(err) {
			return false, reapResult{}, err
		}
		if attempt+1 < reaperMaxAttempts {
			select {
			case <-ctx.Done():
				return false, reapResult{}, fmt.Errorf("application: reap: %w", ctx.Err())
			case <-time.After(reaperRetryBackoff << attempt):
			}
		}
	}
	// The budget ran out. The hold this ladder was reclaiming is still open
	// — every attempt rolled its whole unit back, the sweep's close
	// included — so it strands until a later cycle, which is the same
	// stranded-but-open posture an exhausted ending leaves behind and the
	// reason the next cycle exists at all.
	return false, reapResult{}, fmt.Errorf("application: reap: the reclaim unit did not settle after %d attempts", reaperMaxAttempts)
}

// reapOnce runs one victim's unit of work once: the sweep that closed its
// hold, the return, the request, the replay record, the expired fact last.
// Every step past the sweep is inside the same unit of work as the sweep
// itself, and that is the whole reason for the shape.
//
// ErrExpireOutsideUnitOfWork's own documentation states the tear: a sweep run
// bare commits each close the instant it happens, and a crash before the
// caller's unit opened would leave closed holds the feed never hears about.
// A unit that swept N victims and then tailed them in a second unit would be
// exactly that tear with one extra step of delay in it — the closes would be
// committed, the facts not yet. So the sweep and its tail are one unit, which
// is why the sweep's limit here is ONE: a unit whose append is its last
// statement holds the feed's stream row lock from the append to the commit,
// and moving the append earlier serialises more of the unit for no gain. A
// unit appending fifty victims' facts would therefore hold that global
// serialisation point across fifty victims' worth of tail work and stall
// every live settle in the process behind it. The trade is deliberate and
// it is a throughput-for-latency trade: a backlog drains at one victim per
// unit, the batch size bounds how many a cycle takes, and the loop's interval
// bounds how long a backlog waits between cycles. A drain that is slow is a
// capacity leak that costs nothing but time; a drain that is fast enough to
// stall a live settle costs a request.
//
// The boolean answers "did the sweep find a victim at all", which is how an
// empty queue ends a cycle without being a failure.
func (r *Reaper) reapOnce(ctx context.Context) (bool, reapResult, error) {
	var (
		swept  bool
		result reapResult
	)
	err := r.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := r.clock.TransactionTimestamp(txCtx)
		if err != nil {
			return err
		}
		victims, err := r.reservations.ExpireLapsedLeases(txCtx, 1)
		if err != nil {
			return fmt.Errorf("application: reap: sweep the lapsed leases: %w", err)
		}
		if len(victims) == 0 {
			// An empty sweep is a cycle's ordinary end, and there is
			// nothing to write: the unit commits having done nothing,
			// which is the cheapest way to say "the queue was empty".
			return nil
		}
		lease := victims[0]
		swept = true
		result = reapResult{requestID: string(lease.RequestID), finalStatus: string(execution.StatusFailed)}

		// The return, in the legs' stored ordinal order — the waterfall
		// order every writer of the projection walks, and the order the
		// fact publishes. The sweep read the legs back in that same order
		// out of the hold's own rows, so there is nothing to sort here and
		// nothing that could disagree about the order later. The port's
		// return is unconditional on the balances, so a publication that
		// shrank a ceiling cannot turn a reclaim into a failure; what it
		// is not is silent, which is why the count it answers with is
		// carried onto the observation.
		reclaimed, err := r.ledger.Return(txCtx, lease.Allocations)
		if err != nil {
			return fmt.Errorf("application: reap request %s: return the hold: %w", lease.RequestID, err)
		}
		result.reclaimedLegs = reclaimed

		// The abandoned word, through its own door and no other.
		// FailBeforeCommitment refuses FailedGatewayAbandoned outright —
		// that refusal is the domain's law that a surfaced refusal and an
		// abandonment are different shapes of ending — so the release unit
		// reaches the same status through the switch that names it, and the
		// reaper reaches it through FailAbandoned directly, for the reason
		// its own documentation gives: the runtime can no longer ask the
		// dead process whether a commitment happened, so the row finalises
		// without naming an attempt, and the schema's
		// failure-reason/attempt pairing refuses a row that named one.
		// Nothing was committed, so nothing may be named.
		request := execution.Request{ID: lease.RequestID, Status: execution.StatusExecuting}
		if err := request.FailAbandoned(now); err != nil {
			return err
		}
		finalised, err := r.requests.Finalise(txCtx, request)
		if err != nil {
			return fmt.Errorf("application: reap request %s: finalise the request: %w", lease.RequestID, err)
		}
		decided, err := r.intakes.Finalise(txCtx, lease.AccountID, lease.IdempotencyKey,
			execution.FinalFailed, "", execution.FailedGatewayAbandoned)
		if err != nil {
			return fmt.Errorf("application: reap request %s: finalise the replay record: %w", lease.RequestID, err)
		}
		// DIVERGENCE FROM releaseOnce, and the important one: a false here
		// is a TOLERATED SKIP, not a hard error that rolls the unit back.
		//
		// In releaseOnce a false is fatal because that unit holds the
		// reservation's compare-and-set and therefore owns the ending — a
		// closed hold with no deciding fact behind it is the orphan the
		// doctrine forbids, so the unit rolls back, the close included, and
		// the hold is left open for a retry or for the reaper. The reaper
		// is a SECOND door into the same request, and a second door has to
		// tolerate the first.
		//
		// The safety argument is the sweep's own predicate. The sweep
		// selected this hold out of the open set under FOR UPDATE SKIP
		// LOCKED, which is an exclusive claim on the row for as long as
		// this unit runs, and a request that was settled through a live
		// ending is by construction already terminal: that settle's own
		// reservations.Close moved the row out of open, so the sweep's
		// predicate would never have selected it in the first place. A
		// false can therefore only mean another RELEASE-CLASS ending — the
		// walk abandoning on its own, or an earlier cycle of this loop —
		// already wrote the request row and the replay pointer, and that
		// ending appended its own settlement-relevant fact. Rolling the
		// unit back over that would not reopen anything: the close has
		// been committed by the other ending or is about to be, and the
		// capacity is already given back. It would only lose this cycle's
		// reclaim and re-derive the same victim next cycle.
		//
		// So the tail continues, and the fact is still attempted below: a
		// hold this unit closed must have a word on the feed whatever else
		// has already been written for the request. The skip is recorded
		// on the observation instead of raised as an error, which is what
		// makes Skipped an operator-visible count rather than a hole in
		// the logs.
		result.skipped = !finalised || !decided

		fact, err := accounting.NewExpired(lease.RequestID, factLegs(lease.Allocations), now)
		if err != nil {
			return err
		}
		// The append is the unit's LAST statement, for the same reason the
		// release's is: the feed's row lock is held to the commit, so the
		// order facts are allocated is the order they become visible.
		seq, err := r.facts.Append(txCtx, fact)
		if err != nil {
			if errors.Is(err, persistence.ErrDuplicateFact) {
				// DIVERGENCE FROM releaseOnce, and the same argument again:
				// there ErrDuplicateFact is a NAMED BUG, because the CAS the
				// unit holds guarantees exactly one settlement-relevant
				// fact per request. The reaper's claim is a different
				// claim on a different table — the sweep's, taken on the
				// reservations — and the fact's dedup unique protects
				// usage_events, so nothing about the two arbitrates each
				// other. SKIP LOCKED skips a contended row rather than
				// waiting for it, so between the sweep's snapshot and this
				// unit's append a live release through the other door can
				// genuinely land this request's settlement-relevant fact
				// first. That is a real race with a deterministic winner,
				// not a bug, and the all-or-nothing error discipline the
				// endings are written on is not shaped for it: rolling
				// back here would roll back a capacity return and a sweep
				// over a hold the twin ending is about to leave alone, to
				// no end. The precedent is settleOrphanTail's, which
				// already reads this sentinel as the twin having won —
				// "done, not an error". The winner's fact stands beside
				// this hold's reclaim, and this victim is a skip.
				//
				// BUT ONLY IF the twin is a fact rather than a row: a
				// unique violation inside a unit of work puts that
				// transaction into the aborted state, and the engine
				// reports it at COMMIT rather than at the statement — the
				// Insert returns success, every later statement in the
				// unit fails, and pgx substitutes
				// ErrTxCommitRollback ("commit unexpectedly resulted in
				// rollback") for the COMMIT it never got to send. Reading
				// that as the twin having won would be claiming a commit
				// that did not happen: the hold would read open, the
				// capacity would read drawn, and the report would count a
				// reclaim that was rolled back. So the sentinel is read
				// where it can be true — at the statement, off a RETURNING
				// that produced no row — and the twin is a committed fact,
				// and this unit goes on to commit its own return behind
				// it.
				//
				// Which is only safe because the append writes the fact and
				// its sequence together. Told apart, a refusal would leave
				// the feed's counter committed over a row that was never
				// written, and a gap in append_seq is not something a
				// consumer can distinguish from a lost fact. One statement
				// is what makes "this unit still commits" an honest thing
				// to do here.
				result.skipped = true
				return nil
			}
			return fmt.Errorf("application: reap request %s: append the expiry: %w", lease.RequestID, err)
		}
		result.eventSeq = seq
		return nil
	})
	if err != nil {
		return false, reapResult{}, err
	}
	return swept, result, nil
}
