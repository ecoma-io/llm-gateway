package payments

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// The lifecycle: how a payment is born, how an open checkout records where the
// customer goes, whether a delivery is the capture of this payment, what a
// refund does to it, and when a local deadline moves it on.
//
// The tests below are the four refusals this file exists for — New, OpenCheckout,
// MatchCapture and RecordRefund each refuse more than they accept — plus the
// two operations that answer a question differently from the way a boolean
// would (SettleExpired and the ordering inside MatchCapture).

const (
	testAccountID = "account-1"
	testBucketID  = "bucket-1"
	testPaymentID = "01234567-89ab-7def-8123-456789abcdef"
)

// refOfLength is a provider reference of exactly n bytes, for the bound tests.
func refOfLength(n int) string { return strings.Repeat("r", n) }

// validNewPayment is a payment that New accepts. Every refusal case below
// perturbs exactly one field of it, so a failure names the field and not the
// fixture.
func validNewPayment() NewPayment {
	return NewPayment{
		AccountID:         testAccountID,
		FundingBucketID:   testBucketID,
		AmountMinorUnits:  1000,
		Currency:          "USD",
		MinorUnitExponent: 2,
		Provider:          "stripe",
		IdempotencyKey:    "idem-1",
		CheckoutTTL:       time.Hour,
		Now:               testNow,
		MintedID:          IntentID(testPaymentID),
		MintedAt:          testNow,
	}
}

// mustNewIntent births a payment, perturbed by tweak when one is given.
func mustNewIntent(t *testing.T, tweak func(*NewPayment)) Intent {
	t.Helper()
	in := validNewPayment()
	if tweak != nil {
		tweak(&in)
	}
	intent, err := New(in)
	if err != nil {
		t.Fatalf("New(%+v): %v", in, err)
	}
	return intent
}

// intentAt is a payment in a given status, carrying a capture reference when
// the status implies one. The fixtures for the state-machine refusals read as
// the situation they describe rather than as a struct literal.
func intentAt(t *testing.T, status Status) Intent {
	t.Helper()
	intent := mustNewIntent(t, nil)
	intent.Status = status
	switch status {
	case StatusSucceeded, StatusPartiallyRefunded, StatusRefunded:
		intent.ProviderPaymentRef = "pi_1"
	case StatusCheckoutOpen, StatusRequiresAction, StatusExpired, StatusCancelled:
		intent.ProviderCheckoutRef = "cs_1"
	}
	return intent
}

