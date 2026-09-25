package application

import (
	"errors"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

var errCandidateInsert = errors.New("fake: the candidate insert failed")

// newRegisteredBackend gives alias tests a backend on file.
func newRegisteredBackend(t *testing.T, cat *Catalog) *catalog.Backend {
	t.Helper()
	backend, err := cat.RegisterBackend(t.Context(), "openai-compatible", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("RegisterBackend returned error: %v", err)
	}
	return backend
}

// newDefinedAlias gives alias-edit tests an alias on file.
func newDefinedAlias(t *testing.T, cat *Catalog, backendID catalog.BackendID) *catalog.ModelAlias {
	t.Helper()
	alias, err := cat.DefineAlias(t.Context(), "gpt-5", 4096, 1024, []catalog.Candidate{
		{BackendID: backendID, ProviderModel: "gpt-5"},
	})
	if err != nil {
		t.Fatalf("DefineAlias returned error: %v", err)
	}
	return alias
}

// ---------------------------------------------------------------------------
// backends
// ---------------------------------------------------------------------------

func TestRegisterBackendPersistsTheBirthState(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	backend := newRegisteredBackend(t, cat)
	if backend.State != catalog.BackendActive {
		t.Errorf("State = %q, want the birth state active", backend.State)
	}
	stored, err := cat.Backend(t.Context(), backend.ID)
	if err != nil {
		t.Fatalf("Backend returned error: %v", err)
	}
	if stored.Endpoint != backend.Endpoint || stored.AdapterType != backend.AdapterType {
		t.Errorf("stored backend = %+v, want what RegisterBackend returned", stored)
	}
	if world.rollbackCount() != 0 {
		t.Errorf("a successful register rolled back %d times", world.rollbackCount())
	}
	if world.outsideTx != 0 {
		t.Errorf("%d repository writes ran outside the unit of work", world.outsideTx)
	}
}

func TestRegisterBackendRefusalNeverOpensAUnit(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	_, err := cat.RegisterBackend(t.Context(), "OpenAI", "https://api.example.com", "", "")
	if !errors.Is(err, catalog.ErrInvalidBackendTarget) {
		t.Fatalf("RegisterBackend error = %v, want ErrInvalidBackendTarget", err)
	}
	// The domain validates before the use case opens a transaction: there
	// is nothing to roll back and nothing to commit.
	if len(world.order) != 0 {
		t.Errorf("a refused register still moved the world: %v", world.order)
	}
}

func TestBackendReadMissIsThePortSentinel(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	_, err := cat.Backend(t.Context(), catalog.BackendID("absent"))
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("Backend error = %v, want persistence.ErrNotFound", err)
	}
}

func TestDisableEnableRoundTripAndItsNoOpShortCircuit(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	backend := newRegisteredBackend(t, cat)
	ctx := t.Context()

	if err := cat.DisableBackend(ctx, backend.ID); err != nil {
		t.Fatalf("DisableBackend returned error: %v", err)
	}
	if world.backendSwaps != 1 {
		t.Fatalf("backend swaps = %d, want exactly the one that moved the row", world.backendSwaps)
	}
	// Disabling a disabled backend is a no-op: the read happens, the swap
	// does not.
	if err := cat.DisableBackend(ctx, backend.ID); err != nil {
		t.Errorf("second DisableBackend returned error: %v — a no-op is not a failure", err)
	}
	if world.backendSwaps != 1 {
		t.Errorf("the no-op fired a swap: backend swaps = %d", world.backendSwaps)
	}
	if err := cat.EnableBackend(ctx, backend.ID); err != nil {
		t.Fatalf("EnableBackend returned error: %v", err)
	}
	stored, err := cat.Backend(ctx, backend.ID)
	if err != nil {
		t.Fatalf("Backend returned error: %v", err)
	}
	if stored.State != catalog.BackendActive {
		t.Errorf("State = %q, want active after the round trip", stored.State)
	}
}

