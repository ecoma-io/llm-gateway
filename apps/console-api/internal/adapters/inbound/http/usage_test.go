package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
)

// The tests below are the ones a caller meets: a real request, over the real
// handler, on the real envelope, with only the usage SEAM standing in. That is
// the same line the ten reads' tests draw, and it is drawn in the same place
// for the same reason: everything the transport owns is the production one, and
// the composition below the seam — the domain's walk, the store's statements,
// the use case's attribution and the credential's resolution — is covered where
// it lives rather than through a second implementation of it here.
//
// The corpus is fixed — three hour-buckets over a three-hour range — rather than
// generated, because a generated expectation re-implements the marshaller it
// is checking: `encoding/json` would happily produce the same field names the
// marshaller does, the same struct tags in the same order, and a golden
// comparison would be a tautology. The long strings below are what make this
// file able to go red at all, and they are what a generated one would not be.

const (
	// usageRequestID is fixed so every body below is byte-for-byte comparable:
	// the error envelope carries it, and a generated one would make every
	// refusal case a comparison against a random string.
	usageRequestID = "usage-request"

	// usagePath is the served path, and it is the operation's declared path
	// in console.yaml rather than one spelled here: the document's server base
	// is `/`, so the path a caller calls is the path the contract declares.
	// A test that spelled it wrongly would otherwise agree with itself while
	// the surface and the contract drifted apart.
	usagePath = "/usage"

	// usageQuery is a well-formed request: a three-hour range at the hour grain
	// in UTC, which is the smallest question the surface answers. A bare date is
	// the malformed one, and it is a case below rather than this one.
	usageQuery = "from=2026-09-01T00:00:00Z&to=2026-09-01T03:00:00Z&granularity=hour"
)

// The instants the corpus is built from, named once so a case reads as a
// statement about a range rather than as a wall of digits.
var (
	usageFrom        = time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	usageTo          = time.Date(2026, time.September, 1, 3, 0, 0, 0, time.UTC)
	usageDataThrough = time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
)

// usageServer mounts the product surface over the usage seam a case controls.
// The seam is the ONLY substitution: the readiness probe, the session surface
// and the ten reads are the package's own fakes, and the handler, the envelope
// and every mapping between them are the production ones.
func usageServer(useCases UsageUseCases) stdhttp.Handler {
	return New(application.New("v0.1.0"), &answeringPinger{}, newFakeSessionUseCases(), newFakeConsoleReadUseCases(), useCases)
}

// emptyUsageAnswer is the answer a read produces for an account this plane holds
// no derivation for: the range that was asked for, no buckets, and every figure
// zero. The availability is `not_available` because that is the contract's own
// word for it, and the zone is UTC because an absent timezone parameter names
// the store's own zone.
//
// It is the shape the mount tests and the refusal tables need — an answer that
// is valid, unremarkable and never the subject of the case — so that a case
// about a credential or a query string is not also a case about figures.
func emptyUsageAnswer() UsageAnswer {
	return UsageAnswer{
		Availability: "not_available",
		Granularity:  "hour",
		From:         usageFrom,
		To:           usageTo,
		Timezone:     "UTC",
		Freshness:    UsageAnswerFreshness{DataThrough: usageDataThrough, Basis: "fact_feed_pass"},
	}
}

// issueUsage is one request against a mounted surface, returning the recorded
// response. The credential is a header rather than a parameter of this helper
// because most of these cases are about what happens when it is absent or wrong
// — a helper that always supplied one would make the refusal cases read as if
// they exercised something they do not.
func issueUsage(t *testing.T, handler stdhttp.Handler, query, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(stdhttp.MethodGet, usagePath+"?"+query, nil)
	req.Header.Set(RequestIDHeader, usageRequestID)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// errorEnvelopeOf decodes the shared error envelope off a refusal, so a case can
// assert on the CODE the transport chose while leaving the message — which the
// use case writes to name the bound it broke — to a substring where one matters.
func errorEnvelopeOf(t *testing.T, rec *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	envelope := errorEnvelope{}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("the refusal body %q is not the contract's error envelope: %v", rec.Body.String(), err)
	}
	return envelope
}

