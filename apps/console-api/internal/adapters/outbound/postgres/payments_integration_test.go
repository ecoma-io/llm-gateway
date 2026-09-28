//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/payments"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The payment integration's three repositories against the real `control`
// database, on the same terms as the identity, commerce and accounting suites:
// the fake-driver tests pin which handle a statement runs on and how a
// constraint name becomes the port's vocabulary; only PostgreSQL can answer the
// rest — that the guarded moves are single statements whose verdicts are true
// exactly once, that a delivery's uniqueness is the schema's key and not a
// message, that an absent amount and an amount of zero are different rows, and
// that a delivery recorded beside a ledger leg comes back out of a failed unit
// of work with the leg and the money.
//
// That last one is the reason this file exists at all. The atomicity claim —
// a provider delivery and the funding leg it licenses commit together — is
// made in ports/outbound/persistence/payments.go, asserted by the application,
// and provable NOWHERE but here: a unit test over fakes has two fakes, and two
// fakes calling each other's methods is not a transaction. The test below
// records a delivery and appends a real ledger leg in one unit of work, fails
// that unit of work deliberately, and then asks the DATABASE whether any of it
// survived.
//
// Conventions this file holds, copied from the accounting suite because they
// are the tier's and not this file's invention: no t.Parallel anywhere (the
// database is shared); every id, key, reference and provider namespace is
// run-unique, so a rerun against an already-migrated database cannot trip over
// an earlier run's rows — leftovers are tolerated, never depended on; fixtures
// reach their states through the DOMAIN and the PORTS wherever a port path
// exists, because a fixture the domain built is one more proof the states
// compose; and nothing here deletes, because the schema has no delete path for
// any of these three tables and a DELETE would be refused by an engine guard
// anyway.
//
// Run (the compose project is port-shifted per worktree; the project name must
// be one no other worktree is using):
//
//	GATEWAY_POSTGRES_PROJECT=b15-payments GATEWAY_POSTGRES_PORT=55460 \
//	  docker compose -f deploy/postgres/compose.yaml up -d --wait
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:55460/postgres?sslmode=disable' \
//	  go test -tags=integration ./internal/adapters/outbound/postgres

// paymentRepos gathers the pool and the three payment repositories over it, as
// the ports the use cases see them, plus the accounting suite's repositories —
// because a payment funds a BUCKET, and the bucket, its owner account and its
// ledger are accounting's and identity's rows before they are a payment's.
type paymentRepos struct {
	db         *sql.DB
	store      persistence.Store
	intents    persistence.PaymentIntents
	events     persistence.PaymentEvents
	quarantine persistence.PaymentQuarantine
	accounting *accountingRepos
}

// integrationPayments opens the migrated control database and builds the three
// repositories over it.
func integrationPayments(t *testing.T) *paymentRepos {
	t.Helper()
	acct := integrationAccounting(t)
	return &paymentRepos{
		db:         acct.db,
		store:      acct.store,
		intents:    NewPaymentIntents(acct.store),
		events:     NewPaymentEvents(acct.store),
		quarantine: NewPaymentQuarantine(acct.store),
		accounting: acct,
	}
}

// ---------------------------------------------------------------------------
// fixtures — accounts, buckets, payments and the two evidence rows, every one
// fatal-on-error and every one run-unique.
// ---------------------------------------------------------------------------

// paymentFixture is one test's own corner of the schema: an account with a PAYG
// bucket, and a provider namespace no other test or run shares.
//
// The provider namespace is per-fixture and not global because the schema's
// provider-scoped uniqueness — the capture reference's partial unique index and
// the delivery key — is keyed on it. Two tests that shared a provider would
// share that key space, and a test that reused a capture reference across
// fixtures would be refused by the schema for a reason that had nothing to do
// with what it was testing.
type paymentFixture struct {
	provider string
	account  string
	bucket   accounting.Bucket
}

// payProvider mints a provider namespace no earlier run has used. The alphabet
// is the schema's only requirement for the column (one to sixty-four
// characters) and the prefix makes the row legible in a failure message.
func payProvider(t *testing.T) string {
	t.Helper()
	id, err := payments.NewIntent(time.Now().UTC())
	if err != nil {
		t.Fatalf("mint a provider namespace: %v", err)
	}
	return "it-pay-" + strings.ReplaceAll(string(id), "-", "")
}

// payKey mints a caller's idempotency key, run-unique and bounded well under
// the column's 128.
func payKey(t *testing.T, prefix string) string {
	t.Helper()
	id, err := payments.NewIntent(time.Now().UTC())
	if err != nil {
		t.Fatalf("mint an idempotency key: %v", err)
	}
	return prefix + "-" + strings.ReplaceAll(string(id), "-", "")
}

// payRef mints a provider's own reference — a checkout, a capture or a refund —
// run-unique under the fixture's provider namespace.
func payRef(t *testing.T, prefix string) string {
	t.Helper()
	id, err := payments.NewIntent(time.Now().UTC())
	if err != nil {
		t.Fatalf("mint a provider reference: %v", err)
	}
	return prefix + "_" + string(id)
}

// newPaymentFixture opens one account's PAYG bucket through the accounting
// suite's own fixture — identity account first, because every foreign key here
// is RESTRICT and the owner must exist — and gives it this run's provider
// namespace.
func (p *paymentRepos) newPaymentFixture(t *testing.T, name string) paymentFixture {
	t.Helper()
	bucket := p.accounting.newAccountBucket(t, name)
	return paymentFixture{
		provider: payProvider(t),
		account:  string(bucket.AccountID),
		bucket:   bucket,
	}
}

// openIntent opens a payment at the instant it is told and writes it through
// the port, inside the unit of work the port requires.
//
// `at` is a parameter rather than a call to time.Now for the LIST tests, which
// need several payments in a known order: a payment id is version-7, so its
// leading bits are the millisecond its minter was given, and two ids minted
// inside the same millisecond order by their random tails rather than by
// creation. Passing explicit, strictly increasing instants is what makes
// "newest first" a fact these tests can assert instead of a race they tolerate.
// The instants are truncated to the precision timestamptz keeps, so the row
// this fixture builds and the row the database hands back compare equal.
func (p *paymentRepos) openIntent(t *testing.T, f paymentFixture, key string, at time.Time) payments.Intent {
	t.Helper()
	at = micros(at)
	id, err := payments.NewIntent(at)
	if err != nil {
		t.Fatalf("NewIntent: %v", err)
	}
	intent, err := payments.New(payments.NewPayment{
		AccountID:         f.account,
		FundingBucketID:   string(f.bucket.ID),
		AmountMinorUnits:  5000,
		Currency:          "USD",
		MinorUnitExponent: 2,
		Provider:          f.provider,
		IdempotencyKey:    key,
		CheckoutTTL:       time.Hour,
		Now:               at,
		MintedID:          id,
		MintedAt:          at,
	})
	if err != nil {
		t.Fatalf("New payment: %v", err)
	}
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		return p.intents.Create(ctx, intent)
	}); err != nil {
		t.Fatalf("create payment %s: %v", intent.ID, err)
	}
	return intent
}

