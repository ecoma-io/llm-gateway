package application

import (
	"errors"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/payments"
	paymentprovider "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/payments"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The payment use cases (B15): the customer-facing top-up that opens a payment,
// and the provider-facing webhook that finishes one.
//
// Three properties are what this file is for, and everything else is a refusal
// or a boundary check around them.
//
// The first is that the PROVIDER IS CALLED OUTSIDE ANY UNIT OF WORK. A
// transaction held open across a third party's latency pins a connection and a
// row lock for as long as the provider takes to answer.
//
// The second is that a delivery, its status move and its credit are ONE unit of
// work, with the credit reached through the TRANSACTION's context and not the
// caller's. Hand the caller's context to TopUp and the two writes commit
// independently, no error is raised anywhere, and the provider is told 200.
//
// The third is that a provider's payload chooses no money. Every reference a
// delivery names is matched against a column this platform wrote, and a
// delivery that names none of them is recorded as a question for an operator
// rather than resolved by looking harder.

// applicationCodeOf unwraps an application failure so a test can name the
// category the transport will render, rather than matching on prose.
func applicationCodeOf(t *testing.T, err error) Code {
	t.Helper()
	if err == nil {
		t.Fatalf("the call succeeded, want a typed refusal")
	}
	applicationError, ok := As(err)
	if !ok {
		t.Fatalf("the refusal %v is not an application error, so the transport cannot classify it", err)
	}
	return applicationError.Code
}

// paymentsPorts is the constructor's argument list as a value, so the
// nil-port test can remove one member at a time without eleven near-identical
// test bodies.
type paymentsPorts struct {
	store      persistence.Store
	intents    persistence.PaymentIntents
	events     persistence.PaymentEvents
	quarantine persistence.PaymentQuarantine
	clock      persistence.Clock
	accounts   persistence.Accounts
	buckets    bucketReader
	funding    accountFunding
	ledger     topUpWriter
	provider   paymentprovider.Transfers
	offers     TopUpCatalogue
	settings   PaymentsSettings
}

func (ports paymentsPorts) build() *Payments {
	return NewPayments(ports.store, ports.intents, ports.events, ports.quarantine,
		ports.clock, ports.accounts, ports.buckets, ports.funding, ports.ledger,
		ports.provider, ports.offers, ports.settings)
}

func newPaymentsPorts(w *paymentsWorld) paymentsPorts {
	return paymentsPorts{
		store:      fakePaymentsStore{world: w},
		intents:    fakePaymentIntents{world: w},
		events:     fakePaymentEvents{world: w},
		quarantine: fakePaymentQuarantine{world: w},
		clock:      fakePaymentsClock{world: w},
		accounts:   fakePaymentsAccounts{world: w},
		buckets:    fakeBucketReader{world: w},
		funding:    fakeAccountFunding{world: w},
		ledger:     fakeTopUpWriter{world: w},
		provider:   fakeTransferProvider{world: w},
		offers:     NewTopUpCatalogue(w.offers),
		settings:   w.settings,
	}
}

// TestNewPaymentsRefusesAnyMissingPort pins the wiring rule: a port this use
// case was promised and did not get is a defect to learn about at construction
// rather than in the middle of a webhook, after a delivery is recorded and
// before the credit lands.
func TestNewPaymentsRefusesAnyMissingPort(t *testing.T) {
	cases := []struct {
		name   string
		remove func(*paymentsPorts)
		names  string
	}{
		{"the store", func(p *paymentsPorts) { p.store = nil }, "the store"},
		{"the payment intents repository", func(p *paymentsPorts) { p.intents = nil }, "the payment intents repository"},
		{"the payment events repository", func(p *paymentsPorts) { p.events = nil }, "the payment events repository"},
		{"the payment quarantine repository", func(p *paymentsPorts) { p.quarantine = nil }, "the payment quarantine repository"},
		{"the database clock", func(p *paymentsPorts) { p.clock = nil }, "the database clock"},
		{"the accounts repository", func(p *paymentsPorts) { p.accounts = nil }, "the accounts repository"},
		{"the bucket reader", func(p *paymentsPorts) { p.buckets = nil }, "the bucket reader"},
		{"the account funding use case", func(p *paymentsPorts) { p.funding = nil }, "the account funding use case"},
		{"the accounting top-up primitive", func(p *paymentsPorts) { p.ledger = nil }, "the accounting top-up primitive"},
		{"the payment provider", func(p *paymentsPorts) { p.provider = nil }, "the payment provider"},
		{"the configured provider", func(p *paymentsPorts) { p.settings.Provider = "" }, "the provider this deployment talks to"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newPaymentsWorld(t)
			ports := newPaymentsPorts(w)
			tc.remove(&ports)
			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatalf("NewPayments accepted a missing %s", tc.name)
				}
				if !strings.Contains(recovered.(string), tc.names) {
					t.Errorf("the panic says %q, want it to name %q", recovered, tc.names)
				}
			}()
			ports.build()
		})
	}
}

// TestAnEmptyCatalogueIsADeploymentRatherThanAWiringDefect documents the one
// absence the constructor's checks are deliberately missing. A deployment that
// sells no top-up offers is a real deployment — a self-hosted install whose
// operator funds accounts by hand — and its console must still load: the offers
// read answers an empty list and every path that could open a payment refuses
// against the empty catalogue the way it refuses an unknown offer id. A panic
// here would turn a legitimate configuration into a process that will not
// start.
func TestAnEmptyCatalogueIsADeploymentRatherThanAWiringDefect(t *testing.T) {
	w := newPaymentsWorld(t)
	w.offers = nil
	p := newPayments(w) // must not panic

	offers := p.Offers()
	if offers == nil {
		t.Errorf("Offers() = nil, want an empty list: a deployment that sells nothing still has a catalogue")
	}
	if len(offers) != 0 {
		t.Fatalf("Offers() = %v, want nothing", offers)
	}

	_, err := p.BeginTransfer(t.Context(), BeginTransferRequest{
		AccountID:      paymentsAccountID,
		OfferID:        "starter",
		IdempotencyKey: "key-1",
	})
	if code := applicationCodeOf(t, err); code != CodeInvalidRequest {
		t.Errorf("an empty catalogue answered %q, want %q", code, CodeInvalidRequest)
	}
	if len(w.intents) != 0 || len(w.providerCalls) != 0 {
		t.Errorf("a refused top-up wrote something: %d payments, %d provider calls", len(w.intents), len(w.providerCalls))
	}
}

// TestBeginTransferRefusesBeforeItWritesAnything covers the four gates that run
// ahead of any write. Each refusal is a different category because each calls
// for different client behaviour: an offer this deployment does not sell is the
// client's mistake and the client's to fix, an account that may not fund itself
// is a server state the client cannot see and cannot repair by editing its
// request.
func TestBeginTransferRefusesBeforeItWritesAnything(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*paymentsWorld)
		in    BeginTransferRequest
		want  Code
	}{
		{
			name: "an offer this deployment does not sell",
			in:   BeginTransferRequest{AccountID: paymentsAccountID, OfferID: "platinum", IdempotencyKey: "key-1"},
			want: CodeInvalidRequest,
		},
		{
			name: "no idempotency key",
			in:   BeginTransferRequest{AccountID: paymentsAccountID, OfferID: "starter"},
			want: CodeInvalidRequest,
		},
		{
			// The key is the only caller-chosen bytes that reach the domain's
			// constructor. If this gate did not exist the domain would still
			// refuse the key — and the transport would render that refusal as a
			// 500, because the domain's sentinel is not one of this layer's own
			// error types. A customer holding a key one character too long is a
			// 400 with a sentence about the key, not a page for an operator.
			name: "an idempotency key past what the schema carries",
			in:   BeginTransferRequest{AccountID: paymentsAccountID, OfferID: "starter", IdempotencyKey: strings.Repeat("k", 129)},
			want: CodeInvalidRequest,
		},
		{
			name: "no account at all",
			in:   BeginTransferRequest{OfferID: "starter", IdempotencyKey: "key-1"},
			want: CodeUnauthenticated,
		},
		{
			name: "an account this platform does not have",
			in:   BeginTransferRequest{AccountID: "99999999-9999-4999-8999-999999999999", OfferID: "starter", IdempotencyKey: "key-1"},
			want: CodeUnauthenticated,
		},
		{
			name:  "a suspended account",
			setup: func(w *paymentsWorld) { w.seedAccount(paymentsAccountID, identity.AccountSuspended) },
			in:    BeginTransferRequest{AccountID: paymentsAccountID, OfferID: "starter", IdempotencyKey: "key-1"},
			want:  CodeConflict,
		},
		{
			name:  "a closed account",
			setup: func(w *paymentsWorld) { w.seedAccount(paymentsAccountID, identity.AccountClosed) },
			in:    BeginTransferRequest{AccountID: paymentsAccountID, OfferID: "starter", IdempotencyKey: "key-1"},
			want:  CodeConflict,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newPaymentsWorld(t)
			if tc.setup != nil {
				tc.setup(w)
			}
			p := newPayments(w)

			_, err := p.BeginTransfer(t.Context(), tc.in)
			if code := applicationCodeOf(t, err); code != tc.want {
				t.Errorf("BeginTransfer(%s) answered %q, want %q", tc.name, code, tc.want)
			}
			if len(w.intents) != 0 {
				t.Errorf("a refused top-up opened %d payments", len(w.intents))
			}
			if len(w.providerCalls) != 0 {
				t.Errorf("a refused top-up reached the provider %d times", len(w.providerCalls))
			}
		})
	}
}

// TestABeginTransferAtTheKeyBoundIsCarriedNotRefused is the other side of the
// gate above, and it is a test of the BOUND rather than of the gate: a check
// written by hand at the call site is a second statement of a number the domain
// already holds, and the failure mode of getting it wrong is a customer whose
// perfectly legal 128-character key is refused by an application that is
// stricter than the schema it is writing to. The application asks the domain's
// own function, so this test would fail if the two ever disagreed.
func TestABeginTransferAtTheKeyBoundIsCarriedNotRefused(t *testing.T) {
	w := newPaymentsWorld(t)
	p := newPayments(w)

	if _, err := p.BeginTransfer(t.Context(), BeginTransferRequest{
		AccountID:      paymentsAccountID,
		OfferID:        "starter",
		IdempotencyKey: strings.Repeat("k", 128),
	}); err != nil {
		t.Fatalf("BeginTransfer() with a key at the schema's own width answered %v, want it carried", err)
	}
	if len(w.intents) != 1 {
		t.Fatalf("the top-up opened %d payments, want one", len(w.intents))
	}
	for _, stored := range w.intents {
		if got := len(stored.IdempotencyKey); got != 128 {
			t.Errorf("the stored key is %d characters, want all 128 — a refusal at the bound would lose the last one", got)
		}
	}
}

