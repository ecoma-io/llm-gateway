package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The credential mirror read: the request path's one query against the
// projection foundation's tables (ADR 0007, migration
// 000004_projection_foundation). It is the mirror's consumer side — the rows
// are written only by the projection applier in projection.go, and this file
// only reads them.
//
// The statement is deliberately the ONE join the port promises: the
// credential LEFT JOINed to its account state. The mirror's two tables have
// no foreign key between them by schema design — their rows arrive on
// independent feed entries — so under READ COMMITTED two separate reads
// could straddle an applier commit and pair a credential with an account
// state it never belonged to. One statement takes one snapshot: whatever the
// applier commits around it, the answer pairs the two lifecycles as they
// stood together. The LEFT JOIN, not an INNER JOIN, is what keeps the
// no-account-row case observable: the absence arrives as a NULL the caller
// fails closed on, rather than vanishing into a missed row.
const selectCredentialView = `
SELECT c.digest, c.state, c.revoked_at, c.account_id, a.state
FROM api_key_credentials c
LEFT JOIN account_states a ON a.account_id = c.account_id
WHERE c.key_id = $1`

// NewCredentials returns the persistence port's Credentials repository backed
// by store. It panics on a nil store for the same reason its sibling
// constructors do: the failure a nil dependency produces later is strictly
// worse than a loud one here.
func NewCredentials(store persistence.Store) persistence.Credentials {
	if store == nil {
		panic("postgres: NewCredentials requires a non-nil persistence.Store")
	}
	return &credentialsRepo{store: store}
}

// Compile-time proof that the repository satisfies the port's contract.
var _ persistence.Credentials = (*credentialsRepo)(nil)

type credentialsRepo struct {
	store persistence.Store
}

// Lookup reads one mirror row as CredentialView. A miss is
// ErrCredentialNotFound; a present credential with no account row keeps that
// absence as a nil AccountState — never coalesced, never defaulted — because
// the missing row is a mirror-integrity violation the caller must see and
// fail closed on. Any other error is the store's.
func (r *credentialsRepo) Lookup(ctx context.Context, keyID string) (persistence.CredentialView, error) {
	var view persistence.CredentialView
	var revokedAt sql.NullTime
	var accountState sql.NullString
	err := r.store.Querier(ctx).QueryRowContext(ctx, selectCredentialView, keyID).
		Scan(&view.Digest, &view.KeyState, &revokedAt, &view.AccountID, &accountState)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistence.CredentialView{}, fmt.Errorf("postgres: credential %s: %w", keyID, persistence.ErrCredentialNotFound)
		}
		return persistence.CredentialView{}, fmt.Errorf("postgres: credential %s: %w", keyID, err)
	}
	if revokedAt.Valid {
		at := revokedAt.Time
		view.RevokedAt = &at
	}
	if accountState.Valid {
		state := accountState.String
		view.AccountState = &state
	}
	return view, nil
}
