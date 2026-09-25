package dataplane

import (
	"context"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// settledVersionBody is a well-formed catalog answer: the third version of a
// group, under the id a commerce roll would pin. The id is a real v7-shaped
// uuid rather than "id1" so that a future grammar check — if one is ever
// argued in — has something true to accept and this test has something to
// notice it by.
const settledVersionBody = `{"group_name":"frontier","version":3,"group_version_id":"0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"}`

// TestReadCurrentGroupVersionEscapesTheNameIntoOneSegment is the request half
// of the catalog read, asserted on what the listener received: the name is
// substituted into the operation's own path, escaped as a path *segment*, and
// nothing else is invented — no query, because the operation declares none, and
// no header beyond the credential.
//
// The escaping rows are the point of the test rather than decoration. The
// catalog's name grammar admits `/`, so a name like `team/model` can only reach
// the listener as one segment, which means `%2F` on the wire; and the wildcard
// group's name is literally `*`, which url.PathEscape sends as `%2A` even
// though the character would survive a path unescaped — the same escape either
// way is what makes the two names symmetric. Both spellings are checked on
// EscapedPath, the form the wire carried, because the decoded path cannot
// distinguish `%2A` from a hop that decoded the name early and re-sent it.
func TestReadCurrentGroupVersionEscapesTheNameIntoOneSegment(t *testing.T) {
	tests := []struct {
		name        string
		group       string
		wantDecoded string
		wantOnWire  string
	}{
		{
			name:        "an ordinary name needs no escaping and gets none",
			group:       "frontier",
			wantDecoded: "/internal/alias-groups/frontier/versions/current",
			wantOnWire:  "/internal/alias-groups/frontier/versions/current",
		},
		{
			name:        "the wildcard's character is escaped as any other",
			group:       "*",
			wantDecoded: "/internal/alias-groups/*/versions/current",
			wantOnWire:  "/internal/alias-groups/%2A/versions/current",
		},
		{
			name:        "a slash inside a name stays one segment",
			group:       "team/model",
			wantDecoded: "/internal/alias-groups/team/model/versions/current",
			wantOnWire:  "/internal/alias-groups/team%2Fmodel/versions/current",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := &upstream{body: settledVersionBody}
			client := up.server(t)

			if _, err := client.CurrentGroupVersion(context.Background(), tt.group); err != nil {
				t.Fatalf("CurrentGroupVersion() error = %v", err)
			}
			calls := up.recorded()
			if len(calls) != 1 {
				t.Fatalf("the listener received %d calls, want 1", len(calls))
			}
			call := calls[0]
			if call.path != tt.wantDecoded {
				t.Errorf("the listener decoded the path to %q, want %q", call.path, tt.wantDecoded)
			}
			if call.rawPath != tt.wantOnWire {
				t.Errorf("the wire carried %q, want %q", call.rawPath, tt.wantOnWire)
			}
			if call.rawQuery != "" {
				t.Errorf("the request carried a query %q; the operation declares none", call.rawQuery)
			}
			if want := "Bearer " + testCredential; call.authorization != want {
				t.Errorf("the listener received Authorization = %q, want %q", call.authorization, want)
			}
		})
	}
}

// TestReadCurrentGroupVersionReadsTheDataPlanesVersion pins the success mapping
// field by field. The three values are the whole of what crosses, and the
// version number in particular must arrive as a number and not as a string a
// later hop quietly reformats.
func TestReadCurrentGroupVersionReadsTheDataPlanesVersion(t *testing.T) {
	up := &upstream{body: settledVersionBody}
	client := up.server(t)

	version, err := client.CurrentGroupVersion(context.Background(), "frontier")
	if err != nil {
		t.Fatalf("CurrentGroupVersion() error = %v", err)
	}
	if version.GroupName != "frontier" {
		t.Errorf("GroupName = %q, want %q", version.GroupName, "frontier")
	}
	if version.Version != 3 {
		t.Errorf("Version = %d, want %d", version.Version, 3)
	}
	if want := "0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"; version.GroupVersionID != want {
		t.Errorf("GroupVersionID = %q, want %q", version.GroupVersionID, want)
	}
}

