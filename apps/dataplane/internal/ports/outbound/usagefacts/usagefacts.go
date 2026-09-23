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
type Event struct {
	// RequestID is the fact's business identity and the consumer's idempotency
	// key. Applying the same RequestID twice must settle, consume or release
	// exactly once (ADR 0006 §5).
	//
	// It is deliberately not the HTTP correlation identifier. `X-Request-Id` is
	// a transport handle for tracing one client request through logs; it may be
	// absent, invented by a caller, or reused across unrelated requests. A fact
	// needs an identity the Data Plane minted and owns, because settlement
	// hangs off it.
	RequestID string

	// Kind names what happened, in the closed vocabulary the fact contract
	// declares: a settled request, a released reservation, an expired one, or a
	// request that ended in a way no accounting can be derived from.
	Kind string

	// SchemaVersion is the version of the payload's shape. A consumer that
	// predates the payload it is reading needs to be able to say so rather than
	// guess.
	SchemaVersion int

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
	// Events are the facts at or after the requested position, in the Data
	// Plane's append order. Ascending, and never reordered by a caller: the
	// order is the store's, and a consumer that sorted these would be inventing
	// an order it does not own.
	Events []Event

	// NextCursor is the position immediately after the last event in this page
	// — the value to send as the next `after`. It is opaque (see the package
	// comment) and non-empty even when no events were returned, so a caller
	// that applied nothing still gets a defined position to resume from.
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
const DefaultLimit = 100

// MaxLimit bounds what a caller may ask for. A page is held in memory on both
// sides of the wire, and an unbounded `limit` is a request for the whole
// history — a denial-of-service the caller can trigger by accident.
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
// answers behind it. The implementation that reads a real store arrives with
// the schema that creates one; until then this port's production adapter
// reports ErrSourceUnavailable, which is the honest answer and not a
// placeholder to be filled in with an in-memory log — an in-memory feed would
// pass every test in this repository while losing every fact on restart, which
// is the one failure this whole design exists to prevent.
type Reader interface {
	// Read returns the facts at or after the position named by after, in
	// ascending append order.
	//
	// An empty after means "from the beginning of what is retained": it is how
	// a consumer with no stored position starts, and it is not an error.
	// limit is a page size the caller may ask for; an implementation applies
	// its own bounds, and the application clamps the request before it arrives
	// here.
	//
	// A read is side-effect free. Calling Read twice with the same arguments
	// returns the same facts — modulo facts appended in between — because
	// nothing about reading acknowledges, consumes or advances anything.
	Read(ctx context.Context, after string, limit int) (Page, error)
}
