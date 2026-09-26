package persistence

import (
	"context"
	"errors"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
)

// ErrDuplicateIntake says the replay record's (account, idempotency key)
// already exists — the engine half of replay idempotency, and the final guard
// behind whatever check admission ran first. A caller that treats this as a
// failure of the replay is reading it backwards: the row existing is the
// original request's record doing its job, and the caller's next act is to
// read it, not to retry the insert.
var ErrDuplicateIntake = errors.New("persistence: replay record already exists")

// ErrAttemptNotOfRequest says the attempt a finalisation names was made for a
// different request — the composite foreign key's refusal, surfaced as the
// domain sentinel it exists to produce.
var ErrAttemptNotOfRequest = errors.New("persistence: attempt belongs to another request")

// ErrAttemptAlreadyAppended says the attempt row Insert names is already on
// disk — the unique key on (id) refused a second copy. Like
// ErrDuplicateIntake, this is information, not a failure to retry: the append
// raced a writer that persisted the same row, which without this sentinel is
// reachable without any bug — an append whose commit acknowledgement was lost
// leaves its caller unable to tell "never happened" from "happened and
// unacknowledged", and the retry it then runs is the collision. A caller that
// receives this sentinel reads the append as done.
//
// The sentinel names the (id) collision only. The composite unique key on
// (request, candidate position, retry sequence) guards a different fact —
// that one request has at most one attempt per position — and its refusal is
// a domain violation a caller bug made, not a raced append: a second attempt
// at the same position carries a different id by construction. An adapter
// that reported both keys through this one sentinel would tell a caller its
// append was done when what happened was that its attempt was illegitimate,
// so the adapter refuses the composite case as itself.
var ErrAttemptAlreadyAppended = errors.New("persistence: attempt is already appended")

// RequestRepository persists and finalises requests.
//
// Every method resolves its query surface from ctx through the Store that
// backs the implementation (Querier), so a call made inside a WithinTx unit
// of work joins that unit and one made outside it runs alone. There is no
// transaction parameter to get wrong and no handle for a caller to hold.
//
// The terminal columns are written only by Finalise, and only from executing:
// the UPDATE carries the status precondition in its WHERE clause, and the
// boolean it returns is the whole verdict — false means another writer
// finalised the request first, which is information about who won, not an
// error to retry.
type RequestRepository interface {
	// Insert writes the request's row: executing and fresh, or terminal from
	// birth for the rejection path. The two are the only shapes the store
	// accepts — a row in any other status did not come from an admission
	// decision, and an error names it. The domain has already refused a
	// malformed aggregate; a failure here is the store's, and it is returned
	// wrapped, never with SQL internals of its own.
	Insert(ctx context.Context, request execution.Request) error

	// Finalise writes the request's terminal shape from its current status:
	// status, the one reason column the status owns, the committed attempt the
	// status names, and the finish time. A call whose request is still in
	// executing status is refused — finalising to executing is not a
	// transition, and an adapter that wrote the row back unchanged would
	// only be hiding the caller's bug. It returns false — with no error —
	// when the row is no longer executing, because the losing writer has
	// nothing left to do but read the winner's decision.
	Finalise(ctx context.Context, request execution.Request) (bool, error)
}

// AttemptRepository appends finished upstream calls and writes the one
// sanctioned update.
type AttemptRepository interface {
	// Insert writes one finished call's row — appended as the call finishes,
	// never while it is in flight (ADR 0001 rule 4), so a crash mid-call
	// leaves no row and no transaction is ever open across a provider call.
	// A collision with an already-persisted copy of the same attempt fails
	// with ErrAttemptAlreadyAppended: the row exists, the append already
	// happened, and the caller reads that as success rather than retrying an
	// append only the engine's refusal can end.
	//
	// The sentinel's tolerance is an outside-a-unit shape. Inside a unit of
	// work the engine has already aborted the transaction by the time the
	// refusal surfaces — PostgreSQL poisons the unit after any unique-key
	// failure — so a caller that swallowed the sentinel and kept writing
	// would watch every later statement die with the aborted-transaction
	// class instead. A caller that must survive the collision inside its own
	// unit probes with Exists first, inserts only on a miss, and lets a
	// residual collision abort the unit for its retry to take the probe's
	// path.
	Insert(ctx context.Context, attempt execution.Attempt) error

	// Exists reports whether the attempt's row is already on disk. It is the
	// probe the unit-of-work insert cannot be preceded by a tolerated
	// collision: an append that races a writer which persisted the same row
	// — the lost-commit-ack retry, or the orphan tail of a lost ending race —
	// is read as the done thing it is before any insert runs, so the unit
	// never poisons itself on a refusal that means "already done".
	//
	// It answers presence and nothing else: the row's content is not
	// compared, and a present row written by a racer is the same observation
	// this port's Insert tolerance reads as done.
	Exists(ctx context.Context, attemptID identity.AttemptID) (bool, error)

	// RecordProviderUsage is the one sanctioned update: the provider-usage
	// telemetry columns alone, for usage that arrived after the row existed.
	// Identity, outcome, class and timing are never written by it. It returns
	// false when the attempt does not exist — an update to a row that was
	// never inserted is a caller bug this port declines to paper over.
	RecordProviderUsage(ctx context.Context, attemptID identity.AttemptID, input, output, delivery *int64) (bool, error)
}

// IntakeRepository persists the relational replay record.
type IntakeRepository interface {
	// Insert writes the record admission decided on. A duplicate
	// (account, idempotency key) fails with ErrDuplicateIntake: the database's
	// unique key is the final idempotency guard, behind whatever check the
	// caller ran, and the collision is the replay path's cue to read the
	// original row rather than an error to swallow.
	Insert(ctx context.Context, intake execution.Intake) error

	// Find reads the record for one replay key. ErrNotFound means this
	// (account, key) has never been admitted — the caller is not looking at a
	// replay at all.
	Find(ctx context.Context, accountID, idempotencyKey string) (execution.Intake, error)

	// Finalise writes the terminal pointer, once, and only while it is unset:
	// the UPDATE carries `final_status IS NULL` in its WHERE clause, and false
	// means the pointer was already written. "Immutable afterwards" is scoped
	// to the identity fields; this is the one mutation the record allows, and
	// it happens once.
	Finalise(ctx context.Context, accountID, idempotencyKey string, status execution.FinalStatus, rejection execution.RejectionReason, failure execution.FailureReason) (bool, error)
}
