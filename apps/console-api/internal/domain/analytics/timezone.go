package analytics

import (
	"fmt"
	"time"
)

// The display zone. Every figure in this surface is STORED as a UTC instant
// and will be forever: a wall-clock reading in some operator's zone must never
// move a commercial boundary, so a day boundary is a UTC instant before it is
// a local one. The zone exists anyway, because being what a reader wants to
// see and being what is stored are different things, and a chart labelled in
// the wrong zone is wrong in a way no number defends.

// resolveLocation resolves an IANA zone name to a location. An empty name is
// UTC — the one default this surface takes, and the reason it is safe: a
// caller who named no zone asked for UTC, and asking for nothing is not the
// same as asking for something impossible.
//
// The other half of that sentence is the refusal. A zone that does not resolve
// is ErrInvalidTimezone rather than a silent fallback to UTC, because a
// fallback answers a question the caller did not ask with a zone they did not
// choose, and the difference between those is a number moving.
func resolveLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	// "Local" is the one name time.LoadLocation answers that is not a zone in
	// the database: it means the SERVER PROCESS's own zone, read from the
	// host. Accepting it would answer with bucket edges that depend on where
	// the binary happens to run and on the host's TZ, and would echo a
	// `range.timezone` that a caller cannot resolve back to the same zone —
	// the field documents an IANA name and this is not one. It is refused for
	// the same reason an unresolvable name is: the caller asked for a zone and
	// this plane cannot give them the one they asked for.
	if name == "Local" {
		return nil, fmt.Errorf("%w: %q is not an IANA zone name — it names the server process's own zone, which the caller cannot know and cannot reproduce from the answer", ErrInvalidTimezone, name)
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %q does not resolve to an IANA zone", ErrInvalidTimezone, name)
	}
	return location, nil
}

// canonicalTimezone is the name a query is stored under: the caller's own
// spelling when they gave one, and the resolved location's name when they did
// not.
//
// The response echoes the zone its buckets were cut on, so that a caller
// renders the instants it was handed rather than re-deriving them. Echoing the
// resolved name rather than the input matters for the empty case only — a
// caller who sent no `timezone` gets `UTC` back rather than an empty string,
// which would be a contract that promises a zone and delivers a blank.
func canonicalTimezone(name string, location *time.Location) string {
	if name != "" {
		return name
	}
	return location.String()
}
