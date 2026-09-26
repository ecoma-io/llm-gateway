package openaicompatible

import (
	"context"
	"encoding/json"
	"net"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/executors"
)

// directDial is the plain dial the tests wire in — the shape the egress
// port hands over, without importing the egress adapter, which nothing but
// the composition root may import.
func directDial(ctx context.Context, network, address string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, address)
}

// providerFake is the upstream these tests point the executor at: one
// httptest server whose handler runs under a mutex — the recordings are
// written from server goroutines and read from the test — and which answers
// each request with whatever the test's script returns for its sequence
// number.
type providerFake struct {
	mu       sync.Mutex
	requests []recordedRequest
	script   func(seq int, request recordedRequest) fakeResponse
}

// recordedRequest is one call the provider received, kept whole: the path,
// the headers the executor chose, and the body it translated.
type recordedRequest struct {
	path   string
	header stdhttp.Header
	body   []byte
}

// fakeResponse is one scripted answer.
type fakeResponse struct {
	status int
	header stdhttp.Header
	body   string
}

// startProvider runs the fake; the returned closure reports where it lives.
func startProvider(t *testing.T, script func(seq int, request recordedRequest) fakeResponse) (*providerFake, string) {
	t.Helper()
	fake := &providerFake{script: script}
	server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		body := make([]byte, 0, 1024)
		buffer := make([]byte, 4096)
		for {
			read, err := r.Body.Read(buffer)
			body = append(body, buffer[:read]...)
			if err != nil {
				break
			}
		}
		fake.mu.Lock()
		request := recordedRequest{path: r.URL.Path, header: r.Header.Clone(), body: body}
		fake.requests = append(fake.requests, request)
		response := fake.script(len(fake.requests), request)
		fake.mu.Unlock()
		for key, values := range response.header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.status)
		_, _ = w.Write([]byte(response.body))
	}))
	t.Cleanup(server.Close)
	return fake, server.URL + "/v1/chat/completions"
}

func (p *providerFake) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *providerFake) last() recordedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[len(p.requests)-1]
}

// fakeSink is the answer channel these tests hand the executor. It records
// each Content payload — the transport's framing means each one becomes one
// `data:` frame — and can be told to fail at a chosen call, which is the
// caller-gone shape.
type fakeSink struct {
	mu     sync.Mutex
	chunks []string
	failAt int
}

func newFakeSink() *fakeSink { return &fakeSink{failAt: -1} }

func (s *fakeSink) Content(chunk []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.chunks) == s.failAt {
		return stdhttp.ErrBodyNotAllowed // any error: the caller is gone
	}
	s.chunks = append(s.chunks, string(chunk))
	return nil
}

func (s *fakeSink) Committed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.chunks) > 0
}

// Delivered is the port's delivery observation; recordings is what the
// assertions read — one entry per Content call.
func (s *fakeSink) Delivered() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []byte(strings.Join(s.chunks, ""))
}

func (s *fakeSink) recordings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.chunks...)
}

// newTestExecutor builds the executor over a direct dial and a constructed
// credential — the string is built from repeated letters precisely so no
// test fixture ever carries material that looks like a real credential.
func newTestExecutor(t *testing.T, endpoint string) *Executor {
	t.Helper()
	return New(Target{
		Endpoint:   endpoint,
		Credential: func() (string, bool) { return strings.Repeat("c", 40), true },
		Dial:       directDial,
	})
}

