// The dataplane binary — the runtime that serves LLM traffic.
//
// Everything HTTP lives in internal/adapters/inbound/http and
// internal/adapters/inbound/management so it can be tested through httptest
// without a process; what remains here is the version stamp, the database
// pool, the listeners, and signal handling. Configuration is parsed and
// validated by internal/config before this package sees it, so an invalid
// value fails the process before it has accepted traffic.
//
// This is the only place in the module where an adapter is chosen and handed to
// the application. Nothing above it names a concrete implementation: the
// application depends on the outbound ports, and a port whose adapter is not
// wired yet is simply absent — which is why the cache adapter exists here and
// is not constructed, wired by the change that first has a command to run, not
// before. The database is the exception whose time has come: its pool is opened
// and pinged here at startup, because intake, reservations and usage are
// durable only through the database this process owns. The persistence.Store
// over that pool is still not constructed — no use case runs a query yet, and
// it is wired by the change that first does. The fact reader is constructed,
// and what it answers today is that there is no durable source yet: refusing is
// the honest state of a Data Plane whose fact table arrives with the accounting
// schema, and it is not the same thing as an adapter that was forgotten.
//
// There are two listeners and one application between them. The runtime's is the
// public surface, opened on DATAPLANE_ADDR always; the management surface is
// private, opened on DATAPLANE_MANAGEMENT_ADDR only when deployment asks for it,
// and never without the credential that protects it. Both are the same process
// serving the same application, and the separation is what makes an
// administrative route impossible to reach on the address that serves LLM
// traffic.
//
// Startup takes no dependency on the Control Plane, and neither does the request
// path: a runtime that could not start because a management service was down
// would have handed that service the authority to refuse LLM traffic (ADR 0006
// §4).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	stdhttp "net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/adapters/inbound/http"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/adapters/inbound/management"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/adapters/outbound/postgres"
	usagefactsadapter "github.com/ecoma-io/llm-gateway/apps/dataplane/internal/adapters/outbound/usagefacts"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/config"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/dataplane
//
// release-please tags the repository root `v<version>` — one release unit for
// the whole monorepo — and the release lane passes that tag through this flag.
// It remains the one version source: application.New receives the value and
// GET /version serves it, so no endpoint ever restates it.
var version = "dev"

// postgresOpenTimeout bounds opening the runtime's own database: one ping, at
// boot, against the database this process owns. Startup validation must fail
// fast — an unreachable database is exactly the condition a container
// orchestrator should restart on, and a booting process that hung here would
// look alive to everything but every request it served. Five seconds is the
// whole of the patience a local database on the same plane owes a booting
// process; it is not a retry budget.
const postgresOpenTimeout = 5 * time.Second

// service is one listener this process serves, together with the name that
// makes a failure or a startup line say which one it was.
type service struct {
	name     string
	server   *stdhttp.Server
	listener net.Listener
}

func main() {
	// log without timestamps: the process has a startup line and a shutdown
	// line, and a caller that wants to know when they happened has the process
	// supervisor's clock for that.
	log.SetFlags(0)

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		log.Printf("dataplane configuration: %v", err)
		os.Exit(1)
	}

	// The runtime's database is opened — and pinged — before any listener
	// binds. Intake, reservations and usage are durable only because this
	// database holds them, so a process whose database is unreachable has
	// nothing to serve from and must not pretend otherwise. The dependency
	// points at this process's own plane's database and nowhere else: nothing
	// here waits on the Control Plane (ADR 0006 §4), and failing on an
	// unreachable database is failing on a store this process owns, not one it
	// borrows.
	poolCtx, cancelPool := context.WithTimeout(context.Background(), postgresOpenTimeout)
	pool, err := postgres.Open(poolCtx, postgres.Options{
		DSN:             cfg.Postgres.DSN,
		MaxOpenConns:    cfg.Postgres.MaxOpenConns,
		MaxIdleConns:    cfg.Postgres.MaxIdleConns,
		ConnMaxLifetime: cfg.Postgres.ConnMaxLifetime,
		ConnMaxIdleTime: cfg.Postgres.ConnMaxIdleTime,
	})
	cancelPool()
	if err != nil {
		log.Printf("dataplane postgres: %v", err)
		os.Exit(1)
	}
	log.Printf("dataplane %s postgres pool on %s", version, postgresLocation(cfg))
	// The persistence.Store over this pool is still not constructed: no use
	// case runs a query yet, and postgres.New(pool) is wired by the change
	// that first does. (The usage-fact reader stays refusing until the fact
	// table exists; an open pool does not change that answer.)

	// Every listener is bound before any of them serves. A process that
	// answered on the runtime port while its management port was already taken
	// would be a process an operator believes is fully deployed and which is
	// half of one.
	services, err := bind(cfg)
	if err != nil {
		log.Printf("dataplane listen: %v", err)
		os.Exit(1)
	}

	// SIGINT and SIGTERM are the two signals that mean "stop": Ctrl-C sends
	// the former, every container orchestrator sends the latter. NotifyContext
	// cancels on the first one received.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Buffered so the serve goroutine can record its result and exit even if
	// main never reads it — no goroutine outliving the decision it reports.
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, stop, services, cfg.ShutdownTimeout, pool)
	}()

	for _, s := range services {
		log.Printf("dataplane %s %s listening on %s", version, s.name, s.listener.Addr())
	}
	if err := <-errCh; err != nil {
		log.Printf("dataplane error: %v", err)
		os.Exit(1)
	}

	log.Printf("dataplane %s stopped", version)
}

