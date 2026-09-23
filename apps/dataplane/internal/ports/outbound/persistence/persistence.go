// Package persistence is this application's outbound port for durable state:
// the seam the application layer depends on, and the only thing it is allowed
// to know about the database.
//
// The port is spelled in the standard library's own terms and nothing else —
// no driver, no ORM, no vendor type crosses this boundary. The one vocabulary
// queries share is database/sql's result and row types: a package that
// imports this one can run a query through a Querier, and can never reach
// the pool, the transaction, or the driver behind it. The PostgreSQL adapter
// lives in internal/adapters/outbound/postgres; which driver that adapter
// finally opens is decided where the wiring is, not here, and never by the
// code that asks for a Store.
//
// This port belongs to the Data Plane, and reaches the `dataplane` database:
// the runtime's own state, which the Control Plane reads only as facts over
// the management chain, never by connecting to it (ADR 0006 §5). The
// Control Plane has its own copy of this port in its own module, because the
// two planes own disjoint databases (ADR 0006 §7): separate namespaces,
// separate connection targets, independent transactions and independent
// migration history, so the ordinary query that would join this plane's rows
// to the other's is not a statement PostgreSQL will parse. Each copy is wired
// to its own plane's database, which keeps the crossing out of the code rather
// than merely forbidding it in review — and that is the whole of it. It is an
// ownership boundary and not a credential one: it does not stop a privileged
// role, a foreign data wrapper or a dblink-style path from reaching both
// databases, and the credential boundary is the authenticated management
// surface of ADR 0006 §9.
//
// The port is deliberately small. It carries what the service has a consumer
// for today — a reachability check, a transaction-scoped unit of work, and
// the query surface that unit of work hands to the repositories built on it
// — and it grows one member per real caller. A repository interface invented
// before the table that backs it is a shape guessed at twice.
package persistence

import (
	"context"
	"database/sql"
)

// Pinger reports whether the backing store is answering right now. Readiness
// is what it is for: /readyz gates on it, and nothing else should treat a
// successful ping as evidence that a particular query will succeed.
type Pinger interface {
	// Ping verifies that a connection to the store can be made — or is
	// already pooled — within ctx.
	Ping(ctx context.Context) error
}

// Querier is the query surface this port grants for running SQL, and the only
// one a repository is given: three methods, no begin, no commit, no
// pool to reach for. Which handle answers behind the interface is the
// port's own decision — inside a unit of work the Querier IS that
// unit's transaction, outside one it is the pool itself — so a caller that
// holds only a Querier cannot send a query around the transaction it was
// meant to join. That is the port's oldest query rule — resolve the
// transaction from the context, never from the pool — made mechanical: the
// resolution happens once, where the context is read, instead of at every
// call site where a repository might otherwise pick a handle itself.
type Querier interface {
	// ExecContext executes a statement that does not return rows.
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)

	// QueryContext executes a query that returns rows.
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)

	// QueryRowContext executes a query expected to return at most one row.
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store is everything the application layer may ask of the Data Plane
// database.
type Store interface {
	Pinger

	// Querier resolves the query surface for ctx: the open unit of work's
	// transaction when ctx carries one this store opened or joined, and the
	// pool itself otherwise. A repository calls it with the context it was
	// handed and runs every query through the result; it never names a
	// handle of its own.
	//
	// The resolution is scoped to this store. A context carrying another
	// pool's transaction does not surrender that transaction here — the
	// caller gets its own pool, and with it the guarantee that its queries
	// land outside a unit of work they were never meant to join. WithinTx's
	// join-or-begin decision is this same lookup, so the two can never
	// disagree about which unit of work a context belongs to.
	Querier(ctx context.Context) Querier

	// WithinTx runs fn as a single atomic unit of work: the transaction is
	// committed when fn returns nil and rolled back when fn returns an error
	// or panics, so a caller that returns early cannot leave a half-written
	// unit behind.
	//
	// The transaction travels in the context handed to fn, and every query
	// a repository runs through Querier with that context — rather than the
	// one the call arrived on — participates in it; that is what makes the
	// scope composable rather than a flag threaded through every signature.
	// The rule that follows, and which the Querier resolution enforces
	// rather than merely asks for: the transaction comes from the context,
	// never from the pool. A query sent to the pool while a unit of work
	// holds a connection both escapes that unit's commit and rollback and,
	// on a pool with one connection, waits on the transaction's own
	// connection forever.
	//
	// The scope is the store's. A nested WithinTx on this store — or on any
	// store backed by the same pool — joins the transaction already in
	// flight instead of opening a second: an inner scope that returns an
	// error marks the work failed for the caller that owns the transaction,
	// it does not roll back on its own. A store backed by a different pool
	// never joins one of this store's units of work: its nested scope
	// opens, owns, and commits a transaction of its own, because a
	// transaction on one pool can no more carry another pool's writes than
	// a query can reach across pools.
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}
