package ingestion

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
)

// The refusal vocabulary. Each sentinel names one way a fact can fail to be
// applicable, and Quarantinable splits them by disposition: the first six are
// recorded verbatim in quarantined_facts while the page advances past them,
// and everything else — ErrPayloadOversize, and any error not from this
// package at all — stops the page where it stands.
var (
	// ErrUnknownKind marks a kind outside the feed's vocabulary. The fact is
	// recorded verbatim: the Data Plane has written something this build
	// cannot interpret, and the record is how the mismatch is found without
	// losing the fact that revealed it.
	ErrUnknownKind = errors.New("ingestion: unknown fact kind")
	// ErrUnknownSchemaVersion marks a payload version this consumer cannot
	// read. The contract's own rule — refuse, never guess — because guessing
	// at settlement is guessing with money.
	ErrUnknownSchemaVersion = errors.New("ingestion: unknown payload schema version")
	// ErrMalformedFact marks a fact whose own columns are broken before any
	// payload is read: a request id outside the 1..256 grammar both planes
	// pin, or an append_seq below 1.
	ErrMalformedFact = errors.New("ingestion: the fact's columns do not meet the feed grammar")
	// ErrMalformedPayload marks a body that is not a decodable version-1
	// envelope, or an allocation tail that breaks the leg grammar —
	// ordinals not contiguous from 1, a non-positive amount, a blank or
	// repeated bucket. A payload no writer could have written was either
	// written by one that is broken or placed on the feed by something that
	// is not a writer; either way the fact is recorded, never applied.
	ErrMalformedPayload = errors.New("ingestion: the payload is not a version-1 envelope")
	// ErrCorrectionUnsupported marks a fact that names a correction — the
	// reserved corrects_append_seq column, non-null. This build implements
	// no correction path, so the fact is recorded as the explicit disposition
	// rather than applied as one; a silent skip would book the original and
	// lose the word that it was wrong.
	ErrCorrectionUnsupported = errors.New("ingestion: corrections are not implemented in this build")
	// ErrIncoherentFact marks a fact whose figures contradict each other or
	// the contract's pairing rules: a settled fact missing its pricing
	// basis, a settled_amount the hold formula cannot re-derive from the
	// fact's own counts and prices, a tail that cannot absorb the amount, a
	// released fact carrying usage columns. Every such fact is money that
	// cannot be accounted for if applied; recorded is how it stays
	// accountable.
	ErrIncoherentFact = errors.New("ingestion: the fact's figures do not cohere")
	// ErrPayloadOversize marks a payload too large for quarantined_facts to
	// record verbatim. It is deliberately NOT quarantinable: the choice is
	// not record-versus-apply but record-versus-truncate, and a truncated
	// copy of the evidence is not a record. The page stops; the fact is
	// still on the feed; a human resolves the state the contract says a
	// stopped feed is.
	ErrPayloadOversize = errors.New("ingestion: the payload is over the size a quarantine can record verbatim")
)

// Quarantinable reports whether a refusal from Interpret is one the
// consumer records verbatim and advances past. Everything else — oversize
// payloads, store failures, anything not from this package — is a page stop.
// The applier branches on this and on nothing else about the error's shape,
// so a new sentinel lands in one list or the other here or nowhere.
func Quarantinable(err error) bool {
	return errors.Is(err, ErrUnknownKind) ||
		errors.Is(err, ErrUnknownSchemaVersion) ||
		errors.Is(err, ErrMalformedFact) ||
		errors.Is(err, ErrMalformedPayload) ||
		errors.Is(err, ErrCorrectionUnsupported) ||
		errors.Is(err, ErrIncoherentFact)
}

// Outcome is the derived effect of one interpretable fact: the identity the
// applier dedups on, and the ledger outcome it books. The fields that name
// no effect on a given kind stay at their zero values — an orphan derives no
// legs and no amount, a release carries no price — because an absent effect
// and a zero effect are different sentences and the struct keeps them
// distinct.
type Outcome struct {
	// RequestID and Kind are the fact's identity, verbatim from the wire.
	RequestID string
	Kind      string

	// Class is the fact's idempotency class — the dedup key's second half.
	// Settlement-class facts book at most one effect per request; an
	// orphan's class is its own.
	Class string

	// CaptureMethod is the usage claim's provenance, on the kinds that
	// claim usage (settled, unbillable_orphaned).
	CaptureMethod string

	// SettledAmount is what the settled fact charged, re-derived and
	// re-checked against the fact's own binding. Zero on every other kind.
	SettledAmount int64

	// Price is the fact's pricing basis, for the consume legs a settled
	// derivation books. Zero-valued when no consume leg exists (a
	// zero-priced settle, a release, an expiry, an orphan).
	Price accounting.PriceSnapshot

	// Legs is the waterfall the fact closes out, with each leg's part
	// derived: Held is what the hold leg books, Consumed what the consume
	// leg takes from it (settled facts only), Released what returns.
	// Empty on an orphan, which derives no legs at all.
	Legs []Leg
}

