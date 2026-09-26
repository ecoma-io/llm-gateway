package management

import (
	"encoding/json"
	stdhttp "net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/usagefacts"
)

// maxCursorLength is the bound the page shape declares on a cursor — the same
// 512 the façade contracts, because the two hops carry one page and a bound that
// differed between them would be a page one of them admits and the other does
// not (docs/architecture/cross-plane-protocols.md).
//
// The bound is enforced here and the cursor's *content* is not, and the
// difference is the whole point. A cursor is opaque: this surface produced it,
// this surface will place it, and nothing in between — this handler included —
// may look inside it. Length is not content. What the length check buys is that
// a caller cannot make this process carry an arbitrarily large string into a
// store query whose own limits are a later PR's business, and that a value
// outside the contract's declared shape is refused as such instead of failing
// somewhere deeper with a message nobody can act on.
//
// It is a count of characters and not of bytes, and that is what the contract
// says: `maxLength` in JSON Schema bounds a string's length in code points, so
// counting bytes here would refuse a cursor this feed is entitled to issue and
// the façade is entitled to serve. The disagreement would not be visible for an
// ASCII cursor, which is what a query parameter will almost always be — it
// would appear as a page that one hop refuses and the other accepts, which is
// the failure this constant's first paragraph exists to prevent. The bytes a
// caller can make this process carry are bounded by four times this number,
// which is still a bound.
const maxCursorLength = 512

// usageEvent is one fact as this surface puts it on the wire: the envelope the
// private protocol's page declares, which is the façade's `UsageEvent` schema
// field for field — the identity and the envelope fields the page has always
// carried, plus the typed settlement figures the fact contract requires
// beside the payload. The two hops carry one page and this end is where it
// is written, so the shape is stated here and only mirrored at the façade — a
// field added on this side alone would be dropped at the façade's decoder, and
// the protocol tests on both sides exist to make that a build failure rather
// than a page that quietly loses a column.
//
// The pointer types are the null fidelity the contract requires: a nullable
// field's null is the claim that this fact makes no such claim, and the wire
// must carry the distinction a stored zero is not. encoding/json cannot
// distinguish an absent field from a null one, and the contract requires every
// field, so a nil pointer marshals to the null the contract reads.
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
	AppendSeq     int64           `json:"append_seq"`
	RequestID     string          `json:"request_id"`
	Kind          string          `json:"kind"`
	SchemaVersion int             `json:"schema_version"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`

	CaptureMethod        *string `json:"capture_method"`
	CommittedAttemptID   *string `json:"committed_attempt_id"`
	ProviderInputTokens  *int64  `json:"provider_input_tokens"`
	ProviderOutputTokens *int64  `json:"provider_output_tokens"`
	DeliveryTokens       *int64  `json:"delivery_tokens"`
	PriceRevisionID      *string `json:"price_revision_id"`
	InputUnitPrice       *int64  `json:"input_unit_price"`
	OutputUnitPrice      *int64  `json:"output_unit_price"`
	SettledAmount        *int64  `json:"settled_amount"`
	CorrectsAppendSeq    *int64  `json:"corrects_append_seq"`
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
				AppendSeq:     event.AppendSeq,
				RequestID:     event.RequestID,
				Kind:          event.Kind,
				SchemaVersion: event.SchemaVersion,
				OccurredAt:    event.OccurredAt,
				Payload:       payloadOrEmpty(event.Payload),

				CaptureMethod:        event.CaptureMethod,
				CommittedAttemptID:   event.CommittedAttemptID,
				ProviderInputTokens:  event.ProviderInputTokens,
				ProviderOutputTokens: event.ProviderOutputTokens,
				DeliveryTokens:       event.DeliveryTokens,
				PriceRevisionID:      event.PriceRevision,
				InputUnitPrice:       event.InputUnitPrice,
				OutputUnitPrice:      event.OutputUnitPrice,
				SettledAmount:        event.SettledAmount,
				CorrectsAppendSeq:    event.CorrectsAppendSeq,
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
// It enforces the contract's declared *shape* and nothing about what either
// parameter means. `after` is an optional string of bounded length and is passed
// through byte for byte; parsing it would be interpreting a value this surface
// called opaque. `limit` is an optional integer, defaulted and bounded here, and
// the bounds are the port's own constants — so the page size is chosen once, at
// the surface the caller reached, and the use case below relays a value that is
// already a legal page.
//
// A `limit` outside the bounds is refused rather than clamped, and that is a
// decision about honesty rather than about strictness. Clamping answers a
// question the caller did not ask and hides that it asked an invalid one: a
// consumer that sized a page to fit what it can hold in memory would receive a
// smaller page, see `has_more` still true, and have nothing anywhere telling it
// that its own number was ignored. Both this surface and the façade in front of
// it refuse the same values with the same status, so a caller cannot discover
// the bounds by walking up the chain.
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
//
// An `after` that is present and empty is refused too, and it is the one case
// where a value is refused for what it would otherwise be taken to mean. The
// protocol defines the beginning by the parameter's *absence*; a caller that
// sent `after=` has a position, and a broken one, and answering it as a request
// for everything retained would hide exactly the bug it is evidence of. The
// façade in front of this listener refuses the same value for the same reason,
// so the two ends of one protocol agree about every input rather than about all
// but one.
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
		if after == "" {
			return "", 0, factQueryError("after must be a cursor from a previous page; omit it to read from the beginning")
		}
		if utf8.RuneCountInString(after) > maxCursorLength {
			return "", 0, factQueryError("after must be at most " + strconv.Itoa(maxCursorLength) + " characters")
		}
	default:
		return "", 0, factQueryError("after must be supplied at most once")
	}

	limit := usagefacts.DefaultLimit
	switch limits := query["limit"]; len(limits) {
	case 0:
	case 1:
		parsed, err := strconv.Atoi(limits[0])
		if err != nil {
			return "", 0, factQueryError("limit must be an integer")
		}
		if parsed < 1 || parsed > usagefacts.MaxLimit {
			return "", 0, factQueryError("limit must be an integer between 1 and " + strconv.Itoa(usagefacts.MaxLimit))
		}
		limit = parsed
	default:
		return "", 0, factQueryError("limit must be supplied at most once")
	}

	return after, limit, nil
}

