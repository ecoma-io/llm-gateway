//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The projection applier against the real `dataplane` database. The unit suite
// beside this file pins which statements run and how a driver condition
// becomes the port's vocabulary; only PostgreSQL can answer the rest — that
// rows and position commit together or not at all, that a redelivered batch
// writes nothing while still acknowledging, that a gapped or foreign-timeline
// batch is refused whole, and that the per-row revision guard skips what the
// mirror already supersedes. These are the persistence halves of the task's
// matrix (§23.2–23.6); the grammar's halves live with the domain and the
// listener, which no database can speak for.
//
// The tier's discipline is integration_test.go's: an explicit admin DSN, the
// plane database ensured from it, the lane's migrations applied through the
// runner's own library. The projection tables add one need the catalog tests
// do not have: the position is a singleton the whole suite shares. Every test
// therefore resets the mirror to its migration-seeded state before it runs,
// and no test here marks itself parallel — a shared singleton is tractable
// only while the tests that judge it stay sequential.

// integrationProjection ensures the schema and hands back a pool and a
// projection applier over a mirror reset to its seeded state: no rows, no
// bootstrap, revision zero, no epoch.
func integrationProjection(t *testing.T) (*sql.DB, persistence.ProjectionApplier) {
	t.Helper()
	db, _ := integrationPool(t)
	ensureCatalogSchema(t, db) // applies the whole dataplane lane, 000004 included
	resetProjection(t, db)
	return db, NewProjectionApplier(db)
}

// resetProjection returns the three projection tables to the state their
// migration seeds: empty mirrors and the unbootstrapped singleton. The
// truncation is what makes the suite idempotent against its own history — a
// run killed mid-test leaves rows behind, and the next run must not inherit
// them.
func resetProjection(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for _, stmt := range []string{
		`TRUNCATE api_key_credentials`,
		`TRUNCATE account_states`,
		`UPDATE projection_state SET bootstrapped = false, applied_revision = 0, producer_epoch = NULL WHERE id = 1`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("resetting the projection mirror: %v", err)
		}
	}
}

// projectionUUID mints one canonical UUID for an id a projection message
// carries: key ids, account ids and producer epochs are all just UUIDs here —
// the grammar validates their shape, never their meaning.
func projectionUUID(t *testing.T) string {
	t.Helper()
	return mintUUID(t)
}

// projectionEpoch mints one producer timeline id no other test (and no other
// run) shares.
func projectionEpoch(t *testing.T) string {
	t.Helper()
	return projectionUUID(t)
}

// The digests the fixtures rotate through: 64 lowercase hex each, the shape
// the schema's grammar check demands.
const (
	projectionDigestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	projectionDigestC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	projectionDigestD = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

// projectionInstant is a fixed wall-clock the fixtures stamp rows with: a
// whole second, so the timestamptz round trip is exact and the updated_at
// assertions can compare for equality. Distinct minutes keep a snapshot's
// stamp and a change's recording instant apart.
func projectionInstant(minute int) time.Time {
	return time.Date(2026, 9, 25, 9, minute, 0, 0, time.UTC)
}

// instantPtr is the optional instant at the call site's readability.
func instantPtr(minute int) *time.Time {
	at := projectionInstant(minute)
	return &at
}

// credentialChange builds one grammar-valid api_key entry the way the
// producer's log writes it, through the same NewChange the façade's decode
// path uses.
func credentialChange(t *testing.T, revision uint64, keyID, accountID, digest string, state projection.CredentialState, revokedAt *time.Time) projection.Change {
	t.Helper()
	payload := map[string]any{"account_id": accountID, "digest": digest, "state": string(state), "revoked_at": nil}
	if revokedAt != nil {
		payload["revoked_at"] = revokedAt.Format(time.RFC3339)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("building a credential payload: %v", err)
	}
	change, err := projection.NewChange(revision, projection.KindAPIKey, keyID, projectionInstant(0), raw)
	if err != nil {
		t.Fatalf("building credential change %d: %v", revision, err)
	}
	return change
}

// accountChange builds one grammar-valid account entry.
func accountChange(t *testing.T, revision uint64, accountID string, state projection.AccountLifecycle) projection.Change {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"state": string(state)})
	if err != nil {
		t.Fatalf("building an account payload: %v", err)
	}
	change, err := projection.NewChange(revision, projection.KindAccount, accountID, projectionInstant(0), raw)
	if err != nil {
		t.Fatalf("building account change %d: %v", revision, err)
	}
	return change
}

