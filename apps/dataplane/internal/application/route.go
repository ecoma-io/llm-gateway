package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/routing"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/executors"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The routing stage: which eligible candidate is tried next, and what a
// failure earns. It is the pipeline's second stage, between admission (may
// this request run?) and execution (how does one provider answer?) — and it
// mixes with neither. Admission's decision is final when it arrives here: the
// hold is open, the request row is executing, the replay record is watching.
// Execution's detail is invisible from here: a candidate is one Executor
// call, whose retries are the adapter's own judgment about its provider.
//
// The walk is the whole policy. Eligibility keeps the catalog's order and
// drops what cannot serve — a disabled backend, an unregistered executor — so
// the router sees only candidates that may answer. The walk tries them in
// that order, one attempt each, and the eligible list is the attempt budget:
// there is no separate maximum, because the operator who edits the alias's
// candidate list is already editing the budget. A failure is classified and
// dispositioned — fall through while the budget lasts, or surface the
// refusal — and a commitment ends everything: after the first content-bearing
// byte reaches the client there is no next candidate, only the answer that
// was started.
//
// Every ending is written before it is answered, in its own unit of work —
// route_endings.go — and no transaction is ever open across a provider call
// (ADR 0001 rule 4): attempts are appended when a call finishes, never while
// one is in flight.

// ChatRouting is the routing stage of the chat completion pipeline: a
// ChatCompletion that wraps the admission use case and routes what admission
// admits. It implements the same interface the transport calls, so the
// composition root wraps one around the other and the endpoint's wiring
// reads the same as it always did.
type ChatRouting struct {
	store        persistence.Store
	aliases      persistence.ModelAliases
	backends     persistence.Backends
	requests     persistence.RequestRepository
	attempts     persistence.AttemptRepository
	intakes      persistence.IntakeRepository
	reservations persistence.ReservationRepository
	ledger       persistence.QuotaProjectionRepository
	facts        persistence.FactRepository
	registry     executors.Registry
	routed       ChatCompletion
	clock        txClock
}

// NewChatRouting builds the routing stage over the ports it needs and the
// admission use case it follows. It panics on a nil port or a nil wrapped use
// case for the reason its admission sibling states: a port this stage was
// promised and did not get is a wiring defect, and the middle of a walk —
// after a candidate failed and before the ending is written — is a strictly
// worse place to learn about it.
func NewChatRouting(
	store persistence.Store,
	aliases persistence.ModelAliases,
	backends persistence.Backends,
	requests persistence.RequestRepository,
	attempts persistence.AttemptRepository,
	intakes persistence.IntakeRepository,
	reservations persistence.ReservationRepository,
	ledger persistence.QuotaProjectionRepository,
	facts persistence.FactRepository,
	registry executors.Registry,
	routed ChatCompletion,
) *ChatRouting {
	switch {
	case store == nil:
		panic("application: NewChatRouting requires a store")
	case aliases == nil:
		panic("application: NewChatRouting requires an aliases repository")
	case backends == nil:
		panic("application: NewChatRouting requires a backends repository")
	case requests == nil:
		panic("application: NewChatRouting requires a request repository")
	case attempts == nil:
		panic("application: NewChatRouting requires an attempt repository")
	case intakes == nil:
		panic("application: NewChatRouting requires an intake repository")
	case reservations == nil:
		panic("application: NewChatRouting requires a reservation repository")
	case ledger == nil:
		panic("application: NewChatRouting requires a quota projection repository")
	case facts == nil:
		panic("application: NewChatRouting requires a fact repository")
	case registry == nil:
		panic("application: NewChatRouting requires an executor registry")
	case routed == nil:
		panic("application: NewChatRouting requires the use case it routes for")
	}
	return &ChatRouting{
		store:        store,
		aliases:      aliases,
		backends:     backends,
		requests:     requests,
		attempts:     attempts,
		intakes:      intakes,
		reservations: reservations,
		ledger:       ledger,
		facts:        facts,
		registry:     registry,
		routed:       routed,
		clock:        storeClock{store: store},
	}
}

// Serve routes one admitted request, or hands back the decision admission
// reached. The contract is ChatCompletion's: every answer arrives as an
// outcome with a nil error, and a non-nil error means the pipeline could not
// reach an answer at all. An answer that travelled the request's reply — a
// served answer, a surfaced refusal, an abandoned walk — is still an outcome
// with a nil error; the transport reads `Routing` for what happened and
// writes nothing, because the reply already did.
func (r *ChatRouting) Serve(ctx context.Context, in ChatInput) (ChatOutcome, error) {
	if in.Reply == nil {
		return ChatOutcome{}, fmt.Errorf("application: route request %s: no reply arrived with the request", in.RequestID)
	}
	if r.store.InUnitOfWork(ctx) {
		return ChatOutcome{}, fmt.Errorf("application: routing refuses to run inside a unit of work it did not open")
	}
	outcome, err := r.routed.Serve(ctx, in)
	if err != nil || outcome.Kind != OutcomeAdmitted || outcome.Admitted == nil {
		return outcome, err
	}
	return r.route(ctx, in, outcome.Admitted)
}

