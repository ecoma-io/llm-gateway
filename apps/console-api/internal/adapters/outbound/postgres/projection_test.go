package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/projection"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The projection repositories' tests over a scripted query surface. The
// fake-driver suite beside this file watches orchestration and refuses
// statements; the projection's contract lives one statement deeper — which
// statement is issued, in which order, with which arguments, and how the rows
// a driver answers become the port's types and its error vocabulary. Answering
// that honestly needs real *sql.Rows and *sql.Row values to Scan against, so
// this file ships a second, smaller driver: one that returns the rows the test
// scripted, records every call, and refuses anything it was not told about.
// The Scan and decode paths therefore run through the same standard-library
// machinery a live PostgreSQL sits behind.
//
// What is deliberately NOT proven here is the database's side of the contract
// — gapless allocation under concurrency, a rollback erasing both written
// rows, the upsert keeping one row per resource, the CHECKs pinning the
// grammar to its hex and enum forms. Only the real schema can answer those;
// they are the integration tier's subject, in projection_integration_test.go.

// projectionClock is the recorded instant every write in this file carries.
var projectionClock = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// One of each id the statements below have to carry. All three are valid
// version-4 UUIDs — the grammar the domain holds every projected id to — so
// a failure below is never the fixture's own shape being refused.
const (
	projectionEpoch     = "5f0e9d6c-3a1b-4c2d-8e7f-001122334455"
	projectionKeyID     = "1a2b3c4d-5e6f-4a1b-8c9d-0e1f2a3b4c5d"
	projectionAccountID = "7c3d1e2f-8a9b-4c3d-9e0f-1a2b3c4d5e6f"
)

// ---------------------------------------------------------------------------
// The scripted query surface.
// ---------------------------------------------------------------------------

// projectionScript is the answer sheet one test writes before it drives the
// adapter: for each statement the adapter may issue, either the rows it
// should read or the error it should meet. Every call is recorded with the
// arguments the adapter actually bound, because what the adapter binds is
// half of its contract — the boundary a snapshot is cut at, the limit a page
// is held to, the revision a log entry is written at.
type projectionScript struct {
	mu      sync.Mutex
	answers map[string]statementAnswer
	calls   []projectionCall
}

// statementAnswer is what one scripted statement answers: its rows, or the
// error it refuses with.
type statementAnswer struct {
	columns []string
	rows    [][]driver.Value
	err     error
}

// projectionCall is one recorded statement: the query and the argument values
// the standard library's converter handed the driver.
type projectionCall struct {
	query string
	args  []driver.Value
}

// returnRows scripts query to answer the given rows, one []driver.Value per
// row, positional like the adapter's Scan.
func (s *projectionScript) returnRows(query string, columns []string, rows ...[]driver.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[query] = statementAnswer{columns: columns, rows: rows}
}

// refuse scripts query to answer err. Overwrites any rows scripted before —
// the last word on a statement is the test's.
func (s *projectionScript) refuse(query string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[query] = statementAnswer{err: err}
}

// answer resolves the script for query. A read with nothing scripted is a test
// bug, and it is reported as one rather than answered with an empty page — an
// accidentally empty answer would make half of these tests pass for nothing.
func (s *projectionScript) answer(query string) (statementAnswer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	answer, ok := s.answers[query]
	return answer, ok
}

// record notes one issued statement, whatever its outcome.
func (s *projectionScript) record(query string, args []driver.NamedValue) {
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, projectionCall{query: query, args: values})
}

// callsFor returns the arguments of every run of query, in issue order.
func (s *projectionScript) callsFor(query string) [][]driver.Value {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]driver.Value
	for _, call := range s.calls {
		if call.query == query {
			out = append(out, call.args)
		}
	}
	return out
}

// soleCall returns the arguments of query's only run, failing the test when
// the statement ran a different number of times than the adapter's contract
// allows.
func (s *projectionScript) soleCall(t *testing.T, query string) []driver.Value {
	t.Helper()
	calls := s.callsFor(query)
	if len(calls) != 1 {
		t.Fatalf("the scripted statement %q ran %d times, want exactly 1", firstLine(query), len(calls))
	}
	return calls[0]
}

// order returns every issued statement's text in issue order — the proof that
// a record allocates before it appends and appends before it mirrors.
func (s *projectionScript) order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	for i, call := range s.calls {
		out[i] = call.query
	}
	return out
}

func firstLine(query string) string {
	if i := strings.IndexByte(query, '\n'); i >= 0 {
		return query[:i]
	}
	return query
}