// batchFor wraps entries in the envelope their first revision names.
func batchFor(epoch string, changes ...projection.Change) projection.Batch {
	return projection.Batch{
		ProtocolVersion: projection.ProtocolVersion,
		Epoch:           epoch,
		FromRevision:    changes[0].Revision - 1,
		Changes:         changes,
	}
}

// mustApplyChanges applies a batch or fails the test; a projection the suite
// relies on as ground state is not a place for an if.
func mustApplyChanges(t *testing.T, applier persistence.ProjectionApplier, batch projection.Batch) uint64 {
	t.Helper()
	applied, err := applier.ApplyChanges(t.Context(), batch)
	if err != nil {
		t.Fatalf("ApplyChanges [%d..%d] error = %v", batch.Changes[0].Revision, batch.Changes[len(batch.Changes)-1].Revision, err)
	}
	return applied
}

// mustApplySnapshot applies a snapshot at its boundary or fails the test.
func mustApplySnapshot(t *testing.T, applier persistence.ProjectionApplier, snapshot projection.Snapshot) uint64 {
	t.Helper()
	applied, err := applier.ApplySnapshot(t.Context(), snapshot, projectionInstant(1))
	if err != nil {
		t.Fatalf("ApplySnapshot at %d error = %v", snapshot.SnapshotRevision, err)
	}
	return applied
}

func mustPosition(t *testing.T, applier persistence.ProjectionApplier) projection.Position {
	t.Helper()
	position, err := applier.Position(t.Context())
	if err != nil {
		t.Fatalf("Position() error = %v", err)
	}
	return position
}

// mirrorRow is one api_key_credentials row as the probes read it.
type mirrorRow struct {
	digest         string
	state          string
	revokedAt      sql.NullTime
	sourceRevision int64
	updatedAt      time.Time
}

func credentialMirrorRow(t *testing.T, db *sql.DB, keyID string) mirrorRow {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var row mirrorRow
	err := db.QueryRowContext(ctx,
		`SELECT digest, state, revoked_at, source_revision, updated_at FROM api_key_credentials WHERE key_id = $1`, keyID).
		Scan(&row.digest, &row.state, &row.revokedAt, &row.sourceRevision, &row.updatedAt)
	if err != nil {
		t.Fatalf("reading credential %s from the mirror: %v", keyID, err)
	}
	return row
}

func credentialMirrorAbsent(t *testing.T, db *sql.DB, keyID string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var one int
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM api_key_credentials WHERE key_id = $1`, keyID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true
		}
		t.Fatalf("probing credential %s: %v", keyID, err)
	}
	return false
}

func accountMirrorState(t *testing.T, db *sql.DB, accountID string) (string, int64, time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var state string
	var revision int64
	var updatedAt time.Time
	if err := db.QueryRowContext(ctx,
		`SELECT state, source_revision, updated_at FROM account_states WHERE account_id = $1`, accountID).
		Scan(&state, &revision, &updatedAt); err != nil {
		t.Fatalf("reading account %s from the mirror: %v", accountID, err)
	}
	return state, revision, updatedAt
}

func accountMirrorAbsent(t *testing.T, db *sql.DB, accountID string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var one int
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM account_states WHERE account_id = $1`, accountID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true
		}
		t.Fatalf("probing account %s: %v", accountID, err)
	}
	return false
}

// ---------------------------------------------------------------------------
// the position
// ---------------------------------------------------------------------------

func TestIntegrationProjectionPositionReadsItsSeededState(t *testing.T) {
	_, applier := integrationProjection(t)

	position := mustPosition(t, applier)
	if position.Bootstrapped {
		t.Error("a freshly seeded mirror reported bootstrapped = true")
	}
	if position.Epoch != "" {
		t.Errorf("epoch = %q, want empty — the NULL the seed writes is the unbootstrapped epoch", position.Epoch)
	}
	if position.AppliedRevision != 0 {
		t.Errorf("applied_revision = %d, want 0", position.AppliedRevision)
	}
}

func TestIntegrationProjectionEmptySnapshotBootstrapsAtZero(t *testing.T) {
	// The empty plane's bootstrap: a producer with no identity yet sends empty
	// arrays at boundary zero, and the consumer is bootstrapped afterwards —
	// on a real timeline, at a real position, with nothing mirrored.
	epoch := projectionEpoch(t)
	_, applier := integrationProjection(t)

	applied := mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 0,
	})
	if applied != 0 {
		t.Errorf("ack = %d, want the snapshot's boundary 0", applied)
	}
	position := mustPosition(t, applier)
	if !position.Bootstrapped || position.Epoch != epoch || position.AppliedRevision != 0 {
		t.Errorf("position = %+v, want bootstrapped on %s at 0", position, epoch)
	}
}

// ---------------------------------------------------------------------------
// the snapshot
// ---------------------------------------------------------------------------

