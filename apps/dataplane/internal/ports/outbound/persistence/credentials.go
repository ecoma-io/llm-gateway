package persistence

import (
	"context"
	"errors"
	"time"
)

// ErrCredentialNotFound is the credential mirror's miss sentinel: no
// api_key_credentials row carries the presented key id. It means a miss and
// only a miss — a key the Control Plane never projected, or one a snapshot's
// absence delete removed. The caller does not treat it as its own failure:
// the authentication layer synthesizes a zero credential for it so the
// presented secret's digest is still computed and compared in constant time,
// burning the same work a real comparison would, and only then reports the
// rejection. Any other error from Lookup is the store's own failure — an
// unavailable database, a broken row — and the caller answers for it as such,
// never as an authentication verdict.
var ErrCredentialNotFound = errors.New("persistence: credential not found")

// CredentialView is one api_key_credentials row as the request path needs it:
// the key's verification material and lifecycle beside the account lifecycle
// it belongs to. It is a read model of the mirror, not the Control Plane's
// ownership record — no name, no billing, no ownership edges cross here
// (ADR 0007).
//
// The two lifecycles travel together but are judged by the caller in its own
// order — the presented secret's digest first, then the key state, then the
// account state — so a rejection never reveals which of the later checks
// would have failed. AccountState is nil exactly when the mirror holds no
// account row for the credential's account: a mirror-integrity violation the
// caller must fail closed on, never a serving window. The read preserves that
// absence — it does not coalesce it to any default state — because a missing
// authority row is a fact the caller must see, while a synthesized "active"
// would be this port inventing an authorization.
type CredentialView struct {
	// Digest is the stored lowercase hex SHA-256 of the key's secret — the
	// only form of the secret that exists on this plane. The caller digests
	// the presented token and compares against this value in constant time.
	Digest string

	// AccountID is the identity of the account the credential belongs to, read
	// from the credential row itself — never from the joined half, which is
	// why it is set even when the account row is absent (AccountState nil
	// beside an AccountID is exactly the integrity-violation shape). The
	// account every admission write keys on is this value: the request row,
	// the replay record and the drawdown all name the owner the mirror
	// verified, not one re-derived elsewhere.
	AccountID string

	// KeyState is the credential's lifecycle state as the mirror spells it
	// ("active", "revoked").
	KeyState string

	// RevokedAt is the revocation instant of a revoked key, nil otherwise —
	// the same shape the mirror column carries.
	RevokedAt *time.Time

	// AccountState is the owning account's lifecycle state as the mirror
	// spells it ("active", "suspended", "closed"), or nil when the account
	// row is absent from the mirror — the integrity-violation case above.
	AccountState *string
}

// Credentials is the outbound port the request path verifies presented
// credentials through: it reads the credential mirror and nothing else.
//
// The mirror is the only credential source on this plane (ADR 0007), and this
// port is the only thing that reads it — enforced by tests in internal/arch,
// not by comments. The read it performs is ONE statement, the credential row
// LEFT JOINed to its account state, so the two halves cannot straddle a
// projection apply under READ COMMITTED: a snapshot or batch that replaces
// both rows commits them together, and a single-statement read sees the pair
// either entirely before or entirely after that commit. Two reads — key, then
// account — could observe a revoked key beside its pre-revocation account, or
// an account row deleted by a snapshot's absence delete beside a credential
// it kept; the join makes each lookup answer about one instant of the mirror,
// which is what an admission decision needs to be reproducible.
//
// The lookup rides the caller's unit of work when its context carries one,
// like every query in this port; the admission path typically runs it outside
// one, as a single read.
type Credentials interface {
	// Lookup returns the mirror row for keyID, or ErrCredentialNotFound when
	// no credential row carries that id. An account row missing beside an
	// existing credential is not an error from this method: it arrives as a
	// nil AccountState for the caller to fail closed on, because the absence
	// is a mirror-integrity fact the caller — not the store — decides how to
	// refuse. Every other error is the store's own failure.
	Lookup(ctx context.Context, keyID string) (CredentialView, error)
}
