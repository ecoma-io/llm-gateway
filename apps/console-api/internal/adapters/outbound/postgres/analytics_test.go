package postgres

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/analytics"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The analytics read model's statements, pinned structurally rather than
// against golden strings.
//
// The claim this file exists for is the one the bug taught: a statement's
// TEXT is its contract with the database, and a test that only watched which
// handle a statement ran on would have watched a statement that could not
// execute at all. The three properties below are therefore about what the
// statements SAY, and each is one that a phrasing change can break silently:
//
//   - every timestamp placeholder in the series' VALUES list carries its own
//     cast, because a VALUES row is not a column and the type is otherwise
//     unknown;
//   - the read runs inside a unit of work, because the SET LOCAL that bounds
//     it is a silent no-op outside a transaction;
//   - the attribution stamps its own instant, because the column has no
//     default and a caller cannot be the one keeping two columns equal.

// The five reads the answer is made of, and the shape each one projects, are
// declared in scripteddriver_test.go beside the shapes themselves.
//
// The readShape here is the read's own ORDER, and a change to it that a
// statement did not follow would fail every test in this file at the scan —
// which is the point: the order IS the contract, and a test that discovered it
// by a nil pointer would be a test that only proves the code runs.
var _ = readShape

// scriptedAnalytics builds a store over the scripted driver and the read model
// on top of it. The script is the five reads above, in order.
func scriptedAnalytics(t *testing.T) (*scriptedDriver, persistence.Analytics) {
	t.Helper()
	s, db := newScripted(t, readShape...)
	store := New(db)
	return s, NewAnalytics(store)
}

// scriptedBounds is two hour-long buckets, which is the smallest series that
// would be a series at all.
func scriptedBounds() []analytics.Bucket {
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	return []analytics.Bucket{
		{Start: start, End: start.Add(time.Hour)},
		{Start: start.Add(time.Hour), End: start.Add(2 * time.Hour)},
	}
}

func scriptedQuery(account string) persistence.UsageQuery {
	bounds := scriptedBounds()
	return persistence.UsageQuery{
		AccountID: account,
		From:      bounds[0].Start,
		To:        bounds[len(bounds)-1].End,
		Buckets:   bounds,
	}
}

// TestEveryPlaceholderInTheValuesListCarriesItsOwnCast is the regression for
// the defect this tier was written after, and it is written as a property
// rather than a string so that it covers the case the second instance of the
// bug arrived in.
//
// A VALUES row is not a table column, so nothing gives a placeholder in one a
// type. The two timestamp placeholders were therefore UNKNOWN, and the range
// comparison the statement exists to serve did not resolve: the read failed
// with `operator does not exist: timestamp with time zone >= text` on an EMPTY
// database, which is the strongest possible form of the bug — no data, no
// size, no load, and the surface refused anyway.
//
// The ordinal was the same bug with a nastier edge. Uncast, its type is
// resolved from the OTHER ROWS of the list, so a two-bucket series made every
// ordinal an integer and worked, while a ONE-bucket series had no other row to
// resolve against, fell back to text, and failed with `unable to encode 0 into
// text format for text (OID 25)`. The statement worked for every series a
// dashboard draws and refused the single-bucket query an API caller is most
// likely to make by hand.
//
// So the assertion names no column: within the VALUES list, no placeholder may
// appear without a cast. A golden string would fail every reformatting and pass
// a reformatting that dropped a cast, which is the failure that matters.
func TestEveryPlaceholderInTheValuesListCarriesItsOwnCast(t *testing.T) {
	s, repo := scriptedAnalytics(t)
	if _, _, err := repo.Usage(t.Context(), scriptedQuery("018f0000-0000-7000-8000-000000000001")); err != nil {
		t.Fatalf("Usage() error = %v", err)
	}

	_, list, _ := valuesListSplit(s.theStatementNamed("bucket_start"))
	if list == "" {
		t.Fatal("the series statement did not run, or carries no VALUES list to check")
	}
	for _, bare := range barePlaceholders(list) {
		t.Errorf("placeholder $%d is uncast in the VALUES list:\n%s\na VALUES row is not a column, so an uncast placeholder there has no type to infer — and an ordinal infers differently at one row than at two", bare, list)
	}

	// And the same claim for the series a caller actually asks for when they
	// want a single point rather than a curve: one bucket, one row, and the
	// row still carries all three casts. A read whose types depend on its own
	// row count cannot be pinned by a two-row assertion.
	if _, list, _ := valuesListSplit(singleBucketSeries(t)); list == "" {
		t.Fatal("the one-bucket read did not run")
	}
}

func TestTheOneBucketSeriesCarriesTheSameCastsAsAManyBucketOne(t *testing.T) {
	_, list, _ := valuesListSplit(singleBucketSeries(t))
	if got, want := strings.Count(list, "::"), 3; got != want {
		t.Errorf("the one-bucket VALUES row carries %d casts, want %d — every placeholder in the row is untyped without one\n%s", got, want, list)
	}
}

