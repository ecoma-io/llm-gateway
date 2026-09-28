package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/inbound/http"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The four session operations, as the composition root implements them.
//
// It is HERE, beside consolereads.go and for the same reason: the import rule
// forbids the inbound surface from reaching internal/domain, and the seam
// deliberately speaks in plain strings so that the conversion of an aggregate
// into wire fields happens at the one place allowed to know about both. The
// seam's method set and its input and result types are all exported for
// precisely this implementor — a reader looking for where a session is
// resolved will not find it in the transport, and should not.
//
// What this file is NOT allowed to do is decide anything about authentication.
// It resolves, checks and records; the RULES are the domain's and the
// application's, and every refusal below is one of their sentinels passed
// through rather than a decision invented at this layer. The one judgement
// call this file does own is the uniformity of the sign-in refusal, and it
// makes it by DELETING information rather than by writing a rule: whatever
// caused the refusal, the same error leaves this function.

// sessionTTL is how long a session stays acceptable. It is a constant of the
// composition root rather than a field on config, because it is a decision
// about the credential's lifetime and every other plane-side value in this
// process is read from config: an operator who cannot read the number they
// would need to change is an operator who cannot reason about it. Eight hours
// covers a working day and refuses a credential left on a machine overnight.
const sessionTTL = 8 * time.Hour

// errSignInRefused is the ONE error every pre-credential sign-in failure
// produces: no such account, no such user, a removed user, an `invited` row
// with no credential, a wrong credential, and a suspended or closed account.
//
// Five or six causes, one answer, and the answer is a constant rather than a
// formatted message for a reason that is about the reader as much as the
// caller: this function's stack is the last place the distinction exists, and
// an error that interpolated its cause would be one refactor away from
// printing the account's existence. Nothing in the call chain above can
// recover it — the handler discards whatever it is given and writes its own
// sentence — so a message that varied here would vary nowhere, and a cause
// logged at the boundary would still have to be this same value.
//
// The wrapping is deliberate rather than absent: a caller that must log WHY a
// sign-in failed can still recover the cause with errors.Unwrap, so operators
// keep their diagnostics while clients and anyone timing the endpoint see one
// indistinguishable outcome. Returning the bare sentinel instead would throw
// that away for no gain — the handler is the boundary either way.
var errSignInRefused = errors.New("console-api: the account, email or credential was not accepted")

// sessionNotLive is what resolving a cookie that names no acceptable session
// produces: missing, expired and revoked are one answer, because to the client
// they are one fact and the client cannot act on the difference.
//
// It is NOT errSignInRefused, and the distinction is the point. That one is a
// 401 written for a human choosing a password; this one is a 404 on a
// resource the client did not name. Making them the same error would either
// tell a browser holding a stale cookie that its account or email was wrong —
// which is false and sends the user to re-enter a password they did not get
// wrong — or turn a sign-out into a sign-in failure.
var errSessionNotLive = errors.New("console-api: no live session")

// consoleSessions adapts the identity use cases and the session store to the
// http package's sessionUseCases seam.
//
// The stores are interfaces, not concrete repositories, because this is the
// composition root and the seam is the boundary: the concrete adapters were
// chosen above in main, and the alternative — taking postgres types here —
// would make this file decide which persistence it speaks, which is the one
// decision the rule about infra behind ports exists to keep out of the layers
// that merely use it.
type consoleSessions struct {
	identity   *application.Identity
	sessions   persistence.Sessions
	users      persistence.Users
	accounts   persistence.Accounts
	apiKeys    persistence.APIKeys
	clock      persistence.Clock
	sessionTTL time.Duration
}

// newConsoleSessions refuses a nil dependency rather than answering around it,
// for the reason every constructor in this module has: a half-wired session
// surface should fail where the stack says which screen is missing, not on a
// customer's first sign-in. The nil here would be a wiring mistake, and every
// method below dereferences one of these fields on the floor of a request
// goroutine.
func newConsoleSessions(
	identityUseCases *application.Identity,
	sessions persistence.Sessions,
	users persistence.Users,
	accounts persistence.Accounts,
	apiKeys persistence.APIKeys,
	clock persistence.Clock,
	ttl time.Duration,
) (http.SessionUseCases, error) {
	switch {
	case identityUseCases == nil:
		return nil, errConsoleSessionsUnwired
	case sessions == nil, users == nil, accounts == nil, apiKeys == nil, clock == nil:
		return nil, errConsoleSessionsUnwired
	case ttl <= 0:
		return nil, errConsoleSessionsUnwired
	}
	return consoleSessions{
		identity:   identityUseCases,
		sessions:   sessions,
		users:      users,
		accounts:   accounts,
		apiKeys:    apiKeys,
		clock:      clock,
		sessionTTL: ttl,
	}, nil
}

