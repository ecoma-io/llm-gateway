package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	stdhttp "net/http"
	"strings"

	// The port shares this package's name, so the import is aliased — the
	// same convention the usage-fact half of the client keeps in dataplane.go.
	port "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
)

// The three operations of the projection protocol this adapter reaches, as
// the façade's contract declares them. They are constants because they are
// the contract's, not the caller's: nothing in the Control Plane chooses
// where the mirror's management face lives. A protocol test in this package
// pins each against api/openapi/dataplane.yaml, and the counterpart module's
// tests pin the same three paths from the listening side — the two ends of
// the hop cannot import each other, so each names the strings and each fails
// when its own side moves.
const (
	projectionPositionPath = "/internal/projection/position"
	projectionSnapshotPath = "/internal/projection/snapshot"
	projectionChangesPath  = "/internal/projection/changes"
)

// projectionProtocolVersion is the version of the projection protocol this
// client speaks, and the only value it will accept in an answer's
// `protocol_version`. The version set is closed and every hop fails closed
// against a version it does not know (ADR 0007): an answer claiming another
// version is not an answer this process can act on, however plausible its
// remaining fields look — a position earned under a different protocol is
// exactly the thing the version field exists to catch. A test pins the
// constant to the contract's `const: 1`.
const projectionProtocolVersion = 1

// Compile-time proof that the adapter satisfies the port it claims to, so a
// signature drifting from the seam is a build failure rather than a discovery
// at the composition root.
var _ port.Projection = (*Client)(nil)

// ProjectionPosition reads the consumer's position through the façade. The
// request carries nothing but the credential: the position is the consumer's
// own fact and this call has no parameters to argue about.
//
// Every field the contract marks required is refused when absent, and the
// version is refused when it is not the one this build speaks — the two
// checks a producer may make about an answer without taking over any of the
// consumer's judgements. What the fields MEAN is the delivery loop's to
// interpret: this adapter hands the answer up verbatim, including the empty
// epoch and zero revision of a consumer that has never bootstrapped, which
// are data and not defects.
func (c *Client) ProjectionPosition(ctx context.Context) (port.ProjectionPosition, error) {
	request, err := c.request(ctx, stdhttp.MethodGet, projectionPositionPath, nil)
	if err != nil {
		return port.ProjectionPosition{}, err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return port.ProjectionPosition{}, fmt.Errorf("%w: the position read did not complete", port.ErrProjectionUnavailable)
	}
	defer func() { _ = response.Body.Close() }()

	switch response.StatusCode {
	case stdhttp.StatusOK:
		var body positionResponse
		if err := json.NewDecoder(io.LimitReader(response.Body, projectionAnswerLimit)).Decode(&body); err != nil {
			return port.ProjectionPosition{}, fmt.Errorf("%w: the body would not decode as a position", port.ErrProjectionShape)
		}
		return body.position()
	case stdhttp.StatusBadRequest, stdhttp.StatusConflict:
		if code, ok := refusalCode(response.Body); ok {
			if sentinel := projectionRefusalSentinel(code); sentinel != nil {
				return port.ProjectionPosition{}, sentinel
			}
		}
		return port.ProjectionPosition{}, fmt.Errorf("%w: the position read was refused with status %d", port.ErrProjectionUnavailable, response.StatusCode)
	default:
		return port.ProjectionPosition{}, fmt.Errorf("%w: the position read answered status %d", port.ErrProjectionUnavailable, response.StatusCode)
	}
}

// DeliverSnapshot delivers one bootstrap message through the façade and
// returns the consumer's acknowledgement.
//
// The message crosses byte for byte: it is the projection domain's grammar,
// built and validated before this method was called, and a transport that
// re-encoded it would be a second grammar with a second opinion — and one
// that would strip exactly the unknown additive fields the protocol's
// evolution rule promises to carry. What this method does decide is the
// answer: a refusal is classified into the port's sentinels, because the
// delivery loop's next move is a decision about which refusal it met, and a
// classification is the one thing a transport may add to what it read.
func (c *Client) DeliverSnapshot(ctx context.Context, message []byte) (port.ProjectionAck, error) {
	return c.deliver(ctx, projectionSnapshotPath, message)
}

