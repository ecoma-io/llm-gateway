package payments

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The state machine, the command keys and the refund algebra, pinned in the
// domain's own words.
//
// Everything here is a rule about what a payment IS rather than about how one
// is stored, which is why the tests can state the whole of it: the transition
// matrix is enumerated rather than sampled (a sampled matrix proves the edges
// a reader remembered and says nothing about the ones they did not), the
// command-key properties are stated as properties rather than as golden
// strings, and the refund ceiling is checked at the boundary, one unit past it,
// and at the int64 extremity the naive implementation would wrap.

// testNow is the clock every test in this package reads. It is a fixed instant
// so a failure names the same instant a rerun does.
var testNow = time.Date(2026, time.September, 25, 10, 0, 0, 0, time.UTC)

// allStatuses is the whole vocabulary this build has a word for, in the order
// intent.go declares it.
var allStatuses = []Status{
	StatusCreated,
	StatusAwaitingTransfer,
	StatusRequiresAction,
	StatusSucceeded,
	StatusFailed,
	StatusCancelled,
	StatusExpired,
	StatusPartiallyRefunded,
	StatusRefunded,
	StatusQuarantined,
}

// declaredEdges is the transition matrix as state.go documents it, edge by
// edge. It is written out again here rather than read from legalEdges so that
// the test states the intended contract instead of restating the code: a test
// that compared the table to itself would pass after any edit at all.
var declaredEdges = map[Status][]Status{
	StatusCreated: {
		StatusAwaitingTransfer,
		StatusFailed,
		StatusCancelled,
	},
	StatusAwaitingTransfer: {
		StatusRequiresAction,
		StatusSucceeded,
		StatusFailed,
		StatusCancelled,
		StatusExpired,
	},
	StatusRequiresAction: {
		StatusAwaitingTransfer,
		StatusSucceeded,
		StatusFailed,
		StatusCancelled,
	},
	// The late-payment edges. Expiry and cancellation are local decisions; the
	// provider is the only party that says whether a customer paid.
	StatusExpired:   {StatusSucceeded},
	StatusCancelled: {StatusSucceeded},
	StatusSucceeded: {StatusPartiallyRefunded, StatusRefunded},
	StatusPartiallyRefunded: {
		StatusRefunded,
		StatusPartiallyRefunded,
	},
	StatusRefunded: {StatusRefunded},
	// failed and quarantined have no outgoing edge at all: a failed payment has
	// no money to reverse, and quarantined is a delivery's disposition rather
	// than a state an intent stands in.
	StatusFailed:      nil,
	StatusQuarantined: nil,
}

// hasEdge reports whether edges contains to.
func hasEdge(edges []Status, to Status) bool {
	for _, candidate := range edges {
		if candidate == to {
			return true
		}
	}
	return false
}

