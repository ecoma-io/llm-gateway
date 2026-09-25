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
// reach. It holds no position, no cache and no page: the only state in it is
// the version string it was handed at startup.
type App struct {
	version string
	usage   dataplane.UsageFacts
	catalog dataplane.Catalog
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
func New(version string, usage dataplane.UsageFacts, catalog dataplane.Catalog) *App {
	switch {
	case usage == nil:
		panic("application: New requires a UsageFacts port — the management surface has nothing to answer with without one")
	case catalog == nil:
		panic("application: New requires a Catalog port — the group-version route has nothing to answer with without one")
	}
	return &App{version: version, usage: usage, catalog: catalog}
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
