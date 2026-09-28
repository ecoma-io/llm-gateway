package identity

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
)

// The user credential. A console user's password is a password: it is
// chosen by a human, it is not uniformly random, and it is therefore
// exactly the case where a fast digest is the wrong tool — an offline
// attacker holding a stolen table gets one SHA-256 per guess for free.
// PBKDF2-HMAC-SHA256 is the standard library's answer and is what ADR
// 0012 §2 records: no new dependency, a cost factor that is a deliberate
// decision rather than a library's default.
//
// The salt is per-credential and per-row. Two users with the same
// password produce unrelated digests, so a table disclosure cannot be
// attacked with a single precomputed set of rainbow rows, and one row's
// digest says nothing about another row's password even where the
// passwords are identical.
const (
	// CredentialSaltBytes is the per-row salt's size. 16 bytes is
	// PBKDF2's own recommended minimum and is far past collision.
	CredentialSaltBytes = 16

	// CredentialHashBytes is the derived key's size — a full SHA-256
	// block, 32 bytes. It matches the API key path's Digest width so one
	// storage convention covers both credentials.
	CredentialHashBytes = 32

	// CredentialIterations is the production work factor, set to OWASP's
	// current recommendation for PBKDF2-HMAC-SHA256 rather than to a
	// number tuned for sign-in latency.
	//
	// The temptation is to pick something an operator does not wait for,
	// and the argument for it is usually a rate limit upstream. That
	// argument is about the wrong threat: an online guess is bounded by
	// however fast the endpoint answers, and a slow hash barely moves it.
	// The threat a work factor actually defends is OFFLINE — an attacker
	// holding a copied table, who pays nothing per guess and so is limited
	// only by the derivation cost. Rate limiting does not touch that
	// attacker at all, which is why choosing a cheap factor because the
	// endpoint is rate-limited reasons about online attacks and buys
	// nothing against the one it is defending against.
	//
	// Measured on this module's own hardware: 210,000 verifies in ~77ms and
	// 600,000 in ~218ms. The second is the number this takes, because
	// 218ms on an interactive sign-in is a cost a human waits through once
	// and an offline table pays on every row of every guess.
	//
	// The parameter is a constant here rather than configuration because
	// it is a property of the credential, not of a deployment. Raising it
	// later is a re-hash on next successful sign-in — verify against the
	// parameters the row stored, then re-derive and store the new ones —
	// which is exactly what recording the iteration count per row buys.
	CredentialIterations = 600_000

	// CredentialTestIterations is the work factor the package's own tests
	// use wherever they can reach the injection point. At the production
	// factor one verification costs ~218ms, and this suite derives and
	// verifies many times per case — the wall-time comparison alone runs
	// both a hit and a miss several times over — so leaving it in place
	// would turn a security test into a slow one and invite someone to
	// weaken it later.
	//
	// It is exported rather than test-only because the lowering is a fact
	// about the credential's API, not a private implementation detail: a
	// caller performing an offline re-hash needs the same seam. The
	// lowered factor does not make the test a different test. The
	// derivation, the constant-time comparison and the burn all run
	// identically; only the number of HMAC rounds differs, and rounds are
	// exactly the constant being varied. What would be a fake is asserting
	// the PRODUCTION cost from a test — that asserts a property of the
	// machine it ran on, which is a flaky test wearing a security
	// costume. The production constant is instead pinned by a test that
	// reads the constant, and the chosen value's justification is the
	// measured derivation cost recorded above it.
	CredentialTestIterations = 1
)

// ErrEmptyCredential reports a presented credential that is the empty
// string. It is a separate sentinel from a mismatch because the contract
// admits a password as short as one character and the distinction is
// about a malformed request rather than a wrong secret — the caller
// answers 400 for this and 401 for a mismatch, and neither answer says
// anything about whether the account exists.
var ErrEmptyCredential = errors.New("identity: credential is empty")

// CredentialDigest is the stored half of a user credential: a per-row
// salt and the PBKDF2-HMAC-SHA256 derivation of the plaintext under it.
// It is safe to persist and safe to log — the plaintext is not here, and
// a digest under a per-row salt does not let anyone present the
// credential.
type CredentialDigest struct {
	Salt       []byte
	Hash       []byte
	Iterations int
}

// NewCredentialDigest derives the stored form of a plaintext credential
// at the production work factor. The plaintext is a parameter and is not
// retained, copied into the result, or recoverable from it.
func NewCredentialDigest(plaintext string) (CredentialDigest, error) {
	return deriveCredential(plaintext, CredentialIterations)
}

