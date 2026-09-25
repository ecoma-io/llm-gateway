package http

import (
	"errors"
	stdhttp "net/http"
	"strings"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
)

// The credential half of the chat completion transport: what it takes to turn
// an Authorization header into a verified key identity, and the line where
// the transport's share of that work ends.
//
// The split down the middle of authentication is the point of this file. The
// transport owns the header's SHAPE — one Authorization value, spelling a
// bearer credential, two whitespace-separated fields — because a request that
// fails it has presented nothing verifiable at all, and deciding what a
// malformed header means for admission is not a decision anyone delegated to
// the use case. Everything after the shape — the token's grammar, the mirror
// read, the constant-time comparison, the key's lifecycle, the account's — is
// verification, and verification belongs to the application's Authenticator,
// which answers from the credential mirror and nothing else.
//
// Both halves refuse with the same wire cell: one message, one class, one
// code, for every cause on either side of the split. The contract promises
// that the answer never says which part of verification failed, and a promise
// kept by making every refusal pass through one constant is a promise no
// later edit can quietly unkeep.

// authorizationHeader is the header a caller presents its API key in, and
// bearerScheme is the spelling of the credential type it must name. The
// scheme comparison is case-insensitive because the HTTP authentication
// scheme is case-insensitive by specification; the credential after it is
// compared byte-for-byte and never normalized.
const (
	authorizationHeader = "Authorization"
	bearerScheme        = "bearer"
)

// unauthenticatedRequest is the transport fact that the request presented no
// credential the runtime could even address — or that the application's
// verification refused the one it presented. Both halves refuse through this
// one type so that the wire cell has one definition; label is the server-side
// log token naming the cause, never serialized.
//
// The refusal writes nothing anywhere: no request row, no intake record. A
// credential that was never verified never became a request, so a retry with
// a good key is a first attempt and not a replay of anything.
type unauthenticatedRequest struct {
	label string
}

func (unauthenticatedRequest) Error() string {
	return "unauthenticated"
}

func (unauthenticatedRequest) wireFailure() wireFailure {
	return wireFailure{
		status: stdhttp.StatusUnauthorized,
		body: runtimeErrorBody{
			Message: invalidAPIKeyMessage,
			Type:    typeAuthenticationError,
			Code:    stringCode(codeInvalidAPIKey),
		},
	}
}

// parseCredential extracts the presented credential from the request's
// Authorization header, or reports that the header does not spell one.
//
// The shape requirements are exactly three, and deliberately only three: the
// header appears exactly once (a repeated header has no single credential to
// verify, and choosing one would invent a request the caller did not send);
// its value splits into exactly two whitespace-separated fields; and the
// first field names this credential type, case-insensitively. The second
// field is the credential, carried as presented — no trimming, no folding,
// because the presented bytes are what verification digests, and a
// normalised credential is a different credential.
func parseCredential(r *stdhttp.Request) (string, bool) {
	values := r.Header.Values(authorizationHeader)
	if len(values) != 1 {
		return "", false
	}
	fields := strings.Fields(values[0])
	if len(fields) != 2 || !strings.EqualFold(fields[0], bearerScheme) {
		return "", false
	}
	return fields[1], true
}

// authenticate resolves the presented credential through the application's
// Authenticator, translating the two answers verification can give into the
// transport's two: a refusal that maps onto the unauthenticated cell, and
// anything else — a mirror that would not answer, a row it cannot parse —
// which is never a verdict about the caller and is returned as the failure
// it is.
func authenticate(auth application.Authenticator, r *stdhttp.Request, credential string) (application.AuthenticatedCredential, error) {
	authenticated, err := auth.Authenticate(r.Context(), credential)
	if err == nil {
		return authenticated, nil
	}
	var refusal *application.Unauthenticated
	if errors.As(err, &refusal) {
		return application.AuthenticatedCredential{}, unauthenticatedRequest{label: unauthenticatedLabel(refusal.Reason)}
	}
	return application.AuthenticatedCredential{}, err
}

// unauthenticatedLabel returns the one log token a refusal cause earns. The
// absent-account case is the only one an operator can act on from the log —
// the account row is missing beside its credential, which is a mirror
// integrity fact, and the token names the column whose row is missing, which
// is what an operator greps for when a snapshot goes wrong. The other causes
// earn nothing: the wire cell is deliberately cause-blind, and a per-cause
// log line would rebuild the probe the fixed message just refused to be.
func unauthenticatedLabel(reason application.UnauthenticatedReason) string {
	if reason == application.ReasonAccountAbsent {
		return "account_state=absent"
	}
	return ""
}
