//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/analytics"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/ingestion"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The read model against the real `control` database.
//
// The scripted-statement tests beside this file pin which statements run, in
// what order, on which handle. Only PostgreSQL can answer what the database
// does with them, and the answers this tier exists for are five:
//
//   - a range join against a caller-supplied VALUES list really does return
//     one row per bucket, INCLUDING an empty one — a series that omitted it
//     would hand back a line drawn straight across a quiet hour;
//   - the two-arm bucket set expression really does reach a bucket whose
//     account_id is NULL, which a single arm silently drops;
//   - a settlement and the analytics attribution that scopes it cannot be
//     committed apart, and a redelivered fact produces no second row;
//   - a range is half-open on BOTH axes, so two consecutive windows tile the
//     timeline with neither overlap nor gap;
//   - the statement timeout the file sets really is scoped, which is a claim
//     about transactions and cannot be checked by a fake driver.
//
// Conventions are the ingestion and accounting suites': no t.Parallel, every
// id minted run-unique because nothing here deletes, fixtures reached through
// the ports rather than by writing SQL for the object under test, and the
// subject tables are this suite's to write. Accounts need no run-unique name —
// control.accounts.name carries no uniqueness (migrations/control/000002:44)
// — so the fixtures name themselves after what they are for.
//
// Run:
//
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:5432/postgres?sslmode=disable' \
//	  go test -tags=integration ./internal/adapters/outbound/postgres

func integrationAnalytics(t *testing.T) *analyticsRepos {
	t.Helper()
	acct := integrationAccounting(t)
	return &analyticsRepos{
		accounting: acct,
		commerce:   acct.commerce,
		store:      acct.store,
		repo:       NewAnalytics(acct.store),
		dims:       NewFactDimensions(acct.store),
	}
}

type analyticsRepos struct {
	accounting *accountingRepos
	commerce   *commerceRepos
	store      persistence.Store
	repo       persistence.Analytics
	dims       persistence.FactDimensions
}

// ---------------------------------------------------------------------------
// the series
// ---------------------------------------------------------------------------

// TestTheSeriesIsGapFreeAndKeepsTheBoundsItWasGiven is the read model's
// primary contract, and the empty interior is the half of it that is easy to
// get wrong. A caller rendering a chart has no way to invent a rule for which
// gaps to fill, so a series that dropped the quiet hour would hand back a line
// connecting across it — which reads as an hour of low activity rather than an
// hour of none, the direction that looks like good news.
//
// The bounds assertion is here rather than in a separate test because it is
// the same statement: the store keyed its aggregate off the intervals the
// caller produced, and a row whose own bounds came back different would mean
// it had cut the series against its own calendar instead. That is the timezone
// bug the whole design of the query exists to prevent, and SQL's date_trunc
// truncates in UTC.
func TestTheSeriesIsGapFreeAndKeepsTheBoundsItWasGiven(t *testing.T) {
	a := integrationAnalytics(t)
	account, bucket := a.newAccountWithBucket(t, "series gap-free")

	// One request applied a few minutes ago, so it lands in the LAST of three
	// hour buckets. The first two are the quiet ones.
	a.settleRequest(t, account, bucket, 700, "reported")

	from := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	buckets := a.hourlyBuckets(from, 3)
	usage, _, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: from, To: from.Add(3 * time.Hour), Buckets: buckets,
	})
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}

	if len(usage.Series) != len(buckets) {
		t.Fatalf("the series carries %d buckets, want %d — a bucket with no activity is present with zero figures, never absent:\n%s",
			len(usage.Series), len(buckets), renderSeries(usage.Series, buckets))
	}
	for i, want := range buckets {
		got := usage.Series[i]
		if !got.Start.Equal(want.Start) || !got.End.Equal(want.End) {
			t.Errorf("bucket %d came back as [%s, %s), want [%s, %s) — the store cut the series against its own calendar rather than against the bounds it was given",
				i, got.Start.UTC().Format(time.RFC3339), got.End.UTC().Format(time.RFC3339),
				want.Start.UTC().Format(time.RFC3339), want.End.UTC().Format(time.RFC3339))
		}
	}
	for i := 0; i < 2; i++ {
		if usage.Series[i].WithUsageFacts != 0 || usage.Series[i].Settled != 0 {
			t.Errorf("bucket %d = (with_facts %d, settled %d), want (0, 0) — a zero is a value here, and an absent bucket would be a different claim",
				i, usage.Series[i].WithUsageFacts, usage.Series[i].Settled)
		}
	}
	if last := usage.Series[2]; last.WithUsageFacts != 1 || last.Settled != 1 {
		t.Errorf("the last bucket = (with_facts %d, settled %d), want (1, 1)", last.WithUsageFacts, last.Settled)
	}
}

// TestTheSeriesCountsARequestOnceAcrossItsOwnFactClasses pins the difference
// between the two counts, which share a name in a way that invites confusion.
//
// A request that settles AND is later released is two facts of one class
// family — but the applier's rule is that within the settlement class the
// FIRST terminal fact decides the row, so the pair cannot both be applied. The
// fixture that makes this measurable is an ORPHAN: it is its own kind_class,
// applied alongside the settlement, and it is attributed to the same account.
// A count that grouped by request_id would see one request here; a count that
// counted rows would see two, and only the first is a count of requests.
//
// The fixture carries BOTH shapes of orphan, because the two test different
// counts. An orphan under the settled request's OWN id is the dedup case: two
// dimension rows, one request, and the with-facts count must not double. An
// orphan under a request id of its OWN is the FILTER case: one more request
// with a usage fact, and it has no settlement behind it, so the settled count
// must not move. With only the first, the two counts are one count and
// dropping the settled count's FILTER leaves both at one — the assertion below
// would hold under the very defect it exists to catch.
func TestTheSeriesCountsARequestOnceAcrossItsOwnFactClasses(t *testing.T) {
	a := integrationAnalytics(t)
	account, bucket := a.newAccountWithBucket(t, "series orphan")

	requestID := a.settleRequest(t, account, bucket, 700, "reported")
	// The orphan is a second fact of the SAME request under the other class,
	// which is exactly the pair a naive join would count twice. It carries the
	// settled request's own id, because "one request with two classes" is the
	// only shape that makes the dedup claim testable — two requests in one
	// bucket is two requests, and a count that said one would be wrong.
	a.orphanRequest(t, account, requestID)
	// And a second orphan, under a request id of its own. It moves the
	// with-facts count and must leave the settled count alone, which is the
	// only shape that makes the FILTER claim testable: under the first orphan
	// alone, a settled count with no filter at all still reads one.
	a.orphanRequest(t, account, acctRequestID(t))

	now := time.Now().UTC()
	usage, _, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: now.Add(-time.Hour), To: now.Add(time.Hour), Buckets: a.hourlyBuckets(now.Truncate(time.Hour), 2),
	})
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	if usage.Series[0].WithUsageFacts != 2 {
		t.Errorf("the bucket counts %d requests with usage facts, want 2 — the settled request and the orphan of its own are two requests, and the one carrying both classes is a single request rather than two",
			usage.Series[0].WithUsageFacts)
	}
	if usage.Series[0].Settled != 1 {
		t.Errorf("the bucket counts %d settled requests, want 1 — the orphan carries a capture method but books no charge, and it is not a settlement of record; two here means the settled count is not reading the settlements join at all",
			usage.Series[0].Settled)
	}
	if usage.Capture.Total() != 1 {
		t.Errorf("the capture total is %d, want 1 — the split is over SETTLEMENTS, and an orphan is a settlement of no record", usage.Capture.Total())
	}
}

// TestTheSeriesCarriesOnlyTheCallersRequests is the tenancy rule at the one
// place the database can be made to answer it: two accounts, each with one
// settled request, each reading the same range.
//
// The money half is the one worth having. The request counts are scoped by the
// fact dimension's own account column; the settled amount is a SUM over
// control.settlements, which carries NO account column at all and is scoped
// through an EXISTS against the same dimensions. A read that scoped the counts
// and forgot the EXISTS would produce a series that looks right beside a total
// that belongs to somebody else — and the total is the number a customer bills
// from.
func TestTheSeriesCarriesOnlyTheCallersRequests(t *testing.T) {
	a := integrationAnalytics(t)
	mine, myBucket := a.newAccountWithBucket(t, "tenancy mine")
	theirs, theirBucket := a.newAccountWithBucket(t, "tenancy theirs")

	a.settleRequest(t, mine, myBucket, 700, "reported")
	a.settleRequest(t, theirs, theirBucket, 400, "gateway_observed")

	now := time.Now().UTC()
	usage, _, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: mine, From: now.Add(-time.Hour), To: now.Add(time.Hour), Buckets: a.hourlyBuckets(now.Truncate(time.Hour), 2),
	})
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	if usage.Series[0].WithUsageFacts != 1 {
		t.Errorf("the caller counts %d requests, want 1 — the stranger's request is in the same range and the same table", usage.Series[0].WithUsageFacts)
	}
	if usage.SettledMinorUnits != 700 {
		t.Errorf("SettledMinorUnits = %d, want 700 — the sum is scoped through the dimensions the counts are, and 1100 would mean the EXISTS was not applied", usage.SettledMinorUnits)
	}
	if usage.Capture.Total() != 1 || usage.Capture.GatewayObserved != 0 {
		t.Errorf("the capture split = %+v, want one reported and no gateway_observed — the stranger's settlement is not this account's", usage.Capture)
	}
}

