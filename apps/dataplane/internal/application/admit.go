package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The admission use case: verify a credential, then decide one chat completion
// request — admitted, refused for a named reason, already in flight, in
// conflict with its key's history, or answered again from a decision already
// made. This is the runtime's authority over its own intake: every decision
// below is reached and recorded on this plane, in this process, with the
// Control Plane switched off (ADR 0006), and every decision but the internal
// failure is written down before it is answered.
//
// The flow is the one request-lifecycle.md pins, in its order:
//
//   - the credential is verified before anything else, and a verification
//     refusal writes nothing — a request that was never admitted never became
//     a row;
//   - the account gate refuses a suspended or closed account with a rejected
//     request row and nothing else — no body was read, so there is nothing to
//     digest a replay record against;
//   - the idempotency key's grammar and the body's arrival are refused the
//     same way, for the same reason: an invalid key cannot key a replay
//     record, and a body that never arrived has no digest;
//   - the replay record is probed before any unit of work opens: in flight,
//     decided, or in conflict are answered from the record alone;
//   - one unit of work admits what is left — alias, bounds, price, hold,
//     drawdown, request row, reservation — and writes the replay record LAST,
//     so its unique key is the final guard behind every check above;
//   - a refusal decided inside that unit writes the rejection pair (the
//     rejected request row and the replay record born terminal) and commits,
//     because a refusal is a decision, and a decision is on the record;
//   - the one exception is a missing price: an unpriced alias is an operator's
//     unfinished configuration, not a client's refusal, and it writes nothing.
//
// What happens after admission is the seam at the bottom of this file.

// admissionMaxAttempts bounds the admission unit's retries. Three is not a
// tuned number: the unit retries only on the engine's own contention classes
// (below), a retry re-runs a unit that never committed, and the honest
// terminations are "committed" and, under pathological contention, this
// giving up. The replay record's unique-race path shares the budget, because
// it is the same shape — a unit that lost its one uncontended claim and may
// win on a second run.
const admissionMaxAttempts = 3

const (
	// seamMaxAttempts bounds the compensation unit's retries, and
	// seamRetryBackoff is the pause between them. The seam runs after the
	// caller's answer has effectively been decided, so its budget is measured
	// against the request's own lifetime — three quick attempts, not a
	// backoff ladder a client times out under.
	seamMaxAttempts  = 3
	seamRetryBackoff = 10 * time.Millisecond
)

// errIntakeRaced is the interior signal that the replay record's insert lost
// its unique key: another unit wrote this (account, key) between the probe
// and the insert. It never reaches a caller — Serve answers it by re-reading
// the record OUTSIDE the aborted unit and answering from what actually
// committed, which is the whole point of the unique key being the final
// guard.
var errIntakeRaced = errors.New("application: the replay record was written concurrently")

// AdmissionConfig is what the composition root tells the admission use case
// about time and identity. The horizons arrive from the validated
// configuration; the lease owner is derived, never configured — a knob would
// let two processes claim one lease name, which is the confusion the lease
// exists to prevent.
type AdmissionConfig struct {
	// HoldWindow is how long an admitted request's reservation may live
	// before the reaper may reclaim it. It fences the reservation's
	// expires_at.
	HoldWindow time.Duration

	// LeaseTTL is how long this process's claim on the hold stays valid. It
	// bounds the reservation's lease deadline.
	LeaseTTL time.Duration

	// LeaseOwner names this process in every reservation it opens:
	// hostname:pid, truncated to the schema's 256-octet bound.
	LeaseOwner string
}

// ChatAdmission is the chat completion use case: credential verification and the
// admit-or-account-for-it decision behind one endpoint.
type ChatAdmission struct {
	store        persistence.Store
	credentials  persistence.Credentials
	aliases      persistence.ModelAliases
	prices       persistence.PriceBook
	requests     persistence.RequestRepository
	intakes      persistence.IntakeRepository
	reservations persistence.ReservationRepository
	ledger       persistence.QuotaProjectionRepository
	facts        persistence.FactRepository
	clock        txClock
	cfg          AdmissionConfig
}