// openCaptureReady takes a payment as far as a capture can move it from: created
// → checkout_open (through the port's own guarded move, so the fixture's state
// is one the state machine admits) with a checkout reference a delivery could
// resolve.
func (p *paymentRepos) openCaptureReady(t *testing.T, intent payments.Intent) payments.Intent {
	t.Helper()
	checkout := payments.OpenedCheckout{URL: "https://checkout.example.test/" + payRef(t, "url"), ProviderRef: payRef(t, "cs")}
	var applied bool
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		var err error
		applied, err = p.intents.RecordCheckout(ctx, intent.ID, checkout,
			[]payments.Status{payments.StatusCreated}, micros(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("record checkout for payment %s: %v", intent.ID, err)
	}
	if !applied {
		t.Fatalf("record checkout for payment %s did not apply", intent.ID)
	}
	return p.mustByID(t, intent.ID)
}

// captured takes a payment through checkout and capture, leaving it succeeded
// with the provider's payment reference set — the state a refund starts from.
func (p *paymentRepos) captured(t *testing.T, f paymentFixture, key string) (payments.Intent, string) {
	t.Helper()
	intent := p.openCaptureReady(t, p.openIntent(t, f, key, time.Now().UTC()))
	paymentRef := payRef(t, "pi")
	return p.recordCapture(t, intent, paymentRef), paymentRef
}

func (p *paymentRepos) recordCapture(t *testing.T, intent payments.Intent, paymentRef string) payments.Intent {
	t.Helper()
	var (
		captured payments.Intent
		applied  bool
	)
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		var err error
		captured, applied, err = p.intents.RecordCapture(ctx, intent.ID, paymentRef,
			[]payments.Status{payments.StatusCheckoutOpen, payments.StatusRequiresAction,
				payments.StatusExpired, payments.StatusCancelled}, micros(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("record capture for payment %s: %v", intent.ID, err)
	}
	if !applied {
		t.Fatalf("record capture for payment %s did not apply", intent.ID)
	}
	return captured
}

// mustByID re-reads a payment through the port, failing the test when the read
// fails — the read every assertion about "what the row now shows" goes through.
func (p *paymentRepos) mustByID(t *testing.T, id payments.IntentID) payments.Intent {
	t.Helper()
	intent, err := p.intents.ByID(t.Context(), id)
	if err != nil {
		t.Fatalf("read payment %s: %v", id, err)
	}
	return intent
}

// event builds a delivery this plane would record as applied: it names the
// payment, the provider's own event id, the kind, and what it claimed.
func (p *paymentRepos) event(t *testing.T, f paymentFixture, intent payments.Intent, eventID, paymentRef string) payments.ProviderEventRecord {
	t.Helper()
	return payments.ProviderEventRecord{
		EventID:            eventID,
		ProviderAccountKey: "acct-" + f.provider,
		Provider:           f.provider,
		IntentID:           intent.ID,
		Kind:               "payment_intent.succeeded",
		ProviderPaymentRef: paymentRef,
		AmountMinorUnits:   int64Pointer(5000),
		Currency:           "USD",
		Disposition:        payments.DispositionApplied,
		OccurredAt:         micros(time.Now().UTC()),
	}
}

// int64Pointer is the domain's spelling of "an amount was stated", and the
// pointer is the point: a fixture that passed 0 would be a delivery that
// claimed nothing was charged.
func int64Pointer(value int64) *int64 { return &value }

// sameIntent compares two payments as ROWS: every field exactly, and the three
// instants as instants.
//
// The instants are the one field a struct comparison cannot settle.
// `timestamptz` comes back from the server in the server's own offset — the
// CI cluster runs UTC and this suite's developer cluster runs +07 — while the
// fixture that built the row minted its instants in UTC, so `got == want` would
// fail on the LOCATION of a point that is the same point. The house convention
// is the commerce suite's: micros().Equal(), which compares instants at the
// precision the column keeps. Everything else is still compared with ==, and
// that is the property worth having: a field this adapter translated wrongly
// cannot hide behind a lenient comparison.
func sameIntent(got, want payments.Intent) bool {
	if !micros(got.CreatedAt).Equal(micros(want.CreatedAt)) ||
		!micros(got.UpdatedAt).Equal(micros(want.UpdatedAt)) ||
		!micros(got.ExpiresAt).Equal(micros(want.ExpiresAt)) {
		return false
	}
	got.CreatedAt, got.UpdatedAt, got.ExpiresAt = time.Time{}, time.Time{}, time.Time{}
	want.CreatedAt, want.UpdatedAt, want.ExpiresAt = time.Time{}, time.Time{}, time.Time{}
	return got == want
}

// ---------------------------------------------------------------------------
// the intent round trip and the two lookups a delivery can arrive through.
// ---------------------------------------------------------------------------

// TestIntegrationPaymentIntentRoundTripsThroughEveryLookup is the read half of
// the port: one payment, written through Create and read back through all three
// of its keys.
//
// The whole row is compared, not a field of it, and the comparison is possible
// because every column here is one this fixture chose: an absent checkout
// reference and an absent capture reference are NULL in the schema and "" in the
// domain, and a round trip that turned either into a zero or into the string
// "null" would fail this test rather than pass it.
func TestIntegrationPaymentIntentRoundTripsThroughEveryLookup(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-roundtrip")
	want := p.openIntent(t, f, payKey(t, "roundtrip"), time.Now().UTC())

	reads := map[string]func() (payments.Intent, error){
		"ByID": func() (payments.Intent, error) { return p.intents.ByID(t.Context(), want.ID) },
		"ByAccountAndIdempotencyKey": func() (payments.Intent, error) {
			return p.intents.ByAccountAndIdempotencyKey(t.Context(), f.account, want.IdempotencyKey)
		},
	}
	for name, read := range reads {
		got, err := read()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !sameIntent(got, want) {
			t.Errorf("%s:\n got  %+v\n want %+v", name, got, want)
		}
	}

	// Every read above ran with the test's own context — no unit of work
	// anywhere — and answered. That is the port's asymmetry, asserted rather
	// than described: the WRITE members refuse a context carrying no
	// transaction, and a read does not, because a read outside a unit of work
	// sees a committed state, which is a correct answer to a correct question.
	//
	// A reference nothing opened is ErrNotFound and only that. The distinction
	// is load-bearing on the webhook path: "this platform never opened that
	// checkout" quarantines a delivery, while any other failure is
	// infrastructure and must stay distinguishable from it.
	unknown := payRef(t, "cs-unknown")
	if _, err := p.intents.ByProviderCheckoutRef(t.Context(), f.provider, unknown); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("ByProviderCheckoutRef(unknown) error = %v, want persistence.ErrNotFound", err)
	}
	unknownID, err := payments.NewIntent(time.Now().UTC())
	if err != nil {
		t.Fatalf("NewIntent: %v", err)
	}
	if _, err := p.intents.ByID(t.Context(), unknownID); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("ByID(unknown) error = %v, want persistence.ErrNotFound", err)
	}
	if _, err := p.intents.ByAccountAndIdempotencyKey(t.Context(), f.account, payKey(t, "never-used")); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("ByAccountAndIdempotencyKey(unknown) error = %v, want persistence.ErrNotFound", err)
	}

	// The checkout reference lookup is the read the whole webhook resolution
	// rests on, so it is exercised for real once the reference exists.
	opened := p.openCaptureReady(t, want)
	if got, err := p.intents.ByProviderCheckoutRef(t.Context(), f.provider, opened.ProviderCheckoutRef); err != nil {
		t.Errorf("ByProviderCheckoutRef(opened) error = %v", err)
	} else if got.ID != opened.ID {
		t.Errorf("ByProviderCheckoutRef resolved %s, want %s", got.ID, opened.ID)
	}
}

// TestIntegrationPaymentIntentIdempotencyKeyIsOnePaymentPerAccount is the
// schema's (account_id, idempotency_key) uniqueness arriving as the domain's
// sentinel, and the account half of the key asserted rather than assumed: the
// same key under a different account is a different payment, because two
// customers clicking the same button on the same afternoon is not a duplicate.
func TestIntegrationPaymentIntentIdempotencyKeyIsOnePaymentPerAccount(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-idempotency")
	other := p.newPaymentFixture(t, "payments-idempotency-other")
	key := payKey(t, "same-key")
	first := p.openIntent(t, f, key, time.Now().UTC())

	// A second payment under the same account and key is refused, and the
	// refusal is the sentinel — not a driver message the use case would have to
	// pattern-match to converge on the payment it already has.
	at := micros(time.Now().UTC()).Add(time.Minute)
	secondID, err := payments.NewIntent(at)
	if err != nil {
		t.Fatalf("NewIntent: %v", err)
	}
	second, err := payments.New(payments.NewPayment{
		AccountID: first.AccountID, FundingBucketID: first.FundingBucketID,
		AmountMinorUnits: 5000, Currency: "USD", MinorUnitExponent: 2,
		Provider: f.provider, IdempotencyKey: key, CheckoutTTL: time.Hour,
		Now: at, MintedID: secondID, MintedAt: at,
	})
	if err != nil {
		t.Fatalf("New payment: %v", err)
	}
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		return p.intents.Create(ctx, second)
	}); !errors.Is(err, payments.ErrDuplicatePayment) {
		t.Errorf("second Create under one key error = %v, want payments.ErrDuplicatePayment", err)
	}
	if _, err := p.intents.ByID(t.Context(), second.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("the refused payment is on file: error = %v, want persistence.ErrNotFound", err)
	}

	// The same key under another account is a payment of its own.
	if got := p.openIntent(t, other, key, time.Now().UTC()); got.IdempotencyKey != key {
		t.Errorf("the other account's payment carries key %q, want %q", got.IdempotencyKey, key)
	}
}

// ---------------------------------------------------------------------------
// the four guarded moves: one statement each, a verdict, and a row that says
// what happened.
// ---------------------------------------------------------------------------

