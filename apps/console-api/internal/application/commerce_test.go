package application

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// commercePorts is the constructor's argument list as one value, so the
// nil-port test can nil one port at a time without repeating ten arguments.
type commercePorts struct {
	store         persistence.Store
	accounts      persistence.Accounts
	plans         persistence.Plans
	versions      persistence.PlanVersions
	subscriptions persistence.Subscriptions
	entitlements  persistence.Entitlements
	payg          persistence.PaygAccounts
	clock         persistence.Clock
	catalog       dataplane.CatalogReader
	funder        Funder
}

func commercePortsFor(w *commerceWorld) commercePorts {
	return commercePorts{
		store:         fakeCommerceStore{world: w},
		accounts:      fakeCommerceAccounts{world: w},
		plans:         fakePlans{world: w},
		versions:      fakePlanVersions{world: w},
		subscriptions: fakeSubscriptions{world: w},
		entitlements:  fakeEntitlements{world: w},
		payg:          fakePaygAccounts{world: w},
		clock:         fakeClock{world: w},
		catalog:       fakeCatalog{world: w},
		funder:        fakeFunder{world: w},
	}
}

// TestNewCommerceRefusesAnyNilPort pins the constructor's wiring contract:
// ten ports, every one required, and a nil anywhere is a panic at the
// composition root rather than a nil dereference in the middle of a roll.
func TestNewCommerceRefusesAnyNilPort(t *testing.T) {
	world := newCommerceWorld(t)
	full := commercePortsFor(world)
	nilOne := []struct {
		name string
		nil_ func(p *commercePorts)
	}{
		{"store", func(p *commercePorts) { p.store = nil }},
		{"accounts", func(p *commercePorts) { p.accounts = nil }},
		{"plans", func(p *commercePorts) { p.plans = nil }},
		{"versions", func(p *commercePorts) { p.versions = nil }},
		{"subscriptions", func(p *commercePorts) { p.subscriptions = nil }},
		{"entitlements", func(p *commercePorts) { p.entitlements = nil }},
		{"payg", func(p *commercePorts) { p.payg = nil }},
		{"clock", func(p *commercePorts) { p.clock = nil }},
		{"catalog", func(p *commercePorts) { p.catalog = nil }},
		{"funder", func(p *commercePorts) { p.funder = nil }},
	}
	for _, port := range nilOne {
		t.Run(port.name, func(t *testing.T) {
			broken := full
			port.nil_(&broken)
			func() {
				defer func() {
					if recover() == nil {
						t.Fatalf("NewCommerce accepted a nil %s port", port.name)
					}
				}()
				NewCommerce(broken.store, broken.accounts, broken.plans, broken.versions,
					broken.subscriptions, broken.entitlements, broken.payg, broken.clock, broken.catalog,
					broken.funder)
			}()
		})
	}
}

func TestCreatePlanPersistsTheRootAndSurfacesNameTaken(t *testing.T) {
	world := newCommerceWorld(t)
	commerceUse := newCommerce(world)

	plan, err := commerceUse.CreatePlan(t.Context(), "team")
	if err != nil {
		t.Fatalf("CreatePlan returned error: %v", err)
	}
	if _, ok := world.plans[plan.ID]; !ok {
		t.Fatal("the plan root never landed")
	}
	if !slices.Contains(world.order, "begin") || !slices.Contains(world.order, "commit") {
		t.Fatalf("the insert did not travel as one unit of work: %v", world.order)
	}

	if _, err := commerceUse.CreatePlan(t.Context(), "team"); !errors.Is(err, commerce.ErrPlanNameTaken) {
		t.Fatalf("duplicate name error = %v, want commerce.ErrPlanNameTaken", err)
	}
	if _, err := commerceUse.CreatePlan(t.Context(), "   "); !errors.Is(err, commerce.ErrInvalidPlanName) {
		t.Fatalf("blank name error = %v, want commerce.ErrInvalidPlanName", err)
	}
}

func TestOpenPlanVersionNumbersFromTheHighestAndRetriesTheRace(t *testing.T) {
	world := newCommerceWorld(t)
	commerceUse := newCommerce(world)
	plan, err := commerceUse.CreatePlan(t.Context(), "team")
	if err != nil {
		t.Fatalf("CreatePlan returned error: %v", err)
	}

	first, err := commerceUse.OpenPlanVersion(t.Context(), plan.ID, commerce.RecurringPeriodCalendarMonth, 1900)
	if err != nil {
		t.Fatalf("first OpenPlanVersion returned error: %v", err)
	}
	if first.VersionNumber != 1 || first.State != commerce.PlanVersionDraft {
		t.Fatalf("first version = number %d state %q, want 1 draft", first.VersionNumber, first.State)
	}
	second, err := commerceUse.OpenPlanVersion(t.Context(), plan.ID, commerce.RecurringPeriodCalendarMonth, 2900)
	if err != nil {
		t.Fatalf("second OpenPlanVersion returned error: %v", err)
	}
	if second.VersionNumber != 2 {
		t.Fatalf("second version number = %d, want 2", second.VersionNumber)
	}

	// The race: another author's draft lands beside this attempt, the insert
	// collides, and the retry reads the new highest number and opens the one
	// after it — the outcome the race's winner already produced.
	world.competingVersionOnCreate = true
	third, err := commerceUse.OpenPlanVersion(t.Context(), plan.ID, commerce.RecurringPeriodCalendarMonth, 3900)
	if err != nil {
		t.Fatalf("contended OpenPlanVersion returned error: %v", err)
	}
	if third.VersionNumber != 3 {
		t.Fatalf("contended version number = %d, want 3", third.VersionNumber)
	}

	if _, err := commerceUse.OpenPlanVersion(t.Context(), commerce.PlanID("0198f0a4-3f6c-7000-8000-00000000000a"), commerce.RecurringPeriodCalendarMonth, 100); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown plan error = %v, want persistence.ErrNotFound", err)
	}
}

