package application

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/executors"
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
	highestReadFailure    error
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

func (f fakeAliases) ByName(ctx context.Context, name string) (*catalog.ModelAlias, error) {
	for _, alias := range f.world.aliases {
		if alias.Name == name {
			return cloneAlias(alias), nil
		}
	}
	return nil, fmt.Errorf("fake: alias %s: %w", name, persistence.ErrNotFound)
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
	if f.world.highestReadFailure != nil {
		return 0, f.world.highestReadFailure
	}
	highest := f.world.competitorFloor[groupName]
	for _, version := range f.world.versions[groupName] {
		if version.Version > highest {
			highest = version.Version
		}
	}
	return highest, nil
}

// ---------------------------------------------------------------------------
// the admission world
// ---------------------------------------------------------------------------

// The admission tests share one world for the same reason the catalog's do,
// with one addition: the admission use case opens several units of work per
// request (the early refusals open their own), so the world's event log is
// the only place the whole sequence — which unit wrote what, and which unit
// the replay record closed — can be observed in order. The knobs make a
// unique-key race fire, make the release's compare-and-swap lose, and make a
// drawdown fall short; the store restores the snapshot a rollback owes.
type admissionWorld struct {
	// mu makes the world safe for the concurrent-routing tests: a hundred
	// requests admitted and routed over one world is the shape the race
	// detector is asked about, and the fakes answer it by guarding the
	// shared state they move. Single-threaded tests lock it too; the cost
	// is nothing and the rules stay uniform.
	mu sync.Mutex

	// events is the observation log: "begin", "commit", "rollback", and one
	// entry per repository call that decides, in the order it happened. The
	// write-order assertions read this list, never the maps.
	events    []string
	outsideTx int

	// now is the world's clock. The use case reads it through the txClock
	// seam (the production clock is a SELECT this package cannot fake), and
	// every stamp a test asserts was derived from this instant.
	now time.Time

	credentials   map[string]persistence.CredentialView
	aliasesByName map[string]*catalog.ModelAlias
	prices        map[catalog.AliasID]catalog.PriceSnapshot
	backends      map[catalog.BackendID]catalog.Backend
	requests      map[identity.RequestID]execution.Request
	attempts      []execution.Attempt
	intakes       map[string]execution.Intake
	reservations  map[identity.ReservationID]accounting.Reservation
	buckets       []*admissionBucket
	facts         []accounting.Fact
	appendSeq     int64
	returned      [][]accounting.Allocation

	// The knobs each test sets. The failure knobs with a countdown fire for
	// the first N calls only, so a retry policy can be watched running out of
	// luck; the ones without fire for every call.
	credentialFailure    error
	credentialLookups    int
	aliasFailure         error
	priceFailure         error
	priceMissing         bool
	drawdownFailure      error
	drawdownFailures     int
	returnFailure        error
	returnFailures       int
	reservationDuplicate bool
	clockFailure         error
	seamCloseLost        bool
	requestFinaliseLost  bool   // the compensation's CAS won, but the request row did not finalise
	attemptDuplicate     bool   // the attempt insert races a writer that already persisted the row
	intakePreFinalised   bool   // the replay record is already terminal when the orphan tail reads it
	intakeRace           int    // the first N intake inserts lose the unique race
	intakeRaceWinner     string // "", "in_flight", or "rejected"

	// raceWinners holds the rows a simulated competitor committed. They live
	// outside the snapshot on purpose: a winner's commit is another unit's
	// durability, so this unit's rollback must not take it back — restore
	// merges them into the intakes map after rewinding.
	raceWinners map[string]execution.Intake
}

// admissionBucket is one quota projection row in miniature: whose account it
// funds, how much is available, and whether its scope contains the alias
// asking to be funded. The slice's order IS the waterfall order.
type admissionBucket struct {
	account   string
	id        string
	available int64
	eligible  bool
}

func newAdmissionWorld() *admissionWorld {
	return &admissionWorld{
		now:           time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC),
		credentials:   map[string]persistence.CredentialView{},
		aliasesByName: map[string]*catalog.ModelAlias{},
		prices:        map[catalog.AliasID]catalog.PriceSnapshot{},
		backends:      map[catalog.BackendID]catalog.Backend{},
		requests:      map[identity.RequestID]execution.Request{},
		intakes:       map[string]execution.Intake{},
		reservations:  map[identity.ReservationID]accounting.Reservation{},
		raceWinners:   map[string]execution.Intake{},
	}
}

