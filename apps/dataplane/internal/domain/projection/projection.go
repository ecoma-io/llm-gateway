// Package projection is the Data Plane's half of the Control → Data credential
// projection: the grammar a delivered message must satisfy and the rules an
// application must follow. The delivery model is push with replay (ADR 0006
// §5, ADR 0007): the Control Plane is the authority for who may call the
// gateway, this plane holds the mirror the request path verifies against, and
// the five protocol rules that make the two safe are stated in
// api/openapi/shared/projection.yaml — order is a revision, the producer's
// timeline is named, bootstrap is a snapshot, delivery is at-least-once with
// idempotent application, and a consumer's position advances in the same
// transaction as the rows it describes.
//
// This package owns the fail-closed half of that agreement: what a snapshot or
// a batch may contain, and what a batch means against a position. It knows
// nothing about SQL, HTTP or JSON envelopes — the adapters on both sides
// translate to and from it, and both translate its sentinel errors onto their
// own wire vocabulary rather than leaking these words to a caller.
//
// Two of the refusals are decisions about the projection's health rather than
// about one message: a batch that cannot join the applied position
// (ErrRevisionGap) is refused whole, because applying the entries that happen
// to fit would strand the rest behind a position that has moved past them; and
// a message naming a timeline the consumer never earned its position on
// (ErrSnapshotRequired) asks for a snapshot rather than guessing, because a
// Control Plane database restored from an earlier instant is a new timeline
// even though its counter restarted low.
package projection

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The protocol's numbers, declared in api/openapi/shared/projection.yaml and
// pinned to it by the protocol tests on both hops. They live here as literals
// for the same reason every constant on a wire boundary is a literal: the
// definition is the document, and a constant shared across two processes would
// be a second definition wearing a name.
const (
	// ProtocolVersion is the only version of the protocol this plane speaks.
	// A message naming any other version is refused outright — never guessed
	// at — because a version this code does not know may order its fields,
	// states and revisions differently, and applying such a message would
	// corrupt the mirror precisely when someone believes they are upgrading it.
	ProtocolVersion = 1

	// MaxSnapshotRecords is the bound on each array of a snapshot. A snapshot
	// is atomic and cannot chunk, so the bound is a contract decision rather
	// than a page size: past it, bootstrap is impossible by design until the
	// bound is raised — a reviewed change, not a deployment surprise.
	MaxSnapshotRecords = 5000

	// MaxChangesPerBatch is the bound on one incremental delivery. Unlike a
	// snapshot a batch may be split freely by the producer, so this bound
	// trades only delivery granularity, never correctness.
	MaxChangesPerBatch = 200

	// MaxRevision is the highest revision the protocol carries. The wire's
	// revisions are unsigned, but every revision eventually rests in a
	// bigint column on both planes, so a value past the signed 64-bit
	// ceiling could not be stored by a conforming producer in the first
	// place — the grammar refuses it here, at the boundary, instead of
	// letting it fail as a storage error three layers later.
	MaxRevision = uint64(1)<<63 - 1
)

// The five ways a delivered message can be wrong, each with its own answer on
// the wire. The adapters map these onto their own vocabularies — this plane's
// management listener answers 400/409 in its envelope, and the façade
// translates again for its own contract — so the sentences here are for logs,
// and every one of them must be safe to log: they may name revisions and
// identifiers, never credentials.
var (
	// ErrUnsupportedVersion: the message speaks a protocol version this plane
	// does not support. Delivery halts until both ends agree; it never skips.
	ErrUnsupportedVersion = errors.New("projection: protocol version this plane does not support")

	// ErrBatchShape: the message violates the protocol's grammar — a field
	// missing, a state outside the enum, a digest that is not 64 lowercase
	// hex digits, revisions that are not contiguous or past the signed
	// ceiling the store can hold. Refused whole.
	ErrBatchShape = errors.New("projection: message does not satisfy the protocol's grammar")

	// ErrRevisionGap: the batch neither joins nor sits behind the applied
	// position, so some revision between them is missing from what has been
	// delivered. Applying it would jump the position over changes that may
	// still arrive; refusing it keeps the gap visible until a snapshot
	// replaces guesswork with state.
	ErrRevisionGap = errors.New("projection: batch does not join the applied position")

	// ErrSnapshotRequired: the message names a producer timeline the stored
	// position was not earned on, or the consumer has no position yet. No
	// batch can be judged against a position like that; the honest answer is
	// a snapshot.
	ErrSnapshotRequired = errors.New("projection: position does not join this producer timeline; a snapshot is required")

	// ErrTerminalRegression: a batch entry moves a row out of a terminal
	// state — a revoked credential delivered as active, a closed account
	// delivered as suspended or active. Terminal means terminal on both
	// planes: no conforming producer path can emit such an entry, so one
	// that arrives is a producer defect or a corrupted log, and applying it
	// would admit a credential the authority revoked. The batch is refused
	// whole and the loop stays wedged until a human reads the producer's
	// log; a snapshot remains the one delivery allowed to overwrite these
	// rows, because it is the authority's word entire, not a per-entry claim.
	ErrTerminalRegression = errors.New("projection: change moves a terminal state backwards")
)

