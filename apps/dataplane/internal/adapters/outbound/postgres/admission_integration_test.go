//go:build integration

package postgres

// The admission suite's engine half: the B8 invariants that live in the
// database and are only provable against a real PostgreSQL — that the
// conditional drawdown cannot oversubscribe a grant under contention, that
// the replay record's unique key lets exactly one admission commit, that a
// failed unit of work leaves no residue behind, and that the release seam's
// CAS arbitrates concurrent endings to exactly one.
//
// The suite drives the real adapters through the ports, in the statement
// order the admission use case pins (alias and price reads aside, which are
// the use case's own reads and are driven against these same adapters by the
// composition-root suite in cmd/dataplane). The unit helper below is that
// order spelled out: one clock read, the waterfall, the request row, the hold
// and its legs, the replay record LAST. The use case itself — Serve, its
// probe, its classifications — is composed over these adapters in
// cmd/dataplane's admission_integration_test.go, because the arch rules keep
// internal/application out of this tree (an outbound adapter is constructed
// at the composition root and by nothing else); this file is the store half
// those tests stand on.
//
// It shares the landed runtime-storage harness (integration_test.go,
// runtime_storage_test.go): the same fixture database, the same
// unique-per-run accounts, the same no-truncation discipline, so it runs
// beside the other suites and against a database that already carries rows.

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

// The lease identity this suite's holds are opened under. Distinct from the
// landed suites' owner only so a row in the fixture database says which suite
// opened it.
const integrationAdmissionLeaseOwner = "b8c3-runtime"

// admissionFixture is what one scenario runs against: the pool, the store and
// the repositories over it, and the catalog scaffolding the waterfall's
// eligibility predicate needs. The alias is the harness's converged one; the
// grants are each scenario's own.
type admissionFixture struct {
	db    *sql.DB
	store persistence.Store
	repos integrationRepositories
	scope integrationScope
}

// integrationAdmissionFixture assembles the fixture: pool, schema, harness
// repositories, catalog scope.
func integrationAdmissionFixture(t *testing.T) admissionFixture {
	t.Helper()
	db, store := integrationPool(t)
	integrationRuntimeSchema(t, db)
	repos := integrationRepos(t, store)
	scope := repos.catalogScope(t)
	return admissionFixture{db: db, store: store, repos: repos, scope: scope}
}

// admissionUnitOutcome is what one admission-shaped unit decided, read back
// by the test after the unit's goroutine has joined.
type admissionUnitOutcome struct {
	admitted    bool
	reason      execution.RejectionReason
	requestID   identity.RequestID
	reservation identity.ReservationID
	hold        int64
	legs        []accounting.Allocation
}

// integrationRunAdmissionUnit runs one admission unit of work through the
// real adapters, in the order the use case pins: one clock read, the
// waterfall drawdown, the request row, the hold with the legs the store
// actually granted, the replay record LAST. A typed shortfall is a decision,
// not a malfunction: the unit writes the rejection pair (the rejected request
// row and the replay record born terminal) and commits, classified the way
// the use case classifies it — an eligible grant seen is a shortage, none is
// a scope answer. Every other error aborts the unit whole.
//
// The body parse and the price selection upstream of the waterfall are the
// use case's own reads and are deliberately not replicated here: what this
// unit proves is the engine's behaviour under the writes admission makes.
func integrationRunAdmissionUnit(ctx context.Context, fixture admissionFixture, account, idempotencyKey, digest string, requestID identity.RequestID, hold int64) (admissionUnitOutcome, error) {
	var out admissionUnitOutcome
	err := fixture.store.WithinTx(ctx, func(txCtx context.Context) error {
		var now time.Time
		if err := fixture.store.Querier(txCtx).QueryRowContext(txCtx, "SELECT transaction_timestamp()").Scan(&now); err != nil {
			return fmt.Errorf("postgres: admission unit: read the transaction clock: %w", err)
		}
		if requestID == "" {
			requestID = identity.NewRequestID()
		}
		price := integrationPrice()
		legs, err := fixture.repos.quota.Drawdown(txCtx, account, fixture.scope.alias, hold)
		var shortfall *persistence.InsufficientCapacityError
		if errors.As(err, &shortfall) {
			reason := execution.RejectedInsufficientEntitlement
			if !shortfall.EligibleRowSeen {
				reason = execution.RejectedNoAccess
			}
			if recordErr := integrationRecordRefusal(txCtx, fixture, account, idempotencyKey, digest, requestID, reason, now); recordErr != nil {
				return recordErr
			}
			out = admissionUnitOutcome{reason: reason}
			return nil
		}
		if err != nil {
			return err
		}
		request, err := execution.NewRequest(requestID, account, account+"-api-key", integrationScopeAliasName, 1, 1, price, now)
		if err != nil {
			return err
		}
		if err := fixture.repos.requests.Insert(txCtx, request); err != nil {
			return err
		}
		reservation, err := accounting.NewReservation(identity.NewReservationID(), requestID, price.RevisionID,
			price.InputUnitPrice, price.OutputUnitPrice,
			request.InputTokens, request.MaxOutputTokens,
			hold, legs, now, now.Add(time.Hour), integrationAdmissionLeaseOwner, now.Add(30*time.Minute))
		if err != nil {
			return err
		}
		if err := fixture.repos.reserves.Insert(txCtx, reservation); err != nil {
			return err
		}
		record, err := execution.NewIntake(account, idempotencyKey, digest, requestID, now)
		if err != nil {
			return err
		}
		if err := fixture.repos.intakes.Insert(txCtx, record); err != nil {
			return err
		}
		out = admissionUnitOutcome{
			admitted:    true,
			requestID:   requestID,
			reservation: reservation.ID,
			hold:        hold,
			legs:        legs,
		}
		return nil
	})
	return out, err
}

