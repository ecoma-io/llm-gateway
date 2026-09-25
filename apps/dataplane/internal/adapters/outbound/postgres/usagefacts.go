package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/usagefacts"
)

// readTimeout bounds one fact-feed read against the runtime's own database.
// The feed is the one read on this process that a consumer elsewhere waits
// on, and a page is small (bounded by the port's MaxLimit); five seconds is
// the patience a local database owes a bounded query with a long margin, and
// it is what turns a wedged storage into a retryable failure instead of a
// hung management request. It is a per-read bound, not a retry budget.
const readTimeout = 5 * time.Second

// UsageFacts is the production fact reader: the port's replay semantics over
// the usage_events table the runtime storage schema creates.
//
// It holds the raw pool deliberately, not the persistence port's Store. The
// reader's contract is side-effect free and read-only; a Store would offer it
// WithinTx and a Querier that resolves to a transaction when a context
// carries one — machinery a read must never use, and machinery that, on a
// one-connection pool, would let a stray context wait on its own query
// forever. The raw pool cannot join a transaction by construction, which is
// the guarantee the reader's side-effect freedom actually needs; nothing else
// in this package receives it.
type UsageFacts struct {
	db *sql.DB
}

// Compile-time proof that the reader satisfies the port the management
// surface hands to the application.
var _ usagefacts.Reader = (*UsageFacts)(nil)

// NewUsageFacts builds the reader over an open pool. It panics on nil for the
// same reason postgres.New does: the failure a nil pool produces later — a
// nil dereference mid-request — is strictly worse than a loud one where the
// wiring went wrong.
func NewUsageFacts(db *sql.DB) *UsageFacts {
	if db == nil {
		panic("postgres: NewUsageFacts requires a non-nil *sql.DB — open the pool before building the reader")
	}
	return &UsageFacts{db: db}
}

// pageQuery selects exactly the columns the port's Event carries, no more.
// The settlement figures — capture method, tokens, prices, amount — stay in
// the row: they are the payload's provenance and the Control Plane derives
// from the payload, not from columns this port never promised to carry. The
// order by is the table's append sequence, the one monotonic order the store
// allocates at commit; ordering by occurred_at would be trusting clocks that
// disagree, and the port exists so nobody has to.
const pageQuery = `SELECT append_seq, request_id::text, kind, schema_version, occurred_at, payload
FROM public.usage_events
WHERE append_seq > $1
ORDER BY append_seq ASC
LIMIT $2`

// streamQuery reads the feed's identity row: the epoch cursors are minted
// under, and the newest sequence allocated so far. The row is created by the
// first append and never by a migration, so its absence means no fact has
// ever been appended — an empty feed, not a broken one.
const streamQuery = `SELECT epoch::text, last_seq FROM public.usage_events_stream WHERE singleton`

// Read implements usagefacts.Reader: the facts strictly after `after`, in
// append order, with the position to resume from.
//
// The cursor is decoded in full before any SQL runs — a caller's string is
// never a query parameter here, it is a position this code either places or
// refuses — and every refusal is the port's ErrCursorExpired, which the
// management surface maps to 410. Query-phase failures are the port's
// ErrSourceUnavailable, which maps to the retriable internal failure;
// scan-phase failures are plain errors, fail closed: a row this build cannot
// decode is a schema drift or a corrupted row, and serving a page that skips
// it would be silently losing a fact — the one failure this whole design
// exists to prevent.
func (reader *UsageFacts) Read(ctx context.Context, after string, limit int) (usagefacts.Page, error) {
	pos, err := decodeCursor(after)
	if err != nil {
		return usagefacts.Page{}, err
	}

	readCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	var epoch string
	var lastSeq int64
	err = reader.db.QueryRowContext(readCtx, streamQuery).Scan(&epoch, &lastSeq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No stream row: nothing has ever been appended. A genesis position is
		// answered with an empty page; a cursor naming an epoch names a stream
		// this database has never had — or no longer has — and is expired.
		if pos.epoch != "" {
			return usagefacts.Page{}, expiredf("cursor names epoch %q, but this stream has never appended a fact", pos.epoch)
		}
		return usagefacts.Page{NextCursor: genesisCursor}, nil
	case err != nil:
		return usagefacts.Page{}, fmt.Errorf("%w: read the fact stream identity: %w", usagefacts.ErrSourceUnavailable, err)
	}

	// The epoch is the stream's identity, checked before any page is served:
	// a cursor minted by a previous stream — a database that lost its rows —
	// must fail closed here, or the consumer would skip re-appended facts it
	// believes it already delivered.
	if pos.epoch != "" && pos.epoch != epoch {
		return usagefacts.Page{}, expiredf("cursor names epoch %q, but this stream is %q", pos.epoch, epoch)
	}

	rows, err := reader.db.QueryContext(readCtx, pageQuery, pos.seq, limit+1)
	if err != nil {
		return usagefacts.Page{}, fmt.Errorf("%w: query the fact page: %w", usagefacts.ErrSourceUnavailable, err)
	}
	defer rows.Close()

	page := usagefacts.Page{Events: make([]usagefacts.Event, 0, limit)}
	var lastSeen int64
	for rows.Next() {
		if len(page.Events) == limit {
			page.HasMore = true
			break
		}
		event, seq, scanErr := scanEvent(rows)
		if scanErr != nil {
			// Fail closed: refuse the whole page. The consumer keeps its
			// position and retries; a page that skipped the undecodable row
			// would advance the consumer past a fact it never saw.
			return usagefacts.Page{}, fmt.Errorf("decode fact at append_seq %d: %w", lastSeen+1, scanErr)
		}
		page.Events = append(page.Events, event)
		lastSeen = seq
	}
	if err := rows.Err(); err != nil {
		return usagefacts.Page{}, fmt.Errorf("%w: read the fact page: %w", usagefacts.ErrSourceUnavailable, err)
	}

	if len(page.Events) > 0 {
		page.NextCursor = encodeCursor(epoch, lastSeen)
	} else {
		// No events — the consumer is caught up, or the feed is empty. The
		// position to resume from is the one it asked about, restated in the
		// canonical spelling: non-empty even when nothing was delivered, so a
		// consumer that applied nothing still has a defined position.
		page.NextCursor = encodeCursor(epoch, pos.seq)
	}
	return page, nil
}

// scanEvent decodes one row into the port's Event. The payload is copied out
// of the scan target explicitly: the port's contract is that the bytes the
// consumer reads are the bytes that were stored, and an event that aliased a
// reused buffer would satisfy every test and corrupt the first consumer that
// held two pages at once.
func scanEvent(rows *sql.Rows) (usagefacts.Event, int64, error) {
	var (
		event   usagefacts.Event
		seq     int64
		payload []byte
	)
	if err := rows.Scan(&seq, &event.RequestID, &event.Kind, &event.SchemaVersion, &event.OccurredAt, &payload); err != nil {
		return usagefacts.Event{}, 0, err
	}
	if !json.Valid(payload) {
		return usagefacts.Event{}, 0, fmt.Errorf("payload is not valid json")
	}
	event.Payload = append(json.RawMessage(nil), payload...)
	return event, seq, nil
}
