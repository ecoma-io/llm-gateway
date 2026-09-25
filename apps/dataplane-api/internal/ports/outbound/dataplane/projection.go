package dataplane

import (
	"context"
	"errors"
)

// The projection half of the cross-plane seam: the operations this façade
// carries from the Control Plane's producer to the Data Plane's listener
// (ADR 0007). Where UsageFacts is the one read this application makes, these
// are the writes it fronts — and the shape of the interface is decided by the
// same sentence that decided the read's: this process is a transport, and
// everything here is the call, not the content.
//
// The delivery methods take the message as bytes, and that is a decision about
// correctness rather than about effort. The private protocol's grammar — which
// records a message must carry, which versions mean what — is the Data Plane's
// to judge; that is why the listener answers the refusals below and this
// process translates them. Decoding a delivery here to validate it would
// duplicate that grammar one hop up, and two definitions of one rule fail in
// the worst way a protocol can: independently. Worse, re-encoding a decoded
// message would strip the fields this build does not know, and additive
// tolerance is the one direction the protocol may evolve in — a consumer that
// upgrades before its producer must still receive the fields the producer
// added, and a middle hop that re-serializes what it parsed is precisely the
// hop that would drop them. So the bytes cross untouched, and the judgement
// happens exactly once, where the mirror is.
//
// The answers are the opposite case, and they get the usage-fact treatment:
// the position and the acknowledgement are small, closed shapes, the façade
// re-issues them from values, and every field the private protocol marks
// required is refused when absent — a position decoded into zero values would
// read as "unbootstrapped at revision zero", which is not an answer this
// process may invent on the producer's behalf.

// ProjectionPosition is the Data Plane's statement of where its mirror stands:
// the producer timeline it bootstrapped on, whether it has one, and the
// highest revision whose effects are committed there. It is the fact the
// producer's whole cycle decides from, and this port attaches no meaning to
// any part of it — not even to the empty epoch, which is a legitimate value:
// the unbootstrapped consumer's spelling of "offer a snapshot".
type ProjectionPosition struct {
	// ProtocolVersion is the version of the private protocol the answering
	// listener speaks. It crosses unchanged: which versions the producer may
	// send is decided by the two ends, and this process is neither of them.
	ProtocolVersion int
	// Epoch is the producer timeline the position was earned on, empty only
	// before the first snapshot.
	Epoch string
	// Bootstrapped reports whether the listener holds a projection at all.
	// False means the producer's answer is a snapshot, never a batch.
	Bootstrapped bool
	// AppliedRevision is the highest revision whose effects are committed.
	// An incremental batch must join it exactly; anything else is the
	// listener's refusal to classify, never this process's to pre-judge.
	AppliedRevision uint64
}

// ProjectionAck is the Data Plane's acknowledgement of a delivered message:
// the protocol version it answered under and the position it stands at after
// the operation. For a batch that applied, that is the batch's last revision;
// for the duplicate delivery the protocol's at-least-once expects, it is the
// position the earlier application already earned — which is the lost
// acknowledgement the producer was re-delivering to get.
type ProjectionAck struct {
	ProtocolVersion int
	AppliedRevision uint64
}

// The four refusals the private listener can answer a delivery with, each one
// translated rather than relayed: the adapter reads the listener's envelope,
// classifies it, and returns one of these. The producer's loop acts on the
// classification — a version it must upgrade for, a message it must rebuild,
// a gap a human decides about, a snapshot it delivers on its next cycle by
// itself — so each is distinct, and collapsing any two would take a decision
// away from the loop that has to make it.
var (
	// ErrProjectionUnsupportedVersion reports a delivery in a protocol version
	// the listener does not speak. The delivery halts until both ends speak
	// one version; it never skips.
	ErrProjectionUnsupportedVersion = errors.New("the data plane does not speak this projection protocol version")

	// ErrProjectionShape reports a message outside the private protocol's
	// grammar, as the listener judged it. The producer rebuilds from its own
	// log; the listener's detail stays on the listener.
	ErrProjectionShape = errors.New("the delivered projection message does not satisfy the protocol's grammar")

	// ErrProjectionGap reports a batch that does not join the position —
	// neither a duplicate of applied history nor its exact continuation. It is
	// refused whole, and resolving it is a human decision, because skipping
	// forward would strand the gap's revisions forever.
	ErrProjectionGap = errors.New("the delivered batch does not join the data plane's applied position")

	// ErrProjectionSnapshotRequired reports that the listener's stored
	// position cannot join the producer's timeline — a different epoch, or no
	// bootstrap at all. It is the one refusal the producer's loop resolves by
	// itself: the next cycle reads the position and offers a snapshot.
	ErrProjectionSnapshotRequired = errors.New("the data plane's position cannot join this producer timeline; it requires a snapshot")
)

// ProjectionDelivery is the write half of the cross-plane seam: the two
// deliveries and the one position read this application fronts for the
// Control Plane's producer.
//
// The message parameter is the delivered body as the producer wrote it, and an
// implementation carries it verbatim — the bytes that arrived are the bytes
// the listener must receive, for the reason the interface's comment gives.
// The errors are the seam's own vocabulary: ErrUpstreamUnavailable when no
// classified answer exists to hand back, and one of the four refusals above
// when the listener named one.
type ProjectionDelivery interface {
	// ProjectionPosition reads where the Data Plane's mirror stands. It is the
	// producer cycle's first call, and the decision — bootstrap, deliver, or
	// stop — belongs entirely to the caller above this port.
	ProjectionPosition(ctx context.Context) (ProjectionPosition, error)
	// ApplyProjectionSnapshot delivers a whole-state message at its boundary.
	ApplyProjectionSnapshot(ctx context.Context, message []byte) (ProjectionAck, error)
	// ApplyProjectionChanges delivers one batch of the change log.
	ApplyProjectionChanges(ctx context.Context, message []byte) (ProjectionAck, error)
}
