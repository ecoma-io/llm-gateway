//go:build integration

package main

// The admission use case composed against the real adapters and a real
// PostgreSQL — the suite the adapter tree cannot host. The arch rules keep
// internal/application out of internal/adapters/outbound (an outbound adapter
// is constructed at the composition root and by nothing else), so the
// end-to-end proofs live here, beside the production wiring they mirror: the
// composition below is bind()'s, statement for statement, over one pool.
//
// What these tests prove is the use case's own behaviour over the engine's
// guarantees — that Serve never oversubscribes a grant under concurrency, that
// the replay record's unique key turns a racing field of arrivals into one
// admission, that a mid-unit failure leaves the account byte-identical, that
// every fate the record can carry replays to the decision that wrote it, and
// that credential verification refuses without writing a row. The store-level
// halves of several of these invariants (the conditional drawdown's atomicity,
// the Close CAS, the intake unique) are pinned directly against the same
// fixture by internal/adapters/outbound/postgres/admission_integration_test.go;
// this file is what the use case adds on top.
//
// The suite is self-contained by necessity — the landed integration harness
// (integration_test.go, runtime_storage_test.go) is unexported inside the
// postgres package — so it carries its own compact fixture: DSN derivation,
// migration apply, catalog scaffolding, and the SQL residue reads its
// assertions need. The fixture protocol is the same one: the shared compose
// project, the `dataplane` database, unique-per-run accounts, and no
// truncation of anything a sibling suite may be reading.
//
// Run (from apps/dataplane):
//
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:55443/postgres?sslmode=disable' \
//	  go test -tags=integration ./cmd/dataplane

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/adapters/outbound/postgres"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The fixture's identity: the plane database this application owns (postgres.
// Open refuses any other) and the schema lock this suite serialises its apply
// through — a key of its own, so the postgres package's suites and this one
// queue behind separate locks rather than surprising each other.
const (
	admissionPlaneDatabase = "dataplane"
	admissionSchemaLockKey = 700010
)

// admissionAdminDSN reads the admin DSN the fixture answers on.
func admissionAdminDSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_ADMIN_DSN")
	if dsn == "" {
		t.Fatal("POSTGRES_TEST_ADMIN_DSN is required for the admission suite; start the fixture (docker compose -f deploy/postgres/compose.yaml up -d --wait) and set it to postgres://gateway:gateway-dev-only@127.0.0.1:55443/postgres?sslmode=disable")
	}
	return dsn
}

// admissionPlaneDSN derives the `dataplane` DSN from the admin one, creating
// the database if the cluster lacks it — the same derivation the landed
// harness performs, because the suites must answer to one fixture protocol.
func admissionPlaneDSN(t testing.TB) string {
	t.Helper()
	admin, err := sql.Open("pgx", admissionAdminDSN(t))
	if err != nil {
		t.Fatalf("opening the admin DSN: %v", err)
	}
	defer func() { _ = admin.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("the admin DSN could not reach the server: %v — start the fixture first", err)
	}
	var exists bool
	if err := admin.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", admissionPlaneDatabase).Scan(&exists); err != nil {
		t.Fatalf("looking up the %s database: %v", admissionPlaneDatabase, err)
	}
	if !exists {
		if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+admissionPlaneDatabase); err != nil {
			t.Fatalf("CREATE DATABASE %s: %v", admissionPlaneDatabase, err)
		}
	}
	return strings.Replace(admissionAdminDSN(t), "/postgres?", "/"+admissionPlaneDatabase+"?", 1)
}

// admissionPool opens the pool the way bind() does — through postgres.Open,
// which refuses a DSN naming any database but the plane's.
func admissionPool(t testing.TB, dsn string, maxOpen int) (*sql.DB, persistence.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := postgres.Open(ctx, postgres.Options{
		DSN:             dsn,
		MaxOpenConns:    maxOpen,
		MaxIdleConns:    maxOpen,
		ConnMaxLifetime: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("opening the plane pool: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, postgres.New(db)
}

// admissionRepositoryRoot finds the checkout root the migration lane lives
// under — the same walk the landed harness runs, because these tests apply
// the migration files themselves and a checkout is the only place they exist.
func admissionRepositoryRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("finding the working directory: %v", err)
	}
	for {
		if info, statErr := os.Stat(filepath.Join(dir, "migrations", "dataplane")); statErr == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no ancestor of %s contains migrations/dataplane — the admission suite applies the migration files themselves and cannot run outside a repository checkout", dir)
		}
		dir = parent
	}
}

