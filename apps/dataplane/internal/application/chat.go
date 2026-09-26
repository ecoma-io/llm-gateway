// The admission boundary: the vocabulary the chat completion transport and
// the use cases behind it meet at.
//
// This file is the seam of one endpoint. HTTP hands the runtime's request in
// as a ChatInput and reads a ChatOutcome back; neither side names the other's
// shape — no status code appears here, and no JSON field appears in the use
// case. The outcome kinds are the decisions the pipeline can reach before an
// answer exists: a request is admitted, refused for a named reason, found
// already in flight under its idempotency key, refused as a conflict with
// that key's history, or answered again from a decision already made. What
// the wire does with each decision is the transport's translation, and the
// transport is the only place that owns it.
//
// The vocabulary is deliberately small enough to read whole. A rejection
// carries the admission step that refused it — execution.RejectionReason, the
// same vocabulary the rejected request row stores — and, when the refusal is
// about one field of the request, the detail naming that field. A replay
// carries the original request's identity, because the answer is that
// request's decision said again; an unanswered decision says the original
// this key names ended without producing an answer, and the key with it.
// Everything a later stage of the pipeline
// needs from an admitted request rides in Admission, and nothing in it is
// interpreted here: admission's caller decides what an admission means.
package application

import (
	"context"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/executors"
)

// ChatCompletion is the admission use case the chat completion transport
// calls: admit one request, or account for why it was not admitted.
//
// Serve is called once per HTTP request. Its context is that request's — when
// the caller is gone, admission stops. A non-nil error means the use case
// could not reach a decision at all — a store that would not answer, an
// invariant that broke — and the transport answers it as an internal failure;
// every decision admission CAN reach, including every refusal, arrives as an
// outcome with a nil error.
type ChatCompletion interface {
	Serve(ctx context.Context, in ChatInput) (ChatOutcome, error)
}

// Authenticator is the first half of admission, called before any admission
// work and answerable without one: resolve the credential a request presented
// to the key identity it names, or refuse it.
//
// It sits beside ChatCompletion rather than inside it because its refusals
// write nothing — not a request row, not an intake record. A credential that
// cannot be verified never became a request, so it never enters the vocabulary
// ChatOutcome carries; the transport answers the refusal itself, with one
// message for every cause. A nil error carries the verified key identity; a
// refusal arrives as *Unauthenticated; any other error is the mirror's own
// failure and is answered as an internal one.
type Authenticator interface {
	Authenticate(ctx context.Context, credential string) (AuthenticatedCredential, error)
}

// AuthenticatedCredential is what verification concluded: the key identity
// behind the presented secret, and the account facts the same mirror read
// carried beside it. The presented secret itself never crosses this boundary —
// by the time it is resolved to a key id it has been reduced to a digest
// comparison the runtime keeps no copy of.
type AuthenticatedCredential struct {
	// KeyID is the identity of the api_key_credentials row the presented
	// secret verified against. It is the credential a rejection row records.
	KeyID string

	// AccountID is the identity of the account the credential belongs to, as
	// the mirror's credential row states it. It is the owner every admission
	// write keys on — the request row, the replay record and the drawdown all
	// name this account, never one re-derived elsewhere.
	AccountID string

	// AccountState is the owning account's lifecycle state as the mirror read
	// spelled it, nil only when the account row is absent — the
	// integrity-violation case verification refuses closed. It rides beside
	// the key identity because the mirror delivers both in one statement: the
	// use case judges exactly the lifecycles verification saw, never a
	// re-read that could straddle a projection apply.
	AccountState *string
}

// UnauthenticatedReason names why a credential was refused. The values are
// server-side facts for the log — the wire answer is identical for every one
// of them, which is the point — so they are never serialized to a client.
type UnauthenticatedReason string

const (
	// ReasonCredentialUnknown says no credential row carries the presented
	// key's id — a key that was never minted or was projected away.
	ReasonCredentialUnknown UnauthenticatedReason = "credential_unknown"

	// ReasonCredentialRevoked says the key exists but its lifecycle state is
	// not one that may admit requests.
	ReasonCredentialRevoked UnauthenticatedReason = "credential_revoked"

	// ReasonAccountAbsent says the credential row exists but its owning
	// account row does not — a mirror-integrity violation, unreachable from a
	// conforming projection, and refused closed rather than served through.
	ReasonAccountAbsent UnauthenticatedReason = "account_absent"
)

// Unauthenticated is the typed refusal of a presented credential. The
// transport answers every reason with the same wire failure; the reason
// exists so the log line can say what an operator — never a client — acts on.
type Unauthenticated struct {
	Reason UnauthenticatedReason
}

