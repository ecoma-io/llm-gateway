// Package projection is the Control Plane's side of the Control → Data
// credential projection's grammar (ADR 0007): the values one recorded change,
// one snapshot cut and one delivery batch are built from, and the
// constructors that refuse anything outside the shapes
// api/openapi/shared/projection.yaml declares.
//
// It exists because the delivery pipeline has two ends whose agreement cannot
// be typed across: the Data Plane judges every message against the fragment's
// schemas, and this plane produces every message from its own database. The
// fragment is the contract and the constructor is the enforcement — a value
// that could not be delivered is a value that cannot be built, so a refusal
// happens at mint or at read, next to the code that produced the bad value,
// and never as a 400 discovered by the consumer on the far side of a hop.
//
// The package speaks the projection's vocabulary, not the ownership domain's.
// The states here are the contract's enums, which coincide with
// identity.APIKeyState and identity.AccountState today and are allowed to
// diverge the day the contract evolves: a new projected state is a new
// protocol version and a reviewed migration of every hop, while an ownership
// state is an internal affair of the aggregate that owns it. For the same
// reason nothing here imports identity — the projection grammar carries the
// digest, which is verification material, and the ownership record is the
// thing ADR 0006 §8 keeps digest-free. The two grammars meet only in the
// use case that writes both.
//
// Secrets: the plaintext of an API key never enters this package. What the
// grammar carries is Digest — the lowercase hex SHA-256 of the secret, the
// one form of it that may exist outside the mint call (ADR 0006 §8, as
// amended by ADR 0007).
package projection

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// ProtocolVersion is the version of the projection protocol this module
	// speaks, and the value every message this package assembles carries in
	// `protocol_version`. A hop that receives a version it does not support
	// refuses the message outright rather than guessing (ADR 0007): the
	// failure is loud, the position and the rows are untouched, and delivery
	// resumes when both ends speak the same version. Within the version,
	// unknown additive fields are tolerated by both ends — but nothing
	// ordering- or state-bearing may arrive that way.
	ProtocolVersion = 1

	// MaxChangesPerBatch is the largest number of entries one delivery batch
	// may carry: the contract's `changes.maxItems` in projection.yaml. The
	// producer batches at this bound and the consumer refuses past it, so a
	// build whose two copies disagree delivers batches one side always
	// refuses — which is why the number is pinned to the document in this
	// package's tests rather than trusted to stay in step by eye.
	MaxChangesPerBatch = 200

	// MaxSnapshotKeys is the largest credential array a snapshot may carry,
	// and MaxSnapshotAccounts the account array's bound. A snapshot is atomic
	// and cannot chunk — bootstrap is impossible by design past these bounds
	// until they are raised, which is a reviewed contract change and not a
	// deployment surprise.
	MaxSnapshotKeys     = 5000
	MaxSnapshotAccounts = 5000

	// digestHexLength is the fixed text length of a SHA-256 digest in
	// lowercase hex: 32 bytes, two characters each.
	digestHexLength = 64
)

// Digest is the lowercase hex rendering of an API key secret's SHA-256 — the
// only form of the secret that exists outside the mint call, and the only
// thing the projection delivers (ADR 0006 §8, ADR 0007). It is verification
// material: the runtime confirms a presented credential by digesting it and
// comparing, and can never reconstruct the secret from it. Knowing a digest
// is not holding the key, which is exactly why a digest may transit this
// pipeline while the plaintext may not.
//
// The zero value is not a digest: every Digest is built through NewDigest,
// which refuses anything that is not exactly 64 lowercase hex characters —
// the strongest form the contract pins, and the form the database's own
// CHECKs pin on both stored copies.
type Digest string

