package payments

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// digestReference is the fixed-length, collision-resistant reduction a command
// key is built from.
//
// It is a digest and not a truncation, and the difference is the whole reason
// this function exists rather than a slice. A truncated reference is still a
// reference: cut at 256 characters, two provider payments whose references
// share that prefix produce the SAME key, and B6's convergence would then treat
// the second payment's credit as a redelivery of the first and hand back the
// first leg. One customer is funded, the second customer's webhook is answered
// 2xx as a duplicate, and nothing anywhere reports a discrepancy — the ledger is
// internally consistent, the provider is satisfied, and a customer paid for
// nothing. A digest cannot do that: 256 bits of it, and two distinct references
// colliding is a condition this system will not reach in the lifetime of any
// deployment.
//
// The length is a property of the ALGORITHM, not of the input, which is the
// second reason. A key built by concatenation has a length that is a function of
// what a third party sent, and a command key has a 256-character ceiling (B6's
// validateCommandKey, and the schema's ledger_entries_command_key_scope
// restates it). A provider whose identifiers grew past that ceiling would have
// its webhooks refused at the domain, before any statement runs — a customer
// paid, a permanent 500, a retry loop that can never succeed. Deriving a
// fixed-length segment means the ceiling is unreachable by construction rather
// than defended by a bound someone might forget to tighten.
//
// It is SHA-256 over the UTF-8 bytes, hex-encoded, which is 64 characters and
// therefore ASCII. The ASCII property is worth stating because it is not
// obvious from the construction: B6 checks its command key in BYTES (Go's len)
// while the schema checks it in CHARACTERS (char_length), and those two agree
// for ASCII and disagree for everything else. A key this function produces can
// never be the case that separates them, so a provider that sends a
// non-ASCII reference is carried safely rather than being the first caller to
// discover a divergence between two bounds that were always going to disagree.
func digestReference(reference string) string {
	sum := sha256.Sum256([]byte(reference))
	return hex.EncodeToString(sum[:])
}

// Intents is the port this package's consumer satisfies. It is declared here,
// in the domain, and not in the persistence port, for one reason: the two
// operations below are the ones a payment can be ASKED to do, and a port that
// is written from the database's shape grows a member every time a column is
// added, which is how a repository starts answering questions nobody asked it.
type Intents interface {
	// Create writes a new payment intent in its birth state.
	Create(ctx IntentContext, intent Intent) error
	// ByID returns the intent with id, or ErrNotFound.
	ByID(ctx IntentContext, id IntentID) (Intent, error)
	// ByProviderReference returns the intent a provider's own identifier names.
	ByProviderReference(ctx IntentContext, provider, reference string) (Intent, error)
	// Move applies a status transition, compare-and-swapped: the row moves to
	// to only while it still shows the from state the caller read. False means
	// the world moved and the caller re-reads — never that half a transition
	// landed.
	Move(ctx IntentContext, id IntentID, from []Status, to Status, updatedAt time.Time) (bool, error)
	// RecordEvent writes the durable record of one provider delivery. The
	// first write in a webhook's unit of work, and the arbiter of the race
	// between two concurrent deliveries of the same event.
	RecordEvent(ctx IntentContext, event ProviderEventRecord) error
	// RecentEvents returns a bounded window of a payment's recorded
	// deliveries, newest first, for the reconciliation and console surfaces.
	RecentEvents(ctx IntentContext, intentID IntentID, limit int) ([]ProviderEventRecord, error)
}

// IntentContext is the context a port call runs under: it is
// context.Context in the adapter and nothing more. The alias exists so this
// package's port signatures cannot drift toward database/sql's shape — a
// port that took *sql.Tx would be a port that names a transaction it can
// never own, and a port that took context.Context and nothing else cannot.
type IntentContext = interface {
	Deadline() (deadline time.Time, ok bool)
	Done() <-chan struct{}
	Err() error
	Value(key any) any
}

