//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The commerce repositories against the real `control` database, on the same
// terms as the identity suite above: the fake-driver tests pin which handle a
// statement runs on and how a constraint name becomes the port's vocabulary;
// only PostgreSQL can answer the rest — that the migrations' CHECKs and
// unique constraints actually refuse what they must refuse, that the guarded
// moves are single statements whose verdicts are true exactly once and false
// without error when any one predicate fails, that the due-work scans and the
// database's own clock agree on what is due, and that a cycle advance and the
// entitlements granted in its unit of work land as one fact. What is
// deliberately NOT re-proven here is the pure SQL-level schema behaviour:
// deploy/postgres/verify.sh owns that tier.
//
// Conventions this file holds: no t.Parallel anywhere (the database is
// shared); every id and name is run-unique, minted by the domain's v7
// minters, so a rerun against an already-migrated database cannot trip over
// an earlier run's rows — leftovers are tolerated, never depended on; and
// fixtures reach their states through the ports wherever a port path exists
// (the subscription lifecycle, plan-version publication, the entitlement
// expiry), because a fixture the port itself built is one more proof the
// states compose. Nothing here deletes: the schema has no delete path and
// the ids carry the isolation.
//
// Run (the compose project is port-shifted per worktree):
//
//	GATEWAY_POSTGRES_PROJECT=... GATEWAY_POSTGRES_PORT=... docker compose -f deploy/postgres/compose.yaml up -d --wait
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:PORT/postgres?sslmode=disable' \
//	  go test -tags=integration ./internal/adapters/outbound/postgres

// commerceRepos gathers the pool and every commerce repository over it, as
// the ports the use cases see them — plus the identity Accounts repository,
// because a subscription's owner and a PAYG flag's key are identity rows,
// and the account gate the guarded moves carry is a read of that row's
// state.
type commerceRepos struct {
	db            *sql.DB
	store         persistence.Store
	accounts      persistence.Accounts
	plans         persistence.Plans
	versions      persistence.PlanVersions
	subscriptions persistence.Subscriptions
	entitlements  persistence.Entitlements
	payg          persistence.PaygAccounts
	clock         persistence.Clock
}

// integrationCommerce opens the migrated control database and builds the
// commerce repositories over it.
func integrationCommerce(t *testing.T) *commerceRepos {
	t.Helper()
	db := integrationDB(t)
	store := New(db)
	return &commerceRepos{
		db:            db,
		store:         store,
		accounts:      NewAccounts(store),
		plans:         NewPlans(store),
		versions:      NewPlanVersions(store),
		subscriptions: NewSubscriptions(store),
		entitlements:  NewEntitlements(store),
		payg:          NewPaygAccounts(store),
		clock:         NewClock(store),
	}
}

// micros is the precision timestamptz guarantees: values travel through the
// port and the server at microseconds, so an exact time.Equal against a
// Go nanosecond instant would fail on rounding the server did, not on any
// difference the adapter made.
func micros(t time.Time) time.Time { return t.Truncate(time.Microsecond) }

// uniquePlanName returns an operator-facing plan name no earlier run has
// taken — the v7 minter's randomness is the run-uniqueness, the same trick
// the identity suite plays with emails.
func uniquePlanName(t *testing.T) string {
	t.Helper()
	suffix, err := commerce.NewPlanID()
	if err != nil {
		t.Fatalf("NewPlanID: %v", err)
	}
	return "it-commerce-" + strings.ReplaceAll(string(suffix), "-", "")[:16]
}

// integrationCommerceAccount creates one active account through the identity
// port — accounts first, because every commerce foreign key is RESTRICT and
// the owner must exist before anything points at it — carried in commerce's
// own spelling of the id.
func integrationCommerceAccount(t *testing.T, c *commerceRepos, name string) commerce.AccountID {
	t.Helper()
	account := integrationAccount(t, c.accounts, name)
	return commerce.AccountID(account.ID)
}

