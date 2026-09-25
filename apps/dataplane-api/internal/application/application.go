// Package application is the dataplane-api's use-case boundary.
//
// HTTP translates a request into a call here, and translates this package's
// result or typed error back into a response. Product domains do not exist yet:
// version is the one process fact an HTTP caller may ask the application for,
// which makes this package a boundary without pretending it is a domain.
//
// No domain package exists under this module and none is planned: the Data
// Plane's domain lives in apps/dataplane, and duplicating any part of it here
// would create a second definition of the runtime's rules — the failure mode
// this application's boundary is easiest to get wrong. What this package holds
// is the management vocabulary: the use cases a management caller may invoke,
// each one delegating to the Data Plane rather than deciding anything itself.
//
// The first of those is the usage-fact read (usageevents.go), and it is the
// shape every later one follows — validate what the transport hands over, call
// an outbound port, translate the port's failure into this package's codes.
// What it deliberately does not do is hold a position, a page or a fact: the
// Data Plane owns the feed and the Control Plane owns the cursor, so a copy
// here would be a third party's guess at both.
//
// The second is the alias-group catalog read (aliasgroups.go), which follows
// the same shape and adds one decision of its own: the version it returns is
// carried as the catalog reported it, with its membership withheld — that
// question belongs to the plane that evaluates containment.
//
// The third is the projection deliveries (projection.go), which follow that
// shape with the one difference the direction forces: the message they carry
// is the caller's bytes, judged nowhere in this process, because the grammar
// is the Data Plane's to apply and a second grammar here would drift from the
// first. They add the mirror and the change log to that list of owned facts —
// the Data Plane owns the mirror, the Control Plane the change log — and the
// refusal above holds for them too: a copy here would be a third party's guess
// at all four.
package application

import (
	"errors"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// Code is a machine-readable application failure category.
//
// Transport failures such as a malformed HTTP request or a disallowed method do
// not live here: route selection is HTTP's job, so letting it leak into the
// application would point the boundary in the wrong direction.
type Code string

const (
	// CodeNotFound says a requested application resource does not exist.
	CodeNotFound Code = "not_found"

	// CodeCursorExpired says the Data Plane can no longer replay the position a
	// usage-fact reader asked from. It is a distinct category rather than a
	// flavour of internal because the caller can act on it — a consumer that
	// sees it knows its stored position is unusable and must be reconciled —
	// and because the alternative, silently resuming from a newer position,
	// would skip facts.
	CodeCursorExpired Code = "cursor_expired"

	// CodeUpstreamUnavailable says the answer a use case needed is unknown:
	// the Data Plane could not be reached or could not be read. It is not a
	// NotFound — nothing was looked up and failed to exist — and it is not an
	// Internal, because nothing in this process is broken; the transport maps
	// it to a 502 so the failure is not reported as this application's own.
	CodeUpstreamUnavailable Code = "upstream_unavailable"

	// CodeUnsupportedVersion says a delivered projection message speaks a
	// protocol version this process's Data Plane does not support. The delivery
	// halts until both ends speak one version; it never skips. It is its own
	// category because the caller — the Control Plane's producer — can act on
	// it only by stopping, and a generic internal would tell it to retry what
	// no retry can fix.
	CodeUnsupportedVersion Code = "unsupported_version"

	// CodeInvalidRequest says a delivered projection message does not satisfy
	// the projection protocol's grammar, as the Data Plane judged it. The
	// judgement belongs to the mirror that applies the message; this process
	// carries it and never re-judges it, which is why the message is fixed and
	// carries no detail from the Data Plane's refusal.
	CodeInvalidRequest Code = "invalid_request"

	// CodeRevisionGap says a delivered batch neither continues the Data
	// Plane's applied position nor duplicates history already behind it. It is
	// refused whole — never partially applied, never silently skipped — and
	// resolving it is a human decision, because stepping the position forward
	// would strand the gap's revisions forever.
	CodeRevisionGap Code = "revision_gap"

	// CodeSnapshotRequired says the Data Plane's stored position cannot join
	// the producer's timeline — a different epoch, or no bootstrap at all. It
	// is the one projection refusal the producer resolves by itself: its next
	// cycle reads the position and delivers a snapshot. It is distinct from
	// CodeRevisionGap because the two demand opposite reflexes — one halts for
	// a human, the other is the loop's own recovery path.
	CodeSnapshotRequired Code = "snapshot_required"

	// CodeInternal says the application cannot complete a request safely.
	// Its public representation is deliberately generic at the transport edge.
	CodeInternal Code = "internal"
)

// Error is an application failure whose category is safe for a transport to
// inspect. The optional cause stays behind the boundary: Error exposes it to a
// server-side log and Unwrap exposes it to errors.Is/As, but a transport must
// never serialize it for a client.
type Error struct {
	Code    Code
	Message string
	cause   error
}

// Error implements error.
func (err *Error) Error() string {
	if err.Message != "" {
		return err.Message
	}
	if err.cause != nil {
		return err.cause.Error()
	}
	return string(err.Code)
}

// Unwrap returns the operational cause when one exists.
func (err *Error) Unwrap() error {
	return err.cause
}

// NotFound returns an error for a resource the application could not find.
// Callers supply a message that is already safe to show to a client.
func NotFound(message string) *Error {
	return &Error{Code: CodeNotFound, Message: message}
}

// Internal returns an error for an implementation failure. Its cause remains
// available to the server but the HTTP layer deliberately replaces it with the
// fixed public message "internal error".
func Internal(cause error) *Error {
	return &Error{Code: CodeInternal, cause: cause}
}

// App holds the use-cases this process currently exposes, and the ports they
// reach. It holds no position, no cache, no page and no projection state: the
// only state in it is the version string it was handed at startup. The
// projection deliveries (projection.go) are stateless in the same way the
// usage-fact read is — the mirror's position lives in the Data Plane, the
// change log lives in the Control Plane, and a copy here would be a third
// party's guess at both.
type App struct {
	version     string
	usage       dataplane.UsageFacts
	catalog     dataplane.Catalog
	projections dataplane.ProjectionDelivery
}

// New constructs the dataplane-api application around the build version supplied
// by cmd/dataplane-api and the outbound ports its use cases read through. The
// command's package-level version variable remains the one ldflags source; this
// package receives that value, never recreates it.
//
// It panics on a nil port rather than storing one. The alternative is a nil
// dereference inside the first management request this process serves, which is
// a 500 from a handler whose wiring could not have been tested; and a use case
// with nothing behind it is not a state the composition root can mean.
func New(version string, usage dataplane.UsageFacts, catalog dataplane.Catalog, projections dataplane.ProjectionDelivery) *App {
	switch {
	case usage == nil:
		panic("application: New requires a UsageFacts port — the management surface has nothing to answer with without one")
	case catalog == nil:
		panic("application: New requires a Catalog port — the group-version route has nothing to answer with without one")
	case projections == nil:
		panic("application: New requires a ProjectionDelivery port — the projection surface has nothing to deliver with without one")
	}
	return &App{version: version, usage: usage, catalog: catalog, projections: projections}
}

// Version returns the build version injected into the process. It is a plain
// value on purpose: the wire shape that carries it belongs to the transport,
// which owns every JSON envelope this service emits.
func (app *App) Version() string {
	return app.version
}

// As reports whether err is an application Error.
//
// It keeps errors.As at the boundary's owner, so HTTP knows the one predicate
// it needs without repeating a pointer-shaped type assertion at every future
// handler.
func As(err error) (*Error, bool) {
	var applicationError *Error
	if !errors.As(err, &applicationError) {
		return nil, false
	}
	return applicationError, true
}
