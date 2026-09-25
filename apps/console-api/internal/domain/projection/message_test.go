package projection

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

// syntheticKey returns the credential at index i of a synthetic projection.
// The identity is counted in the id's last block rather than drawn from one
// shared fixture, so a bound test can ask for five thousand of them and every
// one is its own row in the array it populates.
func syntheticKey(t *testing.T, i int) Credential {
	t.Helper()
	key, err := NewCredential(fmt.Sprintf("019203d0-9a1b-4c2a-8f1e-%012x", i), validAccountID, validDigest, CredentialActive, nil)
	if err != nil {
		t.Fatalf("NewCredential refused synthetic credential %d: %v", i, err)
	}
	return key
}

// syntheticAccount is the account twin of syntheticKey.
func syntheticAccount(t *testing.T, i int) Account {
	t.Helper()
	account, err := NewAccount(fmt.Sprintf("5e7b1c9a-2d4f-4a6b-9c8d-%012x", i), AccountActive)
	if err != nil {
		t.Fatalf("NewAccount refused synthetic account %d: %v", i, err)
	}
	return account
}

// syntheticKeys builds count distinct credentials.
func syntheticKeys(t *testing.T, count int) []Credential {
	t.Helper()
	keys := make([]Credential, count)
	for i := range keys {
		keys[i] = syntheticKey(t, i)
	}
	return keys
}

// syntheticAccounts builds count distinct accounts.
func syntheticAccounts(t *testing.T, count int) []Account {
	t.Helper()
	accounts := make([]Account, count)
	for i := range accounts {
		accounts[i] = syntheticAccount(t, i)
	}
	return accounts
}

// batchChanges builds the legal batch body for a position: count entries,
// contiguous from from+1.
func batchChanges(t *testing.T, from uint64, count int) []Change {
	t.Helper()
	changes := make([]Change, count)
	for i := range changes {
		changes[i] = mustCredentialChange(t, from+uint64(i)+1, mustCredential(t))
	}
	return changes
}

// mustBatch builds the legal batch for a position, failing the test when the
// grammar refuses what these fixtures assembled.
func mustBatch(t *testing.T, from uint64, count int) Batch {
	t.Helper()
	batch, err := NewBatch(validEpoch, from, batchChanges(t, from, count))
	if err != nil {
		t.Fatalf("NewBatch refused a batch of %d changes from position %d: %v", count, from, err)
	}
	return batch
}

// mustSnapshot builds a snapshot, failing the test when the grammar refuses
// what these fixtures assembled.
func mustSnapshot(t *testing.T, revision uint64, keys []Credential, accounts []Account) Snapshot {
	t.Helper()
	snapshot, err := NewSnapshot(validEpoch, revision, keys, accounts)
	if err != nil {
		t.Fatalf("NewSnapshot refused %d credentials and %d accounts at revision %d: %v", len(keys), len(accounts), revision, err)
	}
	return snapshot
}

// changesAt builds changes carrying exactly the revisions asked for. Revision
// zero is built as a bare value on purpose: no constructor mints one, and the
// batch check exists to refuse one should a value like it ever exist anyway.
func changesAt(t *testing.T, revisions []uint64) []Change {
	t.Helper()
	changes := make([]Change, len(revisions))
	for i, revision := range revisions {
		if revision == 0 {
			changes[i] = Change{Revision: 0, Kind: KindAPIKey, ResourceID: validKeyID}
			continue
		}
		changes[i] = mustCredentialChange(t, revision, mustCredential(t))
	}
	return changes
}

// TestNewBatchRefusesAnEmptyChangeList pins the line between a delivery and
// the decision not to send one. The loop that has nothing to deliver simply
// does not call, so an empty `changes` array can only mean a producer that
// assembled its message wrong — and a consumer that applied one would advance
// its position past revisions that do not exist.
func TestNewBatchRefusesAnEmptyChangeList(t *testing.T) {
	for name, changes := range map[string][]Change{"nil": nil, "empty": {}} {
		if _, err := NewBatch(validEpoch, 0, changes); err == nil {
			t.Errorf("NewBatch accepted a %s change list; an empty batch is not a delivery, and applying one would advance a position past revisions that do not exist", name)
		}
	}
}