// Leg is one waterfall leg's derived effect. Held, Consumed and Released are
// plain int64 because they are derived figures, not yet money — the applier
// moves them through the accounting constructors, whose own validation is
// what turns a derived figure into a booked amount.
type Leg struct {
	Bucket   accounting.FundingBucketID
	Held     int64
	Consumed int64
	Released int64
}

// wireLeg is the payload envelope's leg, exactly as the writer serialises
// it: the field names are the contract (the writer marshals through a
// struct for the same reason).
type wireLeg struct {
	FundingBucketID string `json:"funding_bucket_id"`
	Amount          int64  `json:"amount"`
	Ordinal         int    `json:"ordinal"`
}

// payloadV1 is the version-1 envelope: the allocation tail, nothing else.
type payloadV1 struct {
	Allocations []wireLeg `json:"allocations"`
}

// decodePayload binds the raw payload against the version-1 envelope.
// Unknown fields refuse: the writer marshals one flat struct and every
// field it writes is named here, so a payload carrying anything else was
// not written by a writer this contract describes — the same
// refuse-don't-ignore rule the rest of the grammar runs on.
func decodePayload(raw []byte) ([]wireLeg, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var payload payloadV1
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}
	// One document, no trailing bytes: a second value in the body is not a
	// tail this grammar describes.
	if dec.More() {
		return nil, fmt.Errorf("%w: more than one JSON value in the body", ErrMalformedPayload)
	}
	if payload.Allocations == nil {
		payload.Allocations = []wireLeg{}
	}
	return payload.Allocations, nil
}

// checkLegs enforces the allocation-tail grammar the writer's own
// reservation validation pins: contiguous ordinals from 1 in drawdown
// order, positive amounts, distinct buckets. The same three rules, in the
// same order, because a consumer that accepted a tail the writer could not
// have written would be deriving ledger legs from nothing.
func checkLegs(legs []wireLeg) error {
	seen := make(map[string]bool, len(legs))
	for i, leg := range legs {
		if leg.Ordinal != i+1 {
			return fmt.Errorf("%w: leg %d carries ordinal %d, want %d: ordinals are contiguous from 1 in drawdown order", ErrMalformedPayload, i, leg.Ordinal, i+1)
		}
		if leg.Amount < 1 {
			return fmt.Errorf("%w: leg %d carries amount %d, want at least 1", ErrMalformedPayload, i, leg.Amount)
		}
		if leg.FundingBucketID == "" || seen[leg.FundingBucketID] {
			return fmt.Errorf("%w: leg %d names a blank or repeated funding bucket", ErrMalformedPayload, i)
		}
		seen[leg.FundingBucketID] = true
	}
	return nil
}

// captureKnown mirrors the writer's capture-method vocabulary; a method
// outside it prices nothing.
func captureKnown(capture string) bool {
	switch capture {
	case "reported", "gateway_observed", "reservation_floor":
		return true
	}
	return false
}

// countOrZero reads a nullable count as the number it prices at — nobody-
// knows and zero both contribute nothing to a derivation, while the fact
// keeps the distinction between them. The writer derives with the same
// rule; the twin is what makes the two derivations agree.
func countOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// maxPriceRevisionOctets is the price-revision grammar the accounting
// domain pins on the textual reference a consume leg carries.
const maxPriceRevisionOctets = 256

// Interpret derives one fact's disposition: the outcome to apply, or the
// refusal to record. It never touches a store and never moves money — the
// applier does that from the outcome — so the whole grammar is testable
// against the contract's sentences alone.
//
// The checks run cheapest and most structural first — columns, version,
// kind, payload bytes, leg grammar — then the per-kind pairing rules, then
// the settled amount's re-derivation and the waterfall. A fact fails at the
// first sentence it breaks, and the error says which.
func Interpret(fact Fact) (Outcome, error) {
	if fact.RequestID == "" || len(fact.RequestID) > maxRequestIDOctets {
		return Outcome{}, fmt.Errorf("%w: request id must be 1..%d octets", ErrMalformedFact, maxRequestIDOctets)
	}
	if fact.AppendSeq < 1 {
		return Outcome{}, fmt.Errorf("%w: append seq %d is below 1", ErrMalformedFact, fact.AppendSeq)
	}
	if len(fact.Payload) > MaxPayloadOctets {
		return Outcome{}, fmt.Errorf("%w: %d octets, over the %d the quarantine can record", ErrPayloadOversize, len(fact.Payload), MaxPayloadOctets)
	}
	if fact.SchemaVersion != SchemaVersion {
		return Outcome{}, fmt.Errorf("%w: %d, want %d", ErrUnknownSchemaVersion, fact.SchemaVersion, SchemaVersion)
	}
	class, known := ClassOf(fact.Kind)
	if !known {
		return Outcome{}, fmt.Errorf("%w: %q", ErrUnknownKind, fact.Kind)
	}
	if fact.CorrectsAppendSeq != nil {
		return Outcome{}, fmt.Errorf("%w: names append seq %d for correction", ErrCorrectionUnsupported, *fact.CorrectsAppendSeq)
	}
	legs, err := decodePayload(fact.Payload)
	if err != nil {
		return Outcome{}, err
	}
	if err := checkLegs(legs); err != nil {
		return Outcome{}, err
	}
	switch fact.Kind {
	case KindSettled:
		return interpretSettled(fact, class, legs)
	case KindReleased, KindExpired:
		return interpretReleased(fact, class, legs)
	default:
		return interpretOrphaned(fact, class, legs)
	}
}

