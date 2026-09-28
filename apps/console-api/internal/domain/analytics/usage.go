package analytics

import (
	"fmt"
	"time"
)

// Availability says whether this plane holds a derivation for the ACCOUNT it
// answered for, on two axes that are deliberately separate from failure.
//
// The two-axis shape is the whole design: there IS an answer or there is not,
// and separately the answer is a failure or it is not. Collapsing them into a
// single status enumeration would put `0` and "no data" in the same member, and
// this plane's money domain has already ruled that collapse out — a zero-priced
// model "books a hold of nothing, settles for nothing, and is still a
// settlement of record" (`accounting/money.go:49-52`). So `available` is what a
// run that legitimately found nothing reports, and a failure is a non-2xx
// carrying the ErrorEnvelope rather than a third member of this type.
//
// The axis is the ACCOUNT's coverage and not the range's, and that is the
// distinction the type exists for: every requested range is answered with a row
// per bucket whether or not anything happened in it, so a range can always be
// answered — "nothing happened in these three hours" is a figure. What cannot
// be answered for is an account this plane has never ingested, and a member
// that meant "this range was quiet" would be the same member as "there is
// nothing to report", which is the collapse this type refuses.
type Availability string

const (
	// AvailabilityAvailable: the figures ARE the answer to the range asked
	// of, including when the answer is that nothing happened in it. The
	// account is in this plane's derived rows; the range may be empty of
	// them.
	AvailabilityAvailable Availability = "available"
	// AvailabilityNotAvailable: this plane holds no derived rows for this
	// account at all — none in the queried range and none in any other. The
	// usage it names has not been ingested, or it predates this surface. It
	// is an ANSWER, not a failure, and a caller renders it as an empty
	// series and asks for a later range.
	//
	// It is NOT what a quiet range reports. A range the account simply did
	// nothing in is available, with zeros, because the account is one this
	// plane can answer for — and a caller that could not tell those apart
	// would read "no data" as "nothing happened".
	AvailabilityNotAvailable Availability = "not_available"
)

// Freshness is what this plane can honestly say about how far it has got, and
// the vocabulary for saying it is one member long on purpose.
//
// DataThrough is a LIVENESS signal: it names when this plane last completed a
// pass over the usage-fact feed, and it does NOT say that every fact the
// runtime has emitted is reflected in the figures. Those are different
// questions and only the first is answerable from this plane's own state,
// because the feed's high-water mark is the Data Plane's to publish and this
// surface does not read it. Basis exists so a reader can tell that from a
// watermark without the contract having to change when one arrives.
type Freshness struct {
	DataThrough time.Time
	Basis       FreshnessBasis
}

// FreshnessBasis is what a DataThrough instant is a statement about.
type FreshnessBasis string

const (
	// FreshnessFactFeedPass is a record-time of this plane's own ingestion
	// loop. It is never an event-time of the runtime that produced the
	// usage, and a bucket is never assigned to it. The single member is
	// deliberate: it is what makes the absence of a true watermark visible
	// in the contract rather than implied by a field that looks like one.
	FreshnessFactFeedPass FreshnessBasis = "fact_feed_pass"
)

// Capture splits the settlements in a range by how they captured their usage.
//
// The population is SETTLEMENTS THAT CAPTURED, not facts and not requests. A
// release or an expiry captures nothing — the ledger's own schema refuses a
// capture method on one — and a fact claiming no usage while being disclaimed
// carries a capture method and books no charge. A rate over this split divides
// by the sum of the three members and by nothing else.
type Capture struct {
	// Reported: priced as the provider's own report of its usage.
	Reported int64
	// GatewayObserved: priced as the gateway's own count of what it
	// received. Delivery is always the gateway's count by construction and
	// never decides the label.
	GatewayObserved int64
	// ReservationFloor: the figure was the reservation's own — a clamp that
	// bound, or the basis fall-through when neither the provider's report
	// nor the gateway's count arrived. This is a measure of PRICING
	// CONFIDENCE: it is biased upward against a report that would have
	// arrived, so a rising floor is a statement about the upstream's usage
	// reporting and not about this account.
	ReservationFloor int64
}

// Total is the population any rate over the capture split divides by.
func (c Capture) Total() int64 { return c.Reported + c.GatewayObserved + c.ReservationFloor }

// Point is one bucket of a series: the interval it covers in the query's zone,
// and the figures derived into it.
//
// The two counts are NOT "requests" and the name is the definition. This plane
// cannot derive the requests the runtime admitted — a request that produced no
// fact leaves no row in either of this plane's fact tables, so any such count
// would measure the set of requests this plane knows about and call it the set
// of requests. WithUsageFacts is the requests whose usage reached the account's
// capacity and was recorded, and it is the denominator of Settled.
type Point struct {
	Start time.Time
	End   time.Time
	// WithUsageFacts: requests this plane holds an applied fact for, whose
	// fact names at least one of the account's funding buckets.
	WithUsageFacts int64
	// Settled: requests that reached a settlement of record. Zero is a real
	// answer — free-model traffic settles for nothing and is still a
	// settlement of record.
	Settled int64
}

