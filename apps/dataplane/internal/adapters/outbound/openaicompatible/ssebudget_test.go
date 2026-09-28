package openaicompatible

import (
	"errors"
	"strings"
	"testing"
)

// The stream budget's own tests. The wall this pins is the one a per-frame
// limit cannot express: maxSSELineOctets bounds a single frame and says
// nothing about how many frames arrive, so a provider that emits well-formed
// frames forever was — before the budget — read forever, with the runtime
// holding the call open for as long as the client kept reading. These three
// cases are the whole of the property: a real stream is unaffected, an
// endless one is refused, and the refusal cannot be evaded by padding.

func TestAStreamWithinItsBudgetReadsToItsEnd(t *testing.T) {
	frames := make([]string, 0, 64)
	for i := 0; i < 64; i++ {
		frames = append(frames, `{"choices":[{"delta":{"content":"a chunk of text"}}]}`)
	}
	stream := "data: " + strings.Join(frames, "\n\ndata: ") + "\n\ndata: [DONE]\n\n"

	reader := newSSEReader(strings.NewReader(stream))
	read := 0
	for {
		payload, done, err := reader.next()
		if err != nil {
			t.Fatalf("a %d-frame stream failed to read: %v", len(frames), err)
		}
		if done {
			break
		}
		if len(payload) == 0 {
			t.Fatalf("frame %d read as an empty payload", read)
		}
		read++
	}
	if read != 64 {
		t.Fatalf("read %d frames, want all 64 — the budget is far above a real completion", read)
	}
}

func TestAStreamThatNeverEndsIsRefusedAtTheBudget(t *testing.T) {
	// A provider that answers every frame well and then keeps going, forever.
	// Each frame is comfortably inside the per-frame wall, so nothing else in
	// the reader would stop it — only the budget can.
	reader := newSSEReader(&endlessStream{frame: endlessFrame})
	read := 0
	var refusal error
	for {
		_, done, err := reader.next()
		switch {
		case err != nil:
			refusal = err
		case done:
			t.Fatalf("an endless stream read to a terminal frame after %d frames", read)
		default:
			read++
			continue
		}
		break
	}
	if !errors.Is(refusal, errStreamBudgetExhausted) {
		t.Fatalf("got %v, want the budget sentinel", refusal)
	}
	// The bound is a real one, and it is the declared one: a reader that
	// stopped after a handful of frames has stopped on something other than
	// the wall this file is about.
	if read < 1000 {
		t.Fatalf("the budget refused after %d frames, far short of the %d-octet wall — the cap is not what stopped it", read, maxStreamBodyOctets)
	}
}

// endlessFrame is one well-formed data line, newline-delimited and nothing
// else, which is what a provider that never terminates actually sends.
const endlessFrame = `data: {"choices":[{"delta":{"content":"a chunk of text"}}]}` + "\n\n"

// endlessStream is a reader with no end: every Read hands back the next copy
// of one frame. Materialising enough of one of these to reach the wall would
// cost tens of megabytes of test fixture to describe a property that is
// exactly "this does not end" — so it is a type, and the wall is crossed by
// reading rather than by allocating.
type endlessStream struct {
	frame string
	off   int
}

func (e *endlessStream) Read(p []byte) (int, error) {
	if e.off >= len(e.frame) {
		e.off = 0
	}
	n := copy(p, e.frame[e.off:])
	e.off += n
	return n, nil
}

// The budget counts what the provider SENT, not what the reader hands on. A
// stream padded with comments and blank lines costs the same as one padded
// with data, so framing cannot buy unbounded reading — which is the whole
// reason the counter sits on the raw line rather than the payload.
func TestPaddingTheStreamWithCommentsSpendsTheSameBudget(t *testing.T) {
	// Two endless streams: one bare, one with a keepalive comment on every
	// data frame. The budget is spent on the bytes the reader actually reads,
	// so the padded stream must hit the wall in fewer DELIVERED frames — not
	// fewer bytes read, which are equal by definition, but fewer frames that
	// carried answer content.
	bare := newSSEReader(&endlessStream{frame: `data: {"choices":[{"delta":{"content":"x"}}]}` + "\n\n"})
	padded := newSSEReader(&endlessStream{frame: ": a keepalive the reader throws away\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"})

	bareRead := framesBeforeRefusal(t, bare)
	paddedRead := framesBeforeRefusal(t, padded)

	if paddedRead >= bareRead {
		t.Fatalf("the padded stream delivered %d frames before the budget ran out, the plain one %d — padding a stream out must not buy reading", paddedRead, bareRead)
	}
}

// framesBeforeRefusal reads one reader to its refusal and reports how many
// data frames it delivered first.
func framesBeforeRefusal(t *testing.T, reader *sseReader) int {
	t.Helper()
	read := 0
	for {
		_, done, err := reader.next()
		switch {
		case err != nil:
			if !errors.Is(err, errStreamBudgetExhausted) {
				t.Fatalf("the stream failed for a reason other than the budget: %v", err)
			}
			return read
		case done:
			t.Fatal("the stream read to a terminal frame; the fixture is not the endless one this test needs")
		default:
			read++
		}
	}
}

// A stream that is merely large, and not endless, is the case the budget must
// not break: the wall is orders of magnitude above any completion a provider
// would actually send, and a test that pinned the exact frame count would be
// pinning the constant rather than the property.
func TestTheBudgetIsNotAProductionVolume(t *testing.T) {
	// 32 MiB is the declared wall; a completion stream is three orders of
	// magnitude below it. If this assertion ever fails, the constant moved to
	// a value the runtime could actually reach, and the comment on it is
	// wrong.
	if maxStreamBodyOctets <= 1<<20 {
		t.Fatalf("maxStreamBodyOctets = %d, want the total wall far above the %d-octet per-frame wall", maxStreamBodyOctets, maxSSELineOctets)
	}
	// The two walls are named independently, and the total one is the larger:
	// a total below the per-frame bound would make every single frame a
	// refusal.
	if maxStreamBodyOctets < maxSSELineOctets*16 {
		t.Fatalf("maxStreamBodyOctets = %d allows fewer than 16 full-size frames, so a legal stream of maximum frames could be refused for being a stream at all", maxStreamBodyOctets)
	}
}