// TestIntegrationPaymentCheckoutIsRecordedOnceAndTheLoserIsToldSo is the
// compare-and-swap as the port describes it: the winner's write lands, and the
// loser is answered with false and NO error — because a second attempt at the
// same checkout is not a failure of this platform, it is the world having moved
// while the caller was deciding.
//
// The loser's write is proved not to have landed by re-reading the row: a
// verdict of false that had nevertheless written something would be worse than
// an error, and only the row can tell the two apart.
func TestIntegrationPaymentCheckoutIsRecordedOnceAndTheLoserIsToldSo(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-checkout")
	intent := p.openIntent(t, f, payKey(t, "checkout"), time.Now().UTC())

	first := payments.OpenedCheckout{URL: "https://checkout.example.test/first", ProviderRef: payRef(t, "cs-first")}
	var applied bool
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		var err error
		applied, err = p.intents.RecordCheckout(ctx, intent.ID, first,
			[]payments.Status{payments.StatusCreated}, micros(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("RecordCheckout: %v", err)
	}
	if !applied {
		t.Fatal("RecordCheckout applied = false, want true for the payment's first checkout")
	}

	won := p.mustByID(t, intent.ID)
	if won.Status != payments.StatusCheckoutOpen {
		t.Errorf("status = %q, want %q", won.Status, payments.StatusCheckoutOpen)
	}
	if won.ProviderCheckoutRef != first.ProviderRef || won.CheckoutURL != first.URL {
		t.Errorf("checkout = (%q, %q), want (%q, %q)", won.ProviderCheckoutRef, won.CheckoutURL, first.ProviderRef, first.URL)
	}
	// The version is bumped by the statement, not supplied by the caller: the
	// caller has no expected version to pass and the column still moves.
	if won.StateVersion != intent.StateVersion+1 {
		t.Errorf("state_version = %d, want %d", won.StateVersion, intent.StateVersion+1)
	}

	// The loser: the row is no longer `created`, so the predicate the caller
	// read no longer holds. False and nil, and the winner's values survive.
	lost := payments.OpenedCheckout{URL: "https://checkout.example.test/second", ProviderRef: payRef(t, "cs-second")}
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		var err error
		applied, err = p.intents.RecordCheckout(ctx, intent.ID, lost,
			[]payments.Status{payments.StatusCreated}, micros(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("the losing RecordCheckout returned an error: %v", err)
	}
	if applied {
		t.Error("the losing RecordCheckout applied = true, want false")
	}
	still := p.mustByID(t, intent.ID)
	if !sameIntent(still, won) {
		t.Errorf("the losing write changed the row:\n got  %+v\n want %+v", still, won)
	}
}

// TestIntegrationPaymentMoveStatusCarriesItsStatusPredicate exercises the
// general move: a legal edge lands and returns the row it wrote, and an edge
// the caller read from a state the row is no longer in loses without error.
func TestIntegrationPaymentMoveStatusCarriesItsStatusPredicate(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-move")
	intent := p.openCaptureReady(t, p.openIntent(t, f, payKey(t, "move"), time.Now().UTC()))

	var (
		moved   payments.Intent
		applied bool
	)
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		var err error
		moved, applied, err = p.intents.MoveStatus(ctx, intent.ID,
			[]payments.Status{payments.StatusCheckoutOpen}, payments.StatusRequiresAction, micros(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("MoveStatus: %v", err)
	}
	if !applied {
		t.Fatal("MoveStatus applied = false, want true for a legal edge from the state the caller read")
	}
	// RETURNING is what the moved value is: the caller never re-reads to learn
	// what its own write did.
	if moved.Status != payments.StatusRequiresAction || moved.StateVersion != intent.StateVersion+1 {
		t.Errorf("returned row = (%q, version %d), want (%q, version %d)",
			moved.Status, moved.StateVersion, payments.StatusRequiresAction, intent.StateVersion+1)
	}

	// A move from a state the row has left is false, not an error: the caller
	// re-reads and returns the winner's payment, as Claim's contract says.
	applied = true
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		var err error
		_, applied, err = p.intents.MoveStatus(ctx, intent.ID,
			[]payments.Status{payments.StatusCreated}, payments.StatusCancelled, micros(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("the losing MoveStatus returned an error: %v", err)
	}
	if applied {
		t.Error("MoveStatus from a state the row has left applied = true, want false")
	}
	if after := p.mustByID(t, intent.ID); !sameIntent(after, moved) {
		t.Errorf("the losing move changed the row:\n got  %+v\n want %+v", after, moved)
	}
}

// TestIntegrationPaymentCaptureWritesTheReferenceAndTheStatusTogether is the
// one statement the schema's biconditional forces, and the reason it must be
// one statement rather than two: `payment_intents_capture_shape` refuses a
// payment whose status says captured while its reference says nothing was, so a
// capture that wrote the status and the reference in two statements would be
// refused between them — and the transition trigger refuses status = succeeded
// with a NULL reference for the same reason.
//
// The row that comes back is the proof both halves landed: one statement, one
// verdict, and the reference the funding leg's command key is derived from.
func TestIntegrationPaymentCaptureWritesTheReferenceAndTheStatusTogether(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-capture")
	intent := p.openCaptureReady(t, p.openIntent(t, f, payKey(t, "capture"), time.Now().UTC()))
	paymentRef := payRef(t, "pi")

	var (
		captured payments.Intent
		applied  bool
	)
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		var err error
		captured, applied, err = p.intents.RecordCapture(ctx, intent.ID, paymentRef,
			[]payments.Status{payments.StatusCheckoutOpen, payments.StatusRequiresAction,
				payments.StatusExpired, payments.StatusCancelled}, micros(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("RecordCapture: %v", err)
	}
	if !applied {
		t.Fatal("RecordCapture applied = false, want true")
	}
	if captured.Status != payments.StatusSucceeded || captured.ProviderPaymentRef != paymentRef {
		t.Errorf("captured row = (%q, ref %q), want (%q, ref %q)",
			captured.Status, captured.ProviderPaymentRef, payments.StatusSucceeded, paymentRef)
	}
	if captured.StateVersion != intent.StateVersion+1 {
		t.Errorf("state_version = %d, want %d", captured.StateVersion, intent.StateVersion+1)
	}

	// A redelivery of the same capture loses on the status predicate — the row
	// is no longer in any of the states the caller read — and the reference is
	// left where the first capture put it, which is what makes the funding
	// command key derived from it converge on ONE leg rather than write a
	// second.
	applied = true
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		var err error
		_, applied, err = p.intents.RecordCapture(ctx, intent.ID, payRef(t, "pi-second"),
			[]payments.Status{payments.StatusCheckoutOpen}, micros(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("the second RecordCapture returned an error: %v", err)
	}
	if applied {
		t.Error("a second capture applied = true, want false")
	}
	if after := p.mustByID(t, intent.ID); !sameIntent(after, captured) {
		t.Errorf("the second capture changed the row:\n got  %+v\n want %+v", after, captured)
	}

	// One capture is one credit: the provider's payment reference is unique per
	// provider while it is set, so a SECOND payment cannot claim the same
	// capture. The refusal is the schema's index, not a verdict — the caller
	// has nothing to re-read, because an event naming a capture that belongs to
	// another payment is a contradiction no retry resolves.
	other := p.openCaptureReady(t, p.openIntent(t, f, payKey(t, "capture-other"), time.Now().UTC()))
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		_, _, err := p.intents.RecordCapture(ctx, other.ID, paymentRef,
			[]payments.Status{payments.StatusCheckoutOpen}, micros(time.Now().UTC()))
		return err
	}); err == nil {
		t.Error("a second payment captured the same provider reference; want the unique index to refuse it")
	}
}

// TestIntegrationPaymentRefundIsWrittenAbsolutely is the projection the port
// insists is the PROVIDER'S figure rather than a value this caller computed:
// two refunds in sequence leave the second reported total on the row, and the
// answer is read back from the row rather than from the arguments passed.
//
// The distinction this pins is between 800 and 1200. A `charge.refunded`
// reports the charge's cumulative `amount_refunded`, so the second delivery of
// a 400-then-800 refund pair says 800 — and a statement that ADDED its argument
// to the row would store 400 + 800 = 1200 against a 5000 capture, pass every
// CHECK, and be uncorrectable: every later report would be below what the row
// holds. See the next test for what that costs under concurrency.
func TestIntegrationPaymentRefundIsWrittenAbsolutely(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-refund")
	intent, _ := p.captured(t, f, payKey(t, "refund"))

	partial := p.refund(t, intent, payRef(t, "re"), 2000, 0,
		[]payments.Status{payments.StatusSucceeded}, payments.StatusPartiallyRefunded)
	if partial.RefundedMinorUnits != 2000 || partial.UncoveredRefundMinorUnits != 0 {
		t.Errorf("after the first refund, refunded = %d and uncovered = %d, want 2000 and 0",
			partial.RefundedMinorUnits, partial.UncoveredRefundMinorUnits)
	}
	if partial.Status != payments.StatusPartiallyRefunded {
		t.Errorf("status = %q, want %q", partial.Status, payments.StatusPartiallyRefunded)
	}

	// The second delivery reports the CUMULATIVE 3500, not its own 1500, and
	// the uncovered figure crosses the same way: 500 is the part of the 3500
	// this payment's balance cannot account for, measured by the caller against
	// the cumulative total, and written whole. A delta-shaped member would have
	// been handed "the part of THIS delivery the balance could not cover" and
	// stored it added to whatever the row already held, which is the drift this
	// member's doc names — refunds of 40 then 30 out of a balance of 10 storing
	// 50 where the payment's shortfall is 60.
	second := p.refund(t, partial, payRef(t, "re"), 3500, 500,
		[]payments.Status{payments.StatusPartiallyRefunded}, payments.StatusPartiallyRefunded)
	if second.RefundedMinorUnits != 3500 || second.UncoveredRefundMinorUnits != 500 {
		t.Errorf("after the second refund, refunded = %d and uncovered = %d, want 3500 and 500 — the caller's cumulative total and the shortfall measured against it are both written whole",
			second.RefundedMinorUnits, second.UncoveredRefundMinorUnits)
	}

	// The final report takes the payment to `refunded`, and the reference a
	// refund leg would carry is not lost: it is on the delivery row that claims
	// the refund, which is where the port's events repository records it.
	final := p.refund(t, second, payRef(t, "re"), 5000, 0,
		[]payments.Status{payments.StatusPartiallyRefunded}, payments.StatusRefunded)
	if final.RefundedMinorUnits != 5000 || final.Status != payments.StatusRefunded {
		t.Errorf("after the final refund, refunded = %d and status = %q, want 5000 and %q",
			final.RefundedMinorUnits, final.Status, payments.StatusRefunded)
	}
}

// TestIntegrationPaymentConcurrentRefundDeliveriesConvergeOnOneTotal is the
// over-count proof, and the reason the write is a monotone move to a reported
// figure rather than an increment.
//
// Two writers record the same reported total at the same time. Both must be
// able to run — one wins the row lock and writes, the other finds the guard
// already satisfied and writes nothing — and whatever the interleaving, the row
// ends at the figure the provider reported. It cannot end above it, which is
// the property that matters: an incrementing statement handed a delta derived
// from a read over-counts here into a state nothing can correct, because every
// later report would then be BELOW what the row holds and the delivery that
// would fix it reads as a contradiction.
//
// The second writer's answer is `false`, and that is not a failure. `false`
// means "this delivery added nothing", and the application tells the two causes
// apart by re-reading — a figure already stored is a satisfied claim and is
// answered applied.
func TestIntegrationPaymentConcurrentRefundDeliveriesConvergeOnOneTotal(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-refund-race")
	intent, _ := p.captured(t, f, payKey(t, "refund-race"))

	type outcome struct {
		refunded int64
		applied  bool
		err      error
	}
	// The refund references are minted BEFORE the goroutines start: a mint
	// helper that fails calls t.Fatalf, and t.Fatalf belongs to the test's own
	// goroutine — a t.Fatalf from a worker is a vet failure and a half-run test.
	refs := []string{payRef(t, "re-race"), payRef(t, "re-race")}
	results := make(chan outcome, len(refs))
	var start sync.WaitGroup
	start.Add(1)
	for _, ref := range refs {
		go func(ref string) {
			start.Wait()
			var (
				refunded payments.Intent
				applied  bool
			)
			// The two writers share no context and no transaction: each one's
			// statement blocks on the row lock the other holds, and the second
			// re-evaluates its predicate against the row the first committed.
			err := p.store.WithinTx(context.Background(), func(ctx context.Context) error {
				var txErr error
				refunded, applied, txErr = p.intents.RecordRefund(ctx, intent.ID, ref, 2000, 0,
					[]payments.Status{payments.StatusSucceeded, payments.StatusPartiallyRefunded},
					payments.StatusPartiallyRefunded, micros(time.Now().UTC()))
				return txErr
			})
			results <- outcome{refunded: refunded.RefundedMinorUnits, applied: applied, err: err}
		}(ref)
	}
	start.Done()

	applied := 0
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatalf("a concurrent refund failed: %v", got.err)
		}
		if got.applied {
			applied++
			if got.refunded != 2000 {
				t.Errorf("the winning writer returned refunded = %d, want the 2000 it reported", got.refunded)
			}
		}
	}
	if applied != 1 {
		t.Errorf("%d of the two writers moved the row, want exactly one: the second names a total the row already holds", applied)
	}
	after := p.mustByID(t, intent.ID)
	if after.RefundedMinorUnits != 2000 {
		t.Errorf("refunded = %d after two concurrent deliveries of the same 2000 total, want 2000 — the write must be the reported figure and never a sum of the two deliveries", after.RefundedMinorUnits)
	}
	if after.StateVersion != intent.StateVersion+1 {
		t.Errorf("state_version = %d after one effective refund, want %d", after.StateVersion, intent.StateVersion+1)
	}
}

// TestIntegrationPaymentRefundGuardsAreVerdictsNotErrors covers the two
// money-shaped predicates the adapter adds to the refund statement, beside the
// statuses the caller read. Neither can fire as an ERROR, and that is the
// property being pinned:
//
// A ceiling breach caught by the schema's refunded_within_capture CHECK is a
// constraint violation, which reaches the provider as a 5xx and a retry of
// bytes the same statement will refuse again, forever. A repeat of a total the
// row already holds caught by nothing would be a second write of the same
// figure, bumping state_version for no change.
//
// Stated in the WHERE clause, both fire zero rows — false and no error — and
// the application already answers a false verdict by re-reading the row and
// either converging or quarantining, then returning 2xx. A verdict is what the
// caller can act on.
func TestIntegrationPaymentRefundGuardsAreVerdictsNotErrors(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-refund-ceiling")
	intent, _ := p.captured(t, f, payKey(t, "refund-ceiling"))

	allowed := p.refund(t, intent, payRef(t, "re"), 4000, 0,
		[]payments.Status{payments.StatusSucceeded}, payments.StatusPartiallyRefunded)

	for _, refusal := range []struct {
		name      string
		reported  int64
		to        payments.Status
		uncovered int64
	}{
		// Past the 5000 capture: the schema's CHECK would have fired here.
		{name: "a total above the capture", reported: 6000, to: payments.StatusRefunded},
		// A figure the row already holds: this is the ordinary redelivery of an
		// older or repeated report, and it must change nothing at all.
		{name: "a total the row already holds", reported: 4000, to: payments.StatusPartiallyRefunded},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			var (
				refused payments.Intent
				applied bool
			)
			if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
				var err error
				refused, applied, err = p.intents.RecordRefund(ctx, allowed.ID, payRef(t, "re"), refusal.reported, refusal.uncovered,
					[]payments.Status{payments.StatusSucceeded, payments.StatusPartiallyRefunded},
					refusal.to, micros(time.Now().UTC()))
				return err
			}); err != nil {
				t.Fatalf("the refusal returned an error (%v), want the predicate to answer with false", err)
			}
			if applied {
				t.Error("applied = true, want false")
			}
			if refused != (payments.Intent{}) {
				t.Errorf("a refused refund returned a row: %+v", refused)
			}
			if after := p.mustByID(t, allowed.ID); !sameIntent(after, allowed) {
				t.Errorf("the refused refund changed the row:\n got  %+v\n want %+v", after, allowed)
			}
		})
	}
}

