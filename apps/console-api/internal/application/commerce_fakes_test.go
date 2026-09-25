package application

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The commerce fakes: one world, in the same spirit as the replay tests'
// fakeWorld, because the property under test is again a relationship — this
// time between the guarded statements, the unit of work that wraps them, and
// the cross-plane seam the roll resolves its scopes from before that unit
// opens. A fake that could not see the other two could not state the
// assertions that matter: that a guard's false verdict rolls back the
// entitlements inserted beside it, that a rollback leaves the row exactly as
// due as it was, and that no scope resolution ever happens inside a unit of
// work.
//
// The guards are not stubs. Each one implements the same predicate its SQL
// statement carries — the state, the cycle, the clock test, the renewal and
// cancellation gates, the owner account being active — with the world's clock
// standing in for transaction_timestamp(). That is what lets a test place
// start_at, period_end and cancel_at in the past and watch the real verdicts
// fire or refuse, which is the whole of what the lanes do.
type commerceWorld struct {
	order []string

	accounts map[identity.AccountID]identity.Account
	plans    map[commerce.PlanID]commerce.Plan
	versions map[commerce.PlanVersionID]commerce.PlanVersion
	defs     map[commerce.PlanVersionID][]commerce.GrantDefinition
	subs     map[commerce.SubscriptionID]commerce.Subscription
	ents     map[commerce.EntitlementID]commerce.Entitlement
	paygRows map[commerce.AccountID]commerce.AccountPayg
	// funded is the fake funder's durable state — the entitlements whose
	// cycle buckets and grant legs landed. It belongs to the same snapshot
	// story as the rest: a roll that fails after funding must unfund.
	funded map[commerce.EntitlementID]int64

	// now is the world's database clock: what transaction_timestamp() reads
	// inside a unit of work and what the scans compare against outside one.
	now time.Time

	// groupVersions is the fake catalog; a name missing from it answers
	// dataplane.ErrGroupNotFound, catalogErr a transport failure.
	groupVersions map[string]dataplane.GroupVersion
	catalogErr    error

	// failOn refuses one operation by key ("versions.create",
	// "entitlements.create", "subscriptions.dueRoll", ...), the way a
	// persistence failure arrives from below.
	failOn map[string]error

	// The race knobs. guardAccountInactive makes every guarded statement's
	// owner-account gate fail — the world moving between a use case's read
	// and its statement. competingVersionOnCreate models the other author
	// winning the open-version race: the first draft insert collides with a
	// row that lands beside it. stallPaygAssign makes the write-once bucket
	// assignment report a lost race without landing anything.
	guardAccountInactive     bool
	competingVersionOnCreate bool
	stallPaygAssign          bool

	// forceScanIDs makes every due-work scan return exactly these ids
	// regardless of state — the mechanical way to hand a lane a row that
	// moved on after the scan, which is the race the skips exist for.
	forceScanIDs []commerce.SubscriptionID

	scopeReads   []string
	scopeCounter int
}

// nextScopeID mints the fake catalog's deterministic, v7-shaped version ids.
func (w *commerceWorld) nextScopeID() string {
	w.scopeCounter++
	return fmt.Sprintf("0198f0a4-3f6c-7000-8000-%012x", w.scopeCounter)
}

func newCommerceWorld(t *testing.T) *commerceWorld {
	t.Helper()
	w := &commerceWorld{
		now:           time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		accounts:      map[identity.AccountID]identity.Account{},
		plans:         map[commerce.PlanID]commerce.Plan{},
		versions:      map[commerce.PlanVersionID]commerce.PlanVersion{},
		defs:          map[commerce.PlanVersionID][]commerce.GrantDefinition{},
		subs:          map[commerce.SubscriptionID]commerce.Subscription{},
		ents:          map[commerce.EntitlementID]commerce.Entitlement{},
		paygRows:      map[commerce.AccountID]commerce.AccountPayg{},
		funded:        map[commerce.EntitlementID]int64{},
		groupVersions: map[string]dataplane.GroupVersion{},
		failOn:        map[string]error{},
	}
	return w
}

