// Package dataplane is the console-api's outbound HTTP adapter for the usage
// fact feed: the Direction Data → Control half of the one cross-plane seam
// (ADR 0006 §5), spoken over the management contract in
// api/openapi/shared/usage-facts.yaml and its operation in dataplane.yaml.
//
// It is the mirror of this module's other outbound adapters in everything but
// its dependency: postgres and valkey reach infrastructure this deployment
// runs, while this one reaches another application, over a network, with a
// credential. What it is not is a second seam — the port it implements lives in
// internal/ports/outbound/dataplane beside Management, and the application
// calls the port. An adapter that added behaviour the port does not declare
// would be a second place the plane boundary is expressed, which is exactly
// what the one-seam rule exists to prevent.
//
// The adapter holds no state: no buffer, no cache, no retry counter, no learned
// position. It sends one request and decodes one answer, so the facts it hands
// the applier are the facts that arrived in that one response, in the order they
// arrived — not because this package promises to pass bytes through, but because
// there is nothing here that could do otherwise.
//
// What it does do is decode, and that is worth stating next to the promise it
// does not make. Every hop on this chain re-encodes the page in its own types —
// the listener from its store's, the façade from its own values, this adapter
// from the façade's body — so "the page travels unchanged" is a statement about
// the facts and their order and not about the bytes carrying them.
//
// Two rules of the seam are enforced in the code below and are worth naming
// where a reader meets them: the cursor travels verbatim in both directions and
// is never inspected, and no failure message this package produces carries the
// endpoint or the credential.
package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	// The port shares this package's name, so the import is aliased: every
	// reference below is unambiguously the interface, and the adapter it
	// implements is the file it is written in.
	port "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
)

// usageEventsPath is the operation this adapter reaches, as dataplane.yaml
// declares it. It is a constant because it is the contract's, not the caller's:
// nothing in the Control Plane chooses which path the feed lives at.
const usageEventsPath = "/internal/usage-events"

// usageCursorMaxLength is the bound the contract puts on a cursor
// (`shared/usage-facts.yaml`, UsageCursor: `maxLength: 512`), and it is checked
// on the way *in* rather than trusted.
//
// A page's position is the one value in this flow that becomes durable state
// outside this process, in the Control Plane's own table, so it is the one
// value worth refusing before anyone stores it. A response carrying no position
// at all is the worse of the two cases and the reason the bound is not merely
// hygiene: a consumer that stored an empty one would ask from the beginning of
// retained history on every cycle and never advance, silently and forever.
//
// Enforcing it here is not parsing the cursor. A string's length is not its
// content, nothing about the value's meaning is examined, and every cursor the
// check admits still crosses byte for byte — the point is that a value outside
// the contract's own declared shape is a response this seam cannot use, which
// is a fact about the answer rather than a question about the position.
//
// Characters, not bytes: `maxLength` in the schema is a count of code points, so
// counting bytes would refuse a page the contract permits. The difference only
// appears for a cursor containing a multi-byte code point, and it would appear
// as a page every other hop accepts and this one rejects.
const usageCursorMaxLength = 512

// Client reads pages of usage facts from a Data Plane's management surface.
type Client struct {
	// httpClient is injected rather than built here, so timeouts, transport,
	// redirect policy and connection pooling are the composition root's
	// decisions — the same division the postgres adapter keeps with its
	// *sql.DB. Which HTTP transport reaches the Data Plane is not a fact this
	// adapter should hold an opinion about.
	httpClient *http.Client

	// endpoint is the feed's URL, parsed once at construction: the base URL is
	// configuration and its shape is fixed for the life of the client, so the
	// only thing a call appends is the query. Keeping a *url.URL rather than a
	// string is what makes building a request a copy-and-set instead of string
	// concatenation, and string concatenation here would be how a cursor with
	// an `&` in it rewrites the request's parameters.
	endpoint url.URL

	// credential is the service credential the management surface authenticates
	// this application with. It is held only to be put on the wire, and it
	// never reaches a log line or an error: the errors this package builds are
	// assembled from status codes and transport causes, never from the request.
	credential string
}

// New returns the feed reader described by a client, a base URL and a service
// credential. The base URL may carry a path prefix — a deployment behind a
// gateway serves the management surface under one — but not a query or a
// fragment: either would be silently merged into, or dropped from, every
// request this adapter builds, and a configuration value that part of the
// request ignores is a configuration value that lies about what it does.
//
// It panics on a nil *http.Client, on a base URL that is not an absolute
// http(s) URL and on an empty credential, for the reason postgres.New panics
// on a nil pool: each is a wiring defect that otherwise appears later, as an
// unexplained nil dereference or a management call that can only ever be
// refused, far from the line that could have said so.
func New(httpClient *http.Client, baseURL, credential string) *Client {
	if httpClient == nil {
		panic("dataplane: New requires a non-nil *http.Client")
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		panic(fmt.Sprintf("dataplane: base URL is not a URL: %v", err))
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		panic("dataplane: base URL must be http or https")
	}
	if base.Host == "" {
		panic("dataplane: base URL must name a host")
	}
	if base.RawQuery != "" || base.Fragment != "" {
		panic("dataplane: base URL must not carry a query or a fragment")
	}
	if credential == "" {
		panic("dataplane: New requires a service credential")
	}

	base.Path = strings.TrimSuffix(base.Path, "/") + usageEventsPath
	return &Client{httpClient: httpClient, endpoint: *base, credential: credential}
}

