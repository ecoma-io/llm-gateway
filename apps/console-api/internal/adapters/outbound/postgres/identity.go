package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The identity repositories: translation between the persistence port's
// identity vocabulary and the `control` database's identity tables. Every
// query resolves its handle through the store — the caller's unit of work
// when the context carries one, the pool otherwise — so a repository can no
// more escape a WithinTx scope than a caller can smuggle one in.
//
// The boundary this file deliberately does not cross: none of these tables
// holds secret material (ADR 0006 §8). api_keys carries ownership, display
// metadata and lifecycle; the digest lives in the Data Plane's credential
// record, which no statement in this file can reach — not by discipline, by
// database: the `dataplane` database is a separate migration history and a
// separate connection target, and no statement here names it.

// pgUniqueViolation is PostgreSQL's SQLSTATE for a uniqueness constraint
// violation. Detected through the SQLState seam below rather than a driver
// type, so the mapping survives whichever database/sql driver the process
// boundary wires in — both drivers this module could reasonably adopt report
// SQLSTATE through the same interface{} shape.
const pgUniqueViolation = "23505"

// sqlStater is the seam for driver errors that carry a SQLSTATE: any error
// value whose chain holds one is a database-reported condition the adapter
// may classify. It is deliberately unexported and structural — a capability
// the driver either has or has not — rather than an import of one driver's
// error type.
type sqlStater interface {
	SQLState() string
}

// sqlStateOf returns the SQLSTATE the error chain carries, if any.
func sqlStateOf(err error) (string, bool) {
	var s sqlStater
	if errors.As(err, &s) {
		return s.SQLState(), true
	}
	return "", false
}

// NewAccounts returns the persistence port's Accounts repository backed by
// store. It panics on a nil store for the same reason the store panics on a
// nil pool: the failure a nil dependency produces later is strictly worse
// than a loud one here.
func NewAccounts(store persistence.Store) persistence.Accounts {
	if store == nil {
		panic("postgres: NewAccounts requires a non-nil persistence.Store")
	}
	return &accountRepo{store: store}
}

// NewUsers returns the persistence port's Users repository backed by store.
func NewUsers(store persistence.Store) persistence.Users {
	if store == nil {
		panic("postgres: NewUsers requires a non-nil persistence.Store")
	}
	return &userRepo{store: store}
}

// NewAPIKeys returns the persistence port's APIKeys repository backed by
// store.
func NewAPIKeys(store persistence.Store) persistence.APIKeys {
	if store == nil {
		panic("postgres: NewAPIKeys requires a non-nil persistence.Store")
	}
	return &keyRepo{store: store}
}

// Compile-time proof that the repositories satisfy the port's contracts.
var (
	_ persistence.Accounts = (*accountRepo)(nil)
	_ persistence.Users    = (*userRepo)(nil)
	_ persistence.APIKeys  = (*keyRepo)(nil)
)

// ---------------------------------------------------------------------------
// accounts
// ---------------------------------------------------------------------------

type accountRepo struct {
	store persistence.Store
}

const insertAccount = `
INSERT INTO control.accounts (id, name, state, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5)`

const selectAccount = `
SELECT id, name, state, created_at, updated_at
FROM control.accounts
WHERE id = $1`

// The compare-and-swap is the whole concurrency story: the move happens only
// while the row still shows the state the caller read, so two racing
// transitions converge instead of one silently overwriting the other.
const transitionAccount = `
UPDATE control.accounts
SET state = $3, updated_at = $4
WHERE id = $1 AND state = $2`

func (r *accountRepo) Create(ctx context.Context, account identity.Account) error {
	_, err := r.store.Querier(ctx).ExecContext(ctx, insertAccount,
		string(account.ID), account.Name, string(account.State), account.CreatedAt, account.UpdatedAt)
	if err != nil {
		return fmt.Errorf("postgres: create account %s: %w", account.ID, err)
	}
	return nil
}

func (r *accountRepo) ByID(ctx context.Context, id identity.AccountID) (identity.Account, error) {
	var a identity.Account
	var state string
	err := r.store.Querier(ctx).QueryRowContext(ctx, selectAccount, string(id)).
		Scan(&a.ID, &a.Name, &state, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return identity.Account{}, fmt.Errorf("postgres: account %s: %w", id, persistence.ErrNotFound)
		}
		return identity.Account{}, fmt.Errorf("postgres: account %s: %w", id, err)
	}
	a.State = identity.AccountState(state)
	return a, nil
}

func (r *accountRepo) TransitionState(ctx context.Context, id identity.AccountID, from, to identity.AccountState, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, transitionAccount,
		string(id), string(from), string(to), updatedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: transition account %s %s->%s: %w", id, from, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: transition account %s: read rows affected: %w", id, err)
	}
	return n == 1, nil
}

// ---------------------------------------------------------------------------
// users
// ---------------------------------------------------------------------------

type userRepo struct {
	store persistence.Store
}

