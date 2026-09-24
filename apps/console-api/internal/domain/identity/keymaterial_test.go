package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
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
		t.Fatalf("token %q does not carry the public prefix %q", token, want)
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
		"short secret":         TokenBrand + "_" + segments[1] + "_" + segments[2][:secretEncodedLen-1],
		"padded secret":        TokenBrand + "_" + segments[1] + "_" + segments[2] + "=",
		"non-base64 secret":    TokenBrand + "_" + segments[1] + "_" + strings.Repeat("!", secretEncodedLen),
		"trailing newline":     goodToken + "\n",
		"leading space":        " " + goodToken,
		"interior space":       TokenBrand + "_ " + segments[1] + "_" + segments[2],
		"four underscore part": TokenBrand + "_" + segments[1] + "_" + segments[2] + "_tail",
	}
	for name, raw := range cases {
		_, _, err := ParseToken(raw)
		if !errors.Is(err, ErrMalformedToken) {
			t.Fatalf("%s: ParseToken error = %v, want ErrMalformedToken", name, err)
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
	check("%s", fmt.Sprintf("%s", secret))
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

func TestCompareAgainstDummyDigestBurnsTheComparisonWithoutMeaning(t *testing.T) {
	// The return value is deliberately meaningless — the miss-path caller
	// ignores it and reports ErrUnknownCredential. The only property worth
	// pinning is that a real secret's digest never collides with the zero
	// digest, so the dummy comparison cannot accidentally authenticate.
	for i := 0; i < 8; i++ {
		s, err := GenerateSecret()
		if err != nil {
			t.Fatalf("GenerateSecret returned error: %v", err)
		}
		if CompareAgainstDummyDigest(s) {
			t.Fatalf("a real secret compared equal to the zero digest")
		}
	}
}

func TestFormatTokenRefusesBlankIDsAndWrongSizedSecrets(t *testing.T) {
	s, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret returned error: %v", err)
	}
	if _, err := FormatToken("", s); err == nil {
		t.Fatalf("FormatToken with blank id returned no error")
	}
	short := Secret{bytes: make([]byte, SecretEntropyBytes-1)}
	if _, err := FormatToken("019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e", short); err == nil {
		t.Fatalf("FormatToken with a short secret returned no error")
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
