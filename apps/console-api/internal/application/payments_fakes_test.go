package application

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/payments"
	paymentprovider "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/payments"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The payments fakes: one world, in the same spirit as the accounting and
// reconciliation worlds.
//
// Two properties of this feature can only be stated against a world that can
// see them, and they are why the fakes are one shared state rather than seven
// independent mocks. The first is that the provider is called OUTSIDE any unit
// of work — a provider call inside a transaction pins a connection and a row
// lock for as long as a third party takes to answer — so the world counts how
// deep the open units of work were when the provider was reached. The second is
// that the delivery record, the status move and the credit are ONE unit of work
// — so the credit records the context it was handed, and a failure rolls the
// world back the way the store would.
//
// The other observation the world keeps is the `outsideTx` counter, which every
// write member raises when it is reached without the transaction's context. It
// is the mechanical check that the halves of the webhook path share the store's
// unit of work rather than each committing separately and silently.

const (
	paymentsAccountID    = "11111111-1111-4111-8111-111111111111"
	paymentsAccount2ID   = "22222222-2222-4222-8222-222222222222"
	paymentsProviderName = "stripe"
	paymentsMerchant     = "acct_merchant_1"
	paymentsReturnURL    = "https://console.example/payments/return"
)

// paymentsNow is the instant every payments test's clock reads, so a failure
// names the same instant a rerun does.
var paymentsNow = time.Date(2026, time.September, 25, 12, 0, 0, 0, time.UTC)

// The two failures the tests inject. They are plain errors rather than
// application ones because both belong to a port, and the use case's job is to
// translate or propagate them rather than to invent them.
var (
	// errPaymentsProviderDown is the retryable class: the provider did not
	// answer at all.
	errPaymentsProviderDown = errors.New("fakes: the provider is unreachable")
	// errPaymentsProviderRefused is the other class: the provider answered, and
	// answered that it will not do this. No retry supplies what it refused, so
	// the application must not report it as an outage.
	errPaymentsProviderRefused = errors.New("fakes: the provider refused the checkout")
	errPaymentsLedgerDown      = errors.New("fakes: the ledger refused the leg")
)

// paymentsWorld is the durable state the fakes answer from, plus the
// observations the properties are judged against.
type paymentsWorld struct {
	// order is the observation log: "begin", "provider", "record-event",
	// "topup", "commit", "rollback", in the order they happened. It survives a
	// rollback on purpose — what the tests need to see is both what was
	// attempted and what survived.
	order []string

	intents          map[payments.IntentID]payments.Intent
	intentByKey      map[string]payments.IntentID
	intentByCheckout map[string]payments.IntentID
	intentByPayment  map[string]payments.IntentID

	events      map[string]payments.ProviderEventRecord
	quarantines []payments.QuarantineRecord

	accounts        map[identity.AccountID]identity.Account
	buckets         map[accounting.FundingBucketID]accounting.Bucket
	bucketByAccount map[commerce.AccountID]accounting.FundingBucketID
	legs            []accounting.LedgerEntry

	now      time.Time
	offers   []TopUpOffer
	settings PaymentsSettings

	// The provider's stand-in.
	providerSession paymentprovider.CheckoutSession
	providerCalls   []paymentprovider.CheckoutRequest
	providerErr     error
	// onProviderCall runs while the provider is "answering", which is how a
	// test lands another writer's commit in the window between the call and the
	// recording of its answer.
	onProviderCall func(call int)

	// The observations.
	beginUnits     int
	unitDepth      int
	providerDepth  []int
	providerInTx   []bool
	outsideTx      int
	creditInTx     []bool
	creditDepths   []int
	listAccounts   []string
	listPages      []persistence.PaymentPage
	resolveLookups []string
	refundRefs     []string
	refundAmounts  []int64
	// refundUncovered is the `uncovered` argument as the repository received
	// it, kept apart from the row it produced so a test can assert the FIGURE
	// that crossed rather than only the state that came back.
	refundUncovered []int64

	// The knobs a test sets.
	topUpErr error
	failOn   map[string]error
}

