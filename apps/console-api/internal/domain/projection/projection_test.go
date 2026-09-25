package projection

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// The fixtures every test in this package branches from. Each string is in the
// exact form its constructor demands — a canonical lowercase UUID with a
// version nibble of 4 and the RFC 4122 variant bits, and 64 lowercase hex
// characters — so a refusal below is the constructor's judgement and never a
// typo in a fixture, and an acceptance is never an accident of a loosely
// written literal.
const (
	validKeyID     = "019203d0-9a1b-4c2a-8f1e-3f5a6b7c8d9e"
	validAccountID = "5e7b1c9a-2d4f-4a6b-9c8d-0e1f2a3b4c5d"
	validEpoch     = "9a1b2c3d-4e5f-4a1b-8c2d-5e6f7a8b9c0d"
	validDigest    = "0a1b2c3d0a1b2c3d0a1b2c3d0a1b2c3d0a1b2c3d0a1b2c3d0a1b2c3d0a1b2c3d"
)

// recordedAt is the instant every change in these tests is recorded at. It is
// fixed rather than drawn from the wall clock so the rendering a test expects
// on the wire is a literal a reader can compare by eye.
var recordedAt = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// recordedAtElsewhere is that same instant held in a zone five and a half
// hours ahead of UTC, which is what a caller's clock looks like when the
// session behind it is not in UTC. The constructors are pinned to fold it
// back, because the projection is stored once in the producer's tables and
// delivered once across the seam, and only one zone renders identically at
// every hop.
var recordedAtElsewhere = recordedAt.In(time.FixedZone("test+0530", 5*60*60+30*60))

// revocationInstantElsewhere is a revocation's moment held away from UTC for
// the same reason: 17:30 in that zone is 12:00 UTC, and the UTC rendering is
// what every pin on the wire expects to see.
var revocationInstantElsewhere = time.Date(2026, 9, 25, 17, 30, 0, 0, time.FixedZone("test+0530", 5*60*60+30*60))

// uuidViolations is every shape of identifier this protocol must refuse
// wherever the contract spells `format: uuid`. The grammar is the canonical
// 36-character lowercase 8-4-4-4-12 form with a version nibble of 4 and the
// RFC 4122 variant bits, so a v1-shaped id, an upper-case rendering or a
// friendly name is not a spelling of an identifier a hop may be asked to guess
// about.
func uuidViolations() map[string]string {
	return map[string]string{
		"blank":                  "",
		"a friendly name":        "deploy-key",
		"a fragment":             "019203d0",
		"short by one":           validKeyID[:35],
		"long by one":            validKeyID + "0",
		"without dashes":         strings.ReplaceAll(validKeyID, "-", ""),
		"a misplaced dash":       "019203d0-9a1b4c2a-8f1e-3f5a6b7c8d9e",
		"upper case":             strings.ToUpper(validKeyID),
		"a version 1 nibble":     "019203d0-9a1b-1c2a-8f1e-3f5a6b7c8d9e",
		"a non-rfc 4122 variant": "019203d0-9a1b-4c2a-cf1e-3f5a6b7c8d9e",
		"the nil uuid":           "00000000-0000-0000-0000-000000000000",
		"every digit f":          "ffffffff-ffff-4fff-ffff-ffffffffffff",
	}
}

// mustCredential builds the valid active credential the tests branch from.
func mustCredential(t *testing.T) Credential {
	t.Helper()
	credential, err := NewCredential(validKeyID, validAccountID, validDigest, CredentialActive, nil)
	if err != nil {
		t.Fatalf("NewCredential refused its own test fixture: %v", err)
	}
	return credential
}

// mustRevokedCredential builds the valid revoked credential, its instant held
// away from UTC so any pin on a UTC rendering is earned rather than assumed.
func mustRevokedCredential(t *testing.T) Credential {
	t.Helper()
	credential, err := NewCredential(validKeyID, validAccountID, validDigest, CredentialRevoked, &revocationInstantElsewhere)
	if err != nil {
		t.Fatalf("NewCredential refused its own revoked fixture: %v", err)
	}
	return credential
}

