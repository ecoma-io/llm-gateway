// The console-api binary: process wiring, and nothing else.
//
// Everything HTTP lives in internal/adapters/inbound/http so it can be tested
// through httptest without a process; what remains here is the version stamp,
// the listener, and signal handling. Configuration is parsed and validated by
// internal/config before this package sees it, so an invalid value fails the
// process before it has accepted traffic.
//
// This is the only place in the module where an adapter is chosen and handed
// to the application. Nothing above it names a concrete implementation: the
// application depends on the outbound ports. The database pool is the one
// piece of storage infrastructure wired here — this process owns Control
// Plane state (ADR 0006 §7), so the pool is opened and validated at startup —
// and it is wired as a store and a pool: the two background loops resolve
// their units of work through the store, and the readiness probe asks the
// store whether the pool can still answer. The repositories and use cases
// built over it arrive with the caller that needs them, and the loops are
// those callers: the projection producer, the usage-fact consumer, and the
// readiness probe.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	stdhttp "net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/inbound/http"
	dataplaneadapter "github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/outbound/postgres"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/config"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	dataplaneport "github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/console-api
//
// release-please tags the repository root `v<version>` — one release unit for
// the whole monorepo — and the release lane passes that tag through this flag.
// It remains the one version source: application.New receives the value and
// GET /version serves it, so no endpoint ever restates it.
var version = "dev"

// postgresConnectTimeout bounds the startup dial to the database. Startup
// validation must fail fast, not hang: a process whose database is
// unreachable is down, and a down process that hangs on connect is a restart
// loop an operator cannot see the shape of. The pool's own settings govern
// every connection after this one dial.
const postgresConnectTimeout = 5 * time.Second

// projectionWG tracks the producer's loop goroutine so the pool is not closed
// under a cycle that is still finishing. One goroutine, one Add, one Wait.
var projectionWG sync.WaitGroup

// ingestionWG is the fact consumer's half of the same discipline: the pool
// closes after the replay loop has stopped, not under the page it is still
// applying. One goroutine, one Add, one Wait.
var ingestionWG sync.WaitGroup

// reconciliationWG is the reconciliation worker's half of it, and it carries
// one more obligation than its two siblings: the worker's pass closes its run
// row on a context derived from the cancelled one (ADR 0011), so this wait is
// what gives that close its grace to land before the pool closes under it.
var reconciliationWG sync.WaitGroup

// runProjectionLoop reconciles the Data Plane's credential mirror with the
// Control Plane's projection log until the process is asked to stop: one
// Reconcile per interval, each under its own deadline, the first immediately.
//
// Every failure is a log line and nothing more. The loop deliberately does
// not crash the process on a refused delivery or an unreachable peer — the
// mirror's freshness is a management-plane concern and the protocol's answer
// to a failed cycle is the next one — and it deliberately does not swallow
// repeated failure either: the line is written every interval, so a producer
// that cannot deliver is a log an operator cannot miss. A context
// cancellation is not a failure: it is the stop signal arriving mid-cycle,
// and it ends the loop quietly.
func runProjectionLoop(ctx context.Context, producer *application.ProjectionDelivery, interval, timeout time.Duration) {
	defer projectionWG.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	runOnce := func() {
		cycleCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := producer.Reconcile(cycleCtx, false); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("console-api projection cycle failed: %v", err)
		}
	}

	runOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// runIngestionLoop pulls the Data Plane's usage-fact feed until the process is
