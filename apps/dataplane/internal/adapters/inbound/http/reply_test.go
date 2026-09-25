package http

import (
	"context"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// The reply channel's bytes, pinned end to end over a recorder.
//
// The routing stage drives the reply the handler hands it, and what the
// client receives is decided half by the walk and half by this transport's
// framing — so every shape the reply can leave on the wire is spelled out
// here byte for byte, the same discipline wiremap_test.go holds the table
// to. The walk itself is the application suite's business; what is pinned
// here is that its decisions arrive at the client exactly as the contract
// frames them.

// fakeRoutingCompletion stands in for the routing stage at the handler's
// boundary: it records the input, drives the reply exactly as the walk
// would — Open, content, an ending — and returns the outcome that ending
// produces. A scenario that never touches the reply exercises the
// admission-only shapes, which the reply must not disturb.
type fakeRoutingCompletion struct {
	act     func(reply application.Reply)
	outcome application.ChatOutcome
}

func (f *fakeRoutingCompletion) Serve(_ context.Context, in application.ChatInput) (application.ChatOutcome, error) {
	if f.act != nil {
		f.act(in.Reply)
	}
	return f.outcome, nil
}

// routingHandler builds the handler over the fake walk, and runs one request
// through it, returning the recorder.
func routingHandler(t *testing.T, chat *fakeRoutingCompletion) *httptest.ResponseRecorder {
	t.Helper()
	handler := newChatCompletionHandler(wiring{
		auth: &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: verifiedKey}},
		chat: chat,
	})
	recorder := httptest.NewRecorder()
	handler(recorder, newChatRequest(`{"model":"test-model","messages":[]}`))
	return recorder
}

// servedOutcome is the outcome a completed answer returns: the walk served
// the request through the reply, and the table's part is the log line only.
func servedOutcome() application.ChatOutcome {
	return application.ChatOutcome{
		Kind:             application.OutcomeServed,
		RuntimeRequestID: identity.RequestID("01930000-0000-7000-8000-000000000042"),
		Routing: &application.RoutingTrace{
			Alias:        "test-model",
			Attempts:     1,
			LastPosition: 1,
		},
	}
}