// Intent is one payment: what was asked for, what the provider says happened,
// and the lineage that lets a later reconciliation say which event funded it.
//
// The account and the funding bucket are on the intent and are set ONCE, at
// creation, by the use case that opened the payment — never by an event. That
// is the single most important structural decision in this package, and it is
// worth being blunt about why: a webhook is a signed message from a third party
// about a third party's own world, and a signature proves the message was sent
// by the party we configured. It proves nothing about which of OUR customers
// the message concerns. An implementation that resolved an account from a
// provider payload has given the payload the authority to say who gets money,
// and the failure is not a bug that shows up in testing — it is a forged
// metadata field on a real, validly-signed, real provider checkout, which is
// something any customer can set through the provider's own dashboard.
//
// So the lookup direction is fixed. An event names a PROVIDER REFERENCE. The
// reference resolves to an intent THIS PLATFORM CREATED. The intent carries the
// account and the bucket that were fixed when the payment was opened. The
// account is a consequence of a stored row, and a stored row is something this
// plane wrote with a session's account id in it.
type Intent struct {
	// ID is this platform's identifier for the payment, and the root of
	// everything derived from it: the provider's idempotency key, the command
	// key namespace, the reconciliation reference. Minted here, v7, because
	// payments are history and a sort by id is a rough sort by mint time.
	ID IntentID

	// AccountID and FundingBucketID are the payment's OWNERSHIP, fixed at
	// creation. They are what a webhook resolves to and what a topup funds,
	// and there is no code path that writes them after creation — the
	// migration's identity-immutability trigger refuses the attempt at the
	// engine, and the domain refuses it here, because a payment that could
	// change hands is a payment whose ledger legs belong to one account and
	// whose money landed in another.
	AccountID       string
	FundingBucketID string

	// AmountMinorUnits is the CANONICAL funding amount: what the ledger will
	// be told, in integer minor units, in the deployment's settlement
	// currency. It is not the provider's figure and never becomes it — see
	// ProviderAmountMinorUnits for the comparison, and the application for
	// what a disagreement does.
	AmountMinorUnits int64

	// Currency is the ISO 4217 code AmountMinorUnits is denominated in, and
	// it is the reason B6's no-per-row-currency rule is not violated here.
	// ADR 0004 recorded that amounts are integer minor units in "the single
	// platform-wide settlement currency" — a rule that assumes the currency
	// is one thing, stated everywhere, which is true of the LEDGER and false
	// of a payment, because a payment is denominated in whatever a CUSTOMER
	// was charged in and that is a fact about a transaction with a third
	// party. So the payment carries it, once, and the platform's
	// configuration must agree with it before any event is accepted: an event
	// in another currency is not converted and not credited, because this
	// plane has no FX rate, no rate source, and no authority to pick one.
	Currency string

	// MinorUnitExponent is how many decimal places this currency has, and it
	// is stored rather than derived because "minor unit" is not a universal
	// idea. USD's hundredth and JPY's whole yen are both "minor units" and
	// they differ by a factor of a hundred, so an amount of 1000 is a
	// thousand yen in one and ten dollars in the other. A payment that did not
	// carry this would be comparing a number whose unit it does not know.
	MinorUnitExponent int

	// Provider names which payment provider opened this. It is a bounded
	// string and not a foreign key to a providers table, for the reason
	// reconciliation_runs chose the same shape: the vocabulary belongs to
	// the adapter, it is one value today, and a new value arrives as a code
	// change with its own CHECK revision rather than as a silent insert.
	Provider string

	// ProviderCheckoutRef is the provider's identifier for the checkout, set
	// when the hosted checkout is opened. It is the reference a later event
	// names, which is why it is the intent's lookup key and why an event
	// arriving before it is set is a "not yet" rather than an "unknown".
	ProviderCheckoutRef string

	// ProviderPaymentRef is the provider's identifier for the PAYMENT — the
	// economic event, as distinct from the delivery that reported it. It is
	// set when the capture is first seen, it is what the funding leg's
	// command key is DERIVED from, and it is unique per provider so that a
	// second event describing the same capture cannot become a second
	// credit. See TopUpCommandKey for why the delivery's id is the wrong
	// input for that derivation.
	ProviderPaymentRef string

	// IdempotencyKey is the caller's key for opening the checkout, unique per
	// account. Two clicks of a "Top up" button produce one payment, not two
	// open checkouts, and a client that retries after a dropped response
	// gets the payment it already has rather than a second one.
	IdempotencyKey string

	// Status is where the payment stands, and the only field an event may
	// move. Everything else on this struct is either fixed (above) or a
	// figure derived from the ledger (below).
	Status Status

	// RefundedMinorUnits is the total the provider has returned so far. It is
	// a PROJECTION of the refund legs, kept here for the read surface and the
	// ceiling check, and the authoritative total is the sum of the legs — the
	// same rule funding_buckets' balances follow, and for the same reason: a
	// cached figure is right until something disagrees, and the legs win.
	RefundedMinorUnits int64

	// UncoveredRefundMinorUnits is the part of a refund this platform could
	// not take out of the balance because the money had been spent.
	//
	// It exists because B6's algebra forbids the alternative. A refund is a
	// debit; a debit that drives settled below zero is refused by
	// ApplyTo, by the echo's WHERE clause, and by the schema's projection
	// CHECK — three refusals that exist because a balance may not record debt
	// the platform does not have. A customer who topped up 10000, spent 9000,
	// and was then refunded is owed 10000 back and has 1000 left; the two
	// facts cannot both be true in a balance, and the balance wins, because
	// the other answer is an account that reads -9000. So the platform
	// debits what it holds, records the rest here as an amount owed to the
	// customer, and the amount is a real figure an operator has to resolve —
	// the money left this platform, and a settlement of negative balances
	// against a future topup is a decision with accounting and legal
	// consequences that this package is not entitled to make silently.
	UncoveredRefundMinorUnits int64

	// StateVersion is the optimistic-concurrency marker. Every move bumps it,
	// the adapter computes the bump rather than taking it as a parameter,
	// and it is what makes "move from the state I read" a statement that can
	// lose.
	StateVersion int64

	CreatedAt time.Time
	UpdatedAt time.Time
	// ExpiresAt is when an un-completed checkout stops being waitable. It is
	// a local deadline and NOT a terminal fact about the money: an event
	// arriving after it is still honoured, because the provider is the only
	// party that gets to say whether the customer paid.
	ExpiresAt time.Time

	// CheckoutURL is the provider's hosted-checkout URL, returned verbatim
	// and never parsed. It is held on the intent so a returning customer can
	// be sent back to a checkout they started, which is why it is stored
	// rather than only returned.
	CheckoutURL string
}

