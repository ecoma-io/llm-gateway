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
// The account predicate is in the WHERE clause and the first argument on both
// statements. On the ledger it is evaluated against the BUCKET rather than
// through a join to the legs, because the legs table carries no account
// column: the account that owns money in a bucket is the account the bucket
// names. A bucket belonging to another account is a bucket the predicate does
// not match, and the answer is an empty page at the cost of an empty page — a
// 404 the query did not return, never a 403.
//
// And on both, "the account the bucket names" is a two-way question, because a
// bucket has exactly one owner and the two kinds name it in two different
// places. A PAYG bucket names the account in its own row; a cycle bucket names
// an entitlement, and the account is one hop further out. Each statement below
// therefore carries both, and the comment on each says why the one that was
// there alone could not reach half the rows in its own table.

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

// The account predicate is a DISJUNCTION of the two owner kinds, and it is
// first. A bucket has exactly one owner — the schema's funding_buckets_owner_xor
// pins `(entitlement_id IS NULL) <> (account_id IS NULL)`, so no row may carry
// both and no row may carry neither — which means the account that owns a
// bucket is reachable two ways and the statement has to ask both of them:
//
//   - an account-kind (PAYG) bucket names its owner in its own account_id
//     column, so the predicate is a column comparison;
//   - an entitlement-kind (cycle) bucket names an ENTITLEMENT, and the
//     account that paid is reached from there: entitlement → subscription →
//     account, the same chain consoleread_commerce.go's entitlement list
//     already walks to resolve the owner of a grant.
//
// The first branch is not an optimisation and the second is not a fallback
// between them. An entitlement bucket carries NO account id — that is not a
// gap in the data but the CHECK's own requirement, and a row that had one
// would be rejected at insert — so a statement carrying only the column
// comparison is not a statement that mostly works on cycle buckets, it is a
// statement that can never return one. That is the defect this comment
// replaces, and the prose that stood here claimed the opposite: that "an
// entitlement bucket's owning account is in that column too, so one predicate
// covers both kinds and no join to entitlements is needed". The intent was
// right and the column does not hold what it assumed. Written down, believed,
// and never checked against the schema it describes — which is why the
// permission to page an operator's money as an empty 200 arrived with a
// COMMENT and no test.
//
// The two branches are ONE statement on purpose, twice over. ADR 0012 §1 makes
// the authorization predicate a property of the query, and a predicate assembled
// in Go from a superset then filtered down is a predicate that has already done
// the damage: the rows cross the port boundary before they are judged. And a
// two-statement version would be two shapes for the predicate to be missing
// from, with the second one the variant that ships — which is exactly what
// happened to the same read's ledger twin below.
//
// The EXISTS is correlated on the bucket row alone, so the planner evaluates
// the owner chain once per candidate bucket rather than once per bucket per
// page, and a bucket owned by somebody else matches nothing before its balances
// are read. entitlements.subscription_id carries the index the correlation
// walks (migrations/control/000011, shipped with this read), and
// subscriptions_account_id_idx already covers the outer hop.
//
// The three balances are the cached columns, selected as they are stored. The
// projection is the schema's, maintained by the guarded echo every leg runs,
// and a reader that derived one from the others would be doing the ledger's
// algebra in a place with no authority behind it.
const listAccountBuckets = `
SELECT ` + fundingBucketColumns + `
FROM control.funding_buckets
WHERE (
        account_id = $1
        OR EXISTS (
              SELECT 1
              FROM control.entitlements
              JOIN control.subscriptions
                ON subscriptions.id = entitlements.subscription_id
              WHERE entitlements.id = funding_buckets.entitlement_id
                AND subscriptions.account_id = $1
          )
      )
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

// The account predicate is the FIRST clause and it reads the BUCKET, not the
// legs — the legs table carries no account column at all, so the only place an
// owner's identity can be evaluated is the bucket row each leg belongs to.
//
// Within it, the two owner kinds are asked separately for the same reason the
// list above asks them as a disjunction: funding_buckets_owner_xor guarantees a
// cycle bucket's account_id is NULL, and a statement that compares that column
// to a uuid can never match a cycle bucket. So the predicate is
//
//	EXISTS (the bucket names this account directly)
//	OR EXISTS (the bucket's entitlement's subscription names this account)
//
// and no kind test is needed to keep the branches apart, because the first one
// cannot match a cycle bucket and the second cannot match a PAYG bucket: the
// entitlements join is on `entitlements.id = funding_buckets.entitlement_id`,
// and a PAYG row's entitlement_id is NULL, so no entitlement row matches it.
// The branches are disjoint by the CHECK, and the joins are what make each one
// say so. Written as a test on entitlement_id in Go, or as one EXISTS with the
// kind test in the WHERE, the statement would be the same query with the kind
// test somewhere a reader has to go looking for it.
//
// The correlation is on the bucket id alone, so the planner evaluates the owner
// chain once per page rather than per leg — the point of the whole shape,
// because a per-leg owner resolution over a long history is a plan that grows
// with the page instead of with the bucket.
//
// The answer to a bucket this account does not own is an empty page, never a
// 403: a 403 would confirm the bucket exists.
//
// The keyset is the bucket's own allocated sequence: strictly increasing per
// bucket, allocated by the store inside the transaction that wrote the leg,
// and together with the bucket id it IS the schema's unique index. The zero
// value is the beginning of the history — a bucket's first leg is sequence 1,
// so `sequence > 0` matches all of them. The ledger's keyset is bigint, so
// unlike the uuid-keyed lists it does hold against the zero value's empty
// string; that is a property of the column, not a decision taken here.
//
// The kind filter is the fourth argument and it is in this statement rather
// than applied to rows that arrived, for the same reason the state filter is
// in the users list: a filter the query does not carry is a filter whose
// absence would return something the caller did not ask for.
const listBucketLedger = `
SELECT ` + ledgerEntryColumns + `
FROM control.ledger_entries
WHERE funding_bucket_id = $2
  AND (
        EXISTS (
              SELECT 1
              FROM control.funding_buckets
              WHERE funding_buckets.id = ledger_entries.funding_bucket_id
                AND funding_buckets.account_id = $1
          )
        OR EXISTS (
              SELECT 1
              FROM control.funding_buckets
              JOIN control.entitlements
                ON entitlements.id = funding_buckets.entitlement_id
              JOIN control.subscriptions
                ON subscriptions.id = entitlements.subscription_id
              WHERE funding_buckets.id = ledger_entries.funding_bucket_id
                AND subscriptions.account_id = $1
          )
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
