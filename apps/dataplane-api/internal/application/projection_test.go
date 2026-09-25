package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/ports/outbound/dataplane"
)

// fakeProjection is the write half of the outbound port as this package's
// tests see it: a position or failure for the read, an acknowledgement or
// failure per delivery, and a record of which method ran with which bytes. The
// record is what pins the one thing a use case may not get wrong — that a
// snapshot delivery reaches the snapshot port and a batch delivery reaches the
// changes port, each with the message it was given.
type fakeProjection struct {
	position      dataplane.ProjectionPosition
	positionErr   error
	snapshotErr   error
	changesErr    error
	ack           dataplane.ProjectionAck
	snapshotCalls int
	changesCalls  int
	lastMessage   string
	positionCalls int
}

func (f *fakeProjection) ProjectionPosition(context.Context) (dataplane.ProjectionPosition, error) {
	f.positionCalls++
	return f.position, f.positionErr
}

func (f *fakeProjection) ApplyProjectionSnapshot(_ context.Context, message []byte) (dataplane.ProjectionAck, error) {
	f.snapshotCalls++
	f.lastMessage = string(message)
	if f.snapshotErr != nil {
		return dataplane.ProjectionAck{}, f.snapshotErr
	}
	return f.ack, nil
}

func (f *fakeProjection) ApplyProjectionChanges(_ context.Context, message []byte) (dataplane.ProjectionAck, error) {
	f.changesCalls++
	f.lastMessage = string(message)
	if f.changesErr != nil {
		return dataplane.ProjectionAck{}, f.changesErr
	}
	return f.ack, nil
}

// opaqueMessage is a delivery built to catch a use case that "helps": HTML
// characters the default encoder would rewrite, a multi-byte word, unknown
// fields. The use case's whole job is to hand the bytes down untouched.
const opaqueMessage = `{"protocol_version":1,"epoch":"e","changes":[{"payload":{"digest":"a&<>b — győr"}}]}`

// TestProjectionPositionCrossesUnchanged pins the position read's whole job:
// the mirror's fact crosses exactly as the port reported it, including the two
// answers a middle hop would be most tempted to rewrite — the empty epoch and
// the zero revision of a mirror that has never bootstrapped. Rewriting either
// would be this process deciding the producer's bootstrap for it.
func TestProjectionPositionCrossesUnchanged(t *testing.T) {
	tests := []struct {
		name string
		want dataplane.ProjectionPosition
	}{
		{
			name: "a bootstrapped mirror",
			want: dataplane.ProjectionPosition{ProtocolVersion: 1, Epoch: "5d6f0a24-3b1e-4c2a-9f8e-7a1b2c3d4e5f", Bootstrapped: true, AppliedRevision: 12},
		},
		{
			name: "a mirror that never bootstrapped",
			want: dataplane.ProjectionPosition{ProtocolVersion: 1, Epoch: "", Bootstrapped: false, AppliedRevision: 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := &fakeProjection{position: tt.want}
			app := New("test", &fakeUsageFacts{}, &fakeCatalog{}, port)

			position, err := app.ProjectionPosition(context.Background())
			if err != nil {
				t.Fatalf("ProjectionPosition() error = %v", err)
			}
			if position != tt.want {
				t.Errorf("ProjectionPosition() = %+v, want %+v", position, tt.want)
			}
			if port.positionCalls != 1 {
				t.Errorf("the port was read %d times, want once", port.positionCalls)
			}
		})
	}
}

// TestProjectionPositionTranslatesItsFailure pins the read's one translation:
// the unavailable sentinel becomes the code, with its cause preserved for
// errors.Is behind the boundary and its public message the fixed text. A
// failure the port does not describe is this process's own defect, and it is
// classified as Internal rather than reported as the Data Plane's problem.
func TestProjectionPositionTranslatesItsFailure(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantCode    Code
		wantMessage string
	}{
		{
			name:        "the listener being unreachable is the data plane being unavailable",
			err:         fmt.Errorf("wrapped: %w", dataplane.ErrUpstreamUnavailable),
			wantCode:    CodeUpstreamUnavailable,
			wantMessage: "the data plane is unavailable",
		},
		{
			name:        "a failure the port does not describe is this process's own",
			err:         errors.New("the storage implementation failed"),
			wantCode:    CodeInternal,
			wantMessage: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := New("test", &fakeUsageFacts{}, &fakeCatalog{}, &fakeProjection{positionErr: tt.err})

			_, err := app.ProjectionPosition(context.Background())
			if !errors.Is(err, tt.err) {
				t.Fatalf("ProjectionPosition() error = %v, want it to unwrap to %v", err, tt.err)
			}
			var applicationError *Error
			if !errors.As(err, &applicationError) {
				t.Fatalf("ProjectionPosition() error = %v, want an application error", err)
			}
			if applicationError.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", applicationError.Code, tt.wantCode)
			}
			if applicationError.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", applicationError.Message, tt.wantMessage)
			}
		})
	}
}

