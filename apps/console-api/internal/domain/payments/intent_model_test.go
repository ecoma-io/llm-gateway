package payments

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The model itself: the digest a command key is built from, the reason
// vocabulary the quarantine column holds, and the one operation this package
// offers on a payment it does not own — the compare-and-swap.

// TestDigestReferenceIsFixedLengthAndSeparatesNeighbouringInputs pins the two
// properties the command key rests on. A truncation would give two references
// sharing a prefix one key and one credit where two were owed; a length that
// followed the input would put a provider's unbounded identifier against B6's
// 256-character ceiling and refuse a real customer's webhook forever.
func TestDigestReferenceIsFixedLengthAndSeparatesNeighbouringInputs(t *testing.T) {
	wantLength := sha256.Size * 2 // hex-encoded
	cases := []string{
		"",
		"r",
		"ref_1",
		strings.Repeat("r", 255),
		strings.Repeat("r", 5000),
		"ref_é",
	}
	for _, reference := range cases {
		digest := digestReference(reference)
		if len(digest) != wantLength {
			t.Errorf("digestReference(%d bytes) is %d characters, want %d", len(reference), len(digest), wantLength)
		}
		if utf8.RuneCountInString(digest) != len(digest) {
			t.Errorf("digest %q is not ASCII: B6's byte bound and the schema's character bound would disagree", digest)
		}
		// The algorithm is pinned rather than the outputs: a change of digest
		// changes which leg a redelivery converges on, which is a second charge.
		sum := sha256.Sum256([]byte(reference))
		if want := hex.EncodeToString(sum[:]); digest != want {
			t.Errorf("digestReference(%q) = %q, want the SHA-256 hex %q", reference, digest, want)
		}
		if digestReference(reference) != digest {
			t.Errorf("digestReference(%q) is not deterministic", reference)
		}
	}

	neighbours := [][2]string{
		{"ref_1", "ref_2"},
		{"a", "b"},
		{strings.Repeat("r", 300) + "a", strings.Repeat("r", 300) + "b"},
	}
	for _, pair := range neighbours {
		if digestReference(pair[0]) == digestReference(pair[1]) {
			t.Errorf("references %q and %q share a digest", pair[0], pair[1])
		}
	}
}

// TestKnownReasonNamesEveryReasonAndNothingElse checks the guard the
// quarantine column's closed set rests on: a reason this build cannot render is
// not recorded at all.
func TestKnownReasonNamesEveryReasonAndNothingElse(t *testing.T) {
	known := []QuarantineReason{
		ReasonUnverifiable, ReasonUnknownKind, ReasonUnknownPayment,
		ReasonAmountMismatch, ReasonCurrencyMismatch, ReasonStale,
		ReasonStateConflict, ReasonAccountClosed, ReasonBucketClosed,
		ReasonRefundAheadOfCapture, ReasonRefundCeiling,
	}
	for _, reason := range known {
		if !knownReason(reason) {
			t.Errorf("knownReason(%q) = false, want true", reason)
		}
	}
	unknown := []QuarantineReason{
		"",
		"stale ", " Stale", "Stale", "STALE",
		"unknown_payment2", "amount-mismatch", "refund_ahead", "succeeded",
	}
	for _, reason := range unknown {
		if knownReason(reason) {
			t.Errorf("knownReason(%q) = true, want false", reason)
		}
	}
}

// fakeIntents is the port a Claim runs against. It is a fake of the STORE
// rather than of a database: the compare-and-swap's verdict and its failure are
// the two things a test needs to set, and everything else the port offers is
// unreachable from Claim.
type fakeIntents struct {
	applied  bool
	moveErr  error
	moves    int
	lastFrom []Status
	lastTo   Status
}

func (f *fakeIntents) Create(IntentContext, Intent) error { return nil }

func (f *fakeIntents) ByID(IntentContext, IntentID) (Intent, error) { return Intent{}, nil }

func (f *fakeIntents) ByProviderReference(IntentContext, string, string) (Intent, error) {
	return Intent{}, nil
}

func (f *fakeIntents) Move(_ IntentContext, _ IntentID, from []Status, to Status, _ time.Time) (bool, error) {
	f.moves++
	f.lastFrom = from
	f.lastTo = to
	return f.applied, f.moveErr
}

func (f *fakeIntents) RecordEvent(IntentContext, ProviderEventRecord) error { return nil }

func (f *fakeIntents) RecentEvents(IntentContext, IntentID, int) ([]ProviderEventRecord, error) {
	return nil, nil
}

// errStoreMiss is an infrastructure failure a store can report, as distinct from
// the compare-and-swap losing.
var errStoreMiss = errors.New("fakes: the store could not answer")

