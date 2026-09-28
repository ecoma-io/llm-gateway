//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The console's two accounting READS, against the real `control` database.
//
// These are the two repositories whose account predicate resolves an owner the
// way the commerce list already resolves one (consoleread_commerce.go's
// correlated EXISTS through control.subscriptions) rather than the way a
// column lookup would, and they are the only two reads in this layer whose
// answer for a paying customer depends on that resolution. The rest of the
// accounting suite exercises the ledger's ALGEBRA — guarded moves, collisions,
// settlement exactly-once — and all of it is done through account buckets,
// because that is the fixture the PAYG reference choreography needs. Nothing
// there ever asks what an entitlement bucket's money looks like to the account
// that paid for it, and the console's reads are where that question is asked.
//
// Why this file exists at all: a bucket's owner is decided by the schema's
// owner-xor CHECK and a row has exactly one of entitlement_id and account_id.
// A reader that filters on funding_buckets.account_id therefore sees the
// account-kind bucket and is structurally blind to the entitlement-kind one —
// not "usually misses it", BLIND: the predicate cannot be satisfied by any
// entitlement bucket the schema will ever accept, because the same constraint
// that makes the bucket well-formed forbids it from carrying an account id.
// The failure is silent in the worst way available. A list returns a
// well-formed empty page, and a ledger read returns a well-formed empty 200
// page — byte-identical to "this bucket has no legs" and to "this bucket is not
// yours". An operator who subscribes to a paid plan and then opens the
// accounting screen concludes their money moved nowhere, and the page gives
// them no way to tell that from the truth.
//
// So the cases below are about VISIBILITY, and the assertions are written as
// presence-and-absence over the whole plane rather than as a count. A count
// would be satisfied by a filter that traded one bucket for another; a named
// bucket that must appear and a named bucket that must not is not.
//
// Conventions, as in every integration file in this package: no t.Parallel (the
// database is shared); every id is run-unique, minted by the domain's own
// minters; nothing deletes — the schema has no delete path and the ids carry
// the isolation; and every fixture reaches its state through the ports, because
// a fixture the port itself built is one more proof that the states compose.
//
// Run (the compose project is port-shifted per worktree):
//
//	GATEWAY_POSTGRES_PROJECT=... GATEWAY_POSTGRES_PORT=... docker compose -f deploy/postgres/compose.yaml up -d --wait
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:PORT/postgres?sslmode=disable' \
//	  go test -tags=integration ./internal/adapters/outbound/postgres

// beforeEverything is the keyset position meaning "the beginning of the
// table": the all-zeros uuid, which sorts before every minted v7 id.
//
// It stands in for the zero value of FundingBucketPage.After throughout this
// file, and the reason is a defect in these statements' own family rather than
// anything about ownership. The zero value reaches the database as the empty
// string, and `id > $2` against a `uuid` column is a cast of that string to
// uuid — which raises SQLSTATE 22P02 before a single row is examined, so the
// first page of every uuid-keyed console list is a 500 rather than the page it
// documents itself as being. consoleread_identity.go states the opposite in
// prose ("the zero-`after` case is `after > $2` against the uuid nil, which no
// real uuid compares greater than"), and that sentence is the second instance
// of the failure this whole fix is about: the intent was written down
// correctly, and the code does something else.
//
// A test for the OWNER PREDICATE must not be the test that silently carries
// the fix for that one. So the position is explicit here, and the two are
// separate changes. The value is typed as the domain's own id so that a
// reviewer reading the page cannot mistake it for a raw database literal.
const beforeEverything = accounting.FundingBucketID("00000000-0000-0000-0000-000000000000")

// consoleAccountingRepos adds the two console read repositories to the
// accounting fixture, over the same pool. They are separate ports from
// FundingBuckets and FundingLedger for the reason the port package states —
// the ownership file is being extended in parallel — so they are wired the way
// main wires them rather than borrowed from the writer's fixtures.
type consoleAccountingRepos struct {
	*accountingRepos
	consoleBuckets persistence.AccountBuckets
	consoleLedger  persistence.AccountLedger
}

