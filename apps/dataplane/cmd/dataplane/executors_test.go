package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/config"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The executor registry's laws, unit-tested where the composition root keeps
// them: which rows become executors and which are left out with a log line,
// what makes one snapshot's signature differ from another's, and what a
// refresh does on each of its three possible reads — changed, unchanged,
// failed. The store integration suite covers the read itself; these tests
// cover the decisions made over what the read returned.

// fakeBackendLists is a persistence.Backends whose List replays a script:
// one outcome per call, the last repeating forever. Every other method is a
// stub, because nothing on the refresh path calls them — the bodies are the
// honest report that nothing did.
type fakeBackendLists struct {
	mu     sync.Mutex
	script []listOutcome
	calls  int
}

type listOutcome struct {
	rows []catalog.Backend
	err  error
}

func (f *fakeBackendLists) List(ctx context.Context) ([]catalog.Backend, error) {
	f.mu.Lock()
	i := f.calls
	if i < len(f.script) {
		f.calls++
	}
	outcome := f.script[min(i, len(f.script)-1)]
	f.mu.Unlock()
	return outcome.rows, outcome.err
}

func (f *fakeBackendLists) Create(ctx context.Context, backend *catalog.Backend) error {
	return errors.New("fakeBackendLists: Create is not on the refresh path")
}

func (f *fakeBackendLists) ByID(ctx context.Context, id catalog.BackendID) (*catalog.Backend, error) {
	return nil, errors.New("fakeBackendLists: ByID is not on the refresh path")
}

func (f *fakeBackendLists) TransitionState(ctx context.Context, id catalog.BackendID, from, to catalog.BackendState, updatedAt time.Time) (bool, error) {
	return false, errors.New("fakeBackendLists: TransitionState is not on the refresh path")
}

func (f *fakeBackendLists) UpdateTarget(ctx context.Context, id catalog.BackendID, endpoint, credentialsRef, egressPolicyRef string, updatedAt time.Time) (bool, error) {
	return false, errors.New("fakeBackendLists: UpdateTarget is not on the refresh path")
}

var _ persistence.Backends = (*fakeBackendLists)(nil)

// row is one backend row as the catalog would return it: everything the
// snapshot's seven signature fields carry, with the timestamps defaulted so
// a test only names what it is varying.
func row(id, adapterType, endpoint, credentialsRef, egressPolicyRef string, state catalog.BackendState, updatedAt time.Time) catalog.Backend {
	return catalog.Backend{
		ID:              catalog.BackendID(id),
		AdapterType:     adapterType,
		Endpoint:        endpoint,
		CredentialsRef:  credentialsRef,
		EgressPolicyRef: egressPolicyRef,
		State:           state,
		UpdatedAt:       updatedAt,
	}
}

// TestBuildExecutorsLeavesOutWhatItCannotCall: one frozen executor per row
// this build can serve — a row of an unknown adapter type has no driver yet,
// and a row whose endpoint the adapter would refuse is left out with the
// skip-and-log, never guessed into callability and never a panic. The two
// endpoint rows here are schema-legal: the store's CHECK reads only the
// `https?://` prefix, so a hostless or userinfo endpoint reaches this build
// and must meet a log line, not a crash in the goroutine that builds the
// snapshot.
func TestBuildExecutorsLeavesOutWhatItCannotCall(t *testing.T) {
	stamp := time.Unix(0, 0).UTC()
	rows := []catalog.Backend{
		row("good", "openai-compatible", "https://provider.example/v1", "", "", catalog.BackendActive, stamp),
		row("unknown-adapter", "acme-v9", "https://provider.example/v1", "", "", catalog.BackendActive, stamp),
		row("hostless", "openai-compatible", "https:///v1", "", "", catalog.BackendActive, stamp),
		row("userinfo", "openai-compatible", "https://someone:example@provider.invalid/v1", "", "", catalog.BackendActive, stamp),
		row("no-such-policy", "openai-compatible", "https://provider.example/v1", "", "policy/nobody-defined", catalog.BackendActive, stamp),
	}
	entries := buildExecutors(rows, config.Config{})

	if _, ok := entries["good"]; !ok {
		t.Error("the build produced no executor for the servable row")
	}
	for _, absent := range []string{"unknown-adapter", "hostless", "userinfo", "no-such-policy"} {
		if _, ok := entries[catalog.BackendID(absent)]; ok {
			t.Errorf("the build produced an executor for %q, want it left out of the snapshot", absent)
		}
	}
}

// TestSnapshotSignatureMovesOnlyWhenARowDoes: the signature is what a
// refresh compares, so it must read the same rows the same way — order
// included — and move whenever any signature field moves, updated_at
// included, because an operator edit that produced identical text is still a
// new catalog.
func TestSnapshotSignatureMovesOnlyWhenARowDoes(t *testing.T) {
	stamp := time.Unix(0, 42).UTC()
	later := time.Unix(0, 43).UTC()
	base := []catalog.Backend{
		row("backend-a", "openai-compatible", "https://a.example/v1", "env:A_KEY", "", catalog.BackendActive, stamp),
		row("backend-b", "openai-compatible", "https://b.example/v1", "", "egress/b", catalog.BackendActive, stamp),
	}

	reversed := []catalog.Backend{base[1], base[0]}
	if snapshotSignature(base) != snapshotSignature(reversed) {
		t.Fatal("row order moved the signature — the comparison is over content, not position")
	}

	variations := map[string][]catalog.Backend{
		"a new row":            append(append([]catalog.Backend{}, base...), row("backend-c", "openai-compatible", "https://c.example/v1", "", "", catalog.BackendActive, stamp)),
		"a removed row":        base[:1],
		"a re-pointed target":  {base[0], row("backend-b", "openai-compatible", "https://b.example/v2", "", "egress/b", catalog.BackendActive, stamp)},
		"a changed credential": {base[0], row("backend-b", "openai-compatible", "https://b.example/v1", "env:B_KEY", "egress/b", catalog.BackendActive, stamp)},
		"a changed egress":     {base[0], row("backend-b", "openai-compatible", "https://b.example/v1", "", "egress/c", catalog.BackendActive, stamp)},
		"a changed state":      {base[0], row("backend-b", "openai-compatible", "https://b.example/v1", "", "egress/b", catalog.BackendDisabled, stamp)},
		"a re-stamped row":     {base[0], row("backend-b", "openai-compatible", "https://b.example/v1", "", "egress/b", catalog.BackendActive, later)},
	}
	for name, rows := range variations {
		if snapshotSignature(base) == snapshotSignature(rows) {
			t.Errorf("%s left the signature unchanged, want it moved", name)
		}
	}
}

