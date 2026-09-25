package accounting

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"
)

// The accounting identifiers are distinct types, not aliases of string, for
// the same reason commerce's are: a settlement id can never be passed where
// a bucket id is expected by a signature accident. Every id this domain
// MINTS is an RFC 9562 version-7 uuid — time-ordered, because settlements
// and legs are history and a sort by id is a rough sort by mint time — and
// every id another context mints is carried here as data: validated for the
// form its owner has published, never minted, and never assumed to exist
// where no foreign key can reach (ADR 0006 §7).
type (
	// FundingBucketID identifies one funding bucket — the authoritative
	// capacity projection for one entitlement cycle or one account PAYG
	// balance. Minted here: the bucket is this domain's aggregate.
	FundingBucketID string
	// SettlementID identifies one settlement of record. Minted here.
	SettlementID string
	// LedgerEntryID identifies one ledger leg. Minted here.
	LedgerEntryID string
	// EntitlementID identifies the commerce entitlement a cycle bucket
	// funds. Commerce mints it; accounting carries the reference blind —
	// validated for the v7 form the schema pins, never assumed to exist
	// until the foreign key says so.
	EntitlementID string
	// AccountID identifies the identity account a PAYG bucket funds.
	// Identity mints it (currently as a version-4 uuid, its recorded
	// deviation), so this reference carries no grammar beyond non-emptiness
	// in the domain and the uuid type's own form check in the database.
	AccountID string
	// ReservationID identifies one Data Plane reservation — the hold's
	// subject. The runtime owns this identifier's grammar and has not
	// published it, so the domain asserts only that it is a canonical
	// uuid; the same assertion the schema makes by typing the column.
	ReservationID string
	// RequestID identifies one Data Plane request — the settlement's
	// exactly-once key. Opaque text on purpose: it crosses the plane
	// boundary as data, and this plane asserts presence and length, not
	// someone else's grammar.
	RequestID string
	// CommandKey is a caller's idempotency key for one bucket: required on
	// topups, optional on adjustments, unique per bucket where present.
	CommandKey string
	// PriceRevisionID names the price revision a consume was priced
	// against, copied by value onto the leg. The B12 catalog owns
	// revisions; this is a textual reference, not a foreign key.
	PriceRevisionID string
	// OperatorID names who authorised an adjustment. Text, not a users-row
	// reference: the operator surface is not built yet, and the ledger
	// must not guess at its grammar.
	OperatorID string
)

// NewFundingBucketID mints a time-ordered funding-bucket identifier.
func NewFundingBucketID() (FundingBucketID, error) {
	id, err := newUUIDv7()
	if err != nil {
		return "", fmt.Errorf("accounting: new funding bucket id: %w", err)
	}
	return FundingBucketID(id), nil
}

// NewSettlementID mints a time-ordered settlement identifier.
func NewSettlementID() (SettlementID, error) {
	id, err := newUUIDv7()
	if err != nil {
		return "", fmt.Errorf("accounting: new settlement id: %w", err)
	}
	return SettlementID(id), nil
}

// NewLedgerEntryID mints a time-ordered ledger-entry identifier.
func NewLedgerEntryID() (LedgerEntryID, error) {
	id, err := newUUIDv7()
	if err != nil {
		return "", fmt.Errorf("accounting: new ledger entry id: %w", err)
	}
	return LedgerEntryID(id), nil
}

// uuidV7Form is the canonical lowercase RFC 9562 version-7 shape: the third
// group's first nibble is the version (7), the fourth group's first nibble
// carries the RFC 4122 variant (89ab). It is the same grammar the accounting
// schema CHECKs on every accounting-minted id and on the entitlement
// reference, so the database refuses what this validation would, and this
// validation refuses first, in the domain's own words.
var uuidV7Form = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// uuidForm is the canonical lowercase uuid shape at any version — the whole
// grammar this domain asserts about a Data Plane reservation id, whose mint
// is the runtime's business and whose published form does not exist yet.
var uuidForm = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// The text grammars' shared ceiling. It matches the columns' CHECKs
// (settlements_request_id_grammar and the command-key, price-revision,
// adjustment-reason and operator columns), so a refusal here is never a
// surprise at the driver a statement later.
const (
	maxRequestIDLength       = 256
	maxCommandKeyLength      = 256
	maxPriceRevisionLength   = 256
	maxOperatorIDLength      = 256
	maxAdjustmentReasonValue = 1024
)

