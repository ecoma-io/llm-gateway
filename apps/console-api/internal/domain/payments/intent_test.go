package payments

import (
	"errors"
	"fmt"
	"testing"
)

// The vocabulary: the status spellings the contract owns, and the sentinels
// every caller branches on.

// TestTheStatusSpellingsAreTheContracts pins the wire values themselves. The
// names are api/openapi/console.yaml's PaymentIntentState, and a Go copy that
// drifted from the document would render one state as another to a customer
// reading their own payments — the reason this file exists rather than the
// values being taken on trust.
func TestTheStatusSpellingsAreTheContracts(t *testing.T) {
	cases := []struct {
		status Status
		want   string
	}{
		{StatusCreated, "created"},
		{StatusAwaitingTransfer, "awaiting_transfer"},
		{StatusRequiresAction, "requires_action"},
		{StatusSucceeded, "succeeded"},
		{StatusFailed, "failed"},
		{StatusCancelled, "cancelled"},
		{StatusExpired, "expired"},
		{StatusPartiallyRefunded, "partially_refunded"},
		{StatusRefunded, "refunded"},
		{StatusQuarantined, "quarantined"},
	}
	seen := map[Status]bool{}
	for _, tc := range cases {
		if string(tc.status) != tc.want {
			t.Errorf("status %q is spelled %q, want %q", tc.want, string(tc.status), tc.want)
		}
		if seen[tc.status] {
			t.Errorf("status %q is declared twice", tc.status)
		}
		seen[tc.status] = true
		if !knownStatus(tc.status) {
			t.Errorf("the contract's status %q is not one knownStatus names", tc.status)
		}
	}
}

// TestTheDispositionsAreThreeDifferentWords checks the answer a webhook returns
// is a vocabulary rather than a boolean: "already applied" and "applied" are
// different facts, and a caller that could not tell them apart would either
// retry forever or apply twice.
func TestTheDispositionsAreThreeDifferentWords(t *testing.T) {
	cases := []struct {
		disposition EventDisposition
		want        string
	}{
		{DispositionApplied, "applied"},
		{DispositionDuplicate, "duplicate"},
		{DispositionQuarantined, "quarantined"},
	}
	seen := map[EventDisposition]bool{}
	for _, tc := range cases {
		if string(tc.disposition) != tc.want {
			t.Errorf("disposition %q is spelled %q, want %q", tc.want, string(tc.disposition), tc.want)
		}
		if seen[tc.disposition] {
			t.Errorf("disposition %q is declared twice", tc.disposition)
		}
		seen[tc.disposition] = true
	}
}

// TestTheSentinelsAreDistinctAndMatchable checks the property every caller
// leans on: a refusal is a value errors.Is can name, and no two refusals that
// want different handling share one.
func TestTheSentinelsAreDistinctAndMatchable(t *testing.T) {
	sentinels := map[string]error{
		"ErrInvalidReference":  ErrInvalidReference,
		"ErrInvalidTransition": ErrInvalidTransition,
		"ErrDuplicateEvent":    ErrDuplicateEvent,
		"ErrUnknownPayment":    ErrUnknownPayment,
		"ErrDuplicatePayment":  ErrDuplicatePayment,
	}
	names := make([]string, 0, len(sentinels))
	for name := range sentinels {
		names = append(names, name)
	}
	for name, sentinel := range sentinels {
		if sentinel == nil {
			t.Fatalf("%s is nil", name)
		}
		if sentinel.Error() == "" {
			t.Errorf("%s has an empty message", name)
		}
		wrapped := fmt.Errorf("payments: some caller's context: %w", sentinel)
		if !errors.Is(wrapped, sentinel) {
			t.Errorf("%s cannot be matched through a wrap: a caller cannot branch on it", name)
		}
		for _, other := range names {
			if other == name {
				continue
			}
			if sentinels[other] == sentinel {
				t.Errorf("%s and %s are the same value", name, other)
			}
			if errors.Is(sentinel, sentinels[other]) {
				t.Errorf("%s matches %s: two refusals that want different handling share a sentinel", name, other)
			}
		}
	}
}

// TestTheQuarantineReasonsAreAClosedVocabulary pins the strings an operator
// reads and the column's CHECK mirrors. Every reason here is a value this build
// can write, and a value outside the set cannot be inserted at all.
func TestTheQuarantineReasonsAreAClosedVocabulary(t *testing.T) {
	cases := []struct {
		reason QuarantineReason
		want   string
	}{
		{ReasonUnverifiable, "unverifiable"},
		{ReasonUnknownKind, "unknown_kind"},
		{ReasonUnknownPayment, "unknown_payment"},
		{ReasonAmountMismatch, "amount_mismatch"},
		{ReasonCurrencyMismatch, "currency_mismatch"},
		{ReasonStale, "stale"},
		{ReasonStateConflict, "state_conflict"},
		{ReasonAccountClosed, "account_closed"},
		{ReasonBucketClosed, "bucket_closed"},
		{ReasonRefundAheadOfCapture, "refund_ahead_of_capture"},
		{ReasonRefundCeiling, "refund_ceiling"},
	}
	seen := map[QuarantineReason]bool{}
	for _, tc := range cases {
		if string(tc.reason) != tc.want {
			t.Errorf("reason %q is spelled %q, want %q", tc.want, string(tc.reason), tc.want)
		}
		if seen[tc.reason] {
			t.Errorf("reason %q is declared twice", tc.reason)
		}
		seen[tc.reason] = true
	}
}

// TestTheTwoKindsThatNeverComeFromAWebhookAreStillKnown documents the pair of
// reasons the transport deliberately refuses without recording. They are in the
// vocabulary because the one trusted caller that can produce them — a
// reconciliation replaying stored bodies — needs a name for what it found, and
// a build that had dropped them would have to invent one.
func TestTheTwoKindsThatNeverComeFromAWebhookAreStillKnown(t *testing.T) {
	for _, reason := range []QuarantineReason{ReasonUnverifiable, ReasonStale} {
		if !knownReason(reason) {
			t.Errorf("knownReason(%q) = false, want true", reason)
		}
	}
}
