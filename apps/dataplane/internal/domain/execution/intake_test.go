package execution

import (
	"errors"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// Every test here pins the replay record's contract: the identity fields
// written once at admission, and the one mutation — the terminal pointer —
// written once, only with a status and reason that agree. A pointer whose
// halves disagree would answer every future replay with a self-contradicting
// sentence, so the pairing switch is pinned value by value, including the
// default branch that keeps every status outside the three-value vocabulary
// (notably executing, which by definition is not a final) out of the row.

var intakeCreatedAt = time.Date(2026, 9, 25, 11, 59, 59, 0, time.UTC)

// openedIntake builds the fixture every Finalise test starts from: the
// record admission opens, pointing at its request, undecided.
func openedIntake(t *testing.T) *Intake {
	t.Helper()
	intake, err := NewIntake("acc-0001", "idem-key-0001", "sha256:digest-0001", identity.RequestID("req-0001"), intakeCreatedAt)
	if err != nil {
		t.Fatalf("NewIntake() error = %v", err)
	}
	return &intake
}

// TestNewIntakeOpensAnUndecidedRecord pins the admission write: the four
// identity fields carried whole, the final pointer still nil, and Decided()
// false — a replay arriving now must be refused with retry-later semantics,
// because the record cannot answer for a request that has not ended.
func TestNewIntakeOpensAnUndecidedRecord(t *testing.T) {
	intake, err := NewIntake("acc-0001", "idem-key-0001", "sha256:digest-0001", identity.RequestID("req-0001"), intakeCreatedAt)
	if err != nil {
		t.Fatalf("NewIntake() error = %v", err)
	}

	if intake.AccountID != "acc-0001" || intake.IdempotencyKey != "idem-key-0001" || intake.RequestDigest != "sha256:digest-0001" || intake.RequestID != identity.RequestID("req-0001") {
		t.Errorf("intake = %+v, want the identity fields carried whole", intake)
	}
	if intake.CreatedAt != intakeCreatedAt {
		t.Errorf("CreatedAt = %v, want %v", intake.CreatedAt, intakeCreatedAt)
	}
	if intake.FinalStatus != nil {
		t.Errorf("FinalStatus = %v, want nil — the record opens before the request has ended", *intake.FinalStatus)
	}
	if intake.Decided() {
		t.Error("Decided() = true on a fresh record, want false")
	}
}

// TestNewIntakeRefusesAnIncompleteRecord pins each identity refusal: a
// replay record that cannot name its account, its key, the digest it decided
// on, or the request it was decided with cannot answer a replay from one row,
// which is the only thing it exists for.
func TestNewIntakeRefusesAnIncompleteRecord(t *testing.T) {
	tests := []struct {
		name      string
		account   string
		key       string
		digest    string
		requestID identity.RequestID
	}{
		{name: "rejects an empty account", key: "idem-key-0001", digest: "sha256:digest-0001", requestID: "req-0001"},
		{name: "rejects an empty idempotency key", account: "acc-0001", digest: "sha256:digest-0001", requestID: "req-0001"},
		{name: "rejects an empty digest", account: "acc-0001", key: "idem-key-0001", requestID: "req-0001"},
		{name: "rejects an empty request id", account: "acc-0001", key: "idem-key-0001", digest: "sha256:digest-0001"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			intake, err := NewIntake(tt.account, tt.key, tt.digest, tt.requestID, intakeCreatedAt)
			if err == nil {
				t.Fatalf("NewIntake() = %+v, want an error", intake)
			}
		})
	}
}

// TestFinaliseWritesAnAgreeingPointer walks every legal status/reason pair:
// succeeded with neither reason, failed with each of the two failure
// reasons, rejected with each of the eight rejection reasons. Each must set
// the pointer and exactly its own reason — the other reason stays empty, or
// the record would answer replays with a status and a reason that tell two
// different stories.
func TestFinaliseWritesAnAgreeingPointer(t *testing.T) {
	// decision is one (status, reasons) triple as Finalise takes it.
	type decision struct {
		status    FinalStatus
		rejection RejectionReason
		failure   FailureReason
	}
	// decisions enumerates the agreeing pairs; the reason loops below expand
	// it to the full legal set rather than sampling it.
	decisions := []decision{{status: FinalSucceeded}}
	for _, failure := range []FailureReason{FailedStreamAfterCommitment, FailedGatewayAbandoned} {
		decisions = append(decisions, decision{status: FinalFailed, failure: failure})
	}
	for _, rejection := range []RejectionReason{
		RejectedAccountSuspended, RejectedAccountClosed, RejectedUnknownAlias,
		RejectedInvalidRequest, RejectedInsufficientEntitlement, RejectedNoAccess,
		RejectedNoCandidate, RejectedNoCandidateSucceeded,
	} {
		decisions = append(decisions, decision{status: FinalRejected, rejection: rejection})
	}

	for _, decision := range decisions {
		t.Run(string(decision.status)+"/"+string(decision.rejection)+"/"+string(decision.failure), func(t *testing.T) {
			intake := openedIntake(t)
			if err := intake.Finalise(decision.status, decision.rejection, decision.failure); err != nil {
				t.Fatalf("Finalise(%q, %q, %q) error = %v", decision.status, decision.rejection, decision.failure, err)
			}
			if intake.FinalStatus == nil || *intake.FinalStatus != decision.status {
				t.Errorf("FinalStatus = %v, want %q", intake.FinalStatus, decision.status)
			}
			if intake.FinalRejectionReason != decision.rejection {
				t.Errorf("FinalRejectionReason = %q, want %q", intake.FinalRejectionReason, decision.rejection)
			}
			if intake.FinalFailureReason != decision.failure {
				t.Errorf("FinalFailureReason = %q, want %q", intake.FinalFailureReason, decision.failure)
			}
			if !intake.Decided() {
				t.Error("Decided() = false after a written pointer, want true")
			}
		})
	}
}

