package http

import (
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
)

// The session cookie itself, and the DTO that renders the one-time secret.
// These are the two things a browser is handed by this surface, so they are
// asserted on the bytes rather than through a helper.

// TestTheSessionCookieCarriesEveryContractedAttribute is the contract's
// SessionCookie header, asserted attribute by attribute.
//
// Each attribute is one mechanism, and removing any of them breaks a specific
// thing:
//   - HttpOnly   — the browser's script never sees the session, so an XSS bug
//     in any dependency exfiltrates nothing.
//   - Secure     — the cookie is not sent over plaintext HTTP at all, which is
//     why a development deployment behind a plain-HTTP proxy cannot sign in.
//     That is the intended failure, not a configuration to work around.
//   - SameSite=Strict — not sent cross-site. A browser control, and not a
//     boundary, which is why three further guards stand behind it.
//   - Path=/     — sent for the whole surface.
//
// The __Host- prefix is asserted alongside them because it is what makes Secure
// and Path=/ enforceable rather than merely requested: a browser refuses a
// cookie so named that lacks either, and refuses one carrying a Domain. So a
// deployment that somehow served this over HTTP would fail at the browser
// instead of quietly downgrading the cookie.
func TestTheSessionCookieCarriesEveryContractedAttribute(t *testing.T) {
	useCases := newFakeSessionUseCases()
	rec := callOn(New(application.New("test"), &answeringPinger{}, useCases, newFakeConsoleReadUseCases()),
		stdhttp.MethodPost, "/auth/sign-in", signInBody, requestOptions{})

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("a valid sign-in: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	cookie := findCookie(t, rec, sessionCookieName)
	if cookie == nil {
		t.Fatalf("the sign-in response set no %s cookie; a 200 is the only proof a session exists", sessionCookieName)
	}
	if !cookie.HttpOnly {
		t.Error("the session cookie is not HttpOnly; a script in the console's own origin would be able to read the session, and an XSS bug in any dependency would exfiltrate it")
	}
	if !cookie.Secure {
		t.Error("the session cookie is not Secure; it would be sent over plaintext HTTP, and the __Host- prefix would make the browser refuse it outright")
	}
	if cookie.SameSite != stdhttp.SameSiteStrictMode {
		t.Errorf("the session cookie: SameSite = %v, want %v", cookie.SameSite, stdhttp.SameSiteStrictMode)
	}
	if cookie.Path != "/" {
		t.Errorf("the session cookie: Path = %q, want %q", cookie.Path, "/")
	}
	if cookie.Value == "" {
		t.Error("the session cookie carried no value")
	}
	// The plaintext in the cookie is the token the application minted, and it
	// is the ONLY place that plaintext appears on a sign-in response.
	if cookie.Value != string(useCases.signInResult.Token) {
		t.Errorf("the session cookie carried %q, want the token the application minted", cookie.Value)
	}
	if got := strings.Count(rec.Body.String(), string(useCases.signInResult.Token)); got != 0 {
		t.Errorf("the session token appears %d times in the sign-in BODY; a JSON body cannot set a cookie, so the credential belongs in the header and nowhere else", got)
	}
}

// TestTheDoubleSubmitCookieIsReadableAndTheSessionIsNot is the one place the
// two cookies deliberately differ, and the difference is the mechanism.
//
// The session cookie is HttpOnly so script cannot read it; the double-submit
// cookie is NOT HttpOnly, because reading it and echoing it in a header IS the
// double submit. A page that can read the double-submit cookie is a page this
// origin served; a cross-origin page that can cause the request cannot read a
// cookie scoped to this host, so it cannot learn the value it would have to
// echo.
func TestTheDoubleSubmitCookieIsReadableAndTheSessionIsNot(t *testing.T) {
	rec := callOn(New(application.New("test"), &answeringPinger{}, newFakeSessionUseCases(), newFakeConsoleReadUseCases()),
		stdhttp.MethodPost, "/auth/sign-in", signInBody, requestOptions{})

	token := findCookie(t, rec, requestTokenCookieName)
	if token == nil {
		t.Fatalf("the sign-in response set no %s cookie; the double-submit guard has nothing to compare against", requestTokenCookieName)
	}
	if token.HttpOnly {
		t.Error("the double-submit cookie is HttpOnly; a page that cannot read it cannot echo it, and the guard would be refusing every real request")
	}
	if !token.Secure {
		t.Error("the double-submit cookie is not Secure; a __Host- cookie without it is refused by the browser")
	}
	if token.Path != "/" {
		t.Errorf("the double-submit cookie: Path = %q, want %q", token.Path, "/")
	}
	if token.Value == "" {
		t.Error("the double-submit cookie carried no value")
	}
}

// TestSignOutClearsBothCookiesWhateverItFound is the sign-out idempotence
// requirement, asserted on the response: a second sign-out, or a sign-out with
// nothing to end, must still clear both cookies.
//
// A sign-out that did not clear them would leave a token in a browser on a
// machine the user believes they signed out of — the exact failure the
// operation exists to prevent, arriving through the operation itself.
func TestSignOutClearsBothCookiesWhateverItFound(t *testing.T) {
	for _, tt := range []struct {
		name string
		// withSession says whether the request presents a session at all, which
		// is the difference between ending one and confirming there is none.
		withSession bool
	}{
		{name: "with a live session", withSession: true},
		{name: "with no session at all", withSession: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := callOn(New(application.New("test"), &answeringPinger{}, newFakeSessionUseCases(), newFakeConsoleReadUseCases()),
				stdhttp.MethodDelete, "/auth/session", "", requestOptions{noSession: !tt.withSession})

			if rec.Code != stdhttp.StatusNoContent {
				t.Fatalf("a sign-out: status = %d, want 204 (body %q)", rec.Code, rec.Body.String())
			}
			for _, name := range []string{sessionCookieName, requestTokenCookieName} {
				cookie := findCookie(t, rec, name)
				if cookie == nil {
					t.Errorf("a sign-out cleared no %s cookie; a caller who signed out twice must read the cookie's absence as the answer", name)
					continue
				}
				if cookie.Value != "" || cookie.MaxAge >= 0 {
					t.Errorf("the %s clearing cookie: Value = %q, MaxAge = %d; it must be empty and expired, or the browser keeps the original", name, cookie.Value, cookie.MaxAge)
				}
			}
		})
	}
}