// Error implements error, with one fixed sentence for every reason: the
// refusal is not a per-cause message, it is a classification.
func (u *Unauthenticated) Error() string {
	return "the presented credential is not a key this runtime will admit requests for"
}

// ChatInput is one request as the transport hands it in.
type ChatInput struct {
	// RequestID is the runtime-minted identity of this request — not the
	// caller's X-Request-Id, which is a transport correlation handle and
	// never crosses here. It is minted fresh per HTTP request; the use case
	// mints its own per retry of its unit of work.
	RequestID identity.RequestID

	// Credential is the key identity the request's credential verified
	// against, as Authenticator returned it. The presented secret is not this
	// field and never was.
	Credential string

	// AccountID is the account the verified credential belongs to, as
	// Authenticator returned it. Every write admission makes keys on this
	// owner.
	AccountID string

	// AccountState is the owning account's lifecycle as verification saw it,
	// nil only on the integrity-violation shape verification already refused.
	// The use case gates on this copy of the fact rather than re-reading the
	// mirror, so the gate and the verification answer one instant of the
	// mirror, not two.
	AccountState *string

	// IdempotencyKey is the request's key, byte-for-byte as presented — no
	// trimming, no folding. Empty when the header was absent; the use case
	// refuses a request without one, because a replay guarantee cannot be
	// offered for a request that named no key.
	IdempotencyKey string

	// Body is the request body as the transport read it — the raw bytes when
	// they were read, and the reason they were not when they were not. The
	// use case decides what each means for admission; the transport decides
	// nothing.
	Body RequestBody

	// Reply is the answer's channel, built by the transport over this
	// request's connection. Admission's own outcomes are complete answers in
	// themselves and never touch it; the routing stage delivers an admitted
	// request's answer through it, which is why it is required — an input
	// without one is a wiring defect the use cases refuse rather than an
	// answer they cannot write.
	Reply Reply
}

// RequestBody is the request body, read or refused. The split exists because
// the two cases are different requests to admission: bytes can be digested
// for idempotency, a refusal cannot — there is nothing to decide a replay
// against — and the refusal's reason is a transport fact the use case
// records, not one it guesses at.
type RequestBody interface {
	requestBody()
}

// BodyBytes is a body the transport read whole, within its bound.
type BodyBytes []byte

func (BodyBytes) requestBody() {}

// BodyRefused is a body the transport could not deliver, with the reason.
type BodyRefused struct {
	// Reason is why the body is not here: over the transport's bound, or
	// unreadable before it was whole.
	Reason BodyRefusal
}

func (BodyRefused) requestBody() {}

// BodyRefusal names why a request body never arrived as bytes.
type BodyRefusal string

const (
	// BodyTooLarge says the body exceeded the transport's bound mid-read.
	// The bound is the one the contract documents; it is a constant, not a
	// knob, so a request's answer does not depend on how a process was
	// configured.
	BodyTooLarge BodyRefusal = "too_large"

	// BodyUnreadable says the body failed before it was whole for a reason
	// other than its size — a connection that broke mid-body, a reader that
	// errored.
	BodyUnreadable BodyRefusal = "unreadable"
)

// OutcomeKind is the class of decision admission reached.
type OutcomeKind string

const (
	// OutcomeAdmitted says the request was admitted: verified, entitled,
	// priced, and holding a live reservation on the spend it implies. The
	// request now belongs to the pipeline stage that routes it.
	OutcomeAdmitted OutcomeKind = "admitted"

	// OutcomeRejected says admission refused the request. Reason names the
	// admission step that refused it, and Detail names the request field at
	// fault when there is one.
	OutcomeRejected OutcomeKind = "rejected"

	// OutcomeInFlight says a request with this idempotency key and the same
	// body is still being processed. The caller retries the same request
	// with the same key.
	OutcomeInFlight OutcomeKind = "in_flight"

	// OutcomeConflict says this idempotency key was already stored with a
	// different body. The caller retries with a fresh key, never this one.
	OutcomeConflict OutcomeKind = "conflict"

	// OutcomeReplay says this key and body match a request already decided,
	// and the answer is that decision again. Original names the request
	// whose decision is being re-answered; its wire shape is the original's,
	// marked as a replay. A replayed request that FAILED answers with its
	// failure reason in Failure.
	OutcomeReplay OutcomeKind = "replay"

	// OutcomeUnanswered says this key and body match a request that ended
	// without producing an answer — the runtime abandoned its own walk before
	// any content existed, and nothing was ever delivered to re-serve. The
	// key is spent: the record is terminal, so no arrival under it can ever
	// be executed, and there is no answer to re-serve. The caller opens a
	// fresh key; Original names the request whose abandonment is being
	// accounted for.
	OutcomeUnanswered OutcomeKind = "unanswered"

	// OutcomeServed says the answer travelled to the client through the
	// request's reply while the routing stage ran — a completed answer, or
	// one that failed after commitment with its failure frame already
	// written into the stream. The transport writes nothing further; what
	// happened is in Routing and, for a stream that ended in failure, in the
	// bytes already gone.
	OutcomeServed OutcomeKind = "served"

	// OutcomeRefused says a candidate's upstream refused the request before
	// any content was committed, and the refusal is surfaced to the caller —
	// the one pre-commitment ending that is an answer, not a retry. Failure
	// names the refusal; the cell it answers with is the transport's table.
	OutcomeRefused OutcomeKind = "refused"

	// OutcomeAbandoned says the routing walk stopped because the caller's
	// context ended before any candidate produced an outcome. Nothing is
	// finalised and nothing can be written — there is no channel left — so
	// the request stays executing for the reaper, the same shape a dead
	// process leaves, and the transport writes nothing.
	OutcomeAbandoned OutcomeKind = "abandoned"
)

