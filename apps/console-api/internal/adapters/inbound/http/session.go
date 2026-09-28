package http

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	stdhttp "net/http"
	"strings"
	"time"
)

// The session cookie, the double-submit token, and the four guards an unsafe
// request must clear. ADR 0012 §2 fixes the design; this file is its
// enforcement on the transport.
//
// # The session
//
// A session is an opaque, server-side, revocable credential: a 256-bit random
// id whose SHA-256 digest is what any store keeps. Opaque rather than a signed
// JWT, so a sign-out is immediate — the row is gone and the token stops working,
// rather than remaining valid until it expires. The plaintext exists in the
// Set-Cookie that mints it and in the digest computation that produced the row,
// and nowhere else: never in a store, never in a log, never in a URL.
//
// # The four guards
//
// The cookie alone is not the defence. `SameSite=Strict` is a browser control,
// and a browser control is a control over a cooperating client — so four
// independent mechanisms stand behind it, each enforceable and testable on its
// own:
//
//  1. the cookie itself: an absent, malformed, expired or revoked session is
//     unauthenticated;
//  2. an origin check on every unsafe method — `Sec-Fetch-Site` first, `Origin`
//     second — which a same-site sibling subdomain cannot satisfy;
//  3. `application/json` required and nothing else accepted, which removes the
//     form-post ambiguity entirely, because no HTML form can produce that
//     content type without a preflight;
//  4. a double-submit token the page must read and echo.
//
// Each is evaluated on its own, and a test that removes one and watches the
// other three still admit the request is the only kind worth writing: it proves
// the four are genuinely independent rather than four spellings of one check.

const (
	// sessionCookieName is the cookie the browser carries the session in. The
	// __Host- prefix is not decoration: a browser refuses to store a cookie so
	// named unless it is Secure, has Path=/ and carries no Domain, so no
	// sibling subdomain can widen the cookie's scope or overwrite this one with
	// a cookie of its own. The prefix makes the browser enforce, rather than
	// merely request, the boundaries this package asserts in prose.
	sessionCookieName = "__Host-console_session"

	// requestTokenCookieName is the double-submit token's cookie. It is
	// host-only for the same __Host- reason, which is what makes the double
	// submit work: a sibling subdomain can cause a request against this host
	// but cannot read a cookie scoped to this host, so it cannot learn the
	// value it would have to echo.
	//
	// It is deliberately NOT HttpOnly — the opposite of the session cookie. The
	// page's script must read this value to put it in the request header, which
	// is the whole of the double submit: the page that can read it is the page
	// this origin served, and a cross-origin page that can cause the request
	// cannot read it.
	requestTokenCookieName = "__Host-console_csrf"

	// requestTokenHeader is where the page echoes the double-submit token. It is
	// a header rather than a form field because a form field is readable by, and
	// submittable from, a cross-origin page — a header set by fetch() cannot
	// cross origins without a preflight, and this surface requires a content
	// type that forces one.
	requestTokenHeader = "X-Console-Csrf"

	// sessionTokenBytes is the entropy behind both a session token and a
	// double-submit token: 256 bits, from crypto/rand, rendered as 43
	// characters of unpadded base64url.
	sessionTokenBytes = 32

	// requestTokenBytes is the same 256 bits, kept as its own constant so the
	// two lengths are independently stated: they are unrelated secrets with
	// unrelated lifetimes, and a future shortening of one must not silently
	// shorten the other.
	requestTokenBytes = 32

	// sessionMaxAge bounds a session's life in the browser's eyes. The server's
	// own expiry is the row's and is what is actually enforced; Max-Age merely
	// stops the browser carrying a token this server would refuse. Eight hours
	// is a working day, chosen so a shared machine's browser does not hold
	// yesterday's session, and the same bound is applied server-side.
	sessionMaxAge = 8 * time.Hour

	// sessionEntropyUnavailable is the panic message for a failed crypto/rand
	// read. As in newRequestID: the host's entropy source failing is not an
	// input a caller can repair, and no fallback is honest — a predictable
	// token is a forgeable session.
	sessionEntropyUnavailable = "console-api session token entropy unavailable"
)