// asked to stop: one Replay pass per interval, each under its own deadline,
// the first immediately. The loop is the consumer half of the pull-with-replay
// delivery model (ADR 0006 §5): the runtime records facts and never notifies
// anyone, and this loop is what moves the Control Plane's position through
// them.
//
// Every failure is a log line and nothing more, for the projection loop's
// reason turned around: the feed is durable and replay is the delivery model,
// so a failed pass loses nothing and the next pass re-reads the same page —
// the applier's idempotency is what makes that free. The line is written
// every interval, so a consumer that cannot apply a page is a log an operator
// cannot miss. One failure has a remedy the next tick cannot supply and gets
// its own sentence: an expired position means the Data Plane can no longer
// replay from where this process stands, and the pass the loop wants is the
// one only an operator can bring about — the loop keeps polling (the position
// never moves but so does nothing else), and the repeated line is the state
// staying visible until someone resolves it.
//
// A context cancellation is not a failure: it is the stop signal arriving
// mid-pass, and it ends the loop quietly — the pass the signal interrupts is
// one unit of work, so it either committed whole before the cancellation
// landed or rolls back whole, and either way the next process re-reads the
// same page.
func runIngestionLoop(ctx context.Context, ingestion *application.FactIngestion, interval, timeout time.Duration) {
	defer ingestionWG.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	runOnce := func() {
		// One deadline for the whole drain: a pass the feed answers with
		// has_more is followed by another immediately, so a burst of facts
		// costs at most one timeout of lag — the deadline, not the interval,
		// is what bounds how far behind the consumer can fall. A pass that
		// ends in an error stops the drain; the next tick retries it from
		// the durable position, which is exactly where the failed one left
		// off.
		cycleCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		for {
			result, err := ingestion.Replay(cycleCtx)
			switch {
			case err == nil:
			case errors.Is(err, dataplaneport.ErrCursorExpired):
				log.Printf("console-api ingestion position is no longer replayable; the feed holds until an operator resolves the position: %v", err)
			case errors.Is(err, context.Canceled):
			default:
				log.Printf("console-api ingestion pass failed: %v", err)
			}
			if err != nil || !result.HasMore || cycleCtx.Err() != nil {
				return
			}
		}
	}

	runOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

func main() {
	// log without timestamps: the process has one startup line and one
	// shutdown line, and a caller that wants to know when they happened has
	// the process supervisor's clock for that.
	log.SetFlags(0)

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		log.Printf("console-api configuration: %v", err)
		os.Exit(1)
	}

	// The pool is opened before anything else can depend on it, and the
	// process refuses to start without it: owning Control Plane state means
	// a database this process cannot reach is not a degraded process but one
	// that should not exist yet. The dial is bounded — postgresConnectTimeout
	// above — so that refusal is prompt. The failure carries no DSN: Open
	// sanitises driver errors, which are the one place a credential could
	// otherwise be quoted back.
	poolCtx, cancelPool := context.WithTimeout(context.Background(), postgresConnectTimeout)
	db, err := postgres.Open(poolCtx, postgres.Options{
		DSN:             cfg.Postgres.DSN,
		MaxOpenConns:    cfg.Postgres.MaxOpenConns,
		MaxIdleConns:    cfg.Postgres.MaxIdleConns,
		ConnMaxLifetime: cfg.Postgres.ConnMaxLifetime,
		ConnMaxIdleTime: cfg.Postgres.ConnMaxIdleTime,
	})
	cancelPool()
	if err != nil {
		log.Printf("console-api postgres: %v", err)
		os.Exit(1)
	}
	// The persistence.Store, the repositories and the use cases are
	// constructed here (postgres.New, the NewXxx repositories, application's
	// NewXxx use cases) by the change that first puts a caller on the other
	// side of them in this process — an HTTP route, a worker loop — not
	// before. The foundation phases (identity, commerce) ship their use cases
	// and repositories tested at the application boundary; wiring them ahead
	// of a caller would be composition guessed at, and a guessed composition
	// is exactly what this file exists to refuse. The projection loop was the
	// first caller this process gained, the readiness probe the second, and
	// the fact consumer the third: the wiring below is theirs, and the store
	// reaches the server because the probe asks the pool a question no use
	// case can.

	// The Control Plane's state access, and the projection producer on top of
	// it (ADR 0007). The store is the one object every persistence port
	// resolves its units of work through; the log is the producer's read face
	// onto the change log and the materialized snapshot. The identity use
	// cases that write the log are not constructed yet — no HTTP surface
	// reaches them — and are wired by the change that first serves one.
	store := postgres.New(db)
	projectionLog := postgres.NewProjectionLog(store)

	// The accounting use cases, the money grammar the fact consumer's derived
	// effects move through. Nothing else in this process calls them yet — no
	// HTTP surface reaches a primitive — but the applier below is their
	// consumer-side caller, and they are constructed here because a use case
	// built and unwired was the state this file used to refuse. They are the
	// same primitives the runtime's own settle path runs (B6); the consumer
	// derives from the feed and books through them, it does not reimplement
	// them.
	accounting := application.NewAccounting(store,
		postgres.NewFundingBuckets(store),
		postgres.NewFundingLedger(store),
		postgres.NewSettlements(store),
		postgres.NewFundingProjections(store),
		postgres.NewPaygAccounts(store),
		postgres.NewClock(store),
	)

	// The Data Plane half of both loops: an HTTP client whose behaviour is
	// deliberately the library default (the per-cycle deadlines below are what
	// bound every call), and the adapter that speaks the management façade's
	// two contracts — the projection producer's and the fact feed's — with
	// the credential this process was given. The credential travels on the
	// wire and nowhere else — the adapter's errors are built from status
	// codes and sentinels, never from the request.
	client := &stdhttp.Client{}
	consumer := dataplaneadapter.New(client, cfg.DataPlane.URL, cfg.DataPlane.Credential)
	producer := application.NewProjectionDelivery(projectionLog, consumer)

	// The usage-fact consumer (ADR 0006 §5): the idempotency ledger and the
	// quarantine it writes its effects and refusals into, the position store
	// that is the one thing about the feed the Data Plane never learns, and
	// the applier that turns facts into accounting effects — or into recorded
	// refusals — inside the unit of work the replay use case opens for each
	// page. The cursor and the applier's writes are unit-of-work-shaped by
	// construction; the wiring gives them the store they resolve those units
	// from, and the loop below is the only caller that drives the four
	// together.
	applier := application.NewFactApplier(accounting,
		postgres.NewAppliedFacts(store),
		postgres.NewQuarantinedFacts(store),
	)
	ingestion := application.NewFactIngestion(consumer, store, postgres.NewIngestionCursor(store), applier)

	// The reconciliation pass (ADR 0011): the two tables it records into, and
	// the three reads it derives its verdicts from. The pass is given no
	// accounting use case — the ledger reader below is a two-method read
	// interface over the same repositories the accounting primitives use, and
	// it is the shape of the port, not a promise in a comment, that keeps
	// Adjust and Settle one call away from a pass that must never make them.
	//
	// The bucket reader is the accounting use case itself, because F1 asks the
	// question its own ReconcileBucket was written to answer. The settlement
	// reader is the settlements repository, whose Ledger read was added for
	// F2 and F3 and which the accounting primitives already hold.
	settlements := postgres.NewSettlements(store)
	reconciliation := application.NewReconciliation(
		postgres.NewFundingBuckets(store),
		postgres.NewAppliedFacts(store),
		postgres.NewIngestionCursor(store),
		postgres.NewReconciliationFindings(store),
		postgres.NewReconciliationRuns(store),
		settlementLedgerReader{accounting: accounting, settlements: settlements},
		postgres.NewClock(store),
		application.ReconciliationSettings{
			Lookback: cfg.Reconciliation.Lookback,
			Batch:    cfg.Reconciliation.Batch,
		},
	)

	// One startup line naming the target — host, port, database — and
	// nothing else. The DSN carries the role's password, so the pieces are
	// read out of it rather than the whole of it printed; url.Parse is the
	// same shape config.Load validated, so the branch below is unreachable
	// for a configuration that got this far. It is still handled rather than
	// trusted, and the DSN is not printed on it: url.Parse's own error text
	// quotes the string it rejected, credentials included.
	target, err := url.Parse(cfg.Postgres.DSN)
	if err != nil {
		log.Printf("console-api postgres: unable to name the database target")
		os.Exit(1)
	}
	log.Printf("console-api postgres pool ready for %s/%s", net.JoinHostPort(target.Hostname(), target.Port()), strings.TrimPrefix(target.Path, "/"))

	// Binding before announcing or serving: a port already in use is a
	// startup failure with a clear line, not a race between two goroutines.
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Printf("console-api listen: %v", err)
		os.Exit(1)
	}

	// SIGINT and SIGTERM are the two signals that mean "stop": Ctrl-C sends
	// the former, every container orchestrator sends the latter. NotifyContext
	// cancels on the first one received.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	server := &stdhttp.Server{
		Handler: newHandler(version, store),
		// ReadHeaderTimeout guards against a peer that connects and says
		// nothing — a slowloris costs a goroutine forever without it. The
		// read/write body and idle timeouts wait until there is real traffic
		// with real sizes to measure them against; inventing numbers before
		// that is guessing with a straight face.
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	// Buffered so the serve goroutine can record its result and exit even if
	// main never reads it — no goroutine outliving the decision it reports.
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, stop, server, listener, cfg.ShutdownTimeout)
	}()

	// The projection producer's loop: one goroutine, one Reconcile per tick,
	// and therefore no concurrent cycles to guard against — a cycle that
	// overruns its interval simply delays the next one. Each cycle runs under
	// its own deadline so a hung read or delivery fails the way any failed
	// cycle does — logged, retried by the next tick — instead of wedging the
	// loop forever. The first cycle runs immediately: a restart should
	// reconcile at once, not wait an interval to discover nothing changed.
	// A failed cycle is one log line, not a crash: delivery is at-least-once
	// against a durable log, so the loop's answer to every failure is the
	// next tick.
	projectionWG.Add(1)
	go runProjectionLoop(ctx, producer, cfg.DataPlane.ProjectionInterval, cfg.DataPlane.ProjectionTimeout)

	// The fact consumer's loop, in the same shape: one goroutine, the first
	// pass immediately, each pass under its own deadline — and a pass the
	// feed answers with has_more keeps passing under that same deadline
	// rather than waiting for the next tick, so the lag a burst of facts
	// costs is bounded by one timeout, not by however long the burst outlasts
	// the interval. The pull model makes the cadence a freshness dial and
	// nothing else — a page this pass missed is the page the next pass
	// reads, from the position the last committed one wrote, and a pass that
	// fails is one log line rather than a crash, because nothing was lost by
	// failing.
	ingestionWG.Add(1)
	go runIngestionLoop(ctx, ingestion, cfg.DataPlane.IngestionInterval, cfg.DataPlane.IngestionTimeout)

	// The reconciliation worker's loop: one goroutine, the first pass
	// immediately, and a gap between passes that widens while the store is the
	// one failing and spreads across replicas while it is not. The cadence and
	// the bound come from the validated configuration, which refuses a bound
	// at or below the cadence — that is the shape in which two passes sweep
	// overlapping windows, and the window claim does not catch it.
	reconciliationWG.Add(1)
	go runReconciliationLoop(ctx, reconciliation.Reconcile,
		cfg.Reconciliation.Interval, cfg.Reconciliation.Timeout)

	log.Printf("console-api %s listening on %s", version, listener.Addr())
	if err := <-errCh; err != nil {
		log.Printf("console-api error: %v", err)
		os.Exit(1)
	}

	// The pool closes after both loops have stopped, not before: a loop's
	// last cycle or pass may still be finishing its reads and writes on
	// pooled connections, and closing earlier would pull the pool out from
	// under them — the same ordering the drain above observes for in-flight
	// requests.
	projectionWG.Wait()
	ingestionWG.Wait()
	reconciliationWG.Wait()

	// The pool is closed here, after run has returned, and not before: the
	// drain inside run may still be finishing in-flight requests, and those
	// requests run their queries on pooled connections — closing earlier
	// would pull the pool out from under them. The error branch above is a
	// process exit without a drain (os.Exit runs no deferred close), so the
	// graceful path is where closing lives.
	if err := db.Close(); err != nil {
		// In the same spirit as a drain that overruns its timeout: a pool
		// that will not close cleanly after a successful drain is a defect
		// worth reporting, and the process exits non-zero instead of green.
		log.Printf("console-api postgres close: %v", err)
		os.Exit(1)
	}

	log.Printf("console-api %s stopped", version)
}

