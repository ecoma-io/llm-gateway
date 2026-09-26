package application

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/executors"
)

// The routing tests. The stage under test is ChatRouting wrapped around the
// same admission fixture the admission tests use, so the world — and the
// event log that reads as the pipeline's whole story — is one world per test.
// The scenarios pin the walk: what is eligible, what is tried in which order,
// what each failure class earns, and that every ending is one unit of work
// whose fact is the last word.

// routeFixture builds the routing stage over the smallest world that admits:
// no candidates on the alias and an empty executor registry — the walk has
// nothing to try, so every admission ends released as no_candidate. The
// scenarios that exercise the walk hand it candidates, backends and
// executors through the same seams this fixture pokes. The input
// comes with the fixture because a routing Serve without its reply is a
// wiring defect the stage refuses.
func routeFixture(t *testing.T) (*ChatRouting, *admissionWorld, *fakeReply, ChatInput) {
	t.Helper()
	return routingFixture(t)
}

// routingFixture is routeFixture with the input handed back, for the tests
// that shape the walk: candidates, backends, executors and stream preference
// are the scenario's own configuration.
func routingFixture(t *testing.T) (*ChatRouting, *admissionWorld, *fakeReply, ChatInput) {
	t.Helper()
	world := newAdmissionWorld()
	world.seedCredential(admissionKeyID, admissionAccount,
		execution.SecretDigest(admissionSecret), "active", statePtr("active"))
	alias := world.seedAlias("test-model", 4096, 1000)
	world.seedPrice(alias.ID, catalog.PriceSnapshot{RevisionID: "rev-1", InputUnitPrice: 1_000_000, OutputUnitPrice: 2_000_000})
	world.seedBucket(admissionAccount, "bucket-1", 10_000, true)
	reply := &fakeReply{}
	in := admissionInput(admissionBody("test-model"))
	in.Reply = reply
	return newRouting(world, map[catalog.BackendID]executors.Executor{}), world, reply, in
}

// newRouting wires the routing stage over one world. The clock is the same
// seam the admission tests use; the registry's entries are the scenario's.
// The execution budget is chosen for hermeticity: the immediate renewal —
// the one the walk always starts — is answered by the fake reservations, and
// the renewal ticker, a third of the lease, never fires inside a test's
// lifetime. The renewal tests build their own clock over the same seams.
func newRouting(world *admissionWorld, entries map[catalog.BackendID]executors.Executor) *ChatRouting {
	routing := NewChatRouting(
		fakeAdmissionStore{world: world},
		fakeAdmissionAliases{world: world},
		fakeRoutingBackends{world: world},
		fakeAdmissionRequests{world: world},
		fakeAdmissionAttempts{world: world},
		fakeAdmissionIntakes{world: world},
		fakeAdmissionReservations{world: world},
		fakeAdmissionLedger{world: world},
		fakeAdmissionFacts{world: world},
		executors.NewRegistry(entries),
		ExecutionConfig{MaxDuration: 30 * time.Second, LeaseTTL: time.Hour},
		newAdmission(world),
	)
	routing.clock = admissionClock{world}
	return routing
}

// twoCandidateWorld turns the fixture alias into a two-candidate walk over
// two active backends, both registered.
func twoCandidateWorld(world *admissionWorld) (first, second *fakeExecutor) {
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
		catalog.Candidate{ID: "cand-b", BackendID: "backend-b", ProviderModel: "model-b", Position: 2},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	world.seedBackend("backend-b", catalog.BackendActive)
	first = &fakeExecutor{}
	second = &fakeExecutor{}
	return first, second
}

// ---------------------------------------------------------------------------
// the no-candidate release — today's whole runtime
// ---------------------------------------------------------------------------

// TestRoutingReleasesAnUnroutableAdmissionAsNoCandidate: an admission whose
// alias offers nothing eligible — here an empty registry, which is exactly
// what a build before B10 wires — ends as no_candidate in one release unit.
// The event sequence IS the assertion: admission's unit, then the ending's,
// with the released fact its last word. This is also the shape that keeps the
// endpoint's observable behavior what it was before the routing stage
// existed.
func TestRoutingReleasesAnUnroutableAdmissionAsNoCandidate(t *testing.T) {
	routing, world, reply, in := routeFixture(t)

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeRejected || outcome.Reason != execution.RejectedNoCandidate {
		t.Fatalf("outcome = %s/%s, want rejected/no_candidate", outcome.Kind, outcome.Reason)
	}
	if outcome.RuntimeRequestID != in.RequestID {
		t.Fatalf("the outcome's runtime identity = %q, want the arrival's own id — the first attempt answers for it", outcome.RuntimeRequestID)
	}
	if outcome.Routing == nil || outcome.Routing.Alias != "test-model" || outcome.Routing.Attempts != 0 {
		t.Fatalf("the routing trace does not read as an empty walk: %+v", outcome.Routing)
	}

	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
		"begin", "reservation.close", "ledger.return", "request.finalise", "intake.finalise", "fact.append", "commit",
	})

	// The hold: released whole, back in the legs' stored order, the grant
	// left exactly as it began.
	reservation := admissionReservationRow(t, world)
	if reservation.State != accounting.StateReleased {
		t.Fatalf("the hold is %s, want released", reservation.State)
	}
	if len(world.returned) != 1 || len(world.returned[0]) != 1 || world.returned[0][0].Ordinal != 1 {
		t.Fatalf("the release returned %v, want the reservation's own legs in ordinal order", world.returned)
	}
	if got := world.available("bucket-1"); got != 10_000 {
		t.Fatalf("the grant holds %d after the release, want 10000", got)
	}

	// The request row: rejected no_candidate, no attempt named.
	row := admissionRequestRow(t, world)
	if row.Status != execution.StatusRejected || row.RejectionReason != execution.RejectedNoCandidate {
		t.Fatalf("the request ended %s/%s, want rejected/no_candidate", row.Status, row.RejectionReason)
	}

	// The replay record: terminal, pointing at the rejection a replay
	// re-answers.
	intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1")
	if intake.FinalStatus == nil || *intake.FinalStatus != execution.FinalRejected ||
		intake.FinalRejectionReason != execution.RejectedNoCandidate {
		t.Fatalf("the replay record is not terminal no_candidate")
	}

	// The fact: one release, appended last, naming the request it closed.
	if len(world.facts) != 1 || world.facts[0].Kind != accounting.KindReleased || world.facts[0].RequestID != row.ID {
		t.Fatalf("the ending's fact is %v, want one released fact for the request", world.facts)
	}

	// The reply: the no-candidate cell and nothing else — no framing was
	// opened, because no byte was ever going to travel.
	if len(reply.served) != 1 || reply.served[0] != "no_candidate" || reply.opened {
		t.Fatalf("the reply served %v (opened=%v), want only the no-candidate cell", reply.served, reply.opened)
	}
}

// ---------------------------------------------------------------------------
// the walk
// ---------------------------------------------------------------------------

