package http

import (
	stdhttp "net/http"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// The wire table pinned cell by cell, as bytes.
//
// The bodies here are spelled out rather than built from the constants they
// assert, for the reason wireerrors_test.go gives: a test that reuses the
// production rendering proves the rendering is self-consistent, not that it is
// the rendering api/openapi/shared/runtime-errors.yaml contracts. Every cell
// below is one the contract declares; a decision with no cell here is a
// decision no client can be written against.
const (
	invalidAPIKeyBody      = "{\"error\":{\"message\":\"the API key presented with this request is not valid\",\"type\":\"authentication_error\",\"param\":null,\"code\":\"invalid_api_key\"}}\n"
	invalidRequestBody     = "{\"error\":{\"message\":\"the request is not a valid chat completion request\",\"type\":\"invalid_request_error\",\"param\":null,\"code\":\"invalid_request\"}}\n"
	modelParamBody         = "{\"error\":{\"message\":\"the model field is missing or not a valid model name\",\"type\":\"invalid_request_error\",\"param\":\"model\",\"code\":\"invalid_request\"}}\n"
	maxTokensParamBody     = "{\"error\":{\"message\":\"the max_tokens field must be a positive integer\",\"type\":\"invalid_request_error\",\"param\":\"max_tokens\",\"code\":\"invalid_request\"}}\n"
	maxCompletionParamBody = "{\"error\":{\"message\":\"the max_completion_tokens field must be a positive integer\",\"type\":\"invalid_request_error\",\"param\":\"max_completion_tokens\",\"code\":\"invalid_request\"}}\n"
	idempotencyKeyBody     = "{\"error\":{\"message\":\"the Idempotency-Key header is required and must be 1 to 256 printable ASCII characters\",\"type\":\"invalid_request_error\",\"param\":\"idempotency_key\",\"code\":\"invalid_request\"}}\n"
	suspendedBody          = "{\"error\":{\"message\":\"the account that owns this API key is suspended\",\"type\":\"permission_error\",\"param\":null,\"code\":\"account_suspended\"}}\n"
	closedBody             = "{\"error\":{\"message\":\"the account that owns this API key is closed\",\"type\":\"permission_error\",\"param\":null,\"code\":\"account_closed\"}}\n"
	noAccessBody           = "{\"error\":{\"message\":\"the account holds no access to this model\",\"type\":\"permission_error\",\"param\":null,\"code\":\"no_access\"}}\n"
	modelNotFoundBody      = "{\"error\":{\"message\":\"the requested model does not exist\",\"type\":\"not_found_error\",\"param\":\"model\",\"code\":\"model_not_found\"}}\n"
	conflictBody           = "{\"error\":{\"message\":\"this Idempotency-Key was already used with a different request body\",\"type\":\"invalid_request_error\",\"param\":null,\"code\":\"idempotency_conflict\"}}\n"
	inProgressBody         = "{\"error\":{\"message\":\"a request with this Idempotency-Key is still in progress; retry the same request with the same key once it completes\",\"type\":\"invalid_request_error\",\"param\":null,\"code\":\"request_in_progress\"}}\n"
	quotaBody              = "{\"error\":{\"message\":\"the account's quota is insufficient for this request\",\"type\":\"insufficient_quota\",\"param\":null,\"code\":\"insufficient_quota\"}}\n"
	noCandidateBody        = "{\"error\":{\"message\":\"the runtime cannot serve this request right now\",\"type\":\"overloaded_error\",\"param\":null,\"code\":null}}\n"
)

// TestEveryOutcomeKindHasItsWireCell is the table's own closure: one row per
// decision admission can reach, byte-exact, with the headers the contract
// promises for that cell and the absence of the headers it does not.
func TestEveryOutcomeKindHasItsWireCell(t *testing.T) {
	original := identity.RequestID("01930000-0000-7000-8000-000000000000")

	tests := []struct {
		name       string
		outcome    application.ChatOutcome
		wantStatus int
		wantBody   string
		wantRetry  string
		wantReplay bool
	}{
		{
			name: "invalid request about the body as a whole",
			outcome: application.ChatOutcome{
				Kind:   application.OutcomeRejected,
				Reason: execution.RejectedInvalidRequest,
			},
			wantStatus: stdhttp.StatusBadRequest,
			wantBody:   invalidRequestBody,
		},
		{
			name: "invalid request about the model",
			outcome: application.ChatOutcome{
				Kind:   application.OutcomeRejected,
				Reason: execution.RejectedInvalidRequest,
				Detail: application.DetailModel,
			},
			wantStatus: stdhttp.StatusBadRequest,
			wantBody:   modelParamBody,
		},
		{
			name: "invalid request about max_tokens",
			outcome: application.ChatOutcome{
				Kind:   application.OutcomeRejected,
				Reason: execution.RejectedInvalidRequest,
				Detail: application.DetailMaxTokens,
			},
			wantStatus: stdhttp.StatusBadRequest,
			wantBody:   maxTokensParamBody,
		},
		{
			name: "invalid request about max_completion_tokens",
			outcome: application.ChatOutcome{
				Kind:   application.OutcomeRejected,
				Reason: execution.RejectedInvalidRequest,
				Detail: application.DetailMaxCompletionTokens,
			},
			wantStatus: stdhttp.StatusBadRequest,
			wantBody:   maxCompletionParamBody,
		},
		{
			name: "invalid request about the idempotency key",
			outcome: application.ChatOutcome{
				Kind:   application.OutcomeRejected,
				Reason: execution.RejectedInvalidRequest,
				Detail: application.DetailIdempotencyKey,
			},
			wantStatus: stdhttp.StatusBadRequest,
			wantBody:   idempotencyKeyBody,
		},
		{
			name: "a detail the contract does not declare falls back to the generic cell",
			outcome: application.ChatOutcome{
				Kind:   application.OutcomeRejected,
				Reason: execution.RejectedInvalidRequest,
				Detail: application.RejectionDetail("prompt"),
			},
			wantStatus: stdhttp.StatusBadRequest,
			wantBody:   invalidRequestBody,
		},
		{
			name: "a suspended account",
			outcome: application.ChatOutcome{
				Kind:   application.OutcomeRejected,
				Reason: execution.RejectedAccountSuspended,
			},
			wantStatus: stdhttp.StatusForbidden,
			wantBody:   suspendedBody,
		},
		{
			name: "a closed account",
			outcome: application.ChatOutcome{
				Kind:   application.OutcomeRejected,
				Reason: execution.RejectedAccountClosed,
			},
			wantStatus: stdhttp.StatusForbidden,
			wantBody:   closedBody,
		},
		{
			name: "an account holding no access",
			outcome: application.ChatOutcome{
				Kind:   application.OutcomeRejected,
				Reason: execution.RejectedNoAccess,
			},
			wantStatus: stdhttp.StatusForbidden,
			wantBody:   noAccessBody,
		},
		{
			name: "a model name the runtime does not know",
			outcome: application.ChatOutcome{
				Kind:   application.OutcomeRejected,
				Reason: execution.RejectedUnknownAlias,
			},
			wantStatus: stdhttp.StatusNotFound,
			wantBody:   modelNotFoundBody,
		},
		{
			name: "a hold the account cannot fund",
			outcome: application.ChatOutcome{
				Kind:   application.OutcomeRejected,
				Reason: execution.RejectedInsufficientEntitlement,
			},
			wantStatus: stdhttp.StatusTooManyRequests,
			wantBody:   quotaBody,
		},
		{
			name: "no eligible candidate",
			outcome: application.ChatOutcome{
				Kind:   application.OutcomeRejected,
				Reason: execution.RejectedNoCandidate,
			},
			wantStatus: stdhttp.StatusServiceUnavailable,
			wantBody:   noCandidateBody,
			wantRetry:  retryAfterNoCandidate,
		},
		{
			name:       "a request already in flight under its key",
			outcome:    application.ChatOutcome{Kind: application.OutcomeInFlight},
			wantStatus: stdhttp.StatusConflict,
			wantBody:   inProgressBody,
			wantRetry:  retryAfterInProgress,
		},
		{
			name:       "a key already used with another body",
			outcome:    application.ChatOutcome{Kind: application.OutcomeConflict},
			wantStatus: stdhttp.StatusConflict,
			wantBody:   conflictBody,
		},
		{
			name: "a replayed rejection carries the original's cell and the replay headers",
			outcome: application.ChatOutcome{
				Kind:     application.OutcomeReplay,
				Reason:   execution.RejectedNoCandidate,
				Original: original,
			},
			wantStatus: stdhttp.StatusServiceUnavailable,
			wantBody:   noCandidateBody,
			wantRetry:  retryAfterNoCandidate,
			wantReplay: true,
		},
		{
			name: "a replayed permission refusal re-answers as a 403",
			outcome: application.ChatOutcome{
				Kind:     application.OutcomeReplay,
				Reason:   execution.RejectedAccountSuspended,
				Original: original,
			},
			wantStatus: stdhttp.StatusForbidden,
			wantBody:   suspendedBody,
			wantReplay: true,
		},
		{
			name:       "an admitted outcome has no cell yet and answers the internal failure",
			outcome:    application.ChatOutcome{Kind: application.OutcomeAdmitted},
			wantStatus: stdhttp.StatusInternalServerError,
			wantBody:   internalErrorBody,
		},
		{
			name:       "no candidate succeeded is unreachable on this wire and answers the internal failure",
			outcome:    application.ChatOutcome{Kind: application.OutcomeRejected, Reason: execution.RejectedNoCandidateSucceeded},
			wantStatus: stdhttp.StatusInternalServerError,
			wantBody:   internalErrorBody,
		},
		{
			name:       "a reason the vocabulary does not declare answers the internal failure",
			outcome:    application.ChatOutcome{Kind: application.OutcomeRejected, Reason: execution.RejectionReason("who_knows")},
			wantStatus: stdhttp.StatusInternalServerError,
			wantBody:   internalErrorBody,
		},
		{
			name:       "a replay with no reason behind it answers the internal failure",
			outcome:    application.ChatOutcome{Kind: application.OutcomeReplay, Original: original},
			wantStatus: stdhttp.StatusInternalServerError,
			wantBody:   internalErrorBody,
			wantReplay: true,
		},
		{
			name:       "an outcome kind the vocabulary does not declare answers the internal failure",
			outcome:    application.ChatOutcome{Kind: application.OutcomeKind("queued")},
			wantStatus: stdhttp.StatusInternalServerError,
			wantBody:   internalErrorBody,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answer := chatWireCell(tt.outcome)
			if answer.failure.status != tt.wantStatus {
				t.Errorf("status = %d, want %d", answer.failure.status, tt.wantStatus)
			}
			if got := renderChatBody(answer); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
			if answer.retryAfter != tt.wantRetry {
				t.Errorf("Retry-After = %q, want %q", answer.retryAfter, tt.wantRetry)
			}
			if answer.replay != tt.wantReplay {
				t.Errorf("replay = %t, want %t", answer.replay, tt.wantReplay)
			}
			if tt.wantReplay && answer.original != original {
				t.Errorf("original = %q, want %q", answer.original, original)
			}
			// The retry hint belongs to the retryable cells alone: a conflict is
			// terminal for this key and a refusal is the caller's to fix, so a
			// Retry-After on either would advise a retry that cannot succeed.
			if tt.wantRetry == "" && answer.retryAfter != "" {
				t.Errorf("Retry-After = %q on a cell that must not carry it", answer.retryAfter)
			}
			if answer.failure.internal && answer.reason != "" && strings.Contains(tt.wantBody, tt.wantBody) {
				// A defensive cell may still name the decision it could not
				// place; what it must never do is describe the cause in prose.
				// The fixed message above is the whole assertion.
				_ = answer.reason
			}
		})
	}
}

