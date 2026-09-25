package routing

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
)

// The routing decision path's benchmark: eligibility, the walk, and the
// disposition judgment a failure earns, per alias size. The decision runs once
// per admitted request before any provider call, so its cost is pure overhead
// on the serving path — these numbers exist to keep it visible, not to chase.
// The candidateRow helper is the test table's; the benchmark varies only the
// alias's length, with every fourth backend disabled so the filter has real
// work in every size.
func BenchmarkRoutingDecisionPath(b *testing.B) {
	for _, size := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("candidates_%d", size), func(b *testing.B) {
			backends := make([]catalog.BackendID, size)
			for i := range backends {
				backends[i] = catalog.BackendID(strconv.Itoa(i))
			}
			rows := candidateRow(backends...)
			disabled := make(map[catalog.BackendID]bool, size)
			for i := 3; i < size; i += 4 {
				disabled[backends[i]] = true // every fourth backend is disabled
			}
			servable := func(id catalog.BackendID) bool { return !disabled[id] }
			callable := func(catalog.BackendID) bool { return true }

			b.ReportAllocs()
			for b.Loop() {
				eligible := Eligible(rows, servable, callable)
				walk := NewSelection(eligible)
				for step := 1; ; step++ {
					if _, ok := walk.Next(step); !ok {
						break
					}
				}
			}
		})
	}
}

// BenchmarkFailureDisposition costs the per-failure judgment: every class in
// execution's vocabulary through both disposition questions — whether the walk
// may fall through and whether the failure surfaces — the two calls the stage
// makes between one attempt's end and the next decision.
func BenchmarkFailureDisposition(b *testing.B) {
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
	b.ReportAllocs()
	for b.Loop() {
		for _, class := range classes {
			if FallbackEligible(class) {
				continue
			}
			SurfacedRefusal(class)
		}
	}
}
