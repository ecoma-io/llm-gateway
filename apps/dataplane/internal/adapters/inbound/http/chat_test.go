package http

import (
	"bytes"
	"errors"
	"io"
	"log"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// The chat completion handler's own behaviours, end to end over a recorder:
// the cost-ordered steps before admission, the key header's shape, the body's
// bound, the replay headers, and the log schema. The answers' bytes are the
// wire table's business and are pinned in wiremap_test.go; what is pinned here
// is the handler executing those answers — once each, in order, and never
// reaching past a refusal.

// verifiedKey is the AuthenticatedCredential every happy-path test presents,
// and the key identity admission receives instead of the presented secret.
const verifiedKey = "key-00000000-0000-0000-0000-000000000001"

func newChatRequest(body string) *stdhttp.Request {
	request := httptest.NewRequest(stdhttp.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set(authorizationHeader, "Bearer "+testAPIKey)
	request.Header.Set(idempotencyKeyHeader, "key-1")
	return request.WithContext(withRequestID(request.Context(), "chat-request"))
}

func TestTheHappyPathReachesAdmissionOnceWithTheBoundaryVocabulary(t *testing.T) {
	// One request in, one Serve call, carrying exactly what crossed the
	// boundary: the verified key identity — never the presented secret — the
	// key header as presented, the body as read, and a runtime-minted request
	// identity. What comes back is executed once, as the table rendered it.
	chat := &fakeChatCompletion{
		outcome: application.ChatOutcome{Kind: application.OutcomeRejected, Reason: execution.RejectedNoCandidate},
	}
	handler := newChatCompletionHandler(wiring{
		auth: &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: verifiedKey}},
		chat: chat,
	})

	recorder := httptest.NewRecorder()
	handler(recorder, newChatRequest(`{"model":"gpt-x","messages":[]}`))

	if len(chat.inputs) != 1 {
		t.Fatalf("admission was called %d times, want 1", len(chat.inputs))
	}
	in := chat.inputs[0]
	if in.Credential != verifiedKey {
		t.Errorf("admission received credential %q, want the key identity %q", in.Credential, verifiedKey)
	}
	if in.Credential == testAPIKey {
		t.Error("admission received the presented secret; the secret never crosses the boundary")
	}
	if in.IdempotencyKey != "key-1" {
		t.Errorf("admission received idempotency key %q, want the header as presented", in.IdempotencyKey)
	}
	if body, isBytes := in.Body.(application.BodyBytes); !isBytes || string(body) != `{"model":"gpt-x","messages":[]}` {
		t.Errorf("admission received body %v, want the bytes as read", in.Body)
	}
	if in.RequestID == "" {
		t.Error("admission received no runtime request identity")
	}

	if recorder.Code != stdhttp.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", recorder.Code)
	}
}

func TestTheRuntimeRequestIdentityIsFreshPerRequest(t *testing.T) {
	// Two requests through the same handler mint two identities. The identity
	// is the runtime's own handle on the request — the caller's X-Request-Id
	// stays a correlation id and never crosses into admission.
	chat := &fakeChatCompletion{}
	handler := newChatCompletionHandler(wiring{
		auth: &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: verifiedKey}},
		chat: chat,
	})

	for i := 0; i < 2; i++ {
		recorder := httptest.NewRecorder()
		handler(recorder, newChatRequest(`{"model":"gpt-x"}`))
	}
	if len(chat.inputs) != 2 {
		t.Fatalf("admission was called %d times, want 2", len(chat.inputs))
	}
	if chat.inputs[0].RequestID == chat.inputs[1].RequestID {
		t.Errorf("both requests carried the same runtime identity %q", chat.inputs[0].RequestID)
	}
}

func TestTheCallersCorrelationIdentifierStaysACorrelationIdentifier(t *testing.T) {
	// X-Request-Id is echoed back and logged, and it is all of that: a handle
	// for correlating, never the identity admission records. The response
	// header is the caller's own value; admission saw a different one.
	chat := &fakeChatCompletion{}
	handler := newChatCompletionHandler(wiring{
		auth: &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: verifiedKey}},
		chat: chat,
	})

	recorder := httptest.NewRecorder()
	handler(recorder, newChatRequest(`{"model":"gpt-x"}`))

	if got := recorder.Header().Get(RequestIDHeader); got != "chat-request" {
		t.Errorf("%s = %q, want the caller's value", RequestIDHeader, got)
	}
	if chat.inputs[0].RequestID == identity.RequestID("chat-request") {
		t.Error("the caller's correlation id crossed into admission as the request identity")
	}
}