// integrationRecordRefusal writes the rejection pair inside the unit that
// decided it: the rejected request row and the replay record born terminal
// with the deciding reason. The use case's refuseInTx, statement for
// statement.
func integrationRecordRefusal(ctx context.Context, fixture admissionFixture, account, idempotencyKey, digest string, requestID identity.RequestID, reason execution.RejectionReason, now time.Time) error {
	rejected, err := execution.RejectNew(requestID, account, account+"-api-key", integrationScopeAliasName, reason, now)
	if err != nil {
		return err
	}
	if err := fixture.repos.requests.Insert(ctx, rejected); err != nil {
		return err
	}
	record, err := execution.NewIntake(account, idempotencyKey, digest, requestID, now)
	if err != nil {
		return err
	}
	if err := record.Finalise(execution.FinalRejected, reason, ""); err != nil {
		return err
	}
	return fixture.repos.intakes.Insert(ctx, record)
}

// integrationAdmissionResidue counts, for one account, every row the
// admission units could have left: request rows, holds joined through their
// requests, the legs' total, replay records, and the rejected rows among the
// requests. Zero-everything is what a rolled-back unit must leave behind.
func integrationAdmissionResidue(t *testing.T, db *sql.DB, account string) (requests, reservations int, legSum int64, intakes, refusals int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reads := []struct {
		query string
		dest  any
	}{
		{"SELECT count(*) FROM public.requests WHERE account_id = $1", &requests},
		{`SELECT count(*) FROM public.reservations r
		  JOIN public.requests rq ON rq.id = r.request_id
		  WHERE rq.account_id = $1`, &reservations},
		{`SELECT COALESCE(sum(l.amount), 0) FROM public.reservation_allocations l
		  JOIN public.reservations r ON r.id = l.reservation_id
		  JOIN public.requests rq ON rq.id = r.request_id
		  WHERE rq.account_id = $1`, &legSum},
		{"SELECT count(*) FROM public.request_intake WHERE account_id = $1", &intakes},
		{"SELECT count(*) FROM public.requests WHERE account_id = $1 AND status = 'rejected'", &refusals},
	}
	for _, read := range reads {
		if err := db.QueryRowContext(ctx, read.query, account).Scan(read.dest); err != nil {
			t.Fatalf("counting admission residue for %s: %v", account, err)
		}
	}
	return requests, reservations, legSum, intakes, refusals
}

// integrationOpenReservationsHeld counts one account's holds still open —
// the number a mid-flight invariant about coexisting capacity reads.
func integrationOpenReservationsHeld(t *testing.T, db *sql.DB, account string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var held int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM public.reservations r
	  JOIN public.requests rq ON rq.id = r.request_id
	  WHERE rq.account_id = $1 AND r.state = 'open'`, account).Scan(&held); err != nil {
		t.Fatalf("counting open holds for %s: %v", account, err)
	}
	return held
}

// integrationFactsOfRequest counts one request's settlement-relevant facts.
func integrationFactsOfRequest(t *testing.T, db *sql.DB, requestID identity.RequestID) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var facts int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM public.usage_events
	  WHERE request_id = $1 AND kind IN ('settled', 'released', 'expired')`, string(requestID)).Scan(&facts); err != nil {
		t.Fatalf("counting facts of request %s: %v", requestID, err)
	}
	return facts
}

