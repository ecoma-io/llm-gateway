package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The identity use cases' tests. The fakes below hold aggregates in memory
// and move them through the same shapes the real adapters do — whole-aggregate
// reads, compare-and-swap writes — so the tests pin the use cases' own
// behaviour: units of work, domain refusals surfaced, lost swaps re-read
// rather than overwritten, and the verification pipeline end to end over a
// minted key. What they deliberately do not pin is SQL; that is the adapter
// tests' and the real-database tiers' job.

// unusedQuerier is the fake store's query surface: a persistence.Querier
// with no implementation behind it. Application tests never run SQL — a
// query reaching this type means a use case reached around a repository, and
// it dies on the nil embedded interface rather than being answered.
type unusedQuerier struct {
	persistence.Querier
}

// identityStore runs every unit of work inline and counts them. It is named
// apart from this package's factingestion fakeStore, whose world is the fact
// feed rather than the identity aggregates.
type identityStore struct {
	units int
}

func (s *identityStore) Ping(context.Context) error { return nil }
func (s *identityStore) Querier(context.Context) persistence.Querier {
	return unusedQuerier{}
}
func (s *identityStore) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	s.units++
	return fn(ctx)
}

// fakeAccounts keeps whole aggregates and moves them as the compare-and-swap
// port describes. Its two racer knobs model the two ways a swap loses:
// failNextSwaps is a concurrent winner of the same move — the swap reports
// not applied AND the row moves to the caller's target state, so the next
// read finds the work done; blockNextSwaps is churn — the swap raced a move
// that is already over, reports not applied, and leaves the row exactly
// where the caller read it, so nothing converges and the loop must give up.
type fakeAccounts struct {
	rows           map[identity.AccountID]identity.Account
	createErr      error
	failNextSwaps  int
	blockNextSwaps int
	swaps          int
}

func (f *fakeAccounts) Create(_ context.Context, a identity.Account) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.rows[a.ID] = a
	return nil
}

func (f *fakeAccounts) ByID(_ context.Context, id identity.AccountID) (identity.Account, error) {
	a, ok := f.rows[id]
	if !ok {
		return identity.Account{}, fmt.Errorf("fake: account %s: %w", id, persistence.ErrNotFound)
	}
	return a, nil
}

func (f *fakeAccounts) TransitionState(_ context.Context, id identity.AccountID, from, to identity.AccountState, at time.Time) (bool, error) {
	f.swaps++
	a, ok := f.rows[id]
	if !ok || a.State != from {
		return false, nil
	}
	if f.blockNextSwaps > 0 {
		f.blockNextSwaps--
		return false, nil
	}
	if f.failNextSwaps > 0 {
		f.failNextSwaps--
		a.State = to // the racer completed the same move
		f.rows[id] = a
		return false, nil
	}
	a.State = to
	a.UpdatedAt = at
	f.rows[id] = a
	return true, nil
}

// fakeUsers mirrors fakeAccounts for the user aggregate, plus a
// firstCreateErr for the uniqueness-collision path the real adapter maps to
// the domain sentinel.
type fakeUsers struct {
	rows           map[identity.UserID]identity.User
	firstCreateErr error
	consumed       bool
	failNextSwaps  int
	swaps          int
}

func (f *fakeUsers) Create(_ context.Context, u identity.User) error {
	if !f.consumed && f.firstCreateErr != nil {
		f.consumed = true
		return f.firstCreateErr
	}
	f.rows[u.ID] = u
	return nil
}

func (f *fakeUsers) ByID(_ context.Context, id identity.UserID) (identity.User, error) {
	u, ok := f.rows[id]
	if !ok {
		return identity.User{}, fmt.Errorf("fake: user %s: %w", id, persistence.ErrNotFound)
	}
	return u, nil
}

func (f *fakeUsers) TransitionState(_ context.Context, id identity.UserID, from, to identity.UserState, at time.Time) (bool, error) {
	f.swaps++
	u, ok := f.rows[id]
	if !ok || u.State != from {
		return false, nil
	}
	if f.failNextSwaps > 0 {
		f.failNextSwaps--
		u.State = to
		f.rows[id] = u
		return false, nil
	}
	u.State = to
	u.UpdatedAt = at
	f.rows[id] = u
	return true, nil
}

