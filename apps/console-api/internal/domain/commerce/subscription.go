package commerce

import (
	"fmt"
	"time"
)

// SubscriptionState is a subscription's position on the lifecycle ADR 0003
// fixes. Pending grants nothing; active rolls cycles and serves; suspended
// is reversible and freezes nothing here (admission treats its capacity as
// zero — that is the runtime's reading of this state, not a stored change to
// the entitlements); cancelled and expired are terminal and keep their rows
// forever, because immutable accounting references them.
type SubscriptionState string

const (
	// SubscriptionPending: created, waiting for start_at. Cycle fields are
	// null exactly here, and nowhere else.
	SubscriptionPending SubscriptionState = "pending"
	// SubscriptionActive: the subscription's cycles roll and its
	// entitlements are live.
	SubscriptionActive SubscriptionState = "active"
	// SubscriptionSuspended: held — reversible only back to active.
	SubscriptionSuspended SubscriptionState = "suspended"
	// SubscriptionCancelled: terminal, reached at a cancellation
	// instruction's effective time.
	SubscriptionCancelled SubscriptionState = "cancelled"
	// SubscriptionExpired: terminal, reached at the natural end of a
	// fixed-term subscription without a cancellation instruction.
	SubscriptionExpired SubscriptionState = "expired"
)

// CancellationMode spells which kind of cancellation instruction a
// subscription carries. Scheduled cancellation is data, not a state: the
// subscription stays active and usable until cancel_at passes.
type CancellationMode string

const (
	// CancellationScheduled: cancel_at was set ahead of time.
	CancellationScheduled CancellationMode = "scheduled"
	// CancellationImmediate: the cancellation took effect at once.
	CancellationImmediate CancellationMode = "immediate"
)

