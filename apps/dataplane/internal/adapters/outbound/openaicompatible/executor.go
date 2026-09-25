// Package openaicompatible is the executor adapter for providers that speak
// the OpenAI chat-completions wire: one POST of the translated request, and
// one of the two answer shapes back — a completion body read whole, or a
// Server-Sent-Events stream read to its terminal frame. It is the first
// adapter behind the executors port, and it holds every rule the port and
// routing.md put on adapters: it calls upstream exactly once (an adapter
// re-issues nothing), it writes only content-bearing bytes into the sink —
// and, once the answer has crossed commitment, the buffered preamble beside
// them — and it classifies its ending into execution's closed error
// vocabulary, which the routing stage alone turns into a disposition.
//
// What the adapter deliberately does not hold: retry (the routing stage's
// next candidate is the only retry this runtime has), egress policy (the
// dial arrives already resolved), credential material (a resolver closure
// hands over one string at call time and nothing is kept), and any opinion
// about which candidate should have been tried. The registry the routing
// stage reads is built by the composition root, one frozen Executor per
// backend snapshot; Execute does no catalog I/O and no configuration reads.
package openaicompatible

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdhttp "net/http"
	"net/url"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/egress"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/executors"
)

// Target is one frozen executor's wiring, as the composition root resolves
// it from a backend row and the process configuration: where to call, with
// what credential, over which dial, and how long the provider may take to
// answer with headers. Everything is resolved before construction — Execute
// reads none of it from any live source except the credential, whose
// resolution at use is the point of the closure (a rotated environment
// variable is picked up by the next call, not the next restart).
type Target struct {
	// Endpoint is the absolute chat-completions URL of the provider —
	// scheme, host and path complete. A userinfo part is refused: the
	// credential travels in its header, never in the address.
	Endpoint string

	// Credential resolves the bearer material for one call. Reporting
	// absence (or an empty string) is the honest wiring for a reference
	// that cannot be resolved; the call then fails as authentication and
	// nothing leaves the process.
	Credential func() (string, bool)

	// Dial is the established egress the call travels on — direct or a
	// resolved policy. A nil dial is a wiring defect: there is no
	// default route here, because a default is a policy decision the
	// composition root already made.
	Dial egress.DialFunc

	// HeaderTimeout bounds the provider's time to response headers, the
	// one stall a streaming request cannot bound through its context
	// without cutting off the stream that follows. Zero means the
	// transport default of none; a caller gone still ends the call
	// through the context.
	HeaderTimeout time.Duration
}

// Executor is the port's adapter for one OpenAI-compatible backend. It is
// immutable after construction and safe for concurrent use; one instance
// serves every call of one backend snapshot.
type Executor struct {
	endpoint   string
	credential func() (string, bool)
	client     *stdhttp.Client

	// preambleCapOctets is the bound on the pre-content frames the stream
	// path holds back from the sink (routing.md's bounded preamble). The
	// default is generous against every provider's real preamble; the
	// field exists so tests can drive the commit-by-flush behaviour with
	// a cap a stream can actually reach.
	preambleCapOctets int
}

// New builds the executor for one target. The refusals are wiring defects at
// the composition root — an unusable endpoint, a missing dial, no credential
// resolver at all — and they are refused loudly here rather than discovered
// as per-call failures a snapshot later.
func New(target Target) *Executor {
	parsed, err := url.Parse(target.Endpoint)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		panic(fmt.Sprintf("openaicompatible: the endpoint %q is not a usable provider URL (scheme, host, no userinfo)", target.Endpoint))
	}
	if target.Credential == nil {
		panic("openaicompatible: a credential resolver is required; one that reports absence is the honest wiring for an unresolvable reference")
	}
	if target.Dial == nil {
		panic("openaicompatible: an egress dial is required; direct is a dial too, and none is a policy the composition root did not make")
	}
	transport := &stdhttp.Transport{
		DialContext:           target.Dial,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: target.HeaderTimeout,
	}
	return &Executor{
		endpoint:          target.Endpoint,
		credential:        target.Credential,
		client:            &stdhttp.Client{Transport: transport, CheckRedirect: func(*stdhttp.Request, []*stdhttp.Request) error { return stdhttp.ErrUseLastResponse }},
		preambleCapOctets: defaultPreambleCapOctets,
	}
}

