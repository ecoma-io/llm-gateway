//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The accounting repositories against the real `control` database, on the
// same terms as the commerce suite above: the fake-driver tests pin which
// handle a statement runs on and the unit tests pin the algebra, so what
// only PostgreSQL can answer lives here — that the guarded echoes fire
// exactly once when they can and zero rows when they must, that a guard
// miss classifies from a fresh read and leaves the caller's unit of work
// alive, that the uniqueness constraints arrive as the port's two duplicate
// sentinels with the savepoint bracket intact, that a settlement header is
// exactly-once by its request id while its legs land in the same unit of
// work, and that two concurrent holds of 80 against an available 100 cannot
// both succeed — the row lock and the WHERE clause, not good intentions,
// are the guard.
//
// What is deliberately NOT re-proven here is the pure SQL-level schema
// behaviour — the append-only triggers, the owner-xor and projection
// CHECKs, the leg-algebra and price-provenance guards, the write-once
// trigger on the PAYG reference as raw writes: deploy/postgres/verify.sh
// owns that tier.
//
// Conventions this file holds: no t.Parallel anywhere (the database is
// shared); every id and key is run-unique, minted by the domain's v7
// minters, so a rerun against an already-migrated database cannot trip over
// an earlier run's rows; fixtures reach their states through the ports — a
// cycle bucket's entitlement row is the ports' to create, and funding moves
// through Append inside real units of work, never through crafted rows.
// Nothing here deletes: the schema has no delete path and the ids carry the
// isolation.
//
// Run (the compose project is port-shifted per worktree):
//
//	GATEWAY_POSTGRES_PROJECT=llm-gateway-postgres-murex GATEWAY_POSTGRES_PORT=55435 \
//	  docker compose -f deploy/postgres/compose.yaml up -d
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:55435/postgres?sslmode=disable' \
//	  go test -tags=integration -race ./internal/adapters/outbound/postgres

// accountingRepos gathers the pool and every accounting repository over it,
// plus the commerce repositories — a cycle bucket's foreign key is a
// commerce entitlement row, and the identity accounts repository builds the
// owners PAYG buckets hang from.
type accountingRepos struct {
	db          *sql.DB
	store       persistence.Store
	buckets     persistence.FundingBuckets
	ledger      persistence.FundingLedger
	settlements persistence.Settlements
	projections persistence.FundingProjections
	payg        persistence.PaygAccounts
	commerce    *commerceRepos
}

// integrationAccounting opens the migrated control database and builds the
// accounting repositories over it.
func integrationAccounting(t *testing.T) *accountingRepos {
	t.Helper()
	c := integrationCommerce(t)
	return &accountingRepos{
		db:          c.db,
		store:       c.store,
		buckets:     NewFundingBuckets(c.store),
		ledger:      NewFundingLedger(c.store),
		settlements: NewSettlements(c.store),
		projections: NewFundingProjections(c.store),
		payg:        c.payg,
		commerce:    c,
	}
}

// ---------------------------------------------------------------------------
// fixtures — ids, entries and buckets, every one fatal-on-error.
// ---------------------------------------------------------------------------

func acctAmount(t *testing.T, raw int64) accounting.Amount {
	t.Helper()
	amount, err := accounting.NewAmount(raw)
	if err != nil {
		t.Fatalf("amount %d: %v", raw, err)
	}
	return amount
}

func acctBucketID(t *testing.T) accounting.FundingBucketID {
	t.Helper()
	id, err := accounting.NewFundingBucketID()
	if err != nil {
		t.Fatalf("mint bucket id: %v", err)
	}
	return id
}

func acctEntryID(t *testing.T) accounting.LedgerEntryID {
	t.Helper()
	id, err := accounting.NewLedgerEntryID()
	if err != nil {
		t.Fatalf("mint entry id: %v", err)
	}
	return id
}

func acctSettlementID(t *testing.T) accounting.SettlementID {
	t.Helper()
	id, err := accounting.NewSettlementID()
	if err != nil {
		t.Fatalf("mint settlement id: %v", err)
	}
	return id
}

// acctReservation mints a reservation reference. The grammar is the Data
// Plane's to own and it has not been published; this domain accepts any
// canonical uuid, and a freshly minted v7 is one.
func acctReservation(t *testing.T) accounting.ReservationID {
	t.Helper()
	return accounting.ReservationID(acctSettlementID(t))
}

// acctCommandKey mints a run-unique idempotency key: the prefix tells the
// tables apart on sight, the v7 carries the run-uniqueness.
func acctCommandKey(t *testing.T, prefix string) accounting.CommandKey {
	t.Helper()
	return accounting.CommandKey(prefix + string(acctSettlementID(t)))
}

// acctRequestID mints a run-unique settlement key. The request id is opaque
// text by design; the minted v7 is only the uniqueness.
func acctRequestID(t *testing.T) accounting.RequestID {
	t.Helper()
	return accounting.RequestID("req-" + string(acctSettlementID(t)))
}

// acctPrice is a well-formed price snapshot — revision present, both unit
// prices positive — the provenance a consume leg must carry.
func acctPrice(t *testing.T) accounting.PriceSnapshot {
	t.Helper()
	return accounting.PriceSnapshot{
		RevisionID:      accounting.PriceRevisionID("rev-" + string(acctSettlementID(t))),
		InputUnitPrice:  acctAmount(t, 1),
		OutputUnitPrice: acctAmount(t, 2),
	}
}

func (a *accountingRepos) grantEntry(t *testing.T, bucketID accounting.FundingBucketID, raw int64) accounting.LedgerEntry {
	t.Helper()
	entry, err := accounting.NewGrantEntry(acctEntryID(t), bucketID, acctAmount(t, raw), time.Now().UTC())
	if err != nil {
		t.Fatalf("grant entry: %v", err)
	}
	return entry
}

func (a *accountingRepos) topupEntry(t *testing.T, bucketID accounting.FundingBucketID, raw int64, key accounting.CommandKey) accounting.LedgerEntry {
	t.Helper()
	entry, err := accounting.NewTopupEntry(acctEntryID(t), bucketID, acctAmount(t, raw), key, time.Now().UTC())
	if err != nil {
		t.Fatalf("topup entry: %v", err)
	}
	return entry
}