// TestFinaliseRefusesADisagreeingPointer walks every illegal combination the
// pairing switch forbids, including the default branch: a succeeded pointer
// with either reason, a failed pointer carrying a rejection or missing or
// inventing its failure reason, a rejected pointer without its reason,
// with a failure, or with an unknown reason, and any status outside the
// three-value vocabulary — the empty string and executing itself, which the
// pointer exists precisely to not be. A refusal must leave the record
// undecided: a refused decision writes nothing.
func TestFinaliseRefusesADisagreeingPointer(t *testing.T) {
	tests := []struct {
		name      string
		status    FinalStatus
		rejection RejectionReason
		failure   FailureReason
	}{
		{name: "succeeded with a rejection reason", status: FinalSucceeded, rejection: RejectedNoCandidate},
		{name: "succeeded with a failure reason", status: FinalSucceeded, failure: FailedStreamAfterCommitment},
		{name: "succeeded with both reasons", status: FinalSucceeded, rejection: RejectedNoCandidate, failure: FailedGatewayAbandoned},
		{name: "failed with a rejection reason", status: FinalFailed, rejection: RejectedNoCandidate, failure: FailedStreamAfterCommitment},
		{name: "failed without a failure reason", status: FinalFailed},
		{name: "failed with an unknown failure reason", status: FinalFailed, failure: "crashed"},
		{name: "rejected without a rejection reason", status: FinalRejected},
		{name: "rejected with an unknown rejection reason", status: FinalRejected, rejection: "account_locked"},
		{name: "rejected with a failure reason", status: FinalRejected, rejection: RejectedNoCandidate, failure: FailedStreamAfterCommitment},
		{name: "an empty status", status: ""},
		{name: "executing, which is not a final", status: "executing"},
		{name: "a status outside the vocabulary", status: "queued"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			intake := openedIntake(t)
			err := intake.Finalise(tt.status, tt.rejection, tt.failure)
			if !errors.Is(err, ErrIntakePairing) {
				t.Fatalf("Finalise(%q, %q, %q) = %v, want ErrIntakePairing", tt.status, tt.rejection, tt.failure, err)
			}
			if intake.FinalStatus != nil || intake.FinalRejectionReason != "" || intake.FinalFailureReason != "" {
				t.Errorf("intake = %+v, want it untouched — a refused decision writes nothing", intake)
			}
			if intake.Decided() {
				t.Error("Decided() = true after a refused finalisation, want false")
			}
		})
	}
}

// TestFinaliseHappensOnce pins the single write of the terminal pointer: a
// second finalisation — of any agreeing shape, even the same one — answers
// ErrIntakeFinalised, and the first decision stands exactly as written. The
// pointer is the one mutation the record has; a silent second write would
// let a late closer rewrite what every replay after it is told.
func TestFinaliseHappensOnce(t *testing.T) {
	t.Run("the second finalisation loses to the succeeded one", func(t *testing.T) {
		intake := openedIntake(t)
		if err := intake.Finalise(FinalSucceeded, "", ""); err != nil {
			t.Fatalf("first Finalise() error = %v", err)
		}
		for _, second := range []struct {
			status    FinalStatus
			rejection RejectionReason
			failure   FailureReason
		}{
			{FinalSucceeded, "", ""},
			{FinalFailed, "", FailedGatewayAbandoned},
			{FinalRejected, RejectedNoCandidate, ""},
		} {
			if err := intake.Finalise(second.status, second.rejection, second.failure); !errors.Is(err, ErrIntakeFinalised) {
				t.Errorf("second Finalise(%q, %q, %q) = %v, want ErrIntakeFinalised", second.status, second.rejection, second.failure, err)
			}
		}
		if intake.FinalStatus == nil || *intake.FinalStatus != FinalSucceeded || intake.FinalRejectionReason != "" || intake.FinalFailureReason != "" {
			t.Errorf("intake = %+v, want the first decision standing untouched", intake)
		}
	})

	t.Run("the second finalisation loses to the rejected one", func(t *testing.T) {
		intake := openedIntake(t)
		if err := intake.Finalise(FinalRejected, RejectedNoAccess, ""); err != nil {
			t.Fatalf("first Finalise() error = %v", err)
		}
		if err := intake.Finalise(FinalSucceeded, "", ""); !errors.Is(err, ErrIntakeFinalised) {
			t.Errorf("second Finalise() = %v, want ErrIntakeFinalised", err)
		}
		if intake.FinalStatus == nil || *intake.FinalStatus != FinalRejected || intake.FinalRejectionReason != RejectedNoAccess || intake.FinalFailureReason != "" {
			t.Errorf("intake = %+v, want the first decision standing untouched", intake)
		}
	})
}
