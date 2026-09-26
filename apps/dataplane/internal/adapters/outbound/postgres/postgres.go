// Package postgres adapts the persistence port to PostgreSQL through the
// standard library's database/sql — the runtime's adapter to the `dataplane`
// database decided in ADR 0006 §7 (runtime state: the model catalog, routing
// and provider configuration, the quota projection, and the usage history the
// Control Plane reconciles from), operated per deploy/postgres/README.md.
//
// The driver that backs the database/sql shape lives here and only here: the
// blank import below registers pgx's stdlib driver under the name "pgx", it
// is this module's one PostgreSQL-driver import, and nothing else under
// internal/ ever names it. Callers that build a pool hand Options to Open —
// a DSN and four pool settings, no driver name — and callers that are handed
// an already-open pool by other means still construct New around it; the
// orchestration below the pool is identical either way.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	// pgx's stdlib package registers a database/sql driver named "pgx". There
	// is no PostgreSQL driver in the standard library; database/sql is the
	// port's decided shape (internal/ports/outbound/persistence names it
	// because a transaction must be expressible); lib/pq is in maintenance
	// mode; pgx is the maintained database/sql driver, and its stdlib package
	// is the one that speaks that shape. The import is blank: it exists to
	// run the driver's init, which registers it, and this file is the only
	// place the registration happens.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// New returns the persistence port backed by db. The store holds no state of its
// own beyond the pool, so it needs no closing: Close belongs to whoever owns
// the *sql.DB.
//
// It panics on a nil pool rather than storing one, because the failure a nil
// pool produces later — a nil dereference inside Ping, far from the code that
// built the store — is strictly worse than a loud one at the construction
// site.
func New(db *sql.DB) persistence.Store {
	if db == nil {
		panic("postgres: New requires a non-nil *sql.DB — open the database before building the store")
	}
	return &store{db: db}
}

// driverName is the name Open hands to sql.Open. The blank import above is
// what makes the name resolvable; spelling it once beside that import keeps
// the registration and the lookup of it in one place.
const driverName = "pgx"

// ownedDatabase is the database this adapter serves, and the only one it will
// open a pool against. The port this package implements belongs to the Data
// Plane and reaches the `dataplane` database (ADR 0006 §7); the copy of the
// rule here is what keeps that sentence true at the last place a foreign DSN
// could slip through — a caller that never went near the configuration
// package, or a future wiring that built Options by hand, still cannot point
// this adapter at another plane's database. It mirrors the constant of the
// same name in internal/config, where the environment's DSN is checked
// against it at load time; Open repeats the check because it is the last door.
const ownedDatabase = "dataplane"

// Options is the description of one pool: the DSN to open and the four
// settings database/sql exposes on it. It is data, not policy — whether a
// failed Open stops the process is the wiring's decision, as it is for the
// cache adapter's Connect.
type Options struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// validate reports whether the options describe a pool that can be built.
// Each failure names the field an operator would correct, the way the
// configuration loader names its environment variables.
func (o Options) validate() error {
	if o.DSN == "" {
		return errors.New("postgres: DSN is required")
	}
	if o.MaxOpenConns < 1 {
		return fmt.Errorf("postgres: MaxOpenConns %d must be at least 1", o.MaxOpenConns)
	}
	if o.MaxIdleConns < 0 {
		return fmt.Errorf("postgres: MaxIdleConns %d must not be negative", o.MaxIdleConns)
	}
	if o.MaxIdleConns > o.MaxOpenConns {
		return fmt.Errorf("postgres: MaxIdleConns %d must not be greater than MaxOpenConns %d", o.MaxIdleConns, o.MaxOpenConns)
	}
	if o.ConnMaxLifetime < 0 {
		return fmt.Errorf("postgres: ConnMaxLifetime %s must not be negative", o.ConnMaxLifetime)
	}
	if o.ConnMaxIdleTime < 0 {
		return fmt.Errorf("postgres: ConnMaxIdleTime %s must not be negative", o.ConnMaxIdleTime)
	}
	return nil
}

// Open builds the pool opts describes and verifies it with one ping bounded
// by ctx. Verification is part of connection establishment, not a separate
// step: the pool is opened at startup, a "connection" that cannot answer a
// ping means the database this process owns is not behind it, and the caller
// learns that here rather than on the first request that needs a row. On any
// failure the half-built pool is closed before the error is returned.
//
// The error never quotes opts.DSN: the DSN's userinfo carries the password,
// and an open failure is a string an operator will paste into a ticket. Every
// failure this package builds goes through withoutDSN, and the driver's own
// message underneath is pgconn's, which masks passwords on its side.
//
// Open refuses any DSN that is not a postgres:// URL naming the `dataplane`
// database and nothing else: the plane binding enforced by the configuration
// loader holds here too, because the adapter is the last place a foreign DSN
// could pass through (see ownedDatabase and validateDSN).
func Open(ctx context.Context, opts Options) (*sql.DB, error) {
	return open(ctx, driverName, opts)
}

