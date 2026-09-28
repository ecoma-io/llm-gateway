package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/payments"
	paymentprovider "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/payments"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The payment integration's use cases (B15).
//
// This file is where the two halves of a payment meet: a customer-facing top-up
// that opens a payment, and a provider-facing webhook that finishes one. The
// rule that shapes every line of it is stated once, in
// ports/outbound/payments, and repeated here because it is the whole design:
//
//	A verified event is a CLAIM about a payment, not a payment.
//
// The signature answers "did our provider produce these bytes"; it says nothing
// about which of our customers the bytes concern. So an event names a PROVIDER
// REFERENCE, that reference resolves to a payment THIS PLATFORM WROTE, and the
// payment carries the account and the funding bucket that were fixed when it
// was opened. There is no path from a provider payload to an account id, and
// that absence is the security property — a provider's event metadata is
// something a customer can often set through the provider's own dashboard, so
// an implementation that trusted it would be funding whoever filled in the
// field.
//
// The second rule is about transactions, and it is the one a future edit is
// most likely to break. A provider delivery is recorded and its funding leg is
// written in ONE unit of work, and the funding leg is reached by threading the
// TRANSACTION's context — `txCtx`, never the caller's `ctx` — into the
// accounting call. Hand the request context to TopUp and the two writes commit
// independently: the delivery is recorded, the credit is not, no error is
// raised anywhere, and the provider is told 200. That failure is invisible in
// review and invisible in a unit test built on fakes, which is why the
// atomicity claim is proven on real PostgreSQL in the integration tests and
// nowhere else.

// topUpWriter is the slice of the accounting use cases the webhook path holds.
//
// It is one method, and the narrowness is the design rather than tidiness. The
// webhook's job is to add funded money to a bucket, and that is exactly what
// TopUp does; the accounting use cases also offer Adjust, Hold, ReleaseHold,
// Settle and CloseBucket, and every one of them is something a provider's
// webhook has no business doing. A webhook that could adjust a balance would be
// a webhook that could correct a customer's ledger on the strength of a
// third party's message, and the correction primitive is precisely the one that
// needs an operator's identity behind it. Taking the whole *Accounting here
// would make that possible by accident; taking this interface makes it
// impossible on purpose.
type topUpWriter interface {
	TopUp(ctx context.Context, bucketID accounting.FundingBucketID, amountMinorUnits int64, commandKey accounting.CommandKey) (accounting.Bucket, error)
}

// accountFunding is the slice that opens an account's PAYG bucket.
//
// It exists because a payment can arrive for an account that has never had a
// bucket: the account-creation choreography opens one, but an account created
// before that choreography existed, or one whose bucket was opened by a path
// that failed, has a payment intent naming a bucket that does not exist yet.
// The alternative — refusing the delivery — would quarantine a real customer's
// real money over a bookkeeping gap this platform caused.
type accountFunding interface {
	OpenAccountFunding(ctx context.Context, accountID commerce.AccountID) (accounting.Bucket, error)
}

// bucketReader is the read the refund path needs: a bucket's balance as it
// stands, for computing how much of a refund the money on hand cannot cover.
//
// It is a read and only a read. The refund path has no use for a write to a
// bucket, and a reader interface that also exposed one would be the correction
// primitive this whole path exists to avoid reaching for.
type bucketReader interface {
	Bucket(ctx context.Context, bucketID accounting.FundingBucketID) (accounting.Bucket, error)
}

// PaymentsSettings is the deployment's own identity in the payment conversation:
// which provider this plane talks to and which merchant account at that
// provider is ours.
//
// It is a struct rather than two more constructor parameters because the two
// travel together and a caller that supplied one and forgot the other would get
// a provider name of "" — which is not a provider, and would silently make
// every stored payment unresolvable.
type PaymentsSettings struct {
	// Provider names which provider adapter this deployment uses. It is the
	// value stored on every payment row and the value every delivery is
	// matched against, so a deployment that changed it would stop resolving
	// its own history — which is why it is configuration rather than a
	// per-request choice.
	Provider string
	// ProviderAccountKey is this deployment's identifier for its merchant
	// account at the provider, and it is part of the webhook dedup key. See
	// the event record's own comment: a provider that scopes its event ids per
	// merchant hands two customers the same id, and a dedup key that ignored
	// the merchant would absorb the second customer's delivery and leave them
	// unfunded with no error anywhere.
	ProviderAccountKey string
}

