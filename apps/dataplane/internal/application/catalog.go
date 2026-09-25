package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The catalog use cases: the serving state the runtime resolves — backends,
// aliases with their candidate lists, and the group versions entitlement
// scopes point at — driven through the persistence port and the catalog
// domain (ADR 0001, ADR 0002, ADR 0003). Every lifecycle move below is a
// read, a domain transition, and a compare-and-swap, the discipline the
// Control Plane's identity use cases established: the CAS is what makes
// concurrent edits converge on the domain's rules instead of one silently
// overwriting a state its caller never saw. The loser of a swap re-reads and
// re-applies from the state that actually exists.
//
// Production constructs this type in cmd/dataplane over the PostgreSQL
// adapters, and one read of it is served over the management chain today:
// CurrentGroupVersion is the catalog fact the Control Plane's commerce roll
// asks for before it can pin an entitlement's scope (ADR 0006 §7). The
// lifecycle moves below are still write-side only, and no HTTP surface reaches
// them — the management operations that will drive them are later phases'
// contract changes, and an OpenAPI edit for those stays out of scope until
// there is something to document. That is also why no method below maps
// failures to application.Error categories: the transport that would own that
// mapping does not exist for them yet, and inventing its statuses ahead of it
// would be a shape guessed twice. Callers match the domain's sentinels
// (errors.Is) and the persistence port's ErrNotFound.
//
// The one method that leaves that shape is the read, precisely because a
// transport does reach it: its not-found outcome crosses the private protocol
// as a contracted 404, so CurrentGroupVersion carries it as this package's
// NotFound — the one application.Error category a Catalog method produces —
// rather than leaving the transport to re-decide what a persistence sentinel
// means on the wire.
//
// What this layer deliberately does not do: resolve an alias to a route. A
// request naming an alias is admitted against the aggregate's bounds and
// candidate list, but the choosing among candidates — failover order, health,
// disabled backends — is the routing engine's, a later phase. This file
// builds and maintains the state that engine reads; it never routes.

// ErrTransitionContended reports that a lifecycle move kept losing its
// compare-and-swap for casMaxAttempts straight attempts. It is not a domain
// rejection — the domain never refused anything — it is this use case giving
// up on a row that is being written faster than it can re-read, and
// surfacing that beats looping forever.
var ErrTransitionContended = errors.New("application: concurrent writes did not settle")

// casMaxAttempts bounds the read–apply–swap loop. Eight is not a tuned
// number: a swap is one statement, a lost swap means someone else completed
// a move first, and the loop's honest terminations are "the move is already
// there" (a no-op on re-read) and, only under pathological contention, this
// error.
const casMaxAttempts = 8

// Catalog is the model catalog's use cases.
type Catalog struct {
	store    persistence.Store
	backends persistence.Backends
	aliases  persistence.ModelAliases
	versions persistence.AliasGroupVersions
}

// NewCatalog builds the catalog use cases around the ports they need. It
// panics on a nil port because a port this use case was promised and did not
// get is a wiring defect, and the middle of a define — after a transaction is
// open and ids are minted — is a strictly worse place to learn about it.
func NewCatalog(store persistence.Store, backends persistence.Backends, aliases persistence.ModelAliases, versions persistence.AliasGroupVersions) *Catalog {
	switch {
	case store == nil:
		panic("application: NewCatalog requires a store")
	case backends == nil:
		panic("application: NewCatalog requires a backends repository")
	case aliases == nil:
		panic("application: NewCatalog requires an aliases repository")
	case versions == nil:
		panic("application: NewCatalog requires a group versions repository")
	}
	return &Catalog{store: store, backends: backends, aliases: aliases, versions: versions}
}

// Ping reports whether the store the catalog's use cases run against is
// answering within ctx. That store is this process's one database — the pool
// every repository over it shares — so the runtime's readiness question is
// asked through the one use case that already holds the store, rather than by
// opening a second handle on the same pool.
func (c *Catalog) Ping(ctx context.Context) error {
	return c.store.Ping(ctx)
}

// ---------------------------------------------------------------------------
// backends
// ---------------------------------------------------------------------------