// newIntegrationPlan creates a plan root through the port.
func newIntegrationPlan(t *testing.T, c *commerceRepos) *commerce.Plan {
	t.Helper()
	id, err := commerce.NewPlanID()
	if err != nil {
		t.Fatalf("NewPlanID: %v", err)
	}
	plan, err := commerce.NewPlan(id, uniquePlanName(t), time.Now().UTC())
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if err := c.plans.Create(t.Context(), *plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return plan
}

// openDraftVersion opens a draft version numbered one past the plan's
// highest — the `highest + 1` discipline HighestVersionNumber exists to
// serve — through the port.
func openDraftVersion(t *testing.T, c *commerceRepos, planID commerce.PlanID) *commerce.PlanVersion {
	t.Helper()
	highest, err := c.versions.HighestVersionNumber(t.Context(), planID)
	if err != nil {
		t.Fatalf("highest version number: %v", err)
	}
	id, err := commerce.NewPlanVersionID()
	if err != nil {
		t.Fatalf("NewPlanVersionID: %v", err)
	}
	version, err := commerce.NewPlanVersion(id, planID, highest+1, commerce.RecurringPeriodCalendarMonth, 4900, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewPlanVersion: %v", err)
	}
	if err := c.versions.Create(t.Context(), *version); err != nil {
		t.Fatalf("create plan version: %v", err)
	}
	return version
}

// addDefinition adds one grant definition to a version that still reads
// draft, through the port, with a minted id.
func addDefinition(t *testing.T, c *commerceRepos, versionID commerce.PlanVersionID, scope commerce.AliasGroupName, amount int64) *commerce.GrantDefinition {
	t.Helper()
	id, err := commerce.NewGrantDefinitionID()
	if err != nil {
		t.Fatalf("NewGrantDefinitionID: %v", err)
	}
	definition, err := commerce.NewGrantDefinition(id, versionID, scope, commerce.DimensionCost, amount)
	if err != nil {
		t.Fatalf("NewGrantDefinition: %v", err)
	}
	if err := c.versions.AddGrantDefinition(t.Context(), *definition); err != nil {
		t.Fatalf("add grant definition %s: %v", scope, err)
	}
	return definition
}

// publishVersionWithDefinitions composes a published version the way the
// domain demands it be composed: definitions added while the version still
// reads draft — the edit guard refuses anything added after publication —
// and then the one-way publish. Amounts are distinct so a test can tell the
// definitions apart by what they grant.
func publishVersionWithDefinitions(t *testing.T, c *commerceRepos, planID commerce.PlanID, scopes ...commerce.AliasGroupName) (*commerce.PlanVersion, []*commerce.GrantDefinition) {
	t.Helper()
	version := openDraftVersion(t, c, planID)
	definitions := make([]*commerce.GrantDefinition, 0, len(scopes))
	for i, scope := range scopes {
		definitions = append(definitions, addDefinition(t, c, version.ID, scope, int64(1000+i)))
	}
	applied, err := c.versions.Publish(t.Context(), version.ID, commerce.PlanVersionDraft, time.Now().UTC())
	if err != nil || !applied {
		t.Fatalf("publish the composed version = (%t, %v), want (true, nil)", applied, err)
	}
	return version, definitions
}

// pendingSubscription creates a subscription in its birth state through the
// port. startAt decides when the promotion becomes due: the database clock,
// not this process's, judges it, so a past start_at is what makes a
// subscription due within this test's run.
func pendingSubscription(t *testing.T, c *commerceRepos, accountID commerce.AccountID, versionID commerce.PlanVersionID, startAt time.Time, renewal bool) *commerce.Subscription {
	t.Helper()
	id, err := commerce.NewSubscriptionID()
	if err != nil {
		t.Fatalf("NewSubscriptionID: %v", err)
	}
	subscription, err := commerce.NewSubscription(id, accountID, versionID, startAt, renewal, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewSubscription: %v", err)
	}
	if err := c.subscriptions.Create(t.Context(), *subscription); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	return subscription
}

// matureStartAt returns a start_at two calendar months and one hour ago: its
// first cycle ended an hour before the test ran, so the promotion is due
// immediately and the first roll is due the moment it lands — while the
// second cycle, ending an hour from now, is not.
func matureStartAt(now time.Time) time.Time {
	return now.AddDate(0, -2, 0).Add(-time.Hour)
}

// liveStartAt returns a start_at a day ago: its first cycle covers the
// present, which is what the derivation's coverage test needs.
func liveStartAt(now time.Time) time.Time {
	return now.AddDate(0, 0, -1)
}

// mustCycleBounds is CycleBounds with the error already fatal — every anchor
// in these tests is a value the domain has already accepted.
func mustCycleBounds(t *testing.T, anchor time.Time, cycle int) (time.Time, time.Time) {
	t.Helper()
	start, end, err := commerce.CycleBounds(anchor, cycle)
	if err != nil {
		t.Fatalf("cycle %d bounds of %s: %v", cycle, anchor, err)
	}
	return start, end
}

// activate runs the promotion with the cycle-1 bounds the domain's own
// arithmetic computes from the subscription's anchor, and returns the
// subscription as the database now holds it.
func activate(t *testing.T, c *commerceRepos, subscription *commerce.Subscription) *commerce.Subscription {
	t.Helper()
	periodStart, periodEnd := mustCycleBounds(t, subscription.StartAt, 1)
	applied, err := c.subscriptions.Activate(t.Context(), subscription.ID, periodStart, periodEnd, time.Now().UTC())
	if err != nil || !applied {
		t.Fatalf("activate a due pending subscription = (%t, %v), want (true, nil)", applied, err)
	}
	stored, err := c.subscriptions.ByID(t.Context(), subscription.ID)
	if err != nil {
		t.Fatalf("read the promoted subscription back: %v", err)
	}
	return &stored
}

// lostRoll is the sentinel rollOnce returns when the guarded advance
// matched nothing: the unit of work must roll back, taking the cycle's
// entitlement with it — the doctrine the port states and this helper
// follows, so a lost roll never leaves a grant behind.
var lostRoll = errors.New("integration: the roll's swap was lost")

// rollOnce advances one subscription from its current cycle to the next,
// inside a unit of work shaped the way the roll lane's is: the row is read
// FOR UPDATE, the clock is the transaction's, the new cycle's entitlement is
// inserted in the same unit of work, and the guarded advance commits or
// takes that insert back with it. The cycle's bounds come from the row's own
// anchor by the domain's arithmetic — the timeline-tiling intervals the
// subscription was born with — while the database clock judges, inside the
// statement, whether the cycle being left has ended.
func rollOnce(t *testing.T, c *commerceRepos, subscriptionID commerce.SubscriptionID, definition *commerce.GrantDefinition) bool {
	t.Helper()
	var applied bool
	err := c.store.WithinTx(t.Context(), func(ctx context.Context) error {
		current, err := c.subscriptions.ByIDForUpdate(ctx, subscriptionID)
		if err != nil {
			return err
		}
		if current.CycleNumber == nil {
			return fmt.Errorf("subscription %s is active with no cycle to roll from", subscriptionID)
		}
		now, err := c.clock.Now(ctx)
		if err != nil {
			return err
		}
		periodStart, periodEnd := mustCycleBounds(t, current.StartAt, *current.CycleNumber+1)
		entitlement := grantCycle(t, current.ID, *current.CycleNumber+1, definition, periodStart, periodEnd)
		if err := c.entitlements.Create(ctx, *entitlement); err != nil {
			return err
		}
		applied, err = c.subscriptions.Roll(ctx, current.ID, *current.CycleNumber, periodStart, periodEnd, now)
		if err != nil {
			return err
		}
		if !applied {
			return lostRoll
		}
		return nil
	})
	if err != nil && !errors.Is(err, lostRoll) {
		t.Fatalf("roll subscription %s: %v", subscriptionID, err)
	}
	return applied
}

// grantCycle returns the entitlement a roll materialises for one cycle of
// one definition, through the domain. The scope version is a stand-in for
// the catalog snapshot the roll resolves: it lives in the Data Plane's
// database, and the v7 form is all the control database promises about it.
func grantCycle(t *testing.T, subscriptionID commerce.SubscriptionID, cycle int, definition *commerce.GrantDefinition, periodStart, periodEnd time.Time) *commerce.Entitlement {
	t.Helper()
	id, err := commerce.NewEntitlementID()
	if err != nil {
		t.Fatalf("NewEntitlementID: %v", err)
	}
	scopeVersion, err := commerce.NewEntitlementID()
	if err != nil {
		t.Fatalf("NewEntitlementID (scope version): %v", err)
	}
	entitlement, err := commerce.NewEntitlement(id, subscriptionID, cycle, *definition,
		commerce.AliasGroupVersionID(scopeVersion), periodStart, periodEnd, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewEntitlement: %v", err)
	}
	return entitlement
}

// TestIntegrationCommercePlansRoundTripWhole covers the plan root's reads:
// create, read back whole, and the miss that is ErrNotFound — the port's
// sentinel, not the driver's.
func TestIntegrationCommercePlansRoundTripWhole(t *testing.T) {
	c := integrationCommerce(t)
	ctx := t.Context()

	plan := newIntegrationPlan(t, c)
	stored, err := c.plans.ByID(ctx, plan.ID)
	if err != nil {
		t.Fatalf("read the plan back: %v", err)
	}
	if stored.ID != plan.ID || stored.Name != plan.Name || !micros(stored.CreatedAt).Equal(micros(plan.CreatedAt)) {
		t.Fatalf("read back = %+v, want the stored plan whole: id %s, name %q, created %s", stored, plan.ID, plan.Name, micros(plan.CreatedAt))
	}

	// A miss is ErrNotFound. The id is freshly minted: absent by
	// construction, and a well-formed uuid, so the database answers no row
	// rather than a syntax error.
	if _, err := c.plans.ByID(ctx, commerce.PlanID(mustAbsentPlanID(t))); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("a missing plan returned %v, want persistence.ErrNotFound", err)
	}
}

