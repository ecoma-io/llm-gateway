package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// ErrUnitOfWorkRequired reports a call that may only run inside a unit of
// work arriving without one. It is the refusal, not a degradation: the
// callers that join someone else's transaction (the cycle roll's funding)
// would silently autocommit on a bare context, and a half of someone else's
// fact is worse than no fact at all.
var ErrUnitOfWorkRequired = errors.New("application: the operation is unit-of-work-shaped and ctx carries no unit of work")

// Accounting is the accounting foundation's use cases: the Control Plane's
// money authority (ADR 0004). Every method here is a composition of one
// domain fact set and one unit of work — a bucket never moves without the
// leg that names the move, a settlement never lands without its legs, and
// the ledger's word outranks the cached balances everywhere the two can
// disagree.
//
// The primitives are deliberately narrower than the flows that will one day
// call them: holds are booked from reservations as facts (B12's applier),
// settlements from usage acknowledgements, topups from the payment provider
// (B15) or an operator. None of those callers exist yet, and none is stubbed
// here — B6 establishes the primitives, the guarded statements and the
// invariants they enforce, exactly as the identity and commerce foundations
// did before their consumers arrived.
type Accounting struct {
	store       persistence.Store
	buckets     persistence.FundingBuckets
	ledger      persistence.FundingLedger
	settlements persistence.Settlements
	projections persistence.FundingProjections
	payg        persistence.PaygAccounts
	clock       persistence.Clock
}

// NewAccounting builds the accounting use cases around the ports they need.
// It panics on a nil port for the reason NewCommerce panics: a port this use
// case was promised and did not get is a wiring defect, and the middle of a
// settlement — after the header insert and half the legs — is a strictly
// worse place to learn about it.
//
// The payg repository is here for the account-creation choreography: opening
// an account's bucket and filing its write-once reference on the PAYG row is
// one fact, and the two ports it spans share this plane's transaction
// boundary.
func NewAccounting(
	store persistence.Store,
	buckets persistence.FundingBuckets,
	ledger persistence.FundingLedger,
	settlements persistence.Settlements,
	projections persistence.FundingProjections,
	payg persistence.PaygAccounts,
	clock persistence.Clock,
) *Accounting {
	switch {
	case store == nil:
		panic("application: NewAccounting requires a store")
	case buckets == nil:
		panic("application: NewAccounting requires a funding buckets repository")
	case ledger == nil:
		panic("application: NewAccounting requires a funding ledger repository")
	case settlements == nil:
		panic("application: NewAccounting requires a settlements repository")
	case projections == nil:
		panic("application: NewAccounting requires a funding projections repository")
	case payg == nil:
		panic("application: NewAccounting requires a payg accounts repository")
	case clock == nil:
		panic("application: NewAccounting requires a clock")
	}
	return &Accounting{
		store:       store,
		buckets:     buckets,
		ledger:      ledger,
		settlements: settlements,
		projections: projections,
		payg:        payg,
		clock:       clock,
	}
}

// FundEntitlement opens the cycle bucket behind one just-materialised
// entitlement and writes its grant leg — the roll's accounting half, and the
// one method here that belongs to somebody else's transaction. It refuses to
// run outside a unit of work (ErrUnitOfWorkRequired) rather than degrade:
// called with the roll's context it joins that transaction, so the
// entitlement row, its bucket and the grant leg commit or roll back as one
// fact — the bucket's foreign key to the entitlement is the structural
// guarantee that it cannot land anywhere else.
//
// A bucket already on file for the entitlement is convergence, not an error:
// a retried roll finds its work done and moves on. The granted amount is the
// definition's, taken as minor units; the money constructors refuse anything
// that is not a strictly positive integer, so a grantless definition fails
// the roll loudly instead of funding silence.
func (a *Accounting) FundEntitlement(ctx context.Context, entitlementID commerce.EntitlementID, grantedAmount int64) error {
	if !a.store.InUnitOfWork(ctx) {
		return fmt.Errorf("application: fund entitlement %s: %w", entitlementID, ErrUnitOfWorkRequired)
	}
	amount, err := accounting.NewAmount(grantedAmount)
	if err != nil {
		return fmt.Errorf("application: fund entitlement %s: %w", entitlementID, err)
	}
	id, err := accounting.NewFundingBucketID()
	if err != nil {
		return fmt.Errorf("application: fund entitlement %s: %w", entitlementID, err)
	}
	if _, err := a.buckets.ByEntitlementID(ctx, accounting.EntitlementID(entitlementID)); err == nil {
		return nil // the roll retried; this cycle's bucket is already on file
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return fmt.Errorf("application: fund entitlement %s: read bucket: %w", entitlementID, err)
	}
	now, err := dbNow(ctx, a.clock, "fund entitlement")
	if err != nil {
		return err
	}
	bucket, err := accounting.NewEntitlementBucket(id, accounting.EntitlementID(entitlementID), now)
	if err != nil {
		return fmt.Errorf("application: fund entitlement %s: %w", entitlementID, err)
	}
	if err := a.buckets.Create(ctx, bucket); err != nil {
		return fmt.Errorf("application: fund entitlement %s: create bucket: %w", entitlementID, err)
	}
	entryID, err := accounting.NewLedgerEntryID()
	if err != nil {
		return fmt.Errorf("application: fund entitlement %s: %w", entitlementID, err)
	}
	entry, err := accounting.NewGrantEntry(entryID, bucket.ID, amount, now)
	if err != nil {
		return fmt.Errorf("application: fund entitlement %s: %w", entitlementID, err)
	}
	if _, _, err := a.ledger.Append(ctx, entry); err != nil {
		return fmt.Errorf("application: fund entitlement %s: append grant: %w", entitlementID, err)
	}
	return nil
}

