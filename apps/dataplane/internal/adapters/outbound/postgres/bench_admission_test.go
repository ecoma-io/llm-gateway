//go:build integration

package postgres

// The admission benchmarks the landed bench_test.go does not already carry:
// the pure arithmetic the hot path runs per request (token grammar, digest
// comparison, hold formula, interim token count) and the reads and walks the
// admission unit is built from — the credential lookup, the alias read, the
// price selection, and the waterfall walk itself at one and at three grants.
// The composed end-to-end number (authenticate, admit, release) is the
// composition-root benchmark in cmd/dataplane.
//
// Run (from apps/dataplane):
//
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:5432/postgres?sslmode=disable' \
//	  go test -run '^$' -bench 'BenchmarkParseToken|BenchmarkSecretDigest|BenchmarkEqualDigests|BenchmarkHold|BenchmarkCountInputTokens|BenchmarkCredentialsLookup|BenchmarkModelAliasesByName|BenchmarkPriceBookEffectiveAt|BenchmarkAdmissionDrawdown' -benchmem -tags=integration ./internal/adapters/outbound/postgres
//
// The pure benchmarks touch no database and are in this file only so the
// whole admission benchmark set rides one command; they are at the top so the
// database suites read as the tail they are.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// The presented token both parse benchmarks read. The key id is a literal
// version-4 uuid in the canonical spelling the grammar demands; the secret is
// the canonical unpadded base64url of 32 zero bytes — 43 characters, exactly
// what ParseToken's length check accepts. The malformed twin is the valid
// token with one extra secret character: it walks the whitespace scan, the
// split, the brand check, the key-id validation and the strict length refusal
// before failing closed.
var (
	benchmarkTokenValid     = "gw_b8c3a000-0000-4000-8000-000000000001_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	benchmarkTokenMalformed = benchmarkTokenValid + "0"
)

// BenchmarkParseTokenValid costs the fail-closed front door on an acceptable
// credential — the work every authenticated request pays before anything
// touches the database.
func BenchmarkParseTokenValid(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := execution.ParseToken(benchmarkTokenValid); err != nil {
			b.Fatalf("parsing a valid token: %v", err)
		}
	}
}

// BenchmarkParseTokenMalformed costs the refusal path — the constant work a
// bad credential pays, which must look like nothing on the wire.
func BenchmarkParseTokenMalformed(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := execution.ParseToken(benchmarkTokenMalformed); err == nil {
			b.Fatal("parsing a malformed token succeeded")
		}
	}
}

// BenchmarkSecretDigest costs the SHA-256 over the presented secret's raw
// bytes and its hex rendering — once per presented credential, valid or not.
func BenchmarkSecretDigest(b *testing.B) {
	secret := make([]byte, 32)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if digest := execution.SecretDigest(secret); len(digest) != 64 {
			b.Fatalf("digest length = %d, want 64", len(digest))
		}
	}
}

// BenchmarkEqualDigests costs the constant-time comparison the credential
// verdict is decided by: the presented secret's digest against the mirror
// column, both fixed-width lowercase hex.
func BenchmarkEqualDigests(b *testing.B) {
	presented := execution.SecretDigest(make([]byte, 32))
	mirrored := execution.SecretDigest(make([]byte, 32))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !execution.EqualDigests(presented, mirrored) {
			b.Fatal("equal digests compared unequal")
		}
	}
}

// BenchmarkHoldTypical costs the hold formula at the request shape the rest
// of the suite prices: 120 input tokens at 2 and 4096 output tokens at 3 —
// a hold of one minor unit, the sum's single ceiling over the 1M scale.
func BenchmarkHoldTypical(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if hold, err := accounting.Hold(120, 4096, 2, 3); err != nil || hold != 1 {
			b.Fatalf("Hold(120, 4096, 2, 3) = %d, %v; want 1, nil", hold, err)
		}
	}
}

