package management

import (
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
)

// The projection operations: this listener's half of the Control → Data
// credential projection (ADR 0007). The Control Plane's delivery loop is the
// only caller, and the three operations are the whole of its side of the
// protocol — read this plane's position, apply a snapshot, apply a batch —
// with the shapes declared in api/openapi/shared/projection.yaml and the
// private-protocol agreement pinned by protocol_test.go on both hops.
//
// Every operation is authenticated at the route (routes.go) and every failure
// is one of errors.go's, translated from the domain's sentinels by failureFor.
// What a handler here decides is transport only: the body's bytes, the
// envelope, and the one number the contract bounds — the body ceiling below.

// maxProjectionBodyBytes is the ceiling on a delivered body, the same number
// the façade's contract declares as x-max-body-bytes on the snapshot and
// changes operations. A snapshot is atomic and cannot chunk, so the ceiling
// is a contract decision (5000 records per array plus envelope) and not a
// page size; a body past it is refused at this hop before it is decoded, and
// the façade refuses it at its hop first, which is why the number is pinned
// against the document rather than chosen here.
const maxProjectionBodyBytes = 10_485_760

// The wire shapes. They are the contract's, field for field, and protocol_test
// pins the key sets against projection.yaml — which is why nothing here adds,
// drops or renames a field without that test going red. Decoding tolerates
// unknown fields (encoding/json's default): additive fields are the one
// direction the protocol evolves in without a version bump, and a decoder
// that refused them would fail the upgrade ordering the protocol requires.
//
// A field that is missing or null decodes to its zero value and is refused by
// the domain's grammar at the application boundary — never defaulted,
// because a defaulted digest or state would write a credential row that
// cannot verify. Three fields are the exception the wire layer refuses
// itself, because their absence would otherwise decode into a legal value a
// grammar cannot see: a snapshot's boundary (zero is a legitimate boundary)
// and its two arrays (missing and empty are different words the contract
// speaks — a snapshot that states no api_keys has said nothing about keys,
// and an apply is unconditional). They decode through pointers, and the
// handlers refuse the nil form before the application is asked anything.

type projectionPositionResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	Epoch           string `json:"epoch"`
	Bootstrapped    bool   `json:"bootstrapped"`
	AppliedRevision uint64 `json:"applied_revision"`
}

type projectionSnapshotRequest struct {
	ProtocolVersion  int                  `json:"protocol_version"`
	Epoch            string               `json:"epoch"`
	SnapshotRevision *uint64              `json:"snapshot_revision"`
	APIKeys          *[]projectionAPIKey  `json:"api_keys"`
	Accounts         *[]projectionAccount `json:"accounts"`
}

type projectionAPIKey struct {
	KeyID     string     `json:"key_id"`
	AccountID string     `json:"account_id"`
	Digest    string     `json:"digest"`
	State     string     `json:"state"`
	RevokedAt *time.Time `json:"revoked_at"`
}

type projectionAccount struct {
	AccountID string `json:"account_id"`
	State     string `json:"state"`
}

type projectionChangesRequest struct {
	ProtocolVersion int                `json:"protocol_version"`
	Epoch           string             `json:"epoch"`
	FromRevision    uint64             `json:"from_revision"`
	Changes         []projectionChange `json:"changes"`
}

type projectionChange struct {
	Revision     uint64          `json:"revision"`
	ResourceKind string          `json:"resource_kind"`
	ResourceID   string          `json:"resource_id"`
	RecordedAt   time.Time       `json:"recorded_at"`
	Payload      json.RawMessage `json:"payload"`
}

type projectionAckResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	AppliedRevision uint64 `json:"applied_revision"`
}

// projectionPosition serves GET /internal/projection/position: this plane's
// own fact about itself. There is no body and nothing to validate; the answer
// is the position and the protocol version this plane speaks, so a producer
// that has been upgraded ahead of this plane learns it here, from a readable
// answer, rather than from a refusal of its first delivery.
func projectionPosition(app *application.App) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		position, err := app.ProjectionPosition(r.Context())
		if err != nil {
			writeFailure(w, r, failureFor(err))
			return
		}
		writeJSON(w, stdhttp.StatusOK, projectionPositionResponse{
			ProtocolVersion: projection.ProtocolVersion,
			Epoch:           position.Epoch,
			Bootstrapped:    position.Bootstrapped,
			AppliedRevision: position.AppliedRevision,
		})
	}
}

