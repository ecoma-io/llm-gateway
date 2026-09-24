package identity

import (
	"fmt"
	"time"
	"unicode/utf8"
)

// AccountState is an account's position on the lifecycle ADR 0001 fixes:
// active → suspended → closed, with closed one-way terminal.
type AccountState string

const (
	// AccountActive: the account may own principals and mint keys.
	AccountActive AccountState = "active"
	// AccountSuspended: frozen — existing principals stop authenticating,
	// and nothing new may attach to the account.
	AccountSuspended AccountState = "suspended"
	// AccountClosed: terminal. The account never reopens.
	AccountClosed AccountState = "closed"
)

// maxAccountNameLen bounds the human-chosen name. The limit is a rune count,
// not a byte count, so a name's worth does not depend on its script.
const maxAccountNameLen = 256

// Account is the ownership root: the thing users and API keys belong to and
// the thing suspension closes down in one move. It carries no billing state
// here — pay-as-you-go buckets are an Accounting concern on their own
// aggregate (ADR 0001); creating an account creates exactly this record.
type Account struct {
	ID        AccountID
	Name      string
	State     AccountState
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewAccount returns an account in its only legal birth state, active.
// The clock is injected so tests stay deterministic; use cases pass
// time.Now(). Name rules: non-blank after trimming, at most
// maxAccountNameLen runes; the trimmed form is what the aggregate stores,
// so the persisted value never disagrees with the validated one.
func NewAccount(id AccountID, name string, now time.Time) (*Account, error) {
	name = trimSpace(name)
	if err := validateAccountName(name); err != nil {
		return nil, err
	}
	if id == "" {
		// A blank id is a programming error upstream (NewAccountID cannot
		// produce one), not a domain rule violation — no sentinel.
		return nil, fmt.Errorf("identity: new account: blank id")
	}
	now = now.UTC()
	return &Account{
		ID:        id,
		Name:      name,
		State:     AccountActive,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// Suspend freezes the account. Suspending an already-suspended account is a
// no-op — the operator's intent is already recorded — but a closed account
// refuses every move: closed is terminal and the machine never resurrects.
func (a *Account) Suspend(now time.Time) error {
	switch a.State {
	case AccountSuspended:
		return nil
	case AccountClosed:
		return fmt.Errorf("identity: suspend account %s: %w: account is closed", a.ID, ErrInvalidTransition)
	case AccountActive:
		a.State = AccountSuspended
		a.UpdatedAt = now.UTC()
		return nil
	default:
		return fmt.Errorf("identity: suspend account %s: %w: unknown state %q", a.ID, ErrInvalidTransition, a.State)
	}
}

// Reinstate lifts a suspension. It is the only transition back out of a
// non-terminal state, and it refuses to touch a closed account.
func (a *Account) Reinstate(now time.Time) error {
	switch a.State {
	case AccountActive:
		return nil
	case AccountClosed:
		return fmt.Errorf("identity: reinstate account %s: %w: account is closed", a.ID, ErrInvalidTransition)
	case AccountSuspended:
		a.State = AccountActive
		a.UpdatedAt = now.UTC()
		return nil
	default:
		return fmt.Errorf("identity: reinstate account %s: %w: unknown state %q", a.ID, ErrInvalidTransition, a.State)
	}
}

// Close ends the account from any state. Closing an already-closed account
// is a no-op, not an error: terminal states absorb repetition, which keeps
// retry paths honest without an existence probe.
func (a *Account) Close(now time.Time) error {
	switch a.State {
	case AccountClosed:
		return nil
	case AccountActive, AccountSuspended:
		a.State = AccountClosed
		a.UpdatedAt = now.UTC()
		return nil
	default:
		return fmt.Errorf("identity: close account %s: %w: unknown state %q", a.ID, ErrInvalidTransition, a.State)
	}
}

// validateAccountName enforces the one rule the name has: it must say
// something. Everything else about an account's presentation is console
// concern, not identity concern.
func validateAccountName(name string) error {
	trimmed := trimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("identity: new account: %w: name is blank", ErrInvalidAccountName)
	}
	if n := utf8.RuneCountInString(trimmed); n > maxAccountNameLen {
		return fmt.Errorf("identity: new account: %w: %d runes exceeds %d", ErrInvalidAccountName, n, maxAccountNameLen)
	}
	return nil
}
