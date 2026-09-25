package execution

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// Outcome is one upstream call's end, and the value places the call on one
// side of the commitment point:
//
//   - Succeeded: the stream completed and its usage is the request's billing
//     subject. No error class exists to name, and the database refuses one.
//   - FailedBeforeCommitment: the call failed early enough that nothing was
//     owed — an authentication refusal, a rate limit, a context too large.
//     There is always a class to name, and the database requires one.
//   - FailedAfterCommitment: the call crossed its commitment point and died
//     anyway — the provider's stream broke mid-flight, or the client walked
//     away from a healthy one. A class may or may not exist: a broken stream
//     has a provider fault to name, a departed client does not, and the
//     settlement of the usage that did arrive proceeds either way.
type Outcome string

const (
	OutcomeSucceeded              Outcome = "succeeded"
	OutcomeFailedBeforeCommitment Outcome = "failed_before_commitment"
	OutcomeFailedAfterCommitment  Outcome = "failed_after_commitment"
)

// ErrorClass is the closed vocabulary of upstream faults, from ADR 0002. It is
// nullable by design: an attempt that succeeded, or one that died to a client
// departure, has no provider fault to name.
type ErrorClass string

const (
	ErrorAuthentication          ErrorClass = "authentication"
	ErrorRateLimited             ErrorClass = "rate_limited"
	ErrorProviderUnavailable     ErrorClass = "provider_unavailable"
	ErrorProviderRejectedRequest ErrorClass = "provider_rejected_request"
	ErrorContextTooLarge         ErrorClass = "context_too_large"
	ErrorInvalidUpstreamResponse ErrorClass = "invalid_upstream_response"
	ErrorUpstreamError           ErrorClass = "upstream_error"
	ErrorStreamAfterCommitment   ErrorClass = "stream_failed_after_commitment"
)

// ErrAttemptOutcomePairing marks an attempt whose outcome and error class
// disagree: a succeeded attempt with a class, or a pre-commitment failure
// without one. The database refuses both shapes; refusing them here is the
// same refusal at the moment the row is formed.
var ErrAttemptOutcomePairing = errors.New("execution: attempt outcome and error class disagree")

// ErrUnknownErrorClass marks an error class outside the vocabulary — a typo
// here is better than a constraint violation at the write.
var ErrUnknownErrorClass = errors.New("execution: unknown attempt error class")

// ErrUsageNegative marks a provider-usage figure below zero. Usage is a count,
// not a balance: there is no negative token.
var ErrUsageNegative = errors.New("execution: provider usage must not be negative")

// maxProviderErrorOctets is the serialised cap on the one sanctioned jsonb in
// the runtime schema. It is half of the database's own 65536-octet guard, for
// the same reason the fact payload's writer cap is half of its: the legible
// error belongs at the write site, the constraint is the wall behind it.
const maxProviderErrorOctets = 32768

// MaxProviderRequestIDOctets is the bound the schema's CHECK puts on the
// provider's own correlation handle. It is exported because the handle is the
// one provider-controlled column written by direct assignment, so the writer
// that composes the row is also the writer that keeps the handle inside the
// wall — a provider echoing a verbose handle must cost the row a truncation,
// never the settlement it rides on.
const MaxProviderRequestIDOctets = 256

// Attempt is one upstream call, appended as the call finishes and never while
// it is in flight (ADR 0001 rule 4) — a crash mid-call leaves no row, and no
// transaction is ever open across a provider call.
//
// The row is insert-only for its business content. The one sanctioned update
// rewrites the provider-usage telemetry: usage reported after a client
// disconnect arrives after the row exists, and RecordProviderUsage is that
// arrival's only door. Identity, outcome, class and timing never change.
type Attempt struct {
	ID                identity.AttemptID
	RequestID         identity.RequestID
	CandidatePosition int
	RetrySequence     int
	BackendID         string
	ProviderModel     string
	ProviderRequestID string
	Outcome           Outcome
	ErrorClass        ErrorClass
	// ProviderUsage is the telemetry the provider reported: input, output and
	// delivery token counts. Nil means not known — a pre-commitment failure
	// has no usage, and a post-disconnect report may not have arrived yet.
	// Counts stay nil rather than zero because zero is a claim (the provider
	// said nothing was used) and nil is a confession (nobody said).
	ProviderInputTokens  *int64
	ProviderOutputTokens *int64
	DeliveryTokens       *int64
	// ProviderError is the provider's own account of a failure — the opaque
	// body, headers or error envelope worth keeping for debugging and worth
	// nothing to settlement. It is an object, capped, and never parsed here:
	// the domain records telemetry, it does not interpret it.
	ProviderError json.RawMessage
	StartedAt     time.Time
	FinishedAt    time.Time
}

