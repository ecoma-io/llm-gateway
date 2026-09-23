// Package postgres adapts the persistence port to PostgreSQL through the
// standard library's database/sql — the runtime's adapter to the `dataplane`
// database decided in ADR 0006 §7 (runtime state: the model catalog, routing
// and provider configuration, the quota projection, and the usage history the
// Control Plane reconciles from), operated per deploy/postgres/README.md.
//
// The adapter is handed an open *sql.DB rather than opening one itself: which
// driver backs that handle is a wiring decision, and the import that
// registers it is a dependency this module deliberately does not carry. The
// change that first wires a real database into cmd/dataplane adds that
// import exactly once, at the process boundary, and nothing under internal/
// ever sees it.
package postgres

import (
	"context"
	"database/sql"
	"fmt"

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
