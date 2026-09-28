package http

import (
	"encoding/json"
	"errors"
	"io"
	stdhttp "net/http"
)

// The four session operations, as handlers. Each owns its wire *shape* — the
// status, the body, the headers — and calls the seam for the *answer*. What the
// answer is belongs to the application; ADR 0012 §2 fixes the session design and
// api/openapi/console.yaml fixes every shape below.
//
// What these handlers deliberately do not do: none of them reads an account id
// from a path or a query parameter. The account comes from the session, and
// SignInInput is the one place an account id enters the surface — as the
// discriminator a sign-in is *resolved* against, not as a scope a caller names.
// There is no `/accounts/{id}/…` route here, and nothing in these handlers
// leaves a place for one to be threaded in.

// signInFailureMessage is the one message every pre-credential sign-in refusal
// carries.
//
// ADR 0008 §2 and the contract's 401 description both require it: no such
// account, no such user, a removed user, a wrong credential and an `invited` row
// are five causes with one answer, because a difference between them — in
// status, code, message, or wall time — is a disclosure to anyone willing to
// submit guesses, and it would put the account's own existence on the wire.
//
// The seam is required to return one indistinguishable error for all five, and
// the handler does not weaken that by forwarding whatever a future application
// error happens to say: this handler is the last place the answer is written, so
// it is the last place the uniformity can be lost. That is why this constant
// exists rather than a pass-through — the uniformity is enforced here, at the
// boundary, and not merely requested of the layer below.
const signInFailureMessage = "the account, email or credential was not accepted"

// maxRequestBodyBytes bounds how much of a request body is read. It is well
// above any legitimate payload on this surface — a sign-in is three short
// fields, a mint is one label of at most 256 runes — and it exists so a caller
// cannot make this server buffer an unbounded body in order to be told its body
// was the wrong shape. The reader refuses at the limit, and the refusal is a 400
// like any other malformed body.
const maxRequestBodyBytes = 8 << 10

// decodeJSONBody reads a request body as one well-formed JSON object into
// target, and refuses anything else with the 400 the contract describes.
//
// The content-type guard has already run by the time this is called, so this is
// the second half of the same requirement: a body that claims to be JSON and
// then is not is malformed, which is 400, not untrusted, which is 415 or 403.
// Neither message names an account: a credential that has not been checked
// cannot be the thing the answer is about.
func decodeJSONBody(w stdhttp.ResponseWriter, r *stdhttp.Request, target any) error {
	if r.Body == nil {
		return &badRequestError{cause: errors.New("the request had no body")}
	}
	decoder := json.NewDecoder(stdhttp.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err := decoder.Decode(target); err != nil {
		return &badRequestError{cause: err}
	}
	// A second value in the stream means the caller sent more than the single
	// object the contract describes. Reading it and refusing keeps
	// `{"a":1}{"b":2}` from parsing as the first object and silently ignoring
	// the rest.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &badRequestError{cause: errors.New("the body carried more than one JSON value")}
	}
	return nil
}

// badRequestError is the one refusal a malformed body produces. The contract's
// 400 for a sign-in says the message names the offending field and never
// whether the account exists; the message here is fixed and shared, which is
// the same discipline from the other direction — a per-field message is a
// field-by-field oracle for which fields the server validates, and there is
// nothing a caller could do with that distinction except probe.
type badRequestError struct{ cause error }

func (e *badRequestError) Error() string {
	if e.cause == nil {
		return "invalid request"
	}
	// The cause is for the server's own log, never the wire: writeError writes
	// the fixed message from response() and never this string.
	return "invalid request: " + e.cause.Error()
}

func (e *badRequestError) response() (int, string, string) {
	return stdhttp.StatusBadRequest, "invalid_request", "the request does not satisfy the contract"
}

