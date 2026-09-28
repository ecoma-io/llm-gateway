package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The console's identity reads: one account's users, one account's API keys.
//
// Two facts govern every statement in this file and neither is a style choice.
//
// The first is that the account predicate is the WHERE clause, and it is the
// FIRST argument. It is not a filter applied after the rows arrive, and there
// is no forbidden sentinel anywhere on this path: a user's or a key's
// belonging to another account has to be a row the query never returned, so
// that it costs exactly what a row that does not exist costs. A post-fetch
// check answers "not yours" in a different time from "not there", and the
// difference is a confirmation oracle — ADR 0012 §2 states the rule and this
// file is where it is mechanical.
//
// The second is the keyset, and it is the same rule accounting.Sweep's own
// docblock states: OFFSET re-reads and re-discards every row already passed,
// and a walk that pages by OFFSET while rows land concurrently can both skip
// a row and read one twice. Every statement here asks for limit+1 rows and
// orders by a UNIQUE key, so a page can neither drop a row nor repeat one.
//
// The optional filters are in the same statement and not in Go. A `state = $2`
// the query does not carry when the caller asked for every state is a second
// code path, and a second code path is where the missing predicate would
// eventually live.

// NewAccountUsers returns the persistence port's account-scoped users
// repository, backed by store. It panics on a nil store for the reason every
// constructor in this package does: the failure a nil dependency produces
// later is strictly worse than a loud one here.
func NewAccountUsers(store persistence.Store) persistence.AccountUsers {
	if store == nil {
		panic("postgres: NewAccountUsers requires a non-nil persistence.Store")
	}
	return &accountUsersRepo{store: store}
}

// NewAccountAPIKeys returns the persistence port's account-scoped keys
// repository, backed by store, and panics on a nil store for the same reason.
func NewAccountAPIKeys(store persistence.Store) persistence.AccountAPIKeys {
	if store == nil {
		panic("postgres: NewAccountAPIKeys requires a non-nil persistence.Store")
	}
	return &accountAPIKeysRepo{store: store}
}

// Compile-time proof that the repositories satisfy the port's contracts.
var (
	_ persistence.AccountUsers   = (*accountUsersRepo)(nil)
	_ persistence.AccountAPIKeys = (*accountAPIKeysRepo)(nil)
)

// pageLimit resolves the caller's page size to the number of rows the
// statement should ask for: one more than wanted, because `has_more` is
// derived from the extra row and there is no count query anywhere near it.
//
// The bounds are the contract's own, and they are enforced here rather than
// clamped: a caller that asked for a page outside them asked a question the
// contract does not answer, and the smaller page it would otherwise get is
// one it cannot tell apart from a page the collection capped itself. The
// zero value is the contract's default, so a caller that did not care gets
// the documented page rather than an error.
func pageLimit(limit int) (int, error) {
	if limit == 0 {
		return persistence.DefaultPageLimit + 1, nil
	}
	if limit < persistence.MinPageLimit || limit > persistence.MaxPageLimit {
		return 0, fmt.Errorf("postgres: page limit %d is outside [%d, %d]",
			limit, persistence.MinPageLimit, persistence.MaxPageLimit)
	}
	return limit + 1, nil
}

// ---------------------------------------------------------------------------
// users
// ---------------------------------------------------------------------------

type accountUsersRepo struct {
	store persistence.Store
}

// The list, with the account predicate FIRST and the keyset second. The
// state filter is the third, and it is a real predicate in the same
// statement rather than a Go-side test on rows already fetched: a filter the
// query does not carry is a filter whose absence would return another
// account's row.
//
// The zero-`after` case is `after > $2` against the uuid nil, which no
// real uuid compares greater than, so the first page is the first page. It
// is written as one statement rather than two because two statements is two
// shapes for the account predicate to be missing from, and the second is the
// one that would ship.
const listAccountUsers = `
SELECT ` + userColumns + `
FROM control.users
WHERE account_id = $1
  AND id > $2
  AND ($3 = '' OR state = $3)
ORDER BY id
LIMIT $4`

// The live count, which is the dashboard's user_count and not a page total:
// invited plus active, excluding removed, over an indexed predicate. It is
// here because the contract names the figure and explains why — an operator
// reading "12 users" should not have to page eleven screens — and it is the
// only count this file has.
const countLiveAccountUsers = `
SELECT count(*)
FROM control.users
WHERE account_id = $1
  AND state <> 'removed'`

func (r *accountUsersRepo) ListForAccount(ctx context.Context, accountID identity.AccountID, page persistence.UserPage) ([]identity.User, error) {
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list users for account %s: %w", accountID, err)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, listAccountUsers,
		string(accountID), string(page.After), page.State, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list users for account %s: %w", accountID, err)
	}
	defer func() { _ = rows.Close() }()

	users := make([]identity.User, 0, limit)
	for rows.Next() {
		user, scanErr := scanUser(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("postgres: list users for account %s: %w", accountID, scanErr)
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list users for account %s: %w", accountID, err)
	}
	return users, nil
}

