package persistence

import (
	"context"
	"errors"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// InsufficientCapacityError is a Drawdown shortfall, carried typed so the
// caller can read the one fact the bare sentinel cannot tell it: whether the
// walk saw any grant eligible to fund the request at all. The bit is set
// inside the walk itself — the walk's statement returns exactly the buckets
// eligible to fund this request — so reading it costs no second query and
// cannot drift from the predicate that decided it.
//
// A shortfall with an eligible row seen is a shortage: the account owns
// funding for this alias and it did not cover the hold. A shortfall without
// one is a scope answer: no grant's stored scope contained the alias, or every
// grant whose scope did had already ended its cycle — a treat-as-zero, not a
// ran-out. The two are different answers to a client, and re-deriving the
// difference outside this error would mean re-running the eligibility
// predicate the walk already evaluated.
type InsufficientCapacityError struct {
	// EligibleRowSeen reports whether the walk saw at least one grant
	// eligible to fund this request — its scope containing the alias and its
	// cycle still live — whether or not any of them had the capacity.
	EligibleRowSeen bool
}

// Error is one sentence for the log. The two shapes read differently because
// they are different facts about the account; neither names the account, the
// alias or the amount, which are the caller's sentence to write.
func (e *InsufficientCapacityError) Error() string {
	if e.EligibleRowSeen {
		return "persistence: the account's eligible grants could not cover the hold"
	}
	return "persistence: no grant of the account is eligible to fund this request"
}

// Is reports membership of the capacity sentinel: every caller that matched
// the bare ErrInsufficientCapacity keeps matching the typed error, which is
// what lets the port's existing tests and callers read unchanged.
func (e *InsufficientCapacityError) Is(target error) bool {
	return target == accounting.ErrInsufficientCapacity
}

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

// ErrExpireOutsideUnitOfWork says ExpireLapsedLeases arrived with no unit of
// work in its context. The sweep's closes are the reaper's half of the
// ending whose fact, request finalisation and replay pointer are the other
// half — a sweep run bare commits each close the instant it happens
// (autocommit), and a crash before the caller's unit opens would leave
// closed holds the feed never hears about, the exact tear "no completed
// close without its fact" exists to prevent. The refusal is the same
// posture Append's is: the pairing is a property of the caller's unit of
// work, and the store refuses to pretend otherwise.
var ErrExpireOutsideUnitOfWork = errors.New("persistence: a lapsed-lease sweep needs a unit of work")

// ExpiredLease is one hold the reaper closed, with everything the expired
// fact derives from: the request it held for, the pricing basis it held
// under, and the legs its hold was split into — the fact's allocation tail,
// read in the same sweep that closed the hold, so the append needs no second
// read. It also carries the request's replay identity — the account and
// idempotency key the intake record was born under — because the hold is not
// the only thing an expired close settles: the request row finalises (the
// domain's FailAbandoned, through RequestRepository.Finalise) and the replay
// record's pointer is written (IntakeRepository.Finalise, keyed exactly as
// the release path keys it). The reaper has no ChatInput to carry those keys
// in memory, so the sweep reads them where it reads the legs — one read, one
// unit of work, no second lookup. It is a read shape of the port, not a
// domain aggregate — the reaper reads what it closed and settles from what it
// read.
//
// The legs are for the return as much as for the fact. An expired close is
// the release's twin — one composition, two doors (data-implications.md) —
// so the ending returns the hold's capacity through
// QuotaProjectionRepository.Return in the same unit of work, in the legs'
// stored ordinal order, exactly as a release does, and the unconditional
// giveback belongs to the unit whose CAS-class close won the hold. The
// capacity return and the fact append are both last-statement work of the
// one unit: a close that committed without either would be a hold the
// runtime ended that no feed page reports and no bucket ever gets back.
type ExpiredLease struct {
	ID              identity.ReservationID
	RequestID       identity.RequestID
	PriceRevision   string
	InputUnitPrice  int64
	OutputUnitPrice int64
	InputTokens     int
	MaxOutputTokens int
	LeaseOwner      string
	AccountID       string
	IdempotencyKey  string
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
	// back whole — legs and replay identity included — so their facts can be
	// appended and their requests and replay records finalised in the same
	// unit of work: no completed close without its fact, and no closed hold
	// whose request row stays executing forever. The sweep must arrive
	// inside a unit of work the caller opened — a call whose context carries
	// none fails with ErrExpireOutsideUnitOfWork, because a bare sweep's
	// closes would commit one by one under autocommit, and a crash before
	// the caller's unit opened would leave closed holds the feed never hears
	// about. Both clocks must
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
	//
	// The sweep fails whole, never in halves: a victim whose settlement tail
	// the store cannot read back whole — a request id that is not an
	// identity, a request row that does not exist, a replay record that does
	// not exist or does not belong to the request's account — errors the
	// sweep, and the unit it ran in rolls back with it. Victims come back
	// oldest-lease-first, so one such row is by construction the first victim
	// every later sweep picks: it stops all reaping until it is repaired.
	// That wedge is the price of never settling a close in halves; a loop
	// consuming this port treats the error as a page to an operator, not a
	// reason to retry.
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
	// drawn — the walk gives back what it took before saying so. The shortfall
	// arrives as an InsufficientCapacityError (which matches the sentinel, so
	// errors.Is reads either): its EligibleRowSeen records whether the walk saw
	// any grant eligible to fund this request, which is the difference between
	// a shortage and a scope answer, read off the walk that already ran. A
	// grant that is ineligible (or lost its capacity to a contender) is not an
	// error of its own: the walk simply passes it by, and only a final
	// shortfall surfaces.
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
	//
	// The credit is bucket-keyed on purpose, and the key is carried by the
	// leg itself: capacity goes back to the exact ceiling row it was drawn
	// from, never to an account-wide pool, so a reassignment of the account
	// between drawdown and return — an entitlement cycle rolling over, a PAYG
	// balance superseded by a new one — leaves the lapsed ceiling holding a
	// credit it can no longer spend rather than topping the replacement
	// beyond what was ever taken from it. A caller that wanted the old
	// bucket's tail moved to the new one states that as its own explicit
	// leg-shaped decision; this method does not infer it.
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
