package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// reservationInsert writes the hold. Its legs are a second statement, below —
// formed together by the domain, written inside whatever unit of work the
// caller is running, so a hold is never persisted without its split.
const reservationInsert = `INSERT INTO public.reservations
    (id, request_id, price_revision_id, input_unit_price, output_unit_price,
     input_tokens, max_output_tokens, reserved_amount,
     created_at, expires_at, lease_owner, lease_expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

// reservationClose is the once-only close, its `state = 'open'` the whole of
// the concurrency story: the settlement path, the release path and the reaper
// all arrive through this statement, the row lock serialises them, and the
// loser reads false. The terminal trigger behind it fires only on a row that
// is already closed — a bug detector, not the guard.
const reservationClose = `UPDATE public.reservations
SET state = $2, closed_at = $3
WHERE id = $1 AND state = 'open'`

// reservationExpireLapsed is the reaper's batch CAS. The select pins the
// victims — open, with the hold window AND the lease both lapsed against the
// store's own clock — and locks them SKIP LOCKED, so a settlement extending a
// lease on one of these rows this instant simply keeps it: the subquery skips
// the locked row, the outer update never sees it, and the two writers
// serialise on the row instead of deadlocking across it. Both clocks must
// have passed because they guard different disasters — taking on a lapsed
// lease alone would close out a request that is still executing, and taking
// on a lapsed window alone would hold capacity for a caller that already
// walked away. The rows that moved come back whole — the request, the pricing
// basis, the lease, the legs their hold was split into (the fact's
// allocation tail, read here so the append needs no second read), and the
// request's replay identity (the intake record's account and idempotency
// key, joined here so finalising what the hold left open needs no second
// lookup) — so the caller can append their facts and finalise their requests
// and replay records inside the same unit of work, and no closed hold is ever
// left without its fact or with a request row still executing (crash
// consistency: no completed close without its settlement tail).
//
// The replay identity's join is LATERAL and oldest-first because
// request_intake's enforced key is (account_id, idempotency_key), not
// request_id — the table's own comment records why — so the one record a
// request can have is found by request_id through
// request_intake_request_id_idx (migration 000008), and ORDER BY created_at
// with the replay pair as tiebreak makes the answer deterministic even for a
// table that does not enforce the pair unique. The identity returned is the
// record's OWN pair — the pair the caller's keyed finalisation will name —
// never the request row's account dressed in the record's key; requests
// joins LEFT only as the sentinel the scan fail-closes on: no foreign key
// stands behind reservations.request_id (cross-family ID references carry
// none), and a hold whose request row has gone missing must not vanish from
// the sweep's answer — a closed hold the caller never hears about is exactly
// the tear the sweep exists to prevent.
//
// THE WEDGE, STATED. Every fail-close below (a request row that does not
// exist, a replay record that does not exist or does not belong to the
// request's account, a request id identity cannot parse) errors the whole
// sweep, and the sweep's unit of work rolls back with it — nothing of the
// batch commits, which is the posture that never settles a close in halves.
// Because victims return oldest-lease-first, one such row is by construction
// the first victim every later sweep picks: it stops all reaping until it is
// repaired, and holds behind it leak their capacity while it stands. That is
// the price of the posture, chosen over silently skipping a hold forever; a
// loop consuming this port must treat the error as a page to an operator,
// not a reason to retry.
const reservationExpireLapsed = `WITH victims AS (
    SELECT id, lease_expires_at, request_id::text AS request_id,
           price_revision_id, input_unit_price, output_unit_price,
           input_tokens, max_output_tokens, lease_owner
    FROM public.reservations
    WHERE state = 'open'
      AND expires_at < clock_timestamp()
      AND lease_expires_at < clock_timestamp()
    ORDER BY lease_expires_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
), closed AS (
    UPDATE public.reservations r
    SET state = 'expired', closed_at = clock_timestamp()
    FROM victims v
    WHERE r.id = v.id
    RETURNING r.id::text AS id, v.request_id, v.price_revision_id,
              v.input_unit_price, v.output_unit_price, v.input_tokens,
              v.max_output_tokens, v.lease_owner, v.lease_expires_at
)
SELECT closed.id, closed.request_id, closed.price_revision_id,
       closed.input_unit_price, closed.output_unit_price,
       closed.input_tokens, closed.max_output_tokens, closed.lease_owner,
       requests.account_id AS request_account,
       replay.account_id AS record_account,
       replay.idempotency_key AS record_key,
       COALESCE((
           SELECT jsonb_agg(jsonb_build_object(
                      'funding_bucket_id', legs.funding_bucket_id,
                      'amount', legs.amount,
                      'ordinal', legs.ordinal)
                      ORDER BY legs.ordinal)
           FROM public.reservation_allocations legs
           WHERE legs.reservation_id = closed.id::uuid
       ), '[]'::jsonb) AS allocations
