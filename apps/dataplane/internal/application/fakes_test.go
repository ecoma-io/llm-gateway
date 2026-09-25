package application

import (
	"context"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The fakes the catalog tests are written against, and why they are one
// world rather than three independent mocks.
//
// The property under test is a relationship: the repositories move the same
// state the store can roll back, and every one of them must run inside the
// unit of work the use case opened. fakeCatalogWorld is that shared state —
// durable-ish rows a rollback restores, counters the no-op short-circuits
// must not advance, and knobs that make a compare-and-swap lose or a
// uniqueness collision fire. The store embeds persistence.Store instead of
// implementing its query surface, exactly as the Control Plane's fakes do:
// the arch rules keep database/sql out of this package, and a fake Querier
// would have to name *sql.Rows to satisfy the port.
type catalogWorld struct {
	// order is the observation log: "begin", "commit", "rollback", in the
	// order they happened.
	order []string

	// outsideTx counts repository writes that arrived without the
	// transaction's context — the mechanical check that the use case's
	// writes ran inside the unit of work, not beside it.
	outsideTx int

	backends map[catalog.BackendID]*catalog.Backend
	aliases  map[catalog.AliasID]*catalog.ModelAlias
	versions map[string][]*catalog.AliasGroupVersion
	members  map[catalog.GroupVersionID][]catalog.AliasID

	// competitorFloor records version numbers other writers committed while
	// one of this world's units of work was failing — state that survives
	// the rollback, because it was never this transaction's work.
	competitorFloor map[string]int

	// Counters the tests read to prove a short-circuit fired: a no-op move
	// must reach no swap, and a retry loop must be observable as attempts.
	backendSwaps   int
	aliasSwaps     int
	retireCalls    int
	versionCreates int

	// The knobs each test sets.
	contendedBackendSwaps int // the first N backend swaps report "lost"
	contendedAliasSwaps   int // the first N alias guards report "lost"
	versionRace           int // the first N version inserts lose the race
	aliasInsertFailure    error
	nameTaken             bool
}

func newCatalogWorld() *catalogWorld {
	return &catalogWorld{
		backends:        map[catalog.BackendID]*catalog.Backend{},
		aliases:         map[catalog.AliasID]*catalog.ModelAlias{},
		versions:        map[string][]*catalog.AliasGroupVersion{},
		members:         map[catalog.GroupVersionID][]catalog.AliasID{},
		competitorFloor: map[string]int{},
	}
}

// newCatalog wires the use case over one world, the way every test builds it,
// so the ports always describe the same flow.
func newCatalog(world *catalogWorld) *Catalog {
	store := fakeCatalogStore{world: world}
	return NewCatalog(store, fakeBackends{world}, fakeAliases{world}, fakeVersions{world})
}

// commitCount counts the units of work that landed.
func (w *catalogWorld) commitCount() int {
	n := 0
	for _, entry := range w.order {
		if entry == "commit" {
			n++
		}
	}
	return n
}

func (w *catalogWorld) rollbackCount() int {
	n := 0
	for _, entry := range w.order {
		if entry == "rollback" {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// the store
// ---------------------------------------------------------------------------

// fakeCatalogStore is the unit of work in miniature: it snapshots the world
// at begin, restores it when the callback fails, and hands the callback a
// context carrying the marker the repositories use to prove they were given
// the transaction's context.
type fakeCatalogStore struct {
	persistence.Store
	world *catalogWorld
}

func (s fakeCatalogStore) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	s.world.order = append(s.world.order, "begin")
	backends := snapshotBackends(s.world.backends)
	aliases := snapshotAliases(s.world.aliases)
	versions := snapshotVersions(s.world.versions)
	members := snapshotMembers(s.world.members)
	if err := fn(context.WithValue(ctx, catalogTxKey{}, true)); err != nil {
		s.world.backends = backends
		s.world.aliases = aliases
		s.world.versions = versions
		s.world.members = members
		s.world.order = append(s.world.order, "rollback")
		return err
	}
	s.world.order = append(s.world.order, "commit")
	return nil
}

type catalogTxKey struct{}

func inCatalogTx(ctx context.Context) bool {
	marked, ok := ctx.Value(catalogTxKey{}).(bool)
	return ok && marked
}

func snapshotBackends(src map[catalog.BackendID]*catalog.Backend) map[catalog.BackendID]*catalog.Backend {
	dst := make(map[catalog.BackendID]*catalog.Backend, len(src))
	for id, backend := range src {
		clone := *backend
		dst[id] = &clone
	}
	return dst
}

func snapshotAliases(src map[catalog.AliasID]*catalog.ModelAlias) map[catalog.AliasID]*catalog.ModelAlias {
	dst := make(map[catalog.AliasID]*catalog.ModelAlias, len(src))
	for id, alias := range src {
		dst[id] = cloneAlias(alias)
	}
	return dst
}

func snapshotVersions(src map[string][]*catalog.AliasGroupVersion) map[string][]*catalog.AliasGroupVersion {
	dst := make(map[string][]*catalog.AliasGroupVersion, len(src))
	for group, list := range src {
		clones := make([]*catalog.AliasGroupVersion, len(list))
		for i, version := range list {
			clone := *version
			clone.Members = append([]catalog.AliasID(nil), version.Members...)
			clones[i] = &clone
		}
		dst[group] = clones
	}
	return dst
}

func snapshotMembers(src map[catalog.GroupVersionID][]catalog.AliasID) map[catalog.GroupVersionID][]catalog.AliasID {
	dst := make(map[catalog.GroupVersionID][]catalog.AliasID, len(src))
	for id, list := range src {
		dst[id] = append([]catalog.AliasID(nil), list...)
	}
	return dst
}

func cloneAlias(alias *catalog.ModelAlias) *catalog.ModelAlias {
	clone := *alias
	clone.Candidates = append([]catalog.Candidate(nil), alias.Candidates...)
	if alias.RetiredAt != nil {
		stamp := *alias.RetiredAt
		clone.RetiredAt = &stamp
	}
	return &clone
}

// ---------------------------------------------------------------------------
// backends
// ---------------------------------------------------------------------------

type fakeBackends struct{ world *catalogWorld }

func (f fakeBackends) Create(ctx context.Context, backend *catalog.Backend) error {
	if !inCatalogTx(ctx) {
		f.world.outsideTx++
	}
	clone := *backend
	f.world.backends[backend.ID] = &clone
	return nil
}

func (f fakeBackends) ByID(ctx context.Context, id catalog.BackendID) (*catalog.Backend, error) {
	backend, ok := f.world.backends[id]
	if !ok {
		return nil, fmt.Errorf("fake: backend %s: %w", id, persistence.ErrNotFound)
	}
	clone := *backend
	return &clone, nil
}

func (f fakeBackends) TransitionState(ctx context.Context, id catalog.BackendID, from, to catalog.BackendState, updatedAt time.Time) (bool, error) {
	if !inCatalogTx(ctx) {
		f.world.outsideTx++
	}
	f.world.backendSwaps++
	backend, ok := f.world.backends[id]
	if !ok || backend.State != from {
		return false, nil // the guard is the whole CAS: no row, no move
	}
	if f.world.contendedBackendSwaps > 0 {
		f.world.contendedBackendSwaps--
		return false, nil // someone else got there first; the caller re-reads
	}
	backend.State = to
	backend.UpdatedAt = updatedAt
	return true, nil
}

func (f fakeBackends) UpdateTarget(ctx context.Context, id catalog.BackendID, endpoint, credentialsRef, egressPolicyRef string, updatedAt time.Time) (bool, error) {
	if !inCatalogTx(ctx) {
		f.world.outsideTx++
	}
	if backend, ok := f.world.backends[id]; ok {
		backend.Endpoint = endpoint
		backend.CredentialsRef = credentialsRef
		backend.EgressPolicyRef = egressPolicyRef
		backend.UpdatedAt = updatedAt
		return true, nil
	}
	return false, nil // the port reports the miss, and so does the fake
}

// ---------------------------------------------------------------------------
// aliases
// ---------------------------------------------------------------------------

type fakeAliases struct{ world *catalogWorld }

func (f fakeAliases) Create(ctx context.Context, alias *catalog.ModelAlias) error {
	if !inCatalogTx(ctx) {
		f.world.outsideTx++
	}
	if f.world.nameTaken {
		// A uniqueness collision inserts nothing: the row the caller read
		// as absent was created by someone else before this insert ran.
		return fmt.Errorf("fake: create alias %s: %w", alias.ID, catalog.ErrAliasNameTaken)
	}
	f.world.aliases[alias.ID] = cloneAlias(alias)
	if f.world.aliasInsertFailure != nil {
		// The alias row is on disk; the candidate insert blows up. What the
		// rollback must show is that neither survives.
		return f.world.aliasInsertFailure
	}
	return nil
}

func (f fakeAliases) ByID(ctx context.Context, id catalog.AliasID) (*catalog.ModelAlias, error) {
	alias, ok := f.world.aliases[id]
	if !ok {
		return nil, fmt.Errorf("fake: alias %s: %w", id, persistence.ErrNotFound)
	}
	return cloneAlias(alias), nil
}

func (f fakeAliases) Retire(ctx context.Context, id catalog.AliasID, from catalog.AliasState, retiredAt time.Time) (bool, error) {
	if !inCatalogTx(ctx) {
		f.world.outsideTx++
	}
	f.world.retireCalls++
	alias, ok := f.world.aliases[id]
	if !ok || alias.State != from {
		return false, nil
	}
	if f.world.contendedAliasSwaps > 0 {
		f.world.contendedAliasSwaps--
		return false, nil
	}
	stamp := retiredAt
	alias.State = catalog.AliasRetired
	alias.RetiredAt = &stamp
	alias.UpdatedAt = retiredAt
	return true, nil
}

func (f fakeAliases) SetCandidates(ctx context.Context, id catalog.AliasID, candidates []catalog.Candidate, updatedAt time.Time) (bool, error) {
	if !inCatalogTx(ctx) {
		f.world.outsideTx++
	}
	alias, ok := f.world.aliases[id]
	if !ok || alias.State != catalog.AliasActive {
		// The guard is the whole CAS: a retired (or vanished) alias edits
		// nothing and answers "lost".
		return false, nil
	}
	if f.world.contendedAliasSwaps > 0 {
		f.world.contendedAliasSwaps--
		return false, nil
	}
	alias.Candidates = append([]catalog.Candidate(nil), candidates...)
	alias.UpdatedAt = updatedAt
	return true, nil
}

func (f fakeAliases) UpdateBounds(ctx context.Context, id catalog.AliasID, from catalog.AliasState, maxOutputTokens, reservationCap int64, updatedAt time.Time) (bool, error) {
	if !inCatalogTx(ctx) {
		f.world.outsideTx++
	}
	f.world.aliasSwaps++
	alias, ok := f.world.aliases[id]
	if !ok || alias.State != from {
		return false, nil
	}
	if f.world.contendedAliasSwaps > 0 {
		f.world.contendedAliasSwaps--
		return false, nil
	}
	alias.MaxOutputTokens = maxOutputTokens
	alias.ReservationCap = reservationCap
	alias.UpdatedAt = updatedAt
	return true, nil
}

// ---------------------------------------------------------------------------
// alias group versions
// ---------------------------------------------------------------------------

type fakeVersions struct{ world *catalogWorld }

func (f fakeVersions) Create(ctx context.Context, version *catalog.AliasGroupVersion) error {
	if !inCatalogTx(ctx) {
		f.world.outsideTx++
	}
	// The fake models the schema's unique (group_name, version) index: a
	// second snapshot at a number that is taken — by an earlier unit of
	// this world, or by a competitor the race knob simulates — is refused
	// exactly as PostgreSQL refuses it.
	if f.world.competitorFloor[version.GroupName] >= version.Version {
		return fmt.Errorf("fake: create group version %s@%d: %w", version.GroupName, version.Version, catalog.ErrGroupVersionExists)
	}
	for _, existing := range f.world.versions[version.GroupName] {
		if existing.Version == version.Version {
			return fmt.Errorf("fake: create group version %s@%d: %w", version.GroupName, version.Version, catalog.ErrGroupVersionExists)
		}
	}
	f.world.versionCreates++
	if f.world.versionRace > 0 {
		f.world.versionRace--
		// The competitor commits the very number this attempt chose, under
		// its own transaction: the unique index fires here, and the
		// competitor's row is part of the world from now on — which is why
		// it is recorded on the floor, not in this attempt's rolled-back
		// unit of work.
		if f.world.competitorFloor[version.GroupName] < version.Version {
			f.world.competitorFloor[version.GroupName] = version.Version
		}
		return fmt.Errorf("fake: create group version %s@%d: %w", version.GroupName, version.Version, catalog.ErrGroupVersionExists)
	}
	clone := *version
	clone.Members = append([]catalog.AliasID(nil), version.Members...)
	f.world.versions[version.GroupName] = append(f.world.versions[version.GroupName], &clone)
	f.world.members[version.ID] = append([]catalog.AliasID(nil), version.Members...)
	return nil
}

func (f fakeVersions) ByGroupAndVersion(ctx context.Context, groupName string, version int) (*catalog.AliasGroupVersion, error) {
	for _, candidate := range f.world.versions[groupName] {
		if candidate.Version == version {
			clone := *candidate
			clone.Members = append([]catalog.AliasID(nil), candidate.Members...)
			return &clone, nil
		}
	}
	return nil, fmt.Errorf("fake: group version %s@%d: %w", groupName, version, persistence.ErrNotFound)
}

func (f fakeVersions) HighestVersion(ctx context.Context, groupName string) (int, error) {
	highest := f.world.competitorFloor[groupName]
	for _, version := range f.world.versions[groupName] {
		if version.Version > highest {
			highest = version.Version
		}
	}
	return highest, nil
}