// open is Open against an explicitly named driver — the seam the unit tests
// drive the hand-written fake through, so the orchestration under Open is
// pinned without a live server. Open is the only production caller, and it
// passes driverName.
func open(ctx context.Context, driver string, opts Options) (*sql.DB, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if err := validateDSN(opts.DSN); err != nil {
		return nil, err
	}

	db, err := sql.Open(driver, opts.DSN)
	if err != nil {
		// sql.Open parses the DSN for drivers that support OpenConnector —
		// pgx is one — so a malformed string can fail here rather than at
		// the first use, with the driver's error quoting the string it was
		// handed. The text goes through withoutDSN before it leaves.
		return nil, fmt.Errorf("postgres: open pool: the driver rejected the DSN: %w", withoutDSN(err, opts.DSN))
	}
	db.SetMaxOpenConns(opts.MaxOpenConns)
	db.SetMaxIdleConns(opts.MaxIdleConns)
	db.SetConnMaxLifetime(opts.ConnMaxLifetime)
	db.SetConnMaxIdleTime(opts.ConnMaxIdleTime)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: open pool: ping: %w", withoutDSN(err, opts.DSN))
	}
	return db, nil
}

// validateDSN checks that the DSN names the database this application owns —
// the plane binding of ADR 0006 §7 made mechanical at the last door. It holds
// the DSN to the shape the loader in internal/config accepts — a
// postgres:// or postgresql:// URL whose path is exactly one database —
// because anything else has already been refused as configuration, and a
// shape the two doors disagree on would be a door a foreign DSN walks
// through: a keyword-value string, a pathless URL naming its database in a
// query parameter, or a double-slash path all parse differently at the
// driver than a lenient trim would read them, and the driver's reading is
// the one that dials. The DSN's grammar beyond scheme and path is the
// driver's business, not this one: the driver refuses what it cannot parse,
// and this package refuses what it must not open.
//
// Errors name the database and never the userinfo: the DSN carries the role's
// password, and an error message is a log line waiting to happen.
func validateDSN(dsn string) error {
	parsed, err := url.Parse(dsn)
	if err != nil {
		// url.Parse quotes the string it rejected, credentials included, so
		// its own text is deliberately not wrapped into this error.
		return errors.New("postgres: DSN must be a postgres:// or postgresql:// URL")
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		// A keyword-value DSN parses as a URL with no scheme and the whole
		// string for a path — quoting the "database" it seems to name would
		// quote the password — so the scheme is checked before anything is
		// quoted, and this branch quotes nothing.
		return errors.New("postgres: DSN must be a postgres:// or postgresql:// URL")
	}
	database := strings.TrimPrefix(parsed.Path, "/")
	if database == "" || strings.Contains(database, "/") {
		return errors.New("postgres: DSN must name exactly one database in its path")
	}
	if database != ownedDatabase {
		return fmt.Errorf("postgres: DSN names database %q, but this application owns only the %s database (ADR 0006 §7): it dials its own plane's database and nothing else", database, ownedDatabase)
	}
	return nil
}

// withoutDSN returns err with any appearance of dsn in its text struck out.
// The driver is trusted with the DSN — it cannot connect without it — but
// not with quoting it back at the caller: pgx's parse errors embed the
// connection string, and an error message is a log line waiting to happen.
// When the text carries no DSN the error is returned unchanged, wrapping
// intact, so errors.Is keeps working on the ordinary failure paths.
func withoutDSN(err error, dsn string) error {
	if err == nil || dsn == "" || !strings.Contains(err.Error(), dsn) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), dsn, "[redacted dsn]"))
}

// store is the PostgreSQL implementation of persistence.Store. It is one field,
// because the transaction semantics the port promises are all orchestration
// around the pool, and orchestration does not need state to be correct.
type store struct {
	db *sql.DB
}

// Compile-time proof that the adapter satisfies the port it claims to, and
// that the two handles it resolves between — the pool outside a unit of
// work, the transaction inside one — are the port's query surface.
var (
	_ persistence.Store   = (*store)(nil)
	_ persistence.Querier = (*sql.DB)(nil)
	_ persistence.Querier = (*sql.Tx)(nil)
)

