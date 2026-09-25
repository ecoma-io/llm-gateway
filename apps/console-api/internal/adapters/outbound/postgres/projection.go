package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The projection repositories: translation between the persistence port's
// ProjectionLog and the `control` database's projection tables (ADR 0007).
// Three tables and one counter, written under two disciplines the identity
// repositories above already established and one that is new here:
//
//   - Every write resolves its handle through the store — the caller's unit
//     of work when the context carries one, the pool otherwise — so a record
//     inside a WithinTx scope commits or rolls back with the authority write
//     beside it. That resolution is the atomicity story, and the recorder
//     enforces its premise rather than trusting it: a context with no unit
//     of work is refused outright, because on the pool the three statements
//     would autocommit apart and the guarantee would be gone.
//   - The revision is allocated by the store, never accepted from a caller:
//     `UPDATE control.projection_revision ... RETURNING` inside the writer's
//     own transaction takes the counter's row lock and holds it to commit,
//     which serializes the management-paced writers and makes allocation
//     order equal commit order — the fact the projection's snapshot
//     consistency and the consumer's gap detection both rest on. A caller
//     passing a revision in could only be guessing it.
//   - Every read that states a boundary reads the boundary and the rows in
//     one unit of work, and filters the rows to it: a concurrent writer can
//     commit between the two statements of the snapshot cut, and a row whose
//     source_revision is past the boundary read belongs to the batches after
//     the snapshot, not to the snapshot.
//
// This file is the one place in the Control Plane's adapter tree that a
// secret's DIGEST is stored (ADR 0006 §8, as amended by ADR 0007): the
// payload copy and the materialized column both carry the lowercase hex
// SHA-256 of a key's secret, each pinned by its own CHECK to the same 64-hex
// form. The plaintext has no column, no parameter and no variable here — the
// domain type this file reads, projection.Credential, cannot hold one.

// NewProjectionLog returns the persistence port's projection repositories
// backed by store. It panics on a nil store for the same reason the identity
// constructors do: the failure a nil dependency produces later is strictly
// worse than a loud one here.
func NewProjectionLog(store persistence.Store) persistence.ProjectionLog {
	if store == nil {
		panic("postgres: NewProjectionLog requires a non-nil persistence.Store")
	}
	return &projectionRepo{store: store}
}

// NewProjectionChangeRecorder returns the write half of the projection port,
// backed by the same store and the same statements as ProjectionLog's write
// methods. The identity use cases take this half; the delivery loop takes the
// read half; the split keeps each caller's dependencies saying what it does.
func NewProjectionChangeRecorder(store persistence.Store) persistence.ProjectionChangeRecorder {
	if store == nil {
		panic("postgres: NewProjectionChangeRecorder requires a non-nil persistence.Store")
	}
	return &projectionRepo{store: store}
}

// Compile-time proof that the repository satisfies both halves of the port it
// claims to.
var (
	_ persistence.ProjectionLog            = (*projectionRepo)(nil)
	_ persistence.ProjectionChangeRecorder = (*projectionRepo)(nil)
)

type projectionRepo struct {
	store persistence.Store
}

// allocateRevision is the counter's only mutation: one row, one increment,
// the new value returned into the writer's transaction. The row lock this
// statement takes is held until the transaction commits, so the next writer's
// identical statement blocks — allocation order is commit order, and an
// aborted transaction releases both its number and its lock, which is what
// keeps the counter gapless rather than merely increasing.
const allocateRevision = `
UPDATE control.projection_revision
SET last_revision = last_revision + 1
WHERE id = 1
RETURNING last_revision`

// headRevision reads the timeline's identity and its highest allocated
// revision. The row must exist: it is created by the projection foundation's
// migration, and its absence is a database that was never migrated — a
// failure to report, never a zero to invent.
const headRevision = `
SELECT epoch, last_revision
FROM control.projection_revision
WHERE id = 1`

// snapshotCredentials cuts the credential projection at a boundary. The
// filter is the cut: rows are written in the same transaction as the log
// entries they correspond to, so everything at or before the boundary
// read is the state at that boundary, and anything a concurrent writer
// commits with a later source_revision is excluded by the comparison
// rather than trusted to be invisible.
const snapshotCredentials = `
SELECT key_id, account_id, digest, state, revoked_at
FROM control.projection_api_keys
WHERE source_revision <= $1
ORDER BY key_id`

// snapshotAccounts cuts the account projection at the same boundary, with
// the same filter and the same reasoning.
const snapshotAccounts = `
SELECT account_id, state
FROM control.projection_accounts
WHERE source_revision <= $1
ORDER BY account_id`

