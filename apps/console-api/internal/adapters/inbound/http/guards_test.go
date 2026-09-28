package http

import (
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
)

// The four guards, each tested on its own.
//
// Every test here is written as a PAIR: a request that fails because of one
// guard, and a request identical except for the presence of that one guard's
// evidence, which must succeed. A test that only showed the refusal would pass
// with a guard that refused EVERYTHING — a door welded shut is a door nobody
// gets through, and it is not a working door. The pair is what makes each
// guard's refusal mean "this guard" rather than "this service", and it is what
// shows the four are independent rather than four spellings of one check.
//
// The evidence a well-formed request carries, so each pair differs in exactly
// one thing:
//
//	guard 1 (cookie):        a valid session cookie
//	guard 2 (origin):        Sec-Fetch-Site: same-origin
//	guard 3 (content type):  Content-Type: application/json
//	guard 4 (double submit): the X-Console-Csrf header echoing its cookie
//
// The zero value of requestOptions is "every guard present", so a test turns
// exactly one off and the diff between the refusal and the success is that one
// piece of evidence.

// requestOptions are the per-guard toggles. A zero value means every guard's
// evidence is PRESENT; a test turns one off to make that the only difference
// between the refusal and the success.
type requestOptions struct {
	noSession bool
	noOrigin  bool
	crossSite bool
	// originValue sets Sec-Fetch-Site to something other than "same-origin",
	// for the "same-site" case: a sibling subdomain is same-SITE and not
	// same-ORIGIN, and the two words are the difference this guard turns on.
	originValue string
	// noFetchSite removes Sec-Fetch-Site while leaving Origin alone, which is
	// the only way to exercise the guard's second signal — the one a
	// non-browser client supplies, since a browser always sets the first.
	noFetchSite   bool
	originHeader  string
	noContentType bool
	formEncoded   bool
	noTokenHeader bool
	wrongToken    bool
	noTokenCookie bool
}

