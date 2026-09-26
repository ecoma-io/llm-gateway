// Package usagefacts is the runtime's outbound port to its own recorded facts:
// the durable history of what this Data Plane did, read back in the order it
// happened.
//
// It is a port in the Data Plane rather than a query written where the answer
// is needed, because the answer is not needed on the request path at all. An
// LLM request is admitted, routed, executed and recorded without anyone
// reading a fact; what reads facts is a consumer that arrives later — the
// Control Plane, over the private management listener — and pays its own
// latency for doing so (ADR 0006 §4, §5). The separation is the whole delivery
// model: a fact is committed here and fetched from there, and the two are not
// the same act.
//
// Three properties of this port are the design, and each is a decision rather
// than an observation about the code:
//
//   - **Ordering is a position, not a timestamp.** Facts are returned in a
//     single monotonic append order — the sequence the store assigns at the
//     moment a fact commits — and a cursor names a position in that order.
//     `OccurredAt` is carried because a consumer wants to know when something
//     happened, never because it is safe to order by: two facts recorded in the
//     same instant, or with clocks that disagree, would sort wrongly and
//     silently.
//   - **Replay is normal.** The same range may be read any number of times and
//     returns the same facts. There is no acknowledgement, no read receipt and
//     no deletion: this port has no method that consumes anything, and the
//     absence is deliberate. A read that destroyed what it read would make a
//     crashed consumer lose facts, and a read that marked what it read would
//     make two consumers share one position. The position belongs to whoever
//     is reading, and it is stored there.
//   - **The cursor is opaque.** It is a string this package returns and
//     accepts, and nothing outside the Data Plane may interpret it: not the
//     façade that forwards it, not the consumer that holds it. A consumer that
//     parsed it would be a consumer coupled to this store's internals through
//     a value it was told not to look inside.
//
// The port has one method because the use case has one shape. The day a
// consumer needs something else — a count, a summary, a replay bounded by time
// — that is a second method with a second caller, not a parameter added to
// this one.
package usagefacts

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Event is one immutable fact the runtime recorded about a request it handled.
//
// Immutable is the operative word: a fact states what happened, and a
// correction is a later fact rather than an edit to this one. That is what
// makes replay safe and what makes the consumer's position meaningful — a
// history that can change under a reader is not a history a cursor can point
// into.
//
// The typed fields below travel beside the payload on purpose. The payload is
// the fact's own schema — the allocation tail, versioned by SchemaVersion —
// while these columns are the feed's envelope: the figures and identities a
// settlement is derived from, carried as typed values so a consumer can
// settle from the fact alone without parsing the payload's shape. A
// settlement-relevant fact carries them whole: a settled fact names its
// attempt, its capture method, its usage counts, its price basis and its
// amount; a released or expired fact carries none of them. The pointer types
// are the null fidelity the settlement needs — zero is a real figure (a free
// model settles at zero), and NULL is a different claim entirely.
type Event struct {
	// AppendSeq is the store-allocated position in the feed's single
	// monotonic order — the same number the cursor is a position in — and it
	// is the fact's identity on the wire. Two facts of one request never
	// share one, and a correction, when the day comes, names the fact it
	// amends through it. It is never the consumer's dedup key (RequestID and
	// the kind's class are), but without it a consumer cannot state which
	// fact a question is about.
	AppendSeq int64

	// RequestID is the fact's business identity and the consumer's
	// idempotency key — keyed by kind class, not alone. The runtime mints one
	// settlement-relevant fact per request at most (a settled, released or
	// expired fact: the three are mutually exclusive, the ending's CAS
	// referees them), and one unbillable_orphaned fact at most, which is not
	// settlement-relevant and may coexist with any of the three by design.
	// A consumer that deduplicated by RequestID alone would drop the second
	// fact it saw — losing a settlement because an orphan arrived first, or
	// the telemetry because a settlement did — so the rule the idempotency
	// promise actually makes is: at most one settlement-relevant fact per
	// RequestID, and applying the same RequestID twice must settle, consume
	// or release exactly once (ADR 0006 §5).
	//
	// It is deliberately not the HTTP correlation identifier. `X-Request-Id` is
	// a transport handle for tracing one client request through logs; it may be
	// absent, invented by a caller, or reused across unrelated requests. A fact
	// needs an identity the Data Plane minted and owns, because settlement
	// hangs off it.
	//
	// One more substitution it is worth naming where a consumer meets it: on
	// this seam the RequestID is also the reservation's identity. The runtime
	// mints exactly one reservation per request — uniqueness on request_id is
	// total — so a Control Plane leg derived from this fact names the
	// reservation through the request id, and the fact carries no separate
	// reservation field because there is no second value to carry.
	RequestID string

	// Kind names what happened, in the closed vocabulary the fact contract
	// declares: a settled request, a released reservation, an expired one, or a
	// request that ended in a way no accounting can be derived from.
	Kind string

	// SchemaVersion is the version of the payload's shape. A consumer that
	// predates the payload it is reading needs to be able to say so rather than
	// guess.
	SchemaVersion int

	// CaptureMethod names how the fact's usage figures were known — the
	// contract's three-value vocabulary as a plain string. Nil on the facts
	// that carry no usage claim (released, expired); present on every
	// settled and unbillable_orphaned fact.
	CaptureMethod *string

	// CommittedAttemptID names the upstream call the usage belongs to. Nil on
	// the facts that name no attempt.
	CommittedAttemptID *string

	// ProviderInputTokens and ProviderOutputTokens are the provider's own
	// usage report as the gateway recorded it, bounded or not. Nil where no
	// report arrived.
	ProviderInputTokens  *int64
	ProviderOutputTokens *int64

	// DeliveryTokens is the gateway's own count of what reached the client —
	// the canonical v1 byte rule, recorded beside every settlement.
	DeliveryTokens *int64

	// PriceRevision and the two unit prices are the settled fact's pricing
	// basis, present together or not at all. The prices are integer minor
	// units per 1M tokens, copied from the revision the hold was priced at —
	// a later price change never rewrites them.
	PriceRevision   *string
	InputUnitPrice  *int64
	OutputUnitPrice *int64

	// SettledAmount is what the request cost, in integer minor units. Nil on
	// every non-settled fact; zero is a real settled amount, and the amount
	// equals the hold formula re-derived over the fact's own figures and
	// prices — that equality is the fact's binding.
	SettledAmount *int64

	// CorrectsAppendSeq is reserved for the correction path: a later fact
	// naming the AppendSeq of the one it amends. Nil on every fact this build
	// writes.
	CorrectsAppendSeq *int64

	// OccurredAt is when the fact happened, for a consumer that wants to
	// reason about time. It is not the ordering key — see the package comment
	// and NextCursor.
	OccurredAt time.Time

	// Payload carries the fact's own fields. It is opaque JSON here because
	// this package's job is to hand back what was recorded, not to interpret
	// it: the shape belongs to the fact contract, and a typed struct in this
	// port would be a second definition of it that could drift.
	//
	// It is deliberately `json.RawMessage` rather than `any`: no map is
	// allocated, no number is turned into a float, and the bytes the consumer
	// eventually reads are the bytes that were stored.
	Payload json.RawMessage
}