// TestNewBatchHoldsTheChangeCountToTheContractsBound pins `changes.maxItems`
// from both sides: a batch of exactly the bound is legal, and one past it is
// refused before any bytes exist. The bound is the producer's batching size
// and the consumer's refusal line at once, so a build whose two copies of the
// number disagree would deliver batches one side always refuses.
func TestNewBatchHoldsTheChangeCountToTheContractsBound(t *testing.T) {
	if _, err := NewBatch(validEpoch, 0, batchChanges(t, 0, MaxChangesPerBatch)); err != nil {
		t.Fatalf("NewBatch refused %d changes, which is exactly the contract's bound: %v", MaxChangesPerBatch, err)
	}
	if _, err := NewBatch(validEpoch, 0, batchChanges(t, 0, MaxChangesPerBatch+1)); err == nil {
		t.Errorf("NewBatch accepted %d changes, one past the contract's bound of %d; a producer released against a wider bound delivers batches its consumer refuses whole", MaxChangesPerBatch+1, MaxChangesPerBatch)
	}
}

// TestNewBatchRequiresContiguousRevisionsAscendingFromThePosition pins the
// producer's half of the delivery rule: a batch joins the consumer's position
// exactly, beginning at the revision after FromRevision. The consumer refuses
// a batch that does not join before applying any of it, and that refusal is
// only meaningful against batches the producer built to join — which is why a
// gap, a repeat, a backwards step or a wrong start is refused here, where the
// batch is assembled, rather than discovered by a consumer holding a position
// it can no longer reason about.
func TestNewBatchRequiresContiguousRevisionsAscendingFromThePosition(t *testing.T) {
	tests := []struct {
		name      string
		from      uint64
		revisions []uint64
	}{
		{name: "the first entry is the position itself, not its successor", from: 10, revisions: []uint64{10, 11}},
		{name: "a gap opens after the first entry", from: 10, revisions: []uint64{11, 13}},
		{name: "an entry repeats a revision", from: 10, revisions: []uint64{11, 11, 12}},
		{name: "an entry runs backwards", from: 10, revisions: []uint64{12, 11}},
		{name: "the first entry is revision zero", from: 0, revisions: []uint64{0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewBatch(validEpoch, tt.from, changesAt(t, tt.revisions)); err == nil {
				t.Errorf("NewBatch accepted the revisions %v from position %d; only %d, %d, ... joins a position, and a batch that does not join would be refused whole by the very consumer it was built for", tt.revisions, tt.from, tt.from+1, tt.from+2)
			}
		})
	}
}

// TestNewBatchAcceptsALegalBatchAndNamesItsLastRevision pins the ordinary
// delivery end to end: the batch keeps the position it was read from, carries
// the revisions that join it, and reports the position its consumer holds once
// the whole batch is applied and committed. That last number is what the next
// delivery is ordered against, so a miscount would not merely misreport a
// batch — it would misorder every batch after it.
func TestNewBatchAcceptsALegalBatchAndNamesItsLastRevision(t *testing.T) {
	const from = 41
	batch := mustBatch(t, from, 3)
	if batch.Epoch != validEpoch {
		t.Fatalf("batch epoch = %q, want the timeline it was read on; the epoch is how a position is known to join the log at all", batch.Epoch)
	}
	if batch.FromRevision != from {
		t.Fatalf("batch from_revision = %d, want %d; the field is what a refusal names when a batch fails to join", batch.FromRevision, from)
	}
	if len(batch.Changes) != 3 {
		t.Fatalf("batch carries %d changes, want the 3 it was built from", len(batch.Changes))
	}
	for i, change := range batch.Changes {
		if want := from + uint64(i) + 1; change.Revision != want {
			t.Fatalf("batch entry %d carries revision %d, want the contiguous %d; the array is the log's slice, in order", i, change.Revision, want)
		}
	}
	if got := batch.LastRevision(); got != from+3 {
		t.Fatalf("LastRevision = %d, want %d; that is the position the consumer holds after applying the whole batch, and a miscount orders the next delivery against the wrong successor", got, from+3)
	}
	if got := mustBatch(t, 0, 1).LastRevision(); got != 1 {
		t.Fatalf("LastRevision of a batch from 0 with one change = %d, want 1; a fresh timeline's first delivery ends at the first revision the counter allocated", got)
	}
}