// TestTheConflictCellsDifferOnlyByCode guards the one pair of answers sharing
// a status. Both 409s are invalid_request_error with a null param; what
// separates them is the code and the Retry-After, and a caller must be able to
// tell a waitable collision from a dead key without parsing prose.
func TestTheConflictCellsDifferOnlyByCode(t *testing.T) {
	inFlight := chatWireCell(application.ChatOutcome{Kind: application.OutcomeInFlight})
	conflict := chatWireCell(application.ChatOutcome{Kind: application.OutcomeConflict})

	if inFlight.failure.body.Code == conflict.failure.body.Code {
		t.Errorf("both conflict cells carry code %v; a client cannot tell a waitable collision from a dead key", *conflict.failure.body.Code)
	}
	if inFlight.retryAfter == "" {
		t.Error("the in-flight cell carries no Retry-After; the contract tells the caller to retry the same request")
	}
	if conflict.retryAfter != "" {
		t.Errorf("the conflict cell carries Retry-After %q; a key with a different body must never be retried", conflict.retryAfter)
	}
	for _, answer := range []chatAnswer{inFlight, conflict} {
		if answer.failure.body.Param != nil {
			t.Errorf("a conflict cell carries param %q; neither conflict is about a request field", *answer.failure.body.Param)
		}
	}
}

