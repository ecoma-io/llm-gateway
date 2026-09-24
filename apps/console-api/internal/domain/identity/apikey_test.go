package identity

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// keyID is a fixed UUIDv4-form id: the grammar NewAPIKey derives the key's
// public prefix from, and the same shape ParseToken enforces on presented
// tokens.
const keyID = APIKeyID("c0000000-0000-4000-8000-000000000001")

func TestNewAPIKeyIsBornActiveWithDerivedPrefixAndNoRevocation(t *testing.T) {
	k, err := NewAPIKey(keyID, "acc-1", "usr-1", "deploy key", clock)
	if err != nil {
		t.Fatalf("NewAPIKey returned error: %v", err)
	}
	if k.State != APIKeyActive {
		t.Fatalf("NewAPIKey state = %q, want active", k.State)
	}
	if k.Prefix != "gw_"+string(keyID)+"_" {
		t.Fatalf("NewAPIKey prefix = %q, want derived brand+id", k.Prefix)
	}
	if k.RevokedAt != nil {
		t.Fatalf("NewAPIKey revoked_at = %v, want nil", k.RevokedAt)
	}
	if !k.CreatedAt.Equal(clock) || !k.UpdatedAt.Equal(clock) {
		t.Fatalf("NewAPIKey timestamps = %v/%v, want the injected clock", k.CreatedAt, k.UpdatedAt)
	}
}

func TestNewAPIKeyRejectsBlankAndOversizedDisplayNames(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"whitespace":  "   ",
		"over length": strings.Repeat("x", maxDisplayNameLen+1),
	}
	for name, input := range cases {
		if _, err := NewAPIKey(keyID, "acc-1", "", input, clock); !errors.Is(err, ErrInvalidDisplayName) {
			t.Fatalf("%s: NewAPIKey error = %v, want ErrInvalidDisplayName", name, err)
		}
	}
	if _, err := NewAPIKey(keyID, "", "", "k", clock); err == nil {
		t.Fatalf("NewAPIKey with blank account id returned no error")
	}
}

func TestAPIKeyRevokeSetsStateTimestampAndStaysIdempotent(t *testing.T) {
	k, err := NewAPIKey(keyID, "acc-1", "usr-1", "deploy key", clock)
	if err != nil {
		t.Fatalf("NewAPIKey returned error: %v", err)
	}
	later := clock.Add(time.Minute)
	if err := k.Revoke(later); err != nil {
		t.Fatalf("Revoke returned error: %v", err)
	}
	if k.State != APIKeyRevoked {
		t.Fatalf("after Revoke: state = %q, want revoked", k.State)
	}
	if k.RevokedAt == nil || !k.RevokedAt.Equal(later) {
		t.Fatalf("after Revoke: revoked_at = %v, want %v", k.RevokedAt, later)
	}
	if !k.UpdatedAt.Equal(later) {
		t.Fatalf("after Revoke: updated_at = %v, want %v", k.UpdatedAt, later)
	}
	// A second revoke must be a no-op, not a timestamp churn.
	if err := k.Revoke(later.Add(time.Hour)); err != nil {
		t.Fatalf("repeated Revoke returned error: %v", err)
	}
	if !k.RevokedAt.Equal(later) {
		t.Fatalf("repeated Revoke moved revoked_at to %v", k.RevokedAt)
	}
	if !k.UpdatedAt.Equal(later) {
		t.Fatalf("repeated Revoke moved updated_at to %v", k.UpdatedAt)
	}
}

func TestAPIKeyCannotLeaveRevoked(t *testing.T) {
	k, err := NewAPIKey(keyID, "acc-1", "", "k", clock)
	if err != nil {
		t.Fatalf("NewAPIKey returned error: %v", err)
	}
	if err := k.Revoke(clock); err != nil {
		t.Fatalf("Revoke returned error: %v", err)
	}
	// There is no un-revoke: the only method that could express one does not
	// exist, and re-revoking is a no-op. The assertion below pins the state.
	if k.State != APIKeyRevoked {
		t.Fatalf("revoked key state = %q", k.State)
	}
}