// TestEveryMessageNamesItsTimelineInAWellFormedEpochOrIsRefused pins the epoch
// grammar across every message the delivery loop sends and reads. The epoch is
// how the pair notices that a position no longer joins the log (ADR 0007 §6),
// and the migration that founds a timeline generates a UUID: a message
// whose timeline is unnamed, misspelled or foreign would ask a consumer to
// trust a position no recovery story can reason about.
func TestEveryMessageNamesItsTimelineInAWellFormedEpochOrIsRefused(t *testing.T) {
	for name, epoch := range uuidViolations() {
		if _, err := NewBatch(epoch, 0, batchChanges(t, 0, 1)); err == nil {
			t.Errorf("NewBatch accepted the %s epoch %q; a timeline must be named in the form the contract's `format: uuid` declares", name, epoch)
		}
		if _, err := NewSnapshot(epoch, 0, nil, nil); err == nil {
			t.Errorf("NewSnapshot accepted the %s epoch %q; a snapshot must name the timeline whose state it cuts, or its boundary is a position nothing later joins", name, epoch)
		}
		if _, err := NewHead(epoch, 0); err == nil {
			t.Errorf("NewHead accepted the %s epoch %q; a malformed epoch is a corrupted counter row, and delivering it would teach consumers a position no recovery story can reason about", name, epoch)
		}
	}
	if _, err := NewBatch(validEpoch, 0, batchChanges(t, 0, 1)); err != nil {
		t.Fatalf("NewBatch refused a well-formed epoch: %v", err)
	}
	if _, err := NewSnapshot(validEpoch, 0, nil, nil); err != nil {
		t.Fatalf("NewSnapshot refused a well-formed epoch: %v", err)
	}
	if _, err := NewHead(validEpoch, 0); err != nil {
		t.Fatalf("NewHead refused a well-formed epoch: %v", err)
	}
}

// TestABatchRendersExactlyTheEnvelopeTheContractDeclares pins the four
// key/value pairs of the fragment's ProjectionChangesBatch, in the spellings
// the schema names, with the protocol version pinned by this package rather
// than by any caller's argument. The schema closes the object, so the
// assertion is of the whole key set: a field added on one side and not the
// other is a message the consumer refuses whole, not a tolerated extra.
func TestABatchRendersExactlyTheEnvelopeTheContractDeclares(t *testing.T) {
	raw, err := json.Marshal(mustBatch(t, 41, 2))
	if err != nil {
		t.Fatalf("marshalling a constructor-built batch: %v", err)
	}
	envelope := decodedObject(t, raw)

	wantKeys := []string{"changes", "epoch", "from_revision", "protocol_version"}
	if got := sortedKeys(envelope); !slices.Equal(got, wantKeys) {
		t.Errorf("batch envelope keys = %v, want exactly %v; ProjectionChangesBatch closes its object, so a field on one side and not the other is a message the consumer refuses whole rather than tolerates", got, wantKeys)
	}
	if got, ok := envelope["protocol_version"].(float64); !ok || got != ProtocolVersion {
		t.Errorf("batch protocol_version = %v, want %d; the version is this package's to pin, and a hop that receives one it does not know must refuse the message outright", envelope["protocol_version"], ProtocolVersion)
	}
	if got, _ := envelope["epoch"].(string); got != validEpoch {
		t.Errorf("batch epoch = %v, want %q; the timeline's name is what makes the batch's position joinable", envelope["epoch"], validEpoch)
	}
	if got, ok := envelope["from_revision"].(float64); !ok || got != 41 {
		t.Errorf("batch from_revision = %v, want 41; the field is what a refusal names when a batch fails to join a position", envelope["from_revision"])
	}
	changes, ok := envelope["changes"].([]any)
	if !ok || len(changes) != 2 {
		t.Fatalf("batch changes = %v, want the 2 entries the batch was built from", envelope["changes"])
	}
	first, ok := changes[0].(map[string]any)
	if !ok {
		t.Fatalf("batch changes[0] = %v, want a ProjectionChange object; the array is the log's slice, entry by entry", changes[0])
	}
	if got, _ := first["revision"].(float64); got != 42 {
		t.Errorf("first change revision = %v, want 42; the array begins at the revision strictly after from_revision", first["revision"])
	}
}