// TestReadCurrentGroupVersionClassifiesEveryFailureTheSeamCanProduce is the
// catalog read's mapping table, and its one row that is not the feed's is the
// first one: a 404 from the listener is the catalog answering *no*, and it is
// carried as its own sentinel rather than folded into unavailable — a roll that
// learns the group is unprovisioned stops, while one that could not reach the
// catalog retries, and a façade that answered both with 502 would tell every
// roll the catalog was broken.
//
// Everything else is the feed's rule restated on this operation: an
// unclassifiable status, an unreadable body and a listener that is not there
// are all one condition, because in each the answer is unknown. The name passed
// in is the wildcard on purpose: it is the row whose cause carries the group's
// name, so the leak checks below run against the request that name came from.
func TestReadCurrentGroupVersionClassifiesEveryFailureTheSeamCanProduce(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		closed  bool
		wantErr error
	}{
		{
			name:    "a group the catalog holds nothing for is an answer, not a failure",
			status:  stdhttp.StatusNotFound,
			body:    `{"error":{"code":"not_found","message":"..."}}`,
			wantErr: dataplane.ErrGroupVersionNotFound,
		},
		{
			name:    "the listener refusing this process's own credential is a deployment fault",
			status:  stdhttp.StatusUnauthorized,
			body:    `{"error":{"code":"unauthenticated","message":"..."}}`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "an implementation failure in the listener is not this process's failure",
			status:  stdhttp.StatusInternalServerError,
			body:    `{"error":{"code":"internal","message":"..."}}`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "a success body this facade cannot read is a version it will not guess at",
			status:  stdhttp.StatusOK,
			body:    `{"group_name":`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "a listener that is not there is the data plane being unavailable",
			closed:  true,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			if tt.closed {
				server.Close()
			} else {
				t.Cleanup(server.Close)
			}

			client := New(server.Client(), server.URL, testCredential)
			_, err := client.CurrentGroupVersion(context.Background(), "*")

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CurrentGroupVersion() error = %v, want it to wrap %v", err, tt.wantErr)
			}
			// The same two values that must not travel with the feed's failures:
			// the address is the deployment's private topology and the credential
			// is a secret, and the error text is what a future caller might choose
			// to log.
			if strings.Contains(err.Error(), testCredential) {
				t.Errorf("the error text carries the credential: %q", err.Error())
			}
			if strings.Contains(err.Error(), server.URL) {
				t.Errorf("the error text carries the data plane's address: %q", err.Error())
			}
		})
	}
}

// TestReadCurrentGroupVersionNamesTheGroupItWasAskedFor is the one field the
// not-found cause is allowed to carry, pinned as a positive: the sentinel's
// text names the group an operator asked about, so a failed roll can be traced
// to its group without re-running it.
func TestReadCurrentGroupVersionNamesTheGroupItWasAskedFor(t *testing.T) {
	server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		w.WriteHeader(stdhttp.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"..."}}`))
	}))
	t.Cleanup(server.Close)

	_, err := New(server.Client(), server.URL, testCredential).CurrentGroupVersion(context.Background(), "frontier")
	if !errors.Is(err, dataplane.ErrGroupVersionNotFound) {
		t.Fatalf("CurrentGroupVersion() error = %v, want it to wrap %v", err, dataplane.ErrGroupVersionNotFound)
	}
	if !strings.Contains(err.Error(), "frontier") {
		t.Errorf("the error %q does not name the group it was asked for", err.Error())
	}
}

// TestReadCurrentGroupVersionRefusesABodyMissingRequiredFields is the
// fail-closed table, one row per way a body can fall short of the three fields
// the contract marks required, including the declared bounds: an absent field,
// a null one, an empty name, and a version below the number the contract calls
// a version.
//
// The refusals are ErrUpstreamUnavailable, the same classification as a body
// the facade cannot read at all, because they are the same condition from a
// caller's point of view. What makes these rows matter more than the page's is
// what a refusal prevents: a version answered as 0 by a missing field would
// become the number a scope is compared against the catalog's current, and an
// id answered as "" would be the value an entitlement pins — both invented
// answers to a question the catalog did answer, just not well enough to store.
func TestReadCurrentGroupVersionRefusesABodyMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "a version with no group_name field at all",
			body: `{"version":3,"group_version_id":"0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"}`,
		},
		{
			name: "a version whose group_name field is null",
			body: `{"group_name":null,"version":3,"group_version_id":"0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"}`,
		},
		{
			name: "a version whose group_name is empty",
			body: `{"group_name":"","version":3,"group_version_id":"0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"}`,
		},
		{
			name: "a version with no version field at all",
			body: `{"group_name":"frontier","group_version_id":"0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"}`,
		},
		{
			name: "a version whose version field is null",
			body: `{"group_name":"frontier","version":null,"group_version_id":"0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"}`,
		},
		{
			name: "a version below the number the contract calls a version",
			body: `{"group_name":"frontier","version":0,"group_version_id":"0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"}`,
		},
		{
			name: "a version that is an empty object",
			body: `{}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
				w.WriteHeader(stdhttp.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(server.Close)

			client := New(server.Client(), server.URL, testCredential)
			version, err := client.CurrentGroupVersion(context.Background(), "frontier")
			if !errors.Is(err, dataplane.ErrUpstreamUnavailable) {
				t.Fatalf("CurrentGroupVersion() error = %v, want it to wrap %v", err, dataplane.ErrUpstreamUnavailable)
			}
			if (version != dataplane.GroupVersion{}) {
				t.Errorf("CurrentGroupVersion() returned %+v beside an error, want the zero GroupVersion — a caller that ignored the error must not find a scope in it", version)
			}
			// The refusal is made of the response and not of the deployment, so it
			// must not smuggle back the address or the credential either.
			if strings.Contains(err.Error(), testCredential) {
				t.Errorf("the error text carries the credential: %q", err.Error())
			}
			if strings.Contains(err.Error(), server.URL) {
				t.Errorf("the error text carries the data plane's address: %q", err.Error())
			}
		})
	}
}