// NewCredentialDigestWithIterations derives at an explicit work factor.
// It exists so a caller with its own recorded parameters — a re-hash, or
// a test that must not spend the production cost — can produce a digest
// the same function verifies. A non-positive iteration count is refused
// rather than defaulted: silently substituting production parameters for
// a caller's recorded ones would make a stored row unverifiable.
func NewCredentialDigestWithIterations(plaintext string, iterations int) (CredentialDigest, error) {
	return deriveCredential(plaintext, iterations)
}

func deriveCredential(plaintext string, iterations int) (CredentialDigest, error) {
	if plaintext == "" {
		return CredentialDigest{}, ErrEmptyCredential
	}
	if iterations <= 0 {
		return CredentialDigest{}, fmt.Errorf("identity: derive credential: iterations must be positive, got %d", iterations)
	}
	salt := make([]byte, CredentialSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return CredentialDigest{}, fmt.Errorf("identity: derive credential: %w", err)
	}
	hash, err := pbkdf2.Key(sha256.New, plaintext, salt, iterations, CredentialHashBytes)
	if err != nil {
		return CredentialDigest{}, fmt.Errorf("identity: derive credential: %w", err)
	}
	return CredentialDigest{Salt: salt, Hash: hash, Iterations: iterations}, nil
}

// burnSalt and burnHash stand in for a record that was not found. They are
// constants of this package rather than of any row, so the miss path
// derives under exactly the same cost as a hit and compares against a
// fixed-width value it can never match.
var (
	burnSalt = make([]byte, CredentialSaltBytes)
	burnHash = make([]byte, CredentialHashBytes)
)

// Verify reports whether a presented plaintext matches the stored digest.
//
// The comparison is constant time and ALWAYS runs, on every path. A digest
// that is not a complete record — the zero CredentialDigest a caller passes
// when resolution found nothing — is verified against the burn constants at
// the production work factor rather than being short-circuited to false. The
// derivation is the expensive part, so refusing to do it on a miss is
// exactly what makes a miss measurably cheaper than a hit, and that gap is
// the oracle.
func (d CredentialDigest) Verify(plaintext string) (bool, error) {
	if plaintext == "" {
		return false, ErrEmptyCredential
	}
	iterations := d.Iterations
	if iterations <= 0 {
		iterations = CredentialIterations
	}
	salt := d.Salt
	if len(salt) == 0 {
		salt = burnSalt
	}
	hash := d.Hash
	if len(hash) != CredentialHashBytes {
		hash = burnHash
	}
	derived, err := pbkdf2.Key(sha256.New, plaintext, salt, iterations, CredentialHashBytes)
	if err != nil {
		return false, fmt.Errorf("identity: verify credential: %w", err)
	}
	// subtle.ConstantTimeCompare over fixed-width inputs: the derived key
	// and the stored hash are both CredentialHashBytes, so the comparison
	// cannot leak through an early exit on length, and the branch on its
	// result happens only after the full comparison has run.
	return subtle.ConstantTimeCompare(derived, hash) == 1, nil
}

// VerifyPasswordCredential is the pre-credential policy as one call, and
// the reason it is a free function taking a digest by value rather than only
// a method.
//
// The name is deliberate: `VerifyCredential` in verification.go is the API
// KEY's whole verification policy — digest, then recorded identity, then
// key state, then account state, then a Principal. This is a different
// thing on a different credential, and a user password's answer is a single
// boolean. Overloading the one name would either make the reader guess which
// policy a call site invokes, or force the key's function to grow a branch
// for a credential it has no record of. Two functions, two names, two
// policies.
//
// Every sign-in caller has four distinct causes to report — no such
// account, no such user, a removed user, wrong credential — and a resolver
// that short-circuits ("no user row, so there is nothing to verify") makes
// three of the four measurably cheaper than the fourth. A caller willing to
// time submissions then learns which addresses exist and which users are
// removed: the existence oracle ADR 0008 §2 names, and the reason sign-in
// keeps ONE unauthenticated answer.
//
// The burn is therefore mechanical rather than a matter of care. This
// function ALWAYS performs a full PBKDF2 derivation and ALWAYS compares it
// in constant time. A caller that resolved nothing passes the zero
// CredentialDigest, which still costs the full derivation because the
// iteration count, key length and salt width are constants of this file
// rather than properties of the row it did not find. A hit and a miss differ
// only in the comparison's result, never in the work performed.
func VerifyPasswordCredential(presented string, recorded CredentialDigest) (bool, error) {
	return recorded.Verify(presented)
}
