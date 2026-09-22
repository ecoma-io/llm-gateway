// Package storage is the persistence port of the Ecoma LLM Gateway: the seam
// the application layer depends on, and the only thing it is allowed to know
// about the database.
//
// The port is spelled in the standard library's own terms and nothing else —
// no driver, no ORM, no vendor type crosses this boundary — so a package that
// imports this one cannot reach the database client by accident. The
// PostgreSQL adapter lives in internal/storage/postgres; which driver that
// adapter finally opens is decided where the wiring is, not here, and never
// by the code that asks for a Store.
//
// The port is deliberately small. It carries the two capabilities the
// service has a consumer for today — a reachability check, and a
// transaction-scoped unit of work — and it grows one method per real caller.
// A repository interface invented before the table that backs it is a shape
// guessed at twice.
package storage

import "context"

// Pinger reports whether the backing store is answering right now. Readiness
// is what it is for: /readyz gates on it, and nothing else should treat a
// successful ping as evidence that a particular query will succeed.
type Pinger interface {
	// Ping verifies that a connection to the store can be made — or is
	// already pooled — within ctx.
	Ping(ctx context.Context) error
}

// Store is everything the application layer may ask of its database.
type Store interface {
	Pinger

	// WithinTx runs fn as a single atomic unit of work: the transaction is
	// committed when fn returns nil and rolled back when fn returns an error
	// or panics, so a caller that returns early cannot leave a half-written
	// unit behind.
	//
	// The transaction travels in the context handed to fn. Any query a
	// future repository method runs with that context — rather than the one
	// the call arrived on — participates in the same transaction; that is
	// what makes the scope composable rather than a flag threaded through
	// every signature. The rule that follows from it, and which every
	// repository method obeys: resolve the transaction from the context,
	// never from the pool. A query sent to the pool while a unit of work
	// holds a connection both escapes that unit's commit and rollback and,
	// on a pool with one connection, waits on the transaction's own
	// connection forever.
	//
	// Nesting a WithinTx inside another one joins the transaction already in
	// flight instead of opening a second: an inner scope that returns an
	// error marks the work failed for the caller that owns the transaction,
	// it does not roll back on its own.
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}
