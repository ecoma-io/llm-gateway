package commerce

import (
	"errors"
	"strings"
	"testing"
)

func TestMintedIDsAreDistinctV7UUIDs(t *testing.T) {
	mints := map[string]func() (string, error){
		"plan":             func() (string, error) { id, err := NewPlanID(); return string(id), err },
		"plan version":     func() (string, error) { id, err := NewPlanVersionID(); return string(id), err },
		"grant definition": func() (string, error) { id, err := NewGrantDefinitionID(); return string(id), err },
		"subscription":     func() (string, error) { id, err := NewSubscriptionID(); return string(id), err },
		"entitlement":      func() (string, error) { id, err := NewEntitlementID(); return string(id), err },
	}
	for name, mint := range mints {
		id, err := mint()
		if err != nil {
			t.Fatalf("%s: mint returned error: %v", name, err)
		}
		if !uuidV7Form.MatchString(id) {
			t.Fatalf("%s: id %q is not a version-7 uuid in canonical lowercase form", name, id)
		}
	}
}

func TestMintedIDsDoNotCollide(t *testing.T) {
	seen := make(map[string]bool, 256)
	for i := 0; i < 256; i++ {
		id, err := NewSubscriptionID()
		if err != nil {
			t.Fatalf("mint %d returned error: %v", i, err)
		}
		if seen[string(id)] {
			t.Fatalf("mint %d repeated id %s", i, id)
		}
		seen[string(id)] = true
	}
}

func TestBlindReferenceValidation(t *testing.T) {
	t.Run("alias group version accepts a v7 form", func(t *testing.T) {
		id := AliasGroupVersionID("0198f0a4-3f6c-7000-8000-000000000001")
		if err := validateAliasGroupVersionID(id); err != nil {
			t.Fatalf("validateAliasGroupVersionID(%s) returned error: %v", id, err)
		}
	})
	t.Run("alias group version refuses a v4 form", func(t *testing.T) {
		v4 := AliasGroupVersionID("0198f0a4-3f6c-4000-8000-000000000001")
		if err := validateAliasGroupVersionID(v4); !errors.Is(err, ErrInvalidAliasGroupVersionID) {
			t.Fatalf("validateAliasGroupVersionID(%s) error = %v, want ErrInvalidAliasGroupVersionID", v4, err)
		}
	})
	t.Run("funding bucket refuses garbage", func(t *testing.T) {
		if err := ValidateFundingBucketID(FundingBucketID("bucket-1")); !errors.Is(err, ErrInvalidFundingBucketID) {
			t.Fatalf("ValidateFundingBucketID error = %v, want ErrInvalidFundingBucketID", err)
		}
	})
	t.Run("unset bucket reference is not validated", func(t *testing.T) {
		p, err := NewAccountPayg("acc-1", true, "", clock)
		if err != nil {
			t.Fatalf("NewAccountPayg with unset bucket returned error: %v", err)
		}
		if p.FundingBucketID != "" {
			t.Fatalf("FundingBucketID = %q, want unset", p.FundingBucketID)
		}
	})
}

func TestAliasGroupNameGrammar(t *testing.T) {
	valid := []AliasGroupName{
		WildcardGroupName,
		"anthropic",
		"openai.gpt-5",
		"google/gemini",
		"A1_b2.c3/d4-e5",
		AliasGroupName(strings.Repeat("a", 128)),
	}
	for _, name := range valid {
		if err := validateAliasGroupName(name); err != nil {
			t.Fatalf("validateAliasGroupName(%q) returned error: %v", name, err)
		}
	}
	invalid := []AliasGroupName{
		"",
		"* ",
		"**",
		" leading-space",
		"has space",
		"-leading-hyphen",
		".leading-dot",
		"exclamation!",
		AliasGroupName(strings.Repeat("a", 129)),
	}
	for _, name := range invalid {
		if err := validateAliasGroupName(name); !errors.Is(err, ErrInvalidAliasGroupName) {
			t.Fatalf("validateAliasGroupName(%q) error = %v, want ErrInvalidAliasGroupName", name, err)
		}
	}
}
