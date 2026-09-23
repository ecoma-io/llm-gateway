package http

import (
	"context"
	"crypto/subtle"
	stdhttp "net/http"
	"strings"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// The service-caller check: one decision, in one file, made before any use case
// runs.
//
// This application's callers are peer applications inside the deployment, not
// people. There is no identity system behind this file — no user, no session,
// no role, no scope and no token issuance — and none is missing: the deployment
// names one peer it trusts and hands both sides the same secret, so the whole
// question is whether the credential on this request is that secret. Building
// an authorisation model for one trusted caller would be inventing a shape
// nothing needs, and an invented shape is one the next change has to keep.
//
// The credential is a shared secret because that is the cheapest thing that can
// work today, not because it is the target: mTLS or a signed credential proves
// the same identity cryptographically rather than by possession of a string,
// and either can replace the implementation below without touching a route, a
// use case or a single line of application code. When it does, the port
// (dataplane.Authenticator) and the fail-closed order — verify, then act — are
// what make the replacement a local change.
//
// What the check must never do is fail open. Every path that cannot decide —
// no header, a header with no scheme, a scheme that is not the contract's, a
// credential that does not match, an empty configured secret — refuses the
// call. The refusal is a 401 with the contract's envelope, and the credential
// itself appears in no log line, no error message and no response body: a
// secret that reaches a log is a secret that has left the deployment.

const (
	// credentialHeader is the request header carrying the service credential.
	// The scheme and its spelling come from the `serviceCredential` security
	// scheme in api/openapi/dataplane.yaml — `type: http, scheme: bearer`.
	credentialHeader = "Authorization"

	// credentialScheme is that scheme. It is compared case-insensitively,
	// because HTTP authentication schemes are case-insensitive by
	// specification and refusing `bearer` would be inventing a rule the
	// contract does not state.
	credentialScheme = "Bearer"

	// serviceCallerName names the peer this deployment trusts: the Control
	// Plane's API, which reaches this surface through the Data Plane's
	// management seam (ADR 0006 §5, §11). One deployment, one configured
	// credential, one peer — so the name is a constant rather than a second
	// configuration value, and nothing branches on it today. The day a
	// deployment trusts two peers, this is the line that becomes a field, in
	// the change that needs it.
	serviceCallerName = "console-api"
)

// serviceAuthenticator is the deployment's answer to "is this caller ours": one
// configured secret, compared against the one presented.
type serviceAuthenticator struct {
	credential string
}

// NewServiceAuthenticator returns the dataplane.Authenticator backed by the
// deployment's shared secret.
//
// An empty credential authenticates nobody, including an empty presented one.
// That case is unreachable through internal/config, which refuses an empty
// value before the process listens, and it is guarded anyway because fail-open
// is the one failure this file may not have: a deployment that lost its secret
// must stop serving, not start admitting.
func NewServiceAuthenticator(credential string) dataplane.Authenticator {
	return serviceAuthenticator{credential: credential}
}

// Authenticate implements dataplane.Authenticator. The decision is a
// constant-time comparison because the alternative leaks the secret one byte at
// a time: `==` on strings returns at the first differing byte, so the time it
// takes to say no is a function of how much of the credential the caller
// guessed right, which is a credential recoverable by patience.
//
// The context is unused: the comparison needs nothing outside this process, and
// a future implementation that does need it — an mTLS handshake, a signed
// credential verified against a key set — receives it here without a change to
// the port or to any caller.
func (a serviceAuthenticator) Authenticate(_ context.Context, credential string) (dataplane.ServiceCaller, bool) {
	if credential == "" || a.credential == "" {
		return dataplane.ServiceCaller{}, false
	}
	if subtle.ConstantTimeCompare([]byte(credential), []byte(a.credential)) != 1 {
		return dataplane.ServiceCaller{}, false
	}
	return dataplane.ServiceCaller{Name: serviceCallerName}, true
}

// requireServiceCaller wraps a handler in the check, so that a route which
// needs a verified caller cannot be written without one: the decision is at the
// composition site in routes.go rather than the first line of a handler a later
// edit could move below the work it guards.
func requireServiceCaller(authenticator dataplane.Authenticator, next stdhttp.HandlerFunc) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if !authenticatedServiceCaller(r, authenticator) {
			writeError(w, r, unauthenticatedError{})
			return
		}
		next(w, r)
	}
}

// authenticatedServiceCaller reports whether the request carries the
// deployment's credential. It returns no identity: nothing here authorises by
// name, and the port's ServiceCaller is dropped at this line rather than
// threaded through a request context that no use case reads.
//
// A repeated header is refused rather than resolved. Two Authorization headers
// are two claims, HTTP gives no rule for which one wins, and picking one would
// mean the answer depends on which side of the connection is parsing — exactly
// the difference a request smuggled through a proxy exploits.
//
// The header itself is split on any run of whitespace (RFC 6750 allows
// optional whitespace between the scheme and the credential), and the
// credential may contain none: the two parts of a bearer header are the
// scheme and the token, nothing else. This is the same shape the Data Plane's
// management listener parses, so a credential the caller can present to one
// hop can be presented to the other.
func authenticatedServiceCaller(r *stdhttp.Request, authenticator dataplane.Authenticator) bool {
	values := r.Header.Values(credentialHeader)
	if len(values) != 1 {
		return false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], credentialScheme) {
		return false
	}
	_, ok := authenticator.Authenticate(r.Context(), parts[1])
	return ok
}

// unauthenticatedError is the transport fact that the caller did not present a
// service credential this deployment trusts. It maps itself, like the other
// transport failures, because it is a fact about the request rather than
// something a use case decided.
type unauthenticatedError struct{}

func (unauthenticatedError) Error() string {
	return "unauthenticated"
}

func (unauthenticatedError) response() (int, string, string) {
	return stdhttp.StatusUnauthorized, "unauthenticated", "the caller did not identify itself as a service this deployment accepts"
}