// TestNewRefusesWhatIsNotAPayment enumerates the refusals. Every one of them is
// ErrInvalidReference and none of them is a repair: a currency is not guessed,
// a bucket is not truncated, and an amount of nothing is not a top-up.
func TestNewRefusesWhatIsNotAPayment(t *testing.T) {
	cases := []struct {
		name  string
		tweak func(*NewPayment)
	}{
		{"a payment with no account", func(in *NewPayment) { in.AccountID = "" }},
		{"a payment with no funding bucket", func(in *NewPayment) { in.FundingBucketID = "" }},
		{"a funding bucket reference past the bound", func(in *NewPayment) { in.FundingBucketID = refOfLength(maxProviderReferenceLength + 1) }},
		{"a top-up of nothing", func(in *NewPayment) { in.AmountMinorUnits = 0 }},
		{"a top-up of a negative amount", func(in *NewPayment) { in.AmountMinorUnits = -1 }},
		{"no currency", func(in *NewPayment) { in.Currency = "" }},
		{"a two-letter currency", func(in *NewPayment) { in.Currency = "US" }},
		{"a four-letter currency", func(in *NewPayment) { in.Currency = "USDD" }},
		{"a currency with a digit in it", func(in *NewPayment) { in.Currency = "US1" }},
		{"a negative minor-unit exponent", func(in *NewPayment) { in.MinorUnitExponent = -1 }},
		{"a minor-unit exponent past four", func(in *NewPayment) { in.MinorUnitExponent = 5 }},
		{"a payment with no provider", func(in *NewPayment) { in.Provider = "" }},
		{"a payment with no idempotency key", func(in *NewPayment) { in.IdempotencyKey = "" }},
		{"an idempotency key past the bound", func(in *NewPayment) { in.IdempotencyKey = strings.Repeat("k", maxIdempotencyKeyLength+1) }},
		{"a checkout with no lifetime", func(in *NewPayment) { in.CheckoutTTL = 0 }},
		{"a checkout with a negative lifetime", func(in *NewPayment) { in.CheckoutTTL = -time.Second }},
		{"a checkout lifetime past the ceiling", func(in *NewPayment) { in.CheckoutTTL = maxCheckoutTTL + time.Nanosecond }},
		{"a payment with no identity", func(in *NewPayment) { in.MintedID = "" }},
		{"a payment with no creation time", func(in *NewPayment) { in.MintedAt = time.Time{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validNewPayment()
			tc.tweak(&in)
			if _, err := New(in); !errors.Is(err, ErrInvalidReference) {
				t.Fatalf("New(%s) = %v, want ErrInvalidReference", tc.name, err)
			}
		})
	}
}

// TestNewAcceptsTheBoundaryValues checks the other side of every bound: the
// smallest legal amount, both ends of the exponent range, the exact maximum
// reference and key lengths, the exact maximum lifetime, and a currency spelled
// the way a provider might send it.
func TestNewAcceptsTheBoundaryValues(t *testing.T) {
	cases := []struct {
		name  string
		tweak func(*NewPayment)
		check func(*testing.T, Intent)
	}{
		{
			name:  "the smallest top-up",
			tweak: func(in *NewPayment) { in.AmountMinorUnits = 1 },
			check: func(t *testing.T, intent Intent) {
				if intent.AmountMinorUnits != 1 {
					t.Errorf("amount = %d, want 1", intent.AmountMinorUnits)
				}
			},
		},
		{
			name:  "a currency with no minor unit",
			tweak: func(in *NewPayment) { in.MinorUnitExponent = 0 },
			check: func(t *testing.T, intent Intent) {
				if intent.MinorUnitExponent != 0 {
					t.Errorf("exponent = %d, want 0", intent.MinorUnitExponent)
				}
			},
		},
		{
			name:  "a currency with four decimal places",
			tweak: func(in *NewPayment) { in.MinorUnitExponent = 4 },
			check: func(t *testing.T, intent Intent) {
				if intent.MinorUnitExponent != 4 {
					t.Errorf("exponent = %d, want 4", intent.MinorUnitExponent)
				}
			},
		},
		{
			name:  "a funding bucket reference at the bound",
			tweak: func(in *NewPayment) { in.FundingBucketID = refOfLength(maxProviderReferenceLength) },
			check: func(t *testing.T, intent Intent) {
				if len(intent.FundingBucketID) != maxProviderReferenceLength {
					t.Errorf("bucket reference length = %d, want %d", len(intent.FundingBucketID), maxProviderReferenceLength)
				}
			},
		},
		{
			name:  "an idempotency key at the bound",
			tweak: func(in *NewPayment) { in.IdempotencyKey = strings.Repeat("k", maxIdempotencyKeyLength) },
			check: func(t *testing.T, intent Intent) {
				if len(intent.IdempotencyKey) != maxIdempotencyKeyLength {
					t.Errorf("idempotency key length = %d, want %d", len(intent.IdempotencyKey), maxIdempotencyKeyLength)
				}
			},
		},
		{
			name:  "a checkout lifetime exactly at the ceiling",
			tweak: func(in *NewPayment) { in.CheckoutTTL = maxCheckoutTTL },
			check: func(t *testing.T, intent Intent) {
				if !intent.ExpiresAt.Equal(testNow.Add(maxCheckoutTTL)) {
					t.Errorf("expiry = %s, want %s", intent.ExpiresAt, testNow.Add(maxCheckoutTTL))
				}
			},
		},
		{
			name:  "a checkout lifetime of almost nothing",
			tweak: func(in *NewPayment) { in.CheckoutTTL = time.Nanosecond },
			check: func(t *testing.T, intent Intent) {
				if !intent.ExpiresAt.Equal(testNow.Add(time.Nanosecond)) {
					t.Errorf("expiry = %s, want %s", intent.ExpiresAt, testNow.Add(time.Nanosecond))
				}
			},
		},
		{
			name:  "a currency spelled the way a provider might send it",
			tweak: func(in *NewPayment) { in.Currency = " usd " },
			check: func(t *testing.T, intent Intent) {
				if intent.Currency != "USD" {
					t.Errorf("currency = %q, want the folded %q", intent.Currency, "USD")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, mustNewIntent(t, tc.tweak))
		})
	}
}

// TestNewBirthsAPaymentInItsBirthState checks everything New decides on its
// own: the state, the zeroed figures, and the stamps. A payment is created
// BEFORE the provider is called, so all of this must be durable without any
// provider having answered anything.
func TestNewBirthsAPaymentInItsBirthState(t *testing.T) {
	intent := mustNewIntent(t, nil)

	if intent.Status != StatusCreated {
		t.Errorf("status = %q, want %q", intent.Status, StatusCreated)
	}
	if intent.ID != IntentID(testPaymentID) {
		t.Errorf("id = %q, want the minted %q", intent.ID, testPaymentID)
	}
	if intent.RefundedMinorUnits != 0 || intent.UncoveredRefundMinorUnits != 0 || intent.StateVersion != 0 {
		t.Errorf("a new payment carries figures: refunded %d, uncovered %d, version %d",
			intent.RefundedMinorUnits, intent.UncoveredRefundMinorUnits, intent.StateVersion)
	}
	if intent.ProviderCheckoutRef != "" || intent.ProviderPaymentRef != "" || intent.CheckoutURL != "" {
		t.Errorf("a new payment already carries provider answers: checkout ref %q, payment ref %q, url %q",
			intent.ProviderCheckoutRef, intent.ProviderPaymentRef, intent.CheckoutURL)
	}
	if !intent.CreatedAt.Equal(testNow) || !intent.UpdatedAt.Equal(testNow) {
		t.Errorf("stamps = (%s, %s), want both %s", intent.CreatedAt, intent.UpdatedAt, testNow)
	}
	if !intent.ExpiresAt.Equal(testNow.Add(time.Hour)) {
		t.Errorf("expiry = %s, want the mint time plus the checkout lifetime (%s)", intent.ExpiresAt, testNow.Add(time.Hour))
	}
	if intent.ExpiresAt.Location() != time.UTC {
		t.Errorf("expiry location = %s, want UTC", intent.ExpiresAt.Location())
	}
}

// TestNewStoresTheCallersIdempotencyKeyVerbatim documents what New does with
// the key, because the comment directly above the check says something else.
//
// FINDING: the comment says "The key is folded here, at the boundary, and
// stored folded, so the uniqueness that catches a duplicate click compares like
// with like. A caller that spelled the same key 'PAY' twice would otherwise get
// two payments". The code does not fold it: the value is validated for
// emptiness and length and then stored exactly as it arrived, so "PAY" and
// "pay" are two payments — the outcome the comment says the fold exists to
// prevent. I believe the comment is stale rather than the code being wrong
// (folding an opaque client key is itself questionable), but one of the two has
// to move. Observed here; inferred on the intent.
func TestNewStoresTheCallersIdempotencyKeyVerbatim(t *testing.T) {
	upper := mustNewIntent(t, func(in *NewPayment) { in.IdempotencyKey = "PAY" })
	lower := mustNewIntent(t, func(in *NewPayment) { in.IdempotencyKey = "pay" })

	if upper.IdempotencyKey != "PAY" {
		t.Errorf("idempotency key = %q, want the caller's own %q", upper.IdempotencyKey, "PAY")
	}
	if upper.IdempotencyKey == lower.IdempotencyKey {
		t.Fatalf("the key was folded after all: %q and %q converged", "PAY", "pay")
	}
}

// TestOpenCheckoutMovesCreatedToCheckoutOpenAndCarriesTheProviderAnswer checks
// the one legal move: the URL and the reference are stored verbatim, and
// nothing else about the payment changes.
func TestOpenCheckoutMovesCreatedToCheckoutOpenAndCarriesTheProviderAnswer(t *testing.T) {
	intent := mustNewIntent(t, nil)
	later := testNow.Add(time.Minute)

	next, err := intent.OpenCheckout(OpenedCheckout{
		URL:         "https://pay.example/checkout/session_1",
		ProviderRef: "cs_1",
	}, later)
	if err != nil {
		t.Fatalf("OpenCheckout: %v", err)
	}
	if next.Status != StatusCheckoutOpen {
		t.Errorf("status = %q, want %q", next.Status, StatusCheckoutOpen)
	}
	if next.CheckoutURL != "https://pay.example/checkout/session_1" {
		t.Errorf("url = %q, want the provider's verbatim", next.CheckoutURL)
	}
	if next.ProviderCheckoutRef != "cs_1" {
		t.Errorf("checkout reference = %q, want %q", next.ProviderCheckoutRef, "cs_1")
	}
	if !next.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt = %s, want %s", next.UpdatedAt, later)
	}
	if !next.CreatedAt.Equal(intent.CreatedAt) {
		t.Errorf("a move restamped creation: CreatedAt = %s, want %s", next.CreatedAt, intent.CreatedAt)
	}
	if next.AmountMinorUnits != intent.AmountMinorUnits || next.Currency != intent.Currency ||
		next.AccountID != intent.AccountID || next.FundingBucketID != intent.FundingBucketID ||
		next.IdempotencyKey != intent.IdempotencyKey || next.ID != intent.ID {
		t.Errorf("a checkout move changed the payment's identity or its ownership")
	}
	// The receiver is a value: the caller's payment is untouched, which is what
	// makes the caller's own compare-and-swap the thing that writes it.
	if intent.Status != StatusCreated || intent.ProviderCheckoutRef != "" {
		t.Errorf("OpenCheckout mutated its receiver: status %q, reference %q", intent.Status, intent.ProviderCheckoutRef)
	}
}