// ResourceKind names which projection table a change belongs to. The set is
// closed: a resource the runtime does not mirror must not be smuggled in as a
// tolerated unknown field — a new kind is a new protocol version.
type ResourceKind string

const (
	// KindAPIKey is a change to a projected API-key credential.
	KindAPIKey ResourceKind = "api_key"

	// KindAccount is a change to a projected account's lifecycle.
	KindAccount ResourceKind = "account"
)

// CredentialState is the lifecycle of a projected credential. Revoked is
// terminal — there is no un-revoke on this plane or the other.
type CredentialState string

const (
	CredentialActive  CredentialState = "active"
	CredentialRevoked CredentialState = "revoked"
)

// AccountLifecycle is the mirrored lifecycle of an account. Closed is
// terminal; suspended stops admission while the account's history stays.
type AccountLifecycle string

const (
	LifecycleActive    AccountLifecycle = "active"
	LifecycleSuspended AccountLifecycle = "suspended"
	LifecycleClosed    AccountLifecycle = "closed"
)

// APIKeyCredential is the credential half of the two-record model as this
// plane mirrors it: verification material and lifecycle, never the plaintext,
// which exists only inside the Control Plane's mint call (ADR 0006 §8 as
// amended by ADR 0007). KeyID is the row's identity on both sides of the
// split, which is what makes redelivery a no-op and revocation a state change.
type APIKeyCredential struct {
	KeyID     string
	AccountID string
	Digest    string
	State     CredentialState
	RevokedAt *time.Time
}

// AccountState is the account half of the mirror: lifecycle only — whether
// credentials derived from this account may be admitted — with no ownership
// edges and no billing data. It carries no foreign key to a credential row,
// because the two arrive on independent entries in either order.
type AccountState struct {
	AccountID string
	State     AccountLifecycle
}

// NewAPIKeyCredential validates the credential grammar and returns the record,
// or an error wrapping ErrBatchShape. This is the one place the grammar is
// decided, and it is a constructor rather than a validation pass because a
// record that would violate its own database's CHECK constraints must be
// refused before a transaction opens, not by the database after one did.
//
// The digest is the lowercase hex SHA-256 of the key's secret and the only
// form of the secret that exists outside the mint; a value that is anything
// else is refused, never normalised — an upper-case or short digest is a
// producer bug, and applying one would write a credential that can never
// verify.
func NewAPIKeyCredential(keyID, accountID, digest string, state CredentialState, revokedAt *time.Time) (APIKeyCredential, error) {
	record := APIKeyCredential{KeyID: keyID, AccountID: accountID, Digest: digest, State: state, RevokedAt: revokedAt}
	if !validUUID(keyID) {
		return APIKeyCredential{}, shapeError("api_key key_id is not a UUID")
	}
	if !validUUID(accountID) {
		return APIKeyCredential{}, shapeError("api_key account_id is not a UUID")
	}
	if !validDigest(digest) {
		return APIKeyCredential{}, shapeError("api_key digest is not 64 lowercase hex digits")
	}
	switch state {
	case CredentialActive:
		if revokedAt != nil {
			return APIKeyCredential{}, shapeError("api_key carries a revocation instant while active")
		}
	case CredentialRevoked:
		if revokedAt == nil {
			return APIKeyCredential{}, shapeError("api_key is revoked with no revocation instant")
		}
	default:
		return APIKeyCredential{}, shapeError("api_key state is outside the credential lifecycle")
	}
	return record, nil
}

