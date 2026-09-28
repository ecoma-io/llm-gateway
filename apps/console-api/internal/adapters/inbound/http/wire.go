package http

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// The wire shapes this surface renders, and the narrow seam its handlers call.
// Every type here mirrors a schema in api/openapi/shared/console.yaml field for
// field, in the same way server.go's errorEnvelope mirrors the shared
// ErrorEnvelope. The application owns the answer; these own its shape, which is
// what an inbound adapter is for.
//
// Nothing in this file imports internal/domain. The import rule
// (internal/arch/imports_test.go) forbids an inbound surface from reaching a
// domain package, because doing so would hand aggregates to the transport
// instead of letting it render its own responses. That is why the seam below
// speaks in plain strings and times rather than in identity.Principal and
// friends: the account id is the one identifier a product operation consults,
// and the session layer has already resolved it before a handler sees it.

// PrincipalClass is what a session's holder is authorised as — user acts for
// one account, operator administers several. The two are classes of a holder,
// not kinds of a credential (ADR 0012 §2, as amended): the credential kind
// says which secret authenticated, this says what the holder may do across
// accounts, and the two are orthogonal. `user` is the only class the first
// console mints, so a signed path that yields `operator` is unreachable
// rather than merely unused.
type PrincipalClass string

const (
	principalClassUser     PrincipalClass = "user"
	principalClassOperator PrincipalClass = "operator"
)

// principal is the wire shape of Principal, mirroring the contract's schema.
// It carries no credential, no session token and no secret: it is who the
// session belongs to, and the header the console renders.
type principal struct {
	Class            PrincipalClass `json:"class"`
	AccountID        string         `json:"account_id"`
	UserID           string         `json:"user_id,omitempty"`
	OperatorID       string         `json:"operator_id,omitempty"`
	Email            string         `json:"email,omitempty"`
	SessionExpiresAt string         `json:"session_expires_at,omitempty"`
}

// signInRequest is the wire shape of SignInRequest — the one unauthenticated
// write this surface accepts. The three inputs are collected in the order that
// makes email resolution total: the account id discriminates, the email
// resolves within it, and the credential is checked against the row that pair
// names (ADR 0008 §2). Never email alone.
type signInRequest struct {
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	Password  string `json:"password"`
}

// signInResponse mirrors SignInResponse: the principal and nothing else. The
// credential is in a Set-Cookie header, because a JSON body cannot set a
// cookie — so there is no token in this body to replay and nothing in it to
// cache.
type signInResponse struct {
	Principal principal `json:"principal"`
}

// mintAPIKeyRequest mirrors MintAPIKeyRequest — the operator's chosen label.
// The plaintext is not an input and cannot be: it does not exist until this call
// returns it, which is what makes the one-time rendering a property of the
// system rather than of the console's care.
type mintAPIKeyRequest struct {
	DisplayName string `json:"display_name"`
}

// apiKeyState is a key's lifecycle state, as the contract spells it.
type apiKeyState string

const (
	apiKeyStateActive  apiKeyState = "active"
	apiKeyStateRevoked apiKeyState = "revoked"
)

// apiKeyRecord mirrors the contract's APIKey — the ownership record as every
// read but the mint returns it. The credential is absent, not null: this plane
// stores no plaintext and no digest, and the Data Plane's credential record is
// written only by the Data Plane (ADR 0006 §8).
type apiKeyRecord struct {
	ID          string      `json:"id"`
	AccountID   string      `json:"account_id"`
	CreatedBy   string      `json:"created_by,omitempty"`
	DisplayName string      `json:"display_name"`
	Prefix      string      `json:"prefix"`
	State       apiKeyState `json:"state"`
	CreatedAt   string      `json:"created_at"`
	UpdatedAt   string      `json:"updated_at,omitempty"`
	RevokedAt   string      `json:"revoked_at,omitempty"`
}

// mintedAPIKeyResponse is the mint's DTO: an ownership record and its
// credential, together, exactly once. It is a hand-written type with a bespoke
// MarshalJSON because the token must appear on the wire exactly once and no
// general-purpose serialisation of a value that carries a secret is safe to
// leave to chance.
//
// The Token field is unexported by construction — see marshalMintedAPIKey — so
// a stray `json.Marshal` of the struct can never pick it up. application.MintedKey
// refuses serialisation outright and stays refusing; the token is copied to
// this DTO at the boundary and nowhere else.
type mintedAPIKeyResponse struct {
	Record apiKeyRecord
	Token  string
}