// admissionSchema brings the pool's database to the lane's newest version by
// hand: golang-migrate's discipline (one row in public.schema_migrations, a
// dirty flag that stops the run, one transaction per file) under a
// session-level advisory lock held for the apply.
func admissionSchema(t testing.TB, db *sql.DB) {
	t.Helper()
	lane := filepath.Join(admissionRepositoryRoot(t), "migrations", "dataplane")
	entries, err := os.ReadDir(lane)
	if err != nil {
		t.Fatalf("reading the migration lane %s: %v", lane, err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".up.sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("taking a connection for the schema lock: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", admissionSchemaLockKey); err != nil {
		t.Fatalf("taking the schema advisory lock: %v", err)
	}
	t.Cleanup(func() {
		unlockCtx, cancelUnlock := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelUnlock()
		_, _ = conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", admissionSchemaLockKey)
	})

	if _, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS public.schema_migrations (version bigint NOT NULL, dirty boolean NOT NULL)"); err != nil {
		t.Fatalf("ensuring public.schema_migrations: %v", err)
	}
	rows, err := conn.QueryContext(ctx, "SELECT version, dirty FROM public.schema_migrations")
	if err != nil {
		t.Fatalf("reading public.schema_migrations: %v", err)
	}
	var applied []struct {
		version int64
		dirty   bool
	}
	for rows.Next() {
		var one struct {
			version int64
			dirty   bool
		}
		if err := rows.Scan(&one.version, &one.dirty); err != nil {
			rows.Close()
			t.Fatalf("scanning public.schema_migrations: %v", err)
		}
		applied = append(applied, one)
	}
	rows.Close()
	if len(applied) > 1 {
		t.Fatalf("public.schema_migrations carries %d rows — golang-migrate keeps exactly one", len(applied))
	}
	var current int64
	if len(applied) == 1 {
		if applied[0].dirty {
			t.Fatalf("public.schema_migrations is dirty at version %d — recover it before running this suite", applied[0].version)
		}
		current = applied[0].version
	}

	for _, name := range files {
		version, err := strconv.Atoi(name[:6])
		if err != nil {
			t.Fatalf("migration file %q does not carry the leading six-digit version golang-migrate's naming requires", name)
		}
		if int64(version) <= current {
			continue
		}
		body, err := os.ReadFile(filepath.Join(lane, name))
		if err != nil {
			t.Fatalf("reading migration %s: %v", name, err)
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("opening the transaction for migration %s: %v", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("applying migration %s: %v", name, err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE public.schema_migrations SET version = $1, dirty = false", version); err != nil {
			_ = tx.Rollback()
			t.Fatalf("recording migration %s: %v", name, err)
		}
		if len(applied) == 0 {
			if _, err := tx.ExecContext(ctx, "INSERT INTO public.schema_migrations (version, dirty) VALUES ($1, false)", version); err != nil {
				_ = tx.Rollback()
				t.Fatalf("recording migration %s: %v", name, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("committing migration %s: %v", name, err)
		}
		current = int64(version)
	}
}

// admissionFixture is one test's world: the pool, the store, the use case
// composed over it exactly as bind() composes it, and the catalog scaffolding
// the waterfall's eligibility predicate reads.
type admissionFixture struct {
	db         *sql.DB
	store      persistence.Store
	admission  *application.ChatAdmission
	auth       application.Authenticator
	aliases    persistence.ModelAliases
	prices     persistence.PriceBook
	requests   persistence.RequestRepository
	intakes    persistence.IntakeRepository
	reserves   persistence.ReservationRepository
	ledger     persistence.QuotaProjectionRepository
	aliasID    catalog.AliasID
	aliasName  string
	wildcardID string
	namedID    string
}

// admissionFixtureAliasName is this suite's own alias name — distinct from
// the landed harness's, so the two suites' scaffolding never converge on one
// row by accident.
const admissionFixtureAliasName = "b8c3-serve-alias"

// newAdmissionFixture assembles the whole world: plane DSN, pool, schema, the
// bind()-shaped composition, and the catalog scope.
func newAdmissionFixture(t *testing.T, maxOpen int) *admissionFixture {
	t.Helper()
	db, store := admissionPool(t, admissionPlaneDSN(t), maxOpen)
	admissionSchema(t, db)

	fixture := &admissionFixture{
		db:       db,
		store:    store,
		aliases:  postgres.NewModelAliases(store),
		prices:   postgres.NewPriceBook(store),
		requests: postgres.NewRequestRepository(store),
		intakes:  postgres.NewIntakeRepository(store),
		reserves: postgres.NewReservationRepository(store),
		ledger:   postgres.NewQuotaProjectionRepository(store),
	}
	fixture.admission = application.NewChatAdmission(
		store,
		postgres.NewCredentials(store),
		fixture.aliases,
		fixture.prices,
		fixture.requests,
		fixture.intakes,
		fixture.reserves,
		fixture.ledger,
		postgres.NewFactRepository(store),
		application.AdmissionConfig{
			HoldWindow: time.Hour,
			LeaseTTL:   30 * time.Minute,
			LeaseOwner: "b8c3-admission-tests",
		},
	)
	fixture.auth = application.NewCredentialAuthenticator(postgres.NewCredentials(store))
	fixture.seedCatalog(t)
	return fixture
}

// admissionSeedCatalog lays the scaffolding down: the named alias, the
// wildcard `*` group version the schema keeps as its singleton, and a named
// group version holding the alias — the shapes a grant's stored scope
// resolves through, and the reason a seeded grant is one the waterfall may
// actually draw. Idempotent by convergence: the shared database keeps its
// rows, so re-seeding adopts what earlier runs minted.
func admissionSeedCatalog(t testing.TB, store persistence.Store, aliasName string) (aliasID catalog.AliasID, wildcardID, namedID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	querier := store.Querier(ctx)
	if _, err := querier.ExecContext(ctx, `INSERT INTO model_aliases (id, name, state, max_output_tokens, reservation_cap, created_at, updated_at)
VALUES ($1, $2, 'active', 4096, 4096, transaction_timestamp(), transaction_timestamp())
ON CONFLICT (name) DO NOTHING`, string(identity.NewRequestID()), aliasName); err != nil {
		t.Fatalf("seeding the alias %q: %v", aliasName, err)
	}
	if err := querier.QueryRowContext(ctx, `SELECT id FROM model_aliases WHERE name = $1`, aliasName).Scan(&aliasID); err != nil {
		t.Fatalf("reading the alias id: %v", err)
	}
	if _, err := querier.ExecContext(ctx, `INSERT INTO alias_group_versions (id, group_name, version, created_at)
VALUES ($1, '*', 1, transaction_timestamp())
ON CONFLICT DO NOTHING`, string(identity.NewRequestID())); err != nil {
		t.Fatalf("seeding the wildcard group version: %v", err)
	}
	if err := querier.QueryRowContext(ctx, `SELECT id FROM alias_group_versions WHERE group_name = '*'`).Scan(&wildcardID); err != nil {
		t.Fatalf("reading the wildcard group version: %v", err)
	}
	namedID = string(identity.NewRequestID())
	if _, err := querier.ExecContext(ctx, `INSERT INTO alias_group_versions (id, group_name, version, created_at)
VALUES ($1, $2, 1, transaction_timestamp())`, namedID, "b8c3-named-group-"+namedID[len(namedID)-8:]); err != nil {
		t.Fatalf("seeding a named group version: %v", err)
	}
	if _, err := querier.ExecContext(ctx, `INSERT INTO alias_group_members (group_version_id, alias_id)
VALUES ($1, $2)`, namedID, string(aliasID)); err != nil {
		t.Fatalf("seeding the named group version's membership: %v", err)
	}
	return aliasID, wildcardID, namedID
}

// admissionSeedPrice mints one activated revision pricing the given alias,
// reading the version and the effective instant from the shared history so
// the seed converges against a database that already carries revisions.
func admissionSeedPrice(t testing.TB, ctx context.Context, store persistence.Store, aliasID catalog.AliasID, input, output int64) string {
	t.Helper()
	querier := store.Querier(ctx)
	var version int
	if err := querier.QueryRowContext(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM client_price_list_revisions`).Scan(&version); err != nil {
		t.Fatalf("reading the next price list version: %v", err)
	}
	effectiveFrom := time.Now().UTC().Add(-time.Hour)
	var latest time.Time
	switch err := querier.QueryRowContext(ctx, `SELECT effective_from FROM client_price_list_revisions
WHERE state = 'activated' ORDER BY effective_from DESC LIMIT 1`).Scan(&latest); {
	case err == nil:
		// The activated-instant partial unique admits one revision per
		// instant; step past the latest one when it is in the way.
		if !latest.Before(effectiveFrom) {
			effectiveFrom = latest.Add(time.Microsecond)
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		t.Fatalf("reading the latest activated effective_from: %v", err)
	}
	revisionID := string(identity.NewRequestID())
	if _, err := querier.ExecContext(ctx, `INSERT INTO client_price_list_revisions (id, version, state, effective_from, activated_at, created_at)
VALUES ($1, $2, 'activated', $3, $3, transaction_timestamp())`, revisionID, version, effectiveFrom); err != nil {
		t.Fatalf("seeding revision v%d: %v", version, err)
	}
	if _, err := querier.ExecContext(ctx, `INSERT INTO client_price_list_entries (revision_id, alias_id, input_unit_price, output_unit_price)
VALUES ($1, $2, $3, $4)`, revisionID, string(aliasID), input, output); err != nil {
		t.Fatalf("seeding the revision's entry: %v", err)
	}
	return revisionID
}

// seedCatalog lays the suite's scaffolding down through the standalone
// seeder, keeping the fixture's own fields beside the rows.
func (f *admissionFixture) seedCatalog(t testing.TB) {
	t.Helper()
	aliasID, wildcardID, namedID := admissionSeedCatalog(t, f.store, admissionFixtureAliasName)
	f.aliasID, f.wildcardID, f.namedID = aliasID, wildcardID, namedID
	f.aliasName = admissionFixtureAliasName
}

// admissionAccount mints one account id unique to this run — the key every
// write in these scenarios carries, and the reason the shared fixture
// database never needs truncating.
func admissionAccount(t testing.TB) string {
	t.Helper()
	return "b8c3-account-" + string(identity.NewRequestID())
}

// seedGrant publishes one grant through the real quota projection repository —
// the same statement a Control Plane publication arrives through. A zero
// period end publishes the PAYG scope; a non-zero one an entitlement cycle.
// The scope resolves through the suite's scaffolding: a named grant pins the
// named version holding the alias, an unscoped one the wildcard.
func (f *admissionFixture) seedGrant(t testing.TB, ctx context.Context, account, bucket string, namedScope bool, periodEnd time.Time, limit int64) {
	t.Helper()
	versionID := f.wildcardID
	if namedScope {
		versionID = f.namedID
	}
	publication := accounting.Publication{
		AccountID:             account,
		FundingBucketID:       bucket,
		AliasGroupVersionID:   versionID,
		ScopeKind:             accounting.ScopePayGBalance,
		NamedScope:            namedScope,
		PeriodEnd:             periodEnd,
		SubscriptionCreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		State:                 accounting.ProjectionActive,
		LimitAmount:           limit,
		Revision:              1,
	}
	if !periodEnd.IsZero() {
		publication.ScopeKind = accounting.ScopeEntitlementCycle
		publication.EntitlementID = "b8c3-entitlement-" + bucket
		publication.CycleNumber = 1
	}
	if _, err := f.ledger.ApplyPublication(ctx, publication); err != nil {
		t.Fatalf("ApplyPublication(%s): %v", bucket, err)
	}
}

// seedPrice mints one activated revision pricing the suite's alias, through
// the standalone seeder.
func (f *admissionFixture) seedPrice(t testing.TB, ctx context.Context, input, output int64) string {
	t.Helper()
	return admissionSeedPrice(t, ctx, f.store, f.aliasID, input, output)
}

// admissionChatBody is the request body the suite's arrivals carry.
type admissionChatBody struct {
	Model     string                 `json:"model"`
	MaxTokens int                    `json:"max_tokens"`
	Messages  []admissionChatMessage `json:"messages"`
}

type admissionChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// admissionBody renders one valid body: a model naming the suite's alias (or
// any name the test chooses), a ceiling of maxTokens, and one content part —
// whose byte length is the interim input-token count, so "b8c3" prices a hold
// of ceil((4·input + 1·output)/1e6) = 1 minor unit at any small prices.
func admissionBody(t testing.TB, model string, maxTokens int, content string) []byte {
	t.Helper()
	raw, err := json.Marshal(admissionChatBody{
		Model:     model,
		MaxTokens: maxTokens,
		Messages:  []admissionChatMessage{{Role: "user", Content: content}},
	})
	if err != nil {
		t.Fatalf("rendering a body: %v", err)
	}
	return raw
}

// admissionInput is one ChatInput as the transport would hand it in: the
// verified identity, the runtime-minted request id, the key, and the body
// bytes.
func admissionInput(requestID identity.RequestID, accountID, credential, key string, accountState *string, body []byte) application.ChatInput {
	return application.ChatInput{
		RequestID:      requestID,
		Credential:     credential,
		AccountID:      accountID,
		AccountState:   accountState,
		IdempotencyKey: key,
		Body:           application.BodyBytes(body),
	}
}

// admissionServeState is the account-lifecycle pointer the verified
// credential carries. Every scenario here serves against an active account
// unless it says otherwise.
func admissionServeState(state string) *string { return &state }

// admissionServe runs Serve and fails the test on a non-nil error — every
// decision admission can reach arrives with a nil error, so an error here is
// the test's own malfunction unless the scenario says otherwise.
func admissionServe(t testing.TB, admission *application.ChatAdmission, in application.ChatInput) application.ChatOutcome {
	t.Helper()
	outcome, err := admission.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("Serve(%s) error = %v — a decision reaches the caller with a nil error, always", in.RequestID, err)
	}
	return outcome
}

// The residue reads. One account's rows are the whole observable: the request
// rows it owns, the holds joined through them, the legs' total, the replay
// records, and the balance a failed unit must leave byte-identical.

func (f *admissionFixture) residue(t testing.TB, account string) (requests, reservations int, legSum int64, intakes, rejected int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reads := []struct {
		query string
		dest  any
	}{
		{"SELECT count(*) FROM public.requests WHERE account_id = $1", &requests},
		{`SELECT count(*) FROM public.reservations r
		  JOIN public.requests rq ON rq.id = r.request_id
		  WHERE rq.account_id = $1`, &reservations},
		{`SELECT COALESCE(sum(l.amount), 0) FROM public.reservation_allocations l
		  JOIN public.reservations r ON r.id = l.reservation_id
		  JOIN public.requests rq ON rq.id = r.request_id
		  WHERE rq.account_id = $1`, &legSum},
		{"SELECT count(*) FROM public.request_intake WHERE account_id = $1", &intakes},
		{"SELECT count(*) FROM public.requests WHERE account_id = $1 AND status = 'rejected'", &rejected},
	}
	for _, read := range reads {
		if err := f.db.QueryRowContext(ctx, read.query, account).Scan(read.dest); err != nil {
			t.Fatalf("counting the residue for %s: %v", account, err)
		}
	}
	return requests, reservations, legSum, intakes, rejected
}

func (f *admissionFixture) balance(t testing.TB, bucket string) (available, limitAmount int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.db.QueryRowContext(ctx,
		`SELECT available, limit_amount FROM public.quota_projections WHERE funding_bucket_id = $1`, bucket).Scan(&available, &limitAmount); err != nil {
		t.Fatalf("reading the balance of %s: %v", bucket, err)
	}
	return available, limitAmount
}

func (f *admissionFixture) rejectedRows(t testing.TB, account, reason string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM public.requests WHERE account_id = $1 AND status = 'rejected' AND rejection_reason = $2`, account, reason).Scan(&count); err != nil {
		t.Fatalf("counting the %s rows of %s: %v", reason, account, err)
	}
	return count
}

func (f *admissionFixture) intakeOf(t testing.TB, account, key string) (requestID string, finalStatus sql.NullString, finalReason sql.NullString) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.db.QueryRowContext(ctx,
		`SELECT request_id, final_status, final_rejection_reason FROM public.request_intake
		 WHERE account_id = $1 AND idempotency_key = $2`, account, key).Scan(&requestID, &finalStatus, &finalReason); err != nil {
		t.Fatalf("reading the replay record of (%s, %s): %v", account, key, err)
	}
	return requestID, finalStatus, finalReason
}

func (f *admissionFixture) factCount(t testing.TB, requestID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM public.usage_events WHERE request_id = $1 AND kind IN ('settled', 'released', 'expired')`, requestID).Scan(&count); err != nil {
		t.Fatalf("counting the facts of %s: %v", requestID, err)
	}
	return count
}

// TestIntegrationAdmissionNeverOversubscribesUnderConcurrency races N
// distinct arrivals over one grant and holds the invariant the build makes
// observable at the use case's altitude: conservation. This milestone's seam
// releases every admitted hold immediately, so capacity re-funds while the
// race runs and a deterministic N-1 refusal cannot be forced through Serve —
// that proof, holds persisting open against an exactly-exhausted grant, is
// the store-level test against the same fixture
// (TestIntegrationConcurrentAdmissionUnitsCannotOversubscribeTheGrant in
// internal/adapters/outbound/postgres). What the use case must answer for is
// the whole round: at every committed instant available is never negative and
// available + open legs equals the capacity exactly — capacity is never
// created, never lost — every arrival reaches a decision and is recorded once,
// every hold that was opened is released exactly once, and the grant ends the
// round as whole as it began. Three rounds shake the scheduler; the -race
// lane runs the same test under the race detector.
func TestIntegrationAdmissionNeverOversubscribesUnderConcurrency(t *testing.T) {
	const arrivals = 24
	capacity := int64(arrivals - 1)
	fixture := newAdmissionFixture(t, 8)
	fixture.seedPrice(t, context.Background(), 1, 1)

	for round := 1; round <= 3; round++ {
		account := admissionAccount(t)
		bucket := account + "-bucket"
		credential := account + "-api-key"
		fixture.seedGrant(t, context.Background(), account, bucket, true, time.Now().UTC().Add(24*time.Hour), capacity)
		if available, limit := fixture.balance(t, bucket); available != capacity || limit != capacity {
			t.Fatalf("round %d: seeded balance (%d/%d), want (%d/%d)", round, available, limit, capacity, capacity)
		}

		// Distinct arrivals: each carries its own idempotency key, because the
		// invariant under test is capacity, not the replay record's unique key
		// — that one is TestIntegrationAdmissionAnswersOnceForOneKeyUnderARace.
		bodies := make([][]byte, arrivals)
		keys := make([]string, arrivals)
		ids := make([]identity.RequestID, arrivals)
		for i := range bodies {
			bodies[i] = admissionBody(t, fixture.aliasName, 1, "b8c3")
			keys[i] = fmt.Sprintf("%s-arrival-%d", account, i)
			ids[i] = identity.NewRequestID()
		}

		// The sampler reads the conservation equation in ONE statement —
		// available and the open legs share the statement's snapshot — so each
		// sample is a committed instant judged whole. A shortage is a
		// legitimate outcome here: when every other hold is open at an
		// arrival's drawdown, the walk answers a shortage, and conservation
		// must survive that too.
		stop := make(chan struct{})
		samplerDone := make(chan struct{})
		var (
			samplerMu     sync.Mutex
			samplerErr    error
			samples       int
			violationText string
		)
		go func() {
			defer close(samplerDone)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for {
				select {
				case <-stop:
					return
				case <-time.After(2 * time.Millisecond):
				}
				var (
					available int64
					openLegs  int64
				)
				err := fixture.db.QueryRowContext(ctx, `SELECT
				  (SELECT available FROM public.quota_projections WHERE funding_bucket_id = $1),
				  COALESCE(sum(l.amount), 0)
				FROM public.reservations r
				JOIN public.requests rq ON rq.id = r.request_id
				JOIN public.reservation_allocations l ON l.reservation_id = r.id
				WHERE rq.account_id = $2 AND r.state = 'open'`, bucket, account).Scan(&available, &openLegs)
				if err != nil {
					samplerMu.Lock()
					samplerErr = err
					samplerMu.Unlock()
					return
				}
				samplerMu.Lock()
				samples++
				if available < 0 {
					violationText = fmt.Sprintf("available = %d below zero", available)
				} else if available+openLegs != capacity {
					violationText = fmt.Sprintf("available %d + open legs %d = %d, want the capacity %d — capacity was created or lost mid-race", available, openLegs, available+openLegs, capacity)
				}
				samplerMu.Unlock()
			}
		}()

		outcomes := make([]application.ChatOutcome, arrivals)
		errs := make([]error, arrivals)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < arrivals; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				outcomes[i], errs[i] = fixture.admission.Serve(context.Background(),
					admissionInput(ids[i], account, credential, keys[i], admissionServeState("active"), bodies[i]))
			}(i)
		}
		close(start)
		wg.Wait()
		close(stop)
		<-samplerDone
		if samplerErr != nil {
			t.Fatalf("round %d: the sampler failed: %v", round, samplerErr)
		}
		samplerMu.Lock()
		observedSamples, violation := samples, violationText
		samplerMu.Unlock()
		if violation != "" {
			t.Errorf("round %d: the sampler held %d instants and the conservation broke: %s", round, observedSamples, violation)
		}

		var (
			released int
			shortage int
			wrong    []string
			firstErr error
		)
		for i := range outcomes {
			switch {
			case errs[i] != nil:
				if firstErr == nil {
					firstErr = fmt.Errorf("arrival %d: %w", i, errs[i])
				}
			case outcomes[i].Kind == application.OutcomeRejected && outcomes[i].Reason == execution.RejectedNoCandidate:
				released++
			case outcomes[i].Kind == application.OutcomeRejected && outcomes[i].Reason == execution.RejectedInsufficientEntitlement:
				shortage++
			default:
				wrong = append(wrong, fmt.Sprintf("arrival %d: kind %q reason %q", i, outcomes[i].Kind, outcomes[i].Reason))
			}
		}
		if firstErr != nil {
			t.Fatalf("round %d: an admission failed: %v", round, firstErr)
		}
		if len(wrong) != 0 {
			t.Errorf("round %d: unexpected outcomes %v — a distinct-key arrival is answered admitted or refused, never replayed", round, wrong)
		}
		admitted := released + shortage
		if admitted != arrivals {
			t.Errorf("round %d: decided arrivals = %d of %d — every arrival reaches a decision", round, admitted, arrivals)
		}

		// The written memory of the round: every arrival decided and recorded,
		// one hold per admitted arrival released exactly once, the feed
		// carrying exactly that many released facts, and the grant restored
		// whole.
		requests, reservations, legSum, intakes, _ := fixture.residue(t, account)
		if requests != arrivals || intakes != arrivals {
			t.Errorf("round %d: rows read %d requests and %d records, want a decision for each of %d arrivals", round, requests, intakes, arrivals)
		}
		if reservations != admitted {
			t.Errorf("round %d: holds on the record = %d, want one per admitted arrival (%d), never more", round, reservations, admitted)
		}
		if legSum != int64(admitted) {
			t.Errorf("round %d: taken capacity across the legs = %d, want one unit per admitted hold (%d)", round, legSum, admitted)
		}
		if available, limit := fixture.balance(t, bucket); available != capacity || limit != capacity {
			t.Errorf("round %d: balance reads (%d/%d), want the grant restored whole (%d/%d) — every released hold gave its units back", round, available, limit, capacity, capacity)
		}
		var facts int
		if err := fixture.db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM public.usage_events e
			 JOIN public.requests rq ON rq.id = e.request_id
			 WHERE rq.account_id = $1 AND e.kind = 'released'`, account).Scan(&facts); err != nil {
			t.Fatalf("round %d: counting the released facts: %v", round, err)
		}
		if facts != released {
			t.Errorf("round %d: released facts = %d, want one per admitted-and-released arrival (%d)", round, facts, released)
		}
	}
}

// TestIntegrationAdmissionRefusesNoAccessWhenNothingIsEligible pins the scope
// answer: an account with no grant eligible to fund the alias is refused
// no_access, concurrently and individually, and the refusal pair is all the
// written memory — no hold, no leg, no balance, because there was none to
// draw.
func TestIntegrationAdmissionRefusesNoAccessWhenNothingIsEligible(t *testing.T) {
	const arrivals = 8
	fixture := newAdmissionFixture(t, 8)
	fixture.seedPrice(t, context.Background(), 1, 1)
	account := admissionAccount(t)
	credential := account + "-api-key"
	// No grant seeded on purpose: the waterfall has nothing eligible to see.

	outcomes := make([]application.ChatOutcome, arrivals)
	errs := make([]error, arrivals)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < arrivals; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			outcomes[i], errs[i] = fixture.admission.Serve(context.Background(),
				admissionInput(identity.NewRequestID(), account, credential, fmt.Sprintf("%s-arrival-%d", account, i), admissionServeState("active"),
					admissionBody(t, fixture.aliasName, 1, "b8c3")))
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range outcomes {
		if errs[i] != nil {
			t.Fatalf("arrival %d: %v", i, errs[i])
		}
		if outcomes[i].Kind != application.OutcomeRejected || outcomes[i].Reason != execution.RejectedNoAccess {
			t.Errorf("arrival %d: outcome %q/%q, want rejected/no_access — no eligible row seen is a scope answer, not a shortage", i, outcomes[i].Kind, outcomes[i].Reason)
		}
	}
	requests, reservations, legSum, intakes, rejected := fixture.residue(t, account)
	if requests != arrivals || intakes != arrivals || rejected != arrivals {
		t.Errorf("residue reads %d requests (%d rejected) and %d records, want the refusal pair for each of %d arrivals", requests, rejected, intakes, arrivals)
	}
	if reservations != 0 || legSum != 0 {
		t.Errorf("residue reads %d holds carrying %d units, want none — a scope answer draws nothing", reservations, legSum)
	}
}

// TestIntegrationAdmissionAnswersOnceForOneKeyUnderARace races M arrivals
// sharing one (account, idempotency key, body): exactly one admission lands,
// and every other arrival is answered — in flight, or replayed from the
// committed decision — never a second reservation, never an error. The
// residue is the winner's one request, one hold, one record, one fact.
func TestIntegrationAdmissionAnswersOnceForOneKeyUnderARace(t *testing.T) {
	const (
		arrivals = 12
		capacity = int64(1000)
	)
	fixture := newAdmissionFixture(t, 8)
	fixture.seedPrice(t, context.Background(), 1, 1)
	account := admissionAccount(t)
	bucket := account + "-bucket"
	key := account + "-shared-key"
	credential := account + "-api-key"
	fixture.seedGrant(t, context.Background(), account, bucket, true, time.Now().UTC().Add(24*time.Hour), capacity)
	body := admissionBody(t, fixture.aliasName, 1, "b8c3")

	outcomes := make([]application.ChatOutcome, arrivals)
	errs := make([]error, arrivals)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < arrivals; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// Distinct request identities, one shared key: the record's unique
			// key is what arbitrates, exactly as it would for a client racing
			// retries.
			outcomes[i], errs[i] = fixture.admission.Serve(context.Background(),
				admissionInput(identity.NewRequestID(), account, credential, key, admissionServeState("active"), body))
		}(i)
	}
	close(start)
	wg.Wait()

	var (
		fresh    int
		inFlight int
		replays  int
		firstErr error
	)
	for i := range outcomes {
		switch {
		case errs[i] != nil:
			if firstErr == nil {
				firstErr = fmt.Errorf("arrival %d: %w", i, errs[i])
			}
		case outcomes[i].Kind == application.OutcomeRejected && outcomes[i].Reason == execution.RejectedNoCandidate:
			fresh++
		case outcomes[i].Kind == application.OutcomeInFlight:
			inFlight++
		case outcomes[i].Kind == application.OutcomeReplay && outcomes[i].Reason == execution.RejectedNoCandidate:
			replays++
		default:
			t.Errorf("arrival %d: outcome %q/%q — a racing arrival is answered fresh, in flight, or as a replay, nothing else", i, outcomes[i].Kind, outcomes[i].Reason)
		}
	}
	if firstErr != nil {
		t.Fatalf("an arrival errored: %v", firstErr)
	}
	if fresh != 1 {
		t.Errorf("arrivals answered with the fresh admission = %d, want exactly 1", fresh)
	}
	if fresh+inFlight+replays != arrivals {
		t.Errorf("arrivals answered = %d (fresh %d, in flight %d, replay %d), want all %d", fresh+inFlight+replays, fresh, inFlight, replays, arrivals)
	}

	requests, reservations, legSum, intakes, _ := fixture.residue(t, account)
	if requests != 1 || reservations != 1 || legSum != 1 || intakes != 1 {
		t.Errorf("residue reads %d requests, %d holds, %d taken unit, %d records — want one of each, the winner's", requests, reservations, legSum, intakes)
	}
	id, finalStatus, finalReason := fixture.intakeOf(t, account, key)
	if !finalStatus.Valid || finalStatus.String != string(execution.FinalRejected) || finalReason.String != string(execution.RejectedNoCandidate) {
		t.Errorf("the record's final pointer = (%s, %s), want rejected/no_candidate — the seam releases the hold and finalises the request as no_candidate in one ending",
			finalStatus.String, finalReason.String)
	}
	if got := fixture.rejectedRows(t, account, string(execution.RejectedNoCandidate)); got != 1 {
		t.Errorf("no_candidate rows on the record = %d, want the winner's one", got)
	}
	if facts := fixture.factCount(t, id); facts != 1 {
		t.Errorf("facts on the winner = %d, want exactly the one released fact", facts)
	}
	if available, limit := fixture.balance(t, bucket); available != capacity || limit != capacity {
		t.Errorf("balance reads (%d/%d), want the hold released back whole", available, limit)
	}
}