func TestAddGrantDefinitionIsDraftOnlyAndScopeUnique(t *testing.T) {
	world := newCommerceWorld(t)
	commerceUse := newCommerce(world)
	plan, err := commerceUse.CreatePlan(t.Context(), "team")
	if err != nil {
		t.Fatalf("CreatePlan returned error: %v", err)
	}
	draft, err := commerceUse.OpenPlanVersion(t.Context(), plan.ID, commerce.RecurringPeriodCalendarMonth, 1900)
	if err != nil {
		t.Fatalf("OpenPlanVersion returned error: %v", err)
	}

	named, err := commerceUse.AddGrantDefinition(t.Context(), draft.ID, "api", commerce.DimensionCost, 5000)
	if err != nil {
		t.Fatalf("AddGrantDefinition returned error: %v", err)
	}
	if _, err := commerceUse.AddGrantDefinition(t.Context(), draft.ID, "api", commerce.DimensionCost, 6000); !errors.Is(err, commerce.ErrDuplicateGrantScope) {
		t.Fatalf("duplicate scope error = %v, want commerce.ErrDuplicateGrantScope", err)
	}
	if _, err := commerceUse.AddGrantDefinition(t.Context(), draft.ID, "*", commerce.DimensionCost, 100); err != nil {
		t.Fatalf("wildcard scope refused: %v", err)
	}

	if err := commerceUse.PublishPlanVersion(t.Context(), draft.ID); err != nil {
		t.Fatalf("PublishPlanVersion returned error: %v", err)
	}
	if _, err := commerceUse.AddGrantDefinition(t.Context(), draft.ID, "storage", commerce.DimensionCost, 500); !errors.Is(err, commerce.ErrVersionNotEditable) {
		t.Fatalf("edit after publication error = %v, want commerce.ErrVersionNotEditable", err)
	}
	if len(world.defs[draft.ID]) != 2 || world.defs[draft.ID][0].ID != named.ID {
		t.Fatalf("the published version's definition set moved: %+v", world.defs[draft.ID])
	}
}

func TestPublishAndRetireDriveTheVersionMachine(t *testing.T) {
	world := newCommerceWorld(t)
	commerceUse := newCommerce(world)
	plan, err := commerceUse.CreatePlan(t.Context(), "team")
	if err != nil {
		t.Fatalf("CreatePlan returned error: %v", err)
	}
	draft, err := commerceUse.OpenPlanVersion(t.Context(), plan.ID, commerce.RecurringPeriodCalendarMonth, 1900)
	if err != nil {
		t.Fatalf("OpenPlanVersion returned error: %v", err)
	}

	if err := commerceUse.RetirePlanVersion(t.Context(), draft.ID); !errors.Is(err, commerce.ErrInvalidTransition) {
		t.Fatalf("retire of a draft error = %v, want commerce.ErrInvalidTransition", err)
	}
	if err := commerceUse.PublishPlanVersion(t.Context(), draft.ID); err != nil {
		t.Fatalf("PublishPlanVersion returned error: %v", err)
	}
	// The idempotent no-op: the second publication confirms without a swap —
	// the fake logs its guarded writes, so a second "versions.publish" entry
	// would mean the confirmation rewrote the row.
	if err := commerceUse.PublishPlanVersion(t.Context(), draft.ID); err != nil {
		t.Fatalf("republish returned error: %v", err)
	}
	if count(world.order, "versions.publish") != 1 {
		t.Fatalf("publish swaps = %d, want the first one only", count(world.order, "versions.publish"))
	}
	if err := commerceUse.RetirePlanVersion(t.Context(), draft.ID); err != nil {
		t.Fatalf("RetirePlanVersion returned error: %v", err)
	}
	if count(world.order, "versions.retire") != 1 {
		t.Fatalf("retire swaps = %d, want 1", count(world.order, "versions.retire"))
	}
	// Retired is terminal: the subscriptions already pinned keep the version
	// — nothing here rewrites any subscription row.
}

func TestSubscribeRequiresAnActiveAccountAndAPublishedVersion(t *testing.T) {
	world := newCommerceWorld(t)
	commerceUse := newCommerce(world)
	accountID := world.seedAccount(t)
	_, published := world.seedPublishedVersion(t, mustGrantDefinition(t, "", "api"))

	start := world.now.Add(24 * time.Hour)
	suspended := world.seedAccount(t)
	world.suspend(suspended)

	if _, err := commerceUse.Subscribe(t.Context(), commerce.AccountID(suspended), published, start, true); !errors.Is(err, identity.ErrAccountNotActive) {
		t.Fatalf("suspended account error = %v, want identity.ErrAccountNotActive", err)
	}

	// A draft copy of the published version, filed beside it under its own id.
	draft := world.versions[published]
	draftID, err := commerce.NewPlanVersionID()
	if err != nil {
		t.Fatalf("NewPlanVersionID: %v", err)
	}
	draft.ID = draftID
	draft.State = commerce.PlanVersionDraft
	world.versions[draftID] = draft
	if _, err := commerceUse.Subscribe(t.Context(), commerce.AccountID(accountID), draftID, start, true); !errors.Is(err, commerce.ErrPlanVersionNotPublished) {
		t.Fatalf("draft version error = %v, want commerce.ErrPlanVersionNotPublished", err)
	}

	subscription, err := commerceUse.Subscribe(t.Context(), commerce.AccountID(accountID), published, start, true)
	if err != nil {
		t.Fatalf("Subscribe returned error: %v", err)
	}
	if subscription.State != commerce.SubscriptionPending || subscription.CycleNumber != nil {
		t.Fatalf("born subscription = %q with cycle %v, want pending with null cycle", subscription.State, subscription.CycleNumber)
	}
}

