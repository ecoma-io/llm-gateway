//go:build integration

package postgres

// The analytics benchmarks.
//
// This plane had none, and the read is the most expensive thing in it: five
// statements, a unit of work, and a VALUES list whose length is the caller's
// bucket count. The first two are the plane's own arithmetic and run anywhere;
// the last is the read itself, which can only mean anything against a real
// database with the indexes the migrations created.
//
// The pure benchmarks are in this file rather than beside the domain so the
// whole analytics set rides one command, and they are at the top so the
// database suite reads as the tail it is.
//
// Run (from apps/console-api):
//
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:5432/postgres?sslmode=disable' \
//	  go test -run '^$' -bench 'BenchmarkNewQuery|BenchmarkBucketBounds|BenchmarkWalkBounds|BenchmarkUsageRead' -benchmem -tags=integration ./internal/adapters/outbound/postgres
//
// The read's cost is LINEAR in the bucket count, which is the property these
// exist to establish rather than a number to record. Measured on an i7-10700K
// against an account with no rows, 30 iterations:
//
//	one bucket     1 bucket    ~1.5 ms fixed
//	seven days     168 buckets  2.25 ms   210 kB   2 091 allocs
//	ninety days    2 160 buckets 9.7 ms   3.1 MB  33 743 allocs
//
// A fixed cost near a millisecond and a per-bucket cost near four
// microseconds. The linear reading matters because the alternative is not a
// slow read but a REFUSED one: AnalyticsReadTimeout is five seconds, and a
// quadratic shape would turn "ask for a quarter more detail" into a timeout
// rather than into a limit the caller could be told about. It is also worth
// saying where the memory goes — 3 MB for the widest series is the argument
// being built and the rows being scanned, and it is the reason the read opens
// ONE unit of work for all five statements rather than five.

import (
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/analytics"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// benchmarkRange is ninety days of hourly buckets — the widest series the
// surface is asked for, and 2160 rows in the VALUES list. It is the shape a
// caller reaches for by asking for "the last three months, by hour", and it is
// the case the statement's text has to stay fixed for.
func benchmarkRange() (time.Time, time.Time) {
	from := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	return from, from.Add(90 * 24 * time.Hour)
}

// BenchmarkNewQuery costs the construction a caller pays before anything
// reaches the database: the zone is loaded and the range is validated. The
// zone load is the interesting half — it is the reason a query is refused
// rather than answered in the wrong zone — so the benchmark reports it rather
// than hiding it behind a cached *time.Location.
func BenchmarkNewQuery(b *testing.B) {
	from, to := benchmarkRange()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := analytics.NewQuery(from, to, analytics.GranularityHour, "Asia/Ho_Chi_Minh"); err != nil {
			b.Fatalf("NewQuery: %v", err)
		}
	}
}

// BenchmarkBucketBounds costs the tiling: one bucket per granularity step from
// the range's start to its end, in the display zone. Ninety days of hours is
// the widest series the surface builds, and the walk is a pure loop over the
// calendar — which is exactly why it is here rather than in SQL, where
// date_trunc would truncate in UTC.
func BenchmarkBucketBounds(b *testing.B) {
	from, to := benchmarkRange()
	query, err := analytics.NewQuery(from, to, analytics.GranularityHour, "Asia/Ho_Chi_Minh")
	if err != nil {
		b.Fatalf("NewQuery: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if got := len(query.BucketBounds()); got != 2160 {
			b.Fatalf("BucketBounds() returned %d buckets, want 2160 for ninety days of hours", got)
		}
	}
}

// BenchmarkWalkBounds is the same tiling reached through the free function, so
// the two cannot drift: a Query method that stopped delegating would show here
// as a second implementation rather than as a call.
func BenchmarkWalkBounds(b *testing.B) {
	from, to := benchmarkRange()
	query, err := analytics.NewQuery(from, to, analytics.GranularityHour, "Asia/Ho_Chi_Minh")
	if err != nil {
		b.Fatalf("NewQuery: %v", err)
	}
	location := query.Location()
	b.ReportAllocs()
	for b.Loop() {
		analytics.WalkBounds(from, to, analytics.GranularityHour, location)
	}
}