// TestDeliveriesTranslateEveryRefusalTheListenerProduces is the translation
// table: each of the port's four refusals becomes its own code, because each
// demands a different reflex from the producer's loop. The cause stays behind
// the boundary — errors.Is still reaches the sentinel — and the public message
// is the fixed text, carrying no word of what the listener said.
func TestDeliveriesTranslateEveryRefusalTheListenerProduces(t *testing.T) {
	tests := []struct {
		name        string
		sentinel    error
		wantCode    Code
		wantMessage string
	}{
		{
			name:        "a version the data plane does not speak halts the loop",
			sentinel:    dataplane.ErrProjectionUnsupportedVersion,
			wantCode:    CodeUnsupportedVersion,
			wantMessage: "the message speaks a protocol version this surface does not support",
		},
		{
			name:        "a message outside the grammar is the listener's judgement",
			sentinel:    dataplane.ErrProjectionShape,
			wantCode:    CodeInvalidRequest,
			wantMessage: "the message does not satisfy the projection protocol's grammar",
		},
		{
			name:        "a gap halts for a human",
			sentinel:    dataplane.ErrProjectionGap,
			wantCode:    CodeRevisionGap,
			wantMessage: "the delivered batch does not join the applied position",
		},
		{
			name:        "a position that cannot join the timeline asks for a snapshot",
			sentinel:    dataplane.ErrProjectionSnapshotRequired,
			wantCode:    CodeSnapshotRequired,
			wantMessage: "the stored position cannot join this producer timeline; deliver a snapshot",
		},
		{
			name:        "the listener being unreachable is not a refusal at all",
			sentinel:    dataplane.ErrUpstreamUnavailable,
			wantCode:    CodeUpstreamUnavailable,
			wantMessage: "the data plane is unavailable",
		},
	}

	deliveries := []struct {
		name    string
		deliver func(app *App, port *fakeProjection) error
	}{
		{
			name: "the snapshot delivery",
			deliver: func(app *App, port *fakeProjection) error {
				_, err := app.ApplyProjectionSnapshot(context.Background(), []byte(opaqueMessage))
				return err
			},
		},
		{
			name: "the changes delivery",
			deliver: func(app *App, port *fakeProjection) error {
				_, err := app.ApplyProjectionChanges(context.Background(), []byte(opaqueMessage))
				return err
			},
		},
	}

	for _, delivery := range deliveries {
		for _, tt := range tests {
			t.Run(delivery.name+" / "+tt.name, func(t *testing.T) {
				port := &fakeProjection{}
				if tt.sentinel == dataplane.ErrUpstreamUnavailable {
					port.snapshotErr, port.changesErr = tt.sentinel, tt.sentinel
				} else {
					wrapped := fmt.Errorf("wrapped: %w", tt.sentinel)
					port.snapshotErr, port.changesErr = wrapped, wrapped
				}
				app := New("test", &fakeUsageFacts{}, &fakeCatalog{}, port)

				err := delivery.deliver(app, port)
				if !errors.Is(err, tt.sentinel) {
					t.Fatalf("the delivery error = %v, want it to unwrap to %v", err, tt.sentinel)
				}
				var applicationError *Error
				if !errors.As(err, &applicationError) {
					t.Fatalf("the delivery error = %v, want an application error", err)
				}
				if applicationError.Code != tt.wantCode {
					t.Errorf("Code = %q, want %q", applicationError.Code, tt.wantCode)
				}
				if applicationError.Message != tt.wantMessage {
					t.Errorf("Message = %q, want %q", applicationError.Message, tt.wantMessage)
				}
			})
		}
	}
}

