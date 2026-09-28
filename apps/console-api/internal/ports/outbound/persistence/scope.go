package persistence

import (
	"context"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
)

// RequestScope is the account one request speaks for, and the only way a
// caller of a read model can name the account its figures are about.
//
// It is a type with one field and no method on purpose. Everything it does NOT
// carry is the point: no account the caller chose, no filter, no scope
// parameter, nothing a query could be built from except this one derived value.
// A scope type that could also be constructed from request input would be a
// scope a caller could tamper with, and the tamper would be invisible in every
// review of the statement that used it — the statement would look identical.
type RequestScope struct {
	// AccountID is the account the resolved credential owns, and the account
	// whose buckets every statement built from this scope is filtered to.
	AccountID identity.AccountID
}

// Scoper turns a presented bearer token into the account it speaks for.
//
// It exists as a PORT and not as a function in the application because the
// secret's half of the verification is not in this plane's schema at all:
// control.api_keys holds the ownership record (account, key state) and no
// digest, because the digest is the Data Plane's. B2's identity domain
// declares the policy — VerifyCredential, which burns a constant-time digest
// comparison before it looks at any state — and this port is where a
// deployment supplies the lookup that policy runs against, including a
// transport that proves the identity cryptographically instead.
//
// The implementation this delivery wires is a STATIC BEARER TOKEN mapped to one
// account by the deployment, which is a real resolution and a real scope: the
// token is compared in constant time, an unknown token is a refusal, and no
// request can name an account because no request carries one. It is honest
// about what it is — the deployment owns credential storage, and a deployment
// that wants mTLS or a per-caller key table replaces this one port without
// touching a statement, a domain type, or this contract's guarantee that the
// scope is derived.
//
// What the port must never be: a default account. A resolution that cannot
// identify the caller must refuse, and must refuse the same way for a token
// that was never issued, because the difference between those two is the
// difference between "we could not tell who you are" and "that token does not
// exist", and only the first is safe to tell a caller.
type Scoper interface {
	// ScopeOf resolves the account the token on this request speaks for. It
	// returns an error for every unresolved credential, and the error carries
	// no account, no token, and no hint about what a different token might
	// have resolved to.
	ScopeOf(ctx context.Context, presented string) (RequestScope, error)
}
