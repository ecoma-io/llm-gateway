package projection

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The grammar tests below are the domain's half of the protocol contract test
// matrix (the task's §23.1): what a valid snapshot, a valid batch and a valid
// change look like, and every way a message can be malformed, missing a field,
// carrying an unknown version or a revision that cannot join a position. The
// wire-level halves of those rows live with the listener that answers them;
// what is pinned here is that the grammar itself refuses, whichever adapter
// built the message.

// The one epoch and the ids shared by the happy-path fixtures. They are valid
// canonical UUIDs minted here rather than constants of the protocol.
const (
	epoch    = "0b6fd7a1-3f6e-4a55-9a21-5c8f2e7d1b90"
	keyID    = "11111111-2222-4333-8444-555555555555"
	account  = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	otherKey = "99999999-8888-4777-8666-555555555555"
)

func mustCredential(t *testing.T, keyID, digest string, state CredentialState, revokedAt *time.Time) APIKeyCredential {
	t.Helper()
	record, err := NewAPIKeyCredential(keyID, account, digest, state, revokedAt)
	if err != nil {
		t.Fatalf("a credential the fixtures rely on was refused: %v", err)
	}
	return record
}

func TestACredentialWithValidGrammarIsAccepted(t *testing.T) {
	revoked := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	record := mustCredential(t, keyID, strings.Repeat("a", 64), CredentialRevoked, &revoked)

	if record.KeyID != keyID || record.AccountID != account {
		t.Errorf("the record's identities moved: %+v", record)
	}
	if record.State != CredentialRevoked || record.RevokedAt == nil {
		t.Errorf("the record's lifecycle moved: %+v", record)
	}
}

func TestACredentialOutsideTheGrammarIsRefused(t *testing.T) {
	revoked := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		keyID     string
		accountID string
		digest    string
		state     CredentialState
		revokedAt *time.Time
	}{
		{name: "a key id that is not a UUID", keyID: "key_1", accountID: account, digest: strings.Repeat("a", 64), state: CredentialActive},
		{name: "an account id that is not a UUID", keyID: keyID, accountID: "acct_1", digest: strings.Repeat("a", 64), state: CredentialActive},
		{name: "a digest too short to be a SHA-256", keyID: keyID, accountID: account, digest: strings.Repeat("a", 63), state: CredentialActive},
		{name: "a digest in upper case", keyID: keyID, accountID: account, digest: strings.Repeat("A", 64), state: CredentialActive},
		{name: "a digest carrying a non-hex character", keyID: keyID, accountID: account, digest: strings.Repeat("a", 63) + "g", state: CredentialActive},
		{name: "a state the credential lifecycle does not name", keyID: keyID, accountID: account, digest: strings.Repeat("a", 64), state: CredentialState("expired")},
		{name: "a revoked credential with no instant", keyID: keyID, accountID: account, digest: strings.Repeat("a", 64), state: CredentialRevoked},
		{name: "an active credential carrying a revocation instant", keyID: keyID, accountID: account, digest: strings.Repeat("a", 64), state: CredentialActive, revokedAt: &revoked},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewAPIKeyCredential(tt.keyID, tt.accountID, tt.digest, tt.state, tt.revokedAt)
			if !errors.Is(err, ErrBatchShape) {
				t.Fatalf("err = %v, want it to wrap ErrBatchShape", err)
			}
		})
	}
}

func TestAnAccountOutsideTheGrammarIsRefused(t *testing.T) {
	if _, err := NewAccountState("acct_1", LifecycleActive); !errors.Is(err, ErrBatchShape) {
		t.Errorf("err = %v, want it to wrap ErrBatchShape", err)
	}
	if _, err := NewAccountState(account, AccountLifecycle("archived")); !errors.Is(err, ErrBatchShape) {
		t.Errorf("err = %v, want it to wrap ErrBatchShape", err)
	}
	if _, err := NewAccountState(account, LifecycleSuspended); err != nil {
		t.Errorf("a suspended account was refused: %v", err)
	}
	if _, err := NewAccountState(account, LifecycleClosed); err != nil {
		t.Errorf("a closed account was refused: %v", err)
	}
}

func validSnapshot(t *testing.T) Snapshot {
	revoked := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	return Snapshot{
		ProtocolVersion:  ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 7,
		APIKeys:          []APIKeyCredential{mustCredential(t, keyID, strings.Repeat("b", 64), CredentialRevoked, &revoked)},
		Accounts:         []AccountState{{AccountID: account, State: LifecycleActive}},
	}
}