// TestIntegrationPaymentConvergencePathsSurviveTheirOwnTransaction is the
// regression test for the two paths that read a unique violation as a SIGNAL.
//
// Both of this plane's convergence routes are built on catching a unique
// violation and then doing more work in the same unit of work: a repeated
// open-checkout request re-reads the payment it already has, and a redelivered
// provider event is answered 200 with nothing written. In PostgreSQL a failed
// statement ABORTS the transaction it fired in — every statement after it
// answers SQLSTATE 25P02, and the commit at the end fails outright, so a scope
// that catches the violation and returns nil does not converge; it 500s. Both
// statements are therefore bracketed in a savepoint that is rolled back to
// before the collision is classified.
//
// This test is shaped to catch exactly that, which is why it does NOT return
// the violation out of the scope. The other duplicate-delivery test in this
// file returns it, and a scope that returns an error never reaches Commit —
// so it passes whether or not the savepoint is there. Here the scope converges:
// it catches the sentinel, does a read afterwards, and commits.
func TestIntegrationPaymentConvergencePathsSurviveTheirOwnTransaction(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-convergence")
	intent := p.openIntent(t, f, payKey(t, "convergence"), time.Now().UTC())
	event := p.event(t, f, intent, payRef(t, "evt"), payRef(t, "pi"))

	// The retry carries a FRESH id, which is what makes this test about the
	// idempotency key at all. A second Create of the same intent STRUCT would
	// collide on the primary key first and answer a bare 23505 on
	// `payment_intents_pkey` — a violation this adapter deliberately does not
	// translate, because two payments sharing an id is a bug to surface and not
	// a convergence, and the read the convergence then does is keyed on
	// (account, key) and could never find such a row anyway. The retry the
	// USE CASE makes mints a new id per attempt, so the constraint it meets is
	// `payment_intents_idempotency_key`, and this fixture reproduces that shape.
	retryAt := micros(time.Now().UTC()).Add(time.Minute)
	retryID, err := payments.NewIntent(retryAt)
	if err != nil {
		t.Fatalf("NewIntent: %v", err)
	}
	retry, err := payments.New(payments.NewPayment{
		AccountID:         intent.AccountID,
		FundingBucketID:   intent.FundingBucketID,
		AmountMinorUnits:  intent.AmountMinorUnits,
		Currency:          intent.Currency,
		MinorUnitExponent: intent.MinorUnitExponent,
		Provider:          f.provider,
		IdempotencyKey:    intent.IdempotencyKey,
		CheckoutTTL:       time.Hour,
		Now:               retryAt,
		MintedID:          retryID,
		MintedAt:          retryAt,
	})
	if err != nil {
		t.Fatalf("New payment: %v", err)
	}
	if retry.ID == intent.ID {
		t.Fatalf("the retry reuses the first payment's id %s; this fixture must not collide on the primary key", retry.ID)
	}

	t.Run("a repeated idempotency key converges on the payment already written", func(t *testing.T) {
		var converged payments.Intent
		err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
			if err := p.intents.Create(ctx, retry); !errors.Is(err, payments.ErrDuplicatePayment) {
				return fmt.Errorf("the repeated create answered %v, want payments.ErrDuplicatePayment", err)
			}
			// The read the convergence is FOR. Without the savepoint this is
			// SQLSTATE 25P02 and the test fails here rather than below.
			found, err := p.intents.ByAccountAndIdempotencyKey(ctx, intent.AccountID, intent.IdempotencyKey)
			if err != nil {
				return fmt.Errorf("re-read the payment the repeated key names: %w", err)
			}
			converged = found
			return nil
		})
		if err != nil {
			t.Fatalf("the repeated create did not converge: %v", err)
		}
		if converged.ID != intent.ID {
			t.Errorf("the repeated key converged on %s, want the payment already written, %s", converged.ID, intent.ID)
		}
	})

	t.Run("a redelivered event is recorded as a duplicate and commits", func(t *testing.T) {
		if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
			return p.events.Record(ctx, event)
		}); err != nil {
			t.Fatalf("the first delivery: %v", err)
		}

		var deliveries int
		err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
			if err := p.events.Record(ctx, event); !errors.Is(err, payments.ErrDuplicateEvent) {
				return fmt.Errorf("the redelivery answered %v, want payments.ErrDuplicateEvent", err)
			}
			// A read after the collision, then a normal return — which is what
			// makes this transaction commit. The application's duplicate
			// branch does exactly this shape, and answers the provider 200.
			recorded, err := p.events.ByIntent(ctx, intent.ID, 10)
			if err != nil {
				return fmt.Errorf("read the deliveries back after the collision: %w", err)
			}
			deliveries = len(recorded)
			return nil
		})
		if err != nil {
			t.Fatalf("the redelivery did not commit: %v", err)
		}
		if deliveries != 1 {
			t.Errorf("the ledger holds %d deliveries, want the one the first write made", deliveries)
		}
	})
}

