package identity

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// canonicalV7 is a hand-built canonical UUIDv7: the version nibble at
// position 14 is '7' and the variant nibble at position 19 is 'b'. A fixed
// literal (rather than a minted id) is what the refusal table needs — every
// refusal below is one small edit of this string, so a check that stops
// catching its defect cannot hide behind a moving fixture.
const canonicalV7 = "7e57d004-2b77-7abc-b0cd-115e7f0a1b2c"

// minters names the three mint entry points over one generator. The table
// shape is what proves the "one generator" claim structurally: a fourth mint
// that disagreed about the spelling would have to be added here, where the
// assertions below would see it.
func minters() []struct {
	name string
	mint func() string
} {
	return []struct {
		name string
		mint func() string
	}{
		{"NewRequestID", func() string { return string(NewRequestID()) }},
		{"NewAttemptID", func() string { return string(NewAttemptID()) }},
		{"NewReservationID", func() string { return string(NewReservationID()) }},
	}
}

// TestMintedIDsAreCanonicalUUIDv7 pins the whole canonical shape at once:
// 36 characters, lowercase hex with hyphens exactly at 8/13/18/23, version
// nibble '7' and the RFC 9562 variant '8'..'b'. These ids are compared as
// strings, embedded in cursors and carried across the plane boundary, so a
// single minted character in the wrong place is not a cosmetic defect — it is
// an id nothing downstream can name.
func TestMintedIDsAreCanonicalUUIDv7(t *testing.T) {
	for _, m := range minters() {
		t.Run(m.name, func(t *testing.T) {
			id := m.mint()
			if len(id) != 36 {
				t.Fatalf("%s() = %q, want a 36-character uuid", m.name, id)
			}
			for i, c := range id {
				switch i {
				case 8, 13, 18, 23:
					if c != '-' {
						t.Errorf("%s() = %q, want a hyphen at position %d", m.name, id, i)
					}
				default:
					if c < '0' || ('9' < c && c < 'a') || 'f' < c {
						t.Errorf("%s() = %q, want lowercase hex at position %d, got %q", m.name, id, i, c)
					}
				}
			}
			if id[14] != '7' {
				t.Errorf("%s() = %q, want the version nibble at position 14 to be '7'", m.name, id)
			}
			if variant := id[19]; variant < '8' || variant > 'b' {
				t.Errorf("%s() = %q, want the variant nibble at position 19 in '8'..'b', got %q", m.name, id, variant)
			}
		})
	}
}

// TestMintedIDsCarryTheirMintingMillisecond pins version 7's reason to exist
// here: the leading 48 bits are the minting time in Unix milliseconds, which
// is what keeps id-clustered indexes locally sorted and lets a request id
// answer "roughly when" without a lookup. The bracket before/after the mint
// is the tolerance window — the value must land between two reads of the
// same clock, so a generator that forgot the timestamp (or encoded it in the
// wrong byte order) cannot pass.
func TestMintedIDsCarryTheirMintingMillisecond(t *testing.T) {
	before := time.Now().UnixMilli()
	minted := make([]string, 0, len(minters()))
	for _, m := range minters() {
		minted = append(minted, m.mint())
	}
	after := time.Now().UnixMilli()

	for i, id := range minted {
		// The first 12 hex characters are the 48-bit big-endian millisecond
		// count: id[0:8] then id[9:13] around the hyphen at position 8.
		ms, err := strconv.ParseUint(id[0:8]+id[9:13], 16, 64)
		if err != nil {
			t.Fatalf("%s prefix of %q does not parse as hex: %v", minters()[i].name, id, err)
		}
		if ms < uint64(before) || ms > uint64(after) {
			t.Errorf("%s timestamp = %d ms, want it within [%d, %d] — the 48-bit prefix is the minting millisecond", minters()[i].name, ms, before, after)
		}
	}
}

// TestMintedIDsAreUniqueAcrossTenThousandMints pins the random half's job:
// the timestamp carries ordering flavour, the random bits carry uniqueness,
// so ids minted within the same millisecond must still differ. All three
// types mint into one identity space ("every id is a uuid in the same
// database column domain"), so the mints share one map — a collision across
// types would be a cross-row collision just the same.
func TestMintedIDsAreUniqueAcrossTenThousandMints(t *testing.T) {
	const perType = 4000
	seen := make(map[string]struct{}, 3*perType)
	for _, m := range minters() {
		for i := 0; i < perType; i++ {
			seen[m.mint()] = struct{}{}
		}
	}
	if len(seen) != 3*perType {
		t.Errorf("minted %d ids across the three types, %d were distinct — the random bits stopped carrying uniqueness", 3*perType, len(seen))
	}
}