// TestIntegrationAdmissionRefusesAKeySpentOnDifferentBytes pins the conflict:
// a key already spent on a body is refused for any other body, whatever the
// record's state, and the conflict writes nothing — the record, the request
// row and the balance stand exactly as the first arrival left them.
func TestIntegrationAdmissionRefusesAKeySpentOnDifferentBytes(t *testing.T) {
	fixture := newAdmissionFixture(t, 4)
	fixture.seedPrice(t, context.Background(), 1, 1)
	account := admissionAccount(t)
	bucket := account + "-bucket"
	key := account + "-key"
	credential := account + "-api-key"
	fixture.seedGrant(t, context.Background(), account, bucket, true, time.Now().UTC().Add(24*time.Hour), 100)

	first := admissionBody(t, fixture.aliasName, 1, "b8c3-first")
	if outcome := admissionServe(t, fixture.admission,
		admissionInput(identity.NewRequestID(), account, credential, key, admissionServeState("active"), first)); outcome.Kind != application.OutcomeRejected || outcome.Reason != execution.RejectedNoCandidate {
		t.Fatalf("the first arrival = %q/%q, want rejected/no_candidate", outcome.Kind, outcome.Reason)
	}
	requests, _, _, intakes, _ := fixture.residue(t, account)
	if requests != 1 || intakes != 1 {
		t.Fatalf("after the first arrival the residue reads %d requests and %d records, want one of each", requests, intakes)
	}
	if available, _ := fixture.balance(t, bucket); available != 100 {
		t.Fatalf("balance after the first arrival = %d, want the grant whole (the hold was released)", available)
	}

	second := admissionBody(t, fixture.aliasName, 1, "b8c3-second")
	outcome := admissionServe(t, fixture.admission,
		admissionInput(identity.NewRequestID(), account, credential, key, admissionServeState("active"), second))
	if outcome.Kind != application.OutcomeConflict {
		t.Fatalf("the second arrival's body = %q, want conflict — same key over different bytes is never re-answered", outcome.Kind)
	}
	afterRequests, _, _, afterIntakes, _ := fixture.residue(t, account)
	if afterRequests != 1 || afterIntakes != 1 {
		t.Errorf("after the conflict the residue reads %d requests and %d records, want the first arrival's one of each — a conflict writes nothing", afterRequests, afterIntakes)
	}
	if _, finalStatus, _ := fixture.intakeOf(t, account, key); !finalStatus.Valid || finalStatus.String != string(execution.FinalRejected) {
		t.Errorf("the record's final pointer reads (%s, %v), want the first arrival's rejected — untouched by the conflict", finalStatus.String, finalStatus.Valid)
	}
}