// TestABatchRendersItsChangesAsAnArrayEvenWhenItCarriesNone pins the rendering
// promise the constructor refusal does not cover. NewBatch refuses an empty
// batch, but the rendering deliberately does not lean on that refusal: the
// schema types `changes` as an array, and a `null` where a consumer's decoder
// expects an array is exactly the kind of difference only a production
// incident finds.
func TestABatchRendersItsChangesAsAnArrayEvenWhenItCarriesNone(t *testing.T) {
	raw, err := json.Marshal(Batch{})
	if err != nil {
		t.Fatalf("marshalling a batch with no changes: %v", err)
	}
	rendered, ok := decodedObject(t, raw)["changes"].([]any)
	if !ok || rendered == nil || len(rendered) != 0 {
		t.Fatalf("a batch with no changes rendered %v for `changes`, want an empty JSON array; a nil slice must not become null on the wire", decodedObject(t, raw)["changes"])
	}
}

// TestNewSnapshotHoldsBothArraysToTheContractsBounds pins both maxItems
// declarations from both sides: at the bound a snapshot is legal, one past it
// is refused before any bytes exist. A snapshot is atomic and cannot chunk, so
// past the bound bootstrap is impossible by design — and a producer released
// against a wider bound would hand its consumer a message the schema refuses,
// discovered only when a wipe forced a bootstrap.
func TestNewSnapshotHoldsBothArraysToTheContractsBounds(t *testing.T) {
	t.Run("api_keys at the bound", func(t *testing.T) {
		if _, err := NewSnapshot(validEpoch, 0, syntheticKeys(t, MaxSnapshotKeys), nil); err != nil {
			t.Fatalf("NewSnapshot refused %d credentials, which is exactly the contract's bound: %v", MaxSnapshotKeys, err)
		}
	})
	t.Run("api_keys one past the bound", func(t *testing.T) {
		if _, err := NewSnapshot(validEpoch, 0, syntheticKeys(t, MaxSnapshotKeys+1), nil); err == nil {
			t.Errorf("NewSnapshot accepted %d credentials, one past the contract's bound of %d; a snapshot cannot chunk, so a bootstrap past the bound is impossible by design and must fail here rather than at the consumer", MaxSnapshotKeys+1, MaxSnapshotKeys)
		}
	})
	t.Run("accounts at the bound", func(t *testing.T) {
		if _, err := NewSnapshot(validEpoch, 0, nil, syntheticAccounts(t, MaxSnapshotAccounts)); err != nil {
			t.Fatalf("NewSnapshot refused %d accounts, which is exactly the contract's bound: %v", MaxSnapshotAccounts, err)
		}
	})
	t.Run("accounts one past the bound", func(t *testing.T) {
		if _, err := NewSnapshot(validEpoch, 0, nil, syntheticAccounts(t, MaxSnapshotAccounts+1)); err == nil {
			t.Errorf("NewSnapshot accepted %d accounts, one past the contract's bound of %d; a snapshot cannot chunk, so a bootstrap past the bound is impossible by design and must fail here rather than at the consumer", MaxSnapshotAccounts+1, MaxSnapshotAccounts)
		}
	})
}