func TestIntegrationProjectionSnapshotLandsRowsAndPosition(t *testing.T) {
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	db, applier := integrationProjection(t)

	appliedAt := projectionInstant(1)
	applied, err := applier.ApplySnapshot(t.Context(), projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 7,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	}, appliedAt)
	if err != nil {
		t.Fatalf("ApplySnapshot error = %v", err)
	}
	if applied != 7 {
		t.Errorf("ack = %d, want the snapshot's boundary 7", applied)
	}

	row := credentialMirrorRow(t, db, key)
	if row.digest != projectionDigestB || row.state != string(projection.CredentialActive) {
		t.Errorf("credential = %s/%s, want the snapshot's record", row.digest, row.state)
	}
	if row.revokedAt.Valid {
		t.Errorf("an active credential carried revoked_at = %v, want NULL", row.revokedAt)
	}
	if row.sourceRevision != 7 {
		t.Errorf("source_revision = %d, want the snapshot's boundary 7", row.sourceRevision)
	}
	if !row.updatedAt.Equal(appliedAt) {
		t.Errorf("updated_at = %v, want the consumer-stamped apply instant %v", row.updatedAt, appliedAt)
	}
	state, revision, updated := accountMirrorState(t, db, account)
	if state != string(projection.LifecycleActive) || revision != 7 || !updated.Equal(appliedAt) {
		t.Errorf("account = %s@%d (updated %v), want active@7 stamped by the apply", state, revision, updated)
	}
	if position := mustPosition(t, applier); !position.Bootstrapped || position.Epoch != epoch || position.AppliedRevision != 7 {
		t.Errorf("position = %+v, want bootstrapped on %s at 7", position, epoch)
	}
}

func TestIntegrationProjectionBackwardSnapshotRewindsRowsAndPosition(t *testing.T) {
	// Recovery's case: the producer's timeline rewound, and the snapshot —
	// the one operation that exists to overwrite — must move the mirror and
	// the position backward, rows included, without a guard refusing it.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	db, applier := integrationProjection(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 10,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})
	mustApplyChanges(t, applier, batchFor(epoch,
		credentialChange(t, 11, key, account, projectionDigestB, projection.CredentialRevoked, instantPtr(3))))

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 5,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestC,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleSuspended}},
	})

	row := credentialMirrorRow(t, db, key)
	if row.digest != projectionDigestC || row.state != string(projection.CredentialActive) || row.revokedAt.Valid {
		t.Errorf("credential = %s/%s revoked_at %v, want the rewound snapshot's active record", row.digest, row.state, row.revokedAt)
	}
	if row.sourceRevision != 5 {
		t.Errorf("source_revision = %d, want the rewound boundary 5", row.sourceRevision)
	}
	if state, revision, _ := accountMirrorState(t, db, account); state != string(projection.LifecycleSuspended) || revision != 5 {
		t.Errorf("account = %s@%d, want the rewound snapshot's suspended@5", state, revision)
	}
	if position := mustPosition(t, applier); position.AppliedRevision != 5 || position.Epoch != epoch {
		t.Errorf("position = %+v, want %s at 5", position, epoch)
	}
}

func TestIntegrationProjectionSnapshotReplacesWhole(t *testing.T) {
	// The snapshot's words are "be exactly this state at this boundary", so
	// the apply is a replacement, not a merge: rows the snapshot names are
	// written, and rows a previous timeline left in the mirror that the
	// snapshot does not name are gone when it commits — on both projections,
	// and inside the same transaction as the position move. A lingering row
	// would be a credential the Control Plane no longer projects, serving on
	// this plane's hot path on the strength of nothing.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	strayKey := projectionUUID(t)
	strayAccount := projectionUUID(t)
	db, applier := integrationProjection(t)

	// A previous timeline's world: two rows the next snapshot will not name.
	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 10,
		APIKeys: []projection.APIKeyCredential{
			{KeyID: strayKey, AccountID: strayAccount, Digest: projectionDigestC, State: projection.CredentialActive},
			{KeyID: key, AccountID: account, Digest: projectionDigestB, State: projection.CredentialActive},
		},
		Accounts: []projection.AccountState{
			{AccountID: strayAccount, State: projection.LifecycleActive},
			{AccountID: account, State: projection.LifecycleActive},
		},
	})

	// The next snapshot names one key and one account; boundary 0 keeps the
	// named rows' source_revision at the boundary, unambiguously the new
	// apply's work.
	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 11,
		APIKeys: []projection.APIKeyCredential{
			{KeyID: key, AccountID: account, Digest: projectionDigestB, State: projection.CredentialActive},
		},
		Accounts: []projection.AccountState{
			{AccountID: account, State: projection.LifecycleActive},
		},
	})

	if !credentialMirrorAbsent(t, db, strayKey) {
		t.Errorf("the credential the snapshot does not name is still in the mirror, want it deleted with the apply")
	}
	if !accountMirrorAbsent(t, db, strayAccount) {
		t.Errorf("the account the snapshot does not name is still in the mirror, want it deleted with the apply")
	}
	row := credentialMirrorRow(t, db, key)
	if row.sourceRevision != 11 {
		t.Errorf("the named credential's source_revision = %d, want the snapshot boundary 11", row.sourceRevision)
	}
	if position := mustPosition(t, applier); position.AppliedRevision != 11 || position.Bootstrapped != true {
		t.Errorf("position = %+v, want bootstrapped at 11", position)
	}
}