func (a *accountingRepos) holdEntry(t *testing.T, bucketID accounting.FundingBucketID, raw int64, reservation accounting.ReservationID) accounting.LedgerEntry {
	t.Helper()
	entry, err := accounting.NewHoldEntry(acctEntryID(t), bucketID, acctAmount(t, raw), reservation, time.Now().UTC())
	if err != nil {
		t.Fatalf("hold entry: %v", err)
	}
	return entry
}

func (a *accountingRepos) releaseEntry(t *testing.T, bucketID accounting.FundingBucketID, raw int64,
	reservation accounting.ReservationID, settlementID accounting.SettlementID) accounting.LedgerEntry {
	t.Helper()
	entry, err := accounting.NewReleaseEntry(acctEntryID(t), bucketID, acctAmount(t, raw), reservation, settlementID, time.Now().UTC())
	if err != nil {
		t.Fatalf("release entry: %v", err)
	}
	return entry
}

func (a *accountingRepos) adjustEntry(t *testing.T, bucketID accounting.FundingBucketID, settled, held accounting.Delta,
	original accounting.LedgerEntryID, key accounting.CommandKey) accounting.LedgerEntry {
	t.Helper()
	entry, err := accounting.NewAdjustmentEntry(acctEntryID(t), bucketID, settled, held,
		"integration correction", original, "it-operator", key, time.Now().UTC())
	if err != nil {
		t.Fatalf("adjustment entry: %v", err)
	}
	return entry
}

func (a *accountingRepos) consumeEntry(t *testing.T, bucketID accounting.FundingBucketID, raw int64,
	settlementID accounting.SettlementID) accounting.LedgerEntry {
	t.Helper()
	entry, err := accounting.NewConsumeEntry(acctEntryID(t), bucketID, acctAmount(t, raw), settlementID,
		acctPrice(t), time.Now().UTC())
	if err != nil {
		t.Fatalf("consume entry: %v", err)
	}
	return entry
}

// newAccountBucket opens a PAYG bucket through the ports: an identity
// account row first (every accounting foreign key is RESTRICT and the owner
// must exist), then the bucket itself.
func (a *accountingRepos) newAccountBucket(t *testing.T, name string) accounting.Bucket {
	t.Helper()
	accountID := integrationCommerceAccount(t, a.commerce, name)
	bucket, err := accounting.NewAccountBucket(acctBucketID(t), accounting.AccountID(accountID), time.Now().UTC())
	if err != nil {
		t.Fatalf("new account bucket: %v", err)
	}
	if err := a.buckets.Create(t.Context(), bucket); err != nil {
		t.Fatalf("create account bucket: %v", err)
	}
	return bucket
}

// newAccountBucketFor opens a PAYG bucket owned by the account the caller
// names — the reference choreography's fixture, where the assignment must
// pair the row with its own account's bucket.
func (a *accountingRepos) newAccountBucketFor(t *testing.T, accountID commerce.AccountID) accounting.Bucket {
	t.Helper()
	bucket, err := accounting.NewAccountBucket(acctBucketID(t), accounting.AccountID(accountID), time.Now().UTC())
	if err != nil {
		t.Fatalf("new account bucket: %v", err)
	}
	if err := a.buckets.Create(t.Context(), bucket); err != nil {
		t.Fatalf("create account bucket: %v", err)
	}
	return bucket
}

// newEntitlementBucket opens a cycle bucket whose entitlement row the
// commerce ports built — the row the funding_buckets_entitlement_id_fkey
// demands, in the state a cycle-1 promotion grant produces: a subscription
// row (pending is enough; the FK asks for existence, not state) and the
// entitlement the promotion grants over its first cycle's bounds. The
// guarded advance that pairs the grant with the cycle flip is the roll
// lane's choreography and commerce's suite's subject; the row is what this
// suite needs.
func (a *accountingRepos) newEntitlementBucket(t *testing.T, name string) accounting.Bucket {
	t.Helper()
	c := a.commerce
	plan := newIntegrationPlan(t, c)
	version, definitions := publishVersionWithDefinitions(t, c, plan.ID, commerce.WildcardGroupName)
	account := integrationCommerceAccount(t, c, name)
	subscription := pendingSubscription(t, c, account, version.ID, matureStartAt(time.Now().UTC()), true)
	periodStart, periodEnd := mustCycleBounds(t, subscription.StartAt, 1)
	entitlement := grantCycle(t, subscription.ID, 1, definitions[0], periodStart, periodEnd)
	if err := c.entitlements.Create(t.Context(), *entitlement); err != nil {
		t.Fatalf("create entitlement row: %v", err)
	}
	bucket, err := accounting.NewEntitlementBucket(acctBucketID(t), accounting.EntitlementID(entitlement.ID), time.Now().UTC())
	if err != nil {
		t.Fatalf("new entitlement bucket: %v", err)
	}
	if err := a.buckets.Create(t.Context(), bucket); err != nil {
		t.Fatalf("create entitlement bucket: %v", err)
	}
	return bucket
}

// appendCommitted runs one append in its own committed unit of work and
// returns the stamped leg and the bucket as the echo left it — the
// fixture's way of moving the world through the port and never around it.
func (a *accountingRepos) appendCommitted(t *testing.T, entry accounting.LedgerEntry) (accounting.LedgerEntry, accounting.Bucket) {
	t.Helper()
	var stamped accounting.LedgerEntry
	var after accounting.Bucket
	err := a.store.WithinTx(t.Context(), func(ctx context.Context) error {
		var err error
		stamped, after, err = a.ledger.Append(ctx, entry)
		return err
	})
	if err != nil {
		t.Fatalf("append %s entry to bucket %s: %v", entry.Kind, entry.FundingBucketID, err)
	}
	return stamped, after
}

// assertBalances reads the bucket and demands the cached projection read
// exactly the stated balances at the stated version and sequence — the
// echo's arithmetic, checked after the fact.
func (a *accountingRepos) assertBalances(t *testing.T, id accounting.FundingBucketID,
	settled, held, available int64, version, lastSequence int64) accounting.Bucket {
	t.Helper()
	bucket, err := a.buckets.ByID(t.Context(), id)
	if err != nil {
		t.Fatalf("read bucket %s: %v", id, err)
	}
	if bucket.Settled != accounting.Balance(settled) || bucket.Held != accounting.Balance(held) ||
		bucket.Available != accounting.Balance(available) {
		t.Fatalf("bucket %s = (settled %d, held %d, available %d), want (%d, %d, %d)",
			id, bucket.Settled, bucket.Held, bucket.Available, settled, held, available)
	}
	if bucket.Version != version || bucket.LastSequence != lastSequence {
		t.Fatalf("bucket %s = (version %d, last_sequence %d), want (%d, %d)",
			id, bucket.Version, bucket.LastSequence, version, lastSequence)
	}
	return bucket
}

