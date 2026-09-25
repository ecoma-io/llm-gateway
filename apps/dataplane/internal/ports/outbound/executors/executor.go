// Package executors is the port the routing stage reaches a provider through,
// and the channel an answer travels back through.
//
// Two directions live here, one type each. An Executor is the arrow toward
// the provider: the routing stage hands it one candidate's call — the
// admitted bytes, the provider model to serve them with, the attempt's
// identity — and it performs that call, translating between the gateway's
// request and one provider's API. A Sink is the arrow back: the transport
// builds one over the caller's connection, and the executor writes the
// answer's content-bearing bytes into it as they are produced. The adapter
// that implements Executor for a provider knows that provider's wire format
// and nothing about HTTP responses to clients; the adapter that implements
// Sink knows the client's framing and nothing about which provider produced
// the bytes. That split is routing.md's hard rule restated as types: an
// adapter never sees a fallback decision, and the router never parses a
// provider payload.
//
// The sink's interface carries two halves with different callers. An
// executor writes through Content, and Content's error is its instruction to
// stop — the caller's connection is gone or the write failed, and no further
// content can be served. The routing stage reads Committed and Delivered
// after the call returns, to classify the attempt and settle what the client
// actually received. An executor that called either would be reaching past
// the routing stage into transport state; neither method exists for it.
//
// Commitment — the gateway's one irreversible moment — is the sink's own
// fact: the first Content call arms the answer's status line, and from there
// no status can be revised. The executor never announces commitment and the
// routing stage never asks before the call returns; the transport, which
// owns the wire, is the only side that knows when the crossing happened.
package executors