func TestASnapshotWithValidGrammarIsAccepted(t *testing.T) {
	if err := ValidateSnapshot(validSnapshot(t)); err != nil {
		t.Errorf("a grammar-valid snapshot was refused: %v", err)
	}
}

// TestAnEmptySnapshotIsALegitimateOne pins the bootstrap of an empty plane: a
// producer that has projected nothing yet sends empty arrays at boundary zero,
// and refusing that would make a fresh deployment unable to ever bootstrap.
func TestAnEmptySnapshotIsALegitimateOne(t *testing.T) {
	snapshot := validSnapshot(t)
	snapshot.SnapshotRevision = 0
	snapshot.APIKeys = nil
	snapshot.Accounts = nil

	if err := ValidateSnapshot(snapshot); err != nil {
		t.Errorf("an empty snapshot was refused: %v", err)
	}
}

func TestASnapshotOutsideTheGrammarIsRefused(t *testing.T) {
	tests := []struct {
		name     string
		snapshot Snapshot
		want     error
	}{
		{
			name: "a version this plane does not speak",
			snapshot: func() Snapshot {
				snapshot := validSnapshot(t)
				snapshot.ProtocolVersion = ProtocolVersion + 1
				return snapshot
			}(),
			want: ErrUnsupportedVersion,
		},
		{
			name: "an epoch that is not a UUID",
			snapshot: func() Snapshot {
				snapshot := validSnapshot(t)
				snapshot.Epoch = "timeline-1"
				return snapshot
			}(),
			want: ErrBatchShape,
		},
		{
			name: "a boundary past the ceiling the store can hold",
			snapshot: func() Snapshot {
				snapshot := validSnapshot(t)
				snapshot.SnapshotRevision = MaxRevision + 1
				return snapshot
			}(),
			want: ErrBatchShape,
		},
		{
			name: "more api keys than the contract bounds",
			snapshot: func() Snapshot {
				snapshot := validSnapshot(t)
				snapshot.APIKeys = make([]APIKeyCredential, MaxSnapshotRecords+1)
				for i := range snapshot.APIKeys {
					snapshot.APIKeys[i] = mustCredential(t, keyID, strings.Repeat("a", 64), CredentialActive, nil)
				}
				return snapshot
			}(),
			want: ErrBatchShape,
		},
		{
			name: "more accounts than the contract bounds",
			snapshot: func() Snapshot {
				snapshot := validSnapshot(t)
				snapshot.Accounts = make([]AccountState, MaxSnapshotRecords+1)
				for i := range snapshot.Accounts {
					snapshot.Accounts[i] = AccountState{AccountID: account, State: LifecycleActive}
				}
				return snapshot
			}(),
			want: ErrBatchShape,
		},
		{
			name: "a credential inside the snapshot that breaks the grammar",
			snapshot: func() Snapshot {
				snapshot := validSnapshot(t)
				snapshot.APIKeys[0].Digest = "short"
				return snapshot
			}(),
			want: ErrBatchShape,
		},
		{
			name: "an account inside the snapshot that breaks the grammar",
			snapshot: func() Snapshot {
				snapshot := validSnapshot(t)
				snapshot.Accounts[0].State = "archived"
				return snapshot
			}(),
			want: ErrBatchShape,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSnapshot(tt.snapshot)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want it to wrap %v", err, tt.want)
			}
		})
	}
}

// payloadFor builds a change payload the way the Control Plane's log writes
// it: only the fields the kind carries, plus whatever additive fields a later
// producer version has added.
func payloadFor(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("building a payload: %v", err)
	}
	return raw
}

func recordedAt() time.Time {
	return time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
}

func apiKeyPayload(extra map[string]any) map[string]any {
	fields := map[string]any{
		"account_id": account,
		"digest":     strings.Repeat("c", 64),
		"state":      "active",
		"revoked_at": nil,
	}
	for key, value := range extra {
		fields[key] = value
	}
	return fields
}