// OpenAccountFunding opens an account's PAYG bucket and files its reference
// on the account's PAYG row, in one unit of work — the account-creation
// choreography's accounting half. The reference is write-once (one PAYG
// source and one bucket per account, ever), so a bucket already on file for
// the account is convergence, and a row already pointing elsewhere is the
// defect the write-once guard exists to stop, named rather than absorbed.
func (a *Accounting) OpenAccountFunding(ctx context.Context, accountID commerce.AccountID) (accounting.Bucket, error) {
	var bucket *accounting.Bucket
	err := a.store.WithinTx(ctx, func(txCtx context.Context) error {
		if existing, err := a.buckets.ByAccountID(txCtx, accounting.AccountID(accountID)); err == nil {
			bucket = &existing
			return nil // the account's bucket is already on file; converged
		} else if !errors.Is(err, persistence.ErrNotFound) {
			return fmt.Errorf("application: open account funding: read bucket: %w", err)
		}
		now, err := dbNow(txCtx, a.clock, "open account funding")
		if err != nil {
			return err
		}
		id, err := accounting.NewFundingBucketID()
		if err != nil {
			return fmt.Errorf("application: open account funding: %w", err)
		}
		created, err := accounting.NewAccountBucket(id, accounting.AccountID(accountID), now)
		if err != nil {
			return fmt.Errorf("application: open account funding: %w", err)
		}
		if err := a.buckets.Create(txCtx, created); err != nil {
			return fmt.Errorf("application: open account funding: create bucket: %w", err)
		}
		applied, err := a.payg.AssignFundingBucket(txCtx, accountID,
			commerce.FundingBucketID(created.ID), now)
		if err != nil {
			return fmt.Errorf("application: open account funding: assign reference: %w", err)
		}
		if !applied {
			// This unit of work is the call's own, so only a concurrent
			// assignment lands here; the rollback below erases the fresh
			// bucket, and the re-read names what did land.
			onFile, err := a.payg.ByAccount(txCtx, accountID)
			if err != nil {
				return fmt.Errorf("application: open account funding: re-read reference: %w", err)
			}
			if onFile.FundingBucketID == commerce.FundingBucketID(created.ID) {
				bucket = &created
				return nil
			}
			return fmt.Errorf("application: open account funding for account %s: %w: bucket %s is already on file",
				accountID, accounting.ErrInvalidTransition, onFile.FundingBucketID)
		}
		bucket = &created
		return nil
	})
	if err != nil {
		return accounting.Bucket{}, fmt.Errorf("application: open account funding for account %s: %w", accountID, err)
	}
	return *bucket, nil
}

