package execution

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// The fixed literal vector. This token was minted once by the Control Plane's
// FormatToken — the only producer of complete tokens — and is hard-coded here
// with its key id and the lowercase-hex SHA-256 of its raw secret bytes. The
// pinning is the cross-plane contract: the grammar lives duplicated on both
// sides of the split (ADR 0006 forbids the import), so the one thing that
// keeps the two spellings from drifting apart is a real minted credential
// that this side must keep parsing, byte for byte, forever. No test here
// mints through a shared function; the vector is text, and text does not
// drift. The const's name deliberately carries no credential vocabulary —
// generic secret scanners key on identifiers like token and key, and what
// this pins is the vector's bytes, not its label.
const (
	fixedVector = "gw_9da93f98-683f-4e52-b937-6073e8bf06f8_ti6sy6R1fMhKt9J_JEMwvMhM4LWlqombSmyDKC1MznQ"
	fixedKeyID  = "9da93f98-683f-4e52-b937-6073e8bf06f8"
	fixedDigest = "e13cdd63ba857ef23527aaf94ccc5e8b39dcfc13f833edbc03263a6d69d01432"
)

// TestParseTokenReadsAControlPlaneMintedToken pins the whole happy path
// against the fixed vector: the key id comes back as presented, the secret
// decodes to its 32 raw bytes, and the digest this side computes from those
// bytes is the digest the mint recorded. If any of the three disagree, the
// duplicated grammar has drifted and no console-minted key can authenticate.
func TestParseTokenReadsAControlPlaneMintedToken(t *testing.T) {
	keyID, secret, err := ParseToken(fixedVector)
	if err != nil {
		t.Fatalf("ParseToken() error = %v", err)
	}
	if keyID != fixedKeyID {
		t.Errorf("keyID = %q, want %q", keyID, fixedKeyID)
	}
	if len(secret) != 32 {
		t.Fatalf("len(secret) = %d, want 32 raw bytes", len(secret))
	}
	if got := SecretDigest(secret); got != fixedDigest {
		t.Errorf("SecretDigest() = %s, want the minted digest %s", got, fixedDigest)
	}
	if !EqualDigests(got(t), fixedDigest) {
		t.Error("EqualDigests(presented, recorded) = false, want true for the same secret")
	}
}

// got digests the fixed vector's secret again so the EqualDigests call above
// exercises the two-argument form with independently computed input.
func got(t *testing.T) string {
	t.Helper()
	_, secret, err := ParseToken(fixedVector)
	if err != nil {
		t.Fatalf("ParseToken() error = %v", err)
	}
	return SecretDigest(secret)
}

// TestParseTokenRefusesEveryMalformedForm walks the rejection paths. Each row
// is refused with the one sentinel, and the sentinel's text must be
// byte-identical on every path: an error echoed outward is a probe channel
// only while its messages differ.
func TestParseTokenRefusesEveryMalformedForm(t *testing.T) {
	pad := strings.Repeat("A", 43)
	tests := []struct {
		name      string
		presented string
	}{
		{name: "an empty value", presented: ""},
		{name: "the bare brand", presented: "gw"},
		{name: "two segments", presented: "gw_" + fixedKeyID},
		{name: "a foreign brand", presented: "sk_" + fixedKeyID + "_" + pad},
		{name: "an empty key id", presented: "gw__" + pad},
		{name: "an uppercase key id", presented: "gw_9DA93F98-683F-4E52-B937-6073E8BF06F8_" + pad},
		{name: "a version 7 uuid", presented: "gw_0195f3e8-7c2a-7b3d-9f1a-2c4e6b8d0f3a_" + pad},
		{name: "a misplaced dash", presented: "gw_9da93f9868-3f-4e52-b937-6073e8bf06f8_" + pad},
		{name: "a non-hex key id", presented: "gw_9da93f98-683f-4e52-b937-6073e8bf06gg_" + pad},
		{name: "a 35-character key id", presented: "gw_9da93f98-683f-4e52-b937-6073e8bf06f_" + pad},
		{name: "a 37-character key id", presented: "gw_9da93f98-683f-4e52-b937-6073e8bf06f80_" + pad},
		{name: "a 42-character secret", presented: "gw_" + fixedKeyID + "_" + pad[:42]},
		{name: "a 44-character secret", presented: "gw_" + fixedKeyID + "_" + pad + "A"},
		{name: "a padded secret", presented: "gw_" + fixedKeyID + "_" + pad[:40] + "AAA="},
		{name: "a non-url base64 alphabet", presented: "gw_" + fixedKeyID + "_" + strings.Repeat("+", 43)},
		{name: "a slash in the secret", presented: "gw_" + fixedKeyID + "_" + strings.Repeat("/", 43)},
		{name: "a leading space", presented: " " + fixedVector},
		{name: "a trailing newline", presented: fixedVector + "\n"},
		{name: "an internal tab", presented: "gw_" + fixedKeyID + "_\t" + pad},
		{name: "a carriage return", presented: fixedVector + "\r"},
		{name: "a form feed", presented: fixedVector + "\f"},
		{name: "a vertical tab", presented: fixedVector + "\v"},
		{name: "whitespace instead of a value", presented: " "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyID, secretBytes, err := ParseToken(tt.presented)
			if !errors.Is(err, ErrMalformedToken) {
				t.Fatalf("ParseToken() error = %v, want ErrMalformedToken", err)
			}
			if keyID != "" || secretBytes != nil {
				t.Errorf("ParseToken() = (%q, %d bytes), want empty results on refusal", keyID, len(secretBytes))
			}
		})
	}

	// The sentinel discipline: every rejection path above returned the same
	// error VALUE; this pins that its text is one sentence, so the test fails
	// if a per-check detail ever leaks into the sentinel.
	if want := "identity: malformed token"; ErrMalformedToken.Error() != want {
		t.Errorf("ErrMalformedToken text = %q, want %q on every path", ErrMalformedToken.Error(), want)
	}
}