func TestAChangeWithValidPayloadsIsAccepted(t *testing.T) {
	revoked := recordedAt().Add(time.Minute)
	credential, err := NewChange(3, KindAPIKey, keyID, recordedAt(), payloadFor(t, apiKeyPayload(map[string]any{
		"state":      "revoked",
		"revoked_at": revoked.Format(time.RFC3339),
	})))
	if err != nil {
		t.Fatalf("an api_key change was refused: %v", err)
	}
	if credential.Credential == nil || credential.Account != nil {
		t.Fatalf("an api_key change did not carry a credential record: %+v", credential)
	}
	if credential.Credential.KeyID != keyID || credential.Credential.State != CredentialRevoked {
		t.Errorf("the credential record did not survive the parse: %+v", credential.Credential)
	}

	state, err := NewChange(4, KindAccount, account, recordedAt(), payloadFor(t, map[string]any{"state": "suspended"}))
	if err != nil {
		t.Fatalf("an account change was refused: %v", err)
	}
	if state.Account == nil || state.Credential != nil {
		t.Fatalf("an account change did not carry an account record: %+v", state)
	}
	if state.Account.State != LifecycleSuspended {
		t.Errorf("the account record did not survive the parse: %+v", state.Account)
	}
}

// TestAChangeToleratesUnknownPayloadFields pins the one additive direction the
// protocol may evolve in: a later producer writing an extra field into the
// payload is delivered, parsed for the fields this plane knows, and applied —
// not refused. A new state value or a missing known field is the opposite and
// is refused by the grammar tests either side of this one.
func TestAChangeToleratesUnknownPayloadFields(t *testing.T) {
	change, err := NewChange(5, KindAPIKey, keyID, recordedAt(), payloadFor(t, apiKeyPayload(map[string]any{
		"policy_ref":   "pr_1",
		"future_field": map[string]any{"nested": true},
	})))
	if err != nil {
		t.Fatalf("a payload with additive fields was refused: %v", err)
	}
	if change.Credential == nil || change.Credential.Digest != strings.Repeat("c", 64) {
		t.Errorf("the known fields did not survive the additive fields: %+v", change.Credential)
	}
}

func TestAChangeOutsideTheGrammarIsRefused(t *testing.T) {
	tests := []struct {
		name       string
		revision   uint64
		kind       ResourceKind
		resourceID string
		payload    json.RawMessage
		want       error
	}{
		{name: "revision zero", revision: 0, kind: KindAPIKey, resourceID: keyID, payload: payloadFor(t, apiKeyPayload(nil)), want: ErrBatchShape},
		{name: "a resource id that is not a UUID", revision: 1, kind: KindAPIKey, resourceID: "key_1", payload: payloadFor(t, apiKeyPayload(nil)), want: ErrBatchShape},
		{name: "no recording instant", revision: 1, kind: KindAPIKey, resourceID: keyID, payload: payloadFor(t, apiKeyPayload(nil)), want: ErrBatchShape},
		{name: "an empty payload", revision: 1, kind: KindAPIKey, resourceID: keyID, payload: json.RawMessage("{}"), want: ErrBatchShape},
		{name: "a payload missing the digest", revision: 1, kind: KindAPIKey, resourceID: keyID, payload: payloadFor(t, func() map[string]any {
			fields := apiKeyPayload(nil)
			delete(fields, "digest")
			return fields
		}()), want: ErrBatchShape},
		{name: "a payload missing the account id", revision: 1, kind: KindAPIKey, resourceID: keyID, payload: payloadFor(t, func() map[string]any {
			fields := apiKeyPayload(nil)
			delete(fields, "account_id")
			return fields
		}()), want: ErrBatchShape},
		{name: "a payload with a null digest", revision: 1, kind: KindAPIKey, resourceID: keyID, payload: payloadFor(t, func() map[string]any {
			fields := apiKeyPayload(nil)
			fields["digest"] = nil
			return fields
		}()), want: ErrBatchShape},
		{name: "an account payload missing its state", revision: 1, kind: KindAccount, resourceID: account, payload: payloadFor(t, map[string]any{"other": 1}), want: ErrBatchShape},
		{name: "a resource kind this plane does not mirror", revision: 1, kind: "quota", resourceID: account, payload: payloadFor(t, map[string]any{"state": "active"}), want: ErrBatchShape},
		{name: "a payload that is not an object", revision: 1, kind: KindAPIKey, resourceID: keyID, payload: json.RawMessage(`[1,2]`), want: ErrBatchShape},
		{name: "a revision past the ceiling the store can hold", revision: MaxRevision + 1, kind: KindAPIKey, resourceID: keyID, payload: payloadFor(t, apiKeyPayload(nil)), want: ErrBatchShape},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := recordedAt()
			if tt.name == "no recording instant" {
				at = time.Time{}
			}
			_, err := NewChange(tt.revision, tt.kind, tt.resourceID, at, tt.payload)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want it to wrap %v", err, tt.want)
			}
		})
	}
}