// newAdmission wires the use case over one world the way every admission test
// builds it, so the ports always describe the same flow. The clock is the
// world's own: the production clock runs a SELECT this package cannot fake,
// which is the seam's whole reason.
func newAdmission(world *admissionWorld) *ChatAdmission {
	useCase := NewChatAdmission(
		fakeAdmissionStore{world: world},
		fakeAdmissionCredentials{world: world},
		fakeAdmissionAliases{world: world},
		fakeAdmissionPrices{world: world},
		fakeAdmissionRequests{world: world},
		fakeAdmissionIntakes{world: world},
		fakeAdmissionReservations{world: world},
		fakeAdmissionLedger{world: world},
		fakeAdmissionFacts{world: world},
		AdmissionConfig{HoldWindow: time.Minute, LeaseTTL: 30 * time.Second, LeaseOwner: "test-host:1"},
	)
	useCase.clock = admissionClock{world}
	return useCase
}

func newCredentialAuthenticator(world *admissionWorld) *CredentialAuthenticator {
	return NewCredentialAuthenticator(fakeAdmissionCredentials{world: world})
}

// note appends one entry to the observation log. Every fake goes through it,
// so the log is ordered under the same lock the state it describes is.
func (w *admissionWorld) note(event string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, event)
}

func (w *admissionWorld) commitCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, entry := range w.events {
		if entry == "commit" {
			n++
		}
	}
	return n
}

func (w *admissionWorld) rollbackCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, entry := range w.events {
		if entry == "rollback" {
			n++
		}
	}
	return n
}

func (w *admissionWorld) happened(event string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, entry := range w.events {
		if entry == event {
			return true
		}
	}
	return false
}

// count is happened's tally: how many times one entry appears in the log —
// the shape a budget assertion reads (a whole retry budget is three closes,
// not one).
func (w *admissionWorld) count(event string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, entry := range w.events {
		if entry == event {
			n++
		}
	}
	return n
}

func (w *admissionWorld) intakeKey(accountID, idempotencyKey string) string {
	return accountID + "\x00" + idempotencyKey
}

// ---------------------------------------------------------------------------
// admission seeds
// ---------------------------------------------------------------------------

func (w *admissionWorld) seedCredential(keyID, accountID, digest, keyState string, accountState *string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	view := persistence.CredentialView{
		Digest:    digest,
		AccountID: accountID,
		KeyState:  keyState,
	}
	if accountState != nil {
		state := *accountState
		view.AccountState = &state
	}
	w.credentials[keyID] = view
}

func (w *admissionWorld) seedAlias(name string, maxOutputTokens, reservationCap int64) *catalog.ModelAlias {
	w.mu.Lock()
	defer w.mu.Unlock()
	alias := &catalog.ModelAlias{
		ID:              catalog.AliasID("alias-" + name),
		Name:            name,
		State:           catalog.AliasActive,
		MaxOutputTokens: maxOutputTokens,
		ReservationCap:  reservationCap,
	}
	w.aliasesByName[name] = alias
	return alias
}

// seedCandidates gives a seeded alias the candidate list the routing walk
// reads. The alias is the world's own row — the fakes hand out clones, so
// this edits the one truth the clones come from.
func (w *admissionWorld) seedCandidates(name string, candidates ...catalog.Candidate) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.aliasesByName[name].Candidates = candidates
}

func (w *admissionWorld) seedBackend(id catalog.BackendID, state catalog.BackendState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.backends[id] = catalog.Backend{ID: id, State: state}
}

func (w *admissionWorld) seedPrice(aliasID catalog.AliasID, snapshot catalog.PriceSnapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prices[aliasID] = snapshot
}

func (w *admissionWorld) seedBucket(account, id string, available int64, eligible bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buckets = append(w.buckets, &admissionBucket{account: account, id: id, available: available, eligible: eligible})
}

