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
// reaches its decision.
//
// Two facts live here, and the difference between them is the settlement's
// guarantee to the client. Committed is the application's: answer content
// exists, crossed by the first Content call — the line past which the walk
// stops trying candidates. Answered is the transport's: at least one byte,
// status line included, has left the process. For a stream the two are the
// same instant — the first frame writes the status — and for a body they
// part: the body buffers whole, because a body answer is one JSON document,
// and a client that holds half of one holds garbage. The buffered bytes
// leave only when the ending states itself — ServeSucceeded after the
// settlement has committed, or the failure cell when it has not — so a
// client never reads success the runtime cannot account for.
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

	// committed is the application's commitment fact: answer content exists.
	// The routing stage trusts this over any executor's claim, and no walk
	// after it tries another candidate.
	committed bool

	// answered is the transport's own fact: a byte has left the process on
	// this connection. The transport's silence is keyed on this — an answer
	// that left may not be written over, and an answer that has not is still
	// the transport's to shape, whatever its content buffers hold.
	answered bool

	// body is the answer's content: for a stream, the chunks that went out
	// frame by frame; for a body, the buffered whole awaiting its ending.
	// Either way it is the settlement's delivery side, counted by the
	// application, never parsed here.
	body bytes.Buffer
}

// newChatReply builds the reply over the writer as the handler received it.
func newChatReply(w stdhttp.ResponseWriter) *chatReply {
	flusher, _ := w.(stdhttp.Flusher)
	return &chatReply{w: w, flusher: flusher}
}

// Content writes one chunk of the answer. The first call crosses the
// commitment point; for a stream it is also the answered one — status line,
// content type and the first frame leave together, and the chunk becomes one
// `data:` frame and is flushed, because a stream that arrives in batches is
// not a stream. For a body the chunk joins the buffer and nothing leaves:
// the body travels whole, at its ending, or not at all. A write error is
// returned to the executor — the client is gone, and the executor is the one
// that knows what its provider call should do about that; a buffered body
// cannot fail.
func (c *chatReply) Content(chunk []byte) error {
	c.committed = true
	if c.stream {
		if !c.answered {
			c.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			c.w.Header().Set("Cache-Control", "no-cache")
			c.w.WriteHeader(stdhttp.StatusOK)
			c.answered = true
		}
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
	c.body.Write(chunk)
	return nil
}

// Committed reports whether the answer crossed its commitment point —
// content exists, whether or not it has left the process.
func (c *chatReply) Committed() bool { return c.committed }

// Answered reports whether a byte has left the process on this connection —
// the fact the transport's silence keys on.
func (c *chatReply) Answered() bool { return c.answered }

// Delivered is the answer's content bytes — for a stream, what was written
// frame by frame; for a body, the buffered whole its ending writes. The
// settlement's delivery side, counted by the application, never parsed here.
func (c *chatReply) Delivered() []byte { return c.body.Bytes() }

// Open arms the reply's framing before the first candidate runs: the answer's
// shape is the caller's request, frozen at admission, not a fact any
// candidate chooses.
func (c *chatReply) Open(stream bool) { c.stream = stream }

// ServeSucceeded closes a delivered answer. For a stream that is the terminal
// frame; for a body it is the moment the buffered answer is allowed to exist
// — the settlement behind it has committed, so the status line and the body
// whole leave together.
func (c *chatReply) ServeSucceeded() {
	if c.stream {
		_, _ = c.w.Write(streamDoneFrame)
		c.flush()
		return
	}
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(stdhttp.StatusOK)
	c.answered = true
	_, _ = c.w.Write(c.body.Bytes())
	c.flush()
}

// ServeMidStreamFailure writes a post-commitment failure into the answer it
// belongs to. For a stream that is the failure frame carrying the one body
// the internal failure carries — nothing about the cause, which by now is
// nobody's to parse — then the terminal frame, then close. For a body the
// wire is still silent, so the failure is an ordinary answer: the buffer is
// discarded whole, and the failure cell is the only thing the client reads —
// never a truncated document that could pass for success.
func (c *chatReply) ServeMidStreamFailure() {
	if c.stream {
		_, _ = c.w.Write(eventFrame(internalFailure().body))
		_, _ = c.w.Write(streamDoneFrame)
		c.flush()
		return
	}
	c.body.Reset()
	cell := internalFailure()
	writeJSON(c.w, cell.status, runtimeErrorResponse{Error: cell.body})
	c.answered = true
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