// newCommerce wires the use case over one world, the way every commerce test
// builds it: all ten ports answer from the same state.
func newCommerce(w *commerceWorld) *Commerce {
	return NewCommerce(
		fakeCommerceStore{world: w},
		fakeCommerceAccounts{world: w},
		fakePlans{world: w},
		fakePlanVersions{world: w},
		fakeSubscriptions{world: w},
		fakeEntitlements{world: w},
		fakePaygAccounts{world: w},
		fakeClock{world: w},
		fakeCatalog{world: w},
		fakeFunder{world: w},
	)
}

// seedAccount plants an active account and returns its id.
func (w *commerceWorld) seedAccount(t *testing.T) identity.AccountID {
	t.Helper()
	id, err := identity.NewAccountID()
	if err != nil {
		t.Fatalf("identity.NewAccountID: %v", err)
	}
	account, err := identity.NewAccount(id, "seeded account", w.now)
	if err != nil {
		t.Fatalf("identity.NewAccount: %v", err)
	}
	w.accounts[id] = *account
	return id
}

// seedPublishedVersion plants a plan and a published version carrying the
// named grant definitions, and registers every scope's group version with the
// fake catalog (version 1 for each, the catalog's only version so far).
func (w *commerceWorld) seedPublishedVersion(t *testing.T, definitions ...commerce.GrantDefinition) (commerce.PlanID, commerce.PlanVersionID) {
	t.Helper()
	planID, err := commerce.NewPlanID()
	if err != nil {
		t.Fatalf("commerce.NewPlanID: %v", err)
	}
	plan, err := commerce.NewPlan(planID, "seeded plan "+string(planID), w.now)
	if err != nil {
		t.Fatalf("commerce.NewPlan: %v", err)
	}
	w.plans[planID] = *plan

	versionID, err := commerce.NewPlanVersionID()
	if err != nil {
		t.Fatalf("commerce.NewPlanVersionID: %v", err)
	}
	version, err := commerce.NewPlanVersion(versionID, planID, 1, commerce.RecurringPeriodCalendarMonth, 1900, w.now)
	if err != nil {
		t.Fatalf("commerce.NewPlanVersion: %v", err)
	}
	if err := version.Publish(w.now); err != nil {
		t.Fatalf("seed publish: %v", err)
	}
	w.versions[versionID] = *version
	w.defs[versionID] = definitions
	for _, definition := range definitions {
		w.groupVersions[string(definition.AliasGroupName)] = dataplane.GroupVersion{
			GroupName:      string(definition.AliasGroupName),
			Version:        1,
			GroupVersionID: w.nextScopeID(),
		}
	}
	return planID, versionID
}

// seedSubscription plants a pending subscription against versionID.
func (w *commerceWorld) seedSubscription(t *testing.T, accountID identity.AccountID, versionID commerce.PlanVersionID, startAt time.Time, renewal bool) commerce.Subscription {
	t.Helper()
	id, err := commerce.NewSubscriptionID()
	if err != nil {
		t.Fatalf("commerce.NewSubscriptionID: %v", err)
	}
	subscription, err := commerce.NewSubscription(id, commerce.AccountID(accountID), versionID, startAt, renewal, w.now)
	if err != nil {
		t.Fatalf("commerce.NewSubscription: %v", err)
	}
	w.subs[id] = *subscription
	return *subscription
}

func (w *commerceWorld) accountActive(accountID commerce.AccountID) bool {
	account, ok := w.accounts[identity.AccountID(accountID)]
	return ok && account.State == identity.AccountActive
}

// suspend moves a seeded account out of active — the world having moved
// between the use case's read and its guarded statement.
func (w *commerceWorld) suspend(accountID identity.AccountID) {
	account := w.accounts[accountID]
	account.State = identity.AccountSuspended
	w.accounts[accountID] = account
}

