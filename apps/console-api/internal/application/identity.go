package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The identity use cases: the Control Plane's ownership root, driven through
// the persistence port and the identity domain (ADR 0001). Every write below
// is one unit of work, and every lifecycle move is a read, a domain
// transition, and a compare-and-swap — the CAS is what makes concurrent
// transitions converge on the domain's rules instead of one silently
// overwriting a state its caller never saw: the loser of a swap re-reads and
// re-applies from the state that actually exists.
//
// Every write that changes something the Data Plane's credential mirror
// holds — accounts, and the api-key lifecycle — also records the projection
// entry for it, inside the same unit of work (ADR 0007). The recording is
// the last step of the write, after the authority row has moved: the entry
// describes a change that happened, not one that is about to, and a
// recording that fails rolls the authority write back with it, so there is
// no state in which the Control Plane decided something its projection did
// not hear. Only the writes that actually moved a row record: the loser of
// a compare-and-swap describes nothing, because the winner's entry already
// describes the row as it now is.
//
// Nothing constructs this type yet, and no HTTP surface reaches it: B2 is the
// foundation phase, and the endpoints that will call these methods are later
// phases' contract changes. That is also why no method here maps failures to
// application.Error categories — the transport that owns that mapping does not
// exist yet, and inventing its statuses ahead of it would be a shape guessed
// twice. Callers match the domain's sentinels (errors.Is) and the persistence
// port's ErrNotFound.

// ErrTransitionContended reports that a lifecycle move kept losing its
// compare-and-swap for casMaxAttempts straight attempts. It is not a domain
// rejection — the domain never refused anything — it is this use case giving
// up on a row that is being transitioned faster than it can re-read, which
// with this port's single-statement swaps means an operator, a retry storm, or
// a bug is racing the account, and surfacing that beats looping forever.
var ErrTransitionContended = errors.New("application: concurrent transitions did not settle")

// casMaxAttempts bounds the read–apply–swap loop. Eight is not a tuned number:
// a swap is one statement, a lost swap means someone else completed a move,
// and every move in this domain is one-way — so the loop's honest terminations
// are "the state I wanted is already there" (a no-op on re-read) and, only
// under pathological contention, this error.
const casMaxAttempts = 8

// CredentialSource is the recorded half of an API-key verification: what the
// system knows about the key a token claims to be.
//
// It is declared here — at the use case that consumes it — because its
// production backing is the Data Plane's credential projection, and that
// projection is a later phase's designed change (ADR 0006 §8): its transport,
// staleness contract and recovery story get decided when they get built, and
// a port invented for them now would be a shape guessed twice. What B2 owes
// the pipeline is everything downstream of the lookup — the token grammar,
// the fail-closed parse, the constant-time digest comparison, the state
// policy — and the domain's VerifyCredential is exactly that, verbatim
// mirrorable by the Data Plane's hot path.
//
// An implementation returns the credential and true, or false when it holds
// no record for keyID — a miss, not a failure. A miss is not an error and
// never an empty struct with a nil flag: the use case runs the verification
// pipeline against the zero Credential on the miss path, so implementation
// side, the only difference between a hit and a miss is the lookup itself.
// Keeping that lookup's cost flat across hits and misses is therefore this
// interface's implementation contract — a backing store whose misses measure
// cheaper is a key-id existence oracle waiting to be timed.
type CredentialSource interface {
	APIKeyCredential(ctx context.Context, keyID identity.APIKeyID) (identity.Credential, bool, error)
}

// Identity is the identity foundation's use cases: accounts, users, API-key
// ownership, and the API-key verification model.
type Identity struct {
	store         persistence.Store
	accounts      persistence.Accounts
	users         persistence.Users
	keys          persistence.APIKeys
	projection    persistence.ProjectionChangeRecorder
	projectionLog persistence.ProjectionLog
}