// spec builds one candidate's call: the caller asked for the second model,
// the alias's candidate row answers with the provider's own name, and an
// operator override rides along.
func testSpec(stream bool) executors.AttemptSpec {
	return executors.AttemptSpec{
		RequestID:          "req-1",
		AttemptID:          "att-1",
		BackendID:          "backend-a",
		ProviderModel:      "provider-model-7",
		ParameterOverrides: []byte(`{"temperature":0.5}`),
		Stream:             stream,
		Body:               []byte(`{"model":"caller-alias-name","messages":[{"role":"user","content":"hello"}],"stream":` + boolText(stream) + `}`),
	}
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// completionBody is a minimal non-streaming completion answer.
const completionBody = `{"choices":[{"message":{"role":"assistant","content":"Answered"}}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`

// sse frames for the stream tests, in provider spelling.
const roleChunk = `{"choices":[{"delta":{"role":"assistant","content":""}}]}`
const textChunkOne = `{"choices":[{"delta":{"content":"Hel"}}]}`
const textChunkTwo = `{"choices":[{"delta":{"content":"lo"}}]}`
const finishChunk = `{"choices":[{"delta":{},"finish_reason":"stop"}]}`
const usageChunk = `{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7}}`

func sse(frames ...string) string {
	stream := strings.Builder{}
	for _, frame := range frames {
		stream.WriteString("data: " + frame + "\n\n")
	}
	stream.WriteString("data: [DONE]\n\n")
	return stream.String()
}

func assertClass(t *testing.T, result executors.Result, want execution.ErrorClass) executors.Failure {
	t.Helper()
	failure, ok := result.(executors.Failure)
	if !ok {
		t.Fatalf("Execute result = %T, want Failure classified %s", result, want)
	}
	if failure.Class != want {
		t.Fatalf("Execute class = %s, want %s", failure.Class, want)
	}
	return failure
}

func assertSuccess(t *testing.T, result executors.Result) executors.Success {
	t.Helper()
	success, ok := result.(executors.Success)
	if !ok {
		t.Fatalf("Execute result = %T, want Success", result)
	}
	return success
}

// TestExecuteNonStreamDeliversTheBodyWhole pins the body answer's shape: one
// Content call carrying the provider's completion body verbatim, the usage
// reported through, and the provider's correlation handle off the header.
func TestExecuteNonStreamDeliversTheBodyWhole(t *testing.T) {
	_, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		return fakeResponse{
			status: stdhttp.StatusOK,
			header: stdhttp.Header{"X-Request-Id": []string{"provider-call-42"}},
			body:   completionBody,
		}
	})
	executor := newTestExecutor(t, endpoint)
	sink := newFakeSink()

	result := executor.Execute(t.Context(), testSpec(false), sink)
	success := assertSuccess(t, result)

	if success.Usage.InputTokens == nil || *success.Usage.InputTokens != 5 {
		t.Errorf("usage input = %v, want 5", success.Usage.InputTokens)
	}
	if success.Usage.OutputTokens == nil || *success.Usage.OutputTokens != 7 {
		t.Errorf("usage output = %v, want 7", success.Usage.OutputTokens)
	}
	if success.ProviderRequestID != "provider-call-42" {
		t.Errorf("provider request id = %q, want provider-call-42", success.ProviderRequestID)
	}
	delivered := sink.recordings()
	if len(delivered) != 1 {
		t.Fatalf("Content calls = %d, want exactly one for a body answer", len(delivered))
	}
	if delivered[0] != completionBody {
		t.Errorf("body delivered = %q, want the provider's body verbatim", delivered[0])
	}
}

