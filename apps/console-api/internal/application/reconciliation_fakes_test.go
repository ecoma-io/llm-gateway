package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/ingestion"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The reconciliation fakes, and the reason they are one world rather than ten
// independent mocks.
//
// The property under test is a pass over a whole plane, so almost every
// assertion is a NEGATIVE one: the finding that must not open, the money port
// that must not be touched, the write that must not happen. A negative
// assertion over ten separately-scribbled doubles is a claim about the doubles
// as much as about the code, and the specific way it goes wrong is a fake that
// was never wired to fail — it answers, so the code under it is never asked
// the question. One world fixes that by making "nothing happened" something
// the world can assert about ITSELF: every money-bearing port here refuses the
// call the pass must not make, so a false positive is a loud failure rather
// than a silent one that happens to agree with the test.
//
// The second property is ORDER — the pass opens its run row before it sweeps
// and closes it after, and a failure between the two leaves it open — and that
// is a relationship between the world and its observation log. So the world
// keeps a `order` of every port entry the way the accounting and commerce
// worlds do, and the sweep order F1 → the windowed families → F6 is legible in
// it.
//
// The ledgers are faithful rather than stubbed, and the fidelity is
// deliberately narrow. The findings fake runs Open's contract — the partial
// unique index over status = 'open' is the arbiter, a second Open for a taken
// key creates nothing and only touches last_seen_at — because the whole
// idempotency bargain is a claim about what that method does, and a fake that
// returned a scripted `created` would let the pass be tested against a
// behaviour it does not have. The bucket fake runs the keyset walk for the
// same reason: pagination is a claim about the cursor it passes and a fake
// that ignored `after` would pass a test that a real adapter fails.

// reconWorld is the shared state the reconciliation fakes answer from, in the
// same spirit as the accounting, commerce and replay worlds: durable-ish state
// the store could roll back, plus an observation log that records what
// happened even when the transaction does not survive.
type reconWorld struct {
	// order is the observation log: "run.begin", "bucket.sweep:<after>",
	// "applied.recent", "finding.open:<check>/<subject>", "run.finish", in the
	// order they happened. It is the only way to say anything about WHEN, since
	// the pass's own summary carries counts and no sequence.
	order []string

	// The state each port owns. The findings map is keyed by the port's own
	// identity — (check, subject kind, subject id) and nothing else — because
	// that is the identity Open converges on, and a fake keyed by anything
	// else would report a created finding for a key the real table already
	// holds.
	findings map[findingKey]*reconFindingRow

	// buckets are the rows the keyset sweep walks, and applied the applied-fact
	// ledger the windowed families read, sorted by their own port's keyset so
	// a test can seed them in any order and still get a stable walk.
	buckets []accounting.Bucket
	applied []appliedRow

	// appliedReads is every applied-facts read the pass made, in order, with
	// the half-open window and the limit it was bounded by. It exists because a
	// run row that records a window the sweep did not use is a row that is a
	// claim about work nobody did — and the only way to see that is to read the
	// port and not only the run history.
	appliedReads []windowedRead

	// findScripts answers a Find call from a script rather than from the
	// ledger, one answer per call with the last repeating. It is how the
	// disposition conflicts' confirming reads are staged: the state those
	// reads exist to catch is a row that changed between the two, and a
	// ledger that answers the same thing twice can never produce it.
	findScripts map[findKey][]findAnswer

	// findReads counts the Find calls per pair, so a script that answers
	// differently on the second read — the state the confirming reads exist to
	// catch — can be told from one that answered the same thing twice.
	findReads map[findKey]int

	// The ledger the pass reads through: a bucket's cached side, a
	// settlement's own legs, and the scripted answers that stage a race.
	ledger *reconLedger

	// runs is the pass history, which is also the high-water mark the next
	// window opens at.
	runs []persistence.Run

	// position is the ingestion cursor's own durable value. The empty string
	// is this port's convention for "never applied anything" and F6's whole
	// subject.
	position string

	now time.Time

	// failOn refuses one port member by key ("ledger.bucket",
	// "runs.begin", "findings.open", "applied.recent", ...), the way a
	// persistence failure arrives from below. The named member is the one
	// that fails, so a test can plant a failure in exactly the family whose
	// propagation it is watching.
	failOn map[string]error

	// pageSize is the batch the sweep pages at. It is separate from the
	// settings' Batch because a test that wanted a two-page walk and the
	// pass's own batch are different assertions: the sweep reads the batch it
	// was configured with, and the walk is checked against THAT.
	pageSize int

	// recentFailFrom is the applied-facts read the cursor's own existence
	// check makes, failing from the Nth call onward — the SECOND call in any
	// pass that has both a window to sweep and an empty position to ask
	// about, and the FIRST in a pass whose position is written and whose
	// window is not read. It is separate from failOn["applied.recent"]
	// because that one fails every read, and F6's read happens second.
	recentFailFrom int

	// finishBy overrides the status Finish's compare-and-set is made against,
	// the way a run row the pass did not begin — or one another worker closed
	// — would present itself. Empty means the honest default, which is the
	// status the row is actually in.
	finishBy string

	// cancelBeforeFinish fires the caller's cancel func from the pass's last
	// read, which is how a test stages a stop signal landing between the sweep
	// and the close. It is nil on a world that is not testing shutdown, and it
	// clears itself after firing once so a pass cannot be cancelled twice by
	// one hook.
	cancelBeforeFinish func()

	// writes counts every call that would move money or rewrite a fact. A
	// pass that finds a divergence and touches one of these is the whole
	// design's claim broken, and it is asserted by counter rather than by
	// eyeballing a trace — a fake that answered the call would have let the
	// pass "fix" the bucket and the test would still have looked clean.
	writes []string
}

// reconFindingRow is one open finding and the state Open maintains on it:
// when it was first detected, when it was last seen, and the evidence it
// currently holds. A re-Open replaces the evidence and moves last_seen_at
// alone, exactly as the table's own statement does.
type reconFindingRow struct {
	check      string
	subject    string
	severity   string
	observed   []byte
	detail     string
	detectedAt time.Time
	lastSeenAt time.Time

	// confirmations counts every Open against this key, which is how a test
	// tells "re-confirmed" from "never opened" without keeping its own log.
	confirmations int
}

// appliedRow is one applied-fact row. The instant the windowed read orders and
// filters on is the port's own AppliedAt — the ledger's applied_at, which the
// port names as this plane's clock and not the fact's occurred_at — so the row
// carries it as a field rather than beside the struct, and a fake that held two
// copies of the same instant could page a window by one and filter by the
// other.
type appliedRow struct {
	persistence.AppliedFact
}

