package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
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
// victims — open, lease lapsed against the store's own clock — and locks them
// SKIP LOCKED, so a settlement extending a lease on one of these rows this
// instant simply keeps it: the subquery skips the locked row, the outer update
// never sees it, and the two writers serialise on the row instead of
// deadlocking across it. The rows that moved come back whole — the request,
// the pricing basis, the lease — so the caller can append their facts inside
// the same unit of work and no closed hold is ever left without its fact
// (crash consistency: no completed close without a fact).
const reservationExpireLapsed = `WITH victims AS (
    SELECT id, lease_expires_at, request_id::text AS request_id,
           price_revision_id, input_unit_price, output_unit_price,
           input_tokens, max_output_tokens, lease_owner
    FROM public.reservations
    WHERE state = 'open' AND lease_expires_at < clock_timestamp()
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
SELECT id, request_id, price_revision_id, input_unit_price, output_unit_price,
       input_tokens, max_output_tokens, lease_owner
FROM closed
ORDER BY lease_expires_at, id`

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

// Close implements persistence.ReservationRepository.
func (repository *ReservationRepository) Close(ctx context.Context, id identity.ReservationID, state accounting.State, closedAt time.Time) (bool, error) {
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
func (repository *ReservationRepository) ExpireLapsedLeases(ctx context.Context, limit int) ([]persistence.ExpiredLease, error) {
	rows, err := repository.store.Querier(ctx).QueryContext(ctx, reservationExpireLapsed, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: expire lapsed leases: %w", err)
	}
	defer rows.Close()

	expired := make([]persistence.ExpiredLease, 0)
	for rows.Next() {
		var (
			expiredRow persistence.ExpiredLease
			requestID  string
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
		); err != nil {
			return nil, fmt.Errorf("postgres: expire lapsed leases: %w", err)
		}
		if expiredRow.RequestID, err = identity.ParseRequestID(requestID); err != nil {
			// Fail closed: a hold whose request is not an identity is not a
			// hold this build can close loudly and correctly.
			return nil, fmt.Errorf("postgres: expire lapsed leases: %w", err)
		}
		expired = append(expired, expiredRow)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: expire lapsed leases: %w", err)
	}
	return expired, nil
}

// publicationApply is the publication algebra in one statement, atomic
// regardless of the unit of work around it. The data-modifying CTEs each run
// exactly once, in order: the update wins when a row exists at an older
// revision and touches only the Control-Plane-owned columns — never
// `available`; the insert seeds when the update matched nothing and a row does
// not already exist; the outer select reads off which of the three outcomes
// happened. A stale redelivery leaves both CTEs empty and is answered
// PublicationStale with nothing written.
const publicationApply = `WITH updated AS (
    UPDATE public.quota_projections
    SET account_id = $1, scope_kind = $2, entitlement_id = $3,
        cycle_number = $4, alias_group_version_id = $5, named_scope = $6,
        period_end = $7, subscription_created_at = $8, state = $9,
        limit_amount = $10, revision = $11, updated_at = clock_timestamp()
    WHERE funding_bucket_id = $12 AND revision < $11
    RETURNING 1
), seeded AS (
    INSERT INTO public.quota_projections
        (account_id, scope_kind, entitlement_id, cycle_number,
         funding_bucket_id, alias_group_version_id, named_scope, dimension,
         period_end, subscription_created_at, state, limit_amount, available,
         revision)
    SELECT $1, $2, $3, $4, $12, $5, $6, 'cost', $7, $8, $9, $10, $10, $11
    WHERE NOT EXISTS (SELECT 1 FROM updated)
    ON CONFLICT (funding_bucket_id) DO NOTHING
    RETURNING 1
)
SELECT CASE
    WHEN EXISTS (SELECT 1 FROM updated) THEN 'updated'
    WHEN EXISTS (SELECT 1 FROM seeded)   THEN 'seeded'
    ELSE 'stale'
END`

// The seeded CTE's ON CONFLICT DO NOTHING is there for the race, not the
// common path: two publications for a brand-new bucket arriving together both
// find no row to update, and the loser of the insert race lands in the
// conflict branch — answering 'stale', which is correct, because the row it
// now conflicts with carries its revision or a newer one.

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
	var outcome string
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
	).Scan(&outcome)
	if err != nil {
		return "", fmt.Errorf("postgres: apply publication: %w", err)
	}
	return accounting.PublicationOutcome(outcome), nil
}