// TestTheUsageSurfaceRefusesEveryCredentialItCannotResolve is the half of the
// credential story that is a TRANSPORT question, because the credential is
// extracted here and nothing else about it is.
//
// Every refusal answers 401 with the contract's `unauthenticated` code and the
// same sentence, and that is the assertion that matters: a caller with a token
// it cannot use learns nothing from the difference between "there was no
// credential" and "that credential is not one of ours". A handler that reached
// for a different code, or a different sentence, for the two would have built an
// oracle over the credential space out of a convenience. The one case that is
// not a refusal is a credential the surface ACCEPTS, and it is here because a
// table of refusals alone would pass over a handler that refused everything.
func TestTheUsageSurfaceRefusesEveryCredentialItCannotResolve(t *testing.T) {
	// The seam answers; a request refused before it would be a different
	// defect, asserted separately below.
	useCases := &fakeUsageUseCases{answer: emptyUsageAnswer()}
	handler := usageServer(useCases)

	tests := []struct {
		name          string
		query         string
		authorization string
		// wantStatus is 401 unless a case is about a credential the surface
		// ACCEPTS, which is the only kind of case here that is not a refusal.
		wantStatus int
	}{
		{
			name:  "no Authorization header at all",
			query: usageQuery,
		},
		{
			name:          "a header this surface cannot read as a bearer credential",
			query:         usageQuery,
			authorization: "console-test-token",
		},
		{
			name:          "a bearer credential with nothing after the scheme",
			query:         usageQuery,
			authorization: "Bearer",
		},
		{
			name:          "a bearer credential with only whitespace after the scheme",
			query:         usageQuery,
			authorization: "Bearer    ",
		},
		{
			// A header carrying a scheme this surface does not read. The
			// credential itself resolves — the seam answers for it, and the use
			// case would reach the read model — so what is refused is the
			// credential's TRANSPORT, before the credential is interpreted at
			// all. A handler that understood more schemes than the contract
			// declares would be answering over an interface a caller generated
			// from this document has no way to speak.
			name:          "a scheme this surface does not read",
			query:         usageQuery,
			authorization: "Basic " + stubToken,
		},
		{
			// A credential the deployment never issued, presented the way the
			// contract's bearer scheme is spelled. The scheme's own case
			// sensitivity is a separate question with its own case below; what
			// matters here is that a well-formed credential which does not
			// resolve is refused identically to the four above.
			name:          "a credential the deployment never issued",
			query:         usageQuery,
			authorization: "Bearer a-token-this-deployment-did-not-issue",
		},
		{
			// RFC 7235 makes the scheme case-insensitive, and a handler that
			// read it case-sensitively would refuse a caller who spelled its own
			// header correctly. The credential here RESOLVES — it is the stub's
			// own — so this case is decided by the spelling of the scheme and by
			// nothing else, which is what makes it a case about the spelling.
			name:          "the scheme spelled in a case the RFC makes equivalent",
			query:         usageQuery,
			authorization: "bEaReR " + stubToken,
			wantStatus:    stdhttp.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := issueUsage(t, handler, tt.query, tt.authorization)

			want := tt.wantStatus
			if want == 0 {
				want = stdhttp.StatusUnauthorized
			}
			if rec.Code != want {
				t.Fatalf("status = %d, want %d; body = %q", rec.Code, want, rec.Body.String())
			}
			if want != stdhttp.StatusUnauthorized {
				// The accepting case has nothing to assert about the envelope;
				// it is here for its status, and its figures are the envelope's
				// own cases.
				return
			}
			envelope := errorEnvelopeOf(t, rec)
			if envelope.Error.Code != string(application.CodeUnauthenticated) {
				t.Errorf("code = %q, want %q; body = %q", envelope.Error.Code, application.CodeUnauthenticated, rec.Body.String())
			}
			if envelope.RequestID != usageRequestID {
				t.Errorf("request_id = %q, want %q", envelope.RequestID, usageRequestID)
			}
			if want := application.UnresolvedCredential().Message; envelope.Error.Message != want {
				t.Errorf("message = %q, want %q; every unresolved credential answers identically, and a caller that can tell two apart has been told one bit about the credential space", envelope.Error.Message, want)
			}
		})
	}

	// The read was reached by exactly the two credentials it could have been
	// reached by, and by neither of the rest: a request with no credential the
	// transport could read never became a read at all, while a well-formed
	// credential this deployment never issued DID — the seam is where resolution
	// happens, and the case above is about the refusal surviving the way back.
	//
	// Both tokens arrived exactly as presented, because extracting a credential
	// and handing it over whole is the whole of what this package does with one.
	tokens := make([]string, 0, len(useCases.calls))
	for _, call := range useCases.calls {
		tokens = append(tokens, call.token)
	}
	// In the order the cases above present them: the credential the deployment
	// never issued, then the one it did.
	want := []string{"a-token-this-deployment-did-not-issue", stubToken}
	if len(tokens) != len(want) || tokens[0] != want[0] || tokens[1] != want[1] {
		t.Errorf("the read was asked about %q by %d requests, want exactly %q — a credential the transport could not read belongs refused before the read, and one it could read belongs handed over whole",
			tokens, len(tests), want)
	}
}

