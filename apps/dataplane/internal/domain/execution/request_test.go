package execution

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// Every test here pins one clause of the request's contract: born executing,
// finalising exactly once, and each finalisation carrying exactly the fields
// its reason claims. The database refuses an illegal shape a second time on
// the row; these tests keep the legible refusal at formation honest first —
// a transition this package allows that the row would refuse is a caller
// debugging a constraint violation that should have been a domain error.

var (
	// admittedAt is the fixed admission instant every fixture in this file
	// shares; fixed times keep failures readable and the suite deterministic.
	admittedAt = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	finalAt    = admittedAt.Add(4 * time.Second)
)

// admissionSnapshot is the price basis an admitted request carries: a
// revision plus the two resolved unit prices, exactly what its eventual fact
// will be priced from.
func admissionSnapshot() PriceSnapshot {
	return PriceSnapshot{RevisionID: "rev-2026-09", InputUnitPrice: 15, OutputUnitPrice: 60}
}

// admittedRequest builds the fixture every transition test starts from: a
// request born executing at admittedAt. Constructor refusals fail the test —
// the refusal table below owns the refusal cases.
func admittedRequest(t *testing.T) *Request {
	t.Helper()
	request, err := NewRequest("req-0001", "acc-0001", "key-0001", "claude-sonnet-4-5", 512, 1024, admissionSnapshot(), admittedAt)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	return &request
}

// TestNewRequestOpensExecutingWithItsAdmissionSnapshot pins the birth state:
// executing, the admission snapshot and bounds carried whole, FinishedAt
// still zero (a request that has not finalised has no finish to name), and
// Terminal() false — the question every replay answer starts from.
func TestNewRequestOpensExecutingWithItsAdmissionSnapshot(t *testing.T) {
	request, err := NewRequest("req-0001", "acc-0001", "key-0001", "claude-sonnet-4-5", 512, 1024, admissionSnapshot(), admittedAt)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	want := Request{
		ID:              identity.RequestID("req-0001"),
		AccountID:       "acc-0001",
		APIKeyID:        "key-0001",
		Alias:           "claude-sonnet-4-5",
		InputTokens:     512,
		MaxOutputTokens: 1024,
		Price:           admissionSnapshot(),
		Status:          StatusExecuting,
		AdmittedAt:      admittedAt,
	}
	if request != want {
		t.Errorf("NewRequest() = %+v, want %+v — a born request is executing, carries its admission snapshot, and names no finish", request, want)
	}
	if request.FinishedAt != (time.Time{}) {
		t.Errorf("FinishedAt = %v, want the zero time — nothing has finished", request.FinishedAt)
	}
	if request.Terminal() {
		t.Error("Terminal() = true on a born request, want false — executing is the one non-terminal status")
	}
}

