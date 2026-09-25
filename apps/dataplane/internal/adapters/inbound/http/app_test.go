package http

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/usagefacts"
)

// noFacts is the fact reader this package's tests construct the application
// with, and it fails the test if it is ever called.
//
// That is an assertion rather than a placeholder. The runtime's surface serves
// no fact-feed operation: reading facts is the management listener's job, and a
// request path that reached them would be the two-plane split failing in the
// one place it is least visible — a use case that quietly depends on the store
// the Control Plane reads from. A stub that returned an empty page would let
// that happen with every test still green; this one does not.
type noFacts struct{ t *testing.T }

// Read implements usagefacts.Reader.
func (f noFacts) Read(context.Context, string, int) (usagefacts.Page, error) {
	f.t.Error("the runtime surface read usage facts; that is the management listener's surface, not this one")
	return usagefacts.Page{}, errors.New("the runtime surface has no fact reader")
}

// The catalog ports behind the application this package's tests build. They
// are the same assertion as noFacts, one layer further out: the runtime's
// surface answers no catalog question — the group-version read is the
// management listener's operation, and a runtime route that reached into the
// catalog would put a cross-plane fact on the address that serves LLM
// traffic — so each port fails the test it is reached from. The catalog itself
// is the real one over these ports, because the use case is concrete rather
// than an interface and New refuses an App without one; what is stubbed is the
// store it would have to ask, which is where a leak would show.
type silentStore struct {
	persistence.Store
	t *testing.T
}

// WithinTx implements persistence.Store.
func (s silentStore) WithinTx(context.Context, func(context.Context) error) error {
	s.t.Error("the runtime surface opened a unit of work against the catalog; that is the management listener's surface, not this one")
	return errors.New("the runtime surface has no catalog")
}

type silentBackends struct{ persistence.Backends }

type silentAliases struct{ persistence.ModelAliases }

type silentVersions struct {
	persistence.AliasGroupVersions
	t *testing.T
}

// HighestVersion implements persistence.AliasGroupVersions — the first read
// CurrentGroupVersion makes, and so the one a leaking route hits first.
func (v silentVersions) HighestVersion(context.Context, string) (int, error) {
	v.t.Error("the runtime surface read the catalog; that is the management listener's surface, not this one")
	return 0, errors.New("the runtime surface has no catalog")
}

// noProjections is the projection applier this package's tests construct the
// application with, and it fails the test if it is ever touched — the same
// assertion as noFacts, for the same reason on the other port: applying the
// credential mirror is the management listener's surface, and a request path
// that wrote it would be a Control Plane decision taken at inference time.
type noProjections struct{ t *testing.T }

// Position implements persistence.ProjectionApplier.
func (p noProjections) Position(context.Context) (projection.Position, error) {
	p.t.Error("the runtime surface read the projection position; that is the management listener's surface, not this one")
	return projection.Position{}, errors.New("the runtime surface has no projection applier")
}

// ApplySnapshot implements persistence.ProjectionApplier.
func (p noProjections) ApplySnapshot(context.Context, projection.Snapshot, time.Time) (uint64, error) {
	p.t.Error("the runtime surface applied a projection snapshot; that is the management listener's surface, not this one")
	return 0, errors.New("the runtime surface has no projection applier")
}

// ApplyChanges implements persistence.ProjectionApplier.
func (p noProjections) ApplyChanges(context.Context, projection.Batch) (uint64, error) {
	p.t.Error("the runtime surface applied projection changes; that is the management listener's surface, not this one")
	return 0, errors.New("the runtime surface has no projection applier")
}

// newTestApp returns the application under test for this package's handlers.
//
// It exists so that the ports this process needs — and this surface does not
// use — are stated once, in a file whose name says why it is there, instead
// of at every call site as an argument a reader has to interpret.
func newTestApp(t *testing.T, version string) *application.App {
	t.Helper()
	catalog := application.NewCatalog(silentStore{t: t}, silentBackends{}, silentAliases{}, silentVersions{t: t})
	return application.New(version, noFacts{t: t}, catalog, noProjections{})
}
