package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The console's commerce reads: the plan catalogue, an account's
// subscriptions, and the entitlements its rolls materialised.
//
// The account predicate is the WHERE clause and the first argument on the
// two account-scoped lists, for the reason the identity read file states in
// full: another account's row must be a row the query never returned, so it
// costs what a genuinely absent row costs. The catalogue is the one list on
// the console's surface with no predicate, and that is the contract's
// reasoning — a plan is not account-scoped, it is what every account buys
// from — not an omission.
//
// Entitlements are the one place the two account-scoped lists differ
// structurally, and the difference is worth stating because it is the only
// cross-aggregate read this file makes. control.entitlements carries no
// account_id of its own: a grant belongs to a subscription, and the
// subscription belongs to an account. So the predicate is an EXISTS against
// control.subscriptions rather than a column test. It is still the WHERE
// clause and still the first argument, and it is still a correlated test the
// planner evaluates per candidate row before any row is returned — which is
// what keeps the "not yours" and "not there" answers indistinguishable.

// NewAccountPlans returns the plan catalogue's read repository, and
// NewAccountSubscriptions and NewAccountEntitlements the two account-scoped
// ones. All three panic on a nil store for the reason every constructor in
// this package does.
func NewAccountPlans(store persistence.Store) persistence.AccountPlans {
	if store == nil {
		panic("postgres: NewAccountPlans requires a non-nil persistence.Store")
	}
	return &accountPlansRepo{store: store}
}

func NewAccountSubscriptions(store persistence.Store) persistence.AccountSubscriptions {
	if store == nil {
		panic("postgres: NewAccountSubscriptions requires a non-nil persistence.Store")
	}
	return &accountSubscriptionsRepo{store: store}
}

func NewAccountEntitlements(store persistence.Store) persistence.AccountEntitlements {
	if store == nil {
		panic("postgres: NewAccountEntitlements requires a non-nil persistence.Store")
	}
	return &accountEntitlementsRepo{store: store}
}

// Compile-time proof that the repositories satisfy the port's contracts.
var (
	_ persistence.AccountPlans         = (*accountPlansRepo)(nil)
	_ persistence.AccountSubscriptions = (*accountSubscriptionsRepo)(nil)
	_ persistence.AccountEntitlements  = (*accountEntitlementsRepo)(nil)
)

// ---------------------------------------------------------------------------
// plans — the catalogue, unfiltered.
// ---------------------------------------------------------------------------

type accountPlansRepo struct {
	store persistence.Store
}

// No WHERE clause and no account predicate, and that is the contract's
// statement rather than an oversight this file inherits: a plan is not
// account-scoped, so "the plans this account uses" is a different question
// and the subscriptions list is where it is answered.
//
// A plan root is immutable after creation — the schema carries no
// updated_at on it for that reason — so the id is the only sort key the
// table has, and it is unique.
const listPlans = `
SELECT ` + planColumns + `
FROM control.plans
WHERE id > $1
ORDER BY id
LIMIT $2`

func (r *accountPlansRepo) List(ctx context.Context, page persistence.PlanPage) ([]commerce.Plan, error) {
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list plans: %w", err)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, listPlans, string(page.After), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list plans: %w", err)
	}
	defer func() { _ = rows.Close() }()

	plans := make([]commerce.Plan, 0, limit)
	for rows.Next() {
		plan, scanErr := scanPlan(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("postgres: list plans: %w", scanErr)
		}
		plans = append(plans, plan)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list plans: %w", err)
	}
	return plans, nil
}

// ---------------------------------------------------------------------------
// subscriptions — the account's.
// ---------------------------------------------------------------------------

type accountSubscriptionsRepo struct {
	store persistence.Store
}

// The account predicate is first, the id keyset second. The cycle fields are
// selected as they are stored — NULL while the row is pending, set in every
// other state, which the schema's two CHECKs pin — and read into pointers so
// a pending row's absent cycle is an absent cycle and not a zero instant
// presented as a real period.
const listAccountSubscriptions = `
SELECT ` + subscriptionColumns + `
FROM control.subscriptions
WHERE account_id = $1
  AND id > $2
ORDER BY id
LIMIT $3`

func (r *accountSubscriptionsRepo) ListForAccount(ctx context.Context, accountID commerce.AccountID, page persistence.SubscriptionPage) ([]commerce.Subscription, error) {
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list subscriptions for account %s: %w", accountID, err)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, listAccountSubscriptions,
		string(accountID), string(page.After), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list subscriptions for account %s: %w", accountID, err)
	}
	defer func() { _ = rows.Close() }()

	subscriptions := make([]commerce.Subscription, 0, limit)
	for rows.Next() {
		subscription, scanErr := scanSubscription(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("postgres: list subscriptions for account %s: %w", accountID, scanErr)
		}
		subscriptions = append(subscriptions, subscription)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list subscriptions for account %s: %w", accountID, err)
	}
	return subscriptions, nil
}

// ---------------------------------------------------------------------------
// entitlements — the account's, through the subscription that owns each.
// ---------------------------------------------------------------------------

type accountEntitlementsRepo struct {
	store persistence.Store
}