// integrationFactKinds returns the kinds one request's facts carry.
func integrationFactKinds(t *testing.T, db *sql.DB, requestID identity.RequestID) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT kind FROM public.usage_events WHERE request_id = $1 ORDER BY append_seq`, string(requestID))
	if err != nil {
		t.Fatalf("reading the facts of request %s: %v", requestID, err)
	}
	defer func() { _ = rows.Close() }()
	var kinds []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scanning a fact kind of request %s: %v", requestID, err)
		}
		kinds = append(kinds, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the facts of request %s: %v", requestID, err)
	}
	return kinds
}

// integrationBackdateHold moves one open hold's whole clock vocabulary into
// the past, the way time would: created first, then the window, then the
// lease, each ordering the schema's CHECKs demand preserved.
func integrationBackdateHold(t *testing.T, db *sql.DB, id identity.ReservationID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `UPDATE public.reservations
	  SET created_at = transaction_timestamp() - interval '3 minutes',
	      expires_at = transaction_timestamp() - interval '2 minutes',
	      lease_expires_at = transaction_timestamp() - interval '1 minute'
	  WHERE id = $1 AND state = 'open'`, string(id)); err != nil {
		t.Fatalf("backdating hold %s: %v", id, err)
	}
}

// TestIntegrationConcurrentAdmissionUnitsCannotOversubscribeTheGrant is the
// headline invariant: N admission units race for a grant whose drawable
// capacity is N-1 units and every unit's hold is 1 unit. Exactly N-1 may
// commit; exactly one is refused as a shortage; the taken capacity sums to
// the capacity and never past it; the refusal is on the record as the pair —
// rejected request row and replay record born terminal. Three rounds in one
// process shake the scheduler; the verification lane also runs this test
// under -race.
func TestIntegrationConcurrentAdmissionUnitsCannotOversubscribeTheGrant(t *testing.T) {
	fixture := integrationAdmissionFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const (
		workers  = 48
		holdSize = int64(1)
	)
	capacity := int64(workers - 1)

	for round := 1; round <= 3; round++ {
		account := integrationRuntimeAccount(t, fmt.Sprintf("b8c3-oversub-%d", round))
		integrationSeedProjection(t, ctx, fixture.repos, account, true, time.Now().UTC().Add(24*time.Hour), capacity)
		before, _, _, _ := integrationProjectionRow(t, fixture.db, account+"-bucket")
		if before != capacity {
			t.Fatalf("round %d: seeded available = %d, want the capacity %d", round, before, capacity)
		}

		type attempt struct {
			outcome admissionUnitOutcome
			err     error
		}
		attempts := make([]attempt, workers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				key := fmt.Sprintf("%s-key-%d", account, i)
				outcome, err := integrationRunAdmissionUnit(ctx, fixture, account, key, "b8c3-digest", "", holdSize)
				attempts[i] = attempt{outcome: outcome, err: err}
			}(i)
		}
		close(start)
		wg.Wait()

		var (
			admittedCount int
			shortageCount int
			otherReasons  []execution.RejectionReason
			firstError    error
		)
		for i, one := range attempts {
			if one.err != nil {
				if firstError == nil {
					firstError = fmt.Errorf("worker %d: %w", i, one.err)
				}
				continue
			}
			switch {
			case one.outcome.admitted:
				admittedCount++
				if len(one.outcome.legs) == 0 || one.outcome.hold != holdSize {
					t.Errorf("round %d worker %d: admitted with hold %d and %d legs, want hold %d and at least one leg", round, i, one.outcome.hold, len(one.outcome.legs), holdSize)
				}
				var granted int64
				for _, leg := range one.outcome.legs {
					granted += leg.Amount
				}
				if granted != holdSize {
					t.Errorf("round %d worker %d: legs grant %d, want the hold %d — a hold its own split does not re-derive", round, i, granted, holdSize)
				}
			case one.outcome.reason == execution.RejectedInsufficientEntitlement:
				shortageCount++
			default:
				otherReasons = append(otherReasons, one.outcome.reason)
			}
		}
		if firstError != nil {
			t.Fatalf("round %d: a concurrent admission unit failed: %v", round, firstError)
		}
		if admittedCount != workers-1 {
			t.Errorf("round %d: admitted units = %d, want exactly the capacity %d", round, admittedCount, workers-1)
		}
		if shortageCount != 1 {
			t.Errorf("round %d: shortage refusals = %d, want exactly 1 — capacity for all but one", round, shortageCount)
		}
		if len(otherReasons) != 0 {
			t.Errorf("round %d: unexpected refusal reasons %v — a contention loss is a shortage or nothing", round, otherReasons)
		}

		available, limit, _, state := integrationProjectionRow(t, fixture.db, account+"-bucket")
		if available != 0 {
			t.Errorf("round %d: available after the race = %d, want 0 — the grant is drawn to exactly its capacity", round, available)
		}
		if limit != capacity || state != "active" {
			t.Errorf("round %d: grant row reads limit %d state %q, want %d and active — a drawdown moves available alone", round, limit, state, capacity)
		}

		requests, reservations, legSum, intakes, refusals := integrationAdmissionResidue(t, fixture.db, account)
		if requests != workers || refusals != 1 {
			t.Errorf("round %d: rows read %d requests (%d rejected), want %d requests and the one refusal", round, requests, refusals, workers)
		}
		if reservations != workers-1 {
			t.Errorf("round %d: holds on the record = %d, want %d — one per admitted unit, never more", round, reservations, workers-1)
		}
		if legSum != capacity {
			t.Errorf("round %d: taken capacity across all legs = %d, want exactly %d — the whole grant and not a unit more", round, legSum, capacity)
		}
		if intakes != workers {
			t.Errorf("round %d: replay records = %d, want %d — every decision, admitted or refused, is on the record", round, intakes, workers)
		}
		if held := integrationOpenReservationsHeld(t, fixture.db, account); held != workers-1 {
			t.Errorf("round %d: open holds = %d, want %d — nothing released them", round, held, workers-1)
		}
	}
}

// TestIntegrationConcurrentAdmissionUnitsRefuseNoAccessTogether pins the
// scope answer: an account with no grant eligible to fund the alias is
// refused no_access, every time, and the refusal pair is all that is written
// — no hold, no leg, no capacity touched, because there was none to touch.
func TestIntegrationConcurrentAdmissionUnitsRefuseNoAccessTogether(t *testing.T) {
	fixture := integrationAdmissionFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	const workers = 8
	account := integrationRuntimeAccount(t, "b8c3-no-access")
	// No projection seeded on purpose: the walk has nothing eligible to see.

	attempts := make([]admissionUnitOutcome, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			key := fmt.Sprintf("%s-key-%d", account, i)
			attempts[i], errs[i] = integrationRunAdmissionUnit(ctx, fixture, account, key, "b8c3-digest", "", 1)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if attempts[i].admitted {
			t.Fatalf("worker %d: admitted with no eligible grant in the walk", i)
		}
		if attempts[i].reason != execution.RejectedNoAccess {
			t.Errorf("worker %d: refused %q, want no_access — no eligible row seen is a scope answer, not a shortage", i, attempts[i].reason)
		}
	}

	requests, reservations, legSum, intakes, refusals := integrationAdmissionResidue(t, fixture.db, account)
	if requests != workers || refusals != workers || intakes != workers {
		t.Errorf("residue reads %d requests (%d rejected) and %d replay records, want the refusal pair for each of %d workers", requests, refusals, intakes, workers)
	}
	if reservations != 0 || legSum != 0 {
		t.Errorf("residue reads %d holds carrying %d units, want none — a scope answer draws nothing", reservations, legSum)
	}
}

// TestIntegrationConcurrentAdmissionsArbitrateOnTheReplayRecordsUniqueKey
// races M admission units sharing one (account, idempotency key): exactly one
// may commit, and the engine's unique key — not a check any unit ran — is
// what tells the others so. Every loser reads ErrDuplicateIntake out of its
// insert and aborts whole, and the residue is one request, one hold, one
// record, one drawn unit.
func TestIntegrationConcurrentAdmissionsArbitrateOnTheReplayRecordsUniqueKey(t *testing.T) {
	fixture := integrationAdmissionFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	const (
		workers  = 24
		capacity = int64(1000)
	)
	account := integrationRuntimeAccount(t, "b8c3-one-key")
	integrationSeedProjection(t, ctx, fixture.repos, account, true, time.Now().UTC().Add(24*time.Hour), capacity)

	const key = "b8c3-shared-idempotency-key"
	const digest = "b8c3-same-body-digest"

	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// Every worker mints its own request identity and its own hold —
			// the only thing they share is the (account, key, digest), which
			// is exactly what the record's unique key arbitrates.
			_, errs[i] = integrationRunAdmissionUnit(ctx, fixture, account, key, digest, "", 1)
		}(i)
	}
	close(start)
	wg.Wait()

	var committed, lost int
	for i, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, persistence.ErrDuplicateIntake):
			lost++
		default:
			t.Fatalf("worker %d: %v — a loser aborts on the record's duplicate, nothing else", i, err)
		}
	}
	if committed != 1 {
		t.Errorf("units committed = %d, want exactly 1 — the unique key admits one arrival per key", committed)
	}
	if lost != workers-1 {
		t.Errorf("units refused by the unique key = %d, want %d", lost, workers-1)
	}

	requests, reservations, legSum, intakes, _ := integrationAdmissionResidue(t, fixture.db, account)
	if requests != 1 || reservations != 1 || legSum != 1 || intakes != 1 {
		t.Errorf("residue reads %d requests, %d holds, %d taken unit, %d records — want one of each, the winner's", requests, reservations, legSum, intakes)
	}

	ctxRead, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRead()
	record, err := fixture.repos.intakes.Find(ctxRead, account, key)
	if err != nil {
		t.Fatalf("reading the winner's replay record: %v", err)
	}
	if record.RequestDigest != digest {
		t.Errorf("the record's digest = %q, want the shared body's %q", record.RequestDigest, digest)
	}
	if record.FinalStatus != nil {
		t.Errorf("the record's final pointer = %v, want nil — nothing finalised the winner here", *record.FinalStatus)
	}
	if available, _, _, _ := integrationProjectionRow(t, fixture.db, account+"-bucket"); available != capacity-1 {
		t.Errorf("available = %d, want %d — exactly one hold was drawn", available, capacity-1)
	}
}

// TestIntegrationFailedAdmissionUnitLeavesNoResidue is the atomic-rollback
// half: a unit that fails after the waterfall must leave the account byte-
// identical and the intake family empty. Two forced failures drive it — a
// hold insert that collides with a pre-seeded hold for the same request
// (the engine's UNIQUE(request_id) answering a bug, after the capacity was
// drawn), and a shortfall whose giveback must restore a two-grant waterfall
// exactly.
func TestIntegrationFailedAdmissionUnitLeavesNoResidue(t *testing.T) {
	fixture := integrationAdmissionFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	t.Run("hold insert failure after the drawdown", func(t *testing.T) {
		account := integrationRuntimeAccount(t, "b8c3-rollback-hold")
		integrationSeedProjection(t, ctx, fixture.repos, account, true, time.Now().UTC().Add(24*time.Hour), 100)
		before, beforeLimit, beforeRevision, beforeState := integrationProjectionRow(t, fixture.db, account+"-bucket")

		// The forced failure: a hold for this request already exists, while no
		// request row does — the reservations table's request_id is a cross-
		// family reference with no foreign key behind it, so a seatbelt-less
		// writer can create exactly this shape, and the admission unit's hold
		// insert is the statement that meets the engine's UNIQUE(request_id).
		duplicate := identity.NewRequestID()
		if _, err := fixture.db.ExecContext(ctx, `INSERT INTO public.reservations
		  (id, request_id, price_revision_id, input_unit_price, output_unit_price,
		   input_tokens, max_output_tokens, reserved_amount,
		   created_at, expires_at, lease_owner, lease_expires_at)
		  VALUES ($1, $2, 'b8c3-price-revision', 2, 3, 1, 1, 0,
		  transaction_timestamp(), transaction_timestamp() + interval '1 hour',
		  $3, transaction_timestamp() + interval '30 minutes')`,
			string(identity.NewReservationID()), string(duplicate), integrationAdmissionLeaseOwner); err != nil {
			t.Fatalf("seeding the colliding hold: %v", err)
		}

		_, err := integrationRunAdmissionUnit(ctx, fixture, account, account+"-key", "b8c3-digest", duplicate, 1)
		if !errors.Is(err, persistence.ErrDuplicateReservation) {
			t.Fatalf("the unit's error = %v, want it to wrap ErrDuplicateReservation — a duplicate hold is a bug, not an answer", err)
		}

		after, afterLimit, afterRevision, afterState := integrationProjectionRow(t, fixture.db, account+"-bucket")
		if after != before || afterLimit != beforeLimit || afterRevision != beforeRevision || afterState != beforeState {
			t.Errorf("grant row read (%d, %d, %d, %s) after the failure, want the pre-unit (%d, %d, %d, %s) — a failed unit leaves the balance byte-identical",
				after, afterLimit, afterRevision, afterState, before, beforeLimit, beforeRevision, beforeState)
		}
		requests, reservations, legSum, intakes, refusals := integrationAdmissionResidue(t, fixture.db, account)
		if requests != 0 || intakes != 0 || refusals != 0 || legSum != 0 {
			t.Errorf("residue reads %d requests, %d records, %d refusals, %d taken units — want all zero: nothing of the aborted unit committed", requests, intakes, refusals, legSum)
		}
		if reservations != 0 {
			t.Errorf("residue reads %d holds joined to this account's requests, want 0", reservations)
		}
	})

	t.Run("shortfall gives a two-grant waterfall back", func(t *testing.T) {
		account := integrationRuntimeAccount(t, "b8c3-rollback-giveback")
		now := time.Now().UTC()
		// Two eligible grants, the waterfall's order decided by their period
		// ends: the early one can fund 3 units, the late one 5 — together less
		// than the 10 the unit asks for, so the walk draws 3 and 5, falls
		// short, and must give every drawn unit back.
		if outcome := integrationPublish(t, ctx, fixture.repos, account, account+"-bucket-early", true, now.Add(24*time.Hour), 3, 1, accounting.ProjectionActive); outcome != accounting.PublicationSeeded {
			t.Fatalf("seeding the early grant = %q, want %q", outcome, accounting.PublicationSeeded)
		}
		if outcome := integrationPublish(t, ctx, fixture.repos, account, account+"-bucket-late", true, now.Add(48*time.Hour), 5, 1, accounting.ProjectionActive); outcome != accounting.PublicationSeeded {
			t.Fatalf("seeding the late grant = %q, want %q", outcome, accounting.PublicationSeeded)
		}
		beforeEarly, _, _, _ := integrationProjectionRow(t, fixture.db, account+"-bucket-early")
		beforeLate, _, _, _ := integrationProjectionRow(t, fixture.db, account+"-bucket-late")

		// The shortfall is a decision, not a malfunction: the unit commits the
		// refusal pair and reports the reason. The balances are the invariant —
		// the walk drew 3 from the early grant and 5 from the late one, fell
		// short of the 10 it wanted, and gave every drawn unit back before
		// saying so.
		outcome, err := integrationRunAdmissionUnit(ctx, fixture, account, account+"-key", "b8c3-digest", "", 10)
		if err != nil {
			t.Fatalf("the shortfall unit failed: %v", err)
		}
		if outcome.admitted {
			t.Fatal("the unit was admitted with more than the two grants' combined capacity")
		}
		if outcome.reason != execution.RejectedInsufficientEntitlement {
			t.Errorf("refused %q, want insufficient_entitlement — EligibleRowSeen is true because the walk saw two eligible grants that could not cover the hold: a shortage, not a scope answer", outcome.reason)
		}

		afterEarly, _, _, _ := integrationProjectionRow(t, fixture.db, account+"-bucket-early")
		afterLate, _, _, _ := integrationProjectionRow(t, fixture.db, account+"-bucket-late")
		if afterEarly != beforeEarly || afterLate != beforeLate {
			t.Errorf("balances read (%d, %d) after the giveback, want (%d, %d) — the walk gives back what it took before saying so", afterEarly, afterLate, beforeEarly, beforeLate)
		}
		requests, reservations, legSum, intakes, refusals := integrationAdmissionResidue(t, fixture.db, account)
		if requests != 1 || refusals != 1 || intakes != 1 {
			t.Errorf("residue reads %d requests (%d rejected) and %d records, want the refusal pair once", requests, refusals, intakes)
		}
		if reservations != 0 || legSum != 0 {
			t.Errorf("residue reads %d holds carrying %d units, want none — a shortfall leaves no hold behind", reservations, legSum)
		}
	})
}

// integrationSeamRelease is the release seam's unit, through the same ports
// the use case drives: the once-only Close decides who owns the hold's
// ending; the winner returns the legs, finalises the request and the record,
// and appends the released fact LAST. A loser writes nothing at all — that
// is the whole content of the CAS.
func integrationSeamRelease(ctx context.Context, fixture admissionFixture, account, idempotencyKey string, reservationID identity.ReservationID, requestID identity.RequestID, legs []accounting.Allocation) (bool, error) {
	var completed bool
	err := fixture.store.WithinTx(ctx, func(txCtx context.Context) error {
		var now time.Time
		if err := fixture.store.Querier(txCtx).QueryRowContext(txCtx, "SELECT transaction_timestamp()").Scan(&now); err != nil {
			return err
		}
		closed, err := fixture.repos.reserves.Close(txCtx, reservationID, accounting.StateReleased, now)
		if err != nil {
			return err
		}
		if !closed {
			return nil
		}
		if _, err := fixture.repos.quota.Return(txCtx, legs); err != nil {
			return err
		}
		request := execution.Request{ID: requestID, Status: execution.StatusExecuting}
		if err := request.Reject(execution.RejectedNoCandidate, now); err != nil {
			return err
		}
		finalised, err := fixture.repos.requests.Finalise(txCtx, request)
		if err != nil {
			return err
		}
		if !finalised {
			return nil
		}
		decided, err := fixture.repos.intakes.Finalise(txCtx, account, idempotencyKey, execution.FinalRejected, execution.RejectedNoCandidate, "")
		if err != nil {
			return err
		}
		if !decided {
			return nil
		}
		fact, err := accounting.NewReleased(requestID, integrationFactLegs(legs), now)
		if err != nil {
			return err
		}
		_, err = fixture.repos.facts.Append(txCtx, fact)
		if err != nil {
			return err
		}
		completed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return completed, nil
}

// TestIntegrationConcurrentSeamReleasesArbitrateOnTheCloseCas races the
// release seam's unit against itself on one admitted request: exactly one
// Close wins, the capacity comes back exactly once, exactly one released
// fact exists, and the request and its replay record finalise once. A unit
// that arrives after the hold closed writes nothing at all.
func TestIntegrationConcurrentSeamReleasesArbitrateOnTheCloseCas(t *testing.T) {
	fixture := integrationAdmissionFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	const (
		releasers = 6
		capacity  = int64(100)
	)
	account := integrationRuntimeAccount(t, "b8c3-seam-race")
	integrationSeedProjection(t, ctx, fixture.repos, account, true, time.Now().UTC().Add(24*time.Hour), capacity)

	admitted, err := integrationRunAdmissionUnit(ctx, fixture, account, account+"-key", "b8c3-digest", "", 1)
	if err != nil || !admitted.admitted {
		t.Fatalf("seeding the admitted request: outcome %+v error %v", admitted, err)
	}
	drawn, _, _, _ := integrationProjectionRow(t, fixture.db, account+"-bucket")
	if drawn != capacity-1 {
		t.Fatalf("available after admission = %d, want %d", drawn, capacity-1)
	}

	completions := make([]bool, releasers)
	errs := make([]error, releasers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < releasers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			completions[i], errs[i] = integrationSeamRelease(ctx, fixture, account, account+"-key", admitted.reservation, admitted.requestID, admitted.legs)
		}(i)
	}
	close(start)
	wg.Wait()

	var finished int
	for i, err := range errs {
		if err != nil {
			t.Fatalf("releaser %d: %v", i, err)
		}
		if completions[i] {
			finished++
		}
	}
	if finished != 1 {
		t.Errorf("seam units that completed = %d, want exactly 1 — the Close CAS admits one ending", finished)
	}

	if available, _, _, _ := integrationProjectionRow(t, fixture.db, account+"-bucket"); available != capacity {
		t.Errorf("available after the race = %d, want %d — restored exactly once, never twice", available, capacity)
	}
	if facts := integrationFactsOfRequest(t, fixture.db, admitted.requestID); facts != 1 {
		t.Errorf("facts on the request = %d, want exactly 1", facts)
	}
	if kinds := integrationFactKinds(t, fixture.db, admitted.requestID); len(kinds) != 1 || kinds[0] != "released" {
		t.Errorf("facts = %v, want exactly one released fact", kinds)
	}
	if status := integrationRequestStatus(t, fixture.db, admitted.requestID); status != "rejected" {
		t.Errorf("request status = %q, want rejected", status)
	}
	if state := integrationReservationState(t, fixture.db, admitted.reservation); state != "released" {
		t.Errorf("hold state = %q, want released", state)
	}
	// The record the winner finalised: terminal, no_candidate.
	record, err := fixture.repos.intakes.Find(ctx, account, account+"-key")
	if err != nil {
		t.Fatalf("reading the replay record: %v", err)
	}
	if record.FinalStatus == nil || *record.FinalStatus != execution.FinalRejected || record.FinalRejectionReason != execution.RejectedNoCandidate {
		t.Errorf("record reads (%v, %q), want rejected/no_candidate", record.FinalStatus, record.FinalRejectionReason)
	}

	// A seam arriving on the already-closed hold loses the CAS and writes
	// nothing: no second fact, no second return, no error.
	completed, err := integrationSeamRelease(ctx, fixture, account, account+"-key", admitted.reservation, admitted.requestID, admitted.legs)
	if err != nil {
		t.Fatalf("the late seam unit failed: %v", err)
	}
	if completed {
		t.Error("the late seam unit reported completion, want a silent CAS loss")
	}
	if available, _, _, _ := integrationProjectionRow(t, fixture.db, account+"-bucket"); available != capacity {
		t.Errorf("available after the late unit = %d, want %d — a CAS loss returns nothing", available, capacity)
	}
	if facts := integrationFactsOfRequest(t, fixture.db, admitted.requestID); facts != 1 {
		t.Errorf("facts after the late unit = %d, want 1 — the winner's fact stands alone", facts)
	}
}

// TestIntegrationSeamReleaseRacesTheReaperToTheEnding races the release seam
// against the reaper's batch CAS on one hold whose window and lease have both
// lapsed. Exactly one ending exists — released or expired, never both — the
// loser's predicate matches nothing, and exactly one settlement-relevant fact
// stands. The reaper's driver is not landed yet (its capacity business is the
// next milestone's), so the sweep here is the port's own shape: close the
// lapsed holds, append each victim's expired fact in the same unit.
func TestIntegrationSeamReleaseRacesTheReaperToTheEnding(t *testing.T) {
	fixture := integrationAdmissionFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	const capacity = int64(100)
	account := integrationRuntimeAccount(t, "b8c3-seam-reaper")
	integrationSeedProjection(t, ctx, fixture.repos, account, true, time.Now().UTC().Add(24*time.Hour), capacity)

	admitted, err := integrationRunAdmissionUnit(ctx, fixture, account, account+"-key", "b8c3-digest", "", 1)
	if err != nil || !admitted.admitted {
		t.Fatalf("seeding the admitted request: outcome %+v error %v", admitted, err)
	}
	integrationBackdateHold(t, fixture.db, admitted.reservation)

	sweep := func() (int, error) {
		victims := 0
		err := fixture.store.WithinTx(ctx, func(txCtx context.Context) error {
			expired, err := fixture.repos.reserves.ExpireLapsedLeases(txCtx, 10)
			if err != nil {
				return err
			}
			var now time.Time
			if err := fixture.store.Querier(txCtx).QueryRowContext(txCtx, "SELECT transaction_timestamp()").Scan(&now); err != nil {
				return err
			}
			for _, victim := range expired {
				fact, err := accounting.NewExpired(victim.RequestID, integrationFactLegs(victim.Allocations), now)
				if err != nil {
					return err
				}
				if _, err := fixture.repos.facts.Append(txCtx, fact); err != nil {
					return err
				}
				victims++
			}
			return nil
		})
		return victims, err
	}

	type ending struct {
		released bool
		swept    int
		err      error
	}
	results := make([]ending, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		released, err := integrationSeamRelease(ctx, fixture, account, account+"-key", admitted.reservation, admitted.requestID, admitted.legs)
		results[0] = ending{released: released, err: err}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		swept, err := sweep()
		results[1] = ending{swept: swept, err: err}
	}()
	close(start)
	wg.Wait()

	for i, one := range results {
		if one.err != nil {
			t.Fatalf("ending %d: %v", i, one.err)
		}
	}
	seamEnded := results[0].released
	reaperEnded := results[1].swept > 0
	if seamEnded == reaperEnded {
		t.Errorf("endings read (released %t, swept %d) — want exactly one ending to exist: both is a double close, neither is a lost hold", results[0].released, results[1].swept)
	}

	kinds := integrationFactKinds(t, fixture.db, admitted.requestID)
	if len(kinds) != 1 {
		t.Fatalf("facts on the request = %v, want exactly one settlement-relevant fact", kinds)
	}
	switch kinds[0] {
	case "released":
		if !results[0].released {
			t.Errorf("the fact says released but the seam unit did not claim the ending")
		}
		if available, _, _, _ := integrationProjectionRow(t, fixture.db, account+"-bucket"); available != capacity {
			t.Errorf("available = %d, want %d — a released hold returns its capacity", available, capacity)
		}
	case "expired":
		if results[0].released {
			t.Errorf("the fact says expired but the seam unit also claimed a release")
		}
	default:
		t.Errorf("fact kind = %q, want released or expired", kinds[0])
	}

	state := integrationReservationState(t, fixture.db, admitted.reservation)
	if state != "released" && state != "expired" {
		t.Errorf("hold state = %q, want the one terminal state the winning ending wrote", state)
	}
	if state == "released" && kinds[0] != "released" {
		t.Errorf("hold reads %s beside a %q fact — the rows and the feed must tell one story", state, kinds[0])
	}
	if state == "expired" && kinds[0] != "expired" {
		t.Errorf("hold reads %s beside a %q fact — the rows and the feed must tell one story", state, kinds[0])
	}
}