// mustAccount builds the valid account the tests branch from.
func mustAccount(t *testing.T) Account {
	t.Helper()
	account, err := NewAccount(validAccountID, AccountActive)
	if err != nil {
		t.Fatalf("NewAccount refused its own test fixture: %v", err)
	}
	return account
}

// mustCredentialChange records a credential at a revision, failing the test
// when the grammar refuses a value these fixtures built.
func mustCredentialChange(t *testing.T, revision uint64, credential Credential) Change {
	t.Helper()
	change, err := NewCredentialChange(revision, recordedAt, credential)
	if err != nil {
		t.Fatalf("NewCredentialChange refused revision %d: %v", revision, err)
	}
	return change
}

// mustAccountChange is the account twin of mustCredentialChange.
func mustAccountChange(t *testing.T, revision uint64, account Account) Change {
	t.Helper()
	change, err := NewAccountChange(revision, recordedAt, account)
	if err != nil {
		t.Fatalf("NewAccountChange refused revision %d: %v", revision, err)
	}
	return change
}

// decodedObject decodes rendered JSON into a map, failing the test when the
// bytes are not an object: every envelope and record this package renders is
// one, and a test that cannot decode what it has just marshalled has found a
// bug worth stopping on rather than a fixture to fix.
func decodedObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoding %s as a JSON object: %v; bytes no consumer could read are bytes no test can pin", raw, err)
	}
	return decoded
}

// sortedKeys returns an object's field names, sorted, so a key-set comparison
// can be made with slices.Equal and without depending on the order a decoder
// happens to visit fields in.
func sortedKeys(object map[string]any) []string {
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// TestNewDigestAcceptsExactlyTheHexFormTheContractPins pins the one spelling
// of a digest this protocol carries.
//
// The digest is the only form of the secret that exists outside the mint call
// (ADR 0006 §8, ADR 0007), and the fragment declares it as `^[0-9a-f]{64}$` —
// a pattern the database's own CHECKs repeat on both stored copies. A digest
// accepted in any other rendering would travel three hops before the consumer
// and the mirror row refused it, which is later than the constructor that
// minted it and further from the code that produced the bad value.
func TestNewDigestAcceptsExactlyTheHexFormTheContractPins(t *testing.T) {
	digest, err := NewDigest(validDigest)
	if err != nil {
		t.Fatalf("NewDigest refused a 64-character lowercase hex value: %v", err)
	}
	if digest != Digest(validDigest) {
		t.Fatalf("NewDigest digest = %q, want the characters it was given verbatim; a digest is stored and compared by its exact text, so a normalised or trimmed value would not be the secret that was hashed", string(digest))
	}
	// Digest is a type of its own, not a string that happens to look right:
	// verification material must not be interchangeable with arbitrary text.
	if _, ok := any(digest).(Digest); !ok {
		t.Fatalf("NewDigest returned %T, want the package's Digest type; a bare string would let verification material pass wherever any text is accepted", digest)
	}
}

// TestNewDigestRefusesEveryOtherRenderingOfADigest walks the renderings a
// caller might actually arrive with — a truncated hash copied from a log line,
// an upper-case rendering from a formatter, a hex-adjacent typo — and requires
// each to be refused at the mint rather than discovered as a schema refusal by
// the hop on the far side of the seam.
func TestNewDigestRefusesEveryOtherRenderingOfADigest(t *testing.T) {
	tests := []struct {
		name string
		hex  string
	}{
		{name: "empty", hex: ""},
		{name: "short by one", hex: validDigest[:63]},
		{name: "long by one", hex: validDigest + "0"},
		{name: "upper case", hex: strings.ToUpper(validDigest)},
		{name: "one upper-case character", hex: "A" + validDigest[1:]},
		{name: "a non-hex character", hex: validDigest[:63] + "g"},
		{name: "an interior space", hex: validDigest[:31] + " " + validDigest[32:]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			digest, err := NewDigest(tt.hex)
			if err == nil {
				t.Errorf("NewDigest accepted %q as %q; only 64 lowercase hex characters is a spelling of a digest this protocol carries, and anything else would be refused by the fragment's pattern and the mirror's column long after the code that minted it", tt.hex, string(digest))
			}
		})
	}
}

