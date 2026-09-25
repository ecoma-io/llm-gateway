package projection

import (
	"encoding/json"
	"fmt"
	"time"
)

// The two message shapes the delivery loop sends across the seam: a snapshot
// (bootstrap, and every answer to a position that no longer joins the log)
// and a batch (the ordinary incremental case). Both are built here rather
// than at the wire so a message that could not be delivered is a message the
// constructors refuse — the bounds, the contiguity and the timeline's name
// are checked where the values are assembled, not re-judged by the consumer
// three hops later.

// Batch is one incremental delivery: contiguous revisions of the log,
// strictly ascending, starting at the revision strictly after FromRevision —
// the position the producer believed the consumer held when it read the feed
// (ADR 0007 §5). The consumer judges the batch by its own stored
// position, not by FromRevision; the field is here so a refusal can name
// what the producer thought it was doing.
type Batch struct {
	Epoch        string
	FromRevision uint64
	Changes      []Change
}

// NewBatch assembles a batch and enforces, before any bytes exist, the three
// properties the contract's `changes` array declares: at least one entry —
// an empty batch is not a delivery, it is the decision not to send one, and
// the loop that has nothing to send simply does not — at most
// MaxChangesPerBatch of them, and contiguous revisions ascending from
// FromRevision. Contiguity is the producer's half of the three-case rule:
// the consumer detects a batch that does not join its position before
// applying any of it, and that detection is only meaningful for batches the
// producer built to join.
func NewBatch(epoch string, from uint64, changes []Change) (Batch, error) {
	if err := validateEpoch(epoch); err != nil {
		return Batch{}, fmt.Errorf("projection: batch: %w", err)
	}
	if len(changes) == 0 {
		return Batch{}, fmt.Errorf("projection: batch: a batch with no changes is not a delivery")
	}
	if len(changes) > MaxChangesPerBatch {
		return Batch{}, fmt.Errorf("projection: batch: %d changes exceeds the contract's bound of %d", len(changes), MaxChangesPerBatch)
	}
	for i, change := range changes {
		if want := from + uint64(i) + 1; change.Revision != want {
			return Batch{}, fmt.Errorf("projection: batch: entry %d carries revision %d, want the contiguous %d", i, change.Revision, want)
		}
	}
	return Batch{Epoch: epoch, FromRevision: from, Changes: changes}, nil
}

// LastRevision is the highest revision the batch carries: the position the
// consumer holds once it has applied and committed the whole batch.
func (b Batch) LastRevision() uint64 {
	return b.FromRevision + uint64(len(b.Changes))
}

// MarshalJSON renders the batch as the contract's ProjectionChangesBatch.
// The changes slice is rendered as an array even when empty — the
// constructors refuse an empty batch, but the rendering does not depend on
// that refusal to be a legal message.
func (b Batch) MarshalJSON() ([]byte, error) {
	changes := b.Changes
	if changes == nil {
		changes = []Change{}
	}
	return json.Marshal(struct {
		ProtocolVersion int      `json:"protocol_version"`
		Epoch           string   `json:"epoch"`
		FromRevision    uint64   `json:"from_revision"`
		Changes         []Change `json:"changes"`
	}{ProtocolVersion: ProtocolVersion, Epoch: b.Epoch, FromRevision: b.FromRevision, Changes: changes})
}

// Snapshot is one bootstrap delivery: the producer's whole projection at a
// named boundary (ADR 0007 §4). Applying it is unconditional — a
// snapshot means "be this state at this boundary", so it consults no
// per-row history — and it sets the consumer's position to the boundary in
// the same transaction as the rows it carries.
type Snapshot struct {
	Epoch    string
	Revision uint64
	Keys     []Credential
	Accounts []Account
}

// NewSnapshot assembles a snapshot and holds it to the contract's array
// bounds. The arrays may be empty — an empty projection is a snapshot too,
// and zero is a legitimate boundary — but they may not exceed their bounds:
// past them bootstrap is impossible by design until the bound is raised,
// which is a reviewed contract change and not a deployment surprise.
func NewSnapshot(epoch string, revision uint64, keys []Credential, accounts []Account) (Snapshot, error) {
	if err := validateEpoch(epoch); err != nil {
		return Snapshot{}, fmt.Errorf("projection: snapshot: %w", err)
	}
	if len(keys) > MaxSnapshotKeys {
		return Snapshot{}, fmt.Errorf("projection: snapshot: %d credentials exceeds the contract's bound of %d", len(keys), MaxSnapshotKeys)
	}
	if len(accounts) > MaxSnapshotAccounts {
		return Snapshot{}, fmt.Errorf("projection: snapshot: %d accounts exceeds the contract's bound of %d", len(accounts), MaxSnapshotAccounts)
	}
	return Snapshot{Epoch: epoch, Revision: revision, Keys: keys, Accounts: accounts}, nil
}

// credentialRecord is one ApiKeyCredential on the wire: the whole projected
// row, with its key id — the payload plus the identity the payload omits
// because the change envelope carries it.
type credentialRecord struct {
	KeyID     string          `json:"key_id"`
	AccountID string          `json:"account_id"`
	Digest    Digest          `json:"digest"`
	State     CredentialState `json:"state"`
	RevokedAt *time.Time      `json:"revoked_at"`
}

// accountRecord is one AccountState on the wire.
type accountRecord struct {
	AccountID string       `json:"account_id"`
	State     AccountState `json:"state"`
}

// MarshalJSON renders the snapshot as the contract's ProjectionSnapshot,
// with protocol_version pinned by this package rather than by any caller's
// argument.
func (s Snapshot) MarshalJSON() ([]byte, error) {
	keys := make([]credentialRecord, 0, len(s.Keys))
	for _, key := range s.Keys {
		keys = append(keys, credentialRecord(key))
	}
	accounts := make([]accountRecord, 0, len(s.Accounts))
	for _, account := range s.Accounts {
		accounts = append(accounts, accountRecord(account))
	}
	return json.Marshal(struct {
		ProtocolVersion  int                `json:"protocol_version"`
		Epoch            string             `json:"epoch"`
		SnapshotRevision uint64             `json:"snapshot_revision"`
		APIKeys          []credentialRecord `json:"api_keys"`
		Accounts         []accountRecord    `json:"accounts"`
	}{
		ProtocolVersion:  ProtocolVersion,
		Epoch:            s.Epoch,
		SnapshotRevision: s.Revision,
		APIKeys:          keys,
		Accounts:         accounts,
	})
}

// validateEpoch refuses a message without its timeline's name. The epoch is
// how the pair notices that a position no longer joins the log (ADR 0007
// rule 2); a producer that delivered an unnamed timeline would be asking the
// consumer to trust a position that no recovery story can reason about.
func validateEpoch(epoch string) error {
	if err := validateUUIDForm(epoch); err != nil {
		return fmt.Errorf("epoch: %w", err)
	}
	return nil
}