func TestAnAdmissionFailureIsAnsweredInternal(t *testing.T) {
	// Serve's error is the one answer that is not an answer: the use case
	// could not reach a decision, so there is nothing to tell the caller about
	// its request. The cause stays behind the boundary, in body and in log.
	secret := "postgres://user:super-secret@database.example/gateway"
	handler := newChatCompletionHandler(wiring{
		auth: &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: verifiedKey}},
		chat: &fakeChatCompletion{err: errors.New(secret)},
	})

	var logs bytes.Buffer
	writer, flags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(writer)
		log.SetFlags(flags)
	})

	recorder := httptest.NewRecorder()
	handler(recorder, newChatRequest(`{"model":"gpt-x"}`))

	if recorder.Code != stdhttp.StatusInternalServerError {
		t.Errorf("status = %d, want 500", recorder.Code)
	}
	if got := recorder.Body.String(); got != internalErrorBody {
		t.Errorf("body = %q, want the internal failure", got)
	}
	if strings.Contains(recorder.Body.String()+logs.String(), secret) {
		t.Errorf("the admission failure leaked its cause: %q / %q", recorder.Body.String(), logs.String())
	}
	if !strings.Contains(logs.String(), "internal error") {
		t.Errorf("the admission failure was not logged as one: %q", logs.String())
	}
}

func TestTheIdempotencyKeyHeadersShape(t *testing.T) {
	// One header, or none. Absent is admission's refusal to make — the use
	// case records it and refuses with the reason the contract declares, so
	// the test asserts the pass-through, not an answer. More than one is
	// undecidable at the transport — there is no single key to store — and is
	// refused here, with no row written and no admission call.
	tests := []struct {
		name       string
		values     []string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "exactly one key passes through to admission",
			values:     []string{"key-1"},
			wantStatus: stdhttp.StatusServiceUnavailable,
		},
		{
			name:       "no key passes through to admission",
			wantStatus: stdhttp.StatusServiceUnavailable,
		},
		{
			name:       "two values are refused at the transport",
			values:     []string{"key-1", "key-2"},
			wantStatus: stdhttp.StatusBadRequest,
			wantBody:   idempotencyKeyBody,
		},
		{
			name:       "three values are refused at the transport",
			values:     []string{"key-1", "key-2", "key-3"},
			wantStatus: stdhttp.StatusBadRequest,
			wantBody:   idempotencyKeyBody,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chat := &fakeChatCompletion{
				outcome: application.ChatOutcome{Kind: application.OutcomeRejected, Reason: execution.RejectedNoCandidate},
			}
			handler := newChatCompletionHandler(wiring{
				auth: &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: verifiedKey}},
				chat: chat,
			})

			recorder := httptest.NewRecorder()
			request := newChatRequest(`{"model":"gpt-x"}`)
			delete(request.Header, idempotencyKeyHeader)
			request.Header[idempotencyKeyHeader] = append(request.Header[idempotencyKeyHeader], tt.values...)
			handler(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && recorder.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", recorder.Body.String(), tt.wantBody)
			}
			// The pass-through rows reached admission with the key as
			// presented, empty or not; the refusal rows reached nothing.
			switch tt.wantStatus {
			case stdhttp.StatusBadRequest:
				if len(chat.inputs) != 0 {
					t.Errorf("admission was called %d times for a request with no single key to store", len(chat.inputs))
				}
			default:
				if len(chat.inputs) != 1 {
					t.Fatalf("admission was called %d times, want 1", len(chat.inputs))
				}
				want := ""
				if len(tt.values) == 1 {
					want = tt.values[0]
				}
				if chat.inputs[0].IdempotencyKey != want {
					t.Errorf("admission received key %q, want %q", chat.inputs[0].IdempotencyKey, want)
				}
			}
		})
	}
}