// newProjectionScriptStore registers a fresh scripted driver under a unique
// name — sql.Register cannot unregister, so one name per test is the price of
// isolation, the same price the fake-driver suite pays — and returns the
// script beside a store over it.
func newProjectionScriptStore(t *testing.T) (*projectionScript, persistence.Store) {
	t.Helper()

	scriptRegisterMu.Lock()
	scriptRegistered++
	name := fmt.Sprintf("script-postgres-%d", scriptRegistered)
	scriptRegisterMu.Unlock()

	script := &projectionScript{answers: make(map[string]statementAnswer)}
	sql.Register(name, &projectionScriptDriver{script: script})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("sql.Open on the scripted driver: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return script, &projectionScriptStore{db: db}
}

var (
	scriptRegisterMu sync.Mutex
	scriptRegistered int
)

// projectionScriptDriver hands out connections that all answer from the one
// script; the pool may open as many as it likes.
type projectionScriptDriver struct{ script *projectionScript }

func (d *projectionScriptDriver) Open(string) (driver.Conn, error) {
	return &projectionScriptConn{script: d.script}, nil
}

// projectionScriptConn answers queries and execs from the script. Prepared
// statements and transactions are refused: the adapter issues its SQL
// directly, and the transaction mechanics around it are the fake-driver
// suite's and the integration tier's subjects, not this one's.
type projectionScriptConn struct{ script *projectionScript }

func (c *projectionScriptConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("script driver: prepared statements are not part of this contract")
}

func (c *projectionScriptConn) Close() error { return nil }

func (c *projectionScriptConn) Begin() (driver.Tx, error) {
	return nil, errors.New("script driver: transactions are not part of this contract")
}

func (c *projectionScriptConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.script.record(query, args)
	answer, scripted := c.script.answer(query)
	if scripted && answer.err != nil {
		return nil, answer.err
	}
	// An exec with nothing scripted succeeds: the tests that care about a
	// write refuse it explicitly, and the rest assert the arguments it
	// carried instead of restating that the write was scripted at all.
	return driver.RowsAffected(1), nil
}

func (c *projectionScriptConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.script.record(query, args)
	answer, scripted := c.script.answer(query)
	if !scripted {
		return nil, fmt.Errorf("script driver: no rows scripted for %q", firstLine(query))
	}
	if answer.err != nil {
		return nil, answer.err
	}
	return &projectionScriptRows{columns: answer.columns, rows: answer.rows}, nil
}

// projectionScriptRows is a driver.Rows over the values a test scripted. The
// values are handed to the standard library untouched, so Scan meets exactly
// the conversion it meets against a real driver — int64 revisions, string
// text, time.Time instants, nil for SQL NULL.
type projectionScriptRows struct {
	columns []string
	rows    [][]driver.Value
	next    int
}

func (r *projectionScriptRows) Columns() []string { return r.columns }

func (r *projectionScriptRows) Close() error { return nil }

func (r *projectionScriptRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

// projectionScriptStore is the store the projection repositories are built on
// in this file. Its Querier is the pool over the scripted driver, and its
// WithinTx hands the work through: this file pins what the repositories say
// and how they read, not what a unit of work commits — the real store's
// transaction mechanics are pinned by the fake-driver suite and, against the
// real database, by the integration tier.
type projectionScriptStore struct{ db *sql.DB }

var _ persistence.Store = (*projectionScriptStore)(nil)

func (s *projectionScriptStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *projectionScriptStore) Querier(context.Context) persistence.Querier { return s.db }

func (s *projectionScriptStore) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

// InUnitOfWork answers true without exception: the statement-order tests
// below call the recorder directly with a bare context and are about the
// statements, not the precondition, so this scripted surface pretends every
// context sits inside a unit of work. The precondition itself has its own
// store and its own test.
func (s *projectionScriptStore) InUnitOfWork(context.Context) bool { return true }

// ---------------------------------------------------------------------------
// Fixtures and assertion helpers.
// ---------------------------------------------------------------------------

// fixtureDigest mints a valid digest from one two-character hex pair repeated
// to the grammar's length, so a test can hold two visually distinct digests
// without sixty-four-character literals.
func fixtureDigest(t *testing.T, pair string) projection.Digest {
	t.Helper()
	digest, err := projection.NewDigest(strings.Repeat(pair, 32))
	if err != nil {
		t.Fatalf("NewDigest(%q repeated to length): %v", pair, err)
	}
	return digest
}

// fixtureCredential builds a projected credential through the domain
// constructor, so every value the adapter is handed below is one the grammar
// vouches for and a refusal is never the fixture's own shape.
func fixtureCredential(t *testing.T, keyID, accountID string, digest projection.Digest, state projection.CredentialState, revokedAt *time.Time) projection.Credential {
	t.Helper()
	credential, err := projection.NewCredential(keyID, accountID, string(digest), state, revokedAt)
	if err != nil {
		t.Fatalf("NewCredential(%s, %s, %s): %v", keyID, accountID, state, err)
	}
	return credential
}

// fixtureAccount is the account half of the same discipline.
func fixtureAccount(t *testing.T, accountID string, state projection.AccountState) projection.Account {
	t.Helper()
	account, err := projection.NewAccount(accountID, state)
	if err != nil {
		t.Fatalf("NewAccount(%s, %s): %v", accountID, state, err)
	}
	return account
}

// wantStatementArg asserts one bound argument of a recorded statement. The
// want side is spelled in the values the driver receives — the standard
// library's converter narrows the adapter's uint64 revisions to int64, the
// form every bigint argument travels in — because that conversion is part of
// what the adapter actually said.
func wantStatementArg(t *testing.T, args []driver.Value, i int, want any) {
	t.Helper()
	if i >= len(args) {
		t.Fatalf("the statement ran with %d arguments, want at least %d", len(args), i+1)
	}
	got := args[i]
	switch want := want.(type) {
	case nil:
		if got != nil {
			t.Fatalf("argument %d = %v (%T), want NULL", i+1, got, got)
		}
	case time.Time:
		instant, ok := got.(time.Time)
		if !ok || !instant.Equal(want) {
			t.Fatalf("argument %d = %v (%T), want the instant %v", i+1, got, got, want)
		}
	case string:
		if got != want {
			t.Fatalf("argument %d = %q, want %q", i+1, got, want)
		}
	case int64:
		if got != want {
			t.Fatalf("argument %d = %v, want %d", i+1, got, want)
		}
	case []byte:
		bytes, ok := got.([]byte)
		if !ok || string(bytes) != string(want) {
			t.Fatalf("argument %d = %v (%T), want the bytes %q", i+1, got, got, want)
		}
	default:
		t.Fatalf("wantStatementArg: unsupported want type %T — extend the helper", want)
	}
}

// ---------------------------------------------------------------------------
// The tests.
// ---------------------------------------------------------------------------

// TestProjectionRecorderRefusesAContextWithNoUnitOfWork pins the recorder's
// precondition: its three statements are one atomic unit beside the authority
// write, and on a pool they would be three autocommits — a concurrent writer
// could commit between the allocation and the append, and a mirror failure
// would leave a committed log entry with no mirror row. A context with no
// unit of work is therefore refused before a statement is issued, not
// degraded into the pool.
// unitOfWorklessStore is the store whose every context sits outside a unit
// of work — the state the recorder must refuse. The embedded interface is
// never reached: the guard fires before any other store member would.
type unitOfWorklessStore struct{ persistence.Store }

func (unitOfWorklessStore) InUnitOfWork(context.Context) bool { return false }

func TestProjectionRecorderRefusesAContextWithNoUnitOfWork(t *testing.T) {
	recorder := NewProjectionChangeRecorder(unitOfWorklessStore{})

	if err := recorder.RecordCredentialChange(t.Context(), projectionClock, projection.Credential{}); err == nil {
		t.Fatal("RecordCredentialChange outside a unit of work was accepted")
	} else if !strings.Contains(err.Error(), "no unit of work") {
		t.Errorf("error = %v, want it to name the missing unit of work", err)
	}
	if err := recorder.RecordAccountChange(t.Context(), projectionClock, projection.Account{}); err == nil {
		t.Fatal("RecordAccountChange outside a unit of work was accepted")
	}
}

func TestProjectionConstructorsRefuseANilStore(t *testing.T) {
	for name, build := range map[string]func(){
		"projection log":      func() { NewProjectionLog(nil) },
		"projection recorder": func() { NewProjectionChangeRecorder(nil) },
	} {
		panicked := false
		func() {
			defer func() {
				if recover() != nil {
					panicked = true
				}
			}()
			build()
		}()
		if !panicked {
			t.Fatalf("%s: New(nil) did not panic — a nil store is a wiring defect, not a runtime surprise", name)
		}
	}
}

func TestProjectionChangesAfterRefusesALimitBelowOneWithoutTouchingTheStore(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	changes := NewProjectionLog(store)

	// Zero and negative are both refusals: a page of no entries is not a
	// limit the contract knows, and the adapter must say so before the
	// database is asked to apply one.
	for _, limit := range []int{0, -1} {
		page, err := changes.ChangesAfter(context.Background(), 3, limit)
		if err == nil {
			t.Fatalf("ChangesAfter with limit %d error = nil, want the refusal", limit)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("limit must be at least 1, got %d", limit)) {
			t.Fatalf("ChangesAfter with limit %d error = %q, want it to name the refused limit", limit, err)
		}
		if page != nil {
			t.Fatalf("ChangesAfter with limit %d returned a page — a refused page must not exist", limit)
		}
		if got := script.order(); len(got) != 0 {
			t.Fatalf("the store was asked %v, want nothing — a bad limit is refused before any statement is issued", got)
		}
	}
}

func TestProjectionHeadReportsAnUnmigratedCounter(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	script.refuse(headRevision, sql.ErrNoRows)

	_, err := NewProjectionLog(store).Head(context.Background())
	if err == nil || !strings.Contains(err.Error(), "run the projection foundation migration") {
		t.Fatalf("Head over a counter with no row error = %v, want the one failure that names the migration that creates it", err)
	}
}

func TestProjectionHeadCarriesADriverFailureUnmapped(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	boom := errors.New("connection refused")
	script.refuse(headRevision, boom)

	_, err := NewProjectionLog(store).Head(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Head returned %v, want the driver's refusal wrapped, not swallowed or replaced", err)
	}
	if strings.Contains(err.Error(), "migration") {
		t.Fatalf("Head error = %q — only a missing counter row is an unmigrated database, not every failure reading it", err)
	}
}

func TestProjectionHeadReturnsTheCounterItRead(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	script.returnRows(headRevision, []string{"epoch", "last_revision"},
		[]driver.Value{projectionEpoch, int64(9)})

	head, err := NewProjectionLog(store).Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Epoch != projectionEpoch || head.LastRevision != 9 {
		t.Fatalf("Head = %+v, want the counter's row whole: epoch %q at revision 9", head, projectionEpoch)
	}
}

func TestProjectionHeadRefusesACorruptEpoch(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	script.returnRows(headRevision, []string{"epoch", "last_revision"},
		[]driver.Value{"not-a-uuid", int64(9)})

	// The epoch names the timeline every delivered position joins. A row that
	// carries anything else is a corrupted counter, and inventing a position
	// from it would be worse than refusing to speak.
	_, err := NewProjectionLog(store).Head(context.Background())
	if err == nil || !strings.Contains(err.Error(), "epoch") {
		t.Fatalf("Head over a non-UUID epoch error = %v, want the epoch refused loudly", err)
	}
}

func TestProjectionCredentialReturnsTheMirrorRowDecoded(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	digest := fixtureDigest(t, "ab")
	script.returnRows(credentialByKeyID, []string{"key_id", "account_id", "digest", "state", "revoked_at"},
		[]driver.Value{projectionKeyID, projectionAccountID, string(digest), "active", nil})

	credential, err := NewProjectionLog(store).Credential(context.Background(), projectionKeyID)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if credential.KeyID != projectionKeyID || credential.AccountID != projectionAccountID ||
		credential.Digest != digest || credential.State != projection.CredentialActive || credential.RevokedAt != nil {
		t.Fatalf("Credential = %+v, want the mirror row decoded with a NULL revocation read as absence", credential)
	}
	wantStatementArg(t, script.soleCall(t, credentialByKeyID), 0, projectionKeyID)
}

func TestProjectionCredentialRefusesARowOutsideTheGrammar(t *testing.T) {
	digest := fixtureDigest(t, "cd")
	tests := []struct {
		name    string
		row     []driver.Value
		wantErr string
	}{
		{
			name:    "a state the contract does not spell",
			row:     []driver.Value{projectionKeyID, projectionAccountID, string(digest), "archived", nil},
			wantErr: "unknown state",
		},
		{
			name:    "a revoked row with no instant",
			row:     []driver.Value{projectionKeyID, projectionAccountID, string(digest), "revoked", nil},
			wantErr: "must carry the instant it was revoked",
		},
		{
			name:    "a digest outside its hex form",
			row:     []driver.Value{projectionKeyID, projectionAccountID, "NOT-HEX", "active", nil},
			wantErr: "digest must be",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script, store := newProjectionScriptStore(t)
			script.returnRows(credentialByKeyID, []string{"key_id", "account_id", "digest", "state", "revoked_at"}, tt.row)

			_, err := NewProjectionLog(store).Credential(context.Background(), projectionKeyID)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Credential error = %v, want the row refused as %q", err, tt.wantErr)
			}
		})
	}
}

