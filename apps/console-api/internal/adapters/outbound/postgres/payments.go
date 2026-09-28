package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/payments"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The payment integration's three repositories: the intents a customer funds
// through a provider, the verified deliveries that fund them, and the
// deliveries this build could not apply.
//
// The file has one rule at its centre and it is not a performance rule. Every
// write below checks InUnitOfWork and REFUSES a context that carries none, and
// the refusal is the reason the port states the rule at all. A provider
// delivery is recorded and its funding leg is written as ONE transaction —
// the delivery first, so a crash rolls both back and a redelivery re-runs; the
// credit last, so the record can never name a leg that did not land. A
// repository that silently autocommitted on the pool when the caller's context
// had lost its transaction would break that sharing without breaking any
// signature: the delivery would be on file, the money would not be in the
// bucket, and the provider would be told 200. So the write members below fail
// loudly, exactly as the ledger's Append does and for the same class of reason.
// The READS carry no such rule, and the asymmetry is deliberate: a read outside
// a unit of work sees a committed state, which is a correct answer to a correct
// question. It is only the pairing of a write with another write that has to be
// atomic.
//
// Everything else here follows the accounting adapter's discipline. A
// repository resolves its handle through store.Querier(ctx) and never through
// the pool directly, so a statement runs on the caller's transaction when there
// is one. A compare-and-swap is ONE guarded UPDATE whose WHERE clause carries
// the states the caller read, with state_version bumped in the SET list; zero
// rows affected is the verdict false and never an error, because a lost race on
// this surface is the ordinary outcome of a provider redelivering a delivery.
// And a uniqueness violation is translated by CONSTRAINT NAME into the domain's
// sentinel, never by message text, because a caller that had to pattern-match a
// driver string would be branching on a message rather than a condition.
//
// The schema is the other half of every rule here and it is worth naming once,
// because three of these statements exist to pre-empt it rather than to
// duplicate it. `payment_intents_capture_shape` is a BICONDITIONAL — a payment
// names a capture reference exactly when its status is one of the three
// post-capture states — so a status write and a reference write are one fact
// told twice. `payment_intents_status_transition_guard` is the state machine in
// plpgsql, fired for every writer including raw SQL, and it refuses a move to
// succeeded with a NULL capture reference. And the two evidence tables are
// append-only at the engine. This layer does not restate any of them: it writes
// the shape the guards already require, so the guards hold for the writers this
// build did not write.

// errPaymentWriteOutsideUnitOfWork is the refusal every write member below
// shares. It is package-private, like errAppendOutsideUnitOfWork, so the rule
// has one spelling here and a caller can branch on the wrapped words.
var errPaymentWriteOutsideUnitOfWork = errors.New("refused: a payment write is unit-of-work-shaped and ctx carries no unit of work")

// requirePaymentUnitOfWork returns the refusal when ctx carries no unit of
// work, and nil when it does. what names the operation, so the failure names
// which write was refused rather than only that one was.
func requirePaymentUnitOfWork(store persistence.Store, ctx context.Context, what string) error {
	if store.InUnitOfWork(ctx) {
		return nil
	}
	return fmt.Errorf("postgres: %s: %w", what, errPaymentWriteOutsideUnitOfWork)
}

// NewPaymentIntents returns the persistence port's PaymentIntents repository
// backed by store. It panics on a nil store for the reason every constructor in
// this package does: the failure a nil dependency produces later is strictly
// worse than a loud one here.
func NewPaymentIntents(store persistence.Store) persistence.PaymentIntents {
	if store == nil {
		panic("postgres: NewPaymentIntents requires a non-nil persistence.Store")
	}
	return &paymentIntentsRepo{store: store}
}

// NewPaymentEvents returns the persistence port's PaymentEvents repository
// backed by store.
func NewPaymentEvents(store persistence.Store) persistence.PaymentEvents {
	if store == nil {
		panic("postgres: NewPaymentEvents requires a non-nil persistence.Store")
	}
	return &paymentEventsRepo{store: store}
}

// NewPaymentQuarantine returns the persistence port's PaymentQuarantine
// repository backed by store.
func NewPaymentQuarantine(store persistence.Store) persistence.PaymentQuarantine {
	if store == nil {
		panic("postgres: NewPaymentQuarantine requires a non-nil persistence.Store")
	}
	return &paymentQuarantineRepo{store: store}
}

// Compile-time proof that the repositories satisfy the port's contracts.
var (
	_ persistence.PaymentIntents    = (*paymentIntentsRepo)(nil)
	_ persistence.PaymentEvents     = (*paymentEventsRepo)(nil)
	_ persistence.PaymentQuarantine = (*paymentQuarantineRepo)(nil)
)

// The unique constraints this file translates into the domain's sentinels, by
// NAME — never by message text, and never by table. Both names are the ones the
// migration declares, and both are named constraints rather than bare indexes
// for exactly this reader.
const (
	// paymentIntentsIdempotencyKey is the (account_id, idempotency_key) unique
	// constraint: two clicks of one button are one payment, and a second insert
	// under the same key is the convergence signal ErrDuplicatePayment carries.
	paymentIntentsIdempotencyKey = "payment_intents_idempotency_key"
	// paymentEventsDeliveryKey is the (provider, provider_account_key,
	// provider_event_id) unique constraint: the dedup key, and the reason a
	// redelivery is a duplicate rather than a second credit.
	paymentEventsDeliveryKey = "payment_events_delivery_key"
	// paymentIntentsProviderPaymentRefKey is the unique constraint on
	// provider_payment_ref. It is not translated into a domain sentinel
	// because it is not a CONVERGENCE: unlike the two above, a second payment
	// naming one provider payment is a fault in the world rather than a
	// concurrent click, and no sentinel would make it one. RecordCapture
	// answers it as a lost state race instead, so the delivery is quarantined
	// and an operator sees it — see that member's comment.
	paymentIntentsProviderPaymentRefKey = "payment_intents_provider_payment_ref_key"
)

