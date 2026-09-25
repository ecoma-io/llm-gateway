package persistence

import (
	"context"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
)

// The catalog repositories, in the store's vocabulary.
//
// The catalog is Data Plane data (ADR 0006 §5): the backends calls go out
// through, the aliases requests name, and the group versions entitlement
// scopes point at. All of it lives in the `dataplane` database and nowhere
// else — the Control Plane learns catalog facts over the management chain,
// never by connecting here, so no statement in this port's implementations
// can be asked to join a control-plane row.
//
// The identity port's three rules carry over unchanged, and one is used
// harder here than there:
//
//   - Aggregates go in whole. An alias IS its candidate list (ADR 0001,
//     rule 3), so Create writes the row and its candidates in the caller's
//     unit of work — a candidates table row without its alias row can never
//     exist, because the schema says so and this port would have to bypass
//     it to try. Reads return every part of the aggregate — ByID loads
//     candidates in fallback order beside the row — and each part as of its
//     own committed statement: whether resolution needs the two statements
//     pinned to one instant is the runtime phase's snapshot decision, not
//     something this port presumes.
//   - State and configuration changes travel as compare-and-swap on the
//     alias row's state. SetCandidates and UpdateBounds apply only while
//     the row still reads active — the state the caller based its new list
//     on — so an edit racing a retirement loses cleanly and re-reads; a
//     false return is that loss, not an error. No timestamp comparison: the
//     state is the contract, the way it is in the Control Plane's port.
//   - Deletes do not exist as an API. The one delete in the catalog — a
//     candidate list being replaced by its successor — is the inside of
//     SetCandidates, never a method.

// Backends persists the backend aggregate — one configured adapter instance
// calls resolve to.
type Backends interface {
	// Create inserts a new backend in its birth state (active). The
	// adapter type, endpoint and references it carries are already the
	// domain's validated values; the schema's grammar checks are the
	// second opinion, not the first.
	Create(ctx context.Context, backend *catalog.Backend) error

	// ByID returns the backend with id, or ErrNotFound.
	ByID(ctx context.Context, id catalog.BackendID) (*catalog.Backend, error)

	// TransitionState applies the active ↔ disabled machine's one move: it
	// sets the state to `to` only while the row still shows `from`, stamps
	// updated_at with the caller's instant, and reports whether the move
	// happened. A false return means someone else moved the row first; the
	// caller re-reads.
	TransitionState(ctx context.Context, id catalog.BackendID, from, to catalog.BackendState, updatedAt time.Time) (bool, error)

	// UpdateTarget re-points the backend's endpoint and references in one
	// move — the target axis of the backend machine, as TransitionState is
	// its state axis. It is a total move of that axis (all three values at
	// once, adapter type absent by design), not a field patch: what it
	// writes is exactly what the domain's UpdateTarget validated. Like its
	// sibling it reports whether the row moved; false means the backend is
	// gone — unreachable today, because nothing deletes backends, but the
	// port does not promise success on a miss.
	UpdateTarget(ctx context.Context, id catalog.BackendID, endpoint, credentialsRef, egressPolicyRef string, updatedAt time.Time) (bool, error)
}

// ModelAliases persists the alias aggregate — the client-facing name and
// everything resolution needs beside it.
type ModelAliases interface {
	// Create inserts a new alias and its candidate list in one unit of work
	// with the caller's: the alias row, then every candidate at its
	// assigned position. The schema's total unique index on name — total,
	// because retired names are never reissued — surfaces a collision here
	// as catalog.ErrAliasNameTaken.
	Create(ctx context.Context, alias *catalog.ModelAlias) error

	// ByID returns the whole aggregate — the alias row and its candidates
	// ordered by position — or ErrNotFound. The parts each read as of their
	// own committed statement; see the package comment on what that does and
	// does not promise.
	ByID(ctx context.Context, id catalog.AliasID) (*catalog.ModelAlias, error)

	// ByName returns the same whole aggregate as ByID, looked up the way
	// requests arrive: by the client-facing name, served by the schema's
	// total unique index. A retired alias is RETURNED, with its state and
	// its frozen candidate list — retirement is the caller's decision to
	// turn into an unknown_alias, not this read's, and a replay of an
	// original request admitted against an alias that has since retired must
	// still be able to resolve the alias from the request's recorded intake
	// rather than from today's catalog. ErrNotFound means no alias has ever
	// carried the name: the names are never reissued, so a miss is final.
	ByName(ctx context.Context, name string) (*catalog.ModelAlias, error)

	// Retire applies the one-way active → retired move, compare-and-swapped
	// like every transition: it flips the state and stamps both retired_at
	// and updated_at only while the row still shows `from`. The candidates
	// are not touched — retirement freezes them exactly as they were.
	Retire(ctx context.Context, id catalog.AliasID, from catalog.AliasState, retiredAt time.Time) (bool, error)

	// SetCandidates replaces the alias's whole candidate list, guarded by
	// the active-state compare-and-swap: the guard statement runs first,
	// and a false return leaves the old list standing. The replacement —
	// delete the list, insert the successor — is the one delete the
	// catalog contains, and it is atomic with the guard inside the
	// caller's unit of work.
	SetCandidates(ctx context.Context, id catalog.AliasID, candidates []catalog.Candidate, updatedAt time.Time) (bool, error)

	// UpdateBounds re-points the alias's output limit and reservation cap,
	// guarded by the active-state compare-and-swap exactly as
	// SetCandidates is.
	UpdateBounds(ctx context.Context, id catalog.AliasID, from catalog.AliasState, maxOutputTokens, reservationCap int64, updatedAt time.Time) (bool, error)
}

// AliasGroupVersions persists the group-version snapshots entitlement scopes
// point at — the immutable end of the catalog.
type AliasGroupVersions interface {
	// Create inserts one version snapshot — the version row and its member
	// rows — in one unit of work with the caller's. The schema's unique
	// (group_name, version) index surfaces a collision here as
	// catalog.ErrGroupVersionExists, which is the open-version race's
	// signal to re-read and retry, not an operator-facing error.
	Create(ctx context.Context, version *catalog.AliasGroupVersion) error

	// ByGroupAndVersion returns the snapshot named by the pair — the
	// version row and its members — or ErrNotFound.
	ByGroupAndVersion(ctx context.Context, groupName string, version int) (*catalog.AliasGroupVersion, error)

	// HighestVersion returns the group's newest version number, or 0 when
	// the group has none — the 0 is what makes the caller's `highest + 1`
	// open version 1 without a special case.
	HighestVersion(ctx context.Context, groupName string) (int, error)
}
