package persistence

import (
	"context"
	"time"
)

// The Control Plane's side of the usage-fact seam, in the store's vocabulary.
//
// The direction is Data → Control (ADR 0006 §5): the runtime records immutable
// facts about what it served, the Control Plane reads them and derives what a
// customer owes. Two things have to be durable for that to survive a crash, and
// they are the two halves of this file —
//
//   - the facts the Control Plane has derived from, at least in the form of the
//     effects they produced. FactApplier is where those effects land, and it is
//     idempotent per (request_id, kind class), which is what makes redelivery
//     free;
//   - the position in the feed it has applied *through*. IngestionCursor is
//     that position, and it is the Control Plane's own: the Data Plane never
//     learns it (shared/usage-facts.yaml, and ADR 0006 §5's no-acknowledgement
//     rule).
//
// The two must move together. Applying a page and advancing the cursor are one
// unit of work, so a failure leaves both untouched and the same page is read
// again; advancing first, or advancing after a partial apply, is a position
// that claims facts were derived from that never were. The store's WithinTx is
// what makes "one unit" true rather than intended, and the application that
// drives it — application.FactIngestion — is the only caller that has to
// respect that.
//
// The cursor is opaque in here too. This port stores a string and hands the
// same string back; parsing it, ordering by it, or validating its shape is a
// rule the whole Control Plane keeps, not a rule about the wire alone.

// Fact is one usage fact as the Control Plane's store records it. It is this
// port's own vocabulary, translated from the seam's by the application, and it
// carries the envelope whole — the typed settlement figures beside the payload,
// because the contract fixes that a settlement must be derivable from the fact
// alone. How they are stored is the schema PR's business; the pointers here
// carry the fact's own claim about absence, so an applier can tell "no such
// claim" (nil) from a stored zero without re-reading the payload.
type Fact struct {
	AppendSeq     int64
	RequestID     string
	Kind          string
	SchemaVersion int
	OccurredAt    time.Time
	Payload       []byte

	CaptureMethod        *string
	CommittedAttemptID   *string
	ProviderInputTokens  *int64
	ProviderOutputTokens *int64
	DeliveryTokens       *int64
	PriceRevision        *string
	InputUnitPrice       *int64
	OutputUnitPrice      *int64
	SettledAmount        *int64
	CorrectsAppendSeq    *int64
}

// IngestionCursor is the Control Plane's own position in the fact feed.
//
// It is a port of its own rather than a table named in the store, because the
// application has to be able to read and move the position without knowing how
// it is stored — and because a position is the one thing about this flow the
// Data Plane never sees. Its implementation is the schema PR's; the shape here
// exists so that the replay use case can be written, and tested, before the
// table it will sit in.
type IngestionCursor interface {
	// Position returns the cursor the consumer has durably applied through,
	// or "" when it has never applied anything.
	//
	// The empty string is this port's own convention and the only meaning the
	// Control Plane assigns to a cursor: it is what makes a first run request
	// the feed from the beginning of what the Data Plane still retains. It is
	// never a value the Data Plane issued, and no other string is compared to
	// anything.
	Position(ctx context.Context) (string, error)

	// Advance records next as the applied-through position. It runs inside
	// the caller's unit of work, and an implementation must refuse a call
	// that arrives without one — an advance is only meaningful in the same
	// transaction as the work it claims to sit after, and outside that
	// transaction it is the claim without the work.
	//
	// The obligation this signature cannot state, and which the caller carries:
	// Advance is called after every fact the position covers has been applied,
	// never before. A position is a claim about work already done, and the
	// store cannot check the claim — it can only record it in the same
	// transaction as the work, which is what makes the claim true or false
	// together with the effects it speaks for.
	Advance(ctx context.Context, next string) error
}

// FactApplier applies the facts the Control Plane derives from.
//
// It takes this port's Fact rather than the seam's Event so the derivation
// lives behind the port, where the schema that gives a payload its meaning is.
// The obligation the signature cannot state is idempotency, and the contract
// is kind-classed rather than by request_id alone: the same fact is expected
// to arrive more than once — replay is normal, not exceptional, and the same
// range may be read any number of times — and applying a fact whose
// (request_id, kind class) has been applied must leave exactly one settlement,
// one consume leg and one release leg behind. A different kind for the same
// request_id is not a duplicate: the feed can carry a request's settlement
// fact and an orphan fact beside it (shared/usage-facts.yaml), each with its
// own effect, and a request_id-keyed refusal would drop the second one
// forever. One class's effect may never cause the other's — a settlement
// fact must never settle twice, an orphan must never consume or release twice
// — and no fact of any kind may make money move twice.
//
// Idempotency has a fail-closed half. A fact the applier cannot interpret —
// a kind or schema_version this build does not implement, a settled fact
// whose figures do not cohere, a correction whose path it has not built —
// must come back as an error, never as a nil it pretends was a replay.
// The caller's whole-page rule turns that error into a stop the operator
// sees; a silent no-op turns it into a fact that was in the feed, is past
// the position, and was derived from by nothing. The contract fixes what
// the feed may carry (shared/usage-facts.yaml); the applier is the only
// code that knows what it implements, so the refusal belongs here and the
// vocabulary it refuses is part of this port's meaning, not the wire's.
type FactApplier interface {
	// Apply records fact. Applying a fact whose (request_id, kind class) has
	// already been applied must be a no-op, and it must never settle, consume
	// or release twice within a class — while a different kind for the same
	// request_id is a distinct fact with its own effect. A fact the applier
	// cannot interpret is an error, not a replay: the refusal is what stops
	// the page and holds the position for a human to resolve.
	Apply(ctx context.Context, fact Fact) error
}