func (w *admissionWorld) available(bucketID string) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, bucket := range w.buckets {
		if bucket.id == bucketID {
			return bucket.available
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// the admission store
// ---------------------------------------------------------------------------

// fakeAdmissionStore is the unit of work in miniature, as its catalog sibling
// is: it snapshots the world at begin, restores it when the callback fails,
// and marks the context the repositories check. It deliberately implements no
// Querier — the arch rules keep database/sql out of this package, tests
// included, and the txClock seam is the answer this use case found for that.
type fakeAdmissionStore struct {
	persistence.Store
	world *admissionWorld
}

func (s fakeAdmissionStore) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	// The real store's BeginTx refuses a context that is already done, and
	// the fake refuses it the same way: it is what makes an ending's
	// detachment visible — a settle that still runs after the caller's
	// context died, and could not have run on it.
	if err := ctx.Err(); err != nil {
		return err
	}
	s.world.note("begin")
	snapshot := s.world.snapshot()
	if err := fn(context.WithValue(ctx, admissionTxKey{}, true)); err != nil {
		s.world.restore(snapshot)
		s.world.note("rollback")
		return err
	}
	s.world.note("commit")
	return nil
}

func (s fakeAdmissionStore) InUnitOfWork(ctx context.Context) bool {
	marked, ok := ctx.Value(admissionTxKey{}).(bool)
	return ok && marked
}

type admissionTxKey struct{}

func inAdmissionTx(ctx context.Context) bool {
	marked, ok := ctx.Value(admissionTxKey{}).(bool)
	return ok && marked
}

// admissionSnapshot is what a rollback owes: every map and slice the unit
// could have touched, copied before the unit ran.
type admissionSnapshot struct {
	credentials   map[string]persistence.CredentialView
	aliasesByName map[string]*catalog.ModelAlias
	prices        map[catalog.AliasID]catalog.PriceSnapshot
	requests      map[identity.RequestID]execution.Request
	attempts      []execution.Attempt
	intakes       map[string]execution.Intake
	reservations  map[identity.ReservationID]accounting.Reservation
	buckets       []admissionBucket
	facts         []accounting.Fact
	appendSeq     int64
	returned      [][]accounting.Allocation
}

func (w *admissionWorld) snapshot() admissionSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	snap := admissionSnapshot{
		credentials:   make(map[string]persistence.CredentialView, len(w.credentials)),
		aliasesByName: make(map[string]*catalog.ModelAlias, len(w.aliasesByName)),
		prices:        make(map[catalog.AliasID]catalog.PriceSnapshot, len(w.prices)),
		requests:      make(map[identity.RequestID]execution.Request, len(w.requests)),
		intakes:       make(map[string]execution.Intake, len(w.intakes)),
		reservations:  make(map[identity.ReservationID]accounting.Reservation, len(w.reservations)),
	}
	for keyID, view := range w.credentials {
		snap.credentials[keyID] = cloneCredentialView(view)
	}
	for name, alias := range w.aliasesByName {
		snap.aliasesByName[name] = cloneAlias(alias)
	}
	for aliasID, price := range w.prices {
		snap.prices[aliasID] = price
	}
	for id, request := range w.requests {
		snap.requests[id] = request
	}
	snap.attempts = append([]execution.Attempt(nil), w.attempts...)
	for key, intake := range w.intakes {
		snap.intakes[key] = cloneAdmissionIntake(intake)
	}
	for id, reservation := range w.reservations {
		snap.reservations[id] = cloneAdmissionReservation(reservation)
	}
	snap.buckets = make([]admissionBucket, len(w.buckets))
	for i, bucket := range w.buckets {
		snap.buckets[i] = *bucket
	}
	snap.facts = append([]accounting.Fact(nil), w.facts...)
	snap.appendSeq = w.appendSeq
	snap.returned = append([][]accounting.Allocation(nil), w.returned...)
	return snap
}

func (w *admissionWorld) restore(snap admissionSnapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.credentials = snap.credentials
	w.aliasesByName = snap.aliasesByName
	w.prices = snap.prices
	w.requests = snap.requests
	w.attempts = snap.attempts
	w.intakes = snap.intakes
	w.reservations = snap.reservations
	w.buckets = make([]*admissionBucket, len(snap.buckets))
	for i, bucket := range snap.buckets {
		clone := bucket
		w.buckets[i] = &clone
	}
	w.facts = snap.facts
	w.appendSeq = snap.appendSeq
	w.returned = snap.returned
	// A competitor's committed row survives this unit's rollback: the rewind
	// puts the begin-time map back, and the winners are merged in on top of
	// it, exactly as they would still be there in a real database.
	for key, winner := range w.raceWinners {
		w.intakes[key] = cloneAdmissionIntake(winner)
	}
}