// ---------------------------------------------------------------------------
// the incremental batch
// ---------------------------------------------------------------------------

func TestIntegrationProjectionJoiningBatchAppliesRowsAndAdvances(t *testing.T) {
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	db, applier := integrationProjection(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 10,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})

	revokedAt := projectionInstant(3)
	applied := mustApplyChanges(t, applier, batchFor(epoch,
		accountChange(t, 11, account, projection.LifecycleSuspended),
		credentialChange(t, 12, key, account, projectionDigestB, projection.CredentialRevoked, &revokedAt),
	))
	if applied != 12 {
		t.Errorf("ack = %d, want the batch's last revision 12", applied)
	}

	revoked := credentialMirrorRow(t, db, key)
	if revoked.state != string(projection.CredentialRevoked) || !revoked.revokedAt.Valid || !revoked.revokedAt.Time.Equal(revokedAt) {
		t.Errorf("credential = %s revoked_at %v, want the revocation landed with its instant", revoked.state, revoked.revokedAt)
	}
	if revoked.sourceRevision != 12 {
		t.Errorf("credential source_revision = %d, want the entry's revision 12", revoked.sourceRevision)
	}
	if state, revision, _ := accountMirrorState(t, db, account); state != string(projection.LifecycleSuspended) || revision != 11 {
		t.Errorf("account = %s@%d, want suspended@11", state, revision)
	}
	if position := mustPosition(t, applier); position.AppliedRevision != 12 || position.Epoch != epoch {
		t.Errorf("position = %+v, want %s at 12", position, epoch)
	}
}

func TestIntegrationProjectionDuplicateRedeliveryWritesNothingAndAcksThePosition(t *testing.T) {
	// At-least-once's whole story in one test: the acknowledgement is lost,
	// the producer delivers what it has already delivered, and the consumer
	// answers with the position it stands at — writing nothing. The
	// updated_at column is the proof no write ran: an UPSERT that fired,
	// guarded or not, would restamp it.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	db, applier := integrationProjection(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 10,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})
	mustApplyChanges(t, applier, batchFor(epoch,
		accountChange(t, 11, account, projection.LifecycleSuspended),
		credentialChange(t, 12, key, account, projectionDigestB, projection.CredentialRevoked, instantPtr(3)),
	))
	before := credentialMirrorRow(t, db, key)

	redeliveries := map[string]projection.Batch{
		"the whole batch again": batchFor(epoch,
			accountChange(t, 11, account, projection.LifecycleSuspended),
			credentialChange(t, 12, key, account, projectionDigestB, projection.CredentialRevoked, instantPtr(3)),
		),
		"its first entry alone":                                batchFor(epoch, accountChange(t, 11, account, projection.LifecycleSuspended)),
		"its last entry alone":                                 batchFor(epoch, credentialChange(t, 12, key, account, projectionDigestB, projection.CredentialRevoked, instantPtr(3))),
		"an entry from an earlier batch a position has passed": batchFor(epoch, credentialChange(t, 9, key, account, projectionDigestB, projection.CredentialActive, nil)),
	}
	for name, redelivery := range redeliveries {
		t.Run(name, func(t *testing.T) {
			applied := mustApplyChanges(t, applier, redelivery)
			if applied != 12 {
				t.Errorf("ack = %d, want the current position 12 — the lost acknowledgement is the answer", applied)
			}
		})
	}

	after := credentialMirrorRow(t, db, key)
	if after != before {
		t.Errorf("the mirror moved under redelivery: before %+v after %+v", before, after)
	}
	if position := mustPosition(t, applier); position.AppliedRevision != 12 {
		t.Errorf("position = %d, want 12 — a duplicate must not advance it either", position.AppliedRevision)
	}
}

