package application

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The reaper's fakes, and the one thing they add to the world's its siblings
// already have.
//
// Every other fake in this file answers the use case that names it, and the
// reaper is the first service in the package whose FIRST act is a batch CAS:
// the sweep decides which holds it is allowed to touch, before it has any
// other work. A fake that answered "here is your victim" on demand would test
// the tail against a premise the engine never agreed to — and the premise is
// the whole difficulty of the reaper, because its two tolerated skips are
// justified by what the sweep's predicate guarantees. So the sweep here
// evaluates that predicate, against the world's clock, and everything the
// reaper's own code must NOT do — re-derive eligibility, second-guess the
// boundary, sort the victims, reach for a second candidate — is a thing this
// fake refuses by construction, and a test that tried would watch the sweep
// answer the same way it always does.

// reapWorld is the reaper's own world rather than three more fields on
// admissionWorld: the reaper's tests never admit anything through the
// pipeline. They seed holds directly, in the state a dead process left behind,
// which is the only state the reaper is ever handed.
type reapWorld struct {
	// mu guards everything below, for the same reason admissionWorld's does:
	// single-threaded tests lock it too, and the rules stay uniform.
	mu sync.Mutex

	events    []string
	outsideTx int
	now       time.Time

	requests     map[identity.RequestID]execution.Request
	intakes      map[string]execution.Intake
	reservations map[identity.ReservationID]accounting.Reservation
	buckets      []*admissionBucket
	facts        []accounting.Fact
	returned     [][]accounting.Allocation

	// The sweep's knobs. sweepFailure fires for the first sweepFailures
	// sweeps, which is how a retryable store failure is staged; sweepSkipsOne
	// makes the next sweep leave its first eligible hold alone, modelling a
	// row another writer holds the lock on — with a limit of one there is no
	// second candidate to fall through to. sweepLatency is the budget a cycle
	// spends per sweep, so the cycle-budget test can reach the bound without
	// sleeping for a whole cycle's worth of tail work.
	sweepFailure  error
	sweepFailures int
	sweepSkipsOne bool
	sweepLatency  time.Duration

	// The tail's knobs. requestFinaliseLost and intakeFinaliseLost stage the
	// tolerated skips: a request row and a replay record that were already
	// written by the ending that won the race.
	requestFinaliseLost bool
	intakeFinaliseLost  bool
	factDuplicate       bool
	returnFailure       error
	returnFailures      int
	clockFailure        error
}

func newReapWorld() *reapWorld {
	return &reapWorld{
		now:          time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC),
		requests:     map[identity.RequestID]execution.Request{},
		intakes:      map[string]execution.Intake{},
		reservations: map[identity.ReservationID]accounting.Reservation{},
	}
}

func (w *reapWorld) note(event string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, event)
}

func (w *reapWorld) count(event string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, entry := range w.events {
		if entry == event {
			n++
		}
	}
	return n
}

func (w *reapWorld) intakeKey(accountID, idempotencyKey string) string {
	return accountID + "\x00" + idempotencyKey
}

func (w *reapWorld) available(bucketID string) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, bucket := range w.buckets {
		if bucket.id == bucketID {
			return bucket.available
		}
	}
	return -1
}

// reapSnapshot is what a rollback owes: every map and slice one reclaim unit
// could have touched, copied before it ran.
type reapSnapshot struct {
	requests     map[identity.RequestID]execution.Request
	intakes      map[string]execution.Intake
	reservations map[identity.ReservationID]accounting.Reservation
	buckets      []admissionBucket
	facts        []accounting.Fact
	returned     [][]accounting.Allocation
}