func TestBackendTransitionRetriesAfterALostSwap(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	backend := newRegisteredBackend(t, cat)
	world.contendedBackendSwaps = 1 // the first swap loses, the retry lands
	if err := cat.DisableBackend(t.Context(), backend.ID); err != nil {
		t.Fatalf("DisableBackend returned error: %v — one lost swap is a retry, not a failure", err)
	}
	if world.backendSwaps != 2 {
		t.Errorf("backend swaps = %d, want the lost one plus the landing one", world.backendSwaps)
	}
	stored, err := cat.Backend(t.Context(), backend.ID)
	if err != nil {
		t.Fatalf("Backend returned error: %v", err)
	}
	if stored.State != catalog.BackendDisabled {
		t.Errorf("State = %q, want disabled after the retry", stored.State)
	}
}

func TestRetargetBackendMovesTheTargetAxisOnly(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	backend := newRegisteredBackend(t, cat)
	ctx := t.Context()
	if err := cat.DisableBackend(ctx, backend.ID); err != nil {
		t.Fatalf("DisableBackend returned error: %v", err)
	}
	if err := cat.RetargetBackend(ctx, backend.ID, "https://new.example.com", "creds/new", "egress/new"); err != nil {
		t.Fatalf("RetargetBackend returned error: %v — a drained backend is still re-pointable", err)
	}
	stored, err := cat.Backend(ctx, backend.ID)
	if err != nil {
		t.Fatalf("Backend returned error: %v", err)
	}
	if stored.Endpoint != "https://new.example.com" || stored.CredentialsRef != "creds/new" || stored.EgressPolicyRef != "egress/new" {
		t.Errorf("target = %q/%q/%q, want the new values", stored.Endpoint, stored.CredentialsRef, stored.EgressPolicyRef)
	}
	if stored.AdapterType != backend.AdapterType {
		t.Errorf("AdapterType = %q, want it untouched by a retarget", stored.AdapterType)
	}
	if stored.State != catalog.BackendDisabled {
		t.Errorf("State = %q, want the state axis untouched", stored.State)
	}
}

// ---------------------------------------------------------------------------
// aliases
// ---------------------------------------------------------------------------

func TestDefineAliasWritesTheWholeAggregate(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	backend := newRegisteredBackend(t, cat)
	alias, err := cat.DefineAlias(t.Context(), "gpt-5", 4096, 1024, []catalog.Candidate{
		{BackendID: backend.ID, ProviderModel: "gpt-5"},
		{BackendID: backend.ID, ProviderModel: "gpt-5-mini", ParameterOverrides: []byte(`{"temperature":0.2}`)},
	})
	if err != nil {
		t.Fatalf("DefineAlias returned error: %v", err)
	}
	stored, err := cat.Alias(t.Context(), alias.ID)
	if err != nil {
		t.Fatalf("Alias returned error: %v", err)
	}
	if stored.State != catalog.AliasActive {
		t.Errorf("State = %q, want the birth state", stored.State)
	}
	if len(stored.Candidates) != 2 {
		t.Fatalf("stored candidates = %d, want the whole list", len(stored.Candidates))
	}
	if stored.Candidates[0].Position != 1 || stored.Candidates[1].Position != 2 {
		t.Errorf("positions = %d/%d, want 1/2 — the fallback order is the aggregate's", stored.Candidates[0].Position, stored.Candidates[1].Position)
	}
	if world.outsideTx != 0 {
		t.Errorf("%d repository writes ran outside the unit of work", world.outsideTx)
	}
	if world.rollbackCount() != 0 {
		t.Errorf("a successful define rolled back %d times", world.rollbackCount())
	}
}

func TestDefineAliasWithAnUnknownBackendIsRefusedAndLeavesNothing(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	_, err := cat.DefineAlias(t.Context(), "gpt-5", 4096, 1024, []catalog.Candidate{
		{BackendID: catalog.BackendID("no-such-backend"), ProviderModel: "gpt-5"},
	})
	if !errors.Is(err, catalog.ErrInvalidCandidates) {
		t.Fatalf("DefineAlias error = %v, want ErrInvalidCandidates", err)
	}
	if world.rollbackCount() != 1 {
		t.Fatalf("rollbacks = %d, want the refused define's one", world.rollbackCount())
	}
	if len(world.aliases) != 0 {
		t.Errorf("the refused define left %d aliases behind", len(world.aliases))
	}
}