// IntentID is this platform's identifier for a payment: a minted version-7
// uuid, distinct as a type so a payment id cannot be passed where a bucket id
// is expected by a signature accident.
type IntentID string

// Claim records that a payment has reached a status, and reports whether the
// payment was in a state that could legally move there.
//
// The store's verdict on the compare-and-swap is the whole of the call, and it
// is reported by whether an error comes back at all. Nil means the row still
// showed one of `from` and is now `to`, so the caller may do the work the new
// status licenses — a credit, a refund. ErrInvalidTransition means it may not,
// because somebody else already moved the row, or because the move is not one
// this state machine has. The two are deliberately NOT distinguished, and the
// reason is that they do not need to be: a compare-and-swap that lost and a
// transition that was illegal have the same consequence for the credit, which
// is none. The caller re-reads, and the re-read distinguishes them for the
// human who has to resolve the leftover — a payment that is already succeeded
// is a duplicate delivery, and a payment that is expired with a succeeded event
// needs a human to look at the timing.
//
// Two things are refused before any statement runs, and both are the caller's
// own mistake rather than the world's: a `to` this build has no status for, and
// a claim that names no state to move from. They are ErrInvalidReference, and
// no store ever sees them.
//
// A store FAILURE is handed back unchanged, and the value a caller will meet on
// it is the miss. "There is no payment with this id" arrives as the STORE's
// sentinel — the persistence port's ErrNotFound, the word the port's own read
// contracts use for a miss — and not as this package's ErrUnknownPayment,
// because no branch here translates one into the other. That is a layering
// fact stated rather than left to be discovered: a domain call is handing out
// an infrastructure package's sentinel, and a caller matching errors.Is against
// payments.ErrUnknownPayment will never see it. The domain's own word for the
// condition is minted where a miss MEANS something — the application resolves a
// delivery through a stored reference and turns the port's ErrNotFound into
// payments.ErrUnknownPayment there, because only the application knows the miss
// came from resolving a delivery rather than from reading an id — and that is
// also why there is no translation here to reach for: this function cannot tell
// a missing payment from a store that could not answer, and a refusal it
// invented would hide the second behind the first.
func Claim(ctx IntentContext, store Intents, id IntentID, from []Status, to Status, now time.Time) error {
	if !knownStatus(to) {
		return fmt.Errorf("%w: %q is not a status this build has a rule for", ErrInvalidReference, to)
	}
	if len(from) == 0 {
		return fmt.Errorf("%w: a claim names the states it may move from", ErrInvalidReference)
	}
	applied, err := store.Move(ctx, id, from, to, now)
	if err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("%w: %s did not move to %s", ErrInvalidTransition, id, to)
	}
	return nil
}