// findingKey is a finding's identity as the table states it: the check kind,
// the subject kind and the subject id, and nothing else. A finding whose key
// carried a timestamp, a run id or a message would be a new row every pass,
// which is the exact failure the identity exists to prevent.
type findingKey struct {
	check       string
	subjectKind string
	subjectID   string
}

// findKey is the pair the applied ledger is keyed on — (request id, kind
// class) — which is the same identity the applier's exactly-once boundary
// turns on, so a row the pass looks up by class is a row the applier filed.
type findKey struct {
	requestID string
	kindClass string
}

// findAnswer is one scripted answer to a Find: a row, or nil for "this class
// has nothing applied", or a failure. All three are needed and they are three
// different states rather than a boolean, because the disposition checks ask
// three different questions of the same read — does the class exist, does it
// hold the kind I expect, and is it still that kind a second time.
type findAnswer struct {
	fact *persistence.AppliedFact
	err  error
}

// The errors a scripted pass can plant. They are plain sentinels so the tests
// can prove the pass wraps its cause with errors.Is rather than merely
// mentioning it — a reconciliation pass that reports "the sweep failed" with
// nothing attached is a bug report nobody can act on.
var (
	errReconClock   = errors.New("fake: the database clock could not be read")
	errReconLatest  = errors.New("fake: the high-water mark could not be read")
	errReconBegin   = errors.New("fake: the run row could not be opened")
	errReconFinish  = errors.New("fake: the run row could not be finished")
	errReconSweep   = errors.New("fake: the bucket sweep could not be read")
	errReconBucket  = errors.New("fake: a bucket could not be reconciled")
	errReconRecent  = errors.New("fake: the applied-facts window could not be read")
	errReconFind    = errors.New("fake: the applied ledger could not be looked up")
	errReconSettle  = errors.New("fake: a settlement's own legs could not be read")
	errReconOpen    = errors.New("fake: a finding could not be opened")
	errReconCounter = errors.New("fake: the open findings could not be counted")
	errReconCursor  = errors.New("fake: the ingestion position could not be read")
)

// errReconWritesRefused is the refusal every money-bearing port in this file
// answers. The pass holds ports that CAN write — settlements, buckets, the
// applied ledger, the quarantine, the cursor — and the claim that it never
// does is a claim about code, so the fakes make the claim's violation loud
// rather than silent: a sweep that found a bucket drift and then settled,
// closed, recorded or quarantined anything lands here and fails the test that
// asserted the pass only reads.
var errReconWritesRefused = errors.New("fake: the reconciliation pass reached a write port it was promised it would not")

func newReconWorld(t *testing.T) *reconWorld {
	t.Helper()
	world := &reconWorld{
		findings:    map[findingKey]*reconFindingRow{},
		failOn:      map[string]error{},
		findScripts: map[findKey][]findAnswer{},
		findReads:   map[findKey]int{},
		ledger:      &reconLedger{},
		now:         time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC),
		pageSize:    500,
	}
	world.ledger.world = world
	world.ledger.reset()
	return world
}

// record appends one entry to the observation log.
func (w *reconWorld) record(entry string) {
	w.order = append(w.order, entry)
}

// write records a call the pass must never make. It is the counter every
// false-positive test asserts on, and it is counted here rather than in each
// fake so that a port added later inherits the guard without being asked.
func (w *reconWorld) write(entry string) {
	w.writes = append(w.writes, entry)
}

// seedBucket files one bucket for the keyset sweep to walk, with a drifted
// cache: the bucket says one thing and the ledger it stands for says another.
// drift is the amount the cached balances are off by, which is how a test says
// "the cache says 100 and the legs derive 100+drift" without writing two
// sets of figures.
func (w *reconWorld) seedBucket(id accounting.FundingBucketID, settled, drift int64) {
	w.buckets = append(w.buckets, accounting.Bucket{
		ID:        id,
		Status:    accounting.BucketActive,
		Settled:   accounting.Balance(settled),
		Available: accounting.Balance(settled),
		CreatedAt: w.now,
		UpdatedAt: w.now,
	})
	w.ledger.buckets[id] = reconBucketSide{
		// The derivation is the cached figure plus the drift, which is what
		// makes the two disagree by exactly the amount a test named rather
		// than by whatever the fake happened to produce.
		cached: accounting.Bucket{
			ID:        id,
			Status:    accounting.BucketActive,
			Settled:   accounting.Balance(settled),
			Available: accounting.Balance(settled),
		},
		derivation: accounting.Derivation{
			Settled:   accounting.Balance(settled + drift),
			Available: accounting.Balance(settled + drift),
			Legs:      2,
		},
	}
}

// seedHealthyBucket files one bucket whose cache and derivation agree, which
// is the state almost every bucket on a running plane is in and therefore the
// state a false positive would land on.
func (w *reconWorld) seedHealthyBucket(id accounting.FundingBucketID, settled int64) {
	w.seedBucket(id, settled, 0)
}

// seedSettlement files one settlement's own legs: the header, the multiset the
// shape rule is checked against, and the two bucket counts it is checked with.
// This is the whole of a SettlementLedger, and the test mints the shapes it
// wants rather than scripting a verdict, because the verdict belongs to the
// code under test and a fake that returned one would be the code.
func (w *reconWorld) seedSettlement(ledger persistence.SettlementLedger) {
	w.ledger.settlements[ledger.Settlement.ID] = ledger
}

// seedApplied files one applied fact at the ledger's own recorded instant,
// which is the clock the windowed read filters on. A fact whose class is left
// empty is filed under the class the contract derives from its kind, because
// the pass's Find asks by class and a fact filed under nothing would be
// invisible to the very lookup the check makes.
func (w *reconWorld) seedApplied(applied persistence.AppliedFact, at time.Time) {
	if applied.KindClass == "" {
		applied.KindClass = kindClass(applied.Kind)
	}
	applied.AppliedAt = at
	w.applied = append(w.applied, appliedRow{AppliedFact: applied})
}

// findingsOpened is how many findings this pass CREATED, read back from the
// world's own table rather than from the summary the pass returned. The two
// agreeing is part of the assertion: a pass that counted a finding it did not
// write, or wrote one it did not count, is a pass whose headline is a guess.
func (w *reconWorld) findingsOpened() int {
	opened := 0
	for _, row := range w.findings {
		if row.confirmations > 0 {
			opened++
		}
	}
	return opened
}

// theFinding is how a test names the row it expects, so a failure reads as
// "the pass opened X" rather than as a comparison of struct literals.
func (w *reconWorld) theFinding(check, subjectKind, subjectID string) (*reconFindingRow, bool) {
	row, ok := w.findings[findingKey{check: check, subjectKind: subjectKind, subjectID: subjectID}]
	return row, ok
}

