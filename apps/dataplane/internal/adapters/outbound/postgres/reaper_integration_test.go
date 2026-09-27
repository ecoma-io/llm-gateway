//go:build integration

package postgres

// The reaper's store half. The landed suite beside it
// (TestIntegrationSeamReleaseRacesTheReaperToTheEnding) proves that a live
// release and a raw sweep of the same hold resolve to exactly one ending, and
// it does it by driving both doors by hand through the repositories, because
// an outbound adapter is constructed at the composition root and by nothing
// else — this package cannot import internal/application. This file follows
// that convention rather than inventing a second way around it, and it is
// explicit about what that costs: the drain loop, the tolerated skips and the
// batch bound are the reaper service's decisions, and they are proved in the
// application package against fakes. What is proved HERE is the half no fake
// can say — that the adapters under the reaper do the real thing. The sweep
// claims with FOR UPDATE SKIP LOCKED against clock_timestamp(), the capacity
// really comes back through the projection repository, the append really takes
// the feed's sequence and its envelope guard, and the two tolerated skips
// really are the sentinels the engine raises rather than shapes this file
// invented.
//
// It runs on its own throwaway database for the reason the seam test does: the
// sweep is a batch over the whole reservations table, so on a shared fixture
// earlier runs' holds — long since lapsed — would buy the sweep's limit before
// it ever reached the hold under test. Every table-wide count below is
// therefore a count of rows this file wrote.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// reapOutcome is what one victim's unit of work reported back. Scanned is the
// sweep's claim; Recovered and Skipped are the two ways a tail that ran can
// end, and the invariant between them is the reaper's own: scanned is recovered
// plus skipped, and a skipped victim is one this unit closed and then found
// already stated.
type reapOutcome struct {
	Scanned   int
	Recovered int
	Skipped   int
	victim    *persistence.ExpiredLease
}

// errToleratedSkip is gone on purpose, and its absence is the point. The
// tolerated skips COMMIT: the hold was closed inside the unit and the capacity
// returned, so the unit that cannot restate what another ending already wrote
// returns nil and records the skip in its outcome. A sentinel returned from
// inside the closure would roll the whole unit back and un-return the leg —
// which is the mistake this file's two tolerated-skip tests exist to catch.

// reapOne is one victim's whole unit of work, written the way the service
// writes it and in the order it writes it: the clock, the sweep's exclusive
// claim, the capacity's return, the request's failure, the replay record's
// pointer, and the fact LAST. A zero outcome with a nil error is the sweep's
// ordinary empty answer.
//
// The two tolerated skips are the reaper's two departures from the routing
// stage's release, and they are the two places this file is most worth having:
// a finalise that lost its race, and a fact the twin had already appended.
// Both COMMIT.
func reapOne(ctx context.Context, repos integrationRepositories) (reapOutcome, error) {
	var outcome reapOutcome
	err := repos.store.WithinTx(ctx, func(txCtx context.Context) error {
		var now time.Time
		if err := repos.store.Querier(txCtx).QueryRowContext(txCtx, "SELECT transaction_timestamp()").Scan(&now); err != nil {
			return err
		}
		expired, err := repos.reserves.ExpireLapsedLeases(txCtx, 1)
		if err != nil {
			return err
		}
		if len(expired) == 0 {
			return nil
		}
		victim := expired[0]
		outcome.victim = &victim
		outcome.Scanned = 1

		if _, err := repos.quota.Return(txCtx, victim.Allocations); err != nil {
			return err
		}
		request := execution.Request{ID: victim.RequestID, Status: execution.StatusExecuting}
		if err := request.FailAbandoned(now); err != nil {
			return err
		}
		finalised, err := repos.requests.Finalise(txCtx, request)
		if err != nil {
			return err
		}
		decided, err := repos.intakes.Finalise(txCtx, victim.AccountID, victim.IdempotencyKey, execution.FinalFailed, "", execution.FailedGatewayAbandoned)
		if err != nil {
			return err
		}
		if !finalised || !decided {
			// The other ending got there first. The hold is already closed and
			// the capacity already returned inside this unit, so this commits
			// rather than unwinding: rolling back would reopen nothing and
			// un-return a leg for no end.
			outcome.Skipped = 1
			return nil
		}
		fact, err := accounting.NewExpired(victim.RequestID, integrationFactLegs(victim.Allocations), now)
		if err != nil {
			return err
		}
		if _, err := repos.facts.Append(txCtx, fact); err != nil {
			if errors.Is(err, persistence.ErrDuplicateFact) {
				// The twin's fact is already on the feed, which is the other
				// door having won. "Done, not an error", as the orphan tail
				// already reads the same sentinel.
				outcome.Skipped = 1
				return nil
			}
			return err
		}
		outcome.Recovered = 1
		return nil
	})
	return outcome, err
}

