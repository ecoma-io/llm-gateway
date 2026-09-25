package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// ProjectionDelivery is the Control → Data projection's producer: the
// reconciliation loop that carries the Control Plane's identity decisions to
// the Data Plane's credential mirror (ADR 0006 §5, ADR 0007). It owns no
// state of its own — not a buffer, not a cursor, not a learned position —
// because everything it needs to know is durable somewhere better: the log
// and the mirror tables in the Control Plane's database, the consumer's
// position in the Data Plane's. A crashed or redeployed producer resumes by
// asking both sides where things stand, which is what makes the loop
// stateless and its recovery story one sentence long.
//
// One Reconcile call is one cycle, and every cycle is the same three steps:
//
//  1. read both sides — the log's head (timeline, highest revision) and the
//     mirror's position (timeline it was earned on, bootstrapped, applied
//     revision);
//  2. decide whether the position joins this timeline: bootstrapped, same
//     epoch, at or before the head. Anything else — never bootstrapped, a
//     foreign timeline, a position that has run past the log's head because
//     the producer's database was restored to an earlier instant, or a
//     caller forcing the question — is answered with a snapshot, cut at the
//     head and delivered whole. The consumer applies it unconditionally and
//     its position becomes the snapshot boundary in the same transaction as
//     the rows it carries;
//  3. drain the log from the position in batches of at most
//     projection.MaxChangesPerBatch contiguous revisions, delivering until
//     the feed is empty. A batch refused as not joining the position — a
//     lost acknowledgement, a timeline dispute found mid-drain — is answered
//     by a snapshot, and the drain resumes from where the snapshot left the
//     mirror.
//
// Failure has exactly two shapes, and telling them apart is the loop's whole
// judgement:
//
//   - A refusal that means "deliver differently" self-heals.
//     ErrProjectionGap and ErrProjectionSnapshotRequired are answered with a
//     snapshot in the same cycle — the protocol's own answer to a position
//     that cannot join its timeline — and the drain continues from the
//     snapshot's boundary. ErrProjectionUnavailable fails the cycle and the
//     next tick starts over: delivery is at-least-once against a durable
//     log, so an unknown answer costs one redelivery and nothing durable.
//   - A refusal that means "this build is wrong" fails loudly and keeps
//     failing. ErrProjectionUnsupportedVersion and ErrProjectionShape are
//     the closed version set and the grammar this process itself owns; no
//     re-snapshot or retry can fix either, so the error is surfaced every
//     cycle for an operator, and nothing is ever skipped past them.
//
// What the loop never does is decide a delivery did not need to happen. The
// consumer's three-case rule (ADR 0007 §5) makes a redundant batch a
// no-op answer, so erring toward delivering is always safe and erring toward
// skipping is how a mirror quietly stops mirroring.
type ProjectionDelivery struct {
	log      persistence.ProjectionLog
	consumer dataplane.Projection
}

// NewProjectionDelivery builds the producer around the two ports it needs.
// It panics on a nil port for the same reason the other constructors here
// do: a wiring defect should be discovered at the composition root, not as
// the first cycle's mysterious failure.
func NewProjectionDelivery(log persistence.ProjectionLog, consumer dataplane.Projection) *ProjectionDelivery {
	switch {
	case log == nil:
		panic("application: NewProjectionDelivery requires a projection log")
	case consumer == nil:
		panic("application: NewProjectionDelivery requires a projection consumer")
	}
	return &ProjectionDelivery{log: log, consumer: consumer}
}

// Reconcile runs one cycle: read both sides, bootstrap the mirror if its
// position does not join the timeline, and drain the log until the feed is
// empty. The returned error is the cycle's outcome, already carrying which
// half failed; the process's ticker decides what a failed cycle costs (a log
// line and a wait), because the protocol's answer to every transient failure
// is the next cycle.
func (p *ProjectionDelivery) Reconcile(ctx context.Context, force bool) error {
	head, err := p.log.Head(ctx)
	if err != nil {
		return fmt.Errorf("application: projection reconcile: read the log's head: %w", err)
	}
	position, err := p.consumer.ProjectionPosition(ctx)
	if err != nil {
		return fmt.Errorf("application: projection reconcile: read the mirror's position: %w", err)
	}

	joins := position.Bootstrapped &&
		position.Epoch == head.Epoch &&
		position.AppliedRevision <= head.LastRevision

	from := position.AppliedRevision
	if force || !joins {
		// The position cannot join this timeline, or the caller asked for a
		// snapshot regardless: bootstrap. The snapshot is cut at the head
		// this cycle read, and the acknowledgement is the mirror's committed
		// answer — the boundary the drain resumes from, not the boundary the
		// producer assumed.
		from, err = p.bootstrap(ctx)
		if err != nil {
			return err
		}
	}

	return p.drain(ctx, head.Epoch, from)
}