// route walks the alias's eligible candidates and states the ending the walk
// earned. The selection reads are pre-unit reads, like the probe's: the alias
// as the catalog stands now, each candidate's backend state, and the
// registry's registrations — no unit is open while they run, because the walk
// itself must never hold a transaction across a provider call.
func (r *ChatRouting) route(ctx context.Context, in ChatInput, admitted *Admission) (ChatOutcome, error) {
	reply := in.Reply
	trace := &RoutingTrace{Alias: admitted.Alias}

	alias, err := r.aliases.ByName(ctx, admitted.Alias)
	if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return ChatOutcome{}, fmt.Errorf("application: route request %s: read the alias: %w", admitted.RuntimeRequestID, err)
	}
	servable := func(id catalog.BackendID) bool {
		backend, err := r.backends.ByID(ctx, id)
		return err == nil && backend != nil && backend.State == catalog.BackendActive
	}
	callable := func(id catalog.BackendID) bool {
		_, ok := r.registry.For(id)
		return ok
	}
	var eligible []catalog.Candidate
	if err == nil && alias.State == catalog.AliasActive {
		eligible = routing.Eligible(alias.Candidates, servable, callable)
	}
	walk := routing.NewSelection(eligible)

	// No eligible candidate is a determinate answer, not a failure: nothing
	// matched the request, the hold goes back unused, and the caller is told
	// to retry later rather than differently. The reply opens on this path
	// only far enough to write the one pre-commitment cell it owes — no
	// framing decision has been made, and none is needed for an error body.
	if walk.Tried() == 0 {
		if err := r.release(ctx, in, admitted, execution.RejectedNoCandidate, ""); err != nil {
			return ChatOutcome{}, err
		}
		reply.ServeNoCandidate()
		return ChatOutcome{
			Kind:             OutcomeRejected,
			Reason:           execution.RejectedNoCandidate,
			RuntimeRequestID: admitted.RuntimeRequestID,
			Routing:          trace,
		}, nil
	}

	// The framing is decided before the first candidate runs: the answer's
	// shape is the caller's request, frozen at admission, not a fact any
	// candidate gets to choose.
	reply.Open(admitted.Stream)

	for step := 1; ; step++ {
		if ctx.Err() != nil {
			// The caller is gone before any candidate answered. Nothing is
			// finalised and nothing can be written — there is no channel —
			// so the request stays executing with its hold open, exactly the
			// shape a dead process leaves, for the reaper to close.
			return ChatOutcome{
				Kind:             OutcomeAbandoned,
				RuntimeRequestID: admitted.RuntimeRequestID,
				Routing:          trace,
			}, nil
		}
		candidate, ok := walk.Next(step)
		if !ok {
			// The walk ran out: every eligible candidate was tried and none
			// served the request. The answer names the walk, never the last
			// candidate's failure alone — the caller may not retry
			// differently on the strength of one provider's bad day.
			if err := r.release(ctx, in, admitted, execution.RejectedNoCandidateSucceeded, ""); err != nil {
				return ChatOutcome{}, err
			}
			reply.ServeNoCandidate()
			return ChatOutcome{
				Kind:             OutcomeRejected,
				Reason:           execution.RejectedNoCandidateSucceeded,
				RuntimeRequestID: admitted.RuntimeRequestID,
				Routing:          trace,
			}, nil
		}

		executor, _ := r.registry.For(candidate.BackendID)
		attemptID := identity.NewAttemptID()
		started := time.Now().UTC()
		result := executor.Execute(ctx, executors.AttemptSpec{
			RequestID:          admitted.RuntimeRequestID,
			AttemptID:          attemptID,
			BackendID:          string(candidate.BackendID),
			ProviderModel:      candidate.ProviderModel,
			ParameterOverrides: candidate.ParameterOverrides,
			Stream:             admitted.Stream,
			Body:               admitted.RawBody,
		}, reply)
		finished := time.Now().UTC()
		trace.Attempts++
		trace.LastPosition = candidate.Position

		if success, ok := result.(executors.Success); ok {
			// The answer was produced and delivered: the hold becomes the
			// request's cost, the attempt names itself the committed one,
			// and nothing about the walk is revisited — there is no better
			// answer than the one the client is reading.
			attempt, err := r.attemptRow(admitted, candidate, attemptID, started, finished,
				execution.OutcomeSucceeded, "", success.ProviderRequestID, nil,
				success.Usage.InputTokens, success.Usage.OutputTokens, deliveredTokens(reply))
			if err != nil {
				return ChatOutcome{}, err
			}
			usage := settleBasis(admitted, deliveredBytes(reply), success.Usage.InputTokens, success.Usage.OutputTokens)
			if err := r.settle(ctx, in, admitted, attempt, usage, true); err != nil {
				return ChatOutcome{}, err
			}
			reply.ServeSucceeded()
			trace.Committed = true
			return ChatOutcome{
				Kind:             OutcomeServed,
				RuntimeRequestID: admitted.RuntimeRequestID,
				Routing:          trace,
			}, nil
		}

		failure, ok := result.(executors.Failure)
		if !ok {
			// A nil or foreign result is the executor's contract broken, not
			// a verdict about the provider: classified as an answer nobody
			// could read, and walked like one.
			failure = executors.Failure{Class: execution.ErrorInvalidUpstreamResponse}
		}
		class := failure.Class
		if class == execution.ErrorStreamAfterCommitment && !reply.Committed() {
			// The sink owns the commitment fact, not the executor: a
			// stream-failure claim over an answer that never crossed its
			// commitment point is an answer nobody could read, and it is
			// classified as one.
			class = execution.ErrorInvalidUpstreamResponse
		}
		trace.LastErrorClass = class

		if reply.Committed() {
			// The failure arrived after commitment: no disposition applies,
			// no candidate is tried, and the usage in flight settles on the
			// attempt the commitment happened on. The attempt's own class is
			// the stream's failure — the vocabulary's one word for this —
			// whatever the provider called it.
			attempt, err := r.attemptRow(admitted, candidate, attemptID, started, finished,
				execution.OutcomeFailedAfterCommitment, execution.ErrorStreamAfterCommitment,
				failure.ProviderRequestID, failure.ProviderError,
				failure.Usage.InputTokens, failure.Usage.OutputTokens, deliveredTokens(reply))
			if err != nil {
				return ChatOutcome{}, err
			}
			usage := settleBasis(admitted, deliveredBytes(reply), failure.Usage.InputTokens, failure.Usage.OutputTokens)
			if err := r.settle(ctx, in, admitted, attempt, usage, false); err != nil {
				return ChatOutcome{}, err
			}
			reply.ServeMidStreamFailure()
			trace.Committed = true
			return ChatOutcome{
				Kind:             OutcomeServed,
				RuntimeRequestID: admitted.RuntimeRequestID,
				Routing:          trace,
			}, nil
		}

		// A pre-commitment failure is appended where it finished — its own
		// write, no unit of work still open, the observation of a call that
		// happened whatever the walk does next.
		attempt, err := r.attemptRow(admitted, candidate, attemptID, started, finished,
			execution.OutcomeFailedBeforeCommitment, class,
			failure.ProviderRequestID, failure.ProviderError, nil, nil, nil)
		if err != nil {
			return ChatOutcome{}, err
		}
		if err := r.appendAttempt(ctx, attempt); err != nil {
			return ChatOutcome{}, err
		}

		if reason, surfaced := routing.SurfacedRefusal(class); surfaced {
			// The caller's request is the thing at fault: the same request
			// at another candidate fails identically or answers differently,
			// so the walk stops and the refusal is surfaced — released hold,
			// failed request row, and the refusal's own cell.
			if err := r.release(ctx, in, admitted, "", reason); err != nil {
				return ChatOutcome{}, err
			}
			reply.ServeSurfaced(reason)
			return ChatOutcome{
				Kind:             OutcomeRefused,
				Failure:          reason,
				RuntimeRequestID: admitted.RuntimeRequestID,
				Routing:          trace,
			}, nil
		}
		if !routing.FallbackEligible(class) {
			// Unreachable while the disposition switch stays exhaustive: a
			// class that neither falls through nor surfaces has no ending
			// this walk can state, and the honest answer is the internal
			// failure — the request stays executing for the reaper, the way
			// every undecided walk does.
			return ChatOutcome{}, fmt.Errorf("application: route request %s: failure class %q has no disposition",
				admitted.RuntimeRequestID, class)
		}
		// Fall through: the next candidate, same request, same answer
		// channel — the caller sees at most one error however deep the walk
		// went.
	}
}

