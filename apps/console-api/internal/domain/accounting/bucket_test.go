package accounting

import (
	"errors"
	"testing"
	"time"
)

// A bucket is born through exactly one of two constructors, funds exactly one
// owner, closes at most once, and never closes with funds still held. This
// file pins those clauses in the domain's own words; the schema's owner-xor
// and close rules mirror them.

var bucketNow = time.Date(2026, time.September, 25, 10, 0, 0, 0, time.UTC)

func mustBucketID(t *testing.T) FundingBucketID {
	t.Helper()
	id, err := NewFundingBucketID()
	if err != nil {
		t.Fatalf("mint funding bucket id: %v", err)
	}
	return id
}

func TestEntitlementBucketsAreBornActiveAtZero(t *testing.T) {
	id := mustBucketID(t)

	b, err := NewEntitlementBucket(id, EntitlementID(validV7), bucketNow)
	if err != nil {
		t.Fatalf("open entitlement bucket: %v", err)
	}
	if b.Status != BucketActive {
		t.Fatalf("new bucket status = %q, want active", b.Status)
	}
	if b.Settled != 0 || b.Held != 0 || b.Available != 0 {
		t.Fatalf("new bucket balances = (%d, %d, %d), want all zero", b.Settled, b.Held, b.Available)
	}
	if b.Version != 0 || b.LastSequence != 0 {
		t.Fatalf("new bucket version/sequence = (%d, %d), want (0, 0)", b.Version, b.LastSequence)
	}
	if !b.OwnedByEntitlement() || b.OwnedByAccount() {
		t.Fatalf("entitlement bucket ownership = (entitlement %v, account %v), want exactly the entitlement",
			b.OwnedByEntitlement(), b.OwnedByAccount())
	}
	if b.CreatedAt != bucketNow || b.UpdatedAt != bucketNow {
		t.Fatalf("bucket stamps = (%s, %s), want both %s in UTC", b.CreatedAt, b.UpdatedAt, bucketNow)
	}
}

func TestAccountBucketsFundThePaygBalance(t *testing.T) {
	b, err := NewAccountBucket(mustBucketID(t), "account-7", bucketNow)
	if err != nil {
		t.Fatalf("open account bucket: %v", err)
	}
	if !b.OwnedByAccount() || b.OwnedByEntitlement() {
		t.Fatalf("account bucket ownership = (entitlement %v, account %v), want exactly the account",
			b.OwnedByEntitlement(), b.OwnedByAccount())
	}
}

func TestABucketFundsExactlyOneOwner(t *testing.T) {
	if _, err := NewEntitlementBucket(mustBucketID(t), "", bucketNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("NewEntitlementBucket with a blank entitlement = %v, want ErrInvalidReference", err)
	}
	if _, err := NewAccountBucket(mustBucketID(t), "", bucketNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("NewAccountBucket with a blank account = %v, want ErrInvalidReference", err)
	}
	if _, err := NewAccountBucket(mustBucketID(t), validV7, bucketNow); err != nil {
		// The account reference deliberately carries no grammar; a uuid-shaped
		// account id is as legal as any other non-empty text.
		t.Fatalf("a uuid-shaped account id is legal text and must pass: %v", err)
	}
	if _, err := NewEntitlementBucket("", EntitlementID(validV7), bucketNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("a blank bucket id = %v, want ErrInvalidReference", err)
	}
}

func TestCloseIsAConvergedNoOpOnAnAlreadyClosedBucket(t *testing.T) {
	b, err := NewAccountBucket(mustBucketID(t), "account-7", bucketNow)
	if err != nil {
		t.Fatalf("open bucket: %v", err)
	}
	closed, err := b.Close(bucketNow)
	if err != nil {
		t.Fatalf("close an active bucket: %v", err)
	}
	if closed.Status != BucketClosed {
		t.Fatalf("closed bucket status = %q, want closed", closed.Status)
	}

	later := bucketNow.Add(time.Hour)
	again, err := closed.Close(later)
	if err != nil {
		t.Fatalf("close a closed bucket: %v", err)
	}
	if again.UpdatedAt != bucketNow {
		t.Fatalf("a converged close must not restamp the bucket: UpdatedAt = %s, want %s", again.UpdatedAt, bucketNow)
	}
}

func TestCloseRefusesWhileFundsAreHeld(t *testing.T) {
	b, err := NewAccountBucket(mustBucketID(t), "account-7", bucketNow)
	if err != nil {
		t.Fatalf("open bucket: %v", err)
	}
	b.Held = Balance(40)
	b.Available = Balance(60)

	if _, err := b.Close(bucketNow); !errors.Is(err, ErrBucketCloseBlocked) {
		t.Fatalf("close with %d held = %v, want ErrBucketCloseBlocked", b.Held, err)
	}
}

func TestCloseSetsTheStatusWithoutTouchingTheBalances(t *testing.T) {
	b, err := NewAccountBucket(mustBucketID(t), "account-7", bucketNow)
	if err != nil {
		t.Fatalf("open bucket: %v", err)
	}
	b.Settled, b.Available = Balance(100), Balance(100)

	closed, err := b.Close(bucketNow)
	if err != nil {
		t.Fatalf("close a funded bucket: %v", err)
	}
	if closed.Settled != Balance(100) || closed.Available != Balance(100) {
		t.Fatalf("a close is administrative: balances = (%d, %d), want untouched",
			closed.Settled, closed.Available)
	}
}