// TestTheUUIDGrammarThisPackageRestates pins the package's own statement of
// the identifier grammar the contract's `format: uuid` fields carry.
//
// The grammar is deliberately restated here rather than imported from the
// ownership domain: the projection speaks the contract's vocabulary, which
// coincides with the ownership ids today and may diverge the day the contract
// does. A restatement is safe exactly while it holds rule by rule, so each
// rule is pinned — length, dash placement, the version nibble and the variant
// bits — because a rule that quietly loosened would mint identifiers the
// Data Plane's schema refuses and a mirror row would never be keyed on.
func TestTheUUIDGrammarThisPackageRestates(t *testing.T) {
	if err := validateUUIDForm(validKeyID); err != nil {
		t.Fatalf("validateUUIDForm rejected a canonical version 4 id: %v", err)
	}
	for name, id := range uuidViolations() {
		if err := validateUUIDForm(id); err == nil {
			t.Errorf("validateUUIDForm accepted the %s id %q; every shape outside the canonical lowercase 8-4-4-4-12 form with version nibble 4 and the RFC 4122 variant is a value a hop's schema refuses", name, id)
		}
	}
}

// TestNewCredentialAcceptsAnActiveCredentialWithoutARevocationInstant pins the
// active half of the pairing the fragment validates: `revoked_at` is null
// exactly when `state` is active, because an instant on an active row is a
// revocation that never happened and a mirror row has no way to act on one.
func TestNewCredentialAcceptsAnActiveCredentialWithoutARevocationInstant(t *testing.T) {
	credential, err := NewCredential(validKeyID, validAccountID, validDigest, CredentialActive, nil)
	if err != nil {
		t.Fatalf("NewCredential refused a well-formed active credential: %v", err)
	}
	if credential.KeyID != validKeyID || credential.AccountID != validAccountID {
		t.Fatalf("NewCredential ids = %q/%q, want %q/%q; the credential's identity is what the mirror row is keyed on and what redelivery idempotently matches", credential.KeyID, credential.AccountID, validKeyID, validAccountID)
	}
	if credential.Digest != Digest(validDigest) {
		t.Fatalf("NewCredential digest = %q, want the digest it was given; the digest is the whole verification payload the runtime admits on", string(credential.Digest))
	}
	if credential.State != CredentialActive {
		t.Fatalf("NewCredential state = %q, want %q; the state is the only thing the mirror row's admission decision reads", credential.State, CredentialActive)
	}
	if credential.RevokedAt != nil {
		t.Fatalf("NewCredential revoked_at = %v, want nil; an instant on an active row is a revocation that never happened", credential.RevokedAt)
	}
}

// TestNewCredentialCarriesARevocationAsTheInstantItWasGivenInUTC pins the
// revoked half of the pairing and the one normalisation the constructor
// performs. The instant is not required to arrive in UTC — a caller's clock is
// whatever its session is — but it is required to leave as UTC: the projection
// is stored twice and delivered once, and an instant kept in its original zone
// would render differently at each hop and make two copies of one revocation
// disagree by spelling.
func TestNewCredentialCarriesARevocationAsTheInstantItWasGivenInUTC(t *testing.T) {
	credential, err := NewCredential(validKeyID, validAccountID, validDigest, CredentialRevoked, &revocationInstantElsewhere)
	if err != nil {
		t.Fatalf("NewCredential refused a revoked credential carrying its instant: %v", err)
	}
	if credential.RevokedAt == nil {
		t.Fatalf("NewCredential dropped the revocation instant; a revoked row without one is a revocation nobody can audit")
	}
	if !credential.RevokedAt.Equal(revocationInstantElsewhere) {
		t.Fatalf("NewCredential revoked_at = %v, want the instant it was given (%v); normalising the zone may never move the moment", credential.RevokedAt, revocationInstantElsewhere)
	}
	if credential.RevokedAt.Location() != time.UTC {
		t.Fatalf("NewCredential revoked_at stayed in %v, want UTC; only one zone renders the same at the producer's table, on the wire and at the mirror", credential.RevokedAt.Location())
	}
}