func TestSubscriptionLifecycleMoves(t *testing.T) {
	world := newCommerceWorld(t)
	commerceUse := newCommerce(world)
	accountID := world.seedAccount(t)
	_, versionID := world.seedPublishedVersion(t)
	start := world.now.Add(-24 * time.Hour)
	subscription := world.seedSubscription(t, accountID, versionID, start, true)

	t.Run("suspension and reinstatement round-trip through the machine", func(t *testing.T) {
		// Promote first: only an active subscription suspends.
		if _, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10); err != nil {
			t.Fatalf("PromoteDueSubscriptions returned error: %v", err)
		}
		if err := commerceUse.SuspendSubscription(t.Context(), subscription.ID); err != nil {
			t.Fatalf("SuspendSubscription returned error: %v", err)
		}
		if world.subs[subscription.ID].State != commerce.SubscriptionSuspended {
			t.Fatalf("state = %q, want suspended", world.subs[subscription.ID].State)
		}
		if err := commerceUse.ReinstateSubscription(t.Context(), subscription.ID); err != nil {
			t.Fatalf("ReinstateSubscription returned error: %v", err)
		}
		if world.subs[subscription.ID].State != commerce.SubscriptionActive {
			t.Fatalf("state = %q, want active", world.subs[subscription.ID].State)
		}
	})

	t.Run("a scheduled cancellation keeps the subscription until its instant and then ends it", func(t *testing.T) {
		cancelAt := world.now.Add(48 * time.Hour)
		if err := commerceUse.ScheduleSubscriptionCancellation(t.Context(), subscription.ID, cancelAt); err != nil {
			t.Fatalf("ScheduleSubscriptionCancellation returned error: %v", err)
		}
		if world.subs[subscription.ID].State != commerce.SubscriptionActive {
			t.Fatal("scheduling ended the subscription early")
		}
		if world.subs[subscription.ID].CancelAt == nil || !world.subs[subscription.ID].CancelAt.Equal(cancelAt) {
			t.Fatalf("cancel_at = %v, want %v", world.subs[subscription.ID].CancelAt, cancelAt)
		}

		world.now = cancelAt
		cancelled, err := commerceUse.CompleteDueCancellations(t.Context(), 10)
		if err != nil {
			t.Fatalf("CompleteDueCancellations returned error: %v", err)
		}
		if cancelled != 1 {
			t.Fatalf("cancelled = %d, want 1", cancelled)
		}
		if world.subs[subscription.ID].State != commerce.SubscriptionCancelled {
			t.Fatalf("state = %q, want cancelled", world.subs[subscription.ID].State)
		}
		if !world.subs[subscription.ID].CancelAt.Equal(cancelAt) {
			t.Fatalf("completion moved cancel_at to %v, want the instructed %v", world.subs[subscription.ID].CancelAt, cancelAt)
		}
	})

	t.Run("cancelling now stamps the instruction beside the state", func(t *testing.T) {
		// The cancelled row is more than a state: cancel_at and the mode
		// travel with it, because the record is what says the customer asked
		// to stop and when the stop took effect.
		world := newCommerceWorld(t)
		commerceUse := newCommerce(world)
		accountID := world.seedAccount(t)
		_, versionID := world.seedPublishedVersion(t)
		subscription := world.seedSubscription(t, accountID, versionID, world.now, true)
		if _, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10); err != nil {
			t.Fatalf("PromoteDueSubscriptions returned error: %v", err)
		}
		if err := commerceUse.CancelSubscriptionNow(t.Context(), subscription.ID); err != nil {
			t.Fatalf("CancelSubscriptionNow returned error: %v", err)
		}
		row := world.subs[subscription.ID]
		if row.State != commerce.SubscriptionCancelled ||
			row.CancellationMode != commerce.CancellationImmediate ||
			row.CancelAt == nil {
			t.Fatalf("cancelled row = %q mode %q cancel_at %v, want cancelled, immediate, stamped",
				row.State, row.CancellationMode, row.CancelAt)
		}
	})

	t.Run("a rescheduled instruction waits for its new instant", func(t *testing.T) {
		world := newCommerceWorld(t)
		commerceUse := newCommerce(world)
		accountID := world.seedAccount(t)
		_, versionID := world.seedPublishedVersion(t)
		subscription := world.seedSubscription(t, accountID, versionID, world.now, true)
		if _, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10); err != nil {
			t.Fatalf("PromoteDueSubscriptions returned error: %v", err)
		}
		first := world.now.Add(48 * time.Hour)
		if err := commerceUse.ScheduleSubscriptionCancellation(t.Context(), subscription.ID, first); err != nil {
			t.Fatalf("ScheduleSubscriptionCancellation returned error: %v", err)
		}
		world.now = first
		later := first.Add(24 * time.Hour)
		if err := commerceUse.ScheduleSubscriptionCancellation(t.Context(), subscription.ID, later); err != nil {
			t.Fatalf("the reschedule returned error: %v", err)
		}

		// The lane that arrives between the two instructions finds an
		// instant the clock has not reached, and waits.
		cancelled, err := commerceUse.CompleteDueCancellations(t.Context(), 10)
		if err != nil {
			t.Fatalf("CompleteDueCancellations returned error: %v", err)
		}
		if cancelled != 0 || world.subs[subscription.ID].State != commerce.SubscriptionActive {
			t.Fatalf("cancelled = %d state = %q, want the rescheduled instruction to wait",
				cancelled, world.subs[subscription.ID].State)
		}

		world.now = later
		cancelled, err = commerceUse.CompleteDueCancellations(t.Context(), 10)
		if err != nil {
			t.Fatalf("CompleteDueCancellations returned error: %v", err)
		}
		row := world.subs[subscription.ID]
		if cancelled != 1 || row.State != commerce.SubscriptionCancelled || row.CancelAt == nil || !row.CancelAt.Equal(later) {
			t.Fatalf("cancelled = %d row = %q cancel_at %v, want one completion that keeps the instructed instant",
				cancelled, row.State, row.CancelAt)
		}
	})

	t.Run("cancelling now is idempotent", func(t *testing.T) {
		world := newCommerceWorld(t)
		commerceUse := newCommerce(world)
		accountID := world.seedAccount(t)
		_, versionID := world.seedPublishedVersion(t)
		subscription := world.seedSubscription(t, accountID, versionID, world.now, true)
		if _, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10); err != nil {
			t.Fatalf("PromoteDueSubscriptions returned error: %v", err)
		}
		if err := commerceUse.CancelSubscriptionNow(t.Context(), subscription.ID); err != nil {
			t.Fatalf("CancelSubscriptionNow returned error: %v", err)
		}
		if err := commerceUse.CancelSubscriptionNow(t.Context(), subscription.ID); err != nil {
			t.Fatalf("retried CancelSubscriptionNow returned error: %v", err)
		}
		if world.subs[subscription.ID].State != commerce.SubscriptionCancelled {
			t.Fatalf("state = %q, want cancelled", world.subs[subscription.ID].State)
		}
	})
}