func TestProjectionCredentialMapsAMissToTheNotFoundSentinel(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	script.refuse(credentialByKeyID, sql.ErrNoRows)

	_, err := NewProjectionLog(store).Credential(context.Background(), projectionKeyID)
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("Credential for an absent row returned %v, want persistence.ErrNotFound — a key minted before the projection foundation is a verdict, not a failure", err)
	}
}

func TestProjectionCredentialRefusesARowShapedUnlikeItsQuery(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	script.returnRows(credentialByKeyID, []string{"key_id", "account_id", "digest"},
		[]driver.Value{projectionKeyID, projectionAccountID, strings.Repeat("ab", 32)})

	// Three columns where the adapter reads five: a row that answers fewer
	// things than its query asks must fail the read, never silently read a
	// neighbour's column.
	_, err := NewProjectionLog(store).Credential(context.Background(), projectionKeyID)
	if err == nil {
		t.Fatal("Credential over a short row error = nil, want the scan refused")
	}
	if !strings.Contains(err.Error(), "projection credential "+projectionKeyID) {
		t.Fatalf("Credential error = %q, want it to name the read that failed", err)
	}
}

func TestProjectionSnapshotCutsTheMirrorsAtTheBoundaryItRead(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	revokedAt := projectionClock.Add(-time.Hour)
	script.returnRows(headRevision, []string{"epoch", "last_revision"},
		[]driver.Value{projectionEpoch, int64(2)})
	script.returnRows(snapshotCredentials, []string{"key_id", "account_id", "digest", "state", "revoked_at"},
		[]driver.Value{projectionKeyID, projectionAccountID, string(fixtureDigest(t, "ab")), "active", nil},
		[]driver.Value{"9d8c7b6a-5e4f-4a3b-8c9d-0e1f2a3b4c5d", projectionAccountID, string(fixtureDigest(t, "cd")), "revoked", revokedAt})
	script.returnRows(snapshotAccounts, []string{"account_id", "state"},
		[]driver.Value{projectionAccountID, "suspended"})

	snapshot, err := NewProjectionLog(store).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Revision != 2 || snapshot.Epoch != projectionEpoch {
		t.Fatalf("Snapshot boundary = (%q, %d), want the counter it read: %q at 2", snapshot.Epoch, snapshot.Revision, projectionEpoch)
	}
	if len(snapshot.Keys) != 2 || snapshot.Keys[0].State != projection.CredentialActive || snapshot.Keys[1].State != projection.CredentialRevoked {
		t.Fatalf("Snapshot keys = %+v, want the active and the revoked credential", snapshot.Keys)
	}
	if snapshot.Keys[1].RevokedAt == nil || !snapshot.Keys[1].RevokedAt.Equal(revokedAt) {
		t.Fatalf("revoked key's instant = %v, want %v — a NULL-adjacent instant must not be invented or lost", snapshot.Keys[1].RevokedAt, revokedAt)
	}
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].State != projection.AccountSuspended {
		t.Fatalf("Snapshot accounts = %+v, want the one suspended account", snapshot.Accounts)
	}
	// The cut is the comparison: both mirror reads must have been handed the
	// boundary the head read produced, as the int64 every bigint argument
	// travels in.
	for _, query := range []string{snapshotCredentials, snapshotAccounts} {
		wantStatementArg(t, script.soleCall(t, query), 0, int64(2))
	}
}

