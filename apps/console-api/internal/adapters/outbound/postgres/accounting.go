package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The accounting repositories: translation between the persistence port's
// accounting vocabulary and the `control` database's funding-bucket tables.
// Every query resolves its handle through the store, exactly as the commerce
// repositories do — the caller's unit of work when the context carries one,
// the pool otherwise — which is what lets the commerce roll insert an
// entitlement and this file's grant leg land in one transaction or not at
// all.
//
// What this file deliberately does not do: touch a balance outside a leg.
// The one write shape here is Append's pair — the bucket's guarded echo
// (balance move, sequence allocation, version bump) and the leg's insert —
// bracketed in a savepoint so a collision the caller can converge on costs
// the caller nothing. The echo's WHERE clause is the whole concurrency
// story: the row lock serialises writers, READ COMMITTED re-evaluates the
// predicate against the winner's committed values, and a predicate that no
// longer holds fires zero rows, which classifies into the domain's
// insufficient and closed sentinels below. A SET expression in one UPDATE
// sees the row's old values, so the available column is restated everywhere
// as `settled + sΔ − held − hΔ` — the projection definition itself, applied
// to the old row — rather than an increment each kind would have to get
// right on its own.

const (
	// pgSerializationFailure and pgDeadlockDetected are the two SQLSTATEs a
	// unit of work can lose to without having been refused: the store pitted
	// two units against each other and picked a loser. Both translate to
	// the port's ErrConflict, the one retryable answer this file gives.
	pgSerializationFailure = "40001"
	pgDeadlockDetected     = "40P01"
)

// fundingBucketColumns is the bucket row, in scan order, shared by the plain
// reads and the echo's RETURNING so they cannot drift apart column by
// column.
const fundingBucketColumns = `id, entitlement_id, account_id, status, version, last_sequence,
       settled_amount, held_amount, available_amount, created_at, updated_at`

// ledgerEntryColumns is the leg row, in scan order, shared by every ledger
// read.
const ledgerEntryColumns = `id, funding_bucket_id, kind, amount, settled_delta, held_delta,
       settlement_id, reservation_id, command_key,
       price_revision_id, input_unit_price, output_unit_price,
       adjustment_reason, original_entry_id, operator_id,
       sequence, created_at`

// NewFundingBuckets returns the persistence port's FundingBuckets repository
// backed by store. It panics on a nil store for the same reason every
// constructor here does: the failure a nil dependency produces later is
// strictly worse than a loud one here.
func NewFundingBuckets(store persistence.Store) persistence.FundingBuckets {
	if store == nil {
		panic("postgres: NewFundingBuckets requires a non-nil persistence.Store")
	}
	return &bucketRepo{store: store}
}

// NewFundingLedger returns the persistence port's FundingLedger repository
// backed by store.
func NewFundingLedger(store persistence.Store) persistence.FundingLedger {
	if store == nil {
		panic("postgres: NewFundingLedger requires a non-nil persistence.Store")
	}
	return &ledgerRepo{store: store}
}

// NewSettlements returns the persistence port's Settlements repository
// backed by store.
func NewSettlements(store persistence.Store) persistence.Settlements {
	if store == nil {
		panic("postgres: NewSettlements requires a non-nil persistence.Store")
	}
	return &settlementRepo{store: store}
}

// NewFundingProjections returns the persistence port's FundingProjections
// repository backed by store.
func NewFundingProjections(store persistence.Store) persistence.FundingProjections {
	if store == nil {
		panic("postgres: NewFundingProjections requires a non-nil persistence.Store")
	}
	return &fundingProjectionRepo{store: store}
}

// Compile-time proof that the repositories satisfy the port's contracts.
var (
	_ persistence.FundingBuckets     = (*bucketRepo)(nil)
	_ persistence.FundingLedger      = (*ledgerRepo)(nil)
	_ persistence.Settlements        = (*settlementRepo)(nil)
	_ persistence.FundingProjections = (*fundingProjectionRepo)(nil)
)

// errAppendOutsideUnitOfWork is Append's refusal to run autocommitted. It is
// a package-private sentinel so the port's unit-of-work rule has one
// spelling here and the use cases above branch on the wrapped words.
var errAppendOutsideUnitOfWork = errors.New("refused: a ledger append is unit-of-work-shaped and ctx carries no unit of work")

// ---------------------------------------------------------------------------
// funding_buckets — the authoritative capacity projections.
// ---------------------------------------------------------------------------

type bucketRepo struct {
	store persistence.Store
}