// handleSignIn is the one unauthenticated write on this surface and the only
// way a session comes into existence.
//
// It is the one unsafe method that is not behind a session cookie, which is
// exactly why its row is marked guarded in routes.go: the attack those guards
// stop here is login CSRF — a cross-origin page that silently signs a victim in
// as an attacker, so the victim then types credentials into an account the
// attacker chose. SameSite=Strict does not help, because what the victim is
// shown is the attacker's response, not this service's.
//
// The guards themselves are applied at the mount, so nothing here reads a
// header or a cookie for them: a request that failed one never reaches this
// function, and its body was never read.
func handleSignIn(useCases sessionUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		var in signInRequest
		if err := decodeJSONBody(w, r, &in); err != nil {
			writeError(w, r, err)
			return
		}

		// The conversion, not a struct literal: the wire shape and the seam's
		// input are the same three fields under the same names, and a literal
		// restating them would be a second place to forget one.
		result, err := useCases.SignIn(r.Context(), SignInInput(in))
		if err != nil {
			// Every pre-credential cause lands here, and every one of them
			// leaves with the same status, the same code and the same message.
			// The uniformity is this handler's to keep, not merely the
			// application's to promise.
			writeSignInRefusal(w, r)
			return
		}

		// The session and its double-submit token are minted together here, at
		// the transport, because both are credentials this server issued rather
		// than facts the application owns: the application decides WHO the
		// session belongs to and that it may exist, and the transport is the
		// only layer that can put a cookie on the wire.
		//
		// They are minted as a pair and rotated as a pair. The double-submit
		// token is a nonce bound to this page load's privileges, so a token that
		// outlived the privilege change that ended them would let a page opened
		// before it keep writing after it.
		requestToken := newRequestToken()
		mintSessionCookie(w, result.Token)
		mintRequestTokenCookie(w, requestToken)

		// The body carries the Principal and nothing else. A JSON body cannot
		// set a cookie, so the credential exists in exactly one place on this
		// response — the Set-Cookie — and there is no second copy for a cache to
		// keep.
		writeJSON(w, stdhttp.StatusOK, signInResponse{Principal: result.Principal})
	}
}

// writeSignInRefusal is the one place a sign-in's failure is written, so the
// uniformity of the five pre-credential causes is stated once instead of being
// a promise repeated at each call site.
//
// A credential that failed its check is not the case that reaches here, because
// the contract puts an account's suspended/closed verdict AFTER the credential
// matches — authentication before authorisation, and a caller who has proved who
// they are is entitled to learn what state their own account is in. So this
// refusal is 401 specifically, and the 403 the lifecycle produces is not
// something this handler can conflate it with.
func writeSignInRefusal(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	writeError(w, r, signInError{})
}

// signInError is the uniform pre-credential refusal. It is a transport type
// because the uniformity is enforced at the transport: the application reports
// what happened as far as it knows, and this is the shape every one of those
// reports leaves in.
type signInError struct{}

func (signInError) Error() string { return signInFailureMessage }

func (signInError) response() (int, string, string) {
	return stdhttp.StatusUnauthorized, "unauthenticated", signInFailureMessage
}

// handleSession is GET /auth/session: who this session belongs to. The console
// calls it on every load, before it renders anything, to decide between a
// signed-in shell and a sign-in form.
//
// A 200 here is the only proof of authentication the client ever has, and the
// client treats it as one — the browser is not a security boundary, and every
// product operation re-checks the session server-side regardless of what this
// said. So the only thing this handler adds is the shape, and the only thing it
// withholds is account data: an unauthenticated caller must not be able to
// enumerate anything by asking.
func handleSession(useCases sessionUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		Principal, ok := resolveSession(w, r, useCases)
		if !ok {
			return
		}
		writeJSON(w, stdhttp.StatusOK, Principal)
	}
}

// resolveSession is the one place a session is read out of a request and
// resolved to a Principal, so that every operation which needs one does it
// identically and the four failure reasons cannot drift into four answers.
//
// Guard 1 lives here: an absent, malformed, expired or revoked cookie is one
// fact to the caller — there is no usable session — so all four produce the
// same 401 and none of them is distinguishable from the others. A caller
// probing with guessed cookies learns nothing from which of the four applied.
//
// It writes the refusal itself and reports false, so a handler's failure path is
// a two-line early return rather than a repeated error construction that could
// drift.
func resolveSession(w stdhttp.ResponseWriter, r *stdhttp.Request, useCases sessionUseCases) (Principal, bool) {
	token, ok := sessionFromRequest(r)
	if !ok {
		writeError(w, r, unauthenticatedError{})
		return Principal{}, false
	}
	result, err := useCases.Session(r.Context(), token)
	if err != nil {
		// Missing, expired and revoked all arrive as the application's "not
		// found", and all three are translated to the same 401 the absent
		// cookie gets. There is deliberately no inspection of which it was: the
		// answer is the same, so asking would learn nothing and looking would
		// be a temptation to start answering differently.
		writeError(w, r, unauthenticatedError{})
		return Principal{}, false
	}
	return result.Principal, true
}

