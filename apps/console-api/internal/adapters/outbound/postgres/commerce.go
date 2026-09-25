package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The commerce repositories: translation between the persistence port's
// commerce vocabulary and the `control` database's commerce tables. Every
// query resolves its handle through the store, exactly as the identity
// repositories do — the caller's unit of work when the context carries one,
// the pool otherwise — so a roll's guarded cycle advance and the entitlement
// inserts that accompany it land in one transaction or not at all.
//
// What this file deliberately does not do: check-then-write. The due-work
// moves are single UPDATE statements whose WHERE clause repeats every
// predicate the caller's scan matched on — the state, the cycle the caller
// read, the clock test against the server's own transaction_timestamp(), the
// renewal and cancellation gates, and the owner account being active — and
// the boolean they return is the whole verdict. A constraint cannot read
// another row (the account gate is the example), so the statement is the
// guard; false always means the world moved and the caller re-reads, never
// that a half of the move landed. The clock the statements judge due-ness by
// is the database's, read back out through the Clock repository below.

// commerceUniqueViolation is the seam the insert mappings below classify on.
// The identity repositories needed only a driver's SQLSTATE, which any
// database/sql driver reports through the same structural interface — but a
// SQLSTATE does not name the constraint that fired, and one insert into the
// commerce tables can lose to several different business rules. The
// constraint name is carried only by the driver's own error type, so this
// file does what the structural seam stopped short of: it reaches for
// pgconn.PgError, the concrete error the pgx driver reports through
// database/sql. The type classifies errors and crosses no API boundary —
// nothing under internal/ ever sees it, exactly as with the driver's own
// registration in postgres.go.

// constraintOfUniqueViolation returns the constraint name behind err when
// the chain carries the driver's own report of a uniqueness violation
// (SQLSTATE 23505), and false otherwise. Every mapping that follows keys on
// the name, not the table, because the schema names its constraints for
// exactly this reader (000003's conventions: "error messages name what
// fired").
func constraintOfUniqueViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// insertSentinel maps the constraint a failed commerce insert lost to, to
// the domain sentinel the port promises for it. The named constraints are
// the ones an application insert can realistically lose to: the id columns
// are fresh random UUIDs and the v7-form CHECKs cannot be violated by a
// value the domain minted, so a primary-key or CHECK failure here is
// infrastructure refusing an impossible row, and it propagates wrapped
// rather than as a domain answer.
func insertSentinel(constraint string) error {
	switch constraint {
	case "plans_name_key":
		return commerce.ErrPlanNameTaken
	case "plan_versions_version_number_key":
		return commerce.ErrPlanVersionNumberTaken
	case "plan_grant_definitions_scope_dimension_key":
		return commerce.ErrGrantScopeTaken
	default:
		return nil
	}
}

// NewClock returns the persistence port's Clock — the database's own clock —
// backed by store. It panics on a nil store for the same reason every
// constructor here does: the failure a nil dependency produces later is
// strictly worse than a loud one here.
func NewClock(store persistence.Store) persistence.Clock {
	if store == nil {
		panic("postgres: NewClock requires a non-nil persistence.Store")
	}
	return &clockRepo{store: store}
}

// NewPlans returns the persistence port's Plans repository backed by store.
func NewPlans(store persistence.Store) persistence.Plans {
	if store == nil {
		panic("postgres: NewPlans requires a non-nil persistence.Store")
	}
	return &planRepo{store: store}
}

// NewPlanVersions returns the persistence port's PlanVersions repository
// backed by store.
func NewPlanVersions(store persistence.Store) persistence.PlanVersions {
	if store == nil {
		panic("postgres: NewPlanVersions requires a non-nil persistence.Store")
	}
	return &planVersionRepo{store: store}
}

// NewSubscriptions returns the persistence port's Subscriptions repository
// backed by store.
func NewSubscriptions(store persistence.Store) persistence.Subscriptions {
	if store == nil {
		panic("postgres: NewSubscriptions requires a non-nil persistence.Store")
	}
	return &subscriptionRepo{store: store}
}

// NewEntitlements returns the persistence port's Entitlements repository
// backed by store.
func NewEntitlements(store persistence.Store) persistence.Entitlements {
	if store == nil {
		panic("postgres: NewEntitlements requires a non-nil persistence.Store")
	}
	return &entitlementRepo{store: store}
}

// NewPaygAccounts returns the persistence port's PaygAccounts repository
// backed by store.
func NewPaygAccounts(store persistence.Store) persistence.PaygAccounts {
	if store == nil {
		panic("postgres: NewPaygAccounts requires a non-nil persistence.Store")
	}
	return &paygRepo{store: store}
}

// Compile-time proof that the repositories satisfy the port's contracts.
var (
	_ persistence.Clock         = (*clockRepo)(nil)
	_ persistence.Plans         = (*planRepo)(nil)
	_ persistence.PlanVersions  = (*planVersionRepo)(nil)
	_ persistence.Subscriptions = (*subscriptionRepo)(nil)
	_ persistence.Entitlements  = (*entitlementRepo)(nil)
	_ persistence.PaygAccounts  = (*paygRepo)(nil)
)

// ---------------------------------------------------------------------------
// clock — the database's own transaction_timestamp(), the cycle clock.
// ---------------------------------------------------------------------------