// TestTheUsageSurfaceRefusesBeforeTheReadAnswers is the same 401 arrived at
// through the SEAM rather than through the handler, and it is its own case
// because the two paths are different code: this one has a credential the
// transport could read and could not resolve, so the refusal is produced below
// the transport and has to survive the mapping back.
//
// The distinction the two paths draw is which layer decides, not which layer
// answers: the handler refuses what it cannot read, and the seam refuses what it
// cannot resolve. Both reach the caller as the same 401, because the caller
// cannot act on the difference — and the token the seam refused on is the one the
// caller presented, which is what keeps this a claim about the transport rather
// than about the resolver's own table.
func TestTheUsageSurfaceRefusesBeforeTheReadAnswers(t *testing.T) {
	useCases := &fakeUsageUseCases{answer: emptyUsageAnswer()}
	handler := usageServer(useCases)

	rec := issueUsage(t, handler, usageQuery, "Bearer not-a-configured-token")

	if rec.Code != stdhttp.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, stdhttp.StatusUnauthorized, rec.Body.String())
	}
	if code := errorEnvelopeOf(t, rec).Error.Code; code != string(application.CodeUnauthenticated) {
		t.Errorf("code = %q, want %q; body = %q", code, application.CodeUnauthenticated, rec.Body.String())
	}
	if len(useCases.calls) != 1 {
		t.Fatalf("the read was asked %d times, want once: the refusal comes from the read, and this case is about it surviving the mapping", len(useCases.calls))
	}
	if got, want := useCases.calls[0].token, "not-a-configured-token"; got != want {
		t.Errorf("the read was asked about %q, want %q", got, want)
	}
}