func TestProjectionSnapshotRefusesTheWholeCutWhenOneRowIsOutsideTheGrammar(t *testing.T) {
	digest := fixtureDigest(t, "ab")
	tests := []struct {
		name     string
		keyRows  [][]driver.Value
		acctRows [][]driver.Value
		wantErr  string
	}{
		{
			name:    "a credential row outside the grammar",
			keyRows: [][]driver.Value{{projectionKeyID, projectionAccountID, string(digest), "archived", nil}},
			wantErr: "unknown state",
		},
		{
			name:     "an account row outside the grammar",
			acctRows: [][]driver.Value{{projectionAccountID, "archived"}},
			wantErr:  "unknown state",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script, store := newProjectionScriptStore(t)
			script.returnRows(headRevision, []string{"epoch", "last_revision"}, []driver.Value{projectionEpoch, int64(1)})
			script.returnRows(snapshotCredentials, []string{"key_id", "account_id", "digest", "state", "revoked_at"}, tt.keyRows...)
			script.returnRows(snapshotAccounts, []string{"account_id", "state"}, tt.acctRows...)

			// Delivering half a snapshot would bootstrap a consumer into a
			// projection nobody can vouch for, so one bad row refuses the cut
			// whole.
			_, err := NewProjectionLog(store).Snapshot(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Snapshot error = %v, want the cut refused as %q", err, tt.wantErr)
			}
		})
	}
}