// TopUp adds funded money to a PAYG bucket, keyed by the caller's command
// key: the same key re-run with the same amount converges on the original
// leg and changes nothing; the same key with a different amount is the
// contract defect ErrDuplicateCommand names. Topup is a PAYG movement by
// definition — a grant is how an entitlement cycle's bucket is funded, and
// an operator's correction is an adjustment — so a topup naming a cycle
// bucket is refused as the transition it is not.
func (a *Accounting) TopUp(ctx context.Context, bucketID accounting.FundingBucketID, amountMinorUnits int64, commandKey accounting.CommandKey) (accounting.Bucket, error) {
	amount, err := accounting.NewAmount(amountMinorUnits)
	if err != nil {
		return accounting.Bucket{}, fmt.Errorf("application: top up bucket %s: %w", bucketID, err)
	}
	return a.appendMovement(ctx, bucketID, "top up bucket "+string(bucketID), accounting.ErrDuplicateCommand,
		func(txCtx context.Context, now time.Time) (accounting.Bucket, accounting.LedgerEntry, error) {
			return a.newLeg(txCtx, bucketID, now, func(id accounting.LedgerEntryID) (accounting.LedgerEntry, error) {
				return accounting.NewTopupEntry(id, bucketID, amount, commandKey, now)
			})
		},
		func(txCtx context.Context) (accounting.LedgerEntry, error) {
			return a.ledger.ByBucketAndCommandKey(txCtx, bucketID, commandKey)
		},
		func(original accounting.LedgerEntry) bool {
			return original.Kind == accounting.KindTopup && original.Amount == amount
		})
}

// Hold books a reservation's ceiling against a bucket: held up, available
// down, the reservation named so a redelivery converges or is named. A hold
// that outruns the bucket's available balance is refused —
// ErrInsufficientAvailable — and that refusal is the verdict of a lost
// admission race as much as of an empty bucket; nothing here queues, waits
// or overcommits.
func (a *Accounting) Hold(ctx context.Context, bucketID accounting.FundingBucketID, reservationID accounting.ReservationID, amountMinorUnits int64) (accounting.Bucket, error) {
	amount, err := accounting.NewAmount(amountMinorUnits)
	if err != nil {
		return accounting.Bucket{}, fmt.Errorf("application: hold on bucket %s: %w", bucketID, err)
	}
	return a.appendMovement(ctx, bucketID, "hold on bucket "+string(bucketID), accounting.ErrDuplicateMovement,
		func(txCtx context.Context, now time.Time) (accounting.Bucket, accounting.LedgerEntry, error) {
			return a.newLeg(txCtx, bucketID, now, func(id accounting.LedgerEntryID) (accounting.LedgerEntry, error) {
				return accounting.NewHoldEntry(id, bucketID, amount, reservationID, now)
			})
		},
		func(txCtx context.Context) (accounting.LedgerEntry, error) {
			return a.ledger.ByBucketReservationAndKind(txCtx, bucketID, reservationID, accounting.KindHold)
		},
		func(original accounting.LedgerEntry) bool {
			return original.Amount == amount
		})
}

// ReleaseHold returns a reservation's held capacity without a settlement
// behind it — a compensation with no candidate to serve, or a reaper's
// expiry booking. The reservation is named, so the same release re-run
// converges, and a release larger than the bucket's remaining hold is
// refused: returning more than was taken would conjure money out of the
// ledger. A settlement's unconsumed tails are released by Settle, not here.
func (a *Accounting) ReleaseHold(ctx context.Context, bucketID accounting.FundingBucketID, reservationID accounting.ReservationID, amountMinorUnits int64) (accounting.Bucket, error) {
	amount, err := accounting.NewAmount(amountMinorUnits)
	if err != nil {
		return accounting.Bucket{}, fmt.Errorf("application: release hold on bucket %s: %w", bucketID, err)
	}
	return a.appendMovement(ctx, bucketID, "release hold on bucket "+string(bucketID), accounting.ErrDuplicateMovement,
		func(txCtx context.Context, now time.Time) (accounting.Bucket, accounting.LedgerEntry, error) {
			return a.newLeg(txCtx, bucketID, now, func(id accounting.LedgerEntryID) (accounting.LedgerEntry, error) {
				return accounting.NewReleaseEntry(id, bucketID, amount, reservationID, "", now)
			})
		},
		func(txCtx context.Context) (accounting.LedgerEntry, error) {
			return a.ledger.ByBucketReservationAndKind(txCtx, bucketID, reservationID, accounting.KindRelease)
		},
		func(original accounting.LedgerEntry) bool {
			return original.Amount == amount
		})
}