// TestTheUsageSurfaceRefusesEveryUnanswerableQuestion is the 400 half of the
// surface, and every case here is a question the TRANSPORT refuses: a parameter
// it cannot parse, an unknown one, or one given twice.
//
// The refusals that belong to the domain and the use case — a grain outside the
// vocabulary, a zone with no calendar, a range that is not a range — are NOT
// here, and that is a line rather than an omission: they are decided below the
// seam this file fakes, and they are pinned where they live
// (internal/domain/analytics/query_test.go and TestUsageRefusesInvalidRequest).
// A copy of one here would be a test of the fake.
//
// What the handler owns in all of these is the same: one status, one code, and a
// message that names what to change. A caller sent back with a generic "invalid
// request" would send the same request again.
func TestTheUsageSurfaceRefusesEveryUnanswerableQuestion(t *testing.T) {
	// A malformed request is refused before the read, so the seam here holds a
	// valid answer that must never be served to any of these.
	useCases := &fakeUsageUseCases{answer: emptyUsageAnswer()}
	handler := usageServer(useCases)

	tests := []struct {
		name    string
		query   string
		wantSay []string
	}{
		{
			// The contract declares both bounds as RFC 3339 `date-time`, and a
			// bare date is not one. It is refused rather than read as the
			// start of a day in some zone the handler picked: "2026-09-01" is
			// ambiguous by a whole day of offset depending on which end of it
			// the reader assumed, and a report that resolved the ambiguity
			// silently is a report whose buckets are in the wrong place with
			// nothing to say so.
			name:    "a from that is a bare date rather than an instant",
			query:   "from=2026-09-01&to=2026-09-01T03:00:00Z&granularity=hour",
			wantSay: []string{"from", "RFC 3339"},
		},
		{
			name:    "a to that carries no zone",
			query:   "from=2026-09-01T00:00:00Z&to=2026-09-01T03:00:00&granularity=hour",
			wantSay: []string{"to", "RFC 3339"},
		},
		{
			// An ignored parameter is indistinguishable from a misspelled one: a
			// caller asking for `group_by=model` would otherwise receive a
			// correct answer about the wrong thing. This is the mutation that
			// silently widens the surface's question set, so it is a case.
			//
			// `account_id` is the same claim with the surface's own tenancy
			// behind it: the account is the credential's, and a parameter that
			// could name one would be a parameter a caller could tamper with.
			name:    "a query parameter this surface does not answer",
			query:   usageQuery + "&account_id=018f0000-0000-7000-8000-000000000009",
			wantSay: []string{"unknown query parameter account_id", "misspelled"},
		},
		{
			name:    "two query parameters this surface does not answer, named in a deterministic order",
			query:   usageQuery + "&group_by=model&account_id=018f0000-0000-7000-8000-000000000009",
			wantSay: []string{"account_id, group_by"},
		},
		{
			// A zone that was asked for and named nothing is not the same as no
			// zone, which is UTC: one is a question this surface cannot answer
			// and the other is the store's own zone. Reading this as UTC would
			// answer it anyway, in a way a caller with a broken client would
			// never notice.
			name:    "a timezone that was present and empty",
			query:   usageQuery + "&timezone=",
			wantSay: []string{"timezone", "empty"},
		},
		{
			// The contract's own maxLength. This zone is 65 characters, so the
			// only bound it can break is the octet count — which makes it a case
			// about the length check rather than about the calendar, and one
			// character shorter makes the two indistinguishable.
			name:    "a timezone longer than the octets the contract accepts",
			query:   usageQuery + "&timezone=" + strings.Repeat("a", 65),
			wantSay: []string{"64", "octets"},
		},
		{
			name:    "a from that is missing",
			query:   "to=2026-09-01T03:00:00Z&granularity=hour",
			wantSay: []string{"from", "required"},
		},
		{
			// Two questions wearing one name. Answering the first and reporting
			// the second's silence would leave a caller with an answer it cannot
			// tell was answered for the wrong value.
			name:    "a parameter given twice",
			query:   usageQuery + "&granularity=day",
			wantSay: []string{"granularity was given 2 times", "one value"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := issueUsage(t, handler, tt.query, "Bearer "+stubToken)

			if rec.Code != stdhttp.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %q", rec.Code, stdhttp.StatusBadRequest, rec.Body.String())
			}
			envelope := errorEnvelopeOf(t, rec)
			if envelope.Error.Code != string(application.CodeInvalidRequest) {
				t.Errorf("code = %q, want %q; body = %q", envelope.Error.Code, application.CodeInvalidRequest, rec.Body.String())
			}
			for _, fragment := range tt.wantSay {
				if !strings.Contains(envelope.Error.Message, fragment) {
					t.Errorf("message = %q, want it to name %q; a refusal that does not say what to change sends the same caller back with the same request", envelope.Error.Message, fragment)
				}
			}
		})
	}

	if len(useCases.calls) != 0 {
		t.Errorf("the read was asked %d times by requests the surface refuses before it can ask anything; a bound broken in the query string costs a caller nothing to be told about", len(useCases.calls))
	}
}