// moveOn drives a subscription into another state directly — the mechanical
// stand-in for another worker advancing the row between a scan and its
// guarded verdict.
func (w *commerceWorld) moveOn(subscriptionID commerce.SubscriptionID, state commerce.SubscriptionState) {
	subscription := w.subs[subscriptionID]
	subscription.State = state
	w.subs[subscriptionID] = subscription
}

func mustGrantDefinition(t *testing.T, versionID commerce.PlanVersionID, scope commerce.AliasGroupName) commerce.GrantDefinition {
	t.Helper()
	id, err := commerce.NewGrantDefinitionID()
	if err != nil {
		t.Fatalf("commerce.NewGrantDefinitionID: %v", err)
	}
	// A blank version id means "not attached yet" — the domain still demands
	// a well-formed reference, so the helper mints a throwaway one.
	if versionID == "" {
		versionID, err = commerce.NewPlanVersionID()
		if err != nil {
			t.Fatalf("commerce.NewPlanVersionID: %v", err)
		}
	}
	definition, err := commerce.NewGrantDefinition(id, versionID, scope, commerce.DimensionCost, 5000)
	if err != nil {
		t.Fatalf("commerce.NewGrantDefinition(%s): %v", scope, err)
	}
	return *definition
}

// fakeCommerceStore is the unit of work: it snapshots every map at begin and
// restores them on error, so a guard's false verdict demonstrably takes the
// entitlements inserted beside it down with the transaction.
type fakeCommerceStore struct {
	persistence.Store
	world *commerceWorld
}

func (s fakeCommerceStore) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	w := s.world
	w.order = append(w.order, "begin")

	accounts := copyMap(w.accounts)
	plans := copyMap(w.plans)
	versions := copyMap(w.versions)
	defs := make(map[commerce.PlanVersionID][]commerce.GrantDefinition, len(w.defs))
	for versionID, definitions := range w.defs {
		defs[versionID] = append([]commerce.GrantDefinition(nil), definitions...)
	}
	subs := copyMap(w.subs)
	ents := copyMap(w.ents)
	paygRows := copyMap(w.paygRows)
	funded := copyMap(w.funded)

	if err := fn(context.WithValue(ctx, txMarkerKey{}, true)); err != nil {
		w.accounts, w.plans, w.versions, w.defs, w.subs, w.ents, w.paygRows, w.funded =
			accounts, plans, versions, defs, subs, ents, paygRows, funded
		w.order = append(w.order, "rollback")
		return err
	}
	w.order = append(w.order, "commit")
	return nil
}

// InUnitOfWork answers the marker lookup WithinTx marks with — the same
// question the real store's member asks of the real context.
func (s fakeCommerceStore) InUnitOfWork(ctx context.Context) bool {
	return inTransaction(ctx)
}

func copyMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

type fakeCommerceAccounts struct {
	persistence.Accounts
	world *commerceWorld
}

func (f fakeCommerceAccounts) ByID(_ context.Context, id identity.AccountID) (identity.Account, error) {
	account, ok := f.world.accounts[id]
	if !ok {
		return identity.Account{}, persistence.ErrNotFound
	}
	return account, nil
}

func (f fakeCommerceAccounts) Create(_ context.Context, account identity.Account) error {
	f.world.accounts[account.ID] = account
	return nil
}

func (f fakeCommerceAccounts) TransitionState(_ context.Context, id identity.AccountID, from, to identity.AccountState, updatedAt time.Time) (bool, error) {
	account, ok := f.world.accounts[id]
	if !ok || account.State != from {
		return false, nil
	}
	account.State = to
	account.UpdatedAt = updatedAt
	f.world.accounts[id] = account
	return true, nil
}

type fakePlans struct {
	persistence.Plans
	world *commerceWorld
}

func (f fakePlans) Create(_ context.Context, plan commerce.Plan) error {
	if err := f.world.failOn["plans.create"]; err != nil {
		return err
	}
	for _, existing := range f.world.plans {
		if existing.Name == plan.Name {
			// The adapter's translation of plans_name_key.
			return commerce.ErrPlanNameTaken
		}
	}
	f.world.plans[plan.ID] = plan
	return nil
}

