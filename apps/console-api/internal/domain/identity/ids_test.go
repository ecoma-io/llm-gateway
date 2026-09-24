package identity

import (
	"errors"
	"strings"
	"testing"
)

func TestMintedIdentifiersAreCanonicalUUIDv4AndDistinct(t *testing.T) {
	account, err := NewAccountID()
	if err != nil {
		t.Fatalf("NewAccountID returned error: %v", err)
	}
	user, err := NewUserID()
	if err != nil {
		t.Fatalf("NewUserID returned error: %v", err)
	}
	key, err := NewAPIKeyID()
	if err != nil {
		t.Fatalf("NewAPIKeyID returned error: %v", err)
	}
	for name, id := range map[string]string{
		"account": string(account),
		"user":    string(user),
		"key":     string(key),
	} {
		if err := validateUUIDForm(id); err != nil {
			t.Fatalf("%s id %q is not canonical UUID form: %v", name, id, err)
		}
		// Version nibble 4 and the RFC 4122 variant bits.
		if id[14] != '4' {
			t.Fatalf("%s id %q is not version 4", name, id)
		}
		if id[19] != '8' && id[19] != '9' && id[19] != 'a' && id[19] != 'b' {
			t.Fatalf("%s id %q lacks the RFC 4122 variant", name, id)
		}
	}
	if account == AccountID(user) || user == UserID(key) || account == AccountID(key) {
		t.Fatalf("minted identifiers collided: %q %q %q", account, user, key)
	}
}

func TestParseTokenValidatesUUIDFormStrictly(t *testing.T) {
	valid := "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e"
	if err := validateUUIDForm(valid); err != nil {
		t.Fatalf("validateUUIDForm rejected a canonical id: %v", err)
	}
	for name, bad := range map[string]string{
		"short":     valid[:35],
		"long":      valid + "0",
		"no dashes": strings.ReplaceAll(valid, "-", ""),
		"upper":     strings.ToUpper(valid),
		"bad char":  "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9g",
	} {
		if err := validateUUIDForm(bad); err == nil {
			t.Fatalf("%s: validateUUIDForm accepted %q", name, bad)
		}
	}
	// The error is a plain description, not a sentinel — callers wrap it
	// into ErrMalformedToken.
	if err := validateUUIDForm("short"); errors.Is(err, ErrMalformedToken) {
		t.Fatalf("grammar helper must not leak the sentinel")
	}
}