func TestDefineAliasRefusesATakenNameAndLeavesNothing(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	backend := newRegisteredBackend(t, cat)
	world.nameTaken = true // the (total) name index fires underneath
	_, err := cat.DefineAlias(t.Context(), "gpt-5", 4096, 1024, []catalog.Candidate{
		{BackendID: backend.ID, ProviderModel: "gpt-5"},
	})
	if !errors.Is(err, catalog.ErrAliasNameTaken) {
		t.Fatalf("DefineAlias error = %v, want ErrAliasNameTaken", err)
	}
	if len(world.aliases) != 0 {
		t.Errorf("a name collision inserted %d rows", len(world.aliases))
	}
}

func TestDefineAliasRollsBackWhenTheCandidateWriteFails(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	backend := newRegisteredBackend(t, cat)
	world.aliasInsertFailure = errCandidateInsert // alias row lands, candidates blow up
	_, err := cat.DefineAlias(t.Context(), "gpt-5", 4096, 1024, []catalog.Candidate{
		{BackendID: backend.ID, ProviderModel: "gpt-5"},
	})
	if !errors.Is(err, errCandidateInsert) {
		t.Fatalf("DefineAlias error = %v, want the candidate failure", err)
	}
	// The alias row that did land must not survive its own failed unit of
	// work: an alias without candidates is not a state the catalog stores.
	if len(world.aliases) != 0 {
		t.Errorf("a half-written aggregate survived: %d aliases on file", len(world.aliases))
	}
}

func TestAliasReadMissIsThePortSentinel(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	_, err := cat.Alias(t.Context(), catalog.AliasID("absent"))
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("Alias error = %v, want persistence.ErrNotFound", err)
	}
}

func TestSetCandidatesReplacesTheListAndRetriesContention(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	backend := newRegisteredBackend(t, cat)
	alias := newDefinedAlias(t, cat, backend.ID)
	world.contendedAliasSwaps = 1 // the first guard loses, the retry lands
	ctx := t.Context()

	if err := cat.SetCandidates(ctx, alias.ID, []catalog.Candidate{
		{BackendID: backend.ID, ProviderModel: "gpt-5-mini"},
	}); err != nil {
		t.Fatalf("SetCandidates returned error: %v — one lost guard is a retry, not a failure", err)
	}
	stored, err := cat.Alias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("Alias returned error: %v", err)
	}
	if len(stored.Candidates) != 1 || stored.Candidates[0].ProviderModel != "gpt-5-mini" {
		t.Errorf("candidates = %+v, want the replacement list", stored.Candidates)
	}
	if stored.Candidates[0].Position != 1 {
		t.Errorf("position = %d, want the replacement renumbered from 1", stored.Candidates[0].Position)
	}
}

func TestSetCandidatesOnARetiredAliasIsRefusedWithoutASwap(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	backend := newRegisteredBackend(t, cat)
	alias := newDefinedAlias(t, cat, backend.ID)
	if err := cat.RetireAlias(t.Context(), alias.ID); err != nil {
		t.Fatalf("RetireAlias returned error: %v", err)
	}
	swapsBefore := world.aliasSwaps
	err := cat.SetCandidates(t.Context(), alias.ID, []catalog.Candidate{
		{BackendID: backend.ID, ProviderModel: "gpt-5-mini"},
	})
	if !errors.Is(err, catalog.ErrAliasRetired) {
		t.Fatalf("SetCandidates error = %v, want ErrAliasRetired", err)
	}
	if world.aliasSwaps != swapsBefore {
		t.Errorf("a refused edit fired %d swaps", world.aliasSwaps-swapsBefore)
	}
	stored, err := cat.Alias(t.Context(), alias.ID)
	if err != nil {
		t.Fatalf("Alias returned error: %v", err)
	}
	if len(stored.Candidates) != 1 || stored.Candidates[0].ProviderModel != "gpt-5" {
		t.Errorf("candidates moved under the freeze: %+v", stored.Candidates)
	}
}

