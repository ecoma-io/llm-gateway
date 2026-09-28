package main

// This file is the one place both halves of the analytics read's deadline are
// visible at once, and it is the composition root's for the same reason the
// fact-ingestion seam test is: the arch rule forbids the adapter from naming
// the application and the application from naming the adapter, so a test that
// compares the two constants has exactly one home, and wiring them together is
// this package's whole job.
//
// The claim being pinned is the one both constants' own comments make: the
// server-side statement timeout and the application-side context deadline are
// EQUAL. The two being equal is the whole design. A server bound LONGER
// would be dead weight — the context is cancelled first and the connection is
// held until PostgreSQL notices — and a server bound SHORTER would make a read
// fail as a timeout while the caller still believed it had time left. Neither
// mistake is visible from inside either layer: the application can set a
// deadline and never learn what the database was told, and the adapter can set
// a statement timeout and never learn when the caller gave up.
//
// So the test asserts against the number the read actually SENDS rather than
// against a constant, because a constant is exactly what a future edit would
// change on one side only. A wiring mistake is silent; this makes it not.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/outbound/postgres"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/analytics"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// registerBoundRecorder registers a recorder under a name unique to this run
// and returns it. sql.Register panics on a duplicate name and a name derived
// from the pointer would defeat the global registry's own bookkeeping, so the
// counter is what makes the name unique and the test is what makes it correct.
var boundRecorderNames = atomic.Uint64{}

func registerBoundRecorder(t *testing.T, r *boundRecorder) string {
	t.Helper()
	name := "analytics-read-bound-" + strconv.FormatUint(boundRecorderNames.Add(1), 10)
	sql.Register(name, r)
	return name
}

func openRecorded(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("sql.Open on the recording driver: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// boundProbeQuery is the smallest query the adapter will accept: one bucket,
// so the VALUES list is one row and the read is five statements and no more.
// The account is a well-formed id that names no row, because the read is
// never completed — the first scan fails — and nothing about the answer is
// under test.
func boundProbeQuery() persistence.UsageQuery {
	from := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	return persistence.UsageQuery{
		AccountID: "018f0000-0000-7000-8000-000000000001",
		From:      from,
		To:        from.Add(time.Hour),
		Buckets:   []analytics.Bucket{{Start: from, End: from.Add(time.Hour)}},
	}
}

// readBoundPattern pulls the number out of a `SET LOCAL statement_timeout = …`
// statement. It matches the digits rather than a literal, so the test compares
// DURATIONS and not spellings: "5s" and "5000ms" are the same bound and a test
// that failed on the second would be pinning a formatting choice while missing
// the one that matters.
var readBoundPattern = regexp.MustCompile(`SET LOCAL statement_timeout\s*=\s*'?([0-9]+)\s*([a-z]+)'?`)

// TestTheReadBoundIsTheSameAsTheApplicationDeadline is the pairing. The
// adapter's bound is unexported and its statement is built inside the adapter,
// so the only way to see the number it will send is to run the read and read
// the statement back — which is what the driver here does.
//
// The driver answers every statement with an empty row set, because the read
// is not being measured: it is being WATCHED. What matters is the text the
// adapter issues, and a driver that records it and returns no rows lets the
// read fail at the first scan without the bound ever having been skipped.
func TestTheReadBoundIsTheSameAsTheApplicationDeadline(t *testing.T) {
	recorder := &boundRecorder{}
	name := registerBoundRecorder(t, recorder)
	db := openRecorded(t, name)

	// The read is issued through the real adapter, so a future edit that
	// dropped the SET LOCAL entirely — the silent case this test exists for —
	// would leave nothing to compare and fail here rather than pass.
	_, _, _ = postgres.NewAnalytics(postgres.New(db)).Usage(t.Context(), boundProbeQuery())

	var found string
	for _, statement := range recorder.seen() {
		if match := readBoundPattern.FindStringSubmatch(statement); match != nil {
			found = strings.TrimSpace(match[1] + match[2])
			break
		}
	}
	if found == "" {
		t.Fatalf("no read issued a SET LOCAL statement_timeout; the read ran unbounded, and a read that cannot be cancelled server-side holds its connection until PostgreSQL notices the context\nstatements:\n%s",
			strings.Join(recorder.seen(), "\n---\n"))
	}

	adapterBound, err := time.ParseDuration(found)
	if err != nil {
		t.Fatalf("the read's own bound %q is not a duration: %v", found, err)
	}
	if adapterBound != application.AnalyticsReadTimeout {
		t.Errorf("the read runs under %s at the server and %s in the application; they must be equal — a server bound longer is dead weight, and one shorter fails a read while the caller still believes it has time left",
			adapterBound, application.AnalyticsReadTimeout)
	}
}

// TestTheFiveReadsShareOneSnapshot pins the isolation the read asks its unit of
// work for, and it is a separate test from the bound because the two are
// different claims with different failure modes.
//
// The plane's units of work run at READ COMMITTED on purpose: the port's
// concurrency model is a single guarded statement per write, with no
// read-modify-write spanning statements, so a stronger level would cost every
// write on the plane to guard something no write can lose. A report is
// different. It runs five statements that answer five halves of ONE question,
// and at READ COMMITTED each takes its own snapshot — so a settlement
// committing between the series read and the money read yields a bucket that
// says "settled" beside a settled amount that excludes that settlement. Both
// figures are individually correct and the answer they make is not, which is
// the shape of defect a reader cannot detect from either number.
//
// The level is read off the BEGIN rather than off a statement, and that is not
// a stylistic choice. PostgreSQL refuses both SET TRANSACTION and
// set_config('transaction_isolation') with SQLSTATE 25001 the moment a
// transaction has issued a query, so the level CANNOT be carried on a
// statement that follows one — the only place it can be is the BEGIN. A test
// that watched statement text would therefore be watching for a construct the
// database does not permit, and would pass an implementation that had got the
// level somewhere it never takes effect.
func TestTheFiveReadsShareOneSnapshot(t *testing.T) {
	recorder := &boundRecorder{}
	name := registerBoundRecorder(t, recorder)
	db := openRecorded(t, name)

	_, _, _ = postgres.NewAnalytics(postgres.New(db)).Usage(t.Context(), boundProbeQuery())

	levels := recorder.isolations()
	if len(levels) == 0 {
		t.Fatalf("the read began no transaction at all; its five statements each take their own snapshot and a settlement committing between them makes the answer internally inconsistent:\nstatements:\n%s",
			strings.Join(recorder.seen(), "\n---\n"))
	}
	for i, level := range levels {
		if level != "repeatable read" {
			t.Errorf("unit of work %d began at %s, want repeatable read: the read's five statements answer one question, and at read committed a settlement committing between the series read and the money read yields a bucket that says settled beside a settled amount that excludes it", i+1, level)
		}
	}
}

// boundRecorder is a driver that records the statement text of everything run
// against it and answers every query with no rows.
//
// It is deliberately the smallest possible driver. The read is under
// observation, not under test: the assertions are about the statements it
// issues, and a driver that tried to answer them would be re-implementing the
// adapter's own scanning to find out whether the adapter scans correctly —
// which the adapter's own tests, against a real database, already do.
type boundRecorder struct {
	mu         sync.Mutex
	statements []string
	// begun records what each transaction was started with. The isolation a
	// unit of work runs at is carried on the BEGIN and on nothing else, so a
	// recorder that kept only statement text would have nothing to assert the
	// read's single-snapshot claim against.
	begun []driver.TxOptions
}

// isolations returns the level every unit of work was begun at, in order,
// spelled rather than numbered: a failure message reading "began at 0" poses a
// question the reader cannot answer, and the whole point of this assertion is
// that the answer is a word.
func (r *boundRecorder) isolations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.begun))
	for _, opts := range r.begun {
		switch opts.Isolation {
		case driver.IsolationLevel(0):
			out = append(out, "default")
		case driver.IsolationLevel(sql.LevelReadCommitted):
			out = append(out, "read committed")
		case driver.IsolationLevel(sql.LevelRepeatableRead):
			out = append(out, "repeatable read")
		case driver.IsolationLevel(sql.LevelSerializable):
			out = append(out, "serializable")
		default:
			out = append(out, fmt.Sprintf("level %d", opts.Isolation))
		}
	}
	return out
}

