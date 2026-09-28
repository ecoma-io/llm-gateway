package postgres

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
)

// decodeCredentialColumns is the one place the three nullable credential
// columns become the aggregate's digest, and it has exactly three outcomes:
// an absent credential, a decoded one, and a refusal. The refusal branches are
// the ones worth testing here, because each is a way the stored value could
// be unusable and a decoder that guessed instead of refusing would hand a
// sign-in a digest built from whatever it could parse.
//
// This is a pure function over three values, so it is tested directly rather
// than through the fake driver — the driver in this package refuses to return
// rows, so a test through it could not reach any of these branches. That the
// query really selects these columns in this shape is the integration tier's
// subject, and it is there that a column that does not exist is caught.

func TestDecodeCredentialColumnsPassesAbsence(t *testing.T) {
	// The `invited` row. All three absent is the only absence the schema
	// permits, and a nil here is what tells the caller the row has no
	// credential to check rather than a credential that failed to check.
	digest, err := decodeCredentialColumns(nil, nil, nil)
	if err != nil {
		t.Fatalf("an all-absent credential is a legitimate invited row, not an error: %v", err)
	}
	if digest != nil {
		t.Errorf("decodeCredentialColumns(all absent) = %+v, want nil: a nil digest is how a caller tells 'no credential' from 'wrong credential'", digest)
	}
}

func TestDecodeCredentialColumnsDecodesAWholeCredential(t *testing.T) {
	// 32 bytes of hash and 16 of salt, which is what the schema's shape
	// checks require — the widths are not this function's business, but a
	// fixture at the real widths is what makes the hex round-trip meaningful.
	hash := make([]byte, identity.CredentialHashBytes)
	salt := make([]byte, identity.CredentialSaltBytes)
	for i := range hash {
		hash[i] = byte(i)
	}
	for i := range salt {
		salt[i] = byte(255 - i)
	}
	const iterations = 600_000

	digest, err := decodeCredentialColumns(
		ptr(hex.EncodeToString(hash)),
		ptr(hex.EncodeToString(salt)),
		ptr(iterations),
	)
	if err != nil {
		t.Fatalf("decoding a well-formed credential: %v", err)
	}
	if digest == nil {
		t.Fatal("a credential with all three columns set decoded to nil")
	}
	if got := hex.EncodeToString(digest.Hash); got != hex.EncodeToString(hash) {
		t.Errorf("hash = %s, want %s", got, hex.EncodeToString(hash))
	}
	if got := hex.EncodeToString(digest.Salt); got != hex.EncodeToString(salt) {
		t.Errorf("salt = %s, want %s", got, hex.EncodeToString(salt))
	}
	if digest.Iterations != iterations {
		t.Errorf("iterations = %d, want %d: the recorded work factor is what a later raise re-derives under, so it must survive the round trip", digest.Iterations, iterations)
	}
}

func TestDecodeCredentialColumnsRefusesAHalfCredential(t *testing.T) {
	// The schema's whole-or-absent CHECK means a real database cannot produce
	// any of these. They are tested anyway because this function is the last
	// place that could turn a partial row into a usable digest, and a caller
	// that trusted a nil here would report a wrong password for a row whose
	// credential is missing — a user who can never sign in and never finds out
	// why.
	cases := []struct {
		name       string
		hash       *string
		salt       *string
		iterations *int
	}{
		{"hash alone", ptr(strings.Repeat("a", 64)), nil, nil},
		{"hash and salt, no iterations", ptr(strings.Repeat("a", 64)), ptr(strings.Repeat("b", 32)), nil},
		{"iterations alone", nil, nil, ptr(600_000)},
		{"salt and iterations, no hash", nil, ptr(strings.Repeat("b", 32)), ptr(600_000)},
		{"no salt", ptr(strings.Repeat("a", 64)), nil, ptr(600_000)},
		{"no hash", nil, ptr(strings.Repeat("b", 32)), ptr(600_000)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			digest, err := decodeCredentialColumns(c.hash, c.salt, c.iterations)
			if err == nil {
				t.Fatalf("a half credential decoded to %+v instead of being refused: users_credential_whole forbids this row, and a digest assembled from part of one is a credential nobody can verify", digest)
			}
			if digest != nil {
				t.Errorf("a refused half credential also returned a digest %+v; the refusal must be the whole answer", digest)
			}
		})
	}
}

func TestDecodeCredentialColumnsRefusesAValueThatIsNotHex(t *testing.T) {
	// The shape checks in the schema make this unreachable from a database
	// this schema created, for the same reason as above: the last reason to
	// reach for it is a row written by something else, and a decoder that
	// accepted a non-hex hash would either fail later with a far less
	// specific error or, worse, verify against empty bytes.
	cases := []struct {
		name string
		hash *string
		salt *string
	}{
		{"hash is not hex", ptr("not-hex-at-all-but-the-right-rough-length-xx"), ptr(strings.Repeat("b", 32))},
		{"salt is not hex", ptr(strings.Repeat("a", 64)), ptr("zzzz-not-hex")},
		{"hash is empty", ptr(""), ptr(strings.Repeat("b", 32))},
		// A SHORT value is hex, and that is what makes it worth its own case:
		// it decodes without error and would otherwise become a digest of the
		// wrong width, which the verifier silently replaces with its burn
		// constant. So a truncated row would read as a wrong password forever
		// instead of as the corruption it is.
		{"hash is short but valid hex", ptr(strings.Repeat("a", 62)), ptr(strings.Repeat("b", 32))},
		{"salt is short but valid hex", ptr(strings.Repeat("a", 64)), ptr(strings.Repeat("b", 30))},
		{"salt is empty", ptr(strings.Repeat("a", 64)), ptr("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			digest, err := decodeCredentialColumns(c.hash, c.salt, ptr(600_000))
			if err == nil {
				t.Fatalf("a credential whose stored value is not hex decoded to %+v instead of being refused", digest)
			}
			if digest != nil {
				t.Errorf("a refused malformed credential also returned a digest %+v", digest)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