func (f fakePlans) ByID(_ context.Context, id commerce.PlanID) (commerce.Plan, error) {
	plan, ok := f.world.plans[id]
	if !ok {
		return commerce.Plan{}, persistence.ErrNotFound
	}
	return plan, nil
}

type fakePlanVersions struct {
	persistence.PlanVersions
	world *commerceWorld
}

func (f fakePlanVersions) Create(_ context.Context, version commerce.PlanVersion) error {
	if err := f.world.failOn["versions.create"]; err != nil {
		return err
	}
	for _, existing := range f.world.versions {
		if existing.PlanID == version.PlanID && existing.VersionNumber == version.VersionNumber {
			return commerce.ErrPlanVersionNumberTaken
		}
	}
	if f.world.competingVersionOnCreate {
		// The other author's row lands first: this attempt collides with it,
		// the competitor's draft stays, and the loser's retry reads the new
		// highest number — exactly the race the retry loop exists for.
		f.world.competingVersionOnCreate = false
		competitor := version
		id, err := commerce.NewPlanVersionID()
		if err != nil {
			return err
		}
		competitor.ID = id
		f.world.versions[id] = competitor
		return commerce.ErrPlanVersionNumberTaken
	}
	f.world.versions[version.ID] = version
	return nil
}

func (f fakePlanVersions) ByID(_ context.Context, id commerce.PlanVersionID) (commerce.PlanVersion, []commerce.GrantDefinition, error) {
	version, ok := f.world.versions[id]
	if !ok {
		return commerce.PlanVersion{}, nil, persistence.ErrNotFound
	}
	return version, append([]commerce.GrantDefinition(nil), f.world.defs[id]...), nil
}

func (f fakePlanVersions) AddGrantDefinition(_ context.Context, definition commerce.GrantDefinition) error {
	if err := f.world.failOn["definitions.add"]; err != nil {
		return err
	}
	version, ok := f.world.versions[definition.PlanVersionID]
	if !ok {
		return persistence.ErrNotFound
	}
	if version.State != commerce.PlanVersionDraft {
		// The statement's own draft guard.
		return commerce.ErrVersionNotEditable
	}
	for _, existing := range f.world.defs[definition.PlanVersionID] {
		if existing.AliasGroupName == definition.AliasGroupName && existing.Dimension == definition.Dimension {
			return commerce.ErrGrantScopeTaken
		}
	}
	f.world.defs[definition.PlanVersionID] = append(f.world.defs[definition.PlanVersionID], definition)
	return nil
}

func (f fakePlanVersions) HighestVersionNumber(_ context.Context, planID commerce.PlanID) (int, error) {
	highest := 0
	for _, version := range f.world.versions {
		if version.PlanID == planID && version.VersionNumber > highest {
			highest = version.VersionNumber
		}
	}
	return highest, nil
}

func (f fakePlanVersions) Publish(_ context.Context, id commerce.PlanVersionID, from commerce.PlanVersionState, publishedAt time.Time) (bool, error) {
	version, ok := f.world.versions[id]
	if !ok || version.State != from {
		return false, nil
	}
	version.State = commerce.PlanVersionPublished
	version.PublishedAt = &publishedAt
	version.UpdatedAt = publishedAt
	f.world.versions[id] = version
	// Logged on application only: the order log's publish entries count the
	// swaps, which is how the idempotence test tells a no-op from a rewrite.
	f.world.order = append(f.world.order, "versions.publish")
	return true, nil
}

func (f fakePlanVersions) Retire(_ context.Context, id commerce.PlanVersionID, from commerce.PlanVersionState, retiredAt time.Time) (bool, error) {
	version, ok := f.world.versions[id]
	if !ok || version.State != from {
		return false, nil
	}
	version.State = commerce.PlanVersionRetired
	version.RetiredAt = &retiredAt
	version.UpdatedAt = retiredAt
	f.world.versions[id] = version
	f.world.order = append(f.world.order, "versions.retire")
	return true, nil
}