// recordedDisposition is what every row this adapter inserts carries, and the
// name is not a shorthand: `event.Disposition` is NOT written here.
//
// The delivery path cannot know the verdict when the row is inserted — the
// insert is the arbiter of the race between two concurrent deliveries of one
// event id, and it has to happen before the payment is resolved — so a row is
// written provisional and then settled by the admitted UPDATE. Trusting the
// caller's disposition here instead would let a caller write a row straight to
// `applied` with no effect behind it, which is the one thing an operator reads
// this table to rule out. The provisional value is therefore this file's own,
// it is the one the engine guard admits a transition away from, and no
// argument reaches the column at insert time.
//
// The cost is stated rather than hidden: a row that is written and never
// settled would stay provisional, and a reader that counted dispositions would
// be counting one. The engine guard is what makes that impossible from outside
// this file — a settle of a settled row is refused — and inside it, Settle is
// the only caller and the use case always reaches it or rolls the insert back.
const recordedDisposition = string(payments.DispositionRecorded)

// nullText maps the domain's empty string onto SQL NULL and everything else
// onto itself.
//
// The domain carries absence as the zero value — ProviderCheckoutRef,
// ProviderPaymentRef, CheckoutURL, IntentID, kind and currency are all strings
// that mean "not said" when empty — while the columns that hold them are
// nullable, and the mapping has to be explicit for the reason accounting's
// ledgerEntryArgs gives: an empty string is not NULL, and a column that
// conflated them would make "this delivery reported no currency" and "this
// delivery reported an empty currency" the same row.
//
// `kind` is one of these nullable columns, and it has to be. A delivery whose
// event type is absent, non-string or longer than the column admits is a real
// delivery that this build cannot interpret, and the adapter's answer for it is
// a quarantine with reason unknown-kind — a path that only runs if the row
// carrying it can be written. Stored raw, the empty string would fail
// payment_events_kind_grammar instead, which is a check violation nothing
// translates, so the whole delivery would roll back as a 500 and the provider
// would retry the same unstorable bytes forever.
func nullText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// nullAmount maps the domain's absent amount onto SQL NULL. A pointer to zero
// stays a zero, which is the whole point: an absent amount and an amount of
// zero are different sentences — one is a delivery that stated nothing, the
// other is a delivery that stated nothing at all was charged — and only the
// pointer can hold both.
func nullAmount(amount *int64) any {
	if amount == nil {
		return nil
	}
	return *amount
}

// nullTime maps the zero instant onto SQL NULL, for the columns where the
// domain's zero value means "not said" — BOTH tables' occurred_at, which the
// provider may simply not have stated.
//
// It is used on payment_events.occurred_at as well as payment_quarantine's, and
// the events column is nullable for the same reason. The adapter states a zero
// OccurredAt is a real answer rather than an oversight — a signed body that
// omits `created` makes no claim about when the provider observed the outcome —
// and a NOT NULL column would take that answer and store year 1, which is not
// an absence but a claim the provider never made, and one that would pass every
// `IS NULL` an operator wrote to find the rows that said nothing.
func nullTime(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at
}

// timeOrZero is the read half of nullTime: a column that said nothing comes
// back as the domain's zero instant rather than as a scan error, so the record
// carries "not said" in the one shape the domain uses for it.
func timeOrZero(at sql.NullTime) time.Time {
	if !at.Valid {
		return time.Time{}
	}
	return at.Time
}

// nullBytes maps an empty payload onto SQL NULL.
//
// It is the same distinction nullText draws and it matters more here: NULL is
// what an UNVERIFIED delivery's payload must be — an unauthenticated body is an
// attacker's free text and is deliberately not stored — while an empty byte
// slice is a body that verified and happened to be empty, which is a real thing
// a broken provider sends and which the evidence bound deliberately admits.
func nullBytes(payload []byte) any {
	if payload == nil {
		return nil
	}
	return payload
}

// ---------------------------------------------------------------------------
// payment_intents — the mutable aggregate.
// ---------------------------------------------------------------------------

type paymentIntentsRepo struct {
	store persistence.Store
}

// paymentIntentColumns is the intent row, in scan order, shared by every read
// and by the compare-and-swap statements' RETURNING so the two cannot drift
// apart column by column.
const paymentIntentColumns = `id, account_id, funding_bucket_id, amount_minor_units,
       currency, minor_unit_exponent, provider, idempotency_key, status,
       provider_checkout_ref, provider_payment_ref, checkout_url,
       refunded_minor_units, uncovered_refund_minor_units, state_version,
       expires_at, created_at, updated_at`

const insertPaymentIntent = `
INSERT INTO control.payment_intents
    (id, account_id, funding_bucket_id, amount_minor_units,
     currency, minor_unit_exponent, provider, idempotency_key, status,
     provider_checkout_ref, provider_payment_ref, checkout_url,
     refunded_minor_units, uncovered_refund_minor_units, state_version,
     expires_at, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`

const selectPaymentIntentByID = `
SELECT ` + paymentIntentColumns + `
FROM control.payment_intents
WHERE id = $1`

const selectPaymentIntentByAccountAndKey = `
SELECT ` + paymentIntentColumns + `
FROM control.payment_intents
WHERE account_id = $1 AND idempotency_key = $2`

// The delivery-resolution read, and the one lookup on this surface a third
// party's payload drives. The account predicate is deliberately ABSENT: an
// event names a provider reference, the reference resolves to a row this
// platform wrote, and the row carries the account. A statement that also
// filtered by an account named in the payload would be a statement trusting the
// payload to say whose money moves — see the port's ByProviderCheckoutRef.
const selectPaymentIntentByCheckoutRef = `
SELECT ` + paymentIntentColumns + `
FROM control.payment_intents
WHERE provider = $1 AND provider_checkout_ref = $2`

// The payment-reference lookup, and the statement a refund resolves through.
//
// It has the checkout lookup's shape and its properties for the same reasons:
// no account predicate, because a webhook has no session — the reference is
// matched against a column THIS PLATFORM WROTE and the row carries the account —
// and the provider predicate first, because a provider scopes its identifiers
// to its own namespace and two providers can hand out the same string.
//
// It seeks rather than scans: `payment_intents_provider_payment_ref_key` is a
// partial unique index on exactly this pair, and the uniqueness is a guard as
// well as a seek — one payment of ours per provider payment, so a statement
// that matched two rows would be a statement whose answer depended on the
// planner. The partiality is what makes it usable at all: most rows hold NULL
// in this column while the payment is `created`, and a plain UNIQUE over a
// nullable column guarantees nothing while looking like it does.
const selectPaymentIntentByPaymentRef = `
SELECT ` + paymentIntentColumns + `
FROM control.payment_intents
WHERE provider = $1 AND provider_payment_ref = $2`