// reaperFixture is one throwaway database, one store, and the repositories a
// reaper's unit of work is written over — plus the catalog scope, which is
// per-fixture and not per-call. catalogScope seeds the suite's alias, its
// wildcard group and its named group version on every call, so a fixture that
// dropped the value and asked again at drawdown time would be asking for a
// different alias than the one its grants were published against.
type reaperFixture struct {
	db    *sql.DB
	store persistence.Store
	repos integrationRepositories
	scope integrationScope
}

func reaperFixtureOnThrowaway(t *testing.T, name string) reaperFixture {
	t.Helper()
	integrationThrowawaySerialise(t)
	db := integrationThrowawayDatabase(t, name)
	integrationRuntimeSchema(t, db)
	store := New(db)
	repos := integrationRepos(t, store)
	return reaperFixture{db: db, store: store, repos: repos, scope: repos.catalogScope(t)}
}

// admissionFixtureView is the landed seam test's fixture seen as a value, so
// this file can reuse the one admission unit every scenario seeds through
// rather than re-deriving admission's write order.
func (f reaperFixture) admissionFixture() admissionFixture {
	return admissionFixture{db: f.db, store: f.store, repos: f.repos, scope: f.scope}
}

// seedStrandedHold opens one hold through a real admission unit — the request
// row, the replay record and the legs the store actually granted, in
// admission's own order — and then backdates the hold's whole clock
// vocabulary, which is what a dead process leaves behind. It returns the
// request the hold belongs to and the hold itself.
func seedStrandedHold(t *testing.T, ctx context.Context, f reaperFixture, account string, capacity, hold int64) (identity.RequestID, identity.ReservationID) {
	t.Helper()
	integrationSeedProjection(t, ctx, f.repos, account, true, time.Now().UTC().Add(24*time.Hour), capacity)
	admitted, err := integrationRunAdmissionUnit(ctx, f.admissionFixture(), account, account+"-key", "b13-digest", "", hold)
	if err != nil || !admitted.admitted {
		t.Fatalf("seeding the stranded hold: outcome %+v error %v", admitted, err)
	}
	integrationBackdateHold(t, f.db, admitted.reservation)
	return admitted.requestID, admitted.reservation
}

// moveHoldClocks puts one hold's two horizons where the test's table says they
// are. The passing ones go into the future rather than into the past, so the
// row still satisfies the schema's CHECK that the lease outlives the window
// and the window outlives the creation.
func moveHoldClocks(t *testing.T, db *sql.DB, id identity.ReservationID, windowPast, leasePast bool) {
	t.Helper()
	window := "transaction_timestamp() + interval '1 minute'"
	lease := "transaction_timestamp() + interval '2 minutes'"
	if windowPast {
		window = "transaction_timestamp() - interval '2 minutes'"
	}
	if leasePast {
		lease = "transaction_timestamp() - interval '1 minute'"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "UPDATE public.reservations SET expires_at = "+window+", lease_expires_at = "+lease+" WHERE id = $1", string(id)); err != nil {
		t.Fatalf("moving hold %s's horizons: %v", id, err)
	}
}

// integrationIntakeStatus reads one replay record's terminal pair, which is
// the whole claim a replay answers from: the same record, the same answer,
// however many times it is asked.
func integrationIntakeStatus(t *testing.T, db *sql.DB, account, key string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var status, failure sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT final_status, final_failure_reason FROM public.request_intake WHERE account_id = $1 AND idempotency_key = $2", account, key).Scan(&status, &failure); err != nil {
		t.Fatalf("reading the replay record (%s, %s): %v", account, key, err)
	}
	return status.String, failure.String
}

// TestIntegrationReaperReclaimsTheStrandedHoldWhole is the headline: a hold
// whose two clocks have both run out is closed, its capacity returns to the
// bucket the drawdown took it from, its request is failed, its replay record
// is pointed at that failure, and an expired fact lands on the feed — all in
// one unit of work, so a reader of the feed that sees the fact can rely on
// everything else having committed with it.
func TestIntegrationReaperReclaimsTheStrandedHoldWhole(t *testing.T) {
	f := reaperFixtureOnThrowaway(t, "dataplane_b13_reaper_whole")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const capacity = int64(500)
	account := integrationRuntimeAccount(t, "b13-reaper-whole")
	requestID, hold := seedStrandedHold(t, ctx, f, account, capacity, 1)

	if available, _, _, _ := integrationProjectionRow(t, f.db, account+"-bucket"); available != capacity-1 {
		t.Fatalf("available before the reap = %d, want %d — the seed's own drawdown is the only thing holding capacity", available, capacity-1)
	}

	outcome, err := reapOne(ctx, f.repos)
	if err != nil {
		t.Fatalf("reaping the stranded hold: %v", err)
	}
	if outcome.Scanned != 1 || outcome.Recovered != 1 || outcome.Skipped != 0 {
		t.Fatalf("outcome = %+v, want exactly one victim scanned and recovered", outcome)
	}

	if got := integrationReservationState(t, f.db, hold); got != "expired" {
		t.Fatalf("hold state = %q, want %q", got, "expired")
	}
	if got, _, _, _ := integrationProjectionRow(t, f.db, account+"-bucket"); got != capacity {
		t.Fatalf("available after the reap = %d, want %d — a reclaimed hold drew nothing, so it returns everything", got, capacity)
	}
	if got := integrationRequestStatus(t, f.db, requestID); got != string(execution.StatusFailed) {
		t.Fatalf("request status = %q, want %q", got, execution.StatusFailed)
	}
	status, failure := integrationIntakeStatus(t, f.db, account, account+"-key")
	if status != string(execution.FinalFailed) || failure != string(execution.FailedGatewayAbandoned) {
		t.Fatalf("replay record = (%q, %q), want (%q, %q) — a replay under this key must be answered with the ending that happened", status, failure, execution.FinalFailed, execution.FailedGatewayAbandoned)
	}
	if kinds := integrationFactKinds(t, f.db, requestID); len(kinds) != 1 || kinds[0] != "expired" {
		t.Fatalf("facts on the request = %v, want exactly one expired fact", kinds)
	}
}