// paymentsSnapshot is the durable state a unit of work restores when its
// callback fails: everything a rollback would take back, and nothing that is an
// observation.
type paymentsSnapshot struct {
	intents          map[payments.IntentID]payments.Intent
	intentByKey      map[string]payments.IntentID
	intentByCheckout map[string]payments.IntentID
	intentByPayment  map[string]payments.IntentID
	events           map[string]payments.ProviderEventRecord
	quarantines      []payments.QuarantineRecord
	buckets          map[accounting.FundingBucketID]accounting.Bucket
	bucketByAccount  map[commerce.AccountID]accounting.FundingBucketID
	legs             []accounting.LedgerEntry
}

func newPaymentsWorld(t *testing.T) *paymentsWorld {
	t.Helper()
	w := &paymentsWorld{
		intents:          map[payments.IntentID]payments.Intent{},
		intentByKey:      map[string]payments.IntentID{},
		intentByCheckout: map[string]payments.IntentID{},
		intentByPayment:  map[string]payments.IntentID{},
		events:           map[string]payments.ProviderEventRecord{},
		accounts:         map[identity.AccountID]identity.Account{},
		buckets:          map[accounting.FundingBucketID]accounting.Bucket{},
		bucketByAccount:  map[commerce.AccountID]accounting.FundingBucketID{},
		now:              paymentsNow,
		offers: []TopUpOffer{{
			ID:                "starter",
			AmountMinorUnits:  1000,
			Currency:          "USD",
			MinorUnitExponent: 2,
			Label:             "Starter credit",
		}},
		settings: PaymentsSettings{
			Provider:           paymentsProviderName,
			ProviderAccountKey: paymentsMerchant,
			CheckoutReturnURL:  paymentsReturnURL,
		},
		providerSession: paymentprovider.CheckoutSession{
			URL:         "https://pay.example/checkout/session_1",
			ProviderRef: "cs_1",
		},
		failOn: map[string]error{},
	}
	w.seedAccount(paymentsAccountID, identity.AccountActive)
	return w
}

// newPayments wires the use cases over one world, the way every payments test
// builds them: all eleven ports answer from the same state.
func newPayments(w *paymentsWorld) *Payments {
	return NewPayments(
		fakePaymentsStore{world: w},
		fakePaymentIntents{world: w},
		fakePaymentEvents{world: w},
		fakePaymentQuarantine{world: w},
		fakePaymentsClock{world: w},
		fakePaymentsAccounts{world: w},
		fakeBucketReader{world: w},
		fakeAccountFunding{world: w},
		fakeTopUpWriter{world: w},
		fakeCheckoutProvider{world: w},
		NewTopUpCatalogue(w.offers),
		w.settings,
	)
}

func (w *paymentsWorld) record(entry string) {
	w.order = append(w.order, entry)
}

// put indexes an intent under every key it can be read by. An empty provider
// reference is not indexed: it is the absence of an answer, not an answer every
// payment shares.
func (w *paymentsWorld) put(intent payments.Intent) {
	w.intents[intent.ID] = intent
	if intent.IdempotencyKey != "" {
		w.intentByKey[paymentsKey(intent.AccountID, intent.IdempotencyKey)] = intent.ID
	}
	if intent.ProviderCheckoutRef != "" {
		w.intentByCheckout[paymentsProviderKey(intent.Provider, intent.ProviderCheckoutRef)] = intent.ID
	}
	if intent.ProviderPaymentRef != "" {
		w.intentByPayment[paymentsProviderKey(intent.Provider, intent.ProviderPaymentRef)] = intent.ID
	}
}

// paymentsKey is the uniqueness the schema puts on (account, idempotency key).
func paymentsKey(accountID, key string) string { return accountID + "\x00" + key }

// paymentsProviderKey is the uniqueness the schema puts on (provider, provider
// reference) — for both of the reference columns, because the two are the two
// ways a third party's payload is kept from choosing whose money moves.
func paymentsProviderKey(provider, ref string) string { return provider + "\x00" + ref }

