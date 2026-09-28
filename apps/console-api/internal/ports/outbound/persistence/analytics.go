package persistence

import (
	"context"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/analytics"
)

// The analytics read model, in the store's vocabulary.
//
// Everything this plane serves here is DERIVED. That is not a description of
// the implementation; it is the property the read model is allowed to have, and
// it is what makes a rebuild a read from authoritative state rather than a
// second writer (docs/architecture/analytics.md §7). The ledger is the money
// record, the settlement header is the settlement of record, and a
// disagreement between a figure served from this port and either of those is
// resolved in the ledger's favour every time.
//
// Five rules the signatures below carry on purpose, in continuation of the
// accounting port's:
//
//   - The account scope is a parameter and it is DERIVED. It is not a
//     request field — the surface declares none — but it is still the first
//     thing the statement must carry, and an implementation has no way to
//     assemble one of these queries without it. A read that could omit the
//     scope is not a read that a caller might tamper with; it is a read
//     that discloses every account's figures to whoever reaches it.
//   - The money is summed from the SETTLEMENT HEADER, never from a
//     per-request re-derivation. The derivation applies one ceiling over a
//     request's summed raw cost, so a per-request amount is a ROUNDED figure
//     and the sum of rounded figures is not the rounded total: a thousand
//     one-minor-unit requests settle for one minor unit between them, and a
//     naive sum of their per-request ceilings would bill a thousand. The
//     header is written once and is the sum of that settlement's own consume
//     legs, so summing headers is exact and additive with no rounding
//     anywhere. Every money SUM carries a ::bigint cast for the reason
//     accounting.go:834-846 gives: SUM over bigint widens to numeric in
//     PostgreSQL, and the cast moves an overflow to a loud driver error
//     instead of a silently-typed scan.
//   - Flows and balances never share a statement, a helper, or an
//     accumulator. The settled, released and funds-added figures are SUMS OF
//     FLOWS over a window and are bucketed; the held and available figures
//     are POINT-IN-TIME BALANCES as at the range's end and are not, and they
//     come from the projection's cached columns rather than from the legs.
//     They have opposite bucketing semantics and opposite overflow profiles,
//     and a reader who "helpfully" unified them would put a factor-of-N
//     error into the money.
//   - The capture split is over SETTLEMENTS, never over facts. A release or
//     an expiry captures nothing, and a fact claiming no usage while being
//     disclaimed carries a capture method and books no charge, so the
//     population is kind = 'settled'. The three methods partition it — the
//     schema's capture-shape CHECK guarantees no fourth and no absence among
//     the kinds that carry one — so a rate divides by their sum and by
//     nothing else.
//   - Nothing here writes, and nothing here may. There is no rebuild, no
//     backfill, no refresh and no projection hook on this port, by design
//     rather than by omission: a second writer to a Control-Plane table is
//     the one concurrency defect that is silent rather than loud, and the
//     source sets this reads are all deduplicated by primary key, so the
//     same answer is reconstructable by re-running the read at any time.

// FactDimension is one applied fact's account attribution: the row that makes
// account-scoped analytics possible at all, since the fact contract carries no
// account and the Data Plane's request table that does is unreachable from
// this plane (docs/architecture/analytics.md §1.1).
type FactDimension struct {
	// RequestID, KindClass and AccountID together are the row's identity,
	// carrying the same exactly-once boundary applied_facts enforces on its own
	// key — so a redelivered page or a second writer can produce no second row
	// for a (fact, account) pair, and a rebuild-by-read from this table and a
	// rebuild-by-read from applied_facts are the same set.
	//
	// The account is PART OF THE KEY, not a column beside it, because one fact
	// can belong to more than one account: a settlement's allocation tail can
	// draw on a PAYG bucket of one account and an entitlement bucket of
	// another, and each of those accounts paid for part of it. A key without
	// the account would record the first and drop the rest, and the dropped
	// account's report would be short by exactly the requests it funded.
	RequestID string
	KindClass string
	// AccountID is the account whose funding buckets the fact's own
	// allocation tail names, resolved at apply time. It is never a
	// caller-supplied value, and a fact that draws on no bucket of an
	// account belongs to no account — which is what makes this a tenancy
	// rule rather than a filter over a population that includes strangers.
	AccountID string
	// AppendSeq is the feed position the fact arrived on: provenance, never
	// an idempotency key, and never a dimension.
	AppendSeq int64
}

// FactDimensions records and serves the account a usage fact belongs to.
type FactDimensions interface {
	// Record inserts dimension as the attribution for one applied fact. It
	// runs inside the caller's unit of work — the same transaction that
	// booked the settlement or recorded the disposition — and refuses a call
	// that arrives without one. An attribution committed apart from the fact
	// it attributes would be a row describing a derivation that rolled back,
	// or a derivation with no account, and the second of those is the one
	// that would quietly remove a request from its customer's report.
	//
	// A row already present for the pair converges to a no-op, exactly as
	// the applied-facts ledger does, because the same redelivery reaches
	// both.
	Record(ctx context.Context, dimension FactDimension) error

	// AccountsOf returns the account every fact in the caller's unit of work
	// touched, derived from the buckets the fact's allocation tail names. It
	// is the one read on the ingestion path and it exists so the account is
	// derived from the derivation already being performed rather than
	// re-derived from the fact's payload by a second implementation.
	//
	// It returns a SET rather than one account because a settlement can draw
	// on an entitlement bucket and a PAYG bucket at once, and those buckets
	// can belong to one account or — in the general shape the schema allows —
	// to more than one. A caller that recorded only the first would
	// attribute a request to one of the accounts that paid for it and drop
	// the other, and the dropped account's report would be short by exactly
	// the requests it funded.
	AccountsOf(ctx context.Context, bucketIDs []string) ([]string, error)
}