// TestTheTransitionMatrixIsExactlyTheDocumentedOne enumerates every pair of the
// status vocabulary — not a sample of them — and checks the code's table, the
// documented table and canMove against each other. Two readers of one rule is
// the defect this package's own header warns about; this test is the third.
func TestTheTransitionMatrixIsExactlyTheDocumentedOne(t *testing.T) {
	for from, edges := range legalEdges {
		for _, to := range edges {
			if !hasEdge(declaredEdges[from], to) {
				t.Errorf("legalEdges declares %s → %s, which the matrix does not name", from, to)
			}
		}
	}
	for from, edges := range declaredEdges {
		for _, to := range edges {
			if !hasEdge(legalEdges[from], to) {
				t.Errorf("the matrix names %s → %s, which legalEdges does not declare", from, to)
			}
		}
	}
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			want := hasEdge(declaredEdges[from], to)
			if got := canMove(from, to); got != want {
				t.Errorf("canMove(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
	for _, to := range allStatuses {
		if canMove(Status("captured"), to) {
			// A status this build has never heard of is not a source with no
			// edges: it is not a source at all.
			t.Errorf("canMove(%q, %q) = true, want false", "captured", to)
		}
	}
}

// TestTheAwaitingTransferEdgesAreTheOnesABankTransferRestsOn names the four
// edges this instrument turns on, each for a reason both of its states have to
// agree about.
//
// `created` to `awaiting_transfer` is how a payment acquires a destination: an
// intent is durable before the provider is called, and the account the provider
// issued is what moves it out of `created`. `awaiting_transfer` to `succeeded`
// is the credit, reachable only from the state that was actually waiting for
// money. `awaiting_transfer` to `expired` is a local timer running out, which is
// a statement about this platform's patience and never about the transfer. And
// `requires_action` to `awaiting_transfer` runs BACKWARD, because the customer
// doing what the provider asked puts the payment back in their hands, waiting on
// their transfer again, rather than forward into a state of its own.
func TestTheAwaitingTransferEdgesAreTheOnesABankTransferRestsOn(t *testing.T) {
	cases := []struct {
		from, to Status
		why      string
	}{
		{StatusCreated, StatusAwaitingTransfer, "a payment is waiting once the provider has issued it a destination"},
		{StatusAwaitingTransfer, StatusSucceeded, "the credit is reached from the state that was waiting for the money"},
		{StatusAwaitingTransfer, StatusExpired, "a local deadline may run out on a customer who sent nothing"},
		{StatusRequiresAction, StatusAwaitingTransfer, "the customer answering the provider puts the payment back in their hands"},
	}
	for _, tc := range cases {
		if !canMove(tc.from, tc.to) {
			t.Errorf("canMove(%q, %q) = false, want true: %s", tc.from, tc.to, tc.why)
		}
		if !hasEdge(declaredEdges[tc.from], tc.to) {
			t.Errorf("the documented matrix does not name %q to %q, which legalEdges declares", tc.from, tc.to)
		}
	}

	// The backward move is the one that goes the wrong way through the machine,
	// so the direction is asserted rather than assumed: a waiting payment has no
	// edge back to `created`, and a captured one has no edge back to waiting.
	// Returning to a state the payment already stood in is what `requires_action`
	// to `awaiting_transfer` is for, and it is the only edge here that does it.
	for _, pair := range [][2]Status{
		{StatusAwaitingTransfer, StatusCreated},
		{StatusSucceeded, StatusAwaitingTransfer},
	} {
		if canMove(pair[0], pair[1]) {
			t.Errorf("canMove(%q, %q) = true, want false: the machine does not run that way", pair[0], pair[1])
		}
	}
}

// TestExpiredToSucceededIsALegalLatePayment pins the edge a reader should
// question first. Expiry is this platform's patience running out, and the
// provider is the only party entitled to say whether the customer paid; a
// payment that succeeded after a local timer fired has PAID, and refusing to
// fund it is the failure this edge exists to prevent.
func TestExpiredToSucceededIsALegalLatePayment(t *testing.T) {
	for _, from := range []Status{StatusExpired, StatusCancelled} {
		if !canMove(from, StatusSucceeded) {
			t.Errorf("canMove(%q, %q) = false, want true: a late payment is still a payment", from, StatusSucceeded)
		}
	}
	// The states that HAVE had the final word may not be succeeded again: that
	// is not a late delivery, it is the provider contradicting itself.
	for _, from := range []Status{StatusSucceeded, StatusPartiallyRefunded, StatusRefunded, StatusFailed} {
		if canMove(from, StatusSucceeded) {
			t.Errorf("canMove(%q, %q) = true, want false", from, StatusSucceeded)
		}
	}
}

// TestQuarantinedIsNeverATransitionTarget is the second counter-intuitive rule.
// quarantined is spelled as a status so the console renders one vocabulary, but
// it is a DELIVERY's disposition: a payment that was succeeded and then met an
// unreadable event has not become quarantined, and moving it there would lose
// the fact that the money is in the bucket.
func TestQuarantinedIsNeverATransitionTarget(t *testing.T) {
	if _, declared := legalEdges[StatusQuarantined]; declared {
		t.Errorf("legalEdges declares %q as a source, want no entry at all", StatusQuarantined)
	}
	for _, from := range allStatuses {
		if canMove(from, StatusQuarantined) {
			t.Errorf("canMove(%q, %q) = true, want false: quarantine is a disposition, not a status", from, StatusQuarantined)
		}
	}
	// The distinction the rule rests on: this build HAS a word for it (a
	// delivery is recorded under it, and a console renders it), it is simply
	// not somewhere an intent moves.
	if !knownStatus(StatusQuarantined) {
		t.Errorf("knownStatus(%q) = false, want true: the vocabulary carries it even though the machine does not", StatusQuarantined)
	}
}

// TestKnownStatusNamesEveryStatusAndNothingElse checks the vocabulary guard an
// event carrying an unfamiliar status is refused by.
func TestKnownStatusNamesEveryStatusAndNothingElse(t *testing.T) {
	for _, status := range allStatuses {
		if !knownStatus(status) {
			t.Errorf("knownStatus(%q) = false, want true", status)
		}
	}
	for _, status := range []Status{"", "captured", "SUCCEEDED", "succeeded ", "quarantine", "refund"} {
		if knownStatus(status) {
			t.Errorf("knownStatus(%q) = true, want false", status)
		}
	}
}

// TestTopUpCommandKeysAreDeterministicAndDistinct checks the derivation's first
// two properties: the same inputs always name the same leg, and different
// inputs never do.
func TestTopUpCommandKeysAreDeterministicAndDistinct(t *testing.T) {
	first, err := TopUpCommandKey("acme", "ref_1")
	if err != nil {
		t.Fatalf("top-up key: %v", err)
	}
	again, err := TopUpCommandKey("acme", "ref_1")
	if err != nil {
		t.Fatalf("top-up key: %v", err)
	}
	if first != again {
		t.Fatalf("TopUpCommandKey is not deterministic: %q then %q", first, again)
	}

	// Two payments of one provider, and one payment under a second provider,
	// are three different legs.
	other, err := TopUpCommandKey("acme", "ref_2")
	if err != nil {
		t.Fatalf("top-up key: %v", err)
	}
	otherProvider, err := TopUpCommandKey("acme-2", "ref_1")
	if err != nil {
		t.Fatalf("top-up key: %v", err)
	}
	if first == other || first == otherProvider || other == otherProvider {
		t.Fatalf("distinct inputs produced a colliding key: %q, %q, %q", first, other, otherProvider)
	}
}

// TestCommandKeysFoldTheProviderButNotThePaymentReference pins the deliberate
// asymmetry: a provider is a configuration value this deployment chose, so
// there is one spelling of it, while a payment reference is the provider's own
// value and two references differing in case are two references to them.
func TestCommandKeysFoldTheProviderButNotThePaymentReference(t *testing.T) {
	// The provider names here are DELIBERATELY case-varied and use no real
	// provider's name, because the rule under test is that the fold collapses
	// spelling — case, surrounding space, punctuation — into one scope. Naming a
	// vendor here would put the provider's identity into a file that must not
	// have it (see the provider-vocabulary rule in internal/arch) without making
	// the case any stronger, since the fold sees characters and not a company.
	cases := []struct{ spelled, canonical string }{
		{"acme", "acme"},
		{"ACME", "acme"},
		{"  aCmE  ", "acme"},
		{"Acme Payments", "acme_payments"},
		{"ac*m*e", "ac_m_e"},
		{"acme-2", "acme-2"},
		{"42", "42"},
		{"Über", "_ber"},
	}
	for _, tc := range cases {
		folded, err := TopUpCommandKey(tc.spelled, "ref_1")
		if err != nil {
			t.Fatalf("top-up key for provider %q: %v", tc.spelled, err)
		}
		canonical, err := TopUpCommandKey(tc.canonical, "ref_1")
		if err != nil {
			t.Fatalf("top-up key for provider %q: %v", tc.canonical, err)
		}
		if folded != canonical {
			t.Errorf("provider %q folded to a different scope than %q: %q vs %q", tc.spelled, tc.canonical, folded, canonical)
		}
	}
	// A provider name that survives the fold as separators (or as nothing at
	// all) still names a scope, so two such providers cannot share one: the
	// empty namespace is replaced rather than produced.
	unusual := []struct{ provider, wantPrefix string }{
		{"   ", "pay:unnamed:topup:"},
		{"!!!", "pay:___:topup:"},
		{"☃", "pay:_:topup:"},
	}
	for _, tc := range unusual {
		key, err := TopUpCommandKey(tc.provider, "ref_1")
		if err != nil {
			t.Fatalf("TopUpCommandKey(%q, ...): %v", tc.provider, err)
		}
		if !strings.HasPrefix(key, tc.wantPrefix) {
			t.Errorf("TopUpCommandKey(%q, ...) = %q, want the %q scope", tc.provider, key, tc.wantPrefix)
		}
	}

	// The reference is carried byte for byte into the digest: two spellings of
	// one reference are two references, and the keys say so.
	lower, err := TopUpCommandKey("acme", "ref_abc")
	if err != nil {
		t.Fatalf("top-up key: %v", err)
	}
	upper, err := TopUpCommandKey("acme", "REF_ABC")
	if err != nil {
		t.Fatalf("top-up key: %v", err)
	}
	if lower == upper {
		t.Errorf("a payment reference was case-folded: %q and %q collided", "ref_abc", "REF_ABC")
	}
}

// TestTopUpAndRefundKeysNeverCollide checks the two namespaces are disjoint
// even when both name the same provider reference. If they were not, a refund
// adjustment would be reported as a duplicate of the topup it reverses.
func TestTopUpAndRefundKeysNeverCollide(t *testing.T) {
	topUp, err := TopUpCommandKey("acme", "ref_1")
	if err != nil {
		t.Fatalf("top-up key: %v", err)
	}
	refund, err := RefundCommandKey("acme", "ref_1", 1)
	if err != nil {
		t.Fatalf("refund key: %v", err)
	}
	if topUp == refund {
		t.Fatalf("a top-up and a refund over one reference share the key %q", topUp)
	}
	if !strings.Contains(topUp, ":topup:") || !strings.Contains(refund, ":refund:") {
		t.Fatalf("the purpose segment is not in the key: %q, %q", topUp, refund)
	}
	for _, key := range []string{topUp, refund} {
		if !strings.HasPrefix(key, "pay:acme:") {
			t.Errorf("key %q does not carry the namespace first", key)
		}
	}

	// Two refunds of one payment are two legs, and the key names the refunded
	// STATE each one leaves behind: the same payment stopped at two different
	// totals derives two keys. The counterpart — the same state reached twice
	// deriving ONE key, which is the convergence a redelivery needs — is
	// TestRefundCommandKeyNamesTheRefundedState.
	firstRefund, err := RefundCommandKey("acme", "ref_1", 400)
	if err != nil {
		t.Fatalf("refund key: %v", err)
	}
	secondRefund, err := RefundCommandKey("acme", "ref_1", 1000)
	if err != nil {
		t.Fatalf("refund key: %v", err)
	}
	if firstRefund == secondRefund {
		t.Errorf("a payment refunded 400 and then 1000 produced one key: %q", firstRefund)
	}
}

// TestCommandKeysRefuseAMissingProviderOrReference checks the two non-optional
// inputs. An absent reference is refused rather than digested: a key derived
// from nothing is a key two different payments can share.
func TestCommandKeysRefuseAMissingProviderOrReference(t *testing.T) {
	if _, err := TopUpCommandKey("", "ref_1"); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("TopUpCommandKey with a blank provider = %v, want ErrInvalidReference", err)
	}
	if _, err := TopUpCommandKey("acme", ""); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("TopUpCommandKey with a blank reference = %v, want ErrInvalidReference", err)
	}
	if _, err := RefundCommandKey("", "refund_1", 1); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("RefundCommandKey with a blank provider = %v, want ErrInvalidReference", err)
	}
	if _, err := RefundCommandKey("acme", "", 1); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("RefundCommandKey with a blank reference = %v, want ErrInvalidReference", err)
	}
	// The total is the third input and it is not optional either: a key naming
	// a refunded state has to name a state, and nothing has gone back at zero.
	// A zero that were accepted would derive one key for every payment's
	// un-refunded state, so two different payments' nothing would collide.
	for _, total := range []int64{0, -1} {
		if _, err := RefundCommandKey("acme", "refund_1", total); !errors.Is(err, ErrInvalidReference) {
			t.Errorf("RefundCommandKey with a refunded total of %d = %v, want ErrInvalidReference", total, err)
		}
	}
}