func TestTheBodyBoundIsTheContractedConstant(t *testing.T) {
	// The bound is 10485760 bytes because the contract says so, not because a
	// process was configured so. A body at the bound is read whole; one byte
	// over is refused — as 400 when admission answers, never 413 — and the
	// two refusals are different facts for admission to record.
	tests := []struct {
		name       string
		onTheBound bool
		want       application.BodyRefusal
	}{
		{name: "at the bound the body arrives whole", onTheBound: true},
		{name: "one byte over the bound the body is too large", want: application.BodyTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chat := &fakeChatCompletion{}
			handler := newChatCompletionHandler(wiring{
				auth: &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: verifiedKey}},
				chat: chat,
			})

			size := maxChatBodyBytes
			if !tt.onTheBound {
				size++
			}
			request := httptest.NewRequest(stdhttp.MethodPost, "/v1/chat/completions", bytes.NewReader(make([]byte, size)))
			request.Header.Set(authorizationHeader, "Bearer "+testAPIKey)
			request.Header.Set(idempotencyKeyHeader, "key-1")

			recorder := httptest.NewRecorder()
			handler(recorder, request)

			if len(chat.inputs) != 1 {
				t.Fatalf("admission was called %d times, want 1", len(chat.inputs))
			}
			refused, isRefused := chat.inputs[0].Body.(application.BodyRefused)
			if tt.onTheBound {
				if isRefused {
					t.Errorf("a body at the bound arrived as %v, want the bytes", refused.Reason)
				}
				if body, isBytes := chat.inputs[0].Body.(application.BodyBytes); !isBytes || len(body) != maxChatBodyBytes {
					t.Errorf("a body at the bound arrived as %v, want %d bytes", chat.inputs[0].Body, maxChatBodyBytes)
				}
				return
			}
			if !isRefused {
				t.Fatalf("an over-bound body arrived as bytes, want the refusal")
			}
			if refused.Reason != application.BodyTooLarge {
				t.Errorf("refusal reason = %q, want %q", refused.Reason, application.BodyTooLarge)
			}
		})
	}
}

func TestAnUnreadableBodyArrivesAsUnreadable(t *testing.T) {
	// A body that fails before it is whole — a connection that broke mid-read —
	// is a different fact from a body that was too large, and admission
	// records which one happened. The error reader stands in for the break.
	chat := &fakeChatCompletion{}
	handler := newChatCompletionHandler(wiring{
		auth: &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: verifiedKey}},
		chat: chat,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(stdhttp.MethodPost, "/v1/chat/completions", io.MultiReader(
		strings.NewReader(`{"model":`),
		&errorReader{},
	))
	request.Header.Set(authorizationHeader, "Bearer "+testAPIKey)
	request.Header.Set(idempotencyKeyHeader, "key-1")
	handler(recorder, request)

	if len(chat.inputs) != 1 {
		t.Fatalf("admission was called %d times, want 1", len(chat.inputs))
	}
	refused, isRefused := chat.inputs[0].Body.(application.BodyRefused)
	if !isRefused {
		t.Fatalf("a broken body arrived as bytes, want the refusal")
	}
	if refused.Reason != application.BodyUnreadable {
		t.Errorf("refusal reason = %q, want %q", refused.Reason, application.BodyUnreadable)
	}
}

// errorReader is a body that fails partway through being read.
type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("connection reset mid-body") }