func TestPaygEnableDisableAndBucketAssignment(t *testing.T) {
	world := newCommerceWorld(t)
	commerceUse := newCommerce(world)
	accountID := world.seedAccount(t)
	commerceAccountID := commerce.AccountID(accountID)
	bucketID := commerce.FundingBucketID("0198f0a4-3f6c-7000-8000-0000000000b1")
	otherBucket := commerce.FundingBucketID("0198f0a4-3f6c-7000-8000-0000000000b2")

	suspended := world.seedAccount(t)
	world.suspend(suspended)
	if err := commerceUse.EnableAccountPayg(t.Context(), commerce.AccountID(suspended)); !errors.Is(err, identity.ErrAccountNotActive) {
		t.Fatalf("enable on suspended account error = %v, want identity.ErrAccountNotActive", err)
	}

	// Enabling before the choreography has put a bucket on file is refused: a
	// spending authorisation that names no bucket authorises draws from
	// nothing.
	if err := commerceUse.EnableAccountPayg(t.Context(), commerceAccountID); !errors.Is(err, commerce.ErrInvalidTransition) {
		t.Fatalf("enable without a bucket on file error = %v, want commerce.ErrInvalidTransition", err)
	}
	if _, ok := world.paygRows[commerceAccountID]; ok {
		t.Fatal("the refused enable materialised a PAYG row")
	}

	// The choreography's assignment brings the row into being with PAYG off —
	// a reference funds and authorises nothing by itself.
	if err := commerceUse.AssignAccountFundingBucket(t.Context(), commerceAccountID, bucketID); err != nil {
		t.Fatalf("AssignAccountFundingBucket returned error: %v", err)
	}
	if world.paygRows[commerceAccountID].FundingBucketID != bucketID || world.paygRows[commerceAccountID].Enabled {
		t.Fatalf("PAYG row after assignment = %+v, want the bucket on file with the flag still off", world.paygRows[commerceAccountID])
	}

	// Re-assigning the same bucket is convergence, not an error; a different
	// one is the write-once refusal.
	if err := commerceUse.AssignAccountFundingBucket(t.Context(), commerceAccountID, bucketID); err != nil {
		t.Fatalf("re-assigning the bucket on file returned error: %v", err)
	}
	if err := commerceUse.AssignAccountFundingBucket(t.Context(), commerceAccountID, otherBucket); !errors.Is(err, commerce.ErrInvalidTransition) {
		t.Fatalf("reassignment error = %v, want the write-once refusal", err)
	}
	if world.paygRows[commerceAccountID].FundingBucketID != bucketID {
		t.Fatalf("bucket on file = %s, want the refused reassignment to have changed nothing", world.paygRows[commerceAccountID].FundingBucketID)
	}

	// With a bucket on file the flag flips, and the flip never touches the
	// bucket; the off-switch below is unconditional.
	if err := commerceUse.EnableAccountPayg(t.Context(), commerceAccountID); err != nil {
		t.Fatalf("EnableAccountPayg returned error: %v", err)
	}
	if !world.paygRows[commerceAccountID].Enabled || world.paygRows[commerceAccountID].FundingBucketID != bucketID {
		t.Fatalf("enabled PAYG row = %+v, want the flag on with the bucket untouched", world.paygRows[commerceAccountID])
	}
	if err := commerceUse.DisableAccountPayg(t.Context(), commerceAccountID); err != nil {
		t.Fatalf("DisableAccountPayg returned error: %v", err)
	}
	if world.paygRows[commerceAccountID].Enabled || world.paygRows[commerceAccountID].FundingBucketID != bucketID {
		t.Fatalf("disabled PAYG row = %+v, want the flag off with the bucket untouched", world.paygRows[commerceAccountID])
	}

	// The stalled race: the write reports a lost swap and lands nothing, and
	// the use case reads what did land. A different bucket got there first —
	// the defect the write-once guard exists to stop — so the read names it
	// and refuses; the winner's own bucket re-fired is convergence.
	world2 := newCommerceWorld(t)
	use2 := newCommerce(world2)
	account2 := world2.seedAccount(t)
	if err := use2.AssignAccountFundingBucket(t.Context(), commerce.AccountID(account2), otherBucket); err != nil {
		t.Fatalf("AssignAccountFundingBucket returned error: %v", err)
	}
	world2.stallPaygAssign = true
	if err := use2.AssignAccountFundingBucket(t.Context(), commerce.AccountID(account2), bucketID); !errors.Is(err, commerce.ErrInvalidTransition) {
		t.Fatalf("a stalled assignment racing a different bucket = %v, want the write-once refusal", err)
	}
	world2.stallPaygAssign = false
	if err := use2.AssignAccountFundingBucket(t.Context(), commerce.AccountID(account2), otherBucket); err != nil {
		t.Fatalf("re-firing the winner's own assignment returned error: %v", err)
	}

	// Disabling an account that never enabled PAYG records the flag
	// explicitly instead of failing — and the row that disable created
	// carries no bucket, so enabling it is refused: the second branch of
	// the enable gate, and the realistic path a bucket-less row takes.
	world3 := newCommerceWorld(t)
	use3 := newCommerce(world3)
	account3 := world3.seedAccount(t)
	payg3 := commerce.AccountID(account3)
	if err := use3.DisableAccountPayg(t.Context(), payg3); err != nil {
		t.Fatalf("DisableAccountPayg on an unknown account returned error: %v", err)
	}
	if world3.paygRows[payg3].Enabled {
		t.Fatal("the explicit disable did not land")
	}
	if err := use3.EnableAccountPayg(t.Context(), payg3); !errors.Is(err, commerce.ErrInvalidTransition) {
		t.Fatalf("enable on a bucket-less row error = %v, want commerce.ErrInvalidTransition", err)
	}
	if world3.paygRows[payg3].Enabled {
		t.Fatal("the refused enable flipped the flag on")
	}

	// A malformed bucket reference is refused in the domain's own words,
	// before any statement runs.
	world4 := newCommerceWorld(t)
	use4 := newCommerce(world4)
	account4 := world4.seedAccount(t)
	if err := use4.AssignAccountFundingBucket(t.Context(), commerce.AccountID(account4), "not-a-uuid"); !errors.Is(err, commerce.ErrInvalidFundingBucketID) {
		t.Fatalf("assign with a malformed bucket id error = %v, want commerce.ErrInvalidFundingBucketID", err)
	}
	if _, ok := world4.paygRows[commerce.AccountID(account4)]; ok {
		t.Fatal("the refused assignment materialised a PAYG row")
	}
}