// RegisterBackend records one configured adapter instance, in its birth
// state, active. The adapter type's grammar is checked here and never again:
// it is immutable past construction, because it is what decides which driver
// reads the endpoint under this id.
func (c *Catalog) RegisterBackend(ctx context.Context, adapterType, endpoint, credentialsRef, egressPolicyRef string) (*catalog.Backend, error) {
	id, err := catalog.NewBackendID()
	if err != nil {
		return nil, fmt.Errorf("application: register backend: %w", err)
	}
	backend, err := catalog.NewBackend(id, adapterType, endpoint, credentialsRef, egressPolicyRef, time.Now())
	if err != nil {
		return nil, fmt.Errorf("application: register backend: %w", err)
	}
	if err := c.store.WithinTx(ctx, func(txCtx context.Context) error {
		return c.backends.Create(txCtx, backend)
	}); err != nil {
		return nil, fmt.Errorf("application: register backend %s: %w", backend.ID, err)
	}
	return backend, nil
}

// Backend returns the backend with id. A miss is the persistence port's
// ErrNotFound, wrapped with what was looked for.
func (c *Catalog) Backend(ctx context.Context, id catalog.BackendID) (*catalog.Backend, error) {
	backend, err := c.backends.ByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("application: read backend %s: %w", id, err)
	}
	return backend, nil
}

// DisableBackend drains a backend: candidate selection skips it. Already-
// disabled is a no-op.
func (c *Catalog) DisableBackend(ctx context.Context, id catalog.BackendID) error {
	return c.transitionBackend(ctx, id, (*catalog.Backend).Disable)
}

// EnableBackend returns a drained backend to selection. Already-active is a
// no-op.
func (c *Catalog) EnableBackend(ctx context.Context, id catalog.BackendID) error {
	return c.transitionBackend(ctx, id, (*catalog.Backend).Enable)
}

// RetargetBackend re-points a backend's endpoint and references. The move is
// legal in both states — re-pointing a drained backend is ordinary incident
// work — so it travels on the target axis alone and no state is read for a
// swap: the port's UpdateTarget writes exactly the three values the domain
// validated, under the id, and touches no state column.
func (c *Catalog) RetargetBackend(ctx context.Context, id catalog.BackendID, endpoint, credentialsRef, egressPolicyRef string) error {
	return c.store.WithinTx(ctx, func(txCtx context.Context) error {
		backend, err := c.backends.ByID(txCtx, id)
		if err != nil {
			return fmt.Errorf("application: read backend %s: %w", id, err)
		}
		if err := backend.UpdateTarget(endpoint, credentialsRef, egressPolicyRef, time.Now()); err != nil {
			return fmt.Errorf("application: retarget backend %s: %w", id, err)
		}
		applied, err := c.backends.UpdateTarget(txCtx, id, backend.Endpoint, backend.CredentialsRef, backend.EgressPolicyRef, backend.UpdatedAt)
		if err != nil {
			return fmt.Errorf("application: retarget backend %s: %w", id, err)
		}
		if !applied {
			// Unreachable through this path — the read above would have missed
			// first, and nothing deletes backends — but the port reports the
			// miss and the use case carries it as one.
			return fmt.Errorf("application: retarget backend %s: %w", id, persistence.ErrNotFound)
		}
		return nil
	})
}

// transitionBackend is the read → domain transition → compare-and-swap loop,
// identical in shape to the Control Plane's transitionAccount: the aggregate's
// own method is what refuses illegal moves and what declares a move a no-op.
func (c *Catalog) transitionBackend(ctx context.Context, id catalog.BackendID, apply func(*catalog.Backend, time.Time) error) error {
	return c.store.WithinTx(ctx, func(txCtx context.Context) error {
		for attempt := 0; attempt < casMaxAttempts; attempt++ {
			backend, err := c.backends.ByID(txCtx, id)
			if err != nil {
				return fmt.Errorf("application: read backend %s: %w", id, err)
			}
			before := backend.State
			if err := apply(backend, time.Now()); err != nil {
				return fmt.Errorf("application: transition backend %s: %w", id, err)
			}
			if backend.State == before {
				return nil // the domain ruled this a no-op; nothing to swap
			}
			applied, err := c.backends.TransitionState(txCtx, id, before, backend.State, backend.UpdatedAt)
			if err != nil {
				return fmt.Errorf("application: transition backend %s: %w", id, err)
			}
			if applied {
				return nil
			}
			// Lost the swap: the row moved under us, and the next iteration
			// re-reads and re-applies from the state that actually exists.
		}
		return fmt.Errorf("application: transition backend %s: %w after %d attempts", id, ErrTransitionContended, casMaxAttempts)
	})
}

// ---------------------------------------------------------------------------
// aliases — the client-facing aggregate
// ---------------------------------------------------------------------------