// TestRefundCommandKeyNamesTheRefundedState pins the two halves of what the key
// is derived from, because they pull in opposite directions and both are
// needed: the same payment at the same total must derive the SAME key, or a
// redelivery of an event already applied writes a second correction leg; and
// the same payment at a different total must derive a different one, or a
// second partial refund is swallowed as a duplicate of the first and the
// customer is never made whole for it.
//
// Neither half is visible from the other, which is why they share a test: an
// implementation keyed on the payment alone passes the first and fails the
// second, and one keyed on the delivery passes the second and fails the first.
func TestRefundCommandKeyNamesTheRefundedState(t *testing.T) {
	redelivered, err := RefundCommandKey("acme", "ref_1", 400)
	if err != nil {
		t.Fatalf("RefundCommandKey: %v", err)
	}
	again, err := RefundCommandKey("ACME", "ref_1", 400)
	if err != nil {
		t.Fatalf("RefundCommandKey: %v", err)
	}
	if redelivered != again {
		t.Errorf("one refunded state under two spellings of the provider produced %q and %q", redelivered, again)
	}

	grown, err := RefundCommandKey("acme", "ref_1", 401)
	if err != nil {
		t.Fatalf("RefundCommandKey: %v", err)
	}
	if grown == redelivered {
		t.Errorf("a payment refunded 400 and one refunded 401 share the key %q", grown)
	}

	// The separator is load-bearing: the reference is digested and the total
	// follows it in the clear, so an implementation that hashed the two
	// CONCATENATED would make the boundary a matter of spelling. These two
	// pairs are the same characters up to the split.
	assertDistinct(t, "a@1", 7, "a", 17)
	assertDistinct(t, "ab", 3, "a", 23)
}