// Page is one replayable slice of the feed.
type Page struct {
	// Events are the facts strictly after the requested position, in the Data
	// Plane's append order. Ascending, and never reordered by a caller: the
	// order is the store's, and a consumer that sorted these would be inventing
	// an order it does not own.
	//
	// Strictly, and not at-or-after, because the requested position is the one
	// the previous page's NextCursor named — which is the last fact that page
	// delivered. Returning that fact again would be harmless (the consumer
	// applies by RequestID and a repeat is a no-op) but returning it *instead*
	// of the first new fact would be a page that makes no progress, and the two
	// differ only by which side of the boundary the convention rounds to.
	Events []Event

	// NextCursor is the position of the last event in this page — the value to
	// send as the next `after`, since the next read resumes strictly after it.
	// It is opaque (see the package comment) and non-empty even when no events
	// were returned, so a caller that applied nothing still gets a defined
	// position to resume from.
	//
	// It is produced here and never by a consumer: a consumer that computed its
	// own position would be deriving a Data Plane address from a shape it was
	// told not to look inside, and the two would disagree the first time the
	// store's ordering changed.
	NextCursor string

	// HasMore reports whether more facts were available at the moment of the
	// read. It is a hint about how much work remains, not a promise that the
	// next read returns something: facts may be appended between the two calls.
	// A consumer that treated it as a promise would poll on a false
	// expectation; one that ignored it would work exactly as well, which is why
	// the feed is safe either way.
	HasMore bool
}