func TestProjectionChangesAfterPagesAndDecodesTheLog(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	credential := fixtureCredential(t, projectionKeyID, projectionAccountID, fixtureDigest(t, "ab"), projection.CredentialActive, nil)
	account := fixtureAccount(t, projectionAccountID, projection.AccountSuspended)
	credentialPayload, err := credential.PayloadJSON()
	if err != nil {
		t.Fatalf("credential payload: %v", err)
	}
	accountPayload, err := account.PayloadJSON()
	if err != nil {
		t.Fatalf("account payload: %v", err)
	}
	script.returnRows(changesAfter, []string{"revision", "resource_kind", "resource_id", "recorded_at", "payload"},
		[]driver.Value{int64(4), "api_key", projectionKeyID, projectionClock, []byte(credentialPayload)},
		[]driver.Value{int64(5), "account", projectionAccountID, projectionClock, []byte(accountPayload)})

	page, err := NewProjectionLog(store).ChangesAfter(context.Background(), 3, 2)
	if err != nil {
		t.Fatalf("ChangesAfter: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("ChangesAfter returned %d entries, want 2", len(page))
	}
	first := page[0]
	if first.Revision != 4 || first.Kind != projection.KindAPIKey || first.ResourceID != projectionKeyID {
		t.Fatalf("first entry = (%d, %s, %s), want revision 4 of kind api_key for %s", first.Revision, first.Kind, first.ResourceID, projectionKeyID)
	}
	if first.Credential().Digest != credential.Digest || first.Credential().State != projection.CredentialActive {
		t.Fatalf("first entry's credential = %+v, want the payload's digest and state decoded through the grammar", first.Credential())
	}
	second := page[1]
	if second.Revision != 5 || second.Kind != projection.KindAccount || second.Account().State != projection.AccountSuspended {
		t.Fatalf("second entry = (%d, %s, %+v), want revision 5 of kind account carrying the suspended state", second.Revision, second.Kind, second.Account())
	}

	// The page's shape was the database's to apply: the boundary strictly
	// after, the limit bound, in the order the statement reads them.
	args := script.soleCall(t, changesAfter)
	wantStatementArg(t, args, 0, int64(3))
	wantStatementArg(t, args, 1, int64(2))
}

func TestProjectionChangesAfterCarriesAQueryFailureUnmapped(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	boom := errors.New("connection refused")
	script.refuse(changesAfter, boom)

	_, err := NewProjectionLog(store).ChangesAfter(context.Background(), 3, 2)
	if !errors.Is(err, boom) {
		t.Fatalf("ChangesAfter returned %v, want the driver's refusal wrapped", err)
	}
	if !strings.Contains(err.Error(), "changes after 3") {
		t.Fatalf("ChangesAfter error = %q, want it to name the read that failed", err)
	}
}

func TestProjectionChangesAfterRefusesAPageCarryingARowOutsideTheGrammar(t *testing.T) {
	// The schema pins most of these shapes at write time, and the log is
	// history: a row the grammar cannot still read is a corrupted log or a
	// schema this build predates, and either is refused whole rather than
	// skipped — a skipped row is how a mirror loses a change forever.
	tests := []struct {
		name    string
		kind    string
		payload string
		wantErr string
	}{
		{
			name:    "an api_key digest outside its hex form",
			kind:    "api_key",
			payload: `{"account_id":"` + projectionAccountID + `","digest":"AB12","state":"active","revoked_at":null}`,
			wantErr: "digest must be",
		},
		{
			name:    "an api_key state the contract does not spell",
			kind:    "api_key",
			payload: `{"account_id":"` + projectionAccountID + `","digest":"` + strings.Repeat("ab", 32) + `","state":"archived","revoked_at":null}`,
			wantErr: "unknown state",
		},
		{
			name:    "an api_key payload that is not the object shape",
			kind:    "api_key",
			payload: `["revoked"]`,
			wantErr: "decode api_key payload",
		},
		{
			name:    "an account state the contract does not spell",
			kind:    "account",
			payload: `{"state":"archived"}`,
			wantErr: "unknown state",
		},
		{
			name:    "a payload that is not JSON",
			kind:    "account",
			payload: `{`,
			wantErr: "decode account payload",
		},
		{
			name:    "a resource kind the protocol does not carry",
			kind:    "tenant",
			payload: `{"state":"active"}`,
			wantErr: "unknown resource kind",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script, store := newProjectionScriptStore(t)
			script.returnRows(changesAfter, []string{"revision", "resource_kind", "resource_id", "recorded_at", "payload"},
				[]driver.Value{int64(4), tt.kind, projectionKeyID, projectionClock, []byte(tt.payload)})

			page, err := NewProjectionLog(store).ChangesAfter(context.Background(), 3, 1)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ChangesAfter error = %v, want the page refused as %q", err, tt.wantErr)
			}
			if page != nil {
				t.Fatalf("ChangesAfter returned a page beside the refusal — a refused page must not exist")
			}
		})
	}
}