// SessionToken is the plaintext of a session cookie. It is a distinct type from
// string so a token cannot reach a log line, a URL or a struct field by
// accident: the only way to spend one is to hand it to the application, and the
// only way to hand it to a client is mintSessionCookie. Its String method
// redacts, so every fmt verb that renders a value shows nothing.
type SessionToken string

// String redacts. A session token is a live credential for as long as its row
// lives, and its value is exactly the thing a debug line prints wholesale.
func (t SessionToken) String() string { return "session token redacted" }

// GoString covers %#v with the same redaction String gives every other verb.
func (t SessionToken) GoString() string { return "console-api.SessionToken{redacted}" }

// MarshalJSON refuses, for the reason application.MintedKey refuses: a
// credential in a wire format is a credential in a log, a file or a response
// body, each with a retention policy. A session's identity travels as a cookie.
func (t SessionToken) MarshalJSON() ([]byte, error) {
	return nil, errors.New("console-api: a session token refuses serialisation: it belongs in a Set-Cookie and nowhere else")
}

// Digest is the form a session store keeps: the SHA-256 of the token. It is
// what makes a database disclosure alone insufficient — the rows carry digests,
// and a digest is not a cookie the browser will send.
func (t SessionToken) Digest() string {
	sum := sha256.Sum256([]byte(t))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newSessionToken mints a session's plaintext. It panics rather than falling
// back for the reason newRequestID does: a guessable session token is a
// forgeable session, and a silently weak one would be worse than an outage.
func newSessionToken() SessionToken { return randomToken(sessionTokenBytes) }

// newRequestToken mints a double-submit token. Same failure posture as
// newSessionToken, and for the same reason.
func newRequestToken() string { return string(randomToken(requestTokenBytes)) }

func randomToken(size int) SessionToken {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		panic(sessionEntropyUnavailable)
	}
	return SessionToken(base64.RawURLEncoding.EncodeToString(bytes))
}

// mintSessionCookie writes the Set-Cookie that carries a session token.
//
// The attributes are the contract's, exactly: HttpOnly so the browser's script
// never sees the value, Secure so the cookie is not sent over plaintext HTTP at
// all, SameSite=Strict so it is not sent cross-site, and Path=/ so it is sent
// for the whole surface. With the __Host- prefix on the name, Secure and
// Path=/ are no longer merely requested — a browser that would refuse a
// contradicting attribute refuses the cookie outright, so a deployment behind a
// plain-HTTP proxy fails at sign-in rather than quietly downgrading.
func mintSessionCookie(w stdhttp.ResponseWriter, token SessionToken) {
	cookie := &stdhttp.Cookie{
		Name:     sessionCookieName,
		Value:    string(token),
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: stdhttp.SameSiteStrictMode,
		MaxAge:   int(sessionMaxAge / time.Second),
	}
	w.Header().Add("Set-Cookie", cookie.String())
}

// mintRequestTokenCookie writes the double-submit token's cookie, and rotates
// it.
//
// Rotation is not decoration. The token is a nonce bound to one page load's
// privileges, and a token that outlived the session it was issued beside would
// let a page that was open before a privilege change keep writing after it —
// so it is re-minted on every sign-in and every privilege change, alongside the
// session, exactly as ADR 0012 §2 requires of the session itself.
//
// The HttpOnly flag is absent here on purpose and is the one place in this
// file where a cookie attribute is left off rather than set: the page's script
// must read this value, because reading it and echoing it in a header IS the
// double submit.
func mintRequestTokenCookie(w stdhttp.ResponseWriter, token string) {
	cookie := &stdhttp.Cookie{
		Name:     requestTokenCookieName,
		Value:    token,
		Path:     "/",
		Secure:   true,
		SameSite: stdhttp.SameSiteStrictMode,
		MaxAge:   int(sessionMaxAge / time.Second),
	}
	w.Header().Add("Set-Cookie", cookie.String())
}