const insertFundingBucket = `
INSERT INTO control.funding_buckets
    (id, entitlement_id, account_id, status, version, last_sequence,
     settled_amount, held_amount, available_amount, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

const selectFundingBucket = `
SELECT ` + fundingBucketColumns + `
FROM control.funding_buckets
WHERE id = $1`

const selectFundingBucketByEntitlement = `
SELECT ` + fundingBucketColumns + `
FROM control.funding_buckets
WHERE entitlement_id = $1`

const selectFundingBucketByAccount = `
SELECT ` + fundingBucketColumns + `
FROM control.funding_buckets
WHERE account_id = $1`

// The administrative close, compare-and-swapped. The version predicate is
// the caller's read; the held-zero predicate is the one a leg can invalidate
// between the read and this write, and the statement — not the read — is
// what makes the close true.
const closeFundingBucket = `
UPDATE control.funding_buckets
SET status = 'closed', updated_at = $3
WHERE id = $1 AND status = 'active' AND version = $2 AND held_amount = 0`

func (r *bucketRepo) Create(ctx context.Context, bucket accounting.Bucket) error {
	// Exactly one owner column is set; the other turns into NULL explicitly,
	// the same rule the subscription creator's NULL cycle fields follow.
	var entitlementID, accountID any
	if bucket.EntitlementID != "" {
		entitlementID = string(bucket.EntitlementID)
	}
	if bucket.AccountID != "" {
		accountID = string(bucket.AccountID)
	}
	if _, err := r.store.Querier(ctx).ExecContext(ctx, insertFundingBucket,
		string(bucket.ID), entitlementID, accountID, string(bucket.Status),
		bucket.Version, bucket.LastSequence,
		bucket.Settled.Int64(), bucket.Held.Int64(), bucket.Available.Int64(),
		bucket.CreatedAt, bucket.UpdatedAt); err != nil {
		// Deliberately unmapped: the uniqueness constraints this insert can
		// lose to are funding_buckets_entitlement_id_key and
		// funding_buckets_account_id_key, and only a caller that did not
		// read first can fire them — the use cases converge through
		// ByEntitlementID/ByAccountID before creating. Whatever fires here
		// surfaces as the infrastructure refusal it is.
		return conflictOf(fmt.Errorf("postgres: create funding bucket %s: %w", bucket.ID, err))
	}
	return nil
}

func (r *bucketRepo) ByID(ctx context.Context, id accounting.FundingBucketID) (accounting.Bucket, error) {
	b, err := scanFundingBucket(r.store.Querier(ctx).QueryRowContext(ctx, selectFundingBucket, string(id)))
	if err != nil {
		return accounting.Bucket{}, wrapBucketRead(err, id)
	}
	return b, nil
}

func (r *bucketRepo) ByEntitlementID(ctx context.Context, id accounting.EntitlementID) (accounting.Bucket, error) {
	b, err := scanFundingBucket(r.store.Querier(ctx).QueryRowContext(ctx, selectFundingBucketByEntitlement, string(id)))
	if err != nil {
		return accounting.Bucket{}, wrapBucketRead(err, accounting.FundingBucketID(id))
	}
	return b, nil
}

func (r *bucketRepo) ByAccountID(ctx context.Context, id accounting.AccountID) (accounting.Bucket, error) {
	b, err := scanFundingBucket(r.store.Querier(ctx).QueryRowContext(ctx, selectFundingBucketByAccount, string(id)))
	if err != nil {
		return accounting.Bucket{}, wrapBucketRead(err, accounting.FundingBucketID(id))
	}
	return b, nil
}

func (r *bucketRepo) Close(ctx context.Context, id accounting.FundingBucketID, fromVersion int64, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, closeFundingBucket,
		string(id), fromVersion, updatedAt)
	if err != nil {
		return false, conflictOf(fmt.Errorf("postgres: close funding bucket %s: %w", id, err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: close funding bucket %s: read rows affected: %w", id, err)
	}
	// False means the world moved: a leg landed (its echo bumped the
	// version), another closer won, or the bucket is gone. The caller
	// re-reads; the domain's held-zero refusal has already run above, and
	// this predicate repeats it for the leg that raced the read.
	return n == 1, nil
}

// scanFundingBucket scans one bucket row out of any single-row result. The
// plain reads and the echo's RETURNING share it so they cannot drift.
func scanFundingBucket(row *sql.Row) (accounting.Bucket, error) {
	var b accounting.Bucket
	var id, status string
	var entitlementID, accountID sql.NullString
	var settled, held, available int64
	if err := row.Scan(&id, &entitlementID, &accountID, &status, &b.Version, &b.LastSequence,
		&settled, &held, &available, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return accounting.Bucket{}, err
	}
	b.ID = accounting.FundingBucketID(id)
	b.EntitlementID = accounting.EntitlementID(entitlementID.String)
	b.AccountID = accounting.AccountID(accountID.String)
	b.Status = accounting.BucketStatus(status)
	b.Settled = accounting.Balance(settled)
	b.Held = accounting.Balance(held)
	b.Available = accounting.Balance(available)
	return b, nil
}

// wrapBucketRead turns a bucket lookup's failure into the port's vocabulary:
// a miss is ErrNotFound and only a miss; everything else is wrapped.
func wrapBucketRead(err error, id accounting.FundingBucketID) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("postgres: funding bucket %s: %w", id, persistence.ErrNotFound)
	}
	return fmt.Errorf("postgres: funding bucket %s: %w", id, err)
}

// ---------------------------------------------------------------------------
// ledger_entries — the legs, and the guarded echo that is their only door.
// ---------------------------------------------------------------------------

type ledgerRepo struct {
	store persistence.Store
}

// The six echoes: one guarded UPDATE per kind, each allocating the leg's
// sequence, bumping the version, stamping updated_at, and restating the
// available column as the projection definition applied to the row's old
// values. The WHERE clause after the active gate is the kind's balance
// predicate — the domain's ApplyTo guard, written where the row lock can
// enforce it. All RETURNING the post-move row, which is the verdict and the
// caller's answer at once.
//
//   - grant/topup move settled only; the balance guard is the active gate
//     itself, because adding settled money cannot break a projection that
//     held the row's lock — but each names its owner: a grant lands on a
//     cycle bucket (entitlement_id IS NOT NULL) and a topup on an account's
//     (entitlement_id IS NULL), the domain's ownership rule written where
//     the row lock can enforce it.
//   - hold needs available ≥ take.
//   - release needs held ≥ take (held_amount + held_delta ≥ 0).
//   - consume needs held ≥ take AND settled ≥ take — the guard that makes
//     ΣG ≥ ΣC structural (ADR 0004: no kind drives settled below zero).
//   - adjustment needs the RESULTING settled, held and available all
//     non-negative: corrections fix records, they do not overdraw or credit
//     accounts, and a delta that would push a balance negative is refused.

const echoGrant = `
UPDATE control.funding_buckets
SET settled_amount = settled_amount + $2,
    held_amount = held_amount + $3,
    available_amount = settled_amount + $2 - held_amount - $3,
    last_sequence = last_sequence + 1,
    version = version + 1,
    updated_at = $4