func cloneCredentialView(view persistence.CredentialView) persistence.CredentialView {
	clone := view
	if view.AccountState != nil {
		state := *view.AccountState
		clone.AccountState = &state
	}
	if view.RevokedAt != nil {
		at := *view.RevokedAt
		clone.RevokedAt = &at
	}
	return clone
}

func cloneAdmissionIntake(intake execution.Intake) execution.Intake {
	clone := intake
	if intake.FinalStatus != nil {
		status := *intake.FinalStatus
		clone.FinalStatus = &status
	}
	return clone
}

func cloneAdmissionReservation(reservation accounting.Reservation) accounting.Reservation {
	clone := reservation
	clone.Allocations = append([]accounting.Allocation(nil), reservation.Allocations...)
	return clone
}

// ---------------------------------------------------------------------------
// admission reads
// ---------------------------------------------------------------------------

type fakeAdmissionCredentials struct{ world *admissionWorld }

func (f fakeAdmissionCredentials) Lookup(ctx context.Context, keyID string) (persistence.CredentialView, error) {
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	f.world.credentialLookups++
	if f.world.credentialFailure != nil {
		return persistence.CredentialView{}, f.world.credentialFailure
	}
	view, ok := f.world.credentials[keyID]
	if !ok {
		return persistence.CredentialView{}, fmt.Errorf("fake: credential %s: %w", keyID, persistence.ErrCredentialNotFound)
	}
	return cloneCredentialView(view), nil
}

// The alias, reservation and ledger fakes embed their port's interface so the
// methods this use case never calls need no fake body; a call on one of them
// would panic on the nil embedded value, which is the honest answer for a
// method nothing in the admission flow should reach.
type fakeAdmissionAliases struct {
	persistence.ModelAliases
	world *admissionWorld
}

func (f fakeAdmissionAliases) ByName(ctx context.Context, name string) (*catalog.ModelAlias, error) {
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if f.world.aliasFailure != nil {
		return nil, f.world.aliasFailure
	}
	alias, ok := f.world.aliasesByName[name]
	if !ok {
		return nil, fmt.Errorf("fake: alias %s: %w", name, persistence.ErrNotFound)
	}
	return cloneAlias(alias), nil
}

type fakeAdmissionPrices struct{ world *admissionWorld }

func (f fakeAdmissionPrices) EffectiveAt(ctx context.Context, aliasID catalog.AliasID) (catalog.PriceSnapshot, error) {
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if f.world.priceFailure != nil {
		return catalog.PriceSnapshot{}, f.world.priceFailure
	}
	if f.world.priceMissing {
		return catalog.PriceSnapshot{}, fmt.Errorf("fake: price for %s: %w", aliasID, persistence.ErrNoEffectivePrice)
	}
	snapshot, ok := f.world.prices[aliasID]
	if !ok {
		return catalog.PriceSnapshot{}, fmt.Errorf("fake: price for %s: %w", aliasID, persistence.ErrNoEffectivePrice)
	}
	return snapshot, nil
}

// admissionClock stands in for the storeClock: the world's fixed instant, and
// the same inside-a-unit-of-work check every repository fake runs.
type admissionClock struct{ world *admissionWorld }

func (c admissionClock) TransactionTimestamp(ctx context.Context) (time.Time, error) {
	if !inAdmissionTx(ctx) {
		c.world.outsideTx++
	}
	if c.world.clockFailure != nil {
		return time.Time{}, c.world.clockFailure
	}
	return c.world.now, nil
}

// ---------------------------------------------------------------------------
// admission writes
// ---------------------------------------------------------------------------

type fakeAdmissionRequests struct{ world *admissionWorld }

func (f fakeAdmissionRequests) Insert(ctx context.Context, request execution.Request) error {
	if !inAdmissionTx(ctx) {
		f.world.outsideTx++
	}
	f.world.note("request.insert")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	f.world.requests[request.ID] = request
	return nil
}

