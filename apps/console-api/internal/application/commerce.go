package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The commerce use cases: the Control Plane's commercial authority (ADR 0001,
// ADR 0003) — plans and their immutable versions, the subscriptions pinned to
// them, the entitlements each cycle materialises, and the per-account PAYG
// flag. Two kinds of method live here, and the difference between them is the
// difference between a customer waiting and a worker catching up:
//
// The interactive methods follow identity's shape — a read, a domain
// transition, and a compare-and-swap retried from the fresh state, because a
// customer's request is allowed to lose a race and try again. The due-work
// methods invert that bargain: a worker pass is not a customer to disappoint,
// its every verdict is the guarded single statement the persistence port
// carries, and a verdict that does not fire is not an error but the world
// having moved on — the row stays due, the next pass asks again, and the only
// failures a lane propagates are the ones no amount of re-asking will fix.
//
// What commerce deliberately is not, so no method below grows into it:
// the financial ledger and settlement (Accounting's, B6), the quota
// projection and runtime admission (the Data Plane's, B8), and the funding
// buckets whose ids this context stores as references and whose balances it
// never reads. Entitlement here is the grant's authoritative record — what
// was purchased, for which cycle, scoped to which catalog version — and
// nothing about what was consumed.

// errDueWorldMoved reports a due-work verdict that did not fire: the guarded
// statement's WHERE clause no longer holds because another writer moved the
// row between this pass's scan and its statement. It is the unit of work's
// rollback signal — the entitlements the roll inserted in the same unit must
// not survive a cycle that did not advance — and the lane's skip signal, the
// one benign outcome that is neither progress nor failure.
var errDueWorldMoved = errors.New("application: the row moved on before the due-work verdict could land")

// dueSkip reports whether a due-work step's error is the benign kind: the
// world moved, or the domain refused a move whose scan predicate had already
// stopped holding. Both leave the row exactly as due as it was — no money
// moved, nothing is lost — and both are answered by the next pass, which is
// why they are not returned to the lane's caller. A persistence failure, an
// unresolvable catalog scope, or a bounds arithmetic error is none of those,
// and stops the lane.
func dueSkip(err error) bool {
	return errors.Is(err, errDueWorldMoved) ||
		errors.Is(err, commerce.ErrNotDue) ||
		errors.Is(err, commerce.ErrInvalidTransition)
}

// dbNow reads the database's clock for the unit of work ctx belongs to, and
// is the only clock this file consults. transaction_timestamp() stamps every
// row this file writes and answers every due test its lanes run — a process
// wall clock deciding commercial state would write rows one instant and rule
// on them by another, and the two clocks disagreeing is precisely the race
// the guarded statements exist to settle. Inside a unit of work the port is
// transaction-stable, so one read per unit is one read per decision.
func dbNow(ctx context.Context, clock persistence.Clock, action string) (time.Time, error) {
	now, err := clock.Now(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("application: %s: read the database clock: %w", action, err)
	}
	return now, nil
}

// Commerce is the commerce foundation's use cases.
type Commerce struct {
	store         persistence.Store
	accounts      persistence.Accounts
	plans         persistence.Plans
	versions      persistence.PlanVersions
	subscriptions persistence.Subscriptions
	entitlements  persistence.Entitlements
	payg          persistence.PaygAccounts
	clock         persistence.Clock
	catalog       dataplane.CatalogReader
}

// NewCommerce builds the commerce use cases around the ports they need. It
// panics on a nil port for the reason NewIdentity panics: a port this use
// case was promised and did not get is a wiring defect, and the middle of a
// roll — after a transaction is open and an entitlement half-inserted — is a
// strictly worse place to learn about it.
//
// The catalog reader is the one port that is not a database: it is the
// cross-plane seam's group-version read (ADR 0006 §5), and the roll resolves
// every grant scope through it before opening its unit of work, so a roll
// never holds row locks while waiting on another process over a network.
func NewCommerce(
	store persistence.Store,
	accounts persistence.Accounts,
	plans persistence.Plans,
	versions persistence.PlanVersions,
	subscriptions persistence.Subscriptions,
	entitlements persistence.Entitlements,
	payg persistence.PaygAccounts,
	clock persistence.Clock,
	catalog dataplane.CatalogReader,
) *Commerce {
	switch {
	case store == nil:
		panic("application: NewCommerce requires a store")
	case accounts == nil:
		panic("application: NewCommerce requires an accounts repository")
	case plans == nil:
		panic("application: NewCommerce requires a plans repository")
	case versions == nil:
		panic("application: NewCommerce requires a plan versions repository")
	case subscriptions == nil:
		panic("application: NewCommerce requires a subscriptions repository")
	case entitlements == nil:
		panic("application: NewCommerce requires an entitlements repository")
	case payg == nil:
		panic("application: NewCommerce requires a payg accounts repository")
	case clock == nil:
		panic("application: NewCommerce requires a clock")
	case catalog == nil:
		panic("application: NewCommerce requires a catalog reader")
	}
	return &Commerce{
		store:         store,
		accounts:      accounts,
		plans:         plans,
		versions:      versions,
		subscriptions: subscriptions,
		entitlements:  entitlements,
		payg:          payg,
		clock:         clock,
		catalog:       catalog,
	}
}

// CreatePlan opens a commercial product: a validated name and the plan root's
// birth row, inserted as one unit of work. A name another plan carries
// surfaces as commerce.ErrPlanNameTaken — the schema's plans_name_key,
// translated below the port — and nothing else can refuse a plan.
func (c *Commerce) CreatePlan(ctx context.Context, name string) (commerce.Plan, error) {
	var plan *commerce.Plan
	if err := c.store.WithinTx(ctx, func(txCtx context.Context) error {
		id, err := commerce.NewPlanID()
		if err != nil {
			return fmt.Errorf("application: create plan: %w", err)
		}
		now, err := dbNow(txCtx, c.clock, "create plan")
		if err != nil {
			return err
		}
		created, err := commerce.NewPlan(id, name, now)
		if err != nil {
			return fmt.Errorf("application: create plan: %w", err)
		}
		plan = created
		return c.plans.Create(txCtx, *created)
	}); err != nil {
		return commerce.Plan{}, fmt.Errorf("application: create plan: %w", err)
	}
	return *plan, nil
}

