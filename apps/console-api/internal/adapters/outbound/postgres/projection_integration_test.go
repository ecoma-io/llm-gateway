//go:build integration

package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// The projection repositories against the real `control` database. The
// scripted-statement tests pin what the repositories say; only PostgreSQL can
// answer what the database does with it — that the revision counter's row lock
// makes allocation gapless and commit-ordered even under concurrent writers,
// that an aborted unit of work gives its number back, that a record rides its
// caller's transaction whole, that the mirror's upsert keeps one row per
// resource, and that the CHECKs the migration pinned are the first guard on
// the grammar. Like the identity suite beside it, this tier re-proves none of
// deploy/postgres/verify.sh's SQL-level work: it drives the port, and the
// schema answers through it.
//
// The subject tables are this suite's alone — the identity repositories never
// read or write them — so the reset below truncates the three projection
// tables and rewinds the counter, touching nothing the identity tests own.
//
// Run:
//
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:5432/postgres?sslmode=disable' \
//	  go test -tags=integration ./internal/adapters/outbound/postgres

// integrationProjection opens the control database, returns the projection
// foundation's two halves over one store — the read half the delivery loop
// consumes, the write half the identity use cases consume — and leaves the
// four projection objects in the state a database migrated from empty holds:
// no log entries, no mirror rows, a counter at zero under a fresh epoch.
func integrationProjection(t *testing.T) (*sql.DB, persistence.Store, persistence.ProjectionLog, persistence.ProjectionChangeRecorder) {
	t.Helper()
	db := integrationDB(t)
	resetProjectionTables(t, db)
	store := New(db)
	return db, store, NewProjectionLog(store), NewProjectionChangeRecorder(store)
}

// resetProjectionTables is this suite's reset: the three projection tables
// truncated and the singleton counter rewound to zero under a fresh epoch —
// the shape the projection foundation's migration itself leaves behind on a
// database with no accounts to backfill. TRUNCATE and UPDATE, never DROP and
// never a database replaced: the fixture is shared state, and the objects
// reset here are the ones this suite's subject owns.
func resetProjectionTables(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx,
		`TRUNCATE control.projection_changes, control.projection_api_keys, control.projection_accounts`); err != nil {
		t.Fatalf("truncate the projection tables: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE control.projection_revision SET last_revision = 0, epoch = gen_random_uuid() WHERE id = 1`); err != nil {
		t.Fatalf("rewind the projection counter: %v", err)
	}
}