type fakeSubscriptions struct {
	persistence.Subscriptions
	world *commerceWorld
}

func (f fakeSubscriptions) Create(_ context.Context, subscription commerce.Subscription) error {
	if err := f.world.failOn["subscriptions.create"]; err != nil {
		return err
	}
	f.world.subs[subscription.ID] = subscription
	return nil
}

func (f fakeSubscriptions) ByID(_ context.Context, id commerce.SubscriptionID) (commerce.Subscription, error) {
	subscription, ok := f.world.subs[id]
	if !ok {
		return commerce.Subscription{}, persistence.ErrNotFound
	}
	return subscription, nil
}

func (f fakeSubscriptions) ByIDForUpdate(ctx context.Context, id commerce.SubscriptionID) (commerce.Subscription, error) {
	return f.ByID(ctx, id)
}

func (f fakeSubscriptions) TransitionState(_ context.Context, id commerce.SubscriptionID, from, to commerce.SubscriptionState, updatedAt time.Time) (bool, error) {
	subscription, ok := f.world.subs[id]
	if !ok || subscription.State != from {
		return false, nil
	}
	subscription.State = to
	subscription.UpdatedAt = updatedAt
	f.world.subs[id] = subscription
	return true, nil
}

func (f fakeSubscriptions) ScheduleCancellation(_ context.Context, id commerce.SubscriptionID, cancelAt time.Time, updatedAt time.Time) (bool, error) {
	subscription, ok := f.world.subs[id]
	if !ok || subscription.State != commerce.SubscriptionActive {
		return false, nil
	}
	subscription.CancelAt = &cancelAt
	subscription.CancellationMode = commerce.CancellationScheduled
	subscription.UpdatedAt = updatedAt
	f.world.subs[id] = subscription
	return true, nil
}

func (f fakeSubscriptions) Cancel(_ context.Context, id commerce.SubscriptionID, cancelledAt time.Time) (bool, error) {
	subscription, ok := f.world.subs[id]
	if !ok || subscription.State != commerce.SubscriptionActive {
		return false, nil
	}
	subscription.CancelAt = &cancelledAt
	subscription.CancellationMode = commerce.CancellationImmediate
	subscription.State = commerce.SubscriptionCancelled
	subscription.UpdatedAt = cancelledAt
	f.world.subs[id] = subscription
	return true, nil
}

// CompleteScheduledCancellation is the cancellation lane's verdict,
// predicate for predicate with the statement: the instruction exactly as the
// scan read it — scheduled, and due on the world's clock — still on file.
// An instruction rescheduled after the scan (a later CancelAt, or a
// different mode) leaves this false, and the row stays what the customer's
// latest instruction made it.
func (f fakeSubscriptions) CompleteScheduledCancellation(_ context.Context, id commerce.SubscriptionID, updatedAt time.Time) (bool, error) {
	subscription, ok := f.world.subs[id]
	if !ok ||
		subscription.State != commerce.SubscriptionActive ||
		subscription.CancellationMode != commerce.CancellationScheduled ||
		subscription.CancelAt == nil ||
		subscription.CancelAt.After(f.world.now) {
		return false, nil
	}
	subscription.State = commerce.SubscriptionCancelled
	subscription.UpdatedAt = updatedAt
	f.world.subs[id] = subscription
	return true, nil
}

// Activate is the promotion's guarded verdict, predicate for predicate with
// the statement: pending, start arrived on the world's clock, owner active.
func (f fakeSubscriptions) Activate(_ context.Context, id commerce.SubscriptionID, periodStart, periodEnd, updatedAt time.Time) (bool, error) {
	subscription, ok := f.world.subs[id]
	if !ok ||
		subscription.State != commerce.SubscriptionPending ||
		subscription.StartAt.After(f.world.now) ||
		!f.world.accountActive(subscription.AccountID) ||
		f.world.guardAccountInactive {
		return false, nil
	}
	subscription.State = commerce.SubscriptionActive
	cycle := 1
	subscription.CycleNumber = &cycle
	subscription.PeriodStart = &periodStart
	subscription.PeriodEnd = &periodEnd
	subscription.UpdatedAt = updatedAt
	f.world.subs[id] = subscription
	return true, nil
}

