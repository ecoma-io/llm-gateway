package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file is the scripted-statement tier for the analytics adapter, and it
// exists because of a bug it would have caught.
//
// The analytics series statement builds a VALUES list from the caller's bucket
// bounds. Every placeholder in that list is UNKNOWN-typed, because a VALUES
// row is not a table column and PostgreSQL has nothing to infer the type from
// — so the comparison the statement exists to serve fails to resolve, and the
// whole read is refused with `operator does not exist: timestamp with time
// zone >= text` before a single row is read. It was not caught by the
// integration tier at the time, because the integration tier did not exist
// yet, and it could not have been caught by a test that only watched WHICH
// HANDLE a statement ran on: the statement text was exactly what it claimed.
//
// So the observable here is the text and the arguments, and the assertion is
// structural rather than a golden string: a golden string pins today's
// phrasing, and a phrasing change that dropped a cast would pass it. What is
// asserted is the property — every timestamp placeholder in a VALUES row
// carries its own cast — which is the thing that was wrong.
//
// A second property is pinned here for the same reason. The read is bounded by
// a SET LOCAL, and SET LOCAL is a silent no-op outside a transaction: a
// scripted test cannot see whether the statement was scoped, but it CAN see
// whether it ran inside one, because this driver records begin and commit
// around the statements. That is the whole reason the read opens a unit of
// work of its own, and it is the one property of the file that a fake
// statement recorder checks better than a real database does.

// scriptedDriver answers every statement from a script and records the
// statement text and its arguments in order.
//
// The script is a list of SHAPES, one per statement the adapter is expected to
// run, and each statement answers with a single row of that shape. A shape is
// the list of driver.Value TYPES the real query projects in that position.
//
// Types rather than values, and not merely a column count: a driver that
// answered with NULLs would fail every scan, which sounds strict until you
// notice it also fails identically for a statement whose columns are correct
// and for one whose are not — so the strictness would not distinguish anything.
// A typed row is the smallest thing a real scan succeeds against, which means
// a scan that succeeds here would succeed there, and a scan that does NOT
// succeed is a real arity or type mismatch worth failing on.
//
// This is how the column-count defect was found: the series statement projects
// five columns and the read scanned four, and a driver that had counted
// columns would have refused the scan exactly as PostgreSQL would have.
type shape []driver.Value

// The shapes the five reads project, in order. A driver.Value of a type
// converts to anything compatible, so int64 covers the counts and the bigint
// casts, time.Time the timestamps and the freshness instant, and string the
// account the resolution returns.
var (
	// ord, bucket_start, bucket_end, with_facts, settled
	shapeSeries = shape{int64(0), time.Time{}, time.Time{}, int64(0), int64(0)}
	// COALESCE(SUM(...), 0)::bigint
	shapeScalar = shape{int64(0)}
	// two FILTERed sums
	shapePair = shape{int64(0), int64(0)}
	// three FILTERed counts
	shapeTriple = shape{int64(0), int64(0), int64(0)}
	// the cursor's own updated_at. The value is a real instant and not the
	// zero time, because the read refuses a zero — and that refusal is one of
	// the properties this tier pins, so a script that answered with a zero
	// would be testing the refusal rather than the read.
	shapeInstant = shape{time.Unix(1, 0).UTC()}
	// the resolved account
	shapeAccount = shape{""}
)

// readShape is the five reads the answer is made of, in order.
var readShape = []shape{shapeSeries, shapeScalar, shapePair, shapePair, shapeTriple, shapeInstant}

// scriptedDriver answers every statement from a script and records the
// statement text and its arguments in order.
type scriptedDriver struct {
	mu      sync.Mutex
	queries []recordedQuery
	shapes  []shape
	cursor  int
	// failures answers the statement at the given position — counting the
	// statements the read ASKS, in the order it asks them, and not the
	// statements it sets — with an error instead of a row. A shape says what a
	// statement RETURNS, and there is no shape for the statement that raised:
	// the read's own classification of a failure is a claim about the text of
	// the error rather than about the columns behind it, and a driver that
	// could only ever answer would have no way to pose it.
	failures map[int]error
	// walkFailures answers the statement at the given position with rows the
	// WALK then fails, which is how a driver reports an error that arrives
	// after the statement was accepted. A statement_timeout raises a few
	// hundred milliseconds into a read rather than at the moment it was issued,
	// so a read whose classification is only ever asked of the query has never
	// been asked about the half of the time it spends with rows in hand.
	walkFailures map[int]error
	// statementsRead counts the statements the read ASKED, as opposed to the
	// ones it set. A position is a place in the read's SEQUENCE OF QUESTIONS,
	// and a statement a read does not ask is not one of them — the bound is
	// set, and setting it is not a question.
	statementsRead int
	// begun records the options every transaction was started with. The
	// isolation level a unit of work runs at is carried HERE and nowhere
	// else — PostgreSQL refuses both SET TRANSACTION and set_config once the
	// transaction has issued a query — so a tier that cannot see this cannot
	// assert that a multi-statement read shared one snapshot.
	begun []driver.TxOptions
}