// DefineAlias opens a model name for business: the client-visible identifier,
// the bounds admission will enforce, and the ordered candidate list that
// resolves it — one aggregate, written in one unit of work. Every candidate's
// backend must exist: the reference is by id alone, and a list naming a
// backend that is not on file is refused here rather than left for the
// foreign key to report in driver dialect. The name's uniqueness is total —
// active and retired names alike are never reissued — and its collision
// surfaces as the domain's ErrAliasNameTaken.
func (c *Catalog) DefineAlias(ctx context.Context, name string, maxOutputTokens, reservationCap int64, candidates []catalog.Candidate) (*catalog.ModelAlias, error) {
	id, err := catalog.NewAliasID()
	if err != nil {
		return nil, fmt.Errorf("application: define alias: %w", err)
	}
	alias, err := catalog.NewAlias(id, name, maxOutputTokens, reservationCap, candidates, time.Now())
	if err != nil {
		return nil, fmt.Errorf("application: define alias: %w", err)
	}
	if err := c.store.WithinTx(ctx, func(txCtx context.Context) error {
		// Existence only, not state: a disabled backend stays referencable —
		// disabling is operational drain, and the alias's list naming it is
		// what makes re-enabling enough. The check reads without a lock, the
		// same documented residual the Control Plane's mint carries: there is
		// no delete path in this schema, so the row a caller just read cannot
		// be gone when the insert lands.
		for _, candidate := range alias.Candidates {
			if _, err := c.backends.ByID(txCtx, candidate.BackendID); err != nil {
				return fmt.Errorf("application: define alias: candidate %d (backend %s): %w", candidate.Position, candidate.BackendID, errors.Join(catalog.ErrInvalidCandidates, err))
			}
		}
		return c.aliases.Create(txCtx, alias)
	}); err != nil {
		return nil, fmt.Errorf("application: define alias %s: %w", alias.ID, err)
	}
	return alias, nil
}

// Alias returns the whole aggregate — the alias row and its candidates in
// fallback order. The parts read as of their own committed statements; a
// caller that needs them pinned to one instant holds the unit of work (or
// the runtime phase decides the snapshot question the port's package comment
// records).
func (c *Catalog) Alias(ctx context.Context, id catalog.AliasID) (*catalog.ModelAlias, error) {
	alias, err := c.aliases.ByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("application: read alias %s: %w", id, err)
	}
	return alias, nil
}

// RetireAlias takes a model name out of resolution, one-way. Already-retired
// is a no-op; the row stays exactly as its historical requests used it.
func (c *Catalog) RetireAlias(ctx context.Context, id catalog.AliasID) error {
	return c.reconcileAlias(ctx, id, func(alias *catalog.ModelAlias, now time.Time) (bool, error) {
		before := alias.State
		if err := alias.Retire(now); err != nil {
			return false, err
		}
		return alias.State != before, nil
	}, func(txCtx context.Context, alias *catalog.ModelAlias) (bool, error) {
		return c.aliases.Retire(txCtx, alias.ID, catalog.AliasActive, alias.UpdatedAt)
	})
}

// SetCandidates replaces an alias's whole fallback list. There is no
// per-candidate edit on purpose — the list is the fallback policy, and it is
// replaced the way it was born: positions 1..n in slice order, one unit of
// work, the outgoing list deleted inside the same transaction that inserts
// its successor. Refused outright on a retired alias: retirement froze it.
func (c *Catalog) SetCandidates(ctx context.Context, id catalog.AliasID, candidates []catalog.Candidate) error {
	return c.reconcileAlias(ctx, id, func(alias *catalog.ModelAlias, now time.Time) (bool, error) {
		// A list replacement always moves the aggregate — the alias's state
		// does not change, so the CAS's no-op short-circuit must not fire.
		return true, alias.SetCandidates(candidates, now)
	}, func(txCtx context.Context, alias *catalog.ModelAlias) (bool, error) {
		return c.aliases.SetCandidates(txCtx, alias.ID, alias.Candidates, alias.UpdatedAt)
	})
}

// UpdateAliasBounds re-points the output limit and reservation cap admission
// reads. Refused outright on a retired alias.
func (c *Catalog) UpdateAliasBounds(ctx context.Context, id catalog.AliasID, maxOutputTokens, reservationCap int64) error {
	return c.reconcileAlias(ctx, id, func(alias *catalog.ModelAlias, now time.Time) (bool, error) {
		// Writing the numbers that are already there is still a move: the
		// operator set them, the stamp says so, and the swap is idempotent.
		return true, alias.UpdateBounds(maxOutputTokens, reservationCap, now)
	}, func(txCtx context.Context, alias *catalog.ModelAlias) (bool, error) {
		return c.aliases.UpdateBounds(txCtx, alias.ID, alias.State, alias.MaxOutputTokens, alias.ReservationCap, alias.UpdatedAt)
	})
}

