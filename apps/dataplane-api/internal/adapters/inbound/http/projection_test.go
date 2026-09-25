package http

import (
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// projectionApp wires a handler over a fake the test owns, so the assertions
// can read what crossed the boundary — which port ran, with which bytes —
// rather than trusting the response alone.
func projectionApp(t *testing.T, port *fakeProjection) stdhttp.Handler {
	t.Helper()
	return testHandler(application.New("test", &fakeUsageFacts{}, &fakeCatalog{}, port))
}

// projectionRequest issues an authenticated request the way the Control
// Plane's producer would: bearer credential, and for the deliveries a JSON
// body.
func projectionRequest(t *testing.T, handler stdhttp.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testCredential)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	return rec
}

// TestThePositionServedIsTheListenerS pins the two position answers byte for
// byte, in both states a mirror can be in. The bytes matter because the
// producer's cycle decides from this document: a field added, reordered out of
// the contract's names or re-spelled here is a change to a protocol this
// surface only fronts.
//
// The unbootstrapped row is the sharp one. An empty epoch and a zero revision
// are the listener's own answer — its instruction to offer a snapshot — and
// the test holds the surface to serving them rather than to substituting
// something more helpful.
func TestThePositionServedIsTheListenerS(t *testing.T) {
	tests := []struct {
		name      string
		position  dataplane.ProjectionPosition
		wantBytes string
	}{
		{
			name:      "a mirror that bootstrapped and applied twelve revisions",
			position:  dataplane.ProjectionPosition{ProtocolVersion: 1, Epoch: "5d6f0a24-3b1e-4c2a-9f8e-7a1b2c3d4e5f", Bootstrapped: true, AppliedRevision: 12},
			wantBytes: `{"protocol_version":1,"epoch":"5d6f0a24-3b1e-4c2a-9f8e-7a1b2c3d4e5f","bootstrapped":true,"applied_revision":12}` + "\n",
		},
		{
			name:      "a mirror that never bootstrapped",
			position:  dataplane.ProjectionPosition{ProtocolVersion: 1, Epoch: "", Bootstrapped: false, AppliedRevision: 0},
			wantBytes: `{"protocol_version":1,"epoch":"","bootstrapped":false,"applied_revision":0}` + "\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := &fakeProjection{position: tt.position}
			handler := projectionApp(t, port)

			rec := projectionRequest(t, handler, stdhttp.MethodGet, projectionPositionPath, "")
			if rec.Code != stdhttp.StatusOK {
				t.Fatalf("GET answered %d, want %d (body: %s)", rec.Code, stdhttp.StatusOK, rec.Body.String())
			}
			if got := rec.Body.String(); got != tt.wantBytes {
				t.Errorf("the body is %q, want %q", got, tt.wantBytes)
			}
		})
	}
}

// TestADeliveryForwardsTheMessageAndServesTheAcknowledgement is the transport's
// whole contract on the write path, asserted on both ends: the body the
// listener's façade received is the bytes the producer sent — unknown fields,
// HTML characters, multi-byte words and all — and the answer written back is
// the contract's shape and nothing else.
func TestADeliveryForwardsTheMessageAndServesTheAcknowledgement(t *testing.T) {
	const message = `{"protocol_version":1,"epoch":"5d6f0a24-3b1e-4c2a-9f8e-7a1b2c3d4e5f","from_revision":4,"changes":[{"revision":5,"resource_kind":"api_key","resource_id":"0c9f1a58-2f8e-4b3d-8a7c-6e5d4c3b2a1f","recorded_at":"2026-09-25T09:00:00Z","payload":{"digest":"a1b2&<>c3","future_field":"győr — 👍"}}]}`

	tests := []struct {
		name string
		path string
	}{
		{name: "the snapshot delivery", path: projectionSnapshotPath},
		{name: "the changes delivery", path: projectionChangesPath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := &fakeProjection{ack: dataplane.ProjectionAck{ProtocolVersion: 1, AppliedRevision: 12}}
			handler := projectionApp(t, port)

			rec := projectionRequest(t, handler, stdhttp.MethodPost, tt.path, message)
			if rec.Code != stdhttp.StatusOK {
				t.Fatalf("POST answered %d, want %d (body: %s)", rec.Code, stdhttp.StatusOK, rec.Body.String())
			}

			if len(port.calls) != 1 {
				t.Fatalf("the port received %d deliveries, want 1", len(port.calls))
			}
			if port.calls[0].message != message {
				t.Errorf("the message did not cross byte for byte:\n got %q\nwant %q", port.calls[0].message, message)
			}

			wantBytes := `{"protocol_version":1,"applied_revision":12}` + "\n"
			if got := rec.Body.String(); got != wantBytes {
				t.Errorf("the body is %q, want %q", got, wantBytes)
			}
		})
	}
}