// Adjust writes an operator correction: exactly one stated non-zero delta,
// the reason, the entry it corrects, the operator who authorised it. It is
// the ledger's only correction path, and an adjustment that does not fit the
// balance it lands on — held, settled or available driven below zero — is
// refused rather than clipped: corrections fix records, they never overdraw
// or credit an account. The command key is optional; with one, the
// convergence bargain applies, keyed by command key like a topup.
func (a *Accounting) Adjust(ctx context.Context, bucketID accounting.FundingBucketID,
	settledDeltaMinorUnits, heldDeltaMinorUnits int64, reason string,
	originalEntryID accounting.LedgerEntryID, operatorID accounting.OperatorID, commandKey accounting.CommandKey,
) (accounting.Bucket, error) {
	settledDelta, heldDelta := accounting.Delta(settledDeltaMinorUnits), accounting.Delta(heldDeltaMinorUnits)
	action := fmt.Sprintf("adjust bucket %s (settled %+d, held %+d)", bucketID, settledDeltaMinorUnits, heldDeltaMinorUnits)
	return a.appendMovement(ctx, bucketID, action, accounting.ErrDuplicateCommand,
		func(txCtx context.Context, now time.Time) (accounting.Bucket, accounting.LedgerEntry, error) {
			return a.newLeg(txCtx, bucketID, now, func(id accounting.LedgerEntryID) (accounting.LedgerEntry, error) {
				return accounting.NewAdjustmentEntry(id, bucketID, settledDelta, heldDelta, reason, originalEntryID, operatorID, commandKey, now)
			})
		},
		func(txCtx context.Context) (accounting.LedgerEntry, error) {
			if commandKey == "" {
				return accounting.LedgerEntry{}, persistence.ErrNotFound
			}
			return a.ledger.ByBucketAndCommandKey(txCtx, bucketID, commandKey)
		},
		func(original accounting.LedgerEntry) bool {
			return original.Kind == accounting.KindAdjustment &&
				original.SettledDelta == settledDelta && original.HeldDelta == heldDelta
		})
}

// SettlementResult reports one Settle call's outcome. Converged marks the
// re-acknowledgement path: the request was already settled with the same
// total, nothing moved again, and the recorded settlement is the answer.
type SettlementResult struct {
	Settlement accounting.Settlement
	Converged  bool
}

// Settle closes one Data Plane request's financial story: the settlement
// header keyed exactly-once by the request id, and per allocation a consume
// leg (both balances down by what was spent, price snapshot by value) plus a
// release leg for the unconsumed tail, in the caller's waterfall order, all
// in one unit of work. A header without its legs cannot commit and legs that
// lose their guards leave no header — the atomicity the ledger's word
// depends on.
//
// An already-settled request is the exactly-once bargain: the recorded
// settlement's total is the one total that request can have. The same total
// converges; a different one is ErrSettlementConflict — the same request
// cannot settle twice at two different totals, and the disagreement is a
// defect to name, not a charge to make.
func (a *Accounting) Settle(ctx context.Context, requestID accounting.RequestID, allocations []accounting.Allocation) (SettlementResult, error) {
	settlementID, err := accounting.NewSettlementID()
	if err != nil {
		return SettlementResult{}, fmt.Errorf("application: settle request %s: %w", requestID, err)
	}
	var result SettlementResult
	err = a.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := dbNow(txCtx, a.clock, "settle request")
		if err != nil {
			return err
		}
		plan, err := accounting.BuildSettle(settlementID, requestID, allocations, accounting.NewLedgerEntryID, now)
		if err != nil {
			return fmt.Errorf("application: settle request %s: %w", requestID, err)
		}
		created, err := a.settlements.Create(txCtx, plan.Settlement)
		if err != nil {
			return fmt.Errorf("application: settle request %s: %w", requestID, err)
		}
		if !created {
			// Another acknowledgement of the same request won the header. The
			// legs are appended only by the call the header belongs to, so
			// what is on file is whole; this caller compares totals and
			// converges or names the conflict — nothing of its own moved.
			recorded, err := a.settlements.ByRequestID(txCtx, requestID)
			if err != nil {
				return fmt.Errorf("application: settle request %s: read recorded settlement: %w", requestID, err)
			}
			if recorded.SettledTotal != plan.Settlement.SettledTotal {
				return fmt.Errorf("application: settle request %s: %w: recorded total %d, planned total %d",
					requestID, accounting.ErrSettlementConflict, recorded.SettledTotal, plan.Settlement.SettledTotal)
			}
			result = SettlementResult{Settlement: recorded, Converged: true}
			return nil
		}
		for _, entry := range plan.Entries {
			if _, _, err := a.ledger.Append(txCtx, entry); err != nil {
				return fmt.Errorf("application: settle request %s: append %s leg on bucket %s: %w",
					requestID, entry.Kind, entry.FundingBucketID, err)
			}
		}
		result = SettlementResult{Settlement: plan.Settlement}
		return nil
	})
	if err != nil {
		return SettlementResult{}, fmt.Errorf("application: settle request %s: %w", requestID, err)
	}
	return result, nil
}

