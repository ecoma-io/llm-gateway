package routing

import (
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
)

// The routing policy's own tests. The stage's whole authority is three
// judgment calls — what is eligible, what order the walk tries them in, and
// what a failure earns — so each is pinned as a table over its whole
// vocabulary, and the tables double as the exhaustive-switch guards: a class
// added to execution's vocabulary without an entry here fails these tests
// before it can fail a request.

// candidateRow builds a candidate list whose only variable is the backend id,
// so a table can say which rows survive eligibility without restating the
// rest of the shape.
func candidateRow(backends ...catalog.BackendID) []catalog.Candidate {
	candidates := make([]catalog.Candidate, len(backends))
	for i, id := range backends {
		candidates[i] = catalog.Candidate{
			ID:            catalog.CandidateID(string(id) + "-candidate"),
			BackendID:     id,
			ProviderModel: "model-" + string(id),
			Position:      i + 1,
		}
	}
	return candidates
}

// TestEligibleKeepsCatalogOrderAndDropsWhatCannotServe: the filter's only
// authority over the fallback order is removal — what survives comes out in
// the catalog's order, a disabled backend is skipped as if absent, a backend
// id no row carries is skipped with it (fail-closed), and a candidate whose
// driver is not wired is skipped the same way.
func TestEligibleKeepsCatalogOrderAndDropsWhatCannotServe(t *testing.T) {
	active := func(catalog.BackendID) bool { return true }
	callable := func(catalog.BackendID) bool { return true }

	t.Run("every candidate stands", func(t *testing.T) {
		in := candidateRow("a", "b", "c")
		got := Eligible(in, active, callable)
		if len(got) != 3 || got[0].BackendID != "a" || got[1].BackendID != "b" || got[2].BackendID != "c" {
			t.Fatalf("eligible = %v, want the catalog's own order preserved", got)
		}
	})

	t.Run("a disabled backend is skipped as if absent", func(t *testing.T) {
		in := candidateRow("a", "b", "c")
		servable := func(id catalog.BackendID) bool { return id != "b" }
		got := Eligible(in, servable, callable)
		if len(got) != 2 || got[0].BackendID != "a" || got[1].BackendID != "c" {
			t.Fatalf("eligible = %v, want b gone and the order kept", got)
		}
	})

	t.Run("a backend no row carries is skipped too", func(t *testing.T) {
		in := candidateRow("a", "ghost", "c")
		servable := func(id catalog.BackendID) bool { return id != "ghost" }
		got := Eligible(in, servable, callable)
		if len(got) != 2 || got[0].BackendID != "a" || got[1].BackendID != "c" {
			t.Fatalf("eligible = %v, want the unknown backend gone, fail-closed", got)
		}
	})

	t.Run("a candidate with no driver wired is skipped", func(t *testing.T) {
		in := candidateRow("a", "b", "c")
		callers := func(id catalog.BackendID) bool { return id != "c" }
		got := Eligible(in, active, callers)
		if len(got) != 2 || got[0].BackendID != "a" || got[1].BackendID != "b" {
			t.Fatalf("eligible = %v, want the unwired candidate gone", got)
		}
	})

	t.Run("nothing survives says so as an empty list", func(t *testing.T) {
		got := Eligible(candidateRow("a"), func(catalog.BackendID) bool { return false }, callable)
		if len(got) != 0 {
			t.Fatalf("eligible = %v, want empty", got)
		}
	})

	t.Run("no candidates at all is an empty list", func(t *testing.T) {
		got := Eligible(nil, active, callable)
		if len(got) != 0 {
			t.Fatalf("eligible = %v, want empty", got)
		}
	})
}

// TestSelectionTriesTheEligibleListInOrder: the walk is one try per eligible
// candidate, in the order eligibility preserved; the step numbering is
// one-based like the catalog's positions; and a miss answers false for two
// truths — never began and ran out — that the caller separates by the step it
// asked for.
func TestSelectionTriesTheEligibleListInOrder(t *testing.T) {
	t.Run("the steps come back in order", func(t *testing.T) {
		walk := NewSelection(candidateRow("a", "b", "c"))
		for step, want := range []catalog.BackendID{"a", "b", "c"} {
			candidate, ok := walk.Next(step + 1)
			if !ok || candidate.BackendID != want {
				t.Fatalf("step %d = %v/%v, want %s", step+1, candidate.BackendID, ok, want)
			}
		}
		if candidate, ok := walk.Next(4); ok {
			t.Fatalf("step 4 answered %s, want the walk exhausted", candidate.BackendID)
		}
	})

	t.Run("a walk that never began misses at step zero", func(t *testing.T) {
		walk := NewSelection(nil)
		if _, ok := walk.Next(1); ok {
			t.Fatalf("an empty walk answered a first step")
		}
		if _, ok := walk.Next(0); ok {
			t.Fatalf("step zero answered a candidate")
		}
		if walk.Tried() != 0 {
			t.Fatalf("an empty walk budgeted %d tries, want 0", walk.Tried())
		}
	})

	t.Run("the budget is the eligible list itself", func(t *testing.T) {
		for _, want := range []int{0, 1, 3, 7} {
			rows := make([]catalog.Candidate, want)
			for i := range rows {
				rows[i] = catalog.Candidate{ID: catalog.CandidateID(string(rune('a' + i)))}
			}
			if got := NewSelection(rows).Tried(); got != want {
				t.Fatalf("budget = %d, want %d", got, want)
			}
		}
	})
}