// reconcileAlias runs one alias edit as the CAS loop the use cases share:
// read the whole aggregate, let the domain apply the move, swap under the
// state the read saw. The apply function reports whether the aggregate
// actually moved — a retirement of an already-retired alias did not, while
// a candidate-list replacement always did, state or no state. The domain's
// refusals — a retired alias above all — are terminal and returned as they
// are; only a lost swap retries, because a lost swap is the one outcome
// that means the world moved under the caller rather than that the caller
// asked for something impossible.
func (c *Catalog) reconcileAlias(ctx context.Context, id catalog.AliasID, apply func(*catalog.ModelAlias, time.Time) (bool, error), swap func(context.Context, *catalog.ModelAlias) (bool, error)) error {
	return c.store.WithinTx(ctx, func(txCtx context.Context) error {
		for attempt := 0; attempt < casMaxAttempts; attempt++ {
			alias, err := c.aliases.ByID(txCtx, id)
			if err != nil {
				return fmt.Errorf("application: read alias %s: %w", id, err)
			}
			changed, err := apply(alias, time.Now())
			if err != nil {
				return fmt.Errorf("application: edit alias %s: %w", id, err)
			}
			if !changed {
				return nil // the domain ruled this a no-op; nothing to swap
			}
			applied, err := swap(txCtx, alias)
			if err != nil {
				return fmt.Errorf("application: edit alias %s: %w", id, err)
			}
			if applied {
				return nil
			}
			// Lost the swap: re-read and re-apply from what actually exists.
		}
		return fmt.Errorf("application: edit alias %s: %w after %d attempts", id, ErrTransitionContended, casMaxAttempts)
	})
}

// ---------------------------------------------------------------------------
// alias group versions — the immutable end of the catalog
// ---------------------------------------------------------------------------

// OpenGroupVersion records a named group's next membership snapshot. The
// version number is never supplied: the use case reads the group's highest
// version and opens highest+1, so callers cannot invent a gap, skip a
// number, or reopen an old one — except through a race, which the
// (group_name, version) uniqueness turns into ErrGroupVersionExists and
// this loop turns into a re-read.
//
// The retry spans separate transactions, and that is forced, not chosen: a
// uniqueness violation does not merely fail a statement, it aborts the
// PostgreSQL transaction carrying it, so the losing attempt's unit of work
// is over the moment it loses. Each attempt is therefore its own unit of
// work; the loop lives outside. That guarantee holds while this use case
// owns its units of work: a caller already inside a joined unit of work
// turns the first lost race into that unit's abort — the violation kills the
// caller's transaction and the retry re-joins it aborted. The wiring rule
// for the phase that calls this: OpenGroupVersion runs outside any open unit
// of work. Two concurrent opens of the same group both
// succeed — one at n+1, one at n+2 — and both snapshots are real versions;
// nothing about monotonicity asks that only one open win.
func (c *Catalog) OpenGroupVersion(ctx context.Context, groupName string, members []catalog.AliasID) (*catalog.AliasGroupVersion, error) {
	id, err := catalog.NewGroupVersionID()
	if err != nil {
		return nil, fmt.Errorf("application: open group version: %w", err)
	}
	var opened *catalog.AliasGroupVersion
	for attempt := 0; attempt < casMaxAttempts; attempt++ {
		err = c.store.WithinTx(ctx, func(txCtx context.Context) error {
			highest, err := c.versions.HighestVersion(txCtx, groupName)
			if err != nil {
				return fmt.Errorf("application: open group version: read highest: %w", err)
			}
			version, err := catalog.NewGroupVersion(id, groupName, highest+1, members, time.Now())
			if err != nil {
				return err // domain rejections are terminal; no retry helps
			}
			if err := c.versions.Create(txCtx, version); err != nil {
				return err
			}
			opened = version
			return nil
		})
		if err == nil {
			return opened, nil
		}
		if !errors.Is(err, catalog.ErrGroupVersionExists) {
			return nil, fmt.Errorf("application: open group version %s: %w", groupName, err)
		}
		// Lost the version race: the next attempt re-reads highest and
		// opens the next number after whatever actually exists now.
	}
	return nil, fmt.Errorf("application: open group version %s: %w after %d attempts", groupName, ErrTransitionContended, casMaxAttempts)
}

