package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// mint is the canonical round-trip used across these tests: draw a secret,
// format it into a token under a fixed id, and return all three views.
func mint(t *testing.T, id APIKeyID) (Secret, string, Digest) {
	t.Helper()
	s, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret returned error: %v", err)
	}
	token, err := FormatToken(id, s)
	if err != nil {
		t.Fatalf("FormatToken returned error: %v", err)
	}
	return s, token, s.Digest()
}

func TestSecretRoundTripsThroughTokenFormatAndParse(t *testing.T) {
	const id = APIKeyID("019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e")
	secret, token, _ := mint(t, id)
	if want := "gw_" + string(id) + "_"; !strings.HasPrefix(token, want) {
		// The prefix is printed, never the token: a failure message is still
		// output, and the whole point of the grammar is that the secret
		// segment is nobody's output.
		t.Fatalf("minted token does not carry the public prefix %q", want)
	}
	parsedID, parsedSecret, err := ParseToken(token)
	if err != nil {
		t.Fatalf("ParseToken rejected a freshly minted token: %v", err)
	}
	if parsedID != id {
		t.Fatalf("ParseToken id = %q, want %q", parsedID, id)
	}
	if !EqualDigests(parsedSecret.Digest(), secret.Digest()) {
		t.Fatalf("parsed secret digest differs from the minted secret's digest")
	}
}

func TestGeneratedSecretsAreUniqueAndCorrectlySized(t *testing.T) {
	seen := make(map[Digest]bool)
	for i := 0; i < 64; i++ {
		s, err := GenerateSecret()
		if err != nil {
			t.Fatalf("GenerateSecret returned error: %v", err)
		}
		d := s.Digest()
		if seen[d] {
			t.Fatalf("GenerateSecret repeated a secret at iteration %d", i)
		}
		seen[d] = true
		token, err := FormatToken("019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e", s)
		if err != nil {
			t.Fatalf("FormatToken returned error: %v", err)
		}
		if len(token) != 3+36+1+secretEncodedLen {
			t.Fatalf("token length = %d, want %d", len(token), 3+36+1+secretEncodedLen)
		}
	}
}

func TestParseTokenFailsClosedOnEveryMalformedShape(t *testing.T) {
	_, goodToken, _ := mint(t, "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e")
	segments := strings.SplitN(goodToken, "_", 3)
	cases := map[string]string{
		"empty":                "",
		"no separators":        "gwarbitrary",
		"wrong brand":          "sk_" + segments[1] + "_" + segments[2],
		"uppercase brand":      "GW_" + segments[1] + "_" + segments[2],
		"two parts":            TokenBrand + "_" + segments[1],
		"blank id":             TokenBrand + "__" + segments[2],
		"non-uuid id":          TokenBrand + "_not-a-uuid_" + segments[2],
		"uppercase uuid":       TokenBrand + "_" + strings.ToUpper(segments[1]) + "_" + segments[2],
		"version 1 uuid":       TokenBrand + "_019203d0-9a1b-1c2a-8f1e-3f5a6b7c8d9e_" + segments[2],
		"non-rfc 4122 variant": TokenBrand + "_019203d0-9a1b-4c2a-cf1e-3f5a6b7c8d9e_" + segments[2],
		"short secret":         TokenBrand + "_" + segments[1] + "_" + segments[2][:secretEncodedLen-1],
		"padded secret":        TokenBrand + "_" + segments[1] + "_" + segments[2] + "=",
		"non-base64 secret":    TokenBrand + "_" + segments[1] + "_" + strings.Repeat("!", secretEncodedLen),
		"trailing newline":     goodToken + "\n",
		"leading space":        " " + goodToken,
		"interior space":       TokenBrand + "_ " + segments[1] + "_" + segments[2],
		"four underscore part": TokenBrand + "_" + segments[1] + "_" + segments[2] + "_tail",
	}
	text := ""
	for name, raw := range cases {
		_, _, err := ParseToken(raw)
		if !errors.Is(err, ErrMalformedToken) {
			t.Fatalf("%s: ParseToken error = %v, want ErrMalformedToken", name, err)
		}
		// The sentinel identity is half the contract; the other half is that
		// the TEXT is one and the same on every rejection path. An error that
		// varies by check is a parsing oracle the first time a transport
		// echoes it.
		if text == "" {
			text = err.Error()
			continue
		}
		if err.Error() != text {
			t.Fatalf("%s: error text %q differs from the first shape's %q — the text is a parsing oracle", name, err.Error(), text)
		}
	}
}

func TestParseTokenRequiresACanonicalSecretSegment(t *testing.T) {
	// The final base64url quantum of a 32-byte secret carries two unused
	// bits, and the non-strict decoder would accept all four characters that
	// encode them — one credential, four spellings, and every audit or rate
	// limit keyed on the presented string defeated by a typo. Strict decoding
	// accepts exactly the spelling FormatToken emits.
	_, token, _ := mint(t, "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e")
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	segment := strings.SplitN(token, "_", 3)[2]
	last := segment[len(segment)-1]
	idx := strings.IndexByte(alphabet, last)
	if idx < 0 || idx&0b11 != 0 {
		t.Fatalf("test assumption broken: canonical last character %q does not end in zero unused bits", string(last))
	}
	for delta := 1; delta <= 3; delta++ {
		alias := segment[:len(segment)-1] + string(alphabet[idx|delta])
		if _, _, err := ParseToken(strings.Replace(token, segment, alias, 1)); !errors.Is(err, ErrMalformedToken) {
			t.Fatalf("ParseToken accepted a non-canonical secret segment spelling")
		}
	}
}