func (r *boundRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.statements...)
}

// record keeps the statement TEXT and drops the arguments, which is the whole
// of what this recorder is for: every assertion in this file is about which
// statements the read issues and in what order, and a driver that collected the
// arguments would be keeping values nothing here reads.
func (r *boundRecorder) record(text string, _ []driver.NamedValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, text)
}

func (r *boundRecorder) Open(string) (driver.Conn, error) { return &boundConn{recorder: r}, nil }

type boundConn struct{ recorder *boundRecorder }

func (c *boundConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }

func (c *boundConn) Begin() (driver.Tx, error) { return boundTx{}, nil }

// BeginTx records the options and begins at the DEFAULT isolation regardless
// of what it was handed, which is what a driver that does not implement
// non-default levels does. The recording is the part under test: the point is
// to see what the adapter ASKED for, not to model a server that would grant it.
func (c *boundConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.recorder.mu.Lock()
	c.recorder.begun = append(c.recorder.begun, opts)
	c.recorder.mu.Unlock()
	return c.Begin()
}

func (c *boundConn) Close() error { return nil }

func (c *boundConn) Ping(context.Context) error { return nil }

func (c *boundConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.recorder.record(query, args)
	return &boundRows{}, nil
}

func (c *boundConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.recorder.record(query, args)
	return driver.RowsAffected(0), nil
}

type boundTx struct{}

func (boundTx) Commit() error   { return nil }
func (boundTx) Rollback() error { return nil }

// boundRows is zero columns and no rows, so every scan the read attempts
// fails and the read returns. The bound is issued before the first read, so
// by the time a scan fails the SET LOCAL has already been recorded — which is
// the ordering the test relies on and the reason the driver can be this empty.
type boundRows struct{}

func (boundRows) Columns() []string { return nil }

func (boundRows) Close() error { return nil }

func (boundRows) Next([]driver.Value) error { return io.EOF }