// interpretSettled derives the settlement-class effect of a settled fact:
// re-derive the amount from the fact's own figures, refuse anything the
// formula and the pairing rules cannot account for, then consume the tail
// greedy from the waterfall's head.
func interpretSettled(fact Fact, class string, legs []wireLeg) (Outcome, error) {
	capture, revision, inputPrice, outputPrice, amount, err := settledColumns(fact)
	if err != nil {
		return Outcome{}, err
	}
	// The binding is the fact's whole point: the amount must be what the
	// hold formula prices the fact's own counts and prices at, or the fact
	// is not a settlement an auditor can re-derive. Derivation overflow and
	// negative inputs are incoherence here — the formula's refusals are
	// answers about this fact, not page stops.
	derived, err := accounting.SettledAmount(countOrZero(fact.ProviderInputTokens), countOrZero(fact.ProviderOutputTokens), inputPrice, outputPrice)
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: the settled amount cannot be re-derived over the fact's figures: %v", ErrIncoherentFact, err)
	}
	if derived.Int64() != amount {
		return Outcome{}, fmt.Errorf("%w: settled amount %d disagrees with the %d the formula prices the fact's figures at", ErrIncoherentFact, amount, derived.Int64())
	}
	// Amount and tail travel together in both directions: a positive amount
	// rides a non-empty tail, a zero amount an empty one — the zero-priced
	// hold draws no legs, and a fact whose amount and tail disagree is
	// malformed, not priced.
	if (amount > 0) != (len(legs) > 0) {
		return Outcome{}, fmt.Errorf("%w: settled amount %d with a %d-leg tail: a positive amount carries legs and a zero amount none", ErrIncoherentFact, amount, len(legs))
	}
	out := Outcome{
		RequestID:     fact.RequestID,
		Kind:          fact.Kind,
		Class:         class,
		CaptureMethod: capture,
		SettledAmount: amount,
		Price: accounting.PriceSnapshot{
			RevisionID:      accounting.PriceRevisionID(revision),
			InputUnitPrice:  accounting.Amount(inputPrice),
			OutputUnitPrice: accounting.Amount(outputPrice),
		},
	}
	// The waterfall, normatively: walk the legs in ascending ordinal order,
	// consume min(remaining, amount) per leg — the boundary leg may be
	// partially consumed — and release each leg's unconsumed remainder.
	// The tail must absorb the total; a tail that sums to less is refused,
	// never converged around.
	remaining := amount
	for _, leg := range legs {
		consumed := leg.Amount
		if remaining < consumed {
			consumed = remaining
		}
		remaining -= consumed
		out.Legs = append(out.Legs, Leg{
			Bucket:   accounting.FundingBucketID(leg.FundingBucketID),
			Held:     leg.Amount,
			Consumed: consumed,
			Released: leg.Amount - consumed,
		})
	}
	if remaining > 0 {
		return Outcome{}, fmt.Errorf("%w: the tail's legs sum to less than the settled amount %d", ErrIncoherentFact, amount)
	}
	return out, nil
}

