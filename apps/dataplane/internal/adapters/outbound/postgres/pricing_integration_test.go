//go:build integration

package postgres

// The price book's integration suite: the two-step selection the client
// price list migration pins, driven through the public PriceBook port. The
// selection runs against the database's own clock (transaction_timestamp()),
// so the effective_from instants here are placed hours apart — margins no
// application/database clock skew can cross.
//
// It runs on a THROWAWAY database on purpose, and not for isolation's sake:
// "no activated revision is effective" is a state the shared fixture database
// can no longer be in once any run has activated a revision whose
// effective_from has arrived, and the selection's refusal of that state is
// the first scenario. A database of its own starts the price list from an
// honest empty.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// TestIntegrationPriceBookSelectsTheGreatestEffectiveRevision walks the
// selection through its whole shape: an empty price list refuses, a draft
// never answers whatever its effective_from says, the greatest activated
// effective_from not after the instant wins, and an alias the effective
// revision does not price is unpriced — alias-exact, never a neighbour's
// price, never zero.
func TestIntegrationPriceBookSelectsTheGreatestEffectiveRevision(t *testing.T) {
	integrationThrowawaySerialise(t)
	db := integrationThrowawayDatabase(t, "dataplane_price_book_probe")
	integrationRuntimeSchema(t, db)
	store := New(db)
	book := NewPriceBook(store)
	repos := integrationRepos(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// One priced alias and one unpriced one.
	scope := repos.catalogScope(t)
	unpriced := integrationSeedUnpricedAlias(t, ctx, store)

	// An empty price list: no revision activated yet — a configuration state
	// the caller refuses, never a price of zero.
	if _, err := book.EffectiveAt(ctx, scope.alias); !errors.Is(err, persistence.ErrNoEffectivePrice) {
		t.Fatalf("EffectiveAt on an empty price list error = %v, want ErrNoEffectivePrice", err)
	}

	hoursAgo := time.Now().UTC().Add(-2 * time.Hour)
	first := integrationSeedRevision(t, ctx, store, 1, "activated", hoursAgo)
	integrationSeedEntry(t, ctx, store, first, scope.alias, 20, 30)

	// One activated revision, effective two hours ago: it answers, with its
	// own identity and prices.
	snapshot, err := book.EffectiveAt(ctx, scope.alias)
	if err != nil {
		t.Fatalf("EffectiveAt with one effective revision: %v", err)
	}
	if want := (catalog.PriceSnapshot{RevisionID: first, Version: 1, InputUnitPrice: 20, OutputUnitPrice: 30}); snapshot != want {
		t.Errorf("EffectiveAt = %+v, want %+v — the revision's own identity and prices", snapshot, want)
	}

	// A draft whose effective_from has already arrived changes nothing: only
	// activation spends an instant, and a draft never answers. The draft's
	// instant is kept — the totality probe below activates a rival at exactly
	// this value, which is the only way two instants can collide.
	sharedInstant := time.Now().UTC().Add(-time.Hour)
	draft := integrationSeedRevision(t, ctx, store, 2, "draft", sharedInstant)
	integrationSeedEntry(t, ctx, store, draft, scope.alias, 40, 50)
	if snapshot, err := book.EffectiveAt(ctx, scope.alias); err != nil || snapshot.Version != 1 {
		t.Errorf("EffectiveAt beside a draft = (%+v, %v), want the activated revision 1 — a draft is invisible to the selection", snapshot, err)
	}

	// Activating the second revision makes it effective: greatest activated
	// effective_from not after the instant now names it.
	integrationActivateRevision(t, ctx, store, draft)
	if snapshot, err := book.EffectiveAt(ctx, scope.alias); err != nil || snapshot.Version != 2 || snapshot.InputUnitPrice != 40 || snapshot.OutputUnitPrice != 50 {
		t.Errorf("EffectiveAt after activation = (%+v, %v), want revision 2 at (40, 50)", snapshot, err)
	}

	// A scheduled revision whose instant has not arrived changes nothing yet.
	future := integrationSeedRevision(t, ctx, store, 3, "draft", time.Now().UTC().Add(time.Hour))
	integrationSeedEntry(t, ctx, store, future, scope.alias, 60, 70)
	if snapshot, err := book.EffectiveAt(ctx, scope.alias); err != nil || snapshot.Version != 2 {
		t.Errorf("EffectiveAt beside a future revision = (%+v, %v), want the effective revision 2", snapshot, err)
	}

	// The unpriced alias: the effective revision prices no entry for it. Same
	// sentinel — and the error names the alias, so the refusal is
	// diagnosable.
	_, err = book.EffectiveAt(ctx, unpriced)
	if !errors.Is(err, persistence.ErrNoEffectivePrice) {
		t.Fatalf("EffectiveAt for the unpriced alias error = %v, want ErrNoEffectivePrice", err)
	}
	if text := err.Error(); !strings.Contains(text, string(unpriced)) || !strings.Contains(text, "prices no entry") {
		t.Errorf("the unpriced alias's error = %q, want it to name the alias and the missing-entry reason", text)
	}

	// Two activated revisions may not share an effective_from: the partial
	// unique is what makes "the single activated revision with the greatest
	// effective_from" a total selection. The rival is seeded at the exact
	// instant the activated revision above spent — its microsecond value, not
	// merely its hour.
	var pgErr *pgconn.PgError
	shared := integrationSeedRevision(t, ctx, store, 4, "draft", sharedInstant)
	_, err = db.ExecContext(ctx, `UPDATE client_price_list_revisions
		SET state = 'activated', activated_at = transaction_timestamp() WHERE id = $1`, shared)
	if err == nil {
		t.Fatalf("activating a second revision at an activated instant succeeded, want the partial unique's refusal")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "client_price_list_revisions_activated_effective_from_key" {
		t.Errorf("the shared-instant activation error = %v, want 23505 on client_price_list_revisions_activated_effective_from_key", err)
	}

	// A revision prices an alias at most once: alias-exactness is a unique,
	// not a convention.
	_, err = db.ExecContext(ctx, `INSERT INTO client_price_list_entries (revision_id, alias_id, input_unit_price, output_unit_price)
		VALUES ($1, $2, 20, 30)`, first, string(scope.alias))
	if err == nil {
		t.Fatalf("the duplicate entry insert succeeded, want the revision-alias unique's refusal")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "client_price_list_entries_revision_alias_key" {
		t.Errorf("the duplicate entry error = %v, want 23505 on client_price_list_entries_revision_alias_key", err)
	}

	// Zero is a legitimate price — a free tier is a row, not an absence — and
	// a negative one is not a price at all. Both probes ride their own
	// revision, so neither collides with an entry the selection steps above
	// already wrote.
	bounds := integrationSeedRevision(t, ctx, store, 5, "draft", time.Now().UTC().Add(48*time.Hour))
	if _, err := db.ExecContext(ctx, `INSERT INTO client_price_list_entries (revision_id, alias_id, input_unit_price, output_unit_price)
		VALUES ($1, $2, 0, 0)`, bounds, unpriced); err != nil {
		t.Errorf("the zero-price entry insert error = %v, want it accepted — zero prices are legitimate", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO client_price_list_entries (revision_id, alias_id, input_unit_price, output_unit_price)
		VALUES ($1, $2, -1, 0)`, bounds, string(scope.alias))
	if err == nil {
		t.Fatalf("the negative-price entry insert succeeded, want the non-negative CHECK's refusal")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "client_price_list_entries_input_unit_price_non_negative" {
		t.Errorf("the negative-price entry error = %v, want 23514 on client_price_list_entries_input_unit_price_non_negative", err)
	}
}

// TestIntegrationPriceBookRefusesWithoutARevision is the missing-revision
// half of the refusal on a database that has carried a price list before:
// every revision activated here is either not yet effective or withdrawn from
// the selection, and the read answers the configuration refusal, not a price.
func TestIntegrationPriceBookRefusesWithoutARevision(t *testing.T) {
	integrationThrowawaySerialise(t)
	db := integrationThrowawayDatabase(t, "dataplane_price_book_refusal")
	integrationRuntimeSchema(t, db)
	store := New(db)
	book := NewPriceBook(store)
	repos := integrationRepos(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scope := repos.catalogScope(t)

	// A revision whose instant has not arrived: activated, but not yet
	// effective — the operator scheduled it, and the schedule holds.
	notYet := integrationSeedRevision(t, ctx, store, 1, "activated", time.Now().UTC().Add(24*time.Hour))
	integrationSeedEntry(t, ctx, store, notYet, scope.alias, 20, 30)
	if _, err := book.EffectiveAt(ctx, scope.alias); !errors.Is(err, persistence.ErrNoEffectivePrice) {
		t.Fatalf("EffectiveAt before the scheduled instant error = %v, want ErrNoEffectivePrice", err)
	}

	// A draft that is older than time: the same refusal. State, not
	// effective_from, is what the selection filters on first.
	draft := integrationSeedRevision(t, ctx, store, 2, "draft", time.Now().UTC().Add(-24*time.Hour))
	integrationSeedEntry(t, ctx, store, draft, scope.alias, 40, 50)
	if _, err := book.EffectiveAt(ctx, scope.alias); !errors.Is(err, persistence.ErrNoEffectivePrice) {
		t.Errorf("EffectiveAt beside only a past-dated draft error = %v, want ErrNoEffectivePrice", err)
	}
}

// integrationSeedUnpricedAlias mints one alias that no price list entry will
// name: the alias-exactness probe.
func integrationSeedUnpricedAlias(t testing.TB, ctx context.Context, store persistence.Store) catalog.AliasID {
	t.Helper()
	suffix := string(identity.NewRequestID())
	name := "b7it-unpriced-alias-" + suffix[len(suffix)-8:]
	id := string(identity.NewRequestID())
	if _, err := store.Querier(ctx).ExecContext(ctx, `INSERT INTO model_aliases (id, name, state, max_output_tokens, reservation_cap, created_at, updated_at)
VALUES ($1, $2, 'active', 4096, 4096, transaction_timestamp(), transaction_timestamp())`, id, name); err != nil {
		t.Fatalf("seeding the unpriced alias: %v", err)
	}
	return catalog.AliasID(id)
}

// integrationSeedRevision mints one price list revision in the state given,
// effective from the instant given.
func integrationSeedRevision(t testing.TB, ctx context.Context, store persistence.Store, version int, state string, effectiveFrom time.Time) string {
	t.Helper()
	id := string(identity.NewRequestID())
	activatedAt := any(nil)
	if state == "activated" {
		activatedAt = effectiveFrom
	}
	if _, err := store.Querier(ctx).ExecContext(ctx, `INSERT INTO client_price_list_revisions (id, version, state, effective_from, activated_at, created_at)
VALUES ($1, $2, $3, $4, $5, transaction_timestamp())`, id, version, state, effectiveFrom, activatedAt); err != nil {
		t.Fatalf("seeding revision v%d: %v", version, err)
	}
	return id
}

// integrationActivateRevision flips a draft to activated, as the operator's
// activation step would.
func integrationActivateRevision(t testing.TB, ctx context.Context, store persistence.Store, id string) {
	t.Helper()
	if _, err := store.Querier(ctx).ExecContext(ctx, `UPDATE client_price_list_revisions
		SET state = 'activated', activated_at = transaction_timestamp() WHERE id = $1`, id); err != nil {
		t.Fatalf("activating revision %s: %v", id, err)
	}
}

// integrationSeedEntry writes one revision's price for one alias, in integer
// minor units per 1M tokens.
func integrationSeedEntry(t testing.TB, ctx context.Context, store persistence.Store, revisionID string, aliasID catalog.AliasID, input, output int64) {
	t.Helper()
	if _, err := store.Querier(ctx).ExecContext(ctx, `INSERT INTO client_price_list_entries (revision_id, alias_id, input_unit_price, output_unit_price)
VALUES ($1, $2, $3, $4)`, revisionID, string(aliasID), input, output); err != nil {
		t.Fatalf("seeding revision %s's entry: %v", revisionID, err)
	}
}