// Payments is the payment integration's use cases.
type Payments struct {
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

// providerName is the configured provider, in the spelling every stored row
// carries.
func (p *Payments) providerName() string { return p.settings.Provider }

// providerAccountKey is this deployment's merchant identifier at the provider.
func (p *Payments) providerAccountKey() string { return p.settings.ProviderAccountKey }

// NewPayments builds the payment use cases around the ports they need. It
// panics on a nil port for the reason every constructor in this package does: a
// port this use case was promised and did not get is a wiring defect, and the
// middle of a webhook — after a delivery is recorded and before the credit
// lands — is a strictly worse place to learn about it.
func NewPayments(
	store persistence.Store,
	intents persistence.PaymentIntents,
	events persistence.PaymentEvents,
	quarantine persistence.PaymentQuarantine,
	clock persistence.Clock,
	accounts persistence.Accounts,
	buckets bucketReader,
	funding accountFunding,
	ledger topUpWriter,
	provider paymentprovider.Transfers,
	offers TopUpCatalogue,
	settings PaymentsSettings,
) *Payments {
	switch {
	case store == nil:
		panic("application: NewPayments requires the store")
	case intents == nil:
		panic("application: NewPayments requires the payment intents repository")
	case events == nil:
		panic("application: NewPayments requires the payment events repository")
	case quarantine == nil:
		panic("application: NewPayments requires the payment quarantine repository")
	case clock == nil:
		panic("application: NewPayments requires the database clock")
	case accounts == nil:
		panic("application: NewPayments requires the accounts repository")
	case buckets == nil:
		panic("application: NewPayments requires the bucket reader")
	case funding == nil:
		panic("application: NewPayments requires the account funding use case")
	case ledger == nil:
		panic("application: NewPayments requires the accounting top-up primitive")
	case provider == nil:
		panic("application: NewPayments requires the payment provider")
	// There is deliberately no check on the catalogue being non-empty, and the
	// absence is a decision rather than an oversight. A deployment that sells no
	// top-up offers is a real deployment — a self-hosted install whose operator
	// funds accounts by hand, or one whose provider credentials are not yet
	// configured — and config.Payments makes that state reachable by omitting
	// one variable. Its console must still load and must still list whatever
	// payments exist; what it must not have is a working top-up button. Every
	// path that could open a payment already refuses correctly against an empty
	// catalogue, because an offer id that is not in the map is not an offer:
	// BeginTransfer answers InvalidRequest with the same sentence an unknown id
	// gets, and the offers read answers an empty list rather than an error. A
	// panic here would turn "this deployment does not sell top-ups" into a
	// process that will not start, which is a worse answer to a legitimate
	// configuration than a screen with the button absent.
	case settings.Provider == "":
		panic("application: NewPayments requires the provider this deployment talks to")
	}
	return &Payments{
		store:      store,
		intents:    intents,
		events:     events,
		quarantine: quarantine,
		clock:      clock,
		accounts:   accounts,
		buckets:    buckets,
		funding:    funding,
		ledger:     ledger,
		provider:   provider,
		offers:     offers,
		settings:   settings,
	}
}

// TopUpOffer is one thing an account may buy, priced by the server.
//
// It exists because a client must never send an amount. A browser-supplied
// amount is not a smaller version of a pricing policy — it is the absence of
// one, and a top-up surface that accepted one would be a surface where the
// customer decides how much money to move and this platform merely agrees. The
// offer inverts that: the console names an offer, the server holds the price,
// and the amount that reaches the ledger is a figure the deployment chose.
//
// The currency rides the offer rather than a deployment-wide setting, and that
// is what lets this plane check a provider's claim without configuring a
// settlement currency it does not otherwise need. An offer is denominated in
// one currency with one exponent; the intent carries them; the provider's event
// must agree with them or it is quarantined.
type TopUpOffer struct {
	// ID is the offer's stable, operator-visible name. It is what the console
	// sends and what a deployment's configuration is keyed by, so it must not
	// change when the price does — a renamed offer is a broken client.
	ID string
	// AmountMinorUnits is the price in integer minor units, strictly
	// positive.
	AmountMinorUnits int64
	// Currency is the ISO 4217 code, uppercase.
	Currency string
	// MinorUnitExponent is that currency's decimal places.
	MinorUnitExponent int
	// Label is what the offer is called on the console. It is the operator's
	// own words for what a customer is buying — "Starter credit", "Annual
	// top-up" — and it is deliberately NOT derived from the amount: an offer
	// whose only name is its price is one an operator cannot describe, and the
	// console must never assemble a price into a sentence of its own, because
	// a formatted amount is a second pricing authority that can disagree with
	// the first one silently.
	//
	// It is display text and nothing else. Nothing in this package reads it,
	// compares it or stores it: it rides to the console so a chooser can render
	// something a human recognises, and the amount on the wire is the one that
	// decides what is charged.
	Label string
}

// TopUpCatalogue is the offers a deployment sells: the order they were
// declared in, and a lookup into them.
//
// It is a struct rather than a bare map because the two questions asked of it
// have different shapes and a map can only answer one of them well. The
// application LOOKS UP by the identifier a client sent, which is a map's own
// operation. The console LISTS what the deployment sells, and a list has to
// come back in the same order every time — ranging a map would hand the
// chooser Go's iteration order, so a customer's options would reshuffle
// between page loads and the operator would have lost the one lever they have
// over how their own price list reads.
//
// A duplicate id is not representable, and that is the constructor's doing
// rather than the type's: `NewTopUpCatalogue` refuses one instead of letting a
// later entry silently win, because which price a customer is charged is not a
// question that may depend on ordering. `config` refuses the same thing at
// load time; this refuses it again because a catalogue can be built by a test
// or by a future caller that never went through config.
type TopUpCatalogue struct {
	byID  map[string]TopUpOffer
	order []TopUpOffer
}

// NewTopUpCatalogue builds a catalogue from offers in declaration order.
//
// It panics on a duplicate id, for the reason every constructor in this package
// panics: a catalogue that cannot say which of two prices is the real one is a
// wiring defect, and the middle of a top-up is a strictly worse place to
// discover it.
func NewTopUpCatalogue(offers []TopUpOffer) TopUpCatalogue {
	byID := make(map[string]TopUpOffer, len(offers))
	order := make([]TopUpOffer, 0, len(offers))
	for _, offer := range offers {
		if _, seen := byID[offer.ID]; seen {
			panic("application: the top-up catalogue declares " + offer.ID + " twice")
		}
		byID[offer.ID] = offer
		order = append(order, offer)
	}
	return TopUpCatalogue{byID: byID, order: order}
}

// Offer returns the offer with id, or false. It is the ONLY way an amount
// enters this use case, and it is deliberately a lookup rather than a parse:
// a client names a thing that exists, and a thing that does not exist has no
// price.
func (c TopUpCatalogue) Offer(id string) (TopUpOffer, bool) {
	offer, ok := c.byID[id]
	return offer, ok
}

// List returns the offers in the order the deployment declared them, as a copy.
//
// The copy is not defensive noise: the slice is the catalogue's own, and a
// caller that sorted or truncated what it was handed in place would reorder
// every later reader of the same catalogue. A handler that wants a different
// order sorts its copy.
func (c TopUpCatalogue) List() []TopUpOffer {
	out := make([]TopUpOffer, len(c.order))
	copy(out, c.order)
	return out
}

// BeginTransferRequest is one attempt to start funding an account.
//
// The account comes from the authenticated session and is never a request
// parameter: an account id in a body would be a way to fund somebody else, and
// the console has no legitimate use for such a field.
type BeginTransferRequest struct {
	// AccountID is the authenticated session's account, resolved by the
	// transport.
	AccountID string
	// OfferID names an offer in this deployment's catalogue.
	OfferID string
	// IdempotencyKey is the caller's stable key for this logical top-up. It is
	// required, and the reason is the opposite of the reason minting an API
	// key has none: a repeated mint should produce a second key, because the
	// caller's intent is "give me another one", while a repeated top-up must
	// produce ONE payment, because the caller's intent is "I want to be
	// funded once" and a network retry does not change what they meant.
	IdempotencyKey string
}

// BeginTransferResult is what the caller gets: the payment, and whether this
// call created it.
type BeginTransferResult struct {
	// Intent is the payment as it now stands. On a converged call it is the
	// payment the key already named, which may be in any later state — a
	// retry after a customer already paid returns a succeeded payment, and
	// the correct console behaviour is to render what it is told.
	Intent payments.Intent
	// Converged reports that this call found an existing payment rather than
	// creating one. It is returned rather than inferred from the status,
	// because a client that retried needs to know its retry was harmless,
	// and a status is not that statement.
	Converged bool
}

// BeginTransfer opens a funding top-up and obtains, from the provider, the
// destination the customer is to pay into.
//
// The order of the four steps is the design, and each step's position closes a
// specific failure:
//
//  1. The payment is recorded BEFORE the provider is called. The provider's
//     idempotency key is derived from the payment's own identifier, and a key
//     derived from something that does not exist until the provider answers
//     cannot make a retry the same request — so a lost response would mean a
//     second call with a new key and a customer handed a second destination
//     for one payment. Durability first is what turns "retry" into "the same
//     request".
//
//  2. The provider call happens OUTSIDE any transaction. No external call
//     belongs inside a database transaction: the provider is an HTTP service
//     with its own latency and its own outages, and a transaction held open
//     across it pins a connection and a row lock for as long as a third party
//     takes to answer. The port says the same thing from the other side
//     (opening a transfer cannot move money), and the two rules meet here.
//
//  3. The destination is recorded in a SECOND transaction, guarded by a
//     compare-and-swap. Two concurrent calls can both reach the provider —
//     that is what the idempotency key is for — but only one of them may
//     write the account the customer will be told to pay into.
//
//  4. Everything that reads the account's state happens before any of it,
//     because a closed account's payment is a payment this platform should
//     not be opening in the first place.
//
// The destination is recorded BEFORE the customer is ever shown it, and that is
// the property the whole path rests on: the value a later delivery will be
// resolved through is one this plane wrote down first, so a delivery naming it
// can only be a delivery about a destination this platform asked the provider
// for.
//
// A provider OUTAGE between steps 1 and 3 leaves the payment in `created`. It
// is not an orphan: the next call with the same idempotency key converges on
// it and retries the provider with the SAME idempotency key, which is exactly
// the retry the provider's own key semantics are designed to absorb. The one
// refusal that is not an outage leaves nothing to retry, and the arm below that
// answers it abandons the payment instead.
func (p *Payments) BeginTransfer(ctx context.Context, in BeginTransferRequest) (BeginTransferResult, error) {
	offer, ok := p.offers.Offer(in.OfferID)
	if !ok {
		return BeginTransferResult{}, InvalidRequest("that top-up offer is not one this deployment sells")
	}
	// The key is checked HERE, against the domain's own rule, and not left to
	// payments.New below to refuse. It is the only field of the request that
	// reaches New as caller-chosen bytes — the amount, the currency, the
	// exponent and the provider all come from the offer this deployment
	// configured, the bucket comes from the account's own row, and the identity
	// and timestamps are minted here — so it is the only input whose refusal is a
	// fact about the request rather than a bug in this process. Left to New it
	// would answer 400's opposite: a 500, telling an operator to investigate a
	// customer who was holding a key one character too long.
	//
	// The message is the domain's, produced by the domain's check, so the rule
	// and its wording have exactly one home on both sides of the boundary.
	if err := payments.CheckIdempotencyKey(in.IdempotencyKey); err != nil {
		return BeginTransferResult{}, InvalidRequest(err.Error())
	}
	if in.AccountID == "" {
		return BeginTransferResult{}, Unauthenticated("a top-up requires an account")
	}

	account, err := p.accounts.ByID(ctx, identity.AccountID(in.AccountID))
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return BeginTransferResult{}, Unauthenticated("that account does not exist")
		}
		return BeginTransferResult{}, fmt.Errorf("application: begin top-up: read account: %w", err)
	}
	if account.State != identity.AccountActive {
		// Suspended and closed alike. A suspended account is one whose
		// principals have stopped authenticating, and letting it fund itself
		// while frozen would mean money arriving for a customer who cannot
		// spend it. The refusal is a conflict rather than a validation error:
		// the request is well-formed and the server's state is what refuses
		// it.
		return BeginTransferResult{}, Conflict("that account may not fund itself while it is not active")
	}

	intent, converged, err := p.openIntent(ctx, in, offer)
	if err != nil {
		return BeginTransferResult{}, err
	}
	if converged && intent.ProviderTransferRef != "" {
		// The payment already has a destination. Returning it is the whole
		// point of the key: the customer's second click gets the account their
		// first click was given, rather than a second one they would have to
		// choose between.
		return BeginTransferResult{Intent: intent, Converged: true}, nil
	}

	// OUTSIDE any unit of work. See the header.
	destination, err := p.provider.OpenTransfer(ctx, paymentprovider.TransferRequest{
		IdempotencyKey:   transferIdempotencyKey(intent.ID),
		AmountMinorUnits: intent.AmountMinorUnits,
		Currency:         intent.Currency,
		ExpiresIn:        transferTTL,
	})
	if err != nil {
		// ORDER IDENTITY THE PROVIDER WILL NOT ISSUE TWICE is checked first,
		// and it is the one failure here whose answer is not a retry.
		//
		// The provider already holds a destination under this payment's own
		// identity, it did not return it to this process, and it documents no
		// way to read one back. What that leaves is a destination that exists
		// and that no customer was ever shown: nobody holds the account, so
		// nobody can pay into it, and the money it might receive is money
		// nobody is waiting for. The payment must not stay in `created` either,
		// because `created` is the state a retry converges on and a retry is
		// precisely what cannot work — the provider refuses this identity
		// identically and forever, so the row would go on implying that one
		// more attempt is all it needs.
		//
		// The attempt is therefore abandoned locally, BEFORE the caller is told
		// anything: the payment moves from `created` to `cancelled`, which is a
		// legal edge of the state machine and the honest one. The customer
		// cancelled nothing — this platform gave up on the attempt — but the
		// vocabulary has no better word for a payment that can never be paid,
		// and the console must not offer to resume it. The next attempt a
		// client makes is then a NEW payment under a NEW idempotency key, which
		// derives a new order identity at the provider; the key itself is not
		// touched, so a client that repeats the SAME call converges on this
		// cancelled payment and is told, truthfully, that this attempt is over.
		//
		// The write is a compare-and-swap out of `created` and a lost race is
		// IGNORED rather than reported. A concurrent attempt may have moved the
		// row already — to `cancelled` for this same reason, or onward to a
		// state this call knows nothing about — and losing the swap means the
		// row is not this caller's to decide any more. Treating that as a
		// failure would turn the ordinary outcome of two concurrent retries
		// into a 5xx, which is the mistake the persistence port's own comment
		// on its compare-and-swap members warns about.
		//
		// WHAT THIS WRITE IS NOT, because the sentence above is easy to
		// overstate: it is a decision about a row in this platform's own table
		// and nothing else. It does not cancel anything at the provider, does
		// not claim the destination there was released, and does not claim the
		// account is gone. Nothing in this process could reach the provider to
		// do any of that — the port has no member for it.
		if errors.Is(err, paymentprovider.ErrOrderCodeTaken) {
			abandonErr := p.store.WithinTx(ctx, func(txCtx context.Context) error {
				now, err := dbNow(txCtx, p.clock, "abandon payment")
				if err != nil {
					return err
				}
				_, _, err = p.intents.MoveStatus(txCtx, intent.ID,
					[]payments.Status{payments.StatusCreated}, payments.StatusCancelled, now)
				return err
			})
			if abandonErr != nil {
				return BeginTransferResult{}, fmt.Errorf("application: abandon payment %s after the provider refused its transfer identity: %w", intent.ID, abandonErr)
			}
			return BeginTransferResult{}, Conflict("this top-up's destination is already held by the provider and cannot be shown; start a new top-up to be given another")
		}

		// Every other failure leaves the payment in `created`, and the next
		// attempt with this key converges on it. No error state is written: an
		// intent that recorded "the provider was down" would be an intent that
		// remembered a fact about a third party rather than about itself.
		//
		// The two CLASSES are separated here and nowhere else, because this is
		// the only layer that can tell them apart: the port says whether the
		// provider failed to answer, and the transport's only job is to render
		// the difference. A provider that is down makes this a 503 that a
		// client retries later — the condition is not the request's and it is
		// expected to clear — while a provider that refused the request, or
		// answered something this build cannot read, is a defect on one side or
		// the other and stays an internal error. Answering both the same way
		// would either invite a retry storm against a permanent refusal or
		// report a passing outage as a fault of this platform.
		cause := fmt.Errorf("application: begin transfer for payment %s: %w", intent.ID, err)
		if errors.Is(err, paymentprovider.ErrProviderUnavailable) {
			return BeginTransferResult{}, UpstreamUnavailable(cause)
		}
		return BeginTransferResult{}, cause
	}

	var recorded payments.Intent
	err = p.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := dbNow(txCtx, p.clock, "record transfer")
		if err != nil {
			return err
		}
		applied, err := p.intents.RecordTransfer(txCtx, intent.ID,
			payments.TransferInstructions{
				TransferCode:  destination.TransferCode,
				BankName:      destination.BankName,
				AccountHolder: destination.AccountHolder,
				QRURL:         destination.QRURL,
			},
			[]payments.Status{payments.StatusCreated}, now)
		if err != nil {
			return fmt.Errorf("record transfer: %w", err)
		}
		after, err := p.intents.ByID(txCtx, intent.ID)
		if err != nil {
			return fmt.Errorf("re-read payment after recording its destination: %w", err)
		}
		recorded = after
		if !applied {
			// Another attempt won the swap and wrote its own destination. The
			// loser does NOT overwrite it: whichever account the customer was
			// last handed is the one that must stand, and choosing between two
			// accounts the provider issued for this payment is not a decision
			// this layer can make — money sent to either would be money for
			// this payment, and a customer shown two different account numbers
			// for one payment has no way to know which to pay. The converged
			// read above returns the winner's payment, and the provider's
			// idempotency key means the two calls were the same request at the
			// provider anyway.
			return nil
		}
		return nil
	})
	if err != nil {
		return BeginTransferResult{}, fmt.Errorf("application: record transfer for payment %s: %w", intent.ID, err)
	}
	return BeginTransferResult{Intent: recorded, Converged: converged}, nil
}