// TestNewSnapshotAcceptsAnEmptyProjectionAtAZeroBoundary pins the case the
// fragment spells out in prose: an empty projection is a snapshot too, and
// zero is a legitimate boundary. A wiped consumer bootstrap's first answer is
// exactly this message, and refusing it would make an empty Control Plane the
// one state that could never be mirrored.
func TestNewSnapshotAcceptsAnEmptyProjectionAtAZeroBoundary(t *testing.T) {
	for name, keys := range map[string][]Credential{"nil": nil, "empty": {}} {
		snapshot, err := NewSnapshot(validEpoch, 0, keys, nil)
		if err != nil {
			t.Fatalf("NewSnapshot refused a %s key array at revision 0: %v", name, err)
		}
		if snapshot.Epoch != validEpoch {
			t.Fatalf("snapshot epoch = %q, want the timeline it was cut on", snapshot.Epoch)
		}
		if snapshot.Revision != 0 {
			t.Fatalf("snapshot revision = %d, want 0; zero is a legitimate boundary, and an empty projection cut at it is the answer a wiped consumer bootstraps from", snapshot.Revision)
		}
		if len(snapshot.Keys) != 0 || len(snapshot.Accounts) != 0 {
			t.Fatalf("snapshot carries %d credentials and %d accounts, want none; the producer had none, and inventing any would be mirroring a decision that was never made", len(snapshot.Keys), len(snapshot.Accounts))
		}
	}
}

// TestNewSnapshotAcceptsBothHalvesPopulatedAndKeepsThemWhole pins the ordinary
// bootstrap: both arrays carried in full, in order, at the boundary they were
// cut at. A snapshot means "be this state at this boundary", so a record lost
// between the constructor and the struct is a row the runtime silently never
// admits.
func TestNewSnapshotAcceptsBothHalvesPopulatedAndKeepsThemWhole(t *testing.T) {
	keys := []Credential{syntheticKey(t, 0), syntheticKey(t, 1)}
	accounts := []Account{syntheticAccount(t, 0)}
	snapshot := mustSnapshot(t, 7, keys, accounts)

	if snapshot.Revision != 7 {
		t.Fatalf("snapshot revision = %d, want 7; the boundary is the position the consumer's mirror is set to in the same transaction as the rows", snapshot.Revision)
	}
	if len(snapshot.Keys) != 2 || snapshot.Keys[0] != keys[0] || snapshot.Keys[1] != keys[1] {
		t.Fatalf("snapshot keys = %+v, want the 2 credentials it was built from, in order; a snapshot is unconditional, so a reordered or lost row is applied as the state of the world", snapshot.Keys)
	}
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0] != accounts[0] {
		t.Fatalf("snapshot accounts = %+v, want the 1 account it was built from; a snapshot is unconditional, so a lost row is applied as the state of the world", snapshot.Accounts)
	}
}

// TestASnapshotRendersExactlyTheEnvelopeTheContractDeclares pins the five
// key/value pairs of the fragment's ProjectionSnapshot, in the spellings the
// schema names, with the protocol version pinned by this package rather than
// by any caller's argument. The schema closes the object, so the assertion is
// of the whole key set and not of a few keys the test happens to care about.
func TestASnapshotRendersExactlyTheEnvelopeTheContractDeclares(t *testing.T) {
	raw, err := json.Marshal(mustSnapshot(t, 7, syntheticKeys(t, 1), syntheticAccounts(t, 1)))
	if err != nil {
		t.Fatalf("marshalling a constructor-built snapshot: %v", err)
	}
	envelope := decodedObject(t, raw)

	wantKeys := []string{"accounts", "api_keys", "epoch", "protocol_version", "snapshot_revision"}
	if got := sortedKeys(envelope); !slices.Equal(got, wantKeys) {
		t.Errorf("snapshot envelope keys = %v, want exactly %v; ProjectionSnapshot closes its object, so a field on one side and not the other is a message the consumer refuses whole rather than tolerates", got, wantKeys)
	}
	if got, ok := envelope["protocol_version"].(float64); !ok || got != ProtocolVersion {
		t.Errorf("snapshot protocol_version = %v, want %d; the version is this package's to pin, and a hop that receives one it does not know must refuse the message outright", envelope["protocol_version"], ProtocolVersion)
	}
	if got, _ := envelope["epoch"].(string); got != validEpoch {
		t.Errorf("snapshot epoch = %v, want %q; the timeline's name is what makes the boundary a position anything later can join", envelope["epoch"], validEpoch)
	}
	if got, ok := envelope["snapshot_revision"].(float64); !ok || got != 7 {
		t.Errorf("snapshot snapshot_revision = %v, want 7; the boundary is the position the snapshot sets, forward or backward, when it is applied", envelope["snapshot_revision"])
	}
	for name, want := range map[string]int{"api_keys": 1, "accounts": 1} {
		records, ok := envelope[name].([]any)
		if !ok || len(records) != want {
			t.Errorf("snapshot %s = %v, want the %d record(s) the snapshot was built from", name, envelope[name], want)
		}
	}
}