// TestBeginTransferRecordsThePaymentBeforeItCallsTheProvider pins the ordering
// the provider's idempotency key depends on. The key is derived from the
// payment's own identifier, and a key derived from something that does not
// exist until the provider answers cannot make a retry the same request — a
// lost response would mean a second call under a new key and a customer handed
// a second destination for one payment. The intent is therefore durable, and
// committed, before the call.
func TestBeginTransferRecordsThePaymentBeforeItCallsTheProvider(t *testing.T) {
	w := newPaymentsWorld(t)
	p := newPayments(w)

	result, err := p.BeginTransfer(t.Context(), BeginTransferRequest{
		AccountID:      paymentsAccountID,
		OfferID:        "starter",
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("BeginTransfer: %v", err)
	}

	order := strings.Join(w.order, " ")
	commit := strings.Index(order, "commit")
	provider := strings.Index(order, "provider")
	if commit < 0 || provider < 0 {
		t.Fatalf("the run reached neither the commit nor the provider: %q", order)
	}
	if commit > provider {
		t.Errorf("the provider was called before the payment committed: %q", order)
	}
	if !strings.Contains(order[:provider], "create") {
		t.Errorf("the payment was not created before the provider was called: %q", order)
	}

	// The provider was handed the payment's own identity and the offer's own
	// price: nothing a browser sent reaches the provider as a figure. The key is
	// the one derived from the payment, and the lifetime is this platform's own
	// window — the same one the console prints for the customer.
	if len(w.providerCalls) != 1 {
		t.Fatalf("the provider was called %d times, want once", len(w.providerCalls))
	}
	call := w.providerCalls[0]
	if want := "transfer:" + string(result.Intent.ID); call.IdempotencyKey != want {
		t.Errorf("the provider was told the transfer is %q, want %q", call.IdempotencyKey, want)
	}
	if call.AmountMinorUnits != 1000 || call.Currency != "USD" {
		t.Errorf("the provider was asked for %d %s, want the offer's 1000 USD", call.AmountMinorUnits, call.Currency)
	}
	if call.ExpiresIn != transferTTL {
		t.Errorf("the provider was told the destination lives %s, want this platform's own %s", call.ExpiresIn, transferTTL)
	}

	// And the answer is durable: the destination a returning customer must be
	// shown again is on the row, not just in the response.
	stored := w.storedIntent(t, result.Intent.ID)
	if stored.ProviderTransferRef != w.providerInstructions.TransferCode {
		t.Errorf("the stored payment names destination %q, want %q", stored.ProviderTransferRef, w.providerInstructions.TransferCode)
	}
	if stored.ProviderQRURL != w.providerInstructions.QRURL {
		t.Errorf("the stored payment carries image %q, want %q", stored.ProviderQRURL, w.providerInstructions.QRURL)
	}
	if stored.ProviderBankName != w.providerInstructions.BankName || stored.ProviderAccountHolder != w.providerInstructions.AccountHolder {
		t.Errorf("the stored payment holds the destination at %q for %q, want %q and %q",
			stored.ProviderBankName, stored.ProviderAccountHolder, w.providerInstructions.BankName, w.providerInstructions.AccountHolder)
	}
	if stored.Status != payments.StatusAwaitingTransfer {
		t.Errorf("the stored payment is %q, want %q", stored.Status, payments.StatusAwaitingTransfer)
	}
}

// TestBeginTransferCallsTheProviderOutsideAnyUnitOfWork is the whole reason the
// top-up is written in two transactions rather than one. A transaction held
// open across the provider's call pins a connection and a row lock for as long
// as a third party takes to answer, and the port says the same thing from the
// other side: opening a transfer cannot move money, so it has no business
// inside the unit of work that does.
func TestBeginTransferCallsTheProviderOutsideAnyUnitOfWork(t *testing.T) {
	w := newPaymentsWorld(t)
	p := newPayments(w)

	if _, err := p.BeginTransfer(t.Context(), BeginTransferRequest{
		AccountID:      paymentsAccountID,
		OfferID:        "starter",
		IdempotencyKey: "key-1",
	}); err != nil {
		t.Fatalf("BeginTransfer: %v", err)
	}

	if len(w.providerDepth) != 1 {
		t.Fatalf("the provider was called %d times, want once", len(w.providerDepth))
	}
	if w.providerDepth[0] != 0 {
		t.Errorf("the provider was called %d units of work deep, want 0", w.providerDepth[0])
	}
	if w.providerInTx[0] {
		t.Errorf("the provider was handed a transaction's context")
	}
	// The two writes around it are inside units of work, so the depth of 0
	// above is the call being outside them rather than the store not existing.
	if w.beginUnits != 2 {
		t.Errorf("the top-up took %d units of work, want two: the payment, then its destination", w.beginUnits)
	}
}

// TestBeginTransferConvergesOnTheKey checks the retry the key exists for. A
// customer who clicks twice, or whose first response was lost, gets the ONE
// payment they meant rather than a second one they would have to choose
// between — and the provider is not asked for a second destination, because a
// customer shown two accounts for one payment has no way to know which to pay.
func TestBeginTransferConvergesOnTheKey(t *testing.T) {
	w := newPaymentsWorld(t)
	p := newPayments(w)
	request := BeginTransferRequest{AccountID: paymentsAccountID, OfferID: "starter", IdempotencyKey: "key-1"}

	first, err := p.BeginTransfer(t.Context(), request)
	if err != nil {
		t.Fatalf("the first top-up: %v", err)
	}
	if first.Converged {
		t.Errorf("the first call reports itself converged")
	}

	second, err := p.BeginTransfer(t.Context(), request)
	if err != nil {
		t.Fatalf("the second top-up: %v", err)
	}
	if !second.Converged {
		t.Errorf("the second call does not report convergence, so a client cannot know its retry was harmless")
	}
	if second.Intent.ID != first.Intent.ID {
		t.Errorf("the second call produced payment %s, want the first one, %s", second.Intent.ID, first.Intent.ID)
	}
	if second.Intent.ProviderTransferRef != first.Intent.ProviderTransferRef {
		t.Errorf("the second call returned destination %q, want the one the customer was already given, %q",
			second.Intent.ProviderTransferRef, first.Intent.ProviderTransferRef)
	}
	if len(w.intents) != 1 {
		t.Errorf("two calls under one key produced %d payments, want one", len(w.intents))
	}
	if len(w.providerCalls) != 1 {
		t.Errorf("the provider was asked for %d destinations, want one", len(w.providerCalls))
	}
}

// TestBeginTransferRetriesUnderTheSameProviderIdempotencyKey covers the window
// the whole design is shaped around: the payment is durable, the provider does
// not answer, and the payment stands in `created` with no destination. The next
// call with the same key converges on that payment and asks the provider again
// — under the SAME key, which is what makes the provider treat the two attempts
// as one request rather than issuing a second destination for one payment.
func TestBeginTransferRetriesUnderTheSameProviderIdempotencyKey(t *testing.T) {
	w := newPaymentsWorld(t)
	w.providerErr = errPaymentsProviderDown
	p := newPayments(w)
	request := BeginTransferRequest{AccountID: paymentsAccountID, OfferID: "starter", IdempotencyKey: "key-1"}

	_, err := p.BeginTransfer(t.Context(), request)
	if err == nil {
		t.Fatalf("a provider outage was reported as a successful top-up")
	}

	// Nothing was written as a failure state: the payment stands in its birth
	// state, because an intent that recorded "the provider was down" would be an
	// intent remembering a fact about a third party rather than about itself.
	intents := make([]payments.Intent, 0, len(w.intents))
	for _, intent := range w.intents {
		intents = append(intents, intent)
	}
	if len(intents) != 1 {
		t.Fatalf("the failed attempt left %d payments, want the one it recorded before calling", len(intents))
	}
	if intents[0].Status != payments.StatusCreated {
		t.Errorf("the payment left behind by a provider outage is %q, want %q", intents[0].Status, payments.StatusCreated)
	}
	if intents[0].ProviderTransferRef != "" {
		t.Errorf("a payment whose provider call failed carries destination %q", intents[0].ProviderTransferRef)
	}

	// The retry: a healthy provider, the same key, the same payment.
	w.providerErr = nil
	result, err := p.BeginTransfer(t.Context(), request)
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if !result.Converged {
		t.Errorf("the retry did not converge on the payment the first attempt recorded")
	}
	if result.Intent.ID != intents[0].ID {
		t.Fatalf("the retry opened %s, want the first attempt's payment %s", result.Intent.ID, intents[0].ID)
	}
	if len(w.providerCalls) != 2 {
		t.Fatalf("the provider was called %d times, want the first attempt and its retry", len(w.providerCalls))
	}
	firstKey := w.providerCalls[0].IdempotencyKey
	secondKey := w.providerCalls[1].IdempotencyKey
	if firstKey != secondKey {
		t.Errorf("the retry used key %q, want the first attempt's %q: a new key is a second charge", secondKey, firstKey)
	}
	if want := "transfer:" + string(result.Intent.ID); firstKey != want {
		t.Errorf("the provider's idempotency key is %q, want %q — derived from the payment, not minted per call", firstKey, want)
	}
	if len(w.intents) != 1 {
		t.Errorf("the retry left %d payments, want one", len(w.intents))
	}
	if result.Intent.Status != payments.StatusAwaitingTransfer {
		t.Errorf("the retry left the payment %q, want %q", result.Intent.Status, payments.StatusAwaitingTransfer)
	}
}

// TestBeginTransferKeepsTheWinnersDestinationWhenTheSwapLoses covers the race
// the second transaction exists for. Two attempts can both reach the provider —
// that is what the idempotency key is for — but only one of them may write the
// account the customer will be told to pay into. Choosing between two accounts
// the provider issued for one payment is not a decision this layer can make, so
// the loser writes nothing and returns the winner's row.
func TestBeginTransferKeepsTheWinnersDestinationWhenTheSwapLoses(t *testing.T) {
	w := newPaymentsWorld(t)
	var losing payments.IntentID
	w.onProviderCall = func(int) {
		// Another attempt with the same key commits its own destination while
		// this call is inside the provider's latency — the window between the
		// provider's answer and the destination being recorded.
		id := w.intentByKey[paymentsKey(paymentsAccountID, "key-1")]
		winner := w.storedIntent(t, id)
		winner.ProviderTransferRef = "dest_winner"
		winner.ProviderBankName = "Techcombank"
		winner.ProviderAccountHolder = "WINNER CO"
		winner.ProviderQRURL = "https://qr.example/winner"
		winner.Status = payments.StatusAwaitingTransfer
		winner.StateVersion++
		w.put(winner)
		losing = id
	}
	p := newPayments(w)

	result, err := p.BeginTransfer(t.Context(), BeginTransferRequest{
		AccountID:      paymentsAccountID,
		OfferID:        "starter",
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("BeginTransfer: %v", err)
	}

	stored := w.storedIntent(t, losing)
	if stored.ProviderTransferRef != "dest_winner" {
		t.Errorf("the payment now names destination %q, want the winner's %q: the loser overwrote an account the customer may already have been given",
			stored.ProviderTransferRef, "dest_winner")
	}
	if stored.ProviderBankName != "Techcombank" || stored.ProviderAccountHolder != "WINNER CO" {
		t.Errorf("the payment now holds the destination at %q for %q, want the winner's", stored.ProviderBankName, stored.ProviderAccountHolder)
	}
	if result.Intent.ProviderTransferRef != "dest_winner" {
		t.Errorf("the caller was returned destination %q rather than the winner's: a customer shown two accounts for one payment cannot know which to pay",
			result.Intent.ProviderTransferRef)
	}
}

// TestBeginTransferSeparatesAProviderOutageFromARefusal pins the one place the
// two failure classes are told apart. The port says whether the provider failed
// to answer, and the difference is what a client does next: an outage is a 503
// whose correct answer is to retry later, while a provider that refused the
// request is a defect on one side or the other and must not invite a retry
// storm against a condition no retry can change.
func TestBeginTransferSeparatesAProviderOutageFromARefusal(t *testing.T) {
	request := BeginTransferRequest{AccountID: paymentsAccountID, OfferID: "starter", IdempotencyKey: "key-1"}

	t.Run("an unreachable provider is the retryable class", func(t *testing.T) {
		w := newPaymentsWorld(t)
		w.providerErr = paymentprovider.ErrProviderUnavailable
		p := newPayments(w)

		_, err := p.BeginTransfer(t.Context(), request)
		if code := applicationCodeOf(t, err); code != CodeUpstreamUnavailable {
			t.Errorf("an unreachable provider answered %q, want %q", code, CodeUpstreamUnavailable)
		}
		if !errors.Is(err, paymentprovider.ErrProviderUnavailable) {
			t.Errorf("the refusal does not wrap the port's own sentinel, so an operator cannot see the cause")
		}
	})

	t.Run("a refused request is not", func(t *testing.T) {
		w := newPaymentsWorld(t)
		w.providerErr = errPaymentsProviderRefused
		p := newPayments(w)

		_, err := p.BeginTransfer(t.Context(), request)
		if err == nil {
			t.Fatalf("a refused transfer was reported as a success")
		}
		if applicationError, ok := As(err); ok && applicationError.Code == CodeUpstreamUnavailable {
			t.Errorf("a refusal was reported as an outage, which asks the client to retry a condition that will not clear")
		}
		if !errors.Is(err, errPaymentsProviderRefused) {
			t.Errorf("the refusal does not carry the provider's own failure: %v", err)
		}
	})
}

// TestBeginTransferAbandonsThePaymentTheProviderWillNotReissue covers the one
// refusal that is neither an outage nor retryable. The provider already holds a
// destination under this payment's order identity, it did not return the
// destination to this process, and it documents no way to read one back — so
// there is an account somewhere that no customer was ever shown, and no retry
// will produce a second one.
//
// What the test pins is the LOCAL consequence: the payment must not stay in
// `created`. That is the state a retry converges on, and leaving the row there
// would go on implying that one more attempt is all this payment needs, when the
// provider refuses the identity identically and forever. The abandonment is a
// decision about a row in this platform's own table, made before the caller is
// told anything.
func TestBeginTransferAbandonsThePaymentTheProviderWillNotReissue(t *testing.T) {
	w := newPaymentsWorld(t)
	w.providerErr = paymentprovider.ErrOrderCodeTaken
	p := newPayments(w)
	request := BeginTransferRequest{AccountID: paymentsAccountID, OfferID: "starter", IdempotencyKey: "key-1"}

	_, err := p.BeginTransfer(t.Context(), request)
	if code := applicationCodeOf(t, err); code != CodeConflict {
		t.Fatalf("a transfer identity the provider would not reissue answered %q, want %q", code, CodeConflict)
	}
	// It is a conflict and not an outage. An outage invites the client to retry
	// later, and this condition is the one thing a retry cannot clear, so the
	// two must not be answered alike.
	if errors.Is(err, paymentprovider.ErrProviderUnavailable) {
		t.Errorf("the refusal was reported as the retryable class")
	}

	intents := make([]payments.Intent, 0, len(w.intents))
	for _, intent := range w.intents {
		intents = append(intents, intent)
	}
	if len(intents) != 1 {
		t.Fatalf("the refused attempt left %d payments, want the one it recorded before calling", len(intents))
	}
	abandoned := intents[0]
	if abandoned.Status != payments.StatusCancelled {
		t.Errorf("the payment is %q after the refusal, want %q: `created` is what a retry converges on, and this attempt can never be paid", abandoned.Status, payments.StatusCancelled)
	}
	if abandoned.ProviderTransferRef != "" {
		t.Errorf("the abandoned payment carries destination %q, though no customer was ever shown one", abandoned.ProviderTransferRef)
	}

	// The repeat: the SAME key, so it converges on the cancelled payment rather
	// than opening a second one, reaches the provider again under the same order
	// identity, is refused again, and must leave the number of payments where it
	// was. Minting a second payment under this key would derive a second order
	// identity at the provider, which is the one thing the key exists to prevent.
	_, err = p.BeginTransfer(t.Context(), request)
	if code := applicationCodeOf(t, err); code != CodeConflict {
		t.Fatalf("the repeat answered %q, want %q", code, CodeConflict)
	}
	if len(w.intents) != 1 {
		t.Errorf("the repeat left %d payments, want the one it converged on", len(w.intents))
	}
	if len(w.providerCalls) != 2 {
		t.Fatalf("the provider was called %d times, want the first attempt and its repeat", len(w.providerCalls))
	}
	if w.providerCalls[0].IdempotencyKey != w.providerCalls[1].IdempotencyKey {
		t.Errorf("the repeat used key %q, want the first attempt's %q: a second identity is a second order at the provider",
			w.providerCalls[1].IdempotencyKey, w.providerCalls[0].IdempotencyKey)
	}
}

// captureFixture is the world one capture test runs against: a payment whose
// customer has been handed a destination, an active account, an active bucket,
// and the delivery that funds it.
func captureFixture(t *testing.T, w *paymentsWorld) (payments.Intent, ProviderDelivery) {
	t.Helper()
	intent := w.seedIntent(t, nil)
	return intent, w.delivery("delivery_1", paymentprovider.KindCaptured, intent.ProviderTransferRef, "ref_1", 1000, "USD")
}

// TestApplyProviderEventAppliesACaptureAsOneUnitOfWork is the webhook's centre
// of gravity, and it asserts four things in one run because they are one
// behaviour: the delivery is recorded FIRST, the status CAS runs before the
// credit, the credit is reached through the transaction's context rather than
// the caller's, and all three commit together.
func TestApplyProviderEventAppliesACaptureAsOneUnitOfWork(t *testing.T) {
	w := newPaymentsWorld(t)
	intent, delivery := captureFixture(t, w)
	p := newPayments(w)

	outcome, err := p.ApplyProviderEvent(t.Context(), delivery)
	if err != nil {
		t.Fatalf("ApplyProviderEvent: %v", err)
	}
	if outcome.Disposition != payments.DispositionApplied {
		t.Fatalf("the capture was answered %q, want %q", outcome.Disposition, payments.DispositionApplied)
	}
	if outcome.IntentID != intent.ID {
		t.Errorf("the capture resolved to %s, want %s", outcome.IntentID, intent.ID)
	}

	// The order inside the unit of work is load-bearing, and it is the insert
	// that is first: a credit that committed before the insert would leave this
	// plane unable to say which delivery produced it, and an insert that
	// committed before the credit would swallow the credit's own redelivery.
	//
	// `settle-event` comes LAST and is in the expected order on purpose. The
	// insert writes the provisional disposition, so a delivery that credits a
	// customer and stops there leaves its row saying `recorded` — the one value
	// the schema says no reader may ever see. The settle is the last statement
	// of the unit of work, which is exactly where the real one is: a verdict
	// written before the effect it describes would survive a rollback and claim
	// an effect that never happened.
	wantOrder := []string{"begin", "record-event", "record-capture", "topup", "settle-event", "commit"}
	if got := strings.Join(w.order, " "); got != strings.Join(wantOrder, " ") {
		t.Errorf("the unit of work ran as %q, want %q", got, strings.Join(wantOrder, " "))
	}

	// And the row says what the outcome says. Before the settle was there, this
	// pair agreed by construction — the fake stored the caller's disposition
	// verbatim while the SQL adapter discarded it — so the application could
	// return `applied` over a row reading `recorded`, with no test able to see
	// it. The two answers are the same delivery, and one of them survives a
	// restart.
	stored, ok := w.events[paymentsEventKey(payments.ProviderEventRecord{
		EventID:            delivery.Event.EventID,
		ProviderAccountKey: newPayments(w).providerAccountKey(),
		Provider:           newPayments(w).providerName(),
	})]
	if !ok {
		t.Fatalf("the delivery left no row behind, so nothing records which delivery produced the credit")
	}
	if stored.Disposition != outcome.Disposition {
		t.Errorf("the stored row says %q while the outcome says %q: a reader reconciling this payment against its evidence would read the other one after a restart",
			stored.Disposition, outcome.Disposition)
	}

	// The credit took the TRANSACTION's context, not the one the call arrived
	// on. Handing it the caller's context commits the credit and the delivery
	// separately, with no error raised anywhere and the provider told 200.
	if len(w.creditInTx) != 1 {
		t.Fatalf("the credit was reached %d times, want once", len(w.creditInTx))
	}
	if !w.creditInTx[0] {
		t.Errorf("the credit was written with the caller's context: it would commit beside the delivery record rather than with it")
	}
	if w.creditDepths[0] != 1 {
		t.Errorf("the credit ran %d units of work deep, want 1: it did not join the delivery's transaction", w.creditDepths[0])
	}
	if w.outsideTx != 0 {
		t.Errorf("%d writes arrived outside a unit of work", w.outsideTx)
	}

	// The money landed, keyed on the payment rather than on the delivery, so a
	// re-notification of the same capture converges on the leg already on file.
	wantKey, err := payments.TopUpCommandKey(paymentsProviderName, "ref_1")
	if err != nil {
		t.Fatalf("deriving the payment's command key: %v", err)
	}
	legs := w.legsFor(accounting.FundingBucketID(intent.FundingBucketID))
	if len(legs) != 1 {
		t.Fatalf("the capture booked %d ledger legs, want one", len(legs))
	}
	if legs[0].CommandKey != accounting.CommandKey(wantKey) {
		t.Errorf("the leg is keyed %q, want %q — derived from the payment, not from the delivery", legs[0].CommandKey, wantKey)
	}
	if legs[0].Amount != accounting.Amount(1000) {
		t.Errorf("the leg credits %d minor units, want the payment's 1000", legs[0].Amount)
	}
	if strings.Contains(string(legs[0].CommandKey), delivery.Event.EventID) {
		t.Errorf("the leg's key contains the delivery's own id: two events for one capture would fund it twice")
	}
	bucket := w.storedBucket(t, accounting.FundingBucketID(intent.FundingBucketID))
	if bucket.Available != accounting.Balance(1000) {
		t.Errorf("the bucket holds %d minor units, want 1000", bucket.Available)
	}

	// The payment is succeeded and carries the provider's payment reference,
	// which is what a later refund resolves by and which the schema refuses to
	// let a succeeded row hold as NULL.
	after := w.storedIntent(t, intent.ID)
	if after.Status != payments.StatusSucceeded {
		t.Errorf("the payment is %q, want %q", after.Status, payments.StatusSucceeded)
	}
	if after.ProviderPaymentRef != "ref_1" {
		t.Errorf("the payment carries provider payment reference %q, want %q", after.ProviderPaymentRef, "ref_1")
	}

	// The delivery is on file with what it claimed and what was done with it.
	record, ok := w.eventRecord("delivery_1")
	if !ok {
		t.Fatalf("the delivery was not recorded")
	}
	if record.IntentID != intent.ID || record.ProviderPaymentRef != "ref_1" {
		t.Errorf("the recorded delivery names %s / %q, want %s / %q", record.IntentID, record.ProviderPaymentRef, intent.ID, "ref_1")
	}
	if record.Disposition != payments.DispositionApplied {
		t.Errorf("the recorded delivery says %q, want %q", record.Disposition, payments.DispositionApplied)
	}
	if len(w.quarantines) != 0 {
		t.Errorf("an applied capture left %d quarantine rows", len(w.quarantines))
	}
}

// TestApplyProviderEventAnswersARedeliveryAsADuplicate checks the answer a
// provider's retry gets. The dedup key is the delivery's own identity, and its
// insert — the FIRST write in the unit of work — is the arbiter: whichever
// transaction inserts first holds the key and the loser is told so by that key
// rather than by a lock. A duplicate funds nothing a second time.
func TestApplyProviderEventAnswersARedeliveryAsADuplicate(t *testing.T) {
	w := newPaymentsWorld(t)
	_, delivery := captureFixture(t, w)
	p := newPayments(w)

	if _, err := p.ApplyProviderEvent(t.Context(), delivery); err != nil {
		t.Fatalf("the first delivery: %v", err)
	}
	outcome, err := p.ApplyProviderEvent(t.Context(), delivery)
	if err != nil {
		t.Fatalf("the redelivery: %v", err)
	}
	if outcome.Disposition != payments.DispositionDuplicate {
		t.Fatalf("the redelivery was answered %q, want %q", outcome.Disposition, payments.DispositionDuplicate)
	}
	if len(w.legs) != 1 {
		t.Errorf("the redelivery booked %d ledger legs in total, want the one the first delivery wrote", len(w.legs))
	}
	if len(w.events) != 1 {
		t.Errorf("the delivery ledger holds %d rows, want one", len(w.events))
	}
	if len(w.quarantines) != 0 {
		t.Errorf("a duplicate left %d quarantine rows, want none: its effect is already durable", len(w.quarantines))
	}
}

// TestApplyProviderEventQuarantinesAnUnknownPaymentWithoutClaimingItsKey pins
// the one place the dedup key is deliberately left unclaimed.
//
// The obvious move — insert the event row anyway so a redelivery is a duplicate
// rather than a second quarantine — is wrong, because the dedup key is a CLAIM.
// A delivery that lands in the window between BeginTransfer's two transactions
// resolves to no payment, and claiming its key would permanently absorb every
// future delivery of that event id, leaving a real customer's real money
// unapplied forever while the provider's own retries arrived as duplicates of a
// refusal.
func TestApplyProviderEventQuarantinesAnUnknownPaymentWithoutClaimingItsKey(t *testing.T) {
	w := newPaymentsWorld(t)
	w.seedIntent(t, nil)
	p := newPayments(w)

	delivery := w.delivery("delivery_unknown", paymentprovider.KindCaptured, "cs_not_ours", "ref_not_ours", 1000, "USD")
	outcome, err := p.ApplyProviderEvent(t.Context(), delivery)
	if err != nil {
		t.Fatalf("ApplyProviderEvent: %v", err)
	}
	if outcome.Disposition != payments.DispositionQuarantined {
		t.Fatalf("an unknown payment was answered %q, want %q", outcome.Disposition, payments.DispositionQuarantined)
	}
	if outcome.Reason != payments.ReasonUnknownPayment {
		t.Errorf("the refusal says %q, want %q", outcome.Reason, payments.ReasonUnknownPayment)
	}
	if len(w.events) != 0 {
		t.Errorf("the delivery claimed its dedup key: %d event rows, want none, so the provider's retry stays applicable", len(w.events))
	}
	if len(w.legs) != 0 {
		t.Errorf("an unknown payment credited %d ledger legs", len(w.legs))
	}
	if len(w.quarantines) != 1 {
		t.Fatalf("the delivery left %d quarantine rows, want one", len(w.quarantines))
	}
	record := w.quarantines[0]
	if record.Reason != payments.ReasonUnknownPayment {
		t.Errorf("the quarantine row says %q, want %q", record.Reason, payments.ReasonUnknownPayment)
	}
	// The evidence survives without the payment: the provider's own reference
	// and the bytes the signature covered are what an operator resolves the row
	// with, and a quarantine that discarded them would be a mystery rather than
	// a question.
	if record.ProviderPaymentRef != "ref_not_ours" {
		t.Errorf("the quarantine row carries reference %q, want the provider's own %q", record.ProviderPaymentRef, "ref_not_ours")
	}
	if string(record.Payload) != string(delivery.RawBody) {
		t.Errorf("the quarantine row kept %q, want the verified bytes %q", record.Payload, delivery.RawBody)
	}
	if record.IntentID != "" {
		t.Errorf("the quarantine row names payment %s, want none: nothing resolved", record.IntentID)
	}

	// The redelivery gets a second row rather than being absorbed — the trade
	// the port's own header records as deliberate, and a far better failure than
	// a stranded payment.
	if _, err := p.ApplyProviderEvent(t.Context(), delivery); err != nil {
		t.Fatalf("the redelivery: %v", err)
	}
	if len(w.quarantines) != 2 {
		t.Errorf("two unapplied deliveries left %d quarantine rows, want two: an operator reading them learns the provider tried twice", len(w.quarantines))
	}
	if len(w.events) != 0 {
		t.Errorf("the redelivery claimed a dedup key, so a later real delivery of that event id would be absorbed")
	}
}

// TestApplyProviderEventQuarantinesAReferenceOnlyDeliveryItCannotFind is the
// payment-reference half of the same property, and it is the case that makes a
// refund survivable. A refund delivery names the payment and never the
// destination the money arrived at, so resolution has no transfer reference to
// try; when the payment reference matches nothing, the delivery is quarantined
// as an unknown payment — and the quarantine row still carries the reference, which
// is what lets an operator answer "which payment did the provider mean" without
// the payload.
func TestApplyProviderEventQuarantinesAReferenceOnlyDeliveryItCannotFind(t *testing.T) {
	w := newPaymentsWorld(t)
	w.seedIntent(t, nil)
	p := newPayments(w)

	delivery := w.delivery("delivery_refund_orphan", paymentprovider.KindRefunded, "", "ref_nowhere", 400, "USD")
	outcome, err := p.ApplyProviderEvent(t.Context(), delivery)
	if err != nil {
		t.Fatalf("ApplyProviderEvent: %v", err)
	}
	if outcome.Disposition != payments.DispositionQuarantined || outcome.Reason != payments.ReasonUnknownPayment {
		t.Fatalf("the delivery was answered %q/%q, want a quarantine for %q",
			outcome.Disposition, outcome.Reason, payments.ReasonUnknownPayment)
	}
	if len(w.quarantines) != 1 {
		t.Fatalf("the delivery left %d quarantine rows, want one", len(w.quarantines))
	}
	if got := w.quarantines[0].ProviderPaymentRef; got != "ref_nowhere" {
		t.Errorf("the quarantine row carries reference %q, want %q", got, "ref_nowhere")
	}
	if len(w.events) != 0 {
		t.Errorf("the delivery claimed its dedup key: %d event rows, want none", len(w.events))
	}
}

// TestApplyProviderEventQuarantinesEveryRefusalWithItsOwnReason walks the
// refusals a webhook can answer with. Every one of them is a REASON rather than
// an error, and the difference is what the provider is told: a reason becomes a
// quarantine and a 2xx, because the provider would send the same bytes again
// and this build would refuse them again — while an error becomes a 5xx and a
// retry the delivery is genuinely worth repeating for.
func TestApplyProviderEventQuarantinesEveryRefusalWithItsOwnReason(t *testing.T) {
	succeeded := func(intent *payments.Intent) {
		intent.Status = payments.StatusSucceeded
		intent.ProviderPaymentRef = "ref_1"
	}
	refunded := func(intent *payments.Intent) {
		intent.Status = payments.StatusRefunded
		intent.ProviderPaymentRef = "ref_1"
		intent.RefundedMinorUnits = 1000
	}
	cases := []struct {
		name     string
		seed     func(*payments.Intent)
		setup    func(*testing.T, *paymentsWorld)
		delivery func(*paymentsWorld, payments.Intent) ProviderDelivery
		want     payments.QuarantineReason
		// claimsKey reports whether the delivery is recordable at all. An
		// unknown payment is the one refusal that claims nothing.
		claimsKey bool
		after     func(*testing.T, *paymentsWorld, payments.Intent)
	}{
		{
			name: "a kind this build does not act on",
			delivery: func(w *paymentsWorld, i payments.Intent) ProviderDelivery {
				// A kind the adapter would report verbatim for an event this
				// build has no rule for: not one of the port's two, and not a
				// name this plane pretends to know.
				return w.delivery("delivery_1", "an_unrecognised_kind", i.ProviderTransferRef, "", 1000, "USD")
			},
			want:      payments.ReasonUnknownKind,
			claimsKey: true,
		},
		{
			name: "an amount that is not the one this payment was opened for",
			delivery: func(w *paymentsWorld, i payments.Intent) ProviderDelivery {
				return w.delivery("delivery_1", paymentprovider.KindCaptured, i.ProviderTransferRef, "ref_1", 999, "USD")
			},
			want:      payments.ReasonAmountMismatch,
			claimsKey: true,
		},
		{
			name: "an amount the delivery does not state at all",
			delivery: func(w *paymentsWorld, i payments.Intent) ProviderDelivery {
				return w.noAmountDelivery("delivery_1", paymentprovider.KindCaptured, i.ProviderTransferRef, "ref_1", "USD")
			},
			want:      payments.ReasonAmountMismatch,
			claimsKey: true,
		},
		{
			name: "a currency that is not the one this payment is denominated in",
			delivery: func(w *paymentsWorld, i payments.Intent) ProviderDelivery {
				return w.delivery("delivery_1", paymentprovider.KindCaptured, i.ProviderTransferRef, "ref_1", 1000, "EUR")
			},
			want:      payments.ReasonCurrencyMismatch,
			claimsKey: true,
		},
		{
			name: "a payment that already has the final word on the money",
			seed: succeeded,
			delivery: func(w *paymentsWorld, i payments.Intent) ProviderDelivery {
				return w.delivery("delivery_1", paymentprovider.KindCaptured, i.ProviderTransferRef, "ref_1", 1000, "USD")
			},
			want:      payments.ReasonStateConflict,
			claimsKey: true,
		},
		{
			name:  "a capture for an account that is no longer active",
			setup: func(_ *testing.T, w *paymentsWorld) { w.seedAccount(paymentsAccountID, identity.AccountSuspended) },
			delivery: func(w *paymentsWorld, i payments.Intent) ProviderDelivery {
				return w.delivery("delivery_1", paymentprovider.KindCaptured, i.ProviderTransferRef, "ref_1", 1000, "USD")
			},
			want:      payments.ReasonAccountClosed,
			claimsKey: true,
		},
		{
			name: "a capture for an account this platform cannot find",
			setup: func(_ *testing.T, w *paymentsWorld) {
				delete(w.accounts, identity.AccountID(paymentsAccountID))
			},
			delivery: func(w *paymentsWorld, i payments.Intent) ProviderDelivery {
				return w.delivery("delivery_1", paymentprovider.KindCaptured, i.ProviderTransferRef, "ref_1", 1000, "USD")
			},
			want:      payments.ReasonAccountClosed,
			claimsKey: true,
		},
		{
			name: "a capture for a bucket that has been closed",
			setup: func(_ *testing.T, w *paymentsWorld) {
				w.setBucketStatus(accounting.FundingBucketID("bucket-"+paymentsAccountID), accounting.BucketClosed)
			},
			delivery: func(w *paymentsWorld, i payments.Intent) ProviderDelivery {
				return w.delivery("delivery_1", paymentprovider.KindCaptured, i.ProviderTransferRef, "ref_1", 1000, "USD")
			},
			want:      payments.ReasonBucketClosed,
			claimsKey: true,
			// The status CAS runs before the credit, so the payment IS recorded
			// as captured while the money has nowhere to go. That state is the
			// operator's work item and the reason this refusal is quarantined
			// rather than rolled back.
			after: func(t *testing.T, w *paymentsWorld, intent payments.Intent) {
				t.Helper()
				if got := w.storedIntent(t, intent.ID).Status; got != payments.StatusSucceeded {
					t.Errorf("the payment is %q, want %q: the capture is real even where the money cannot land", got, payments.StatusSucceeded)
				}
			},
		},
		{
			name: "a refund ahead of the capture it refunds",
			delivery: func(w *paymentsWorld, i payments.Intent) ProviderDelivery {
				return w.delivery("delivery_1", paymentprovider.KindRefunded, i.ProviderTransferRef, "ref_1", 400, "USD")
			},
			want:      payments.ReasonRefundAheadOfCapture,
			claimsKey: true,
		},
		{
			name: "a refund that states no amount at all",
			seed: succeeded,
			delivery: func(w *paymentsWorld, i payments.Intent) ProviderDelivery {
				return w.noAmountDelivery("delivery_1", paymentprovider.KindRefunded, "", "ref_1", "USD")
			},
			// A refund with no figure is not a refund of zero and it is not a
			// ceiling: this platform cannot tell how much went back, and the
			// honest classification is a figure that disagrees with the one
			// stored — absent here rather than different. The catch-all behind
			// these branches would have called it a ceiling breach, which tells
			// an operator to go and check a number nobody stated.
			want:      payments.ReasonAmountMismatch,
			claimsKey: true,
		},
		{
			name: "a refund above what the capture took",
			seed: succeeded,
			delivery: func(w *paymentsWorld, i payments.Intent) ProviderDelivery {
				return w.delivery("delivery_1", paymentprovider.KindRefunded, "", "ref_1", 1001, "USD")
			},
			want:      payments.ReasonRefundCeiling,
			claimsKey: true,
		},
		{
			// The delivery reports MORE refunded than the payment ever took, on
			// a payment that has already given all of it back. It is not a
			// smaller total arriving late — that converges, see
			// TestAnOlderRefundReportConvergesInsteadOfQuarantining — it is a
			// state the payment cannot be in, and it is worth an operator.
			name: "a refund against a payment that is already fully refunded",
			seed: refunded,
			delivery: func(w *paymentsWorld, i payments.Intent) ProviderDelivery {
				return w.delivery("delivery_1", paymentprovider.KindRefunded, "", "ref_1", 1200, "USD")
			},
			want:      payments.ReasonStateConflict,
			claimsKey: true,
		},
		{
			name: "a delivery naming a payment this platform never opened",
			delivery: func(w *paymentsWorld, i payments.Intent) ProviderDelivery {
				return w.delivery("delivery_1", paymentprovider.KindCaptured, "cs_not_ours", "ref_not_ours", 1000, "USD")
			},
			want:      payments.ReasonUnknownPayment,
			claimsKey: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newPaymentsWorld(t)
			// The payment first, so a setup that moves a bucket into another
			// state has a bucket to move.
			intent := w.seedIntent(t, tc.seed)
			if tc.setup != nil {
				tc.setup(t, w)
			}
			p := newPayments(w)

			outcome, err := p.ApplyProviderEvent(t.Context(), tc.delivery(w, intent))
			if err != nil {
				t.Fatalf("a refusal was reported as an error (%v), which the transport would answer with a retry", err)
			}
			if outcome.Disposition != payments.DispositionQuarantined {
				t.Fatalf("the delivery was answered %q, want %q", outcome.Disposition, payments.DispositionQuarantined)
			}
			if outcome.Reason != tc.want {
				t.Errorf("the refusal says %q, want %q", outcome.Reason, tc.want)
			}
			if len(w.quarantines) != 1 {
				t.Fatalf("the refusal left %d quarantine rows, want one", len(w.quarantines))
			}
			if got := w.quarantines[0].Reason; got != tc.want {
				t.Errorf("the quarantine row says %q, want %q", got, tc.want)
			}
			if len(w.legs) != 0 {
				t.Errorf("a refused delivery credited %d ledger legs", len(w.legs))
			}
			if w.outsideTx != 0 {
				t.Errorf("%d writes arrived outside a unit of work", w.outsideTx)
			}
			if tc.claimsKey && len(w.events) != 1 {
				t.Errorf("the delivery claimed %d dedup rows, want one: a redelivery of it must be a duplicate", len(w.events))
			}
			if !tc.claimsKey && len(w.events) != 0 {
				t.Errorf("the delivery claimed its dedup key, so the provider's retry would be absorbed as a duplicate")
			}
			if tc.after != nil {
				tc.after(t, w, intent)
			}
		})
	}
}

// TestARefusedDeliveryIsLabelledQuarantinedInTheEventLedger covers the
// disposition a refused-but-recordable delivery leaves behind.
//
// A delivery that resolves to a payment and is then refused — a state conflict,
// an amount or currency mismatch, a closed account or bucket, a refund ceiling
// — has still CLAIMED its dedup key, because the claim is what stops the
// provider's retry becoming a second quarantine row and, on the paths that
// could have credited, what stops a redelivery being a second credit. So the
// row exists, and it must not say `applied`: ProviderEventRecord.Disposition is
// documented as what this plane DID with the delivery, and a reader that trusts
// it — a reconciliation counting applied deliveries, an operator listing what
// was acted on — would be shown money applied that never was.
//
// The row is therefore inserted provisionally and settled with the verdict
// before the transaction commits (persistence.PaymentEvents.Settle). Both
// tables then tell the same story: the ledger says this delivery was
// quarantined, and the quarantine holds the reason and the bytes.
func TestARefusedDeliveryIsLabelledQuarantinedInTheEventLedger(t *testing.T) {
	w := newPaymentsWorld(t)
	intent := w.seedIntent(t, func(intent *payments.Intent) {
		intent.Status = payments.StatusSucceeded
		intent.ProviderPaymentRef = "ref_1"
	})
	p := newPayments(w)

	// A capture for a payment that already has the final word: recorded, and
	// refused as a state conflict.
	delivery := w.delivery("delivery_conflict", paymentprovider.KindCaptured, intent.ProviderTransferRef, "ref_1", 1000, "USD")
	outcome, err := p.ApplyProviderEvent(t.Context(), delivery)
	if err != nil {
		t.Fatalf("ApplyProviderEvent: %v", err)
	}
	if outcome.Disposition != payments.DispositionQuarantined || outcome.Reason != payments.ReasonStateConflict {
		t.Fatalf("the delivery was answered %q/%q, want a quarantine for %q",
			outcome.Disposition, outcome.Reason, payments.ReasonStateConflict)
	}

	record, ok := w.eventRecord("delivery_conflict")
	if !ok {
		t.Fatalf("the delivery was not recorded at all, so its retry would be a second quarantine row rather than a duplicate")
	}
	if record.Disposition != payments.DispositionQuarantined {
		t.Fatalf("the event row says %q, want %q: the ledger must not claim an effect the plane refused to produce",
			record.Disposition, payments.DispositionQuarantined)
	}
	// The reason stays out of the ledger and in the quarantine ledger, where
	// the bytes are: one column per table, each saying the thing its own reader
	// asks for. The event row's job is the delivery's identity and its verdict.
	if record.Reason != "" {
		t.Errorf("the event row carries reason %q; the verdict's reason belongs on the quarantine row", record.Reason)
	}
	if len(w.quarantines) != 1 || w.quarantines[0].Reason != payments.ReasonStateConflict {
		t.Fatalf("the refusal is not in the quarantine ledger: %v", w.quarantines)
	}
	if len(w.quarantines[0].Payload) == 0 {
		t.Error("the quarantine row kept no payload, so the operator has no bytes to read")
	}
}

// TestADeliveryThatCarriedItsClaimOutIsLabelledApplied is the other half of the
// settlement, and the case a fix that simply stopped writing a disposition
// would break.
//
// The provisional value on a fresh row is `applied`, and for a delivery that
// carried its claim out that stands: nothing settles it, because there is
// nothing to correct. A capture that funds a bucket must read as applied in the
// ledger, or the reconciliation cannot tell it from the deliveries it refused.
func TestADeliveryThatCarriedItsClaimOutIsLabelledApplied(t *testing.T) {
	w := newPaymentsWorld(t)
	intent := w.seedIntent(t, func(intent *payments.Intent) {
		intent.Status = payments.StatusAwaitingTransfer
	})
	p := newPayments(w)

	outcome, err := p.ApplyProviderEvent(t.Context(),
		w.delivery("delivery_capture", paymentprovider.KindCaptured, intent.ProviderTransferRef, "ref_1", 1000, "USD"))
	if err != nil {
		t.Fatalf("ApplyProviderEvent: %v", err)
	}
	if outcome.Disposition != payments.DispositionApplied {
		t.Fatalf("the capture was answered %q/%q, want it applied", outcome.Disposition, outcome.Reason)
	}

	record, ok := w.eventRecord("delivery_capture")
	if !ok {
		t.Fatalf("the applied delivery was not recorded")
	}
	if record.Disposition != payments.DispositionApplied {
		t.Errorf("the event row says %q, want %q", record.Disposition, payments.DispositionApplied)
	}
	if len(w.quarantines) != 0 {
		t.Errorf("an applied delivery left %d quarantine rows, want none", len(w.quarantines))
	}
}

// TestApplyProviderEventRollsTheDeliveryAndTheCreditBackTogether is the other
// half of the atomicity claim. When the credit fails for a reason that is not a
// refusal the claim already answers, the delivery is not recorded either: the
// two halves share one unit of work, so a crash between them rolls back both
// and the provider's retry re-runs from the beginning.
func TestApplyProviderEventRollsTheDeliveryAndTheCreditBackTogether(t *testing.T) {
	t.Run("a credit that failed takes the delivery record with it", func(t *testing.T) {
		w := newPaymentsWorld(t)
		intent, delivery := captureFixture(t, w)
		w.topUpErr = errPaymentsLedgerDown
		p := newPayments(w)

		_, err := p.ApplyProviderEvent(t.Context(), delivery)
		if !errors.Is(err, errPaymentsLedgerDown) {
			t.Fatalf("ApplyProviderEvent = %v, want the ledger's own failure", err)
		}
		// The credit WAS attempted, inside the transaction — the rollback below
		// is the transaction's, not a call that never happened.
		if len(w.creditInTx) != 1 || !w.creditInTx[0] {
			t.Errorf("the credit was attempted %v inside a transaction, want once and inside", w.creditInTx)
		}
		if got := strings.Join(w.order, " "); !strings.HasSuffix(got, "rollback") {
			t.Errorf("the unit of work ran as %q, want it to end in a rollback", got)
		}
		if len(w.events) != 0 {
			t.Errorf("the failed delivery stayed in the event ledger: %d rows, want none — a retry would be absorbed as a duplicate", len(w.events))
		}
		if len(w.quarantines) != 0 {
			t.Errorf("a failed delivery left %d quarantine rows", len(w.quarantines))
		}
		if len(w.legs) != 0 {
			t.Errorf("a failed delivery credited %d ledger legs", len(w.legs))
		}
		if got := w.storedIntent(t, intent.ID).Status; got != payments.StatusAwaitingTransfer {
			t.Errorf("the payment is %q after a rollback, want %q", got, payments.StatusAwaitingTransfer)
		}
	})

	t.Run("a refusal the claim answers commits as a quarantine", func(t *testing.T) {
		w := newPaymentsWorld(t)
		_, delivery := captureFixture(t, w)
		w.topUpErr = accounting.ErrBucketClosed
		p := newPayments(w)

		outcome, err := p.ApplyProviderEvent(t.Context(), delivery)
		if err != nil {
			t.Fatalf("a closed bucket was reported as an error, which would ask for a retry that cannot help: %v", err)
		}
		if outcome.Disposition != payments.DispositionQuarantined || outcome.Reason != payments.ReasonBucketClosed {
			t.Fatalf("the delivery was answered %q/%q, want a quarantine for %q", outcome.Disposition, outcome.Reason, payments.ReasonBucketClosed)
		}
		if len(w.events) != 1 || len(w.quarantines) != 1 {
			t.Errorf("the committed refusal holds %d event rows and %d quarantine rows, want one of each", len(w.events), len(w.quarantines))
		}
		if w.outsideTx != 0 {
			t.Errorf("%d writes arrived outside a unit of work", w.outsideTx)
		}
	})
}

// TestACaptureResolvesByItsTransferReferenceBeforeItsPaymentReference pins the
// order of the two lookups. A capture carries both references, and the transfer
// is the one this plane WROTE: the provider issued that destination for this one
// payment and it was recorded before the customer was shown it, so resolving
// through it lands the capture on the payment the console opened rather than on
// whatever row happens to hold that payment id.
func TestACaptureResolvesByItsTransferReferenceBeforeItsPaymentReference(t *testing.T) {
	w := newPaymentsWorld(t)
	intent, delivery := captureFixture(t, w)
	p := newPayments(w)

	if _, err := p.ApplyProviderEvent(t.Context(), delivery); err != nil {
		t.Fatalf("ApplyProviderEvent: %v", err)
	}
	if len(w.resolveLookups) == 0 {
		t.Fatalf("the delivery resolved through no lookup at all")
	}
	if want := "transfer:" + intent.ProviderTransferRef; w.resolveLookups[0] != want {
		t.Errorf("resolution tried %q first, want %q", w.resolveLookups[0], want)
	}
}

// TestARefundResolvesByThePaymentReferenceItNames covers the reference a refund
// delivery can carry and the reason the port has two fields rather than one. A
// refund names the payment and never the destination it arrived at, so a lookup
// that only knew transfer references would leave every refund this platform
// received recorded as a payment it could not find.
func TestARefundResolvesByThePaymentReferenceItNames(t *testing.T) {
	w := newPaymentsWorld(t)
	intent := w.seedIntent(t, func(intent *payments.Intent) {
		intent.Status = payments.StatusSucceeded
		intent.ProviderPaymentRef = "ref_1"
	})
	p := newPayments(w)

	// No transfer reference at all: the only route to this payment is the
	// provider's payment id.
	outcome, err := p.ApplyProviderEvent(t.Context(), w.delivery("delivery_refund", paymentprovider.KindRefunded, "", "ref_1", 400, "USD"))
	if err != nil {
		t.Fatalf("ApplyProviderEvent: %v", err)
	}
	if outcome.Disposition != payments.DispositionApplied {
		t.Fatalf("a reference-only refund was answered %q/%q, want it applied", outcome.Disposition, outcome.Reason)
	}
	if outcome.IntentID != intent.ID {
		t.Errorf("the refund resolved to %s, want %s", outcome.IntentID, intent.ID)
	}
	if want := "payment:ref_1"; !slicesContains(w.resolveLookups, want) {
		t.Errorf("resolution never tried %q; it tried %v", want, w.resolveLookups)
	}
	after := w.storedIntent(t, intent.ID)
	if after.RefundedMinorUnits != 400 {
		t.Errorf("the payment records %d refunded minor units, want 400", after.RefundedMinorUnits)
	}
	if after.Status != payments.StatusPartiallyRefunded {
		t.Errorf("the payment is %q, want %q", after.Status, payments.StatusPartiallyRefunded)
	}
	// The uncovered figure is a RECORD and not a debit: B6 cannot express a
	// refund once the money has been spent, so this path books no leg at all.
	if len(w.legs) != 0 {
		t.Errorf("a refund booked %d ledger legs, want none: the correction is not expressible in this ledger", len(w.legs))
	}
	if after.UncoveredRefundMinorUnits != 400 {
		t.Errorf("the uncovered refund is recorded as %d, want the 400 the empty bucket could not cover", after.UncoveredRefundMinorUnits)
	}
}

// TestTwoPartialRefundsOfOnePaymentARecordedAsTwoCorrections is the money-facing
// half of the refund path: the provider reports a CUMULATIVE total and the
// domain wants an INCREMENT, and BOTH figures have to reach the right place for
// the customer to be made whole.
//
// Both deliveries below report what a real charge-level refund delivery reports
// — the charge's cumulative refunded total, not one refund's amount — so the
// second one says 800 after the first said 400. Handed to the DOMAIN as a total, the second adds 800
// to a total of 400 and breaches the 1000 ceiling, and a real,
// provider-acknowledged refund is quarantined as an over-refund that no operator
// can resolve, because both figures in front of them are correct. Handed to the
// PORT as an increment, the projection is a number derived from a read this
// caller made, which is what the absolute write exists to remove.
//
// So the test asserts what the repository was handed as well as the outcome: the
// totals [400 800] are the provider's own figures written whole, and the stored
// projection says 800 either way — which is exactly why the argument list has to
// be checked, and not just the row it produced.
func TestTwoPartialRefundsOfOnePaymentARecordedAsTwoCorrections(t *testing.T) {
	w := newPaymentsWorld(t)
	intent := w.seedIntent(t, func(intent *payments.Intent) {
		intent.Status = payments.StatusSucceeded
		intent.ProviderPaymentRef = "ref_1"
	})
	p := newPayments(w)

	for _, refund := range []struct {
		eventID  string
		reported int64
		want     payments.Status
	}{
		{eventID: "delivery_refund_a", reported: 400, want: payments.StatusPartiallyRefunded},
		{eventID: "delivery_refund_b", reported: 800, want: payments.StatusPartiallyRefunded},
	} {
		outcome, err := p.ApplyProviderEvent(t.Context(), w.delivery(refund.eventID, paymentprovider.KindRefunded, "", "ref_1", refund.reported, "USD"))
		if err != nil {
			t.Fatalf("refund %s: %v", refund.eventID, err)
		}
		if outcome.Disposition != payments.DispositionApplied {
			t.Fatalf("refund of a cumulative %d was answered %q/%q, want it applied: the provider reports the charge's total, not this delivery's addition",
				refund.reported, outcome.Disposition, outcome.Reason)
		}
		if got := w.storedIntent(t, intent.ID); got.Status != refund.want {
			t.Errorf("after a cumulative %d the payment is %q, want %q", refund.reported, got.Status, refund.want)
		}
	}

	after := w.storedIntent(t, intent.ID)
	if after.RefundedMinorUnits != 800 {
		t.Fatalf("the payment records %d refunded minor units, want the 800 the provider reported as its total", after.RefundedMinorUnits)
	}
	if after.UncoveredRefundMinorUnits != 800 {
		t.Errorf("the uncovered refund is recorded as %d, want 800", after.UncoveredRefundMinorUnits)
	}

	// The ABSOLUTE totals, not increments: [400 800] is what the provider
	// reported and what the statement writes. An increment here — [400 400] —
	// would mean the application had converted the provider's figure into a
	// difference against a state it read, which is the shape this member was
	// rewritten to remove: two concurrent deliveries both read the same base,
	// both add, and the projection lands above what the provider says with
	// nothing afterwards able to correct it.
	if len(w.refundAmounts) != 2 {
		t.Fatalf("the repository was handed %d refunds, want two", len(w.refundAmounts))
	}
	if w.refundAmounts[0] != 400 || w.refundAmounts[1] != 800 {
		t.Errorf("the refunds were recorded as %v, want [400 800]: each delivery hands the repository the provider's own cumulative figure", w.refundAmounts)
	}
	if w.refundRefs[0] != "ref_1" || w.refundRefs[1] != "ref_1" {
		t.Errorf("the refunds were recorded under %v, want the payment reference both times: a charge-level refund names no individual refund", w.refundRefs)
	}

	// Two refunded states, two keys. One key for both would make the second
	// correction read as a duplicate of the first at the ledger, and the
	// customer would never be made whole for it.
	first, err := payments.RefundCommandKey(paymentsProviderName, w.refundRefs[0], 400)
	if err != nil {
		t.Fatalf("deriving the first refund key: %v", err)
	}
	second, err := payments.RefundCommandKey(paymentsProviderName, w.refundRefs[1], 800)
	if err != nil {
		t.Fatalf("deriving the second refund key: %v", err)
	}
	if first == second {
		t.Errorf("two refunded states of one payment share the key %q, so the second correction would be refused as a duplicate", first)
	}
	if !strings.HasPrefix(first, "pay:"+paymentsProviderName+":refund:") {
		t.Errorf("the refund key %q is not in the refund namespace", first)
	}
}

// TestTheUncoveredRefundIsMeasuredAgainstThePaymentNotTheDelivery is the
// arithmetic the `uncovered` figure stands on, and the reason it is the
// CUMULATIVE total and not this delivery's increment that is measured.
//
// The fixture is chosen so the two readings disagree. The bucket holds 10; the
// provider reports refunds of 40 and then 70 as its running totals. Measured
// against the payment, the shortfall after the second report is 70 - 10 = 60.
// Measured per delivery — "the part of THIS delivery the balance could not
// cover", 40 - 10 = 30 and then 30 - 10 = 20 — the balance is spent twice and
// the figures sum to 50. Nothing else in the system notices: 50 passes every
// CHECK, and the operator who has to resolve the case is handed a number that
// is short by ten units of the very thing the case is about.
//
// So this asserts the ARGUMENT as well as the row. The row alone cannot decide
// it — a fake that accumulated would produce the same 60 from the right
// arguments and a wrong total from the wrong ones — and the argument is where
// the question actually lives.
func TestTheUncoveredRefundIsMeasuredAgainstThePaymentNotTheDelivery(t *testing.T) {
	w := newPaymentsWorld(t)
	bucketID := accounting.FundingBucketID("bucket-" + paymentsAccountID)
	intent := w.seedIntent(t, func(intent *payments.Intent) {
		intent.Status = payments.StatusSucceeded
		intent.ProviderPaymentRef = "ref_1"
	})
	// 10 units of the 1000-unit capture are still in the bucket; the rest has
	// been spent, which is what makes a refund leave a shortfall at all.
	w.setBucketAvailable(bucketID, 10)
	p := newPayments(w)

	for _, refund := range []struct {
		eventID  string
		reported int64
		short    int64
	}{
		{eventID: "delivery_refund_a", reported: 40, short: 30},
		{eventID: "delivery_refund_b", reported: 70, short: 60},
	} {
		outcome, err := p.ApplyProviderEvent(t.Context(), w.delivery(refund.eventID, paymentprovider.KindRefunded, "", "ref_1", refund.reported, "USD"))
		if err != nil {
			t.Fatalf("refund %s: %v", refund.eventID, err)
		}
		if outcome.Disposition != payments.DispositionApplied {
			t.Fatalf("a cumulative refund of %d was answered %q/%q, want it applied", refund.reported, outcome.Disposition, outcome.Reason)
		}
	}

	if len(w.refundUncovered) != 2 {
		t.Fatalf("the repository was handed %d shortfalls, want two", len(w.refundUncovered))
	}
	if w.refundUncovered[0] != 30 || w.refundUncovered[1] != 60 {
		t.Errorf("the shortfalls handed to the repository were %v, want [30 60]: each is the part of the CUMULATIVE refunded total the balance cannot account for, so the second cannot be this delivery's own 30",
			w.refundUncovered)
	}
	after := w.storedIntent(t, intent.ID)
	if after.UncoveredRefundMinorUnits != 60 {
		t.Errorf("the payment records an uncovered refund of %d, want 60: 70 refunded against a balance of 10", after.UncoveredRefundMinorUnits)
	}
	if after.RefundedMinorUnits != 70 {
		t.Errorf("the payment records %d refunded minor units, want the 70 the provider reported as its total", after.RefundedMinorUnits)
	}
}

// TestARedeliveredRefundConvergesInsteadOfQuarantining is the other side of the
// cumulative reading, and the reason it is not merely a duplicate-event case.
//
// A provider re-notifies by sending the same EVENT id, which the dedup ledger
// absorbs; but it also sends the same STATE under a fresh id, and the ledger
// cannot see that these are one fact. The delivery below carries a new event id
// and a total this platform has already recognised, so it adds nothing — and it
// must be answered as applied rather than refused, because the platform's record
// agrees with the provider's and there is no reconciliation for a human to do.
// Refusing it would put a work item in front of an operator for every
// re-notification, which is how a quarantine queue stops being read.
func TestARedeliveredRefundConvergesInsteadOfQuarantining(t *testing.T) {
	w := newPaymentsWorld(t)
	intent := w.seedIntent(t, func(intent *payments.Intent) {
		intent.Status = payments.StatusSucceeded
		intent.ProviderPaymentRef = "ref_1"
	})
	p := newPayments(w)

	first, err := p.ApplyProviderEvent(t.Context(), w.delivery("delivery_refund_a", paymentprovider.KindRefunded, "", "ref_1", 400, "USD"))
	if err != nil {
		t.Fatalf("the first refund: %v", err)
	}
	if first.Disposition != payments.DispositionApplied {
		t.Fatalf("the first refund was answered %q/%q, want it applied", first.Disposition, first.Reason)
	}

	// The same refunded state, a different delivery id.
	again, err := p.ApplyProviderEvent(t.Context(), w.delivery("delivery_refund_b", paymentprovider.KindRefunded, "", "ref_1", 400, "USD"))
	if err != nil {
		t.Fatalf("the re-notified refund: %v", err)
	}
	if again.Disposition != payments.DispositionApplied {
		t.Fatalf("a re-notified refund was answered %q/%q, want it applied: the record already agrees with the provider", again.Disposition, again.Reason)
	}
	if again.IntentID != intent.ID {
		t.Errorf("the re-notification resolved to %s, want %s", again.IntentID, intent.ID)
	}
	// Applied, and not a second time: one correction was recorded, and the
	// projection did not move.
	if len(w.refundAmounts) != 1 {
		t.Errorf("the repository was handed %d refunds, want one: a delivery that adds nothing is not a second refund", len(w.refundAmounts))
	}
	after := w.storedIntent(t, intent.ID)
	if after.RefundedMinorUnits != 400 {
		t.Errorf("the payment records %d refunded minor units, want the 400 already there", after.RefundedMinorUnits)
	}
	if len(w.quarantines) != 0 {
		t.Errorf("the re-notification left %d quarantine rows, want none: an operator has nothing to resolve here", len(w.quarantines))
	}
	// It is still a delivery the platform received, so it is recorded as one —
	// the ledger's answer to "did the provider send this" is yes.
	if len(w.events) != 2 {
		t.Errorf("the ledger holds %d deliveries, want both: a duplicate claim is a different verdict from an unrecorded one", len(w.events))
	}
}

// TestARedeliveredRefundInAFoldedCurrencyStillConverges pins the refund
// currency check's CASE FOLDING, which is the half of the rule that is easy to
// leave out.
//
// The check above the convergence branch exists to catch a refund stated in a
// different unit, and the one that is not in this payment's unit is a different
// CURRENCY — not a differently spelled one. A provider that reports "usd" for a
// payment this plane denominated "USD" is saying the same thing, and comparing
// the two strings raw would quarantine a real customer's real refund for its
// capitalisation. This is the same fold the CAPTURE path's comparator applies,
// and the reason is the same: the adapter's own vocabulary normalises a code
// before this plane ever sees it, and a comparison that skipped the fold would
// be stricter about the unit than about the code.
//
// The delivery below is a re-notification of a total already recognised, so
// what the fold decides is convergence against a figure this platform holds —
// the case where being wrong is quiet, because the delivery is still answered
// 2xx either way and only the reason differs.
func TestARedeliveredRefundInAFoldedCurrencyStillConverges(t *testing.T) {
	w := newPaymentsWorld(t)
	w.seedIntent(t, func(intent *payments.Intent) {
		intent.Status = payments.StatusSucceeded
		intent.ProviderPaymentRef = "ref_1"
	})
	p := newPayments(w)

	first, err := p.ApplyProviderEvent(t.Context(), w.delivery("delivery_refund_a", paymentprovider.KindRefunded, "", "ref_1", 400, "USD"))
	if err != nil {
		t.Fatalf("the first refund: %v", err)
	}
	if first.Disposition != payments.DispositionApplied {
		t.Fatalf("the first refund was answered %q/%q, want it applied", first.Disposition, first.Reason)
	}

	folded, err := p.ApplyProviderEvent(t.Context(), w.delivery("delivery_refund_b", paymentprovider.KindRefunded, "", "ref_1", 400, "usd"))
	if err != nil {
		t.Fatalf("the refund whose currency is folded: %v", err)
	}
	if folded.Disposition != payments.DispositionApplied {
		t.Errorf("a refund naming the same currency in lower case was answered %q/%q, want it applied: a code's spelling is not a disagreement about the unit",
			folded.Disposition, folded.Reason)
	}
	if len(w.quarantines) != 0 {
		t.Errorf("the folded delivery left %d quarantine rows, want none", len(w.quarantines))
	}
}

// TestARefundInAnotherCurrencyIsNotACeiling pins the OTHER half of the rule: a
// genuinely different unit is refused BEFORE the convergence branch, and the
// reason it carries is the currency rather than the ceiling.
//
// This is the case the check was added for, and the reason it sits above the
// convergence branch rather than inside the domain call. A total at or below
// what the row holds converges — the platform's record already agrees with the
// provider's — and that reading is sound only for a figure in the payment's own
// unit. A EUR 250.00 refund against a USD payment that has already been
// refunded says nothing this payment can agree with, and answering "applied"
// for it would put a delivery with no agreement behind it into the ledger as
// one that was.
//
// The reason is asserted rather than only the disposition, because both a
// currency mismatch and a ceiling are quarantines and a 2xx: an operator reads
// the reason, and a ceiling here would send them to reconcile two figures that
// are not in the same unit — a reconciliation that cannot be performed.
func TestARefundInAnotherCurrencyIsNotACeiling(t *testing.T) {
	w := newPaymentsWorld(t)
	intent := w.seedIntent(t, func(intent *payments.Intent) {
		intent.Status = payments.StatusSucceeded
		intent.ProviderPaymentRef = "ref_1"
		intent.RefundedMinorUnits = 1000
	})
	p := newPayments(w)

	// 25000 is far above the 1000 already recognised, so this is NOT the
	// convergence branch doing its job: the currency is what refuses it, and a
	// check placed below the branch would have reached the ceiling instead.
	outcome, err := p.ApplyProviderEvent(t.Context(), w.delivery("delivery_refund_eur", paymentprovider.KindRefunded, "", "ref_1", 25000, "EUR"))
	if err != nil {
		t.Fatalf("the foreign-currency refund: %v", err)
	}
	if outcome.Disposition != payments.DispositionQuarantined {
		t.Fatalf("a refund in another currency was answered %q, want it quarantined", outcome.Disposition)
	}
	if outcome.Reason != payments.ReasonCurrencyMismatch {
		t.Errorf("a refund in another currency is filed as %q, want %q: the two figures are not in the same unit, which is not a ceiling",
			outcome.Reason, payments.ReasonCurrencyMismatch)
	}
	if after := w.storedIntent(t, intent.ID); after.RefundedMinorUnits != 1000 {
		t.Errorf("the payment records %d refunded minor units, want the 1000 it already held", after.RefundedMinorUnits)
	}
}

// TestAnOlderRefundReportConvergesInsteadOfQuarantining covers the delivery
// that quotes a total BELOW what this platform already holds: an older report
// arriving after a newer one.
//
// Delivery order is not guaranteed and providers retry for days, so a
// refund notification for a 400 refund can land after the delivery of the 800
// total that includes it. The claim it makes is already satisfied — the row holds
// more than it asks for — and the answer is applied rather than a quarantine,
// because there is no reconciliation for a human to do: the platform's record
// already agrees with, and exceeds, the provider's own older figure. Filling the
// operator queue with these is how a quarantine queue stops being read.
//
// The premise is asserted here rather than assumed, because the whole
// convergence rests on it: the provider's cumulative refunded total is
// MONOTONE. A reversal is not a lower figure on this event kind — it is a
// different kind, which this build quarantines as one it does not recognise.
//
// The direction is what makes this test worth having at all. Were the
// comparison to run the other way, an older total would be read as a fresh
// refund of the difference, and the projection would move on a subtraction that
// ran backwards.
func TestAnOlderRefundReportConvergesInsteadOfQuarantining(t *testing.T) {
	w := newPaymentsWorld(t)
	intent := w.seedIntent(t, func(intent *payments.Intent) {
		intent.Status = payments.StatusPartiallyRefunded
		intent.ProviderPaymentRef = "ref_1"
		intent.RefundedMinorUnits = 800
	})
	p := newPayments(w)

	outcome, err := p.ApplyProviderEvent(t.Context(), w.delivery("delivery_refund_older", paymentprovider.KindRefunded, "", "ref_1", 400, "USD"))
	if err != nil {
		t.Fatalf("ApplyProviderEvent: %v", err)
	}
	if outcome.Disposition != payments.DispositionApplied {
		t.Fatalf("a delivery reporting less refunded than the platform holds was answered %q/%q, want it applied: the claim is already met", outcome.Disposition, outcome.Reason)
	}
	if len(w.refundAmounts) != 0 {
		t.Errorf("a backwards total was recorded as a refund of %v, want no correction: the projection is monotone", w.refundAmounts)
	}
	if after := w.storedIntent(t, intent.ID); after.RefundedMinorUnits != 800 {
		t.Errorf("the payment records %d refunded minor units, want the 800 it already held", after.RefundedMinorUnits)
	}
	if len(w.quarantines) != 0 {
		t.Errorf("an older report left %d quarantine rows, want none: the record already agrees with the provider", len(w.quarantines))
	}
	// It is still a delivery this platform received and it is recorded as one,
	// with its own figure on the row — which is why converging loses nothing:
	// the fact that the provider once reported 400 is durable even though the
	// projection did not move for it.
	if len(w.events) != 1 {
		t.Fatalf("the ledger holds %d deliveries, want the one that arrived", len(w.events))
	}
	record, ok := w.eventRecord("delivery_refund_older")
	if !ok {
		t.Fatalf("the delivery was not recorded at all")
	}
	if record.AmountMinorUnits == nil || *record.AmountMinorUnits != 400 {
		t.Errorf("the delivery row records %v, want the 400 the provider actually reported", record.AmountMinorUnits)
	}
}

// TestListPaymentsPutsTheAccountPredicateInTheQuery checks the isolation the
// console rests on. Another account's payment is a row the statement never
// returned — the same answer, at the same cost, a genuinely absent row gives —
// and not a row fetched and then filtered out above the port, which would be
// the same answer with the data already in this process.
func TestListPaymentsPutsTheAccountPredicateInTheQuery(t *testing.T) {
	w := newPaymentsWorld(t)
	w.seedAccount(paymentsAccount2ID, identity.AccountActive)
	for i := 0; i < 3; i++ {
		w.seedIntent(t, nil)
	}
	mine := w.seedIntent(t, func(intent *payments.Intent) {
		intent.AccountID = paymentsAccount2ID
		intent.IdempotencyKey = "theirs"
	})
	p := newPayments(w)

	page, err := p.ListPayments(t.Context(), paymentsAccount2ID, "", 10)
	if err != nil {
		t.Fatalf("ListPayments: %v", err)
	}
	if len(w.listAccounts) != 1 || w.listAccounts[0] != paymentsAccount2ID {
		t.Fatalf("the repository was asked about %v, want the caller's own account %q", w.listAccounts, paymentsAccount2ID)
	}
	if len(page.Items) != 1 {
		t.Fatalf("the page holds %d payments, want the one this account owns", len(page.Items))
	}
	if page.Items[0].ID != mine.ID {
		t.Errorf("the page holds %s, want %s", page.Items[0].ID, mine.ID)
	}
	for _, item := range page.Items {
		if item.AccountID != paymentsAccount2ID {
			t.Errorf("the page carries a payment of account %q", item.AccountID)
		}
	}
	if page.HasMore {
		t.Errorf("a one-row page of a one-row list reports more")
	}
}

// TestListPaymentsTrimsTheProbeRowAndAnswersHasMore pins the trim that used to
// be missing here. The repository asks for one row more than the caller wanted,
// and that extra row is the whole of `has_more`; returning the port's slice
// unchanged hands the caller a row more than it asked for AND reports
// `has_more: false` on a full page — two contract violations from one missing
// trim, neither of them visible to a test that only ever pages a single row.
func TestListPaymentsTrimsTheProbeRowAndAnswersHasMore(t *testing.T) {
	w := newPaymentsWorld(t)
	for i := 0; i < 3; i++ {
		w.seedIntent(t, nil)
	}
	p := newPayments(w)

	page, err := p.ListPayments(t.Context(), paymentsAccountID, "", 2)
	if err != nil {
		t.Fatalf("ListPayments: %v", err)
	}
	if len(page.Items) != 2 {
		t.Errorf("the page holds %d payments, want the 2 that were asked for", len(page.Items))
	}
	if !page.HasMore {
		t.Errorf("a full page over a longer list reports has_more: false, so a client stops early")
	}
	if page.NextCursor == "" {
		t.Errorf("a non-empty page carries no cursor, so a client cannot continue")
	}
	if len(w.listPages) != 1 || w.listPages[0].Limit != 2 {
		t.Errorf("the repository was asked for %v, want the caller's resolved page size of 2", w.listPages)
	}
	// The cursor resumes where this page stopped: the next page is the row
	// after the last one handed over.
	rest, err := p.ListPayments(t.Context(), paymentsAccountID, page.NextCursor, 2)
	if err != nil {
		t.Fatalf("the second page: %v", err)
	}
	if len(rest.Items) != 1 {
		t.Errorf("the second page holds %d payments, want the one left over", len(rest.Items))
	}
	if rest.HasMore {
		t.Errorf("the last page reports more rows after it")
	}
	for _, item := range page.Items {
		if item.ID == rest.Items[0].ID {
			t.Errorf("payment %s appears on both pages", item.ID)
		}
	}

	// A page that is not full is the end, and it still carries a cursor: the
	// contract's next_cursor is non-empty on every page.
	last, err := p.ListPayments(t.Context(), paymentsAccountID, "", 10)
	if err != nil {
		t.Fatalf("the whole list: %v", err)
	}
	if len(last.Items) != 3 || last.HasMore {
		t.Errorf("the whole list is %d rows with has_more %v, want 3 and false", len(last.Items), last.HasMore)
	}
	if last.NextCursor == "" {
		t.Errorf("an empty next_cursor on a complete page")
	}
}

// TestListPaymentsRefusesWhatTheSurfaceRefuses checks that this list re-decides
// none of the paging refusals. ResolvePage owns the limit bounds and the
// cursor's binding to a collection, and a payments list that resolved them
// differently from the nine beside it would be a list whose page size means
// something else — the class of difference a client cannot see.
func TestListPaymentsRefusesWhatTheSurfaceRefuses(t *testing.T) {
	cases := []struct {
		name   string
		cursor string
		limit  int
	}{
		{"a limit over the contract's bound", "", persistence.MaxPageLimit + 1},
		{"a negative page size", "", -1},
		{"a cursor this surface cannot place", "not-a-cursor", 10},
		{"a cursor minted by another list", EncodeCursor(collectionUsers, "user-2", Fingerprint(collectionUsers, nil)), 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A world per case, because the assertion is that the refusal
			// happened BEFORE the repository was reached, and a shared world
			// would count the previous case's call.
			w := newPaymentsWorld(t)
			p := newPayments(w)

			_, err := p.ListPayments(t.Context(), paymentsAccountID, tc.cursor, tc.limit)
			if code := applicationCodeOf(t, err); code != CodeInvalidRequest {
				t.Errorf("%s answered %q, want %q", tc.name, code, CodeInvalidRequest)
			}
			if len(w.listAccounts) != 0 {
				t.Errorf("%s reached the repository: %v", tc.name, w.listAccounts)
			}
		})
	}

	// An absent page size is the contract's default rather than a request for
	// no rows, and the default is the port's own constant so the two layers
	// cannot disagree about where the bound is. A caller that sends 0 has said
	// the same thing as one that sent nothing, which is what the contract
	// documents rather than a refusal.
	w := newPaymentsWorld(t)
	p := newPayments(w)
	if _, err := p.ListPayments(t.Context(), paymentsAccountID, "", 0); err != nil {
		t.Fatalf("the default page size was refused: %v", err)
	}
	if len(w.listPages) != 1 || w.listPages[0].Limit != persistence.DefaultPageLimit {
		t.Errorf("an absent page size reached the repository as %v, want the default %d", w.listPages, persistence.DefaultPageLimit)
	}

	// And a list with no account is not an empty list: it is a caller that did
	// not identify whose payments it means.
	if _, err := p.ListPayments(t.Context(), "", "", 10); applicationCodeOf(t, err) != CodeUnauthenticated {
		t.Errorf("an accountless list was not refused as unauthenticated")
	}
}

