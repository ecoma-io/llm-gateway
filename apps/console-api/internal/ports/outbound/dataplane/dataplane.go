// Package dataplane is the console-api's outbound port to the Data Plane's
// management surface: the one seam between the two planes, the only place the
// Control Plane and the Data Plane change anything about each other.
//
// It is a port in this module rather than a shared package on purpose. The two
// planes are separate Go modules (ADR 0006 §1), so there is no import path
// through which the Control Plane could reach a Data Plane type; what crosses
// the line is a call over an interface declared here, implemented in
// internal/adapters/outbound by an adapter that speaks the Data Plane's
// management contract. Which transport that adapter uses is its business, and
// the day it changes, this package does not.
//
// The directions are not symmetric, and the asymmetry is the architecture:
// credential decisions flow Control → Data (Projection, ADR 0007) — pushed,
// because the authority over who may call the gateway is this plane's and the
// database the request path reads is the other's — while facts flow Data →
// Control (UsageFacts), which the Control Plane reconciles from at its own
// pace. Credit, quota and subscription state travel as grants, not as shared
// tables, because neither plane may read the other's database (ADR 0006 §5,
// §7).
//
// Exactly two operations were enumerated here when the seam was drawn, because
// they were the two the architecture already fixed end to end: a credential
// the Control Plane has withdrawn must stop being accepted at the runtime,
// within a bounded staleness window rather than the moment the withdrawal
// commits (ADR 0006 §8); and the usage facts the runtime recorded must be
// readable by the Control Plane, idempotently and with replay, so that
// settlement survives either side crashing (ADR 0006 §5). The revocation
// story's spelling is the projection (ADR 0007, declared beside this file in
// projection.go): a withdrawn credential reaches the runtime as a projected
// state change on a durable, ordered, replayable feed — pushed by this plane,
// not asked — rather than as a notification call whose failure would leave a
// revoked key authenticating indefinitely. The commerce roll is the third
// caller the seam has been waiting for, and it arrives with the one read its
// transaction needs: which alias-group version is current for a group name,
// answered by the Data Plane that owns the catalog (ADR 0006 §5), so an
// entitlement can pin a scope by id without this plane ever reading the
// catalog's tables. The other operations a management surface will eventually
// need — a grant, a capacity publication, a cache invalidation — are still not
// declared as guesses: each arrives with the use case that has to make it,
// and a port method with no caller is a shape invented twice.
//
// The fact half and the projection are implemented, and the seam's wire half
// is served by the adapter beside it in internal/adapters/outbound/dataplane,
// which claims UsageFacts, CatalogReader and Projection. Management as a whole
// still has no full implementation: WithdrawCredential remains a declared
// callerless method — the projection carries the revocation over its own
// contract instead, and the notification's one caller has not arrived
// (ADR 0006 §8) — while CurrentGroupVersion is spoken over the management
// contract in api/openapi/dataplane.yaml by the commerce roll, which takes the
// narrow CatalogReader rather than the whole interface it does not use. The
// port exists ahead of its callers because the seam has to exist before either
// side of it is built, and because its absence would leave a future author
// with no recorded answer to "how does the Control Plane talk to the Data
// Plane?".
package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Management is what the Control Plane may ask of the Data Plane.
//
// It is deliberately an interface at the application boundary rather than a
// concrete client: application code calls this, and only the composition root
// chooses what answers behind it.
type Management interface {
	// WithdrawCredential tells the Data Plane that the credential identified
	// by credentialID must no longer authenticate a runtime request.
	//
	// The call is a notification, not a transaction: it can fail, be retried,
	// or arrive after the runtime has already served a request with the
	// credential, and the contract is bounded staleness rather than linear
	// consistency. An implementation reports a transport failure as an error
	// and must not treat a failed withdrawal as a completed one — the Control
	// Plane keeps the credential's revocation pending until the Data Plane has
	// acknowledged it.
	WithdrawCredential(ctx context.Context, credentialID string) error

	// CurrentGroupVersion returns the version of the named alias group that
	// is current in the Data Plane's catalog right now — the snapshot id a
	// commerce entitlement pins as its scope when a cycle rolls.
	//
	// "Current" is the catalog's own rule (the group's highest version), and
	// the answer is a point-in-time reading, not a subscription: the Data
	// Plane may open the group's next version the instant after answering,
	// and this plane's already-rolled entitlements keep the version they
	// pinned — purchased scope never changes retroactively (ADR 0003). The
	// commerce roll resolves names before it opens its unit of work and
	// treats a transport failure as the roll's "not now": the subscription
	// stays due and the next pass asks again.
	//
	// A group the catalog has never opened is ErrGroupNotFound; a failure to
	// reach the Data Plane is a transport error. The wildcard group is a
	// group like any other here — its name is "*", and the catalog holds a
	// version for it from the moment the catalog is provisioned.
	CurrentGroupVersion(ctx context.Context, groupName string) (GroupVersion, error)
}