// fakeKeys keeps the API-key ownership records, counts its swaps, and can
// lose a revoke swap to a simulated racing revoker.
type fakeKeys struct {
	rows            map[identity.APIKeyID]identity.APIKey
	failNextRevokes int
	revokes         int
}

func (f *fakeKeys) Create(_ context.Context, k identity.APIKey) error {
	f.rows[k.ID] = k
	return nil
}

func (f *fakeKeys) ByID(_ context.Context, id identity.APIKeyID) (identity.APIKey, error) {
	k, ok := f.rows[id]
	if !ok {
		return identity.APIKey{}, fmt.Errorf("fake: api key %s: %w", id, persistence.ErrNotFound)
	}
	return k, nil
}

func (f *fakeKeys) Revoke(_ context.Context, id identity.APIKeyID, at time.Time) (bool, error) {
	f.revokes++
	k, ok := f.rows[id]
	if !ok || k.State != identity.APIKeyActive {
		return false, nil
	}
	if f.failNextRevokes > 0 {
		f.failNextRevokes--
		now := at
		k.State = identity.APIKeyRevoked
		k.RevokedAt = &now
		f.rows[id] = k
		return false, nil
	}
	now := at
	k.State = identity.APIKeyRevoked
	k.RevokedAt = &now
	k.UpdatedAt = at
	f.rows[id] = k
	return true, nil
}

// fakeCredentials is the CredentialSource: the recorded half of a
// verification, as the tests mint it.
type fakeCredentials struct {
	byID map[identity.APIKeyID]identity.Credential
}

func (f *fakeCredentials) APIKeyCredential(_ context.Context, id identity.APIKeyID) (identity.Credential, bool, error) {
	c, ok := f.byID[id]
	return c, ok, nil
}

// identityHarness wires the real use cases to the fakes.
type identityHarness struct {
	store    *identityStore
	accounts *fakeAccounts
	users    *fakeUsers
	keys     *fakeKeys
	identity *Identity
}

func newIdentityHarness() *identityHarness {
	store := &identityStore{}
	accounts := &fakeAccounts{rows: make(map[identity.AccountID]identity.Account)}
	users := &fakeUsers{rows: make(map[identity.UserID]identity.User)}
	keys := &fakeKeys{rows: make(map[identity.APIKeyID]identity.APIKey)}
	return &identityHarness{
		store:    store,
		accounts: accounts,
		users:    users,
		keys:     keys,
		identity: NewIdentity(store, accounts, users, keys),
	}
}

func (h *identityHarness) mustAccount(t *testing.T, name string) identity.Account {
	t.Helper()
	a, err := h.identity.CreateAccount(context.Background(), name)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	return a
}

func (h *identityHarness) mustUser(t *testing.T, accountID identity.AccountID, email string) identity.User {
	t.Helper()
	u, err := h.identity.CreateUser(context.Background(), accountID, email)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

func (h *identityHarness) mustMint(t *testing.T, accountID identity.AccountID, createdBy identity.UserID, name string) MintedKey {
	t.Helper()
	minted, err := h.identity.MintAPIKey(context.Background(), accountID, createdBy, name)
	if err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}
	return minted
}

func TestCreateAccountPersistsTheBornActiveAggregate(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")

	stored, err := h.identity.Account(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if stored.State != identity.AccountActive || stored.Name != "Acme" {
		t.Fatalf("stored account = %+v, want the born-active aggregate", stored)
	}
	if h.store.units == 0 {
		t.Fatal("CreateAccount ran no unit of work")
	}
}

func TestCreateAccountSurfacesTheDomainNameRules(t *testing.T) {
	h := newIdentityHarness()
	if _, err := h.identity.CreateAccount(context.Background(), "   "); !errors.Is(err, identity.ErrInvalidAccountName) {
		t.Fatalf("CreateAccount with a blank name returned %v, want ErrInvalidAccountName", err)
	}
}

func TestSuspendReinstateAndCloseDriveTheAccountMachine(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")

	if err := h.identity.SuspendAccount(context.Background(), a.ID); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	stored, _ := h.identity.Account(context.Background(), a.ID)
	if stored.State != identity.AccountSuspended {
		t.Fatalf("after suspend: %q", stored.State)
	}
	if err := h.identity.ReinstateAccount(context.Background(), a.ID); err != nil {
		t.Fatalf("ReinstateAccount: %v", err)
	}
	stored, _ = h.identity.Account(context.Background(), a.ID)
	if stored.State != identity.AccountActive {
		t.Fatalf("after reinstate: %q", stored.State)
	}
	if err := h.identity.CloseAccount(context.Background(), a.ID); err != nil {
		t.Fatalf("CloseAccount: %v", err)
	}
	if err := h.identity.ReinstateAccount(context.Background(), a.ID); !errors.Is(err, identity.ErrInvalidTransition) {
		t.Fatalf("reinstate a closed account returned %v, want ErrInvalidTransition", err)
	}
}

func TestAccountTransitionsAreNoOpsWhenTheStateAlreadyHolds(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	if err := h.identity.SuspendAccount(context.Background(), a.ID); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}

	if err := h.identity.SuspendAccount(context.Background(), a.ID); err != nil {
		t.Fatalf("repeated SuspendAccount: %v", err)
	}
	if h.accounts.swaps != 1 {
		t.Fatalf("the second suspend performed %d swaps, want 0 — a no-op is decided by the domain, before any swap", h.accounts.swaps)
	}
}