// TestTheInternalCellNamesNoCause is the table's own leak guard: a decision the
// runtime could not place must render as the one fixed sentence, whatever the
// outcome carried. The rejected cell's reason strings are the runtime's own
// vocabulary, but they belong in the log and not in the prose.
func TestTheInternalCellNamesNoCause(t *testing.T) {
	answers := []chatAnswer{
		chatWireCell(application.ChatOutcome{Kind: application.OutcomeAdmitted}),
		chatWireCell(application.ChatOutcome{Kind: application.OutcomeRejected, Reason: execution.RejectedNoCandidateSucceeded}),
		chatWireCell(application.ChatOutcome{Kind: application.OutcomeKind("queued")}),
	}
	for _, answer := range answers {
		if got := renderChatBody(answer); got != internalErrorBody {
			t.Errorf("an internal cell rendered as %q, want the fixed %q", got, internalErrorBody)
		}
		if !answer.failure.internal {
			t.Error("an internal cell is not marked internal, so its log line would say nothing")
		}
	}
}

// TestTheAdmittedCellLogsTheRuntimesOwnRequestID is the one admission fact the
// log keeps for a stranded outcome: the identity the hold belongs to. An
// operator holding a reservation row and no request must be able to find the
// process that took it, and the wire still carries nothing.
func TestTheAdmittedCellLogsTheRuntimesOwnRequestID(t *testing.T) {
	admitted := application.ChatOutcome{
		Kind:     application.OutcomeAdmitted,
		Admitted: &application.Admission{RuntimeRequestID: identity.RequestID("01930000-0000-7000-8000-000000000042")},
	}
	answer := chatWireCell(admitted)
	if answer.runtimeID != admitted.Admitted.RuntimeRequestID {
		t.Errorf("runtime_request_id = %q, want %q", answer.runtimeID, admitted.Admitted.RuntimeRequestID)
	}
	if answer.reason != "" {
		t.Errorf("the admitted cell names reason %q on an outcome that carries none", answer.reason)
	}
	if got := renderChatBody(answer); got != internalErrorBody {
		t.Errorf("the admitted cell rendered as %q, want the internal failure", got)
	}
}

// renderChatBody encodes one answer the way the writer does, so the table's
// cells can be asserted as the bytes a client reads without a recorder.
func renderChatBody(answer chatAnswer) string {
	recorder := newFlushRecorder()
	writeJSON(recorder, answer.failure.status, runtimeErrorResponse{Error: answer.failure.body})
	return recorder.Body.String()
}