// Plan returns the plan root with id. A miss is the persistence port's
// ErrNotFound, wrapped with what was looked for.
func (c *Commerce) Plan(ctx context.Context, id commerce.PlanID) (commerce.Plan, error) {
	plan, err := c.plans.ByID(ctx, id)
	if err != nil {
		return commerce.Plan{}, fmt.Errorf("application: read plan %s: %w", id, err)
	}
	return plan, nil
}

// OpenPlanVersion drafts the plan's next immutable version: the highest
// existing version number plus one, inserted as a draft that accepts edits
// and no subscriptions.
//
// Two authors opening a plan's next version at once both read the same
// highest number, and exactly one insert carries it — the schema's
// plan_versions_version_number_key refuses the other, as the port's
// commerce.ErrPlanVersionNumberTaken. The loser does not fail: it re-reads
// the highest number and tries again from there, because the race's outcome
// is exactly the state it wanted — the plan's next version, now opened by
// someone else. casMaxAttempts bounds the loop with the same non-tuned
// argument identity's CAS loop carries.
func (c *Commerce) OpenPlanVersion(ctx context.Context, planID commerce.PlanID, period commerce.RecurringPeriod, recurringPriceMinorUnits int64) (commerce.PlanVersion, error) {
	for attempt := 0; attempt < casMaxAttempts; attempt++ {
		var version *commerce.PlanVersion
		err := c.store.WithinTx(ctx, func(txCtx context.Context) error {
			if _, err := c.plans.ByID(txCtx, planID); err != nil {
				return fmt.Errorf("application: open plan version: read plan: %w", err)
			}
			highest, err := c.versions.HighestVersionNumber(txCtx, planID)
			if err != nil {
				return fmt.Errorf("application: open plan version: read highest version: %w", err)
			}
			now, err := dbNow(txCtx, c.clock, "open plan version")
			if err != nil {
				return err
			}
			id, err := commerce.NewPlanVersionID()
			if err != nil {
				return fmt.Errorf("application: open plan version: %w", err)
			}
			version, err = commerce.NewPlanVersion(id, planID, highest+1, period, recurringPriceMinorUnits, now)
			if err != nil {
				return fmt.Errorf("application: open plan version: %w", err)
			}
			return c.versions.Create(txCtx, *version)
		})
		switch {
		case errors.Is(err, commerce.ErrPlanVersionNumberTaken):
			// Another author opened the number this draft tried to claim:
			// re-read and try the next one.
			continue
		case err != nil:
			return commerce.PlanVersion{}, fmt.Errorf("application: open plan version of plan %s: %w", planID, err)
		}
		return *version, nil
	}
	return commerce.PlanVersion{}, fmt.Errorf("application: open plan version of plan %s: %w after %d attempts", planID, ErrTransitionContended, casMaxAttempts)
}

// PlanVersion returns the version with id, with its whole grant-definition
// set — the immutable end of the catalog, read as one unit.
func (c *Commerce) PlanVersion(ctx context.Context, id commerce.PlanVersionID) (commerce.PlanVersion, []commerce.GrantDefinition, error) {
	version, definitions, err := c.versions.ByID(ctx, id)
	if err != nil {
		return commerce.PlanVersion{}, nil, fmt.Errorf("application: read plan version %s: %w", id, err)
	}
	return version, definitions, nil
}

// AddGrantDefinition appends one grant to a draft version. The version must
// still read draft — published terms are frozen forever, and a definition
// added after publication would be an edit in exactly the sense the model
// refuses — and the (scope, dimension) pair must be the version's first.
// Both rules are checked in the domain's vocabulary here and repeated by the
// insert's own single-statement guard and the schema's scope key below it, so
// an edit racing a publication loses cleanly as commerce.ErrVersionNotEditable
// instead of landing half of an already-frozen version's terms.
func (c *Commerce) AddGrantDefinition(ctx context.Context, versionID commerce.PlanVersionID, scope commerce.AliasGroupName, dimension commerce.Dimension, grantedAmount int64) (commerce.GrantDefinition, error) {
	id, err := commerce.NewGrantDefinitionID()
	if err != nil {
		return commerce.GrantDefinition{}, fmt.Errorf("application: add grant definition: %w", err)
	}
	definition, err := commerce.NewGrantDefinition(id, versionID, scope, dimension, grantedAmount)
	if err != nil {
		return commerce.GrantDefinition{}, fmt.Errorf("application: add grant definition: %w", err)
	}
	err = c.store.WithinTx(ctx, func(txCtx context.Context) error {
		createdAt, err := dbNow(txCtx, c.clock, "add grant definition")
		if err != nil {
			return fmt.Errorf("application: add grant definition: %w", err)
		}
		definition.CreatedAt = createdAt
		version, definitions, err := c.versions.ByID(txCtx, versionID)
		if err != nil {
			return fmt.Errorf("application: add grant definition: read version: %w", err)
		}
		if version.State != commerce.PlanVersionDraft {
			return fmt.Errorf("application: add grant definition to version %s: %w: version is %s", versionID, commerce.ErrVersionNotEditable, version.State)
		}
		for _, existing := range definitions {
			if existing.AliasGroupName == scope && existing.Dimension == dimension {
				return fmt.Errorf("application: add grant definition to version %s: %w: %s/%s is already defined", versionID, commerce.ErrDuplicateGrantScope, scope, dimension)
			}
		}
		return c.versions.AddGrantDefinition(txCtx, *definition)
	})
	if err != nil {
		return commerce.GrantDefinition{}, fmt.Errorf("application: add grant definition to version %s: %w", versionID, err)
	}
	return *definition, nil
}