// NewIdentity builds the identity use cases around the ports they need. It
// panics on a nil port for the reason NewFactIngestion panics: a port this
// use case was promised and did not get is a wiring defect, and the middle of
// a mint — after a transaction is open and a secret generated — is a strictly
// worse place to learn about it. The projection recorder is fifth and no
// less required than the rest: a mint without its projection entry is a
// credential the runtime never learns about, discovered only when the key
// fails to authenticate — the worst possible place to learn about it. The
// projection log is sixth because revocation reads the mirror to describe
// the credential it is revoking — the digest exists in this plane's database
// only inside the projection pipeline's own tables, and the mirror row is
// where a read finds it.
func NewIdentity(store persistence.Store, accounts persistence.Accounts, users persistence.Users, keys persistence.APIKeys, projectionRecorder persistence.ProjectionChangeRecorder, projectionLog persistence.ProjectionLog) *Identity {
	switch {
	case store == nil:
		panic("application: NewIdentity requires a store")
	case accounts == nil:
		panic("application: NewIdentity requires an accounts repository")
	case users == nil:
		panic("application: NewIdentity requires a users repository")
	case keys == nil:
		panic("application: NewIdentity requires an api keys repository")
	case projectionRecorder == nil:
		panic("application: NewIdentity requires a projection change recorder")
	case projectionLog == nil:
		panic("application: NewIdentity requires a projection log")
	}
	return &Identity{store: store, accounts: accounts, users: users, keys: keys, projection: projectionRecorder, projectionLog: projectionLog}
}

// MintedKey is what minting hands back, exactly once. Token is the only copy
// of the secret anyone ever sees — the use case keeps none, stores none, and
// logs none — and Digest is the SHA-256 of that secret, ready for the
// credential record the projection phase will deliver to the Data Plane. The
// plaintext itself deliberately does not survive this call: after Token is
// returned, no code path in the Control Plane can reproduce it.
type MintedKey struct {
	Key identity.APIKey
	// Token is the full gw_<key-id>_<secret> credential. Shown once.
	Token string
	// Digest is the recorded form of Token's secret — not secret material
	// itself, and therefore the part that may be delivered onward.
	Digest identity.Digest
}

// String redacts: a mint result is exactly the value a debug line prints
// wholesale, and its Token field is the one copy of the credential anyone
// will ever hold. Every fmt verb that renders a value (%v, %+v, %s, %q)
// goes through here and shows the key, never the token.
func (k MintedKey) String() string {
	return fmt.Sprintf("minted key %s (token redacted)", k.Key.ID)
}

// GoString covers %#v with the same redaction String gives every other verb.
func (k MintedKey) GoString() string {
	return fmt.Sprintf("application.MintedKey{Key: %s, Token: redacted, Digest: %s}", k.Key.ID, k.Digest.Hex())
}

// MarshalJSON refuses. Serialisation is where redaction-by-Stringer stops
// working — encoding/json reads fields, not verbs — and a mint result in a
// JSON log, an audit file or a response body is a credential leak with a
// retention policy. Nothing may persist this value's Token; callers who need
// the record, not the credential, marshal MintedKey.Key.
func (k MintedKey) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("application: minted key %s refuses serialisation: the one-time token must not land in a log, file or wire format", k.Key.ID)
}

// CreateAccount opens an account: a fresh identity, a validated name, and the
// aggregate's birth state, inserted as one unit of work — the projection
// entry included, so the mirror learns the account in the same commit the
// authority does.
func (i *Identity) CreateAccount(ctx context.Context, name string) (identity.Account, error) {
	id, err := identity.NewAccountID()
	if err != nil {
		return identity.Account{}, fmt.Errorf("application: create account: %w", err)
	}
	account, err := identity.NewAccount(id, name, time.Now())
	if err != nil {
		return identity.Account{}, fmt.Errorf("application: create account: %w", err)
	}
	projected, err := projection.NewAccount(string(account.ID), projection.AccountState(account.State))
	if err != nil {
		return identity.Account{}, fmt.Errorf("application: create account %s: %w", account.ID, err)
	}
	if err := i.store.WithinTx(ctx, func(txCtx context.Context) error {
		if err := i.accounts.Create(txCtx, *account); err != nil {
			return err
		}
		// The authority row is in; the projection entry describes it. The
		// recorded instant is the account's own creation instant, so the
		// log's story and the row's timestamps are one story.
		return i.projection.RecordAccountChange(txCtx, account.CreatedAt, projected)
	}); err != nil {
		return identity.Account{}, fmt.Errorf("application: create account %s: %w", account.ID, err)
	}
	return *account, nil
}