// NewAttempt forms a finished call's row and refuses the outcome/error pairings
// the vocabulary forbids before the write has to.
func NewAttempt(id identity.AttemptID, requestID identity.RequestID, candidatePosition, retrySequence int, backendID, providerModel string, outcome Outcome, errorClass ErrorClass, startedAt, finishedAt time.Time) (Attempt, error) {
	if id == "" || requestID == "" {
		return Attempt{}, errors.New("execution: an attempt needs its id and its request")
	}
	if candidatePosition < 0 || retrySequence < 0 {
		return Attempt{}, errors.New("execution: candidate position and retry sequence count from zero")
	}
	if backendID == "" || providerModel == "" {
		return Attempt{}, errors.New("execution: an attempt names its backend and provider model")
	}
	if !outcomeKnown(outcome) {
		return Attempt{}, errors.New("execution: unknown attempt outcome " + string(outcome))
	}
	if errorClass != "" && !errorClassKnown(errorClass) {
		return Attempt{}, ErrUnknownErrorClass
	}
	switch {
	case outcome == OutcomeSucceeded && errorClass != "":
		return Attempt{}, ErrAttemptOutcomePairing
	case outcome == OutcomeFailedBeforeCommitment && errorClass == "":
		return Attempt{}, ErrAttemptOutcomePairing
	}
	if startedAt.IsZero() || finishedAt.IsZero() {
		return Attempt{}, errors.New("execution: an attempt is timed by its caller, never by this package")
	}
	return Attempt{
		ID:                id,
		RequestID:         requestID,
		CandidatePosition: candidatePosition,
		RetrySequence:     retrySequence,
		BackendID:         backendID,
		ProviderModel:     providerModel,
		Outcome:           outcome,
		ErrorClass:        errorClass,
		StartedAt:         startedAt,
		FinishedAt:        finishedAt,
	}, nil
}

// SetProviderError attaches the provider's own failure telemetry. It is an
// object by contract and capped at half the database's guard, and a value that
// is neither is refused here rather than stored as something the debugging
// session that wants it cannot trust.
func (a *Attempt) SetProviderError(err json.RawMessage) error {
	if len(err) == 0 {
		a.ProviderError = nil
		return nil
	}
	if !json.Valid(err) || json.RawMessage(err)[0] != '{' {
		return errors.New("execution: provider error telemetry must be a json object")
	}
	if len(err) > maxProviderErrorOctets {
		return errors.New("execution: provider error telemetry exceeds 32768 octets")
	}
	a.ProviderError = append(json.RawMessage(nil), err...)
	return nil
}

// RecordProviderUsage is the one sanctioned update's domain half: usage that
// arrived after the row existed, written over the nil it started with. A
// report that contradicts a value already present is not an update but a
// second claim, and the caller decides which stands — this method writes nil
// fields and leaves set ones alone, because overwriting observed telemetry
// with a later, different number is a settlement decision, not a telemetry
// one.
func (a *Attempt) RecordProviderUsage(input, output, delivery *int64) error {
	for _, v := range []*int64{input, output, delivery} {
		if v != nil && *v < 0 {
			return ErrUsageNegative
		}
	}
	if a.ProviderInputTokens == nil {
		a.ProviderInputTokens = input
	}
	if a.ProviderOutputTokens == nil {
		a.ProviderOutputTokens = output
	}
	if a.DeliveryTokens == nil {
		a.DeliveryTokens = delivery
	}
	return nil
}

// outcomeKnown and errorClassKnown are the vocabulary checks behind the
// constructors, spelled as switches over the constants above so a new value is
// a one-line edit and an unknown string is always refused.
func outcomeKnown(outcome Outcome) bool {
	switch outcome {
	case OutcomeSucceeded, OutcomeFailedBeforeCommitment, OutcomeFailedAfterCommitment:
		return true
	}
	return false
}

func errorClassKnown(class ErrorClass) bool {
	switch class {
	case ErrorAuthentication, ErrorRateLimited, ErrorProviderUnavailable,
		ErrorProviderRejectedRequest, ErrorContextTooLarge, ErrorInvalidUpstreamResponse,
		ErrorUpstreamError, ErrorStreamAfterCommitment:
		return true
	}
	return false
}

// ErrorClasses is the whole vocabulary, in one slice, for the consumers that
// must face every class a future edit could add — the disposition table and
// the tests that pin it. A class appended to the constants above but not to
// this slice is a lie the vocabulary checks below catch; a class appended to
// both walks straight into every switch that asks this slice, which is the
// point: the addition is a visible edit at each consumer, never a silent
// fall-through.
func ErrorClasses() []ErrorClass {
	return []ErrorClass{
		ErrorAuthentication,
		ErrorRateLimited,
		ErrorProviderUnavailable,
		ErrorProviderRejectedRequest,
		ErrorContextTooLarge,
		ErrorInvalidUpstreamResponse,
		ErrorUpstreamError,
		ErrorStreamAfterCommitment,
	}
}