// TestIntegrationAdmissionRollsBackAnUnpricedAlias pins the one interior
// outcome that writes nothing: an alias the price book cannot price is an
// operator's unfinished configuration, answered as an internal failure — an
// error, not a decision — with the account byte-identical and no admission
// row of any kind. The alias is this test's own, minted fresh per run and
// never priced by any sibling: the suite's shared alias carries prices the
// other scenarios seeded, and an alias without an entry of its own in any
// revision is exactly the unpriced shape the selection must refuse. The grant
// is seeded healthy against that alias's named scope, so the failure the test
// observes is pricing's alone.
func TestIntegrationAdmissionRollsBackAnUnpricedAlias(t *testing.T) {
	fixture := newAdmissionFixture(t, 4)
	unpricedName := "b8c3-unpriced-" + string(identity.NewRequestID())[:8]
	_, _, namedID := admissionSeedCatalog(t, fixture.store, unpricedName)
	account := admissionAccount(t)
	bucket := account + "-bucket"
	key := account + "-key"
	credential := account + "-api-key"
	fixture.namedID = namedID // the grant funds the unpriced alias's own named scope
	fixture.seedGrant(t, context.Background(), account, bucket, true, time.Now().UTC().Add(24*time.Hour), 100)

	requestID := identity.NewRequestID()
	outcome, err := fixture.admission.Serve(context.Background(),
		admissionInput(requestID, account, credential, key, admissionServeState("active"),
			admissionBody(t, unpricedName, 1, "b8c3")))
	if err == nil {
		t.Fatalf("Serve = %+v with a nil error, want the internal failure an unpriced alias is answered with", outcome)
	}
	if !errors.Is(err, persistence.ErrNoEffectivePrice) {
		t.Errorf("the failure = %v, want it to wrap ErrNoEffectivePrice — pricing's refusal, nothing else", err)
	}
	if outcome.Kind != "" {
		t.Errorf("the outcome reads kind %q, want the zero value — no decision was reached", outcome.Kind)
	}
	requests, reservations, legSum, intakes, rejected := fixture.residue(t, account)
	if requests != 0 || reservations != 0 || legSum != 0 || intakes != 0 || rejected != 0 {
		t.Errorf("residue reads %d requests, %d holds, %d taken units, %d records, %d refusals — want all zero: an interior failure writes nothing", requests, reservations, legSum, intakes, rejected)
	}
	if available, limit := fixture.balance(t, bucket); available != 100 || limit != 100 {
		t.Errorf("balance reads (%d/%d), want the seeded grant untouched", available, limit)
	}
}

