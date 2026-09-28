package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/analytics"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// bucketIDForm is the same RFC 9562 version-7 shape the ingestion domain
// checks before a fact's allocation tail is accepted — so every bucket id
// reaching this point has already passed it. It is duplicated here rather
// than imported to avoid a domain dependency in the adapter layer, and the
// equality of the two patterns is the tripwire: a divergence would mean an
// id the ingestion accepted reaches the adapter and fails here, which is
// exactly the silent-drop failure the NOT NULL column exists to prevent.
var bucketIDForm = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// The analytics repositories: translation between the persistence port's
// derived-state vocabulary and the `control` database's settlement, ledger,
// bucket and fact-dimension tables.
//
// This file is the read model, and the property it implements is that
// analytics is DERIVED. Every figure below is reconstructable from state the
// ledger already holds, so the rebuild path is this same read at a later
// instant rather than a second writer. There is no INSERT here except the
// ingestion-time attribution, and it is a denormalized copy of something the
// derivation already knows — not a new fact about the world.
//
// What this file deliberately does not do, and each of these is a decision
// rather than an omission:
//
//   - It does not re-derive a per-request amount. Every money figure below is
//     a SUM over settlement headers or ledger legs, and the header is the
//     authority precisely because it is written once and summed: the
//     derivation applies one ceiling over a request's summed raw cost, so a
//     per-request amount is ROUNDED, and the sum of rounded figures is not
//     the rounded total. A thousand one-minor-unit requests settle for one
//     minor unit between them; summing their per-request ceilings would bill a
//     thousand, and the error would GROW with volume — the more successful a
//     month looked, the more it overstated.
//   - It does not join a price list. Spend is priced from the frozen snapshot
//     on the consume leg, never from a current price revision, because
//     joining the live list would restate last month's consumption at today's
//     prices — a figure that is wrong, internally consistent, and reconcilable
//     against nothing.
//   - It does not mix flows and balances. The three money figures are sums
//     over a window; held and available come from the projection's cached
//     columns as at the range's end and are never re-derived from the legs.
//     A third figure beside the cache and the legs is a number ADR 0011 has
//     no reconciliation check for, which is the last thing a read model
//     should introduce.
//   - It does not straddle the two clocks. Request counts bucket on
//     applied_at — when THIS PLANE recorded the derivation — and money
//     buckets on created_at, the database clock at booking. Neither is the
//     fact's occurred_at, which is the runtime's clock and is telemetry only.
//   - It does not scan a growing table for a bounded answer. The two
//     created_at indexes exist because without them a 90-day money series is
//     a sequential scan of the whole ledger, and against a fixed
//     statement_timeout that is a report whose latency grows without limit.

// pgQueryCanceled and pgAdminShutdown are the two SQLSTATEs a bounded read
// can end on because the bound was reached, as opposed to a statement that
// failed. They are what a statement_timeout raises, and they must not be
// confused with a genuine query error: the surface answers the first with a
// retry-later refusal and the second with a failure, and a caller that got the
// wrong one would treat a temporarily slow report as a broken one.
//
// The names are constants because the classification is a claim, and a claim
// expressed as two string literals in a switch is a claim no test can name:
// this file is the one place a tier can pose "a bound reached mid-walk is the
// same refusal as a bound reached at the statement", and it can only ask it of
// values the production code reads.
const (
	pgQueryCanceled = "57014"
	pgAdminShutdown = "57P01"
)

var (
	errAnalyticsReadExceeded      = errors.New("postgres: analytics read exceeded its bound")
	errAnalyticsOutsideUnitOfWork = errors.New("postgres: analytics fact attribution requires a unit of work")
	errInvalidBucketReference     = errors.New("invalid funding bucket reference")
)

// NewFactDimensions returns the persistence port's FactDimensions repository
// backed by store. It panics on a nil store for the same reason every
// constructor here does: the failure a nil dependency produces later is
// strictly worse than a loud one here.
func NewFactDimensions(store persistence.Store) persistence.FactDimensions {
	if store == nil {
		panic("postgres: NewFactDimensions requires a non-nil persistence.Store")
	}
	return &factDimensionsRepo{store: store}
}