func (o requestOptions) apply(r *stdhttp.Request) *stdhttp.Request {
	if !o.noSession {
		r.AddCookie(&stdhttp.Cookie{Name: sessionCookieName, Value: string(mustToken())})
	}
	// The double-submit cookie is the server's half of guard 4; the header is
	// the page's echo of it. Both must be present, which is why they have
	// separate toggles — a header with no cookie is a value the caller chose.
	token := newRequestToken()
	if !o.noTokenCookie {
		r.AddCookie(&stdhttp.Cookie{Name: requestTokenCookieName, Value: token})
	}
	switch {
	case o.noOrigin:
		// Neither signal. The fail-closed case: silence is not same-origin.
		r.Header.Del("Sec-Fetch-Site")
		r.Header.Del("Origin")
	case o.noFetchSite:
		// The browser's own signal absent, the non-browser one present. This is
		// the only shape in which the guard consults Origin at all.
		r.Header.Del("Sec-Fetch-Site")
		if o.originHeader != "" {
			r.Header.Set("Origin", o.originHeader)
		} else {
			r.Header.Set("Origin", requestOrigin(r))
		}
	case o.originHeader != "":
		r.Header.Del("Sec-Fetch-Site")
		r.Header.Set("Origin", o.originHeader)
	case o.originValue != "":
		r.Header.Set("Sec-Fetch-Site", o.originValue)
		r.Header.Del("Origin")
	case o.crossSite:
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Del("Origin")
	default:
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	switch {
	case o.noContentType:
		r.Header.Del("Content-Type")
	case o.formEncoded:
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	default:
		r.Header.Set("Content-Type", "application/json")
	}
	switch {
	case o.noTokenHeader:
		r.Header.Del(requestTokenHeader)
	case o.wrongToken:
		r.Header.Set(requestTokenHeader, "a-token-the-attacker-chose")
	default:
		r.Header.Set(requestTokenHeader, token)
	}
	return r
}

// mintBody is a well-formed mint payload: one display name and nothing else.
// There is deliberately no account field in it, and that absence IS the
// assertion — there is no account in a request to name (ADR 0012 §2).
const mintBody = `{"display_name":"ci"}`

// signInBody is a well-formed sign-in payload. The account id is the
// discriminator the email resolves within, not a scope the caller names.
const signInBody = `{"account_id":"11111111-1111-4111-8111-111111111111","email":"operator@example.com","password":"correct horse battery staple"}`

// formBody is what a simple form post actually sends: a urlencoded body, a
// urlencoded content type, and no custom header a preflight would be needed for.
const formBody = "display_name=ci"

// call drives one request through a fresh server whose every session resolves.
// Nothing in the guard tests is about the use cases — they are about the
// transport refusing — so the fake is the permissive one.
func call(method, path, body string, options requestOptions) *httptest.ResponseRecorder {
	return callOn(New(application.New("test"), &answeringPinger{}, newFakeSessionUseCases(), newFakeConsoleReadUseCases()), method, path, body, options)
}

// callOn is call against an already-built handler, for a test that needs its own
// fake behind it.
func callOn(handler stdhttp.Handler, method, path, body string, options requestOptions) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = options.apply(req)
	req.Header.Set(RequestIDHeader, "guard-request")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// unsafeOperations is every unsafe operation on this surface. The guard tests
// run against all three, because a guard that only one endpoint carries is a
// guard the other two do not have.
var unsafeOperations = []struct {
	name   string
	method string
	path   string
	body   string
}{
	{name: "sign in", method: stdhttp.MethodPost, path: "/auth/sign-in", body: signInBody},
	{name: "sign out", method: stdhttp.MethodDelete, path: "/auth/session"},
	{name: "mint an api key", method: stdhttp.MethodPost, path: "/api-keys", body: mintBody},
}

// TestEachGuardRefusesOnItsOwn is the pair test. For every unsafe operation and
// every way a guard can be unsatisfied, it asserts the refusal AND that the same
// request with that one guard's evidence restored is admitted.
//
// The second half is the half a welded door fails. The first half alone would be
// satisfied by a service that refuses all writes, which is a service nobody can
// use and a test that would still be green.
func TestEachGuardRefusesOnItsOwn(t *testing.T) {
	guards := []struct {
		guard     string
		options   requestOptions
		wantState int
		wantCode  string
		// body overrides the operation's payload, for the content-type cases:
		// a form-encoded request sends a form body, and pairing a form content
		// type with a JSON body would test two things at once.
		body string
		// needsBody marks the content-type cases, which have no body to be
		// wrong about on DELETE /auth/session.
		needsBody bool
		// mintOnly marks the cookie case, which is a refusal only on the one
		// operation a session is required for.
		mintOnly bool
	}{
		{
			// Guard 1, the cookie — on the one operation that needs one. A
			// request that cleared all three cross-cutting guards and still has
			// no session is told exactly that, rather than something about where
			// it came from.
			//
			// It is a refusal here and not on the other two unsafe operations
			// because that is what those operations are FOR: sign-in is how a
			// session comes into existence, and sign-out is idempotent, so a
			// caller with no session has already achieved what sign-out would
			// have done. Refusing either would make the surface unable to
			// produce a session and unable to end one.
			guard:     "the session cookie",
			options:   requestOptions{noSession: true},
			wantState: stdhttp.StatusUnauthorized,
			wantCode:  "unauthenticated",
			mintOnly:  true,
		},
		{
			// Guard 2, the origin, ABSENT. A request carrying neither
			// Sec-Fetch-Site nor Origin is refused rather than admitted on the
			// assumption that its silence means innocence. This is the
			// fail-closed case, and it is the one a permissive reading of
			// "check the origin when present" would get wrong.
			guard:     "any same-origin signal",
			options:   requestOptions{noOrigin: true},
			wantState: stdhttp.StatusForbidden,
			wantCode:  "invalid_request",
		},
		{
			// Guard 2, present and affirmatively cross-site. The two ways this
			// guard refuses render identically, so a prober cannot tell an
			// absent signal from a hostile one.
			guard:     "a same-origin signal, cross-site instead",
			options:   requestOptions{crossSite: true},
			wantState: stdhttp.StatusForbidden,
			wantCode:  "invalid_request",
		},
		{
			// Guard 2, "same-site" rather than "same-origin". A sibling
			// subdomain is a same-site request that this origin did not serve
			// and cannot vouch for; admitting it would make the double-submit
			// token the only thing standing between a compromised sibling and a
			// money-moving write here.
			guard:     "a same-origin signal, same-site instead",
			options:   requestOptions{originValue: "same-site"},
			wantState: stdhttp.StatusForbidden,
			wantCode:  "invalid_request",
		},
		{
			// Guard 2, Sec-Fetch-Site absent and Origin present but WRONG. The
			// guard's second signal is the one a non-browser client supplies, so
			// it is the only thing standing between a script and this surface
			// when the browser's own signal is not there — and it refuses the
			// same way the first signal's refusals do.
			guard:     "a same-origin Origin when Sec-Fetch-Site is absent",
			options:   requestOptions{noFetchSite: true, originHeader: "https://evil.example.net"},
			wantState: stdhttp.StatusForbidden,
			wantCode:  "invalid_request",
		},
		{
			// Guard 3, the content type, absent on a request that has a body.
			guard:     "an application/json content type",
			options:   requestOptions{noContentType: true},
			body:      formBody,
			wantState: stdhttp.StatusUnsupportedMediaType,
			wantCode:  "invalid_request",
			needsBody: true,
		},
		{
			// Guard 3, present and affirmatively a form. This is the encoding a
			// simple cross-origin form post sends, and the one no preflight is
			// needed for.
			guard:     "an application/json content type, form-encoded instead",
			options:   requestOptions{formEncoded: true},
			body:      formBody,
			wantState: stdhttp.StatusUnsupportedMediaType,
			wantCode:  "invalid_request",
			needsBody: true,
		},
		{
			// Guard 4, the echoed header, absent. A cross-origin page cannot
			// read the token cookie, so it cannot echo it; this is what its
			// requests look like.
			guard:     "the double-submit header",
			options:   requestOptions{noTokenHeader: true},
			wantState: stdhttp.StatusForbidden,
			wantCode:  "invalid_request",
		},
		{
			// Guard 4, present and wrong. A cross-origin page can CHOOSE a
			// header value; it cannot learn the right one, so the mismatch is
			// the refusal that carries the weight.
			guard:     "the double-submit header, wrong",
			options:   requestOptions{wrongToken: true},
			wantState: stdhttp.StatusForbidden,
			wantCode:  "invalid_request",
		},
		{
			// Guard 4's other half, absent. A header with no token cookie is a
			// value the caller chose for itself, which is not what a double
			// submit means, so it is refused just as firmly.
			guard:     "the double-submit cookie",
			options:   requestOptions{noTokenCookie: true},
			wantState: stdhttp.StatusForbidden,
			wantCode:  "invalid_request",
		},
	}

	for _, operation := range unsafeOperations {
		for _, guard := range guards {
			t.Run(operation.name+": without "+guard.guard, func(t *testing.T) {
				// Sign-out carries no body, so the content-type guard has
				// nothing to police there. That is the contract's 415 being
				// unreachable for a case that does not exist, not a gap: a
				// body-less DELETE has no representation to be wrong about.
				if guard.needsBody && operation.body == "" {
					t.Skip("a body-less request has no content type to police")
				}
				// The cookie guard refuses only where a session is required.
				if guard.mintOnly && operation.path != "/api-keys" {
					t.Skip(operation.name + " does not require a session to do its job")
				}

				body := operation.body
				if guard.body != "" {
					body = guard.body
				}

				// Half one: the refusal, and it is this guard's.
				refused := call(operation.method, operation.path, body, guard.options)
				if refused.Code != guard.wantState {
					t.Fatalf("without %s: status = %d, want %d (body %q)", guard.guard, refused.Code, guard.wantState, refused.Body.String())
				}
				if code := envelopeCode(t, refused); code != guard.wantCode {
					t.Errorf("without %s: code = %q, want %q", guard.guard, code, guard.wantCode)
				}

				// Half two: with that one guard's evidence restored, the request
				// is admitted. This is what distinguishes a working guard from
				// a closed door — the other three were present and unchallenged
				// in the refusal above, so they are demonstrably not the reason.
				admitted := call(operation.method, operation.path, operation.body, requestOptions{})
				if admitted.Code == guard.wantState && envelopeCode(t, admitted) == guard.wantCode {
					t.Errorf("with every guard present: still refused with %d %s; the refusal is not specific to the evidence it names",
						admitted.Code, guard.wantCode)
				}
			})
		}
	}
}

// TestTheGuardsRenderIdentically is the disclosure half of the guards being
// independent.
//
// The origin guard and the double-submit guard are separate mechanisms, and
// they are tested separately, but a client must not be able to tell which one
// stopped it. The difference would be a byte-for-byte oracle for "was the
// origin actually wrong, or only the token" — a fact about this service's guard
// configuration that a prober could map one probe at a time. So every guard
// refusal renders as the same status, code and message, and the message names
// none of them.
func TestTheGuardsRenderIdentically(t *testing.T) {
	refusals := []struct {
		name    string
		options requestOptions
	}{
		{name: "a cross-site origin", options: requestOptions{crossSite: true}},
		{name: "an absent origin signal", options: requestOptions{noOrigin: true}},
		{name: "a same-site origin", options: requestOptions{originValue: "same-site"}},
		{name: "a missing double-submit header", options: requestOptions{noTokenHeader: true}},
		{name: "a wrong double-submit header", options: requestOptions{wrongToken: true}},
		{name: "a missing double-submit cookie", options: requestOptions{noTokenCookie: true}},
	}

	// Every refusal is collected, and the assertion is made once on the SET of
	// answers rather than pairwise. Pairwise comparison reads a match as a
	// failure — the mistake this shape exists to avoid, since a match is
	// exactly the property wanted — and it reports N-1 errors for one real
	// defect. One assertion over one distinct answer says what is true: there
	// is a single answer, and every guard produced it.
	answers := map[string]string{}
	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			rec := call(stdhttp.MethodPost, "/api-keys", mintBody, refusal.options)
			// The request id is the one field that legitimately differs between
			// two answers to two different requests, so the comparison is over
			// the code and the message rather than the raw bytes.
			answers[refusal.name] = envelopeCode(t, rec) + "|" + envelopeMessage(t, rec)
		})
	}
	distinct := map[string][]string{}
	for name, answer := range answers {
		distinct[answer] = append(distinct[answer], name)
	}
	if len(distinct) != 1 {
		t.Errorf("the guards produced %d distinct answers, want exactly 1: %v; a client that can tell which guard refused can map the guard configuration one probe at a time",
			len(distinct), distinct)
	}
	// And the message must not name the guard that fired, which is the leak a
	// shared status and code alone would still allow.
	for answer := range distinct {
		for _, mechanism := range []string{"origin", "token", "cookie", "fetch", "csrf"} {
			if strings.Contains(strings.ToLower(answer), mechanism) {
				t.Errorf("the shared refusal %q names %q; naming the mechanism that refused is the disclosure the shared answer exists to prevent", answer, mechanism)
			}
		}
	}
}