// refund is the refund path as the application drives it: one unit of work, one
// guarded statement, and the row the statement wrote.
func (p *paymentRepos) refund(t *testing.T, intent payments.Intent, refundRef string, amount, uncovered int64, from []payments.Status, to payments.Status) payments.Intent {
	t.Helper()
	var (
		refunded payments.Intent
		applied  bool
	)
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		var err error
		refunded, applied, err = p.intents.RecordRefund(ctx, intent.ID, refundRef, amount, uncovered, from, to, micros(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("RecordRefund(%s, %d): %v", to, amount, err)
	}
	if !applied {
		t.Fatalf("RecordRefund(%s, %d) applied = false, want true", to, amount)
	}
	return refunded
}

// ---------------------------------------------------------------------------
// the delivery ledger and the quarantine: the dedup key, the nullable claims
// and the bytes.
// ---------------------------------------------------------------------------

// TestIntegrationPaymentEventDeliveryKeyIsOneDeliveryPerProviderAccount is the
// idempotency ledger's whole purpose: the same delivery twice is a duplicate
// and NOT a second credit, and the same event id under another merchant account
// is a different delivery, because a provider that scopes its event ids per
// merchant hands two customers the same id.
//
// The duplicate surfaces as the domain's sentinel and NOT as nil, which is the
// assertion that pins the absence of `ON CONFLICT DO NOTHING`: a repository
// that swallowed the violation would answer every redelivery with success, and
// a caller could no longer tell "already recorded" from "recorded now" — the
// two answers differ in whether the money has moved.
func TestIntegrationPaymentEventDeliveryKeyIsOneDeliveryPerProviderAccount(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-events")
	intent := p.openIntent(t, f, payKey(t, "events"), time.Now().UTC())
	event := p.event(t, f, intent, payRef(t, "evt"), payRef(t, "pi"))

	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		return p.events.Record(ctx, event)
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Identical delivery: the same provider, the same merchant account and the
	// same event id.
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		return p.events.Record(ctx, event)
	}); !errors.Is(err, payments.ErrDuplicateEvent) {
		t.Errorf("the redelivery error = %v, want payments.ErrDuplicateEvent", err)
	}

	// The same event id under another merchant account is another delivery: the
	// dedup key carries the merchant precisely so that a customer is not left
	// unfunded by another customer's event id colliding with theirs.
	elsewhere := event
	elsewhere.ProviderAccountKey = event.ProviderAccountKey + "-other"
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		return p.events.Record(ctx, elsewhere)
	}); err != nil {
		t.Errorf("the same event id under another merchant account error = %v, want it recorded", err)
	}

	// Both deliveries are on file for the payment, newest first by recorded_at
	// with the identity as the tiebreaker. The order is asserted as a set here
	// because the two rows are written by two transactions and a page is read
	// for its contents rather than for the sub-microsecond order the two
	// statements happened to commit in.
	recorded, err := p.events.ByIntent(t.Context(), intent.ID, 10)
	if err != nil {
		t.Fatalf("ByIntent: %v", err)
	}
	if len(recorded) != 2 {
		t.Fatalf("ByIntent returned %d deliveries, want 2", len(recorded))
	}
	seen := map[string]payments.EventDisposition{}
	for _, got := range recorded {
		seen[got.ProviderAccountKey+"/"+got.EventID] = got.Disposition
	}
	for _, want := range []string{event.ProviderAccountKey, elsewhere.ProviderAccountKey} {
		disposition, ok := seen[want+"/"+event.EventID]
		if !ok {
			t.Errorf("delivery %s/%s was not read back", want, event.EventID)
			continue
		}
		if disposition != payments.DispositionApplied {
			t.Errorf("delivery %s/%s has disposition %q, want %q", want, event.EventID, disposition, payments.DispositionApplied)
		}
	}

	// A delivery that resolved to no payment is readable too, and it is
	// readable through the payment that did not exist while it was recorded:
	// the empty id matches nothing rather than everything.
	none, err := p.events.ByIntent(t.Context(), payments.IntentID(""), 10)
	if err != nil {
		t.Fatalf("ByIntent with no payment: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("ByIntent with no payment returned %d deliveries, want none", len(none))
	}
}

