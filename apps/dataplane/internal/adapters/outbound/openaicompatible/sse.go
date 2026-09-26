package openaicompatible

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/executors"
)

// sseReader turns a provider stream's bytes into the payloads of its data
// lines. It is deliberately smaller than an SSE implementation: the only
// events an OpenAI-compatible completion stream carries are data lines and
// the terminal sentinel, so everything else — comments, event names,
// dispatch ids, blank lines — is skipped on sight, and each data line is one
// chunk's JSON, which is exactly the unit the sink receives.
//
// One line at a time is also the memory shape: bufio.Scanner never holds
// more than the current frame, and a frame longer than maxSSELineOctets is
// not a chunk anyone sent on purpose — it surfaces as the scanner's error,
// which the stream loop classifies where it stands.
type sseReader struct {
	scanner *bufio.Scanner
}

// maxSSELineOctets is the frame-size wall. Provider chunks run to a few
// kilobytes; a megabyte is already four orders of magnitude past anything a
// completion frame carries.
const maxSSELineOctets = 1 << 20

// doneSentinel is the terminal frame's payload, as the SSE convention spells
// it. It is matched whitespace-blind — see next.
const doneSentinel = "[DONE]"

func newSSEReader(r io.Reader) *sseReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineOctets)
	return &sseReader{scanner: scanner}
}

// next returns the next data line's payload. done is the [DONE] sentinel;
// io.EOF is the stream's end as the provider left it; any other error is a
// stream that can no longer be read, the oversized frame included.
//
// Emptiness and the sentinel are read whitespace-blind: a whitespace-only
// data line, or a trailing space or tab after the sentinel, is the
// provider's typography, not a frame — and mistyping the sentinel breaks the
// stream at its own terminal, the one failure a completed answer must never
// take. The payload itself travels as the line carries it.
func (r *sseReader) next() (payload []byte, done bool, err error) {
	for r.scanner.Scan() {
		line := r.scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payloadText := strings.TrimLeft(strings.TrimPrefix(line, "data:"), " ")
		switch strings.TrimSpace(payloadText) {
		case "":
			continue
		case doneSentinel:
			return nil, true, nil
		}
		return []byte(payloadText), false, nil
	}
	if err := r.scanner.Err(); err != nil {
		return nil, false, err
	}
	return nil, false, io.EOF
}

// providerChunk is the shape a streamed frame is parsed into — just enough
// of the chat.completion.chunk object to know what the frame is: content,
// lifecycle annotation, usage report. The frame's own bytes are what travel;
// this parse is a reading, never a rewrite.
type providerChunk struct {
	Choices []providerChoice `json:"choices"`
	Usage   *providerUsage   `json:"usage"`
}

// providerChoice is one choice of a chunk: its delta and, once present, the
// finish reason that says the provider considers this answer finished.
type providerChoice struct {
	Delta        providerDelta `json:"delta"`
	FinishReason *string       `json:"finish_reason"`
}

// providerDelta is the chunk's increment. Tool-call and function-call
// fragments are content by the same reading as text — routing.md's
// commitment counts them both — so their presence makes a frame
// content-bearing even when no text travels in it.
type providerDelta struct {
	Content      *string           `json:"content"`
	Role         string            `json:"role"`
	ToolCalls    []json.RawMessage `json:"tool_calls"`
	FunctionCall json.RawMessage   `json:"function_call"`
}

// providerUsage is the usage report a provider attaches — to the terminal
// chunk of a stream it was asked to instrument, or to the completion body.
// Each field stands alone because each is reported alone: nil is silence.
type providerUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
}

// asPort renders the report in the port's own vocabulary. A negative count
// is not a report: it is a malformed figure, and passing it through would
// let the settlement's arithmetic price negative tokens — a credit. The
// settlement reads an absent field as "the gateway prices its own
// observation" — the admitted count for input, the delivered tokens for
// output — which is the honest settle for a figure nobody can defend, so a
// negative count is rendered absent here, at the boundary where the
// provider's words become the port's.
func (u *providerUsage) asPort() executors.Usage {
	if u == nil {
		return executors.Usage{}
	}
	return executors.Usage{
		InputTokens:  nonNegative(u.PromptTokens),
		OutputTokens: nonNegative(u.CompletionTokens),
	}
}

// nonNegative nils a count that claims to be negative — absence, as far as
// the settlement is concerned.
func nonNegative(v *int64) *int64 {
	if v == nil || *v < 0 {
		return nil
	}
	return v
}

// parseChunk reads one data-line payload as a chunk.
func parseChunk(payload []byte) (providerChunk, error) {
	var chunk providerChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return providerChunk{}, err
	}
	return chunk, nil
}

// contentBearing reports whether any choice of the chunk carries answer
// material: text, or a tool-call or function-call fragment. An empty-string
// content beside a role is the preamble's shape, not content — which is
// precisely what keeps the preamble bufferable.
func (c providerChunk) contentBearing() bool {
	for _, choice := range c.Choices {
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			return true
		}
		if len(choice.Delta.ToolCalls) > 0 {
			return true
		}
		if len(choice.Delta.FunctionCall) != 0 {
			return true
		}
	}
	return false
}

// finished reports whether the provider marked an answer complete — the
// difference, at the stream's end, between an answer that finished and one
// that was cut.
func (c providerChunk) finished() bool {
	for _, choice := range c.Choices {
		if choice.FinishReason != nil {
			return true
		}
	}
	return false
}
