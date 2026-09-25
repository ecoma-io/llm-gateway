package dataplane

import (
	"context"
	"errors"
)

// The Control → Data half of the cross-plane seam (ADR 0006 §5, ADR 0007):
// the projection delivery protocol, as the Control Plane's producer speaks
// it to the Data Plane's credential mirror.
//
// The messages this interface carries are assembled by the projection domain
// (internal/domain/projection) and cross as bytes: the producer is the
// message's author, the domain is the grammar, and the adapter below this
// port is a transport. Nothing in the application or the adapter re-reads
// the bytes on the way out, because there is no second opinion to have about
// a message the same process just built and validated — and keeping the port
// byte-shaped is what makes the producer's grammar the only grammar on this
// side, the same discipline the façade keeps on the other side of the hop,
// where re-encoding a delivery would strip exactly the unknown additive
// fields the protocol's evolution rule promises to carry.
//
// The five failures a method here can report, and why each is its own
// sentinel — the delivery loop's every decision is a decision about one of
// them, and a loop that cannot tell them apart either skips deliveries it
// must not or halts on ones its own next cycle would resolve:
//
//   - ErrProjectionUnsupportedVersion — the other side speaks a protocol
//     version this build does not know, or answered in one. Never retried
//     into submission, never worked around: the version set is closed, so
//     the only exits are an upgrade or an operator.
//   - ErrProjectionShape — the answer did not carry the fields the contract
//     requires, or a delivery was refused as malformed. The producer built
//     the message, so this is this process's defect: loud, and never a
//     reason to skip ahead.
//   - ErrProjectionGap — the delivered batch does not join the consumer's
//     stored position. Self-healing: the answer to a position that cannot
//     join this timeline is a snapshot.
//   - ErrProjectionSnapshotRequired — the consumer holds a position from a
//     timeline this producer is no longer on. Self-healing the same way.
//   - ErrProjectionUnavailable — transport failed, the status was not one
//     the operation declares, the answer would not decode, or the refusal
//     carried no code this protocol names. The answer is unknown, the
//     delivery will happen again, and nothing durable was harmed: at-least-
//     once delivery means the next cycle is a correct response to all of it.
type Projection interface {
	// ProjectionPosition reads the consumer's position: its protocol version,
	// the timeline it was earned on, whether it has bootstrapped at all, and
	// the highest revision it has applied and committed. The producer's
	// bootstrap decision is made from this and from the log's head, and from
	// nothing else.
	ProjectionPosition(ctx context.Context) (ProjectionPosition, error)

	// DeliverSnapshot delivers one bootstrap message and returns the
	// consumer's acknowledgement: its position after applying, which is the
	// snapshot boundary the message named. Application is unconditional on
	// the consumer's side, so the only interesting answers are the ack and
	// the failure.
	DeliverSnapshot(ctx context.Context, message []byte) (ProjectionAck, error)

	// DeliverChanges delivers one incremental batch and returns the
	// consumer's acknowledgement — which is also the acknowledgement of a
	// batch delivered before, whose answer was lost: a redelivered batch is
	// answered without work (ADR 0007 §5), so the producer never needs a
	// separate "did it land?" call.
	DeliverChanges(ctx context.Context, message []byte) (ProjectionAck, error)
}

// ProjectionPosition is the consumer's answer to a position read. Every
// field is the consumer's own fact, carried across the seam verbatim; the
// producer compares and acts, and never corrects.
type ProjectionPosition struct {
	// ProtocolVersion is the version the answer was written for. A value
	// this build does not speak is refused by the adapter — an implementation
	// hands back only answers this package's version names.
	ProtocolVersion int
	// Epoch is the timeline the position was earned on, empty exactly when
	// the consumer has never bootstrapped. An empty value is data, not
	// absence: it is the unbootstrapped consumer's legitimate spelling.
	Epoch string
	// Bootstrapped says whether the consumer holds a projection at all. A
	// consumer that does not is offered a snapshot, not a batch — an
	// incremental batch needs a position to join and there is none.
	Bootstrapped bool
	// AppliedRevision is the highest revision whose effect the consumer has
	// applied and committed: the resume point and the value the producer
	// compares against the log's head to notice a position that has run past
	// the timeline it was earned on.
	AppliedRevision uint64
}

// ProjectionAck is the consumer's answer to a delivery: its position after
// the operation. There is no separate acknowledgement call — this answer is
// the acknowledgement, and a producer that never receives it simply
// delivers again.
type ProjectionAck struct {
	ProtocolVersion int
	AppliedRevision uint64
}

// The five sentinels, declared here because the application classifies on
// them and the adapter produces them — the port is where their contract
// lives, exactly as ErrCursorExpired and ErrMalformedPage are for the read
// half of the seam above.
var (
	// ErrProjectionUnsupportedVersion reports that the other side named a
	// protocol version this build does not speak, in an answer or in a
	// refusal of this build's own message.
	ErrProjectionUnsupportedVersion = errors.New("the data plane speaks a projection protocol version this process does not support")

	// ErrProjectionShape reports that an answer was not the shape the
	// contract requires — a required field absent or null, a body that would
	// not decode — or that a delivery was refused as malformed. What arrived
	// is known and wrong; what to do about it is a human's call.
	ErrProjectionShape = errors.New("the data plane answered outside the projection protocol's grammar")

	// ErrProjectionGap reports that a delivered batch does not join the
	// consumer's stored position — neither wholly behind it (the lost-ack
	// case) nor starting at its successor. The consumer applied none of it.
	ErrProjectionGap = errors.New("the data plane refused a batch that does not join its applied position")

	// ErrProjectionSnapshotRequired reports that the consumer's position
	// cannot join this producer's timeline — a foreign epoch, or a revision
	// the log will never re-issue. The answer is a snapshot, never guesses.
	ErrProjectionSnapshotRequired = errors.New("the data plane's stored position cannot join this producer timeline")

	// ErrProjectionUnavailable reports that this cycle's answer is unknown:
	// a transport failure, an unexpected status, an undecodable answer, or a
	// refusal carrying no code this protocol names. Delivery is at-least-once
	// and durable, so the honest response is to try the whole cycle again.
	ErrProjectionUnavailable = errors.New("the data plane's projection surface is unavailable")
)
