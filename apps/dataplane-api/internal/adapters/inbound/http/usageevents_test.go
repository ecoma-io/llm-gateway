package http

import (
	"errors"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// cursor is a value that looks nothing like a number, on purpose. A façade that
// parsed the cursor as an offset, trimmed it or formatted it back would pass a
// test written with "1234" and fail this one.
const cursor = "cur:9f2 &=<not-a-number>/+=="

// TestTheUsageEventFeedRefusesAnUntrustedCaller pins the fail-closed half of
// the authentication boundary: every caller this deployment cannot place is
// refused with the contract's 401, and — the part a status assertion alone
// would miss — the refusal happens before the application, so the Data Plane is
// never consulted on behalf of a caller nobody vouched for.
func TestTheUsageEventFeedRefusesAnUntrustedCaller(t *testing.T) {
	tests := []struct {
		name          string
		configured    string
		authorization []string
	}{
		{
			name:       "no credential at all",
			configured: testCredential,
		},
		{
			name:          "a credential that is not the deployment's",
			configured:    testCredential,
			authorization: []string{"Bearer another-deployment-secret"},
		},
		{
			name:          "a credential presented with no scheme",
			configured:    testCredential,
			authorization: []string{testCredential},
		},
		{
			name:          "a credential presented under another scheme",
			configured:    testCredential,
			authorization: []string{"Basic " + testCredential},
		},
		{
			name:          "an empty bearer credential",
			configured:    testCredential,
			authorization: []string{"Bearer "},
		},
		{
			name:          "a credential carrying whitespace inside itself is refused",
			configured:    testCredential,
			authorization: []string{"Bearer " + "bad secret"},
		},
		{
			// The two length cases are here because the comparison must not
			// decide them by their length alone: a prefix and an extension are
			// both refused, and they are refused by the same fixed-width
			// comparison that refuses a same-length wrong guess.
			name:          "a credential that is a prefix of the deployment's",
			configured:    testCredential,
			authorization: []string{"Bearer " + testCredential[:len(testCredential)-1]},
		},
		{
			name:          "a credential one character longer than the deployment's",
			configured:    testCredential,
			authorization: []string{"Bearer " + testCredential + "x"},
		},
		{
			name:          "a much longer credential",
			configured:    testCredential,
			authorization: []string{"Bearer " + strings.Repeat("s", 4096)},
		},
		{
			name:          "the header given twice",
			configured:    testCredential,
			authorization: []string{"Bearer " + testCredential, "Bearer another-deployment-secret"},
		},
		{
			name:          "a deployment that configured no credential",
			configured:    "",
			authorization: []string{"Bearer "},
		},
		{
			name:          "a deployment that configured no credential, presented with one",
			configured:    "",
			authorization: []string{"Bearer " + testCredential},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := &fakeUsageFacts{}
			handler := New(application.New("test", usage), NewServiceAuthenticator(tt.configured))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(stdhttp.MethodGet, usageEventsPath, nil)
			for _, value := range tt.authorization {
				req.Header.Add("Authorization", value)
			}
			handler.ServeHTTP(rec, req)

			if rec.Code != stdhttp.StatusUnauthorized {
				t.Errorf("GET %s status = %d, want %d", usageEventsPath, rec.Code, stdhttp.StatusUnauthorized)
			}
			if got, want := rec.Body.String(), `{"error":{"code":"unauthenticated","message":"the caller did not identify itself as a service this deployment accepts"},"request_id":"`; !strings.HasPrefix(got, want) {
				t.Errorf("GET %s body = %q, want it to start %q", usageEventsPath, got, want)
			}
			if usage.called() {
				t.Errorf("the refusal still reached the Data Plane: %+v", usage.calls)
			}
			if strings.Contains(rec.Body.String(), testCredential) {
				t.Errorf("the 401 body names the deployment credential: %q", rec.Body.String())
			}
		})
	}
}