// maxCompletionBodyOctets bounds the non-streaming answer read. A completion
// body is bounded far below this by every provider's own output limits; the
// cap is abuse insurance against a hostile or broken upstream, and an
// oversized body is an unreadable answer, not a limit to surface.
const maxCompletionBodyOctets = 32 << 20

// maxRefusalEnvelopeOctets bounds how much of a refusal body is read before
// the redaction pass. Refusal envelopes are small; anything past this is
// dropped unread, and the class still stands on the status.
const maxRefusalEnvelopeOctets = 64 << 10

// defaultPreambleCapOctets is the pre-content buffer's bound — the same
// register as the error envelope's cap, and far above any provider's real
// preamble, so a stream reaches it only when a provider is misbehaving and
// the commit-by-flush trade routing.md describes is the right one.
const defaultPreambleCapOctets = 32768

// Execute performs the one upstream call the port allows: the admitted body
// translated for this provider's wire, one POST over the frozen dial, and
// the answer — stream or body — delivered through the sink. It returns a
// non-nil Result on every path, classified into execution's vocabulary; the
// routing stage reads the class and owns every decision after it.
func (e *Executor) Execute(ctx context.Context, spec executors.AttemptSpec, sink executors.Sink) executors.Result {
	credential, ok := e.credential()
	if !ok || credential == "" {
		// An unresolvable credential reference is an operator's
		// misconfiguration, not a provider fault — but authentication is
		// the class the vocabulary holds for "this call cannot be served
		// with the material at hand", and the disposition table's answer
		// to it is the right one: surface the refusal, try nothing else.
		return executors.Failure{Class: execution.ErrorAuthentication}
	}

	body, err := translate(spec)
	if err != nil {
		// The admitted body failed to translate. This is not an upstream
		// answer in any sense, but the vocabulary has no class for a
		// gateway defect either; unreadable-upstream-response names the
		// honest effect — no servable answer was produced — and the
		// attempt row's provider error stays empty, because there is
		// nothing of the provider's to keep.
		return executors.Failure{Class: execution.ErrorInvalidUpstreamResponse}
	}

	request, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		// The endpoint was validated at construction; a failure here is
		// the same gateway defect as above, classified the same way.
		return executors.Failure{Class: execution.ErrorInvalidUpstreamResponse}
	}
	request.Header.Set("Content-Type", "application/json")
	if spec.Stream {
		request.Header.Set("Accept", "text/event-stream")
	} else {
		request.Header.Set("Accept", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+credential)

	response, err := e.client.Do(request)
	if err != nil {
		// Dial failure, TLS failure, refused redirect to follow, headers
		// past their timeout, context past its budget — from this side of
		// the call they are one thing: the provider could not be reached
		// in time. A caller gone shows up here too; the walk's own
		// abandoned check owns that ending, and this class falls through
		// to the guard either way.
		return executors.Failure{Class: execution.ErrorProviderUnavailable}
	}
	defer func() { _ = response.Body.Close() }()

	requestID := response.Header.Get("X-Request-Id")
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return e.refusal(response, requestID)
	}
	if spec.Stream {
		return e.stream(response, requestID, sink)
	}
	return e.deliverBody(response, requestID, sink)
}

// refusal normalises a non-2xx status: the envelope is read bounded, redacted
// of every auth-shaped key and value, and kept only if it survives both —
// the provider's own account of the fault is telemetry, never a liability,
// and never worth failing the classification over.
func (e *Executor) refusal(response *stdhttp.Response, requestID string) executors.Failure {
	envelope, _ := readBounded(response.Body, maxRefusalEnvelopeOctets)
	return executors.Failure{
		Class:             classifyFailure(response.StatusCode, envelope),
		ProviderError:     redactedEnvelope(envelope),
		ProviderRequestID: requestID,
	}
}

