package accounting

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// Kind is the closed vocabulary of what a fact says, and it splits exactly the
// way the request's terminal vocabulary does:
//
//   - Settled: the request completed and its usage is priced — the one
//     settlement-relevant ending that carries an amount.
//   - Released: the request finalised without settleable usage and the whole
//     hold went back.
//   - Expired: the reaper closed a hold whose window lapsed; no usage is
//     claimed and the hold is gone.
//   - UnbillableOrphaned: a proven completion whose usage cannot bill — usage
//     observed, no charge derived, never a customer amount. It is not
//     settlement-relevant and may coexist with an expired fact by design.
//
// Released and expired facts carry none of the settlement figures; their
// allocation tail rides in the payload.
type Kind string

const (
	KindSettled            Kind = "settled"
	KindReleased           Kind = "released"
	KindExpired            Kind = "expired"
	KindUnbillableOrphaned Kind = "unbillable_orphaned"
)

// CaptureMethod names how a usage-bearing fact's figures were obtained, and
// the value is the consumer's confidence label:
//
//   - Reported: the provider's own usage report, received whole.
//   - GatewayObserved: the runtime counted what crossed the wire itself —
//     the honest number when no report arrives, and marked as such.
//   - ReservationFloor: the settlement fell back to what the reservation held,
//     because nothing better was known; the most conservative number there is.
type CaptureMethod string

const (
	CaptureReported         CaptureMethod = "reported"
	CaptureGatewayObserved  CaptureMethod = "gateway_observed"
	CaptureReservationFloor CaptureMethod = "reservation_floor"
)

// SchemaVersion is the payload shape's version, and 1 is the only version this
// build writes. The column exists so a consumer that predates a payload can
// say so instead of guessing; the constant lives beside the envelope it
// versions because the two change together or not at all.
const SchemaVersion = 1

// maxPayloadOctets is the writer-side cap on a serialised payload: half of the
// database column's own 32768-octet CHECK, so the legible error happens at the
// write site and the constraint stays the wall behind it. A payload that
// cannot fit half its column is a settlement gone wrong — thousands of
// waterfall legs on one request — and it is better refused than trimmed.
const maxPayloadOctets = 16384

// ErrFactShape marks a fact whose kind and figures disagree — a settled fact
// without an amount, a released one carrying tokens, an orphaned one without
// its capture method. Every pairing here has a twin CHECK on the row; this is
// the same refusal, spoken before the write.
var ErrFactShape = errors.New("accounting: fact kind and figures disagree")

// Fact is one immutable statement about a request's ending: what happened
// (Kind), how its usage was known (CaptureMethod), what it priced to, and the
// allocation legs it closes out.
//
// Immutable is the operative word: a fact states what happened, and a
// correction — when the day comes — is a NEW fact whose CorrectsAppendSeq
// names the one it amends (ADR 0004 invariant 1). Nothing updates a fact,
// nothing deletes one, and no caller acknowledges having read one; the feed is
// pull-and-replay for exactly these reasons.
//
// The append sequence is not set by the caller: it is allocated by the store,
// at commit, in the order commits become visible. A caller that numbered its
// own facts would be stamping an order the database does not guarantee, and
// the consumer's cursor would trust it.
type Fact struct {
	// AppendSeq is the store-allocated position in the feed's single monotonic
	// order. Zero means "not yet appended"; the repository assigns it.
	AppendSeq int64

	RequestID identity.RequestID
	Kind      Kind
	// SchemaVersion is always SchemaVersion in this build; it is a field so
	// the value travels with the row it describes rather than being
	// re-derived at every write.
	SchemaVersion int
	CaptureMethod CaptureMethod
	// CommittedAttemptID names the attempt the usage belongs to. Usage-bearing
	// facts always name one; released and expired facts name none.
	CommittedAttemptID identity.AttemptID

	ProviderInputTokens  *int64
	ProviderOutputTokens *int64
	DeliveryTokens       *int64

	// PriceRevision and the two unit prices are the settled fact's pricing
	// basis, present together or not at all (the settled_price shape). The
	// prices are pointers for the same reason the usage counts are: zero is a
	// real price — a free model settles at zero — and NULL is a different
	// claim entirely (this ending carries no pricing at all).
	PriceRevision   string
	InputUnitPrice  *int64
	OutputUnitPrice *int64
	// SettledAmount is what the request cost, in integer minor units. Nil on
	// every non-settled fact; zero is a real settled amount.
	SettledAmount *int64

	// CorrectsAppendSeq is reserved for the correction path and is nil on
	// every fact this build writes. Its existence in the type is what keeps
	// the wire shape from foreclosing corrections later.
	CorrectsAppendSeq *int64

	// Payload is the fact's allocation tail — the waterfall legs this ending
	// closes out — serialised once, here, by BuildPayload. Opaque on the wire
	// and to every consumer: the shape belongs to this package.
	Payload json.RawMessage

	OccurredAt time.Time
}

// AllocationLeg is one funding bucket's part in a fact's allocation tail —
// the same shape the reservation's legs carry, because the fact is where the
// reservation's memory becomes the Control Plane's derivation input.
type AllocationLeg struct {
	FundingBucketID string `json:"funding_bucket_id"`
	Amount          int64  `json:"amount"`
	Ordinal         int    `json:"ordinal"`
}

// payloadV1 is the version-1 payload envelope: the allocation tail, nothing
// else. Fields stay in one flat object so a consumer's decoder has one shape
// to bind against for every kind.
type payloadV1 struct {
	Allocations []AllocationLeg `json:"allocations"`
}

