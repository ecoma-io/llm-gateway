package http

import (
	"encoding/json"
	stdhttp "net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// GET /internal/usage-events: the Data Plane's usage-fact feed, served from
// here and answered from there.
//
// The delivery model is stated once in api/openapi/shared/usage-facts.yaml and
// this file is the transport's part of it. What matters at this layer is what
// the handler does *not* do: it keeps nothing between the two calls — no cursor
// for the next call, no page for the next reader, no fact reordered or dropped.
// A position held here would be a third party's opinion about a protocol whose
// two participants are the Data Plane that owns the facts and the Control Plane
// that owns its position.
//
// Keeping nothing is not the same as passing everything through, and the
// difference is this file's three checks: the parameters are validated against
// the contract's declared shape, an undeclared name is refused, and the answer
// is written from the port's values rather than copied from the listener's
// body. Re-encoding a page from values read once, in this call, is statelessness
// — the response type below exists for it. Answering from anything read earlier
// is what the paragraph above forbids. The two are easy to conflate under the
// word "relay", so the word is not used here (ADR 0006 §9, §11).
//
// The caller is verified before any of that runs. The check is the wrapper in
// serviceauth.go, applied at the route table rather than inside the handler, so
// a request without a trusted credential never reaches the application — the
// refusal is a 401 in the contract's envelope and the feed is never touched.

const (
	// usageEventsPath is the operation's path, spelled exactly as
	// api/openapi/dataplane.yaml declares it. routes_test.go pins the route
	// table against a literal list and contract_test.go compares that table
	// with the document, so this constant is the one place the spelling lives
	// in code.
	usageEventsPath = "/internal/usage-events"

	// defaultUsageEventsLimit is the page size used when the caller names none.
	// It is the default the contract declares in
	// api/openapi/shared/usage-facts.yaml, applied here rather than left for
	// the Data Plane to fill in because the port carries an int: a caller's
	// page size should be decided once, at the surface it called, instead of
	// depending on which hop happened to notice the parameter was absent.
	defaultUsageEventsLimit = 100

	// The bounds the contract declares for `limit`. A value outside them is a
	// request this surface refuses rather than clamps: the Data Plane would
	// reject it too, and clamping here would answer a question the caller did
	// not ask while hiding that it asked an invalid one.
	//
	// The Data Plane's private listener refuses exactly the same values — the
	// numbers come from the one contract both hops implement
	// (shared/usage-facts.yaml) — so a caller cannot discover a different bound
	// by walking up or down the chain.
	minUsageEventsLimit = 1
	maxUsageEventsLimit = 1000

	// maxUsageEventsCursorLength is the bound the contract declares on a cursor
	// (`maxLength: 512`), and it is checked here for the same reason the Data
	// Plane's own listener checks it: length is not content. A cursor longer
	// than this cannot be one the feed issued, so forwarding it would spend a
	// round trip to be told so — and the caller would learn it as a 502, which
	// says the Data Plane is unavailable rather than that its own request was
	// impossible. Refusing it here keeps the answer about the caller's request.
	//
	// Characters, not bytes, because that is what the schema's `maxLength`
	// counts. The two differ only for a cursor with a multi-byte code point in
	// it, and that is the case where being stricter than the contract would mean
	// refusing a value the Data Plane had already issued and this surface had
	// already served.
	maxUsageEventsCursorLength = 512
)

// usageEventsHandler serves one page of the feed. It reaches the application —
// and through it the Data Plane — only after requireServiceCaller has admitted
// the caller, and it adds nothing to what comes back.
func usageEventsHandler(app *application.App) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		query := r.URL.Query()

		if usageEventsUnknownParameter(query) {
			writeError(w, r, invalidRequestError{message: "the only query parameters this operation accepts are after and limit"})
			return
		}

		after, err := usageEventsAfter(query)
		if err != nil {
			writeError(w, r, err)
			return
		}
		limit, err := usageEventsLimit(query)
		if err != nil {
			writeError(w, r, err)
			return
		}

		page, err := app.UsageEvents(r.Context(), after, limit)
		if err != nil {
			writeError(w, r, err)
			return
		}

		writeJSON(w, stdhttp.StatusOK, newUsageEventsResponse(page))
	}
}

