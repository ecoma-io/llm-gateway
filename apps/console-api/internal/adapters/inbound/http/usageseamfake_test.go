package http

import (
	"context"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
)

// The usage read's test seam, in one place rather than one per test file,
// because it is a fact about the package's wiring rather than about any one
// case: a handler is mounted on it, and every test that builds a server needs
// one. It lives in a _test.go file for the reason the ten reads' fake does —
// cmd/console-api can never reach it — and it stands in for a seam of plain
// fields only (usage.go), so neither this file nor any test that uses it names
// a grammar. What a usage answer MEANS is decided below the seam: the domain
// cuts the buckets, the store reads them and the use case attributes them. The
// transport's claim is that it renders what it was handed.
//
// stubToken and stubAccount are the one credential and the one account this
// package's tests share, so a case about the credential reads as a case about
// one credential rather than as a case about a token generator.
const (
	stubToken   = "console-test-token"
	stubAccount = "018f0000-0000-7000-8000-000000000001"
)

// fakeUsageUseCases is the seam a case controls.
//
// It resolves ONE credential and refuses every other, because that is the
// property the surface has: the account is derived from the credential and from
// nowhere else. A fake that answered whatever account a test asked it for would
// let every other assertion in this package pass while the one property that
// matters was never under test. The resolution itself is not this file's claim
// — StaticScoper's own tests pin the comparison, and the use case's pin what an
// unresolved credential becomes — so the refusal here is the shape of that
// answer, taken from the same constructor the use case uses.
//
// Every call is recorded, because two of the cases below are about the seam NOT
// being reached, and "the read was never asked" is a claim only a counter can
// make.
type fakeUsageUseCases struct {
	// answer is what a resolved credential's read returns.
	answer UsageAnswer
	// err, when set, is what the read answers with whatever the credential was.
	err error

	calls []usageCall
}

// usageCall is one request as the seam received it: the credential the handler
// extracted and the question it bound. Both are the transport's to get right —
// the token is handed over whole rather than interpreted, and the request is
// bound from the query string — so both are what a case asserts on.
type usageCall struct {
	token   string
	request application.UsageRequest
}

// Usage implements UsageUseCases.
func (f *fakeUsageUseCases) Usage(_ context.Context, token string, request application.UsageRequest) (UsageAnswer, error) {
	f.calls = append(f.calls, usageCall{token: token, request: request})
	if f.err != nil {
		return UsageAnswer{}, f.err
	}
	if token != stubToken {
		return UsageAnswer{}, application.UnresolvedCredential()
	}
	return f.answer, nil
}

// stubUsage is the stand-in every test that mounts a server needs and is not
// about the usage answer itself: it resolves the shared credential and answers
// it with an empty range, which is the honest answer for a test server and the
// one that exercises the envelope's not_available branch rather than a
// fabricated series of figures.
func stubUsage() UsageUseCases {
	return &fakeUsageUseCases{answer: emptyUsageAnswer()}
}

// Compile-time proof that the fake stands in for the seam the handler calls and
// not for something wider, so a seam that grew a method would fail here rather
// than in a case that quietly stopped covering it.
var _ UsageUseCases = (*fakeUsageUseCases)(nil)
