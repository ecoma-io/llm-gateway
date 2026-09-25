package http

import (
	"errors"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// aGroupVersion is the answer the catalog read's tests are written against: the
// third version of a group whose membership has rolled twice, whose id is what
// a commerce roll would pin.
var aGroupVersion = dataplane.GroupVersion{
	GroupName:      "frontier",
	Version:        3,
	GroupVersionID: "0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d",
}

// authedCatalogRead builds an authenticated GET for the catalog read of group.
func authedCatalogRead(group string) *stdhttp.Request {
	req := httptest.NewRequest(stdhttp.MethodGet, catalogReadTarget(group), nil)
	req.Header.Set("Authorization", "Bearer "+testCredential)
	return req
}

// catalogReadTarget renders the operation's path for a group name, escaping the
// segment the way the contract tells a caller to: characters that would end the
// segment go percent-encoded, and the wildcard's `*` is legal either way. It is
// a helper rather than string concatenation at every call site because the
// escaping is part of the operation's declared shape, not a detail of a test.
func catalogReadTarget(group string) string {
	return "/internal/alias-groups/" + strings.ReplaceAll(strings.ReplaceAll(group, "%", "%25"), "/", "%2F") + "/versions/current"
}

func TestTheCatalogReadIsTheThreeFields(t *testing.T) {
	catalog := &fakeCatalog{version: aGroupVersion}
	handler := testHandler(application.New("test", &fakeUsageFacts{}, catalog, &fakeProjection{}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedCatalogRead("frontier"))

	// Pinned as bytes rather than decoded, for the reason the feed's body is:
	// this is the object the Control Plane's commerce roll reads a scope from,
	// and a decoder would pass a renamed field without a word.
	want := `{"group_name":"frontier","version":3,"group_version_id":"0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if rec.Code != stdhttp.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, stdhttp.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get(RequestIDHeader); got == "" {
		t.Errorf("%s is missing; every response carries one", RequestIDHeader)
	}
}

// TestANameWhoseDecodingLooksNonCanonicalStillReachesTheCatalog pins the
// guard's segment discipline on the façade side: the structure of a request
// is judged on its escaped path, so a legal name whose percent-encoded form
// rides through — and whose decoded form merely resembles a doubled slash or
// a dot segment — is one segment, and reaches the catalog as itself. A guard
// on the decoded path would 404 it before the mux ever ran, and the caller
// would read a provisioned group as "no version exists".
func TestANameWhoseDecodingLooksNonCanonicalStillReachesTheCatalog(t *testing.T) {
	catalog := &fakeCatalog{version: dataplane.GroupVersion{
		GroupName: "a//b", Version: 2, GroupVersionID: "0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d",
	}}
	handler := testHandler(application.New("test", &fakeUsageFacts{}, catalog, &fakeProjection{}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedCatalogRead("a//b"))

	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d (body %s), want %d — the name is one escaped segment, not a non-canonical path", rec.Code, rec.Body.String(), stdhttp.StatusOK)
	}
	if want := `"group_name":"a//b"`; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body = %s, want it to name the group %s", rec.Body.String(), want)
	}
	if got := catalog.names; len(got) != 1 || got[0] != "a//b" {
		t.Errorf("the port was asked about %v, want [a//b]", got)
	}
}

// TestTheWildcardNameReachesTheCatalogAsItself is the catalog read's own
// segment test, on the surface a caller actually reaches. The reserved name is
// a single `*`, which is also wildcard syntax in the mux's pattern language —
// the whole reason the route works is that `{group_name}` matches the segment
// rather than interpreting it — and the assertion is that the name arrives at
// the port as the one-character string, raw and percent-encoded alike. A façade
// that resolved `%2A` to something else, or that refused the raw form, would
// make the wildcard group unreadable through the one hop a Control Plane has.
func TestTheWildcardNameReachesTheCatalogAsItself(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{name: "the raw character", target: "/internal/alias-groups/*/versions/current"},
		{name: "the percent-encoded character", target: "/internal/alias-groups/%2A/versions/current"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog := &fakeCatalog{version: dataplane.GroupVersion{
				GroupName:      "*",
				Version:        1,
				GroupVersionID: "0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d",
			}}
			handler := testHandler(application.New("test", &fakeUsageFacts{}, catalog, &fakeProjection{}))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(stdhttp.MethodGet, tt.target, nil)
			req.Header.Set("Authorization", "Bearer "+testCredential)
			handler.ServeHTTP(rec, req)

			if rec.Code != stdhttp.StatusOK {
				t.Fatalf("GET %s status = %d (body %q), want %d — the wildcard is an ordinary group name to this operation", tt.target, rec.Code, rec.Body.String(), stdhttp.StatusOK)
			}
			if len(catalog.names) != 1 || catalog.names[0] != "*" {
				t.Errorf("the port was asked for %q, want [\"*\"]", catalog.names)
			}
			if want := `"group_name":"*"`; !strings.Contains(rec.Body.String(), want) {
				t.Errorf("body = %q, want it to name the group %s", rec.Body.String(), want)
			}
		})
	}
}

// TestASegmentedNameTravelsAsOneSegment pins the other half of the name's
// journey: the catalog's name grammar admits `/`, so a name like `team/model`
// can only arrive percent-encoded, and the decode is the caller's side of the
// bargain. The unencoded spelling is a different path — two segments where the
// route has one — and it stays a 404 rather than being re-joined by a handler
// that guessed.
func TestASegmentedNameTravelsAsOneSegment(t *testing.T) {
	t.Run("an encoded slash arrives decoded", func(t *testing.T) {
		catalog := &fakeCatalog{version: aGroupVersion}
		handler := testHandler(application.New("test", &fakeUsageFacts{}, catalog, &fakeProjection{}))

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, authedCatalogRead("team/model"))

		if rec.Code != stdhttp.StatusOK {
			t.Fatalf("status = %d (body %q), want %d", rec.Code, rec.Body.String(), stdhttp.StatusOK)
		}
		if len(catalog.names) != 1 || catalog.names[0] != "team/model" {
			t.Errorf("the port was asked for %q, want [\"team/model\"]", catalog.names)
		}
	})

	t.Run("an unencoded slash is a path this surface does not serve", func(t *testing.T) {
		catalog := &fakeCatalog{}
		handler := testHandler(application.New("test", &fakeUsageFacts{}, catalog, &fakeProjection{}))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(stdhttp.MethodGet, "/internal/alias-groups/team/model/versions/current", nil)
		req.Header.Set("Authorization", "Bearer "+testCredential)
		handler.ServeHTTP(rec, req)

		if rec.Code != stdhttp.StatusNotFound {
			t.Fatalf("status = %d (body %q), want %d", rec.Code, rec.Body.String(), stdhttp.StatusNotFound)
		}
		if catalog.called() {
			t.Errorf("a path that matches no route still reached the catalog: %q", catalog.names)
		}
	})
}

// TestACatalogFailureMapsToItsContractedResponse pins the seam's three
// failures to the statuses the operation declares. The 404 is the interesting
// one: it is an answer, not a failure, and a roll that reads one stops —
// which is why the mapping may not collapse it into the 502 an unreachable
// Data Plane also produces. The 502 stays 502, and a failure the port does not
// describe stays this process's own 500.
func TestACatalogFailureMapsToItsContractedResponse(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name:        "an unprovisioned group is a 404 the caller acts on",
			err:         fmt.Errorf("%w: the group %q has no version", dataplane.ErrGroupVersionNotFound, "frontier"),
			wantStatus:  stdhttp.StatusNotFound,
			wantCode:    "not_found",
			wantMessage: "no version of the requested alias group exists",
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
			catalog := &fakeCatalog{err: tt.err}
			handler := testHandler(application.New("test", &fakeUsageFacts{}, catalog, &fakeProjection{}))

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, authedCatalogRead("frontier"))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d (body %q), want %d", rec.Code, rec.Body.String(), tt.wantStatus)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"code":"`+tt.wantCode+`"`) {
				t.Errorf("body = %q, want code %q", body, tt.wantCode)
			}
			if tt.wantMessage != "" && !strings.Contains(body, tt.wantMessage) {
				t.Errorf("body = %q, want it to carry the declared message", body)
			}
			// Neither the cause's own wording nor a private address belongs on
			// the wire; the error path is where a URL most easily reaches a
			// caller, because everything about the failure is built around it.
			if strings.Contains(body, tt.err.Error()) {
				t.Errorf("body = %q, relays the cause's own text", body)
			}
			if strings.Contains(body, "://") {
				t.Errorf("body = %q, carries a URL", body)
			}
		})
	}
}