// assertDistinct fails if the two (reference, total) pairs derive one key.
func assertDistinct(t *testing.T, firstRef string, firstTotal int64, secondRef string, secondTotal int64) {
	t.Helper()
	first, err := RefundCommandKey("acme", firstRef, firstTotal)
	if err != nil {
		t.Fatalf("RefundCommandKey(%q, %d): %v", firstRef, firstTotal, err)
	}
	second, err := RefundCommandKey("acme", secondRef, secondTotal)
	if err != nil {
		t.Fatalf("RefundCommandKey(%q, %d): %v", secondRef, secondTotal, err)
	}
	if first == second {
		t.Errorf("the refunded states (%q, %d) and (%q, %d) share the key %q — a digest over a concatenation would do that",
			firstRef, firstTotal, secondRef, secondTotal, first)
	}
}

// TestCommandKeysAreFixedLengthByConstruction pins the property that keeps the
// key under B6's 256-character ceiling no matter what a provider sends: the
// digest is the length of the ALGORITHM, not of the input, and it is ASCII so
// the byte bound and the character bound agree.
//
// The refund key is bounded the same way even though its tail is not fixed: the
// variable part is a decimal total, which is at most nineteen digits for the
// largest int64 this domain can carry, so the worst case is a constant a reader
// can check rather than a length that grows with the input. Both figures are
// asserted, because the reason the ceiling holds is that the PROVIDER's string
// is digested — everything after the digest comes from this build's own
// arithmetic.
func TestCommandKeysAreFixedLengthByConstruction(t *testing.T) {
	wantLength := len("pay:acme:topup:") + 64
	wantRefundCeiling := len("pay:acme:refund:") + 64 + len(".") + len("9223372036854775807")
	for _, size := range []int{1, 2, 63, 64, 255, 256, 4096} {
		reference := strings.Repeat("r", size)
		key, err := TopUpCommandKey("acme", reference)
		if err != nil {
			t.Fatalf("top-up key for a %d-byte reference: %v", size, err)
		}
		if len(key) != wantLength {
			t.Errorf("TopUpCommandKey for a %d-byte reference is %d bytes, want %d", size, len(key), wantLength)
		}
		if utf8.RuneCountInString(key) != len(key) {
			t.Errorf("key %q is not ASCII: %d runes, %d bytes — B6's byte bound and the schema's character bound would disagree", key, utf8.RuneCountInString(key), len(key))
		}

		// The largest total a capture can reach is the largest int64, which is
		// also the widest the tail can be. A digest over the reference is what
		// keeps the leading part independent of `size`.
		refundKey, err := RefundCommandKey("acme", reference, math.MaxInt64)
		if err != nil {
			t.Fatalf("refund key for a %d-byte reference: %v", size, err)
		}
		if got := len(refundKey); got != wantRefundCeiling {
			t.Errorf("RefundCommandKey for a %d-byte reference at the largest total is %d bytes, want %d — B6 refuses a key past 256 characters", size, got, wantRefundCeiling)
		}
		if utf8.RuneCountInString(refundKey) != len(refundKey) {
			t.Errorf("refund key %q is not ASCII: %d runes, %d bytes", refundKey, utf8.RuneCountInString(refundKey), len(refundKey))
		}
		if wantRefundCeiling > 256 {
			t.Fatalf("the refund key's worst case is %d bytes, which B6 refuses: the total must not be carried in the clear", wantRefundCeiling)
		}
	}
}