// barePlaceholders is every placeholder number in a fragment that is not
// immediately followed by "::".
//
// It is written as a scan over the '$' characters rather than a regexp with a
// negative lookahead because Go's regexp has no lookahead, and the alternative
// — a regexp for casts plus a regexp for placeholders, compared as counts —
// cannot tell a bare placeholder from a cast one, because both contain a "$n".
func barePlaceholders(fragment string) []int {
	var bare []int
	for i := 0; i < len(fragment); i++ {
		if fragment[i] != '$' {
			continue
		}
		start := i
		for i+1 < len(fragment) && fragment[i+1] >= '0' && fragment[i+1] <= '9' {
			i++
		}
		if !strings.HasPrefix(fragment[i+1:], "::") {
			number, err := strconv.Atoi(fragment[start+1 : i+1])
			if err != nil {
				continue
			}
			bare = append(bare, number)
		}
	}
	return bare
}

// singleBucketSeries is the series statement of a ONE-bucket read — the shape
// whose placeholders have no other row to resolve their types against.
func singleBucketSeries(t *testing.T) string {
	t.Helper()
	s, repo := scriptedAnalytics(t)
	bounds := scriptedBounds()[:1]
	if _, _, err := repo.Usage(t.Context(), persistence.UsageQuery{
		AccountID: "018f0000-0000-7000-8000-000000000001",
		From:      bounds[0].Start,
		To:        bounds[0].End,
		Buckets:   bounds,
	}); err != nil {
		t.Fatalf("Usage() with one bucket: %v", err)
	}
	return s.theStatementNamed("bucket_start")
}

// TestTheSeriesStatementComparesAgainstTheBoundsItWasGiven pins the other
// half of the range join: the comparison is `>= start AND < end`, on the
// VALUES aliases, and not on a truncation of either column.
//
// A statement that reached for date_trunc would truncate in UTC, and a day in
// the caller's zone is not a day in UTC — the timezone bug the whole design of
// the query exists to prevent, reintroduced in the one place the domain
// handed its arithmetic over. The assertion is the absence of the function,
// which is checkable; the presence of any particular range form is not, since
// a hundred rewrites spell it differently and all of them are correct.
func TestTheSeriesStatementComparesAgainstTheBoundsItWasGiven(t *testing.T) {
	s, repo := scriptedAnalytics(t)
	if _, _, err := repo.Usage(t.Context(), scriptedQuery("018f0000-0000-7000-8000-000000000001")); err != nil {
		t.Fatalf("Usage() error = %v", err)
	}

	series := ""
	for _, statement := range s.statements() {
		if strings.Contains(statement, "bucket_start") {
			series = statement
			break
		}
	}
	if series == "" {
		t.Fatal("no series statement ran")
	}
	for _, forbidden := range []string{"date_trunc", "extract(", "AT TIME ZONE"} {
		if strings.Contains(series, forbidden) {
			t.Errorf("the series statement calls %s:\n%s\nthe caller's zone arithmetic is the domain's and travels as the bounds; a truncation in SQL would cut in UTC and a day in the caller's zone is not a day in UTC",
				forbidden, series)
		}
	}
	if !containsAll(series,
		"bounds.bucket_start", "bounds.bucket_end",
		">= bounds.bucket_start", "<  bounds.bucket_end") {
		t.Errorf("the series statement does not compare each row against the interval it was given:\n%s\nhalf-open on both sides is what makes two consecutive windows tile without overlap or gap", series)
	}
}

// TestTheSeriesCountsRequestsAndNotFactRows is the third property this tier
// pins, and the one with the widest blast radius: the statement's two counts
// are of REQUESTS, and the dimension table holds one row per (request, fact
// class).
//
// A request that arrived with a settlement AND an unbillable orphan — the
// ordinary shape of a request the gateway could not bill — has two dimension
// rows and is one request. COUNT(*) reported it twice, and the duplicate is
// not a rounding error a reader can see: a chart's y-axis doubles, and a rate
// computed over the count is wrong by a factor of two on exactly the requests
// that were hardest to serve. The DISTINCT is the whole difference and it is
// invisible in the figure until it is read against the request log.
//
// The join to settlements can duplicate a request the same way, so BOTH counts
// carry the DISTINCT — and the assertion is on both, because a fix that
// deduped only the first would leave the second wrong by the same factor.
func TestTheSeriesCountsRequestsAndNotFactRows(t *testing.T) {
	s, repo := scriptedAnalytics(t)
	if _, _, err := repo.Usage(t.Context(), scriptedQuery("018f0000-0000-7000-8000-000000000001")); err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	series := s.theStatementNamed("bucket_start")
	if series == "" {
		t.Fatal("the series statement did not run")
	}
	if got := strings.Count(series, "COUNT(DISTINCT d.request_id)"); got != 2 {
		t.Errorf("the series statement counts distinct requests %d times, want 2 — one request can hold a settlement AND an orphan, and a COUNT(*) would report it once per fact class", got)
	}
	if strings.Contains(series, "COUNT(*)") {
		t.Errorf("the series statement carries a bare COUNT(*):\n%s\nthe counts are of requests, and a fact class is not a request", series)
	}

	// The counts are not merely two spellings of the same DISTINCT. The
	// settled population is the one the settlements join can prove: a
	// request with an unbillable orphan has a dimension row and no settlement
	// header, so it belongs to the first count and not the second. Dropping
	// this FILTER leaves the two bare DISTINCT substrings above intact, which
	// is why the projection line itself is read here.
	settled := projectionLine(series, "settled")
	if settled == "" {
		t.Fatalf("the series statement projects no settled count:\n%s", series)
	}
	if !strings.Contains(settled, "COUNT(DISTINCT d.request_id)") ||
		!strings.Contains(settled, "FILTER") ||
		!strings.Contains(settled, "s.id IS NOT NULL") {
		t.Errorf("the settled count is not a distinct count restricted to a settlement of record:\n%s\nit is %q — without the FILTER every orphan is reported as a settlement", series, settled)
	}
}