// GroupVersion is the one fact this seam carries about a catalog group: its
// name, the version that is current, and the immutable id of that version's
// snapshot — the id a commerce entitlement stores and the runtime's
// membership checks resolve against. The name is carried beside the id so a
// caller can confirm it asked about the group it meant to; the id is the
// only part with a future.
type GroupVersion struct {
	GroupName      string
	Version        int
	GroupVersionID string
}

// ErrGroupNotFound reports that the Data Plane's catalog holds no version of
// the named group. It is distinct from a transport failure because it asks
// for a different response: a transport error is retried, while a group that
// does not exist will not begin to — a grant definition naming such a group
// is a commercial catalog error, and the roll that met it must stop and say
// so rather than spin.
var ErrGroupNotFound = errors.New("the data plane's catalog holds no version of that alias group")

// ErrMalformedAnswer reports that the Data Plane answered 200 with a body
// that is not the group-version answer the contract describes — a field
// absent or null, a version below the 1 the catalog's numbering starts at,
// or an echo of a group this call did not ask about. It is the read's
// counterpart of the fact feed's ErrMalformedPage, and exists for the same
// reason: a consumer must be able to tell "the peer broke the contract"
// apart from every transport failure, because the first is a version-skew
// or a defect to stop on, and the second is a retry.
var ErrMalformedAnswer = errors.New("the data plane answered with a malformed group-version body")

// CatalogReader is the read this seam carries from the Data Plane's catalog:
// the group-version lookup the commerce roll resolves its entitlement scopes
// with. It stands beside Management rather than inside it on purpose —
// WithdrawCredential is still waiting for the use case that will call it (the
// projection that carries the revocation arrived with its own protocol beside
// this file instead), and a use case that needs only the read should not have
// to name the withdrawal it never makes.
type CatalogReader interface {
	// CurrentGroupVersion is documented on Management, where the seam's
	// operations are enumerated; the interface exists so the composition
	// root can hand the commerce use cases exactly the seam they use.
	CurrentGroupVersion(ctx context.Context, groupName string) (GroupVersion, error)
}

// Event is one immutable fact the runtime recorded, as the feed carries it.
//
// It is the seam's vocabulary and deliberately not the store's: the
// application translates an Event into a persistence.Fact, so the Control
// Plane's database never grows a column because the wire grew a field, and the
// wire never carries a type the store invented. RequestID is the fact's
// immutable business identity and the idempotency key for everything derived
// from it — it is not the HTTP X-Request-Id, which correlates one call and
// means nothing across a retry. Kind is the contract's enum as a plain string,
// because a Go copy of an enum the Data Plane's document owns is a copy that
// goes stale; the applier is what decides whether it recognizes one. Payload is
// the fact's body: the contract fixes that a settlement must be derivable from
// the fact alone, and leaves its columns to the schema the facts are stored in.
//
// The typed settlement figures travel beside the payload on the contract's
// say-so (shared/usage-facts.yaml): a settlement must be derivable from the
// fact alone, and the figures a settlement prices with are the envelope's, not
// the payload's. Every one is required on the wire and nullable in meaning —
// null is the claim that the fact makes no such claim, and a stored zero is a
// different sentence about money — so they cross as pointers: nil for the
// absence, a pointer for the figure, including a zero. Which fields a given
// kind makes non-null is the producer's vocabulary, and this port carries them
// without an opinion; AppendSeq is the one exception, required and non-null,
// because a page that cannot say where its facts stand is a page this seam
// refuses rather than delivers.
type Event struct {
	// AppendSeq is the fact's position in the Data Plane's append order and
	// its identity on the wire. It is not the consumer's idempotency key —
	// that is RequestID and the kind's class — but it is the only value that
	// names which fact a question is about.
	AppendSeq int64
	RequestID string
	Kind      string
	// SchemaVersion is the version of the payload shape the fact was written
	// with, carried through so a consumer that does not know a version can
	// refuse the fact rather than guess at settlement.
	SchemaVersion int
	// OccurredAt is when the runtime observed the outcome. Informational: the
	// ordering key is the Data Plane's append sequence, which the cursor names.
	OccurredAt time.Time
	// Payload is the fact's body, carried as the bytes the Data Plane wrote.
	Payload json.RawMessage

	// CaptureMethod names how the fact's usage figures were known. Nil where
	// the fact claims no usage.
	CaptureMethod *string
	// CommittedAttemptID names the upstream call the usage belongs to. Nil
	// where the fact names no attempt.
	CommittedAttemptID *string
	// ProviderInputTokens and ProviderOutputTokens are the provider's own
	// report as the gateway recorded it. Nil where no report arrived.
	ProviderInputTokens  *int64
	ProviderOutputTokens *int64
	// DeliveryTokens is the gateway's own count of what reached the client.
	DeliveryTokens *int64
	// PriceRevision and the two unit prices are the settlement's pricing
	// basis, present together or not at all.
	PriceRevision   *string
	InputUnitPrice  *int64
	OutputUnitPrice *int64
	// SettledAmount is what the request cost. Nil on every fact where no
	// money moved; zero is a real amount.
	SettledAmount *int64
	// CorrectsAppendSeq is reserved for the correction path. Nil on every
	// fact this generation's producer writes.
	CorrectsAppendSeq *int64
}