// TestASnapshotRendersTheCredentialRecordTheContractDeclares pins the wire
// record: the payload's fields plus the key id — the identity a change
// envelope carries and a snapshot has no envelope for, which is why the record
// and not the payload is the snapshot's unit. Every field is pinned, including
// `revoked_at` present as null on an active row, because the fragment lists it
// among the record's required fields.
func TestASnapshotRendersTheCredentialRecordTheContractDeclares(t *testing.T) {
	t.Run("an active credential", func(t *testing.T) {
		raw, err := json.Marshal(mustSnapshot(t, 7, syntheticKeys(t, 1), nil))
		if err != nil {
			t.Fatalf("marshalling a snapshot with one credential: %v", err)
		}
		record := snapshotRecord(t, raw, "api_keys", 0)
		wantKeys := []string{"account_id", "digest", "key_id", "revoked_at", "state"}
		if got := sortedKeys(record); !slices.Equal(got, wantKeys) {
			t.Errorf("credential record keys = %v, want exactly %v; the record is the mirror row's whole state, and an omitted field is a fact the runtime never learns", got, wantKeys)
		}
		for key, want := range map[string]string{
			"key_id":     syntheticKey(t, 0).KeyID,
			"account_id": validAccountID,
			"digest":     validDigest,
			"state":      string(CredentialActive),
		} {
			if got, _ := record[key].(string); got != want {
				t.Errorf("credential record %s = %v, want %q; a misspelled field decodes to a zero value at the consumer rather than failing, which is the quietest way to mirror a wrong row", key, record[key], want)
			}
		}
		if got, present := record["revoked_at"]; !present || got != nil {
			t.Errorf("credential record revoked_at = %v (present: %v), want an explicit null; the fragment requires the field, and null is its spelling for a row nothing was revoked from", got, present)
		}
	})
	t.Run("a revoked credential", func(t *testing.T) {
		revoked, err := NewCredential(syntheticKey(t, 0).KeyID, validAccountID, validDigest, CredentialRevoked, &revocationInstantElsewhere)
		if err != nil {
			t.Fatalf("NewCredential refused its own revoked fixture: %v", err)
		}
		raw, err := json.Marshal(mustSnapshot(t, 7, []Credential{revoked}, nil))
		if err != nil {
			t.Fatalf("marshalling a snapshot with one revoked credential: %v", err)
		}
		record := snapshotRecord(t, raw, "api_keys", 0)
		if got, _ := record["state"].(string); got != string(CredentialRevoked) {
			t.Errorf("credential record state = %v, want %q; the terminal state is what stops the mirror row admitting", record["state"], CredentialRevoked)
		}
		if got, _ := record["revoked_at"].(string); got != "2026-09-25T12:00:00Z" {
			t.Errorf("credential record revoked_at = %v, want the instant rendered in UTC; the revocation was recorded in another zone and must not arrive spelling a different moment than the producer's own stored copy", record["revoked_at"])
		}
	})
}