// TestRoutingFallsThroughToTheNextCandidateAndSettles: a rate-limited first
// candidate falls through — its failure observed on its own row — and the
// second candidate's answer settles the request: reported usage priced by the
// hold formula, the delivery the gateway counted, the settled fact last.
func TestRoutingFallsThroughToTheNextCandidateAndSettles(t *testing.T) {
	routing, world, reply, in := routingFixture(t)
	first, second := twoCandidateWorld(world)
	first.results = []executors.Result{failExec(execution.ErrorRateLimited)}
	second.act = func(sink executors.Sink) { _ = sink.Content([]byte(`{"answer":true}`)) }
	second.results = []executors.Result{okExec(101, 55)}
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{
		"backend-a": first,
		"backend-b": second,
	})

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeServed {
		t.Fatalf("outcome = %s, want served", outcome.Kind)
	}
	if outcome.Routing.Attempts != 2 || outcome.Routing.LastPosition != 2 ||
		outcome.Routing.LastErrorClass != execution.ErrorRateLimited || !outcome.Routing.Committed {
		t.Fatalf("the routing trace does not read as a two-step walk: %+v", outcome.Routing)
	}

	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
		"attempt.insert",
		"begin", "reservation.close", "attempt.insert", "request.finalise", "intake.finalise", "fact.append", "commit",
	})

	// Two attempt rows: the fall-through's observation, then the committed
	// one — each on its own candidate, in catalog positions counted from
	// zero.
	if len(world.attempts) != 2 {
		t.Fatalf("the walk appended %d attempts, want 2", len(world.attempts))
	}
	fallen, committed := world.attempts[0], world.attempts[1]
	if fallen.Outcome != execution.OutcomeFailedBeforeCommitment || fallen.ErrorClass != execution.ErrorRateLimited ||
		fallen.BackendID != "backend-a" || fallen.CandidatePosition != 0 {
		t.Fatalf("the first attempt reads %s/%s on %s at %d, want the rate-limited observation on backend-a at 0",
			fallen.Outcome, fallen.ErrorClass, fallen.BackendID, fallen.CandidatePosition)
	}
	if committed.Outcome != execution.OutcomeSucceeded || committed.ErrorClass != "" ||
		committed.BackendID != "backend-b" || committed.CandidatePosition != 1 {
		t.Fatalf("the committed attempt reads %s/%s on %s at %d, want a clean success on backend-b at 1",
			committed.Outcome, committed.ErrorClass, committed.BackendID, committed.CandidatePosition)
	}

	// The request settled on the committed attempt; the hold became cost, so
	// nothing went back to the grant.
	row := admissionRequestRow(t, world)
	if row.Status != execution.StatusSucceeded || row.CommittedAttemptID != committed.ID {
		t.Fatalf("the request ended %s naming %s, want succeeded on the committed attempt", row.Status, row.CommittedAttemptID)
	}
	if reservation := admissionReservationRow(t, world); reservation.State != accounting.StateSettled {
		t.Fatalf("the hold is %s, want settled", reservation.State)
	}
	if len(world.returned) != 0 {
		t.Fatalf("the settlement returned %d legs, want none — a settle keeps its drawdown", len(world.returned))
	}
	if got := world.available("bucket-1"); got != 10_000-103 {
		t.Fatalf("the grant holds %d, want 9897 — the hold became cost", got)
	}

	// The fact: the provider reported 101 input and 55 output — both beyond
	// the counts the hold was derived from (71 input, 16 output basis) — so
	// both figures clamp to the reservation's own and the capture attests the
	// floor: 71·1 + 16·2 = 103, the hold to the token. The provider's claims
	// are not erased by the clamping; they stand on the attempt row as the
	// telemetry they are.
	fact := world.facts[len(world.facts)-1]
	if fact.Kind != accounting.KindSettled || fact.CaptureMethod != accounting.CaptureReservationFloor ||
		fact.CommittedAttemptID != committed.ID {
		t.Fatalf("the settled fact reads %s/%s on %s, want a reservation-floor settlement on the committed attempt",
			fact.Kind, fact.CaptureMethod, fact.CommittedAttemptID)
	}
	if fact.ProviderInputTokens == nil || *fact.ProviderInputTokens != 71 ||
		fact.ProviderOutputTokens == nil || *fact.ProviderOutputTokens != 16 {
		t.Fatalf("the settled fact's provider counts drifted: %v/%v, want the hold's own counts (71, 16)",
			fact.ProviderInputTokens, fact.ProviderOutputTokens)
	}
	if fact.DeliveryTokens == nil || *fact.DeliveryTokens != 15 {
		t.Fatalf("the settled fact's delivery = %v, want the 15 bytes the sink recorded", fact.DeliveryTokens)
	}
	if fact.SettledAmount == nil || *fact.SettledAmount != 103 {
		t.Fatalf("the settled amount = %v, want 103 — the hold, which is what the clamped figures re-derive", fact.SettledAmount)
	}

	// The intake closed succeeded; the reply closed the answer it delivered.
	if intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1"); intake.FinalStatus == nil ||
		*intake.FinalStatus != execution.FinalSucceeded {
		t.Fatalf("the replay record did not close succeeded")
	}
	if len(reply.served) != 1 || reply.served[0] != "succeeded" || !reply.committed {
		t.Fatalf("the reply served %v, want the delivered answer closed", reply.served)
	}
}

// TestRoutingSurfacesARefusalAndStopsTheWalk: a refusal the caller's own
// request caused is not a retry — the walk stops on the first candidate, the
// hold is released, the request and the replay record carry the refusal, and
// the reply serves the refusal's own cell.
func TestRoutingSurfacesARefusalAndStopsTheWalk(t *testing.T) {
	routing, world, reply, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
		catalog.Candidate{ID: "cand-b", BackendID: "backend-b", ProviderModel: "model-b", Position: 2},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	world.seedBackend("backend-b", catalog.BackendActive)
	first := &fakeExecutor{results: []executors.Result{failExec(execution.ErrorContextTooLarge)}}
	second := &fakeExecutor{}
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{
		"backend-a": first,
		"backend-b": second,
	})

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeRefused || outcome.Failure != execution.FailedContextTooLarge {
		t.Fatalf("outcome = %s/%s, want refused/context_too_large", outcome.Kind, outcome.Failure)
	}
	if outcome.Routing.Attempts != 1 || second.calls != 0 {
		t.Fatalf("the walk tried %d candidates (second called %d times), want it stopped at the first",
			outcome.Routing.Attempts, second.calls)
	}

	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
		"attempt.insert",
		"begin", "reservation.close", "ledger.return", "request.finalise", "intake.finalise", "fact.append", "commit",
	})

	// The refusal's observation stands on its own row; the ending released
	// the hold unused.
	if len(world.attempts) != 1 || world.attempts[0].ErrorClass != execution.ErrorContextTooLarge {
		t.Fatalf("the walk appended %v, want one context_too_large observation", world.attempts)
	}
	row := admissionRequestRow(t, world)
	if row.Status != execution.StatusFailed || row.FailureReason != execution.FailedContextTooLarge ||
		row.CommittedAttemptID != "" {
		t.Fatalf("the request ended %s/%s naming %s, want failed before commitment, no attempt",
			row.Status, row.FailureReason, row.CommittedAttemptID)
	}
	if got := world.available("bucket-1"); got != 10_000 {
		t.Fatalf("the grant holds %d, want 10000 — nothing was owed", got)
	}
	intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1")
	if intake.FinalStatus == nil || *intake.FinalStatus != execution.FinalFailed ||
		intake.FinalFailureReason != execution.FailedContextTooLarge {
		t.Fatalf("the replay record does not carry the refusal a replay re-answers")
	}
	if len(world.facts) != 1 || world.facts[0].Kind != accounting.KindReleased {
		t.Fatalf("the ending's fact is %v, want one released fact", world.facts)
	}
	if reply.surfaced != execution.FailedContextTooLarge {
		t.Fatalf("the reply served %v, want the refusal's own cell", reply.served)
	}
}

// TestRoutingExhaustsTheWalkAsNoCandidateSucceeded: every eligible candidate
// fell through and none served the request — the answer names the walk, the
// hold is released, and the exhaustion is a determinate answer, not an error.
func TestRoutingExhaustsTheWalkAsNoCandidateSucceeded(t *testing.T) {
	routing, world, reply, in := routingFixture(t)
	first, second := twoCandidateWorld(world)
	first.results = []executors.Result{failExec(execution.ErrorRateLimited)}
	second.results = []executors.Result{failExec(execution.ErrorProviderUnavailable)}
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{
		"backend-a": first,
		"backend-b": second,
	})

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeRejected || outcome.Reason != execution.RejectedNoCandidateSucceeded {
		t.Fatalf("outcome = %s/%s, want rejected/no_candidate_succeeded", outcome.Kind, outcome.Reason)
	}
	if outcome.Routing.Attempts != 2 || outcome.Routing.LastErrorClass != execution.ErrorProviderUnavailable {
		t.Fatalf("the routing trace does not read as an exhausted walk: %+v", outcome.Routing)
	}

	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
		"attempt.insert", "attempt.insert",
		"begin", "reservation.close", "ledger.return", "request.finalise", "intake.finalise", "fact.append", "commit",
	})
	row := admissionRequestRow(t, world)
	if row.Status != execution.StatusRejected || row.RejectionReason != execution.RejectedNoCandidateSucceeded {
		t.Fatalf("the request ended %s/%s, want rejected/no_candidate_succeeded", row.Status, row.RejectionReason)
	}
	if got := world.available("bucket-1"); got != 10_000 {
		t.Fatalf("the grant holds %d, want 10000 — the exhausted walk kept nothing", got)
	}
	if len(reply.served) != 1 || reply.served[0] != "no_candidate" {
		t.Fatalf("the reply served %v, want the no-candidate cell", reply.served)
	}
}

