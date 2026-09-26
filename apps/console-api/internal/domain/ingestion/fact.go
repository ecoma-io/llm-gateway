// Package ingestion is the Control Plane's own grammar for the usage-fact
// feed: the kinds a fact can carry, the idempotency class each kind belongs
// to, the version-1 payload envelope, and the one derivation that turns a
// fact into the ledger outcome its settlement books (ADR 0004, ADR 0006 §5).
//
// It is the consumer-side twin of the Data Plane's fact writer
// (apps/dataplane/internal/domain/accounting — the Fact type, its payloadV1
// envelope and its Hold formula), and the two packages meet in exactly one
// place: the normative sentences of api/openapi/shared/usage-facts.yaml.
// Nothing here imports the writer and nothing there imports here — a shared
// Go package across the plane boundary would let one plane's refactor quietly
// rewrite the other's contract, and the contract is the only thing the two
// planes are allowed to share.
//
// The package's whole job is to answer one question about each fact — what
// ledger effect does it derive, or why may none be derived — and to refuse
// everything else in words a caller can branch on. Interpret is that answer;
// the sentinels below are its refusal vocabulary, split by what the caller
// does with each refusal: the quarantinable ones are recorded verbatim and
// the page advances past them, and the rest stop the page where it stands,
// because a fact a consumer can neither apply nor record verbatim is a state
// a human resolves (the contract's own words).
package ingestion

import "time"

// The feed's kinds. The four strings are the wire vocabulary — the values
// the Data Plane writes and this contract pins — and a kind outside them is
// not an error to crash on but a disposition to record (ErrUnknownKind).
const (
	KindSettled            = "settled"
	KindReleased           = "released"
	KindExpired            = "expired"
	KindUnbillableOrphaned = "unbillable_orphaned"
)

// The idempotency classes — the two values applied_facts.kind_class is
// pinned to. A request travels to exactly one terminal state, so its
// settled, released and expired facts are one class and the first to arrive
// books the only effect; an unbillable orphan is separate bookkeeping a
// request may carry beside its settlement. The class is the dedup key's
// second half everywhere downstream; the kind alone is never one.
const (
	ClassSettlement         = "settlement"
	ClassUnbillableOrphaned = "unbillable_orphaned"
)

// SchemaVersion is the only payload version this consumer can read. The
// contract pins the version field's minimum at 1 and demands a consumer
// refuse what it does not know rather than guess — guessing at settlement is
// guessing with money.
const SchemaVersion = 1

// MaxPayloadOctets is the size the quarantined_facts payload column can
// record verbatim — the schema's own CHECK. The writer's cap is half this
// (16384, in the Data Plane's domain guard), so a payload that passes this
// one was not written by any writer that exists, and it cannot be taken
// verbatim either: the refusal stops the page rather than filing a
// truncated copy of the thing being complained about.
const MaxPayloadOctets = 32768

// maxRequestIDOctets is the request-id grammar this plane asserts — the same
// 1..256 the accounting domain and both fact tables pin. The identifier
// crosses the boundary as opaque text; presence and length are the whole
// assertion.
const maxRequestIDOctets = 256

// Fact is one feed fact, as the wire delivered it: the typed columns the
// envelope carries and the payload verbatim, undecoded. The zero-value
// pointers are meaningful — a column's absence is a statement about the
// fact (the contract says which kinds carry which columns), not a missing
// field — so the struct is the wire's shape, not a constructor-normalised
// one.
type Fact struct {
	// AppendSeq is the fact's position in the feed's commit ordering. It
	// identifies the fact inside a quarantine record; it is never an
	// idempotency key (a redelivery re-reads the same seq, and the class
	// key — not the seq — is what makes that a no-op).
	AppendSeq int64

	// RequestID is the request the fact closes out — the reservation's
	// identity on this plane and the settlement's exactly-once key.
	RequestID string

	// Kind is the raw wire kind. It is a string, not an enum: an unknown
	// kind must survive decoding to be quarantined with its name, and an
	// enum would refuse it before anyone could record it.
	Kind string

	// SchemaVersion is the payload version the writer declared. Only
	// SchemaVersion (1) is interpretable here.
	SchemaVersion int

	// OccurredAt is when the runtime observed the outcome. Telemetry: never
	// an ordering key, never a dedup key, carried into quarantine verbatim.
	OccurredAt time.Time

	// Payload is the fact body verbatim — the allocation tail under the
	// version-1 envelope, the only sanctioned bytes on this feed. Decode
	// belongs to this package (decodePayload); nothing downstream ever
	// sees it raw.
	Payload []byte

	// The usage claim and its provenance. Present on settled and
	// unbillable_orphaned facts; the contract makes each column null on
	// released and expired, and a violation of that pairing is incoherence,
	// not absence.
	CaptureMethod        *string
	CommittedAttemptID   *string
	ProviderInputTokens  *int64
	ProviderOutputTokens *int64
	DeliveryTokens       *int64

	// The pricing basis: the revision the settlement priced at and the two
	// unit prices copied from it. Settled facts carry all three or none.
	PriceRevisionID *string
	InputUnitPrice  *int64
	OutputUnitPrice *int64

	// SettledAmount is what the request cost on a settled fact — bound, by
	// the writer, to the hold formula over this fact's own figures. The
	// consumer re-derives rather than trusts.
	SettledAmount *int64

	// CorrectsAppendSeq is the correction path's reserved column. This
	// build implements no correction path, so a fact that names one is
	// refused with ErrCorrectionUnsupported — recorded, never applied.
	CorrectsAppendSeq *int64
}

// ClassOf maps a kind to its idempotency class. It returns false for a kind
// outside the feed's vocabulary — the caller's signal to refuse with
// ErrUnknownKind rather than invent a class for it (an invented class would
// be an applied_facts row the schema's CHECK refuses anyway, and the fact
// would come back as a database error instead of a recorded disposition).
func ClassOf(kind string) (string, bool) {
	switch kind {
	case KindSettled, KindReleased, KindExpired:
		return ClassSettlement, true
	case KindUnbillableOrphaned:
		return ClassUnbillableOrphaned, true
	}
	return "", false
}
