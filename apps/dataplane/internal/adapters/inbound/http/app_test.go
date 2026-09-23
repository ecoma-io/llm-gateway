package http

import (
	"context"
	"errors"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
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

// newTestApp returns the application under test for this package's handlers.
//
// It exists so that the fact reader this process needs — and this surface does
// not use — is stated once, in a file whose name says why it is there, instead
// of at every call site as an argument a reader has to interpret.
func newTestApp(t *testing.T, version string) *application.App {
	t.Helper()
	return application.New(version, noFacts{t: t})
}
