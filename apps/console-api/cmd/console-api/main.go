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
		cycleCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_, err := ingestion.Replay(cycleCtx)
		switch {
		case err == nil:
		case errors.Is(err, dataplaneport.ErrCursorExpired):
			log.Printf("console-api ingestion position is no longer replayable; the feed holds until an operator resolves the position: %v", err)
		case errors.Is(err, context.Canceled):
		default:
			log.Printf("console-api ingestion pass failed: %v", err)
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
	// is exactly what this file exists to refuse. The projection loop is the
	// first caller this process has gained, and the readiness probe the
	// second: the wiring below is theirs, and the store reaches the server
	// because the probe asks the pool a question no use case can.

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

	// The fact consumer's loop, in the same shape: one goroutine, one pass
	// per tick, the first immediately, each pass under its own deadline. The
	// pull model makes the cadence a freshness dial and nothing else — a page
	// this pass missed is the page the next pass reads, from the position the
	// last committed one wrote, and a pass that fails is one log line rather
	// than a crash, because nothing was lost by failing.
	ingestionWG.Add(1)
	go runIngestionLoop(ctx, ingestion, cfg.DataPlane.IngestionInterval, cfg.DataPlane.IngestionTimeout)

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