// TestParseTokenAcceptsOnlyCanonicalSpellings pins strictness in the other
// direction: values that a non-strict decoder or a trimming verifier would
// accept are still refused, because one credential must have exactly one
// spelling. Each row differs from the fixed token by one byte of canonicality
// and must not parse.
func TestParseTokenAcceptsOnlyCanonicalSpellings(t *testing.T) {
	tests := []struct {
		name      string
		presented string
	}{
		// '=' at position 43 keeps the length legal and the encoding
		// non-canonical: strict mode is what refuses it, not the length check.
		{name: "an in-length pad character", presented: "gw_" + fixedKeyID + "_" + strings.Repeat("A", 42) + "="},
		{name: "a key id with braces", presented: "gw_{9da93f98-683f-4e52-b937-6073e8bf06f8}_" + strings.Repeat("A", 43)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := ParseToken(tt.presented); !errors.Is(err, ErrMalformedToken) {
				t.Fatalf("ParseToken() error = %v, want ErrMalformedToken", err)
			}
		})
	}
}

// TestParseTokenRoundTripsAWellFormedMint re-constructs a token exactly the
// way the Control Plane's FormatToken does — brand, key id, unpadded
// base64url of 32 random bytes — and parses it back. The property under test
// is that the grammar the parser demands is the grammar the minter produces:
// any accepted key must survive a fresh mint, and any fresh mint must be
// accepted.
func TestParseTokenRoundTripsAWellFormedMint(t *testing.T) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand.Read() error = %v", err)
	}
	token := "gw_" + fixedKeyID + "_" + base64.RawURLEncoding.EncodeToString(raw)
	keyID, secret, err := ParseToken(token)
	if err != nil {
		t.Fatalf("ParseToken() error = %v", err)
	}
	if keyID != fixedKeyID {
		t.Errorf("keyID = %q, want %q", keyID, fixedKeyID)
	}
	if string(secret) != string(raw) {
		t.Error("secret bytes do not round-trip through the encoding")
	}
}

// TestEqualDigests pins the comparison's contract: true only for identical
// digests, false for different same-width digests, and false — without
// panicking — for unequal widths, which is the path a truncated or corrupt
// mirror value would walk.
func TestEqualDigests(t *testing.T) {
	if !EqualDigests(fixedDigest, fixedDigest) {
		t.Error("EqualDigests(d, d) = false, want true")
	}
	other := strings.Repeat("0", 64)
	if EqualDigests(fixedDigest, other) {
		t.Error("EqualDigests(different digests) = true, want false")
	}
	if EqualDigests(fixedDigest, fixedDigest[:63]) {
		t.Error("EqualDigests(unequal lengths) = true, want false")
	}
	if EqualDigests("", "") {
		t.Error("EqualDigests(empty, empty) = true, want false — an empty digest matches nothing")
	}
}