func integrationConsoleAccounting(t *testing.T) *consoleAccountingRepos {
	t.Helper()
	a := integrationAccounting(t)
	return &consoleAccountingRepos{
		accountingRepos: a,
		consoleBuckets:  NewAccountBuckets(a.store),
		consoleLedger:   NewAccountLedger(a.store),
	}
}

// newEntitlementBucketForAccount opens a cycle bucket and returns it with the
// ACCOUNT that owns it, which the existing accounting fixture does not hand
// back. The owning account is a fact the whole file needs and the fixture that
// builds the bucket has no reason to expose: the bucket's row names an
// entitlement, the entitlement names a subscription, and the subscription names
// the account. Reaching it here rather than by a second query per case is the
// point — the expected value of every assertion below is a FACT about the
// fixture, not a fact the query under test produced.
func (a *consoleAccountingRepos) newEntitlementBucketForAccount(t *testing.T, name string) (accounting.Bucket, commerce.AccountID) {
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
	return bucket, account
}

// TestIntegrationConsoleBucketListShowsTheCycleBucketsMoneyLivesIn is the P0
// case. An operator's paid subscription produces a cycle bucket on every roll
// (application/commerce.go's FundEntitlement is the production path, not a test
// one), and that bucket is where the money they paid for is projected. The
// list must return it.
//
// The case also pins the other half — the PAYG bucket of the SAME account must
// come back — because the two halves fail differently and a fix that traded one
// for the other would be green here if the assertion were only about the
// entitlement side. And it pins a THIRD account's entitlement bucket as
// absent, because a predicate that resolves owners through the entitlement
// chain is exactly the kind of predicate that can be written one `OR` too
// wide, and an authorization predicate that leaks is a worse defect than one
// that hides.
func TestIntegrationConsoleBucketListShowsTheCycleBucketsMoneyLivesIn(t *testing.T) {
	a := integrationConsoleAccounting(t)

	cycle, owner := a.newEntitlementBucketForAccount(t, "it-consoleacct cycle owner")
	payg := a.newAccountBucketFor(t, owner)
	strangerCycle, _ := a.newEntitlementBucketForAccount(t, "it-consoleacct stranger")

	// The keyset, positioned deliberately. The zero value of After is NOT
	// used in this file, and the reason is a separate defect this test would
	// otherwise have been the wrong place to fix: `id > after` against a
	// uuid column raises SQLSTATE 22P02 for the empty string the first page
	// sends, which makes the first page of every uuid-keyed console list a
	// 500. That is its own report, in its own change, because it touches five
	// statements in three files and none of them is this one.
	//
	// So the position used here is beforeEverything, whose meaning is "the
	// beginning of the table" — which is what the zero value was documenting
	// when it claimed to be it. The keyset clause is a real predicate in this
	// statement and a test that never sends one is a test that has not proved
	// the statement carries it.
	rows, err := a.consoleBuckets.ListForAccount(t.Context(), accounting.AccountID(owner),
		persistence.FundingBucketPage{After: beforeEverything, Limit: 50})
	if err != nil {
		t.Fatalf("list the owner's buckets: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the owner's own bucket list is empty, so there is no last row to page from")
	}
	last := rows[len(rows)-1].ID
	tail, err := a.consoleBuckets.ListForAccount(t.Context(), accounting.AccountID(owner),
		persistence.FundingBucketPage{After: last, Limit: 50})
	if err != nil {
		t.Fatalf("list the owner's buckets from the last row of the first page: %v", err)
	}
	for _, row := range tail {
		if row.ID <= last {
			t.Errorf("a keyset read after %s returned %s, which is not past the keyset", last, row.ID)
		}
		if row.EntitlementID == "" && row.AccountID != accounting.AccountID(owner) {
			t.Errorf("the paged read returned bucket %s owned by account %q, not the reader's", row.ID, row.AccountID)
		}
	}

	seen := make(map[accounting.FundingBucketID]accounting.Bucket, len(rows))
	for _, row := range rows {
		seen[row.ID] = row
	}

	for _, want := range []struct {
		name   string
		bucket accounting.Bucket
	}{
		{"the account's own entitlement cycle bucket", cycle},
		{"the same account's PAYG bucket", payg},
	} {
		got, ok := seen[want.bucket.ID]
		if !ok {
			t.Errorf("%s (id %s) is missing from the owner's bucket list; the list returned %d row(s) of %s",
				want.name, want.bucket.ID, len(rows), bucketIDsOf(rows))
			continue
		}
		if got.EntitlementID != want.bucket.EntitlementID || got.AccountID != want.bucket.AccountID {
			t.Errorf("%s came back with the wrong owner: got entitlement_id %q account_id %q, want %q / %q",
				want.name, got.EntitlementID, got.AccountID, want.bucket.EntitlementID, want.bucket.AccountID)
		}
	}

	if _, leaked := seen[strangerCycle.ID]; leaked {
		t.Errorf("another account's entitlement cycle bucket %s appeared in the owner's list", strangerCycle.ID)
	}
}

