package dataplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"

	port "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
)

// This file pins the projection half of the client: the Control → Data
// direction of the seam, where this adapter is a transport and not a second
// grammar. Four promises are held here, and each has its own test.
//
//   - the message crosses byte for byte, whatever bytes it is (the delivery
//     tests at the top);
//   - an answer crosses back as the port's vocabulary, with the consumer's
//     own facts passed through unjudged and every required field demanded;
//   - a refusal is classified into the port's sentinels, because the delivery
//     loop's next move is a decision about which refusal it met;
//   - nothing about the request — the endpoint, the credential, the peer's
//     own words — comes back inside an error the loop will log every cycle.
//
// Where a shape repeats across the protocol's three operations, the test
// drives all three, so a rule proved for one read or delivery is proved for
// its siblings in the same run.

const (
	// aSnapshotMessage is written the way the projection domain would hand a
	// message over: legal, and awkward on purpose. The hand-set spacing, the
	// escaped slash, the multi-byte character and the fields no version of
	// this adapter has ever heard of are each a byte that a decode-and-
	// re-encode round trip would "tidy" — and a tidied message is not the
	// message the producer built and validated.
	aSnapshotMessage = `{ "protocol_version" : 1, "epoch" : "7c1f7b8e-2a4d-4e60-9a3b-5f1d0c8b2e41", "snapshot_revision" : 7, "api_keys" : [ ], "accounts" : [ ], "note" : "forward\/slash", "future_field" : { "why" : "café" } }`

	// aChangesMessage is the incremental batch's own awkwardness: a padded
	// string inside a payload, and an additive field at the envelope's edge.
	aChangesMessage = `{"protocol_version":1,"epoch":"7c1f7b8e-2a4d-4e60-9a3b-5f1d0c8b2e41","from_revision":8,"changes":[{"revision":8,"resource_kind":"api_key","resource_id":"k-1","recorded_at":"2026-09-25T00:00:00Z","payload":{"state":"revoked","padded ":"kept"}}],"additive_hint":"ignored-here"}`

	// anAcknowledgementBody and aPositionBody are the answers a listener that
	// did its work gives back.
	anAcknowledgementBody = `{"protocol_version":1,"applied_revision":7}`
	aPositionBody         = `{"protocol_version":1,"epoch":"7c1f7b8e-2a4d-4e60-9a3b-5f1d0c8b2e41","bootstrapped":true,"applied_revision":7}`

	// aListenerMarker is prose the façade's refusal carries in its message
	// field: a string that must never appear in an error this package
	// returns, however loudly the peer shouts it.
	aListenerMarker = "LISTENER-PROSE-NEVER-FOR-A-PRODUCER-LOG"
)

const (
	// projectionContractPath is the contract document this file pins the
	// protocol's own fragment against, relative to this package's directory —
	// the same six levels back contract_test.go counts for the usage-fact
	// fragment and the group-version read: dataplane, outbound, adapters,
	// internal, console-api, apps. dataplaneContractPath itself is
	// contract_test.go's, and names the same document for the whole package.
	projectionContractPath = "../../../../../../api/openapi/shared/projection.yaml"
)

// capturedRequest is everything a test's listener saw of one call: the method,
// the path, the headers that carry the credential and the media type, and the
// body exactly as it arrived.
type capturedRequest struct {
	method        string
	path          string
	contentType   string
	authorization string
	body          []byte
}