// Compile-time proof that the adapter satisfies the port it claims to, so a
// signature drifting from the seam is a build failure rather than a discovery
// at the composition root.
var _ port.UsageFacts = (*Client)(nil)

// ReadUsageEvents fetches the page of facts strictly after `after`, and maps
// the answer onto the port's Page.
//
// `after` goes onto the wire verbatim. The one thing this adapter decides about
// it is emptiness: a consumer that has never applied anything has the empty
// string for a position (the port's own convention), and the contract spells
// "from the beginning of what is retained" as an absent parameter — so an empty
// position is left off, and every non-empty one is escaped into the query
// untouched. Nothing here trims it, defaults it, validates its shape, compares
// it or derives one.
func (c *Client) ReadUsageEvents(ctx context.Context, after string, limit int) (port.Page, error) {
	requestURL := c.endpoint
	query := requestURL.Query()
	if after != "" {
		query.Set("after", after)
	}
	query.Set("limit", strconv.Itoa(limit))
	requestURL.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return port.Page{}, fmt.Errorf("dataplane: build the usage events request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.credential)

	response, err := c.httpClient.Do(request)
	if err != nil {
		return port.Page{}, fmt.Errorf("dataplane: read usage events: %w", transportCause(err))
	}
	defer func() { _ = response.Body.Close() }()

	switch response.StatusCode {
	case http.StatusOK:
		// The page is decoded below; JSON is the only shape the contract
		// declares, and a peer that answers something else fails the decode
		// rather than being guessed at.
	case http.StatusGone:
		// The stored position is no longer replayable. It is the one failure of
		// the operation the Control Plane can act on differently from all the
		// others — it has to re-establish its position, not retry it — so it is
		// the one the port names.
		return port.Page{}, fmt.Errorf("dataplane: read usage events: %w", port.ErrCursorExpired)
	default:
		// 400, 401, 404, 405, 500 and the façade's 502 are not distinguished
		// here: the port has one sentinel, and inventing a second for a status
		// no caller branches on yet would be a refinement the seam does not
		// carry. The status is the whole message because it is the part an
		// operator needs, and the URL is deliberately not part of it.
		return port.Page{}, fmt.Errorf("dataplane: read usage events: unexpected status %d", response.StatusCode)
	}

	var body pageResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return port.Page{}, fmt.Errorf("dataplane: decode the usage events page: %w", err)
	}
	page, err := body.page()
	if err != nil {
		// The refusal is the whole point of this line: a page the consumer
		// cannot act on leaves the read failed, so the stored position is not
		// touched and the next pass asks for the same range again. Returning
		// the page and letting the caller notice would put the decision
		// somewhere that does not read the wire.
		return port.Page{}, err
	}
	return page, nil
}

// pageResponse is the wire shape of one page, field for field as
// UsageFactPage declares it. It exists as a type of its own — rather than the
// port's Page being decoded into directly — for the reason every response type
// in this repository exists: JSON tags and wire spellings are the transport's
// vocabulary, and the port's types stay free of them.
//
// Every field the contract marks required is a pointer. With a plain value,
// encoding/json answers a body that omits the field — or carries it as null —
// with the Go zero value, and the page reads as well-formed all the way to the
// durable position it moves; with a pointer, both shapes land on a nil, which
// page below refuses. A field that is present decodes exactly as it did
// before, and a field the contract does not name is still ignored, so the
// tolerance the decoder keeps is untouched.
//
// Of the contract's constraints, this decode checks presence and exactly one
// bound, and the line between them and everything else is the whole design.
// The envelope's three fields and each fact's five must be there, and a cursor
// may be no longer than a cursor can be — all statements about the declared
// shape, all decidable without knowing what a cursor or a fact means. What is
// left alone is everything that would require understanding the page: the enum
// of kinds belongs to the applier, which is the only code that knows which
// ones this build can settle; the payload's shape belongs to the schema that
// wrote it; and the cursor's meaning belongs to the Data Plane that issued it.
// A consumer that re-checked those would be interpreting a value whose only
// promise is that it may be returned verbatim, and the one thing this side
// must never do with a cursor is decide what a valid one looks like.
type pageResponse struct {
	Events     *[]eventResponse `json:"events"`
	NextCursor *string          `json:"next_cursor"`
	HasMore    *bool            `json:"has_more"`
}