// TestIntegrationCommercePlanNameUniquenessSurfacesAsPlanNameTaken pins the
// first of the three constraint translations: two plans, one name, and the
// database's refusal arriving as the domain's word for it.
func TestIntegrationCommercePlanNameUniquenessSurfacesAsPlanNameTaken(t *testing.T) {
	c := integrationCommerce(t)

	name := uniquePlanName(t)
	firstID, err := commerce.NewPlanID()
	if err != nil {
		t.Fatalf("NewPlanID: %v", err)
	}
	first, err := commerce.NewPlan(firstID, name, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if err := c.plans.Create(t.Context(), *first); err != nil {
		t.Fatalf("create the first plan: %v", err)
	}

	secondID, err := commerce.NewPlanID()
	if err != nil {
		t.Fatalf("NewPlanID: %v", err)
	}
	second, err := commerce.NewPlan(secondID, name, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if err := c.plans.Create(t.Context(), *second); !errors.Is(err, commerce.ErrPlanNameTaken) {
		t.Fatalf("a second plan named %q returned %v, want commerce.ErrPlanNameTaken", name, err)
	}
}

// TestIntegrationCommerceVersionNumbersAreUniquePerPlan pins the second
// constraint translation and the 0-to-1 discipline of HighestVersionNumber.
func TestIntegrationCommerceVersionNumbersAreUniquePerPlan(t *testing.T) {
	c := integrationCommerce(t)
	ctx := t.Context()
	plan := newIntegrationPlan(t, c)

	highest, err := c.versions.HighestVersionNumber(ctx, plan.ID)
	if err != nil || highest != 0 {
		t.Fatalf("a new plan's highest version number = (%d, %v), want (0, nil)", highest, err)
	}

	first := openDraftVersion(t, c, plan.ID)
	second := openDraftVersion(t, c, plan.ID)
	if first.VersionNumber != 1 || second.VersionNumber != 2 {
		t.Fatalf("open versions numbered %d and %d, want 1 and 2 — highest + 1 with no special case", first.VersionNumber, second.VersionNumber)
	}
	if highest, err := c.versions.HighestVersionNumber(ctx, plan.ID); err != nil || highest != 2 {
		t.Fatalf("highest version number after two drafts = (%d, %v), want (2, nil)", highest, err)
	}

	// The open-version race: a second attempt at number 2 loses to the
	// schema's plan_versions_version_number_key, and the adapter names it.
	duplicateID, err := commerce.NewPlanVersionID()
	if err != nil {
		t.Fatalf("NewPlanVersionID: %v", err)
	}
	duplicate, err := commerce.NewPlanVersion(duplicateID, plan.ID, 2, commerce.RecurringPeriodCalendarMonth, 4900, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewPlanVersion: %v", err)
	}
	if err := c.versions.Create(ctx, *duplicate); !errors.Is(err, commerce.ErrPlanVersionNumberTaken) {
		t.Fatalf("a duplicate version number returned %v, want commerce.ErrPlanVersionNumberTaken", err)
	}

	if _, _, err := c.versions.ByID(ctx, commerce.PlanVersionID(mustAbsentPlanID(t))); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("a missing plan version returned %v, want persistence.ErrNotFound", err)
	}
}

// TestIntegrationCommerceGrantDefinitionsTravelWholeAndDraftOnly walks the
// editing rules in one version's life: definitions added while draft, the
// duplicate scope refused by the schema and named by the adapter, the whole
// set read back in a stable order, the publication that freezes it, the edit
// that loses to the freeze, and the retirement that closes the machine.
func TestIntegrationCommerceGrantDefinitionsTravelWholeAndDraftOnly(t *testing.T) {
	c := integrationCommerce(t)
	ctx := t.Context()
	plan := newIntegrationPlan(t, c)
	version := openDraftVersion(t, c, plan.ID)

	named := addDefinition(t, c, version.ID, "it-commerce-group-a", 1000)
	wildcard := addDefinition(t, c, version.ID, commerce.WildcardGroupName, 100)

	// One (scope, dimension) per version: the schema refuses a second
	// definition for the same scope, and the adapter names the rule.
	duplicateID, err := commerce.NewGrantDefinitionID()
	if err != nil {
		t.Fatalf("NewGrantDefinitionID: %v", err)
	}
	duplicate, err := commerce.NewGrantDefinition(duplicateID, version.ID, "it-commerce-group-a", commerce.DimensionCost, 5)
	if err != nil {
		t.Fatalf("NewGrantDefinition: %v", err)
	}
	if err := c.versions.AddGrantDefinition(ctx, *duplicate); !errors.Is(err, commerce.ErrGrantScopeTaken) {
		t.Fatalf("a duplicate scope returned %v, want commerce.ErrGrantScopeTaken", err)
	}

	stored, definitions, err := c.versions.ByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("read the version back: %v", err)
	}
	if stored.ID != version.ID || stored.State != commerce.PlanVersionDraft || stored.PublishedAt != nil || stored.RetiredAt != nil {
		t.Fatalf("draft version read back = %+v, want draft with both stamps unset", stored)
	}
	if len(definitions) != 2 {
		t.Fatalf("the version read back with %d definitions, want 2 — the set travels whole", len(definitions))
	}
	if definitions[0].ID != named.ID || definitions[1].ID != wildcard.ID {
		t.Fatalf("definitions read back as [%s, %s], want the stable id order the query promises", definitions[0].ID, definitions[1].ID)
	}
	if definitions[0].PlanVersionID != version.ID || definitions[0].AliasGroupName != "it-commerce-group-a" ||
		definitions[0].Dimension != commerce.DimensionCost || definitions[0].GrantedAmount != 1000 {
		t.Fatalf("definition read back = %+v, want the whole row the port inserted", definitions[0])
	}

	// Publication freezes the terms, one applied swap.
	applied, err := c.versions.Publish(ctx, version.ID, commerce.PlanVersionDraft, time.Now().UTC())
	if err != nil || !applied {
		t.Fatalf("publish the draft = (%t, %v), want (true, nil)", applied, err)
	}
	applied, err = c.versions.Publish(ctx, version.ID, commerce.PlanVersionDraft, time.Now().UTC())
	if err != nil || applied {
		t.Fatalf("a second publish from draft = (%t, %v), want (false, nil) — the row no longer reads draft", applied, err)
	}

	// The edit racing the publication loses cleanly: the statement's SELECT
	// found no version still in draft, and the adapter names that verdict.
	lateID, err := commerce.NewGrantDefinitionID()
	if err != nil {
		t.Fatalf("NewGrantDefinitionID: %v", err)
	}
	late, err := commerce.NewGrantDefinition(lateID, version.ID, "it-commerce-group-b", commerce.DimensionCost, 7)
	if err != nil {
		t.Fatalf("NewGrantDefinition: %v", err)
	}
	if err := c.versions.AddGrantDefinition(ctx, *late); !errors.Is(err, commerce.ErrVersionNotEditable) {
		t.Fatalf("an edit after publication returned %v, want commerce.ErrVersionNotEditable", err)
	}

	// Retirement closes the machine, again one applied swap — and from the
	// wrong from-state it is refused, because the CAS is the whole story.
	applied, err = c.versions.Retire(ctx, version.ID, commerce.PlanVersionPublished, time.Now().UTC())
	if err != nil || !applied {
		t.Fatalf("retire the published version = (%t, %v), want (true, nil)", applied, err)
	}
	applied, err = c.versions.Retire(ctx, version.ID, commerce.PlanVersionPublished, time.Now().UTC())
	if err != nil || applied {
		t.Fatalf("a second retire from published = (%t, %v), want (false, nil)", applied, err)
	}

	retired, definitions, err := c.versions.ByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("read the retired version back: %v", err)
	}
	if retired.State != commerce.PlanVersionRetired || retired.PublishedAt == nil || retired.RetiredAt == nil {
		t.Fatalf("retired version read back = %+v, want retired with both stamps set", retired)
	}
	if len(definitions) != 2 {
		t.Fatalf("retirement rewrote the definition set: %d definitions, want the 2 it froze with", len(definitions))
	}

	// A draft refuses retirement in the domain before the statement runs;
	// asked anyway, the statement's CAS refuses it here.
	draft := openDraftVersion(t, c, plan.ID)
	if applied, err := c.versions.Retire(ctx, draft.ID, commerce.PlanVersionPublished, time.Now().UTC()); err != nil || applied {
		t.Fatalf("retire a draft from published = (%t, %v), want (false, nil) — the row never read published", applied, err)
	}
}

// TestIntegrationCommerceActivationIsASingleStatementVerdict drives the
// promotion both ways: it fires when every predicate holds — pending, start
// arrived on the database clock, owner account active — and refuses, without
// error, when any one of them is violated.
func TestIntegrationCommerceActivationIsASingleStatementVerdict(t *testing.T) {
	c := integrationCommerce(t)
	ctx := t.Context()
	plan := newIntegrationPlan(t, c)
	version, _ := publishVersionWithDefinitions(t, c, plan.ID, commerce.WildcardGroupName)
	now := time.Now().UTC()

	// Fires: a due pending subscription under an active account.
	account := integrationCommerceAccount(t, c, "it-commerce activation probe")
	due := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), true))
	if due.State != commerce.SubscriptionActive || due.CycleNumber == nil || *due.CycleNumber != 1 {
		t.Fatalf("promoted subscription = %+v, want active with cycle 1", due)
	}
	wantStart, wantEnd := mustCycleBounds(t, due.StartAt, 1)
	if due.PeriodStart == nil || due.PeriodEnd == nil ||
		!micros(*due.PeriodStart).Equal(micros(wantStart)) || !micros(*due.PeriodEnd).Equal(micros(wantEnd)) {
		t.Fatalf("promoted subscription carries bounds [%v, %v], want the domain's cycle-1 bounds of %s", due.PeriodStart, due.PeriodEnd, due.StartAt)
	}
	// And it cannot fire twice: the state moved, the statement says so.
	if applied, err := c.subscriptions.Activate(ctx, due.ID, wantStart, wantEnd, time.Now().UTC()); err != nil || applied {
		t.Fatalf("a second activate = (%t, %v), want (false, nil) — the row no longer reads pending", applied, err)
	}

	// Refuses: the start has not arrived on the database's clock.
	early := pendingSubscription(t, c, account, version.ID, now.Add(time.Hour), true)
	earlyStart, earlyEnd := mustCycleBounds(t, early.StartAt, 1)
	if applied, err := c.subscriptions.Activate(ctx, early.ID, earlyStart, earlyEnd, time.Now().UTC()); err != nil || applied {
		t.Fatalf("activate before start_at = (%t, %v), want (false, nil)", applied, err)
	}
	if stored, err := c.subscriptions.ByID(ctx, early.ID); err != nil || stored.State != commerce.SubscriptionPending || stored.CycleNumber != nil {
		t.Fatalf("the refused subscription is %+v (%v), want pending with the cycle fields still null", stored, err)
	}

	// Refuses: the owner account is not active — the gate no constraint
	// could carry, carried by the statement's EXISTS.
	suspendedOwner := integrationCommerceAccount(t, c, "it-commerce suspended owner probe")
	if applied, err := c.accounts.TransitionState(ctx, identity.AccountID(suspendedOwner), identity.AccountActive, identity.AccountSuspended, time.Now().UTC()); err != nil || !applied {
		t.Fatalf("suspend the owner account = (%t, %v), want (true, nil)", applied, err)
	}
	blocked := pendingSubscription(t, c, suspendedOwner, version.ID, matureStartAt(now), true)
	blockedStart, blockedEnd := mustCycleBounds(t, blocked.StartAt, 1)
	if applied, err := c.subscriptions.Activate(ctx, blocked.ID, blockedStart, blockedEnd, time.Now().UTC()); err != nil || applied {
		t.Fatalf("activate under a suspended account = (%t, %v), want (false, nil) — the EXISTS gate is the statement's", applied, err)
	}
	// The gate is the account's state, not the subscription's fate: the
	// moment the owner is active again, the same promotion lands.
	if applied, err := c.accounts.TransitionState(ctx, identity.AccountID(suspendedOwner), identity.AccountSuspended, identity.AccountActive, time.Now().UTC()); err != nil || !applied {
		t.Fatalf("reinstate the owner account = (%t, %v), want (true, nil)", applied, err)
	}
	if applied, err := c.subscriptions.Activate(ctx, blocked.ID, blockedStart, blockedEnd, time.Now().UTC()); err != nil || !applied {
		t.Fatalf("activate after reinstatement = (%t, %v), want (true, nil)", applied, err)
	}
}