// Account returns the account with id. A miss is the persistence port's
// ErrNotFound, wrapped with what was looked for.
func (i *Identity) Account(ctx context.Context, id identity.AccountID) (identity.Account, error) {
	account, err := i.accounts.ByID(ctx, id)
	if err != nil {
		return identity.Account{}, fmt.Errorf("application: read account %s: %w", id, err)
	}
	return account, nil
}

// SuspendAccount freezes an account: nothing new attaches to it, and its
// existing principals stop authenticating (the verification policy reads the
// account state). Already-suspended is a no-op; closed is a domain rejection.
func (i *Identity) SuspendAccount(ctx context.Context, id identity.AccountID) error {
	return i.transitionAccount(ctx, id, (*identity.Account).Suspend)
}

// ReinstateAccount lifts a suspension. Already-active is a no-op; closed is a
// domain rejection — closed is terminal, and no transition leaves it.
func (i *Identity) ReinstateAccount(ctx context.Context, id identity.AccountID) error {
	return i.transitionAccount(ctx, id, (*identity.Account).Reinstate)
}

// CloseAccount ends an account from any state. Already-closed is a no-op, so
// a retry after a success cannot fail.
func (i *Identity) CloseAccount(ctx context.Context, id identity.AccountID) error {
	return i.transitionAccount(ctx, id, (*identity.Account).Close)
}

// transitionAccount runs one lifecycle move as read → domain transition →
// compare-and-swap, retrying from the fresh state when the swap loses. The
// apply function is the aggregate's own method, so the domain — not this use
// case — is what refuses illegal moves and what decides a move was a no-op.
// The move that wins the swap records its projection entry in the same unit
// of work; a lost swap records nothing, because the winner's entry already
// describes the row as it now is.
func (i *Identity) transitionAccount(ctx context.Context, id identity.AccountID, apply func(*identity.Account, time.Time) error) error {
	return i.store.WithinTx(ctx, func(txCtx context.Context) error {
		for attempt := 0; attempt < casMaxAttempts; attempt++ {
			account, err := i.accounts.ByID(txCtx, id)
			if err != nil {
				return fmt.Errorf("application: read account %s: %w", id, err)
			}
			before := account.State
			if err := apply(&account, time.Now()); err != nil {
				return fmt.Errorf("application: transition account %s: %w", id, err)
			}
			if account.State == before {
				return nil // the domain ruled this a no-op; nothing to swap
			}
			applied, err := i.accounts.TransitionState(txCtx, id, before, account.State, account.UpdatedAt)
			if err != nil {
				return fmt.Errorf("application: transition account %s: %w", id, err)
			}
			if applied {
				projected, err := projection.NewAccount(string(account.ID), projection.AccountState(account.State))
				if err != nil {
					return fmt.Errorf("application: transition account %s: %w", id, err)
				}
				return i.projection.RecordAccountChange(txCtx, account.UpdatedAt, projected)
			}
			// Lost the swap: the row moved under us, and the next iteration
			// re-reads and re-applies from the state that actually exists.
		}
		return fmt.Errorf("application: transition account %s: %w after %d attempts", id, ErrTransitionContended, casMaxAttempts)
	})
}