// BenchmarkUsageRead is the composed number: five statements inside one unit of
// work against a real database, for the widest series the surface builds.
//
// The account is a well-formed id that names no row, and that is deliberate.
// This benchmark measures the cost of the read's SHAPE — the 2160-row VALUES
// list, the LATERAL per bucket, the two-armed bucket set expression — and none
// of that depends on how many requests the account has. A populated fixture
// would fold an index scan into the number, and the scan is a property of the
// migrations rather than of the statement this plane chose. It also keeps the
// benchmark out of the write path entirely: a benchmark that funded a bucket
// would need a *testing.B twin of every test fixture, for a number the write
// path does not affect.
func BenchmarkUsageRead(b *testing.B) {
	repo, account := benchmarkAnalytics(b)

	from, to := benchmarkRange()
	query, err := analytics.NewQuery(from, to, analytics.GranularityHour, "Asia/Ho_Chi_Minh")
	if err != nil {
		b.Fatalf("NewQuery: %v", err)
	}
	buckets := query.BucketBounds()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := repo.Usage(b.Context(), persistence.UsageQuery{
			AccountID: account,
			From:      from,
			To:        to,
			Buckets:   buckets,
		}); err != nil {
			b.Fatalf("Usage(): %v", err)
		}
	}
}

// BenchmarkUsageReadSevenDay is the middle of the range, and it is here
// because the three together are the only way to see whether the read's cost
// is linear in the bucket count or whether something in it is not. A shape
// that is linear is a shape a caller can predict; a shape that is quadratic
// in the series length is a shape a caller can be refused by asking for a
// quarter more, and the refusal would arrive as a timeout rather than as a
// limit.
func BenchmarkUsageReadSevenDay(b *testing.B) {
	repo, account := benchmarkAnalytics(b)

	from, to := benchmarkRange()
	from = to.Add(-7 * 24 * time.Hour)
	query, err := analytics.NewQuery(from, to, analytics.GranularityHour, "Asia/Ho_Chi_Minh")
	if err != nil {
		b.Fatalf("NewQuery: %v", err)
	}
	buckets := query.BucketBounds()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := repo.Usage(b.Context(), persistence.UsageQuery{
			AccountID: account, From: from, To: to, Buckets: buckets,
		}); err != nil {
			b.Fatalf("Usage(): %v", err)
		}
	}
}

// BenchmarkUsageReadOneBucket is the same read for the single-bucket series an
// API caller is most likely to make by hand, and it is here beside the wide
// one because the two are the two ends of what the statement has to be good
// for. A statement tuned for the wide case alone would be measured only by
// the wide case; the ordinal cast that makes the narrow one work at all is
// exactly the kind of defect a single benchmark hides.
func BenchmarkUsageReadOneBucket(b *testing.B) {
	repo, account := benchmarkAnalytics(b)

	from, to := benchmarkRange()
	from = to.Add(-time.Hour)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := repo.Usage(b.Context(), persistence.UsageQuery{
			AccountID: account,
			From:      from,
			To:        to,
			Buckets:   []analytics.Bucket{{Start: from, End: to}},
		}); err != nil {
			b.Fatalf("Usage(): %v", err)
		}
	}
}

// benchmarkAnalytics opens the migrated control database for a benchmark and
// mints an account id that names no row — the read's tenancy predicate is an
// equality on this column, and an id with no row is the cheapest way to hold
// the population at zero.
func benchmarkAnalytics(b *testing.B) (persistence.Analytics, string) {
	b.Helper()
	store := New(integrationDB(b))
	account, err := identity.NewAccountID()
	if err != nil {
		b.Fatalf("mint the account id: %v", err)
	}
	return NewAnalytics(store), string(account)
}
