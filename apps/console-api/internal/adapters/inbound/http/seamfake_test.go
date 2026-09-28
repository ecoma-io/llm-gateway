package http

import (
	"context"
	"errors"
	"time"
)

// The test seam's fake. It lives in a _test.go file beside the tests rather
// than in a non-test file so it can never be reached by cmd/console-api, and it
// is the constructor ADR 0012's Consequences names: contract_test.go and the
// route inventory both need a SessionUseCases to call routes(), and the seam is
// a narrow interface precisely so a fake of it needs no database, no
// persistence port and no application wiring.
//
// Every use case records the call it received and returns a configured answer,
// so a test can assert both what a handler asked for (the account id came from
// the session, not the request) and what it answered. The defaults are a live
// session for a fixed Principal, because that is the state in which most of the
// surface is interesting; a test that wants otherwise sets a field.
type fakeSessionUseCases struct {
	// signIn is the answer SignIn returns. The zero value's error is nil, so a
	// test that does not care about sign-in gets a successful one and must set
	// signInErr to exercise a refusal.
	signInResult SessionResult
	signInErr    error

	// sessionResult is what a live session resolves to, and sessionErr is what a
	// missing, expired or revoked one produces. The default sessionErr is nil,
	// meaning "the session is live" — the common case.
	sessionResult SessionResult
	sessionErr    error

	// signOutErr is SignOut's answer.
	signOutErr error

	// mintResult is MintAPIKey's answer, and mintErr its refusal.
	mintResult MintedAPIKeyResult
	mintErr    error

	// The calls each use case received, in order, for a test to assert on.
	signInCalls  []SignInInput
	sessionCalls []SessionToken
	signOutCalls []SessionToken
	mintCalls    []MintAPIKeyInput
}

func (f *fakeSessionUseCases) SignIn(_ context.Context, in SignInInput) (SessionResult, error) {
	f.signInCalls = append(f.signInCalls, in)
	return f.signInResult, f.signInErr
}

func (f *fakeSessionUseCases) Session(_ context.Context, token SessionToken) (SessionResult, error) {
	f.sessionCalls = append(f.sessionCalls, token)
	return f.sessionResult, f.sessionErr
}

func (f *fakeSessionUseCases) SignOut(_ context.Context, token SessionToken) error {
	f.signOutCalls = append(f.signOutCalls, token)
	return f.signOutErr
}

func (f *fakeSessionUseCases) MintAPIKey(_ context.Context, in MintAPIKeyInput) (MintedAPIKeyResult, error) {
	f.mintCalls = append(f.mintCalls, in)
	return f.mintResult, f.mintErr
}

// liveSessionResult is the session a fake resolves a good cookie to. The
// account id is a fixed UUID-shaped string and the class is `user` — the only
// class the first console mints (ADR 0012 §2) — so a test that asserts "the
// account came from the session" has something real to compare against.
func liveSessionResult() SessionResult {
	return SessionResult{
		Principal: Principal{
			Class:     principalClassUser,
			AccountID: "11111111-1111-4111-8111-111111111111",
			UserID:    "22222222-2222-4222-8222-222222222222",
			Email:     "operator@example.com",
		},
		Token:     SessionToken(string(mustToken())),
		ExpiresAt: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC),
	}
}

// mustToken mints a session token for a test that needs a cookie value without
// caring what it is. It never returns an error because newSessionToken panics
// rather than returning one, and a test harness cannot recover from a missing
// entropy source any more usefully than the server can.
func mustToken() SessionToken { return newSessionToken() }

// a well-formed mint answer, for a test that wants a 201 without building one.
func liveMintedKey() MintedAPIKeyResult {
	return MintedAPIKeyResult{
		Record: APIKeyRecord{
			ID:          "33333333-3333-4333-8333-333333333333",
			AccountID:   "11111111-1111-4111-8111-111111111111",
			DisplayName: "ci",
			Prefix:      "gw_33333333",
			State:       string(apiKeyStateActive),
			CreatedAt:   "2026-09-28T12:00:00Z",
		},
		Token: "gw_33333333-3333-4333-8333-333333333333_secretpart",
	}
}

// errNoSession is what the fake returns for a session that is not live, and it
// is the same error for all three reasons a session can be dead. The handler
// must not be able to tell them apart, and the fake is written so it cannot:
// it has one field, not three.
var errNoSession = errors.New("no such session")

// a live fake: a session that resolves, a sign-in that succeeds, a mint that
// succeeds. Most handler tests start from here and override the one field they
// are about.
func newFakeSessionUseCases() *fakeSessionUseCases {
	return &fakeSessionUseCases{
		signInResult:  liveSessionResult(),
		sessionResult: liveSessionResult(),
		mintResult:    liveMintedKey(),
		sessionErr:    nil,
	}
}