// TestIntegrationReaperSecondCycleTakesNothing is the property the money half
// turns on. A return is not idempotent — a leg that lands twice credits twice
// — so the only thing standing between a second cycle and a double credit is
// the sweep's predicate refusing to select a hold that is no longer open. If
// the sweep ever selected it again, this is where the extra capacity shows up.
func TestIntegrationReaperSecondCycleTakesNothing(t *testing.T) {
	f := reaperFixtureOnThrowaway(t, "dataplane_b13_reaper_idempotent")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const capacity = int64(500)
	account := integrationRuntimeAccount(t, "b13-reaper-idempotent")
	requestID, _ := seedStrandedHold(t, ctx, f, account, capacity, 1)

	if first, err := reapOne(ctx, f.repos); err != nil || first.Recovered != 1 {
		t.Fatalf("first cycle = %+v, %v; want one victim recovered", first, err)
	}
	after, _, _, _ := integrationProjectionRow(t, f.db, account+"-bucket")

	// A drain runs until the sweep comes back dry, so the second cycle is one
	// more sweep rather than none — which is also the idle cost the cadence
	// comment in the configuration describes: one indexed statement.
	second, err := reapOne(ctx, f.repos)
	if err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if second.Scanned != 0 {
		t.Fatalf("second cycle = %+v, want nothing selected — a closed hold is not open, and the predicate says state = 'open'", second)
	}
	if got, _, _, _ := integrationProjectionRow(t, f.db, account+"-bucket"); got != after {
		t.Fatalf("available moved on the second cycle: %d, want %d — a return is not idempotent and must only ever run behind the sweep's close", got, after)
	}
	if got := integrationFactCount(t, f.db, requestID); got != 1 {
		t.Fatalf("facts after two cycles = %d, want the one the first published", got)
	}
}

// TestIntegrationReaperRequiresBothClocksToHaveLapsed is the dual-clock rule
// against a real clock_timestamp(), on the two horizons the sweep's WHERE
// clause names. A hold whose hold window has run out while its lease is still
// held belongs to a process that may still be executing it, and closing it
// would be a reaper ending a live request's reservation from under it.
func TestIntegrationReaperRequiresBothClocksToHaveLapsed(t *testing.T) {
	tests := []struct {
		name       string
		windowPast bool
		leasePast  bool
		wantReaped bool
	}{
		{name: "both horizons passed", windowPast: true, leasePast: true, wantReaped: true},
		{name: "the hold window has run out but the lease is held", windowPast: true, leasePast: false, wantReaped: false},
		{name: "the lease has run out but the hold window is open", windowPast: false, leasePast: true, wantReaped: false},
		{name: "neither horizon has passed", windowPast: false, leasePast: false, wantReaped: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := reaperFixtureOnThrowaway(t, "dataplane_b13_reaper_clocks")
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()

			const capacity = int64(500)
			account := integrationRuntimeAccount(t, "b13-reaper-clocks")
			requestID, hold := seedStrandedHold(t, ctx, f, account, capacity, 1)
			moveHoldClocks(t, f.db, hold, tc.windowPast, tc.leasePast)

			outcome, err := reapOne(ctx, f.repos)
			if err != nil {
				t.Fatalf("reaping: %v", err)
			}
			if reaped := outcome.Recovered == 1; reaped != tc.wantReaped {
				t.Fatalf("recovered %d (scanned %d skipped %d), want reaped %t", outcome.Recovered, outcome.Scanned, outcome.Skipped, tc.wantReaped)
			}
			// The capacity is the second witness, and the more important one:
			// a hold the reaper declined is a hold whose capacity is still
			// drawn, which is the difference the reaper exists to make.
			want := capacity - 1
			wantState := "open"
			wantFacts := 0
			if tc.wantReaped {
				want, wantState, wantFacts = capacity, "expired", 1
			}
			if got, _, _, _ := integrationProjectionRow(t, f.db, account+"-bucket"); got != want {
				t.Fatalf("available = %d, want %d", got, want)
			}
			if got := integrationReservationState(t, f.db, hold); got != wantState {
				t.Fatalf("hold state = %q, want %q", got, wantState)
			}
			if got := integrationFactCount(t, f.db, requestID); got != wantFacts {
				t.Fatalf("facts = %d, want %d", got, wantFacts)
			}
		})
	}
}

