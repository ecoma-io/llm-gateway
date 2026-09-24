package identity

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewUserIsBornInvitedWithNormalisedEmail(t *testing.T) {
	u, err := NewUser("usr-1", "acc-1", "  Mixed.Case@Example.COM  ", clock)
	if err != nil {
		t.Fatalf("NewUser returned error: %v", err)
	}
	if u.State != UserInvited {
		t.Fatalf("NewUser state = %q, want invited", u.State)
	}
	if u.Email != "mixed.case@example.com" {
		t.Fatalf("NewUser email = %q, want folded and trimmed", u.Email)
	}
	if !u.CreatedAt.Equal(clock) || !u.UpdatedAt.Equal(clock) {
		t.Fatalf("NewUser timestamps = %v/%v, want the injected clock", u.CreatedAt, u.UpdatedAt)
	}
}

func TestNewUserRejectsImplausibleEmails(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"whitespace only":  "   ",
		"no at":            "plainaddress",
		"two ats":          "a@b@c",
		"empty local":      "@example.com",
		"empty domain":     "local@",
		"internal space":   "lo cal@example.com",
		"interior newline": "local\n@example.com",
		"interior nbsp":    "lo\u00a0cal@example.com",
		"interior u+2028":  "lo\u2028cal@example.com",
		"over length":      strings.Repeat("x", maxEmailLen) + "@example.com",
		"long local part":  strings.Repeat("x", maxEmailLocalLen+1) + "@example.com",
	}
	for name, input := range cases {
		if _, err := NewUser("usr-1", "acc-1", input, clock); !errors.Is(err, ErrInvalidEmail) {
			t.Fatalf("%s: NewUser error = %v, want ErrInvalidEmail", name, err)
		}
	}
	// Surrounding whitespace is input hygiene, not malice: the trimmed form
	// is what the aggregate keeps, exactly as for the operator labels.
	u, err := NewUser("usr-1", "acc-1", "person@example.com\n", clock)
	if err != nil {
		t.Fatalf("NewUser with a trailing newline returned error: %v", err)
	}
	if u.Email != "person@example.com" {
		t.Fatalf("NewUser email = %q, want the trimmed form", u.Email)
	}
	if _, err := NewUser("", "acc-1", "a@b", clock); err == nil || errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("NewUser with blank id: error = %v, want a non-sentinel id error", err)
	}
}

func TestUserActivatesOnlyFromInvitedAndRefusesRemoved(t *testing.T) {
	t.Run("invited activates", func(t *testing.T) {
		u := mustUser(t)
		if err := u.Activate(clock.Add(time.Minute)); err != nil {
			t.Fatalf("Activate returned error: %v", err)
		}
		if u.State != UserActive || u.UpdatedAt.Equal(clock) {
			t.Fatalf("after Activate: state = %q, updated = %v", u.State, u.UpdatedAt)
		}
	})
	t.Run("active re-activate is a no-op", func(t *testing.T) {
		u := activeUser(t)
		if err := u.Activate(clock.Add(time.Minute)); err != nil {
			t.Fatalf("repeated Activate returned error: %v", err)
		}
		if !u.UpdatedAt.Equal(clock) {
			t.Fatalf("no-op Activate moved UpdatedAt to %v", u.UpdatedAt)
		}
	})
	t.Run("removed refuses activation", func(t *testing.T) {
		u := removedUser(t)
		if err := u.Activate(clock.Add(time.Minute)); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("Activate on removed error = %v, want ErrInvalidTransition", err)
		}
		if u.State != UserRemoved {
			t.Fatalf("failed Activate mutated state to %q", u.State)
		}
	})
}

func TestUserRemoveIsTerminalFromEveryState(t *testing.T) {
	for name, u := range map[string]*User{
		"invited": mustUser(t),
		"active":  activeUser(t),
	} {
		if err := u.Remove(clock.Add(time.Minute)); err != nil {
			t.Fatalf("Remove from %s returned error: %v", name, err)
		}
		if u.State != UserRemoved {
			t.Fatalf("Remove from %s left state %q", name, u.State)
		}
	}
	u := removedUser(t)
	if err := u.Remove(clock.Add(time.Minute)); err != nil {
		t.Fatalf("repeated Remove returned error: %v", err)
	}
	if u.State != UserRemoved {
		t.Fatalf("repeated Remove left state %q", u.State)
	}
	// Terminal repetition must not churn the row: the recorded moment of
	// removal is when the first Remove ran, not the last.
	if !u.UpdatedAt.Equal(clock) {
		t.Fatalf("repeated Remove moved UpdatedAt to %v", u.UpdatedAt)
	}
}

func mustUser(t *testing.T) *User {
	t.Helper()
	u, err := NewUser("usr-1", "acc-1", "person@example.com", clock)
	if err != nil {
		t.Fatalf("NewUser returned error: %v", err)
	}
	return u
}

func activeUser(t *testing.T) *User {
	t.Helper()
	u := mustUser(t)
	if err := u.Activate(clock); err != nil {
		t.Fatalf("Activate returned error: %v", err)
	}
	return u
}

func removedUser(t *testing.T) *User {
	t.Helper()
	u := mustUser(t)
	if err := u.Remove(clock); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	return u
}