func TestRetireAliasIsOneWayAndItsSecondCallIsANoOp(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	backend := newRegisteredBackend(t, cat)
	alias := newDefinedAlias(t, cat, backend.ID)
	ctx := t.Context()

	if err := cat.RetireAlias(ctx, alias.ID); err != nil {
		t.Fatalf("RetireAlias returned error: %v", err)
	}
	if world.retireCalls != 1 {
		t.Fatalf("retire swaps = %d, want exactly the one that moved the row", world.retireCalls)
	}
	stored, err := cat.Alias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("Alias returned error: %v", err)
	}
	if stored.State != catalog.AliasRetired || stored.RetiredAt == nil {
		t.Fatalf("state = %q, retired_at = %v, want a stamped retirement", stored.State, stored.RetiredAt)
	}
	// A retry after a success cannot fail: terminal states absorb
	// repetition, and no second swap fires.
	if err := cat.RetireAlias(ctx, alias.ID); err != nil {
		t.Errorf("second RetireAlias returned error: %v", err)
	}
	if world.retireCalls != 1 {
		t.Errorf("the no-op fired a swap: retire swaps = %d", world.retireCalls)
	}
}

func TestUpdateAliasBoundsAndItsRefusalOnRetired(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	backend := newRegisteredBackend(t, cat)
	alias := newDefinedAlias(t, cat, backend.ID)
	ctx := t.Context()

	if err := cat.UpdateAliasBounds(ctx, alias.ID, 8192, 2048); err != nil {
		t.Fatalf("UpdateAliasBounds returned error: %v", err)
	}
	stored, err := cat.Alias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("Alias returned error: %v", err)
	}
	if stored.MaxOutputTokens != 8192 || stored.ReservationCap != 2048 {
		t.Fatalf("bounds = %d/%d, want 8192/2048", stored.MaxOutputTokens, stored.ReservationCap)
	}
	if err := cat.RetireAlias(ctx, alias.ID); err != nil {
		t.Fatalf("RetireAlias returned error: %v", err)
	}
	if err := cat.UpdateAliasBounds(ctx, alias.ID, 16384, 4096); !errors.Is(err, catalog.ErrAliasRetired) {
		t.Errorf("UpdateAliasBounds on retired error = %v, want ErrAliasRetired", err)
	}
	stored, err = cat.Alias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("Alias returned error: %v", err)
	}
	if stored.MaxOutputTokens != 8192 {
		t.Errorf("bounds moved under the freeze: %d", stored.MaxOutputTokens)
	}
}

// ---------------------------------------------------------------------------
// alias group versions
// ---------------------------------------------------------------------------

func TestOpenGroupVersionSuppliesTheNumbers(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	members := []catalog.AliasID{"alias-1", "alias-2"}

	first, err := cat.OpenGroupVersion(t.Context(), "frontier", members)
	if err != nil {
		t.Fatalf("OpenGroupVersion returned error: %v", err)
	}
	// The caller never names a version: the first snapshot of a group is
	// 1, whatever the caller believes.
	if first.Version != 1 {
		t.Fatalf("first Version = %d, want 1", first.Version)
	}
	second, err := cat.OpenGroupVersion(t.Context(), "frontier", members)
	if err != nil {
		t.Fatalf("second OpenGroupVersion returned error: %v", err)
	}
	if second.Version != 2 {
		t.Fatalf("second Version = %d, want the group's next number", second.Version)
	}
	if got := len(world.versions["frontier"]); got != 2 {
		t.Fatalf("frontier snapshots = %d, want two", got)
	}
}

