package commerce

import (
	"fmt"
	"time"
)

// AccountPayg is the per-account PAYG commercial state: the enablement flag
// this context owns, plus the funding-bucket reference ADR 0001 rule 5
// assigns to Accounting. Enabling authorises spending and funds nothing —
// the bucket row it names exists (from B6's account-creation choreography)
// with a zero balance before the first topup, so enabling is only a flag
// flip. Disabling blocks new spills at admission — the waterfall treats
// PAYG as absent — while holds already secured against the bucket settle
// normally against it.
//
// The row is keyed by the account: one PAYG source per account, ever. No
// row means the account has never enabled PAYG, and there is no expiry and
// no cycle here — PAYG is a balance, not a subscription, and never appears
// as a "current plan" because no such field exists in this model.
type AccountPayg struct {
	AccountID       AccountID
	Enabled         bool
	FundingBucketID FundingBucketID
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// NewAccountPayg returns the account's PAYG state row. The bucket reference
// may be unset — zero value — until the account-creation choreography
// (B6's) assigns it; a set reference is validated for the v7 form the
// schema pins, the one promise commerce makes about another aggregate's id.
func NewAccountPayg(accountID AccountID, enabled bool, fundingBucketID FundingBucketID, now time.Time) (*AccountPayg, error) {
	if accountID == "" {
		return nil, fmt.Errorf("commerce: new account payg: blank account id")
	}
	if fundingBucketID != "" {
		if err := validateFundingBucketID(fundingBucketID); err != nil {
			return nil, err
		}
	}
	stamp := now.UTC()
	return &AccountPayg{
		AccountID:       accountID,
		Enabled:         enabled,
		FundingBucketID: fundingBucketID,
		CreatedAt:       stamp,
		UpdatedAt:       stamp,
	}, nil
}

// Enable turns the spending flag on. Enabling an enabled account is a
// no-op: the intent is already recorded.
func (p *AccountPayg) Enable(now time.Time) {
	if p.Enabled {
		return
	}
	p.Enabled = true
	p.UpdatedAt = now.UTC()
}

// Disable turns the spending flag off. Disabling a disabled account is a
// no-op. Nothing here touches the bucket: open holds settle normally, and
// the funds are Accounting's to keep.
func (p *AccountPayg) Disable(now time.Time) {
	if !p.Enabled {
		return
	}
	p.Enabled = false
	p.UpdatedAt = now.UTC()
}

// AssignFundingBucket records the bucket reference the account-creation
// choreography assigns. It is write-once: the invariant is one PAYG source
// and one bucket per account, ever, so an attempt to reassign is refused —
// a changed reference would silently split the account's prepaid money
// across two buckets.
func (p *AccountPayg) AssignFundingBucket(bucketID FundingBucketID, now time.Time) error {
	if p.FundingBucketID != "" {
		return fmt.Errorf("commerce: assign funding bucket to account %s: %w: bucket %s is already assigned", p.AccountID, ErrInvalidTransition, p.FundingBucketID)
	}
	if err := validateFundingBucketID(bucketID); err != nil {
		return err
	}
	p.FundingBucketID = bucketID
	p.UpdatedAt = now.UTC()
	return nil
}