// deliverBody serves the non-streaming answer: the completion body read
// whole and written into the sink once — all-or-nothing, so on a body answer
// a post-commitment failure is unreachable, and the sink's commitment is
// crossed only by a body that was complete and parseable. The body travels
// verbatim: this provider's dialect is the gateway's public dialect, and
// re-marshalling it would be translation where passthrough is the contract.
func (e *Executor) deliverBody(response *stdhttp.Response, requestID string, sink executors.Sink) executors.Result {
	raw, err := readBounded(response.Body, maxCompletionBodyOctets)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return executors.Failure{Class: execution.ErrorProviderUnavailable, ProviderRequestID: requestID}
		}
		// A cut or oversized body never reached the client; the next
		// candidate may still serve the request whole.
		return executors.Failure{Class: execution.ErrorInvalidUpstreamResponse, ProviderRequestID: requestID}
	}
	var probe struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   *providerUsage    `json:"usage"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe.Choices) == 0 {
		// An answer with no choices is an empty answer in a completion's
		// clothing; the vocabulary classifies it unreadable.
		return executors.Failure{Class: execution.ErrorInvalidUpstreamResponse, ProviderRequestID: requestID}
	}
	if err := sink.Content(raw); err != nil {
		return executors.Failure{Class: execution.ErrorInvalidUpstreamResponse, ProviderRequestID: requestID}
	}
	return executors.Success{
		Usage:             probe.Usage.asPort(),
		ProviderRequestID: requestID,
	}
}

// stream serves the streaming answer: the provider's SSE read to its
// terminal frame or EOF, content-bearing chunks written through the sink as
// one newline-free frame each, and every lifecycle frame — the role
// preamble, the finish-reason chunk, the usage chunk, the [DONE] sentinel —
// consumed by this loop rather than forwarded, because the gateway is the
// framer and none of those bytes are content.
//
// The preamble frames are buffered, not forwarded, for exactly the window
// routing.md describes: a provider that accepts the request and dies before
// content is still fallback-eligible, and the buffer is discarded with the
// attempt. Two things end the window: the first content-bearing chunk, which
// flushes the buffer ahead of itself, and the buffer's cap, whose overflow
// commits by flushing — a cap-sized preamble is content for commitment
// purposes.
func (e *Executor) stream(response *stdhttp.Response, requestID string, sink executors.Sink) executors.Result {
	reader := newSSEReader(response.Body)
	var preamble [][]byte
	buffered := 0
	committed := false
	terminal := false
	usage := executors.Usage{}

	flush := func() error {
		for _, frame := range preamble {
			if err := sink.Content(frame); err != nil {
				return err
			}
		}
		preamble, buffered = nil, 0
		return nil
	}
	// sinkStop is the one class a sink error earns, whatever stood behind
	// it: the vocabulary has no word for a departed caller, and the walk's
	// abandoned check owns that ending.
	sinkStop := func() executors.Result {
		return executors.Failure{
			Class:             execution.ErrorInvalidUpstreamResponse,
			ProviderRequestID: requestID,
			Usage:             usage,
		}
	}

	for {
		payload, done, err := reader.next()
		if done {
			terminal = true
			break
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// An unreadable stream — an oversized frame, a read error.
			if committed {
				return executors.Failure{Class: execution.ErrorStreamAfterCommitment, ProviderRequestID: requestID, Usage: usage}
			}
			return executors.Failure{Class: execution.ErrorInvalidUpstreamResponse, ProviderRequestID: requestID}
		}
		chunk, parseErr := parseChunk(payload)
		if parseErr != nil {
			// A frame that will not parse breaks the stream where it
			// stands: before commitment the answer is unreadable, after
			// it the stream has failed, whatever the bytes meant.
			if committed {
				return executors.Failure{Class: execution.ErrorStreamAfterCommitment, ProviderRequestID: requestID, Usage: usage}
			}
			return executors.Failure{Class: execution.ErrorInvalidUpstreamResponse, ProviderRequestID: requestID}
		}
		if chunk.Usage != nil {
			usage = chunk.Usage.asPort()
		}
		if chunk.finished() {
			terminal = true
		}
		if chunk.contentBearing() {
			if !committed {
				if err := flush(); err != nil {
					return sinkStop()
				}
				committed = true
			}
			if err := sink.Content(payload); err != nil {
				return sinkStop()
			}
			continue
		}
		if !committed {
			preamble = append(preamble, payload)
			buffered += len(payload)
			if buffered >= e.preambleCapOctets {
				// Commit by flush: the preamble is now the answer's head.
				if err := flush(); err != nil {
					return sinkStop()
				}
				committed = true
			}
		}
		// Post-commitment lifecycle frames are consumed: the finish
		// reason and the usage report are read above, and nothing of the
		// provider's framing follows the content it annotated.
	}

	if !committed {
		// The stream ended — cleanly or not — having never produced
		// content: an empty answer, which no candidate's success may
		// pretend to be.
		return executors.Failure{Class: execution.ErrorInvalidUpstreamResponse, ProviderRequestID: requestID, Usage: usage}
	}
	if !terminal {
		// Content flowed and the stream was cut before the provider said
		// it had finished: the answer died mid-flight, and what settled
		// on the delivered bytes is the walk's to compute.
		return executors.Failure{Class: execution.ErrorStreamAfterCommitment, ProviderRequestID: requestID, Usage: usage}
	}
	return executors.Success{Usage: usage, ProviderRequestID: requestID}
}

// translate builds the provider's request from the admitted bytes: the
// caller's object carried over, the candidate's model pinned over whatever
// the caller named, the operator's parameter overrides laid on top of the
// caller's values but under the two fields this gateway owns — the stream
// switch, and, for a streamed call, the usage report the settlement reads.
// The map's marshalling is sorted-key deterministic, which is what makes the
// translation a function of the spec rather than of Go's map order.
func translate(spec executors.AttemptSpec) ([]byte, error) {
	request := map[string]json.RawMessage{}
	if len(spec.Body) > 0 {
		if err := json.Unmarshal(spec.Body, &request); err != nil {
			return nil, err
		}
	}
	if len(spec.ParameterOverrides) > 0 {
		var overrides map[string]json.RawMessage
		if err := json.Unmarshal(spec.ParameterOverrides, &overrides); err != nil {
			return nil, err
		}
		for key, value := range overrides {
			request[key] = value
		}
	}
	model, err := json.Marshal(spec.ProviderModel)
	if err != nil {
		return nil, err
	}
	request["model"] = model
	stream, err := json.Marshal(spec.Stream)
	if err != nil {
		return nil, err
	}
	request["stream"] = stream
	if spec.Stream {
		// The usage frame is the settlement's only provider-reported view
		// of a streamed call, and providers send it only when asked. Any
		// stream_options the caller or an override carried is merged
		// under include_usage: the report is the gateway's to require.
		options := map[string]json.RawMessage{}
		if existing, ok := request["stream_options"]; ok {
			if err := json.Unmarshal(existing, &options); err != nil {
				options = map[string]json.RawMessage{}
			}
		}
		options["include_usage"] = json.RawMessage("true")
		merged, err := json.Marshal(options)
		if err != nil {
			return nil, err
		}
		request["stream_options"] = merged
	} else {
		// A body answer carries no stream_options at all: the option is
		// the streamed call's, and passing it through would be asking a
		// question whose answer shape the sink is not framing for.
		delete(request, "stream_options")
	}
	return json.Marshal(request)
}