// TestASnapshotRendersTheAccountRecordTheContractDeclares pins the account
// wire record as the identity and the lifecycle state and nothing else — the
// runtime mirrors whether credentials derived from the account may be
// admitted, and a record that grew a field would mirror an ownership fact the
// Control Plane never projected.
func TestASnapshotRendersTheAccountRecordTheContractDeclares(t *testing.T) {
	raw, err := json.Marshal(mustSnapshot(t, 7, nil, syntheticAccounts(t, 1)))
	if err != nil {
		t.Fatalf("marshalling a snapshot with one account: %v", err)
	}
	record := snapshotRecord(t, raw, "accounts", 0)
	wantKeys := []string{"account_id", "state"}
	if got := sortedKeys(record); !slices.Equal(got, wantKeys) {
		t.Errorf("account record keys = %v, want exactly %v; the projected account is an identity and a lifecycle, and a field beyond them mirrors a fact the runtime was never given", got, wantKeys)
	}
	if got, _ := record["account_id"].(string); got != syntheticAccount(t, 0).AccountID {
		t.Errorf("account record account_id = %v, want the account's id; the id is what the mirror row is keyed on", record["account_id"])
	}
	if got, _ := record["state"].(string); got != string(AccountActive) {
		t.Errorf("account record state = %v, want %q; the state is the only thing the mirror row's admission decision reads", record["state"], AccountActive)
	}
}

// TestASnapshotRendersEmptyArraysAsArraysNotNulls pins the JSON compatibility
// promise the empty snapshot leans on. The constructors accept empty arrays
// because an empty projection is a snapshot too; the rendering must meet them
// halfway, because the schema types both arrays as arrays and a `null` where a
// consumer's decoder expects an array is exactly the kind of difference only a
// production incident finds.
func TestASnapshotRendersEmptyArraysAsArraysNotNulls(t *testing.T) {
	raw, err := json.Marshal(mustSnapshot(t, 0, nil, nil))
	if err != nil {
		t.Fatalf("marshalling an empty snapshot: %v", err)
	}
	envelope := decodedObject(t, raw)
	for name := range map[string]bool{"api_keys": true, "accounts": true} {
		records, ok := envelope[name].([]any)
		if !ok || records == nil || len(records) != 0 {
			t.Errorf("an empty snapshot rendered %v for %q, want an empty JSON array; a nil slice must not become null on the wire", envelope[name], name)
		}
	}
}

// TestAHeadNamesItsTimelineAndItsHighestRevision pins the producer's position
// as the delivery loop reads it: the timeline's name and the highest revision
// ever allocated. Revision zero is accepted on purpose — the fragment gives a
// boundary a minimum of zero and calls zero legitimate, and a timeline
// restored from a backup is a head at zero until its counter allocates again.
func TestAHeadNamesItsTimelineAndItsHighestRevision(t *testing.T) {
	head, err := NewHead(validEpoch, 0)
	if err != nil {
		t.Fatalf("NewHead refused a fresh timeline's head: %v", err)
	}
	if head.Epoch != validEpoch || head.LastRevision != 0 {
		t.Fatalf("head = {%q %d}, want {%q 0}; the epoch names the timeline and the revision is its highest allocated, and a head that lost either is a position nothing joins", head.Epoch, head.LastRevision, validEpoch)
	}
	head, err = NewHead(validEpoch, 1<<40)
	if err != nil {
		t.Fatalf("NewHead refused a well-formed head: %v", err)
	}
	if head.LastRevision != 1<<40 {
		t.Fatalf("head revision = %d, want %d; the head is the log's ceiling, and every delivery is judged against it", head.LastRevision, uint64(1<<40))
	}
}

// snapshotRecord returns one record of a rendered snapshot's array, failing
// the test when the array or its element is not the shape the contract types.
func snapshotRecord(t *testing.T, raw []byte, array string, index int) map[string]any {
	t.Helper()
	records, ok := decodedObject(t, raw)[array].([]any)
	if !ok {
		t.Fatalf("snapshot %s = missing, want an array in %s; the schema types both record arrays as arrays", array, raw)
	}
	if index >= len(records) {
		t.Fatalf("snapshot %s holds %d records, want at least %d; the test built the snapshot with a record in it", array, len(records), index+1)
	}
	record, ok := records[index].(map[string]any)
	if !ok {
		t.Fatalf("snapshot %s[%d] = %v, want an object; the schema types each record as an object", array, index, records[index])
	}
	return record
}
