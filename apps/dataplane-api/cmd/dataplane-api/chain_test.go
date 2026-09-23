package main

import (
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/adapters/inbound/http"
	dataplaneadapter "github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/adapters/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/application"
)

// The composition is tested here, at the composition root, and that placement
// is the architecture rather than a convenience.
//
// A test that wires the real outbound adapter into the real inbound handler has
// to import both trees, and the only package allowed to do that is `cmd` — the
// dependency rule refuses `internal/adapters/inbound` importing
// `internal/adapters/outbound` and the reverse, because a façade whose surface
// can reach the cross-plane client directly is a façade whose one seam can be
// bypassed from a route. So the chain this test builds — console-api's shape,
// this application's handler, this application's adapter, and a server standing
// in for the Data Plane — is exactly the object only this package can
// construct, which is what makes it worth constructing.
//
// What it pins is the façade's whole promise: the page arrives at the caller
// byte for byte, with the cursor's characters and the payload's characters
// intact, and the deployment's credential went out with the call.
//
// The assertion is on the exact response body rather than on decoded fields
// because "the façade transforms nothing" is a claim about bytes. A façade that
// re-encoded the cursor, escaped it for HTML or dropped a key would satisfy a
// field-by-field check and fail this one.

// chainCredential is the shared secret the chain is configured with on both
// sides. It is distinctive so that a leak into a log line, an error or a
// response body is a finding rather than a coincidence.
const chainCredential = "chain-test-service-credential-4b17"

// chainCursor is a position the façade is required not to understand. It
// carries a space, an ampersand, an angle bracket and a colon for the same
// reason: every one of them is a character a transit layer is tempted to
// re-encode.
const chainCursor = "cur:9f2 &=<not-a-number>/+=="

// chainUpstreamPage is the Data Plane's answer as the upstream sends it —
// compact, which is what makes the byte comparison below meaningful: the
// façade's own encoder, not the upstream's formatting, decides what the caller
// receives.
const chainUpstreamPage = `{"events":[{"request_id":"req_01HZ","kind":"settled","schema_version":1,"occurred_at":"2026-09-23T10:00:00Z","payload":{"allocation_id":"alloc-1","note":"a<b & c>d"}}],"next_cursor":"` + chainCursor + `","has_more":true}`

// TestTheChainCarriesThePageFromTheDataPlaneToTheCaller is the end-to-end
// composition: a caller with this deployment's credential asks this
// application for a page, and the bytes the Data Plane answered are the bytes
// the caller receives.
func TestTheChainCarriesThePageFromTheDataPlaneToTheCaller(t *testing.T) {
	type upstreamCall struct {
		authorization string
		after         string
		limit         string
	}
	observed := make(chan upstreamCall, 1)
	upstream := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		query := r.URL.Query()
		observed <- upstreamCall{
			authorization: r.Header.Get("Authorization"),
			after:         query.Get("after"),
			limit:         query.Get("limit"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chainUpstreamPage))
	}))
	t.Cleanup(upstream.Close)

	// The wiring main() performs, in the order it performs it: the port, the
	// application over it, the handler over that.
	app := application.New("test", dataplaneadapter.New(upstream.Client(), upstream.URL, chainCredential))
	handler := http.New(app, http.NewServiceAuthenticator(chainCredential))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(stdhttp.MethodGet, "/internal/usage-events?after="+url.QueryEscape(chainCursor)+"&limit=7", nil)
	req.Header.Set("Authorization", "Bearer "+chainCredential)
	handler.ServeHTTP(rec, req)

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("GET /internal/usage-events status = %d, want %d (body %q)", rec.Code, stdhttp.StatusOK, rec.Body.String())
	}
	if got, want := rec.Body.String(), chainUpstreamPage+"\n"; got != want {
		t.Errorf("the page did not cross unchanged:\n got %q\nwant %q", got, want)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	call := <-observed
	if want := "Bearer " + chainCredential; call.authorization != want {
		t.Errorf("the Data Plane saw Authorization = %q, want %q", call.authorization, want)
	}
	if call.after != chainCursor {
		t.Errorf("the Data Plane saw after = %q, want the caller's cursor %q", call.after, chainCursor)
	}
	if call.limit != "7" {
		t.Errorf("the Data Plane saw limit = %q, want 7", call.limit)
	}
}

// TestTheChainRefusesACallerWithNoCredential is the same wiring with the one
// thing changed that must change the answer. It is here rather than in the
// handler's own tests because the claim is about the chain: a caller the
// deployment does not trust reaches neither the Data Plane nor this
// application's use case, whichever of the two the deployment misconfigured.
func TestTheChainRefusesACallerWithNoCredential(t *testing.T) {
	var upstreamCalls int
	upstream := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chainUpstreamPage))
	}))
	t.Cleanup(upstream.Close)

	app := application.New("test", dataplaneadapter.New(upstream.Client(), upstream.URL, chainCredential))
	handler := http.New(app, http.NewServiceAuthenticator(chainCredential))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(stdhttp.MethodGet, "/internal/usage-events", nil))

	if rec.Code != stdhttp.StatusUnauthorized {
		t.Errorf("GET /internal/usage-events with no credential: status = %d, want %d", rec.Code, stdhttp.StatusUnauthorized)
	}
	if upstreamCalls != 0 {
		t.Errorf("the Data Plane was called %d times by an unauthenticated request", upstreamCalls)
	}
}