// Reply is the answer's channel: the transport builds one per request over
// the caller's connection, and the routing stage delivers an admitted
// request's answer through it. It extends the executors port's Sink — the
// channel an executor writes content into — with the endings only the routing
// stage may call, so the bytes of every ending travel the one channel and the
// transport owns their framing in one place.
//
// The endings are named for what happened, never for what the bytes look
// like: the transport's table (its wire map) decides status, body and
// headers, and this interface never carries one.
type Reply interface {
	// Sink is the channel's executor-facing half: the routing stage hands
	// the reply to every Executor it runs, and the executor writes content
	// into it — the commitment point among those calls.
	executors.Sink

	// Open arms the reply for its answer: stream says whether the answer
	// travels as a stream or as one body. The routing stage calls it before
	// the first executor runs, because the framing is decided before the
	// first byte is chosen.
	Open(stream bool)

	// ServeSucceeded closes a delivered answer. For a stream that is the
	// terminal frame; for a body answer it is nothing, because the body was
	// the whole answer.
	ServeSucceeded()

	// ServeMidStreamFailure writes a post-commitment failure into the stream
	// it belongs to: the failure frame, then the terminal frame, then close.
	// It has no meaning for a body answer, because a body answer is written
	// once and complete.
	ServeMidStreamFailure()

	// ServeSurfaced answers a pre-commitment surfaced refusal: the refusal's
	// own cell, status and all, nothing having been written before it.
	ServeSurfaced(failure execution.FailureReason)

	// ServeNoCandidate answers the no-candidate ending: the runtime could
	// not route the request, the hold was released unused, and the caller
	// retries later rather than differently.
	ServeNoCandidate()
}

// RejectionDetail names the request field a rejection is about, when one
// field is at fault. The values are the request's own field names — the
// transport renders them into the wire body's `param`, and the use case never
// formats a client-facing sentence it has no business owning.
type RejectionDetail string

const (
	// DetailNone says the rejection is not about one field: the body as a
	// whole, the request's arithmetic, or nothing the caller sent.
	DetailNone RejectionDetail = ""

	// DetailModel says the model field is at fault.
	DetailModel RejectionDetail = "model"

	// DetailMaxTokens says the max_tokens spelling of the output ceiling is
	// at fault — absent, non-positive, or disagreeing with its twin.
	DetailMaxTokens RejectionDetail = "max_tokens"

	// DetailMaxCompletionTokens says the max_completion_tokens spelling of
	// the ceiling is at fault.
	DetailMaxCompletionTokens RejectionDetail = "max_completion_tokens"

	// DetailIdempotencyKey says the Idempotency-Key header is at fault —
	// absent, or outside the grammar the runtime will store.
	DetailIdempotencyKey RejectionDetail = "idempotency_key"
)

