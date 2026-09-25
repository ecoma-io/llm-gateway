package identity

import (
	"errors"
	"strings"
	"testing"
)

// verifiedFixture mints a secret and returns it with a matching live
// credential, the happy-path shape every deviation test below perturbs.
func verifiedFixture(t *testing.T) (Secret, Credential) {
	t.Helper()
	secret, _, digest := mint(t, "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e")
	return secret, Credential{
		KeyID:        "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e",
		Digest:       digest,
		KeyState:     APIKeyActive,
		AccountID:    "acc-1",
		AccountState: AccountActive,
	}
}

func TestVerificationAuthenticatesAMatchingLiveCredential(t *testing.T) {
	secret, recorded := verifiedFixture(t)
	p, err := VerifyCredential(recorded.KeyID, secret, recorded)
	if err != nil {
		t.Fatalf("VerifyCredential returned error: %v", err)
	}
	if p.Kind != PrincipalAPIKey {
		t.Fatalf("principal kind = %q, want api_key", p.Kind)
	}
	if p.AccountID != "acc-1" || p.APIKeyID != recorded.KeyID {
		t.Fatalf("principal = %+v, want the credential's account and key", p)
	}
	if p.UserID != "" {
		t.Fatalf("an api-key principal must not carry a user id, got %q", p.UserID)
	}
}

func TestVerificationRefusesARecordForAnotherKeyIdenticallyToAMiss(t *testing.T) {
	// A record whose key id disagrees with the id it was looked up by is a
	// source failure — cache keying, column mapping, a corrupt projection —
	// and it must read exactly as a miss: the digest is left valid for the
	// secret, so only the key-id check can reject it, and the rejection
	// shares the miss path's sentinel and byte-for-byte text.
	secret, recorded := verifiedFixture(t)
	presentedID := recorded.KeyID
	_, miss := VerifyCredential(presentedID, secret, Credential{})
	if miss == nil {
		t.Fatal("the zero record must miss")
	}
	recorded.KeyID = APIKeyID("119203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e")
	_, disagree := VerifyCredential(presentedID, secret, recorded)
	if !errors.Is(disagree, ErrUnknownCredential) {
		t.Fatalf("record for another key error = %v, want ErrUnknownCredential", disagree)
	}
	if disagree.Error() != miss.Error() {
		t.Fatalf("record for another key error = %q, want the miss text %q", disagree.Error(), miss.Error())
	}
}

func TestVerificationTreatsMismatchAndMissingAsTheSameUnknown(t *testing.T) {
	secret, recorded := verifiedFixture(t)

	// Wrong secret.
	other, _, _ := mint(t, "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e")
	if _, err := VerifyCredential(recorded.KeyID, other, recorded); !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("mismatched secret error = %v, want ErrUnknownCredential", err)
	}

	// Same secret, wrong digest column (the missing-record caller burns a
	// dummy compare and passes a zero digest).
	recorded.Digest = Digest{}
	if _, err := VerifyCredential(recorded.KeyID, secret, recorded); !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("zero digest error = %v, want ErrUnknownCredential", err)
	}
}

func TestVerificationReportsUnknownBeforeRevocation(t *testing.T) {
	// A wrong secret against a revoked key must be indistinguishable from a
	// wrong secret against anything else: the state report (ErrKeyRevoked)
	// is gated behind the digest match, so possession of the secret is the
	// only way to learn a key's lifecycle.
	_, recorded := verifiedFixture(t)
	recorded.KeyState = APIKeyRevoked
	wrong, _, _ := mint(t, "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e")
	if _, err := VerifyCredential(recorded.KeyID, wrong, recorded); !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("wrong secret on revoked key error = %v, want ErrUnknownCredential", err)
	}
}

func TestVerificationRefusesARevokedKeyEvenWithAMatchingSecret(t *testing.T) {
	secret, recorded := verifiedFixture(t)
	recorded.KeyState = APIKeyRevoked
	if _, err := VerifyCredential(recorded.KeyID, secret, recorded); !errors.Is(err, ErrKeyRevoked) {
		t.Fatalf("revoked key error = %v, want ErrKeyRevoked", err)
	}
}

func TestVerificationRefusesInactiveAccountStates(t *testing.T) {
	secret, recorded := verifiedFixture(t)
	recorded.AccountState = AccountSuspended
	if _, err := VerifyCredential(recorded.KeyID, secret, recorded); !errors.Is(err, ErrAccountSuspended) {
		t.Fatalf("suspended account error = %v, want ErrAccountSuspended", err)
	}
	secret, recorded = verifiedFixture(t)
	recorded.AccountState = AccountClosed
	if _, err := VerifyCredential(recorded.KeyID, secret, recorded); !errors.Is(err, ErrAccountClosed) {
		t.Fatalf("closed account error = %v, want ErrAccountClosed", err)
	}
}

func TestVerificationChecksTheKeyBeforeTheAccount(t *testing.T) {
	// A revoked key on a suspended account reports the key: revocation is
	// the operator's loudest signal and must not be masked by account state.
	secret, recorded := verifiedFixture(t)
	recorded.KeyState = APIKeyRevoked
	recorded.AccountState = AccountSuspended
	if _, err := VerifyCredential(recorded.KeyID, secret, recorded); !errors.Is(err, ErrKeyRevoked) {
		t.Fatalf("revoked key on suspended account error = %v, want ErrKeyRevoked", err)
	}
}

func TestVerificationFailsClosedOnUnknownRecordedStates(t *testing.T) {
	secret, recorded := verifiedFixture(t)
	recorded.KeyState = APIKeyState("time-travelled")
	if _, err := VerifyCredential(recorded.KeyID, secret, recorded); !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("unknown key state error = %v, want ErrUnknownCredential", err)
	}
	secret, recorded = verifiedFixture(t)
	recorded.AccountState = AccountState("haunted")
	if _, err := VerifyCredential(recorded.KeyID, secret, recorded); !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("unknown account state error = %v, want ErrUnknownCredential", err)
	}
}

func TestVerificationErrorsNeverEchoSecretMaterial(t *testing.T) {
	secret, token, _ := mint(t, "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e")
	secretSegment := strings.SplitN(token, "_", 3)[2]
	// Drive every rejection path and assert none of the error text carries
	// the presented secret.
	wrongSecret, _, _ := mint(t, "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e")
	for name, fn := range map[string]func() error{
		"mismatch": func() error {
			_, err := VerifyCredential("019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e", wrongSecret, credentialFor(t, secret))
			return err
		},
		"revoked": func() error {
			c := credentialFor(t, secret)
			c.KeyState = APIKeyRevoked
			_, err := VerifyCredential(c.KeyID, secret, c)
			return err
		},
		"suspended": func() error {
			c := credentialFor(t, secret)
			c.AccountState = AccountSuspended
			_, err := VerifyCredential(c.KeyID, secret, c)
			return err
		},
		"closed": func() error {
			c := credentialFor(t, secret)
			c.AccountState = AccountClosed
			_, err := VerifyCredential(c.KeyID, secret, c)
			return err
		},
	} {
		err := fn()
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if strings.Contains(err.Error(), secretSegment) {
			t.Fatalf("%s: error text carries the secret: %q", name, err.Error())
		}
	}
}

// credentialFor builds a live credential recording exactly this secret.
func credentialFor(t *testing.T, s Secret) Credential {
	t.Helper()
	return Credential{
		KeyID:        "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e",
		Digest:       s.Digest(),
		KeyState:     APIKeyActive,
		AccountID:    "acc-1",
		AccountState: AccountActive,
	}
}