// TestRoutingSettlesAMidStreamFailure: a stream that broke after its
// commitment point has no next candidate and no fallback — the usage in
// flight settles on the attempt the commitment happened on, the capture is
// the gateway's own where the provider said nothing, and the failure frame is
// the reply's to write.
func TestRoutingSettlesAMidStreamFailure(t *testing.T) {
	routing, world, reply, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	output := int64(77)
	exec := &fakeExecutor{
		act: func(sink executors.Sink) { _ = sink.Content([]byte("data: partial")) },
		results: []executors.Result{executors.Failure{
			Class:             execution.ErrorStreamAfterCommitment,
			ProviderRequestID: "prov-req-9",
			Usage:             executors.Usage{OutputTokens: &output},
		}},
	}
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{"backend-a": exec})

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeServed || !outcome.Routing.Committed {
		t.Fatalf("outcome = %s committed=%v, want served of a committed answer", outcome.Kind, outcome.Routing.Committed)
	}

	// The committed attempt is appended inside the settle unit — no
	// standalone write, no transaction open across the call.
	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
		"begin", "reservation.close", "attempt.insert", "request.finalise", "intake.finalise", "fact.append", "commit",
	})

	if len(world.attempts) != 1 {
		t.Fatalf("the walk appended %d attempts, want the one commitment happened on", len(world.attempts))
	}
	attempt := world.attempts[0]
	if attempt.Outcome != execution.OutcomeFailedAfterCommitment ||
		attempt.ErrorClass != execution.ErrorStreamAfterCommitment ||
		attempt.ProviderRequestID != "prov-req-9" {
		t.Fatalf("the committed attempt reads %s/%s (%s), want the stream failure on the provider's own handle",
			attempt.Outcome, attempt.ErrorClass, attempt.ProviderRequestID)
	}

	row := admissionRequestRow(t, world)
	if row.Status != execution.StatusFailed || row.FailureReason != execution.FailedStreamAfterCommitment ||
		row.CommittedAttemptID != attempt.ID {
		t.Fatalf("the request ended %s/%s naming %s, want the stream failure on the committed attempt",
			row.Status, row.FailureReason, row.CommittedAttemptID)
	}

	// The provider reported 77 output — about a stream it never finished —
	// and that report is the attempt row's telemetry, never the bill. The
	// delivery boundary prices the stream's output at the 13 delivered bytes
	// and nothing else; no input figure arrived, so input is the count
	// admission priced the hold from. Capture gateway_observed — the priced
	// pair is the gateway's own — and the amount re-derives:
	// 71·1 + 13·2 = 97, under the 103 hold. The 13 delivered bytes ride
	// beside the settlement as the delivery figure, recorded and never priced.
	fact := world.facts[len(world.facts)-1]
	if fact.Kind != accounting.KindSettled || fact.CaptureMethod != accounting.CaptureGatewayObserved {
		t.Fatalf("the settled fact reads %s/%s, want a gateway-observed settlement", fact.Kind, fact.CaptureMethod)
	}
	if fact.ProviderInputTokens == nil || *fact.ProviderInputTokens != 71 {
		t.Fatalf("the settled fact's input = %v, want the count admission priced the hold from", fact.ProviderInputTokens)
	}
	if fact.ProviderOutputTokens == nil || *fact.ProviderOutputTokens != 13 {
		t.Fatalf("the settled fact's output = %v, want the 13 delivered bytes — a failed stream bills what reached the client, never the basis", fact.ProviderOutputTokens)
	}
	if fact.DeliveryTokens == nil || *fact.DeliveryTokens != 13 {
		t.Fatalf("the settled fact's delivery = %v, want the 13 bytes that left", fact.DeliveryTokens)
	}
	if fact.SettledAmount == nil || *fact.SettledAmount != 97 {
		t.Fatalf("the settled amount = %v, want 97 — what the delivered pair re-derives, under the 103 hold", fact.SettledAmount)
	}

	if intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1"); intake.FinalStatus == nil ||
		*intake.FinalStatus != execution.FinalFailed ||
		intake.FinalFailureReason != execution.FailedStreamAfterCommitment {
		t.Fatalf("the replay record does not carry the stream failure")
	}
	if len(reply.served) != 1 || reply.served[0] != "mid_stream_failure" {
		t.Fatalf("the reply served %v, want the mid-stream failure frames", reply.served)
	}
}

// TestRoutingReleasesTheHoldWhenTheCallerLeaves: a caller gone between
// attempts stops the walk. Nothing was committed, so there is no settlement;
// but the hold the request opened is real reserved capacity, and the ending
// that returns it has a channel of its own — the walk releases it detached
// from the dead caller's context, naming gateway_abandoned on the request
// row and the replay record, the same word the reaper uses for the process
// it cannot ask. The outcome kind stays abandoned: the transport writes
// nothing either way.
func TestRoutingReleasesTheHoldWhenTheCallerLeaves(t *testing.T) {
	routing, world, reply, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	ctx, cancel := context.WithCancel(context.Background())
	exec := &fakeExecutor{
		act:     func(executors.Sink) { cancel() },
		results: []executors.Result{failExec(execution.ErrorRateLimited)},
	}
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{"backend-a": exec})

	outcome, err := routing.Serve(ctx, in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeAbandoned {
		t.Fatalf("outcome = %s, want abandoned", outcome.Kind)
	}

	// The abandoned ending is a whole release unit: the hold returned, the
	// request finalised through the abandoned door, the replay record
	// failed with it, and the released fact the feed's last word. The one
	// observation of the call that happened stands before it.
	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
		"attempt.insert",
		"begin", "reservation.close", "ledger.return", "request.finalise", "intake.finalise", "fact.append", "commit",
	})
	if reservation := admissionReservationRow(t, world); reservation.State != accounting.StateReleased {
		t.Fatalf("the hold is %s, want released — the walk that saw the caller leave returns it", reservation.State)
	}
	row := admissionRequestRow(t, world)
	if row.Status != execution.StatusFailed || row.FailureReason != execution.FailedGatewayAbandoned {
		t.Fatalf("the request is %s/%s, want failed/gateway_abandoned", row.Status, row.FailureReason)
	}
	if intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1"); intake.FinalStatus == nil ||
		*intake.FinalStatus != execution.FinalFailed ||
		intake.FinalFailureReason != execution.FailedGatewayAbandoned {
		t.Fatalf("the replay record does not carry the abandonment")
	}
	if len(world.facts) != 1 {
		t.Fatalf("the abandoned walk appended %d facts, want the one release", len(world.facts))
	}
	if got := world.available("bucket-1"); got != 10_000 {
		t.Fatalf("the grant holds %d, want 10000 — the release returned the hold whole", got)
	}
	if len(reply.served) != 0 {
		t.Fatalf("the reply served %v, want nothing — there is no channel left", reply.served)
	}
}

// TestRoutingAnswersTheNoCandidateCellWhenTheLeaseIsLost: the other door to
// the abandoned ending — the renewal reported the hold reaped mid-call, the
// call was cancelled before anything was committed, and the caller's
// connection may be perfectly alive. The lease died, not the request, so
// the walk answers through the reply with the no-candidate cell — the
// runtime cannot serve this request right now, the one reading a client can
// act on — and then states the abandoned ending: the release whose CAS may
// lose to whoever closed the hold, the request row's gateway_abandoned, and
// the outcome that stays the log's fact.
func TestRoutingAnswersTheNoCandidateCellWhenTheLeaseIsLost(t *testing.T) {
	routing, world, reply, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	world.renewalGone = 1
	cfg := ExecutionConfig{MaxDuration: 30 * time.Second, LeaseTTL: 25 * time.Millisecond}
	routing.execution = cfg
	release := make(chan struct{}) // never closed: only the lost renewal can end this call
	exec := &renewWatchingExecutor{release: release}
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{"backend-a": exec})

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeAbandoned {
		t.Fatalf("outcome = %s, want abandoned — the hold is gone from the open set", outcome.Kind)
	}
	if len(reply.served) != 1 || reply.served[0] != "no_candidate" {
		t.Fatalf("the reply served %v, want the one no-candidate cell — the caller was never told its lease died", reply.served)
	}

	// The ending is the whole abandoned release, exactly as the caller-gone
	// door states it: on this fake world the hold is still open when the
	// release's CAS runs, so the release wins it back — in production the
	// sweep or a concurrent settlement has already closed the hold and the
	// CAS loses, which changes nothing the caller reads.
	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
		"attempt.insert",
		"begin", "reservation.close", "ledger.return", "request.finalise", "intake.finalise", "fact.append", "commit",
	})
	if reservation := admissionReservationRow(t, world); reservation.State != accounting.StateReleased {
		t.Fatalf("the hold is %s, want released — the release unit claimed the ending this world left open", reservation.State)
	}
	row := admissionRequestRow(t, world)
	if row.Status != execution.StatusFailed || row.FailureReason != execution.FailedGatewayAbandoned {
		t.Fatalf("the request is %s/%s, want failed/gateway_abandoned", row.Status, row.FailureReason)
	}
}

