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
//     idempotent by request_id, which is what makes redelivery free;
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
// port's own vocabulary, translated from the seam's by the application.
type Fact struct {
	RequestID     string
	Kind          string
	SchemaVersion int
	OccurredAt    time.Time
	Payload       []byte
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
	// the caller's unit of work.
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
// The obligation the signature cannot state is idempotency: the same fact is
// expected to arrive more than once — replay is normal, not exceptional, and
// the same range may be read any number of times — and applying it twice must
// leave exactly one settlement, one consume leg and one release leg behind.
type FactApplier interface {
	// Apply records fact. Applying the same request_id twice must be a no-op,
	// and it must never settle, consume or release twice.
	Apply(ctx context.Context, fact Fact) error
}