// TestTheCatalogReadRefusesAnUntrustedCaller is the fail-closed half stated on
// this operation as well: the refusal happens before the application, so a
// caller nobody vouched for never costs the Data Plane a call — and never
// learns whether a group exists.
func TestTheCatalogReadRefusesAnUntrustedCaller(t *testing.T) {
	catalog := &fakeCatalog{version: aGroupVersion}
	handler := testHandler(application.New("test", &fakeUsageFacts{}, catalog, &fakeProjection{}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(stdhttp.MethodGet, catalogReadTarget("frontier"), nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != stdhttp.StatusUnauthorized {
		t.Fatalf("status = %d (body %q), want %d", rec.Code, rec.Body.String(), stdhttp.StatusUnauthorized)
	}
	if catalog.called() {
		t.Errorf("the refusal still reached the Data Plane: %q", catalog.names)
	}
}

// TestTheVersionThisSurfaceWritesIsTheVersionTheContractDescribes pins the
// response type's JSON names to the schema the contract declares, read out of
// the document rather than restated — the same pin the page's response type is
// held to in contract_test.go, and for the same reason: a renamed field does
// not arrive as a wrong value but as an absent one, and nothing compiles a Go
// struct tag to the YAML that describes it.
func TestTheVersionThisSurfaceWritesIsTheVersionTheContractDescribes(t *testing.T) {
	document := scanContract(t, contractPath)

	path := "components.schemas.CurrentAliasGroupVersion.properties"
	declared := document.children[path]
	if len(declared) == 0 {
		t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", contractPath, path)
	}
	sorted := slices.Clone(declared)
	slices.Sort(sorted)

	if got := jsonFieldNames(t, currentGroupVersionResponse{}); !slices.Equal(got, sorted) {
		t.Errorf("this surface serializes %v and %s declares %v; a caller decoding this response reads the contract's names, and one that is only on one side arrives as an absent field rather than a wrong one", got, contractPath, sorted)
	}
}

// TestTheCatalogReadsDeclaredBoundsAreTheContracts pins the two numbers this
// surface keeps the private listener to — a version's minimum, and the name
// and id's minimum length — to the schema that declares them. Without the pin
// the refusals in the outbound adapter are unfalsifiable from inside this
// module: the constants could be relaxed and the façade would start passing a
// version the document it answers under does not describe.
func TestTheCatalogReadsDeclaredBoundsAreTheContracts(t *testing.T) {
	numbers := scanContract(t, contractPath).numbers

	tests := []struct {
		name string
		key  string
	}{
		{name: "the version's declared minimum", key: "components.schemas.CurrentAliasGroupVersion.properties.version.minimum"},
		{name: "the group name's declared minimum length", key: "components.schemas.CurrentAliasGroupVersion.properties.group_name.minLength"},
		{name: "the id's declared minimum length", key: "components.schemas.CurrentAliasGroupVersion.properties.group_version_id.minLength"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared, ok := numbers[tt.key]
			if !ok {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", contractPath, tt.key)
			}
			if declared != 1 {
				t.Errorf("%s says %s is %d; the refusals this surface keeps assume the contract's floor of 1, so the document and the code have drifted apart", contractPath, tt.key, declared)
			}
		})
	}
}