import (
	"context"
	"encoding/json"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// Executor performs one upstream call for the routing stage: the whole call,
// including whatever retrying that provider's own errors deserve — retrying
// inside the call is the adapter's judgment about its provider, while moving
// to another candidate is the routing stage's judgment about the alias. The
// call's answer is delivered through the sink as it is produced; the returned
// Result is the call's end, delivered once, after everything the sink
// received.
//
// Execute returns a non-nil Result on every path. A nil result would leave
// the routing stage a call with no verdict to record and no ending to state;
// the router treats one as a broken contract and classifies it as an
// unreadable upstream answer rather than guessing at what the executor meant.
type Executor interface {
	Execute(ctx context.Context, spec AttemptSpec, sink Sink) Result
}

// AttemptSpec is one candidate's call as the routing stage hands it in:
// the admitted request's bytes and the catalog's answer of where and how to
// send them. Everything here is a copy of a decision already made — the body
// is idempotency's digest subject, the model and overrides are the alias's
// candidate row — and an executor may keep none of it past the call: the
// spec is a mandate for one call, not a lease on the request.
type AttemptSpec struct {
	// RequestID is the runtime identity of the request the call belongs to,
	// as the attempt row and every usage fact will carry it.
	RequestID identity.RequestID

	// AttemptID is the identity minted for this one call. The executor does
	// not persist the attempt — appending is the routing stage's write, made
	// after the call finishes (ADR 0001 rule 4) — but the id travels in so
	// the executor's telemetry can name the call it observed.
	AttemptID identity.AttemptID

	// BackendID is the backend this call goes to, as the catalog configures
	// it. The executor resolves the backend's endpoint and credentials
	// reference itself; the spec carries the name, never the secret.
	BackendID string

	// ProviderModel is the model name the candidate row carries — the
	// provider's own vocabulary, already resolved from the caller's alias.
	ProviderModel string

	// ParameterOverrides is the candidate's operator-supplied JSON object,
	// passed through exactly as stored. Its meaning is the provider's; the
	// gateway never interprets it, and the executor's only obligation is to
	// carry it into the request unaltered. Nil is absence.
	ParameterOverrides json.RawMessage

	// Stream says whether the caller asked for the answer as a stream, so
	// the executor knows which of the provider's two answer shapes to ask
	// for. The framing of what comes back is the sink's business either way.
	Stream bool

	// Body is the admitted request's raw bytes, verbatim — the bytes the
	// request's digest was taken over. The executor translates what they
	// mean into its provider's request shape; it never logs them and never
	// stores them.
	Body []byte
}

// Result is one upstream call's end: an answer delivered whole, or a fault
// classified in the attempt vocabulary's error classes. It is a closed pair —
// Success and Failure are the only implementers — because the routing stage
// switches over exactly these two, and a third shape would be a classification
// it has no disposition for.
//
// There is deliberately no retryable flag. Whether a failure may be retried
// is routing's decision, and routing decides it from the error class alone
// (routing.md's disposition table); an executor's opinion beside the class
// would be a second policy where the catalog expects one.
type Result interface {
	result()
}

// Success is a call whose answer was produced and delivered through the sink.
// The usage is the provider's own report when it gave one; nil fields are the
// executor confessing the provider said nothing, and are settled from what
// the gateway observed instead.
//
// Delivery tokens are absent on purpose: what reached the client is the
// transport's observation, counted at the sink, and an executor's estimate of
// it would be a claim about bytes it never saw leave the process.
type Success struct {
	Usage             Usage
	ProviderRequestID string
}

// Usage is the token counts a provider reported for one call. Each field
// stands alone: a provider may report input without output or the reverse,
// and nil means unreported, never zero — zero is a claim and nil is silence,
// and the settlement keeps the distinction.
type Usage struct {
	InputTokens  *int64
	OutputTokens *int64
}

// Failure is a call that ended without a servable answer, classified into the
// attempt vocabulary's error classes. The class is the routing stage's only
// input to its disposition — fall through to the next candidate, or surface
// the refusal — which is why an unknown class is refused where the Result is
// formed, not discovered by the router's switch falling through.
type Failure struct {
	// Class is the fault, in execution's closed vocabulary. Post-commitment
	// failures carry it too; the router re-derives the attempt's own class
	// there, because after commitment the only outcome the vocabulary
	// records is the stream's failure, whatever the provider called it.
	Class execution.ErrorClass

	// ProviderError is the provider's own account of the fault — the error
	// envelope worth keeping for debugging, capped and never interpreted.
	// The gateway's wire answers never carry it.
	ProviderError json.RawMessage

	// ProviderRequestID is the provider's correlation handle for the call,
	// kept for the attempt row's telemetry. It names no candidate in any
	// log line this runtime writes.
	ProviderRequestID string

	// Usage is what the provider reported before the fault, when it reported
	// anything — a mid-stream death may arrive after a usage frame. Nil
	// fields are unreported, as in Success.
	Usage Usage
}

func (Success) result() {}
func (Failure) result() {}

// Sink is the answer's channel back through the transport. The transport
// builds one per request over the caller's connection; the routing stage
// passes it to every executor it calls, so every candidate's answer travels
// the same channel the final answer would have.
type Sink interface {
	// Content carries one piece of the answer's content-bearing bytes toward
	// the client. For a streamed answer these are the payload pieces between
	// the transport's frames; for a body answer it is the body itself,
	// delivered once. The first call arms the answer's status line — after
	// it, no status can be revised and no other candidate can be tried.
	//
	// A nil error means the piece left the process. A non-nil one is the
	// executor's instruction to stop: the caller is gone or the connection
	// failed, no further content can be served, and the executor returns its
	// Failure with whatever classification the stop deserves. The executor
	// must not call Content again after an error.
	Content(chunk []byte) error

	// Committed reports whether the answer has crossed its commitment point.
	// The routing stage reads it after a call returns; it is the fallback
	// gate — a committed answer is final, whatever fault followed.
	Committed() bool

	// Delivered is the content-bearing bytes that actually left the process,
	// the gateway's own observation of what the client received. The routing
	// stage reads it after a call returns, to settle a post-commitment death
	// on what was forwarded rather than on what the provider generated.
	Delivered() []byte
}

// Registry is the executors the composition root registered, by backend id.
// It is read-only after construction and safe for concurrent use; the routing
// stage consults it once per candidate, and a backend with no executor is a
// candidate that cannot be served — eligibility's registration leg.
type Registry interface {
	// For returns the executor registered for a backend, and whether one is.
	For(backendID catalog.BackendID) (Executor, bool)
}

// NewRegistry builds a registry over its entries. The map is copied, so the
// caller keeps no handle into the registry's state; an executor registered
// under no backend would be unreachable, and a nil one would be a routing
// stage one call away from a nil dereference — both are wiring defects, and
// this is the last place they can be refused loudly.
func NewRegistry(entries map[catalog.BackendID]Executor) Registry {
	registered := make(map[catalog.BackendID]Executor, len(entries))
	for id, executor := range entries {
		if id == "" {
			panic("executors: NewRegistry requires every executor to name its backend")
		}
		if executor == nil {
			panic("executors: NewRegistry requires a non-nil executor for backend " + string(id))
		}
		registered[id] = executor
	}
	return mapRegistry(registered)
}

// mapRegistry is the one Registry implementation this build needs: a map
// written once, at construction, and read by every routing decision since.
type mapRegistry map[catalog.BackendID]Executor

func (m mapRegistry) For(backendID catalog.BackendID) (Executor, bool) {
	executor, ok := m[backendID]
	return executor, ok
}