func TestEffectiveEntitlementsDeriveTheWaterfall(t *testing.T) {
	world := newCommerceWorld(t)
	commerceUse := newCommerce(world)
	accountID := world.seedAccount(t)
	named := mustGrantDefinition(t, "", "api")
	wild := mustGrantDefinition(t, "", "*")
	_, versionID := world.seedPublishedVersion(t, named, wild)

	// The subscription's first cycle covers the derivation instant: promoted
	// just after its start, asked at an instant the cycle still covers.
	start := world.now.Add(-48 * time.Hour)
	subscription := world.seedSubscription(t, accountID, versionID, start, true)
	world.now = start.Add(25 * time.Hour)
	if _, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10); err != nil {
		t.Fatalf("PromoteDueSubscriptions returned error: %v", err)
	}

	effective, err := commerceUse.EffectiveEntitlements(t.Context(), commerce.AccountID(accountID), world.now)
	if err != nil {
		t.Fatalf("EffectiveEntitlements returned error: %v", err)
	}
	if len(effective) != 2 {
		t.Fatalf("effective = %d grants, want the named and the wildcard", len(effective))
	}
	if effective[0].IsWildcard || !effective[1].IsWildcard {
		t.Fatalf("waterfall order wrong: named=%v wildcard=%v", effective[0].IsWildcard, effective[1].IsWildcard)
	}
	if effective[0].SubscriptionID != subscription.ID {
		t.Fatalf("named grant subscription = %s, want %s", effective[0].SubscriptionID, subscription.ID)
	}
	if effective[0].GrantedAmount != named.GrantedAmount || effective[1].GrantedAmount != wild.GrantedAmount {
		t.Fatalf("amounts = %d/%d, want the definitions' %d/%d",
			effective[0].GrantedAmount, effective[1].GrantedAmount, named.GrantedAmount, wild.GrantedAmount)
	}
	for _, grant := range effective {
		resolved := world.groupVersions[string(grant.AliasGroupName)]
		if grant.AliasGroupVersionID != commerce.AliasGroupVersionID(resolved.GroupVersionID) {
			t.Fatalf("grant %s scope id = %s, want the resolved catalog id %s",
				grant.EntitlementID, grant.AliasGroupVersionID, resolved.GroupVersionID)
		}
	}
}