// TestClaimRefusesAStatusOrASourceItHasNoRuleFor checks the two guards that run
// before any statement: a status this build has no rule for, and a claim that
// names no predecessor. Neither reaches the store, because neither is a
// question the store could answer.
func TestClaimRefusesAStatusOrASourceItHasNoRuleFor(t *testing.T) {
	cases := []struct {
		name string
		from []Status
		to   Status
	}{
		{"a status this build has no rule for", []Status{StatusCreated}, Status("captured")},
		{"an empty status", []Status{StatusCreated}, Status("")},
		{"no predecessor named", nil, StatusSucceeded},
		{"an empty predecessor list", []Status{}, StatusSucceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeIntents{applied: true, lastTo: Status("untouched")}
			err := Claim(t.Context(), store, IntentID(testPaymentID), tc.from, tc.to, testNow)
			if !errors.Is(err, ErrInvalidReference) {
				t.Fatalf("Claim(%s) = %v, want ErrInvalidReference", tc.name, err)
			}
			if store.moves != 0 {
				t.Errorf("Claim(%s) asked the store to move the row", tc.name)
			}
		})
	}
}

// TestClaimHandsTheStoreTheWholeClaim checks the pass-through: the states it
// may move from are the caller's, not a default, because a claim that widened
// its own predecessor list would be a claim that moves a payment somebody else
// already finished.
func TestClaimHandsTheStoreTheWholeClaim(t *testing.T) {
	store := &fakeIntents{applied: true}
	from := []Status{StatusAwaitingTransfer, StatusRequiresAction}
	err := Claim(t.Context(), store, IntentID(testPaymentID), from, StatusSucceeded, testNow)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if store.moves != 1 {
		t.Fatalf("the store was asked %d times, want once", store.moves)
	}
	if len(store.lastFrom) != len(from) || store.lastFrom[0] != from[0] || store.lastFrom[1] != from[1] {
		t.Errorf("the store saw %v as the predecessors, want %v", store.lastFrom, from)
	}
	if store.lastTo != StatusSucceeded {
		t.Errorf("the store was told to move to %q, want %q", store.lastTo, StatusSucceeded)
	}
}

// TestClaimReportsALostCompareAndSwapAsItsOwnAnswer checks the distinction the
// whole function exists for: "somebody else moved this row" is a domain answer
// a caller can act on (re-read and decide), while "this call failed" is an
// error the caller must not mistake for it.
func TestClaimReportsALostCompareAndSwapAsItsOwnAnswer(t *testing.T) {
	lost := &fakeIntents{applied: false}
	err := Claim(t.Context(), lost, IntentID(testPaymentID), []Status{StatusAwaitingTransfer}, StatusSucceeded, testNow)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a lost compare-and-swap = %v, want ErrInvalidTransition", err)
	}
	if lost.moves != 1 {
		t.Errorf("a lost compare-and-swap did not reach the store")
	}

	// A store failure is NOT reported as a lost race: the caller must be able
	// to tell the two apart, and does.
	failed := &fakeIntents{applied: true, moveErr: errStoreMiss}
	err = Claim(t.Context(), failed, IntentID(testPaymentID), []Status{StatusAwaitingTransfer}, StatusSucceeded, testNow)
	if !errors.Is(err, errStoreMiss) {
		t.Fatalf("a store failure = %v, want the store's own error", err)
	}
	if errors.Is(err, ErrInvalidTransition) {
		t.Errorf("a store failure was reported as a lost compare-and-swap: %v", err)
	}
}

// TestClaimReturnsTheStoresMissUnchanged pins the layering fact Claim's own
// comment states: the store's error is handed back verbatim and no branch here
// translates it. A store answering a miss with the persistence port's
// ErrNotFound therefore hands the caller a persistence sentinel from a domain
// call, and that is deliberate rather than overlooked — this function cannot
// tell a missing payment from a store that could not answer, so a refusal it
// invented would hide the second behind the first. The domain's own word for
// the condition is minted one layer up, where the caller knows the miss came
// from resolving a delivery rather than from reading an id.
func TestClaimReturnsTheStoresMissUnchanged(t *testing.T) {
	missing := &fakeIntents{applied: false, moveErr: errStoreMiss}
	err := Claim(t.Context(), missing, IntentID(testPaymentID), []Status{StatusAwaitingTransfer}, StatusSucceeded, testNow)
	if !errors.Is(err, errStoreMiss) {
		t.Fatalf("Claim = %v, want the store's error", err)
	}
	if errors.Is(err, ErrUnknownPayment) {
		t.Fatalf("Claim translated the store's miss into %v; that word is minted by the caller that resolved a delivery, not here", ErrUnknownPayment)
	}
	if err.Error() != errStoreMiss.Error() {
		t.Errorf("Claim rewrote the store's error as %v, want it unchanged", err)
	}
}