// TestNewCredentialRefusesIdentityAndDigestOutsideTheGrammar walks the
// identity and digest shapes a producer's own database could hand the
// constructor and requires each to be refused where the value is picked up.
// The constructor is the enforcement of the fragment's schemas; a credential
// it let through would fail at the consumer's schema validation and at the
// mirror's columns, three hops after the code that produced it.
func TestNewCredentialRefusesIdentityAndDigestOutsideTheGrammar(t *testing.T) {
	tests := []struct {
		name      string
		keyID     string
		accountID string
		digest    string
	}{
		{name: "the key id is a friendly name", keyID: "deploy-key", accountID: validAccountID, digest: validDigest},
		{name: "the key id has a version 1 nibble", keyID: "019203d0-9a1b-1c2a-8f1e-3f5a6b7c8d9e", accountID: validAccountID, digest: validDigest},
		{name: "the key id is upper case", keyID: strings.ToUpper(validKeyID), accountID: validAccountID, digest: validDigest},
		{name: "the account id is a friendly name", keyID: validKeyID, accountID: "account-1", digest: validDigest},
		{name: "the account id lacks the rfc 4122 variant", keyID: validKeyID, accountID: "5e7b1c9a-2d4f-4a6b-c9c8-0e1f2a3b4c5d", digest: validDigest},
		{name: "the account id is upper case", keyID: validKeyID, accountID: strings.ToUpper(validAccountID), digest: validDigest},
		{name: "the digest is short", keyID: validKeyID, accountID: validAccountID, digest: validDigest[:63]},
		{name: "the digest is upper case", keyID: validKeyID, accountID: validAccountID, digest: strings.ToUpper(validDigest)},
		{name: "the digest is not hex", keyID: validKeyID, accountID: validAccountID, digest: validDigest[:63] + "g"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewCredential(tt.keyID, tt.accountID, tt.digest, CredentialActive, nil); err == nil {
				t.Errorf("NewCredential accepted the credential with key id %q, account id %q and digest %q; a value outside the grammar would be refused by the fragment's schemas and by the mirror's own columns, and the refusal belongs beside the code that produced the value", tt.keyID, tt.accountID, tt.digest)
			}
		})
	}
}

// TestNewCredentialRefusesStatesOutsideTheContractEnum pins the credential
// state vocabulary as closed. The states here are the contract's enums, not
// the ownership domain's — `suspended` is an account state and has no meaning
// for a credential — and a value outside the enum is a new protocol version
// and a reviewed migration, never a tolerated unknown that a mirror row would
// have to guess at.
func TestNewCredentialRefusesStatesOutsideTheContractEnum(t *testing.T) {
	tests := []struct {
		name  string
		state string
	}{
		{name: "empty", state: ""},
		{name: "the account-only state", state: "suspended"},
		{name: "upper case", state: "ACTIVE"},
		{name: "a state the wire has never spelled", state: "archived"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewCredential(validKeyID, validAccountID, validDigest, CredentialState(tt.state), nil); err == nil {
				t.Errorf("NewCredential accepted the state %q; the contract's enum is closed and a state outside it is undeliverable, so the refusal belongs at the mint rather than at the consumer's schema", tt.state)
			}
		})
	}
}

// TestNewCredentialRefusesTheRevocationPairingsTheContractCallsImpossible pins
// both halves of the pairing the fragment validates rather than assumes:
// `revoked` with no instant is a revocation nobody can audit, and an instant
// on an `active` row is a revocation that never happened. Either shape
// delivered would leave the mirror row holding a state its own columns
// contradict.
func TestNewCredentialRefusesTheRevocationPairingsTheContractCallsImpossible(t *testing.T) {
	t.Run("active with an instant", func(t *testing.T) {
		if _, err := NewCredential(validKeyID, validAccountID, validDigest, CredentialActive, &revocationInstantElsewhere); err == nil {
			t.Errorf("NewCredential accepted an active credential carrying a revocation instant; the fragment pairs revoked_at with the state, and an instant on an active row is a revocation that never happened")
		}
	})
	t.Run("revoked without an instant", func(t *testing.T) {
		if _, err := NewCredential(validKeyID, validAccountID, validDigest, CredentialRevoked, nil); err == nil {
			t.Errorf("NewCredential accepted a revoked credential carrying no instant; the fragment pairs revoked_at with the state, and a revoked row without one is a revocation nobody can audit")
		}
	})
}