// PublishPlanVersion freezes a draft's terms forever. Publishing an
// already-published version is a no-op, so a retried publication converges;
// the compare-and-swap below is what makes two racing publications — or a
// publication racing an edit — resolve to one winner and one no-op instead of
// two stamps.
func (c *Commerce) PublishPlanVersion(ctx context.Context, id commerce.PlanVersionID) error {
	return c.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := dbNow(txCtx, c.clock, "publish plan version")
		if err != nil {
			return err
		}
		for attempt := 0; attempt < casMaxAttempts; attempt++ {
			version, _, err := c.versions.ByID(txCtx, id)
			if err != nil {
				return fmt.Errorf("application: publish plan version %s: %w", id, err)
			}
			before := version.State
			if err := version.Publish(now); err != nil {
				return fmt.Errorf("application: publish plan version %s: %w", id, err)
			}
			if version.State == before {
				return nil // the domain ruled this a no-op; nothing to swap
			}
			applied, err := c.versions.Publish(txCtx, id, before, *version.PublishedAt)
			if err != nil {
				return fmt.Errorf("application: publish plan version %s: %w", id, err)
			}
			if applied {
				return nil
			}
			// Lost the swap: the row moved under us, and the next iteration
			// re-reads and re-applies from the state that actually exists.
		}
		return fmt.Errorf("application: publish plan version %s: %w after %d attempts", id, ErrTransitionContended, casMaxAttempts)
	})
}

// RetirePlanVersion stops new subscriptions to a published version and
// rewrites nothing else: the subscriptions already pinned to it keep every
// term they were bought under. A draft refuses retirement in the domain —
// a version that never went on sale is abandoned, not retired.
func (c *Commerce) RetirePlanVersion(ctx context.Context, id commerce.PlanVersionID) error {
	return c.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := dbNow(txCtx, c.clock, "retire plan version")
		if err != nil {
			return err
		}
		for attempt := 0; attempt < casMaxAttempts; attempt++ {
			version, _, err := c.versions.ByID(txCtx, id)
			if err != nil {
				return fmt.Errorf("application: retire plan version %s: %w", id, err)
			}
			before := version.State
			if err := version.Retire(now); err != nil {
				return fmt.Errorf("application: retire plan version %s: %w", id, err)
			}
			if version.State == before {
				return nil
			}
			applied, err := c.versions.Retire(txCtx, id, before, *version.RetiredAt)
			if err != nil {
				return fmt.Errorf("application: retire plan version %s: %w", id, err)
			}
			if applied {
				return nil
			}
		}
		return fmt.Errorf("application: retire plan version %s: %w after %d attempts", id, ErrTransitionContended, casMaxAttempts)
	})
}

// Subscribe attaches an account to a plan version's published terms: the
// subscription is born pending with its cycle fields null, and its first
// cycle arrives with the promotion when start_at does.
//
// Two checks are this use case's because no constraint can carry them: the
// account must be active — identity's freeze stops growth, and a new
// subscription is growth — and the pinned version must be published. The
// checks read without locks, the same documented residual identity's mints
// carry: the window is one transaction wide, the version cannot un-publish,
// and the promotion's guarded statement re-checks the account's state against
// the database clock, so a subscription slipped into a freezing account stays
// inert until the account is reinstated.
func (c *Commerce) Subscribe(ctx context.Context, accountID commerce.AccountID, versionID commerce.PlanVersionID, startAt time.Time, renewalEnabled bool) (commerce.Subscription, error) {
	var subscription *commerce.Subscription
	err := c.store.WithinTx(ctx, func(txCtx context.Context) error {
		id, err := commerce.NewSubscriptionID()
		if err != nil {
			return fmt.Errorf("application: subscribe: %w", err)
		}
		now, err := dbNow(txCtx, c.clock, "subscribe")
		if err != nil {
			return err
		}
		created, err := commerce.NewSubscription(id, accountID, versionID, startAt, renewalEnabled, now)
		if err != nil {
			return fmt.Errorf("application: subscribe: %w", err)
		}
		subscription = created
		account, err := c.accounts.ByID(txCtx, identity.AccountID(accountID))
		if err != nil {
			return fmt.Errorf("application: subscribe: read account: %w", err)
		}
		if account.State != identity.AccountActive {
			return fmt.Errorf("application: subscribe: %w (%s is %s)", identity.ErrAccountNotActive, accountID, account.State)
		}
		version, _, err := c.versions.ByID(txCtx, versionID)
		if err != nil {
			return fmt.Errorf("application: subscribe: read plan version: %w", err)
		}
		if !version.AcceptsSubscriptions() {
			return fmt.Errorf("application: subscribe: %w (version %s is %s)", commerce.ErrPlanVersionNotPublished, versionID, version.State)
		}
		return c.subscriptions.Create(txCtx, *subscription)
	})
	if err != nil {
		return commerce.Subscription{}, fmt.Errorf("application: subscribe to version %s: %w", versionID, err)
	}
	return *subscription, nil
}

// Subscription returns the subscription with id.
func (c *Commerce) Subscription(ctx context.Context, id commerce.SubscriptionID) (commerce.Subscription, error) {
	subscription, err := c.subscriptions.ByID(ctx, id)
	if err != nil {
		return commerce.Subscription{}, fmt.Errorf("application: read subscription %s: %w", id, err)
	}
	return subscription, nil
}

