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
// This port belongs to the Control Plane. The runtime has its own copy of it
// in its own module, because the two planes own disjoint databases (ADR 0006
// §7): separate namespaces, separate connection targets, independent
// transactions and independent migration history, so the ordinary query that
// would join this plane's rows to the other's is not a statement PostgreSQL
// will parse. Each copy is wired to its own plane's database, which keeps the
// crossing out of the code rather than merely forbidding it in review — and
// that is the whole of it. It is an ownership boundary and not a credential
// one: it does not stop a privileged role, a foreign data wrapper or a
// dblink-style path from reaching both databases, and the credential boundary
// is the authenticated management surface of ADR 0006 §9.
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
	"errors"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
)

// ErrNotFound is the repositories' miss sentinel: a lookup whose subject does
// not exist. It means a miss and only a miss — the application layer decides
// which of its own errors a miss becomes, the way application.Error pairs with
// the cache port's sentinel today. Distinguishable outcomes travel as
// sentinels so callers branch with errors.Is; everything else arrives wrapped
// with context.
var ErrNotFound = errors.New("persistence: not found")

// ErrConflict is a unit of work the database itself aborted — a serialization
// failure (SQLSTATE 40001) or a deadlock victim (40P01). It is retryable by
// definition: nothing about the caller's intent was refused, the store only
// pitted two units of work against each other and chose a loser. The
// adapter translates both states to this one sentinel and nothing above it
// sees driver strings; the caller that owns the unit of work re-runs it from
// its first statement, re-reading whatever it read before. It is not the
// guard-miss vocabulary — a guarded statement that fires zero rows is that
// statement's domain verdict, not a conflict.
var ErrConflict = errors.New("persistence: unit of work aborted by a concurrent write, retry")

// ErrWindowClaimed is a reconciliation pass whose window another RUNNING pass
// is already covering. It is a sentinel rather than a failure because the
// right response is to do nothing and wait for the next tick: the other pass is
// doing the work this one would have done, and there is nothing to retry and
// nothing to report.
//
// It exists because a claim is the one place where "the world moved" is the
// expected answer rather than a defect, and because the alternative — letting
// the unique violation arrive as a driver string — puts a distinction the
// caller must act on into the hands of a string it would have to pattern
// match. Two replicas of the control plane compute the same high-water mark
// and therefore the same window; the engine arbitrates which of them sweeps
// it, and the other is told so in a word.
var ErrWindowClaimed = errors.New("persistence: reconciliation window is already claimed by a running pass")

// Pinger reports whether the backing store is answering right now. Readiness
// is what it is for: /readyz gates on this, over the port itself rather than
// through the application — there is no use case for a ping — and nothing else
// should treat a successful ping as evidence that a particular query will
// succeed.
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

// Store is everything the application layer may ask of the Control Plane
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
	//
	// Every unit of work runs at the driver's default isolation, READ
	// COMMITTED, and the port's concurrency model is built on that rather
	// than on a stronger level: a guarded statement is a single statement —
	// its WHERE clause is the guard and its rows-affected count the verdict
	// — so no read-modify-write sequence spans statements for a second
	// writer to slip into. A caller that needs a stricter level for one
	// unit raises it itself as the unit's first statement
	// (`SELECT set_config(...)`) and owns the retry policy that level
	// demands; the port neither hides nor automates that choice.
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error

	// InUnitOfWork reports whether ctx carries a unit of work this store
	// opened or joined — the same lookup Querier and WithinTx make, so the
	// three can never disagree about which unit of work a context belongs
	// to. It exists for the port member whose contract is unit-of-work-
	// shaped and who must refuse rather than silently degrade: the
	// projection change recorder, whose revision allocation, log append and
	// mirror write would each autocommit on the pool if the caller's
	// context lost its transaction, breaking the gapless counter and the
	// log/mirror atomicity the projection's correctness rests on.
	InUnitOfWork(ctx context.Context) bool
}

// The console's read surface: the query shapes ADR 0012 §1 and §2 settle for
// the ten Control-Plane read operations, in the port's own vocabulary.
//
// Three rules every member below carries, and none of them is a style choice:
//
//   - The account predicate is in the WHERE clause, and it is the FIRST
//     argument. A resource the session's account does not own is a row the
//     query never returned, which is the same answer — zero rows, at the
//     same cost — a genuinely absent row gives. That equivalence is the whole
//     reason the predicate is a clause and not a filter applied afterwards: a
//     post-fetch check answers in a different time for "not yours" than for
//     "not there", and a 403 turns the difference into a confirmation oracle.
//     There is deliberately no forbidden sentinel on this page of the port.
//   - Keyset, never OFFSET. The reason is accounting.Sweep's own comment, and
//     it is a correctness one: OFFSET re-reads and re-discards every row
//     already passed, and a pass that pages by OFFSET while rows land
//     concurrently can both skip a row and read one twice. Every keyset
//     below is a UNIQUE sort key, so the cursor is total and a page can
//     neither drop nor repeat a row.
//   - No total. A page is a page, and the caller learns what follows it from
//     fetching limit+1. There is no count member on a list, and a count
//     added to one later would be a number that is wrong the instant it is
//     written.
//
// The cursor a caller holds is opaque to it and to this package: the
// application layer mints and places it, and only the sort key crosses this
// boundary. A filter fingerprint rides beside it there, which is why no
// member below takes a fingerprint — the fingerprint is compared against the
// request's own filters before any of these is called, and a mismatch never
// reaches a query.

