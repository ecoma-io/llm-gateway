package accounting

import (
	"errors"
	"sort"
	"time"
)

// ScopeKind says which kind of grant a projection locks: a named entitlement's
// billing cycle, or a pay-as-you-go balance. The two share one row shape and
// one drawdown verb because the waterfall treats them identically — the scope
// fields are the only thing that differs, and they ride in the row.
type ScopeKind string

const (
	ScopeEntitlementCycle ScopeKind = "entitlement_cycle"
	ScopePayGBalance      ScopeKind = "payg_balance"
)

// ProjectionState says whether the grant behind a projection is still live.
// An inactive projection is capacity the runtime can no longer offer; its row
// stays, because outstanding draws against it still need their return to land
// somewhere.
type ProjectionState string

const (
	ProjectionActive   ProjectionState = "active"
	ProjectionInactive ProjectionState = "inactive"
)

// Dimension is the one measure a projection can lock. The column exists so
// the schema says "cost" out loud rather than by omission — ADR 0003's
// waterfall is a cost waterfall, and a second dimension one day is a second
// decision, not a new value here.
const Dimension = "cost"

// ErrUnknownScope marks a projection whose scope kind and scope fields
// disagree — an entitlement cycle without its cycle number, a PAYG balance
// carrying one. The quota table's two mirror CHECKs refuse the same rows.
var ErrUnknownScope = errors.New("accounting: projection scope kind and scope fields disagree")

// ErrInsufficientCapacity is the waterfall's verdict: the account's live
// projections cannot cover the hold, in this order, from this much. It is a
// verdict, not a malfunction — admission answers it with
// insufficient_entitlement — and it is returned instead of a partial hold
// because a hold for less than the request asked is a claim on money the
// request will spend without covering.
var ErrInsufficientCapacity = errors.New("accounting: no waterfall order covers the hold")

// Projection is the runtime's lockable copy of one grant (ADR 0006): one row
// per funding bucket, because the funding bucket id is the only key an
// allocation leg carries, so drawdown and return need no other join.
//
// Available is this row's one runtime-owned number. Everything else —
// identity, limit, state — is the Control Plane's, updated only through
// publications; available moves only through drawdowns and returns this
// runtime itself performed. Available carries a floor but no ceiling: a
// publication that shrinks the grant below outstanding capacity must not turn
// every later capacity return into a failed release.
type Projection struct {
	AccountID           string
	ScopeKind           ScopeKind
	EntitlementID       string
	CycleNumber         int
	FundingBucketID     string
	AliasGroupVersionID string
	// NamedScope records the seed-time specificity: the grant was made under a
	// named alias group (true) or under the singleton `*` (false). Stored
	// rather than derived so that the waterfall needs nothing but this table.
	NamedScope            bool
	PeriodEnd             time.Time
	SubscriptionCreatedAt time.Time
	State                 ProjectionState
	LimitAmount           int64
	Available             int64
	Revision              int64
	SeededAt              time.Time
	UpdatedAt             time.Time
}

// Validate refuses a projection whose scope kind and scope fields disagree.
// The entitlement scope names its entitlement, its cycle and the cycle's end;
// the PAYG scope names none of them. Everything else about the row is data,
// not invariant.
func (p Projection) Validate() error {
	switch p.ScopeKind {
	case ScopeEntitlementCycle:
		if p.EntitlementID == "" || p.CycleNumber <= 0 || p.PeriodEnd.IsZero() {
			return ErrUnknownScope
		}
	case ScopePayGBalance:
		if p.EntitlementID != "" || p.CycleNumber != 0 || !p.PeriodEnd.IsZero() {
			return ErrUnknownScope
		}
	default:
		return ErrUnknownScope
	}
	if p.FundingBucketID == "" || p.AliasGroupVersionID == "" {
		return ErrUnknownScope
	}
	if p.LimitAmount < 0 || p.Available < 0 {
		return ErrUnknownScope
	}
	return nil
}

