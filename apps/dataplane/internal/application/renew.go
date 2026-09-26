package application

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/executors"
)

// The routing stage's lease renewal: the thing that keeps a live hold from
// being reclaimed while its provider call runs.
//
// The relationship between the two clocks is the whole design. The hold
// window is the reservation's lifetime, stamped once at admission and never
// extended; the lease is this process's claim that it is still working on the
// hold, and it lapses on a shorter clock so a dead process's holds are
// recovered quickly. A provider call is exactly the span during which the
// process is alive and yet silent about the hold — nothing else in the
// request's life takes longer than a database write — so the renewal runs
// wrapped around each executor call, on the walk's own level: the walk starts
// it before the call and stops it when the call returns, and the call's
// context carries both the renewal's permission to continue and the one
// deadline every call answers to.
//
// The renewer is deliberately not a port, not a goroutine pool and not a
// feature of the executors: an executor knows one provider call and nothing
// of the reservation behind it, while the walk knows both. Keeping the
// renewal here is what keeps the executor's contract single-call and the
// reservation's bookkeeping in the stage that owns the reservation.
const (
	// leaseRenewalTimeout bounds one renewal's own unit of work. A renewal
	// is one conditional update; a database that cannot answer one inside
	// five seconds is a database whose other work is failing too, and the
	// both-clocks rule below makes a missed renewal survivable anyway.
	leaseRenewalTimeout = 5 * time.Second
)

// ExecutionConfig is what the composition root tells the routing stage about
// the time one provider call may take. Both horizons arrive from the
// validated configuration — the ceiling strictly below the reservation hold
// window, so a call that used all of it plus its settlement endings would
// still finish inside the hold it was admitted under.
type ExecutionConfig struct {
	// MaxDuration is the deadline the walk puts on every executor call's
	// context. It is the one budget a provider call answers to: streaming
	// clients wait as long as the provider produces, and a stalled provider
	// must not hold the walk — or the hold it stands behind — open forever.
	MaxDuration time.Duration

	// LeaseTTL is how long one renewal keeps the process's claim on the hold
	// alive. The renewer asks every TTL third, so a single missed renewal
	// never lapses the lease, and the first renewal lands immediately — the
	// walk starts after the admission unit stamped the lease, and a slow
	// unit's TTL may already be part-spent by the time the first call runs.
	LeaseTTL time.Duration
}

// executionContext wraps one executor call's context: the call's own deadline
// on top of the walk's context, and the renewal goroutine whose life is
// exactly the call's. The leaseDeadline argument is the walk's chain — the
// last expiry a renewer of this walk wrote, the admission's stamp for the
// first call — and the returned stop ends the renewal, waits for it, and
// reports both of the walk's facts: whether the renewal ended the call (a
// renewal that found the hold gone from the open set cancelled the context,
// and the walk owes that call the abandoned ending, not a disposition), and
// the deadline the renewer last wrote, which is the next candidate's chain
// seed. The stop must be called on the call's every exit — it holds the
// call's cancel, and a renewer left running would renew a hold nobody will
// ever settle or release.
//
// The renewal's deadline chain never reads this process's clock for expiry:
// the first renewal extends the lease to the chain's seed plus the TTL, and
// every later one extends the previous deadline by the TTL. The chain is
// therefore monotone by construction — a renewal can never write an earlier
// expiry than the one before it, and a fall-through to the next candidate
// continues from where the last call's renewer left off instead of restarting
// from the admission stamp, which a long first call would already have left
// behind — and clock skew between this process and the store's stamp never
// enters the arithmetic, because the store's own stamp is the only instant
// the chain starts from.
//
// A renewal that reports the hold no longer open — closed by a concurrent
// settlement, or swept — cancels the call's context at once: the provider
// call is working on a hold nobody owns, and the ending that closed it has
// already been stated by whoever closed it. A renewal that merely ERRORS is
// not that: the deadline on the reservation row is whatever the last
// successful renewal wrote, the next tick re-asks, and an outage shorter
// than the remaining hold window survives.
func (r *ChatRouting) executionContext(ctx context.Context, admitted *Admission, leaseDeadline time.Time) (context.Context, func() (bool, time.Time)) {
	callCtx, cancel := context.WithDeadline(ctx, time.Now().UTC().Add(r.execution.MaxDuration))
	stopRenewing := make(chan struct{})
	doneRenewing := make(chan struct{})
	var lost atomic.Bool

	deadline := leaseDeadline.Add(r.execution.LeaseTTL)
	go func() {
		defer close(doneRenewing)
		ticker := time.NewTicker(r.execution.LeaseTTL / 3)
		defer ticker.Stop()
		for {
			if !r.renewLeaseOnce(ctx, admitted, deadline) {
				// The hold is gone from the open set: cancel the call now,
				// so the walk learns of it from the call's own ending
				// instead of spending the rest of its budget on a provider
				// whose answer nobody can settle.
				lost.Store(true)
				cancel()
				return
			}
			select {
			case <-stopRenewing:
				return
			case <-callCtx.Done():
				return
			case <-ticker.C:
				deadline = deadline.Add(r.execution.LeaseTTL)
			}
		}
	}()

	return callCtx, func() (bool, time.Time) {
		close(stopRenewing)
		<-doneRenewing
		cancel()
		// Reading deadline here is race-free: the goroutine has finished —
		// doneRenewing's close is the handoff — so no renewal can be
		// writing it while the walk carries it to the next candidate.
		return lost.Load(), deadline
	}
}

// executeWithRenewal pairs the renewer's stop with the call's every exit,
// panics included. An adapter panic does not end the process — the transport's
// handler recovers it — but it would end this call's straight-line flow, and
// a renewer whose stop never ran would keep renewing the hold forever: a
// goroutine leak, a reservation that never leaves the open set, and a
// permanent defeat of the sweep. The deferred stop runs on the panic's way
// out, so the pairing holds on the paths the walk cannot name.
func (r *ChatRouting) executeWithRenewal(ctx context.Context, admitted *Admission, leaseDeadline *time.Time, executor executors.Executor, spec executors.AttemptSpec, reply Reply) (result executors.Result, renewalLost bool) {
	callCtx, stop := r.executionContext(ctx, admitted, *leaseDeadline)
	defer func() {
		renewalLost, *leaseDeadline = stop()
	}()
	result = executor.Execute(callCtx, spec, reply)
	return
}

// renewLeaseOnce runs one renewal as its own short unit: detached from the
// caller's cancellation — a caller walking away is no reason to leave the
// claim lapsed while the store still answers — and bounded by its own
// timeout. True means the lease stands; false means the hold is no longer
// this process's to renew, and the caller must stop. An error is neither:
// the claim stands until a renewal says otherwise, and the next tick asks
// again.
func (r *ChatRouting) renewLeaseOnce(ctx context.Context, admitted *Admission, leaseExpiresAt time.Time) bool {
	renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseRenewalTimeout)
	defer cancel()
	extended, err := r.reservations.RenewLease(renewCtx, admitted.ReservationID, admitted.LeaseOwner, leaseExpiresAt)
	if err != nil {
		return true
	}
	return extended
}
