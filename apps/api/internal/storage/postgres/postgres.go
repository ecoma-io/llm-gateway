// Package postgres adapts the storage port to PostgreSQL through the standard
// library's database/sql — the one adapter surface the gateway will have for
// the authoritative store decided in ADR 0005 (relational state and
// Timescale-oriented event history) and operated per deploy/postgres/README.md.
//
// The adapter is handed an open *sql.DB rather than opening one itself: which
// driver backs that handle is a wiring decision, and the import that
// registers it is a dependency this module deliberately does not carry. The
// change that first wires a real database into cmd/gateway adds that import
// exactly once, at the process boundary, and nothing under internal/ ever
// sees it.
package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ecoma-io/llm-gateway/apps/api/internal/storage"
)

// New returns the storage port backed by db. The store holds no state of its
// own beyond the pool, so it needs no closing: Close belongs to whoever owns
// the *sql.DB.
//
// It panics on a nil pool rather than storing one, because the failure a nil
// pool produces later — a nil dereference inside Ping, far from the code that
// built the store — is strictly worse than a loud one at the construction
// site.
func New(db *sql.DB) storage.Store {
	if db == nil {
		panic("postgres: New requires a non-nil *sql.DB — open the database before building the store")
	}
	return &store{db: db}
}

// store is the PostgreSQL implementation of storage.Store. It is one field,
// because the transaction semantics the port promises are all orchestration
// around the pool, and orchestration does not need state to be correct.
type store struct {
	db *sql.DB
}

// Compile-time proof that the adapter satisfies the port it claims to, and
// that the two handles it resolves between — the pool outside a unit of
// work, the transaction inside one — are the port's query surface.
var (
	_ storage.Store   = (*store)(nil)
	_ storage.Querier = (*sql.DB)(nil)
	_ storage.Querier = (*sql.Tx)(nil)
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
		return fmt.Errorf("storage: ping postgres: %w", err)
	}
	return nil
}

// Querier resolves the port's query surface for ctx as storage.Store
// promises: the transaction in flight on this store's own pool when ctx
// carries one, the pool itself otherwise. The lookup is the same one
// WithinTx joins on, so a repository resolving its query surface can never
// land anywhere but the unit of work its context belongs to.
func (s *store) Querier(ctx context.Context) storage.Querier {
	if tx, ok := ctx.Value(txKey{db: s.db}).(*sql.Tx); ok {
		return tx
	}
	return s.db
}

// WithinTx honours storage.Store's contract: commit on nil, roll back on
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
		return fmt.Errorf("storage: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(context.WithValue(ctx, txKey{db: s.db}, tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit transaction: %w", err)
	}
	return nil
}