// DefaultLimit is the page size when a caller does not ask for one: large
// enough that a healthy consumer catches up in a few round trips, small enough
// that one page is not a long-running query holding a read view open.
//
// It is one of the two numbers the fact contract declares (1..1000, default
// 100) and this is the module that owns their meaning; the management surface
// applies them to the caller's query, so this constant is enforced rather than
// merely documented.
const DefaultLimit = 100

// MaxLimit bounds what a caller may ask for. A page is held in memory on both
// sides of the wire, and an unbounded `limit` is a request for the whole
// history — a denial-of-service the caller can trigger by accident.
//
// A request above it is refused with `400 invalid_request` rather than served
// at the bound: a clamped answer is indistinguishable from a page the feed
// itself produced, so the caller would never learn that the number it chose was
// discarded.
const MaxLimit = 1000

// ErrCursorExpired says the requested position can no longer be replayed —
// it has fallen out of what this Data Plane retains.
//
// It exists so that losing facts is a loud, distinguishable failure. The
// alternative, and the reason this error is worth a name, is a read that
// silently starts from the nearest surviving position: the consumer would
// advance past facts it never saw, and the gap would surface much later as
// money that cannot be accounted for. An expired cursor stops the consumer and
// asks a human what to do; that is the correct behaviour for a history whose
// whole purpose is to be complete.
var ErrCursorExpired = errors.New("usage fact cursor is no longer replayable")

// ErrSourceUnavailable says the durable source could not be read at all —
// the store is unreachable, or this build has none.
//
// It is separate from ErrCursorExpired because the two call for opposite
// responses: an expired cursor is a decision to make, a transient read failure
// is a retry. A transport that collapsed them would make a consumer either
// retry forever against a position that will never come back, or give up on a
// read that would have succeeded a second later.
var ErrSourceUnavailable = errors.New("usage fact source is unavailable")

// Reader reads the runtime's recorded facts.
//
// It is an interface at the application boundary rather than a concrete store:
// the application calls this, and only the composition root decides what
// answers behind it. The production adapter is the PostgreSQL reader over the
// usage_events table the runtime storage schema creates; ErrSourceUnavailable
// is its answer for a store that cannot be read, and there is deliberately no
// in-memory implementation anywhere in this repository — an in-memory feed
// would pass every test while losing every fact on restart, which is the one
// failure this whole design exists to prevent.
type Reader interface {
	// Read returns the facts strictly after the position named by after, in
	// ascending append order.
	//
	// An empty after means "from the beginning of what is retained": it is how
	// a consumer with no stored position starts, and it is not an error.
	//
	// limit is a page size the caller asked for, already bounded: the management
	// surface applies DefaultLimit and MaxLimit below before this method is
	// reached, so the value here is one the fact contract allows and an
	// implementation has no page size of its own to impose. A limit outside those
	// bounds is a caller that reached this port without passing the surface, and
	// that is a defect rather than a request to be adjusted.
	//
	// A read is side-effect free. Calling Read twice with the same arguments
	// returns the same facts — modulo facts appended in between — because
	// nothing about reading acknowledges, consumes or advances anything.
	Read(ctx context.Context, after string, limit int) (Page, error)
}