// The account's own list, newest first. The account predicate is FIRST and it
// is a predicate in the statement rather than a filter applied to rows already
// fetched, so another account's payment is a row this query never returned —
// the same answer, at the same cost, a genuinely absent row gives.
//
// The keyset is the payment id, descending, and both halves of that are
// load-bearing. Ids are version-7, so the leading forty-eight bits ARE a
// millisecond timestamp and `id DESC` is already newest-first; the id is also
// unique, so a page break can never land inside a tie — which is exactly what
// ordering by `created_at` would do, since two payments opened in the same
// microsecond would need a tiebreaker and a keyset that breaks inside a tie
// skips or repeats a row. `payment_intents_account_id_idx` is (account_id, id
// DESC) and exists so this predicate can seek rather than sort.
//
// The empty-`after` branch is the OR's first disjunct, and it is not
// decoration. The zero value of the cursor means "the beginning of the list"
// — the port says so — and a bare `id < ”` against a uuid column is not a
// predicate that matches nothing: the driver rejects the empty string with
// SQLSTATE 22P02 and the first page of the list becomes a 500. `NULLIF`
// restates that bound so the second disjunct can never be evaluated against
// the empty string even if the planner chooses to, which makes the statement
// safe without relying on OR's short-circuit.
//
// The cost of the OR is real and is recorded rather than hidden: the planner
// cannot fold `NULLIF($2, ”)::uuid` into an index bound, so the id range is a
// filter over the account's rows in index order rather than a seek. The
// alternative — a second statement for the first page — is refused here for the
// reason consoleread_identity.go gives: two statements are two shapes for the
// account predicate to be missing from, and the second is the one that ships.
const listPaymentIntentsForAccount = `
SELECT ` + paymentIntentColumns + `
FROM control.payment_intents
WHERE account_id = $1
  AND ($2 = '' OR id < NULLIF($2, '')::uuid)
ORDER BY id DESC
LIMIT $3`

// Create inserts a payment in its birth state.
//
// The row is written BEFORE the provider is called, and that ordering is the
// reason the idempotency key can work at all: the provider's key is derived
// from the payment's identity, so a key derived from something that does not
// exist until the provider answers could not make a retry the same request.
//
// The insert is deliberately UNMAPPED for every failure but one. A second
// payment for the same (account, idempotency key) is the schema's
// payment_intents_idempotency_key, and it is translated here into the domain's
// ErrDuplicatePayment — the convergence signal the use case re-reads on, and
// the only condition this statement can lose to that is a fact about the
// caller's own request. Everything else (the v7 grammar, the amount's
// positivity, the owner guard that pairs the bucket with the account, the
// positive-refund ceilings) is this package handing the engine a row the domain
// already built, so a refusal there is infrastructure naming an impossible row
// and propagates wrapped.
//
// The insert is bracketed in a SAVEPOINT, and the reason is not the insert
// itself but what the caller does next. ErrDuplicatePayment is a CONVERGENCE
// SIGNAL: the use case answers it by re-reading the payment that already
// exists, in this same unit of work. In PostgreSQL a failed statement aborts
// the whole transaction — every statement after it answers SQLSTATE 25P02 and
// the commit at the end fails outright — so without the savepoint the re-read
// that makes two clicks of one button one payment would instead be a 500, on a
// path that is working exactly as designed. ROLLBACK TO SAVEPOINT undoes the
// failed statement and nothing else, leaving the unit of work alive. The same
// bracket and the same reason exist in the accounting adapter's ledger append.
func (r *paymentIntentsRepo) Create(ctx context.Context, intent payments.Intent) error {
	if err := requirePaymentUnitOfWork(r.store, ctx, fmt.Sprintf("create payment %s", intent.ID)); err != nil {
		return err
	}
	q := r.store.Querier(ctx)
	if _, err := q.ExecContext(ctx, `SAVEPOINT payment_intent_create`); err != nil {
		return fmt.Errorf("postgres: create payment %s: open savepoint: %w", intent.ID, err)
	}
	if _, err := q.ExecContext(ctx, insertPaymentIntent,
		string(intent.ID), intent.AccountID, intent.FundingBucketID, intent.AmountMinorUnits,
		intent.Currency, int64(intent.MinorUnitExponent), intent.Provider, intent.IdempotencyKey,
		string(intent.Status),
		nullText(intent.ProviderCheckoutRef), nullText(intent.ProviderPaymentRef), nullText(intent.CheckoutURL),
		intent.RefundedMinorUnits, intent.UncoveredRefundMinorUnits, intent.StateVersion,
		intent.ExpiresAt, intent.CreatedAt, intent.UpdatedAt); err != nil {
		if _, rollbackErr := q.ExecContext(ctx, `ROLLBACK TO SAVEPOINT payment_intent_create`); rollbackErr != nil {
			return fmt.Errorf("postgres: create payment %s: roll back to savepoint after %v: %w", intent.ID, err, rollbackErr)
		}
		if constraint, ok := constraintOfUniqueViolation(err); ok && constraint == paymentIntentsIdempotencyKey {
			return fmt.Errorf("postgres: create payment %s: %w", intent.ID, payments.ErrDuplicatePayment)
		}
		return conflictOf(fmt.Errorf("postgres: create payment %s: %w", intent.ID, err))
	}
	if _, err := q.ExecContext(ctx, `RELEASE SAVEPOINT payment_intent_create`); err != nil {
		return conflictOf(fmt.Errorf("postgres: create payment %s: release savepoint: %w", intent.ID, err))
	}
	return nil
}

func (r *paymentIntentsRepo) ByID(ctx context.Context, id payments.IntentID) (payments.Intent, error) {
	intent, err := scanPaymentIntent(r.store.Querier(ctx).QueryRowContext(ctx, selectPaymentIntentByID, string(id)))
	if err != nil {
		return payments.Intent{}, wrapPaymentIntentRead(err, "payment "+string(id))
	}
	return intent, nil
}