// TestExecuteTranslatesTheRequestForTheProvider reads the call the provider
// actually received: the model pinned to the candidate's, the override
// applied, the stream switch owned by the gateway, and the usage option
// injected for a streamed call and absent for a body call.
func TestExecuteTranslatesTheRequestForTheProvider(t *testing.T) {
	for _, stream := range []bool{true, false} {
		fake, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
			if stream {
				return fakeResponse{status: stdhttp.StatusOK, body: sse(roleChunk, textChunkOne, finishChunk, usageChunk)}
			}
			return fakeResponse{status: stdhttp.StatusOK, body: completionBody}
		})
		executor := newTestExecutor(t, endpoint)

		executor.Execute(t.Context(), testSpec(stream), newFakeSink())

		if fake.count() != 1 {
			t.Fatalf("stream=%v: the provider saw %d calls, want exactly one", stream, fake.count())
		}
		received := fake.last()
		if received.header.Get("Content-Type") != "application/json" {
			t.Errorf("stream=%v: Content-Type = %q, want application/json", stream, received.header.Get("Content-Type"))
		}
		if stream && received.header.Get("Accept") != "text/event-stream" {
			t.Errorf("stream request Accept = %q, want text/event-stream", received.header.Get("Accept"))
		}
		if !strings.HasPrefix(received.header.Get("Authorization"), "Bearer ") {
			t.Errorf("stream=%v: Authorization = %q, want a bearer credential", stream, received.header.Get("Authorization"))
		}
		translated := decodeObject(t, received.body)
		if translated["model"] != "provider-model-7" {
			t.Errorf("stream=%v: model = %v, want the candidate's provider model over the caller's alias", stream, translated["model"])
		}
		if translated["temperature"] != 0.5 {
			t.Errorf("stream=%v: temperature = %v, want the operator override applied", stream, translated["temperature"])
		}
		if translated["stream"] != stream {
			t.Errorf("stream=%v: stream flag = %v, want %v", stream, translated["stream"], stream)
		}
		options, ok := translated["stream_options"].(map[string]any)
		if stream {
			if !ok || options["include_usage"] != true {
				t.Errorf("streamed call stream_options = %v, want include_usage true — the settlement reads that report", translated["stream_options"])
			}
		} else if ok {
			t.Errorf("body call carried stream_options %v, want none", translated["stream_options"])
		}
	}
}

// TestExecuteStreamForwardsContentAndConsumesTheLifecycle walks the whole
// streamed shape: the preamble is buffered until the first content chunk,
// then travels ahead of it; the finish, usage and [DONE] frames are consumed
// by the loop; the usage report lands in the Result.
func TestExecuteStreamForwardsContentAndConsumesTheLifecycle(t *testing.T) {
	_, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		return fakeResponse{status: stdhttp.StatusOK, body: sse(roleChunk, textChunkOne, textChunkTwo, finishChunk, usageChunk)}
	})
	executor := newTestExecutor(t, endpoint)
	sink := newFakeSink()

	result := executor.Execute(t.Context(), testSpec(true), sink)
	success := assertSuccess(t, result)

	if success.Usage.InputTokens == nil || *success.Usage.InputTokens != 5 {
		t.Errorf("usage input = %v, want 5", success.Usage.InputTokens)
	}
	if success.Usage.OutputTokens == nil || *success.Usage.OutputTokens != 7 {
		t.Errorf("usage output = %v, want 7", success.Usage.OutputTokens)
	}
	delivered := sink.recordings()
	want := []string{roleChunk, textChunkOne, textChunkTwo}
	if len(delivered) != len(want) {
		t.Fatalf("Content calls = %v, want %v — preamble flushed before the first content, lifecycle frames consumed", delivered, want)
	}
	for i := range want {
		if delivered[i] != want[i] {
			t.Errorf("Content call %d = %q, want %q", i, delivered[i], want[i])
		}
	}
	for i, frame := range delivered {
		if strings.Contains(frame, "\n") {
			t.Errorf("Content call %d carries a newline; each frame must be one newline-free payload", i)
		}
	}
}

// TestExecuteStreamBuffersThePreambleAgainstAPreContentDeath: a provider
// that accepts and dies before content leaves the sink untouched — the
// buffer discarded, the attempt unreadable, the walk free to fall through.
func TestExecuteStreamBuffersThePreambleAgainstAPreContentDeath(t *testing.T) {
	_, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		return fakeResponse{status: stdhttp.StatusOK, body: "data: " + roleChunk + "\n\n"}
	})
	executor := newTestExecutor(t, endpoint)
	sink := newFakeSink()

	result := executor.Execute(t.Context(), testSpec(true), sink)
	assertClass(t, result, execution.ErrorInvalidUpstreamResponse)

	if delivered := sink.recordings(); len(delivered) != 0 {
		t.Errorf("Content calls = %v, want none — a pre-content death must leave the answer unwritten", delivered)
	}
	if sink.Committed() {
		t.Error("sink committed = true, want false — nothing reached the client")
	}
}