// paymentsEventKey is the delivery dedup key: one row per (provider, merchant
// account, event id).
func paymentsEventKey(event payments.ProviderEventRecord) string {
	return deliveryKeyString(event.DeliveryKey())
}

// deliveryKeyString keys a settled delivery by the same triple the adapter's
// statement predicates on, and it is derived through the SAME method the
// application settles with — so a fake that stopped agreeing with the port's
// key would fail here rather than quietly settle a different row.
func deliveryKeyString(key payments.DeliveryKey) string {
	return key.Provider + "\x00" + key.ProviderAccountKey + "\x00" + key.EventID
}

func (w *paymentsWorld) snapshot() paymentsSnapshot {
	return paymentsSnapshot{
		intents:          copyIntentMap(w.intents),
		intentByKey:      copyIDIndex(w.intentByKey),
		intentByCheckout: copyIDIndex(w.intentByCheckout),
		intentByPayment:  copyIDIndex(w.intentByPayment),
		events:           copyEventMap(w.events),
		quarantines:      append([]payments.QuarantineRecord(nil), w.quarantines...),
		buckets:          copyBucketMap(w.buckets),
		bucketByAccount:  copyBucketIndex(w.bucketByAccount),
		legs:             append([]accounting.LedgerEntry(nil), w.legs...),
	}
}

func (w *paymentsWorld) restore(s paymentsSnapshot) {
	w.intents, w.intentByKey = s.intents, s.intentByKey
	w.intentByCheckout, w.intentByPayment = s.intentByCheckout, s.intentByPayment
	w.events, w.quarantines = s.events, s.quarantines
	w.buckets, w.bucketByAccount, w.legs = s.buckets, s.bucketByAccount, s.legs
}

// seedAccount puts an account in a state, defaulting the fields nothing reads.
func (w *paymentsWorld) seedAccount(id string, state identity.AccountState) identity.Account {
	account := identity.Account{ID: identity.AccountID(id), Name: "Acme", State: state}
	w.accounts[account.ID] = account
	return account
}

// seedFunding opens an active funding bucket for an account directly, for the
// tests that need one before any use case runs.
func (w *paymentsWorld) seedFunding(t *testing.T, accountID string) accounting.Bucket {
	t.Helper()
	bucket := accounting.Bucket{
		ID:        accounting.FundingBucketID("bucket-" + accountID),
		AccountID: accounting.AccountID(accountID),
		Status:    accounting.BucketActive,
		CreatedAt: w.now,
		UpdatedAt: w.now,
	}
	w.buckets[bucket.ID] = bucket
	w.bucketByAccount[commerce.AccountID(accountID)] = bucket.ID
	return bucket
}

// seedIntent writes a payment directly, for the tests that start from a state
// the use cases would have reached later. It is a struct literal rather than a
// constructor because the interesting fixtures are states New refuses to build.
func (w *paymentsWorld) seedIntent(t *testing.T, tweak func(*payments.Intent)) payments.Intent {
	t.Helper()
	bucketID := accounting.FundingBucketID("bucket-" + paymentsAccountID)
	if _, ok := w.buckets[bucketID]; !ok {
		w.seedFunding(t, paymentsAccountID)
	}
	intent := payments.Intent{
		ID:                  payments.IntentID(fmt.Sprintf("33333333-3333-4333-8333-%012d", len(w.intents)+1)),
		AccountID:           paymentsAccountID,
		FundingBucketID:     string(bucketID),
		AmountMinorUnits:    1000,
		Currency:            "USD",
		MinorUnitExponent:   2,
		Provider:            paymentsProviderName,
		ProviderCheckoutRef: "cs_1",
		IdempotencyKey:      fmt.Sprintf("idem-%d", len(w.intents)+1),
		Status:              payments.StatusCheckoutOpen,
		CreatedAt:           w.now,
		UpdatedAt:           w.now,
		ExpiresAt:           w.now.Add(30 * time.Minute),
	}
	if tweak != nil {
		tweak(&intent)
	}
	w.put(intent)
	return intent
}

