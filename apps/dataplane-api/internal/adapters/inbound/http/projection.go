package http

import (
	"context"
	"errors"
	"io"
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// The three projection operations: GET /internal/projection/position,
// POST /internal/projection/snapshot and POST /internal/projection/changes —
// the façade's contracted half of the private control-to-data protocol
// (docs/architecture/cross-plane-protocols.md, ADR 0007). The Control Plane's
// producer calls these; the Data Plane's listener answers behind them.
//
// This file is the transport's part of one delivery model, and the delivery
// model does its deciding at the two ends: the producer composes the message
// from its change log, the mirror applies it and advances its position in one
// transaction. What is left for the middle hop is transport, and the discipline
// is the usage-events handler's carried in the opposite direction — there the
// answer crossed as values read once; here the *request* crosses as bytes read
// once. The handler does not decode a delivery, does not validate a delivery
// and does not rebuild a delivery: the grammar is the listener's to judge,
// which is why the refusals arrive as its answer and not as this layer's
// opinion. A body read here is handed to the application and written out again
// only as the closed answer shapes below.
//
// What this layer does own is the envelope around the bytes: the caller is
// verified before any of it runs (the route table's wrapper, as everywhere on
// this surface), the body is read under the byte bound the contract declares,
// and the answer is the contract's shapes and statuses and nothing else.

const (
	// projectionPositionPath, projectionSnapshotPath and projectionChangesPath
	// are spelled exactly as api/openapi/dataplane.yaml declares them. The
	// route table pins the spellings against that document, so these constants
	// are the one place they live in code.
	projectionPositionPath = "/internal/projection/position"
	projectionSnapshotPath = "/internal/projection/snapshot"
	projectionChangesPath  = "/internal/projection/changes"

	// maxProjectionBodyBytes is the bound the contract declares on a delivery
	// body (`x-max-body-bytes: 10485760` — the same literal both operations
	// carry), and it is enforced here because a bound is transport: it is what
	// keeps an oversized body from being read into this process at all, on this
	// side of the credential check and before any round trip to the listener.
	// The number is ten mebibytes, sized to hold a full snapshot at the
	// contract's array bounds with room for the additive fields a newer
	// producer may add; raising it is a contract change, not a deployment
	// toggle, which is what pinning it in the OpenAPI document buys.
	maxProjectionBodyBytes = 10_485_760
)

// projectionPositionHandler serves GET /internal/projection/position: where
// the Data Plane's credential mirror stands, crossing unchanged. It takes no
// body, and the listener's answer — including the empty epoch and the zero
// revision of a consumer that has never bootstrapped — is re-issued from the
// port's values, the same treatment the usage-fact page gets.
func projectionPositionHandler(app *application.App) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		position, err := app.ProjectionPosition(r.Context())
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, newProjectionPositionResponse(position))
	}
}

// projectionSnapshotHandler and projectionChangesHandler serve the two
// delivery operations. Both do the same transport work on different paths and
// different application methods, so both are one shared body with the
// application call as the only difference.
func projectionSnapshotHandler(app *application.App) stdhttp.HandlerFunc {
	return projectionDeliveryHandler(app, app.ApplyProjectionSnapshot)
}

func projectionChangesHandler(app *application.App) stdhttp.HandlerFunc {
	return projectionDeliveryHandler(app, app.ApplyProjectionChanges)
}

// projectionDeliveryHandler reads the delivery body under the contract's byte
// bound and hands it to the application as the bytes that arrived.
//
// The read stops at the bound, and an overrun is a 400 about the caller's
// request rather than a 502 about the listener: a body this surface can see is
// impossible should cost the Data Plane nothing at all, the same rule the
// limit check on the usage-fact read applies. Every other read failure — a
// truncated body, a broken chunk — is the same answer, because from the
// caller's side the request was not a message this operation could read.
//
// An empty body is not special-cased. Deciding that an empty delivery "means
// something" would be this process inventing grammar, and the one rule the
// private protocol does not let a middle hop write is grammar: the bytes cross,
// the listener refuses them if they are not a message, and the refusal is
// translated one layer down and arrives here as a normal application error.
func projectionDeliveryHandler(
	app *application.App,
	deliver func(ctx context.Context, message []byte) (dataplane.ProjectionAck, error),
) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		body, err := readProjectionBody(w, r)
		if err != nil {
			writeError(w, r, err)
			return
		}

		ack, err := deliver(r.Context(), body)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, newProjectionAckResponse(ack))
	}
}

// readProjectionBody reads a delivery body under the contract's bound.
//
// The limit reader's error is inspected for *http.MaxBytesError specifically,
// because the two failures deserve different sentences: an oversized body is
// the caller's request being impossible, and anything else is the request
// arriving unreadable. Neither message echoes the body — caller-shaped input
// has no business in a response.
func readProjectionBody(w stdhttp.ResponseWriter, r *stdhttp.Request) ([]byte, error) {
	body, err := io.ReadAll(stdhttp.MaxBytesReader(w, r.Body, maxProjectionBodyBytes))
	if err != nil {
		var tooLarge *stdhttp.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, invalidRequestError{message: "the request body is larger than this operation accepts"}
		}
		return nil, invalidRequestError{message: "the request body is not a message this operation can read"}
	}
	return body, nil
}

// projectionPositionResponse and projectionAckResponse are the wire shapes
// this surface writes, mirroring ProjectionPosition and ProjectionAppliedAck
// in api/openapi/shared/projection.yaml field for field. The port speaks in
// values; serialization stays on this side of the boundary, as it does for the
// page and the error envelope.
type projectionPositionResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	Epoch           string `json:"epoch"`
	Bootstrapped    bool   `json:"bootstrapped"`
	AppliedRevision uint64 `json:"applied_revision"`
}

type projectionAckResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	AppliedRevision uint64 `json:"applied_revision"`
}

// newProjectionPositionResponse and newProjectionAckResponse render the two
// answers. Every value crosses unchanged — the epoch exactly as the listener
// reported it, empty or not; the revision exactly as the mirror holds it —
// because a façade that reformats a position is a façade the producer's cycle
// can no longer trust to speak for the mirror.
func newProjectionPositionResponse(position dataplane.ProjectionPosition) projectionPositionResponse {
	return projectionPositionResponse{
		ProtocolVersion: position.ProtocolVersion,
		Epoch:           position.Epoch,
		Bootstrapped:    position.Bootstrapped,
		AppliedRevision: position.AppliedRevision,
	}
}

func newProjectionAckResponse(ack dataplane.ProjectionAck) projectionAckResponse {
	return projectionAckResponse{
		ProtocolVersion: ack.ProtocolVersion,
		AppliedRevision: ack.AppliedRevision,
	}
}
