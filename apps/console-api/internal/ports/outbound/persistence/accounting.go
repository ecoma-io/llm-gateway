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
}

// FundingProjections reads what the ledger alone derives — the input
// Reconcile compares a bucket's cached projection against.
type FundingProjections interface {
	// DerivedBalances recomputes a bucket's settled, held and available
	// balances and its leg count from its legs alone, in one aggregate
	// query. The cached columns are the projection; this is the truth they
	// must match, and when they do not, this is what a correction is
	// computed from.
	DerivedBalances(ctx context.Context, bucketID accounting.FundingBucketID) (accounting.Derivation, error)
}