// TestTheMintedKeyDTORendersTheSecretExactlyOnce is the bespoke MarshalJSON,
// asserted against a value shaped to break a struct-tag implementation.
//
// A hand-written marshaller is a place a later field can be forgotten and a
// place an added struct field can be dropped, so this asserts three things the
// contract's MintedAPIKey schema promises: the record's fields are all there,
// the token is there under its documented name, and marshalling the same value
// twice produces the same bytes — the determinism a struct tag would give for
// free and a hand-rolled assembly has to keep.
func TestTheMintedKeyDTORendersTheSecretExactlyOnce(t *testing.T) {
	dto := mintedAPIKeyResponse(liveMintedKey())

	encoded, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshalling the mint DTO: %v", err)
	}

	var rendered map[string]any
	if err := json.Unmarshal(encoded, &rendered); err != nil {
		t.Fatalf("the mint DTO did not render a JSON object: %v (%s)", err, encoded)
	}
	for _, field := range []string{"id", "account_id", "display_name", "prefix", "state", "created_at", "token"} {
		if _, present := rendered[field]; !present {
			t.Errorf("the mint DTO omitted %q, which the contract's MintedAPIKey schema requires: %s", field, encoded)
		}
	}
	if rendered["token"] != liveMintedKey().Token {
		t.Errorf("the mint DTO rendered token = %v, want the plaintext", rendered["token"])
	}
	// The optional record fields are absent, not null: the contract types them
	// as [string, "null"] and the console's read model treats null and "" as
	// different facts about a key.
	for _, field := range []string{"created_by", "updated_at", "revoked_at"} {
		if _, present := rendered[field]; present {
			t.Errorf("the mint DTO rendered %q, which this record does not have; absent and null are different facts about a key: %s", field, encoded)
		}
	}

	again, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshalling the mint DTO a second time: %v", err)
	}
	if string(again) != string(encoded) {
		t.Errorf("the same DTO rendered differently twice: %s then %s", encoded, again)
	}
}

// TestTheMintDTOEscapesItsStrings is the hand-rolled assembly's sharpest edge.
//
// The record's fields are operator-chosen strings — a display name a human
// typed — so a quote, a backslash or a control character in one of them must not
// be able to break out of its string literal and corrupt the response. This
// asserts the value round-trips rather than the byte sequence, because the byte
// sequence is encoding/json's business and reimplementing it here would be the
// defect.
func TestTheMintDTOEscapesItsStrings(t *testing.T) {
	minted := liveMintedKey()
	minted.Record.DisplayName = `ci "quoted" \ back` + "\n" + "second line"
	dto := mintedAPIKeyResponse(minted)

	encoded, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshalling a DTO whose display name contains quotes and a newline: %v", err)
	}

	var rendered struct {
		DisplayName string `json:"display_name"`
		Token       string `json:"token"`
	}
	if err := json.Unmarshal(encoded, &rendered); err != nil {
		t.Fatalf("the escaped record did not parse back: %v (%s)", err, encoded)
	}
	if rendered.DisplayName != minted.Record.DisplayName {
		t.Errorf("display_name round-tripped as %q, want %q", rendered.DisplayName, minted.Record.DisplayName)
	}
	if rendered.Token != minted.Token {
		t.Errorf("token round-tripped as %q; a quote in a display name must not have disturbed the field after it", rendered.Token)
	}
}

// findCookie returns the named cookie from a response, or nil. It fails the
// test rather than returning nil silently when the header is malformed, because
// a malformed Set-Cookie is exactly the defect the assertions downstream are
// looking for and a nil here would report it as "absent" instead.
func findCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *stdhttp.Cookie {
	t.Helper()
	for _, header := range rec.Header().Values("Set-Cookie") {
		cookie, err := readSetCookie(header)
		if err != nil {
			t.Fatalf("the response set a malformed Set-Cookie %q: %v", header, err)
		}
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

// readSetCookie parses one Set-Cookie header value.
func readSetCookie(header string) (*stdhttp.Cookie, error) {
	return (&stdhttp.Response{Header: stdhttp.Header{"Set-Cookie": []string{header}}}).Cookies()[0], nil
}