// TestTheUsageSurfaceAnswersAReadModelThatHoldsNothing pins the answer the
// contract calls out by name: a range this plane holds no derivation for is a
// 200, an empty `series` and `not_available` — an ANSWER, distinct from a
// failure and distinct from a range that was simply quiet.
//
// It is a case rather than a footnote because the alternative serialisations are
// both plausible: a `null` series, which a caller has to special-case, and a
// `200` with `availability: available` and no points, which is a different
// sentence about the same empty data. The figures are zero because zero is a
// value on this surface, not an absence.
func TestTheUsageSurfaceAnswersAReadModelThatHoldsNothing(t *testing.T) {
	handler := usageServer(&fakeUsageUseCases{answer: emptyUsageAnswer()})

	rec := issueUsage(t, handler, usageQuery, "Bearer "+stubToken)

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, stdhttp.StatusOK, rec.Body.String())
	}

	want := `{"availability":"not_available","metric":"usage",` +
		`"range":{"start_at":"2026-09-01T00:00:00Z","end_at":"2026-09-01T03:00:00Z","timezone":"UTC"},` +
		`"granularity":"hour",` +
		`"final_bucket_partial":false,"series":[],` +
		`"requests_with_usage_facts":0,"requests_settled":0,` +
		`"settled_amount_minor_units":0,"released_amount_minor_units":0,"funds_added_minor_units":0,` +
		`"held_minor_units":0,"available_minor_units":0,` +
		`"capture":{"reported":0,"gateway_observed":0,"reservation_floor":0},` +
		`"freshness":{"data_through":"2026-09-01T00:00:00Z","basis":"fact_feed_pass"}}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("the not_available envelope is\n got %s\nwant %s", got, want)
	}
}

// TestTheUsageSurfaceRendersTheContractedEnvelope is the case that would catch a
// field renamed, a unit moved or a type swapped — the three conversions a
// reflection-based marshaller gets approximately right, which is how a date-time
// ends up in local time and a _minor_units field ends up in major units.
//
// The corpus below is written to make each of those a visible difference: every
// figure is a distinct value, so a field swapped with its neighbour cannot
// match, and the two request counts are not equal in either the series or the
// totals, so a mix-up between the two populations a caller must not confuse is a
// different number. One bucket holds no settlements — a free-model hour is a
// real answer rather than a missing one — and the two lists must not be confused
// with the two figures in the answer that are BALANCES as at the read's own
// instant rather than sums over the range.
func TestTheUsageSurfaceRendersTheContractedEnvelope(t *testing.T) {
	handler := usageServer(&fakeUsageUseCases{answer: UsageAnswer{
		Availability: "available",
		Granularity:  "hour",
		From:         usageFrom,
		To:           usageTo,
		Timezone:     "UTC",
		Series: []UsageAnswerBucket{
			{Start: usageFrom, End: usageFrom.Add(time.Hour), RequestsWithUsageFacts: 412, RequestsSettled: 410},
			// A bucket with no settlements in it is still a bucket, and zero
			// is what it holds: a free-model hour is a real answer, not a
			// missing one, and a series that omitted it would leave the
			// caller inventing a rule for which gaps to fill.
			{Start: usageFrom.Add(time.Hour), End: usageFrom.Add(2 * time.Hour), RequestsWithUsageFacts: 3, RequestsSettled: 0},
			{Start: usageFrom.Add(2 * time.Hour), End: usageFrom.Add(3 * time.Hour), RequestsWithUsageFacts: 7, RequestsSettled: 7},
		},
		RequestsWithUsageFacts: 422,
		RequestsSettled:        417,
		SettledMinorUnits:      4321,
		ReleasedMinorUnits:     654,
		FundsAddedMinorUnits:   100000,
		HeldMinorUnits:         5000,
		AvailableMinorUnits:    97500,
		Capture:                UsageAnswerCapture{Reported: 11, GatewayObserved: 12, ReservationFloor: 13},
		Freshness:              UsageAnswerFreshness{DataThrough: usageDataThrough, Basis: "fact_feed_pass"},
	}})

	rec := issueUsage(t, handler, usageQuery, "Bearer "+stubToken)

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, stdhttp.StatusOK, rec.Body.String())
	}

	want := `{"availability":"available","metric":"usage",` +
		`"range":{"start_at":"2026-09-01T00:00:00Z","end_at":"2026-09-01T03:00:00Z","timezone":"UTC"},` +
		`"granularity":"hour",` +
		`"final_bucket_partial":false,` +
		`"series":[` +
		`{"bucket_start":"2026-09-01T00:00:00Z","bucket_end":"2026-09-01T01:00:00Z","requests_with_usage_facts":412,"requests_settled":410},` +
		`{"bucket_start":"2026-09-01T01:00:00Z","bucket_end":"2026-09-01T02:00:00Z","requests_with_usage_facts":3,"requests_settled":0},` +
		`{"bucket_start":"2026-09-01T02:00:00Z","bucket_end":"2026-09-01T03:00:00Z","requests_with_usage_facts":7,"requests_settled":7}],` +
		`"requests_with_usage_facts":422,"requests_settled":417,` +
		`"settled_amount_minor_units":4321,"released_amount_minor_units":654,"funds_added_minor_units":100000,` +
		`"held_minor_units":5000,"available_minor_units":97500,` +
		`"capture":{"reported":11,"gateway_observed":12,"reservation_floor":13},` +
		`"freshness":{"data_through":"2026-09-01T00:00:00Z","basis":"fact_feed_pass"}}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("the usage envelope is\n got %s\nwant %s", got, want)
	}
}