// aRecordingServer answers every request with answer and hands the request it
// saw to captured. The channel is what makes the capture race-free rather than
// merely usually-fine: the handler sends before it answers, so a client that
// received an answer has necessarily been handed the request that produced it,
// and a receive after a nil error can never block.
func aRecordingServer(t *testing.T, captured chan<- capturedRequest, answer func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
		}
		captured <- capturedRequest{
			method:        r.Method,
			path:          r.URL.Path,
			contentType:   r.Header.Get("Content-Type"),
			authorization: r.Header.Get("Authorization"),
			body:          body,
		}
		answer(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// anAnswer writes one status and one body, the way the façade writes an
// answer or a refusal.
func anAnswer(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// aClientFor returns the adapter pointed at server, holding the credential
// every test in this package uses.
func aClientFor(server *httptest.Server) *Client {
	return New(server.Client(), server.URL, credential)
}

// refusalBody is one ErrorEnvelope as shared/errors.yaml declares it: a code,
// a message, a request id.
func refusalBody(code, message string) string {
	return `{"error":{"code":"` + code + `","message":"` + message + `"},"request_id":"req-projection-1"}`
}

// errorCarriesNoSecrets is the no-relay rule as an assertion. An error this
// package returns may carry the classification and the status, and nothing
// about the request that produced it: not the endpoint, not the credential,
// and none of the markers a test planted in the listener's prose or in the
// transport's own diagnostics.
func errorCarriesNoSecrets(t *testing.T, err error, endpoint string, markers ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want a failure to inspect for secrets")
		return
	}
	message := err.Error()
	for _, secret := range append([]string{endpoint, credential}, markers...) {
		if strings.Contains(message, secret) {
			t.Errorf("error %q carries %q — a failure must not name the endpoint, the credential, or anything the peer said", message, secret)
		}
	}
}

// deliveryOperation is one of the protocol's two POST operations: the endpoint
// the contract gives it, the message the domain hands it, and the call a test
// makes.
type deliveryOperation struct {
	name    string
	path    string
	message string
	deliver func(c *Client) (port.ProjectionAck, error)
}

// deliveryOperations lists both deliveries, so a rule proved for one is proved
// for its twin in the same run.
func deliveryOperations() []deliveryOperation {
	return []deliveryOperation{
		{
			name:    "the snapshot delivery",
			path:    projectionSnapshotPath,
			message: aSnapshotMessage,
			deliver: func(c *Client) (port.ProjectionAck, error) {
				return c.DeliverSnapshot(context.Background(), []byte(aSnapshotMessage))
			},
		},
		{
			name:    "the changes delivery",
			path:    projectionChangesPath,
			message: aChangesMessage,
			deliver: func(c *Client) (port.ProjectionAck, error) {
				return c.DeliverChanges(context.Background(), []byte(aChangesMessage))
			},
		},
	}
}

// projectionOperation is any of the protocol's three operations, reduced to
// the one thing a classification test reads: the error it reports.
type projectionOperation struct {
	name   string
	invoke func(c *Client) error
}

// projectionOperations lists all three, so a refusal rule proved for one
// operation is proved for the other two in the same run.
func projectionOperations() []projectionOperation {
	operations := []projectionOperation{
		{
			name: "the position read",
			invoke: func(c *Client) error {
				_, err := c.ProjectionPosition(context.Background())
				return err
			},
		},
	}
	for _, delivery := range deliveryOperations() {
		operations = append(operations, projectionOperation{name: delivery.name, invoke: func(c *Client) error {
			_, err := delivery.deliver(c)
			return err
		}})
	}
	return operations
}

// TestProjectionDeliveriesSendTheMessageBytesVerbatim is the adapter's core
// promise: it is a transport, not a second grammar. The message was built and
// validated by the projection domain before this method was called, so the
// bytes that arrive at the listener must be the bytes the caller handed over —
// the hand-set spacing, the escaped slash, the multi-byte character and the
// additive fields included, because those are exactly what a decode-and-
// re-encode round trip would strip or rewrite. The headers are pinned beside
// the body: the credential on the authorization header, JSON as the media
// type, and the operation's own path and no other.
func TestProjectionDeliveriesSendTheMessageBytesVerbatim(t *testing.T) {
	for _, op := range deliveryOperations() {
		t.Run(op.name, func(t *testing.T) {
			captured := make(chan capturedRequest, 1)
			server := aRecordingServer(t, captured, anAnswer(http.StatusOK, anAcknowledgementBody))

			if _, err := op.deliver(aClientFor(server)); err != nil {
				t.Fatalf("%s error = %v, want nil", op.name, err)
			}
			got := <-captured

			if string(got.body) != op.message {
				t.Errorf("the body that arrived was %q, want %q byte for byte — re-encoding here would be a second grammar, and it would strip exactly the additive fields the protocol's evolution rule promises to carry", got.body, op.message)
			}
			if got, want := got.method, http.MethodPost; got != want {
				t.Errorf("method = %s, want %s", got, want)
			}
			if got, want := got.path, op.path; got != want {
				t.Errorf("path = %q, want %q — the operation's own endpoint and no other", got, want)
			}
			if got, want := got.contentType, "application/json"; got != want {
				t.Errorf("Content-Type = %q, want %q", got, want)
			}
			if got, want := got.authorization, "Bearer "+credential; got != want {
				t.Errorf("Authorization = %q, want %q", got, want)
			}
		})
	}
}

// TestProjectionDeliveriesKeepTheBaseURLsPathPrefix pins the base URL's path
// prefix onto the deliveries. A deployment behind a gateway serves the
// management surface under one, and the usage-fact read keeps it — a delivery
// that dropped it would POST revocations to a path the façade does not serve,
// and the seam would go quiet while every call answered 404.
func TestProjectionDeliveriesKeepTheBaseURLsPathPrefix(t *testing.T) {
	for _, op := range deliveryOperations() {
		t.Run(op.name, func(t *testing.T) {
			captured := make(chan capturedRequest, 1)
			server := aRecordingServer(t, captured, anAnswer(http.StatusOK, anAcknowledgementBody))
			client := New(server.Client(), server.URL+"/management", credential)

			if _, err := op.deliver(client); err != nil {
				t.Fatalf("%s error = %v, want nil", op.name, err)
			}
			got := <-captured

			if got, want := got.path, "/management"+op.path; got != want {
				t.Errorf("path = %q, want %q — the base URL's prefix is kept", got, want)
			}
		})
	}
}

// TestThePositionReadReachesItsOwnEndpointWithNothingButTheCredential pins the
// shape of the read: GET at the position's path, the credential on the
// authorization header, and nothing else — no body and no media type, because
// the position is the consumer's own fact and this call has no parameters to
// argue about.
func TestThePositionReadReachesItsOwnEndpointWithNothingButTheCredential(t *testing.T) {
	captured := make(chan capturedRequest, 1)
	server := aRecordingServer(t, captured, anAnswer(http.StatusOK, aPositionBody))

	if _, err := aClientFor(server).ProjectionPosition(context.Background()); err != nil {
		t.Fatalf("ProjectionPosition() error = %v, want nil", err)
	}
	got := <-captured

	if got, want := got.method, http.MethodGet; got != want {
		t.Errorf("method = %s, want %s", got, want)
	}
	if got, want := got.path, projectionPositionPath; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if got, want := got.authorization, "Bearer "+credential; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got.contentType != "" {
		t.Errorf("Content-Type = %q, want none — a read that carries no body names no media type", got.contentType)
	}
	if len(got.body) != 0 {
		t.Errorf("the read carried a body of %d bytes, want none", len(got.body))
	}
}

// TestProjectionPositionDecodesTheConsumerOwnFactsVerbatim pins the pass-
// through: a legal position becomes the port's ProjectionPosition field for
// field, and the consumer's own facts cross unjudged. The empty epoch, the
// false bootstrapped flag and the zero revision of a consumer that has never
// bootstrapped are that consumer's legitimate spelling of where it stands —
// they are data, and an adapter that refused or defaulted them would take the
// bootstrap decision away from the code that knows what they mean.
func TestProjectionPositionDecodesTheConsumerOwnFactsVerbatim(t *testing.T) {
	tests := []struct {
		name string
		body string
		want port.ProjectionPosition
	}{
		{
			name: "a position earned on a timeline",
			body: aPositionBody,
			want: port.ProjectionPosition{
				ProtocolVersion: 1,
				Epoch:           "7c1f7b8e-2a4d-4e60-9a3b-5f1d0c8b2e41",
				Bootstrapped:    true,
				AppliedRevision: 7,
			},
		},
		{
			name: "a consumer that has never bootstrapped",
			body: `{"protocol_version":1,"epoch":"","bootstrapped":false,"applied_revision":0}`,
			want: port.ProjectionPosition{ProtocolVersion: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := aRecordingServer(t, make(chan capturedRequest, 1), anAnswer(http.StatusOK, tt.body))

			position, err := aClientFor(server).ProjectionPosition(context.Background())
			if err != nil {
				t.Fatalf("ProjectionPosition() error = %v, want nil — every field the contract requires is present, empty or not", err)
			}
			if got, want := position.ProtocolVersion, tt.want.ProtocolVersion; got != want {
				t.Errorf("ProtocolVersion = %d, want %d", got, want)
			}
			if got, want := position.Epoch, tt.want.Epoch; got != want {
				t.Errorf("Epoch = %q, want %q — an empty epoch is the unbootstrapped consumer's own fact, not a defect", got, want)
			}
			if got, want := position.Bootstrapped, tt.want.Bootstrapped; got != want {
				t.Errorf("Bootstrapped = %t, want %t", got, want)
			}
			if got, want := position.AppliedRevision, tt.want.AppliedRevision; got != want {
				t.Errorf("AppliedRevision = %d, want %d — a revision of zero is a position, the unbootstrapped one", got, want)
			}
		})
	}
}

// TestProjectionDeliveriesDecodeTheAcknowledgementVerbatim pins the answer
// half of a delivery: the ack becomes the port's ProjectionAck field for
// field, and a boundary of zero is a legal answer — an empty projection is a
// snapshot too, so an applied_revision of 0 is a position the loop may act
// on, not an absence to refuse.
func TestProjectionDeliveriesDecodeTheAcknowledgementVerbatim(t *testing.T) {
	tests := []struct {
		name string
		body string
		want port.ProjectionAck
	}{
		{
			name: "a batch applied up to its last revision",
			body: `{"protocol_version":1,"applied_revision":9}`,
			want: port.ProjectionAck{ProtocolVersion: 1, AppliedRevision: 9},
		},
		{
			name: "a snapshot applied at the zero boundary",
			body: `{"protocol_version":1,"applied_revision":0}`,
			want: port.ProjectionAck{ProtocolVersion: 1},
		},
	}

	for _, op := range deliveryOperations() {
		for _, tt := range tests {
			t.Run(op.name+" / "+tt.name, func(t *testing.T) {
				server := aRecordingServer(t, make(chan capturedRequest, 1), anAnswer(http.StatusOK, tt.body))

				ack, err := op.deliver(aClientFor(server))
				if err != nil {
					t.Fatalf("%s error = %v, want nil", op.name, err)
				}
				if got, want := ack.ProtocolVersion, tt.want.ProtocolVersion; got != want {
					t.Errorf("ProtocolVersion = %d, want %d", got, want)
				}
				if got, want := ack.AppliedRevision, tt.want.AppliedRevision; got != want {
					t.Errorf("AppliedRevision = %d, want %d — the ack is the acknowledgement, and zero is a position", got, want)
				}
			})
		}
	}
}

// TestProjectionPositionRefusesAnAnswerMissingARequiredField is the read
// side's fail-closed table. Every field the contract marks required is refused
// when the body omits it or carries it as null, and the refusal names the
// field, because an operator reading a log needs to know what was missing and
// not merely that something was.
//
// The danger every row stands against is the zero value: `encoding/json`
// answers an absent `bootstrapped` with false and an absent `applied_revision`
// with zero, and those are a real position — the unbootstrapped one. A missing
// field that read as a position would send the producer into a re-snapshot it
// had no evidence for, so each body below differs from a legal position by the
// single field under test and nothing else.
func TestProjectionPositionRefusesAnAnswerMissingARequiredField(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		missingField string
	}{
		{
			name:         "no protocol_version field at all",
			body:         `{"epoch":"e-1","bootstrapped":true,"applied_revision":7}`,
			missingField: "protocol_version",
		},
		{
			name:         "a null protocol_version",
			body:         `{"protocol_version":null,"epoch":"e-1","bootstrapped":true,"applied_revision":7}`,
			missingField: "protocol_version",
		},
		{
			name:         "no epoch field at all",
			body:         `{"protocol_version":1,"bootstrapped":true,"applied_revision":7}`,
			missingField: "epoch",
		},
		{
			name:         "a null epoch",
			body:         `{"protocol_version":1,"epoch":null,"bootstrapped":true,"applied_revision":7}`,
			missingField: "epoch",
		},
		{
			name:         "no bootstrapped field at all",
			body:         `{"protocol_version":1,"epoch":"e-1","applied_revision":7}`,
			missingField: "bootstrapped",
		},
		{
			name:         "a null bootstrapped",
			body:         `{"protocol_version":1,"epoch":"e-1","bootstrapped":null,"applied_revision":7}`,
			missingField: "bootstrapped",
		},
		{
			name:         "no applied_revision field at all",
			body:         `{"protocol_version":1,"epoch":"e-1","bootstrapped":true}`,
			missingField: "applied_revision",
		},
		{
			name:         "a null applied_revision",
			body:         `{"protocol_version":1,"epoch":"e-1","bootstrapped":true,"applied_revision":null}`,
			missingField: "applied_revision",
		},
		{
			name:         "an answer that is an empty object",
			body:         `{}`,
			missingField: "protocol_version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := aRecordingServer(t, make(chan capturedRequest, 1), anAnswer(http.StatusOK, tt.body))

			position, err := aClientFor(server).ProjectionPosition(context.Background())
			if !errors.Is(err, port.ErrProjectionShape) {
				t.Fatalf("ProjectionPosition() error = %v, want it to wrap %v", err, port.ErrProjectionShape)
			}
			if !strings.Contains(err.Error(), tt.missingField) {
				t.Errorf("error %q does not name %q — a refusal that cannot say what was missing is a riddle where an operator needs an answer", err.Error(), tt.missingField)
			}
			if position != (port.ProjectionPosition{}) {
				t.Errorf("ProjectionPosition() returned %+v beside an error, want the zero position — a caller that ignored the error must not find a position in it", position)
			}
			errorCarriesNoSecrets(t, err, server.URL)
		})
	}
}

