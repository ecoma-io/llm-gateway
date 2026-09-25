package application

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/executors"
)

// The renewal tests. The renewer is the walk's claim on the hold it is
// spending: started before the provider call, stopped when the call returns,
// its deadline chain built from the admission-stamped expiry so no clock this
// process reads decides when a lease expires. These tests watch the chain
// climb, watch a hold-reported-gone stop it, and watch a mere error ride
// through — the both-clocks bargain the lease lives on.

// fakeRenewals wraps the world's reservations fake with a memory of what the
// walk asked for: every renewal's requested expiry, and the expiry the row
// carried when the first renewal found it. The wrapping is the observation
// point; the answer still comes from the world.
type fakeRenewals struct {
	fakeAdmissionReservations

	mu       sync.Mutex
	asked    []time.Time
	admitted time.Time // the lease expiry the first renewal found stamped on the row
}

func (f *fakeRenewals) RenewLease(ctx context.Context, id identity.ReservationID, owner string, leaseExpiresAt time.Time) (bool, error) {
	f.mu.Lock()
	if len(f.asked) == 0 {
		f.world.mu.Lock()
		if reservation, ok := f.world.reservations[id]; ok {
			f.admitted = reservation.LeaseExpiresAt
		}
		f.world.mu.Unlock()
	}
	f.asked = append(f.asked, leaseExpiresAt)
	f.mu.Unlock()
	return f.fakeAdmissionReservations.RenewLease(ctx, id, owner, leaseExpiresAt)
}

// renewWatchingExecutor holds its call open until it is released or its
// context is cancelled, and remembers which — the observation the renewal
// tests assert on, because the cancellation a lost lease produces is felt by
// the call, not by the walk.
type renewWatchingExecutor struct {
	release <-chan struct{}

	mu     sync.Mutex
	calls  int
	ctxErr error
}

func (e *renewWatchingExecutor) Execute(ctx context.Context, spec executors.AttemptSpec, sink executors.Sink) executors.Result {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	select {
	case <-e.release:
		// The answer arrives through the sink — the walk reads its
		// commitment, not the executor's claim, so a success that never
		// delivered is a failed attempt.
		_ = sink.Content([]byte(`{"answer":true}`))
		return okExec(3, 4)
	case <-ctx.Done():
		e.mu.Lock()
		e.ctxErr = ctx.Err()
		e.mu.Unlock()
		return failExec(execution.ErrorRateLimited)
	}
}

// TestTheRenewalChainStartsImmediatelyAndClimbsByTheLease: the first renewal
// lands the moment the call starts and asks for the admission-stamped expiry
// plus the lease; every later one asks for the previous deadline plus the
// lease again. The chain is therefore monotone by construction and a store
// clock's opinion never enters it, and the walk's last claim stands on the
// row where the reaper reads it.
func TestTheRenewalChainStartsImmediatelyAndClimbsByTheLease(t *testing.T) {
	routing, world, _, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	renewals := &fakeRenewals{fakeAdmissionReservations: fakeAdmissionReservations{world: world}}
	routing.reservations = renewals
	cfg := ExecutionConfig{MaxDuration: 30 * time.Second, LeaseTTL: 25 * time.Millisecond}
	routing.execution = cfg
	release := make(chan struct{})
	time.AfterFunc(200*time.Millisecond, func() { close(release) })
	exec := &renewWatchingExecutor{release: release}
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{"backend-a": exec})

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeServed {
		t.Fatalf("outcome = %s, want served — the call outlived the ticks and was settled", outcome.Kind)
	}
	if len(renewals.asked) < 4 {
		t.Fatalf("the renewer asked %d times, want at least 4 — a 200ms call under a 25ms lease renews repeatedly", len(renewals.asked))
	}
	if got := renewals.asked[0].Sub(renewals.admitted); got != cfg.LeaseTTL {
		t.Fatalf("the first renewal asked for admission+lease: %s after the stamped expiry, want exactly the lease %s", got, cfg.LeaseTTL)
	}
	for i := 1; i < len(renewals.asked); i++ {
		if got := renewals.asked[i].Sub(renewals.asked[i-1]); got != cfg.LeaseTTL {
			t.Fatalf("renewal %d asked %s after its predecessor, want exactly the lease %s — the chain is previous+ttl, always", i, got, cfg.LeaseTTL)
		}
	}
	if reservation := admissionReservationRow(t, world); !reservation.LeaseExpiresAt.Equal(renewals.asked[len(renewals.asked)-1]) {
		t.Fatalf("the row's lease = %s, want the chain's last ask %s — the claim lives where the reaper reads it", reservation.LeaseExpiresAt, renewals.asked[len(renewals.asked)-1])
	}
}

