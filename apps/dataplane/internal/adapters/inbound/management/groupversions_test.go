package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The stubs the catalog read's tests are written against.
//
// Only the two reads CurrentGroupVersion makes exist on stubVersions; the rest
// of every port is embedded unimplemented, because the read cannot reach it
// and a stub for a method the use case never calls would be a stub pretending
// to be tested. The reads themselves are the smallest honest model of the
// port's contract: HighestVersion answers a number — 0 among them, meaning
// the group has nothing — and ByGroupAndVersion answers the snapshot, or the
// miss sentinel a store that lost its row would answer.
type stubStore struct{ persistence.Store }

type stubBackends struct{ persistence.Backends }

type stubAliases struct{ persistence.ModelAliases }

type stubVersions struct {
	persistence.AliasGroupVersions
	highest    int
	snapshot   *catalog.AliasGroupVersion
	highestErr error
	reads      int
}

// HighestVersion implements persistence.AliasGroupVersions.
func (s *stubVersions) HighestVersion(context.Context, string) (int, error) {
	s.reads++
	if s.highestErr != nil {
		return 0, s.highestErr
	}
	return s.highest, nil
}

// ByGroupAndVersion implements persistence.AliasGroupVersions.
func (s *stubVersions) ByGroupAndVersion(_ context.Context, groupName string, version int) (*catalog.AliasGroupVersion, error) {
	s.reads++
	if s.snapshot == nil {
		return nil, fmt.Errorf("stub: group version %s@%d: %w", groupName, version, persistence.ErrNotFound)
	}
	clone := *s.snapshot
	clone.Members = append([]catalog.AliasID(nil), s.snapshot.Members...)
	return &clone, nil
}

// newStubCatalog wires the real catalog use cases over the stubs, the way the
// composition root wires them over PostgreSQL — the surface under test calls
// the use case, not the port, so the stub sits where the database sits.
func newStubCatalog(versions *stubVersions) *application.Catalog {
	return application.NewCatalog(stubStore{}, stubBackends{}, stubAliases{}, versions)
}

// serveCatalog drives one request through the whole handler chain with the
// catalog stubbed behind the application, exactly as serve does for the fact
// feed.
func serveCatalog(t *testing.T, versions *stubVersions, request *stdhttp.Request) *httptest.ResponseRecorder {
	t.Helper()
	handler := New(application.New("v0.1.0", &stubFacts{}, newStubCatalog(versions)), serviceCredential)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	return rec
}

// aStoredSnapshot is the group version the read's happy tests answer with:
// the second snapshot of a group whose membership already rolled once.
func aStoredSnapshot(t *testing.T) *catalog.AliasGroupVersion {
	t.Helper()
	snapshot, err := catalog.NewGroupVersion(
		catalog.GroupVersionID("0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"),
		"frontier", 2, []catalog.AliasID{"alias-1", "alias-2"},
		time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("building the stub snapshot: %v", err)
	}
	return snapshot
}

