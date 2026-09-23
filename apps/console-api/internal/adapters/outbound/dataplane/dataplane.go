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
// position. It sends one request and decodes one answer, so a page is exactly
// what the Data Plane said in that response — the façade's documented promise
// that a page is never cached, buffered, reordered or transformed holds here as
// well, by there being nothing in this package that could do it.
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

	// The port shares this package's name, so the import is aliased: every
	// reference below is unambiguously the interface, and the adapter it
	// implements is the file it is written in.
	port "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
)

// usageEventsPath is the operation this adapter reaches, as dataplane.yaml
// declares it. It is a constant because it is the contract's, not the caller's:
// nothing in the Control Plane chooses which path the feed lives at.
const usageEventsPath = "/internal/usage-events"

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
	return body.page(), nil
}

// pageResponse is the wire shape of one page, field for field as
// UsageFactPage declares it. It exists as a type of its own — rather than the
// port's Page being decoded into directly — for the reason every response type
// in this repository exists: JSON tags and wire spellings are the transport's
// vocabulary, and the port's types stay free of them.
//
// Nothing here validates the page. The contract's constraints — a non-empty
// next_cursor, an enum of kinds, a payload whose shape only the schema knows —
// are the Data Plane's to keep and the applier's to act on; a consumer that
// re-checked them would be interpreting a value whose only promise is that it
// may be returned verbatim, and the one thing this side must never do with a
// cursor is decide what a valid one looks like.
type pageResponse struct {
	Events     []eventResponse `json:"events"`
	NextCursor string          `json:"next_cursor"`
	HasMore    bool            `json:"has_more"`
}

// eventResponse is one UsageEvent on the wire. OccurredAt is decoded as a
// time.Time because the contract types it as an RFC 3339 date-time, and a
// consumer that cannot parse one is looking at a response it cannot trust to
// have carried a page at all — the decode fails loudly instead. Payload stays
// raw: its columns belong to the schema that wrote the fact.
type eventResponse struct {
	RequestID     string          `json:"request_id"`
	Kind          string          `json:"kind"`
	SchemaVersion int             `json:"schema_version"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}

// page translates the wire shape into the port's, one field at a time so a
// field added to one and not the other is a compile-time omission rather than
// a silent zero.
func (r pageResponse) page() port.Page {
	events := make([]port.Event, 0, len(r.Events))
	for _, event := range r.Events {
		events = append(events, port.Event{
			RequestID:     event.RequestID,
			Kind:          event.Kind,
			SchemaVersion: event.SchemaVersion,
			OccurredAt:    event.OccurredAt,
			Payload:       event.Payload,
		})
	}
	return port.Page{Events: events, NextCursor: r.NextCursor, HasMore: r.HasMore}
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