func TestAccountTransitionReAppliesAfterALostSwap(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	h.accounts.failNextSwaps = 1 // the racer completes the same move first

	if err := h.identity.SuspendAccount(context.Background(), a.ID); err != nil {
		t.Fatalf("SuspendAccount after a lost swap: %v", err)
	}
	stored, _ := h.identity.Account(context.Background(), a.ID)
	if stored.State != identity.AccountSuspended {
		t.Fatalf("after a lost swap the account is %q, want suspended", stored.State)
	}
	if h.accounts.swaps != 1 {
		t.Fatalf("%d swaps ran, want 1 — the lost one; the re-read found the racer's move, and the domain ruled the re-apply a no-op", h.accounts.swaps)
	}
}

func TestAccountTransitionGivesUpUnderContinuedContention(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	h.accounts.blockNextSwaps = casMaxAttempts // every swap races churn that leaves the row where it was

	if err := h.identity.SuspendAccount(context.Background(), a.ID); !errors.Is(err, ErrTransitionContended) {
		t.Fatalf("SuspendAccount under permanent contention returned %v, want ErrTransitionContended", err)
	}
	if h.accounts.swaps != casMaxAttempts {
		t.Fatalf("%d swaps ran under permanent contention, want %d — the loop is bounded, not endless", h.accounts.swaps, casMaxAttempts)
	}
}

func TestUnknownAccountSurfacesAsNotFound(t *testing.T) {
	h := newIdentityHarness()
	_, err := h.identity.Account(context.Background(), "a0000000-0000-0000-0000-0000000000ff")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("reading an unknown account returned %v, want persistence.ErrNotFound", err)
	}
	if err := h.identity.SuspendAccount(context.Background(), "a0000000-0000-0000-0000-0000000000ff"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("suspending an unknown account returned %v, want persistence.ErrNotFound", err)
	}
}

func TestCreateUserRequiresAnActiveAccount(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	if err := h.identity.SuspendAccount(context.Background(), a.ID); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}

	if _, err := h.identity.CreateUser(context.Background(), a.ID, "person@example.com"); !errors.Is(err, identity.ErrAccountNotActive) {
		t.Fatalf("CreateUser on a suspended account returned %v, want ErrAccountNotActive", err)
	}
	if _, err := h.identity.CreateUser(context.Background(), "a0000000-0000-0000-0000-0000000000ff", "person@example.com"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("CreateUser under an unknown account returned %v, want persistence.ErrNotFound", err)
	}
}

func TestCreateUserSurfacesTheEmailTakenSentinel(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	h.users.firstCreateErr = identity.ErrUserEmailTaken

	if _, err := h.identity.CreateUser(context.Background(), a.ID, "person@example.com"); !errors.Is(err, identity.ErrUserEmailTaken) {
		t.Fatalf("CreateUser with a collision returned %v, want ErrUserEmailTaken", err)
	}
}

func TestCreateUserSurfacesTheDomainEmailRules(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	if _, err := h.identity.CreateUser(context.Background(), a.ID, "no-at-sign"); !errors.Is(err, identity.ErrInvalidEmail) {
		t.Fatalf("CreateUser with an implausible email returned %v, want ErrInvalidEmail", err)
	}
}

