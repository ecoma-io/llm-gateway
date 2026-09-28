package http

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"

	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
)

// newTestApp is the application the tests under this file drive. It is a
// function rather than an inline application.New("test") at each call site so
// that a test constructing a server four different ways is four ways to forget
// the version stamp.
func newTestApp() *application.App { return application.New("test") }

// The ten reads' transport tests. readhandlers.go is the layer that decides
// what reaches a caller, and this file is where that is pinned.
//
// Every test here is a DELETION test in disguise. Each one removes something —
// a session, a limit, an account, a row — and asserts that the answer says so,
// because the handler is correct in the presence of the thing and the question
// is what it does without it. A test that asserted only the happy path would
// pass against a handler that read the account from a query parameter, clamped
// an out-of-range limit, computed a total, or rendered a currency — every one of
// which is a rule this surface states in prose and a client depends on.
//
// Four rules run through the file, and each is worth more than any single
// handler's behaviour:
//
//  1. the account is the session's and is never an input,
//  2. a cross-account id is 404 and never 403,
//  3. a page has three fields and no total, and
//  4. money is minor units with no currency and no arithmetic.

// signedInCookie is the cookie a signed-in request carries. It is built from
// the fake's own live session so the value and the Principal the handlers
// resolve agree — a test that wanted them to disagree would set its own.
func signedInCookie() *stdhttp.Cookie {
	live := liveSessionResult()
	return &stdhttp.Cookie{Name: sessionCookieName, Value: live.Token.CookieValue()}
}

// drive issues one request against a handler built over these fakes and returns
// the recorder, so a test states the request and nothing else.
func drive(t *testing.T, reads *fakeConsoleReadUseCases, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(signedInCookie())
	req.Header.Set(RequestIDHeader, "read-request")
	rec := httptest.NewRecorder()
	newFakeSessionWithReadUseCases(reads).ServeHTTP(rec, req)
	return rec
}

// decode reads a body into a generic map, so a test can assert on the KEYS a
// response carries and the values it puts in them without naming a DTO.
func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not a JSON object: %v (body %q)", err, rec.Body.String())
	}
	return body
}

// topLevelKeys is the sorted key set of a JSON object, for asserting a shape
// without asserting an order.
func topLevelKeys(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestEveryReadRequiresALiveSession is rule 1's floor. A read is a product
// surface read, and the ten of them are the ones a console page loads: if any
// of them answered without a session it would be a screen rendering account
// data to a caller nobody authenticated, and the test that matters is the one
// that removes the cookie rather than the one that sends it.
//
// The three whole-plane reads are in the table beside the account-scoped ones
// precisely so that their being session-gated is asserted like everything else —
// "it is only a catalogue" is the reasoning that has historically let a
// read-only endpoint skip authentication.
func TestEveryReadRequiresALiveSession(t *testing.T) {
	reads := newFakeConsoleReadUseCases()

	for _, path := range readPaths() {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(stdhttp.MethodGet, path, nil)
			req.Header.Set(RequestIDHeader, "unauthenticated-read")
			rec := httptest.NewRecorder()
			New(newTestApp(), &answeringPinger{}, newFakeSessionUseCases(), reads, stubUsage()).ServeHTTP(rec, req)

			if rec.Code != stdhttp.StatusUnauthorized {
				t.Errorf("GET %s with no session: status = %d, want 401 (body %q)", path, rec.Code, rec.Body.String())
			}
		})
	}

	// And a session that resolves to nothing is the same answer, not a
	// different one. The fake's field is set after the table above ran so the
	// two properties are independent: a dead session is not a special case of
	// an absent one on any read.
	dead := newFakeSessionUseCases()
	dead.sessionErr = errNoSession
	req := httptest.NewRequest(stdhttp.MethodGet, "/account/overview", nil)
	req.AddCookie(signedInCookie())
	req.Header.Set(RequestIDHeader, "dead-session-read")
	rec := httptest.NewRecorder()
	New(newTestApp(), &answeringPinger{}, dead, reads, stubUsage()).ServeHTTP(rec, req)
	if rec.Code != stdhttp.StatusUnauthorized {
		t.Errorf("GET /account/overview with a dead session: status = %d, want 401", rec.Code)
	}
}

// readPaths is the ten reads as paths, with the one path parameter filled in —
// the list every "every read" assertion above walks, so a read added to the
// route table is covered by these tests without editing them.
func readPaths() []string {
	return []string{
		"/account/overview",
		"/users",
		"/api-keys",
		"/plans",
		"/subscriptions",
		"/entitlements",
		"/funding-buckets",
		"/funding-buckets/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/ledger",
		"/reconciliation/findings",
		"/reconciliation/runs",
	}
}