// OpenWildcardVersion records the wildcard group's membership snapshot —
// every alias, encoded as the reserved name, version 1, and an empty member
// set. It exists once: a second call is refused with the domain's
// ErrGroupVersionExists and no retry, because "already there" is the
// wildcard's finished state, not a race this caller can resolve by trying
// again.
func (c *Catalog) OpenWildcardVersion(ctx context.Context) (*catalog.AliasGroupVersion, error) {
	id, err := catalog.NewGroupVersionID()
	if err != nil {
		return nil, fmt.Errorf("application: open wildcard version: %w", err)
	}
	version, err := catalog.NewWildcardGroupVersion(id, time.Now())
	if err != nil {
		return nil, fmt.Errorf("application: open wildcard version: %w", err)
	}
	if err := c.store.WithinTx(ctx, func(txCtx context.Context) error {
		return c.versions.Create(txCtx, version)
	}); err != nil {
		return nil, fmt.Errorf("application: open wildcard version: %w", err)
	}
	return version, nil
}

// GroupVersion returns the snapshot named by group and version, members and
// all. This is the read the Control Plane's entitlement scope resolves to
// over the management chain — by id-pair here, by the version's id over the
// wire.
func (c *Catalog) GroupVersion(ctx context.Context, groupName string, version int) (*catalog.AliasGroupVersion, error) {
	snapshot, err := c.versions.ByGroupAndVersion(ctx, groupName, version)
	if err != nil {
		return nil, fmt.Errorf("application: read group version %s@%d: %w", groupName, version, err)
	}
	return snapshot, nil
}

// CurrentGroupVersion returns the snapshot a Control Plane entitlement pins:
// the group's highest version, because that is what "current" means in this
// catalog — OpenGroupVersion above opens highest+1 and never edits what
// exists, so the newest number is by construction the snapshot a roll would
// have pinned, and the schema's column comment states the same rule in its
// own vocabulary. The read is two statements — the highest number, then the
// snapshot at it — and they are deliberately not pinned into one instant:
// versions are immutable, so the worst a concurrent open between the two
// statements can produce is the snapshot that was current an instant earlier,
// which is still a real version whose membership never changes. A caller that
// pins that id pinned something true; the roll that raced it belongs to the
// next grant, not to a retry of this read.
//
// A group with no version at all is an answer, not a failure of the read: a
// Control Plane asks before the operator has provisioned, and the wildcard
// `*` is only another name here — it is created by OpenWildcardVersion like
// any other row, so an unprovisioned `*` misses exactly the way an
// unprovisioned named group does. The outcome is this package's NotFound,
// which the management surface maps to its 404; everything else — a store
// that will not answer, a statement that will not run — stays a wrapped
// infrastructure error, because a caller must not be invited to treat "the
// catalog is down" as "this group does not exist" and pin a scope on the
// difference.
func (c *Catalog) CurrentGroupVersion(ctx context.Context, groupName string) (*catalog.AliasGroupVersion, error) {
	highest, err := c.versions.HighestVersion(ctx, groupName)
	if err != nil {
		return nil, fmt.Errorf("application: read current group version %s: %w", groupName, err)
	}
	if highest == 0 {
		// The port's 0 — the same 0 that lets `highest + 1` open version 1
		// without a special case — is here the not-found it means.
		return nil, NotFound("no version of the requested alias group exists")
	}
	snapshot, err := c.versions.ByGroupAndVersion(ctx, groupName, highest)
	if err != nil {
		return nil, fmt.Errorf("application: read current group version %s@%d: %w", groupName, highest, err)
	}
	return snapshot, nil
}

// CurrentGroupVersion on App is the shape the management listener calls. The
// method is one receiver wide on purpose, the same discipline
// ReadUsageEvents holds: inbound adapters call the application, not each
// other's use-case types, so a surface's vocabulary stays the boundary's own
// and a handler cannot reach past it into a use-case struct it half-owns.
// It returns the domain snapshot whole and lets the transport choose the
// three fields the private protocol carries — the member set is not among
// them, because which aliases a version contains is admission's question,
// asked inside this process, never the Control Plane's over this read.
func (app *App) CurrentGroupVersion(ctx context.Context, groupName string) (*catalog.AliasGroupVersion, error) {
	return app.catalog.CurrentGroupVersion(ctx, groupName)
}