WHERE id = $1 AND status = 'active' AND entitlement_id IS NOT NULL
RETURNING ` + fundingBucketColumns

const echoTopup = `
UPDATE control.funding_buckets
SET settled_amount = settled_amount + $2,
    held_amount = held_amount + $3,
    available_amount = settled_amount + $2 - held_amount - $3,
    last_sequence = last_sequence + 1,
    version = version + 1,
    updated_at = $4
WHERE id = $1 AND status = 'active' AND entitlement_id IS NULL
RETURNING ` + fundingBucketColumns

const echoHold = `
UPDATE control.funding_buckets
SET held_amount = held_amount + $3,
    available_amount = settled_amount - held_amount - $3,
    last_sequence = last_sequence + 1,
    version = version + 1,
    updated_at = $4
WHERE id = $1 AND status = 'active' AND settled_amount - held_amount >= $2
RETURNING ` + fundingBucketColumns

// The release echo carries the provenance its schema guard pins: the named
// reservation has a hold leg on file for this bucket. The ledger appends,
// so the condition is monotone — once true it cannot be withdrawn — and the
// INSERT-time trigger is braces for writers that skip the echo.
const echoRelease = `
UPDATE control.funding_buckets
SET held_amount = held_amount + $2,
    available_amount = settled_amount - held_amount - $2,
    last_sequence = last_sequence + 1,
    version = version + 1,
    updated_at = $3
WHERE id = $1 AND status = 'active' AND held_amount + $2 >= 0
  AND EXISTS (
      SELECT 1 FROM control.ledger_entries prior
      WHERE prior.funding_bucket_id = funding_buckets.id
        AND prior.reservation_id = $4
        AND prior.kind = 'hold')
RETURNING ` + fundingBucketColumns

const echoConsume = `
UPDATE control.funding_buckets
SET settled_amount = settled_amount + $2,
    held_amount = held_amount + $3,
    available_amount = settled_amount + $2 - held_amount - $3,
    last_sequence = last_sequence + 1,
    version = version + 1,
    updated_at = $4
WHERE id = $1 AND status = 'active'
  AND held_amount + $3 >= 0 AND settled_amount + $2 >= 0
RETURNING ` + fundingBucketColumns

// The adjustment echo carries the same provenance the schema guard pins:
// the entry this correction cites lives on the bucket the correction lands
// on — a foreign leg is not this bucket's history to correct.
const echoAdjustment = `
UPDATE control.funding_buckets
SET settled_amount = settled_amount + $2,
    held_amount = held_amount + $3,
    available_amount = settled_amount + $2 - held_amount - $3,
    last_sequence = last_sequence + 1,
    version = version + 1,
    updated_at = $4
WHERE id = $1 AND status = 'active'
  AND settled_amount + $2 >= 0
  AND held_amount + $3 >= 0
  AND settled_amount + $2 - held_amount - $3 >= 0
  AND EXISTS (
      SELECT 1 FROM control.ledger_entries original
      WHERE original.id = $5
        AND original.funding_bucket_id = funding_buckets.id)
