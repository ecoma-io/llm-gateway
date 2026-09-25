//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// ByName against the real `dataplane` database. The name lookup is how
// requests resolve aliases, and what only PostgreSQL can answer here is the
// index's side of the contract: that the total unique index on name serves
// the lookup, that the aggregate comes back whole and in fallback order —
// the same shape ByID returns, since a replay must resolve today exactly
// what the original resolved — and that a retired alias is returned with its
// frozen state rather than reported missing. The fixtures ride the catalog
// suite's helpers; every name is minted fresh because names are never
// reissued.

// mustByName is ByName with a miss failing the test.
func mustByName(t *testing.T, aliases persistence.ModelAliases, name string) *catalog.ModelAlias {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	alias, err := aliases.ByName(ctx, name)
	if err != nil {
		t.Fatalf("Aliases.ByName(%s) error = %v", name, err)
	}
	return alias
}

func TestIntegrationAliasByNameReturnsTheWholeAggregateInOrder(t *testing.T) {
	store, backends, aliases, _ := integrationCatalogRepos(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	backend := catalogBackend(t, ctx, backends)
	alias := newCatalogAlias(t, backend.ID, true)
	createAlias(t, ctx, store, aliases, alias)

	got := mustByName(t, aliases, alias.Name)
	if got.ID != alias.ID || got.Name != alias.Name || got.State != catalog.AliasActive {
		t.Errorf("alias = %s/%s/%s, want %s/%s/active", got.ID, got.Name, got.State, alias.ID, alias.Name)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("candidates = %d, want the whole list", len(got.Candidates))
	}
	if got.Candidates[0].ProviderModel != "primary-model" || got.Candidates[0].Position != 1 {
		t.Errorf("first candidate = %s@%d, want primary-model@1 — fallback order is the aggregate's", got.Candidates[0].ProviderModel, got.Candidates[0].Position)
	}
	if got.Candidates[1].Position != 2 {
		t.Errorf("second position = %d, want 2", got.Candidates[1].Position)
	}
}

func TestIntegrationAliasByNameReturnsARetiredAliasWithItsState(t *testing.T) {
	// Retirement is the caller's unknown_alias decision, not the read's: a
	// replay of an original admitted against this alias must still resolve
	// the name to the aggregate it named — the frozen candidate list with
	// the retired stamp beside it. A miss here would turn a replayable
	// original into an unknown-alias refusal.
	store, backends, aliases, _ := integrationCatalogRepos(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	backend := catalogBackend(t, ctx, backends)
	alias := newCatalogAlias(t, backend.ID, false)
	createAlias(t, ctx, store, aliases, alias)
	retiredAt := time.Now()
	if applied, err := aliases.Retire(ctx, alias.ID, catalog.AliasActive, retiredAt); err != nil || !applied {
		t.Fatalf("Retire = %v, %v; want the retirement to land", applied, err)
	}

	got := mustByName(t, aliases, alias.Name)
	if got.State != catalog.AliasRetired {
		t.Errorf("state = %s, want retired — the read returns the alias, the caller decides unknown_alias", got.State)
	}
	if got.RetiredAt == nil {
		t.Error("retired_at = nil, want the retirement's stamp")
	}
	if len(got.Candidates) != 1 || got.Candidates[0].ProviderModel != "primary-model" {
		t.Errorf("candidates = %v, want the list frozen exactly as retirement left it", got.Candidates)
	}
}

func TestIntegrationAliasByNameMissIsTheNotFoundSentinel(t *testing.T) {
	_, _, aliases, _ := integrationCatalogRepos(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := aliases.ByName(ctx, freshAliasName(t)); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("err = %v, want it to wrap ErrNotFound — the same miss ByID reports", err)
	}
}

func TestIntegrationAliasByNameNeverCrossesCandidates(t *testing.T) {
	// Two aliases over one backend must each resolve their own list: the
	// candidates query is the aggregate's, keyed by the looked-up alias's
	// id, never by the name that found it.
	store, backends, aliases, _ := integrationCatalogRepos(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	backend := catalogBackend(t, ctx, backends)
	first := newCatalogAlias(t, backend.ID, false)
	second := newCatalogAlias(t, backend.ID, false)
	createAlias(t, ctx, store, aliases, first)
	createAlias(t, ctx, store, aliases, second)

	gotFirst := mustByName(t, aliases, first.Name)
	gotSecond := mustByName(t, aliases, second.Name)
	if gotFirst.ID != first.ID || gotSecond.ID != second.ID {
		t.Fatalf("lookups resolved %s/%s, want %s/%s", gotFirst.ID, gotSecond.ID, first.ID, second.ID)
	}
	for _, c := range gotFirst.Candidates {
		if c.ID == gotSecond.Candidates[0].ID {
			t.Errorf("first alias's list carries a candidate belonging to the second: %s", c.ID)
		}
	}
	if len(gotFirst.Candidates) != 1 || len(gotSecond.Candidates) != 1 {
		t.Fatalf("candidate counts = %d/%d, want one list per alias", len(gotFirst.Candidates), len(gotSecond.Candidates))
	}
}