// MarshalJSON emits the credential exactly once. The contract's MintedAPIKey
// schema is the ownership record's fields plus `token`; this is the only place
// the two are joined, and it is deliberately not a struct-tag marshalling that a
// later field addition could widen by accident.
func (m mintedAPIKeyResponse) MarshalJSON() ([]byte, error) {
	// Delegate to the DTO so the record and the token are joined in exactly one
	// function. A reader who wants to know where the secret lands finds it in
	// marshalMintedAPIKey, not scattered across a struct tag and a post-hoc
	// field injection.
	record, err := json.Marshal(m.Record)
	if err != nil {
		return nil, err
	}
	token, err := quoteJSON(m.Token)
	if err != nil {
		return nil, err
	}
	// Splice the token into the record object: strip the closing brace, add
	// `,"token":…}`. The token is appended to a record the encoder has already
	// validated, so the result is a well-formed object whether or not the
	// record itself was empty.
	if len(record) == 0 || record[len(record)-1] != '}' {
		return nil, errors.New("console-api: the minted key record did not marshal to a JSON object")
	}
	body := make([]byte, 0, len(record)+len(token)+10)
	body = append(body, record[:len(record)-1]...)
	body = append(body, `,"token":`...)
	body = append(body, token...)
	body = append(body, '}')
	return body, nil
}

// quoteJSON renders a Go string as a JSON string literal, or an error if the
// value cannot be represented. encoding/json is the one implementation of string
// escaping that is unquestionably correct, and hand-rolling the escape table for
// a value that may be an email or a label would be a place a quote or a control
// character could escape the literal and corrupt the response — so this
// delegates rather than reimplements. A JSON string is a value json.Marshal
// cannot fail on, but the error is propagated rather than swallowed: a caller
// that ignored it would emit a body missing a field, and an unrenderable value
// is a bug to fix, not a condition to paper over.
func quoteJSON(value string) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// The seam. Each use case the four session operations call, declared here as
// the narrowest interface that handler needs, so this package compiles and its
// tests run without the application layer's concrete types. The application
// agent's SignIn/Session/SignOut/MintAPIKey land against these shapes; when its
// signatures settle, only the call sites in sessionHandlers.go re-point, and
// nothing above them changes.
//
// The seam takes the principal as the plain fields the wire needs (an account
// id string, a user id) rather than a domain type, for the import-rule reason at
// the top of this file.

// sessionUseCases is the four session operations, as the transport needs them.
// A missing, expired or revoked session is one outcome — not a session — so the
// sign-out and mint paths are written to tolerate it rather than to branch on
// which it was.
type sessionUseCases interface {
	// SignIn resolves (account id, email) over live rows, checks the
	// credential, and returns the session it created together with the principal
	// it belongs to. Every pre-credential failure is one uniform answer, so a
	// refusal here never distinguishes a missing account from a missing user
	// from a wrong password from an `invited` row.
	SignIn(ctx context.Context, in SignInInput) (SessionResult, error)

	// Session resolves a live session to the principal it belongs to. A missing,
	// expired or revoked session is reported as not found, because to the
	// client it is one fact and the client cannot act on the difference.
	Session(ctx context.Context, token SessionToken) (SessionResult, error)

	// SignOut ends the session a token names. It is idempotent: a sign-out with
	// no live session is not an error, so the handler answers 204 either way.
	SignOut(ctx context.Context, token SessionToken) error

	// MintAPIKey creates a key's ownership record for the principal's account
	// and returns the record together with its credential, the one time the
	// credential exists anywhere. The account comes from the principal — never
	// from the request — so there is no account for a caller to name.
	MintAPIKey(ctx context.Context, in MintAPIKeyInput) (MintedAPIKeyResult, error)
}

// SignInInput is what a sign-in request carries across the seam. It is the
// wire shape's own fields, unchanged, so nothing is lost or renamed in
// translation and the application reads exactly the contract's three inputs.
type SignInInput struct {
	AccountID string
	Email     string
	Password  string
}

// SessionResult is a live session's answer: the principal it names and the
// plaintext token that addresses it. The token is returned to the transport
// only so it can be written into a Set-Cookie; it is never serialized, never
// logged, and never persisted here.
type SessionResult struct {
	Principal principal
	Token     SessionToken
	// ExpiresAt is the moment the session stops being accepted. It is rendered
	// into the principal so a client may show it, and it is enforced by the
	// application on every use — a client's clock is not consulted.
	ExpiresAt time.Time
}

// MintAPIKeyInput is a mint's request across the seam. The account is not an
// input: it is the principal's, carried inside the principal, so a caller has
// no field in which to name another account's.
type MintAPIKeyInput struct {
	Principal principal
	// The creator is the session's own user, derived from the principal; there
	// is no request field for it either.
	DisplayName string
}

// MintedAPIKeyResult is a mint's answer: the ownership record and the
// credential, together, once. The credential is a plain string here so the
// transport can copy it into its bespoke DTO at the boundary — the
// application.MintedKey refusal stands, and this struct is where the copy
// lands.
type MintedAPIKeyResult struct {
	Record apiKeyRecord
	Token  string
}