type clockRepo struct {
	store persistence.Store
}

const selectTransactionTimestamp = `
SELECT transaction_timestamp()`

// Now reads the database's transaction_timestamp() through whatever handle
// the context resolves to. Inside a unit of work the answer is that
// transaction's start instant — one value for the whole unit, which is what
// lets a roll read its now once and let every gate and every new cycle bound
// in the same transaction agree on it; outside one it is the reading
// statement's own instant. It is never the application's clock: a worker
// whose process clock drifts must not move a commercial boundary.
func (r *clockRepo) Now(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := r.store.Querier(ctx).QueryRowContext(ctx, selectTransactionTimestamp).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("postgres: read the database clock: %w", err)
	}
	return now, nil
}

// ---------------------------------------------------------------------------
// plans — the commercial product's identity root.
// ---------------------------------------------------------------------------

type planRepo struct {
	store persistence.Store
}

const insertPlan = `
INSERT INTO control.plans (id, name, created_at)
VALUES ($1, $2, $3)`

const selectPlan = `
SELECT id, name, created_at
FROM control.plans
WHERE id = $1`

func (r *planRepo) Create(ctx context.Context, plan commerce.Plan) error {
	_, err := r.store.Querier(ctx).ExecContext(ctx, insertPlan,
		string(plan.ID), plan.Name, plan.CreatedAt)
	if err != nil {
		// The one constraint an application insert can lose to here is
		// plans_name_key; the mapping keeps the constraint's name in this
		// file and the domain's word for it above the port.
		if constraint, ok := constraintOfUniqueViolation(err); ok {
			if sentinel := insertSentinel(constraint); sentinel != nil {
				return fmt.Errorf("postgres: create plan %s: %w", plan.ID, sentinel)
			}
		}
		return fmt.Errorf("postgres: create plan %s: %w", plan.ID, err)
	}
	return nil
}

