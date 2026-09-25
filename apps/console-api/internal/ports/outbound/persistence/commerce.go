package persistence

import (
	"context"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
)

// The commerce repositories, in the store's vocabulary.
//
// Commerce is the Control Plane's commercial authority (ADR 0001, ADR 0003):
// plans and their immutable versions, the subscriptions pinned to them, the
// entitlements each cycle materialises, and the per-account PAYG flag. Every
// table it names lives in the `control` database — the authority for
// everything a customer is billed from — and nowhere else. What commerce
// deliberately does not reach from here: the quota projection and the funding
// buckets (the Data Plane's and Accounting's to own), the catalog's
// group-version rows (read over the management seam, never joined), and
// identity's accounts (read through the identity repositories when a use case
// must check ownership state).
//
// Four rules the signatures below carry on purpose, in continuation of the
// identity port's:
//
//   - Aggregates go in whole and come out whole. A plan version IS its grant
//     definitions: ByID returns both, and a definition row without its
//     version cannot exist through this port. Subscriptions carry their
//     cycle fields as one unit — the advance writes all of them or none.
//   - The due-work moves travel as single-statement verdicts, not as
//     check-then-write. Each guard method is one UPDATE whose WHERE clause
//     carries the whole predicate the scan matched on — the state, the cycle
//     the caller read, the clock test, the renewal and cancellation gates,
//     and the owner account being active — and its boolean answer is the
//     whole verdict: false means the world moved and the caller re-reads,
//     never that a half of the move landed. A constraint cannot read another
//     row (the account gate is the example), so the statement is the guard,
//     and the transaction the lane runs in is what makes the row's advance
//     and its new entitlements one fact.
//   - The cycle clock is the database's. Every scan and every guard compares
//     against the server's own transaction_timestamp(), never against an
//     instant the application supplys: a worker whose process clock drifts
//     must not move a commercial boundary. The one corollary is documented
//     at the use cases: the bounds of a new cycle are computed from a
//     transaction_timestamp() read inside the roll's unit of work — the
//     lane's single, deliberate exception.
//   - Deletes do not exist. Cancelled and expired are states, entitlements
//     of ended cycles flip to expired and stay, and the rows are history
//     immutable accounting references point at.

// Clock is the due-work lanes' time source: the database's own clock, read
// through whatever connection the context resolves to. Inside a unit of work
// the answer is that transaction's transaction_timestamp() — one instant,
// stable for the whole unit, which is what makes every gate in the unit
// evaluate against the same now; outside one it is the reading statement's
// own. It is a port member rather than a SQL string in the application
// because "what time does the database think it is" is an adapter question,
// and the application's only honest alternatives would be its own drifting
// process clock or a raw query.
type Clock interface {
	// Now returns the database's transaction_timestamp() for the connection
	// ctx resolves to. It never returns the application's clock, and an
	// implementation wraps transport failures as itself.
	Now(ctx context.Context) (time.Time, error)
}

// Plans persists the plan aggregate — the commercial product's identity root.
type Plans interface {
	// Create inserts a new plan root. A name another plan already carries
	// surfaces as commerce.ErrPlanNameTaken — the schema's plans_name_key,
	// translated here and nowhere above it.
	Create(ctx context.Context, plan commerce.Plan) error

	// ByID returns the plan with id, or ErrNotFound.
	ByID(ctx context.Context, id commerce.PlanID) (commerce.Plan, error)
}

