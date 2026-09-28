package persistence

import (
	"context"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
)

// The accounting repositories, in the store's vocabulary.
//
// Accounting is the Control Plane's money authority (ADR 0004): funding
// buckets, the append-only ledger and its six leg kinds, and the settlements
// of record keyed by Data Plane request. Every table it names lives in the
// `control` database, alongside commerce's — the two share the plane and its
// transaction boundary, which is what lets a cycle roll materialise an
// entitlement and fund its bucket as one fact, and what keeps the ledger's
// word final without any cross-plane transaction existing.
//
// Five rules the signatures below carry on purpose, in continuation of the
// commerce port's:
//
//   - The bucket move and the leg are one fact, always. Append is the only
//     door: it runs the bucket's guarded echo (the balance move, the
//     sequence allocation, the version bump) and the leg's insert inside the
//     caller's unit of work, so a bucket whose cache moved without a leg
//     naming it cannot exist through this port. There is no Update-balance
//     member and there must never be one.
//   - The statement is the guard and the guard is in the WHERE clause. Every
//     Append re-states the leg's balance predicate on the bucket row —
//     available for a hold, held for a release, held and settled for a
//     consume, the non-negative results of an adjustment — so under READ
//     COMMITTED the row lock serialises concurrent legs and each statement
//     re-evaluates its predicate against the winner's committed values. Two
//     holds racing one bucket's last 80 of 100 available do not both win;
//     the loser's UPDATE fires zero rows and arrives above as the domain's
//     insufficient sentinel.
//   - Sequence is allocated, never guessed. The bucket row owns
//     last_sequence, the echo bumps it, and the leg is inserted with the
//     RETURNING value — a bucket's ledger order is that counter's order,
//     never id order and never timestamp order.
//   - Idempotent commands converge or refuse, inside the unit of work. The
//     adapter brackets Append's two statements in a savepoint, so a command
//     key or a reservation movement that collides with an already-committed
//     leg surfaces as the domain's duplicate sentinel with the unit of work
//     still alive — the caller re-reads the original leg, compares payloads,
//     and either converges on it or names the conflict. A dead transaction
//     would make every retry a rollback, which is the one thing an
//     idempotency key must never cost.
//   - The ledger takes no corrections in place. There is no update and no
//     delete for legs or settlements on purpose: a mistake is a new
//     adjustment leg naming the entry it corrects, and the schema's
//     append-only guards make the refusal structural rather than
//     conventional.

// FundingBuckets persists the funding bucket aggregate — the authoritative
// capacity projection for one entitlement cycle or one account PAYG balance.
type FundingBuckets interface {
	// Create inserts a bucket in its birth state (active, zero balances,
	// zero version). The owner-xor and balance-projection CHECKs re-state
	// what the domain constructors enforce; a unique violation on the
	// owner columns means the entitlement cycle or account already has its
	// bucket, which the calling use case surfaces in its own words.
	Create(ctx context.Context, bucket accounting.Bucket) error

	// ByID returns the bucket with id, or ErrNotFound.
	ByID(ctx context.Context, id accounting.FundingBucketID) (accounting.Bucket, error)

	// ByEntitlementID returns the cycle bucket funding the entitlement, or
	// ErrNotFound. The unique index on entitlement_id makes this a lookup,
	// never a scan.
	ByEntitlementID(ctx context.Context, id accounting.EntitlementID) (accounting.Bucket, error)

	// ByAccountID returns the PAYG bucket funding the account, or
	// ErrNotFound. Same uniqueness, same shape.
	ByAccountID(ctx context.Context, id accounting.AccountID) (accounting.Bucket, error)

	// Close applies the administrative close, compare-and-swapped: the row
	// becomes closed only while it still shows the version the caller
	// read, is still active, and still holds nothing. False means the
	// world moved — a leg landed, another closer won, the bucket is gone —
	// and the caller re-reads; the close is refused above on held funds
	// before this is ever reached, and the statement repeats the held-zero
	// gate because a leg can land between the read and the write.
	Close(ctx context.Context, id accounting.FundingBucketID, fromVersion int64, updatedAt time.Time) (bool, error)

	// Sweep returns at most limit buckets whose id is strictly greater than
	// after, in ascending id order — the bounded, keyset-paginated walk a
	// reconciliation pass (B13) makes. It is the ONLY enumeration member on
	// this port and it was absent until a caller needed it, which is the port
	// package's own rule stated as history: a member is added when a consumer
	// asks for it, not before.
	//
	// Keyset pagination, never OFFSET, and the reason is the whole reason a
	// sweep over a growing table is safe at all: OFFSET re-reads and re-discards
	// every row already passed, so its cost grows with the table, and a pass
	// that pages by OFFSET while legs land concurrently can both skip a row
	// (a bucket inserted before the current offset shifts it forward) and read
	// one twice. An exclusive lower bound on the id has neither failure: a
	// bucket that appears mid-sweep sorts after the cursor or before it, and
	// either way the next pass's sweep of the same keyspace reaches it. Ids
	// are uuid v7, so id order is very nearly mint order and the walk is close
	// to sequential; correctness does not depend on that at all.
	//
	// Fewer than limit rows means the table holds nothing further past the
	// cursor — the ordinary end of the sweep, and never an error. A caller
	// treating a short page as "done" is right: a row that appears after this
	// read sorts at or after the cursor and the next pass's sweep finds it.
	//
	// It is a read, and it deliberately stays one: there is still no
	// update-balance member on this port and there must never be one.
	Sweep(ctx context.Context, after accounting.FundingBucketID, limit int) ([]accounting.Bucket, error)
}