// TestIntegrationAdmissionReplaysEveryStoredFate walks the replay matrix over
// real rows: each fate the use case stores gets a first arrival, a replay of
// the same key and body (answered from the record with the original's
// decision), and a companion arrival under the same key with different bytes
// (a conflict, writing nothing). Settlement-written fates are out of this
// milestone's reach and say so.
func TestIntegrationAdmissionReplaysEveryStoredFate(t *testing.T) {
	fixture := newAdmissionFixture(t, 4)

	cases := []struct {
		name   string
		body   func(t testing.TB, fixture *admissionFixture) []byte
		seed   func(t testing.TB, fixture *admissionFixture, account string)
		reason execution.RejectionReason
	}{
		{
			name: "unknown alias",
			seed: func(t testing.TB, fixture *admissionFixture, account string) {
				fixture.seedGrant(t, context.Background(), account, account+"-bucket", true, time.Now().UTC().Add(24*time.Hour), 100)
			},
			body: func(t testing.TB, fixture *admissionFixture) []byte {
				return admissionBody(t, "b8c3-no-such-alias", 1, "b8c3")
			},
			reason: execution.RejectedUnknownAlias,
		},
		{
			name: "malformed body",
			seed: func(t testing.TB, fixture *admissionFixture, account string) {
				fixture.seedGrant(t, context.Background(), account, account+"-bucket", true, time.Now().UTC().Add(24*time.Hour), 100)
			},
			body: func(t testing.TB, fixture *admissionFixture) []byte {
				return []byte("{not json at all")
			},
			reason: execution.RejectedInvalidRequest,
		},
		{
			name: "no eligible grant",
			seed: func(t testing.TB, fixture *admissionFixture, account string) {
				// No grant: the scope answer.
			},
			body: func(t testing.TB, fixture *admissionFixture) []byte {
				return admissionBody(t, fixture.aliasName, 1, "b8c3")
			},
			reason: execution.RejectedNoAccess,
		},
		{
			name: "exhausted eligible grant",
			seed: func(t testing.TB, fixture *admissionFixture, account string) {
				// Eligible but empty: the walk sees the grant and cannot fund
				// the hold, so the answer is the shortage, not the scope.
				fixture.seedGrant(t, context.Background(), account, account+"-bucket", true, time.Now().UTC().Add(24*time.Hour), 0)
			},
			body: func(t testing.TB, fixture *admissionFixture) []byte {
				return admissionBody(t, fixture.aliasName, 1, "b8c3")
			},
			reason: execution.RejectedInsufficientEntitlement,
		},
		{
			name: "released as no candidate",
			seed: func(t testing.TB, fixture *admissionFixture, account string) {
				fixture.seedGrant(t, context.Background(), account, account+"-bucket", true, time.Now().UTC().Add(24*time.Hour), 100)
			},
			body: func(t testing.TB, fixture *admissionFixture) []byte {
				return admissionBody(t, fixture.aliasName, 1, "b8c3")
			},
			reason: execution.RejectedNoCandidate,
		},
		{
			name: "still in flight",
			seed: func(t testing.TB, fixture *admissionFixture, account string) {
				fixture.seedGrant(t, context.Background(), account, account+"-bucket", true, time.Now().UTC().Add(24*time.Hour), 100)
			},
			body: func(t testing.TB, fixture *admissionFixture) []byte {
				return admissionBody(t, fixture.aliasName, 1, "b8c3")
			},
			reason: "",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture.seedPrice(t, context.Background(), 1, 1)
			account := admissionAccount(t)
			key := account + "-key"
			credential := account + "-api-key"
			testCase.seed(t, fixture, account)
			body := testCase.body(t, fixture)
			digest := execution.SecretDigest(body)

			// The in-flight fate is seeded, not served: a request whose record
			// exists with no terminal pointer, the shape a concurrently
			// executing original leaves behind.
			if testCase.reason == "" {
				requestID := identity.NewRequestID()
				price := execution.PriceSnapshot{RevisionID: "b8c3-inflight-revision", InputUnitPrice: 1, OutputUnitPrice: 1}
				now := time.Now().UTC()
				request, err := execution.NewRequest(requestID, account, credential, fixture.aliasName, 4, 1, price, now)
				if err != nil {
					t.Fatalf("forming the in-flight original: %v", err)
				}
				record, err := execution.NewIntake(account, key, digest, requestID, now)
				if err != nil {
					t.Fatalf("forming the in-flight record: %v", err)
				}
				if err := fixture.store.WithinTx(context.Background(), func(ctx context.Context) error {
					if err := fixture.requests.Insert(ctx, request); err != nil {
						return err
					}
					return fixture.intakes.Insert(ctx, record)
				}); err != nil {
					t.Fatalf("seeding the in-flight original: %v", err)
				}
			}

			first := admissionServe(t, fixture.admission,
				admissionInput(identity.NewRequestID(), account, credential, key, admissionServeState("active"), body))
			switch {
			case testCase.reason == "":
				if first.Kind != application.OutcomeInFlight {
					t.Fatalf("the first arrival = %q, want in_flight — the record has no terminal pointer to answer from", first.Kind)
				}
			default:
				if first.Kind != application.OutcomeRejected || first.Reason != testCase.reason {
					t.Fatalf("the first arrival = %q/%q, want rejected/%q", first.Kind, first.Reason, testCase.reason)
				}
			}

			// The same key and body again: answered from the record, with the
			// original's identity on the outcome for every terminal fate.
			replay := admissionServe(t, fixture.admission,
				admissionInput(identity.NewRequestID(), account, credential, key, admissionServeState("active"), body))
			storedID, finalStatus, finalReason := fixture.intakeOf(t, account, key)
			if testCase.reason == "" {
				if replay.Kind != application.OutcomeInFlight {
					t.Errorf("the re-arrival = %q, want in_flight again — the original has not ended", replay.Kind)
				}
			} else {
				if replay.Kind != application.OutcomeReplay || replay.Reason != testCase.reason {
					t.Errorf("the re-arrival = %q/%q, want replay/%q", replay.Kind, replay.Reason, testCase.reason)
				}
				if replay.Original == "" || string(replay.Original) != storedID {
					t.Errorf("the replay names original %q, want the stored request %q", replay.Original, storedID)
				}
				if !finalStatus.Valid || finalStatus.String != string(execution.FinalRejected) || finalReason.String != string(testCase.reason) {
					t.Errorf("the record reads (%s, %s), want rejected/%q", finalStatus.String, finalReason.String, testCase.reason)
				}
			}

			// The companion: same key, different bytes — a conflict, and
			// nothing written.
			other := admissionBody(t, fixture.aliasName, 1, "b8c3-different-bytes")
			if string(other) == string(body) {
				t.Fatal("the companion body equals the original's — the matrix built a useless companion")
			}
			conflict := admissionServe(t, fixture.admission,
				admissionInput(identity.NewRequestID(), account, credential, key, admissionServeState("active"), other))
			if conflict.Kind != application.OutcomeConflict {
				t.Errorf("the companion = %q, want conflict", conflict.Kind)
			}
			requests, _, _, intakes, _ := fixture.residue(t, account)
			if intakes != 1 {
				t.Errorf("records after the conflict = %d, want the one record — a conflict writes nothing", intakes)
			}
			if testCase.reason == "" {
				if requests != 1 {
					t.Errorf("requests after the conflict = %d, want the seeded original's one", requests)
				}
			} else if requests != 1 {
				t.Errorf("requests after the conflict = %d, want the first arrival's one", requests)
			}
			if digest2 := execution.SecretDigest(body); digest2 != digest {
				t.Errorf("the digest moved between reads — the sameness test is not stable")
			}
		})
	}

	t.Run("settlement-written fates are out of reach", func(t *testing.T) {
		t.Skip("succeeded and failed originals are written by the settlement driver, which is the next milestone's work — this build's probe refuses them as terminal in a way it cannot answer (admit.go probe); the matrix covers every fate the record can carry today")
	})
}

