package main

import (
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// This file is the catalog read's half of chain_test.go, and it is a separate
// file because it pins a different promise about the same wiring: the page's
// test is about values crossing unchanged, and this one is about a *name*
// crossing unchanged — including the one name that is also syntax, `*` — and
// about the private listener's refusal of an unknown group arriving as this
// surface's own contracted 404 rather than as a translation of the upstream's
// body.
//
// Everything about the composition is the object under test for the same reason
// chain_test.go gives: only `cmd` may construct the whole chain, so only here
// can the assertion be about the deployment's real wiring rather than about a
// rebuilt one.

// chainUpstreamVersion is the catalog answer the upstream sends, compact for the
// same reason the page fixture is: the façade's encoder, not the upstream's
// formatting, decides what the caller receives.
const chainUpstreamVersion = `{"group_name":"frontier","version":3,"group_version_id":"0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"}`

// chainPrivateMarker is text the private listener puts in its refusal body and
// that appears nowhere in this module's vocabulary. Any of it reaching the
// caller is the façade relaying the hop behind it, which is the one thing this
// seam exists to prevent.
const chainPrivateMarker = "private-listener-refusal-marker-71e4"

// versionUpstream starts a server standing in for the Data Plane's private
// listener on the catalog read: it answers status and body as configured and
// records the escaped path it was called at, so the tests can assert on the
// spelling that crossed rather than on a decoded reconstruction of it.
func versionUpstream(t *testing.T, status int, body string, path *string) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if path != nil {
			*path = r.URL.EscapedPath()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// TestTheChainCarriesTheCatalogReadFromTheDataPlaneToTheCaller is the
// composition for the second operation: a caller with this deployment's
// credential asks for a group's current version, and the three values the
// private listener answered are the three the caller receives — the id byte for
// byte, because that id is what an entitlement will store as its scope.
func TestTheChainCarriesTheCatalogReadFromTheDataPlaneToTheCaller(t *testing.T) {
	upstreamPath := ""
	upstream := versionUpstream(t, stdhttp.StatusOK, chainUpstreamVersion, &upstreamPath)

	handler := newHandler(chainConfig(upstream.URL), upstream.Client())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(stdhttp.MethodGet, "/internal/alias-groups/frontier/versions/current", nil)
	req.Header.Set("Authorization", "Bearer "+chainServiceCredential)
	handler.ServeHTTP(rec, req)

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("GET the catalog read: status = %d, want %d (body %q)", rec.Code, stdhttp.StatusOK, rec.Body.String())
	}
	if got, want := rec.Body.String(), chainUpstreamVersion+"\n"; got != want {
		t.Errorf("the version did not cross unchanged:\n got %q\nwant %q", got, want)
	}
	// The hop's own credential went out, as the page's test asserts and as this
	// operation's wiring is the same one line of composition.
	if !strings.Contains(upstreamPath, "/internal/alias-groups/frontier/versions/current") {
		t.Errorf("the private listener was called at %q, want the operation's own path", upstreamPath)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// TestTheChainCarriesTheWildcardNameThroughTheWholeHop is the end-to-end form
// of the name's journey, on the one name the seam exists to serve that a router
// would be tempted to interpret: the caller sends `*` raw, the façade sends
// `%2A` to the private listener, and the catalog's answer names the group `*`
// again — so a commerce roll that pins the wildcard's scope reads back the
// group it asked about, and nothing between the two ends decided the character
// meant something else.
//
// The request is sent both ways a caller may spell it, raw and percent-encoded,
// because the contract admits both and a façade that decoded one and not the
// other would make the wildcard's reachability depend on a client's escaping
// choice.
func TestTheChainCarriesTheWildcardNameThroughTheWholeHop(t *testing.T) {
	tests := []struct {
		name         string
		target       string
		wantUpstream string
	}{
		{
			name:         "the raw character",
			target:       "/internal/alias-groups/*/versions/current",
			wantUpstream: "/internal/alias-groups/%2A/versions/current",
		},
		{
			name:         "the percent-encoded character",
			target:       "/internal/alias-groups/%2A/versions/current",
			wantUpstream: "/internal/alias-groups/%2A/versions/current",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamPath := ""
			upstream := versionUpstream(t, stdhttp.StatusOK, `{"group_name":"*","version":1,"group_version_id":"0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"}`, &upstreamPath)

			handler := newHandler(chainConfig(upstream.URL), upstream.Client())

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(stdhttp.MethodGet, tt.target, nil)
			req.Header.Set("Authorization", "Bearer "+chainServiceCredential)
			handler.ServeHTTP(rec, req)

			if rec.Code != stdhttp.StatusOK {
				t.Fatalf("GET %s: status = %d, want %d (body %q) — the wildcard is an ordinary group name to this operation", tt.target, rec.Code, stdhttp.StatusOK, rec.Body.String())
			}
			if upstreamPath != tt.wantUpstream {
				t.Errorf("the private listener was called at %q, want %q — the name crosses as one escaped segment", upstreamPath, tt.wantUpstream)
			}
			if want := `"group_name":"*"`; !strings.Contains(rec.Body.String(), want) {
				t.Errorf("the answer %q does not name the group %s byte for byte", rec.Body.String(), want)
			}
		})
	}
}

// TestTheChainAnswersAnUnknownGroupWithItsOwn404 is the mapping that makes the
// operation useful to its caller, asserted through the whole wiring: the
// private listener's 404 arrives as this surface's 404, in this application's
// envelope, with the message the contract declares — and with none of the
// upstream's body in it, because the caller is a Control Plane that has no
// business learning the private protocol's wording.
//
// The status is checked before the body on purpose: a 500 here would be the
// failure mode this seam was built against, a façade that could not tell "the
// catalog answered no" from "the catalog is unreachable" and reported both as
// its own breakage.
func TestTheChainAnswersAnUnknownGroupWithItsOwn404(t *testing.T) {
	upstream := versionUpstream(t, stdhttp.StatusNotFound,
		`{"error":{"code":"not_found","message":"`+chainPrivateMarker+`"},"request_id":"r-1"}`, nil)

	handler := newHandler(chainConfig(upstream.URL), upstream.Client())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(stdhttp.MethodGet, "/internal/alias-groups/unprovisioned/versions/current", nil)
	req.Header.Set("Authorization", "Bearer "+chainServiceCredential)
	handler.ServeHTTP(rec, req)

	if rec.Code != stdhttp.StatusNotFound {
		t.Fatalf("GET an unprovisioned group: status = %d, want %d (body %q)", rec.Code, stdhttp.StatusNotFound, rec.Body.String())
	}
	body := rec.Body.String()
	if want := `"code":"not_found"`; !strings.Contains(body, want) {
		t.Errorf("the answer %q does not carry %s", body, want)
	}
	if want := "no version of the requested alias group exists"; !strings.Contains(body, want) {
		t.Errorf("the answer %q does not carry the contract's message", body)
	}
	if strings.Contains(body, chainPrivateMarker) {
		t.Errorf("the answer %q carries the private listener's own wording; the façade translates and does not relay", body)
	}
}