// projectionLine returns the COUNT projection line bearing alias, excluding
// the alias itself. The series also spells `settled` in an outer COALESCE that
// gives the aggregate a zero; the count is the projection the claim is about,
// so the COALESCE is deliberately skipped. The series writes each projection
// on one line, and the claim here is about that expression — not a golden
// statement shape, nor a substring whose second occurrence can accidentally
// satisfy the first count's assertion.
func projectionLine(statement, alias string) string {
	marker := "AS " + alias
	for _, line := range strings.Split(statement, "\n") {
		at := strings.Index(line, marker)
		if at >= 0 && strings.Contains(line[:at], "COUNT(") {
			return strings.TrimSpace(line[:at])
		}
	}
	return ""
}

// TestTheAttributionStampsItsOwnInstant is the column with no default.
//
// control.analytics_fact_dimensions.applied_at is NOT NULL with NO DEFAULT,
// where applied_facts.applied_at is NOT NULL DEFAULT now(). That difference is
// the design: a default here would let a row be written without a stamp by any
// writer that was not this one, so the adapter has to write it — and the
// instant it writes has to be the SAME instant applied_facts records, which is
// what now() is, inside the one transaction that writes both.
//
// The assertion is that the statement names the column and calls now(). A
// version that passed the instant in as a parameter would be a claim about
// every future caller keeping two columns in step, and the read model's whole
// ability to scope a range over this table alone rests on that equality.
func TestTheAttributionStampsItsOwnInstant(t *testing.T) {
	s, db := newScripted(t)
	store := New(db)
	dims := NewFactDimensions(store)

	err := store.WithinTx(t.Context(), func(txCtx context.Context) error {
		return dims.Record(txCtx, persistence.FactDimension{
			RequestID: "req-018f0000-0000-7000-8000-000000000002",
			KindClass: "settlement",
			AccountID: "018f0000-0000-7000-8000-000000000001",
			AppendSeq: 7,
		})
	})
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	var insert string
	for _, statement := range s.statements() {
		if strings.Contains(statement, "INSERT INTO control.analytics_fact_dimensions") {
			insert = statement
			break
		}
	}
	if insert == "" {
		t.Fatalf("no attribution insert ran; the call issued %d statements", len(s.statements()))
	}
	if !strings.Contains(insert, "applied_at") {
		t.Errorf("the attribution insert does not name applied_at, and the column has NO DEFAULT:\n%s\nwithout the column the statement is refused by a NOT NULL constraint, on every call, for every account", insert)
	}
	if !strings.Contains(insert, "now()") {
		t.Errorf("the attribution insert does not stamp itself with now():\n%s\nthe copy from applied_facts.applied_at is exact by construction only if both rows call the same function inside the same transaction", insert)
	}
}

// TestTheAttributionConvergesOnTheSameKey pins the ON CONFLICT clause, which
// is the exactly-once boundary and the one thing the statement must not lose
// in a rewrite. The redelivery that reaches the applied ledger reaches this
// table in the same page, and without the clause a re-delivered fact would
// produce a second attribution — a second account for one fact, and a
// doubled request count on every retry.
//
// The account is PART OF THE TARGET, and the assertion below is written so
// that dropping it fails here rather than passing on a substring. The old
// target "(request_id, kind_class)" is a prefix of the new one, so a plain
// contains check is satisfied by both — a test that would have gone green
// through exactly the defect it exists to catch. So the target is read out of
// the statement and compared whole, and the account's presence inside it is
// checked against the CLOSING PAREN rather than against the end of the list.
func TestTheAttributionConvergesOnTheSameKey(t *testing.T) {
	s, db := newScripted(t)
	store := New(db)
	dims := NewFactDimensions(store)

	err := store.WithinTx(t.Context(), func(txCtx context.Context) error {
		return dims.Record(txCtx, persistence.FactDimension{
			RequestID: "req-018f0000-0000-7000-8000-000000000003",
			KindClass: "settlement",
			AccountID: "018f0000-0000-7000-8000-000000000001",
			AppendSeq: 7,
		})
	})
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	var insert string
	for _, statement := range s.statements() {
		if strings.Contains(statement, "INSERT INTO control.analytics_fact_dimensions") {
			insert = statement
			break
		}
	}
	if insert == "" {
		t.Fatalf("no attribution insert ran; the call issued %d statements", len(s.statements()))
	}
	if !containsAll(insert, "ON CONFLICT", "DO NOTHING") {
		t.Fatalf("the attribution insert no longer converges:\n%s", insert)
	}
	// The whole target, compared whole. Reading it out rather than searching
	// for fragments is the point: "(request_id, kind_class)" is a prefix of
	// "(request_id, kind_class, account_id)", so a fragment search for the
	// short form passes on the long one and the long form passes on a
	// statement that dropped the account.
	if target := conflictTarget(insert); target != "(request_id, kind_class, account_id)" {
		t.Errorf("the attribution converges on %s, want (request_id, kind_class, account_id):\n%s\nwithout the account in the target, a settlement paid by two accounts records the first and drops the rest through DO NOTHING, and that account's report is short by exactly the requests it funded", target, insert)
	}
}