// TestRoutingSettlesAnEndingTheCallerDidNotStayFor: a client that leaves
// after the commitment does not take the settlement with them — the bytes
// that arrived were delivered, and the ending that records them runs
// detached from the connection that died. The failure arrives committed, the
// walk settles it on a context the caller's cancellation cannot reach, and
// the settled fact lands exactly as it does for a caller who stayed. This is
// the test the fake store's own BeginTx refusal gives meaning to: on the
// caller's context the unit would not even begin.
func TestRoutingSettlesAnEndingTheCallerDidNotStayFor(t *testing.T) {
	routing, world, reply, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	ctx, cancel := context.WithCancel(context.Background())
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{
		"backend-a": &fakeExecutor{
			act: func(sink executors.Sink) {
				_ = sink.Content([]byte(`{"delta":"par`)) // the commitment, then the client goes
				cancel()
			},
			results: []executors.Result{failExec(execution.ErrorUpstreamError)},
		},
	})
	defer cancel()

	outcome, err := routing.Serve(ctx, in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeServed || !outcome.Routing.Committed {
		t.Fatalf("outcome = %s committed=%v, want served of a committed answer", outcome.Kind, outcome.Routing.Committed)
	}
	if outcome.Routing.LastErrorClass != execution.ErrorStreamAfterCommitment {
		t.Fatalf("the committed failure read %s, want the stream's own class", outcome.Routing.LastErrorClass)
	}

	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
		"begin", "reservation.close", "attempt.insert", "request.finalise", "intake.finalise", "fact.append", "commit",
	})
	if reservation := admissionReservationRow(t, world); reservation.State != accounting.StateSettled {
		t.Fatalf("the hold is %s, want settled — the ending outlived the connection", reservation.State)
	}
	fact := world.facts[len(world.facts)-1]
	if fact.Kind != accounting.KindSettled {
		t.Fatalf("the feed's last word is %s, want settled", fact.Kind)
	}
	if intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1"); intake.FinalStatus == nil ||
		*intake.FinalStatus != execution.FinalFailed {
		t.Fatalf("the replay record is not terminal failed")
	}
	if len(reply.served) != 1 || reply.served[0] != "mid_stream_failure" {
		t.Fatalf("the reply served %v, want the mid-stream failure frames", reply.served)
	}
}

// TestRoutingClassifiesABrokenExecutorContract: a nil result is the
// executor's contract broken, not a verdict about any provider — classified
// as an answer nobody could read, and walked like one.
func TestRoutingClassifiesABrokenExecutorContract(t *testing.T) {
	routing, world, reply, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{
		"backend-a": &fakeExecutor{}, // an empty script answers nil
	})

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeRejected || outcome.Reason != execution.RejectedNoCandidateSucceeded {
		t.Fatalf("outcome = %s/%s, want the walk exhausted as no_candidate_succeeded", outcome.Kind, outcome.Reason)
	}
	if outcome.Routing.LastErrorClass != execution.ErrorInvalidUpstreamResponse {
		t.Fatalf("the last failure read %s, want invalid_upstream_response", outcome.Routing.LastErrorClass)
	}
	if len(world.attempts) != 1 || world.attempts[0].ErrorClass != execution.ErrorInvalidUpstreamResponse {
		t.Fatalf("the observation reads %v, want one invalid_upstream_response row", world.attempts)
	}
	if len(reply.served) != 1 || reply.served[0] != "no_candidate" {
		t.Fatalf("the reply served %v, want the no-candidate cell", reply.served)
	}
}

// TestRoutingDoesNotSettleASuccessTheSinkNeverCommitted: an executor's
// Success claim is a settlement only when the sink's commitment stands behind
// it — the client received nothing otherwise, and billing for bytes that
// never crossed is the one lie a settlement must never tell. The claim is
// re-classed as an answer nobody could read and walked like one; the hold
// goes back released, never settled.
func TestRoutingDoesNotSettleASuccessTheSinkNeverCommitted(t *testing.T) {
	routing, world, reply, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{
		"backend-a": &fakeExecutor{results: []executors.Result{okExec(50, 70)}}, // Success, sink untouched
	})

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeRejected || outcome.Reason != execution.RejectedNoCandidateSucceeded {
		t.Fatalf("outcome = %s/%s, want the walk exhausted as no_candidate_succeeded", outcome.Kind, outcome.Reason)
	}
	if outcome.Routing.LastErrorClass != execution.ErrorInvalidUpstreamResponse {
		t.Fatalf("the uncommitted success read %s, want invalid_upstream_response", outcome.Routing.LastErrorClass)
	}
	if reservation := admissionReservationRow(t, world); reservation.State != accounting.StateReleased {
		t.Fatalf("the hold is %s, want released — nothing was delivered to settle", reservation.State)
	}
	if len(world.facts) != 1 || world.facts[0].Kind != accounting.KindReleased {
		t.Fatalf("the feed holds %v, want one released fact and no settlement", world.facts)
	}
	if len(reply.served) != 1 || reply.served[0] != "no_candidate" {
		t.Fatalf("the reply served %v, want the no-candidate cell", reply.served)
	}
}

// TestRoutingFailsTheRequestWhenTheCatalogCannotAnswer: a backend read that
// FAILED is not a catalog with nothing in it — answering it as no_candidate
// would write a permanent rejection over a transient fault. The honest
// ending is the error to the caller, and the walk still states its own
// ending on the way out: the hold it holds is released as gateway_abandoned,
// so no client reservation waits on the reaper for a fault the walk watched
// happen. The caller's error and the gateway's ending are different
// sentences about the same fault, and both get said.
func TestRoutingFailsTheRequestWhenTheCatalogCannotAnswer(t *testing.T) {
	routing, world, _, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{
		"backend-a": &fakeExecutor{act: func(sink executors.Sink) { _ = sink.Content([]byte(`{"answer":true}`)) }},
	})
	routing.backends = fakeRoutingBackends{
		world:       world,
		readFailure: fmt.Errorf("fake: the catalog read timed out"),
	}

	_, err := routing.Serve(context.Background(), in)
	if err == nil {
		t.Fatalf("Serve answered from a catalog it could not read, want the error")
	}
	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
		"begin", "reservation.close", "ledger.return", "request.finalise", "intake.finalise", "fact.append", "commit",
	})
	if reservation := admissionReservationRow(t, world); reservation.State != accounting.StateReleased {
		t.Fatalf("the hold is %s, want released — the abandoned walk gave the hold back", reservation.State)
	}
	if row := admissionRequestRow(t, world); row.Status != execution.StatusFailed {
		t.Fatalf("the request is %s, want failed — the walk's ending is stated even though the call never happened", row.Status)
	}
	if len(world.facts) != 1 || world.facts[0].Kind != accounting.KindReleased {
		t.Fatalf("the feed holds %v, want the one released fact the abandoned walk stated", world.facts)
	}
}