func (w *reapWorld) snapshot() reapSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	snap := reapSnapshot{
		requests:     make(map[identity.RequestID]execution.Request, len(w.requests)),
		intakes:      make(map[string]execution.Intake, len(w.intakes)),
		reservations: make(map[identity.ReservationID]accounting.Reservation, len(w.reservations)),
	}
	for id, request := range w.requests {
		snap.requests[id] = request
	}
	for key, intake := range w.intakes {
		snap.intakes[key] = cloneAdmissionIntake(intake)
	}
	for id, reservation := range w.reservations {
		snap.reservations[id] = cloneAdmissionReservation(reservation)
	}
	snap.buckets = make([]admissionBucket, len(w.buckets))
	for i, bucket := range w.buckets {
		snap.buckets[i] = *bucket
	}
	snap.facts = append([]accounting.Fact(nil), w.facts...)
	snap.returned = append([][]accounting.Allocation(nil), w.returned...)
	return snap
}

func (w *reapWorld) restore(snap reapSnapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.requests = snap.requests
	w.intakes = snap.intakes
	w.reservations = snap.reservations
	w.buckets = make([]*admissionBucket, len(snap.buckets))
	for i, bucket := range snap.buckets {
		clone := bucket
		w.buckets[i] = &clone
	}
	w.facts = snap.facts
	w.returned = snap.returned
}

// ---------------------------------------------------------------------------
// the seeds
// ---------------------------------------------------------------------------

// seedStrandedHold is what a dead process leaves behind: an open hold whose
// request row is executing, whose replay record is still waiting, and whose
// capacity was drawn from one bucket. The two clocks are offsets from the
// world's clock, so a test can say "lapsed an hour ago" or "expires at exactly
// now" and the sweep's predicate reads them the way it reads the engine's own
// timestamps.
func (w *reapWorld) seedStrandedHold(id identity.ReservationID, requestID identity.RequestID, account, key string, legs []accounting.Allocation, windowOffset, leaseOffset time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now
	w.reservations[id] = accounting.Reservation{
		ID:              id,
		RequestID:       requestID,
		PriceRevision:   "rev-1",
		InputUnitPrice:  1_000_000,
		OutputUnitPrice: 2_000_000,
		InputTokens:     71,
		MaxOutputTokens: 16,
		ReservedAmount:  103,
		Allocations:     append([]accounting.Allocation(nil), legs...),
		State:           accounting.StateOpen,
		CreatedAt:       now.Add(-time.Hour),
		ExpiresAt:       now.Add(windowOffset),
		LeaseOwner:      "dead-host:9",
		LeaseExpiresAt:  now.Add(leaseOffset),
	}
	w.requests[requestID] = execution.Request{
		ID:        requestID,
		AccountID: account,
		Status:    execution.StatusExecuting,
	}
	w.intakes[w.intakeKey(account, key)] = execution.Intake{
		AccountID:      account,
		IdempotencyKey: key,
		RequestDigest:  "digest",
		RequestID:      requestID,
		CreatedAt:      now.Add(-time.Hour),
	}
}

func (w *reapWorld) seedBucket(account, id string, available int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buckets = append(w.buckets, &admissionBucket{account: account, id: id, available: available, eligible: true})
}

// ---------------------------------------------------------------------------
// the store
// ---------------------------------------------------------------------------

// fakeReapStore is the admission store's shape over the reaper's world: it
// snapshots at begin, restores on a failed callback, and marks the context the
// repositories check. It refuses a context that is already done, for the reason
// its sibling does — a cycle that keeps sweeping on a cancelled context is not a
// cycle a stop signal can end.
type fakeReapStore struct {
	persistence.Store
	world *reapWorld
}

func (s fakeReapStore) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.world.note("begin")
	snapshot := s.world.snapshot()
	if err := fn(context.WithValue(ctx, reapTxKey{}, true)); err != nil {
		s.world.restore(snapshot)
		s.world.note("rollback")
		return err
	}
	s.world.note("commit")
	return nil
}

func (s fakeReapStore) InUnitOfWork(ctx context.Context) bool {
	marked, ok := ctx.Value(reapTxKey{}).(bool)
	return ok && marked
}

type reapTxKey struct{}

func inReapTx(ctx context.Context) bool {
	marked, ok := ctx.Value(reapTxKey{}).(bool)
	return ok && marked
}

// reapClock stands in for the storeClock over the reaper's world, with the same
// outside-a-unit-of-work check every repository fake runs.
type reapClock struct{ world *reapWorld }