// NewSettled builds the fact a settlement close produces: usage, capture,
// price, amount, and the legs the hold was drawn down against.
func NewSettled(requestID identity.RequestID, attempt identity.AttemptID, capture CaptureMethod, input, output, delivery *int64, priceRevision string, inputUnitPrice, outputUnitPrice, settledAmount int64, legs []AllocationLeg, occurredAt time.Time) (Fact, error) {
	if attempt == "" {
		return Fact{}, fmt.Errorf("%w: a settled fact names the attempt its usage came from", ErrFactShape)
	}
	if !captureKnown(capture) {
		return Fact{}, fmt.Errorf("%w: %q is not a capture method", ErrFactShape, string(capture))
	}
	if priceRevision == "" || inputUnitPrice < 0 || outputUnitPrice < 0 || settledAmount < 0 {
		return Fact{}, fmt.Errorf("%w: a settled fact carries its pricing basis and a non-negative amount", ErrFactShape)
	}
	if err := checkUsage(input, output, delivery); err != nil {
		return Fact{}, err
	}
	pricedIn := inputUnitPrice
	pricedOut := outputUnitPrice
	amount := settledAmount
	fact := Fact{
		RequestID:            requestID,
		Kind:                 KindSettled,
		SchemaVersion:        SchemaVersion,
		CaptureMethod:        capture,
		CommittedAttemptID:   attempt,
		ProviderInputTokens:  input,
		ProviderOutputTokens: output,
		DeliveryTokens:       delivery,
		PriceRevision:        priceRevision,
		InputUnitPrice:       &pricedIn,
		OutputUnitPrice:      &pricedOut,
		SettledAmount:        &amount,
		OccurredAt:           occurredAt,
	}
	if err := checkUsage(input, output, delivery); err != nil {
		return Fact{}, err
	}
	if err := fact.setPayload(legs); err != nil {
		return Fact{}, err
	}
	return fact, nil
}

// NewReleased builds the fact a release close produces: the whole hold went
// back, no usage is claimed, and the legs ride in the payload so the Control
// Plane can return each bucket's part.
func NewReleased(requestID identity.RequestID, legs []AllocationLeg, occurredAt time.Time) (Fact, error) {
	fact := Fact{
		RequestID:     requestID,
		Kind:          KindReleased,
		SchemaVersion: SchemaVersion,
		OccurredAt:    occurredAt,
	}
	if err := fact.setPayload(legs); err != nil {
		return Fact{}, err
	}
	return fact, nil
}

// NewExpired builds the fact the reaper's close produces: the hold lapsed, no
// usage is claimed, the legs ride in the payload as a released one's do.
func NewExpired(requestID identity.RequestID, legs []AllocationLeg, occurredAt time.Time) (Fact, error) {
	fact := Fact{
		RequestID:     requestID,
		Kind:          KindExpired,
		SchemaVersion: SchemaVersion,
		OccurredAt:    occurredAt,
	}
	if err := fact.setPayload(legs); err != nil {
		return Fact{}, err
	}
	return fact, nil
}

// NewUnbillableOrphaned builds the fact a proven orphaned completion
// produces: usage was observed on a named attempt, no amount is derived, and
// the fact exists so the request's ending is stated rather than silent. It is
// never a customer charge, and it may coexist with an expired fact — which is
// why it sits outside the settlement dedup unique by design.
func NewUnbillableOrphaned(requestID identity.RequestID, attempt identity.AttemptID, capture CaptureMethod, input, output, delivery *int64, legs []AllocationLeg, occurredAt time.Time) (Fact, error) {
	fact := Fact{
		RequestID:            requestID,
		Kind:                 KindUnbillableOrphaned,
		SchemaVersion:        SchemaVersion,
		CaptureMethod:        capture,
		CommittedAttemptID:   attempt,
		ProviderInputTokens:  input,
		ProviderOutputTokens: output,
		DeliveryTokens:       delivery,
		OccurredAt:           occurredAt,
	}
	if attempt == "" {
		return Fact{}, fmt.Errorf("%w: an orphaned fact names the attempt its usage was observed on", ErrFactShape)
	}
	if !captureKnown(capture) {
		return Fact{}, fmt.Errorf("%w: %q is not a capture method", ErrFactShape, string(capture))
	}
	if err := checkUsage(input, output, delivery); err != nil {
		return Fact{}, err
	}
	if err := fact.setPayload(legs); err != nil {
		return Fact{}, err
	}
	return fact, nil
}

// setPayload serialises the allocation tail into the envelope and enforces the
// writer-side cap. The envelope marshals through a struct, not a map: the
// field names are the contract, and a map would let a caller spell one wrong.
func (f *Fact) setPayload(legs []AllocationLeg) error {
	if legs == nil {
		legs = []AllocationLeg{}
	}
	encoded, err := json.Marshal(payloadV1{Allocations: legs})
	if err != nil {
		return fmt.Errorf("accounting: serialise fact payload: %w", err)
	}
	if len(encoded) > maxPayloadOctets {
		return fmt.Errorf("accounting: fact payload is %d octets, over the %d-octet writer cap", len(encoded), maxPayloadOctets)
	}
	f.Payload = encoded
	return nil
}

// checkUsage refuses a negative usage figure on a usage-bearing fact. Counts
// stay optional — usage that could not be observed is settled conservatively,
// never invented — but a count that is present and negative is a claim nothing
// can settle.
func checkUsage(input, output, delivery *int64) error {
	for _, v := range []*int64{input, output, delivery} {
		if v != nil && *v < 0 {
			return fmt.Errorf("%w: usage counts must not be negative", ErrFactShape)
		}
	}
	return nil
}

func captureKnown(capture CaptureMethod) bool {
	switch capture {
	case CaptureReported, CaptureGatewayObserved, CaptureReservationFloor:
		return true
	}
	return false
}
