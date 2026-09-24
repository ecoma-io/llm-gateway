package management

import (
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/usagefacts"
)

func TestOnlyTheConfiguredCredentialAuthenticates(t *testing.T) {
	tests := []struct {
		name   string
		header []string
	}{
		{name: "no Authorization header at all"},
		{name: "an empty Authorization header", header: []string{""}},
		{name: "a different credential", header: []string{credentialScheme + " " + otherCredential}},
		{name: "the right credential under the wrong scheme", header: []string{"Basic " + serviceCredential}},
		{name: "the credential with no scheme", header: []string{serviceCredential}},
		{name: "the credential presented twice", header: []string{credentialScheme + " " + serviceCredential, credentialScheme + " " + otherCredential}},
		{name: "the scheme alone", header: []string{credentialScheme + " "}},
		// The three length cases are here because the comparison must not decide
		// them by their length alone. A prefix, an extension and a same-length
		// wrong guess are all refused, and they are refused by the same
		// fixed-width comparison: both secrets are digested before they are
		// compared, so `subtle.ConstantTimeCompare` never sees two different
		// lengths and never returns early on one. The façade's hop has its own
		// copy of these rows, because the property is the boundary's and not one
		// hop's.
		{name: "a credential that is a prefix of the deployment's", header: []string{credentialScheme + " " + serviceCredential[:len(serviceCredential)-1]}},
		{name: "a credential one character longer than the deployment's", header: []string{credentialScheme + " " + serviceCredential + "x"}},
		{name: "a same-length variation in the last byte", header: []string{credentialScheme + " " + serviceCredential[:len(serviceCredential)-1] + "z"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, &stubFacts{}, requestWithAuthorization(t, tt.header))

			if rec.Code != stdhttp.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, stdhttp.StatusUnauthorized)
			}
			if got := envelopeCode(t, rec); got != codeUnauthenticated {
				t.Errorf("error code = %q, want %q", got, codeUnauthenticated)
			}
		})
	}
}

func TestTheCorrectCredentialIsAcceptedRegardlessOfSchemeCase(t *testing.T) {
	// RFC 6750 makes the scheme case-insensitive, so a caller that wrote
	// `bearer` is presenting the same credential as one that wrote `Bearer`.
	// Refusing it would be this surface inventing a rule the standard does not
	// have, and the credential itself is still compared byte for byte.
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		t.Run(scheme, func(t *testing.T) {
			rec := serve(t, &stubFacts{page: usagePage()}, requestWithAuthorization(t, []string{scheme + " " + serviceCredential}))

			if rec.Code != stdhttp.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, stdhttp.StatusOK)
			}
		})
	}
}

func TestAnUnconfiguredCredentialAuthenticatesNobody(t *testing.T) {
	// The fail-closed half of the boundary, and the reason it is tested here
	// rather than trusted to the loader: a listener constructed without a
	// credential — by a test, by a composition root not yet written — must
	// refuse every caller, including one presenting nothing and one presenting
	// an empty secret. The alternative reading, "no credential configured means
	// no credential required", is one word away and would open an
	// administrative surface to anything that can reach the port.
	handler := New(newTestApp(t), "")

	for _, header := range [][]string{nil, {""}, {credentialScheme + " "}} {
		req := httptest.NewRequest(stdhttp.MethodGet, "/internal/usage-events", nil)
		for _, value := range header {
			req.Header.Add(authorizationHeader, value)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != stdhttp.StatusUnauthorized {
			t.Errorf("with Authorization %v: status = %d, want %d", header, rec.Code, stdhttp.StatusUnauthorized)
		}
	}
}

func TestTheCredentialIsCheckedBeforeAnythingElseHappens(t *testing.T) {
	// An unauthenticated caller must not reach use-case code — not the source,
	// and not even the argument parsing that runs in front of it. A guard
	// written inside the handler is one copy-paste away from a handler that
	// forgets it, and this is the assertion that would notice.
	facts := &stubFacts{}
	rec := serve(t, facts, httptest.NewRequest(stdhttp.MethodGet, "/internal/usage-events?limit=not-a-number", nil))

	if rec.Code != stdhttp.StatusUnauthorized {
		t.Fatalf("status = %d, want %d — the credential check must run before the query is read", rec.Code, stdhttp.StatusUnauthorized)
	}
	if facts.calls != 0 {
		t.Errorf("the port was read %d times by an unauthenticated request", facts.calls)
	}
}

func TestAnUnmatchedOrMisroutedPathStillAnswersWithTheEnvelope(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		target     string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "a path this surface does not serve",
			method:     stdhttp.MethodGet,
			target:     "/api/console/accounts",
			wantStatus: stdhttp.StatusNotFound,
			wantCode:   codeNotFound,
		},
		{
			name:       "a management path with a method it does not accept",
			method:     stdhttp.MethodDelete,
			target:     "/internal/usage-events",
			wantStatus: stdhttp.StatusMethodNotAllowed,
			wantCode:   codeMethodNotAllowed,
		},
		{
			name:       "a path that is not canonical",
			method:     stdhttp.MethodGet,
			target:     "/internal//usage-events",
			wantStatus: stdhttp.StatusNotFound,
			wantCode:   codeNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := authed(t, tt.target)
			request.Method = tt.method
			rec := serve(t, &stubFacts{}, request)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := envelopeCode(t, rec); got != tt.wantCode {
				t.Errorf("error code = %q, want %q", got, tt.wantCode)
			}
			// Every response carries the identifier, including the ones produced
			// before routing and after an authentication failure.
			if got := rec.Header().Get(RequestIDHeader); got == "" {
				t.Errorf("%s is missing on a %d response", RequestIDHeader, tt.wantStatus)
			}
		})
	}
}

func TestAMisroutedMethodIsRefusedOnlyAfterTheCallerIsIdentified(t *testing.T) {
	// An unauthenticated request using the wrong method on an internal path is
	// answered 401, not 405. The other order would let a caller that holds no
	// credential learn which methods this surface accepts, and it would make
	// the guard's placement depend on the method the caller happened to choose.
	request := httptest.NewRequest(stdhttp.MethodDelete, "/internal/usage-events", nil)
	rec := serve(t, &stubFacts{}, request)

	if rec.Code != stdhttp.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, stdhttp.StatusUnauthorized)
	}
}

func requestWithAuthorization(t *testing.T, header []string) *stdhttp.Request {
	t.Helper()
	req := httptest.NewRequest(stdhttp.MethodGet, "/internal/usage-events", nil)
	for _, value := range header {
		req.Header.Add(authorizationHeader, value)
	}
	return req
}

// usagePage is a minimal valid page, for the tests whose subject is the
// credential rather than the body.
func usagePage() usagefacts.Page {
	return usagefacts.Page{NextCursor: "position-1"}
}
