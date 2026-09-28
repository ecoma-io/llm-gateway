package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// DigestFromHex is the inverse of Hex, and the two properties worth pinning
// are that it round-trips every digest and that it refuses everything that is
// not the ONE spelling Hex produces. The second is the stricter claim and the
// more useful one: a decoder that accepts more than the encoder writes is a
// place two different texts can name one value, and the value being decoded
// here is what a session lookup is keyed on.

func TestDigestFromHexRoundTripsEveryDigest(t *testing.T) {
	// Two digests, deliberately different in every byte, because a decoder
	// that silently dropped a byte would pass a test whose fixture was
	// constant.
	for _, seed := range [][]byte{
		{},
		{0x00, 0xff, 0x10, 0x0f},
		ramp(),
	} {
		var d Digest
		copy(d[:], seed)

		encoded := d.Hex()
		decoded, err := DigestFromHex(encoded)
		if err != nil {
			t.Fatalf("DigestFromHex(%s): %v", encoded, err)
		}
		if !EqualDigests(decoded, d) {
			t.Errorf("round trip changed the digest: %s became %s", encoded, decoded.Hex())
		}
	}
}

func TestDigestFromHexRefusesEveryOtherSpelling(t *testing.T) {
	var d Digest
	d[0] = 0xab
	d[31] = 0xcd
	canonical := d.Hex()

	cases := []struct {
		name  string
		value string
	}{
		// The case the strictness exists for. hex.DecodeString accepts it and
		// this function must not: a stored digest is written by Hex and read
		// back by this, and if a writer ever produced uppercase the two
		// spellings would compare unequal as text while naming one value.
		{"uppercase", strings.ToUpper(canonical)},
		// A short digest is the dangerous one. Padded to Digest's width it
		// would match a DIFFERENT session's row, so a decoder that accepted a
		// short value and zero-filled the rest would be a lookup answering
		// "yes" for the wrong row.
		{"one character short", canonical[:len(canonical)-1]},
		{"one character long", canonical + "0"},
		{"empty", ""},
		{"not hex at all", strings.Repeat("z", 64)},
		{"hex with a space", canonical[:32] + " " + canonical[33:]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			decoded, err := DigestFromHex(c.value)
			if err == nil {
				t.Fatalf("DigestFromHex(%q) = %s, want a refusal: a stored digest has exactly one spelling and this is not it", c.value, decoded.Hex())
			}
			if !errors.Is(err, ErrDigestShape) {
				t.Errorf("the refusal is %v, want one wrapping ErrDigestShape so a caller can classify it", err)
			}
			if decoded != (Digest{}) {
				t.Errorf("a refused value also returned the digest %s; the refusal must be the whole answer", decoded.Hex())
			}
		})
	}
}

// TestDigestFromHexAgreesWithTheColumnShape pins the agreement this function
// and the session table's CHECK both depend on: a value the column accepts is
// a value the decoder reads back. If the two ever drifted — the column
// loosened, or the decoder tightened — a row could be written that this plane
// can never resolve again, which is a session that can never be signed out.
func TestDigestFromHexAgreesWithTheColumnShape(t *testing.T) {
	var d Digest
	copy(d[:], ramp())
	encoded := d.Hex()

	// What sessions_token_hash_shape says: lowercase hex of exactly 64.
	columnAccepts := func(s string) bool {
		if len(s) != 2*sha256.Size {
			return false
		}
		for _, c := range []byte(s) {
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				return false
			}
		}
		return true
	}

	if !columnAccepts(encoded) {
		t.Fatalf("Hex() produced %q, which the column's own shape check would reject: the encoder and the schema disagree", encoded)
	}
	if _, err := DigestFromHex(encoded); err != nil {
		t.Fatalf("the column accepts %q but DigestFromHex refuses it: a row this schema admits must be readable", encoded)
	}

	// And the reverse, over the two spellings that differ.
	if columnAccepts(strings.ToUpper(encoded)) {
		t.Error("the shape check would accept an uppercase digest, which DigestFromHex refuses — the two must agree")
	}
}

// ramp materialises a byte sequence covering every value, so a test can prove
// the decoder carries every byte through rather than the first few or the
// last few.
func ramp() []byte {
	out := make([]byte, sha256.Size)
	for i := range out {
		out[i] = byte(i)
	}
	return out
}

func TestDigestHexIsLowercaseAndFixedWidth(t *testing.T) {
	var d Digest
	for i := range d {
		d[i] = byte(i * 7)
	}
	encoded := d.Hex()
	if len(encoded) != 2*sha256.Size {
		t.Errorf("Hex() is %d characters, want %d", len(encoded), 2*sha256.Size)
	}
	if got := hex.EncodeToString(d[:]); got != encoded {
		t.Errorf("Hex() = %s, but encoding/hex says %s: the canonical form must not depend on which encoder wrote it", encoded, got)
	}
}
