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
// Three rules the signatures below carry on purpose:
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
