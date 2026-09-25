package http

import (
	"bytes"
	"context"
	"errors"
	"log"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
)

// The authentication matrix.
//
// Every way a credential can fail to be one this runtime admits requests for,
// transport shape first, then the application's own verdicts. The whole table
// shares one assertion: the answer is byte-identical. That is the property the
// contract promises and the one a client could attack if it broke — a 401 that
// says "no such key" for one cause and "revoked" for another turns a client, or
// anyone reading its logs, into a probe of the credential store. Two rows at
// the end answer 500 on purpose and live in their own test: a mirror that
// cannot answer is not a verdict about the caller, and answering it as one
// would be the one lie this endpoint must never tell.

// fakeAuthenticator is the application's verification seam, standing in for
// the mirror read the admission use case implements. It records what it was
// asked so the tests can assert the transport's ordering — a request refused
// for its header's shape must never have reached verification at all.
type fakeAuthenticator struct {
	credential application.AuthenticatedCredential
	refusal    *application.Unauthenticated
	err        error

	calls  []string
	sawCtx context.Context
}

func (f *fakeAuthenticator) Authenticate(ctx context.Context, credential string) (application.AuthenticatedCredential, error) {
	f.calls = append(f.calls, credential)
	f.sawCtx = ctx
	switch {
	case f.refusal != nil:
		return application.AuthenticatedCredential{}, f.refusal
	case f.err != nil:
		return application.AuthenticatedCredential{}, f.err
	default:
		return f.credential, nil
	}
}

// fakeChatCompletion is the admission seam. It records what crossed the
// boundary; a nil error hands back the outcome the test set.
type fakeChatCompletion struct {
	inputs  []application.ChatInput
	outcome application.ChatOutcome
	err     error
	// act is what the use case does to the reply before answering, the way
	// the routing stage's endings do. A fake that answers a walk-produced
	// outcome without acting writes the answer from the wrong half — the
	// exact seam the byte-identity suite pins.
	act func(application.Reply)
}

func (f *fakeChatCompletion) Serve(ctx context.Context, in application.ChatInput) (application.ChatOutcome, error) {
	f.inputs = append(f.inputs, in)
	if f.act != nil {
		f.act(in.Reply)
	}
	if f.err != nil {
		return application.ChatOutcome{}, f.err
	}
	return f.outcome, nil
}

const testAPIKey = "test-api-key-0000-0000-0000-0000"

var unauthenticatedMatrix = []struct {
	name string
	// headers is every Authorization value the request presents, in order —
	// one entry for the ordinary case, two for the repeated header.
	headers []string
	// refusal is the verdict verification returns, for the rows that get that
	// far. Nil on the shape rows, which verification must never see.
	refusal *application.Unauthenticated
}{
	{name: "no Authorization header at all"},
	{name: "the header appears twice", headers: []string{"Bearer one", "Bearer " + testAPIKey}},
	{name: "the header is not a bearer credential", headers: []string{"Basic " + testAPIKey}},
	{name: "the header names the scheme with no credential", headers: []string{"Bearer"}},
	{name: "the header carries a credential with no scheme", headers: []string{testAPIKey}},
	{name: "the header carries a third field", headers: []string{"Bearer " + testAPIKey + " extra"}},
	{name: "the credential is empty after the scheme", headers: []string{"Bearer   "}},
	{
		name:    "verification knows no such key",
		headers: []string{"Bearer " + testAPIKey},
		refusal: &application.Unauthenticated{Reason: application.ReasonCredentialUnknown},
	},
	{
		name:    "the key is revoked",
		headers: []string{"Bearer " + testAPIKey},
		refusal: &application.Unauthenticated{Reason: application.ReasonCredentialRevoked},
	},
	{
		name:    "the account the key names is gone",
		headers: []string{"Bearer " + testAPIKey},
		refusal: &application.Unauthenticated{Reason: application.ReasonAccountAbsent},
	},
	{
		name:    "a verdict with a reason the vocabulary does not declare",
		headers: []string{"Bearer " + testAPIKey},
		refusal: &application.Unauthenticated{Reason: application.UnauthenticatedReason("future_cause")},
	},
}