// TestAnOversizedDeliveryIsRefusedHereWithoutReachingTheListener pins the body
// bound: a delivery larger than the contract declares is a 400 about the
// caller's request, refused on this side of the credential check's work and
// before the listener hears of it. The row at the bound is the other half —
// a body of exactly the declared size is the contract's own shape and must
// arrive whole.
func TestAnOversizedDeliveryIsRefusedHereWithoutReachingTheListener(t *testing.T) {
	t.Run("a body past the bound is refused as the caller's problem", func(t *testing.T) {
		port := &fakeProjection{}
		handler := projectionApp(t, port)

		oversized := `{"protocol_version":1,"padding":"` + strings.Repeat("x", maxProjectionBodyBytes) + `"}`
		rec := projectionRequest(t, handler, stdhttp.MethodPost, projectionChangesPath, oversized)

		if rec.Code != stdhttp.StatusBadRequest {
			t.Fatalf("the oversized delivery answered %d, want %d (body: %s)", rec.Code, stdhttp.StatusBadRequest, rec.Body.String())
		}
		if len(port.calls) != 0 {
			t.Errorf("the listener received %d deliveries; a body this surface can see is impossible must cost it nothing", len(port.calls))
		}
		assertEnvelope(t, rec, "invalid_request", "the request body is larger than this operation accepts")
	})

	t.Run("a body at the bound arrives whole", func(t *testing.T) {
		port := &fakeProjection{ack: dataplane.ProjectionAck{ProtocolVersion: 1, AppliedRevision: 1}}
		handler := projectionApp(t, port)

		// The fixture is padded to the exact bound and still a JSON object, so
		// the delivery is refused by nothing and the port sees every byte.
		padding := maxProjectionBodyBytes - len(`{"protocol_version":1,"padding":""}`)
		atBound := `{"protocol_version":1,"padding":"` + strings.Repeat("x", padding) + `"}`
		if len(atBound) != maxProjectionBodyBytes {
			t.Fatalf("the fixture is %d bytes, want exactly %d", len(atBound), maxProjectionBodyBytes)
		}

		rec := projectionRequest(t, handler, stdhttp.MethodPost, projectionChangesPath, atBound)
		if rec.Code != stdhttp.StatusOK {
			t.Fatalf("the delivery at the bound answered %d, want %d (body: %s)", rec.Code, stdhttp.StatusOK, rec.Body.String())
		}
		if len(port.calls) != 1 || port.calls[0].message != atBound {
			t.Errorf("the message did not arrive whole at the bound")
		}
	})
}

// TestAnUnreadableDeliveryIsRefusedHere covers the read failure that is not an
// overrun: a body that breaks mid-read is not a message this operation can
// read, and the answer is a 400 about the request rather than a 502 about the
// listener.
func TestAnUnreadableDeliveryIsRefusedHere(t *testing.T) {
	port := &fakeProjection{}
	handler := projectionApp(t, port)

	request := httptest.NewRequest(stdhttp.MethodPost, projectionChangesPath, brokenReader{})
	request.Header.Set("Authorization", "Bearer "+testCredential)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)

	if rec.Code != stdhttp.StatusBadRequest {
		t.Fatalf("the unreadable delivery answered %d, want %d", rec.Code, stdhttp.StatusBadRequest)
	}
	if len(port.calls) != 0 {
		t.Errorf("the listener received %d deliveries from an unreadable body", len(port.calls))
	}
	assertEnvelope(t, rec, "invalid_request", "the request body is not a message this operation can read")
}

// brokenReader is a request body whose read fails partway through.
type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// TestAnEmptyDeliveryIsDeliveredVerbatim pins the no-second-grammar rule at
// its most tempting input: an empty body. This surface does not decide that
// emptiness means anything — the bytes cross, and the listener's refusal of a
// non-message arrives here as an ordinary application error. Deciding it
// locally would be the façade writing grammar, which is the one thing the
// private protocol does not let a middle hop write.
func TestAnEmptyDeliveryIsDeliveredVerbatim(t *testing.T) {
	port := &fakeProjection{ack: dataplane.ProjectionAck{ProtocolVersion: 1, AppliedRevision: 0}}
	handler := projectionApp(t, port)

	rec := projectionRequest(t, handler, stdhttp.MethodPost, projectionChangesPath, "")
	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("the empty delivery answered %d, want %d (body: %s)", rec.Code, stdhttp.StatusOK, rec.Body.String())
	}
	if len(port.calls) != 1 || port.calls[0].message != "" {
		t.Errorf("the port received %+v, want one delivery of the empty body verbatim", port.calls)
	}
}