func (r *planRepo) ByID(ctx context.Context, id commerce.PlanID) (commerce.Plan, error) {
	var p commerce.Plan
	err := r.store.Querier(ctx).QueryRowContext(ctx, selectPlan, string(id)).
		Scan(&p.ID, &p.Name, &p.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return commerce.Plan{}, fmt.Errorf("postgres: plan %s: %w", id, persistence.ErrNotFound)
		}
		return commerce.Plan{}, fmt.Errorf("postgres: plan %s: %w", id, err)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// plan_versions — the immutable commercial template, and the grant
// definitions that travel with it whole.
// ---------------------------------------------------------------------------

type planVersionRepo struct {
	store persistence.Store
}

const insertPlanVersion = `
INSERT INTO control.plan_versions
    (id, plan_id, version_number, period, recurring_price_minor_units,
     state, published_at, retired_at, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

const selectPlanVersion = `
SELECT id, plan_id, version_number, period, recurring_price_minor_units,
       state, published_at, retired_at, created_at, updated_at
FROM control.plan_versions
WHERE id = $1`

// The set travels whole and in a stable order: a version IS its definitions,
// and the read must not depend on the planner's mood. uuid order is mint
// order to within the v7 timestamp's millisecond, which is more determinism
// than anything here needs — it is only asked to be one order, always.
const selectPlanVersionDefinitions = `
SELECT id, plan_version_id, alias_group_name, dimension, granted_amount, created_at
FROM control.plan_grant_definitions
WHERE plan_version_id = $1
ORDER BY id ASC`

// The edit's single-statement verdict. The INSERT's SELECT carries the
// values and the guard together: the row lands only while the version still
// reads draft, so an edit racing a publication loses cleanly instead of
// editing a version a subscription has already pinned. A constraint cannot
// read another row, so the statement — not a check before it — is what makes
// the edit true.
const insertGrantDefinitionIfDraft = `
INSERT INTO control.plan_grant_definitions
    (id, plan_version_id, alias_group_name, dimension, granted_amount, created_at)
SELECT $1, $2, $3, $4, $5, $6
WHERE EXISTS (
    SELECT 1 FROM control.plan_versions pv
    WHERE pv.id = $2 AND pv.state = 'draft'
)`

const selectHighestVersionNumber = `
SELECT COALESCE(MAX(version_number), 0)
FROM control.plan_versions
WHERE plan_id = $1`

// The one-way moves, compare-and-swapped: the stamps land in the same
// statement as the state flip, only while the row still shows the state the
// caller read. The schema's plan_versions_state_stamps check would refuse
// the row if the stamps and the state ever disagreed — the statements and
// the constraint say the same thing, which is what makes either of them
// trustworthy.
const publishPlanVersion = `
UPDATE control.plan_versions
SET state = 'published', published_at = $2, updated_at = $2
WHERE id = $1 AND state = $3`

const retirePlanVersion = `
UPDATE control.plan_versions
SET state = 'retired', retired_at = $2, updated_at = $2
WHERE id = $1 AND state = $3`

func (r *planVersionRepo) Create(ctx context.Context, version commerce.PlanVersion) error {
	// A version is born draft with both stamps unset; the domain object
	// carries that as nil pointers, and the insert turns the absence into
	// NULL explicitly — an empty string is not.
	var publishedAt, retiredAt any
	if version.PublishedAt != nil {
		publishedAt = *version.PublishedAt
	}
	if version.RetiredAt != nil {
		retiredAt = *version.RetiredAt
	}
	_, err := r.store.Querier(ctx).ExecContext(ctx, insertPlanVersion,
		string(version.ID), string(version.PlanID), version.VersionNumber,
		string(version.Period), version.RecurringPriceMinorUnits, string(version.State),
		publishedAt, retiredAt, version.CreatedAt, version.UpdatedAt)
	if err != nil {
		// The live constraint is plan_versions_version_number_key — the
		// open-version race's signal to re-read the plan's highest number.
		if constraint, ok := constraintOfUniqueViolation(err); ok {
			if sentinel := insertSentinel(constraint); sentinel != nil {
				return fmt.Errorf("postgres: create plan version %s: %w", version.ID, sentinel)
			}
		}
		return fmt.Errorf("postgres: create plan version %s: %w", version.ID, err)
	}
	return nil
}

func (r *planVersionRepo) ByID(ctx context.Context, id commerce.PlanVersionID) (commerce.PlanVersion, []commerce.GrantDefinition, error) {
	var v commerce.PlanVersion
	var planID, period, state string
	var publishedAt, retiredAt sql.NullTime
	err := r.store.Querier(ctx).QueryRowContext(ctx, selectPlanVersion, string(id)).
		Scan(&v.ID, &planID, &v.VersionNumber, &period, &v.RecurringPriceMinorUnits,
			&state, &publishedAt, &retiredAt, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return commerce.PlanVersion{}, nil, fmt.Errorf("postgres: plan version %s: %w", id, persistence.ErrNotFound)
		}
		return commerce.PlanVersion{}, nil, fmt.Errorf("postgres: plan version %s: %w", id, err)
	}
	v.PlanID = commerce.PlanID(planID)
	v.Period = commerce.RecurringPeriod(period)
	v.State = commerce.PlanVersionState(state)
	if publishedAt.Valid {
		t := publishedAt.Time
		v.PublishedAt = &t
	}
	if retiredAt.Valid {
		t := retiredAt.Time
		v.RetiredAt = &t
	}

	// The definitions are the other half of the aggregate: a version without
	// them is a lie about what a subscription pins, so the read never stops
	// at the version row. Zero definitions is a whole set of none.
	rows, err := r.store.Querier(ctx).QueryContext(ctx, selectPlanVersionDefinitions, string(id))
	if err != nil {
		return commerce.PlanVersion{}, nil, fmt.Errorf("postgres: plan version %s: read grant definitions: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	definitions := make([]commerce.GrantDefinition, 0)
	for rows.Next() {
		var d commerce.GrantDefinition
		var versionID, scope, dimension string
		if err := rows.Scan(&d.ID, &versionID, &scope, &dimension, &d.GrantedAmount, &d.CreatedAt); err != nil {
			return commerce.PlanVersion{}, nil, fmt.Errorf("postgres: plan version %s: read grant definitions: %w", id, err)
		}
		d.PlanVersionID = commerce.PlanVersionID(versionID)
		d.AliasGroupName = commerce.AliasGroupName(scope)
		d.Dimension = commerce.Dimension(dimension)
		definitions = append(definitions, d)
	}
	if err := rows.Err(); err != nil {
		return commerce.PlanVersion{}, nil, fmt.Errorf("postgres: plan version %s: read grant definitions: %w", id, err)
	}
	return v, definitions, nil
}

func (r *planVersionRepo) AddGrantDefinition(ctx context.Context, definition commerce.GrantDefinition) error {
	res, err := r.store.Querier(ctx).ExecContext(ctx, insertGrantDefinitionIfDraft,
		string(definition.ID), string(definition.PlanVersionID), string(definition.AliasGroupName),
		string(definition.Dimension), definition.GrantedAmount, definition.CreatedAt)
	if err != nil {
		// The live constraint is plan_grant_definitions_scope_dimension_key —
		// one definition per (scope, dimension) per version.
		if constraint, ok := constraintOfUniqueViolation(err); ok {
			if sentinel := insertSentinel(constraint); sentinel != nil {
				return fmt.Errorf("postgres: add grant definition %s: %w", definition.ID, sentinel)
			}
		}
		return fmt.Errorf("postgres: add grant definition %s: %w", definition.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: add grant definition %s: read rows affected: %w", definition.ID, err)
	}
	// Zero rows is the guard's answer, not a failure: the version left draft
	// (or never was one) between the caller's read and this write, and the
	// published version a subscription may already pin must not be edited.
	// The domain's word for it is ErrVersionNotEditable.
	if n == 0 {
		return fmt.Errorf("postgres: add grant definition %s to version %s: %w",
			definition.ID, definition.PlanVersionID, commerce.ErrVersionNotEditable)
	}
	return nil
}

func (r *planVersionRepo) HighestVersionNumber(ctx context.Context, planID commerce.PlanID) (int, error) {
	var highest int
	if err := r.store.Querier(ctx).QueryRowContext(ctx, selectHighestVersionNumber, string(planID)).Scan(&highest); err != nil {
		return 0, fmt.Errorf("postgres: highest version number of plan %s: %w", planID, err)
	}
	return highest, nil
}

func (r *planVersionRepo) Publish(ctx context.Context, id commerce.PlanVersionID, from commerce.PlanVersionState, publishedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, publishPlanVersion,
		string(id), publishedAt, string(from))
	if err != nil {
		return false, fmt.Errorf("postgres: publish plan version %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: publish plan version %s: read rows affected: %w", id, err)
	}
	return n == 1, nil
}

func (r *planVersionRepo) Retire(ctx context.Context, id commerce.PlanVersionID, from commerce.PlanVersionState, retiredAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, retirePlanVersion,
		string(id), retiredAt, string(from))
	if err != nil {
		return false, fmt.Errorf("postgres: retire plan version %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: retire plan version %s: read rows affected: %w", id, err)
	}
	return n == 1, nil
}

// ---------------------------------------------------------------------------
// subscriptions — the lifecycle, its due-work scans, and the guarded moves
// that are their own verdicts.
// ---------------------------------------------------------------------------

type subscriptionRepo struct {
	store persistence.Store
}

const insertSubscription = `
INSERT INTO control.subscriptions
    (id, account_id, plan_version_id, state, start_at, renewal_enabled,
     cancel_at, cancellation_mode, cycle_number, period_start, period_end,
     created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`

const selectSubscription = `
SELECT id, account_id, plan_version_id, state, start_at, renewal_enabled,
       cancel_at, cancellation_mode, cycle_number, period_start, period_end,
       created_at, updated_at
FROM control.subscriptions
WHERE id = $1`

// The compare-and-swap for the reversible moves: the state lands only while
// the row still shows the one the caller read, so two racing transitions
// converge instead of one silently overwriting the other.
const transitionSubscription = `
UPDATE control.subscriptions
SET state = $3, updated_at = $4
WHERE id = $1 AND state = $2`

// The scheduled cancellation in one statement: cancel_at and its mode land
// together — the schema's subscriptions_cancellation_consistency demands
// exactly that — and only while the row still reads active.
const scheduleSubscriptionCancellation = `
UPDATE control.subscriptions
SET cancel_at = $2, cancellation_mode = 'scheduled', updated_at = $3
WHERE id = $1 AND state = 'active'`

// The immediate cancellation: state, stamp and mode land together, only
// while the row still reads active. A retried or racing cancellation
// converges — one caller sees the row active and wins, the rest read
// cancelled and stop. This is the immediate path only: a scheduled
// cancellation's completion keeps its original cancel_at, and that
// completion is not this statement.
const cancelSubscription = `
UPDATE control.subscriptions
SET state = 'cancelled', cancel_at = $2, cancellation_mode = 'immediate', updated_at = $2
WHERE id = $1 AND state = 'active'`

// The promotion's single-statement verdict. Every condition the caller
// checked before this statement ran is repeated in it — the row still
// pending, its start arrived on the database's clock, its owner account
// active — because a row can move between the read and the write, and the
// statement, not the check, is what makes the promotion true. The account
// gate is an EXISTS and not a constraint for exactly the reason the port
// records: a constraint cannot read another row.
const activateSubscription = `
UPDATE control.subscriptions
SET state = 'active', cycle_number = 1, period_start = $2, period_end = $3, updated_at = $4
WHERE id = $1
  AND state = 'pending'
  AND start_at <= transaction_timestamp()
  AND EXISTS (
      SELECT 1 FROM control.accounts a
      WHERE a.id = control.subscriptions.account_id AND a.state = 'active'
  )`

// The roll's single-statement verdict: the cycle fields advance only while
// the row still shows the state and cycle the caller read, the current
// period has ended on the database's clock, renewal is enabled, no
// scheduled cancellation reaches into the cycle being granted (a
// cancellation due exactly when the new cycle ends grants nothing — the
// strict >), and the owner account is active. The entitlements the caller
// inserts in the same unit of work commit or roll back with this
// statement's outcome.
const rollSubscription = `
UPDATE control.subscriptions
SET cycle_number = cycle_number + 1, period_start = $2, period_end = $3, updated_at = $4
WHERE id = $1
  AND state = 'active'
  AND cycle_number = $5
  AND renewal_enabled
  AND period_end IS NOT NULL
  AND period_end <= transaction_timestamp()
  AND (cancel_at IS NULL OR cancel_at > $3)
  AND EXISTS (
      SELECT 1 FROM control.accounts a
      WHERE a.id = control.subscriptions.account_id AND a.state = 'active'
  )`

// The natural end in one statement. Cancelled and already-expired rows are
// not due, and the statement says so by not firing; a renewing
// subscription is never here, because the roll lane owns its period end.
const expireSubscription = `
UPDATE control.subscriptions
SET state = 'expired', updated_at = $2
WHERE id = $1
  AND state IN ('active', 'suspended')
  AND renewal_enabled = false
  AND period_end IS NOT NULL
  AND period_end <= transaction_timestamp()
  AND cancel_at IS NULL`

// The due-work scans. Each repeats its move's predicate — the same words the
// guarded statement below it carries — so a row the scan hands over is a row
// the move's WHERE clause can still match, unless the world moved in
// between; when it did, the guard's false is the answer and the row waits
// for the next pass. LIMIT takes the worker's batch size.

const selectDuePromotionIDs = `
SELECT id FROM control.subscriptions
WHERE state = 'pending' AND start_at <= transaction_timestamp()
  AND EXISTS (
      SELECT 1 FROM control.accounts a
      WHERE a.id = control.subscriptions.account_id AND a.state = 'active'
  )
ORDER BY start_at ASC
LIMIT $1`

// A row whose cancel_at has also passed still appears here on purpose: the
// cancellation lane runs first in a worker pass, and a row that reaches this
// scan anyway is skipped by its own roll gate — cancel_at inside the new
// cycle refuses the advance.
const selectDueRollIDs = `
SELECT id FROM control.subscriptions
WHERE state = 'active' AND renewal_enabled
  AND period_end IS NOT NULL AND period_end <= transaction_timestamp()
  AND EXISTS (
      SELECT 1 FROM control.accounts a
      WHERE a.id = control.subscriptions.account_id AND a.state = 'active'
  )
ORDER BY period_end ASC
LIMIT $1`

const selectDueCancellationIDs = `
SELECT id FROM control.subscriptions
WHERE state = 'active' AND cancellation_mode = 'scheduled'
  AND cancel_at IS NOT NULL AND cancel_at <= transaction_timestamp()
ORDER BY cancel_at ASC
LIMIT $1`

const selectDueExpiryIDs = `
SELECT id FROM control.subscriptions
WHERE state IN ('active', 'suspended') AND renewal_enabled = false
  AND period_end IS NOT NULL AND period_end <= transaction_timestamp()
  AND cancel_at IS NULL
ORDER BY period_end ASC
LIMIT $1`

func (r *subscriptionRepo) Create(ctx context.Context, subscription commerce.Subscription) error {
	// The cycle fields are NULL exactly while pending, and the birth state is
	// where that holds; the nil pointers turn into NULL explicitly, the same
	// rule the api_keys creator NULL follows in identity.
	var cancelAt, periodStart, periodEnd any
	if subscription.CancelAt != nil {
		cancelAt = *subscription.CancelAt
	}
	if subscription.PeriodStart != nil {
		periodStart = *subscription.PeriodStart
	}
	if subscription.PeriodEnd != nil {
		periodEnd = *subscription.PeriodEnd
	}
	var cycleNumber any
	if subscription.CycleNumber != nil {
		cycleNumber = *subscription.CycleNumber
	}
	var cancellationMode any
	if subscription.CancellationMode != "" {
		cancellationMode = string(subscription.CancellationMode)
	}
	_, err := r.store.Querier(ctx).ExecContext(ctx, insertSubscription,
		string(subscription.ID), string(subscription.AccountID), string(subscription.PlanVersionID),
		string(subscription.State), subscription.StartAt, subscription.RenewalEnabled,
		cancelAt, cancellationMode, cycleNumber, periodStart, periodEnd,
		subscription.CreatedAt, subscription.UpdatedAt)
	if err != nil {
		// No uniqueness constraint is reachable from this insert: ids are
		// fresh, and the version-accepts-subscriptions check is the use
		// case's (a foreign key cannot filter another row's state). Whatever
		// fails here is infrastructure refusing an impossible row.
		return fmt.Errorf("postgres: create subscription %s: %w", subscription.ID, err)
	}
	return nil
}

func (r *subscriptionRepo) ByID(ctx context.Context, id commerce.SubscriptionID) (commerce.Subscription, error) {
	s, err := r.readSubscription(ctx, selectSubscription, id)
	if err != nil {
		return commerce.Subscription{}, err
	}
	return s, nil
}

// ByIDForUpdate is the roll's first statement inside its transaction: FOR
// UPDATE locks the row against concurrent writers until the unit of work
// ends, so every gate the domain re-checks after this read is checked
// against a row nobody else can move — and the guarded advance is what makes
// the verdict final.
func (r *subscriptionRepo) ByIDForUpdate(ctx context.Context, id commerce.SubscriptionID) (commerce.Subscription, error) {
	s, err := r.readSubscription(ctx, selectSubscription+" FOR UPDATE", id)
	if err != nil {
		return commerce.Subscription{}, err
	}
	return s, nil
}

// readSubscription runs one subscription lookup — plain, or row-locked — and
// scans the aggregate whole. The two entry points share it so the plain and
// FOR UPDATE reads cannot drift apart column by column.
func (r *subscriptionRepo) readSubscription(ctx context.Context, query string, id commerce.SubscriptionID) (commerce.Subscription, error) {
	var s commerce.Subscription
	var accountID, versionID, state string
	var cancelAt sql.NullTime
	var cancellationMode sql.NullString
	var cycleNumber sql.NullInt64
	var periodStart, periodEnd sql.NullTime
	err := r.store.Querier(ctx).QueryRowContext(ctx, query, string(id)).
		Scan(&s.ID, &accountID, &versionID, &state, &s.StartAt, &s.RenewalEnabled,
			&cancelAt, &cancellationMode, &cycleNumber, &periodStart, &periodEnd,
			&s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return commerce.Subscription{}, fmt.Errorf("postgres: subscription %s: %w", id, persistence.ErrNotFound)
		}
		return commerce.Subscription{}, fmt.Errorf("postgres: subscription %s: %w", id, err)
	}
	s.AccountID = commerce.AccountID(accountID)
	s.PlanVersionID = commerce.PlanVersionID(versionID)
	s.State = commerce.SubscriptionState(state)
	if cancelAt.Valid {
		t := cancelAt.Time
		s.CancelAt = &t
	}
	s.CancellationMode = commerce.CancellationMode(cancellationMode.String)
	if cycleNumber.Valid {
		c := int(cycleNumber.Int64)
		s.CycleNumber = &c
	}
	if periodStart.Valid {
		t := periodStart.Time
		s.PeriodStart = &t
	}
	if periodEnd.Valid {
		t := periodEnd.Time
		s.PeriodEnd = &t
	}
	return s, nil
}

func (r *subscriptionRepo) TransitionState(ctx context.Context, id commerce.SubscriptionID, from, to commerce.SubscriptionState, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, transitionSubscription,
		string(id), string(from), string(to), updatedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: transition subscription %s %s->%s: %w", id, from, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: transition subscription %s: read rows affected: %w", id, err)
	}
	return n == 1, nil
}

func (r *subscriptionRepo) ScheduleCancellation(ctx context.Context, id commerce.SubscriptionID, cancelAt time.Time, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, scheduleSubscriptionCancellation,
		string(id), cancelAt, updatedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: schedule cancellation of subscription %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: schedule cancellation of subscription %s: read rows affected: %w", id, err)
	}
	// False means the subscription moved on — the caller re-reads and lets
	// the domain refuse what the state no longer admits.
	return n == 1, nil
}

// The cancellation lane's single-statement verdict: the completion keeps the
// instructed instant and its mode on the terminal row — the historical
// record of when the customer asked — and refuses to fire on anything the
// scan's own words no longer describe: a row that moved, an instruction no
// longer scheduled, one the database clock has not reached. The `updated_at`
// stamp is the caller's; `cancel_at` is not touched, because the instant it
// holds is the instruction's, not the completion's.
const completeScheduledCancellation = `
UPDATE control.subscriptions
SET state = 'cancelled', updated_at = $2
WHERE id = $1
  AND state = 'active'
  AND cancellation_mode = 'scheduled'
  AND cancel_at IS NOT NULL
  AND cancel_at <= transaction_timestamp()`

func (r *subscriptionRepo) CompleteScheduledCancellation(ctx context.Context, id commerce.SubscriptionID, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, completeScheduledCancellation, string(id), updatedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: complete scheduled cancellation of subscription %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: complete scheduled cancellation of subscription %s: read rows affected: %w", id, err)
	}
	// False means the world moved — the row left active, or the instruction
	// was rescheduled or withdrawn after the scan read it. The row waits
	// for the next pass.
	return n == 1, nil
}

func (r *subscriptionRepo) Cancel(ctx context.Context, id commerce.SubscriptionID, cancelledAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, cancelSubscription, string(id), cancelledAt)
	if err != nil {
		return false, fmt.Errorf("postgres: cancel subscription %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: cancel subscription %s: read rows affected: %w", id, err)
	}
	// False means the row was not active: a racing or repeated cancellation
	// already won, or the subscription is somewhere cancellation does not
	// apply. Either way the caller re-reads and stops.
	return n == 1, nil
}

func (r *subscriptionRepo) Activate(ctx context.Context, id commerce.SubscriptionID, periodStart, periodEnd, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, activateSubscription,
		string(id), periodStart, periodEnd, updatedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: activate subscription %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: activate subscription %s: read rows affected: %w", id, err)
	}
	// False means the world moved and the caller re-reads: the row is no
	// longer pending, its start has not arrived, or its owner account is not
	// active. The statement is the verdict, not the check that preceded it.
	return n == 1, nil
}

func (r *subscriptionRepo) Roll(ctx context.Context, id commerce.SubscriptionID, fromCycle int, periodStart, periodEnd, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, rollSubscription,
		string(id), periodStart, periodEnd, updatedAt, fromCycle)
	if err != nil {
		return false, fmt.Errorf("postgres: roll subscription %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: roll subscription %s: read rows affected: %w", id, err)
	}
	// False means the world moved and the caller re-reads: the state, the
	// cycle the caller read, the clock, the renewal gate, the cancellation
	// gate or the account gate refused the advance, and a half-rolled
	// subscription does not exist to find.
	return n == 1, nil
}

func (r *subscriptionRepo) Expire(ctx context.Context, id commerce.SubscriptionID, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, expireSubscription, string(id), updatedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: expire subscription %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: expire subscription %s: read rows affected: %w", id, err)
	}
	// False means the row is not due: renewal is on, the period has not
	// ended, a cancellation instruction is outstanding, or the state is
	// already terminal. The caller re-reads; the next scan settles it.
	return n == 1, nil
}

func (r *subscriptionRepo) DuePromotionIDs(ctx context.Context, limit int) ([]commerce.SubscriptionID, error) {
	return scanSubscriptionIDs(ctx, r.store.Querier(ctx), selectDuePromotionIDs, limit,
		"scan due promotion ids")
}

func (r *subscriptionRepo) DueRollIDs(ctx context.Context, limit int) ([]commerce.SubscriptionID, error) {
	return scanSubscriptionIDs(ctx, r.store.Querier(ctx), selectDueRollIDs, limit,
		"scan due roll ids")
}

func (r *subscriptionRepo) DueCancellationIDs(ctx context.Context, limit int) ([]commerce.SubscriptionID, error) {
	return scanSubscriptionIDs(ctx, r.store.Querier(ctx), selectDueCancellationIDs, limit,
		"scan due cancellation ids")
}

func (r *subscriptionRepo) DueExpiryIDs(ctx context.Context, limit int) ([]commerce.SubscriptionID, error) {
	return scanSubscriptionIDs(ctx, r.store.Querier(ctx), selectDueExpiryIDs, limit,
		"scan due expiry ids")
}

// scanSubscriptionIDs runs one due-work scan and collects its ids. The
// result is an empty slice, never nil: a lane with nothing due is a normal
// pass, and a nil that must be told apart from an empty result is a bug
// waiting for a worker to write it.
func scanSubscriptionIDs(ctx context.Context, q persistence.Querier, query string, limit int, what string) ([]commerce.SubscriptionID, error) {
	rows, err := q.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: %s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]commerce.SubscriptionID, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("postgres: %s: %w", what, err)
		}
		ids = append(ids, commerce.SubscriptionID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: %s: %w", what, err)
	}
	return ids, nil
}

// ---------------------------------------------------------------------------
// entitlements — the grants each roll materialises, and the derivation's
// inputs.
// ---------------------------------------------------------------------------

type entitlementRepo struct {
	store persistence.Store
}

const insertEntitlement = `
INSERT INTO control.entitlements
    (id, subscription_id, cycle_number, grant_definition_id, alias_group_version_id,
     dimension, granted_amount, state, period_start, period_end, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

const expireEntitlement = `
UPDATE control.entitlements
SET state = 'expired', updated_at = $2
WHERE id = $1 AND state = 'active' AND period_end <= transaction_timestamp()`

const selectDueEntitlementExpiryIDs = `
SELECT id FROM control.entitlements
WHERE state = 'active' AND period_end <= transaction_timestamp()
ORDER BY period_end ASC
LIMIT $1`

// ActiveCandidates is the query the domain derivation is written against:
// every active entitlement whose owning subscription is active, each joined
// to the two facts the waterfall reads from neighbouring rows — the
// subscription's creation instant (the third tie-break, never normalised to
// start_at) and the grant definition's scope NAME (the specificity key and
// the seam the membership predicate closes over). The period-coverage test
// is deliberately absent: coverage is judged at the caller's instant, by the
// derivation, which re-checks it defensively because a wrong answer here is
// money.
const selectActiveCandidates = `
SELECT e.id, e.subscription_id, e.cycle_number, e.grant_definition_id, e.alias_group_version_id,
       e.dimension, e.granted_amount, e.state, e.period_start, e.period_end,
       e.created_at, e.updated_at,
       s.created_at, d.alias_group_name
FROM control.entitlements e
JOIN control.subscriptions s ON s.id = e.subscription_id
JOIN control.plan_grant_definitions d ON d.id = e.grant_definition_id
WHERE s.account_id = $1
  AND s.state = 'active'
  AND e.state = 'active'
ORDER BY e.id ASC`

func (r *entitlementRepo) Create(ctx context.Context, entitlement commerce.Entitlement) error {
	_, err := r.store.Querier(ctx).ExecContext(ctx, insertEntitlement,
		string(entitlement.ID), string(entitlement.SubscriptionID), entitlement.CycleNumber,
		string(entitlement.GrantDefinitionID), string(entitlement.AliasGroupVersionID),
		string(entitlement.Dimension), entitlement.GrantedAmount, string(entitlement.State),
		entitlement.PeriodStart, entitlement.PeriodEnd, entitlement.CreatedAt, entitlement.UpdatedAt)
	if err != nil {
		// Deliberately unmapped: the one uniqueness constraint this insert
		// can lose to is entitlements_grant_once_per_cycle, and only a buggy
		// roll can fire it — the insert runs inside the same unit of work as
		// the guarded cycle advance, so a duplicate would mean the cycle was
		// already rolled and the unit of work would not have been opened.
		// The guarded advance is the real guard; this error surfaces as the
		// infrastructure refusal it is.
		return fmt.Errorf("postgres: create entitlement %s: %w", entitlement.ID, err)
	}
	return nil
}

func (r *entitlementRepo) Expire(ctx context.Context, id commerce.EntitlementID, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, expireEntitlement, string(id), updatedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: expire entitlement %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: expire entitlement %s: read rows affected: %w", id, err)
	}
	// False means the row is not due — its cycle has not ended on the
	// database's clock, or it is already expired. Expiry is terminal and
	// absorbs repetition; the caller re-reads and moves on.
	return n == 1, nil
}

func (r *entitlementRepo) DueExpiryIDs(ctx context.Context, limit int) ([]commerce.EntitlementID, error) {
	rows, err := r.store.Querier(ctx).QueryContext(ctx, selectDueEntitlementExpiryIDs, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: scan due entitlement expiry ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]commerce.EntitlementID, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("postgres: scan due entitlement expiry ids: %w", err)
		}
		ids = append(ids, commerce.EntitlementID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: scan due entitlement expiry ids: %w", err)
	}
	return ids, nil
}

func (r *entitlementRepo) ActiveCandidates(ctx context.Context, accountID commerce.AccountID) ([]commerce.CandidateGrant, error) {
	rows, err := r.store.Querier(ctx).QueryContext(ctx, selectActiveCandidates, string(accountID))
	if err != nil {
		return nil, fmt.Errorf("postgres: active candidates of account %s: %w", accountID, err)
	}
	defer func() { _ = rows.Close() }()
	candidates := make([]commerce.CandidateGrant, 0)
	for rows.Next() {
		var e commerce.Entitlement
		var subscriptionID, definitionID, scopeVersion, dimension, state string
		var scope string
		var subscriptionCreatedAt time.Time
		if err := rows.Scan(&e.ID, &subscriptionID, &e.CycleNumber, &definitionID, &scopeVersion,
			&dimension, &e.GrantedAmount, &state, &e.PeriodStart, &e.PeriodEnd,
			&e.CreatedAt, &e.UpdatedAt,
			&subscriptionCreatedAt, &scope); err != nil {
			return nil, fmt.Errorf("postgres: active candidates of account %s: %w", accountID, err)
		}
		e.SubscriptionID = commerce.SubscriptionID(subscriptionID)
		e.GrantDefinitionID = commerce.GrantDefinitionID(definitionID)
		e.AliasGroupVersionID = commerce.AliasGroupVersionID(scopeVersion)
		e.Dimension = commerce.Dimension(dimension)
		e.State = commerce.EntitlementState(state)
		candidates = append(candidates, commerce.CandidateGrant{
			Entitlement:           &e,
			SubscriptionCreatedAt: subscriptionCreatedAt,
			AliasGroupName:        commerce.AliasGroupName(scope),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: active candidates of account %s: %w", accountID, err)
	}
	return candidates, nil
}

// ---------------------------------------------------------------------------
// account_payg — the PAYG enablement flag, and the bucket reference
// Accounting's choreography assigns.
// ---------------------------------------------------------------------------

type paygRepo struct {
	store persistence.Store
}

const selectAccountPayg = `
SELECT account_id, enabled, funding_bucket_id, created_at, updated_at
FROM control.account_payg
WHERE account_id = $1`

// The one place this port writes without a prior read: the flag's whole
// state is one boolean, and insert-or-update is the single statement that
// carries it. created_at rides the insert path only — the row's birth is
// the first recorded state, an enable or a disable alike, and an update must
// not rewrite it; funding_bucket_id
// is deliberately absent from both paths, because disabling does not touch
// the bucket and the reference is Accounting's to assign once.
const setAccountPaygEnabled = `
INSERT INTO control.account_payg (account_id, enabled, created_at, updated_at)
VALUES ($1, $2, $3, $3)
ON CONFLICT (account_id) DO UPDATE
SET enabled = EXCLUDED.enabled, updated_at = EXCLUDED.updated_at`

// Write-once: the reference lands only while the row's reference is still
// unset. One PAYG source and one bucket per account, ever — a changed
// reference would silently split the account's prepaid money across two
// buckets, so the statement refuses what a read-then-write would race into.
const assignFundingBucket = `
UPDATE control.account_payg
SET funding_bucket_id = $2, updated_at = $3
WHERE account_id = $1 AND funding_bucket_id IS NULL`

func (r *paygRepo) ByAccount(ctx context.Context, accountID commerce.AccountID) (commerce.AccountPayg, error) {
	var p commerce.AccountPayg
	var id string
	var fundingBucketID sql.NullString
	err := r.store.Querier(ctx).QueryRowContext(ctx, selectAccountPayg, string(accountID)).
		Scan(&id, &p.Enabled, &fundingBucketID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return commerce.AccountPayg{}, fmt.Errorf("postgres: account payg %s: %w", accountID, persistence.ErrNotFound)
		}
		return commerce.AccountPayg{}, fmt.Errorf("postgres: account payg %s: %w", accountID, err)
	}
	p.AccountID = commerce.AccountID(id)
	// The absent reference is the zero value: an account that has never been
	// funded has no bucket, and no string here is not an empty string.
	p.FundingBucketID = commerce.FundingBucketID(fundingBucketID.String)
	return p, nil
}

func (r *paygRepo) SetEnabled(ctx context.Context, accountID commerce.AccountID, enabled bool, now time.Time) error {
	if _, err := r.store.Querier(ctx).ExecContext(ctx, setAccountPaygEnabled,
		string(accountID), enabled, now); err != nil {
		return fmt.Errorf("postgres: set payg enabled for account %s: %w", accountID, err)
	}
	return nil
}

func (r *paygRepo) AssignFundingBucket(ctx context.Context, accountID commerce.AccountID, bucketID commerce.FundingBucketID, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, assignFundingBucket,
		string(accountID), string(bucketID), updatedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: assign funding bucket to account %s: %w", accountID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: assign funding bucket to account %s: read rows affected: %w", accountID, err)
	}
	// False means the world moved and the caller re-reads: a reference is
	// already on file, or the account has no PAYG row at all — a row comes
	// into being the first time any PAYG state is recorded, enable or
	// disable alike, so no row means no state and no reference has ever
	// existed to assign into. Either way nothing was written.
	return n == 1, nil
}