// NewAnalytics returns the persistence port's Analytics read model backed by
// store.
func NewAnalytics(store persistence.Store) persistence.Analytics {
	if store == nil {
		panic("postgres: NewAnalytics requires a non-nil persistence.Store")
	}
	return &analyticsRepo{store: store}
}

type factDimensionsRepo struct {
	store persistence.Store
}

// Record inserts the account attribution for one applied fact, inside the
// caller's unit of work.
//
// The ON CONFLICT DO NOTHING is the same convergence the applied-facts ledger
// uses, and for the same reason: the redelivery that reaches that ledger
// reaches this one in the same page, and a second row would be a second
// account for a fact that has one.
//
// The conflict target carries the ACCOUNT as well as the fact's own key, and
// that is not a detail. A settlement's allocation tail can name buckets owned
// by more than one account — the waterfall plans per bucket, and nothing in
// funding_buckets_owner_xor stops a fact drawing on a PAYG bucket of one
// account and an entitlement bucket of another — so the caller records one row
// per account. A target of (request_id, kind_class) alone would make every
// account after the first collide into DO NOTHING, and the collision would be
// invisible: the statement succeeds, the row is simply not there, and the
// dropped account's report is short by exactly the requests it funded. That is
// a figure wrong in the direction that looks like good news, which is the worst
// direction for a number to be wrong in.
func (r *factDimensionsRepo) Record(ctx context.Context, dimension persistence.FactDimension) error {
	if !r.store.InUnitOfWork(ctx) {
		return fmt.Errorf("postgres: record analytics fact dimension for request %s class %s: %w",
			dimension.RequestID, dimension.KindClass, errAnalyticsOutsideUnitOfWork)
	}
	//
	// applied_at is stamped by now() rather than passed in, and that is the
	// copy-is-exact-by-construction claim the migration's column comment
	// rests on: applied_facts.applied_at defaults to now(), PostgreSQL
	// defines now() as transaction_timestamp(), and this row is written inside
	// the same transaction as the fact it belongs to. The two columns are
	// therefore equal by construction rather than by a caller keeping them in
	// step — a design that took the instant as a parameter would make the
	// equality a promise about every future caller, and the whole reason the
	// read model can scope a range over this table alone is that the instant
	// is not a thing anybody can get wrong.
	//
	// The column carries NO default of its own, deliberately: a default here
	// would let a row be written without a stamp by any writer that was not
	// this one, including a hand-rolled INSERT, and the read would then have
	// to cope with a NULL it could not have predicted. The NOT NULL is what
	// makes that unreachable.
	_, err := r.store.Querier(ctx).ExecContext(ctx, `
		INSERT INTO control.analytics_fact_dimensions
			(request_id, kind_class, account_id, append_seq, applied_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (request_id, kind_class, account_id) DO NOTHING
	`, dimension.RequestID, dimension.KindClass, dimension.AccountID, dimension.AppendSeq)
	if err != nil {
		return fmt.Errorf("postgres: record analytics fact dimension for request %s class %s: %w",
			dimension.RequestID, dimension.KindClass, err)
	}
	return nil
}