func (f fakeAdmissionRequests) Finalise(ctx context.Context, request execution.Request) (bool, error) {
	if !inAdmissionTx(ctx) {
		f.world.outsideTx++
	}
	f.world.note("request.finalise")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if f.world.requestFinaliseLost {
		// The store answered, and the row did not move: nobody won it, it
		// simply refused the ending. The caller must not read this as done.
		return false, nil
	}
	stored, ok := f.world.requests[request.ID]
	if !ok || stored.Status != execution.StatusExecuting {
		return false, nil // the row moved on: the loser reads the winner's decision
	}
	stored.Status = request.Status
	stored.RejectionReason = request.RejectionReason
	stored.FailureReason = request.FailureReason
	stored.CommittedAttemptID = request.CommittedAttemptID
	stored.FinishedAt = request.FinishedAt
	f.world.requests[request.ID] = stored
	return true, nil
}

type fakeAdmissionIntakes struct{ world *admissionWorld }

func (f fakeAdmissionIntakes) Insert(ctx context.Context, intake execution.Intake) error {
	if !inAdmissionTx(ctx) {
		f.world.outsideTx++
	}
	f.world.note("intake.insert")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	key := f.world.intakeKey(intake.AccountID, intake.IdempotencyKey)
	if _, ok := f.world.intakes[key]; ok {
		return fmt.Errorf("fake: intake for %s: %w", key, persistence.ErrDuplicateIntake)
	}
	if f.world.intakeRace > 0 {
		// The race: another unit's commit landed this key between the use
		// case's probe and this insert. The winner is recorded in the
		// competitor map — a committed row is another unit's durability, so it
		// survives the loser's rollback, which is the whole reason the loser
		// must re-read — and the insert answers the unique key.
		f.world.intakeRace--
		switch f.world.intakeRaceWinner {
		case "in_flight":
			f.world.raceWinners[key] = execution.Intake{
				AccountID:      intake.AccountID,
				IdempotencyKey: intake.IdempotencyKey,
				RequestDigest:  intake.RequestDigest,
				RequestID:      identity.RequestID("winner-in-flight"),
			}
		case "rejected":
			rejected := execution.FinalRejected
			f.world.raceWinners[key] = execution.Intake{
				AccountID:            intake.AccountID,
				IdempotencyKey:       intake.IdempotencyKey,
				RequestDigest:        intake.RequestDigest,
				RequestID:            identity.RequestID("winner-rejected"),
				FinalStatus:          &rejected,
				FinalRejectionReason: execution.RejectedAccountSuspended,
			}
		}
		return fmt.Errorf("fake: intake for %s: %w", key, persistence.ErrDuplicateIntake)
	}
	if f.world.intakePreFinalised {
		// The winner of the ending's race finalised this record between the
		// loser's lost close and the orphan tail's read — the skip branch's
		// premise, staged where that read will find it.
		final := execution.FinalSucceeded
		intake.FinalStatus = &final
	}
	f.world.intakes[key] = cloneAdmissionIntake(intake)
	return nil
}

func (f fakeAdmissionIntakes) Find(ctx context.Context, accountID, idempotencyKey string) (execution.Intake, error) {
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	intake, ok := f.world.intakes[f.world.intakeKey(accountID, idempotencyKey)]
	if !ok {
		return execution.Intake{}, fmt.Errorf("fake: intake for %s: %w", accountID, persistence.ErrNotFound)
	}
	return cloneAdmissionIntake(intake), nil
}

func (f fakeAdmissionIntakes) Finalise(ctx context.Context, accountID, idempotencyKey string, status execution.FinalStatus, rejection execution.RejectionReason, failure execution.FailureReason) (bool, error) {
	if !inAdmissionTx(ctx) {
		f.world.outsideTx++
	}
	f.world.note("intake.finalise")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	key := f.world.intakeKey(accountID, idempotencyKey)
	intake, ok := f.world.intakes[key]
	if !ok {
		return false, fmt.Errorf("fake: intake for %s: %w", key, persistence.ErrNotFound)
	}
	if intake.FinalStatus != nil {
		return false, nil // the pointer was written once, by whoever won it
	}
	if err := intake.Finalise(status, rejection, failure); err != nil {
		return false, err
	}
	f.world.intakes[key] = intake
	return true, nil
}

type fakeAdmissionReservations struct {
	persistence.ReservationRepository
	world *admissionWorld
}