// payloadOrEmpty substitutes an empty JSON object for a fact that carries no
// body: no bytes at all, or the four bytes of a literal JSON `null` — the two
// spellings of the same absence, because a fact source that surfaces a JSON
// null column hands back `json.RawMessage("null")` where an empty one hands
// back an empty slice, and neither is a page this surface may write.
//
// The contract types `payload` as an object and requires it, so `null` would be
// a response no consumer could parse against the document it was written from.
// An empty body is a fact with nothing in it, which is what an empty object
// says; `null` would say the fact has no body at all, and it would make every
// consumer write the same nil check to find that out.
//
// The match is byte-exact and not whitespace-tolerant, because no producer
// exists that could pad it: jsonb's text rendering carries no surrounding
// whitespace, and nothing on this path rewrites the bytes. A lenient match here
// would be guessing at inputs nothing upstream can emit, while making this
// guard's charter — absence, exactly as written — harder to state. The other
// boundary is the JSON *string* `"null"`, four bytes wrapped in quotes: it is
// a value and not an absence, it passes through untouched like every other
// body, and the object shape behind it is already refused elsewhere — by the
// store's `usage_events_payload_shape` CHECK on the way in and by the fact
// source's own decoding on the way out — so widening this guard to judge
// shapes would duplicate a refusal that belongs to those two places.
func payloadOrEmpty(payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 || string(payload) == "null" {
		return json.RawMessage(`{}`)
	}
	return payload
}
