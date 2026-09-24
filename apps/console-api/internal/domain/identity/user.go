package identity

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// UserState is a console identity's position on the lifecycle ADR 0001
// fixes: invited → active → removed, with removed one-way terminal.
type UserState string

const (
	// UserInvited: the identity exists but has not completed activation.
	UserInvited UserState = "invited"
	// UserActive: the identity may act for its account.
	UserActive UserState = "active"
	// UserRemoved: terminal. The identity never returns; its email may be
	// invited again because the live-email uniqueness is scoped to
	// non-removed rows.
	UserRemoved UserState = "removed"
)

// Email bounds mirror common address limits: 254 for the whole address,
// 64 for the local part. The shape check stays minimal — one "@", no
// whitespace anywhere — because plausibility, not deliverability, is the
// domain's bar (see ErrInvalidEmail).
const (
	maxEmailLen      = 254
	maxEmailLocalLen = 64
)

// User is one console identity. Every user belongs to exactly one account —
// ADR 0001 fixes the ownership edge as user → account, with no secondary
// membership and no cross-account identity, so the field below is the whole
// story.
type User struct {
	ID        UserID
	AccountID AccountID
	Email     string
	State     UserState
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewUser returns a user in its only legal birth state, invited, with the
// email normalised to the lowercase form uniqueness is enforced on.
func NewUser(id UserID, accountID AccountID, email string, now time.Time) (*User, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return nil, err
	}
	if id == "" {
		// A blank id is a programming error upstream, not a domain rule —
		// no sentinel.
		return nil, fmt.Errorf("identity: new user: blank id")
	}
	if accountID == "" {
		return nil, fmt.Errorf("identity: new user: blank account id")
	}
	now = now.UTC()
	return &User{
		ID:        id,
		AccountID: accountID,
		Email:     email,
		State:     UserInvited,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// Activate completes an invitation. Activating an active user is a no-op;
// a removed user refuses: removal is terminal and its row exists only as
// history.
func (u *User) Activate(now time.Time) error {
	switch u.State {
	case UserActive:
		return nil
	case UserRemoved:
		return fmt.Errorf("identity: activate user %s: %w: user is removed", u.ID, ErrInvalidTransition)
	case UserInvited:
		u.State = UserActive
		u.UpdatedAt = now.UTC()
		return nil
	default:
		return fmt.Errorf("identity: activate user %s: %w: unknown state %q", u.ID, ErrInvalidTransition, u.State)
	}
}

// Remove retires the identity from any state. Removing a removed user is a
// no-op. There is no un-remove: the aggregate cannot resurrect, and the
// persistence layer never deletes the row.
func (u *User) Remove(now time.Time) error {
	switch u.State {
	case UserRemoved:
		return nil
	case UserInvited, UserActive:
		u.State = UserRemoved
		u.UpdatedAt = now.UTC()
		return nil
	default:
		return fmt.Errorf("identity: remove user %s: %w: unknown state %q", u.ID, ErrInvalidTransition, u.State)
	}
}

// normalizeEmail trims, lowercases and shape-checks an address. Lowercase is
// the canonical comparison form — the uniqueness constraint in the Control
// Plane database compares this column directly, so the domain must fold case
// before persistence, not after.
func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(trimSpace(email))
	if email == "" {
		return "", fmt.Errorf("identity: new user: %w: email is blank", ErrInvalidEmail)
	}
	if n := utf8.RuneCountInString(email); n > maxEmailLen {
		return "", fmt.Errorf("identity: new user: %w: %d runes exceeds %d", ErrInvalidEmail, n, maxEmailLen)
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" {
		return "", fmt.Errorf("identity: new user: %w: need exactly one @ with content either side", ErrInvalidEmail)
	}
	// The reject set here is deliberately the same set trimSpace trims at the
	// edges: unicode.IsSpace, not an ASCII table. An interior U+00A0 or
	// U+2028 is whitespace by the same definition the trim applies, and an
	// address holding one would be stored, displayed and never match anything
	// a human types or another system normalizes.
	if strings.IndexFunc(email, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("identity: new user: %w: whitespace is not allowed", ErrInvalidEmail)
	}
	if n := utf8.RuneCountInString(local); n > maxEmailLocalLen {
		return "", fmt.Errorf("identity: new user: %w: local part %d runes exceeds %d", ErrInvalidEmail, n, maxEmailLocalLen)
	}
	if strings.Count(email, "@") != 1 {
		return "", fmt.Errorf("identity: new user: %w: exactly one @ is allowed", ErrInvalidEmail)
	}
	return email, nil
}
