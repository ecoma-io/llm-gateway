// Package application is the dataplane's use-case boundary.
//
// HTTP translates a request into a call here, and translates this package's
// result or typed error back into a response. Product domains do not exist yet:
// version is the one process fact an HTTP caller may ask the application for,
// which makes this package a boundary without pretending it is a domain.
//
// What the runtime's domain eventually becomes — the model catalog, routing,
// providers, egress, execution, usage capture and the quota projection — lands
// in an internal/domain package this one depends on. Nothing here reaches for
// an adapter: the outbound ports this application would call live in
// internal/ports/outbound, and a concrete PostgreSQL or Valkey type appearing
// in this file would be the boundary failing, not a shortcut.
//
// The Control Plane's vocabulary never appears here — no account, no session,
// no subscription, no ledger entry. A request is admitted against what the
// runtime already knows; reconciling that with what the Control Plane decides
// is a fact published back, not a call made out (ADR 0006 §5).
package application

import "errors"

// Code is a machine-readable application failure category.
//
// Transport failures such as a malformed HTTP request or a disallowed method do
// not live here: route selection is HTTP's job, so letting it leak into the
// application would point the boundary in the wrong direction.
type Code string

const (
	// CodeNotFound says a requested application resource does not exist.
	CodeNotFound Code = "not_found"

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

// App holds the use-cases this process currently exposes. It is concrete on
// purpose: there is no infrastructure below it to substitute yet, and tests
// drive the real handler and real application rather than a speculative mock.
type App struct {
	version string
}

// New constructs the dataplane application around the build version supplied
// by cmd/dataplane. The command's package-level version variable remains the
// one ldflags source; this package receives that value, never recreates it.
func New(version string) *App {
	return &App{version: version}
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
