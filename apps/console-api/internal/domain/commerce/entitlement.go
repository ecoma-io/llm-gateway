package commerce

import (
	"fmt"
	"time"
)

// EntitlementState is an entitlement's whole machine: active or expired.
// There is no suspended entitlement — suspension is a subscription state
// applied at admission, never stored per grant (ADR 0003) — and expiry is
// terminal: rows are retained forever because immutable accounting
// references them.
type EntitlementState string

const (
	// EntitlementActive: the grant is live for its cycle.
	EntitlementActive EntitlementState = "active"
	// EntitlementExpired: the grant's cycle has ended; the row remains as
	// history.
	EntitlementExpired EntitlementState = "expired"
)

// Entitlement is one live quota grant materialised from exactly one
// (subscription, cycle, grant definition) — a triple unique by the schema's
// entitlements_grant_once_per_cycle, so a retried roll cannot grant a cycle
// twice.
//
// The row is immutable after the roll except the terminal state flip: it
// records what was granted, for which cycle, scoped to which alias-group
// version — and deliberately nothing about what was consumed. Capacity is
// drawn down in Accounting's funding buckets and the runtime's quota
// projections; a balance column here would be a third copy of the same
// number with no rebuild story, and the conflation of entitlement with
// quota is exactly what the model refuses.
type Entitlement struct {
	ID                  EntitlementID
	SubscriptionID      SubscriptionID
	CycleNumber         int
	GrantDefinitionID   GrantDefinitionID
	AliasGroupVersionID AliasGroupVersionID
	Dimension           Dimension
	GrantedAmount       int64
	State               EntitlementState
	PeriodStart         time.Time
	PeriodEnd           time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// NewEntitlement returns an entitlement in its only legal birth state,
// active, stamped as the roll that materialised it. The roll resolves the
// grant definition's group name to the version current at roll time and
// passes the resolved id here; the scope version, the dimension and the
// amount are copied from the definition so the row stands alone as history
// even though the definition row itself is immutable too. The bounds are
// the cycle's, snapshotted from the subscription — stored here because the
// subscription's own fields advance to the next cycle, and history must
// stay reconstructible without them.
func NewEntitlement(id EntitlementID, subscriptionID SubscriptionID, cycle int, definition GrantDefinition, scopeVersion AliasGroupVersionID, periodStart, periodEnd, now time.Time) (*Entitlement, error) {
	if id == "" || subscriptionID == "" || definition.ID == "" {
		return nil, fmt.Errorf("commerce: new entitlement: blank id")
	}
	if cycle < 1 {
		return nil, fmt.Errorf("commerce: new entitlement: %w: cycle %d starts below 1", ErrInvalidPeriod, cycle)
	}
	if err := validateAliasGroupVersionID(scopeVersion); err != nil {
		return nil, err
	}
	start := periodStart.UTC()
	end := periodEnd.UTC()
	if !end.After(start) {
		return nil, fmt.Errorf("commerce: new entitlement: %w: [%s, %s) is empty or inverted", ErrInvalidPeriod, start, end)
	}
	stamp := now.UTC()
	return &Entitlement{
		ID:                  id,
		SubscriptionID:      subscriptionID,
		CycleNumber:         cycle,
		GrantDefinitionID:   definition.ID,
		AliasGroupVersionID: scopeVersion,
		Dimension:           definition.Dimension,
		GrantedAmount:       definition.GrantedAmount,
		State:               EntitlementActive,
		PeriodStart:         start,
		PeriodEnd:           end,
		CreatedAt:           stamp,
		UpdatedAt:           stamp,
	}, nil
}

// Covers reports whether the entitlement's cycle covers at — the half-open
// test [PeriodStart, PeriodEnd) the waterfall's match step runs. Capacity
// stops being available to new admissions at PeriodEnd; an allocation
// reserved before it stays settlement-eligible against this row after it.
func (e *Entitlement) Covers(at time.Time) bool {
	return e.State == EntitlementActive && CoversPeriod(e.PeriodStart, e.PeriodEnd, at)
}

// Expire is the terminal flip: the cycle has ended and the grant stops
// being available to new admissions. Expiring an expired entitlement is a
// no-op — the terminal state absorbs repetition — and expiring an active
// one before its end is refused: the expiry scan runs the same predicate
// IsDueForExpiry checks, and a row that has not ended stays active.
func (e *Entitlement) Expire(now time.Time) error {
	switch e.State {
	case EntitlementExpired:
		return nil
	case EntitlementActive:
		if !now.UTC().Before(e.PeriodEnd) {
			e.State = EntitlementExpired
			e.UpdatedAt = now.UTC()
			return nil
		}
		return fmt.Errorf("commerce: expire entitlement %s: %w: its cycle ends at %s", e.ID, ErrNotDue, e.PeriodEnd.UTC())
	default:
		return fmt.Errorf("commerce: expire entitlement %s: %w: unknown state %q", e.ID, ErrInvalidTransition, e.State)
	}
}