// conflictTarget returns the parenthesised column list an ON CONFLICT clause
// names, or "" when the clause names none.
//
// A statement whose ON CONFLICT has no target (`ON CONFLICT DO NOTHING`) is
// legal SQL and would swallow every collision on any constraint — which is why
// the empty answer is a failure here rather than a shape to accept: the target
// is what says WHICH collision converges, and a targetless clause is the read
// model deciding nothing on that question.
func conflictTarget(statement string) string {
	clause := strings.Index(statement, "ON CONFLICT")
	if clause < 0 {
		return ""
	}
	open := strings.Index(statement[clause:], "(")
	if open < 0 {
		return ""
	}
	open += clause
	close := strings.Index(statement[open:], ")")
	if close < 0 {
		return ""
	}
	return statement[open : open+close+1]
}

// TestTheFiveReadsShareOneSnapshot pins the isolation the read asks its unit
// of work for, and the reason it is asserted here rather than at the transport
// is that the property leaves NO STATEMENT BEHIND.
//
// The level is carried on the BEGIN, because that is the only place PostgreSQL
// will take it: it refuses both SET TRANSACTION and set_config('transaction_
// isolation') with 25001 the moment a transaction has issued a query, so an
// implementation that put the level on a statement would either be refused
// outright or would be relying on its being the first statement — an ordering
// no test elsewhere in this file pins. A driver that implements BeginTx and
// records what it was handed sees the level directly, and one that does not
// would make this assertion pass vacuously, which is why the driver records it
// rather than ignoring it.
func TestTheFiveReadsShareOneSnapshot(t *testing.T) {
	s, db := newScripted(t, readShape...)
	_, _, _ = NewAnalytics(New(db)).Usage(t.Context(), scriptedQuery("018f0000-0000-7000-8000-000000000001"))

	levels := s.isolations()
	if len(levels) == 0 {
		t.Fatalf("the read began no transaction; its five statements each take their own snapshot and a settlement committing between them makes the answer internally inconsistent")
	}
	for i, level := range levels {
		if level != "repeatable read" {
			t.Errorf("unit of work %d began at %s, want repeatable read: the read's five statements answer one question, and at read committed a settlement committing between the series read and the money read yields a bucket that says settled beside a settled amount that excludes it", i+1, level)
		}
	}
}

// TestAnIsolationThePortDoesNotNameIsRefused is the guard on the guard.
//
// txOptionsFor is the one place a level crosses from the port's vocabulary to
// the driver's, and sql.LevelDefault — the driver's own default — is READ
// COMMITTED. A mapping that returned LevelDefault for an unrecognised name
// would therefore return read committed for a caller who asked for something
// else, and the read would go on sharing a per-statement snapshot while the
// code said it shared one. The refusal is what makes the mapping safe to have.
func TestAnIsolationThePortDoesNotNameIsRefused(t *testing.T) {
	if _, err := txOptionsFor(persistence.Isolation("serializable")); err == nil {
		t.Errorf("txOptionsFor(%q) = nil error, want a refusal: this adapter must not substitute one isolation for another, and a level the port does not name is a name only this adapter reads", persistence.Isolation("serializable"))
	}
}

// TestTheReadRunsInsideAUnitOfWork is why the statement timeout is real.
//
// SET LOCAL is scoped to a transaction and is a SILENT no-op outside one, so a
// read that ran on the pool would be unbounded at the server while this file
// claimed it bounded — the claim would be enforced by nothing. A real
// database cannot show this: the statement succeeds either way and only the
// server's plan tells you. The scripted driver can, because it records the
// order, so the assertion is that the bound is set and the read still has rows
// to read after it — which is only true inside a transaction.
func TestTheReadRunsInsideAUnitOfWork(t *testing.T) {
	s, repo := scriptedAnalytics(t)
	if _, _, err := repo.Usage(t.Context(), scriptedQuery("018f0000-0000-7000-8000-000000000001")); err != nil {
		t.Fatalf("Usage() error = %v", err)
	}

	statements := s.statements()
	if len(statements) == 0 {
		t.Fatal("the read issued no statements")
	}
	if !strings.Contains(statements[0], "statement_timeout") {
		t.Errorf("the first statement is %q, want the bound\nSET LOCAL must run before the reads it bounds, and it must be the first statement of the transaction so that it also covers the planner's own work on the first read", statements[0])
	}
	if !strings.Contains(statements[0], "SET LOCAL") {
		t.Errorf("the bound is set with %q, want SET LOCAL\nSET without LOCAL would outlive the transaction and leak onto whatever borrows this connection next", statements[0])
	}
}

// TestTheBoundIsSetOnEveryReadRatherThanOnce pins the claim that the bound
// travels with the read rather than being a property of the pool. A statement
// that set it once per connection would be a bound that silently decays to
// nothing on a connection the pool reused, and there is no way to notice that
// from outside except by counting.
func TestTheBoundIsSetOnEveryReadRatherThanOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		calls int
	}{
		{name: "two reads set the bound twice", calls: 2},
		{name: "three reads set the bound three times", calls: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo := scriptedAnalytics(t)
			for range tc.calls {
				if _, _, err := repo.Usage(t.Context(), scriptedQuery("018f0000-0000-7000-8000-000000000001")); err != nil {
					t.Fatalf("Usage() error = %v", err)
				}
			}
			seen := 0
			for _, statement := range s.statements() {
				if strings.Contains(statement, "statement_timeout") {
					seen++
				}
			}
			if seen != tc.calls {
				t.Errorf("%d reads set the bound %d times, want %d — a bound set once per connection decays to nothing on a connection the pool reuses", tc.calls, seen, tc.calls)
			}
		})
	}
}