// errConsoleSessionsUnwired is what a nil dependency produces. It is named here
// rather than imported so the composition root's own refusal and the http
// package's are the same sentence a reader finds in one place.
var errConsoleSessionsUnwired = errors.New("console-api: the session use cases are required and one of their dependencies was nil or unusable")

// SignIn resolves an (account, email) pair over live rows, checks the
// credential, and returns the session it created together with the principal it
// belongs to.
//
// The ORDER is the design, not an implementation detail. Everything that can
// be refused without a credential is decided first, so that by the time the
// expensive derivation runs there is exactly one outcome left to reach it —
// and every refusal before it costs the same wall time as the one after, which
// is the property that keeps "no such email" from being measurable.
//
// The credential check is called with the ZERO digest when the row carries
// none, and that is the reason identity.VerifyPasswordCredential documents
// burning: a zero digest still performs a full derivation at the production
// work factor, so an `invited` row costs what a wrong password costs. Passing
// an empty digest straight back as a refusal would make "invited" the cheapest
// guess in the vocabulary and turn a submission-timing probe into a membership
// oracle for which addresses have credentials.
func (s consoleSessions) SignIn(ctx context.Context, in http.SignInInput) (http.SessionResult, error) {
	user, credential, err := s.users.CredentialByAccountAndEmail(ctx, identity.AccountID(in.AccountID), in.Email)
	if err != nil {
		// A miss and a storage failure are not the same thing, and pretending
		// they are would hide a broken database behind a sign-in refusal that
		// looks like a typo. Both still leave this function with ONE answer;
		// they are separated only so the operational cause survives to a log
		// line, which no client can reach.
		if errors.Is(err, persistence.ErrNotFound) {
			return http.SessionResult{}, errSignInRefused
		}
		return http.SessionResult{}, fmt.Errorf("%w: %v", errSignInRefused, err)
	}

	// The recorded digest, or the zero one. An `invited` row has no credential
	// and that is a legitimate state, not a corrupt row — so the absence flows
	// into the verifier and the verifier's burn makes it indistinguishable in
	// both time and result from a wrong password.
	recorded := identity.CredentialDigest{}
	if credential != nil {
		recorded = *credential
	}

	accepted, err := identity.VerifyPasswordCredential(in.Password, recorded)
	if err != nil {
		return http.SessionResult{}, fmt.Errorf("%w: the credential could not be checked: %v", errSignInRefused, err)
	}
	if !accepted {
		return http.SessionResult{}, errSignInRefused
	}

	// Post-credential, and only now: an `invited` row holds a credential but
	// has proved nothing, because an invitation is record-keeping and a
	// credential is a grant of access (ADR 0012 §2). The admission rule lives
	// here rather than as a CHECK on the column precisely so that it is one
	// predicate in one place — a schema rule would make every future answer to
	// "what may an invited user do" a migration over a live column.
	if user.State != identity.UserActive {
		return http.SessionResult{}, errSignInRefused
	}

	// The account's own state, last. A credential that verifies against a
	// suspended account is a credential that works, and the suspension is what
	// says it must not — so this is checked after the derivation deliberately,
	// to keep it on the uniform side of the boundary.
	account, err := s.accounts.ByID(ctx, user.AccountID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return http.SessionResult{}, errSignInRefused
		}
		return http.SessionResult{}, fmt.Errorf("%w: %v", errSignInRefused, err)
	}
	if account.State != identity.AccountActive {
		return http.SessionResult{}, errSignInRefused
	}

	now, err := s.clock.Now(ctx)
	if err != nil {
		return http.SessionResult{}, fmt.Errorf("console-api: sign in: read the clock: %w", err)
	}

	session, token, err := identity.NewSession(user.ID, user.AccountID, identity.SessionUser, s.sessionTTL, now)
	if err != nil {
		return http.SessionResult{}, fmt.Errorf("console-api: sign in: %w", err)
	}
	if err := s.sessions.Create(ctx, *session); err != nil {
		return http.SessionResult{}, fmt.Errorf("console-api: sign in: store the session: %w", err)
	}

	// This is the only copy of the token that will ever exist outside the
	// cookie, and it leaves here as a field the transport writes into a
	// Set-Cookie. It is not logged, not persisted, and not rendered into a
	// body — the response is the principal and nothing else.
	return http.SessionResult{
		Principal: principalFor(session, user),
		Token:     http.SessionTokenFromString(token),
		ExpiresAt: session.ExpiresAt,
	}, nil
}