// handleSignOut is DELETE /auth/session: end this session and clear the
// cookies.
//
// It is idempotent, and that is a correctness property rather than a
// convenience: a sign-out that failed would leave a session alive on a machine
// the user believes they signed out of. So the second call answers 204 whether
// or not there was anything to end, and a caller who cares can read the
// cookie's absence — the clearing Set-Cookie, present on every 204 — as the
// answer.
//
// Note the guard asymmetry. Sign-out requires a valid session to END, but does
// not treat its absence as an error: a caller with no session has already
// achieved the goal. So this handler clears the cookies on every path and asks
// the application to revoke only when there is something to revoke. Its row is
// still guarded, because a cross-origin page must not be able to end somebody's
// session as a denial of service against their console.
//
// The clearing is unconditional, including on the error paths, and that is
// deliberate in both directions. On a 500 the browser should stop carrying a
// token to a surface that is failing; the application has still revoked it
// server-side, so a token captured some other way stops working too. Nothing
// here can produce a state where the browser believes it is still signed in.
func handleSignOut(useCases sessionUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		defer clearSessionCookies(w)

		token, ok := sessionFromRequest(r)
		if ok {
			if err := useCases.SignOut(r.Context(), token); err != nil {
				writeError(w, r, err)
				return
			}
		}
		// 204, no body. A caller who signed out twice, or signed out with
		// nothing to begin with, reads the same 204 and the same cleared
		// cookies.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(stdhttp.StatusNoContent)
	}
}

// handleMintAPIKey is POST /api-keys: mint a key and return its credential
// exactly once.
//
// The account is the session's. There is no account field in the request
// because there is no account to name: ADR 0012 §2 puts the account predicate in
// the query, so the key lands under the caller's own account by construction and
// there is no cross-account key for a caller to aim at. Its creator is the
// session's own user, taken from the same Principal.
//
// The credential reaches the wire through mintedAPIKeyResponse's bespoke
// MarshalJSON, exactly once, and the response is marked no-store by writeJSON so
// a disk cache, a back button or a shared proxy cannot turn a one-time secret
// into a durable one.
//
// All four guards are already in force by the time this runs — the first three
// at the mount, the cookie by resolveSession below — so a handler that arrives
// here has a same-origin request, a JSON body, a double-submit token this server
// issued, and a live session. That is the whole of the mint's trust
// precondition, and the account follows from the session rather than from
// anything in the request.
func handleMintAPIKey(useCases sessionUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		// Guard 1, the one that can still fail on a request that cleared all the
		// others: a caller with no session is told exactly that, rather than
		// something about where the request came from.
		sessionPrincipal, ok := resolveSession(w, r, useCases)
		if !ok {
			return
		}

		var in mintAPIKeyRequest
		if err := decodeJSONBody(w, r, &in); err != nil {
			writeError(w, r, err)
			return
		}

		minted, err := useCases.MintAPIKey(r.Context(), MintAPIKeyInput{
			Principal:   sessionPrincipal,
			DisplayName: in.DisplayName,
		})
		if err != nil {
			writeError(w, r, err)
			return
		}

		// 201, and the one response on this surface that carries a secret. The
		// DTO's MarshalJSON is what puts the token on the wire, once, and
		// application.MintedKey's refusal one layer down is untouched by this
		// copy: the token crossed the boundary as a plain string on a purpose-
		// built DTO, and nothing weakened the refusal to allow it.
		//
		// The conversion rather than a literal, for the same reason as the
		// sign-in above: the result and the DTO are the record and the token
		// under the same two names, and restating them would be a second place
		// to get one wrong.
		writeJSON(w, stdhttp.StatusCreated, renderMintedAPIKey(minted))
	}
}
