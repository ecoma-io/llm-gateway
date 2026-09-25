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
// piece of infrastructure wired today — this process owns Control Plane state
// (ADR 0006 §7), so the pool is opened and validated at startup — and it is
// wired as a pool only: the persistence.Store is built by the change that
// first gives a use case a query to run, not before.
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
	"syscall"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/inbound/http"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/outbound/postgres"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/config"
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
	// is exactly what this file exists to refuse.

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
		Handler: http.New(application.New(version)),
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

	log.Printf("console-api %s listening on %s", version, listener.Addr())
	if err := <-errCh; err != nil {
		log.Printf("console-api error: %v", err)
		os.Exit(1)
	}

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