// Session resolves a presented token to the principal it names, and touches
// the row so an operator can see which sessions are in use.
//
// The touch is best-effort by design and its failure is swallowed: it records
// an operational fact about usage, and a console that refused to render because
// a last-seen stamp could not be written would be a console whose availability
// depends on a column nobody reads. Everything load-bearing — is this token
// real, is it revoked, has it expired — has already been decided from the row
// that was read.
func (s consoleSessions) Session(ctx context.Context, token http.SessionToken) (http.SessionResult, error) {
	session, user, err := s.resolve(ctx, token)
	if err != nil {
		return http.SessionResult{}, err
	}

	now, err := s.clock.Now(ctx)
	if err == nil {
		// A false return means the row was not live — which resolve already
		// established it was — so there is nothing to do and nothing to say.
		// The stamp is an optimisation, not a gate, and a caller that treated
		// its failure as a refusal would be refusing a session it just proved
		// was live.
		_, _ = s.sessions.Touch(ctx, session.ID, now)
	}

	return http.SessionResult{
		Principal: principalFor(&session, user),
		Token:     token,
		ExpiresAt: session.ExpiresAt,
	}, nil
}

// SignOut ends the session a token names.
//
// It is IDEMPOTENT, and the reason is the client rather than the server: a
// browser that has already discarded the cookie must still get its 204, or the
// console's sign-out button reports a failure for work that was done. A token
// that does not parse, names no row, or names a row already revoked are all
// the same successful outcome — the goal, "this session is no longer accepted",
// holds in every one of them.
func (s consoleSessions) SignOut(ctx context.Context, token http.SessionToken) error {
	_, secret, err := parseSessionToken(token)
	if err != nil {
		// A cookie the server cannot parse is a cookie that identifies no
		// session, and no session is exactly what a sign-out leaves behind.
		return nil
	}

	session, err := s.sessions.ByTokenHash(ctx, secret.Digest())
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("console-api: sign out: read the session: %w", err)
	}

	now, err := s.clock.Now(ctx)
	if err != nil {
		return fmt.Errorf("console-api: sign out: read the clock: %w", err)
	}

	// A false return is success, not failure: it means somebody else already
	// revoked the row, which is the outcome this call exists to reach. There
	// is no un-revoke, and no second attempt that could do more.
	if _, err := s.sessions.Revoke(ctx, session.ID, now); err != nil {
		return fmt.Errorf("console-api: sign out: revoke: %w", err)
	}
	return nil
}

// MintAPIKey creates a key's ownership record for the principal's account.
//
// The account is the PRINCIPAL's and the creator is the session's own user —
// neither is a field on the request, and neither can be, which is the property
// that makes the mint's trust precondition a statement about the code rather
// than a review item. The checks themselves belong to application.MintAPIKey:
// it verifies the account is active and that the creator belongs to it, in the
// same transaction as the insert, and duplicating them here would be a second
// copy of a rule that must not drift from the one that enforces it.
func (s consoleSessions) MintAPIKey(ctx context.Context, in http.MintAPIKeyInput) (http.MintedAPIKeyResult, error) {
	if in.Principal.UserID == "" {
		return http.MintedAPIKeyResult{}, application.Unauthenticated("no user is signed in")
	}
	if in.Principal.AccountID == "" {
		return http.MintedAPIKeyResult{}, application.Unauthenticated("the session names no account")
	}

	minted, err := s.identity.MintAPIKey(
		ctx,
		identity.AccountID(in.Principal.AccountID),
		identity.UserID(in.Principal.UserID),
		in.DisplayName,
	)
	if err != nil {
		// Passed through rather than re-decided. application.MintAPIKey's
		// refusals are the application's to make; this layer's only job is not
		// to flatten them into a generic error, which would turn a suspended
		// account and a missing creator into the same 500 and hide both.
		return http.MintedAPIKeyResult{}, err
	}

	return http.MintedAPIKeyResult{
		Record: http.APIKeyRecord{
			ID:          string(minted.Key.ID),
			AccountID:   string(minted.Key.AccountID),
			CreatedBy:   string(minted.Key.CreatedBy),
			DisplayName: minted.Key.DisplayName,
			Prefix:      minted.Key.Prefix,
			State:       string(minted.Key.State),
			CreatedAt:   wireTimestamp(minted.Key.CreatedAt),
			UpdatedAt:   wireTimestamp(minted.Key.UpdatedAt),
			RevokedAt:   optionalTimestamp(minted.Key.RevokedAt),
		},
		Token: minted.Token,
	}, nil
}