// TestOpenCheckoutIsAcceptedFromExactlyTheStatesTheTableNames walks the whole
// vocabulary rather than a sample. Two states may open a checkout and the
// second is easy to miss: created is a payment that has never had one, and
// requires_action is a customer the provider sent back to finish — both have an
// edge to checkout_open in the table, and every other state is refused.
func TestOpenCheckoutIsAcceptedFromExactlyTheStatesTheTableNames(t *testing.T) {
	checkout := OpenedCheckout{URL: "https://pay.example/checkout/session_1", ProviderRef: "cs_1"}
	openable := []Status{StatusCreated, StatusRequiresAction}
	for _, status := range allStatuses {
		intent := intentAt(t, status)
		next, err := intent.OpenCheckout(checkout, testNow)
		if hasEdge(openable, status) {
			if err != nil {
				t.Fatalf("OpenCheckout from %q = %v, want acceptance", status, err)
			}
			if next.Status != StatusCheckoutOpen {
				t.Errorf("OpenCheckout from %q left the payment at %q", status, next.Status)
			}
			continue
		}
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("OpenCheckout from %q = %v, want ErrInvalidTransition", status, err)
		}
		if next.ID != "" {
			t.Errorf("OpenCheckout from %q returned a usable payment alongside its refusal", status)
		}
	}
}

// TestOpenCheckoutRefusesAURLOrReferenceAConsoleCouldNotUse checks the two
// values that would make the feature look like it worked: a blank URL is a
// button that goes nowhere and a blank or oversized reference is a payment an
// event can never be matched to.
func TestOpenCheckoutRefusesAURLOrReferenceAConsoleCouldNotUse(t *testing.T) {
	cases := []struct {
		name     string
		checkout OpenedCheckout
	}{
		{"no URL", OpenedCheckout{ProviderRef: "cs_1"}},
		{"no provider reference", OpenedCheckout{URL: "https://pay.example/checkout/session_1"}},
		{"a provider reference past the bound", OpenedCheckout{
			URL:         "https://pay.example/checkout/session_1",
			ProviderRef: refOfLength(maxProviderReferenceLength + 1),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intent := mustNewIntent(t, nil)
			next, err := intent.OpenCheckout(tc.checkout, testNow)
			if !errors.Is(err, ErrInvalidReference) {
				t.Fatalf("OpenCheckout with %s = %v, want ErrInvalidReference", tc.name, err)
			}
			if next.ID != "" {
				t.Errorf("OpenCheckout with %s returned a payment alongside its refusal", tc.name)
			}
		})
	}
}