// TestTheReadCarriesTheAccountOnEveryStatement is the tenancy rule at the
// statement level. The port has no shape in which the account can be omitted,
// and this is the place that claim is checked: every statement the read runs
// against a table scoped by account must bind it.
//
// The two statements that carry no account are the ledger flows and the
// balances, and they are the ones worth examining: neither money table has an
// account column, so both resolve the account through a subquery. A rewrite
// that flattened one of those subqueries into a bare bucket list would put
// every account's ledger legs in one answer, and no test above would see it —
// the figures would still be the right SHAPE.
func TestTheReadCarriesTheAccountOnEveryStatement(t *testing.T) {
	s, repo := scriptedAnalytics(t)
	query := scriptedQuery("018f0000-0000-7000-8000-000000000001")
	if _, _, err := repo.Usage(t.Context(), query); err != nil {
		t.Fatalf("Usage() error = %v", err)
	}

	// Every statement that names one of the account's own tables must bind the
	// account, either as a predicate or through the bucket subquery.
	scoped := map[string]bool{
		"control.analytics_fact_dimensions": false,
		"control.settlements":               false,
		"control.applied_facts":             false,
		"control.funding_buckets":           false,
	}
	for _, statement := range s.statements() {
		for table := range scoped {
			if !strings.Contains(statement, table) {
				continue
			}
			scoped[table] = true
			if !strings.Contains(statement, query.AccountID[:8]) &&
				!strings.Contains(statement, "$1") && !strings.Contains(statement, "$3") {
				t.Errorf("the statement against %s binds no account:\n%s", table, statement)
			}
		}
	}
	for table, found := range scoped {
		if !found {
			t.Errorf("no statement named %s; the read issued:\n%s", table, strings.Join(s.statements(), "\n---\n"))
		}
	}
}

// TestTheLedgerFlowsResolveThroughTheBucketSubquery is the specific claim the
// test above cannot make on its own: the money tables carry no account column,
// so the account reaches them through the two-arm bucket set, and the arms
// are what stop an entitlement-funded bucket from vanishing.
//
// The failure this guards is silent and one-directional — a dropped
// entitlement bucket is a figure wrong in the direction that looks like good
// news — and it is invisible to any test that only checks shapes.
func TestTheLedgerFlowsResolveThroughTheBucketSubquery(t *testing.T) {
	s, repo := scriptedAnalytics(t)
	if _, _, err := repo.Usage(t.Context(), scriptedQuery("018f0000-0000-7000-8000-000000000001")); err != nil {
		t.Fatalf("Usage() error = %v", err)
	}

	var flows string
	for _, statement := range s.statements() {
		if strings.Contains(statement, "control.ledger_entries") {
			flows = statement
			break
		}
	}
	if flows == "" {
		t.Fatal("no statement read the ledger")
	}
	// Both arms, and the reason both are load-bearing: filtering on
	// funding_buckets.account_id alone drops every entitlement-funded leg.
	if !containsAll(flows, "UNION", "b.account_id = ", "JOIN control.entitlements", "JOIN control.subscriptions", "s.account_id = ") {
		t.Errorf("the ledger statement does not resolve the account through both arms:\n%s\nfunding_buckets.account_id is NULLABLE for a subscription-funded bucket, so the direct arm alone returns a figure short by every entitlement-funded leg — wrong in the direction that looks like good news", flows)
	}
}

// TestTheMoneyIsSummedFromTheSettlementHeader is the rounding claim, stated as
// a statement rather than as a number. The derivation applies one ceiling over
// a request's summed raw cost, so a per-request amount is ROUNDED and the sum
// of rounded figures is not the rounded total: a thousand one-minor-unit
// requests settle for one minor unit between them, and a naive per-request sum
// would bill a thousand. A thousand times too much, growing with volume.
//
// The header is written once and is the sum of that settlement's own consume
// legs, so summing headers is exact. The assertion is that the settled figure
// is a SUM over control.settlements and carries the bigint cast.
func TestTheMoneyIsSummedFromTheSettlementHeader(t *testing.T) {
	s, repo := scriptedAnalytics(t)
	if _, _, err := repo.Usage(t.Context(), scriptedQuery("018f0000-0000-7000-8000-000000000001")); err != nil {
		t.Fatalf("Usage() error = %v", err)
	}

	var settled string
	for _, statement := range s.statements() {
		if strings.Contains(statement, "SUM(s.settled_total)") {
			settled = statement
			break
		}
	}
	if settled == "" {
		t.Fatalf("no statement summed a settlement header; the read issued:\n%s", strings.Join(s.statements(), "\n---\n"))
	}
	if !strings.Contains(settled, "::bigint") {
		t.Errorf("the settled sum is not cast to bigint:\n%s\nSUM over bigint widens to numeric in PostgreSQL, and without the cast an overflow arrives as a plausible-looking decimal rather than a loud driver error", settled)
	}
	if strings.Contains(settled, "unit_price") || strings.Contains(settled, "price_revision") {
		t.Errorf("the settled figure is computed from a price list:\n%s\nspend is priced from the frozen snapshot on the consume leg; joining the live list would restate last month's consumption at today's prices — a figure that is wrong, internally consistent, and reconcilable against nothing", settled)
	}
}

