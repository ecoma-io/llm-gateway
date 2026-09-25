//go:build integration

package postgres

// The drawdown eligibility suite: the half of the waterfall the landed
// runtime-storage tests do not vary — WHOSE grant may fund a request. A grant
// funds a request only when its stored scope contains the requesting alias
// (the wildcard `*` version contains every alias; a named version exactly the
// aliases its snapshot lists; a dangling version contains nothing) and its
// cycle has not ended at the transaction's instant. Every test here drives
// the eligibility rule through the public Drawdown port against a real
// database, because the rule lives in the walk's and the take's SQL WHERE
// clauses, not in Go.
//
// The statements and helpers this file leans on — integrationPublish,
// integrationSeedProjection, integrationProjectionRow, catalogScope — are the
// runtime-storage suite's; they are deliberately not duplicated here.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// integrationPublishScoped delivers one grant whose stored scope the caller
// chooses outright — the shape the Control Plane's publications arrive in,
// and the shape the eligibility tests need, because their subject is exactly
// the grant's stored scope. A zero period end publishes the PAYG scope, a
// non-zero one an entitlement cycle, as integrationPublish does.
func integrationPublishScoped(t testing.TB, ctx context.Context, repos integrationRepositories, account, bucket, versionID string, namedScope bool, periodEnd time.Time, limit int64) {
	t.Helper()
	publication := accounting.Publication{
		AccountID:             account,
		FundingBucketID:       bucket,
		AliasGroupVersionID:   versionID,
		ScopeKind:             accounting.ScopePayGBalance,
		NamedScope:            namedScope,
		PeriodEnd:             periodEnd,
		SubscriptionCreatedAt: integrationSubscriptionCreatedAt,
		State:                 accounting.ProjectionActive,
		LimitAmount:           limit,
		Revision:              1,
	}
	if !periodEnd.IsZero() {
		publication.ScopeKind = accounting.ScopeEntitlementCycle
		publication.EntitlementID = "b7it-entitlement-" + bucket
		publication.CycleNumber = 1
	}
	outcome, err := repos.quota.ApplyPublication(ctx, publication)
	if err != nil {
		t.Fatalf("ApplyPublication(%s): %v", bucket, err)
	}
	if outcome != accounting.PublicationSeeded {
		t.Fatalf("ApplyPublication(%s) = %q, want %q", bucket, outcome, accounting.PublicationSeeded)
	}
}

// integrationForeignScope mints an alias the suite's alias is not, and one
// named group version whose snapshot contains only that foreign alias: the
// scope a grant carries when its group simply does not include the alias
// asking to be funded. The alias comes back by id — the form grants and
// drawdowns speak, not the operator-facing name.
func integrationForeignScope(t testing.TB, ctx context.Context, repos integrationRepositories) (foreignAlias catalog.AliasID, foreignVersion string) {
	t.Helper()
	querier := repos.store.Querier(ctx)
	foreignID := string(identity.NewRequestID())
	foreignName := "b7it-foreign-alias-" + foreignID[len(foreignID)-8:]
	if _, err := querier.ExecContext(ctx, `INSERT INTO model_aliases (id, name, state, max_output_tokens, reservation_cap, created_at, updated_at)
VALUES ($1, $2, 'active', 4096, 4096, transaction_timestamp(), transaction_timestamp())`, foreignID, foreignName); err != nil {
		t.Fatalf("seeding the foreign alias: %v", err)
	}
	foreignVersion = string(identity.NewRequestID())
	foreignGroup := "b7it-foreign-group-" + foreignVersion[len(foreignVersion)-8:]
	if _, err := querier.ExecContext(ctx, `INSERT INTO alias_group_versions (id, group_name, version, created_at)
VALUES ($1, $2, 1, transaction_timestamp())`, foreignVersion, foreignGroup); err != nil {
		t.Fatalf("seeding the foreign group version: %v", err)
	}
	if _, err := querier.ExecContext(ctx, `INSERT INTO alias_group_members (group_version_id, alias_id)
VALUES ($1, $2)`, foreignVersion, foreignID); err != nil {
		t.Fatalf("seeding the foreign group version's membership: %v", err)
	}
	return catalog.AliasID(foreignID), foreignVersion
}