// clearSessionCookies expires both cookies in the browser. The attributes must
// match the ones they were minted with or the browser treats each as a
// different cookie and the original survives — a sign-out that did not clear
// the cookie would leave a token on a machine the user believes they signed out
// of, which is one failure away from the outcome it claims to prevent.
//
// A sign-out answers this even when there was no session: the goal is that the
// browser stops carrying these tokens, and a browser carrying them has not
// reached it whether or not a row existed.
func clearSessionCookies(w stdhttp.ResponseWriter) {
	for _, name := range []string{sessionCookieName, requestTokenCookieName} {
		cookie := &stdhttp.Cookie{
			Name:  name,
			Value: "",
			Path:  "/",
			// HttpOnly is set on both, including the double-submit cookie that
			// was minted without it: an expiring cookie's attributes need not
			// match its original for a browser to replace it, and setting the
			// stronger flag on a deletion narrows nothing that matters.
			HttpOnly: true,
			Secure:   true,
			SameSite: stdhttp.SameSiteStrictMode,
			MaxAge:   -1,
		}
		w.Header().Add("Set-Cookie", cookie.String())
	}
}

// sessionFromRequest extracts the session token from a request's cookie.
//
// An absent, empty or wrong-length cookie is reported as absent rather than as
// an error: to the caller it is one fact — there is no usable session here — and
// the answer is the same 401 in every case, so distinguishing them at this
// level would only create a difference for something to leak. The length check
// is a cheap early refusal of anything this server did not mint, before any
// digest is computed.
func sessionFromRequest(r *stdhttp.Request) (SessionToken, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	if len(cookie.Value) != base64.RawURLEncoding.EncodedLen(sessionTokenBytes) {
		return "", false
	}
	return SessionToken(cookie.Value), true
}

