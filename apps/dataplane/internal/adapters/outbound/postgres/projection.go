package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The projection applier: the consumer side of the Control → Data projection
// (ADR 0007), written against the mirror schema migration
// 000004_projection_foundation.up.sql defines. Three tables carry it —
// api_key_credentials and account_states are the mirror rows, projection_state
// is the position — and every rule this file implements is one of the
// protocol's five, restated here in SQL:
//
//   - Rows and position commit together. Both applies run inside one
//     WithinTx, so a crash before the commit replays and a crash after it
//     re-acknowledges; there is no state in which rows landed and the
//     position does not say so.
//   - A snapshot is unconditional and whole. It consults no per-row history
//     — it is the operation that exists to overwrite — and it makes the
//     mirror exactly the delivered arrays: every named row is upserted and
//     every row the snapshot does not name is deleted, in the same
//     transaction that assigns the position to the snapshot boundary.
//     Backward moves included, because recovery must be able to reach a
//     producer timeline that rewound. Half a snapshot is a lie the mirror
//     would serve on its hot path: a key the Control Plane no longer
//     projects must not survive here by lingering, any more than a key the
//     mirror never saw may serve.
//   - A batch is judged whole before anything is written. The position row is
//     locked FOR UPDATE first, so two deliveries cannot both pass the
//     judgement and advance the position twice; then the batch's revision
//     range is classified against the stored position, and only a batch that
//     joins it exactly writes anything.
//   - A row is never regressed. The incremental upsert carries
//     `WHERE source_revision < EXCLUDED.source_revision`: an entry the mirror
//     has already superseded — the duplicate delivery the protocol calls
//     at-least-once — updates nothing. The snapshot path takes no such guard
//     on purpose: it is the operation that exists to overwrite.
//   - A terminal state is never left. A batch entry that would move a revoked
//     credential or a closed account back into a live state is refused whole
//     (ErrTerminalRegression): a conforming producer cannot express it, so
//     one that arrives is a defect the mirror must not honour on its auth
//     path. The snapshot stays exempt — it is the authority's word entire —
//     and the guard reads the row's current state inside the apply
//     transaction, ahead of the upsert, because a silent skip under an
//     advanced position would be a divergence nobody is told about.
var _ persistence.ProjectionApplier = (*store)(nil)

// NewProjectionApplier builds the projection applier over the runtime's pool.
// It is a second constructor over the same *sql.DB as New, not a second
// object: the store and the applier are one transaction discipline, and the
// split exists so the application receives the port it depends on as that
// port — a use case that needs the applier must not also be holding the
// query surface, and the composition root hands out exactly what each
// dependency names.
func NewProjectionApplier(db *sql.DB) persistence.ProjectionApplier {
	if db == nil {
		panic("postgres: NewProjectionApplier requires a *sql.DB; a projection applier over nothing has nowhere to write and no transaction to write in")
	}
	return &store{db: db}
}

const projectionPositionQuery = `
SELECT bootstrapped, applied_revision, producer_epoch
FROM projection_state
WHERE id = 1`

const snapshotAPIKeyUpsert = `
INSERT INTO api_key_credentials (key_id, account_id, digest, state, revoked_at,
                                 source_revision, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (key_id) DO UPDATE SET
    account_id = EXCLUDED.account_id,
    digest = EXCLUDED.digest,
    state = EXCLUDED.state,
    revoked_at = EXCLUDED.revoked_at,
    source_revision = EXCLUDED.source_revision,
    updated_at = EXCLUDED.updated_at`

const snapshotAccountUpsert = `
INSERT INTO account_states (account_id, state, source_revision, updated_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (account_id) DO UPDATE SET
    state = EXCLUDED.state,
    source_revision = EXCLUDED.source_revision,
    updated_at = EXCLUDED.updated_at`

// The snapshot's absence deletes. Whole replacement is the two halves of one
// statement pair: upsert what the snapshot names, delete what it does not.
// The comparison is on the text form of the id because the delivered ids are
// grammar-validated strings — canonical UUID text, lowercase — so the array
// crosses as the text[] the driver encodes natively, and the column's text
// form is that same canonical spelling. A full scan of the mirror is the
// cost, and it is the right one: snapshot applies are the recovery path,
// run once per bootstrap or epoch change, not per cycle.
const snapshotKeyAbsenceDelete = `
DELETE FROM api_key_credentials
WHERE NOT (key_id::text = ANY($1))`

