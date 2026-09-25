package catalog

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewBackendAcceptsAnHonestTarget(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	id, err := NewBackendID()
	if err != nil {
		t.Fatalf("NewBackendID returned error: %v", err)
	}
	backend, err := NewBackend(id, " openai-compatible ", " https://api.example.com/v1 ", " creds/main ", " ", now)
	if err != nil {
		t.Fatalf("NewBackend returned error: %v", err)
	}
	// The persisted values are the trimmed ones — the database layer must
	// not re-decide what an operator wrote.
	if backend.AdapterType != "openai-compatible" {
		t.Errorf("AdapterType = %q, want the trimmed token", backend.AdapterType)
	}
	if backend.Endpoint != "https://api.example.com/v1" {
		t.Errorf("Endpoint = %q, want the trimmed URL", backend.Endpoint)
	}
	if backend.CredentialsRef != "creds/main" {
		t.Errorf("CredentialsRef = %q, want the trimmed reference", backend.CredentialsRef)
	}
	// Whitespace-only is absence after trimming: an empty reference is
	// data, and a blank one is not a credential.
	if backend.EgressPolicyRef != "" {
		t.Errorf("EgressPolicyRef = %q, want absence", backend.EgressPolicyRef)
	}
	if backend.State != BackendActive {
		t.Errorf("State = %q, want the birth state active", backend.State)
	}
	if !backend.CreatedAt.Equal(now) || !backend.UpdatedAt.Equal(now) {
		t.Errorf("timestamps = %v/%v, want the caller's instant", backend.CreatedAt, backend.UpdatedAt)
	}
}

func TestNewBackendRefusesTargetsOutsideTheGrammars(t *testing.T) {
	valid := func(overrides map[string]string) (string, string) {
		adapter, endpoint := "openai-compatible", "https://api.example.com"
		if v, ok := overrides["adapter"]; ok {
			adapter = v
		}
		if v, ok := overrides["endpoint"]; ok {
			endpoint = v
		}
		return adapter, endpoint
	}
	for name, tc := range map[string]struct {
		overrides map[string]string
	}{
		"blank adapter":        {map[string]string{"adapter": ""}},
		"uppercase adapter":    {map[string]string{"adapter": "OpenAI"}},
		"underscore adapter":   {map[string]string{"adapter": "open_ai"}},
		"leading dash":         {map[string]string{"adapter": "-openai"}},
		"trailing dash":        {map[string]string{"adapter": "openai-"}},
		"empty dash run":       {map[string]string{"adapter": "openai--compatible"}},
		"adapter too long":     {map[string]string{"adapter": strings.Repeat("a", 65)}},
		"blank endpoint":       {map[string]string{"endpoint": ""}},
		"short endpoint":       {map[string]string{"endpoint": "https://"}},
		"wrong scheme":         {map[string]string{"endpoint": "ftp://api.example.com"}},
		"no scheme":            {map[string]string{"endpoint": "api.example.com"}},
		"endpoint too long":    {map[string]string{"endpoint": "https://" + strings.Repeat("a", 2048)}},
		"oversized credential": {map[string]string{}},
	} {
		id := BackendID("test-backend")
		adapter, endpoint := valid(tc.overrides)
		credentialsRef := ""
		if name == "oversized credential" {
			credentialsRef = strings.Repeat("x", 513)
		}
		_, err := NewBackend(id, adapter, endpoint, credentialsRef, "", time.Now())
		if err == nil {
			t.Fatalf("%s: NewBackend accepted an outside-the-grammar target", name)
		}
		if !errors.Is(err, ErrInvalidBackendTarget) {
			t.Errorf("%s: error = %v, want ErrInvalidBackendTarget", name, err)
		}
	}
}

func TestNewBackendAcceptsTheGrammarEdgeValues(t *testing.T) {
	id := BackendID("test-backend")
	for name, adapter := range map[string]string{
		"single character":  "a",
		"digits":            "0123456789",
		"exactly sixtyfour": strings.Repeat("a", 64),
		"multi segment":     "gcp-vertex-1",
	} {
		if _, err := NewBackend(id, adapter, "https://api.example.com", "", "", time.Now()); err != nil {
			t.Errorf("%s (%q): NewBackend returned error: %v", name, adapter, err)
		}
	}
}

func TestBackendLifecycleIsReversible(t *testing.T) {
	id, err := NewBackendID()
	if err != nil {
		t.Fatalf("NewBackendID returned error: %v", err)
	}
	backend, err := NewBackend(id, "anthropic", "https://api.example.com", "", "", time.Now())
	if err != nil {
		t.Fatalf("NewBackend returned error: %v", err)
	}
	first := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if err := backend.Disable(first); err != nil {
		t.Fatalf("Disable returned error: %v", err)
	}
	if backend.State != BackendDisabled {
		t.Fatalf("State = %q, want disabled", backend.State)
	}
	if !backend.UpdatedAt.Equal(first) {
		t.Errorf("UpdatedAt = %v, want the disabling instant", backend.UpdatedAt)
	}
	// Disabling a disabled backend is a no-op, not an error — the
	// operator's intent is already recorded.
	if err := backend.Disable(first.Add(time.Minute)); err != nil {
		t.Errorf("second Disable returned error: %v", err)
	}
	if !backend.UpdatedAt.Equal(first) {
		t.Errorf("a no-op move moved the stamp: UpdatedAt = %v", backend.UpdatedAt)
	}
	// And the machine runs the other way, because nothing here is terminal.
	second := first.Add(2 * time.Minute)
	if err := backend.Enable(second); err != nil {
		t.Fatalf("Enable returned error: %v", err)
	}
	if backend.State != BackendActive {
		t.Fatalf("State = %q, want active again", backend.State)
	}
	if err := backend.Enable(second); err != nil {
		t.Errorf("second Enable returned error: %v", err)
	}
}

func TestBackendUpdateTargetRepointsWithoutTouchingTheAdapter(t *testing.T) {
	id, err := NewBackendID()
	if err != nil {
		t.Fatalf("NewBackendID returned error: %v", err)
	}
	backend, err := NewBackend(id, "anthropic", "https://old.example.com", "", "", time.Now())
	if err != nil {
		t.Fatalf("NewBackend returned error: %v", err)
	}
	if err := backend.Disable(time.Now()); err != nil {
		t.Fatalf("Disable returned error: %v", err)
	}
	// Re-pointing a drained backend is ordinary incident work — the move
	// must not require an active state.
	now := time.Date(2026, 9, 25, 13, 0, 0, 0, time.UTC)
	if err := backend.UpdateTarget("https://new.example.com", "creds/new", "egress/new", now); err != nil {
		t.Fatalf("UpdateTarget returned error: %v", err)
	}
	if backend.Endpoint != "https://new.example.com" || backend.CredentialsRef != "creds/new" || backend.EgressPolicyRef != "egress/new" {
		t.Fatalf("target = %q/%q/%q, want the new values", backend.Endpoint, backend.CredentialsRef, backend.EgressPolicyRef)
	}
	if backend.AdapterType != "anthropic" {
		t.Errorf("AdapterType = %q, want it untouched", backend.AdapterType)
	}
	if backend.State != BackendDisabled {
		t.Errorf("State = %q, want the state axis untouched by a target move", backend.State)
	}
	if err := backend.UpdateTarget("https://", "", "", now); !errors.Is(err, ErrInvalidBackendTarget) {
		t.Errorf("UpdateTarget with a bad endpoint error = %v, want ErrInvalidBackendTarget", err)
	}
}