// TestMatchCaptureReportsTheCurrencyBeforeTheAmount checks the ordering that is
// the whole of this function's design: an amount is not a figure until its unit
// is known. The case constructed here disagrees about BOTH, and the refusal
// must name the currency — comparing the integers first would let a capture in
// another currency through whenever the numbers happened to agree.
func TestMatchCaptureReportsTheCurrencyBeforeTheAmount(t *testing.T) {
	intent := mustNewIntent(t, nil) // 1000 USD, created
	intent.Status = StatusCheckoutOpen
	disagreeing := int64(999)

	_, err := intent.MatchCapture("pi_1", &disagreeing, "EUR")
	if !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("MatchCapture = %v, want ErrInvalidReference", err)
	}
	if !strings.Contains(err.Error(), "EUR") || !strings.Contains(err.Error(), "denominated") {
		t.Errorf("MatchCapture named %v, want the currency the payment is not denominated in (%q)", err, "EUR")
	}
	if strings.Contains(err.Error(), "999") {
		t.Errorf("MatchCapture reported the amount before the currency: %v", err)
	}

	// And the same two figures agreeing on the currency do name the amount, so
	// the test above is about the order and not about a missing message.
	_, err = intent.MatchCapture("pi_1", &disagreeing, "usd")
	if !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("MatchCapture = %v, want ErrInvalidReference", err)
	}
	if !strings.Contains(err.Error(), "999") || !strings.Contains(err.Error(), "1000") {
		t.Errorf("MatchCapture named %v, want the two amounts it disagrees about", err)
	}
}

// TestMatchCaptureRefusesANilAmountWhichIsNotAnAmountOfZero pins the port's
// pointer-shaped money: an absent amount and an amount of zero are different
// sentences, and neither is a capture.
func TestMatchCaptureRefusesANilAmountWhichIsNotAnAmountOfZero(t *testing.T) {
	intent := mustNewIntent(t, nil)
	intent.Status = StatusCheckoutOpen

	_, nilErr := intent.MatchCapture("pi_1", nil, "USD")
	if !errors.Is(nilErr, ErrInvalidReference) {
		t.Fatalf("MatchCapture with no amount = %v, want ErrInvalidReference", nilErr)
	}
	zero := int64(0)
	_, zeroErr := intent.MatchCapture("pi_1", &zero, "USD")
	if !errors.Is(zeroErr, ErrInvalidReference) {
		t.Fatalf("MatchCapture of zero = %v, want ErrInvalidReference", zeroErr)
	}
	if nilErr.Error() == zeroErr.Error() {
		t.Errorf("an absent amount and an amount of zero are reported identically: %v", nilErr)
	}
	if !strings.Contains(nilErr.Error(), "states none") {
		t.Errorf("MatchCapture with no amount said %v, want it to say no amount was stated", nilErr)
	}

	negative := int64(-1000)
	if _, err := intent.MatchCapture("pi_1", &negative, "USD"); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("MatchCapture of a negative amount = %v, want ErrInvalidReference", err)
	}
}

// TestMatchCaptureIsAcceptedFromEveryWaitableState walks the four states a
// capture may legally move from — including the two the state machine calls
// late payments — and checks the key it derives is the one the funding leg will
// carry.
func TestMatchCaptureIsAcceptedFromEveryWaitableState(t *testing.T) {
	amount := int64(1000)
	for _, status := range []Status{StatusCheckoutOpen, StatusRequiresAction, StatusExpired, StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			intent := mustNewIntent(t, nil)
			intent.Status = status

			match, err := intent.MatchCapture("pi_1", &amount, "USD")
			if err != nil {
				t.Fatalf("MatchCapture from %q = %v, want acceptance", status, err)
			}
			if !match.Matched {
				t.Fatalf("MatchCapture from %q did not match", status)
			}
			wantKey, err := TopUpCommandKey("stripe", "pi_1")
			if err != nil {
				t.Fatalf("top-up key: %v", err)
			}
			if match.CommandKey != wantKey {
				t.Errorf("command key = %q, want the derived %q", match.CommandKey, wantKey)
			}
		})
	}
}

// TestMatchCaptureRefusesEveryStateItCannotFund walks the rest of the
// vocabulary. A payment at `created` was never sent to anybody; a payment that
// already succeeded has been funded; and the refunded ones have had the
// provider's final word.
func TestMatchCaptureRefusesEveryStateItCannotFund(t *testing.T) {
	amount := int64(1000)
	for _, status := range allStatuses {
		switch status {
		case StatusCheckoutOpen, StatusRequiresAction, StatusExpired, StatusCancelled:
			continue
		}
		t.Run(string(status), func(t *testing.T) {
			intent := mustNewIntent(t, nil)
			intent.Status = status

			match, err := intent.MatchCapture("pi_1", &amount, "USD")
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("MatchCapture at %q = %v, want ErrInvalidTransition", status, err)
			}
			if match.Matched || match.CommandKey != "" {
				t.Errorf("a refused capture carried a verdict or a key: %+v", match)
			}
		})
	}
}

// TestMatchCaptureRefusesASecondCaptureUnderAnotherReference checks a payment
// has one capture: a provider that renamed a charge between deliveries is
// refused rather than funding the same payment under two identities.
func TestMatchCaptureRefusesASecondCaptureUnderAnotherReference(t *testing.T) {
	intent := mustNewIntent(t, nil)
	intent.Status = StatusCheckoutOpen
	intent.ProviderPaymentRef = "pi_1"
	amount := int64(1000)

	_, err := intent.MatchCapture("pi_2", &amount, "USD")
	if !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("MatchCapture under a second reference = %v, want ErrInvalidReference", err)
	}
	if !strings.Contains(err.Error(), "pi_1") || !strings.Contains(err.Error(), "pi_2") {
		t.Errorf("the refusal named %v, want both references", err)
	}

	// The payment's own reference is still its own: a redelivery of the same
	// capture is not a second capture.
	if _, err := intent.MatchCapture("pi_1", &amount, "USD"); err != nil {
		t.Fatalf("MatchCapture under the payment's own reference = %v, want acceptance", err)
	}
}