// ScheduleSubscriptionCancellation records the customer's instruction that the
// subscription ends at cancel_at: the subscription stays active and usable
// until then, and the roll refuses to open a cycle the instruction reaches
// into. The identity-shaped CAS loop is what makes the instruction land on
// the state the customer actually saw when they asked.
func (c *Commerce) ScheduleSubscriptionCancellation(ctx context.Context, id commerce.SubscriptionID, cancelAt time.Time) error {
	return c.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := dbNow(txCtx, c.clock, "schedule cancellation of subscription")
		if err != nil {
			return err
		}
		for attempt := 0; attempt < casMaxAttempts; attempt++ {
			subscription, err := c.subscriptions.ByID(txCtx, id)
			if err != nil {
				return fmt.Errorf("application: schedule cancellation of subscription %s: %w", id, err)
			}
			if subscription.CancellationMode == commerce.CancellationScheduled &&
				subscription.CancelAt != nil && subscription.CancelAt.Equal(cancelAt) {
				return nil // the instruction is already on file; nothing to swap
			}
			if err := subscription.ScheduleCancellation(cancelAt, now); err != nil {
				return fmt.Errorf("application: schedule cancellation of subscription %s: %w", id, err)
			}
			// The dedicated statement, not the state CAS: cancel_at and its
			// mode land beside the guard in one write, only while the row
			// still reads active — the state a scheduled instruction lives in.
			applied, err := c.subscriptions.ScheduleCancellation(txCtx, id, *subscription.CancelAt, subscription.UpdatedAt)
			if err != nil {
				return fmt.Errorf("application: schedule cancellation of subscription %s: %w", id, err)
			}
			if applied {
				return nil
			}
			// Lost the swap: the subscription moved on, and the next
			// iteration lets the domain rule on the state that exists now.
		}
		return fmt.Errorf("application: schedule cancellation of subscription %s: %w after %d attempts", id, ErrTransitionContended, casMaxAttempts)
	})
}

// CancelSubscriptionNow cancels at once: terminal, the current cycle's unused
// quota forfeited. Cancelling an already-cancelled subscription is a no-op,
// so a retried cancellation — or two racing ones — converges instead of
// fighting; the lost swap is confirmed by a read, exactly as a revocation's
// is.
func (c *Commerce) CancelSubscriptionNow(ctx context.Context, id commerce.SubscriptionID) error {
	return c.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := dbNow(txCtx, c.clock, "cancel subscription")
		if err != nil {
			return err
		}
		for attempt := 0; attempt < casMaxAttempts; attempt++ {
			subscription, err := c.subscriptions.ByID(txCtx, id)
			if err != nil {
				return fmt.Errorf("application: cancel subscription %s: %w", id, err)
			}
			if subscription.State == commerce.SubscriptionCancelled {
				return nil
			}
			if err := subscription.Cancel(now); err != nil {
				return fmt.Errorf("application: cancel subscription %s: %w", id, err)
			}
			// The immediate path has its own statement, because the record it
			// writes is more than a state: cancel_at and the immediate mode
			// land beside it, in the same guarded write.
			applied, err := c.subscriptions.Cancel(txCtx, id, now)
			if err != nil {
				return fmt.Errorf("application: cancel subscription %s: %w", id, err)
			}
			if applied {
				return nil
			}
			// Lost the swap: a racing cancellation completed first (the row
			// reads cancelled, and the loop converges), or a suspend won the
			// moment — the next attempt refuses at the domain check above,
			// and that refusal is the truth about the row.
			subscription, err = c.subscriptions.ByID(txCtx, id)
			if err != nil {
				return fmt.Errorf("application: cancel subscription %s: %w", id, err)
			}
			if subscription.State == commerce.SubscriptionCancelled {
				return nil
			}
		}
		return fmt.Errorf("application: cancel subscription %s: %w after %d attempts", id, ErrTransitionContended, casMaxAttempts)
	})
}

// SuspendSubscription holds an active subscription: its entitlements stay,
// admission treats their capacity as zero, and reinstatement returns them to
// use with whatever remained.
func (c *Commerce) SuspendSubscription(ctx context.Context, id commerce.SubscriptionID) error {
	return c.transitionSubscription(ctx, id, "suspend subscription", (*commerce.Subscription).Suspend)
}

// ReinstateSubscription returns a suspended subscription to active.
func (c *Commerce) ReinstateSubscription(ctx context.Context, id commerce.SubscriptionID) error {
	return c.transitionSubscription(ctx, id, "reinstate subscription", (*commerce.Subscription).Reinstate)
}

// transitionSubscription is transitionAccount's subscription-shaped twin: one
// lifecycle move as read → domain transition → compare-and-swap, retried from
// the fresh state while the swap keeps losing. The domain refuses what the
// state machine forbids; this loop only settles who gets to apply the move.
func (c *Commerce) transitionSubscription(ctx context.Context, id commerce.SubscriptionID, action string, apply func(*commerce.Subscription, time.Time) error) error {
	return c.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := dbNow(txCtx, c.clock, action)
		if err != nil {
			return err
		}
		for attempt := 0; attempt < casMaxAttempts; attempt++ {
			subscription, err := c.subscriptions.ByID(txCtx, id)
			if err != nil {
				return fmt.Errorf("application: %s %s: %w", action, id, err)
			}
			before := subscription.State
			if err := apply(&subscription, now); err != nil {
				return fmt.Errorf("application: %s %s: %w", action, id, err)
			}
			if subscription.State == before {
				return nil // the domain ruled this a no-op; nothing to swap
			}
			applied, err := c.subscriptions.TransitionState(txCtx, id, before, subscription.State, subscription.UpdatedAt)
			if err != nil {
				return fmt.Errorf("application: %s %s: %w", action, id, err)
			}
			if applied {
				return nil
			}
		}
		return fmt.Errorf("application: %s %s: %w after %d attempts", action, id, ErrTransitionContended, casMaxAttempts)
	})
}