// EventDisposition is what became of one provider delivery: the verdict this
// plane reached about a delivery it authenticated, which is the answer the
// webhook is given and the record of what this build did with the message.
//
// The three values are genuinely different states of the world rather than
// three ways of spelling success, and a single boolean would lose exactly the
// information an operator needs:
//
//   - Applied: the delivery's claim was CARRIED OUT. The payment moved, and
//     where the claim was a capture the money was credited under the payment's
//     own command key. It is the verdict of a delivery that did the work it
//     claimed to do — not merely one that was read and recorded.
//   - Duplicate: the delivery was already recorded, so its effect, if any, is
//     already durable and repeating it changes nothing. It is a success rather
//     than a refusal — the re-notification the provider sends by design has
//     been answered — and the difference from Applied is one an operator
//     reading the ledger needs, because "we did this now" and "we had already
//     done this" are different answers to "did this delivery fund anything".
//   - Quarantined: the delivery authenticated and was recorded, but this build
//     could not act on it — against any payment, or against the one it named.
//     It is nobody's fault yet, and it is not a refusal of the message itself:
//     the delivery is kept as evidence, and the REASON lives on
//     control.payment_quarantine, which is the row an operator resolves — the
//     verdict alone would not tell them which check refused the delivery.
type EventDisposition string

const (
	// DispositionApplied means the delivery's claim was carried out: the
	// payment moved, and a capture among them was credited.
	DispositionApplied EventDisposition = "applied"
	// DispositionDuplicate means the delivery was already recorded, so its
	// effect is already durable and replaying it changes nothing.
	DispositionDuplicate EventDisposition = "duplicate"
	// DispositionQuarantined means the delivery authenticated and was recorded,
	// but this build could not act on it. The reason it could not is on the
	// control.payment_quarantine row.
	DispositionQuarantined EventDisposition = "quarantined"
)