func TestUserTransitionsDriveTheUserMachine(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	u := h.mustUser(t, a.ID, "person@example.com")

	if err := h.identity.ActivateUser(context.Background(), u.ID); err != nil {
		t.Fatalf("ActivateUser: %v", err)
	}
	if err := h.identity.RemoveUser(context.Background(), u.ID); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	if err := h.identity.ActivateUser(context.Background(), u.ID); !errors.Is(err, identity.ErrInvalidTransition) {
		t.Fatalf("activate a removed user returned %v, want ErrInvalidTransition", err)
	}
	if err := h.identity.RemoveUser(context.Background(), u.ID); err != nil {
		t.Fatalf("repeated RemoveUser: %v", err)
	}
	if h.users.swaps != 2 {
		t.Fatalf("%d user swaps ran, want 2 — activate and the first remove; the rest were no-ops", h.users.swaps)
	}
}

func TestMintStoresTheOwnershipRecordAndHandsBackTheOneTimeToken(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	minted := h.mustMint(t, a.ID, "", "deploy key")

	keyID, secret, err := identity.ParseToken(minted.Token)
	if err != nil {
		t.Fatalf("the minted token does not parse: %v", err)
	}
	if keyID != minted.Key.ID {
		t.Fatalf("the token's key id %q is not the record's %q", keyID, minted.Key.ID)
	}
	if !identity.EqualDigests(secret.Digest(), minted.Digest) {
		t.Fatalf("the returned digest is not the token secret's digest")
	}
	stored, err := h.identity.APIKey(context.Background(), minted.Key.ID)
	if err != nil {
		t.Fatalf("APIKey: %v", err)
	}
	if stored.State != identity.APIKeyActive || stored.RevokedAt != nil {
		t.Fatalf("stored key = %+v, want an active key with no revocation", stored)
	}
	if stored.Prefix != identity.TokenPrefix(minted.Key.ID) {
		t.Fatalf("stored prefix %q, want the id-derived prefix", stored.Prefix)
	}
	// The security regression this test really pins: nothing persisted
	// carries the token or its secret segment.
	secretSegment := strings.SplitN(minted.Token, "_", 3)[2]
	persisted := fmt.Sprintf("%s|%s|%s", stored.Prefix, stored.DisplayName, stored.ID)
	if strings.Contains(persisted, secretSegment) || strings.Contains(persisted, minted.Token) {
		t.Fatalf("the persisted ownership record carries secret material: %q", persisted)
	}
}

func TestMintRequiresAnActiveAccountAndAValidCreator(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	other := h.mustAccount(t, "Other")
	creator := h.mustUser(t, a.ID, "creator@example.com")

	if _, err := h.identity.MintAPIKey(context.Background(), other.ID, creator.ID, "k"); !errors.Is(err, identity.ErrCreatorOutsideAccount) {
		t.Fatalf("mint with a cross-account creator returned %v, want ErrCreatorOutsideAccount", err)
	}
	if err := h.identity.RemoveUser(context.Background(), creator.ID); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	if _, err := h.identity.MintAPIKey(context.Background(), a.ID, creator.ID, "k"); !errors.Is(err, identity.ErrCreatorRemoved) {
		t.Fatalf("mint with a removed creator returned %v, want ErrCreatorRemoved", err)
	}
	if _, err := h.identity.MintAPIKey(context.Background(), "a0000000-0000-0000-0000-0000000000ff", "", "k"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("mint under an unknown account returned %v, want persistence.ErrNotFound", err)
	}
	if err := h.identity.SuspendAccount(context.Background(), a.ID); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	if _, err := h.identity.MintAPIKey(context.Background(), a.ID, "", "k"); !errors.Is(err, identity.ErrAccountNotActive) {
		t.Fatalf("mint on a suspended account returned %v, want ErrAccountNotActive", err)
	}
}