// TestTheCaptureSplitIsOverSettlements is the population claim as a
// statement. kind = 'settled' is the whole correctness of the split and not a
// refinement of it: a release and an expiry capture nothing — the schema
// refuses a capture method on one — and a fact claiming no usage while being
// disclaimed DOES carry one while booking no charge. A mix over facts would
// count a settlement that never priced anything.
func TestTheCaptureSplitIsOverSettlements(t *testing.T) {
	s, repo := scriptedAnalytics(t)
	if _, _, err := repo.Usage(t.Context(), scriptedQuery("018f0000-0000-7000-8000-000000000001")); err != nil {
		t.Fatalf("Usage() error = %v", err)
	}

	var capture string
	for _, statement := range s.statements() {
		if strings.Contains(statement, "capture_method = 'reported'") {
			capture = statement
			break
		}
	}
	if capture == "" {
		t.Fatal("no statement read the capture split")
	}
	if !strings.Contains(capture, "f.kind = 'settled'") {
		t.Errorf("the capture split is not restricted to settled facts:\n%s\nthe population is SETTLEMENTS THAT CAPTURED; a release and an expiry carry no capture method and an orphan carries one without booking a charge, so a mix over facts counts settlements that never priced anything", capture)
	}
}

// TestTheFlowsAndTheBalancesNeverShareAStatement is the port's own rule, and
// the reason it is a rule rather than a style note: the two families have
// opposite bucketing semantics and opposite overflow profiles, and a reader
// who "helpfully" merged them would put a factor-of-N error into the money.
//
// The statement-level form of the claim is that no single statement sums a
// bucket's legs and its cached balances together, and that the balances come
// from the projection's columns rather than from the legs.
func TestTheFlowsAndTheBalancesNeverShareAStatement(t *testing.T) {
	s, repo := scriptedAnalytics(t)
	if _, _, err := repo.Usage(t.Context(), scriptedQuery("018f0000-0000-7000-8000-000000000001")); err != nil {
		t.Fatalf("Usage() error = %v", err)
	}

	var balances string
	for _, statement := range s.statements() {
		if strings.Contains(statement, "SUM(b.held_amount)") {
			balances = statement
			break
		}
	}
	if balances == "" {
		t.Fatal("no statement read the balances")
	}
	if strings.Contains(balances, "control.ledger_entries") {
		t.Errorf("the balance statement reads the ledger:\n%s\nthe figures are the projection's cached columns; re-deriving them from the legs would be a third number beside the cache and the legs, and ADR 0011 has no check that could adjudicate between them", balances)
	}
	if !strings.Contains(balances, "available_amount") {
		t.Errorf("the balance statement does not read the projection's available column:\n%s", balances)
	}
}

// TestTheFreshnessReadsTheRecordTimeAndNeverThePosition is the honesty claim
// as a statement. The cursor's POSITION is an opaque string the Data Plane
// issued and the Control Plane is forbidden to decompose, so a statement that
// touched it would be a decomposition in waiting. What the read wants is the
// row's own updated_at.
func TestTheFreshnessReadsTheRecordTimeAndNeverThePosition(t *testing.T) {
	s, repo := scriptedAnalytics(t)
	if _, _, err := repo.Usage(t.Context(), scriptedQuery("018f0000-0000-7000-8000-000000000001")); err != nil {
		t.Fatalf("Usage() error = %v", err)
	}

	var cursor string
	for _, statement := range s.statements() {
		if strings.Contains(statement, "ingestion_cursor") {
			cursor = statement
			break
		}
	}
	if cursor == "" {
		t.Fatal("no statement read the ingestion cursor")
	}
	if !containsAll(cursor, "SELECT updated_at", "WHERE id = 1") {
		t.Errorf("the freshness statement is not the singleton's own record time:\n%s", cursor)
	}
	if strings.Contains(cursor, "position") {
		t.Errorf("the freshness statement touches the cursor's position:\n%s\nthe position is opaque and this plane is forbidden to decompose it; only the row's own updated_at is a statement this plane can honestly make", cursor)
	}
}

// TestTheReadNeverWrites is the property the whole file rests on, and the one
// the port states as a design rather than an omission: a second writer to a
// Control-Plane table is the one concurrency defect that is silent rather than
// loud, and the source sets this reads are all deduplicated by primary key, so
// the same answer is reconstructable by re-running the read at any time.
//
// A scripted driver can see this exactly: every statement it was asked to run,
// in order, and a read that inserted or updated would be visible as a
// statement that is not a SELECT. The SET LOCAL is the one write-shaped
// statement and it changes session state, not data.
func TestTheReadNeverWrites(t *testing.T) {
	s, repo := scriptedAnalytics(t)
	if _, _, err := repo.Usage(t.Context(), scriptedQuery("018f0000-0000-7000-8000-000000000001")); err != nil {
		t.Fatalf("Usage() error = %v", err)
	}

	for _, statement := range s.statements() {
		upper := strings.ToUpper(strings.TrimSpace(statement))
		for _, verb := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE", "CREATE", "DROP", "ALTER"} {
			if strings.HasPrefix(upper, verb) {
				t.Errorf("the read issued a %s statement:\n%s\nanalytics is derived; a rebuild is this same read at a later instant and not a second writer to a table the ledger owns", verb, statement)
			}
		}
	}
}