// TestASessionIsUnauthenticatedWhateverTheReason covers the four reasons a
// session can be unusable — absent, malformed, expired, revoked — and the one
// answer they share.
//
// The first two are the transport's to refuse and it can tell them apart
// internally; the last two are the application's, and the transport is not told
// which. All four leave with the same status, the same code and the same
// message, because a difference is a disclosure to anyone willing to send
// cookies, and the client cannot act differently on any of them anyway.
func TestASessionIsUnauthenticatedWhateverTheReason(t *testing.T) {
	live := liveSessionResult()

	tests := []struct {
		name       string
		cookie     *stdhttp.Cookie
		sessionErr error
	}{
		{name: "absent"},
		{
			name:   "malformed — not a token this server would have minted",
			cookie: &stdhttp.Cookie{Name: sessionCookieName, Value: "not-a-session-token"},
		},
		{
			name:       "expired",
			cookie:     &stdhttp.Cookie{Name: sessionCookieName, Value: string(live.Token)},
			sessionErr: errNoSession,
		},
		{
			// Same application error, deliberately: expired and revoked are two
			// facts the transport must not be able to separate, and a fake with
			// one field for both is what makes that impossible to fake.
			name:       "revoked",
			cookie:     &stdhttp.Cookie{Name: sessionCookieName, Value: string(live.Token)},
			sessionErr: errNoSession,
		},
	}

	var reference string
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useCases := newFakeSessionUseCases()
			useCases.sessionErr = tt.sessionErr

			req := httptest.NewRequest(stdhttp.MethodGet, "/auth/session", nil)
			if tt.cookie != nil {
				req.AddCookie(tt.cookie)
			}
			req.Header.Set(RequestIDHeader, "unauthenticated-request")
			rec := httptest.NewRecorder()
			New(application.New("test"), &answeringPinger{}, useCases, newFakeConsoleReadUseCases()).ServeHTTP(rec, req)

			if rec.Code != stdhttp.StatusUnauthorized {
				t.Fatalf("a %s session: status = %d, want 401 (body %q)", tt.name, rec.Code, rec.Body.String())
			}
			got := envelopeCode(t, rec) + "|" + envelopeMessage(t, rec)
			if reference == "" {
				reference = got
				return
			}
			if got != reference {
				t.Errorf("a %s session answers %q; every unusable session answers %q, because a difference is a disclosure to anyone sending cookies",
					tt.name, got, reference)
			}
		})
	}
}

