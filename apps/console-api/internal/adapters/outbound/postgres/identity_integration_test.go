//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// The identity repositories against the real `control` database. The
// fake-driver tests pin which handle a statement runs on and how a driver
// condition becomes the port's vocabulary; only PostgreSQL can answer the
// rest of the adapter's promises — that the schema its migration claims
// (foreign keys, the per-account live-email uniqueness) actually refuses what
// it must refuse, that a compare-and-swap is one statement that lands exactly
// once under concurrent callers, and that nothing an ownership row persists
// carries secret material. What is deliberately NOT re-proven here is the
// pure SQL-level schema behaviour: deploy/postgres/verify.sh owns that tier,
// and duplicating it would be two definitions of the same green.
//
// Like every integration tier in this repository, the database is required
// explicitly rather than started from go test (see valkey_integration_test.go
// for the reasoning). deploy/postgres/compose.yaml provides the pinned
// instance, and both lanes' migrations must be applied first:
//
//	docker compose -f deploy/postgres/compose.yaml up -d --wait
//	docker compose -f deploy/postgres/compose.yaml run --rm migrate up
//	GATEWAY_MIGRATE_LANE=control docker compose -f deploy/postgres/compose.yaml run --rm migrate up
//
// Run:
//
//	CONTROL_DATABASE_ADDRESS='postgres://gateway:gateway-dev-only@127.0.0.1:5432/control?sslmode=disable' \
//	  go test -tags=integration -race ./internal/adapters/outbound/postgres
//
// bash deploy/postgres/verify.sh leaves the database in exactly that state.
//
// The pgx driver registers itself under database/sql; the adapter never names
// it (its wiring comment records why), and this import is the module's only
// one — the integration tier's, not the production build's.

// integrationDB opens the control database the run names, or fails the test
// with the one-line recipe for getting there.
func integrationDB(t *testing.T) *sql.DB {
	t.Helper()
	address := os.Getenv("CONTROL_DATABASE_ADDRESS")
	if address == "" {
		t.Fatal("CONTROL_DATABASE_ADDRESS is required for integration tests; start deploy/postgres, apply both lanes' migrations (bash deploy/postgres/verify.sh does exactly this), and point it at the control database")
	}
	db, err := sql.Open("pgx", address)
	if err != nil {
		t.Fatalf("open the control database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping the control database: %v", err)
	}
	return db
}

// integrationIdentity builds the three repositories over one pool, as the
// ports the use cases see them — the integration tier tests the port
// contract, not the concrete types behind it.
func integrationIdentity(t *testing.T) (*sql.DB, persistence.Accounts, persistence.Users, persistence.APIKeys) {
	t.Helper()
	db := integrationDB(t)
	store := New(db)
	return db, NewAccounts(store), NewUsers(store), NewAPIKeys(store)
}