// Publication is the Control Plane's word about one grant, delivered to the
// runtime's projection. It carries the Control-Plane-owned half of the row —
// identity, limit, state, and the revision that orders deliveries — and it
// never carries a balance: a redelivered or out-of-order publication must not
// resurrect capacity a drawdown already spent.
type Publication struct {
	AccountID             string
	FundingBucketID       string
	AliasGroupVersionID   string
	ScopeKind             ScopeKind
	EntitlementID         string
	CycleNumber           int
	NamedScope            bool
	PeriodEnd             time.Time
	SubscriptionCreatedAt time.Time
	State                 ProjectionState
	LimitAmount           int64
	Revision              int64
}

// PublicationOutcome is what applying one publication did, and the three
// values are the whole of the algebra the migration header states:
//
//   - Seeded: no row existed; the grant is new and available starts at the
//     limit, because a fresh grant's whole ceiling is offerable.
//   - Updated: a row existed at an older revision; the Control-Plane-owned
//     fields move. Available is deliberately not among them — spent capacity
//     must never be resurrected by a republication.
//   - Stale: a row existed at this revision or newer; the publication is a
//     redelivery or arrives out of order, and the row answers nothing.
type PublicationOutcome string

const (
	PublicationSeeded  PublicationOutcome = "seeded"
	PublicationUpdated PublicationOutcome = "updated"
	PublicationStale   PublicationOutcome = "stale"
)

// Refill is a capacity increase beyond the original grant, delivered as its
// own operation rather than smuggled through a republication: the guard
// revision is the projection's revision at the time the increase was granted,
// and the operation applies only to a projection still sitting at it. The
// refill id is the Control Plane's identity for the increase, and it is what
// makes the operation safe to deliver at-least-once: the store records every
// id once (the quota_refills unique), so a redelivery answers with the first
// delivery's outcome instead of minting the amount a second time.
type Refill struct {
	RefillID        string
	FundingBucketID string
	Amount          int64
	AtRevision      int64
}

// RefillOutcome is what applying one refill did, and the three values are the
// whole of the refill algebra:
//
//   - Applied: the identity's first sighting at a live guard revision; the
//     projection's available rose by the amount.
//   - AlreadyApplied: the identity was recorded before — a redelivery, or a
//     replay — and the answer repeats the first sighting's outcome instead of
//     minting again. The amount on the wire is not re-checked against the
//     recorded one: the identity is the contract, and a Control Plane that
//     reuses an identity for a different amount has already broken it.
//   - Stale: the projection moved past the guard revision before the refill
//     arrived; the identity is recorded unapplied, and the increase must be
//     re-derived under a new one.
type RefillOutcome string

const (
	RefillApplied        RefillOutcome = "applied"
	RefillAlreadyApplied RefillOutcome = "already_applied"
	RefillStale          RefillOutcome = "stale"
)

// Validate refuses a publication whose scope kind and scope fields disagree —
// the same shape rules a projection answers to, since a publication is how a
// projection is born. The check runs where the publication is applied, so a
// malformed one is refused at the call that carried it instead of at the row.
func (p Publication) Validate() error {
	switch p.ScopeKind {
	case ScopeEntitlementCycle:
		if p.EntitlementID == "" || p.CycleNumber <= 0 || p.PeriodEnd.IsZero() {
			return ErrUnknownScope
		}
	case ScopePayGBalance:
		if p.EntitlementID != "" || p.CycleNumber != 0 || !p.PeriodEnd.IsZero() {
			return ErrUnknownScope
		}
	default:
		return ErrUnknownScope
	}
	if p.AccountID == "" || p.FundingBucketID == "" || p.AliasGroupVersionID == "" {
		return ErrUnknownScope
	}
	// SubscriptionCreatedAt is NOT NULL on the row and the waterfall's third
	// sort input; a publication that left it out would seed a row the
	// waterfall could not place against its siblings.
	if p.SubscriptionCreatedAt.IsZero() {
		return ErrUnknownScope
	}
	if p.LimitAmount < 0 {
		return ErrUnknownScope
	}
	return nil
}