// settlementLedgerReader is the two-read face of the control plane's own money
// tables that the reconciliation pass is given, and it is a struct at the
// composition root rather than a method on an existing type for a reason that
// is about reading: F1 and F2/F3 are answered by two different owners, and
// naming both here is what makes it visible that the pass holds READS of the
// ledger and not the ledger's write path.
//
// The field types are the narrowest each question admits — the accounting use
// case for the bucket, whose own ReconcileBucket exists to answer it, and the
// settlements repository for the legs, whose Ledger read exists for F2 and F3.
// Nothing here widens to a use case without changing this type, and this type
// is one line of the composition root.
type settlementLedgerReader struct {
	accounting  *application.Accounting
	settlements persistence.Settlements
}

func (r settlementLedgerReader) ReconcileBucket(ctx context.Context, bucketID accounting.FundingBucketID) (application.ReconcileReport, error) {
	return r.accounting.ReconcileBucket(ctx, bucketID)
}

func (r settlementLedgerReader) SettlementLedger(ctx context.Context, settlementID accounting.SettlementID) (persistence.SettlementLedger, error) {
	return r.settlements.Ledger(ctx, settlementID)
}

// runReconciliationLoop sweeps one window per interval until the process is
// asked to stop, widening the gap while the store is failing and spreading it
// across replicas while it is not.
//
// It is the third loop in this process and the only one of the three that
// backs off, and the reason is what its failures mean. A failed projection
// cycle or a failed replay pass has already cost nothing: the next tick
// re-reads the same durable page, so the loop's answer to every failure is
// the next tick at the same cadence. A failed reconciliation pass has cost
// nothing either — the window is unadvanced and the next pass re-sweeps it —
// so the same answer would be defensible. What is not defensible is a plane
// asking a database for a full pass every minute while that database is
// unable to answer one, which is the state a tight retry cadence turns a
// recoverable slowdown into. Hence a plain doubling, with no error
// classifier: a worker cannot tell "the database is gone" from "one check's
// read failed", and both are answered by waiting longer.
//
// The ceiling is higher here than on the Data Plane's reaper (sixteen against
// eight) because the work is different in kind. A delayed reclaim widens the
// window of stranded capacity; a delayed detection costs an operator a few
// more minutes before they look at a finding that was already recorded, and
// the finding table is not racing anything.
//
// A context cancellation is not a failure: it is the stop signal arriving
// mid-pass. The pass it interrupts closes its own run row on a context the
// cancellation cannot reach, so nothing is owed on this goroutine's behalf
// and backing off for it would only change how long the loop sits before
// ctx.Done() returns it.
func runReconciliationLoop(ctx context.Context, reconcile func(context.Context) (application.ReconcileSummary, error), interval, timeout time.Duration) {
	defer reconciliationWG.Done()

	// The first gap is the configured interval with its jitter, not the
	// interval bare: a worker that waited the interval before its first pass
	// would have a cold start one cadence longer than a jittered one, and this
	// loop's whole job is converging a window.
	gap := jittered(interval, reconJitterFraction, rand.Int64N)

	runOnce := func() bool {
		cycleCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		summary, err := reconcile(cycleCtx)
		switch {
		case err == nil:
		case errors.Is(err, context.Canceled):
			return false
		default:
			log.Printf("console-api reconciliation pass failed (scanned %d, opened %d, unchanged %d): %v",
				summary.Scanned, summary.FindingsOpened, summary.FindingsUnchanged, err)
			return true
		}
		// Counts, and never an identity: a request id or a settlement id on a
		// log line is a field one dashboard change away from being a metric
		// label, and the findings table is where the identities belong.
		log.Printf("console-api reconciliation pass: scanned %d, opened %d, unchanged %d",
			summary.Scanned, summary.FindingsOpened, summary.FindingsUnchanged)
		return false
	}

	// The first pass runs before the loop waits at all, because a pass that
	// waited an interval before its first sweep would leave everything written
	// during that interval unexamined after every restart. Its outcome seeds
	// the streak: whatever it reports, the gap it sets is the base every
	// doubling after it multiplies.
	streak := 0
	if runOnce() {
		streak = 1
	}
	for {
		timer := time.NewTimer(gap)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		// Recomputed per cycle, so one bad pass widens only the next one and a
		// single success drops the cadence straight back to what the
		// deployment configured.
		streak, gap = nextBackoff(streak, runOnce(), interval)
		gap = jittered(gap, reconJitterFraction, rand.Int64N)
	}
}

