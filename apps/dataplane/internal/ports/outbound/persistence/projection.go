package persistence

import (
	"context"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
)

// ProjectionApplier is the outbound port the credential mirror is applied
// through: the consumer side of the Control → Data projection (ADR 0007). It
// is the one port whose writes another plane dictates — the rows are the
// Control Plane's decisions, delivered over the private management listener —
// and its contract is the protocol's five rules restated as obligations:
//
//   - Position reports this plane's own fact: whether a snapshot has been
//     applied, the highest revision whose effects are committed, and the
//     producer timeline that position was earned on. An unbootstrapped store
//     reports Bootstrapped false, revision zero and an empty epoch.
//   - ApplySnapshot writes a whole projection and the position in one
//     transaction, unconditionally: a snapshot means "be this state at this
//     boundary", so it consults no per-row history and may move the position
//     either way — recovery has to be able to reach a producer timeline that
//     rewound. appliedAt is the application instant stamped on every row the
//     snapshot writes; a snapshot carries no per-row time of its own, and the
//     write time is the honest one.
//   - ApplyChanges judges a whole incremental batch against the stored
//     position under one transaction: applied with per-row revision guards
//     and the position advanced to its last revision; acknowledged without
//     work when it sits entirely behind the position (the lost-acknowledgement
//     case); or refused whole — never partially applied — when it cannot join.
//
// Every method assumes the message it is given has passed the domain's grammar
// (projection.ValidateSnapshot / projection.ValidateBatch); the adapter's
// transaction and guards are the second line of defence, not the first. The
// sentinel errors the rules refuse with — projection.ErrUnsupportedVersion,
// projection.ErrRevisionGap, projection.ErrSnapshotRequired — come back
// wrapped, so a caller can classify with errors.Is.
type ProjectionApplier interface {
	Position(ctx context.Context) (projection.Position, error)
	ApplySnapshot(ctx context.Context, snapshot projection.Snapshot, appliedAt time.Time) (uint64, error)
	ApplyChanges(ctx context.Context, batch projection.Batch) (uint64, error)
}
