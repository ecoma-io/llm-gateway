package http

import (
	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// The admission wire map: every answer POST /v1/chat/completions can produce,
// in one table, because a wire answer that is decided in two places is a wire
// answer that drifts.
//
// The table is a pure function of an admission outcome. Status codes, error
// bodies, headers and log tokens are chosen here and nowhere else; the handler
// executes the answer the table returns, and the table's test pins every cell
// as bytes. This is the same discipline the transport failures in
// wireerrors.go follow — those are facts about the request's arrival (an
// unmatched path, a disallowed method, an unreadable credential), this table
// is facts about admission's decisions — and the two never merge, because a
// cell that both an arrival fact and an admission decision could produce would
// have no owner when they disagree.

// The failure classes this table produces, in the vocabulary
// RuntimeErrorBody declares in shared/runtime-errors.yaml. Three of the four
// are new with admission: the runtime now refuses callers (the
// authentication class), refuses for their account's sake (the permission
// class), and refuses for capacity (insufficient_quota, the OpenAI-compatible
// class a client already treats as "come back later with a smaller request").
// A type this surface cannot produce still has no constant here — adding one
// is a visible edit, as it was for the four constants in wireerrors.go.
const (
	// typeAuthenticationError says the presented credential could not be
	// verified. One message covers every cause, so the answer can never be
	// turned into a probe of the verification pipeline.
	typeAuthenticationError = "authentication_error"

	// typePermissionError says the caller is known and the refusal is an
	// account decision: suspended, closed, or holding no access at all.
	typePermissionError = "permission_error"

	// typeInsufficientQuota says capacity was matched and came up short. It
	// is the runtime's eighth declared class and the one widened into the
	// contract with admission: an OpenAI-compatible client branches on it as
	// capacity rather than correctness.
	typeInsufficientQuota = "insufficient_quota"
)

// The machine-readable refinements the admission wire produces, one per cell
// of the map below. They are spelled as literals for the same reason every
// vocabulary in this repository is: the definition is the contract document,
// and the constant is its restatement.
const (
	codeInvalidAPIKey       = "invalid_api_key"
	codeInvalidRequest      = "invalid_request"
	codeAccountSuspended    = "account_suspended"
	codeAccountClosed       = "account_closed"
	codeNoAccess            = "no_access"
	codeModelNotFound       = "model_not_found"
	codeInsufficientQuota   = "insufficient_quota"
	codeIdempotencyConflict = "idempotency_conflict"
	codeRequestInProgress   = "request_in_progress"

	// The surfaced refusals: the upstream faults a candidate's provider named
	// that the endpoint answers as ordinary HTTP, because nothing was
	// committed when they arrived. Their codes are the failure reasons the
	// request row stores — one vocabulary, two renderings.
	codeProviderRejectedRequest = "provider_rejected_request"
	codeContextTooLarge         = "context_too_large"
	codeUpstreamAuthentication  = "upstream_authentication"
)

// The one message per wire cell. Each is a fixed sentence, never composed
// from request content: a message that echoes what the caller sent is a
// reflection surface, and a message that varies per cause turns the 401 into
// a verification probe. The 401's sentence is shared by every
// authentication refusal the endpoint can produce — transport shape and
// mirror verdict alike — and pinned byte-for-byte by tests.
const (
	invalidAPIKeyMessage       = "the API key presented with this request is not valid"
	invalidRequestMessage      = "the request is not a valid chat completion request"
	modelDetailMessage         = "the model field is missing or not a valid model name"
	maxTokensDetailMessage     = "the max_tokens field must be a positive integer"
	maxCompletionDetailMessage = "the max_completion_tokens field must be a positive integer"
	idempotencyKeyMessage      = "the Idempotency-Key header is required and must be 1 to 256 printable ASCII characters"
	suspendedMessage           = "the account that owns this API key is suspended"
	closedMessage              = "the account that owns this API key is closed"
	noAccessMessage            = "the account holds no access to this model"
	modelNotFoundMessage       = "the requested model does not exist"
	conflictMessage            = "this Idempotency-Key was already used with a different request body"
	inProgressMessage          = "a request with this Idempotency-Key is still in progress; retry the same request with the same key once it completes"
	quotaMessage               = "the account's quota is insufficient for this request"
	noCandidateMessage         = "the runtime cannot serve this request right now"

	// The surfaced refusals' sentences. Each names the shape of the fault and
	// nothing else: not the provider that refused, not the words it refused
	// with — a provider's own error text is its content, and content is
	// exactly what this surface never echoes.
	providerRejectedMessage = "the upstream provider refused this request before any content was generated"
	contextTooLargeMessage  = "this request's context exceeds what the upstream model accepts"
	upstreamAuthMessage     = "the gateway could not authenticate to the upstream provider"
)

// The two Retry-After hints the endpoint sends, in seconds, as strings — the
// HTTP header's own spelling. They are constants, not configuration: the
// contract documents them as the runtime's advice, and advice that varies by
// deployment is advice a client cannot be written against. The in-flight
// hint is short because the original request is already being processed; the
// no-candidate hint is longer because capacity changes on its own.
const (
	retryAfterInProgress  = "1"
	retryAfterNoCandidate = "5"
)

// The headers a replay answer carries, beside the error body every answer
// shares. The values are literals because the contract declares them
// verbatim: the replay mark is the string "true", and the original's runtime
// identity travels under its own header so a client can correlate the answer
// with the request that decided it — never under X-Request-Id, which stays
// this attempt's correlation handle.
const (
	idempotentReplayHeader  = "Idempotent-Replay"
	idempotentReplayValue   = "true"
	originalRequestIDHeader = "X-Original-Request-Id"
	retryAfterHeader        = "Retry-After"
)

// chatAnswer is one wire answer the table returns: the status and body every
// answer has, the two headers only a replay carries, the one header only the
// retryable cells carry, and the tokens the handler's log line is built from.
// The log tokens ride the answer so the log and the wire cannot disagree —
// one decision, rendered twice, by one function.
type chatAnswer struct {
	failure wireFailure

	retryAfter string
	replay     bool
	original   identity.RequestID

	// reason is the log token naming the decision: the rejection reason the
	// row stores, or the idempotency code that refused the request. Empty
	// for answers that name nothing.
	reason string

	// label is the server-side log token naming why this request was refused
	// at the transport's own edge — the credential header's shape, the
	// mirror's verdict. It never crosses the wire: a 401's message is one
	// sentence for every cause, and the cause belongs to the log.
	label string

	// runtimeID is the runtime identity to log, when the outcome carried
	// one.
	runtimeID identity.RequestID

	// silent marks the outcomes whose bytes already travelled through the
	// request's reply — or never could, because the caller was gone — so the
	// handler's only remaining act is the log line. A silent answer writes
	// no status and no body: the wire was the reply's to shape, and re-
	// writing it would mean two answers for one request.
	silent bool

	// routing carries what the routing stage did with an admitted request,
	// set on its three outcomes. It rides the answer for the log line only —
	// the trace names no candidate, no provider model, no provider handle.
	routing *application.RoutingTrace
}

// chatWireCell is the table: one admission outcome, one answer. It is
// exhaustive over the outcome kinds and over the rejection reasons — a reason
// with no cell is a wiring defect, not a caller's answer, and lands on the
// internal failure rather than inventing a plausible one.
//
// The admitted cell deserves its comment. Until the routing half lands, an
// admitted request is released through the compensation seam inside the use
// case and reaches the wire as a no-candidate rejection; the endpoint has no
// success response to write, which is why the contract declares none. An
// admitted outcome arriving here therefore means the release seam did not
// run, and the honest answer is the internal failure — not a fabricated
// refusal that would strand the hold while telling the caller it was never
// opened.
func chatWireCell(outcome application.ChatOutcome) chatAnswer {
	switch outcome.Kind {
	case application.OutcomeRejected:
		cell, retryAfter := rejectionCell(outcome.Reason, outcome.Detail)
		return chatAnswer{
			failure:    cell,
			retryAfter: retryAfter,
			reason:     string(outcome.Reason),
		}
	case application.OutcomeInFlight:
		return chatAnswer{
			failure: wireFailure{
				status: stdhttp.StatusConflict,
				body: runtimeErrorBody{
					Message: inProgressMessage,
					Type:    typeInvalidRequest,
					Code:    stringCode(codeRequestInProgress),
				},
			},
			retryAfter: retryAfterInProgress,
			reason:     codeRequestInProgress,
		}
	case application.OutcomeConflict:
		return chatAnswer{
			failure: wireFailure{
				status: stdhttp.StatusConflict,
				body: runtimeErrorBody{
					Message: conflictMessage,
					Type:    typeInvalidRequest,
					Code:    stringCode(codeIdempotencyConflict),
				},
			},
			reason: codeIdempotencyConflict,
		}
	case application.OutcomeReplay:
		// A replay is the original's decision said again: the same cell the
		// original answered, plus the two headers that say it is a replay.
		// The original's identity rides to the wire, so a client correlating
		// a replay with its first attempt has the runtime's own handle for
		// it. A failed original's decision was one of the surfaced refusals,
		// so its cell comes from the failure half of the table — the same
		// bytes the original was served.
		if outcome.Failure != "" {
			return chatAnswer{
				failure:  failureCell(outcome.Failure),
				replay:   true,
				original: outcome.Original,
				reason:   string(outcome.Failure),
			}
		}
		cell, retryAfter := rejectionCell(outcome.Reason, outcome.Detail)
		return chatAnswer{
			failure:    cell,
			retryAfter: retryAfter,
			replay:     true,
			original:   outcome.Original,
			reason:     string(outcome.Reason),
		}
	case application.OutcomeServed:
		// The answer already travelled through the request's reply — the
		// completed body or stream, or the stream that failed after
		// commitment and carried its failure frame. Nothing is written here;
		// the outcome exists so the log line answers for what the client
		// received.
		return chatAnswer{
			silent:    true,
			reason:    string(application.OutcomeServed),
			runtimeID: outcome.RuntimeRequestID,
			routing:   outcome.Routing,
		}
	case application.OutcomeRefused:
		// The surfaced refusal was written through the reply as the walk
		// reached it — status, body and all, nothing having been committed
		// first. What is left for the table is the log line, naming the
		// refusal the way the request row does.
		return chatAnswer{
			silent:    true,
			reason:    string(outcome.Failure),
			runtimeID: outcome.RuntimeRequestID,
			routing:   outcome.Routing,
		}
	case application.OutcomeAbandoned:
		// The caller's context ended mid-walk; there is no channel left to
		// answer on and nothing was finalised. The outcome is the log's
		// correlation fact and the reaper's, never the wire's.
		return chatAnswer{
			silent:    true,
			reason:    string(application.OutcomeAbandoned),
			runtimeID: outcome.RuntimeRequestID,
			routing:   outcome.Routing,
		}
	case application.OutcomeAdmitted:
		// The routing stage stands between admission and this table inside
		// the use case's own Serve, and an admitted request never leaves it
		// as an outcome. One arriving here means the stage did not run — a
		// wiring defect, answered as the internal failure rather than as any
		// refusal that would strand the hold while telling the caller
		// something false about their request.
		answer := chatAnswer{failure: internalFailure()}
		if outcome.Admitted != nil {
			answer.runtimeID = outcome.Admitted.RuntimeRequestID
		}
		return answer
	default:
		return chatAnswer{failure: internalFailure()}
	}
}

// rejectionCell is the rejection half of the table, shared by the rejected
// and replay kinds because a replay re-answers a rejection's cell. It is
// exhaustive over the rejection vocabulary; an unknown reason is a defect and
// lands on the internal failure — an unknown reason is never a client's
// answer anywhere.
//
// The two no-candidate reasons share one cell on purpose. The caller's
// reading of both is the same sentence — the runtime cannot serve this right
// now, come back later — and how deep the walk went before it knew changes
// nothing a client can act on. Which of the two produced an answer is the log
// line's fact, carried by the reason token, not the wire's.
func rejectionCell(reason execution.RejectionReason, detail application.RejectionDetail) (wireFailure, string) {
	switch reason {
	case execution.RejectedInvalidRequest:
		param, message := invalidRequestDetail(detail)
		return wireFailure{
			status: stdhttp.StatusBadRequest,
			body: runtimeErrorBody{
				Message: message,
				Type:    typeInvalidRequest,
				Param:   stringPtr(param),
				Code:    stringCode(codeInvalidRequest),
			},
		}, ""
	case execution.RejectedAccountSuspended:
		return permissionCell(codeAccountSuspended, suspendedMessage), ""
	case execution.RejectedAccountClosed:
		return permissionCell(codeAccountClosed, closedMessage), ""
	case execution.RejectedNoAccess:
		return permissionCell(codeNoAccess, noAccessMessage), ""
	case execution.RejectedUnknownAlias:
		return wireFailure{
			status: stdhttp.StatusNotFound,
			body: runtimeErrorBody{
				Message: modelNotFoundMessage,
				Type:    typeNotFound,
				Param:   stringPtr(application.DetailModel),
				Code:    stringCode(codeModelNotFound),
			},
		}, ""
	case execution.RejectedInsufficientEntitlement:
		return wireFailure{
			status: stdhttp.StatusTooManyRequests,
			body: runtimeErrorBody{
				Message: quotaMessage,
				Type:    typeInsufficientQuota,
				Code:    stringCode(codeInsufficientQuota),
			},
		}, ""
	case execution.RejectedNoCandidate, execution.RejectedNoCandidateSucceeded:
		return wireFailure{
			status: stdhttp.StatusServiceUnavailable,
			body: runtimeErrorBody{
				Message: noCandidateMessage,
				Type:    typeOverloadedError,
			},
		}, retryAfterNoCandidate
	default:
		return internalFailure(), ""
	}
}

// failureCell is the surfaced refusal's half of the table: the three
// pre-commitment faults a candidate's provider named that this endpoint
// answers as ordinary HTTP, because the walk reached them with nothing
// committed. Each carries its own fixed sentence. The upstream
// authentication cell is the one answered in the internal class on the wire
// — the fault is the gateway's credential, not the caller's request — but it
// is a classified failure all the same: the log line names it through the
// refusal reason, and the internal flag stays reserved for the failures the
// runtime could not place. A reason outside the three is a defect in the
// caller, not a refusal to render, and lands on the internal failure.
func failureCell(failure execution.FailureReason) wireFailure {
	switch failure {
	case execution.FailedProviderRejectedRequest:
		return wireFailure{
			status: stdhttp.StatusBadRequest,
			body: runtimeErrorBody{
				Message: providerRejectedMessage,
				Type:    typeInvalidRequest,
				Code:    stringCode(codeProviderRejectedRequest),
			},
		}
	case execution.FailedContextTooLarge:
		return wireFailure{
			status: stdhttp.StatusBadRequest,
			body: runtimeErrorBody{
				Message: contextTooLargeMessage,
				Type:    typeInvalidRequest,
				Code:    stringCode(codeContextTooLarge),
			},
		}
	case execution.FailedUpstreamAuthentication:
		return wireFailure{
			status: stdhttp.StatusInternalServerError,
			body: runtimeErrorBody{
				Message: upstreamAuthMessage,
				Type:    typeAPIError,
				Code:    stringCode(codeUpstreamAuthentication),
			},
		}
	default:
		return internalFailure()
	}
}

// invalidRequestDetail maps a rejection's detail to the wire body's `param`
// and its one fixed message. The detail is the request field at fault; the
// param is that field's name as the contract declares it; a rejection about
// no one field answers with a null param and the generic sentence.
func invalidRequestDetail(detail application.RejectionDetail) (application.RejectionDetail, string) {
	switch detail {
	case application.DetailModel:
		return detail, modelDetailMessage
	case application.DetailMaxTokens:
		return detail, maxTokensDetailMessage
	case application.DetailMaxCompletionTokens:
		return detail, maxCompletionDetailMessage
	case application.DetailIdempotencyKey:
		return detail, idempotencyKeyMessage
	default:
		return application.DetailNone, invalidRequestMessage
	}
}

// permissionCell builds the one 403 shape: a known caller refused by an
// account decision, the decision in the code, no parameter — the request was
// not what failed, the account was.
func permissionCell(code, message string) wireFailure {
	return wireFailure{
		status: stdhttp.StatusForbidden,
		body: runtimeErrorBody{
			Message: message,
			Type:    typePermissionError,
			Code:    stringCode(code),
		},
	}
}

// stringPtr renders a non-empty detail as the wire body's param, and the
// empty detail as the null every client reads positionally.
func stringPtr(detail application.RejectionDetail) *string {
	if detail == application.DetailNone {
		return nil
	}
	value := string(detail)
	return &value
}