// attemptRow forms one finished call's row. Candidate position counts from
// zero on the row and from one in the catalog — the walk's first try is the
// catalog's position 1 — and the retry sequence is always zero: retries
// inside one candidate are the executor's judgment about its provider, and a
// router retry of the same candidate would only repeat a failure the catalog
// forbids listing twice.
func (r *ChatRouting) attemptRow(
	admitted *Admission,
	candidate catalog.Candidate,
	attemptID identity.AttemptID,
	started, finished time.Time,
	outcome execution.Outcome,
	class execution.ErrorClass,
	providerRequestID string,
	providerError []byte,
	reportedInput, reportedOutput, delivery *int64,
) (execution.Attempt, error) {
	attempt, err := execution.NewAttempt(attemptID, admitted.RuntimeRequestID, candidate.Position-1, 0,
		string(candidate.BackendID), candidate.ProviderModel, outcome, class, started, finished)
	if err != nil {
		return execution.Attempt{}, fmt.Errorf("application: route request %s: form the attempt: %w", admitted.RuntimeRequestID, err)
	}
	attempt.ProviderRequestID = providerRequestID
	if len(providerError) > 0 {
		// Telemetry never blocks the request path: a provider error that
		// breaks the telemetry's own shape (not a json object, over the cap)
		// is dropped rather than written, and the row keeps the class that
		// decided the walk.
		_ = attempt.SetProviderError(providerError)
	}
	if err := attempt.RecordProviderUsage(reportedInput, reportedOutput, delivery); err != nil {
		return execution.Attempt{}, fmt.Errorf("application: route request %s: record the attempt usage: %w", admitted.RuntimeRequestID, err)
	}
	return attempt, nil
}