func (c reapClock) TransactionTimestamp(ctx context.Context) (time.Time, error) {
	if !inReapTx(ctx) {
		c.world.outsideTx++
	}
	if c.world.clockFailure != nil {
		return time.Time{}, c.world.clockFailure
	}
	return c.world.now, nil
}

// ---------------------------------------------------------------------------
// the ports
// ---------------------------------------------------------------------------

// fakeReapReservations carries the sweep, and the sweep is the whole behaviour
// under test that the store would otherwise own. Its predicate is the
// statement's: open, window lapsed, lease lapsed, oldest lease first, up to the
// limit — where "lapsed" is the strict `<` the SQL carries, which is what makes
// the boundaries at exactly now, and one nanosecond either side of it, three
// different answers.
//
// The unit-of-work refusal is the real one's, because the real one is what
// keeps the reaper's discipline honest: a sweep arriving with no unit in its
// context is the tear ErrExpireOutsideUnitOfWork exists to prevent, and a fake
// that did not refuse it would let a reaper that got the nesting wrong pass
// here anyway.
type fakeReapReservations struct {
	persistence.ReservationRepository
	world *reapWorld
}

func (f fakeReapReservations) ExpireLapsedLeases(ctx context.Context, limit int) ([]persistence.ExpiredLease, error) {
	if !inReapTx(ctx) {
		f.world.outsideTx++
		return nil, fmt.Errorf("fake: sweep the lapsed leases: %w", persistence.ErrExpireOutsideUnitOfWork)
	}
	if limit < 1 {
		return nil, fmt.Errorf("fake: sweep the lapsed leases: a limit below one is refused, got %d", limit)
	}
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	f.world.events = append(f.world.events, "reservation.sweep")
	if f.world.sweepFailures > 0 {
		f.world.sweepFailures--
		return nil, f.world.sweepFailure
	}
	if f.world.sweepLatency > 0 {
		// The cycle budget is read off the wall clock, so a test that wants to
		// reach it spends it here rather than sleeping for it. The wait itself
		// is real, so a cancelled context still ends a sweep.
		latency := f.world.sweepLatency
		f.world.mu.Unlock()
		select {
		case <-time.After(latency):
		case <-ctx.Done():
			f.world.mu.Lock()
			return nil, ctx.Err()
		}
		f.world.mu.Lock()
	}

	now := f.world.now
	var eligible []identity.ReservationID
	for id, reservation := range f.world.reservations {
		if reservation.State != accounting.StateOpen {
			continue
		}
		if !reservation.ExpiresAt.Before(now) || !reservation.LeaseExpiresAt.Before(now) {
			continue
		}
		eligible = append(eligible, id)
	}
	// Oldest lease first, with the id as the tiebreak, exactly as the statement
	// orders. A cycle asks for one victim, so this ordering is what decides
	// WHICH hold a cycle reclaims when several have lapsed.
	sort.Slice(eligible, func(i, j int) bool {
		left, right := f.world.reservations[eligible[i]], f.world.reservations[eligible[j]]
		if !left.LeaseExpiresAt.Equal(right.LeaseExpiresAt) {
			return left.LeaseExpiresAt.Before(right.LeaseExpiresAt)
		}
		return eligible[i] < eligible[j]
	})
	if len(eligible) > limit {
		eligible = eligible[:limit]
	}
	if f.world.sweepSkipsOne && len(eligible) > 0 {
		// SKIP LOCKED's shape: the row another writer holds the lock on is not
		// this sweep's, and with a limit of one there is nothing behind it to
		// fall through to.
		eligible = eligible[1:]
	}

	victims := make([]persistence.ExpiredLease, 0, len(eligible))
	for _, id := range eligible {
		reservation := f.world.reservations[id]
		reservation.State = accounting.StateExpired
		reservation.ClosedAt = now
		f.world.reservations[id] = reservation
		intake := f.world.replayIdentityLocked(reservation.RequestID)
		victims = append(victims, persistence.ExpiredLease{
			ID:              reservation.ID,
			RequestID:       reservation.RequestID,
			PriceRevision:   reservation.PriceRevision,
			InputUnitPrice:  reservation.InputUnitPrice,
			OutputUnitPrice: reservation.OutputUnitPrice,
			InputTokens:     reservation.InputTokens,
			MaxOutputTokens: reservation.MaxOutputTokens,
			LeaseOwner:      reservation.LeaseOwner,
			AccountID:       intake.AccountID,
			IdempotencyKey:  intake.IdempotencyKey,
			Allocations:     append([]accounting.Allocation(nil), reservation.Allocations...),
		})
	}
	return victims, nil
}