// TestAVersionWithoutAnIDIsNotAScope covers the one field the table above
// leaves out on purpose: the id is refused for absence and for emptiness in
// their own rows, because it is the field an entitlement stores — the two
// spellings of nothing reach the same refusal from two different arms of the
// adapter's switch, and a table that merged them would pass with one arm
// deleted.
func TestAVersionWithoutAnIDIsNotAScope(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "a version with no group_version_id field at all",
			body: `{"group_name":"frontier","version":3}`,
		},
		{
			name: "a version whose group_version_id field is null",
			body: `{"group_name":"frontier","version":3,"group_version_id":null}`,
		},
		{
			name: "a version whose group_version_id is empty",
			body: `{"group_name":"frontier","version":3,"group_version_id":""}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := &upstream{body: tt.body}
			client := up.server(t)

			version, err := client.CurrentGroupVersion(context.Background(), "frontier")
			if !errors.Is(err, dataplane.ErrUpstreamUnavailable) {
				t.Fatalf("CurrentGroupVersion() error = %v, want it to wrap %v", err, dataplane.ErrUpstreamUnavailable)
			}
			if version.GroupVersionID != "" {
				t.Errorf("GroupVersionID = %q beside an error, want empty — an entitlement must not find a scope to pin", version.GroupVersionID)
			}
		})
	}
}

// TestAVersionMayCarryFieldsTheContractDoesNotName is the decoder's forward
// compatibility, restated on the smaller body: the data plane may add a field
// to this object before this module ships, and an unfamiliar key is ignored,
// never refused. A facade that refused one would be a version lock on the plane
// it fronts — the same reason the page reader leaves unknown keys alone.
func TestAVersionMayCarryFieldsTheContractDoesNotName(t *testing.T) {
	body := `{"group_name":"frontier","version":3,"group_version_id":"0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d","issued_at":"2026-09-23T10:00:00Z"}`

	up := &upstream{body: body}
	client := up.server(t)

	version, err := client.CurrentGroupVersion(context.Background(), "frontier")
	if err != nil {
		t.Fatalf("CurrentGroupVersion() error = %v, want nil — unknown fields are tolerated", err)
	}
	if got, want := version.GroupVersionID, "0197c1a2-7b31-7cc1-9e4e-6f5d2a1b3c4d"; got != want {
		t.Errorf("GroupVersionID = %q, want %q", got, want)
	}
}

// TestAVersionWhoseIDIsNotAUuidStillCrosses is the boundary the refusal tables
// above must not erode from the other side: `format: uuid` is an annotation the
// producer declares, and this consumer does not re-judge it. The facade carries
// the id and never compares, resolves or stores it — the entitlement below the
// port does that — so a grammar check here would be a second definition of a
// format this process has no use for, and one that turns a future id scheme
// into this facade's outage.
func TestAVersionWhoseIDIsNotAUuidStillCrosses(t *testing.T) {
	up := &upstream{body: `{"group_name":"frontier","version":3,"group_version_id":"not-a-uuid"}`}
	client := up.server(t)

	version, err := client.CurrentGroupVersion(context.Background(), "frontier")
	if err != nil {
		t.Fatalf("CurrentGroupVersion() error = %v, want nil — the id's grammar is the producer's declaration, not this consumer's check", err)
	}
	if version.GroupVersionID != "not-a-uuid" {
		t.Errorf("GroupVersionID = %q, want it to cross byte for byte", version.GroupVersionID)
	}
}