// TestNewAccountAcceptsEveryStateInTheContractEnum walks the three states the
// fragment spells and requires each to pass through exactly as given. The
// account half of the mirror carries lifecycle only — no ownership edges, no
// billing data — so the accepted value is the whole record, and any
// normalisation of it would be a fact the runtime mirrors that the Control
// Plane never decided.
func TestNewAccountAcceptsEveryStateInTheContractEnum(t *testing.T) {
	for _, state := range []AccountState{AccountActive, AccountSuspended, AccountClosed} {
		account, err := NewAccount(validAccountID, state)
		if err != nil {
			t.Fatalf("NewAccount refused the state %q: %v", state, err)
		}
		if account.AccountID != validAccountID || account.State != state {
			t.Fatalf("NewAccount = {%q %q}, want {%q %q}; the projected account is exactly an identity and a lifecycle, and anything else on it is a fact the runtime mirrors unasked", account.AccountID, account.State, validAccountID, state)
		}
	}
}

// TestNewAccountRefusesStatesOutsideTheContractEnum pins the account state
// vocabulary as closed, including against the credential vocabulary's values:
// `revoked` is a credential's terminal state, and an account carrying it is a
// smuggled vocabulary that no mirror column has a meaning for.
func TestNewAccountRefusesStatesOutsideTheContractEnum(t *testing.T) {
	tests := []struct {
		name  string
		state string
	}{
		{name: "empty", state: ""},
		{name: "the credential-only state", state: "revoked"},
		{name: "upper case", state: "CLOSED"},
		{name: "a state the wire has never spelled", state: "archived"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewAccount(validAccountID, AccountState(tt.state)); err == nil {
				t.Errorf("NewAccount accepted the state %q; the contract's enum is closed, and a state outside it is a message every hop fails closed against", tt.state)
			}
		})
	}
}

// TestNewAccountRefusesIdentifiersOutsideTheUUIDGrammar pins the account id to
// the same grammar the credential's ids answer to: the mirror's account rows
// are keyed by it, and an id a hop's schema refuses would strand the row it
// was meant to describe.
func TestNewAccountRefusesIdentifiersOutsideTheUUIDGrammar(t *testing.T) {
	for name, id := range uuidViolations() {
		if _, err := NewAccount(id, AccountActive); err == nil {
			t.Errorf("NewAccount accepted the %s id %q; the fragment spells the account id `format: uuid`, and a value outside that grammar is undeliverable", name, id)
		}
	}
}

// TestAChangeIsRecordedOnlyAtARevisionTheCounterAllocated pins the floor both
// change constructors enforce. The counter allocates from one — the fragment
// gives `revision` a minimum of 1 — and a zero in a delivered batch would
// claim a revision the log never issued, which is exactly the kind of position
// a consumer cannot reason its way out of.
func TestAChangeIsRecordedOnlyAtARevisionTheCounterAllocated(t *testing.T) {
	if _, err := NewCredentialChange(0, recordedAt, mustCredential(t)); err == nil {
		t.Errorf("NewCredentialChange accepted revision 0; the counter allocates from 1, and a zero in a delivered batch would name a revision the log never issued")
	}
	if _, err := NewAccountChange(0, recordedAt, mustAccount(t)); err == nil {
		t.Errorf("NewAccountChange accepted revision 0; the counter allocates from 1, and a zero in a delivered batch would name a revision the log never issued")
	}
	for _, revision := range []uint64{1, 2, 1 << 40} {
		if _, err := NewCredentialChange(revision, recordedAt, mustCredential(t)); err != nil {
			t.Errorf("NewCredentialChange refused revision %d: %v", revision, err)
		}
		if _, err := NewAccountChange(revision, recordedAt, mustAccount(t)); err != nil {
			t.Errorf("NewAccountChange refused revision %d: %v", revision, err)
		}
	}
}