// CreateUser invites a console identity under an account. The account must
// exist and be active — suspension freezes the account, and growth stops with
// it — and the (account, email) uniqueness rule surfaces as the domain's
// ErrUserEmailTaken. The active check is a read without a lock, the same
// documented residual as MintAPIKey's: an invitation can land in the instant
// the account suspends, and the verification policy's account-state read is
// what keeps such a row inert.
func (i *Identity) CreateUser(ctx context.Context, accountID identity.AccountID, email string) (identity.User, error) {
	id, err := identity.NewUserID()
	if err != nil {
		return identity.User{}, fmt.Errorf("application: create user: %w", err)
	}
	user, err := identity.NewUser(id, accountID, email, time.Now())
	if err != nil {
		return identity.User{}, fmt.Errorf("application: create user: %w", err)
	}
	if err := i.store.WithinTx(ctx, func(txCtx context.Context) error {
		account, err := i.accounts.ByID(txCtx, accountID)
		if err != nil {
			return fmt.Errorf("application: create user: read account: %w", err)
		}
		if account.State != identity.AccountActive {
			return fmt.Errorf("application: create user: %w (%s is %s)", identity.ErrAccountNotActive, accountID, account.State)
		}
		return i.users.Create(txCtx, *user)
	}); err != nil {
		return identity.User{}, fmt.Errorf("application: create user %s: %w", user.ID, err)
	}
	return *user, nil
}

// User returns the user with id.
func (i *Identity) User(ctx context.Context, id identity.UserID) (identity.User, error) {
	user, err := i.users.ByID(ctx, id)
	if err != nil {
		return identity.User{}, fmt.Errorf("application: read user %s: %w", id, err)
	}
	return user, nil
}

// ActivateUser completes an invitation. Already-active is a no-op; removed is
// a domain rejection.
func (i *Identity) ActivateUser(ctx context.Context, id identity.UserID) error {
	return i.transitionUser(ctx, id, (*identity.User).Activate)
}

// RemoveUser retires a console identity from any state. Already-removed is a
// no-op; the row stays as history, and the live-email index frees the address.
func (i *Identity) RemoveUser(ctx context.Context, id identity.UserID) error {
	return i.transitionUser(ctx, id, (*identity.User).Remove)
}

// transitionUser is transitionAccount's user-shaped twin; the aggregation
// rules differ per aggregate, which is why there are two rather than one
// generic loop over interfaces.
func (i *Identity) transitionUser(ctx context.Context, id identity.UserID, apply func(*identity.User, time.Time) error) error {
	return i.store.WithinTx(ctx, func(txCtx context.Context) error {
		for attempt := 0; attempt < casMaxAttempts; attempt++ {
			user, err := i.users.ByID(txCtx, id)
			if err != nil {
				return fmt.Errorf("application: read user %s: %w", id, err)
			}
			before := user.State
			if err := apply(&user, time.Now()); err != nil {
				return fmt.Errorf("application: transition user %s: %w", id, err)
			}
			if user.State == before {
				return nil
			}
			applied, err := i.users.TransitionState(txCtx, id, before, user.State, user.UpdatedAt)
			if err != nil {
				return fmt.Errorf("application: transition user %s: %w", id, err)
			}
			if applied {
				return nil
			}
		}
		return fmt.Errorf("application: transition user %s: %w after %d attempts", id, ErrTransitionContended, casMaxAttempts)
	})
}

