package identity

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var clock = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func TestNewAccountIsBornActiveWithTrimmedName(t *testing.T) {
	a, err := NewAccount("acc-1", "  Acme  ", clock)
	if err != nil {
		t.Fatalf("NewAccount returned error: %v", err)
	}
	if a.State != AccountActive {
		t.Fatalf("NewAccount state = %q, want active", a.State)
	}
	if a.Name != "Acme" {
		t.Fatalf("NewAccount name = %q, want trimmed %q", a.Name, "Acme")
	}
	if !a.CreatedAt.Equal(clock) || !a.UpdatedAt.Equal(clock) {
		t.Fatalf("NewAccount timestamps = %v/%v, want the injected clock %v", a.CreatedAt, a.UpdatedAt, clock)
	}
}

func TestNewAccountRejectsBlankAndOversizedNames(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"whitespace":  "   ",
		"tabs":        "\t\t",
		"over length": strings.Repeat("x", maxAccountNameLen+1),
	}
	for name, input := range cases {
		if _, err := NewAccount("acc-1", input, clock); !errors.Is(err, ErrInvalidAccountName) {
			t.Fatalf("%s: NewAccount error = %v, want ErrInvalidAccountName", name, err)
		}
	}
	if _, err := NewAccount("", "Acme", clock); err == nil || errors.Is(err, ErrInvalidAccountName) {
		t.Fatalf("NewAccount with blank id: error = %v, want a non-sentinel id error", err)
	}
	if _, err := NewAccount("acc-1", strings.Repeat("x", maxAccountNameLen), clock); err != nil {
		t.Fatalf("NewAccount at exactly the length limit returned error: %v", err)
	}
}

func TestAccountSuspendsOnlyFromActiveAndReinstatesOnlyFromSuspended(t *testing.T) {
	t.Run("active suspends", func(t *testing.T) {
		a := mustAccount(t)
		if err := a.Suspend(clock.Add(time.Minute)); err != nil {
			t.Fatalf("Suspend returned error: %v", err)
		}
		if a.State != AccountSuspended || a.UpdatedAt.Equal(clock) {
			t.Fatalf("after Suspend: state = %q, updated = %v", a.State, a.UpdatedAt)
		}
	})
	t.Run("suspended reinstates", func(t *testing.T) {
		a := suspendedAccount(t)
		if err := a.Reinstate(clock.Add(time.Minute)); err != nil {
			t.Fatalf("Reinstate returned error: %v", err)
		}
		if a.State != AccountActive {
			t.Fatalf("after Reinstate: state = %q, want active", a.State)
		}
	})
	t.Run("closed refuses suspend", func(t *testing.T) {
		a := closedAccount(t)
		if err := a.Suspend(clock.Add(time.Minute)); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("Suspend on closed error = %v, want ErrInvalidTransition", err)
		}
		if a.State != AccountClosed {
			t.Fatalf("failed Suspend mutated state to %q", a.State)
		}
	})
	t.Run("closed refuses reinstate", func(t *testing.T) {
		a := closedAccount(t)
		if err := a.Reinstate(clock.Add(time.Minute)); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("Reinstate on closed error = %v, want ErrInvalidTransition", err)
		}
	})
}

func TestAccountCloseIsTerminalAndAbsorbsRepetition(t *testing.T) {
	t.Run("from active", func(t *testing.T) {
		a := mustAccount(t)
		if err := a.Close(clock.Add(time.Minute)); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
		if a.State != AccountClosed {
			t.Fatalf("after Close: state = %q", a.State)
		}
	})
	t.Run("from suspended", func(t *testing.T) {
		a := suspendedAccount(t)
		if err := a.Close(clock.Add(time.Minute)); err != nil {
			t.Fatalf("Close from suspended returned error: %v", err)
		}
		if a.State != AccountClosed {
			t.Fatalf("after Close: state = %q", a.State)
		}
	})
	t.Run("repeated close is a no-op", func(t *testing.T) {
		a := closedAccount(t)
		if err := a.Close(clock.Add(time.Minute)); err != nil {
			t.Fatalf("second Close returned error: %v", err)
		}
		if a.State != AccountClosed {
			t.Fatalf("after second Close: state = %q", a.State)
		}
	})
	t.Run("closed never reactivates", func(t *testing.T) {
		a := closedAccount(t)
		if err := a.Reinstate(clock.Add(time.Minute)); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("Reinstate on closed error = %v, want ErrInvalidTransition", err)
		}
		if a.State != AccountClosed {
			t.Fatalf("closed account changed state to %q", a.State)
		}
	})
}

func TestAccountSuspendsWithNoChangeLeavesUpdatedAtAlone(t *testing.T) {
	a := suspendedAccount(t)
	if err := a.Suspend(clock.Add(time.Minute)); err != nil {
		t.Fatalf("repeated Suspend returned error: %v", err)
	}
	if !a.UpdatedAt.Equal(clock) {
		t.Fatalf("no-op Suspend moved UpdatedAt to %v", a.UpdatedAt)
	}
}

func mustAccount(t *testing.T) *Account {
	t.Helper()
	a, err := NewAccount("acc-1", "Acme", clock)
	if err != nil {
		t.Fatalf("NewAccount returned error: %v", err)
	}
	return a
}

func suspendedAccount(t *testing.T) *Account {
	t.Helper()
	a := mustAccount(t)
	if err := a.Suspend(clock); err != nil {
		t.Fatalf("Suspend returned error: %v", err)
	}
	return a
}

func closedAccount(t *testing.T) *Account {
	t.Helper()
	a := mustAccount(t)
	if err := a.Close(clock); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	return a
}