func TestProjectionRecordCredentialChangeAllocatesThenAppendsThenMirrors(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	script.returnRows(allocateRevision, []string{"last_revision"}, []driver.Value{int64(4)})
	credential := fixtureCredential(t, projectionKeyID, projectionAccountID, fixtureDigest(t, "ab"), projection.CredentialActive, nil)
	payload, err := credential.PayloadJSON()
	if err != nil {
		t.Fatalf("credential payload: %v", err)
	}

	if err := NewProjectionChangeRecorder(store).RecordCredentialChange(context.Background(), projectionClock, credential); err != nil {
		t.Fatalf("RecordCredentialChange: %v", err)
	}

	// Allocation first, log append second, mirror third — the order the port
	// promises, and the order that keeps the mirror never ahead of the log.
	got, want := script.order(), []string{allocateRevision, insertCredentialChange, upsertCredentialMirror}
	if !slices.Equal(got, want) {
		t.Fatalf("statements issued were %q, want %q — allocation first, log append second, mirror third", got, want)
	}

	// The log entry carries the allocated revision, the recorded instant and
	// the payload verbatim — the same bytes a redelivery will carry.
	appendArgs := script.soleCall(t, insertCredentialChange)
	wantStatementArg(t, appendArgs, 0, int64(4))
	wantStatementArg(t, appendArgs, 1, projectionKeyID)
	wantStatementArg(t, appendArgs, 2, projectionClock)
	wantStatementArg(t, appendArgs, 3, []byte(payload))

	// The mirror row is the same state, with the revision it came from — and,
	// for an active credential, NULL where a revocation instant would be.
	mirrorArgs := script.soleCall(t, upsertCredentialMirror)
	wantStatementArg(t, mirrorArgs, 0, projectionKeyID)
	wantStatementArg(t, mirrorArgs, 1, projectionAccountID)
	wantStatementArg(t, mirrorArgs, 2, string(credential.Digest))
	wantStatementArg(t, mirrorArgs, 3, "active")
	wantStatementArg(t, mirrorArgs, 4, nil)
	wantStatementArg(t, mirrorArgs, 5, int64(4))
	wantStatementArg(t, mirrorArgs, 6, projectionClock)
}

