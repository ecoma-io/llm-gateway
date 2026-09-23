package http

import (
	"errors"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
)

// The exact bytes api/openapi/runtime.yaml contracts for a failure, on both
// channels. They are spelled out rather than built from the structs they
// assert, because a test that reuses the production field ordering would pass
// while the wire shape changed — and the wire shape is the whole point of this
// file. `param` and `code` being present and null is part of it: an
// OpenAI-compatible client reads those keys positionally as often as by name.
const (
	notFoundBody         = "{\"error\":{\"message\":\"the requested path is not served by this runtime\",\"type\":\"not_found_error\",\"param\":null,\"code\":null}}\n"
	methodNotAllowedBody = "{\"error\":{\"message\":\"this path does not accept the request method\",\"type\":\"invalid_request_error\",\"param\":null,\"code\":null}}\n"
	notImplementedBody   = "{\"error\":{\"message\":\"this operation is contracted and not implemented\",\"type\":\"api_error\",\"param\":null,\"code\":\"not_implemented\"}}\n"
	internalErrorBody    = "{\"error\":{\"message\":\"internal error\",\"type\":\"api_error\",\"param\":null,\"code\":null}}\n"

	notImplementedFrame = "data: {\"error\":{\"message\":\"this operation is contracted and not implemented\",\"type\":\"api_error\",\"param\":null,\"code\":\"not_implemented\"}}\n\n" +
		"data: [DONE]\n\n"
)

func TestTheRuntimeErrorBodyIsTheOpenAICompatibleShape(t *testing.T) {
	// Every failure this surface can produce, as bytes. The runtime speaks a
	// different error vocabulary from the console and from the Data Plane's
	// management API (ADR 0006 §11), so this test is the place the runtime's
	// half of that split is pinned: an ErrorEnvelope — `request_id` beside
	// `error`, `code` above `message` — appearing here would be the split
	// silently regressing.
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{name: "an unmatched path", err: notFoundError{}, wantCode: stdhttp.StatusNotFound, wantBody: notFoundBody},
		{name: "a disallowed method", err: methodNotAllowedError{}, wantCode: stdhttp.StatusMethodNotAllowed, wantBody: methodNotAllowedBody},
		{name: "a contracted operation that is not built", err: notImplementedError{}, wantCode: stdhttp.StatusNotImplemented, wantBody: notImplementedBody},
		{name: "an unclassified failure", err: errors.New("any cause at all"), wantCode: stdhttp.StatusInternalServerError, wantBody: internalErrorBody},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(stdhttp.MethodGet, "/v1/chat/completions", nil)
			req = req.WithContext(withRequestID(req.Context(), "wire-request"))

			writeError(rec, req, tt.err)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			// The request identifier stays a header on this surface. It is the
			// one thing the runtime's error protocol kept from the shared
			// envelope, and an operator tracing a failed request has nothing
			// else to correlate on.
			if got := rec.Header().Get(RequestIDHeader); got != "wire-request" {
				t.Errorf("%s = %q, want %q", RequestIDHeader, got, "wire-request")
			}
			if strings.Contains(rec.Body.String(), "request_id") {
				t.Errorf("the runtime error body carries the management envelope's request_id: %q", rec.Body.String())
			}
		})
	}
}

func TestAnUnclassifiedFailureNeverReachesTheWire(t *testing.T) {
	// The message a client reads for a failure the runtime could not place is
	// fixed, and the cause is bounded to the server. This is asserted on all
	// three channels, because a leak is a leak whichever one carries it.
	secret := "postgres://user:super-secret@database.example/gateway"

	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "an application internal error", err: application.Internal(errors.New(secret))},
		{name: "a bare error", err: errors.New(secret)},
		{name: "an application error with an unrecognized code", err: &application.Error{Code: application.Code("unexpected"), Message: secret}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(streamErrorFrame(tt.err)); strings.Contains(got, secret) {
				t.Errorf("the stream error frame leaked the cause: %q", got)
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(stdhttp.MethodGet, "/version", nil)
			req = req.WithContext(withRequestID(req.Context(), "secret-request"))
			writeError(rec, req, tt.err)

			if got := rec.Body.String(); strings.Contains(got, secret) {
				t.Errorf("the HTTP error body leaked the cause: %q", got)
			}
		})
	}
}