// TestTransferInstructionsForAnswersAnotherAccountsPaymentAsAMiss pins the
// shape of the ownership refusal. The caller asked about a payment that is not
// its own, and the answer is the one a genuinely absent row gets, because a
// distinct answer for "not yours" turns the difference into a confirmation
// oracle.
func TestTransferInstructionsForAnswersAnotherAccountsPaymentAsAMiss(t *testing.T) {
	w := newPaymentsWorld(t)
	w.seedAccount(paymentsAccount2ID, identity.AccountActive)
	intent := w.seedIntent(t, nil)
	p := newPayments(w)

	instructions, err := p.TransferInstructionsFor(t.Context(), paymentsAccountID, intent.ID)
	if err != nil {
		t.Fatalf("TransferInstructionsFor: %v", err)
	}
	// Verbatim, and never parsed: they are a promise the provider made and this
	// platform is keeping on the provider's behalf.
	if instructions.TransferCode != w.providerInstructions.TransferCode ||
		instructions.BankName != w.providerInstructions.BankName ||
		instructions.AccountHolder != w.providerInstructions.AccountHolder ||
		instructions.QRURL != w.providerInstructions.QRURL {
		t.Errorf("TransferInstructionsFor = %+v, want the stored destination %+v unchanged", instructions, w.providerInstructions)
	}

	if _, err := p.TransferInstructionsFor(t.Context(), paymentsAccount2ID, intent.ID); applicationCodeOf(t, err) != CodeNotFound {
		t.Errorf("another account's payment was not answered as a miss")
	}
	missing := payments.IntentID("44444444-4444-4444-8444-444444444444")
	if _, err := p.TransferInstructionsFor(t.Context(), paymentsAccountID, missing); applicationCodeOf(t, err) != CodeNotFound {
		t.Errorf("a payment that does not exist was not answered as a miss")
	}

	// A payment that has not reached the provider has no destination, and the
	// answer for it is the zero value rather than an error: the console renders
	// no instructions, which is exactly what there are.
	unopened := w.seedIntent(t, func(intent *payments.Intent) {
		intent.Status = payments.StatusCreated
		intent.ProviderTransferRef = ""
		intent.ProviderBankName = ""
		intent.ProviderAccountHolder = ""
		intent.ProviderQRURL = ""
	})
	instructions, err = p.TransferInstructionsFor(t.Context(), paymentsAccountID, unopened.ID)
	if err != nil {
		t.Fatalf("a payment with no destination was refused: %v", err)
	}
	if (instructions != payments.TransferInstructions{}) {
		t.Errorf("a payment with no destination answered %+v, want nothing", instructions)
	}
}

