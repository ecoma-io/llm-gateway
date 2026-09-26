package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

// requestInsert columns are the row in table order. A rejection row arrives
// terminal-from-birth with most columns unset, and the nil-valued arguments
// carry that: NULL is what "the fields known so far" looks like in a row.
const requestInsert = `INSERT INTO public.requests
    (id, account_id, api_key_id, alias, input_tokens, max_output_tokens,
     price_revision_id, input_unit_price, output_unit_price,
     status, rejection_reason, failure_reason, committed_attempt_id,
     admitted_at, finished_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`

// requestFinalise is the once-only close. The status precondition in the WHERE
// clause is the entire concurrency story: two writers finalising one request
// run this statement against the same row, the row lock serialises them, and
// the loser's predicate no longer matches — zero rows, no error, no second
// finalisation. The engine's trigger behind it is the bug detector for any
// path that reaches the row some other way.
const requestFinalise = `UPDATE public.requests
SET status = $2, rejection_reason = $3, failure_reason = $4,
    committed_attempt_id = $5, finished_at = $6
WHERE id = $1 AND status = 'executing'`

// RequestRepository is the PostgreSQL implementation of the request half of
// the persistence port. It holds the Store and resolves its query surface per
// call, so a call inside a unit of work joins it and one outside runs alone.
type RequestRepository struct {
	store persistence.Store
}

// NewRequestRepository builds the repository over a store.
func NewRequestRepository(store persistence.Store) *RequestRepository {
	return &RequestRepository{store: store}
}

// Insert implements persistence.RequestRepository. The two shapes the store
// accepts are the two admission decisions: executing and fresh, or rejected
// and terminal from birth. A row in any other status did not come from an
// admission path — succeeded and failed are transitions Finalise writes, and
// inserting one directly would be a finalisation that skipped the once-only
// close — so it is refused here rather than left for a shape constraint to
// catch after the fact.
func (repository *RequestRepository) Insert(ctx context.Context, request execution.Request) error {
	switch request.Status {
	case execution.StatusExecuting, execution.StatusRejected:
	default:
		return fmt.Errorf("postgres: insert request: %q is not a status a row is born with", string(request.Status))
	}
	// The price columns ride the snapshot's presence, not its arithmetic: the
	// schema's requests_price_snapshot_pairing prices each unit column NULL
	// exactly when the revision is NULL, so a row written "with the fields
	// known so far" — a rejection, which carries no snapshot — must carry no
	// prices either. An admitted request always has a revision (NewRequest
	// refuses its absence), so the free-model zero price is still written as
	// a real zero.
	var inputUnitPrice, outputUnitPrice any
	if request.Price.RevisionID != "" {
		inputUnitPrice = int64Value(request.Price.InputUnitPrice)
		outputUnitPrice = int64Value(request.Price.OutputUnitPrice)
	}
	_, err := repository.store.Querier(ctx).ExecContext(ctx, requestInsert,
		string(request.ID),
		request.AccountID,
		request.APIKeyID,
		textOrNil(request.Alias),
		intOrNil(request.InputTokens),
		intOrNil(request.MaxOutputTokens),
		textOrNil(request.Price.RevisionID),
		inputUnitPrice,
		outputUnitPrice,
		string(request.Status),
		textOrNil(string(request.RejectionReason)),
		textOrNil(string(request.FailureReason)),
		textOrNil(string(request.CommittedAttemptID)),
		request.AdmittedAt,
		timeOrNil(request.FinishedAt),
	)
	if err != nil {
		return fmt.Errorf("postgres: insert request: %w", err)
	}
	return nil
}