func TestRevokeConfirmsIdempotenceWithoutASwap(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	minted := h.mustMint(t, a.ID, "", "deploy key")

	if err := h.identity.RevokeAPIKey(context.Background(), minted.Key.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if err := h.identity.RevokeAPIKey(context.Background(), minted.Key.ID); err != nil {
		t.Fatalf("repeated RevokeAPIKey: %v", err)
	}
	if h.keys.revokes != 1 {
		t.Fatalf("%d revoke swaps ran, want 1 — the second revocation is confirmed by a read, not a second swap", h.keys.revokes)
	}
	stored, _ := h.identity.APIKey(context.Background(), minted.Key.ID)
	if stored.State != identity.APIKeyRevoked || stored.RevokedAt == nil {
		t.Fatalf("stored key = %+v, want revoked with a revocation stamp", stored)
	}
}

func TestRevokeConfirmsARevokedRowAfterALostSwap(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	minted := h.mustMint(t, a.ID, "", "deploy key")
	h.keys.failNextRevokes = 1 // a racing revoker wins

	if err := h.identity.RevokeAPIKey(context.Background(), minted.Key.ID); err != nil {
		t.Fatalf("RevokeAPIKey after a lost swap: %v", err)
	}
	if h.keys.revokes != 1 {
		t.Fatalf("%d revoke swaps ran, want 1 — the lost one; the confirming read found the racing revoker's completed work", h.keys.revokes)
	}
}

func TestVerifyAuthenticatesAMintedToken(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	minted := h.mustMint(t, a.ID, "", "deploy key")
	source := &fakeCredentials{byID: map[identity.APIKeyID]identity.Credential{
		minted.Key.ID: {
			KeyID:        minted.Key.ID,
			Digest:       minted.Digest,
			KeyState:     minted.Key.State,
			AccountID:    minted.Key.AccountID,
			AccountState: identity.AccountActive,
		},
	}}

	principal, err := h.identity.VerifyAPIKey(context.Background(), minted.Token, source)
	if err != nil {
		t.Fatalf("VerifyAPIKey: %v", err)
	}
	if principal != identity.APIKeyPrincipal(minted.Key.AccountID, minted.Key.ID) {
		t.Fatalf("principal = %+v, want the minted key's principal", principal)
	}
}

func TestVerifyRejectsARevokedKeyWithItsSecretIntact(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	minted := h.mustMint(t, a.ID, "", "deploy key")
	if err := h.identity.RevokeAPIKey(context.Background(), minted.Key.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	source := &fakeCredentials{byID: map[identity.APIKeyID]identity.Credential{
		minted.Key.ID: {
			KeyID:        minted.Key.ID,
			Digest:       minted.Digest,
			KeyState:     identity.APIKeyRevoked,
			AccountID:    minted.Key.AccountID,
			AccountState: identity.AccountActive,
		},
	}}

	if _, err := h.identity.VerifyAPIKey(context.Background(), minted.Token, source); !errors.Is(err, identity.ErrKeyRevoked) {
		t.Fatalf("VerifyAPIKey on a revoked key returned %v, want ErrKeyRevoked", err)
	}
}

func TestVerifyReportsAMissAsUnknownCredential(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	minted := h.mustMint(t, a.ID, "", "deploy key")

	// An empty source: the key id has no record anywhere.
	if _, err := h.identity.VerifyAPIKey(context.Background(), minted.Token, &fakeCredentials{}); !errors.Is(err, identity.ErrUnknownCredential) {
		t.Fatalf("VerifyAPIKey against a missing record returned %v, want ErrUnknownCredential", err)
	}
}

func TestVerifyReportsAMissAndAMismatchWithOneIdenticalText(t *testing.T) {
	// The two unknowns travel different code paths — the miss binds the zero
	// Credential and verifies against it; the mismatch compares two live
	// digests — but the observable outcome must be byte-identical. The first
	// transport that echoes the error text would otherwise tell an attacker
	// which key ids exist.
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	minted := h.mustMint(t, a.ID, "", "deploy key")

	mismatch := &fakeCredentials{byID: map[identity.APIKeyID]identity.Credential{
		minted.Key.ID: {
			KeyID:        minted.Key.ID,
			Digest:       identity.Digest{}, // a recorded digest that cannot match
			KeyState:     identity.APIKeyActive,
			AccountID:    minted.Key.AccountID,
			AccountState: identity.AccountActive,
		},
	}}
	_, missErr := h.identity.VerifyAPIKey(context.Background(), minted.Token, &fakeCredentials{})
	_, mismatchErr := h.identity.VerifyAPIKey(context.Background(), minted.Token, mismatch)

	if !errors.Is(missErr, identity.ErrUnknownCredential) || !errors.Is(mismatchErr, identity.ErrUnknownCredential) {
		t.Fatalf("miss = %v, mismatch = %v; both want ErrUnknownCredential", missErr, mismatchErr)
	}
	if missErr.Error() != mismatchErr.Error() {
		t.Fatalf("miss text %q differs from mismatch text %q — the text is a key-id existence oracle", missErr.Error(), mismatchErr.Error())
	}
}

func TestMintedKeyRedactsEveryAccidentalPrintingPath(t *testing.T) {
	// MintAPIKey's return value carries the one-time token. The struct's own
	// rendering methods are the last line of defence before a caller's
	// fmt.Sprintf turns a handover moment into a log entry.
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	minted := h.mustMint(t, a.ID, "", "deploy key")
	secretSegment := strings.SplitN(minted.Token, "_", 3)[2]

	check := func(name, rendered string) {
		t.Helper()
		if strings.Contains(rendered, secretSegment) || strings.Contains(rendered, minted.Token) {
			t.Fatalf("%s rendered secret material: %q", name, rendered)
		}
		if !strings.Contains(rendered, "redacted") {
			t.Fatalf("%s rendered %q without the redaction marker", name, rendered)
		}
	}
	check("%v", fmt.Sprintf("%v", minted))
	check("%+v", fmt.Sprintf("%+v", minted))
	check("%s", fmt.Sprintf("%s", minted)) //nolint:staticcheck // the fmt dispatch, not the method call, is the subject under test
	check("%q", fmt.Sprintf("%q", minted))
	check("GoString", fmt.Sprintf("%#v", minted))
	check("String method", minted.String())

	if _, err := minted.MarshalJSON(); err == nil {
		t.Fatalf("MarshalJSON succeeded; a serialiser smuggled the one-time token")
	}
}

func TestVerifyFailsClosedOnMalformedTokens(t *testing.T) {
	h := newIdentityHarness()
	source := &fakeCredentials{}
	for name, raw := range map[string]string{
		"garbage":     "garbage",
		"wrong brand": "sk_019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e_" + strings.Repeat("A", 43),
		"truncated":   "gw_019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e_" + strings.Repeat("A", 42),
		"whitespace":  "gw_019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e_" + strings.Repeat("A", 43) + "\n",
	} {
		if _, err := h.identity.VerifyAPIKey(context.Background(), raw, source); !errors.Is(err, identity.ErrMalformedToken) {
			t.Fatalf("%s: VerifyAPIKey returned %v, want ErrMalformedToken", name, err)
		}
	}
}

func TestVerifyReportsInactiveAccounts(t *testing.T) {
	h := newIdentityHarness()
	a := h.mustAccount(t, "Acme")
	minted := h.mustMint(t, a.ID, "", "deploy key")
	credential := func(state identity.AccountState) *fakeCredentials {
		return &fakeCredentials{byID: map[identity.APIKeyID]identity.Credential{
			minted.Key.ID: {
				KeyID:        minted.Key.ID,
				Digest:       minted.Digest,
				KeyState:     identity.APIKeyActive,
				AccountID:    minted.Key.AccountID,
				AccountState: state,
			},
		}}
	}

	if _, err := h.identity.VerifyAPIKey(context.Background(), minted.Token, credential(identity.AccountSuspended)); !errors.Is(err, identity.ErrAccountSuspended) {
		t.Fatalf("suspended account returned %v, want ErrAccountSuspended", err)
	}
	if _, err := h.identity.VerifyAPIKey(context.Background(), minted.Token, credential(identity.AccountClosed)); !errors.Is(err, identity.ErrAccountClosed) {
		t.Fatalf("closed account returned %v, want ErrAccountClosed", err)
	}
}

func TestNewIdentityRefusesAnyNilPort(t *testing.T) {
	store := &identityStore{}
	accounts := &fakeAccounts{rows: map[identity.AccountID]identity.Account{}}
	users := &fakeUsers{rows: map[identity.UserID]identity.User{}}
	keys := &fakeKeys{rows: map[identity.APIKeyID]identity.APIKey{}}

	builds := map[string]func(){
		"store":    func() { NewIdentity(nil, accounts, users, keys) },
		"accounts": func() { NewIdentity(store, nil, users, keys) },
		"users":    func() { NewIdentity(store, accounts, nil, keys) },
		"api keys": func() { NewIdentity(store, accounts, users, nil) },
	}
	for name, build := range builds {
		panicked := false
		func() {
			defer func() {
				if recover() != nil {
					panicked = true
				}
			}()
			build()
		}()
		if !panicked {
			t.Fatalf("%s: NewIdentity accepted a nil port", name)
		}
	}
}