// TestTheAccountIsTheSessionsAndIsNeverAnInput is rule 1, stated as the
// positive. Each account-scoped read is driven with a query string full of
// things a caller might try to name an account with, and each is asserted to
// have forwarded the SESSION's account and not one of the query's.
//
// The hostile query is the point. `?account_id=`, `?account=`, and a path that
// is a UUID rather than a bucket are the three shapes a caller actually
// reaches for, and a handler that read any of them would answer 200 with
// somebody else's rows while every test that sent no query at all still passed.
func TestTheAccountIsTheSessionsAndIsNeverAnInput(t *testing.T) {
	foreign := "99999999-9999-4999-8999-999999999999"

	tests := []struct {
		name  string
		path  string
		calls func(*fakeConsoleReadUseCases) []string
	}{
		{
			name:  "/account/overview",
			path:  "/account/overview?account_id=" + foreign + "&account=" + foreign,
			calls: func(f *fakeConsoleReadUseCases) []string { return f.overviewCalls },
		},
		{
			name: "/users",
			path: "/users?account_id=" + foreign,
			calls: func(f *fakeConsoleReadUseCases) []string {
				out := []string{}
				for _, c := range f.userCalls {
					out = append(out, c.AccountID)
				}
				return out
			},
		},
		{
			name: "/api-keys",
			path: "/api-keys?account_id=" + foreign,
			calls: func(f *fakeConsoleReadUseCases) []string {
				out := []string{}
				for _, c := range f.keyCalls {
					out = append(out, c.AccountID)
				}
				return out
			},
		},
		{
			name: "/subscriptions",
			path: "/subscriptions?account_id=" + foreign,
			calls: func(f *fakeConsoleReadUseCases) []string {
				out := []string{}
				for _, c := range f.subCalls {
					out = append(out, c.AccountID)
				}
				return out
			},
		},
		{
			name: "/entitlements",
			path: "/entitlements?account_id=" + foreign,
			calls: func(f *fakeConsoleReadUseCases) []string {
				out := []string{}
				for _, c := range f.entCalls {
					out = append(out, c.AccountID)
				}
				return out
			},
		},
		{
			name: "/funding-buckets",
			path: "/funding-buckets?account_id=" + foreign,
			calls: func(f *fakeConsoleReadUseCases) []string {
				out := []string{}
				for _, c := range f.bucketCalls {
					out = append(out, c.AccountID)
				}
				return out
			},
		},
		{
			// The ledger takes the session's account AND a bucket from the path,
			// so it is the read where the two can be confused with each other.
			name: "/funding-buckets/{id}/ledger",
			path: "/funding-buckets/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/ledger?account_id=" + foreign,
			calls: func(f *fakeConsoleReadUseCases) []string {
				out := []string{}
				for _, c := range f.ledgerCalls {
					out = append(out, c.AccountID)
				}
				return out
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reads := newFakeConsoleReadUseCases()
			rec := drive(t, reads, stdhttp.MethodGet, tt.path)

			if rec.Code != stdhttp.StatusOK {
				t.Fatalf("GET %s: status = %d, want 200 (body %q)", tt.path, rec.Code, rec.Body.String())
			}

			forwarded := tt.calls(reads)
			if len(forwarded) != 1 {
				t.Fatalf("GET %s reached the use case %d times, want once", tt.path, len(forwarded))
			}
			if forwarded[0] != reads.sessionAccountID {
				t.Errorf("GET %s forwarded account %q; the account is the session's and no query parameter can replace it",
					tt.path, forwarded[0])
			}
		})
	}
}

