package accounting

import "errors"

// The domain's sentinel errors. Use cases and adapters match on these with
// errors.Is; everything a caller could branch on is here, and the aggregate
// methods wrap them with the context that names the row and the rule.
var (
	// ErrInvalidReference is an identifier outside the grammar its owner
	// publishes: a blind reference that is not a uuid, a request id or
	// command key that is blank or over-long. Refused before any statement
	// runs, because a driver error is not an answer a caller can branch on.
	ErrInvalidReference = errors.New("invalid reference")
	// ErrInvalidTransition is a move the bucket's lifecycle forbids — the
	// catch-all the more specific refusals below do not cover.
	ErrInvalidTransition = errors.New("invalid transition")
	// ErrBucketClosed is a leg attempted against a closed bucket. A closed
	// bucket takes no legs of any kind: its history is complete.
	ErrBucketClosed = errors.New("bucket closed")
	// ErrBucketCloseBlocked is a close of a bucket that still has funds
	// held against it. Closing would strand the held amount — the holds are
	// ceilings that must settle or release first.
	ErrBucketCloseBlocked = errors.New("bucket close refused while held funds remain")
	// ErrInsufficientAvailable is a hold whose amount exceeds what the
	// bucket can still secure: available < take. The bucket guard is the
	// statement, so this is the verdict of a lost race as much as of a
	// genuinely empty bucket — both refuse, and neither moves money.
	ErrInsufficientAvailable = errors.New("insufficient available balance")
	// ErrInsufficientHeld is a release or consume whose amount exceeds what
	// the bucket currently holds: held < take. A release above the hold
	// would conjure money out of the ledger.
	ErrInsufficientHeld = errors.New("insufficient held balance")
	// ErrInsufficientSettled is a consume that would take the bucket's
	// settled balance below zero. ADR 0004: no kind can make settled
	// negative — ΣG ≥ ΣC is enforced by this guard — so settled, like held
	// and available, never goes below zero.
	ErrInsufficientSettled = errors.New("insufficient settled balance")
	// ErrInvalidAdjustment is an adjustment that is malformed (its deltas
	// not exactly-one-nonzero, its reason or original entry missing) or no
	// longer fits the balance it lands on (it would push held, settled or
	// available below zero). Adjustments are operator corrections, not an
	// overdraw or credit path; "no longer fits" is a refused correction,
	// never a negative balance.
	ErrInvalidAdjustment = errors.New("invalid adjustment")
	// ErrDuplicateCommand is an idempotency-key collision with a DIFFERENT
	// payload: the same command key re-used with another amount. The same
	// key with the same payload is not this error — it converges on the
	// original leg, which is what the key is for.
	ErrDuplicateCommand = errors.New("duplicate command key with a different payload")
	// ErrDuplicateMovement is a reservation-keyed collision with a
	// different payload: a second hold (or release) for a reservation and
	// bucket that already has one, at a different amount. Facts are
	// immutable, so a differing redelivery is a contract defect, refused
	// rather than silently absorbed.
	ErrDuplicateMovement = errors.New("duplicate reservation movement with a different payload")
	// ErrInvalidSettlement is a settlement the fact cannot support: an
	// allocation consuming more than it held (usage above the hard ceiling
	// is impossible by construction), a consume without its price
	// snapshot, or a header with no legs behind it.
	ErrInvalidSettlement = errors.New("invalid settlement")
	// ErrSettlementConflict is a re-ack of a request_id whose recorded
	// settlement disagrees with the plan at hand — the same request cannot
	// settle twice at two different totals, and the disagreement is a
	// defect to name, not a charge to make.
	ErrSettlementConflict = errors.New("settlement conflict for an already-settled request")
)
