package main

import (
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/config"
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

// The chain is configured with two credentials, and the two being distinct is
// the point rather than an inconvenience. One secret serving both hops is the
// defect this application's configuration refuses at startup and that its
// wiring must not reintroduce: whoever may call the management surface would
// also hold the key to the Data Plane's private listener, and would reach the
// untranslated status vocabulary behind the façade. A test that configured both
// hops with one value could not tell the correct wiring from a swapped pair —
// every assertion below would pass either way — which is exactly how this test
// used to be written.
//
// Both are distinctive so that a leak into a log line, an error or a response
// body is a finding rather than a coincidence.
const (
	chainServiceCredential   = "chain-test-caller-credential-4b17"
	chainDataPlaneCredential = "chain-test-hop-two-credential-9c02"
)

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

// chainUpstream starts a server standing in for the Data Plane's private
// listener, counting the calls it receives. The counter is what the refusal
// tests assert on: an unauthenticated caller must reach neither this server nor
// the use case behind it, and a count of zero is the part of that sentence a
// status code alone cannot show.
func chainUpstream(t *testing.T, calls *int) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		*calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chainUpstreamPage))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// chainConfig is the configuration main() would have loaded, with the two hops'
// secrets deliberately different.
func chainConfig(dataPlaneURL string) config.Config {
	return config.Config{
		DataPlaneURL:        dataPlaneURL,
		ServiceCredential:   chainServiceCredential,
		DataPlaneCredential: chainDataPlaneCredential,
	}
}

// TestTheChainCarriesThePageFromTheDataPlaneToTheCaller is the end-to-end
// composition: a caller with this deployment's credential asks this
// application for a page, and the bytes the Data Plane answered are the bytes
// the caller receives.
//
// It drives `newHandler` — the function main() composes the server from — with
// a configuration rather than rebuilding the wiring here, because the wiring is
// what is under test. Four lines repeated in this file would be four lines that
// keep passing after main.go's were exchanged, and the exchange is the defect
// that matters: it hands every management caller the key to the private
// listener.
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

	handler := newHandler(chainConfig(upstream.URL), upstream.Client())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(stdhttp.MethodGet, "/internal/usage-events?after="+url.QueryEscape(chainCursor)+"&limit=7", nil)
	req.Header.Set("Authorization", "Bearer "+chainServiceCredential)
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
	// The direction of the second credential, asserted where it can be seen:
	// the Data Plane is presented the hop-two secret, and never the one the
	// caller presented here.
	if want := "Bearer " + chainDataPlaneCredential; call.authorization != want {
		t.Errorf("the Data Plane saw Authorization = %q, want %q — the adapter must present the hop-two credential, not the caller-facing one", call.authorization, want)
	}
	if call.authorization == "Bearer "+chainServiceCredential {
		t.Error("the Data Plane was presented the caller-facing credential: one secret is serving both hops, so whoever may call this surface can also reach the private listener")
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
	upstream := chainUpstream(t, &upstreamCalls)

	handler := newHandler(chainConfig(upstream.URL), upstream.Client())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(stdhttp.MethodGet, "/internal/usage-events", nil))

	if rec.Code != stdhttp.StatusUnauthorized {
		t.Errorf("GET /internal/usage-events with no credential: status = %d, want %d", rec.Code, stdhttp.StatusUnauthorized)
	}
	if upstreamCalls != 0 {
		t.Errorf("the Data Plane was called %d times by an unauthenticated request", upstreamCalls)
	}
}

// TestTheChainsCredentialsAreNotInterchangeable is the other half of the
// wiring's one claim, and the half a swap would otherwise survive.
//
// The deployment holds two secrets: the one a caller presents here, and the one
// this process presents to the Data Plane. They are unequal — internal/config
// refuses a deployment that sets them equal — but equality is not the property
// that matters. The property is that they are not *interchangeable*: a caller
// holding the hop-two secret must still be refused at this surface, or the
// separate credential buys nothing at all, because the party the caller-facing
// secret was handed to is exactly the party that must not reach the private
// listener.
//
// So the test presents each secret to the façade and requires opposite answers.
// A wiring that exchanged the two arguments answers 401 to the first and 200 to
// the second, which is the failure this asserts against; so does a wiring that
// handed both hops the same value.
func TestTheChainsCredentialsAreNotInterchangeable(t *testing.T) {
	tests := []struct {
		name         string
		presented    string
		wantStatus   int
		wantUpstream int
		explanation  string
	}{
		{
			name:         "the credential this deployment gives its callers is admitted",
			presented:    chainServiceCredential,
			wantStatus:   stdhttp.StatusOK,
			wantUpstream: 1,
			explanation:  "a caller the deployment trusts must reach the Data Plane through the façade",
		},
		{
			name:         "the credential this process presents to the Data Plane is not a caller credential",
			presented:    chainDataPlaneCredential,
			wantStatus:   stdhttp.StatusUnauthorized,
			wantUpstream: 0,
			explanation:  "the hop-two secret is what the façade sends outward; a caller holding it must not be able to call inward with it, or the two hops share one credential in effect",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamCalls int
			upstream := chainUpstream(t, &upstreamCalls)

			handler := newHandler(chainConfig(upstream.URL), upstream.Client())

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(stdhttp.MethodGet, "/internal/usage-events", nil)
			req.Header.Set("Authorization", "Bearer "+tt.presented)
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("presenting the credential %q: status = %d, want %d (%s)", tt.name, rec.Code, tt.wantStatus, tt.explanation)
			}
			if upstreamCalls != tt.wantUpstream {
				t.Errorf("the Data Plane was called %d times, want %d — %s", upstreamCalls, tt.wantUpstream, tt.explanation)
			}
		})
	}
}