// TestMatchCaptureRefusesAReferenceThisBuildWillNotCarry checks the reference
// is bounded before it is used as a lookup key or digested into one.
func TestMatchCaptureRefusesAReferenceThisBuildWillNotCarry(t *testing.T) {
	intent := mustNewIntent(t, nil)
	intent.Status = StatusCheckoutOpen
	amount := int64(1000)

	if _, err := intent.MatchCapture("", &amount, "USD"); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("MatchCapture with no reference = %v, want ErrInvalidReference", err)
	}
	if _, err := intent.MatchCapture(refOfLength(maxProviderReferenceLength+1), &amount, "USD"); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("MatchCapture with an oversized reference = %v, want ErrInvalidReference", err)
	}
}

// TestRecordRefundRefusesBeforeAnyCaptureWasRecorded pins the sentinel that
// wants the opposite handling from a refused transition: a refund ahead of its
// capture is a sequence the provider is entitled to produce, so it is recorded
// and waited on rather than quarantined as a contradiction.
func TestRecordRefundRefusesBeforeAnyCaptureWasRecorded(t *testing.T) {
	intent := mustNewIntent(t, nil)
	intent.Status = StatusCheckoutOpen
	amount := int64(500)

	_, err := intent.RecordRefund("re_1", &amount, "USD")
	if !errors.Is(err, ErrRefundAheadOfCapture) {
		t.Fatalf("RecordRefund before a capture = %v, want ErrRefundAheadOfCapture", err)
	}
	if errors.Is(err, ErrInvalidTransition) {
		t.Errorf("a refund ahead of its capture shares the contradiction sentinel: %v", err)
	}
}

// TestRecordRefundMovesAPartialRefundToPartiallyRefunded checks the part of the
// refund path that works: a partial refund is recognised, its own reference
// keys it, and the projection the caller writes is the one the ceiling
// arithmetic just performed.
func TestRecordRefundMovesAPartialRefundToPartiallyRefunded(t *testing.T) {
	intent := intentAt(t, StatusSucceeded)
	amount := int64(300)

	record, err := intent.RecordRefund("re_1", &amount, "USD")
	if err != nil {
		t.Fatalf("RecordRefund(300 of 1000): %v", err)
	}
	if record.Status != StatusPartiallyRefunded {
		t.Errorf("status = %q, want %q", record.Status, StatusPartiallyRefunded)
	}
	if record.TotalRefunded != 300 || record.AmountMinorUnits != 300 {
		t.Errorf("record = (%d, %d), want the 300 refunded of a 300 total", record.AmountMinorUnits, record.TotalRefunded)
	}
	if record.RefundRef != "re_1" {
		t.Errorf("RefundRef = %q, want the provider's own", record.RefundRef)
	}
	wantKey, err := RefundCommandKey("stripe", "re_1", record.TotalRefunded)
	if err != nil {
		t.Fatalf("refund key: %v", err)
	}
	if record.CommandKey != wantKey {
		t.Errorf("command key = %q, want the derived %q", record.CommandKey, wantKey)
	}
	if !strings.Contains(record.CommandKey, ":refund:") {
		t.Errorf("a refund leg's key is not in the refund namespace: %q", record.CommandKey)
	}

	// The same refunded state reached again derives the key it already had.
	// This is the convergence a redelivery needs: the ledger sees a leg it has
	// already booked rather than a second correction for one refund. The intent
	// is unchanged between the two calls — nothing about the payment moved, and
	// nothing about the key should.
	again, err := intent.RecordRefund("re_1", &amount, "USD")
	if err != nil {
		t.Fatalf("RecordRefund of the same refunded state: %v", err)
	}
	if again.CommandKey != record.CommandKey {
		t.Errorf("one refunded state derived two keys: %q and %q", record.CommandKey, again.CommandKey)
	}

	// And a refund that leaves the payment at a DIFFERENT total derives a
	// different key, which is what makes a second partial refund expressible at
	// all. The payment has to be moved on first: the increment is added to the
	// total it already stands at, so the same increment on an unmoved intent is
	// the same refunded state.
	more := intentAt(t, StatusPartiallyRefunded)
	more.RefundedMinorUnits = record.TotalRefunded
	second := int64(200)
	other, err := more.RecordRefund("re_2", &second, "USD")
	if err != nil {
		t.Fatalf("RecordRefund of a second partial refund: %v", err)
	}
	if other.CommandKey == record.CommandKey {
		t.Errorf("a payment refunded 300 and then 500 share the key %q", record.CommandKey)
	}
}

