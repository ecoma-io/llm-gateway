package accounting

import (
	"fmt"
	"time"
)

// BucketStatus is the bucket's whole lifecycle: active takes legs, closed
// takes none. There is no reopening — a closed bucket's history is complete,
// and a new funding need is a new bucket, not a resurrected one.
type BucketStatus string

const (
	// BucketActive is a bucket that takes legs.
	BucketActive BucketStatus = "active"
	// BucketClosed is a bucket whose history is complete. Close is
	// administrative and refuses while funds are held (ErrBucketCloseBlocked):
	// closing with open holds would strand money that a reservation still
	// means to settle against.
	BucketClosed BucketStatus = "closed"
)

// Bucket is the funding bucket aggregate: the authoritative capacity
// projection for exactly one entitlement cycle or one account PAYG balance
// (ADR 0004). Exactly one owner, ever — the owner-xor rule the schema pins
// and the constructors below enforce.
//
// The three cached balances are the projections ADR 0004 states formally:
//
//	settled   = Σ grants/topups − Σ consumes + Σ adjustment settled_deltas
//	held      = Σ holds − Σ releases − Σ consumes + Σ adjustment held_deltas
//	available = settled − held
//
// and they are a concurrency control projection, not an independently
// editable balance: every move goes through Apply with the leg that states
// it, and the persisted echo of that move is the guarded statement the
// ledger adapter runs in the same transaction as the leg's insert. If the
// cache and the legs ever disagree, the legs win — Reconcile exists to say
// so, and the cache is rebuildable from them.
type Bucket struct {
	ID            FundingBucketID
	EntitlementID EntitlementID // set exactly when AccountID is not
	AccountID     AccountID     // set exactly when EntitlementID is not
	Status        BucketStatus
	Version       int64
	LastSequence  int64
	Settled       Balance
	Held          Balance
	Available     Balance
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// NewEntitlementBucket opens the bucket behind one commerce entitlement's
// cycle: born active with zero balances, version zero and an empty ledger.
// The entitlement reference is validated for the v7 form commerce publishes;
// whether the entitlement exists is the foreign key's to say at insert time,
// because no grammar check can vouch for another context's row.
func NewEntitlementBucket(id FundingBucketID, entitlementID EntitlementID, now time.Time) (Bucket, error) {
	if err := validateEntitlementID(entitlementID); err != nil {
		return Bucket{}, err
	}
	return newBucket(id, entitlementID, "", now)
}

// NewAccountBucket opens the bucket behind one account's PAYG balance — the
// one control.account_payg references. The account reference carries no
// grammar beyond non-emptiness, because identity owns that identifier's
// shape and mints it outside this domain.
func NewAccountBucket(id FundingBucketID, accountID AccountID, now time.Time) (Bucket, error) {
	if accountID == "" {
		return Bucket{}, fmt.Errorf("accounting: %w: blank account id", ErrInvalidReference)
	}
	return newBucket(id, "", accountID, now)
}

// newBucket is the one birth path. Balances are born at zero and every
// bucket starts with an empty ledger, so the projections hold trivially.
func newBucket(id FundingBucketID, entitlementID EntitlementID, accountID AccountID, now time.Time) (Bucket, error) {
	if err := validateMintedID(id); err != nil {
		return Bucket{}, err
	}
	if (entitlementID == "") == (accountID == "") {
		return Bucket{}, fmt.Errorf("accounting: %w: a bucket funds exactly one of an entitlement cycle or an account PAYG balance", ErrInvalidReference)
	}
	stamp := now.UTC()
	return Bucket{
		ID:            id,
		EntitlementID: entitlementID,
		AccountID:     accountID,
		Status:        BucketActive,
		CreatedAt:     stamp,
		UpdatedAt:     stamp,
	}, nil
}

// Close applies the administrative close. Closing a closed bucket is a
// converged no-op — the intent is already recorded — and closing one with
// funds still held is refused: the holds are ceilings that must settle or
// release first, and stranding them is not a state, it is a loss.
func (b Bucket) Close(now time.Time) (Bucket, error) {
	if b.Status == BucketClosed {
		return b, nil
	}
	if b.Held > 0 {
		return Bucket{}, fmt.Errorf("accounting: close bucket %s: %w (%d minor units held)", b.ID, ErrBucketCloseBlocked, b.Held)
	}
	b.Status = BucketClosed
	b.UpdatedAt = now.UTC()
	return b, nil
}

// OwnedByEntitlement reports whether the bucket funds an entitlement cycle.
func (b Bucket) OwnedByEntitlement() bool { return b.EntitlementID != "" }

// OwnedByAccount reports whether the bucket funds a PAYG balance.
func (b Bucket) OwnedByAccount() bool { return b.AccountID != "" }