// openIntent creates the payment, or converges on the one the key already
// names.
//
// The account's funding bucket is resolved in the SAME unit of work as the
// intent's insert, and that is deliberate: the payment's ownership is fixed at
// creation and nothing may change it afterwards, so the bucket and the account
// have to be decided together by the platform. Doing it in a second transaction
// would leave a window where a payment exists naming a bucket this platform has
// not yet committed to opening.
//
// A duplicate key is convergence, not failure. The caller re-reads and gets the
// payment it already has — which is the entire purpose of the key, and turning
// it into an error would make the retry the key exists to make safe into the
// one thing it is not.
func (p *Payments) openIntent(ctx context.Context, in BeginTransferRequest, offer TopUpOffer) (payments.Intent, bool, error) {
	var intent payments.Intent
	var converged bool
	err := p.store.WithinTx(ctx, func(txCtx context.Context) error {
		if existing, err := p.intents.ByAccountAndIdempotencyKey(txCtx, in.AccountID, in.IdempotencyKey); err == nil {
			intent = existing
			converged = true
			return nil
		} else if !errors.Is(err, persistence.ErrNotFound) {
			return fmt.Errorf("look for a payment under that key: %w", err)
		}

		now, err := dbNow(txCtx, p.clock, "open payment")
		if err != nil {
			return err
		}
		bucket, err := p.funding.OpenAccountFunding(txCtx, commerce.AccountID(in.AccountID))
		if err != nil {
			return fmt.Errorf("open the account's funding bucket: %w", err)
		}
		id, err := payments.NewIntent(now)
		if err != nil {
			return err
		}
		created, err := payments.New(payments.NewPayment{
			AccountID:         in.AccountID,
			FundingBucketID:   string(bucket.ID),
			AmountMinorUnits:  offer.AmountMinorUnits,
			Currency:          offer.Currency,
			MinorUnitExponent: offer.MinorUnitExponent,
			Provider:          p.providerName(),
			IdempotencyKey:    in.IdempotencyKey,
			TransferTTL:       transferTTL,
			Now:               now,
			MintedID:          id,
			MintedAt:          now,
		})
		if err != nil {
			return fmt.Errorf("open payment: %w", err)
		}
		if err := p.intents.Create(txCtx, created); err != nil {
			if errors.Is(err, payments.ErrDuplicatePayment) {
				// The pre-read raced another attempt with the same key. The
				// re-read names what landed, exactly as the accounting
				// writer's own collision path does.
				onFile, err := p.intents.ByAccountAndIdempotencyKey(txCtx, in.AccountID, in.IdempotencyKey)
				if err != nil {
					return fmt.Errorf("re-read the payment under that key: %w", err)
				}
				intent = onFile
				converged = true
				return nil
			}
			return fmt.Errorf("create payment: %w", err)
		}
		intent = created
		return nil
	})
	if err != nil {
		return payments.Intent{}, false, fmt.Errorf("application: begin top-up for account %s: %w", in.AccountID, err)
	}
	return intent, converged, nil
}