// validateMintedID pins the v7 form of an id this domain minted. A minter
// cannot produce anything else, so this is a tripwire on corruption rather
// than a gate on callers — the same judgment the schema's v7 CHECKs make.
func validateMintedID(id FundingBucketID) error {
	if !uuidV7Form.MatchString(string(id)) {
		return fmt.Errorf("accounting: %w: %q is not a version-7 uuid", ErrInvalidReference, id)
	}
	return nil
}

// validateMintedEntryID pins the v7 form of a ledger leg this domain minted
// — the same tripwire bucket ids get, at the leg's own type.
func validateMintedEntryID(id LedgerEntryID) error {
	if !uuidV7Form.MatchString(string(id)) {
		return fmt.Errorf("accounting: %w: %q is not a version-7 uuid", ErrInvalidReference, id)
	}
	return nil
}

// validateMintedSettlementID pins the v7 form of a settlement header this
// domain minted.
func validateMintedSettlementID(id SettlementID) error {
	if !uuidV7Form.MatchString(string(id)) {
		return fmt.Errorf("accounting: %w: %q is not a version-7 uuid", ErrInvalidReference, id)
	}
	return nil
}

// validateEntitlementID pins the v7 form of the commerce reference a cycle
// bucket funds.
func validateEntitlementID(id EntitlementID) error {
	if !uuidV7Form.MatchString(string(id)) {
		return fmt.Errorf("accounting: %w: %q is not a version-7 uuid", ErrInvalidReference, id)
	}
	return nil
}

// validateReservationID pins the uuid form of the runtime reference a hold
// or release names.
func validateReservationID(id ReservationID) error {
	if !uuidForm.MatchString(string(id)) {
		return fmt.Errorf("accounting: %w: %q is not a canonical uuid", ErrInvalidReference, id)
	}
	return nil
}

// validateRequestID pins the settlement key's presence and length — the one
// grammar an opaque cross-plane identifier is owed.
func validateRequestID(id RequestID) error {
	if len(id) == 0 || len(id) > maxRequestIDLength {
		return fmt.Errorf("accounting: %w: request id must be 1..%d characters", ErrInvalidReference, maxRequestIDLength)
	}
	return nil
}

// validateCommandKey pins the idempotency key's presence and length.
func validateCommandKey(key CommandKey) error {
	if len(key) == 0 || len(key) > maxCommandKeyLength {
		return fmt.Errorf("accounting: %w: command key %q must be 1..%d characters", ErrInvalidReference, key, maxCommandKeyLength)
	}
	return nil
}

// validatePriceRevisionID pins the price reference's presence and length.
func validatePriceRevisionID(id PriceRevisionID) error {
	if len(id) == 0 || len(id) > maxPriceRevisionLength {
		return fmt.Errorf("accounting: %w: price revision id %q must be 1..%d characters", ErrInvalidReference, id, maxPriceRevisionLength)
	}
	return nil
}

// validateOperatorID pins the authoriser's presence and length.
func validateOperatorID(id OperatorID) error {
	if len(id) == 0 || len(id) > maxOperatorIDLength {
		return fmt.Errorf("accounting: %w: operator id %q must be 1..%d characters", ErrInvalidReference, id, maxOperatorIDLength)
	}
	return nil
}

// validateAdjustmentReason pins the stated reason's presence and length.
func validateAdjustmentReason(reason string) error {
	if len(reason) == 0 || len(reason) > maxAdjustmentReasonValue {
		return fmt.Errorf("accounting: %w: adjustment reason %q must be 1..%d characters", ErrInvalidReference, reason, maxAdjustmentReasonValue)
	}
	return nil
}

// newUUIDv7 returns an RFC 9562 version-7 UUID in lowercase canonical form:
// forty-eight bits of Unix millisecond timestamp for ordering, then
// random version, variant and padding bits for uniqueness. The standard
// library is enough — sixteen bytes, two fields fixed, hex-encoded — the
// same judgment identity's v4 and commerce's v7 minters recorded. Nothing
// here depends on monotonicity within a millisecond: a bucket's true order
// is its ledger sequence, never its ids.
func newUUIDv7() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("accounting: read random bytes: %w", err)
	}
	// The Unix epoch in milliseconds fits the forty-eight bits this layout
	// gives it until far past any year a gateway will meet, so the widening
	// conversion loses nothing.
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	h := make([]byte, 36)
	hex.Encode(h[0:8], b[0:4])
	h[8] = '-'
	hex.Encode(h[9:13], b[4:6])
	h[13] = '-'
	hex.Encode(h[14:18], b[6:8])
	h[18] = '-'
	hex.Encode(h[19:23], b[8:10])
	h[23] = '-'
	hex.Encode(h[24:36], b[10:16])
	return string(h), nil
}