FROM closed
LEFT JOIN public.requests ON requests.id = closed.request_id::uuid
LEFT JOIN LATERAL (
    SELECT intake.account_id, intake.idempotency_key
    FROM public.request_intake intake
    WHERE intake.request_id = closed.request_id::uuid
    ORDER BY intake.created_at, intake.account_id, intake.idempotency_key
    LIMIT 1
) replay ON true
ORDER BY closed.lease_expires_at, closed.id`

// ReservationRepository is the PostgreSQL implementation of the reservation
// half of the persistence port.
type ReservationRepository struct {
	store persistence.Store
}

// NewReservationRepository builds the repository over a store.
func NewReservationRepository(store persistence.Store) *ReservationRepository {
	return &ReservationRepository{store: store}
}

// Insert implements persistence.ReservationRepository: the hold first, then
// its legs, both through the caller's unit of work.
func (repository *ReservationRepository) Insert(ctx context.Context, reservation accounting.Reservation) error {
	querier := repository.store.Querier(ctx)
	if _, err := querier.ExecContext(ctx, reservationInsert,
		string(reservation.ID),
		string(reservation.RequestID),
		reservation.PriceRevision,
		reservation.InputUnitPrice,
		reservation.OutputUnitPrice,
		reservation.InputTokens,
		reservation.MaxOutputTokens,
		reservation.ReservedAmount,
		reservation.CreatedAt,
		reservation.ExpiresAt,
		reservation.LeaseOwner,
		reservation.LeaseExpiresAt,
	); err != nil {
		if code(err) == "23505" && constraint(err) == "reservations_request_id_key" {
			return fmt.Errorf("postgres: insert reservation: %w", persistence.ErrDuplicateReservation)
		}
		return fmt.Errorf("postgres: insert reservation: %w", err)
	}
	if err := repository.insertAllocations(ctx, querier, reservation); err != nil {
		return err
	}
	return nil
}

// insertAllocations writes the waterfall split as one multi-row statement. The
// legs are small — one per bucket the waterfall drew from — so a single
// statement keeps the unit of work short without a batch API.
func (repository *ReservationRepository) insertAllocations(ctx context.Context, querier persistence.Querier, reservation accounting.Reservation) error {
	if len(reservation.Allocations) == 0 {
		return nil
	}
	var statement strings.Builder
	statement.WriteString(`INSERT INTO public.reservation_allocations (reservation_id, funding_bucket_id, amount, ordinal) VALUES `)
	args := make([]any, 0, len(reservation.Allocations)*4)
	for i, leg := range reservation.Allocations {
		if i > 0 {
			statement.WriteString(", ")
		}
		base := i * 4
		fmt.Fprintf(&statement, "($%d, $%d, $%d, $%d)", base+1, base+2, base+3, base+4)
		args = append(args, string(reservation.ID), leg.FundingBucketID, leg.Amount, leg.Ordinal)
	}
	if _, err := querier.ExecContext(ctx, statement.String(), args...); err != nil {
		return fmt.Errorf("postgres: insert reservation allocations: %w", err)
	}
	return nil
}

// Close implements persistence.ReservationRepository. A close to the open
// state is refused before the statement runs: "closing" a hold to open is not
// a transition, and a statement that would rewrite closed_at and no-op the
// state would only be hiding the caller's bug behind a true RowsAffected.
func (repository *ReservationRepository) Close(ctx context.Context, id identity.ReservationID, state accounting.State, closedAt time.Time) (bool, error) {
	if state == accounting.StateOpen {
		return false, errors.New("postgres: close reservation: the state to close to must be terminal")
	}
	result, err := repository.store.Querier(ctx).ExecContext(ctx, reservationClose,
		string(id), string(state), closedAt,
	)
	if err != nil {
		return false, fmt.Errorf("postgres: close reservation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: close reservation: %w", err)
	}
	return affected > 0, nil
}

// ExpireLapsedLeases implements persistence.ReservationRepository, returning
// the holds it closed so their facts can be appended in the same unit of work.
// The unit-of-work refusal is first, for the same reason Append's is: the
// closes this statement makes are the reaper's half of an ending whose fact,
// request finalisation and replay pointer are the other half, and a sweep run
// bare would commit each close the instant it happened — the tearing the
// same-unit discipline exists to make impossible. A limit below one is
// refused: an unbounded sweep is how a backlog becomes a long transaction,
// and "no limit today" is a decision a caller makes by passing the table's
// size, not by omitting the argument.
func (repository *ReservationRepository) ExpireLapsedLeases(ctx context.Context, limit int) ([]persistence.ExpiredLease, error) {
	if !repository.store.InUnitOfWork(ctx) {
		return nil, fmt.Errorf("postgres: expire lapsed leases: %w", persistence.ErrExpireOutsideUnitOfWork)
	}
	if limit < 1 {
		return nil, fmt.Errorf("postgres: expire lapsed leases: limit must be at least one, got %d", limit)
	}
	rows, err := repository.store.Querier(ctx).QueryContext(ctx, reservationExpireLapsed, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: expire lapsed leases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	expired := make([]persistence.ExpiredLease, 0)
	for rows.Next() {
		var (
			expiredRow     persistence.ExpiredLease
			requestID      string
			requestAccount sql.NullString
			recordAccount  sql.NullString
			recordKey      sql.NullString
			legsJSON       []byte
		)
		if err := rows.Scan(
			&expiredRow.ID,
			&requestID,
			&expiredRow.PriceRevision,
			&expiredRow.InputUnitPrice,
			&expiredRow.OutputUnitPrice,
			&expiredRow.InputTokens,
			&expiredRow.MaxOutputTokens,
			&expiredRow.LeaseOwner,
			&requestAccount,
			&recordAccount,
			&recordKey,
			&legsJSON,
		); err != nil {
			return nil, fmt.Errorf("postgres: expire lapsed leases: %w", err)
		}
		if expiredRow.RequestID, err = identity.ParseRequestID(requestID); err != nil {
			// Fail closed: a hold whose request is not an identity is not a
			// hold this build can close loudly and correctly.
			return nil, fmt.Errorf("postgres: expire lapsed leases: %w", err)
		}
		// Fail closed on the identity, for the same reason the sweep already
		// fail-closes on an unparseable request id: admission writes the
		// request row and its replay record inside one unit of work, so a
		// closed hold whose identity did not come back whole is a row the
		// write discipline says cannot exist. Erroring makes the corruption
		// loud and keeps the sweep's answer whole — the unit it ran in
		// aborts, so no victim of the batch is left closed without its
		// settlement tail. The wedge this buys into (one such row is the
		// oldest lapsed lease and stops every sweep behind it) is stated on
		// the statement above.
		if !requestAccount.Valid {
			return nil, fmt.Errorf("postgres: expire lapsed leases: hold %s for request %s names a request row that does not exist — admission writes the request and its replay record together, so the pair is torn and the sweep refuses it", expiredRow.ID, requestID)
		}
		if !recordAccount.Valid || !recordKey.Valid || recordKey.String == "" {
			return nil, fmt.Errorf("postgres: expire lapsed leases: hold %s for request %s has no replay record to finalise — admission writes the request and its replay record together, so the pair is torn and the sweep refuses it", expiredRow.ID, requestID)
		}
		if requestAccount.String != recordAccount.String {
			return nil, fmt.Errorf("postgres: expire lapsed leases: hold %s for request %s carries a replay record of account %q beside a request row of account %q — the pair is torn and the sweep refuses it", expiredRow.ID, requestID, recordAccount.String, requestAccount.String)
		}
		expiredRow.AccountID = recordAccount.String
		expiredRow.IdempotencyKey = recordKey.String
		// The legs came back in ordinal order; decode them through the domain's
		// own leg shape (the one the fact payload uses) so a schema drift here
		// is a decode error, not silently-empty allocations.
		var legs []accounting.AllocationLeg
		if len(legsJSON) > 0 {
			if err := json.Unmarshal(legsJSON, &legs); err != nil {
				return nil, fmt.Errorf("postgres: expire lapsed leases: decode the allocation legs of %s: %w", expiredRow.ID, err)
			}
		}
		expiredRow.Allocations = make([]accounting.Allocation, 0, len(legs))
		for _, leg := range legs {
			// Allocation and AllocationLeg are field-for-field the same shape
			// on purpose — the hold's memory and the fact's publication of it
			// — so the conversion is a spelling, not a mapping.
			expiredRow.Allocations = append(expiredRow.Allocations, accounting.Allocation(leg))
		}
		expired = append(expired, expiredRow)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: expire lapsed leases: %w", err)
	}
	return expired, nil
}

// reservationRenewLease extends one open hold's lease, naming its owner. The
// predicate carries the owner and the open state: a renewal cannot extend a
// lease someone else holds, cannot resurrect a closed hold, and cannot be the
// statement that mutates a terminal row — the terminal trigger behind it is
// the bug detector, this predicate is the guard. False means the hold is gone
// from the open set, and the renewing process must stop settling on its
// behalf.
const reservationRenewLease = `UPDATE public.reservations
SET lease_expires_at = $3
WHERE id = $1 AND lease_owner = $2 AND state = 'open'`

// RenewLease implements persistence.ReservationRepository.
func (repository *ReservationRepository) RenewLease(ctx context.Context, id identity.ReservationID, owner string, leaseExpiresAt time.Time) (bool, error) {
	if owner == "" {
		return false, errors.New("postgres: renew lease: a renewal names its owner")
	}
	result, err := repository.store.Querier(ctx).ExecContext(ctx, reservationRenewLease,
		string(id), owner, leaseExpiresAt,
	)
	if err != nil {
		return false, fmt.Errorf("postgres: renew lease: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: renew lease: %w", err)
	}
	return affected > 0, nil
}

// publicationApply is the publication algebra in one statement, atomic
// regardless of the unit of work around it. One INSERT with an upsert carries
// the whole algebra: no row exists and it inserts — seeded; a row exists and
// the conflict branch's guard (`revision < EXCLUDED.revision`, re-evaluated
// under the row lock against the committed row, so a concurrent publication
// cannot slip between the check and the write) moves only the
// Control-Plane-owned columns — never `available` — and the outcome is
// updated; the guard fails and nothing is written — stale. RETURNING
// (xmax = 0) tells the seed and the update apart on the row that comes back:
// xmax is 0 exactly for a tuple this statement inserted, never for one it
// updated in place.
const publicationApply = `INSERT INTO public.quota_projections
    (account_id, scope_kind, entitlement_id, cycle_number,
     funding_bucket_id, alias_group_version_id, named_scope, dimension,
     period_end, subscription_created_at, state, limit_amount, available,
     revision)