// TestIntegrationConsoleCycleBucketLedgerAnswersItsOwner is the other half of
// the P0, and the half that is harder to notice: a bucket the account owns
// whose legs are all there, read through the console's ledger, returns an
// EMPTY page.
//
// The three legs below are appended through FundingLedger in real units of
// work, so the rows exist, carry the account's own money, and are reachable by
// any read that resolves the owner correctly. The assertion is that the console
// read returns all three — and the same read is then asked about a bucket
// belonging to somebody else, which must stay empty rather than leak. The kind
// filter and the keyset are also exercised here, because a statement that
// resolves the owner but drops one of the two optional predicates would still
// pass a case that only counted rows.
func TestIntegrationConsoleCycleBucketLedgerAnswersItsOwner(t *testing.T) {
	a := integrationConsoleAccounting(t)

	cycle, owner := a.newEntitlementBucketForAccount(t, "it-consoleacct ledger owner")
	grant, _ := a.appendCommitted(t, a.grantEntry(t, cycle.ID, 5000))
	a.appendCommitted(t, a.holdEntry(t, cycle.ID, 1000, acctReservation(t)))
	a.appendCommitted(t, a.adjustEntry(t, cycle.ID, 500, 0, grant.ID, acctCommandKey(t, "it-consoleacct-adjust")))

	strangerCycle, stranger := a.newEntitlementBucketForAccount(t, "it-consoleacct ledger stranger")
	a.appendCommitted(t, a.grantEntry(t, strangerCycle.ID, 7000))

	entries, err := a.consoleLedger.ListForBucket(t.Context(), accounting.AccountID(owner), cycle.ID,
		persistence.LedgerPage{Limit: 50})
	if err != nil {
		t.Fatalf("read the cycle bucket's ledger: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("the owner's own cycle bucket returned %d leg(s) %v, want 3 — the bucket exists, the legs exist, and the account owns it",
			len(entries), entryKindsOf(entries))
	}
	for i, want := range []struct {
		kind     accounting.Kind
		sequence int64
	}{
		{accounting.KindGrant, 1},
		{accounting.KindHold, 2},
		{accounting.KindAdjustment, 3},
	} {
		if entries[i].Kind != want.kind {
			t.Errorf("leg %d is a %q, want %q", i, entries[i].Kind, want.kind)
		}
		if entries[i].Sequence != want.sequence {
			t.Errorf("leg %d has sequence %d, want %d", i, entries[i].Sequence, want.sequence)
		}
		if entries[i].FundingBucketID != cycle.ID {
			t.Errorf("leg %d cites bucket %s, want %s", i, entries[i].FundingBucketID, cycle.ID)
		}
	}

	// The optional kind filter, on the statement that now carries the owner
	// predicate too: a reader filtering for "what came in" must still get the
	// grant and the topup, and must not get the hold.
	inflow, err := a.consoleLedger.ListForBucket(t.Context(), accounting.AccountID(owner), cycle.ID,
		persistence.LedgerPage{Kind: "grant", Limit: 50})
	if err != nil {
		t.Fatalf("read the cycle bucket's ledger filtered on kind: %v", err)
	}
	if len(inflow) != 1 || inflow[0].Kind != "grant" {
		t.Errorf("a kind-filtered read of the owner's own cycle bucket returned %v, want exactly the one grant", entryKindsOf(inflow))
	}

	// The keyset, for the same reason: the page after sequence 1 is legs two
	// and three, which is what a client resuming a cursor gets.
	second, err := a.consoleLedger.ListForBucket(t.Context(), accounting.AccountID(owner), cycle.ID,
		persistence.LedgerPage{AfterSequence: 1, Limit: 50})
	if err != nil {
		t.Fatalf("read the cycle bucket's ledger from a cursor: %v", err)
	}
	if len(second) != 2 || second[0].Sequence != 2 || second[1].Sequence != 3 {
		t.Errorf("a keyset read after sequence 1 returned %d leg(s) starting at %d, want 2 starting at 2",
			len(second), firstSequence(second))
	}

	// And the refusal, which is the one that must not become a 403: another
	// account's bucket is an empty page, never a leak of somebody else's
	// money and never an error that confirms the bucket exists.
	foreign, err := a.consoleLedger.ListForBucket(t.Context(), accounting.AccountID(stranger), cycle.ID,
		persistence.LedgerPage{Limit: 50})
	if err != nil {
		t.Fatalf("reading a bucket the account does not own = %v; it must be an empty page, not an error", err)
	}
	if len(foreign) != 0 {
		t.Errorf("a cycle bucket belonging to another account returned %d leg(s) to a reader that does not own it", len(foreign))
	}
}