// DeliverChanges delivers one incremental batch, with the same
// byte-for-byte discipline and the same refusal classification as its
// snapshot twin. A batch delivered before whose acknowledgement was lost is
// answered here without work — the consumer's three-case rule sees it wholly
// behind its position — so at-least-once redelivery costs one round trip and
// nothing durable.
func (c *Client) DeliverChanges(ctx context.Context, message []byte) (port.ProjectionAck, error) {
	return c.deliver(ctx, projectionChangesPath, message)
}

// deliver is the POST half shared by the two deliveries: the message on the
// wire verbatim, the answer decoded into the port's vocabulary or refused
// into one of its sentinels.
func (c *Client) deliver(ctx context.Context, path string, message []byte) (port.ProjectionAck, error) {
	request, err := c.request(ctx, stdhttp.MethodPost, path, message)
	if err != nil {
		return port.ProjectionAck{}, err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return port.ProjectionAck{}, fmt.Errorf("%w: the delivery did not complete", port.ErrProjectionUnavailable)
	}
	defer func() { _ = response.Body.Close() }()

	switch response.StatusCode {
	case stdhttp.StatusOK:
		var body ackResponse
		if err := json.NewDecoder(io.LimitReader(response.Body, projectionAnswerLimit)).Decode(&body); err != nil {
			return port.ProjectionAck{}, fmt.Errorf("%w: the body would not decode as an acknowledgement", port.ErrProjectionShape)
		}
		return body.acknowledgement()
	case stdhttp.StatusBadRequest, stdhttp.StatusConflict:
		if code, ok := refusalCode(response.Body); ok {
			if sentinel := projectionRefusalSentinel(code); sentinel != nil {
				return port.ProjectionAck{}, sentinel
			}
		}
		return port.ProjectionAck{}, fmt.Errorf("%w: the delivery was refused with status %d", port.ErrProjectionUnavailable, response.StatusCode)
	default:
		return port.ProjectionAck{}, fmt.Errorf("%w: the delivery answered status %d", port.ErrProjectionUnavailable, response.StatusCode)
	}
}