// transferTTL is how long this platform waits for a customer to make a bank
// transfer before giving up on the payment locally.
//
// It is a product decision rather than a technical bound, and it is short on
// purpose: a stale destination in a customer's list is a thing they may try to
// pay twice.
//
// It is handed to the provider as the lifetime of the account it issues, and
// that is the whole reason it travels rather than living in the adapter's
// configuration: the deadline the console prints for a customer and the window
// the account stays payable are ONE fact, and two authorities for one fact
// drift. A console that told a customer thirty minutes while the account
// remained payable for a day would send them hurrying for nothing, and the
// reverse would refuse money at a moment nobody had mentioned.
//
// Note what it is NOT — it is not a deadline on the money. A payment that
// succeeds after this expires is still honoured, because expiry is this
// platform's patience and the provider is the only party that states whether a
// customer paid.
const transferTTL = 30 * time.Minute

// transferIdempotencyKey derives the provider's idempotency key for a transfer
// from the payment's identity.
//
// Derived and not minted, because the whole value of the key is that the SAME
// value appears on every attempt at the same logical transfer. A key minted per
// call would be a key that makes every retry a NEW request at the provider,
// which is a customer handed a second destination for one payment — and the
// failure is silent, because the provider would be behaving exactly as
// documented. It is also the identity the provider derives its own order code
// from, which is what makes a second attempt under this key a collision the
// provider refuses rather than a second destination it issues.
func transferIdempotencyKey(id payments.IntentID) string {
	return "transfer:" + string(id)
}

// EventOutcome is what became of one verified delivery.
//
// It is the value the webhook's response is built from, and it exists so that
// the three answers a provider can get are computed in ONE place. The response
// contract is unusual enough to be worth stating here rather than at the
// transport: applied, duplicate and quarantined are all 2xx, and the rule
// behind that is
//
//	2xx means "this event needs no further delivery from you".
//
// A duplicate needs no further delivery because its effect is already durable.
// A quarantine needs no further delivery because the provider would send the
// same bytes again and this build would refuse them again — retrying a
// permanent refusal forever is a retry storm, and the provider is not going to
// fix its own event by sending it a hundred times. A 4xx is reserved for the
// deliveries the endpoint refuses AS deliveries — a signature that did not
// verify, a body whose declared encoding or content type cannot be the signed
// message, a body past the read bound, and a timestamp outside the freshness
// tolerance — every one of which is permanent, because the provider's next
// attempt carries the same bytes and the same header. A 5xx is reserved for the
// case where nothing was durably recorded and the delivery is genuinely worth
// repeating; it is the only status that asks for the delivery again.
//
// The list is the handler's, not this type's, and it is stated here because
// this comment is where the three answers are argued. A 4xx is never a verdict
// on an event that WAS authenticated and readable: an unreadable one and one
// this build has no rule for are both 2xx, so that no delivery the provider can
// legitimately resend is asked for twice.
type EventOutcome struct {
	// Disposition is what happened: applied, duplicate or quarantined.
	Disposition payments.EventDisposition
	// Reason is why, for a quarantine.
	Reason payments.QuarantineReason
	// IntentID is the payment the delivery resolved to, when it resolved to
	// one.
	IntentID payments.IntentID
}

// ProviderDelivery is a verified delivery on its way to being applied: the
// normalised event, and the raw bytes it was verified over.
//
// The bytes travel beside the event rather than being re-read from a request
// because they are EVIDENCE. A delivery this build cannot apply is recorded
// with the authenticated bytes so an operator can answer "what did the provider
// actually send" without going to the provider's dashboard — and the bytes are
// the ones the signature covered, not a re-serialisation of the parsed event,
// because an operator comparing our record against the provider's own log needs
// them to be the same bytes.
type ProviderDelivery struct {
	Event   paymentprovider.ProviderEvent
	RawBody []byte
}

// ApplyProviderEvent applies one verified provider delivery.
//
// Everything below happens in a SINGLE unit of work, and the order inside it is
// load-bearing:
//
//  1. The delivery is recorded FIRST — before any move of the payment or the
//     money, which is the ordering this sentence is actually about. The insert
//     is the arbiter of the race between two concurrent deliveries of the same
//     event: whichever transaction inserts the dedup key first holds it, and
//     the loser is told so by that key rather than by a lock. Recording it
//     anywhere else would open a window — an insert that committed before the
//     credit swallows the credit's own redelivery; a credit that committed
//     before the insert leaves this plane unable to say which delivery produced
//     it. One transaction with the insert first has neither window, because a
//     crash rolls back both. "First" is relative to the EFFECT and not to the
//     READ: resolution below is a lookup, and the one path that refuses on it
//     deliberately claims no key at all (see step 2).
//
//  2. The payment is resolved from the STORED reference. Not found is an
//     ordinary answer, not a fault: a delivery can arrive for a payment
//     another deployment opened, or for one this platform created outside this
//     surface. It is quarantined — and NOT recorded as an event, so that the
//     redelivery every provider will send stays applicable. The branch's own
//     comment is where that trade is argued, and it is the one place in this
//     function where the dedup key is left unclaimed on purpose.
//
//  3. The claim is checked against the payment by the DOMAIN — the amount, the
//     currency, and whether the move is one the state machine has. Every
//     refusal here is a quarantine, with the reason carrying which check
//     failed.
//
//  4. The status CAS runs BEFORE the credit. It is the effect's arbiter: a
//     payment that has already reached succeeded does not get funded twice, and
//     losing the CAS means somebody else is doing this work rather than that
//     this call failed.
//
//  5. The credit is written LAST, with txCtx. TopUp opens its own unit of work
//     internally and JOINS this one, because the store resolves a transaction
//     from the context — which is why passing ctx here instead of txCtx would
//     commit the two halves separately and silently.
func (p *Payments) ApplyProviderEvent(ctx context.Context, delivery ProviderDelivery) (EventOutcome, error) {
	event := delivery.Event
	var outcome EventOutcome
	err := p.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := dbNow(txCtx, p.clock, "apply provider event")
		if err != nil {
			return err
		}

		// 1. The dedup insert, FIRST.
		//
		// The disposition it carries is PROVISIONAL and is settled once the
		// verdict is known, below. It has to be: the insert is the arbiter of
		// the race between two concurrent deliveries of one event id, so it
		// must happen before this plane knows what the payment will turn out to
		// be, and a delivery that claims its key and is then refused is a real
		// row with a real verdict that is neither applied nor duplicate. Every
		// path below that does not carry the claim out settles the row before
		// the transaction commits, so no reader is ever shown the provisional
		// value — see persistence.PaymentEvents.Settle.
		record := payments.ProviderEventRecord{
			EventID:            event.EventID,
			ProviderAccountKey: p.providerAccountKey(),
			Provider:           p.providerName(),
			Kind:               event.Kind,
			ProviderPaymentRef: claimedPaymentRef(event),
			AmountMinorUnits:   event.AmountMinorUnits,
			Currency:           event.Currency,
			Disposition:        payments.DispositionApplied,
			OccurredAt:         event.OccurredAt,
			RecordedAt:         now,
		}

		// 2. Resolve the payment this delivery names.
		intent, resolveErr := p.resolveDelivery(txCtx, event)
		if resolveErr != nil {
			if !errors.Is(resolveErr, payments.ErrUnknownPayment) {
				return resolveErr
			}
			// A delivery naming a payment this plane never opened is
			// QUARANTINED AND NOT RECORDED AS AN EVENT, and the one thing a
			// reader will expect here is the opposite, so the reasoning is
			// worth the paragraph.
			//
			// The obvious move is to insert `record` anyway — payment_events
			// holds the dedup key, and the row would make a redelivery a
			// duplicate rather than a second quarantine row. That is exactly
			// what makes it wrong: the dedup key is a CLAIM, and claiming it
			// here would permanently absorb every future delivery of this
			// event id. There is a real, if narrow, window in which a delivery
			// arrives for a payment this plane HAS opened but has not yet
			// written the provider's transfer reference for — BeginTransfer
			// records the intent before it calls the provider and writes the
			// destination in a second transaction after the provider answers —
			// and a delivery that landed in that gap would be refused as
			// unknown, burn the key, and leave the customer's real money
			// unapplied forever, with the provider's own retries arriving as
			// duplicates of a refusal.
			//
			// Leaving the key unclaimed keeps the redelivery APPLICABLE: when
			// the provider retries — and it will, for hours — the reference is
			// by then stored, the delivery resolves, and the money lands. The
			// cost is that two identical unapplied deliveries are two
			// quarantine rows, which is the trade payment_quarantine's own
			// header records as deliberate: an operator reading them learns
			// the provider tried twice, and a repeated row is a far better
			// failure than a stranded payment.
			//
			// The bytes are still transmitted, because a quarantine row is
			// where the evidence is kept and it is spent exactly once per
			// delivery rather than once per event id.
			outcome = EventOutcome{Disposition: payments.DispositionQuarantined, Reason: payments.ReasonUnknownPayment}
			return p.recordQuarantine(txCtx, now, event, delivery.RawBody, "", payments.ReasonUnknownPayment)
		}
		record.IntentID = intent.ID

		if err := p.events.Record(txCtx, record); err != nil {
			if errors.Is(err, payments.ErrDuplicateEvent) {
				outcome = EventOutcome{Disposition: payments.DispositionDuplicate, IntentID: intent.ID}
				return nil
			}
			return fmt.Errorf("record provider delivery: %w", err)
		}

		// 3. Check the claim against the payment, in the domain's words.
		applied, reason, err := p.applyClaim(txCtx, intent, event, now)
		if err != nil {
			return err
		}
		if !applied {
			outcome = EventOutcome{Disposition: payments.DispositionQuarantined, Reason: reason, IntentID: intent.ID}
			// The delivery claimed its dedup key before the verdict existed and
			// is now refused, so the row it wrote is neither applied nor a
			// duplicate. Its verdict is corrected here, in the same unit of
			// work, before anything can read it — and it is corrected rather
			// than left claiming an effect that did not happen, because the
			// ledger's own row is what an operator reconciles against and a
			// delivery that says `applied` beside a quarantine row that says
			// otherwise is two records disagreeing about one event.
			if err := p.events.Settle(txCtx, record.DeliveryKey(), payments.DispositionQuarantined); err != nil {
				return fmt.Errorf("settle provider delivery %s: %w", event.EventID, err)
			}
			return p.quarantineRecord(txCtx, now, event, delivery.RawBody, intent.ID, reason)
		}
		outcome = EventOutcome{Disposition: payments.DispositionApplied, IntentID: intent.ID}
		// THE APPLIED PATH SETTLES ITS ROW TOO. This is the last statement in the
		// function and the easy one to leave off, because `applied` is what the
		// caller reads from `outcome` and the row is not obviously the same
		// thing. They are: `outcome` is what this process returns to the
		// handler, and the row is what survives a restart. A delivery that
		// credits a customer and leaves its row saying `recorded` has recorded
		// a state the schema's own comment says no reader may ever see — and the
		// row is the evidence an operator reconciles a payment against, so the
		// one delivery class that matters most is the one that would be unreadable.
		//
		// The duplicate path above is the exception, and it is not an oversight:
		// there the row was written by an EARLIER delivery, which settled it, and
		// a second settle would be the second UPDATE the append-only guard
		// refuses — turning an ordinary redelivery into a 500 and a retry storm
		// against a payment that has already been credited.
		if err := p.events.Settle(txCtx, record.DeliveryKey(), payments.DispositionApplied); err != nil {
			return fmt.Errorf("settle provider delivery %s: %w", event.EventID, err)
		}
		return nil
	})
	if err != nil {
		return EventOutcome{}, err
	}
	return outcome, nil
}