VALUES ($1, $2, $3, $4, $12, $5, $6, 'cost', $7, $8, $9, $10, $10, $11)
ON CONFLICT (funding_bucket_id) DO UPDATE
SET account_id = EXCLUDED.account_id,
    scope_kind = EXCLUDED.scope_kind,
    entitlement_id = EXCLUDED.entitlement_id,
    cycle_number = EXCLUDED.cycle_number,
    alias_group_version_id = EXCLUDED.alias_group_version_id,
    named_scope = EXCLUDED.named_scope,
    period_end = EXCLUDED.period_end,
    subscription_created_at = EXCLUDED.subscription_created_at,
    state = EXCLUDED.state,
    limit_amount = EXCLUDED.limit_amount,
    revision = EXCLUDED.revision,
    updated_at = clock_timestamp()
WHERE public.quota_projections.revision < EXCLUDED.revision
RETURNING (xmax = 0) AS seeded`

// The guard lives in the conflict branch, not the VALUES, so it is
// re-evaluated against the committed row at the moment the row lock is won:
// two publications arriving together serialise on the row, the loser's
// re-check reads the winner's revision, and the answer is stale by the same
// rule that decides a redelivery. `seeded_at`, `dimension` and `available`
// appear on no SET line on purpose — seed time is immutable, the dimension is
// the schema's, and spent capacity must never be resurrected by a
// republication.

// QuotaProjectionRepository is the PostgreSQL implementation of the quota
// half of the persistence port.
type QuotaProjectionRepository struct {
	store persistence.Store
}

// NewQuotaProjectionRepository builds the repository over a store.
func NewQuotaProjectionRepository(store persistence.Store) *QuotaProjectionRepository {
	return &QuotaProjectionRepository{store: store}
}

// ApplyPublication implements persistence.QuotaProjectionRepository.
func (repository *QuotaProjectionRepository) ApplyPublication(ctx context.Context, publication accounting.Publication) (accounting.PublicationOutcome, error) {
	if err := publication.Validate(); err != nil {
		return "", err
	}
	var seeded bool
	err := repository.store.Querier(ctx).QueryRowContext(ctx, publicationApply,
		publication.AccountID,
		string(publication.ScopeKind),
		textOrNil(publication.EntitlementID),
		intOrNil(publication.CycleNumber),
		publication.AliasGroupVersionID,
		publication.NamedScope,
		timeOrNil(publication.PeriodEnd),
		publication.SubscriptionCreatedAt,
		string(publication.State),
		publication.LimitAmount,
		publication.Revision,
		publication.FundingBucketID,
	).Scan(&seeded)
	if errors.Is(err, sql.ErrNoRows) {
		// No row came back: the conflict branch's guard refused the write, and
		// the publication is a redelivery or arrives out of order.
		return accounting.PublicationStale, nil
	}
	if err != nil {
		return "", fmt.Errorf("postgres: apply publication: %w", err)
	}
	if seeded {
		return accounting.PublicationSeeded, nil
	}
	return accounting.PublicationUpdated, nil
}

// refillApply is the refill algebra in one statement. The claim CTE records
// the identity first — INSERT ON CONFLICT DO NOTHING, whose unique is the
// engine's at-most-once mint guard — and only a claimed identity reaches the
// raise, which still demands the projection to be sitting at the guard
// revision. Three outcomes, one CASE: claimed and raised is applied; claimed
// and not raised is stale (the identity is spent unapplied, by design — the
// alternative is raising against a grant state the Control Plane has already
// superseded); not claimed is a redelivery, answered already_applied with the
// first sighting's receipt standing. Two sessions racing the same identity
// serialise on the claim insert (the second waits on the first's unique-index
// entry), so the mint really is at-most-once and not merely declared so.
const refillApply = `WITH claim AS (
    INSERT INTO public.quota_refills (funding_bucket_id, refill_id, amount, at_revision)
    VALUES ($2, $1, $3, $4)
    ON CONFLICT (refill_id) DO NOTHING
    RETURNING funding_bucket_id, amount, at_revision
), raised AS (
    UPDATE public.quota_projections p
    SET available = p.available + claim.amount, updated_at = clock_timestamp()
    FROM claim
    WHERE p.funding_bucket_id = claim.funding_bucket_id
      AND p.revision = claim.at_revision
    RETURNING 1
)
SELECT CASE
    WHEN EXISTS (SELECT 1 FROM claim) AND EXISTS (SELECT 1 FROM raised) THEN 'applied'
    WHEN EXISTS (SELECT 1 FROM claim) THEN 'stale'
    ELSE 'already_applied'