// replayIdentityLocked is the sweep's replay-identity join read the way the
// statement reads it: the request row's account, and the record that row's key
// was born under. It is a second lookup here only because the seeded world
// keeps the two in different maps.
func (w *reapWorld) replayIdentityLocked(requestID identity.RequestID) execution.Intake {
	account := w.requests[requestID].AccountID
	for _, intake := range w.intakes {
		if intake.RequestID == requestID && intake.AccountID == account {
			return intake
		}
	}
	return execution.Intake{}
}

type fakeReapLedger struct {
	persistence.QuotaProjectionRepository
	world *reapWorld
}

func (f fakeReapLedger) Return(ctx context.Context, legs []accounting.Allocation) (int, error) {
	if !inReapTx(ctx) {
		f.world.outsideTx++
	}
	f.world.note("ledger.return")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if f.world.returnFailures > 0 {
		f.world.returnFailures--
		return 0, f.world.returnFailure
	}
	f.world.returned = append(f.world.returned, append([]accounting.Allocation(nil), legs...))
	landed := 0
	for _, leg := range legs {
		for _, bucket := range f.world.buckets {
			if bucket.id == leg.FundingBucketID {
				bucket.available += leg.Amount
				landed++
			}
		}
	}
	return landed, nil
}

// fakeReapRequests is the admission requests fake over the reaper's world, with
// the knob that stages a tolerated skip: a request row that was already written
// by the ending which won the race.
type fakeReapRequests struct {
	persistence.RequestRepository
	world *reapWorld
}

func (f fakeReapRequests) Finalise(ctx context.Context, request execution.Request) (bool, error) {
	if !inReapTx(ctx) {
		f.world.outsideTx++
	}
	f.world.note("request.finalise")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if f.world.requestFinaliseLost {
		// The store answered, and the row did not move: nobody won it, it
		// simply refused the ending. The caller must not read this as done.
		return false, nil
	}
	stored, ok := f.world.requests[request.ID]
	if !ok || stored.Status != execution.StatusExecuting {
		return false, nil // the row moved on: the loser reads the winner's decision
	}
	stored.Status = request.Status
	stored.RejectionReason = request.RejectionReason
	stored.FailureReason = request.FailureReason
	stored.CommittedAttemptID = request.CommittedAttemptID
	stored.FinishedAt = request.FinishedAt
	f.world.requests[request.ID] = stored
	return true, nil
}

type fakeReapIntakes struct {
	persistence.IntakeRepository
	world *reapWorld
}

func (f fakeReapIntakes) Finalise(ctx context.Context, accountID, idempotencyKey string, status execution.FinalStatus, rejection execution.RejectionReason, failure execution.FailureReason) (bool, error) {
	if !inReapTx(ctx) {
		f.world.outsideTx++
	}
	f.world.note("intake.finalise")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if f.world.intakeFinaliseLost {
		return false, nil
	}
	key := f.world.intakeKey(accountID, idempotencyKey)
	intake, ok := f.world.intakes[key]
	if !ok {
		return false, fmt.Errorf("fake: intake for %s: %w", key, persistence.ErrNotFound)
	}
	if intake.FinalStatus != nil {
		return false, nil // the pointer was written once, by whoever won it
	}
	if err := intake.Finalise(status, rejection, failure); err != nil {
		return false, err
	}
	f.world.intakes[key] = intake
	return true, nil
}