// TestRoutingDispositionTable walks every class in the vocabulary through a
// one-candidate walk and pins what each earns: the four ineligible-for-fallback
// faults fall through to exhaustion, the three surfaced refusals stop the
// walk as answers, and a stream-failure claim over an uncommitted answer is
// reclassified — the sink owns the commitment fact, not the executor.
func TestRoutingDispositionTable(t *testing.T) {
	for _, tt := range []struct {
		class         execution.ErrorClass
		wantKind      OutcomeKind
		wantReason    execution.RejectionReason
		wantFailure   execution.FailureReason
		wantRowClass  execution.ErrorClass
		wantGrantBack bool
	}{
		{class: execution.ErrorRateLimited, wantKind: OutcomeRejected, wantReason: execution.RejectedNoCandidateSucceeded, wantRowClass: execution.ErrorRateLimited, wantGrantBack: true},
		{class: execution.ErrorProviderUnavailable, wantKind: OutcomeRejected, wantReason: execution.RejectedNoCandidateSucceeded, wantRowClass: execution.ErrorProviderUnavailable, wantGrantBack: true},
		{class: execution.ErrorUpstreamError, wantKind: OutcomeRejected, wantReason: execution.RejectedNoCandidateSucceeded, wantRowClass: execution.ErrorUpstreamError, wantGrantBack: true},
		{class: execution.ErrorInvalidUpstreamResponse, wantKind: OutcomeRejected, wantReason: execution.RejectedNoCandidateSucceeded, wantRowClass: execution.ErrorInvalidUpstreamResponse, wantGrantBack: true},
		{class: execution.ErrorProviderRejectedRequest, wantKind: OutcomeRefused, wantFailure: execution.FailedProviderRejectedRequest, wantRowClass: execution.ErrorProviderRejectedRequest, wantGrantBack: true},
		{class: execution.ErrorContextTooLarge, wantKind: OutcomeRefused, wantFailure: execution.FailedContextTooLarge, wantRowClass: execution.ErrorContextTooLarge, wantGrantBack: true},
		{class: execution.ErrorAuthentication, wantKind: OutcomeRefused, wantFailure: execution.FailedUpstreamAuthentication, wantRowClass: execution.ErrorAuthentication, wantGrantBack: true},
		{class: execution.ErrorStreamAfterCommitment, wantKind: OutcomeRejected, wantReason: execution.RejectedNoCandidateSucceeded, wantRowClass: execution.ErrorInvalidUpstreamResponse, wantGrantBack: true},
	} {
		t.Run(string(tt.class), func(t *testing.T) {
			routing, world, reply, in := routingFixture(t)
			world.seedCandidates("test-model",
				catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
			)
			world.seedBackend("backend-a", catalog.BackendActive)
			routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{
				"backend-a": &fakeExecutor{results: []executors.Result{failExec(tt.class)}},
			})

			outcome, err := routing.Serve(context.Background(), in)
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if outcome.Kind != tt.wantKind || outcome.Failure != tt.wantFailure {
				t.Fatalf("outcome = %s/%s, want %s/%s", outcome.Kind, outcome.Failure, tt.wantKind, tt.wantFailure)
			}
			if tt.wantReason != "" && outcome.Reason != tt.wantReason {
				t.Fatalf("outcome reason = %s, want %s", outcome.Reason, tt.wantReason)
			}
			if len(world.attempts) != 1 || world.attempts[0].ErrorClass != tt.wantRowClass {
				t.Fatalf("the observation reads %v, want one row classed %s", world.attempts, tt.wantRowClass)
			}
			want := int64(10_000)
			if !tt.wantGrantBack {
				want = 10_000 - 37
			}
			if got := world.available("bucket-1"); got != want {
				t.Fatalf("the grant holds %d, want %d", got, want)
			}
			if tt.wantKind == OutcomeRefused && reply.surfaced != tt.wantFailure {
				t.Fatalf("the reply served %v, want the refusal's own cell", reply.served)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the endings' budget and guards
// ---------------------------------------------------------------------------

// TestRoutingStrandsTheHoldWhenTheEndingExhaustsItsBudget: an ending that
// cannot settle fails the request as an internal error and leaves the hold
// stranded — admission committed, the reaper's to reclaim — and never answers
// no_candidate for it, because that answer invites a second hold.
func TestRoutingStrandsTheHoldWhenTheEndingExhaustsItsBudget(t *testing.T) {
	routing, world, _, in := routeFixture(t)
	world.returnFailure = fakePGError{code: "40001"}
	world.returnFailures = 99

	_, err := routing.Serve(context.Background(), in)
	if err == nil {
		t.Fatalf("Serve answered an ending that never settled, want the internal failure")
	}
	if closes := world.count("reservation.close"); closes != 3 {
		t.Fatalf("the ending attempted %d closes, want its whole budget of 3", closes)
	}
	if world.commitCount() != 1 {
		t.Fatalf("the world committed %d units, want only admission's", world.commitCount())
	}
	reservation := admissionReservationRow(t, world)
	if reservation.State != accounting.StateOpen {
		t.Fatalf("the hold is %s, want open — stranded, the reaper's to reclaim", reservation.State)
	}
	row := admissionRequestRow(t, world)
	if row.Status != execution.StatusExecuting {
		t.Fatalf("the request is %s, want executing — no ending was ever stated", row.Status)
	}
	intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1")
	if intake.FinalStatus != nil {
		t.Fatalf("the replay record is terminal, want it still waiting")
	}
	if len(world.facts) != 0 {
		t.Fatalf("the stranded ending appended a fact, want none")
	}
	if got := world.available("bucket-1"); got != 10_000-103 {
		t.Fatalf("the grant holds %d, want 9897 — the drawdown committed and the hold is out", got)
	}
}

// TestRoutingStrandsTheHoldWhenTheSettleLadderExhausts: a settle unit that
// fails after commitment never falls through to a release — a release whose
// compare-and-set then wins would publish "released, unused" over an answer
// the client is already reading, and the feed's last word would be a
// falsehood. The hold stands for the sweep instead: the walk fails as an
// internal error, nothing is appended, and nothing is given back.
func TestRoutingStrandsTheHoldWhenTheSettleLadderExhausts(t *testing.T) {
	routing, world, _, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1})
	world.seedBackend("backend-a", catalog.BackendActive)
	exec := &fakeExecutor{}
	exec.act = func(sink executors.Sink) { _ = sink.Content([]byte(`{"answer":true}`)) }
	exec.results = []executors.Result{okExec(20, 8)}
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{"backend-a": exec})
	// The fault bites the ending's attempt insert — a step only the settle
	// unit takes — for the whole ladder, while every step a release unit
	// would take stays healthy: exactly the sequence where the old shape
	// converted the exhaustion into a release.
	world.attemptFailure = fakePGError{code: "40001"}
	world.attemptFailures = 3

	_, err := routing.Serve(context.Background(), in)
	if err == nil {
		t.Fatalf("Serve answered an ending whose settle ladder never landed, want the internal failure")
	}
	if closes := world.count("reservation.close"); closes != 3 {
		t.Fatalf("the ending attempted %d closes, want the settle ladder's 3 and no release's fourth", closes)
	}
	if returns := world.count("ledger.return"); returns != 0 {
		t.Fatalf("the stranded ending gave capacity back %d times, want none — a release never ran", returns)
	}
	if world.commitCount() != 1 {
		t.Fatalf("the world committed %d units, want only admission's", world.commitCount())
	}
	if reservation := admissionReservationRow(t, world); reservation.State != accounting.StateOpen {
		t.Fatalf("the hold is %s, want open — stranded, the reaper's to reclaim", reservation.State)
	}
	if row := admissionRequestRow(t, world); row.Status != execution.StatusExecuting {
		t.Fatalf("the request is %s, want executing — no ending was ever stated", row.Status)
	}
	if intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1"); intake.FinalStatus != nil {
		t.Fatalf("the replay record is terminal, want it still waiting on the stranded walk")
	}
	if len(world.facts) != 0 {
		t.Fatalf("the stranded ending appended a fact, want none")
	}
}

// TestRoutingSettleRefusesADuplicateSettlementFactAsABug: a second
// settlement-relevant fact behind a CAS the unit owns is the feed's
// invariants broken — the append refuses through the dedup unique's
// sentinel, the unit rolls back whole, and the refusal is not retried,
// because a retry cannot improve a broken invariant.
func TestRoutingSettleRefusesADuplicateSettlementFactAsABug(t *testing.T) {
	routing, world, _, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1})
	world.seedBackend("backend-a", catalog.BackendActive)
	exec := &fakeExecutor{}
	exec.act = func(sink executors.Sink) { _ = sink.Content([]byte(`{"answer":true}`)) }
	exec.results = []executors.Result{okExec(20, 8)}
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{"backend-a": exec})
	world.factDuplicate = true

	if _, err := routing.Serve(context.Background(), in); err == nil {
		t.Fatalf("Serve answered a settlement whose fact the dedup unique refused, want the internal failure")
	}
	if closes := world.count("reservation.close"); closes != 1 {
		t.Fatalf("the ending attempted %d closes, want 1 — the refusal is a bug, not contention to retry", closes)
	}
	if world.rollbackCount() != 1 || world.commitCount() != 1 {
		t.Fatalf("the ending committed %d and rolled back %d beyond admission's commit, want the ending rolled back whole", world.commitCount()-1, world.rollbackCount())
	}
	if reservation := admissionReservationRow(t, world); reservation.State != accounting.StateOpen {
		t.Fatalf("the hold is %s, want open — the rolled-back unit left it standing", reservation.State)
	}
	if len(world.facts) != 0 {
		t.Fatalf("the refused settlement left a fact behind, want none")
	}
}