const insertUser = `
INSERT INTO control.users (id, account_id, email, state, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)`

const selectUser = `
SELECT id, account_id, email, state, created_at, updated_at
FROM control.users
WHERE id = $1`

const transitionUser = `
UPDATE control.users
SET state = $3, updated_at = $4
WHERE id = $1 AND state = $2`

func (r *userRepo) Create(ctx context.Context, user identity.User) error {
	_, err := r.store.Querier(ctx).ExecContext(ctx, insertUser,
		string(user.ID), string(user.AccountID), user.Email, string(user.State), user.CreatedAt, user.UpdatedAt)
	if err != nil {
		// On this insert a uniqueness violation can only be the live-email
		// index: the id is a fresh random UUID, so the primary key is not a
		// realistic second path to 23505. The domain sentinel keeps that
		// reasoning here instead of leaking a constraint name upward.
		if state, ok := sqlStateOf(err); ok && state == pgUniqueViolation {
			return fmt.Errorf("postgres: create user %s: %w", user.ID, identity.ErrUserEmailTaken)
		}
		return fmt.Errorf("postgres: create user %s: %w", user.ID, err)
	}
	return nil
}

func (r *userRepo) ByID(ctx context.Context, id identity.UserID) (identity.User, error) {
	var u identity.User
	var accountID, state string
	err := r.store.Querier(ctx).QueryRowContext(ctx, selectUser, string(id)).
		Scan(&u.ID, &accountID, &u.Email, &state, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return identity.User{}, fmt.Errorf("postgres: user %s: %w", id, persistence.ErrNotFound)
		}
		return identity.User{}, fmt.Errorf("postgres: user %s: %w", id, err)
	}
	u.AccountID = identity.AccountID(accountID)
	u.State = identity.UserState(state)
	return u, nil
}

func (r *userRepo) TransitionState(ctx context.Context, id identity.UserID, from, to identity.UserState, updatedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, transitionUser,
		string(id), string(from), string(to), updatedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: transition user %s %s->%s: %w", id, from, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: transition user %s: read rows affected: %w", id, err)
	}
	return n == 1, nil
}

// ---------------------------------------------------------------------------
// api_keys — the ownership record; no column here is secret material.
// ---------------------------------------------------------------------------

type keyRepo struct {
	store persistence.Store
}

const insertAPIKey = `
INSERT INTO control.api_keys (id, account_id, created_by, display_name, prefix, state, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

const selectAPIKey = `
SELECT id, account_id, created_by, display_name, prefix, state, created_at, updated_at, revoked_at
FROM control.api_keys
WHERE id = $1`

// The one-way move. revoked_at is stamped in the same statement as the state
// flip, and the schema's api_keys_revocation_consistency check would refuse
// the row if the two ever disagreed — the statement and the constraint say
// the same thing, which is what makes either of them trustworthy.
const revokeAPIKey = `
UPDATE control.api_keys
SET state = 'revoked', revoked_at = $2, updated_at = $2
WHERE id = $1 AND state = 'active'`

func (r *keyRepo) Create(ctx context.Context, key identity.APIKey) error {
	// created_by is NULL when the domain says "no creator": the zero UserID
	// is that absence, and an empty string is not.
	var createdBy any
	if key.CreatedBy != "" {
		createdBy = string(key.CreatedBy)
	}
	_, err := r.store.Querier(ctx).ExecContext(ctx, insertAPIKey,
		string(key.ID), string(key.AccountID), createdBy, key.DisplayName, key.Prefix, string(key.State), key.CreatedAt, key.UpdatedAt)
	if err != nil {
		return fmt.Errorf("postgres: create api key %s: %w", key.ID, err)
	}
	return nil
}

func (r *keyRepo) ByID(ctx context.Context, id identity.APIKeyID) (identity.APIKey, error) {
	var k identity.APIKey
	var accountID, state string
	var createdBy sql.NullString
	var revokedAt sql.NullTime
	err := r.store.Querier(ctx).QueryRowContext(ctx, selectAPIKey, string(id)).
		Scan(&k.ID, &accountID, &createdBy, &k.DisplayName, &k.Prefix, &state, &k.CreatedAt, &k.UpdatedAt, &revokedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return identity.APIKey{}, fmt.Errorf("postgres: api key %s: %w", id, persistence.ErrNotFound)
		}
		return identity.APIKey{}, fmt.Errorf("postgres: api key %s: %w", id, err)
	}
	k.AccountID = identity.AccountID(accountID)
	k.CreatedBy = identity.UserID(createdBy.String)
	k.State = identity.APIKeyState(state)
	if revokedAt.Valid {
		t := revokedAt.Time
		k.RevokedAt = &t
	}
	return k, nil
}

func (r *keyRepo) Revoke(ctx context.Context, id identity.APIKeyID, revokedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, revokeAPIKey, string(id), revokedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: revoke api key %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: revoke api key %s: read rows affected: %w", id, err)
	}
	return n == 1, nil
}