// AppliedFact is one row of the idempotency ledger: the record that a fact
// of this (request_id, kind class) pair has been derived from, with the
// lineage the derived effect itself cannot carry — the feed position it
// arrived on, the amount and capture provenance it charged from, and the
// settlement of record it produced. It is what a reconciliation pass (B13)
// walks backwards from ledger to fact, and what the applier's read before
// every write consults.
type AppliedFact struct {
	// RequestID and KindClass are the row's identity, and the fact's
	// exactly-once boundary.
	RequestID string
	KindClass string

	// Kind is which of the class's facts landed. Within the settlement
	// class the first terminal fact to arrive decides this; an orphan's
	// row is always its own kind.
	Kind string

	// AppendSeq is the feed position the fact arrived on — provenance,
	// never an idempotency key.
	AppendSeq int64

	// SettledAmount is what the settled fact charged; nil on every other
	// kind, the same shape statement the schema's CHECK pins.
	SettledAmount *int64

	// CaptureMethod is the usage claim's provenance, on the kinds that
	// claim one (settled, unbillable_orphaned); nil on the rest.
	CaptureMethod *string

	// SettlementID is the settlement of record a settled fact produced,
	// or "" on every other kind.
	SettlementID string
}

// AppliedFacts is the idempotency ledger the applier reads before it writes.
//
// The read is not an optimisation. Convergence at the accounting layer would
// absorb a replayed fact's movements one primitive at a time, but the
// readable answer to "has this class been derived from" is what turns a
// replay into a single no-op — one query against this ledger instead of a
// walk through every primitive's convergence path — and it is what keeps a
// second delivery from re-deriving anything at all.
type AppliedFacts interface {
	// Find returns the applied row for (requestID, kindClass), or nil when
	// no fact of that class has been derived from. It may run inside or
	// outside a unit of work; inside one, it sees that unit's own earlier
	// writes.
	Find(ctx context.Context, requestID, kindClass string) (*AppliedFact, error)

	// Record inserts applied as a new row of the ledger. It runs inside the
	// caller's unit of work and refuses a call that arrives without one —
	// the row is the second half of the exactly-once boundary, and recording
	// it outside the transaction that booked the effect would be a claim
	// that outlives the work it speaks for. A row already present for the
	// pair is a defect above this port: the applier's Find is the check that
	// makes the insert unloseable, and the store's primary key is the
	// arbiter of the one race Find cannot close (two workers deriving the
	// same fact concurrently — the loser's whole page rolls back, and its
	// retry finds the row).
	Record(ctx context.Context, applied AppliedFact) error
}

// QuarantinedFact is one refused fact, recorded verbatim: every column the
// feed delivered it with, beside the refusal reason. The record is the
// explicit disposition for a fact the consumer cannot apply — the
// operator-visible state and the reconciliation surface — and never a silent
// skip: under-recording is the conservative direction, because a fact that
// is never applied never charges.
type QuarantinedFact struct {
	RequestID string
	AppendSeq int64
	Kind      string
	// SchemaVersion travels even when it is the reason for the refusal: an
	// unknown version is exactly the fact a reconciliation pass needs to
	// see whole.
	SchemaVersion int
	OccurredAt    time.Time
	// Payload is the fact body verbatim, undecoded. Nothing downstream
	// interprets it here; the record exists so nothing is lost by refusing.
	Payload []byte

	// The typed columns, verbatim, nil where the fact carried null.
	CaptureMethod        *string
	CommittedAttemptID   *string
	ProviderInputTokens  *int64
	ProviderOutputTokens *int64
	DeliveryTokens       *int64
	PriceRevision        *string
	InputUnitPrice       *int64
	OutputUnitPrice      *int64
	SettledAmount        *int64
	CorrectsAppendSeq    *int64

	// Reason is the consumer's own refusal vocabulary — why this fact was
	// not applied. It is a diagnostic written for the operator who resolves
	// the quarantine, not a machine contract, and an implementation may
	// bound its length to what the schema records.
	Reason string
}

// QuarantinedFacts is where a refused fact is recorded.
type QuarantinedFacts interface {
	// Record records quarantined verbatim. It runs inside the caller's unit
	// of work — the same transaction that advances the position past the
	// fact, so the record and the advance stand or fall together — and
	// refuses a call that arrives without one.
	//
	// Recording a fact whose (request_id, append_seq, kind) is already on
	// file converges to a no-op: the refusal is already recorded, and the
	// original record stands. The only path that re-delivers a quarantined
	// fact is a position that moved backwards — a restored cursor replaying
	// a range the feed already refused through — and a second refusal of
	// the same fact is the same sentence about it, not a second fact.
	Record(ctx context.Context, quarantined QuarantinedFact) error
}