// TestIntegrationCommerceRollIsASingleStatementVerdict drives the cycle roll
// both ways, one refused predicate at a time: renewal off, the current
// period not ended, a cancellation reaching into the cycle being granted, a
// stale cycle number, and a suspended owner account.
func TestIntegrationCommerceRollIsASingleStatementVerdict(t *testing.T) {
	c := integrationCommerce(t)
	ctx := t.Context()
	plan := newIntegrationPlan(t, c)
	version, definition := publishVersionWithDefinitions(t, c, plan.ID, commerce.WildcardGroupName)
	now := time.Now().UTC()

	// Fires: an active, renewing subscription whose first cycle the database
	// clock has ended, rolling from cycle 1 into cycle 2.
	account := integrationCommerceAccount(t, c, "it-commerce roll probe")
	renewing := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), true))
	wantStart, wantEnd := mustCycleBounds(t, renewing.StartAt, 2)
	if applied := rollOnce(t, c, renewing.ID, definition[0]); !applied {
		t.Fatal("the first roll of an ended cycle reported false — every predicate held, the advance must land")
	}
	rolled, err := c.subscriptions.ByID(ctx, renewing.ID)
	if err != nil {
		t.Fatalf("read the rolled subscription: %v", err)
	}
	if rolled.CycleNumber == nil || *rolled.CycleNumber != 2 ||
		!micros(*rolled.PeriodStart).Equal(micros(wantStart)) || !micros(*rolled.PeriodEnd).Equal(micros(wantEnd)) {
		t.Fatalf("rolled subscription = %+v, want cycle 2 with bounds [%s, %s]", rolled, micros(wantStart), micros(wantEnd))
	}

	// Fires again: cycle 2 ended an hour ago too — a start_at two months back
	// leaves two advances due — rolling from cycle 2 into cycle 3.
	if applied := rollOnce(t, c, renewing.ID, definition[0]); !applied {
		t.Fatal("the second due roll reported false")
	}
	rolled, err = c.subscriptions.ByID(ctx, renewing.ID)
	if err != nil || rolled.CycleNumber == nil || *rolled.CycleNumber != 3 {
		t.Fatalf("the twice-rolled subscription = %+v (%v), want cycle 3", rolled, err)
	}

	// Refuses: the period the row sits in has not ended yet — cycle 3 runs
	// an hour past the present, and the clock test is part of the statement.
	if applied := rollOnce(t, c, renewing.ID, definition[0]); applied {
		t.Fatal("a roll inside a live period reported true — the clock test is part of the statement")
	}

	// Refuses: renewal is off. Cycle 1 still lands for a fixed-term
	// subscription; nothing follows it.
	fixed := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), false))
	if applied := rollOnce(t, c, fixed.ID, definition[0]); applied {
		t.Fatal("a roll of a renewal-disabled subscription reported true")
	}

	// Refuses: a scheduled cancellation reaches into the cycle being granted.
	ending := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), true))
	_, cycle2End := mustCycleBounds(t, renewing.StartAt, 2)
	cancelAt := cycle2End.Add(-time.Hour) // inside cycle 2, the cycle the roll would grant
	if applied, err := c.subscriptions.ScheduleCancellation(ctx, ending.ID, cancelAt, time.Now().UTC()); err != nil || !applied {
		t.Fatalf("schedule the cancellation = (%t, %v), want (true, nil)", applied, err)
	}
	if applied := rollOnce(t, c, ending.ID, definition[0]); applied {
		t.Fatal("a roll into a cycle a cancellation reaches into reported true")
	}

	// Refuses: the caller's cycle number is stale. The row sits in cycle 1;
	// asked to roll "from cycle 2", the statement matches nothing.
	stale := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), true))
	staleStart, staleEnd := mustCycleBounds(t, stale.StartAt, 3)
	applied, err := c.subscriptions.Roll(ctx, stale.ID, 2, staleStart, staleEnd, time.Now().UTC())
	if err != nil || applied {
		t.Fatalf("a roll from a cycle the row never held = (%t, %v), want (false, nil)", applied, err)
	}

	// Refuses: the owner account is suspended — the same EXISTS gate the
	// promotion carries.
	suspendedOwner := integrationCommerceAccount(t, c, "it-commerce roll suspended owner probe")
	toFreeze := activate(t, c, pendingSubscription(t, c, suspendedOwner, version.ID, matureStartAt(now), true))
	if applied, err := c.accounts.TransitionState(ctx, identity.AccountID(suspendedOwner), identity.AccountActive, identity.AccountSuspended, time.Now().UTC()); err != nil || !applied {
		t.Fatalf("suspend the owner account = (%t, %v), want (true, nil)", applied, err)
	}
	if applied := rollOnce(t, c, toFreeze.ID, definition[0]); applied {
		t.Fatal("a roll under a suspended account reported true")
	}
}

// TestIntegrationCommerceDueScansAndTheExpiryVerdict pins the scan/gate
// pairing on the expiry lane: the scan lists exactly the rows whose
// predicates hold, the statement fires on one of them and refuses on every
// neighbour the predicate excludes, and a fired row stops being due.
func TestIntegrationCommerceDueScansAndTheExpiryVerdict(t *testing.T) {
	c := integrationCommerce(t)
	ctx := t.Context()
	plan := newIntegrationPlan(t, c)
	version, _ := publishVersionWithDefinitions(t, c, plan.ID, commerce.WildcardGroupName)
	now := time.Now().UTC()
	account := integrationCommerceAccount(t, c, "it-commerce expiry probe")

	// A fixed term ended an hour ago: due for expiry.
	ended := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), false))
	// A renewing neighbour whose cycle also ended: the roll lane owns it, and
	// the expiry scan must not list it.
	renewing := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), true))
	// A fixed term still live: not due.
	activate(t, c, pendingSubscription(t, c, account, version.ID, liveStartAt(now), false))

	due, err := c.subscriptions.DueExpiryIDs(ctx, 100)
	if err != nil {
		t.Fatalf("scan due expiries: %v", err)
	}
	if !containsSubscription(due, ended.ID) {
		t.Fatalf("the ended fixed term is missing from the expiry scan: %v", due)
	}
	if containsSubscription(due, renewing.ID) {
		t.Fatal("a renewing subscription appeared in the expiry scan — the roll lane owns its period end")
	}

	applied, err := c.subscriptions.Expire(ctx, ended.ID, time.Now().UTC())
	if err != nil || !applied {
		t.Fatalf("expire the ended fixed term = (%t, %v), want (true, nil)", applied, err)
	}
	stored, err := c.subscriptions.ByID(ctx, ended.ID)
	if err != nil || stored.State != commerce.SubscriptionExpired {
		t.Fatalf("the expired subscription is %+v (%v), want expired", stored, err)
	}

	// Terminal absorbs repetition, and the scan agrees.
	if applied, err := c.subscriptions.Expire(ctx, ended.ID, time.Now().UTC()); err != nil || applied {
		t.Fatalf("a second expire = (%t, %v), want (false, nil)", applied, err)
	}
	if due, err = c.subscriptions.DueExpiryIDs(ctx, 100); err != nil {
		t.Fatalf("re-scan due expiries: %v", err)
	}
	if containsSubscription(due, ended.ID) {
		t.Fatal("an expired subscription is still due for expiry — the scan and the statement disagree")
	}

	// The renewing neighbour: the statement refuses what the scan omitted,
	// asked directly.
	if applied, err := c.subscriptions.Expire(ctx, renewing.ID, time.Now().UTC()); err != nil || applied {
		t.Fatalf("expire a renewing subscription = (%t, %v), want (false, nil) — renewal_enabled = true refuses the fire", applied, err)
	}

	// Suspension is one of the two states the statement accepts, and the
	// scan lists it: the predicate is the state pair, the ended period, and
	// the absence of an instruction — nothing about activeness.
	suspendedDue := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), false))
	if applied, err := c.subscriptions.TransitionState(ctx, suspendedDue.ID, commerce.SubscriptionActive, commerce.SubscriptionSuspended, time.Now().UTC()); err != nil || !applied {
		t.Fatalf("suspend the subscription = (%t, %v), want (true, nil)", applied, err)
	}
	if due, err = c.subscriptions.DueExpiryIDs(ctx, 100); err != nil {
		t.Fatalf("re-scan due expiries: %v", err)
	}
	if !containsSubscription(due, suspendedDue.ID) {
		t.Fatalf("a suspended ended fixed term is missing from the expiry scan: %v", due)
	}
	if applied, err := c.subscriptions.Expire(ctx, suspendedDue.ID, time.Now().UTC()); err != nil || !applied {
		t.Fatalf("expire while suspended = (%t, %v), want (true, nil)", applied, err)
	}

	// A cancellation instruction outstanding: the expiry refuses — the
	// cancellation lane owns the end of a subscription the customer asked to
	// stop.
	instructed := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), false))
	if applied, err := c.subscriptions.ScheduleCancellation(ctx, instructed.ID, time.Now().UTC().Add(-time.Minute), time.Now().UTC()); err != nil || !applied {
		t.Fatalf("schedule the cancellation = (%t, %v), want (true, nil)", applied, err)
	}
	if applied, err := c.subscriptions.Expire(ctx, instructed.ID, time.Now().UTC()); err != nil || applied {
		t.Fatalf("expire under an outstanding instruction = (%t, %v), want (false, nil)", applied, err)
	}
}