func (r *paymentIntentsRepo) ByAccountAndIdempotencyKey(ctx context.Context, accountID, key string) (payments.Intent, error) {
	intent, err := scanPaymentIntent(r.store.Querier(ctx).QueryRowContext(ctx, selectPaymentIntentByAccountAndKey,
		accountID, key))
	if err != nil {
		return payments.Intent{}, wrapPaymentIntentRead(err, fmt.Sprintf("payment for account %s under that key", accountID))
	}
	return intent, nil
}

func (r *paymentIntentsRepo) ByProviderCheckoutRef(ctx context.Context, provider, ref string) (payments.Intent, error) {
	intent, err := scanPaymentIntent(r.store.Querier(ctx).QueryRowContext(ctx, selectPaymentIntentByCheckoutRef,
		provider, ref))
	if err != nil {
		return payments.Intent{}, wrapPaymentIntentRead(err, fmt.Sprintf("payment for provider %s checkout %s", provider, ref))
	}
	return intent, nil
}

func (r *paymentIntentsRepo) ByProviderPaymentRef(ctx context.Context, provider, ref string) (payments.Intent, error) {
	intent, err := scanPaymentIntent(r.store.Querier(ctx).QueryRowContext(ctx, selectPaymentIntentByPaymentRef,
		provider, ref))
	if err != nil {
		return payments.Intent{}, wrapPaymentIntentRead(err, fmt.Sprintf("payment for provider %s payment %s", provider, ref))
	}
	return intent, nil
}

// The four guarded moves. Each is ONE statement carrying the states the caller
// read in its WHERE clause and the version bump in its SET list, and each
// answers with the row it wrote so a caller never has to re-read to learn what
// happened. Every one of them deliberately omits a state_version PREDICATE, and
// the omission is a fact about the port rather than a simplification: no member
// of persistence.PaymentIntents takes the version the caller read, so the
// predicate that could express "still what I read" for a move that stays on the
// same status has nothing to compare against. What the port does carry is the
// STATES the caller saw, and on this state machine that is nearly the whole
// guard: of the four moves only a refund can land on a status it started from
// (`partially_refunded` is idempotent on itself), and that is the one move whose
// WHERE clause carries a second predicate of its own — `refunded_minor_units <
// $reported`, a money-shaped guard rather than a state-shaped one. That
// predicate, not the from-states, is what arbitrates a race between two refund
// deliveries; RecordRefund below is where it is argued.

// RecordCheckout writes the provider's checkout reference and URL and moves the
// payment to checkout_open, in one statement.
//
// The status predicate is the arbiter and the reference's write-once trigger is
// the second refusal: a payment that already carries a checkout reference cannot
// reach this statement's `from` (`created`), because the only writer that sets
// the reference sets the status in the same breath — so the two predicates agree
// and the CAS never degrades into a re-point.
const recordPaymentCheckout = `
UPDATE control.payment_intents
SET provider_checkout_ref = $2,
    checkout_url = $3,
    status = 'checkout_open',
    state_version = state_version + 1,
    updated_at = $4
WHERE id = $1
  AND status = ANY($5::text[])
  AND provider_checkout_ref IS NULL`