func TestReplayAnswersCarryTheReplayHeadersAndTheOriginalIdentity(t *testing.T) {
	// A replay is the original's decision said again, and the two headers are
	// how a client tells it from a first answer: the mark, and the original
	// request's runtime identity — which is not this attempt's identity, and
	// never claims to be.
	original := identity.RequestID("01930000-0000-7000-8000-000000000042")
	chat := &fakeChatCompletion{
		outcome: application.ChatOutcome{
			Kind:     application.OutcomeReplay,
			Reason:   execution.RejectedNoCandidate,
			Original: original,
		},
	}
	handler := newChatCompletionHandler(wiring{
		auth: &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: verifiedKey}},
		chat: chat,
	})

	recorder := httptest.NewRecorder()
	handler(recorder, newChatRequest(`{"model":"gpt-x"}`))

	if got := recorder.Header().Get(idempotentReplayHeader); got != idempotentReplayValue {
		t.Errorf("%s = %q, want %q", idempotentReplayHeader, got, idempotentReplayValue)
	}
	if got := recorder.Header().Get(originalRequestIDHeader); got != string(original) {
		t.Errorf("%s = %q, want the original's identity %q", originalRequestIDHeader, got, original)
	}
	// The mark rides a decision-bearing answer and nothing else. A first
	// answer must never claim to be a replay.
	if got := recorder.Header().Get(retryAfterHeader); got != retryAfterNoCandidate {
		t.Errorf("Retry-After = %q, want the re-answered cell's own %q", got, retryAfterNoCandidate)
	}
	if recorder.Code != stdhttp.StatusServiceUnavailable {
		t.Errorf("status = %d, want the original's 503", recorder.Code)
	}
}

func TestAFirstAnswerNeverClaimsToBeAReplay(t *testing.T) {
	// The negative form, over the decision kinds a first attempt can reach:
	// no replay header, no original identity, whatever the decision was.
	tests := []struct {
		name    string
		outcome application.ChatOutcome
	}{
		{
			name:    "a rejection",
			outcome: application.ChatOutcome{Kind: application.OutcomeRejected, Reason: execution.RejectedNoAccess},
		},
		{
			name:    "an in-flight collision",
			outcome: application.ChatOutcome{Kind: application.OutcomeInFlight},
		},
		{
			name:    "a conflict",
			outcome: application.ChatOutcome{Kind: application.OutcomeConflict},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chat := &fakeChatCompletion{outcome: tt.outcome}
			handler := newChatCompletionHandler(wiring{
				auth: &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: verifiedKey}},
				chat: chat,
			})

			recorder := httptest.NewRecorder()
			handler(recorder, newChatRequest(`{"model":"gpt-x"}`))

			if got := recorder.Header().Get(idempotentReplayHeader); got != "" {
				t.Errorf("%s = %q on a first answer", idempotentReplayHeader, got)
			}
			if got := recorder.Header().Get(originalRequestIDHeader); got != "" {
				t.Errorf("%s = %q on a first answer", originalRequestIDHeader, got)
			}
		})
	}
}

func TestTheDecisionLogCarriesTheSchemaTheContractNames(t *testing.T) {
	// One line per request: the caller's correlation id, the runtime's own
	// request identity when admission decided under one, the replay mark, and
	// the reason the decision was reached. The body, the key and the
	// credential appear nowhere — they are the three things a shared log
	// stream must never hold.
	tests := []struct {
		name      string
		outcome   application.ChatOutcome
		refusal   *application.Unauthenticated
		wantParts []string
		notParts  []string
	}{
		{
			name:      "a no-candidate rejection",
			outcome:   application.ChatOutcome{Kind: application.OutcomeRejected, Reason: execution.RejectedNoCandidate},
			wantParts: []string{"request_id=chat-request", "reason=no_candidate", "runtime_request_id="},
			notParts:  []string{"replayed=true"},
		},
		{
			name: "a replay of one",
			outcome: application.ChatOutcome{
				Kind:     application.OutcomeReplay,
				Reason:   execution.RejectedNoCandidate,
				Original: identity.RequestID("01930000-0000-7000-8000-000000000042"),
			},
			wantParts: []string{"request_id=chat-request", "reason=no_candidate", "replayed=true", "runtime_request_id="},
		},
		{
			name:      "an in-flight collision",
			outcome:   application.ChatOutcome{Kind: application.OutcomeInFlight},
			wantParts: []string{"reason=request_in_progress"},
			notParts:  []string{"replayed=true"},
		},
		{
			name:    "a transport refusal before admission",
			outcome: application.ChatOutcome{},
			// The refusal is verification's, produced before any identity was
			// minted: the log line is the correlation fact and nothing more.
			refusal:   &application.Unauthenticated{Reason: application.ReasonCredentialUnknown},
			wantParts: []string{"reason=unauthenticated"},
			notParts:  []string{"runtime_request_id=", "replayed=true"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chat := &fakeChatCompletion{outcome: tt.outcome}
			auth := &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: verifiedKey}}
			if tt.refusal != nil {
				auth = &fakeAuthenticator{refusal: tt.refusal}
			}
			handler := newChatCompletionHandler(wiring{auth: auth, chat: chat})

			var logs bytes.Buffer
			writer, flags := log.Writer(), log.Flags()
			log.SetOutput(&logs)
			log.SetFlags(0)
			t.Cleanup(func() {
				log.SetOutput(writer)
				log.SetFlags(flags)
			})

			// The body carries what would be the presented secret's neighbours
			// on a real request — a body that echoes a key material — so the
			// leak check below has something real to find if the handler ever
			// started logging bodies.
			recorder := httptest.NewRecorder()
			handler(recorder, newChatRequest(`{"model":"gpt-x","messages":[],"api_key":"`+testAPIKey+`"}`))

			logged := logs.String()
			for _, part := range tt.wantParts {
				if !strings.Contains(logged, part) {
					t.Errorf("the log line is missing %q: %q", part, logged)
				}
			}
			for _, part := range tt.notParts {
				if strings.Contains(logged, part) {
					t.Errorf("the log line carries %q: %q", part, logged)
				}
			}
			for _, secret := range []string{testAPIKey, `"api_key"`} {
				if strings.Contains(logged, secret) {
					t.Errorf("the log line carries %q: %q", secret, logged)
				}
			}
		})
	}
}