// TestIntegrationReaperTakesTheOldestLeaseFirst is the order the sweep's own
// ORDER BY states, observed at the store. A sweep that drained in some other
// order would starve the holds whose callers are already gone, and starvation
// is invisible in any single cycle — it shows only as the oldest hold still
// open after everything else is drained, which is what this asserts.
func TestIntegrationReaperTakesTheOldestLeaseFirst(t *testing.T) {
	f := reaperFixtureOnThrowaway(t, "dataplane_b13_reaper_order")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const capacity = int64(500)
	account := integrationRuntimeAccount(t, "b13-reaper-order")
	integrationSeedProjection(t, ctx, f.repos, account, true, time.Now().UTC().Add(24*time.Hour), capacity)

	// Three holds whose leases are an absolute minute apart, so the order is a
	// fact about the table rather than about an insertion sequence that
	// happens to agree with it.
	var (
		holds []identity.ReservationID
		first identity.ReservationID
	)
	for i := range 3 {
		member := integrationRuntimeAccount(t, fmt.Sprintf("b13-reaper-order-%d", i))
		admitted, err := integrationRunAdmissionUnit(ctx, f.admissionFixture(), account, member+"-key", "b13-digest-"+member, "", 1)
		if err != nil || !admitted.admitted {
			t.Fatalf("seeding hold %d: outcome %+v error %v", i, admitted, err)
		}
		// Hold 0 gets the EARLIEST lease, so the hold whose lease lapsed
		// first is the first one seeded. Each hold's lease is therefore moved
		// to an absolute instant, spaced a whole minute apart so the order is
		// a property of the table rather than of an insertion sequence that
		// happens to agree with it, and so no amount of slowness in the
		// seeding can reorder the holds: the instants are a minute apart and
		// the whole loop is bounded by the context above. Spacing by the
		// admission unit's own duration would not survive a slow one. The
		// instant is read from go's clock rather than the engine's, so a long
		// schema apply or a slow container cannot push it into the future and
		// make every hold eligible at once — which would leave nothing for
		// the sweep to order.
		leaseAt := time.Now().UTC().Add(-time.Duration(30-10*i) * time.Minute)
		if _, err := f.db.ExecContext(ctx,
			"UPDATE public.reservations SET created_at = $2::timestamptz - interval '10 seconds', expires_at = $2::timestamptz, lease_expires_at = $3::timestamptz WHERE id = $1",
			string(admitted.reservation), leaseAt.Add(-time.Hour), leaseAt); err != nil {
			t.Fatalf("aging hold %d: %v", i, err)
		}
		if i == 0 {
			first = admitted.reservation
		}
		holds = append(holds, admitted.reservation)
	}
	// Hold 0 was given the EARLIEST lease, so the hold whose lease lapsed
	// first is the first one seeded. Stated rather than computed: the
	// assertion below is that the sweep took that one first.
	earliestLease, err := integrationLeaseExpiry(t, f.db, first)
	if err != nil {
		t.Fatal(err)
	}
	latestLease, err := integrationLeaseExpiry(t, f.db, holds[len(holds)-1])
	if err != nil {
		t.Fatal(err)
	}
	if !earliestLease.Before(latestLease) {
		t.Fatalf("the first hold's lease (%s) is not earlier than the last one's (%s); the seeding did not order the table the way this test needs", earliestLease, latestLease)
	}

	// One victim per cycle, because the order is only visible across cycles.
	for i, hold := range holds {
		outcome, err := reapOne(ctx, f.repos)
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		if outcome.Scanned != 1 || outcome.Recovered != 1 {
			t.Fatalf("cycle %d = %+v, want one victim recovered", i, outcome)
		}
		if got := integrationReservationState(t, f.db, hold); got != "expired" {
			t.Fatalf("after %d cycles the hold whose lease lapsed at rank %d reads %q — the sweep's ORDER BY lease_expires_at is the reaper's order", i+1, i+1, got)
		}
	}
}

// integrationLeaseExpiry reads one hold's stored lease horizon, which is the
// column the sweep orders by.
func integrationLeaseExpiry(t *testing.T, db *sql.DB, id identity.ReservationID) (time.Time, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var lease time.Time
	if err := db.QueryRowContext(ctx, "SELECT lease_expires_at FROM public.reservations WHERE id = $1", string(id)).Scan(&lease); err != nil {
		return time.Time{}, fmt.Errorf("reading hold %s's lease horizon: %w", id, err)
	}
	return lease, nil
}