// Finalise implements persistence.RequestRepository. A call whose request is
// still in executing status is refused before the statement runs — finalising
// to executing is not a transition, and letting the statement through would
// rewrite the row to exactly what it already was and report that as a win.
// False comes back with no error when the row is no longer executing: the
// writer that lost the race has nothing to do but read the winner's decision,
// and an error would tell it to retry a decision that can only lose again.
func (repository *RequestRepository) Finalise(ctx context.Context, request execution.Request) (bool, error) {
	if request.Status == execution.StatusExecuting {
		return false, errors.New("postgres: finalise request: executing is not a status to finalise to")
	}
	result, err := repository.store.Querier(ctx).ExecContext(ctx, requestFinalise,
		string(request.ID),
		string(request.Status),
		textOrNil(string(request.RejectionReason)),
		textOrNil(string(request.FailureReason)),
		textOrNil(string(request.CommittedAttemptID)),
		timeOrNil(request.FinishedAt),
	)
	if err != nil {
		if code(err) == "23503" && constraint(err) == "requests_committed_attempt_fkey" {
			// The finalisation named an attempt of another request: the
			// composite foreign key's refusal, surfaced as the domain sentinel
			// that exists for exactly this.
			return false, fmt.Errorf("postgres: finalise request: %w", persistence.ErrAttemptNotOfRequest)
		}
		return false, fmt.Errorf("postgres: finalise request: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: finalise request: %w", err)
	}
	return affected > 0, nil
}

// attemptInsert is the row appended when one upstream call finishes — never
// while it is in flight, so the insert rides no transaction across a provider
// call and a crash mid-call leaves no row at all.
const attemptInsert = `INSERT INTO public.request_attempts
    (id, request_id, candidate_position, retry_sequence,
     backend_id, provider_model, provider_request_id, outcome, error_class,
     provider_input_tokens, provider_output_tokens, delivery_tokens,
     provider_error, started_at, finished_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`

// attemptUsageUpdate is the ONE sanctioned update to an attempt row, and
// COALESCE is its whole discipline: the first figure a row carries stands —
// the column is the store side of keep-first, matching the domain's own
// merge — and a later report writes only into the columns still NULL.
// Overwriting observed telemetry, with a number or with a NULL, would be a
// settlement decision made by an UPDATE; the second report simply never
// displaces the first.
const attemptUsageUpdate = `UPDATE public.request_attempts
SET provider_input_tokens = COALESCE(provider_input_tokens, $2),
    provider_output_tokens = COALESCE(provider_output_tokens, $3),
    delivery_tokens = COALESCE(delivery_tokens, $4)
WHERE id = $1`

// AttemptRepository is the PostgreSQL implementation of the attempt half of
// the persistence port.
type AttemptRepository struct {
	store persistence.Store
}

// NewAttemptRepository builds the repository over a store.
func NewAttemptRepository(store persistence.Store) *AttemptRepository {
	return &AttemptRepository{store: store}
}

// Insert implements persistence.AttemptRepository. A 23505 on the row's own
// id — request_attempts_pkey — is not a store failure but the answer that the
// attempt is already appended: an append whose commit acknowledgement was lost
// looks exactly like a lost append to the caller that retries it, and the
// engine's refusal is the only writer that knows the difference. The
// constraint name is matched the way every classification in this adapter is —
// never the message text.
//
// The composite key's refusal is deliberately not classified. The business
// unique key on (request, candidate position, retry sequence) guards one
// attempt per position, and a second attempt at the same position carries a
// different id by construction — so its 23505 is a caller bug (the walk made
// two attempts at one position), not a raced append, and reporting it through
// the already-appended sentinel would tell the caller its row was written when
// what happened was that its attempt was illegitimate. It surfaces as the
// engine's own error, loud, at the caller that made it.
func (repository *AttemptRepository) Insert(ctx context.Context, attempt execution.Attempt) error {
	var providerError any
	if len(attempt.ProviderError) > 0 {
		providerError = []byte(attempt.ProviderError)
	}
	_, err := repository.store.Querier(ctx).ExecContext(ctx, attemptInsert,
		string(attempt.ID),
		string(attempt.RequestID),
		attempt.CandidatePosition,
		attempt.RetrySequence,
		attempt.BackendID,
		attempt.ProviderModel,
		textOrNil(attempt.ProviderRequestID),
		string(attempt.Outcome),
		textOrNil(string(attempt.ErrorClass)),
		attempt.ProviderInputTokens,
		attempt.ProviderOutputTokens,
		attempt.DeliveryTokens,
		providerError,
		attempt.StartedAt,
		attempt.FinishedAt,
	)
	if err != nil {
		if code(err) == "23505" && constraint(err) == "request_attempts_pkey" {
			return fmt.Errorf("postgres: insert attempt: %w", persistence.ErrAttemptAlreadyAppended)
		}
		return fmt.Errorf("postgres: insert attempt: %w", err)
	}
	return nil
}