// InUnitOfWork reports whether ctx carries a unit of work this store opened
// or joined — the same lookup Querier and WithinTx make, so the three can
// never disagree about which unit of work a context belongs to. It exists for
// the caller whose contract is unit-of-work-shaped and who must refuse rather
// than silently degrade: the fact append, whose sequence would otherwise be
// allocated on the pool and could commit apart from the fact it numbers.
func (s *store) InUnitOfWork(ctx context.Context) bool {
	_, ok := ctx.Value(txKey{db: s.db}).(*sql.Tx)
	return ok
}

// ValidateSchema reports whether the runtime storage schema this package's
// repositories are written against is present. It is a boot-time guard, not a
// port member: a pool answers a ping all day while the migration that creates
// its tables has not run, and a process that discovers that on its first real
// query turns a one-line deployment mistake into per-request failures. The
// probe asks for the ten objects whose absence breaks the very first
// statement of each repository — the feed's ordering authority, the request
// family, the quota family, the projection foundation's mirror and position,
// the client price list, and the catalog's two tables, whose absence the
// executor registry's snapshot read would otherwise discover per refresh
// after a boot that reported ready — and the error
// names the missing object and nothing else: no SQL, no driver prose, the
// same discipline the repositories' own failures follow.
func ValidateSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		panic("postgres: ValidateSchema requires a non-nil *sql.DB — open the pool before validating")
	}
	const probe = `SELECT to_regclass($1)`
	for _, required := range []struct{ object, why string }{
		{"public.usage_events_stream", "the usage fact feed has no ordering authority"},
		{"public.requests", "the request family is missing"},
		{"public.quota_projections", "the quota projections are missing"},
		{"public.api_key_credentials", "the credential mirror is missing"},
		{"public.account_states", "the account mirror is missing"},
		{"public.projection_state", "the projection position is missing"},
		{"public.client_price_list_revisions", "the client price list has no revisions"},
		{"public.client_price_list_entries", "the client price list has no entries"},
		{"public.backends", "the backend catalog is missing"},
		{"public.model_aliases", "the alias catalog is missing"},
	} {
		var found *string
		if err := db.QueryRowContext(ctx, probe, required.object).Scan(&found); err != nil {
			return fmt.Errorf("postgres: validate schema: %w", err)
		}
		if found == nil {
			return fmt.Errorf("postgres: validate schema: %s is missing from the runtime database (%s) — apply migrations/dataplane before serving", required.object, required.why)
		}
	}
	return nil
}

// txKey is the context key carrying the transaction a WithinTx scope opened,
// keyed by the pool that opened it. Unexported on purpose: only this package
// can put a transaction in a context, so no caller can smuggle one in from
// elsewhere and have it honoured. The pool inside the key is what makes that
// carrying precise — a scope can only ever find a transaction opened on its
// own pool, so a store backed by another pool can neither join nor be joined
// by it. Two stores over one pool share one database and therefore one unit
// of work; the key says which pool a transaction belongs to, and nothing
// else can confuse them.
type txKey struct {
	db *sql.DB
}

// Ping reports whether the pool can produce a working connection within ctx.
func (s *store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("postgres: ping postgres: %w", err)
	}
	return nil
}

// Querier resolves the port's query surface for ctx as persistence.Store
// promises: the transaction in flight on this store's own pool when ctx
// carries one, the pool itself otherwise. The lookup is the same one
// WithinTx joins on, so a repository resolving its query surface can never
// land anywhere but the unit of work its context belongs to.
func (s *store) Querier(ctx context.Context) persistence.Querier {
	if tx, ok := ctx.Value(txKey{db: s.db}).(*sql.Tx); ok {
		return tx
	}
	return s.db
}

// WithinTx honours persistence.Store's contract: commit on nil, roll back on
// error and on panic, join a transaction already in flight on this store's
// own pool.
//
// The rollback lives in a defer and runs on every path out of here — the
// error return and a panic in fn above all, where the transaction is still
// open and this call is the only thing standing between a half-written unit
// of work and the pool handing its connection to someone else. After a
// successful Commit, and after a failed one (where database/sql, since Go
// 1.20, discards the connection as being in an unknown state), the deferred
// call answers ErrTxDone without touching the driver — the no-op Rollback
// the standard library exists to make safe.
func (s *store) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	if _, ok := ctx.Value(txKey{db: s.db}).(*sql.Tx); ok {
		// A transaction is already in flight on this store's own pool: this
		// scope belongs to it, and the owner of that transaction is the one
		// who commits or rolls it back. Opening a second one here would
		// either deadlock or split an atomic unit of work in two, depending
		// on the pool's mood. A transaction opened on a different pool is
		// invisible to this lookup by construction — the key carries the
		// pool — so a store backed by another pool never silently writes
		// into someone else's unit of work: it falls through and begins its
		// own.
		return fn(ctx)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(context.WithValue(ctx, txKey{db: s.db}, tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: commit transaction: %w", err)
	}
	return nil
}