// Roll is the cycle roll's guarded verdict: same state and cycle the caller
// read, period ended on the world's clock, renewal on, the scheduled
// cancellation surviving past the new cycle, owner active.
func (f fakeSubscriptions) Roll(_ context.Context, id commerce.SubscriptionID, fromCycle int, periodStart, periodEnd, updatedAt time.Time) (bool, error) {
	subscription, ok := f.world.subs[id]
	if !ok ||
		subscription.State != commerce.SubscriptionActive ||
		subscription.CycleNumber == nil ||
		*subscription.CycleNumber != fromCycle ||
		subscription.PeriodEnd == nil ||
		subscription.PeriodEnd.After(f.world.now) ||
		!subscription.RenewalEnabled ||
		(subscription.CancelAt != nil && !subscription.CancelAt.After(periodEnd)) ||
		!f.world.accountActive(subscription.AccountID) ||
		f.world.guardAccountInactive {
		return false, nil
	}
	next := fromCycle + 1
	subscription.CycleNumber = &next
	subscription.PeriodStart = &periodStart
	subscription.PeriodEnd = &periodEnd
	subscription.UpdatedAt = updatedAt
	f.world.subs[id] = subscription
	return true, nil
}

// Expire is the fixed-term expiry's guarded verdict.
func (f fakeSubscriptions) Expire(_ context.Context, id commerce.SubscriptionID, updatedAt time.Time) (bool, error) {
	subscription, ok := f.world.subs[id]
	if !ok ||
		(subscription.State != commerce.SubscriptionActive && subscription.State != commerce.SubscriptionSuspended) ||
		subscription.RenewalEnabled ||
		subscription.PeriodEnd == nil ||
		subscription.PeriodEnd.After(f.world.now) ||
		subscription.CancelAt != nil {
		return false, nil
	}
	subscription.State = commerce.SubscriptionExpired
	subscription.UpdatedAt = updatedAt
	f.world.subs[id] = subscription
	return true, nil
}

func (f fakeSubscriptions) scanIDs(limit int, order func(a, b commerce.Subscription) bool, predicate func(commerce.Subscription) bool) ([]commerce.SubscriptionID, error) {
	if f.world.forceScanIDs != nil {
		return f.world.forceScanIDs, nil
	}
	var matched []commerce.Subscription
	for _, subscription := range f.world.subs {
		if predicate(subscription) {
			matched = append(matched, subscription)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return order(matched[i], matched[j]) })
	if len(matched) > limit {
		matched = matched[:limit]
	}
	ids := make([]commerce.SubscriptionID, 0, len(matched))
	for _, subscription := range matched {
		ids = append(ids, subscription.ID)
	}
	return ids, nil
}

func (f fakeSubscriptions) DuePromotionIDs(_ context.Context, limit int) ([]commerce.SubscriptionID, error) {
	if err := f.world.failOn["subscriptions.duePromotion"]; err != nil {
		return nil, err
	}
	return f.scanIDs(limit, func(a, b commerce.Subscription) bool { return a.StartAt.Before(b.StartAt) }, func(s commerce.Subscription) bool {
		return s.State == commerce.SubscriptionPending && !s.StartAt.After(f.world.now) &&
			f.world.accountActive(s.AccountID)
	})
}

func (f fakeSubscriptions) DueRollIDs(_ context.Context, limit int) ([]commerce.SubscriptionID, error) {
	if err := f.world.failOn["subscriptions.dueRoll"]; err != nil {
		return nil, err
	}
	return f.scanIDs(limit, func(a, b commerce.Subscription) bool { return a.PeriodEnd.Before(*b.PeriodEnd) }, func(s commerce.Subscription) bool {
		return s.State == commerce.SubscriptionActive && s.RenewalEnabled &&
			s.PeriodEnd != nil && !s.PeriodEnd.After(f.world.now) &&
			f.world.accountActive(s.AccountID)
	})
}