// TestAPageCarriesThreeFieldsAndNoTotal is rule 3, asserted on the key set of
// every list.
//
// A total is the field this rule exists to refuse, and it is refused because it
// is the one number a client cannot derive: a console that wanted "12 keys"
// would have to ask for a count the keyset page cannot honestly give, and the
// count would be a second, inconsistent view of the same table. Asserting the
// ABSENCE by key set is stronger than asserting the presence of the three
// wanted keys — a total added beside them would leave a presence assertion
// passing — and asserting the set is stronger still than checking a list of
// forbidden names, because a field nobody thought to forbid still fails.
func TestAPageCarriesThreeFieldsAndNoTotal(t *testing.T) {
	listPaths := []string{
		"/users",
		"/api-keys",
		"/plans",
		"/subscriptions",
		"/entitlements",
		"/funding-buckets",
		"/funding-buckets/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/ledger",
		"/reconciliation/findings",
		"/reconciliation/runs",
	}

	for _, path := range listPaths {
		t.Run(path, func(t *testing.T) {
			body := decode(t, drive(t, newFakeConsoleReadUseCases(), stdhttp.MethodGet, path))

			got := topLevelKeys(body)
			want := []string{"has_more", "items", "next_cursor"}
			if len(got) != len(want) {
				t.Fatalf("GET %s: page keys = %v, want exactly %v — a fourth field is a claim the keyset page cannot keep", path, got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("GET %s: page keys = %v, want exactly %v", path, got, want)
					break
				}
			}

			// items is an ARRAY and never null. The contract types it as one, and
			// a console iterating `items.length` on a null is a crash on the
			// emptiest screen in the product.
			items, ok := body["items"].([]any)
			if !ok {
				t.Fatalf("GET %s: items = %#v, want an array; null is not an array", path, body["items"])
			}
			if len(items) == 0 {
				t.Errorf("GET %s: items is empty; the fake always answers one row, so an empty page means the handler dropped them", path)
			}
		})
	}
}