// TestIntegrationPaymentEventAmountsRoundTripAbsence is the domain's "absent is
// not zero" doctrine made mechanical at the storage boundary: a delivery that
// claimed no amount is NULL and reads back as nil, and a delivery that claimed
// zero is a zero and reads back as a pointer to zero.
//
// The two rows are the same payment, the same provider and the same kind, and
// they differ in exactly the one field. A scan that collapsed NULL into 0 would
// make them indistinguishable, and every comparison above would then be
// comparing a fact with a non-fact.
func TestIntegrationPaymentEventAmountsRoundTripAbsence(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-events-amount")
	intent := p.openIntent(t, f, payKey(t, "events-amount"), time.Now().UTC())

	silent := p.event(t, f, intent, payRef(t, "evt-silent"), payRef(t, "pi"))
	silent.AmountMinorUnits = nil
	silent.Currency = ""
	// The third absence on the same row, and the one a NOT NULL column would
	// have taken and stored as year 1: a signed body that omits the provider's
	// `created` makes no claim about when it observed the outcome, and the
	// adapter records that as the domain's zero instant rather than inventing
	// one.
	silent.OccurredAt = time.Time{}
	zero := p.event(t, f, intent, payRef(t, "evt-zero"), payRef(t, "pi"))
	zero.AmountMinorUnits = int64Pointer(0)

	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		if err := p.events.Record(ctx, silent); err != nil {
			return err
		}
		return p.events.Record(ctx, zero)
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	recorded, err := p.events.ByIntent(t.Context(), intent.ID, 10)
	if err != nil {
		t.Fatalf("ByIntent: %v", err)
	}
	byEvent := map[string]payments.ProviderEventRecord{}
	for _, got := range recorded {
		byEvent[got.EventID] = got
	}

	silentRead, ok := byEvent[silent.EventID]
	if !ok {
		t.Fatal("the delivery that claimed no amount was not read back")
	}
	if silentRead.AmountMinorUnits != nil {
		t.Errorf("a delivery with no amount read back as %d, want nil — NULL must not become zero", *silentRead.AmountMinorUnits)
	}
	if silentRead.Currency != "" {
		t.Errorf("a delivery with no currency read back as %q, want the empty string", silentRead.Currency)
	}
	if silentRead.ProviderPaymentRef != silent.ProviderPaymentRef || silentRead.Kind != silent.Kind {
		t.Errorf("the delivery's claim did not round trip: %+v", silentRead)
	}
	if !silentRead.OccurredAt.IsZero() {
		t.Errorf("occurred_at = %s, want the zero instant — the delivery stated no time", silentRead.OccurredAt)
	}
	// The read-back above is this layer's own round trip; this is the COLUMN,
	// which is the thing that would have been wrong. A NOT NULL column takes the
	// zero instant and keeps it as 0001-01-01, a date the provider never claimed
	// and one no `WHERE occurred_at IS NULL` would ever find — so the assertion
	// is on NULL itself rather than on the value this layer reconstructs from it.
	var nullOccurred int
	if err := p.store.Querier(t.Context()).QueryRowContext(t.Context(),
		`SELECT count(*) FROM control.payment_events
		  WHERE provider_event_id = $1 AND occurred_at IS NULL`, silent.EventID).Scan(&nullOccurred); err != nil {
		t.Fatalf("count the deliveries recorded with no occurred_at: %v", err)
	}
	if nullOccurred != 1 {
		t.Errorf("the delivery that stated no time has %d rows with a NULL occurred_at, want 1 — the absence was stored as a claim", nullOccurred)
	}

	zeroRead, ok := byEvent[zero.EventID]
	if !ok {
		t.Fatal("the delivery that claimed zero was not read back")
	}
	if zeroRead.AmountMinorUnits == nil {
		t.Error("a delivery that claimed zero read back as nil, want a pointer to zero")
	} else if *zeroRead.AmountMinorUnits != 0 {
		t.Errorf("a delivery that claimed zero read back as %d, want 0", *zeroRead.AmountMinorUnits)
	}
}

// TestIntegrationPaymentQuarantineKeepsTheClaimAndTheBytes is the one table
// with no read member in the port, so it is read back through raw SQL: what an
// operator's queue would show, and the three-way distinction the columns carry.
//
//   - A delivery that did not verify has no event id, no kind and NO PAYLOAD —
//     an unauthenticated body is an attacker's free text, and its absence is
//     what keeps this table's contents to things the provider actually said.
//   - A delivery that verified but could not be interpreted keeps its claimed
//     identity, its claim, and the bytes, because the bytes are the only answer
//     to "what did the provider actually send".
//   - `occurred_at` is nullable and the domain's zero time means "the provider
//     stated no time", so an absent timestamp is NULL rather than year one.
//     payment_events.occurred_at is nullable for the same reason and holds the
//     same three-way distinction: the timing the freshness tolerance is
//     measured against is the SIGNATURE HEADER's, a different fact, and the
//     provider's `created` is a claim its body may simply omit.
func TestIntegrationPaymentQuarantineKeepsTheClaimAndTheBytes(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-quarantine")
	intent := p.openIntent(t, f, payKey(t, "quarantine"), time.Now().UTC())

	unverified := payments.QuarantineRecord{
		Provider:           f.provider,
		ProviderAccountKey: "acct-" + f.provider,
		Reason:             payments.ReasonUnverifiable,
		RecordedAt:         micros(time.Now().UTC()),
	}
	mismatch := payments.QuarantineRecord{
		Provider:           f.provider,
		ProviderAccountKey: "acct-" + f.provider,
		EventID:            payRef(t, "evt"),
		IntentID:           intent.ID,
		Kind:               "payment_intent.amount_mismatch",
		// The claim this delivery made, kept as claimed and never acted on: an
		// amount of zero is a claim, and the pointer is what says so.
		ProviderPaymentRef: payRef(t, "pi"),
		AmountMinorUnits:   int64Pointer(0),
		Currency:           "USD",
		Reason:             payments.ReasonAmountMismatch,
		Payload:            []byte(`{"id":"` + payRef(t, "payload") + `"}`),
		OccurredAt:         micros(time.Now().UTC()),
		RecordedAt:         micros(time.Now().UTC()),
	}
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		if err := p.quarantine.Record(ctx, unverified); err != nil {
			return err
		}
		return p.quarantine.Record(ctx, mismatch)
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// The unverified row: every claim absent, and the row is still there. The
	// read is by provider because that namespace is this test's alone.
	var (
		eventID, kind, currency sql.NullString
		intentID                sql.NullString
		amount                  sql.NullInt64
		payload                 []byte
		occurredAt              sql.NullTime
		recordedAt              time.Time
		reason                  string
	)
	readUnverified := func() error {
		return p.store.Querier(t.Context()).QueryRowContext(t.Context(),
			`SELECT provider_event_id, intent_id, kind, amount_minor_units, currency, payload, occurred_at, recorded_at, reason
			   FROM control.payment_quarantine
			  WHERE provider = $1 AND reason = $2
			  ORDER BY id DESC LIMIT 1`, f.provider, string(payments.ReasonUnverifiable)).
			Scan(&eventID, &intentID, &kind, &amount, &currency, &payload, &occurredAt, &recordedAt, &reason)
	}
	if err := readUnverified(); err != nil {
		t.Fatalf("read the unverified quarantine row: %v", err)
	}
	for name, present := range map[string]bool{
		"provider_event_id":  eventID.Valid,
		"intent_id":          intentID.Valid,
		"kind":               kind.Valid,
		"amount_minor_units": amount.Valid,
		"currency":           currency.Valid,
		"payload":            payload != nil,
		"occurred_at":        occurredAt.Valid,
	} {
		if present {
			t.Errorf("an unverified delivery recorded a %s, want NULL — the claim is only worth recording when something authenticated it", name)
		}
	}
	if recordedAt.IsZero() {
		t.Error("recorded_at is the zero time; the column's own now() default did not reach the row")
	}

	// The verified-but-uninterpretable row: the claim, the bytes, and the
	// claimed timestamp all survive.
	mismatchRead := func() (sql.NullString, sql.NullInt64, []byte, sql.NullTime, error) {
		var (
			readEventID, readKind sql.NullString
			readAmount            sql.NullInt64
			readPayload           []byte
			readOccurred          sql.NullTime
		)
		err := p.store.Querier(t.Context()).QueryRowContext(t.Context(),
			`SELECT provider_event_id, kind, amount_minor_units, payload, occurred_at
			   FROM control.payment_quarantine
			  WHERE provider = $1 AND reason = $2
			  ORDER BY id DESC LIMIT 1`, f.provider, string(payments.ReasonAmountMismatch)).
			Scan(&readEventID, &readKind, &readAmount, &readPayload, &readOccurred)
		return readEventID, readAmount, readPayload, readOccurred, err
	}
	gotEventID, gotAmount, gotPayload, gotOccurred, err := mismatchRead()
	if err != nil {
		t.Fatalf("read the verified quarantine row: %v", err)
	}
	if gotEventID.String != mismatch.EventID {
		t.Errorf("provider_event_id = %q, want %q", gotEventID.String, mismatch.EventID)
	}
	if !gotAmount.Valid || gotAmount.Int64 != 0 {
		t.Errorf("amount_minor_units = %+v, want a stored zero rather than NULL", gotAmount)
	}
	if string(gotPayload) != string(mismatch.Payload) {
		t.Errorf("payload = %q, want %q", gotPayload, mismatch.Payload)
	}
	if !micros(gotOccurred.Time).Equal(micros(mismatch.OccurredAt)) {
		t.Errorf("occurred_at = %s, want %s", gotOccurred.Time, mismatch.OccurredAt)
	}

	// The delivery the application most often quarantines is the one naming a
	// payment this platform never opened. Its intent_id is empty, the schema
	// stores NULL, and the FK does not fire — which is the case that makes the
	// column nullable in the first place.
	unknown := mismatch
	unknown.IntentID = ""
	unknown.Payload = nil
	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		return p.quarantine.Record(ctx, unknown)
	}); err != nil {
		t.Fatalf("record a quarantine with no payment: %v", err)
	}
	var back sql.NullString
	if err := p.store.Querier(t.Context()).QueryRowContext(t.Context(),
		`SELECT intent_id FROM control.payment_quarantine WHERE provider = $1 ORDER BY id DESC LIMIT 1`,
		f.provider).Scan(&back); err != nil {
		t.Fatalf("read back the quarantine with no payment: %v", err)
	}
	if back.Valid {
		t.Errorf("intent_id = %q, want NULL for a delivery that resolved to no payment", back.String)
	}
}