// bootstrap cuts a snapshot at the log's head and delivers it. The returned
// revision is the mirror's position after it committed the snapshot — the
// drain's resume point.
func (p *ProjectionDelivery) bootstrap(ctx context.Context) (uint64, error) {
	snapshot, err := p.log.Snapshot(ctx)
	if err != nil {
		return 0, fmt.Errorf("application: projection bootstrap: cut the snapshot: %w", err)
	}
	message, err := json.Marshal(snapshot)
	if err != nil {
		return 0, fmt.Errorf("application: projection bootstrap: render the snapshot: %w", err)
	}
	ack, err := p.consumer.DeliverSnapshot(ctx, message)
	if err != nil {
		// A snapshot refused as not joining the mirror's position is the
		// consumer contradicting the protocol's own rule — a snapshot is
		// unconditional. The cycle fails and the next one tries again: the
		// answer is durable on the producer's side, and retrying a refusal
		// that should not exist is loud, visible and harmless.
		return 0, fmt.Errorf("application: projection bootstrap: deliver the snapshot: %w", err)
	}
	return ack.AppliedRevision, nil
}

// drain delivers the log's entries from `from` forward, in batches, until
// the feed is empty. The epoch starts as the one the cycle's opening head
// read named, and is re-read after every heal: a heal cuts its snapshot
// against the log as it stands at that moment, and if the timeline's epoch
// was re-minted under the loop's feet — the operator step ADR 0007's restore
// procedure names — the batches this loop assembles afterwards must speak
// the re-minted word, not the dead one the cycle started with.
func (p *ProjectionDelivery) drain(ctx context.Context, epoch string, from uint64) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("application: projection drain: %w", err)
		}
		changes, err := p.log.ChangesAfter(ctx, from, projection.MaxChangesPerBatch)
		if err != nil {
			return fmt.Errorf("application: projection drain: read the log after revision %d: %w", from, err)
		}
		if len(changes) == 0 {
			return nil
		}

		batch, err := projection.NewBatch(epoch, from, changes)
		if err != nil {
			// Unreachable while the guarantees hold: the counter allocates
			// gaplessly inside each writer's transaction, and the foundation
			// migration's SHARE lock kept its backfill from ever seeding a
			// revision with no log entry behind it. The contiguity check is
			// here anyway so that if either guarantee is ever broken — a
			// restored database whose counter was not re-minted being the
			// realistic way — the corruption fails this process loudly
			// instead of being delivered as a batch the consumer must
			// refuse.
			return fmt.Errorf("application: projection drain: assemble the batch after revision %d: %w", from, err)
		}
		message, err := json.Marshal(batch)
		if err != nil {
			return fmt.Errorf("application: projection drain: render the batch after revision %d: %w", from, err)
		}

		ack, err := p.consumer.DeliverChanges(ctx, message)
		if err != nil {
			if errors.Is(err, dataplane.ErrProjectionGap) || errors.Is(err, dataplane.ErrProjectionSnapshotRequired) {
				// The mirror's position cannot join the batches: answer with
				// a snapshot — the protocol's own recovery — and resume
				// draining from the boundary it committed. The `from` this
				// loop held is replaced, not compared: the snapshot is the
				// authority on where the mirror now stands.
				from, err = p.bootstrap(ctx)
				if err != nil {
					return err
				}
				// The snapshot was cut against the log as it stands now, so
				// the epoch is re-read with it. Without this, a timeline
				// re-minted mid-cycle would leave the loop assembling batches
				// under the dead epoch — each refused, each healed, the
				// deadline the only thing that ever stops it.
				head, err := p.log.Head(ctx)
				if err != nil {
					return fmt.Errorf("application: projection drain: re-read the log's head after the heal: %w", err)
				}
				epoch = head.Epoch
				continue
			}
			// Everything else — a version this build does not speak, a
			// grammar refusal of a message this process built, an unknown
			// answer — fails the cycle with its cause named. The next tick
			// retries it; the error is the operator's signal either way.
			return fmt.Errorf("application: projection drain: deliver the batch through revision %d: %w", batch.LastRevision(), err)
		}
		from = ack.AppliedRevision
	}
}
