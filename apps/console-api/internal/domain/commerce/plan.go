package commerce

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// maxPlanNameLen bounds the operator-chosen plan name. Like identity's name
// bound, it is a rune count, not a byte count, so a name's worth does not
// depend on its script.
const maxPlanNameLen = 256

// Dimension denominates grants. Cost is the only defined dimension — ledger
// currency's minor units; token and request dimensions are named future
// extensions, never implicit conversion rules (ADR 0003).
type Dimension string

// DimensionCost denominates grants in the settlement currency's minor units.
const DimensionCost Dimension = "cost"

// Plan is the commercial product's identity root: a name and nothing else.
// A plan grants nothing by itself — every commercial term lives on one of
// its immutable versions — and the row never changes after creation, which
// is why it carries no updated instant and no transitions.
type Plan struct {
	ID        PlanID
	Name      string
	CreatedAt time.Time
}

// NewPlan returns a new plan root. The name must say something, within the
// bound the schema enforces; uniqueness across plans is the schema's
// plans_name_key, surfaced by the persistence adapter as a sentinel the use
// case maps onward.
func NewPlan(id PlanID, name string, now time.Time) (*Plan, error) {
	name = trimSpace(name)
	if err := validatePlanName(name); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("commerce: new plan: blank id")
	}
	return &Plan{ID: id, Name: name, CreatedAt: now.UTC()}, nil
}

// PlanVersionState is a version's position on the lifecycle ADR 0003 fixes:
// draft → published → retired. Drafts are editable freely; once published a
// version is immutable; retiring stops new subscriptions and rewrites
// nothing.
type PlanVersionState string

const (
	// PlanVersionDraft: the version is being composed and may be edited.
	// It accepts no subscriptions.
	PlanVersionDraft PlanVersionState = "draft"
	// PlanVersionPublished: the version's terms are frozen forever and it
	// is the only state a subscription may pin.
	PlanVersionPublished PlanVersionState = "published"
	// PlanVersionRetired: terminal. New subscriptions are refused;
	// existing ones are untouched — their pinned version keeps every term
	// it was bought under.
	PlanVersionRetired PlanVersionState = "retired"
)

// RecurringPeriod is the billing-period shape of a version. Calendar-month
// is the only shape v1 defines; others are named future extensions, spelled
// exactly as the schema's CHECK does.
type RecurringPeriod string

// RecurringPeriodCalendarMonth: each cycle is a calendar month anchored at
// the subscription's start_at, day-of-month clipped at month end.
const RecurringPeriodCalendarMonth RecurringPeriod = "calendar_month"