// TestTheCursorAndPageSizeTravelUntouched pins this surface's own behaviour
// without a socket: what the caller sent is what the port is asked for, and
// what the port answered is what the caller gets. The cursor is the point of
// the test — this façade has no idea what it means, and the moment it starts
// having one, a value like this stops round-tripping.
func TestTheCursorAndPageSizeTravelUntouched(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantAfter string
		wantLimit int
	}{
		{
			name:      "a cursor the caller supplied crosses verbatim",
			query:     "?after=" + url.QueryEscape(cursor) + "&limit=7",
			wantAfter: cursor,
			wantLimit: 7,
		},
		{
			name:      "an omitted cursor stays omitted rather than becoming empty",
			query:     "?limit=1",
			wantAfter: "",
			wantLimit: 1,
		},
		{
			name:      "the contract's page size is used when the caller names none",
			query:     "?after=" + url.QueryEscape(cursor),
			wantAfter: cursor,
			wantLimit: defaultUsageEventsLimit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := &fakeUsageFacts{page: dataplane.Page{Events: nil, NextCursor: cursor, HasMore: false}}
			handler := testHandler(application.New("test", usage))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(stdhttp.MethodGet, usageEventsPath+tt.query, nil)
			req.Header.Set("Authorization", "Bearer "+testCredential)
			handler.ServeHTTP(rec, req)

			if rec.Code != stdhttp.StatusOK {
				t.Fatalf("GET %s status = %d, want %d (body %q)", usageEventsPath, rec.Code, stdhttp.StatusOK, rec.Body.String())
			}
			if len(usage.calls) != 1 {
				t.Fatalf("the port was called %d times, want exactly 1", len(usage.calls))
			}
			if got := usage.calls[0].after; got != tt.wantAfter {
				t.Errorf("the port was asked for after = %q, want %q", got, tt.wantAfter)
			}
			if got := usage.calls[0].limit; got != tt.wantLimit {
				t.Errorf("the port was asked for limit = %d, want %d", got, tt.wantLimit)
			}

			// The cursor the Data Plane issued comes back as it was issued, and
			// an empty page is an empty array rather than null: the schema says
			// `type: array`, and a strict consumer would reject null.
			want := `{"events":[],"next_cursor":"` + cursor + `","has_more":false}` + "\n"
			if got := rec.Body.String(); got != want {
				t.Errorf("body = %q, want %q", got, want)
			}
		})
	}
}