// admissionKeyID mints a key id in the presentation grammar — a canonical
// version-4 uuid spelling. The identity minter produces version-7 ids, whose
// version nibble ParseToken refuses, so the suite rewrites the two grammar
// nibbles of a fresh id; the mirror's uuid column takes the result as-is.
func admissionKeyID(t testing.TB) string {
	t.Helper()
	id := []byte(identity.NewRequestID())
	id[14] = '4' // the RFC's version nibble
	id[19] = '8' // the RFC's variant bits
	return string(id)
}

// seedMirror fills the mirror the only way anything fills it — through the
// projection applier, the door the Control Plane's feed arrives through. The
// suite names no mirror table of its own: the mirror is the adapter's, and
// the arch rule that keeps it that way (one credential source, ADR 0007) is
// honoured here by seeding through the same port production writes it with.
// An empty keyID seeds no credential record (the unknown-key shape); an empty
// accountState seeds no account record — the integrity-violation shape
// verification refuses closed.
func (f *admissionFixture) seedMirror(t testing.TB, ctx context.Context, keyID, accountID, digest, keyState, accountState string) {
	t.Helper()
	snapshot := projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            string(identity.NewRequestID()),
		SnapshotRevision: 1,
	}
	if keyID != "" {
		var record projection.APIKeyCredential
		if keyState == "revoked" {
			revokedAt := time.Now().UTC()
			record = projection.APIKeyCredential{
				KeyID: keyID, AccountID: accountID, Digest: digest,
				State: projection.CredentialRevoked, RevokedAt: &revokedAt,
			}
		} else {
			record = projection.APIKeyCredential{
				KeyID: keyID, AccountID: accountID, Digest: digest,
				State: projection.CredentialActive,
			}
		}
		snapshot.APIKeys = append(snapshot.APIKeys, record)
	}
	if accountState != "" {
		var state projection.AccountLifecycle
		switch accountState {
		case "active":
			state = projection.LifecycleActive
		case "suspended":
			state = projection.LifecycleSuspended
		case "closed":
			state = projection.LifecycleClosed
		default:
			t.Fatalf("unknown account lifecycle %q", accountState)
		}
		snapshot.Accounts = append(snapshot.Accounts, projection.AccountState{AccountID: accountID, State: state})
	}
	if _, err := postgres.NewProjectionApplier(f.db).ApplySnapshot(ctx, snapshot, time.Now().UTC()); err != nil {
		t.Fatalf("applying the seed snapshot: %v", err)
	}
}

