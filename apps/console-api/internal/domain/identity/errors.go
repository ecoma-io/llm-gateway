package identity

import "errors"

// The package's distinguishable outcomes. Callers branch on these with
// errors.Is; everything else arrives wrapped with context. None of them may
// ever carry secret material in their text — they are echoed to consoles,
// logs and, eventually, API responses.

var (
	// ErrInvalidAccountName reports an account name that is blank or too long.
	ErrInvalidAccountName = errors.New("identity: invalid account name")

	// ErrInvalidEmail reports an email that is not a plausible address. The
	// bar is deliberately low — one "@", no whitespace, bounded length —
	// because the domain's job is to reject garbage, not to run an MX check.
	ErrInvalidEmail = errors.New("identity: invalid email")

	// ErrInvalidDisplayName reports a blank or oversized API-key label.
	ErrInvalidDisplayName = errors.New("identity: invalid api key display name")

	// ErrInvalidTransition reports a lifecycle move the state machine
	// forbids, such as reinstating a closed account. The aggregate methods
	// that return it name the offending move in their wrapped context.
	ErrInvalidTransition = errors.New("identity: invalid lifecycle transition")

	// ErrUnknownAccount reports an account id that does not exist.
	ErrUnknownAccount = errors.New("identity: unknown account")

	// ErrUnknownUser reports a user id that does not exist.
	ErrUnknownUser = errors.New("identity: unknown user")

	// ErrUnknownAPIKey reports an API-key id that does not exist.
	ErrUnknownAPIKey = errors.New("identity: unknown api key")

	// ErrCreatorOutsideAccount reports a mint request whose creator user
	// exists but belongs to a different account than the key would. The
	// ownership graph must stay a tree; this sentinel keeps a forged
	// cross-account creator from bending it.
	ErrCreatorOutsideAccount = errors.New("identity: creator belongs to another account")

	// ErrAccountNotActive reports an attempt to attach a new principal —
	// user or API key — to an account that is suspended or closed.
	// Suspension freezes the account; growth stops with it.
	ErrAccountNotActive = errors.New("identity: account is not active")

	// ErrUserEmailTaken reports a create-user whose (account, email) pair
	// collides with a live user. A removed user's email may be re-invited.
	ErrUserEmailTaken = errors.New("identity: email already used by a live user of this account")

	// ErrMalformedToken reports a presented token that does not parse at
	// all — wrong brand, wrong shape, bad UUID, bad base64, wrong secret
	// length, embedded whitespace. Malformed fails closed, and it fails
	// with this one sentinel regardless of which check tripped, so the
	// error text cannot become a parsing oracle.
	ErrMalformedToken = errors.New("identity: malformed api key token")

	// ErrUnknownCredential reports a well-formed token whose secret does
	// not match the recorded digest — or whose key id has no record at
	// all. Unknown and mismatched are deliberately the same sentinel: a
	// verifier that distinguishes them leaks which key ids exist.
	ErrUnknownCredential = errors.New("identity: unknown or mismatched credential")

	// ErrKeyRevoked reports a credential whose secret matched but whose
	// key has been revoked. Revocation must survive a correct secret.
	ErrKeyRevoked = errors.New("identity: api key is revoked")

	// ErrAccountSuspended reports a credential whose key is live but whose
	// account is suspended. The runtime contract names this state
	// explicitly (request-lifecycle: account_suspended).
	ErrAccountSuspended = errors.New("identity: account is suspended")

	// ErrAccountClosed reports a credential whose key is live but whose
	// account is closed. The runtime contract names this state explicitly
	// (request-lifecycle: account_closed).
	ErrAccountClosed = errors.New("identity: account is closed")
)