// TestARangeIsHalfOpenOnBothAxes is the interval claim, measured on the
// boundaries themselves rather than around them. A range closed at the top
// would double-count an instant that falls on the shared edge of two
// consecutive windows, and this is a report a caller may bill from: an
// off-by-one-nanosecond at the seam is a month of double-billing.
//
// The windows are derived from where the request ACTUALLY landed rather than
// from an instant the test chose. The dimension's applied_at is the
// transaction's own now(), so no caller can steer it — a test that asserted
// against a hand-picked instant would be asserting against a row that was
// never written, and would pass or fail for reasons that have nothing to do
// with the interval claim. The instant is read back from the row the read
// itself projects, which is the one number both windows must agree about.
func TestARangeIsHalfOpenOnBothAxes(t *testing.T) {
	a := integrationAnalytics(t)
	account, bucket := a.newAccountWithBucket(t, "half-open")
	requestID := a.settleRequest(t, account, bucket, 700, "reported")

	// The instant the attribution stamped, read back rather than assumed. It
	// is truncated to the second because the window below is placed to end one
	// second after it — the attribution carries a timestamp, and a boundary a
	// few microseconds off would be measuring the clock rather than the
	// interval.
	var appliedAt time.Time
	if err := a.accounting.db.QueryRowContext(t.Context(),
		`SELECT applied_at FROM control.analytics_fact_dimensions WHERE request_id = $1`,
		string(requestID)).Scan(&appliedAt); err != nil {
		t.Fatalf("read back the applied instant: %v", err)
	}
	applied := appliedAt.UTC().Truncate(time.Second)
	after := applied.Add(time.Second)

	// The windows tile the hour the request landed in: the first ENDS one
	// second after the request, the second STARTS there. Half-open on both
	// axes means the request — at `applied`, strictly between the two seams —
	// is in the first and not the second, and the second's own bucket is
	// present with zero figures rather than absent.
	first := a.hourlyBuckets(applied.Truncate(time.Hour), 1)
	included, _, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: first[0].Start, To: after, Buckets: first,
	})
	if err != nil {
		t.Fatalf("Usage() over the including window: %v", err)
	}
	if included.SettledMinorUnits != 700 || included.Series[0].WithUsageFacts != 1 {
		t.Errorf("the including window returned (%d minor units, %d requests), want (700, 1)",
			included.SettledMinorUnits, included.Series[0].WithUsageFacts)
	}

	second := []analytics.Bucket{{Start: after, End: after.Truncate(time.Hour).Add(time.Hour)}}
	excluded, _, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: after, To: second[0].End, Buckets: second,
	})
	if err != nil {
		t.Fatalf("Usage() over the following window: %v", err)
	}
	if excluded.SettledMinorUnits != 0 || excluded.Series[0].WithUsageFacts != 0 {
		t.Errorf("the following window returned (%d minor units, %d requests), want (0, 0) — the request is before this window's exclusive start, so it belongs to the first window alone",
			excluded.SettledMinorUnits, excluded.Series[0].WithUsageFacts)
	}
}

// ---------------------------------------------------------------------------
// money, flows and balances
// ---------------------------------------------------------------------------

// TestTheBalancesReachAnEntitlementFundedBucket is the failure this plane's own
// migration header names: funding_buckets.account_id is NULLABLE, because a
// subscription-funded bucket names an entitlement instead (migrations/control/
// 000006, the owner XOR). A read scoped through that column alone returns a
// figure that is wrong in the direction that looks like good news — an
// entitlement balance simply does not appear, and the customer is shown less
// than they have.
//
// The fixture is the only one that can catch it: ONE account holding a PAYG
// bucket of 1000 and an entitlement bucket of 2000. Either arm alone returns
// 1000 or 2000; only the two-arm set expression returns 3000. A regression to
// one arm is a number short by the entitlement's whole balance, and the
// assertion below is written so that the shortfall is unmistakable.
func TestTheBalancesReachAnEntitlementFundedBucket(t *testing.T) {
	acct := integrationAccounting(t)

	// The entitlement side first, because it owns the account: the PAYG
	// bucket below is created FOR that account rather than opening its own,
	// which is what makes the two comparable.
	entitled := acct.newEntitlementBucket(t, "balances entitled")
	account := entitledAccount(t, acct, entitled)

	payg, err := accounting.NewAccountBucket(acctBucketID(t), accounting.AccountID(account), time.Now().UTC())
	if err != nil {
		t.Fatalf("new account bucket: %v", err)
	}
	if err := acct.buckets.Create(t.Context(), payg); err != nil {
		t.Fatalf("create account bucket: %v", err)
	}
	// A topup funds an ACCOUNT bucket and a grant funds a CYCLE bucket — the
	// ownership is part of the algebra (accounting.ApplyTo refuses the other
	// pairing), so a fixture that granted both would be refused before the
	// read it exists to exercise ever ran.
	acct.appendCommitted(t, acct.topupEntry(t, payg.ID, 1000, acctCommandKey(t, "analytics-payg-")))
	acct.appendCommitted(t, acct.grantEntry(t, entitled.ID, 2000))

	repo := NewAnalytics(acct.store)
	now := time.Now().UTC()
	usage, _, err := repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: now.Add(-time.Hour), To: now, Buckets: a0(now),
	})
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	if usage.Balances.Available != 3000 {
		t.Errorf("Balances.Available = %d, want 3000 — a PAYG bucket of 1000 and an entitlement bucket of 2000 under one account, and 1000 is exactly what a read scoped through funding_buckets.account_id alone returns",
			usage.Balances.Available)
	}
	if usage.Balances.Held != 0 {
		t.Errorf("Balances.Held = %d, want 0 — the grants are credited capacity, not reserved capacity", usage.Balances.Held)
	}
}

// TestTheBalancesAreTheProjectionAndNotAReDerivation is the figure the ledger
// is the authority for. A balance read that recomputed it from the legs would
// be a THIRD figure beside the cached columns and the legs themselves, and
// ADR 0011 has no reconciliation check that could adjudicate between them.
//
// The fixture tops up 1000 and holds then consumes 300 of it. The three
// figures it derives are settled 700, held 0 and available 700: a consume
// moves BOTH balances, so the hold it spent is the hold the consume reserved,
// and what settles is capacity left rather than money spent. The assertion is
// that the read agrees with the derivation the port computes from the legs
// alone — which is the reconciliation check this plane already runs, reached
// here as the check that the read did not re-derive in a way that could
// disagree with it.
func TestTheBalancesAreTheProjectionAndNotAReDerivation(t *testing.T) {
	acct := integrationAccounting(t)
	bucket := acct.newAccountBucket(t, "balances are the projection")
	acct.appendCommitted(t, acct.topupEntry(t, bucket.ID, 1000, acctCommandKey(t, "analytics-seed-")))
	acct.heldConsumeCommitted(t, bucket.ID, 300)

	derived, err := acct.projections.DerivedBalances(t.Context(), bucket.ID)
	if err != nil {
		t.Fatalf("DerivedBalances() error = %v", err)
	}
	if derived.Settled != accounting.BalanceOf(acctAmount(t, 700)) || derived.Available != accounting.BalanceOf(acctAmount(t, 700)) {
		t.Fatalf("the fixture does not derive the balances the assertions below are about: settled %d, available %d — a consume moves both balances, so 1000 topped up and 300 consumed settles 700", derived.Settled, derived.Available)
	}

	usage, _, err := NewAnalytics(acct.store).Usage(t.Context(), persistence.UsageQuery{
		AccountID: paygAccount(t, acct, bucket), From: time.Now().UTC().Add(-time.Hour), To: time.Now().UTC(), Buckets: a0(time.Now().UTC()),
	})
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	if usage.Balances.Held != int64(derived.Held) || usage.Balances.Available != int64(derived.Available) {
		t.Errorf("the read returned (held %d, available %d), want (held %d, available %d) — the figures are the projection, and a re-derivation would be a third number the plane has no check for",
			usage.Balances.Held, usage.Balances.Available, derived.Held, derived.Available)
	}
}

// TestTheFlowsAndTheBalancesAreNotTheSameFigure pins the port's own rule —
// flows and balances never share a statement, a helper or an accumulator —
// with a fixture where the two genuinely differ.
//
// A topup of 1000 today, consumed 300 today, with the window closing BEFORE
// the consumption is impossible on one clock; the separable form is funding
// before the window and a consumption inside it. Then money-settled-over-the-
// range is 0 — the consume here books a leg, not a settlement of record — while
// available-as-at-the-end is 500, and a reader who had merged them would
// report one of the two as the other.
//
// The release is on a reservation of its own, because a consume spends the
// hold it was made under: releasing against the consume's reservation would
// release capacity that is no longer held.
func TestTheFlowsAndTheBalancesAreNotTheSameFigure(t *testing.T) {
	acct := integrationAccounting(t)
	bucket := acct.newAccountBucket(t, "flows versus balances")
	acct.appendCommitted(t, acct.topupEntry(t, bucket.ID, 1000, acctCommandKey(t, "analytics-seed-")))
	acct.heldConsumeCommitted(t, bucket.ID, 300)
	// A hold that survives, so there is something to release: 400 reserved,
	// 200 of it given back. The two legs name ONE reservation — a release
	// names the hold it is releasing, and a second mint would be a release of
	// a hold that was never made.
	acct.heldThenReleased(t, bucket.ID, 400, 200)

	account := paygAccount(t, acct, bucket)
	now := time.Now().UTC()
	usage, _, err := NewAnalytics(acct.store).Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: now.Add(-time.Hour), To: now, Buckets: a0(now),
	})
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	if usage.SettledMinorUnits != 0 {
		t.Errorf("SettledMinorUnits = %d, want 0 — this bucket has legs but no settlement of record, and the money read is a sum over settlement headers rather than over legs", usage.SettledMinorUnits)
	}
	if usage.ReleasedMinorUnits != 200 {
		t.Errorf("ReleasedMinorUnits = %d, want 200", usage.ReleasedMinorUnits)
	}
	if usage.FundsAddedMinorUnits != 1000 {
		t.Errorf("FundsAddedMinorUnits = %d, want 1000 — a grant is capacity credited, not revenue: nothing in this plane has received a payment", usage.FundsAddedMinorUnits)
	}
	// 1000 topped up, 300 consumed, 400 held and 200 of it released. The
	// consume and its hold cancel, so what is left held is the 400 minus the
	// 200 released; available is the capacity not held.
	if usage.Balances.Held != 200 || usage.Balances.Available != 500 {
		t.Errorf("the balances = (held %d, available %d), want (held 200, available 500) — 1000 topped up, 300 consumed, 400 held, 200 released",
			usage.Balances.Held, usage.Balances.Available)
	}
}