// Page bounds. The wire contract names them, and the adapter is the last place
// that can refuse a value outside them, so they live here rather than being
// restated at each call site: a list that clamped would be answering a
// question the contract says it does not answer.
const (
	// MinPageLimit and MaxPageLimit are the contract's own bounds, and
	// DefaultPageLimit its own default. A caller that asked for a larger page
	// than MaxPageLimit asked a question this surface does not answer, and
	// the smaller page it would otherwise get is one it cannot tell apart
	// from a page the collection capped itself.
	MinPageLimit     = 1
	MaxPageLimit     = 200
	DefaultPageLimit = 50
)

// UserPage is the users list's own request: the lifecycle state to restrict
// to, the keyset position, and the page size.
//
// State is empty for "every live user" — invited and active together, which
// is what a member list is — and a removed row is reachable by naming it,
// because an operator auditing a retired address needs to see it.
type UserPage struct {
	// State is one lifecycle state, or empty for every live user. It is a
	// string rather than identity.UserState so that "no filter" is
	// representable without a zero-value constant, which the domain's
	// vocabulary deliberately has none of.
	State string

	// After is the exclusive lower bound on the keyset. The zero value is the
	// beginning of the list, not a cursor that matches nothing.
	After identity.UserID

	// Limit is the number of rows wanted; the adapter asks for one more.
	Limit int
}

// APIKeyPage is the api-keys list's request: a keyset position and a page
// size. The list is unfiltered by design — an account's keys are a small,
// bounded set an operator reads whole, and a state filter here would be a
// filter the account predicate has nothing to do with.
type APIKeyPage struct {
	After identity.APIKeyID
	Limit int
}

// AccountUsers is the console's account-scoped read of the members list.
//
// It is not a member on Users, and the reason is mechanical rather than
// architectural: the identity port file is being extended for the session
// surface in parallel with this one, and two agents editing one interface is
// a merge a reviewer has to unpick. The query is the same query Users would
// carry — the same table, the same columns, the same keyset — and a reader
// looking for what a user list can do reads both files.
//
// CountLiveForAccount is the one count on the console's read surface, and it
// is there because the dashboard's projection names it (user_count) and the
// contract states the reason: an operator reading "12 users" should not have
// to page eleven screens to learn there are twelve. It counts a filtered,
// bounded set — invited plus active, excluding removed — and it is not a
// page total: no list below returns one, and a count that is not asked for is
// not computed.
type AccountUsers interface {
	// ListForAccount returns at most page.Limit of the account's users, in
	// id order after page.After, and the account predicate in the query's
	// WHERE clause. The id is the keyset and it is unique, so a page can
	// neither skip a user nor read one twice; ids are uuid v7, so the order
	// is very nearly mint order.
	ListForAccount(ctx context.Context, accountID identity.AccountID, page UserPage) ([]identity.User, error)

	// CountLiveForAccount returns how many of the account's users are
	// invited or active. Removed rows are excluded and the answer is the
	// dashboard's figure, never a page's.
	CountLiveForAccount(ctx context.Context, accountID identity.AccountID) (int, error)
}

// AccountAPIKeys is the console's account-scoped read of the keys list, for
// the same reason AccountUsers stands beside Users.
//
// Every key is an ownership record: no plaintext, no digest, and nothing
// recoverable from a row. The credential a key was minted with exists once,
// in the mint's response, and no member here can return it.
type AccountAPIKeys interface {
	// ListForAccount returns at most page.Limit of the account's keys, oldest
	// first, keyed on id. The account predicate is in the WHERE clause, so
	// another account's key is a row the query never returned.
	ListForAccount(ctx context.Context, accountID identity.AccountID, page APIKeyPage) ([]identity.APIKey, error)

	// CountActiveForAccount returns how many of the account's keys are
	// currently active. Revoked keys are counted in the key list and never
	// here: a dashboard that said "12 keys" with twelve revoked among them
	// would be describing history as if it were capacity.
	CountActiveForAccount(ctx context.Context, accountID identity.AccountID) (int, error)
}
