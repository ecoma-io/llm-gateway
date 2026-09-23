// Package dataplane adapts the outbound port to the Data Plane's management
// listener over HTTP: the call this façade makes so it has something to answer
// with, and the only network dependency in the module.
//
// The listener it calls is not the surface this application contracts. The
// façade answers a caller under api/openapi/dataplane.yaml; the process behind
// it serves a private protocol, defined in
// docs/architecture/cross-plane-protocols.md, that no caller outside this
// repository reaches and that is deliberately not a fourth OpenAPI document.
// The two hops carry the same page and do not share a failure vocabulary, so
// what this package does with a status is translate it into a classification
// this application can answer from — never relay it.
//
// The adapter is handed an open *http.Client, the listener's origin and the
// service credential rather than reading any of them itself. Which client,
// which address and which secret are deployment decisions made once in
// cmd/dataplane-api; a client this package built would take the process's
// timeouts and proxy configuration out of the composition root's hands, and the
// credential would be a second thing reading the environment.
//
// It carries values across and nothing else. `after` and `limit` reach the
// listener as the parameters the caller sent — the cursor as the opaque string
// it is, never parsed, never trimmed, never remembered — and every field of the
// page has to come back for the caller to get it at all, because this adapter
// reads the answer into the port's values and the inbound adapter writes the
// caller's body from those. So the page is re-issued rather than copied: the
// two bodies coincide today and no line here assumes they must. There is no
// cache and no buffer, and that is the part worth separating from the
// re-encoding: re-writing a page from values this call just read is
// statelessness, while answering from a page read earlier would be this process
// holding a position it does not own, which is the one thing a management
// façade may not do (ADR 0006 §9, §11).
package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	stdhttp "net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// usageEventsPath is the operation this adapter calls on the private listener
// behind this façade, and it is the private protocol's path rather than the
// façade's. The two are the same string on purpose: the façade sends the values
// it was given and names the path itself, rather than handing on the request
// object it received, so the path a caller published is a path the façade calls
// rather than one this hop re-derives. A path rewritten hop by hop would be a
// translation step the protocol would have to describe and could get wrong. It
// is therefore pinned here by
// protocol_test.go and in the listener's own package by its own copy of that
// test, not by api/openapi/dataplane.yaml — that document is this application's
// contract with its caller, and this line is the application's own call out
// (docs/architecture/cross-plane-protocols.md).
//
// It is a constant rather than an argument because this adapter serves one
// operation: the day the façade fronts a second one, that is a new method and a
// visible second path here, not a parameter every call site has to get right.
const usageEventsPath = "/internal/usage-events"

// usageCursorMaxLength is the bound the façade's contract puts on a cursor
// (`shared/usage-facts.yaml`, UsageCursor: `maxLength: 512`), and it is applied
// to what the private listener answers rather than only to what a caller sends.
// The private protocol carries the same bound, so the two agreeing is the
// expected case; the check exists for the case where they do not, and the cost
// of it is one comparison against a body that has already been read. See page
// for why keeping this promise belongs here rather than at the consumer.
//
// Characters and not bytes, because `maxLength` is the number of code points:
// both ends of this hop count the same quantity, so a cursor one of them issues
// is one the other accepts.
const usageCursorMaxLength = 512

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
		// The Data Plane refuses a position it can no longer replay, and this
		// process is forbidden from resuming from a newer one: that would skip
		// the facts between the two, and a skipped fact is unsettled money. So
		// the refusal is carried, and it is *decided* here rather than relayed —
		// the façade builds its own envelope from this classification, and not
		// one byte of the private listener's body reaches the caller. The two
		// hops happen to spell this one the same way, which is a consequence of
		// the translation and not a substitute for it.
		return dataplane.Page{}, fmt.Errorf("%w: the data plane no longer retains that position", dataplane.ErrCursorExpired)
	default:
		// Everything else is one condition from a caller's point of view: the
		// answer is unknown, and this process holds nothing it could answer
		// with instead. That includes the Data Plane refusing *this process's*
		// own credential, which is a deployment fault and not to be reported as
		// the caller's — a caller shown 401 for a secret it never sent would go
		// and rotate the wrong one.
		//
		// The status is named because it is the one fact that tells an operator
		// which side to look at, and a status code is not an address. It is the
		// private listener's status and not a contracted one: nothing here is
		// relayed, so the façade's own answer is chosen above in the inbound
		// adapter from the sentinel, not from this number.
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
//
// A page whose `next_cursor` is missing or over-long is refused for the same
// reason and not a different one, and the reason is worth stating where the
// check sits rather than where it is documented. The façade contracts
// `next_cursor` — required, at least one character, at most 512 — so a body
// that carries none, or carries one longer than the contract's own bound, is a
// body this application cannot write out without contradicting the document it
// answers under. The check is the façade keeping its own promise, which is why
// it is here and not delegated to the consumer: the consumer has its own
// promise to keep, about the position it stores, and one of the two keeping it
// does not excuse the other.
//
// Nothing here reads the cursor. An admissible value crosses byte for byte;
// what is decided is only whether a value this façade may put on the wire
// exists at all.
func (c *Client) page(response *stdhttp.Response) (dataplane.Page, error) {
	var body pageBody
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return dataplane.Page{}, fmt.Errorf("%w: the data plane answered with a body this facade cannot read", dataplane.ErrUpstreamUnavailable)
	}

	switch {
	case body.NextCursor == "":
		return dataplane.Page{}, fmt.Errorf("%w: the data plane answered with a page carrying no next_cursor, which this surface contracts as required", dataplane.ErrUpstreamUnavailable)
	case utf8.RuneCountInString(body.NextCursor) > usageCursorMaxLength:
		return dataplane.Page{}, fmt.Errorf("%w: the data plane answered with a next_cursor of %d characters, past the %d this surface contracts", dataplane.ErrUpstreamUnavailable, utf8.RuneCountInString(body.NextCursor), usageCursorMaxLength)
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

// pageBody and eventBody are the wire shapes of the private listener's answer,
// as docs/architecture/cross-plane-protocols.md defines it: the same fields, in
// the same spellings, as UsageFactPage in api/openapi/shared/usage-facts.yaml.
// The two agree because the page is one page and the hops are two; they are
// written twice because they are two sides of a boundary, and the protocol test
// in this package and its counterpart in the listener's package are what hold
// them together.
//
// They are private to this adapter because they are the one place in this module
// that knows the persistence-side JSON: the port above speaks in values, and the
// inbound adapter owns the JSON this application writes. That separation is what
// makes the façade's answer a re-encoding of what it read rather than a relay of
// it — the two shapes coincide today, and the code does not assume they must.
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