func (r *accountUsersRepo) CountLiveForAccount(ctx context.Context, accountID identity.AccountID) (int, error) {
	var count int
	if err := r.store.Querier(ctx).QueryRowContext(ctx, countLiveAccountUsers, string(accountID)).Scan(&count); err != nil {
		return 0, fmt.Errorf("postgres: count live users for account %s: %w", accountID, err)
	}
	return count, nil
}

// ---------------------------------------------------------------------------
// api_keys
// ---------------------------------------------------------------------------

type accountAPIKeysRepo struct {
	store persistence.Store
}

// An ownership record and nothing else: the six columns are the whole
// readable row, and there is no digest column to select because the table
// has none. A credential a key was minted with exists once, in the mint's
// response, and no read here can return it.
//
// The keyset is the id, and it is UNIQUE, so the ordering is total. It is
// also very nearly mint order — ids are uuid v4 from identity's minting —
// so "oldest first" is what a reader sees and what the contract promises.
const listAccountAPIKeys = `
SELECT ` + apiKeyColumns + `
FROM control.api_keys
WHERE account_id = $1
  AND id > $2
ORDER BY id
LIMIT $3`

const countActiveAccountAPIKeys = `
SELECT count(*)
FROM control.api_keys
WHERE account_id = $1
  AND state = 'active'`

func (r *accountAPIKeysRepo) ListForAccount(ctx context.Context, accountID identity.AccountID, page persistence.APIKeyPage) ([]identity.APIKey, error) {
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list api keys for account %s: %w", accountID, err)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, listAccountAPIKeys,
		string(accountID), string(page.After), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list api keys for account %s: %w", accountID, err)
	}
	defer func() { _ = rows.Close() }()

	keys := make([]identity.APIKey, 0, limit)
	for rows.Next() {
		key, scanErr := scanAPIKey(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("postgres: list api keys for account %s: %w", accountID, scanErr)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list api keys for account %s: %w", accountID, err)
	}
	return keys, nil
}

func (r *accountAPIKeysRepo) CountActiveForAccount(ctx context.Context, accountID identity.AccountID) (int, error) {
	var count int
	if err := r.store.Querier(ctx).QueryRowContext(ctx, countActiveAccountAPIKeys, string(accountID)).Scan(&count); err != nil {
		return 0, fmt.Errorf("postgres: count active api keys for account %s: %w", accountID, err)
	}
	return count, nil
}

// ---------------------------------------------------------------------------
// the row scanners, shared with ByID above so the two cannot drift.
// ---------------------------------------------------------------------------

// rowScanner is database/sql's own shape for "a row being read": both
// *sql.Row and *sql.Rows satisfy it, which is what lets one scanner serve
// the single-row lookups and the list pages. It is spelled here rather than
// taken from a package so the port's vocabulary stays the standard
// library's.
type rowScanner interface {
	Scan(dest ...any) error
}

// userColumns and apiKeyColumns are the two projections, named once because
// a column list written twice is a column list that will differ. Both are the
// full readable row; neither invents a column, and neither omits one the
// contract declares.
const (
	userColumns   = `id, account_id, email, state, created_at, updated_at`
	apiKeyColumns = `id, account_id, created_by, display_name, prefix, state, created_at, updated_at, revoked_at`
)

// scanUser reads one users row. The state is read as text and narrowed to
// the domain's own type afterwards, because the schema's CHECK is the
// grammar and the domain's constants are the same three strings — a row the
// schema accepted is a row whose state the domain knows.
func scanUser(row rowScanner) (identity.User, error) {
	var user identity.User
	var state string
	if err := row.Scan(&user.ID, &user.AccountID, &user.Email, &state, &user.CreatedAt, &user.UpdatedAt); err != nil {
		return identity.User{}, err
	}
	user.State = identity.UserState(state)
	return user, nil
}

// scanAPIKey reads one api_keys row, including the nullable created_by and
// revoked_at. Both are scanned into their own locals and assigned only when
// the database says so, because a NULL scanned straight into the domain's
// zero value and a NULL scanned into a pointer are the same fact and only
// one of them is honest about "not set".
func scanAPIKey(row rowScanner) (identity.APIKey, error) {
	var key identity.APIKey
	var state string
	var createdBy sql.NullString
	var revokedAt sql.NullTime
	if err := row.Scan(&key.ID, &key.AccountID, &createdBy, &key.DisplayName, &key.Prefix,
		&state, &key.CreatedAt, &key.UpdatedAt, &revokedAt); err != nil {
		return identity.APIKey{}, err
	}
	key.State = identity.APIKeyState(state)
	if createdBy.Valid {
		key.CreatedBy = identity.UserID(createdBy.String)
	}
	if revokedAt.Valid {
		finish := revokedAt.Time
		key.RevokedAt = &finish
	}
	return key, nil
}