// failAt makes the statement at the zero-based position answer with err. It is
// set before the read runs, because the driver counts statements as they
// arrive and a failure armed afterwards would land on the next read.
func (s *scriptedDriver) failAt(position int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures == nil {
		s.failures = map[int]error{}
	}
	s.failures[position] = err
}

// failWalkAt makes the statement at the zero-based position answer with rows
// and then fail while they are being read.
func (s *scriptedDriver) failWalkAt(position int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.walkFailures == nil {
		s.walkFailures = map[int]error{}
	}
	s.walkFailures[position] = err
}

// takeFailure is the error the statement at the current position owes, if one
// was armed for it.
func (s *scriptedDriver) takeFailure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	position := s.statementsRead
	s.statementsRead++
	err, ok := s.failures[position]
	if !ok {
		return nil
	}
	delete(s.failures, position)
	return err
}

// isolations returns the isolation every unit of work was begun at, in order.
// It is the test's window onto a property that leaves no statement behind.
//
// The names are returned rather than the levels because a failure message that
// says "began at 0" is not a message a reader can act on: the question a
// failing test poses is "which level did it get instead of the one it asked
// for", and the answer to that is a word.
func (s *scriptedDriver) isolations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.begun))
	for _, opts := range s.begun {
		out = append(out, isolationName(opts.Isolation))
	}
	return out
}

// isolationName spells a driver isolation level, and says "default" for the
// zero value rather than printing a number — the zero value is what a caller
// gets when it names no level at all, and that is a meaningful thing to read
// in a failure message rather than an absence of one.
func isolationName(level driver.IsolationLevel) string {
	switch level {
	case driver.IsolationLevel(0):
		return "default"
	case driver.IsolationLevel(sql.LevelReadUncommitted):
		return "read uncommitted"
	case driver.IsolationLevel(sql.LevelReadCommitted):
		return "read committed"
	case driver.IsolationLevel(sql.LevelRepeatableRead):
		return "repeatable read"
	case driver.IsolationLevel(sql.LevelSerializable):
		return "serializable"
	default:
		return fmt.Sprintf("level %d", level)
	}
}

type recordedQuery struct {
	text string
	args []driver.Value
}

// newScripted registers a driver answering with the given shapes in order.
func newScripted(t *testing.T, shapes ...shape) (*scriptedDriver, *sql.DB) {
	t.Helper()

	s := &scriptedDriver{shapes: shapes}
	name := "scripted-postgres-" + nextScriptedName(t)
	sql.Register(name, s)

	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("sql.Open on the scripted driver: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return s, db
}

var scriptedNames = struct {
	sync.Mutex
	n int
}{}

func nextScriptedName(t *testing.T) string {
	t.Helper()
	scriptedNames.Lock()
	defer scriptedNames.Unlock()
	scriptedNames.n++
	return strconv.Itoa(scriptedNames.n)
}

func (s *scriptedDriver) Open(string) (driver.Conn, error) { return &scriptedConn{s: s}, nil }

type scriptedConn struct{ s *scriptedDriver }

func (c *scriptedConn) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}

func (c *scriptedConn) Begin() (driver.Tx, error) { return scriptedTx{}, nil }

// BeginTx records the options the caller began the transaction with. It exists
// so a test can assert on the isolation level WITHOUT the statement text: the
// level is carried on the BEGIN and nowhere else, and a driver that ignored
// driver.TxOptions would make the assertion pass for any implementation that
// got the constant onto the statement instead.
//
// The level is recorded and the transaction is begun READ COMMITTED, because
// that is what this scripted database is: a unit of work here is a scope for
// the statements, and nothing in this tier is about how a real server treats
// two readers at different moments. The recording is what travels back to the
// test; the behaviour is the driver's own.
func (c *scriptedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.s.mu.Lock()
	c.s.begun = append(c.s.begun, opts)
	c.s.mu.Unlock()
	return c.Begin()
}

func (c *scriptedConn) Close() error { return nil }

func (c *scriptedConn) Ping(context.Context) error { return nil }