func TestIntegrationProjectionGappedBatchesAreRefusedWhole(t *testing.T) {
	// No fourth answer: a batch that straddles the position and one that
	// overshoots it are both the gap refusal, and neither leaves anything
	// behind — applying what fits would strand the rest as duplicates forever.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	stranger := projectionUUID(t)
	account := projectionUUID(t)
	db, applier := integrationProjection(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 10,
		Accounts:         []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})

	tests := []struct {
		name  string
		batch projection.Batch
	}{
		{
			name: "a batch straddling the position",
			batch: batchFor(epoch,
				credentialChange(t, 10, key, account, projectionDigestB, projection.CredentialActive, nil),
				accountChange(t, 11, account, projection.LifecycleSuspended),
			),
		},
		{
			name: "a batch overshooting the position",
			batch: batchFor(epoch,
				credentialChange(t, 12, stranger, account, projectionDigestB, projection.CredentialActive, nil),
				accountChange(t, 13, account, projection.LifecycleSuspended),
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applied, err := applier.ApplyChanges(t.Context(), tt.batch)
			if !errors.Is(err, projection.ErrRevisionGap) {
				t.Fatalf("err = %v, want it to wrap ErrRevisionGap", err)
			}
			if applied != 0 {
				t.Errorf("ack = %d, want 0 — a refused batch acknowledges nothing", applied)
			}
		})
	}

	if !credentialMirrorAbsent(t, db, stranger) {
		t.Error("the overshoot batch's credential landed; a refused batch must write nothing")
	}
	if state, revision, _ := accountMirrorState(t, db, account); state != string(projection.LifecycleActive) || revision != 10 {
		t.Errorf("account = %s@%d, want the snapshot's active@10 untouched", state, revision)
	}
	if position := mustPosition(t, applier); position.AppliedRevision != 10 {
		t.Errorf("position = %d, want 10 — the refusals must not have advanced it", position.AppliedRevision)
	}
}

func TestIntegrationProjectionForeignTimelineDemandsASnapshot(t *testing.T) {
	// The epoch check precedes the arithmetic: a position earned on one
	// producer timeline cannot judge a batch from another, so the answer is
	// the re-snapshot refusal whatever the revisions say.
	epochA := projectionEpoch(t)
	epochB := projectionEpoch(t)
	account := projectionUUID(t)
	db, applier := integrationProjection(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epochA,
		SnapshotRevision: 10,
		Accounts:         []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})

	if _, err := applier.ApplyChanges(t.Context(), batchFor(epochB,
		accountChange(t, 11, account, projection.LifecycleSuspended))); !errors.Is(err, projection.ErrSnapshotRequired) {
		t.Fatalf("err = %v, want it to wrap ErrSnapshotRequired", err)
	}

	if position := mustPosition(t, applier); position.Epoch != epochA || position.AppliedRevision != 10 {
		t.Errorf("position = %+v, want %s at 10 untouched by the refusal", position, epochA)
	}
	if state, revision, _ := accountMirrorState(t, db, account); state != string(projection.LifecycleActive) || revision != 10 {
		t.Errorf("account = %s@%d, want the refusal to have written nothing", state, revision)
	}
}

func TestIntegrationProjectionUnbootstrappedStoreDemandsASnapshot(t *testing.T) {
	// The same refusal is the unbootstrapped store's answer: there is no
	// position to join yet, so even a perfectly-formed batch from revision
	// one is not judgeable — the snapshot is the only way in.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	db, applier := integrationProjection(t)

	if _, err := applier.ApplyChanges(t.Context(), batchFor(epoch,
		credentialChange(t, 1, key, account, projectionDigestB, projection.CredentialActive, nil))); !errors.Is(err, projection.ErrSnapshotRequired) {
		t.Fatalf("err = %v, want it to wrap ErrSnapshotRequired", err)
	}

	position := mustPosition(t, applier)
	if position.Bootstrapped || position.Epoch != "" || position.AppliedRevision != 0 {
		t.Errorf("position = %+v, want the seeded unbootstrapped state untouched", position)
	}
	if !credentialMirrorAbsent(t, db, key) {
		t.Error("the refused batch's credential landed; a refusal must write nothing")
	}
}

// ---------------------------------------------------------------------------
// crash and retry: the transaction discipline
// ---------------------------------------------------------------------------