// TestIntegrationDrawdownSkipsAGrantWhoseScopeExcludesTheAlias: a grant whose
// stored scope is a named version that does not contain the alias is not a
// bucket the walk may draw, and neither is a grant whose version id dangles —
// one no group version answers for at all. Both are treat-as-zero: the
// drawdown answers the shortfall sentinel, no leg names the excluded bucket,
// and nothing errors — a mis-scoped grant is a configuration state, not a
// malfunction.
func TestIntegrationDrawdownSkipsAGrantWhoseScopeExcludesTheAlias(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scope := repos.catalogScope(t)
	foreignAlias, foreignVersion := integrationForeignScope(t, ctx, repos)

	live := time.Now().UTC().Add(24 * time.Hour)

	// The excluded grant: pinned to a named version whose snapshot contains
	// only the foreign alias.
	excluded := integrationRuntimeAccount(t, "scope-excluded")
	excludedBucket := excluded + "-bucket"
	integrationPublishScoped(t, ctx, repos, excluded, excludedBucket, foreignVersion, true, live, 500)

	// The dangling grant: pinned to a version id no alias_group_versions row
	// answers for — the shape of a grant whose snapshot was lost, or of a
	// publication that ran ahead of its catalog.
	dangling := integrationRuntimeAccount(t, "scope-dangling")
	danglingBucket := dangling + "-bucket"
	integrationPublishScoped(t, ctx, repos, dangling, danglingBucket, "b7it-no-such-group-version-"+dangling[len(dangling)-8:], true, live, 500)

	for _, tt := range []struct {
		name    string
		account string
		bucket  string
	}{
		{name: "a named version that excludes the alias", account: excluded, bucket: excludedBucket},
		{name: "a version id nothing answers for", account: dangling, bucket: danglingBucket},
	} {
		legs, err := repos.quota.Drawdown(ctx, tt.account, scope.alias, 100)
		if !errors.Is(err, accounting.ErrInsufficientCapacity) {
			t.Errorf("%s: Drawdown(100) error = %v, want ErrInsufficientCapacity", tt.name, err)
		}
		var shortfall *persistence.InsufficientCapacityError
		if !errors.As(err, &shortfall) {
			t.Errorf("%s: Drawdown(100) error = %v, want it typed as InsufficientCapacityError", tt.name, err)
		} else if shortfall.EligibleRowSeen {
			t.Errorf("%s: EligibleRowSeen = true, want false — the exclusion is the scope's, so the walk saw no eligible grant", tt.name)
		}
		if len(legs) != 0 {
			t.Errorf("%s: Drawdown(100) = %v, want no legs — the excluded grant must not be drawn", tt.name, legs)
		}
		if available, _, _, _ := integrationProjectionRow(t, db, tt.bucket); available != 500 {
			t.Errorf("%s: available after the refused draw = %d, want 500 — nothing moved", tt.name, available)
		}
	}

	// The exclusion is the scope's, not the grant's: the very same grant does
	// fund the alias its snapshot names.
	legs, err := repos.quota.Drawdown(ctx, excluded, foreignAlias, 100)
	if err != nil {
		t.Fatalf("Drawdown(100) for the grant's own alias: %v", err)
	}
	if len(legs) != 1 || legs[0].Amount != 100 || legs[0].FundingBucketID != excludedBucket {
		t.Fatalf("Drawdown(100) for the grant's own alias = %v, want one leg of 100 from %s", legs, excludedBucket)
	}
}