func (r *paymentIntentsRepo) RecordCheckout(ctx context.Context, id payments.IntentID, checkout payments.OpenedCheckout, from []payments.Status, now time.Time) (bool, error) {
	if err := requirePaymentUnitOfWork(r.store, ctx, fmt.Sprintf("record checkout for payment %s", id)); err != nil {
		return false, err
	}
	res, err := r.store.Querier(ctx).ExecContext(ctx, recordPaymentCheckout,
		string(id), checkout.ProviderRef, checkout.URL, now, statusStrings(from))
	if err != nil {
		return false, conflictOf(fmt.Errorf("postgres: record checkout for payment %s: %w", id, err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: record checkout for payment %s: read rows affected: %w", id, err)
	}
	// False means the world moved — another attempt wrote its own reference, or
	// the payment is no longer in a state this move starts from. The caller
	// re-reads and returns the winner's payment; it does not overwrite, because
	// choosing between two live checkout sessions is not a decision this layer
	// can make.
	return n == 1, nil
}

// MoveStatus applies a status transition with the same discipline, and returns
// the payment as it now stands so the caller need not re-read.
//
// The trigger fires only when the status actually CHANGES, so a write that
// restates the state it found passes — which is what makes a move whose target
// is inside its own `from` set able to lose without being refused.
const movePaymentStatus = `
UPDATE control.payment_intents
SET status = $2,
    state_version = state_version + 1,
    updated_at = $3
WHERE id = $1
  AND status = ANY($4::text[])
RETURNING ` + paymentIntentColumns

func (r *paymentIntentsRepo) MoveStatus(ctx context.Context, id payments.IntentID, from []payments.Status, to payments.Status, now time.Time) (payments.Intent, bool, error) {
	if err := requirePaymentUnitOfWork(r.store, ctx, fmt.Sprintf("move payment %s to %s", id, to)); err != nil {
		return payments.Intent{}, false, err
	}
	intent, err := scanPaymentIntent(r.store.Querier(ctx).QueryRowContext(ctx, movePaymentStatus,
		string(id), string(to), now, statusStrings(from)))
	if errors.Is(err, sql.ErrNoRows) {
		// The verdict, not a miss. The payment is there; it is not in a state
		// this move starts from. Wrapped as an error it would turn every lost
		// race into a 5xx for a caller whose correct answer is to re-read.
		return payments.Intent{}, false, nil
	}
	if err != nil {
		return payments.Intent{}, false, conflictOf(fmt.Errorf("postgres: move payment %s to %s: %w", id, to, err))
	}
	return intent, true, nil
}

// RecordCapture records the provider's payment reference AND moves the payment
// to succeeded in one statement, which is not a convenience but the only shape
// the schema admits: `payment_intents_capture_shape` is a biconditional that
// ties a non-NULL provider_payment_ref to exactly the three post-capture
// statuses, and the transition trigger refuses status = 'succeeded' while the
// reference is NULL. Two statements would race the trigger against the second
// write; one statement satisfies both guards by construction.
//
// The reference is what the funding leg's command key is derived from, which is
// the reason the guard is a biconditional rather than an implication: a payment
// that reached succeeded without one is a payment whose credit cannot be
// re-derived, so a redelivery of the same capture could not converge on the leg
// it already wrote.
//
// WHAT KEEPS A REDELIVERY OFF `payment_intents_provider_payment_ref_key` IS THE
// FROM-LIST, and not the index, which is worth stating because the reasoning
// runs the other way from what a reader expects. The partial unique index would
// NOT fire on a redelivery: this statement writes the value the row already
// holds, and a row updated to its own value produces one index entry, which the
// index is satisfied by. So `succeeded` is excluded from the caller's from-list
// not because re-writing the reference would raise, but because re-writing it
// would SUCCEED — moving a funded payment's state_version forward a second time
// and re-deriving the funding command key from a reference nothing has since
// agreed to. The index is the second line of defence and it answers a different
// question: a DIFFERENT payment claiming the same capture, which is a
// contradiction rather than a retry. The arm below translates that one into a
// verdict, and the from-list is what keeps the ordinary redelivery from ever
// reaching it.
const recordPaymentCapture = `
UPDATE control.payment_intents
SET provider_payment_ref = $2,
    status = 'succeeded',
    state_version = state_version + 1,
    updated_at = $3
WHERE id = $1
  AND status = ANY($4::text[])
RETURNING ` + paymentIntentColumns

func (r *paymentIntentsRepo) RecordCapture(ctx context.Context, id payments.IntentID, providerPaymentRef string, from []payments.Status, now time.Time) (payments.Intent, bool, error) {
	if err := requirePaymentUnitOfWork(r.store, ctx, fmt.Sprintf("record capture for payment %s", id)); err != nil {
		return payments.Intent{}, false, err
	}
	intent, err := scanPaymentIntent(r.store.Querier(ctx).QueryRowContext(ctx, recordPaymentCapture,
		string(id), providerPaymentRef, now, statusStrings(from)))
	if errors.Is(err, sql.ErrNoRows) {
		return payments.Intent{}, false, nil
	}
	if constraint, ok := constraintOfUniqueViolation(err); ok && constraint == paymentIntentsProviderPaymentRefKey {
		// TWO OF OUR OWN PAYMENTS NOW NAME THE SAME PROVIDER PAYMENT, and the
		// honest answer is `moved = false` rather than an error. The caller's
		// vocabulary for a `false` is a state conflict, which it quarantines and
		// answers 2xx; wrapped as an error instead, `conflictOf` does not map
		// 23505 and the delivery answers 500, the provider retries, the retry
		// answers 500, and there is no quarantine row anywhere for a human —
		// the exact shape the ceiling in RecordRefund below is written to avoid.
		//
		// Refusing to CONCEAL it is the other half: the transaction is rolled
		// back rather than continued, and the caller's re-read finds its own
		// payment still waiting for a capture, so the delivery is answered as a
		// conflict and the operator has the event id to look up.
		return payments.Intent{}, false, nil
	}
	if err != nil {
		return payments.Intent{}, false, conflictOf(fmt.Errorf("postgres: record capture for payment %s: %w", id, err))
	}
	return intent, true, nil
}

// RecordRefund writes the refund projection the provider reported and moves the
// payment to the status the projection implies, in one statement.
//
// The write is ABSOLUTE — `refunded_minor_units = $2`, the whole figure the
// provider reported for this payment — and never an increment the caller
// computed. The port's own comment on this member is where the argument lives;
// the short form is that an increment is a value derived from a read THIS
// CALLER made, two deliveries of two refunds process independently, and both
// reading the same base and both adding over-counts into a state nothing can
// correct (every later report is then below what the row holds, so the delivery
// that would fix it reads as a contradiction and is quarantined). An absolute
// write has no read in it: two concurrent writers name the same figure, the row
// lock serialises them, and the second finds the guard already satisfied.
//
// The `refunded_minor_units < $2` predicate is what makes the write MONOTONE
// and gives `moved = false` its second meaning. A delivery that reports a figure
// this row already holds — or a smaller one, from a reordered or retried
// delivery — changes nothing and fires zero rows. The caller re-reads and tells
// "already satisfied" from "the row is not in a from-state" by comparing what it
// reported against what the row holds; that comparison belongs above this layer
// because it is a statement about the payment, not about the statement.
//
// `uncovered` is ABSOLUTE too, and for the same reason — it is the shortfall
// this caller measured against the balance as it stands, not an increment to
// add to a figure it read earlier. The delta shape is worth naming because it
// looks harmless and is not: the balance is a property of the whole payment,
// not of one delivery, so a caller that measured "the part of THIS delivery the
// balance could not cover" would let every delivery spend the same balance
// again — refunds of 40 and then 30 out of a balance of 10 would store 30 + 20
// = 50 while the payment's real shortfall is 70 − 10 = 60. It is not a rounding
// either way; the figure is what an operator resolves the case with, and a
// figure that drifts from the case is worse than no figure at all.
//
// Nothing in the provider's report carries it, so unlike the total it is this
// plane's own measurement — which is exactly why it is written whole by the
// delivery that measured it, rather than summed into by every delivery that
// arrives. The guard below is what keeps a stale measurement out: only the
// statement that ADVANCES the refunded total runs at all.
//
// The ceiling is in the WHERE clause as well as in the schema's CHECK, and that
// duplication is deliberate. `payment_intents_refunded_within_capture` refuses
// a refund past what was captured, and it is the guard that holds when the
// caller's read is stale — but a stale read that hit the CHECK would surface as
// a constraint violation, which reaches the provider as a 5xx and a retry the
// provider can never satisfy, because the same bytes would be refused again.
// Stated in the WHERE clause, the same race fires zero rows and is reported as
// moved = false, which the caller answers by quarantining the delivery and
// returning 2xx.
//
// refundRef is carried for the port's signature and is deliberately not written.
// The domain derives a refund's command key from it (RefundRecord.CommandKey),
// and the delivery that reported the refund is recorded in payment_events with
// that reference as its provider_payment_ref; payment_intents has no column for
// it, because a payment's refunds are many and the intent carries their TOTAL.
// The reference is therefore not lost — it is on the delivery row that claims
// it — and this parameter is unused rather than dropped silently.
const recordPaymentRefund = `
UPDATE control.payment_intents
SET refunded_minor_units = $2,
    uncovered_refund_minor_units = $3,
    status = $4,
    state_version = state_version + 1,
    updated_at = $5
WHERE id = $1
  AND status = ANY($6::text[])
  AND refunded_minor_units < $2
  AND $2 <= amount_minor_units
RETURNING ` + paymentIntentColumns

func (r *paymentIntentsRepo) RecordRefund(ctx context.Context, id payments.IntentID, refundRef string, totalRefunded, uncovered int64, from []payments.Status, to payments.Status, now time.Time) (payments.Intent, bool, error) {
	if err := requirePaymentUnitOfWork(r.store, ctx, fmt.Sprintf("record refund for payment %s", id)); err != nil {
		return payments.Intent{}, false, err
	}
	intent, err := scanPaymentIntent(r.store.Querier(ctx).QueryRowContext(ctx, recordPaymentRefund,
		string(id), totalRefunded, uncovered, string(to), now, statusStrings(from)))
	if errors.Is(err, sql.ErrNoRows) {
		return payments.Intent{}, false, nil
	}
	if err != nil {
		return payments.Intent{}, false, conflictOf(fmt.Errorf("postgres: record refund for payment %s: %w", id, err))
	}
	return intent, true, nil
}

// settlePaymentEvent writes the verdict on a delivery this unit of work
// recorded.
//
// The WHERE clause is the dedup key and nothing else — the same three columns,
// in the same order, as `payment_events_delivery_key` — so the statement can
// address exactly one row and cannot be widened to a bulk rewrite by a caller
// that passed an empty string. `provider` leads the index and is not optional;
// an UPDATE with an empty provider matches nothing rather than everything,
// which is the failure direction a money-adjacent statement has to fail in.
//
// RowsAffected is checked rather than ignored: settling a delivery that is not
// on file means the caller's unit of work is not the one that recorded it, and
// that is a defect rather than an empty result.
const settlePaymentEvent = `
UPDATE control.payment_events
SET disposition = $4
WHERE provider = $1
  AND provider_account_key = $2
  AND provider_event_id = $3`

// Settle writes the delivery's verdict, in the unit of work that recorded it.
func (r *paymentEventsRepo) Settle(ctx context.Context, key payments.DeliveryKey, disposition payments.EventDisposition) error {
	if err := requirePaymentUnitOfWork(r.store, ctx, fmt.Sprintf("settle provider delivery %s", key.EventID)); err != nil {
		return err
	}
	res, err := r.store.Querier(ctx).ExecContext(ctx, settlePaymentEvent,
		key.Provider, key.ProviderAccountKey, key.EventID, string(disposition))
	if err != nil {
		return conflictOf(fmt.Errorf("postgres: settle provider delivery %s: %w", key.EventID, err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: settle provider delivery %s: read rows affected: %w", key.EventID, err)
	}
	if n == 0 {
		return fmt.Errorf("postgres: settle provider delivery %s: no delivery on file under that key: %w",
			key.EventID, persistence.ErrNotFound)
	}
	return nil
}

func (r *paymentIntentsRepo) ListForAccount(ctx context.Context, accountID string, page persistence.PaymentPage) ([]payments.Intent, error) {
	// One more row than the caller asked for, so `has_more` is derived from the
	// probe row rather than from a page that happens to be short — which is also
	// what a concurrent write produces, and a client that treated one as the end
	// is the bug the extra row exists to prevent.
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list payments for account %s: %w", accountID, err)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, listPaymentIntentsForAccount,
		accountID, string(page.After), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list payments for account %s: %w", accountID, err)
	}
	defer func() { _ = rows.Close() }()

	intents := make([]payments.Intent, 0, limit)
	for rows.Next() {
		intent, scanErr := scanPaymentIntent(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("postgres: list payments for account %s: %w", accountID, scanErr)
		}
		intents = append(intents, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list payments for account %s: %w", accountID, err)
	}
	return intents, nil
}

// statusStrings widens the domain's status set for a text[] parameter. The
// column is text and the domain's Status is a string, so the widening is the
// only translation and a status the schema's CHECK does not know simply matches
// nothing.
func statusStrings(statuses []payments.Status) []string {
	widened := make([]string, len(statuses))
	for i, status := range statuses {
		widened[i] = string(status)
	}
	return widened
}

// scanPaymentIntent reads one intent row out of any single-row result — the
// three lookups, the list page, and the three compare-and-swaps' RETURNING — so
// the projection cannot differ depending on which statement found the row.
//
// The three nullable text columns are scanned into their own locals and
// assigned only when the database says so. NULL becomes the domain's empty
// string, which is how this aggregate spells absence, and the direction matters
// in both: a row with no checkout reference is a payment whose checkout has not
// been opened — a "not yet", not an "unknown" — and the delivery resolution
// reads exactly that distinction.
func scanPaymentIntent(row rowScanner) (payments.Intent, error) {
	var (
		intent                                      payments.Intent
		id, accountID, bucketID, currency, provider string
		idempotencyKey, status                      string
		exponent                                    int64
		checkoutRef, paymentRef, checkoutURL        sql.NullString
	)
	if err := row.Scan(&id, &accountID, &bucketID, &intent.AmountMinorUnits,
		&currency, &exponent, &provider, &idempotencyKey, &status,
		&checkoutRef, &paymentRef, &checkoutURL,
		&intent.RefundedMinorUnits, &intent.UncoveredRefundMinorUnits, &intent.StateVersion,
		&intent.ExpiresAt, &intent.CreatedAt, &intent.UpdatedAt); err != nil {
		return payments.Intent{}, err
	}
	intent.ID = payments.IntentID(id)
	intent.AccountID = accountID
	intent.FundingBucketID = bucketID
	intent.Currency = currency
	intent.MinorUnitExponent = int(exponent)
	intent.Provider = provider
	intent.IdempotencyKey = idempotencyKey
	intent.Status = payments.Status(status)
	intent.ProviderCheckoutRef = checkoutRef.String
	intent.ProviderPaymentRef = paymentRef.String
	intent.CheckoutURL = checkoutURL.String
	return intent, nil
}

// wrapPaymentIntentRead turns an intent lookup's failure into the port's
// vocabulary: a miss is ErrNotFound and only a miss, because every other
// failure is infrastructure and must stay distinguishable from "this platform
// never opened that payment" — the answer the webhook quarantines an unknown
// delivery on.
func wrapPaymentIntentRead(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("postgres: %s: %w", what, persistence.ErrNotFound)
	}
	return fmt.Errorf("postgres: %s: %w", what, err)
}

// ---------------------------------------------------------------------------
// payment_events — the idempotency ledger.
// ---------------------------------------------------------------------------

type paymentEventsRepo struct {
	store persistence.Store
}

// paymentEventColumns is the delivery row, in scan order.
const paymentEventColumns = `provider, provider_account_key, provider_event_id, provider_payment_ref,
       intent_id, kind, amount_minor_units, currency, disposition, occurred_at, recorded_at`

// insertPaymentEvent writes one verified delivery.
//
// There is deliberately NO `ON CONFLICT DO NOTHING`, and the absence is the
// point of the whole table. DO NOTHING would make a redelivery report success —
// a row affected count of zero, an error of nil — and a caller could not then
// tell "already recorded" from "recorded now". The two answers differ in
// whether the caller should credit, so the caller is handed the distinction:
// the unique violation surfaces as the domain's ErrDuplicateEvent, and the
// credit path is skipped on it. Letting the engine's own uniqueness emit the
// verdict is also why this is not a SELECT-then-INSERT: whichever transaction
// inserts first holds the key, and the loser is told so by that key rather than
// by a lock — a unique violation is a decision the engine has already made, and
// a lock would be this layer making it again, worse.
//
// recorded_at's COALESCE is the column's own `DEFAULT now()` kept reachable: a
// caller that supplied a reading gets it, and a caller that supplied none gets
// the DATABASE's clock rather than a Go zero time being written into a NOT NULL
// column. The default is now() rather than a Go clock for the same reason the
// payments domain takes its Now from the store: two replicas minting timelines
// from their own clocks produce two timelines nothing can interleave.
const insertPaymentEvent = `
INSERT INTO control.payment_events
    (provider, provider_account_key, provider_event_id, provider_payment_ref,
     intent_id, kind, amount_minor_units, currency, disposition, occurred_at, recorded_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, COALESCE($11, now()))`

// The delivery read behind ByIntent. newest first, which is what the
// reconciliation and console surfaces ask for, and the tiebreaker on `id` is
// required rather than decorative: `recorded_at` is not unique — one ingestion
// page or one webhook burst can stamp several rows inside one microsecond — and
// an order with a tie in it is an order a reader cannot page.
const selectPaymentEventsByIntent = `
SELECT ` + paymentEventColumns + `
FROM control.payment_events
WHERE intent_id = NULLIF($1, '')::uuid
ORDER BY recorded_at DESC, id DESC
LIMIT $2`

// Record writes the delivery. The caller MUST have checked InUnitOfWork: this
// insert is the FIRST write in the webhook's unit of work, and the ordering is
// what makes a crash safe rather than merely unlucky — see the port.
//
// The insert is bracketed in a SAVEPOINT, and here the bracket is not a
// nicety but the difference between a redelivery working and a redelivery
// being a permanent 500. A duplicate is the ORDINARY outcome of a provider
// retrying — it is exactly what this table exists to detect — and the use
// case's response to ErrDuplicateEvent is to answer the provider 200 and
// return, which COMMITS the unit of work. A unique violation aborts the
// transaction it fired in, so without the savepoint that commit fails
// ("commit unexpectedly resulted in rollback"), the handler answers 5xx, and
// the provider retries forever against an endpoint that can never succeed. The
// ROLLBACK TO SAVEPOINT undoes the failed insert alone and leaves the
// transaction able to commit the absence of an effect, which is the correct
// and only durable statement a duplicate makes.
func (r *paymentEventsRepo) Record(ctx context.Context, event payments.ProviderEventRecord) error {
	if err := requirePaymentUnitOfWork(r.store, ctx, fmt.Sprintf("record provider delivery %s", event.EventID)); err != nil {
		return err
	}
	q := r.store.Querier(ctx)
	// The savepoint name is fixed on purpose: Record runs at most once per unit
	// of work on the webhook path, and a fixed name keeps the bracket in this
	// call's own identity rather than in a counter that could drift from it.
	if _, err := q.ExecContext(ctx, `SAVEPOINT payment_event_record`); err != nil {
		return fmt.Errorf("postgres: record provider delivery %s: open savepoint: %w", event.EventID, err)
	}
	if _, err := q.ExecContext(ctx, insertPaymentEvent,
		event.Provider, event.ProviderAccountKey, event.EventID, nullText(event.ProviderPaymentRef),
		nullText(string(event.IntentID)), nullText(event.Kind), nullAmount(event.AmountMinorUnits),
		nullText(event.Currency), recordedDisposition, nullTime(event.OccurredAt),
		nullTime(event.RecordedAt)); err != nil {
		if _, rollbackErr := q.ExecContext(ctx, `ROLLBACK TO SAVEPOINT payment_event_record`); rollbackErr != nil {
			return fmt.Errorf("postgres: record provider delivery %s: roll back to savepoint after %v: %w", event.EventID, err, rollbackErr)
		}
		if constraint, ok := constraintOfUniqueViolation(err); ok && constraint == paymentEventsDeliveryKey {
			return fmt.Errorf("postgres: record provider delivery %s: %w", event.EventID, payments.ErrDuplicateEvent)
		}
		return conflictOf(fmt.Errorf("postgres: record provider delivery %s: %w", event.EventID, err))
	}
	if _, err := q.ExecContext(ctx, `RELEASE SAVEPOINT payment_event_record`); err != nil {
		return conflictOf(fmt.Errorf("postgres: record provider delivery %s: release savepoint: %w", event.EventID, err))
	}
	return nil
}

func (r *paymentEventsRepo) ByIntent(ctx context.Context, intentID payments.IntentID, limit int) ([]payments.ProviderEventRecord, error) {
	if limit < 1 {
		return nil, fmt.Errorf("postgres: list deliveries for payment %s: limit must be at least 1, got %d", intentID, limit)
	}
	// An empty intent id is not an error and not a wildcard: NULLIF makes it
	// NULL, `intent_id = NULL` matches nothing, and the read answers "this
	// payment has no deliveries" — which is also the true answer for the
	// deliveries that resolved to no payment at all, since those carry no
	// intent by design.
	rows, err := r.store.Querier(ctx).QueryContext(ctx, selectPaymentEventsByIntent, string(intentID), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list deliveries for payment %s: %w", intentID, err)
	}
	defer func() { _ = rows.Close() }()

	events := make([]payments.ProviderEventRecord, 0, limit)
	for rows.Next() {
		event, scanErr := scanPaymentEvent(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("postgres: list deliveries for payment %s: %w", intentID, scanErr)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list deliveries for payment %s: %w", intentID, err)
	}
	return events, nil
}

// scanPaymentEvent reads one delivery row.
//
// The amount is scanned into a sql.NullInt64 and becomes a pointer only when the
// database says a figure was there. That round trip is the domain's "absent is
// not zero" doctrine made mechanical: a NULL must come back nil and a stored 0
// must come back a pointer to 0, because a delivery that reported no amount and
// a delivery that reported an amount of zero are different sentences and only
// one of them is a payment. Scanning the column straight into an int64 would
// collapse them at the first layer, and every comparison above would then be
// comparing a fact with a non-fact.
func scanPaymentEvent(row rowScanner) (payments.ProviderEventRecord, error) {
	var (
		event                                payments.ProviderEventRecord
		provider, accountKey, eventID        string
		disposition                          string
		kind, paymentRef, intentID, currency sql.NullString
		amount                               sql.NullInt64
		occurred                             sql.NullTime
	)
	if err := row.Scan(&provider, &accountKey, &eventID, &paymentRef,
		&intentID, &kind, &amount, &currency, &disposition, &occurred, &event.RecordedAt); err != nil {
		return payments.ProviderEventRecord{}, err
	}
	event.OccurredAt = timeOrZero(occurred)
	event.Provider = provider
	event.ProviderAccountKey = accountKey
	event.EventID = eventID
	event.ProviderPaymentRef = paymentRef.String
	event.IntentID = payments.IntentID(intentID.String)
	event.Kind = kind.String
	if amount.Valid {
		value := amount.Int64
		event.AmountMinorUnits = &value
	}
	event.Currency = currency.String
	event.Disposition = payments.EventDisposition(disposition)
	return event, nil
}

// ---------------------------------------------------------------------------
// payment_quarantine — the unapplied-delivery evidence.
// ---------------------------------------------------------------------------

type paymentQuarantineRepo struct {
	store persistence.Store
}

// insertPaymentQuarantine writes one delivery this build could not apply.
//
// There is no uniqueness here and no conflict clause, and that is the design
// rather than an omission: quarantines are EVIDENCE, not dedup. Two identical
// unapplied deliveries are two rows, and an operator reading them learns that
// the provider tried twice. The table cannot be a second dedup ledger either —
// an unverifiable delivery's event id is precisely the thing that cannot be
// trusted, which is why this is a separate table from payment_events at all.
//
// The nullable columns carry the domain's absence faithfully. provider_event_id
// is NULL for a delivery that did not verify (the column's own comment: a NOT
// NULL here would force this plane to invent an id, and inventing an id to store
// an unauthenticated body is the forgery vector the signature check exists to
// prevent). payload is NULL for the same case and for the opposite reason — an
// unauthenticated body is an attacker's free text and is deliberately not kept —
// while a VERIFIED but uninterpretable body is kept, because that is the evidence
// an operator resolves the row with.
const insertPaymentQuarantine = `
INSERT INTO control.payment_quarantine
    (provider, provider_account_key, provider_event_id, provider_payment_ref, intent_id,
     kind, reason, amount_minor_units, currency, payload, occurred_at, recorded_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, COALESCE($12, now()))`

// Record writes a delivery this build could not apply. It never surfaces a
// duplicate: see the statement's comment.
//
// ProviderPaymentRef IS WRITTEN, and the column it lands in is the reason the
// quarantine exists in this shape. A delivery naming a payment this platform
// never opened — the `unknown_payment` case — is identified by nothing else on
// the row: it has no intent_id (that is what unknown means), and the reference
// is the ONLY handle an operator has on the provider's own record of the
// transaction. Dropping it would leave the row resolvable only by decoding the
// raw payload, which is a job for a human reading bytes rather than for a
// query, and it would make every unknown_payment row indistinguishable from
// every other one of the same amount.
//
// It is deliberately NOT recoverable from the delivery's payment_events row, in
// the cases this table holds: a quarantined delivery is precisely one whose
// event may be absent from that table — the unknown-payment branch claims no
// dedup key at all, on purpose, so that the provider's retry stays applicable —
// and payment_events.disposition cannot represent a quarantine anyway. Evidence
// a reader has to reconstruct from a rule that holds sometimes is not evidence.
func (r *paymentQuarantineRepo) Record(ctx context.Context, record payments.QuarantineRecord) error {
	if err := requirePaymentUnitOfWork(r.store, ctx, fmt.Sprintf("record quarantined delivery for provider %s", record.Provider)); err != nil {
		return err
	}
	if _, err := r.store.Querier(ctx).ExecContext(ctx, insertPaymentQuarantine,
		record.Provider, record.ProviderAccountKey, nullText(record.EventID),
		nullText(record.ProviderPaymentRef),
		nullText(string(record.IntentID)), nullText(record.Kind), string(record.Reason),
		nullAmount(record.AmountMinorUnits), nullText(record.Currency), nullBytes(record.Payload),
		// occurred_at is nullable because the provider's timestamp is a claim and
		// a delivery that made none has none to record — while the domain's
		// OccurredAt is a plain time.Time whose zero value is that absence. The
		// asymmetry is deliberate and it is the only place on this surface where
		// a zero time means "not said" rather than "an instant nobody could
		// have meant": a provider's claimed instant is the one column here that
		// the provider may simply not have supplied.
		nullTime(record.OccurredAt),
		nullTime(record.RecordedAt)); err != nil {
		return conflictOf(fmt.Errorf("postgres: record quarantined delivery for provider %s: %w", record.Provider, err))
	}
	return nil
}