// credentialByKeyID is the mirror's point read — one row by its key id, the
// digest the revoke path needs to describe the credential it is killing. The
// row's state says nothing about whether the read is for a revoke; the
// caller decides what the returned state is worth.
const credentialByKeyID = `
SELECT key_id, account_id, digest, state, revoked_at
FROM control.projection_api_keys
WHERE key_id = $1`

// changesAfter reads the durable log in its own order. The limit is the
// delivery batch's bound, applied here so a drained-then-refilled log can
// never hand the producer a batch the contract refuses.
const changesAfter = `
SELECT revision, resource_kind, resource_id, recorded_at, payload
FROM control.projection_changes
WHERE revision > $1
ORDER BY revision
LIMIT $2`

// insertCredentialChange appends one api_key entry. The payload is the
// change's full state — account, digest, state, revocation instant — built
// once by the domain type and stored verbatim, so the copy this database
// holds and the copy a redelivery carries are the same bytes.
const insertCredentialChange = `
INSERT INTO control.projection_changes (revision, resource_kind, resource_id, recorded_at, payload)
VALUES ($1, 'api_key', $2, $3, $4)`

// upsertCredentialMirror writes the materialized projection — the snapshot
// source. The conflict arm is the whole point of an upsert rather than an
// insert-or-update pair: a key's second entry (its revocation, usually)
// replaces the row in the same transaction as the log append, so the mirror
// is never ahead of, behind, or apart from the log.
const upsertCredentialMirror = `
INSERT INTO control.projection_api_keys (key_id, account_id, digest, state, revoked_at, source_revision, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (key_id) DO UPDATE SET
    account_id = EXCLUDED.account_id,
    digest = EXCLUDED.digest,
    state = EXCLUDED.state,
    revoked_at = EXCLUDED.revoked_at,
    source_revision = EXCLUDED.source_revision,
    updated_at = EXCLUDED.updated_at`

// insertAccountChange and upsertAccountMirror are the account half of the
// pair above, one statement each, same transaction, same reasoning.
const insertAccountChange = `
INSERT INTO control.projection_changes (revision, resource_kind, resource_id, recorded_at, payload)
VALUES ($1, 'account', $2, $3, $4)`

const upsertAccountMirror = `
INSERT INTO control.projection_accounts (account_id, state, source_revision, updated_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (account_id) DO UPDATE SET
    state = EXCLUDED.state,
    source_revision = EXCLUDED.source_revision,
    updated_at = EXCLUDED.updated_at`

// RecordCredentialChange allocates the entry's revision and writes the log
// entry and the mirror row through the caller's unit of work. The unit of
// work is a precondition, not a convention: outside one the three statements
// would each autocommit on the pool — a concurrent writer could commit
// between this writer's allocation and its append, breaking allocation order
// = commit order, and a mirror failure would leave a committed log entry
// with no mirror row behind it — so a context with no transaction is
// refused, not degraded.
func (r *projectionRepo) RecordCredentialChange(ctx context.Context, at time.Time, credential projection.Credential) error {
	if !r.store.InUnitOfWork(ctx) {
		return fmt.Errorf("postgres: record credential change %s: no unit of work in the context; the projection log is only written inside the caller's transaction", credential.KeyID)
	}
	q := r.store.Querier(ctx)

	payload, err := credential.PayloadJSON()
	if err != nil {
		return fmt.Errorf("postgres: record credential change %s: %w", credential.KeyID, err)
	}

	var revision uint64
	if err := q.QueryRowContext(ctx, allocateRevision).Scan(&revision); err != nil {
		return fmt.Errorf("postgres: record credential change %s: allocate revision: %w", credential.KeyID, err)
	}

	if _, err := q.ExecContext(ctx, insertCredentialChange,
		revision, credential.KeyID, at, []byte(payload)); err != nil {
		return fmt.Errorf("postgres: record credential change %s at revision %d: %w", credential.KeyID, revision, err)
	}

	// revoked_at is NULL when the domain says the key is active — the zero
	// pointer is that absence, and an empty timestamp is not.
	var revokedAt any
	if credential.RevokedAt != nil {
		revokedAt = *credential.RevokedAt
	}
	if _, err := q.ExecContext(ctx, upsertCredentialMirror,
		credential.KeyID, credential.AccountID, string(credential.Digest), string(credential.State), revokedAt, revision, at); err != nil {
		return fmt.Errorf("postgres: record credential change %s at revision %d: mirror: %w", credential.KeyID, revision, err)
	}
	return nil
}

