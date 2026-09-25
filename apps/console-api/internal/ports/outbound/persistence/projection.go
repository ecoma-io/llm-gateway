package persistence

import (
	"context"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/projection"
)

// ProjectionLog is the Control Plane's half of the Control → Data projection
// (ADR 0007): the durable change log, the materialized tables the snapshot is
// cut from, and the revision counter that orders both — in the store's
// vocabulary, speaking the projection domain's types the way the identity
// ports above speak the identity domain's.
//
// The port exists beside the identity repositories rather than inside them
// because the recording is business policy, not storage mechanics: which
// authority writes project, and what a projected row's full state is, are
// decisions the use case makes — and the use case makes them in the same
// unit of work as the authority write, which is the whole architecture. The
// adapter implements the mechanics: allocate a revision, append the entry,
// write the mirror row, one transaction, no gaps.
//
// Three properties the signatures carry on purpose:
//
//   - The revision is never an argument. It is allocated inside the
//     caller's transaction by the store — the counter's row lock, held to
//     commit, is what makes allocation order equal commit order — so a
//     caller that passed one in could only be guessing it. A method without
//     a revision parameter is the promise "the log numbers its own entries",
//     and no call site can break it.
//   - Recording runs in the caller's unit of work and nowhere else. Every
//     method resolves its statements through the same Querier the identity
//     repositories use, so a record inside a WithinTx scope commits or rolls
//     back with the authority write beside it: there is no state in which the
//     authority moved and the projection did not hear, or the projection
//     claims a change the authority never committed.
//   - Reads are whole. Snapshot returns the cut and its boundary revision
//     together, read in one unit of work, because a snapshot is a claim about
//     a boundary and half a claim is worse than none.
//
// Revisions cross as uint64 here and as bigint in the database. The column's
// ceiling is the real one: 2^63−1 revisions at one allocation per
// transaction is a number no deployment counts down to, and the domain's
// uint64 exists so a leaked negative or a wrapped subtraction is caught in
// Go before it becomes a row — not because 2^64 is reachable.
//
// The log never deletes. Revocation is a state, the entries are history, and
// retention is a later phase's decision about a table whose every row is
// still the recovery story for a consumer that was down when it was written.

// ProjectionChangeRecorder appends projection entries inside the caller's
// unit of work. It is the write half, split from the read half below so the
// identity use cases depend on the two methods they use and the delivery
// loop on the three it does — a reader of this port can see which half a
// caller plays without reading its body.
type ProjectionChangeRecorder interface {
	// RecordCredentialChange appends one api_key entry at the next revision
	// and writes the credential into the materialized projection — the
	// snapshot source — in the same unit of work. `at` is the recorded
	// instant: it orders nothing (ADR 0007 §3), it is carried for
	// operators, and passing the same instant the authority write used is
	// what keeps the log's story of a row and the row's own timestamps one
	// story.
	RecordCredentialChange(ctx context.Context, at time.Time, credential projection.Credential) error

	// RecordAccountChange appends one account entry and writes the account
	// state into the materialized projection, with the same discipline as
	// its credential twin.
	RecordAccountChange(ctx context.Context, at time.Time, account projection.Account) error
}

// ProjectionLog is the read half: the counter's position, the snapshot cut,
// and the entries after a revision. The delivery loop reads through this
// interface and never learns how the log is stored — which is what keeps the
// delivery loop testable against a feed that answers instantly and a
// database that does not.
type ProjectionLog interface {
	// Head returns the timeline's identity and its highest allocated
	// revision.
	Head(ctx context.Context) (projection.Head, error)

	// Snapshot cuts the whole projection at a boundary: every credential and
	// account row whose entry is at or before the head revision, read in one
	// unit of work with that revision. The cut is atomic by construction —
	// the mirror rows are written in the same transaction as the log entries
	// they correspond to — so what returns is a state the log can resume
	// from, not an approximation of one.
	Snapshot(ctx context.Context) (projection.Snapshot, error)

	// ChangesAfter returns at most limit entries strictly after `after`, in
	// ascending revision order — the log's own order, which is commit order.
	// Fewer than limit entries means the feed is drained up to whoever wrote
	// last; the delivery loop treats the difference as ordinary, because a
	// concurrent writer is always producing a short page too.
	ChangesAfter(ctx context.Context, after uint64, limit int) ([]projection.Change, error)

	// Credential returns the credential projection's row for keyID — the
	// mirror read the revoke path needs: a revoked credential's log entry
	// carries the row's whole state, and the digest lives only inside the
	// projection pipeline's own tables — the log payload and this mirror row
	// — the ownership record staying digest-free by the two-record model. It
	// resolves through the caller's unit of work like every method here, so
	// a revoke reads the row and rewrites it in the one transaction that
	// makes the key dead.
	//
	// ErrNotFound is an answer, not a failure: keys minted before the
	// projection foundation have no mirror row and no digest anywhere in
	// this lane — the migration that introduced it backfills accounts only
	// (ADR 0007 records the recovery). A caller that finds no credential has
	// nothing to project and no credential to describe.
	Credential(ctx context.Context, keyID string) (projection.Credential, error)
}