func TestProjectionRecordCredentialChangePassesTheRevocationInstantOn(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	script.returnRows(allocateRevision, []string{"last_revision"}, []driver.Value{int64(4)})
	revokedAt := projectionClock.Add(-time.Hour)
	credential := fixtureCredential(t, projectionKeyID, projectionAccountID, fixtureDigest(t, "ab"), projection.CredentialRevoked, &revokedAt)

	if err := NewProjectionChangeRecorder(store).RecordCredentialChange(context.Background(), projectionClock, credential); err != nil {
		t.Fatalf("RecordCredentialChange: %v", err)
	}

	// The zero pointer is the absence of a revocation; a pointer is the
	// instant itself. Either way the mirror's column receives what the
	// grammar pairs with the state, never an empty stand-in for it.
	wantStatementArg(t, script.soleCall(t, upsertCredentialMirror), 4, revokedAt)
}

func TestProjectionRecordCredentialChangeNamesTheStatementThatFailed(t *testing.T) {
	tests := []struct {
		name    string
		refuse  string
		wantErr string
	}{
		{name: "the counter refuses", refuse: allocateRevision, wantErr: "allocate revision"},
		{name: "the log append refuses", refuse: insertCredentialChange, wantErr: "at revision 4"},
		{name: "the mirror refuses", refuse: upsertCredentialMirror, wantErr: "mirror"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script, store := newProjectionScriptStore(t)
			script.returnRows(allocateRevision, []string{"last_revision"}, []driver.Value{int64(4)})
			boom := errors.New("the statement was refused")
			script.refuse(tt.refuse, boom)
			credential := fixtureCredential(t, projectionKeyID, projectionAccountID, fixtureDigest(t, "ab"), projection.CredentialActive, nil)

			err := NewProjectionChangeRecorder(store).RecordCredentialChange(context.Background(), projectionClock, credential)
			if !errors.Is(err, boom) {
				t.Fatalf("RecordCredentialChange returned %v, want the refusal wrapped", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("RecordCredentialChange error = %q, want it to name the failed statement as %q", err, tt.wantErr)
			}
		})
	}
}