// NewChatAdmission builds the admission use case over the ports it needs. It
// panics on a nil port or an unusable configuration for the reason its
// catalog sibling states: a port this use case was promised and did not get
// is a wiring defect, and the middle of an admission — after a transaction is
// open and a hold is drawn — is a strictly worse place to learn about it.
func NewChatAdmission(
	store persistence.Store,
	credentials persistence.Credentials,
	aliases persistence.ModelAliases,
	prices persistence.PriceBook,
	requests persistence.RequestRepository,
	intakes persistence.IntakeRepository,
	reservations persistence.ReservationRepository,
	ledger persistence.QuotaProjectionRepository,
	facts persistence.FactRepository,
	cfg AdmissionConfig,
) *ChatAdmission {
	switch {
	case store == nil:
		panic("application: NewChatAdmission requires a store")
	case credentials == nil:
		panic("application: NewChatAdmission requires a credentials repository")
	case aliases == nil:
		panic("application: NewChatAdmission requires an aliases repository")
	case prices == nil:
		panic("application: NewChatAdmission requires a price book")
	case requests == nil:
		panic("application: NewChatAdmission requires a request repository")
	case intakes == nil:
		panic("application: NewChatAdmission requires an intake repository")
	case reservations == nil:
		panic("application: NewChatAdmission requires a reservation repository")
	case ledger == nil:
		panic("application: NewChatAdmission requires a quota projection repository")
	case facts == nil:
		panic("application: NewChatAdmission requires a fact repository")
	case cfg.HoldWindow <= 0 || cfg.LeaseTTL <= 0:
		panic("application: NewChatAdmission requires a positive hold window and lease TTL")
	case cfg.LeaseOwner == "":
		panic("application: NewChatAdmission requires a lease owner")
	}
	return &ChatAdmission{
		store:        store,
		credentials:  credentials,
		aliases:      aliases,
		prices:       prices,
		requests:     requests,
		intakes:      intakes,
		reservations: reservations,
		ledger:       ledger,
		facts:        facts,
		clock:        storeClock{store: store},
		cfg:          cfg,
	}
}

// Serve admits one request, or accounts for why it was not admitted. The
// contract is ChatCompletion's: every decision arrives as an outcome with a
// nil error, and a non-nil error means no decision was reached at all.
func (a *ChatAdmission) Serve(ctx context.Context, in ChatInput) (ChatOutcome, error) {
	// Admission owns its units of work. A caller already inside one is a
	// wiring defect — the account gate and the admission unit below each open
	// their own, and joining a caller's would commit their work with ours.
	if a.store.InUnitOfWork(ctx) {
		return ChatOutcome{}, fmt.Errorf("application: admission refuses to run inside a unit of work it did not open")
	}

	// The account fact arrives with the verified credential, from the same
	// joined read that decided it — the mirror's one-statement guarantee means
	// admission judges exactly the lifecycles verification saw, never a
	// re-read that could straddle a projection apply. A fact that never
	// arrived is a wiring defect, and admission does not guess.
	if in.AccountState == nil {
		return ChatOutcome{}, fmt.Errorf("application: admit request %s: no account fact arrived with the verified key", in.RequestID)
	}
	switch *in.AccountState {
	case string(projection.LifecycleSuspended):
		return a.refuseEarly(ctx, in, execution.RejectedAccountSuspended, DetailNone)
	case string(projection.LifecycleClosed):
		return a.refuseEarly(ctx, in, execution.RejectedAccountClosed, DetailNone)
	}

	// The key's grammar, before the body: the refusal is the same rejected
	// row with the key's own field named, and a key outside the grammar can
	// never key a replay record — the record's identity is the key itself.
	if err := execution.ValidateIdempotencyKey(in.IdempotencyKey); err != nil {
		return a.refuseEarly(ctx, in, execution.RejectedInvalidRequest, DetailIdempotencyKey)
	}

	// The body, as the transport read it. A refused body is recorded and
	// refused: there is nothing to digest, so no replay record is written and
	// the request cannot be replayed — the caller fixes the transport problem
	// and sends a fresh request.
	var raw []byte
	switch body := in.Body.(type) {
	case BodyBytes:
		raw = body
	case BodyRefused:
		return a.refuseEarly(ctx, in, execution.RejectedInvalidRequest, DetailNone)
	default:
		return ChatOutcome{}, fmt.Errorf("application: admit request %s: unknown request body shape", in.RequestID)
	}
	digest := execution.SecretDigest(raw)

	// The probe, before any unit of work: a decision this account and key
	// already own is answered from its record and no unit opens at all. The
	// digest is the sameness test, so it is asked first — the same key over
	// different bytes is a conflict whatever the record's state, and the
	// original's decision must never be re-answered for bytes it never saw.
	outcome, decided, err := a.probe(ctx, in, digest)
	if err != nil || decided {
		return outcome, err
	}

	// The admission unit, with the one retry policy this use case runs: the
	// whole unit retries only on the engine's contention classes, and each
	// retry runs as a fresh unit under a fresh request identity — the first
	// attempt answers for the identity the transport minted, because nothing
	// that names it has committed yet.
	for attempt := 0; attempt < admissionMaxAttempts; attempt++ {
		requestID := in.RequestID
		if attempt > 0 {
			requestID = identity.NewRequestID()
		}
		outcome, err := a.admitOnce(ctx, in, requestID, raw, digest)
		if err == nil {
			return outcome, nil
		}
		if errors.Is(err, errIntakeRaced) {
			// The record's unique key did the guarding the probe could not:
			// a concurrent unit wrote this (account, key) after the probe
			// read. That unit's insert aborted ours — PostgreSQL aborts the
			// transaction carrying a unique violation — so the answer is
			// re-read OUTSIDE the aborted unit, from whatever actually
			// committed. Nothing found there means the winner aborted too,
			// and the unit runs again under a fresh identity.
			probed, decided, perr := a.probe(ctx, in, digest)
			if perr != nil {
				return ChatOutcome{}, perr
			}
			if decided {
				return probed, nil
			}
			continue
		}
		if isRetryableStoreFailure(err) {
			continue
		}
		return ChatOutcome{}, err
	}
	return ChatOutcome{}, fmt.Errorf("application: admit request %s: %w after %d attempts", in.RequestID, ErrTransitionContended, admissionMaxAttempts)
}