// PlanVersions persists one plan version together with its grant
// definitions — the immutable end of the commercial catalog.
type PlanVersions interface {
	// Create inserts the version row in its birth state (draft). A version
	// number the plan already carries surfaces as
	// commerce.ErrPlanVersionNumberTaken — the open-version race's signal to
	// re-read the plan's highest number and open the next one.
	Create(ctx context.Context, version commerce.PlanVersion) error

	// ByID returns the version with its whole grant-definition set, or
	// ErrNotFound. The definitions read as of their own committed statement;
	// a version that has left draft is frozen by the domain and the schema,
	// so the set cannot grow under the caller that reads it.
	ByID(ctx context.Context, id commerce.PlanVersionID) (commerce.PlanVersion, []commerce.GrantDefinition, error)

	// AddGrantDefinition inserts one definition under the version named by
	// the definition's PlanVersionID. The insert is a single-statement
	// verdict: it lands only while the version still reads draft, so an
	// edit racing a publication loses cleanly — a false move surfaces as
	// commerce.ErrVersionNotEditable, and a scope the version already
	// defines as commerce.ErrGrantScopeTaken.
	AddGrantDefinition(ctx context.Context, definition commerce.GrantDefinition) error

	// HighestVersionNumber returns the plan's newest version number, or 0
	// when the plan has none — the 0 that makes the caller's `highest + 1`
	// open version 1 without a special case.
	HighestVersionNumber(ctx context.Context, planID commerce.PlanID) (int, error)

	// Publish applies the draft → published move, compare-and-swapped: the
	// stamps land only while the row still shows `from`, and a false return
	// means someone else moved the version first.
	Publish(ctx context.Context, id commerce.PlanVersionID, from commerce.PlanVersionState, publishedAt time.Time) (bool, error)

	// Retire applies the published → retired move, compare-and-swapped like
	// Publish. A draft refuses retirement in the domain before this is
	// reached; the statement repeats the gate as the schema cannot read the
	// state it would depend on either.
	Retire(ctx context.Context, id commerce.PlanVersionID, from commerce.PlanVersionState, retiredAt time.Time) (bool, error)
}

// Subscriptions persists the subscription aggregate and runs the due-work
// lanes' guarded moves.
type Subscriptions interface {
	// Create inserts a subscription in its birth state (pending, cycle
	// fields null). That the pinned version accepts subscriptions is the
	// subscribing use case's check — no statement this port runs could read
	// another row's state and stay single-table — so the check's documented
	// residual is the same one the identity use cases carry: the window is
	// one transaction wide, and retirement never rewrites a published
	// version's terms.
	Create(ctx context.Context, subscription commerce.Subscription) error

	// ByID returns the subscription with id, or ErrNotFound.
	ByID(ctx context.Context, id commerce.SubscriptionID) (commerce.Subscription, error)

	// ByIDForUpdate returns the subscription with id, locking its row
	// against concurrent writers until the caller's unit of work ends. It is
	// the roll's first statement inside its transaction: every gate the
	// domain re-checks after this read is checked against a row nobody else
	// can move, and the guarded advance is what makes the verdict final.
	// A miss is ErrNotFound.
	ByIDForUpdate(ctx context.Context, id commerce.SubscriptionID) (commerce.Subscription, error)

	// TransitionState applies the lifecycle's reversible moves — the
	// active ↔ suspended pair — compare-and-swapped exactly as the identity
	// port's: the state lands only while the row still shows `from`, the
	// stamp is the caller's instant, and false means re-read.
	TransitionState(ctx context.Context, id commerce.SubscriptionID, from, to commerce.SubscriptionState, updatedAt time.Time) (bool, error)

	// ScheduleCancellation records a scheduled cancellation on an active
	// subscription in one statement: cancel_at and its mode land together,
	// only while the row still shows active. False means the subscription
	// moved on — the caller re-reads and lets the domain refuse what the
	// state no longer admits.
	ScheduleCancellation(ctx context.Context, id commerce.SubscriptionID, cancelAt time.Time, updatedAt time.Time) (bool, error)

	// Cancel applies the immediate cancellation in one statement — state
	// cancelled, cancel_at and mode stamped, only while the row still shows
	// active. A retried or racing cancellation converges: one of them sees
	// the row active and wins, the rest read cancelled and stop.
	Cancel(ctx context.Context, id commerce.SubscriptionID, cancelledAt time.Time) (bool, error)

	// Activate is the promotion's single-statement verdict: the pending row
	// becomes active with cycle 1's bounds only while it still reads
	// pending, its start_at has arrived on the database clock, and its
	// owner account is active. Every condition the domain checked before
	// this statement ran is repeated in it, because a row can move between
	// the read and the write and the statement — not the check — is what
	// makes the promotion true. False means the world moved; the caller
	// re-reads.
	Activate(ctx context.Context, id commerce.SubscriptionID, periodStart, periodEnd, updatedAt time.Time) (bool, error)

	// Roll is the cycle roll's single-statement verdict: the cycle fields
	// advance only while the row still shows the state and cycle the caller
	// read, the current period has ended on the database clock, renewal is
	// enabled, no scheduled cancellation reaches into the new cycle, and the
	// owner account is active. The new entitlements the caller inserted in
	// the same unit of work commit or roll back with this statement's
	// outcome — that is what makes a roll one fact.
	Roll(ctx context.Context, id commerce.SubscriptionID, fromCycle int, periodStart, periodEnd, updatedAt time.Time) (bool, error)

	// Expire applies the natural end in one statement — state expired, only
	// while the row reads active or suspended, renewal is off, and its
	// period has ended on the database clock. Cancelled and already-expired
	// rows are not due, and the statement says so by not firing.
	Expire(ctx context.Context, id commerce.SubscriptionID, updatedAt time.Time) (bool, error)

	// DuePromotionIDs returns at most limit ids of pending subscriptions
	// whose start_at the database clock has passed, oldest start first.
	DuePromotionIDs(ctx context.Context, limit int) ([]commerce.SubscriptionID, error)

	// DueRollIDs returns at most limit ids of active, renewing
	// subscriptions whose period the database clock has ended, soonest
	// period end first. A subscription whose cancel_at has also passed
	// still appears here — the cancellation lane runs first in a worker
	// pass, and a row that reaches this scan anyway is skipped by its own
	// roll gate.
	DueRollIDs(ctx context.Context, limit int) ([]commerce.SubscriptionID, error)

	// DueCancellationIDs returns at most limit ids of active subscriptions
	// whose scheduled cancel_at the database clock has passed, earliest
	// instruction first.
	DueCancellationIDs(ctx context.Context, limit int) ([]commerce.SubscriptionID, error)

	// DueExpiryIDs returns at most limit ids of active or suspended
	// subscriptions whose fixed term has ended on the database clock —
	// renewal off, period over, no cancellation instruction outstanding.
	// Soonest period end first.
	DueExpiryIDs(ctx context.Context, limit int) ([]commerce.SubscriptionID, error)
}

