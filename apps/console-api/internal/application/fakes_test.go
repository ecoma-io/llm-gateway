package application

import (
	"context"
	"errors"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The fakes the replay tests are written against, and the reason they are one
// world rather than three independent mocks.
//
// The property under test is a relationship between three ports — the applier
// and the cursor move together inside a unit of work the store opens, and the
// reader is what is read outside it — so a fake that could not see the other
// two would make the assertions about ordering and rollback impossible to
// state. fakeWorld is that shared state: durable-ish state (the position, the
// applied facts) that the store can roll back, and an observation log (order)
// that records what happened even when the transaction does not survive. The
// two are deliberately different: a rollback restores the state and leaves the
// record of the attempt, because what the tests need to see is both.
//
// The store embeds persistence.Store instead of implementing its full query
// surface. That is not a shortcut — the arch rules keep database/sql out of the
// application package, and the flow under test never runs a query, so a fake
// Querier here would have to name *sql.Rows to satisfy the port and would drag
// the database into this package's test build. Embedding keeps the port
// satisfied, keeps the import out, and is honest about which half of the store
// this use case uses.
type fakeWorld struct {
	// order is the observation log: "begin", "apply:<request_id>",
	// "advance:<cursor>", "commit", "rollback", in the order they happened.
	order []string

	// position is the Control Plane's durable position, and effects is the
	// applier's durable state (the request_ids whose effect was recorded, with
	// the fact that produced it). Both are what a rollback restores.
	position string
	effects  map[string]persistence.Fact

	// outsideTx counts port calls that arrived without the transaction's
	// context. It is the mechanical check that the applier and the cursor ran
	// inside the store's unit of work rather than beside it.
	outsideTx int

	// The knobs each test sets.
	pages       map[string]dataplane.Page
	readErr     error
	positionErr error
	failOn      map[string]error
	advanceErr  error

	// What the reader was asked for.
	reads  []string
	limits []int
}

func newWorld() *fakeWorld {
	return &fakeWorld{
		effects: map[string]persistence.Fact{},
		pages:   map[string]dataplane.Page{},
		failOn:  map[string]error{},
	}
}

// newIngestion wires the use case over one world. Every test builds its fakes
// this way, so the four ports always describe the same flow.
func newIngestion(world *fakeWorld) *FactIngestion {
	return NewFactIngestion(
		fakeUsageFacts{world: world},
		fakeStore{world: world},
		fakeCursor{world: world},
		fakeApplier{world: world},
	)
}

// rewind returns the position to "never applied anything", simulating the
// Control Plane reading a range it has already read. That is the feed's normal
// state — the same range may be requested any number of times — and it is what
// makes the idempotency of a replayed fact observable from outside the applier.
func (w *fakeWorld) rewind() {
	w.position = ""
}

func (w *fakeWorld) record(entry string) {
	w.order = append(w.order, entry)
}

// fakeUsageFacts is the fake Data Plane: it answers from a fixed set of pages
// keyed by the position it was asked for, and records every position it was
// asked about, in order.
type fakeUsageFacts struct {
	world *fakeWorld
}

func (f fakeUsageFacts) ReadUsageEvents(_ context.Context, after string, limit int) (dataplane.Page, error) {
	f.world.reads = append(f.world.reads, after)
	f.world.limits = append(f.world.limits, limit)
	if f.world.readErr != nil {
		return dataplane.Page{}, f.world.readErr
	}
	page, known := f.world.pages[after]
	if !known {
		// A position with nothing after it: the Data Plane answers with the
		// position the request carried, which is what the contract says an
		// empty page looks like.
		return dataplane.Page{NextCursor: after}, nil
	}
	return page, nil
}

// fakeCursor is the Control Plane's own position. Its Position is where a test
// starts, and its Advance is what the unit of work carries.
type fakeCursor struct {
	world *fakeWorld
}

func (c fakeCursor) Position(_ context.Context) (string, error) {
	if c.world.positionErr != nil {
		return "", c.world.positionErr
	}
	return c.world.position, nil
}

func (c fakeCursor) Advance(ctx context.Context, next string) error {
	if !inTransaction(ctx) {
		c.world.outsideTx++
	}
	c.world.record("advance:" + next)
	if c.world.advanceErr != nil {
		return c.world.advanceErr
	}
	c.world.position = next
	return nil
}

// fakeApplier is an idempotent applier: the first delivery of a request_id
// records the effect, and every later delivery of the same request_id is a
// no-op that still records the attempt. It fails for the request_ids a test
// names, and the failure is what the unit of work rolls back.
type fakeApplier struct {
	world *fakeWorld
}

func (a fakeApplier) Apply(ctx context.Context, fact persistence.Fact) error {
	if !inTransaction(ctx) {
		a.world.outsideTx++
	}
	a.world.record("apply:" + fact.RequestID)
	if err := a.world.failOn[fact.RequestID]; err != nil {
		return err
	}
	if _, applied := a.world.effects[fact.RequestID]; applied {
		// Already settled, consumed or released for this request_id. The
		// second delivery changes nothing, which is the contract FactApplier
		// states and the reason redelivery costs nothing.
		return nil
	}
	a.world.effects[fact.RequestID] = fact
	return nil
}

// fakeStore is the unit of work. It is a real one in miniature: it snapshots
// the durable state at begin, restores it when the callback returns an error,
// and hands the callback a context carrying the marker the other fakes use to
// prove they were given the transaction's context and not the one the call
// arrived on.
type fakeStore struct {
	persistence.Store
	world *fakeWorld
}

func (s fakeStore) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	s.world.record("begin")

	position := s.world.position
	effects := make(map[string]persistence.Fact, len(s.world.effects))
	for requestID, fact := range s.world.effects {
		effects[requestID] = fact
	}

	if err := fn(context.WithValue(ctx, txMarkerKey{}, true)); err != nil {
		s.world.position = position
		s.world.effects = effects
		s.world.record("rollback")
		return err
	}
	s.world.record("commit")
	return nil
}

// txMarkerKey marks the context a unit of work handed its callback. It is
// unexported and only fakeStore puts it there, so a call arriving without it
// can only mean the code under test reached a port with the wrong context —
// which is a real defect, not a test artifact: a store resolves its query
// surface from the context it is given, so a port called with the outer
// context writes outside the transaction that is supposed to cover it.
type txMarkerKey struct{}

func inTransaction(ctx context.Context) bool {
	marked, ok := ctx.Value(txMarkerKey{}).(bool)
	return ok && marked
}

// InUnitOfWork answers the same lookup WithinTx marks with — the fake of the
// port member the ledger-side repositories ask before refusing to append
// outside a unit of work.
func (s fakeStore) InUnitOfWork(ctx context.Context) bool {
	return inTransaction(ctx)
}

// errApply is the applier failure the rollback test injects. It is a plain
// error rather than an application one because the applier's failures belong to
// the port, and the use case's job is to propagate them rather than to
// translate them.
var errApply = errors.New("the applier refused the fact")

// errPosition is the cursor failure the position test injects.
var errPosition = errors.New("the cursor could not be read")

// errAdvance is the failure a cursor write can report on its own, after every
// fact in the page has already been applied.
var errAdvance = errors.New("the cursor could not be advanced")