// checks is every check kind currently in the findings table, sorted. A
// healthy pass's assertion is that this is EMPTY, and a failing pass's is that
// it holds exactly the keys the test named.
func (w *reconWorld) checks() []string {
	seen := map[string]bool{}
	for key := range w.findings {
		seen[key.check] = true
	}
	out := make([]string, 0, len(seen))
	for check := range seen {
		out = append(out, check)
	}
	sort.Strings(out)
	return out
}

// reconPorts is the constructor's argument list as one value, in the shape
// commercePorts established: the nil-port test nils one port at a time without
// repeating eight arguments, and every other test builds its worker from the
// same one function so the fakes are always wired identically.
//
// It carries SEVEN ports and the worker reaches every one of them, which is
// the shape the constructor's own comment is about. It used to carry ten, of
// which the worker reached seven: the store, the settlements repository and
// the quarantine were held and read nowhere. Their fakes live on in this file
// but are no longer wired here — see each of them for why the refusal counter
// outlasts the port.
type reconPorts struct {
	buckets  persistence.FundingBuckets
	applied  persistence.AppliedFacts
	cursor   persistence.IngestionCursor
	findings persistence.ReconciliationFindings
	runs     persistence.ReconciliationRuns
	ledger   LedgerReader
	clock    persistence.Clock
}

func reconPortsFor(w *reconWorld) reconPorts {
	return reconPorts{
		buckets:  reconBuckets{world: w},
		applied:  reconApplied{world: w},
		cursor:   reconCursor{world: w},
		findings: reconFindings{world: w},
		runs:     reconRuns{world: w},
		ledger:   w.ledger,
		clock:    reconClock{world: w},
	}
}

// newReconciliation wires the worker over one world. Every reconciliation test
// builds it this way.
func newReconciliation(w *reconWorld) *Reconciliation {
	return newReconciliationWith(w, DefaultReconciliationSettings())
}

// newReconciliationWith is the same wiring with the settings a test wants, so
// the pacing tests can page at two and the rest at the default without either
// of them hand-rolling a constructor call. The port values are the world's own
// fakes, one per argument, and the worker reaches every port it is handed.
func newReconciliationWith(w *reconWorld, settings ReconciliationSettings) *Reconciliation {
	ports := reconPortsFor(w)
	return NewReconciliation(
		ports.buckets,
		ports.applied,
		ports.cursor,
		ports.findings,
		ports.runs,
		ports.ledger,
		ports.clock,
		settings,
	)
}

// ---------------------------------------------------------------------------
// The world as the store, the repositories and the clock
// ---------------------------------------------------------------------------

// The pass holds seven ports, and a fake per port would be seven fakes that
// cannot see each other. This file's answer is one type that IS all of them, in
// the same spirit as the trading world's fakes: every method is one of the
// ports' members and each one records what it was asked. The struct does not
// embed the port interfaces — the constructor takes them one at a time, and
// Go's implicit interface satisfaction is what lets a single value stand in
// for eight arguments without a nil embedded interface hiding a missing method.

// recordWindow is how an applied-facts read enters the window log the tiling
// assertions read: the half-open window and the limit it was bounded by. The
// window bounds WHICH rows are read; the batch bounds HOW MANY per statement,
// and a test that pinned only one of them would leave the other unstated.
func (w *reconWorld) recordWindow(from, to time.Time, limit int) {
	w.appliedReads = append(w.appliedReads, windowedRead{from: from, to: to, limit: limit})
}

// windowedRead is one applied-facts read: the half-open window and the limit
// it was bounded by.
type windowedRead struct {
	from  time.Time
	to    time.Time
	limit int
}

// reconStore is the unit of work, and it is NOT WIRED. The constructor does
// not take it any more, and the pass never reached for it while the
// constructor did: a sweep that opened a transaction would be a long-lived
// unit of work holding row locks for a window's population, which is the
// decision the pass's own comment refuses.
//
// It is kept here rather than deleted because the refusal is a recorded call
// rather than a nil panic, and a test should be able to say "the pass opened no
// transaction" and have that be an observation. Nothing constructs it, so
// nothing counts it: the guard a future check would have to remove is the
// compiler, not this counter. A check that needed a unit of work could not
// pass one to the worker at all, which is the outcome worth having.
//
// persistence.Store is embedded for the members the pass never calls — Querier,
// Pinger, WithinTx, InUnitOfWork — the same honest shortcut the other worlds
// take: the arch rules keep database/sql out of this package, so a fake
// Querier here would have to name *sql.Rows to satisfy the port and would drag
// the database into the application package's test build.
type reconStore struct {
	persistence.Store
	world *reconWorld
}

func (s reconStore) WithinTx(_ context.Context, _ func(ctx context.Context) error) error {
	s.world.write("store.within-tx")
	return errReconWritesRefused
}

func (s reconStore) InUnitOfWork(_ context.Context) bool { return false }

func (s reconStore) Querier(context.Context) persistence.Querier {
	s.world.write("store.querier")
	return nil
}

// reconBuckets is the funding-bucket repository. Sweep is the real keyset
// walk — strictly-greater-than on the id, ascending, a short page as the end
// of the table and never an error — because pagination is a claim about the
// cursor the pass threads, and a fake that ignored `after` would pass a test a
// real adapter fails. Every other member REFUSES, and counts the call: the
// pass's whole claim about money is that it never writes here, and the only
// way that claim is tested rather than assumed is for the write to be loud.
type reconBuckets struct {
	persistence.FundingBuckets
	world *reconWorld
}

func (b reconBuckets) Sweep(_ context.Context, after accounting.FundingBucketID, limit int) ([]accounting.Bucket, error) {
	w := b.world
	w.record("bucket.sweep:" + string(after))
	if err := w.failOn["buckets.sweep"]; err != nil {
		return nil, err
	}
	// The world's rows are sorted by id once, so the walk is the same
	// regardless of the order a test seeded them in — and a real uuid v7
	// column is in id order too.
	page := make([]accounting.Bucket, 0, limit)
	for _, bucket := range w.buckets {
		if string(bucket.ID) <= string(after) {
			continue
		}
		page = append(page, bucket)
		if len(page) == limit {
			break
		}
	}
	return page, nil
}

func (b reconBuckets) Create(context.Context, accounting.Bucket) error {
	b.world.write("buckets.create")
	return errReconWritesRefused
}

func (b reconBuckets) ByID(context.Context, accounting.FundingBucketID) (accounting.Bucket, error) {
	b.world.write("buckets.by-id")
	return accounting.Bucket{}, errReconWritesRefused
}

