package postgres

import "time"

// The nil helpers are the boundary's NULL vocabulary: a domain's zero values —
// an empty string, a zero count, an untimed instant — are meaningful there and
// meaningless here, and the insert sites pass through these so a caller reading
// the argument list sees `textOrNil(...)` where the row allows NULL instead of
// a bare value that only happens to be empty.

// textOrNil maps an empty string to SQL NULL and anything else through.
func textOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// intOrNil maps a zero count to SQL NULL. Counts that are optional ride this
// helper; counts that are required are validated by the domain long before a
// row is formed, so a zero arriving here is the optional kind.
func intOrNil(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// int64Value passes a required integer amount through as itself. Named for
// what it does rather than paired with intOrNil, because the amounts it
// carries — prices, holdings, settled sums — are never NULL in this schema,
// and a helper that could return nil for one would promise a row shape the
// constraints forbid.
func int64Value(v int64) any {
	return v
}

// timeOrNil maps an untimed instant to SQL NULL.
func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