// BenchmarkHoldOverflowAdjacent costs the formula where the 128-bit path is
// the whole point: the raw product is far past 64 bits (so both words of the
// mul64 carry real value) yet the high word stays under the 1M divisor, and
// the quotient lands just inside the largest reservable amount. Arithmetic
// that wrapped to 64 bits would answer a quietly smaller hold here; the
// assertion pins the checked answer instead.
func BenchmarkHoldOverflowAdjacent(b *testing.B) {
	const (
		inputTokens = 1_000_000_000_000         // 1e12 tokens
		inputPrice  = 9_000_000_000_000         // 9e12 minor units per 1M tokens
		wantHold    = 9_000_000_000_000_000_000 // 9e18, just under 2^63
	)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		hold, err := accounting.Hold(inputTokens, 0, inputPrice, 0)
		if err != nil || hold != wantHold {
			b.Fatalf("Hold at the 128-bit edge = %d, %v; want %d, nil", hold, err, wantHold)
		}
	}
}

// BenchmarkCountInputTokens costs the interim byte counter on a request body
// the suite's shapes produce: three content parts, one of them multibyte, so
// the byte (not rune) walk is what is measured.
func BenchmarkCountInputTokens(b *testing.B) {
	contents := []string{
		"You are a helpful assistant.",
		"Summarise the following contract in five bullet points.",
		"Здесь многоязычный текст, считаемый байтами, не рунами.",
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if count := catalog.CountInputTokens(contents); count <= 0 {
			b.Fatalf("CountInputTokens = %d, want the parts' byte sum", count)
		}
	}
}

// BenchmarkCredentialsLookup costs the credential read every request opens
// with: one LEFT JOINed statement over the mirror's two rows, by the
// presented key id. The rows are seeded straight into the mirror tables once,
// outside the timer — the TB-shaped seeding the projection helpers
// (testing.T-shaped, in projection_integration_test.go) do not offer a
// benchmark — and the lookup is the whole measurement.
func BenchmarkCredentialsLookup(b *testing.B) {
	db, store := integrationPoolTB(b)
	integrationRuntimeSchema(b, db)
	credentials := NewCredentials(store)
	ctx := benchmarkContext(b)

	keyID := string(identity.NewRequestID())
	accountID := string(identity.NewRequestID())
	digest := execution.SecretDigest(make([]byte, 32))
	if _, err := db.ExecContext(ctx, `INSERT INTO api_key_credentials (key_id, account_id, digest, state, revoked_at, source_revision, updated_at)
VALUES ($1, $2, $3, 'active', NULL, 1, transaction_timestamp())`, keyID, accountID, digest); err != nil {
		b.Fatalf("seeding the credential row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO account_states (account_id, state, source_revision, updated_at)
VALUES ($1, 'active', 1, transaction_timestamp())`, accountID); err != nil {
		b.Fatalf("seeding the account row: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		view, err := credentials.Lookup(ctx, keyID)
		if err != nil {
			b.Fatalf("Credentials.Lookup: %v", err)
		}
		if view.Digest != digest || view.AccountID != accountID {
			b.Fatalf("lookup read (%s, %s), want the seeded pair", view.Digest, view.AccountID)
		}
	}
}

// BenchmarkModelAliasesByName costs the alias read admission resolves the
// request's model name through — one row by its unique name, the bounds and
// the reservation cap riding along in the aggregate. The alias is the suite's
// converged one, so the benchmark and the tests read the same row.
func BenchmarkModelAliasesByName(b *testing.B) {
	db, store := integrationPoolTB(b)
	integrationRuntimeSchema(b, db)
	aliases := NewModelAliases(store)
	repos := integrationRepos(b, store)
	ctx := benchmarkContext(b)

	scope := repos.catalogScope(b)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		alias, err := aliases.ByName(ctx, integrationScopeAliasName)
		if err != nil {
			b.Fatalf("ModelAliases.ByName(%s): %v", integrationScopeAliasName, err)
		}
		if alias.ID != scope.alias {
			b.Fatalf("ByName read alias %s, want the seeded scope alias %s", alias.ID, scope.alias)
		}
	}
}

// benchmarkPriceListSeed mints one activated revision pricing the given
// alias, with a version past every revision the fixture database carries and
// an effective instant no activated revision can collide with — the shared
// database keeps its price history, so both numbers are read, not assumed.
func benchmarkPriceListSeed(b *testing.B, ctx context.Context, store persistence.Store, aliasID catalog.AliasID, input, output int64) string {
	b.Helper()
	querier := store.Querier(ctx)
	var version int
	if err := querier.QueryRowContext(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM client_price_list_revisions`).Scan(&version); err != nil {
		b.Fatalf("reading the next price list version: %v", err)
	}
	effectiveFrom := time.Now().UTC().Add(-time.Hour)
	var latest time.Time
	switch err := querier.QueryRowContext(ctx, `SELECT effective_from FROM client_price_list_revisions
WHERE state = 'activated' ORDER BY effective_from DESC LIMIT 1`).Scan(&latest); {
	case err == nil:
		// The activated-instant partial unique admits one revision per
		// instant, so a latest instant at or past the benchmark's choice
		// pushes the seed one microsecond past it.
		if !latest.Before(effectiveFrom) {
			effectiveFrom = latest.Add(time.Microsecond)
		}
	case errors.Is(err, sql.ErrNoRows):
		// No activated revision yet: the benchmark's own instant is free.
	default:
		b.Fatalf("reading the latest activated effective_from: %v", err)
	}
	revisionID := integrationSeedRevision(b, ctx, store, version, "activated", effectiveFrom)
	integrationSeedEntry(b, ctx, store, revisionID, aliasID, input, output)
	return revisionID
}

// BenchmarkPriceBookEffectiveAt costs the price selection admission prices
// under: the read that resolves the one activated revision effective at now
// and its entry for the alias. A revision is minted per benchmark run —
// version and effective instant read from the shared history, so the seed
// converges against a database that already carries revisions.
func BenchmarkPriceBookEffectiveAt(b *testing.B) {
	db, store := integrationPoolTB(b)
	integrationRuntimeSchema(b, db)
	book := NewPriceBook(store)
	repos := integrationRepos(b, store)
	ctx := benchmarkContext(b)

	scope := repos.catalogScope(b)
	benchmarkPriceListSeed(b, ctx, store, scope.alias, 2, 3)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snapshot, err := book.EffectiveAt(ctx, scope.alias)
		if err != nil {
			b.Fatalf("PriceBook.EffectiveAt: %v", err)
		}
		if snapshot.InputUnitPrice != 2 || snapshot.OutputUnitPrice != 3 {
			b.Fatalf("EffectiveAt read (%d, %d), want the seeded (2, 3)", snapshot.InputUnitPrice, snapshot.OutputUnitPrice)
		}
	}
}

