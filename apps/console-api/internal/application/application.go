// Package application is the console-api's use-case boundary.
//
// HTTP translates a request into a call here, and translates this package's
// result or typed error back into a response. Two product domains live here
// so far — identity, the ownership root, and commerce, the commercial
// authority — beside version, the one process fact an HTTP caller may ask the
// application for today.
//
// Every domain lands in an internal/domain package this one depends on.
// Nothing here reaches for an adapter: the outbound ports this application
// calls live in internal/ports/outbound, and a concrete PostgreSQL, Valkey or
// HTTP type appearing in this package would be the boundary failing, not a
// shortcut.
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

	// CodeInvalidRequest says a request is well-formed HTTP and its inputs do
	// not satisfy the contract: a page size outside the contract's bounds, a
	// cursor this surface cannot place, a cursor minted under filters the
	// request no longer makes, a filter value outside its closed vocabulary.
	// It is a refusal rather than a correction in every one of those cases — a
	// page the caller did not ask for is one it cannot tell apart from a page
	// the collection capped itself.
	CodeInvalidRequest Code = "invalid_request"

	// CodeUnauthenticated says the caller did not identify itself as a service
	// this surface accepts: neither a live session nor a service credential.
	// It is named here because every product operation is behind it, but the
	// refusal itself is the session surface's to make.
	CodeUnauthenticated Code = "unauthenticated"

	// CodeConflict says the request is well-formed and the server's own state
	// refuses it — the caller asked for something that is not available from
	// here, and asking again in the same way will fail the same way until
	// something else changes.
	//
	// It is separated from CodeInvalidRequest because the two call for
	// different client behaviour, and conflating them makes one of those
	// behaviours wrong. An invalid request is the client's mistake and its
	// correct response is to change the request; a conflict is not the
	// client's mistake, and a client that "fixed" it by mutating its payload
	// would be guessing at server state it cannot see. A payment top-up asked
	// for on a suspended account is the case this exists for: there is
	// nothing wrong with the request, and there is no edit to it that would
	// help.
	CodeConflict Code = "conflict"

	// CodeUpstreamUnavailable says a dependency this plane does not control —
	// the payment provider — did not answer, or answered that it is unwell.
	//
	// It is separated from CodeInternal because the two call for opposite
	// client behaviour, and this is the only code in the vocabulary whose
	// correct answer is RETRY. The condition is the provider's, it is expected
	// to clear, and the request was never faulted: a client that retried an
	// internal error would be reporting a bug, and a client that did not retry
	// this one would be abandoning a top-up that is about to become possible.
	// The contract names both directions — "the caller retries later rather
	// than differently" — and the durable payment it leaves behind is what
	// makes that retry converge on the payment it already has.
	//
	// It is NOT a 5xx for the payment: nothing about the payment is unwell, and
	// the payment exists in `created` either way.
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

// InvalidRequest returns an error for a request whose inputs do not satisfy
// the contract. The message names the offending parameter and never whether
// the resource behind it exists.
func InvalidRequest(message string) *Error {
	return &Error{Code: CodeInvalidRequest, Message: message}
}

// Conflict returns an error for a request the server's own state refuses. The
// message says what is not available from here and never what would make it
// available, because that is a decision an operator makes rather than a client.
func Conflict(message string) *Error {
	return &Error{Code: CodeConflict, Message: message}
}

// invalidRequest is InvalidRequest for this package's own refusals, where the
// message is written at the point that knows what was wrong with the input.
func invalidRequest(message string) *Error {
	return &Error{Code: CodeInvalidRequest, Message: message}
}

// Unauthenticated returns an error for a caller that did not identify itself
// as a service this surface accepts. The message is the caller's to write,
// because a session's failure has a reason worth naming.
func Unauthenticated(message string) *Error {
	return &Error{Code: CodeUnauthenticated, Message: message}
}

// UnresolvedCredential is Unauthenticated for a credential whose failure must
// not describe what it failed against. A session belongs to a browser that
// can be told "no user is signed in"; a bearer credential belongs to a client
// that would learn the account list by reading the refusals, so the message
// here is fixed and says only that the credential did not resolve.
func UnresolvedCredential() *Error {
	return &Error{Code: CodeUnauthenticated, Message: "the credential presented does not resolve to an account"}
}

// Internal returns an error for an implementation failure. Its cause remains
// available to the server but the HTTP layer deliberately replaces it with the
// fixed public message "internal error".
func Internal(cause error) *Error {
	return &Error{Code: CodeInternal, cause: cause}
}

// UpstreamUnavailable returns an error for a dependency that did not answer.
// The cause is kept for the server and never serialized, for the same reason
// Internal's is: a provider's own prose is written for an operator holding a
// credential, and the status, the code and the request identifier are what a
// caller acts on.
func UpstreamUnavailable(cause error) *Error {
	return &Error{Code: CodeUpstreamUnavailable, cause: cause}
}

// App holds the use-cases this process currently exposes. It is concrete on
// purpose: there is no infrastructure below it to substitute yet, and tests
// drive the real handler and real application rather than a speculative mock.
type App struct {
	version string
}

// New constructs the console-api application around the build version supplied
// by cmd/console-api. The command's package-level version variable remains the
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