func TestIntegrationProjectionMidBatchFailurePersistsNothingAndReplays(t *testing.T) {
	// A real abort mid-batch, not a simulated one: the second entry violates
	// the schema's revocation-consistency check, the unit of work dies after
	// the first entry wrote — and nothing survives. The position still names
	// 10, the first entry's write is gone, and the producer replays the whole
	// batch: the crash story §23.5 asks for, proven against the server.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	db, applier := integrationProjection(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 10,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})

	refused := projection.Batch{
		ProtocolVersion: projection.ProtocolVersion,
		Epoch:           epoch,
		FromRevision:    10,
		Changes: []projection.Change{
			accountChange(t, 11, account, projection.LifecycleSuspended),
			// Built as a struct, past the grammar that would refuse it, on
			// purpose: the port assumes validated input, and this is the tier
			// where the suite arranges state directly to reach the schema's
			// own refusal — the one a real crash most resembles.
			{
				Revision: 12, Kind: projection.KindAPIKey, ResourceID: key,
				RecordedAt: projectionInstant(0),
				Credential: &projection.APIKeyCredential{
					KeyID: key, AccountID: account, Digest: projectionDigestB,
					State: projection.CredentialRevoked, RevokedAt: nil,
				},
			},
		},
	}
	if _, err := applier.ApplyChanges(t.Context(), refused); err == nil {
		t.Fatal("the schema accepted a revoked credential with no instant; the batch should have aborted")
	}

	if state, revision, _ := accountMirrorState(t, db, account); state != string(projection.LifecycleActive) || revision != 10 {
		t.Errorf("account = %s@%d, want the snapshot's active@10 — the aborted entry must not survive", state, revision)
	}
	if row := credentialMirrorRow(t, db, key); row.sourceRevision != 10 {
		t.Errorf("credential source_revision = %d, want 10 — the batch must have landed nothing", row.sourceRevision)
	}
	if position := mustPosition(t, applier); position.AppliedRevision != 10 {
		t.Errorf("position = %d, want 10 — a crashed delivery must not have advanced it", position.AppliedRevision)
	}

	// The replay: the same revisions, delivered whole and well-formed, join
	// the position the crash left behind and land once.
	applied := mustApplyChanges(t, applier, batchFor(epoch,
		accountChange(t, 11, account, projection.LifecycleSuspended),
		credentialChange(t, 12, key, account, projectionDigestB, projection.CredentialRevoked, instantPtr(3)),
	))
	if applied != 12 {
		t.Errorf("replay ack = %d, want 12", applied)
	}
	if state, _, _ := accountMirrorState(t, db, account); state != string(projection.LifecycleSuspended) {
		t.Errorf("account = %s after the replay, want suspended", state)
	}
	revoked := credentialMirrorRow(t, db, key)
	if revoked.state != string(projection.CredentialRevoked) || !revoked.revokedAt.Valid {
		t.Errorf("credential = %s revoked_at %v after the replay, want the revocation with its instant", revoked.state, revoked.revokedAt)
	}
}

// ---------------------------------------------------------------------------
// the per-row revision guard
// ---------------------------------------------------------------------------

func TestIntegrationProjectionGuardSkipsSupersededRows(t *testing.T) {
	// The guard's own case, arranged directly: mirror rows already written at
	// a revision the position has not reached (the state a non-protocol write
	// or a legacy row leaves behind). A joining batch must not regress them —
	// the position still advances, the rows the mirror already supersedes do
	// not move.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	db, applier := integrationProjection(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 50,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx,
		`UPDATE api_key_credentials SET digest = $1, source_revision = 100 WHERE key_id = $2`,
		projectionDigestC, key); err != nil {
		t.Fatalf("arranging the superseded credential row: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE account_states SET state = 'suspended', source_revision = 100 WHERE account_id = $1`,
		account); err != nil {
		t.Fatalf("arranging the superseded account row: %v", err)
	}

	applied := mustApplyChanges(t, applier, batchFor(epoch,
		credentialChange(t, 51, key, account, projectionDigestD, projection.CredentialActive, nil),
		accountChange(t, 52, account, projection.LifecycleSuspended),
	))
	if applied != 52 {
		t.Errorf("ack = %d, want 52 — the guard skips rows, never the position", applied)
	}

	row := credentialMirrorRow(t, db, key)
	if row.digest != projectionDigestC || row.sourceRevision != 100 {
		t.Errorf("credential = %s@%d, want the superseded content c@100 kept — entry 51 must not regress it", row.digest, row.sourceRevision)
	}
	if state, revision, _ := accountMirrorState(t, db, account); state != "suspended" || revision != 100 {
		t.Errorf("account = %s@%d, want the superseded suspended@100 kept — entry 52 must not regress it", state, revision)
	}
	if position := mustPosition(t, applier); position.AppliedRevision != 52 {
		t.Errorf("position = %d, want 52", position.AppliedRevision)
	}
}