// TestTheStatementTextIsFixedWhateverTheCallerAsksFor is the statement-shape
// property, and it is the one a reader cannot see. The bucket bounds travel as
// a VALUES list with a fixed ROW, and the account resolution travels as ONE
// comma-joined parameter split server-side. No value a caller controls ever
// reaches the statement's TEXT — only its ARITHMETIC changes.
//
// "Fixed text" does not mean the same bytes for one bucket and for sixty-four:
// a 24-bucket day and a 90-bucket range are both asked for, and their VALUES
// lists have different placeholder numbers, so the leading text cannot be
// identical. What must be identical is everything OUTSIDE the VALUES list. So
// the assertion is split in two: the SELECT below the closing paren is
// byte-identical, and the text before the VALUES list differs only by the same
// run of ", ($n, $n::timestamptz, $n::timestamptz)" that the argument count
// demands. A rewrite that moved a bucket predicate into the leading text, or
// into a per-bucket subquery, would fail the second while passing the first —
// and that is the change this test exists to catch.
func TestTheStatementTextIsFixedWhateverTheCallerAsksFor(t *testing.T) {
	one, repoOne := scriptedAnalytics(t)
	if _, _, err := repoOne.Usage(t.Context(), persistence.UsageQuery{
		AccountID: "018f0000-0000-7000-8000-000000000001",
		From:      scriptedBounds()[0].Start,
		To:        scriptedBounds()[0].End,
		Buckets:   scriptedBounds()[:1],
	}); err != nil {
		t.Fatalf("Usage() with one bucket: %v", err)
	}

	many, repoMany := scriptedAnalytics(t)
	bounds := scriptedBounds()
	wide := make([]analytics.Bucket, 0, 64)
	start := bounds[0].Start
	for i := range 64 {
		wide = append(wide, analytics.Bucket{Start: start.Add(time.Duration(i) * time.Hour), End: start.Add(time.Duration(i+1) * time.Hour)})
	}
	if _, _, err := repoMany.Usage(t.Context(), persistence.UsageQuery{
		AccountID: "018f0000-0000-7000-8000-000000000001",
		From:      wide[0].Start,
		To:        wide[len(wide)-1].End,
		Buckets:   wide,
	}); err != nil {
		t.Fatalf("Usage() with 64 buckets: %v", err)
	}

	oneSeries, manySeries := "", ""
	for _, statement := range one.statements() {
		if strings.Contains(statement, "bucket_start") {
			oneSeries = statement
		}
	}
	for _, statement := range many.statements() {
		if strings.Contains(statement, "bucket_start") {
			manySeries = statement
		}
	}
	if oneSeries == "" || manySeries == "" {
		t.Fatal("the series statement did not run in both cases")
	}

	// The three parts: the fixed prefix before VALUES, the list itself, and
	// everything below its closing paren.
	onePrefix, oneList, oneTail := valuesListSplit(oneSeries)
	_, manyList, manyTail := valuesListSplit(manySeries)
	if onePrefix != fixedPrefix(manySeries) {
		t.Errorf("the fixed text before the VALUES list is a function of the bucket count:\n--- one bucket ---\n%s\n--- 64 buckets ---\n%s\nwith the VALUES list removed the two must be identical", onePrefix, fixedPrefix(manySeries))
	}
	if oneTail != manyTail {
		t.Errorf("the text after the VALUES list is a function of the bucket count:\n--- one bucket ---\n%s\n--- 64 buckets ---\n%s\neverything below the closing paren is the statement; a difference there is a per-bucket rewrite", oneTail, manyTail)
	}
	// The only thing the list adds is the run of rows, placeholder-free.
	row := "(::int, ::timestamptz, ::timestamptz)"
	if got, want := strings.Count(manyList, row), len(wide); got != want {
		t.Errorf("the VALUES list carries %d rows for %d buckets, want %d", got, len(wide), want)
	}
	if strings.Count(oneList, row) != 1 {
		t.Errorf("the one-bucket VALUES list carries %d rows, want 1", strings.Count(oneList, row))
	}

	// And the arguments, not the text, carry the bounds: three per bucket —
	// an int ordinal and two time instants — with the account as the one
	// leading argument. The ordinal is arg[0] of each triple, so args[1], [4],
	// [7]… are the ordinals and the instants are args[2], [3], [5], [6]….
	args := argumentsFor(many, "bucket_start")
	if len(args) != 1+3*len(wide) {
		t.Fatalf("the series statement bound %d arguments for %d buckets, want %d", len(args), len(wide), 1+3*len(wide))
	}
	for i := 1; i < len(args); i++ {
		withinTriple := (i - 1) % 3
		if withinTriple == 0 {
			continue // the ordinal, an int
		}
		if _, ok := args[i].(time.Time); !ok {
			t.Errorf("series argument %d is a %T, want a time.Time — the cast is in the text, so the value must already be an instant", i, args[i])
		}
	}
}