// TestCommandKeysSeparateNeighbouringReferences is the collision property the
// digest exists for: a truncation would give two references sharing a prefix
// one key and one credit where two were owed, silently and permanently.
func TestCommandKeysSeparateNeighbouringReferences(t *testing.T) {
	neighbours := [][2]string{
		{"ref_000", "ref_001"},
		{"a", "b"},
		{strings.Repeat("r", 300) + "a", strings.Repeat("r", 300) + "b"},
		{"ref_é", "ref_è"},
	}
	for _, pair := range neighbours {
		first, err := TopUpCommandKey("acme", pair[0])
		if err != nil {
			t.Fatalf("top-up key for %q: %v", pair[0], err)
		}
		second, err := TopUpCommandKey("acme", pair[1])
		if err != nil {
			t.Fatalf("top-up key for %q: %v", pair[1], err)
		}
		if first == second {
			t.Errorf("references %q and %q produced one key %q", pair[0], pair[1], first)
		}
	}
}

// TestRefundCeilingIsTheCapturedAmountAndNothingElse checks the ceiling's
// domain and its refusals: a ceiling is a positive captured amount, and a
// figure that is not one is refused rather than used as the base of a
// comparison.
func TestRefundCeilingIsTheCapturedAmountAndNothingElse(t *testing.T) {
	ceiling, err := RefundCeiling(1234)
	if err != nil {
		t.Fatalf("RefundCeiling(1234): %v", err)
	}
	if ceiling != 1234 {
		t.Errorf("RefundCeiling(1234) = %d, want 1234", ceiling)
	}
	for _, captured := range []int64{0, -1, math.MinInt64} {
		if _, err := RefundCeiling(captured); !errors.Is(err, ErrInvalidReference) {
			t.Errorf("RefundCeiling(%d) = %v, want ErrInvalidReference", captured, err)
		}
	}
}