// TestProjectionPositionRefusesAVersionItDoesNotSpeak pins the closed version
// set (ADR 0007): an answer written for a protocol this build does not know is
// refused as a version problem, however plausible its remaining fields look —
// a position earned under another protocol is exactly what the version field
// exists to catch. The second row is the discriminator: the version is checked
// before the shape, so a wrong version is never misreported as a mere missing
// field, and a refusal to interoperate is never dressed up as a typo.
func TestProjectionPositionRefusesAVersionItDoesNotSpeak(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "a position written for version 2",
			body: `{"protocol_version":2,"epoch":"e-1","bootstrapped":true,"applied_revision":7}`,
		},
		{
			name: "a version claim on an otherwise empty body",
			body: `{"protocol_version":2}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := aRecordingServer(t, make(chan capturedRequest, 1), anAnswer(http.StatusOK, tt.body))

			position, err := aClientFor(server).ProjectionPosition(context.Background())
			if !errors.Is(err, port.ErrProjectionUnsupportedVersion) {
				t.Fatalf("ProjectionPosition() error = %v, want it to wrap %v", err, port.ErrProjectionUnsupportedVersion)
			}
			if !strings.Contains(err.Error(), "2") {
				t.Errorf("error %q does not name the version it was handed; an operator upgrading one side needs to see which one the other spoke", err.Error())
			}
			if position != (port.ProjectionPosition{}) {
				t.Errorf("ProjectionPosition() returned %+v beside an error, want the zero position", position)
			}
			errorCarriesNoSecrets(t, err, server.URL)
		})
	}
}

// TestProjectionDeliveryRefusesAnAcknowledgementMissingARequiredField is the
// delivery side's half of the same table. An acknowledgement whose
// applied_revision was missing would decode to zero, and zero is a position —
// the unbootstrapped one — so an absent field must be a refusal rather than an
// answer this process cannot tell apart from a real one.
func TestProjectionDeliveryRefusesAnAcknowledgementMissingARequiredField(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		missingField string
	}{
		{
			name:         "no protocol_version field at all",
			body:         `{"applied_revision":9}`,
			missingField: "protocol_version",
		},
		{
			name:         "a null protocol_version",
			body:         `{"protocol_version":null,"applied_revision":9}`,
			missingField: "protocol_version",
		},
		{
			name:         "no applied_revision field at all",
			body:         `{"protocol_version":1}`,
			missingField: "applied_revision",
		},
		{
			name:         "a null applied_revision",
			body:         `{"protocol_version":1,"applied_revision":null}`,
			missingField: "applied_revision",
		},
		{
			name:         "an answer that is an empty object",
			body:         `{}`,
			missingField: "protocol_version",
		},
	}

	for _, op := range deliveryOperations() {
		for _, tt := range tests {
			t.Run(op.name+" / "+tt.name, func(t *testing.T) {
				server := aRecordingServer(t, make(chan capturedRequest, 1), anAnswer(http.StatusOK, tt.body))

				ack, err := op.deliver(aClientFor(server))
				if !errors.Is(err, port.ErrProjectionShape) {
					t.Fatalf("%s error = %v, want it to wrap %v", op.name, err, port.ErrProjectionShape)
				}
				if !strings.Contains(err.Error(), tt.missingField) {
					t.Errorf("error %q does not name %q — the loop logs this refusal every cycle, so it must say what broke", err.Error(), tt.missingField)
				}
				if ack != (port.ProjectionAck{}) {
					t.Errorf("%s returned %+v beside an error, want the zero ack — a caller that ignored the error must not find a position in it", op.name, ack)
				}
				errorCarriesNoSecrets(t, err, server.URL)
			})
		}
	}
}

// TestProjectionDeliveryRefusesAVersionItDoesNotSpeak is the closed version
// set on the delivery side: an acknowledgement in a protocol this build does
// not speak is refused as a version problem, and the version is checked before
// the shape, for the same reason the position read refuses it before its own.
func TestProjectionDeliveryRefusesAVersionItDoesNotSpeak(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "an acknowledgement written for version 2", body: `{"protocol_version":2,"applied_revision":9}`},
		{name: "a version claim on an otherwise empty body", body: `{"protocol_version":2}`},
	}

	for _, op := range deliveryOperations() {
		for _, tt := range tests {
			t.Run(op.name+" / "+tt.name, func(t *testing.T) {
				server := aRecordingServer(t, make(chan capturedRequest, 1), anAnswer(http.StatusOK, tt.body))

				ack, err := op.deliver(aClientFor(server))
				if !errors.Is(err, port.ErrProjectionUnsupportedVersion) {
					t.Fatalf("%s error = %v, want it to wrap %v", op.name, err, port.ErrProjectionUnsupportedVersion)
				}
				if !strings.Contains(err.Error(), "2") {
					t.Errorf("error %q does not name the version it was handed", err.Error())
				}
				if ack != (port.ProjectionAck{}) {
					t.Errorf("%s returned %+v beside an error, want the zero ack", op.name, ack)
				}
				errorCarriesNoSecrets(t, err, server.URL)
			})
		}
	}
}

// TestProjectionRefusesABodyThatIsNotAnAnswerAtAll covers the other way a 200
// can lie: the status promised an answer and the body is not one — not JSON,
// cut off mid-answer, or JSON of an entirely different shape. The port has one
// name for an answer that arrived and is wrong, so these classify as a shape
// failure rather than surfacing as a bare decode error, which is the shape a
// failure of transport would take: an unknown answer is retryable bad luck, a
// body that breaks its own promise is a peer breaking the contract.
func TestProjectionRefusesABodyThatIsNotAnAnswerAtAll(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "a body that is not JSON at all", body: `not json`},
		{name: "a body cut off mid-answer", body: `{"protocol_version":1,"applied_rev`},
		{name: "a body that is a JSON array", body: `[1,2,3]`},
	}

	for _, op := range projectionOperations() {
		for _, tt := range tests {
			t.Run(op.name+" / "+tt.name, func(t *testing.T) {
				server := aRecordingServer(t, make(chan capturedRequest, 1), anAnswer(http.StatusOK, tt.body))

				if err := op.invoke(aClientFor(server)); !errors.Is(err, port.ErrProjectionShape) {
					t.Fatalf("%s error = %v, want it to wrap %v", op.name, err, port.ErrProjectionShape)
				}
			})
		}
	}
}