// TestExecuteStreamCommitsByFlushWhenThePreambleOverflows drives the cap: a
// preamble larger than the buffer commits by flushing what it holds, so
// memory stays bounded and the trade routing.md names is the one made. The
// frames past the overflow are post-commitment lifecycle frames now — the
// window they would have been buffered in is spent — and the loop consumes
// them like the finish and usage frames they sit beside.
func TestExecuteStreamCommitsByFlushWhenThePreambleOverflows(t *testing.T) {
	bigFrame := `{"choices":[{"delta":{"role":"assistant","content":""}}],"padding":"` + strings.Repeat("p", 200) + `"}`
	frames := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		frames = append(frames, bigFrame)
	}
	_, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		return fakeResponse{status: stdhttp.StatusOK, body: sse(frames...)}
	})
	executor := newTestExecutor(t, endpoint)
	executor.preambleCapOctets = 512 // two of the frames above overflow it
	sink := newFakeSink()

	result := executor.Execute(t.Context(), testSpec(true), sink)
	assertSuccess(t, result)

	delivered := sink.recordings()
	if len(delivered) != 2 {
		t.Fatalf("Content calls = %d, want 2 — the frames the buffer held when it overflowed are the ones that travel", len(delivered))
	}
	for i, frame := range delivered {
		if frame != bigFrame {
			t.Errorf("Content call %d = %q, want the buffered preamble frame", i, frame)
		}
	}
	if !sink.Committed() {
		t.Error("sink committed = false, want true — a cap-sized preamble is content for commitment purposes")
	}
}

// TestExecuteStreamClassifiesAPostContentCut: content flowed, then the
// stream died before the provider finished. The sink is committed; the class
// is the stream's own failure.
func TestExecuteStreamClassifiesAPostContentCut(t *testing.T) {
	_, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		return fakeResponse{status: stdhttp.StatusOK, body: "data: " + textChunkOne + "\n\ndata: {\"choices\":[\"\n\n"}
	})
	executor := newTestExecutor(t, endpoint)
	sink := newFakeSink()

	result := executor.Execute(t.Context(), testSpec(true), sink)
	assertClass(t, result, execution.ErrorStreamAfterCommitment)

	if !sink.Committed() {
		t.Error("sink committed = false, want true — content left the process before the cut")
	}
}

// oversizeFrame is one data line past the reader's frame-size wall — the
// scanner refuses to hold it, and the refusal surfaces as the unreadable
// stream the loop classifies where it stands.
func oversizeFrame() string {
	return `{"choices":[{"delta":{"content":""}}],"padding":"` + strings.Repeat("p", (1<<20)+64) + `"}`
}

// TestExecuteStreamWallBeforeCommitmentFallsBack: the frame-size wall broke
// the stream before any content was forwarded. The answer is unreadable, so
// the class is the fallback-eligible one — the walk may still try the next
// candidate — and the sink never saw a byte.
func TestExecuteStreamWallBeforeCommitmentFallsBack(t *testing.T) {
	_, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		return fakeResponse{status: stdhttp.StatusOK, body: "data: " + roleChunk + "\n\ndata: " + oversizeFrame() + "\n\n"}
	})
	executor := newTestExecutor(t, endpoint)
	sink := newFakeSink()

	result := executor.Execute(t.Context(), testSpec(true), sink)
	assertClass(t, result, execution.ErrorInvalidUpstreamResponse)

	if delivered := sink.recordings(); len(delivered) != 0 {
		t.Errorf("Content calls = %v, want none — the wall held before commitment", delivered)
	}
	if sink.Committed() {
		t.Error("sink committed = true, want false — the answer never reached the client")
	}
}

