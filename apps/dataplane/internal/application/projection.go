package application

import (
	"context"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
)

// The projection use cases: this process's half of the Control → Data
// credential projection (ADR 0007), exposed to the private management
// listener. The Control Plane is the authority for who may call the gateway;
// these are the calls by which its decisions land in the database the request
// path reads. None of them reaches back to the Control Plane, and none of
// them is on the request path — a projection that stalls must slow a mirror's
// freshness, never a request's admission.
//
// The use cases are thin by design: the protocol's rules live in the domain
// package and the transactional application lives behind the persistence
// port. What is decided here is the boundary itself — the grammar is checked
// before a store is touched, so a message that would violate its own
// database's constraints is refused without opening a transaction, and the
// port's answer is passed through with its sentinel errors intact for the
// listener to translate onto its own wire vocabulary.

// ProjectionPosition reports this plane's own fact: whether a snapshot has
// been applied, the highest revision whose effects are committed, and the
// producer timeline that position was earned on. The Control Plane's delivery
// loop reads it once per cycle and decides from it — bootstrap or drain — so
// it is the one call that makes the producer stateless.
func (app *App) ProjectionPosition(ctx context.Context) (projection.Position, error) {
	return app.projections.Position(ctx)
}

// ApplyProjectionSnapshot applies a whole projection at a named boundary:
// "be this state at this revision". The grammar is judged before the store is
// touched; the application itself is unconditional and transactional, the
// port's contract. The answer is the new position, and it is the
// acknowledgement.
func (app *App) ApplyProjectionSnapshot(ctx context.Context, snapshot projection.Snapshot) (uint64, error) {
	if err := projection.ValidateSnapshot(snapshot); err != nil {
		return 0, err
	}
	return app.projections.ApplySnapshot(ctx, snapshot, time.Now().UTC())
}

// ApplyProjectionChanges judges an incremental batch against the stored
// position and applies it only when the judgement says "apply". The grammar is
// judged first — a batch that is not contiguous, or carries a state this plane
// has never heard of, is refused before anything is read, let alone written.
// The three answers — apply, duplicate, gap — are the port's to give, and the
// returned revision is the acknowledgement either way.
func (app *App) ApplyProjectionChanges(ctx context.Context, batch projection.Batch) (uint64, error) {
	if err := projection.ValidateBatch(batch); err != nil {
		return 0, fmt.Errorf("apply changes: %w", err)
	}
	return app.projections.ApplyChanges(ctx, batch)
}