// TestRoutingObservesTheClose: every stated ending hands a wired observer
// its structured record — the observability contract's fields, filled from
// what the ending proved, and nothing on the shape that could carry a
// prompt, a credential or a provider response. A nil observer is the
// wired-nothing default, which is why every other test in this file runs
// without one and passes.
func TestRoutingObservesTheClose(t *testing.T) {
	routing, world, _, in := routingFixture(t)
	first, second := twoCandidateWorld(world)
	first.results = []executors.Result{failExec(execution.ErrorRateLimited)}
	second.act = func(sink executors.Sink) { _ = sink.Content([]byte(`{"answer":true}`)) }
	second.results = []executors.Result{okExec(101, 55)}
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{
		"backend-a": first,
		"backend-b": second,
	})
	var observed []CloseObservation
	routing.ObserveClose = func(o CloseObservation) { observed = append(observed, o) }

	if _, err := routing.Serve(context.Background(), in); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if len(observed) != 1 {
		t.Fatalf("the walk observed %d closings, want the one settlement", len(observed))
	}
	o := observed[0]
	committed := world.attempts[1]
	fact := world.facts[len(world.facts)-1]
	if o.FinalStatus != string(execution.StatusSucceeded) || !o.Committed || o.UsageState != CloseUsageClaimed {
		t.Fatalf("the observation reads %s/%v/%s, want a committed, claimed success", o.FinalStatus, o.Committed, o.UsageState)
	}
	if o.RequestID != string(committed.RequestID) || o.AttemptID != string(committed.ID) {
		t.Fatalf("the observation names %s/%s, want the committed walk's request and attempt", o.RequestID, o.AttemptID)
	}
	if o.UsageEventID != fact.AppendSeq {
		t.Fatalf("the observation's event id = %d, want the settled fact's feed position %d", o.UsageEventID, fact.AppendSeq)
	}
	if o.Provider != "model-b" || o.Backend != "backend-b" || o.Candidate != 1 {
		t.Fatalf("the observation's attribution reads %s/%s/%d, want the second candidate", o.Provider, o.Backend, o.Candidate)
	}
	if o.InputTokens == nil || *o.InputTokens != 71 || o.OutputTokens == nil || *o.OutputTokens != 16 || o.DeliveryTokens == nil || *o.DeliveryTokens != 15 {
		t.Fatalf("the observation's counts read %v/%v/%v, want the clamped pair and the delivery", o.InputTokens, o.OutputTokens, o.DeliveryTokens)
	}
}

// TestRoutingObservesTheRelease: the release's observation states an ending
// that claimed nothing — no attempt named, commitment false, and the feed
// position of the released fact.
func TestRoutingObservesTheRelease(t *testing.T) {
	routing, world, _, in := routeFixture(t) // the empty registry walks to no_candidate
	var observed []CloseObservation
	routing.ObserveClose = func(o CloseObservation) { observed = append(observed, o) }

	if _, err := routing.Serve(context.Background(), in); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if len(observed) != 1 {
		t.Fatalf("the walk observed %d closings, want the one release", len(observed))
	}
	o := observed[0]
	fact := world.facts[len(world.facts)-1]
	if o.FinalStatus != string(execution.StatusRejected) || o.Committed || o.UsageState != CloseUsageUnclaimed {
		t.Fatalf("the observation reads %s/%v/%s, want an unclaimed, uncommitted rejection", o.FinalStatus, o.Committed, o.UsageState)
	}
	if o.AttemptID != "" || o.UsageEventID != fact.AppendSeq {
		t.Fatalf("the release observation names attempt %q at event %d, want no attempt and the released fact's position", o.AttemptID, o.UsageEventID)
	}
}

// TestRoutingDoesNotRetryTheEndingOnAHardFailure: an ending failure that is
// not contention is not retried — a second attempt would only repeat it.
func TestRoutingDoesNotRetryTheEndingOnAHardFailure(t *testing.T) {
	routing, world, _, in := routeFixture(t)
	world.returnFailure = fakePGError{code: "23514"}
	world.returnFailures = 1

	if _, err := routing.Serve(context.Background(), in); err == nil {
		t.Fatalf("Serve answered an ending that failed on its merits, want the failure")
	}
	if closes := world.count("reservation.close"); closes != 1 {
		t.Fatalf("the ending attempted %d closes, want 1", closes)
	}
}

// TestRoutingLossOfTheEndingStillAnswersNoCandidate: when the ending's
// compare-and-swap loses, someone else owns the hold's ending — the unit
// stops having written nothing further, and this caller's answer is still the
// one the walk reached.
func TestRoutingLossOfTheEndingStillAnswersNoCandidate(t *testing.T) {
	routing, world, _, in := routeFixture(t)
	world.seamCloseLost = true

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeRejected || outcome.Reason != execution.RejectedNoCandidate {
		t.Fatalf("outcome = %s/%s, want rejected/no_candidate", outcome.Kind, outcome.Reason)
	}
	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
		"begin", "reservation.close", "commit",
	})
	intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1")
	if intake.FinalStatus != nil {
		t.Fatalf("the lost CAS finalised the replay record anyway")
	}
	if len(world.facts) != 0 {
		t.Fatalf("the loser of the CAS appended a fact, want none")
	}
}

// TestRoutingDoesNotCommitACloseWithoutItsFact: the release fact is the
// close's receipt, and a close whose receipt never lands is the orphan the
// doctrine forbids. When the request row will not take the ending, the whole
// unit rolls back — the CAS's close included — leaving the hold open for a
// whole retry or, at exhaustion, for the reaper.
func TestRoutingDoesNotCommitACloseWithoutItsFact(t *testing.T) {
	routing, world, _, in := routeFixture(t)
	world.requestFinaliseLost = true

	outcome, err := routing.Serve(context.Background(), in)
	if err == nil {
		t.Fatalf("Serve answered %s from a close whose fact never landed, want the error", outcome.Kind)
	}
	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
		"begin", "reservation.close", "ledger.return", "request.finalise", "rollback",
	})
	if world.commitCount() != 1 || world.rollbackCount() != 1 {
		t.Fatalf("the world committed %d and rolled back %d units, want the admission's one commit and the ending's one rollback", world.commitCount(), world.rollbackCount())
	}
	// Nothing stayed half-closed: the hold is back open, the row still
	// executing, the record still in flight, and no fact exists.
	if reservation := admissionReservationRow(t, world); reservation.State != accounting.StateOpen {
		t.Fatalf("the hold is %s after the rollback, want open — the close went back with the unit", reservation.State)
	}
	if row := admissionRequestRow(t, world); row.Status != execution.StatusExecuting {
		t.Fatalf("the request row is %s after the rollback, want executing", row.Status)
	}
	if intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1"); intake.FinalStatus != nil {
		t.Fatalf("the replay record is terminal after the rollback, want still in flight")
	}
	if len(world.facts) != 0 {
		t.Fatalf("the factless close appended %d facts, want none", len(world.facts))
	}
}

// TestRoutingSettleToleratesARacedAttemptAppend: the settle unit's attempt
// insert racing a writer that already persisted the row reads as done — the
// lost-commit-ack shape the sentinel exists for — and the ending lands whole.
func TestRoutingSettleToleratesARacedAttemptAppend(t *testing.T) {
	routing, world, _, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	world.attemptDuplicate = true
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{
		"backend-a": &fakeExecutor{
			// The answer commits — the sink's commitment is what makes the
			// executor's Success a settlement, not a claim the walk re-classes.
			act:     func(sink executors.Sink) { _ = sink.Content([]byte(`{"answer":true}`)) },
			results: []executors.Result{okExec(9, 9)},
		},
	})

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeServed {
		t.Fatalf("outcome = %s, want served", outcome.Kind)
	}
	if len(world.attempts) != 0 {
		t.Fatalf("the raced append stored %d rows, want none — the twin was already on disk", len(world.attempts))
	}
	if world.facts[len(world.facts)-1].Kind != accounting.KindSettled {
		t.Fatalf("the ending did not land its settled fact")
	}
}

// ---------------------------------------------------------------------------
// the settlement's arithmetic, in one table
// ---------------------------------------------------------------------------

// clampedPtr is the figure pointer the table cases are written with.
func clampedPtr(v int64) *int64 { return &v }