// refuseEarly records a refusal that needs no admission unit: the rejected
// request row, written in its own small unit and committed, with the fields
// known so far — which at the account gate and the key grammar is no alias
// and no snapshot at all. No replay record is written on this path: the
// account gate's refusal is about the account, not the key, and the grammar
// refusal's key cannot key a record.
func (a *ChatAdmission) refuseEarly(ctx context.Context, in ChatInput, reason execution.RejectionReason, detail RejectionDetail) (ChatOutcome, error) {
	err := a.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := a.clock.TransactionTimestamp(txCtx)
		if err != nil {
			return err
		}
		rejected, err := execution.RejectNew(in.RequestID, in.AccountID, in.Credential, "", reason, now)
		if err != nil {
			return err
		}
		return a.requests.Insert(txCtx, rejected)
	})
	if err != nil {
		return ChatOutcome{}, fmt.Errorf("application: admit request %s: record the %s refusal: %w", in.RequestID, reason, err)
	}
	return ChatOutcome{Kind: OutcomeRejected, Reason: reason, Detail: detail}, nil
}

// probe reads the replay record for one (account, key) and decides what the
// record already answers. The boolean is "the record answered": a false with
// a nil error means this request is the first arrival, and admission goes on.
// A record terminal in a way this build cannot answer is an error, not an
// answer: succeeded and failed originals are the execution pipeline's
// outcomes, and inventing an answer for them here would be vocabulary this
// milestone has no transport for.
func (a *ChatAdmission) probe(ctx context.Context, in ChatInput, digest string) (ChatOutcome, bool, error) {
	record, err := a.intakes.Find(ctx, in.AccountID, in.IdempotencyKey)
	if errors.Is(err, persistence.ErrNotFound) {
		return ChatOutcome{}, false, nil
	}
	if err != nil {
		return ChatOutcome{}, false, fmt.Errorf("application: admit request %s: read the replay record: %w", in.RequestID, err)
	}
	if record.RequestDigest != digest {
		return ChatOutcome{Kind: OutcomeConflict}, true, nil
	}
	if record.FinalStatus == nil {
		return ChatOutcome{Kind: OutcomeInFlight}, true, nil
	}
	if *record.FinalStatus == execution.FinalRejected {
		return ChatOutcome{
			Kind:     OutcomeReplay,
			Reason:   record.FinalRejectionReason,
			Original: record.RequestID,
		}, true, nil
	}
	return ChatOutcome{}, false, fmt.Errorf("application: admit request %s: the replay record is terminal in a way this build cannot answer", in.RequestID)
}

