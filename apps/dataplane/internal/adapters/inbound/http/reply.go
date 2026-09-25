package http

import (
	"bytes"
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
)

// The chat reply: the transport's half of the answer channel the routing
// stage delivers an admitted request's answer through.
//
// One reply per request, built over the ResponseWriter as it arrived — never
// wrapped, because a wrapper that hides Flusher breaks every streamed
// response this runtime writes (the register() doctrine). The routing stage
// holds both halves of it: it hands the reply to each executor as the sink
// content is written into, and it calls the ending methods when the walk
// reaches its decision. That ordering is what makes the commitment point
// real on this transport — the first Content call is the moment the status
// line and content type leave the process, and every method after it knows
// the difference between an answer it can still shape and one it cannot.
//
// The bytes themselves are the wire map's, not this file's inventions: the
// stream's framing is `data: ` + payload + blank line, the terminal frame is
// the [DONE] sentinel, the failure frames are the same bodies the ordinary
// HTTP failures carry, and the pre-commitment endings render the exact cells
// the admission map renders — one table, two transports.

// chatReply implements the application's Reply over one request's writer.
type chatReply struct {
	w       stdhttp.ResponseWriter
	flusher stdhttp.Flusher // nil when the connection cannot flush; the write still lands, buffered

	// stream says the answer travels as Server-Sent Events; false is the one
	// JSON body. Set by Open before the first byte is chosen.
	stream bool

	// committed is the transport's commitment fact: a status line and at
	// least one content-bearing byte left the process. The routing stage
	// trusts this over any executor's claim, and no ending after it may
	// write a status.
	committed bool

	// body is what the client received, kept because the settlement counts
	// it — the gateway's own observation of the delivery side.
	body bytes.Buffer
}

// newChatReply builds the reply over the writer as the handler received it.
func newChatReply(w stdhttp.ResponseWriter) *chatReply {
	flusher, _ := w.(stdhttp.Flusher)
	return &chatReply{w: w, flusher: flusher}
}

// Content writes one chunk of the answer, arming the response on its first
// call — the commitment point. The content type is the framing's, decided by
// Open, and the status is 200: bytes are arriving. For a stream the chunk
// becomes one `data:` frame and is flushed, because a stream that arrives in
// batches is not a stream; for a body it is the body itself. A write error is
// returned to the executor — the client is gone, and the executor is the one
// that knows what its provider call should do about that.
func (c *chatReply) Content(chunk []byte) error {
	if !c.committed {
		if c.stream {
			c.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			c.w.Header().Set("Cache-Control", "no-cache")
		} else {
			c.w.Header().Set("Content-Type", "application/json")
		}
		c.w.WriteHeader(stdhttp.StatusOK)
		c.committed = true
	}
	if c.stream {
		frame := make([]byte, 0, len(chunk)+8)
		frame = append(frame, "data: "...)
		frame = append(frame, chunk...)
		frame = append(frame, '\n', '\n')
		if _, err := c.w.Write(frame); err != nil {
			return err
		}
		c.body.Write(chunk)
		c.flush()
		return nil
	}
	if _, err := c.w.Write(chunk); err != nil {
		return err
	}
	c.body.Write(chunk)
	return nil
}

// Committed reports whether the answer crossed its commitment point.
func (c *chatReply) Committed() bool { return c.committed }

// Delivered is the bytes the client received — the settlement's delivery
// side, counted by the application, never parsed here.
func (c *chatReply) Delivered() []byte { return c.body.Bytes() }

// Open arms the reply's framing before the first candidate runs: the answer's
// shape is the caller's request, frozen at admission, not a fact any
// candidate chooses.
func (c *chatReply) Open(stream bool) { c.stream = stream }

// ServeSucceeded closes a delivered answer. For a stream that is the terminal
// frame; for a body the body was the whole answer and there is nothing to
// add.
func (c *chatReply) ServeSucceeded() {
	if !c.stream {
		return
	}
	_, _ = c.w.Write(streamDoneFrame)
	c.flush()
}

// ServeMidStreamFailure writes a post-commitment failure into the stream it
// belongs to: the failure frame carrying the one body the internal failure
// carries — nothing about the cause, which by now is nobody's to parse — then
// the terminal frame, then close. It has no meaning for a body answer, which
// is written once and complete.
func (c *chatReply) ServeMidStreamFailure() {
	if !c.stream {
		return
	}
	_, _ = c.w.Write(eventFrame(internalFailure().body))
	_, _ = c.w.Write(streamDoneFrame)
	c.flush()
}

// ServeSurfaced answers a pre-commitment surfaced refusal: nothing has been
// written, so the refusal's own cell from the wire map — status, body and
// all — is still entirely the caller's to read.
func (c *chatReply) ServeSurfaced(failure execution.FailureReason) {
	cell := failureCell(failure)
	writeJSON(c.w, cell.status, runtimeErrorResponse{Error: cell.body})
}

// ServeNoCandidate answers the no-candidate ending with the exact cell the
// admission map renders for it — status, body, and the Retry-After advice —
// because the caller's reading of it does not depend on how early the walk
// knew. The request identifier was set before the pipeline ran; the cell
// inherits it.
func (c *chatReply) ServeNoCandidate() {
	cell, retryAfter := rejectionCell(execution.RejectedNoCandidate, "")
	if retryAfter != "" {
		c.w.Header().Set(retryAfterHeader, retryAfter)
	}
	writeJSON(c.w, cell.status, runtimeErrorResponse{Error: cell.body})
}

// flush pushes whatever is buffered toward the client. A connection that
// cannot flush buffers instead — the answer still arrives, only later.
func (c *chatReply) flush() {
	if c.flusher != nil {
		c.flusher.Flush()
	}
}