// TestSettleBasisCases is the settlement arithmetic's whole table: what the
// provider reported, against the counts the hold was derived from (5 input,
// 16 output basis — the fixture's own), decides the settled pair and the
// capture label that attests where each figure came from, under both arms of
// the delivery boundary — the completed answer and the stream that failed
// after commitment. Every case's pair prices at or below the hold — the
// invariant the clamping exists to keep.
func TestSettleBasisCases(t *testing.T) {
	// The prices make the hold guard below bite: at a minor unit per token,
	// Hold(5, 16, 1M, 2M) = 37, the hold the fixture carries, so every row's
	// amount is a real number the guard actually compares — not a zero
	// priced against a ceiling.
	admitted := &Admission{
		InputTokens: 5, OutputBasis: 16, Hold: 37,
		Price: catalog.PriceSnapshot{InputUnitPrice: 1_000_000, OutputUnitPrice: 2_000_000},
	}
	delivered := []byte(`{"answer":true}`) // 15 bytes, the delivery figure on every row

	cases := []struct {
		name          string
		streamFailed  bool
		reportedInput *int64
		reportedOut   *int64
		wantCapture   accounting.CaptureMethod
		wantInput     int64
		wantOutput    int64
	}{
		// The completed answer: the provider's report prices where it stands
		// within the counts the hold priced, clamps where it does not, and the
		// delivered tokens take over where no output report arrived — the
		// gateway's own observation of work the client provably received,
		// which is why the label is gateway_observed and not the floor.
		{"completed: both reported within the bounds", false, clampedPtr(4), clampedPtr(15), accounting.CaptureReported, 4, 15},
		{"completed: both reported at the bounds", false, clampedPtr(5), clampedPtr(16), accounting.CaptureReported, 5, 16},
		{"completed: output beyond the basis clamps to it", false, clampedPtr(4), clampedPtr(77), accounting.CaptureReservationFloor, 4, 16},
		{"completed: input beyond the count clamps to it", false, clampedPtr(9), clampedPtr(15), accounting.CaptureReservationFloor, 5, 15},
		{"completed: both beyond clamp both", false, clampedPtr(9), clampedPtr(77), accounting.CaptureReservationFloor, 5, 16},
		{"completed: output unreported falls to the delivered tokens", false, clampedPtr(4), nil, accounting.CaptureGatewayObserved, 4, 15},
		{"completed: input unreported falls to the count", false, nil, clampedPtr(15), accounting.CaptureGatewayObserved, 5, 15},
		{"completed: nothing reported prices the gateway's own pair", false, nil, nil, accounting.CaptureGatewayObserved, 5, 15},
		// The stream that failed after commitment: the delivered tokens are
		// the only output that can bill, whatever the provider counted about
		// the stream it never finished, and the label never claims reported —
		// the priced figures are not the provider's pair even when the
		// provider named one.
		{"failed: an output report as given is still the delivered tokens", true, clampedPtr(4), clampedPtr(9), accounting.CaptureGatewayObserved, 4, 15},
		{"failed: an output report beyond the basis is still the delivered tokens", true, clampedPtr(4), clampedPtr(77), accounting.CaptureGatewayObserved, 4, 15},
		{"failed: input beyond the count still clamps", true, clampedPtr(9), clampedPtr(9), accounting.CaptureReservationFloor, 5, 15},
		{"failed: nothing reported prices the count and the delivery", true, nil, nil, accounting.CaptureGatewayObserved, 5, 15},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := settleBasis(admitted, delivered, tc.reportedInput, tc.reportedOut, tc.streamFailed)
			if usage.capture != tc.wantCapture {
				t.Fatalf("capture = %s, want %s", usage.capture, tc.wantCapture)
			}
			if usage.input == nil || *usage.input != tc.wantInput {
				t.Fatalf("input = %v, want %d", usage.input, tc.wantInput)
			}
			if usage.output == nil || *usage.output != tc.wantOutput {
				t.Fatalf("output = %v, want %d", usage.output, tc.wantOutput)
			}
			if usage.delivery == nil || *usage.delivery != 15 {
				t.Fatalf("delivery = %v, want the 15 bytes delivered", usage.delivery)
			}
			amount, err := settleAmount(usage, admitted.Price)
			if err != nil {
				t.Fatalf("settleAmount: %v", err)
			}
			if amount > admitted.Hold {
				t.Fatalf("the settled amount = %d, above the hold %d — the bound the arithmetic exists to keep", amount, admitted.Hold)
			}
		})
	}
}

// TestSettleBasisDeliveredBeyondTheBasis is the regime the first table's
// fixture cannot reach: a delivered count the basis cannot cover. The clamp
// binds, the reservation's own figure is what settles, and the capture
// attests the floor — while the delivery column still carries the full count
// the client provably received. Without the clamp these pairs price above
// the hold: delivered bytes are wire bytes and the basis is the token
// ceiling the hold was sized with, so the unclamped arms breached the
// ending's first law in ordinary traffic.
func TestSettleBasisDeliveredBeyondTheBasis(t *testing.T) {
	admitted := &Admission{
		InputTokens: 5, OutputBasis: 8, Hold: 21,
		Price: catalog.PriceSnapshot{InputUnitPrice: 1_000_000, OutputUnitPrice: 2_000_000},
	}
	delivered := []byte(`{"answer":true}`) // 15 delivered bytes against an 8 basis

	cases := []struct {
		name         string
		streamFailed bool
		reportedIn   *int64
		reportedOut  *int64
		wantCapture  accounting.CaptureMethod
		wantInput    int64
		wantOutput   int64
	}{
		{"failed: the delivered bytes clamp to the basis", true, nil, nil, accounting.CaptureReservationFloor, 5, 8},
		{"failed: clamped past a report that named less", true, nil, clampedPtr(3), accounting.CaptureReservationFloor, 5, 8},
		{"completed: the unreported delivery clamps to the basis", false, nil, nil, accounting.CaptureReservationFloor, 5, 8},
		{"completed: input unreported, output clamped", false, nil, clampedPtr(77), accounting.CaptureReservationFloor, 5, 8},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := settleBasis(admitted, delivered, tc.reportedIn, tc.reportedOut, tc.streamFailed)
			if usage.capture != tc.wantCapture {
				t.Fatalf("capture = %s, want %s — the clamp that bound is the reservation's own figure settling", usage.capture, tc.wantCapture)
			}
			if usage.input == nil || *usage.input != tc.wantInput {
				t.Fatalf("input = %v, want %d", usage.input, tc.wantInput)
			}
			if usage.output == nil || *usage.output != tc.wantOutput {
				t.Fatalf("output = %v, want the %d basis the delivered count was clamped to", usage.output, tc.wantOutput)
			}
			if usage.delivery == nil || *usage.delivery != 15 {
				t.Fatalf("delivery = %v, want the 15 bytes delivered — the clamp bounds the priced figure, never the recorded one", usage.delivery)
			}
			amount, err := settleAmount(usage, admitted.Price)
			if err != nil {
				t.Fatalf("settleAmount: %v", err)
			}
			if amount != admitted.Hold {
				t.Fatalf("the settled amount = %d, want %d — the clamped pair prices exactly at the hold", amount, admitted.Hold)
			}
		})
	}
}

// TestAStreamFailureWithNoDeliveryPricesARealZero pins the failed arm's nil
// vs zero doctrine: a stream that died before a byte reached the client
// prices its output at a present zero — the delivered tokens, of which there
// were none — and never at the basis the hold reserved. The zero is a real
// figure about real work (a prompt was read, nothing was answered), and nil
// would claim nobody knows, which is false: the gateway watched every byte
// that did not arrive.
func TestAStreamFailureWithNoDeliveryPricesARealZero(t *testing.T) {
	admitted := &Admission{InputTokens: 5, OutputBasis: 16, Hold: 37}

	usage := settleBasis(admitted, nil, clampedPtr(4), clampedPtr(9), true)
	if usage.output == nil || *usage.output != 0 {
		t.Fatalf("output = %v, want a present zero — the delivery boundary prices what was delivered, and nothing was", usage.output)
	}
	if usage.capture != accounting.CaptureGatewayObserved {
		t.Errorf("capture = %s, want gateway_observed — the zero is the gateway's own observation", usage.capture)
	}
	if usage.input == nil || *usage.input != 4 {
		t.Errorf("input = %v, want the report's 4 — the delivery boundary does not touch input", usage.input)
	}
}

// ---------------------------------------------------------------------------
// the settle that loses its race — the orphan tail
// ---------------------------------------------------------------------------