// resolveDelivery finds the payment a delivery names, or refuses in domain
// words.
//
// THE TRANSFER REFERENCE IS TRIED FIRST, and the order is a decision rather
// than an accident of which field is written first. A capture delivery carries
// both references, and the transfer is the one this plane WROTE: the provider
// issued that destination for this one payment, and this platform recorded it
// before the customer was ever shown it. Resolving through it means a capture
// lands on the payment the console opened rather than on whatever row happens
// to hold that payment id — and it does so on the strength of a value that is
// evidence about where money went rather than a claim about what somebody
// meant: an account the provider issued can only receive money this platform
// asked for, while a transfer's free-text memo is the customer's own words,
// which a bank may rewrite, truncate or uppercase and which a customer may
// simply not type. The payment reference is the fallback, and it is the ONLY
// route a refund has: a refund delivery names the money and never the
// destination it arrived at.
//
// There is still deliberately no fallback to anything inside the payload. Both
// references are matched against columns this platform wrote; a delivery that
// names neither is a delivery about a payment this platform did not open, and
// the answer is to record it rather than to look harder for somebody to credit.
func (p *Payments) resolveDelivery(ctx context.Context, event paymentprovider.ProviderEvent) (payments.Intent, error) {
	if payments.ValidProviderReference(event.TransferRef) {
		return p.resolveBy(ctx, "transfer", event.TransferRef,
			func(ctx context.Context, ref string) (payments.Intent, error) {
				return p.intents.ByProviderTransferRef(ctx, p.providerName(), ref)
			})
	}
	if payments.ValidProviderReference(event.PaymentRef) {
		return p.resolveBy(ctx, "payment", event.PaymentRef,
			func(ctx context.Context, ref string) (payments.Intent, error) {
				return p.intents.ByProviderPaymentRef(ctx, p.providerName(), ref)
			})
	}
	return payments.Intent{}, fmt.Errorf("%w: a delivery that names neither a destination this platform was issued nor a payment it recorded", payments.ErrUnknownPayment)
}

// resolveBy runs one of the two lookups and translates its miss into the
// domain's own refusal.
//
// The two arms differ only in which column they match, and folding them
// together is what keeps the miss identical between them: a provider reference
// nothing stored resolves to is one answer whether it arrived in the transfer
// field or the payment field, and two hand-written copies of that translation
// would be two chances to report one of them as a fault instead.
func (p *Payments) resolveBy(ctx context.Context, what, ref string, lookup func(context.Context, string) (payments.Intent, error)) (payments.Intent, error) {
	intent, err := lookup(ctx, ref)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return payments.Intent{}, fmt.Errorf("%w: no payment of this deployment names the %s reference %s", payments.ErrUnknownPayment, what, ref)
		}
		return payments.Intent{}, fmt.Errorf("resolve provider %s reference: %w", what, err)
	}
	return intent, nil
}

// claimedPaymentRef is the payment reference a delivery is taken to name: the
// provider's payment id when the delivery states one, and the transfer's own
// identifier when it does not.
//
// The fallback is a real decision rather than a convenience, and it is
// documented here because it is the one place this feature accepts a weaker
// identifier than the port would prefer. Three things read this value: the
// event row's claimed reference, the payment's stored reference — which the
// schema refuses to let a `succeeded` row hold as NULL — and the funding leg's
// command key, which must be derived from something unique per paid payment and
// a transfer's identifier is that, because the provider issued it for this one
// payment and nobody else holds it. A provider that takes the customer's money
// without naming the payment it created has still taken it, and refusing the
// capture over the absent name would strand a real payment for an operator to
// fund by hand — a worse outcome than recording the economic event under the
// identifier the delivery did carry.
//
// What the fallback COSTS is stated rather than hidden: a refund naming the
// payment intent will not resolve against a row whose reference is a transfer
// id, and that delivery quarantines as an unknown payment — with the provider's
// own reference on the quarantine row, which is the case that column exists
// for.
func claimedPaymentRef(event paymentprovider.ProviderEvent) string {
	if event.PaymentRef != "" {
		return event.PaymentRef
	}
	return event.TransferRef
}