func TestEveryUnauthenticatedCauseAnswersIdentically(t *testing.T) {
	var firstBody string

	for _, tt := range unauthenticatedMatrix {
		t.Run(tt.name, func(t *testing.T) {
			auth := &fakeAuthenticator{refusal: tt.refusal}
			chat := &fakeChatCompletion{}
			handler := newChatCompletionHandler(wiring{auth: auth, chat: chat})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(stdhttp.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
			request.Header.Set(idempotencyKeyHeader, "key-1")
			request.Header[authorizationHeader] = append(request.Header[authorizationHeader], tt.headers...)
			request = request.WithContext(withRequestID(request.Context(), "auth-request"))
			handler(recorder, request)

			if recorder.Code != stdhttp.StatusUnauthorized {
				t.Errorf("status = %d, want 401", recorder.Code)
			}
			if got := recorder.Body.String(); got != invalidAPIKeyBody {
				t.Errorf("body = %q, want the one fixed 401 %q", got, invalidAPIKeyBody)
			}
			if got := recorder.Header().Get(RequestIDHeader); got != "auth-request" {
				t.Errorf("%s = %q, want the request's own id", RequestIDHeader, got)
			}
			if got := recorder.Header().Get(retryAfterHeader); got != "" {
				t.Errorf("a 401 carries Retry-After %q; a bad key does not become good by waiting", got)
			}
			if got := recorder.Header().Get(idempotentReplayHeader); got != "" {
				t.Errorf("a 401 carries %s = %q; an unauthenticated request has no prior decision", idempotentReplayHeader, got)
			}
			// The wire answer must not name which part of the refusal failed.
			// The fixed message is asserted byte-exact above; this asserts the
			// absence of cause vocabulary a client could branch on.
			for _, leak := range []string{"revoked", "unknown", "absent", "authorization_header", "account_state"} {
				if strings.Contains(recorder.Body.String(), leak) {
					t.Errorf("the 401 body leaked the cause %q: %q", leak, recorder.Body.String())
				}
			}
			// And the whole table is one answer: byte-identical to the first
			// row's, which is what turns a per-cause drift into a red build
			// rather than a review note.
			if firstBody == "" {
				firstBody = recorder.Body.String()
			} else if recorder.Body.String() != firstBody {
				t.Errorf("body = %q differs from the matrix's first answer %q", recorder.Body.String(), firstBody)
			}
			// No unauthenticated request reaches admission, whatever refused it.
			if len(chat.inputs) != 0 {
				t.Errorf("admission was called %d times for a request that was never authenticated", len(chat.inputs))
			}
			// A shape refusal has presented nothing verifiable, so the mirror
			// must not be read for one — a read per malformed header is a free
			// denial-of-service the transport's ordering exists to remove.
			if tt.refusal == nil && len(auth.calls) != 0 {
				t.Errorf("verification was called %d times for a header that names no credential", len(auth.calls))
			}
		})
	}
}

func TestVerificationFailuresAnswerInternalNeverUnauthenticated(t *testing.T) {
	// The two non-verdicts. A mirror that would not answer, and a route built
	// with no verification behind it, are defects of the runtime, not facts
	// about the caller — the internal failure is the honest answer, and a 401
	// for either would tell a caller with a perfect key that their key was bad.
	secret := "postgres://user:super-secret@database.example/gateway"
	tests := []struct {
		name  string
		auth  application.Authenticator
		fails bool
	}{
		{name: "the credential mirror would not answer", auth: &fakeAuthenticator{err: errors.New(secret)}, fails: true},
		{name: "verification is not wired at all", auth: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chat := &fakeChatCompletion{}
			handler := newChatCompletionHandler(wiring{auth: tt.auth, chat: chat})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(stdhttp.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
			request.Header.Set(authorizationHeader, "Bearer "+testAPIKey)
			request.Header.Set(idempotencyKeyHeader, "key-1")
			request = request.WithContext(withRequestID(request.Context(), "mirror-request"))
			handler(recorder, request)

			if recorder.Code != stdhttp.StatusInternalServerError {
				t.Errorf("status = %d, want 500", recorder.Code)
			}
			if got := recorder.Body.String(); got != internalErrorBody {
				t.Errorf("body = %q, want the internal failure %q", got, internalErrorBody)
			}
			if strings.Contains(recorder.Body.String(), secret) {
				t.Errorf("the 500 leaked the mirror's cause: %q", recorder.Body.String())
			}
			if len(chat.inputs) != 0 {
				t.Errorf("admission was called %d times for a request whose credential was never verified", len(chat.inputs))
			}
		})
	}
}

func TestTheUnauthenticatedAnswerNeverEchoesThePresentedSecret(t *testing.T) {
	// The strongest form of the same promise: whatever a caller presented, the
	// runtime does not hand it back — in the body, in a header, or in the log.
	// An echoed secret in a 401 is a reflection surface even when the body is
	// otherwise fixed, because the caller's own client logs its answer.
	secret := "test-presented-secret-a-very-distinctive-value-1234567890"

	for _, tt := range unauthenticatedMatrix {
		t.Run(tt.name, func(t *testing.T) {
			auth := &fakeAuthenticator{refusal: tt.refusal}
			chat := &fakeChatCompletion{}
			handler := newChatCompletionHandler(wiring{auth: auth, chat: chat})

			var logs bytes.Buffer
			writer, flags := log.Writer(), log.Flags()
			log.SetOutput(&logs)
			log.SetFlags(0)
			t.Cleanup(func() {
				log.SetOutput(writer)
				log.SetFlags(flags)
			})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(stdhttp.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
			request.Header.Set(idempotencyKeyHeader, "key-1")
			request.Header[authorizationHeader] = append(request.Header[authorizationHeader], tt.headers...)
			handler(recorder, request)

			for _, seen := range [][]byte{recorder.Body.Bytes(), logs.Bytes()} {
				if bytes.Contains(seen, []byte(secret)) {
					t.Errorf("the answer or its log echoed the presented secret: %q / %q", recorder.Body.String(), logs.String())
				}
			}
			// The header's own name never reaches a log line either: the log's
			// whole job on a 401 is the correlation fact.
			if strings.Contains(logs.String(), authorizationHeader) {
				t.Errorf("the log named the credential header: %q", logs.String())
			}
		})
	}
}

func TestAHeaderShapeRefusalNeverReachesAdmission(t *testing.T) {
	// The handler's own wiring defect, positive form: with verification and
	// admission both ready to answer, a request the header's shape refuses
	// still reaches neither. Cost ordering, stated as an assertion.
	auth := &fakeAuthenticator{}
	chat := &fakeChatCompletion{}
	handler := newChatCompletionHandler(wiring{auth: auth, chat: chat})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(stdhttp.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	request.Header.Set(authorizationHeader, "Token "+testAPIKey)
	request.Header.Set(idempotencyKeyHeader, "key-1")
	handler(recorder, request)

	if recorder.Code != stdhttp.StatusUnauthorized {
		t.Errorf("status = %d, want 401", recorder.Code)
	}
	if len(auth.calls) != 0 || len(chat.inputs) != 0 {
		t.Errorf("the shape refusal reached verification (%d) or admission (%d)", len(auth.calls), len(chat.inputs))
	}
}

func TestTheMixedCaseSchemeIsAcceptedAndTheCredentialIsCarriedVerbatim(t *testing.T) {
	// The scheme is case-insensitive by specification; the credential after it
	// is compared byte-for-byte, because a normalised credential is a different
	// credential. Both halves of that are asserted here, since a transport that
	// lowercased the secret would verify a digest no key was ever minted with.
	auth := &fakeAuthenticator{credential: application.AuthenticatedCredential{KeyID: "key-123"}}
	chat := &fakeChatCompletion{
		outcome: application.ChatOutcome{Kind: application.OutcomeRejected, Reason: execution.RejectedNoAccess},
	}
	handler := newChatCompletionHandler(wiring{auth: auth, chat: chat})

	secret := "test-presented-secret-MiXeD_CaSe_0123456789"
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(stdhttp.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	request.Header.Set(authorizationHeader, "bEaReR "+secret)
	request.Header.Set(idempotencyKeyHeader, "key-1")
	handler(recorder, request)

	if len(auth.calls) != 1 || auth.calls[0] != secret {
		t.Errorf("verification saw %q, want the credential exactly as presented %q", auth.calls, secret)
	}
	if auth.sawCtx == nil {
		t.Error("verification was called without the request's context")
	}
	if len(chat.inputs) != 1 {
		t.Fatalf("admission was called %d times, want 1", len(chat.inputs))
	}
	if chat.inputs[0].Credential != "key-123" {
		t.Errorf("admission received credential %q; it must receive the key identity, never the presented secret", chat.inputs[0].Credential)
	}
}

// TestTheVerifiedAccountsFactsRideWithTheInput pins the two fields the handler
// copies from what verification returned: the account the key belongs to, and
// the account lifecycle read in the same statement. The use case gates on this
// copy rather than re-reading the mirror — a re-read would straddle the mirror's
// own write, and a gate taken on a second snapshot is not the gate verification
// made — so the copy is asserted field by field, not assumed.
func TestTheVerifiedAccountsFactsRideWithTheInput(t *testing.T) {
	state := string(projection.LifecycleActive)
	auth := &fakeAuthenticator{credential: application.AuthenticatedCredential{
		KeyID:        "key-123",
		AccountID:    "account-9",
		AccountState: &state,
	}}
	chat := &fakeChatCompletion{
		outcome: application.ChatOutcome{Kind: application.OutcomeRejected, Reason: execution.RejectedNoAccess},
	}
	handler := newChatCompletionHandler(wiring{auth: auth, chat: chat})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(stdhttp.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	request.Header.Set(authorizationHeader, "Bearer "+testAPIKey)
	request.Header.Set(idempotencyKeyHeader, "key-1")
	handler(recorder, request)

	if len(chat.inputs) != 1 {
		t.Fatalf("admission was called %d times, want 1", len(chat.inputs))
	}
	if chat.inputs[0].AccountID != "account-9" {
		t.Errorf("admission received account id %q, want the verified credential's owner", chat.inputs[0].AccountID)
	}
	if chat.inputs[0].AccountState == nil || *chat.inputs[0].AccountState != string(projection.LifecycleActive) {
		t.Errorf("admission received account state %v, want the lifecycle verification read", chat.inputs[0].AccountState)
	}
}

// TestTheAbsentAccountVerdictIsTheOnlyOneTheLogNames pins the one asymmetry
// the 401 carries: a missing account row beside a present credential is a
// mirror-integrity fact an operator can act on, so the log says so. The other
// causes earn nothing, because a per-cause log line would rebuild the probe
// the fixed message refused to be.
func TestTheAbsentAccountVerdictIsTheOnlyOneTheLogNames(t *testing.T) {
	for _, reason := range []application.UnauthenticatedReason{
		application.ReasonCredentialUnknown,
		application.ReasonCredentialRevoked,
		application.ReasonAccountAbsent,
		application.UnauthenticatedReason("future_cause"),
	} {
		t.Run(string(reason), func(t *testing.T) {
			auth := &fakeAuthenticator{refusal: &application.Unauthenticated{Reason: reason}}
			chat := &fakeChatCompletion{}

			var logs bytes.Buffer
			writer, flags := log.Writer(), log.Flags()
			log.SetOutput(&logs)
			log.SetFlags(0)
			t.Cleanup(func() {
				log.SetOutput(writer)
				log.SetFlags(flags)
			})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(stdhttp.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
			request.Header.Set(authorizationHeader, "Bearer "+testAPIKey)
			request = request.WithContext(withRequestID(request.Context(), "log-auth-request"))
			newChatCompletionHandler(wiring{auth: auth, chat: chat})(recorder, request)

			logged := logs.String()
			if !strings.Contains(logged, "log-auth-request") {
				t.Errorf("the 401 log lost the request id: %q", logged)
			}
			if !strings.Contains(logged, "reason=unauthenticated") {
				t.Errorf("the 401 log does not classify the refusal: %q", logged)
			}
			absent := strings.Contains(logged, "account_state=absent")
			if reason == application.ReasonAccountAbsent && !absent {
				t.Errorf("the absent-account refusal was not logged as such: %q", logged)
			}
			if reason != application.ReasonAccountAbsent && absent {
				t.Errorf("cause %q was logged as an absent account: %q", reason, logged)
			}
			// No cause may name the other reasons, whatever the verdict was.
			for _, leak := range []string{"credential_unknown", "credential_revoked", "revoked", "unknown"} {
				if strings.Contains(logged, leak) {
					t.Errorf("the log named the verification cause %q: %q", leak, logged)
				}
			}
		})
	}
}