// AccountBuckets is the console's read of where the account's money is, and
// it is a separate interface from FundingBuckets for the same reason
// AccountUsers stands beside Users: the ownership file is being extended for
// the session surface in parallel with this one, and two agents editing one
// interface is a merge a reviewer has to unpick. The rows are the same rows
// ByID returns, read through the same columns.
//
// The three balances it returns are the CACHED columns the database already
// maintains — settled, held, and available, which funding_buckets_balance_projection
// pins to available = settled − held at write time. They come back as
// stored. There is deliberately no member here that sums legs, and one must
// never be added: a Σ amount across grant/topup/hold/release/consume/
// adjustment is not a number any of those kinds means, and a balance
// derived from a page of the ledger is a balance derived from a page.
type AccountBuckets interface {
	// ListForAccount returns at most page.Limit of the account's buckets —
	// its entitlement cycles and its PAYG balance alike — keyed on id, with
	// the account predicate in the WHERE clause.
	//
	// The predicate is on funding_buckets.account_id, which is the column
	// that names an owner directly. An entitlement bucket's owning account
	// is present in that column too, so one predicate covers both kinds and
	// no join to entitlements is needed to evaluate it.
	ListForAccount(ctx context.Context, accountID accounting.AccountID, page FundingBucketPage) ([]accounting.Bucket, error)
}

// FundingBucketPage is the buckets list's request.
type FundingBucketPage struct {
	After accounting.FundingBucketID
	Limit int
}

// FundingLedger appends legs and reads a bucket's history. Every write goes
// through Append; every read is a read.
type FundingLedger interface {
	// Append lands one leg: the bucket's guarded echo and the leg's insert,
	// in that order, inside the caller's unit of work, bracketed in a
	// savepoint per the package rules above. It returns the leg stamped
	// with its allocated sequence and the bucket row as the echo left it —
	// the post-move truth, not a re-read.
	//
	// The contract is unit-of-work-shaped and refuses rather than degrades:
	// called outside a unit of work it returns an error without touching
	// the store, because an autocommitted echo would break the leg-and-bucket
	// atomicity every invariant in ADR 0004 rests on.
	//
	// A balance guard that fires zero rows is classified by a fresh read of
	// the bucket (missing, closed, or insufficient for the leg's kind) and
	// arrives above as that domain sentinel. A colliding command key or
	// reservation movement arrives as ErrDuplicateCommand or
	// ErrDuplicateMovement — whether the collision converges on the
	// original leg or names a contract defect is the calling use case's
	// comparison to make, and it re-reads through this port to do it.
	Append(ctx context.Context, entry accounting.LedgerEntry) (accounting.LedgerEntry, accounting.Bucket, error)

	// ByBucketAndCommandKey returns the leg a topup or keyed adjustment
	// command landed as, or ErrNotFound — the convergence read a retried
	// command compares its payload against.
	ByBucketAndCommandKey(ctx context.Context, bucketID accounting.FundingBucketID, commandKey accounting.CommandKey) (accounting.LedgerEntry, error)

	// ByBucketReservationAndKind returns the leg a reservation's hold or
	// release booked on the bucket, or ErrNotFound — the convergence read a
	// redelivered movement compares its amount against.
	ByBucketReservationAndKind(ctx context.Context, bucketID accounting.FundingBucketID, reservationID accounting.ReservationID, kind accounting.Kind) (accounting.LedgerEntry, error)
}