// refillApply adds capacity only to a projection still sitting at the guard
// revision. RowsAffected is the whole verdict: zero means the projection moved
// on — a newer publication, a refill already applied — and the operation must
// be re-derived, never retried blind.
const refillApply = `UPDATE public.quota_projections
SET available = available + $2, updated_at = clock_timestamp()
WHERE funding_bucket_id = $1 AND revision = $3`

// ApplyRefill implements persistence.QuotaProjectionRepository.
func (repository *QuotaProjectionRepository) ApplyRefill(ctx context.Context, refill accounting.Refill) (bool, error) {
	result, err := repository.store.Querier(ctx).ExecContext(ctx, refillApply,
		refill.FundingBucketID, refill.Amount, refill.AtRevision,
	)
	if err != nil {
		return false, fmt.Errorf("postgres: apply refill: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: apply refill: %w", err)
	}
	return affected > 0, nil
}

// projectionWalk reads one account's live projections in ADR 0003's waterfall
// order — named scope before `*`, earliest period end, oldest subscription,
// entitlement id last. The order here is the canonical lock order: every
// multi-row writer walks the same sequence, so two of them can interleave
// without ever waiting on each other in opposite orders.
const projectionWalk = `SELECT funding_bucket_id, available
FROM public.quota_projections
WHERE account_id = $1 AND state = 'active'
ORDER BY named_scope DESC, period_end ASC NULLS LAST,
         subscription_created_at ASC, entitlement_id ASC`

// projectionTake is the conditional drawdown of one bucket, and its predicate
// is where exactly-one-wins lives: two contenders run this against the same
// row, the row lock serialises them, and the second re-evaluates
// `available >= $2` against the first's committed write — the losing predicate
// matches zero rows, and no available ever goes negative. No SELECT FOR
// UPDATE, no read-then-write window: the decision and the write are one
// statement.
const projectionTake = `UPDATE public.quota_projections
SET available = available - $2, updated_at = clock_timestamp()
WHERE funding_bucket_id = $1 AND available >= $2`

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
// The read and the takes are separate statements by design: the takes are
// conditional, so a stale read costs a failed walk and a giveback, never a
// negative balance. Contended admission retries the walk; the row locks
// serialise the contenders, and the waterfall order keeps that serialisation
// deadlock-free.
func (repository *QuotaProjectionRepository) Drawdown(ctx context.Context, accountID string, amount int64) ([]accounting.Allocation, error) {
	if amount < 0 {
		return nil, accounting.ErrNegativeAmount
	}
	if amount == 0 {
		return []accounting.Allocation{}, nil
	}
	querier := repository.store.Querier(ctx)
	rows, err := querier.QueryContext(ctx, projectionWalk, accountID)
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

	// The walk: take from each bucket in order until the hold is covered. The
	// legs record reality — what the store granted, which may be less than a
	// bucket's whole available.
	legs := make([]accounting.Allocation, 0, len(buckets))
	remaining := amount
	for i, one := range buckets {
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
		result, err := querier.ExecContext(ctx, projectionTake, one.id, take)
		if err != nil {
			return nil, repository.giveback(ctx, querier, legs, fmt.Errorf("postgres: drawdown take from %q: %w", one.id, err))
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, repository.giveback(ctx, querier, legs, fmt.Errorf("postgres: drawdown take from %q: %w", one.id, err))
		}
		if affected == 0 {
			// Another writer took this bucket's capacity between the walk and
			// the take. The walk's numbers are stale: fail the whole hold and
			// let the caller retry against fresh numbers — a partial hold is
			// money reserved against a request it will not cover.
			return nil, repository.giveback(ctx, querier, legs, accounting.ErrInsufficientCapacity)
		}
		legs = append(legs, accounting.Allocation{FundingBucketID: one.id, Amount: take, Ordinal: i + 1})
		remaining -= take
	}
	if remaining > 0 {
		return nil, repository.giveback(ctx, querier, legs, accounting.ErrInsufficientCapacity)
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

// Append implements persistence.FactRepository: allocate the sequence, write
// the fact, return the sequence. Two statements, one unit of work, the
// allocation last.
func (repository *FactRepository) Append(ctx context.Context, fact accounting.Fact) (int64, error) {
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