// TestIntegrationReaperSkipsALockedHold is what SKIP LOCKED means from the
// reaper's side, and it is why the sweep's answer can be empty while work
// exists. A live ending's unit holds the hold's row for the length of its own
// transaction; a sweep that arrived in that window leaves the row alone rather
// than blocking behind it. With a limit of one there is nothing behind it to
// fall through to, so the cycle ends having selected nothing — and must end
// quietly, because a reaper that treated a locked row as a failure would page
// an operator for correct behaviour.
func TestIntegrationReaperSkipsALockedHold(t *testing.T) {
	f := reaperFixtureOnThrowaway(t, "dataplane_b13_reaper_locked")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const capacity = int64(500)
	account := integrationRuntimeAccount(t, "b13-reaper-locked")
	_, hold := seedStrandedHold(t, ctx, f, account, capacity, 1)

	// The other writer: a unit of work that took the hold's row lock and is
	// still holding it, which is what a live ending's own unit looks like
	// from the sweep's side.
	lockCtx, releaseLock := context.WithCancel(ctx)
	holding := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- f.store.WithinTx(lockCtx, func(txCtx context.Context) error {
			if _, err := f.store.Querier(txCtx).ExecContext(txCtx, "SELECT id FROM public.reservations WHERE id = $1 FOR UPDATE", string(hold)); err != nil {
				return err
			}
			close(holding)
			<-lockCtx.Done()
			return lockCtx.Err()
		})
	}()
	<-holding

	outcome, sweepErr := reapOne(ctx, f.repos)
	releaseLock()
	if lockErr := <-finished; lockErr != nil && !errors.Is(lockErr, context.Canceled) {
		t.Fatalf("the other writer's unit: %v", lockErr)
	}
	if sweepErr != nil {
		t.Fatalf("reaping against a locked hold: %v", sweepErr)
	}
	if outcome.Scanned != 0 {
		t.Fatalf("outcome = %+v, want a cycle that found nothing reclaimable — SKIP LOCKED left the row alone", outcome)
	}
	if got := integrationReservationState(t, f.db, hold); got != "open" {
		t.Fatalf("hold state = %q, want %q — the sweep left the locked row alone", got, "open")
	}
	// And once the other writer is gone the hold is still there, which is the
	// property a skip must not cost.
	if after, err := reapOne(ctx, f.repos); err != nil || after.Recovered != 1 {
		t.Fatalf("the follow-up cycle = %+v, %v; want the hold the locked one left", after, err)
	}
}

// TestIntegrationReaperToleratesTheDuplicateFactTheOtherEndingAppended is the
// tolerated skip against a real unique index. The reaper's door and the
// routing stage's first door are separate units of work writing the same
// request's fact, and the fact table's UNIQUE is what turns that race into a
// sentinel the reaper can read. It must treat it as "the twin won" rather than
// as a failure: the hold is already closed inside this unit, so rolling back
// over a duplicate would un-return a capacity for no end.
func TestIntegrationReaperToleratesTheDuplicateFactTheOtherEndingAppended(t *testing.T) {
	f := reaperFixtureOnThrowaway(t, "dataplane_b13_reaper_duplicate")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const capacity = int64(500)
	account := integrationRuntimeAccount(t, "b13-reaper-dup")
	requestID, hold := seedStrandedHold(t, ctx, f, account, capacity, 1)

	// The twin's fact, committed before this cycle: the other door finished
	// first, which is the only way a duplicate is reachable.
	if err := f.store.WithinTx(ctx, func(txCtx context.Context) error {
		fact, err := accounting.NewExpired(requestID, nil, time.Now().UTC())
		if err != nil {
			return err
		}
		_, err = f.repos.facts.Append(txCtx, fact)
		return err
	}); err != nil {
		t.Fatalf("appending the twin's expired fact: %v", err)
	}

	outcome, err := reapOne(ctx, f.repos)
	if err != nil {
		t.Fatalf("reaping with the twin's fact already on the feed: %v", err)
	}
	if outcome.Scanned != 1 || outcome.Skipped != 1 || outcome.Recovered != 0 {
		t.Fatalf("outcome = %+v, want the victim scanned and counted as skipped", outcome)
	}
	if got := integrationReservationState(t, f.db, hold); got != "expired" {
		t.Fatalf("hold state = %q, want %q — a tolerated skip still closes the hold", got, "expired")
	}
	// The capacity came back exactly once, which is the whole point: the unit
	// that swallowed the duplicate still committed its return.
	if got, _, _, _ := integrationProjectionRow(t, f.db, account+"-bucket"); got != capacity {
		t.Fatalf("available = %d, want %d — the return committed behind the duplicate", got, capacity)
	}
	// And the feed carries one fact, not two: the unique held.
	if kinds := integrationFactKinds(t, f.db, requestID); len(kinds) != 1 || kinds[0] != "expired" {
		t.Fatalf("facts = %v, want exactly the twin's expired fact", kinds)
	}
}