// TestTheMintCarriesItsCredentialExactlyOnce is ADR 0012 §3's one-time
// rendering, asserted on the bytes.
//
// Exactly one occurrence is the requirement. Zero would be a mint that returned
// no credential at all; two would be a secret rendered into a shape something
// else might repeat, and the whole design is that the response is the only place
// it appears and it appears once.
func TestTheMintCarriesItsCredentialExactlyOnce(t *testing.T) {
	useCases := newFakeSessionUseCases()
	rec := callOn(New(application.New("test"), &answeringPinger{}, useCases, newFakeConsoleReadUseCases()),
		stdhttp.MethodPost, "/api-keys", mintBody, requestOptions{})

	if rec.Code != stdhttp.StatusCreated {
		t.Fatalf("a well-formed mint: status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	secret := useCases.mintResult.Token
	if got := strings.Count(rec.Body.String(), secret); got != 1 {
		t.Errorf("the one-time token appears %d times in the mint response, want exactly 1: body %q", got, rec.Body.String())
	}
	// Under the contract's field name, so a client reading the documented shape
	// finds it rather than guessing where the secret went.
	if !strings.Contains(rec.Body.String(), `"token":"`+secret+`"`) {
		t.Errorf("the mint response did not carry the token under the contract's field: %q", rec.Body.String())
	}
}

// TestAMintedKeyStillRefusesSerialisation is the refusal that must SURVIVE the
// boundary this change crosses.
//
// ADR 0012 §3 says application.MintedKey keeps refusing, and that a handler
// needing a token on the wire copies the string at the boundary rather than
// weakening the refusal. The mint test above proves the copy works; this proves
// the refusal is still standing, so the copy did not quietly become a licence.
func TestAMintedKeyStillRefusesSerialisation(t *testing.T) {
	// A direct assertion against the type's own method, not a comment, because
	// the failure this guards against is a future edit that deletes the method
	// to make some other path compile.
	if _, err := (application.MintedKey{}).MarshalJSON(); err == nil {
		t.Error("application.MintedKey no longer refuses serialisation; the one-time token's guard has been removed")
	}
	// And a session token, the other credential on this surface, refuses for
	// the same reason and must keep doing so: a session's identity travels in
	// a cookie, and a credential in a wire format is a credential in a log.
	if _, err := SessionToken("x").MarshalJSON(); err == nil {
		t.Error("a SessionToken no longer refuses serialisation; it belongs in a Set-Cookie and nowhere else")
	}
}

// TestTheProductResponseIsUncacheable is ADR 0012 §3's second rule, asserted on
// a product response rather than on the helper that sets it.
//
// The reason is a minted credential: a response a browser or a shared proxy
// could keep turns "shown once" into a durable copy — in a disk cache, in the
// back-forward cache, in a corporate proxy's store — and there is no way to
// revoke a copy nobody told the server about.
func TestTheProductResponseIsUncacheable(t *testing.T) {
	handler := New(application.New("test"), &answeringPinger{}, newFakeSessionUseCases(), newFakeConsoleReadUseCases())

	// A successful product response, and a refused one: an error body carries a
	// request id, and a cached error is a way to replay a request a caller
	// should have to make again.
	for _, tt := range []struct {
		name    string
		options requestOptions
	}{
		{name: "a successful product response", options: requestOptions{}},
		{name: "a refused product response", options: requestOptions{noSession: true}},
		{name: "a cross-origin product response", options: requestOptions{crossSite: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := callOn(handler, stdhttp.MethodPost, "/api-keys", mintBody, tt.options)
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want %q", got, "no-store")
			}
		})
	}

	// The session read is a product response too: it returns a Principal, which
	// is account data, and a cached Principal is another account's header on a
	// shared machine.
	read := httptest.NewRequest(stdhttp.MethodGet, "/auth/session", nil)
	read.AddCookie(&stdhttp.Cookie{Name: sessionCookieName, Value: string(mustToken())})
	read.Header.Set(RequestIDHeader, "guard-request")
	readRec := httptest.NewRecorder()
	handler.ServeHTTP(readRec, read)
	if got := readRec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("GET /auth/session: Cache-Control = %q, want %q", got, "no-store")
	}
}