// TestADataPlaneFailureMapsToItsContractedResponse pins the two failures this
// seam can produce to the statuses api/openapi/dataplane.yaml declares for
// them, and pins the third case as what it is: a failure the port did not
// describe is this process's own, and it is reported as such rather than as the
// Data Plane's problem.
//
// The bodies are checked for the address and the credential as well as for the
// status, because an error path is where a URL most easily reaches a caller: it
// is the value everything about the failure tends to be built around.
func TestADataPlaneFailureMapsToItsContractedResponse(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "an unreplayable position is a 410 the caller can act on",
			err:        fmt.Errorf("%w: the data plane no longer retains that position", dataplane.ErrCursorExpired),
			wantStatus: stdhttp.StatusGone,
			wantCode:   "cursor_expired",
		},
		{
			name:       "an unreadable Data Plane is a 502 rather than this process's 500",
			err:        fmt.Errorf("%w: the management call did not complete", dataplane.ErrUpstreamUnavailable),
			wantStatus: stdhttp.StatusBadGateway,
			wantCode:   "upstream_unavailable",
		},
		{
			name:       "a failure the port does not describe is this application's own",
			err:        errors.New("the adapter returned something the port does not define"),
			wantStatus: stdhttp.StatusInternalServerError,
			wantCode:   "internal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := &fakeUsageFacts{err: tt.err}
			handler := testHandler(application.New("test", usage))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(stdhttp.MethodGet, usageEventsPath, nil)
			req.Header.Set("Authorization", "Bearer "+testCredential)
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("GET %s status = %d, want %d", usageEventsPath, rec.Code, tt.wantStatus)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"code":"`+tt.wantCode+`"`) {
				t.Errorf("body = %q, want code %q", body, tt.wantCode)
			}
			if strings.Contains(body, testCredential) {
				t.Errorf("the error body leaks the deployment credential: %q", body)
			}
			if strings.Contains(body, "://") {
				t.Errorf("the error body carries a URL: %q", body)
			}
		})
	}
}

// TestAMalformedPageSizeIsRefusedBeforeTheFeedIsTouched is the 400 half of the
// contract: a parameter this surface can already see is invalid must not cost
// the Data Plane a call, and the refusal must not echo the value the caller
// sent back at it.
func TestAMalformedPageSizeIsRefusedBeforeTheFeedIsTouched(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "zero", query: "?limit=0"},
		{name: "one above the maximum", query: "?limit=1001"},
		{name: "negative", query: "?limit=-1"},
		{name: "not a number", query: "?limit=abc"},
		{name: "fractional", query: "?limit=1.5"},
		{name: "empty", query: "?limit="},
		{name: "padded with a space", query: "?limit=%205"},
		{name: "given twice with different values", query: "?limit=5&limit=6"},
		{name: "given twice with the same value", query: "?limit=5&limit=5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := &fakeUsageFacts{}
			handler := testHandler(application.New("test", usage))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(stdhttp.MethodGet, usageEventsPath+tt.query, nil)
			req.Header.Set("Authorization", "Bearer "+testCredential)
			handler.ServeHTTP(rec, req)

			if rec.Code != stdhttp.StatusBadRequest {
				t.Errorf("GET %s status = %d, want %d", usageEventsPath+tt.query, rec.Code, stdhttp.StatusBadRequest)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"code":"invalid_request"`) {
				t.Errorf("GET %s body = %q, want code invalid_request", usageEventsPath+tt.query, body)
			}
			if !strings.Contains(body, "limit") {
				t.Errorf("GET %s body = %q, want it to name the parameter", usageEventsPath+tt.query, body)
			}
			if usage.called() {
				t.Errorf("a malformed limit still reached the Data Plane: %+v", usage.calls)
			}
		})
	}
}

// TestAnEmptyCursorIsRefusedRatherThanReadAsTheBeginning pins the one thing
// this surface may know about a cursor without interpreting it: that an empty
// value is not a cursor. Reading `after=` as "from the beginning" would answer
// a question the caller did not ask and hide the broken consumer that sent it.
func TestAnEmptyCursorIsRefusedRatherThanReadAsTheBeginning(t *testing.T) {
	usage := &fakeUsageFacts{}
	handler := testHandler(application.New("test", usage))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(stdhttp.MethodGet, usageEventsPath+"?after=", nil)
	req.Header.Set("Authorization", "Bearer "+testCredential)
	handler.ServeHTTP(rec, req)

	if rec.Code != stdhttp.StatusBadRequest {
		t.Errorf("GET %s?after= status = %d, want %d", usageEventsPath, rec.Code, stdhttp.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), `"code":"invalid_request"`) {
		t.Errorf("GET %s?after= body = %q, want code invalid_request", usageEventsPath, rec.Body.String())
	}
	if usage.called() {
		t.Errorf("an empty cursor still reached the Data Plane: %+v", usage.calls)
	}

	// The same header without the parameter is a different request and is
	// served: absence is how a caller asks for the beginning.
	usage = &fakeUsageFacts{}
	handler = testHandler(application.New("test", usage))
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(stdhttp.MethodGet, usageEventsPath, nil)
	req.Header.Set("Authorization", "Bearer "+testCredential)
	handler.ServeHTTP(rec, req)

	if rec.Code != stdhttp.StatusOK {
		t.Errorf("GET %s status = %d, want %d (body %q)", usageEventsPath, rec.Code, stdhttp.StatusOK, rec.Body.String())
	}
	if len(usage.calls) != 1 || usage.calls[0].after != "" {
		t.Errorf("the port was asked for %+v, want one call with no cursor", usage.calls)
	}
}