// ---------------------------------------------------------------------------
// the flows.
// ---------------------------------------------------------------------------

// TestIntegrationAccountingAccountFundingWalksTheLedger funds one PAYG
// bucket and walks it through the waterfall, checking at each step that the
// echo's verdict is the bucket's truth and the ledger's reads can name every
// leg by its key. The balances are the projection; the legs are the truth;
// the derivation is what says so.
func TestIntegrationAccountingAccountFundingWalksTheLedger(t *testing.T) {
	a := integrationAccounting(t)
	ctx := t.Context()
	bucket := a.newAccountBucket(t, "it-accounting lifecycle bucket")

	key := acctCommandKey(t, "topup-")
	topup, after := a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, key))
	if topup.Sequence != 1 || after.LastSequence != 1 || after.Version != 1 {
		t.Fatalf("the first leg takes sequence 1 and version 1, got (leg %d, row %d/%d)",
			topup.Sequence, after.Version, after.LastSequence)
	}
	stored := a.assertBalances(t, bucket.ID, 10000, 0, 10000, 1, 1)

	byAccount, err := a.buckets.ByAccountID(ctx, stored.AccountID)
	if err != nil || byAccount.ID != bucket.ID {
		t.Fatalf("ByAccountID = (%s, %v), want the bucket", byAccount.ID, err)
	}

	reservation := acctReservation(t)
	hold, after := a.appendCommitted(t, a.holdEntry(t, bucket.ID, 4000, reservation))
	if hold.Sequence != 2 {
		t.Fatalf("the hold takes sequence 2, got %d", hold.Sequence)
	}
	a.assertBalances(t, bucket.ID, 10000, 4000, 6000, 2, 2)

	release, _ := a.appendCommitted(t, a.releaseEntry(t, bucket.ID, 1500, reservation, ""))
	if release.Sequence != 3 {
		t.Fatalf("the release takes sequence 3, got %d", release.Sequence)
	}
	a.assertBalances(t, bucket.ID, 10000, 2500, 7500, 3, 3)

	found, err := a.ledger.ByBucketAndCommandKey(ctx, bucket.ID, key)
	if err != nil || found.ID != topup.ID {
		t.Fatalf("ByBucketAndCommandKey = (%s, %v), want the topup leg", found.ID, err)
	}
	holdLeg, err := a.ledger.ByBucketReservationAndKind(ctx, bucket.ID, reservation, accounting.KindHold)
	if err != nil || holdLeg.ID != hold.ID {
		t.Fatalf("ByBucketReservationAndKind(hold) = (%s, %v), want the hold leg", holdLeg.ID, err)
	}
	releaseLeg, err := a.ledger.ByBucketReservationAndKind(ctx, bucket.ID, reservation, accounting.KindRelease)
	if err != nil || releaseLeg.ID != release.ID {
		t.Fatalf("ByBucketReservationAndKind(release) = (%s, %v), want the release leg", releaseLeg.ID, err)
	}

	derived, err := a.projections.DerivedBalances(ctx, bucket.ID)
	if err != nil {
		t.Fatalf("derive balances: %v", err)
	}
	if derived.Legs != 3 || !derived.ConsistentWith(a.assertBalances(t, bucket.ID, 10000, 2500, 7500, 3, 3)) {
		t.Fatalf("the derivation (%d settled, %d held, %d available, %d legs) must agree with the cached row",
			derived.Settled, derived.Held, derived.Available, derived.Legs)
	}

	// Zeroing the hold takes a correction, not a second release: the first
	// reservation's release leg already exists — one release per reservation
	// per bucket is the uniqueness — and the adjustment is the ledger's only
	// other way to move held money. The tail goes back by correction, and
	// only then does the bucket close: the domain refuses an administrative
	// close while held funds remain.
	a.appendCommitted(t, a.adjustEntry(t, bucket.ID, 0, -2500, release.ID, ""))
	// The close is compare-and-swapped on a FRESH read — the same
	// choreography the use case runs — and the balances walk out untouched.
	current := a.assertBalances(t, bucket.ID, 10000, 0, 10000, 4, 4)
	closed, err := current.Close(time.Now().UTC())
	if err != nil {
		t.Fatalf("close the bucket: %v", err)
	}
	applied, err := a.buckets.Close(ctx, bucket.ID, closed.Version, closed.UpdatedAt)
	if err != nil || !applied {
		t.Fatalf("close an unheld bucket = (%t, %v), want (true, nil)", applied, err)
	}
	if after := a.assertBalances(t, bucket.ID, 10000, 0, 10000, 4, 4); after.Status != accounting.BucketClosed {
		t.Fatalf("the closed bucket reports status %q", after.Status)
	}
}