// TestTheProjectionRefusalsAreTheStatusesTheContractDeclares is the status
// mapping for the write path: each of the port's four refusals becomes its own
// status and code, with the application's fixed public message and nothing
// from whatever the listener said. The table is the transport half of
// application's translation test — the codes are the same ones, and the
// statuses are what makes the difference visible to a producer reading only
// HTTP.
func TestTheProjectionRefusalsAreTheStatusesTheContractDeclares(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name:        "a version the data plane does not speak is the caller's impossible request",
			err:         fmt.Errorf("wrapped: %w", dataplane.ErrProjectionUnsupportedVersion),
			wantStatus:  stdhttp.StatusBadRequest,
			wantCode:    "unsupported_version",
			wantMessage: "the message speaks a protocol version this surface does not support",
		},
		{
			name:        "a message outside the grammar is invalid_request",
			err:         fmt.Errorf("wrapped: %w", dataplane.ErrProjectionShape),
			wantStatus:  stdhttp.StatusBadRequest,
			wantCode:    "invalid_request",
			wantMessage: "the message does not satisfy the projection protocol's grammar",
		},
		{
			name:        "a gap is a conflict a human decides about",
			err:         fmt.Errorf("wrapped: %w", dataplane.ErrProjectionGap),
			wantStatus:  stdhttp.StatusConflict,
			wantCode:    "revision_gap",
			wantMessage: "the delivered batch does not join the applied position",
		},
		{
			name:        "a snapshot requirement is a conflict the producer resolves itself",
			err:         fmt.Errorf("wrapped: %w", dataplane.ErrProjectionSnapshotRequired),
			wantStatus:  stdhttp.StatusConflict,
			wantCode:    "snapshot_required",
			wantMessage: "the stored position cannot join this producer timeline; deliver a snapshot",
		},
		{
			name:        "the listener being unreachable is not this process's failure",
			err:         fmt.Errorf("wrapped: %w", dataplane.ErrUpstreamUnavailable),
			wantStatus:  stdhttp.StatusBadGateway,
			wantCode:    "upstream_unavailable",
			wantMessage: "the data plane is unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := &fakeProjection{snapshotErr: tt.err, changesErr: tt.err}
			handler := projectionApp(t, port)

			rec := projectionRequest(t, handler, stdhttp.MethodPost, projectionSnapshotPath, `{"protocol_version":1}`)
			if rec.Code != tt.wantStatus {
				t.Fatalf("the refusal answered %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			assertEnvelope(t, rec, tt.wantCode, tt.wantMessage)
		})
	}
}

// TestTheProjectionRefusalsCarryNoListenerVocabulary is the leak check at the
// last hop: whatever text the port's cause carries, the envelope a producer
// reads is the fixed message alone. The marker stands in for anything the
// Data Plane's own refusal might have said — its internals, its paths, its
// request ids.
func TestTheProjectionRefusalsCarryNoListenerVocabulary(t *testing.T) {
	const marker = "listener-refusal-marker-c2d9"
	port := &fakeProjection{
		changesErr: fmt.Errorf("%w: the listener said %s", dataplane.ErrProjectionGap, marker),
	}
	handler := projectionApp(t, port)

	rec := projectionRequest(t, handler, stdhttp.MethodPost, projectionChangesPath, `{"protocol_version":1}`)
	if rec.Code != stdhttp.StatusConflict {
		t.Fatalf("the refusal answered %d, want %d", rec.Code, stdhttp.StatusConflict)
	}
	if strings.Contains(rec.Body.String(), marker) {
		t.Errorf("the envelope %q carries the listener's own words", rec.Body.String())
	}
	assertEnvelope(t, rec, "revision_gap", "the delivered batch does not join the applied position")
}

// assertEnvelope reads the one error envelope the surface writes and compares
// its two meaningful fields. The request ID is present in every envelope by
// contract; its value is generated per request, so only its presence is
// asserted here.
func assertEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantCode, wantMessage string) {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("the refusal body did not decode: %v (body: %s)", err, rec.Body.String())
	}
	if envelope.Error.Code != wantCode {
		t.Errorf("code = %q, want %q", envelope.Error.Code, wantCode)
	}
	if envelope.Error.Message != wantMessage {
		t.Errorf("message = %q, want %q", envelope.Error.Message, wantMessage)
	}
	if envelope.RequestID == "" {
		t.Error("the envelope carries no request id; the contract guarantees one in every failure")
	}
}