// TestAParameterThisOperationDoesNotDeclareIsRefused pins the façade's half of
// the one input where the two hops of this protocol could disagree. The private
// listener refuses an undeclared name; before this check existed the façade
// forwarded one, so `?position=abc` was a request the public surface accepted
// and the process behind it rejected — and the caller would have learnt that as
// a 502, which says the Data Plane is down.
//
// It is a 400 rather than a forward-and-hope for the reason shared/errors.yaml
// gives the code: a caller who named their starting point and got a 200 would
// believe it was honoured, and the whole retained history would arrive as if it
// were the answer to their question.
func TestAParameterThisOperationDoesNotDeclareIsRefused(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "a plausible name for the position", query: "?position=abc"},
		{name: "the same name capitalised differently", query: "?After=abc"},
		{name: "an undeclared name beside a declared one", query: "?limit=5&position=abc"},
		{name: "a name the caller invented", query: "?foo=1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := &fakeUsageFacts{}
			handler := testHandler(application.New("test", usage))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(stdhttp.MethodGet, usageEventsPath+tt.query, nil)
			req.Header.Set("Authorization", "Bearer "+testCredential)
			handler.ServeHTTP(rec, req)

			if rec.Code != stdhttp.StatusBadRequest {
				t.Fatalf("GET %s status = %d, want %d (body %q)", usageEventsPath+tt.query, rec.Code, stdhttp.StatusBadRequest, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"code":"invalid_request"`) {
				t.Errorf("GET %s body = %q, want code invalid_request", usageEventsPath+tt.query, body)
			}
			// The refused name is not echoed, for the reason every refusal on
			// this surface withholds the value: it is a string the caller wrote.
			if strings.Contains(body, "position") || strings.Contains(body, "foo") {
				t.Errorf("body = %q, echoes the name the caller sent", body)
			}
			if usage.called() {
				t.Errorf("an undeclared parameter still reached the Data Plane: %+v", usage.calls)
			}
		})
	}
}

// TestTheCursorLengthBoundIsTheContracts pins the other end of the cursor's
// declared shape: a value exactly as long as the contract allows is a cursor
// like any other and crosses untouched, and one character more is refused
// before the Data Plane is asked anything at all.
//
// The refusal is a 400 rather than the 502 the caller would otherwise see, and
// that is the reason the check is here. A cursor the contract cannot describe
// is a fact about the caller's request, and answering it as "the Data Plane is
// unavailable" would send an operator to look at the wrong process. Nothing
// about the value's content is examined either way: length is not meaning, and
// a bound this surface applies is still not a parser.
func TestTheCursorLengthBoundIsTheContracts(t *testing.T) {
	atLimit := strings.Repeat("c", maxUsageEventsCursorLength)
	overLimit := strings.Repeat("c", maxUsageEventsCursorLength+1)

	// A cursor of the same declared length in *code points* and twice that in
	// bytes. This is the row that fails if the bound is counted with Go's
	// `len()`, and it is the reason the bound is `utf8.RuneCountInString`: the
	// schema's `maxLength` counts characters, so a byte count would refuse a
	// cursor this feed is entitled to issue, at the one hop a consumer cannot
	// route around.
	multiByteAtLimit := strings.Repeat("é", maxUsageEventsCursorLength)
	multiByteOverLimit := strings.Repeat("é", maxUsageEventsCursorLength+1)
	if len(multiByteAtLimit) <= maxUsageEventsCursorLength {
		t.Fatalf("the fixture is %d bytes for %d characters; it does not exercise the bytes-versus-characters difference at all",
			len(multiByteAtLimit), maxUsageEventsCursorLength)
	}

	t.Run("a cursor of the declared maximum length in characters but more in bytes crosses untouched", func(t *testing.T) {
		usage := &fakeUsageFacts{page: dataplane.Page{NextCursor: cursor}}
		handler := testHandler(application.New("test", usage))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(stdhttp.MethodGet, usageEventsPath+"?after="+url.QueryEscape(multiByteAtLimit), nil)
		req.Header.Set("Authorization", "Bearer "+testCredential)
		handler.ServeHTTP(rec, req)

		if rec.Code != stdhttp.StatusOK {
			t.Fatalf("status = %d, want %d for a cursor the contract permits (body %q)", rec.Code, stdhttp.StatusOK, rec.Body.String())
		}
		if len(usage.calls) != 1 {
			t.Fatalf("the port was called %d time(s), want 1", len(usage.calls))
		}
		// The lengths are named rather than the values: a 512-character cursor is
		// not a useful thing to read in a failure message, and the two numbers are
		// the whole assertion — a value that reached the port as fewer characters
		// was truncated, and one that reached it as more was re-encoded on the way.
		if got := usage.calls[0].after; got != multiByteAtLimit {
			t.Errorf("the port was asked for a cursor of %d character(s) and %d byte(s), and what it was sent was a cursor of %d character(s) and %d byte(s); a value that differs at all is a cursor this surface did not receive unchanged, which is the one thing it may not do with one",
				utf8.RuneCountInString(multiByteAtLimit), len(multiByteAtLimit), utf8.RuneCountInString(got), len(got))
		}
	})

	t.Run("a cursor past the declared maximum length in characters is refused", func(t *testing.T) {
		usage := &fakeUsageFacts{}
		handler := testHandler(application.New("test", usage))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(stdhttp.MethodGet, usageEventsPath+"?after="+url.QueryEscape(multiByteOverLimit), nil)
		req.Header.Set("Authorization", "Bearer "+testCredential)
		handler.ServeHTTP(rec, req)

		if rec.Code != stdhttp.StatusBadRequest {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, stdhttp.StatusBadRequest, rec.Body.String())
		}
		if usage.called() {
			t.Errorf("an over-long cursor still reached the Data Plane: %+v", usage.calls)
		}
	})

	t.Run("a cursor of the declared maximum length crosses untouched", func(t *testing.T) {
		usage := &fakeUsageFacts{page: dataplane.Page{NextCursor: cursor}}
		handler := testHandler(application.New("test", usage))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(stdhttp.MethodGet, usageEventsPath+"?after="+url.QueryEscape(atLimit), nil)
		req.Header.Set("Authorization", "Bearer "+testCredential)
		handler.ServeHTTP(rec, req)

		if rec.Code != stdhttp.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, stdhttp.StatusOK, rec.Body.String())
		}
		if len(usage.calls) != 1 || usage.calls[0].after != atLimit {
			t.Errorf("the port was asked for %+v, want the cursor unchanged", usage.calls)
		}
	})

	t.Run("a cursor past the declared maximum length is refused before the feed is touched", func(t *testing.T) {
		usage := &fakeUsageFacts{}
		handler := testHandler(application.New("test", usage))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(stdhttp.MethodGet, usageEventsPath+"?after="+url.QueryEscape(overLimit), nil)
		req.Header.Set("Authorization", "Bearer "+testCredential)
		handler.ServeHTTP(rec, req)

		if rec.Code != stdhttp.StatusBadRequest {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, stdhttp.StatusBadRequest, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"code":"invalid_request"`) {
			t.Errorf("body = %q, want code invalid_request", body)
		}
		if !strings.Contains(body, "after") {
			t.Errorf("body = %q, want it to name the parameter", body)
		}
		if strings.Contains(body, overLimit) {
			t.Errorf("body = %q, echoes the value the caller sent", body)
		}
		if usage.called() {
			t.Errorf("an over-long cursor still reached the Data Plane: %+v", usage.calls)
		}
	})
}