// ErrRefillNotGuarded marks a refill whose revision guard is missing — a
// negative revision cannot be a projection's state at any grant. An unguarded
// refill replayed is capacity minted twice, which is the one way this table
// could invent money — so the guard is not optional and the constructor
// refuses to build the operation without it.
var ErrRefillNotGuarded = errors.New("accounting: a refill must carry the revision it is guarded by")

// NewRefill builds a guarded, identified refill and refuses an unguarded or
// unidentified one at the site that forms it. Both refusals protect the same
// invariant — one identity mints at most once — from its two failure modes:
// without the guard, a replay mints twice; without the identity, there is
// nothing to tell the replay from the first delivery.
func NewRefill(refillID, fundingBucketID string, amount, atRevision int64) (Refill, error) {
	if refillID == "" {
		return Refill{}, errors.New("accounting: a refill carries the Control Plane's refill identity")
	}
	if fundingBucketID == "" {
		return Refill{}, errors.New("accounting: a refill names its bucket")
	}
	if amount <= 0 {
		return Refill{}, errors.New("accounting: a refill adds a positive amount")
	}
	if atRevision < 0 {
		return Refill{}, ErrRefillNotGuarded
	}
	return Refill{RefillID: refillID, FundingBucketID: fundingBucketID, Amount: amount, AtRevision: atRevision}, nil
}

// ByWaterfall orders an account's projections the way ADR 0003's one and only
// waterfall reads them: grants made under a named alias group before grants
// made under the singleton `*`, then the earliest period end, then the
// oldest subscription, then the entitlement id, then the funding bucket id as
// the last tiebreak. A PAYG balance names no period end, and no period end is
// the end of the order — the cycles drain before the balance does.
//
// The order is computed here, from the raw inputs the row stores, every time
// it is needed — never precomputed into a seed-time rank. A rank frozen at
// seed time would go stale the first time a publication moved a period end,
// and the waterfall would draw from a bucket the account's grants no longer
// name first. The function is also the canonical lock order: every writer
// that touches more than one of an account's rows touches them in this order,
// so two writers can never deadlock across the set. Admission and return call
// this one function; there is no second ordering anywhere in the runtime. The
// final funding-bucket tiebreak is what makes the order total — two
// projections can share every other input (one entitlement, two buckets) and
// the lock order still needs to pick one first — and the adapter's SQL walks
// this same order (`named_scope DESC, period_end ASC NULLS LAST,
// subscription_created_at ASC, entitlement_id ASC, funding_bucket_id ASC`);
// the two spellings are one order, and the integration suite proves it.
func ByWaterfall(projections []Projection) []Projection {
	ordered := make([]Projection, len(projections))
	copy(ordered, projections)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.NamedScope != b.NamedScope {
			return a.NamedScope
		}
		// No period end sorts last, matching the SQL's NULLS LAST: a zero
		// time would otherwise sort before every real date and draw the PAYG
		// balance first, which is the one order the waterfall is not.
		if a.PeriodEnd.IsZero() != b.PeriodEnd.IsZero() {
			return b.PeriodEnd.IsZero()
		}
		if !a.PeriodEnd.Equal(b.PeriodEnd) {
			return a.PeriodEnd.Before(b.PeriodEnd)
		}
		if !a.SubscriptionCreatedAt.Equal(b.SubscriptionCreatedAt) {
			return a.SubscriptionCreatedAt.Before(b.SubscriptionCreatedAt)
		}
		if a.EntitlementID != b.EntitlementID {
			return a.EntitlementID < b.EntitlementID
		}
		return a.FundingBucketID < b.FundingBucketID
	})
	return ordered
}