// TestIntegrationAccountingGuardMissesClassifyAndLeaveTheWorkAlive walks
// every echo's refusal: zero rows arrive as the domain's named error, the
// refused leg leaves nothing behind, and — the savepoint's whole point —
// the unit of work that suffered the refusal goes on to commit a leg the
// guard does admit.
func TestIntegrationAccountingGuardMissesClassifyAndLeaveTheWorkAlive(t *testing.T) {
	a := integrationAccounting(t)
	ctx := t.Context()

	t.Run("a hold above available is refused", func(t *testing.T) {
		bucket := a.newAccountBucket(t, "it-accounting hold guard")
		a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, acctCommandKey(t, "seed-")))

		err := a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.holdEntry(t, bucket.ID, 80000, acctReservation(t)))
			if !errors.Is(err, accounting.ErrInsufficientAvailable) {
				t.Fatalf("hold 80000 against 10000 = %v, want ErrInsufficientAvailable", err)
			}
			// The unit of work is unharmed: the next leg the guard admits lands
			// and commits.
			_, _, err = a.ledger.Append(ctx, a.holdEntry(t, bucket.ID, 2000, acctReservation(t)))
			return err
		})
		if err != nil {
			t.Fatalf("the unit of work that suffered a guard miss must commit: %v", err)
		}
		a.assertBalances(t, bucket.ID, 10000, 2000, 8000, 2, 2)
	})

	t.Run("a release above held is refused", func(t *testing.T) {
		bucket := a.newAccountBucket(t, "it-accounting release guard")
		a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, acctCommandKey(t, "seed-")))
		reservation := acctReservation(t)
		a.appendCommitted(t, a.holdEntry(t, bucket.ID, 3000, reservation))

		err := a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.releaseEntry(t, bucket.ID, 9999, reservation, ""))
			if !errors.Is(err, accounting.ErrInsufficientHeld) {
				t.Fatalf("release 9999 against 3000 held = %v, want ErrInsufficientHeld", err)
			}
			_, _, err = a.ledger.Append(ctx, a.releaseEntry(t, bucket.ID, 1000, reservation, ""))
			return err
		})
		if err != nil {
			t.Fatalf("the unit of work that suffered a guard miss must commit: %v", err)
		}
		a.assertBalances(t, bucket.ID, 10000, 2000, 8000, 3, 3)
	})

	t.Run("an adjustment that overdraws is refused", func(t *testing.T) {
		bucket := a.newAccountBucket(t, "it-accounting adjustment guard")
		seed, _ := a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, acctCommandKey(t, "seed-")))

		err := a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.adjustEntry(t, bucket.ID, -20000, 0, seed.ID, ""))
			if !errors.Is(err, accounting.ErrInvalidAdjustment) {
				t.Fatalf("adjustment of -20000 against settled 10000 = %v, want ErrInvalidAdjustment", err)
			}
			_, _, err = a.ledger.Append(ctx, a.adjustEntry(t, bucket.ID, -3000, 0, seed.ID, ""))
			return err
		})
		if err != nil {
			t.Fatalf("the unit of work that suffered a guard miss must commit: %v", err)
		}
		// The correction fixed the record; it did not credit or overdraw it.
		a.assertBalances(t, bucket.ID, 7000, 0, 7000, 2, 2)
	})

	t.Run("a consume above held is refused", func(t *testing.T) {
		bucket := a.newAccountBucket(t, "it-accounting consume guard")
		a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, acctCommandKey(t, "seed-")))
		a.appendCommitted(t, a.holdEntry(t, bucket.ID, 4000, acctReservation(t)))
		settlement := acctSettlementID(t)
		// A consume leg names a settlement header on file — the foreign key
		// the ledger carries — so the header exists before the legs do.
		if _, err := a.settlements.Create(ctx, accounting.Settlement{
			ID:           settlement,
			RequestID:    acctRequestID(t),
			SettledTotal: 0,
			CreatedAt:    time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed the settlement header: %v", err)
		}

		err := a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.consumeEntry(t, bucket.ID, 5000, settlement))
			if !errors.Is(err, accounting.ErrInsufficientHeld) {
				t.Fatalf("consume 5000 against held 4000 = %v, want ErrInsufficientHeld", err)
			}
			_, _, err = a.ledger.Append(ctx, a.consumeEntry(t, bucket.ID, 3000, settlement))
			return err
		})
		if err != nil {
			t.Fatalf("the unit of work that suffered a guard miss must commit: %v", err)
		}
		a.assertBalances(t, bucket.ID, 7000, 1000, 6000, 3, 3)
	})

	t.Run("an adjustment that underflows held is refused", func(t *testing.T) {
		bucket := a.newAccountBucket(t, "it-accounting held underflow")
		seed, _ := a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, acctCommandKey(t, "seed-")))
		a.appendCommitted(t, a.holdEntry(t, bucket.ID, 4000, acctReservation(t)))

		err := a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.adjustEntry(t, bucket.ID, 0, -5000, seed.ID, ""))
			if !errors.Is(err, accounting.ErrInvalidAdjustment) {
				t.Fatalf("adjustment of held −5000 against held 4000 = %v, want ErrInvalidAdjustment", err)
			}
			_, _, err = a.ledger.Append(ctx, a.adjustEntry(t, bucket.ID, 0, -1000, seed.ID, ""))
			return err
		})
		if err != nil {
			t.Fatalf("the unit of work that suffered a guard miss must commit: %v", err)
		}
		a.assertBalances(t, bucket.ID, 10000, 3000, 7000, 3, 3)
	})

	t.Run("an adjustment that underflows available is refused", func(t *testing.T) {
		bucket := a.newAccountBucket(t, "it-accounting available underflow")
		seed, _ := a.appendCommitted(t, a.topupEntry(t, bucket.ID, 5000, acctCommandKey(t, "seed-")))
		a.appendCommitted(t, a.holdEntry(t, bucket.ID, 4000, acctReservation(t)))

		err := a.store.WithinTx(ctx, func(ctx context.Context) error {
			// One delta alone can still leave the algebra negative: settled
			// 2000 under held 4000 is an available the no-credit rule
			// refuses, though neither balance itself went below zero.
			_, _, err := a.ledger.Append(ctx, a.adjustEntry(t, bucket.ID, -3000, 0, seed.ID, ""))
			if !errors.Is(err, accounting.ErrInvalidAdjustment) {
				t.Fatalf("adjustment to available −2000 = %v, want ErrInvalidAdjustment", err)
			}
			_, _, err = a.ledger.Append(ctx, a.adjustEntry(t, bucket.ID, -500, 0, seed.ID, ""))
			return err
		})
		if err != nil {
			t.Fatalf("the unit of work that suffered a guard miss must commit: %v", err)
		}
		a.assertBalances(t, bucket.ID, 4500, 4000, 500, 3, 3)
	})

	t.Run("a topup on a closed bucket is refused", func(t *testing.T) {
		bucket := a.newAccountBucket(t, "it-accounting closed gate")
		stored := a.assertBalances(t, bucket.ID, 0, 0, 0, 0, 0)
		closed, err := stored.Close(time.Now().UTC())
		if err != nil {
			t.Fatalf("close the bucket: %v", err)
		}
		if applied, err := a.buckets.Close(ctx, bucket.ID, closed.Version, closed.UpdatedAt); err != nil || !applied {
			t.Fatalf("close = (%t, %v), want (true, nil)", applied, err)
		}

		err = a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.topupEntry(t, bucket.ID, 5000, acctCommandKey(t, "topup-")))
			return err
		})
		if !errors.Is(err, accounting.ErrBucketClosed) {
			t.Fatalf("append to a closed bucket = %v, want ErrBucketClosed", err)
		}
	})

	t.Run("a leg naming an absent bucket is refused", func(t *testing.T) {
		ghost := acctBucketID(t)
		err := a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.grantEntry(t, ghost, 1000))
			return err
		})
		if !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("append to an absent bucket = %v, want ErrNotFound", err)
		}
	})
}