// TestExtraWhitespaceBetweenSchemeAndCredentialIsAccepted pins the RFC 6750
// shape of the header on the façade's side: the scheme and the bearer token
// are separated by optional whitespace, and the token itself carries none. The
// Data Plane's management listener accepts the same shape, so a credential a
// caller can present to one hop can be presented to the other.
func TestExtraWhitespaceBetweenSchemeAndCredentialIsAccepted(t *testing.T) {
	usage := &fakeUsageFacts{}
	handler := testHandler(application.New("test", usage))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(stdhttp.MethodGet, usageEventsPath, nil)
	req.Header.Set("Authorization", "Bearer  "+testCredential)
	handler.ServeHTTP(rec, req)

	if rec.Code != stdhttp.StatusOK {
		t.Errorf("GET %s status = %d, want %d (body %q)", usageEventsPath, rec.Code, stdhttp.StatusOK, rec.Body.String())
	}
	if len(usage.calls) != 1 {
		t.Errorf("the port was called %d times, want exactly 1", len(usage.calls))
	}
}

// TestTheCredentialIsCheckedBeforeTheMethod pins the order of the two checks
// on the one protected path, in both halves, because either half alone is
// satisfied by the wrong implementation.
//
// Without the first, an unauthenticated caller would learn which methods the
// path accepts; without the second, guarding the companion would have turned
// the path into a 401 for every verb and the contract's 405 would be
// unreachable. The order is a property of the path — identify the caller, then
// decide what it asked for — and not of the verb that happened to arrive.
func TestTheCredentialIsCheckedBeforeTheMethod(t *testing.T) {
	tests := []struct {
		name          string
		authorization string
		wantStatus    int
		wantAllow     string
	}{
		{
			name:       "no credential, wrong method: the caller is the question, not the verb",
			wantStatus: stdhttp.StatusUnauthorized,
		},
		{
			name:          "the credential, wrong method: now the verb is the question",
			authorization: "Bearer " + testCredential,
			wantStatus:    stdhttp.StatusMethodNotAllowed,
			wantAllow:     "GET, HEAD",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := &fakeUsageFacts{}
			handler := testHandler(application.New("test", usage))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(stdhttp.MethodPost, usageEventsPath, nil)
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("POST %s status = %d, want %d (body %q)", usageEventsPath, rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Allow"); got != tt.wantAllow {
				t.Errorf("POST %s Allow = %q, want %q", usageEventsPath, got, tt.wantAllow)
			}
			// Neither half may reach the Data Plane: the request was refused
			// before any use case ran, whichever of the two checks refused it.
			if usage.called() {
				t.Errorf("a refused method still reached the Data Plane: %+v", usage.calls)
			}
		})
	}
}