// appendAttempt writes one finished call's row on its own — outside any unit
// of work, a single statement the engine makes atomic. It carries the ending
// budget's retries for the same reason the endings do: a contention class
// aborts a write that never happened, and a fresh one may win.
func (r *ChatRouting) appendAttempt(ctx context.Context, attempt execution.Attempt) error {
	var err error
	for budget := 0; budget < endingMaxAttempts; budget++ {
		if err = r.attempts.Insert(ctx, attempt); err == nil {
			return nil
		}
		if errors.Is(err, persistence.ErrAttemptAlreadyAppended) {
			// The append raced a writer that persisted the same row — the
			// lost-commit-ack shape the sentinel exists for. The row is on
			// disk; the append is done.
			return nil
		}
		if !isRetryableStoreFailure(err) {
			return fmt.Errorf("application: route request %s: append the attempt: %w", attempt.RequestID, err)
		}
		if budget+1 < endingMaxAttempts {
			select {
			case <-ctx.Done():
				return fmt.Errorf("application: route request %s: append the attempt: %w", attempt.RequestID, ctx.Err())
			case <-time.After(endingRetryBackoff << budget):
			}
		}
	}
	return fmt.Errorf("application: route request %s: the attempt did not settle after %d attempts: %w", attempt.RequestID, endingMaxAttempts, err)
}

// deliveredBytes and deliveredTokens are the transport's own observation of
// what the client received, read once after a call returns.
func deliveredBytes(reply Reply) []byte { return reply.Delivered() }

func deliveredTokens(reply Reply) *int64 {
	delivered := accounting.CountDeliveredTokens(reply.Delivered())
	return &delivered
}

// settleUsage is what a settled ending claims: the capture method naming how
// the figures were known, and the three counts, each nil when nobody knows.
type settleUsage struct {
	capture  accounting.CaptureMethod
	input    *int64
	output   *int64
	delivery *int64
}

// settleBasis derives what the gateway claims for a settled ending — the
// rules in one place, so the success and the mid-stream failure settle by the
// same arithmetic:
//
//   - delivery is always the gateway's own count of what left the process;
//   - input is the provider's report when it gave one, otherwise the count
//     admission priced the hold from — the gateway's own observation of the
//     input side;
//   - output is the provider's report when it gave one, otherwise the
//     delivered count — which can only understate, because generation that
//     was never forwarded is unknowable;
//   - capture is reported when the provider's report was whole (input and
//     output both), gateway_observed whenever any figure is the gateway's
//     own — the label is the consumer's confidence, and a mixed answer is
//     not a report.
//
// The reservation floor is deliberately out of reach on this path: a settled
// ending always names a committed attempt, and a commitment means content
// reached the client, so there is always a delivered count to stand on.
func settleBasis(admitted *Admission, delivered []byte, reportedInput, reportedOutput *int64) settleUsage {
	observed := accounting.CountDeliveredTokens(delivered)
	delivery := observed

	input := reportedInput
	if input == nil {
		count := int64(admitted.InputTokens)
		input = &count
	}
	output := reportedOutput
	if output == nil {
		output = &delivery
	}

	capture := accounting.CaptureGatewayObserved
	if reportedInput != nil && reportedOutput != nil {
		capture = accounting.CaptureReported
	}
	return settleUsage{
		capture:  capture,
		input:    input,
		output:   output,
		delivery: &delivery,
	}
}