// TestTheThreeMoneyFiguresAreScopedTheSameWayAnAccountOwnsABucket is the
// reconciliation claim, and it is the only test here that reads all three flow
// figures at once — because the three are only comparable to each other if the
// account that owns them is the same account.
//
// The three are scoped by two different routes. Released and funds added are
// scoped through the account's BUCKET SET — both arms of it, the direct arm
// and the entitlement arm — because a ledger leg names a bucket and the bucket
// is what carries the ownership. Settled is scoped through the fact
// dimensions instead, because a settlement header names a request and the
// request is what carries the attribution. Two routes, and nothing on this
// plane asserts they name the same set: a request that drew on an entitlement
// bucket is written to the dimensions by the applier and is perfectly visible
// to the settled read, while the entitlement bucket that actually paid for it
// reaches the flows through two joins from subscriptions. One route naming the
// account and the other not would be invisible in every other assertion here —
// the balances test proves the bucket set reaches the balances, and the money
// test proves the header is the source of the figure, and neither one would
// notice that the settled figure and the flows disagreed about who they are
// counting.
//
// So the fixture pays one request ENTIRELY out of an entitlement cycle, under
// one account, and reads all three. The grant that credits the cycle is a
// funds-added flow and the consume the settlement is a settled flow, both of
// them on the same bucket, and the entitlement bucket is the account's only
// bucket. A scoping route that resolved the account through
// funding_buckets.account_id alone returns zero for the first two — 1000
// credited and 300 spent, a customer who was never charged and never funded —
// which is the direction a number is least likely to be questioned in.
func TestTheThreeMoneyFiguresAreScopedTheSameWayAnAccountOwnsABucket(t *testing.T) {
	a := integrationAnalytics(t)
	acct := a.accounting
	bucket := acct.newEntitlementBucket(t, "money scoped like a balance")
	account := entitledAccount(t, acct, bucket)

	// The cycle is credited by a GRANT rather than a topup: a topup is the
	// PAYG arm's funding command and would be a second way for this test to
	// be right for the wrong reason. A grant is the entitlement arm's.
	acct.appendCommitted(t, acct.grantEntry(t, bucket.ID, 1000))
	// And the request is settled out of that granted capacity, in the single
	// unit of work the applier uses: the hold, the consume, the settlement
	// header, the applied fact and the account attribution, all five or none.
	//
	// The attribution row is the half a dimension-missing test cannot reach.
	// A bucket-set scope finds this settlement whether or not the row is
	// there, so a fixture that forgot it would still see a correct settled
	// figure — and the test would pass for the wrong reason. It is the settled
	// route alone that reads the dimensions, so it is the settled figure
	// alone that can notice.
	a.settleRequest(t, account, bucket.ID, 300, "reported")

	now := time.Now().UTC()
	usage, _, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: now.Add(-time.Hour), To: now, Buckets: a0(now),
	})
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}

	// Each figure is stated against the OTHER TWO rather than against zero, so
	// a failure says which route disagreed rather than merely that something
	// is zero. The released figure has no release in this fixture, so it is
	// stated alone: the point at stake is the scope, and a zero there is
	// indistinguishable from a correctly scoped zero.
	if usage.SettledMinorUnits != 300 {
		t.Errorf("SettledMinorUnits = %d, want 300 — a request charged against this account's entitlement cycle, and the settled read reaches it through the fact dimensions",
			usage.SettledMinorUnits)
	}
	if usage.FundsAddedMinorUnits != 1000 {
		t.Errorf("FundsAddedMinorUnits = %d, want 1000 — the grant that credited this account's entitlement cycle, and the flows reach it through the account's bucket set; a scope that resolved the account through funding_buckets.account_id alone returns 0 for both of this request's flows",
			usage.FundsAddedMinorUnits)
	}
	if usage.ReleasedMinorUnits != 0 {
		t.Errorf("ReleasedMinorUnits = %d, want 0 — nothing in this fixture released anything", usage.ReleasedMinorUnits)
	}

	// The same money, counted two ways. The settlement header is the one the
	// plane's own derivation wrote, and the grant is the leg that funded it;
	// this account spent 300 of the 1000 its cycle was credited, and a report
	// that says otherwise is reporting on a request the account did not make.
	//
	// The comparison is made by the SUM rather than by each figure alone,
	// because a zero on both sides would satisfy any pairwise equality and
	// this test's whole claim is that the two sides are not both zero.
	spent := usage.SettledMinorUnits
	credited := usage.FundsAddedMinorUnits
	if spent == 0 || credited == 0 {
		t.Fatalf("this fixture booked a settlement of 300 against a grant of 1000 and read back (settled %d, funds added %d): at least one of the two money routes is counting nothing, and the test cannot tell which",
			spent, credited)
	}
	if spent > credited {
		t.Errorf("this account spent %d against a capacity it was credited %d — the settled figure is scoped to an account the flows do not name, or the grant never reached the flows", spent, credited)
	}
}

// TestTheMoneyIsTheSettlementHeaderAndNotAReDerivedAmount is the rounding
// claim, and it is the one that cannot be demonstrated with two requests — it
// takes a request whose own amount is a ROUNDED ceiling, so a sum of
// per-request figures and a sum of headers differ by construction.
//
// The port's header states it plainly: the derivation applies one ceiling over
// a request's summed raw cost, so a per-request amount is rounded and the sum
// of rounded figures is not the rounded total. What this test can pin is the
// weaker and equally load-bearing half — that the settled figure tracks the
// HEADER, which is written once. Two settlements of 700 and 400 must sum to
// 1100, and the per-request legs on those buckets carry a different total
// only if a leg and a header disagree — which is the projection's subject and
// not this one's.
func TestTheMoneyIsTheSettlementHeaderAndNotAReDerivedAmount(t *testing.T) {
	a := integrationAnalytics(t)
	account, bucket := a.newAccountWithBucket(t, "money is the header")
	a.settleRequest(t, account, bucket, 700, "reported")
	a.settleRequest(t, account, bucket, 400, "gateway_observed")

	now := time.Now().UTC()
	usage, _, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: now.Add(-time.Hour), To: now, Buckets: a0(now),
	})
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	if usage.SettledMinorUnits != 1100 {
		t.Errorf("SettledMinorUnits = %d, want 1100 — two settlement headers of 700 and 400, summed exactly", usage.SettledMinorUnits)
	}
	// And the legs agree with the headers here, which is what makes the
	// header-sum a safe stand-in for a leg-sum in the ordinary case. The
	// difference only appears under rounding, and the plane's own projection
	// is what asserts they agree.
	//
	// The derivation is a CAPACITY balance, not a spend: it nets the topup
	// that funded the bucket against the consumes that spent it, so what it
	// settles is what is LEFT. Adding the funding back would double it.
	// Subtracting it from the funding gives the consumed total, which is the
	// figure the headers are supposed to add up to — and comparing the whole
	// derivation to 1100 would be comparing "money spent" to "capacity left",
	// two numbers that differ by the funding and agree about nothing.
	derived, err := a.accounting.projections.DerivedBalances(t.Context(), bucket)
	if err != nil {
		t.Fatalf("DerivedBalances() error = %v", err)
	}
	settlementLegs := accounting.BalanceOf(acctAmount(t, analyticsTestFund)) - derived.Settled
	if settlementLegs != accounting.BalanceOf(acctAmount(t, 1100)) {
		t.Errorf("the bucket's settlement legs net to %d, want 1100 — the settlement headers and the bucket's legs disagree, and the read summed the headers",
			settlementLegs)
	}
}

// TestAMinorUnitArrivesWholeIs the fixture every other money case in this file
// cannot be. 700 and 400 and 300 are all multiples of a hundred, so each of
// them survives being divided by a hundred, truncated, or cast through a float
// and rounded back — the three operations a SUM over money is most likely to
// pick up by accident, and none of which any assertion here would have noticed.
// The figure is an int64 of minor units, so a read that divided by the scale
// and re-multiplied would answer 100 for a request that settled for 101, and a
// report that is short by one minor unit on every odd request is not a report
// anyone can reconcile against an invoice.
//
// 101 is the smallest amount that makes the claim: it is not divisible by any
// round number, it survives as 101, and the fixtures beside it are all round,
// so a suite whose every other sum is a multiple of a hundred is a suite that
// has never asked whether the read preserves the units it was given. The sum is
// asserted against 101 AND 111 rather than against 101 alone, because the second
// figure is what rules out the read having produced a number that is merely
// close — a per-request ceiling, or a division applied on the way in and again
// on the way out, each lands elsewhere.
//
// Two requests rather than one, deliberately: one request settled for 101 would
// be satisfied by a read that returned the request's amount without summing it
// at all. It is the SECOND that a header-sum has to keep, and it is also what
// catches a read that summed per-REQUEST amounts where the two requests had
// charged differently — the state the port's own header warns about when it
// says the per-request amount is rounded and the sum of rounded figures is not
// the rounded total. Here the two agree, and that agreement is the point: the
// figure is exact either way, and this fixture is what would notice a read that
// stopped being exact.
func TestAMinorUnitArrivesWhole(t *testing.T) {
	a := integrationAnalytics(t)
	account, bucket := a.newAccountWithBucket(t, "minor unit arrives whole")

	// The two charges differ by ten minor units, so the second is the
	// distinguishing one: a read that lost the odd remainder on the first
	// request and summed faithfully after it would return 110 rather than
	// either of the two figures asserted below.
	a.settleRequest(t, account, bucket, 101, "reported")
	a.settleRequest(t, account, bucket, 10, "gateway_observed")

	now := time.Now().UTC()
	usage, _, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: now.Add(-time.Hour), To: now, Buckets: a0(now),
	})
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	if usage.SettledMinorUnits != 111 {
		t.Errorf("SettledMinorUnits = %d, want 111 — two settlement headers of 101 and 10 minor units, summed exactly, and a figure that has passed through a scale or a float on the way in would land at 100, 110 or something no pair of headers could produce",
			usage.SettledMinorUnits)
	}
	if usage.SettledMinorUnits%100 == 0 {
		t.Errorf("SettledMinorUnits = %d, which is a whole number of major units; a fixture whose every amount is a multiple of a hundred cannot tell a read that divides by the scale and re-multiplies from one that does not", usage.SettledMinorUnits)
	}
	// The counts are the control: a read that mangled the money alone would
	// leave the request counts untouched, so they say the range and the
	// attribution are the two requests the fixture booked and the figure
	// above is about the amount rather than about what was found.
	if usage.Capture.Total() != 2 {
		t.Errorf("the capture split counts %d settlements, want 2 — the two this fixture booked, and a capture count of 1 would mean the sum above was a one-request figure wearing a two-request fixture", usage.Capture.Total())
	}
}