// NewAccountState validates the account grammar and returns the record, or an
// error wrapping ErrBatchShape.
func NewAccountState(accountID string, state AccountLifecycle) (AccountState, error) {
	record := AccountState{AccountID: accountID, State: state}
	if !validUUID(accountID) {
		return AccountState{}, shapeError("account account_id is not a UUID")
	}
	switch state {
	case LifecycleActive, LifecycleSuspended, LifecycleClosed:
	default:
		return AccountState{}, shapeError("account state is outside the account lifecycle")
	}
	return record, nil
}

// Snapshot is a whole projection at a named revision: "be this state at this
// boundary". Applying one consults no per-row history — that is what makes it
// the answer to every position this plane cannot join, from a wiped database
// to a producer timeline that rewound.
type Snapshot struct {
	ProtocolVersion  int
	Epoch            string
	SnapshotRevision uint64
	APIKeys          []APIKeyCredential
	Accounts         []AccountState
}

// ValidateSnapshot judges a snapshot against the protocol's grammar. An empty
// snapshot is a legitimate one: a Control Plane that has projected nothing yet
// bootstraps a consumer with empty arrays and a boundary of zero.
func ValidateSnapshot(snapshot Snapshot) error {
	if snapshot.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("%w: snapshot speaks version %d, this plane speaks %d", ErrUnsupportedVersion, snapshot.ProtocolVersion, ProtocolVersion)
	}
	if !validUUID(snapshot.Epoch) {
		return shapeError("snapshot epoch is not a UUID")
	}
	if snapshot.SnapshotRevision > MaxRevision {
		return shapeError(fmt.Sprintf("snapshot boundary %d is past the ceiling the store can hold", snapshot.SnapshotRevision))
	}
	if len(snapshot.APIKeys) > MaxSnapshotRecords {
		return shapeError(fmt.Sprintf("snapshot carries %d api keys, the contract allows %d", len(snapshot.APIKeys), MaxSnapshotRecords))
	}
	if len(snapshot.Accounts) > MaxSnapshotRecords {
		return shapeError(fmt.Sprintf("snapshot carries %d accounts, the contract allows %d", len(snapshot.Accounts), MaxSnapshotRecords))
	}
	for _, record := range snapshot.APIKeys {
		if _, err := NewAPIKeyCredential(record.KeyID, record.AccountID, record.Digest, record.State, record.RevokedAt); err != nil {
			return err
		}
	}
	for _, record := range snapshot.Accounts {
		if _, err := NewAccountState(record.AccountID, record.State); err != nil {
			return err
		}
	}
	return nil
}

// Change is one entry of the feed: the full state of one resource at one
// revision, never a delta. Full state is what makes replay safe — a redelivered
// entry is applied again and lands in the same place.
type Change struct {
	Revision   uint64
	Kind       ResourceKind
	ResourceID string
	RecordedAt time.Time
	// Exactly one of the two records below is set, and it is the one Kind
	// names. The payload's unknown fields were tolerated and dropped when the
	// change was built; the known fields were parsed and validated then, so an
	// applied change never carries a state this plane would refuse.
	Credential *APIKeyCredential
	Account    *AccountState
}