// Page is one replayable slice of the fact feed.
//
// NextCursor is the position of the last event in the page, and it is the Data
// Plane's to compute: a consumer stores it and sends it back as the next
// `after`, which resumes strictly after that event, and never derives one. When
// the page is empty it is the position the request carried, so a consumer that
// has caught up keeps the cursor it had; a first read that carried no `after`
// still receives one, because a page with no position is not a page this seam
// can use.
// HasMore answers whether the feed has more facts *right now*, which is why a
// page shorter than the requested limit does not mean the feed is drained — a
// concurrent writer produces a short page too.
//
// NextCursor is non-empty on every page the contract allows, and a page that
// carries none is not a page this seam can use: an implementation of UsageFacts
// must refuse it rather than return it, and a consumer holding an empty
// position would ask from the beginning of retained history on every cycle —
// re-reading the same facts forever behind a position that never moves.
type Page struct {
	Events     []Event
	NextCursor string
	HasMore    bool
}

// ErrCursorExpired reports that the position a consumer asked for is no longer
// replayable — it has fallen behind the Data Plane's retention window, or the
// value is not a position this feed ever issued. The distinction from any other
// failure is the point: the consumer's cursor is unusable and must be
// re-established by a decision (a re-bootstrap, an operator's intervention),
// whereas a transport failure is retried with the cursor it already has. A
// consumer that treated the two alike would either retry forever or silently
// resume past facts it never applied, and the second quietly loses money.
var ErrCursorExpired = errors.New("usage fact cursor is no longer replayable")

// ErrMalformedPage reports that the Data Plane answered with a page that is not
// one the contract describes — a body that would not decode as a page, a field
// the contract requires that arrived absent or null, or a position that is
// absent, empty or longer than a cursor can be.
//
// It is separate from the other failures because it is a different kind of
// event: the answer arrived, and it is wrong. A transport failure says the
// answer is unknown and the read will be retried; this says the peer broke the
// contract, and the consumer must not act on what it sent. The distinction
// matters most for the position: a consumer that stored a page's cursor anyway
// would move its durable position on the strength of a response it could not
// trust, and a position moved wrongly is facts skipped.
//
// What is checked is the page's declared shape and only that. A cursor may not
// be parsed, decoded, compared or ordered here or anywhere else, which is why
// the check is a length and not a reading: "is this a cursor" is a question the
// Data Plane answers, and "is this field present and of a legal size" is the
// most a consumer may ask without taking over that answer.
var ErrMalformedPage = errors.New("the data plane answered with a malformed usage fact page")

// UsageFacts is the fact half of the cross-plane seam, beside Projection's
// decision half. One package, both directions.
//
// The feed is a pull with replay, chosen in ADR 0006 §5 and declared in
// api/openapi/shared/usage-facts.yaml: the Control Plane remembers a position,
// asks for what follows it, and applies the answer. Three rules come with it,
// and they are this interface's rather than any adapter's —
//
//   - the cursor is opaque. `after` and Page.NextCursor are values to store and
//     to hand back verbatim, never to parse, validate, compare, order, decode
//     or synthesise: treating one as a number makes the Data Plane's encoding a
//     compatibility surface it never promised. The empty string is the one
//     meaning the Control Plane assigns to a cursor — IngestionCursor.Position
//     returns it for a consumer that has never applied anything — and it is
//     this seam's spelling of "no position", not a value the Data Plane issued;
//   - there is no acknowledgement, and no method here that could be one. The
//     facts are durable history; telling the Data Plane that a fact has been
//     consumed is not an operation this seam has, and the consumer's position
//     is the consumer's;
//   - ordering is the Data Plane's monotonic append sequence. A page is
//     ascending in that order, and never in OccurredAt: clocks disagree between
//     processes and two facts can share a millisecond.
type UsageFacts interface {
	// ReadUsageEvents returns the facts strictly after the position `after`, at
	// most limit of them. `after` is passed through untouched; an empty one
	// means from the beginning of what the Data Plane still retains, which is
	// what a consumer with no stored position asks for. The contract's limit
	// range is 1..1000, and a page shorter than it does not mean the feed is
	// drained — HasMore is what says that.
	//
	// It returns ErrCursorExpired when the position can no longer be replayed,
	// ErrMalformedPage when the answer is not a page the contract describes,
	// and a transport failure as itself. It must never skip forward to what is
	// available: a consumer that has fallen behind has to be told, because
	// resuming silently would lose the facts in between.
	//
	// A Page returned here is one the consumer may act on, which puts the
	// page's shape — every field the contract requires, and a non-empty
	// position bounded in length — inside this method's contract rather than
	// in its caller's. An implementation that handed back a page it had not
	// checked would be delegating a wire question to code that does not read
	// the wire.
	ReadUsageEvents(ctx context.Context, after string, limit int) (Page, error)
}