func (b reconBuckets) ByEntitlementID(context.Context, accounting.EntitlementID) (accounting.Bucket, error) {
	b.world.write("buckets.by-entitlement")
	return accounting.Bucket{}, errReconWritesRefused
}

func (b reconBuckets) ByAccountID(context.Context, accounting.AccountID) (accounting.Bucket, error) {
	b.world.write("buckets.by-account")
	return accounting.Bucket{}, errReconWritesRefused
}

func (b reconBuckets) Close(context.Context, accounting.FundingBucketID, int64, time.Time) (bool, error) {
	b.world.write("buckets.close")
	return false, errReconWritesRefused
}

// reconSettlements is the settlements repository, and it is NOT WIRED. Every
// member refuses, because the pass reaches settlements only through its
// LedgerReader and a fake that answered here would let a test's "no settlement
// was created" assertion pass on a pass that had created one.
//
// It is kept for the counting, and the counting is now a statement about the
// future rather than about this pass: the constructor no longer takes the
// repository, so a check that reached for one would have nothing to hold and
// the build would fail. That is the stronger version of the guard the
// refusals used to be — the compiler refuses it rather than a test noticing.
type reconSettlements struct {
	persistence.Settlements
	world *reconWorld
}

func (s reconSettlements) Create(context.Context, accounting.Settlement) (bool, error) {
	s.world.write("settlements.create")
	return false, errReconWritesRefused
}

func (s reconSettlements) ByRequestID(context.Context, accounting.RequestID) (accounting.Settlement, error) {
	s.world.write("settlements.by-request")
	return accounting.Settlement{}, errReconWritesRefused
}

func (s reconSettlements) Ledger(context.Context, accounting.SettlementID) (persistence.SettlementLedger, error) {
	s.world.write("settlements.ledger")
	return persistence.SettlementLedger{}, errReconWritesRefused
}

// reconApplied is the idempotency ledger, and it is the one repository the
// pass reads heavily — Find for the disposition conflict, Recent for the
// windowed families and for the cursor's own existence — and never writes.
// Find and Recent run the real filters: Find is the exact pair lookup, Recent
// is the half-open window over the ledger's OWN recorded instant, oldest
// first, bounded by the limit, with a short page as the ordinary answer and
// never an error. Recent also takes the keyset's open bound, because the pass
// pages its window and a fake that ignored it would be a fake that could pass
// a read which skipped rows.
//
// The window is over applied_at and not over the fact, because that is the
// port's stated rule and it is load-bearing for the tiling assertion: two
// consecutive passes must see each row exactly once, and a fake that
// filtered on some other instant would make that true for the wrong reason.
type reconApplied struct {
	persistence.AppliedFacts
	world *reconWorld
}

func (a reconApplied) Find(_ context.Context, requestID, kindClass string) (*persistence.AppliedFact, error) {
	w := a.world
	if err := w.failOn["applied.find"]; err != nil {
		return nil, err
	}
	key := findKey{requestID: requestID, kindClass: kindClass}
	if script, ok := w.findScripts[key]; ok && len(script) > 0 {
		read := w.findReads[key]
		w.findReads[key]++
		if read >= len(script) {
			read = len(script) - 1
		}
		return script[read].fact, script[read].err
	}
	for _, row := range w.applied {
		if row.RequestID == requestID && row.KindClass == kindClass {
			found := row.AppliedFact
			return &found, nil
		}
	}
	return nil, nil
}

func (a reconApplied) Recent(_ context.Context, from, to, after time.Time, afterID string, limit int) ([]persistence.AppliedFact, error) {
	w := a.world
	read := countOrder(w.order, "applied.recent")
	w.record("applied.recent")
	w.recordWindow(from, to, limit)
	if read >= w.recentFailFrom {
		if err := w.failOn["applied.recent"]; err != nil {
			return nil, err
		}
	}
	// The window and the keyset are the adapter's two bounds and they are not
	// the same question: [from, to) is which window this is, and the pair
	// (applied_at, request_id) > (after, afterID) is which page of it.
	//
	// The keyset is a PAIR here, and a fake that compared the instant alone
	// would be a fake that could pass a read which silently skipped every row
	// sharing an instant — which is most of a window, because a whole
	// ingestion page commits in one transaction and its rows all share one
	// applied_at. So the fake compares the pair, the way the adapter does.
	//
	// The rows are already in insertion order, and the tests seed them oldest
	// first, so the order the read promises is the order they are in. The
	// recorded instant rides on the row now, which is what makes the next
	// page's resume point available to the caller rather than something the
	// caller has to guess.
	page := make([]persistence.AppliedFact, 0, limit)
	for _, row := range w.applied {
		if row.AppliedAt.Before(from) || !row.AppliedAt.Before(to) {
			continue
		}
		if row.AppliedAt.Before(after) ||
			(row.AppliedAt.Equal(after) && row.RequestID <= afterID) {
			continue
		}
		page = append(page, row.AppliedFact)
		if len(page) == limit {
			break
		}
	}
	return page, nil
}

func (a reconApplied) Record(context.Context, persistence.AppliedFact) error {
	a.world.write("applied.record")
	return errReconWritesRefused
}

// reconQuarantine is the quarantine, and it is NOT WIRED. It refuses both
// directions, and a fake that answered Recent with an empty slice would have
// let a test pass against a port the pass had quietly started reading — the
// cut orphan_hold_unreleased check the file's own comment names as one this
// plane may not ship.
//
// The constructor no longer takes the quarantine, and that is the honest
// version of that comment: the check may not ship, so the port does not sit in
// the worker's shape promising it. Kept here, unwired, because a future check
// that wants it must put the parameter back, and this is where its fake will
// be when it does.
type reconQuarantine struct {
	persistence.QuarantinedFacts
	world *reconWorld
}

func (q reconQuarantine) Record(context.Context, persistence.QuarantinedFact) error {
	q.world.write("quarantine.record")
	return errReconWritesRefused
}

func (q reconQuarantine) Recent(context.Context, time.Time, time.Time, int) ([]persistence.QuarantinedFact, error) {
	q.world.write("quarantine.recent")
	return nil, errReconWritesRefused
}

// reconCursor is the ingestion position. Position answers the world's value
// and nothing else — the pass's own comment is at length about why the one
// cursor question this plane may not answer is whether the position moved
// backwards, so a fake that offered a way to script the position's ORDER would
// be offering a test for a check that must not exist. Advance refuses and
// counts: a pass that moved the position would be changing the feed's resume
// point to say something about its own bookkeeping.
type reconCursor struct {
	persistence.IngestionCursor
	world *reconWorld
}

