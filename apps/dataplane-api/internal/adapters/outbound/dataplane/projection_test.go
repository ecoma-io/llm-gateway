package dataplane

import (
	"context"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// settledPositionBody and settledAckBody are well-formed answers as the private
// listener writes them: the position of a mirror that bootstrapped and applied
// twelve revisions, and the acknowledgement of a delivery that landed.
const (
	settledPositionBody = `{"protocol_version":1,"epoch":"5d6f0a24-3b1e-4c2a-9f8e-7a1b2c3d4e5f","bootstrapped":true,"applied_revision":12}`
	settledAckBody      = `{"protocol_version":1,"applied_revision":12}`
)

// opaqueMessage is a delivery as a producer might have written it, built to
// catch a middle hop that "helps": unknown fields at both levels, HTML
// characters the default JSON encoder would rewrite, a multi-byte word,
// internal whitespace. Any transformation on the way through — a decode plus a
// re-encode, an escaping change, a field dropped — shows up as a byte
// difference below.
const opaqueMessage = `{"protocol_version":1,"epoch":"5d6f0a24-3b1e-4c2a-9f8e-7a1b2c3d4e5f","from_revision":4,"changes":[{"revision":5,"resource_kind":"api_key","resource_id":"0c9f1a58-2f8e-4b3d-8a7c-6e5d4c3b2a1f","recorded_at":"2026-09-25T09:00:00Z","payload":{"digest":"a1b2&<>c3","future_field":"győr — 👍","trailing space":"kept"}}]}`

// TestTheDeliveriesCarryTheMessageByteForByte is the claim the port's comment
// argues for, asserted on what the listener actually received: the producer's
// message crosses as the bytes that arrived — no decode, no re-encode, no
// field dropped — whichever delivery carried it.
//
// The wire's Content-Type is pinned too, because it is part of what the
// listener accepts and a delivery that arrived as text/plain would be refused
// before its grammar was ever judged.
func TestTheDeliveriesCarryTheMessageByteForByte(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		deliver func(t *testing.T, c *Client, message []byte) error
	}{
		{
			name: "the snapshot delivery",
			path: "/internal/projection/snapshot",
			deliver: func(t *testing.T, c *Client, message []byte) error {
				t.Helper()
				_, err := c.ApplyProjectionSnapshot(context.Background(), message)
				return err
			},
		},
		{
			name: "the changes delivery",
			path: "/internal/projection/changes",
			deliver: func(t *testing.T, c *Client, message []byte) error {
				t.Helper()
				_, err := c.ApplyProjectionChanges(context.Background(), message)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := &upstream{body: settledAckBody}
			client := up.server(t)

			if err := tt.deliver(t, client, []byte(opaqueMessage)); err != nil {
				t.Fatalf("the delivery failed: %v", err)
			}

			calls := up.recorded()
			if len(calls) != 1 {
				t.Fatalf("the listener received %d calls, want 1", len(calls))
			}
			call := calls[0]
			if want := stdhttp.MethodPost; call.method != want {
				t.Errorf("the delivery arrived as %s, want %s", call.method, want)
			}
			if want := tt.path; call.path != want {
				t.Errorf("the delivery arrived at %q, want %q", call.path, want)
			}
			if call.body != opaqueMessage {
				t.Errorf("the message did not cross byte for byte:\n got %q\nwant %q", call.body, opaqueMessage)
			}
			if want := "Bearer " + testCredential; call.authorization != want {
				t.Errorf("the delivery arrived with Authorization = %q, want %q", call.authorization, want)
			}
		})
	}

	t.Run("an empty message crosses as an empty body, for the listener to refuse", func(t *testing.T) {
		up := &upstream{body: settledAckBody}
		client := up.server(t)

		if _, err := client.ApplyProjectionChanges(context.Background(), nil); err != nil {
			t.Fatalf("ApplyProjectionChanges() error = %v", err)
		}
		calls := up.recorded()
		if len(calls) != 1 {
			t.Fatalf("the listener received %d calls, want 1", len(calls))
		}
		if calls[0].body != "" {
			t.Errorf("the empty message crossed as %q; this adapter has no grammar to add bytes with", calls[0].body)
		}
	})
}

