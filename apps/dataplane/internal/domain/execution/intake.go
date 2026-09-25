package execution

import (
	"errors"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// FinalStatus is the terminal pointer the replay record carries: which way the
// original request ended. It is Status minus executing — the pointer exists to
// say the wait is over, so a pointer at executing would be the record telling
// a replay caller to queue, which the protocol refuses to do (a replay arrives
// then with retry-later semantics, never a queue slot).
type FinalStatus string

const (
	FinalSucceeded FinalStatus = "succeeded"
	FinalFailed    FinalStatus = "failed"
	FinalRejected  FinalStatus = "rejected"
)

// ErrIntakeFinalised is a second finalisation of one replay record. The
// pointer is written once at the original request's finalisation; a caller
// seeing this error is looking at a finalisation that already happened, which
// is information, not a failure to retry.
var ErrIntakeFinalised = errors.New("execution: replay record is already finalised")

// ErrIntakePairing marks a final pointer whose status and reason disagree —
// a rejected pointer without its rejection reason, a failed one carrying a
// rejection, a succeeded one carrying either. The intake table's three shape
// CHECKs refuse the same rows.
var ErrIntakePairing = errors.New("execution: final status and reason disagree")

// Intake is the relational replay record: one row per (account,
// idempotency key), decided on the hot path with the Control Plane switched
// off, and never joined to the event family for a business decision (ADR
// 0004, ADR 0005).
//
// Its identity fields — account, key, digest, request id — are written once
// at admission and are immutable afterwards; "immutable afterwards" in
// request-lifecycle.md is scoped to exactly these. The final pointer is the
// one mutation, written once, at the original request's finalisation.
type Intake struct {
	AccountID      string
	IdempotencyKey string
	// RequestDigest is the canonicalised request body's digest: the replay's
	// sameness test. Two arrivals with one key but different digests are a
	// conflict, refused with no second row and no overwrite.
	RequestDigest string
	RequestID     identity.RequestID

	// FinalStatus is nil while the original request still executes. Its
	// non-nil forms each carry their own reason, and the pairings are the
	// intake table's shape CHECKs: rejected pairs with a rejection reason and
	// nothing else, failed with a failure reason, succeeded with neither.
	FinalStatus          *FinalStatus
	FinalRejectionReason RejectionReason
	FinalFailureReason   FailureReason

	CreatedAt time.Time
}

// NewIntake opens the replay record at admission, before the request it
// names has finalised or possibly even executed: the record exists so a
// replay can be answered from one row, whatever happens next.
func NewIntake(accountID, idempotencyKey, requestDigest string, requestID identity.RequestID, createdAt time.Time) (Intake, error) {
	if accountID == "" || idempotencyKey == "" {
		return Intake{}, errors.New("execution: a replay record needs its account and idempotency key")
	}
	if requestDigest == "" {
		return Intake{}, errors.New("execution: a replay record carries the digest it decided on")
	}
	if requestID == "" {
		return Intake{}, errors.New("execution: a replay record names the request it was decided with")
	}
	return Intake{
		AccountID:      accountID,
		IdempotencyKey: idempotencyKey,
		RequestDigest:  requestDigest,
		RequestID:      requestID,
		CreatedAt:      createdAt,
	}, nil
}

// Finalise writes the terminal pointer, once. The status/reason pairings here
// are the intake table's shape CHECKs spoken where the decision is formed, and
// the check is worth its lines: a pointer whose halves disagree would answer
// every future replay with a sentence that contradicts itself.
func (i *Intake) Finalise(status FinalStatus, rejection RejectionReason, failure FailureReason) error {
	if i.FinalStatus != nil {
		return ErrIntakeFinalised
	}
	switch status {
	case FinalSucceeded:
		if rejection != "" || failure != "" {
			return ErrIntakePairing
		}
	case FinalRejected:
		if !rejection.known() || failure != "" {
			return ErrIntakePairing
		}
	case FinalFailed:
		if rejection != "" || !failureKnown(failure) {
			return ErrIntakePairing
		}
	default:
		return ErrIntakePairing
	}
	i.FinalStatus = &status
	i.FinalRejectionReason = rejection
	i.FinalFailureReason = failure
	return nil
}

// Decided reports whether the original request has finalised. A replay
// arriving while this is false is refused with retry-later semantics — the
// record cannot answer for a request that has not ended.
func (i Intake) Decided() bool {
	return i.FinalStatus != nil
}

// failureKnown is request.go's rejection check's twin for the two-value
// failure vocabulary.
func failureKnown(reason FailureReason) bool {
	switch reason {
	case FailedStreamAfterCommitment, FailedGatewayAbandoned:
		return true
	}
	return false
}
