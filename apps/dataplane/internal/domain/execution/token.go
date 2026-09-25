package execution

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
)

// The presented API key's grammar, restated on this side of the plane split.
// The Control Plane mints every token (console-api identity/keymaterial.go is
// the canonical grammar); the Data Plane never mints, it only verifies — but
// verification needs the same parse, and the planes do not import each other
// (ADR 0006), so the grammar is duplicated here and cross-pinned by
// token_test.go's fixed literal vector: one real token, minted by the Control
// Plane's FormatToken, whose key id and secret digest are hard-coded beside
// it. A grammar that drifts on either side fails that test instead of
// failing an operator's key silently.
//
//	gw_<key-id>_<secret>
//
// <key-id> is the key's UUID in its canonical 36-character spelling, and
// <secret> is the unpadded base64url rendering of 32 crypto/rand bytes — 43
// characters. SHA-256 of the raw secret bytes is the digest a credential
// projection stores, rendered lowercase hex (the mirror column's own CHECK
// pins that rendering); SecretDigest is this side's helper for it.
const (
	tokenBrand         = "gw"
	secretEntropyBytes = 32
	// secretEncodedLen is base64.RawURLEncoding.EncodedLen(secretEntropyBytes):
	// 32 raw bytes encode to exactly 43 characters unpadded. ParseToken checks
	// it so a truncated or padded secret fails closed.
	secretEncodedLen = 43
	// apiKeyIDLen is a canonical uuid's length; apiKeyIDVersionIndex and
	// apiKeyIDVariantIndex are the two nibbles the RFC pins inside it.
	apiKeyIDLen          = 36
	apiKeyIDVersionIndex = 14
	apiKeyIDVariantIndex = 19
)

// ErrMalformedToken is every parse failure: wrong brand, wrong segment count,
// bad key id, non-canonical base64, wrong secret length, or any whitespace
// anywhere in the presented value. One sentinel with one byte-identical text
// is the point — an error echoed to a log or an API response must not be
// turned into a probe that tells which stage of the grammar a value reached.
// This is the Control Plane's ParseToken discipline, restated for the runtime
// hot path.
var ErrMalformedToken = errors.New("identity: malformed token")

// ParseToken is the fail-closed front door of verification: it takes the
// presented bearer value apart or refuses it whole. It returns the key id as
// it appears in the token and the raw secret bytes for digesting — never the
// presented text — and every failure path returns ErrMalformedToken itself,
// with no per-check detail.
//
// The split is SplitN on "_" with a limit of 3 because the secret's base64url
// alphabet itself contains underscores; the third segment may legally hold any
// number of them. The decode is base64's strict mode: a canonical encoding is
// required, so one credential has exactly one spelling — the non-strict
// decoder would accept four, and four textual aliases of one token defeat any
// audit or rate limit keyed on the presented string.
func ParseToken(raw string) (keyID string, secret []byte, err error) {
	// Whitespace anywhere is disqualification, before anything else: a token
	// pasted with a trailing newline must not be rescued by a trim (trimming
	// would make acceptance depend on the padding, and a verifier that accepts
	// "token " and "token" differently is a probe).
	if strings.IndexFunc(raw, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
	}) >= 0 {
		return "", nil, ErrMalformedToken
	}
	parts := strings.SplitN(raw, "_", 3)
	if len(parts) != 3 {
		return "", nil, ErrMalformedToken
	}
	if parts[0] != tokenBrand {
		return "", nil, ErrMalformedToken
	}
	if err := validateTokenKeyID(parts[1]); err != nil {
		return "", nil, ErrMalformedToken
	}
	encoded := parts[2]
	if len(encoded) != secretEncodedLen {
		return "", nil, ErrMalformedToken
	}
	rawSecret, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return "", nil, ErrMalformedToken
	}
	if len(rawSecret) != secretEntropyBytes {
		return "", nil, ErrMalformedToken
	}
	return parts[1], rawSecret, nil
}

// SecretDigest renders the SHA-256 of a presented secret's raw bytes as
// lowercase hex — the exact text form the credential projection stores and
// the only form the runtime ever compares. The digest is not credential
// material: knowing it does not let anyone present the key, which is why it
// may sit in the mirror while the secret may not.
func SecretDigest(secret []byte) string {
	sum := sha256.Sum256(secret)
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, 2*sha256.Size)
	for _, b := range sum {
		out = append(out, hexDigits[b>>4], hexDigits[b&0x0f])
	}
	return string(out)
}

// EqualDigests compares two lowercase-hex digests in constant time. Both
// callers hold fixed-width values — the presented secret's digest this side
// computes, and the mirror column whose CHECK pins 64 hex characters — so the
// equal-length path is the real path; the unequal-length path burns a
// constant-time compare on dummies anyway, so the branch on length cannot
// become a timing oracle about how wrong a presented credential was. An empty
// digest matches nothing: a mirror row without its digest is not a credential
// that could authenticate, whatever the presented bytes are.
func EqualDigests(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if len(a) != len(b) {
		subtle.ConstantTimeCompare([]byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
			[]byte("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"))
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// validateTokenKeyID checks the canonical 8-4-4-4-12 lowercase-hex shape,
// including RFC 4122's version nibble (4) and variant bits (one of 8, 9, a,
// b). This is the mint-side grammar check's twin, kept private: on the
// presentation path its detail is discarded and the caller reads
// ErrMalformedToken, so the detail exists only to keep this function honest
// about what it checked.
func validateTokenKeyID(s string) error {
	if len(s) != apiKeyIDLen {
		return errors.New("api key id must be 36 characters")
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return errors.New("api key id dashes are misplaced")
			}
		case apiKeyIDVersionIndex:
			if r != '4' {
				return errors.New("api key id is not a version 4 uuid")
			}
		case apiKeyIDVariantIndex:
			if r != '8' && r != '9' && r != 'a' && r != 'b' {
				return errors.New("api key id is not an rfc 4122 variant")
			}
		default:
			if r < '0' || (r > '9' && r < 'a') || r > 'f' {
				return errors.New("api key id is not lowercase hex")
			}
		}
	}
	return nil
}
