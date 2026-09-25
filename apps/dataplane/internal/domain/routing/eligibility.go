package routing

import (
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
)

// Eligible filters an alias's candidates to the ones that may serve the
// request, preserving the alias's own order — the fallback order is the
// catalog's business, and this function's only authority over it is removal.
//
// A candidate is eligible when it stands on both legs, asked through the two
// functions rather than known here, because the catalog aggregates keep their
// states apart on purpose and the domain does not re-join them:
//
//   - servable answers for the candidate's backend: is it there, and is it
//     active? A disabled backend is skipped, selection treating it as if it
//     were not there (catalog.BackendDisable's own doctrine); a backend id no
//     row carries is skipped with it — the database's foreign key should make
//     that unreachable, and an unreachable-but-skipped candidate is the
//     fail-closed reading of a catalog that surprised us. A read that FAILED
//     is neither of those: the caller returns an error, because a store that
//     could not answer is not a catalog with nothing in it, and answering a
//     determinate refusal from one would write a permanent rejection over a
//     transient fault.
//   - callable answers whether an executor is registered for the backend. A
//     configured candidate with no driver wired is a deployment's gap, not a
//     client's error, and the walk cannot stop to report it: it skips the
//     candidate exactly as it skips a disabled one, which is why the two legs
//     are asked the same way.
//
// The result is the whole list the walk may try, in the order it may try
// them. An empty result is a determinate answer, not a failure: no candidate
// matched the request, and the caller ends the request the no-candidate way.
// An error, by contrast, means no decision was reached at all — the caller
// ends the request the internal-failure way, leaving the record open for the
// reaper, exactly as an admission that could not read its catalog does.
func Eligible(
	candidates []catalog.Candidate,
	servable func(catalog.BackendID) (bool, error),
	callable func(catalog.BackendID) (bool, error),
) ([]catalog.Candidate, error) {
	eligible := make([]catalog.Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		mayServe, err := servable(candidate.BackendID)
		if err != nil {
			return nil, fmt.Errorf("routing: candidate %d could not be asked whether its backend may serve: %w", candidate.Position, err)
		}
		mayCall, err := callable(candidate.BackendID)
		if err != nil {
			return nil, fmt.Errorf("routing: candidate %d could not be asked whether its backend may be called: %w", candidate.Position, err)
		}
		if mayServe && mayCall {
			eligible = append(eligible, candidate)
		}
	}
	return eligible, nil
}