// attemptExists is the probe the unit-of-work insert is preceded by: a row's
// presence read before any insert runs, so a raced append is recognised while
// the unit can still act on the answer instead of poisoning itself on the
// engine's abort.
const attemptExists = `SELECT EXISTS (
    SELECT 1 FROM public.request_attempts WHERE id = $1
)`

// Exists implements persistence.AttemptRepository.
func (repository *AttemptRepository) Exists(ctx context.Context, attemptID identity.AttemptID) (bool, error) {
	var exists bool
	if err := repository.store.Querier(ctx).QueryRowContext(ctx, attemptExists, string(attemptID)).Scan(&exists); err != nil {
		return false, fmt.Errorf("postgres: exists attempt: %w", err)
	}
	return exists, nil
}

// RecordProviderUsage implements persistence.AttemptRepository.
func (repository *AttemptRepository) RecordProviderUsage(ctx context.Context, attemptID identity.AttemptID, input, output, delivery *int64) (bool, error) {
	result, err := repository.store.Querier(ctx).ExecContext(ctx, attemptUsageUpdate,
		string(attemptID), input, output, delivery,
	)
	if err != nil {
		return false, fmt.Errorf("postgres: record provider usage: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: record provider usage: %w", err)
	}
	return affected > 0, nil
}

// intakeInsert writes the replay record admission decided on. The unique key
// on (account_id, idempotency_key) is the final idempotency guard: whatever
// admission checked first, the engine has the last word, and its 23505 on
// this key is the replay path's cue to read the original row.
const intakeInsert = `INSERT INTO public.request_intake
    (account_id, idempotency_key, request_digest, request_id,
     final_status, final_rejection_reason, final_failure_reason, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

// intakeFind reads one replay record whole. request_id is an ID reference, not
// a foreign key (enforced keys stop at the family boundary, ADR 0005), and the
// scan takes it as text for exactly the reason the column is spelled that way
// in the domain: it is an identity, parsed where identities are parsed.
const intakeFind = `SELECT account_id, idempotency_key, request_digest, request_id::text,
       final_status, final_rejection_reason, final_failure_reason, created_at