// The account predicate is an EXISTS, and it is the first thing the WHERE
// says. The correlation is on subscription_id alone — the key the schema's
// foreign key gives — so the planner tests it per candidate grant and no row
// of another account's ever reaches the reader.
//
// The grant's own columns are returned whole. There is no balance column
// here and no member that computes one: capacity is drawn on funding
// buckets, and a "remaining" figure derived from a page of grants would be a
// third copy of a number with no rebuild story.
const listAccountEntitlements = `
SELECT ` + entitlementColumns + `
FROM control.entitlements
WHERE EXISTS (
        SELECT 1
        FROM control.subscriptions
        WHERE subscriptions.id = entitlements.subscription_id
          AND subscriptions.account_id = $1
      )
  AND id > $2
ORDER BY id
LIMIT $3`

func (r *accountEntitlementsRepo) ListForAccount(ctx context.Context, accountID commerce.AccountID, page persistence.EntitlementPage) ([]commerce.Entitlement, error) {
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list entitlements for account %s: %w", accountID, err)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, listAccountEntitlements,
		string(accountID), string(page.After), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list entitlements for account %s: %w", accountID, err)
	}
	defer func() { _ = rows.Close() }()

	entitlements := make([]commerce.Entitlement, 0, limit)
	for rows.Next() {
		entitlement, scanErr := scanEntitlement(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("postgres: list entitlements for account %s: %w", accountID, scanErr)
		}
		entitlements = append(entitlements, entitlement)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list entitlements for account %s: %w", accountID, err)
	}
	return entitlements, nil
}

// ---------------------------------------------------------------------------
// the row scanners, shared with ByID above so the two cannot drift.
// ---------------------------------------------------------------------------

// The three column lists, named once because a projection written twice is a
// projection that will differ. Each is the full readable row of its table and
// invents no column.
const (
	planColumns         = `id, name, created_at`
	subscriptionColumns = `id, account_id, plan_version_id, state, start_at, renewal_enabled, cancel_at, cancellation_mode, cycle_number, period_start, period_end, created_at, updated_at`
	entitlementColumns  = `id, subscription_id, cycle_number, grant_definition_id, alias_group_version_id, dimension, granted_amount, state, period_start, period_end, created_at, updated_at`
)

func scanPlan(row rowScanner) (commerce.Plan, error) {
	var plan commerce.Plan
	if err := row.Scan(&plan.ID, &plan.Name, &plan.CreatedAt); err != nil {
		return commerce.Plan{}, err
	}
	return plan, nil
}

// scanSubscription reads one subscriptions row. The four nullable columns
// (cancel_at, cancellation_mode, cycle_number, period_start, period_end —
// five, of which the contract shows three) each go into a sql.Null
// counterpart and are assigned to the domain's pointer field only when the
// database says they are set. A zero time assigned straight into a *time.Time
// would be a real instant presented for an absent one, and the console
// renders a subscription's cancel_at as a decision that has or has not taken
// effect.
func scanSubscription(row rowScanner) (commerce.Subscription, error) {
	var subscription commerce.Subscription
	var state string
	var renewalEnabled bool
	var cancelAt, periodStart, periodEnd sql.NullTime
	var cancellationMode sql.NullString
	var cycleNumber sql.NullInt64
	if err := row.Scan(&subscription.ID, &subscription.AccountID, &subscription.PlanVersionID,
		&state, &subscription.StartAt, &renewalEnabled, &cancelAt, &cancellationMode,
		&cycleNumber, &periodStart, &periodEnd, &subscription.CreatedAt, &subscription.UpdatedAt); err != nil {
		return commerce.Subscription{}, err
	}
	subscription.State = commerce.SubscriptionState(state)
	subscription.RenewalEnabled = renewalEnabled
	subscription.CancellationMode = commerce.CancellationMode(cancellationMode.String)
	if cancelAt.Valid {
		instant := cancelAt.Time
		subscription.CancelAt = &instant
	}
	if cycleNumber.Valid {
		cycle := int(cycleNumber.Int64)
		subscription.CycleNumber = &cycle
	}
	if periodStart.Valid {
		instant := periodStart.Time
		subscription.PeriodStart = &instant
	}
	if periodEnd.Valid {
		instant := periodEnd.Time
		subscription.PeriodEnd = &instant
	}
	return subscription, nil
}

// scanEntitlement reads one entitlements row. The dimension and the state are
// the schema's closed vocabularies read as text, and the scope is the
// alias-group VERSION id as stored: the contract's `scope` is a name an
// operator reads, and the console resolves it against the Data Plane that
// owns the catalogue — this query does not, and cannot.
func scanEntitlement(row rowScanner) (commerce.Entitlement, error) {
	var entitlement commerce.Entitlement
	var dimension, state string
	if err := row.Scan(&entitlement.ID, &entitlement.SubscriptionID, &entitlement.CycleNumber,
		&entitlement.GrantDefinitionID, &entitlement.AliasGroupVersionID, &dimension,
		&entitlement.GrantedAmount, &state, &entitlement.PeriodStart, &entitlement.PeriodEnd,
		&entitlement.CreatedAt, &entitlement.UpdatedAt); err != nil {
		return commerce.Entitlement{}, err
	}
	entitlement.Dimension = commerce.Dimension(dimension)
	entitlement.State = commerce.EntitlementState(state)
	return entitlement, nil
}
