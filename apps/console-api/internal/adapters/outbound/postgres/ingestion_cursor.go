package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The ingestion cursor: translation between the persistence port's
// IngestionCursor and the `control` database's ingestion_cursor singleton
// (control migration 000008). The cursor is the one thing about the fact feed
// the Data Plane never learns (ADR 0006 §5), and this table is where that
// privacy lives — a position the Control Plane wrote for itself, read by
// nothing outside this module.
//
// Reads run through the store's context-resolved handle and so may run
// anywhere; the advance refuses to. An advance is only meaningful in the same
// transaction as the work it claims to sit after — the whole-page rule stands
// on that — and on the pool it would commit apart from that work, which is
// the claim without it.

// errCursorAdvanceOutsideUnitOfWork is Advance's refusal to run
// autocommitted: a position moved outside the transaction that did the work
// is a claim about work that never happened.
var errCursorAdvanceOutsideUnitOfWork = errors.New("refused: a cursor advance is unit-of-work-shaped and ctx carries no unit of work")

// NewIngestionCursor returns the persistence port's position store, backed by
// store. It panics on a nil store for the reason every constructor in this
// package does.
func NewIngestionCursor(store persistence.Store) persistence.IngestionCursor {
	if store == nil {
		panic("postgres: NewIngestionCursor requires a non-nil persistence.Store")
	}
	return &ingestionCursorRepo{store: store}
}

type ingestionCursorRepo struct {
	store persistence.Store
}

// Position reads the singleton row. The migration seeds row 1 with the empty
// position, so an absent row is not "never applied anything" — that state is
// the seeded empty string itself — but a schema this process did not migrate,
// and it comes back as the error it is rather than as a position.
func (r *ingestionCursorRepo) Position(ctx context.Context) (string, error) {
	var position string
	err := r.store.Querier(ctx).QueryRowContext(ctx,
		`SELECT position FROM control.ingestion_cursor WHERE id = 1`).Scan(&position)
	if err != nil {
		return "", fmt.Errorf("postgres: read the usage fact position: %w", err)
	}
	return position, nil
}

// Advance writes next over the singleton's position, inside the caller's unit
// of work. The position's grammar is the Data Plane's and stays unexamined
// here — verbatim in, verbatim out — down to its size, which the column's own
// CHECK (control migration 000008) is the bound of; a page whose position
// outgrows the column fails the way any failed page does, rolled back whole
// and retried, never advanced in part.
func (r *ingestionCursorRepo) Advance(ctx context.Context, next string) error {
	if !r.store.InUnitOfWork(ctx) {
		return fmt.Errorf("postgres: advance the usage fact position: %w", errCursorAdvanceOutsideUnitOfWork)
	}
	result, err := r.store.Querier(ctx).ExecContext(ctx,
		`UPDATE control.ingestion_cursor SET position = $1, updated_at = now() WHERE id = 1`, next)
	if err != nil {
		return fmt.Errorf("postgres: advance the usage fact position: %w", err)
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		// Unreachable against the migrated schema — the singleton is seeded by
		// the same migration that creates it — but an advance that named no
		// row must never read as an advance that happened.
		return fmt.Errorf("postgres: advance the usage fact position: no singleton row to advance")
	}
	return nil
}