// TestExecuteStreamWallAfterCommitmentSettlesOnDelivered: the same wall,
// met after content flowed. The answer is already out — the class is the
// stream's own post-commitment failure, the settled usage (if the provider
// reported any before the wall) rides it as telemetry, and the delivered
// bytes stand for the ending to price.
func TestExecuteStreamWallAfterCommitmentSettlesOnDelivered(t *testing.T) {
	_, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		return fakeResponse{status: stdhttp.StatusOK, body: "data: " + textChunkOne + "\n\ndata: " + usageChunk + "\n\ndata: " + oversizeFrame() + "\n\n"}
	})
	executor := newTestExecutor(t, endpoint)
	sink := newFakeSink()

	result := executor.Execute(t.Context(), testSpec(true), sink)
	failure := assertClass(t, result, execution.ErrorStreamAfterCommitment)

	if !sink.Committed() {
		t.Error("sink committed = false, want true — content left the process before the wall")
	}
	if failure.Usage.InputTokens == nil || *failure.Usage.InputTokens != 5 {
		t.Errorf("failure usage input tokens = %v, want 5 — the report before the wall is the telemetry that survives", failure.Usage.InputTokens)
	}
}

// TestExecuteStreamStopsAtASinkError: the caller is gone, the executor
// returns at once, and the class is the one the contract assigns a sink
// stop — the walk's abandoned check owns the ending.
func TestExecuteStreamStopsAtASinkError(t *testing.T) {
	fake, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		return fakeResponse{status: stdhttp.StatusOK, body: sse(textChunkOne, textChunkTwo, finishChunk)}
	})
	executor := newTestExecutor(t, endpoint)
	sink := newFakeSink()
	sink.failAt = 0

	result := executor.Execute(t.Context(), testSpec(true), sink)
	failure := assertClass(t, result, execution.ErrorInvalidUpstreamResponse)

	if delivered := sink.recordings(); len(delivered) != 0 {
		t.Errorf("Content calls = %d, want none after the sink refused", len(delivered))
	}
	if failure.ProviderError != nil {
		t.Errorf("provider error = %s, want none — a departed caller has no envelope to keep", failure.ProviderError)
	}
	if fake.count() != 1 {
		t.Errorf("the provider saw %d calls, want one — an adapter re-issues nothing", fake.count())
	}
}

// TestExecuteStreamUsageNilWhenUnreported: silence stays silence — a
// provider that reported nothing settles from what the gateway observed,
// and a zero in the Result would be a claim nobody made.
func TestExecuteStreamUsageNilWhenUnreported(t *testing.T) {
	_, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		return fakeResponse{status: stdhttp.StatusOK, body: sse(textChunkOne, finishChunk)}
	})
	executor := newTestExecutor(t, endpoint)

	result := executor.Execute(t.Context(), testSpec(true), newFakeSink())
	success := assertSuccess(t, result)

	if success.Usage.InputTokens != nil || success.Usage.OutputTokens != nil {
		t.Errorf("usage = %+v, want nil fields — unreported is not zero", success.Usage)
	}
}

// TestExecuteNegativeUsageIsAbsentNotNegative: a negative count is a
// malformed figure, not a report. Rendered through, it would let the
// settlement's arithmetic price negative tokens — a credit; rendered
// absent, the settle basis does what it does for any report that never
// came — it settles on the reservation's own basis.
func TestExecuteNegativeUsageIsAbsentNotNegative(t *testing.T) {
	negativeChunk := `{"choices":[],"usage":{"prompt_tokens":-3,"completion_tokens":-5}}`
	_, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		return fakeResponse{status: stdhttp.StatusOK, body: sse(textChunkOne, finishChunk, negativeChunk)}
	})
	executor := newTestExecutor(t, endpoint)

	result := executor.Execute(t.Context(), testSpec(true), newFakeSink())
	success := assertSuccess(t, result)

	if success.Usage.InputTokens != nil || success.Usage.OutputTokens != nil {
		t.Errorf("usage = %+v, want nil fields — a negative count is not a report, it is absence", success.Usage)
	}
}