// ---------------------------------------------------------------------------
// the capture split
// ---------------------------------------------------------------------------

// TestTheCaptureSplitCountsSettlementsAndNotFacts is the population the
// contract states, proven with a fixture that answers differently under the
// two readings: one settled fact carrying a capture method, and one RELEASED
// fact with none.
//
// A split counted over facts would carry the release into a denominator it does
// not belong in. A split counted over ALL facts carrying a capture method would
// additionally count the ORPHAN, which carries a capture method and books no
// charge — a settlement in the population that never priced anything. The
// kind = 'settled' predicate is therefore load-bearing twice over, and this
// fixture breaks both readings at once.
func TestTheCaptureSplitCountsSettlementsAndNotFacts(t *testing.T) {
	a := integrationAnalytics(t)
	account, bucket := a.newAccountWithBucket(t, "capture population")

	a.settleRequest(t, account, bucket, 700, "reported")
	a.settleRequest(t, account, bucket, 700, "gateway_observed")
	a.settleRequest(t, account, bucket, 700, "reservation_floor")
	a.releaseRequest(t, account, bucket, 700)
	// The orphan is a request of its OWN: it is a fact that carries a capture
	// method and books no charge, which is the row a split over "facts with a
	// capture method" would count and a split over settlements would not.
	a.orphanRequest(t, account, acctRequestID(t))

	now := time.Now().UTC()
	usage, _, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: now.Add(-time.Hour), To: now, Buckets: a0(now),
	})
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	want := analytics.Capture{Reported: 1, GatewayObserved: 1, ReservationFloor: 1}
	if usage.Capture != want {
		t.Errorf("the capture split = %+v, want %+v — the release captures nothing and the orphan books no charge, and both carry a capture method or a fact row that a split over facts would have counted", usage.Capture, want)
	}
	if usage.Capture.Total() != 3 {
		t.Errorf("the capture total = %d, want 3 — the three methods partition the settlements that captured, so a rate over this split divides by this and by nothing else", usage.Capture.Total())
	}
}

// ---------------------------------------------------------------------------
// freshness
// ---------------------------------------------------------------------------

// TestTheFreshnessIsARecordTimeAndNotAWatermark is the honesty claim, and it
// has two halves. The basis must say which of the two it is, so a caller
// cannot mistake this for a watermark the runtime published; and the instant
// must be the cursor row's own updated_at, which is why the read proves it by
// advancing the cursor and watching the answer move.
//
// The cursor's POSITION is never parsed by the read — it is an opaque string
// the Data Plane issued and this plane is forbidden to decompose. So the
// fixture advances the position to a value that says nothing about time, and
// the instant that comes back is the server's own: the proof that the read is
// reading a record time rather than decoding a position.
func TestTheFreshnessIsARecordTimeAndNotAWatermark(t *testing.T) {
	a := integrationAnalytics(t)
	account, _ := a.newAccountWithBucket(t, "freshness")
	now := time.Now().UTC()

	_, first, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: now.Add(-time.Hour), To: now, Buckets: a0(now),
	})
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	if first.Basis != analytics.FreshnessFactFeedPass {
		t.Errorf("the freshness basis is %q, want %q — a DataThrough instant is a statement about this plane's own pass and about nothing else", first.Basis, analytics.FreshnessFactFeedPass)
	}
	if first.DataThrough.IsZero() {
		t.Fatal("the freshness carries no instant — the cursor is a seeded singleton and 000001 inserts it, so an absent instant is a missing row rather than a fresh database")
	}

	// Advance the cursor with a position that is not a time, and read again.
	// A monotonic clock is the only way this test can be wrong: the instant
	// comes from the database, and the advance is a separate statement.
	cursor := NewIngestionCursor(a.store)
	held, err := cursor.Position(t.Context())
	if err != nil {
		t.Fatalf("Position() error = %v", err)
	}
	if err := a.store.WithinTx(t.Context(), func(txCtx context.Context) error {
		return cursor.Advance(txCtx, held, "pos-"+string(acctSettlementID(t)))
	}); err != nil {
		t.Fatalf("advance the cursor: %v", err)
	}

	_, second, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: now.Add(-time.Hour), To: now, Buckets: a0(now),
	})
	if err != nil {
		t.Fatalf("Usage() after the advance: %v", err)
	}
	if second.DataThrough.Before(first.DataThrough) {
		t.Errorf("the freshness went backwards: %s then %s — the instant is the cursor row's own record time, which an advance moves forward",
			first.DataThrough.UTC().Format(time.RFC3339Nano), second.DataThrough.UTC().Format(time.RFC3339Nano))
	}
	if second.Basis != first.Basis {
		t.Errorf("the basis changed from %q to %q across an advance; the basis is a property of the read, not of the cursor's state", first.Basis, second.Basis)
	}
}

// TestAFreshnessOfNeverIsNotAFreshnessOfZero is the guard on the singleton
// cursor. The row is seeded by 000001, so its ABSENCE means the schema was not
// applied as it ships — and reporting that as a time would be a caller drawing
// a chart from a statement about nothing. The read refuses instead.
//
// The fixture removes the row rather than blanking its instant, and that is
// the reachable state. ingestion_cursor.updated_at is
// `timestamptz NOT NULL DEFAULT now()` (migrations/control/000008:74), so a
// NULL instant is not a state the row can be in: an earlier version of this
// test tried to reach the blank-instant refusal with an UPDATE ... SET
// updated_at = NULL and was refused by the very constraint the refusal was
// defending. A test that cannot set up the state it claims to cover is not
// evidence the code handles it, so the test covers the state that exists —
// and the read keeps a defensive zero check beside the absent-row one, marked
// as unreachable, because a future migration that relaxed the column should
// find a refusal rather than an epoch time.
func TestAFreshnessOfNeverIsNotAFreshnessOfZero(t *testing.T) {
	a := integrationAnalytics(t)
	account, _ := a.newAccountWithBucket(t, "freshness missing")
	now := time.Now().UTC()

	ctx := t.Context()
	if _, err := a.store.Querier(ctx).ExecContext(ctx, `DELETE FROM control.ingestion_cursor WHERE id = 1`); err != nil {
		t.Fatalf("remove the cursor row: %v", err)
	}
	// A background context, because t.Context() is already cancelled by the
	// time cleanup runs — a restore that cannot run leaves the singleton
	// missing for every suite after this one on the same database.
	t.Cleanup(func() {
		if _, err := a.store.Querier(context.Background()).ExecContext(context.Background(), `
			INSERT INTO control.ingestion_cursor (id, position, updated_at)
			VALUES (1, '', now())
			ON CONFLICT (id) DO NOTHING`); err != nil {
			t.Errorf("restore the cursor row: %v", err)
		}
	})

	_, freshness, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: now.Add(-time.Hour), To: now, Buckets: a0(now),
	})
	if err == nil {
		t.Fatalf("Usage() with no cursor row = freshness %s, want the refusal — a caller drawing a chart from a statement about nothing is the failure this guards", freshness.DataThrough)
	}
	if !strings.Contains(err.Error(), "singleton row is absent") {
		t.Errorf("Usage() with no cursor row = %v, want the named refusal", err)
	}
}

// TestTheCoverageAnswersForThisAccountAndNotForThisRange is the read the
// answer's availability turns on, and the one fact the series cannot supply.
//
// Three properties, and none of them is checkable without a database:
//
//   - the flag is FALSE for an account this plane has no derived row for,
//     which is the state a fresh tenant is in;
//   - it becomes TRUE once a fact of this account's is applied, so it is driven
//     by the attribution the ingestion path writes rather than by the account's
//     existence (the fixture's account and bucket exist from the first read);
//   - it stays FALSE for a DIFFERENT account while the plane holds rows for the
//     first — which is the whole reason the probe is scoped to the account. A
//     plane-wide EXISTS would answer TRUE here and would make one tenant's
//     availability depend on another tenant's traffic.
//
// The last assertion is the mechanism the use case depends on: the uncovered
// account's range still comes back with a row per bucket, every figure zero.
// That is what a quiet range looks like too, and it is why the decision cannot
// be made on the series — the two states are the same shape.
func TestTheCoverageAnswersForThisAccountAndNotForThisRange(t *testing.T) {
	a := integrationAnalytics(t)
	covered, bucket := a.newAccountWithBucket(t, "coverage covered")
	uncovered, _ := a.newAccountWithBucket(t, "coverage uncovered")
	now := time.Now().UTC()

	from := now.Add(-2 * time.Hour).Truncate(time.Hour)
	to := from.Add(3 * time.Hour)
	buckets := a.hourlyBuckets(from, 3)
	query := func(account string) persistence.UsageQuery {
		return persistence.UsageQuery{AccountID: account, From: from, To: to, Buckets: buckets}
	}

	before, _, err := a.repo.Usage(t.Context(), query(uncovered))
	if err != nil {
		t.Fatalf("Usage() before any fact = %v", err)
	}
	if before.AccountHasDerivations {
		t.Error("the coverage says this plane holds a derived row for an account it has never ingested — the flag would then be a property of the plane rather than of the account")
	}

	// A settlement books this account's first derived row. The account and its
	// bucket already existed, so what moves the flag is the ATTRIBUTION and not
	// the fixture.
	a.settleRequest(t, covered, bucket, 900, "reported")

	after, _, err := a.repo.Usage(t.Context(), query(covered))
	if err != nil {
		t.Fatalf("Usage() after a settlement = %v", err)
	}
	if !after.AccountHasDerivations {
		t.Error("the coverage still says this plane holds nothing for an account whose settlement it has just applied — the read would answer not_available for every account it HAS ingested")
	}

	other, _, err := a.repo.Usage(t.Context(), query(uncovered))
	if err != nil {
		t.Fatalf("Usage() for the neighbour = %v", err)
	}
	if other.AccountHasDerivations {
		t.Error("the coverage for one account moved when another account's fact was applied — the probe is not scoped to the account, and one tenant's report would then depend on another tenant's traffic")
	}

	// The shape that makes the flag necessary: the same zero-filled,
	// gap-free series a genuinely quiet range produces.
	if len(other.Series) != len(buckets) {
		t.Fatalf("the uncovered account's series carries %d buckets, want %d — the series is gap-free for an account this plane holds nothing for, which is the whole reason the coverage cannot be read off it:\n%s",
			len(other.Series), len(buckets), renderSeries(other.Series, buckets))
	}
	for i, point := range other.Series {
		if point.WithUsageFacts != 0 || point.Settled != 0 {
			t.Errorf("the uncovered account's bucket %d = (with_facts %d, settled %d), want (0, 0)", i, point.WithUsageFacts, point.Settled)
		}
	}
}