// TestTheUsageEnvelopeCarriesNoAccount is the tenancy property, asserted on the
// wire rather than in a comment: there is no `account_id` in this surface's
// response, and the token a caller presented does not appear in it either.
//
// The account is proven by the numbers, not asserted by an echo. An envelope
// carrying the account would be one a caller could assert against — the one
// field a cross-account test would want to tamper with if this surface ever grew
// a scope parameter — and a caller that could read its scope back would have a
// second source of truth about which account it was answered for, which is the
// one thing the derivation exists to avoid.
//
// The answer is the one a read produces, because it is the ANSWER that would
// have to carry an account for it to reach the wire: the seam has no account to
// put in it either (usage.go).
func TestTheUsageEnvelopeCarriesNoAccount(t *testing.T) {
	answer := emptyUsageAnswer()
	answer.Availability = "available"
	answer.Series = []UsageAnswerBucket{{Start: usageFrom, End: usageTo, RequestsWithUsageFacts: 1, RequestsSettled: 1}}

	rec := issueUsage(t, usageServer(&fakeUsageUseCases{answer: answer}), usageQuery, "Bearer "+stubToken)

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, stdhttp.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leak := range []string{stubAccount, stubToken, "account", "scope"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(leak)) {
			t.Errorf("the usage envelope names %q; the scope is derived on the server and proven by the figures, not echoed to the caller: %s", leak, body)
		}
	}
}