// TestIntegrationReaperToleratesAFinaliseTheOtherEndingWon is the other
// tolerated skip, staged the only way it is reachable: an ending that already
// stated this request and left its hold open, which the sweep then finds. The
// two finalise calls lose their compare-and-set, the unit swallows the loss,
// and the capacity the return made still commits — a rollback here would
// un-return a leg for a request whose ending is already recorded.
func TestIntegrationReaperToleratesAFinaliseTheOtherEndingWon(t *testing.T) {
	f := reaperFixtureOnThrowaway(t, "dataplane_b13_reaper_finalise")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const capacity = int64(500)
	account := integrationRuntimeAccount(t, "b13-reaper-finalise")
	requestID, hold := seedStrandedHold(t, ctx, f, account, capacity, 1)

	// The other door, completed: the request rejected and the replay record
	// pointed, with the hold deliberately left open for the sweep.
	if err := f.store.WithinTx(ctx, func(txCtx context.Context) error {
		var now time.Time
		if err := f.store.Querier(txCtx).QueryRowContext(txCtx, "SELECT transaction_timestamp()").Scan(&now); err != nil {
			return err
		}
		request := execution.Request{ID: requestID, Status: execution.StatusExecuting}
		if err := request.Reject(execution.RejectedNoCandidate, now); err != nil {
			return err
		}
		if _, err := f.repos.requests.Finalise(txCtx, request); err != nil {
			return err
		}
		_, err := f.repos.intakes.Finalise(txCtx, account, account+"-key", execution.FinalRejected, execution.RejectedNoCandidate, "")
		return err
	}); err != nil {
		t.Fatalf("staging the ending that won the race: %v", err)
	}

	outcome, err := reapOne(ctx, f.repos)
	if err != nil {
		t.Fatalf("reaping against an already-stated request: %v", err)
	}
	if outcome.Scanned != 1 || outcome.Skipped != 1 || outcome.Recovered != 0 {
		t.Fatalf("outcome = %+v, want the victim scanned and counted as skipped", outcome)
	}
	if got := integrationReservationState(t, f.db, hold); got != "expired" {
		t.Fatalf("hold state = %q, want %q — a tolerated skip still closes the hold", got, "expired")
	}
	if got, _, _, _ := integrationProjectionRow(t, f.db, account+"-bucket"); got != capacity {
		t.Fatalf("available = %d, want %d — the return committed behind the tolerated skip", got, capacity)
	}
	// The request keeps the ending the other door wrote: the reaper restated
	// nothing, and the row still says rejected.
	if got := integrationRequestStatus(t, f.db, requestID); got != string(execution.StatusRejected) {
		t.Fatalf("request status = %q, want %q — the ending that won is the one the row keeps", got, execution.StatusRejected)
	}
}

// TestIntegrationReaperLeavesTheHoldOpenWhenItsFactCannotBeAppended is the
// unit-of-work answer to a failure the ladder cannot fix. An append the fact
// repository refuses for a reason that is not the duplicate sentinel aborts
// the whole unit, sweep included: the hold goes back to open and the capacity
// stays drawn, because a closed hold with no fact behind it is the orphan the
// whole discipline exists to make impossible. The next cycle is where it gets
// picked up.
func TestIntegrationReaperLeavesTheHoldOpenWhenItsFactCannotBeAppended(t *testing.T) {
	f := reaperFixtureOnThrowaway(t, "dataplane_b13_reaper_rollback")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const capacity = int64(500)
	account := integrationRuntimeAccount(t, "b13-reaper-rollback")
	requestID, hold := seedStrandedHold(t, ctx, f, account, capacity, 1)

	// A fact table that refuses: the envelope guard fires on a payload this
	// test smuggles in through a trigger, which is the only way to make the
	// append fail for a reason the reaper does not recognise. The trigger
	// lives for this test and is dropped by the throwaway database's own
	// teardown.
	if _, err := f.db.ExecContext(ctx, `CREATE FUNCTION b13_refuse_fact() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.request_id LIKE '%' || current_setting('b13.refuse') THEN
        RAISE EXCEPTION 'b13: this test refuses the fact';
    END IF;
    RETURN NEW;
END $$`); err != nil {
		t.Fatalf("installing the refusing trigger: %v", err)
	}
	if _, err := f.db.ExecContext(ctx, `CREATE TRIGGER b13_refuse_fact_guard BEFORE INSERT ON public.usage_events FOR EACH ROW EXECUTE FUNCTION b13_refuse_fact()`); err != nil {
		t.Fatalf("installing the refusing trigger: %v", err)
	}
	// The guard reads its own request id off the row, so the refusal is armed
	// for exactly the request under test and no other.
	armed := "SELECT set_config('b13.refuse', $1, false)"
	if _, err := f.db.ExecContext(ctx, armed, requestID[len(requestID)-8:]); err != nil {
		t.Fatalf("arming the refusal: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.db.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS b13_refuse_fact_guard ON public.usage_events`)
	})

	outcome, err := reapOne(ctx, f.repos)
	if err == nil {
		t.Fatalf("outcome = %+v, want the refused append to abort the unit", outcome)
	}
	if outcome.Recovered != 0 {
		t.Fatalf("outcome = %+v, want nothing recovered from an aborted unit", outcome)
	}
	if got := integrationReservationState(t, f.db, hold); got != "open" {
		t.Fatalf("hold state = %q, want %q — a rolled-back unit must not leave a closed hold with no fact behind it", got, "open")
	}
	if got := integrationFactCount(t, f.db, requestID); got != 0 {
		t.Fatalf("facts = %d, want none from an aborted unit", got)
	}
	if got, _, _, _ := integrationProjectionRow(t, f.db, account+"-bucket"); got != capacity-1 {
		t.Fatalf("available = %d, want %d — the return rolled back with the rest of the unit", got, capacity-1)
	}
	// And the request is still executing: the finalise rolled back too, which
	// is the other half of what "one unit" means.
	if got := integrationRequestStatus(t, f.db, requestID); got != string(execution.StatusExecuting) {
		t.Fatalf("request status = %q, want %q — nothing in the aborted unit survived", got, execution.StatusExecuting)
	}
}