// Subscription is one account's instantiation of one specific plan version,
// which it pins forever: a plan change is a new version and a new
// subscription, never an edit in place. Any number of subscriptions may be
// active concurrently — including several to the same version; they never
// merge, each has independent cycles and entitlements, and no account
// carries a "current plan" anywhere in this model.
type Subscription struct {
	ID SubscriptionID
	// The owner. Identity's aggregate, carried as commerce's own distinct
	// AccountID (see ids.go) — the application layer converts from
	// identity's spelling.
	AccountID        AccountID
	PlanVersionID    PlanVersionID
	State            SubscriptionState
	StartAt          time.Time
	RenewalEnabled   bool
	CancelAt         *time.Time
	CancellationMode CancellationMode
	CycleNumber      *int
	PeriodStart      *time.Time
	PeriodEnd        *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// NewSubscription returns a subscription in its only legal birth state,
// pending: cycle fields null, no cancellation instruction, the version pin
// taken as given. Whether the version accepts new subscriptions is the
// subscribing use case's check (ErrPlanVersionNotPublished) — the domain has
// no version row to read, and a constraint cannot read one either.
func NewSubscription(id SubscriptionID, accountID AccountID, planVersionID PlanVersionID, startAt time.Time, renewalEnabled bool, now time.Time) (*Subscription, error) {
	if id == "" || accountID == "" || planVersionID == "" {
		return nil, fmt.Errorf("commerce: new subscription: blank id")
	}
	if startAt.IsZero() {
		return nil, fmt.Errorf("commerce: new subscription: start_at is unset")
	}
	now = now.UTC()
	return &Subscription{
		ID:             id,
		AccountID:      accountID,
		PlanVersionID:  planVersionID,
		State:          SubscriptionPending,
		StartAt:        startAt.UTC(),
		RenewalEnabled: renewalEnabled,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// IsDueForStart reports whether the pending promotion's scan predicate
// holds at now: the subscription is pending and its start has arrived. The
// first roll is not gated on renewal_enabled — a fixed-term subscription is
// born renewal_enabled = false and still receives cycle 1 (ADR 0003 as
// amended by the due-work table in docs/architecture/commerce.md).
func (s *Subscription) IsDueForStart(now time.Time) bool {
	return s.State == SubscriptionPending && !now.UTC().Before(s.StartAt)
}

// ActivateWithCycle promotes a due pending subscription to active and
// hands it its first cycle in the same move — the promotion and the roll
// are one transition, because a subscription that is active with no cycle
// is a state the schema refuses to store. The bounds come from the roll
// transaction's database-clock instant (CycleBounds of StartAt); the domain
// refuses bounds that are not a non-empty half-open interval and a cycle
// number that is not 1, and a promotion whose start has not arrived is
// ErrNotDue — the scan predicate and the transition are the same rule.
func (s *Subscription) ActivateWithCycle(cycle int, periodStart, periodEnd, now time.Time) error {
	if s.State != SubscriptionPending {
		return fmt.Errorf("commerce: activate subscription %s: %w: state is %s, not pending", s.ID, ErrInvalidTransition, s.State)
	}
	if !s.IsDueForStart(now) {
		return fmt.Errorf("commerce: activate subscription %s: %w: start_at %s has not arrived", s.ID, ErrNotDue, s.StartAt.UTC())
	}
	if cycle != 1 {
		return fmt.Errorf("commerce: activate subscription %s: %w: first cycle is 1, not %d", s.ID, ErrInvalidPeriod, cycle)
	}
	return s.applyCycle(cycle, periodStart, periodEnd, now)
}

// RollCycle advances an active subscription into its next cycle. The gates
// are ADR 0003's, in order: the subscription is active with its current
// period ended (the scan's own predicate — a roll before the end is
// ErrNotDue); renewal is enabled — a fixed-term subscription's cycle 1 is
// its only one; the next cycle's end does not reach a scheduled
// cancellation (cancel_at null, or strictly after the new cycle would end —
// a cancellation due exactly when the cycle ends grants nothing); and the
// numbering is exactly one past the current cycle, because the roll
// transaction is the only writer of cycle fields and it never skips.
func (s *Subscription) RollCycle(cycle int, periodStart, periodEnd, now time.Time) error {
	if s.State != SubscriptionActive {
		return fmt.Errorf("commerce: roll subscription %s: %w: state is %s, not active", s.ID, ErrInvalidTransition, s.State)
	}
	if s.PeriodEnd == nil || now.UTC().Before(*s.PeriodEnd) {
		return fmt.Errorf("commerce: roll subscription %s: %w: the current period has not ended", s.ID, ErrNotDue)
	}
	if !s.RenewalEnabled {
		return fmt.Errorf("commerce: roll subscription %s: %w: renewal is not enabled", s.ID, ErrInvalidTransition)
	}
	if s.CancelAt != nil && !s.CancelAt.After(periodEnd) {
		return fmt.Errorf("commerce: roll subscription %s: %w: cancel_at %s does not survive past period_end %s",
			s.ID, ErrInvalidTransition, s.CancelAt.UTC(), periodEnd.UTC())
	}
	if s.CycleNumber == nil {
		return fmt.Errorf("commerce: roll subscription %s: %w: no current cycle to roll from", s.ID, ErrInvalidTransition)
	}
	if cycle != *s.CycleNumber+1 {
		return fmt.Errorf("commerce: roll subscription %s: %w: cycle %d does not follow %d", s.ID, ErrInvalidPeriod, cycle, *s.CycleNumber)
	}
	return s.applyCycle(cycle, periodStart, periodEnd, now)
}

// applyCycle stamps the next cycle's fields. It is the one writer of cycle
// state, reached only through ActivateWithCycle and RollCycle, both of
// which have already checked the state machine and the roll gates.
func (s *Subscription) applyCycle(cycle int, periodStart, periodEnd, now time.Time) error {
	start := periodStart.UTC()
	end := periodEnd.UTC()
	if !end.After(start) {
		return fmt.Errorf("commerce: cycle %d of subscription %s: %w: [%s, %s) is empty or inverted",
			cycle, s.ID, ErrInvalidPeriod, start, end)
	}
	stamp := now.UTC()
	c := cycle
	s.State = SubscriptionActive
	s.CycleNumber = &c
	s.PeriodStart = &start
	s.PeriodEnd = &end
	s.UpdatedAt = stamp
	return nil
}

// CycleBoundsFor returns the bounds of the given cycle for this
// subscription's anchor, in the arithmetic ADR 0003 fixes.
func (s *Subscription) CycleBoundsFor(cycle int) (time.Time, time.Time, error) {
	return CycleBounds(s.StartAt, cycle)
}

// CycleCovers reports whether the subscription's current cycle covers at —
// the half-open test the waterfall's match step runs.
func (s *Subscription) CycleCovers(at time.Time) bool {
	if s.PeriodStart == nil || s.PeriodEnd == nil {
		return false
	}
	return CoversPeriod(*s.PeriodStart, *s.PeriodEnd, at)
}

// ScheduleCancellation records a scheduled cancellation: the subscription
// stays active with cancel_at set and its capacity usable until then, and
// rolls are suppressed past the instruction. Only an active subscription
// takes the instruction — a pending one has nothing to cancel yet, and the
// terminal states have nothing left to cancel ever.
func (s *Subscription) ScheduleCancellation(cancelAt time.Time, now time.Time) error {
	if s.State != SubscriptionActive {
		return fmt.Errorf("commerce: schedule cancellation of subscription %s: %w: state is %s, not active", s.ID, ErrInvalidTransition, s.State)
	}
	if cancelAt.IsZero() {
		return fmt.Errorf("commerce: schedule cancellation of subscription %s: %w: cancel_at is unset", s.ID, ErrInvalidCancellation)
	}
	stamp := cancelAt.UTC()
	s.CancelAt = &stamp
	s.CancellationMode = CancellationScheduled
	s.UpdatedAt = now.UTC()
	return nil
}

// Cancel is the immediate cancellation: cancel_at = now, mode immediate,
// state cancelled — terminal at once, the cycle's unused quota forfeited.
// A scheduled cancellation whose time has come is completed by
// CompleteDueCancellation, not by calling this.
func (s *Subscription) Cancel(now time.Time) error {
	if s.State != SubscriptionActive {
		return fmt.Errorf("commerce: cancel subscription %s: %w: state is %s, not active", s.ID, ErrInvalidTransition, s.State)
	}
	stamp := now.UTC()
	s.CancelAt = &stamp
	s.CancellationMode = CancellationImmediate
	s.State = SubscriptionCancelled
	s.UpdatedAt = stamp
	return nil
}

// IsDueForCancellation reports whether a scheduled cancellation's effective
// time has passed at now.
func (s *Subscription) IsDueForCancellation(now time.Time) bool {
	return s.State == SubscriptionActive && s.CancelAt != nil &&
		s.CancellationMode == CancellationScheduled && !now.UTC().Before(*s.CancelAt)
}

// CompleteDueCancellation carries out a scheduled cancellation whose
// cancel_at the clock has passed: the same terminal state the immediate
// path reaches, with the instruction's original instant kept as the
// historical record of when the customer asked.
func (s *Subscription) CompleteDueCancellation(now time.Time) error {
	if !s.IsDueForCancellation(now) {
		return fmt.Errorf("commerce: complete cancellation of subscription %s: %w: no scheduled cancellation is due", s.ID, ErrNotDue)
	}
	s.State = SubscriptionCancelled
	s.UpdatedAt = now.UTC()
	return nil
}

// Suspend holds an active subscription. Suspension keeps its entitlements;
// admission treats their capacity as zero, and reinstatement returns them
// to use with whatever remained. Suspending a suspended subscription is a
// no-op, mirroring identity's machine; terminal states refuse.
func (s *Subscription) Suspend(now time.Time) error {
	switch s.State {
	case SubscriptionSuspended:
		return nil
	case SubscriptionActive:
		s.State = SubscriptionSuspended
		s.UpdatedAt = now.UTC()
		return nil
	default:
		return fmt.Errorf("commerce: suspend subscription %s: %w: state is %s", s.ID, ErrInvalidTransition, s.State)
	}
}

// Reinstate returns a suspended subscription to active.
func (s *Subscription) Reinstate(now time.Time) error {
	switch s.State {
	case SubscriptionActive:
		return nil
	case SubscriptionSuspended:
		s.State = SubscriptionActive
		s.UpdatedAt = now.UTC()
		return nil
	default:
		return fmt.Errorf("commerce: reinstate subscription %s: %w: state is %s", s.ID, ErrInvalidTransition, s.State)
	}
}

// IsDueForExpiry reports whether the natural end has arrived at now: the
// period the cycle fields describe has ended and no renewal follows — a
// renewing subscription is never due for expiry, because the roll lane owns
// its period end (a scan that ran expiry first would otherwise kill a live
// subscription). It holds for active subscriptions (the fixed term ended
// without a cancellation instruction) and for suspended ones (the diagram's
// expiry while suspended) alike; a cancelled subscription is already
// terminal and never expires.
func (s *Subscription) IsDueForExpiry(now time.Time) bool {
	if s.State != SubscriptionActive && s.State != SubscriptionSuspended {
		return false
	}
	// An outstanding cancellation instruction is not this end's to execute:
	// the cancellation lane owns it, and an instruction due after the term
	// must complete as a cancellation, not be pre-empted by an expiry.
	if s.PeriodEnd == nil || s.RenewalEnabled || s.CancelAt != nil {
		return false
	}
	return !now.UTC().Before(*s.PeriodEnd)
}

// Expire is the natural end of a fixed-term subscription: terminal, no
// cancellation instruction, rows retained forever.
func (s *Subscription) Expire(now time.Time) error {
	if !s.IsDueForExpiry(now) {
		return fmt.Errorf("commerce: expire subscription %s: %w: state is %s and its period has not ended", s.ID, ErrNotDue, s.State)
	}
	s.State = SubscriptionExpired
	s.UpdatedAt = now.UTC()
	return nil
}