END`

// ApplyRefill implements persistence.QuotaProjectionRepository.
func (repository *QuotaProjectionRepository) ApplyRefill(ctx context.Context, refill accounting.Refill) (accounting.RefillOutcome, error) {
	// The domain constructor is the shape guard — identity, bucket, positive
	// amount, a guard revision — so a malformed refill is refused in the
	// domain's vocabulary before the statement exists.
	if _, err := accounting.NewRefill(refill.RefillID, refill.FundingBucketID, refill.Amount, refill.AtRevision); err != nil {
		return "", err
	}
	var outcome string
	err := repository.store.Querier(ctx).QueryRowContext(ctx, refillApply,
		refill.RefillID, refill.FundingBucketID, refill.Amount, refill.AtRevision,
	).Scan(&outcome)
	if err != nil {
		return "", fmt.Errorf("postgres: apply refill: %w", err)
	}
	return accounting.RefillOutcome(outcome), nil
}

// drawdownEligibility is the eligibility predicate a grant must satisfy to
// fund a request for one alias — the same conjuncts in projectionWalk's WHERE
// and in projectionTake's. Their identity is load-bearing: the walk picks the
// buckets it may draw from, and the take re-asserts, under the row lock,
// that the bucket is still eligible before a unit of it moves. Never edit one
// side without the other — a take that is looser than its walk can draw a
// grant the walk would have passed by, and one that is stricter fails takes
// the walk offered it for reasons the caller cannot see.
//
// The two conjuncts, in ADR 0003's words:
//
//   - the grant's cycle has not ended: an entitlement cycle is eligible only
//     while its period_end is still ahead of the statement's own instant.
//     transaction_timestamp() is transaction-stable, so the walk and every
//     take of one Drawdown test "has the cycle ended" against the same
//     instant and cannot disagree with each other, however long the unit
//     runs;
//   - the grant's stored scope contains the alias: the wildcard `*` version
//     contains every alias by definition (000002 keeps no member rows for it),
//     while a named version contains exactly the aliases its snapshot lists.
//     A dangling alias_group_version_id — one no group version answers for —
//     is simply ineligible, a treat-as-zero, never an error. The wildcard
//     subquery returns NULL while no `*` row exists, and `= NULL` matches
//     nothing: an absent wildcard is a normal state of an unseeded catalog,
//     not a failure. The scope column is text while the group version's id is
//     a uuid, so the comparison casts the uuid to text — a cast that can
//     never fail on a stored value, where the reverse cast would turn one
//     malformed scope value into a dead walk. The alias, by contrast, is
//     caller-supplied and cast to uuid, the type its column and its index
//     speak.
const drawdownEligibility = `AND (q.period_end IS NULL OR q.period_end > transaction_timestamp())
  AND (q.alias_group_version_id
           = (SELECT g.id::text FROM public.alias_group_versions g WHERE g.group_name = '*')
       OR EXISTS (SELECT 1
                  FROM public.alias_group_members m
                  WHERE m.group_version_id::text = q.alias_group_version_id
                    AND m.alias_id = $2::uuid))`

// projectionWalk reads one account's live projections in ADR 0003's waterfall
// order — entitlement cycles before PAYG, named scope before `*`, earliest
// period end, oldest subscription, entitlement id, funding bucket id last.
// The order here is the canonical lock order: every multi-row writer walks
// the same sequence, so two of them can interleave without ever waiting on
// each other in opposite orders.
//
// The leading `(q.scope_kind = 'payg_balance')` key is deliberate
// explicitness: `named_scope DESC` already happens to separate the two kinds
// today (a PAYG row is never a named scope, and the schema now pins that with
// quota_projections_payg_scope_wildcard), but the waterfall's entitlements-
// before-PAYG rule is about the kind of the funding, not the shape of its
// scope, and the order pins the rule it serves rather than borrowing one that
// merely implies it.
//
// $1 is the account, $2 the alias the request asks as — the eligibility
// conjuncts are drawdownEligibility, the same text projectionTake re-asserts
// (the alias sits at $2 in both statements so that text can be shared whole).
const projectionWalk = `SELECT q.funding_bucket_id, q.available
FROM public.quota_projections q
WHERE q.account_id = $1
  AND q.state = 'active'
  ` + drawdownEligibility + `