// storedIntent reads a payment back out of the world, so an assertion names the
// durable state rather than a return value.
func (w *paymentsWorld) storedIntent(t *testing.T, id payments.IntentID) payments.Intent {
	t.Helper()
	intent, ok := w.intents[id]
	if !ok {
		t.Fatalf("payment %s is not in the world", id)
	}
	return intent
}

// storedBucket reads the world's bucket row.
func (w *paymentsWorld) storedBucket(t *testing.T, id accounting.FundingBucketID) accounting.Bucket {
	t.Helper()
	bucket, ok := w.buckets[id]
	if !ok {
		t.Fatalf("bucket %s is not in the world", id)
	}
	return bucket
}

// legsFor is the ledger as the credit primitive left it.
func (w *paymentsWorld) legsFor(bucketID accounting.FundingBucketID) []accounting.LedgerEntry {
	var legs []accounting.LedgerEntry
	for _, leg := range w.legs {
		if leg.FundingBucketID == bucketID {
			legs = append(legs, leg)
		}
	}
	return legs
}

// eventRecord reads a recorded delivery by its event id.
func (w *paymentsWorld) eventRecord(eventID string) (payments.ProviderEventRecord, bool) {
	for _, record := range w.events {
		if record.EventID == eventID {
			return record, true
		}
	}
	return payments.ProviderEventRecord{}, false
}

// delivery builds a verified delivery the way a transport would hand one over:
// a normalised event beside the bytes it was verified over.
//
// checkoutRef is the session this platform opened and paymentRef is the
// provider's id for the money. Either may be empty, and which of the two a
// delivery carries is the whole difference between a capture and a refund: a
// capture states both and a refund states only the payment.
func (w *paymentsWorld) delivery(eventID, kind, checkoutRef, paymentRef string, amount int64, currency string) ProviderDelivery {
	return ProviderDelivery{
		Event: paymentprovider.ProviderEvent{
			Kind:             kind,
			EventID:          eventID,
			CheckoutRef:      checkoutRef,
			PaymentRef:       paymentRef,
			AmountMinorUnits: &amount,
			Currency:         currency,
			OccurredAt:       w.now,
		},
		RawBody: []byte(`{"id":"` + eventID + `"}`),
	}
}

// noAmountDelivery is a delivery that states no amount at all, for the paths
// whose refusal is about an absent figure rather than a wrong one.
func (w *paymentsWorld) noAmountDelivery(eventID, kind, checkoutRef, paymentRef, currency string) ProviderDelivery {
	return ProviderDelivery{
		Event: paymentprovider.ProviderEvent{
			Kind:        kind,
			EventID:     eventID,
			CheckoutRef: checkoutRef,
			PaymentRef:  paymentRef,
			Currency:    currency,
			OccurredAt:  w.now,
		},
		RawBody: []byte(`{"id":"` + eventID + `"}`),
	}
}

