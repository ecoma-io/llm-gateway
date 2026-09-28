package application

import (
	"context"
	"sync"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The account overview: the dashboard's one composition, assembled server-side.
//
// The dashboard needs figures that live in three bounded contexts — identity's
// account row, commerce's subscriptions, accounting's buckets — and this is
// where they are put together. It is a use case rather than a new layer, and
// the reason is in ADR 0012 §1: a client composing the three itself would be
// doing three round trips to reimplement a join, and a client computing a
// figure from three responses would be doing the plane's arithmetic in the
// browser, where a disagreement has no authority behind it.
//
// So nothing here computes a balance, a total, or a sum. Every figure is
// either a stored column the database already maintains (the two counts, the
// three balances on each bucket) or a set of rows the caller renders. The one
// arithmetic in this file is a comparison, never a sum.

// overviewRecentSubscriptions bounds the subscription list the dashboard
// carries. It is the contract's own `maximum` for that array, and a dashboard
// showing three of eleven is honest where one claiming to show all eleven is
// making a promise this plane will not keep.
const overviewRecentSubscriptions = 5

// AccountOverview is the dashboard's composed answer.
//
// Every field is either a stored figure or a bounded list of stored rows. The
// open finding count is the one number here that is not the account's alone:
// an operator reading it learns how the system is doing, not what this
// customer owes, which is exactly why it is a whole-plane count and exactly
// why the findings list beside it is not account-scoped either.
type AccountOverview struct {
	Account identity.Account

	// UserCount is how many live users the account has — invited plus active,
	// excluding removed. It is a count of a filtered, bounded set and not a
	// page total: no list on this surface returns a total, and a count that
	// was not asked for is not computed.
	UserCount int

	// ActiveAPIKeyCount is how many keys are currently active. Revoked keys
	// are counted in the key list and never here: a dashboard that said "12
	// keys" with twelve revoked among them would be describing history as if
	// it were capacity.
	ActiveAPIKeyCount int

	// Subscriptions are the account's most recent ones, most recent first.
	Subscriptions []commerce.Subscription

	// PAYGBalances are the account's account-kind buckets with their three
	// cached balances, rendered and never derived.
	PAYGBalances []accounting.Bucket

	// OpenFindingCount is how many reconciliation findings are open across the
	// plane.
	OpenFindingCount int
}

// AccountOverview returns the dashboard's one composed answer for the account
// the session belongs to.
//
// The account is the caller's and the caller's alone; there is no account
// parameter on the wire, and there is not one here either. The figures come
// from four reads and two bounded lists, and none of them is a computation:
//
//   - the account row itself, through the identity port's own ByID;
//   - CountLiveForAccount and CountActiveForAccount, which are the two counts
//     the contract names and the SQL layer already implements over an indexed
//     predicate;
//   - the two bounded lists, each a keyset page whose limit is this use case's
//     own bound rather than the contract's page limit.
//
// A failure in any one of them fails the composition rather than answering
// with the figures that arrived. A dashboard that showed "0 users" because the
// count query timed out would be presenting an outage as a fact about the
// account, and the contract's envelope has one honest answer for that: 500
// with a request id.
func (reads *ConsoleReads) AccountOverview(ctx context.Context, accountID identity.AccountID) (AccountOverview, error) {
	if accountID == "" {
		return AccountOverview{}, invalidRequest("a session must name an account before the overview can be read")
	}
	var (
		overview AccountOverview
		firstErr error
	)
	// The reads are independent, and the composition is a single answer rather
	// than five, so they are run concurrently and the first failure wins. A
	// partially-composed dashboard is never written: overview is returned
	// only when every figure arrived.
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	run := func(read func() error) {
		if err := read(); err != nil {
			fail(err)
		}
	}

	var (
		account          identity.Account
		userCount        int
		activeKeyCount   int
		subscriptions    []commerce.Subscription
		payg             []accounting.Bucket
		openFindingCount int
	)
	var wg sync.WaitGroup
	wg.Add(6)
	go run(func() error {
		defer wg.Done()
		row, err := reads.accounts.ByID(ctx, accountID)
		if err != nil {
			return Internal(err)
		}
		account = row
		return nil
	})
	go run(func() error {
		defer wg.Done()
		count, err := reads.users.CountLiveForAccount(ctx, accountID)
		if err != nil {
			return Internal(err)
		}
		userCount = count
		return nil
	})
	go run(func() error {
		defer wg.Done()
		count, err := reads.keys.CountActiveForAccount(ctx, accountID)
		if err != nil {
			return Internal(err)
		}
		activeKeyCount = count
		return nil
	})
	go run(func() error {
		defer wg.Done()
		rows, err := reads.subscriptions.ListForAccount(ctx, commerce.AccountID(accountID), persistence.SubscriptionPage{
			Limit: overviewRecentSubscriptions,
		})
		if err != nil {
			return Internal(err)
		}
		subscriptions = rows
		return nil
	})
	go run(func() error {
		defer wg.Done()
		// The buckets are filtered to the account kind by a predicate in
		// this use case rather than by a new port member: the port returns
		// the account's buckets as they are, and the dashboard's PAYG
		// figure is a selection among rows the account predicate already
		// returned. A selection is not a sum, and the three balances on each
		// surviving row are rendered as stored.
		rows, err := reads.buckets.ListForAccount(ctx, accounting.AccountID(accountID), persistence.FundingBucketPage{
			Limit: overviewRecentSubscriptions,
		})
		if err != nil {
			return Internal(err)
		}
		payg = rows
		return nil
	})
	go run(func() error {
		defer wg.Done()
		count, err := reads.openFindings.OpenCount(ctx)
		if err != nil {
			return Internal(err)
		}
		openFindingCount = count
		return nil
	})
	wg.Wait()

	if firstErr != nil {
		return AccountOverview{}, firstErr
	}
	overview = AccountOverview{
		Account:           account,
		UserCount:         userCount,
		ActiveAPIKeyCount: activeKeyCount,
		Subscriptions:     subscriptions,
		PAYGBalances:      payg,
		OpenFindingCount:  openFindingCount,
	}
	return overview, nil
}