func TestProjectionRecordCredentialChangeWritesNothingWhenTheCounterRefuses(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	script.refuse(allocateRevision, errors.New("the counter was refused"))
	credential := fixtureCredential(t, projectionKeyID, projectionAccountID, fixtureDigest(t, "ab"), projection.CredentialActive, nil)

	if err := NewProjectionChangeRecorder(store).RecordCredentialChange(context.Background(), projectionClock, credential); err == nil {
		t.Fatal("RecordCredentialChange error = nil, want the allocation refusal")
	}
	// No revision, no entry and no mirror: a record that cannot be numbered
	// must not become half of one.
	for _, query := range []string{insertCredentialChange, upsertCredentialMirror} {
		if got := script.callsFor(query); len(got) != 0 {
			t.Fatalf("a statement was issued after the counter refused: %q — nothing may be written without a revision", firstLine(query))
		}
	}
}

func TestProjectionRecordAccountChangeAppendsThenMirrors(t *testing.T) {
	script, store := newProjectionScriptStore(t)
	script.returnRows(allocateRevision, []string{"last_revision"}, []driver.Value{int64(7)})
	account := fixtureAccount(t, projectionAccountID, projection.AccountClosed)
	payload, err := account.PayloadJSON()
	if err != nil {
		t.Fatalf("account payload: %v", err)
	}

	if err := NewProjectionChangeRecorder(store).RecordAccountChange(context.Background(), projectionClock, account); err != nil {
		t.Fatalf("RecordAccountChange: %v", err)
	}

	got, want := script.order(), []string{allocateRevision, insertAccountChange, upsertAccountMirror}
	if !slices.Equal(got, want) {
		t.Fatalf("statements issued were %q, want %q — allocation first, log append second, mirror third", got, want)
	}
	appendArgs := script.soleCall(t, insertAccountChange)
	wantStatementArg(t, appendArgs, 0, int64(7))
	wantStatementArg(t, appendArgs, 1, projectionAccountID)
	wantStatementArg(t, appendArgs, 2, projectionClock)
	wantStatementArg(t, appendArgs, 3, []byte(payload))
	mirrorArgs := script.soleCall(t, upsertAccountMirror)
	wantStatementArg(t, mirrorArgs, 0, projectionAccountID)
	wantStatementArg(t, mirrorArgs, 1, "closed")
	wantStatementArg(t, mirrorArgs, 2, int64(7))
	wantStatementArg(t, mirrorArgs, 3, projectionClock)
}

func TestProjectionRecordAccountChangeNamesTheStatementThatFailed(t *testing.T) {
	tests := []struct {
		name    string
		refuse  string
		wantErr string
	}{
		{name: "the counter refuses", refuse: allocateRevision, wantErr: "allocate revision"},
		{name: "the log append refuses", refuse: insertAccountChange, wantErr: "at revision 7"},
		{name: "the mirror refuses", refuse: upsertAccountMirror, wantErr: "mirror"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script, store := newProjectionScriptStore(t)
			script.returnRows(allocateRevision, []string{"last_revision"}, []driver.Value{int64(7)})
			boom := errors.New("the statement was refused")
			script.refuse(tt.refuse, boom)
			account := fixtureAccount(t, projectionAccountID, projection.AccountClosed)

			err := NewProjectionChangeRecorder(store).RecordAccountChange(context.Background(), projectionClock, account)
			if !errors.Is(err, boom) {
				t.Fatalf("RecordAccountChange returned %v, want the refusal wrapped", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("RecordAccountChange error = %q, want it to name the failed statement as %q", err, tt.wantErr)
			}
		})
	}
}
