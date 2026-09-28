package postgres

import (
	"context"
	"database/sql"
	"encoding/hex"
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

// The live-filtered, keyed read (ADR 0008 §1 and §3). The `state <> 'removed'`
// predicate and the `(account_id, email)` key are both load-bearing and both
// come from the same partial unique index, `users_account_live_email_key`, that
// makes the result total — the query asks for the rows the index says may exist,
// and the index says there are at most one. There is no LIMIT and no ORDER BY
// here on purpose: a first-match would be a choice the engine never had to
// offer, and the case it would get wrong is the removed row and the live user
// who later re-invited its address, which an unfiltered predicate matches
// twice.
const selectUserByAccountAndEmail = `
SELECT id, account_id, email, state, created_at, updated_at
FROM control.users
WHERE account_id = $1 AND email = $2 AND state <> 'removed'`

// The sign-in statement. It is selectUserByAccountAndEmail with the three
// credential columns appended, and the repetition is deliberate rather than
// lazy: the two must return the SAME row, so a caller cannot be handed a user
// from one read and a credential from another, and composing one SELECT from
// the other's text would put that guarantee behind a string substitution
// nobody reviews. The predicate is character-for-character the same, including
// the `state <> 'removed'` filter, because a sign-in resolving a tombstone is
// the same defect whichever statement does it.
//
// The credential columns come back as NULLABLE because an `invited` row
// legitimately has none — the schema's whole-or-absent CHECK guarantees that
// a present credential is complete, so three nulls or three values is every
// state a row can be in, and there is no half-credential to reconcile here.
const selectUserCredentialByAccountAndEmail = `
SELECT id, account_id, email, state, created_at, updated_at,
       credential_hash, credential_salt, credential_iterations
FROM control.users
WHERE account_id = $1 AND email = $2 AND state <> 'removed'`

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

func (r *userRepo) ByAccountAndEmail(ctx context.Context, accountID identity.AccountID, email string) (identity.User, error) {
	var u identity.User
	var account, state string
	// QueryRowContext is the totality check in one call: the key is total by
	// the index, so a second row cannot exist and a first row is never
	// chosen — the driver reports zero rows as sql.ErrNoRows and the
	// translator below turns that into the port's one miss sentinel.
	err := r.store.Querier(ctx).QueryRowContext(ctx, selectUserByAccountAndEmail, string(accountID), email).
		Scan(&u.ID, &account, &u.Email, &state, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return identity.User{}, fmt.Errorf("postgres: user %s/%s: %w", accountID, email, persistence.ErrNotFound)
		}
		return identity.User{}, fmt.Errorf("postgres: user %s/%s: %w", accountID, email, err)
	}
	u.AccountID = identity.AccountID(account)
	u.State = identity.UserState(state)
	return u, nil
}

// CredentialByAccountAndEmail is the sign-in read: the live user and the
// credential stored on the SAME row, in one statement.
//
// The hex columns are decoded here rather than by the application, because
// their widths are the schema's business and the application should not have
// to know that a salt is 32 hex characters. A decode failure is reported as
// itself rather than as ErrNotFound — a row that cannot be decoded is a
// CORRUPTION, and reporting it as "no such user" would let it masquerade as a
// failed sign-in for as long as nobody noticed the distinction.
func (r *userRepo) CredentialByAccountAndEmail(ctx context.Context, accountID identity.AccountID, email string) (identity.User, *identity.CredentialDigest, error) {
	var u identity.User
	var account, state string
	var hashHex, saltHex *string
	var iterations *int

	err := r.store.Querier(ctx).QueryRowContext(ctx, selectUserCredentialByAccountAndEmail, string(accountID), email).
		Scan(&u.ID, &account, &u.Email, &state, &u.CreatedAt, &u.UpdatedAt,
			&hashHex, &saltHex, &iterations)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return identity.User{}, nil, fmt.Errorf("postgres: user %s/%s: %w", accountID, email, persistence.ErrNotFound)
		}
		return identity.User{}, nil, fmt.Errorf("postgres: user %s/%s: %w", accountID, email, err)
	}
	u.AccountID = identity.AccountID(account)
	u.State = identity.UserState(state)

	credential, err := decodeCredentialColumns(hashHex, saltHex, iterations)
	if err != nil {
		return identity.User{}, nil, fmt.Errorf("postgres: user %s/%s credential: %w", u.ID, email, err)
	}
	return u, credential, nil
}

// decodeCredentialColumns turns the three nullable columns into the aggregate's
// digest, or nil when the row has none.
//
// The three-absent case is an `invited` identity and is the only absence the
// schema permits: users_credential_whole requires all three or none, so a
// partial value cannot reach here from a database this schema created. The
// partial case is still handled — as a REFUSAL, not as a guess — because this
// adapter's caller must be able to trust the nil it gets, and a nil assembled
// from two of three columns would break that trust silently. A decoder that
// papered over it would be the difference between a corrupted row and a
// working sign-in, decided by a branch nobody wrote down.
func decodeCredentialColumns(hashHex, saltHex *string, iterations *int) (*identity.CredentialDigest, error) {
	if hashHex == nil && saltHex == nil && iterations == nil {
		return nil, nil
	}
	if hashHex == nil || saltHex == nil || iterations == nil {
		return nil, errors.New("the credential columns are partially set, which users_credential_whole forbids")
	}

	hash, err := hex.DecodeString(*hashHex)
	if err != nil {
		return nil, fmt.Errorf("the stored credential hash is not hex: %w", err)
	}
	salt, err := hex.DecodeString(*saltHex)
	if err != nil {
		return nil, fmt.Errorf("the stored credential salt is not hex: %w", err)
	}
	// The two widths are checked HERE rather than left to the verifier, and the
	// empty case is why: hex.DecodeString("") succeeds and yields zero bytes, so
	// a row holding an empty string would produce a digest whose Hash is empty —
	// and CredentialDigest.Verify substitutes its burn constant for a wrong-width
	// hash, which would turn a CORRUPT row into "wrong password" for as long as
	// the column stayed corrupt. The schema's shape checks make both values
	// unreachable, so this is the adapter refusing a row no database this schema
	// created can hold, and it says so by width rather than by emptiness: an
	// empty hash and a short hash are the same defect seen twice.
	if len(hash) != identity.CredentialHashBytes {
		return nil, fmt.Errorf("the stored credential hash is %d bytes, want %d", len(hash), identity.CredentialHashBytes)
	}
	if len(salt) != identity.CredentialSaltBytes {
		return nil, fmt.Errorf("the stored credential salt is %d bytes, want %d", len(salt), identity.CredentialSaltBytes)
	}
	return &identity.CredentialDigest{
		Hash:       hash,
		Salt:       salt,
		Iterations: *iterations,
	}, nil
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
