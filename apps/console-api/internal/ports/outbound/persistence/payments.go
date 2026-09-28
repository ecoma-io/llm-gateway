package persistence

import (
	"context"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/payments"
)

// The payment integration's durable surface (B15): the three repositories the
// payment use cases are built on, and the one rule that shapes all three.
//
// THE RULE: every write below runs inside a unit of work, and the adapter
// refuses one that does not. It is stated here, in the port, because the
// consequence of losing it is not a slow query — it is a customer funded
// twice or funded not at all, and both arrive silently.
//
// The webhook's write path is the reason. A provider delivery must be recorded
// and its funding leg written as ONE transaction: an insert that committed
// before the credit would swallow the credit's own redelivery and lose the
// money with no error anywhere; a credit that committed before the insert
// would leave this plane unable to say which delivery produced it. Both halves
// exist only because they share a transaction, and a repository that silently
// autocommitted on the pool when the caller's context had lost its transaction
// would break that sharing without breaking any signature. So the write
// members below check InUnitOfWork and refuse — the same posture the projection
// change recorder takes, and for the same class of reason.
//
// The READS do not carry that rule, and the asymmetry is deliberate: a read
// outside a unit of work sees a committed state, which is a correct answer to
// a correct question. It is only the pairing of a write with another write that
// has to be atomic.