// TestIntegrationDrawdownShortfallNamesWhetherAnyGrantWasEligible: the other
// shortfall shape, beside the scope exclusions above. A grant whose scope
// contains the alias and whose cycle is live but whose available cannot cover
// the hold is a real shortage: the shortfall still answers the sentinel, and
// the typed error names the difference — EligibleRowSeen true, because the
// walk saw a grant it was allowed to draw from, just not enough of one.
func TestIntegrationDrawdownShortfallNamesWhetherAnyGrantWasEligible(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scope := repos.catalogScope(t)
	account := integrationRuntimeAccount(t, "eligible-short")
	bucket := account + "-bucket"
	integrationPublishScoped(t, ctx, repos, account, bucket, scope.wildcard, false,
		time.Now().UTC().Add(24*time.Hour), 100)

	legs, err := repos.quota.Drawdown(ctx, account, scope.alias, 250)
	if !errors.Is(err, accounting.ErrInsufficientCapacity) {
		t.Fatalf("Drawdown(250) error = %v, want ErrInsufficientCapacity", err)
	}
	var shortfall *persistence.InsufficientCapacityError
	if !errors.As(err, &shortfall) {
		t.Fatalf("Drawdown(250) error = %T, want *persistence.InsufficientCapacityError", err)
	}
	if !shortfall.EligibleRowSeen {
		t.Errorf("EligibleRowSeen = false, want true — the grant's scope contains the alias, so the walk saw it; it was just too small")
	}
	if len(legs) != 0 {
		t.Errorf("Drawdown(250) = %v, want no legs — a shortfall draws nothing", legs)
	}
	if available, _, _, _ := integrationProjectionRow(t, db, bucket); available != 100 {
		t.Errorf("available after the refused draw = %d, want 100 — the giveback restored what the walk took", available)
	}
}

// TestIntegrationDrawdownSkipsAGrantWhoseCycleHasEnded: an entitlement cycle
// is eligible only while its period_end is still ahead of the transaction's
// instant — an ended cycle is not a smaller grant, it is no grant. The live
// cycle beside it is drawn, so the test proves exclusion and eligibility with
// one drawdown: the walk passes the ended grant by and funds from the live
// one.
func TestIntegrationDrawdownSkipsAGrantWhoseCycleHasEnded(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scope := repos.catalogScope(t)
	account := integrationRuntimeAccount(t, "cycle-expired")

	integrationPublishScoped(t, ctx, repos, account, account+"-ended", scope.wildcard, false,
		time.Now().UTC().Add(-time.Hour), 500)
	integrationPublishScoped(t, ctx, repos, account, account+"-live", scope.wildcard, false,
		time.Now().UTC().Add(24*time.Hour), 500)

	legs, err := repos.quota.Drawdown(ctx, account, scope.alias, 250)
	if err != nil {
		t.Fatalf("Drawdown(250): %v", err)
	}
	if len(legs) != 1 || legs[0].Amount != 250 || legs[0].FundingBucketID != account+"-live" {
		t.Fatalf("Drawdown(250) = %v, want one leg of 250 from the live cycle — the ended cycle is not a bucket", legs)
	}
	if available, _, _, _ := integrationProjectionRow(t, db, account+"-ended"); available != 500 {
		t.Errorf("the ended cycle's available = %d, want 500 — an expired grant is never drawn", available)
	}
	if available, _, _, _ := integrationProjectionRow(t, db, account+"-live"); available != 250 {
		t.Errorf("the live cycle's available = %d, want 250", available)
	}
}