// AccountLedger is the console's read of ONE bucket's history, and the
// per-bucket shape is the feature rather than a limitation of this
// implementation.
//
// The keyset is (funding_bucket_id, sequence) and the schema's
// ledger_entries_bucket_sequence_key is exactly that pair, so a page is a
// direct range scan on an index that already exists — no migration, no new
// index, and on a table this lane builds with plain CREATE INDEX rather than
// CONCURRENTLY. A cross-bucket timeline keyed on created_at instead would
// need an index that does not exist, built non-concurrently on the fastest-
// growing table in the plane, which is a multi-hour exclusive lock bought to
// answer a question a ledger should never be asked as one whole: a ledger is
// a fact about where money went for ONE owner of that money.
//
// Kind is an optional filter because operators read a ledger for two
// different questions — what came in, and what went out — and the two are not
// the same read.
type AccountLedger interface {
	// ListForBucket returns at most page.Limit of the bucket's legs, oldest
	// first, keyed on the bucket's own allocated sequence.
	//
	// The account predicate is in the WHERE clause and is the first argument,
	// evaluated against the bucket's account_id rather than through a join to
	// funding_buckets — the ledger table carries no account column, and the
	// account that owns a bucket is the account the bucket names. A bucket
	// belonging to another account is therefore a bucket the statement's
	// first predicate does not match, and the answer is an empty page at the
	// cost of an empty page: a 404 the query did not return, and never a 403.
	//
	// It returns legs, not a balance. A Σ over the returned deltas is not
	// one of the three balances the bucket caches, and this member must never
	// grow a sibling that computes one from a page.
	ListForBucket(ctx context.Context, accountID accounting.AccountID, bucketID accounting.FundingBucketID, page LedgerPage) ([]accounting.LedgerEntry, error)
}

// LedgerPage is the ledger list's request: an optional leg-kind filter, the
// exclusive lower bound on the sequence, and a page size.
type LedgerPage struct {
	// Kind is one leg kind, or empty for the whole history.
	Kind string

	// AfterSequence is the exclusive lower bound on the leg's sequence. The
	// zero value is the beginning of the bucket's history, not a cursor that
	// matches nothing — a bucket's first leg is sequence 1.
	AfterSequence int64

	// Limit is the number of rows wanted; the adapter asks for one more.
	Limit int
}

// Settlements persists the settlement headers — the exactly-once edge for a
// Data Plane request's financial closure.
type Settlements interface {
	// Create inserts a settlement header keyed by its request id and
	// reports whether this call created it. A re-acknowledgement of an
	// already-settled request inserts nothing (the request_id unique index
	// absorbs it) and returns false: the caller reads the recorded
	// settlement through ByRequestID, compares totals, and converges or
	// names the conflict. The legs of the plan a true return belongs to are
	// appended in the same unit of work — a header without its legs cannot
	// commit, and a plan whose legs lose a guard leaves no header behind.
	Create(ctx context.Context, settlement accounting.Settlement) (bool, error)

	// ByRequestID returns the settlement recorded for the request, or
	// ErrNotFound.
	ByRequestID(ctx context.Context, requestID accounting.RequestID) (accounting.Settlement, error)

	// Recent is deliberately absent, and the absence is a decision rather than
	// an oversight. A draft of the reconciliation pass added a windowed read
	// over settlements here, and no check ended up calling it: F2 and F3 are
	// reached through Ledger by settlement id, because a fact is the thing
	// that names a settlement and the pass already holds the fact. A read half
	// with no reader is a promise the composition root makes and nothing keeps,
	// and the index it would have wanted is not in the migration — which is
	// the shape of a thing nobody asked for. When a check needs to sweep
	// settlements by time, it comes back with the index and the caller that
	// wants it.
	//
	// Ledger returns the settlement with settlementID beside what its own legs
	// say about it: the consume-leg sum, the leg count, the per-kind multiset
	// and the two bucket counts. ErrNotFound when the settlement is not on
	// file, which is a real answer a caller must be able to distinguish from
	// "it is on file and carries no legs" — the zero-priced settle is the
	// second case and is entirely legitimate.
	//
	// It sits here and not on FundingProjections because it is the settlement
	// header's own read of its own history, and the header is this port's
	// subject: a caller that has a settlement and wants to know what its legs
	// say about it should not have to reach for the bucket projection to
	// find out. (The aggregation itself lives in the same store either way;
	// this is about which question the method is named after.)
	Ledger(ctx context.Context, settlementID accounting.SettlementID) (SettlementLedger, error)
}