// EnableAccountPayg turns the account's prepaid-spending flag on. Enabling
// authorises spending and funds nothing: the flag is commerce's whole state
// here, the bucket it will draw from is Accounting's row, and the first
// topup is B6's choreography. The account must be active — growth stops with
// a suspension, and authorising new spending is growth — while the off-switch
// below deliberately has no such gate: disabling a spending path is never the
// move a suspension should be able to block.
func (c *Commerce) EnableAccountPayg(ctx context.Context, accountID commerce.AccountID) error {
	return c.store.WithinTx(ctx, func(txCtx context.Context) error {
		account, err := c.accounts.ByID(txCtx, identity.AccountID(accountID))
		if err != nil {
			return fmt.Errorf("application: enable payg: read account: %w", err)
		}
		if account.State != identity.AccountActive {
			return fmt.Errorf("application: enable payg: %w (%s is %s)", identity.ErrAccountNotActive, accountID, account.State)
		}
		// The active gate is a plain read, not part of the write: the flag's
		// statement guards on nothing but the account key. An account
		// deactivated between this read and the write keeps the flag it was
		// given — a residual the admission side already prices in, because a
		// disabled account's subscriptions stop being served regardless of
		// what its spending flag says.
		now, err := dbNow(txCtx, c.clock, "enable payg")
		if err != nil {
			return err
		}
		if err := c.payg.SetEnabled(txCtx, accountID, true, now); err != nil {
			return fmt.Errorf("application: enable payg for account %s: %w", accountID, err)
		}
		return nil
	})
}

// DisableAccountPayg turns the spending flag off. Disabling blocks new
// spills at admission and touches nothing else: open holds settle normally,
// and the funds are Accounting's to keep. The write is the port's
// insert-or-update single statement, so the first disable of an account that
// never enabled PAYG records the flag explicitly instead of failing.
func (c *Commerce) DisableAccountPayg(ctx context.Context, accountID commerce.AccountID) error {
	now, err := dbNow(ctx, c.clock, "disable payg")
	if err != nil {
		return err
	}
	if err := c.payg.SetEnabled(ctx, accountID, false, now); err != nil {
		return fmt.Errorf("application: disable payg for account %s: %w", accountID, err)
	}
	return nil
}

// AccountPayg returns the account's PAYG commercial state. A miss is the
// persistence port's ErrNotFound — the absence of a row is the disabled
// state, and there is no state cheaper than that.
func (c *Commerce) AccountPayg(ctx context.Context, accountID commerce.AccountID) (commerce.AccountPayg, error) {
	payg, err := c.payg.ByAccount(ctx, accountID)
	if err != nil {
		return commerce.AccountPayg{}, fmt.Errorf("application: read payg state of account %s: %w", accountID, err)
	}
	return payg, nil
}

// AssignAccountFundingBucket records the bucket reference the account-creation
// choreography assigns. The assignment is write-once — one PAYG source and
// one bucket per account, ever, because a changed reference would silently
// split the account's prepaid money across two buckets — so the lost race is
// resolved by reading what did land: the same bucket is convergence, a
// different one is the defect the guard exists to stop.
func (c *Commerce) AssignAccountFundingBucket(ctx context.Context, accountID commerce.AccountID, bucketID commerce.FundingBucketID) error {
	return c.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := dbNow(txCtx, c.clock, "assign funding bucket")
		if err != nil {
			return err
		}
		payg, err := c.payg.ByAccount(txCtx, accountID)
		if err != nil {
			return fmt.Errorf("application: assign funding bucket: read payg state: %w", err)
		}
		if err := payg.AssignFundingBucket(bucketID, now); err != nil {
			return fmt.Errorf("application: assign funding bucket to account %s: %w", accountID, err)
		}
		applied, err := c.payg.AssignFundingBucket(txCtx, accountID, bucketID, payg.UpdatedAt)
		if err != nil {
			return fmt.Errorf("application: assign funding bucket to account %s: %w", accountID, err)
		}
		if applied {
			return nil
		}
		onFile, err := c.payg.ByAccount(txCtx, accountID)
		if err != nil {
			return fmt.Errorf("application: assign funding bucket to account %s: %w", accountID, err)
		}
		if onFile.FundingBucketID == bucketID {
			return nil // the same bucket already landed; converged
		}
		return fmt.Errorf("application: assign funding bucket to account %s: %w: bucket %s is already on file",
			accountID, commerce.ErrInvalidTransition, onFile.FundingBucketID)
	})
}

// EffectiveEntitlements derives the account's effective entitlements at at:
// every active entitlement of an active subscription whose cycle covers the
// instant, in the single allocation waterfall ADR 0003 fixes. It is the
// canonical answer to "what may this account consume, from which grant, until
// when" — the projection seed's upstream, and deliberately nothing about what
// has been consumed.
//
// The instant is the caller's to source: this method takes no clock, and a
// caller making a commercial decision on the answer owes it the same
// discipline as every writer — an instant read from transaction_timestamp()
// inside the caller's transaction, not a process wall clock. The derivation
// is pure, so the same rows and the same instant always answer identically;
// a caller that hands in a different clock has asked a different question.
func (c *Commerce) EffectiveEntitlements(ctx context.Context, accountID commerce.AccountID, at time.Time) ([]commerce.EffectiveEntitlement, error) {
	candidates, err := c.entitlements.ActiveCandidates(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("application: derive effective entitlements of account %s: %w", accountID, err)
	}
	return commerce.EffectiveEntitlements(candidates, at), nil
}