// TestIntegrationConcurrentReaperCyclesClaimEachVictimOnce is the concurrency
// answer, and the reason the reaper is safe to run beside itself: several
// cycles in several goroutines over one pool, each victim closed by exactly
// one of them, every hold's capacity returned exactly once, and one
// settlement-relevant fact per request. Two reapers over one table is a
// configuration the reaper's own configuration comment calls correct but
// redundant, so this is the test that claim stands on.
//
// The capacity is the aggregate assertion: six holds of one unit drawn from a
// grant of six returns to six and not to twelve, and a sweep that closed one
// hold twice would credit twice.
func TestIntegrationConcurrentReaperCyclesClaimEachVictimOnce(t *testing.T) {
	f := reaperFixtureOnThrowaway(t, "dataplane_b13_reaper_concurrent")
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	const (
		workers  = 8
		holds    = 6
		holdSize = int64(1)
	)
	capacity := holdSize * holds
	account := integrationRuntimeAccount(t, "b13-reaper-concurrent")
	integrationSeedProjection(t, ctx, f.repos, account, true, time.Now().UTC().Add(24*time.Hour), capacity)

	var requests []identity.RequestID
	for i := 1; i <= holds; i++ {
		member := integrationRuntimeAccount(t, fmt.Sprintf("b13-reaper-conc-%d", i))
		admitted, err := integrationRunAdmissionUnit(ctx, f.admissionFixture(), account, member+"-key", "b13-digest-"+member, "", holdSize)
		if err != nil || !admitted.admitted {
			t.Fatalf("seeding hold %d: outcome %+v error %v", i, admitted, err)
		}
		integrationBackdateHold(t, f.db, admitted.reservation)
		requests = append(requests, admitted.requestID)
	}

	var (
		mu       sync.Mutex
		failures []string
		wg       sync.WaitGroup
	)
	start := make(chan struct{})
	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for round := range holds * 2 {
				outcome, err := reapOne(ctx, f.repos)
				if err != nil {
					mu.Lock()
					failures = append(failures, fmt.Sprintf("worker %d round %d: %v", worker, round, err))
					mu.Unlock()
					return
				}
				if outcome.Scanned == 0 {
					return
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("worker failures: %v", failures)
	}

	if got, _, _, _ := integrationProjectionRow(t, f.db, account+"-bucket"); got != capacity {
		t.Fatalf("available = %d, want %d — six holds' returns landed exactly once each", got, capacity)
	}
	for _, requestID := range requests {
		if kinds := integrationFactKinds(t, f.db, requestID); len(kinds) != 1 {
			t.Fatalf("facts on %s = %v, want exactly one settlement-relevant fact — a claim is a claim, not a count", requestID, kinds)
		}
	}
}

// TestIntegrationAToleratedDuplicateLeavesNoGapInTheFeed is the regression
// the one-statement append exists for, and it is the assertion no other test
// in this file makes: the twin test above proves the reaper SURVIVES a
// duplicate fact, and a reaper that survives one is just as happy whether the
// refusal left a hole in the feed behind it or not. The hole is the half that
// costs someone money.
//
// Its shape is the tolerated skip staged exactly as production reaches it:
// hold A is a real settlement; hold B is stranded, its clocks backdated, and
// its twin's expired fact committed by the other door before the sweep — so
// the reaper's append is refused, it swallows the sentinel, its unit commits,
// and B's hold still closes and its capacity still comes back. B's fact is
// therefore the LAST sequence the stream hands out while never becoming a row.
//
// A gap is not cosmetic. The feed is a serialised stream and a consumer's
// cursor is a position in it, so a number that no fact bears is
// indistinguishable from a fact that was written and then lost — the one
// thing the Control Plane's whole replay contract cannot reconstruct. It is
// why a reader of a fact stream checks for it first, and why merging the two
// statements was never a tidiness decision: on the separate-allocation shape
// this exact path advanced last_seq and committed the advance with no row
// behind it.
//
// The property, stated because it is a property and tests do not usually say
// their own arithmetic: the sequence the stream CONSUMED equals the number of
// rows INSERTED, so a gap is exactly an off-by-N — consumed above inserted by
// the count of refusals — and the cheapest witness of that equality is both
// ends at once, the least sequence ever handed out against the count of rows
// holding it.
func TestIntegrationAToleratedDuplicateLeavesNoGapInTheFeed(t *testing.T) {
	f := reaperFixtureOnThrowaway(t, "dataplane_b13_reaper_nogap")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// One request, two outcomes, one feed, in the order the numbers are
	// compared in: the settlement fills the first sequence, the refused append
	// is handed the second.
	const capacity = int64(500)
	integrationAppendSettled(t, ctx, f.repos, integrationRuntimeAccount(t, "b13-nogap-charged"), nil, time.Now().UTC())

	// The stranded hold, and the twin's fact committed before this cycle — the
	// only way a duplicate is reachable, and the same staging the twin test
	// above uses. Duplicated here on purpose rather than shared with it: that
	// test is about the OUTCOME (the hold closed, the capacity came back) and
	// this one is about the FEED, and a shared helper between them would make
	// each depend on the other's subject matter.
	account := integrationRuntimeAccount(t, "b13-nogap-duplicate")
	requestID, hold := seedStrandedHold(t, ctx, f, account, capacity, 1)
	if err := f.store.WithinTx(ctx, func(txCtx context.Context) error {
		fact, err := accounting.NewExpired(requestID, nil, time.Now().UTC())
		if err != nil {
			return err
		}
		_, err = f.repos.facts.Append(txCtx, fact)
		return err
	}); err != nil {
		t.Fatalf("appending the twin's expired fact: %v", err)
	}

	outcome, err := reapOne(ctx, f.repos)
	if err != nil {
		t.Fatalf("reaping against the twin's fact already on the feed: %v", err)
	}
	if outcome.Scanned != 1 || outcome.Skipped != 1 || outcome.Recovered != 0 {
		t.Fatalf("outcome = %+v, want the victim scanned and counted as skipped", outcome)
	}
	if got := integrationReservationState(t, f.db, hold); got != "expired" {
		t.Fatalf("hold state = %q, want %q — the tolerated skip still closes the hold", got, "expired")
	}

	// The feed's own accounting, from both ends. Nothing in this test read
	// either, so both are exactly what the appends produced.
	var first, last, rows int64
	if err := f.db.QueryRowContext(ctx, `
		SELECT coalesce(min(append_seq), 0), coalesce(max(append_seq), 0), count(*)
		FROM public.usage_events`).Scan(&first, &last, &rows); err != nil {
		t.Fatalf("reading the feed's two ends: %v", err)
	}
	if first == 0 {
		t.Fatal("the feed is empty — nothing was written, so there is no gap to be missing")
	}
	if last-first+1 != rows {
		t.Fatalf("the feed spans sequences %d..%d holding %d facts, so %d numbers were handed out and no fact bears them — a refused append consumed its sequence",
			first, last, rows, (last-first+1)-rows)
	}

	// And the stream stopped at its last fact, which is the only state a
	// consumer can resume from: a last_seq past the end is the cursor's gap
	// seen from the other side.
	if _, streamLast, ok := integrationStreamRow(t, f.db); !ok || streamLast != last {
		t.Fatalf("stream last_seq = %d (present %t), want %d — the stream stops where its last fact is",
			streamLast, ok, last)
	}

	// The gap this guards against is the one the pre-merge shape left, named
	// here so a future reader can see the test is not tautological: the
	// reaper's refused append is the last number the stream handed out and no
	// row bears it, which is exactly what the arithmetic above would have
	// caught had that advance committed.
	var expiredSeqs int
	if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM public.usage_events WHERE request_id = $1`, requestID).Scan(&expiredSeqs); err != nil {
		t.Fatalf("counting the twin's fact: %v", err)
	}
	if expiredSeqs != 1 {
		t.Fatalf("facts on the reaped request = %d, want the twin's single expired fact", expiredSeqs)
	}
	if last != first+1 {
		t.Fatalf("the feed's first two sequences are %d and %d, want 1 apart — this test's whole claim is that one settlement and one refused append fill two numbers between them",
			first, last)
	}
}