func TestTheCurrentGroupVersionIsTheThreeFields(t *testing.T) {
	versions := &stubVersions{highest: 2, snapshot: aStoredSnapshot(t)}

	rec := serveCatalog(t, versions, authed(t, "/internal/alias-groups/frontier/versions/current"))

	// The body is pinned as bytes rather than decoded and inspected, for the
	// same reason the fact feed's is: this is a contract between two
	// processes, and a decoder would pass a renamed field without a word.
	want := `{"group_name":"frontier","version":2,` +
		`"group_version_id":"0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
	if rec.Code != stdhttp.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, stdhttp.StatusOK)
	}
	if got := rec.Header().Get(RequestIDHeader); got == "" {
		t.Errorf("%s is missing; every response carries one", RequestIDHeader)
	}
}

// TestTheWildcardNameResolvesAsAnOrdinarySegment is the test the route was
// written to survive. The catalog's reserved wildcard name is the single
// character `*`, which is also ServeMux's own wildcard syntax — and a router
// that conflated the two would either refuse the segment or interpret it, so
// the reserved name is requested here through the whole chain, once raw and
// once percent-encoded, and the assertion is that the use case received the
// one-character name and answered it. The name is an ordinary path segment to
// this surface: the mux matches `{group_name}` against it and PathValue hands
// the handler the decoded `*`, which is exactly the string the catalog stores
// for the wildcard's singleton version.
func TestTheWildcardNameResolvesAsAnOrdinarySegment(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{name: "the raw character", target: "/internal/alias-groups/*/versions/current"},
		{name: "the percent-encoded character a careful caller sends", target: "/internal/alias-groups/%2A/versions/current"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			versions := &stubVersions{
				highest:  1,
				snapshot: mustWildcardSnapshot(t),
			}

			rec := serveCatalog(t, versions, authed(t, tt.target))

			if rec.Code != stdhttp.StatusOK {
				t.Fatalf("status = %d (body %s), want %d — the wildcard is a legal group name and this read must resolve it", rec.Code, rec.Body.String(), stdhttp.StatusOK)
			}
			want := `"group_name":"*"`
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("body = %s, want it to name the group %s", rec.Body.String(), want)
			}
		})
	}
}

// mustWildcardSnapshot is the wildcard's one version, built the way the use
// case builds it.
func mustWildcardSnapshot(t *testing.T) *catalog.AliasGroupVersion {
	t.Helper()
	snapshot, err := catalog.NewWildcardGroupVersion(
		catalog.GroupVersionID("0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"),
		time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("building the wildcard snapshot: %v", err)
	}
	return snapshot
}

// TestAGroupNameIsOneSegment pins the segment discipline the client half of
// this hop depends on. The catalog's name grammar admits `/`, so a name like
// `team/model` is legal — and it can only travel percent-encoded, because the
// unencoded slash is a second segment and the path it would build matches no
// route. The first row is the legal request and asserts the decode happened;
// the second is the same name unencoded, which must stay a 404 rather than be
// re-joined into a name by a handler that guessed.
func TestAGroupNameIsOneSegment(t *testing.T) {
	t.Run("an encoded slash arrives decoded", func(t *testing.T) {
		versions := &stubVersions{
			highest:  1,
			snapshot: mustNamedSnapshot(t, "team/model", catalog.GroupVersionID("0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d")),
		}

		rec := serveCatalog(t, versions, authed(t, "/internal/alias-groups/team%2Fmodel/versions/current"))

		if rec.Code != stdhttp.StatusOK {
			t.Fatalf("status = %d (body %s), want %d", rec.Code, rec.Body.String(), stdhttp.StatusOK)
		}
		if want := `"group_name":"team/model"`; !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body = %s, want it to name the group %s", rec.Body.String(), want)
		}
	})

	t.Run("a name whose decoding looks non-canonical still reaches the catalog", func(t *testing.T) {
		// The guard judges the escaped path, so an encoding that decodes to
		// something resembling a doubled slash stays one segment — and a
		// group the catalog legally holds under that name is readable, not
		// answered with the structural 404.
		versions := &stubVersions{
			highest:  1,
			snapshot: mustNamedSnapshot(t, "a//b", catalog.GroupVersionID("0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d")),
		}

		rec := serveCatalog(t, versions, authed(t, "/internal/alias-groups/a%2F%2Fb/versions/current"))

		if rec.Code != stdhttp.StatusOK {
			t.Fatalf("status = %d (body %s), want %d", rec.Code, rec.Body.String(), stdhttp.StatusOK)
		}
		if want := `"group_name":"a//b"`; !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body = %s, want it to name the group %s", rec.Body.String(), want)
		}
	})

	t.Run("an unencoded slash is a path this listener does not serve", func(t *testing.T) {
		versions := &stubVersions{}

		rec := serveCatalog(t, versions, authed(t, "/internal/alias-groups/team/model/versions/current"))

		if rec.Code != stdhttp.StatusNotFound {
			t.Fatalf("status = %d (body %s), want %d", rec.Code, rec.Body.String(), stdhttp.StatusNotFound)
		}
		if got := envelopeCode(t, rec); got != codeNotFound {
			t.Errorf("error code = %q, want %q", got, codeNotFound)
		}
		if versions.reads != 0 {
			t.Errorf("the port was read %d times; a path that matches no route must not reach the store", versions.reads)
		}
	})
}

// mustNamedSnapshot builds a named group's snapshot for the tests above.
func mustNamedSnapshot(t *testing.T, groupName string, id catalog.GroupVersionID) *catalog.AliasGroupVersion {
	t.Helper()
	snapshot, err := catalog.NewGroupVersion(id, groupName, 1, []catalog.AliasID{"alias-1"}, time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("building the %q snapshot: %v", groupName, err)
	}
	return snapshot
}

func TestAGroupWithNoVersionIsANotFoundAndNotAFailure(t *testing.T) {
	versions := &stubVersions{highest: 0} // the group has nothing on file

	rec := serveCatalog(t, versions, authed(t, "/internal/alias-groups/unprovisioned/versions/current"))

	if rec.Code != stdhttp.StatusNotFound {
		t.Fatalf("status = %d (body %s), want %d — an unprovisioned group is the answer the contract declares, not a defect", rec.Code, rec.Body.String(), stdhttp.StatusNotFound)
	}
	if got := envelopeCode(t, rec); got != codeNotFound {
		t.Errorf("error code = %q, want %q", got, codeNotFound)
	}
	if got := envelopeMessage(t, rec); got != "no version of the requested alias group exists" {
		t.Errorf("error message = %q, want the use case's own declared answer", got)
	}
}

// TestAStoreFailureIsNotANotFound is the half the 404 above would be cheapest
// to blur into. Both outcomes arrive at this handler as an error; only one of
// them means "this group does not exist", and the difference is a caller's
// next move — a Control Plane that answers a store outage with a 404 would
// refuse an entitlement for a group that exists, and pin nothing, and never
// say why.
func TestAStoreFailureIsNotANotFound(t *testing.T) {
	versions := &stubVersions{highestErr: errors.New("stub: the connection is gone")}

	rec := serveCatalog(t, versions, authed(t, "/internal/alias-groups/frontier/versions/current"))

	if rec.Code != stdhttp.StatusInternalServerError {
		t.Fatalf("status = %d (body %s), want %d", rec.Code, rec.Body.String(), stdhttp.StatusInternalServerError)
	}
	if got := envelopeCode(t, rec); got != codeInternal {
		t.Errorf("error code = %q, want %q", got, codeInternal)
	}
	if strings.Contains(rec.Body.String(), "the connection is gone") {
		t.Errorf("body = %s, leaks the store's own error text", rec.Body.String())
	}
}

func TestTheCatalogReadRefusesAnUnidentifiedCaller(t *testing.T) {
	versions := &stubVersions{highest: 2, snapshot: aStoredSnapshot(t)}

	request := httptest.NewRequest(stdhttp.MethodGet, "/internal/alias-groups/frontier/versions/current", nil)
	rec := serveCatalog(t, versions, request)

	if rec.Code != stdhttp.StatusUnauthorized {
		t.Fatalf("status = %d (body %s), want %d", rec.Code, rec.Body.String(), stdhttp.StatusUnauthorized)
	}
	if got := envelopeCode(t, rec); got != codeUnauthenticated {
		t.Errorf("error code = %q, want %q", got, codeUnauthenticated)
	}
	if versions.reads != 0 {
		t.Errorf("the port was read %d times; the credential check runs before any use case", versions.reads)
	}
}

// TestTheVersionThisListenerWritesIsTheVersionTheContractDescribes is the
// catalog read's counterpart to the page pin in protocol_test.go: the three
// keys this handler writes are the three the façade's document declares, read
// out of that document rather than restated here, so a field added to the
// response without a contract change — or renamed in one without the other —
// is red on the side that produces it.
//
// It lives in this file rather than protocol_test.go because it is a
// property of this handler's answer and not of the protocol's page: the
// catalog read is a single object, and the façade republishes it field for
// field.
func TestTheVersionThisListenerWritesIsTheVersionTheContractDescribes(t *testing.T) {
	versions := &stubVersions{highest: 2, snapshot: aStoredSnapshot(t)}

	rec := serveCatalog(t, versions, authed(t, "/internal/alias-groups/frontier/versions/current"))
	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d (body %s), want %d", rec.Code, rec.Body.String(), stdhttp.StatusOK)
	}

	document := scanContract(t, contractPath)

	var version map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &version); err != nil {
		t.Fatalf("decoding the version from %s: %v", rec.Body.String(), err)
	}
	wantKeys := document.children["components.schemas.CurrentAliasGroupVersion.properties"]
	if len(wantKeys) == 0 {
		t.Fatalf("%s declares no CurrentAliasGroupVersion properties; this pin proves nothing until the scan finds it", contractPath)
	}
	sorted := slices.Clone(wantKeys)
	slices.Sort(sorted)
	if got := sortedKeys(version); !slices.Equal(got, sorted) {
		t.Errorf("the version's keys are %v and %s declares %v — this is the object the façade republishes, and a key added or renamed here is one it would drop in silence", got, contractPath, sorted)
	}
}
