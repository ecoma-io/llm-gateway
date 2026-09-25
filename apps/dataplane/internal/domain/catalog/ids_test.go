package catalog

import (
	"strings"
	"testing"
	"time"
)

func TestMintedCatalogIdentifiersAreCanonicalUUIDv7AndDistinct(t *testing.T) {
	backend, err := NewBackendID()
	if err != nil {
		t.Fatalf("NewBackendID returned error: %v", err)
	}
	alias, err := NewAliasID()
	if err != nil {
		t.Fatalf("NewAliasID returned error: %v", err)
	}
	version, err := NewGroupVersionID()
	if err != nil {
		t.Fatalf("NewGroupVersionID returned error: %v", err)
	}
	candidate, err := newCandidateID()
	if err != nil {
		t.Fatalf("newCandidateID returned error: %v", err)
	}
	for name, id := range map[string]string{
		"backend":   string(backend),
		"alias":     string(alias),
		"version":   string(version),
		"candidate": string(candidate),
	} {
		// Canonical form: 36 characters, lowercase hex, dashes at the RFC's
		// positions — the form the schema's uuid columns and every other
		// consumer of these ids will see.
		if len(id) != 36 || strings.Count(id, "-") != 4 || id != strings.ToLower(id) {
			t.Fatalf("%s id %q is not canonical lowercase UUID form", name, id)
		}
		// Version nibble 7 (RFC 9562 §5.7) and the RFC variant bits. These
		// ids cross the plane boundary, so the version is a contract, not a
		// curio — persistence.md's rule is v7 for exactly these rows.
		if id[14] != '7' {
			t.Fatalf("%s id %q is not version 7", name, id)
		}
		if id[19] != '8' && id[19] != '9' && id[19] != 'a' && id[19] != 'b' {
			t.Fatalf("%s id %q lacks the RFC variant", name, id)
		}
	}
	if backend == BackendID(alias) || alias == AliasID(version) || backend == BackendID(version) {
		t.Fatalf("minted identifiers collided: %q %q %q", backend, alias, version)
	}
}

func TestNewUUIDv7EncodesTheInjectedClock(t *testing.T) {
	stamp := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	id, err := newUUIDv7(stamp)
	if err != nil {
		t.Fatalf("newUUIDv7 returned error: %v", err)
	}
	// The first 48 bits are the big-endian Unix millisecond; strip the two
	// dash-separated hex groups back into a number and compare.
	var ms int64
	packed := strings.ReplaceAll(id[:13], "-", "")
	for _, r := range packed {
		ms = ms<<4 | int64(hexDigit(t, byte(r)))
	}
	if got, want := ms, stamp.UnixMilli(); got != want {
		t.Fatalf("id %q carries millisecond %d, want %d", id, got, want)
	}
}

func TestNewUUIDv7OrdersByTime(t *testing.T) {
	earlier, err := newUUIDv7(time.UnixMilli(1_000))
	if err != nil {
		t.Fatalf("newUUIDv7(1s) returned error: %v", err)
	}
	later, err := newUUIDv7(time.UnixMilli(2_000))
	if err != nil {
		t.Fatalf("newUUIDv7(2s) returned error: %v", err)
	}
	// Time-ordering is the property version 7 was chosen for: ids minted
	// later sort after ids minted earlier, which is what keeps a B-tree on
	// these columns from rewriting itself on every insert.
	if earlier >= later {
		t.Fatalf("id minted at 1s (%q) does not sort before the id minted at 2s (%q)", earlier, later)
	}
}

func TestNewUUIDv7RejectsNothingAboutTheRandomHalf(t *testing.T) {
	// Two ids at the same instant must differ: uniqueness leans on the
	// random bits, not the clock.
	stamp := time.UnixMilli(1_000_000)
	first, err := newUUIDv7(stamp)
	if err != nil {
		t.Fatalf("first newUUIDv7 returned error: %v", err)
	}
	second, err := newUUIDv7(stamp)
	if err != nil {
		t.Fatalf("second newUUIDv7 returned error: %v", err)
	}
	if first == second {
		t.Fatalf("two ids at the same instant collided: %q", first)
	}
}

func hexDigit(t *testing.T, c byte) uint64 {
	t.Helper()
	switch {
	case c >= '0' && c <= '9':
		return uint64(c - '0')
	case c >= 'a' && c <= 'f':
		return uint64(c-'a') + 10
	default:
		t.Fatalf("%q is not a hex digit", c)
		return 0
	}
}