FROM public.request_intake
WHERE account_id = $1 AND idempotency_key = $2`

// intakeFinalise writes the terminal pointer once. `final_status IS NULL` in
// the WHERE clause is the pointer's whole story: one writer wins, the rest
// learn the pointer was already set, and no second write ever lands.
const intakeFinalise = `UPDATE public.request_intake
SET final_status = $3, final_rejection_reason = $4, final_failure_reason = $5
WHERE account_id = $1 AND idempotency_key = $2 AND final_status IS NULL`

// intakeExists is what tells Finalise's two zero-row outcomes apart — pointer
// already set (the caller's answer is false) versus record never written (the
// caller is looking at a bug, and gets ErrNotFound). It runs only on the
// zero-row path, so the happy path pays one statement, not two.
const intakeExists = `SELECT EXISTS (
    SELECT 1 FROM public.request_intake
    WHERE account_id = $1 AND idempotency_key = $2
)`

// IntakeRepository is the PostgreSQL implementation of the replay-record half
// of the persistence port.
type IntakeRepository struct {
	store persistence.Store
}

// NewIntakeRepository builds the repository over a store.
func NewIntakeRepository(store persistence.Store) *IntakeRepository {
	return &IntakeRepository{store: store}
}

// Insert implements persistence.IntakeRepository.
func (repository *IntakeRepository) Insert(ctx context.Context, intake execution.Intake) error {
	_, err := repository.store.Querier(ctx).ExecContext(ctx, intakeInsert,
		intake.AccountID,
		intake.IdempotencyKey,
		intake.RequestDigest,
		string(intake.RequestID),
		textOrNil(intakeFinalStatusText(intake)),
		textOrNil(string(intake.FinalRejectionReason)),
		textOrNil(string(intake.FinalFailureReason)),
		intake.CreatedAt,
	)
	if err != nil {
		if code(err) == "23505" && constraint(err) == "request_intake_scope_key" {
			return fmt.Errorf("postgres: insert intake: %w", persistence.ErrDuplicateIntake)
		}
		return fmt.Errorf("postgres: insert intake: %w", err)
	}
	return nil
}

// Find implements persistence.IntakeRepository.
func (repository *IntakeRepository) Find(ctx context.Context, accountID, idempotencyKey string) (execution.Intake, error) {
	row := repository.store.Querier(ctx).QueryRowContext(ctx, intakeFind, accountID, idempotencyKey)
	var (
		intake    execution.Intake
		requestID string
		final     sql.NullString
		rejection sql.NullString
		failure   sql.NullString
	)
	err := row.Scan(
		&intake.AccountID,
		&intake.IdempotencyKey,
		&intake.RequestDigest,
		&requestID,
		&final,
		&rejection,
		&failure,
		&intake.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return execution.Intake{}, fmt.Errorf("persistence: no replay record for account %q: %w", accountID, persistence.ErrNotFound)
	}
	if err != nil {
		return execution.Intake{}, fmt.Errorf("postgres: find intake: %w", err)
	}
	if intake.RequestID, err = identity.ParseRequestID(requestID); err != nil {
		// Fail closed: a replay record whose request id is not an identity is
		// a row this build cannot answer from.
		return execution.Intake{}, fmt.Errorf("postgres: find intake: %w", err)
	}
	if final.Valid {
		status := execution.FinalStatus(final.String)
		intake.FinalStatus = &status
		intake.FinalRejectionReason = execution.RejectionReason(rejection.String)
		intake.FinalFailureReason = execution.FailureReason(failure.String)
	}
	return intake, nil
}

// Finalise implements persistence.IntakeRepository.
func (repository *IntakeRepository) Finalise(ctx context.Context, accountID, idempotencyKey string, status execution.FinalStatus, rejection execution.RejectionReason, failure execution.FailureReason) (bool, error) {
	result, err := repository.store.Querier(ctx).ExecContext(ctx, intakeFinalise,
		accountID, idempotencyKey,
		string(status),
		textOrNil(string(rejection)),
		textOrNil(string(failure)),
	)
	if err != nil {
		return false, fmt.Errorf("postgres: finalise intake: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: finalise intake: %w", err)
	}
	if affected > 0 {
		return true, nil
	}
	// Zero rows: the pointer was already written, or the record does not
	// exist. One cheap read tells the caller which.
	var exists bool
	if err := repository.store.Querier(ctx).QueryRowContext(ctx, intakeExists, accountID, idempotencyKey).Scan(&exists); err != nil {
		return false, fmt.Errorf("postgres: finalise intake: %w", err)
	}
	if !exists {
		return false, fmt.Errorf("persistence: no replay record for account %q: %w", accountID, persistence.ErrNotFound)
	}
	return false, nil
}

// intakeFinalStatusText renders the pointer's status for the insert: empty
// while the original request still executes (textOrNil turns that into NULL),
// the terminal status once it has ended. It is a free function rather than a
// domain method so the domain's Intake never grows a row-shaped accessor it
// has no other use for.
func intakeFinalStatusText(intake execution.Intake) string {
	if intake.FinalStatus == nil {
		return ""
	}
	return string(*intake.FinalStatus)
}

// code returns the SQLSTATE of a driver error, or the empty string for
// everything else. Matching happens on the code and the constraint name —
// never on message text, which is the driver's prose and can change under a
// point release.
func code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// constraint returns the constraint name a 23505 or 23503 names, or the empty
// string. Two unique keys can fire on one table; which one fired is the
// difference between "the replay record exists" and "the settlement already
// happened", and the name is where that difference lives.
func constraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}