ORDER BY (q.scope_kind = 'payg_balance'), named_scope DESC, period_end ASC NULLS LAST,
         subscription_created_at ASC, entitlement_id ASC, funding_bucket_id ASC`

// projectionTake is the conditional drawdown of one bucket, and its predicate
// is where exactly-one-wins lives: two contenders run this against the same
// row, the row lock serialises them, and the second re-evaluates the whole
// predicate against the first's committed write — the losing predicate
// matches zero rows, and no available ever goes negative. The predicate also
// re-checks `state = 'active'` and the drawdown eligibility conjuncts: the
// walk read them, but a publication can deactivate the grant, close its cycle
// or re-scope it between the walk and the take, and capacity the Control
// Plane withdrew must not leave the row on a stale read's word. The
// eligibility conjuncts are drawdownEligibility — the walk's own text, since
// predicate identity between walk and take is load-bearing. No
// SELECT FOR UPDATE, no read-then-write window: the decision and the write
// are one statement.
//
// $1 is the bucket, $2 the alias (the same slot the walk binds it to), $3 the
// take.
const projectionTake = `UPDATE public.quota_projections q
SET available = q.available - $3, updated_at = clock_timestamp()
WHERE q.funding_bucket_id = $1 AND q.state = 'active' AND q.available >= $3
  ` + drawdownEligibility

// projectionGiveback returns capacity drawn before a walk failed. It is the
// unconditional return shape — a giveback must never fail for the same reason
// a return must not — so a short waterfall leaves the account exactly as it
// was.
const projectionGiveback = `UPDATE public.quota_projections
SET available = available + $2, updated_at = clock_timestamp()
WHERE funding_bucket_id = $1`

// Drawdown implements persistence.QuotaProjectionRepository: walk in waterfall
// order, take each bucket conditionally, give back everything taken if the
// order falls short. The walk runs inside the caller's unit of work — that is
// what makes the giveback atomic with the takes — and the legs it returns are
// what the caller builds the reservation's allocations from.
//
// A zero amount is not a short-circuit past the walk. Admission asks the
// waterfall even when the hold prices out at nothing, because the question
// the walk answers is "may this account fund this alias at all" — the
// scope question — and a zero-priced alias has to answer it like any other:
// no eligible bucket is the no-access refusal, and an eligible one takes
// nothing and funds the request for free.
//
// The read and the takes are separate statements by design: the takes are
// conditional, so a stale read costs a passed-by bucket, never a negative
// balance. A take that matches zero rows — a contender won the capacity, or
// the grant's eligibility moved under the row lock — is a stale number, not a
// failure: the walk passes the bucket by and continues down the waterfall,
// and the shortfall check at the end is the one place that decides the hold's
// fate. Contended admission retries the walk; the row locks serialise the
// contenders, and the waterfall order keeps that serialisation deadlock-free.
func (repository *QuotaProjectionRepository) Drawdown(ctx context.Context, accountID string, aliasID catalog.AliasID, amount int64) ([]accounting.Allocation, error) {
	if amount < 0 {
		return nil, accounting.ErrNegativeAmount
	}
	if aliasID == "" {
		return nil, errors.New("postgres: drawdown: a drawdown names the alias it serves")
	}
	querier := repository.store.Querier(ctx)
	rows, err := querier.QueryContext(ctx, projectionWalk, accountID, string(aliasID))
	if err != nil {
		return nil, fmt.Errorf("postgres: drawdown walk: %w", err)
	}
	type bucket struct {
		id        string
		available int64
	}
	var buckets []bucket
	for rows.Next() {
		var one bucket
		if err := rows.Scan(&one.id, &one.available); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("postgres: drawdown walk: %w", err)
		}
		buckets = append(buckets, one)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("postgres: drawdown walk: %w", err)
	}
	_ = rows.Close()

	// The zero hold, classified by the walk that just ran: the eligibility
	// question is answered off the same read every amount answers it from,
	// and nothing is taken. An eligible waterfall funds the request for
	// nothing — empty legs, no error — and the reservation built on them is
	// a hold of zero with no split, which the domain's own shape allows.
	if amount == 0 {
		if len(buckets) == 0 {
			return nil, &persistence.InsufficientCapacityError{EligibleRowSeen: false}
		}
		return []accounting.Allocation{}, nil
	}

	// The walk: take from each bucket in order until the hold is covered. The
	// legs record reality — what the store granted, which may be less than a
	// bucket's whole available. The ordinal is the leg's own counter, not the
	// bucket index: the schema's shape demands ordinals that count the legs
	// contiguously from 1, and a bucket with nothing to give (take <= 0) is
	// skipped without spending one.
	legs := make([]accounting.Allocation, 0, len(buckets))
	remaining := amount
	ordinal := 0
	for _, one := range buckets {
		if remaining == 0 {
			break
		}
		take := one.available
		if take > remaining {
			take = remaining
		}
		if take <= 0 {
			continue
		}
		result, err := querier.ExecContext(ctx, projectionTake, one.id, string(aliasID), take)
		if err != nil {
			return nil, repository.giveback(ctx, querier, legs, fmt.Errorf("postgres: drawdown take from %q: %w", one.id, err))
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, repository.giveback(ctx, querier, legs, fmt.Errorf("postgres: drawdown take from %q: %w", one.id, err))
		}
		if affected == 0 {
			// This bucket's number went stale between the walk and the take —
			// a contender took the capacity, or the grant's eligibility moved
			// under the row lock. The bucket is passed by, not fatal: the walk
			// continues down the waterfall, and either a later bucket covers
			// the hold or the shortfall check below gives everything back and
			// says ErrInsufficientCapacity — the same sentinel a walk with no
			// eligible bucket at all raises.
			continue
		}
		ordinal++
		legs = append(legs, accounting.Allocation{FundingBucketID: one.id, Amount: take, Ordinal: ordinal})
		remaining -= take
	}
	if remaining > 0 {
		// The shortfall carries its classification with it, read off the walk
		// this Drawdown already ran: the walk's statement returns exactly the
		// buckets eligible to fund this request, so "the walk saw at least
		// one" answers the caller's shortage-or-no-access question here, for
		// free, and never by asking the database a second time.
		return nil, repository.giveback(ctx, querier, legs, &persistence.InsufficientCapacityError{EligibleRowSeen: len(buckets) > 0})
	}
	return legs, nil
}

// giveback returns what a failed walk took, then reports the walk's error.
// The giveback's own failure is folded into the report — the caller must see
// both, and neither is more important than the other.
func (repository *QuotaProjectionRepository) giveback(ctx context.Context, querier persistence.Querier, legs []accounting.Allocation, walkErr error) error {
	for _, leg := range legs {
		if _, err := querier.ExecContext(ctx, projectionGiveback, leg.FundingBucketID, leg.Amount); err != nil {
			return fmt.Errorf("%w (and the giveback of %d minor units from %q failed: %v)", walkErr, leg.Amount, leg.FundingBucketID, err)
		}
	}
	return walkErr
}

// returnApply is the unconditional return. No predicate on available, on
// purpose: a publication that shrank the ceiling below outstanding capacity
// must not turn a capacity return into a failure — the return is the truth
// about what the runtime no longer holds.
const returnApply = `UPDATE public.quota_projections
SET available = available + $2, updated_at = clock_timestamp()
WHERE funding_bucket_id = $1`

// Return implements persistence.QuotaProjectionRepository: one statement per
// leg, in the order given — the waterfall order the reservation's legs carry,
// which is the canonical order every multi-row writer of this table uses.
func (repository *QuotaProjectionRepository) Return(ctx context.Context, legs []accounting.Allocation) (int, error) {
	querier := repository.store.Querier(ctx)
	returned := 0
	for _, leg := range legs {
		result, err := querier.ExecContext(ctx, returnApply, leg.FundingBucketID, leg.Amount)
		if err != nil {
			return returned, fmt.Errorf("postgres: return to %q: %w", leg.FundingBucketID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return returned, fmt.Errorf("postgres: return to %q: %w", leg.FundingBucketID, err)
		}
		returned += int(affected)
	}
	return returned, nil
}

// streamAppend allocates the next append sequence from the stream's single
// row. The upsert is the whole mechanism: the insert branch mints the epoch
// (the server's, never a migration's), every later append takes the conflict
// branch and increments under the row lock — which is held to the
// transaction's commit, so allocation order IS visibility order. This
// statement is the LAST write of the settlement unit of work; moving it
// earlier serialises more of the unit for nothing.
const streamAppend = `INSERT INTO public.usage_events_stream (singleton, epoch, last_seq)
VALUES (true, gen_random_uuid(), 1)
ON CONFLICT (singleton)
DO UPDATE SET last_seq = public.usage_events_stream.last_seq + 1,
              updated_at = clock_timestamp()
