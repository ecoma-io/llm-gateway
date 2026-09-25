package management

import (
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
)

// groupVersionPath is the operation this surface serves for the Control
// Plane's commerce roll, and it is stated once here because three places
// depend on the spelling: this route table, the façade that calls it, and the
// contract the façade publishes — the two tests that hold the last two against
// this file's route inventory exist so a rename is a build failure on both
// sides of the seam rather than a hop that stopped resolving.
//
// The path is a template and the middle segment is its variable: ServeMux
// matches `{group_name}` against exactly one path segment and hands the
// handler the decoded value, so a group's name travels as one segment,
// percent-encoded by the caller where the name's own characters would end the
// segment early.
const groupVersionPath = "/internal/alias-groups/{group_name}/versions/current"

// groupNamePathParameter names the template variable the route carries, so
// the route's pattern and the read below cannot drift apart over which
// segment holds the name.
const groupNamePathParameter = "group_name"

// currentGroupVersionResponse is the whole answer: the group's name, its
// current version's number, and that version's immutable id — the three fields
// the private protocol carries and the façade re-publishes field for field.
// Like every wire type in this package it is a transport type and not the
// domain's, because the domain's snapshot carries the member set and the
// creation instant, and neither crosses: which aliases a version contains is
// admission's question, asked inside this process, and the Control Plane's
// entitlement stores the id and nothing else (ADR 0006 §7) — a fourth field
// here would be an invitation to depend on more than the id, which the
// catalog's immutability contract covers and nothing else does.
//
// The id is carried as its string form: the domain types it so an alias id can
// never be passed where a group-version id is expected, but on the wire both
// are uuids and the distinction that matters stops at this process's edge.
type currentGroupVersionResponse struct {
	GroupName      string `json:"group_name"`
	Version        int    `json:"version"`
	GroupVersionID string `json:"group_version_id"`
}

// currentGroupVersion answers the catalog read.
//
// It does no parsing and applies no policy — there is nothing to parse (the
// operation declares no query parameters, so anything after the path is not
// this operation's business) and no policy to apply (which version is current
// is the catalog's own rule, highest wins, and the use case below owns it). A
// group the catalog holds nothing for arrives as the application's NotFound
// and leaves as a 404 through the same failureFor mapping every other
// status on this surface travels; an infrastructure failure arrives wrapped
// and leaves a 500, and the difference between the two is the one judgement
// this handler is responsible for keeping intact.
func currentGroupVersion(app *application.App) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		snapshot, err := app.CurrentGroupVersion(r.Context(), r.PathValue(groupNamePathParameter))
		if err != nil {
			writeFailure(w, r, failureFor(err))
			return
		}
		writeJSON(w, stdhttp.StatusOK, currentGroupVersionResponse{
			GroupName:      snapshot.GroupName,
			Version:        snapshot.Version,
			GroupVersionID: string(snapshot.ID),
		})
	}
}