// CloseBucket retires a bucket administratively: no further legs of any
// kind, history complete. A bucket with funds still held refuses — the holds
// are ceilings that must settle or release first — and a close that races a
// leg is retried from the fresh row until the leg's echo says the version
// moved again. Closing a closed bucket is a converged no-op.
func (a *Accounting) CloseBucket(ctx context.Context, bucketID accounting.FundingBucketID) error {
	return a.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := dbNow(txCtx, a.clock, "close bucket")
		if err != nil {
			return err
		}
		for attempt := 0; attempt < casMaxAttempts; attempt++ {
			bucket, err := a.buckets.ByID(txCtx, bucketID)
			if err != nil {
				return fmt.Errorf("application: close bucket %s: %w", bucketID, err)
			}
			before := bucket.Status
			closed, err := bucket.Close(now)
			if err != nil {
				return fmt.Errorf("application: close bucket %s: %w", bucketID, err)
			}
			if closed.Status == before {
				return nil // the domain ruled this a no-op; nothing to swap
			}
			applied, err := a.buckets.Close(txCtx, bucketID, closed.Version, now)
			if err != nil {
				return fmt.Errorf("application: close bucket %s: %w", bucketID, err)
			}
			if applied {
				return nil
			}
			// Lost the swap: a leg landed between the read and the write. The
			// next attempt re-reads — and if that leg was a hold, the
			// domain's refusal above is the answer the caller keeps.
		}
		return fmt.Errorf("application: close bucket %s: %w after %d attempts", bucketID, ErrTransitionContended, casMaxAttempts)
	})
}

// Bucket returns the bucket with id — the cached projection, plus the
// version and sequence the next guarded write will judge itself against.
func (a *Accounting) Bucket(ctx context.Context, bucketID accounting.FundingBucketID) (accounting.Bucket, error) {
	bucket, err := a.buckets.ByID(ctx, bucketID)
	if err != nil {
		return accounting.Bucket{}, fmt.Errorf("application: read bucket %s: %w", bucketID, err)
	}
	return bucket, nil
}

// BucketByEntitlement returns the cycle bucket funding the entitlement.
func (a *Accounting) BucketByEntitlement(ctx context.Context, entitlementID accounting.EntitlementID) (accounting.Bucket, error) {
	bucket, err := a.buckets.ByEntitlementID(ctx, entitlementID)
	if err != nil {
		return accounting.Bucket{}, fmt.Errorf("application: read bucket of entitlement %s: %w", entitlementID, err)
	}
	return bucket, nil
}

// BucketByAccount returns the PAYG bucket funding the account.
func (a *Accounting) BucketByAccount(ctx context.Context, accountID commerce.AccountID) (accounting.Bucket, error) {
	bucket, err := a.buckets.ByAccountID(ctx, accounting.AccountID(accountID))
	if err != nil {
		return accounting.Bucket{}, fmt.Errorf("application: read bucket of account %s: %w", accountID, err)
	}
	return bucket, nil
}

// ReconcileReport is one ReconcileBucket call's verdict: the cached
// projection, the legs' own derivation of the same balances, and whether the
// two agree.
type ReconcileReport struct {
	Bucket     accounting.Bucket
	Derivation accounting.Derivation
	Consistent bool
}

// ReconcileBucket compares a bucket's cached balances against what its legs
// alone derive. The cache exists so reads do not aggregate the ledger on
// every hold; this is the check that keeps it honest, and when it fails the
// legs win — the derivation names the truth a correction is computed from,
// the caller decides whether the remedy is an adjustment or an investigation.
func (a *Accounting) ReconcileBucket(ctx context.Context, bucketID accounting.FundingBucketID) (ReconcileReport, error) {
	bucket, err := a.buckets.ByID(ctx, bucketID)
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("application: reconcile bucket %s: %w", bucketID, err)
	}
	derivation, err := a.projections.DerivedBalances(ctx, bucketID)
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("application: reconcile bucket %s: %w", bucketID, err)
	}
	return ReconcileReport{
		Bucket:     bucket,
		Derivation: derivation,
		Consistent: derivation.ConsistentWith(bucket),
	}, nil
}