// TestParseRoundTripsMintedIDsAndAcceptsOneSharedSpelling pins Parse as the
// exact inverse of the mints, and pins that the three types read one shared
// identity space: a single canonical v7 string parses as all three, because
// the types are distinct views over one database column domain, not three
// different string formats.
func TestParseRoundTripsMintedIDsAndAcceptsOneSharedSpelling(t *testing.T) {
	for _, m := range minters() {
		minted := m.mint()
		switch m.name {
		case "NewRequestID":
			got, err := ParseRequestID(minted)
			if err != nil || got != RequestID(minted) {
				t.Errorf("ParseRequestID(%q) = (%q, %v), want the minted id back without error", minted, got, err)
			}
		case "NewAttemptID":
			got, err := ParseAttemptID(minted)
			if err != nil || got != AttemptID(minted) {
				t.Errorf("ParseAttemptID(%q) = (%q, %v), want the minted id back without error", minted, got, err)
			}
		case "NewReservationID":
			got, err := ParseReservationID(minted)
			if err != nil || got != ReservationID(minted) {
				t.Errorf("ParseReservationID(%q) = (%q, %v), want the minted id back without error", minted, got, err)
			}
		}
	}

	parsedRequest, errRequest := ParseRequestID(canonicalV7)
	parsedAttempt, errAttempt := ParseAttemptID(canonicalV7)
	parsedReservation, errReservation := ParseReservationID(canonicalV7)
	if errRequest != nil || errAttempt != nil || errReservation != nil {
		t.Fatalf("parsing one canonical v7 as all three types: (%v, %v, %v), want all accepted", errRequest, errAttempt, errReservation)
	}
	if string(parsedRequest) != canonicalV7 || string(parsedAttempt) != canonicalV7 || string(parsedReservation) != canonicalV7 {
		t.Errorf("parsed ids = (%q, %q, %q), want all three equal to the input %q", parsedRequest, parsedAttempt, parsedReservation, canonicalV7)
	}
}

// TestParseRefusesEverythingButTheCanonicalSpelling pins the boundary check:
// parsing is where a wire or row value becomes an identity, so everything
// that is not the exact canonical spelling of a v7 uuid — loose spellings
// PostgreSQL itself would accept, and canonical spellings of identities this
// package never mints (the nil uuid, a v4, a foreign variant) — is refused
// with the one sentinel. Accepting any of these would push normalisation or
// identity decisions to every later reader of the value.
func TestParseRefusesEverythingButTheCanonicalSpelling(t *testing.T) {
	refusals := []struct {
		name  string
		value string
	}{
		{"the empty string", ""},
		{"hex without hyphens", "7e57d0042b777abcb0cd115e7f0a1b2c"},
		{"one character short", canonicalV7[:35]},
		{"one character too many", canonicalV7 + "0"},
		{"uppercase hex", strings.ToUpper(canonicalV7)},
		{"braces around the value", "{" + canonicalV7 + "}"},
		{"the urn prefix", "urn:uuid:" + canonicalV7},
		{"a hyphen out of position", "7e57d004-2b77-7abc-b0cd1-15e7f0a1b2c"},
		{"a non-hex character", "7e57d004-2b77-7abc-b0cd-115e7f0a1b2g"},
		{"the nil uuid", "00000000-0000-0000-0000-000000000000"},
		{"a version 4 nibble", "7e57d004-2b77-4abc-b0cd-115e7f0a1b2c"},
		{"a version 0 nibble", "7e57d004-2b77-0abc-b0cd-115e7f0a1b2c"},
		{"a non-rfc-9562 variant", "7e57d004-2b77-7abc-c0cd-115e7f0a1b2c"},
	}
	parsers := []struct {
		name  string
		parse func(string) error
	}{
		{"ParseRequestID", func(s string) error { _, err := ParseRequestID(s); return err }},
		{"ParseAttemptID", func(s string) error { _, err := ParseAttemptID(s); return err }},
		{"ParseReservationID", func(s string) error { _, err := ParseReservationID(s); return err }},
	}

	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			for _, parser := range parsers {
				err := parser.parse(refusal.value)
				if err == nil {
					t.Errorf("%s(%q) accepted a value that is not a canonical uuid", parser.name, refusal.value)
					continue
				}
				if !errors.Is(err, ErrNotACanonicalUUID) {
					t.Errorf("%s(%q) error = %v, want it to wrap ErrNotACanonicalUUID — one sentinel keeps the parse site's vocabulary whole", parser.name, refusal.value, err)
				}
			}
		})
	}
}

// TestIdentityTypesAreDistinctOverTheSameValue pins the reason the types
// exist: a function that takes a RequestID must not be handed an AttemptID.
// From the same 36 characters the two types hold byte-identical strings, so
// the pin is the type, not the bytes — boxed through `any`, values of the two
// types must compare unequal (dynamic types differ), and reflect must see
// two distinct types. If either pin weakens, every identity parameter in the
// codebase has silently become a string with extra steps.
func TestIdentityTypesAreDistinctOverTheSameValue(t *testing.T) {
	requestID, err := ParseRequestID(canonicalV7)
	if err != nil {
		t.Fatalf("ParseRequestID() error = %v", err)
	}
	attemptID, err := ParseAttemptID(canonicalV7)
	if err != nil {
		t.Fatalf("ParseAttemptID() error = %v", err)
	}
	reservationID, err := ParseReservationID(canonicalV7)
	if err != nil {
		t.Fatalf("ParseReservationID() error = %v", err)
	}

	if string(requestID) != canonicalV7 || string(attemptID) != canonicalV7 || string(reservationID) != canonicalV7 {
		t.Errorf("string round-trips = (%q, %q, %q), want all equal to %q", requestID, attemptID, reservationID, canonicalV7)
	}

	var boxedRequest any = requestID
	var boxedAttempt any = attemptID
	var boxedReservation any = reservationID
	if boxedRequest == boxedAttempt || boxedRequest == boxedReservation || boxedAttempt == boxedReservation {
		t.Error("boxed values of the three identity types compared equal — the types have stopped being distinct")
	}
	if reflect.TypeOf(requestID) == reflect.TypeOf(attemptID) || reflect.TypeOf(requestID) == reflect.TypeOf(reservationID) {
		t.Errorf("types = (%s, %s, %s), want three distinct types", reflect.TypeOf(requestID), reflect.TypeOf(attemptID), reflect.TypeOf(reservationID))
	}
}
