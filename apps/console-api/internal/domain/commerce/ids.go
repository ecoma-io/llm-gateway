package commerce

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"
)

// The commerce identifiers are distinct types, not aliases of string, so a
// subscription can never be passed where an entitlement is expected and a
// grant definition can never be filed under the wrong version by a signature
// accident. Every id this domain mints is an RFC 9562 version-7 UUID:
// time-ordered, because subscriptions and entitlements cross the plane
// boundary — they travel in the grant publications and the usage facts, and
// persistence.md pins v7 for exactly that — while the identifiers of the
// aggregates this context only references (catalog's alias-group versions,
// Accounting's funding buckets) arrive from outside and are validated, never
// minted, here.

type (
	// AccountID identifies the identity context's ownership root — the
	// account a subscription belongs to and a PAYG flag is keyed by. It is
	// minted by the identity domain (as identity.AccountID, currently a
	// version-4 uuid; that deviation is that context's), and commerce
	// carries its own distinct type for it, because a bounded context
	// neither imports another's package nor accepts a bare string where an
	// owner belongs; the application layer converts between the two
	// spellings, and the persistence adapter stores the rendering.
	AccountID string
	// PlanID identifies one commercial product's identity root.
	PlanID string
	// PlanVersionID identifies one immutable version of a plan — the version
	// a subscription pins forever.
	PlanVersionID string
	// GrantDefinitionID identifies one grant of one plan version; it is the
	// stable id the entitlement uniqueness triple names.
	GrantDefinitionID string
	// SubscriptionID identifies one account's instantiation of one plan
	// version.
	SubscriptionID string
	// EntitlementID identifies one live grant and is the waterfall's final,
	// total tie-break.
	EntitlementID string
	// AliasGroupVersionID identifies one immutable catalog membership
	// snapshot. It is minted by the Data Plane's catalog (B3) and arrives
	// here as data: no foreign key can cross the plane boundary, so the
	// only promises the domain makes about it are its v7 form and that the
	// row it names is immutable wherever it lives.
	AliasGroupVersionID string
	// FundingBucketID identifies one Accounting funding bucket (B6). Like
	// AliasGroupVersionID it is referenced blind — the bucket is another
	// aggregate's row, assigned by that context's choreography.
	FundingBucketID string
	// AliasGroupName is the operator-facing name of a catalog alias group,
	// or the reserved wildcard "*". Grant definitions carry the name; the
	// roll resolves it to the group's current AliasGroupVersionID.
	AliasGroupName string
)

// NewPlanID mints a time-ordered plan identifier.
func NewPlanID() (PlanID, error) {
	id, err := newUUIDv7()
	if err != nil {
		return "", fmt.Errorf("commerce: new plan id: %w", err)
	}
	return PlanID(id), nil
}

// NewPlanVersionID mints a time-ordered plan-version identifier.
func NewPlanVersionID() (PlanVersionID, error) {
	id, err := newUUIDv7()
	if err != nil {
		return "", fmt.Errorf("commerce: new plan version id: %w", err)
	}
	return PlanVersionID(id), nil
}

// NewGrantDefinitionID mints a time-ordered grant-definition identifier.
func NewGrantDefinitionID() (GrantDefinitionID, error) {
	id, err := newUUIDv7()
	if err != nil {
		return "", fmt.Errorf("commerce: new grant definition id: %w", err)
	}
	return GrantDefinitionID(id), nil
}

// NewSubscriptionID mints a time-ordered subscription identifier.
func NewSubscriptionID() (SubscriptionID, error) {
	id, err := newUUIDv7()
	if err != nil {
		return "", fmt.Errorf("commerce: new subscription id: %w", err)
	}
	return SubscriptionID(id), nil
}

// NewEntitlementID mints a time-ordered entitlement identifier.
func NewEntitlementID() (EntitlementID, error) {
	id, err := newUUIDv7()
	if err != nil {
		return "", fmt.Errorf("commerce: new entitlement id: %w", err)
	}
	return EntitlementID(id), nil
}

// The wildcard group name, as catalog spells it. A grant definition scoped
// to this name covers every alias — including aliases that do not exist
// yet; that open scope is the deliberate exception to "purchased scope
// never changes retroactively" (docs/architecture/commerce.md).
const WildcardGroupName = AliasGroupName("*")

// uuidV7Form is the canonical lowercase RFC 9562 version-7 shape: the third
// group's first nibble is the version (7), the fourth group's first nibble
// carries the RFC 4122 variant (89ab). It is the same grammar the commerce
// schema CHECKs on every commerce-minted column and on every blind
// reference — the database refuses what this validation would, and this
// validation refuses first, in the domain's own words.
var uuidV7Form = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// aliasGroupNameForm mirrors catalog's own group-name grammar verbatim
// (migrations/dataplane/000002_catalog_foundation.up.sql): either the
// reserved wildcard or 1..128 characters of letters, digits, dot,
// underscore, slash and hyphen, starting with a letter or digit. A grant
// definition can therefore never name a group the catalog would refuse to
// have.
var aliasGroupNameForm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

// validateAliasGroupName enforces the group-name grammar.
func validateAliasGroupName(name AliasGroupName) error {
	if name == WildcardGroupName {
		return nil
	}
	if !aliasGroupNameForm.MatchString(string(name)) {
		return fmt.Errorf("commerce: %w: %q is not an alias group name", ErrInvalidAliasGroupName, name)
	}
	return nil
}

// validateAliasGroupVersionID pins the v7 form of a catalog snapshot
// reference the domain did not mint.
func validateAliasGroupVersionID(id AliasGroupVersionID) error {
	if !uuidV7Form.MatchString(string(id)) {
		return fmt.Errorf("commerce: %w: %q is not a version-7 uuid", ErrInvalidAliasGroupVersionID, id)
	}
	return nil
}

// validateFundingBucketID pins the v7 form of an Accounting bucket
// reference the domain did not mint.
func validateFundingBucketID(id FundingBucketID) error {
	if !uuidV7Form.MatchString(string(id)) {
		return fmt.Errorf("commerce: %w: %q is not a version-7 uuid", ErrInvalidFundingBucketID, id)
	}
	return nil
}

// newUUIDv7 returns an RFC 9562 version-7 UUID in lowercase canonical form:
// forty-eight bits of Unix millisecond timestamp for ordering, then
// random version, variant and padding bits for uniqueness. The standard
// library is enough — sixteen bytes, two fields fixed, hex-encoded — the
// same judgment identity's v4 minter recorded; what differs is that the
// timestamp is real ordering, not decoration, so no two calls may ever
// share a millisecond bit pattern by construction and a sort by id is a
// rough sort by mint time. Monotonicity within a millisecond is not
// promised and nothing may depend on it: the waterfall's last tie-break
// orders by entitlement id precisely because ids order nothing else.
func newUUIDv7() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("commerce: read random bytes: %w", err)
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
