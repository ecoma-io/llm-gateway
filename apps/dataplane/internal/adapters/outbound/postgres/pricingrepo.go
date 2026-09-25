package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The price book: the read half of the client price list (ADR 0003). The
// selection is the two steps the migration pins, and both run through the
// store's Querier so they land in the caller's unit of work — one snapshot,
// one transaction_timestamp(), and the revision and its entry cannot disagree
// about which instant they were read at.
//
// The two steps are separate statements on purpose: the second's predicate
// needs the first's answer (the entry is keyed by the revision), and a join
// would silently turn "revision effective, alias unpriced" and "no revision
// effective" into one indistinguishable empty set. Two statements keep the
// two configuration states distinct at the read, even though both surface as
// the one sentinel — the miss is an answer, not a malfunction.

// selectEffectiveRevision is step one: the effective revision, the single
// activated revision with the greatest effective_from not after the
// transaction's own instant. transaction_timestamp() is transaction-stable —
// every read in the unit of work selects at the same instant, which is the
// clock rule ADR 0003 pins (the database's, never a caller's). The partial
// unique client_price_list_revisions_activated_effective_from_key is what
// makes "the single" true; this ORDER BY ... LIMIT 1 is how the selection
// reads, and the id is carried out as text because the revision id travels
// through the domain and the facts as text.
const selectEffectiveRevision = `SELECT id::text, version
FROM client_price_list_revisions
WHERE state = 'activated' AND effective_from <= transaction_timestamp()
ORDER BY effective_from DESC
LIMIT 1`

// selectEntryPrice is step two: the effective revision's price for the alias.
// Alias-exact: an alias the revision does not price has no row, and the
// missing row is the refusal — never a neighbouring alias's price, never
// zero. $1 is the revision id from step one, $2 the alias.
const selectEntryPrice = `SELECT input_unit_price, output_unit_price
FROM client_price_list_entries
WHERE revision_id = $1 AND alias_id = $2`

// NewPriceBook returns the persistence port's PriceBook repository backed by
// store. It panics on a nil store for the same reason the store panics on a
// nil pool: the failure a nil dependency produces later is strictly worse
// than a loud one here.
func NewPriceBook(store persistence.Store) persistence.PriceBook {
	if store == nil {
		panic("postgres: NewPriceBook requires a non-nil persistence.Store")
	}
	return &priceBookRepo{store: store}
}

// Compile-time proof that the repository satisfies the port's contract.
var _ persistence.PriceBook = (*priceBookRepo)(nil)

// priceBookRepo is the PostgreSQL implementation of the persistence port's
// PriceBook.
type priceBookRepo struct {
	store persistence.Store
}

// EffectiveAt implements persistence.PriceBook: the two-step selection, and
// either step coming up empty is ErrNoEffectivePrice — wrapped, so the
// caller sees the alias it could not price and never a SQL-shaped miss.
func (repository *priceBookRepo) EffectiveAt(ctx context.Context, aliasID catalog.AliasID) (catalog.PriceSnapshot, error) {
	querier := repository.store.Querier(ctx)
	var revisionID string
	var version int
	err := querier.QueryRowContext(ctx, selectEffectiveRevision).Scan(&revisionID, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return catalog.PriceSnapshot{}, fmt.Errorf("postgres: effective price for alias %s: no activated revision is effective: %w", aliasID, persistence.ErrNoEffectivePrice)
	}
	if err != nil {
		return catalog.PriceSnapshot{}, fmt.Errorf("postgres: effective price for alias %s: read the effective revision: %w", aliasID, err)
	}
	var inputUnitPrice, outputUnitPrice int64
	err = querier.QueryRowContext(ctx, selectEntryPrice, revisionID, string(aliasID)).Scan(&inputUnitPrice, &outputUnitPrice)
	if errors.Is(err, sql.ErrNoRows) {
		return catalog.PriceSnapshot{}, fmt.Errorf("postgres: effective price for alias %s: the effective revision prices no entry for it: %w", aliasID, persistence.ErrNoEffectivePrice)
	}
	if err != nil {
		return catalog.PriceSnapshot{}, fmt.Errorf("postgres: effective price for alias %s: read the alias's entry: %w", aliasID, err)
	}
	return catalog.PriceSnapshot{
		RevisionID:      revisionID,
		Version:         version,
		InputUnitPrice:  inputUnitPrice,
		OutputUnitPrice: outputUnitPrice,
	}, nil
}