// ---------------------------------------------------------------------------
// the bound
// ---------------------------------------------------------------------------

// TestTheReadBoundBinds is the property a unit of work buys and a fake driver
// cannot: `SET LOCAL` is scoped to a transaction and is a SILENT NO-OP outside
// one. A read that ran on the pool would be unbounded at the server while this
// file claimed it bounded — the claim would be decoration, enforced by nothing.
//
// The test makes the server take longer than the bound allows and reads the
// refusal back as the RETRY-LATER error rather than as a failure. That
// distinction is the whole reason classifyRead exists: a caller told a slow
// report had failed would page an operator for a database that is answering.
// So the assertion is both halves — that pg_sleep was cut off, and that the
// error is the named one.
func TestTheReadBoundBinds(t *testing.T) {
	a := integrationAnalytics(t)
	account, _ := a.newAccountWithBucket(t, "read bound")
	now := time.Now().UTC()

	// A test that could not exceed the bound would prove nothing, so this
	// drives the same SET LOCAL the read drives with a body that cannot finish
	// inside it. The read's own path is exercised through the port below rather
	// than here: reaching INTO readSeries would be reaching past the method
	// that owns the transaction, and a test that did that would still pass if
	// the method stopped opening one.
	err := a.store.WithinTx(t.Context(), func(txCtx context.Context) error {
		if _, err := a.store.Querier(txCtx).ExecContext(txCtx,
			`SET LOCAL statement_timeout = '`+analyticsReadBound+`'`); err != nil {
			return err
		}
		_, err := a.store.Querier(txCtx).QueryContext(txCtx, `SELECT pg_sleep(10)`)
		return err
	})
	if err == nil {
		t.Fatalf("pg_sleep(10) under a %s statement_timeout returned no error; the bound does not bind", analyticsReadBound)
	}
	// The server's own refusal is the evidence that the bound bound. The
	// ADAPTER's classification is deliberately not checked here — this call
	// bypasses classifyRead on its way out, because the test issues the
	// statement itself — and it is checked where the read's own path takes a
	// statement through it.
	if !strings.Contains(err.Error(), "canceling statement due to statement timeout") {
		t.Errorf("pg_sleep(10) under a %s statement_timeout failed with %v, want the server's own timeout refusal — a different error would mean the statement was cut off by something other than the bound", analyticsReadBound, err)
	}

	// And the read itself still works, with its bound in place — a bound that
	// fired on every statement would be a bound that refused the surface.
	usage, _, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: now.Add(-time.Hour), To: now, Buckets: a0(now),
	})
	if err != nil {
		t.Fatalf("Usage() under the same bound: %v", err)
	}
	if len(usage.Series) != 1 {
		t.Errorf("the read returned %d buckets, want 1", len(usage.Series))
	}
}

// TestTheReadRefusesAnUnscopedQuery is the tenancy rule's other half. The port
// has no shape in which a caller can omit the scope, and the repository CHECKS
// rather than assumes: "the caller always passes an account" is a claim about
// every future caller rather than a property of this one, and the empty string
// is the one input a future caller could plausibly pass.
func TestTheReadRefusesAnUnscopedQuery(t *testing.T) {
	a := integrationAnalytics(t)
	now := time.Now().UTC()
	_, _, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: "", From: now.Add(-time.Hour), To: now, Buckets: a0(now),
	})
	if err == nil || !strings.Contains(err.Error(), "no account scope") {
		t.Fatalf("Usage() with no account = %v, want the unscoped refusal — an unscoped read has no predicate to remove", err)
	}
}

// TestTheReadRefusesAnEmptySeries is the other degenerate shape. A range with
// no buckets cannot produce a gap-free series, and returning an empty answer to
// a request that asked for buckets would be indistinguishable from a window in
// which nothing happened.
func TestTheReadRefusesAnEmptySeries(t *testing.T) {
	a := integrationAnalytics(t)
	account, _ := a.newAccountWithBucket(t, "no buckets")
	now := time.Now().UTC()
	_, _, err := a.repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: account, From: now.Add(-time.Hour), To: now, Buckets: nil,
	})
	if err == nil || !strings.Contains(err.Error(), "no buckets") {
		t.Fatalf("Usage() with no buckets = %v, want the refusal", err)
	}
}

// ---------------------------------------------------------------------------
// the attribution
// ---------------------------------------------------------------------------