func (f fakeSubscriptions) DueCancellationIDs(_ context.Context, limit int) ([]commerce.SubscriptionID, error) {
	if err := f.world.failOn["subscriptions.dueCancellation"]; err != nil {
		return nil, err
	}
	return f.scanIDs(limit, func(a, b commerce.Subscription) bool { return a.CancelAt.Before(*b.CancelAt) }, func(s commerce.Subscription) bool {
		return s.State == commerce.SubscriptionActive && s.CancellationMode == commerce.CancellationScheduled &&
			s.CancelAt != nil && !s.CancelAt.After(f.world.now)
	})
}

func (f fakeSubscriptions) DueExpiryIDs(_ context.Context, limit int) ([]commerce.SubscriptionID, error) {
	if err := f.world.failOn["subscriptions.dueExpiry"]; err != nil {
		return nil, err
	}
	return f.scanIDs(limit, func(a, b commerce.Subscription) bool { return a.PeriodEnd.Before(*b.PeriodEnd) }, func(s commerce.Subscription) bool {
		return (s.State == commerce.SubscriptionActive || s.State == commerce.SubscriptionSuspended) &&
			!s.RenewalEnabled && s.PeriodEnd != nil && !s.PeriodEnd.After(f.world.now) && s.CancelAt == nil
	})
}

type fakeEntitlements struct {
	persistence.Entitlements
	world *commerceWorld
}

func (f fakeEntitlements) Create(_ context.Context, entitlement commerce.Entitlement) error {
	if err := f.world.failOn["entitlements.create"]; err != nil {
		return err
	}
	for _, existing := range f.world.ents {
		if existing.SubscriptionID == entitlement.SubscriptionID &&
			existing.CycleNumber == entitlement.CycleNumber &&
			existing.GrantDefinitionID == entitlement.GrantDefinitionID {
			return fmt.Errorf("fakes: entitlement %s duplicates subscription %s cycle %d definition %s",
				entitlement.ID, entitlement.SubscriptionID, entitlement.CycleNumber, entitlement.GrantDefinitionID)
		}
	}
	f.world.ents[entitlement.ID] = entitlement
	return nil
}

func (f fakeEntitlements) Expire(_ context.Context, id commerce.EntitlementID, updatedAt time.Time) (bool, error) {
	entitlement, ok := f.world.ents[id]
	if !ok || entitlement.State != commerce.EntitlementActive || entitlement.PeriodEnd.After(f.world.now) {
		return false, nil
	}
	entitlement.State = commerce.EntitlementExpired
	entitlement.UpdatedAt = updatedAt
	f.world.ents[id] = entitlement
	return true, nil
}

func (f fakeEntitlements) DueExpiryIDs(_ context.Context, limit int) ([]commerce.EntitlementID, error) {
	if err := f.world.failOn["entitlements.dueExpiry"]; err != nil {
		return nil, err
	}
	var matched []commerce.Entitlement
	for _, entitlement := range f.world.ents {
		if entitlement.State == commerce.EntitlementActive && !entitlement.PeriodEnd.After(f.world.now) {
			matched = append(matched, entitlement)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].PeriodEnd.Before(matched[j].PeriodEnd) })
	if len(matched) > limit {
		matched = matched[:limit]
	}
	ids := make([]commerce.EntitlementID, 0, len(matched))
	for _, entitlement := range matched {
		ids = append(ids, entitlement.ID)
	}
	return ids, nil
}