// TestARefusalCodeBecomesTheSentinelTheLoopActsOn is the classification table,
// and the one opinion the adapter is allowed to hold. The delivery loop's next
// move is a decision about which refusal it met — a gap is healed by
// re-reading the position, a required snapshot by snapshotting, a version by
// an upgrade and a human — so the four codes the protocol names become the
// port's four sentinels, for all three operations alike.
//
// The last three rows are the fail-closed half: a code this build predates, a
// body that is not an envelope, and an envelope that names no code are all
// answers whose meaning is unknown, and unknown is ErrProjectionUnavailable —
// a cycle that runs again, never a guess.
func TestARefusalCodeBecomesTheSentinelTheLoopActsOn(t *testing.T) {
	refusals := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{
			name:   "unsupported_version",
			status: http.StatusBadRequest,
			body:   refusalBody("unsupported_version", aListenerMarker),
			want:   port.ErrProjectionUnsupportedVersion,
		},
		{
			name:   "invalid_request",
			status: http.StatusBadRequest,
			body:   refusalBody("invalid_request", aListenerMarker),
			want:   port.ErrProjectionShape,
		},
		{
			name:   "revision_gap",
			status: http.StatusConflict,
			body:   refusalBody("revision_gap", aListenerMarker),
			want:   port.ErrProjectionGap,
		},
		{
			name:   "snapshot_required",
			status: http.StatusConflict,
			body:   refusalBody("snapshot_required", aListenerMarker),
			want:   port.ErrProjectionSnapshotRequired,
		},
		{
			name:   "a code this protocol does not name",
			status: http.StatusBadRequest,
			body:   refusalBody("solar_flare", aListenerMarker),
			want:   port.ErrProjectionUnavailable,
		},
		{
			name:   "a body that is not an error envelope",
			status: http.StatusBadRequest,
			body:   `<html>the gateway fell over</html>`,
			want:   port.ErrProjectionUnavailable,
		},
		{
			name:   "an envelope whose error carries no code",
			status: http.StatusConflict,
			body:   `{"error":{"message":"` + aListenerMarker + `"},"request_id":"req-projection-1"}`,
			want:   port.ErrProjectionUnavailable,
		},
	}

	for _, op := range projectionOperations() {
		for _, tt := range refusals {
			t.Run(op.name+" / "+tt.name, func(t *testing.T) {
				server := aRecordingServer(t, make(chan capturedRequest, 1), anAnswer(tt.status, tt.body))

				err := op.invoke(aClientFor(server))
				if !errors.Is(err, tt.want) {
					t.Fatalf("%s error = %v, want it to wrap %v — the loop's next move is a decision about which refusal it met", op.name, err, tt.want)
				}
				errorCarriesNoSecrets(t, err, server.URL, aListenerMarker)
			})
		}
	}
}

