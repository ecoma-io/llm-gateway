package application

import (
	"context"
	"errors"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// The three projection operations this application fronts for the Control
// Plane's producer (ADR 0007): read where the Data Plane's mirror stands,
// deliver a whole-state snapshot, deliver one batch of the change log. They
// are the private protocol's caller-facing half — the façade is the only hop
// contracted in OpenAPI, and these use cases are what it contracts.
//
// They are pass-throughs, and the usage-fact read's argument carries over
// whole: the mirror's position is the Data Plane's fact, the delivery grammar
// is the Data Plane's to judge, and a decision made in this process would be a
// middle hop rewriting a protocol whose two ends are other processes. What
// this package adds is the translation of the port's sentinels into codes —
// the transport maps codes and has no business knowing which outbound adapter
// is behind the port.
//
// The one deliberate difference from the read is the message parameter: it is
// the producer's bytes, and this package does not decode them. The port's
// comment carries the full argument — re-encoding a parsed delivery would
// strip the additive fields a newer producer may have added, and the
// additive-tolerant direction of the protocol's evolution is exactly the one
// that ordering would break.

// ProjectionPosition reads where the Data Plane's credential mirror stands.
// It is the use case behind GET /internal/projection/position. The position
// crosses unchanged — including an empty epoch, which is the unbootstrapped
// listener's legitimate spelling of "offer me a snapshot", and revision zero,
// which is the same answer stated numerically.
func (app *App) ProjectionPosition(ctx context.Context) (dataplane.ProjectionPosition, error) {
	position, err := app.projections.ProjectionPosition(ctx)
	if err == nil {
		return position, nil
	}
	return dataplane.ProjectionPosition{}, app.translate(err)
}

// ApplyProjectionSnapshot delivers one whole-state snapshot message to the
// Data Plane. It is the use case behind POST /internal/projection/snapshot.
// The message is the producer's bytes and crosses verbatim; the answer is the
// position the mirror stands at after applying it, which is the snapshot's
// boundary revision.
func (app *App) ApplyProjectionSnapshot(ctx context.Context, message []byte) (dataplane.ProjectionAck, error) {
	ack, err := app.projections.ApplyProjectionSnapshot(ctx, message)
	if err == nil {
		return ack, nil
	}
	return dataplane.ProjectionAck{}, app.translate(err)
}

// ApplyProjectionChanges delivers one batch of the change log to the Data
// Plane. It is the use case behind POST /internal/projection/changes, under
// the same terms as the snapshot delivery above: bytes verbatim, answers
// translated, nothing judged here.
func (app *App) ApplyProjectionChanges(ctx context.Context, message []byte) (dataplane.ProjectionAck, error) {
	ack, err := app.projections.ApplyProjectionChanges(ctx, message)
	if err == nil {
		return ack, nil
	}
	return dataplane.ProjectionAck{}, app.translate(err)
}

// translate turns a projection port failure into this package's vocabulary —
// the one table both deliveries and the position read share, so no arm of the
// surface can grow its own spelling of a refusal.
//
// Each of the four refusals becomes its own code because each demands a
// different reflex from the producer's loop — halt for an upgrade, rebuild
// the message, halt for a human, re-bootstrap on the next cycle — and
// collapsing any two would take a decision away from the loop that has to
// make it. The causes stay behind the boundary, and every public message is
// fixed text: no word of the Data Plane's refusal body, its URLs or its
// internals reaches a caller through one of these.
func (app *App) translate(err error) error {
	switch {
	case errors.Is(err, dataplane.ErrProjectionUnsupportedVersion):
		return &Error{
			Code:    CodeUnsupportedVersion,
			Message: "the message speaks a protocol version this surface does not support",
			cause:   err,
		}
	case errors.Is(err, dataplane.ErrProjectionShape):
		return &Error{
			Code:    CodeInvalidRequest,
			Message: "the message does not satisfy the projection protocol's grammar",
			cause:   err,
		}
	case errors.Is(err, dataplane.ErrProjectionGap):
		return &Error{
			Code:    CodeRevisionGap,
			Message: "the delivered batch does not join the applied position",
			cause:   err,
		}
	case errors.Is(err, dataplane.ErrProjectionSnapshotRequired):
		return &Error{
			Code:    CodeSnapshotRequired,
			Message: "the stored position cannot join this producer timeline; deliver a snapshot",
			cause:   err,
		}
	case errors.Is(err, dataplane.ErrUpstreamUnavailable):
		return &Error{
			Code:    CodeUpstreamUnavailable,
			Message: "the data plane is unavailable",
			cause:   err,
		}
	}

	// An adapter that failed in a way the port does not describe is this
	// process's own defect, and it is classified as such rather than reported
	// as the Data Plane's problem. The cause stays server-side: Internal's
	// message is replaced with a fixed one at the wire.
	return Internal(err)
}