// TestRefreshOnceKeepsTheLastGoodSnapshotOnAFailedRead: a read that fails
// changes nothing — the snapshot pointer survives untouched and the next
// comparison runs against the same signature, because an empty registry
// would turn one bad read into every answer being no_candidate.
func TestRefreshOnceKeepsTheLastGoodSnapshotOnAFailedRead(t *testing.T) {
	stamp := time.Unix(0, 0).UTC()
	rows := []catalog.Backend{
		row("backend-a", "openai-compatible", "https://provider.example/v1", "", "", catalog.BackendActive, stamp),
	}
	cfg := config.Config{}
	fake := &fakeBackendLists{script: []listOutcome{
		{err: errors.New("the read failed")},
	}}
	registry := &executorRegistry{}
	registry.store(buildExecutors(rows, cfg))
	before := registry.snapshot.Load()

	signature := refreshOnce(context.Background(), fake, registry, cfg, "the-boot-signature")

	if registry.snapshot.Load() != before {
		t.Error("a failed read swapped the snapshot — the last good one must keep serving")
	}
	if signature != "the-boot-signature" {
		t.Errorf("refreshOnce returned %q, want the given signature — a failed read seeds the next comparison with what is serving", signature)
	}
}

// TestRefreshOnceSwapsOnlyWhenTheCatalogChanged: rows that read the same as
// the serving snapshot swap nothing — executors are frozen, and rebuilding
// them would retire transports that may hold live streams — while rows that
// differ, an emptied catalog included, owe their one swap. The snapshot
// pointer is the observation: a skip leaves it where it stood, a swap
// replaces it whole.
func TestRefreshOnceSwapsOnlyWhenTheCatalogChanged(t *testing.T) {
	stamp := time.Unix(0, 0).UTC()
	rows := []catalog.Backend{
		row("backend-a", "openai-compatible", "https://provider.example/v1", "", "", catalog.BackendActive, stamp),
	}
	cfg := config.Config{}

	t.Run("identical rows swap nothing", func(t *testing.T) {
		fake := &fakeBackendLists{script: []listOutcome{{rows: rows}}}
		registry := &executorRegistry{}
		registry.store(buildExecutors(rows, cfg))
		before := registry.snapshot.Load()

		returned := refreshOnce(context.Background(), fake, registry, cfg, snapshotSignature(rows))

		if registry.snapshot.Load() != before {
			t.Error("identical rows swapped the snapshot — a rebuild retires transports that may hold live streams")
		}
		if returned != snapshotSignature(rows) {
			t.Errorf("refreshOnce returned %q, want the unchanged signature", returned)
		}
	})

	t.Run("changed rows swap once", func(t *testing.T) {
		changed := []catalog.Backend{
			row("backend-a", "openai-compatible", "https://provider.example/v1", "", "", catalog.BackendActive, stamp),
			row("backend-b", "openai-compatible", "https://other.example/v1", "", "", catalog.BackendActive, stamp),
		}
		fake := &fakeBackendLists{script: []listOutcome{{rows: changed}}}
		registry := &executorRegistry{}
		registry.store(buildExecutors(rows, cfg))
		before := registry.snapshot.Load()

		returned := refreshOnce(context.Background(), fake, registry, cfg, snapshotSignature(rows))

		if registry.snapshot.Load() == before {
			t.Error("changed rows left the snapshot in place — the new backend would never become callable")
		}
		if _, ok := (*registry.snapshot.Load())["backend-b"]; !ok {
			t.Error("the swapped snapshot carries no executor for the added row")
		}
		if returned != snapshotSignature(changed) {
			t.Errorf("refreshOnce returned %q, want the new rows' signature — the next comparison runs against what is serving", returned)
		}
	})

	t.Run("an emptied catalog still swaps", func(t *testing.T) {
		fake := &fakeBackendLists{script: []listOutcome{{rows: nil}}}
		registry := &executorRegistry{}
		registry.store(buildExecutors(rows, cfg))
		before := registry.snapshot.Load()

		returned := refreshOnce(context.Background(), fake, registry, cfg, snapshotSignature(rows))

		if registry.snapshot.Load() == before {
			t.Error("an emptied catalog left the snapshot in place — the swap to empty is the honest read")
		}
		if returned != "" {
			t.Errorf("refreshOnce returned %q, want the empty catalog's empty signature", returned)
		}
		if entries := *registry.snapshot.Load(); len(entries) != 0 {
			t.Errorf("the swapped snapshot holds %d executor(s), want none", len(entries))
		}
	})
}