// TestFallbackEligibleTable: exactly the four faults another candidate might
// clear send the walk on; every other class in the vocabulary answers false —
// including the post-commitment stream class, which the walk's gate prevents
// reaching here and which must never read as a retry.
func TestFallbackEligibleTable(t *testing.T) {
	for _, tt := range []struct {
		class execution.ErrorClass
		want  bool
	}{
		{class: execution.ErrorRateLimited, want: true},
		{class: execution.ErrorProviderUnavailable, want: true},
		{class: execution.ErrorUpstreamError, want: true},
		{class: execution.ErrorInvalidUpstreamResponse, want: true},
		{class: execution.ErrorAuthentication},
		{class: execution.ErrorProviderRejectedRequest},
		{class: execution.ErrorContextTooLarge},
		{class: execution.ErrorStreamAfterCommitment},
	} {
		t.Run(string(tt.class), func(t *testing.T) {
			if got := FallbackEligible(tt.class); got != tt.want {
				t.Fatalf("FallbackEligible(%s) = %v, want %v", tt.class, got, tt.want)
			}
		})
	}
}

// TestSurfacedRefusalTable: exactly the three request-fault refusals surface,
// each keeping its own failure reason — the name a replay re-derives the
// original answer from — and no other class produces one.
func TestSurfacedRefusalTable(t *testing.T) {
	for _, tt := range []struct {
		class  execution.ErrorClass
		want   bool
		reason execution.FailureReason
	}{
		{class: execution.ErrorProviderRejectedRequest, want: true, reason: execution.FailedProviderRejectedRequest},
		{class: execution.ErrorContextTooLarge, want: true, reason: execution.FailedContextTooLarge},
		{class: execution.ErrorAuthentication, want: true, reason: execution.FailedUpstreamAuthentication},
		{class: execution.ErrorRateLimited},
		{class: execution.ErrorProviderUnavailable},
		{class: execution.ErrorUpstreamError},
		{class: execution.ErrorInvalidUpstreamResponse},
		{class: execution.ErrorStreamAfterCommitment},
	} {
		t.Run(string(tt.class), func(t *testing.T) {
			reason, got := SurfacedRefusal(tt.class)
			if got != tt.want || reason != tt.reason {
				t.Fatalf("SurfacedRefusal(%s) = %q/%v, want %q/%v", tt.class, reason, got, tt.reason, tt.want)
			}
		})
	}
}

// TestEveryVocabularyClassIsDispositionedOrPrevented is the guard the
// exhaustive switches cannot give themselves: the test walks execution's
// whole class vocabulary and fails if a class appears that neither falls
// through, nor surfaces, nor is the one class the walk prevents from ever
// reaching this switch — a stream failure after commitment is decided by the
// commitment gate above the disposition, never by it. A new class added
// without a disposition is a decision, and this test is where refusing to
// make it silently happens.
func TestEveryVocabularyClassIsDispositionedOrPrevented(t *testing.T) {
	classes := []execution.ErrorClass{
		execution.ErrorAuthentication,
		execution.ErrorRateLimited,
		execution.ErrorProviderUnavailable,
		execution.ErrorProviderRejectedRequest,
		execution.ErrorContextTooLarge,
		execution.ErrorInvalidUpstreamResponse,
		execution.ErrorUpstreamError,
		execution.ErrorStreamAfterCommitment,
	}
	for _, class := range classes {
		_, surfaced := SurfacedRefusal(class)
		if FallbackEligible(class) || surfaced {
			continue
		}
		if class == execution.ErrorStreamAfterCommitment {
			continue // prevented, not dispositioned: the commitment gate owns it
		}
		t.Fatalf("class %q has no disposition: it neither falls through nor surfaces", class)
	}
}