// NewDigest validates the hex form and returns it as a Digest. Anything but
// 64 lowercase hex characters is refused: an upper-case, short, long or
// non-hex rendering is not a spelling of a digest this protocol carries, and
// a value that reached a payload would be refused by the consumer and by the
// database — loudly, and later than here.
func NewDigest(hex string) (Digest, error) {
	if len(hex) != digestHexLength {
		return "", fmt.Errorf("projection: digest must be %d lowercase hex characters, got %d", digestHexLength, len(hex))
	}
	for _, r := range hex {
		if !isLowerHex(r) {
			return "", fmt.Errorf("projection: digest must be lowercase hex: %q is neither", string(r))
		}
	}
	return Digest(hex), nil
}

// CredentialState is a projected credential's lifecycle state: the
// contract's enum, spelled exactly as the wire spells it. `revoked` is
// terminal — there is no un-revoke, and a key that revokes is a key whose
// mirror row must stop admitting, however the revocation reaches the runtime.
type CredentialState string

const (
	// CredentialActive: the credential may authenticate.
	CredentialActive CredentialState = "active"
	// CredentialRevoked: terminal. The mirror row must never authenticate
	// again.
	CredentialRevoked CredentialState = "revoked"
)

// AccountState is a projected account's lifecycle state: the contract's enum.
// `closed` is terminal; `suspended` stops admission while keeping the
// account's history intact.
type AccountState string

const (
	// AccountActive: credentials derived from the account may be admitted.
	AccountActive AccountState = "active"
	// AccountSuspended: admission stops, the account's history does not.
	AccountSuspended AccountState = "suspended"
	// AccountClosed: terminal.
	AccountClosed AccountState = "closed"
)

// ResourceKind names which projection table an entry belongs to. The set is
// closed: a resource the runtime does not mirror must not be smuggled in as a
// tolerated unknown — a new kind is a new protocol version.
type ResourceKind string

const (
	// KindAPIKey marks the credential half of the mirror.
	KindAPIKey ResourceKind = "api_key"
	// KindAccount marks the account-state half.
	KindAccount ResourceKind = "account"
)

// Credential is one projected API key's full state — the self-contained row
// the mirror holds, never a delta. KeyID, AccountID and Digest travel as the
// contract spells them; State and RevokedAt are the pairing both the domain
// and the database enforce: revoked exactly when an instant is recorded.
type Credential struct {
	KeyID     string
	AccountID string
	Digest    Digest
	State     CredentialState
	RevokedAt *time.Time
}

// NewCredential validates one projected credential's whole grammar: both ids
// in UUID form, the digest in its 64-hex form, the state in the contract's
// enum, and the revocation pairing — `revoked` with no instant is a
// revocation nobody can audit, and an instant on an `active` row is a
// revocation that never happened.
func NewCredential(keyID, accountID string, digest string, state CredentialState, revokedAt *time.Time) (Credential, error) {
	if err := validateUUIDForm(keyID); err != nil {
		return Credential{}, fmt.Errorf("projection: credential: %w", err)
	}
	if err := validateUUIDForm(accountID); err != nil {
		return Credential{}, fmt.Errorf("projection: credential %s: account id: %w", keyID, err)
	}
	checked, err := NewDigest(digest)
	if err != nil {
		return Credential{}, fmt.Errorf("projection: credential %s: %w", keyID, err)
	}
	switch state {
	case CredentialActive:
		if revokedAt != nil {
			return Credential{}, fmt.Errorf("projection: credential %s: state %q must not carry a revocation instant", keyID, state)
		}
	case CredentialRevoked:
		if revokedAt == nil {
			return Credential{}, fmt.Errorf("projection: credential %s: state %q must carry the instant it was revoked", keyID, state)
		}
	default:
		return Credential{}, fmt.Errorf("projection: credential %s: unknown state %q", keyID, state)
	}
	credential := Credential{KeyID: keyID, AccountID: accountID, Digest: checked, State: state}
	if revokedAt != nil {
		t := revokedAt.UTC()
		credential.RevokedAt = &t
	}
	return credential, nil
}