// benchmarkAdmissionWalk costs the waterfall walk at a fixed grant count:
// every iteration draws 40 minor units through one unit of work, exactly as
// the admission unit runs it, against grants seeded deep enough that capacity
// is never the variable. The grant count is the walk's row count, so the two
// benchmarks bracket the waterfall's per-row cost.
func benchmarkAdmissionWalk(b *testing.B, grants int) {
	db, store := integrationPoolTB(b)
	repos := integrationRepos(b, store)
	integrationRuntimeSchema(b, db)
	ctx := benchmarkContext(b)

	account := integrationRuntimeAccount(b, "bench-walk")
	now := time.Now().UTC()
	for i := 0; i < grants; i++ {
		// Distinct buckets, staggered period ends: the waterfall's order is
		// decided by the inputs the benchmark chose, so every iteration walks
		// the same rows and takes from the same head grant.
		bucket := account + "-bucket-" + string(rune('a'+i))
		integrationPublish(b, ctx, repos, account, bucket, true, now.Add(time.Duration(24+i)*time.Hour), 1_000_000_000_000, int64(i+1), accounting.ProjectionActive)
	}
	scope := repos.catalogScope(b)
	const draw = int64(40)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := store.WithinTx(ctx, func(ctx context.Context) error {
			legs, err := repos.quota.Drawdown(ctx, account, scope.alias, draw)
			if err != nil {
				return err
			}
			var taken int64
			for _, leg := range legs {
				taken += leg.Amount
			}
			if taken != draw {
				b.Fatalf("the walk granted %d, want %d", taken, draw)
			}
			return nil
		})
		if err != nil {
			b.Fatalf("walk %d: %v", i, err)
		}
	}
}

// BenchmarkAdmissionDrawdownWalk1Grant is the single-grant waterfall: the
// common case, one eligible row walked, one conditional take.
func BenchmarkAdmissionDrawdownWalk1Grant(b *testing.B) { benchmarkAdmissionWalk(b, 1) }

// BenchmarkAdmissionDrawdownWalk3Grant is the three-grant waterfall: the
// entitlement-stacked case, three eligible rows walked in the ADR 0003 order.
func BenchmarkAdmissionDrawdownWalk3Grant(b *testing.B) { benchmarkAdmissionWalk(b, 3) }