// TestAServedPageIsExactlyWhatTheDataPlaneAnswered guards the shape of a
// non-empty response: every field of the fact reaches the wire, the timestamp
// is RFC 3339 as the contract declares, and a payload this process never
// decoded crosses as the bytes it arrived as.
func TestAServedPageIsExactlyWhatTheDataPlaneAnswered(t *testing.T) {
	occurredAt := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	usage := &fakeUsageFacts{page: dataplane.Page{
		Events: []dataplane.Event{{
			RequestID:     "req_01HZ",
			Kind:          "unbillable_orphaned",
			SchemaVersion: 3,
			OccurredAt:    occurredAt,
			Payload:       []byte(`{"allocation_id":"alloc-1","reason":"a<b"}`),
		}},
		NextCursor: cursor,
		HasMore:    true,
	}}
	handler := testHandler(application.New("test", usage))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(stdhttp.MethodGet, usageEventsPath, nil)
	req.Header.Set("Authorization", "Bearer "+testCredential)
	handler.ServeHTTP(rec, req)

	want := `{"events":[{"request_id":"req_01HZ","kind":"unbillable_orphaned","schema_version":3,"occurred_at":"2026-09-23T10:00:00Z","payload":{"allocation_id":"alloc-1","reason":"a<b"}}],"next_cursor":"` + cursor + `","has_more":true}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}