// TestTheUsageEnvelopeFollowsTheZoneItWasCutIn is the transport's half of the
// zone contract, and the answer it is handed is the shape the use case produces
// for a caller who asked in a zone whose offset is not a whole number of hours:
// the same three-hour range cut into four buckets, the outer two of them
// partial, with the display zone travelling beside the instants.
//
// Two claims are the transport's and no one else's. The first is that the
// response ECHOES the zone as the answer named it, so a caller who asked for a
// zone is told which one its buckets were cut in without the transport
// re-deriving a name from the offsets. The second is that every instant goes out
// as a UTC reading: this answer's instants are located in the display zone, and
// a marshaller that formatted them where they stood would write
// `2026-09-01T05:30:00+05:30` — a correct instant that a caller comparing two
// responses from two zones could not compare, and that a client in a third zone
// would have to re-derive. The contract's `timezone` parameter says every figure
// it stores is UTC and that only the labels move; this is the line where that
// stops being a convention and becomes a property of the response.
//
// The range's end falls inside the last bucket here rather than on its edge,
// which is what almost every real range looks like: `final_bucket_partial` is a
// fact about the RANGE rather than about the data, and an empty partial bucket
// is still a partial bucket. A caller drawing it as a complete zero would be
// drawing a number this plane never claimed.
func TestTheUsageEnvelopeFollowsTheZoneItWasCutIn(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("the test's own fixture zone Asia/Kolkata did not load: %v", err)
	}
	// The hour containing 00:00Z begins at 23:30Z the previous day, and the
	// range's end falls inside the hour containing 03:00Z — so the walk yields
	// FOUR buckets for a three-hour range: the first is a partial at the bottom
	// and the last is a partial at the top.
	handler := usageServer(&fakeUsageUseCases{answer: UsageAnswer{
		Availability:       "available",
		Granularity:        "hour",
		From:               usageFrom,
		To:                 usageTo,
		Timezone:           "Asia/Kolkata",
		FinalBucketPartial: true,
		Series: []UsageAnswerBucket{
			{Start: time.Date(2026, time.August, 31, 23, 30, 0, 0, time.UTC), End: time.Date(2026, time.September, 1, 0, 30, 0, 0, time.UTC)},
			{Start: time.Date(2026, time.September, 1, 0, 30, 0, 0, time.UTC), End: time.Date(2026, time.September, 1, 1, 30, 0, 0, time.UTC)},
			{Start: time.Date(2026, time.September, 1, 1, 30, 0, 0, time.UTC), End: time.Date(2026, time.September, 1, 2, 30, 0, 0, time.UTC)},
			// The last bucket, spelled in the DISPLAY zone rather than in UTC,
			// so a marshaller that formatted the instant where it stood would
			// answer the caller with an offset. Both spellings are one instant;
			// only one of them is the contract's.
			{Start: time.Date(2026, time.September, 1, 8, 0, 0, 0, kolkata), End: time.Date(2026, time.September, 1, 9, 0, 0, 0, kolkata)},
		},
		Freshness: UsageAnswerFreshness{DataThrough: usageDataThrough, Basis: "fact_feed_pass"},
	}})

	rec := issueUsage(t, handler, usageQuery+"&timezone=Asia/Kolkata", "Bearer "+stubToken)

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, stdhttp.StatusOK, rec.Body.String())
	}

	envelope := usageEnvelope{}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("the usage body %q is not the contract's envelope: %v", rec.Body.String(), err)
	}

	if envelope.Range.Timezone != "Asia/Kolkata" {
		t.Errorf("range.timezone = %q, want the zone the answer named, %q", envelope.Range.Timezone, "Asia/Kolkata")
	}
	if envelope.Metric != "usage" {
		t.Errorf("metric = %q, want %q; a caller binds a renderer to a family rather than to a field that happens to be present today", envelope.Metric, "usage")
	}
	if envelope.Availability != "available" {
		t.Errorf("availability = %q, want %q; an answer carrying buckets has answered the range asked of", envelope.Availability, "available")
	}
	// The grain travels beside the range, because a figure cannot be
	// re-aggregated without re-deriving what a bucket meant — and a report
	// mixing grains is a defect rather than a wide sheet.
	if envelope.Granularity != "hour" {
		t.Errorf("granularity = %q, want %q", envelope.Granularity, "hour")
	}
	if envelope.Range.StartAt != "2026-09-01T00:00:00Z" || envelope.Range.EndAt != "2026-09-01T03:00:00Z" {
		t.Errorf("the range came back as [%s, %s), want the range asked for; a zone moves where a bucket begins, not where the range is",
			envelope.Range.StartAt, envelope.Range.EndAt)
	}
	if !envelope.FinalBucketPartial {
		t.Error("final_bucket_partial = false, want true; the range's end falls inside the last bucket, and that is a fact about the range rather than about the data")
	}
	if len(envelope.Series) != 4 {
		t.Fatalf("the series has %d points, want 4; the first bucket is the one CONTAINING the range's start, so an unaligned start yields one more than the hours it spans: %+v",
			len(envelope.Series), envelope.Series)
	}
	if got, want := envelope.Series[0].Start, "2026-08-31T23:30:00Z"; got != want {
		t.Errorf("the first bucket begins at %s, want %s — a zone whose offset is not a whole number of hours moves the edge to the half hour", got, want)
	}
	if got, want := envelope.Series[3].Start, "2026-09-01T02:30:00Z"; got != want {
		t.Errorf("the last bucket begins at %s, want %s; an instant located in the display zone is still the caller's UTC reading of that edge", got, want)
	}
	if got, want := envelope.Series[3].End, "2026-09-01T03:30:00Z"; got != want {
		t.Errorf("the last bucket ends at %s, want %s", got, want)
	}
}