// integrationAccount opens a fresh account whose id and name collide with no
// earlier run's rows — every probe mints its own ids, so a rerun against a
// migrated database cannot trip over history.
func integrationAccount(t *testing.T, r persistence.Accounts, name string) identity.Account {
	t.Helper()
	id, err := identity.NewAccountID()
	if err != nil {
		t.Fatalf("NewAccountID: %v", err)
	}
	account, err := identity.NewAccount(id, name, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	if err := r.Create(t.Context(), *account); err != nil {
		t.Fatalf("create account: %v", err)
	}
	return *account
}

// integrationUser invites a user under account with a run-unique address.
func integrationUser(t *testing.T, r persistence.Users, accountID identity.AccountID) identity.User {
	t.Helper()
	id, err := identity.NewUserID()
	if err != nil {
		t.Fatalf("NewUserID: %v", err)
	}
	suffix, err := identity.NewUserID()
	if err != nil {
		t.Fatalf("NewUserID: %v", err)
	}
	email := fmt.Sprintf("it-%s@example.com", strings.ReplaceAll(string(suffix), "-", "")[:12])
	user, err := identity.NewUser(id, accountID, email, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	if err := r.Create(t.Context(), *user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return *user
}

// recordedCredentials is the verification source the control side can honestly
// build today: the digests minted in this process, keyed by key id. The
// control database holds no digest — the projection that delivers one to the
// Data Plane is a later phase (ADR 0006 §8) — so a source backed by anything
// else would be lying about what exists.
type recordedCredentials map[identity.APIKeyID]identity.Credential

func (m recordedCredentials) APIKeyCredential(_ context.Context, id identity.APIKeyID) (identity.Credential, bool, error) {
	c, ok := m[id]
	return c, ok, nil
}

// TestIntegrationIdentityLifecyclePersistsAndVerifies walks the foundation's
// whole arc against the real database: account, user, key, retrieve, verify,
// revoke, verify refused — the path the phase spec pins as the integration
// tier's spine.
func TestIntegrationIdentityLifecyclePersistsAndVerifies(t *testing.T) {
	_, accounts, users, keys := integrationIdentity(t)
	ctx := t.Context()

	account := integrationAccount(t, accounts, "integration lifecycle probe")
	if account.State != identity.AccountActive || account.CreatedAt.IsZero() {
		t.Fatalf("created account = %+v, want born active with a creation stamp", account)
	}

	// Retrieve: the row round-trips whole, timestamps included.
	stored, err := accounts.ByID(ctx, account.ID)
	if err != nil {
		t.Fatalf("read account back: %v", err)
	}
	if stored.Name != "integration lifecycle probe" || stored.State != identity.AccountActive || stored.CreatedAt.IsZero() || stored.UpdatedAt.IsZero() {
		t.Fatalf("read back = %+v, want the stored aggregate with both stamps", stored)
	}

	user := integrationUser(t, users, account.ID)
	if user.State != identity.UserInvited {
		t.Fatalf("created user is %q, want invited", user.State)
	}
	readUser, err := users.ByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user back: %v", err)
	}
	if readUser.Email != user.Email {
		t.Fatalf("read back email %q, want %q", readUser.Email, user.Email)
	}

	// Mint: an ownership row only — the digest never enters this database,
	// which the dedicated scan below proves rather than trusts.
	secret, err := identity.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	keyID, err := identity.NewAPIKeyID()
	if err != nil {
		t.Fatalf("NewAPIKeyID: %v", err)
	}
	key, err := identity.NewAPIKey(keyID, account.ID, user.ID, "deploy key", time.Now().UTC())
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	token, err := identity.FormatToken(key.ID, secret)
	if err != nil {
		t.Fatalf("FormatToken: %v", err)
	}
	if err := keys.Create(ctx, *key); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	storedKey, err := keys.ByID(ctx, key.ID)
	if err != nil {
		t.Fatalf("read api key back: %v", err)
	}
	if storedKey.State != identity.APIKeyActive || storedKey.RevokedAt != nil || storedKey.Prefix != identity.TokenPrefix(key.ID) {
		t.Fatalf("read back key = %+v, want an active key, unrevoked, with the id-derived prefix", storedKey)
	}

	// Verify the minted token against the credential the ownership rows and
	// the in-memory digest describe.
	source := recordedCredentials{key.ID: {
		KeyID:        storedKey.ID,
		Digest:       secret.Digest(),
		KeyState:     storedKey.State,
		AccountID:    storedKey.AccountID,
		AccountState: stored.State,
	}}
	parsedID, parsedSecret, err := identity.ParseToken(token)
	if err != nil {
		t.Fatalf("ParseToken on the minted token: %v", err)
	}
	principal, err := identity.VerifyCredential(parsedID, parsedSecret, source[parsedID])
	if err != nil {
		t.Fatalf("VerifyCredential on a freshly minted key: %v", err)
	}
	if principal != identity.APIKeyPrincipal(account.ID, key.ID) {
		t.Fatalf("principal = %+v, want the key's principal under its account", principal)
	}

	// Revoke: one applied swap, the stamp set, and the same token refused.
	applied, err := keys.Revoke(ctx, key.ID, time.Now().UTC())
	if err != nil || !applied {
		t.Fatalf("revoke = (%t, %v), want (true, nil)", applied, err)
	}
	revoked, err := keys.ByID(ctx, key.ID)
	if err != nil {
		t.Fatalf("read revoked key: %v", err)
	}
	if revoked.State != identity.APIKeyRevoked || revoked.RevokedAt == nil {
		t.Fatalf("revoked key = %+v, want revoked with a stamp", revoked)
	}
	source[key.ID] = identity.Credential{
		KeyID:        revoked.ID,
		Digest:       secret.Digest(),
		KeyState:     revoked.State,
		AccountID:    revoked.AccountID,
		AccountState: identity.AccountActive,
	}
	parsedID, parsedSecret, _ = identity.ParseToken(token)
	if _, err := identity.VerifyCredential(parsedID, parsedSecret, source[parsedID]); !errors.Is(err, identity.ErrKeyRevoked) {
		t.Fatalf("verify a revoked key returned %v, want identity.ErrKeyRevoked", err)
	}

	// And an idempotent second revocation loses its swap and confirms by read.
	if applied, err := keys.Revoke(ctx, key.ID, time.Now().UTC()); err != nil || applied {
		t.Fatalf("second revoke = (%t, %v), want (false, nil) — the row left active state exactly once", applied, err)
	}
}

// TestIntegrationTheDatabaseEnforcesTheIdentityRules drives the refusals the
// migration promises through the adapter, so the port's error vocabulary is
// proven against the driver that actually reports the condition — not only
// against a fake instructed to imitate it.
func TestIntegrationTheDatabaseEnforcesTheIdentityRules(t *testing.T) {
	_, accounts, users, keys := integrationIdentity(t)
	ctx := t.Context()
	account := integrationAccount(t, accounts, "integration rules probe")

	// Two accounts may each hold the same address; one account may not.
	first := integrationUser(t, users, account.ID)
	other := integrationAccount(t, accounts, "integration rules probe, other account")

	otherID, err := identity.NewUserID()
	if err != nil {
		t.Fatalf("NewUserID: %v", err)
	}
	twin, err := identity.NewUser(otherID, other.ID, first.Email, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewUser with the first account's address: %v", err)
	}
	if err := users.Create(ctx, *twin); err != nil {
		t.Fatalf("the same address under another account was refused: %v — uniqueness is per account", err)
	}

	dupID, err := identity.NewUserID()
	if err != nil {
		t.Fatalf("NewUserID: %v", err)
	}
	duplicate, err := identity.NewUser(dupID, account.ID, first.Email, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewUser duplicating a live address: %v", err)
	}
	if err := users.Create(ctx, *duplicate); !errors.Is(err, identity.ErrUserEmailTaken) {
		t.Fatalf("duplicate live address returned %v, want identity.ErrUserEmailTaken", err)
	}

	// Removal frees the address for that account, and only that account's
	// rule: the partial index ignores removed rows.
	if applied, err := users.TransitionState(ctx, first.ID, identity.UserInvited, identity.UserRemoved, time.Now().UTC()); err != nil || !applied {
		t.Fatalf("remove first user = (%t, %v), want (true, nil)", applied, err)
	}
	reID, err := identity.NewUserID()
	if err != nil {
		t.Fatalf("NewUserID: %v", err)
	}
	reinvited, err := identity.NewUser(reID, account.ID, first.Email, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewUser reusing a removed row's address: %v", err)
	}
	if err := users.Create(ctx, *reinvited); err != nil {
		t.Fatalf("re-inviting a removed row's address was refused: %v", err)
	}

	// Foreign keys: a user under an unknown account and a key with an unknown
	// creator are refused by the database, through the adapter, as errors —
	// not mapped to anything the domain names, because they are infrastructure
	// refusing an impossible graph, not a domain rule firing.
	orphanID, err := identity.NewUserID()
	if err != nil {
		t.Fatalf("NewUserID: %v", err)
	}
	orphan, err := identity.NewUser(orphanID, "a0000000-0000-0000-0000-0000000000ff", "it-orphan@example.com", time.Now().UTC())
	if err != nil {
		t.Fatalf("NewUser under an unknown account: %v", err)
	}
	if err := users.Create(ctx, *orphan); err == nil || errors.Is(err, identity.ErrUserEmailTaken) {
		t.Fatalf("a user under an unknown account returned %v, want the foreign-key refusal", err)
	}

	keyID, err := identity.NewAPIKeyID()
	if err != nil {
		t.Fatalf("NewAPIKeyID: %v", err)
	}
	adopted, err := identity.NewAPIKey(keyID, account.ID, "b0000000-0000-0000-0000-0000000000ff", "orphaned key", time.Now().UTC())
	if err != nil {
		t.Fatalf("NewAPIKey with an unknown creator: %v", err)
	}
	if err := keys.Create(ctx, *adopted); err == nil {
		t.Fatal("a key naming an unknown creator was stored — the ownership graph bent")
	}
}

// TestIntegrationSwapsReportWhetherTheyApplied pins the compare-and-swap's
// real-database semantics: one statement, applied exactly when the row still
// held the from-state, never an error when it did not.
func TestIntegrationSwapsReportWhetherTheyApplied(t *testing.T) {
	_, accounts, _, keys := integrationIdentity(t)
	ctx := t.Context()
	account := integrationAccount(t, accounts, "integration swaps probe")

	applied, err := accounts.TransitionState(ctx, account.ID, identity.AccountActive, identity.AccountSuspended, time.Now().UTC())
	if err != nil || !applied {
		t.Fatalf("first swap = (%t, %v), want (true, nil)", applied, err)
	}
	applied, err = accounts.TransitionState(ctx, account.ID, identity.AccountActive, identity.AccountSuspended, time.Now().UTC())
	if err != nil || applied {
		t.Fatalf("repeat swap = (%t, %v), want (false, nil) — the row no longer holds the from-state", applied, err)
	}
	applied, err = accounts.TransitionState(ctx, "a0000000-0000-0000-0000-0000000000ff", identity.AccountActive, identity.AccountSuspended, time.Now().UTC())
	if err != nil || applied {
		t.Fatalf("swap on an unknown id = (%t, %v), want (false, nil) — a verdict, not a failure", applied, err)
	}

	keyID, err := identity.NewAPIKeyID()
	if err != nil {
		t.Fatalf("NewAPIKeyID: %v", err)
	}
	key, err := identity.NewAPIKey(keyID, account.ID, "", "swap probe", time.Now().UTC())
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if err := keys.Create(ctx, *key); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	applied, err = keys.Revoke(ctx, key.ID, time.Now().UTC())
	if err != nil || !applied {
		t.Fatalf("first revoke = (%t, %v), want (true, nil)", applied, err)
	}
	applied, err = keys.Revoke(ctx, key.ID, time.Now().UTC())
	if err != nil || applied {
		t.Fatalf("second revoke = (%t, %v), want (false, nil)", applied, err)
	}
}

// TestIntegrationConcurrentRevocationsAwardExactlyOneSwap races revokers at
// the repository: however the scheduler interleaves them, exactly one swap can
// match the active row, every loser gets a clean false — and the row ends
// revoked once, with one stamp. Run with -race; this is the revocation-versus-
// revocation half of the phase's race analysis against real PostgreSQL.
func TestIntegrationConcurrentRevocationsAwardExactlyOneSwap(t *testing.T) {
	_, accounts, _, keys := integrationIdentity(t)
	ctx := t.Context()
	account := integrationAccount(t, accounts, "integration revoke race probe")
	keyID, err := identity.NewAPIKeyID()
	if err != nil {
		t.Fatalf("NewAPIKeyID: %v", err)
	}
	key, err := identity.NewAPIKey(keyID, account.ID, "", "race probe", time.Now().UTC())
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if err := keys.Create(ctx, *key); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	const racers = 8
	applies := make(chan bool, racers)
	failures := make(chan error, racers)
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			applied, err := keys.Revoke(ctx, keyID, time.Now().UTC())
			if err != nil {
				failures <- err
				return
			}
			applies <- applied
		}()
	}
	wg.Wait()
	close(applies)
	close(failures)
	for err := range failures {
		t.Errorf("a racing revocation failed: %v — losing a swap is a verdict, never an error", err)
	}
	applied := 0
	for a := range applies {
		if a {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("%d of %d racing revocations reported their swap applied, want exactly 1 — active can be left once", applied, racers)
	}
	stored, err := keys.ByID(ctx, keyID)
	if err != nil {
		t.Fatalf("read the raced key: %v", err)
	}
	if stored.State != identity.APIKeyRevoked || stored.RevokedAt == nil {
		t.Fatalf("raced key = %+v, want revoked with exactly the winner's stamp", stored)
	}
}

// TestIntegrationConcurrentTransitionsConvergeOnTheSameMove races eight
// callers driving one account to suspended through the read–apply–swap shape
// the use cases use: the winner's swap lands, everyone else's reports false or
// re-reads a row that no longer needs moving, and the aggregate ends
// suspended. Run with -race.
func TestIntegrationConcurrentTransitionsConvergeOnTheSameMove(t *testing.T) {
	_, accounts, _, _ := integrationIdentity(t)
	ctx := t.Context()
	account := integrationAccount(t, accounts, "integration transition race probe")

	const racers = 8
	type outcome struct {
		applied bool
		skipped bool // read a row that already held the target state
	}
	outcomes := make(chan outcome, racers)
	failures := make(chan error, racers)
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			current, err := accounts.ByID(ctx, account.ID)
			if err != nil {
				failures <- err
				return
			}
			if current.State == identity.AccountSuspended {
				outcomes <- outcome{skipped: true}
				return
			}
			applied, err := accounts.TransitionState(ctx, account.ID, current.State, identity.AccountSuspended, time.Now().UTC())
			if err != nil {
				failures <- err
				return
			}
			outcomes <- outcome{applied: applied}
		}()
	}
	wg.Wait()
	close(outcomes)
	close(failures)
	for err := range failures {
		t.Errorf("a racing transition failed: %v", err)
	}
	applied := 0
	for o := range outcomes {
		if o.applied {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("%d of %d racing transitions reported their swap applied, want exactly 1 — active can be left once", applied, racers)
	}
	stored, err := accounts.ByID(ctx, account.ID)
	if err != nil {
		t.Fatalf("read the raced account: %v", err)
	}
	if stored.State != identity.AccountSuspended {
		t.Fatalf("raced account is %q, want suspended", stored.State)
	}
}