// AccountsOf resolves the accounts behind a set of funding buckets.
//
// The resolution is a UNION over both ways a bucket is owned rather than a COALESCE
// between them: a PAYG bucket names its account directly and an entitlement
// bucket reaches it through the subscription the entitlement was granted under.
// A single COALESCE(b.account_id, s.account_id) would be shorter and would be
// wrong in the same way everywhere — it depends on the join producing a row
// only for the side that owns the bucket, and a bucket whose entitlement
// subscription row is missing would resolve to NULL and drop the fact's
// account entirely, which is the failure mode a NOT NULL column exists to
// prevent.
func (r *factDimensionsRepo) AccountsOf(ctx context.Context, bucketIDs []string) ([]string, error) {
	if len(bucketIDs) == 0 {
		// An allocation tail with no legs is a zero-priced settle, which is
		// a real settlement of record and books no capacity. It belongs to
		// no account on this plane, and the caller records no attribution
		// for it rather than inventing one.
		return nil, nil
	}
	// The bucket ids travel as ONE comma-joined string parameter and are
	// split server-side with string_to_array. Three reasons, in order of
	// weight:
	//
	//   - It keeps the statement's shape fixed. A variadic IN list would
	//     need a placeholder per bucket, and a statement whose text grows
	//     with caller input is the one thing this file must never build.
	//   - A uuid contains no comma, and every id here came through the
	//     ingestion domain's uuid grammar check before reaching this point,
	//     so the separator cannot occur inside a value.
	//   - It needs no driver array type. pgtype has no UUIDArray, and a
	//     hand-rolled one would be a second encoding of the same value for
	//     one call site.
	buckets, err := uuidArray(bucketIDs)
	if err != nil {
		return nil, fmt.Errorf("postgres: resolve accounts for %d funding buckets: %w", len(bucketIDs), err)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, `
		WITH named(bucket) AS (
			SELECT unnest(string_to_array($1, ','))::uuid
		)
		SELECT DISTINCT owner
		FROM (
			SELECT b.account_id::text AS owner
			FROM control.funding_buckets b
			JOIN named n ON n.bucket = b.id
			WHERE b.account_id IS NOT NULL
			UNION
			SELECT s.account_id::text AS owner
			FROM control.funding_buckets b
			JOIN named n ON n.bucket = b.id
			JOIN control.entitlements e ON e.id = b.entitlement_id
			JOIN control.subscriptions s ON s.id = e.subscription_id
			WHERE b.entitlement_id IS NOT NULL
		) owners
	`, buckets)
	if err != nil {
		return nil, fmt.Errorf("postgres: resolve accounts for %d funding buckets: %w", len(bucketIDs), err)
	}
	defer func() { _ = rows.Close() }()

	accounts := make([]string, 0, len(bucketIDs))
	for rows.Next() {
		var account string
		if err := rows.Scan(&account); err != nil {
			return nil, fmt.Errorf("postgres: resolve accounts for %d funding buckets: scan: %w", len(bucketIDs), err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: resolve accounts for %d funding buckets: %w", len(bucketIDs), err)
	}
	return accounts, nil
}

type analyticsRepo struct {
	store persistence.Store
}

// bucketSelect is the request-count series, as a parameterised statement.
//
// The bucket boundaries arrive as a VALUES list rather than as a width and a
// stride, which is what lets the domain own the calendar arithmetic and the
// store aggregate against intervals the store never re-derives. Each bucket is
// found by the range of instants that fall inside it, and the join is a range
// join rather than an equality on a truncated timestamp for the same reason:
// truncating to a DAY in SQL would truncate in UTC, and a day in the caller's
// zone is not a day in UTC. Half-open on both sides — >= start AND < end — so
// an instant on a boundary belongs to exactly one bucket.
//
// The two counts are two DISTINCT populations and are deliberately separate
// subqueries rather than one grouped COUNT with two CASE arms. `settled` is
// read from the fact dimensions joined to settlements, so it counts SETTLEMENTS
// OF RECORD and can never exceed the requests those settlements name; the two
// halves cannot disagree because they are two sets, not one set counted twice.
//
// Both counts are of REQUESTS, so both are COUNT(DISTINCT d.request_id) and
// not COUNT(*). The dimension table holds one row per (request, fact class),
// so a request that arrived with a settlement AND an unbillable orphan —
// which is the ordinary shape of a request the gateway could not bill — has
// two rows and is one request. A COUNT(*) would report it twice, and the
// duplicate is not a rounding error a reader can see: a chart's y-axis
// doubles, and a rate computed over the count is wrong by a factor of two on
// exactly the requests that were hardest to serve. DISTINCT is the whole
// difference, and it is invisible in the figure until the numbers are read
// against the request log.
// The two timestamp columns in the VALUES list are CAST rather than left for
// the driver to infer, and the casts are the whole reason this statement runs
// at all. A bare placeholder in a VALUES list is an UNKNOWN-typed parameter:
// PostgreSQL has no column to infer it from, and the comparison it exists to
// serve then fails to resolve — `operator does not exist: timestamp with time
// zone >= text`. The error is raised before a single row is read, so this is
// not a statement that works on a small table and breaks on a large one; it
// is a statement that does not work.
//
// Every other VALUES list in this adapter binds a placeholder that appears
// elsewhere in a column's position, so the type is already known by the time
// this one is bound. The bucket bounds travel in a list of their own — which
// is the whole point of handing the domain the calendar arithmetic — and so
// have to carry their own types with them.
//
// The ORDINAL is cast for a second, separate reason, and it is the one that
// makes a single-bucket query work. Without a cast, the type of an uncast
// placeholder in a VALUES list is resolved from the OTHER ROWS of the list: a
// query with two or more buckets makes every ordinal an integer and the list
// resolves, while a query with exactly one bucket has no other row to resolve
// against, the CTE column falls back to text, and the read is refused with
// `unable to encode 0 into text format for text (OID 25)`. So the statement
// worked for every series a dashboard draws and failed for the one-bucket
// series the API caller can ask for — the shape that is easiest to reach and
// hardest to notice, because it is the case a person tests by hand first.
const bucketSelect = `
WITH bounds(ord, bucket_start, bucket_end) AS (
	VALUES %s
)
SELECT bounds.ord,
       bounds.bucket_start,
       bounds.bucket_end,
       COALESCE(facts.with_facts, 0) AS with_facts,
       COALESCE(facts.settled, 0)    AS settled
FROM bounds
LEFT JOIN LATERAL (
	SELECT COUNT(DISTINCT d.request_id) AS with_facts,
	       COUNT(DISTINCT d.request_id) FILTER (WHERE s.id IS NOT NULL) AS settled
	FROM control.analytics_fact_dimensions d
	LEFT JOIN control.settlements s ON s.request_id = d.request_id
	WHERE d.account_id = $1
	  AND d.applied_at >= bounds.bucket_start
	  AND d.applied_at <  bounds.bucket_end
) facts ON TRUE
ORDER BY bounds.ord`

// accountBucketsSelect is the account's funding-bucket set, as a set
// expression.
//
// An account owns a bucket in one of two ways and the schema admits both:
// directly, for a PAYG balance, where funding_buckets.account_id is set; and
// through a subscription, for an entitlement cycle, where the bucket names an
// entitlement instead and the account is two joins away
// (control.entitlements has no account column of its own). Both arms are
// required — filtering on account_id alone drops every entitlement-funded row,
// and a figure wrong in the direction that looks like good news is the one a
// reader is least likely to question.
//
// It is a UNION rather than the more obvious `WHERE account_id = $1 OR
// entitlement_id IN (...)`, and the reason is what the planner can do with each.
// Written as a disjunction, the subquery inside the OR is hashed and applied as
// a post-filter, so the arm is read as a Seq Scan over funding_buckets however
// selective the account is. As two separate arms each predicate is a plain
// equality on the leading column of funding_buckets_account_id_key or
// funding_buckets_entitlement_id_key.
//
// Measured on a migrated database seeded with 15,502 buckets, for one account
// owning two of them: the OR form executes in 1.365ms having scanned all
// 15,502 rows and rejected 15,500; the UNION form executes in 0.173ms with an
// index condition on every arm. The OR form is not WRONG — it returns the same
// two rows — and at a small table size the planner picks a Seq Scan for the
// UNION form too, because a two-row table is always cheaper to read whole. It
// is a scan whose cost grows with every entitlement cycle ever created, against
// a statement timeout that does not grow with it.
//
// funding_buckets_account_id_key is UNIQUE, so an account owns at most one PAYG
// bucket, and funding_buckets_entitlement_id_key is UNIQUE, so an entitlement
// owns at most one. The volume this table carries is therefore one row per
// entitlement CYCLE rather than one per account, which is exactly why the arm
// that has to scan it is the one that hurts: an account with a long history is
// a handful of rows found among every cycle the platform has ever granted.
//
// Both arms are load-bearing, measured rather than assumed: for an account
// holding a PAYG balance of 1000 and an entitlement balance of 2000, a read
// scoped through account_id alone returns 1000 and the two-arm set returns
// 3000. The dropped entitlement bucket is a number that only ever goes the way
// that looks like good news, which is the direction a reader is least likely
// to question and a customer least likely to believe.
//
// The projection carries the cached balance columns as well as the identity
// ones, because the caller that aggregates them needs them and a second lookup
// to fetch them would be a second read of the same rows. This is also why the
// arms must project IDENTICAL column lists: UNION takes its column names from
// the first arm, and a set expression whose arms disagree is an error the
// database raises rather than one a reader would notice — which is the same
// defect the projection originally had, and which the database caught before a
// caller could.
//
// The account placeholder is a format argument because the two callers bind
// the account at different positions. PostgreSQL numbers parameters by position
// and a repeated $n names the same value twice, so the entitlement arm can
// reuse the direct arm's placeholder rather than the caller having to bind it
// twice.
const accountBucketsSelect = `
	SELECT b.id, b.account_id, b.entitlement_id,
	       b.settled_amount, b.held_amount, b.available_amount
	FROM control.funding_buckets b
	WHERE b.account_id = %s
	UNION
	SELECT b.id, b.account_id, b.entitlement_id,
	       b.settled_amount, b.held_amount, b.available_amount
	FROM control.funding_buckets b
	JOIN control.entitlements e ON e.id = b.entitlement_id
	JOIN control.subscriptions s ON s.id = e.subscription_id
	WHERE s.account_id = %[1]s`

// analyticsReadBound is the statement timeout every analytics read runs under.
//
// It is the same five seconds the application layer's own deadline uses, and
// the two are deliberately equal: the server-side bound is what releases the
// connection, and the application-side one is what the caller is told. A
// server bound LONGER than the application deadline would be dead weight —
// the context is cancelled first and the connection is held until PostgreSQL
// notices — and a server bound SHORTER would make a read fail as a timeout
// while the caller still believed it had time left.
const analyticsReadBound = "5s"

// Usage returns one account's derived usage over a bounded range at one grain.
//
// The read runs inside a unit of work of its own, and that is not ceremony —
// it is what makes two of this method's properties true at once:
//
//   - The statement timeout BINDS. `SET LOCAL` is scoped to a transaction and
//     is a silent no-op outside one, so a read that ran on the pool would be
//     unbounded at the server while appearing bounded in this file. The
//     bound is the reason a report that cannot finish releases its
//     connection rather than pinning one, and the pool this surface shares
//     with the settlement path is ten connections wide.
//   - The five reads are ONE snapshot. A transaction alone does not give
//     that: this plane's units of work run at READ COMMITTED, deliberately,
//     because the port's concurrency model is a single guarded statement per
//     write and no read-modify-write spans statements (persistence.go,
//     WithinTx). Under READ COMMITTED each of the five statements takes a
//     fresh snapshot when it reaches the server, so a settlement committing
//     between the series read and the money read would produce an answer that
//     is internally inconsistent: a bucket saying "settled" beside a settled
//     amount that does not include that settlement. Five statements about one
//     question must be one moment, so this read asks for its unit at
//     REPEATABLE READ.
//
// The unit is read-only, and that costs the stronger level nothing: a
// repeatable-read unit that takes no row locks cannot block a writer and
// cannot be blocked by one, so the settlement path is exactly as concurrent
// against this read as it is against the pool.
func (r *analyticsRepo) Usage(ctx context.Context, query persistence.UsageQuery) (persistence.Usage, persistence.Freshness, error) {
	if query.AccountID == "" {
		// The one shape in which this statement could disclose every
		// account's figures: an unscoped read has no predicate to remove.
		// It is refused here rather than assumed away by the caller,
		// because "the caller always passes an account" is a claim about
		// every future caller rather than a property of this one.
		return persistence.Usage{}, persistence.Freshness{},
			errors.New("postgres: analytics read refused: no account scope")
	}
	if len(query.Buckets) == 0 {
		return persistence.Usage{}, persistence.Freshness{},
			errors.New("postgres: analytics read refused: no buckets to cut the range into")
	}

	var (
		usage     persistence.Usage
		freshness persistence.Freshness
	)
	err := r.store.WithinTxAt(ctx, persistence.IsolationRepeatableRead, func(txCtx context.Context) error {
		// The bound is the first statement, and it is SET LOCAL so the bound
		// covers every read below including the planner's own work on the
		// first one — and so it leaves the connection when the transaction
		// does, rather than outliving it onto whatever borrows the connection
		// next.
		//
		// It comes AFTER the isolation, and that order is the point: the
		// isolation is on the BEGIN, which the store has already issued by the
		// time fn runs, so this statement is the read's first and its bound
		// covers everything after it.
		if _, err := r.store.Querier(txCtx).ExecContext(txCtx,
			`SET LOCAL statement_timeout = '`+analyticsReadBound+`'`); err != nil {
			return fmt.Errorf("postgres: set the analytics read bound: %w", err)
		}

		var err error
		usage, err = r.readSeries(txCtx, query)
		if err != nil {
			return err
		}
		money, err := r.readMoney(txCtx, query)
		if err != nil {
			return err
		}
		usage.Balances, err = r.readBalances(txCtx, query)
		if err != nil {
			return err
		}
		usage.Capture, err = r.readCapture(txCtx, query)
		if err != nil {
			return err
		}
		freshness, err = r.readFreshness(txCtx)
		if err != nil {
			return err
		}

		usage.SettledMinorUnits = money.Settled
		usage.ReleasedMinorUnits = money.Released
		usage.FundsAddedMinorUnits = money.FundsAdded
		return nil
	})
	if err != nil {
		return persistence.Usage{}, persistence.Freshness{}, err
	}
	return usage, freshness, nil
}

func (r *analyticsRepo) readSeries(ctx context.Context, query persistence.UsageQuery) (persistence.Usage, error) {
	values, args := bucketValues(query.Buckets)
	rows, err := r.store.Querier(ctx).QueryContext(ctx,
		fmt.Sprintf(bucketSelect, values), append([]any{query.AccountID}, args...)...)
	if err != nil {
		return persistence.Usage{}, r.classifyRead("read the request series", err)
	}
	defer func() { _ = rows.Close() }()

	// The scan takes the statement's FIVE columns and drops the first. The
	// ordinal numbers the buckets — it exists so the ORDER BY sorts integers
	// rather than timestamps, because two buckets of a series can share a
	// start when a zone's calendar does something unusual, and a row's
	// position in the answer is not its timestamp. The domain produced the
	// bounds and put them on persistence.Bucket already, so the row's own copy
	// of them is redundant; the scan reads it into a throwaway rather than
	// projecting it away in SQL, because a statement that stopped returning
	// the ordinal would then change shape silently and the scan would fail on
	// a row it used to read.
	series := make([]persistence.Bucket, 0, len(query.Buckets))
	for rows.Next() {
		var (
			ordinal int
			bucket  persistence.Bucket
		)
		if err := rows.Scan(&ordinal, &bucket.Start, &bucket.End, &bucket.WithUsageFacts, &bucket.Settled); err != nil {
			return persistence.Usage{}, fmt.Errorf("postgres: read the request series: scan: %w", err)
		}
		series = append(series, bucket)
	}
	if err := rows.Err(); err != nil {
		return persistence.Usage{}, r.classifyRead("read the request series", err)
	}
	return persistence.Usage{Series: series}, nil
}

// readMoney returns the three flow figures. Each is a separate statement
// because each is a different axis and a different set:
//
//   - settled: the SUM of settlement headers' settled_total, scoped to the
//     account through the fact dimensions so it is the account's spend and
//     not the platform's.
//   - released: the SUM of release legs' amount on the account's buckets. A
//     release is a different balance from a consume, so this is never netted
//     against settled — there is no arithmetic that turns the two into one
//     figure today, independent of any future refund.
//   - funds added: the SUM of grant and topup legs, the two crediting kinds
//     the ledger already has. This is capacity credited, not revenue: nothing
//     in this plane has ever received a payment.
//
// Every SUM carries a ::bigint cast. SUM over bigint widens to numeric in
// PostgreSQL, and without the cast the value arrives as an arbitrary-precision
// decimal that a careless scan would take as a float — the cast moves an
// overflow to a loud driver error instead of a plausible-looking wrong number.
func (r *analyticsRepo) readMoney(ctx context.Context, query persistence.UsageQuery) (money, error) {
	querier := r.store.Querier(ctx)
	from, to := query.From, query.To

	var settled int64
	settledRow := querier.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(s.settled_total), 0)::bigint
		FROM control.settlements s
		WHERE s.created_at >= $1 AND s.created_at < $2
		  AND EXISTS (
			SELECT 1 FROM control.analytics_fact_dimensions d
			WHERE d.request_id = s.request_id AND d.account_id = $3
		  )
	`, from, to, query.AccountID)
	if err := settledRow.Scan(&settled); err != nil {
		return money{}, r.classifyRead("read the settled amount", err)
	}

	var released, fundsAdded int64
	legRow := querier.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(l.amount) FILTER (WHERE l.kind = 'release'), 0)::bigint,
			COALESCE(SUM(l.amount) FILTER (WHERE l.kind IN ('grant', 'topup')), 0)::bigint
		FROM control.ledger_entries l
		WHERE l.created_at >= $1 AND l.created_at < $2
		  AND l.funding_bucket_id IN (
			SELECT b.id FROM (`+fmt.Sprintf(accountBucketsSelect, "$3")+`) b
			WHERE b.id IS NOT NULL
		  )
	`, from, to, query.AccountID)
	if err := legRow.Scan(&released, &fundsAdded); err != nil {
		return money{}, r.classifyRead("read the ledger flows", err)
	}

	return money{Settled: settled, Released: released, FundsAdded: fundsAdded}, nil
}