// DueWorkLimits bounds each lane's batch in one worker pass. Zero disables a
// lane; the composition root's configuration owns the numbers, and no lane
// infers a limit from another's.
type DueWorkLimits struct {
	// Promotions bounds the promotion lane's pending subscriptions per pass.
	Promotions int
	// Cancellations bounds the cancellation lane's due instructions per pass.
	Cancellations int
	// Rolls bounds the roll lane's due cycles per pass.
	Rolls int
	// SubscriptionExpiries bounds the fixed-term expiry lane per pass.
	SubscriptionExpiries int
	// EntitlementExpiries bounds the entitlement expiry lane per pass.
	EntitlementExpiries int
}

// DueWorkResult is one worker pass's ledger: how far each lane got. A skipped
// row — a verdict that did not fire because the world moved — is not counted
// anywhere: it stays due, and the next pass owns it.
type DueWorkResult struct {
	Promoted            int
	Cancelled           int
	Rolled              int
	Expired             int
	EntitlementsExpired int
}

// RunDueWork executes the five lanes in the order the model's dependencies
// demand: promotions first (a due pending subscription is dead capacity until
// it activates), cancellations before rolls (a due instruction must end the
// subscription before the roll asks whether to extend it — a row that lost
// that ordering would roll a cycle its customer had already terminated),
// then the expiry lanes, which only ever observe what the first three left
// behind. The first lane error stops the pass: every completed row is its own
// committed fact, the remainder stay due, and the next pass resumes from the
// scans.
func (c *Commerce) RunDueWork(ctx context.Context, limits DueWorkLimits) (DueWorkResult, error) {
	var result DueWorkResult

	promoted, err := c.PromoteDueSubscriptions(ctx, limits.Promotions)
	result.Promoted = promoted
	if err != nil {
		return result, err
	}

	cancelled, err := c.CompleteDueCancellations(ctx, limits.Cancellations)
	result.Cancelled = cancelled
	if err != nil {
		return result, err
	}

	rolled, err := c.RollDueSubscriptions(ctx, limits.Rolls)
	result.Rolled = rolled
	if err != nil {
		return result, err
	}

	expired, err := c.ExpireDueSubscriptions(ctx, limits.SubscriptionExpiries)
	result.Expired = expired
	if err != nil {
		return result, err
	}

	entitlementsExpired, err := c.ExpireDueEntitlements(ctx, limits.EntitlementExpiries)
	result.EntitlementsExpired = entitlementsExpired
	return result, err
}

// PromoteDueSubscriptions runs the promotion lane: every pending subscription
// whose start_at the database clock has passed becomes active and receives
// cycle 1's entitlements, in one unit of work per subscription — the
// promotion and its first roll are one transition, and a promotion whose
// entitlements failed to insert is a subscription that never happened.
func (c *Commerce) PromoteDueSubscriptions(ctx context.Context, limit int) (int, error) {
	ids, err := c.subscriptions.DuePromotionIDs(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("application: scan due promotions: %w", err)
	}
	promoted := 0
	for _, id := range ids {
		ok, err := c.promoteSubscription(ctx, id)
		if err != nil {
			return promoted, err
		}
		if ok {
			promoted++
		}
	}
	return promoted, nil
}