// TestAReadThatOutranItsBudgetIsARetryLaterAnswer is the 503 the contract
// promises for a read that exceeds its deadline, and the reason it exists is
// that a 500 would read as a defect in this service rather than as a load shape
// it would answer a moment later.
//
// The deadline is exercised in the form the port can surface it: the context's
// own context.DeadlineExceeded, carried through the use case's Internal, whose
// Unwrap chain is the only thing this mapping can read. A driver that returns
// its own "canceling statement due to statement timeout" instead is translated
// by the store adapter to a sentinel the same chain carries, and the mapping is
// the same either way — which is the reason this case asserts the status and the
// code rather than the cause.
func TestAReadThatOutranItsBudgetIsARetryLaterAnswer(t *testing.T) {
	// Wrapped rather than bare, because that is how it arrives: the use case
	// names the budget it broke and keeps the cause, and the transport's whole
	// job here is to read through the wrapping.
	useCases := &fakeUsageUseCases{err: fmt.Errorf("the analytics read exceeded its %s budget: %w", application.AnalyticsReadTimeout, context.DeadlineExceeded)}
	handler := usageServer(useCases)

	rec := issueUsage(t, handler, usageQuery, "Bearer "+stubToken)

	if rec.Code != stdhttp.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, stdhttp.StatusServiceUnavailable, rec.Body.String())
	}
	envelope := errorEnvelopeOf(t, rec)
	if envelope.Error.Code != "service_unavailable" {
		t.Errorf("code = %q, want %q; body = %q", envelope.Error.Code, "service_unavailable", rec.Body.String())
	}
	if envelope.Error.Message != readTimeoutMessage {
		t.Errorf("message = %q, want %q; a read that ran out of time names no figure, no range and no account", envelope.Error.Message, readTimeoutMessage)
	}
	// A failed read is a failed read, not a partial answer: the series is not
	// reported for a range whose later buckets were never read, because a chart
	// drawn from a truncated series would show real activity having stopped at
	// whatever instant the deadline landed on.
	if strings.Contains(rec.Body.String(), `"series"`) {
		t.Errorf("a read that ran out of time still reported a series: %q", rec.Body.String())
	}
	if len(useCases.calls) != 1 {
		t.Errorf("the read was asked %d times, want once", len(useCases.calls))
	}
}

// TestAFailedReadIsAnInternalErrorAndNotAPartialAnswer is the other half of the
// same pair: a read that fails for a reason that is not the deadline is a 500,
// not a 503, because the condition is not one a retry is expected to clear.
//
// The two statuses mean opposite things to a caller, which is why they cannot
// both be 503: a retry-later answer tells the console to try the same report
// again in a moment, and telling it that about a query it will never answer is a
// caller spinning on a defect. The cause never reaches the body either way.
func TestAFailedReadIsAnInternalErrorAndNotAPartialAnswer(t *testing.T) {
	secret := "postgres://usage:super-secret@database.example/gateway?sslmode=disable"
	useCases := &fakeUsageUseCases{err: fmt.Errorf("read the analytics usage: %w", errors.New(secret))}

	rec := issueUsage(t, usageServer(useCases), usageQuery, "Bearer "+stubToken)

	if rec.Code != stdhttp.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, stdhttp.StatusInternalServerError, rec.Body.String())
	}
	envelope := errorEnvelopeOf(t, rec)
	if envelope.Error.Code != string(application.CodeInternal) {
		t.Errorf("code = %q, want %q; body = %q", envelope.Error.Code, application.CodeInternal, rec.Body.String())
	}
	if envelope.Error.Message != internalErrorMessage {
		t.Errorf("message = %q, want %q", envelope.Error.Message, internalErrorMessage)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("the refusal leaked the store's cause: %q", rec.Body.String())
	}
}

// TestTheUsageSurfaceRefusesEveryMethodItDoesNotServe is the last of the route's
// four answers, and it is here rather than in the route table because a table row
// is a claim about a path and not about what answering it does.
//
// The companion answers a mismatched method on a path the contract declares
// with 405 and the contracted envelope, and says in the Allow header which
// methods the path does take. ServeMux's own 405 is a plain-text one, which is
// a response shape this contract does not have.
func TestTheUsageSurfaceRefusesEveryMethodItDoesNotServe(t *testing.T) {
	handler := usageServer(&fakeUsageUseCases{answer: emptyUsageAnswer()})

	for _, method := range []string{stdhttp.MethodPost, stdhttp.MethodPut, stdhttp.MethodDelete, stdhttp.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, usagePath+"?"+usageQuery, nil)
			req.Header.Set(RequestIDHeader, usageRequestID)
			req.Header.Set("Authorization", "Bearer "+stubToken)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != stdhttp.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d; body = %q", rec.Code, stdhttp.StatusMethodNotAllowed, rec.Body.String())
			}
			if got, want := rec.Header().Get("Allow"), "GET, HEAD"; got != want {
				t.Errorf("Allow = %q, want %q", got, want)
			}
			if got, want := rec.Body.String(),
				`{"error":{"code":"method_not_allowed","message":"method not allowed"},"request_id":"usage-request"}`+"\n"; got != want {
				t.Errorf("body = %q, want %q", got, want)
			}
		})
	}
}