RETURNING ` + fundingBucketColumns

// echoFor returns the kind's echo statement. An unknown kind is a minted-leg
// invariant the constructors already enforce; the default is a refusal so a
// new Kind cannot silently run the wrong statement.
func echoFor(kind accounting.Kind) (string, error) {
	switch kind {
	case accounting.KindGrant:
		return echoGrant, nil
	case accounting.KindTopup:
		return echoTopup, nil
	case accounting.KindHold:
		return echoHold, nil
	case accounting.KindRelease:
		return echoRelease, nil
	case accounting.KindConsume:
		return echoConsume, nil
	case accounting.KindAdjustment:
		return echoAdjustment, nil
	default:
		return "", fmt.Errorf("postgres: append ledger entry: %w: unknown kind %q", accounting.ErrInvalidTransition, kind)
	}
}

// echoArgs binds the echo's parameters for the kind. The layouts match the
// statements above parameter by parameter — grant/topup and adjustment move
// both balances, hold and release move held only, consume moves both by the
// same take.
func echoArgs(entry accounting.LedgerEntry) ([]any, error) {
	switch entry.Kind {
	case accounting.KindGrant, accounting.KindTopup:
		return []any{string(entry.FundingBucketID),
			entry.SettledDelta.Int64(), entry.HeldDelta.Int64(), entry.CreatedAt}, nil
	case accounting.KindHold:
		return []any{string(entry.FundingBucketID),
			entry.Amount.Int64(), entry.HeldDelta.Int64(), entry.CreatedAt}, nil
	case accounting.KindRelease:
		// $4 is the reservation the provenance guard matches the hold on.
		return []any{string(entry.FundingBucketID),
			entry.HeldDelta.Int64(), entry.CreatedAt, string(entry.ReservationID)}, nil
	case accounting.KindConsume:
		// The amount column rides the deltas for this kind — both move by the
		// same take — so the echo binds no fourth value; an unreferenced
		// parameter is a statement error, not a null.
		return []any{string(entry.FundingBucketID),
			entry.SettledDelta.Int64(), entry.HeldDelta.Int64(), entry.CreatedAt}, nil
	case accounting.KindAdjustment:
		// $5 is the entry the provenance guard pins to this bucket.
		return []any{string(entry.FundingBucketID),
			entry.SettledDelta.Int64(), entry.HeldDelta.Int64(), entry.CreatedAt,
			string(entry.OriginalEntryID)}, nil
	default:
		return nil, fmt.Errorf("postgres: append ledger entry: %w: unknown kind %q", accounting.ErrInvalidTransition, entry.Kind)
	}
}

const insertLedgerEntry = `
INSERT INTO control.ledger_entries
    (id, funding_bucket_id, kind, amount, settled_delta, held_delta,
     settlement_id, reservation_id, command_key,
     price_revision_id, input_unit_price, output_unit_price,
     adjustment_reason, original_entry_id, operator_id,
     sequence, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`

const selectLedgerByCommandKey = `
SELECT ` + ledgerEntryColumns + `
FROM control.ledger_entries
WHERE funding_bucket_id = $1 AND command_key = $2`

const selectLedgerByReservationKind = `
SELECT ` + ledgerEntryColumns + `
FROM control.ledger_entries
WHERE funding_bucket_id = $1 AND reservation_id = $2 AND kind = $3`

// The provenance conditions the release and adjustment echoes carry in their
// WHERE clauses, run fresh when an echo fires zero rows so the
// classification can name which verdict fired. The ledger appends, so both
// answers are monotone: a hold on file is never withdrawn, and the entry a
// correction cites is never rewritten.
const selectHoldOnFile = `SELECT EXISTS (
    SELECT 1 FROM control.ledger_entries
    WHERE funding_bucket_id = $1 AND reservation_id = $2 AND kind = 'hold')`

const selectAdjustmentOnOwnBucket = `SELECT EXISTS (
    SELECT 1 FROM control.ledger_entries
    WHERE id = $1 AND funding_bucket_id = $2)`

func (r *ledgerRepo) holdOnFile(ctx context.Context, bucketID accounting.FundingBucketID, reservationID accounting.ReservationID) (bool, error) {
	var onFile bool
	if err := r.store.Querier(ctx).QueryRowContext(ctx, selectHoldOnFile,
		string(bucketID), string(reservationID)).Scan(&onFile); err != nil {
		return false, err
	}
	return onFile, nil
}

func (r *ledgerRepo) correctsOwnBucket(ctx context.Context, entryID accounting.LedgerEntryID, bucketID accounting.FundingBucketID) (bool, error) {
	var own bool
	if err := r.store.Querier(ctx).QueryRowContext(ctx, selectAdjustmentOnOwnBucket,
		string(entryID), string(bucketID)).Scan(&own); err != nil {
		return false, err
	}
	return own, nil
}

// Append lands one leg. The order inside the savepoint is the echo first,
// the insert second: the echo both allocates the sequence the insert needs
// and refuses the move the guard cannot afford, and the insert is the only
// statement of the two that can lose to a uniqueness constraint. A guard
// that fires zero rows or an insert that loses its constraint rolls back to
// the savepoint — the bucket row and the unit of work are left exactly as
// the caller found them — and classifies, so the caller can converge or
// refuse in the domain's words without its transaction dying under it.
func (r *ledgerRepo) Append(ctx context.Context, entry accounting.LedgerEntry) (accounting.LedgerEntry, accounting.Bucket, error) {
	if !r.store.InUnitOfWork(ctx) {
		return accounting.LedgerEntry{}, accounting.Bucket{}, fmt.Errorf("postgres: append %s entry to bucket %s: %w",
			entry.Kind, entry.FundingBucketID, errAppendOutsideUnitOfWork)
	}
	stmt, err := echoFor(entry.Kind)
	if err != nil {
		return accounting.LedgerEntry{}, accounting.Bucket{}, err
	}
	args, err := echoArgs(entry)
	if err != nil {
		return accounting.LedgerEntry{}, accounting.Bucket{}, err
	}
	q := r.store.Querier(ctx)

	// The savepoint name is fixed on purpose: Append can run several times
	// in one unit of work (a settlement's plan), PostgreSQL nests duplicate
	// names most-recent-wins, and every ROLLBACK TO / RELEASE below targets
	// this call's own bracket.
	if _, err := q.ExecContext(ctx, `SAVEPOINT ledger_append`); err != nil {
		return accounting.LedgerEntry{}, accounting.Bucket{}, fmt.Errorf("postgres: append %s entry to bucket %s: open savepoint: %w",
			entry.Kind, entry.FundingBucketID, err)
	}

	bucket, err := scanFundingBucket(q.QueryRowContext(ctx, stmt, args...))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		_, _ = q.ExecContext(ctx, `ROLLBACK TO SAVEPOINT ledger_append`)
		return accounting.LedgerEntry{}, accounting.Bucket{}, conflictOf(fmt.Errorf("postgres: append %s entry to bucket %s: bucket echo: %w",
			entry.Kind, entry.FundingBucketID, err))
	}
	if errors.Is(err, sql.ErrNoRows) {
		// The guard's verdict: the bucket is gone, closed, or cannot afford
		// the leg. The savepoint undoes nothing but the bracket — the UPDATE
		// fired zero rows — and the classification is a fresh read that names
		// which.
		_, _ = q.ExecContext(ctx, `ROLLBACK TO SAVEPOINT ledger_append`)
		return accounting.LedgerEntry{}, accounting.Bucket{}, r.classifyGuardMiss(ctx, entry)
	}

	stamped := entry
	stamped.Sequence = bucket.LastSequence
	if _, err := q.ExecContext(ctx, insertLedgerEntry, ledgerEntryArgs(stamped)...); err != nil {
		_, _ = q.ExecContext(ctx, `ROLLBACK TO SAVEPOINT ledger_append`)
		// The collision mappings. Same key or same movement at the same
		// amount is convergence and stays the CALLER's comparison — it
		// re-reads through the port and compares payloads; this layer only
		// says that a collision happened, with the unit of work still alive.
		if constraint, ok := constraintOfUniqueViolation(err); ok {
			switch constraint {
			case "ledger_entries_bucket_command_key":
				return accounting.LedgerEntry{}, accounting.Bucket{}, fmt.Errorf("postgres: append %s entry to bucket %s: %w",
					entry.Kind, entry.FundingBucketID, accounting.ErrDuplicateCommand)
			case "ledger_entries_reservation_bucket_kind", "ledger_entries_settlement_bucket_kind":
				return accounting.LedgerEntry{}, accounting.Bucket{}, fmt.Errorf("postgres: append %s entry to bucket %s: %w",
					entry.Kind, entry.FundingBucketID, accounting.ErrDuplicateMovement)
			}
		}
		return accounting.LedgerEntry{}, accounting.Bucket{}, conflictOf(fmt.Errorf("postgres: append %s entry to bucket %s: insert leg: %w",
			entry.Kind, entry.FundingBucketID, err))
	}

	if _, err := q.ExecContext(ctx, `RELEASE SAVEPOINT ledger_append`); err != nil {
		return accounting.LedgerEntry{}, accounting.Bucket{}, conflictOf(fmt.Errorf("postgres: append %s entry to bucket %s: release savepoint: %w",
			entry.Kind, entry.FundingBucketID, err))
	}
	return stamped, bucket, nil
}

// classifyGuardMiss names why an echo fired zero rows, from a fresh read of
// the bucket. It never guesses from stale state: the read happens after the
// verdict, in the same unit of work, so what it reports is what the next
// writer will see too.
func (r *ledgerRepo) classifyGuardMiss(ctx context.Context, entry accounting.LedgerEntry) error {
	bucket, err := scanFundingBucket(r.store.Querier(ctx).QueryRowContext(ctx, selectFundingBucket, string(entry.FundingBucketID)))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("postgres: append %s entry: funding bucket %s: %w", entry.Kind, entry.FundingBucketID, persistence.ErrNotFound)
	}
	if err != nil {
		return conflictOf(fmt.Errorf("postgres: append %s entry to bucket %s: classify guard miss: %w", entry.Kind, entry.FundingBucketID, err))
	}
	if bucket.Status != accounting.BucketActive {
		return fmt.Errorf("postgres: append %s entry to bucket %s: %w", entry.Kind, bucket.ID, accounting.ErrBucketClosed)
	}
	take := entry.Amount
	switch entry.Kind {
	case accounting.KindHold:
		return fmt.Errorf("postgres: append hold entry to bucket %s: %w: %d available, %d requested",
			bucket.ID, accounting.ErrInsufficientAvailable, bucket.Available, take)
	case accounting.KindRelease:
		// The echo's WHERE names two verdicts — the bucket cannot afford the
		// return, or the named reservation has no hold on file. The fresh
		// read separates them in that order, the echo's own.
		if bucket.Held < accounting.Balance(take.Int64()) {
			return fmt.Errorf("postgres: append release entry to bucket %s: %w: %d held, %d released",
				bucket.ID, accounting.ErrInsufficientHeld, bucket.Held, take)
		}
		onFile, err := r.holdOnFile(ctx, bucket.ID, entry.ReservationID)
		if err != nil {
			return conflictOf(fmt.Errorf("postgres: append release entry to bucket %s: classify guard miss: %w", bucket.ID, err))
		}
		if !onFile {
			return fmt.Errorf("postgres: append release entry to bucket %s: %w: reservation %s has no hold leg on file here",
				bucket.ID, accounting.ErrInvalidReference, entry.ReservationID)
		}
		return fmt.Errorf("postgres: append release entry to bucket %s: %w: the guard's verdict does not reproduce on a fresh read",
			bucket.ID, accounting.ErrInvalidTransition)
	case accounting.KindConsume:
		if bucket.Held < accounting.Balance(take.Int64()) {
			return fmt.Errorf("postgres: append consume entry to bucket %s: %w: %d held, %d consumed",
				bucket.ID, accounting.ErrInsufficientHeld, bucket.Held, take)
		}
		return fmt.Errorf("postgres: append consume entry to bucket %s: %w: %d settled, %d consumed",
			bucket.ID, accounting.ErrInsufficientSettled, bucket.Settled, take)
	case accounting.KindAdjustment:
		// Same split, same order: the balance fit first — re-run as the
		// domain guard against the fresh row — then the cited entry's
		// provenance.
		if _, applyErr := entry.ApplyTo(bucket); applyErr != nil {
			return fmt.Errorf("postgres: append adjustment entry to bucket %s: %w: settled %d, held %d, available %d does not admit the stated deltas",
				bucket.ID, accounting.ErrInvalidAdjustment, bucket.Settled, bucket.Held, bucket.Available)
		}
		own, err := r.correctsOwnBucket(ctx, entry.OriginalEntryID, bucket.ID)
		if err != nil {
			return conflictOf(fmt.Errorf("postgres: append adjustment entry to bucket %s: classify guard miss: %w", bucket.ID, err))
		}
		if !own {
			return fmt.Errorf("postgres: append adjustment entry to bucket %s: %w: the entry it corrects, %s, is not this bucket's",
				bucket.ID, accounting.ErrInvalidReference, entry.OriginalEntryID)
		}
		return fmt.Errorf("postgres: append adjustment entry to bucket %s: %w: the guard's verdict does not reproduce on a fresh read",
			bucket.ID, accounting.ErrInvalidTransition)
	case accounting.KindGrant, accounting.KindTopup:
		// The only guard these echoes carry beyond the active gate is the
		// owner rule, and the owner cannot change under a leg: an active
		// bucket here is the wrong owner for this kind.
		if entry.Kind == accounting.KindGrant && bucket.OwnedByAccount() {
			return fmt.Errorf("postgres: append grant entry to bucket %s: %w: a grant funds a cycle bucket, and %s is account %s's",
				bucket.ID, accounting.ErrInvalidTransition, bucket.ID, bucket.AccountID)
		}
		if entry.Kind == accounting.KindTopup && bucket.OwnedByEntitlement() {
			return fmt.Errorf("postgres: append topup entry to bucket %s: %w: a topup funds an account bucket, and %s is entitlement %s's cycle",
				bucket.ID, accounting.ErrInvalidTransition, bucket.ID, bucket.EntitlementID)
		}
		return fmt.Errorf("postgres: append %s entry to bucket %s: %w: guard refused an unguarded kind",
			entry.Kind, bucket.ID, accounting.ErrInvalidTransition)
	default:
		return fmt.Errorf("postgres: append %s entry to bucket %s: %w: guard refused an unguarded kind",
			entry.Kind, bucket.ID, accounting.ErrInvalidTransition)
	}
}

// ledgerEntryArgs binds the leg insert's seventeen columns, turning the
// domain's zero values into NULLs explicitly — an empty string is not.
func ledgerEntryArgs(e accounting.LedgerEntry) []any {
	var settlementID, reservationID, commandKey any
	if e.SettlementID != "" {
		settlementID = string(e.SettlementID)
	}
	if e.ReservationID != "" {
		reservationID = string(e.ReservationID)
	}
	if e.CommandKey != "" {
		commandKey = string(e.CommandKey)
	}
	var priceRevision, inputPrice, outputPrice any
	if e.Price != nil {
		priceRevision = string(e.Price.RevisionID)
		inputPrice = e.Price.InputUnitPrice.Int64()
		outputPrice = e.Price.OutputUnitPrice.Int64()
	}
	var reason, originalEntryID, operatorID any
	if e.AdjustmentReason != "" {
		reason = e.AdjustmentReason
	}
	if e.OriginalEntryID != "" {
		originalEntryID = string(e.OriginalEntryID)
	}
	if e.OperatorID != "" {
		operatorID = string(e.OperatorID)
	}
	return []any{
		string(e.ID), string(e.FundingBucketID), string(e.Kind), e.Amount.Int64(),
		e.SettledDelta.Int64(), e.HeldDelta.Int64(),
		settlementID, reservationID, commandKey,
		priceRevision, inputPrice, outputPrice,
		reason, originalEntryID, operatorID,
		e.Sequence, e.CreatedAt,
	}
}

func (r *ledgerRepo) ByBucketAndCommandKey(ctx context.Context, bucketID accounting.FundingBucketID, commandKey accounting.CommandKey) (accounting.LedgerEntry, error) {
	entry, err := scanLedgerEntry(r.store.Querier(ctx).QueryRowContext(ctx, selectLedgerByCommandKey,
		string(bucketID), string(commandKey)))
	if err != nil {
		return accounting.LedgerEntry{}, wrapLedgerRead(err, "command key "+string(commandKey), bucketID)
	}
	return entry, nil
}

func (r *ledgerRepo) ByBucketReservationAndKind(ctx context.Context, bucketID accounting.FundingBucketID, reservationID accounting.ReservationID, kind accounting.Kind) (accounting.LedgerEntry, error) {
	entry, err := scanLedgerEntry(r.store.Querier(ctx).QueryRowContext(ctx, selectLedgerByReservationKind,
		string(bucketID), string(reservationID), string(kind)))
	if err != nil {
		return accounting.LedgerEntry{}, wrapLedgerRead(err, "reservation "+string(reservationID)+" "+string(kind), bucketID)
	}
	return entry, nil
}

// scanLedgerEntry scans one leg row out of any single-row result, reassembling
// the optional columns the insert spread across NULLs.
func scanLedgerEntry(row *sql.Row) (accounting.LedgerEntry, error) {
	var e accounting.LedgerEntry
	var id, bucketID, kind string
	var settlementID, reservationID, commandKey sql.NullString
	var priceRevision, reason, originalEntryID, operatorID sql.NullString
	var inputPrice, outputPrice sql.NullInt64
	if err := row.Scan(&id, &bucketID, &kind, &e.Amount, &e.SettledDelta, &e.HeldDelta,
		&settlementID, &reservationID, &commandKey,
		&priceRevision, &inputPrice, &outputPrice,
		&reason, &originalEntryID, &operatorID,
		&e.Sequence, &e.CreatedAt); err != nil {
		return accounting.LedgerEntry{}, err
	}
	e.ID = accounting.LedgerEntryID(id)
	e.FundingBucketID = accounting.FundingBucketID(bucketID)
	e.Kind = accounting.Kind(kind)
	if settlementID.Valid {
		e.SettlementID = accounting.SettlementID(settlementID.String)
	}
	if reservationID.Valid {
		e.ReservationID = accounting.ReservationID(reservationID.String)
	}
	if commandKey.Valid {
		e.CommandKey = accounting.CommandKey(commandKey.String)
	}
	if priceRevision.Valid {
		e.Price = &accounting.PriceSnapshot{
			RevisionID:      accounting.PriceRevisionID(priceRevision.String),
			InputUnitPrice:  accounting.Amount(inputPrice.Int64),
			OutputUnitPrice: accounting.Amount(outputPrice.Int64),
		}
	}
	if reason.Valid {
		e.AdjustmentReason = reason.String
	}
	if originalEntryID.Valid {
		e.OriginalEntryID = accounting.LedgerEntryID(originalEntryID.String)
	}
	if operatorID.Valid {
		e.OperatorID = accounting.OperatorID(operatorID.String)
	}
	return e, nil
}

// wrapLedgerRead turns a ledger lookup's failure into the port's vocabulary.
func wrapLedgerRead(err error, what string, bucketID accounting.FundingBucketID) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("postgres: ledger entry for %s on bucket %s: %w", what, bucketID, persistence.ErrNotFound)
	}
	return fmt.Errorf("postgres: ledger entry for %s on bucket %s: %w", what, bucketID, err)
}

// ---------------------------------------------------------------------------
// settlements — the exactly-once headers.
// ---------------------------------------------------------------------------

type settlementRepo struct {
	store persistence.Store
}

// The header insert, keyed by the request. ON CONFLICT DO NOTHING is the
// whole exactly-once mechanic: the first acknowledgement writes the header
// and reports true, every later one reports false and reads the recorded
// settlement to compare totals — the legs are appended only by the call the
// header belongs to, in the same unit of work.
const insertSettlement = `
INSERT INTO control.settlements (id, request_id, settled_total, created_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (request_id) DO NOTHING`

const selectSettlementByRequestID = `
SELECT id, request_id, settled_total, created_at
FROM control.settlements
WHERE request_id = $1`

func (r *settlementRepo) Create(ctx context.Context, settlement accounting.Settlement) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, insertSettlement,
		string(settlement.ID), string(settlement.RequestID), settlement.SettledTotal.Int64(), settlement.CreatedAt)
	if err != nil {
		// The request-id unique index cannot fire here — ON CONFLICT absorbs
		// it — and the id is fresh; whatever fails is infrastructure.
		return false, conflictOf(fmt.Errorf("postgres: create settlement %s for request %s: %w",
			settlement.ID, settlement.RequestID, err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: create settlement %s for request %s: read rows affected: %w",
			settlement.ID, settlement.RequestID, err)
	}
	return n == 1, nil
}

func (r *settlementRepo) ByRequestID(ctx context.Context, requestID accounting.RequestID) (accounting.Settlement, error) {
	var s accounting.Settlement
	var id string
	err := r.store.Querier(ctx).QueryRowContext(ctx, selectSettlementByRequestID, string(requestID)).
		Scan(&id, &s.RequestID, &s.SettledTotal, &s.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return accounting.Settlement{}, fmt.Errorf("postgres: settlement for request %s: %w", requestID, persistence.ErrNotFound)
		}
		return accounting.Settlement{}, fmt.Errorf("postgres: settlement for request %s: %w", requestID, err)
	}
	s.ID = accounting.SettlementID(id)
	return s, nil
}

// ---------------------------------------------------------------------------
// funding_buckets, again — the ledger's own derivation of the balances.
// ---------------------------------------------------------------------------

type fundingProjectionRepo struct {
	store persistence.Store
}

// The projection definition, computed from the legs alone. Available is
// settled minus held BY LEG as by row: a leg's available delta is its
// settled delta minus its held delta (a hold takes available out, a release
// puts it back), and consume's two moves cancel under exactly that sign —
// spending converts held money into spent money and moves available not at
// all. So the third SUM is settled_delta - held_delta, the same shape the
// echo's available_amount assignment restates. SUM over bigint widens to
// numeric in PostgreSQL; the cast back to bigint is deliberate, so an
// overflow here is a loud error at the driver instead of a silently-typed
// numeric scan. The COUNT is the leg count Reconcile reports alongside the
// balances — a bucket with a drifted cache and no legs is a different
// defect from one with ten thousand.
const selectDerivedBalances = `
SELECT COALESCE(SUM(settled_delta), 0)::bigint,
       COALESCE(SUM(held_delta), 0)::bigint,
       COALESCE(SUM(settled_delta - held_delta), 0)::bigint,
       COUNT(*)
FROM control.ledger_entries
WHERE funding_bucket_id = $1`

func (r *fundingProjectionRepo) DerivedBalances(ctx context.Context, bucketID accounting.FundingBucketID) (accounting.Derivation, error) {
	var d accounting.Derivation
	var settled, held, available int64
	if err := r.store.Querier(ctx).QueryRowContext(ctx, selectDerivedBalances, string(bucketID)).
		Scan(&settled, &held, &available, &d.Legs); err != nil {
		return accounting.Derivation{}, fmt.Errorf("postgres: derive balances of bucket %s: %w", bucketID, err)
	}
	d.Settled = accounting.Balance(settled)
	d.Held = accounting.Balance(held)
	d.Available = accounting.Balance(available)
	return d, nil
}

// conflictOf re-wraps a write failure that lost the store's own race —
// serialization (40001) or deadlock (40P01) — as the port's retryable
// ErrConflict. Anything else passes through unchanged; the guard's own
// verdicts never reach this helper, because they are not errors.
func conflictOf(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == pgSerializationFailure || pgErr.Code == pgDeadlockDetected) {
		return fmt.Errorf("postgres: %w: %s", persistence.ErrConflict, err)
	}
	return err
}