// TestExecuteClassifiesRefusals walks the status table: each class the
// vocabulary holds earns the status the convention gives it.
func TestExecuteClassifiesRefusals(t *testing.T) {
	contextEnvelope := `{"error":{"message":"This model's maximum context length is 4096 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`
	genericEnvelope := `{"error":{"message":"Something about the request was not acceptable","type":"invalid_request_error"}}`
	tests := []struct {
		name    string
		status  int
		body    string
		want    execution.ErrorClass
		envelop bool // whether the envelope should survive into the Result
	}{
		{name: "a rate limit is a rate limit", status: 429, body: genericEnvelope, want: execution.ErrorRateLimited, envelop: true},
		{name: "an unaccepted credential is authentication", status: 401, body: genericEnvelope, want: execution.ErrorAuthentication, envelop: true},
		{name: "a forbidden call is authentication too", status: 403, body: genericEnvelope, want: execution.ErrorAuthentication, envelop: true},
		{name: "a 413 is context sized by its status", status: 413, body: genericEnvelope, want: execution.ErrorContextTooLarge, envelop: true},
		{name: "a provider 500 is its own fault", status: 500, body: genericEnvelope, want: execution.ErrorUpstreamError, envelop: true},
		{name: "a request fault without context words is rejected", status: 400, body: genericEnvelope, want: execution.ErrorProviderRejectedRequest, envelop: true},
		{name: "a request fault naming context size is context", status: 400, body: contextEnvelope, want: execution.ErrorContextTooLarge, envelop: true},
		{name: "a redirect is refused, not followed", status: 302, body: "", want: execution.ErrorProviderUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
				return fakeResponse{status: test.status, body: test.body}
			})
			executor := newTestExecutor(t, endpoint)

			result := executor.Execute(t.Context(), testSpec(false), newFakeSink())
			failure := assertClass(t, result, test.want)

			if test.envelop && failure.ProviderError == nil {
				t.Errorf("provider error = nil, want the refusal's redacted envelope")
			}
			if !test.envelop && failure.ProviderError != nil {
				t.Errorf("provider error = %s, want none", failure.ProviderError)
			}
			if fake.count() != 1 {
				t.Errorf("the provider saw %d calls, want one", fake.count())
			}
		})
	}
}

// TestExecuteTransportFailureIsProviderUnavailable: a provider that cannot
// be reached at all — the listener is closed under the executor — is
// unavailable, whatever layer refused.
func TestExecuteTransportFailureIsProviderUnavailable(t *testing.T) {
	server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {}))
	endpoint := server.URL + "/v1/chat/completions"
	server.Close() // nothing is listening, and nothing will be

	executor := newTestExecutor(t, endpoint)
	result := executor.Execute(t.Context(), testSpec(false), newFakeSink())
	assertClass(t, result, execution.ErrorProviderUnavailable)
}

// TestExecuteUnresolvableCredentialNeverCalls: a credential reference that
// cannot be resolved ends the call before anything leaves the process, as
// authentication.
func TestExecuteUnresolvableCredentialNeverCalls(t *testing.T) {
	fake, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		return fakeResponse{status: stdhttp.StatusOK, body: completionBody}
	})
	executor := New(Target{
		Endpoint:   endpoint,
		Credential: func() (string, bool) { return "", false },
		Dial:       directDial,
	})

	result := executor.Execute(t.Context(), testSpec(false), newFakeSink())
	assertClass(t, result, execution.ErrorAuthentication)

	if fake.count() != 0 {
		t.Errorf("the provider saw %d calls, want none — the call ends before the process speaks", fake.count())
	}
}