// NewChange parses one log entry's payload into a Change. The envelope is the
// contract's; the payload's grammar is this package's. Unknown fields inside
// the payload are tolerated and dropped — that is the one direction the
// protocol may evolve without a version bump — while every known field is
// validated fail-closed: a payload missing its digest, or carrying a state
// this plane has never heard of, is a producer speaking a language this mirror
// must not guess at.
func NewChange(revision uint64, kind ResourceKind, resourceID string, recordedAt time.Time, payload json.RawMessage) (Change, error) {
	if revision == 0 {
		return Change{}, shapeError("change carries revision zero; revisions start at one")
	}
	if revision > MaxRevision {
		return Change{}, shapeError(fmt.Sprintf("change carries revision %d, past the ceiling the store can hold", revision))
	}
	if !validUUID(resourceID) {
		return Change{}, shapeError("change resource_id is not a UUID")
	}
	if recordedAt.IsZero() {
		return Change{}, shapeError("change carries no recording instant")
	}
	if len(payload) == 0 {
		return Change{}, shapeError("change carries an empty payload")
	}

	switch kind {
	case KindAPIKey:
		var fields struct {
			AccountID string     `json:"account_id"`
			Digest    string     `json:"digest"`
			State     string     `json:"state"`
			RevokedAt *time.Time `json:"revoked_at"`
		}
		if err := json.Unmarshal(payload, &fields); err != nil {
			return Change{}, shapeError("api_key payload is not a JSON object")
		}
		record, err := NewAPIKeyCredential(resourceID, fields.AccountID, fields.Digest, CredentialState(fields.State), fields.RevokedAt)
		if err != nil {
			return Change{}, err
		}
		return Change{Revision: revision, Kind: kind, ResourceID: resourceID, RecordedAt: recordedAt, Credential: &record}, nil
	case KindAccount:
		var fields struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(payload, &fields); err != nil {
			return Change{}, shapeError("account payload is not a JSON object")
		}
		record, err := NewAccountState(resourceID, AccountLifecycle(fields.State))
		if err != nil {
			return Change{}, err
		}
		return Change{Revision: revision, Kind: kind, ResourceID: resourceID, RecordedAt: recordedAt, Account: &record}, nil
	default:
		return Change{}, shapeError(fmt.Sprintf("change names resource kind %q, which this plane does not mirror", kind))
	}
}

// Batch is one incremental delivery: contiguous revisions, strictly ascending,
// judged whole against the consumer's stored position.
type Batch struct {
	ProtocolVersion int
	Epoch           string
	// FromRevision is the position the producer believed this consumer held
	// when it read the feed. The consumer judges the batch by its own stored
	// position and never by this field; it is carried so a refusal can name
	// what the producer thought it was doing.
	FromRevision uint64
	Changes      []Change
}

// ValidateBatch judges a batch against the protocol's grammar before any
// question of position arises. A batch carries at least one entry — an empty
// one cannot join anything, and a producer that sends one is a producer whose
// page arithmetic has broken — and its revisions are contiguous, which is what
// lets the consumer detect a gap between its position and the batch without
// asking the producer anything.
func ValidateBatch(batch Batch) error {
	if batch.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("%w: batch speaks version %d, this plane speaks %d", ErrUnsupportedVersion, batch.ProtocolVersion, ProtocolVersion)
	}
	if !validUUID(batch.Epoch) {
		return shapeError("batch epoch is not a UUID")
	}
	if len(batch.Changes) == 0 {
		return shapeError("batch carries no changes")
	}
	if len(batch.Changes) > MaxChangesPerBatch {
		return shapeError(fmt.Sprintf("batch carries %d changes, the contract allows %d", len(batch.Changes), MaxChangesPerBatch))
	}
	if batch.Changes[0].Revision == 0 {
		// Refused here rather than trusted to the constructor: a batch is a
		// value type a caller can assemble directly, and the contiguity walk
		// below decrements its first element, which would wrap rather than
		// refuse a zero.
		return shapeError("batch carries revision zero; revisions start at one")
	}
	if batch.FromRevision > MaxRevision {
		return shapeError(fmt.Sprintf("batch names a from-position %d past the ceiling the store can hold", batch.FromRevision))
	}
	previous := batch.Changes[0].Revision
	for _, change := range batch.Changes {
		if change.Revision != previous {
			return shapeError(fmt.Sprintf("batch revisions are not contiguous at %d", change.Revision))
		}
		if change.Revision > MaxRevision {
			return shapeError(fmt.Sprintf("batch carries revision %d, past the ceiling the store can hold", change.Revision))
		}
		previous = change.Revision + 1
		switch change.Kind {
		case KindAPIKey:
			if change.Credential == nil || change.Account != nil {
				return shapeError("api_key change carries no credential record")
			}
			if change.Credential.KeyID != change.ResourceID {
				return shapeError("api_key change's record and resource_id disagree")
			}
		case KindAccount:
			if change.Account == nil || change.Credential != nil {
				return shapeError("account change carries no account record")
			}
			if change.Account.AccountID != change.ResourceID {
				return shapeError("account change's record and resource_id disagree")
			}
		default:
			return shapeError(fmt.Sprintf("change names resource kind %q, which this plane does not mirror", change.Kind))
		}
	}
	return nil
}

