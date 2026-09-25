//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
	migrate "github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// The catalog repositories against the real `dataplane` database. The
// fake-driver tier pins which handle a statement runs on and how a driver
// condition becomes the port's vocabulary; only PostgreSQL can answer the
// rest — that the schema the migration claims (the grammars, the total name
// uniqueness, the wildcard singleton, the RESTRICT foreign keys) actually
// refuses what it must refuse, and that the compare-and-swaps converge under
// concurrent callers. What is deliberately NOT re-proven here is the use
// cases' retry loops themselves: they live in the application layer, whose
// tests drive them against fakes, because an outbound adapter importing the
// application to reach them would invert the arrow the arch rules pin — the
// loops this file runs below are spelled locally, and marked as mirrors.
//
// Like every integration tier in this repository, the server is required
// explicitly rather than started from go test. deploy/postgres/compose.yaml
// provides the one pinned disposable cluster; the database and its migration
// history are ensured from the one admin DSN, the way the port suite ensures
// the database itself — a database this suite prepared and one `bash
// deploy/postgres/verify.sh` prepared are indistinguishable afterwards.
//
// Because alias names and group names are unique forever, every test mints
// its names from fresh identifiers: a fixed name would collide with this
// suite's own history on the second run against a populated database. The
// one row this suite seeds idempotently is the wildcard's singleton, which
// the schema admits exactly once per database lifetime.

// integrationCatalog ensures the dataplane lane's migration history on the
// plane database and hands back the pool and store the repositories run on.
func integrationCatalog(t *testing.T) (*sql.DB, persistence.Store) {
	t.Helper()
	db, store := integrationPool(t)
	ensureCatalogSchema(t, db)
	return db, store
}

// migrationsDir locates the Data Plane's migration lane relative to this
// file, so the suite applies exactly the files the repository ships — not a
// copy, and not an embedded snapshot that could drift from
// migrations/dataplane/.
func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: cannot locate this test file")
	}
	// postgres/ -> outbound -> adapters -> internal -> dataplane -> apps ->
	// the repository root the lane lives under.
	return filepath.Join(filepath.Dir(thisFile),
		"..", "..", "..", "..", "..", "..", "migrations", "dataplane")
}

