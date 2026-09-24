package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// This file is a hand-written database/sql driver, in test code, because the
// adapter's contract is orchestration — begin, commit, roll back, join — and
// the only honest way to pin orchestration without a live PostgreSQL is to
// watch the driver calls it makes. A real database proves the other half, in
// deploy/postgres's suite. It records events, fails on demand, and refuses
// everything else: a Prepare or query reaching this driver means the adapter
// grew a surface this test did not agree to.

// event names one observable driver call.
type event string

const (
	evConnect  event = "connect"
	evPing     event = "ping"
	evBegin    event = "begin"
	evCommit   event = "commit"
	evRollback event = "rollback"
	evExec     event = "exec"
	evClose    event = "close"
)

// fakeDriver records every call into it. There is no per-connection
// intelligence: the pool may open and close connections as it pleases, so
// assertions read the recorded stream rather than any one connection's state.
type fakeDriver struct {
	mu     sync.Mutex
	events []event
	fail   map[event]error
}

// newFake registers a fresh fake driver under a unique name and returns the
// driver with the name it registered under. sql.Register cannot unregister,
// so one driver per name per test is the price of isolation; names come from
// a counter, never reused. The name is returned because open — the
// orchestration under Open — takes the driver name it hands sql.Open, and
// the tests that drive it need the fake's name rather than the real one.
func newFake(t *testing.T) (*fakeDriver, string) {
	t.Helper()

	registerMu.Lock()
	registered++
	name := fmt.Sprintf("fake-postgres-%d", registered)
	registerMu.Unlock()

	f := &fakeDriver{fail: make(map[event]error)}
	sql.Register(name, f)
	return f, name
}

// registerFake puts a fresh fake driver behind a unique name and opens a pool
// on it.
func registerFake(t *testing.T) (*fakeDriver, *sql.DB) {
	t.Helper()

	f, name := newFake(t)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("sql.Open on the fake driver: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return f, db
}

var (
	registerMu sync.Mutex
	registered int
)

// failOn makes the next recorded call of e return err instead.
func (f *fakeDriver) failOn(e event, err error) {
	f.mu.Lock()
	f.fail[e] = err
	f.mu.Unlock()
}

// recorded returns a copy of the events so far.
func (f *fakeDriver) recorded() []event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]event(nil), f.events...)
}

// of filters the stream down to the events an assertion cares about — the
// pool's connect/close bookkeeping is not the adapter's contract.
func of(events []event, want ...event) []event {
	var out []event
	for _, e := range events {
		for _, w := range want {
			if e == w {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// outcome returns err for e if one was injected, else nil.
func (f *fakeDriver) outcome(e event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail[e]
}

func (f *fakeDriver) record(e event) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *fakeDriver) Open(string) (driver.Conn, error) {
	f.record(evConnect)
	return &fakeConn{f: f}, nil
}

// fakeConn implements exactly the legacy driver surface database/sql needs to
// pool, ping and transact. Prepare answers with an error because no test in
// this package makes the adapter run statements — the day one does, this is
// the loud no rather than a green test over an unimplemented stub.
type fakeConn struct{ f *fakeDriver }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake driver: statements are not part of this contract")
}

// ExecContext implements driver.ExecerContext, which database/sql consults
// before Prepare for a plain ExecContext — the one path both the pool and
// the transaction take. That makes exec the observable that says which
// handle a resolved Querier answered with: an exec through the transaction
// lands on the connection the transaction holds; an exec through the pool
// finds no idle connection and opens a second one. The event stream, not
// any pointer identity, is the proof.
func (c *fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	if err := c.f.outcome(evExec); err != nil {
		return nil, err
	}
	c.f.record(evExec)
	return driver.RowsAffected(0), nil
}

// Close is recorded, because who closes a connection is the one observable
// that says a pool was torn down: open closes its half-built pool on failure,
// and the close event is how the tests see that promise kept.
func (c *fakeConn) Close() error {
	c.f.record(evClose)
	return nil
}

func (c *fakeConn) Begin() (driver.Tx, error) {
	if err := c.f.outcome(evBegin); err != nil {
		return nil, err
	}
	c.f.record(evBegin)
	return fakeTx{f: c.f}, nil
}

// Ping implements driver.Pinger, which is what database/sql consults for
// PingContext — the call persistence.Store.Ping eventually rests on.
func (c *fakeConn) Ping(context.Context) error {
	if err := c.f.outcome(evPing); err != nil {
		return err
	}
	c.f.record(evPing)
	return nil
}

// fakeTx is every transaction the driver hands out; commit and rollback are
// the only two things that can happen to one.
type fakeTx struct{ f *fakeDriver }

func (t fakeTx) Commit() error {
	if err := t.f.outcome(evCommit); err != nil {
		return err
	}
	t.f.record(evCommit)
	return nil
}

func (t fakeTx) Rollback() error {
	if err := t.f.outcome(evRollback); err != nil {
		return err
	}
	t.f.record(evRollback)
	return nil
}
