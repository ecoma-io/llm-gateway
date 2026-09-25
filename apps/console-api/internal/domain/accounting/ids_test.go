package accounting

import (
	"errors"
	"strings"
	"testing"
)

// The identifier grammar this file pins is the one the schema CHECKs mirror:
// ids this domain mints are version-7 uuids, references from other contexts
// carry only the grammar their owner published, and every text field lives
// inside the length its column enforces. A refusal here is never a surprise
// at the driver later.

const (
	// A v7 uuid whose timestamp field is real but whose randomness is fixed;
	// only the form matters to these tests.
	validV7 = "0198c0a8-1b7d-7f6e-89ab-cdef01234567"
	// A v4 uuid — the shape identity mints. Legal as a reservation or an
	// account reference, illegal as anything this domain minted.
	validV4 = "3d6f8a20-93e1-4c2b-9a7d-5f1e2d3c4b5a"
	// Same v7 with an uppercase hex — canonical form is lowercase everywhere.
	uppercaseV7 = "0198C0A8-1B7D-7F6E-89AB-CDEF01234567"
	// A v7 whose variant nibble is not RFC 4122.
	wrongVariantV7 = "0198c0a8-1b7d-7f6e-c9ab-cdef01234567"
)

func TestMintedIdentifiersAreCanonicalVersionSeven(t *testing.T) {
	bucketID, err := NewFundingBucketID()
	if err != nil {
		t.Fatalf("mint funding bucket id: %v", err)
	}
	settlementID, err := NewSettlementID()
	if err != nil {
		t.Fatalf("mint settlement id: %v", err)
	}
	entryID, err := NewLedgerEntryID()
	if err != nil {
		t.Fatalf("mint ledger entry id: %v", err)
	}
	if err := validateMintedID(bucketID); err != nil {
		t.Fatalf("minted bucket id %s fails its own form check: %v", bucketID, err)
	}
	if err := validateMintedSettlementID(settlementID); err != nil {
		t.Fatalf("minted settlement id %s fails its own form check: %v", settlementID, err)
	}
	if err := validateMintedEntryID(entryID); err != nil {
		t.Fatalf("minted entry id %s fails its own form check: %v", entryID, err)
	}
}

func TestValidateMintedIDRefusesAnythingThatIsNotAVersionSevenForm(t *testing.T) {
	for name, id := range map[string]FundingBucketID{
		"blank":            "",
		"v4 uuid":          FundingBucketID(validV4),
		"uppercase":        FundingBucketID(uppercaseV7),
		"wrong variant":    FundingBucketID(wrongVariantV7),
		"not a uuid":       FundingBucketID("bucket-1"),
		"extra characters": FundingBucketID(validV7 + "0"),
	} {
		if err := validateMintedID(id); !errors.Is(err, ErrInvalidReference) {
			t.Errorf("%s: validateMintedID(%q) = %v, want ErrInvalidReference", name, id, err)
		}
	}
	if err := validateMintedID(FundingBucketID(validV7)); err != nil {
		t.Fatalf("a real v7 id must pass: %v", err)
	}
}

func TestEntitlementReferenceCarriesCommerceV7Grammar(t *testing.T) {
	if err := validateEntitlementID(EntitlementID(validV7)); err != nil {
		t.Fatalf("a v7 entitlement reference must pass: %v", err)
	}
	for name, id := range map[string]EntitlementID{
		"blank":     "",
		"v4 uuid":   EntitlementID(validV4),
		"uppercase": EntitlementID(uppercaseV7),
	} {
		if err := validateEntitlementID(id); !errors.Is(err, ErrInvalidReference) {
			t.Errorf("%s: validateEntitlementID(%q) = %v, want ErrInvalidReference", name, id, err)
		}
	}
}

func TestReservationReferenceAcceptsAnyCanonicalUuidVersion(t *testing.T) {
	if err := validateReservationID(ReservationID(validV4)); err != nil {
		t.Fatalf("a v4 reservation reference must pass: %v", err)
	}
	if err := validateReservationID(ReservationID(validV7)); err != nil {
		t.Fatalf("a v7 reservation reference must pass: %v", err)
	}
	if err := validateReservationID(""); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("validateReservationID(\"\") = %v, want ErrInvalidReference", err)
	}
	if err := validateReservationID(ReservationID(uppercaseV7)); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("an uppercase reservation reference must be refused: %v", err)
	}
}

func TestOpaqueCrossPlaneTextIsPresenceAndLengthOnly(t *testing.T) {
	atLimit := strings.Repeat("r", maxRequestIDLength)
	if err := validateRequestID(RequestID(atLimit)); err != nil {
		t.Fatalf("a request id at the %d-character ceiling must pass: %v", maxRequestIDLength, err)
	}
	if err := validateRequestID(RequestID(atLimit + "r")); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("a request id past the ceiling must be refused: %v", err)
	}
	if err := validateRequestID(""); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("a blank request id must be refused: %v", err)
	}
}

func TestCommandKeyIsPresenceAndLengthOnly(t *testing.T) {
	atLimit := strings.Repeat("k", maxCommandKeyLength)
	if err := validateCommandKey(CommandKey(atLimit)); err != nil {
		t.Fatalf("a command key at the ceiling must pass: %v", err)
	}
	if err := validateCommandKey(CommandKey(atLimit + "k")); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("a command key past the ceiling must be refused: %v", err)
	}
	if err := validateCommandKey(""); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("a blank command key must be refused: %v", err)
	}
}

func TestOperatorAndReasonAndPriceRevisionLimitsMatchTheirColumns(t *testing.T) {
	if err := validateOperatorID(OperatorID(strings.Repeat("o", maxOperatorIDLength))); err != nil {
		t.Fatalf("an operator id at the ceiling must pass: %v", err)
	}
	if err := validateOperatorID(OperatorID(strings.Repeat("o", maxOperatorIDLength+1))); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("an operator id past the ceiling must be refused: %v", err)
	}
	if err := validateAdjustmentReason(strings.Repeat("w", maxAdjustmentReasonValue)); err != nil {
		t.Fatalf("an adjustment reason at the ceiling must pass: %v", err)
	}
	if err := validateAdjustmentReason(strings.Repeat("w", maxAdjustmentReasonValue+1)); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("an adjustment reason past the ceiling must be refused: %v", err)
	}
	if err := validatePriceRevisionID(PriceRevisionID(strings.Repeat("p", maxPriceRevisionLength))); err != nil {
		t.Fatalf("a price revision id at the ceiling must pass: %v", err)
	}
	if err := validatePriceRevisionID(""); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("a blank price revision id must be refused: %v", err)
	}
}