// usageEventsAfter reads the cursor parameter, or reports that it is not one.
//
// The façade does not interpret the value — that is the whole point of an
// opaque cursor, and this function is where the temptation to do it would
// appear. What it does check is the little that can be known without
// interpreting: that the parameter is present at most once, that it is not
// empty, and that it is not longer than a cursor can be. Those three are the
// contract's declared *shape* and nothing about what the value means; the
// length bound in particular is not a step toward parsing, since a string's
// length says nothing about its content and every value the check admits is
// still forwarded byte for byte.
//
// An empty `after` is refused rather than read as "from the beginning", because
// the contract declares the cursor's minimum length as one and defines the
// beginning by the parameter's *absence*; treating `after=` as a synonym would
// be this process deciding that a value the caller sent means something else,
// which is exactly the kind of help that hides a broken consumer.
func usageEventsAfter(query url.Values) (string, error) {
	values, present := query["after"]
	if !present {
		return "", nil
	}
	if len(values) != 1 {
		return "", invalidRequestError{message: "after must be given at most once"}
	}
	if values[0] == "" {
		return "", invalidRequestError{message: "after must be a cursor from a previous page; omit it to read from the beginning"}
	}
	if utf8.RuneCountInString(values[0]) > maxUsageEventsCursorLength {
		return "", invalidRequestError{message: "after must be at most " + strconv.Itoa(maxUsageEventsCursorLength) + " characters; pass back the cursor a previous page returned"}
	}
	return values[0], nil
}

// usageEventsUnknownParameter reports whether the caller named something this
// operation does not declare.
//
// The contract attaches `400 invalid_request` to this operation, and
// shared/errors.yaml says what that code is for: a request the caller cannot be
// allowed to believe took effect. `?position=abc` is exactly that — a caller
// who named their starting point, and who would read a 200 as confirmation that
// it was honoured. The private listener behind this façade refuses an
// undeclared name for the same reason; without this check the two hops
// disagree about one input and this is the permissive end of the protocol.
//
// The name that arrived is not in the message, which is the same rule the
// listener follows: a caller-shaped string has no business travelling back out
// of this process, however harmless it looks.
func usageEventsUnknownParameter(query url.Values) bool {
	for name := range query {
		if name != "after" && name != "limit" {
			return true
		}
	}
	return false
}

// usageEventsLimit reads the page size, defaulting it and refusing anything the
// contract does not allow.
//
// A malformed limit is a 400 and nothing else: it must not travel to the Data
// Plane to be refused there, because a request this surface can already see is
// invalid should cost the Data Plane nothing at all.
func usageEventsLimit(query url.Values) (int, error) {
	values, present := query["limit"]
	if !present {
		return defaultUsageEventsLimit, nil
	}
	if len(values) != 1 {
		return 0, invalidRequestError{message: "limit must be given at most once"}
	}
	limit, err := strconv.Atoi(values[0])
	if err != nil || limit < minUsageEventsLimit || limit > maxUsageEventsLimit {
		return 0, invalidRequestError{message: "limit must be an integer between 1 and 1000"}
	}
	return limit, nil
}

// usageEventsResponse and usageEventResponse are the wire shapes this surface
// writes, mirroring UsageFactPage in api/openapi/shared/usage-facts.yaml field
// for field. The port above speaks in values; serialization stays on this side
// of the boundary, as it does for the version and error envelopes.
type usageEventsResponse struct {
	Events     []usageEventResponse `json:"events"`
	NextCursor string               `json:"next_cursor"`
	HasMore    bool                 `json:"has_more"`
}

type usageEventResponse struct {
	RequestID     string          `json:"request_id"`
	Kind          string          `json:"kind"`
	SchemaVersion int             `json:"schema_version"`
	OccurredAt    string          `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}

// newUsageEventsResponse renders a page. Every value crosses unchanged: the
// cursor is the string the Data Plane issued, `has_more` is the Data Plane's
// answer rather than something derived from the page length, and the payload
// travels as the bytes it arrived as, so nothing in the body is a re-encoding
// of a fact this process understood.
//
// The events slice is built non-nil so an empty page serializes as `[]` rather
// than `null`. Go's zero value for a slice is nil and its JSON is null, which
// the schema — `type: array` — does not allow, and a consumer that decodes
// strictly would fail on the empty page a caught-up consumer sees most often.
func newUsageEventsResponse(page dataplane.Page) usageEventsResponse {
	events := make([]usageEventResponse, 0, len(page.Events))
	for _, event := range page.Events {
		events = append(events, usageEventResponse{
			RequestID:     event.RequestID,
			Kind:          event.Kind,
			SchemaVersion: event.SchemaVersion,
			// RFC 3339 as the contract declares it. The instant is the one the
			// Data Plane recorded; only its rendering is chosen here, because
			// the port carries a time and the wire carries a date-time string.
			// `occurred_at` is informational — never the ordering key and never
			// the deduplication key — so nothing downstream can depend on the
			// difference.
			OccurredAt: event.OccurredAt.Format(time.RFC3339Nano),
			Payload:    event.Payload,
		})
	}
	return usageEventsResponse{Events: events, NextCursor: page.NextCursor, HasMore: page.HasMore}
}

// invalidRequestError is the transport fact that a request is well-formed HTTP
// but breaks this operation's contract. The message names the parameter and the
// rule; it never echoes the value, which is caller-shaped input and belongs in
// the access log if anywhere, not in a response an operator reads.
type invalidRequestError struct{ message string }

func (err invalidRequestError) Error() string {
	return err.message
}

func (err invalidRequestError) response() (int, string, string) {
	return stdhttp.StatusBadRequest, "invalid_request", err.message
}