// TestNewRequestRefusesAnUnusableAdmission pins each constructor refusal:
// an admitted request knows its id, its owner, its alias, its bounds and its
// price, because the fact of it cannot be settled without them. Every refusal
// here has a twin CHECK on the row; the value of this table is that the caller
// reads the reason at the moment it forms the request, not at the write.
func TestNewRequestRefusesAnUnusableAdmission(t *testing.T) {
	tests := []struct {
		name    string
		id      identity.RequestID
		account string
		key     string
		alias   string
		input   int
		maxOut  int
		price   PriceSnapshot
		wantErr string
	}{
		{
			name:    "rejects an empty id",
			account: "acc-0001", key: "key-0001", alias: "claude-sonnet-4-5", input: 512, maxOut: 1024, price: admissionSnapshot(),
			wantErr: "a request needs an id",
		},
		{
			name: "rejects an empty account",
			id:   "req-0001", key: "key-0001", alias: "claude-sonnet-4-5", input: 512, maxOut: 1024, price: admissionSnapshot(),
			wantErr: "needs its account and api key",
		},
		{
			name: "rejects an empty api key",
			id:   "req-0001", account: "acc-0001", alias: "claude-sonnet-4-5", input: 512, maxOut: 1024, price: admissionSnapshot(),
			wantErr: "needs its account and api key",
		},
		{
			name: "rejects an empty alias",
			id:   "req-0001", account: "acc-0001", key: "key-0001", input: 512, maxOut: 1024, price: admissionSnapshot(),
			wantErr: "an admitted request knows its alias",
		},
		{
			name: "rejects zero input tokens",
			id:   "req-0001", account: "acc-0001", key: "key-0001", alias: "claude-sonnet-4-5", input: 0, maxOut: 1024, price: admissionSnapshot(),
			wantErr: "input tokens must be positive",
		},
		{
			name: "rejects negative input tokens",
			id:   "req-0001", account: "acc-0001", key: "key-0001", alias: "claude-sonnet-4-5", input: -1, maxOut: 1024, price: admissionSnapshot(),
			wantErr: "input tokens must be positive",
		},
		{
			name: "rejects a zero output ceiling",
			id:   "req-0001", account: "acc-0001", key: "key-0001", alias: "claude-sonnet-4-5", input: 512, maxOut: 0, price: admissionSnapshot(),
			wantErr: "max output tokens must be positive",
		},
		{
			name: "rejects a negative output ceiling",
			id:   "req-0001", account: "acc-0001", key: "key-0001", alias: "claude-sonnet-4-5", input: 512, maxOut: -100, price: admissionSnapshot(),
			wantErr: "max output tokens must be positive",
		},
		{
			name: "rejects a missing price revision",
			id:   "req-0001", account: "acc-0001", key: "key-0001", alias: "claude-sonnet-4-5", input: 512, maxOut: 1024,
			wantErr: "carries a price snapshot",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request, err := NewRequest(tt.id, tt.account, tt.key, tt.alias, tt.input, tt.maxOut, tt.price, admittedAt)
			if err == nil {
				t.Fatalf("NewRequest() = %+v, want an error", request)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("NewRequest() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestRejectNewFormsATerminalRowFromTheFieldsKnownSoFar pins the rejection
// row's two claims: it is terminal from birth (Terminal() true, FinishedAt ==
// AdmittedAt — admission refused it, so admission's instant is its finish),
// and it carries only the fields known so far — no token bounds, no price
// snapshot, no committed attempt, no failure reason. Each of the eight
// declared reasons is looped: a reason missing from the accepted set would
// strand an admission step that names it.
func TestRejectNewFormsATerminalRowFromTheFieldsKnownSoFar(t *testing.T) {
	reasons := []RejectionReason{
		RejectedAccountSuspended, RejectedAccountClosed, RejectedUnknownAlias,
		RejectedInvalidRequest, RejectedInsufficientEntitlement, RejectedNoAccess,
		RejectedNoCandidate, RejectedNoCandidateSucceeded,
	}
	for _, reason := range reasons {
		t.Run(string(reason), func(t *testing.T) {
			request, err := RejectNew("req-0001", "acc-0001", "key-0001", "claude-sonnet-4-5", reason, admittedAt)
			if err != nil {
				t.Fatalf("RejectNew(%q) error = %v, want every declared reason accepted", reason, err)
			}
			if request.Status != StatusRejected {
				t.Errorf("Status = %q, want %q", request.Status, StatusRejected)
			}
			if !request.Terminal() {
				t.Error("Terminal() = false, want true — a rejection row is born final")
			}
			if request.FinishedAt != admittedAt {
				t.Errorf("FinishedAt = %v, want %v — admission's instant is the finish of a rejection born terminal", request.FinishedAt, admittedAt)
			}
			if request.RejectionReason != reason {
				t.Errorf("RejectionReason = %q, want %q", request.RejectionReason, reason)
			}
			if request.InputTokens != 0 || request.MaxOutputTokens != 0 {
				t.Errorf("token bounds = (%d, %d), want (0, 0) — the rejection path knows no bounds", request.InputTokens, request.MaxOutputTokens)
			}
			if request.Price != (PriceSnapshot{}) {
				t.Errorf("Price = %+v, want the zero snapshot — the rejection path carries no price", request.Price)
			}
			if request.CommittedAttemptID != "" || request.FailureReason != "" {
				t.Errorf("CommittedAttemptID = %q, FailureReason = %q, want both unset — a rejection claims no execution", request.CommittedAttemptID, request.FailureReason)
			}
		})
	}
}

// TestRejectNewRefusesAnUnknownReason pins the vocabulary check on the
// rejection-born path: a reason outside the eight is a caller's typo or a
// foreign vocabulary, and the error carries the offending value back so the
// log line names what was actually passed.
func TestRejectNewRefusesAnUnknownReason(t *testing.T) {
	for _, reason := range []RejectionReason{"", "account_locked", "Account_Suspended", "no_candidate "} {
		_, err := RejectNew("req-0001", "acc-0001", "key-0001", "claude-sonnet-4-5", reason, admittedAt)
		if err == nil {
			t.Errorf("RejectNew(%q) succeeded, want the unknown reason refused", reason)
			continue
		}
		if !strings.Contains(err.Error(), string(reason)) {
			t.Errorf("RejectNew(%q) error = %q, want it to carry the offending value", reason, err)
		}
	}
}

// TestTransitionsFromExecutingSetExactlyTheirOwnDecision pins each of the
// four transitions on a fresh executing request: the status it lands on, the
// reason it names, the attempt it claims (or refuses to claim — abandoned
// claims none, because the reaper has no evidence for one), and the finish
// it stamps. What each transition does NOT set is pinned as firmly as what
// it does: a leftover reason from a previous life would contradict the row
// the database accepts.
func TestTransitionsFromExecutingSetExactlyTheirOwnDecision(t *testing.T) {
	t.Run("succeed commits the named attempt", func(t *testing.T) {
		request := admittedRequest(t)
		if err := request.Succeed("att-0001", finalAt); err != nil {
			t.Fatalf("Succeed() error = %v", err)
		}
		if request.Status != StatusSucceeded || request.CommittedAttemptID != "att-0001" || request.FinishedAt != finalAt {
			t.Errorf("request = %+v, want succeeded with attempt att-0001 at %v", request, finalAt)
		}
		if request.FailureReason != "" || request.RejectionReason != "" {
			t.Errorf("reasons = (%q, %q), want both unset — a success names no failure and no rejection", request.FailureReason, request.RejectionReason)
		}
	})

	t.Run("failing after commitment commits the named attempt and the reason", func(t *testing.T) {
		request := admittedRequest(t)
		if err := request.FailAfterCommitment("att-0001", finalAt); err != nil {
			t.Fatalf("FailAfterCommitment() error = %v", err)
		}
		if request.Status != StatusFailed || request.FailureReason != FailedStreamAfterCommitment {
			t.Errorf("status = %q, reason = %q, want failed / %q", request.Status, request.FailureReason, FailedStreamAfterCommitment)
		}
		if request.CommittedAttemptID != "att-0001" || request.FinishedAt != finalAt {
			t.Errorf("attempt = %q, finishedAt = %v, want att-0001 at %v — the in-flight usage belongs to this attempt", request.CommittedAttemptID, request.FinishedAt, finalAt)
		}
		if request.RejectionReason != "" {
			t.Errorf("RejectionReason = %q, want unset", request.RejectionReason)
		}
	})

	t.Run("failing abandoned claims no attempt", func(t *testing.T) {
		request := admittedRequest(t)
		if err := request.FailAbandoned(finalAt); err != nil {
			t.Fatalf("FailAbandoned() error = %v", err)
		}
		if request.Status != StatusFailed || request.FailureReason != FailedGatewayAbandoned {
			t.Errorf("status = %q, reason = %q, want failed / %q", request.Status, request.FailureReason, FailedGatewayAbandoned)
		}
		if request.CommittedAttemptID != "" {
			t.Errorf("CommittedAttemptID = %q, want empty — the reaper cannot know whether a commitment happened, so it claims none", request.CommittedAttemptID)
		}
		if request.FinishedAt != finalAt {
			t.Errorf("FinishedAt = %v, want %v", request.FinishedAt, finalAt)
		}
	})

	t.Run("reject sets only the rejection reason", func(t *testing.T) {
		request := admittedRequest(t)
		if err := request.Reject(RejectedNoCandidate, finalAt); err != nil {
			t.Fatalf("Reject() error = %v", err)
		}
		if request.Status != StatusRejected || request.RejectionReason != RejectedNoCandidate {
			t.Errorf("status = %q, reason = %q, want rejected / %q", request.Status, request.RejectionReason, RejectedNoCandidate)
		}
		if request.FailureReason != "" || request.CommittedAttemptID != "" {
			t.Errorf("FailureReason = %q, CommittedAttemptID = %q, want both unset", request.FailureReason, request.CommittedAttemptID)
		}
		if request.FinishedAt != finalAt {
			t.Errorf("FinishedAt = %v, want %v", request.FinishedAt, finalAt)
		}
	})
}

// transitioners names the four transitions uniformly so the once-only table
// below can drive each of them against each terminal state.
func transitioners() map[string]func(*Request, time.Time) error {
	return map[string]func(*Request, time.Time) error{
		"succeed":             func(r *Request, at time.Time) error { return r.Succeed("att-0002", at) },
		"failAfterCommitment": func(r *Request, at time.Time) error { return r.FailAfterCommitment("att-0002", at) },
		"failAbandoned":       func(r *Request, at time.Time) error { return r.FailAbandoned(at) },
		"reject":              func(r *Request, at time.Time) error { return r.Reject(RejectedNoCandidate, at) },
	}
}

// finaliseEachWay builds one finalised request per terminal shape — the four
// decisions the vocabulary allows — so the once-only table can prove that no
// transition can move any of them. Terminal statuses number three; terminal
// shapes number four, because failed carries two reasons that a later
// transition must not overwrite into one another.
func finaliseEachWay(t *testing.T) map[string]*Request {
	t.Helper()
	ways := map[string]func(*Request) error{
		"succeeded":               func(r *Request) error { return r.Succeed("att-0001", finalAt) },
		"failed after commitment": func(r *Request) error { return r.FailAfterCommitment("att-0001", finalAt) },
		"failed abandoned":        func(r *Request) error { return r.FailAbandoned(finalAt) },
		"rejected":                func(r *Request) error { return r.Reject(RejectedInvalidRequest, finalAt) },
	}
	finalised := make(map[string]*Request, len(ways))
	for name, way := range ways {
		request := admittedRequest(t)
		if err := way(request); err != nil {
			t.Fatalf("finalising a %s request: %v", name, err)
		}
		finalised[name] = request
	}
	return finalised
}

// TestEveryTransitionOnAFinalisedRequestIsErrFinalised pins finalise-exactly-
// once over the full 4x4 matrix: each of the four transitions attempted on
// each of the four terminal shapes returns exactly ErrFinalised and leaves
// the standing decision byte-for-byte intact. A second finalisation is not a
// retry but a decision to re-read — someone else's decision already stands —
// and any silent overwrite here would rewrite history the fact feed has
// already stated.
func TestEveryTransitionOnAFinalisedRequestIsErrFinalised(t *testing.T) {
	for shape, request := range finaliseEachWay(t) {
		for name, transition := range transitioners() {
			before := *request
			err := transition(request, finalAt.Add(time.Minute))
			if !errors.Is(err, ErrFinalised) {
				t.Errorf("%s request: %s returned %v, want ErrFinalised", shape, name, err)
			}
			if *request != before {
				t.Errorf("%s request: %s mutated it to %+v, want %+v — a refused transition touches nothing", shape, name, *request, before)
			}
		}
	}
}

// TestFinalisedWithNoAttemptAnswersErrFinalisedBeforeErrNilAttempt pins the
// guard ORDER inside checkFinalisable: an abandoned request carries no
// committed attempt, so Succeed("") on it could plausibly be read as a
// missing-attempt refusal — but the finalisation guard runs first, and the
// caller must be told the decision already stands, not sent chasing an
// attempt it never owed. The same order holds on the rejection shape, which
// also names no attempt.
func TestFinalisedWithNoAttemptAnswersErrFinalisedBeforeErrNilAttempt(t *testing.T) {
	for shape, request := range finaliseEachWay(t) {
		err := request.Succeed("", finalAt.Add(time.Minute))
		if !errors.Is(err, ErrFinalised) {
			t.Errorf("%s request: Succeed(\"\") returned %v, want ErrFinalised — finalised outranks nil-attempt in the guard order", shape, err)
		}
	}
}

// TestSucceedAndFailAfterCommitmentRefuseAnUnnamedAttempt pins the attempt
// pairing on the two finalisations that owe one: an executing request asked
// to succeed or stream-fail without an attempt is refused with ErrNilAttempt,
// and the request stays executing — the refusal must not spend the request's
// only finalisation on a decision the vocabulary does not allow.
func TestSucceedAndFailAfterCommitmentRefuseAnUnnamedAttempt(t *testing.T) {
	request := admittedRequest(t)
	if err := request.Succeed("", finalAt); !errors.Is(err, ErrNilAttempt) {
		t.Errorf("Succeed(\"\") = %v, want ErrNilAttempt", err)
	}
	if err := request.FailAfterCommitment("", finalAt); !errors.Is(err, ErrNilAttempt) {
		t.Errorf("FailAfterCommitment(\"\") = %v, want ErrNilAttempt", err)
	}
	if request.Status != StatusExecuting || !request.FinishedAt.IsZero() {
		t.Errorf("request = %+v, want it still executing with a zero FinishedAt — a refused decision finalises nothing", request)
	}
}

// TestRejectWithAnUnknownReasonLeavesTheRequestExecuting pins the rejection
// transition's own guard order and its refusal's side effects: the unknown
// reason is refused with the value in the message, and because the status
// guard runs first the request is untouched — still executing, still zero
// finished, still finalisable by a later, correctly spelled decision.
func TestRejectWithAnUnknownReasonLeavesTheRequestExecuting(t *testing.T) {
	request := admittedRequest(t)
	err := request.Reject("account_locked", finalAt)
	if err == nil {
		t.Fatal("Reject(\"account_locked\") succeeded, want the unknown reason refused")
	}
	if !strings.Contains(err.Error(), "account_locked") {
		t.Errorf("Reject() error = %q, want it to carry the offending value", err)
	}
	if request.Status != StatusExecuting || !request.FinishedAt.IsZero() || request.RejectionReason != "" {
		t.Errorf("request = %+v, want it untouched and still executing", request)
	}
	if request.Terminal() {
		t.Error("Terminal() = true after a refused rejection, want false")
	}

	// The status guard outranks the vocabulary check: on a finalised request
	// even an unknown reason answers ErrFinalised, because the request the
	// reason would land on no longer exists to receive it.
	finalised := admittedRequest(t)
	if err := finalised.Reject(RejectedNoCandidate, finalAt); err != nil {
		t.Fatalf("finalising the fixture: %v", err)
	}
	if err := finalised.Reject("account_locked", finalAt.Add(time.Minute)); !errors.Is(err, ErrFinalised) {
		t.Errorf("Reject(unknown) on a finalised request = %v, want ErrFinalised", err)
	}
}
