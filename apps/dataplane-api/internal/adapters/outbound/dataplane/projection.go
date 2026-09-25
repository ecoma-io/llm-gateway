package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// The projection operations of the private listener, as this adapter calls
// them. The paths are the private protocol's own and the same strings the
// listener serves — pinned on both sides by the two protocol tests, neither of
// which can import the other (ADR 0006 §1). They are constants rather than
// arguments for the reason usageEventsPath is: this adapter serves named
// operations, not a caller-chosen one, and a path that could be renamed at a
// call site is a path nobody agreed to.
const (
	projectionPositionPath = "/internal/projection/position"
	projectionSnapshotPath = "/internal/projection/snapshot"
	projectionChangesPath  = "/internal/projection/changes"
)

// The listener's refusal codes, as the private protocol's envelope spells
// them. They are literals rather than imports for the reason every constant on
// a wire boundary is: the definition is the document
// (docs/architecture/cross-plane-protocols.md), and a constant shared across
// two modules would be a second definition wearing a name. The four below are
// the ones this adapter translates into sentinels; any other code is a
// condition this process cannot classify, and is refused as unavailable
// instead of guessed at.
const (
	refusalUnsupportedVersion = "unsupported_version"
	refusalInvalidRequest     = "invalid_request"
	refusalRevisionGap        = "revision_gap"
	refusalSnapshotRequired   = "snapshot_required"
)

// Compile-time proof that the adapter satisfies the write half of the port,
// beside the read half's proof in dataplane.go. A method rename on either side
// is a build failure here rather than a runtime surprise at the first
// projection cycle.
var _ dataplane.ProjectionDelivery = (*Client)(nil)

// ProjectionPosition implements the port's read: where the Data Plane's mirror
// stands. See dataplane.ProjectionPosition for the contract; what this
// implementation adds is the wire.
func (c *Client) ProjectionPosition(ctx context.Context) (dataplane.ProjectionPosition, error) {
	request, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, c.baseURL+projectionPositionPath, nil)
	if err != nil {
		return dataplane.ProjectionPosition{}, fmt.Errorf("%w: the management request could not be built", dataplane.ErrUpstreamUnavailable)
	}
	request.Header.Set("Authorization", "Bearer "+c.credential)
	return c.position(c.do(request))
}

// ApplyProjectionSnapshot implements the port. The message is forwarded as the
// bytes that arrived — see dataplane.ProjectionDelivery for why re-encoding it
// here would be a defect rather than a service.
func (c *Client) ApplyProjectionSnapshot(ctx context.Context, message []byte) (dataplane.ProjectionAck, error) {
	return c.deliver(ctx, projectionSnapshotPath, message)
}

// ApplyProjectionChanges implements the port, under the same terms as the
// snapshot delivery above.
func (c *Client) ApplyProjectionChanges(ctx context.Context, message []byte) (dataplane.ProjectionAck, error) {
	return c.deliver(ctx, projectionChangesPath, message)
}

// deliver is the one POST this adapter makes, twice. The message crosses
// verbatim — no decode, no re-encode, no field dropped — with the credential
// presenting this process as the service the listener accepts, and the answer
// is classified into the port's vocabulary, never relayed.
//
// The four refusals are named by the listener's envelope code, because that
// code is the private protocol's own word for which decision the producer's
// loop must take; the status alone cannot carry four answers. A refusal whose
// code this adapter does not translate, a status it does not expect, and a
// body it cannot read are all the same condition from the caller's point of
// view: the answer is unknown, and ErrUpstreamUnavailable says so rather than
// inventing a classification.
func (c *Client) deliver(ctx context.Context, path string, message []byte) (dataplane.ProjectionAck, error) {
	request, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, c.baseURL+path, bytes.NewReader(message))
	if err != nil {
		return dataplane.ProjectionAck{}, fmt.Errorf("%w: the management request could not be built", dataplane.ErrUpstreamUnavailable)
	}
	request.Header.Set("Authorization", "Bearer "+c.credential)
	request.Header.Set("Content-Type", "application/json")
	return c.acknowledgement(c.do(request))
}

