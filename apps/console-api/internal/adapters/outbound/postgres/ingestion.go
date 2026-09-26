package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The fact consumer's effect tables: translation between the persistence
// port's AppliedFacts and QuarantinedFacts and the `control` database's
// applied_facts and quarantined_facts (control migration 000008).
//
// Both tables are written only inside the replay's unit of work — the whole
// point of them is to stand or fall with the cursor advance in the same
// transaction — and both refuse a write that arrives outside one. Reads run
// through the store's context-resolved handle, so an applier's Find inside
// the unit of work sees that unit's own earlier writes.

// errAppliedOutsideUnitOfWork is Record's refusal to run autocommitted: the
// row is the second half of the exactly-once boundary, and on the pool it
// would commit apart from the effect it speaks for.
var errAppliedOutsideUnitOfWork = errors.New("refused: an applied-fact record is unit-of-work-shaped and ctx carries no unit of work")

// errQuarantineOutsideUnitOfWork is the quarantine's equivalent refusal: a
// refusal recorded outside the transaction that advanced past the fact is
// one half of a disposition, and half a disposition is a skipped fact with
// extra steps.
var errQuarantineOutsideUnitOfWork = errors.New("refused: a quarantined-fact record is unit-of-work-shaped and ctx carries no unit of work")

// maxReasonRunes is the refusal-reason length the schema records
// (quarantined_facts_reason_grammar). The reason is a diagnostic for the
// operator who resolves the quarantine, not a machine contract, so an
// interpretation error whose sentence outgrows the column is truncated at
// the record rather than turned into a second refusal about the first.
const maxReasonRunes = 512

// NewAppliedFacts returns the persistence port's idempotency ledger, backed
// by store. It panics on a nil store for the reason every constructor in
// this package does.
func NewAppliedFacts(store persistence.Store) persistence.AppliedFacts {
	if store == nil {
		panic("postgres: NewAppliedFacts requires a non-nil persistence.Store")
	}
	return &appliedFactsRepo{store: store}
}

// NewQuarantinedFacts returns the persistence port's quarantine, backed by
// store.
func NewQuarantinedFacts(store persistence.Store) persistence.QuarantinedFacts {
	if store == nil {
		panic("postgres: NewQuarantinedFacts requires a non-nil persistence.Store")
	}
	return &quarantinedFactsRepo{store: store}
}

type appliedFactsRepo struct {
	store persistence.Store
}

func (r *appliedFactsRepo) Find(ctx context.Context, requestID, kindClass string) (*persistence.AppliedFact, error) {
	q := r.store.Querier(ctx)
	row := q.QueryRowContext(ctx, `
		SELECT request_id, kind_class, kind, append_seq, settled_amount, capture_method, settlement_id
		FROM control.applied_facts
		WHERE request_id = $1 AND kind_class = $2
	`, requestID, kindClass)

	var applied persistence.AppliedFact
	var settledAmount *int64
	var captureMethod *string
	var settlementID []byte
	err := row.Scan(&applied.RequestID, &applied.KindClass, &applied.Kind, &applied.AppendSeq,
		&settledAmount, &captureMethod, &settlementID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: find applied fact for request %s class %s: %w", requestID, kindClass, err)
	}
	applied.SettledAmount = settledAmount
	applied.CaptureMethod = captureMethod
	applied.SettlementID = string(settlementID)
	return &applied, nil
}

func (r *appliedFactsRepo) Record(ctx context.Context, applied persistence.AppliedFact) error {
	if !r.store.InUnitOfWork(ctx) {
		return fmt.Errorf("postgres: record applied fact for request %s class %s: %w",
			applied.RequestID, applied.KindClass, errAppliedOutsideUnitOfWork)
	}
	var settlementID any
	if applied.SettlementID != "" {
		settlementID = applied.SettlementID
	}
	_, err := r.store.Querier(ctx).ExecContext(ctx, `
		INSERT INTO control.applied_facts
			(request_id, kind_class, kind, append_seq, settled_amount, capture_method, settlement_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, applied.RequestID, applied.KindClass, applied.Kind, applied.AppendSeq,
		applied.SettledAmount, applied.CaptureMethod, settlementID)
	if err != nil {
		return fmt.Errorf("postgres: record applied fact for request %s class %s: %w",
			applied.RequestID, applied.KindClass, err)
	}
	return nil
}

type quarantinedFactsRepo struct {
	store persistence.Store
}

func (r *quarantinedFactsRepo) Record(ctx context.Context, quarantined persistence.QuarantinedFact) error {
	if !r.store.InUnitOfWork(ctx) {
		return fmt.Errorf("postgres: record quarantined fact for request %s seq %d: %w",
			quarantined.RequestID, quarantined.AppendSeq, errQuarantineOutsideUnitOfWork)
	}
	_, err := r.store.Querier(ctx).ExecContext(ctx, `
		INSERT INTO control.quarantined_facts
			(request_id, append_seq, kind, schema_version, occurred_at, payload,
			 capture_method, committed_attempt_id, provider_input_tokens, provider_output_tokens,
			 delivery_tokens, price_revision_id, input_unit_price, output_unit_price,
			 settled_amount, corrects_append_seq, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (request_id, append_seq, kind) DO NOTHING
	`, quarantined.RequestID, quarantined.AppendSeq, quarantined.Kind, quarantined.SchemaVersion,
		quarantined.OccurredAt, string(quarantined.Payload),
		quarantined.CaptureMethod, quarantined.CommittedAttemptID,
		quarantined.ProviderInputTokens, quarantined.ProviderOutputTokens, quarantined.DeliveryTokens,
		quarantined.PriceRevision, quarantined.InputUnitPrice, quarantined.OutputUnitPrice,
		quarantined.SettledAmount, quarantined.CorrectsAppendSeq,
		truncateReason(quarantined.Reason))
	if err != nil {
		return fmt.Errorf("postgres: record quarantined fact for request %s seq %d: %w",
			quarantined.RequestID, quarantined.AppendSeq, err)
	}
	return nil
}

// truncateReason bounds the refusal reason to the length the schema records,
// by runes — the column's CHECK counts characters, not octets, and the
// truncation should give up exactly as much as the column would refuse.
func truncateReason(reason string) string {
	if utf8.RuneCountInString(reason) <= maxReasonRunes {
		return reason
	}
	runes := []rune(reason)
	return strings.TrimSpace(string(runes[:maxReasonRunes]))
}