func TestOpenGroupVersionLosesTheRaceAndWinsTheRetry(t *testing.T) {
	world := newCatalogWorld()
	world.versionRace = 1 // the first insert loses; a competitor took the number
	cat := newCatalog(world)
	members := []catalog.AliasID{"alias-1"}

	opened, err := cat.OpenGroupVersion(t.Context(), "frontier", members)
	if err != nil {
		t.Fatalf("OpenGroupVersion returned error: %v — losing one version race is a retry, not a failure", err)
	}
	// The retry re-read highest — now 1, the competitor's committed
	// number — and opened the next one: no gap, no caller-supplied version.
	if opened.Version != 2 {
		t.Fatalf("opened Version = %d, want 2 after the lost race", opened.Version)
	}
	if world.rollbackCount() != 1 || world.commitCount() != 1 {
		t.Errorf("units = %d rollbacks / %d commits, want the lost attempt and the landing one", world.rollbackCount(), world.commitCount())
	}
	snapshot, err := cat.GroupVersion(t.Context(), "frontier", 2)
	if err != nil {
		t.Fatalf("the opened snapshot is unreadable: %v", err)
	}
	if snapshot.ID != opened.ID || len(snapshot.Members) != 1 {
		t.Errorf("snapshot = %s with %d members, want the opened one with its member set", snapshot.ID, len(snapshot.Members))
	}
}

func TestOpenGroupVersionRefusesAnIllFormedSnapshotOnce(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	_, err := cat.OpenGroupVersion(t.Context(), "frontier", nil)
	if !errors.Is(err, catalog.ErrInvalidGroupMembers) {
		t.Fatalf("OpenGroupVersion error = %v, want ErrInvalidGroupMembers", err)
	}
	// A domain refusal is terminal: the retry loop must not spin on it —
	// one unit of work, refused before any insert, nothing committed.
	if world.versionCreates != 0 {
		t.Errorf("version creates = %d, want none", world.versionCreates)
	}
	if world.rollbackCount() != 1 || world.commitCount() != 0 {
		t.Errorf("units = %d rollbacks / %d commits, want the one refused attempt", world.rollbackCount(), world.commitCount())
	}
}

func TestOpenWildcardVersionIsOnceEver(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)

	first, err := cat.OpenWildcardVersion(t.Context())
	if err != nil {
		t.Fatalf("OpenWildcardVersion returned error: %v", err)
	}
	if first.GroupName != catalog.GroupWildcardName || first.Version != 1 || len(first.Members) != 0 {
		t.Fatalf("wildcard = %s@%d with %d members, want %q@1 with none", first.GroupName, first.Version, len(first.Members), catalog.GroupWildcardName)
	}
	_, err = cat.OpenWildcardVersion(t.Context())
	if !errors.Is(err, catalog.ErrGroupVersionExists) {
		t.Fatalf("second OpenWildcardVersion error = %v, want ErrGroupVersionExists", err)
	}
	if !first.Contains("any-alias") {
		t.Errorf("the wildcard refused a member of everything")
	}
}

func TestGroupVersionReadMissIsThePortSentinel(t *testing.T) {
	world := newCatalogWorld()
	cat := newCatalog(world)
	_, err := cat.GroupVersion(t.Context(), "frontier", 7)
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("GroupVersion error = %v, want persistence.ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// construction
// ---------------------------------------------------------------------------

func TestNewCatalogPanicsOnAMissingPort(t *testing.T) {
	store := fakeCatalogStore{world: newCatalogWorld()}
	backends := fakeBackends{world: newCatalogWorld()}
	aliases := fakeAliases{world: newCatalogWorld()}
	versions := fakeVersions{world: newCatalogWorld()}
	for name, wire := range map[string]func(){
		"no store":    func() { NewCatalog(nil, backends, aliases, versions) },
		"no backends": func() { NewCatalog(store, nil, aliases, versions) },
		"no aliases":  func() { NewCatalog(store, backends, nil, versions) },
		"no versions": func() { NewCatalog(store, backends, aliases, nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewCatalog with %s did not panic", name)
				}
			}()
			wire()
		}()
	}
}