// ProviderEventRecord is one provider delivery as this platform records it.
//
// The row is the IDEMPOTENCY LEDGER, and it is the whole reason a second
// delivery of the same event is a duplicate rather than a second credit. It is
// written FIRST in the unit of work that applies the event — first, and not
// last, and not after the effect — because the order is what makes a crash
// safe. An insert that committed before the credit would swallow the credit's
// own redelivery and lose the money silently; a credit that committed before
// the insert would leave the plane unable to say which event produced it. One
// transaction with the insert first has neither window: a crash rolls back
// both, and a redelivery re-runs.
//
// EventID is the DEDUP identity and ProviderPaymentRef is the ECONOMIC one, and
// they are separate columns for the reason they are separate ideas. Two
// deliveries of one capture share the payment reference and must be collapsed
// by it; two deliveries are the same delivery only if they share the event id.
// A schema that used one column for both would be able to enforce exactly one
// of those two rules and not the other.
type ProviderEventRecord struct {
	// EventID is the provider's delivery identity, unique per provider and
	// per provider account. It is what makes a redelivery a duplicate.
	EventID string
	// ProviderAccountKey is the provider's own identifier for the merchant
	// account the delivery belongs to, and it is part of the dedup key rather
	// than a decoration. A provider that scopes its event ids per merchant
	// hands two different customers the same id, and a global uniqueness
	// claim on (provider, event_id) would then refuse the second customer's
	// insert, absorb it, and leave a paying customer unfunded with no error
	// anywhere. Carrying the merchant makes that collision structurally
	// impossible instead of merely unlikely.
	ProviderAccountKey string
	// Provider is the provider's name, part of the same key.
	Provider string

	// IntentID is the payment this delivery resolved to, or empty for a
	// delivery that named none. It is empty precisely when the delivery is
	// quarantined as an unknown payment, which is the disposition an event
	// for a checkout this platform has no record of gets.
	IntentID IntentID

	// Kind is the provider's own vocabulary for the delivery, carried
	// through as the provider wrote it so a reconciliation can show a
	// customer what the provider said rather than this platform's
	// paraphrase of it.
	Kind string

	// ProviderPaymentRef and the amount are what the delivery CLAIMED. They
	// are recorded on a quarantined row too, because the claim is the
	// evidence an operator resolves the row with, and a quarantine that
	// discarded the figure would make the row a mystery rather than a
	// question.
	ProviderPaymentRef string
	AmountMinorUnits   *int64
	Currency           string

	// Disposition is what this plane did with it, and Reason is the
	// enumeration of why. Reason is a fixed vocabulary rather than a
	// formatted sentence on purpose: a reason carrying provider text into a
	// column an operator surface will eventually render is a
	// cross-site-scripting vector built into the storage layer, and the
	// cheapest way to prevent it is for the value to have never been able to
	// hold anything else.
	Disposition EventDisposition
	Reason      QuarantineReason

	// Payload is the authenticated bytes, recorded only for a quarantine.
	//
	// It is the only place in this plane where a third party's raw content
	// is stored, and the reason it is bounded and not kept for a
	// successfully-applied event is asymmetry of cost: an applied event
	// needs no evidence, because the ledger leg and the intent's own state
	// ARE the evidence; a quarantined one needs it, because without the
	// bytes there is no way to answer "what did the provider actually
	// send" — and it is retained under the same bound as the fact
	// ingestion's quarantine, in the same spirit, with the same refusal to
	// store a provider blob indefinitely.
	Payload []byte

	OccurredAt time.Time
	RecordedAt time.Time
}

// QuarantineReason is the closed set of reasons a delivery was not applied.
//
// It is an enum and not a formatted message because every value here is
// something a human will read, and a message built by concatenating a
// provider's own strings is a message an operator's browser will one day
// render. Enumerating them also makes the SET of things that can go wrong
// legible: a deployment that wanted an alert on one of these names it, and a
// value outside the set cannot be inserted at all.
type QuarantineReason string