// TestRoutingSettleLosesTheCloseAndRecordsTheOrphanTail: a settle whose
// close loses the race does not take the stream's telemetry with it. The
// unit itself writes nothing — the winner owns the ending — and the tail
// that follows records what the loser saw: the attempt appended standalone,
// then the unbillable-orphaned fact, in one unit, the feed's statement that
// usage was observed on a named committed attempt this process did not
// settle. No terminal row moves, and the fact carries no amount.
func TestRoutingSettleLosesTheCloseAndRecordsTheOrphanTail(t *testing.T) {
	routing, world, reply, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	world.seamCloseLost = true
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{
		"backend-a": &fakeExecutor{
			act:     func(sink executors.Sink) { _ = sink.Content([]byte(`{"answer":true}`)) },
			results: []executors.Result{okExec(999, 999)},
		},
	})

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeServed || !outcome.Routing.Committed {
		t.Fatalf("outcome = %s committed=%v, want served of a committed answer — the loser's answer already left", outcome.Kind, outcome.Routing.Committed)
	}

	// The settle unit loses at its first step and commits empty; the tail
	// appends the attempt standalone, reads the record, and states the
	// orphaned fact in one unit of its own.
	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
		"begin", "reservation.close", "commit",
		"attempt.insert",
		"begin", "fact.append", "commit",
	})

	if len(world.attempts) != 1 || world.attempts[0].Outcome != execution.OutcomeSucceeded {
		t.Fatalf("the tail stored %v, want the one committed attempt the race nearly erased", world.attempts)
	}
	attempt := world.attempts[0]

	// The winner's writes are not this test's to make — the fake's lost
	// close moved nothing — so the ending's rows read exactly as a reaper
	// claim would leave them, and the orphan fact stands beside that
	// outcome rather than overwriting it.
	if reservation := admissionReservationRow(t, world); reservation.State != accounting.StateOpen {
		t.Fatalf("the hold is %s, want open — the loser of the CAS ends no ending", reservation.State)
	}
	if row := admissionRequestRow(t, world); row.Status != execution.StatusExecuting {
		t.Fatalf("the request is %s, want executing — no terminal row is the tail's to write", row.Status)
	}
	if intake := admissionIntakeRow(t, world, admissionAccount, "replay-key-1"); intake.FinalStatus != nil {
		t.Fatalf("the replay record is terminal, want it waiting on the ending's owner")
	}

	if len(world.facts) != 1 || world.facts[0].Kind != accounting.KindUnbillableOrphaned {
		t.Fatalf("the feed holds %v, want the one unbillable-orphaned fact", world.facts)
	}
	fact := world.facts[0]
	if fact.CommittedAttemptID != attempt.ID {
		t.Fatalf("the orphaned fact names %s, want the attempt it observed", fact.CommittedAttemptID)
	}
	if fact.CaptureMethod != accounting.CaptureReservationFloor {
		t.Fatalf("the orphaned fact's capture = %s, want reservation_floor — the report beyond the hold clamps like any settlement's", fact.CaptureMethod)
	}
	if fact.SettledAmount != nil {
		t.Fatalf("the orphaned fact carries an amount %v, want none — it is never a charge", fact.SettledAmount)
	}
	if len(reply.served) != 1 || reply.served[0] != "succeeded" {
		t.Fatalf("the reply served %v, want the delivered answer closed", reply.served)
	}
}

// TestRoutingSettleSkipsTheOrphanFactWhenTheEndingWasOwned: when the record
// already carries its terminal pointer, a settle finalised the request and
// its settled fact already carried this usage into the feed — the tail
// preserves the attempt and adds nothing the feed needs twice.
func TestRoutingSettleSkipsTheOrphanFactWhenTheEndingWasOwned(t *testing.T) {
	routing, world, _, in := routingFixture(t)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	world.seamCloseLost = true
	world.intakePreFinalised = true
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{
		"backend-a": &fakeExecutor{
			act:     func(sink executors.Sink) { _ = sink.Content([]byte(`{"answer":true}`)) },
			results: []executors.Result{okExec(4, 4)},
		},
	})

	outcome, err := routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if outcome.Kind != OutcomeServed {
		t.Fatalf("outcome = %s, want served", outcome.Kind)
	}

	// The tail ran — the attempt stands — and stopped at the verdict: no
	// unit opened for a fact the winner's own already stated.
	wantEvents(t, world, []string{
		"begin", "ledger.drawdown", "request.insert", "reservation.insert", "intake.insert", "commit",
		"begin", "reservation.close", "commit",
		"attempt.insert",
	})
	if len(world.facts) != 0 {
		t.Fatalf("the feed holds %v, want no orphaned fact beside an owner's settled one", world.facts)
	}
	if len(world.attempts) != 1 {
		t.Fatalf("the tail stored %d attempts, want the one the race nearly erased", len(world.attempts))
	}
}

// ---------------------------------------------------------------------------
// many at once
// ---------------------------------------------------------------------------

// TestRoutingServesManyRequestsConcurrently: a hundred requests admitted and
// routed over one runtime — one world, one registry, one alias — all served
// and all settled, with no trace the walk is not safe to run concurrently.
// The race detector is the assertion's other half; `go test -race` is the
// way this test is meant to be read.
func TestRoutingServesManyRequestsConcurrently(t *testing.T) {
	routing, world, _, in := routingFixture(t)
	// A hundred concurrent admissions each draw the whole 103 hold before any
	// settlement returns its remainder, so the grant must cover the peak:
	// 100 × 103 against 10 000 would starve the tail of the burst for a
	// reason this test is not about. The subject is the walk's concurrency,
	// not the waterfall's capacity. The fixture's bucket is replaced, not
	// re-seeded — a second leg naming the same bucket id is a hold the
	// domain refuses to form.
	world.buckets = nil
	world.seedBucket(admissionAccount, "bucket-1", 1_000_000, true)
	world.seedCandidates("test-model",
		catalog.Candidate{ID: "cand-a", BackendID: "backend-a", ProviderModel: "model-a", Position: 1},
	)
	world.seedBackend("backend-a", catalog.BackendActive)
	routing.registry = executors.NewRegistry(map[catalog.BackendID]executors.Executor{
		"backend-a": &fakeExecutor{
			act:     func(sink executors.Sink) { _ = sink.Content([]byte("answer")) },
			results: []executors.Result{okExec(20, 30)},
		},
	})

	const requests = 100
	var wg sync.WaitGroup
	answers := make([]ChatOutcome, requests)
	errs := make([]error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			one := in
			one.RequestID = identity.RequestID("req-concurrent-" + strconv.Itoa(i))
			one.IdempotencyKey = "replay-key-concurrent-" + strconv.Itoa(i)
			one.Reply = &fakeReply{}
			answers[i], errs[i] = routing.Serve(context.Background(), one)
		}(i)
	}
	wg.Wait()

	for i := 0; i < requests; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d: Serve: %v", i, errs[i])
		}
		if answers[i].Kind != OutcomeServed {
			t.Fatalf("request %d: outcome = %s, want served", i, answers[i].Kind)
		}
	}
	if got := world.commitCount(); got != 2*requests {
		t.Fatalf("the world committed %d units, want %d — admission and settlement for each", got, 2*requests)
	}
	if got := len(world.facts); got != requests {
		t.Fatalf("the feed holds %d facts, want %d settled", got, requests)
	}
	for _, fact := range world.facts {
		// The scripted report reads 20 input — inside the 71 the hold priced —
		// and 30 output, beyond the 16 basis, so the output clamps and the
		// capture attests the floor; the input crosses as reported. Every
		// request's fact reads the settlement identically.
		if fact.Kind != accounting.KindSettled || fact.CaptureMethod != accounting.CaptureReservationFloor {
			t.Fatalf("a fact reads %s/%s, want a reservation-floor settlement", fact.Kind, fact.CaptureMethod)
		}
		if fact.ProviderInputTokens == nil || *fact.ProviderInputTokens != 20 ||
			fact.ProviderOutputTokens == nil || *fact.ProviderOutputTokens != 16 {
			t.Fatalf("a fact settles %v/%v, want the report's 20 and the basis's 16", fact.ProviderInputTokens, fact.ProviderOutputTokens)
		}
	}
	if got := world.available("bucket-1"); got != 1_000_000-103*requests {
		t.Fatalf("the grant holds %d, want %d — every hold became cost", got, 1_000_000-103*requests)
	}
}