// ---------------------------------------------------------------------------
// the list, its keyset and the account predicate that leads it.
// ---------------------------------------------------------------------------

// TestIntegrationPaymentListForAccountPagesByKeyset pins the console list's
// whole contract: the account predicate leads and excludes another account's
// rows, the order is newest-first by id, one more row than asked for comes back
// so `has_more` is derivable, and the empty cursor is the BEGINNING of the list
// rather than a cursor that matches nothing.
//
// That last one is a regression this repository has a live instance of: a bare
// `id > ”` against a uuid column raises SQLSTATE 22P02, so the first page of a
// uuid-keyed list is a 500 for the zero cursor. The port's cursor is a payment
// id and its zero value means "the beginning", and this test is what holds the
// statement to that: a first page that errors fails here.
func TestIntegrationPaymentListForAccountPagesByKeyset(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-list")
	other := p.newPaymentFixture(t, "payments-list-other")

	// Three payments for this fixture's account and two for the other, in a
	// known order: the minted instant is strictly increasing, so the ids order
	// the way creation did.
	base := micros(time.Now().UTC())
	mine := make([]payments.Intent, 0, 3)
	for i := range 3 {
		mine = append(mine, p.openIntent(t, f, payKey(t, fmt.Sprintf("list-%d", i)), base.Add(time.Duration(i)*time.Millisecond)))
	}
	theirs := make([]payments.Intent, 0, 2)
	for i := range 2 {
		theirs = append(theirs, p.openIntent(t, other, payKey(t, fmt.Sprintf("list-other-%d", i)), base.Add(time.Duration(i)*time.Millisecond)))
	}
	newestFirst := []payments.Intent{mine[2], mine[1], mine[0]}

	// The first page, with the ZERO cursor: the beginning of the list.
	first, err := p.intents.ListForAccount(t.Context(), f.account, persistence.PaymentPage{Limit: 2})
	if err != nil {
		t.Fatalf("ListForAccount with an empty cursor: %v", err)
	}
	// One more row than the caller asked for — the probe row the application's
	// PageOf drops and reads `has_more` from.
	if len(first) != 3 {
		t.Fatalf("the first page returned %d rows for a limit of 2, want 3 (the probe row included)", len(first))
	}
	for i, want := range newestFirst {
		if first[i].ID != want.ID {
			t.Errorf("page row %d = %s, want %s — the order is id descending, newest first", i, first[i].ID, want.ID)
		}
	}

	// The second page continues from the last row of the first.
	second, err := p.intents.ListForAccount(t.Context(), f.account, persistence.PaymentPage{
		After: newestFirst[1].ID, Limit: 2,
	})
	if err != nil {
		t.Fatalf("ListForAccount from a cursor: %v", err)
	}
	if len(second) != 1 || second[0].ID != newestFirst[2].ID {
		t.Errorf("the second page = %+v, want the single oldest payment %s", second, newestFirst[2].ID)
	}

	// Past the end there is nothing, and it is not an error: and the cursor is
	// not a filter that a client can use to read around the account predicate.
	exhausted, err := p.intents.ListForAccount(t.Context(), f.account, persistence.PaymentPage{
		After: newestFirst[2].ID, Limit: 2,
	})
	if err != nil {
		t.Fatalf("ListForAccount past the end: %v", err)
	}
	if len(exhausted) != 0 {
		t.Errorf("the page past the end returned %d rows, want none", len(exhausted))
	}

	// The account predicate is the statement's first argument, so the other
	// account's payments are rows this query never returned — asserted in both
	// directions, and with the cursor at the very beginning, which is where a
	// missing predicate would leak the most.
	for _, account := range []struct {
		id    string
		own   []payments.Intent
		other []payments.Intent
	}{
		{f.account, mine, theirs},
		{other.account, theirs, mine},
	} {
		page, err := p.intents.ListForAccount(t.Context(), account.id, persistence.PaymentPage{Limit: 10})
		if err != nil {
			t.Fatalf("ListForAccount(%s): %v", account.id, err)
		}
		if len(page) != len(account.own) {
			t.Fatalf("ListForAccount(%s) returned %d rows, want %d", account.id, len(page), len(account.own))
		}
		returned := map[payments.IntentID]bool{}
		for _, got := range page {
			returned[got.ID] = true
			if got.AccountID != account.id {
				t.Errorf("ListForAccount(%s) returned a payment owned by %s", account.id, got.AccountID)
			}
		}
		for _, foreign := range account.other {
			if returned[foreign.ID] {
				t.Errorf("ListForAccount(%s) returned another account's payment %s", account.id, foreign.ID)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// the refusal, and the transaction the writes are shaped by.
// ---------------------------------------------------------------------------

// TestIntegrationPaymentWritesRefuseOutsideAUnitOfWork is the port's one
// standing order, asserted for EVERY write member and for no read.
//
// The rule exists because a delivery is recorded and its funding leg is written
// as one transaction, and a repository that quietly autocommitted on the pool
// when the caller's context had lost its transaction would break that sharing
// without breaking any signature: the delivery would be on file, the money would
// not be in the bucket, and the provider would be told 200. So each of the seven
// writes below is handed a context that carries no unit of work, and each one
// must refuse — and, the stronger half, must have written NOTHING.
func TestIntegrationPaymentWritesRefuseOutsideAUnitOfWork(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-refusal")
	ctx := t.Context()
	intent := p.openIntent(t, f, payKey(t, "refusal"), time.Now().UTC())
	opened := p.openCaptureReady(t, intent)
	now := micros(time.Now().UTC())

	// A payment that has never been written, so the refused Create has nothing
	// to collide with and the assertion below is about the refusal rather than
	// about a duplicate key.
	unwrittenID, err := payments.NewIntent(now)
	if err != nil {
		t.Fatalf("NewIntent: %v", err)
	}
	unwritten, err := payments.New(payments.NewPayment{
		AccountID: f.account, FundingBucketID: string(f.bucket.ID),
		AmountMinorUnits: 5000, Currency: "USD", MinorUnitExponent: 2,
		Provider: f.provider, IdempotencyKey: payKey(t, "refusal-unwritten"),
		CheckoutTTL: time.Hour, Now: now, MintedID: unwrittenID, MintedAt: now,
	})
	if err != nil {
		t.Fatalf("New payment: %v", err)
	}

	// Each entry answers one question — did this port call report that it wrote?
	// A member with a verdict says so through the verdict, and a member with
	// only an error says so by that error being nil, which is what a repository
	// that had silently autocommitted would have returned.
	writes := map[string]func() (bool, error){
		"PaymentIntents.Create": func() (bool, error) {
			err := p.intents.Create(ctx, unwritten)
			return err == nil, err
		},
		"PaymentIntents.RecordCheckout": func() (bool, error) {
			applied, err := p.intents.RecordCheckout(ctx, opened.ID,
				payments.OpenedCheckout{URL: "https://checkout.example.test/refused", ProviderRef: payRef(t, "cs")},
				[]payments.Status{payments.StatusCreated}, now)
			return applied, err
		},
		"PaymentIntents.MoveStatus": func() (bool, error) {
			_, applied, err := p.intents.MoveStatus(ctx, opened.ID,
				[]payments.Status{payments.StatusCheckoutOpen}, payments.StatusRequiresAction, now)
			return applied, err
		},
		"PaymentIntents.RecordCapture": func() (bool, error) {
			_, applied, err := p.intents.RecordCapture(ctx, opened.ID, payRef(t, "pi"),
				[]payments.Status{payments.StatusCheckoutOpen}, now)
			return applied, err
		},
		"PaymentIntents.RecordRefund": func() (bool, error) {
			_, applied, err := p.intents.RecordRefund(ctx, opened.ID, payRef(t, "re"), 100, 0,
				[]payments.Status{payments.StatusSucceeded}, payments.StatusPartiallyRefunded, now)
			return applied, err
		},
		"PaymentEvents.Record": func() (bool, error) {
			err := p.events.Record(ctx, p.event(t, f, intent, payRef(t, "evt"), payRef(t, "pi")))
			return err == nil, err
		},
		"PaymentEvents.Settle": func() (bool, error) {
			err := p.events.Settle(ctx, payments.DeliveryKey{
				Provider: f.provider, ProviderAccountKey: "acct-" + f.provider, EventID: payRef(t, "evt"),
			}, payments.DispositionQuarantined)
			return err == nil, err
		},
		"PaymentQuarantine.Record": func() (bool, error) {
			err := p.quarantine.Record(ctx, payments.QuarantineRecord{
				Provider: f.provider, ProviderAccountKey: "acct-" + f.provider,
				Reason: payments.ReasonUnknownPayment, RecordedAt: now,
			})
			return err == nil, err
		},
	}
	for name, write := range writes {
		wrote, err := write()
		if !errors.Is(err, errPaymentWriteOutsideUnitOfWork) {
			t.Errorf("%s outside a unit of work error = %v, want the refusal", name, err)
		}
		if wrote {
			t.Errorf("%s outside a unit of work reported that it wrote something", name)
		}
	}

	// Nothing was written — the half of the refusal a caller only learns from
	// the database.
	if _, err := p.intents.ByID(ctx, unwritten.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("the refused Create left a payment on file: %v", err)
	}
	var events, quarantines int
	if err := p.store.Querier(ctx).QueryRowContext(ctx,
		`SELECT count(*) FROM control.payment_events WHERE provider = $1`, f.provider).Scan(&events); err != nil {
		t.Fatalf("count the provider's deliveries: %v", err)
	}
	if err := p.store.Querier(ctx).QueryRowContext(ctx,
		`SELECT count(*) FROM control.payment_quarantine WHERE provider = $1 AND reason = $2`,
		f.provider, string(payments.ReasonUnknownPayment)).Scan(&quarantines); err != nil {
		t.Fatalf("count the provider's quarantines: %v", err)
	}
	if events != 0 || quarantines != 0 {
		t.Errorf("the refused writes left %d deliveries and %d quarantines behind, want none", events, quarantines)
	}
	if after := p.mustByID(t, opened.ID); !sameIntent(after, opened) {
		t.Errorf("a refused write changed the payment:\n got  %+v\n want %+v", after, opened)
	}

	// And the asymmetry: reads carry no such rule. Both of these run on the
	// same unit-of-work-free context the writes above were refused on.
	if _, err := p.intents.ByID(ctx, opened.ID); err != nil {
		t.Errorf("ByID outside a unit of work error = %v, want the committed row", err)
	}
	if _, err := p.events.ByIntent(ctx, opened.ID, 10); err != nil {
		t.Errorf("ByIntent outside a unit of work error = %v, want an answer", err)
	}
}

// TestIntegrationPaymentDeliveryAndLedgerLegRollBackTogether is THE test this
// suite exists for, and it is the reason the atomicity claim is made here and
// nowhere else: fakes have two method calls, and two method calls are not a
// transaction.
//
// One unit of work records a verified delivery and appends the funding leg the
// delivery licenses — the ordering the application uses, delivery first, so a
// crash rolls both back and a redelivery re-runs, and the credit last, so a
// record can never name a leg that did not land. Both writes are then shown to
// be VISIBLE INSIDE the unit of work, which is the half that makes the rollback
// meaningful: there was something to undo. Then the unit of work fails, and the
// database is asked whether the delivery, the leg and the money survived.
//
// The two evidence tables are append-only at the engine, with no-delete guards
// that refuse DELETE outright. That is what makes this test worth running
// rather than reasoning about: the guards do not stand in the rollback's way,
// because a rollback is not a DELETE — it is the transaction's own undo — and
// the assertion that the row is gone afterwards is the proof.
func TestIntegrationPaymentDeliveryAndLedgerLegRollBackTogether(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-atomic")
	intent := p.openIntent(t, f, payKey(t, "atomic"), time.Now().UTC())
	event := p.event(t, f, intent, payRef(t, "evt-atomic"), payRef(t, "pi-atomic"))
	leg := p.accounting.topupEntry(t, f.bucket.ID, 5000, acctCommandKey(t, "it-pay-atomic-"))

	boom := errors.New("the webhook failed after the ledger leg landed")
	err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		if err := p.events.Record(ctx, event); err != nil {
			return err
		}
		_, updated, err := p.accounting.ledger.Append(ctx, leg)
		if err != nil {
			return err
		}
		if int64(updated.Settled) != 5000 {
			t.Errorf("the bucket's settled balance inside the unit of work = %d, want 5000", int64(updated.Settled))
		}
		// Visible to the unit of work's own context — whether it is visible to
		// anyone ELSE yet is the question this test answers by failing the unit.
		recorded, err := p.events.ByIntent(ctx, intent.ID, 10)
		if err != nil {
			return err
		}
		if len(recorded) != 1 {
			t.Errorf("the unit of work read back %d of its own deliveries, want 1", len(recorded))
		}
		if _, err := p.accounting.ledger.ByBucketAndCommandKey(ctx, f.bucket.ID, leg.CommandKey); err != nil {
			t.Errorf("the unit of work did not read its own ledger leg back: %v", err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithinTx returned %v, want the unit's own error handed back unchanged", err)
	}

	// The delivery is gone.
	recorded, err := p.events.ByIntent(t.Context(), intent.ID, 10)
	if err != nil {
		t.Fatalf("ByIntent after the rollback: %v", err)
	}
	if len(recorded) != 0 {
		t.Errorf("%d deliveries survived the rollback, want none", len(recorded))
	}
	// The leg is gone.
	if _, err := p.accounting.ledger.ByBucketAndCommandKey(t.Context(), f.bucket.ID, leg.CommandKey); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("the ledger leg survived the rollback: error = %v, want persistence.ErrNotFound", err)
	}
	// And the money never moved.
	after, err := p.accounting.buckets.ByID(t.Context(), f.bucket.ID)
	if err != nil {
		t.Fatalf("read the bucket after the rollback: %v", err)
	}
	if int64(after.Settled) != 0 || after.LastSequence != f.bucket.LastSequence {
		t.Errorf("the balance survived the rollback: settled = %d, sequence = %d, want 0 and %d",
			int64(after.Settled), after.LastSequence, f.bucket.LastSequence)
	}
}

// TestIntegrationPaymentDeliveryAndLedgerLegCommitTogether is the failing test
// above read the other way round, and it is not redundant: a rollback test
// whose fixture never wrote anything passes for the wrong reason, and only a
// committed twin shows that the three writes it asserts the absence of are
// writes that land when the unit of work succeeds.
//
// It is also the shape the webhook path actually runs: the delivery is recorded
// FIRST and the credit lands after, both inside the one unit of work the
// route opens.
func TestIntegrationPaymentDeliveryAndLedgerLegCommitTogether(t *testing.T) {
	p := integrationPayments(t)
	f := p.newPaymentFixture(t, "payments-atomic-commit")
	intent := p.openIntent(t, f, payKey(t, "atomic-commit"), time.Now().UTC())
	event := p.event(t, f, intent, payRef(t, "evt-atomic"), payRef(t, "pi-atomic"))
	leg := p.accounting.topupEntry(t, f.bucket.ID, 2500, acctCommandKey(t, "it-pay-commit-"))

	if err := p.store.WithinTx(t.Context(), func(ctx context.Context) error {
		if err := p.events.Record(ctx, event); err != nil {
			return err
		}
		_, _, err := p.accounting.ledger.Append(ctx, leg)
		return err
	}); err != nil {
		t.Fatalf("the committed unit of work: %v", err)
	}

	// A second pool is the proof the commit reached the server: it shares
	// nothing with the first but the database, so what it reads is what the
	// server kept — the same trick integration_test.go plays with the probe
	// table, applied to the two writes this suite is about.
	secondDB := integrationDB(t)
	second := NewPaymentEvents(New(secondDB))
	recorded, err := second.ByIntent(t.Context(), intent.ID, 10)
	if err != nil {
		t.Fatalf("ByIntent through a second pool: %v", err)
	}
	if len(recorded) != 1 || recorded[0].EventID != event.EventID {
		t.Fatalf("the second pool read %+v, want the one delivery %s", recorded, event.EventID)
	}
	after, err := p.accounting.buckets.ByID(t.Context(), f.bucket.ID)
	if err != nil {
		t.Fatalf("read the bucket: %v", err)
	}
	if int64(after.Settled) != 2500 {
		t.Errorf("the bucket's settled balance = %d, want 2500 — the credit must have landed with the delivery", int64(after.Settled))
	}
}
