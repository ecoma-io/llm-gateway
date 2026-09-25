package persistence

import (
	"context"
	"errors"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
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

// ExpiredLease is one hold the reaper closed, with everything the expired
// fact derives from: the request it held for and the pricing basis it held
// under. It is a read shape of the port, not a domain aggregate — the reaper
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
	// whose lease has lapsed against the store's own clock moves to expired,
	// up to limit, and the holds that moved come back whole so their facts can
	// be appended in the same unit of work — no completed close without its
	// fact. The precondition is carried whole — `state = 'open' AND
	// lease_expires_at < clock_timestamp()` — so a hold whose lease the
	// settlement path is extending this instant is never touched: the two
	// writers' predicates overlap on one row exactly once, and the row lock
	// decides. Contended rows are skipped, not waited on, so a reaper sweep
	// never stands behind a settlement.
	ExpireLapsedLeases(ctx context.Context, limit int) ([]ExpiredLease, error)
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

	// ApplyRefill adds capacity beyond the original grant, only to a
	// projection still sitting at the refill's guard revision; false means the
	// projection moved on and the refill must be re-derived, never re-applied
	// blindly — an unguarded refill replayed is capacity minted twice.
	ApplyRefill(ctx context.Context, refill accounting.Refill) (bool, error)

	// Drawdown takes `amount` of minor units from one account's projections,
	// walking them in ADR 0003's waterfall order and drawing each bucket
	// conditionally — the update carries `available >= take`, so exactly one
	// contender wins each unit under concurrency. The legs it actually drew
	// come back in waterfall order; they are the reservation's allocations,
	// built from what the store granted, never from what the caller hoped.
	// ErrInsufficientCapacity means the whole order fell short, with nothing
	// drawn — the walk gives back what it took before saying so.
	Drawdown(ctx context.Context, accountID string, amount int64) ([]accounting.Allocation, error)

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
	// allocated for it. The sequence is allocated from the stream's single row
	// by an upsert whose lock is held to the transaction's commit, so
	// allocation order equals visibility order — and Append must therefore be
	// the LAST statement of the unit of work that calls it: it serialises the
	// tail of every settlement, and moving it earlier serialises more of the
	// unit for no gain. A fact written outside a unit of work still appends,
	// alone; the sequence is no less real for that.
	//
	// A second settlement-relevant fact for one request fails with
	// ErrDuplicateFact — the dedup partial unique is the final idempotency
	// guard, and it exists so a duplicated close can never poison a feed page
	// that can then never be applied.
	Append(ctx context.Context, fact accounting.Fact) (int64, error)
}
