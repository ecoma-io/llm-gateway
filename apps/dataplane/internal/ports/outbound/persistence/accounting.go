package persistence

import (
	"context"
	"errors"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// ErrDuplicateReservation says a hold already exists for the request — the
// reservation table's UNIQUE(request_id), the engine half of "one reservation
// per request" (ADR 0004 invariant 6). Like the intake's duplicate it is an
// answer rather than a malfunction: the request already holds capacity, and
// the caller reads that hold instead of opening a second one.
var ErrDuplicateReservation = errors.New("persistence: the request already holds a reservation")

// ErrDuplicateFact says a settlement-relevant fact already exists for the
// request — the dedup partial unique refusing a second close. The settlement
// unit of work closes the reservation and appends its fact atomically, so a
// caller that reaches this sentinel has raced another closer past its own CAS
// or is looking at a bug; either way the winner's fact stands and this caller
// writes nothing.
var ErrDuplicateFact = errors.New("persistence: the request already has its settlement fact")

// ErrAppendOutsideUnitOfWork says Append arrived with no unit of work in its
// context. The append sequence must be allocated inside the unit that writes
// the fact — a sequence allocated outside one could commit apart from the
// fact it numbers, which is the tearing the commit-order guarantee exists to
// make impossible — so the store refuses the call instead of appending a lone
// fact whose position the caller believes is atomic with something.
var ErrAppendOutsideUnitOfWork = errors.New("persistence: a fact append needs a unit of work")

// ExpiredLease is one hold the reaper closed, with everything the expired
// fact derives from: the request it held for, the pricing basis it held
// under, and the legs its hold was split into — the fact's allocation tail,
// read in the same sweep that closed the hold, so the append needs no second
// read. It is a read shape of the port, not a domain aggregate — the reaper
// reads what it closed and appends from what it read, in one unit of work.
type ExpiredLease struct {
	ID              identity.ReservationID
	RequestID       identity.RequestID
	PriceRevision   string
	InputUnitPrice  int64
	OutputUnitPrice int64
	InputTokens     int
	MaxOutputTokens int
	LeaseOwner      string
	Allocations     []accounting.Allocation
}

// ReservationRepository persists holds and closes them exactly once.
//
// As with every repository on this port, each method resolves its query
// surface from ctx (Querier), so a call inside a WithinTx unit of work joins
// it — which is what makes the settlement atomic: the close and the fact
// append are two calls, one unit of work, and the append is that unit's last
// statement, because the stream's row lock serialises only the tail of each
// settlement (migration 000002, "FACT ORDERING IS COMMIT ORDERING").
type ReservationRepository interface {
	// Insert writes the hold and its waterfall legs — formed together, because
	// a hold without its split cannot be settled, and split across two calls
	// would be a hold a crash could orphan. A duplicate hold for the request
	// fails with ErrDuplicateReservation.
	Insert(ctx context.Context, reservation accounting.Reservation) error

	// Close moves an open hold to the named terminal state — settled,
	// released, or expired — and stamps the close time. The UPDATE carries
	// `state = 'open'` in its WHERE clause; false means someone else closed
	// the hold first, and the loser's fact must not be appended.
	Close(ctx context.Context, id identity.ReservationID, state accounting.State, closedAt time.Time) (bool, error)

	// ExpireLapsedLeases is the reaper's batch CAS: every hold still open
	// whose hold window AND whose lease have both lapsed against the store's
	// own clock moves to expired, up to limit, and the holds that moved come
	// back whole — legs included — so their facts can be appended in the same
	// unit of work: no completed close without its fact. Both clocks must
	// have passed because they guard different disasters: a lapsed window is
	// the caller walking away, a lapsed lease is the owner dying, and taking
	// a hold on only one of them would close out a request that is still
	// executing (live window, hiccuping renewal) or hold capacity for a
	// caller that already left (live lease, lapsed window) until the lease
	// too runs out. A hold whose lease the settlement path is extending this
	// instant is never touched: the row lock decides between the two writers'
	// predicates. Contended rows are skipped, not waited on, so a reaper
	// sweep never stands behind a settlement. A limit below one is refused —
	// an unbounded sweep is how a backlog becomes a long transaction.
	ExpireLapsedLeases(ctx context.Context, limit int) ([]ExpiredLease, error)

	// RenewLease extends one open hold's lease to the caller's new expiry,
	// naming the owner that holds it. True means the lease moved; false means
	// the hold is gone from the open set — closed by a concurrent settlement,
	// or swept — and the renewing process must stop settling on its behalf.
	// The lease is what tells the reaper "this process is alive and this hold
	// is not abandoned"; a renewal for a hold whose window has already lapsed
	// still succeeds if the hold is open, because the window and the lease
	// are different clocks with different owners, and the settlement path —
	// not the reaper — decides when the window matters.
	RenewLease(ctx context.Context, id identity.ReservationID, owner string, leaseExpiresAt time.Time) (bool, error)
}

// QuotaProjectionRepository is the runtime's lock over its copy of grants:
// publications in, drawdowns and returns out, every number on the
// runtime-owned side of the algebra moving through a conditional update.
type QuotaProjectionRepository interface {
	// ApplyPublication applies the Control Plane's word about one grant. The
	// revision decides: a row seeded when none existed, the Control-Plane-
	// owned fields updated on an older row, and a stale redelivery answered
	// with PublicationStale and no write — available is never touched by a
	// publication, because spent capacity must never be resurrected.
	ApplyPublication(ctx context.Context, publication accounting.Publication) (accounting.PublicationOutcome, error)

	// ApplyRefill adds capacity beyond the original grant. The refill's id is
	// the idempotency key and the store records every id once: the first
	// sighting at a live guard revision answers RefillApplied with the
	// projection's available raised by the amount; a redelivery of the same
	// id answers RefillAlreadyApplied and moves nothing; a refill whose
	// projection has moved past its guard revision answers RefillStale — the
	// identity is recorded unapplied, and the increase must be re-derived
	// under a new one. The stale asymmetry is the guard doing its job: the
	// alternative is raising capacity against a grant state the Control Plane
	// has already superseded.
	ApplyRefill(ctx context.Context, refill accounting.Refill) (accounting.RefillOutcome, error)

	// Drawdown takes `amount` of minor units from one account's projections
	// on behalf of `aliasID`, walking them in ADR 0003's waterfall order and
	// drawing each bucket conditionally — the update carries `available >=
	// take`, so exactly one contender wins each unit under concurrency. Only
	// grants that may fund this request are eligible: the grant's stored scope
	// must contain the alias (the wildcard `*` version contains every alias; a
	// named version only the aliases its snapshot lists) and the grant's cycle
	// must not have ended at the transaction's own instant. The take
	// re-asserts the identical eligibility predicate under the row lock, so a
	// grant whose eligibility moved between the walk and the take is never
	// drawn on a stale read's word. The legs it actually drew come back in
	// waterfall order; they are the reservation's allocations, built from what
	// the store granted, never from what the caller hoped.
	// ErrInsufficientCapacity means the whole order fell short, with nothing
	// drawn — the walk gives back what it took before saying so. A grant that
	// is ineligible (or lost its capacity to a contender) is not an error of
	// its own: the walk simply passes it by, and only a final shortfall
	// surfaces.
	//
	// The walk, the takes and the giveback run through the Querier ctx
	// resolves, so they are one atomic unit of work exactly when ctx carries
	// one — admission's call belongs inside a WithinTx, and the concurrency
	// guarantee above is a unit-of-work guarantee: a Drawdown spread over
	// pool connections is statements without a snapshot, and its giveback is
	// three writes where a rollback would have been one. The store's doctrine
	// is that the caller owns the boundary; this is the method where forgetting
	// it would look like it worked.
	Drawdown(ctx context.Context, accountID string, aliasID catalog.AliasID, amount int64) ([]accounting.Allocation, error)

	// Return puts drawn-down capacity back, one leg at a time, each update
	// unconditional on the balance: a publication that shrank the ceiling
	// below outstanding capacity must not turn a return into a failure. A leg
	// whose bucket has no row yet is a publication lag, not a loss to invent
	// against — it is skipped, and the count of legs that landed comes back so
	// the caller can decide what a lagging grant is worth complaining about.
	Return(ctx context.Context, legs []accounting.Allocation) (int, error)
}

// FactRepository appends facts to the runtime's durable feed.
type FactRepository interface {
	// Append writes one fact and returns the append sequence the store
	// allocated for it. The call must arrive inside a unit of work — an
	// append with none in its context fails with ErrAppendOutsideUnitOfWork,
	// because a sequence allocated outside one could commit apart from the
	// fact it numbers, and the feed's whole ordering discipline exists to
	// make that impossible.
	//
	// Within its unit, the sequence is allocated from the stream's single row
	// by an upsert whose lock is held to the transaction's commit, so
	// allocation order equals visibility order — and Append should therefore
	// be the LAST statement of the unit that calls it: it serialises the tail
	// of every settlement, and moving it earlier serialises more of the unit
	// for no gain. Aborted units roll the counter back with everything else.
	//
	// A second settlement-relevant fact for one request fails with
	// ErrDuplicateFact — the dedup partial unique is the final idempotency
	// guard, and it exists so a duplicated close can never poison a feed page
	// that can then never be applied.
	Append(ctx context.Context, fact accounting.Fact) (int64, error)
}