func TestARuntimeErrorFrameUsesTheManagementEnvelopeVocabularyNever(t *testing.T) {
	// The console's and the management API's code vocabulary — not_found,
	// method_not_allowed, internal — must not appear on this surface's wire.
	// The two enums are different contracts; a runtime response carrying one of
	// the other's members would mean the split had quietly become cosmetic, and
	// a client branching on `error.type` would find a value it does not know.
	foreign := []string{"not_found", "method_not_allowed", "internal"}

	failures := []error{
		notFoundError{},
		methodNotAllowedError{},
		notImplementedError{},
		application.NotFound("nothing here"),
		application.Internal(errors.New("cause")),
		errors.New("bare"),
	}
	for _, err := range failures {
		for _, name := range foreign {
			body := string(streamErrorFrame(err))
			if strings.Contains(body, `"code":"`+name+`"`) {
				t.Errorf("frame for %T carries the management code %q: %q", err, name, body)
			}
		}
	}
}

func TestACommittedStreamFailureEndsInTheDoneFrame(t *testing.T) {
	// Post-commitment failure has one shape and no status: the error frame, the
	// terminal frame, close. A client that has already rendered content must be
	// able to tell "the answer ended in failure" from "the answer ended", and
	// `[DONE]` is how it does.
	for _, err := range []error{notImplementedError{}, application.Internal(errors.New("cause"))} {
		frame := string(streamErrorFrame(err))
		if !strings.HasPrefix(frame, "data: {") {
			t.Errorf("the error frame does not begin as an SSE data frame: %q", frame)
		}
		if !strings.HasSuffix(frame, "\n\n") {
			t.Errorf("the error frame is not terminated by a blank line: %q", frame)
		}
		if strings.Contains(frame, "\n\n\n") {
			t.Errorf("the error frame carries an embedded event separator: %q", frame)
		}
	}

	if got, want := string(streamDoneFrame), "data: [DONE]\n\n"; got != want {
		t.Errorf("the terminal frame = %q, want %q", got, want)
	}
}

func TestACommittedStreamFailureWritesBothFramesAndFlushes(t *testing.T) {
	// The composition of the two frames with the writer is what a streaming
	// handler will call, and the flush is not decoration: without it both
	// frames sit in a buffer waiting for content that is never coming, and the
	// client waits for a connection the runtime has already given up on.
	recorder := newFlushRecorder()

	writeStreamError(recorder, notImplementedError{})

	if got := recorder.Body.String(); got != notImplementedFrame {
		t.Errorf("stream error = %q, want %q", got, notImplementedFrame)
	}
	if !recorder.flushed {
		t.Error("the stream error was written without flushing")
	}
}

func TestACommittedStreamFailureLeavesTheCommittedHeadersAlone(t *testing.T) {
	// A response that has been committed has already sent its status and its
	// headers; the content type of a streamed answer is decided before the
	// first byte, and the failure path must not contradict it. Asserted over a
	// real socket, because an httptest.ResponseRecorder decides a content type
	// of its own when it is flushed and would answer this question about
	// itself rather than about the code.
	server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(stdhttp.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n"))
		writeStreamError(w, notImplementedError{})
	}))
	t.Cleanup(server.Close)

	response, err := stdhttp.Get(server.URL)
	if err != nil {
		t.Fatalf("GET %s error = %v", server.URL, err)
	}
	defer func() { _ = response.Body.Close() }()

	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if response.StatusCode != stdhttp.StatusOK {
		t.Errorf("status = %d, want 200 — a committed stream has no status left to change", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the streamed body error = %v", err)
	}
	if got := string(body); !strings.HasSuffix(got, notImplementedFrame) {
		t.Errorf("body = %q, want it to end with %q", got, notImplementedFrame)
	}
}

// flushRecorder is a ResponseWriter that implements Flusher and remembers
// whether it was flushed, so the flush above can be asserted without a socket.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *flushRecorder) Flush() {
	r.flushed = true
	r.ResponseRecorder.Flush()
}