// mintProjectionID is one fresh version-4 UUID — the form the projection
// grammar holds every projected id to — so a rerun against a database other
// probes have written cannot trip over history.
func mintProjectionID(t *testing.T) string {
	t.Helper()
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatalf("mint a uuid: %v", err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40 // the version nibble
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // the RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// projectionFixtureAt is the recorded instant the tests carry: truncated to
// microseconds, because that is the precision a timestamptz keeps. An
// assertion against a stored instant must compare what the database can hold,
// not the nanoseconds the process clock happened to have.
func projectionFixtureAt(t *testing.T) time.Time {
	t.Helper()
	return time.Now().UTC().Truncate(time.Microsecond)
}

// projectionFixtureDigest mints a valid digest from one two-character hex
// pair repeated to the grammar's length, so tests can hold visually distinct
// digests without sixty-four-character literals.
func projectionFixtureDigest(t *testing.T, pair string) projection.Digest {
	t.Helper()
	digest, err := projection.NewDigest(strings.Repeat(pair, 32))
	if err != nil {
		t.Fatalf("NewDigest(%q repeated to length): %v", pair, err)
	}
	return digest
}

// projectionFixtureCredential builds a projected credential through the domain
// constructor: a value the grammar vouches for, so a refusal below is the
// subject's, never the fixture's shape.
func projectionFixtureCredential(t *testing.T, keyID, accountID string, digest projection.Digest, state projection.CredentialState, revokedAt *time.Time) projection.Credential {
	t.Helper()
	credential, err := projection.NewCredential(keyID, accountID, string(digest), state, revokedAt)
	if err != nil {
		t.Fatalf("NewCredential(%s, %s, %s): %v", keyID, accountID, state, err)
	}
	return credential
}

// projectionFixtureAccount is the account half of the same discipline.
func projectionFixtureAccount(t *testing.T, accountID string, state projection.AccountState) projection.Account {
	t.Helper()
	account, err := projection.NewAccount(accountID, state)
	if err != nil {
		t.Fatalf("NewAccount(%s, %s): %v", accountID, state, err)
	}
	return account
}

// recordProjectionCredential records one credential change inside its own unit
// of work — the discipline the port's contract puts on every caller, and the
// reason allocation order is commit order.
func recordProjectionCredential(t *testing.T, store persistence.Store, recorder persistence.ProjectionChangeRecorder, at time.Time, credential projection.Credential) {
	t.Helper()
	if err := store.WithinTx(t.Context(), func(ctx context.Context) error {
		return recorder.RecordCredentialChange(ctx, at, credential)
	}); err != nil {
		t.Fatalf("record credential change %s: %v", credential.KeyID, err)
	}
}

// recordProjectionAccount is the account half of the same helper.
func recordProjectionAccount(t *testing.T, store persistence.Store, recorder persistence.ProjectionChangeRecorder, at time.Time, account projection.Account) {
	t.Helper()
	if err := store.WithinTx(t.Context(), func(ctx context.Context) error {
		return recorder.RecordAccountChange(ctx, at, account)
	}); err != nil {
		t.Fatalf("record account change %s: %v", account.AccountID, err)
	}
}

// projectionLogEntry is one row of the durable log, read back past the
// adapter: what the database kept, not what the port handed out.
type projectionLogEntry struct {
	revision   uint64
	kind       string
	resourceID string
	payload    string
}

func projectionLogEntries(t *testing.T, db *sql.DB) []projectionLogEntry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx,
		`SELECT revision, resource_kind, resource_id, payload FROM control.projection_changes ORDER BY revision`)
	if err != nil {
		t.Fatalf("read the projection log: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var entries []projectionLogEntry
	for rows.Next() {
		var entry projectionLogEntry
		if err := rows.Scan(&entry.revision, &entry.kind, &entry.resourceID, &entry.payload); err != nil {
			t.Fatalf("scan a projection log row: %v", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the projection log: %v", err)
	}
	return entries
}

func projectionLogRevisions(t *testing.T, db *sql.DB) []uint64 {
	t.Helper()
	entries := projectionLogEntries(t, db)
	revisions := make([]uint64, len(entries))
	for i, entry := range entries {
		revisions[i] = entry.revision
	}
	return revisions
}

// projectionKeyRow is one materialized credential row, read back past the
// adapter.
type projectionKeyRow struct {
	accountID      string
	digest         string
	state          string
	revokedAt      sql.NullTime
	sourceRevision uint64
}

func projectionKeyMirror(t *testing.T, db *sql.DB, keyID string) (projectionKeyRow, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var row projectionKeyRow
	err := db.QueryRowContext(ctx,
		`SELECT account_id, digest, state, revoked_at, source_revision FROM control.projection_api_keys WHERE key_id = $1`,
		keyID,
	).Scan(&row.accountID, &row.digest, &row.state, &row.revokedAt, &row.sourceRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return projectionKeyRow{}, false
	}
	if err != nil {
		t.Fatalf("read the credential mirror row for %s: %v", keyID, err)
	}
	return row, true
}

func projectionAccountMirror(t *testing.T, db *sql.DB, accountID string) (state string, sourceRevision uint64, present bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := db.QueryRowContext(ctx,
		`SELECT state, source_revision FROM control.projection_accounts WHERE account_id = $1`,
		accountID,
	).Scan(&state, &sourceRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false
	}
	if err != nil {
		t.Fatalf("read the account mirror row for %s: %v", accountID, err)
	}
	return state, sourceRevision, true
}

func projectionCounterHead(t *testing.T, db *sql.DB) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var lastRevision uint64
	if err := db.QueryRowContext(ctx,
		`SELECT last_revision FROM control.projection_revision WHERE id = 1`).Scan(&lastRevision); err != nil {
		t.Fatalf("read the projection counter: %v", err)
	}
	return lastRevision
}

func projectionCounterRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM control.projection_revision`).Scan(&count); err != nil {
		t.Fatalf("count the projection counter's rows: %v", err)
	}
	return count
}

// wantRevisionSequence asserts the log's revisions are exactly 1..N ascending:
// the set and the order at once.
func wantRevisionSequence(t *testing.T, got []uint64, n int) {
	t.Helper()
	want := make([]uint64, n)
	for i := range want {
		want[i] = uint64(i) + 1
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the log holds revisions %v, want the gapless 1..%d — allocation order is the log's own order", got, n)
	}
}

// ---------------------------------------------------------------------------
// Allocation: the gapless, commit-ordered counter.
// ---------------------------------------------------------------------------

func TestIntegrationSequentialRecordsNumberTheLogWithoutGaps(t *testing.T) {
	db, store, log, recorder := integrationProjection(t)
	ctx := t.Context()
	accountID := mintProjectionID(t)
	at := projectionFixtureAt(t)

	// Five changes, credentials and accounts alternating, each in its own
	// unit of work.
	for i := range 5 {
		if i%2 == 0 {
			recordProjectionCredential(t, store, recorder, at, projectionFixtureCredential(t,
				mintProjectionID(t), accountID, projectionFixtureDigest(t, "ab"), projection.CredentialActive, nil))
			continue
		}
		recordProjectionAccount(t, store, recorder, at, projectionFixtureAccount(t, mintProjectionID(t), projection.AccountActive))
	}

	wantRevisionSequence(t, projectionLogRevisions(t, db), 5)
	head, err := log.Head(ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.LastRevision != 5 {
		t.Fatalf("Head.LastRevision = %d, want 5 — the counter ends where the log does", head.LastRevision)
	}
}

func TestIntegrationConcurrentRecordsAllocateDistinctGaplessRevisions(t *testing.T) {
	db, store, _, recorder := integrationProjection(t)
	ctx := t.Context()
	accountID := mintProjectionID(t)
	digest := projectionFixtureDigest(t, "ab")
	at := projectionFixtureAt(t)

	// Eight writers, ten records each, every record in its own unit of work:
	// the counter's row lock is held from the allocation to the commit, so
	// the next writer's identical UPDATE blocks. Two things would go wrong
	// without that serialization, and both are asserted here — duplicate
	// allocations would collide on the log's primary key and fail a writer,
	// and a released-early lock would leave gaps.
	const racers = 8
	const perRacer = 10
	revisions := make(chan uint64, racers*perRacer)
	failures := make(chan error, racers*perRacer)
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perRacer {
				keyID := mintProjectionID(t)
				credential := projectionFixtureCredential(t, keyID, accountID, digest, projection.CredentialActive, nil)
				var revision uint64
				err := store.WithinTx(ctx, func(ctx context.Context) error {
					if err := recorder.RecordCredentialChange(ctx, at, credential); err != nil {
						return err
					}
					// The writer's own mirror row names the revision its
					// transaction allocated — read inside the same unit, so
					// the number belongs to a commit that really happened.
					return store.Querier(ctx).QueryRowContext(ctx,
						`SELECT source_revision FROM control.projection_api_keys WHERE key_id = $1`, keyID,
					).Scan(&revision)
				})
				if err != nil {
					failures <- err
					continue
				}
				revisions <- revision
			}
		}()
	}
	wg.Wait()
	close(revisions)
	close(failures)
	for err := range failures {
		t.Errorf("a concurrent record failed: %v — losing a race for the counter must never be possible", err)
	}

	allocated := make([]uint64, 0, racers*perRacer)
	for revision := range revisions {
		allocated = append(allocated, revision)
	}
	if len(allocated) != racers*perRacer {
		t.Fatalf("%d of %d concurrent records produced a readable revision, want all of them", len(allocated), racers*perRacer)
	}
	slices.Sort(allocated)
	wantRevisionSequence(t, allocated, racers*perRacer)

	// And the mirrors agree with the log: one row per record, each naming a
	// different revision — the same numbers, from the same allocations.
	var logRows, distinctRevisions, mirrorRows, distinctSources int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*), count(DISTINCT revision) FROM control.projection_changes`).Scan(&logRows, &distinctRevisions); err != nil {
		t.Fatalf("count the log: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*), count(DISTINCT source_revision) FROM control.projection_api_keys`).Scan(&mirrorRows, &distinctSources); err != nil {
		t.Fatalf("count the credential mirror: %v", err)
	}
	if logRows != racers*perRacer || distinctRevisions != racers*perRacer || mirrorRows != racers*perRacer || distinctSources != racers*perRacer {
		t.Fatalf("log = (%d rows, %d revisions), mirror = (%d rows, %d source revisions); want %d of each — no two records may share a revision",
			logRows, distinctRevisions, mirrorRows, distinctSources, racers*perRacer)
	}
}

func TestIntegrationAnAbortedUnitReleasesItsNumber(t *testing.T) {
	db, store, log, recorder := integrationProjection(t)
	ctx := t.Context()
	accountID := mintProjectionID(t)
	digest := projectionFixtureDigest(t, "ab")
	at := projectionFixtureAt(t)

	for range 3 {
		recordProjectionCredential(t, store, recorder, at,
			projectionFixtureCredential(t, mintProjectionID(t), accountID, digest, projection.CredentialActive, nil))
	}

	// The record lands, then the unit fails on its own business — the
	// authority write the projection rode along with did not happen.
	abortedKey := mintProjectionID(t)
	boom := errors.New("the authority write failed — the projection must not keep its number")
	err := store.WithinTx(ctx, func(ctx context.Context) error {
		if err := recorder.RecordCredentialChange(ctx, at,
			projectionFixtureCredential(t, abortedKey, accountID, digest, projection.CredentialActive, nil)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithinTx returned %v, want the unit's own error handed back", err)
	}

	// The counter rolled back with the unit: the number is as if never taken.
	head, err := log.Head(ctx)
	if err != nil {
		t.Fatalf("Head after the aborted unit: %v", err)
	}
	if head.LastRevision != 3 {
		t.Fatalf("Head.LastRevision after the aborted unit = %d, want 3 — an aborted allocation must not survive its own rollback", head.LastRevision)
	}
	wantRevisionSequence(t, projectionLogRevisions(t, db), 3)
	if _, present := projectionKeyMirror(t, db, abortedKey); present {
		t.Fatal("the aborted record's mirror row survived its unit's rollback")
	}

	// And the next successful record takes the released number: the gap an
	// abort would otherwise open is the gap the consumer's position detection
	// would trip over.
	nextKey := mintProjectionID(t)
	recordProjectionCredential(t, store, recorder, at,
		projectionFixtureCredential(t, nextKey, accountID, digest, projection.CredentialActive, nil))
	head, err = log.Head(ctx)
	if err != nil {
		t.Fatalf("Head after the reused number: %v", err)
	}
	if head.LastRevision != 4 {
		t.Fatalf("Head.LastRevision after the reused number = %d, want 4 — the released number must be issued again, not skipped", head.LastRevision)
	}
	row, present := projectionKeyMirror(t, db, nextKey)
	if !present || row.sourceRevision != 4 {
		t.Fatalf("the next record's mirror = (%v, %v), want a row at revision 4 — the released number, reused", present, row.sourceRevision)
	}
	wantRevisionSequence(t, projectionLogRevisions(t, db), 4)
}

// ---------------------------------------------------------------------------
// The unit of work: both rows or neither.
// ---------------------------------------------------------------------------

func TestIntegrationARecordRidesItsUnitOfWorkWhole(t *testing.T) {
	db, store, _, recorder := integrationProjection(t)
	ctx := t.Context()
	accountID := mintProjectionID(t)
	digest := projectionFixtureDigest(t, "cd")
	at := projectionFixtureAt(t)
	keyID := mintProjectionID(t)
	credential := projectionFixtureCredential(t, keyID, accountID, digest, projection.CredentialActive, nil)

	// The record runs its two statements inside the caller's transaction, so
	// the caller's failure is the record's: neither the log entry nor the
	// mirror row may survive a rollback. The mirror cannot be made to fail by
	// any value the domain lets through — the CHECKs hold both rows to shapes
	// the grammar already guarantees — so the rollback is the honest probe of
	// both-or-neither.
	boom := errors.New("the unit of work failed after the record")
	err := store.WithinTx(ctx, func(ctx context.Context) error {
		if err := recorder.RecordCredentialChange(ctx, at, credential); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithinTx returned %v, want the unit's own error handed back", err)
	}
	if entries := projectionLogEntries(t, db); len(entries) != 0 {
		t.Fatalf("the log kept %d entries after the failed unit, want none — the record must not outlive its transaction", len(entries))
	}
	if _, present := projectionKeyMirror(t, db, keyID); present {
		t.Fatal("the mirror row survived the failed unit — the projection now claims a change the authority never committed")
	}
	if head := projectionCounterHead(t, db); head != 0 {
		t.Fatalf("the counter sits at %d after the failed unit, want 0 — the allocation rolled back with the unit", head)
	}

	// The same record in a unit that succeeds leaves both, in one piece.
	if err := store.WithinTx(ctx, func(ctx context.Context) error {
		return recorder.RecordCredentialChange(ctx, at, credential)
	}); err != nil {
		t.Fatalf("WithinTx of the committed record: %v", err)
	}
	entries := projectionLogEntries(t, db)
	if len(entries) != 1 || entries[0].revision != 1 {
		t.Fatalf("the log after the committed unit = %+v, want the one entry at revision 1", entries)
	}
	row, present := projectionKeyMirror(t, db, keyID)
	if !present || row.sourceRevision != 1 {
		t.Fatalf("the mirror after the committed unit = (%v, %+v), want the row at revision 1", present, row)
	}
	if head := projectionCounterHead(t, db); head != 1 {
		t.Fatalf("the counter sits at %d after the committed unit, want 1", head)
	}
}

func TestIntegrationARefusedLogAppendLeavesNoMirrorBehind(t *testing.T) {
	db, store, _, recorder := integrationProjection(t)
	ctx := t.Context()
	accountID := mintProjectionID(t)
	digest := projectionFixtureDigest(t, "ab")
	at := projectionFixtureAt(t)
	keyID := mintProjectionID(t)

	// The revision the next record will allocate is already taken — a shape
	// the adapter cannot produce, written past it by hand to ask the one
	// question the ordering claim makes testable: when the log append fails,
	// does the mirror stay behind?
	taken := fmt.Sprintf(`{"account_id":%q,"digest":%q,"state":"active","revoked_at":null}`, accountID, string(digest))
	if _, err := db.ExecContext(ctx,
		`INSERT INTO control.projection_changes (revision, resource_kind, resource_id, recorded_at, payload)
		 VALUES (1, 'api_key', $1, $2, $3)`, keyID, at, taken); err != nil {
		t.Fatalf("seed the taken revision: %v", err)
	}

	err := store.WithinTx(ctx, func(ctx context.Context) error {
		return recorder.RecordCredentialChange(ctx, at,
			projectionFixtureCredential(t, keyID, accountID, digest, projection.CredentialActive, nil))
	})
	if err == nil || !strings.Contains(err.Error(), "at revision 1") {
		t.Fatalf("the record under a taken revision error = %v, want the append's failure at revision 1", err)
	}
	if _, present := projectionKeyMirror(t, db, keyID); present {
		t.Fatal("the mirror row was written for a log append that failed — the mirror is ahead of the log")
	}
	if head := projectionCounterHead(t, db); head != 0 {
		t.Fatalf("the counter sits at %d, want 0 — the failed allocation rolled back with the unit", head)
	}
	if entries := projectionLogEntries(t, db); len(entries) != 1 {
		t.Fatalf("the log holds %d entries, want only the seeded one", len(entries))
	}
}

// ---------------------------------------------------------------------------
// The mirrors: one row per resource, the log's last word.
// ---------------------------------------------------------------------------

func TestIntegrationTheCredentialMirrorKeepsOneRowPerKey(t *testing.T) {
	db, store, _, recorder := integrationProjection(t)
	ctx := t.Context()
	accountID := mintProjectionID(t)
	digest := projectionFixtureDigest(t, "ab")
	at := projectionFixtureAt(t)
	keyID := mintProjectionID(t)

	activeAt := at.Add(-2 * time.Hour)
	recordProjectionCredential(t, store, recorder, activeAt,
		projectionFixtureCredential(t, keyID, accountID, digest, projection.CredentialActive, nil))
	row, present := projectionKeyMirror(t, db, keyID)
	if !present {
		t.Fatal("the minted key has no mirror row — the snapshot source would miss a live credential")
	}
	// An active row carries SQL NULL where a revocation instant would be —
	// the absence the any(nil) path produces, not an empty timestamp standing
	// in for it. The schema's revocation-consistency CHECK makes the pairing
	// unstateable in either direction; this pins the adapter's half of it.
	if row.state != "active" || row.revokedAt.Valid {
		t.Fatalf("the active mirror row = (state %q, revoked_at %v), want active with a NULL revocation instant", row.state, row.revokedAt)
	}
	if row.digest != string(digest) || row.accountID != accountID {
		t.Fatalf("the active mirror row = (account %s, digest %s), want (%s, %s) verbatim", row.accountID, row.digest, accountID, digest)
	}
	if row.sourceRevision != 1 {
		t.Fatalf("the active mirror row names source_revision %d, want 1", row.sourceRevision)
	}

	// The revocation replaces the row — same key, one row, the log's last
	// word and the revision it came from.
	revokedAt := at.Add(-time.Hour)
	recordProjectionCredential(t, store, recorder, at,
		projectionFixtureCredential(t, keyID, accountID, digest, projection.CredentialRevoked, &revokedAt))
	var mirrorRows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM control.projection_api_keys WHERE key_id = $1`, keyID).Scan(&mirrorRows); err != nil {
		t.Fatalf("count the key's mirror rows: %v", err)
	}
	if mirrorRows != 1 {
		t.Fatalf("the key has %d mirror rows, want 1 — revocation is a state, not a second row", mirrorRows)
	}
	row, present = projectionKeyMirror(t, db, keyID)
	if !present || row.state != "revoked" || !row.revokedAt.Valid {
		t.Fatalf("the revoked mirror row = (state %q, revoked_at %v), want revoked with its instant", row.state, row.revokedAt)
	}
	if !row.revokedAt.Time.Equal(revokedAt) {
		t.Fatalf("the revoked mirror row's instant = %v, want %v — the recorded instant, not the write's", row.revokedAt.Time, revokedAt)
	}
	if row.sourceRevision != 2 {
		t.Fatalf("the revoked mirror row names source_revision %d, want 2 — the revocation's revision, not the mint's", row.sourceRevision)
	}
	wantRevisionSequence(t, projectionLogRevisions(t, db), 2)
}

func TestIntegrationCredentialReturnsTheRowOrTheMiss(t *testing.T) {
	_, store, log, recorder := integrationProjection(t)
	ctx := t.Context()
	accountID := mintProjectionID(t)
	digest := projectionFixtureDigest(t, "cd")
	at := projectionFixtureAt(t)

	activeKey := mintProjectionID(t)
	recordProjectionCredential(t, store, recorder, at,
		projectionFixtureCredential(t, activeKey, accountID, digest, projection.CredentialActive, nil))
	revokedKey := mintProjectionID(t)
	revokedAt := at.Add(-time.Hour)
	recordProjectionCredential(t, store, recorder, at,
		projectionFixtureCredential(t, revokedKey, accountID, digest, projection.CredentialRevoked, &revokedAt))

	// The point read is the revoke path's window onto the row: the digest it
	// must describe exists nowhere else in this database.
	active, err := log.Credential(ctx, activeKey)
	if err != nil {
		t.Fatalf("Credential for the live key: %v", err)
	}
	if active.KeyID != activeKey || active.AccountID != accountID || active.Digest != digest ||
		active.State != projection.CredentialActive || active.RevokedAt != nil {
		t.Fatalf("Credential for the live key = %+v, want the mirror row decoded, revocation absent", active)
	}
	revoked, err := log.Credential(ctx, revokedKey)
	if err != nil {
		t.Fatalf("Credential for the revoked key: %v", err)
	}
	if revoked.State != projection.CredentialRevoked || revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(revokedAt) {
		t.Fatalf("Credential for the revoked key = %+v, want revoked with the recorded instant", revoked)
	}

	// A key the projection never heard of — every key minted before the
	// projection foundation — is an answer, not a failure.
	missing := mintProjectionID(t)
	if _, err := log.Credential(ctx, missing); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("Credential for an unprojected key returned %v, want persistence.ErrNotFound", err)
	}
}

func TestIntegrationTheAccountMirrorKeepsOneRowPerAccount(t *testing.T) {
	db, store, _, recorder := integrationProjection(t)
	at := projectionFixtureAt(t)
	accountID := mintProjectionID(t)

	for i, state := range []projection.AccountState{projection.AccountActive, projection.AccountSuspended, projection.AccountClosed} {
		recordProjectionAccount(t, store, recorder, at, projectionFixtureAccount(t, accountID, state))
		gotState, revision, present := projectionAccountMirror(t, db, accountID)
		if !present || gotState != string(state) {
			t.Fatalf("after recording %s the mirror = (%q, %v), want that state in one row", state, gotState, present)
		}
		if revision != uint64(i+1) {
			t.Fatalf("after recording %s the mirror names source_revision %d, want %d — the revision of that state, not an earlier one", state, revision, i+1)
		}
	}

	var mirrorRows int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM control.projection_accounts WHERE account_id = $1`, accountID).Scan(&mirrorRows); err != nil {
		t.Fatalf("count the account's mirror rows: %v", err)
	}
	if mirrorRows != 1 {
		t.Fatalf("the account has %d mirror rows, want 1 — three changes, one row, the last state", mirrorRows)
	}
	wantRevisionSequence(t, projectionLogRevisions(t, db), 3)
}

// ---------------------------------------------------------------------------
// The snapshot cut.
// ---------------------------------------------------------------------------

func TestIntegrationSnapshotCutsTheProjectionAtTheHeadItRead(t *testing.T) {
	db, store, log, recorder := integrationProjection(t)
	ctx := t.Context()
	accountID := mintProjectionID(t)
	digest := projectionFixtureDigest(t, "ab")
	otherDigest := projectionFixtureDigest(t, "cd")
	at := projectionFixtureAt(t)

	keyA := mintProjectionID(t)
	keyB := mintProjectionID(t)
	revokedAt := at.Add(-time.Hour)
	recordProjectionCredential(t, store, recorder, at,
		projectionFixtureCredential(t, keyA, accountID, digest, projection.CredentialActive, nil))
	recordProjectionAccount(t, store, recorder, at,
		projectionFixtureAccount(t, accountID, projection.AccountSuspended))
	recordProjectionCredential(t, store, recorder, at,
		projectionFixtureCredential(t, keyB, accountID, otherDigest, projection.CredentialRevoked, &revokedAt))

	head, err := log.Head(ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	snapshot, err := log.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Epoch != head.Epoch || snapshot.Revision != head.LastRevision {
		t.Fatalf("Snapshot cut at (%q, %d), want the counter it read: (%q, %d)", snapshot.Epoch, snapshot.Revision, head.Epoch, head.LastRevision)
	}
	states := map[string]projection.CredentialState{}
	for _, key := range snapshot.Keys {
		states[key.KeyID] = key.State
	}
	if len(states) != 2 || states[keyA] != projection.CredentialActive || states[keyB] != projection.CredentialRevoked {
		t.Fatalf("Snapshot keys = %v, want %s active and %s revoked", states, keyA, keyB)
	}
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].AccountID != accountID || snapshot.Accounts[0].State != projection.AccountSuspended {
		t.Fatalf("Snapshot accounts = %+v, want the one suspended account", snapshot.Accounts)
	}

	// The filter is the comparison, not the read's luck: a mirror row whose
	// source_revision is past the boundary — the shape a concurrent writer's
	// committed row has, hand-written here because a revision the log has not
	// issued cannot be produced through the port — belongs to the batches
	// after the snapshot, not to the snapshot.
	keyC := mintProjectionID(t)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO control.projection_api_keys (key_id, account_id, digest, state, revoked_at, source_revision, updated_at)
		 VALUES ($1, $2, $3, 'active', NULL, 999, $4)`, keyC, accountID, string(otherDigest), at); err != nil {
		t.Fatalf("seed the past-the-boundary mirror row: %v", err)
	}
	next, err := log.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot past the boundary row: %v", err)
	}
	if next.Revision != head.LastRevision {
		t.Fatalf("the second cut is at %d, want %d — the boundary is the head, not the newest row", next.Revision, head.LastRevision)
	}
	states = map[string]projection.CredentialState{}
	for _, key := range next.Keys {
		states[key.KeyID] = key.State
	}
	if len(states) != 2 || states[keyA] != projection.CredentialActive || states[keyB] != projection.CredentialRevoked {
		t.Fatalf("the second cut's keys = %v, want exactly the two rows at or before %d — %s belongs to what comes after", states, head.LastRevision, keyC)
	}
}

// ---------------------------------------------------------------------------
// The delivery page.
// ---------------------------------------------------------------------------

func TestIntegrationChangesAfterPagesTheLogInItsOwnOrder(t *testing.T) {
	_, store, log, recorder := integrationProjection(t)
	ctx := t.Context()
	accountID := mintProjectionID(t)
	digest := projectionFixtureDigest(t, "ab")
	at := projectionFixtureAt(t)

	keyA := mintProjectionID(t)
	keyB := mintProjectionID(t)
	revokedAt := at.Add(-time.Hour)

	// Seven entries across two credentials and one account — mint, suspend,
	// revoke, rotate, revoke — so every page boundary lands between kinds.
	recordProjectionCredential(t, store, recorder, at,
		projectionFixtureCredential(t, keyA, accountID, digest, projection.CredentialActive, nil)) // 1
	recordProjectionAccount(t, store, recorder, at,
		projectionFixtureAccount(t, accountID, projection.AccountActive)) // 2
	recordProjectionCredential(t, store, recorder, at,
		projectionFixtureCredential(t, keyA, accountID, digest, projection.CredentialRevoked, &revokedAt)) // 3
	recordProjectionAccount(t, store, recorder, at,
		projectionFixtureAccount(t, accountID, projection.AccountSuspended)) // 4
	recordProjectionCredential(t, store, recorder, at,
		projectionFixtureCredential(t, keyB, accountID, digest, projection.CredentialActive, nil)) // 5
	recordProjectionAccount(t, store, recorder, at,
		projectionFixtureAccount(t, accountID, projection.AccountClosed)) // 6
	recordProjectionCredential(t, store, recorder, at,
		projectionFixtureCredential(t, keyB, accountID, digest, projection.CredentialRevoked, &revokedAt)) // 7

	// Ascending, strictly after, bounded: the three clauses the delivery
	// loop's position arithmetic rests on.
	first, err := log.ChangesAfter(ctx, 0, 3)
	if err != nil {
		t.Fatalf("ChangesAfter(0, 3): %v", err)
	}
	if got := revisionsOf(first); !slices.Equal(got, []uint64{1, 2, 3}) {
		t.Fatalf("the first page = %v, want 1, 2, 3 ascending", got)
	}
	second, err := log.ChangesAfter(ctx, 3, 3)
	if err != nil {
		t.Fatalf("ChangesAfter(3, 3): %v", err)
	}
	if got := revisionsOf(second); !slices.Equal(got, []uint64{4, 5, 6}) {
		t.Fatalf("the second page = %v, want 4, 5, 6 — strictly after the position, not from it", got)
	}
	third, err := log.ChangesAfter(ctx, 6, 5)
	if err != nil {
		t.Fatalf("ChangesAfter(6, 5): %v", err)
	}
	if got := revisionsOf(third); !slices.Equal(got, []uint64{7}) {
		t.Fatalf("the third page = %v, want the drained remainder, one entry", got)
	}
	drained, err := log.ChangesAfter(ctx, 7, 3)
	if err != nil {
		t.Fatalf("ChangesAfter(7, 3): %v", err)
	}
	if len(drained) != 0 {
		t.Fatalf("the drained feed returned %v, want an empty page — drained is ordinary, not an error", drained)
	}

	// Every entry is decoded through the grammar on its way out, so the page
	// carries the row's full state: the digest verbatim, the paired instant.
	if first[0].Kind != projection.KindAPIKey || first[0].Credential().Digest != digest || first[0].Credential().State != projection.CredentialActive {
		t.Fatalf("entry 1 = (%s, %+v), want the minted key's payload decoded", first[0].Kind, first[0].Credential())
	}
	if first[1].Kind != projection.KindAccount || first[1].Account().State != projection.AccountActive {
		t.Fatalf("entry 2 = (%s, %+v), want the account's payload decoded", first[1].Kind, first[1].Account())
	}
	if first[2].Credential().State != projection.CredentialRevoked || first[2].Credential().RevokedAt == nil || !first[2].Credential().RevokedAt.Equal(revokedAt) {
		t.Fatalf("entry 3 = %+v, want revoked with the recorded instant read back from the payload", first[2].Credential())
	}
	if !third[0].RecordedAt.Equal(at) {
		t.Fatalf("entry 7's recorded instant = %v, want %v — carried for operators, not reordered", third[0].RecordedAt, at)
	}
}

func revisionsOf(changes []projection.Change) []uint64 {
	revisions := make([]uint64, len(changes))
	for i, change := range changes {
		revisions[i] = change.Revision
	}
	return revisions
}

func TestIntegrationChangesAfterRefusesALogRowTheGrammarCannotRead(t *testing.T) {
	db, _, log, _ := integrationProjection(t)
	ctx := t.Context()
	accountID := mintProjectionID(t)
	digest := projectionFixtureDigest(t, "ab")
	at := projectionFixtureAt(t)

	// Two rows no caller of this adapter could have written: an account
	// payload in a state the contract does not spell, and an api_key payload
	// whose digest is 64 hex characters but whose state is not one the
	// contract carries. The log is history, and history that cannot still be
	// decoded is refused, whole page — a skipped row is how a mirror loses a
	// change forever.
	archivedAccount := fmt.Sprintf(`{"state":"archived"}`)
	archivedKey := fmt.Sprintf(`{"account_id":%q,"digest":%q,"state":"archived","revoked_at":null}`, accountID, string(digest))
	for _, seed := range []struct {
		revision int
		kind     string
		payload  string
	}{
		{101, "account", archivedAccount},
		{102, "api_key", archivedKey},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO control.projection_changes (revision, resource_kind, resource_id, recorded_at, payload)
			 VALUES ($1, $2, $3, $4, $5)`, seed.revision, seed.kind, accountID, at, seed.payload); err != nil {
			t.Fatalf("seed the undecodable log row at revision %d: %v", seed.revision, err)
		}
	}

	if page, err := log.ChangesAfter(ctx, 100, 10); err == nil {
		t.Fatalf("ChangesAfter over the undecodable rows = %v, want the whole page refused", page)
	}
	// Refused whole means no partial page either: each row alone still kills
	// the page it opens.
	if _, err := log.ChangesAfter(ctx, 100, 1); err == nil {
		t.Fatal("the page holding only revision 101 was delivered — an undecodable row must refuse its own page")
	}
	if _, err := log.ChangesAfter(ctx, 101, 1); err == nil {
		t.Fatal("the page holding only revision 102 was delivered")
	}

	// And the schema is the first guard: the shapes this test seeds by hand
	// for the api_key half cannot even reach an unknown kind, and a digest
	// outside its hex form cannot reach the log at all.
	_, err := db.ExecContext(ctx,
		`INSERT INTO control.projection_changes (revision, resource_kind, resource_id, recorded_at, payload)
		 VALUES (201, 'tenant', $1, $2, '{"state":"active"}'::jsonb)`, mintProjectionID(t), at)
	if err == nil {
		t.Fatal("a log row of a resource kind the protocol does not carry was stored — the kind set's CHECK did not guard")
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO control.projection_changes (revision, resource_kind, resource_id, recorded_at, payload)
		 VALUES (202, 'api_key', $1, $2, $3)`, mintProjectionID(t), at,
		`{"account_id":"`+accountID+`","digest":"nothex","state":"active","revoked_at":null}`)
	if err == nil {
		t.Fatal("an api_key payload whose digest is not 64 hex characters was stored — the payload's CHECK did not guard")
	}
}

// ---------------------------------------------------------------------------
// The foundation itself.
// ---------------------------------------------------------------------------

func TestIntegrationTheFoundationLeavesACountableTimelineBehind(t *testing.T) {
	db, _, log, _ := integrationProjection(t)
	ctx := t.Context()

	// One counter row, ever: the singleton the migration creates, whose
	// existence is the difference between a timeline and a database that was
	// never migrated.
	if rows := projectionCounterRows(t, db); rows != 1 {
		t.Fatalf("the counter holds %d rows, want exactly 1", rows)
	}

	// The migration backfills accounts only, at migration time — keys have no
	// digest to backfill, by the two-record boundary working as designed. On
	// this fixture, migrated before any of this suite's records, the honest
	// observable of that backfill is its zero case: an empty log, an empty
	// projection, a counter at zero. (The seeded backfill — two accounts
	// entering at revisions 1 and 2 — needs a database migrated from a
	// pre-populated identity state, which this shared fixture is not.)
	head, err := log.Head(ctx)
	if err != nil {
		t.Fatalf("Head over the fresh foundation: %v", err)
	}
	if head.LastRevision != 0 || head.Epoch == "" {
		t.Fatalf("Head = %+v, want revision 0 under a named timeline", head)
	}
	if entries := projectionLogEntries(t, db); len(entries) != 0 {
		t.Fatalf("the fresh foundation's log holds %v, want none", entries)
	}

	// An empty projection is a snapshot too, and zero is a legitimate
	// boundary: the cut marshals as a legal document — arrays, not nulls —
	// that a consumer can apply unconditionally.
	snapshot, err := log.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot over the fresh foundation: %v", err)
	}
	if snapshot.Revision != 0 || len(snapshot.Keys) != 0 || len(snapshot.Accounts) != 0 {
		t.Fatalf("Snapshot = (%d, %d keys, %d accounts), want the empty cut at boundary 0",
			snapshot.Revision, len(snapshot.Keys), len(snapshot.Accounts))
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal the empty snapshot: %v", err)
	}
	var document struct {
		ProtocolVersion  int    `json:"protocol_version"`
		Epoch            string `json:"epoch"`
		SnapshotRevision uint64 `json:"snapshot_revision"`
		APIKeys          []any  `json:"api_keys"`
		Accounts         []any  `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("unmarshal the empty snapshot document: %v", err)
	}
	if document.ProtocolVersion != projection.ProtocolVersion || document.SnapshotRevision != 0 ||
		document.Epoch != head.Epoch || document.APIKeys == nil || document.Accounts == nil ||
		len(document.APIKeys) != 0 || len(document.Accounts) != 0 {
		t.Fatalf("the empty snapshot document = %s, want protocol %d, the timeline's epoch, boundary 0, and two empty arrays", raw, projection.ProtocolVersion)
	}
}