// TestTheProbesAreTheDocumentedExemption is the deliberate divergence from the
// no-store rule, asserted so that it STAYS deliberate.
//
// The contract enumerates the three probes as exempt because they carry no
// account data. Sending no-store on them would be a defensible
// over-approximation, but it would contradict the contract's headers, and a
// header that contradicts the document is a defect no client-side test can tell
// from a bug. The exemption is pinned to exactly those paths here, so a fourth
// path cannot inherit it by accident.
func TestTheProbesAreTheDocumentedExemption(t *testing.T) {
	handler := New(application.New("test"), &answeringPinger{}, newFakeSessionUseCases(), newFakeConsoleReadUseCases())
	for _, path := range []string{"/healthz", "/readyz", "/version"} {
		req := httptest.NewRequest(stdhttp.MethodGet, path, nil)
		req.Header.Set(RequestIDHeader, "probe-request")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if got := rec.Header().Get("Cache-Control"); got == "no-store" {
			t.Errorf("%s: Cache-Control = %q; the contract enumerates the probes as the exemption, and the header would now contradict it", path, got)
		}
	}
}

// TestSignInRefusalsAreByteIdentical is the uniform-failure rule, asserted on
// the exact bytes rather than on the status alone.
//
// ADR 0008 §2 and the contract's 401 description both require it: no such
// account, no such user, a removed user, a wrong credential and an `invited`
// row are five causes with ONE answer, because a difference between them — in
// status, code, message, or even wall time — is a disclosure to anyone willing
// to submit guesses. The handler is the last place the answer is written, so it
// is the last place the uniformity can be lost, and this is what holds it there.
//
// Each case below is a DIFFERENT application error, with a different message, so
// a pass-through in the handler would visibly break the test rather than happen
// to coincide.
func TestSignInRefusalsAreByteIdentical(t *testing.T) {
	causes := []error{
		errNoSuchAccount,
		errNoSuchUser,
		errRemovedUser,
		errWrongCredential,
		errInvitedRow,
	}

	var reference string
	for _, cause := range causes {
		t.Run(cause.Error(), func(t *testing.T) {
			useCases := newFakeSessionUseCases()
			useCases.signInErr = cause
			rec := callOn(New(application.New("test"), &answeringPinger{}, useCases, newFakeConsoleReadUseCases()),
				stdhttp.MethodPost, "/auth/sign-in", signInBody, requestOptions{})

			if rec.Code != stdhttp.StatusUnauthorized {
				t.Fatalf("%v: status = %d, want 401 (body %q)", cause, rec.Code, rec.Body.String())
			}
			// The request id is the one field that legitimately differs between
			// two refusals, so it is normalised out and everything else must be
			// byte-identical — including the absence of the cause's own message.
			body := strings.ReplaceAll(rec.Body.String(), "guard-request", "<id>")
			if reference == "" {
				reference = body
				return
			}
			if body != reference {
				t.Errorf("%v renders %q; the first refusal rendered %q. A difference between pre-credential causes is a disclosure to anyone submitting guesses",
					cause, body, reference)
			}
		})
	}
}