// TestTheOverviewCarriesNoTotalAndNoArithmetic is the same rule on the one
// operation that is not a page, and it is the read a dashboard is built on — so
// it is the read where a derived figure would be most tempting and most
// consequential.
//
// The counts on it are STORED columns. A handler that summed the subscription
// list, or added two lists together, or derived `available` from `settled` and
// `held`, would pass every "the value is right" test and fail this one, because
// the fixture's numbers are chosen so that any of those derivations gives a
// different answer.
func TestTheOverviewCarriesNoTotalAndNoArithmetic(t *testing.T) {
	reads := newFakeConsoleReadUseCases()
	rec := drive(t, reads, stdhttp.MethodGet, "/account/overview")
	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("GET /account/overview: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	body := decode(t, rec)

	// The seven fields the schema declares. Enumerated as a set so a field
	// added beside them fails here rather than being ignored.
	// Six, and the sixth is `account`: the overview carries no instant of its
	// own. It is a composition over rows that each carry theirs, and an
	// `updated_at` here would be a moment this system cannot source — the
	// nearest honest statement of "when was this assembled" is the arrival time
	// of the response, which is not a fact about the account at all.
	want := []string{
		"account", "active_api_key_count", "open_finding_count",
		"payg_balances", "subscriptions", "user_count",
	}
	got := topLevelKeys(body)
	if len(got) != len(want) {
		t.Fatalf("/account/overview keys = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("/account/overview keys = %v, want exactly %v", got, want)
			break
		}
	}

	// The fixture's user count is 2 and it holds ONE user row. A handler that
	// counted the list would render 1; a handler that summed anything would
	// render a number that matches neither. The stored value is the one that
	// must survive.
	if got := body["user_count"]; got != float64(2) {
		t.Errorf("/account/overview user_count = %#v, want the stored 2; a count derived from a page is a second, disagreeing view of the same table", got)
	}
}

// TestTheTransportForwardsTheLimitVerbatimAndNeverClampsIt is the paging
// rule as the TRANSPORT owns it, which is a narrower claim than the one the
// rule sounds like and is therefore worth stating precisely.
//
// The refusal is application.ResolvePage's: it is the one place the bounds live
// (persistence.MinPageLimit and MaxPageLimit) and the one place an out-of-range
// ask becomes invalid_request, so a test here that asserted the 400 would be
// asserting a layer down a seam and would pass against a handler that clamped
// on the way there. What the transport owns is everything before that: the
// value is parsed, a non-integer is refused HERE because a caller that sent
// `limit=abc` sent a value this operation does not read, and an in-range or
// absent value crosses the seam byte-for-byte without a ceiling applied on the
// way — because a clamp in the transport is invisible to ResolvePage, which
// would then validate a limit the caller never sent.
func TestTheTransportForwardsTheLimitVerbatimAndNeverClampsIt(t *testing.T) {
	t.Run("an in-range limit crosses unchanged", func(t *testing.T) {
		for _, limit := range []int{1, 7, 200} {
			reads := newFakeConsoleReadUseCases()
			rec := drive(t, reads, stdhttp.MethodGet, fmt.Sprintf("/users?limit=%d", limit))
			if rec.Code != stdhttp.StatusOK {
				t.Fatalf("limit=%d: status = %d, want 200", limit, rec.Code)
			}
			if len(reads.userCalls) != 1 {
				t.Fatalf("limit=%d reached the use case %d times, want once", limit, len(reads.userCalls))
			}
			if got := reads.userCalls[0].Page.Limit; got != limit {
				t.Errorf("limit=%d forwarded as %d; the transport parses a page size and does not second-guess it", limit, got)
			}
		}
	})

	t.Run("an absent limit crosses as zero, which is the documented default", func(t *testing.T) {
		for _, query := range []string{"", "?", "?limit="} {
			reads := newFakeConsoleReadUseCases()
			rec := drive(t, reads, stdhttp.MethodGet, "/users"+query)
			if rec.Code != stdhttp.StatusOK {
				t.Fatalf("GET /users%s: status = %d, want 200", query, rec.Code)
			}
			if got := reads.userCalls[0].Page.Limit; got != 0 {
				t.Errorf("GET /users%s forwarded limit %d, want 0: the default is resolved against the persistence layer's own bound, and a value invented here would be a second copy of it",
					query, got)
			}
		}
	})

	t.Run("a limit that is not a whole number is refused without reaching the use case", func(t *testing.T) {
		// Not clamped, not defaulted, not passed through. `limit=abc` is a value
		// this operation does not read, and answering it as though it had said
		// nothing would make a malformed request indistinguishable from one that
		// said nothing at all.
		for _, raw := range []string{"abc", "1.5", "1e3", "0x10", " 5", "5 "} {
			reads := newFakeConsoleReadUseCases()
			rec := drive(t, reads, stdhttp.MethodGet, "/users?limit="+url.QueryEscape(raw))
			if rec.Code != stdhttp.StatusBadRequest {
				t.Errorf("limit=%q: status = %d, want 400 (body %q)", raw, rec.Code, rec.Body.String())
			}
			if len(reads.userCalls) != 0 {
				t.Errorf("limit=%q reached the use case; a value this operation cannot read is refused at the transport", raw)
			}
		}
	})

	t.Run("every list pages on the same two parameters", func(t *testing.T) {
		// All nine list-shaped reads read `after` and `limit` and nothing else.
		// A collection that paged on a differently-named pair would be invisible
		// to a test that only walked one of them, and a console that learned one
		// spelling would break on the other.
		const cursor = "eyJwb3NpdGlvbiI6MSwia2V5IjoiYXV0bWF0YTowMDAwMDAwMC0wMDAwLTAwMDAtMDAwMC0wMDAwLTAwMDAwMDAwMDAwMCJ9"
		for _, path := range []string{
			"/users", "/api-keys", "/plans", "/subscriptions", "/entitlements",
			"/funding-buckets", "/funding-buckets/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/ledger",
			"/reconciliation/findings", "/reconciliation/runs",
		} {
			reads := newFakeConsoleReadUseCases()
			rec := drive(t, reads, stdhttp.MethodGet, path+"?after="+cursor+"&limit=3")
			if rec.Code != stdhttp.StatusOK {
				t.Errorf("GET %s with a cursor: status = %d, want 200 (body %q)", path, rec.Code, rec.Body.String())
				continue
			}
			if got := forwardedPage(reads, path); got != "after="+cursor+" limit=3" {
				t.Errorf("GET %s forwarded %q, want the cursor and the limit passed through on every list alike", path, got)
			}
		}
	})
}

// forwardedPage renders the page ask one of the nine lists actually received, so
// the table above can assert against the same two values without each entry
// growing its own accessor.
func forwardedPage(reads *fakeConsoleReadUseCases, path string) string {
	var ask pageRequest
	switch {
	case path == "/users":
		ask = reads.userCalls[0].Page
	case path == "/api-keys":
		ask = reads.keyCalls[0].Page
	case path == "/plans":
		ask = reads.planCalls[0]
	case path == "/subscriptions":
		ask = reads.subCalls[0].Page
	case path == "/entitlements":
		ask = reads.entCalls[0].Page
	case path == "/funding-buckets":
		ask = reads.bucketCalls[0].Page
	case strings.HasSuffix(path, "/ledger"):
		ask = reads.ledgerCalls[0].Page
	case path == "/reconciliation/findings":
		ask = reads.findingCalls[0].Page
	default:
		ask = reads.runCalls[0]
	}
	return "after=" + ask.After + " limit=" + strconv.Itoa(ask.Limit)
}

// TestAForeignBucketIs404AndNever403 is rule 2, and the ledger is the only
// operation on the surface where a caller can NAME a resource that is not
// theirs — every other read's answer is a predicate over the session's own
// rows. That makes this the one place the rule can be caught, and the one place
// a 403 would be a real disclosure: a caller learns that a bucket they do not
// own EXISTS, which for a UUID drawn from a log or a support ticket is a fact
// about another account's business.
//
// The three cases are deliberately the same request with three different
// buckets: one the account does not own, one that does not exist at all, and
// the one it does own. The assertion is that the first two are byte-identical,
// not merely both 4xx.
func TestAForeignBucketIs404AndNever403(t *testing.T) {
	own := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	foreign := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	absent := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"

	bodies := map[string]string{}
	for _, bucket := range []struct {
		name   string
		id     string
		status int
	}{
		{name: "owned", id: own, status: stdhttp.StatusOK},
		{name: "foreign", id: foreign, status: stdhttp.StatusNotFound},
		{name: "absent", id: absent, status: stdhttp.StatusNotFound},
	} {
		t.Run(bucket.name, func(t *testing.T) {
			rec := drive(t, newFakeConsoleReadUseCases(), stdhttp.MethodGet,
				"/funding-buckets/"+bucket.id+"/ledger")
			if rec.Code != bucket.status {
				t.Fatalf("GET the ledger of a %s bucket: status = %d, want %d (body %q)",
					bucket.name, rec.Code, bucket.status, rec.Body.String())
			}
			bodies[bucket.name] = strings.TrimSpace(rec.Body.String())

			// Only a refusal carries an envelope, so only a refusal is asked for
			// its code — and what is asked is that it is not `forbidden`. The
			// surface has no 403 on this path at all, and a 403 here would
			// confirm the bucket exists, which is the disclosure the rule exists
			// to prevent. Asserting on the CODE rather than the status class
			// makes that specific: some future 403 with a different code would
			// fail the status assertion above instead of passing this one.
			if bucket.name != "owned" && envelopeCode(t, rec) == "forbidden" {
				t.Errorf("a %s bucket answers forbidden; a 403 confirms the bucket exists, which is the disclosure the rule exists to prevent", bucket.name)
			}
		})
	}

	// The two refusals are the SAME refusal. A caller who probes a foreign id
	// and a non-existent id must not be able to tell which they hit — that is
	// the whole content of the rule, and two 404s with different messages would
	// pass every status assertion above.
	if bodies["foreign"] != bodies["absent"] {
		t.Errorf("a foreign bucket answers %q and a non-existent one %q; they must be one indistinguishable answer",
			bodies["foreign"], bodies["absent"])
	}
}

// TestTheLedgerForwardsTheBucketAndTheKind filters unchanged is the cursor's
// other half. A cursor earned under one filter cannot be replayed under
// another, and that is a property of what the CURSOR carries rather than of
// what the handler passes — so the test is that the handler forwards both,
// verbatim and unparsed, for a test to be able to tell at all.
func TestTheLedgerForwardsTheBucketAndTheKindFiltersUnchanged(t *testing.T) {
	reads := newFakeConsoleReadUseCases()
	const cursor = "b3Q6Y3Vyc29yOjEyMw"
	rec := drive(t, reads, stdhttp.MethodGet,
		"/funding-buckets/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/ledger?kind=consume&after="+cursor+"&limit=25")
	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	if len(reads.ledgerCalls) != 1 {
		t.Fatalf("the ledger use case ran %d times, want once", len(reads.ledgerCalls))
	}
	call := reads.ledgerCalls[0]
	if call.BucketID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Errorf("forwarded bucket %q, want the one the path named", call.BucketID)
	}
	if call.Kind != "consume" {
		t.Errorf("forwarded kind %q, want the query's own value", call.Kind)
	}
	if call.Page.After != cursor {
		t.Errorf("forwarded cursor %q, want %q passed through untouched; this transport never decodes a cursor", call.Page.After, cursor)
	}
	if call.Page.Limit != 25 {
		t.Errorf("forwarded limit %d, want 25", call.Page.Limit)
	}
}