// applyClaim checks one delivery against the payment and, when it is a capture
// this platform should fund, writes the credit.
//
// It returns a reason rather than an error for every refusal, and the
// difference matters at the transport: a reason becomes a quarantine and a 2xx,
// while an error becomes a 5xx and a retry. Nothing a provider sent can be a
// refusal this platform should retry — the provider would send the same bytes.
//
// THE PARAMETER IS NAMED txCtx AND MUST STAY NAMED THAT. Every call below that
// writes money or moves the payment takes this context, and it is the
// TRANSACTION's — the one ApplyProviderEvent's unit of work put in the closure.
// Naming it `ctx` here would make the single highest-risk line in this feature
// read like an ordinary request context, and a later edit that swapped it for
// the caller's would commit the credit and the dedup record separately, with no
// error raised anywhere and the provider told 200.
func (p *Payments) applyClaim(txCtx context.Context, intent payments.Intent, event paymentprovider.ProviderEvent, now time.Time) (bool, payments.QuarantineReason, error) {
	switch event.Kind {
	case paymentprovider.KindCaptured:
		// The state of the payer's account and bucket are checked BEFORE the
		// domain match, because a capture for a closed account is not a
		// refusal of the claim — the claim is perfectly good — it is a
		// situation an operator has to resolve, and the money is real.
		if reason, err := p.fundingTargetUsable(txCtx, intent); err != nil || reason != "" {
			return false, reason, err
		}

		match, err := intent.MatchCapture(claimedPaymentRef(event), event.AmountMinorUnits, event.Currency)
		if err != nil {
			// A domain refusal here is a mismatch the operator must see: the
			// amount, the currency or the state disagreed, and this plane
			// refuses rather than guessing which of the two is right.
			return false, reasonForClaimRefusal(err, intent, event), nil
		}

		// 4. The status CAS is the effect's arbiter and runs BEFORE the
		// credit. Losing it means another delivery of this same capture is
		// already doing the work.
		_, moved, err := p.intents.RecordCapture(txCtx, intent.ID, claimedPaymentRef(event),
			[]payments.Status{payments.StatusAwaitingTransfer, payments.StatusRequiresAction,
				payments.StatusExpired, payments.StatusCancelled}, now)
		if err != nil {
			return false, "", fmt.Errorf("record capture for payment %s: %w", intent.ID, err)
		}
		if !moved {
			// The world moved between the match and the statement. The
			// delivery is recorded and this is reported as a duplicate: the
			// payment is not in a state a capture moves, which means somebody
			// else already applied this or the payment is finished.
			return false, payments.ReasonStateConflict, nil
		}

		// 5. The credit, with the TRANSACTION's context.
		if _, err := p.ledger.TopUp(txCtx, accounting.FundingBucketID(intent.FundingBucketID),
			intent.AmountMinorUnits, accounting.CommandKey(match.CommandKey)); err != nil {
			if errors.Is(err, accounting.ErrBucketClosed) {
				// The bucket closed between the check above and the write.
				// The delivery is recorded, the credit did not land, and an
				// operator has a real payment and nowhere to put it.
				return false, payments.ReasonBucketClosed, nil
			}
			return false, "", fmt.Errorf("credit payment %s: %w", intent.ID, err)
		}
		return true, "", nil

	case paymentprovider.KindRefunded:
		return p.applyRefund(txCtx, intent, event, now)

	default:
		// The provider sent something this build does not interpret. Recorded
		// and acknowledged rather than refused with an error: providers add
		// event types, and a build that 5xx'd on a new one would page someone
		// every time the provider shipped a feature.
		return false, payments.ReasonUnknownKind, nil
	}
}

// fundingTargetUsable reports whether the payment's ACCOUNT can still receive
// money, and names the reason when it cannot.
//
// It reads the account and deliberately NOT the bucket, and the asymmetry is
// the decision this comment exists for rather than an oversight. The two
// questions look alike and are not:
//
//   - Whose account this is, and whether that account may still hold money, is
//     this plane's own fact. It is read from this plane's own table, it does
//     not move under a lock anyone else holds, and a wrong answer here is a
//     wrong answer the domain can own.
//
//   - Whether the BUCKET can take the credit is not decidable above the
//     ledger. "Closed", and "cannot afford it by one minor unit", are settled
//     by B6's own guarded statement, inside the transaction, against the row's
//     live state — and a copy of that verdict taken here would be a check that
//     can be false by the time it matters. B6 reports it as
//     accounting.ErrBucketClosed from inside TopUp, and that is the answer this
//     path uses.
//
// A capture for a closed or suspended account is refused BEFORE the status
// compare-and-swap, which is why a payment in that situation is left where it
// was rather than moved to succeeded. A capture for a closed BUCKET is refused
// after the swap, by the ledger, and the payment stays `succeeded`: the money
// is real, the customer has already sent it, and a payment that says it happened
// with a quarantine naming why it could not land is the state an operator can
// act on. The refusal table asserts exactly that, under "a capture for a bucket
// that has been closed".
func (p *Payments) fundingTargetUsable(txCtx context.Context, intent payments.Intent) (payments.QuarantineReason, error) {
	account, err := p.accounts.ByID(txCtx, identity.AccountID(intent.AccountID))
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return payments.ReasonAccountClosed, nil
		}
		return "", fmt.Errorf("read the payment's account: %w", err)
	}
	if account.State != identity.AccountActive {
		return payments.ReasonAccountClosed, nil
	}
	return "", nil
}