func TestPromotionLane(t *testing.T) {
	definitions := func() []commerce.GrantDefinition {
		return []commerce.GrantDefinition{
			mustGrantDefinition(t, "", "api"),
			mustGrantDefinition(t, "", "*"),
		}
	}

	t.Run("a due pending subscription promotes with cycle 1's entitlements", func(t *testing.T) {
		world := newCommerceWorld(t)
		commerceUse := newCommerce(world)
		accountID := world.seedAccount(t)
		defs := definitions()
		_, versionID := world.seedPublishedVersion(t, defs...)
		world.seedSubscription(t, accountID, versionID, world.now.Add(-time.Hour), true)

		promoted, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10)
		if err != nil {
			t.Fatalf("PromoteDueSubscriptions returned error: %v", err)
		}
		if promoted != 1 {
			t.Fatalf("promoted = %d, want 1", promoted)
		}
		for _, subscription := range world.subs {
			if subscription.State != commerce.SubscriptionActive || subscription.CycleNumber == nil || *subscription.CycleNumber != 1 {
				t.Fatalf("promoted subscription = %q cycle %v", subscription.State, subscription.CycleNumber)
			}
			if subscription.PeriodStart == nil || !subscription.PeriodStart.Equal(subscription.StartAt) {
				t.Fatalf("cycle 1 anchored at %v, want the start_at %v", subscription.PeriodStart, subscription.StartAt)
			}
		}
		if len(world.ents) != 2 {
			t.Fatalf("entitlements = %d, want one per definition", len(world.ents))
		}
		scopes := map[commerce.AliasGroupVersionID]bool{}
		for _, definition := range defs {
			scopes[commerce.AliasGroupVersionID(world.groupVersions[string(definition.AliasGroupName)].GroupVersionID)] = true
		}
		seen := map[commerce.AliasGroupVersionID]bool{}
		for _, entitlement := range world.ents {
			if entitlement.CycleNumber != 1 || entitlement.State != commerce.EntitlementActive {
				t.Fatalf("materialised entitlement = cycle %d state %q", entitlement.CycleNumber, entitlement.State)
			}
			if !entitlement.CreatedAt.Equal(world.now) {
				t.Fatalf("entitlement stamped %v, want the database clock's %v", entitlement.CreatedAt, world.now)
			}
			if !scopes[entitlement.AliasGroupVersionID] {
				t.Fatalf("entitlement scope id %q is not one of the resolved catalog ids", entitlement.AliasGroupVersionID)
			}
			seen[entitlement.AliasGroupVersionID] = true
		}
		if len(seen) != 2 {
			t.Fatalf("the definitions resolved to %d distinct scopes, want 2", len(seen))
		}
		if len(world.scopeReads) != 2 {
			t.Fatalf("the catalog was asked %d times, want once per definition", len(world.scopeReads))
		}
	})

	t.Run("the catalog seam is asked before the unit of work opens", func(t *testing.T) {
		world := newCommerceWorld(t)
		commerceUse := newCommerce(world)
		accountID := world.seedAccount(t)
		_, versionID := world.seedPublishedVersion(t, definitions()...)
		world.seedSubscription(t, accountID, versionID, world.now.Add(-time.Hour), true)

		if _, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10); err != nil {
			t.Fatalf("PromoteDueSubscriptions returned error: %v", err)
		}
		// Both resolutions happened, and the pass opened exactly one unit of
		// work for the one subscription: the reads answered the scope map the
		// transaction consumes, so nothing inside it needed the seam again.
		if len(world.scopeReads) != 2 {
			t.Fatalf("scope reads = %d, want 2", len(world.scopeReads))
		}
		if count(world.order, "begin") != 1 || count(world.order, "commit") != 1 {
			t.Fatalf("units of work = %v, want one begin and one commit", world.order)
		}
	})

	t.Run("a subscription that moved on after the scan is skipped without an error", func(t *testing.T) {
		world := newCommerceWorld(t)
		commerceUse := newCommerce(world)
		accountID := world.seedAccount(t)
		_, versionID := world.seedPublishedVersion(t, definitions()...)
		due := world.seedSubscription(t, accountID, versionID, world.now.Add(-time.Hour), true)
		world.forceScanIDs = []commerce.SubscriptionID{due.ID}
		// The row is no longer pending: the scan's answer is stale.
		world.moveOn(due.ID, commerce.SubscriptionCancelled)

		promoted, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10)
		if err != nil {
			t.Fatalf("PromoteDueSubscriptions returned error: %v", err)
		}
		if promoted != 0 {
			t.Fatalf("promoted = %d, want the race to be a skip", promoted)
		}
	})

	t.Run("a guard that will not fire rolls the entitlements back", func(t *testing.T) {
		world := newCommerceWorld(t)
		commerceUse := newCommerce(world)
		accountID := world.seedAccount(t)
		_, versionID := world.seedPublishedVersion(t, definitions()...)
		world.seedSubscription(t, accountID, versionID, world.now.Add(-time.Hour), true)
		// The world moves between the use case's read and the verdict: the
		// account gate stops firing. The unit of work must roll back whole —
		// no promotion, no grants — and the lane must not fail.
		world.guardAccountInactive = true

		promoted, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10)
		if err != nil {
			t.Fatalf("PromoteDueSubscriptions returned error: %v", err)
		}
		if promoted != 0 || len(world.ents) != 0 {
			t.Fatalf("promoted = %d entitlements = %d, want a clean rollback", promoted, len(world.ents))
		}
		for _, subscription := range world.subs {
			if subscription.State != commerce.SubscriptionPending {
				t.Fatalf("state after rollback = %q, want pending", subscription.State)
			}
		}
		if !slices.Contains(world.order, "rollback") {
			t.Fatalf("the world never rolled back: %v", world.order)
		}
	})

	t.Run("an unanswerable catalog stops the lane", func(t *testing.T) {
		world := newCommerceWorld(t)
		commerceUse := newCommerce(world)
		accountID := world.seedAccount(t)
		_, versionID := world.seedPublishedVersion(t, definitions()...)
		world.seedSubscription(t, accountID, versionID, world.now.Add(-time.Hour), true)

		world.catalogErr = errors.New("connection refused")
		if _, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10); err == nil {
			t.Fatal("a transport failure returned nil — the pass must end, not lie")
		}

		world.catalogErr = nil
		delete(world.groupVersions, "api")
		if _, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10); !errors.Is(err, dataplane.ErrGroupNotFound) {
			t.Fatalf("missing group error = %v, want dataplane.ErrGroupNotFound", err)
		}
	})

	t.Run("promotion funds each materialised cycle's bucket", func(t *testing.T) {
		world := newCommerceWorld(t)
		commerceUse := newCommerce(world)
		accountID := world.seedAccount(t)
		defs := definitions()
		_, versionID := world.seedPublishedVersion(t, defs...)
		world.seedSubscription(t, accountID, versionID, world.now.Add(-time.Hour), true)

		if _, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10); err != nil {
			t.Fatalf("PromoteDueSubscriptions returned error: %v", err)
		}
		if len(world.funded) != len(defs) {
			t.Fatalf("funded entitlements = %d, want one per definition", len(world.funded))
		}
		for entitlementID, granted := range world.funded {
			if granted != 5000 {
				t.Fatalf("entitlement %s funded with %d, want the definition's 5000", entitlementID, granted)
			}
		}
	})

	t.Run("a funder refusal fails the roll and unfunds the cycle", func(t *testing.T) {
		world := newCommerceWorld(t)
		commerceUse := newCommerce(world)
		accountID := world.seedAccount(t)
		_, versionID := world.seedPublishedVersion(t, definitions()...)
		world.seedSubscription(t, accountID, versionID, world.now.Add(-time.Hour), true)
		// The accounting half refuses inside the unit of work: a cycle whose
		// grants bought nothing must not exist half-funded, so the
		// entitlements and the promotion roll back with the funding.
		world.failOn["funder.fund"] = errors.New("the ledger refused the grant")

		if _, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10); err == nil {
			t.Fatal("a funding failure returned nil — the roll must fail loudly")
		}
		if len(world.ents) != 0 || len(world.funded) != 0 {
			t.Fatalf("after the failed roll: %d entitlements, %d funded — want both empty",
				len(world.ents), len(world.funded))
		}
		for _, subscription := range world.subs {
			if subscription.State != commerce.SubscriptionPending {
				t.Fatalf("state after the failed roll = %q, want pending", subscription.State)
			}
		}
		if !slices.Contains(world.order, "rollback") {
			t.Fatalf("the world never rolled back: %v", world.order)
		}
	})
}

