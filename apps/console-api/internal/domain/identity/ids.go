package identity

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// The aggregate identifiers are distinct types, not aliases of string, so a
// user ID can never be passed where an account ID is expected and a key can
// never be filed under the wrong owner by a signature accident. They are
// rendered as RFC 4122 version-4 UUIDs, which the persistence adapter and
// the token grammar both rely on (see keymaterial.go for the latter).

type (
	// AccountID identifies one billing-and-ownership root.
	AccountID string
	// UserID identifies one console identity belonging to exactly one account.
	UserID string
	// APIKeyID identifies one API key and doubles as the key's public token
	// prefix material — it appears in every presentation of the key, so it
	// must never be secret.
	APIKeyID string
)

// NewAccountID mints a random account identifier.
func NewAccountID() (AccountID, error) {
	id, err := newUUID()
	if err != nil {
		return "", fmt.Errorf("identity: new account id: %w", err)
	}
	return AccountID(id), nil
}

// NewUserID mints a random user identifier.
func NewUserID() (UserID, error) {
	id, err := newUUID()
	if err != nil {
		return "", fmt.Errorf("identity: new user id: %w", err)
	}
	return UserID(id), nil
}

// NewAPIKeyID mints a random API-key identifier.
func NewAPIKeyID() (APIKeyID, error) {
	id, err := newUUID()
	if err != nil {
		return "", fmt.Errorf("identity: new api key id: %w", err)
	}
	return APIKeyID(id), nil
}

// newUUID returns a random RFC 4122 version-4 UUID in lowercase canonical
// form. The standard library is enough: sixteen random bytes, the version
// and variant bits fixed, hex-encoded. A dedicated uuid dependency would buy
// nothing this module keeps.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("identity: read random bytes: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	h := make([]byte, 36)
	hex.Encode(h[0:8], b[0:4])
	h[8] = '-'
	hex.Encode(h[9:13], b[4:6])
	h[13] = '-'
	hex.Encode(h[14:18], b[6:8])
	h[18] = '-'
	hex.Encode(h[19:23], b[8:10])
	h[23] = '-'
	hex.Encode(h[24:36], b[10:16])
	return string(h), nil
}