// TestMoneyIsMinorUnitsWithNoCurrencyAndNoArithmetic is rule 4, walked over
// every field on this surface that is money.
//
// The refusal has two halves and the test asserts both. No field anywhere in a
// money position is named `currency`, `unit`, or carries a symbol — the system
// has not decided one (issue #63) and a client that formats one has invented it.
// And no money field is anything but the integer the application sent: a
// handler that added settled to held, or computed available as a difference,
// would answer with a plausible number and no way for a caller to know the
// ledger and the balance had stopped agreeing.
func TestMoneyIsMinorUnitsWithNoCurrencyAndNoArithmetic(t *testing.T) {
	// The names a money field must never take, in any position, in any of the
	// bodies below. A currency is a decision this system has not made.
	forbidden := map[string]bool{
		"currency": true, "currency_code": true, "unit": true,
		"symbol": true, "iso_currency": true, "iso4217": true,
		"total": true, "total_minor_units": true, "balance": true,
		"sum": true, "computed": true, "derived": true,
	}

	for _, path := range readPaths() {
		t.Run(path, func(t *testing.T) {
			var body any
			rec := drive(t, newFakeConsoleReadUseCases(), stdhttp.MethodGet, path)
			if rec.Code != stdhttp.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			assertNoMoneyClaims(t, path, body, forbidden)
		})
	}
}

