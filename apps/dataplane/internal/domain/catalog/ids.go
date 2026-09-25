package catalog

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// The aggregate identifiers are distinct types, not aliases of string, so a
// backend ID can never be passed where an alias ID is expected and a group
// version can never be filed under the wrong group by a signature accident.
// The stakes are concrete here: group-version IDs are the one identifier the
// Control Plane stores (entitlement scope, ADR 0003), and alias and backend
// IDs travel inside usage facts the Control Plane settles from — a conflated
// ID type would be a cross-plane integrity bug with no local symptom.

type (
	// BackendID identifies one configured adapter instance.
	BackendID string
	// AliasID identifies one logical model name and its candidate list.
	AliasID string
	// CandidateID identifies one entry in one alias's fallback order.
	CandidateID string
	// GroupVersionID identifies one immutable alias-group snapshot. It is
	// the catalog's one client-facing cross-plane reference: entitlements
	// in the control database store exactly this.
	GroupVersionID string
)

// NewBackendID mints a backend identifier.
func NewBackendID() (BackendID, error) {
	id, err := newUUIDv7(time.Now())
	if err != nil {
		return "", fmt.Errorf("catalog: new backend id: %w", err)
	}
	return BackendID(id), nil
}

// NewAliasID mints an alias identifier.
func NewAliasID() (AliasID, error) {
	id, err := newUUIDv7(time.Now())
	if err != nil {
		return "", fmt.Errorf("catalog: new alias id: %w", err)
	}
	return AliasID(id), nil
}

// NewGroupVersionID mints a group-version identifier.
func NewGroupVersionID() (GroupVersionID, error) {
	id, err := newUUIDv7(time.Now())
	if err != nil {
		return "", fmt.Errorf("catalog: new group version id: %w", err)
	}
	return GroupVersionID(id), nil
}

// newCandidateID mints a candidate identifier. It is unexported because a
// candidate has no life outside its alias's aggregate: the alias mints one
// per entry when a candidate list is set, the way the list itself is the
// domain's to shape, and no caller outside the package has a reason to hold
// an ID for a candidate that does not exist yet.
func newCandidateID() (CandidateID, error) {
	id, err := newUUIDv7(time.Now())
	if err != nil {
		return "", fmt.Errorf("catalog: new candidate id: %w", err)
	}
	return CandidateID(id), nil
}

// newUUIDv7 returns an RFC 9562 version-7 UUID in lowercase canonical form,
// stamped with now's Unix millisecond. The identity domain hands-mints v4
// with the same sixteen-bytes-and-nibbles technique, and this is its v7
// twin — the standard library has no UUID at all, and a dependency to buy
// one layout would be a dependency the module does not otherwise want.
//
// Version 7 rather than 4 is persistence.md's rule, not a taste: these ids
// cross the plane boundary (entitlements store group-version ids; usage
// facts carry alias and backend ids) and reach clients, and the convention
// prescribes time-ordered ids for exactly those rows. The identity domain's
// v4 is the recorded exception — the API-key id IS the token's prefix
// material and the token grammar pins the v4 form — and nothing about this
// package needs a second exception.
//
// The layout, for the reader checking it against RFC 9562 section 5.7:
// 48 bits of big-endian Unix millisecond, 12 bits of random ver-and-a
// (version nibble 7 included), 2 variant bits (10xx), 62 bits of random.
// Uniqueness does not lean on the clock: only the ordering does, and the
// clock is injected so tests stay deterministic.
func newUUIDv7(now time.Time) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("catalog: read random bytes: %w", err)
	}
	ms := now.UnixMilli()
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 9562 variant
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