// admitOnce runs the admission unit once: one WithinTx, one clock read, and
// the order request-lifecycle.md pins — alias, bounds, price, hold, drawdown,
// request row, reservation, replay record last. A refusal decided inside the
// unit writes the rejection pair and commits with the outcome in hand; the
// price's absence is the one interior outcome that writes nothing, because it
// is nobody's refusal to record.
func (a *ChatAdmission) admitOnce(ctx context.Context, in ChatInput, requestID identity.RequestID, raw []byte, digest string) (ChatOutcome, error) {
	var (
		outcome  ChatOutcome
		legs     []accounting.Allocation
		admitted *Admission
	)
	err := a.store.WithinTx(ctx, func(txCtx context.Context) error {
		// ONE clock read. Everything this unit stamps — the request's
		// admission, the replay record's birth, the reservation's horizons —
		// and the price selection sharing the unit answer to the same
		// instant, the transaction's own, so a request's written memory
		// cannot disagree with the price it was drawn under.
		now, err := a.clock.TransactionTimestamp(txCtx)
		if err != nil {
			return err
		}

		// The body's shape. Malformed JSON and a missing model are refused
		// here; the ceiling's grammar is judged after the alias resolves,
		// because the alias refusal outranks it and one parse serves both.
		parsed, refusal := parseChatRequest(raw)
		if refusal != nil {
			return a.refuseInTx(txCtx, in, requestID, parsedModel(parsed), digest, now, refusal, &outcome)
		}
		model := parsed.model

		// The alias: a name no catalog row carries is refused, and a retired
		// name is not — retirement froze a row that still resolves, and B8
		// refuses only a name with nothing behind it.
		alias, err := a.aliases.ByName(txCtx, model)
		if err != nil {
			if errors.Is(err, persistence.ErrNotFound) {
				return a.refuseInTx(txCtx, in, requestID, model, digest, now,
					&chatRefusal{reason: execution.RejectedUnknownAlias}, &outcome)
			}
			return fmt.Errorf("application: admit request %s: read the alias: %w", requestID, err)
		}

		// The bounds, now that the alias they are bounded against is known.
		ceiling, inputTokens, refusal := parsed.bounds(alias)
		if refusal != nil {
			return a.refuseInTx(txCtx, in, requestID, model, digest, now, refusal, &outcome)
		}

		// The price. An unpriced alias is an operator's unfinished
		// configuration — the one interior outcome that writes nothing and
		// rolls back, because there is no client refusal to record and no
		// price of zero to invent.
		snapshot, err := a.prices.EffectiveAt(txCtx, alias.ID)
		if err != nil {
			return fmt.Errorf("application: admit request %s: read the price for alias %s: %w", requestID, alias.ID, err)
		}

		// The hold, from the snapshot the unit just read. An overflow is the
		// request's arithmetic refusing to be held — a client refusal, and it
		// is recorded as one before any capacity is touched.
		hold, err := accounting.Hold(int(inputTokens), ceiling, snapshot.InputUnitPrice, snapshot.OutputUnitPrice)
		if err != nil {
			return a.refuseInTx(txCtx, in, requestID, model, digest, now,
				&chatRefusal{reason: execution.RejectedInvalidRequest}, &outcome)
		}
		if hold > alias.ReservationCap {
			// The cap bounds a single request's hold, not the account: a
			// hold at the cap is admissible, one over it is not a capacity
			// question at all and never reaches the waterfall.
			return a.refuseInTx(txCtx, in, requestID, model, digest, now,
				&chatRefusal{reason: execution.RejectedInvalidRequest}, &outcome)
		}

		// The waterfall. The typed shortfall carries its own classification —
		// whether the walk saw any grant eligible to fund this alias — and
		// the two shapes are different refusals: no eligible grant is a scope
		// answer, an exhausted one is a capacity answer.
		legs, err = a.ledger.Drawdown(txCtx, in.AccountID, alias.ID, hold)
		if err != nil {
			var shortfall *persistence.InsufficientCapacityError
			if errors.As(err, &shortfall) {
				reason := execution.RejectedInsufficientEntitlement
				if !shortfall.EligibleRowSeen {
					reason = execution.RejectedNoAccess
				}
				return a.refuseInTx(txCtx, in, requestID, model, digest, now,
					&chatRefusal{reason: reason}, &outcome)
			}
			return fmt.Errorf("application: admit request %s: draw the hold: %w", requestID, err)
		}

		// The request row, born executing.
		request, err := execution.NewRequest(requestID, in.AccountID, in.Credential, model,
			int(inputTokens), ceiling,
			execution.PriceSnapshot{
				RevisionID:      snapshot.RevisionID,
				InputUnitPrice:  snapshot.InputUnitPrice,
				OutputUnitPrice: snapshot.OutputUnitPrice,
			}, now)
		if err != nil {
			return fmt.Errorf("application: admit request %s: form the request: %w", requestID, err)
		}
		if err := a.requests.Insert(txCtx, request); err != nil {
			return fmt.Errorf("application: admit request %s: insert the request: %w", requestID, err)
		}

		// The hold, formed with the legs the store actually granted — never
		// the amounts the caller hoped for — and asserted to re-derive its
		// own amount immediately before the insert: a hold whose split cannot
		// reproduce it is a leak, not a memory.
		reservationID := identity.NewReservationID()
		reservation, err := accounting.NewReservation(reservationID, requestID, snapshot.RevisionID,
			snapshot.InputUnitPrice, snapshot.OutputUnitPrice, int(inputTokens), ceiling, hold, legs,
			now, now.Add(a.cfg.HoldWindow), a.cfg.LeaseOwner, now.Add(a.cfg.LeaseTTL))
		if err != nil {
			return fmt.Errorf("application: admit request %s: form the hold: %w", requestID, err)
		}
		reDerived, err := reservation.ReDerivedHold()
		if err != nil || reDerived != hold {
			return fmt.Errorf("application: admit request %s: the hold cannot be re-derived from its legs", requestID)
		}
		if err := a.reservations.Insert(txCtx, reservation); err != nil {
			// A duplicate hold here is a bug, not an answer: the probe and
			// the record's unique key below both stand guard before a second
			// hold for one request can exist. It is failed loudly, and the
			// unit with it.
			return fmt.Errorf("application: admit request %s: insert the hold: %w", requestID, err)
		}

		// The replay record, LAST. Born with no terminal pointer, it is the
		// concurrent replay's rendezvous point, and its unique key is the
		// final guard behind every check this unit ran.
		record, err := execution.NewIntake(in.AccountID, in.IdempotencyKey, digest, requestID, now)
		if err != nil {
			return fmt.Errorf("application: admit request %s: form the replay record: %w", requestID, err)
		}
		if err := a.intakes.Insert(txCtx, record); err != nil {
			if errors.Is(err, persistence.ErrDuplicateIntake) {
				return errIntakeRaced
			}
			return fmt.Errorf("application: admit request %s: insert the replay record: %w", requestID, err)
		}

		admitted = &Admission{
			RawBody:          raw,
			Stream:           parsed.stream,
			Price:            snapshot,
			Hold:             hold,
			ReservationID:    reservationID,
			LeaseOwner:       a.cfg.LeaseOwner,
			LeaseExpiresAt:   now.Add(a.cfg.LeaseTTL),
			RuntimeRequestID: requestID,
		}
		return nil
	})
	if err != nil {
		return ChatOutcome{}, err
	}
	if outcome.Kind == OutcomeRejected {
		// A refusal decided inside the unit is a committed decision — the
		// pair is on the record, and the outcome answers for it.
		return outcome, nil
	}

	// The B8 seam: the stand-in for the routing stage. Nothing routes yet, so
	// the admission this unit just committed is completed by releasing the
	// hold and finishing the request as no_candidate — the one ending an
	// admitted request can reach before there is a candidate to serve it to.
	if err := a.complete(ctx, in, admitted, legs); err != nil {
		return ChatOutcome{}, err
	}
	return ChatOutcome{Kind: OutcomeRejected, Reason: execution.RejectedNoCandidate, Detail: DetailNone}, nil
}