func TestRollLane(t *testing.T) {
	// A subscription anchored a day before the world's clock, promoted once,
	// then the clock advanced past the first cycle's end so the roll is due.
	seedActive := func(t *testing.T, renewal bool) (*commerceWorld, *Commerce, commerce.Subscription) {
		t.Helper()
		world := newCommerceWorld(t)
		commerceUse := newCommerce(world)
		accountID := world.seedAccount(t)
		_, versionID := world.seedPublishedVersion(t,
			mustGrantDefinition(t, "", "api"),
			mustGrantDefinition(t, "", "*"),
		)
		subscription := world.seedSubscription(t, accountID, versionID, world.now.Add(-24*time.Hour), renewal)
		if _, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10); err != nil {
			t.Fatalf("PromoteDueSubscriptions returned error: %v", err)
		}
		world.now = world.now.Add(31 * 24 * time.Hour)
		return world, commerceUse, subscription
	}

	t.Run("an ended period rolls with its own cycle's entitlements", func(t *testing.T) {
		world, commerceUse, subscription := seedActive(t, true)
		before := len(world.ents)

		rolled, err := commerceUse.RollDueSubscriptions(t.Context(), 10)
		if err != nil {
			t.Fatalf("RollDueSubscriptions returned error: %v", err)
		}
		if rolled != 1 {
			t.Fatalf("rolled = %d, want 1", rolled)
		}
		after := world.subs[subscription.ID]
		if after.CycleNumber == nil || *after.CycleNumber != 2 {
			t.Fatalf("cycle = %v, want 2", after.CycleNumber)
		}
		if len(world.ents) != before+2 {
			t.Fatalf("entitlements = %d, want %d — one per definition for cycle 2", len(world.ents), before+2)
		}
		for _, entitlement := range world.ents {
			if entitlement.CycleNumber == 2 && !entitlement.PeriodEnd.After(world.now) {
				t.Fatal("cycle 2's bounds do not reach past the roll instant")
			}
		}
	})

	t.Run("a fixed-term subscription is never scanned", func(t *testing.T) {
		_, commerceUse, _ := seedActive(t, false)
		rolled, err := commerceUse.RollDueSubscriptions(t.Context(), 10)
		if err != nil {
			t.Fatalf("RollDueSubscriptions returned error: %v", err)
		}
		if rolled != 0 {
			t.Fatalf("rolled = %d, want 0", rolled)
		}
	})

	t.Run("a cancellation due inside the new cycle suppresses the roll benignly", func(t *testing.T) {
		world, commerceUse, subscription := seedActive(t, true)
		before := len(world.ents)
		cancelAt := *world.subs[subscription.ID].PeriodEnd // exactly the boundary the cycle would not survive
		if err := commerceUse.ScheduleSubscriptionCancellation(t.Context(), subscription.ID, cancelAt); err != nil {
			t.Fatalf("ScheduleSubscriptionCancellation returned error: %v", err)
		}

		rolled, err := commerceUse.RollDueSubscriptions(t.Context(), 10)
		if err != nil {
			t.Fatalf("RollDueSubscriptions returned error: %v", err)
		}
		if rolled != 0 {
			t.Fatalf("rolled = %d, want the suppressed roll to be a skip", rolled)
		}
		if *world.subs[subscription.ID].CycleNumber != 1 || len(world.ents) != before {
			t.Fatal("the suppressed roll advanced something")
		}

		// The cancellation lane, ordered first in a pass, ends it instead.
		cancelled, err := commerceUse.CompleteDueCancellations(t.Context(), 10)
		if err != nil {
			t.Fatalf("CompleteDueCancellations returned error: %v", err)
		}
		if cancelled != 1 || world.subs[subscription.ID].State != commerce.SubscriptionCancelled {
			t.Fatalf("cancellation lane did not complete the instruction: cancelled=%d state=%q",
				cancelled, world.subs[subscription.ID].State)
		}
	})

	t.Run("a guard that will not fire rolls nothing back and fails nothing", func(t *testing.T) {
		world, commerceUse, subscription := seedActive(t, true)
		world.guardAccountInactive = true

		rolled, err := commerceUse.RollDueSubscriptions(t.Context(), 10)
		if err != nil {
			t.Fatalf("RollDueSubscriptions returned error: %v", err)
		}
		if rolled != 0 {
			t.Fatalf("rolled = %d, want a benign skip", rolled)
		}
		if *world.subs[subscription.ID].CycleNumber != 1 || len(world.ents) != 2 {
			t.Fatalf("cycle = %v entitlements = %d, want cycle 1 untouched with its two grants",
				world.subs[subscription.ID].CycleNumber, len(world.ents))
		}
	})

	t.Run("a row that moved after the scan is skipped", func(t *testing.T) {
		world, commerceUse, subscription := seedActive(t, true)
		world.forceScanIDs = []commerce.SubscriptionID{subscription.ID}
		world.moveOn(subscription.ID, commerce.SubscriptionExpired)

		rolled, err := commerceUse.RollDueSubscriptions(t.Context(), 10)
		if err != nil {
			t.Fatalf("RollDueSubscriptions returned error: %v", err)
		}
		if rolled != 0 {
			t.Fatalf("rolled = %d, want the stale scan to skip", rolled)
		}
	})

	t.Run("a retired version still feeds its subscribers' rolls", func(t *testing.T) {
		// Retirement stops new sales; it rewrites nothing. The subscription
		// pinned the version forever, so its cycles keep rolling on the
		// version's definitions — freezing them would be the retroactive
		// rewrite the model forbids.
		world, commerceUse, subscription := seedActive(t, true)
		versionID := world.subs[subscription.ID].PlanVersionID
		version := world.versions[versionID]
		if err := version.Retire(world.now); err != nil {
			t.Fatalf("Retire returned error: %v", err)
		}
		world.versions[versionID] = version
		before := len(world.ents)

		rolled, err := commerceUse.RollDueSubscriptions(t.Context(), 10)
		if err != nil {
			t.Fatalf("RollDueSubscriptions returned error: %v", err)
		}
		if rolled != 1 {
			t.Fatalf("rolled = %d, want the retired version to keep feeding its pinned subscriber", rolled)
		}
		if *world.subs[subscription.ID].CycleNumber != 2 || len(world.ents) != before+2 {
			t.Fatalf("cycle = %v entitlements = %d, want cycle 2 with its two grants",
				world.subs[subscription.ID].CycleNumber, len(world.ents))
		}
	})
}