// The five pre-credential causes, as the application would name them.
//
// They are declared here rather than in the application package because the
// application agent owns that vocabulary, and what this test needs is five
// DISTINCT errors — distinctness is the property under test, so a shared error
// type would make the test vacuous. Distinct messages matter too: they are what
// a pass-through would leak.
var (
	errNoSuchAccount   = application.NotFound("no such account")
	errNoSuchUser      = application.NotFound("no such user")
	errRemovedUser     = application.NotFound("the user was removed")
	errWrongCredential = application.NotFound("the credential did not match")
	errInvitedRow      = application.NotFound("the row is invited and has not been activated")
)

// TestACrossAccountResourceIs404Not403 is the disclosure oracle ADR 0012 §2
// refuses, stated as a test because nothing else on this surface would catch a
// 403 where a 404 belongs.
//
// A 403 confirms to a caller that a guessed id is REAL. So the account predicate
// belongs inside the query — the persistence layer's WHERE clause — and the
// transport's contribution is that an application not-found reaches the client
// as 404, never as 403.
//
// The scope this change owns has no account-scoped path to read (there is no
// /accounts/{id}/… and TestNoRouteCarriesAnAccountID holds that), so the test
// drives the transport's half directly: every application not-found — which is
// what a cross-account resource produces, because the query's account predicate
// matched no row, and what a resource that does not exist produces, at the same
// cost — must render as 404 with the not_found code. A 403 anywhere in that
// path would confirm a guessed id, and the invalid_request code would make a
// permission answer indistinguishable from a CSRF refusal.
func TestACrossAccountResourceIs404Not403(t *testing.T) {
	// The application's not-found, verbatim — the same error a resource that
	// does not exist produces, and the one a cross-account resource produces
	// because the account predicate is inside the query.
	crossAccount := application.NotFound("resource not found")

	for _, tt := range []struct {
		name    string
		options requestOptions
	}{
		// Both cases put a valid session and a fully-guarded request in front
		// of the application, so the ONLY thing the handler can render is the
		// application's answer. A 403 here could not have come from a guard,
		// which is what makes the assertion about the mapping rather than about
		// the request.
		{name: "behind a fully-guarded same-origin request", options: requestOptions{}},
		{name: "from a non-browser client with a matching Origin", options: requestOptions{noFetchSite: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useCases := newFakeSessionUseCases()
			useCases.mintErr = crossAccount

			rec := callOn(New(application.New("test"), &answeringPinger{}, useCases, newFakeConsoleReadUseCases()),
				stdhttp.MethodPost, "/api-keys", mintBody, tt.options)

			if rec.Code == stdhttp.StatusForbidden {
				t.Errorf("a cross-account resource answered 403: a 403 confirms a guessed id is real to someone who may not have it (body %q)", rec.Body.String())
			}
			if rec.Code != stdhttp.StatusNotFound {
				t.Errorf("a cross-account resource answered %d, want 404 (body %q)", rec.Code, rec.Body.String())
			}
			if got := envelopeCode(t, rec); got != "not_found" {
				t.Errorf("a cross-account resource rendered code %q, want %q", got, "not_found")
			}
		})
	}

	// And the mapping itself, asserted directly: the transport renders the
	// application's not-found as 404 with the contract's code, whatever
	// message the application attached to it.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(stdhttp.MethodGet, "/version", nil)
	req = req.WithContext(withRequestID(req.Context(), "not-found-request"))
	writeError(rec, req, crossAccount)

	if rec.Code != stdhttp.StatusNotFound {
		t.Errorf("an application not-found: status = %d, want 404", rec.Code)
	}
	if got := envelopeCode(t, rec); got != "not_found" {
		t.Errorf("an application not-found: code = %q, want %q", got, "not_found")
	}
}

// envelopeCode reads the wire code out of an error envelope, and fails the test
// if the body is not one. Every assertion in this file that says "the code is
// X" is really saying "the body is a well-formed envelope carrying X", and a
// helper that silently returned "" for a malformed body would let all of them
// pass vacuously.
func envelopeCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	body := rec.Body.String()
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("the response body is not the contract's error envelope: %v (body %q)", err, body)
	}
	if envelope.Error.Code == "" {
		t.Fatalf("the error envelope carried no code: %q", body)
	}
	if envelope.RequestID == "" {
		t.Fatalf("the error envelope carried no request id, which the contract requires: %q", body)
	}
	return envelope.Error.Code
}

// envelopeMessage reads the wire message out of an error envelope.
func envelopeMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("the response body is not the contract's error envelope: %v", err)
	}
	return envelope.Error.Message
}