// TestIntegrationAccountingAFunderRefusesTheOtherOwner proves the owner
// half of the funder echoes: a grant funds a cycle bucket and a topup an
// account's, so each echo's WHERE clause names its owner and the leg for
// the other owner is a guard miss the classification names as the invalid
// transition it is — with the unit of work alive underneath, its own
// funder still landing.
func TestIntegrationAccountingAFunderRefusesTheOtherOwner(t *testing.T) {
	a := integrationAccounting(t)
	ctx := t.Context()

	t.Run("a topup on a cycle bucket is refused", func(t *testing.T) {
		bucket := a.newEntitlementBucket(t, "it-accounting topup owner")
		a.appendCommitted(t, a.grantEntry(t, bucket.ID, 10000))

		err := a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.topupEntry(t, bucket.ID, 5000, acctCommandKey(t, "topup-owner-")))
			if !errors.Is(err, accounting.ErrInvalidTransition) {
				t.Fatalf("topup on cycle bucket %s = %v, want ErrInvalidTransition", bucket.ID, err)
			}
			// The unit of work is unharmed: the bucket's own funder lands.
			_, _, err = a.ledger.Append(ctx, a.grantEntry(t, bucket.ID, 1000))
			return err
		})
		if err != nil {
			t.Fatalf("the unit of work that suffered an owner miss must commit: %v", err)
		}
		a.assertBalances(t, bucket.ID, 11000, 0, 11000, 2, 2)
	})

	t.Run("a grant on an account bucket is refused", func(t *testing.T) {
		bucket := a.newAccountBucket(t, "it-accounting grant owner")
		a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, acctCommandKey(t, "topup-feed-")))

		err := a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.grantEntry(t, bucket.ID, 5000))
			if !errors.Is(err, accounting.ErrInvalidTransition) {
				t.Fatalf("grant on account bucket %s = %v, want ErrInvalidTransition", bucket.ID, err)
			}
			_, _, err = a.ledger.Append(ctx, a.topupEntry(t, bucket.ID, 1000, acctCommandKey(t, "topup-more-")))
			return err
		})
		if err != nil {
			t.Fatalf("the unit of work that suffered an owner miss must commit: %v", err)
		}
		a.assertBalances(t, bucket.ID, 11000, 0, 11000, 2, 2)
	})
}

// TestIntegrationAccountingLegsCiteTheirOwnProvenance proves the provenance
// half of the release and adjustment echoes: a release names a reservation
// this bucket held, and a correction cites this bucket's own leg — the
// foreign variants are refused as invalid references, with the unit of work
// alive underneath. The INSERT-time triggers are the same promise for
// writers that skip the echo; deploy/postgres/verify.sh owns that tier.
func TestIntegrationAccountingLegsCiteTheirOwnProvenance(t *testing.T) {
	a := integrationAccounting(t)
	ctx := t.Context()

	t.Run("a release names a hold on file", func(t *testing.T) {
		bucket := a.newAccountBucket(t, "it-accounting release provenance")
		a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, acctCommandKey(t, "topup-prov-")))
		held := acctReservation(t)
		a.appendCommitted(t, a.holdEntry(t, bucket.ID, 4000, held))

		err := a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.releaseEntry(t, bucket.ID, 4000, acctReservation(t), ""))
			if !errors.Is(err, accounting.ErrInvalidReference) {
				t.Fatalf("release naming an unheld reservation = %v, want ErrInvalidReference", err)
			}
			// The unit of work is unharmed: the hold's own release lands.
			_, _, err = a.ledger.Append(ctx, a.releaseEntry(t, bucket.ID, 4000, held, ""))
			return err
		})
		if err != nil {
			t.Fatalf("the unit of work that suffered a provenance miss must commit: %v", err)
		}
		a.assertBalances(t, bucket.ID, 10000, 0, 10000, 3, 3)
	})

	t.Run("an adjustment cites its own bucket's leg", func(t *testing.T) {
		bucket := a.newAccountBucket(t, "it-accounting adjust provenance")
		foreign := a.newAccountBucket(t, "it-accounting adjust foreign")
		own, _ := a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, acctCommandKey(t, "topup-own-")))
		stray, _ := a.appendCommitted(t, a.topupEntry(t, foreign.ID, 5000, acctCommandKey(t, "topup-foreign-")))

		err := a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.adjustEntry(t, bucket.ID, -1000, 0, stray.ID, ""))
			if !errors.Is(err, accounting.ErrInvalidReference) {
				t.Fatalf("adjustment citing another bucket's leg = %v, want ErrInvalidReference", err)
			}
			// The unit of work is unharmed: the bucket's own correction lands.
			_, _, err = a.ledger.Append(ctx, a.adjustEntry(t, bucket.ID, -1000, 0, own.ID, ""))
			return err
		})
		if err != nil {
			t.Fatalf("the unit of work that suffered a provenance miss must commit: %v", err)
		}
		a.assertBalances(t, bucket.ID, 9000, 0, 9000, 2, 2)
	})
}