// TestIntegrationDrawdownDrawsTheWildcardGrantWithoutMemberRows: a grant
// pinned to the wildcard `*` version funds any alias — by definition, not by
// membership, which is why the schema keeps no member rows for it at all. The
// test pins that definition twice: the drawdown draws the grant, and the
// version it is pinned to provably has an empty snapshot.
func TestIntegrationDrawdownDrawsTheWildcardGrantWithoutMemberRows(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scope := repos.catalogScope(t)
	account := integrationRuntimeAccount(t, "wildcard")
	bucket := account + "-bucket"
	integrationPublishScoped(t, ctx, repos, account, bucket, scope.wildcard, false,
		time.Now().UTC().Add(24*time.Hour), 500)

	var members int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM public.alias_group_members WHERE group_version_id = $1`, scope.wildcard).Scan(&members); err != nil {
		t.Fatalf("counting the wildcard version's member rows: %v", err)
	}
	if members != 0 {
		t.Fatalf("the wildcard version carries %d member rows, want 0 — its membership is the empty set by definition", members)
	}

	legs, err := repos.quota.Drawdown(ctx, account, scope.alias, 250)
	if err != nil {
		t.Fatalf("Drawdown(250): %v", err)
	}
	if len(legs) != 1 || legs[0].Amount != 250 || legs[0].FundingBucketID != bucket {
		t.Fatalf("Drawdown(250) = %v, want one leg of 250 from %s — the wildcard contains every alias without a single member row", legs, bucket)
	}
}

// TestIntegrationDrawdownDrawsAGrantInANamedGroupContainingTheAlias: the
// other half of containment — a grant pinned to a named version whose
// snapshot names the alias is drawn, on the same rule that excludes the
// foreign versions above.
func TestIntegrationDrawdownDrawsAGrantInANamedGroupContainingTheAlias(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scope := repos.catalogScope(t)
	account := integrationRuntimeAccount(t, "named")
	bucket := account + "-bucket"
	integrationPublish(t, ctx, repos, account, bucket, true, time.Now().UTC().Add(24*time.Hour), 500, 1, accounting.ProjectionActive)

	legs, err := repos.quota.Drawdown(ctx, account, scope.alias, 250)
	if err != nil {
		t.Fatalf("Drawdown(250): %v", err)
	}
	if len(legs) != 1 || legs[0].Amount != 250 || legs[0].FundingBucketID != bucket {
		t.Fatalf("Drawdown(250) = %v, want one leg of 250 from %s — the grant's named version contains the alias", legs, bucket)
	}
}

// TestIntegrationDrawdownPaysEntitlementCyclesBeforePayg: the waterfall's
// kind rule — entitlement cycles strictly before the PAYG balance, whatever
// their sizes — and the schema guard behind it. The drawdown crosses the
// boundary: the cycle funds first, the balance covers only the remainder.
// Beside it, the guard the migration pins: a PAYG row that claimed a named
// scope cannot exist at all, because an account's own money is not an alias
// group's to scope.
func TestIntegrationDrawdownPaysEntitlementCyclesBeforePayg(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scope := repos.catalogScope(t)
	account := integrationRuntimeAccount(t, "kind-order")
	integrationPublishScoped(t, ctx, repos, account, account+"-cycle", scope.wildcard, false,
		time.Now().UTC().Add(24*time.Hour), 30)
	integrationPublishScoped(t, ctx, repos, account, account+"-balance", scope.wildcard, false,
		time.Time{}, 100)

	legs, err := repos.quota.Drawdown(ctx, account, scope.alias, 50)
	if err != nil {
		t.Fatalf("Drawdown(50): %v", err)
	}
	want := []accounting.Allocation{
		{FundingBucketID: account + "-cycle", Amount: 30, Ordinal: 1},
		{FundingBucketID: account + "-balance", Amount: 20, Ordinal: 2},
	}
	if len(legs) != len(want) || legs[0].FundingBucketID != want[0].FundingBucketID || legs[0].Amount != want[0].Amount || legs[1].FundingBucketID != want[1].FundingBucketID || legs[1].Amount != want[1].Amount {
		t.Errorf("Drawdown(50) = %v, want %v — the cycle funds first, the balance only the remainder", legs, want)
	}

	// The schema's half: the row that would put an alias group between an
	// account and its own money is refused at insert, by the CHECK twin of
	// the ordering this drawdown just walked.
	_, err = db.ExecContext(ctx, `INSERT INTO public.quota_projections
		(account_id, scope_kind, funding_bucket_id, alias_group_version_id, named_scope,
		 dimension, subscription_created_at, state, limit_amount, available)
	VALUES ($1, 'payg_balance', $2, $3, true, 'cost', transaction_timestamp(), 'active', 100, 100)`,
		account, account+"-payg-named", scope.wildcard)
	if err == nil {
		t.Fatalf("the scoped PAYG balance insert succeeded, want quota_projections_payg_scope_wildcard's refusal")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "quota_projections_payg_scope_wildcard" {
		t.Errorf("the scoped PAYG balance insert error = %v, want 23514 on quota_projections_payg_scope_wildcard", err)
	}
}

// TestIntegrationDrawdownOfZeroStillAnswersTheScopeQuestion: a zero amount is
// not a short-circuit past the walk. The use case turns the walk's answer
// into `no_access` or `insufficient_entitlement`, and that classification is
// exactly what a free alias still needs — an account with no entitlement to
// it is refused the same at zero cost as at any cost. A zero draw over an
// eligible grant answers no error and no legs; over a scope that excludes the
// alias, and over an account with no rows at all, it answers the shortfall
// with EligibleRowSeen false. Nothing moves in any case, and the foreign
// grant funds its own alias for free — the walk ran and found it.
func TestIntegrationDrawdownOfZeroStillAnswersTheScopeQuestion(t *testing.T) {
	db, store := integrationPool(t)
	repos := integrationRepos(t, store)
	integrationRuntimeSchema(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scope := repos.catalogScope(t)
	foreignAlias, foreignVersion := integrationForeignScope(t, ctx, repos)
	live := time.Now().UTC().Add(24 * time.Hour)

	eligible := integrationRuntimeAccount(t, "zero-eligible")
	eligibleBucket := eligible + "-bucket"
	integrationPublishScoped(t, ctx, repos, eligible, eligibleBucket, scope.wildcard, false, live, 500)

	excluded := integrationRuntimeAccount(t, "zero-excluded")
	excludedBucket := excluded + "-bucket"
	integrationPublishScoped(t, ctx, repos, excluded, excludedBucket, foreignVersion, true, live, 500)

	absent := integrationRuntimeAccount(t, "zero-absent")

	// Eligible: zero buys nothing, and the walk says so without error.
	legs, err := repos.quota.Drawdown(ctx, eligible, scope.alias, 0)
	if err != nil {
		t.Fatalf("Drawdown(0) over an eligible grant: %v", err)
	}
	if len(legs) != 0 {
		t.Fatalf("Drawdown(0) over an eligible grant = %v, want no legs — zero buys nothing", legs)
	}
	if available, _, _, _ := integrationProjectionRow(t, db, eligibleBucket); available != 500 {
		t.Fatalf("available after the zero draw = %d, want 500 — nothing moved", available)
	}

	for _, tt := range []struct {
		name    string
		account string
	}{
		{name: "a scope that excludes the alias", account: excluded},
		{name: "an account with no grants at all", account: absent},
	} {
		legs, err := repos.quota.Drawdown(ctx, tt.account, scope.alias, 0)
		if !errors.Is(err, accounting.ErrInsufficientCapacity) {
			t.Errorf("%s: Drawdown(0) error = %v, want ErrInsufficientCapacity", tt.name, err)
		}
		var shortfall *persistence.InsufficientCapacityError
		if !errors.As(err, &shortfall) {
			t.Errorf("%s: Drawdown(0) error = %v, want it typed as InsufficientCapacityError", tt.name, err)
		} else if shortfall.EligibleRowSeen {
			t.Errorf("%s: EligibleRowSeen = true, want false — no grant was eligible to draw", tt.name)
		}
		if len(legs) != 0 {
			t.Errorf("%s: Drawdown(0) = %v, want no legs", tt.name, legs)
		}
	}
	if available, _, _, _ := integrationProjectionRow(t, db, excludedBucket); available != 500 {
		t.Errorf("available after the excluded zero draw = %d, want 500", available)
	}

	// The exclusion is the scope's, not the amount's: the same zero draw for
	// the foreign grant's own alias finds the eligible row and answers
	// nothing owed.
	if legs, err := repos.quota.Drawdown(ctx, excluded, foreignAlias, 0); err != nil {
		t.Errorf("Drawdown(0) for the grant's own alias: %v", err)
	} else if len(legs) != 0 {
		t.Errorf("Drawdown(0) for the grant's own alias = %v, want no legs", legs)
	}
}
