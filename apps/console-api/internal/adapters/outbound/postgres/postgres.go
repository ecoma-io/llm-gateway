// Package postgres adapts the persistence port to PostgreSQL through the
// standard library's database/sql — the console-api's adapter to the `control`
// database decided in ADR 0006 §7 (Control Plane state, and the authority for
// everything a customer is billed from), operated per deploy/postgres/README.md.
//
// The driver is registered here, once, by the blank import below: pgx's
// stdlib wrapper, the maintained database/sql driver for PostgreSQL. The
// standard library ships no PostgreSQL driver of its own, and lib/pq is in
// maintenance mode; the port's database/sql shape — Querier, BeginTx, the
// driver-level fakes these tests watch — is the abstraction this repository
// decided on, and what that shape needs is a driver the standard library can
// load. The import lives at this package boundary and nowhere else: nothing
// under internal/ ever sees it, and no pgx type crosses this package's API —
// the store New hands out is the port and nothing else.
//
// Open is how the process boundary dials. It validates the pool settings it
// is handed, checks that the DSN names the database this application owns,
// opens the pool on the registered driver, and pings it before returning —
// so a database that will not answer, or a DSN pointed at another plane's
// database, is a startup failure and not the first request's.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	// github.com/jackc/pgx/v5/stdlib is the PostgreSQL driver, registered as
	// "pgx" and imported here and nowhere else. The justification is AGENTS
	// rule 11: the standard library has no PostgreSQL driver, so the port's
	// database/sql shape needs one from outside; lib/pq is in maintenance
	// mode; pgx's stdlib wrapper is the maintained database/sql driver. The
	// blank import is the whole of the dependency — this package's API names
	// only database/sql and the port, never a pgx type.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// ownedDatabase is the only database this application may open — the plane
// binding of ADR 0006 §7. It is a constant and not a setting: which database
// is this plane's is decided by the architecture, and a deployment that wants
// a different answer is misconfigured, not retuned. It mirrors the constant
// of the same name in internal/config, where the environment's DSN is checked
// against it at load time; Open repeats the check because it is the last door
// and its Options can be built by any caller.
const ownedDatabase = "control"

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

// Options configures Open. It is the wiring half of the connection settings:
// internal/config loads them from the environment and the composition root
// maps them here field by field, because this package must not read the
// environment and must not import the configuration package — one value, one
// place it is named, and the mapping visible in the diff.
type Options struct {
	// DSN is the connection string, in the URL form PostgreSQL documents. It
	// names the database this application owns and carries the role's
	// password: no error this package returns quotes it.
	DSN string

	// MaxOpenConns bounds the pool's open connections. At least one: a pool
	// that may not open a connection is not a pool.
	MaxOpenConns int

	// MaxIdleConns bounds the connections the pool keeps when idle. It may
	// not be negative, and it may not exceed MaxOpenConns.
	MaxIdleConns int

	// ConnMaxLifetime bounds the total service of one pooled connection
	// before the pool retires it.
	ConnMaxLifetime time.Duration

	// ConnMaxIdleTime bounds how long an idle connection is kept before the
	// pool closes it.
	ConnMaxIdleTime time.Duration
}

// Open dials the application's own plane's database and returns a validated
// pool: one whose settings are the ones Options asked for, and one that has
// answered a ping within ctx — startup validation, not a lazy maybe. The
// caller owns the pool from here: Close belongs to whoever holds it, and the
// store built over it (New) needs no closing of its own.
//
// A failure is a startup failure and carries no DSN: driver errors are run
// through withoutDSN below, because a driver's parse error quotes the
// connection string it was handed and the string carries the password.
func Open(ctx context.Context, opts Options) (*sql.DB, error) {
	return open(ctx, "pgx", opts)
}

// open is Open with the driver named. The name is a parameter of this one
// function so the hand-written driver in fakedriver_test.go can stand exactly
// where pgx stands in the process; every caller but the tests names the
// constant Open pins.
func open(ctx context.Context, driver string, opts Options) (*sql.DB, error) {
	switch {
	case opts.DSN == "":
		return nil, errors.New("postgres: DSN is required")
	case opts.MaxOpenConns < 1:
		return nil, fmt.Errorf("postgres: MaxOpenConns must be greater than zero, got %d", opts.MaxOpenConns)
	case opts.MaxIdleConns < 0:
		return nil, fmt.Errorf("postgres: MaxIdleConns must not be negative, got %d", opts.MaxIdleConns)
	case opts.MaxIdleConns > opts.MaxOpenConns:
		return nil, fmt.Errorf("postgres: MaxIdleConns (%d) must not exceed MaxOpenConns (%d)", opts.MaxIdleConns, opts.MaxOpenConns)
	case opts.ConnMaxLifetime < 0:
		return nil, fmt.Errorf("postgres: ConnMaxLifetime must not be negative, got %s", opts.ConnMaxLifetime)
	case opts.ConnMaxIdleTime < 0:
		return nil, fmt.Errorf("postgres: ConnMaxIdleTime must not be negative, got %s", opts.ConnMaxIdleTime)
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
		return nil, fmt.Errorf("postgres: open: %w", withoutDSN(err, opts.DSN))
	}

	db.SetMaxOpenConns(opts.MaxOpenConns)
	db.SetMaxIdleConns(opts.MaxIdleConns)
	db.SetConnMaxLifetime(opts.ConnMaxLifetime)
	db.SetConnMaxIdleTime(opts.ConnMaxIdleTime)

	if err := db.PingContext(ctx); err != nil {
		// Ping is the startup validation: a pool that has not answered
		// within ctx is a process that should not have started. Close
		// before reporting, so a failed dial leaves no pool — and no
		// connection it did open — behind for the caller to leak.
		_ = db.Close()
		return nil, fmt.Errorf("postgres: connect: %w", withoutDSN(err, opts.DSN))
	}
	return db, nil
}

// validateDSN checks that the DSN names the database this application owns —
// the plane binding of ADR 0006 §7 made mechanical at the last door. The same
// check runs at load time in internal/config against the environment's
// variable; it is repeated here with this package's vocabulary because Open's
// Options have callers other than the composition root's mapping, and Open is
// where a wrong database would become a live pool. The check holds the DSN to
// the shape the loader accepts — a postgres:// URL whose path is exactly one
// database — because a shape the two doors disagree on would be a door a
// foreign DSN walks through: a keyword-value string parses as a URL with the
// whole string for a path, and the driver's reading of any such shape is the
// one that dials. The DSN's grammar beyond scheme and path is the driver's
// business, not this one: the driver refuses what it cannot parse, and this
// package refuses what it must not open.
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

// InUnitOfWork reports whether ctx carries a unit of work this store opened
// or joined — the same lookup Querier and WithinTx make, so the three can
// never disagree about which unit of work a context belongs to. The
// projection change recorder is the caller whose contract is
// unit-of-work-shaped and who must refuse rather than silently degrade: the
// fact append's sequence-allocation argument, applied to a counter whose
// gaplessness and whose log/mirror atomicity are the projection's
// correctness.
func (s *store) InUnitOfWork(ctx context.Context) bool {
	_, ok := ctx.Value(txKey{db: s.db}).(*sql.Tx)
	return ok
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