// ensureCatalogSchema applies the dataplane lane's migrations through the
// same golang-migrate library the pinned runner image runs. The catalog
// tests are the integration tier's first consumers of this lane's schema —
// the grammars, the uniqueness set and the foreign keys are their subjects —
// so the schema is a precondition the suite ensures rather than one the
// caller is told to arrange. The driver is deliberately never Close()d —
// closing it would close the pool the tests still use; t.Cleanup owns that.
func ensureCatalogSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		t.Fatalf("migrate driver over the dataplane database: %v", err)
	}
	source, err := iofs.New(os.DirFS(migrationsDir(t)), ".")
	if err != nil {
		t.Fatalf("read the dataplane lane's migrations: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "dataplane", driver)
	if err != nil {
		t.Fatalf("wire the migration runner: %v", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("apply the dataplane lane's migrations: %v", err)
	}
}

// integrationCatalogRepos builds the three repositories over one store, as
// the ports the use cases see them — the integration tier tests the port
// contract, not the concrete types behind it.
func integrationCatalogRepos(t *testing.T) (persistence.Store, persistence.Backends, persistence.ModelAliases, persistence.AliasGroupVersions) {
	t.Helper()
	_, store := integrationCatalog(t)
	return store, NewBackends(store), NewModelAliases(store), NewAliasGroupVersions(store)
}

// catalogPool opens a pool wide enough for n concurrent units of work; the
// shared integrationPool is sized for one caller's short probes. The
// repositories returned ride the same store, so a race test's reads and
// writes all travel one pool.
func catalogPool(t *testing.T, n int) (persistence.Store, persistence.Backends, persistence.ModelAliases, persistence.AliasGroupVersions) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	db, err := Open(ctx, Options{
		DSN:             integrationPlaneDSN(t),
		MaxOpenConns:    n + 2,
		MaxIdleConns:    n,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Open() with %d connections: %v", n+2, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ensureCatalogSchema(t, db)
	store := New(db)
	return store, NewBackends(store), NewModelAliases(store), NewAliasGroupVersions(store)
}

// catalogBackend mints and creates one backend whose identity collides with
// no earlier run's rows.
func catalogBackend(t *testing.T, ctx context.Context, backends persistence.Backends) *catalog.Backend {
	t.Helper()
	id, err := catalog.NewBackendID()
	if err != nil {
		t.Fatalf("NewBackendID: %v", err)
	}
	backend, err := catalog.NewBackend(id, "openai-compatible", "https://api.example.com/v1", "", "", time.Now())
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if err := backends.Create(ctx, backend); err != nil {
		t.Fatalf("Backends.Create: %v", err)
	}
	return backend
}

// freshAliasName mints an alias name unique across runs: names are never
// reissued, so a fixed name would collide with this suite's own history on
// the second run. A dash-stripped UUIDv7 is inside the alias grammar.
func freshAliasName(t *testing.T) string {
	t.Helper()
	id, err := catalog.NewAliasID()
	if err != nil {
		t.Fatalf("NewAliasID: %v", err)
	}
	return strings.ReplaceAll(string(id), "-", "")
}

// mintUUID mints one bare UUID string for raw-SQL probes. The probes insert
// rows the schema is expected to refuse (or admit once, with a fresh id), and
// the id columns are uuid — no suffixing a real id, which stopped being a
// uuid the moment the suffix landed.
func mintUUID(t *testing.T) string {
	t.Helper()
	id, err := catalog.NewAliasID()
	if err != nil {
		t.Fatalf("NewAliasID: %v", err)
	}
	return string(id)
}

// withCandidateID fills in the candidate identity the domain mints in its
// own SetCandidates path: the port takes complete candidates, so a test
// calling the repository directly supplies them.
func withCandidateID(t *testing.T, candidate catalog.Candidate) catalog.Candidate {
	t.Helper()
	candidate.ID = catalog.CandidateID(mintUUID(t))
	return candidate
}

// newCatalogAlias builds an unsaved alias aggregate with a fresh name and
// one candidate; with overrides, a second candidate carrying parameter
// overrides, so the round trip has an object to carry through jsonb.
func newCatalogAlias(t *testing.T, backendID catalog.BackendID, overrides bool) *catalog.ModelAlias {
	t.Helper()
	id, err := catalog.NewAliasID()
	if err != nil {
		t.Fatalf("NewAliasID: %v", err)
	}
	candidates := []catalog.Candidate{
		{BackendID: backendID, ProviderModel: "primary-model"},
	}
	if overrides {
		candidates = append(candidates, catalog.Candidate{
			BackendID:          backendID,
			ProviderModel:      "fallback-model",
			ParameterOverrides: []byte(`{"temperature":0.2}`),
		})
	}
	alias, err := catalog.NewAlias(id, freshAliasName(t), 4096, 1024, candidates, time.Now())
	if err != nil {
		t.Fatalf("NewAlias: %v", err)
	}
	return alias
}

// createAlias writes the aggregate the way the use case does — alias row and
// candidates inside one unit of work.
func createAlias(t *testing.T, ctx context.Context, store persistence.Store, aliases persistence.ModelAliases, alias *catalog.ModelAlias) {
	t.Helper()
	if err := store.WithinTx(ctx, func(txCtx context.Context) error {
		return aliases.Create(txCtx, alias)
	}); err != nil {
		t.Fatalf("create alias %s: %v", alias.ID, err)
	}
}

// ---------------------------------------------------------------------------
// round trips
// ---------------------------------------------------------------------------

func TestIntegrationBackendRoundTripCarriesOptionalRefs(t *testing.T) {
	_, backends, _, _ := integrationCatalogRepos(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// A target move stamps the references on: the round trip must carry
	// both out of their columns.
	withRefs := catalogBackend(t, ctx, backends)
	if err := backends.UpdateTarget(ctx, withRefs.ID, withRefs.Endpoint, "creds/main", "egress/main", time.Now()); err != nil {
		t.Fatalf("UpdateTarget: %v", err)
	}
	got, err := backends.ByID(ctx, withRefs.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got.CredentialsRef != "creds/main" || got.EgressPolicyRef != "egress/main" {
		t.Errorf("refs = %q/%q, want the round trip to carry both", got.CredentialsRef, got.EgressPolicyRef)
	}

	// Absence is data: a backend created without references reads back
	// without them, not with empty strings the column never stored.
	bare := catalogBackend(t, ctx, backends)
	got, err = backends.ByID(ctx, bare.ID)
	if err != nil {
		t.Fatalf("ByID on the bare backend: %v", err)
	}
	if got.CredentialsRef != "" || got.EgressPolicyRef != "" {
		t.Errorf("refs = %q/%q, want absence", got.CredentialsRef, got.EgressPolicyRef)
	}
}

func TestIntegrationAliasAggregateRoundTripKeepsOrderAndOverrides(t *testing.T) {
	store, backends, aliases, _ := integrationCatalogRepos(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	backend := catalogBackend(t, ctx, backends)
	alias := newCatalogAlias(t, backend.ID, true)
	createAlias(t, ctx, store, aliases, alias)

	got, err := aliases.ByID(ctx, alias.ID)
	if err != nil {
		t.Fatalf("Aliases.ByID: %v", err)
	}
	if got.Name != alias.Name || got.State != catalog.AliasActive {
		t.Errorf("alias = %s/%s, want %s/active", got.Name, got.State, alias.Name)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("candidates = %d, want the whole list", len(got.Candidates))
	}
	// Fallback order is the aggregate's: the read returns the list in
	// position order, whatever order the rows landed in.
	if got.Candidates[0].ProviderModel != "primary-model" || got.Candidates[0].Position != 1 {
		t.Errorf("first candidate = %s@%d, want primary-model@1", got.Candidates[0].ProviderModel, got.Candidates[0].Position)
	}
	if got.Candidates[1].Position != 2 {
		t.Errorf("second position = %d, want 2", got.Candidates[1].Position)
	}
	// jsonb does not promise the text back verbatim — it normalizes
	// whitespace and key order — so the round trip is compared as the value
	// it carries, not the bytes.
	var gotOverrides map[string]any
	if err := json.Unmarshal(got.Candidates[1].ParameterOverrides, &gotOverrides); err != nil {
		t.Fatalf("overrides %s do not decode: %v", got.Candidates[1].ParameterOverrides, err)
	}
	if gotOverrides["temperature"] != 0.2 {
		t.Errorf("overrides = %s, want the object round-tripped through jsonb", got.Candidates[1].ParameterOverrides)
	}
	if got.Candidates[0].ParameterOverrides != nil {
		t.Errorf("absent overrides = %s, want nil", got.Candidates[0].ParameterOverrides)
	}
}

// ---------------------------------------------------------------------------
// the schema refuses what it must
// ---------------------------------------------------------------------------

func TestIntegrationSchemaRefusesWhatItMust(t *testing.T) {
	store, backends, aliases, versions := integrationCatalogRepos(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	backend := catalogBackend(t, ctx, backends)
	alias := newCatalogAlias(t, backend.ID, false)
	createAlias(t, ctx, store, aliases, alias)

	t.Run("alias name grammar", func(t *testing.T) {
		_, err := store.Querier(ctx).ExecContext(ctx,
			`INSERT INTO model_aliases (id, name, state, max_output_tokens, reservation_cap, created_at, updated_at)
			 VALUES ($1, 'bad name', 'active', 1, 1, now(), now())`, mintUUID(t))
		if err == nil || !strings.Contains(err.Error(), "model_aliases_name_grammar") {
			t.Errorf("error = %v, want the name-grammar check", err)
		}
	})

	t.Run("positive bounds", func(t *testing.T) {
		_, err := store.Querier(ctx).ExecContext(ctx,
			`INSERT INTO model_aliases (id, name, state, max_output_tokens, reservation_cap, created_at, updated_at)
			 VALUES ($1, 'zero-bounds', 'active', 0, 1, now(), now())`, mintUUID(t))
		if err == nil || !strings.Contains(err.Error(), "model_aliases_max_output_tokens_positive") {
			t.Errorf("error = %v, want the positive-bounds check", err)
		}
	})

	t.Run("retirement consistency", func(t *testing.T) {
		_, err := store.Querier(ctx).ExecContext(ctx,
			`INSERT INTO model_aliases (id, name, state, max_output_tokens, reservation_cap, created_at, updated_at, retired_at)
			 VALUES ($1, 'active-retired', 'active', 1, 1, now(), now(), now())`, mintUUID(t))
		if err == nil || !strings.Contains(err.Error(), "model_aliases_retirement_consistency") {
			t.Errorf("error = %v, want the retirement-consistency check", err)
		}
	})

	t.Run("candidate position is one-based", func(t *testing.T) {
		_, err := store.Querier(ctx).ExecContext(ctx,
			`INSERT INTO model_candidates (id, alias_id, position, backend_id, provider_model, created_at)
			 VALUES ($1, $2, 0, $3, 'm', now())`, mintUUID(t), alias.ID, backend.ID)
		if err == nil || !strings.Contains(err.Error(), "model_candidates_position_valid") {
			t.Errorf("error = %v, want the position check", err)
		}
	})

	t.Run("candidate target uniqueness", func(t *testing.T) {
		_, err := store.Querier(ctx).ExecContext(ctx,
			`INSERT INTO model_candidates (id, alias_id, position, backend_id, provider_model, created_at)
			 VALUES ($1, $2, 9, $3, 'primary-model', now())`, mintUUID(t), alias.ID, backend.ID)
		// Position 9 is free and the checks pass; the
		// (alias_id, backend_id, provider_model) index is what refuses.
		if state, ok := sqlStateOf(err); !ok || state != pgUniqueViolation {
			t.Errorf("error = %v, want the SQLSTATE 23505 of the target index", err)
		}
	})

	t.Run("backends with candidates on file are not deletable", func(t *testing.T) {
		_, err := store.Querier(ctx).ExecContext(ctx, `DELETE FROM backends WHERE id = $1`, backend.ID)
		if err == nil || !strings.Contains(err.Error(), "model_candidates_backend_id_fkey") {
			t.Errorf("error = %v, want the RESTRICT foreign key", err)
		}
	})

	t.Run("the wildcard is one row at version one", func(t *testing.T) {
		// Seed the singleton, but only if an earlier run of this suite (or
		// verify.sh) already did: the schema admits exactly one wildcard row
		// per database lifetime, so the seed is conditional and the refusals
		// below hold either way.
		_, err := versions.ByGroupAndVersion(ctx, catalog.GroupWildcardName, 1)
		switch {
		case errors.Is(err, persistence.ErrNotFound):
			wid, err := catalog.NewGroupVersionID()
			if err != nil {
				t.Fatalf("NewGroupVersionID: %v", err)
			}
			wildcard, err := catalog.NewWildcardGroupVersion(wid, time.Now())
			if err != nil {
				t.Fatalf("NewWildcardGroupVersion: %v", err)
			}
			if err := store.WithinTx(ctx, func(txCtx context.Context) error {
				return versions.Create(txCtx, wildcard)
			}); err != nil {
				t.Fatalf("seed the wildcard: %v", err)
			}
		case err != nil:
			t.Fatalf("looking up the wildcard: %v", err)
		}

		// A second wildcard row — the singleton index's subject — is
		// refused at whatever number the imposter picks.
		_, err = store.Querier(ctx).ExecContext(ctx,
			`INSERT INTO alias_group_versions (id, group_name, version, created_at)
			 VALUES ($1, '*', 1, now())`, mintUUID(t))
		if state, ok := sqlStateOf(err); !ok || state != pgUniqueViolation {
			t.Errorf("second wildcard row error = %v, want the singleton index's 23505", err)
		}
		// And no version but 1 is expressible for the reserved name at all.
		_, err = store.Querier(ctx).ExecContext(ctx,
			`INSERT INTO alias_group_versions (id, group_name, version, created_at)
			 VALUES ($1, '*', 2, now())`, mintUUID(t))
		if err == nil || !strings.Contains(err.Error(), "alias_group_versions_wildcard_version") {
			t.Errorf("wildcard version 2 error = %v, want the wildcard-version check", err)
		}
	})

	t.Run("an alias may exist without candidates at the SQL level", func(t *testing.T) {
		// The ≥1-candidate invariant is application-owned by design — a
		// CHECK cannot see across tables — and this probe pins that the
		// schema does not pretend otherwise: the bare row inserts, and the
		// domain's refusal (which the application tests carry) is the only
		// thing standing between this row and an alias that resolves to
		// nothing.
		name := strings.ReplaceAll(mintUUID(t), "-", "") + "bare"
		_, err := store.Querier(ctx).ExecContext(ctx,
			`INSERT INTO model_aliases (id, name, state, max_output_tokens, reservation_cap, created_at, updated_at)
			 VALUES ($1, $2, 'active', 1, 1, now(), now())`, mintUUID(t), name)
		if err != nil {
			t.Errorf("the schema gated an application-owned invariant: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// compare-and-swaps
// ---------------------------------------------------------------------------

func TestIntegrationRetireCompareAndSwapLandsOnce(t *testing.T) {
	store, backends, aliases, _ := integrationCatalogRepos(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	backend := catalogBackend(t, ctx, backends)
	alias := newCatalogAlias(t, backend.ID, false)
	createAlias(t, ctx, store, aliases, alias)

	stamp := time.Now()
	applied, err := aliases.Retire(ctx, alias.ID, catalog.AliasActive, stamp)
	if err != nil || !applied {
		t.Fatalf("Retire = %v, %v; want the move to land", applied, err)
	}
	applied, err = aliases.Retire(ctx, alias.ID, catalog.AliasActive, time.Now())
	if err != nil || applied {
		t.Errorf("second Retire = %v, %v; want a clean loss — the row no longer reads active", applied, err)
	}
	got, err := aliases.ByID(ctx, alias.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got.State != catalog.AliasRetired || got.RetiredAt == nil {
		t.Fatalf("state = %s, retired_at = %v, want a stamped retirement", got.State, got.RetiredAt)
	}

	// The freeze is the guard's, not just the domain's: a SetCandidates or
	// UpdateBounds arriving after retirement loses, and loses cleanly.
	applied, err = aliases.SetCandidates(ctx, alias.ID,
		[]catalog.Candidate{withCandidateID(t, catalog.Candidate{BackendID: backend.ID, ProviderModel: "new"})}, time.Now())
	if err != nil || applied {
		t.Errorf("SetCandidates on retired = %v, %v; want a clean loss", applied, err)
	}
	applied, err = aliases.UpdateBounds(ctx, alias.ID, catalog.AliasActive, 8192, 2048, time.Now())
	if err != nil || applied {
		t.Errorf("UpdateBounds on retired = %v, %v; want a clean loss", applied, err)
	}
	after, err := aliases.ByID(ctx, alias.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if len(after.Candidates) != 1 || after.MaxOutputTokens != 4096 {
		t.Errorf("the retired aggregate moved: %+v", after)
	}
}

func TestIntegrationSetCandidatesReplacesTheListInOneUnit(t *testing.T) {
	store, backends, aliases, _ := integrationCatalogRepos(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	backend := catalogBackend(t, ctx, backends)
	alias := newCatalogAlias(t, backend.ID, true) // two candidates on file
	createAlias(t, ctx, store, aliases, alias)
	oldIDs := []catalog.CandidateID{alias.Candidates[0].ID, alias.Candidates[1].ID}

	replacement := []catalog.Candidate{withCandidateID(t, catalog.Candidate{
		BackendID:     backend.ID,
		ProviderModel: "successor",
		Position:      1, // the domain assigns positions; the direct caller supplies them
	})}
	applied, err := aliases.SetCandidates(ctx, alias.ID, replacement, time.Now())
	if err != nil || !applied {
		t.Fatalf("SetCandidates = %v, %v; want the replacement to land", applied, err)
	}
	got, err := aliases.ByID(ctx, alias.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].ProviderModel != "successor" {
		t.Fatalf("candidates = %+v, want exactly the successor", got.Candidates)
	}
	for _, old := range oldIDs {
		if got.Candidates[0].ID == old {
			t.Errorf("the successor reused the outgoing list's id %s", old)
		}
	}

	// A failed replacement leaves the standing list untouched: the guard and
	// the replacement are one unit of work, and this unit is aborted after
	// the replacement ran.
	err = store.WithinTx(ctx, func(txCtx context.Context) error {
		if _, err := aliases.SetCandidates(txCtx, alias.ID, nil, time.Now()); err != nil {
			return err
		}
		return errors.New("abort the unit of work after the replacement")
	})
	if err == nil || !strings.Contains(err.Error(), "abort the unit of work") {
		t.Fatalf("the aborted unit reported: %v", err)
	}
	after, err := aliases.ByID(ctx, alias.ID)
	if err != nil {
		t.Fatalf("re-read after the abort: %v", err)
	}
	if len(after.Candidates) != 1 || after.Candidates[0].ProviderModel != "successor" {
		t.Errorf("an aborted replacement left %d candidates", len(after.Candidates))
	}
}

func TestIntegrationAliasNameUniquenessIsTotal(t *testing.T) {
	store, backends, aliases, _ := integrationCatalogRepos(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	backend := catalogBackend(t, ctx, backends)
	alias := newCatalogAlias(t, backend.ID, false)
	createAlias(t, ctx, store, aliases, alias)
	// Retire it: the name does not come back. This is the contrast with the
	// control lane's live-only email index, and the reason this index has
	// no WHERE clause.
	if applied, err := aliases.Retire(ctx, alias.ID, catalog.AliasActive, time.Now()); err != nil || !applied {
		t.Fatalf("Retire = %v, %v", applied, err)
	}

	challenger, err := catalog.NewAliasID()
	if err != nil {
		t.Fatalf("NewAliasID: %v", err)
	}
	imposter, err := catalog.NewAlias(challenger, alias.Name, 1, 1, []catalog.Candidate{{BackendID: backend.ID, ProviderModel: "m"}}, time.Now())
	if err != nil {
		t.Fatalf("NewAlias on the retired name: %v", err)
	}
	err = store.WithinTx(ctx, func(txCtx context.Context) error {
		return aliases.Create(txCtx, imposter)
	})
	if !errors.Is(err, catalog.ErrAliasNameTaken) {
		t.Fatalf("Create on a retired name error = %v, want ErrAliasNameTaken", err)
	}
}

func TestIntegrationGroupVersionsRoundTripAndMonotonicHighest(t *testing.T) {
	store, backends, aliases, versions := integrationCatalogRepos(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	backend := catalogBackend(t, ctx, backends)
	members := []*catalog.ModelAlias{
		newCatalogAlias(t, backend.ID, false),
		newCatalogAlias(t, backend.ID, false),
	}
	for _, a := range members {
		createAlias(t, ctx, store, aliases, a)
	}
	group := freshAliasName(t)[:16] + "-group"

	highest, err := versions.HighestVersion(ctx, group)
	if err != nil || highest != 0 {
		t.Fatalf("HighestVersion on a fresh group = %d, %v; want 0", highest, err)
	}

	vid, err := catalog.NewGroupVersionID()
	if err != nil {
		t.Fatalf("NewGroupVersionID: %v", err)
	}
	v1, err := catalog.NewGroupVersion(vid, group, 1, []catalog.AliasID{members[0].ID, members[1].ID}, time.Now())
	if err != nil {
		t.Fatalf("NewGroupVersion: %v", err)
	}
	if err := store.WithinTx(ctx, func(txCtx context.Context) error {
		return versions.Create(txCtx, v1)
	}); err != nil {
		t.Fatalf("Create v1: %v", err)
	}

	got, err := versions.ByGroupAndVersion(ctx, group, 1)
	if err != nil {
		t.Fatalf("ByGroupAndVersion: %v", err)
	}
	if got.ID != v1.ID || len(got.Members) != 2 {
		t.Fatalf("snapshot = %s with %d members, want %s with 2", got.ID, len(got.Members), v1.ID)
	}
	stranger, err := catalog.NewAliasID()
	if err != nil {
		t.Fatalf("NewAliasID: %v", err)
	}
	if !got.Contains(members[1].ID) || got.Contains(stranger) {
		t.Errorf("containment misread the stored snapshot")
	}

	highest, err = versions.HighestVersion(ctx, group)
	if err != nil || highest != 1 {
		t.Fatalf("HighestVersion = %d, %v; want 1", highest, err)
	}

	// The (group_name, version) index, through the port: re-creating the
	// pair is the sentinel, not a driver error.
	if err := store.WithinTx(ctx, func(txCtx context.Context) error {
		return versions.Create(txCtx, v1)
	}); !errors.Is(err, catalog.ErrGroupVersionExists) {
		t.Fatalf("re-created (group, 1) error = %v, want ErrGroupVersionExists", err)
	}
}

// ---------------------------------------------------------------------------
// concurrency — the races the compare-and-swaps exist for
// ---------------------------------------------------------------------------

// openVersionRetry is the application's OpenGroupVersion loop, spelled
// locally because the adapter's test cannot import the application to call
// it: read the group's highest version, open highest+1, and on the unique
// collision re-read in a fresh unit of work — the collision aborts the
// transaction that hit it, which is why each attempt is its own unit.
func openVersionRetry(ctx context.Context, store persistence.Store, versions persistence.AliasGroupVersions, group string, members []catalog.AliasID) (int, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var opened int
		err := store.WithinTx(ctx, func(txCtx context.Context) error {
			highest, err := versions.HighestVersion(txCtx, group)
			if err != nil {
				return err
			}
			id, err := catalog.NewGroupVersionID()
			if err != nil {
				return err
			}
			version, err := catalog.NewGroupVersion(id, group, highest+1, members, time.Now())
			if err != nil {
				return err
			}
			if err := versions.Create(txCtx, version); err != nil {
				return err
			}
			opened = version.Version
			return nil
		})
		if err == nil {
			return opened, nil
		}
		if !errors.Is(err, catalog.ErrGroupVersionExists) {
			return 0, err
		}
	}
	return 0, fmt.Errorf("open group version %s: contention did not settle in 8 attempts", group)
}

// transitionRetry is the application's backend transition loop, spelled
// locally for the same reason: read, apply the domain move, swap under the
// state the read saw, re-read on a lost swap.
func transitionRetry(ctx context.Context, store persistence.Store, backends persistence.Backends, id catalog.BackendID, to catalog.BackendState) error {
	for attempt := 0; attempt < 8; attempt++ {
		var done bool
		err := store.WithinTx(ctx, func(txCtx context.Context) error {
			backend, err := backends.ByID(txCtx, id)
			if err != nil {
				return err
			}
			if backend.State == to {
				done = true // the domain's no-op: the intent is already recorded
				return nil
			}
			from := backend.State
			if to == catalog.BackendDisabled {
				if err := backend.Disable(time.Now()); err != nil {
					return err
				}
			} else {
				if err := backend.Enable(time.Now()); err != nil {
					return err
				}
			}
			applied, err := backends.TransitionState(txCtx, id, from, to, time.Now())
			if err != nil {
				return err
			}
			done = applied
			return nil
		})
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return fmt.Errorf("transition backend %s: contention did not settle in 8 attempts", id)
}

func TestIntegrationDuplicateAliasNameRaceAdmitsExactlyOne(t *testing.T) {
	// The shared pool's four connections serve the eight racers: each holds
	// a connection only for the length of one short unit of work, so the
	// rest queue rather than deadlock.
	const racers = 8
	store, backends, aliases, _ := integrationCatalogRepos(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	backend := catalogBackend(t, ctx, backends)
	name := freshAliasName(t)

	results := make([]error, racers)
	winner := make([]catalog.AliasID, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			id, err := catalog.NewAliasID()
			if err != nil {
				results[slot] = err
				return
			}
			alias, err := catalog.NewAlias(id, name, 4096, 1024, []catalog.Candidate{{BackendID: backend.ID, ProviderModel: "m"}}, time.Now())
			if err != nil {
				results[slot] = err
				return
			}
			err = store.WithinTx(ctx, func(txCtx context.Context) error {
				return aliases.Create(txCtx, alias)
			})
			results[slot] = err
			if err == nil {
				winner[slot] = alias.ID
			}
		}(i)
	}
	wg.Wait()

	admitted := 0
	var winnerID catalog.AliasID
	for slot, err := range results {
		switch {
		case err == nil:
			admitted++
			winnerID = winner[slot]
		case errors.Is(err, catalog.ErrAliasNameTaken):
			// the expected loss
		default:
			t.Fatalf("racer %d failed with an unexpected error: %v", slot, err)
		}
	}
	if admitted != 1 {
		t.Fatalf("%d racers admitted the same name, want exactly one", admitted)
	}
	got, err := aliases.ByID(ctx, winnerID)
	if err != nil {
		t.Fatalf("the winner is unreadable: %v", err)
	}
	if got.Name != name {
		t.Errorf("winner name = %q, want %q", got.Name, name)
	}
}

func TestIntegrationGroupVersionRaceOpensDistinctMonotonicVersions(t *testing.T) {
	const racers = 6
	store, backends, aliases, versions := catalogPool(t, racers)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// The group needs real member aliases: the foreign keys demand them.
	backend := catalogBackend(t, ctx, backends)
	memberOne := newCatalogAlias(t, backend.ID, false)
	memberTwo := newCatalogAlias(t, backend.ID, false)
	for _, a := range []*catalog.ModelAlias{memberOne, memberTwo} {
		createAlias(t, ctx, store, aliases, a)
	}
	group := freshAliasName(t)[:16] + "-race"
	members := []catalog.AliasID{memberOne.ID, memberTwo.ID}

	opened := make([]int, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			opened[slot], errs[slot] = openVersionRetry(ctx, store, versions, group, members)
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for slot, err := range errs {
		if err != nil {
			t.Fatalf("racer %d failed: %v — the retry loop exists so this cannot", slot, err)
		}
		if seen[opened[slot]] {
			t.Errorf("two racers opened version %d — the unique index let a duplicate through", opened[slot])
		}
		seen[opened[slot]] = true
	}
	// Every number 1..racers was taken, none skipped, none doubled: the
	// losers re-read and opened the next number after whatever landed.
	for v := 1; v <= racers; v++ {
		if !seen[v] {
			t.Errorf("version %d was never opened; opened set = %v", v, opened)
		}
	}
	highest, err := versions.HighestVersion(ctx, group)
	if err != nil || highest != racers {
		t.Errorf("HighestVersion = %d, %v; want %d", highest, err, racers)
	}
}

func TestIntegrationBackendTransitionRaceConverges(t *testing.T) {
	const racers = 8
	store, backends, _, _ := catalogPool(t, racers)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	id, err := catalog.NewBackendID()
	if err != nil {
		t.Fatalf("NewBackendID: %v", err)
	}
	backend, err := catalog.NewBackend(id, "openai-compatible", "https://api.example.com", "", "", time.Now())
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if err := backends.Create(ctx, backend); err != nil {
		t.Fatalf("Create: %v", err)
	}

	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			// Half the field drains, half restores; the swaps interleave,
			// and every racer must settle — a lost swap is a re-read, not
			// an error.
			to := catalog.BackendDisabled
			if slot%2 == 1 {
				to = catalog.BackendActive
			}
			errs[slot] = transitionRetry(ctx, store, backends, backend.ID, to)
		}(i)
	}
	wg.Wait()

	for slot, err := range errs {
		if err != nil {
			t.Fatalf("racer %d failed: %v", slot, err)
		}
	}
	got, err := backends.ByID(ctx, backend.ID)
	if err != nil {
		t.Fatalf("ByID after the race: %v", err)
	}
	// The winner is whichever swap landed last; what must hold is that the
	// row is one of the two well-formed states and carries the machine's
	// stamp — never a value the race invented.
	if got.State != catalog.BackendActive && got.State != catalog.BackendDisabled {
		t.Errorf("state after the race = %q, want one of the machine's two states", got.State)
	}
}
