// Package execution is the domain of one request's lifecycle: the request
// itself, the upstream attempts it spent, and the replay record admission
// keeps for its idempotency key.
//
// The package owns the terminal vocabulary and the state machine the
// `dataplane` database enforces a second time (migration
// 000003_runtime_storage: every shape CHECK here has a twin constraint on the
// row). The doubling is the design, not a courtesy copy: the database is the
// final guard — no process, version or bug can write a row the vocabulary
// forbids — while this package is where a caller gets a legible error instead
// of a constraint violation, at the moment it forms the decision rather than
// at the moment it tries to persist it. A transition this package refuses was
// never legal; one it allows can still be refused by a concurrent writer, and
// the repositories carry that answer as their CAS result.
//
// Nothing here imports a driver, a port or a transport. A domain type that
// knew how it was stored would not be a domain type.
package execution

import (
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// The terminal vocabulary of a request's final status. These are the values
// request-lifecycle.md's step table and the migration's CHECKs spell out, and
// they are spelled as literals rather than read from anywhere for the same
// reason the wire codes are: the definition is the document.
//
//   - Executing is the row's birth state: admitted, reservation opened, no
//     attempt appended yet (attempts are written when a call finishes, never
//     while it is in flight — ADR 0001 rule 4).
//   - Succeeded is a request whose committed attempt produced a settled usage.
//   - Failed is a request whose execution ended without a settleable outcome;
//     the failure reason says which side of the commitment it died on.
//   - Rejected is a request admission refused before any upstream call; the
//     rejection reason names the admission step that refused it.
//
// Two failure words the docs name are deliberately absent from every reason
// vocabulary: `unauthenticated` writes no row at all, and `idempotency_conflict`
// reuses the original request's row (request-lifecycle.md step 4). Neither can
// appear on a request, so neither has a constant here to be chosen by mistake.
type Status string

const (
	StatusExecuting Status = "executing"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusRejected  Status = "rejected"
)

// RejectionReason names the admission step that refused a request. The eight
// values are request-lifecycle.md steps 2–8 verbatim; a rejection row is
// written "with the fields known so far", so these name what refused it and
// nothing else.
type RejectionReason string

const (
	RejectedAccountSuspended        RejectionReason = "account_suspended"
	RejectedAccountClosed           RejectionReason = "account_closed"
	RejectedUnknownAlias            RejectionReason = "unknown_alias"
	RejectedInvalidRequest          RejectionReason = "invalid_request"
	RejectedInsufficientEntitlement RejectionReason = "insufficient_entitlement"
	RejectedNoAccess                RejectionReason = "no_access"
	RejectedNoCandidate             RejectionReason = "no_candidate"
	RejectedNoCandidateSucceeded    RejectionReason = "no_candidate_succeeded"
)

// FailureReason names how an execution died, and the value carries the
// commitment with it:
//
//   - StreamFailedAfterCommitment: the provider stream broke after the request
//     had crossed its commitment point. The committed attempt is part of the
//     decision — the usage that was in flight belongs to it, and the settlement
//     of that usage is exactly what B7's fact vocabulary records. This is ADR
//     0001 rule 6's "finalise the request with its failure reason".
//   - GatewayAbandoned: the runtime died (or gave the request up) and the
//     reaper closed the row. The reaper cannot know whether a commitment
//     happened in the dead process, so this reason claims no attempt — and the
//     pairing is enforced by the database, which refuses an abandoned request
//     that names one.
type FailureReason string

const (
	FailedStreamAfterCommitment FailureReason = "stream_failed_after_commitment"
	FailedGatewayAbandoned      FailureReason = "gateway_abandoned"
)

// PriceSnapshot is the price basis a final request carries: the revision it
// was priced under and the two resolved unit prices in integer minor units.
//
// It is carried on the row because the settlement has no other input: a final
// request whose fact cannot be priced from its own columns would need a join
// into a catalog that can change under it. The all-or-nothing pairing is the
// requests_price_snapshot_pairing CHECK.
type PriceSnapshot struct {
	RevisionID      string
	InputUnitPrice  int64
	OutputUnitPrice int64
}

// ErrFinalised is returned by every transition attempted on a request that has
// already reached a terminal status. A request finalises exactly once — by
// settlement close, by release, or by the reaper — and a second finalisation is
// not an error to retry but a decision to re-read: someone else's decision
// already stands.
var ErrFinalised = errors.New("execution: request is already finalised")

// ErrNilAttempt names a succeeded or stream-failed finalisation that arrived
// without the attempt that decision must name. The pairing is enforced by the
// database; refusing it here keeps the legible error at the call site.
var ErrNilAttempt = errors.New("execution: the decision must name the attempt it commits")

// ErrAttemptOfAnotherRequest marks a finalisation that names an attempt made
// for a different request. No field of Request can check it — the attempts are
// the repository's rows, not this struct's — so the rule is spoken where the
// evidence lives: the composite requests_committed_attempt_fkey refuses the
// write, and the repository translates that refusal into this sentinel so the
// caller reads one vocabulary on both sides of the boundary.
var ErrAttemptOfAnotherRequest = errors.New("execution: the named attempt does not belong to this request")

// fmtErrUnknownRejection builds the error the rejection paths return for a
// reason outside the vocabulary. It is a function rather than a sentinel
// because the value itself is the message: a caller reading the log line sees
// the string it actually passed.
func fmtErrUnknownRejection(reason RejectionReason) error {
	return fmt.Errorf("execution: %q is not a rejection reason this vocabulary declares", string(reason))
}

// Request is one admitted request and the only mutable aggregate in the
// package: it is born executing and finalises exactly once.
type Request struct {
	ID          identity.RequestID
	AccountID   string
	APIKeyID    string
	Alias       string
	InputTokens int
	// MaxOutputTokens is the ceiling the caller asked for, canonicalised at
	// admission. It is positive on every row the database accepts, and
	// NewRequest refuses zero before the row is formed.
	MaxOutputTokens int
	Price           PriceSnapshot
	// Status is the row's state; every field below it is shaped by the status,
	// and the transition methods are the only writers of both.
	Status             Status
	RejectionReason    RejectionReason
	FailureReason      FailureReason
	CommittedAttemptID identity.AttemptID
	AdmittedAt         time.Time
	FinishedAt         time.Time
}

// NewRequest opens a request at admission: executing, with the admission
// snapshot its eventual fact will be priced from. The rejection path may know
// less — a row written "with the fields known so far" is formed directly by
// RejectNew — but an admitted request knows its alias, its bounds and its
// price, because the fact of it cannot be settled without them. The alias and
// a positive input count are part of that knowledge: the alias is what every
// final row must carry (the final-snapshot shape refuses one without it, so
// an alias-less admission would be a request that can never finalise), and a
// zero input count would open a zero hold whose reservation could take no
// legs — a settlement that could never be built.
func NewRequest(id identity.RequestID, accountID, apiKeyID, alias string, inputTokens, maxOutputTokens int, price PriceSnapshot, admittedAt time.Time) (Request, error) {
	if id == "" {
		return Request{}, errors.New("execution: a request needs an id")
	}
	if accountID == "" || apiKeyID == "" {
		return Request{}, errors.New("execution: a request needs its account and api key")
	}
	if alias == "" {
		return Request{}, errors.New("execution: an admitted request knows its alias")
	}
	if inputTokens <= 0 {
		return Request{}, errors.New("execution: input tokens must be positive")
	}
	if maxOutputTokens <= 0 {
		return Request{}, errors.New("execution: max output tokens must be positive")
	}
	if price.RevisionID == "" {
		return Request{}, errors.New("execution: an admitted request carries a price snapshot")
	}
	return Request{
		ID:              id,
		AccountID:       accountID,
		APIKeyID:        apiKeyID,
		Alias:           alias,
		InputTokens:     inputTokens,
		MaxOutputTokens: maxOutputTokens,
		Price:           price,
		Status:          StatusExecuting,
		AdmittedAt:      admittedAt,
	}, nil
}

// RejectNew forms the rejection row admission writes when it refuses a
// request: terminal from birth, carrying "the fields known so far" — which at
// step 2–8 may be none of the snapshot. It exists beside NewRequest because
// the two writes differ in kind, not only in status: an admitted request is
// born mutable, a rejected one is born final.
func RejectNew(id identity.RequestID, accountID, apiKeyID, alias string, reason RejectionReason, admittedAt time.Time) (Request, error) {
	if id == "" {
		return Request{}, errors.New("execution: a request needs an id")
	}
	if !reason.known() {
		return Request{}, fmtErrUnknownRejection(reason)
	}
	return Request{
		ID:              id,
		AccountID:       accountID,
		APIKeyID:        apiKeyID,
		Alias:           alias,
		Status:          StatusRejected,
		RejectionReason: reason,
		AdmittedAt:      admittedAt,
		FinishedAt:      admittedAt,
	}, nil
}

// Reject refuses an executing request at an admission step that ran after the
// row was opened. It is the transition behind a replay that turns out to be
// its request's first arrival's duplicate fate and other late-admission
// refusals; like every finalisation it is once-only.
func (r *Request) Reject(reason RejectionReason, finishedAt time.Time) error {
	if r.Status != StatusExecuting {
		return ErrFinalised
	}
	if !reason.known() {
		return fmtErrUnknownRejection(reason)
	}
	r.Status = StatusRejected
	r.RejectionReason = reason
	r.FinishedAt = finishedAt
	return nil
}

// Succeed finalises an executing request as succeeded, naming the attempt
// whose usage the settlement will carry. That the attempt is one of this
// request's own is the composite foreign key's rule, checked when the
// decision is persisted (see checkFinalisable).
func (r *Request) Succeed(attempt identity.AttemptID, finishedAt time.Time) error {
	if err := r.checkFinalisable(attempt); err != nil {
		return err
	}
	r.Status = StatusSucceeded
	r.CommittedAttemptID = attempt
	r.FinishedAt = finishedAt
	return nil
}

// FailAfterCommitment finalises an executing request whose provider stream
// died after its commitment point. The named attempt is the one the commitment
// happened on; the pairing of reason and attempt is what the fact vocabulary
// and the database both pin.
func (r *Request) FailAfterCommitment(attempt identity.AttemptID, finishedAt time.Time) error {
	if err := r.checkFinalisable(attempt); err != nil {
		return err
	}
	r.Status = StatusFailed
	r.FailureReason = FailedStreamAfterCommitment
	r.CommittedAttemptID = attempt
	r.FinishedAt = finishedAt
	return nil
}

// FailAbandoned is the reaper's close: the runtime can no longer ask the dead
// process whether a commitment happened, so the row finalises without naming
// an attempt. An abandoned request that named one would be a claim the reaper
// has no evidence for, and requests_failure_reason_attempt_pairing refuses it
// on the row.
func (r *Request) FailAbandoned(finishedAt time.Time) error {
	if r.Status != StatusExecuting {
		return ErrFinalised
	}
	r.Status = StatusFailed
	r.FailureReason = FailedGatewayAbandoned
	r.FinishedAt = finishedAt
	return nil
}

// checkFinalisable is the shared guard of the two finalisations that name an
// attempt: the request must still be executing, and the decision must carry
// the attempt it commits. That the attempt belongs to this request is the
// composite foreign key's rule, enforced when the decision is persisted and
// surfaced as ErrAttemptOfAnotherRequest; no field of this struct can check it
// here. Every reason column the transition leaves unset stays unset — an
// executing row cannot have grown a rejection reason.
func (r *Request) checkFinalisable(attempt identity.AttemptID) error {
	if r.Status != StatusExecuting {
		return ErrFinalised
	}
	if attempt == "" {
		return ErrNilAttempt
	}
	return nil
}

// known reports whether the reason is one the vocabulary declares. The check
// behind every rejection path: an unknown reason is a caller's typo or a
// foreign vocabulary, and both are better refused than persisted into a row
// the database would refuse a statement later.
func (reason RejectionReason) known() bool {
	switch reason {
	case RejectedAccountSuspended, RejectedAccountClosed, RejectedUnknownAlias,
		RejectedInvalidRequest, RejectedInsufficientEntitlement, RejectedNoAccess,
		RejectedNoCandidate, RejectedNoCandidateSucceeded:
		return true
	}
	return false
}

// Terminal reports whether the request has finalised. It is the question every
// replay answer starts from, and the one the intake's final pointer mirrors.
func (r Request) Terminal() bool {
	return r.Status != StatusExecuting
}