// refuseInTx records the rejection pair inside the admission unit that
// decided it: the rejected request row with the fields known so far, and the
// replay record born terminal — final_status 'rejected' and the deciding
// reason, so every future arrival under this key is answered from the record
// without re-running any of the checks that refused this one. The unit
// COMMITS the pair; a refusal is a decision, and a decision is on the record.
func (a *ChatAdmission) refuseInTx(
	txCtx context.Context,
	in ChatInput,
	requestID identity.RequestID,
	model string,
	digest string,
	now time.Time,
	refusal *chatRefusal,
	outcome *ChatOutcome,
) error {
	rejected, err := execution.RejectNew(requestID, in.AccountID, in.Credential, model, refusal.reason, now)
	if err != nil {
		return err
	}
	if err := a.requests.Insert(txCtx, rejected); err != nil {
		return fmt.Errorf("application: admit request %s: record the rejection: %w", requestID, err)
	}
	record, err := execution.NewIntake(in.AccountID, in.IdempotencyKey, digest, requestID, now)
	if err != nil {
		return err
	}
	if err := record.Finalise(execution.FinalRejected, refusal.reason, ""); err != nil {
		return err
	}
	if err := a.intakes.Insert(txCtx, record); err != nil {
		return fmt.Errorf("application: admit request %s: record the replay refusal: %w", requestID, err)
	}
	*outcome = ChatOutcome{Kind: OutcomeRejected, Reason: refusal.reason, Detail: refusal.detail}
	return nil
}

// complete is the B8 seam, and it is deliberately shaped like the stage that
// will replace it: handed exactly what an admitted request hands forward, it
// finishes the request the only way this milestone can — the hold released,
// the request finalised no_candidate, the replay record terminal, the release
// fact the feed's last word. The next milestone's routing stage reads the
// same hand-off and either routes the request or calls this same ending for
// its own reasons.
//
// The unit is detached on purpose: the admission unit has committed, so the
// compensation stands on its own — its own retry budget, its own clock, its
// own commit. When the seam exhausts its retries the error is the answer: the
// hold stays stranded (open reservation, executing request, the reaper's to
// reclaim) and the caller is told nothing that invites a retry, because a
// retry would draw a second hold. The no_candidate answer exists only for a
// seam that committed — or lost its own CAS, in which case whoever won the
// hold owns its ending and this caller still has no candidate.
func (a *ChatAdmission) complete(ctx context.Context, in ChatInput, admitted *Admission, legs []accounting.Allocation) error {
	for attempt := 0; attempt < seamMaxAttempts; attempt++ {
		// The unit's own "completed" is not this loop's business: a seam that
		// lost its CAS settled too — someone else owns the ending — and an
		// error is the only thing left to retry.
		_, err := a.completeOnce(ctx, in, admitted, legs)
		if err == nil {
			return nil
		}
		if !isRetryableStoreFailure(err) {
			return err
		}
		if attempt+1 < seamMaxAttempts {
			select {
			case <-ctx.Done():
				return fmt.Errorf("application: complete request %s: %w", admitted.RuntimeRequestID, ctx.Err())
			case <-time.After(seamRetryBackoff << attempt):
			}
		}
	}
	return fmt.Errorf("application: complete request %s: the release unit did not settle after %d attempts", admitted.RuntimeRequestID, seamMaxAttempts)
}