// TestRecordRefundAcceptsARefundThatReachesTheCaptureExactly pins the case that
// gives the refunded status its only entrance.
//
// It is one test rather than two because the two arrivals are one arithmetic
// fact — a refund that reaches the capture exactly — and the defect this
// replaces was visible only when both were written down. RecordRefund used to
// learn the remaining room by PROBING CanRefund for one more unit, and the one
// case where the room is zero is exactly this one: the probe could not fit, its
// refusal was read as an over-refund, and a legitimately refunded customer was
// quarantined as a ceiling breach. The room is computed now, so a refund that
// lands on the ceiling is accepted, moves the payment to `refunded`, and leaves
// nothing further refundable.
func TestRecordRefundAcceptsARefundThatReachesTheCaptureExactly(t *testing.T) {
	// A full refund in one go.
	intent := intentAt(t, StatusSucceeded)
	whole := int64(1000)
	record, err := intent.RecordRefund("re_1", &whole, "USD")
	if err != nil {
		t.Fatalf("RecordRefund(1000 of a 1000 capture) = %v, want it accepted: a full refund is the ordinary case, not a ceiling breach", err)
	}
	if record.Status != StatusRefunded {
		t.Errorf("status = %q, want %q: a total that reaches the capture is the whole of the refunded edge", record.Status, StatusRefunded)
	}
	if record.TotalRefunded != 1000 || record.RemainingRefundable != 0 {
		t.Errorf("record = (%d total, %d remaining), want the 1000 refunded of a 1000 capture and nothing further", record.TotalRefunded, record.RemainingRefundable)
	}

	// A partial refund topped up to exactly the capture. The second call is a
	// small increment against a large remaining room, so a probe-based
	// implementation gets further here before refusing — which is what makes
	// this half worth its own lines rather than trusting the first.
	partial := intentAt(t, StatusSucceeded)
	first := int64(300)
	partialRecord, err := partial.RecordRefund("re_1", &first, "USD")
	if err != nil {
		t.Fatalf("the first partial refund was refused: %v", err)
	}
	partial.Status = partialRecord.Status
	partial.RefundedMinorUnits = partialRecord.TotalRefunded

	remainder := int64(700)
	topUp, err := partial.RecordRefund("re_2", &remainder, "USD")
	if err != nil {
		t.Fatalf("RecordRefund(700 of the remaining 700) = %v, want it accepted", err)
	}
	if topUp.Status != StatusRefunded || topUp.TotalRefunded != 1000 || topUp.RemainingRefundable != 0 {
		t.Errorf("record = (%q, %d total, %d remaining), want the payment refunded in full", topUp.Status, topUp.TotalRefunded, topUp.RemainingRefundable)
	}
	// The two arrivals are two refunded states and two keys: a partial refund
	// and the completion of it are not the same claim, so the second leg must
	// not be swallowed as a redelivery of the first.
	if topUp.CommandKey == partialRecord.CommandKey {
		t.Errorf("a payment refunded 300 and one refunded 1000 share the key %q", partialRecord.CommandKey)
	}
}

// TestRecordRefundReportsTheRealRemainingRoom pins the figure the probe used to
// report instead of the room.
//
// The field is documented as what may still be refunded of this capture, and it
// was the literal probe constant for every successful refund — a caller reading
// it to decide whether a further refund fits was answered "one minor unit" no
// matter the payment. The room is now computed from the same ceiling the check
// above used, so the figure a booking site reads and the figure the ceiling
// arithmetic enforced cannot disagree.
func TestRecordRefundReportsTheRealRemainingRoom(t *testing.T) {
	intent := intentAt(t, StatusSucceeded)
	amount := int64(300)

	record, err := intent.RecordRefund("re_1", &amount, "USD")
	if err != nil {
		t.Fatalf("RecordRefund(300 of 1000): %v", err)
	}
	if record.RemainingRefundable != 700 {
		t.Errorf("RemainingRefundable = %d, want the 700 still refundable of a 1000 capture", record.RemainingRefundable)
	}
	// The figure is the room, and the room is the ceiling less the total: a
	// caller that subtracted for itself must arrive at the same number.
	if record.RemainingRefundable != 1000-record.TotalRefunded {
		t.Errorf("RemainingRefundable = %d against a total of %d; the two disagree about one capture", record.RemainingRefundable, record.TotalRefunded)
	}
}

// TestRecordRefundRefusesAnAmountThatBreaksTheCeiling checks the ceiling is a
// refusal and not a clamp, and that the refusal carries by how much so an
// operator has a question they can answer.
func TestRecordRefundRefusesAnAmountThatBreaksTheCeiling(t *testing.T) {
	intent := intentAt(t, StatusSucceeded)
	intent.RefundedMinorUnits = 900
	over := int64(200)

	_, err := intent.RecordRefund("re_2", &over, "USD")
	if !errors.Is(err, ErrRefundCeiling) {
		t.Fatalf("RecordRefund(200 of the remaining 100) = %v, want ErrRefundCeiling", err)
	}
	if errors.Is(err, ErrRefundAheadOfCapture) || errors.Is(err, ErrInvalidTransition) {
		t.Errorf("a ceiling refusal shares a sentinel with a different refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "100") {
		t.Errorf("the refusal said %v, want it to name the 100 still refundable", err)
	}

	// One unit over, not only a large amount over.
	one := int64(101)
	if _, err := intent.RecordRefund("re_2", &one, "USD"); !errors.Is(err, ErrRefundCeiling) {
		t.Errorf("RecordRefund(101 of the remaining 100) = %v, want ErrRefundCeiling", err)
	}
}

// TestRecordRefundRefusesANilAmount checks the port's pointer again: a refund
// that states no amount is not a refund of zero.
func TestRecordRefundRefusesANilAmount(t *testing.T) {
	intent := intentAt(t, StatusSucceeded)

	_, err := intent.RecordRefund("re_1", nil, "USD")
	if !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("RecordRefund(nil) = %v, want ErrInvalidReference", err)
	}
	if !strings.Contains(err.Error(), "states none") {
		t.Errorf("RecordRefund(nil) said %v, want it to say no amount was stated", err)
	}
}

