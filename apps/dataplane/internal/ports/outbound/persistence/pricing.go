package persistence

import (
	"context"
	"errors"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
)

// ErrNoEffectivePrice says no price answers for the alias: either no
// activated revision is yet effective (none activated, or none whose
// effective_from has arrived), or the effective revision prices no entry for
// the alias. Both are configuration states, not malfunctions — an admission
// with no price is a refusal, never a price of zero, and never a
// neighbouring alias's price (entries are alias-exact).
var ErrNoEffectivePrice = errors.New("persistence: no effective price for the alias")

// PriceBook reads the client price list: the operator-managed prices the
// runtime prices a request under (ADR 0003).
type PriceBook interface {
	// EffectiveAt resolves the two-step selection inside the caller's unit of
	// work: first the effective revision — the single activated revision with
	// the greatest effective_from not after transaction_timestamp() — then
	// that revision's entry for the alias. The instant is the SQL
	// transaction's own, never a caller-supplied clock: ADR 0003 pins the
	// read clock to the database's, and transaction_timestamp() being
	// transaction-stable is what lets both steps of one read, and the
	// drawdown that shares the unit of work, select at one instant. The two
	// statements run through the Querier ctx resolves, so the revision and
	// its entry are read as one consistent whole exactly when ctx carries a
	// unit of work.
	//
	// ErrNoEffectivePrice means the alias is unpriced at that instant;
	// ErrNotFound is never used here, because an unpriced alias is a
	// configuration answer rather than a missing row the caller could retry.
	EffectiveAt(ctx context.Context, aliasID catalog.AliasID) (catalog.PriceSnapshot, error)
}
