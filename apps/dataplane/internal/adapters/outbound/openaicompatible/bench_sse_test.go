package openaicompatible

import (
	"strings"
	"testing"
)

// The streaming read path's benchmark: the SSE frame split, the chunk parse
// and the content-bearing judgment, per frame size. Every octet of every
// streamed answer passes through this reader once, so its cost is the
// streaming path's per-frame overhead — these numbers exist to keep it
// visible, not to chase. The frames are a realistic completion stream: a
// role preamble, content deltas of the size providers actually send, and the
// terminal frames, with the parser started from scratch per iteration to
// price the whole read rather than one frame of a warm one.
func BenchmarkSSEReaderStream(b *testing.B) {
	small := `{"choices":[{"delta":{"content":"ok"}}]}`
	large := `{"choices":[{"delta":{"content":"` + strings.Repeat("x", 2048) + `"}},{"delta":{"role":"assistant"}}],"usage":null}`
	frames := []string{
		`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		small, small, small, large, small, large, small, small,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
	}
	stream := []byte("data: " + strings.Join(frames, "\n\ndata: ") + "\n\n" + "data: [DONE]\n\n")

	b.Run("frames_11", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			reader := newSSEReader(strings.NewReader(string(stream)))
			var read int
			for {
				payload, done, err := reader.next()
				if err != nil {
					b.Fatal(err)
				}
				if done {
					break
				}
				chunk, err := parseChunk(payload)
				if err != nil {
					b.Fatal(err)
				}
				if chunk.contentBearing() {
					read++
				}
			}
			if read != 8 {
				b.Fatalf("content-bearing frames = %d, want 8", read)
			}
		}
	})
}