// TestRecordRefundRefusesARefundOnAFullyRefundedPayment pins this layer's
// backstop, and it is worth being exact about what it is a backstop FOR.
//
// A payment already refunded in full has nothing left to give: any increment is
// over the ceiling and CanRefund refuses it, and an increment of zero is not a
// refund at all. So no input reaches this method's success path from a refunded
// payment, and the guard refuses the attempt rather than letting the arithmetic
// answer a question with no legal answer. `refunded → refunded` stays declared
// in legalEdges because a terminal status may be reported again — the table says
// the payment may still be described as refunded, not that a further refund may
// be booked against it.
//
// The case the table's comment is about — a full refund delivered again under a
// fresh event id, which is the ordinary shape of a provider redelivery — is
// converged one layer up and never arrives here: a delivery reporting a
// cumulative total this platform has already recognised is answered as applied
// without a refund being recorded at all. See the application's refund tests.
// What this test holds is the other direction: a delivery that somehow claims
// MORE than the payment has left is refused, and refused distinguishably, so an
// operator gets a contradiction rather than a silent no-op.
func TestRecordRefundRefusesARefundOnAFullyRefundedPayment(t *testing.T) {
	intent := intentAt(t, StatusRefunded)
	intent.RefundedMinorUnits = 1000
	amount := int64(1000)

	_, err := intent.RecordRefund("re_2", &amount, "USD")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("RecordRefund on a fully refunded payment = %v, want ErrInvalidTransition", err)
	}
	// The two facts that make the guard the whole of the answer: there is no
	// legal edge back to a partially refunded state, and the ceiling has no room
	// in it either. If either flipped, the refusal above would be the wrong
	// refusal and this test would be pinning the wrong behaviour.
	if canMove(StatusRefunded, StatusPartiallyRefunded) {
		t.Fatalf("refunded → partially_refunded is legal after all; this test is pinning the wrong refusal")
	}
	if room, err := RefundableRemaining(1000, 1000); err != nil || room != 0 {
		t.Fatalf("RefundableRemaining(1000, 1000) = (%d, %v), want no room: a further refund has no ceiling to fit under", room, err)
	}
}

// TestRecordRefundRefusesAReferenceThisBuildWillNotCarry checks the refund's
// own identity is bounded, like the payment's.
func TestRecordRefundRefusesAReferenceThisBuildWillNotCarry(t *testing.T) {
	intent := intentAt(t, StatusSucceeded)
	amount := int64(100)

	if _, err := intent.RecordRefund("", &amount, "USD"); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("RecordRefund with no reference = %v, want ErrInvalidReference", err)
	}
	if _, err := intent.RecordRefund(refOfLength(maxProviderReferenceLength+1), &amount, "USD"); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("RecordRefund with an oversized reference = %v, want ErrInvalidReference", err)
	}
}

// TestSettleExpiredMovesAPastDeadlineCheckoutToExpired checks the sweep's
// ordinary case, and that the second return is a real answer: the caller is
// told the payment moved rather than left to infer it from a status.
func TestSettleExpiredMovesAPastDeadlineCheckoutToExpired(t *testing.T) {
	intent := intentAt(t, StatusCheckoutOpen)
	now := intent.ExpiresAt.Add(time.Minute)

	next, moved, err := intent.SettleExpired(now)
	if err != nil {
		t.Fatalf("SettleExpired: %v", err)
	}
	if !moved {
		t.Fatalf("SettleExpired reported no move after the deadline")
	}
	if next.Status != StatusExpired {
		t.Errorf("status = %q, want %q", next.Status, StatusExpired)
	}
	if !next.UpdatedAt.Equal(now) {
		t.Errorf("UpdatedAt = %s, want %s", next.UpdatedAt, now)
	}
	// The deadline is a local fact: the expiry does not touch the money.
	if next.AmountMinorUnits != intent.AmountMinorUnits || next.ProviderCheckoutRef != intent.ProviderCheckoutRef {
		t.Errorf("expiry changed something other than the status")
	}
}

// TestSettleExpiredLeavesAWaitingCheckoutAloneBeforeItsDeadline checks the
// answer distinguishes "not yet" from "I moved it": the zero payment is
// returned with no error, and a caller that ignored the second return would
// write an empty row rather than a moved one.
func TestSettleExpiredLeavesAWaitingCheckoutAloneBeforeItsDeadline(t *testing.T) {
	intent := intentAt(t, StatusCheckoutOpen)

	next, moved, err := intent.SettleExpired(intent.ExpiresAt.Add(-time.Nanosecond))
	if err != nil {
		t.Fatalf("SettleExpired before the deadline: %v", err)
	}
	if moved {
		t.Fatalf("SettleExpired before the deadline reported a move")
	}
	if next.ID != "" {
		t.Errorf("an unmoved sweep returned a payment to write: %+v", next)
	}

	// Exactly on the deadline is expired: the bound is inclusive.
	if _, moved, err := intent.SettleExpired(intent.ExpiresAt); err != nil || !moved {
		t.Errorf("SettleExpired exactly on the deadline = (%v, %v), want a move", moved, err)
	}
}

// TestSettleExpiredLeavesEveryFinishedPaymentAlone walks the states a sweep
// must not touch. A payment that has succeeded is not expired by a local timer,
// and the ones with no money left to move are not swept either.
func TestSettleExpiredLeavesEveryFinishedPaymentAlone(t *testing.T) {
	for _, status := range []Status{
		StatusCreated, StatusSucceeded, StatusFailed, StatusCancelled,
		StatusPartiallyRefunded, StatusRefunded, StatusQuarantined,
	} {
		t.Run(string(status), func(t *testing.T) {
			intent := intentAt(t, status)
			now := intent.ExpiresAt.Add(24 * time.Hour)

			next, moved, err := intent.SettleExpired(now)
			if err != nil {
				t.Fatalf("SettleExpired at %q: %v", status, err)
			}
			if moved {
				t.Errorf("SettleExpired swept a payment at %q", status)
			}
			if next.ID != "" {
				t.Errorf("an unmoved sweep returned a payment to write: %+v", next)
			}
		})
	}
}