// Entitlements persists the entitlement rows — the grants each roll
// materialises — and reads the derivation's inputs.
type Entitlements interface {
	// Create inserts one entitlement row. The schema's
	// entitlements_grant_once_per_cycle makes a retried roll unable to
	// grant a cycle twice: the insert is made only inside the same unit of
	// work as the guarded cycle advance, so a duplicate would mean the
	// cycle was already rolled and the unit of work would not have been
	// opened.
	Create(ctx context.Context, entitlement commerce.Entitlement) error

	// Expire applies the active → expired flip in one statement, only while
	// the row reads active and its period has ended on the database clock.
	// An expired row is not due, and the statement says so by not firing.
	Expire(ctx context.Context, id commerce.EntitlementID, updatedAt time.Time) (bool, error)

	// DueExpiryIDs returns at most limit ids of active entitlements whose
	// period the database clock has ended, soonest period end first.
	DueExpiryIDs(ctx context.Context, limit int) ([]commerce.EntitlementID, error)

	// ActiveCandidates returns the derivation's inputs for one account:
	// every active entitlement whose owning subscription is active, each
	// carrying the two facts the waterfall reads from neighbouring rows —
	// the subscription's creation instant and the grant definition's scope
	// name. It is the query the domain derivation is written against; the
	// ordering of the result is the query's, and the derivation's output
	// order is the waterfall's.
	ActiveCandidates(ctx context.Context, accountID commerce.AccountID) ([]commerce.CandidateGrant, error)
}

// PaygAccounts persists the per-account PAYG commercial state — the flag
// commerce owns and the bucket reference Accounting's choreography assigns.
type PaygAccounts interface {
	// ByAccount returns the account's PAYG state row, or ErrNotFound. The
	// absence of a row is the disabled state — an account that never enabled
	// PAYG has no row, and there is no state cheaper than that.
	ByAccount(ctx context.Context, accountID commerce.AccountID) (commerce.AccountPayg, error)

	// SetEnabled writes the flag in one statement that inserts the row when
	// it does not exist and updates it when it does — the one place this
	// port writes without a prior read, because the flag's whole state is
	// one boolean and an insert-or-update is the single statement that
	// carries it. Enabling does not fund anything and disabling does not
	// touch the bucket; both facts are the use case's to state and the
	// ledger's to discover.
	SetEnabled(ctx context.Context, accountID commerce.AccountID, enabled bool, now time.Time) error

	// AssignFundingBucket records the bucket reference, write-once: the
	// assignment lands only while the row's reference is still unset, and
	// false means a reference is already on file — one PAYG source and one
	// bucket per account, ever. The row a first PAYG enablement inserted is
	// the row this assigns into; an account with no row has never enabled
	// PAYG and has nothing to fund, and false is not that case's answer.
	AssignFundingBucket(ctx context.Context, accountID commerce.AccountID, bucketID commerce.FundingBucketID, updatedAt time.Time) (bool, error)
}
