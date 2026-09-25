package routing

import (
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
)

// Selection is the walk the routing stage takes over one request's eligible
// candidates, and position order is the one policy this build ships. The
// catalog assigns positions 1..n in the alias's candidate order; this type
// walks that order and nothing else, because no other strategy has a consumer
// — weights, latencies and cooldowns are named in routing.md as deliberate
// absences, and each would arrive here as a visible edit, not a default.
//
// The type is immutable and carries no state past a call: a selection is a
// list and an index into it, so a hundred concurrent requests over one alias
// walk one hundred independent selections and none of them sees another's.
type Selection struct {
	candidates []catalog.Candidate
}

// NewSelection builds the walk over candidates that are already eligible, in
// the order Eligible preserved. The step numbering is one-based, matching the
// catalog's positions — the walk's first try is step 1 over the alias's first
// eligible candidate.
func NewSelection(eligible []catalog.Candidate) Selection {
	return Selection{candidates: eligible}
}

// Next returns the candidate the walk reaches at one-based step, and whether
// one exists there. False is the walk's own answer for two different truths —
// the walk never began and the walk ran out — and the caller distinguishes
// them by the step it asked for: zero means nothing was eligible, any other
// miss means every eligible candidate was tried and none served the request.
// The exhausted shape is the one that decides the request's ending, which is
// why the two are the same signal at this boundary and never merged again
// below it.
func (s Selection) Next(step int) (catalog.Candidate, bool) {
	if step < 1 || step > len(s.candidates) {
		return catalog.Candidate{}, false
	}
	return s.candidates[step-1], true
}

// Tried reports how many candidates the walk can try in total — the attempt
// budget this policy allows, one try per eligible candidate. There is no
// separate maximum-attempts knob on purpose: the budget is the eligible list,
// exactly, and an operator widens or narrows it by editing the alias's
// candidate list, where every other fact about the fallback order lives.
func (s Selection) Tried() int {
	return len(s.candidates)
}