// MintAPIKey creates an API key and its credential in one unit of work: the
// ownership record lands in the control database, and the one-time token —
// the only copy of the secret that will ever exist — is returned to the
// caller. The account must exist and be active; a creating user must exist,
// belong to that account and not be removed, because the ownership graph is
// a tree and a forged cross-account creator would bend it.
//
// The mint is deliberately the Control Plane's last sight of the plaintext.
// The digest recorded alongside the ownership row is the credential the
// projection delivers — the one form of the secret that exists outside this
// call, hex-encoded, harmless to store and to ship, useless for presenting
// the key (ADR 0006 §8, as amended by ADR 0007). It is recorded inside the
// same transaction as the ownership row: a mint whose projection entry
// failed rolls back whole, so no ownership record can exist whose credential
// is missing from the pipeline the runtime verifies against.
//
// One residual is documented rather than locked away: the account-active and
// creator checks read rows without locks, so a suspension (or a removal) can
// commit between a check and the insert, and the key lands in the same
// instant its account froze. The window is one transaction wide, and the
// invariant that matters is enforced downstream of it — verification
// consults the account's state, so nothing minted into a suspended account
// authenticates. Serialising mints against suspensions with row locks is a
// deliberate escalation for the phase that gives suspension a hot-path
// consequence, not a default this use case reaches for.
func (i *Identity) MintAPIKey(ctx context.Context, accountID identity.AccountID, createdBy identity.UserID, displayName string) (MintedKey, error) {
	secret, err := identity.GenerateSecret()
	if err != nil {
		return MintedKey{}, fmt.Errorf("application: mint api key: %w", err)
	}
	id, err := identity.NewAPIKeyID()
	if err != nil {
		return MintedKey{}, fmt.Errorf("application: mint api key: %w", err)
	}
	token, err := identity.FormatToken(id, secret)
	if err != nil {
		return MintedKey{}, fmt.Errorf("application: mint api key: %w", err)
	}
	key, err := identity.NewAPIKey(id, accountID, createdBy, displayName, time.Now())
	if err != nil {
		return MintedKey{}, fmt.Errorf("application: mint api key: %w", err)
	}

	if err := i.store.WithinTx(ctx, func(txCtx context.Context) error {
		account, err := i.accounts.ByID(txCtx, accountID)
		if err != nil {
			return fmt.Errorf("application: mint api key: read account: %w", err)
		}
		if account.State != identity.AccountActive {
			return fmt.Errorf("application: mint api key: %w (%s is %s)", identity.ErrAccountNotActive, accountID, account.State)
		}
		if createdBy != "" {
			creator, err := i.users.ByID(txCtx, createdBy)
			if err != nil {
				return fmt.Errorf("application: mint api key: read creator: %w", err)
			}
			if creator.AccountID != accountID {
				return fmt.Errorf("application: mint api key: creator %s: %w", createdBy, identity.ErrCreatorOutsideAccount)
			}
			if creator.State == identity.UserRemoved {
				return fmt.Errorf("application: mint api key: creator %s: %w", createdBy, identity.ErrCreatorRemoved)
			}
		}
		if err := i.keys.Create(txCtx, *key); err != nil {
			return err
		}
		// The credential enters the projection pipeline in the same commit
		// as its ownership record, carrying the digest and nothing else;
		// recorded_at is the key's own creation instant. Built here, at the
		// point of recording, so the projection's grammar is the write's
		// last question and never outranks the authority checks above it.
		digest, err := projection.NewDigest(secret.Digest().Hex())
		if err != nil {
			return fmt.Errorf("application: mint api key %s: %w", key.ID, err)
		}
		projected, err := projection.NewCredential(string(key.ID), string(key.AccountID), string(digest), projection.CredentialActive, nil)
		if err != nil {
			return fmt.Errorf("application: mint api key %s: %w", key.ID, err)
		}
		return i.projection.RecordCredentialChange(txCtx, key.CreatedAt, projected)
	}); err != nil {
		return MintedKey{}, fmt.Errorf("application: mint api key %s: %w", key.ID, err)
	}
	return MintedKey{Key: *key, Token: token, Digest: secret.Digest()}, nil
}

// APIKey returns an API key's ownership record.
func (i *Identity) APIKey(ctx context.Context, id identity.APIKeyID) (identity.APIKey, error) {
	key, err := i.keys.ByID(ctx, id)
	if err != nil {
		return identity.APIKey{}, fmt.Errorf("application: read api key %s: %w", id, err)
	}
	return key, nil
}