func TestIntegrationProjectionTerminalRegressionIsRefusedWhole(t *testing.T) {
	// The consumer-side lifecycle rule against the real schema: a batch entry
	// that would move a revoked credential or a closed account back into a
	// live state is refused, the mirror keeps the terminal state, the
	// position does not move, and the refusal is whole — the healthy entry
	// riding beside the regressing one lands no more than it does. A
	// conforming producer cannot express such an entry; this is the
	// fail-closed answer to one that does (§23.5's corrupted-feed case).
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	freshKey := projectionUUID(t)
	revokedAt := projectionInstant(3)
	db, applier := integrationProjection(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 10,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialRevoked, RevokedAt: &revokedAt,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleClosed}},
	})

	applied, err := applier.ApplyChanges(t.Context(), batchFor(epoch,
		credentialChange(t, 11, key, account, projectionDigestC, projection.CredentialActive, nil),
		credentialChange(t, 12, freshKey, account, projectionDigestD, projection.CredentialActive, nil),
	))
	if !errors.Is(err, projection.ErrTerminalRegression) {
		t.Fatalf("err = %v, want it to wrap ErrTerminalRegression", err)
	}
	if applied != 0 {
		t.Errorf("ack = %d on a refused batch, want 0", applied)
	}

	row := credentialMirrorRow(t, db, key)
	if row.state != "revoked" || row.digest != projectionDigestB || row.sourceRevision != 10 {
		t.Errorf("credential = %s@%d, want the revoked content b@10 kept", row.digest, row.sourceRevision)
	}
	if !credentialMirrorAbsent(t, db, freshKey) {
		t.Error("the batch's healthy entry landed; a refused batch must land nothing")
	}
	if state, revision, _ := accountMirrorState(t, db, account); state != "closed" || revision != 10 {
		t.Errorf("account = %s@%d, want the closed state at 10 kept", state, revision)
	}
	if position := mustPosition(t, applier); position.AppliedRevision != 10 {
		t.Errorf("position = %d, want 10 — a refused batch advances nothing", position.AppliedRevision)
	}

	// The account half of the same rule: a closed account is not reopened,
	// whatever the revision names. The position never moved, so this batch
	// joins at the same successor the refused one did.
	_, err = applier.ApplyChanges(t.Context(), batchFor(epoch,
		accountChange(t, 11, account, projection.LifecycleActive),
	))
	if !errors.Is(err, projection.ErrTerminalRegression) {
		t.Fatalf("account err = %v, want it to wrap ErrTerminalRegression", err)
	}
	if state, _, _ := accountMirrorState(t, db, account); state != "closed" {
		t.Errorf("account state = %s, want closed kept", state)
	}
	if position := mustPosition(t, applier); position.AppliedRevision != 10 {
		t.Errorf("position = %d after the refused account batch, want 10", position.AppliedRevision)
	}
}

func TestIntegrationProjectionSnapshotMayOverwriteTerminalRows(t *testing.T) {
	// The one delivery allowed to move a terminal row is the snapshot: it is
	// the authority's word entire, not a per-entry claim, and recovery must
	// be able to replace the mirror with whatever the Control Plane now
	// says. This pins the exemption the refusal test above rests on.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	revokedAt := projectionInstant(3)
	db, applier := integrationProjection(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 10,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialRevoked, RevokedAt: &revokedAt,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleClosed}},
	})

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 20,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestC,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})

	row := credentialMirrorRow(t, db, key)
	if row.state != "active" || row.revokedAt.Valid || row.digest != projectionDigestC {
		t.Errorf("credential = %s (revoked_at valid: %t), want the snapshot's active state whole", row.state, row.revokedAt.Valid)
	}
	if state, revision, _ := accountMirrorState(t, db, account); state != "active" || revision != 20 {
		t.Errorf("account = %s@%d, want active@20 from the snapshot", state, revision)
	}
	if position := mustPosition(t, applier); position.AppliedRevision != 20 {
		t.Errorf("position = %d, want 20", position.AppliedRevision)
	}
}

// ---------------------------------------------------------------------------
// durability and concurrency
// ---------------------------------------------------------------------------