// TestSettleExpiredRefusesARequiresActionPayment documents a disagreement
// inside SettleExpired itself.
//
// FINDING: the function's comment names the states it moves from as "the two
// that are still WAITING — a checkout the customer has been sent to but not
// completed, and one the provider says needs something from them", and its
// implementation lists both in `waited`. But legalEdges has no
// requires_action → expired edge, and the loop returns ErrInvalidTransition
// when it finds that disagreement — so a requires_action payment is refused
// BEFORE the deadline is even consulted: a sweep calling this on a customer
// mid-3-DS gets an error rather than "not yet", and a lane that treats an error
// as a stop condition stalls on an ordinary state. The inline comment calls the
// disagreement "a defect in this file", so I believe the wait list (or the
// table) is what is wrong; I did not change either.
func TestSettleExpiredRefusesARequiresActionPayment(t *testing.T) {
	intent := intentAt(t, StatusRequiresAction)

	// Before the deadline, where every other waitable state answers "not yet".
	next, moved, err := intent.SettleExpired(intent.ExpiresAt.Add(-time.Hour))
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("SettleExpired on a requires_action payment before its deadline = %v, want ErrInvalidTransition", err)
	}
	if moved || next.ID != "" {
		t.Errorf("a refused sweep returned a payment: moved %v, payment %+v", moved, next)
	}

	// And after it, where a checkout_open payment is swept.
	if _, _, err := intent.SettleExpired(intent.ExpiresAt.Add(24 * time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("SettleExpired on a requires_action payment after its deadline = %v, want ErrInvalidTransition", err)
	}
}

// TestNewIntentMintsAVersionSevenIdentifierThatOrdersByTime checks the id
// shape the console's keyset and the reconciliation's pass both order by: the
// version and variant bits, the 48 bits of milliseconds at the front, and the
// fact that two ids minted apart sort apart.
func TestNewIntentMintsAVersionSevenIdentifierThatOrdersByTime(t *testing.T) {
	first, err := NewIntent(testNow)
	if err != nil {
		t.Fatalf("NewIntent: %v", err)
	}
	if len(first) != 36 {
		t.Fatalf("id %q is %d characters, want 36", first, len(first))
	}
	for _, at := range []int{8, 13, 18, 23} {
		if first[at] != '-' {
			t.Errorf("id %q has %q at %d, want a hyphen", first, first[at], at)
		}
	}
	if first[14] != '7' {
		t.Errorf("id %q is version %q, want 7", first, first[14])
	}
	if variant := first[19]; variant != '8' && variant != '9' && variant != 'a' && variant != 'b' {
		t.Errorf("id %q has variant %q, want an RFC 4122 variant", first, variant)
	}

	// The 48 bits of milliseconds are the first six bytes — the id's first
	// twelve hex digits, with the group separator at index 8 — so the id
	// carries the clock it was minted from.
	prefix, err := hex.DecodeString(strings.ReplaceAll(string(first)[0:13], "-", ""))
	if err != nil {
		t.Fatalf("id %q does not begin with hex: %v", first, err)
	}
	if len(prefix) != 6 {
		t.Fatalf("id %q begins with %d bytes of timestamp, want 6", first, len(prefix))
	}
	var millis uint64
	for _, b := range prefix {
		millis = millis<<8 | uint64(b)
	}
	if millis != uint64(testNow.UnixMilli()) {
		t.Errorf("the id's timestamp is %d, want the mint clock's %d", millis, testNow.UnixMilli())
	}

	// Two ids minted in order sort in order, and no two are the same.
	later, err := NewIntent(testNow.Add(time.Millisecond))
	if err != nil {
		t.Fatalf("NewIntent: %v", err)
	}
	if first == later {
		t.Fatalf("two mints produced one id: %q", first)
	}
	if string(first) >= string(later) {
		t.Errorf("ids do not order by mint time: %q then %q", first, later)
	}
	again, err := NewIntent(testNow)
	if err != nil {
		t.Fatalf("NewIntent: %v", err)
	}
	if again == first {
		t.Errorf("two mints at one instant produced one id: %q", again)
	}
}

// TestValidCurrencyCodeChecksTheShapeNotAList pins the guard New runs after
// folding: three uppercase ASCII letters, and nothing else. A list of
// currencies would go stale; a shape cannot.
func TestValidCurrencyCodeChecksTheShapeNotAList(t *testing.T) {
	valid := []string{"USD", "EUR", "JPY", "CLF", "XTS", "AAA", "ZZZ"}
	for _, code := range valid {
		if !validCurrencyCode(code) {
			t.Errorf("validCurrencyCode(%q) = false, want true", code)
		}
	}
	invalid := []string{"", "U", "US", "USDD", "usd", "Usd", "US1", "1US", "US ", " US", "ÜSD", "US-"}
	for _, code := range invalid {
		if validCurrencyCode(code) {
			t.Errorf("validCurrencyCode(%q) = true, want false", code)
		}
	}
}

// TestValidMinorUnitExponentBoundsPrecisionAtZeroToFour pins the bound that is
// a bound on PRECISION rather than a list of currencies: JPY's whole yen and
// CLF's four places are both minor units, and 1000 is a thousand yen or ten
// dollars depending on this number.
func TestValidMinorUnitExponentBoundsPrecisionAtZeroToFour(t *testing.T) {
	for _, exponent := range []int{0, 1, 2, 3, 4} {
		if !validMinorUnitExponent(exponent) {
			t.Errorf("validMinorUnitExponent(%d) = false, want true", exponent)
		}
	}
	for _, exponent := range []int{-1, -100, 5, 6, 100} {
		if validMinorUnitExponent(exponent) {
			t.Errorf("validMinorUnitExponent(%d) = true, want false", exponent)
		}
	}
}