// projectionSnapshot serves POST /internal/projection/snapshot. The apply is
// the application's to make; the handler's whole judgement is whether the
// bytes are a body this operation accepts at all.
func projectionSnapshot(app *application.App) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		var request projectionSnapshotRequest
		if !decodeProjectionBody(w, r, &request) {
			return
		}
		// Presence is this hop's own check, for the reason the wire shapes
		// document: an absent boundary or an absent array would decode into
		// the legal empty form, and an unconditional apply must never be
		// handed a message that omitted half of itself.
		if request.SnapshotRevision == nil || request.APIKeys == nil || request.Accounts == nil {
			writeFailure(w, r, invalidRequestFailure("the snapshot must state its boundary and both of its projections, empty or not"))
			return
		}

		snapshot := projection.Snapshot{
			ProtocolVersion:  request.ProtocolVersion,
			Epoch:            request.Epoch,
			SnapshotRevision: *request.SnapshotRevision,
		}
		for _, wire := range *request.APIKeys {
			snapshot.APIKeys = append(snapshot.APIKeys, projection.APIKeyCredential{
				KeyID:     wire.KeyID,
				AccountID: wire.AccountID,
				Digest:    wire.Digest,
				State:     projection.CredentialState(wire.State),
				RevokedAt: wire.RevokedAt,
			})
		}
		for _, wire := range *request.Accounts {
			snapshot.Accounts = append(snapshot.Accounts, projection.AccountState{
				AccountID: wire.AccountID,
				State:     projection.AccountLifecycle(wire.State),
			})
		}

		applied, err := app.ApplyProjectionSnapshot(r.Context(), snapshot)
		if err != nil {
			writeFailure(w, r, failureFor(err))
			return
		}
		writeJSON(w, stdhttp.StatusOK, projectionAckResponse{ProtocolVersion: projection.ProtocolVersion, AppliedRevision: applied})
	}
}

// projectionChanges serves POST /internal/projection/changes. Each entry's
// payload is parsed into a domain change here — that is where a payload
// missing its digest, or naming a state this plane does not mirror, is
// refused — and the assembled batch is judged against the stored position by
// the application, whole or not at all.
func projectionChanges(app *application.App) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		var request projectionChangesRequest
		if !decodeProjectionBody(w, r, &request) {
			return
		}
		// The version is judged before any entry is parsed, so a batch that
		// speaks a version this plane does not know is answered
		// unsupported_version even when its entries are also malformed — the
		// diagnosis must not depend on where else the message is broken. The
		// domain judges the same field again inside the application's
		// validation; this early answer is the wire's courtesy to a producer
		// reading a refusal.
		if request.ProtocolVersion != projection.ProtocolVersion {
			writeFailure(w, r, failureFor(fmt.Errorf("%w: the batch speaks protocol version %d, this plane speaks %d",
				projection.ErrUnsupportedVersion, request.ProtocolVersion, projection.ProtocolVersion)))
			return
		}

		batch := projection.Batch{
			ProtocolVersion: request.ProtocolVersion,
			Epoch:           request.Epoch,
			FromRevision:    request.FromRevision,
		}
		for _, wire := range request.Changes {
			change, err := projection.NewChange(wire.Revision,
				projection.ResourceKind(wire.ResourceKind), wire.ResourceID,
				wire.RecordedAt, wire.Payload)
			if err != nil {
				writeFailure(w, r, failureFor(err))
				return
			}
			batch.Changes = append(batch.Changes, change)
		}

		applied, err := app.ApplyProjectionChanges(r.Context(), batch)
		if err != nil {
			writeFailure(w, r, failureFor(err))
			return
		}
		writeJSON(w, stdhttp.StatusOK, projectionAckResponse{ProtocolVersion: projection.ProtocolVersion, AppliedRevision: applied})
	}
}

// decodeProjectionBody reads and decodes a delivered body under the contract's
// ceiling, answering the failure itself when the body is refused and telling
// the caller whether to carry on.
//
// The refusals are deliberately undiscriminating in what they echo: a body
// that is too large and a body that is not JSON get fixed sentences with no
// excerpt and no byte count beyond the declared ceiling, because a request
// body is attacker-controlled input and this surface is the one place the
// mirror's rows come from.
func decodeProjectionBody(w stdhttp.ResponseWriter, r *stdhttp.Request, into any) bool {
	r.Body = stdhttp.MaxBytesReader(w, r.Body, maxProjectionBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		var tooLarge *stdhttp.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			writeFailure(w, r, invalidRequestFailure("the request body is larger than this operation accepts"))
		default:
			writeFailure(w, r, invalidRequestFailure("the request body is not a message this operation can read"))
		}
		return false
	}
	return true
}