func TestTheRejectionReasonVocabularyIsTheRowsVocabulary(t *testing.T) {
	// The reason= token is the rejection reason the request row stores, not a
	// second vocabulary: one decision, named the same way on the row and in
	// the log. Every reason with a wire cell is asserted to travel verbatim.
	for _, reason := range []execution.RejectionReason{
		execution.RejectedInvalidRequest,
		execution.RejectedAccountSuspended,
		execution.RejectedAccountClosed,
		execution.RejectedNoAccess,
		execution.RejectedUnknownAlias,
		execution.RejectedInsufficientEntitlement,
		execution.RejectedNoCandidate,
	} {
		t.Run(string(reason), func(t *testing.T) {
			chat := &fakeChatCompletion{outcome: application.ChatOutcome{Kind: application.OutcomeRejected, Reason: reason}}

			var logs bytes.Buffer
			writer, flags := log.Writer(), log.Flags()
			log.SetOutput(&logs)
			log.SetFlags(0)
			t.Cleanup(func() {
				log.SetOutput(writer)
				log.SetFlags(flags)
			})

			recorder := httptest.NewRecorder()
			handler := newChatCompletionHandler(wiring{
				auth: &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: verifiedKey}},
				chat: chat,
			})
			handler(recorder, newChatRequest(`{"model":"gpt-x"}`))

			if !strings.Contains(logs.String(), "reason="+string(reason)) {
				t.Errorf("the log names %q, want reason=%s", logs.String(), reason)
			}
		})
	}
}

func TestTheNoCandidateAnswerIsJSONNeverAStream(t *testing.T) {
	// Admission's answers precede any stream: nothing has been committed, so
	// the answer is an ordinary HTTP response on the JSON channel. A 503
	// rendered as an event stream would be unanswerable by every client the
	// compatibility contract is written for.
	chat := &fakeChatCompletion{
		outcome: application.ChatOutcome{Kind: application.OutcomeRejected, Reason: execution.RejectedNoCandidate},
	}
	handler := newChatCompletionHandler(wiring{
		auth: &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: verifiedKey}},
		chat: chat,
	})

	recorder := httptest.NewRecorder()
	handler(recorder, newChatRequest(`{"model":"gpt-x","stream":true}`))

	if recorder.Code != stdhttp.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", recorder.Code)
	}
	if got := recorder.Body.String(); strings.Contains(got, "data: ") {
		t.Errorf("the admission answer is an event stream: %q", got)
	}
	if got := recorder.Body.String(); got != noCandidateBody {
		t.Errorf("body = %q, want the contracted JSON cell", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}