// eventResponse is one UsageEvent on the wire. OccurredAt is decoded as a
// *time.Time because the contract types it as an RFC 3339 date-time, and a
// consumer that cannot parse one is looking at a response it cannot trust to
// have carried a page at all — the decode fails loudly instead, and a body
// that never carried the field is a nil rather than an instant that looks like
// the beginning of time. Payload stays raw: its columns belong to the schema
// that wrote the fact.
type eventResponse struct {
	RequestID     *string          `json:"request_id"`
	Kind          *string          `json:"kind"`
	SchemaVersion *int             `json:"schema_version"`
	OccurredAt    *time.Time       `json:"occurred_at"`
	Payload       *json.RawMessage `json:"payload"`
}

// page translates the wire shape into the port's, one field at a time so a
// field added to one and not the other is a compile-time omission rather than
// a silent zero — and refuses every way the answer can fail to be a page at
// all.
//
// The refusal is a precondition on the translation rather than an extra rule
// on top of it. Every field below is copied verbatim, and the only reason the
// copy is conditional is that the port's Page promises a position a consumer
// can store and facts a consumer can settle from: a page out of the contract's
// declared shape would be a page that broke that promise, and a caller that
// received it anyway would go on to advance a durable cursor with it. The
// whole page is refused when one fact in it fails, so a body that cannot be
// read whole is never applied in part.
func (r pageResponse) page() (port.Page, error) {
	switch {
	case r.Events == nil:
		return port.Page{}, fmt.Errorf("%w: the page carried no events field, which the contract requires", port.ErrMalformedPage)
	case r.NextCursor == nil:
		return port.Page{}, fmt.Errorf("%w: the page carried no next_cursor field, which the contract requires", port.ErrMalformedPage)
	case *r.NextCursor == "":
		return port.Page{}, fmt.Errorf("%w: the page carried no next_cursor, so there is no position to advance to", port.ErrMalformedPage)
	case utf8.RuneCountInString(*r.NextCursor) > usageCursorMaxLength:
		return port.Page{}, fmt.Errorf("%w: the page's next_cursor is %d characters long and the contract allows %d", port.ErrMalformedPage, utf8.RuneCountInString(*r.NextCursor), usageCursorMaxLength)
	case r.HasMore == nil:
		return port.Page{}, fmt.Errorf("%w: the page carried no has_more field, which the contract requires", port.ErrMalformedPage)
	}

	events := make([]port.Event, 0, len(*r.Events))
	for _, event := range *r.Events {
		one, err := event.event()
		if err != nil {
			return port.Page{}, err
		}
		events = append(events, one)
	}
	return port.Page{Events: events, NextCursor: *r.NextCursor, HasMore: *r.HasMore}, nil
}

// event translates one fact into the port's vocabulary, refusing it when any
// of the five fields the contract marks required is absent or null. Presence
// is the only judgement: an empty request_id, a kind this build has never
// heard of and a schema_version it does not know all cross — deciding which of
// them can settle is the applier's, below the port.
func (r eventResponse) event() (port.Event, error) {
	switch {
	case r.RequestID == nil:
		return port.Event{}, fmt.Errorf("%w: the page carries a fact with no request_id, which the contract requires", port.ErrMalformedPage)
	case r.Kind == nil:
		return port.Event{}, fmt.Errorf("%w: the page carries a fact with no kind, which the contract requires", port.ErrMalformedPage)
	case r.SchemaVersion == nil:
		return port.Event{}, fmt.Errorf("%w: the page carries a fact with no schema_version, which the contract requires", port.ErrMalformedPage)
	case r.OccurredAt == nil:
		return port.Event{}, fmt.Errorf("%w: the page carries a fact with no occurred_at, which the contract requires", port.ErrMalformedPage)
	case r.Payload == nil:
		return port.Event{}, fmt.Errorf("%w: the page carries a fact with no payload, which the contract requires", port.ErrMalformedPage)
	}
	return port.Event{
		RequestID:     *r.RequestID,
		Kind:          *r.Kind,
		SchemaVersion: *r.SchemaVersion,
		OccurredAt:    *r.OccurredAt,
		Payload:       *r.Payload,
	}, nil
}

// transportCause strips the URL net/http attaches to a failed request.
//
// http.Client wraps every transport failure in a *url.Error whose message
// embeds the full request URL — which here carries the base URL and the
// `after` cursor, a value whose only promise is opacity. The cause underneath
// is what says what went wrong (a refused connection, a closed connection, an
// expired context), so the cause is what this package reports and the wrapper
// is deliberately dropped. Everything else is returned as it came.
func transportCause(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}