// TestAChangeDerivesItsKindAndIdentityFromTheValueItCarries pins the
// constructor as the only way the envelope's identity exists: the kind and the
// resource id follow from which constructor built the change, so a change
// cannot disagree with the row it carries — an api_key envelope naming an
// account id would route the row to the wrong mirror table.
func TestAChangeDerivesItsKindAndIdentityFromTheValueItCarries(t *testing.T) {
	credentialChange := mustCredentialChange(t, 4, mustCredential(t))
	if credentialChange.Kind != KindAPIKey {
		t.Fatalf("an api_key change carries kind %q, want %q; the kind is what the consumer's mirror routes the row on", credentialChange.Kind, KindAPIKey)
	}
	if credentialChange.ResourceID != validKeyID {
		t.Fatalf("an api_key change carries resource id %q, want the credential's key id %q; the id is what makes redelivery a no-op on an existing row", credentialChange.ResourceID, validKeyID)
	}
	accountChange := mustAccountChange(t, 5, mustAccount(t))
	if accountChange.Kind != KindAccount {
		t.Fatalf("an account change carries kind %q, want %q; the kind is what the consumer's mirror routes the row on", accountChange.Kind, KindAccount)
	}
	if accountChange.ResourceID != validAccountID {
		t.Fatalf("an account change carries resource id %q, want the account's id %q; the id is what makes redelivery a no-op on an existing row", accountChange.ResourceID, validAccountID)
	}
}

// TestAChangeRecordsItsInstantInUTCWhateverZoneTheCallerHeld pins the
// recorded_at normalisation. The instant orders nothing — revisions are the
// only ordering key (ADR 0007 §3) — but it is read by operators on both
// planes, and one change rendered in two zones across the copies of the log is
// an audit trail that disagrees with itself.
func TestAChangeRecordsItsInstantInUTCWhateverZoneTheCallerHeld(t *testing.T) {
	credentialChange, err := NewCredentialChange(4, recordedAtElsewhere, mustCredential(t))
	if err != nil {
		t.Fatalf("NewCredentialChange refused a well-formed change: %v", err)
	}
	accountChange, err := NewAccountChange(5, recordedAtElsewhere, mustAccount(t))
	if err != nil {
		t.Fatalf("NewAccountChange refused a well-formed change: %v", err)
	}
	for name, change := range map[string]Change{"the credential change": credentialChange, "the account change": accountChange} {
		if !change.RecordedAt.Equal(recordedAt) {
			t.Errorf("%s recorded_at = %v, want the instant it was given (%v); normalising the zone may never move the moment", name, change.RecordedAt, recordedAt)
		}
		if change.RecordedAt.Location() != time.UTC {
			t.Errorf("%s recorded_at stayed in %v, want UTC; only one zone renders the same at both copies of the log", name, change.RecordedAt.Location())
		}
	}
}

// TestAChangeAccessorHandsBackTheValueOfItsOwnKindOnly pins the kind-scoped
// accessors. Replay rebuilds the mirror row from exactly the value the change
// was built from, so the accessor of a change's own kind must hand that value
// back whole; the other kind's accessor answers with the zero value, which the
// caller's own kind switch makes unreachable — the same shape the JSON union
// decode uses, and no second error path invented for a question nobody asks.
func TestAChangeAccessorHandsBackTheValueOfItsOwnKindOnly(t *testing.T) {
	credential := mustCredential(t)
	account := mustAccount(t)
	credentialChange := mustCredentialChange(t, 4, credential)
	accountChange := mustAccountChange(t, 5, account)

	if got := credentialChange.Credential(); got != credential {
		t.Fatalf("Credential() on an api_key change = %+v, want the value the change was built from; replay rebuilds the mirror row from exactly this value", got)
	}
	if got := accountChange.Account(); got != account {
		t.Fatalf("Account() on an account change = %+v, want the value the change was built from; replay rebuilds the mirror row from exactly this value", got)
	}
	if got := credentialChange.Account(); got != (Account{}) {
		t.Fatalf("Account() on an api_key change = %+v, want the zero account; a change of one kind carries no value of the other", got)
	}
	if got := accountChange.Credential(); got != (Credential{}) {
		t.Fatalf("Credential() on an account change = %+v, want the zero credential; a change of one kind carries no value of the other", got)
	}
}