func TestIntegrationProjectionPositionSurvivesAConsumerRestart(t *testing.T) {
	// The consumer went down and came back as a new pool over the same
	// database: the position, the rows and the epoch must all still be there,
	// and a redelivery of what was already applied must answer with the
	// position — the downtime story §23.6 asks for.
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	db, applier := integrationProjection(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 10,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})
	mustApplyChanges(t, applier, batchFor(epoch,
		accountChange(t, 11, account, projection.LifecycleSuspended),
		credentialChange(t, 12, key, account, projectionDigestB, projection.CredentialActive, nil),
	))

	if err := db.Close(); err != nil {
		t.Fatalf("closing the first pool: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reborn, err := Open(ctx, Options{
		DSN:             integrationPlaneDSN(t),
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("reopening the pool: %v", err)
	}
	t.Cleanup(func() { _ = reborn.Close() })
	applier = NewProjectionApplier(reborn)

	position := mustPosition(t, applier)
	if !position.Bootstrapped || position.Epoch != epoch || position.AppliedRevision != 12 {
		t.Fatalf("position after the restart = %+v, want bootstrapped on %s at 12", position, epoch)
	}
	applied := mustApplyChanges(t, applier, batchFor(epoch,
		accountChange(t, 11, account, projection.LifecycleSuspended),
		credentialChange(t, 12, key, account, projectionDigestB, projection.CredentialActive, nil),
	))
	if applied != 12 {
		t.Errorf("redelivery after the restart ack = %d, want the standing position 12", applied)
	}
}

func TestIntegrationProjectionConcurrentRedeliveryAppliesOnce(t *testing.T) {
	// The lock's observable contract under contention: every racer of the
	// same joining batch comes back answered — the winner applied it, the
	// rest found the position the winner left and acknowledged it — and the
	// mirror ends in exactly the state one application writes. No racer is
	// refused, none is dropped, and the position never passes the batch.
	const racers = 4
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)
	db, applier := integrationProjection(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 10,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	})

	joined := batchFor(epoch,
		accountChange(t, 11, account, projection.LifecycleSuspended),
		credentialChange(t, 12, key, account, projectionDigestB, projection.CredentialActive, nil),
	)
	acks := make([]uint64, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			acks[slot], errs[slot] = applier.ApplyChanges(t.Context(), joined)
		}(i)
	}
	wg.Wait()

	for slot, err := range errs {
		if err != nil {
			t.Fatalf("racer %d was refused: %v — a redelivery of a joining batch must answer, not fail", slot, err)
		}
		if acks[slot] != 12 {
			t.Errorf("racer %d ack = %d, want 12", slot, acks[slot])
		}
	}
	if position := mustPosition(t, applier); position.AppliedRevision != 12 {
		t.Errorf("position = %d, want 12 — the race must not advance it past the batch", position.AppliedRevision)
	}
	if state, revision, _ := accountMirrorState(t, db, account); state != string(projection.LifecycleSuspended) || revision != 11 {
		t.Errorf("account = %s@%d after the race, want suspended@11 exactly once", state, revision)
	}
	row := credentialMirrorRow(t, db, key)
	if row.digest != projectionDigestB || row.sourceRevision != 12 {
		t.Errorf("credential = %s@%d after the race, want the batch's record@12 exactly once", row.digest, row.sourceRevision)
	}
}

// ---------------------------------------------------------------------------
// what the mirror holds
// ---------------------------------------------------------------------------

func TestIntegrationProjectionMirrorHoldsADigestAndNoSecretColumn(t *testing.T) {
	// §23.7's storage half against the live schema: the credential mirror's
	// column set is exactly the declared one — there is no column a plaintext
	// could hide in — and the one secret-shaped column holds the 64-hex
	// digest verbatim, which confirms a secret and cannot reconstruct one.
	db, applier := integrationProjection(t)
	epoch := projectionEpoch(t)
	key := projectionUUID(t)
	account := projectionUUID(t)

	mustApplySnapshot(t, applier, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            epoch,
		SnapshotRevision: 1,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: key, AccountID: account, Digest: projectionDigestB,
			State: projection.CredentialActive,
		}},
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	columns := func(table string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx,
			`SELECT column_name FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name = $1 ORDER BY column_name`, table)
		if err != nil {
			t.Fatalf("listing %s's columns: %v", table, err)
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scanning a column name: %v", err)
			}
			names = append(names, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("listing %s's columns: %v", table, err)
		}
		return names
	}

	wantCredential := []string{"account_id", "digest", "key_id", "revoked_at", "source_revision", "state", "updated_at"}
	if got := columns("api_key_credentials"); strings.Join(got, ",") != strings.Join(wantCredential, ",") {
		t.Errorf("api_key_credentials columns = %v, want exactly %v — no column beyond the declared set", got, wantCredential)
	}
	wantAccount := []string{"account_id", "source_revision", "state", "updated_at"}
	if got := columns("account_states"); strings.Join(got, ",") != strings.Join(wantAccount, ",") {
		t.Errorf("account_states columns = %v, want exactly %v", got, wantAccount)
	}

	if row := credentialMirrorRow(t, db, key); row.digest != projectionDigestB {
		t.Errorf("digest on disk = %q, want the 64-hex the producer sent, verbatim", row.digest)
	}
}