func validBatch(t *testing.T) Batch {
	first, err := NewChange(11, KindAccount, account, recordedAt(), payloadFor(t, map[string]any{"state": "active"}))
	if err != nil {
		panic(err)
	}
	second, err := NewChange(12, KindAPIKey, keyID, recordedAt(), payloadFor(t, apiKeyPayload(nil)))
	if err != nil {
		panic(err)
	}
	return Batch{
		ProtocolVersion: ProtocolVersion,
		Epoch:           epoch,
		FromRevision:    10,
		Changes:         []Change{first, second},
	}
}

func TestABatchWithValidGrammarIsAccepted(t *testing.T) {
	if err := ValidateBatch(validBatch(t)); err != nil {
		t.Errorf("a grammar-valid batch was refused: %v", err)
	}
}

func TestABatchOutsideTheGrammarIsRefused(t *testing.T) {
	tests := []struct {
		name  string
		batch Batch
		want  error
	}{
		{
			name: "a version this plane does not speak",
			batch: func() Batch {
				batch := validBatch(t)
				batch.ProtocolVersion = 0
				return batch
			}(),
			want: ErrUnsupportedVersion,
		},
		{
			name: "an epoch that is not a UUID",
			batch: func() Batch {
				batch := validBatch(t)
				batch.Epoch = ""
				return batch
			}(),
			want: ErrBatchShape,
		},
		{name: "no changes at all", batch: func() Batch {
			batch := validBatch(t)
			batch.Changes = nil
			return batch
		}(), want: ErrBatchShape},
		{name: "more changes than the contract bounds", batch: func() Batch {
			batch := validBatch(t)
			for i := 0; len(batch.Changes) <= MaxChangesPerBatch; i++ {
				change, err := NewChange(uint64(13+i), KindAccount, account, recordedAt(), payloadFor(t, map[string]any{"state": "active"}))
				if err != nil {
					panic(err)
				}
				batch.Changes = append(batch.Changes, change)
			}
			return batch
		}(), want: ErrBatchShape},
		{
			name: "revisions that are not contiguous",
			batch: func() Batch {
				batch := validBatch(t)
				batch.Changes[1].Revision = 14
				return batch
			}(),
			want: ErrBatchShape,
		},
		{
			name: "revisions that are not ascending",
			batch: func() Batch {
				batch := validBatch(t)
				batch.Changes[1].Revision = 11
				return batch
			}(),
			want: ErrBatchShape,
		},
		{
			name: "a first revision of zero",
			batch: func() Batch {
				batch := validBatch(t)
				batch.Changes[0].Revision = 0
				return batch
			}(),
			want: ErrBatchShape,
		},
		{
			name: "a from-position past the ceiling the store can hold",
			batch: func() Batch {
				batch := validBatch(t)
				batch.FromRevision = MaxRevision + 1
				return batch
			}(),
			want: ErrBatchShape,
		},
		{
			name: "a change revision past the ceiling the store can hold",
			batch: func() Batch {
				batch := validBatch(t)
				batch.Changes[0].Revision = MaxRevision
				batch.Changes[1].Revision = MaxRevision + 1
				return batch
			}(),
			want: ErrBatchShape,
		},
		{
			name: "an api_key change carrying no credential record",
			batch: func() Batch {
				batch := validBatch(t)
				batch.Changes[1].Credential = nil
				return batch
			}(),
			want: ErrBatchShape,
		},
		{
			name: "a record whose identity disagrees with the entry",
			batch: func() Batch {
				batch := validBatch(t)
				batch.Changes[1].Credential.KeyID = otherKey
				return batch
			}(),
			want: ErrBatchShape,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBatch(tt.batch)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want it to wrap %v", err, tt.want)
			}
		})
	}
}

