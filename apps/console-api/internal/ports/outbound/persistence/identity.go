package persistence

import (
	"context"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
)

// The identity repositories, in the store's vocabulary.
//
// Identity is the Control Plane's ownership root (ADR 0001): the accounts
// everything belongs to, the users who act for them, and the API keys minted
// under them. All three tables live in the `control` database and nowhere
// else — that is the whole point of the two-record API-key model (ADR 0006
// §8): this port persists the ownership record, and the Data Plane's
// credential record is written only by the Data Plane. The digest is the one
// deliberate qualification since the projection (ADR 0007): it now transits
// and rests in this database's projection tables — the change log and the
// materialized mirror this package's ProjectionLog reads — and nowhere in the
// ownership records these interfaces persist. The ownership row keeps no
// digest column, and nothing behind these ports can reach the digest except
// through the projection port that exists to carry it.
//
// Sessions sits beside those three rather than inside them: it is the
// console's browser credential, it carries a token digest where the
// ownership records carry none, and it is in the same database by the same
// argument — the Control Plane is the authority for who may act.
//
// Four rules the signatures below carry on purpose:
//
//   - Aggregates go in whole and come out whole. Create takes the domain
//     aggregate, ByID returns one; there is no partial patch method, because
//     the domain's transitions are total — a state change is a new state, not
//     a delta. The adapter translates, it does not reinterpret.
//   - State changes travel as compare-and-swap. TransitionState and Revoke
//     take the state the caller read and only apply when the row still shows
//     it, so two concurrent transitions converge on the domain's rules — the
//     loser re-reads through the application layer instead of overwriting a
//     state it never saw. The database constraint set is the final guard;
//     this is the concurrency discipline that keeps it from being needed.
//   - Deletes do not exist. Every lifecycle exit is a state, and the rows
//     are history the Control Plane is the authority for.
//   - A lookup is keyed or it is not offered. ByAccountAndEmail is keyed on
//     the pair a partial unique index makes total; ByTokenHash is keyed on
//     the digest. Neither may be answered with a first match, and the
//     docblocks below say what each one would get wrong if it were.

// Accounts persists the account aggregate — the ownership root.
type Accounts interface {
	// Create inserts a new account in one unit of work with its caller's.
	// The row must be in its birth state (active); the domain constructs
	// every new account that way. accounts_state_valid is a membership
	// guard, not a birth guard: suspended and closed must remain storable
	// for their later states. Whether a stored account may authenticate is
	// a separate runtime verdict, made by VerifyCredential's state switch.
	Create(ctx context.Context, account identity.Account) error

	// ByID returns the account with id, or ErrNotFound.
	ByID(ctx context.Context, id identity.AccountID) (identity.Account, error)

	// TransitionState applies the active → suspended → closed machine's one
	// move: it sets the state to `to` only while the row still shows `from`,
	// stamps updated_at with the caller's instant, and reports whether the
	// move happened. A false return means someone else moved the row first;
	// the caller re-reads. The schema's state check admits every lifecycle
	// state so transitions can be persisted; it neither polices the move
	// nor decides whether a stored state may authenticate. The domain owns
	// the former, VerifyCredential's switch the latter.
	TransitionState(ctx context.Context, id identity.AccountID, from, to identity.AccountState, updatedAt time.Time) (bool, error)
}

// Users persists the console identity aggregate. Uniqueness is the schema's:
// (account_id, email) is unique across live rows — invited or active — so a
// removed user's address may be invited again, and Create surfaces the
// collision as ErrUserEmailTaken rather than a raw constraint error.
type Users interface {
	// Create inserts a new user in its birth state (invited).
	Create(ctx context.Context, user identity.User) error

	// ByID returns the user with id, or ErrNotFound.
	ByID(ctx context.Context, id identity.UserID) (identity.User, error)

	// TransitionState is the invited → active → removed machine's one move,
	// compare-and-swapped exactly as Accounts.TransitionState is.
	TransitionState(ctx context.Context, id identity.UserID, from, to identity.UserState, updatedAt time.Time) (bool, error)

	// ByAccountAndEmail returns the LIVE user for an (account, email) pair,
	// or ErrNotFound. This is ADR 0008 §1's sign-in resolution and §3's
	// lookup rule, and both halves of that rule are load-bearing.
	//
	// It filters live states — `state <> 'removed'` — so a removed row
	// resolves to nothing, ever. Removal is terminal, the domain refuses
	// to resurrect the identity, and a removed row's only remaining job is
	// history; handing one back as somebody's principal would be the
	// domain contradicting itself through its own read path. The live
	// vocabulary is `invited` and `active`, and an `invited` row IS in
	// scope here: resolution says the invitation's own redemption — the
	// row is found, so the lookup is right — while what that row may
	// authenticate is decided above authentication, not here.
	//
	// It is keyed, so it is total: zero rows or exactly one, because the
	// partial unique index `users_account_live_email_key` on
	// (account_id, email) WHERE state <> 'removed' says so at the engine
	// level. That is why this method may never become a first-match. A
	// `LIMIT 1`, an `ORDER BY` tiebreak or a "first row" would be a
	// choice the engine never had to offer, and the case where it would
	// choose wrong is the one ADR 0008 §Context-2 exists to name: a
	// removed row and the live user who later re-invited its address both
	// match an unfiltered `WHERE account_id AND email`, and a bare first
	// match can resolve the tombstone over the live user. An implementation
	// that cannot prove the filter is in the statement has not implemented
	// this method.
	//
	// The account's state is deliberately NOT part of this predicate.
	// Liveness is the user row's own lifecycle question; whether the
	// account may act is the post-credential verdict, and folding account
	// state in here would leak it before any credential was checked.
	ByAccountAndEmail(ctx context.Context, accountID identity.AccountID, email string) (identity.User, error)
}

