package identity

import (
	"fmt"
	"time"
	"unicode/utf8"
)

// APIKeyState is an API key's whole lifecycle: active → revoked, one-way.
// There is no suspended, no expired and no un-revoked — a key that must stop
// authenticating is revoked, and revocation is forever (ADR 0001).
type APIKeyState string

const (
	// APIKeyActive: the key's credential, where it is projected, may
	// authenticate.
	APIKeyActive APIKeyState = "active"
	// APIKeyRevoked: terminal. The credential must never authenticate
	// again, wherever the record lives.
	APIKeyRevoked APIKeyState = "revoked"
)

// maxDisplayNameLen bounds the operator-chosen label. Like the account name,
// it is a rune count so the limit does not depend on script.
const maxDisplayNameLen = 256

// APIKey is the ownership record of one API key — the Control Plane half of
// the two-record model (ADR 0006 §8). It holds who the key belongs to, who
// created it, a display label and a lifecycle state. It holds NO secret
// material: not the plaintext, not the digest. The plaintext exists only in
// the mint call's return value; the digest is the Data Plane credential
// record's column, delivered there by a later phase.
type APIKey struct {
	ID          APIKeyID
	AccountID   AccountID
	CreatedBy   UserID // zero when the key was minted without a creating user
	DisplayName string
	// Prefix is the public lookup hint baked into every presentation of the
	// token: brand + key id. It is derived from the id, carries no secret,
	// and lets consoles show operators what they created.
	Prefix    string
	State     APIKeyState
	CreatedAt time.Time
	UpdatedAt time.Time
	RevokedAt *time.Time // non-nil exactly when State is APIKeyRevoked
}

// NewAPIKey returns a key in its only legal birth state, active, with its
// public prefix derived from the id. RevokedAt is nil; the
// (state = revoked) ⇔ (revoked_at non-nil) pairing is also enforced by a
// database check, because the two records of the pair must never disagree.
//
// The id must be a UUIDv4-form string — the same grammar ParseToken enforces
// on the id segment of a presented token. A key minted with any other id
// would carry a prefix outside the token grammar and produce a one-time
// credential that can never authenticate: the refusals must happen here, at
// mint, where the error fronts the developer — not at first presentation,
// where it fronts the operator holding a broken credential.
func NewAPIKey(id APIKeyID, accountID AccountID, createdBy UserID, displayName string, now time.Time) (*APIKey, error) {
	displayName = trimSpace(displayName)
	if err := validateDisplayName(displayName); err != nil {
		return nil, err
	}
	if err := validateUUIDForm(string(id)); err != nil {
		// A malformed id is a programming error upstream, not a domain rule —
		// no sentinel.
		return nil, fmt.Errorf("identity: new api key: %w", err)
	}
	if accountID == "" {
		return nil, fmt.Errorf("identity: new api key: blank account id")
	}
	now = now.UTC()
	return &APIKey{
		ID:          id,
		AccountID:   accountID,
		CreatedBy:   createdBy,
		DisplayName: displayName,
		Prefix:      TokenPrefix(id),
		State:       APIKeyActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// Revoke retires the key. Revoking an already-revoked key is a no-op —
// idempotence here is what makes concurrent revoke races converge instead of
// fighting (the application layer still drives the state change through a
// compare-and-swap so a lost race re-reads rather than overwrites). There is
// no un-revoke anywhere in the model.
func (k *APIKey) Revoke(now time.Time) error {
	switch k.State {
	case APIKeyRevoked:
		return nil
	case APIKeyActive:
		t := now.UTC()
		k.State = APIKeyRevoked
		k.RevokedAt = &t
		k.UpdatedAt = t
		return nil
	default:
		return fmt.Errorf("identity: revoke api key %s: %w: unknown state %q", k.ID, ErrInvalidTransition, k.State)
	}
}

// validateDisplayName enforces the same bar as the account name: present and
// bounded. The label is operator-facing metadata; it never authenticates
// anything.
func validateDisplayName(name string) error {
	trimmed := trimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("identity: new api key: %w: display name is blank", ErrInvalidDisplayName)
	}
	if n := utf8.RuneCountInString(trimmed); n > maxDisplayNameLen {
		return fmt.Errorf("identity: new api key: %w: %d runes exceeds %d", ErrInvalidDisplayName, n, maxDisplayNameLen)
	}
	return nil
}