// RecordAccountChange is the account half, with the same allocation, the
// same two writes, and the same refusal of a context with no unit of work.
func (r *projectionRepo) RecordAccountChange(ctx context.Context, at time.Time, account projection.Account) error {
	if !r.store.InUnitOfWork(ctx) {
		return fmt.Errorf("postgres: record account change %s: no unit of work in the context; the projection log is only written inside the caller's transaction", account.AccountID)
	}
	q := r.store.Querier(ctx)

	payload, err := account.PayloadJSON()
	if err != nil {
		return fmt.Errorf("postgres: record account change %s: %w", account.AccountID, err)
	}

	var revision uint64
	if err := q.QueryRowContext(ctx, allocateRevision).Scan(&revision); err != nil {
		return fmt.Errorf("postgres: record account change %s: allocate revision: %w", account.AccountID, err)
	}

	if _, err := q.ExecContext(ctx, insertAccountChange,
		revision, account.AccountID, at, []byte(payload)); err != nil {
		return fmt.Errorf("postgres: record account change %s at revision %d: %w", account.AccountID, revision, err)
	}

	if _, err := q.ExecContext(ctx, upsertAccountMirror,
		account.AccountID, string(account.State), revision, at); err != nil {
		return fmt.Errorf("postgres: record account change %s at revision %d: mirror: %w", account.AccountID, revision, err)
	}
	return nil
}

// Head reads the counter: the timeline's name and its head revision, both
// validated by the domain constructor, because a corrupted row should fail
// this process loudly here rather than become a delivered message.
func (r *projectionRepo) Head(ctx context.Context) (projection.Head, error) {
	var epoch string
	var lastRevision uint64
	err := r.store.Querier(ctx).QueryRowContext(ctx, headRevision).Scan(&epoch, &lastRevision)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return projection.Head{}, fmt.Errorf("postgres: projection head: the revision counter's row does not exist; run the projection foundation migration")
		}
		return projection.Head{}, fmt.Errorf("postgres: projection head: %w", err)
	}
	head, err := projection.NewHead(epoch, lastRevision)
	if err != nil {
		return projection.Head{}, fmt.Errorf("postgres: projection head: %w", err)
	}
	return head, nil
}

// Snapshot cuts the projection at one boundary: the head read and both
// mirror reads run inside one unit of work, and the rows are filtered to the
// boundary read, so the result is the state at that revision regardless of
// who commits while the cut is being read.
func (r *projectionRepo) Snapshot(ctx context.Context) (projection.Snapshot, error) {
	var snapshot projection.Snapshot
	err := r.store.WithinTx(ctx, func(txCtx context.Context) error {
		head, err := r.Head(txCtx)
		if err != nil {
			return err
		}

		q := r.store.Querier(txCtx)
		keys, err := readSnapshotCredentials(txCtx, q, head.LastRevision)
		if err != nil {
			return err
		}
		accounts, err := readSnapshotAccounts(txCtx, q, head.LastRevision)
		if err != nil {
			return err
		}

		cut, err := projection.NewSnapshot(head.Epoch, head.LastRevision, keys, accounts)
		if err != nil {
			return fmt.Errorf("postgres: projection snapshot: %w", err)
		}
		snapshot = cut
		return nil
	})
	if err != nil {
		return projection.Snapshot{}, err
	}
	return snapshot, nil
}

// Credential reads the credential projection's row for keyID, decoded through
// the domain constructor like every other row this file lets out. ErrNotFound
// is returned as itself — the miss is the caller's decision, not this
// method's: keys minted before the projection foundation have no mirror row,
// and the revoke path treats that as "nothing to project", not as a failure.
func (r *projectionRepo) Credential(ctx context.Context, keyID string) (projection.Credential, error) {
	var rowKeyID, accountID, digest, state string
	var revokedAt sql.NullTime
	err := r.store.Querier(ctx).QueryRowContext(ctx, credentialByKeyID, keyID).
		Scan(&rowKeyID, &accountID, &digest, &state, &revokedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return projection.Credential{}, fmt.Errorf("postgres: projection credential %s: %w", keyID, persistence.ErrNotFound)
		}
		return projection.Credential{}, fmt.Errorf("postgres: projection credential %s: %w", keyID, err)
	}
	var revoked *time.Time
	if revokedAt.Valid {
		t := revokedAt.Time
		revoked = &t
	}
	credential, err := projection.NewCredential(rowKeyID, accountID, digest, projection.CredentialState(state), revoked)
	if err != nil {
		return projection.Credential{}, fmt.Errorf("postgres: projection credential %s: %w", keyID, err)
	}
	return credential, nil
}