// TestProjectionPositionReadsTheListenersAnswer pins the position read field
// by field, and in particular the two answers a plain-value decoder would
// mangle: the unbootstrapped listener's empty epoch and its zero revision.
// Both are facts the producer's cycle decides from — the empty epoch is its
// instruction to offer a snapshot — and a decode that turned an absent field
// into either would be this process issuing that instruction itself.
func TestProjectionPositionReadsTheListenersAnswer(t *testing.T) {
	tests := []struct {
		name string
		body string
		want dataplane.ProjectionPosition
	}{
		{
			name: "a bootstrapped mirror reports its epoch and its revision",
			body: settledPositionBody,
			want: dataplane.ProjectionPosition{ProtocolVersion: 1, Epoch: "5d6f0a24-3b1e-4c2a-9f8e-7a1b2c3d4e5f", Bootstrapped: true, AppliedRevision: 12},
		},
		{
			name: "a mirror that never bootstrapped reports an empty epoch at zero",
			body: `{"protocol_version":1,"epoch":"","bootstrapped":false,"applied_revision":0}`,
			want: dataplane.ProjectionPosition{ProtocolVersion: 1, Epoch: "", Bootstrapped: false, AppliedRevision: 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := &upstream{body: tt.body}
			client := up.server(t)

			position, err := client.ProjectionPosition(context.Background())
			if err != nil {
				t.Fatalf("ProjectionPosition() error = %v", err)
			}
			if position != tt.want {
				t.Errorf("ProjectionPosition() = %+v, want %+v", position, tt.want)
			}
		})
	}

	t.Run("the read is a GET at the position path", func(t *testing.T) {
		up := &upstream{body: settledPositionBody}
		client := up.server(t)

		if _, err := client.ProjectionPosition(context.Background()); err != nil {
			t.Fatalf("ProjectionPosition() error = %v", err)
		}
		calls := up.recorded()
		if len(calls) != 1 {
			t.Fatalf("the listener received %d calls, want 1", len(calls))
		}
		if want := stdhttp.MethodGet; calls[0].method != want {
			t.Errorf("the read arrived as %s, want %s", calls[0].method, want)
		}
		if want := "/internal/projection/position"; calls[0].path != want {
			t.Errorf("the read arrived at %q, want %q", calls[0].path, want)
		}
		if want := "Bearer " + testCredential; calls[0].authorization != want {
			t.Errorf("the read arrived with Authorization = %q, want %q", calls[0].authorization, want)
		}
	})
}

// TestProjectionPositionRefusesAPositionMissingRequiredFields is the
// fail-closed table: every field the private protocol marks required is
// refused when the body omits it or carries it as null, and the refusal is the
// unavailable sentinel — the answer is unknown, not zero.
//
// The rows are built from a valid position by deleting the one field under
// test, so a green run cannot come from some other refusal firing instead.
func TestProjectionPositionRefusesAPositionMissingRequiredFields(t *testing.T) {
	const epoch = "5d6f0a24-3b1e-4c2a-9f8e-7a1b2c3d4e5f"
	tests := []struct {
		name string
		body string
	}{
		{name: "no protocol_version at all", body: `{"epoch":"` + epoch + `","bootstrapped":true,"applied_revision":12}`},
		{name: "a null protocol_version", body: `{"protocol_version":null,"epoch":"` + epoch + `","bootstrapped":true,"applied_revision":12}`},
		{name: "no epoch at all", body: `{"protocol_version":1,"bootstrapped":true,"applied_revision":12}`},
		{name: "a null epoch", body: `{"protocol_version":1,"epoch":null,"bootstrapped":true,"applied_revision":12}`},
		{name: "no bootstrapped at all", body: `{"protocol_version":1,"epoch":"` + epoch + `","applied_revision":12}`},
		{name: "a null bootstrapped", body: `{"protocol_version":1,"epoch":"` + epoch + `","bootstrapped":null,"applied_revision":12}`},
		{name: "no applied_revision at all", body: `{"protocol_version":1,"epoch":"` + epoch + `","bootstrapped":true}`},
		{name: "a null applied_revision", body: `{"protocol_version":1,"epoch":"` + epoch + `","bootstrapped":true,"applied_revision":null}`},
		{name: "an empty object", body: `{}`},
		{name: "a body that is not JSON at all", body: "<html>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := &upstream{body: tt.body}
			client := up.server(t)

			position, err := client.ProjectionPosition(context.Background())
			if !errors.Is(err, dataplane.ErrUpstreamUnavailable) {
				t.Fatalf("ProjectionPosition() error = %v, want it to wrap %v", err, dataplane.ErrUpstreamUnavailable)
			}
			if position != (dataplane.ProjectionPosition{}) {
				t.Errorf("ProjectionPosition() returned %+v beside an error, want the zero position — a caller that ignored the error must not find an answer in it", position)
			}
			if strings.Contains(err.Error(), testCredential) {
				t.Errorf("the error text carries the credential: %q", err.Error())
			}
			if strings.Contains(err.Error(), "http://") {
				t.Errorf("the error text carries an address: %q", err.Error())
			}
		})
	}
}

