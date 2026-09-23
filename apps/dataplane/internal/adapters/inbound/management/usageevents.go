package management

import (
	"encoding/json"
	stdhttp "net/http"
	"strconv"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
)

// maxCursorLength is the bound api/openapi/shared/usage-facts.yaml declares on a
// cursor (`maxLength: 512`).
//
// The bound is enforced here and the cursor's *content* is not, and the
// difference is the whole point. A cursor is opaque: this surface produced it,
// this surface will place it, and nothing in between — this handler included —
// may look inside it. Length is not content. What the length check buys is that
// a caller cannot make this process carry an arbitrarily large string into a
// store query whose own limits are a later PR's business, and that a value
// outside the contract's declared shape is refused as such instead of failing
// somewhere deeper with a message nobody can act on.
const maxCursorLength = 512

// usageEvent is one fact as this surface puts it on the wire, mirroring the
// UsageEvent schema field for field.
//
// It is a transport type and not the port's Event for the usual reason: the
// port's vocabulary is Go's and this one's is JSON's, and the day the wire
// changes — a field renamed, an envelope version added — the contract changes
// and this file changes with it, while the port the application talks to does
// not.
//
// Payload travels as raw JSON because there is nothing here to do to it. This
// surface is a reader of facts, not their interpreter: the bytes the Data Plane
// recorded are the bytes that go out, and a `map[string]any` in between would
// re-encode every number as a float and drop the key order for no one's
// benefit.
type usageEvent struct {
	RequestID     string          `json:"request_id"`
	Kind          string          `json:"kind"`
	SchemaVersion int             `json:"schema_version"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}

type usageFactPage struct {
	Events     []usageEvent `json:"events"`
	NextCursor string       `json:"next_cursor"`
	HasMore    bool         `json:"has_more"`
}

// usageEvents answers the fact feed.
//
// It reads a page and writes it, and it does nothing else on purpose. Every
// rule of the delivery protocol lives one layer down or one layer up: the
// ordering is the store's, the cursor is the port's, and the position belongs to
// the consumer. A handler that sorted a page, filtered it, or advanced anything
// would be applying a policy to every consumer at once from the process that
// serves LLM traffic.
func usageEvents(app *application.App) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		after, limit, err := readFactQuery(r)
		if err != nil {
			writeFailure(w, r, invalidRequestFailure(err.Error()))
			return
		}

		page, err := app.ReadUsageEvents(r.Context(), after, limit)
		if err != nil {
			writeFailure(w, r, failureFor(err))
			return
		}

		// A page with no position is a page the protocol cannot use. The
		// contract promises `next_cursor` is non-empty, and a consumer that
		// stored an empty one would ask from the beginning of retained history
		// on every cycle, re-reading the same facts forever without ever
		// advancing. Refusing it here turns a source that cannot count into a
		// loud failure at the surface that promised otherwise.
		if page.NextCursor == "" {
			writeFailure(w, r, failureFor(application.Internal(errNoCursor)))
			return
		}

		events := make([]usageEvent, 0, len(page.Events))
		for _, event := range page.Events {
			events = append(events, usageEvent{
				RequestID:     event.RequestID,
				Kind:          event.Kind,
				SchemaVersion: event.SchemaVersion,
				OccurredAt:    event.OccurredAt,
				Payload:       payloadOrEmpty(event.Payload),
			})
		}

		writeJSON(w, stdhttp.StatusOK, usageFactPage{
			Events:     events,
			NextCursor: page.NextCursor,
			HasMore:    page.HasMore,
		})
	}
}

type factQueryError string

func (err factQueryError) Error() string { return string(err) }

// errNoCursor is the cause behind a page that arrived without a position. It
// exists to be logged against the request ID, never to be serialized.
var errNoCursor = factQueryError("the fact source returned a page with no position")

// readFactQuery turns the query string into the use case's arguments, and is the
// only place in this package that looks at a caller-supplied value.
//
// It enforces the contract's declared *shape* — `after` is an optional string of
// bounded length, `limit` is an optional integer of at least one — and nothing
// about what either one means. `after` is passed through byte for byte; parsing
// it would be interpreting a value this surface called opaque. `limit` travels
// as the caller wrote it, including a value above the maximum, which the
// application serves at the maximum: that caller's intent is legible, a smaller
// page than asked for is unambiguous against `has_more`, and failing a
// reconciliation run over a page size would trade a slightly smaller answer
// against a consumer that never advances. A value below the minimum is the
// opposite case — there is no intent behind "minus one facts" to honour — and it
// is refused rather than quietly upgraded to a default.
//
// A parameter supplied twice is refused rather than resolved. There is no
// defensible rule for which of two cursors a caller meant, and answering with
// either would let a caller believe it had been read as the other — which, on a
// feed whose position is the only thing standing between a settlement and a
// double settlement, is not a guess worth making.
//
// A parameter this operation does not declare is refused for the same reason in
// a different shape: `?position=` is a caller who believes they named the
// starting point, and answering with the whole retained history would leave them
// believing it worked. The message names the accepted parameters rather than
// echoing what arrived — a caller-supplied string has no business travelling
// back out of this process, however harmless it looks.
func readFactQuery(r *stdhttp.Request) (string, int, error) {
	query := r.URL.Query()
	for name := range query {
		if name != "after" && name != "limit" {
			return "", 0, factQueryError("the only query parameters this operation accepts are after and limit")
		}
	}

	after := ""
	switch cursors := query["after"]; len(cursors) {
	case 0:
	case 1:
		after = cursors[0]
		if len(after) > maxCursorLength {
			return "", 0, factQueryError("after must be at most " + strconv.Itoa(maxCursorLength) + " characters")
		}
	default:
		return "", 0, factQueryError("after must be supplied at most once")
	}

	limit := 0
	switch limits := query["limit"]; len(limits) {
	case 0:
	case 1:
		parsed, err := strconv.Atoi(limits[0])
		if err != nil {
			return "", 0, factQueryError("limit must be an integer")
		}
		if parsed < 1 {
			return "", 0, factQueryError("limit must be at least 1")
		}
		limit = parsed
	default:
		return "", 0, factQueryError("limit must be supplied at most once")
	}

	return after, limit, nil
}

// payloadOrEmpty substitutes an empty JSON object for a fact that carries no
// body.
//
// The contract types `payload` as an object and requires it, so `null` would be
// a response no consumer could parse against the document it was written from.
// An empty body is a fact with nothing in it, which is what an empty object
// says; `null` would say the fact has no body at all, and it would make every
// consumer write the same nil check to find that out.
func payloadOrEmpty(payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 {
		return json.RawMessage(`{}`)
	}
	return payload
}