const (
	// ReasonUnverifiable: a body this platform holds did not verify against
	// the configured signing secret.
	//
	// NO WEBHOOK EVER PRODUCES THIS ROW, and that is the reason it needs
	// stating rather than the reason it should be deleted. A delivery whose
	// signature does not verify is refused at the transport with a 400 and
	// is NOT written anywhere: an unverified body is a stranger's free text,
	// and a table that records one is a table anyone on the internet can
	// write to, in whatever volume they like, forever. The refusal is logged
	// instead — a log line is not a row, and a refusal that leaves nothing
	// behind costs an attacker a byte rather than a disk write.
	//
	// What the value is FOR is the one caller this platform could trust with
	// it: a reconciliation replaying bodies this platform stored earlier,
	// which can fail to verify for a reason that is nobody's attack at all —
	// a rotated signing secret. That is a real operator question ("why does
	// this stored delivery no longer verify?") and it is the case this member
	// exists for. That caller does not exist yet — the reconciliation is the
	// change ADR 0013 §12 lists as out of scope — and this member is what it
	// would write.
	ReasonUnverifiable QuarantineReason = "unverifiable"

	// ReasonUnknownKind: the provider's event kind is not one this build
	// interprets. The most common reason in practice, and the reason a
	// quarantine is not an error: providers add event types, and a build
	// that refused them loudly would page someone every time the provider
	// shipped a feature.
	ReasonUnknownKind QuarantineReason = "unknown_kind"

	// ReasonUnknownPayment: the delivery named a payment this platform
	// never opened. Real, and correct to refuse: the answer is to record it
	// and let a reconciliation decide, not to credit a payment this plane
	// has no record of creating.
	ReasonUnknownPayment QuarantineReason = "unknown_payment"

	// ReasonAmountMismatch: the provider's figure disagrees with what this
	// platform sent. Never converted and never credited: the platform's
	// canonical amount is the one the ledger will be told, and a provider
	// reporting something else means one of the two is wrong, and this
	// plane cannot tell which.
	ReasonAmountMismatch QuarantineReason = "amount_mismatch"

	// ReasonCurrencyMismatch: the provider's currency is not the currency
	// this payment was opened in. This plane has no FX rate and no authority
	// to choose one, so the correct answer is to refuse rather than to
	// convert.
	ReasonCurrencyMismatch QuarantineReason = "currency_mismatch"

	// ReasonStale: the delivery's signed timestamp is outside the accepted
	// window. Distinguished from ReasonUnverifiable on purpose: the signature
	// is GOOD, so this is not a stranger's free text — it is a real delivery
	// (or a replay of one) that this platform could not use in time.
	//
	// NO WEBHOOK PRODUCES THIS ROW EITHER, and for the reason that makes it a
	// sibling of ReasonUnverifiable rather than a special case. The transport
	// answers a stale delivery 400 and writes NOTHING, because a stale
	// delivery is the shape a REPLAY takes: a body that once verified stays
	// verifiable forever, so an endpoint that recorded every stale arrival
	// would be an endpoint any holder of one captured body could fill with
	// rows indefinitely. The refusal is logged instead — a log line is not a
	// row, and the same argument that keeps an unverified body out of the
	// table keeps a replayed old one out of it.
	//
	// What the value is FOR is the one caller this platform could trust with
	// it: a reconciliation replaying bodies this platform stored earlier,
	// where an old body recomputes as stale through nothing more sinister
	// than the passage of time. That caller does not exist yet — the
	// reconciliation is the change ADR 0013 §12 lists as out of scope — and
	// this member is what it would write.
	ReasonStale QuarantineReason = "stale"

	// ReasonStateConflict: the delivery is authentic and understood, and
	// names a transition this state machine does not have. The provider
	// describing a payment this platform believes it has already had the
	// final word on is a contradiction worth a human.
	ReasonStateConflict QuarantineReason = "state_conflict"

	// ReasonAccountClosed: the payment's account was closed before the
	// capture landed. The money is real and the credit has nowhere to go,
	// so it is an operator's refund to issue out of band rather than a
	// balance to create.
	ReasonAccountClosed QuarantineReason = "account_closed"

	// ReasonBucketClosed: the payment's funding bucket was closed before the
	// capture landed. B6's buckets are terminal, so this is the same
	// situation as a closed account in its consequences and different in its
	// cause.
	ReasonBucketClosed QuarantineReason = "bucket_closed"

	// ReasonRefundAheadOfCapture: a refund was reported for a payment whose
	// capture this platform has not recorded. The provider is within its
	// rights — a refund can arrive before the capture that preceded it if the
	// two are delivered out of order — and the right answer is to record it
	// and wait rather than to book a correction against a leg that does not
	// exist yet.
	ReasonRefundAheadOfCapture QuarantineReason = "refund_ahead_of_capture"

	// ReasonRefundCeiling: the refund would take more than the payment
	// captured, or more than remains of what it captured. B6's arithmetic
	// refuses the debit, so the answer is a record and a human rather than a
	// balance that reads as debt.
	ReasonRefundCeiling QuarantineReason = "refund_ceiling"
)