// TestIntegrationCommerceTheWholeLifecycleWalksOnOneSubscription is the
// spine: promotion with its first entitlement, two guarded rolls each with
// theirs, the scheduled cancellation recorded, seen due, and completed, and
// the terminal state every lane agrees on — one subscription, walked end to
// end through the port.
func TestIntegrationCommerceTheWholeLifecycleWalksOnOneSubscription(t *testing.T) {
	c := integrationCommerce(t)
	ctx := t.Context()
	plan := newIntegrationPlan(t, c)
	version, definitions := publishVersionWithDefinitions(t, c, plan.ID, commerce.WildcardGroupName)
	definition := definitions[0]
	account := integrationCommerceAccount(t, c, "it-commerce lifecycle walk probe")

	subscription := pendingSubscription(t, c, account, version.ID, matureStartAt(time.Now().UTC()), true)

	// Promotion and its first entitlement are one fact: one unit of work,
	// the advance and the insert committing together or not at all.
	cycle1Start, cycle1End := mustCycleBounds(t, subscription.StartAt, 1)
	first := grantCycle(t, subscription.ID, 1, definition, cycle1Start, cycle1End)
	err := c.store.WithinTx(ctx, func(txCtx context.Context) error {
		if applied, err := c.subscriptions.Activate(txCtx, subscription.ID, cycle1Start, cycle1End, time.Now().UTC()); err != nil || !applied {
			t.Fatalf("promote the pending subscription = (%t, %v), want (true, nil)", applied, err)
		}
		return c.entitlements.Create(txCtx, *first)
	})
	if err != nil {
		t.Fatalf("the promotion unit of work: %v", err)
	}

	// Two rolls, each granting its cycle in the same unit of work as its
	// advance. Cycle 1 and cycle 2 both ended an hour before this test ran,
	// so exactly two advances are due; cycle 3 runs an hour past the present.
	if !rollOnce(t, c, subscription.ID, definition) {
		t.Fatal("roll 1 -> 2 reported false")
	}
	if !rollOnce(t, c, subscription.ID, definition) {
		t.Fatal("roll 2 -> 3 reported false")
	}
	rolled, err := c.subscriptions.ByID(ctx, subscription.ID)
	if err != nil || rolled.CycleNumber == nil || *rolled.CycleNumber != 3 {
		t.Fatalf("the walked subscription = %+v (%v), want cycle 3", rolled, err)
	}

	// The customer asks to stop: cancel_at is recorded with its mode in one
	// statement, the subscription stays active and usable until it passes,
	// and the cancellation scan sees it the moment the database clock does.
	cancelAt := time.Now().UTC().Add(-time.Minute)
	if applied, err := c.subscriptions.ScheduleCancellation(ctx, subscription.ID, cancelAt, time.Now().UTC()); err != nil || !applied {
		t.Fatalf("schedule the cancellation = (%t, %v), want (true, nil)", applied, err)
	}
	dueCancellations, err := c.subscriptions.DueCancellationIDs(ctx, 100)
	if err != nil {
		t.Fatalf("scan due cancellations: %v", err)
	}
	if !containsSubscription(dueCancellations, subscription.ID) {
		t.Fatalf("the instructed subscription is missing from the cancellation scan: %v", dueCancellations)
	}

	// Completion: the lane's own statement is the verdict — it repeats the
	// instruction the scan read (scheduled, and due on the database clock),
	// and the first completion absorbs every later one. The instructed
	// instant is kept: the row is ended by the cancellation the customer
	// asked for, not rewritten by the lane that executed it.
	applied, err := c.subscriptions.CompleteScheduledCancellation(ctx, subscription.ID, time.Now().UTC())
	if err != nil || !applied {
		t.Fatalf("complete the scheduled cancellation = (%t, %v), want (true, nil)", applied, err)
	}
	if applied, err = c.subscriptions.CompleteScheduledCancellation(ctx, subscription.ID, time.Now().UTC()); err != nil || applied {
		t.Fatalf("a second completion = (%t, %v), want (false, nil)", applied, err)
	}
	final, err := c.subscriptions.ByID(ctx, subscription.ID)
	if err != nil {
		t.Fatalf("read the cancelled subscription: %v", err)
	}
	if final.State != commerce.SubscriptionCancelled || final.CancellationMode != commerce.CancellationScheduled {
		t.Fatalf("the cancelled subscription = %+v, want cancelled with the scheduled instruction it was ended by", final)
	}
	if final.CancelAt == nil || !micros(*final.CancelAt).Equal(micros(cancelAt)) {
		t.Fatalf("completion moved cancel_at to %v, want the instructed %v", final.CancelAt, cancelAt)
	}

	// Terminal is terminal for every lane: no roll, no expiry, no scan.
	if applied := rollOnce(t, c, subscription.ID, definition); applied {
		t.Fatal("a roll of a cancelled subscription reported true")
	}
	if dueCancellations, err = c.subscriptions.DueCancellationIDs(ctx, 100); err != nil {
		t.Fatalf("re-scan due cancellations: %v", err)
	}
	if containsSubscription(dueCancellations, subscription.ID) {
		t.Fatal("a cancelled subscription is still due for cancellation")
	}

	// The three cycles' grants remain — history immutable, retained for the
	// accounting rows that point at them — but none is a live candidate.
	candidates, err := c.entitlements.ActiveCandidates(ctx, account)
	if err != nil {
		t.Fatalf("read candidates after cancellation: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("a cancelled subscription's grants are still candidates: %d of them", len(candidates))
	}
}

// TestIntegrationCommerceScheduledCompletionIsItsOwnStatementVerdict pins
// the cancellation lane's verdict: the statement repeats the instruction the
// scan read — state active, mode scheduled, cancel_at due on the database
// clock — so every neighbour the predicates exclude waits, and an
// instruction rescheduled after the scan is not executed under words the
// customer already superseded.
func TestIntegrationCommerceScheduledCompletionIsItsOwnStatementVerdict(t *testing.T) {
	c := integrationCommerce(t)
	ctx := t.Context()
	plan := newIntegrationPlan(t, c)
	version, _ := publishVersionWithDefinitions(t, c, plan.ID, commerce.WildcardGroupName)
	now := time.Now().UTC()
	account := integrationCommerceAccount(t, c, "it-commerce completion probe")

	// Fires: due, scheduled, active — and the completion keeps the
	// instructed instant and the scheduled mode beside the terminal state.
	due := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), true))
	dueAt := now.Add(-time.Minute)
	if applied, err := c.subscriptions.ScheduleCancellation(ctx, due.ID, dueAt, now); err != nil || !applied {
		t.Fatalf("schedule the instruction = (%t, %v), want (true, nil)", applied, err)
	}
	applied, err := c.subscriptions.CompleteScheduledCancellation(ctx, due.ID, now)
	if err != nil || !applied {
		t.Fatalf("complete the due instruction = (%t, %v), want (true, nil)", applied, err)
	}
	stored, err := c.subscriptions.ByID(ctx, due.ID)
	if err != nil || stored.State != commerce.SubscriptionCancelled || stored.CancellationMode != commerce.CancellationScheduled {
		t.Fatalf("the completed row = %+v (%v), want cancelled with the scheduled mode kept", stored, err)
	}
	if stored.CancelAt == nil || !micros(*stored.CancelAt).Equal(micros(dueAt)) {
		t.Fatalf("completion moved cancel_at to %v, want the instructed %v", stored.CancelAt, dueAt)
	}

	// Refuses: the instruction is not due. The clock test is part of the
	// statement, so a lane arriving before the instant leaves the row
	// exactly as it found it.
	early := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), true))
	if applied, err := c.subscriptions.ScheduleCancellation(ctx, early.ID, now.Add(time.Hour), now); err != nil || !applied {
		t.Fatalf("schedule the instruction = (%t, %v), want (true, nil)", applied, err)
	}
	if applied, err := c.subscriptions.CompleteScheduledCancellation(ctx, early.ID, now); err != nil || applied {
		t.Fatalf("a completion before the instant = (%t, %v), want (false, nil)", applied, err)
	}

	// Refuses: the immediate path got there first. The row is terminal, and
	// terminal is no verdict's to revisit.
	imm := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), true))
	if applied, err := c.subscriptions.Cancel(ctx, imm.ID, now); err != nil || !applied {
		t.Fatalf("the immediate cancel = (%t, %v), want (true, nil)", applied, err)
	}
	if applied, err := c.subscriptions.CompleteScheduledCancellation(ctx, imm.ID, now); err != nil || applied {
		t.Fatalf("a completion of an immediately-cancelled row = (%t, %v), want (false, nil)", applied, err)
	}

	// Refuses: the row is suspended. A suspension on file supersedes the
	// lane's verdict the way any moved world does — the instruction waits
	// for a row whose state the statement names.
	frozen := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), true))
	if applied, err := c.subscriptions.ScheduleCancellation(ctx, frozen.ID, now.Add(-time.Minute), now); err != nil || !applied {
		t.Fatalf("schedule the instruction = (%t, %v), want (true, nil)", applied, err)
	}
	if applied, err := c.subscriptions.TransitionState(ctx, frozen.ID, commerce.SubscriptionActive, commerce.SubscriptionSuspended, now); err != nil || !applied {
		t.Fatalf("suspend the instructed row = (%t, %v), want (true, nil)", applied, err)
	}
	if applied, err := c.subscriptions.CompleteScheduledCancellation(ctx, frozen.ID, now); err != nil || applied {
		t.Fatalf("a completion of a suspended row = (%t, %v), want (false, nil)", applied, err)
	}

	// Refuses: the instruction moved after the scan. The lane's read said
	// due; the customer pushed the instant out; the statement's clock test
	// judges the instant on file, not the one the scan saw.
	moved := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(now), true))
	if applied, err := c.subscriptions.ScheduleCancellation(ctx, moved.ID, now.Add(-time.Minute), now); err != nil || !applied {
		t.Fatalf("schedule the instruction = (%t, %v), want (true, nil)", applied, err)
	}
	pushedTo := now.Add(time.Hour)
	if applied, err := c.subscriptions.ScheduleCancellation(ctx, moved.ID, pushedTo, now); err != nil || !applied {
		t.Fatalf("reschedule the instruction = (%t, %v), want (true, nil)", applied, err)
	}
	if applied, err := c.subscriptions.CompleteScheduledCancellation(ctx, moved.ID, now); err != nil || applied {
		t.Fatalf("a completion of a rescheduled instruction = (%t, %v), want (false, nil)", applied, err)
	}
	still, err := c.subscriptions.ByID(ctx, moved.ID)
	if err != nil || still.State != commerce.SubscriptionActive ||
		still.CancelAt == nil || !micros(*still.CancelAt).Equal(micros(pushedTo)) {
		t.Fatalf("the rescheduled row = %+v (%v), want active with the pushed instant intact", still, err)
	}
}