// admissionMint renders a presentable credential from a key id and returns
// the secret bytes beside it, so the test can present a different secret
// against the same key id when it wants the miss path.
func admissionMint(keyID string, secret []byte) string {
	return "gw_" + keyID + "_" + base64.RawURLEncoding.EncodeToString(secret)
}

// TestIntegrationCredentialVerificationJudgesTheMirrorLifecycles walks the
// verification matrix against real mirror rows: a valid key verifies to its
// key and account identity; an unknown key, a wrong secret and a revoked key
// are refused with the credential reasons; a credential whose account row is
// absent is the integrity refusal, carrying a nil account fact. Every refusal
// writes nothing — not a request row, not a replay record — and every refusal
// that is not the unknown-shape is answered with the identical cause on the
// wire. The account gate is then served through: suspended and closed both
// arrive as rejected rows with their own reason and no replay record, so a
// re-arrival is judged fresh again.
func TestIntegrationCredentialVerificationJudgesTheMirrorLifecycles(t *testing.T) {
	fixture := newAdmissionFixture(t, 4)
	fixture.seedPrice(t, context.Background(), 1, 1)

	secret := make([]byte, 32)
	digest := execution.SecretDigest(secret)
	otherSecret := make([]byte, 32)
	otherSecret[0] = 1 // a different 32-byte secret, so its digest differs

	type shape struct {
		name          string
		keyState      string
		accountState  string
		present       string // which secret the arrival presents
		wantReason    application.UnauthenticatedReason
		wantAccountID bool
	}
	shapes := []shape{
		{name: "active key, active account", keyState: "active", accountState: "active", present: "own", wantAccountID: true},
		{name: "unknown key", keyState: "", accountState: "active", present: "own", wantReason: application.ReasonCredentialUnknown},
		{name: "wrong secret", keyState: "active", accountState: "active", present: "other", wantReason: application.ReasonCredentialUnknown},
		{name: "revoked key", keyState: "revoked", accountState: "active", present: "own", wantReason: application.ReasonCredentialRevoked},
		{name: "account row absent", keyState: "active", accountState: "", present: "own", wantReason: application.ReasonAccountAbsent},
	}

	for _, one := range shapes {
		t.Run(one.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// The mirror's identity columns are uuid: the account id here is a
			// bare id, not the prefixed shape the text-typed runtime tables
			// take elsewhere in the suite.
			account := string(identity.NewRequestID())
			keyID := admissionKeyID(t)
			if one.keyState != "" {
				fixture.seedMirror(t, ctx, keyID, account, digest, one.keyState, one.accountState)
			} else if one.accountState != "" {
				// The unknown-key shape: no credential record, but the account
				// exists — the miss must stay a miss, not trip the integrity
				// refusal.
				fixture.seedMirror(t, ctx, "", account, "", "", one.accountState)
			}

			presented := secret
			if one.present == "other" {
				presented = otherSecret
			}
			verified, err := fixture.auth.Authenticate(ctx, admissionMint(keyID, presented))
			if one.wantReason == "" {
				if err != nil {
					t.Fatalf("Authenticate error = %v, want the verified identity", err)
				}
				if verified.KeyID != keyID || verified.AccountID != account {
					t.Errorf("verification read (%s, %s), want the presented key and its owner", verified.KeyID, verified.AccountID)
				}
				if verified.AccountState == nil || *verified.AccountState != one.accountState {
					t.Errorf("verification read account state %v, want %q", verified.AccountState, one.accountState)
				}
				return
			}
			var refused *application.Unauthenticated
			if err == nil {
				t.Fatalf("Authenticate verified a key the mirror refuses (%s)", one.name)
			}
			if !errors.As(err, &refused) {
				t.Fatalf("the refusal = %v, want *application.Unauthenticated", err)
			}
			if refused.Reason != one.wantReason {
				t.Errorf("the refusal reason = %q, want %q", refused.Reason, one.wantReason)
			}
			// Nothing was written: the refusal never became a request.
			requests, reservations, legSum, intakes, rejected := fixture.residue(t, account)
			if requests != 0 || reservations != 0 || legSum != 0 || intakes != 0 || rejected != 0 {
				t.Errorf("residue after the refusal reads %d requests, %d holds, %d taken units, %d records, %d refusals — want all zero: verification writes nothing", requests, reservations, legSum, intakes, rejected)
			}
		})
	}

	t.Run("a miss burns the constant-time compare on the zero digest", func(t *testing.T) {
		// The unknown-shape and the wrong-secret shape must be the same
		// refusal: the reason values are asserted equal here, so the wire
		// cannot tell a never-minted key from a mis-typed one.
		var unknown, wrong *application.Unauthenticated
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, errUnknown := fixture.auth.Authenticate(ctx, admissionMint(admissionKeyID(t), secret))
		if !errors.As(errUnknown, &unknown) {
			t.Fatalf("the unknown-key refusal = %v, want *application.Unauthenticated", errUnknown)
		}
		presented := admissionKeyID(t)
		fixture.seedMirror(t, ctx, presented, string(identity.NewRequestID()), digest, "active", "active")
		_, errWrong := fixture.auth.Authenticate(ctx, admissionMint(presented, otherSecret))
		if !errors.As(errWrong, &wrong) {
			t.Fatalf("the wrong-secret refusal = %v, want *application.Unauthenticated", errWrong)
		}
		if unknown.Reason != wrong.Reason {
			t.Errorf("the refusals read %q and %q — a miss is one refusal, whatever the mirror held", unknown.Reason, wrong.Reason)
		}
	})

	t.Run("the account gate refuses through Serve", func(t *testing.T) {
		for _, state := range []struct {
			mirror string
			reason execution.RejectionReason
		}{
			{mirror: "suspended", reason: execution.RejectedAccountSuspended},
			{mirror: "closed", reason: execution.RejectedAccountClosed},
		} {
			t.Run(state.mirror, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				// The gate's refusal rows key on the account too — the request
				// table's account_id is text, so the bare mirror uuid rides
				// through unchanged.
				account := string(identity.NewRequestID())
				key := account + "-key"
				keyID := admissionKeyID(t)
				fixture.seedMirror(t, ctx, keyID, account, digest, "active", state.mirror)
				verified, err := fixture.auth.Authenticate(ctx, admissionMint(keyID, secret))
				if err != nil {
					t.Fatalf("Authenticate error = %v — the key verifies, the gate is admission's judgement", err)
				}
				in := admissionInput(identity.NewRequestID(), verified.AccountID, verified.KeyID, key, verified.AccountState,
					admissionBody(t, fixture.aliasName, 1, "b8c3"))
				outcome := admissionServe(t, fixture.admission, in)
				if outcome.Kind != application.OutcomeRejected || outcome.Reason != state.reason {
					t.Fatalf("Serve = %q/%q, want rejected/%q", outcome.Kind, outcome.Reason, state.reason)
				}
				if got := fixture.rejectedRows(t, account, string(state.reason)); got != 1 {
					t.Errorf("%s rows on the record = %d, want the one refused arrival", state.mirror, got)
				}
				if _, _, _, intakes, _ := fixture.residue(t, account); intakes != 0 {
					t.Errorf("replay records = %d, want none — the gate's refusal writes no record", intakes)
				}
				// A second arrival is judged fresh again: no record was left
				// behind, so nothing replays and the refusal is written once
				// more.
				outcome = admissionServe(t, fixture.admission, admissionInput(identity.NewRequestID(), verified.AccountID, verified.KeyID, key, verified.AccountState,
					admissionBody(t, fixture.aliasName, 1, "b8c3")))
				if outcome.Kind != application.OutcomeRejected || outcome.Reason != state.reason {
					t.Fatalf("the re-arrival = %q/%q, want rejected/%q — the gate's refusal leaves no record to answer from", outcome.Kind, outcome.Reason, state.reason)
				}
				if got := fixture.rejectedRows(t, account, string(state.reason)); got != 2 {
					t.Errorf("%s rows after the re-arrival = %d, want 2 — each arrival refused and recorded on its own", state.mirror, got)
				}
			})
		}
	})
}