// credentialPayload is the wire shape of an api_key entry's payload: the
// full state of the row at a revision, with nothing ordering-bearing in it.
// Unknown fields inside a payload are tolerated and ignored by the consumer,
// which is the one direction this protocol may evolve without a version bump.
type credentialPayload struct {
	AccountID string          `json:"account_id"`
	Digest    Digest          `json:"digest"`
	State     CredentialState `json:"state"`
	RevokedAt *time.Time      `json:"revoked_at"`
}

// PayloadJSON renders the credential as the payload an api_key log entry
// carries — the same bytes the database stores in projection_changes.payload
// and the wire carries inside the change. One rendering, two destinations:
// the stored copy and the delivered copy of a revision are the same payload
// by construction, so replay cannot disagree with history.
func (c Credential) PayloadJSON() (json.RawMessage, error) {
	raw, err := json.Marshal(credentialPayload{
		AccountID: c.AccountID,
		Digest:    c.Digest,
		State:     c.State,
		RevokedAt: c.RevokedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("projection: credential %s payload: %w", c.KeyID, err)
	}
	return raw, nil
}

// Account is one projected account's full state: lifecycle only, no
// ownership edges and no billing data — the runtime mirrors whether
// credentials derived from this account may be admitted, and nothing else.
type Account struct {
	AccountID string
	State     AccountState
}

// NewAccount validates the projected account grammar: the id in UUID form
// and the state in the contract's enum.
func NewAccount(accountID string, state AccountState) (Account, error) {
	if err := validateUUIDForm(accountID); err != nil {
		return Account{}, fmt.Errorf("projection: account: %w", err)
	}
	switch state {
	case AccountActive, AccountSuspended, AccountClosed:
	default:
		return Account{}, fmt.Errorf("projection: account %s: unknown state %q", accountID, state)
	}
	return Account{AccountID: accountID, State: state}, nil
}

// accountPayload is the wire shape of an account entry's payload.
type accountPayload struct {
	State AccountState `json:"state"`
}

// PayloadJSON renders the account as the payload an account log entry
// carries — the twin of Credential.PayloadJSON for the account half.
func (a Account) PayloadJSON() (json.RawMessage, error) {
	raw, err := json.Marshal(accountPayload{State: a.State})
	if err != nil {
		return nil, fmt.Errorf("projection: account %s payload: %w", a.AccountID, err)
	}
	return raw, nil
}

// Head is the producer log's current position: the timeline's identity and
// the highest revision ever allocated on it. The delivery loop reads one
// Head per cycle and judges every consumer answer against it.
type Head struct {
	// Epoch names the producer timeline (ADR 0007 §6). A database
	// restored to an earlier instant rewinds the counter; without the epoch
	// the consumer's position would silently name revisions that will never
	// be re-issued. The epoch is assigned once by the projection
	// foundation's migration and never changes within a timeline — a
	// re-mint is a new timeline, and it is the restore procedure's step,
	// because a restore puts the old epoch back with the old rows.
	Epoch string
	// LastRevision is the highest allocated revision: the log's head.
	LastRevision uint64
}

// NewHead validates a read of the counter: the epoch must be a UUID — the
// migration generates one, so a malformed epoch is a corrupted row, and a
// producer that delivered its timeline's name in any other shape would teach
// the consumer a position nothing later joins.
func NewHead(epoch string, lastRevision uint64) (Head, error) {
	if err := validateUUIDForm(epoch); err != nil {
		return Head{}, fmt.Errorf("projection: head: epoch: %w", err)
	}
	return Head{Epoch: epoch, LastRevision: lastRevision}, nil
}

// Change is one entry of the durable log: a self-contained full-state row
// for one resource at one revision, never a delta — replay is the delivery
// model's answer to every crash and every lost acknowledgement, and replay
// needs the entry to describe the row's whole state.
//
// The fields derive from the projected value: Kind and ResourceID follow
// from which constructor built the change, so a change cannot disagree with
// the row it carries. RecordedAt is carried for operators and orders nothing
// (ADR 0007 §3) — revisions are the only ordering key.
type Change struct {
	Revision   uint64
	Kind       ResourceKind
	ResourceID string
	RecordedAt time.Time

	credential Credential // meaningful exactly when Kind == KindAPIKey
	account    Account    // meaningful exactly when Kind == KindAccount
}

// NewCredentialChange records an api_key change at a revision. Revision zero
// is refused: the counter allocates from one, and a zero in a delivered
// batch would claim a revision the log never issued.
func NewCredentialChange(revision uint64, at time.Time, credential Credential) (Change, error) {
	if revision == 0 {
		return Change{}, fmt.Errorf("projection: change for %s: revision must be at least 1", credential.KeyID)
	}
	return Change{
		Revision:   revision,
		Kind:       KindAPIKey,
		ResourceID: credential.KeyID,
		RecordedAt: at.UTC(),
		credential: credential,
	}, nil
}

// NewAccountChange records an account change at a revision, with the same
// zero-revision refusal as its credential twin.
func NewAccountChange(revision uint64, at time.Time, account Account) (Change, error) {
	if revision == 0 {
		return Change{}, fmt.Errorf("projection: change for %s: revision must be at least 1", account.AccountID)
	}
	return Change{
		Revision:   revision,
		Kind:       KindAccount,
		ResourceID: account.AccountID,
		RecordedAt: at.UTC(),
		account:    account,
	}, nil
}

// Credential returns the projected credential the change carries. It is
// meaningful exactly when Kind is KindAPIKey; a caller asking on the wrong
// kind gets a zero value, which the caller's own kind switch makes
// unreachable — the same shape the encoding/json union decode uses.
func (c Change) Credential() Credential { return c.credential }

// Account returns the projected account the change carries, with the same
// kind-scoped meaning as Credential.
func (c Change) Account() Account { return c.account }

// payloadJSON renders the change's projected value as its payload, by kind.
func (c Change) payloadJSON() (json.RawMessage, error) {
	switch c.Kind {
	case KindAPIKey:
		return c.credential.PayloadJSON()
	case KindAccount:
		return c.account.PayloadJSON()
	default:
		return nil, fmt.Errorf("projection: change %d: unknown resource kind %q", c.Revision, c.Kind)
	}
}

// MarshalJSON renders the change as the contract's ProjectionChange: the
// envelope around the payload, with the revision and the resource identity
// the consumer's guards act on. It exists as a method rather than an
// exported wire struct so the envelope's shape is stated once, here, beside
// the constructors that guarantee it.
func (c Change) MarshalJSON() ([]byte, error) {
	payload, err := c.payloadJSON()
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Revision   uint64          `json:"revision"`
		Kind       ResourceKind    `json:"resource_kind"`
		ResourceID string          `json:"resource_id"`
		RecordedAt time.Time       `json:"recorded_at"`
		Payload    json.RawMessage `json:"payload"`
	}{
		Revision:   c.Revision,
		Kind:       c.Kind,
		ResourceID: c.ResourceID,
		RecordedAt: c.RecordedAt,
		Payload:    payload,
	})
}

// validateUUIDForm checks the canonical lowercase 8-4-4-4-12 hex shape,
// including RFC 4122's version nibble (4) and variant bits. It is this
// package's own statement of the grammar the contract's `format: uuid`
// fields carry — deliberately not imported from the identity domain, whose
// ids merely coincide with it today, for the same reason the states are
// restated: the projection grammar is the contract's, and it evolves when
// the contract does.
func validateUUIDForm(s string) error {
	if len(s) != 36 {
		return errors.New("id must be 36 characters")
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return errors.New("id dashes are misplaced")
			}
		case 14:
			if r != '4' {
				return errors.New("id is not a version 4 uuid")
			}
		case 19:
			if r != '8' && r != '9' && r != 'a' && r != 'b' {
				return errors.New("id is not an rfc 4122 variant")
			}
		default:
			if !isLowerHex(r) {
				return errors.New("id is not lowercase hex")
			}
		}
	}
	return nil
}

func isLowerHex(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
}