// money is the three flow figures, kept apart so the two families never share
// a struct, an accumulator or a statement.
type money struct {
	Settled    int64
	Released   int64
	FundsAdded int64
}

// readBalances returns the account's cached capacity.
//
// The account's buckets come from accountBucketsSelect — the same set readMoney
// resolves its legs against and for the same reason the money tables carry no
// account column: a read scoped through funding_buckets.account_id alone would
// silently drop every entitlement-funded bucket. The figures come from the
// projection's cached columns and are never re-derived from the legs: a
// re-derived balance would be a third figure beside the cache and the legs, and
// ADR 0011 has no check that could adjudicate between them.
func (r *analyticsRepo) readBalances(ctx context.Context, query persistence.UsageQuery) (persistence.Balances, error) {
	row := r.store.Querier(ctx).QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(SUM(b.held_amount), 0)::bigint,
		       COALESCE(SUM(b.available_amount), 0)::bigint
		FROM (`+accountBucketsSelect+`) b
		WHERE b.id IS NOT NULL
	`, "$1"), query.AccountID)

	var balances persistence.Balances
	if err := row.Scan(&balances.Held, &balances.Available); err != nil {
		return persistence.Balances{}, r.classifyRead("read the account balances", err)
	}
	return balances, nil
}

// readCapture splits the settlements in the range by how they captured their
// usage.
//
// The population is kind = 'settled', and that predicate is the whole
// correctness of the split rather than a refinement of it. A release and an
// expiry capture nothing — the schema's capture-shape CHECK refuses a capture
// method on one — and a fact claiming no usage while being disclaimed DOES
// carry a capture method while booking no charge, so a mix over facts would
// count a settlement that never priced anything. The three methods partition
// the settlements that captured: the CHECK guarantees no fourth method and no
// absence among the kinds that carry one, so the sum is the population any
// rate over this split divides by.
func (r *analyticsRepo) readCapture(ctx context.Context, query persistence.UsageQuery) (analytics.Capture, error) {
	row := r.store.Querier(ctx).QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE f.capture_method = 'reported')::bigint,
			COUNT(*) FILTER (WHERE f.capture_method = 'gateway_observed')::bigint,
			COUNT(*) FILTER (WHERE f.capture_method = 'reservation_floor')::bigint
		FROM control.applied_facts f
		WHERE f.kind = 'settled'
		  AND f.applied_at >= $1 AND f.applied_at < $2
		  AND EXISTS (
			SELECT 1 FROM control.analytics_fact_dimensions d
			WHERE d.request_id = f.request_id AND d.account_id = $3
		  )
	`, query.From, query.To, query.AccountID)

	var capture analytics.Capture
	if err := row.Scan(&capture.Reported, &capture.GatewayObserved, &capture.ReservationFloor); err != nil {
		return analytics.Capture{}, r.classifyRead("read the capture split", err)
	}
	return capture, nil
}