// promoteSubscription advances one subscription through its first cycle. The
// scope resolution runs before the unit of work opens — the roll must never
// hold row locks while waiting on the cross-plane seam — and the row is
// re-read under lock inside, because a scan's answer is a glance and the
// verdict is the statement: the world between the two is exactly what the
// locked re-read and the guarded advance exist to absorb.
func (c *Commerce) promoteSubscription(ctx context.Context, id commerce.SubscriptionID) (bool, error) {
	pending, err := c.subscriptions.ByID(ctx, id)
	if err != nil {
		return false, fmt.Errorf("application: promote subscription %s: %w", id, err)
	}
	if pending.State != commerce.SubscriptionPending {
		return false, nil // the scan raced a writer; the row is not due anymore
	}
	definitions, scopes, skip, err := c.resolveCycleInputs(ctx, pending.PlanVersionID)
	if err != nil || skip {
		return false, err
	}

	err = c.store.WithinTx(ctx, func(txCtx context.Context) error {
		locked, err := c.subscriptions.ByIDForUpdate(txCtx, id)
		if err != nil {
			return fmt.Errorf("application: promote subscription %s: %w", id, err)
		}
		if locked.State != commerce.SubscriptionPending {
			return errDueWorldMoved
		}
		now, err := c.clock.Now(txCtx)
		if err != nil {
			return fmt.Errorf("application: promote subscription %s: %w", id, err)
		}
		periodStart, periodEnd, err := locked.CycleBoundsFor(1)
		if err != nil {
			return fmt.Errorf("application: promote subscription %s: %w", id, err)
		}
		if err := locked.ActivateWithCycle(1, periodStart, periodEnd, now); err != nil {
			return fmt.Errorf("application: promote subscription %s: %w", id, err)
		}
		if err := c.materialiseEntitlements(txCtx, locked, 1, definitions, scopes, periodStart, periodEnd, now); err != nil {
			return err
		}
		applied, err := c.subscriptions.Activate(txCtx, id, periodStart, periodEnd, locked.UpdatedAt)
		if err != nil {
			return fmt.Errorf("application: promote subscription %s: %w", id, err)
		}
		if !applied {
			return errDueWorldMoved
		}
		return nil
	})
	if dueSkip(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RollDueSubscriptions runs the roll lane: every active, renewing
// subscription whose period the database clock has ended advances into its
// next cycle with that cycle's entitlements, one unit of work per
// subscription. A subscription whose scheduled cancellation has also due'd
// may still appear here — the pass orders the cancellation lane first, and
// the roll's own gates refuse whatever survives that ordering.
func (c *Commerce) RollDueSubscriptions(ctx context.Context, limit int) (int, error) {
	ids, err := c.subscriptions.DueRollIDs(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("application: scan due rolls: %w", err)
	}
	rolled := 0
	for _, id := range ids {
		ok, err := c.rollSubscription(ctx, id)
		if err != nil {
			return rolled, err
		}
		if ok {
			rolled++
		}
	}
	return rolled, nil
}

// rollSubscription advances one subscription into its next cycle, the
// promotion's senior twin and the reason the seam's group-version read
// exists. The entitlements of the new cycle are inserted in the same unit of
// work as the guarded cycle advance, so a roll is one fact: either the cycle
// turned and its grants exist, or neither happened. The lock the unit opens
// with is what keeps two rollers from both inserting cycle N+1 — the second
// blocks on the row, re-reads the advanced cycle, and skips benignly instead
// of colliding on the schema's once-per-cycle key.
func (c *Commerce) rollSubscription(ctx context.Context, id commerce.SubscriptionID) (bool, error) {
	due, err := c.subscriptions.ByID(ctx, id)
	if err != nil {
		return false, fmt.Errorf("application: roll subscription %s: %w", id, err)
	}
	if due.State != commerce.SubscriptionActive || due.CycleNumber == nil || due.PeriodEnd == nil {
		return false, nil // the scan raced a writer; the row is not due anymore
	}
	fromCycle := *due.CycleNumber
	definitions, scopes, skip, err := c.resolveCycleInputs(ctx, due.PlanVersionID)
	if err != nil || skip {
		return false, err
	}

	err = c.store.WithinTx(ctx, func(txCtx context.Context) error {
		locked, err := c.subscriptions.ByIDForUpdate(txCtx, id)
		if err != nil {
			return fmt.Errorf("application: roll subscription %s: %w", id, err)
		}
		if locked.State != commerce.SubscriptionActive || locked.CycleNumber == nil ||
			*locked.CycleNumber != fromCycle || locked.PeriodEnd == nil || !locked.PeriodEnd.Equal(*due.PeriodEnd) {
			return errDueWorldMoved
		}
		now, err := c.clock.Now(txCtx)
		if err != nil {
			return fmt.Errorf("application: roll subscription %s: %w", id, err)
		}
		cycle := fromCycle + 1
		periodStart, periodEnd, err := locked.CycleBoundsFor(cycle)
		if err != nil {
			return fmt.Errorf("application: roll subscription %s: %w", id, err)
		}
		if err := locked.RollCycle(cycle, periodStart, periodEnd, now); err != nil {
			return fmt.Errorf("application: roll subscription %s: %w", id, err)
		}
		if err := c.materialiseEntitlements(txCtx, locked, cycle, definitions, scopes, periodStart, periodEnd, now); err != nil {
			return err
		}
		applied, err := c.subscriptions.Roll(txCtx, id, fromCycle, periodStart, periodEnd, locked.UpdatedAt)
		if err != nil {
			return fmt.Errorf("application: roll subscription %s: %w", id, err)
		}
		if !applied {
			return errDueWorldMoved
		}
		return nil
	})
	if dueSkip(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// resolveCycleInputs fetches the pinned version's grant definitions and
// resolves each one's group NAME to the version that is current right now —
// the one read commerce makes across the seam (ADR 0006 §5), done here
// deliberately outside any unit of work.
//
// A group the catalog has never opened is a lane-stopping error: the grant
// definition is a commercial catalog defect, and a roll that "skipped" it
// every pass would silently sell a plan that grants nothing. A transport
// failure is also returned rather than swallowed — the pass ends, everything
// already rolled stays rolled, the unrolled rows stay due, and the next pass
// asks the seam again; that is the port's "not now", and it is the caller's
// retry policy, not this file's, that decides what a run of failures means.
//
// The boolean is the one skip that is not an error: a version read as draft
// has no business feeding a roll yet, and waiting a pass costs nothing. A
// retired version keeps feeding the rolls of the subscriptions that pinned
// it — retirement stops new sales, it rewrites nothing, and freezing its
// existing subscribers' cycles would be exactly the retroactive rewrite the
// model forbids. The version cannot otherwise change under the lane —
// publication freezes the set forever — so the definitions resolved here are
// the definitions the unit of work will insert against.
func (c *Commerce) resolveCycleInputs(ctx context.Context, versionID commerce.PlanVersionID) ([]commerce.GrantDefinition, map[commerce.GrantDefinitionID]commerce.AliasGroupVersionID, bool, error) {
	version, definitions, err := c.versions.ByID(ctx, versionID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("application: resolve cycle inputs of version %s: %w", versionID, err)
	}
	if version.State == commerce.PlanVersionDraft {
		return nil, nil, true, nil
	}
	scopes := make(map[commerce.GrantDefinitionID]commerce.AliasGroupVersionID, len(definitions))
	for _, definition := range definitions {
		groupVersion, err := c.catalog.CurrentGroupVersion(ctx, string(definition.AliasGroupName))
		if err != nil {
			return nil, nil, false, fmt.Errorf("application: resolve grant scope %s (%s) of version %s: %w",
				definition.ID, definition.AliasGroupName, versionID, err)
		}
		scopes[definition.ID] = commerce.AliasGroupVersionID(groupVersion.GroupVersionID)
	}
	return definitions, scopes, false, nil
}

// materialiseEntitlements inserts one entitlement per grant definition for
// the cycle being opened — the roll's write half, always called inside the
// same unit of work as the guarded cycle advance, so the schema's
// once-per-cycle key is belt to the statement's braces. A missing scope for a
// definition is a programmer error: resolveCycleInputs resolved every
// definition this list carries.
func (c *Commerce) materialiseEntitlements(
	ctx context.Context,
	subscription commerce.Subscription,
	cycle int,
	definitions []commerce.GrantDefinition,
	scopes map[commerce.GrantDefinitionID]commerce.AliasGroupVersionID,
	periodStart, periodEnd, now time.Time,
) error {
	for _, definition := range definitions {
		scope, ok := scopes[definition.ID]
		if !ok {
			return fmt.Errorf("application: materialise cycle %d of subscription %s: grant definition %s has no resolved scope",
				cycle, subscription.ID, definition.ID)
		}
		id, err := commerce.NewEntitlementID()
		if err != nil {
			return fmt.Errorf("application: materialise cycle %d of subscription %s: %w", cycle, subscription.ID, err)
		}
		entitlement, err := commerce.NewEntitlement(id, subscription.ID, cycle, definition, scope, periodStart, periodEnd, now)
		if err != nil {
			return fmt.Errorf("application: materialise cycle %d of subscription %s: %w", cycle, subscription.ID, err)
		}
		if err := c.entitlements.Create(ctx, *entitlement); err != nil {
			return fmt.Errorf("application: materialise cycle %d of subscription %s: insert entitlement %s: %w",
				cycle, subscription.ID, entitlement.ID, err)
		}
	}
	return nil
}

// CompleteDueCancellations runs the cancellation lane: every active
// subscription whose scheduled cancel_at the database clock has passed flips
// to cancelled, keeping the instruction's original instant as the historical
// record of when the customer asked. Each move is a single guarded
// statement — the scan's predicate is the clock, the statement is the
// verdict — so no unit of work is opened for a move that is one statement.
func (c *Commerce) CompleteDueCancellations(ctx context.Context, limit int) (int, error) {
	ids, err := c.subscriptions.DueCancellationIDs(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("application: scan due cancellations: %w", err)
	}
	cancelled := 0
	for _, id := range ids {
		ok, err := c.completeCancellation(ctx, id)
		if err != nil {
			return cancelled, err
		}
		if ok {
			cancelled++
		}
	}
	return cancelled, nil
}

// completeCancellation completes one scheduled cancellation: the domain
// validates the instruction against the row as read, and the completion
// statement is the verdict — it repeats the instruction the scan read
// (scheduled, and due on the database clock), so an instruction rescheduled
// or withdrawn between the read and the write leaves the row uncompleted
// rather than cancelled under words the customer had already superseded.
func (c *Commerce) completeCancellation(ctx context.Context, id commerce.SubscriptionID) (bool, error) {
	subscription, err := c.subscriptions.ByID(ctx, id)
	if err != nil {
		return false, fmt.Errorf("application: complete cancellation of subscription %s: %w", id, err)
	}
	now, err := dbNow(ctx, c.clock, "complete cancellation of subscription")
	if err != nil {
		return false, fmt.Errorf("application: complete cancellation of subscription %s: %w", id, err)
	}
	if err := subscription.CompleteDueCancellation(now); err != nil {
		if dueSkip(err) {
			return false, nil
		}
		return false, fmt.Errorf("application: complete cancellation of subscription %s: %w", id, err)
	}
	applied, err := c.subscriptions.CompleteScheduledCancellation(ctx, id, subscription.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("application: complete cancellation of subscription %s: %w", id, err)
	}
	return applied, nil
}

// ExpireDueSubscriptions runs the fixed-term expiry lane: active and
// suspended subscriptions whose term ended without renewal and without a
// cancellation instruction become expired, rows retained forever. A
// renewing subscription never appears — the roll lane owns its period end —
// and a suspended one does: suspension does not stop the clock.
func (c *Commerce) ExpireDueSubscriptions(ctx context.Context, limit int) (int, error) {
	ids, err := c.subscriptions.DueExpiryIDs(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("application: scan due subscription expiries: %w", err)
	}
	expired := 0
	for _, id := range ids {
		ok, err := c.expireSubscription(ctx, id)
		if err != nil {
			return expired, err
		}
		if ok {
			expired++
		}
	}
	return expired, nil
}

// expireSubscription expires one fixed-term subscription: the domain
// validates against the row as read, and the guarded statement — which
// repeats the whole predicate against the database clock — is the verdict.
// A false verdict is a benign skip in both of its possible causes: the row
// moved (cancelled, expired) or the two clocks disagreed about whether the
// term has ended. Neither is worth a retry inside this pass; the next scan
// settles it.
func (c *Commerce) expireSubscription(ctx context.Context, id commerce.SubscriptionID) (bool, error) {
	subscription, err := c.subscriptions.ByID(ctx, id)
	if err != nil {
		return false, fmt.Errorf("application: expire subscription %s: %w", id, err)
	}
	now, err := dbNow(ctx, c.clock, "expire subscription")
	if err != nil {
		return false, fmt.Errorf("application: expire subscription %s: %w", id, err)
	}
	if err := subscription.Expire(now); err != nil {
		if dueSkip(err) {
			return false, nil
		}
		return false, fmt.Errorf("application: expire subscription %s: %w", id, err)
	}
	applied, err := c.subscriptions.Expire(ctx, id, subscription.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("application: expire subscription %s: %w", id, err)
	}
	return applied, nil
}

// ExpireDueEntitlements runs the entitlement expiry lane: active grants whose
// cycle the database clock has ended flip to expired and stay as history.
// Capacity stops being available to new admissions at the period end; this
// lane is the record-keeping flip, not the enforcement instant.
func (c *Commerce) ExpireDueEntitlements(ctx context.Context, limit int) (int, error) {
	ids, err := c.entitlements.DueExpiryIDs(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("application: scan due entitlement expiries: %w", err)
	}
	expired := 0
	for _, id := range ids {
		now, err := dbNow(ctx, c.clock, "expire entitlement")
		if err != nil {
			return expired, err
		}
		applied, err := c.entitlements.Expire(ctx, id, now)
		if err != nil {
			return expired, fmt.Errorf("application: expire entitlement %s: %w", id, err)
		}
		if applied {
			expired++
		}
	}
	return expired, nil
}