// TestCanRefundAcceptsTheCeilingAndRefusesOneMinorUnitOver checks the inclusive
// boundary: two partial refunds summing exactly to the capture are legal, and
// one unit past it is not.
func TestCanRefundAcceptsTheCeilingAndRefusesOneMinorUnitOver(t *testing.T) {
	room, err := CanRefund(0, 100, 100)
	if err != nil {
		t.Fatalf("CanRefund(0, 100, 100): %v", err)
	}
	if room != 100 {
		t.Errorf("CanRefund(0, 100, 100) = %d, want 100: the ceiling is inclusive", room)
	}
	over, err := CanRefund(0, 101, 100)
	if !errors.Is(err, ErrRefundCeiling) {
		t.Fatalf("CanRefund(0, 101, 100) = %v, want ErrRefundCeiling", err)
	}
	if over != 100 {
		t.Errorf("a refused refund reported %d refundable, want the 100 still available", over)
	}
	// A partial followed by its remainder is exactly the ceiling.
	if room, err = CanRefund(40, 60, 100); err != nil || room != 60 {
		t.Errorf("CanRefund(40, 60, 100) = (%d, %v), want (60, nil)", room, err)
	}
	// One unit past the REMAINING room is refused too, and reports the room.
	if room, err = CanRefund(60, 41, 100); !errors.Is(err, ErrRefundCeiling) || room != 40 {
		t.Errorf("CanRefund(60, 41, 100) = (%d, %v), want (40, ErrRefundCeiling)", room, err)
	}
}