// requestTokenFromRequest reads the double-submit token the page was issued,
// from the cookie the server minted beside the session.
func requestTokenFromRequest(r *stdhttp.Request) (string, bool) {
	cookie, err := r.Cookie(requestTokenCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	if len(cookie.Value) != base64.RawURLEncoding.EncodedLen(requestTokenBytes) {
		return "", false
	}
	return cookie.Value, true
}

// The four guards' refusals. Each is a distinct type so the tests can prove the
// guards are independently enforced rather than resting on one check.

// crossOriginError is the origin guard's refusal. ADR 0012 §2 and the
// contract's CrossOriginRequest response both put it at 403 with the
// invalid_request code: the request is well-formed HTTP and is refused for being
// untrusted, not for being malformed.
//
// The message is fixed. Which of the two signals was missing or wrong is a fact
// about the caller, and the only thing a cross-site prober could learn from the
// difference is what its own request already disclosed.
type crossOriginError struct{}

func (crossOriginError) Error() string { return "cross-origin request" }

func (crossOriginError) response() (int, string, string) {
	return stdhttp.StatusForbidden, "invalid_request", "the request did not come from this console"
}

// missingRequestTokenError is the double-submit token's refusal. It renders
// byte-identically to crossOriginError and is a separate type only so a test can
// show the two guards refuse independently; a client cannot tell them apart,
// and must not be able to.
type missingRequestTokenError struct{}

func (missingRequestTokenError) Error() string { return "missing or wrong request token" }

func (missingRequestTokenError) response() (int, string, string) {
	return stdhttp.StatusForbidden, "invalid_request", "the request did not come from this console"
}

// nonJSONError is the content-type guard's refusal. It is 415, not 403: the
// request is well-formed HTTP and names a representation this operation does
// not accept, which is what 415 means. It matters for the reason the contract's
// NonJSONRequest gives — a simple form post crosses origins with no preflight
// and no custom content type, so requiring a content type no form can produce
// is what removes the ambiguity the other guards alone leave open.
type nonJSONError struct{}

func (nonJSONError) Error() string { return "not an application/json request" }

func (nonJSONError) response() (int, string, string) {
	return stdhttp.StatusUnsupportedMediaType, "invalid_request", "this operation accepts application/json only"
}

// unauthenticatedError is the cookie guard's refusal. It is the same answer
// whether the cookie was absent, malformed, expired or revoked, because a
// difference is a disclosure to anyone willing to send cookies and the client
// cannot act differently on any of them anyway. The action is the same: sign in
// again.
type unauthenticatedError struct{}

func (unauthenticatedError) Error() string { return "unauthenticated" }

func (unauthenticatedError) response() (int, string, string) {
	return stdhttp.StatusUnauthorized, "unauthenticated", "this operation needs a signed-in session"
}

// originAllowed is Guard 2: the origin check, `Sec-Fetch-Site` first and
// `Origin` second.
//
// The two are consulted in that order because they differ in what a hostile
// caller can do about them. Sec-Fetch-Site is a forbidden header the browser
// sets itself and script cannot overwrite, so when it is present it is a fact
// about the browser and it answers the question completely. Origin is a header
// a non-browser client sets freely, so it is consulted only when the browser's
// own signal is absent.
//
// Both signals must affirm same-origin, and an ABSENT signal is never read as
// same-site. This is the fail-closed choice the absence of the headers forces: a
// request carrying neither is an old client, a command-line tool, or a caller
// that deliberately suppressed them, and the failure mode that keeps the guard
// worth having is to refuse it. Absence of evidence is not evidence of
// same-origin, and a guard that admits what it cannot vouch for is a guard with
// one fewer guard in front of it.
func originAllowed(r *stdhttp.Request) bool {
	// Sec-Fetch-Site first. "same-origin" is the only affirmative value this
	// surface admits. "same-site" is a sibling subdomain, which SameSite=Strict
	// does not protect against and which the double-submit token exists to
	// stop; "none" is a user-initiated navigation; "cross-site" and the two
	// unknown-value sentinels are all refusals.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return constantTimeEqual(strings.ToLower(strings.TrimSpace(site)), "same-origin")
	}
	// Sec-Fetch-Site absent: fall through to Origin. A browser sends Origin on
	// an unsafe method and it names the origin the request came from, so an
	// Origin equal to this request's own origin is same-origin. Anything else —
	// a different host, a null origin from a sandboxed frame, or no Origin at
	// all — is refused.
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	return constantTimeEqual(origin, requestOrigin(r))
}