// TestAChangeRendersExactlyTheEnvelopeTheContractDeclares pins the five
// key/value pairs of the fragment's ProjectionChange, in the spellings the
// schema names. The schema closes the object — additionalProperties is false —
// so the assertion is of the whole key set and not of a few keys the test
// happens to care about: a field added on one side and not the other is a
// message the consumer refuses whole, exactly like a renamed one.
func TestAChangeRendersExactlyTheEnvelopeTheContractDeclares(t *testing.T) {
	raw, err := json.Marshal(mustCredentialChange(t, 7, mustCredential(t)))
	if err != nil {
		t.Fatalf("marshalling a constructor-built change: %v", err)
	}
	envelope := decodedObject(t, raw)

	wantKeys := []string{"payload", "recorded_at", "resource_id", "resource_kind", "revision"}
	if got := sortedKeys(envelope); !slices.Equal(got, wantKeys) {
		t.Errorf("change envelope keys = %v, want exactly %v; ProjectionChange closes its object, so a field on one side and not the other is a message the consumer refuses whole rather than tolerates", got, wantKeys)
	}
	if got, ok := envelope["revision"].(float64); !ok || got != 7 {
		t.Errorf("change revision = %v, want 7; the revision is the log's only ordering key and the consumer's guard, and a rendering that lost it loses the whole delivery model", envelope["revision"])
	}
	if got, _ := envelope["resource_kind"].(string); got != string(KindAPIKey) {
		t.Errorf("change resource_kind = %v, want %q; the kind is what the consumer's mirror routes the row on", envelope["resource_kind"], KindAPIKey)
	}
	if got, _ := envelope["resource_id"].(string); got != validKeyID {
		t.Errorf("change resource_id = %v, want %q; the id is what makes redelivery a no-op on an existing row", envelope["resource_id"], validKeyID)
	}
	if got, _ := envelope["recorded_at"].(string); got != "2026-09-25T12:00:00Z" {
		t.Errorf("change recorded_at = %v, want the RFC 3339 rendering of the recorded instant in UTC; operators on both planes read it, and one change must not render in two zones", envelope["recorded_at"])
	}
}

// TestACredentialPayloadCarriesTheWholeRow pins the api_key payload as the
// record's fields less the identity the envelope carries. The payload is
// stored in the producer's own projection table and delivered unchanged, so
// the stored copy and the delivered copy of a revision are the same bytes by
// construction — which is only true while the rendering is pinned, field by
// field, including `revoked_at` present as null where nothing was revoked,
// because the fragment lists it among the required fields and null is the
// wire's spelling for an active row.
func TestACredentialPayloadCarriesTheWholeRow(t *testing.T) {
	t.Run("an active row renders revoked_at as null", func(t *testing.T) {
		raw, err := json.Marshal(mustCredentialChange(t, 7, mustCredential(t)))
		if err != nil {
			t.Fatalf("marshalling an api_key change: %v", err)
		}
		payload := changePayload(t, raw)
		wantKeys := []string{"account_id", "digest", "revoked_at", "state"}
		if got := sortedKeys(payload); !slices.Equal(got, wantKeys) {
			t.Errorf("api_key payload keys = %v, want exactly %v; the payload is the mirror row's whole state, and an omitted field is a fact the runtime never learns", got, wantKeys)
		}
		for key, want := range map[string]string{
			"account_id": validAccountID,
			"digest":     validDigest,
			"state":      string(CredentialActive),
		} {
			if got, _ := payload[key].(string); got != want {
				t.Errorf("payload %s = %v, want %q; a misspelled field decodes to a zero value at the consumer rather than failing, which is the quietest way to mirror a wrong row", key, payload[key], want)
			}
		}
		if got, present := payload["revoked_at"]; !present || got != nil {
			t.Errorf("payload revoked_at = %v (present: %v), want an explicit null; the fragment requires the field, and null is its spelling for a row nothing was revoked from", got, present)
		}
	})
	t.Run("a revoked row renders its instant in UTC", func(t *testing.T) {
		raw, err := json.Marshal(mustCredentialChange(t, 7, mustRevokedCredential(t)))
		if err != nil {
			t.Fatalf("marshalling a revoked api_key change: %v", err)
		}
		payload := changePayload(t, raw)
		if got, _ := payload["state"].(string); got != string(CredentialRevoked) {
			t.Errorf("payload state = %v, want %q; the terminal state is what stops the mirror row admitting", payload["state"], CredentialRevoked)
		}
		if got, _ := payload["revoked_at"].(string); got != "2026-09-25T12:00:00Z" {
			t.Errorf("payload revoked_at = %v, want the instant rendered in UTC; the revocation was recorded in another zone and must not arrive spelling a different moment than the producer's own stored copy", payload["revoked_at"])
		}
	})
}