// PlanVersion is one immutable version of a plan: the recurring price, the
// period shape, and the grant definitions. A subscription pins one version
// forever — a plan change is a new version and a new subscription, never an
// edit in place — which is why the methods below refuse every mutation once
// the version has left draft.
type PlanVersion struct {
	ID                       PlanVersionID
	PlanID                   PlanID
	VersionNumber            int
	Period                   RecurringPeriod
	RecurringPriceMinorUnits int64
	State                    PlanVersionState
	PublishedAt              *time.Time
	RetiredAt                *time.Time
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// NewPlanVersion returns a version in its only legal birth state, draft,
// with no publication or retirement stamps. The price may be zero — a free
// plan is a plan — but never negative. Version numbering is the caller's
// discipline (monotonic per plan, unique by the schema's
// plan_versions_version_number_key); the domain refuses only what is
// nonsensical on its face.
func NewPlanVersion(id PlanVersionID, planID PlanID, versionNumber int, period RecurringPeriod, recurringPriceMinorUnits int64, now time.Time) (*PlanVersion, error) {
	if id == "" || planID == "" {
		return nil, fmt.Errorf("commerce: new plan version: blank id")
	}
	if versionNumber < 1 {
		return nil, fmt.Errorf("commerce: new plan version: %w: version number %d starts below 1", ErrInvalidPeriod, versionNumber)
	}
	if period != RecurringPeriodCalendarMonth {
		return nil, fmt.Errorf("commerce: new plan version: unknown period %q", period)
	}
	if recurringPriceMinorUnits < 0 {
		return nil, fmt.Errorf("commerce: new plan version: %w: %d minor units", ErrInvalidPrice, recurringPriceMinorUnits)
	}
	now = now.UTC()
	return &PlanVersion{
		ID:                       id,
		PlanID:                   planID,
		VersionNumber:            versionNumber,
		Period:                   period,
		RecurringPriceMinorUnits: recurringPriceMinorUnits,
		State:                    PlanVersionDraft,
		CreatedAt:                now,
		UpdatedAt:                now,
	}, nil
}

// Publish freezes the version's terms forever. Only a draft publishes; the
// stamps the state machine expects are set exactly as the schema's
// plan_versions_state_stamps CHECK will demand of the persisted row.
func (v *PlanVersion) Publish(now time.Time) error {
	switch v.State {
	case PlanVersionPublished:
		return nil
	case PlanVersionRetired:
		return fmt.Errorf("commerce: publish plan version %s: %w: version is retired", v.ID, ErrInvalidTransition)
	case PlanVersionDraft:
		stamp := now.UTC()
		v.State = PlanVersionPublished
		v.PublishedAt = &stamp
		v.UpdatedAt = stamp
		return nil
	default:
		return fmt.Errorf("commerce: publish plan version %s: %w: unknown state %q", v.ID, ErrInvalidTransition, v.State)
	}
}

// Retire stops new subscriptions to the version and rewrites nothing else.
// Only a published version retires — a draft that will never be sold is
// abandoned, not retired. Retired is terminal.
func (v *PlanVersion) Retire(now time.Time) error {
	switch v.State {
	case PlanVersionRetired:
		return nil
	case PlanVersionDraft:
		return fmt.Errorf("commerce: retire plan version %s: %w: a draft is abandoned, not retired", v.ID, ErrInvalidTransition)
	case PlanVersionPublished:
		stamp := now.UTC()
		v.State = PlanVersionRetired
		v.RetiredAt = &stamp
		v.UpdatedAt = stamp
		return nil
	default:
		return fmt.Errorf("commerce: retire plan version %s: %w: unknown state %q", v.ID, ErrInvalidTransition, v.State)
	}
}

// AcceptsSubscriptions reports whether a new subscription may pin this
// version: only a published one. The subscribing use case enforces this —
// no constraint can read another row's state — and this method is the
// domain's own statement of the same rule, so the use case and any future
// caller cannot drift apart.
func (v *PlanVersion) AcceptsSubscriptions() bool {
	return v.State == PlanVersionPublished
}

// GrantDefinition is one grant of one plan version: a stable id, the alias
// group NAME the grant is scoped to (the roll resolves that name to the
// group's current version — ADR 0003), a dimension, and the amount granted
// per cycle.
type GrantDefinition struct {
	ID             GrantDefinitionID
	PlanVersionID  PlanVersionID
	AliasGroupName AliasGroupName
	Dimension      Dimension
	GrantedAmount  int64
}

// NewGrantDefinition validates one definition against the rules the schema
// will enforce: the group-name grammar (catalog's own, wildcard included),
// the one defined dimension, and a strictly positive amount — a zero grant
// purchases nothing.
func NewGrantDefinition(id GrantDefinitionID, planVersionID PlanVersionID, scope AliasGroupName, dimension Dimension, grantedAmount int64) (*GrantDefinition, error) {
	if id == "" || planVersionID == "" {
		return nil, fmt.Errorf("commerce: new grant definition: blank id")
	}
	if err := validateAliasGroupName(scope); err != nil {
		return nil, err
	}
	if dimension != DimensionCost {
		return nil, fmt.Errorf("commerce: new grant definition: unknown dimension %q", dimension)
	}
	if grantedAmount <= 0 {
		return nil, fmt.Errorf("commerce: new grant definition: %w: %d minor units", ErrInvalidGrantAmount, grantedAmount)
	}
	return &GrantDefinition{
		ID:             id,
		PlanVersionID:  planVersionID,
		AliasGroupName: scope,
		Dimension:      dimension,
		GrantedAmount:  grantedAmount,
	}, nil
}

// IsWildcard reports whether the definition's scope is the reserved
// wildcard — every alias, including aliases that do not exist yet. The
// waterfall reads this as scope specificity: a named group is always
// consumed before the wildcard.
func (d *GrantDefinition) IsWildcard() bool {
	return d.AliasGroupName == WildcardGroupName
}

// validatePlanName enforces the one rule the plan name has.
func validatePlanName(name string) error {
	if trimmed := trimSpace(name); trimmed == "" {
		return fmt.Errorf("commerce: new plan: %w: name is blank", ErrInvalidPlanName)
	}
	if n := utf8.RuneCountInString(name); n > maxPlanNameLen {
		return fmt.Errorf("commerce: new plan: %w: %d runes exceeds %d", ErrInvalidPlanName, n, maxPlanNameLen)
	}
	return nil
}

// trimSpace is the single trimming rule for operator-supplied labels —
// identity's convention, kept here rather than imported across a bounded
// context: plain whitespace around the value goes, interior content stays
// untouched, and the persisted value is the trimmed form.
func trimSpace(s string) string {
	return strings.TrimSpace(s)
}