// TestARenewalReportingTheHoldGoneCancelsTheCall: a renewal answered false
// means the hold is no longer this process's — closed by a concurrent
// settlement or swept — so the renewer stops and the call's context is
// cancelled at once: a provider answer nobody can settle is not worth its
// remaining budget.
func TestARenewalReportingTheHoldGoneCancelsTheCall(t *testing.T) {
	routing, world, _, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	world.renewalGone = 1
	renewals := &fakeRenewals{fakeAdmissionReservations: fakeAdmissionReservations{world: world}}
	routing.reservations = renewals
	cfg := ExecutionConfig{MaxDuration: 30 * time.Second, LeaseTTL: 25 * time.Millisecond}
	routing.execution = cfg
	release := make(chan struct{}) // never closed: only the renewal can end this call
	exec := &renewWatchingExecutor{release: release}
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{"backend-a": exec})

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	exec.mu.Lock()
	ctxErr := exec.ctxErr
	exec.mu.Unlock()
	if !errors.Is(ctxErr, context.Canceled) {
		t.Fatalf("the call's context error = %v, want canceled — the false renewal stops the call", ctxErr)
	}
	if len(renewals.asked) != 1 {
		t.Fatalf("the renewer asked %d times, want exactly 1 — a hold reported gone is not asked about again", len(renewals.asked))
	}
	if outcome.Kind != OutcomeAbandoned {
		t.Fatalf("outcome = %s, want abandoned — the walk learns of the loss from the cancelled call", outcome.Kind)
	}
}

// TestARenewingErrorKeepsTheClaimAndTheCall: a renewal that merely ERRORS is
// not evidence the hold is gone — the row's deadline is the last successful
// renewal's, the next tick asks again, and an outage shorter than the
// remaining hold window survives. The call runs on; the chain, when answers
// return, is exactly as continuous as it ever was.
func TestARenewingErrorKeepsTheClaimAndTheCall(t *testing.T) {
	routing, world, _, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	world.renewalFailures = 2
	renewals := &fakeRenewals{fakeAdmissionReservations: fakeAdmissionReservations{world: world}}
	routing.reservations = renewals
	cfg := ExecutionConfig{MaxDuration: 30 * time.Second, LeaseTTL: 25 * time.Millisecond}
	routing.execution = cfg
	release := make(chan struct{})
	time.AfterFunc(200*time.Millisecond, func() { close(release) })
	exec := &renewWatchingExecutor{release: release}
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{"backend-a": exec})

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeServed {
		t.Fatalf("outcome = %s, want served — errors do not end the call", outcome.Kind)
	}
	exec.mu.Lock()
	ctxErr := exec.ctxErr
	exec.mu.Unlock()
	if ctxErr != nil {
		t.Fatalf("the call's context error = %v, want none — the renewal errors never cancelled it", ctxErr)
	}
	if len(renewals.asked) < 4 {
		t.Fatalf("the renewer asked %d times, want at least 4 — two errors then the chain continued", len(renewals.asked))
	}
	for i := 1; i < len(renewals.asked); i++ {
		if got := renewals.asked[i].Sub(renewals.asked[i-1]); got != cfg.LeaseTTL {
			t.Fatalf("renewal %d asked %s after its predecessor, want exactly the lease %s — the chain does not stutter for an error", i, got, cfg.LeaseTTL)
		}
	}
	if reservation := admissionReservationRow(t, world); !reservation.LeaseExpiresAt.Equal(renewals.asked[len(renewals.asked)-1]) {
		t.Fatalf("the row's lease = %s, want the chain's last ask %s", reservation.LeaseExpiresAt, renewals.asked[len(renewals.asked)-1])
	}
}