// TestIntegrationCommerceALostRollTakesItsGrantBack pins the roll unit of
// work's all-or-nothing shape: when the guarded advance matches nothing, the
// entitlement inserted beside it in the same transaction does not survive —
// a grant without its cycle advance is exactly the half-state the doctrine
// forbids.
func TestIntegrationCommerceALostRollTakesItsGrantBack(t *testing.T) {
	c := integrationCommerce(t)
	ctx := t.Context()
	plan := newIntegrationPlan(t, c)
	version, definitions := publishVersionWithDefinitions(t, c, plan.ID, commerce.WildcardGroupName)
	definition := definitions[0]
	account := integrationCommerceAccount(t, c, "it-commerce rollback probe")
	subscription := activate(t, c, pendingSubscription(t, c, account, version.ID, liveStartAt(time.Now().UTC()), true))

	// Cycle 1's grant, the way the promotion's unit of work leaves it.
	cycle1Start, cycle1End := mustCycleBounds(t, subscription.StartAt, 1)
	if err := c.entitlements.Create(ctx, *grantCycle(t, subscription.ID, 1, definition, cycle1Start, cycle1End)); err != nil {
		t.Fatalf("grant cycle 1: %v", err)
	}

	// The live period means the advance refuses; the unit of work returns
	// with cycle 2's insert undone.
	if applied := rollOnce(t, c, subscription.ID, definition); applied {
		t.Fatal("a roll inside a live period reported true")
	}
	candidates, err := c.entitlements.ActiveCandidates(ctx, account)
	if err != nil {
		t.Fatalf("read the candidates after the lost roll: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Entitlement.CycleNumber != 1 {
		t.Fatalf("candidates after the lost roll = %+v, want only cycle 1's grant — the next cycle's insert must not survive the rollback", candidates)
	}
	stored, err := c.subscriptions.ByID(ctx, subscription.ID)
	if err != nil || stored.CycleNumber == nil || *stored.CycleNumber != 1 {
		t.Fatalf("the subscription after the lost roll = %+v (%v), want cycle 1 untouched", stored, err)
	}
}

// TestIntegrationCommerceTheDueScansRepeatTheirStatementsAccountGate pins
// the EXISTS gate the promotion and roll scans carry beside their own
// predicates: a row whose owner account is not active waits in no lane, so
// a permanently unpayable row cannot sit at a batch's head starving the
// rows behind it.
func TestIntegrationCommerceTheDueScansRepeatTheirStatementsAccountGate(t *testing.T) {
	c := integrationCommerce(t)
	ctx := t.Context()
	plan := newIntegrationPlan(t, c)
	version, _ := publishVersionWithDefinitions(t, c, plan.ID, commerce.WildcardGroupName)
	now := time.Now().UTC()
	owner := integrationCommerceAccount(t, c, "it-commerce scan gate probe")
	pending := pendingSubscription(t, c, owner, version.ID, matureStartAt(now), true)
	rolling := activate(t, c, pendingSubscription(t, c, owner, version.ID, matureStartAt(now), true))

	if applied, err := c.accounts.TransitionState(ctx, identity.AccountID(owner), identity.AccountActive, identity.AccountSuspended, now); err != nil || !applied {
		t.Fatalf("suspend the owner account = (%t, %v), want (true, nil)", applied, err)
	}
	duePromotions, err := c.subscriptions.DuePromotionIDs(ctx, 100)
	if err != nil {
		t.Fatalf("scan due promotions: %v", err)
	}
	if containsSubscription(duePromotions, pending.ID) {
		t.Fatal("a suspended owner's pending subscription is listed for promotion — the scan's gate is missing")
	}
	dueRolls, err := c.subscriptions.DueRollIDs(ctx, 100)
	if err != nil {
		t.Fatalf("scan due rolls: %v", err)
	}
	if containsSubscription(dueRolls, rolling.ID) {
		t.Fatal("a suspended owner's ended cycle is listed for roll — the scan's gate is missing")
	}

	// The gate is the account's state, not the rows' worthiness: the moment
	// the account is back, the same rows return to the same scans.
	if applied, err := c.accounts.TransitionState(ctx, identity.AccountID(owner), identity.AccountSuspended, identity.AccountActive, now); err != nil || !applied {
		t.Fatalf("reinstate the owner account = (%t, %v), want (true, nil)", applied, err)
	}
	if duePromotions, err = c.subscriptions.DuePromotionIDs(ctx, 100); err != nil {
		t.Fatalf("re-scan due promotions: %v", err)
	}
	if !containsSubscription(duePromotions, pending.ID) {
		t.Fatalf("the reinstated owner's pending subscription is missing from the promotion scan: %v", duePromotions)
	}
	if dueRolls, err = c.subscriptions.DueRollIDs(ctx, 100); err != nil {
		t.Fatalf("re-scan due rolls: %v", err)
	}
	if !containsSubscription(dueRolls, rolling.ID) {
		t.Fatalf("the reinstated owner's ended cycle is missing from the roll scan: %v", dueRolls)
	}
}

// TestIntegrationCommerceEntitlementsAreGrantedOncePerCycleAndExpireOnTheClock
// pins the entitlement lane: the once-per-cycle uniqueness is the schema's
// and surfaces unmapped (only a buggy roll can fire it), the expiry flip is
// a guarded verdict judged by the database clock, and the expiry scan agrees
// with the statement about what is due.
func TestIntegrationCommerceEntitlementsAreGrantedOncePerCycleAndExpireOnTheClock(t *testing.T) {
	c := integrationCommerce(t)
	ctx := t.Context()
	plan := newIntegrationPlan(t, c)
	version, definitions := publishVersionWithDefinitions(t, c, plan.ID, commerce.WildcardGroupName, "it-commerce-group-second")
	definition, secondDefinition := definitions[0], definitions[1]
	account := integrationCommerceAccount(t, c, "it-commerce entitlement probe")
	subscription := activate(t, c, pendingSubscription(t, c, account, version.ID, matureStartAt(time.Now().UTC()), true))
	cycle1Start, cycle1End := mustCycleBounds(t, subscription.StartAt, 1)

	first := grantCycle(t, subscription.ID, 1, definition, cycle1Start, cycle1End)
	if err := c.entitlements.Create(ctx, *first); err != nil {
		t.Fatalf("create the cycle-1 entitlement: %v", err)
	}

	// The retried roll: same subscription, same cycle, same definition. The
	// schema refuses it, and the refusal arrives as infrastructure — no
	// domain sentinel claims it, because the guarded cycle advance is the
	// guard that should have kept the unit of work from opening.
	twin := grantCycle(t, subscription.ID, 1, definition, cycle1Start, cycle1End)
	err := c.entitlements.Create(ctx, *twin)
	if err == nil {
		t.Fatal("a second grant for one (subscription, cycle, definition) was stored")
	}
	for _, sentinel := range []error{commerce.ErrPlanNameTaken, commerce.ErrPlanVersionNumberTaken, commerce.ErrGrantScopeTaken} {
		if errors.Is(err, sentinel) {
			t.Fatalf("the once-per-cycle refusal was mapped to %v", sentinel)
		}
	}

	// Expiry before the cycle's end is refused; at the end it fires once.
	// The not-yet-ended grant rides the second definition, so the
	// once-per-cycle triple stays free for it, and its bounds reach a month
	// past the present.
	now := time.Now().UTC()
	laterStart, laterEnd := cycle1End, now.AddDate(0, 1, 0)
	notYetEnded := grantCycle(t, subscription.ID, 1, secondDefinition, laterStart, laterEnd)
	if err := c.entitlements.Create(ctx, *notYetEnded); err != nil {
		t.Fatalf("create the not-yet-ended entitlement: %v", err)
	}
	if applied, err := c.entitlements.Expire(ctx, notYetEnded.ID, time.Now().UTC()); err != nil || applied {
		t.Fatalf("expire before the period end = (%t, %v), want (false, nil)", applied, err)
	}

	due, err := c.entitlements.DueExpiryIDs(ctx, 100)
	if err != nil {
		t.Fatalf("scan due entitlement expiries: %v", err)
	}
	// The ended-cycle grant is due — its bounds closed an hour before this
	// test ran — and the not-yet-ended one is not.
	if containsEntitlement(due, notYetEnded.ID) {
		t.Fatalf("the expiry scan listed a grant whose cycle has not ended: %v", due)
	}
	if !containsEntitlement(due, first.ID) {
		t.Fatalf("the ended-cycle grant is missing from the expiry scan: %v", due)
	}

	applied, err := c.entitlements.Expire(ctx, first.ID, time.Now().UTC())
	if err != nil || !applied {
		t.Fatalf("expire the ended-cycle entitlement = (%t, %v), want (true, nil)", applied, err)
	}
	if applied, err := c.entitlements.Expire(ctx, first.ID, time.Now().UTC()); err != nil || applied {
		t.Fatalf("a second entitlement expire = (%t, %v), want (false, nil)", applied, err)
	}
	if due, err = c.entitlements.DueExpiryIDs(ctx, 100); err != nil {
		t.Fatalf("re-scan due entitlement expiries: %v", err)
	}
	if containsEntitlement(due, first.ID) {
		t.Fatal("an expired entitlement is still due for expiry")
	}
	if containsEntitlement(due, notYetEnded.ID) {
		t.Fatal("an entitlement whose cycle has not ended appeared in the expiry scan")
	}
}

// TestIntegrationCommerceActiveCandidatesFeedTheDerivation pins the read the
// waterfall is written against: every active entitlement of an active
// subscription, joined to the subscription's creation instant and the
// definition's scope name, with the suspended subscription's grants, the
// expired grants, and the other account's grants all kept out — and the
// result feeding the domain's own derivation unchanged.
func TestIntegrationCommerceActiveCandidatesFeedTheDerivation(t *testing.T) {
	c := integrationCommerce(t)
	ctx := t.Context()
	plan := newIntegrationPlan(t, c)
	version, definitions := publishVersionWithDefinitions(t, c, plan.ID, "it-commerce-group-candidates", commerce.WildcardGroupName)
	named, wildcard := definitions[0], definitions[1]
	now := time.Now().UTC()

	account := integrationCommerceAccount(t, c, "it-commerce candidates probe")
	other := integrationCommerceAccount(t, c, "it-commerce candidates probe, other account")

	// Two active subscriptions with the same anchor, so their cycles share
	// bounds and the waterfall's specificity key is the only difference
	// between their grants.
	startAt := liveStartAt(now)
	firstSub := activate(t, c, pendingSubscription(t, c, account, version.ID, startAt, true))
	secondSub := activate(t, c, pendingSubscription(t, c, account, version.ID, startAt, true))
	cycleStart, cycleEnd := mustCycleBounds(t, startAt, 1)

	// The named grant on the first subscription, the wildcard on the second:
	// both cover the present.
	namedGrant := grantCycle(t, firstSub.ID, 1, named, cycleStart, cycleEnd)
	if err := c.entitlements.Create(ctx, *namedGrant); err != nil {
		t.Fatalf("create the named grant: %v", err)
	}
	wildcardGrant := grantCycle(t, secondSub.ID, 1, wildcard, cycleStart, cycleEnd)
	if err := c.entitlements.Create(ctx, *wildcardGrant); err != nil {
		t.Fatalf("create the wildcard grant: %v", err)
	}

	// An expired grant on the same live subscription: the state filter keeps
	// it out of the candidates. Its cycle is an already-ended one, because
	// the expiry statement judges the grant's own bounds by the clock.
	endedStart, endedEnd := mustCycleBounds(t, matureStartAt(now), 1)
	expiredGrant := grantCycle(t, secondSub.ID, 1, named, endedStart, endedEnd)
	if err := c.entitlements.Create(ctx, *expiredGrant); err != nil {
		t.Fatalf("create the doomed grant: %v", err)
	}
	if applied, err := c.entitlements.Expire(ctx, expiredGrant.ID, time.Now().UTC()); err != nil || !applied {
		t.Fatalf("expire the doomed grant = (%t, %v), want (true, nil)", applied, err)
	}

	// A suspended subscription's grant: the subscription filter keeps it out.
	suspendedSub := activate(t, c, pendingSubscription(t, c, account, version.ID, startAt, true))
	if applied, err := c.subscriptions.TransitionState(ctx, suspendedSub.ID, commerce.SubscriptionActive, commerce.SubscriptionSuspended, time.Now().UTC()); err != nil || !applied {
		t.Fatalf("suspend the third subscription = (%t, %v), want (true, nil)", applied, err)
	}
	suspendedGrant := grantCycle(t, suspendedSub.ID, 1, wildcard, cycleStart, cycleEnd)
	if err := c.entitlements.Create(ctx, *suspendedGrant); err != nil {
		t.Fatalf("create the suspended subscription's grant: %v", err)
	}

	// Another account's grant: the account filter keeps it out.
	otherSub := activate(t, c, pendingSubscription(t, c, other, version.ID, startAt, true))
	otherGrant := grantCycle(t, otherSub.ID, 1, named, cycleStart, cycleEnd)
	if err := c.entitlements.Create(ctx, *otherGrant); err != nil {
		t.Fatalf("create the other account's grant: %v", err)
	}

	candidates, err := c.entitlements.ActiveCandidates(ctx, account)
	if err != nil {
		t.Fatalf("read the candidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want exactly the named and the wildcard grant: %v", len(candidates), scopesOf(candidates))
	}
	byScope := map[commerce.AliasGroupName]commerce.CandidateGrant{}
	for _, candidate := range candidates {
		byScope[candidate.AliasGroupName] = candidate
	}
	namedCandidate, hasNamed := byScope[named.AliasGroupName]
	wildcardCandidate, hasWildcard := byScope[commerce.WildcardGroupName]
	if !hasNamed || !hasWildcard {
		t.Fatalf("candidates by scope = %v, want the named group and the wildcard", scopesOf(candidates))
	}

	// The entitlement travels whole — every field the row holds, not a
	// projection of it.
	if namedCandidate.Entitlement.ID != namedGrant.ID ||
		namedCandidate.Entitlement.SubscriptionID != firstSub.ID ||
		namedCandidate.Entitlement.CycleNumber != 1 ||
		namedCandidate.Entitlement.GrantDefinitionID != named.ID ||
		namedCandidate.Entitlement.Dimension != commerce.DimensionCost ||
		namedCandidate.Entitlement.GrantedAmount != 1000 ||
		namedCandidate.Entitlement.State != commerce.EntitlementActive ||
		!micros(namedCandidate.Entitlement.PeriodStart).Equal(micros(cycleStart)) ||
		!micros(namedCandidate.Entitlement.PeriodEnd).Equal(micros(cycleEnd)) {
		t.Fatalf("the named candidate = %+v, want the entitlement row whole for grant %s", namedCandidate, namedGrant.ID)
	}
	// The two facts the waterfall reads from neighbouring rows: the
	// subscription's creation instant — never normalised to start_at — and
	// the definition's scope name.
	if !namedCandidate.SubscriptionCreatedAt.Equal(firstSub.CreatedAt) {
		t.Fatalf("the candidate carries subscription created_at %s, want the row's %s",
			namedCandidate.SubscriptionCreatedAt, firstSub.CreatedAt)
	}
	if wildcardCandidate.Entitlement.SubscriptionID != secondSub.ID ||
		wildcardCandidate.Entitlement.GrantedAmount != 1001 {
		t.Fatalf("the wildcard candidate = %+v, want second subscription's grant of 1001", wildcardCandidate)
	}

	// And the derivation consumes them unchanged: with the cycle bounds
	// equal, the named scope ranks before the wildcard — the waterfall's
	// first key, over candidates the query delivered.
	effective := commerce.EffectiveEntitlements(candidates, time.Now().UTC())
	if len(effective) != 2 {
		t.Fatalf("the derivation saw %d effective grants, want 2", len(effective))
	}
	if effective[0].AliasGroupName != named.AliasGroupName || effective[1].AliasGroupName != commerce.WildcardGroupName {
		t.Fatalf("the waterfall ranked [%s, %s], want the named scope before the wildcard", effective[0].AliasGroupName, effective[1].AliasGroupName)
	}
}

// TestIntegrationCommercePaygIsARowPerAccountWithAWriteOnceBucket pins the
// PAYG row's whole life through the port: absence is the disabled state, the
// flag flips in one insert-or-update that never rewrites the row's birth,
// and the bucket reference lands once and never again.
func TestIntegrationCommercePaygIsARowPerAccountWithAWriteOnceBucket(t *testing.T) {
	c := integrationCommerce(t)
	ctx := t.Context()
	account := integrationCommerceAccount(t, c, "it-commerce payg probe")
	neverEnabled := integrationCommerceAccount(t, c, "it-commerce payg probe, never enabled")

	// Absence is the disabled state, and it is ErrNotFound, not a zero value.
	if _, err := c.payg.ByAccount(ctx, account); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("an account that never enabled PAYG returned %v, want persistence.ErrNotFound", err)
	}

	created := time.Now().UTC().Truncate(time.Millisecond)
	if err := c.payg.SetEnabled(ctx, account, true, created); err != nil {
		t.Fatalf("enable PAYG: %v", err)
	}
	stored, err := c.payg.ByAccount(ctx, account)
	if err != nil {
		t.Fatalf("read the PAYG row back: %v", err)
	}
	if !stored.Enabled || stored.FundingBucketID != "" || !micros(stored.CreatedAt).Equal(micros(created)) {
		t.Fatalf("the enabled PAYG row = %+v, want enabled, unfunded, born at %s", stored, micros(created))
	}

	disabledAt := created.Add(time.Minute)
	if err := c.payg.SetEnabled(ctx, account, false, disabledAt); err != nil {
		t.Fatalf("disable PAYG: %v", err)
	}
	stored, err = c.payg.ByAccount(ctx, account)
	if err != nil {
		t.Fatalf("read the disabled PAYG row back: %v", err)
	}
	if stored.Enabled {
		t.Fatal("the flag did not flip off")
	}
	if !micros(stored.CreatedAt).Equal(micros(created)) {
		t.Fatalf("the update rewrote the row's birth: created_at %s, want %s", micros(stored.CreatedAt), micros(created))
	}
	if !micros(stored.UpdatedAt).Equal(micros(disabledAt)) {
		t.Fatalf("updated_at = %s, want the update's instant %s", micros(stored.UpdatedAt), micros(disabledAt))
	}

	// The bucket: write-once, by the statement's own WHERE clause.
	bucketOne := mustFundingBucketID(t)
	applied, err := c.payg.AssignFundingBucket(ctx, account, bucketOne, time.Now().UTC())
	if err != nil || !applied {
		t.Fatalf("assign the funding bucket = (%t, %v), want (true, nil)", applied, err)
	}
	bucketTwo := mustFundingBucketID(t)
	if applied, err := c.payg.AssignFundingBucket(ctx, account, bucketTwo, time.Now().UTC()); err != nil || applied {
		t.Fatalf("reassign the funding bucket = (%t, %v), want (false, nil) — one bucket per account, ever", applied, err)
	}
	stored, err = c.payg.ByAccount(ctx, account)
	if err != nil {
		t.Fatalf("read the funded PAYG row back: %v", err)
	}
	if stored.FundingBucketID != bucketOne {
		t.Fatalf("funding_bucket_id = %s, want the first assignment %s", stored.FundingBucketID, bucketOne)
	}

	// Disabling never touched the bucket, and enabling again must not either.
	if err := c.payg.SetEnabled(ctx, account, true, time.Now().UTC()); err != nil {
		t.Fatalf("re-enable PAYG: %v", err)
	}
	if stored, err = c.payg.ByAccount(ctx, account); err != nil || stored.FundingBucketID != bucketOne {
		t.Fatalf("the re-enabled PAYG row = %+v (%v), want the bucket reference untouched", stored, err)
	}

	// An account with no PAYG row has nothing to fund: the statement writes
	// nothing and says so.
	if applied, err := c.payg.AssignFundingBucket(ctx, neverEnabled, bucketTwo, time.Now().UTC()); err != nil || applied {
		t.Fatalf("assign a bucket to an unfunded account = (%t, %v), want (false, nil)", applied, err)
	}
	if _, err := c.payg.ByAccount(ctx, neverEnabled); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("the refused assignment materialised a row: %v", err)
	}
}