// Position is the consumer's own fact: whether a snapshot has been applied,
// the highest revision whose effects are committed, and the producer timeline
// that position was earned on. It is advanced in the same transaction as the
// rows it describes, which is why a crash between the two is a replay rather
// than a loss.
type Position struct {
	Epoch           string
	Bootstrapped    bool
	AppliedRevision uint64
}

// Decision is what an incremental batch means against an applied position.
// Deciding before applying is what makes every delivery one of three exact
// cases instead of a judgement call per entry.
type Decision int

const (
	// DecisionApply: the batch joins the position exactly — its first revision
	// is the position's successor — so the whole batch is applied with
	// per-row guards and the position advances to its last revision.
	DecisionApply Decision = iota
	// DecisionDuplicate: the batch sits entirely behind the position. This is
	// the lost-acknowledgement case; the entries are already applied, so the
	// correct answer is the current position, re-acknowledged.
	DecisionDuplicate
	// DecisionGap: the batch straddles or overshoots the position, so some
	// revision between them was never delivered. Refused whole, never
	// partially applied and never skipped.
	DecisionGap
)

// DecideBatch classifies a batch against the position the caller has already
// read. It expects an unbootstrapped position to have been answered with
// ErrSnapshotRequired before this is reached — with no position there is
// nothing to join — and it expects a grammar-valid batch, whose revisions are
// contiguous and non-empty, which ValidateBatch guarantees.
func DecideBatch(applied uint64, batch Batch) Decision {
	last := batch.Changes[len(batch.Changes)-1].Revision
	if last <= applied {
		return DecisionDuplicate
	}
	if batch.Changes[0].Revision == applied+1 {
		return DecisionApply
	}
	return DecisionGap
}

// CredentialRegressesTerminal reports whether a delivered credential state
// would move a row that already stands in a terminal state out of it. Revoked
// is terminal by the grammar both planes agree on; a delivered active for a
// row the mirror holds revoked is not a lifecycle the producer can express,
// so the caller refuses the batch rather than admitting a credential the
// authority took back.
func CredentialRegressesTerminal(applied, delivered CredentialState) bool {
	return applied == CredentialRevoked && delivered != CredentialRevoked
}

// AccountRegressesTerminal is the account half of the same judgement: closed
// is terminal, and a delivered suspended or active for a closed row is a
// producer defect the mirror refuses.
func AccountRegressesTerminal(applied, delivered AccountLifecycle) bool {
	return applied == LifecycleClosed && delivered != LifecycleClosed
}

// shapeError returns a grammar failure carrying detail for the log while
// wrapping the sentinel the wire will answer with.
func shapeError(detail string) error {
	return fmt.Errorf("%w: %s", ErrBatchShape, detail)
}

// validUUID reports whether s is a canonical RFC UUID: 36 characters,
// hyphens where the form puts them, hex everywhere else. The version nibble
// is deliberately not pinned — the identifiers crossing this protocol are the
// producer's business, minted on whichever version that side chose, and this
// plane treats them as opaque identity rather than as structure it owns.
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHex(r) {
				return false
			}
		}
	}
	return true
}

// validDigest reports whether s is a SHA-256 digest in its strongest form:
// exactly 64 lowercase hex digits. This is the same grammar the mirror's
// database enforces with a CHECK constraint and the contract declares with a
// pattern; a digest is verification material, and a weaker spelling of one is
// refused rather than normalised.
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !isHex(r) {
			return false
		}
	}
	return true
}

// isHex reports whether r is an ASCII lowercase hex digit. Upper case is
// refused by the callers above: a digest that arrives capitalised is a
// producer that did not use its own encoder, not a value to fold.
func isHex(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f'
}