// nextBackoff is the cadence arithmetic, a function rather than a field
// because the loop's whole state is one integer and the arithmetic is what a
// reader has to be able to check at a glance.
//
// Three properties are load-bearing, and each was a bug before it was a
// property:
//
//   - a success resets to the CONFIGURED interval, so the loop returns to
//     what the deployment asked for the moment the store recovers
//   - the shift is CLAMPED, not merely bounded by the ceiling. A streak long
//     enough to overflow a shift produces a negative duration, and a negative
//     duration handed to time.NewTimer fires immediately — a worker backing
//     off into a hot loop against the store it was backing off from
//   - the ceiling is measured against the configured interval rather than
//     against the gap the last doubling produced, so it stays a constant
//     number of passes instead of drifting with this worker's own history
func nextBackoff(failedStreak int, failedNow bool, interval time.Duration) (int, time.Duration) {
	if !failedNow {
		return 0, interval
	}
	streak := failedStreak + 1
	shift := streak - 1
	if shift > reconBackoffMaxShift {
		shift = reconBackoffMaxShift
	}
	gap := interval * time.Duration(1<<shift)
	if gap > reconBackoffCeiling*interval {
		gap = reconBackoffCeiling * interval
	}
	return streak, gap
}

// jittered spreads a gap by a bounded fraction of itself, and only ever
// LENGTHENS it. A spread that could also shrink a gap would let a fleet
// re-synchronise on a short gap, which is the one thing the spread exists to
// prevent — so the range handed to the draw starts at zero.
//
// int63n is a parameter rather than a direct call so the arithmetic can be
// tested against both ends of the range without a loop that sleeps.
func jittered(gap time.Duration, fraction float64, int63n func(int64) int64) time.Duration {
	if gap <= 0 || fraction <= 0 {
		return gap
	}
	span := int64(float64(gap) * fraction)
	if span < 1 {
		// Too short to spread by a whole nanosecond. Rounding up instead
		// would turn a one-nanosecond gap into a two-nanosecond one, which is
		// a hundred percent jitter rather than a jitter.
		return gap
	}
	// Int64N is half-open, so the range is one wider than the span: passing
	// the span itself would leave the top of the range undrawable.
	return gap + time.Duration(int63n(span+1))
}