// assertNoMoneyClaims walks a decoded body and refuses two shapes: a forbidden
// key anywhere, and a money object that is anything other than a bare integer
// under `minor_units`.
//
// The walk is over the DECODED value rather than the raw text on purpose. A
// source scan for the word "currency" would have to be written carefully around
// this file's own comments; a walk of what actually reaches a client cannot
// match a comment.
func assertNoMoneyClaims(t *testing.T, path string, node any, forbidden map[string]bool) {
	t.Helper()
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			if forbidden[key] {
				t.Errorf("GET %s: a field named %q reached the wire; a currency is a decision this system has not made, and a total is a view a keyset page cannot keep", path, key)
			}
			// A Money object is exactly one field. Anything beside minor_units
			// is a claim the application did not make.
			if key == "minor_units" {
				if len(typed) != 1 {
					t.Errorf("GET %s: a money object carries %v, want only minor_units", path, topLevelKeys(typed))
				}
				// And the value is an integer, not a formatted number: a JSON
				// string here is a client that will parse and re-format it, and
				// a float here has already lost precision on a balance.
				switch typed[key].(type) {
				case string:
					t.Errorf("GET %s: minor_units is a string; money crosses the wire as an integer or it has been formatted into a currency somewhere", path)
				case float64:
					if n, ok := typed[key].(float64); ok && n != float64(int64(n)) {
						t.Errorf("GET %s: minor_units = %v, a value with a fractional part; money is whole minor units", path, n)
					}
				}
			}
			assertNoMoneyClaims(t, path, value, forbidden)
		}
	case []any:
		for _, element := range typed {
			assertNoMoneyClaims(t, path, element, forbidden)
		}
	}
}

// TestABucketRendersItsThreeStoredBalances is the positive half of rule 4: the
// three numbers the server maintains are rendered, and the fixture's values are
// the fixture's values.
//
// The fixture is chosen so the three cannot be confused with one another — 100
// settled, 10 held, 90 available — and so that a handler computing any of them
// from another gets a different answer. A bucket is a projection the server
// maintains, and the legs are the authority; a browser that re-derives it has
// nothing to reconcile a disagreement against.
func TestABucketRendersItsThreeStoredBalances(t *testing.T) {
	body := decode(t, drive(t, newFakeConsoleReadUseCases(), stdhttp.MethodGet, "/funding-buckets"))

	items, ok := body["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("/funding-buckets: no items in %q", rec0(body))
	}
	bucket, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("/funding-buckets: the first item is not an object: %#v", items[0])
	}

	// The three sit under `balances` as the contract's schema declares them, and
	// this test reads the NESTED shape on purpose: a test that read a flat
	// `settled` would fail against the real wire shape and pass against a
	// flattened one, so it would be pinning the wrong thing in the wrong place.
	cached, ok := bucket["balances"].(map[string]any)
	if !ok {
		t.Fatalf("the bucket rendered no balances object: %#v", bucket)
	}
	for field, want := range map[string]float64{"settled": 100, "held": 10, "available": 90} {
		balance, present := cached[field]
		if !present {
			t.Errorf("the bucket rendered no %q; all three balances are stored columns the contract declares", field)
			continue
		}
		units, ok := balance.(map[string]any)
		if !ok {
			t.Errorf("the bucket's %q = %#v, want a Money object of minor_units", field, balance)
			continue
		}
		raw, present := units["minor_units"]
		if !present {
			t.Errorf("the bucket's %q carries no minor_units: %#v", field, units)
			continue
		}
		got, ok := raw.(float64)
		if !ok {
			t.Errorf("the bucket's %q minor_units = %#v, want a number", field, raw)
			continue
		}
		if got != want {
			t.Errorf("the bucket's %q = %v, want the stored %v; a figure derived here is a browser's opinion of the server's own projection", field, got, want)
		}
	}
}

