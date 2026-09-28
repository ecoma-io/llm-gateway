package http

import (
	"strings"
	"testing"
)

// mustWireTime is the assertion every read DTO's timestamps pass through, and
// it has exactly two legal outcomes: an absent instant passes as the empty
// string, and a present one is a date-time or the process panics at the
// boundary. Both directions are asserted here because the rule that changed is
// the ABSENCE direction — the empty string used to panic, which made a
// legitimately absent `revoked_at` unrepresentable — and a relaxation is
// exactly the kind of change that later gets relaxed further without anyone
// deciding to.
//
// The panic is asserted by recovering rather than by spawning a process,
// because a panic recovered here proves the value was REFUSED at this
// boundary; a sub-process test would prove it too and would also prove
// nothing about which function refused it.
func TestMustWireTimePassesAbsenceAndRefusesAMalformedInstant(t *testing.T) {
	// Absence is legal, and it is the empty string precisely so an
	// `omitempty` field drops itself. A newly minted key has no updated_at and
	// no revoked_at, and both are absent by design rather than by omission.
	if got := mustWireTime(""); got != "" {
		t.Errorf("mustWireTime(%q) = %q, want the empty string: absence must survive the validator so an omitempty field can drop itself", "", got)
	}

	// A present instant is returned unchanged, not reformatted. The value the
	// application rendered is the value that goes on the wire, and a
	// reformatting here would make the stored precision and the shown precision
	// two different facts.
	const present = "2026-09-28T12:00:00Z"
	if got := mustWireTime(present); got != present {
		t.Errorf("mustWireTime(%q) = %q, want it unchanged", present, got)
	}

	// A present-but-unparseable instant is the case the panic exists for. A
	// client would read it as a wrong type, and nothing downstream can render
	// it — so it is refused where the stack names the conversion.
	for _, malformed := range []string{
		"2026-09-28",             // a date, not a date-time
		"28/09/2026 12:00",       // a locale's idea of a timestamp
		"1759060800",             // a Unix epoch
		"2026-09-28T12:00:00",    // RFC 3339 missing its offset
		"not a timestamp at all", // the obvious one
	} {
		t.Run(malformed, func(t *testing.T) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatalf("mustWireTime(%q) returned instead of panicking: a present instant that is not RFC 3339 must be refused at the boundary, not written to the wire", malformed)
				}
				// The message has to name the offending value, because the
				// caller that has to fix it is the one that supplied it and a
				// panic saying only "bad timestamp" sends them looking through
				// every conversion in the package.
				if !strings.Contains(toString(recovered), malformed) {
					t.Errorf("the panic does not name the value %q: %v", malformed, recovered)
				}
			}()
			mustWireTime(malformed)
		})
	}
}

// toString renders a recovered value without importing fmt into a file whose
// only job is to assert two sentences about a validator.
func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if err, ok := v.(error); ok {
		return err.Error()
	}
	return "a non-string panic value"
}