// postgresLocation renders where the pool points — host, port, database — for
// the startup line. The DSN it reads is never rendered itself: its userinfo
// carries the credential, and a startup line is exactly the kind of string
// that ends up pasted somewhere public.
func postgresLocation(cfg config.Config) string {
	parsed, err := url.Parse(cfg.Postgres.DSN)
	if err != nil {
		// Unreachable behind Load, which has already validated the DSN; the
		// fallback stays silent about the value rather than risk the
		// credential.
		return "(dsn not rendered)"
	}
	return fmt.Sprintf("%s:%s/%s", parsed.Hostname(), parsed.Port(), strings.TrimPrefix(parsed.Path, "/"))
}

// bind opens every listener this configuration asks for and wires the handlers
// onto them.
//
// The application is constructed once and shared, because it is one process
// with one set of outbound ports; the two listeners are two surfaces over it.
// That is also why the runtime handler is built here and not inside the
// management branch: the surface that serves traffic must exist whether or not
// an administrative port was configured.
func bind(cfg config.Config) ([]service, error) {
	app := application.New(version, usagefactsadapter.New())

	runtimeListener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}

	services := []service{{
		name:     "runtime",
		server:   &stdhttp.Server{Handler: http.New(app), ReadHeaderTimeout: cfg.ReadHeaderTimeout},
		listener: runtimeListener,
	}}

	if cfg.ManagementAddr == "" {
		return services, nil
	}

	managementListener, err := net.Listen("tcp", cfg.ManagementAddr)
	if err != nil {
		_ = runtimeListener.Close()
		return nil, err
	}

	services = append(services, service{
		name:     "management",
		server:   &stdhttp.Server{Handler: management.New(app, cfg.ManagementToken), ReadHeaderTimeout: cfg.ReadHeaderTimeout},
		listener: managementListener,
	})
	return services, nil
}

// run serves every listener until the process is asked to stop, then drains.
//
// On the stop signal it calls stop before draining — restoring the kernel's
// default handling, so an impatient operator's second signal is answered by the
// process dying rather than by another graceful attempt — and only then waits
// the configured timeout for in-flight requests. A drain that overruns its
// timeout is a defect worth reporting, so run returns the error and the process
// exits non-zero instead of green.
//
// The database pool is closed when run returns, which orders it correctly on
// every path: after the servers have drained on the stop path, so no request
// still in flight loses its database mid-query, and immediately on the
// listener-failed path, where nothing is being served at all. It arrives as
// io.Closer because closing is the whole of what run does with it.
func run(ctx context.Context, stop context.CancelFunc, services []service, shutdownTimeout time.Duration, pool io.Closer) error {
	// The close's result has nothing to be reported to: the drain outcome run
	// is about to return is the last word this process speaks, and a failed
	// close after a successful drain would only bury it.
	defer func() { _ = pool.Close() }()

	// Buffered for the same reason as main's channel: each goroutine records
	// its result and exits even if the select below never reads it.
	errCh := make(chan error, len(services))
	for _, s := range services {
		go func(s service) {
			errCh <- s.server.Serve(s.listener)
		}(s)
	}

	select {
	case err := <-errCh:
		// A listener failed on its own — a closed socket, a listener taken
		// away. ErrServerClosed belongs to the shutdown path (Shutdown closes
		// the listener and Serve reports it), and on this branch it cannot
		// occur because Shutdown has not run.
		if !errors.Is(err, stdhttp.ErrServerClosed) {
			return err
		}
		return nil

	case <-ctx.Done():
		stop()
		return shutdown(services, shutdownTimeout)
	}
}

// shutdown drains every listener and reports what went wrong, if anything did.
//
// The listeners are drained together rather than one after another, against a
// single deadline. Draining them in sequence would give a process with two
// busy listeners twice the grace period its deployment granted it, and the
// second listener's drain would start with the first one's already spent.
func shutdown(services []service, timeout time.Duration) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Each outcome carries the name of the listener it came from: results
	// arrive in completion order, so reading them positionally would blame
	// whichever listener happened to be mentioned first in the configuration
	// for a timeout the other one caused.
	type outcome struct {
		name string
		err  error
	}
	outcomes := make(chan outcome, len(services))
	for _, s := range services {
		go func(s service) {
			outcomes <- outcome{name: s.name, err: s.server.Shutdown(shutdownCtx)}
		}(s)
	}

	var failures []error
	for range services {
		if result := <-outcomes; result.err != nil {
			failures = append(failures, fmt.Errorf("graceful shutdown of the %s listener: %w", result.name, result.err))
		}
	}
	return errors.Join(failures...)
}