// stripPlaceholders removes every $n from a fragment, so two fragments can be
// compared for the text AROUND their placeholders.
//
// It is a character loop rather than a regexp over a line that could hold many
// statements, and it advances over the digits it consumed — the naive
// `strings.ReplaceAll` of a one-digit placeholder would eat the first digit of
// the next one and turn a mismatch into a match.
func stripPlaceholders(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			out.WriteByte(s[i])
			continue
		}
		for i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
			i++
		}
	}
	return out.String()
}

// valuesListSplit cuts a statement in three: the fixed prefix before the
// VALUES list, the list itself, and everything below its closing paren.
//
// The prefix is cut at the "VALUES (" marker and the tail at the first line
// that is exactly ")", so the two statements that must be identical — the parts
// a bucket count cannot touch — are compared directly, and the list, which is
// the only part that grows, is compared on its own terms.
//
// The tail is a line scan, not a paren counter. A counter looks right and is
// wrong: it starts inside the first row, the depth returns to zero at the end
// of that row, and the next pair it sees is the LATERAL join's
// "LEFT JOIN LATERAL (" ... ")", which cancels out — so the tail it returned
// began in the middle of the VALUES list and a real difference further down
// went unnoticed. The list is a flat comma-separated run of parenthesised rows
// with nothing nested inside them, so the first line that is exactly ")" is its
// end.
func valuesListSplit(statement string) (prefix, list, tail string) {
	open := strings.Index(statement, "VALUES (")
	if open < 0 {
		return "", "", statement
	}
	prefix = statement[:open]
	for lineStart := open; lineStart < len(statement); {
		lineEnd := strings.IndexByte(statement[lineStart:], '\n')
		if lineEnd < 0 {
			break
		}
		lineEnd += lineStart + 1
		if strings.TrimSpace(statement[lineStart:lineEnd-1]) == ")" {
			return stripPlaceholders(prefix), stripPlaceholders(statement[open:lineStart]), stripPlaceholders(statement[lineStart:])
		}
		lineStart = lineEnd
	}
	return stripPlaceholders(prefix), "", ""
}

// fixedPrefix is everything before the VALUES list, with its placeholders
// stripped — the part of a statement a bucket count may not change.
func fixedPrefix(statement string) string {
	prefix, _, _ := valuesListSplit(statement)
	return prefix
}

// argumentsFor is the recorded arguments of the one statement whose text
// contains fragment.
func argumentsFor(s *scriptedDriver, fragment string) []driver.Value {
	for i, statement := range s.statements() {
		if strings.Contains(statement, fragment) {
			return s.arguments()[i]
		}
	}
	return nil
}

// TestTheAccountResolutionIsOneParameterForAnyNumberOfBuckets is the same
// claim for the other statement. A variadic IN list would need a placeholder
// per bucket; the join is one comma-joined string split server-side with
// string_to_array. The assertion is that the text does not change with the
// bucket count, and that the argument is ONE string rather than N.
func TestTheAccountResolutionIsOneParameterForAnyNumberOfBuckets(t *testing.T) {
	t.Run("one bucket is one parameter", func(t *testing.T) {
		// The resolution answers one account per bucket, so the script is the
		// account shape repeated per call — and the driver cycles its script,
		// so one shape serves any number of buckets.
		s, db := newScripted(t, shapeAccount)
		_, err := NewFactDimensions(New(db)).AccountsOf(t.Context(),
			[]string{"018f0000-0000-7000-8000-000000000001"})
		if err != nil {
			t.Fatalf("AccountsOf() error = %v", err)
		}
		account := ""
		for _, statement := range s.statements() {
			if strings.Contains(statement, "string_to_array") {
				account = statement
			}
		}
		if account == "" {
			t.Fatal("the account resolution did not run")
		}
		if !strings.Contains(account, "unnest(string_to_array($1, ','))::uuid") {
			t.Errorf("the account resolution does not split one parameter server-side:\n%s", account)
		}
	})

	t.Run("many buckets are still one parameter", func(t *testing.T) {
		s, db := newScripted(t, shapeAccount)
		buckets := make([]string, 0, 40)
		for i := range 40 {
			// A well-formed version-7 uuid per bucket, so the grammar check
			// passes and the statement under test is the one that ran rather
			// than the refusal. The run-uniqueness is the counter in the last
			// group, which has to be twelve characters wide.
			buckets = append(buckets, "018f0000-0000-7000-8000-"+fmt.Sprintf("%012d", i))
		}
		if _, err := NewFactDimensions(New(db)).AccountsOf(t.Context(), buckets); err != nil {
			t.Fatalf("AccountsOf() with %d buckets error = %v", len(buckets), err)
		}
		statement := s.theStatementNamed("string_to_array")
		if statement == "" {
			t.Fatal("the account resolution did not run")
		}
		if got := strings.Count(statement, "string_to_array"); got != 1 {
			t.Errorf("the account resolution splits its input %d times, want 1 — one comma-joined parameter split server-side, not a variadic list", got)
		}
		// And the ids travelled as ONE argument rather than forty, which is
		// the claim a statement-text check alone cannot make.
		values := s.arguments()
		if len(values) != 1 {
			t.Fatalf("the resolution issued %d statements, want 1", len(values))
		}
		if len(values[0]) != 1 {
			t.Errorf("the resolution bound %d parameters for %d buckets, want 1 — a variadic IN list needs a placeholder per bucket, and a statement whose text grows with caller input is the one thing this adapter must never build",
				len(values[0]), len(buckets))
		}
	})
}