// TestADeliveryThePortDoesNotDescribeIsInternal closes the table: an error
// from the port that names no sentinel is this process's own defect, and it
// must not masquerade as one of the four refusals — a producer acting on the
// wrong code would either skip a delivery it should have halted on or halt on
// one its own next cycle resolves.
func TestADeliveryThePortDoesNotDescribeIsInternal(t *testing.T) {
	port := &fakeProjection{changesErr: errors.New("the storage implementation failed")}
	app := New("test", &fakeUsageFacts{}, &fakeCatalog{}, port)

	_, err := app.ApplyProjectionChanges(context.Background(), []byte(opaqueMessage))
	var applicationError *Error
	if !errors.As(err, &applicationError) {
		t.Fatalf("ApplyProjectionChanges() error = %v, want an application error", err)
	}
	if applicationError.Code != CodeInternal {
		t.Errorf("Code = %q, want %q — an unclassified port failure is this process's own defect", applicationError.Code, CodeInternal)
	}
	if errors.Is(err, dataplane.ErrProjectionGap) || errors.Is(err, dataplane.ErrProjectionSnapshotRequired) {
		t.Errorf("the error %v masquerades as a refusal; the four refusals are named by their sentinels alone", err)
	}
}

// TestDeliveriesReachTheirOwnPortsAndPassTheMessageThrough pins the two
// behaviours a pass-through can silently lose: the snapshot method must reach
// the snapshot port and the changes method the changes port — a swap would
// bootstrap the mirror with a batch — and the message must cross byte for
// byte, unknown fields and all.
func TestDeliveriesReachTheirOwnPortsAndPassTheMessageThrough(t *testing.T) {
	t.Run("a snapshot reaches the snapshot port", func(t *testing.T) {
		port := &fakeProjection{ack: dataplane.ProjectionAck{ProtocolVersion: 1, AppliedRevision: 7}}
		app := New("test", &fakeUsageFacts{}, &fakeCatalog{}, port)

		ack, err := app.ApplyProjectionSnapshot(context.Background(), []byte(opaqueMessage))
		if err != nil {
			t.Fatalf("ApplyProjectionSnapshot() error = %v", err)
		}
		if port.snapshotCalls != 1 || port.changesCalls != 0 {
			t.Fatalf("the snapshot reached snapshot=%d changes=%d, want snapshot=1 changes=0", port.snapshotCalls, port.changesCalls)
		}
		if want := (dataplane.ProjectionAck{ProtocolVersion: 1, AppliedRevision: 7}); ack != want {
			t.Errorf("ack = %+v, want %+v", ack, want)
		}
		if port.lastMessage != opaqueMessage {
			t.Errorf("the message did not cross byte for byte:\n got %q\nwant %q", port.lastMessage, opaqueMessage)
		}
	})

	t.Run("a batch reaches the changes port", func(t *testing.T) {
		port := &fakeProjection{ack: dataplane.ProjectionAck{ProtocolVersion: 1, AppliedRevision: 12}}
		app := New("test", &fakeUsageFacts{}, &fakeCatalog{}, port)

		ack, err := app.ApplyProjectionChanges(context.Background(), []byte(opaqueMessage))
		if err != nil {
			t.Fatalf("ApplyProjectionChanges() error = %v", err)
		}
		if port.changesCalls != 1 || port.snapshotCalls != 0 {
			t.Fatalf("the batch reached snapshot=%d changes=%d, want snapshot=0 changes=1", port.snapshotCalls, port.changesCalls)
		}
		if want := (dataplane.ProjectionAck{ProtocolVersion: 1, AppliedRevision: 12}); ack != want {
			t.Errorf("ack = %+v, want %+v", ack, want)
		}
		if port.lastMessage != opaqueMessage {
			t.Errorf("the message did not cross byte for byte:\n got %q\nwant %q", port.lastMessage, opaqueMessage)
		}
	})
}

// TestTheTranslatedMessagesCarryNoCause pins the boundary the cause stays
// behind: the public messages are the fixed texts, and an implementation that
// interpolated the cause's text into one would leak the listener's words —
// which the marker here stands in for.
func TestTheTranslatedMessagesCarryNoCause(t *testing.T) {
	const marker = "cause-marker-81f3"
	port := &fakeProjection{changesErr: fmt.Errorf("%w: listener said %s", dataplane.ErrProjectionGap, marker)}
	app := New("test", &fakeUsageFacts{}, &fakeCatalog{}, port)

	_, err := app.ApplyProjectionChanges(context.Background(), []byte(opaqueMessage))
	var applicationError *Error
	if !errors.As(err, &applicationError) {
		t.Fatalf("ApplyProjectionChanges() error = %v, want an application error", err)
	}
	if strings.Contains(applicationError.Message, marker) {
		t.Errorf("the public message %q carries the cause's text; the cause stays behind the boundary", applicationError.Message)
	}
	if applicationError.Error() != applicationError.Message {
		t.Errorf("Error() = %q, want the public message %q", applicationError.Error(), applicationError.Message)
	}
}
