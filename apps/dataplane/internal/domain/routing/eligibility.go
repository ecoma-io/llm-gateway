package routing

import (
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
//     fail-closed reading of a catalog that surprised us.
//   - callable answers whether an executor is registered for the backend. A
//     configured candidate with no driver wired is a deployment's gap, not a
//     client's error, and the walk cannot stop to report it: it skips the
//     candidate exactly as it skips a disabled one, which is why the two legs
//     are asked the same way.
//
// The result is the whole list the walk may try, in the order it may try
// them. An empty result is a determinate answer, not a failure: no candidate
// matched the request, and the caller ends the request the no-candidate way.
func Eligible(
	candidates []catalog.Candidate,
	servable func(catalog.BackendID) bool,
	callable func(catalog.BackendID) bool,
) []catalog.Candidate {
	eligible := make([]catalog.Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if servable(candidate.BackendID) && callable(candidate.BackendID) {
			eligible = append(eligible, candidate)
		}
	}
	return eligible
}