const snapshotAccountAbsenceDelete = `
DELETE FROM account_states
WHERE NOT (account_id::text = ANY($1))`

const changeAPIKeyUpsert = `
INSERT INTO api_key_credentials (key_id, account_id, digest, state, revoked_at,
                                 source_revision, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (key_id) DO UPDATE SET
    account_id = EXCLUDED.account_id,
    digest = EXCLUDED.digest,
    state = EXCLUDED.state,
    revoked_at = EXCLUDED.revoked_at,
    source_revision = EXCLUDED.source_revision,
    updated_at = EXCLUDED.updated_at
WHERE api_key_credentials.source_revision < EXCLUDED.source_revision`

const changeAccountUpsert = `
INSERT INTO account_states (account_id, state, source_revision, updated_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (account_id) DO UPDATE SET
    state = EXCLUDED.state,
    source_revision = EXCLUDED.source_revision,
    updated_at = EXCLUDED.updated_at
WHERE account_states.source_revision < EXCLUDED.source_revision`

// The two reads below fetch a mirror row's current lifecycle inside the
// apply transaction, ahead of its upsert. They are the terminal-regression
// guard's eyes: a batch entry that would move a revoked credential or a
// closed account back into a live state is refused whole (the domain's
// ErrTerminalRegression) instead of being written. The revision guard above
// cannot catch this — the arriving revision is always strictly higher than
// the stored one inside a joining batch — and the upsert must not grow a
// silent skip, because a skipped write under an advanced position is a
// divergence nobody is told about. Reads and writes share the transaction's
// snapshot and row locks, so a state read here is the state the upsert
// conflicts against.
const mirrorCredentialState = `
SELECT state FROM api_key_credentials WHERE key_id = $1`

const mirrorAccountState = `
SELECT state FROM account_states WHERE account_id = $1`

// Position reads this plane's own fact out of projection_state. The singleton
// is seeded by its migration, so "no row" is a migrated-away database rather
// than a state to default — it is reported as the error it is.
func (s *store) Position(ctx context.Context) (projection.Position, error) {
	position, err := readPosition(ctx, s.Querier(ctx), false)
	if err != nil {
		return projection.Position{}, fmt.Errorf("postgres: read projection position: %w", err)
	}
	return position, nil
}

