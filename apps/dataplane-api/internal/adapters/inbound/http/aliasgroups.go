package http

import (
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/application"
)

// GET /internal/alias-groups/{group_name}/versions/current: the catalog version
// a Control Plane commerce roll pins, served from here and answered from there.
//
// The shape is the usage feed's, and the file exists separately from it for the
// reason the operation itself is separate: this read's answer is one immutable
// fact — which version of a group is current — and the whole of the judgement
// this surface makes about it is which of its fields travel. Three do, and they
// are the three the contract's CurrentAliasGroupVersion declares; the version's
// membership does not, because which aliases a snapshot contains is the
// question the runtime answers when it admits a request, and an entitlement
// resolves what it grants from the id alone.
//
// The caller is verified before any of that runs, by the same wrapper in
// serviceauth.go applied at the route table: a request without a trusted
// credential never reaches the application, and the catalog is never touched.

const (
	// aliasGroupsPath is the operation's path, spelled exactly as
	// api/openapi/dataplane.yaml declares it. contract_test.go compares the
	// route table against that document and the outbound adapter pins the
	// private hop's copy of the same string, so this constant is the one place
	// the façade's spelling lives in code.
	aliasGroupsPath = "/internal/alias-groups/{group_name}/versions/current"

	// groupNamePathParameter names the path variable the route carries, so the
	// route's pattern and the read below cannot drift apart over which segment
	// holds the group's name.
	groupNamePathParameter = "group_name"
)

// currentGroupVersionHandler serves the catalog read.
//
// There is no parameter work here on purpose, and the absence is the
// operation's own: it declares no query parameters and no 400, because the one
// value it takes arrives in the path and is only ever a name — a name the
// catalog does not hold is the application's not-found and so a 404, not an
// invalid request. A handler that invented a grammar for that name here would
// be a second definition of the catalog's, checked one hop earlier than the
// authority that owns it; the query string is left alone for the same reason
// the private listener behind this façade leaves it alone, so the two hops
// agree about every input rather than about all but one.
func currentGroupVersionHandler(app *application.App) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		version, err := app.CurrentGroupVersion(r.Context(), r.PathValue(groupNamePathParameter))
		if err != nil {
			writeError(w, r, err)
			return
		}

		writeJSON(w, stdhttp.StatusOK, currentGroupVersionResponse{
			GroupName:      version.GroupName,
			Version:        version.Version,
			GroupVersionID: version.GroupVersionID,
		})
	}
}

// currentGroupVersionResponse is the wire shape this surface writes, mirroring
// CurrentAliasGroupVersion in api/openapi/dataplane.yaml field for field. The
// port speaks in values; serialization stays on this side of the boundary, as
// it does for the page and the error envelope.
//
// The id is the field a caller keeps, and the other two are the read's
// context — the name it asked for, and a number that describes the group as it
// stands now rather than the snapshot forever. Keeping the response to exactly
// the contract's three fields is what stops this surface implying that more of
// the version travels.
type currentGroupVersionResponse struct {
	GroupName      string `json:"group_name"`
	Version        int    `json:"version"`
	GroupVersionID string `json:"group_version_id"`
}