// TestIntegrationCommerceTheDatabaseClockIsStableInsideAUnitOfWork pins the
// property the roll lane is written on: transaction_timestamp() is one
// instant for the whole unit of work — two reads either side of a deliberate
// delay agree — and the reading statement after the commit moves on.
func TestIntegrationCommerceTheDatabaseClockIsStableInsideAUnitOfWork(t *testing.T) {
	c := integrationCommerce(t)

	var insideFirst, insideSecond time.Time
	err := c.store.WithinTx(t.Context(), func(ctx context.Context) error {
		now, err := c.clock.Now(ctx)
		if err != nil {
			return err
		}
		insideFirst = now
		if _, err := c.store.Querier(ctx).ExecContext(ctx, "SELECT pg_sleep(0.05)"); err != nil {
			return err
		}
		now, err = c.clock.Now(ctx)
		if err != nil {
			return err
		}
		insideSecond = now
		return nil
	})
	if err != nil {
		t.Fatalf("the clock unit of work: %v", err)
	}
	if !insideFirst.Equal(insideSecond) {
		t.Fatalf("transaction_timestamp() moved inside one unit of work: %s then %s — every gate in the unit must judge the same now", insideFirst, insideSecond)
	}

	after, err := c.clock.Now(t.Context())
	if err != nil {
		t.Fatalf("read the clock after the commit: %v", err)
	}
	if !after.After(insideFirst) {
		t.Fatalf("transaction_timestamp() after the commit = %s, want it after the transaction's %s", after, insideFirst)
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func mustFundingBucketID(t *testing.T) commerce.FundingBucketID {
	t.Helper()
	// The bucket is Accounting's aggregate (B6): another context's id, taken
	// here from commerce's v7 minter for its form alone.
	id, err := commerce.NewEntitlementID()
	if err != nil {
		t.Fatalf("NewEntitlementID (bucket stand-in): %v", err)
	}
	return commerce.FundingBucketID(id)
}

// mustAbsentPlanID mints a well-formed uuid that names no row: a fresh v7 is
// absent by construction, which is what a miss-lookup's subject must be — a
// hand-written stand-in like "…ff" is not a uuid at all, and the database
// answers a syntax error instead of no row.
func mustAbsentPlanID(t *testing.T) string {
	t.Helper()
	id, err := commerce.NewPlanID()
	if err != nil {
		t.Fatalf("NewPlanID: %v", err)
	}
	return string(id)
}

func containsSubscription(ids []commerce.SubscriptionID, want commerce.SubscriptionID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func containsEntitlement(ids []commerce.EntitlementID, want commerce.EntitlementID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func scopesOf(candidates []commerce.CandidateGrant) []commerce.AliasGroupName {
	scopes := make([]commerce.AliasGroupName, 0, len(candidates))
	for _, candidate := range candidates {
		scopes = append(scopes, candidate.AliasGroupName)
	}
	return scopes
}