// SettlementLedger is what one settlement's own legs say about it: the sum of
// its consume legs, how many legs it carries, and the per-kind multiset.
//
// It exists because a settlement's header and its legs are one fact written in
// one unit of work, and the one question a header alone cannot answer is
// whether its legs agree with it. It is deliberately a projection of the legs
// and not a verdict: comparing ConsumeSum against the header's SettledTotal
// is the caller's comparison, and a mismatch is a divergence to record rather
// than a number to reconcile — this port moves no money and holds no opinion
// about which of the two is right.
//
// The multiset is here, and not just the sum, because a PARTIAL leg write can
// still balance. A consume of +50 against a release of -50 sums to the cached
// zero, so an existence-only anti-join would report "this settlement has an
// accounting effect" while the settlement in fact carries one consume leg
// where the derivation said two, or a release leg no tail justified. A sum
// cannot see that; a count per kind can, and the comparison against the
// shape the terminal fact implies is what turns it into a finding.
type SettlementLedger struct {
	// Settlement is the header the legs are read beside, so a caller never
	// has to re-read it to compare the two.
	Settlement accounting.Settlement

	// ConsumeSum is the sum of the settlement's consume leg amounts — the
	// figure the header's SettledTotal is defined to equal (BuildSettle
	// computes the total from the legs it wrote).
	ConsumeSum int64

	// Legs is the total leg count, and LegsByKind the per-kind multiset. A
	// settlement with no legs is a legitimate shape — the zero-priced settle
	// books a header of record and nothing else — so the empty multiset is an
	// answer this port returns rather than a miss.
	Legs       int64
	LegsByKind map[string]int64

	// Buckets is the number of distinct funding buckets the settlement's legs
	// name.
	//
	// BucketsWithoutRelease is the number of those that carry a consume and no
	// release beside it, and it is the OBSERVATION and not the cause: a consume
	// with no release beside it has two explanations the legs cannot tell
	// apart — the consume genuinely took the whole hold, or the release leg was
	// never written — and a port that named it after the first would be
	// asserting a cause its reader cannot see.
	//
	// It is no longer read by any check, and the reason is worth recording
	// rather than leaving as an unused field. An earlier shape rule compared
	// consume-count minus release-count against it, on the model that
	// BuildSettle writes one of each leg per bucket; it does not. The two tests
	// are independent — a consume iff the spend was positive, a release iff the
	// tail was — so a bucket the spend never reached carries a release alone,
	// and the equality fired on most ordinary multi-bucket settlements. The
	// count is kept because it is one extra column of an aggregate this read
	// already computes, and because a rule that wants it again should not have
	// to add a query to find out. A shape rule must not use it to assert a
	// consume/release pairing, which is the mistake it was last used for.
	Buckets               int64
	BucketsWithoutRelease int64
}

// FundingProjections reads what the ledger alone derives — the input
// ReconcileBucket compares a bucket's cached projection against.
type FundingProjections interface {
	// DerivedBalances recomputes a bucket's settled, held and available
	// balances and its leg count from its legs alone, in one aggregate
	// query. The cached columns are the projection; this is the truth they
	// must match, and when they do not, this is what a correction is
	// computed from.
	DerivedBalances(ctx context.Context, bucketID accounting.FundingBucketID) (accounting.Derivation, error)
}
