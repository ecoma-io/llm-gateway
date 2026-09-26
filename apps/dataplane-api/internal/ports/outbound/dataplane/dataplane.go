// Package dataplane is the dataplane-api's outbound port to the Data Plane: the
// one seam through which this façade reaches the process whose facts it serves.
//
// The façade is a transport, and this package is what keeps it one. Nothing
// here resolves a client, dials a socket or holds a byte: the port declares the
// things this application may ask of the Data Plane — read a page of usage
// facts, read the current version of an alias group, and tell whether a caller
// is a service this deployment trusts — and the adapter under
// internal/adapters/outbound decides how each is answered. Because the ports
// are interfaces, the concrete HTTP client is chosen once, in cmd/dataplane-api,
// and this package never learns that the answer arrives over a network
// connection at all.
//
// The types are declared here rather than imported from apps/dataplane, and
// that is the plane boundary rather than a copy that drifted apart. The two
// applications are separate Go modules (ADR 0006 §1), so no import path exists
// through which this one could reach the runtime's types; what crosses is a
// call over an interface. The day the Data Plane changes its own port, nothing
// here moves until someone decides it should.
//
// One rule is load-bearing enough to be written into the method rather than
// left to its comment: `after` is an opaque cursor. This package may not parse
// it, compare it, order by it or synthesise one — it travels as the string it
// is, from the caller that stored it to the Data Plane that issued it. The
// cursor's encoding is the Data Plane's to change, and treating the value as
// meaningful is precisely what would make that change breaking
// (api/openapi/shared/usage-facts.yaml).
//
// What is deliberately absent is as much a part of the port as what is here.
// There is no acknowledgement, no consume and no delete: the facts are durable
// history, replay is normal, and the position a consumer has applied through
// belongs to the consumer. A method that destroyed a fact would turn a
// retryable read into a one-shot delivery and make a Control Plane outage lose
// money.
package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Event is one immutable fact the runtime recorded, as the Data Plane reports
// it. The envelope fields are the wire contract's
// (api/openapi/shared/usage-facts.yaml); what is inside Payload is the fact
// schema's business and not this application's.
//
// The typed settlement figures travel beside the payload on the contract's
// say-so: a settlement must be derivable from the fact alone, and the figures
// a settlement prices with are the envelope's, not the payload's. This
// application carries them without an opinion — which fields a given kind
// makes non-null is the producer's vocabulary, and this transport's only
// obligation to them is fidelity: a null crosses as nil, a stored zero as a
// pointer to zero.
type Event struct {
	// AppendSeq is the fact's position in the Data Plane's append order and
	// its identity on the wire. It is not the consumer's idempotency key —
	// that is RequestID and the kind's class — but it is the only value that
	// names which fact a question is about, and this surface requires it:
	// a page that cannot say where its facts stand is a page this
	// application cannot answer from.
	AppendSeq int64
	// RequestID is the runtime request this fact is about, and the logical
	// idempotency key for everything derived from it. It is not the HTTP
	// X-Request-Id: one correlates a call, the other identifies the business
	// event, and confusing them would make a retry look like a second charge.
	RequestID string
	// Kind is the terminal outcome the fact records. This application carries it
	// without an opinion: which kinds it recognises is a property of the
	// consumer's schema version, not of the transport in between.
	Kind string
	// SchemaVersion is the version of the payload shape the fact was written
	// with, carried through so a consumer that does not know a version can
	// refuse the fact rather than guess at settlement.
	SchemaVersion int
	// OccurredAt is when the runtime observed the outcome. Informational: the
	// ordering key is the Data Plane's append sequence, which the cursor names,
	// and clocks between processes agree only approximately.
	OccurredAt time.Time
	// Payload is the fact's body, carried as the bytes the Data Plane wrote. It
	// is raw rather than decoded because decoding it here would be a second
	// definition of a shape this application never reads.
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

// Page is one replayable slice of the feed: the facts, the position of the last
// of them, and whether more exist. It is exactly what the Data Plane answered,
// and this port attaches no meaning to any part of it.
type Page struct {
	// Events are the facts in ascending append order, strictly after the
	// requested position.
	Events []Event
	// NextCursor is the position of the last fact above, which a consumer stores
	// and sends back as `after` once every one of them is applied — the next read
	// resumes strictly after it. It is never empty: when the page carried no
	// facts it is the position the request was read from, and the Data Plane
	// answers with one even for a first read that carried no `after`.
	NextCursor string
	// HasMore reports whether facts exist beyond this page. False means the
	// consumer has reached the end of what the Data Plane has committed so far,
	// not that the feed is closed.
	HasMore bool
}

// ErrCursorExpired reports that the Data Plane can no longer replay the
// requested position. It is a refusal and never a skip forward: a consumer that
// resumed from a newer position would silently lose the facts between the two,
// which is lost settlement rather than a cheaper answer.
var ErrCursorExpired = errors.New("usage fact cursor is no longer replayable")

// ErrUpstreamUnavailable reports that the Data Plane could not be read — it
// could not be reached, it answered with a status this façade cannot use, its
// answer could not be decoded, or the page it answered with is missing one of
// the fields this surface contracts as required. All four are one condition
// from a caller's point of view: the answer is unknown, and this process holds
// nothing it could answer with instead.
var ErrUpstreamUnavailable = errors.New("the data plane is unavailable")

// UsageFacts is the fact half of the cross-plane seam: the one read this
// application makes of the Data Plane.
type UsageFacts interface {
	// ReadUsageEvents returns one page of the feed, strictly after the position
	// after names and at most limit facts long. An empty after means "from the
	// beginning of what is retained".
	//
	// The implementation sends after and limit and returns what the Data
	// Plane said. This port has no position of its own, and an implementation
	// that remembered one — a cached cursor, a buffered last page, a
	// next_cursor computed locally — would be the façade holding state it must
	// not, because a position this process invented is a position the Control
	// Plane could skip facts from.
	//
	// A cursor the Data Plane can no longer place is ErrCursorExpired; anything
	// that stops the page being read is ErrUpstreamUnavailable.
	ReadUsageEvents(ctx context.Context, after string, limit int) (Page, error)
}

// GroupVersion is one version of a Data Plane alias group, as the catalog
// reports it: the immutable snapshot a Control Plane entitlement pins.
//
// The three fields are the whole of what the read contracts (dataplane.yaml,
// CurrentAliasGroupVersion), and the one a caller is expected to keep is the
// id. The name and the number name what was read; the number in particular
// describes the group as it stands now — highest of the group's versions — and
// not the snapshot forever, which is why a scope that stored it instead of the
// id would change meaning the next time the group rolled.
type GroupVersion struct {
	// GroupName is the alias-group name the version belongs to, as the catalog
	// stores it — the requested name, and `*` for the wildcard group's
	// singleton. The reserved name is an ordinary value here: this port carries
	// group names and has no opinion about which of them are special.
	GroupName string
	// Version is the version's number within its group. Information rather
	// than a handle.
	Version int
	// GroupVersionID is the version's immutable identifier, and the value an
	// entitlement stores as its scope. A version, once issued, never changes.
	GroupVersionID string
}

// ErrGroupVersionNotFound reports that the Data Plane holds no version of the
// alias group a read named. It is an answer and not a failure: the read
// completed, the catalog's answer is that there is nothing to pin, and the
// caller's next move is to act on that — not to retry a condition no retry
// changes. It is the one outcome of the catalog read that this façade is
// allowed to describe to its caller in the caller's own vocabulary; everything
// else about the read is this process's condition, not the caller's.
var ErrGroupVersionNotFound = errors.New("the data plane holds no version of that alias group")

// Catalog is the catalog half of the cross-plane seam: the read a Control
// Plane commerce roll makes before it can write anything of its own.
type Catalog interface {
	// CurrentGroupVersion returns the alias group's current version — the
	// group's highest, under the catalog's own rule that a membership change
	// opens a new version rather than editing one. The name crosses exactly as
	// it is given, `*` included: the wildcard is an ordinary group name to the
	// catalog, and a façade that special-cased it would be deciding catalog
	// policy the catalog owns.
	//
	// No version of the group is ErrGroupVersionNotFound; anything that stops
	// the read — a listener that did not answer, an answer that is not a
	// version — is ErrUpstreamUnavailable. The membership of the version does
	// not cross this port at all: which aliases a snapshot contains is the
	// runtime's question, and an entitlement resolves what it grants by id.
	CurrentGroupVersion(ctx context.Context, groupName string) (GroupVersion, error)
}

// ServiceCaller identifies the peer application making a management call.
//
// It is deliberately a name and nothing else. There is no account, no role and
// no scope: this surface admits peer applications, not users, and an
// authorisation model would be invented rather than needed — the deployment
// trusts one caller, and the only decision to make is whether this call is from
// it.
type ServiceCaller struct{ Name string }

// Authenticator decides whether a caller is a service this deployment trusts.
//
// The credential is a shared secret supplied by deployment configuration, and
// the contract every implementation must keep is fail-closed: false means the
// caller is refused, and an implementation that cannot decide — no credential
// presented, a credential it cannot compare, a transport that proves nothing —
// returns false rather than admitting a request it could not place.
//
// The authentication transport is expected to be replaced without this
// interface changing. mTLS or a signed credential establishes the same fact
// cryptographically rather than by shared secret, and when it does, the
// application's semantics stay exactly as they are: a call is either from a
// service this deployment trusts or it is refused before any use case runs.
// That is the reason the decision is an interface here rather than a comparison
// inlined into the route.
type Authenticator interface {
	Authenticate(ctx context.Context, credential string) (ServiceCaller, bool)
}