func TestParseTokenAcceptsUnderscoresInsideTheSecret(t *testing.T) {
	// The base64url alphabet contains '_' and the id contains none, so the
	// grammar must split at most twice. Draw secrets until one encodes with
	// an underscore (near-certain within a handful of draws) and require the
	// token to still parse.
	var token string
	for i := 0; i < 1000; i++ {
		s, err := GenerateSecret()
		if err != nil {
			t.Fatalf("GenerateSecret returned error: %v", err)
		}
		token, err = FormatToken("019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e", s)
		if err != nil {
			t.Fatalf("FormatToken returned error: %v", err)
		}
		segment := strings.SplitN(token, "_", 3)[2]
		if strings.Contains(segment, "_") {
			break
		}
		if i == 999 {
			t.Fatalf("1000 secrets and none encoded an underscore; test lost its point")
		}
	}
	if _, _, err := ParseToken(token); err != nil {
		t.Fatalf("ParseToken rejected an underscore-bearing secret: %v", err)
	}
}

func TestSecretRedactsEveryAccidentalPrintingPath(t *testing.T) {
	secret, token, _ := mint(t, "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e")
	secretSegment := strings.SplitN(token, "_", 3)[2]

	check := func(name, rendered string) {
		t.Helper()
		if strings.Contains(rendered, secretSegment) {
			t.Fatalf("%s rendered the secret: %q", name, rendered)
		}
		if !strings.Contains(rendered, "redacted") {
			t.Fatalf("%s rendered %q without the redaction marker", name, rendered)
		}
	}
	check("%v", fmt.Sprintf("%v", secret))
	check("%+v", fmt.Sprintf("%+v", secret))
	check("%s", fmt.Sprintf("%s", secret)) //nolint:staticcheck // the fmt dispatch, not the method call, is the subject under test
	check("%q", fmt.Sprintf("%q", secret))
	check("GoString", fmt.Sprintf("%#v", secret))
	check("String method", secret.String())

	if _, err := secret.MarshalText(); err == nil {
		t.Fatalf("MarshalText succeeded; a serialiser smuggled the secret")
	}
}

func TestDigestIsDeterministicAndHexRenders(t *testing.T) {
	secret, _, digest := mint(t, "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e")
	if !EqualDigests(digest, secret.Digest()) {
		t.Fatalf("recomputed digest differs")
	}
	// The digest is a plain SHA-256 of the raw bytes — prove it against the
	// standard library directly so the derivation can never drift.
	raw, err := base64.RawURLEncoding.DecodeString(strings.SplitN(
		mustToken(t, secret), "_", 3)[2])
	if err != nil {
		t.Fatalf("test could not decode its own token: %v", err)
	}
	if want := sha256.Sum256(raw); digest != want {
		t.Fatalf("Digest = %x, want sha256 of raw secret %x", digest, want)
	}
	if len(digest.Hex()) != 2*sha256.Size {
		t.Fatalf("Hex length = %d, want %d", len(digest.Hex()), 2*sha256.Size)
	}
}

func TestEqualDigestsDistinguishesOnlyEqualDigests(t *testing.T) {
	a, _, da := mint(t, "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e")
	_, _, db := mint(t, "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e")
	if !EqualDigests(da, da) {
		t.Fatalf("EqualDigests rejected a digest compared with itself")
	}
	if EqualDigests(da, db) {
		t.Fatalf("EqualDigests accepted two independent secrets")
	}
	// A fresh decode of the same secret matches: comparison is by content.
	parsedID, parsed, err := ParseToken(mustToken(t, a))
	if err != nil {
		t.Fatalf("ParseToken returned error: %v", err)
	}
	_ = parsedID
	if !EqualDigests(parsed.Digest(), da) {
		t.Fatalf("re-parsed secret's digest differs from the original")
	}
}

func TestFormatTokenRefusesIDsOutsideTheTokenGrammar(t *testing.T) {
	// Every id FormatToken accepts must produce a token ParseToken accepts:
	// a minted credential that could never authenticate would fail at
	// exactly the moment an operator is copying it somewhere safe.
	s, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret returned error: %v", err)
	}
	for name, id := range map[string]APIKeyID{
		"blank":        "",
		"friendly":     "key-1",
		"uppercase":    "019203D0-9A1B-4C2A-8F1E-3F5A6B7C8D9E",
		"version 1":    "019203d0-9a1b-1c2a-8f1e-3f5a6b7c8d9e",
		"nil uuid":     "00000000-0000-0000-0000-000000000000",
		"all variants": "ffffffff-ffff-4fff-ffff-ffffffffffff",
	} {
		if _, err := FormatToken(id, s); err == nil {
			t.Fatalf("%s: FormatToken accepted an id outside the token grammar", name)
		}
	}
	short := Secret{bytes: make([]byte, SecretEntropyBytes-1)}
	if _, err := FormatToken("019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e", short); err == nil {
		t.Fatalf("FormatToken with a short secret returned no error")
	}
}

func TestNewAPIKeyRefusesIDsOutsideTheTokenGrammar(t *testing.T) {
	// The ownership record's prefix is derived from the id; a record minted
	// with a non-grammar id would carry a prefix the database's own shape
	// check refuses and a token nobody could ever present.
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	for name, id := range map[string]APIKeyID{
		"blank":     "",
		"friendly":  "deploy-key",
		"version 0": "a0000000-0000-0000-8000-0000000000a1",
	} {
		if _, err := NewAPIKey(id, "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e", "", "probe", now); err == nil {
			t.Fatalf("%s: NewAPIKey accepted an id outside the token grammar", name)
		}
	}
}

// mustToken formats a secret under a fixed id or fails the test.
func mustToken(t *testing.T, s Secret) string {
	t.Helper()
	token, err := FormatToken("019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e", s)
	if err != nil {
		t.Fatalf("FormatToken returned error: %v", err)
	}
	return token
}
