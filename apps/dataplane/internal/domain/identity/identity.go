// Package identity mints and names the identities the runtime persists: one
// generator for the UUID-shaped row keys of the `dataplane` database, and one
// distinct type per semantic identity those rows carry.
//
// The generator produces UUID version 7 — 48 bits of Unix milliseconds
// followed by 74 random bits — because persistence.md asks for version 7 for
// "any row whose identity crosses a plane or reaches a client", and every id
// this package mints belongs to a row that does both: a request id arrives on
// the OpenAI-compatible surface, and a fact carries its request id to the
// Control Plane. Version 7 puts the minting time in the value itself, so a
// request id tells you roughly when the request was admitted without a lookup,
// and indexes on id-clustered data stay locally sorted instead of scattering
// inserts the way fully random ids do.
//
// The types are distinct on purpose. A string can stand for anything, which is
// exactly why it must not stand for an identity here: a function that takes a
// RequestID cannot be handed an AttemptID, at a compile time a string would
// defer to a review that has better things to read. Each type is its own
// declaration rather than an alias, and parsing is explicit at the edges —
// where a wire value or a row value becomes an identity is where validation
// belongs.
package identity

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"
)

// RequestID is the runtime-minted identity of one request, from admission
// through every attempt, reservation and usage fact that hangs off it.
//
// It is deliberately not the HTTP correlation identifier: `X-Request-Id` is a
// transport handle a caller may invent, omit or reuse, while settlement hangs
// off this identity — so it is minted here, carried in the row that owns it,
// and never taken from the wire.
type RequestID string

// AttemptID is the identity of one upstream call, appended when the call
// finishes (ADR 0001 rule 4).
type AttemptID string

// ReservationID is the identity of one runtime reservation: the hold on
// future spend a request opens at admission and closes exactly once.
type ReservationID string

// ErrNotACanonicalUUID names every rejection Parse returns. One sentinel keeps
// the parse site honest about what it checked: an id either round-trips
// through the canonical 8-4-4-4-12 spelling or it is not an id this package
// mints, and no caller needs the reason split further.
var ErrNotACanonicalUUID = errors.New("identity: not a canonical uuid")

// NewRequestID, NewAttemptID and NewReservationID mint a fresh id of their
// type. All three mint through one generator — newUUIDv7 — because the
// identity space is shared: every id is a uuid in the same database column
// domain, and three generators would be three chances to disagree about how.
func NewRequestID() RequestID         { return RequestID(newUUIDv7()) }
func NewAttemptID() AttemptID         { return AttemptID(newUUIDv7()) }
func NewReservationID() ReservationID { return ReservationID(newUUIDv7()) }

// ParseRequestID, ParseAttemptID and ParseReservationID validate a string as
// the canonical spelling of an id and return it as that type. Parsing is the
// boundary check — a value that arrives from a row, a cursor or a wire field
// is not an identity until it passes — and a value that does not parse is
// refused with ErrNotACanonicalUUID rather than silently carried as a string
// that only looks like one.
func ParseRequestID(s string) (RequestID, error) {
	if !isCanonicalUUID(s) {
		return "", fmt.Errorf("%w: %q is not a request id", ErrNotACanonicalUUID, s)
	}
	return RequestID(s), nil
}

func ParseAttemptID(s string) (AttemptID, error) {
	if !isCanonicalUUID(s) {
		return "", fmt.Errorf("%w: %q is not an attempt id", ErrNotACanonicalUUID, s)
	}
	return AttemptID(s), nil
}

func ParseReservationID(s string) (ReservationID, error) {
	if !isCanonicalUUID(s) {
		return "", fmt.Errorf("%w: %q is not a reservation id", ErrNotACanonicalUUID, s)
	}
	return ReservationID(s), nil
}

// newUUIDv7 mints one RFC 9562 version-7 uuid in its canonical lowercase
// spelling: 48 bits of unix milliseconds, then 12 version and variant bits
// laid over 62 random bits, then 62 more.
//
// The random halves come from crypto/rand. A plain PRNG would make ids
// predictable, and these ids are addressable from the public surface — a
// predictable request id is a request another caller can name. Milliseconds
// are read once per call from the wall clock: ids minted within the same
// millisecond still differ, because the random bits carry the uniqueness and
// the timestamp only carries the ordering flavour.
func newUUIDv7() string {
	var b [16]byte
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand fails only when the operating system's entropy source
		// does, and a runtime that cannot mint identities cannot admit
		// requests. The panic is the honest failure: every caller of this
		// package would have to turn the error into one anyway.
		panic("identity: crypto/rand failed: " + err.Error())
	}
	unixMs := uint64(time.Now().UnixMilli())
	// 48 bits, big-endian, by hand: the standard library has no PutUint48.
	b[0] = byte(unixMs >> 40)
	b[1] = byte(unixMs >> 32)
	b[2] = byte(unixMs >> 24)
	b[3] = byte(unixMs >> 16)
	b[4] = byte(unixMs >> 8)
	b[5] = byte(unixMs)
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 9562 variant
	return formatUUID(b)
}

// formatUUID renders 16 bytes in the canonical 8-4-4-4-12 lowercase spelling
// the database stores and every cursor and wire field carries.
func formatUUID(b [16]byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hexDigits[v>>4], hexDigits[v&0x0f])
	}
	return string(out)
}

// isCanonicalUUID reports whether s is the exact 36-character canonical
// spelling of a uuid — lowercase hex with hyphens where RFC 9562 puts them.
//
// The check is deliberately stricter than "parses as a uuid": PostgreSQL
// accepts braces, urn prefixes and mixed case, but an id this package compares,
// embeds in a cursor or hands across a plane is canonical or it is nothing, and
// accepting the loose spellings here would mean normalising them everywhere
// else instead.
func isCanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(('0' <= c && c <= '9') || ('a' <= c && c <= 'f')) {
				return false
			}
		}
	}
	return true
}