// request builds one call to the façade: the operation's path joined to the
// base URL by copy-and-set, the credential on the authorization header, and
// — for a delivery — the message as the body, verbatim. Transport failures
// come back as ErrProjectionUnavailable with the cause deliberately dropped:
// net/http wraps them in a *url.Error that quotes the full request URL,
// which names the endpoint and, through it, the deployment this process
// talks to — a fact for operators, and therefore not a fact for an error
// that a delivery loop will log every few seconds until it stops being true.
func (c *Client) request(ctx context.Context, method, path string, body []byte) (*stdhttp.Request, error) {
	requestURL := c.endpoint
	requestURL.Path += path

	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	request, err := stdhttp.NewRequestWithContext(ctx, method, requestURL.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("%w: the %s request could not be built", port.ErrProjectionUnavailable, method)
	}
	request.Header.Set("Authorization", "Bearer "+c.credential)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

// positionResponse is one ProjectionPosition on the wire, field for field as
// the fragment declares it. Every field the contract marks required is a
// pointer: with a plain value, encoding/json answers a body that omits the
// field with the Go zero value, and a position that "read" as unbootstrapped
// because its epoch field was missing would send the producer into a
// re-snapshot it had no evidence for — the one answer on this hop whose
// absence is indistinguishable, byte for byte, from a real one. With a
// pointer, both shapes land on a nil, which position below refuses.
//
// What is checked is presence and the version, and nothing else: the epoch's
// emptiness, the bootstrapped flag and the revision's zero are the
// consumer's own facts, and the delivery loop is the code that knows what
// they mean.
type positionResponse struct {
	ProtocolVersion *int    `json:"protocol_version"`
	Epoch           *string `json:"epoch"`
	Bootstrapped    *bool   `json:"bootstrapped"`
	AppliedRevision *uint64 `json:"applied_revision"`
}

// position refuses the ways an answer can fail to be a position at all,
// then translates the rest verbatim.
func (r positionResponse) position() (port.ProjectionPosition, error) {
	switch {
	case r.ProtocolVersion == nil:
		return port.ProjectionPosition{}, fmt.Errorf("%w: the position carried no protocol_version, which the contract requires", port.ErrProjectionShape)
	case *r.ProtocolVersion != projectionProtocolVersion:
		return port.ProjectionPosition{}, fmt.Errorf("%w: the position speaks protocol version %d, this process speaks %d", port.ErrProjectionUnsupportedVersion, *r.ProtocolVersion, projectionProtocolVersion)
	case r.Epoch == nil:
		return port.ProjectionPosition{}, fmt.Errorf("%w: the position carried no epoch, which the contract requires", port.ErrProjectionShape)
	case r.Bootstrapped == nil:
		return port.ProjectionPosition{}, fmt.Errorf("%w: the position carried no bootstrapped field, which the contract requires", port.ErrProjectionShape)
	case r.AppliedRevision == nil:
		return port.ProjectionPosition{}, fmt.Errorf("%w: the position carried no applied_revision, which the contract requires", port.ErrProjectionShape)
	}
	return port.ProjectionPosition{
		ProtocolVersion: *r.ProtocolVersion,
		Epoch:           *r.Epoch,
		Bootstrapped:    *r.Bootstrapped,
		AppliedRevision: *r.AppliedRevision,
	}, nil
}

// ackResponse is one ProjectionAppliedAck on the wire, with the same
// pointer-required discipline as positionResponse: an acknowledgement whose
// applied_revision was missing would read as zero, and zero is a position —
// the unbootstrapped one — so an absent field must be a refusal, not a
// position this process cannot tell apart from a real one.
type ackResponse struct {
	ProtocolVersion *int    `json:"protocol_version"`
	AppliedRevision *uint64 `json:"applied_revision"`
}

// acknowledgement refuses the ways an answer can fail to be an
// acknowledgement, then translates the rest verbatim.
func (r ackResponse) acknowledgement() (port.ProjectionAck, error) {
	switch {
	case r.ProtocolVersion == nil:
		return port.ProjectionAck{}, fmt.Errorf("%w: the acknowledgement carried no protocol_version, which the contract requires", port.ErrProjectionShape)
	case *r.ProtocolVersion != projectionProtocolVersion:
		return port.ProjectionAck{}, fmt.Errorf("%w: the acknowledgement speaks protocol version %d, this process speaks %d", port.ErrProjectionUnsupportedVersion, *r.ProtocolVersion, projectionProtocolVersion)
	case r.AppliedRevision == nil:
		return port.ProjectionAck{}, fmt.Errorf("%w: the acknowledgement carried no applied_revision, which the contract requires", port.ErrProjectionShape)
	}
	return port.ProjectionAck{
		ProtocolVersion: *r.ProtocolVersion,
		AppliedRevision: *r.AppliedRevision,
	}, nil
}

// projectionRefusalSentinel maps a refusal code this protocol names to the
// sentinel the delivery loop acts on, or nil for a code it does not. The
// four codes are the private protocol's own refusal vocabulary, and the
// mapping is the adapter's one opinion: everything else — a code this build
// predates, a body that would not decode — is an answer whose meaning is
// unknown, which is ErrProjectionUnavailable and a cycle that runs again.
func projectionRefusalSentinel(code string) error {
	switch code {
	case "unsupported_version":
		return port.ErrProjectionUnsupportedVersion
	case "invalid_request":
		return port.ErrProjectionShape
	case "revision_gap":
		return port.ErrProjectionGap
	case "snapshot_required":
		return port.ErrProjectionSnapshotRequired
	default:
		return nil
	}
}

// projectionAnswerLimit bounds what this adapter reads of any one answer —
// success or refusal. A legal answer on this hop is a few hundred bytes: a
// position, an acknowledgement, a refusal envelope. The bound is not a belief
// that peers are well behaved, it is what keeps a misbehaving one from
// choosing this process's memory ceiling: a body that cannot state itself
// inside the bound does not decode, and the answer fails closed as a shape
// this process refuses. The same bound caps the refusal read below, which is
// why the limit lives here once.
const projectionAnswerLimit = 1 << 16

// refusalCode reads the refusal envelope's `error.code` and nothing else.
// The bodies on this hop are the façade's own vocabulary, closed and small,
// and the only field a caller can act on is the code — so the only field
// this function reads is the code. Everything else in the body stays
// unread: no message text, no request id, nothing that could turn a
// peer's words into this process's error and leak them into a log line
// the delivery loop writes every cycle.
func refusalCode(body io.Reader) (string, bool) {
	raw, err := io.ReadAll(io.LimitReader(body, projectionAnswerLimit))
	if err != nil {
		return "", false
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", false
	}
	if envelope.Error.Code == "" {
		return "", false
	}
	return envelope.Error.Code, true
}
