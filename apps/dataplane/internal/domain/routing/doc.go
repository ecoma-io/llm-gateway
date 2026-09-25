// Package routing is the domain of one decision: which eligible candidate is
// tried next, and what a failure earns. It is the policy half of the
// pipeline's second stage (routing.md ② and ⑤) — the half that must know the
// alias's candidate list and the attempt vocabulary, and must never know a
// provider, a wire shape or a database.
//
// The package is deliberately small, and its size is the design. Selection
// has exactly one policy — position order — because that is the only strategy
// with a consumer in this build; a second one waits for the demand that names
// it. Eligibility is the two legs a candidate must stand on — its backend is
// active, and an executor is registered for it — and the router sees only
// candidates that pass both, so a disabled backend and an unwired adapter are
// the same invisible thing to the walk. Disposition is the closed switch that
// turns a failure class into its consequence: fall through to the next
// candidate, or surface the refusal and stop. Nothing here keeps state, reads
// a clock, or touches a port; a routing decision over the same inputs is the
// same decision forever, which is what makes it testable as a table.
//
// What the package refuses to own is worth naming. It does not count
// attempts — the walk over the eligible list is the attempt budget. It does
// not track provider health — routing.md's policy-inputs section keeps
// weights, latencies and cooldowns out until something consumes them. And it
// does not know about commitment: a failure that arrives after the answer's
// commitment point never reaches disposition at all, because no policy can
// unsend a byte the client has already read.
package routing