// readSnapshotCredentials reads one cut's credential rows and decodes each
// through the domain constructor. A mirror row outside the grammar is a
// database that has stopped meaning what it stores, and the cut is refused
// whole: delivering half a snapshot would bootstrap a consumer into a
// projection nobody can vouch for.
func readSnapshotCredentials(ctx context.Context, q persistence.Querier, boundary uint64) ([]projection.Credential, error) {
	rows, err := q.QueryContext(ctx, snapshotCredentials, boundary)
	if err != nil {
		return nil, fmt.Errorf("postgres: projection snapshot: read credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()

	keys := []projection.Credential{}
	for rows.Next() {
		var keyID, accountID, digest, state string
		var revokedAt sql.NullTime
		if err := rows.Scan(&keyID, &accountID, &digest, &state, &revokedAt); err != nil {
			return nil, fmt.Errorf("postgres: projection snapshot: scan credential: %w", err)
		}
		var revoked *time.Time
		if revokedAt.Valid {
			t := revokedAt.Time
			revoked = &t
		}
		credential, err := projection.NewCredential(keyID, accountID, digest, projection.CredentialState(state), revoked)
		if err != nil {
			return nil, fmt.Errorf("postgres: projection snapshot: %w", err)
		}
		keys = append(keys, credential)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: projection snapshot: read credentials: %w", err)
	}
	return keys, nil
}

// readSnapshotAccounts is the account half of the cut, with the same
// whole-or-nothing decoding.
func readSnapshotAccounts(ctx context.Context, q persistence.Querier, boundary uint64) ([]projection.Account, error) {
	rows, err := q.QueryContext(ctx, snapshotAccounts, boundary)
	if err != nil {
		return nil, fmt.Errorf("postgres: projection snapshot: read accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	accounts := []projection.Account{}
	for rows.Next() {
		var accountID, state string
		if err := rows.Scan(&accountID, &state); err != nil {
			return nil, fmt.Errorf("postgres: projection snapshot: scan account: %w", err)
		}
		account, err := projection.NewAccount(accountID, projection.AccountState(state))
		if err != nil {
			return nil, fmt.Errorf("postgres: projection snapshot: %w", err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: projection snapshot: read accounts: %w", err)
	}
	return accounts, nil
}

// ChangesAfter reads at most limit entries strictly after `after`, decoded
// through the domain constructors so every entry that leaves this function
// is one the grammar vouches for. The payload column is re-read, not
// remembered: the log is history, and history that cannot still be decoded
// is not history this plane can deliver.
func (r *projectionRepo) ChangesAfter(ctx context.Context, after uint64, limit int) ([]projection.Change, error) {
	if limit < 1 {
		return nil, fmt.Errorf("postgres: projection changes after %d: limit must be at least 1, got %d", after, limit)
	}
	rows, err := r.store.Querier(ctx).QueryContext(ctx, changesAfter, after, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: projection changes after %d: %w", after, err)
	}
	defer func() { _ = rows.Close() }()

	changes := []projection.Change{}
	for rows.Next() {
		var revision uint64
		var kind, resourceID string
		var recordedAt time.Time
		var payload []byte
		if err := rows.Scan(&revision, &kind, &resourceID, &recordedAt, &payload); err != nil {
			return nil, fmt.Errorf("postgres: projection changes after %d: scan: %w", after, err)
		}
		change, err := decodeChange(revision, projection.ResourceKind(kind), resourceID, recordedAt, payload)
		if err != nil {
			return nil, fmt.Errorf("postgres: projection changes after %d: %w", after, err)
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: projection changes after %d: %w", after, err)
	}
	return changes, nil
}

// decodeChange turns one log row into a domain Change, by kind. An unknown
// kind is refused rather than skipped: the kind set is closed by the
// database's own CHECK, so a row outside it is a corrupted log or a schema
// this build predates — and silently skipping either is how a mirror loses
// a row forever.
func decodeChange(revision uint64, kind projection.ResourceKind, resourceID string, recordedAt time.Time, payload []byte) (projection.Change, error) {
	switch kind {
	case projection.KindAPIKey:
		var p struct {
			AccountID string     `json:"account_id"`
			Digest    string     `json:"digest"`
			State     string     `json:"state"`
			RevokedAt *time.Time `json:"revoked_at"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return projection.Change{}, fmt.Errorf("decode api_key payload at revision %d: %w", revision, err)
		}
		credential, err := projection.NewCredential(resourceID, p.AccountID, p.Digest, projection.CredentialState(p.State), p.RevokedAt)
		if err != nil {
			return projection.Change{}, fmt.Errorf("decode api_key payload at revision %d: %w", revision, err)
		}
		return projection.NewCredentialChange(revision, recordedAt, credential)
	case projection.KindAccount:
		var p struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return projection.Change{}, fmt.Errorf("decode account payload at revision %d: %w", revision, err)
		}
		account, err := projection.NewAccount(resourceID, projection.AccountState(p.State))
		if err != nil {
			return projection.Change{}, fmt.Errorf("decode account payload at revision %d: %w", revision, err)
		}
		return projection.NewAccountChange(revision, recordedAt, account)
	default:
		return projection.Change{}, fmt.Errorf("log row at revision %d carries unknown resource kind %q", revision, kind)
	}
}