func (f fakeAdmissionReservations) Insert(ctx context.Context, reservation accounting.Reservation) error {
	if !inAdmissionTx(ctx) {
		f.world.outsideTx++
	}
	f.world.note("reservation.insert")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if f.world.reservationDuplicate {
		return fmt.Errorf("fake: reservation %s: %w", reservation.ID, persistence.ErrDuplicateReservation)
	}
	f.world.reservations[reservation.ID] = cloneAdmissionReservation(reservation)
	return nil
}

func (f fakeAdmissionReservations) Close(ctx context.Context, id identity.ReservationID, state accounting.State, closedAt time.Time) (bool, error) {
	if !inAdmissionTx(ctx) {
		f.world.outsideTx++
	}
	f.world.note("reservation.close")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if f.world.seamCloseLost {
		return false, nil // another writer closed the hold first
	}
	reservation, ok := f.world.reservations[id]
	if !ok || reservation.State != accounting.StateOpen {
		return false, nil // the CAS is the whole claim to the ending
	}
	reservation.State = state
	reservation.ClosedAt = closedAt
	f.world.reservations[id] = reservation
	return true, nil
}

type fakeAdmissionLedger struct {
	persistence.QuotaProjectionRepository
	world *admissionWorld
}

func (f fakeAdmissionLedger) Drawdown(ctx context.Context, accountID string, aliasID catalog.AliasID, amount int64) ([]accounting.Allocation, error) {
	if !inAdmissionTx(ctx) {
		f.world.outsideTx++
	}
	f.world.note("ledger.drawdown")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if f.world.drawdownFailures > 0 {
		f.world.drawdownFailures--
		return nil, f.world.drawdownFailure
	}
	var eligible []*admissionBucket
	for _, bucket := range f.world.buckets {
		if bucket.account == accountID && bucket.eligible {
			eligible = append(eligible, bucket)
		}
	}
	var legs []accounting.Allocation
	remaining := amount
	for _, bucket := range eligible {
		if remaining == 0 {
			break
		}
		take := bucket.available
		if take > remaining {
			take = remaining
		}
		if take <= 0 {
			continue
		}
		bucket.available -= take
		remaining -= take
		legs = append(legs, accounting.Allocation{FundingBucketID: bucket.id, Amount: take, Ordinal: len(legs) + 1})
	}
	if remaining > 0 {
		// The giveback: a shortfall draws nothing and leaves nothing drawn.
		// The typed error carries the classification the use case refuses
		// on — whether the walk saw any grant it was allowed to draw from.
		for _, leg := range legs {
			for _, bucket := range eligible {
				if bucket.id == leg.FundingBucketID {
					bucket.available += leg.Amount
				}
			}
		}
		return nil, &persistence.InsufficientCapacityError{EligibleRowSeen: len(eligible) > 0}
	}
	return legs, nil
}

func (f fakeAdmissionLedger) Return(ctx context.Context, legs []accounting.Allocation) (int, error) {
	if !inAdmissionTx(ctx) {
		f.world.outsideTx++
	}
	f.world.note("ledger.return")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if f.world.returnFailures > 0 {
		f.world.returnFailures--
		return 0, f.world.returnFailure
	}
	f.world.returned = append(f.world.returned, append([]accounting.Allocation(nil), legs...))
	for _, leg := range legs {
		for _, bucket := range f.world.buckets {
			if bucket.id == leg.FundingBucketID {
				bucket.available += leg.Amount
			}
		}
	}
	return len(legs), nil
}

type fakeAdmissionFacts struct{ world *admissionWorld }

func (f fakeAdmissionFacts) Append(ctx context.Context, fact accounting.Fact) (int64, error) {
	if !inAdmissionTx(ctx) {
		f.world.outsideTx++
	}
	f.world.note("fact.append")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	f.world.appendSeq++
	fact.AppendSeq = f.world.appendSeq
	f.world.facts = append(f.world.facts, fact)
	return fact.AppendSeq, nil
}

// ---------------------------------------------------------------------------
// the routing stage's fakes
// ---------------------------------------------------------------------------