// TestCanRefundRefusesANonPositiveRequestAndAnImpossibleTotal pins the two
// refusals that are not about the ceiling: a refund of nothing is not a
// refund, and a payment already refunded outside [0, ceiling] is a state this
// build's own rules cannot reach, so it is refused loudly rather than used.
func TestCanRefundRefusesANonPositiveRequestAndAnImpossibleTotal(t *testing.T) {
	if _, err := CanRefund(0, 0, 100); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("CanRefund(0, 0, 100) = %v, want ErrInvalidReference", err)
	}
	if _, err := CanRefund(0, -1, 100); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("CanRefund(0, -1, 100) = %v, want ErrInvalidReference", err)
	}
	if _, err := CanRefund(-1, 10, 100); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("CanRefund(-1, 10, 100) = %v, want ErrInvalidReference", err)
	}
	if _, err := CanRefund(101, 10, 100); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("CanRefund(101, 10, 100) = %v, want ErrInvalidReference", err)
	}
	if _, err := CanRefund(0, 1, 0); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("CanRefund(0, 1, 0) = %v, want ErrInvalidReference: there is no capture to refund", err)
	}
}

// TestCanRefundNeverAcceptsMoreThanWasCaptured is the most important arithmetic
// in the package. The check is a subtraction rather than an addition because
// already+requested wraps past MaxInt64, and a wrapped total is NEGATIVE —
// which is not greater than the ceiling, so the naive implementation accepts a
// refund larger than the capture. If any input here can produce an accepted
// room above what was captured, that is a P0: it books a correction for money
// the ledger never took.
func TestCanRefundNeverAcceptsMoreThanWasCaptured(t *testing.T) {
	accepted := []struct{ already, requested, captured int64 }{
		{0, 1, 1},
		{0, 100, 100},
		{99, 1, 100},
		{0, math.MaxInt64, math.MaxInt64},
		{math.MaxInt64 - 1, 1, math.MaxInt64},
		{1, math.MaxInt64 - 1, math.MaxInt64},
	}
	for _, tc := range accepted {
		room, err := CanRefund(tc.already, tc.requested, tc.captured)
		if err != nil {
			// The only acceptable refusal of an in-table case is the one whose
			// request genuinely does not fit; the table keeps those below.
			t.Fatalf("CanRefund(%d, %d, %d) = %v, want acceptance", tc.already, tc.requested, tc.captured, err)
		}
		if room != tc.requested {
			t.Errorf("CanRefund(%d, %d, %d) = %d, want the requested %d", tc.already, tc.requested, tc.captured, room, tc.requested)
		}
		if room > tc.captured-tc.already {
			t.Errorf("CanRefund(%d, %d, %d) accepted %d, which is more than the %d still captured",
				tc.already, tc.requested, tc.captured, room, tc.captured-tc.already)
		}
	}

	refused := []struct{ already, requested, captured int64 }{
		{100, 1, 100},
		{5, math.MaxInt64, math.MaxInt64},
		{math.MaxInt64 - 1, math.MaxInt64, math.MaxInt64},
		{math.MaxInt64, math.MaxInt64, math.MaxInt64},
		{math.MaxInt64, 1, math.MaxInt64},
	}
	for _, tc := range refused {
		room, err := CanRefund(tc.already, tc.requested, tc.captured)
		// ErrRefundCeiling, and NOT the umbrella ErrInvalidReference, is what
		// these rows earn. The arithmetic is what the table has always pinned
		// and it is unchanged; what is asserted here is the SENTINEL, because a
		// caller above this package has to tell a refund that does not fit from
		// a delivery that named something this payment is not, and one sentinel
		// for both makes that untellable. See ErrRefundCeiling.
		if !errors.Is(err, ErrRefundCeiling) {
			t.Errorf("CanRefund(%d, %d, %d) = (%d, %v), want ErrRefundCeiling: it accepts more than was captured",
				tc.already, tc.requested, tc.captured, room, err)
			continue
		}
		if room > tc.captured-tc.already || room < 0 {
			t.Errorf("CanRefund(%d, %d, %d) refused with room %d, want a value in [0, %d]",
				tc.already, tc.requested, tc.captured, room, tc.captured-tc.already)
		}
	}

	// The wrap the naive form reaches: already+requested overflows to a
	// negative, and a negative total compares as under the ceiling.
	var wraps int64 = math.MaxInt64
	wrapped := wraps + 1
	if wrapped >= 0 {
		t.Fatalf("this test's premise is broken: MaxInt64+1 did not wrap to a negative (%d)", wrapped)
	}
	if _, err := CanRefund(math.MaxInt64, 1, math.MaxInt64); !errors.Is(err, ErrRefundCeiling) {
		t.Errorf("a refund one unit past a MaxInt64 capture was accepted: an overflowed total is not a ceiling")
	}
}