// TestExecuteRefusesToFollowRedirects pins the client shape the class table
// assumes: a 302 is classified where it stands, never answered.
func TestExecuteRefusesToFollowRedirects(t *testing.T) {
	fake, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		if seq == 1 {
			return fakeResponse{status: stdhttp.StatusFound, header: stdhttp.Header{"Location": []string{"/elsewhere"}}, body: ""}
		}
		return fakeResponse{status: stdhttp.StatusOK, body: completionBody}
	})
	executor := newTestExecutor(t, endpoint)

	result := executor.Execute(t.Context(), testSpec(false), newFakeSink())
	assertClass(t, result, execution.ErrorProviderUnavailable)

	if fake.count() != 1 {
		t.Errorf("the provider saw %d calls, want one — the redirect is a refusal, not an itinerary", fake.count())
	}
}

// TestExecuteRefusalEnvelopeIsRedacted: the envelope the attempt row keeps
// has lost every auth-shaped key and every credential-shaped value, and kept
// the prose a debugging reader needs.
func TestExecuteRefusalEnvelopeIsRedacted(t *testing.T) {
	credential := strings.Repeat("z", 40)
	envelope := `{"error":{"message":"Malformed request","api_key":"` + credential + `","nested":{"authorization":"Bearer ` + credential + `"},"safe":"kept"}}`
	_, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		return fakeResponse{status: 400, body: envelope}
	})
	executor := newTestExecutor(t, endpoint)

	result := executor.Execute(t.Context(), testSpec(false), newFakeSink())
	failure := assertClass(t, result, execution.ErrorProviderRejectedRequest)

	kept := decodeObject(t, failure.ProviderError)
	errorBody, ok := kept["error"].(map[string]any)
	if !ok {
		t.Fatalf("provider error = %s, want the envelope's object shape", failure.ProviderError)
	}
	if errorBody["message"] != "Malformed request" {
		t.Errorf("envelope message = %v, want the prose kept", errorBody["message"])
	}
	if _, gone := errorBody["api_key"]; gone {
		t.Error("envelope carries api_key; auth-shaped keys must not survive redaction")
	}
	nested, ok := errorBody["nested"].(map[string]any)
	if !ok {
		t.Fatalf("envelope nested = %v, want the object kept", errorBody["nested"])
	}
	if _, gone := nested["authorization"]; gone {
		t.Error("envelope carries a nested authorization key; the strip is recursive")
	}
	if errorBody["safe"] != "kept" {
		t.Errorf("envelope safe = %v, want the ordinary fields kept", errorBody["safe"])
	}
}

// TestExecuteDropsAnEnvelopeThatOutgrowsTheCap: a refusal whose envelope
// survives redaction only to overflow the domain's bound is dropped whole —
// the class stands on the status, and no attempt row is endangered.
func TestExecuteDropsAnEnvelopeThatOutgrowsTheCap(t *testing.T) {
	// Spaced prose, not a charset run: a bare [A-Za-z0-9_-] string past 32
	// octets is credential-shaped and would be value-stripped before the
	// cap had its say. This envelope must reach the cap intact.
	padding := `{"error":{"message":"` + strings.Repeat("m ", execution.MaxProviderErrorOctets/2+1) + `"}}`
	_, endpoint := startProvider(t, func(seq int, request recordedRequest) fakeResponse {
		return fakeResponse{status: 500, body: padding}
	})
	executor := newTestExecutor(t, endpoint)

	result := executor.Execute(t.Context(), testSpec(false), newFakeSink())
	failure := assertClass(t, result, execution.ErrorUpstreamError)

	if failure.ProviderError != nil {
		t.Errorf("provider error length = %d, want the envelope dropped whole", len(failure.ProviderError))
	}
}

// decodeObject parses a JSON object for the assertions above, failing the
// test rather than the assertion when the bytes are not an object at all.
func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}
	return decoded
}