func (f fakeEntitlements) ActiveCandidates(_ context.Context, accountID commerce.AccountID) ([]commerce.CandidateGrant, error) {
	if err := f.world.failOn["entitlements.candidates"]; err != nil {
		return nil, err
	}
	var candidates []commerce.CandidateGrant
	for _, entitlement := range f.world.ents {
		if entitlement.State != commerce.EntitlementActive {
			continue
		}
		subscription, ok := f.world.subs[entitlement.SubscriptionID]
		if !ok || subscription.State != commerce.SubscriptionActive {
			continue
		}
		if subscription.AccountID != accountID {
			continue
		}
		var scope commerce.AliasGroupName
		for _, definition := range f.world.defs[subscription.PlanVersionID] {
			if definition.ID == entitlement.GrantDefinitionID {
				scope = definition.AliasGroupName
			}
		}
		ent := entitlement
		candidates = append(candidates, commerce.CandidateGrant{
			Entitlement:           &ent,
			SubscriptionCreatedAt: subscription.CreatedAt,
			AliasGroupName:        scope,
		})
	}
	return candidates, nil
}

type fakePaygAccounts struct {
	persistence.PaygAccounts
	world *commerceWorld
}

func (f fakePaygAccounts) ByAccount(_ context.Context, accountID commerce.AccountID) (commerce.AccountPayg, error) {
	payg, ok := f.world.paygRows[accountID]
	if !ok {
		return commerce.AccountPayg{}, persistence.ErrNotFound
	}
	return payg, nil
}

func (f fakePaygAccounts) SetEnabled(_ context.Context, accountID commerce.AccountID, enabled bool, now time.Time) error {
	payg, ok := f.world.paygRows[accountID]
	if !ok {
		payg = commerce.AccountPayg{AccountID: accountID, CreatedAt: now}
	}
	payg.Enabled = enabled
	payg.UpdatedAt = now
	f.world.paygRows[accountID] = payg
	return nil
}

// AssignFundingBucket models the port's insert-or-update: an absent row is
// created around the reference with PAYG off, a row whose reference is unset
// takes it, and a row that already carries a reference is left untouched and
// answers false — the caller re-reads to converge or to name the conflict.
func (f fakePaygAccounts) AssignFundingBucket(_ context.Context, accountID commerce.AccountID, bucketID commerce.FundingBucketID, updatedAt time.Time) (bool, error) {
	if f.world.stallPaygAssign {
		return false, nil
	}
	payg, ok := f.world.paygRows[accountID]
	switch {
	case !ok:
		payg = commerce.AccountPayg{AccountID: accountID, CreatedAt: updatedAt}
	case payg.FundingBucketID != "":
		return false, nil
	}
	payg.FundingBucketID = bucketID
	payg.UpdatedAt = updatedAt
	f.world.paygRows[accountID] = payg
	return true, nil
}

type fakeClock struct {
	persistence.Clock
	world *commerceWorld
}

func (f fakeClock) Now(_ context.Context) (time.Time, error) {
	if err := f.world.failOn["clock.now"]; err != nil {
		return time.Time{}, err
	}
	return f.world.now, nil
}

// fakeFunder is the accounting seam's stand-in: it records each grant into
// the world's durable funded map — snapshot and rollback apply to it, which
// is what makes a failed roll demonstrably unfund what it funded — and
// refuses by the funder.failOn key the way a real accounting refusal would
// arrive.
type fakeFunder struct {
	world *commerceWorld
}

func (f fakeFunder) FundEntitlement(_ context.Context, entitlementID commerce.EntitlementID, grantedAmount int64) error {
	if err := f.world.failOn["funder.fund"]; err != nil {
		return err
	}
	if _, ok := f.world.funded[entitlementID]; ok {
		return nil // a retried roll converges on the bucket already on file
	}
	f.world.funded[entitlementID] = grantedAmount
	return nil
}

type fakeCatalog struct {
	dataplane.CatalogReader
	world *commerceWorld
}

func (f fakeCatalog) CurrentGroupVersion(_ context.Context, groupName string) (dataplane.GroupVersion, error) {
	f.world.scopeReads = append(f.world.scopeReads, groupName)
	if f.world.catalogErr != nil {
		return dataplane.GroupVersion{}, f.world.catalogErr
	}
	groupVersion, ok := f.world.groupVersions[groupName]
	if !ok {
		return dataplane.GroupVersion{}, dataplane.ErrGroupNotFound
	}
	return groupVersion, nil
}