// TestAnAccountChangeRendersTheLifecycleAlone pins the account half of the
// log: an envelope like the credential's, around a payload of the lifecycle
// state and nothing else. An account payload that grew a field would mirror a
// fact the ownership record never decided to project.
func TestAnAccountChangeRendersTheLifecycleAlone(t *testing.T) {
	raw, err := json.Marshal(mustAccountChange(t, 3, mustAccount(t)))
	if err != nil {
		t.Fatalf("marshalling an account change: %v", err)
	}
	envelope := decodedObject(t, raw)
	wantKeys := []string{"payload", "recorded_at", "resource_id", "resource_kind", "revision"}
	if got := sortedKeys(envelope); !slices.Equal(got, wantKeys) {
		t.Errorf("account change envelope keys = %v, want exactly %v; ProjectionChange closes its object for both kinds alike", got, wantKeys)
	}
	if got, _ := envelope["resource_kind"].(string); got != string(KindAccount) {
		t.Errorf("account change resource_kind = %v, want %q; the kind is what the consumer's mirror routes the row on", envelope["resource_kind"], KindAccount)
	}
	if got, _ := envelope["resource_id"].(string); got != validAccountID {
		t.Errorf("account change resource_id = %v, want %q; the id is what makes redelivery a no-op on an existing row", envelope["resource_id"], validAccountID)
	}
	payload := changePayload(t, raw)
	wantPayloadKeys := []string{"state"}
	if got := sortedKeys(payload); !slices.Equal(got, wantPayloadKeys) {
		t.Errorf("account payload keys = %v, want exactly %v; the projected account is lifecycle only, and a field beyond it mirrors an ownership fact the runtime was never given", got, wantPayloadKeys)
	}
	if got, _ := payload["state"].(string); got != string(AccountActive) {
		t.Errorf("account payload state = %v, want %q; the state is the only thing the mirror row's admission decision reads", payload["state"], AccountActive)
	}
}

// TestAChangeBuiltOutsideTheConstructorsRendersNothing pins the refusal that
// keeps the envelope honest: the payload is derived from the kind, and a
// Change assembled by no constructor carries a kind that names no payload.
// Rendering it would mean inventing a payload a consumer could apply, which is
// the one outcome worse than failing loudly.
func TestAChangeBuiltOutsideTheConstructorsRendersNothing(t *testing.T) {
	raw, err := json.Marshal(Change{})
	if err == nil {
		t.Fatalf("marshalling a Change no constructor built produced %s; a kind that names no payload must fail the rendering rather than emit a row a consumer could apply", raw)
	}
}

// changePayload returns the payload object of a rendered change, failing the
// test when the envelope carries no object there.
func changePayload(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	payload, ok := decodedObject(t, raw)["payload"].(map[string]any)
	if !ok {
		t.Fatalf("the change rendered no payload object in %s; the payload is the row's whole state and nothing else can carry it", raw)
	}
	return payload
}