// completeOnce runs the compensation unit once. Every step past the CAS is
// guarded by the one before it: the CAS decides who owns the hold's ending,
// and a lost CAS ends the unit with nothing written — the winner's ending
// stands, and this caller's answer is unchanged.
func (a *ChatAdmission) completeOnce(ctx context.Context, in ChatInput, admitted *Admission, legs []accounting.Allocation) (bool, error) {
	var completed bool
	err := a.store.WithinTx(ctx, func(txCtx context.Context) error {
		now, err := a.clock.TransactionTimestamp(txCtx)
		if err != nil {
			return err
		}
		// The CAS is the whole claim to the compensation: only the writer
		// that moves the hold out of open may return its legs and state the
		// ending.
		closed, err := a.reservations.Close(txCtx, admitted.ReservationID, accounting.StateReleased, now)
		if err != nil {
			return fmt.Errorf("application: complete request %s: release the hold: %w", admitted.RuntimeRequestID, err)
		}
		if !closed {
			return nil
		}
		// The return, in the legs' stored ordinal order — the waterfall order
		// every writer of the projection walks, and the order the fact
		// publishes. The port's return is unconditional on the balances, so a
		// publication that shrank a ceiling cannot turn a release into a
		// failure.
		if _, err := a.ledger.Return(txCtx, legs); err != nil {
			return fmt.Errorf("application: complete request %s: return the hold: %w", admitted.RuntimeRequestID, err)
		}
		request := execution.Request{ID: admitted.RuntimeRequestID, Status: execution.StatusExecuting}
		if err := request.Reject(execution.RejectedNoCandidate, now); err != nil {
			return err
		}
		finalised, err := a.requests.Finalise(txCtx, request)
		if err != nil {
			return fmt.Errorf("application: complete request %s: finalise the request: %w", admitted.RuntimeRequestID, err)
		}
		if !finalised {
			return nil
		}
		decided, err := a.intakes.Finalise(txCtx, in.AccountID, in.IdempotencyKey,
			execution.FinalRejected, execution.RejectedNoCandidate, "")
		if err != nil {
			return fmt.Errorf("application: complete request %s: finalise the replay record: %w", admitted.RuntimeRequestID, err)
		}
		if !decided {
			return nil
		}
		fact, err := accounting.NewReleased(admitted.RuntimeRequestID, factLegs(legs), now)
		if err != nil {
			return err
		}
		// The append is the unit's LAST statement: the feed's row lock is
		// held to the commit, so the order facts are allocated is the order
		// they become visible, and this ending reads as one fact.
		if _, err := a.facts.Append(txCtx, fact); err != nil {
			return fmt.Errorf("application: complete request %s: append the release: %w", admitted.RuntimeRequestID, err)
		}
		completed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return completed, nil
}

// ---------------------------------------------------------------------------
// the body, parsed once
// ---------------------------------------------------------------------------

// chatRequest is what admission reads out of one body. It carries the raw
// spellings, not verdicts: the alias refusal outranks the bounds refusals, so
// the shape is parsed first and judged after the alias resolves.
type chatRequest struct {
	model         string
	maxTokens     json.RawMessage
	maxCompletion json.RawMessage
	stream        bool
	inputTokens   int64
}

// chatRefusal is a body-level refusal already in admission's vocabulary: the
// reason the rejected row stores and the detail the wire names.
type chatRefusal struct {
	reason execution.RejectionReason
	detail RejectionDetail
}

// parseChatRequest reads the fields admission decides on. A body that will
// not parse and a body with no model are refused here — nothing downstream
// can decide without them; everything else is judged with the alias in hand.
func parseChatRequest(raw []byte) (*chatRequest, *chatRefusal) {
	var body struct {
		Model         json.RawMessage `json:"model"`
		MaxTokens     json.RawMessage `json:"max_tokens"`
		MaxCompletion json.RawMessage `json:"max_completion_tokens"`
		Stream        *bool           `json:"stream"`
		Messages      []struct {
			Content *string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, &chatRefusal{reason: execution.RejectedInvalidRequest}
	}
	var model string
	if err := json.Unmarshal(body.Model, &model); err != nil || model == "" {
		return nil, &chatRefusal{reason: execution.RejectedInvalidRequest, detail: DetailModel}
	}
	parsed := &chatRequest{
		model:         model,
		maxTokens:     body.MaxTokens,
		maxCompletion: body.MaxCompletion,
		stream:        body.Stream != nil && *body.Stream,
	}
	for _, message := range body.Messages {
		if message.Content != nil {
			parsed.inputTokens += int64(len(*message.Content))
		}
	}
	return parsed, nil
}

// parsedModel is the model a parsed body names, for the one refusal that can
// happen before a body parses at all (the refusal path writes the row with
// the fields known so far, and an unparsed body names no model).
func parsedModel(parsed *chatRequest) string {
	if parsed == nil {
		return ""
	}
	return parsed.model
}

// bounds judges the parsed body against the alias it names: the ceiling's
// grammar, the ceiling against the alias's own bound, and the input count.
// The refusal's detail names the spelling at fault — the two output-ceiling
// spellings are the client's fields, and the wire tells the client which one
// to fix. A spelling that was simply never sent is not at fault: the two
// exist so a caller can use either, and the ceiling is missing only when
// neither arrives.
func (p *chatRequest) bounds(alias *catalog.ModelAlias) (ceiling int, inputTokens int64, refusal *chatRefusal) {
	maxTokens := ceilingSpellingFrom(p.maxTokens)
	maxCompletion := ceilingSpellingFrom(p.maxCompletion)
	refuse := func(detail RejectionDetail) (int, int64, *chatRefusal) {
		return 0, 0, &chatRefusal{reason: execution.RejectedInvalidRequest, detail: detail}
	}
	switch {
	case !maxTokens.present && !maxCompletion.present:
		// No ceiling spelled at all — absent or null on both spellings. The
		// canonical spelling the contract documents is the one the wire names.
		return refuse(DetailMaxTokens)
	case maxTokens.fault && maxCompletion.fault:
		return refuse(DetailMaxTokens)
	case maxTokens.fault:
		return refuse(DetailMaxTokens)
	case maxCompletion.fault:
		return refuse(DetailMaxCompletionTokens)
	case maxTokens.present && maxCompletion.present && maxTokens.value != maxCompletion.value:
		// Both usable and disagreeing: the older spelling is the one the
		// client is told to drop.
		return refuse(DetailMaxTokens)
	}
	ceiling, ceilingDetail := maxTokens.value, DetailMaxTokens
	if maxCompletion.present {
		ceiling, ceilingDetail = maxCompletion.value, DetailMaxCompletionTokens
	}
	if ceiling > int(alias.MaxOutputTokens) {
		return refuse(ceilingDetail)
	}
	if p.inputTokens == 0 {
		// No content anywhere in the request: nothing this build could count,
		// so nothing it could price. The fault is the body's whole shape, and
		// no one field is named.
		return 0, 0, &chatRefusal{reason: execution.RejectedInvalidRequest}
	}
	return ceiling, p.inputTokens, nil
}

// ceilingSpelling is one spelling of the output ceiling, read but not judged:
// present says whether the field arrived at all (absent is a state, not a
// fault — the two spellings exist so a caller may use either), and fault says
// whether an ARRIVED spelling is unusable.
type ceilingSpelling struct {
	value   int
	present bool
	fault   bool
}

// ceilingSpellingFrom reads one spelling of the output ceiling. A spelling
// that arrived but is not a number, not an integer, not positive, or past
// MaxInt32 — the bound the integer columns the bound lives in impose — is at
// fault.
func ceilingSpellingFrom(raw json.RawMessage) ceilingSpelling {
	if len(raw) == 0 || string(raw) == "null" {
		return ceilingSpelling{}
	}
	// A JSON string that looks like a number is still a string: the standard
	// library happily unmarshals "16" into a json.Number, so the literal is
	// checked for its own first byte before the library is trusted with it.
	if raw[0] != '-' && (raw[0] < '0' || raw[0] > '9') {
		return ceilingSpelling{present: true, fault: true}
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return ceilingSpelling{present: true, fault: true}
	}
	text := string(number)
	if strings.ContainsAny(text, ".eE") {
		return ceilingSpelling{present: true, fault: true}
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 1 || value > math.MaxInt32 {
		return ceilingSpelling{present: true, fault: true}
	}
	return ceilingSpelling{value: int(value), present: true}
}

// ---------------------------------------------------------------------------
// the database clock
// ---------------------------------------------------------------------------

// txClock is the one query admission runs that is not a repository call: the
// database's own clock, read inside a unit of work so every stamp the unit
// writes answers to the transaction's instant. It is an interface for the
// tests and only for the tests; the production implementation is below.
type txClock interface {
	TransactionTimestamp(ctx context.Context) (time.Time, error)
}

// storeClock reads the clock through the store's own Querier, so the read
// resolves to the unit of work the context carries — inside an admission unit
// it is that unit's transaction-stable instant, the same one the price
// selection and the drawdown's eligibility predicate use.
//
// The indirection exists because persistence.Querier names database/sql's row
// type, and the arch rules keep database/sql out of this package — its test
// files included, where a fake Querier would have to name it. One method the
// fakes can implement is the seam; the query itself stays here, on the
// production path.
type storeClock struct {
	store persistence.Store
}

func (s storeClock) TransactionTimestamp(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := s.store.Querier(ctx).QueryRowContext(ctx, "SELECT transaction_timestamp()").Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("application: read the transaction clock: %w", err)
	}
	return now, nil
}

// ---------------------------------------------------------------------------
// retry classification
// ---------------------------------------------------------------------------

// sqlStateReporter is the structural question the retry policy asks of a
// store error: PostgreSQL reports an error's class in its SQLSTATE, and the
// driver's error type carries it. The application cannot name the driver —
// the arch rules keep it behind the postgres adapter — so it asks the
// question structurally, and any error whose type can answer is asked.
type sqlStateReporter interface {
	SQLState() string
}

// isRetryableStoreFailure reports whether a failed unit may be retried whole:
// the engine aborted it for contention (serialization failure, deadlock) or
// the connection died before any commit could be in question. A unit that
// failed for any other reason failed on its merits, and a retry would only
// repeat it. A caller's cancelled context is never retried — the caller is
// gone, and the budget belongs to the request.
func isRetryableStoreFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var reporter sqlStateReporter
	if !errors.As(err, &reporter) {
		return false
	}
	switch state := reporter.SQLState(); {
	case state == "40001" || state == "40P01":
		// serialization_failure, deadlock_detected: the engine aborted this
		// contender so another could commit; a fresh unit may win.
		return true
	case strings.HasPrefix(state, "08"):
		// connection exception: the unit never reached a commit, so nothing
		// is in question and a fresh unit on a new connection may run.
		return true
	}
	return false
}

// factLegs renders the hold's split as the fact's allocation tail. Allocation
// and AllocationLeg are field-for-field the same shape on purpose — the
// hold's memory and the fact's publication of it — so the conversion is a
// spelling, not a mapping.
func factLegs(legs []accounting.Allocation) []accounting.AllocationLeg {
	out := make([]accounting.AllocationLeg, 0, len(legs))
	for _, leg := range legs {
		out = append(out, accounting.AllocationLeg(leg))
	}
	return out
}

// ---------------------------------------------------------------------------
// credential verification
// ---------------------------------------------------------------------------

// CredentialAuthenticator is the Authenticator over the credential mirror:
// the presented bearer value is parsed, looked up, and compared, and the
// answer is the key identity behind it or one refusal.
type CredentialAuthenticator struct {
	credentials persistence.Credentials
}

// NewCredentialAuthenticator builds the authenticator over the mirror read.
// It panics on a nil port with the same reasoning as its siblings.
func NewCredentialAuthenticator(credentials persistence.Credentials) *CredentialAuthenticator {
	if credentials == nil {
		panic("application: NewCredentialAuthenticator requires a credentials repository")
	}
	return &CredentialAuthenticator{credentials: credentials}
}

// zeroDigest is the digested secret of the credential synthesized on a miss:
// 64 hex zeros, the shape a stored digest has and no real secret's digest
// does. It exists so the miss burns the same constant-time comparison a
// match would have.
const zeroDigest = "0000000000000000000000000000000000000000000000000000000000000000"

// Authenticate verifies one presented credential. Every refusal is the same
// typed answer with one of three server-side reasons, and the presented text
// crosses no error message and no log line — a credential that reaches an
// error message is a credential the log has leaked.
//
// The order is the leak-resistance order: parse, then the mirror row, then
// the digest, then the key's state, then the account's presence — each check
// runs only after the one before it passed, so a refusal never reveals which
// of the later checks would have failed to a caller that never presented the
// right secret.
func (a *CredentialAuthenticator) Authenticate(ctx context.Context, credential string) (AuthenticatedCredential, error) {
	keyID, secret, err := execution.ParseToken(credential)
	if err != nil {
		return AuthenticatedCredential{}, &Unauthenticated{Reason: ReasonCredentialUnknown}
	}
	view, err := a.credentials.Lookup(ctx, keyID)
	if err != nil {
		if errors.Is(err, persistence.ErrCredentialNotFound) {
			// The miss is answered with the work a match would have cost: the
			// synthesized zero credential is compared against the presented
			// secret's digest in constant time, so the shape of the burn —
			// not its result, which is decided before it starts — is what a
			// clock can observe.
			execution.EqualDigests(execution.SecretDigest(secret), zeroDigest)
			return AuthenticatedCredential{}, &Unauthenticated{Reason: ReasonCredentialUnknown}
		}
		// The mirror's own failure: an unavailable store is not an
		// authentication verdict, and it is answered as the internal failure
		// it is.
		return AuthenticatedCredential{}, fmt.Errorf("application: authenticate key %s: %w", keyID, err)
	}
	if !execution.EqualDigests(execution.SecretDigest(secret), view.Digest) {
		return AuthenticatedCredential{}, &Unauthenticated{Reason: ReasonCredentialUnknown}
	}
	if view.KeyState != string(projection.CredentialActive) {
		// Not a state that may admit requests — "revoked" today, and any
		// state a later projection grows fails closed here with it.
		// RevokedAt is informational: the refusal is the same whatever the
		// revocation's instant.
		return AuthenticatedCredential{}, &Unauthenticated{Reason: ReasonCredentialRevoked}
	}
	if view.AccountState == nil {
		// The credential row exists and its account row does not: a
		// mirror-integrity violation no conforming projection produces, and
		// refused closed rather than served through.
		return AuthenticatedCredential{}, &Unauthenticated{Reason: ReasonAccountAbsent}
	}
	return AuthenticatedCredential{
		KeyID:        keyID,
		AccountID:    view.AccountID,
		AccountState: view.AccountState,
	}, nil
}