// hasPaymentsStatus reports whether statuses names status.
func hasPaymentsStatus(statuses []payments.Status, status payments.Status) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func copyIntentMap(in map[payments.IntentID]payments.Intent) map[payments.IntentID]payments.Intent {
	out := make(map[payments.IntentID]payments.Intent, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func copyIDIndex(in map[string]payments.IntentID) map[string]payments.IntentID {
	out := make(map[string]payments.IntentID, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func copyEventMap(in map[string]payments.ProviderEventRecord) map[string]payments.ProviderEventRecord {
	out := make(map[string]payments.ProviderEventRecord, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func copyBucketMap(in map[accounting.FundingBucketID]accounting.Bucket) map[accounting.FundingBucketID]accounting.Bucket {
	out := make(map[accounting.FundingBucketID]accounting.Bucket, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func copyBucketIndex(in map[commerce.AccountID]accounting.FundingBucketID) map[commerce.AccountID]accounting.FundingBucketID {
	out := make(map[commerce.AccountID]accounting.FundingBucketID, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// fakePaymentsStore is the unit of work. It is a real one in miniature: it
// snapshots the durable state at begin, restores it when the callback returns
// an error, and hands the callback a context carrying the marker the other
// fakes use to prove they were given the transaction's context and not the one
// the call arrived on.
type fakePaymentsStore struct {
	persistence.Store
	world *paymentsWorld
}

func (s fakePaymentsStore) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	w := s.world
	w.beginUnits++
	w.unitDepth++
	w.record("begin")
	snapshot := w.snapshot()
	if err := fn(context.WithValue(ctx, txMarkerKey{}, true)); err != nil {
		w.restore(snapshot)
		w.unitDepth--
		w.record("rollback")
		return err
	}
	w.unitDepth--
	w.record("commit")
	return nil
}

func (s fakePaymentsStore) InUnitOfWork(ctx context.Context) bool { return inTransaction(ctx) }

// fakePaymentIntents is the payment aggregate's durable surface, running the
// same compare-and-swap discipline the adapter's statements do: a move lands
// only while the row still shows a state the caller read, and a miss is
// ErrNotFound.
type fakePaymentIntents struct {
	persistence.PaymentIntents
	world *paymentsWorld
}

func (f fakePaymentIntents) Create(ctx context.Context, intent payments.Intent) error {
	w := f.world
	if !inTransaction(ctx) {
		w.outsideTx++
	}
	w.record("create")
	if _, seen := w.intentByKey[paymentsKey(intent.AccountID, intent.IdempotencyKey)]; seen {
		return payments.ErrDuplicatePayment
	}
	w.put(intent)
	return nil
}

func (f fakePaymentIntents) ByID(_ context.Context, id payments.IntentID) (payments.Intent, error) {
	intent, ok := f.world.intents[id]
	if !ok {
		return payments.Intent{}, persistence.ErrNotFound
	}
	return intent, nil
}

func (f fakePaymentIntents) ByAccountAndIdempotencyKey(_ context.Context, accountID, key string) (payments.Intent, error) {
	id, ok := f.world.intentByKey[paymentsKey(accountID, key)]
	if !ok {
		return payments.Intent{}, persistence.ErrNotFound
	}
	return f.world.intents[id], nil
}

// The two provider-reference lookups record WHICH one was asked, because the
// order between them is a decision: a capture carries both references and must
// resolve through the checkout this platform wrote, while a refund carries only
// the payment and has no other route.
func (f fakePaymentIntents) ByProviderCheckoutRef(_ context.Context, provider, ref string) (payments.Intent, error) {
	f.world.resolveLookups = append(f.world.resolveLookups, "checkout:"+ref)
	id, ok := f.world.intentByCheckout[paymentsProviderKey(provider, ref)]
	if !ok {
		return payments.Intent{}, persistence.ErrNotFound
	}
	return f.world.intents[id], nil
}

func (f fakePaymentIntents) ByProviderPaymentRef(_ context.Context, provider, ref string) (payments.Intent, error) {
	f.world.resolveLookups = append(f.world.resolveLookups, "payment:"+ref)
	id, ok := f.world.intentByPayment[paymentsProviderKey(provider, ref)]
	if !ok {
		return payments.Intent{}, persistence.ErrNotFound
	}
	return f.world.intents[id], nil
}

// RecordCheckout is the compare-and-swap on status and state version together:
// a row that has moved under the caller's feet matches nothing and answers
// false, and the caller's reference is NOT written.
func (f fakePaymentIntents) RecordCheckout(_ context.Context, id payments.IntentID, checkout payments.OpenedCheckout, from []payments.Status, now time.Time) (bool, error) {
	w := f.world
	w.record("record-checkout")
	intent, ok := w.intents[id]
	if !ok {
		return false, persistence.ErrNotFound
	}
	if !hasPaymentsStatus(from, intent.Status) {
		return false, nil
	}
	intent.ProviderCheckoutRef = checkout.ProviderRef
	intent.CheckoutURL = checkout.URL
	intent.Status = payments.StatusCheckoutOpen
	intent.UpdatedAt = now
	intent.StateVersion++
	w.put(intent)
	return true, nil
}

// RecordCapture records the provider's payment reference and moves the payment
// to succeeded in one statement, the way the adapter's guarded UPDATE does.
func (f fakePaymentIntents) RecordCapture(_ context.Context, id payments.IntentID, providerPaymentRef string, from []payments.Status, now time.Time) (payments.Intent, bool, error) {
	w := f.world
	w.record("record-capture")
	intent, ok := w.intents[id]
	if !ok {
		return payments.Intent{}, false, persistence.ErrNotFound
	}
	if !hasPaymentsStatus(from, intent.Status) {
		return payments.Intent{}, false, nil
	}
	intent.ProviderPaymentRef = providerPaymentRef
	intent.Status = payments.StatusSucceeded
	intent.UpdatedAt = now
	intent.StateVersion++
	w.put(intent)
	return intent, true, nil
}

// RecordRefund writes the ABSOLUTE refund total the provider reported and moves
// the payment to the status the projection implies, in one statement with the
// same CAS — including the adapter's two money-shaped guards: the write is
// monotone (`refunded < reported`) and it stays inside the capture
// (`reported <= captured`). Reproducing both here is the point of the fake:
// the application-level tests that cover this path are the only ones that run
// without a database, so a fake that accumulated a delta would let the very
// defect this member was rewritten to remove pass every test.
//
// `uncovered` is written WHOLE for the same reason and by the same argument: it
// is the shortfall measured against the balance as it stands, and a fake that
// added it to what the row already held would reproduce the drift the adapter's
// own comment names — a per-delivery figure summed into a cumulative column,
// letting every delivery spend the same balance again.
func (f fakePaymentIntents) RecordRefund(_ context.Context, id payments.IntentID, refundRef string, totalRefunded, uncovered int64, from []payments.Status, to payments.Status, now time.Time) (payments.Intent, bool, error) {
	w := f.world
	w.record("record-refund")
	w.refundRefs = append(w.refundRefs, refundRef)
	w.refundAmounts = append(w.refundAmounts, totalRefunded)
	w.refundUncovered = append(w.refundUncovered, uncovered)
	intent, ok := w.intents[id]
	if !ok {
		return payments.Intent{}, false, persistence.ErrNotFound
	}
	if !hasPaymentsStatus(from, intent.Status) {
		return payments.Intent{}, false, nil
	}
	if intent.RefundedMinorUnits >= totalRefunded || totalRefunded > intent.AmountMinorUnits {
		return payments.Intent{}, false, nil
	}
	intent.RefundedMinorUnits = totalRefunded
	intent.UncoveredRefundMinorUnits = uncovered
	intent.Status = to
	intent.UpdatedAt = now
	intent.StateVersion++
	w.put(intent)
	return intent, true, nil
}

// ListForAccount is the account predicate in the query, not a filter applied to
// rows already fetched: it records what it was ASKED for, so a test can tell
// the two apart.
func (f fakePaymentIntents) ListForAccount(_ context.Context, accountID string, page persistence.PaymentPage) ([]payments.Intent, error) {
	w := f.world
	w.listAccounts = append(w.listAccounts, accountID)
	w.listPages = append(w.listPages, page)

	var found []payments.Intent
	for _, intent := range w.intents {
		if intent.AccountID != accountID {
			continue
		}
		// The keyset is the last row handed over, and the list is newest first,
		// so the next page is the rows BELOW it — strictly smaller identifiers,
		// the cursor's own row excluded. Getting this the other way round makes
		// the second page repeat the first, which is exactly the kind of error
		// the trim test exists to catch.
		if page.After != "" && intent.ID >= page.After {
			continue
		}
		found = append(found, intent)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].ID > found[j].ID })
	// One row MORE than asked for, which is what the adapter's own statement
	// does: the probe row is what tells the use case whether another page
	// exists, and a fake that answered exactly the limit would make every page
	// look like the last one.
	if page.Limit > 0 && len(found) > page.Limit+1 {
		found = found[:page.Limit+1]
	}
	return found, nil
}

// setBucketStatus moves a seeded bucket into another state, for the paths that
// refuse on a bucket's own standing rather than on its existence.
func (w *paymentsWorld) setBucketStatus(id accounting.FundingBucketID, status accounting.BucketStatus) {
	if bucket, ok := w.buckets[id]; ok {
		bucket.Status = status
		w.buckets[id] = bucket
	}
}

// setBucketAvailable puts a balance on a seeded bucket, for the refund tests
// that measure a shortfall against money that is actually there. Without it a
// seeded bucket is empty, and an empty bucket makes every cumulative refund
// wholly uncovered — which is a true answer that cannot tell a shortfall
// measured against the payment apart from one measured against a delivery.
func (w *paymentsWorld) setBucketAvailable(id accounting.FundingBucketID, available int64) {
	if bucket, ok := w.buckets[id]; ok {
		bucket.Settled = accounting.Balance(available)
		bucket.Available = accounting.Balance(available)
		w.buckets[id] = bucket
	}
}

// fakePaymentEvents is the idempotency ledger: one row per verified delivery,
// and the uniqueness that makes a redelivery a duplicate rather than a second
// credit.
type fakePaymentEvents struct {
	persistence.PaymentEvents
	world *paymentsWorld
}

func (f fakePaymentEvents) Record(ctx context.Context, event payments.ProviderEventRecord) error {
	w := f.world
	if !inTransaction(ctx) {
		w.outsideTx++
	}
	w.record("record-event")
	key := paymentsEventKey(event)
	if _, seen := w.events[key]; seen {
		return payments.ErrDuplicateEvent
	}
	w.events[key] = event
	return nil
}

// Settle writes the verdict on the row Record wrote, the way the adapter's
// guarded UPDATE does — including its refusal to settle a delivery this unit of
// work never recorded, which is the difference between correcting a verdict and
// inventing one.
func (f fakePaymentEvents) Settle(ctx context.Context, key payments.DeliveryKey, disposition payments.EventDisposition) error {
	w := f.world
	if !inTransaction(ctx) {
		w.outsideTx++
	}
	w.record("settle-event")
	stored, ok := w.events[deliveryKeyString(key)]
	if !ok {
		return persistence.ErrNotFound
	}
	stored.Disposition = disposition
	w.events[deliveryKeyString(key)] = stored
	return nil
}

// fakePaymentQuarantine is the unapplied-delivery ledger. It never surfaces a
// duplicate: quarantines are evidence, so two identical unapplied deliveries
// are two rows.
type fakePaymentQuarantine struct {
	persistence.PaymentQuarantine
	world *paymentsWorld
}

func (f fakePaymentQuarantine) Record(ctx context.Context, record payments.QuarantineRecord) error {
	w := f.world
	if !inTransaction(ctx) {
		w.outsideTx++
	}
	w.record("record-quarantine")
	if err := w.failOn["quarantine.record"]; err != nil {
		return err
	}
	w.quarantines = append(w.quarantines, record)
	return nil
}

// fakePaymentsClock is the database's clock: one instant for the whole unit of
// work, which is what makes every gate in it evaluate against the same now.
type fakePaymentsClock struct {
	persistence.Clock
	world *paymentsWorld
}

func (f fakePaymentsClock) Now(_ context.Context) (time.Time, error) {
	return f.world.now, nil
}

type fakePaymentsAccounts struct {
	persistence.Accounts
	world *paymentsWorld
}

func (f fakePaymentsAccounts) ByID(_ context.Context, id identity.AccountID) (identity.Account, error) {
	account, ok := f.world.accounts[id]
	if !ok {
		return identity.Account{}, persistence.ErrNotFound
	}
	return account, nil
}

type fakeBucketReader struct {
	world *paymentsWorld
}

func (f fakeBucketReader) Bucket(_ context.Context, id accounting.FundingBucketID) (accounting.Bucket, error) {
	bucket, ok := f.world.buckets[id]
	if !ok {
		return accounting.Bucket{}, persistence.ErrNotFound
	}
	return bucket, nil
}

// fakeAccountFunding stands in for the accounting use case that opens a PAYG
// bucket. It is converged like the real one: an account that already has a
// bucket gets it back rather than a second one.
type fakeAccountFunding struct {
	world *paymentsWorld
}

func (f fakeAccountFunding) OpenAccountFunding(ctx context.Context, accountID commerce.AccountID) (accounting.Bucket, error) {
	w := f.world
	if !inTransaction(ctx) {
		w.outsideTx++
	}
	w.record("open-funding")
	if err := w.failOn["funding.open"]; err != nil {
		return accounting.Bucket{}, err
	}
	if id, ok := w.bucketByAccount[accountID]; ok {
		return w.buckets[id], nil
	}
	bucket := accounting.Bucket{
		ID:        accounting.FundingBucketID(fmt.Sprintf("bucket-%d", len(w.buckets)+1)),
		AccountID: accounting.AccountID(accountID),
		Status:    accounting.BucketActive,
		CreatedAt: w.now,
		UpdatedAt: w.now,
	}
	w.buckets[bucket.ID] = bucket
	w.bucketByAccount[accountID] = bucket.ID
	return bucket, nil
}

// fakeTopUpWriter is the one accounting primitive the webhook path holds. It
// records the CONTEXT it was handed, which is the mechanical check for the
// rule this feature is most likely to lose: a credit written with the caller's
// context commits beside the delivery record rather than with it.
type fakeTopUpWriter struct {
	world *paymentsWorld
}

func (f fakeTopUpWriter) TopUp(ctx context.Context, bucketID accounting.FundingBucketID, amountMinorUnits int64, commandKey accounting.CommandKey) (accounting.Bucket, error) {
	w := f.world
	w.creditInTx = append(w.creditInTx, inTransaction(ctx))
	w.creditDepths = append(w.creditDepths, w.unitDepth)
	w.record("topup")
	if w.topUpErr != nil {
		return accounting.Bucket{}, w.topUpErr
	}
	bucket, ok := w.buckets[bucketID]
	if !ok {
		return accounting.Bucket{}, persistence.ErrNotFound
	}
	if bucket.Status != accounting.BucketActive {
		return accounting.Bucket{}, accounting.ErrBucketClosed
	}
	// One leg per command key: a second delivery of one capture converges on
	// the leg already on file rather than booking a second one.
	for _, leg := range w.legs {
		if leg.FundingBucketID == bucketID && leg.CommandKey == commandKey {
			return bucket, nil
		}
	}
	w.legs = append(w.legs, accounting.LedgerEntry{
		FundingBucketID: bucketID,
		Kind:            accounting.KindTopup,
		Amount:          accounting.Amount(amountMinorUnits),
		SettledDelta:    accounting.Delta(amountMinorUnits),
		CommandKey:      commandKey,
		CreatedAt:       w.now,
	})
	bucket.Settled += accounting.Balance(amountMinorUnits)
	bucket.Available += accounting.Balance(amountMinorUnits)
	w.buckets[bucketID] = bucket
	return bucket, nil
}

// fakeCheckoutProvider is the provider's stand-in. It records the depth of the
// open units of work at the moment it was called and whether the context it was
// handed was a transaction's — the two facts the "outside any unit of work"
// property is stated against.
type fakeCheckoutProvider struct {
	world *paymentsWorld
}

func (f fakeCheckoutProvider) OpenCheckout(ctx context.Context, in paymentprovider.CheckoutRequest) (paymentprovider.CheckoutSession, error) {
	w := f.world
	w.providerCalls = append(w.providerCalls, in)
	w.providerInTx = append(w.providerInTx, inTransaction(ctx))
	w.providerDepth = append(w.providerDepth, w.unitDepth)
	w.record("provider")
	if w.onProviderCall != nil {
		w.onProviderCall(len(w.providerCalls))
	}
	if w.providerErr != nil {
		return paymentprovider.CheckoutSession{}, w.providerErr
	}
	return w.providerSession, nil
}
