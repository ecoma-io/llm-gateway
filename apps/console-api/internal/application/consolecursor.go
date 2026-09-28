package application

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The opaque cursor every list on this surface pages with, and the filter
// fingerprint that rides inside it.
//
// A cursor is a keyset position, and the keyset is the only thing the
// persistence port ever sees: a uuid, an allocated sequence, an integer
// identity — never a number the caller could decrement, and never an OFFSET
// (ADR 0012 §4: OFFSET re-reads and re-discards every row already passed, and
// a pass that pages by OFFSET while rows land concurrently can both skip a row
// and read one twice). Only the sort key crosses the port boundary; every
// byte of the position, its collection, and its filters stays here.
//
// The wire form is base64url of a versioned JSON body, MAC'd with a key this
// process holds. Three properties follow, and each is a decision rather than
// an encoding choice:
//
//   - Opaque. The contract says the value "is not safe to interpret, and
//     neither the console nor a generated client may decode, compare, order or
//     synthesise one". Nothing in this package reads a caller-supplied cursor
//     as anything but a signed, version-matched position, so a client that
//     decodes one anyway learns nothing it could not have guessed.
//   - Filter-bound. The fingerprint covers the collection and every filter
//     the request made. A cursor minted under `state=active` and replayed
//     under `state=invited` is refused (400 invalid_request) rather than
//     served against the new filter, because a page reached through a
//     mismatched cursor is a page whose rows belong to a question the caller
//     is no longer asking — ADR 0012 §4 names this case exactly.
//   - Refused, never honoured loosely. A cursor this build cannot place —
//     malformed, truncated, forged, or minted by a process that has since
//     restarted with a different key — is an error. Silently resuming from
//     the newest page instead would skip rows the caller has not read and
//     present the result as continuity, which is the one outcome a page's
//     `has_more` cannot express.
//
// The key is minted per process rather than configured. A cursor that stops
// being placeable across a deploy is a 400 the client answers by re-reading
// from the beginning, and the alternative — a long-lived key in configuration
// or the environment — is a secret this read surface does not otherwise need.
// Nothing durable is keyed by it.
const cursorVersion = 1

// cursorSecret is the MAC key for this process's cursors: 256 bits from the
// system CSPRNG, drawn once at initialisation. A panic rather than a fallback,
// because a weak key would make a forged position indistinguishable from a
// real one.
var cursorSecret = newCursorSecret()

func newCursorSecret() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(fmt.Sprintf("application: read random bytes for the cursor key: %v", err))
	}
	return key
}

// cursorBody is what a cursor carries: the collection it positions within, the
// exclusive lower bound on that collection's keyset, the filters the position
// was earned under, and the format version.
type cursorBody struct {
	V       int               `json:"v"`
	List    string            `json:"l"`
	After   string            `json:"a,omitempty"`
	Filters map[string]string `json:"f,omitempty"`
}

// EncodeCursor mints the cursor naming the position after which the next page
// begins, for the collection called list under exactly the filters the request
// made.
//
// after is the last row's keyset as the persistence port spells it, already
// rendered to its wire form — a uuid string, a decimal sequence, a decimal
// identity. It is a string rather than a typed key because the ten lists key
// on three different sorts and one cursor type serving all of them is worth
// more than three wrappers; only this package reads it back.
//
// The result is never empty. The contract requires `next_cursor` on every page
// including the last, because a page carrying none is not a page a client can
// hold: a client asked to continue from an empty position would resume from
// the beginning of retained history on every page, re-reading the same rows
// forever behind a position that never moves. The last page's cursor resumes
// the walk and returns an empty page, which is the honest answer.
func EncodeCursor(list, after string, filters map[string]string) string {
	encoded, err := json.Marshal(cursorBody{
		V:       cursorVersion,
		List:    list,
		After:   after,
		Filters: filters,
	})
	if err != nil {
		// The body is a struct of strings and a map of them, which
		// json.Marshal cannot fail on. A panic here is a programmer error in a
		// type that was just edited, not a runtime condition to report.
		panic(fmt.Sprintf("application: encode cursor for %s: %v", list, err))
	}
	payload := base64.RawURLEncoding.EncodeToString(encoded)
	return payload + "." + base64.RawURLEncoding.EncodeToString(signCursorPayload(payload))
}

// DecodeCursor is the whole of a caller's cursor: what it verifies, what it
// refuses, and the exclusive lower bound it names.
//
// A nil error means the caller may page from After; the empty string is the
// beginning of the collection, which is not a cursor that matches nothing. The
// lists' own statements rely on that: every keyset here is ordered from a nil
// uuid, a zero sequence, or a zero identity, and the `> $n` test against that
// is the first row.
//
// Every refusal is an *Error with CodeInvalidRequest, so the transport renders
// the contract's 400 invalid_request without knowing anything about cursors. A
// caller that mints one and replays it under different filters gets the same
// answer as one that sends a forged string, and for the same reason: the value
// names a position this request is not entitled to resume.
func DecodeCursor(list, value string, filters map[string]string) (string, error) {
	if value == "" {
		return "", nil
	}
	payload, signature, ok := strings.Cut(value, ".")
	if !ok {
		return "", invalidRequest("the after parameter is not a cursor this surface minted")
	}
	// Constant-time, and MAC compared before the body is even decoded: a
	// forged cursor must not be distinguishable from a real one by how long
	// the answer took, in the same way and for the same reason the sign-in
	// comparison is.
	if !hmac.Equal([]byte(signature), []byte(base64.RawURLEncoding.EncodeToString(signCursorPayload(payload)))) {
		return "", invalidRequest("the after parameter is not a cursor this surface minted")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", invalidRequest("the after parameter is not a cursor this surface minted")
	}
	var body cursorBody
	if err := json.Unmarshal(decoded, &body); err != nil {
		return "", invalidRequest("the after parameter is not a cursor this surface minted")
	}
	if body.V != cursorVersion {
		// A cursor minted by a build that encoded a position this one cannot
		// read. Refused rather than reinterpreted — reading a body of another
		// version as this one would page from a position nobody chose.
		return "", invalidRequest("the after cursor was minted in a format this surface does not read")
	}
	if body.List != list {
		// A cursor names one collection's order and no other. Replaying a
		// users cursor against /api-keys would otherwise page one collection
		// by another's keyset, and a uuid keyset is total enough to make that
		// succeed and return the wrong rows.
		return "", invalidRequest("the after cursor names a different collection")
	}
	if !sameFilters(body.Filters, filters) {
		return "", invalidRequest("the after cursor was minted under a different set of filters")
	}
	return body.After, nil
}