// TestAnAbsentInstantIsNullAndAnOmitemptyOneIsAbsent is the renderer's two
// spellings of absence, and it is the one that is easy to get wrong in the
// direction that looks harmless: emitting "" for a nullable instant makes a
// client parse a timestamp and fail.
//
// The subscription fixture carries a scheduled cancellation and an open period,
// and the reconciliation run fixture carries no finished_at. Between them the
// two spellings both appear in one file, so a conversion that collapsed them
// would show up here as one of the two shapes.
func TestAnAbsentInstantIsNullAndAnOmitemptyOneIsAbsent(t *testing.T) {
	t.Run("a nullable instant is null, not an empty string", func(t *testing.T) {
		reads := newFakeConsoleReadUseCases()
		reads.subscriptions = &SubscriptionPageResult{
			Items: []SubscriptionRecord{{
				ID: "sub-1", AccountID: reads.sessionAccountID, PlanVersionID: "ver-1",
				State: "active", StartsAt: fixtureTime, CreatedAt: fixtureTime, UpdatedAt: fixtureTime,
				// CancelAt nil: the contract types it [string, "null"].
			}},
		}
		body := decode(t, drive(t, reads, stdhttp.MethodGet, "/subscriptions"))
		item := firstItem(t, body)

		cancel, present := item["cancel_at"]
		if !present {
			t.Fatalf("cancel_at is absent; the contract types it [string, \"null\"], so it must be rendered as null (body %q)", body)
		}
		if cancel != nil {
			t.Errorf("cancel_at = %#v, want null; an empty string is a timestamp a client will try to parse", cancel)
		}
	})

	t.Run("an omitempty instant is dropped entirely", func(t *testing.T) {
		reads := newFakeConsoleReadUseCases()
		// A freshly minted key has no updated_at and no revoked_at. The
		// conversion this exercises is the one that panicked on the empty
		// string before it was fixed, so it is worth pinning on the wire too.
		rec := drive(t, reads, stdhttp.MethodGet, "/api-keys")
		item := firstItem(t, decode(t, rec))

		if _, present := item["revoked_at"]; present {
			t.Errorf("an unrevoked key rendered revoked_at = %#v; the field is omitempty and an absent instant must drop itself", item["revoked_at"])
		}
		if _, present := item["updated_at"]; !present {
			t.Errorf("the fixture key has an updated_at of %q, which the wire dropped", fixtureTime)
		}
	})
}

// TestAnEvidenceFieldReachesTheWireByteForByte is the one test here that is
// about fidelity rather than refusal, and it earns its place because the
// alternative — re-encoding the evidence — is the obvious implementation and is
// wrong for a reason a caller would never see: a finding's evidence is a
// snapshot of figures computed at detection time, and a re-encoded object with
// the same keys in a different order is still a different artifact to anyone
// diffing two of them.
//
// The evidence is checked in THREE ORDERINGS, because key order is the only
// thing a re-encoding can lose and a single assertion cannot see it: a decoder
// that unmarshalled the evidence into a map would reproduce the first ordering
// on all three. The pairs are chosen so that every one of them differs from the
// next — the first and third are the same keys in opposite orders, which is
// exactly the pair a map round-trip would collapse.
func TestAnEvidenceFieldReachesTheWireByteForByte(t *testing.T) {
	for _, evidence := range []string{
		`{"cached_available":90,"from_legs":90}`,
		`{"from_legs":90,"cached_available":90}`,
		`{"b":1,"a":2,"c":3}`,
	} {
		t.Run(evidence, func(t *testing.T) {
			reads := newFakeConsoleReadUseCases()
			reads.findings = &FindingPageResult{
				Items: []FindingRecord{{
					ID: 7, CheckKind: "f1", SubjectKind: "funding_bucket", SubjectID: "bucket-1",
					Severity: "warning", Status: "open", Observed: evidence,
					DetectedAt: fixtureTime, LastSeenAt: fixtureTime,
				}},
				NextCursor: "cursor-findings",
			}

			rec := drive(t, reads, stdhttp.MethodGet, "/reconciliation/findings")
			if body := rec.Body.String(); !strings.Contains(body, evidence) {
				t.Errorf("the evidence did not reach the wire byte-for-byte.\n  want substring: %s\n  body: %s", evidence, body)
			}
		})
	}

	// An empty evidence is an ABSENT field, not `{}` and not null. A check that
	// compared two singleton values wrote no figures, and a client reading `{}`
	// would conclude it compared two empty objects — a different claim about
	// what the check did.
	t.Run("an empty evidence drops itself", func(t *testing.T) {
		reads := newFakeConsoleReadUseCases()
		reads.findings = &FindingPageResult{
			Items: []FindingRecord{{
				ID: 7, CheckKind: "singleton", SubjectKind: "control_plane", SubjectID: "",
				Severity: "warning", Status: "open", DetectedAt: fixtureTime, LastSeenAt: fixtureTime,
			}},
		}
		item := firstItem(t, decode(t, drive(t, reads, stdhttp.MethodGet, "/reconciliation/findings")))
		if _, present := item["observed"]; present {
			t.Errorf("a finding with no evidence rendered observed = %#v; the field is omitempty", item["observed"])
		}
	})

	// And evidence that is not JSON is refused where the stack names the
	// conversion, rather than reaching a client as a body no JSON parser can
	// read. It is recovered rather than spawned so the assertion is about the
	// refusal and not about a process.
	t.Run("evidence that is not JSON is refused at the boundary", func(t *testing.T) {
		for _, malformed := range []string{"not json at all", `{"unterminated": `, `{'single':'quoted'}`, "{"} {
			func() {
				defer func() {
					recovered := recover()
					if recovered == nil {
						t.Errorf("evidence %q reached the wire; a column that is not JSON is a corrupt row, not a response", malformed)
						return
					}
					if !strings.Contains(toString(recovered), malformed) {
						t.Errorf("the panic does not name the value %q: %v", malformed, recovered)
					}
				}()
				reads := newFakeConsoleReadUseCases()
				reads.findings = &FindingPageResult{
					Items: []FindingRecord{{
						ID: 7, CheckKind: "f1", SubjectKind: "funding_bucket", SubjectID: "bucket-1",
						Severity: "warning", Status: "open", Observed: malformed,
						DetectedAt: fixtureTime, LastSeenAt: fixtureTime,
					}},
				}
				drive(t, reads, stdhttp.MethodGet, "/reconciliation/findings")
			}()
		}
	})
}