// TestDecideBatchIsTheThreeCaseRule pins the whole of the position judgement:
// a batch behind the position is the lost acknowledgement and is re-acked, a
// batch joining it exactly is applied, and everything else — straddling or
// overshooting — is a gap refused whole. These three are the only answers;
// there is no fourth "apply what fits".
func TestDecideBatchIsTheThreeCaseRule(t *testing.T) {
	change := func(revision uint64) Change {
		c, err := NewChange(revision, KindAccount, account, recordedAt(), payloadFor(t, map[string]any{"state": "active"}))
		if err != nil {
			panic(err)
		}
		return c
	}
	batch := func(revisions ...uint64) Batch {
		changes := make([]Change, len(revisions))
		for i, revision := range revisions {
			changes[i] = change(revision)
		}
		return Batch{ProtocolVersion: ProtocolVersion, Epoch: epoch, Changes: changes}
	}

	tests := []struct {
		name    string
		applied uint64
		batch   Batch
		want    Decision
	}{
		{name: "the duplicate delivered whole again", applied: 10, batch: batch(9, 10), want: DecisionDuplicate},
		{name: "a single entry already applied", applied: 10, batch: batch(10), want: DecisionDuplicate},
		{name: "the batch that joins exactly", applied: 10, batch: batch(11, 12), want: DecisionApply},
		{name: "the first batch after bootstrap", applied: 0, batch: batch(1, 2), want: DecisionApply},
		{name: "a batch straddling the position", applied: 10, batch: batch(10, 11), want: DecisionGap},
		{name: "a batch overshooting the position", applied: 10, batch: batch(12, 13), want: DecisionGap},
		{name: "a batch from a position nothing has earned", applied: 0, batch: batch(5, 6), want: DecisionGap},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecideBatch(tt.applied, tt.batch); got != tt.want {
				t.Errorf("DecideBatch(%d, [%d..%d]) = %d, want %d", tt.applied, tt.batch.Changes[0].Revision, tt.batch.Changes[len(tt.batch.Changes)-1].Revision, got, tt.want)
			}
		})
	}
}

// TestTerminalStatesAreNeverLeftPins the consumer-side lifecycle rule the
// grammar alone does not carry: revoked and closed are terminal on both
// planes, so a delivered state that would move a row out of one is a
// regression the applier refuses — while the moves a conforming producer
// makes, including the backwards-looking ones recovery legitimately needs a
// snapshot for, all read as "no regression".
func TestTerminalStatesAreNeverLeft(t *testing.T) {
	credentialTests := []struct {
		name      string
		applied   CredentialState
		delivered CredentialState
		want      bool
	}{
		{name: "a revoked credential delivered as active", applied: CredentialRevoked, delivered: CredentialActive, want: true},
		{name: "a revoked credential redelivered as revoked", applied: CredentialRevoked, delivered: CredentialRevoked, want: false},
		{name: "an active credential delivered as revoked", applied: CredentialActive, delivered: CredentialRevoked, want: false},
		{name: "an active credential delivered as active", applied: CredentialActive, delivered: CredentialActive, want: false},
	}
	for _, tt := range credentialTests {
		t.Run("credential: "+tt.name, func(t *testing.T) {
			if got := CredentialRegressesTerminal(tt.applied, tt.delivered); got != tt.want {
				t.Errorf("CredentialRegressesTerminal(%q, %q) = %t, want %t", tt.applied, tt.delivered, got, tt.want)
			}
		})
	}

	accountTests := []struct {
		name      string
		applied   AccountLifecycle
		delivered AccountLifecycle
		want      bool
	}{
		{name: "a closed account delivered as active", applied: LifecycleClosed, delivered: LifecycleActive, want: true},
		{name: "a closed account delivered as suspended", applied: LifecycleClosed, delivered: LifecycleSuspended, want: true},
		{name: "a closed account redelivered as closed", applied: LifecycleClosed, delivered: LifecycleClosed, want: false},
		{name: "a suspended account delivered as active", applied: LifecycleSuspended, delivered: LifecycleActive, want: false},
		{name: "an active account delivered as closed", applied: LifecycleActive, delivered: LifecycleClosed, want: false},
	}
	for _, tt := range accountTests {
		t.Run("account: "+tt.name, func(t *testing.T) {
			if got := AccountRegressesTerminal(tt.applied, tt.delivered); got != tt.want {
				t.Errorf("AccountRegressesTerminal(%q, %q) = %t, want %t", tt.applied, tt.delivered, got, tt.want)
			}
		})
	}
}