// parseSessionToken turns a presented cookie into the id it names and the
// secret whose digest the row stores.
//
// The call goes through SessionToken.CookieValue rather than a string
// conversion, because CookieValue is the one named door onto a token's
// plaintext and this is a place that genuinely has to be on the other side of
// it: a digest has to be computed from the same bytes the browser will send
// back. The id segment is discarded — a session row is FOUND BY ITS DIGEST, not
// by the id its token claims, and resolving the id first would be the
// id-existence oracle persistence.Sessions.ByTokenHash's own documentation
// refuses to build.
func parseSessionToken(token http.SessionToken) (identity.SessionID, identity.Secret, error) {
	return identity.ParseSessionToken(token.CookieValue())
}

// resolve is the one place a presented token becomes a session, shared by
// Session and SignOut so the two can never disagree about what a token names.//
// It returns the user the session belongs to because the principal is built
// from the SESSION's columns and the USER's email, and reading the user twice
// — once here for the session and once in a caller for the principal — would
// be a second read that could answer differently from the first.
func (s consoleSessions) resolve(ctx context.Context, token http.SessionToken) (identity.Session, identity.User, error) {
	_, secret, err := parseSessionToken(token)
	if err != nil {
		return identity.Session{}, identity.User{}, errSessionNotLive
	}

	session, err := s.sessions.ByTokenHash(ctx, secret.Digest())
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return identity.Session{}, identity.User{}, errSessionNotLive
		}
		return identity.Session{}, identity.User{}, fmt.Errorf("console-api: resolve session: %w", err)
	}

	now, err := s.clock.Now(ctx)
	if err != nil {
		return identity.Session{}, identity.User{}, fmt.Errorf("console-api: resolve session: read the clock: %w", err)
	}

	// Expiry and revocation are the aggregate's verdict, applied by the caller
	// that holds the row — the port deliberately returns dead rows so a sign-out
	// can still find one. This is that caller. All three outcomes become the
	// same error because to a caller holding a cookie they are one fact.
	if !session.Active(now) {
		return identity.Session{}, identity.User{}, errSessionNotLive
	}

	user, err := s.users.ByID(ctx, session.UserID)
	if err != nil {
		// A session whose user has been removed is refused, and it is refused
		// as "not live" rather than as a server error. Removal is terminal and
		// the row is history; a console that answered 500 here would be
		// reporting an outage for a state the system decided on purpose.
		if errors.Is(err, persistence.ErrNotFound) {
			return identity.Session{}, identity.User{}, errSessionNotLive
		}
		return identity.Session{}, identity.User{}, fmt.Errorf("console-api: resolve session: read the user: %w", err)
	}
	if user.State != identity.UserActive {
		return identity.Session{}, identity.User{}, errSessionNotLive
	}

	return session, user, nil
}

// principalFor renders the wire principal from a session and the user it
// belongs to.
//
// The class is the SESSION's and not the user's, because it says what the
// holder may do across accounts rather than which credential authenticated —
// the two are orthogonal axes, and a principal that took the class from the
// user row would be describing the wrong one. `user` is the only class this
// surface mints, and reading it from the session means a future operator class
// is admitted by a row rather than by a branch in this function.
func principalFor(session *identity.Session, user identity.User) http.Principal {
	return http.Principal{
		Class:            http.PrincipalClass(session.Class),
		AccountID:        string(session.AccountID),
		UserID:           string(session.UserID),
		Email:            user.Email,
		SessionExpiresAt: wireTimestamp(session.ExpiresAt),
	}
}