// TestAStreamedAnswerIsFramedAsServerSentEvents pins the stream's whole
// shape: the content type armed at the commitment point, one `data:` frame
// per content call with the blank line that ends each frame, the terminal
// [DONE] frame closing it, and the flushes that make it a stream rather than
// a batched body. The request identifier was set before the first byte — a
// streamed answer's headers freeze at its commitment, and the guarantee
// cannot wait for a writer that runs later.
func TestAStreamedAnswerIsFramedAsServerSentEvents(t *testing.T) {
	chat := &fakeRoutingCompletion{
		outcome: servedOutcome(),
		act: func(reply application.Reply) {
			reply.Open(true)
			if err := reply.Content([]byte(`{"delta":"hel"}`)); err != nil {
				t.Errorf("the first chunk's write failed: %v", err)
			}
			if err := reply.Content([]byte(`{"delta":"lo"}`)); err != nil {
				t.Errorf("the second chunk's write failed: %v", err)
			}
			reply.ServeSucceeded()
		},
	}

	recorder := routingHandler(t, chat)

	if recorder.Code != stdhttp.StatusOK {
		t.Errorf("status = %d, want 200 — the answer was committed", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Errorf("content type = %q, want the stream's", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("cache control = %q, want no-cache", got)
	}
	want := "data: {\"delta\":\"hel\"}\n\ndata: {\"delta\":\"lo\"}\n\ndata: [DONE]\n\n"
	if got := recorder.Body.String(); got != want {
		t.Errorf("body = %q, want the framed stream %q", got, want)
	}
	if !recorder.Flushed {
		t.Error("the stream was never flushed; a client reads batches, not a stream")
	}
	if got := recorder.Header().Get(RequestIDHeader); got != "chat-request" {
		t.Errorf("X-Request-Id = %q, want the correlation identifier set before the first byte", got)
	}
}

// TestABodyAnswerIsOneWritePinsTheNonStreamShape: no frames, no [DONE], no
// stream content type — the answer is the bytes as the executor wrote them,
// under the JSON content type, and nothing else.
func TestABodyAnswerIsOneWrite(t *testing.T) {
	chat := &fakeRoutingCompletion{
		outcome: servedOutcome(),
		act: func(reply application.Reply) {
			reply.Open(false)
			if err := reply.Content([]byte(`{"answer":true}`)); err != nil {
				t.Errorf("the body's write failed: %v", err)
			}
			reply.ServeSucceeded()
		},
	}

	recorder := routingHandler(t, chat)

	if recorder.Code != stdhttp.StatusOK {
		t.Errorf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q, want application/json", got)
	}
	if got := recorder.Body.String(); got != `{"answer":true}` {
		t.Errorf("body = %q, want the answer's own bytes and nothing else", got)
	}
}

// TestAMidStreamFailureClosesTheStreamWithTheFailureFrame pins the
// post-commitment ending: the delivered content stands, the failure frame
// carries the one internal body — no cause, no provider words — and the
// terminal [DONE] frame closes the stream so the client knows the answer
// ended rather than stalled.
func TestAMidStreamFailureClosesTheStreamWithTheFailureFrame(t *testing.T) {
	chat := &fakeRoutingCompletion{
		outcome: application.ChatOutcome{
			Kind:             application.OutcomeServed,
			RuntimeRequestID: identity.RequestID("01930000-0000-7000-8000-000000000042"),
			Routing: &application.RoutingTrace{
				Alias:          "test-model",
				Attempts:       1,
				LastPosition:   1,
				LastErrorClass: execution.ErrorStreamAfterCommitment,
				Committed:      true,
			},
		},
		act: func(reply application.Reply) {
			reply.Open(true)
			if err := reply.Content([]byte(`{"delta":"par"}`)); err != nil {
				t.Errorf("the delivered chunk's write failed: %v", err)
			}
			reply.ServeMidStreamFailure()
		},
	}

	recorder := routingHandler(t, chat)

	if recorder.Code != stdhttp.StatusOK {
		t.Errorf("status = %d, want 200 — no status survives a commitment", recorder.Code)
	}
	want := "data: {\"delta\":\"par\"}\n\n" +
		"data: {\"error\":{\"message\":\"internal error\",\"type\":\"api_error\",\"param\":null,\"code\":null}}\n\n" +
		"data: [DONE]\n\n"
	if got := recorder.Body.String(); got != want {
		t.Errorf("body = %q, want the delivered content, the failure frame and the terminal frame", got)
	}
}

// TestASurfacedRefusalIsAnOrdinaryHTTPEntityPinsThePreCommitmentEnding: the
// walk armed the stream but committed nothing, so the refusal is still a
// status and a body — the failure half's cell, exactly as the table pins it —
// and carries no stream frame and no committed status.
func TestASurfacedRefusalIsAnOrdinaryHTTPEntity(t *testing.T) {
	chat := &fakeRoutingCompletion{
		outcome: application.ChatOutcome{
			Kind:             application.OutcomeRefused,
			Failure:          execution.FailedContextTooLarge,
			RuntimeRequestID: identity.RequestID("01930000-0000-7000-8000-000000000042"),
			Routing: &application.RoutingTrace{
				Alias:          "test-model",
				Attempts:       1,
				LastPosition:   1,
				LastErrorClass: execution.ErrorContextTooLarge,
			},
		},
		act: func(reply application.Reply) {
			reply.Open(true)
			reply.ServeSurfaced(execution.FailedContextTooLarge)
		},
	}

	recorder := routingHandler(t, chat)

	if recorder.Code != stdhttp.StatusBadRequest {
		t.Errorf("status = %d, want 400 — nothing was committed, the status is the caller's to act on", recorder.Code)
	}
	want := "{\"error\":{\"message\":\"this request's context exceeds what the upstream model accepts\",\"type\":\"invalid_request_error\",\"param\":null,\"code\":\"context_too_large\"}}\n"
	if got := recorder.Body.String(); got != want {
		t.Errorf("body = %q, want the refusal's own cell", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q, want the ordinary JSON an uncommitted answer carries", got)
	}
	if strings.Contains(recorder.Body.String(), "data: ") {
		t.Error("a pre-commitment refusal carries a stream frame; the answer was never a stream")
	}
}

// TestTheNoCandidateEndingIsByteIdenticalWithTheAdmissionCell pins today's
// behaviour end to end: an admitted request the walk cannot route — the empty
// registry answers before any candidate — is released and answered with the
// same 503 the admission map always rendered, Retry-After and all. The
// routing stage's arrival at this endpoint must not move one byte of the
// contract a client already retries against.
func TestTheNoCandidateEndingIsByteIdenticalWithTheAdmissionCell(t *testing.T) {
	chat := &fakeRoutingCompletion{
		// The empty walk touches no reply method: the answer is the
		// admission-era cell, and the outcome is the rejection it always was.
		outcome: application.ChatOutcome{
			Kind:             application.OutcomeRejected,
			Reason:           execution.RejectedNoCandidate,
			RuntimeRequestID: identity.RequestID("01930000-0000-7000-8000-000000000042"),
		},
	}

	recorder := routingHandler(t, chat)

	if recorder.Code != stdhttp.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", recorder.Code)
	}
	if got := recorder.Header().Get(retryAfterHeader); got != retryAfterNoCandidate {
		t.Errorf("Retry-After = %q, want %q", got, retryAfterNoCandidate)
	}
	want := "{\"error\":{\"message\":\"the runtime cannot serve this request right now\",\"type\":\"overloaded_error\",\"param\":null,\"code\":null}}\n"
	if got := recorder.Body.String(); got != want {
		t.Errorf("body = %q, want the admission-era cell byte for byte", got)
	}
	if got := recorder.Header().Get(RequestIDHeader); got != "chat-request" {
		t.Errorf("X-Request-Id = %q, want the correlation identifier", got)
	}
}

// TestTheReplyDeliversWhatTheSettlementCounts closes the loop the
// commitment doctrine hangs on: Delivered is the bytes the client received,
// and a settlement that trusts it is counting what actually crossed. The
// framing is excluded — a settlement counts content, not the envelope.
func TestTheReplyDeliversWhatTheSettlementCounts(t *testing.T) {
	var delivered []byte
	chat := &fakeRoutingCompletion{
		outcome: servedOutcome(),
		act: func(reply application.Reply) {
			reply.Open(true)
			_ = reply.Content([]byte(`{"delta":"a"}`))
			_ = reply.Content([]byte(`{"delta":"b"}`))
			delivered = reply.Delivered()
		},
	}
	routingHandler(t, chat)

	if got, want := string(delivered), `{"delta":"a"}{"delta":"b"}`; got != want {
		t.Errorf("delivered = %q, want the content without its framing (%q)", got, want)
	}
}

// TestAnAbandonedRequestWritesNothing pins the one ending with no answer:
// the caller's context ended mid-walk, the walk touched nothing, and the
// transport's only act is the log line. The recorder's empty 200 is what a
// gone caller's connection would carry — bytes nobody reads.
func TestAnAbandonedRequestWritesNothing(t *testing.T) {
	chat := &fakeRoutingCompletion{
		outcome: application.ChatOutcome{
			Kind:             application.OutcomeAbandoned,
			RuntimeRequestID: identity.RequestID("01930000-0000-7000-8000-000000000042"),
			Routing:          &application.RoutingTrace{Alias: "test-model"},
		},
	}

	recorder := routingHandler(t, chat)

	if recorder.Body.Len() != 0 {
		t.Errorf("an abandoned request wrote %q; there is no channel left to answer on", recorder.Body.String())
	}
}