// TestIntegrationAccountingCollisionsMapToTheirSentinels loses to both
// uniqueness constraints on purpose. The adapter's verdict is the sentinel
// and a living unit of work — the payload comparison that turns a collision
// into convergence is the caller's, and the integration tier proves only
// that the constraint fires, the savepoint keeps the transaction usable,
// and the uniqueness is per bucket.
func TestIntegrationAccountingCollisionsMapToTheirSentinels(t *testing.T) {
	a := integrationAccounting(t)
	ctx := t.Context()

	t.Run("the same command key twice is a duplicate command", func(t *testing.T) {
		bucket := a.newAccountBucket(t, "it-accounting command collision")
		key := acctCommandKey(t, "topup-")
		a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, acctCommandKey(t, "seed-")))
		a.appendCommitted(t, a.topupEntry(t, bucket.ID, 5000, key))

		err := a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.topupEntry(t, bucket.ID, 5000, key))
			if !errors.Is(err, accounting.ErrDuplicateCommand) {
				t.Fatalf("a replayed command key = %v, want ErrDuplicateCommand", err)
			}
			// The bracket held: this unit of work still commits a leg of its own.
			_, _, err = a.ledger.Append(ctx, a.holdEntry(t, bucket.ID, 1000, acctReservation(t)))
			return err
		})
		if err != nil {
			t.Fatalf("the unit of work that lost the collision must commit: %v", err)
		}
		original, err := a.ledger.ByBucketAndCommandKey(ctx, bucket.ID, key)
		if err != nil || original.Amount != acctAmount(t, 5000) {
			t.Fatalf("the surviving leg = (amount %d, %v), want the original's 5000", original.Amount, err)
		}
		a.assertBalances(t, bucket.ID, 15000, 1000, 14000, 3, 3)
	})

	t.Run("the same reservation and kind twice is a duplicate movement", func(t *testing.T) {
		bucket := a.newAccountBucket(t, "it-accounting movement collision")
		a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, acctCommandKey(t, "seed-")))
		reservation := acctReservation(t)
		a.appendCommitted(t, a.holdEntry(t, bucket.ID, 2000, reservation))

		err := a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.holdEntry(t, bucket.ID, 2000, reservation))
			if !errors.Is(err, accounting.ErrDuplicateMovement) {
				t.Fatalf("a replayed movement = %v, want ErrDuplicateMovement", err)
			}
			_, _, err = a.ledger.Append(ctx, a.holdEntry(t, bucket.ID, 1000, acctReservation(t)))
			return err
		})
		if err != nil {
			t.Fatalf("the unit of work that lost the collision must commit: %v", err)
		}
		a.assertBalances(t, bucket.ID, 10000, 3000, 7000, 3, 3)
	})

	t.Run("the same settlement leg twice is a duplicate movement", func(t *testing.T) {
		bucket := a.newAccountBucket(t, "it-accounting settlement collision")
		a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, acctCommandKey(t, "seed-")))
		reservation := acctReservation(t)
		a.appendCommitted(t, a.holdEntry(t, bucket.ID, 4000, reservation))
		allocation, err := accounting.NewAllocation(bucket.ID, reservation, acctAmount(t, 4000))
		if err != nil {
			t.Fatalf("allocation: %v", err)
		}
		allocation, err = allocation.SettleConsumed(acctAmount(t, 1000), acctPrice(t))
		if err != nil {
			t.Fatalf("settle consumed: %v", err)
		}
		plan, err := accounting.BuildSettle(acctSettlementID(t), acctRequestID(t),
			[]accounting.Allocation{allocation}, accounting.NewLedgerEntryID, time.Now().UTC())
		if err != nil {
			t.Fatalf("build settle: %v", err)
		}

		// The settlement's own legs land once, behind their header — the
		// settle choreography's shape.
		err = a.store.WithinTx(ctx, func(ctx context.Context) error {
			created, err := a.settlements.Create(ctx, plan.Settlement)
			if err != nil || !created {
				if err == nil {
					err = errors.New("integration: the first acknowledgement must create the header")
				}
				return err
			}
			for _, entry := range plan.Entries {
				if _, _, err := a.ledger.Append(ctx, entry); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("settle: %v", err)
		}

		// The settle left the bucket with nothing held, and the guards speak
		// before the uniqueness does — a fresh hold gives the replayed leg
		// balances the guards admit, so the constraint is what refuses it.
		a.appendCommitted(t, a.holdEntry(t, bucket.ID, 1000, acctReservation(t)))

		// The settlement re-booked — a fresh leg carrying the same settlement
		// id and kind on the same bucket — is the uniqueness constraint's
		// duplicate movement (the original leg's own id would only reach the
		// primary key first), and the unit of work that lost it still
		// commits a leg of its own.
		err = a.store.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := a.ledger.Append(ctx, a.consumeEntry(t, bucket.ID, 1000, plan.Settlement.ID))
			if !errors.Is(err, accounting.ErrDuplicateMovement) {
				t.Fatalf("a replayed settlement leg = %v, want ErrDuplicateMovement", err)
			}
			_, _, err = a.ledger.Append(ctx, a.topupEntry(t, bucket.ID, 500, acctCommandKey(t, "topup-")))
			return err
		})
		if err != nil {
			t.Fatalf("the unit of work that lost the collision must commit: %v", err)
		}
		a.assertBalances(t, bucket.ID, 9500, 1000, 8500, 6, 6)
	})

	t.Run("the same command key on another bucket is no collision", func(t *testing.T) {
		one := a.newAccountBucket(t, "it-accounting scoped key one")
		two := a.newAccountBucket(t, "it-accounting scoped key two")
		key := acctCommandKey(t, "topup-")
		a.appendCommitted(t, a.topupEntry(t, one.ID, 700, key))
		a.appendCommitted(t, a.topupEntry(t, two.ID, 900, key))

		for _, bucket := range []accounting.Bucket{one, two} {
			if _, err := a.ledger.ByBucketAndCommandKey(ctx, bucket.ID, key); err != nil {
				t.Fatalf("bucket %s must carry its own leg under the shared key: %v", bucket.ID, err)
			}
		}
	})
}