// TestAPositionMayCarryFieldsTheContractDoesNotName pins the decoder's forward
// compatibility on the read: the listener may answer with a field a newer
// protocol added, and an unfamiliar key is ignored, never refused. A façade
// that refused one would be a version lock on the plane it fronts.
func TestAPositionMayCarryFieldsTheContractDoesNotName(t *testing.T) {
	body := `{"protocol_version":1,"epoch":"5d6f0a24-3b1e-4c2a-9f8e-7a1b2c3d4e5f","bootstrapped":true,"applied_revision":12,"snapshot_count":3}`

	up := &upstream{body: body}
	client := up.server(t)

	position, err := client.ProjectionPosition(context.Background())
	if err != nil {
		t.Fatalf("ProjectionPosition() error = %v, want nil — unknown fields are tolerated", err)
	}
	if position.AppliedRevision != 12 {
		t.Errorf("AppliedRevision = %d, want 12", position.AppliedRevision)
	}
}

// TestTheDeliveriesClassifyEveryAnswerTheListenerCanGive is the mapping table
// for the write half: the four refusals are named by the listener's envelope
// code because each one tells the producer's loop to do something different,
// and anything this adapter cannot classify — an untranslatable code, an
// unexpected status, an unreadable body, a listener that is not there — is the
// unknown answer, never a guess.
//
// Every row is also checked for the two values that must not travel with it:
// the listener's address and the shared secret.
func TestTheDeliveriesClassifyEveryAnswerTheListenerCanGive(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{
			name:    "an applied delivery is acknowledged with the mirror's new position",
			status:  stdhttp.StatusOK,
			body:    settledAckBody,
			wantErr: nil,
		},
		{
			name:    "an acknowledgement with no applied_revision is not an acknowledgement this adapter may pass on",
			status:  stdhttp.StatusOK,
			body:    `{"protocol_version":1}`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "an acknowledgement with no protocol_version is not one either",
			status:  stdhttp.StatusOK,
			body:    `{"applied_revision":12}`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "a version the listener does not speak halts the loop",
			status:  stdhttp.StatusBadRequest,
			body:    `{"error":{"code":"unsupported_version","message":"..."}}`,
			wantErr: dataplane.ErrProjectionUnsupportedVersion,
		},
		{
			name:    "a message outside the grammar is the listener's judgement to report",
			status:  stdhttp.StatusBadRequest,
			body:    `{"error":{"code":"invalid_request","message":"..."}}`,
			wantErr: dataplane.ErrProjectionShape,
		},
		{
			name:    "a batch that does not join the position is a gap a human decides about",
			status:  stdhttp.StatusConflict,
			body:    `{"error":{"code":"revision_gap","message":"..."}}`,
			wantErr: dataplane.ErrProjectionGap,
		},
		{
			name:    "a position that cannot join the timeline asks for a snapshot",
			status:  stdhttp.StatusConflict,
			body:    `{"error":{"code":"snapshot_required","message":"..."}}`,
			wantErr: dataplane.ErrProjectionSnapshotRequired,
		},
		{
			name:    "a refusal with a code this adapter does not translate is an unknown answer",
			status:  stdhttp.StatusConflict,
			body:    `{"error":{"code":"throttled","message":"..."}}`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "a refusal whose body is not the envelope is an unknown answer",
			status:  stdhttp.StatusBadRequest,
			body:    `{"detail":"no envelope here"}`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "a refusal that is not JSON at all is an unknown answer",
			status:  stdhttp.StatusConflict,
			body:    "<html>",
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "a listener that refuses this process's credential is not the caller's problem",
			status:  stdhttp.StatusUnauthorized,
			body:    `{"error":{"code":"unauthenticated","message":"..."}}`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "an implementation failure in the listener is not this process's failure",
			status:  stdhttp.StatusInternalServerError,
			body:    `{"error":{"code":"internal","message":"..."}}`,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
		{
			name:    "a status no contract declares is an unknown answer",
			status:  stdhttp.StatusNotImplemented,
			body:    settledAckBody,
			wantErr: dataplane.ErrUpstreamUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := &upstream{status: tt.status, body: tt.body}
			client := up.server(t)

			ack, err := client.ApplyProjectionChanges(context.Background(), []byte(opaqueMessage))
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ApplyProjectionChanges() error = %v, want an acknowledgement", err)
				}
				if want := (dataplane.ProjectionAck{ProtocolVersion: 1, AppliedRevision: 12}); ack != want {
					t.Errorf("ApplyProjectionChanges() = %+v, want %+v", ack, want)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ApplyProjectionChanges() error = %v, want it to wrap %v", err, tt.wantErr)
			}
			if ack != (dataplane.ProjectionAck{}) {
				t.Errorf("ApplyProjectionChanges() returned %+v beside an error, want the zero ack", ack)
			}
			if strings.Contains(err.Error(), testCredential) {
				t.Errorf("the error text carries the credential: %q", err.Error())
			}
			if strings.Contains(err.Error(), "http://") {
				t.Errorf("the error text carries an address: %q", err.Error())
			}
		})
	}

	t.Run("a listener that is not there is the data plane being unavailable", func(t *testing.T) {
		server := httptest.NewServer(stdhttp.HandlerFunc(func(stdhttp.ResponseWriter, *stdhttp.Request) {}))
		server.Close()

		client := New(server.Client(), server.URL, testCredential)
		_, err := client.ApplyProjectionSnapshot(context.Background(), []byte(opaqueMessage))
		if !errors.Is(err, dataplane.ErrUpstreamUnavailable) {
			t.Fatalf("ApplyProjectionSnapshot() error = %v, want it to wrap %v", err, dataplane.ErrUpstreamUnavailable)
		}
		if strings.Contains(err.Error(), testCredential) {
			t.Errorf("the error text carries the credential: %q", err.Error())
		}
		if strings.Contains(err.Error(), "http://") {
			t.Errorf("the error text carries an address: %q", err.Error())
		}
	})
}

