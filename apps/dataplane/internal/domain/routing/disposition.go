package routing

import (
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
)

// Disposition is the closed switch behind routing.md's retry-vs-fallback
// table: one failure class, one consequence. Four classes send the walk on —
// the condition might clear at another candidate, so the next one is tried
// while the attempt budget lasts. Three surface their refusal and stop the
// walk, because the caller's request is the thing at fault and a second
// candidate would fail identically or answer differently. The eighth class —
// a stream that died after commitment — never reaches this switch at all: the
// commitment gate above it has already decided, and no disposition can unsend
// what the client read.
//
// A post-commitment call is prevented by the caller's sequencing, not by this
// package; the two functions below are pre-commitment vocabulary only, and
// the surfaced refusals are exactly the three failure reasons whose domain
// transition is FailBeforeCommitment.
//
// The switch is exhaustive over the error-class vocabulary, spelled as a
// default-less mapping between two functions so a class added to execution's
// list without an entry here is a compile-visible decision rather than a
// silent fall-through. The table in routing.md is this switch's prose twin;
// the two change together or the docs lie.

// FallbackEligible reports whether a failed candidate's class sends the walk
// to the next candidate. It is asked only for a pre-commitment failure.
func FallbackEligible(class execution.ErrorClass) bool {
	switch class {
	case execution.ErrorRateLimited,
		execution.ErrorProviderUnavailable,
		execution.ErrorUpstreamError,
		execution.ErrorInvalidUpstreamResponse:
		return true
	case execution.ErrorAuthentication,
		execution.ErrorProviderRejectedRequest,
		execution.ErrorContextTooLarge,
		execution.ErrorStreamAfterCommitment:
		return false
	}
	return false
}

// SurfacedRefusal maps a failure class to the failure reason the request row
// stores and the wire re-derives its answer from, and reports whether the
// class surfaces at all. The mapping is execution's own vocabulary — each
// refusal keeps its own name, because the replay record carries the reason to
// re-derive the original answer byte for byte, and the three refusals' answers
// differ. A class that is not surfaced has no reason, and the false return is
// the caller's cue to fall through instead of asking again.
func SurfacedRefusal(class execution.ErrorClass) (execution.FailureReason, bool) {
	switch class {
	case execution.ErrorProviderRejectedRequest:
		return execution.FailedProviderRejectedRequest, true
	case execution.ErrorContextTooLarge:
		return execution.FailedContextTooLarge, true
	case execution.ErrorAuthentication:
		return execution.FailedUpstreamAuthentication, true
	}
	return "", false
}