// TestValidProviderReferenceBoundsTheReferenceAtItsEdge checks the bound that
// keeps a third party's identifier out of a column and out of a key, at exactly
// the maximum and one past it.
func TestValidProviderReferenceBoundsTheReferenceAtItsEdge(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want bool
	}{
		{"empty", "", false},
		{"one byte", "r", true},
		{"the maximum", strings.Repeat("r", maxProviderReferenceLength), true},
		{"one past the maximum", strings.Repeat("r", maxProviderReferenceLength+1), false},
		{"far past the maximum", strings.Repeat("r", 4096), false},
		{"whitespace is a reference this build carries", " ", true},
	}
	for _, tc := range cases {
		if got := validProviderReference(tc.ref); got != tc.want {
			t.Errorf("validProviderReference(%s) = %v, want %v", tc.name, got, tc.want)
		}
		if got := ValidProviderReference(tc.ref); got != tc.want {
			t.Errorf("ValidProviderReference(%s) = %v, want %v: the exported bound is not the same bound", tc.name, got, tc.want)
		}
	}
}

// TestOccurredWithinToleranceIsSymmetric pins the sign of the freshness test:
// a provider clock running slightly ahead of this platform's is common and
// benign, so the bound admits it in both directions, and it is a bound on
// staleness rather than a replay defence.
func TestOccurredWithinToleranceIsSymmetric(t *testing.T) {
	const tolerance = 5 * time.Minute
	cases := []struct {
		name     string
		occurred time.Time
		want     bool
	}{
		{"now", testNow, true},
		{"half the window old", testNow.Add(-tolerance / 2), true},
		{"exactly at the old edge", testNow.Add(-tolerance), true},
		{"one nanosecond past the old edge", testNow.Add(-tolerance - time.Nanosecond), false},
		{"half the window ahead", testNow.Add(tolerance / 2), true},
		{"exactly at the ahead edge", testNow.Add(tolerance), true},
		{"one nanosecond past the ahead edge", testNow.Add(tolerance + time.Nanosecond), false},
		{"an hour old", testNow.Add(-time.Hour), false},
		{"an hour ahead", testNow.Add(time.Hour), false},
	}
	for _, tc := range cases {
		if got := occurredWithinTolerance(tc.occurred, testNow, tolerance); got != tc.want {
			t.Errorf("occurredWithinTolerance(%s, now, %s) = %v, want %v", tc.name, tolerance, got, tc.want)
		}
	}
}

// TestOccurredWithinToleranceAdmitsEverythingWithNoWindow pins the zero and
// negative tolerance: there is no window, so every delivery is fresh. A caller
// that wants a window configures one; the function does not invent one.
func TestOccurredWithinToleranceAdmitsEverythingWithNoWindow(t *testing.T) {
	for _, tolerance := range []time.Duration{0, -time.Second, -time.Hour} {
		for _, occurred := range []time.Time{testNow.Add(-1000 * time.Hour), testNow, testNow.Add(1000 * time.Hour)} {
			if !occurredWithinTolerance(occurred, testNow, tolerance) {
				t.Errorf("occurredWithinTolerance(%s, now, %s) = false, want true: a tolerance of %s is no window at all",
					occurred.Sub(testNow), tolerance, tolerance)
			}
		}
	}
}

// TestCurrencyCodeUpperFoldsSpellingAndNothingElse checks the normalisation
// that keeps a comparison honest: "usd" is unambiguously USD, and a provider
// spelling it in lowercase is not a currency mismatch.
func TestCurrencyCodeUpperFoldsSpellingAndNothingElse(t *testing.T) {
	cases := []struct{ in, want string }{
		{"USD", "USD"},
		{"usd", "USD"},
		{"UsD", "USD"},
		{" usd ", "USD"},
		{"", ""},
		{"us", "US"},
		{"USDD", "USDD"},
		{"us1", "US1"},
	}
	for _, tc := range cases {
		if got := currencyCodeUpper(tc.in); got != tc.want {
			t.Errorf("currencyCodeUpper(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := CurrencyCode(tc.in); got != tc.want {
			t.Errorf("CurrencyCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