// TestTheDeliveryTranslationNeverRelaysTheListenersBody is the
// translate-don't-relay rule asserted on the delivery answers, the counterpart
// of the read's test in protocol_test.go.
//
// The translated refusals are the sharp edge: four of the listener's codes are
// ones this adapter recognises, and an implementation that "helpfully"
// forwarded the listener's message alongside the sentinel would still pass
// every classification test above. Each body below therefore carries a marker
// that appears nowhere in this module — in the message of a translated refusal
// as readily as in an untranslated one — and any of them surviving into the
// error text is the leak this test exists to catch.
func TestTheDeliveryTranslationNeverRelaysTheListenersBody(t *testing.T) {
	const marker = "private-listener-body-marker-b47e"

	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "a translated refusal's own message", status: stdhttp.StatusBadRequest, body: `{"error":{"code":"unsupported_version","message":"` + marker + `"},"request_id":"r-1"}`},
		{name: "a translated shape refusal's message", status: stdhttp.StatusBadRequest, body: `{"error":{"code":"invalid_request","message":"` + marker + `"},"request_id":"r-2"}`},
		{name: "a translated gap refusal's message", status: stdhttp.StatusConflict, body: `{"error":{"code":"revision_gap","message":"` + marker + `"},"request_id":"r-3"}`},
		{name: "a translated snapshot refusal's message", status: stdhttp.StatusConflict, body: `{"error":{"code":"snapshot_required","message":"` + marker + `"},"request_id":"r-4"}`},
		{name: "an untranslated refusal", status: stdhttp.StatusGone, body: `{"error":{"code":"cursor_expired","message":"` + marker + `"},"request_id":"r-5"}`},
		{name: "a body that is not JSON at all", status: stdhttp.StatusInternalServerError, body: marker},
		{name: "an envelope shape this façade never declared", status: stdhttp.StatusBadGateway, body: `{"detail":"` + marker + `"}`},
		{name: "an unreadable success body", status: stdhttp.StatusOK, body: `{"protocol_version":1,"applied_revision":"` + marker + `"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := &upstream{status: tt.status, body: tt.body}
			client := up.server(t)

			_, err := client.ApplyProjectionSnapshot(context.Background(), []byte(opaqueMessage))
			if err == nil {
				t.Fatalf("ApplyProjectionSnapshot() error = nil, want a failure for status %d", tt.status)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("the error %q carries a value from the listener's body; the façade classifies what it read and answers from its own vocabulary", err.Error())
			}
		})
	}
}

// TestTheTwoDeliveriesAreTheTwoPathsTheyAreNamedFor is the compile-time claim
// made behavioural: the snapshot method goes to the snapshot path and the
// changes method to the changes path, and never the reverse. The two POSTs are
// otherwise identical — same body discipline, same credential, same
// translation — which is exactly why a swap would be invisible everywhere
// except at the mirror, where a snapshot would be refused as a batch and the
// bootstrap would quietly never happen.
func TestTheTwoDeliveriesAreTheTwoPathsTheyAreNamedFor(t *testing.T) {
	up := &upstream{body: settledAckBody}
	client := up.server(t)

	if _, err := client.ApplyProjectionSnapshot(context.Background(), []byte(`{"snapshot":true}`)); err != nil {
		t.Fatalf("ApplyProjectionSnapshot() error = %v", err)
	}
	if _, err := client.ApplyProjectionChanges(context.Background(), []byte(`{"snapshot":false}`)); err != nil {
		t.Fatalf("ApplyProjectionChanges() error = %v", err)
	}

	calls := up.recorded()
	if len(calls) != 2 {
		t.Fatalf("the listener received %d calls, want 2", len(calls))
	}
	if want := "/internal/projection/snapshot"; calls[0].path != want {
		t.Errorf("the snapshot delivery arrived at %q, want %q", calls[0].path, want)
	}
	if want := "/internal/projection/changes"; calls[1].path != want {
		t.Errorf("the changes delivery arrived at %q, want %q", calls[1].path, want)
	}
}

// TestAnAnswerPastTheAnswerBoundIsRefusedAsUnreadable is the read side of the
// same bargain the delivery side's byte-for-byte rule strikes: what crosses
// out is the producer's; what is read back is bounded. A legal answer on this
// hop is a few hundred bytes, so a body that carries more is not a bigger
// answer — it is the answerer choosing this process's memory ceiling, and the
// read refuses it as a body the facade cannot read rather than buffering it
// whole.
//
// Each body is built to be a legal answer if the bound did not exist, so a
// green run cannot come from the grammar refusing it — the truncation is the
// only thing that can. The padded refusal row is the one with teeth: a
// refusal whose code is cut off by the bound is an answer of unknown meaning,
// and unknown is the unavailable sentinel — never the code the body names,
// because acting on a half-read code would have the producer's loop heal or
// halt on evidence this facade never actually held.
func TestAnAnswerPastTheAnswerBoundIsRefusedAsUnreadable(t *testing.T) {
	// Twice the bound, so the cut lands mid-field and the decoder never sees
	// a closing brace.
	filler := strings.Repeat("x", 1<<17)

	t.Run("the position read", func(t *testing.T) {
		up := &upstream{body: `{"padding":"` + filler +
			`","protocol_version":1,"epoch":"5d6f0a24-3b1e-4c2a-9f8e-7a1b2c3d4e5f","bootstrapped":true,"applied_revision":12}`}
		client := up.server(t)

		if _, err := client.ProjectionPosition(context.Background()); !errors.Is(err, dataplane.ErrUpstreamUnavailable) {
			t.Fatalf("error = %v, want it to wrap %v", err, dataplane.ErrUpstreamUnavailable)
		}
	})

	t.Run("the acknowledgement", func(t *testing.T) {
		up := &upstream{body: `{"padding":"` + filler + `","protocol_version":1,"applied_revision":12}`}
		client := up.server(t)

		if _, err := client.ApplyProjectionChanges(context.Background(), []byte(opaqueMessage)); !errors.Is(err, dataplane.ErrUpstreamUnavailable) {
			t.Fatalf("error = %v, want it to wrap %v", err, dataplane.ErrUpstreamUnavailable)
		}
	})

	t.Run("a refusal padded past the bound loses its code", func(t *testing.T) {
		up := &upstream{
			status: stdhttp.StatusConflict,
			body:   `{"error":{"code":"revision_gap","message":"` + filler + `"},"request_id":"r"}`,
		}
		client := up.server(t)

		_, err := client.ApplyProjectionChanges(context.Background(), []byte(opaqueMessage))
		if !errors.Is(err, dataplane.ErrUpstreamUnavailable) {
			t.Fatalf("error = %v, want it to wrap %v", err, dataplane.ErrUpstreamUnavailable)
		}
		if errors.Is(err, dataplane.ErrProjectionGap) {
			t.Fatalf("error = %v: a refusal this facade could not fully read must not classify as the code it names", err)
		}
	})
}