// TestOffersPublishesTheDeclarationOrderAsACopy pins the two properties the
// chooser depends on, both of which a bare map would lose.
//
// The ORDER is the operator's one lever over how their own price list reads. Go
// hands a map's iteration order to whoever ranges it, so a catalogue built on
// one would reshuffle a customer's options between page loads.
//
// The COPY is not defensive noise: a handler that sorted or truncated the slice
// in place would reorder every later reader of the same catalogue, and the
// deployment's price list would depend on which request happened to render
// first.
func TestOffersPublishesTheDeclarationOrderAsACopy(t *testing.T) {
	declared := []TopUpOffer{
		{ID: "starter", AmountMinorUnits: 1000, Currency: "USD", MinorUnitExponent: 2, Label: "Starter credit"},
		{ID: "annual", AmountMinorUnits: 120000, Currency: "USD", MinorUnitExponent: 2, Label: "Annual top-up"},
	}
	w := newPaymentsWorld(t)
	w.offers = declared
	p := newPayments(w)

	got := p.Offers()
	if len(got) != 2 {
		t.Fatalf("Offers() held %d offers, want 2", len(got))
	}
	for i, offer := range got {
		if offer.ID != declared[i].ID {
			t.Errorf("offer %d is %q, want the declared %q", i, offer.ID, declared[i].ID)
		}
	}
	if got[1].Label != "Annual top-up" {
		t.Errorf("the label is not carried through: %q", got[1].Label)
	}

	// Mutating what was handed out does not touch the catalogue.
	got[0] = TopUpOffer{ID: "hijacked"}
	got[1].ID = "rewritten"
	again := p.Offers()
	if again[0].ID != "starter" || again[1].ID != "annual" {
		t.Errorf("Offers() returned the catalogue's own slice: a later reader sees %q, %q", again[0].ID, again[1].ID)
	}

	// The lookup is by the identifier a client sent, and an identifier that is
	// not in the catalogue has no price.
	if offer, ok := p.offers.Offer("annual"); !ok || offer.AmountMinorUnits != 120000 {
		t.Errorf("the catalogue did not resolve the offer it declares: %v %v", offer, ok)
	}
	if _, ok := p.offers.Offer("platinum"); ok {
		t.Errorf("the catalogue resolved an offer it does not declare")
	}
}

// TestNewTopUpCatalogueRefusesADuplicateIdentifier checks the constructor's one
// refusal. Which price a customer is charged may not depend on ordering, so a
// catalogue that declares one offer id twice is a wiring defect rather than a
// later-entry-wins, and the place to learn about it is construction rather than
// the middle of a top-up.
func TestNewTopUpCatalogueRefusesADuplicateIdentifier(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatalf("a catalogue declaring one offer id twice was accepted")
		}
	}()
	NewTopUpCatalogue([]TopUpOffer{
		{ID: "starter", AmountMinorUnits: 1000, Currency: "USD", MinorUnitExponent: 2},
		{ID: "starter", AmountMinorUnits: 2000, Currency: "USD", MinorUnitExponent: 2},
	})
}

// slicesContains reports whether values holds want. It is a local helper rather
// than an import because the two call sites read better as a sentence.
func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