// BenchmarkChatAdmissionServe is the flagship number: one request's full
// admission cost as the transport drives it — verification against the real
// mirror, the probe, the alias and price reads, the waterfall, the writes,
// the seam's release — everything but the provider call that does not exist
// yet. The grant is seeded deep enough that capacity is never the variable;
// every iteration is a fresh account-keyed arrival under a fresh key, so the
// loop costs what a fresh request costs and not what a replay answers for.
//
// Run (from apps/dataplane):
//
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:55443/postgres?sslmode=disable' \
//	  go test -run '^$' -bench BenchmarkChatAdmissionServe -benchmem -tags=integration ./cmd/dataplane
func BenchmarkChatAdmissionServe(b *testing.B) {
	db, store := admissionPool(b, admissionPlaneDSN(b), 8)
	admissionSchema(b, db)

	ledger := postgres.NewQuotaProjectionRepository(store)
	admission := application.NewChatAdmission(
		store,
		postgres.NewCredentials(store),
		postgres.NewModelAliases(store),
		postgres.NewPriceBook(store),
		postgres.NewRequestRepository(store),
		postgres.NewIntakeRepository(store),
		postgres.NewReservationRepository(store),
		ledger,
		postgres.NewFactRepository(store),
		application.AdmissionConfig{HoldWindow: time.Hour, LeaseTTL: 30 * time.Minute, LeaseOwner: "b8c3-bench"},
	)
	auth := application.NewCredentialAuthenticator(postgres.NewCredentials(store))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The catalog scaffolding, the price, the grant and the mirror row: every
	// iteration's arrival is fresh-keyed against one deep account.
	aliasName := "b8c3-bench-alias"
	aliasID, wildcardID, _ := admissionSeedCatalog(b, store, aliasName)
	admissionSeedPrice(b, ctx, store, aliasID, 1, 1)
	account := string(identity.NewRequestID()) // bare uuid: the mirror's identity columns are uuid
	if _, err := ledger.ApplyPublication(ctx, accounting.Publication{
		AccountID:             account,
		FundingBucketID:       account + "-bucket",
		AliasGroupVersionID:   wildcardID,
		ScopeKind:             accounting.ScopePayGBalance,
		PeriodEnd:             time.Time{},
		SubscriptionCreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		State:                 accounting.ProjectionActive,
		LimitAmount:           1_000_000_000_000,
		Revision:              1,
	}); err != nil {
		b.Fatalf("seeding the bench grant: %v", err)
	}
	keyID := []byte(identity.NewRequestID())
	keyID[14], keyID[19] = '4', '8'
	secret := make([]byte, 32)
	token := "gw_" + string(keyID) + "_" + base64.RawURLEncoding.EncodeToString(secret)
	// The bench credential rides the projection applier too: the mirror is
	// filled the way production fills it, and no second write path is named.
	if _, err := postgres.NewProjectionApplier(db).ApplySnapshot(ctx, projection.Snapshot{
		ProtocolVersion:  projection.ProtocolVersion,
		Epoch:            string(identity.NewRequestID()),
		SnapshotRevision: 1,
		APIKeys: []projection.APIKeyCredential{{
			KeyID: string(keyID), AccountID: account,
			Digest: execution.SecretDigest(secret), State: projection.CredentialActive,
		}},
		Accounts: []projection.AccountState{{AccountID: account, State: projection.LifecycleActive}},
	}, time.Now().UTC()); err != nil {
		b.Fatalf("seeding the bench credential: %v", err)
	}
	body, err := json.Marshal(admissionChatBody{
		Model:     aliasName,
		MaxTokens: 1,
		Messages:  []admissionChatMessage{{Role: "user", Content: "b8c3"}},
	})
	if err != nil {
		b.Fatalf("rendering the bench body: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		verified, err := auth.Authenticate(ctx, token)
		if err != nil {
			b.Fatalf("iteration %d: verification failed: %v", i, err)
		}
		outcome, err := admission.Serve(ctx, application.ChatInput{
			RequestID:      identity.NewRequestID(),
			Credential:     verified.KeyID,
			AccountID:      verified.AccountID,
			AccountState:   verified.AccountState,
			IdempotencyKey: fmt.Sprintf("bench-key-%s-%d", account, i),
			Body:           application.BodyBytes(body),
		})
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
		if outcome.Kind != application.OutcomeRejected || outcome.Reason != execution.RejectedNoCandidate {
			b.Fatalf("iteration %d: outcome %q/%q, want rejected/no_candidate — the benchmark's own arrival is wrong", i, outcome.Kind, outcome.Reason)
		}
	}
}