// APIKeys persists the API key's ownership record — display metadata,
// ownership edges and lifecycle. It deliberately has no column for anything
// secret: the plaintext exists only inside the mint call, and the digest is
// the Data Plane credential record's column (ADR 0006 §8).
type APIKeys interface {
	// Create inserts a new key's ownership record in its birth state
	// (active, revoked_at null). The row's state/revoked_at pairing is
	// enforced by the schema, so an inconsistent record cannot exist. That
	// is consistency, not liveness: the state check admits revoked rows, and
	// VerifyCredential's state switch refuses them at authentication.
	Create(ctx context.Context, key identity.APIKey) error

	// ByID returns the key's ownership record, or ErrNotFound.
	ByID(ctx context.Context, id identity.APIKeyID) (identity.APIKey, error)

	// Revoke applies the one-way active → revoked move, compare-and-swapped
	// like the other transitions: it flips the state and stamps both
	// revoked_at and updated_at only while the row still shows active.
	// There is no un-revoke method; the machine's terminal state is the
	// port's too.
	Revoke(ctx context.Context, id identity.APIKeyID, revokedAt time.Time) (bool, error)
}

// Sessions persists the session row — the console's browser credential's
// stored half. It lives in the same `control` database as the ownership
// records above, and it holds a DIGEST where the API key's record holds
// nothing: the row a disclosure yields is a hash nobody can present.
//
// It is a distinct port from Users and APIKeys rather than a member of
// either, because its lifecycle is not either's. A user is invited,
// active, removed; a key is active, revoked; a session is issued, touched,
// expired, revoked — and it is never re-instated, so the one-way
// transitions above remain its transitions and a session row is never
// edited in place to mean something new.
type Sessions interface {
	// Create inserts a new session row. The aggregate carries the token's
	// SHA-256 digest and never the token itself; there is no column for
	// the plaintext, and a row that did have one would make a database
	// disclosure a credential disclosure. The row is born un-revoked with
	// its own created/expires/last-seen stamps, all UTC, and the schema
	// checks the pairing that matters — a revoked row has a revoked_at.
	Create(ctx context.Context, session identity.Session) error

	// ByTokenHash returns the session whose stored digest equals digest, or
	// ErrNotFound. It is keyed on the digest rather than on the token's id
	// segment so that the row is found by the secret itself: an adapter
	// that resolved the id first and then compared digests in Go would
	// branch on which sessions exist, and a lookup that answers "yes" for
	// a known id and "no" for an unknown one is an id-existence oracle at
	// the one place the caller cannot see it.
	//
	// The digest is supplied as its fixed-width [32]byte type and compared
	// by the adapter in constant time, or the row is found by an equality
	// predicate the engine evaluates — either way the caller must not be
	// able to tell a stored digest from an absent row by anything but the
	// ErrNotFound sentinel.
	//
	// It does NOT filter on expiry or revocation. A revoked row resolves
	// and an expired row resolves: both are facts about a session that
	// happened, and a reader that hid them would make sign-out look like
	// a session that never existed — which is precisely the confusion that
	// makes "did my sign-out work?" unanswerable. The liveness verdict is
	// the aggregate's Expired/Revoked/Active, applied by the caller that
	// holds the row.
	ByTokenHash(ctx context.Context, digest identity.Digest) (identity.Session, error)

	// Revoke stamps revoked_at on the row, compare-and-swapped like every
	// other transition here: it applies only while the row is still
	// un-revoked, so two concurrent sign-outs converge instead of
	// fighting, and a false return means someone else already revoked it
	// — which is a success for a caller whose goal was "no longer
	// accepted", not a failure. There is no un-revoke: the aggregate's
	// terminal state is the port's too.
	//
	// This is immediate. It is not "valid until it expires", and the
	// difference is the whole reason a session is an opaque server-side
	// row rather than a self-validating signed token.
	Revoke(ctx context.Context, id identity.SessionID, revokedAt time.Time) (bool, error)

	// Touch stamps last_seen_at for a still-live row, so an operator can
	// see which sessions are actually in use. It is guarded the same way
	// Revoke is — a row that is revoked or expired is not touched, and a
	// false return says the row was not live — because a last-seen stamp
	// that moves on a dead session is a fact the aggregate forbids.
	Touch(ctx context.Context, id identity.SessionID, seenAt time.Time) (bool, error)
}