RETURNING epoch::text, last_seq`

// factInsert writes the fact itself, with the sequence the stream just
// allocated. The dedup partial uniques are the final idempotency guard: a
// second settlement-relevant fact for one request is refused here, on the
// engine's word, no matter what any caller believed.
const factInsert = `INSERT INTO public.usage_events
    (append_seq, request_id, kind, schema_version, capture_method,
     committed_attempt_id, provider_input_tokens, provider_output_tokens,
     delivery_tokens, price_revision_id, input_unit_price, output_unit_price,
     settled_amount, corrects_append_seq, payload, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`

// FactRepository is the PostgreSQL implementation of the fact half of the
// persistence port.
type FactRepository struct {
	store persistence.Store
}

// NewFactRepository builds the repository over a store.
func NewFactRepository(store persistence.Store) *FactRepository {
	return &FactRepository{store: store}
}

// Append implements persistence.FactRepository: refuse a contextless append,
// allocate the sequence, write the fact, return the sequence. The refusal is
// first because it decides whether the two statements after it may run
// together at all: a sequence allocated outside a unit of work could commit
// apart from the fact it numbers — a consumer's cursor would then hold a
// position for a fact that does not exist yet, and no error anywhere would
// say so.
func (repository *FactRepository) Append(ctx context.Context, fact accounting.Fact) (int64, error) {
	if !repository.store.InUnitOfWork(ctx) {
		return 0, fmt.Errorf("postgres: append fact: %w", persistence.ErrAppendOutsideUnitOfWork)
	}
	querier := repository.store.Querier(ctx)

	var epoch string
	var appendSeq int64
	if err := querier.QueryRowContext(ctx, streamAppend).Scan(&epoch, &appendSeq); err != nil {
		return 0, fmt.Errorf("postgres: allocate append sequence: %w", err)
	}

	var corrects any
	if fact.CorrectsAppendSeq != nil {
		corrects = *fact.CorrectsAppendSeq
	}
	if _, err := querier.ExecContext(ctx, factInsert,
		appendSeq,
		string(fact.RequestID),
		string(fact.Kind),
		fact.SchemaVersion,
		textOrNil(string(fact.CaptureMethod)),
		textOrNil(string(fact.CommittedAttemptID)),
		fact.ProviderInputTokens,
		fact.ProviderOutputTokens,
		fact.DeliveryTokens,
		textOrNil(fact.PriceRevision),
		fact.InputUnitPrice,
		fact.OutputUnitPrice,
		fact.SettledAmount,
		corrects,
		[]byte(fact.Payload),
		fact.OccurredAt,
	); err != nil {
		if code(err) == "23505" && (constraint(err) == "usage_events_settlement_key" || constraint(err) == "usage_events_orphan_key") {
			return 0, fmt.Errorf("postgres: append fact: %w", persistence.ErrDuplicateFact)
		}
		return 0, fmt.Errorf("postgres: append fact: %w", err)
	}
	return appendSeq, nil
}

// Compile-time proof that the repositories satisfy the ports they claim to.
var (
	_ persistence.ReservationRepository     = (*ReservationRepository)(nil)
	_ persistence.QuotaProjectionRepository = (*QuotaProjectionRepository)(nil)
	_ persistence.FactRepository            = (*FactRepository)(nil)
)
