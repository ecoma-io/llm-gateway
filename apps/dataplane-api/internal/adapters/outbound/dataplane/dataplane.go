// Package dataplane adapts the outbound port to the Data Plane's management
// listener over HTTP: the call this façade makes so it has something to answer
// with, and the only network dependency in the module.
//
// The adapter is handed an open *http.Client, the listener's origin and the
// service credential rather than reading any of them itself. Which client,
// which address and which secret are deployment decisions made once in
// cmd/dataplane-api; a client this package built would take the process's
// timeouts and proxy configuration out of the composition root's hands, and the
// credential would be a second thing reading the environment.
//
// It is a relay and nothing else. It forwards `after` and `limit` — the cursor
// as the opaque string it received, never parsed, never trimmed, never
// remembered — and returns the page exactly as the Data Plane wrote it. There
// is no cache and no buffer here: a cached page would be this process answering
// from state it does not own, which is the one thing a management façade may
// not do (ADR 0006 §9, §11).
package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	stdhttp "net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// usageEventsPath is the operation this adapter calls, spelled as
// api/openapi/dataplane.yaml declares it. It is a constant rather than an
// argument because this adapter serves one operation: the day the management
// surface fronts a second one, that is a new method and a visible second path
// here, not a parameter every call site has to get right.
const usageEventsPath = "/internal/usage-events"

// Client is the Data Plane management listener seen as the usage-fact port.
type Client struct {
	client     *stdhttp.Client
	baseURL    string
	credential string
}

// compile-time proof that the adapter satisfies the port it claims to. A method
// rename on either side is a build failure here rather than a runtime surprise
// at the first poll.
var _ dataplane.UsageFacts = (*Client)(nil)

// New returns the usage-fact port backed by client, calling the listener at
// baseURL and presenting credential as the service identity.
//
// It panics on a nil client rather than storing one, for the reason the
// repository's other outbound adapters do: the failure a nil client produces
// later is a panic inside net/http, far from the wiring that caused it, and
// that is strictly worse than a loud one at the construction site.
//
// baseURL and credential are trusted here rather than validated. internal/config
// refuses an empty credential, a non-http(s) URL and a URL with anything after
// the port, so an empty value reaching this constructor is a wiring bug, and
// re-checking it here would be a second, weaker statement of a rule that
// already has a home.
func New(client *stdhttp.Client, baseURL, credential string) *Client {
	if client == nil {
		panic("dataplane: New requires a non-nil *http.Client — build one before the adapter")
	}
	return &Client{client: client, baseURL: baseURL, credential: credential}
}

// ReadUsageEvents implements the port. See dataplane.UsageFacts for the
// contract; what this implementation adds is the wire.
//
// The request carries the caller's context and nothing else, so the call dies
// with the request that caused it: a Control Plane that gives up or disconnects
// cancels this read rather than leaving it to occupy a connection until some
// timeout nobody measured expires. A second, invented deadline here would be a
// number picked without latency to measure against, and it would compound with
// the caller's own rather than replace it.
func (c *Client) ReadUsageEvents(ctx context.Context, after string, limit int) (dataplane.Page, error) {
	request, err := c.request(ctx, after, limit)
	if err != nil {
		return dataplane.Page{}, fmt.Errorf("%w: the management request could not be built", dataplane.ErrUpstreamUnavailable)
	}

	response, err := c.client.Do(request)
	if err != nil {
		// The cause is deliberately dropped, and this is the one place in the
		// file where dropping information is the point: net/http builds the
		// text of a transport failure around the request URL — `Get
		// "http://dataplane:8083/...": dial tcp ...: connect: connection
		// refused` — and this error reaches an operator's log, where the
		// deployment's private addresses do not belong. What a caller can act
		// on is the classification, which the sentinel carries.
		return dataplane.Page{}, fmt.Errorf("%w: the management call did not complete", dataplane.ErrUpstreamUnavailable)
	}
	defer func() { _ = response.Body.Close() }()

	switch response.StatusCode {
	case stdhttp.StatusOK:
		return c.page(response)
	case stdhttp.StatusGone:
		// The Data Plane refuses a position it can no longer replay. Relaying
		// the refusal is the whole job: this process may not resume from a
		// newer position, because that would skip facts between the two and a
		// skipped fact is unsettled money.
		return dataplane.Page{}, fmt.Errorf("%w: the data plane no longer retains that position", dataplane.ErrCursorExpired)
	default:
		// Everything else — including the Data Plane refusing this process's
		// own credential — is one condition to a caller: the answer is unknown.
		// The status is named because it is the one fact that tells an operator
		// which side to look at, and a status code is not an address.
		return dataplane.Page{}, fmt.Errorf("%w: the data plane answered %d", dataplane.ErrUpstreamUnavailable, response.StatusCode)
	}
}