// PaymentIntents is the payment aggregate's durable surface.
//
// The two compare-and-swap members are the interesting ones. MoveStatus and
// RecordTransfer both answer a bool, and the bool means "the row still showed
// what you read, and it is now what you wrote" — never "nothing happened". A
// caller that receives false re-reads and decides again; a caller that treated
// false as an error would turn every lost race into a 5xx, and a lost race here
// is the ordinary outcome of a provider redelivering a delivery, not a fault.
type PaymentIntents interface {
	// Create inserts a new payment. A second payment for the same
	// (account, idempotency key) surfaces as payments.ErrDuplicatePayment —
	// the schema's payment_intents_idempotency_key, translated here and
	// nowhere above it, because a caller that had to pattern-match a driver
	// string would be branching on a message rather than a condition.
	Create(ctx context.Context, intent payments.Intent) error

	// ByID returns the payment with id, or ErrNotFound.
	ByID(ctx context.Context, id payments.IntentID) (payments.Intent, error)

	// ByAccountAndIdempotencyKey returns the payment a repeated request names,
	// or ErrNotFound. It is the read behind convergence: a client that
	// retried the call that opens a top-up gets the payment it already has
	// rather than a second one.
	ByAccountAndIdempotencyKey(ctx context.Context, accountID, key string) (payments.Intent, error)

	// ByProviderTransferRef returns the payment a provider's TRANSFER
	// identifier names — the destination it issued for one payment — or
	// ErrNotFound.
	//
	// This is the lookup that keeps a third party's payload from choosing
	// whose money moves. An event names a reference; the reference resolves to
	// a row THIS PLATFORM WROTE; the row carries the account and the bucket.
	// There is deliberately no member that resolves an account from anything
	// the provider said, because such a member would be a way to say "credit
	// whatever the payload names" and a signature proves who sent a message,
	// not what the message may do.
	//
	// The reference is the destination and not a memo, and the difference is
	// the whole reason this lookup is trustworthy: a destination is issued by
	// the provider and is where the money can only have gone, while a memo is
	// the customer's own free text that a bank may rewrite.
	ByProviderTransferRef(ctx context.Context, provider, ref string) (payments.Intent, error)

	// ByProviderPaymentRef returns the payment a provider's PAYMENT
	// identifier names — the id of the money rather than of the destination it
	// arrived at — or ErrNotFound.
	//
	// It exists because a refund delivery carries the second and not the
	// first: a delivery reporting money going back names the payment, not the
	// account it was paid into, so it never mentions the reference a capture
	// was resolved by. Before this member existed every refund this platform
	// received resolved against a transfer-id column with a payment id and was
	// quarantined as a payment it could not find — the refund path was
	// reachable in the domain and unreachable in production.
	//
	// It is a SECOND lookup and not a replacement: a capture's transfer
	// reference is written before the customer is ever shown it, so it is the
	// reference a capture resolves by; this one is the fallback and the
	// refund's only route. The same rule governs both — the reference matches
	// a column THIS PLATFORM wrote, and there is still no member that resolves
	// a payment from anything else a provider said.
	//
	// The column is UNIQUE per provider (the schema's partial unique index),
	// so at most one row can answer.
	ByProviderPaymentRef(ctx context.Context, provider, ref string) (payments.Intent, error)

	// RecordTransfer writes the destination the provider issued — its
	// reference, its image, the bank it sits at and the name it is held in —
	// moving the payment to awaiting_transfer, and reports whether the payment
	// still showed the state the caller read.
	//
	// It is a compare-and-swap on status and state version TOGETHER, and both
	// predicates are load-bearing. The status predicate alone would let two
	// concurrent attempts both write a destination — the second overwriting
	// the first's reference, and the account one customer was already told to
	// pay into being replaced by another nobody holds. The version predicate
	// alone would let a stale writer overwrite a payment that had since
	// succeeded. The statement that implements this carries the status in its
	// WHERE clause, because the transition trigger fires on the ROW and a
	// guarded statement that matched zero rows fires nothing at all — which is
	// what makes the CAS atomic rather than optimistic.
	RecordTransfer(ctx context.Context, id payments.IntentID, transfer payments.TransferInstructions, from []payments.Status, now time.Time) (bool, error)

	// MoveStatus applies a status transition with the same compare-and-swap
	// discipline and the same both-predicates rule, and returns the payment as
	// it now stands so the caller need not re-read.
	MoveStatus(ctx context.Context, id payments.IntentID, from []payments.Status, to payments.Status, now time.Time) (payments.Intent, bool, error)

	// RecordCapture records the provider's payment reference and whatever the
	// refund projection already holds, moving the payment to succeeded.
	//
	// It is separate from MoveStatus rather than being a caller that sets
	// fields afterwards, because the two writes have to be ONE statement: a
	// payment that reached succeeded without its capture reference recorded
	// is a payment whose funding leg cannot be re-derived, and the schema's
	// own transition trigger refuses to let status become succeeded while
	// provider_payment_ref is NULL. Doing it in two statements would race the
	// trigger against the second write.
	RecordCapture(ctx context.Context, id payments.IntentID, providerPaymentRef string, from []payments.Status, now time.Time) (payments.Intent, bool, error)

	// RecordRefund writes the refund projection the provider reported and moves
	// the payment to the status the projection implies, in one statement, with
	// the same CAS discipline.
	//
	// totalRefunded is the ABSOLUTE figure the provider reported for this
	// payment — everything that has gone back over the payment's whole life —
	// and NOT a delta this caller computed. That distinction is the whole of
	// this member's correctness, and it is worth being exact about, because the
	// shape it replaced looked safer and was not:
	//
	// `refunded_minor_units = refunded_minor_units + $delta` reads like the
	// race-free form — the increment happens in the database, so no write is
	// lost — and it is what this member used to specify. But a DELTA is not a
	// figure the provider ever reported; it is a difference between the figure
	// it reported and a figure THIS CALLER READ EARLIER, and two deliveries of
	// two refunds on one payment are processed independently. Both read the same
	// base, both subtract it from their own report, and both add: a payment
	// refunded 3000 and then 6000 against a base of zero stores 9000 while the
	// provider says 6000. The schema's `refunded_within_capture` CHECK does not
	// catch it, because 9000 is still inside a 10000 capture, and nothing
	// afterwards can: every later report of 6000 or 7000 is now BELOW what this
	// row holds, so the row reads as a contradiction and the delivery that would
	// have corrected it is quarantined. Over-counting is worse than a lost
	// update for exactly that reason — a lost update is corrected by the next
	// write, and this is corrected by nothing.
	//
	// Written absolutely, the same race is inert: both writers name the same
	// figure, the row lock serialises them, and the second finds the guard below
	// already satisfied by the first. The statement is a monotone move to a
	// reported state, so it is idempotent, it cannot move backwards, and a
	// redelivery of a figure already stored changes nothing and reports
	// moved = false.
	//
	// moved = false therefore means "this delivery added nothing", which is two
	// situations the caller must tell apart and can: the row already holds this
	// figure or more (the claim is satisfied — answer it as applied), or the row
	// is not in one of the `from` statuses (a genuine disagreement — quarantine
	// it). The caller re-reads the row for that question; see the application's
	// applyRefund.
	//
	// `uncovered` crosses as an ABSOLUTE figure too — the part of the CUMULATIVE
	// refunded total the balance cannot account for, as this caller measured it
	// — and it is written whole rather than added to. The delta shape was the
	// natural-looking alternative and it is arithmetically wrong: the balance is
	// a property of the payment, not of one delivery, so a caller measuring "the
	// part of THIS delivery the balance could not cover" lets every delivery
	// spend the same balance again — refunds of 40 and then 30 out of a balance
	// of 10 would store 50 where the payment's shortfall is 60. The figure is
	// the one an operator resolves the case with, so a figure that drifts from
	// the case is worse than none.
	//
	// It carries no guard of its own, and does not need one: the statement runs
	// only for a delivery that ADVANCES refunded_minor_units, so the measurement
	// written is always the one taken against the figure that just landed.
	RecordRefund(ctx context.Context, id payments.IntentID, refundRef string, totalRefunded, uncovered int64, from []payments.Status, to payments.Status, now time.Time) (payments.Intent, bool, error)

	// ListForAccount returns at most page.Limit of an account's payments,
	// newest first. The account predicate is in the WHERE clause and is the
	// FIRST argument, so another account's payment is a row the query never
	// returned — the same answer, at the same cost, a genuinely absent row
	// gives.
	ListForAccount(ctx context.Context, accountID string, page PaymentPage) ([]payments.Intent, error)
}