// fakeAdmissionAttempts is the attempt repository the routing tests share.
// The insert is deliberately legal in BOTH contexts the routing stage writes
// it in — standalone, for a pre-commitment failure (the observation of a call
// that happened, committed on its own), and inside the settle unit, for a
// committed one — which is why this fake runs no outside-unit check: the
// outside unit IS one of the shapes under test.
type fakeAdmissionAttempts struct {
	persistence.AttemptRepository
	world *admissionWorld
}

func (f fakeAdmissionAttempts) Insert(ctx context.Context, attempt execution.Attempt) error {
	f.world.note("attempt.insert")
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if f.world.attemptDuplicate {
		// The row is already on disk — the lost-commit-ack shape whose
		// sentinel reads as done.
		return fmt.Errorf("fake: attempt %s: %w", attempt.ID, persistence.ErrAttemptAlreadyAppended)
	}
	f.world.attempts = append(f.world.attempts, attempt)
	return nil
}

// fakeRoutingBackends is the backend half of the servable closure. Only ByID
// is reachable from the routing stage's pre-unit reads; every other method of
// the port panics on the nil embedded value, the honest answer for a call the
// walk must never make.
type fakeRoutingBackends struct {
	persistence.Backends
	world *admissionWorld

	// readFailure, when set, is what ByID answers instead of the world: the
	// read that failed, against which the servable question has no answer.
	readFailure error
}

func (f fakeRoutingBackends) ByID(ctx context.Context, id catalog.BackendID) (*catalog.Backend, error) {
	if f.readFailure != nil {
		return nil, f.readFailure
	}
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	backend, ok := f.world.backends[id]
	if !ok {
		return nil, fmt.Errorf("fake: backend %s: %w", id, persistence.ErrNotFound)
	}
	clone := backend
	return &clone, nil
}

// fakeReply is the Reply the routing tests serve through: the sink half an
// executor writes into and the ending half the walk calls, recording both.
// Its commitment is armed by the first Content call, exactly as the
// transport's reply arms its status line — the router trusts this fact over
// anything an executor claims.
type fakeReply struct {
	stream    bool
	opened    bool
	committed bool
	delivered []byte
	served    []string
	surfaced  execution.FailureReason
}

func (r *fakeReply) Content(chunk []byte) error {
	r.committed = true
	r.delivered = append(r.delivered, chunk...)
	return nil
}

func (r *fakeReply) Committed() bool        { return r.committed }
func (r *fakeReply) Delivered() []byte      { return r.delivered }
func (r *fakeReply) Open(stream bool)       { r.opened = true; r.stream = stream }
func (r *fakeReply) ServeSucceeded()        { r.served = append(r.served, "succeeded") }
func (r *fakeReply) ServeMidStreamFailure() { r.served = append(r.served, "mid_stream_failure") }

func (r *fakeReply) ServeSurfaced(failure execution.FailureReason) {
	r.surfaced = failure
	r.served = append(r.served, "surfaced")
}

func (r *fakeReply) ServeNoCandidate() { r.served = append(r.served, "no_candidate") }

// fakeExecutor is one registered candidate's stand-in: it answers from a
// script, consuming one result per call and repeating the last once the
// script runs out. Its act, when set, runs before the answer — writing a
// chunk through the sink is how the mid-stream shapes are reached. The mutex
// is what lets one executor stand behind a registry a hundred concurrent
// requests walk.
type fakeExecutor struct {
	mu      sync.Mutex
	results []executors.Result
	act     func(sink executors.Sink)
	calls   int
	specs   []executors.AttemptSpec
}

func (e *fakeExecutor) Execute(ctx context.Context, spec executors.AttemptSpec, sink executors.Sink) executors.Result {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	e.specs = append(e.specs, spec)
	if e.act != nil {
		e.act(sink)
	}
	if len(e.results) == 0 {
		// An empty script answers nil — the broken-contract shape, used
		// deliberately by the test that pins the router's verdict on it.
		return nil
	}
	result := e.results[0]
	if len(e.results) > 1 {
		e.results = e.results[1:]
	}
	return result
}

// okExec reports a completed call with the provider's own usage report.
func okExec(input, output int64) executors.Result {
	return executors.Success{
		Usage: executors.Usage{InputTokens: &input, OutputTokens: &output},
	}
}

// failExec reports a finished call that failed with a classified fault.
func failExec(class execution.ErrorClass) executors.Result {
	return executors.Failure{Class: class}
}
