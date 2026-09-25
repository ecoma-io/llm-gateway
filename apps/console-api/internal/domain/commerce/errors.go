package commerce

import "errors"

// The domain's sentinel errors. Use cases and adapters match on these with
// errors.Is; everything a caller could branch on is here, and the aggregate
// methods wrap them with the context that names the row and the rule.
var (
	// ErrInvalidTransition is a lifecycle move the state machine forbids:
	// a move out of a terminal state, a promotion that is not due, a roll
	// the cancellation or renewal data blocks.
	ErrInvalidTransition = errors.New("invalid transition")
	// ErrNotDue is a due-work move whose scan predicate does not hold at
	// the instant it was asked for — the subscription's start has not
	// arrived, its period has not ended, its cancel_at has not passed.
	// It is a no-op answer, not a rule violation: the worker's scan and
	// the row's state simply raced, and the next scan settles it.
	ErrNotDue = errors.New("not due")
	// ErrPlanVersionNotPublished is a subscription created against a plan
	// version that is not published — drafts are editable and a live
	// commercial state must never reinterpret under an edit, and retired
	// versions take no new subscriptions.
	ErrPlanVersionNotPublished = errors.New("plan version not published")
	// ErrVersionNotEditable is an edit of a plan version that has left
	// draft: published versions are immutable, retiring rewrites nothing.
	ErrVersionNotEditable = errors.New("plan version not editable")
	// ErrInvalidPlanName is a plan name outside the grammar the schema
	// enforces: non-blank, at most 256 runes.
	ErrInvalidPlanName = errors.New("invalid plan name")
	// ErrInvalidAliasGroupName is a grant scope outside catalog's own
	// group-name grammar.
	ErrInvalidAliasGroupName = errors.New("invalid alias group name")
	// ErrInvalidAliasGroupVersionID is a catalog snapshot reference that
	// is not a version-7 uuid — the one promise the domain makes about a
	// reference it did not mint.
	ErrInvalidAliasGroupVersionID = errors.New("invalid alias group version id")
	// ErrInvalidFundingBucketID is an Accounting bucket reference that is
	// not a version-7 uuid.
	ErrInvalidFundingBucketID = errors.New("invalid funding bucket id")
	// ErrInvalidGrantAmount is a grant amount that grants nothing or
	// owes: amounts are strictly positive minor units.
	ErrInvalidGrantAmount = errors.New("invalid grant amount")
	// ErrInvalidPrice is a recurring price that is negative; prices are
	// minor units, and zero is a free plan, not an invalid one.
	ErrInvalidPrice = errors.New("invalid recurring price")
	// ErrInvalidPeriod is a cycle whose bounds do not form a non-empty
	// half-open interval.
	ErrInvalidPeriod = errors.New("invalid period")
	// ErrDuplicateGrantScope is a second grant definition for the same
	// (alias group, dimension) within one plan version.
	ErrDuplicateGrantScope = errors.New("duplicate grant scope")
	// ErrInvalidCancellation is a cancellation instruction the lifecycle
	// cannot record: a blank instant, or one set on a subscription that
	// is not active.
	ErrInvalidCancellation = errors.New("invalid cancellation")
)