// QueryContext implements driver.QueryerContext, which is the surface the
// reads in this adapter use. Arguments are recorded as the driver sees them,
// which is the point: a test that asserts a cast is in the TEXT is checking
// the text, and a test that asserts the ARGUMENT is a time.Time is checking
// that the value the caller passed is the value that will be compared.
func (c *scriptedConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	values := make([]driver.Value, 0, len(args))
	for _, a := range args {
		values = append(values, a.Value)
	}
	c.s.record(recordedQuery{text: query, args: values})
	if err := c.s.takeFailure(); err != nil {
		return nil, err
	}
	// The shape is taken before the walk failure is read, because takeFailure
	// is what advances the position both are counted against.
	shaped := c.s.shape()
	return c.s.result(shaped, c.s.walkFailure(shaped)), nil
}

// shape is the row shape the statement at the cursor answers with, from the
// script.
//
// The script CYCLES: past the last shape it starts again. A read is a
// sequence of statements, and a test that runs the read twice is running the
// same sequence twice — which is the only way to say "the bound is set on
// every read" rather than "on the first". A script that ran dry would have to
// guess a shape, and a guess that happened to be wide enough would let an
// arity defect through.
//
// It is the CLOSING edge that is walked first: takeFailure advances the same
// cursor, so whichever runs first must leave the position to the other. A
// failing statement is scripted out of the question, not answered with a shape
// it never gets as far as scanning.
func (s *scriptedDriver) shape() shape {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.shapes) == 0 {
		return nil
	}
	shaped := s.shapes[s.cursor%len(s.shapes)]
	s.cursor++
	return shaped
}

func (c *scriptedConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	values := make([]driver.Value, 0, len(args))
	for _, a := range args {
		values = append(values, a.Value)
	}
	c.s.record(recordedQuery{text: query, args: values})
	// Not armed with failures and not counted: the bound is a statement this
	// read sets rather than one it asks, so a position a test arms is a
	// position in the read's SEQUENCE OF QUESTIONS and the bound is not one of
	// them. Letting it consume a position would make every armed failure land
	// one statement later than the test said.
	return driver.RowsAffected(0), nil
}

func (s *scriptedDriver) record(q recordedQuery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, q)
}

// statements is the recorded stream, in order.
func (s *scriptedDriver) statements() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.queries))
	for _, q := range s.queries {
		out = append(out, q.text)
	}
	return out
}

// arguments is the recorded stream with its values, in order.
func (s *scriptedDriver) arguments() [][]driver.Value {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]driver.Value, 0, len(s.queries))
	for _, q := range s.queries {
		out = append(out, q.args)
	}
	return out
}

// theStatementNamed returns the first recorded statement containing fragment,
// or "" — which is what makes "this method issued no such statement" a
// failure an assertion can state rather than a nil dereference.
func (s *scriptedDriver) theStatementNamed(fragment string) string {
	for _, q := range s.statements() {
		if strings.Contains(q, fragment) {
			return q
		}
	}
	return ""
}

// result is the row the current statement answers with: the script's shape,
// as one row, and a walk that fails if a failure was armed for this position.
func (s *scriptedDriver) result(shaped shape, walkErr error) driver.Rows {
	return &scriptedRows{values: append(shape(nil), shaped...), walkErr: walkErr}
}

// walkFailure is the error the statement's rows owe once the read has begun
// walking them, and consumes the armed failure for that statement's position.
//
// A statement that failed rather than answered owes no walk at all, so the
// empty shape skips the lookup: the position has already been consumed by the
// failure that produced it.
func (s *scriptedDriver) walkFailure(shaped shape) error {
	if len(shaped) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	position := s.statementsRead - 1
	err, ok := s.walkFailures[position]
	if !ok {
		return nil
	}
	delete(s.walkFailures, position)
	return err
}

type scriptedTx struct{}

func (scriptedTx) Commit() error   { return nil }
func (scriptedTx) Rollback() error { return nil }

type scriptedRows struct {
	values  []driver.Value
	served  bool
	walkErr error
}

func (r *scriptedRows) Columns() []string {
	names := make([]string, len(r.values))
	for i := range names {
		names[i] = "c" + strconv.Itoa(i)
	}
	return names
}

func (r *scriptedRows) Close() error { return nil }

func (r *scriptedRows) Next(dest []driver.Value) error {
	if r.walkErr != nil {
		// Once, and then the walk is over: a driver that failed every call
		// would let a caller that ignored the error spin, and the callers under
		// test stop at the first one.
		err := r.walkErr
		r.walkErr = nil
		return err
	}
	if r.served {
		return io.EOF
	}
	r.served = true
	copy(dest, r.values)
	return nil
}

// containsAll reports whether every fragment appears in text. It is used for
// the structural assertions below, where a golden string would be the wrong
// tool: the claims are about the presence of a cast and a clause, not about
// the exact phrasing around them.
func containsAll(text string, fragments ...string) bool {
	for _, f := range fragments {
		if !strings.Contains(text, f) {
			return false
		}
	}
	return true
}
