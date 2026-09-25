package application

import (
	"context"
	"errors"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// CurrentGroupVersion returns the Data Plane alias group's current version and
// nothing else. It is the use case behind
// GET /internal/alias-groups/{group_name}/versions/current.
//
// Like UsageEvents it is a pass-through, and for the same reasons: which
// version of a group is current is the catalog's own rule — its highest, under
// a discipline that opens a new version rather than editing one — so this
// process re-deriving it would be a second definition of it, and the version
// is answered from the Data Plane or not at all. What it does not carry is
// decided as deliberately as what it does: the version's membership stays on
// the Data Plane, because which aliases a snapshot contains is the question
// the runtime answers when it admits a request, and an entitlement resolves
// what it grants from the id alone. A façade that fetched and forwarded the
// member list would be widening the cross-plane answer without anyone asking
// it to.
//
// The group's name crosses untouched, `*` included: the wildcard is an
// ordinary group name to the catalog, and special-casing it here would be a
// façade deciding catalog policy. Likewise nothing about the name is
// validated — a name the catalog does not hold is the port's
// ErrGroupVersionNotFound and so a 404, not a 400, because the group-name
// grammar is the catalog's and copying it would be a second definition of it.
func (app *App) CurrentGroupVersion(ctx context.Context, groupName string) (dataplane.GroupVersion, error) {
	version, err := app.catalog.CurrentGroupVersion(ctx, groupName)
	if err == nil {
		return version, nil
	}

	// The port's failures become this package's codes for the reason
	// UsageEvents gives: the transport maps codes and has no business knowing
	// which outbound adapter is behind the port. The not-found is the one
	// answer a caller can act on, so it keeps a message that says what the
	// condition is rather than that something failed — a commerce roll that
	// reads it should stop, not retry. Both causes are kept for errors.Is and
	// neither is serialized.
	if errors.Is(err, dataplane.ErrGroupVersionNotFound) {
		return dataplane.GroupVersion{}, &Error{
			Code:    CodeNotFound,
			Message: "no version of the requested alias group exists",
			cause:   err,
		}
	}
	if errors.Is(err, dataplane.ErrUpstreamUnavailable) {
		return dataplane.GroupVersion{}, &Error{
			Code:    CodeUpstreamUnavailable,
			Message: "the data plane is unavailable",
			cause:   err,
		}
	}

	// An adapter that failed in a way the port does not describe is this
	// process's own defect, classified as such rather than reported as the
	// Data Plane's problem. The cause stays server-side: Internal's message is
	// replaced with a fixed one at the wire.
	return dataplane.GroupVersion{}, Internal(err)
}
