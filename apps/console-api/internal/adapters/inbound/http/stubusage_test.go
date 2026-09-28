package http

import (
	"context"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The shared stand-in for the usage use case, in one place rather than one
// per test file, because it is a fact about the package's wiring rather than
// about any one route: a handler is mounted on it, and every test that builds
// a server needs one.
//
// It resolves a FIXED credential to a FIXED account and reads a fixed empty
// answer. That is the smallest stand-in that is still honest about the one
// property the surface has: the account is derived from the credential and
// from nowhere else. A stub that accepted whatever account a test asked it
// for would let every other assertion in this package pass while the one
// property that matters was never under test.
const (
	stubToken   = "console-test-token"
	stubAccount = "018f0000-0000-7000-8000-000000000001"
)

// stubReadModel is a store that holds no derived rows and answers with
// NOT_AVAILABLE for the account the stub credential speaks for — the honest
// answer for a test server, and the one that exercises the envelope's
// not_available branch rather than a fabricated series of figures.
type stubReadModel struct{}

// Usage implements persistence.Analytics.
func (stubReadModel) Usage(context.Context, persistence.UsageQuery) (persistence.Usage, persistence.Freshness, error) {
	return persistence.Usage{}, persistence.Freshness{}, nil
}

// stubScoper is a resolver with exactly one entry, so a test can present a
// credential and know which account it lands on without configuring a table
// per case.
type stubScoper struct{}

var scopedAccount = identity.AccountID(stubAccount)

// ScopeOf implements persistence.Scoper.
func (stubScoper) ScopeOf(_ context.Context, presented string) (persistence.RequestScope, error) {
	if presented != stubToken {
		// The one refusal, for every reason, in the same shape the real
		// resolver refuses: an unresolved credential is not described.
		return persistence.RequestScope{}, ErrUnresolvedCredential
	}
	return persistence.RequestScope{AccountID: scopedAccount}, nil
}

// stubUsage is the use case this package's tests mount. It is the PRODUCTION
// composition — the real use case over the real Scoper port — so a test
// exercising the handler is exercising the wiring a deployment gets rather
// than a test-only shape that skips the scope resolution entirely.
func stubUsage() *application.Usage {
	return application.NewUsageUseCase(stubReadModel{}, stubScoper{})
}

// Compile-time proofs that the two stubs stand in for the ports and not for
// something wider, so a port that grew a member would fail here rather than in
// a test that quietly stopped covering it.
var (
	_ persistence.Analytics = stubReadModel{}
	_ persistence.Scoper    = stubScoper{}
)
