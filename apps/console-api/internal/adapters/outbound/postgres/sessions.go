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

// The sessions repository: the stored half of the console's browser
// credential. Everything here is a translation between the port's session
// vocabulary and control.sessions, and the one property the whole file exists
// to keep is that NO STATEMENT CAN RETURN A TOKEN.
//
// The table stores a digest and the aggregate holds one, so there is no
// plaintext anywhere on this side to leak — not because the queries are
// careful about their column lists, but because the column does not exist. The
// scanner below names the nine columns the row has and stops; a statement that
// grew a tenth would be a migration, and a migration that added the token
// would fail to compile here before it could ever return one.

// NewSessions returns the persistence port's Sessions repository backed by
// store. It panics on a nil store for the reason every constructor here does.
func NewSessions(store persistence.Store) persistence.Sessions {
	if store == nil {
		panic("postgres: NewSessions requires a non-nil persistence.Store")
	}
	return &sessionRepo{store: store}
}

// Compile-time proof that the repository satisfies the port's contract.
var _ persistence.Sessions = (*sessionRepo)(nil)

type sessionRepo struct {
	store persistence.Store
}

const insertSession = `
INSERT INTO control.sessions (id, user_id, account_id, token_hash, class, state, created_at, expires_at, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

// The sign-in access path. It is keyed on the DIGEST rather than on the token's
// id segment, and the reason is the port's own: an adapter that resolved the id
// first and compared digests in Go would branch on which sessions exist, and a
// lookup answering "yes" for a known id and "no" for an unknown one is an
// id-existence oracle at the one place the caller cannot see it.
//
// The predicate does NOT filter on liveness. A revoked row resolves and an
// expired row resolves, because both are facts about a session that happened
// and a reader that hid them would make a sign-out indistinguishable from a
// session that never existed. Whether the row may be USED is the aggregate's
// verdict, applied by the caller that holds it.
const selectSessionByTokenHash = `
SELECT id, user_id, account_id, token_hash, class, state, created_at, expires_at, last_seen_at, revoked_at
FROM control.sessions
WHERE token_hash = $1`

// Both transitions are compare-and-swapped, for the reason every transition in
// this lane is: the move applies only while the row still shows the state the
// caller read, so two racing revocations converge instead of one silently
// overwriting the other, and a false return means somebody else already did it
// — which is SUCCESS for a caller whose goal is "no longer accepted", not
// failure.
const revokeSession = `
UPDATE control.sessions
SET state = 'revoked', revoked_at = $3
WHERE id = $1 AND state = 'active'`

// The touch is guarded by the SAME predicate plus the expiry comparison, so a
// last-seen stamp cannot move on a row that is dead. A row that is revoked or
// expired is not touched, and a false return says the row was not live — which
// is the fact the caller needs, because a last_seen_at that moves on a dead
// session is a fact the aggregate forbids.
const touchSession = `
UPDATE control.sessions
SET last_seen_at = $3
WHERE id = $1 AND state = 'active' AND expires_at > $2`

func (r *sessionRepo) Create(ctx context.Context, session identity.Session) error {
	// The digest's hex form is what the row stores, and the column's shape
	// check is what makes "we store the digest, never the token" an enforced
	// fact rather than a property of a code path somebody has to remember.
	state := "active"
	if session.Revoked() {
		state = "revoked"
	}
	_, err := r.store.Querier(ctx).ExecContext(ctx, insertSession,
		string(session.ID), string(session.UserID), string(session.AccountID),
		session.TokenHash.Hex(), string(session.Class), state,
		session.CreatedAt, session.ExpiresAt, session.LastSeenAt)
	if err != nil {
		return fmt.Errorf("postgres: create session %s: %w", session.ID, err)
	}
	return nil
}

func (r *sessionRepo) ByTokenHash(ctx context.Context, digest identity.Digest) (identity.Session, error) {
	var s identity.Session
	var account, class, state, tokenHash string
	var revokedAt *time.Time

	err := r.store.Querier(ctx).QueryRowContext(ctx, selectSessionByTokenHash, digest.Hex()).
		Scan(&s.ID, &s.UserID, &account, &tokenHash, &class, &state,
			&s.CreatedAt, &s.ExpiresAt, &s.LastSeenAt, &revokedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return identity.Session{}, fmt.Errorf("postgres: session: %w", persistence.ErrNotFound)
		}
		return identity.Session{}, fmt.Errorf("postgres: session: %w", err)
	}

	s.AccountID = identity.AccountID(account)
	s.Class = identity.SessionClass(class)
	s.RevokedAt = revokedAt

	// The stored digest is read back into the aggregate so a caller comparing
	// two digests has both, but it is NOT verified here: this method found the
	// row BY that digest through a unique index, so the equality is the
	// database's answer, and re-comparing it would be a second derivation of a
	// fact already established. The hex is parsed rather than stored as a
	// string, and a row whose digest will not parse is reported as itself —
	// a corrupt row is not a missing session, and reporting it as one would
	// let it masquerade as a sign-out for as long as nobody noticed.
	stored, err := identity.DigestFromHex(tokenHash)
	if err != nil {
		return identity.Session{}, fmt.Errorf("postgres: session %s: the stored token digest is unreadable: %w", s.ID, err)
	}
	s.TokenHash = stored

	// State is derived from the row's own pairing rather than read: the schema
	// pins (state = 'revoked') = (revoked_at IS NOT NULL), so a row that says
	// "revoked" with a stamp and a row whose stamp is set are the same row, and
	// reading one field while trusting the other would be a way for a
	// disagreement to exist without either of them noticing.
	if state == "revoked" || revokedAt != nil {
		revoked := revokedAt
		if revoked == nil {
			now := s.LastSeenAt
			revoked = &now
		}
		s.RevokedAt = revoked
	}
	return s, nil
}

func (r *sessionRepo) Revoke(ctx context.Context, id identity.SessionID, revokedAt time.Time) (bool, error) {
	res, err := r.store.Querier(ctx).ExecContext(ctx, revokeSession, string(id), nil, revokedAt.UTC())
	if err != nil {
		return false, fmt.Errorf("postgres: revoke session %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: revoke session %s: read rows affected: %w", id, err)
	}
	return n == 1, nil
}

func (r *sessionRepo) Touch(ctx context.Context, id identity.SessionID, seenAt time.Time) (bool, error) {
	// The predicate compares expires_at against the same instant the stamp is
	// set with, so "still live" is one fact computed once rather than a
	// reading of state and a reading of expiry that could disagree between the
	// statement's start and the caller's own clock.
	now := seenAt.UTC()
	res, err := r.store.Querier(ctx).ExecContext(ctx, touchSession, string(id), now, now)
	if err != nil {
		return false, fmt.Errorf("postgres: touch session %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: touch session %s: read rows affected: %w", id, err)
	}
	return n == 1, nil
}