// RevokeAPIKey retires a key. Revoking an already-revoked key is a no-op
// confirmed by a read, not an error, so a retried revocation — or two racing
// ones — converge instead of fighting. There is no un-revoke on this use
// case, in the domain, or in the schema.
//
// The revocation is also the projection's deterministic revoke story
// (ADR 0007): the authority row and the revoked-credential change commit in
// one unit of work, stamped with the same instant, so the mirror learns the
// credential is dead exactly when this transaction makes it dead. Only the
// swap that won records: a revocation that lost its race describes a change
// the winner's entry already carries, and recording it too would spend a
// revision on nothing.
func (i *Identity) RevokeAPIKey(ctx context.Context, id identity.APIKeyID) error {
	at := time.Now()
	return i.store.WithinTx(ctx, func(txCtx context.Context) error {
		key, err := i.keys.ByID(txCtx, id)
		if err != nil {
			return fmt.Errorf("application: revoke api key %s: %w", id, err)
		}
		if key.State == identity.APIKeyRevoked {
			return nil
		}
		applied, err := i.keys.Revoke(txCtx, id, at)
		if err != nil {
			return fmt.Errorf("application: revoke api key %s: %w", id, err)
		}
		if applied {
			// The revoked credential's log entry carries the row's whole
			// state, and the digest lives only inside the projection
			// pipeline's own tables — the ownership record is digest-free by
			// the two-record model — so the mirror row is where a read finds
			// it. The read runs in this same transaction, so the row it
			// reads is the one this transaction is about to rewrite.
			projected, err := i.projectionLog.Credential(txCtx, string(key.ID))
			if errors.Is(err, persistence.ErrNotFound) {
				// The key predates the projection foundation: its digest
				// never existed in this database, so no mirror anywhere —
				// here or on the Data Plane — holds the credential, and
				// there is nothing to project. The revocation is still
				// real: it is on the ownership record above, which is the
				// authority (ADR 0007 records this recovery).
				return nil
			}
			if err != nil {
				return fmt.Errorf("application: revoke api key %s: %w", id, err)
			}
			revoked, err := projection.NewCredential(string(key.ID), string(key.AccountID), string(projected.Digest), projection.CredentialRevoked, &at)
			if err != nil {
				return fmt.Errorf("application: revoke api key %s: %w", id, err)
			}
			return i.projection.RecordCredentialChange(txCtx, at, revoked)
		}
		// Lost the swap: another revocation completed first. Confirm the row
		// really is revoked — it is the only state a lost revoke swap can
		// mean, and a read is cheaper than trusting that arithmetic. This
		// path records nothing: the winner's own transaction carried the
		// change, and a second entry would be a revision spent on a state
		// the mirror already holds.
		key, err = i.keys.ByID(txCtx, id)
		if err != nil {
			return fmt.Errorf("application: revoke api key %s: %w", id, err)
		}
		if key.State != identity.APIKeyRevoked {
			return fmt.Errorf("application: revoke api key %s: %w: row is %q after a lost swap", id, ErrTransitionContended, key.State)
		}
		return nil
	})
}

// VerifyAPIKey turns a presented token into a principal, or into one of the
// domain's distinguishable rejections: ErrMalformedToken (fail closed, one
// sentinel for every malformed shape), ErrUnknownCredential (mismatch and
// missing record are the same sentinel, on paths that have done the same
// work), ErrKeyRevoked, ErrAccountSuspended, ErrAccountClosed.
//
// This is the verification MODEL, not a runtime authentication flow: the
// Data Plane's hot path authenticates requests against its own projection
// with the Control Plane off the request path (ADR 0006 §4). What lands here
// is the policy and the seam it consumes — the same pipeline, later, fed by
// the projection's credential read.
func (i *Identity) VerifyAPIKey(ctx context.Context, presentedToken string, source CredentialSource) (identity.Principal, error) {
	keyID, secret, err := identity.ParseToken(presentedToken)
	if err != nil {
		return identity.Principal{}, fmt.Errorf("application: verify api key: %w", err)
	}
	credential, ok, err := source.APIKeyCredential(ctx, keyID)
	if err != nil {
		return identity.Principal{}, fmt.Errorf("application: verify api key: read credential: %w", err)
	}
	if !ok {
		// A record that does not exist verifies against the zero Credential:
		// the pipeline runs its own digest comparison — the burn that keeps
		// wall time from answering whether a key id exists — and both this
		// path and the mismatch path flow through the same code and return
		// the same sentinel with byte-identical text, because error text
		// that distinguishes them is a key-id existence oracle in exactly
		// the way a timing difference would be. The zero digest can only
		// match a presented secret hashing to 32 zero bytes, and even that
		// outcome fails closed on the unknown key state that follows.
		credential = identity.Credential{}
	}
	principal, err := identity.VerifyCredential(keyID, secret, credential)
	if err != nil {
		return identity.Principal{}, fmt.Errorf("application: verify api key: %w", err)
	}
	return principal, nil
}
