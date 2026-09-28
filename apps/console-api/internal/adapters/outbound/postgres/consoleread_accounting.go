package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The console's accounting reads: an account's funding buckets with their
// three cached balances, and one bucket's ledger.
//
// Three facts govern this file, and each of them is a refusal.
//
// The balances are the CACHED COLUMNS, returned as stored. The schema itself
// maintains them — funding_buckets_balance_projection pins available =
// settled − held at write time, and every leg moves them inside the same
// transaction that writes the leg. There is no SUM here and no member that
// could grow one: a Σ over grant/topup/hold/release/consume/adjustment is
// not a number any of those kinds means, and a balance derived from a page
// of the ledger is a balance derived from a page.
//
// The ledger is PER BUCKET, and the keyset is (funding_bucket_id, sequence) —
// which is the schema's ledger_entries_bucket_sequence_key, exactly. A page is
// therefore a direct range scan on an index that already exists: no
// migration, and on a table this lane builds with plain CREATE INDEX rather
// than CONCURRENTLY, no exclusive lock either. A cross-bucket timeline keyed
// on created_at would need an index that does not exist, built
// non-concurrently on the fastest-growing table in the plane, to answer a
// question a ledger should never be asked as one whole.
//
// The account predicate is in the WHERE clause and the first argument on
// both. On the ledger it is evaluated against the BUCKET's account_id rather
// than through a join to the legs, because the legs table carries no account
// column: the account that owns money in a bucket is the account the bucket
// names. A bucket belonging to another account is a bucket the first
// predicate does not match, and the answer is an empty page at the cost of an
// empty page — a 404 the query did not return, never a 403.

// NewAccountBuckets returns the console's bucket-list repository and
// NewAccountLedger its per-bucket ledger read. Both panic on a nil store for
// the reason every constructor in this package does.
func NewAccountBuckets(store persistence.Store) persistence.AccountBuckets {
	if store == nil {
		panic("postgres: NewAccountBuckets requires a non-nil persistence.Store")
	}
	return &accountBucketsRepo{store: store}
}

func NewAccountLedger(store persistence.Store) persistence.AccountLedger {
	if store == nil {
		panic("postgres: NewAccountLedger requires a non-nil persistence.Store")
	}
	return &accountLedgerRepo{store: store}
}

// Compile-time proof that the repositories satisfy the port's contracts.
var (
	_ persistence.AccountBuckets = (*accountBucketsRepo)(nil)
	_ persistence.AccountLedger  = (*accountLedgerRepo)(nil)
)

// ---------------------------------------------------------------------------
// funding_buckets — the account's, with the balances as stored.
// ---------------------------------------------------------------------------

type accountBucketsRepo struct {
	store persistence.Store
}

// The account predicate is on funding_buckets.account_id and it is first. An
// entitlement bucket's owning account is in that column too, so one
// predicate covers both kinds and no join to entitlements is needed to
// evaluate it — which is the point: the account is a column on the row being
// returned, not a fact to be joined in from another aggregate.
//
// The three balances are the cached columns, selected as they are stored.
// The projection is the schema's, maintained by the guarded echo every leg
// runs, and a reader that derived one from the others would be doing the
// ledger's algebra in a place with no authority behind it.
const listAccountBuckets = `
SELECT ` + fundingBucketColumns + `
FROM control.funding_buckets
WHERE account_id = $1
  AND id > $2
ORDER BY id
LIMIT $3`

func (r *accountBucketsRepo) ListForAccount(ctx context.Context, accountID accounting.AccountID, page persistence.FundingBucketPage) ([]accounting.Bucket, error) {
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list funding buckets for account %s: %w", accountID, err)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, listAccountBuckets,
		string(accountID), string(page.After), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list funding buckets for account %s: %w", accountID, err)
	}
	defer func() { _ = rows.Close() }()

	buckets := make([]accounting.Bucket, 0, limit)
	for rows.Next() {
		bucket, scanErr := scanBucket(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("postgres: list funding buckets for account %s: %w", accountID, scanErr)
		}
		buckets = append(buckets, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list funding buckets for account %s: %w", accountID, err)
	}
	return buckets, nil
}

// ---------------------------------------------------------------------------
// ledger_entries — one bucket's history, keyed on the bucket's sequence.
// ---------------------------------------------------------------------------

type accountLedgerRepo struct {
	store persistence.Store
}

// The account predicate is the FIRST clause and it reads the bucket, not the
// legs. The EXISTS is correlated on the bucket id alone, so the planner
// evaluates it once per page rather than per leg, and a bucket another
// account owns matches nothing before a single row is scanned.
//
// The keyset is the bucket's own allocated sequence: strictly increasing per
// bucket, allocated by the store inside the transaction that wrote the leg,
// and together with the bucket id it IS the schema's unique index. The zero
// value is the beginning of the history — a bucket's first leg is sequence 1,
// so `sequence > 0` matches all of them.
//
// The kind filter is the fourth argument and it is in this statement rather
// than applied to rows that arrived, for the same reason the state filter is
// in the users list: a filter the query does not carry is a filter whose
// absence would return something the caller did not ask for.
const listBucketLedger = `
SELECT ` + ledgerEntryColumns + `
FROM control.ledger_entries
WHERE funding_bucket_id = $2
  AND EXISTS (
        SELECT 1
        FROM control.funding_buckets
        WHERE funding_buckets.id = ledger_entries.funding_bucket_id
          AND funding_buckets.account_id = $1
      )
  AND sequence > $3
  AND ($4 = '' OR kind = $4)
ORDER BY sequence
LIMIT $5`

func (r *accountLedgerRepo) ListForBucket(ctx context.Context, accountID accounting.AccountID, bucketID accounting.FundingBucketID, page persistence.LedgerPage) ([]accounting.LedgerEntry, error) {
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list ledger for bucket %s: %w", bucketID, err)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, listBucketLedger,
		string(accountID), string(bucketID), page.AfterSequence, page.Kind, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list ledger for bucket %s: %w", bucketID, err)
	}
	defer func() { _ = rows.Close() }()

	entries := make([]accounting.LedgerEntry, 0, limit)
	for rows.Next() {
		entry, scanErr := scanLedgerEntry(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("postgres: list ledger for bucket %s: %w", bucketID, scanErr)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list ledger for bucket %s: %w", bucketID, err)
	}
	return entries, nil
}

// ---------------------------------------------------------------------------
// the bucket scanner, shared with ByID above so the two cannot drift.
// ---------------------------------------------------------------------------

// scanBucket reads one funding_buckets row, with the three cached balances
// scanned straight into the domain's own fields and returned AS STORED.
//
// The one thing this scanner does not do is check the projection. A row the
// schema accepted already satisfies available = settled − held; a row that
// does not is a finding the reconciliation pass exists to record, and a
// reader that recomputed and corrected would be hiding it.
func scanBucket(row rowScanner) (accounting.Bucket, error) {
	var bucket accounting.Bucket
	var status string
	var entitlementID, accountID sql.NullString
	if err := row.Scan(&bucket.ID, &entitlementID, &accountID, &status, &bucket.Version,
		&bucket.LastSequence, &bucket.Settled, &bucket.Held, &bucket.Available,
		&bucket.CreatedAt, &bucket.UpdatedAt); err != nil {
		return accounting.Bucket{}, err
	}
	bucket.Status = accounting.BucketStatus(status)
	bucket.EntitlementID = accounting.EntitlementID(entitlementID.String)
	bucket.AccountID = accounting.AccountID(accountID.String)
	return bucket, nil
}