// Usage is one account's usage over one range at one grain: the whole answer,
// and the shape the contract declares.
type Usage struct {
	Availability Availability
	// Range echoes the query it answered: the half-open bounds and the zone
	// the buckets were cut in, so a caller renders the instants it was
	// given rather than re-deriving them.
	Range Query
	// FinalBucketPartial: the range's end falls inside the last bucket, so
	// that bucket is still filling and its figures will grow. A fact about
	// the RANGE, not the data — an empty partial bucket is still partial.
	FinalBucketPartial bool
	Series             []Point

	RequestsWithUsageFacts int64
	RequestsSettled        int64

	// The money. Every figure is an integer number of minor units in the
	// deployment's single platform-wide settlement currency, and NO field
	// here names a currency: the money domain holds no currency value
	// either, because the settlement currency is configuration that lives
	// above the domain and no per-row currency column exists anywhere to
	// contradict it. A consumer takes the currency from its own
	// configuration; one that guesses has invented a currency the model
	// never had.
	//
	// Settled and Released are SUMS OF EXACT INTEGERS and must never be
	// re-derived from a per-request amount, which is rounded: the
	// derivation applies one ceiling over a request's summed raw cost, and
	// the sum of rounded figures is not the rounded total. A thousand
	// one-minor-unit requests settle for one minor unit between them, and
	// summing their per-request ceilings would bill a thousand. The
	// settlement header is the authority precisely because it is written
	// once and summed.
	//
	// Held and Available are POINT-IN-TIME balances and are not bucketed:
	// they are the account's capacity as at the moment the READ ran, carried
	// once rather than on every point because a caller that received them per
	// point had no way to know they were true of none of them.
	//
	// The instant is the read's and NOT the range's, and that is a correction
	// rather than a nuance: a range that ended in June is answered with the
	// balance the account holds today, and it cannot be answered with
	// anything else — the projection caches the present and keeps no history,
	// so there is no as-of read to make. A caller comparing two historical
	// ranges is comparing one balance read twice.
	//
	// They are the accounting projection's CACHED balances, read and never
	// re-derived — exact, because a balance is a running accumulation of
	// signed leg deltas and never a sum of rounded per-request figures.
	SettledMinorUnits    int64
	ReleasedMinorUnits   int64
	FundsAddedMinorUnits int64
	// Held and Available are sums over the account's buckets, NOT
	// recomputations of them. A bucket's own balance is one bucket; an
	// account with one PAYG balance and N entitlement cycles has N+1 of
	// them, so a per-account total and a per-bucket figure differ by
	// construction and are not in conflict.
	HeldMinorUnits      int64
	AvailableMinorUnits int64

	Capture   Capture
	Freshness Freshness
}

// NewUsage is the one construction path for an AVAILABLE answer, and it is
// where the arithmetic that every reader would otherwise be tempted to redo
// lives.
//
// The totals are the sum of the series rather than a separate set of figures
// the store also returned. That is not a convenience: two independent totals
// for the same question are two numbers that can disagree, and a caller that
// received both would have no way to tell which one the chart was drawn from.
// One place computes them, so the envelope and the series cannot drift. This is
// exported because the application layer is the caller that has read the store's
// two halves — a series of counts and a set of money figures — and it is the
// only place both exist at once; letting a second construction path through
// would be the one way the two halves could be summed separately.
//
// The balances are NOT summed here. They are point-in-time figures as at the
// read's own instant and belong to exactly one place in the answer, so adding
// them to the series would mean the same number appeared once per point with the
// implication that it was true of each — a per-bucket reading of a per-account
// figure read once, which is the most likely way a caller would misread one.
func NewUsage(query Query, series []Point, capture Capture, freshness Freshness) Usage {
	var withFacts, settled int64
	for _, point := range series {
		withFacts += point.WithUsageFacts
		settled += point.Settled
	}
	return Usage{
		Availability:           AvailabilityAvailable,
		Range:                  query,
		FinalBucketPartial:     query.FinalBucketPartial(),
		Series:                 series,
		RequestsWithUsageFacts: withFacts,
		RequestsSettled:        settled,
		Capture:                capture,
		Freshness:              freshness,
	}
}

// NotAvailable is the answer for an account this plane holds no derived rows
// for.
//
// It is a complete answer, not an error and not an empty successful one: the
// series is empty, every count is zero, and Availability says why. A caller can
// tell it from a range that was simply quiet — which is an AVAILABLE answer
// whose buckets are zero, because the account is one this plane can answer for
// and the range is one it found nothing in — and that difference is the whole
// reason the field exists rather than the absence of the figures.
func NotAvailable(query Query, freshness Freshness) Usage {
	return Usage{
		Availability:       AvailabilityNotAvailable,
		Range:              query,
		Series:             []Point{},
		FinalBucketPartial: query.FinalBucketPartial(),
		Freshness:          freshness,
	}
}

// errInvalidGranularity is the wrapped refusal ParseGranularity raises, naming
// the vocabulary the caller could have used.
func errInvalidGranularity(raw string) error {
	return fmt.Errorf("%w: %q is not one of hour, day, calendar_month", ErrInvalidGranularity, raw)
}