// settledColumns reads and pairs a settled fact's required columns. Each
// missing or malformed pairing is incoherence: the contract makes these
// columns the settled fact's shape, and a fact without them is not a
// settlement this plane can account for.
func settledColumns(fact Fact) (capture, revision string, inputPrice, outputPrice, amount int64, err error) {
	if fact.CaptureMethod == nil || !captureKnown(*fact.CaptureMethod) {
		return "", "", 0, 0, 0, fmt.Errorf("%w: a settled fact carries a known capture method", ErrIncoherentFact)
	}
	if fact.CommittedAttemptID == nil || *fact.CommittedAttemptID == "" {
		return "", "", 0, 0, 0, fmt.Errorf("%w: a settled fact names the attempt its usage came from", ErrIncoherentFact)
	}
	if fact.PriceRevisionID == nil || *fact.PriceRevisionID == "" || len(*fact.PriceRevisionID) > maxPriceRevisionOctets {
		return "", "", 0, 0, 0, fmt.Errorf("%w: a settled fact carries its price revision", ErrIncoherentFact)
	}
	if fact.InputUnitPrice == nil || fact.OutputUnitPrice == nil || *fact.InputUnitPrice < 0 || *fact.OutputUnitPrice < 0 {
		return "", "", 0, 0, 0, fmt.Errorf("%w: a settled fact carries two non-negative unit prices", ErrIncoherentFact)
	}
	if fact.SettledAmount == nil || *fact.SettledAmount < 0 {
		return "", "", 0, 0, 0, fmt.Errorf("%w: a settled fact carries a non-negative settled amount", ErrIncoherentFact)
	}
	if !countsNonNegative(fact.ProviderInputTokens, fact.ProviderOutputTokens, fact.DeliveryTokens) {
		return "", "", 0, 0, 0, fmt.Errorf("%w: usage counts must not be negative", ErrIncoherentFact)
	}
	return *fact.CaptureMethod, *fact.PriceRevisionID, *fact.InputUnitPrice, *fact.OutputUnitPrice, *fact.SettledAmount, nil
}

// interpretReleased derives a released or expired fact: the whole tail goes
// back, no usage is claimed, and the contract's pairing rule makes every
// usage-bearing column null — a claim of usage on a no-usage fact is not
// extra telemetry but a contradiction.
func interpretReleased(fact Fact, class string, legs []wireLeg) (Outcome, error) {
	if fact.CaptureMethod != nil || fact.CommittedAttemptID != nil ||
		fact.ProviderInputTokens != nil || fact.ProviderOutputTokens != nil ||
		fact.DeliveryTokens != nil ||
		fact.PriceRevisionID != nil || fact.InputUnitPrice != nil || fact.OutputUnitPrice != nil ||
		fact.SettledAmount != nil {
		return Outcome{}, fmt.Errorf("%w: a %s fact claims no usage, and carries a usage column", ErrIncoherentFact, fact.Kind)
	}
	out := Outcome{RequestID: fact.RequestID, Kind: fact.Kind, Class: class}
	for _, leg := range legs {
		out.Legs = append(out.Legs, Leg{
			Bucket:   accounting.FundingBucketID(leg.FundingBucketID),
			Held:     leg.Amount,
			Released: leg.Amount,
		})
	}
	return out, nil
}

// interpretOrphaned derives an unbillable orphan: the feed's word that usage
// was observed on a named attempt, with no amount derived and no money
// moved. Its provenance columns are required — the fact exists to name who
// observed what — while the pricing columns must be absent: a price on a
// fact that derives no charge is a contradiction.
func interpretOrphaned(fact Fact, class string, legs []wireLeg) (Outcome, error) {
	if fact.CaptureMethod == nil || !captureKnown(*fact.CaptureMethod) {
		return Outcome{}, fmt.Errorf("%w: an unbillable orphan carries a known capture method", ErrIncoherentFact)
	}
	if fact.CommittedAttemptID == nil || *fact.CommittedAttemptID == "" {
		return Outcome{}, fmt.Errorf("%w: an unbillable orphan names the attempt its usage was observed on", ErrIncoherentFact)
	}
	if fact.PriceRevisionID != nil || fact.InputUnitPrice != nil || fact.OutputUnitPrice != nil || fact.SettledAmount != nil {
		return Outcome{}, fmt.Errorf("%w: an unbillable orphan derives no charge, and carries a pricing column", ErrIncoherentFact)
	}
	if !countsNonNegative(fact.ProviderInputTokens, fact.ProviderOutputTokens, fact.DeliveryTokens) {
		return Outcome{}, fmt.Errorf("%w: usage counts must not be negative", ErrIncoherentFact)
	}
	// The tail rides along as the telemetry of what was held; the orphan's
	// effect is none of it — no legs, no amount, an applied_facts row and
	// nothing else. decodePayload and checkLegs have already held the bytes
	// to the grammar, which is all the effect they are allowed to have.
	return Outcome{RequestID: fact.RequestID, Kind: fact.Kind, Class: class, CaptureMethod: *fact.CaptureMethod}, nil
}

// countsNonNegative refuses a present-and-negative count on any
// usage-bearing fact.
func countsNonNegative(counts ...*int64) bool {
	for _, count := range counts {
		if count != nil && *count < 0 {
			return false
		}
	}
	return true
}