// applyRefund recognises a refund and records the claim, booking no ledger
// entry.
//
// This is the decision the whole refund surface rests on, and it is worth
// stating plainly here where a reader will meet it: B15 does NOT take money out
// of a bucket when a provider says it refunded a customer. The reason is not
// caution — it is that the correction is not expressible in this ledger. A
// refund is a debit that leaves `settled`, and a customer who topped up 10000
// and spent 9000 before being refunded is owed 10000 while holding 1000. B6's
// algebra refuses any adjustment driving a balance below zero, in the code, in
// the persisted echo's WHERE clause, and in the schema's own projection CHECK,
// and all three refusals exist because a balance may not record debt the
// platform does not have. The alternative — minting a machine principal to
// satisfy an adjustment's operator field — is a decision this repository has
// already refused to make, because an audit trail that can be taught to lie is
// not one.
//
// So the refund is RECOGNISED and the claim is recorded: the total refunded,
// and how much of it the balance could not cover. Those are two real figures an
// operator resolves. The one thing this path does not do is invent a
// bookkeeping entry for money that has left the building.
func (p *Payments) applyRefund(txCtx context.Context, intent payments.Intent, event paymentprovider.ProviderEvent, now time.Time) (bool, payments.QuarantineReason, error) {
	if intent.ProviderPaymentRef == "" {
		return false, payments.ReasonRefundAheadOfCapture, nil
	}

	// THE PROVIDER REPORTS A CUMULATIVE TOTAL AND THE DOMAIN WANTS AN
	// INCREMENT, and the two figures travel to different places rather than one
	// being converted into the other. The delivery this build consumes is a
	// refund notification, whose amount is everything that has gone back on the
	// charge over its whole life, not what this delivery added. The domain
	// needs the increment to decide which status the payment has reached; the
	// PORT needs the total, because that is what it writes and what a concurrent
	// writer must be measured against.
	//
	// Passing the provider's total where the domain wants an increment would
	// count the first refund a second time on the second delivery (400 + 800 =
	// 1200 against an 800 capture), turning a legitimate second partial refund
	// into a ceiling breach no operator can resolve, because both figures in
	// front of them are correct.
	//
	// Passing a caller-computed increment where the port wants the total is the
	// mirror mistake, and a worse one: the increment is a difference against a
	// state this caller READ, two deliveries of two refunds are processed
	// independently, and both reading the same base and both adding over-counts
	// the projection into a state nothing can correct — every later report is
	// then below what the row holds. The port's own comment on RecordRefund is
	// where that argument is made in full.
	if event.AmountMinorUnits == nil {
		// The provider stated no amount. This is not a refund of zero and it is
		// not a ceiling: this platform cannot tell how much went back, and the
		// honest classification is a figure that disagrees with the one stored —
		// which is absent here rather than different.
		return false, payments.ReasonAmountMismatch, nil
	}

	// THE CURRENCY IS CHECKED BEFORE THE CONVERGENCE BRANCH BELOW, and putting
	// it here rather than inside the domain call is the whole of the fix. The
	// branch under it treats any total at or below what is already stored as a
	// re-notification of a state this platform already agrees with, and that is
	// a sound reading of a figure expressed in the payment's own unit. It is not
	// a sound reading of one expressed in another: a refund notification in a
	// different currency is a claim about a different sum of money, and letting
	// it converge would be answering "applied" for a delivery that says nothing
	// this payment can agree with. A refund reported in the wrong currency is
	// rare enough that it will not be recognised by eye, and the domain's
	// ceiling arithmetic is in no position to notice.
	//
	// THE COMPARISON FOLDS CASE through the domain's own CurrencyCode, for the
	// reason the capture path's comparator folds it: a provider that reports
	// "usd" against a payment denominated "USD" is not disagreeing about the
	// unit, and refusing that delivery would quarantine a real customer's real
	// refund over its spelling. Compared with `!=` raw, this check would be
	// stricter about CURRENCY and looser about case, which is the wrong way
	// round for a rule whose only job is to catch a sum in another unit.
	//
	// NO `!= ""` GUARD, and the absence is the fix rather than an oversight —
	// the same absence reasonForClaimRefusal's comment gives. The adapter's
	// answer to a `null`, non-string or over-long currency is the empty string,
	// which is the one value this payment's own currency can never hold. A
	// refund that states no currency is therefore a mismatch, and saying so is
	// the finding: a delivery whose figure cannot be tied to a unit has nothing
	// to converge against, and a guard would let it converge on a bare number.
	if payments.CurrencyCode(event.Currency) != payments.CurrencyCode(intent.Currency) {
		return false, payments.ReasonCurrencyMismatch, nil
	}

	// A NON-POSITIVE TOTAL IS REFUSED, and it is refused HERE — above the
	// convergence branch and above the domain's own `requested <= 0` guard, both
	// of which sit below a payment that already holds 0.
	//
	// The convergence branch answers "applied" to any total at or below what is
	// stored, on the reasoning that a cumulative figure only goes up and so a
	// lower one is an older report. That reasoning assumes the figure is a
	// plausible one. Zero and negative are not: a payment that has never been
	// refunded holds 0, so a signed refund notification reporting a cumulative
	// total of -400 satisfies `-400 <= 0` and converges — a delivery that credits
	// nothing, books nothing and raises nothing, answered 200, with the
	// idempotency ledger recording it as applied and no quarantine row anywhere
	// for a human to find. The ledger would then say a refund happened while the
	// payment's own projection says none did, and the two are reconciled by an
	// operator who has no way to learn which is right.
	//
	// The negative figure is well-formed and parses: `readableAmount` accepts any
	// int64 the provider states, and refusing it here rather than in the adapter
	// keeps the refusal in domain words, where an operator reads it beside the
	// payment. A provider that reports a negative cumulative refund is reporting
	// something this build cannot interpret, and `amount_mismatch` says exactly
	// that: the figure disagrees with the one stored, and it is not any figure.
	if *event.AmountMinorUnits <= 0 {
		return false, payments.ReasonAmountMismatch, nil
	}
	reported := *event.AmountMinorUnits

	// A delivery reporting a total this platform has ALREADY recognised — the
	// same figure, or a smaller one — is not a second refund and not a fault.
	// Three ordinary situations produce one, and each is why this converges
	// rather than quarantines:
	//
	//   - A re-notification of the same state under a fresh event id. The dedup
	//     ledger keys on the id alone, and a provider may send one state twice
	//     under two ids.
	//   - An OLDER report arriving after a newer one. Delivery order is not
	//     guaranteed and providers retry for days, so the delivery of a 400
	//     refund can land after the delivery of the 800 total that includes it.
	//   - A retry of a delivery whose first attempt was answered 5xx after the
	//     credit had in fact been recorded.
	//
	// The claim all three make is satisfied by what is already stored, and the
	// answer is "applied" because the platform's record already agrees with the
	// provider's. Quarantining them would fill the operator queue with
	// reconciliation requests that are already correct.
	//
	// The premise that makes a SMALLER figure safe to read this way is that the
	// provider's cumulative refunded total is MONOTONE — it goes up and never
	// down — so a lower number is always an older report. A provider that
	// reverses a refund does not lower this figure: it sends a different event
	// kind, which this build quarantines as one it does not recognise, with the
	// bytes intact.
	//
	// Nothing is lost by converging: the delivery's own figure is durable on its
	// payment_events row, which is written before this function is reached.
	if reported <= intent.RefundedMinorUnits {
		return true, "", nil
	}
	increment := reported - intent.RefundedMinorUnits

	// The reference is the PAYMENT's, and it is the same value the delivery
	// resolved by: a charge-level refund names no individual refund, so the
	// finest identity it carries is the payment. The command key derived from it
	// also carries the resulting total, which is what keeps two partial refunds
	// from sharing one key — see payments.RefundCommandKey.
	record, err := intent.RecordRefund(claimedPaymentRef(event), &increment, event.Currency)
	if err != nil {
		if errors.Is(err, payments.ErrRefundAheadOfCapture) {
			return false, payments.ReasonRefundAheadOfCapture, nil
		}
		if errors.Is(err, payments.ErrInvalidTransition) {
			return false, payments.ReasonStateConflict, nil
		}
		if errors.Is(err, payments.ErrRefundCeiling) {
			return false, payments.ReasonRefundCeiling, nil
		}
		if errors.Is(err, payments.ErrInvalidReference) {
			// A figure this plane cannot credit, named rather than folded into
			// the ceiling above. The ceiling has its own sentinel precisely so
			// that this arm is reachable: the amount and the currency were
			// both checked above, and the domain refuses nothing else about a
			// refund it has agreed to recognise, so what is left is a payment
			// this build cannot refund — and an operator told "refund ceiling"
			// for that is sent to reconcile two figures that agree perfectly.
			return false, payments.ReasonStateConflict, nil
		}
		// The last refusal, and it is deliberately mapped to the ceiling rather
		// than raised as an error. Two reasons, and the second is the one that
		// decides it.
		//
		// First, an error here reaches the provider as a 5xx and the provider
		// retries the same bytes forever. Every other refusal in this chain is
		// a reason precisely so that it cannot.
		//
		// Second — and this is why the chain does not grow an arm for a case
		// the domain has not produced — an exhaustive arm is a PROMISE that
		// the code is driven by the enum, and this chain is not. The domain is
		// free to add a refusal tomorrow, and an arm written for each would go
		// stale silently, with a new refusal falling through to whatever sits
		// last here. That is the failure this shape is chosen against: the
		// fallback IS the contract, and it is the most conservative reason this
		// function can name. When a future refusal deserves a better answer
		// than the ceiling, the change to make is a sentinel for it in the
		// domain — as ErrRefundCeiling is — rather than a guess about which
		// enum value fits.
		return false, payments.ReasonRefundCeiling, nil
	}

	// The uncovered part is measured against the TOTAL the provider reported,
	// and not against this delivery's increment, because the balance it is
	// measured against is the payment's rather than the delivery's: asking
	// "how much of this increment exceeds the balance" would let every delivery
	// spend the same balance again, and the sum of those answers is not the
	// payment's shortfall. Asking "how much of everything refunded so far
	// exceeds the balance" is a question about the payment, and it has one
	// answer rather than one per delivery.
	//
	// It is a RECORD rather than a debit — see the function's comment.
	uncovered, err := p.uncoveredRefund(txCtx, intent, record.TotalRefunded)
	if err != nil {
		return false, "", err
	}

	// THE ABSOLUTE TOTAL CROSSES, not the increment. record.TotalRefunded is the
	// figure the provider reported, and the statement writes it whole.
	_, moved, err := p.intents.RecordRefund(txCtx, intent.ID, record.RefundRef,
		record.TotalRefunded, uncovered,
		[]payments.Status{payments.StatusSucceeded, payments.StatusPartiallyRefunded},
		record.Status, now)
	if err != nil {
		return false, "", fmt.Errorf("record refund for payment %s: %w", intent.ID, err)
	}
	if moved {
		return true, "", nil
	}

	// The statement fired zero rows, which has two causes and the caller must
	// tell them apart rather than guess: the guard `refunded_minor_units <
	// reported` was already satisfied (somebody else wrote this figure, or a
	// larger one — the claim is met, answer applied), or the row is not in one
	// of the `from` statuses (a genuine disagreement). What decides is the
	// figure the row now holds, so this re-reads it rather than assuming.
	//
	// The read is in this unit of work and after the failed update, so it sees
	// the concurrent writer's row whether it has committed or is still issued.
	//
	// IT TAKES NO ROW LOCK, and the reason matters enough to state rather than
	// assume: an UPDATE that matched zero rows locks zero rows. There is no
	// second concurrent writer to race here, because a writer that could match
	// this row would have taken the row lock and this transaction would have
	// blocked on it rather than observed zero rows. The zero-row result is
	// therefore itself the evidence that no other writer is in the row, and the
	// re-read answers about a row that is now exclusively this transaction's to
	// read. A claim that a lock was held here would be a claim about a guarantee
	// the database does not give, resting on a mechanism that was never used.
	stored, err := p.intents.ByID(txCtx, intent.ID)
	if err != nil {
		return false, "", fmt.Errorf("re-read payment %s after a refund that moved nothing: %w", intent.ID, err)
	}
	if stored.RefundedMinorUnits >= reported {
		return true, "", nil
	}
	return false, payments.ReasonStateConflict, nil
}

// uncoveredRefund reports how much of what has gone back to the customer the
// bucket's balance cannot account for.
//
// refunded is the CUMULATIVE total the provider reported — everything that has
// gone back over the payment's whole life — and the answer is the part of that
// figure which exceeds what the bucket holds. Passing this delivery's increment
// instead would be the natural-looking mistake and the wrong question: the
// balance is a property of the payment rather than of one delivery, so every
// delivery measuring its own increment against the same balance spends that
// balance once more, and the stored total — which is what the operator resolves
// the case with — drifts below the truth with each refund.
//
// It reads the bucket's SETTLED balance and answers the shortfall, and the word
// is the whole of one fix. `Available` is B6's settled-minus-held: a figure that
// is temporarily lower because a live reservation sits against it, not because
// the money is gone. Measuring the shortfall against it counts a reservation as
// a spend — a 1000 top-up with 900 of live usage against it and a full 1000
// refund would record `uncovered = 900` when nothing is missing at all, and the
// recorded figure is what the operator resolves the case with.
//
// `Settled` is the money that was actually added and has not been taken back.
// A refund that is covered by settled funds is a refund this bucket absorbs; a
// refund beyond them is a shortfall someone has to look at. Held funds are
// neither, and a reservation is not a loss.
//
// The figure is recorded for the operator who has to resolve the case; it is NOT
// booked, for the reason applyRefund states at length.
func (p *Payments) uncoveredRefund(txCtx context.Context, intent payments.Intent, refunded int64) (int64, error) {
	bucket, err := p.buckets.Bucket(txCtx, accounting.FundingBucketID(intent.FundingBucketID))
	if err != nil {
		return 0, fmt.Errorf("read the payment's funding bucket: %w", err)
	}
	settled := int64(bucket.Settled)
	if refunded <= settled {
		return 0, nil
	}
	return refunded - settled, nil
}