// do sends one built request and returns the response, or the unavailable
// sentinel. The transport failure's cause is deliberately dropped, for the
// reason dataplane.go's read documents in full: net/http builds the text
// around the request URL, the URL names the deployment's private topology, and
// what a caller can act on is the classification.
func (c *Client) do(request *stdhttp.Request) (*stdhttp.Response, error) {
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: the management call did not complete", dataplane.ErrUpstreamUnavailable)
	}
	return response, nil
}

// position reads a position answer into the port's vocabulary.
//
// Every field the private protocol marks required is a pointer, and a body
// that omits one — or carries it as null — is refused, for the reason the
// usage-fact page's read documents: with a plain value, encoding/json answers
// a missing applied_revision with zero and a missing bootstrapped with false,
// and that decoded position would read as "never bootstrapped, nothing
// applied" — a fact about the mirror this process would have invented. The
// epoch's emptiness is data, not absence: the unbootstrapped listener reports
// an empty epoch, so presence is what the pointer checks and emptiness crosses.
func (c *Client) position(response *stdhttp.Response, err error) (dataplane.ProjectionPosition, error) {
	if err != nil {
		return dataplane.ProjectionPosition{}, err
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != stdhttp.StatusOK {
		return dataplane.ProjectionPosition{}, fmt.Errorf("%w: the data plane answered %d", dataplane.ErrUpstreamUnavailable, response.StatusCode)
	}
	var body positionBody
	if err := json.NewDecoder(io.LimitReader(response.Body, projectionAnswerLimit)).Decode(&body); err != nil {
		return dataplane.ProjectionPosition{}, fmt.Errorf("%w: the data plane answered with a body this facade cannot read", dataplane.ErrUpstreamUnavailable)
	}
	switch {
	case body.ProtocolVersion == nil:
		return dataplane.ProjectionPosition{}, fmt.Errorf("%w: the data plane answered with a position carrying no protocol_version, which the private protocol marks required", dataplane.ErrUpstreamUnavailable)
	case body.Epoch == nil:
		return dataplane.ProjectionPosition{}, fmt.Errorf("%w: the data plane answered with a position carrying no epoch, which the private protocol marks required", dataplane.ErrUpstreamUnavailable)
	case body.Bootstrapped == nil:
		return dataplane.ProjectionPosition{}, fmt.Errorf("%w: the data plane answered with a position carrying no bootstrapped, which the private protocol marks required", dataplane.ErrUpstreamUnavailable)
	case body.AppliedRevision == nil:
		return dataplane.ProjectionPosition{}, fmt.Errorf("%w: the data plane answered with a position carrying no applied_revision, which the private protocol marks required", dataplane.ErrUpstreamUnavailable)
	}
	return dataplane.ProjectionPosition{
		ProtocolVersion: *body.ProtocolVersion,
		Epoch:           *body.Epoch,
		Bootstrapped:    *body.Bootstrapped,
		AppliedRevision: *body.AppliedRevision,
	}, nil
}

// acknowledgement reads a delivery answer, or translates the listener's
// refusal into the port's vocabulary. The classification is the whole answer:
// the listener's envelope is read for its code and nothing else, so no word of
// the private listener's body can reach the producer — the façade's own
// surface, one hop out, builds the caller-facing refusal from the sentinel
// alone.
func (c *Client) acknowledgement(response *stdhttp.Response, err error) (dataplane.ProjectionAck, error) {
	if err != nil {
		return dataplane.ProjectionAck{}, err
	}
	defer func() { _ = response.Body.Close() }()

	switch response.StatusCode {
	case stdhttp.StatusOK:
		var body ackBody
		if err := json.NewDecoder(io.LimitReader(response.Body, projectionAnswerLimit)).Decode(&body); err != nil {
			return dataplane.ProjectionAck{}, fmt.Errorf("%w: the data plane answered with a body this facade cannot read", dataplane.ErrUpstreamUnavailable)
		}
		switch {
		case body.ProtocolVersion == nil:
			return dataplane.ProjectionAck{}, fmt.Errorf("%w: the data plane answered with an acknowledgement carrying no protocol_version, which the private protocol marks required", dataplane.ErrUpstreamUnavailable)
		case body.AppliedRevision == nil:
			return dataplane.ProjectionAck{}, fmt.Errorf("%w: the data plane answered with an acknowledgement carrying no applied_revision, which the private protocol marks required", dataplane.ErrUpstreamUnavailable)
		}
		return dataplane.ProjectionAck{ProtocolVersion: *body.ProtocolVersion, AppliedRevision: *body.AppliedRevision}, nil

	case stdhttp.StatusBadRequest, stdhttp.StatusConflict:
		code, readable := refusalCode(response.Body)
		if !readable {
			return dataplane.ProjectionAck{}, fmt.Errorf("%w: the data plane answered %d with a refusal this facade cannot read", dataplane.ErrUpstreamUnavailable, response.StatusCode)
		}
		switch code {
		case refusalUnsupportedVersion:
			return dataplane.ProjectionAck{}, fmt.Errorf("%w: the delivery spoke a version the data plane does not support", dataplane.ErrProjectionUnsupportedVersion)
		case refusalInvalidRequest:
			return dataplane.ProjectionAck{}, fmt.Errorf("%w: the delivery did not satisfy the protocol's grammar", dataplane.ErrProjectionShape)
		case refusalRevisionGap:
			return dataplane.ProjectionAck{}, fmt.Errorf("%w: the delivered batch does not join the data plane's position", dataplane.ErrProjectionGap)
		case refusalSnapshotRequired:
			return dataplane.ProjectionAck{}, fmt.Errorf("%w: the data plane's position cannot join this timeline", dataplane.ErrProjectionSnapshotRequired)
		default:
			return dataplane.ProjectionAck{}, fmt.Errorf("%w: the data plane answered %d with a refusal this facade cannot classify", dataplane.ErrUpstreamUnavailable, response.StatusCode)
		}

	default:
		// The status is named because it is the one fact that tells an operator
		// which side to look at; it is the private listener's status and not a
		// contracted one. A caller-facing status is chosen from the sentinel,
		// one hop out, never from this number.
		return dataplane.ProjectionAck{}, fmt.Errorf("%w: the data plane answered %d", dataplane.ErrUpstreamUnavailable, response.StatusCode)
	}
}

// refusalCode reads the one field a refusal classification needs: the code
// inside the listener's envelope. Everything else about the body — its
// message, its request id, whether it is even the envelope at all — is
// irrelevant to the classification and unread for it, which is what keeps
// "translate, do not relay" from being a discipline into a hope.
func refusalCode(body io.Reader) (string, bool) {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(body, projectionAnswerLimit)).Decode(&envelope); err != nil {
		return "", false
	}
	if envelope.Error.Code == "" {
		return "", false
	}
	return envelope.Error.Code, true
}