// newLeg reads the bucket, builds the entry through the domain constructor,
// and pre-applies it — the domain's refusal in the domain's words before any
// statement runs, on the read this call's own transaction sees. The
// statement's guard is still the verdict: a bucket that moves between this
// pre-apply and the append is refused by the echo, classified from a fresh
// read, and arrives as the same sentinel the pre-apply would have raised.
func (a *Accounting) newLeg(ctx context.Context, bucketID accounting.FundingBucketID, now time.Time,
	build func(accounting.LedgerEntryID) (accounting.LedgerEntry, error),
) (accounting.Bucket, accounting.LedgerEntry, error) {
	bucket, err := a.buckets.ByID(ctx, bucketID)
	if err != nil {
		return accounting.Bucket{}, accounting.LedgerEntry{}, fmt.Errorf("read bucket: %w", err)
	}
	entryID, err := accounting.NewLedgerEntryID()
	if err != nil {
		return accounting.Bucket{}, accounting.LedgerEntry{}, err
	}
	entry, err := build(entryID)
	if err != nil {
		return accounting.Bucket{}, accounting.LedgerEntry{}, err
	}
	if _, err := entry.ApplyTo(bucket); err != nil {
		return accounting.Bucket{}, accounting.LedgerEntry{}, err
	}
	return bucket, entry, nil
}

// appendMovement is the one flow every single-bucket movement shares, in one
// unit of work, and its order is the redelivery bargain: the convergence
// question comes FIRST, because an original leg on file with the same payload
// means the command's effect is already in the world — re-applying it would
// double the move and the balance guard would rightly refuse it. Only a
// command with no original behind it is built, pre-applied and appended; and
// when the append itself loses to a constraint — the pre-read having raced
// another redelivery — the port's savepoint has kept this unit of work alive,
// so the re-read decides converge versus defect. duplicateErr is the sentinel
// a mismatched original names: ErrDuplicateCommand for keyed commands,
// ErrDuplicateMovement for reservation movements.
func (a *Accounting) appendMovement(
	ctx context.Context,
	bucketID accounting.FundingBucketID,
	action string,
	duplicateErr error,
	buildLeg func(txCtx context.Context, now time.Time) (accounting.Bucket, accounting.LedgerEntry, error),
	findOriginal func(txCtx context.Context) (accounting.LedgerEntry, error),
	samePayload func(original accounting.LedgerEntry) bool,
) (accounting.Bucket, error) {
	var bucket *accounting.Bucket
	err := a.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := dbNow(txCtx, a.clock, action)
		if err != nil {
			return err
		}

		if original, err := findOriginal(txCtx); err == nil {
			if !samePayload(original) {
				return fmt.Errorf("%w: an original leg with a different payload is on file", duplicateErr)
			}
			// Converged on the original leg. The bucket is read fresh so the
			// answer carries the original's effect — on this path the read
			// also postdates the guard, so what it returns is what the next
			// writer sees.
			converged, err := a.buckets.ByID(txCtx, bucketID)
			if err != nil {
				return fmt.Errorf("read the converged bucket: %w", err)
			}
			bucket = &converged
			return nil
		} else if !errors.Is(err, persistence.ErrNotFound) {
			return fmt.Errorf("look for the original leg: %w", err)
		}

		_, entry, err := buildLeg(txCtx, now)
		if err != nil {
			return err
		}

		_, after, err := a.ledger.Append(txCtx, entry)
		if err != nil {
			if errors.Is(err, accounting.ErrDuplicateCommand) || errors.Is(err, accounting.ErrDuplicateMovement) {
				// The pre-read raced a redelivery of the same command or
				// movement, and the winner's leg is on file. The re-read
				// decides converge versus defect, and the converged answer
				// is the bucket as the winner's echo left it.
				original, lookupErr := findOriginal(txCtx)
				if lookupErr != nil {
					return fmt.Errorf("re-read the original leg after a collision: %w", lookupErr)
				}
				if !samePayload(original) {
					return err
				}
				winner, err := a.buckets.ByID(txCtx, bucketID)
				if err != nil {
					return fmt.Errorf("read the bucket the winner left: %w", err)
				}
				bucket = &winner
				return nil
			}
			return err
		}
		bucket = &after
		return nil
	})
	if err != nil {
		return accounting.Bucket{}, fmt.Errorf("application: %s: %w", action, err)
	}
	return *bucket, nil
}