// TestIntegrationNothingPersistedCarriesSecretMaterial is the security
// regression: mint a token whose every byte this test knows, write the
// ownership row, then read every column of every row the flow touched as
// PostgreSQL itself serialises it and demand the secret segment — and the
// token — appear nowhere. Not a digest, not a prefix of it, not in a column
// someone later added without telling the security review.
func TestIntegrationNothingPersistedCarriesSecretMaterial(t *testing.T) {
	db, accounts, users, keys := integrationIdentity(t)
	ctx := t.Context()
	account := integrationAccount(t, accounts, "integration secret scan probe")
	user := integrationUser(t, users, account.ID)

	secret, err := identity.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	keyID, err := identity.NewAPIKeyID()
	if err != nil {
		t.Fatalf("NewAPIKeyID: %v", err)
	}
	key, err := identity.NewAPIKey(keyID, account.ID, user.ID, "scan probe", time.Now().UTC())
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	token, err := identity.FormatToken(key.ID, secret)
	if err != nil {
		t.Fatalf("FormatToken: %v", err)
	}
	secretSegment := strings.SplitN(token, "_", 3)[2]
	if err := keys.Create(ctx, *key); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	for _, probe := range []struct {
		table string
		id    string
	}{
		{"control.api_keys", string(key.ID)},
		{"control.users", string(user.ID)},
		{"control.accounts", string(account.ID)},
	} {
		var serialised string
		if err := db.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT to_jsonb(t)::text FROM %s t WHERE id = $1`, probe.table),
			probe.id,
		).Scan(&serialised); err != nil {
			t.Fatalf("serialise the %s row: %v", probe.table, err)
		}
		if strings.Contains(serialised, token) {
			// The row is deliberately not printed: a failure message is
			// output too, and this test exists precisely because the row
			// must never carry the token anywhere.
			t.Fatalf("the %s row carries the full token", probe.table)
		}
		if strings.Contains(serialised, secretSegment) {
			t.Fatalf("the %s row carries the token's secret segment", probe.table)
		}
	}
}