func (c reconCursor) Position(context.Context) (string, error) {
	if err := c.world.failOn["cursor.position"]; err != nil {
		return "", err
	}
	// The stop signal's landing place. This is the pass's LAST read before it
	// closes its run row, so cancelling here stages exactly the race the
	// shutdown path has to survive: every check is done, every finding is
	// recorded, and the only thing left is the close. It is a field on the
	// world rather than a clock the test advances because a clock cannot
	// cancel anything — the signal is a fact about the caller's context, and
	// only the caller can produce it.
	if c.world.cancelBeforeFinish != nil {
		c.world.cancelBeforeFinish()
		c.world.cancelBeforeFinish = nil
	}
	return c.world.position, nil
}

func (c reconCursor) Advance(context.Context, string, string) error {
	c.world.write("cursor.advance")
	return errReconWritesRefused
}

// reconClock is the database clock — the pass's ONLY source of "now", for the
// window's upper bound, the finding's two timestamps and nothing else. The
// world advances it between passes in the window tests, so a pass that read a
// process wall clock instead would produce a window no two passes could tile.
type reconClock struct {
	persistence.Clock
	world *reconWorld
}

func (c reconClock) Now(context.Context) (time.Time, error) {
	if err := c.world.failOn["clock.now"]; err != nil {
		return time.Time{}, err
	}
	return c.world.now, nil
}

// ---------------------------------------------------------------------------
// The findings table and the run history
// ---------------------------------------------------------------------------

// reconFindings is the durable output of the pass, and it runs Open's
// contract rather than scripting one: the partial unique index over
// status = 'open' is the arbiter, a second Open for a key already held
// creates nothing, touches last_seen_at alone, and reports false. That bool
// is the whole dedup story — the pass's FindingsOpened and FindingsUnchanged
// counters are read straight off it — so a fake returning a scripted answer
// would be testing the pass against a behaviour Open does not have.
//
// Restatus and OpenCount are implemented rather than refused because they are
// reads of the same table an operator resolves rows in, and a resolution must
// free the key for a genuine later recurrence — which is the one thing a
// re-Open test can check and the one property a stub could not carry.
type reconFindings struct {
	persistence.ReconciliationFindings
	world *reconWorld
}

func (f reconFindings) Open(_ context.Context, finding persistence.Finding) (bool, error) {
	w := f.world
	if err := w.failOn["findings.open"]; err != nil {
		return false, err
	}
	w.record("finding.open:" + finding.CheckKind + "/" + finding.SubjectKind)
	key := findingKey{check: finding.CheckKind, subjectKind: finding.SubjectKind, subjectID: finding.SubjectID}
	row, open := w.findings[key]
	if open && row.confirmations > 0 {
		// The convergence: last_seen_at and the evidence move, detected_at and
		// the identity do not, and the caller is told the row was not new.
		row.lastSeenAt = finding.LastSeenAt
		row.observed = finding.Observed
		row.detail = finding.Detail
		row.severity = finding.Severity
		row.confirmations++
		return false, nil
	}
	w.findings[key] = &reconFindingRow{
		check:         finding.CheckKind,
		subject:       finding.SubjectID,
		severity:      finding.Severity,
		observed:      finding.Observed,
		detail:        finding.Detail,
		detectedAt:    finding.DetectedAt,
		lastSeenAt:    finding.LastSeenAt,
		confirmations: 1,
	}
	return true, nil
}

// Restatus is the ONE sanctioned UPDATE, compare-and-set on the status the
// caller read. The world's rows are all open and it is not the pass that
// resolves them, so this exists for the test that closes a finding and then
// re-opens the same divergence — the one that proves a key is freed by a
// resolution rather than held for ever by the first sighting.
func (f reconFindings) Restatus(_ context.Context, id int64, from, to string, at time.Time) (bool, error) {
	if err := f.world.failOn["findings.restatus"]; err != nil {
		return false, err
	}
	if from != "open" || to != "resolved" {
		return false, errors.New("fake: the test script only resolves a finding")
	}
	// The world's findings carry no surrogate id of their own — the pass never
	// reads one back — so the resolve is keyed by the row's position in a
	// stable order, and the id is what Open would have allocated.
	for i, key := range f.world.findingOrder() {
		if int64(i+1) == id {
			row := f.world.findings[key]
			row.confirmations = 0 // resolved: no longer open, so the key is free
			row.lastSeenAt = at
			return true, nil
		}
	}
	return false, nil
}

func (f reconFindings) OpenCount(context.Context) (int, error) {
	if err := f.world.failOn["findings.count"]; err != nil {
		return 0, err
	}
	open := 0
	for _, row := range f.world.findings {
		if row.confirmations > 0 {
			open++
		}
	}
	return open, nil
}

// findingOrder is the world's open findings in a stable order, so Restatus's
// surrogate id means the same thing twice. Sorted by the key's own fields
// because Go's map iteration order is deliberately unspecified and a test that
// resolved "the finding" and then opened the same key must not depend on which
// row it happened to hit.
func (w *reconWorld) findingOrder() []findingKey {
	keys := make([]findingKey, 0, len(w.findings))
	for key := range w.findings {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].check != keys[j].check {
			return keys[i].check < keys[j].check
		}
		if keys[i].subjectKind != keys[j].subjectKind {
			return keys[i].subjectKind < keys[j].subjectKind
		}
		return keys[i].subjectID < keys[j].subjectID
	})
	return keys
}

// reconRuns is the pass history, which is also the high-water mark. Begin
// allocates the id (never a caller's), stamps the window and leaves the row
// 'running'; Finish is the compare-and-set, and returns false when the row is
// not in the status the caller claims to have read — so a pass that finished
// twice, or finished a row that was never opened, is refused rather than
// quietly overwriting a verdict.
//
// The observations are the ones a test asserts on: the exact (from, to) every
// window carried, the order Begin and Finish happened in relative to the
// sweep, and the counters each finish wrote.
type reconRuns struct {
	persistence.ReconciliationRuns
	world *reconWorld
}

func (r reconRuns) Begin(_ context.Context, scope string, from, to time.Time) (int64, error) {
	w := r.world
	if err := w.failOn["runs.begin"]; err != nil {
		return 0, err
	}
	// The claim, enforced the way the partial unique index enforces it: a
	// window another RUNNING row already holds is refused, and finishing a
	// pass releases it. Modelling it here rather than only in the adapter's
	// integration test is what lets the pass's own handling be proved — the
	// adapter test proves the engine refuses, and this proves the pass does
	// not treat the refusal as a failure.
	for _, run := range w.runs {
		if run.Status == runRunning && run.Scope == scope && run.WindowFrom.Equal(from) {
			w.record(fmt.Sprintf("run.begin:refused:%s:%s", scope, windowLabel(from)))
			return 0, persistence.ErrWindowClaimed
		}
	}
	w.record(fmt.Sprintf("run.begin:%s:%s:%s", scope, windowLabel(from), windowLabel(to)))
	id := int64(len(w.runs)) + 1
	w.runs = append(w.runs, persistence.Run{
		ID:         id,
		Scope:      scope,
		Status:     runRunning,
		StartedAt:  w.now,
		WindowFrom: from,
		WindowTo:   to,
	})
	return id, nil
}