// PaymentEvents is the idempotency ledger's durable surface: one row per
// verified delivery, and the table whose uniqueness is what makes a
// redelivery a duplicate rather than a second credit.
//
// There is no delete member, and that absence is the design rather than an
// omission: an event that has been recorded has been recorded, and a table that
// could lose one could not answer "which delivery funded this payment".
//
// There IS one update member, and it is worth saying why it is not the same
// shape as a delete. A delivery's row is written before its verdict is known —
// the insert has to be the arbiter of the race between two concurrent
// deliveries, and the verdict depends on what the payment turns out to be in —
// so the disposition the row carries at insert time is provisional and Settle
// is what makes it true. Settle cannot change anything else on the row, cannot
// change which delivery the row IS, and cannot revive or remove one; and
// because it runs inside the same unit of work as the insert, no reader ever
// observes the provisional value. A revision of WHAT A DELIVERY CLAIMED is what
// this surface still refuses, and always will: an evidence table that could be
// edited answers a different question than the one it exists for.
type PaymentEvents interface {
	// Record writes the delivery. A delivery already on file — same provider,
	// same provider account, same event id — surfaces as
	// payments.ErrDuplicateEvent, the schema's payment_events_delivery_key
	// translated here.
	//
	// The caller MUST have checked InUnitOfWork. This insert is the FIRST
	// write in the webhook's unit of work and the arbiter of the race between
	// two concurrent deliveries: whichever transaction inserts first holds the
	// key, and the loser is told so by that key rather than by a lock, because
	// a unique violation is a decision the engine has already made and a lock
	// would be this layer making it again, worse.
	Record(ctx context.Context, event payments.ProviderEventRecord) error

	// Settle writes the verdict the delivery earned, naming the row by its
	// DeliveryKey rather than by the record — so an implementation can only
	// touch the disposition, and a caller cannot accidentally revise what the
	// delivery claimed.
	//
	// The caller MUST have checked InUnitOfWork and MUST be in the same unit of
	// work as the Record that wrote the row. Those two facts together are what
	// make the provisional disposition on a fresh row unobservable, and they
	// are the whole reason an update is admissible here at all: a Settle in a
	// LATER transaction would be a revision of history that a reader can see,
	// which is exactly what this table's absence of an update member is there
	// to prevent.
	//
	// A key that names no row is not an error and not a no-op to be silent
	// about: it is reported, because the only way to reach it is a caller that
	// settled a delivery this unit of work never recorded.
	Settle(ctx context.Context, key payments.DeliveryKey, disposition payments.EventDisposition) error

	// ByIntent returns at most limit of a payment's recorded deliveries,
	// newest first, for the reconciliation and console surfaces.
	ByIntent(ctx context.Context, intentID payments.IntentID, limit int) ([]payments.ProviderEventRecord, error)
}

// PaymentQuarantine is the unapplied-delivery ledger.
//
// It is a separate surface from PaymentEvents and not a disposition value on
// it, and the reason is structural rather than stylistic: an unverifiable
// delivery's event id is NOT trustworthy — that is what "unverifiable" means —
// so it cannot live in a table whose uniqueness rests on that id being real.
// Filing it there would require inventing an id, and inventing an id to store
// an unauthenticated body is precisely the forgery vector the signature check
// exists to prevent.
type PaymentQuarantine interface {
	// Record writes a delivery this build could not apply. It never surfaces a
	// duplicate: quarantines are evidence rather than dedup, so two identical
	// unapplied deliveries are two rows, and an operator reading them learns
	// that the provider tried twice.
	Record(ctx context.Context, record payments.QuarantineRecord) error
}

// PaymentPage is the payments list's request: a keyset position and a page
// size.
//
// The cursor is a payment ID rather than a `(created_at, id)` pair, and that is
// a decision about the order the list is in: ids here are version-7, so an id
// IS a mint instant with 48 bits of milliseconds at the front, and ordering by
// id descending is ordering newest-first. The id is unique, so the keyset is a
// total order with no ties to break, and one column is the whole cursor. A
// compound cursor over `(created_at, id)` would say the same thing in two
// values and would need the index below to carry both.
//
// The alternative — ordering by the `created_at` column, which is what a reader
// means by "newest" — was rejected because `created_at` is not unique: two
// payments opened in the same microsecond would need a tiebreaker, and a keyset
// that page-breaks inside a tie skips or repeats a row. The two orders agree
// anyway, because the id's own timestamp is minted from the SAME database
// clock reading that sets `created_at`.
type PaymentPage struct {
	// After is the exclusive lower bound on the keyset. The zero value is the
	// beginning of the list, not a cursor that matches nothing.
	After payments.IntentID
	// Limit is the number of rows wanted; the adapter asks for one more.
	Limit int
}

// There is deliberately no cleanup member here, and the absence is worth a
// sentence because a reviewer will look for one. Quarantined deliveries are
// evidence, `payment_quarantine` carries an unconditional engine guard against
// deletion for that reason, and a port member that could not run would be a
// member that lies about what this surface can do. Retention for that table is
// a real operational question and it belongs to a change that brings a retention
// policy, a cutoff configuration and the operator surface that sets them —
// which is also the change that would have to decide whether the guard it is
// asking to pierce is one it should.
