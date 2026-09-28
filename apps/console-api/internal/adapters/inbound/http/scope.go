package http

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// ErrUnresolvedCredential is what every refusal from a Scoper is, and it is one
// sentinel for every reason: an absent token, a token the deployment never
// issued, and a token that was issued and has been retired all answer with this
// and with nothing else.
//
// Collapsing them is the security property, not a convenience. A resolver that
// distinguished "no such token" from "that token is retired" would be an
// oracle over the credential space, and a caller with one retired token would
// learn it from the difference. A caller learns only what it already knows —
// this token does not work — and learns it identically for every token that
// does not work.
var ErrUnresolvedCredential = errors.New("the credential presented does not resolve to an account")

// StaticScoper resolves a bearer token to an account by a table the deployment
// supplies, and is what this delivery wires in place of a credential store.
//
// It exists because the secret's half of the verification is not in this
// plane. control.api_keys holds the OWNERSHIP record — account, key state, and
// no digest at all — because the digest is the Data Plane's, and the Data
// Plane's requests table is not reachable across the plane boundary
// (docs/architecture/analytics.md §1.1 is the same argument for the fact's
// account). Writing a resolver over a column that does not exist would have
// meant either inventing one, which is a signature change to the identity
// record taken in an analytics delivery, or reading the secret from a
// deployment that has none.
//
// What this adapter therefore guarantees, and what a replacement must too:
//
//   - THE COMPARISON IS CONSTANT TIME. sha256 of the presented token against
//     each configured token, compared with subtle.ConstantTimeCompare. A
//     byte-by-byte compare would leak a prefix-match length through wall time,
//     and a token is short enough to be guessed from a timing signal.
//   - NO TOKEN IS STORED IN PLAIN COMPARABLE FORM. The constructor digests
//     each configured token once and the struct holds digests, so a heap dump
//     or a log line of the resolver's state carries no credential. This is the
//     same discipline identity.Digest already applies to a key secret.
//   - A REFUSAL NAMES NOTHING. Not the account, not the token, not how many
//     candidates were compared. The error is the sentinel, constant, and the
//     loop always runs to completion over every candidate so the number of
//     comparisons never depends on which token was presented either.
//   - THE SCOPE IS NOT A DEFAULT. A construction that resolves an unknown
//     token to some account would be an unauthenticated read of a real
//     customer's spend, and the struct has no field that could hold one.
type StaticScoper struct {
	// digests is the deployment's table, in configuration order, each entry a
	// sha256 of one configured token. The account travels beside the digest
	// because the pair is the record; a digest without its account would be a
	// credential store with nothing to authorize.
	digests []scopedDigest
}

type scopedDigest struct {
	digest [sha256.Size]byte
	// accountID is the deployment's own spelling of the account, held as the
	// string it was configured as and converted at the one place a scope is
	// built. The transport does not name the identity grammar — see
	// persistence.ScopeFor — so the table's account column arrives and leaves
	// this package as text.
	accountID string
}

// NewStaticScoper builds the resolver from the deployment's token-to-account
// table. Every token must be non-empty and every account must be a canonical
// account identifier: a misconfigured deployment should fail to start, not
// start with a credential that resolves to an account nobody owns.
//
// The table is digested here, once, so that the comparison path is a fixed
// number of SHA-256 evaluations over fixed-size inputs and nothing that varies
// with the presented token's content.
func NewStaticScoper(table map[string]string) (*StaticScoper, error) {
	if len(table) == 0 {
		return nil, errors.New("http: the static scope table is empty: a surface that serves one account's figures and cannot name a caller is a surface with no access control at all")
	}
	resolver := &StaticScoper{digests: make([]scopedDigest, 0, len(table))}
	for token, accountID := range table {
		if token == "" {
			return nil, errors.New("http: the static scope table carries an empty token: an empty credential must never resolve to an account")
		}
		if accountID == "" {
			return nil, fmt.Errorf("http: the static scope table maps a token to an empty account: a scope that is not an account is not a scope")
		}
		// The grammar check the constructor's own doc promises, made where the
		// value is still configuration. It is asked through the port rather
		// than here because this package may not name the identity grammar —
		// which is the same reason the table arrives as text at all.
		//
		// The refusal names the ACCOUNT and never the token. The account is
		// not a secret and is the only thing that identifies the entry: the
		// table arrives as a map, so its order is Go's and varies between
		// runs, and an operator told nothing but "one of your account ids is
		// wrong" has no way to find it. The token is the credential, and a
		// start-up log line is still a log line.
		if _, err := persistence.ScopeForConfigured(accountID); err != nil {
			return nil, fmt.Errorf("http: the static scope table maps a token to the account %q, which this plane cannot resolve: %w", accountID, err)
		}
		resolver.digests = append(resolver.digests, scopedDigest{
			digest:    sha256.Sum256([]byte(token)),
			accountID: accountID,
		})
	}
	return resolver, nil
}

// ScopeOf resolves the account a presented bearer token speaks for.
//
// The whole loop runs for every presentation, matching or not, and the match
// is recorded rather than returned from inside the loop. Returning early on a
// match would make the comparison count depend on the outcome, which is a
// narrower timing channel than the byte comparison is — the same one a
// constant-time compare exists to close.
func (s *StaticScoper) ScopeOf(_ context.Context, presented string) (persistence.RequestScope, error) {
	presentedDigest := sha256.Sum256([]byte(presented))

	var resolved string
	matched := 0
	for _, candidate := range s.digests {
		// ConstantTimeCompare returns 0 for a length mismatch without
		// inspecting content, which is fine: every candidate digest is the
		// same 32 bytes by construction, so the lengths here can never differ
		// and the early return is unreachable rather than a shortcut.
		if subtle.ConstantTimeCompare(candidate.digest[:], presentedDigest[:]) == 1 {
			resolved = candidate.accountID
			matched++
		}
	}

	if matched != 1 {
		// Zero and more-than-one are the same refusal, deliberately. Two
		// configured tokens resolving to the same account is a deployment
		// mistake; answering it by picking one would make the deployment's
		// configuration decide which of its own credentials works, which is
		// not a thing a caller can reason about and not a thing a report
		// should depend on.
		return persistence.RequestScope{}, ErrUnresolvedCredential
	}
	return persistence.ScopeFor(resolved), nil
}