func (r reconRuns) Finish(ctx context.Context, id int64, from, to string, finishedAt time.Time, counters persistence.RunCounters) (bool, error) {
	w := r.world
	finishBy := w.finishBy
	// A store writes through the context it is handed, and a context the
	// caller has already cancelled is a context the write is refused on. The
	// fake models that, because the pass's own shutdown path depends on it:
	// without the check here, a Finish issued on a cancelled context would
	// succeed against the fake and the regression test for it would pass
	// against the bug it exists to catch.
	if err := ctx.Err(); err != nil {
		w.record(fmt.Sprintf("run.finish:refused:%s", err))
		return false, err
	}
	// The call is logged BEFORE the refusal, because the call happened: a
	// pass that closed its run and was refused is a different fact from a
	// pass that never tried, and the log is the only place that difference
	// is visible. A fake that recorded nothing on a failed Finish would make
	// the two indistinguishable and would let a pass that silently gave up
	// pass the same test.
	w.record(fmt.Sprintf("run.finish:%d:%s:%s:%d:%d:%d", id, from, to,
		counters.BucketsScanned, counters.FindingsOpened, counters.FindingsUnchanged))
	if err := w.failOn["runs.finish"]; err != nil {
		return false, err
	}
	for i := range w.runs {
		if w.runs[i].ID != id || w.runs[i].Status != from {
			continue
		}
		// The compare-and-set is the port's own guard, and a fake that
		// ignored it would let a test pass that the real statement refuses:
		// finishBy names the status the caller believes the row is in, and a
		// row in any other status is not this call's to move. It is
		// deliberately settable, because the case that matters is the one a
		// correct caller never produces — a pass whose own window moved
		// under it — and that case cannot be staged otherwise.
		if finishBy != "" && w.runs[i].Status != finishBy {
			continue
		}
		finished := finishedAt
		w.runs[i].Status = to
		w.runs[i].FinishedAt = &finished
		w.runs[i].BucketsScanned = counters.BucketsScanned
		w.runs[i].FindingsOpened = counters.FindingsOpened
		w.runs[i].FindingsUnchanged = counters.FindingsUnchanged
		return true, nil
	}
	return false, nil
}

// Latest is the newest run BY ID and never by finished_at, and the difference
// is the whole point: a pass that died mid-sweep has no finish and is still
// where the sweep stopped, so a fake that sorted by finished_at would hand the
// next pass a high-water mark it may not move backwards from.
func (r reconRuns) Latest(context.Context) (persistence.Run, error) {
	if err := r.world.failOn["runs.latest"]; err != nil {
		return persistence.Run{}, err
	}
	if len(r.world.runs) == 0 {
		return persistence.Run{}, persistence.ErrNotFound
	}
	return r.world.runs[len(r.world.runs)-1], nil
}

// ---------------------------------------------------------------------------
// The ledger reader
// ---------------------------------------------------------------------------

// reconBucketSide is one bucket's two sides: what its row caches and what its
// own legs derive. A test drifts one by seeding them apart, which is the state
// F1 exists to find and the state no real production path creates — which is
// why the fake can mint it and the migration can forbid it.
type reconBucketSide struct {
	cached     accounting.Bucket
	derivation accounting.Derivation
}

// reconLedger is the LedgerReader — the slice of the accounting use cases the
// pass is allowed to see, and the narrowest interface in the package. It is
// the only way the pass reaches money at all, and it holds no write.
//
// The race is staged the only way it can be staged honestly: a scripted
// sequence of ANSWERS per bucket, where the first read can disagree with the
// second. That is precisely the situation the re-read exists for — a leg
// landing between the sweep's read and the finding's write — and a fake that
// returned the same answer twice could never exercise it. The settlement side
// works the same way, and additionally tracks HOW MANY times each settlement
// has been read, which is how a test proves the pass's per-settlement cache
// holds for the first read and that the re-read bypasses it.
type reconLedger struct {
	// world is the shared state, so the ledger's own failure switches are the
	// world's rather than a second set of knobs a test would have to keep in
	// step with the first.
	world *reconWorld

	// buckets and settlements are the steady state each read answers from.
	buckets     map[accounting.FundingBucketID]reconBucketSide
	settlements map[accounting.SettlementID]persistence.SettlementLedger

	// bucketAnswers overrides the steady state for the first bucketCalls
	// reads of one bucket, in order; the last scripted answer repeats. It is
	// how a test says "the first read disagrees, the second agrees" — the
	// race — without the fake having to know what a race is.
	bucketAnswers map[accounting.FundingBucketID][]reconBucketAnswer

	// settlementAnswers overrides the steady state for the first
	// settlementCalls reads of one settlement, in order. Past the script the
	// steady state answers again, which is how a test stages a first read that
	// was one thing and a later read that was another — the state the
	// re-reads exist to catch, and the one a script that repeated its last
	// answer forever could not express.
	settlementAnswers map[accounting.SettlementID][]persistence.SettlementLedger

	// settlementFailFrom makes every read of one settlement from the Nth
	// onward fail, and the Nth is counted over ANSWERED reads only. That
	// qualification is the whole reason this switch exists rather than a plain
	// "fail from the Nth call": the confirming re-reads deliberately bypass
	// the pass's per-settlement cache, so a counter over calls would raise
	// the error on a re-read the pass has not reached yet and the test would
	// be asserting a different read than the one it staged. A settlement with
	// no entry here is unaffected, so the world's plain settlement failure —
	// the one that fails every read of every settlement — is the default and
	// a stage narrows it to one subject and one point in its read history.
	//
	// The first read it fails is the
	// failure a real database can produce: a read that answered a moment ago and
	// does not answer now. Without it a fake could only fail a settlement from
	// its FIRST read, and the re-reads' error branch — the one that decides a
	// pass fails rather than records off a read that did not complete — would
	// be unreachable.
	settlementFailFrom map[accounting.SettlementID]int

	// bucketCalls and settlementReads count the reads, so the cache and the
	// re-read are both observable rather than inferred.
	bucketCalls     map[accounting.FundingBucketID]int
	settlementReads map[accounting.SettlementID]int
}