// TestIntegrationConsoleAccountBucketLedgerIsUnchanged is the regression
// guard for the direction the fix must not move: the account-kind bucket, whose
// owner IS a column on its own row, keeps working. The new predicate resolves
// owners through the entitlement chain, and the cheapest way to write that
// chain's second leg wrongly is to forget that a PAYG bucket has no entitlement
// at all — an inner join rather than a disjunction would drop every PAYG
// bucket on the floor, and an account that had topped up would watch its
// balance disappear from the same screen that was supposed to show it.
func TestIntegrationConsoleAccountBucketLedgerIsUnchanged(t *testing.T) {
	a := integrationConsoleAccounting(t)

	payg := a.newAccountBucket(t, "it-consoleacct payg owner")
	a.appendCommitted(t, a.topupEntry(t, payg.ID, 3000, acctCommandKey(t, "it-consoleacct-payg-topup")))
	a.appendCommitted(t, a.holdEntry(t, payg.ID, 1200, acctReservation(t)))

	entries, err := a.consoleLedger.ListForBucket(t.Context(), accounting.AccountID(payg.AccountID), payg.ID,
		persistence.LedgerPage{Limit: 50})
	if err != nil {
		t.Fatalf("read the PAYG bucket's ledger: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("the account's own PAYG bucket returned %d leg(s) %v, want 2", len(entries), entryKindsOf(entries))
	}

	rows, err := a.consoleBuckets.ListForAccount(t.Context(), accounting.AccountID(payg.AccountID),
		persistence.FundingBucketPage{After: beforeEverything, Limit: 50})
	if err != nil {
		t.Fatalf("list the PAYG owner's buckets: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != payg.ID {
		t.Errorf("the PAYG owner's bucket list returned %d row(s) %s, want exactly its own PAYG bucket %s",
			len(rows), bucketIDsOf(rows), payg.ID)
	}
}

func bucketIDsOf(rows []accounting.Bucket) string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, string(row.ID))
	}
	return "[" + joinAll(ids, " ") + "]"
}

func entryKindsOf(entries []accounting.LedgerEntry) string {
	kinds := make([]string, 0, len(entries))
	for _, entry := range entries {
		kinds = append(kinds, string(entry.Kind))
	}
	return "[" + joinAll(kinds, " ") + "]"
}

func firstSequence(entries []accounting.LedgerEntry) int64 {
	if len(entries) == 0 {
		return 0
	}
	return entries[0].Sequence
}

func joinAll(parts []string, sep string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += sep
		}
		out += part
	}
	return out
}