// readFreshness reads the ingestion cursor's own last-pass instant.
//
// The cursor's POSITION is never parsed and never compared to anything. It is
// an opaque string the Data Plane issued, the Control Plane is forbidden to
// decompose, and the only thing this read wants is when the pass that wrote it
// last completed — the cursor row's own updated_at, which is this plane's
// record time. What that instant does and does not prove is the contract's
// sentence, not this query's: it is a liveness signal, never a completeness
// one, because the feed's high-water mark is the Data Plane's to publish.
func (r *analyticsRepo) readFreshness(ctx context.Context) (persistence.Freshness, error) {
	// Scanned into a plain time.Time, NOT a sql.NullTime. The column is
	// `updated_at timestamptz NOT NULL DEFAULT now()` (migrations/control/
	// 000008:74), so a NULL instant is not a state this row can be in — a
	// missing freshness is a MISSING ROW, and that is what no-rows-below
	// reports. A NullTime here would have had a branch for a value the schema
	// cannot hold: the branch would have been unreachable, and a test written
	// to reach it would have had to UPDATE the column to NULL and been refused
	// by the very constraint the branch was defending. Which is the better
	// outcome: a plan that reads the column as what it is, and a database
	// that says so.
	//
	// A missing row is still a failure rather than a zero instant. Reporting
	// "never" as a time would be a caller drawing a chart from a statement
	// about nothing, and the cursor is a seeded singleton that 000001 inserts,
	// so its absence means the schema was not applied as it ships.
	var updatedAt time.Time
	row := r.store.Querier(ctx).QueryRowContext(ctx,
		`SELECT updated_at FROM control.ingestion_cursor WHERE id = 1`)
	switch err := row.Scan(&updatedAt); {
	case errors.Is(err, sql.ErrNoRows):
		return persistence.Freshness{}, errors.New("postgres: read the ingestion cursor: the singleton row is absent, and the schema seeds it in 000001")
	case err != nil:
		return persistence.Freshness{}, r.classifyRead("read the ingestion cursor", err)
	}
	if updatedAt.IsZero() {
		// Unreachable while the column is NOT NULL, and kept because a future
		// migration that relaxed the column would otherwise turn a missing
		// freshness into a plausible-looking epoch time — the year 1, which a
		// chart would draw as "no data, a very long time ago".
		return persistence.Freshness{}, errors.New("postgres: read the ingestion cursor: the singleton row carries no instant")
	}
	return persistence.Freshness{
		DataThrough: updatedAt.UTC(),
		Basis:       analytics.FreshnessFactFeedPass,
	}, nil
}