// Bucket is one point of a derived series, as the store produces it. The
// bucket's own instants come from the store's clock and the store's calendar
// arithmetic, and the application places them into the domain's bucket walk
// rather than asking the store to cut the series — so the bounds the contract
// promises and the bounds the query used are the same rule.
type Bucket struct {
	Start time.Time
	End   time.Time
	// WithUsageFacts is the count of distinct requests with an applied fact
	// in this bucket, whose fact names at least one of the account's
	// buckets. It is never named "requests admitted": that population is not
	// derivable from this plane's rows (see the port's header).
	WithUsageFacts int64
	// Settled is the count of requests that reached a settlement of record
	// in this bucket. Zero is a real answer.
	Settled int64
}

// Balances is the account's cached capacity, as at an instant: a point-in-time
// reading and not a flow, which is why it is a separate type from Bucket and
// is summed over the account's buckets rather than cut into them.
//
// A bucket's own balance is ONE bucket; an account holds one PAYG balance plus
// one bucket per entitlement cycle, so a per-account total and a per-bucket
// figure differ by construction and are not in conflict. Both are exact: a
// balance is a running accumulation of signed leg deltas, never a sum of
// rounded per-request figures.
type Balances struct {
	// Held is capacity reserved and not yet consumed.
	Held int64
	// Available is capacity settled and unspent, held subtracted.
	Available int64
}

// UsageQuery is one bounded read, already validated. The account and the
// range are here because the statement cannot be built without them; the
// grain and the bucket bounds are here because the store cuts the series
// against a set of intervals rather than re-deriving them.
type UsageQuery struct {
	// AccountID is the derived scope. Every statement built from this query
	// must carry it, and the implementation has no shape in which to omit it.
	AccountID string
	// Range is the half-open window [From, To) over the store's own clocks:
	// applied_at for the request counts, created_at for the money. The two
	// are different axes and one statement does not straddle them, because
	// they are different clocks and a figure that mixed them would be in
	// neither.
	From time.Time
	To   time.Time
	// Buckets are the intervals to cut the series into, in the query's zone.
	// The caller produces them and the store keys its aggregate off them, so
	// a bucket with no activity comes back as a zero rather than as a gap.
	Buckets []analytics.Bucket
}

// Usage is the whole answer for one account, one range, one grain — the
// money, the balances and the capture split beside the series.
//
// The series and the envelope totals are the same numbers computed once: the
// caller sums the series rather than asking for a second, independent total,
// because two totals for one question are two numbers that can disagree and a
// caller receiving both has no way to know which the chart came from.
type Usage struct {
	// Series is one entry per requested bucket, in the order the buckets
	// were given, with no bucket omitted.
	Series []Bucket
	// SettledMinorUnits, ReleasedMinorUnits and FundsAddedMinorUnits are
	// sums of exact integers over the range. They are NOT the sum of a
	// re-derived per-request amount (see the header) and they are never
	// netted against one another: a release moves the held axis and a
	// consume moves the settled one, so a net figure would be a
	// subtraction of two unrelated quantities rather than a refund.
	SettledMinorUnits    int64
	ReleasedMinorUnits   int64
	FundsAddedMinorUnits int64
	// Balances is the account's cached capacity as at the range's end, and
	// the only point-in-time figures in the answer.
	Balances Balances
	// Capture splits the settlements that captured, by the method each used.
	// The population is settlements of kind 'settled'; a release and an
	// expiry capture nothing and a disclaimed orphan books no charge.
	Capture analytics.Capture
}

// Freshness is how far this plane has got through the fact feed: the instant
// of the last completed pass, and what that instant is a statement about. It
// is a liveness signal and not a completeness one, because the feed's
// high-water mark is the Data Plane's to publish and this surface does not
// read it — so the port returns the basis alongside the instant and the
// contract can say what it is rather than a reader having to guess.
type Freshness struct {
	DataThrough time.Time
	Basis       analytics.FreshnessBasis
}

// Analytics is the read model. Every member is a read; the port has no write
// beyond the ingestion-time attribution above, and a surface that wants a
// rebuild or a backfill is asking this plane for a second writer to a table
// the ledger owns.
type Analytics interface {
	// Usage returns one account's derived usage over a bounded range at one
	// grain, and the freshness statement to read it with. It runs under a
	// per-request deadline and its statements carry a statement_timeout of
	// their own, so a read that cannot finish inside the bound fails closed
	// (as a refusal) rather than pinning a connection until the pool is
	// gone — the surface shares its pool with the settlement path, and a
	// report must never be the thing that starves a charge.
	//
	// The returned slice's length equals the query's bucket count. A bucket
	// with no activity is present with zero figures, because a caller
	// rendering a gap-free series has no way to invent a rule for which
	// gaps to fill.
	Usage(ctx context.Context, query UsageQuery) (Usage, Freshness, error)
}
