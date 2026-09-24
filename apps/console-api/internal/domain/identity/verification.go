package identity

import "fmt"

// Credential is the recorded half of a verification: what a credential
// store knows about one key. In the end-state architecture the store is the
// Data Plane's credential record (digest, runtime state) joined with the
// Control Plane's ownership record (account, account state); in B2 neither
// record for the digest side exists yet, so the application layer declares
// the lookup as a port and the tests drive it with the values minting
// produced. The digest lives here as an opaque [32]byte for exactly the
// life of one verification call — it is never persisted by this package.
type Credential struct {
	KeyID APIKeyID
	// Digest is the recorded SHA-256 of the key's secret.
	Digest Digest
	// KeyState is the key's lifecycle state at verification time.
	KeyState APIKeyState
	// AccountID and AccountState are the owning account and its state.
	AccountID    AccountID
	AccountState AccountState
}

// VerifyCredential is the whole verification policy, in order:
//
//  1. Constant-time digest equality — always executed, on every path, so
//     wall time never reveals whether a key id exists. A mismatch and a
//     missing record are the same sentinel (ErrUnknownCredential).
//  2. Key state — a matched secret still does not authenticate a revoked
//     key. Revocation must survive a correct secret.
//  3. Account state — the runtime contract (request-lifecycle steps 1–2)
//     requires the key active AND the account active, with distinct
//     outcomes for suspended and closed accounts.
//  4. Only then does a Principal exist.
//
// The order is load-bearing: authentication (step 1) precedes authorisation
// (steps 2–3), and no step after the first can be reached with a mismatched
// secret. The function is pure — same inputs, same verdict, no clock, no I/O
// — so the Data Plane's hot path can mirror it verbatim against its own
// records.
func VerifyCredential(presentedID APIKeyID, presented Secret, recorded Credential) (Principal, error) {
	matched := EqualDigests(recorded.Digest, presented.Digest())
	if !matched {
		return Principal{}, fmt.Errorf("identity: verify credential %s: %w", presentedID, ErrUnknownCredential)
	}
	switch recorded.KeyState {
	case APIKeyRevoked:
		return Principal{}, fmt.Errorf("identity: verify credential %s: %w", presentedID, ErrKeyRevoked)
	case APIKeyActive:
		// Fall through to the account checks.
	default:
		return Principal{}, fmt.Errorf("identity: verify credential %s: %w: unknown key state %q", presentedID, ErrUnknownCredential, recorded.KeyState)
	}
	switch recorded.AccountState {
	case AccountSuspended:
		return Principal{}, fmt.Errorf("identity: verify credential %s: %w", presentedID, ErrAccountSuspended)
	case AccountClosed:
		return Principal{}, fmt.Errorf("identity: verify credential %s: %w", presentedID, ErrAccountClosed)
	case AccountActive:
		return APIKeyPrincipal(recorded.AccountID, recorded.KeyID), nil
	default:
		return Principal{}, fmt.Errorf("identity: verify credential %s: %w: unknown account state %q", presentedID, ErrUnknownCredential, recorded.AccountState)
	}
}