// reconBucketAnswer is one scripted read of a bucket: either the report, or a
// failure. The two are separate because a first read that FAILS and a second
// that agrees are different tests, and a fake that could not express one of
// them would leave the pass's error paths untested.
type reconBucketAnswer struct {
	report ReconcileReport
	err    error
}

// newReconLedger is the zero world the reconciliation fakes share; it is
// built by newReconWorld rather than here so a ledger cannot exist without one.
func (l *reconLedger) reset() {
	l.buckets = map[accounting.FundingBucketID]reconBucketSide{}
	l.settlements = map[accounting.SettlementID]persistence.SettlementLedger{}
	l.bucketAnswers = map[accounting.FundingBucketID][]reconBucketAnswer{}
	l.settlementAnswers = map[accounting.SettlementID][]persistence.SettlementLedger{}
	l.settlementFailFrom = map[accounting.SettlementID]int{}
	l.bucketCalls = map[accounting.FundingBucketID]int{}
	l.settlementReads = map[accounting.SettlementID]int{}
}

// The read that stages the race: a scripted answer, the steady state, and the
// call counter. The order is fixed — the Nth read of a bucket takes the Nth
// scripted answer, and past the script it takes the last one — so a test that
// scripts two answers and the pass reads three times gets the same second
// answer twice, which is what a leg that landed and stayed looks like.
//
// A script is AUTHORITATIVE when it is present: it answers every read of that
// subject whether or not the steady state has an entry, which is what lets a
// test stage a settlement that is absent on the first read and present on the
// second — a state the steady state cannot express at all, and one the pass
// must be able to read without a second find.
func (l *reconLedger) ReconcileBucket(_ context.Context, bucketID accounting.FundingBucketID) (ReconcileReport, error) {
	if err := l.world.failOn["ledger.bucket"]; err != nil {
		return ReconcileReport{}, err
	}
	read := l.bucketCalls[bucketID]
	l.bucketCalls[bucketID]++
	if scripted, ok := l.bucketAnswers[bucketID]; ok && len(scripted) > 0 {
		if read >= len(scripted) {
			read = len(scripted) - 1
		}
		return scripted[read].report, scripted[read].err
	}
	side, ok := l.buckets[bucketID]
	if !ok {
		return ReconcileReport{}, persistence.ErrNotFound
	}
	return ReconcileReport{
		Bucket:     side.cached,
		Derivation: side.derivation,
		Consistent: side.derivation.ConsistentWith(side.cached),
	}, nil
}

func (l *reconLedger) SettlementLedger(_ context.Context, settlementID accounting.SettlementID) (persistence.SettlementLedger, error) {
	read := l.settlementReads[settlementID]
	l.settlementReads[settlementID]++
	// The world's failure is the default and a stage narrows it; the switch is
	// read BEFORE the answer is chosen because a read that fails does not
	// answer, and the count the next read sees must be the count of the ones
	// that did.
	if from, staged := l.settlementFailFrom[settlementID]; staged {
		if read >= from {
			if err := l.world.failOn["ledger.settlement"]; err != nil {
				return persistence.SettlementLedger{}, err
			}
		}
	} else if err := l.world.failOn["ledger.settlement"]; err != nil {
		return persistence.SettlementLedger{}, err
	}
	// The script covers the first N reads; past that the steady state answers
	// again, which is how a test stages a first read that was whole and a
	// confirming read that was not.
	if scripted, ok := l.settlementAnswers[settlementID]; ok && read < len(scripted) {
		return scripted[read], nil
	}
	ledger, ok := l.settlements[settlementID]
	if !ok {
		return persistence.SettlementLedger{}, persistence.ErrNotFound
	}
	return ledger, nil
}

// ---------------------------------------------------------------------------
// The settlement shapes the tests mint
// ---------------------------------------------------------------------------

// A settlement fixture is a header, a per-kind multiset, and the two bucket
// counts the shape rule is checked with. The helpers below name the shapes the
// checks care about — a header with no legs at all, a whole plan's legs, the
// partial writes a plan cannot produce — so a test says which shape it means
// and the arithmetic lives in one place.

// healthySettlement is the shape BuildSettle writes for one bucket's full
// spend: a consume and no release, because the consume took the hold's whole
// amount. consumes 1, releases 0, buckets 1, without-release 1 — the
// equality the shape rule states, and the shape a passing settlement almost
// always has.
func healthySettlement(id accounting.SettlementID, requestID accounting.RequestID, total int64) persistence.SettlementLedger {
	return persistence.SettlementLedger{
		Settlement: accounting.Settlement{
			ID:           id,
			RequestID:    requestID,
			SettledTotal: accounting.Amount(total),
		},
		ConsumeSum:            total,
		Legs:                  1,
		LegsByKind:            map[string]int64{string(accounting.KindConsume): 1},
		Buckets:               1,
		BucketsWithoutRelease: 1,
	}
}

// zeroPricedSettlement is the shape the zero-priced settle writes: a header of
// record and NO legs at all. It is a legitimate answer — the request is named
// settled and nothing moved — and the clauses of the shape rule are
// inequalities precisely so that zero satisfies them. A shape rule with a
// minimum of one leg would file a critical finding against every free request
// the plane ever saw.
func zeroPricedSettlement(id accounting.SettlementID, requestID accounting.RequestID) persistence.SettlementLedger {
	return persistence.SettlementLedger{
		Settlement: accounting.Settlement{
			ID:           id,
			RequestID:    requestID,
			SettledTotal: accounting.Amount(0),
		},
		LegsByKind: map[string]int64{},
	}
}

// tailReleasedSettlement is the other healthy shape: the spend came in under
// the hold, so a release went back beside the consume. Two buckets, two
// consumes, one release, one bucket without a release — the arithmetic the
// rule states, with a tail that had to be written back.
func tailReleasedSettlement(id accounting.SettlementID, requestID accounting.RequestID, total int64) persistence.SettlementLedger {
	return persistence.SettlementLedger{
		Settlement: accounting.Settlement{
			ID:           id,
			RequestID:    requestID,
			SettledTotal: accounting.Amount(total),
		},
		ConsumeSum: total,
		Legs:       3,
		LegsByKind: map[string]int64{
			string(accounting.KindConsume): 2,
			string(accounting.KindRelease): 1,
		},
		Buckets:               2,
		BucketsWithoutRelease: 1,
	}
}