// ApplySnapshot makes the mirror exactly the delivered projection and the
// position exactly the snapshot's boundary, in one transaction. Every
// delivered row takes the boundary as its source_revision — after a
// snapshot, an incremental entry from the same timeline carries a strictly
// higher revision, which is exactly what makes the incremental guard skip
// entries the snapshot already covered.
//
// The position row is locked FOR UPDATE first, for the same reason
// ApplyChanges locks it: a batch delivery judged against the pre-snapshot
// position must not interleave its writes with the snapshot's replacement.
// The lock orders the two operations into one of two deterministic
// outcomes — the batch lands on the old position and the snapshot then
// replaces its world, or the snapshot lands and the batch is judged against
// the snapshot's — never a mixture.
func (s *store) ApplySnapshot(ctx context.Context, snapshot projection.Snapshot, appliedAt time.Time) (uint64, error) {
	applied := uint64(0)
	err := s.WithinTx(ctx, func(ctx context.Context) error {
		q := s.Querier(ctx)
		if _, err := readPosition(ctx, q, true); err != nil {
			return fmt.Errorf("postgres: read projection position: %w", err)
		}
		for _, record := range snapshot.APIKeys {
			if _, err := q.ExecContext(ctx, snapshotAPIKeyUpsert,
				record.KeyID, record.AccountID, record.Digest,
				string(record.State), nullTime(record.RevokedAt),
				int64(snapshot.SnapshotRevision), appliedAt); err != nil {
				return fmt.Errorf("postgres: apply snapshot credential %s: %w", record.KeyID, err)
			}
		}
		for _, record := range snapshot.Accounts {
			if _, err := q.ExecContext(ctx, snapshotAccountUpsert,
				record.AccountID, string(record.State),
				int64(snapshot.SnapshotRevision), appliedAt); err != nil {
				return fmt.Errorf("postgres: apply snapshot account %s: %w", record.AccountID, err)
			}
		}
		// The absence deletes run after the upserts and inside the same
		// transaction: what survives is exactly the delivered arrays, whatever
		// the mirror held before — including rows a previous timeline left
		// behind.
		keyIDs := make([]string, len(snapshot.APIKeys))
		for i, record := range snapshot.APIKeys {
			keyIDs[i] = record.KeyID
		}
		if _, err := q.ExecContext(ctx, snapshotKeyAbsenceDelete, keyIDs); err != nil {
			return fmt.Errorf("postgres: delete credentials absent from the snapshot: %w", err)
		}
		accountIDs := make([]string, len(snapshot.Accounts))
		for i, record := range snapshot.Accounts {
			accountIDs[i] = record.AccountID
		}
		if _, err := q.ExecContext(ctx, snapshotAccountAbsenceDelete, accountIDs); err != nil {
			return fmt.Errorf("postgres: delete accounts absent from the snapshot: %w", err)
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE projection_state
			SET bootstrapped = true, applied_revision = $1, producer_epoch = $2
			WHERE id = 1`,
			int64(snapshot.SnapshotRevision), snapshot.Epoch); err != nil {
			return fmt.Errorf("postgres: advance position to the snapshot boundary: %w", err)
		}
		applied = snapshot.SnapshotRevision
		return nil
	})
	if err != nil {
		return 0, err
	}
	return applied, nil
}

// ApplyChanges judges a whole batch against the stored position and applies it
// only when the judgement says "apply". The answer — the position after the
// operation — is the acknowledgement: returned for a batch that applied, and
// returned unchanged for the duplicate that was already applied, so a producer
// that lost its earlier acknowledgement learns where the consumer stands
// either way.
func (s *store) ApplyChanges(ctx context.Context, batch projection.Batch) (uint64, error) {
	if len(batch.Changes) == 0 {
		return 0, fmt.Errorf("postgres: apply changes: %w: a batch with no changes cannot be judged", projection.ErrBatchShape)
	}

	applied := uint64(0)
	err := s.WithinTx(ctx, func(ctx context.Context) error {
		q := s.Querier(ctx)
		position, err := readPosition(ctx, q, true)
		if err != nil {
			return fmt.Errorf("postgres: read projection position: %w", err)
		}
		if !position.Bootstrapped || position.Epoch != batch.Epoch {
			// Not "stale" but "unjudgeable": the position this plane holds was
			// earned on a timeline this batch does not belong to (or was never
			// earned at all). No comparison of revisions means anything across
			// that line, so the only honest answer is a snapshot.
			return fmt.Errorf("postgres: %w: batch names epoch %s, the stored position is bootstrapped %t on epoch %q",
				projection.ErrSnapshotRequired, batch.Epoch, position.Bootstrapped, position.Epoch)
		}

		switch projection.DecideBatch(position.AppliedRevision, batch) {
		case projection.DecisionDuplicate:
			// Every entry is already applied; this is the lost acknowledgement
			// arriving. Nothing is written and the current position is the
			// acknowledgement.
			applied = position.AppliedRevision
			return nil
		case projection.DecisionGap:
			// Refused whole. Applying the entries that fit would advance the
			// position past revisions that may still arrive, and the stranded
			// remainder would then be duplicates forever.
			return fmt.Errorf("postgres: %w: batch [%d..%d] cannot join applied revision %d",
				projection.ErrRevisionGap,
				batch.Changes[0].Revision, batch.Changes[len(batch.Changes)-1].Revision, position.AppliedRevision)
		}

		for _, change := range batch.Changes {
			if err := applyChange(ctx, q, change); err != nil {
				return err
			}
		}
		last := batch.Changes[len(batch.Changes)-1].Revision
		if _, err := q.ExecContext(ctx, `
			UPDATE projection_state
			SET applied_revision = $1
			WHERE id = 1`,
			int64(last)); err != nil {
			return fmt.Errorf("postgres: advance position to revision %d: %w", last, err)
		}
		applied = last
		return nil
	})
	if err != nil {
		return 0, err
	}
	return applied, nil
}

// applyChange writes one entry under the per-row revision guard. The guard is
// the idempotency: the same entry delivered twice writes once, and an entry
// the mirror has already superseded is a no-op rather than a regression. The
// terminal-regression read ahead of the write is the other half: a joining
// batch always carries a strictly higher revision, so the revision guard
// alone would let a later entry overwrite a revoked credential with a live
// one, and that refusal belongs to the domain, not to a constraint the
// database discovers mid-transaction.
func applyChange(ctx context.Context, q persistence.Querier, change projection.Change) error {
	switch change.Kind {
	case projection.KindAPIKey:
		record := change.Credential
		if err := refuseTerminalCredentialRegression(ctx, q, record.KeyID, record.State); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, changeAPIKeyUpsert,
			record.KeyID, record.AccountID, record.Digest,
			string(record.State), nullTime(record.RevokedAt),
			int64(change.Revision), change.RecordedAt); err != nil {
			return fmt.Errorf("postgres: apply credential change %d for %s: %w", change.Revision, change.ResourceID, err)
		}
		return nil
	case projection.KindAccount:
		record := change.Account
		if err := refuseTerminalAccountRegression(ctx, q, record.AccountID, record.State); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, changeAccountUpsert,
			record.AccountID, string(record.State),
			int64(change.Revision), change.RecordedAt); err != nil {
			return fmt.Errorf("postgres: apply account change %d for %s: %w", change.Revision, change.ResourceID, err)
		}
		return nil
	default:
		// Unreachable for a validated batch; a kind the mirror does not hold
		// is refused by the grammar before a transaction opens.
		return fmt.Errorf("postgres: apply change %d: %w: %s is not a kind this mirror holds", change.Revision, projection.ErrBatchShape, change.Kind)
	}
}

// refuseTerminalCredentialRegression answers ErrTerminalRegression when the
// mirror already holds the credential revoked and the delivered state is
// anything else. A row that is not there yet has no state to regress.
func refuseTerminalCredentialRegression(ctx context.Context, q persistence.Querier, keyID string, delivered projection.CredentialState) error {
	var state string
	err := q.QueryRowContext(ctx, mirrorCredentialState, keyID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("postgres: read credential state for %s: %w", keyID, err)
	}
	if projection.CredentialRegressesTerminal(projection.CredentialState(state), delivered) {
		return fmt.Errorf("postgres: %w: credential %s stands revoked at the applied position and the batch delivers %q",
			projection.ErrTerminalRegression, keyID, delivered)
	}
	return nil
}

// refuseTerminalAccountRegression is the account half: a closed account is
// not reopened by a batch entry, whatever revision it names.
func refuseTerminalAccountRegression(ctx context.Context, q persistence.Querier, accountID string, delivered projection.AccountLifecycle) error {
	var state string
	err := q.QueryRowContext(ctx, mirrorAccountState, accountID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("postgres: read account state for %s: %w", accountID, err)
	}
	if projection.AccountRegressesTerminal(projection.AccountLifecycle(state), delivered) {
		return fmt.Errorf("postgres: %w: account %s stands closed at the applied position and the batch delivers %q",
			projection.ErrTerminalRegression, accountID, delivered)
	}
	return nil
}

// readPosition reads the singleton, optionally locking it. The lock is what
// serializes two concurrent deliveries: the first to take it judges, writes
// and commits before the second reads, so the second judges against the
// position the first left behind rather than the one it found.
func readPosition(ctx context.Context, q persistence.Querier, lock bool) (projection.Position, error) {
	query := projectionPositionQuery
	if lock {
		query += " FOR UPDATE"
	}
	var (
		position projection.Position
		epoch    sql.NullString
		revision int64
	)
	row := q.QueryRowContext(ctx, query)
	if err := row.Scan(&position.Bootstrapped, &revision, &epoch); err != nil {
		return projection.Position{}, err
	}
	position.AppliedRevision = uint64(revision)
	position.Epoch = epoch.String
	return position, nil
}

// nullTime maps the domain's optional instant onto the column's NULL.
func nullTime(at *time.Time) any {
	if at == nil {
		return nil
	}
	return *at
}