// TestIntegrationAccountingSettlementIsExactlyOnceAndItsLegsTellTheTruth
// runs the settle choreography the use case runs — header first, legs of the
// plan behind it, one unit of work — and then races nothing: the second
// acknowledgement of the same request inserts nothing, reads the recorded
// header, and appends no legs of its own.
func TestIntegrationAccountingSettlementIsExactlyOnceAndItsLegsTellTheTruth(t *testing.T) {
	a := integrationAccounting(t)
	ctx := t.Context()
	bucket := a.newAccountBucket(t, "it-accounting settlement")
	key := acctCommandKey(t, "topup-")
	a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, key))
	reservation := acctReservation(t)
	a.appendCommitted(t, a.holdEntry(t, bucket.ID, 4000, reservation))

	allocation, err := accounting.NewAllocation(bucket.ID, reservation, acctAmount(t, 4000))
	if err != nil {
		t.Fatalf("allocation: %v", err)
	}
	allocation, err = allocation.SettleConsumed(acctAmount(t, 2500), acctPrice(t))
	if err != nil {
		t.Fatalf("settle consumed: %v", err)
	}
	requestID := acctRequestID(t)
	plan, err := accounting.BuildSettle(acctSettlementID(t), requestID,
		[]accounting.Allocation{allocation}, accounting.NewLedgerEntryID, time.Now().UTC())
	if err != nil {
		t.Fatalf("build settle: %v", err)
	}

	var created bool
	err = a.store.WithinTx(ctx, func(ctx context.Context) error {
		var err error
		created, err = a.settlements.Create(ctx, plan.Settlement)
		if err != nil || !created {
			if err == nil {
				err = errors.New("integration: the first acknowledgement must create the header")
			}
			return err
		}
		for _, entry := range plan.Entries {
			if _, _, err := a.ledger.Append(ctx, entry); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !created {
		t.Fatal("the first acknowledgement reported false")
	}

	// Consume 2500 vacates its hold; the 1500 tail goes back. Settled pays
	// only what was spent; the header's total is the consume legs' sum.
	a.assertBalances(t, bucket.ID, 7500, 0, 7500, 4, 4)
	tail, err := a.ledger.ByBucketReservationAndKind(ctx, bucket.ID, reservation, accounting.KindRelease)
	if err != nil || tail.SettlementID != plan.Settlement.ID {
		t.Fatalf("the release leg must name the settlement (leg settlement %q, %v)", tail.SettlementID, err)
	}

	// The redelivery: a fresh header id under the same request. The insert
	// is absorbed, the verdict is false, the recorded header is the first
	// one's, and — because the caller only appends behind a true return —
	// no legs move twice.
	created = true
	err = a.store.WithinTx(ctx, func(ctx context.Context) error {
		var err error
		created, err = a.settlements.Create(ctx, accounting.Settlement{
			ID:           acctSettlementID(t),
			RequestID:    requestID,
			SettledTotal: plan.Settlement.SettledTotal,
			CreatedAt:    time.Now().UTC(),
		})
		if err != nil {
			return err
		}
		// What the converge path reads: the recorded header, totals intact.
		recorded, err := a.settlements.ByRequestID(ctx, requestID)
		if err != nil {
			return err
		}
		if recorded.ID != plan.Settlement.ID || recorded.SettledTotal != plan.Settlement.SettledTotal {
			t.Fatalf("the recorded settlement = (%s, total %d), want (%s, %d)",
				recorded.ID, recorded.SettledTotal, plan.Settlement.ID, plan.Settlement.SettledTotal)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("re-acknowledge: %v", err)
	}
	if created {
		t.Fatal("the redelivery must not report that it created the header")
	}
	a.assertBalances(t, bucket.ID, 7500, 0, 7500, 4, 4)

	if _, err := a.settlements.ByRequestID(ctx, acctRequestID(t)); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("an unsettle request = %v, want ErrNotFound", err)
	}
}

// TestIntegrationAccountingAppendRefusesOutsideAUnitOfWork pins the
// contract's shape on the real store: an autocommitted append is refused
// before a statement runs, and the world it declined to touch is unchanged.
func TestIntegrationAccountingAppendRefusesOutsideAUnitOfWork(t *testing.T) {
	a := integrationAccounting(t)
	bucket := a.newAccountBucket(t, "it-accounting outside tx")

	_, _, err := a.ledger.Append(context.Background(), a.grantEntry(t, bucket.ID, 1000))
	if !errors.Is(err, errAppendOutsideUnitOfWork) {
		t.Fatalf("append outside a unit of work = %v, want the refusal sentinel", err)
	}
	a.assertBalances(t, bucket.ID, 0, 0, 0, 0, 0)
}

// ---------------------------------------------------------------------------
// the concurrency the guard exists for.
// ---------------------------------------------------------------------------

// TestIntegrationAccountingTwoConcurrentHoldsCannotBothSucceed is the spec's
// named scenario, played for real: one bucket holding an available 100, two
// goroutines released through one barrier, each appending a hold of 80 in
// its own unit of work against the same row. Exactly one hold lands; the
// loser's echo fires zero rows on the row's post-commit version — READ
// COMMITTED re-evaluates the WHERE under the lock it waited on — and the
// refusal is ErrInsufficientAvailable with the bucket left at 80 held.
func TestIntegrationAccountingTwoConcurrentHoldsCannotBothSucceed(t *testing.T) {
	a := integrationAccounting(t)
	ctx := t.Context()
	bucket := a.newAccountBucket(t, "it-accounting concurrent holds")
	a.appendCommitted(t, a.topupEntry(t, bucket.ID, 10000, acctCommandKey(t, "seed-")))

	const holdTake = int64(8000)
	one, two := acctReservation(t), acctReservation(t)
	verdicts := make([]error, 2)
	landed := make([]accounting.ReservationID, 2)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, reservation := range []accounting.ReservationID{one, two} {
		wg.Add(1)
		go func(i int, reservation accounting.ReservationID) {
			defer wg.Done()
			<-start
			err := a.store.WithinTx(ctx, func(ctx context.Context) error {
				_, _, err := a.ledger.Append(ctx, a.holdEntry(t, bucket.ID, holdTake, reservation))
				return err
			})
			if err == nil {
				landed[i] = reservation
			}
			verdicts[i] = err
		}(i, reservation)
	}
	close(start)
	wg.Wait()

	var winners, losers int
	for i, err := range verdicts {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, accounting.ErrInsufficientAvailable):
			losers++
		default:
			t.Fatalf("goroutine %d met an unexpected verdict: %v", i, err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("one hold must land and one must be refused, got %d winners and %d losers", winners, losers)
	}

	a.assertBalances(t, bucket.ID, 10000, holdTake, 10000-holdTake, 2, 2)
	winner := landed[0]
	if winner == "" {
		winner = landed[1]
	}
	if _, err := a.ledger.ByBucketReservationAndKind(ctx, bucket.ID, winner, accounting.KindHold); err != nil {
		t.Fatalf("the winner's leg must be on file: %v", err)
	}
	loser := one
	if loser == winner {
		loser = two
	}
	if _, err := a.ledger.ByBucketReservationAndKind(ctx, bucket.ID, loser, accounting.KindHold); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("the loser's leg must not be on file: %v", err)
	}

	derived, err := a.projections.DerivedBalances(ctx, bucket.ID)
	if err != nil || derived.Legs != 2 || !derived.ConsistentWith(a.assertBalances(t, bucket.ID, 10000, holdTake, 2000, 2, 2)) {
		t.Fatalf("the derivation (%d legs) must agree with the row the winner left: %v", derived.Legs, err)
	}
}

// TestIntegrationAccountingConcurrentTopupsOnOneCommandKeyLandOneLeg races
// the other writer the convergence bargain names: two goroutines, one
// command key, one bucket. The echo has no guard to lose — funding a bucket
// is always affordable — so the uniqueness constraint is the referee, the
// loser gets ErrDuplicateCommand with its unit of work alive, and exactly
// one leg exists to show for both calls.
func TestIntegrationAccountingConcurrentTopupsOnOneCommandKeyLandOneLeg(t *testing.T) {
	a := integrationAccounting(t)
	ctx := t.Context()
	bucket := a.newAccountBucket(t, "it-accounting concurrent topups")
	key := acctCommandKey(t, "topup-")

	verdicts := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			verdicts[i] = a.store.WithinTx(ctx, func(ctx context.Context) error {
				_, _, err := a.ledger.Append(ctx, a.topupEntry(t, bucket.ID, 5000, key))
				return err
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var collisions int
	for i, err := range verdicts {
		switch {
		case err == nil:
		case errors.Is(err, accounting.ErrDuplicateCommand):
			collisions++
		default:
			t.Fatalf("goroutine %d met an unexpected verdict: %v", i, err)
		}
	}
	if collisions != 1 {
		t.Fatalf("exactly one caller must lose the key, got %d collisions", collisions)
	}

	a.assertBalances(t, bucket.ID, 5000, 0, 5000, 1, 1)
	if _, err := a.ledger.ByBucketAndCommandKey(ctx, bucket.ID, key); err != nil {
		t.Fatalf("the winner's leg must be on file: %v", err)
	}
}

// ---------------------------------------------------------------------------
// the PAYG reference and the cycle bucket's foreign key.
// ---------------------------------------------------------------------------

// TestIntegrationAccountingThePaygReferenceIsWriteOnce walks the reference
// choreography through the port: the first assignment files it — and files it
// only against the row's own account's bucket, the owner guard refusing
// anything else outright — a re-offer of the filed bucket is the no-op that
// reports false, and — the upsert path the enable-flag's row shares — a PAYG
// row that exists without a reference takes one.
func TestIntegrationAccountingThePaygReferenceIsWriteOnce(t *testing.T) {
	a := integrationAccounting(t)
	ctx := t.Context()
	now := time.Now().UTC()

	account := integrationCommerceAccount(t, a.commerce, "it-accounting payg reference")
	bucket := a.newAccountBucketFor(t, account)

	assigned, err := a.payg.AssignFundingBucket(ctx, account, commerce.FundingBucketID(bucket.ID), now)
	if err != nil || !assigned {
		t.Fatalf("assign a reference to a row without one = (%t, %v), want (true, nil)", assigned, err)
	}
	payg, err := a.payg.ByAccount(ctx, account)
	if err != nil || commerce.FundingBucketID(payg.FundingBucketID) != commerce.FundingBucketID(bucket.ID) {
		t.Fatalf("ByAccount = (bucket %q, %v), want the reference filed", payg.FundingBucketID, err)
	}

	// Same bucket again: the statement matches nothing and reports false.
	if assigned, err := a.payg.AssignFundingBucket(ctx, account, commerce.FundingBucketID(bucket.ID), now); err != nil || assigned {
		t.Fatalf("re-assign the same reference = (%t, %v), want (false, nil)", assigned, err)
	}

	// Another account's bucket: the owner guard refuses the call outright —
	// an edge into someone else's money is not a silent no-op — and the
	// reference on file does not move.
	other := a.newAccountBucket(t, "it-accounting payg other bucket")
	if _, err := a.payg.AssignFundingBucket(ctx, account, commerce.FundingBucketID(other.ID), now); err == nil {
		t.Fatalf("assign another account's bucket = (nil error), want the owner guard's refusal")
	}
	if payg, err := a.payg.ByAccount(ctx, account); err != nil || commerce.FundingBucketID(payg.FundingBucketID) != commerce.FundingBucketID(bucket.ID) {
		t.Fatalf("ByAccount = (bucket %q, %v), want the original reference standing", payg.FundingBucketID, err)
	}

	// The enable flag's insert-or-update makes a row that carries no
	// reference; the assignment's UPDATE path is what fills it — with the
	// row's own account's bucket, as ever.
	later := integrationCommerceAccount(t, a.commerce, "it-accounting payg enabled account")
	if err := a.payg.SetEnabled(ctx, later, true, now); err != nil {
		t.Fatalf("enable payg: %v", err)
	}
	enabledBucket := a.newAccountBucketFor(t, later)
	assigned, err = a.payg.AssignFundingBucket(ctx, later, commerce.FundingBucketID(enabledBucket.ID), now)
	if err != nil || !assigned {
		t.Fatalf("assign a reference to an enabled row = (%t, %v), want (true, nil)", assigned, err)
	}
}

// TestIntegrationAccountingCycleBucketFundsARowThePortsBuilt proves the
// cycle bucket's foreign key both ways through the ports: a bucket over a
// real entitlement row opens and funds, and a bucket naming an entitlement
// that was never granted is refused by the schema, with the constraint's
// own name to show for it.
func TestIntegrationAccountingCycleBucketFundsARowThePortsBuilt(t *testing.T) {
	a := integrationAccounting(t)
	bucket := a.newEntitlementBucket(t, "it-accounting cycle bucket")
	a.appendCommitted(t, a.grantEntry(t, bucket.ID, 5000))
	a.assertBalances(t, bucket.ID, 5000, 0, 5000, 1, 1)

	ghost, err := commerce.NewEntitlementID()
	if err != nil {
		t.Fatalf("mint a ghost entitlement reference: %v", err)
	}
	unfunded, err := accounting.NewEntitlementBucket(acctBucketID(t), accounting.EntitlementID(ghost), time.Now().UTC())
	if err != nil {
		t.Fatalf("new ghost bucket: %v", err)
	}
	err = a.buckets.Create(t.Context(), unfunded)
	if err == nil || !strings.Contains(err.Error(), "funding_buckets_entitlement_id_fkey") {
		t.Fatalf("a bucket over an ungranted entitlement = %v, want the entitlement foreign key's refusal", err)
	}
}