// TestAStatusTheProtocolDoesNotDeclareIsUnavailableWithItsNumber is the
// fallthrough: a status no operation on this surface declares leaves the
// cycle's answer unknown, which is unavailability — and the number is the one
// fact about it an operator acts on, so the number is the part of the message.
func TestAStatusTheProtocolDoesNotDeclareIsUnavailableWithItsNumber(t *testing.T) {
	for _, op := range projectionOperations() {
		for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusUnauthorized} {
			t.Run(fmt.Sprintf("%s / %d", op.name, status), func(t *testing.T) {
				server := aRecordingServer(t, make(chan capturedRequest, 1), anAnswer(status, refusalBody("internal", aListenerMarker)))

				err := op.invoke(aClientFor(server))
				if !errors.Is(err, port.ErrProjectionUnavailable) {
					t.Fatalf("%s error = %v, want it to wrap %v", op.name, err, port.ErrProjectionUnavailable)
				}
				if !strings.Contains(err.Error(), fmt.Sprintf("%d", status)) {
					t.Errorf("error %q does not name the status %d — the number is the part an operator acts on", err.Error(), status)
				}
				errorCarriesNoSecrets(t, err, server.URL, aListenerMarker)
			})
		}
	}
}

// TestARefusalIsReadForItsCodeAndNothingElse pins the no-leak rule on the one
// field the adapter reads. A refusal's only actionable part is its code, so the
// code is the only part this package reads: the listener's own prose stays in
// the listener, and never becomes a line in a log this process writes every
// few seconds.
//
// The first row proves the classification survives a message beside the code
// and none of the message survives with it. The other two prove the reading is
// bounded: prose padded past the window this adapter will read, and junk
// shouted after an otherwise legal envelope, both fail closed to
// ErrProjectionUnavailable with nothing of the shout in the error.
func TestARefusalIsReadForItsCodeAndNothingElse(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{
			name:   "a refusal whose message is the listener's own prose",
			status: http.StatusConflict,
			body:   refusalBody("revision_gap", aListenerMarker),
			want:   port.ErrProjectionGap,
		},
		{
			name:   "a refusal whose prose runs past the window this adapter reads",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"snapshot_required","message":"` + strings.Repeat(aListenerMarker, 4000) + `"}}`,
			want:   port.ErrProjectionUnavailable,
		},
		{
			name:   "a legal refusal followed by a shout of junk",
			status: http.StatusBadRequest,
			body:   refusalBody("revision_gap", "the batch does not join the position") + strings.Repeat(aListenerMarker, 4000),
			want:   port.ErrProjectionUnavailable,
		},
	}

	for _, op := range projectionOperations() {
		for _, tt := range tests {
			t.Run(op.name+" / "+tt.name, func(t *testing.T) {
				server := aRecordingServer(t, make(chan capturedRequest, 1), anAnswer(tt.status, tt.body))

				err := op.invoke(aClientFor(server))
				if !errors.Is(err, tt.want) {
					t.Fatalf("%s error = %v, want it to wrap %v", op.name, err, tt.want)
				}
				errorCarriesNoSecrets(t, err, server.URL, aListenerMarker)
			})
		}
	}
}

// TestATransportFailureCarriesNeitherTheEndpointNorTheCredential is the leak
// guard on the transport half. net/http wraps a failed request in a *url.Error
// that quotes the whole request URL — which names the endpoint and, through it,
// the deployment this process talks to — so the wrapper is dropped outright,
// diagnostics included. The first subtest hands the client an error that quotes
// everything at once and asserts none of it comes back; the second lets a real
// socket fail against a listener that is gone.
func TestATransportFailureCarriesNeitherTheEndpointNorTheCredential(t *testing.T) {
	t.Run("a failure that quotes the whole request", func(t *testing.T) {
		cause := fmt.Errorf("dial tcp %q: connect: connection refused; the peer's diagnostics: %s %s", baseURL, aListenerMarker, credential)
		quoted := &url.Error{Op: http.MethodPost, URL: baseURL + projectionChangesPath, Err: cause}
		client := New(&http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, quoted
		})}, baseURL, credential)

		for _, op := range projectionOperations() {
			t.Run(op.name, func(t *testing.T) {
				err := op.invoke(client)
				if !errors.Is(err, port.ErrProjectionUnavailable) {
					t.Fatalf("%s error = %v, want it to wrap %v", op.name, err, port.ErrProjectionUnavailable)
				}
				// The wrapper and its cause are dropped together, so neither the
				// quoted URL nor the reason inside it may appear — the delivery
				// loop logs this error every cycle until the peer is back.
				errorCarriesNoSecrets(t, err, baseURL, aListenerMarker, projectionChangesPath, "connection refused")
			})
		}
	})

	t.Run("a listener that is simply gone", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		endpoint := server.URL
		server.Close()

		client := New(&http.Client{}, endpoint, credential)
		for _, op := range projectionOperations() {
			t.Run(op.name, func(t *testing.T) {
				err := op.invoke(client)
				if !errors.Is(err, port.ErrProjectionUnavailable) {
					t.Fatalf("%s error = %v, want it to wrap %v", op.name, err, port.ErrProjectionUnavailable)
				}
				errorCarriesNoSecrets(t, err, endpoint)
			})
		}
	})
}

// TestTheProjectionPathsAreThePathsTheContractDeclares pins the three path
// constants to the façade's document. The two ends of the hop cannot import
// each other — separate Go modules by ADR 0006 §1 — so each names the strings
// and each fails when its own side moves: a path renamed in the contract and
// not here is a 404 discovered in production, and a constant invented here and
// not in the contract is an endpoint nobody serves.
func TestTheProjectionPathsAreThePathsTheContractDeclares(t *testing.T) {
	document := scanContract(t, dataplaneContractPath)

	declaredPaths := document.children["paths"]
	if len(declaredPaths) == 0 {
		t.Fatalf("%s declares no paths; this pin proves nothing until the scan finds them", dataplaneContractPath)
	}

	var projectionPaths []string
	for _, declared := range declaredPaths {
		if strings.HasPrefix(declared, "/internal/projection/") {
			projectionPaths = append(projectionPaths, declared)
		}
	}
	if len(projectionPaths) == 0 {
		t.Fatalf("%s declares no /internal/projection paths; this pin proves nothing until the scan finds them", dataplaneContractPath)
	}
	sort.Strings(projectionPaths)

	want := []string{projectionChangesPath, projectionPositionPath, projectionSnapshotPath}
	sort.Strings(want)
	if !slices.Equal(projectionPaths, want) {
		t.Errorf("%s declares the projection paths %v and this adapter calls %v; a path on one side and not the other is a refusal discovered in production rather than here", dataplaneContractPath, projectionPaths, want)
	}

	// The verbs, pinned beside the paths: a method swapped for its sibling is
	// the same silence with a different status code.
	verbs := []struct {
		path   string
		method string
	}{
		{path: projectionPositionPath, method: http.MethodGet},
		{path: projectionSnapshotPath, method: http.MethodPost},
		{path: projectionChangesPath, method: http.MethodPost},
	}
	for _, tt := range verbs {
		t.Run(tt.path, func(t *testing.T) {
			declared := document.children["paths."+tt.path]
			if len(declared) == 0 {
				t.Fatalf("%s declares nothing under %s; this pin proves nothing until the scan finds it", dataplaneContractPath, tt.path)
			}
			if !slices.Contains(declared, strings.ToLower(tt.method)) {
				t.Errorf("%s declares %v under %s and this adapter calls it with %s", dataplaneContractPath, declared, tt.path, tt.method)
			}
			for _, other := range []string{"get", "post", "put", "patch", "delete"} {
				if other != strings.ToLower(tt.method) && slices.Contains(declared, other) {
					t.Errorf("%s also declares %s under %s, which this adapter never calls; the surface grew an operation this client does not know about", dataplaneContractPath, other, tt.path)
				}
			}
		})
	}
}

// TestTheProtocolVersionThisBuildSpeaksIsTheOneTheContractDeclares pins the
// adapter's copy of the version to the fragment that declares it. The version
// is the closed set both hops fail closed against, so a bump on one side and
// not the other must be a refusal seen here rather than an interoperation
// discovered in production — and a constant that moved while the document kept
// its value would stay green in every behaviour test, because those use the
// same constant.
func TestTheProtocolVersionThisBuildSpeaksIsTheOneTheContractDeclares(t *testing.T) {
	declared, ok := scanContract(t, projectionContractPath).numbers["components.schemas.ProjectionProtocolVersion.const"]
	if !ok {
		t.Fatalf("%s declares no components.schemas.ProjectionProtocolVersion.const; this pin proves nothing until the scan finds it", projectionContractPath)
	}
	if declared != projectionProtocolVersion {
		t.Errorf("%s says the projection protocol's version is %d and this adapter speaks %d; the version set is closed and every hop fails closed against a version it does not know", projectionContractPath, declared, projectionProtocolVersion)
	}
}

// TestTheAnswersThisAdapterDecodesAreTheAnswersTheContractDescribes pins the
// field names this adapter reads to the ones the fragment declares. A decode is
// where a renamed field is most quietly wrong: the refusal table above is
// measured against the struct tags themselves, and tags that moved together
// would agree with each other and with nothing else — every field landing on a
// nil, every answer refused, and the pin still green. This reads the document
// instead, so the one file both hops claim to implement is the thing the code
// is measured against.
func TestTheAnswersThisAdapterDecodesAreTheAnswersTheContractDescribes(t *testing.T) {
	document := scanContract(t, projectionContractPath)

	tests := []struct {
		name   string
		path   string
		sample any
	}{
		{
			name:   "the position",
			path:   "components.schemas.ProjectionPosition.properties",
			sample: positionResponse{},
		},
		{
			name:   "the acknowledgement",
			path:   "components.schemas.ProjectionAppliedAck.properties",
			sample: ackResponse{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declared := document.children[tt.path]
			if len(declared) == 0 {
				t.Fatalf("%s declares no %s; this pin proves nothing until the scan finds it", projectionContractPath, tt.path)
			}
			sort.Strings(declared)

			if got := jsonFieldNames(t, tt.sample); !slices.Equal(got, declared) {
				t.Errorf("this adapter decodes %v and %s declares %v; a name on one side and not the other lands on a nil pointer or a zero value rather than failing, and the nil pointer is the one the refusal table would then blame on the wrong field", got, projectionContractPath, declared)
			}
		})
	}
}

// TestAnAnswerPastTheAnswerBoundIsRefusedAsAShape is the success path's half
// of the discipline its refusal read already keeps: what this adapter reads
// of an answer is bounded, not trusted. A legal answer on this hop is a few
// hundred bytes, so a body that carries more is not a bigger answer — it is
// the other end choosing this process's memory ceiling, and the read refuses
// it as a shape rather than buffering it whole.
//
// The body is built to be a legal answer if the bound did not exist — a
// well-formed position with every required field present and one giant
// additive field in front, the kind of field the version rule says to
// tolerate — so a green run cannot come from the grammar refusing it. The
// truncation is the only thing that can.
func TestAnAnswerPastTheAnswerBoundIsRefusedAsAShape(t *testing.T) {
	// Twice the bound, so the cut lands mid-field and the decoder never sees
	// a closing brace.
	body := `{"padding":"` + strings.Repeat("x", 1<<17) +
		`","protocol_version":1,"epoch":"0b6fd7a1-3f6e-4a55-9a21-5c8f2e7d1b90","bootstrapped":true,"applied_revision":12}`

	for _, op := range projectionOperations() {
		t.Run(op.name, func(t *testing.T) {
			server := aRecordingServer(t, make(chan capturedRequest, 1), anAnswer(http.StatusOK, body))

			if err := op.invoke(aClientFor(server)); !errors.Is(err, port.ErrProjectionShape) {
				t.Fatalf("%s error = %v, want it to wrap %v", op.name, err, port.ErrProjectionShape)
			}
		})
	}
}