// quarantineRecord writes a quarantine row with the reason's own evidence
// rules applied.
//
// The rule worth stating: the PAYLOAD is recorded only when the delivery
// VERIFIED. An unverified body is an attacker's free text, and recording it
// verbatim would make this table a place where anyone on the internet can
// write. A verified-but-uninterpretable body is our provider's own words and is
// kept, because that is the evidence an operator resolves the row with.
func (p *Payments) quarantineRecord(ctx context.Context, now time.Time, event paymentprovider.ProviderEvent, payload []byte, intentID payments.IntentID, reason payments.QuarantineReason) error {
	record := payments.QuarantineRecord{
		Provider:           p.providerName(),
		ProviderAccountKey: p.providerAccountKey(),
		EventID:            event.EventID,
		IntentID:           intentID,
		Kind:               event.Kind,
		ProviderPaymentRef: claimedPaymentRef(event),
		AmountMinorUnits:   event.AmountMinorUnits,
		Currency:           event.Currency,
		Reason:             reason,
		Payload:            payload,
		OccurredAt:         event.OccurredAt,
		RecordedAt:         now,
	}
	if err := p.quarantine.Record(ctx, record); err != nil {
		return fmt.Errorf("record quarantined delivery: %w", err)
	}
	return nil
}

// recordQuarantine is quarantineRecord with an explicitly empty payment, for
// the two cases that resolve to no payment at all.
func (p *Payments) recordQuarantine(ctx context.Context, now time.Time, event paymentprovider.ProviderEvent, payload []byte, _ string, reason payments.QuarantineReason) error {
	return p.quarantineRecord(ctx, now, event, payload, "", reason)
}

// reasonForClaimRefusal maps a domain refusal onto the reason an operator
// reads.
//
// The mapping is by the SIGN of the disagreement rather than by the error's
// spelling, because the domain's sentinels answer "is this a refusal" and the
// operator's question is "which of the two figures disagrees". A currency
// mismatch is separated from an amount mismatch before anything else, since an
// amount is not a figure until the unit it counts in is known.
//
// The currency comparison has NO `!= ""` guard, and the absence of one is the
// fix rather than an oversight. The adapter's answer to a `null`, non-string or
// over-long currency is the empty string — the same sentinel it uses for a
// field it could not read — so a guard would route exactly the deliveries that
// are MOST about the currency to the reason for a different fault. A paid
// transfer whose `currency` is `null` arrives with an amount that matches to the
// unit and is filed as `amount_mismatch`, beside a quarantine row whose own
// currency column is NULL: the operator is told the sum disagrees when the sum
// is not what is wrong. Every payment this plane opens has a non-empty
// currency, so the empty string is never a match and the fold is safe.
func reasonForClaimRefusal(err error, intent payments.Intent, event paymentprovider.ProviderEvent) payments.QuarantineReason {
	if payments.CurrencyCode(intent.Currency) != payments.CurrencyCode(event.Currency) {
		return payments.ReasonCurrencyMismatch
	}
	if errors.Is(err, payments.ErrInvalidTransition) {
		return payments.ReasonStateConflict
	}
	return payments.ReasonAmountMismatch
}

// Offers returns what this deployment sells, in declaration order.
//
// It is not account-scoped, not paged and not a read of anything this plane
// recorded: it is configuration, published so the console can render a chooser
// without hardcoding a price list of its own. That is the whole reason an
// operation exists for it — the alternative is a console whose prices are a
// second authority, and two authorities disagree silently until a customer is
// charged a figure that is not on the screen they clicked.
//
// It requires no account argument for the same reason and one more: a caller
// with no account may still be told what the deployment sells. Nothing here is
// a customer's, so there is nothing to authorize.
func (p *Payments) Offers() []TopUpOffer {
	return p.offers.List()
}

// collectionPayments is this list's own name, the one the cursor binds a
// position to. It is a constant of this package rather than a string written at
// the two call sites below, for the reason every other collection's is: the
// name travels inside the signed cursor, and a list that spelled it differently
// in two places would mint cursors it refuses to read.
const collectionPayments = "console.payments"

// PaymentPage is one page of an account's payments, as the console carries it:
// the rows, whether more exist, and where to continue. There is deliberately no
// total, for the reason every other page on this surface has none.
type PaymentPage struct {
	Items      []payments.Intent
	HasMore    bool
	NextCursor string
}

// ListPayments returns an account's own payments, newest first.
//
// The account predicate is in the query rather than applied to rows already
// fetched, so another account's payment is a row the statement never returned —
// the same answer, at the same cost, a genuinely absent row gives.
//
// The three paging refusals are the surface's own and are NOT re-decided here:
// ResolvePage owns the limit bounds and the cursor's filter binding, and this
// list supplies only its own name and the absence of a filter. A payments list
// that resolved its limits differently from the nine beside it would be a list
// whose page size means something else, which is the class of difference a
// client cannot see.
//
// The extra row the repository asks for is dropped HERE and nowhere else, by
// PageOf — which is also the only thing that answers `has_more`. Returning the
// repository's slice unchanged, as this used to, hands the caller one row more
// than it asked for and reports `has_more: false` on a page that was full: two
// contract violations from one missing trim, and neither is visible in a test
// that only ever pages an account with one payment.
func (p *Payments) ListPayments(ctx context.Context, accountID, cursor string, limit int) (PaymentPage, error) {
	if accountID == "" {
		return PaymentPage{}, Unauthenticated("a payments list requires an account")
	}
	filters := Fingerprint(collectionPayments, nil)
	after, pageSize, err := ResolvePage(collectionPayments, cursor, limit, filters)
	if err != nil {
		return PaymentPage{}, err
	}
	intents, err := p.intents.ListForAccount(ctx, accountID, persistence.PaymentPage{
		After: payments.IntentID(after),
		Limit: pageSize,
	})
	if err != nil {
		return PaymentPage{}, fmt.Errorf("application: list payments for account %s: %w", accountID, err)
	}
	items, hasMore, next := PageOf(intents, pageSize,
		func(intent payments.Intent) string { return string(intent.ID) }, collectionPayments, filters, after)
	return PaymentPage{Items: items, HasMore: hasMore, NextCursor: next}, nil
}

// TransferInstructionsFor returns the destination a payment's customer was
// given, if the payment has one.
//
// It exists so the transport never tells a customer where to send money from
// anything but a stored row. The four values are returned VERBATIM: they are the
// provider's own account, its own name for the bank, its own name for the
// holder and its own image, all of them a promise the provider made and this
// platform is keeping on the provider's behalf — none is parsed, host-checked
// or composed here, and none is assembled from a request parameter.
//
// A payment in `created` has no destination, and the zero value is the true
// answer for it rather than an error: the console renders no instructions,
// which is exactly what there are. The reason is worth stating, because it is
// the reason the destination is written down at all: the customer must be shown
// the account the provider issued for this one payment, and a payment that has
// not reached the provider yet has none to show.
func (p *Payments) TransferInstructionsFor(ctx context.Context, accountID string, id payments.IntentID) (payments.TransferInstructions, error) {
	intent, err := p.intents.ByID(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return payments.TransferInstructions{}, NotFound("that payment does not exist")
		}
		return payments.TransferInstructions{}, fmt.Errorf("application: read payment %s: %w", id, err)
	}
	if intent.AccountID != accountID {
		// The caller asked about a payment that is not its own. Answered as a
		// miss rather than as a refusal, for the reason the console's reads
		// carry no forbidden sentinel: a distinct answer for "not yours"
		// turns the difference into a confirmation oracle.
		return payments.TransferInstructions{}, NotFound("that payment does not exist")
	}
	return payments.TransferInstructions{
		TransferCode:  intent.ProviderTransferRef,
		BankName:      intent.ProviderBankName,
		AccountHolder: intent.ProviderAccountHolder,
		QRURL:         intent.ProviderQRURL,
	}, nil
}
