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

// ScopeFor is the one way a resolver builds a scope from the account
// identifier it resolved, and it exists so that a resolver does not have to
// name the identity grammar to do it.
//
// The conversion is trivial and that is not the point. The account id is a
// DOMAIN value with a canonical form the grammar owns, and the packages that
// may speak the grammar are enumerated in the dependency rule
// (internal/arch/imports_test.go). A bearer-token resolver is a transport
// concern — it reads a header, compares a digest in constant time and answers
// with the account it names — and a transport that could name the identity
// grammar directly would be one allow-list entry away from naming the
// credential's own type, whose one secret-bearing form is the digest. So the
// conversion lives here, in the vocabulary both sides already speak, and the
// transport passes a plain string across the port.
//
// It validates nothing beyond that, and deliberately: a resolver is the layer
// that decides what a malformed identifier means (this deployment refuses one
// at construction), and a constructor that silently accepted or replaced an
// empty account would be the default account this package's Scoper doc
// forbids.
func ScopeFor(accountID string) RequestScope {
	return RequestScope{AccountID: identity.AccountID(accountID)}
}

// ScopeForConfigured is ScopeFor for an identifier that arrived as TEXT — a
// deployment's own configuration naming which account a credential speaks for
// — and it refuses one the account grammar does not accept.
//
// The distinction from ScopeFor is where the string came from and not how
// strict this is. An identifier read from the database is a `uuid` column and
// the database has already answered the question; an identifier written by
// hand in an environment variable has had nobody check it, and its first use
// is a cast failure inside a query — every request answered with a server
// fault because an operator mistyped a configuration value. This is the
// constructor that lets a deployment find that out at start-up, and it is why
// ScopeFor's own note says the resolver is the layer that decides what a
// malformed identifier means: the resolver is the layer that holds it as text.
//
// The grammar check itself stays in the domain and is reached through here, so
// the transport refuses a bad identifier without ever naming the type it is
// not allowed to name.
func ScopeForConfigured(accountID string) (RequestScope, error) {
	if err := identity.ValidateAccountID(accountID); err != nil {
		return RequestScope{}, err
	}
	return ScopeFor(accountID), nil
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