// classifyRead separates a read that ran out of time from a read that failed.
//
// The distinction is the difference between a retry-later refusal and a
// failure, and it is why this file does not fold every error into one: a
// caller told a slow report had failed would page an operator for a database
// that is answering.
func (r *analyticsRepo) classifyRead(what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgQueryCanceled, pgAdminShutdown:
			return fmt.Errorf("postgres: %s: %w", what, errAnalyticsReadExceeded)
		}
	}
	return fmt.Errorf("postgres: %s: %w", what, err)
}

// bucketValues renders the bucket list as a VALUES list with its arguments,
// numbering the buckets so the ORDER BY is a sort of integers rather than of
// timestamps — two buckets of a series can share a start when a zone's
// calendar does something unusual, and a row's position in the answer is not
// its timestamp.
func bucketValues(buckets []analytics.Bucket) (string, []any) {
	placeholders := make([]string, 0, len(buckets))
	args := make([]any, 0, len(buckets)*3)
	for i, bucket := range buckets {
		// The casts are deliberate and are the comment on bucketSelect's own: a
		// placeholder in a VALUES list has no column to infer its type from, and
		// the ordinal's type would otherwise depend on how many rows the list
		// has — which makes the one-bucket query the one that fails.
		placeholders = append(placeholders, fmt.Sprintf("($%d::int, $%d::timestamptz, $%d::timestamptz)", i*3+2, i*3+3, i*3+4))
		args = append(args, i, bucket.Start, bucket.End)
	}
	return strings.Join(placeholders, ", "), args
}

// uuidArray joins a set of identifiers into the one string parameter the
// account resolution reads, validating each as a uuid on the way.
//
// It refuses a malformed identifier rather than passing it through. A bad id
// would make the server-side ::uuid cast fail the whole statement, so this is
// not about avoiding an error — it is about the error being a NAMED one. The
// ids are runtime-authored text from a fact's allocation tail, and a caller
// that reached this with a blank or malformed one has a bug upstream that a
// PostgreSQL cast error would hide behind a generic SQL message.
func uuidArray(ids []string) (string, error) {
	for _, id := range ids {
		if !bucketIDForm.MatchString(id) {
			return "", fmt.Errorf("%w: funding bucket %q is not a version-7 uuid", errInvalidBucketReference, id)
		}
	}
	return strings.Join(ids, ","), nil
}