// ChatOutcome is one decision admission reached.
type ChatOutcome struct {
	// Kind is the class of the decision.
	Kind OutcomeKind

	// Reason is the admission step that refused the request, set on
	// OutcomeRejected and on OutcomeReplay — a replay re-answers the
	// original's decision, and the reason is how the transport knows which
	// answer that is.
	Reason execution.RejectionReason

	// Detail names the request field the rejection is about, set when the
	// refusal concerns one.
	Detail RejectionDetail

	// Original is the runtime identity of the request whose decision a
	// replay re-answers.
	Original identity.RequestID

	// Failure is the upstream refusal an ending or a replay names: set on
	// OutcomeRefused, where it is the refusal this arrival was just served,
	// and on OutcomeReplay of a failed original, where it is the refusal the
	// original was served and this arrival is answered with again. The three
	// values it can carry are the surfaced refusals — the pre-commitment
	// endings whose answers are ordinary HTTP.
	Failure execution.FailureReason

	// RuntimeRequestID is the runtime identity of the arrival this outcome
	// answers — the attempt identity the decision was actually reached under:
	// the transport-minted id for every decision made before a unit of work
	// opens (the probe, the early refusals), the unit's own attempt id for
	// everything decided inside one. It is set on every decision, and the
	// transport logs it beside the answer so a caller's report resolves to
	// exactly the arrival that produced it.
	RuntimeRequestID identity.RequestID

	// Admitted carries what an admitted request hands forward. It is set
	// exactly on OutcomeAdmitted.
	Admitted *Admission

	// Routing carries what the routing stage did with an admitted request,
	// set on OutcomeServed, OutcomeRefused and OutcomeAbandoned and nil on
	// every decision admission reached alone. The transport logs it beside
	// the answer; it names no candidate, no provider model and no provider
	// handle — a wire-adjacent line is not the place for a provider's
	// identity.
	Routing *RoutingTrace
}

// RoutingTrace is what the routing stage did, for the log line that answers
// for it. The counts are the walk's own — how many candidates were tried,
// where the walk stopped, what the last failure was classified as, and
// whether an answer crossed its commitment point — and they are the whole of
// what a correlation question needs: which alias, how deep the walk went,
// what stopped it, did the client get bytes.
type RoutingTrace struct {
	// Alias is the alias the walk resolved.
	Alias string

	// Attempts is how many candidates the walk tried, in total.
	Attempts int

	// LastPosition is the catalog position of the last candidate the walk
	// tried — 1 for a first-try answer, higher when the walk fell through —
	// and zero when no candidate was tried at all.
	LastPosition int

	// LastErrorClass is the class the last failure was classified as, empty
	// when the walk ended any other way.
	LastErrorClass execution.ErrorClass

	// Committed reports whether an answer crossed its commitment point —
	// the difference between a request whose failure is a surfaced refusal
	// and one whose failure reached the client as bytes.
	Committed bool
}

// Admission is what an admitted request hands to the pipeline stage that
// routes it: everything admission decided, frozen at the moment it decided
// it.
type Admission struct {
	// RawBody is the request body as admitted — the bytes idempotency
	// digested.
	RawBody []byte

	// Stream is whether the caller asked for the answer as a stream.
	Stream bool

	// Price is the price basis admission priced the hold under, frozen so a
	// later price edit cannot rewrite what this request reserved.
	Price catalog.PriceSnapshot

	// Hold is the amount reserved against the caller's capacity, in minor
	// units.
	Hold int64

	// InputTokens is the input count admission priced the hold from — the
	// gateway's own count of the request it read. A settled ending reports
	// the provider's input figure when the provider gave one; this is what it
	// claims when none arrived, so a settlement never needs to re-count a
	// body the admission already counted.
	InputTokens int

	// OutputBasis is the output count the hold was sized against — the bound
	// admission put between the request and its answer. Seen from the provider
	// side it is a ceiling; seen from the settlement side it is the floor the
	// settled figure cannot fall below without undercharging and cannot rise
	// above without overcharging: a settled ending reports the provider's
	// output figure bounded by it, and claims the basis itself when no figure
	// arrived, so what a settlement claims is always a number the hold
	// already funded.
	OutputBasis int

	// ReservationID is the live reservation the request holds.
	ReservationID identity.ReservationID

	// Alias is the name of the alias the request was admitted against — the
	// name the routing stage resolves to its candidate list. The stage
	// re-reads the alias rather than trusting a snapshot of it: selection is
	// a routing-time judgment over the catalog as it stands, and an alias
	// edited between admission and routing is routed by its current list.
	Alias string

	// Legs is the waterfall split the hold was drawn down against, in the
	// stored ordinal order. The release ending returns them and the settle
	// ending closes them out in its fact; both need the split exactly as it
	// was granted, never re-derived.
	Legs []accounting.Allocation

	// LeaseOwner and LeaseExpiresAt are the reservation lease's holder and
	// deadline — the facts that let the runtime tell a reservation its
	// process still owns from one it must reclaim.
	LeaseOwner       string
	LeaseExpiresAt   time.Time
	RuntimeRequestID identity.RequestID
}