// TestEveryReadIsUncacheable is the last rule, and it is not about the reads at
// all — it is about every response on the surface, and it is here because
// writeJSON is the shared writer and a read is the cheapest way to exercise it.
//
// A cached account screen is a data disclosure with a retention policy nobody
// chose, and the responses that MUST NOT be kept are the ones carrying an
// account's rows. The header is set at the one place all bodies are written, so
// a future handler that writes its own body would be the defect; the probes are
// exempt and are asserted separately.
func TestEveryReadIsUncacheable(t *testing.T) {
	for _, path := range readPaths() {
		t.Run(path, func(t *testing.T) {
			rec := drive(t, newFakeConsoleReadUseCases(), stdhttp.MethodGet, path)
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("GET %s: Cache-Control = %q, want no-store; a cached account screen is a disclosure with a retention policy nobody chose", path, got)
			}
		})
	}
}

// TestAFailedReadIsAnEnvelopeAndNotAStack is the failure shape every read
// shares.
//
// A handler that let a raw error reach a caller would leak a table name, a
// column name or a DSN fragment; the classification in errorResponse is the
// only place a status is decided, and this asserts the outcome from above it —
// that a refusal is the contract's envelope, carries the request id a caller
// can quote to an operator, and says nothing the application did not intend.
func TestAFailedReadIsAnEnvelopeAndNotAStack(t *testing.T) {
	reads := newFakeConsoleReadUseCases()
	reads.err = errNoReads

	rec := drive(t, reads, stdhttp.MethodGet, "/account/overview")
	if rec.Code != stdhttp.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// `internal` and not a spelling of this package's own: the code is one the
	// contract's enum declares, and a console branching on it reads the
	// contract, not the string a Go constant happens to hold.
	if got := envelopeCode(t, rec); got != "internal" {
		t.Errorf("code = %q, want the contract's `internal`", got)
	}

	body := rec.Body.String()
	if strings.Contains(body, "answered nothing") {
		t.Errorf("the use case's own error text reached the wire: %q. The cause belongs in a log line, never in a response body", body)
	}
	if got := rec.Header().Get(RequestIDHeader); got != "read-request" {
		t.Errorf("response request id = %q, want the one the request carried; a caller needs it to quote the failure to an operator", got)
	}
}

// firstItem pulls the first row out of a decoded page, failing the test if the
// page is not the shape the caller expected.
func firstItem(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("no items in %s", rec0(body))
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("the first item is not an object: %#v", items[0])
	}
	return item
}

// rec0 renders a decoded body for a failure message, tolerating anything.
func rec0(body any) string {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Sprintf("%#v", body)
	}
	return string(encoded)
}