// projectionAnswerLimit bounds what this adapter reads of any one answer from
// the private listener — a position, an acknowledgement or a refusal. A legal
// answer on this hop is a few hundred bytes, so the bound is not a statement
// about the listener but about what any answerer can make this process hold:
// a body that cannot state itself inside the bound does not decode, and the
// answer is refused as unread rather than buffered to whatever size the other
// end chose. The same limit is why the refusals read for their code stay
// capped too.
const projectionAnswerLimit = 1 << 16

// positionBody and ackBody are the wire shapes of the private listener's
// answers, as docs/architecture/cross-plane-protocols.md defines them: the
// same fields, in the same spellings, as ProjectionPosition and
// ProjectionAppliedAck in api/openapi/shared/projection.yaml. They are written
// here and in the listener's package rather than shared, because the
// applications are separate Go modules and each side's protocol test is what
// holds them to the same document.
//
// Every required field is a pointer; the comment on position above carries the
// full argument, and it is the usage-fact page's argument restated on a shape
// whose zero values would lie harder: a position that decoded into
// "unbootstrapped at zero" is a snapshot request, and a middle hop that
// answers it from a decode failure has just told the producer to reset a
// mirror that was fine.
type positionBody struct {
	ProtocolVersion *int    `json:"protocol_version"`
	Epoch           *string `json:"epoch"`
	Bootstrapped    *bool   `json:"bootstrapped"`
	AppliedRevision *uint64 `json:"applied_revision"`
}

type ackBody struct {
	ProtocolVersion *int    `json:"protocol_version"`
	AppliedRevision *uint64 `json:"applied_revision"`
}