// signCursorPayload is the MAC over one cursor's encoded body. The collection
// and the keyset are both inside the signed body, so neither a swapped
// collection nor an edited position is reachable without the key.
func signCursorPayload(payload string) []byte {
	mac := hmac.New(sha256.New, cursorSecret)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// sameFilters compares the filters a cursor was minted under with the ones this
// request makes. A filter this request makes and the cursor did not, or the
// reverse, is a mismatch: both change the row set, and a keyset's meaning is
// defined relative to the rows it orders.
func sameFilters(minted, current map[string]string) bool {
	if len(minted) != len(current) {
		return false
	}
	for key, value := range minted {
		if current[key] != value {
			return false
		}
	}
	return true
}

// Fingerprint builds the filter map a cursor is bound to.
//
// list is the operation's own name and filters are the request's, with any
// omitted one dropped: an absent filter and an empty-valued one are the same
// request, and the two must not mint two cursors for one position. The map is
// what makes the collection and the filters travel inside the cursor instead
// of beside it — a caller cannot keep one and send it to another page.
func Fingerprint(list string, filters map[string]string) map[string]string {
	fingerprint := make(map[string]string, len(filters)+1)
	fingerprint[filterCollectionKey] = list
	for key, value := range filters {
		if value == "" {
			continue
		}
		fingerprint[key] = value
	}
	return fingerprint
}

// filterCollectionKey is the reserved entry under which a request carries its
// collection's own name inside the fingerprint. It is not a query parameter: it
// never reaches the persistence port, and every caller builds it from a
// constant in this package.
const filterCollectionKey = "\x00collection"

// ResolvePage is the one place a list call's paging ask is decided: the
// caller's cursor, the caller's page size, and the filters this request made.
//
// It is here rather than in each of the ten use cases because the three
// refusals are the same refusals on every list, and a list that resolved its
// own cursor differently from its nine siblings would be a list the next
// reader has to re-verify: a limit outside the contract's bounds, a cursor
// this surface cannot place, and a cursor carried under filters the request no
// longer makes. All three are invalid_request, all three are answered with the
// same envelope, and none of them is clamped — a page the caller did not ask
// for is one it cannot tell apart from a page the collection capped itself.
//
// limit is the caller's page size as the transport parsed it, and zero for
// "not supplied". Zero is the contract's documented default rather than a
// demand for no rows, and it is resolved against the port's own bound
// constants so this package and the SQL layer can never disagree about where
// the bounds are.
func ResolvePage(list, cursor string, limit int, filters map[string]string) (after string, resolved int, err error) {
	if limit != 0 && (limit < persistence.MinPageLimit || limit > persistence.MaxPageLimit) {
		return "", 0, invalidRequest(fmt.Sprintf("the limit must be between %d and %d",
			persistence.MinPageLimit, persistence.MaxPageLimit))
	}
	after, err = DecodeCursor(list, cursor, filters)
	if err != nil {
		return "", 0, err
	}
	if limit == 0 {
		return after, persistence.DefaultPageLimit, nil
	}
	return after, limit, nil
}

// PageOf turns a port's page of limit+1 rows into the three facts every list
// on this surface answers with, and it is the only place those three are
// derived.
//
// The extra row the adapters ask for is the whole of `has_more`. The page's
// own length is not the answer, because a short page is also what a concurrent
// write produces, and a client that treats one as the end is the bug
// `has_more` exists to prevent. The probe row is dropped rather than carried,
// so items holds at most the limit the caller asked for.
//
// keyset reads the last returned row's key from the list it is told about, and
// every caller names its own sort rather than this package guessing: the ten
// lists key on three different sorts, and a guess would silently page the
// wrong position rather than fail.
//
// There is no total, and no member of this package computes one. The
// PageEnvelope schema says why at length — a count is either a second query
// whose answer is a different instant from the page's, or a COUNT the first
// page pays for and the last one wastes — and either way the number is wrong
// the moment it is written. A page is a page, a cursor is where to continue,
// and has_more says whether continuing is worth it.
func PageOf[T any](rows []T, limit int, keyset func(T) string, list string, filters map[string]string) (items []T, hasMore bool, nextCursor string) {
	items = rows
	if len(items) > limit {
		hasMore = true
		items = items[:limit]
	}
	// An empty page is a legitimate answer — an account with no users is not
	// an error — and it still carries a cursor, because the contract's
	// `next_cursor` is non-empty on every page and a client holding an empty
	// position would resume from the beginning on every request.
	if items == nil {
		items = []T{}
	}
	last := ""
	if len(items) > 0 {
		last = keyset(items[len(items)-1])
	}
	return items, hasMore, EncodeCursor(list, last, filters)
}