// QuarantineRecord is one unapplied delivery as this platform records it.
//
// It is a type of its own rather than a ProviderEventRecord with a disposition,
// and the split is the whole point. A ProviderEventRecord is written to the
// dedup ledger, whose identity rests on the provider's event id being real; a
// quarantined delivery is precisely the case where that id is either absent or
// untrustworthy, so filing it in that table would need an invented id — and
// inventing an id to store an unauthenticated body is the forgery vector the
// signature check exists to prevent. Two records, two tables, one meaning each.
//
// Every field below is EVIDENCE rather than grammar, which is why so many are
// optional. The row's job is to let an operator answer "what did the provider
// actually send, and why would this build not take it" — and a record that
// discarded the fields it could not parse would leave that question
// unanswerable. The bounds on it are bounds on SIZE, not on shape: a payload is
// bounded because an unbounded one is a way to fill a disk, and it has no lower
// bound because an empty body is a real thing a broken provider sends.
type QuarantineRecord struct {
	// Provider is the provider whose delivery this was.
	Provider string
	// ProviderAccountKey is this deployment's own identifier for its merchant
	// account, carried so a multi-merchant deployment can tell its
	// quarantines apart. It is this platform's configured value and never
	// anything read out of the unverified body.
	ProviderAccountKey string

	// EventID is the delivery's claimed identity, and it is EMPTY FOR AN
	// UNVERIFIED delivery on purpose: the claim is only worth recording when
	// something authenticated it. For a well-signed but uninterpretable
	// delivery it is the provider's real id and is recorded, because a human
	// resolving the row needs to find the delivery in the provider's own
	// dashboard.
	EventID string

	// IntentID is the payment this delivery resolved to, when it resolved to
	// one. It is empty for the two cases that resolve to nothing: an
	// unverified body, and a verified delivery naming a payment this platform
	// never opened.
	IntentID IntentID

	// Kind, ProviderPaymentRef, AmountMinorUnits and Currency are what the
	// delivery claimed, recorded as claimed and never acted on.
	Kind               string
	ProviderPaymentRef string
	AmountMinorUnits   *int64
	Currency           string

	// Reason is why this build would not apply it, from the closed vocabulary.
	Reason QuarantineReason

	// Payload is the authenticated body, present only when the delivery
	// VERIFIED. An unverified body is an attacker's free text — recording it
	// verbatim would make this table a place where anyone on the internet can
	// write, and the absence of the bytes is what keeps the table's contents
	// to things our provider actually said.
	Payload []byte

	// OccurredAt is the provider's claimed timestamp, when it claimed one.
	OccurredAt time.Time
	// RecordedAt is when this platform recorded it.
	RecordedAt time.Time
}

// knownReason reports whether a reason is one this build can record. It is the
// same shape as knownStatus, and for the same reason: the column is a closed
// set, and a value outside it is a code that will not compile into the
// enumeration the operator surface renders.
func knownReason(r QuarantineReason) bool {
	switch r {
	case ReasonUnverifiable, ReasonUnknownKind, ReasonUnknownPayment,
		ReasonAmountMismatch, ReasonCurrencyMismatch, ReasonStale,
		ReasonStateConflict, ReasonAccountClosed, ReasonBucketClosed,
		ReasonRefundAheadOfCapture, ReasonRefundCeiling:
		return true
	}
	return false
}