// TestTheFactAttributionRefusesToRunAutocommitted is the same discipline the
// ingestion tier holds its two writes to. An attribution committed apart from
// the derivation it describes is a row about work that rolled back — or a
// derivation with no account, and the second of those is a request that has
// silently vanished from its customer's report.
func TestTheFactAttributionRefusesToRunAutocommitted(t *testing.T) {
	a := integrationAnalytics(t)
	account, _ := a.newAccountWithBucket(t, "attribution autocommitted")
	err := a.dims.Record(t.Context(), persistence.FactDimension{
		RequestID: string(acctRequestID(t)),
		KindClass: ingestion.ClassSettlement,
		AccountID: account,
		AppendSeq: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "unit of work") {
		t.Fatalf("Record() outside a unit of work = %v, want the refusal", err)
	}
}

// TestTheFactAttributionConvergesOnRedelivery is the exactly-once boundary the
// same key applied_facts enforces. The redelivery that reaches the applied
// ledger reaches this table in the same page, and a second row would be a
// second account for a fact that has exactly one — which would double the
// account's request count on every re-delivered page.
func TestTheFactAttributionConvergesOnRedelivery(t *testing.T) {
	a := integrationAnalytics(t)
	account, _ := a.newAccountWithBucket(t, "attribution convergence")
	dimension := persistence.FactDimension{
		RequestID: string(acctRequestID(t)),
		KindClass: ingestion.ClassSettlement,
		AccountID: account,
		AppendSeq: 7,
	}

	err := a.store.WithinTx(t.Context(), func(txCtx context.Context) error {
		if err := a.dims.Record(txCtx, dimension); err != nil {
			return err
		}
		// The same fact, delivered again, in the same unit of work — the
		// redelivery the applier's page is entitled to produce.
		return a.dims.Record(txCtx, dimension)
	})
	if err != nil {
		t.Fatalf("record the same fact twice in one unit of work: %v", err)
	}

	var rows int
	if err := a.store.Querier(t.Context()).QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM control.analytics_fact_dimensions WHERE request_id = $1`,
		dimension.RequestID).Scan(&rows); err != nil {
		t.Fatalf("count the attribution rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("the redelivered fact produced %d attribution rows, want 1", rows)
	}
}

// TestAFactPaidByTwoAccountsIsAttributedToBoth is the case a single-column key
// cannot hold, and the reason the primary key carries the account.
//
// A settlement's allocation tail can draw on buckets owned by DIFFERENT
// accounts — the waterfall plans per bucket, and funding_buckets_owner_xor
// constrains a BUCKET's owner, never a FACT's set of them. When that happens
// both accounts paid for part of the request, and each one's report is owed the
// request. A key of (request_id, kind_class) with ON CONFLICT DO NOTHING would
// record whichever account was written first and drop the other with no error
// at all, and the failure is invisible in every direction that matters: the
// statement succeeds, the row count for the surviving account is right, and the
// other account is simply short by exactly the requests it funded.
//
// The fixture is two ordinary PAYG accounts with funded buckets, which is the
// general shape — the schema permits a fact to name either, both or neither.
func TestAFactPaidByTwoAccountsIsAttributedToBoth(t *testing.T) {
	a := integrationAnalytics(t)
	first, firstBucket := a.newAccountWithBucket(t, "attribution split first")
	second, secondBucket := a.newAccountWithBucket(t, "attribution split second")
	requestID := string(acctRequestID(t))

	// The derivation resolves BOTH accounts from the two buckets the tail
	// names — the same call the applier makes, so the resolution and the write
	// are proven together rather than one assumed from the other.
	accounts, err := a.dims.AccountsOf(t.Context(), []string{string(firstBucket), string(secondBucket)})
	if err != nil {
		t.Fatalf("AccountsOf() over two accounts' buckets: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("AccountsOf() over two accounts' buckets = %v, want both", accounts)
	}

	err = a.store.WithinTx(t.Context(), func(txCtx context.Context) error {
		for _, accountID := range accounts {
			if err := a.dims.Record(txCtx, persistence.FactDimension{
				RequestID: requestID,
				KindClass: ingestion.ClassSettlement,
				AccountID: accountID,
				AppendSeq: 9,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("record the cross-account attribution: %v", err)
	}

	var rows int
	var scoped string
	if err := a.store.Querier(t.Context()).QueryRowContext(t.Context(),
		`SELECT count(*), count(*) FILTER (WHERE account_id IN ($2, $3))
		   FROM control.analytics_fact_dimensions WHERE request_id = $1`,
		requestID, first, second).Scan(&rows, &scoped); err != nil {
		t.Fatalf("count the cross-account attribution: %v", err)
	}
	if rows != 2 || scoped != "2" {
		t.Errorf("a request funded by two accounts produced %d rows (%q of them for the two accounts), want 2 and 2 — a dropped account is a report short by exactly the requests it funded",
			rows, scoped)
	}
}

// TestTheFactAttributionIsRolledBackWithItsSettlement is the other half of the
// transaction claim, and the half that is easier to get wrong. The attribution
// rides the SAME unit of work as the settlement; a design that recorded it
// afterwards would leave a row describing a settlement that rolled back, and
// the account's request count would include a request that was never billed.
//
// All three writes are inside one unit of work here — the settlement header,
// the applied fact and the attribution — and the applied fact's foreign key is
// what makes that arrangement the only one the schema permits. The unit then
// fails ON PURPOSE, after every write has been issued, and the assertion is
// that none of the three is visible: an attribution that outlived its
// derivation is a request counted and not billed.
func TestTheFactAttributionIsRolledBackWithItsSettlement(t *testing.T) {
	a := integrationAnalytics(t)
	account := a.newAccount(t, "attribution rollback")
	requestID := string(acctRequestID(t))
	settlementID := acctSettlementID(t)
	settled := int64(700)

	errBoom := errors.New("boom")
	err := a.store.WithinTx(t.Context(), func(txCtx context.Context) error {
		if _, err := a.accounting.settlements.Create(txCtx, accounting.Settlement{
			ID:           settlementID,
			RequestID:    accounting.RequestID(requestID),
			SettledTotal: acctAmount(t, 700),
			CreatedAt:    time.Now().UTC(),
		}); err != nil {
			return err
		}
		if err := NewAppliedFacts(a.store).Record(txCtx, persistence.AppliedFact{
			RequestID:     requestID,
			KindClass:     ingestion.ClassSettlement,
			Kind:          ingestion.KindSettled,
			AppendSeq:     1,
			AppliedAt:     time.Now().UTC(),
			SettledAmount: &settled,
			CaptureMethod: capturePtr("reported"),
			SettlementID:  string(settlementID),
		}); err != nil {
			return err
		}
		if err := a.dims.Record(txCtx, persistence.FactDimension{
			RequestID: requestID, KindClass: ingestion.ClassSettlement, AccountID: account, AppendSeq: 1,
		}); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("the failing unit of work returned %v, want the fixture's own error", err)
	}

	var dimensions, applied, settlements int
	if err := a.store.Querier(t.Context()).QueryRowContext(t.Context(),
		`SELECT (SELECT count(*) FROM control.analytics_fact_dimensions WHERE request_id = $1),
		        (SELECT count(*) FROM control.applied_facts WHERE request_id = $1),
		        (SELECT count(*) FROM control.settlements WHERE request_id = $1)`,
		requestID).Scan(&dimensions, &applied, &settlements); err != nil {
		t.Fatalf("count the rolled-back rows: %v", err)
	}
	if dimensions != 0 {
		t.Errorf("the rolled-back unit of work left %d attribution rows, want 0 — an attribution that outlives the derivation it describes counts a request that was never billed", dimensions)
	}
	if applied != 0 || settlements != 0 {
		t.Errorf("the rolled-back unit of work left %d applied facts and %d settlements, want 0 and 0 — the fixture's own unit of work did not roll back whole", applied, settlements)
	}
}

// TestTheAttributionRejectsAnAccountThatDoesNotExist is the NOT NULL
// constraint's real work. The column is NOT NULL because a scope that can
// omit a row is a scope that can be wrong without saying so, and the foreign
// key is what makes "the account exists" part of that claim: a dimension naming
// an account this plane has never heard of would scope to a report nobody can
// ever read.
func TestTheAttributionRejectsAnAccountThatDoesNotExist(t *testing.T) {
	a := integrationAnalytics(t)
	ghost, err := accounting.NewFundingBucketID()
	if err != nil {
		t.Fatalf("mint a ghost id: %v", err)
	}
	err = a.store.WithinTx(t.Context(), func(txCtx context.Context) error {
		return a.dims.Record(txCtx, persistence.FactDimension{
			RequestID: string(acctRequestID(t)),
			KindClass: ingestion.ClassSettlement,
			AccountID: string(ghost),
			AppendSeq: 1,
		})
	})
	if err == nil {
		t.Error("Record() naming an account that does not exist returned no error; the foreign key is not guarding the scope")
	}
}

// TestTheAttributionRejectsAKindClassTheSchemaDoesNotKnow is the other CHECK on
// the table, and it is not duplication: applied_facts owns the pairing between
// a class and a kind, and this table carries the class verbatim rather than
// deriving it. A third value here would be a class the plane has no meaning
// for.
func TestTheAttributionRejectsAKindClassTheSchemaDoesNotKnow(t *testing.T) {
	a := integrationAnalytics(t)
	account, _ := a.newAccountWithBucket(t, "attribution class")
	err := a.store.WithinTx(t.Context(), func(txCtx context.Context) error {
		return a.dims.Record(txCtx, persistence.FactDimension{
			RequestID: string(acctRequestID(t)),
			KindClass: "advisory",
			AccountID: account,
			AppendSeq: 1,
		})
	})
	if err == nil {
		t.Error("Record() with an unknown kind class returned no error; the table's own CHECK is not guarding the grammar")
	}
}

// TestTheAccountResolutionReachesBothBucketOwners is the ingestion side of the
// tenancy rule. A fact's allocation tail can name a PAYG bucket and an
// entitlement bucket at once — a request drawing on an entitlement cycle and
// topping up from the balance — and a resolution that returned only the first
// would attribute the request to one of the accounts that paid for it and drop
// the other. The dropped account's report is then short by exactly the
// requests it funded.
func TestTheAccountResolutionReachesBothBucketOwners(t *testing.T) {
	acct := integrationAccounting(t)

	// The entitlement side owns the account, and the PAYG bucket is created
	// for it below, so the two arms resolve to ONE account and the assertion
	// is that the set has two members rather than that it has a stranger.
	entitled := acct.newEntitlementBucket(t, "resolve entitled")
	account := entitledAccount(t, acct, entitled)

	payg, err := accounting.NewAccountBucket(acctBucketID(t), accounting.AccountID(account), time.Now().UTC())
	if err != nil {
		t.Fatalf("new account bucket: %v", err)
	}
	if err := acct.buckets.Create(t.Context(), payg); err != nil {
		t.Fatalf("create account bucket: %v", err)
	}

	accounts, err := NewFactDimensions(acct.store).AccountsOf(t.Context(),
		[]string{string(payg.ID), string(entitled.ID)})
	if err != nil {
		t.Fatalf("AccountsOf() error = %v", err)
	}
	// One account owns both, so DISTINCT collapses them — and that is the
	// right answer, because the row that is recorded carries that one
	// account. The test is here for the opposite failure: a resolution that
	// returned NOTHING, which is what a single COALESCE between a NULL
	// account_id and a missing subscription row produces.
	if len(accounts) != 1 || accounts[0] != account {
		t.Errorf("AccountsOf() = %v, want exactly [%s] — the entitlement arm resolves through two joins and the direct arm through one, and a resolution that returned neither would drop the fact's account entirely", accounts, account)
	}
}

// TestTheAccountResolutionSpansTwoAccountsWhenTheBucketsDo is the half of the
// case above that a single-account fixture cannot reach: two DIFFERENT
// accounts, one bucket each, and a resolution that returns both. The
// signature returns a set for this and the port's comment names the failure
// this prevents, so the set is proven rather than asserted.
func TestTheAccountResolutionSpansTwoAccountsWhenTheBucketsDo(t *testing.T) {
	acct := integrationAccounting(t)
	first := acct.newAccountBucket(t, "resolve first")
	second := acct.newAccountBucket(t, "resolve second")

	accounts, err := NewFactDimensions(acct.store).AccountsOf(t.Context(),
		[]string{string(first.ID), string(second.ID)})
	if err != nil {
		t.Fatalf("AccountsOf() error = %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("AccountsOf() = %v (%d accounts), want 2 — a fact whose tail names two accounts' buckets belongs to both, and a caller that recorded only the first would drop the second account's report",
			accounts, len(accounts))
	}
	if !slices.Contains(accounts, string(first.AccountID)) || !slices.Contains(accounts, string(second.AccountID)) {
		t.Errorf("AccountsOf() = %v, want it to carry both %s and %s", accounts, first.AccountID, second.AccountID)
	}
}

// TestTheAccountResolutionRefusesAMalformedBucketID is the naming of the
// error. A server-side ::uuid cast failure is a generic SQL message naming a
// table; these ids are runtime-authored text from a fact's allocation tail,
// and a caller that reached this with a malformed one has a bug upstream that
// the cast would hide. The refusal is checked here because the whole reason
// the join is done server-side is that the statement's shape must stay fixed.
func TestTheAccountResolutionRefusesAMalformedBucketID(t *testing.T) {
	a := integrationAnalytics(t)
	if _, err := a.dims.AccountsOf(t.Context(), []string{"not-a-uuid"}); err == nil ||
		!strings.Contains(err.Error(), "not a version-7 uuid") {
		t.Fatalf("AccountsOf() with a malformed id = %v, want the named refusal", err)
	}
}

// TestAnEmptyAllocationTailBelongsToNoAccount is the zero-priced settle: a
// fact whose tail names no legs is a real settlement of record that books no
// capacity, and inventing an account for it would put a request in a
// customer's report that no customer paid for.
func TestAnEmptyAllocationTailBelongsToNoAccount(t *testing.T) {
	a := integrationAnalytics(t)
	for _, tail := range [][]string{nil, {}} {
		accounts, err := a.dims.AccountsOf(t.Context(), tail)
		if err != nil {
			t.Fatalf("AccountsOf(%v) error = %v", tail, err)
		}
		if len(accounts) != 0 {
			t.Errorf("AccountsOf(%v) = %v, want no accounts", tail, accounts)
		}
	}
}

// ---------------------------------------------------------------------------
// the indexes, and the claim that they are load-bearing
// ---------------------------------------------------------------------------

// TestTheMoneyIndexesAreTheReadersAccessPath is the migration's own claim,
// proven by the only method that can prove it: dropping each index inside a
// transaction, re-running the identical statement, and reading the plan.
//
// A plan line naming an index shows only that the planner accepted it. The
// REVERSAL is what shows it was doing the work — a plan that still named the
// index after a drop would mean the drop did nothing, and a plan that was
// already a sequential scan would mean the index was decoration. The
// transaction is rolled back, so the fixture leaves the shared database's schema
// exactly as it found it, which is the only reason this suite may touch DDL at
// all.
//
// THE PLANNER IS TOLD NOT TO SCAN SEQUENTIALLY, and that is the whole reason
// this test can mean anything on a fixture. A fresh database holds a handful of
// rows, and on a handful of rows a sequential scan costs less than an index
// scan — so a plain "the plan names the index" assertion is a test that passes
// on a real deployment and fails on CI, or worse, one an author "fixes" by
// relaxing the assertion until it accepts anything. With enable_seqscan off the
// claim becomes a fact about the SCHEMA rather than about the row count:
//
//   - index present, seqscan disabled  → planner has no choice but the index,
//     so naming it proves the index can serve the range predicate. An index on
//     the wrong column, or a predicate it cannot answer, is still a sequential
//     scan here — the planner says so rather than failing, which is exactly the
//     signal the assertion wants.
//   - index dropped, seqscan disabled  → the planner CANNOT use the index and
//     is not permitted to scan sequentially, so the only plan it can produce is
//     a sequential scan it marks "Disabled: true" — a node it has decided it
//     may not execute. That marker is the proof the index was load-bearing:
//     with the index the statement is executable, and without it the planner
//     has nothing to run.
//
// The second is the stronger of the two and the reason the reversal exists. A
// 90-day report has a fixed 90-day window against a ledger that grows without
// bound, so a scan the index replaced is a scan whose cost grows while the
// statement timeout does not — failing closed forever, which is safe and useless.
//
// A bare count is used rather than the read's real statement because the plan
// shape under test is the range predicate's, and a statement with a LATERAL over
// a VALUES list would make the difference unreadable in a plan listing. The
// speed the migration's own header quotes came from a database seeded with
// eight years of history; what this test establishes is the shape, and the
// shape is what makes those numbers grow.
func TestTheMoneyIndexesAreTheReadersAccessPath(t *testing.T) {
	a := integrationAnalytics(t)
	now := time.Now().UTC()
	from, to := now.Add(-90*24*time.Hour), now

	for _, index := range []string{"settlements_created_at_idx", "ledger_entries_created_at_idx"} {
		t.Run(index, func(t *testing.T) {
			statement := a.moneyStatement(index)

			restored := a.planForcingIndex(t, index, statement, from, to)
			if !strings.Contains(restored, index) {
				t.Errorf("with %s in place and sequential scans disabled the planner still avoided it, so the index cannot serve the range predicate the report's 90-day window runs on:\n%s", index, restored)
			}

			// The reversal, and the half that means something on a fixture with
			// a handful of rows. With the index gone and sequential scans
			// refused, the only plan PostgreSQL can produce is one it is not
			// allowed to execute, and "Disabled: true" is it saying so. The
			// statement is therefore executable with the index and
			// unexecutable without it, which is what "load-bearing" means for
			// a table this plane's ledger appends to forever.
			dropped := a.probeMoneyIndexDropped(t, index, statement, from, to)
			if strings.Contains(dropped.plan, index) {
				t.Errorf("with %s dropped the plan still names it — the drop did not take:\n%s", index, dropped.plan)
			}
			if dropped.plan == "" {
				t.Errorf("with %s dropped the plan was empty; the probe proved nothing", index)
			}
			if !dropped.disabled {
				t.Errorf("with %s dropped and sequential scans disabled the planner still produced a runnable plan:\n%s\nso the index is not what makes the 90-day money series executable — the migration's claim is a claim about decoration", index, dropped.plan)
			}
		})
	}
}

// moneyStatement is the range predicate each money index exists to serve. The
// settlements index and the ledger index cover different tables, and the
// statement is chosen per index so that the plan under test is the one the
// index could serve.
func (a *analyticsRepos) moneyStatement(index string) string {
	if strings.HasPrefix(index, "settlements") {
		return `SELECT count(*) FROM control.settlements WHERE created_at >= $1 AND created_at < $2`
	}
	return `SELECT count(*) FROM control.ledger_entries WHERE created_at >= $1 AND created_at < $2`
}

// moneyIndexProbe is what one index probe found: the plan PostgreSQL produced
// with the index dropped and sequential scans disabled, and whether that plan
// was itself disabled.
//
// A bare string cannot carry both halves. A refusal and an empty plan read the
// same, and the difference matters here: the second says the probe learned
// nothing, the first says the planner declined. A function returning only the
// plan would have to encode "empty means the assertion held" as a rule a reader
// cannot check without re-deriving it, and a function returning only the boolean
// would throw away the plan a failure message needs to show. So the probe returns
// both and the test reads both.
type moneyIndexProbe struct {
	plan string
	// disabled is PostgreSQL's own marker on a plan node it may not execute.
	// It is not this suite's judgement: a Seq Scan line carrying "Disabled:
	// true" is the planner saying it has no other way to run the statement and
	// that the way it does have is the one it was told not to take.
	disabled bool
}

// probeMoneyIndexDropped runs statement with one index dropped, captures the
// outcome, and rolls the drop back. The rollback is what keeps this safe on the
// shared fixture: the index exists again before this function returns, and the
// restored-plan assertion that follows reads it back in place.
//
// Sequential scans are disabled for the capture, so the plan that comes back is
// the index's plan or PostgreSQL's statement that it has nothing else — never a
// sequential scan the small fixture would have made cheaper.
//
// Rolling back rather than re-creating the index is deliberate: a hand-rolled
// CREATE INDEX would be a different index than the migration's, and the
// assertion is about the one the repository ships.
func (a *analyticsRepos) probeMoneyIndexDropped(t *testing.T, index, statement string, from, to time.Time) moneyIndexProbe {
	t.Helper()
	var probe moneyIndexProbe
	err := a.store.WithinTx(t.Context(), func(txCtx context.Context) error {
		if err := refuseSequentialScans(txCtx, a.store); err != nil {
			return err
		}
		if _, err := a.store.Querier(txCtx).ExecContext(txCtx, `DROP INDEX control.`+index); err != nil {
			return fmt.Errorf("drop %s: %w", index, err)
		}
		plan, err := a.explain(txCtx, statement, from, to)
		if err != nil {
			return err
		}
		probe.plan = plan
		probe.disabled = strings.Contains(plan, "Disabled: true")
		return errRollback
	})
	if err != nil && !errors.Is(err, errRollback) {
		t.Fatalf("probe %s: %v", index, err)
	}
	return probe
}

// refuseSequentialScans turns off the sequential-scan plan node for this unit of
// work only.
//
// It is SET LOCAL rather than SET because a session-level setting on a
// connection from a pool outlives the test that set it: the next suite to borrow
// that connection would inherit a planner that cannot choose a sequential scan,
// which is a global change made by a test that only meant to change its own
// probe. The rollback at the end of the transaction would undo a SET LOCAL
// anyway, but the transaction here is also the only thing that keeps the two
// settings from leaking if the probe is ever refactored to run outside one.
func refuseSequentialScans(ctx context.Context, store persistence.Store) error {
	if _, err := store.Querier(ctx).ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		return fmt.Errorf("disable sequential scans: %w", err)
	}
	return nil
}

// planForcingIndex captures statement's plan with the index in place and
// sequential scans disabled, which is what makes the plan a statement about the
// SCHEMA rather than about how many rows a fresh fixture happens to hold.
//
// The window is the read's own maximum — ninety days, the constant the
// surface's range bound is written against — because a plan for a shorter window
// would prove less than a plan for the longest one the surface will ever ask
// for.
func (a *analyticsRepos) planForcingIndex(t *testing.T, index, statement string, from, to time.Time) string {
	t.Helper()
	var plan string
	err := a.store.WithinTx(t.Context(), func(txCtx context.Context) error {
		if err := refuseSequentialScans(txCtx, a.store); err != nil {
			return err
		}
		var err error
		plan, err = a.explain(txCtx, statement, from, to)
		return err
	})
	if err != nil {
		t.Fatalf("explain with %s in place: %v", index, err)
	}
	if plan == "" {
		t.Fatalf("the plan with %s in place and sequential scans disabled was empty; the probe proved nothing", index)
	}
	return plan
}

func (a *analyticsRepos) explain(ctx context.Context, statement string, args ...any) (string, error) {
	rows, err := a.store.Querier(ctx).QueryContext(ctx, "EXPLAIN "+statement, args...)
	if err != nil {
		return "", fmt.Errorf("explain %s: %w", statement, err)
	}
	defer rows.Close()

	plan := ""
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", fmt.Errorf("scan the plan: %w", err)
		}
		plan += line + "\n"
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("walk the plan: %w", err)
	}
	return plan, nil
}

var errRollback = errors.New("rollback the index probe")

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// newAccount opens a commerce account through the identity port, for the
// fixtures that need a scope to attribute to and have no bucket to write a leg
// against.
func (a *analyticsRepos) newAccount(t *testing.T, name string) string {
	t.Helper()
	return string(integrationCommerceAccount(t, a.commerce, name))
}

// newAccountWithBucket opens a commerce account and a PAYG funding bucket for
// it, through the ports, and returns the pair as the port's scope string and
// the bucket's id.
//
// The bucket is created empty and nothing grants it, so a test that reads a
// balance reads zero unless it wrote a leg — the figures an assertion states
// are then the ones the fixture wrote, and a bucket carrying a balance from an
// earlier run would have made every balance assertion relative.
func (a *analyticsRepos) newAccountWithBucket(t *testing.T, name string) (string, accounting.FundingBucketID) {
	t.Helper()
	account := a.newAccount(t, name)
	bucket, err := accounting.NewAccountBucket(acctBucketID(t), accounting.AccountID(account), time.Now().UTC())
	if err != nil {
		t.Fatalf("new account bucket: %v", err)
	}
	if err := NewFundingBuckets(a.store).Create(t.Context(), bucket); err != nil {
		t.Fatalf("create account bucket: %v", err)
	}
	// A bucket with no capacity cannot hold anything, and the ledger's algebra
	// has no overdraw — so an unfunded bucket makes every charge against it
	// fail at the hold rather than at the read under test. The topup is what
	// an account bucket is funded by; a grant would be refused as the
	// transition it is not.
	a.accounting.appendCommitted(t, a.accounting.topupEntry(t, bucket.ID, analyticsTestFund, acctCommandKey(t, "analytics-fund-")))
	return account, bucket.ID
}

// analyticsTestFund is the capacity every fixture bucket is topped up with —
// well above the 700 the charges are made of, so a test that books several
// charges on one bucket is not refused for capacity it did not mean to test.
//
// The CHARGE amounts are not this round, or every money assertion in this file
// would be satisfied by a read that divided by a hundred and multiplied back;
// the topup is a fixture of capacity rather than of a settled figure, and no
// assertion below reads a sum of it for money spent.
const analyticsTestFund = 100_000

// capturePtr is the address of one capture method, for the applied facts whose
// shape carries one. A pointer is what the port asks for so that "this kind
// claims no capture method" is distinct from "this kind claims the empty
// string", and a fixture that passed an empty string would be asserting a
// shape the schema refuses.
func capturePtr(method string) *string { return &method }

// heldConsume performs the two legs a real charge is made of: a hold that
// reserves capacity, and the consume that spends it. A consume is refused
// without a matching held reservation — `insufficient held balance` — because
// the ledger's algebra has no overdraw, so any fixture that appends a bare
// consume is not exercising a shape the system can produce.
//
// The reservation is minted per call so two charges on one bucket do not
// collide on the hold's uniqueness.
func (a *analyticsRepos) heldConsume(ctx context.Context, t *testing.T, bucket accounting.FundingBucketID, raw int64, settlementID accounting.SettlementID) error {
	t.Helper()
	reservation := acctReservation(t)
	if _, _, err := a.accounting.ledger.Append(ctx, a.accounting.holdEntry(t, bucket, raw, reservation)); err != nil {
		return err
	}
	_, _, err := a.accounting.ledger.Append(ctx, a.accounting.consumeEntry(t, bucket, raw, settlementID))
	return err
}

// settleRequest books one settled request of raw minor units against the
// account's bucket, in the single unit of work the applier uses: the ledger's
// consume leg, the settlement header, the applied fact and the analytics
// attribution, all four or none.
//
// Everything goes through the ports, so a fixture that bypassed one would be
// asserting against a shape the application never produces — and the
// attribution in particular is only meaningful if it was written by the same
// statement the applier writes.
func (a *analyticsRepos) settleRequest(t *testing.T, account string, bucket accounting.FundingBucketID, raw int64, capture string) accounting.RequestID {
	t.Helper()
	acct := a.accounting
	requestID := acctRequestID(t)
	settlementID := acctSettlementID(t)
	settled := raw

	if _, err := acct.settlements.Create(t.Context(), accounting.Settlement{
		ID:           settlementID,
		RequestID:    requestID,
		SettledTotal: acctAmount(t, raw),
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create the settlement: %v", err)
	}

	err := acct.store.WithinTx(t.Context(), func(txCtx context.Context) error {
		if err := a.heldConsume(txCtx, t, bucket, raw, settlementID); err != nil {
			return err
		}
		if err := NewAppliedFacts(acct.store).Record(txCtx, persistence.AppliedFact{
			RequestID:     string(requestID),
			KindClass:     ingestion.ClassSettlement,
			Kind:          ingestion.KindSettled,
			AppendSeq:     1,
			AppliedAt:     time.Now().UTC(),
			SettledAmount: &settled,
			CaptureMethod: capturePtr(capture),
			SettlementID:  string(settlementID),
		}); err != nil {
			return err
		}
		return a.dims.Record(txCtx, persistence.FactDimension{
			RequestID: string(requestID),
			KindClass: ingestion.ClassSettlement,
			AccountID: account,
			AppendSeq: 1,
		})
	})
	if err != nil {
		t.Fatalf("record the settled request: %v", err)
	}
	return requestID
}

// releaseRequest books the release half of a request that was never settled —
// a held reservation that expired back to capacity. It carries no settled
// amount and no capture method, which is the shape the applied table's CHECK
// pins, and it is the row a capture split counted over facts would have
// carried into its denominator.
func (a *analyticsRepos) releaseRequest(t *testing.T, account string, bucket accounting.FundingBucketID, raw int64) {
	t.Helper()
	acct := a.accounting
	requestID := acctRequestID(t)
	settlementID := acctSettlementID(t)
	reservation := acctReservation(t)

	if _, err := acct.settlements.Create(t.Context(), accounting.Settlement{
		ID:           settlementID,
		RequestID:    requestID,
		SettledTotal: acctAmount(t, raw),
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create the settlement: %v", err)
	}

	// The hold is inside the unit of work rather than beside it, so the pair
	// is one fact about the request rather than two that could disagree. A
	// release names the hold it releases, so a hold committed elsewhere would
	// make this a release of capacity that is not held.
	err := acct.store.WithinTx(t.Context(), func(txCtx context.Context) error {
		if _, _, err := acct.ledger.Append(txCtx, acct.holdEntry(t, bucket, raw, reservation)); err != nil {
			return err
		}
		if _, _, err := acct.ledger.Append(txCtx, acct.releaseEntry(t, bucket, raw, reservation, settlementID)); err != nil {
			return err
		}
		if err := NewAppliedFacts(acct.store).Record(txCtx, persistence.AppliedFact{
			RequestID: string(requestID),
			KindClass: ingestion.ClassSettlement,
			Kind:      ingestion.KindReleased,
			AppendSeq: 1,
			AppliedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		return a.dims.Record(txCtx, persistence.FactDimension{
			RequestID: string(requestID),
			KindClass: ingestion.ClassSettlement,
			AccountID: account,
			AppendSeq: 1,
		})
	})
	if err != nil {
		t.Fatalf("record the released request: %v", err)
	}
}

// orphanRequest records a fact of the OTHER class for a request that has no
// settlement: an unbillable orphan, which carries a capture method and books
// no charge. It is the row a split over facts would have counted, and the
// reason the population is kind = 'settled' rather than "facts with a capture
// method".
func (a *analyticsRepos) orphanRequest(t *testing.T, account string, requestID accounting.RequestID) {
	t.Helper()
	acct := a.accounting
	err := acct.store.WithinTx(t.Context(), func(txCtx context.Context) error {
		if err := NewAppliedFacts(acct.store).Record(txCtx, persistence.AppliedFact{
			RequestID:     string(requestID),
			KindClass:     ingestion.ClassUnbillableOrphaned,
			Kind:          ingestion.KindUnbillableOrphaned,
			AppendSeq:     1,
			AppliedAt:     time.Now().UTC(),
			CaptureMethod: capturePtr("reported"),
		}); err != nil {
			return err
		}
		return a.dims.Record(txCtx, persistence.FactDimension{
			RequestID: string(requestID),
			KindClass: ingestion.ClassUnbillableOrphaned,
			AccountID: account,
			AppendSeq: 1,
		})
	})
	if err != nil {
		t.Fatalf("record the orphan: %v", err)
	}
}

// hourlyBuckets is count consecutive hour-long buckets beginning at from —
// the shape the surface uses at its finest grain, and the one whose bucket
// count a caller would have to get right.
func (a *analyticsRepos) hourlyBuckets(from time.Time, count int) []analytics.Bucket {
	buckets := make([]analytics.Bucket, 0, count)
	for i := range count {
		start := from.Add(time.Duration(i) * time.Hour)
		buckets = append(buckets, analytics.Bucket{Start: start, End: start.Add(time.Hour)})
	}
	return buckets
}

// a0 is the one-bucket series, for the tests that are about a total rather
// than about a series. One bucket spanning an hour is the minimum the read
// will accept, because a read with no buckets is refused.
func a0(now time.Time) []analytics.Bucket {
	return []analytics.Bucket{{
		Start: now.Add(-time.Hour).Truncate(time.Hour),
		End:   now.Add(-time.Hour).Truncate(time.Hour).Add(time.Hour),
	}}
}

// entitledAccount reads the account behind an entitlement-funded bucket,
// through the two joins the account set expression makes. It is the database's
// own answer, so a fixture that paired two buckets across two accounts would
// be caught by the test's own assertion rather than silently accepted.
func entitledAccount(t *testing.T, acct *accountingRepos, bucket accounting.Bucket) string {
	t.Helper()
	var account string
	if err := acct.db.QueryRowContext(t.Context(), `
		SELECT s.account_id::text
		FROM control.funding_buckets b
		JOIN control.entitlements e ON e.id = b.entitlement_id
		JOIN control.subscriptions s ON s.id = e.subscription_id
		WHERE b.id = $1`, bucket.ID).Scan(&account); err != nil {
		t.Fatalf("read the entitlement bucket's account: %v", err)
	}
	return account
}

// paygAccount reads a PAYG bucket's account directly, so the fixture's own
// claim about which account it belongs to is checked against the table rather
// than assumed from the argument that created it.
func paygAccount(t *testing.T, acct *accountingRepos, bucket accounting.Bucket) string {
	t.Helper()
	var account string
	if err := acct.db.QueryRowContext(t.Context(),
		`SELECT account_id::text FROM control.funding_buckets WHERE id = $1`, bucket.ID).Scan(&account); err != nil {
		t.Fatalf("read the PAYG bucket's account: %v", err)
	}
	if account == "" {
		t.Fatal("the bucket has no account_id; it is an entitlement bucket and this fixture asked for the direct arm")
	}
	return account
}

func renderSeries(series []persistence.Bucket, buckets []analytics.Bucket) string {
	out := ""
	for i := range series {
		asked := "(beyond the requested buckets)"
		if i < len(buckets) {
			asked = "[" + buckets[i].Start.UTC().Format(time.RFC3339) + ", " + buckets[i].End.UTC().Format(time.RFC3339) + ")"
		}
		out += "  " + asked + " -> with_facts=" +
			strconv.FormatInt(series[i].WithUsageFacts, 10) +
			" settled=" + strconv.FormatInt(series[i].Settled, 10) + "\n"
	}
	if out == "" {
		return "  (no buckets at all)\n"
	}
	return out
}