func TestExpiryLanes(t *testing.T) {
	seedFixedTerm := func(t *testing.T) (*commerceWorld, *Commerce, commerce.Subscription) {
		t.Helper()
		world := newCommerceWorld(t)
		commerceUse := newCommerce(world)
		accountID := world.seedAccount(t)
		_, versionID := world.seedPublishedVersion(t, mustGrantDefinition(t, "", "api"))
		subscription := world.seedSubscription(t, accountID, versionID, world.now.Add(-24*time.Hour), false)
		if _, err := commerceUse.PromoteDueSubscriptions(t.Context(), 10); err != nil {
			t.Fatalf("PromoteDueSubscriptions returned error: %v", err)
		}
		world.now = world.now.Add(31 * 24 * time.Hour)
		return world, commerceUse, subscription
	}

	t.Run("a fixed-term subscription expires with its history intact", func(t *testing.T) {
		world, commerceUse, subscription := seedFixedTerm(t)
		expired, err := commerceUse.ExpireDueSubscriptions(t.Context(), 10)
		if err != nil {
			t.Fatalf("ExpireDueSubscriptions returned error: %v", err)
		}
		if expired != 1 {
			t.Fatalf("expired = %d, want 1", expired)
		}
		row := world.subs[subscription.ID]
		if row.State != commerce.SubscriptionExpired || row.CycleNumber == nil || *row.CycleNumber != 1 || row.PeriodEnd == nil {
			t.Fatalf("expired row = %q cycle %v, want expired with cycle 1's history", row.State, row.CycleNumber)
		}
	})

	t.Run("the entitlement expiry lane flips ended grants", func(t *testing.T) {
		world, commerceUse, _ := seedFixedTerm(t)
		expired, err := commerceUse.ExpireDueEntitlements(t.Context(), 10)
		if err != nil {
			t.Fatalf("ExpireDueEntitlements returned error: %v", err)
		}
		if expired != 1 {
			t.Fatalf("expired = %d, want 1", expired)
		}
		for _, entitlement := range world.ents {
			if entitlement.State != commerce.EntitlementExpired {
				t.Fatalf("entitlement %s stayed %q past its period end", entitlement.ID, entitlement.State)
			}
		}
	})
}

func TestRunDueWorkOrdersTheLanesAndStopsOnTheFirstError(t *testing.T) {
	world := newCommerceWorld(t)
	commerceUse := newCommerce(world)
	accountID := world.seedAccount(t)
	_, versionID := world.seedPublishedVersion(t, mustGrantDefinition(t, "", "api"))
	// Anchored far enough back that cycle 1 ends before the world's clock:
	// one pass promotes the pending subscription and then rolls its ended
	// first cycle, promotion's output feeding the roll lane's input.
	world.seedSubscription(t, accountID, versionID, world.now.Add(-40*24*time.Hour), true)

	all := DueWorkLimits{Promotions: 10, Cancellations: 10, Rolls: 10, SubscriptionExpiries: 10, EntitlementExpiries: 10}
	result, err := commerceUse.RunDueWork(t.Context(), all)
	if err != nil {
		t.Fatalf("RunDueWork returned error: %v", err)
	}
	if result.Promoted != 1 || result.Rolled != 1 {
		t.Fatalf("promoted = %d rolled = %d, want 1 and 1", result.Promoted, result.Rolled)
	}
	// The roll retired cycle 1, so its grant's cycle is over: the entitlement
	// expiry lane — the pass's last — flips exactly that one record.
	if result.Cancelled != 0 || result.Expired != 0 || result.EntitlementsExpired != 1 {
		t.Fatalf("the other lanes moved something: %+v", result)
	}
	if count(world.order, "begin") < 2 || count(world.order, "rollback") != 0 {
		t.Fatalf("units of work = %v, want at least two clean commits", world.order)
	}

	// The first lane's failure stops the pass: nothing after it runs.
	world.failOn["subscriptions.duePromotion"] = errors.New("the promotion scan failed")
	if _, err := commerceUse.RunDueWork(t.Context(), all); err == nil {
		t.Fatal("a failed lane returned nil")
	}
}

// count counts the exact occurrences of want in values.
func count(values []string, want string) int {
	var n int
	for _, value := range values {
		if value == want {
			n++
		}
	}
	return n
}