const (
	// reconBackoffMaxShift caps the doubling's exponent at six, which is
	// already past the ceiling for any interval this process would be
	// configured with. The clamp exists so an unbounded streak cannot
	// overflow the shift: a wrapped shift is a NEGATIVE gap, and a negative
	// gap handed to a timer is a hot loop.
	reconBackoffMaxShift = 6

	// reconBackoffCeiling is the longest gap a failing worker ever waits, as a
	// multiple of the configured interval. Sixteen passes is a quarter of an
	// hour at the shipped cadence — long enough that a plane asking a
	// struggling database for a full pass every minute is not asking it every
	// minute any more, and short enough that a recovery is noticed inside an
	// operator's patience rather than across a shift.
	reconBackoffCeiling = 16

	// reconJitterFraction is how far a gap may be spread by its jitter, as a
	// fraction of itself. Twenty percent spreads a fleet of workers wide
	// enough that they stop colliding without making a one-minute cadence feel
	// like a one-twenty cadence to the process reading the run rows.
	reconJitterFraction = 0.2
)

// newHandler composes the one HTTP surface this process serves: the
// application over the build stamp, and the store whose answers gate the
// readiness probe. It is a function rather than an inline field so the
// composition is a fact a test can drive — the probe's gate is the one piece
// of wiring whose absence would leave this process answering ready while its
// database is lost, and a handler rebuilt without the store is exactly the
// regression nothing else would notice.
//
// Readiness never travels through the application: there is no use case for a
// ping, and the probe wants the port itself, so the constructor receives the
// store beside the application rather than an application carrying it.
func newHandler(version string, readiness persistence.Pinger) stdhttp.Handler {
	return http.New(application.New(version), readiness)
}

// run serves until the process is asked to stop, then drains.
//
// On the stop signal it calls stop before draining — restoring the kernel's
// default handling, so an impatient operator's second signal is answered by
// the process dying rather than by another graceful attempt — and only then
// waits the configured timeout for in-flight requests. A drain that overruns
// its timeout is a defect worth reporting, so run returns the error and the
// process exits non-zero instead of green.
func run(ctx context.Context, stop context.CancelFunc, server *stdhttp.Server, listener net.Listener, shutdownTimeout time.Duration) error {
	// Buffered for the same reason as main's channel: the goroutine records
	// its result and exits even if the select below never reads it.
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()

	select {
	case err := <-errCh:
		// The listener failed on its own — a closed socket, a listener taken
		// away. ErrServerClosed belongs to the shutdown path (Shutdown closes
		// the listener and Serve reports it), and on this branch it cannot
		// occur because Shutdown has not run.
		if !errors.Is(err, stdhttp.ErrServerClosed) {
			return err
		}
		return nil

	case <-ctx.Done():
		stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		// Read the serve goroutine's result so it finishes before the process
		// does. Shutdown already closed the listener, so this unblocks
		// immediately with ErrServerClosed.
		<-errCh
		return nil
	}
}