type fakeReapFacts struct{ world *reapWorld }

func (f fakeReapFacts) Append(ctx context.Context, fact accounting.Fact) (int64, error) {
	f.world.mu.Lock()
	duplicate := f.world.factDuplicate
	f.world.mu.Unlock()
	if duplicate {
		// The dedup unique refused: a settlement-relevant fact already stands for
		// this request. The engine's own law, staged for the tests that pin the
		// reaper's reading of it.
		return 0, fmt.Errorf("fake: fact for request %s: %w", fact.RequestID, persistence.ErrDuplicateFact)
	}
	if !inReapTx(ctx) {
		f.world.outsideTx++
	}
	f.world.note("fact.append")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	fact.AppendSeq = int64(len(f.world.facts)) + 1
	f.world.facts = append(f.world.facts, fact)
	return fact.AppendSeq, nil
}

// ---------------------------------------------------------------------------
// the fixture
// ---------------------------------------------------------------------------

// reaperFixture is what the reaper's tests share: one account, one bucket, one
// hold whose capacity came out of that bucket, and the clock the sweep judges
// both horizons against.
type reaperFixture struct {
	reaper   *Reaper
	world    *reapWorld
	account  string
	key      string
	request  identity.RequestID
	hold     identity.ReservationID
	capacity int64
	legs     []accounting.Allocation
}

const (
	reaperTestAccount = "account-1"
	reaperTestKey     = "replay-key-1"
	reaperTestBucket  = "bucket-1"
)

func newReaperFixture(t testing.TB, batch int, budget time.Duration) *reaperFixture {
	t.Helper()
	world := newReapWorld()
	world.seedBucket(reaperTestAccount, reaperTestBucket, 10_000)
	fixture := &reaperFixture{
		world:    world,
		account:  reaperTestAccount,
		key:      reaperTestKey,
		request:  identity.RequestID("req-reaped-1"),
		hold:     identity.ReservationID("res-reaped-1"),
		capacity: 10_000,
		legs:     []accounting.Allocation{{FundingBucketID: reaperTestBucket, Amount: 103, Ordinal: 1}},
	}
	fixture.reaper = NewReaper(
		fakeReapStore{world: world},
		fakeReapReservations{world: world},
		fakeReapLedger{world: world},
		fakeReapRequests{world: world},
		fakeReapIntakes{world: world},
		fakeReapFacts{world: world},
		ReaperConfig{BatchSize: batch, CycleBudget: budget},
	)
	// The production clock is a SELECT this package cannot fake, which is the
	// seam's whole reason for existing.
	fixture.reaper.clock = reapClock{world}
	return fixture
}

// stranded seeds the fixture's one hold with both clocks an hour past, which
// is the state a dead process leaves and the only state the reaper reclaims.
func (f *reaperFixture) stranded() {
	f.world.seedStrandedHold(f.hold, f.request, f.account, f.key, f.legs, -time.Hour, -time.Hour)
}

// addHold seeds one more stranded hold in the same account, for the tests
// about a cycle's own bounds rather than about one victim.
func (f *reaperFixture) addHold(n int, leaseOffset time.Duration) identity.ReservationID {
	id := identity.ReservationID(fmt.Sprintf("res-reaped-%d", n))
	f.world.seedStrandedHold(id, identity.RequestID(fmt.Sprintf("req-reaped-%d", n)),
		f.account, fmt.Sprintf("replay-key-%d", n), f.legs, -time.Hour, leaseOffset)
	return id
}

func (f *reaperFixture) reservation(id identity.ReservationID) accounting.Reservation {
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	return f.world.reservations[id]
}

func (f *reaperFixture) requestRow(id identity.RequestID) execution.Request {
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	return f.world.requests[id]
}

func (f *reaperFixture) intake(account, key string) execution.Intake {
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	return f.world.intakes[f.world.intakeKey(account, key)]
}

func (f *reaperFixture) factCount() int {
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	return len(f.world.facts)
}

func (f *reaperFixture) factAt(i int) accounting.Fact {
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	return f.world.facts[i]
}