// request builds the one call this adapter makes. The query is assembled from
// the operation's two parameters and nothing else: no extra parameter is
// invented, so the Data Plane's own contract stays the only description of what
// a caller may ask for.
//
// An empty `after` is omitted rather than sent empty, because the contract
// reads absence as "from the beginning of what is retained" and declares the
// cursor's minimum length as one — an `after=` on the wire would be a value the
// Data Plane has already said it does not accept. The inbound surface refuses
// an explicitly empty cursor for the same reason; by the time a value reaches
// this function it is either absent or a cursor.
func (c *Client) request(ctx context.Context, after string, limit int) (*stdhttp.Request, error) {
	query := url.Values{}
	if after != "" {
		query.Set("after", after)
	}
	query.Set("limit", strconv.Itoa(limit))

	// Concatenation rather than url.JoinPath: internal/config has already
	// established that baseURL is an origin with no path, so appending the
	// operation's own path cannot produce a doubled slash or swallow a prefix
	// an operator meant. The encoded query is what keeps an opaque cursor
	// containing `&`, `=` or a space from arriving as two parameters or a
	// truncated value; the Data Plane decodes it back to the identical string,
	// so the cursor still crosses untouched.
	endpoint := c.baseURL + usageEventsPath + "?" + query.Encode()

	request, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.credential)
	return request, nil
}

// page reads a success body into the port's vocabulary.
//
// The decoder is deliberately tolerant of unknown keys: the Data Plane may add
// a field to the envelope or the payload before this module ships, and a façade
// that refused an unfamiliar key would be a version lock on the plane it
// fronts. What it is not tolerant of is a body it cannot read at all — a
// truncated page or an unparseable timestamp is ErrUpstreamUnavailable, because
// answering with part of a page would be this process deciding which facts a
// consumer gets to see.
func (c *Client) page(response *stdhttp.Response) (dataplane.Page, error) {
	var body pageBody
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return dataplane.Page{}, fmt.Errorf("%w: the data plane answered with a body this facade cannot read", dataplane.ErrUpstreamUnavailable)
	}

	page := dataplane.Page{
		Events:     make([]dataplane.Event, 0, len(body.Events)),
		NextCursor: body.NextCursor,
		HasMore:    body.HasMore,
	}
	for _, event := range body.Events {
		page.Events = append(page.Events, dataplane.Event{
			RequestID:     event.RequestID,
			Kind:          event.Kind,
			SchemaVersion: event.SchemaVersion,
			OccurredAt:    event.OccurredAt,
			Payload:       event.Payload,
		})
	}
	return page, nil
}

// pageBody and eventBody are the wire shapes of the Data Plane's answer,
// mirroring UsageFactPage in api/openapi/shared/usage-facts.yaml field for
// field. They are private to this adapter because they are the one place in
// this module that knows the persistence-side JSON: the port above speaks in
// values, and the inbound adapter owns the JSON this application writes.
//
// OccurredAt is a time.Time so that encoding/json parses the contract's
// RFC 3339 itself. A timestamp the runtime could not write is then a decode
// failure rather than a zero value travelling onward as if it meant something.
type pageBody struct {
	Events     []eventBody `json:"events"`
	NextCursor string      `json:"next_cursor"`
	HasMore    bool        `json:"has_more"`
}

type eventBody struct {
	RequestID     string          `json:"request_id"`
	Kind          string          `json:"kind"`
	SchemaVersion int             `json:"schema_version"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}