// foreignKindSettlement is a settlement carrying a leg kind the settle path
// never writes on one — a grant, a topup, an adjustment. A settlement with
// such a leg is a leg attached to a record it does not belong to: the
// schema's reference shape pins those kinds' settlement_id to NULL, so the
// presence means the constraint did not hold either.
func foreignKindSettlement(id accounting.SettlementID, requestID accounting.RequestID, total int64, kind string) persistence.SettlementLedger {
	return persistence.SettlementLedger{
		Settlement: accounting.Settlement{
			ID:           id,
			RequestID:    requestID,
			SettledTotal: accounting.Amount(total),
		},
		ConsumeSum: total,
		Legs:       2,
		LegsByKind: map[string]int64{
			string(accounting.KindConsume): 1,
			kind:                           1,
		},
		Buckets:               1,
		BucketsWithoutRelease: 1,
	}
}

// shapeKeyOf is a test's shorthand for the multiset a shape finding is keyed
// on. The key is the leg multiset rendered sorted, and it is written out here
// rather than read from the production shapeKey so a test that asserted
// against the same function would pass even if that function were wrong.
func shapeKeyOf(byKind map[string]int64) string {
	kinds := make([]string, 0, len(byKind))
	for kind := range byKind {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, fmt.Sprintf("%s=%d", kind, byKind[kind]))
	}
	if len(parts) == 0 {
		return ""
	}
	return joinWith(parts, ",")
}

// joinWith renders a multiset key the way the finding's own key is rendered:
// the parts in sorted order, separated by commas, with no trailing separator.
// It is written out here rather than reached for in the production file so
// that a test asserting the key is asserting the STRING an operator will read
// in the findings table and not a call into the function under test.
func joinWith(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, sep)
}

// decodeObserved is how a test reads a finding's evidence back. The evidence
// is jsonb the pass built, and a test that string-matched it would be pinning
// the key ORDER rather than the figures — so it decodes and compares the
// values, which is the thing a reader of the table compares.
func decodeObserved(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("the finding carries no evidence; the columns that made the comparison are the evidence")
	}
	fields := map[string]any{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("the evidence is not the json object the port stores (%v): %s", err, raw)
	}
	return fields
}

// ---------------------------------------------------------------------------
// Small helpers the tests share
// ---------------------------------------------------------------------------

// settledAmount is the pointer a settled fact carries its charge in, and the
// nil is the honest value on every other kind — the same shape statement the
// schema's CHECK pins, and the reason the pass's evidence dereferences rather
// than dereferences blindly.
func settledAmount(v int64) *int64 { return &v }

// captureMethod is the same for the usage claim's provenance.
func captureMethod(v string) *string { return &v }

// equalWindows is the comparison every window assertion wants: two instants,
// compared by value, with a message that says which bound is wrong. A
// difference between two bounds is often the whole question — the doc on
// windowLabel says as much about the error strings that carry them — so the
// failure prints both.
func equalWindows(gotFrom, gotTo, wantFrom, wantTo time.Time) bool {
	return gotFrom.Equal(wantFrom) && gotTo.Equal(wantTo)
}

// bucketID mints a bucket id in uuid v7 form: version nibble 7, RFC 4122
// variant, lowercase hex. The sweep compares ids as strings and a test whose
// ids were not in the canonical form would be testing a walk the real column
// never performs.
func bucketID(seq int) accounting.FundingBucketID {
	return accounting.FundingBucketID(fmt.Sprintf("0198f0a4-3f6c-7000-8000-%012x", seq))
}

// settlementID mints a settlement id in the same form. Distinct settlements
// must be distinct ids, because a finding's subject is the id and two
// divergences keyed on one id would be one finding.
func settlementID(seq int) accounting.SettlementID {
	return accounting.SettlementID(fmt.Sprintf("0198f0a4-3f6c-7000-9000-%012x", seq))
}

// requestID mints a request id in the same form, for the same reason.
func requestID(seq int) accounting.RequestID {
	return accounting.RequestID(fmt.Sprintf("0198f0a4-3f6c-7000-a000-%012x", seq))
}

// appliedSettled is the applied row a settled fact produces, with the
// settlement of record it claims — which is what F4 and F5 both walk BACKWARDS
// from. It is an AppliedFact and not the wire Fact settledFact mints in the
// applier's file: the pass never sees a fact, only what the applier recorded
// it derived, and a test that handed the pass a wire fact would be testing a
// different subject than the one the pass checks.
func appliedSettled(req accounting.RequestID, settlement accounting.SettlementID, seq int64, amount int64) persistence.AppliedFact {
	return persistence.AppliedFact{
		RequestID:     string(req),
		Kind:          ingestion.KindSettled,
		AppendSeq:     seq,
		SettledAmount: settledAmount(amount),
		CaptureMethod: captureMethod("reported"),
		SettlementID:  string(settlement),
	}
}

// appliedOrphan is the applied row an unbillable orphan produces. It is its
// own class by design — a request MAY carry one beside its settlement — which
// is what makes the charged-and-disclaimed finding a question about money
// rather than about a schema.
func appliedOrphan(req accounting.RequestID, seq int64) persistence.AppliedFact {
	return persistence.AppliedFact{
		RequestID:     string(req),
		Kind:          ingestion.KindUnbillableOrphaned,
		AppendSeq:     seq,
		CaptureMethod: captureMethod("estimated"),
	}
}

// appliedTerminal is the applied row a released or expired effect produces.
// It carries no amount and no settlement, and it lands in the SETTLEMENT class
// beside the settled row — which is the split F5 exists to name.
func appliedTerminal(req accounting.RequestID, seq int64, kind string) persistence.AppliedFact {
	return persistence.AppliedFact{
		RequestID: string(req),
		Kind:      kind,
		AppendSeq: seq,
	}
}

// orderIs reports whether the observation log contains want as a contiguous
// run starting at from. The tests that pin ORDER do not pin the whole log —
// the pass makes a different number of calls in different shapes — so they pin
// the two entries that must not swap.
func orderIs(order []string, from int, want ...string) bool {
	if len(order) < from+len(want) {
		return false
	}
	return slices.Equal(order[from:from+len(want)], want)
}

// countOrder is how many times an entry appears in the log, for the
// assertions that care about the NUMBER of reads rather than their order —
// the bucket re-read is one extra read per diverging bucket and not one per
// bucket, which is a cost claim the log can state.
// The entries the world writes for these are all suffixed with the arguments
// they carried, so "how many finishes" has to be a count of the PREFIX rather
// than of the whole entry — an equality test would be asking every caller to
// restate the window, the run id and the three counters in the call, and a
// call that forgot one of them would report a pass that issued no finish at
// all. A bare `run.finish` would lose the arguments an order assertion needs,
// so the count matches the prefix and the exact entry is still readable in the
// log.
func countOrder(order []string, entry string) int {
	total := 0
	for _, seen := range order {
		if seen == entry || strings.HasPrefix(seen, entry+":") {
			total++
		}
	}
	return total
}