// requestOrigin is the scheme://host this request was addressed to, which is
// what a same-origin browser puts in Origin.
func requestOrigin(r *stdhttp.Request) string {
	scheme := "https"
	if r.TLS == nil {
		// Behind a plaintext listener the only origin that can legitimately
		// match is the plaintext one. Deriving the scheme from the connection
		// rather than assuming https keeps the guard honest in a test harness
		// and in a local http deployment; the Secure cookie means no real
		// session can be established there in any case.
		scheme = "http"
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	return scheme + "://" + host
}

// requireJSONContentType is Guard 3: application/json required, nothing else
// accepted.
//
// The check applies to a request that ARRIVES with a body. An unsafe method
// with no body at all — the contract's DELETE /auth/session — has no
// representation to be wrong about, and requiring a Content-Type header on a
// body-less DELETE would make the contract's 415 unreachable for the case it
// actually describes: a body in an encoding this surface does not parse.
//
// The media type is compared on its lowercased essence alone, so a charset
// parameter is tolerated — a charset on a JSON body is a client being explicit
// about UTF-8, not a different representation — while a form encoding, a
// multipart body, or an absent type on a request that has a body are refused.
func requireJSONContentType(r *stdhttp.Request) error {
	if !requestHasBody(r) {
		return nil
	}
	mediaType := r.Header.Get("Content-Type")
	if mediaType == "" {
		return nonJSONError{}
	}
	essence, _, _ := strings.Cut(mediaType, ";")
	if !constantTimeEqual(strings.ToLower(strings.TrimSpace(essence)), "application/json") {
		return nonJSONError{}
	}
	return nil
}

// requestHasBody reports whether the request carries a body to be parsed.
//
// Content-Length is checked first because it is the cheap case: a request that
// declares zero bytes has no body whatever Transfer-Encoding suggests. A
// request with neither header is a body of unknown length — Go's server treats
// such a request as having an empty body, but a caller that set
// Transfer-Encoding without a length is asking for something the server has not
// finished receiving, so the conservative answer is that there IS a body to
// police.
func requestHasBody(r *stdhttp.Request) bool {
	if r.Body == nil {
		return false
	}
	if r.ContentLength == 0 {
		return false
	}
	if r.ContentLength > 0 {
		return true
	}
	// ContentLength is -1 for a request whose length is unknown, and there is a
	// body: an unsafe method with an unread body is one this surface must not
	// accept untyped.
	return true
}

// verifyDoubleSubmitToken is Guard 4, the half of CSRF defence a cross-origin
// page cannot reach. The page reads the token the server issued in a host-only
// cookie and echoes it in a header; a sibling subdomain can cause a request
// against this host but cannot read that cookie, so it cannot learn the value it
// would have to echo.
//
// Both halves must be present. A request with a header and no cookie is refused
// just as firmly as one with a cookie and no header: the cookie is what makes
// the header mean "this page issued me", and a header alone is a value a
// cross-origin caller chose for itself.
func verifyDoubleSubmitToken(r *stdhttp.Request) error {
	expected, ok := requestTokenFromRequest(r)
	if !ok {
		return missingRequestTokenError{}
	}
	provided := r.Header.Get(requestTokenHeader)
	if provided == "" {
		return missingRequestTokenError{}
	}
	if !constantTimeEqual(provided, expected) {
		return missingRequestTokenError{}
	}
	return nil
}

// constantTimeEqual compares two strings without leaking their contents
// through timing. It is used on the origin signals as well as on the
// double-submit token: both are attacker-supplied and both are compared
// character by character, so a comparison that returned early on a mismatch
// would be a byte-at-a-time oracle.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// guardUnsafeRequest is the middleware the route table wraps every unsafe row
// in, and the reason the guards are a property of a route rather than of a
// handler: a handler cannot forget a check it does not contain, and a new
// unsafe row cannot arrive without them.
//
// The order is a reading order, not a security ranking. Each guard is
// independently sufficient to refuse, and the first to fail is the one reported —
// which is a decision, because a caller that fixed one guard and re-read the
// response would otherwise learn from the code that changed which guard had
// stopped it. Both refusals the guards can produce (cross-origin, missing token)
// render byte-identically, so there is nothing to learn from the difference in
// the response, and the order is chosen for the reader of the log.
func guardUnsafeRequest(next stdhttp.HandlerFunc) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if !originAllowed(r) {
			writeError(w, r, crossOriginError{})
			return
		}
		if err := requireJSONContentType(r); err != nil {
			writeError(w, r, err)
			return
		}
		if err := verifyDoubleSubmitToken(r); err != nil {
			writeError(w, r, err)
			return
		}
		next(w, r)
	}
}

// isUnsafeMethod reports whether a method must clear the three cross-cutting
// guards. GET and HEAD are safe by definition — they are not supposed to change
// state — and the session cookie is the only guard they need. Everything else,
// including any method this surface does not currently serve, is unsafe, so a
// method added later inherits the guards rather than arriving unguarded.
func isUnsafeMethod(method string) bool {
	switch strings.ToUpper(method) {
	case stdhttp.MethodGet, stdhttp.MethodHead:
		return false
	default:
		return true
	}
}
