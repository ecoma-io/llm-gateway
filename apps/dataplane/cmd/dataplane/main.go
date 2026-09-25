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
// over that pool is constructed here too, and this is the change that first
// wires it: the catalog read the Control Plane's commerce roll makes is the
// first use case that runs a query, so the store and the catalog repositories
// over it are built below rather than deferred again. The fact reader is
// constructed over the same pool and answers from the usage_events table the
// runtime storage schema creates: replaying what this process recorded, in the
// order the store committed it, to whoever presents a position the stream can
// still honour. The projection applier is built straight over that pool
// beside them — the consumer side of the Control → Data credential projection
// (ADR 0007) and the first use case in this process that writes.
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
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	stdhttp "net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/adapters/inbound/http"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/adapters/inbound/management"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/adapters/outbound/postgres"
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

// maxLeaseOwnerOctets is the reservation lease owner's storage bound, the one
// the runtime storage schema CHECKs. It lives here, beside the derivation, so
// the truncation cannot drift from the column it serves.
const maxLeaseOwnerOctets = 256

const (
	// readTimeout bounds one request's read — headers and body together — on
	// both listeners. Without it a client that connects and stalls mid-upload
	// holds its connection and its goroutine for as long as it likes. Thirty
	// seconds is room for the largest request this surface reads on a slow
	// client, and a ceiling a stalled peer cannot outwait.
	readTimeout = 30 * time.Second

	// idleTimeout closes a keep-alive connection that has carried no request
	// for two minutes. It applies between requests and never inside one, so it
	// cannot shorten a response that is still being written — the streamed
	// answers this process exists to serve are bounded by the WriteTimeout
	// decision in newServer, not by this.
	idleTimeout = 120 * time.Second

	// maxHeaderBytes is the request-header ceiling, stated explicitly rather
	// than left sitting at the standard library's default of the same size: a
	// limit a reader can see is a limit a reviewer can argue with.
	maxHeaderBytes = 1 << 20
)

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

	// The schema check is the other half of "opened and pinged": the pool can
	// answer a ping while the migration that creates its tables has not run,
	// and a process that learns that on its first real query has been serving
	// readiness the whole time. The check runs at boot, where the DSN and the
	// migration are deployment's business together, and it fails the boot
	// rather than every later request.
	schemaCtx, cancelSchema := context.WithTimeout(context.Background(), postgresOpenTimeout)
	err = postgres.ValidateSchema(schemaCtx, pool)
	cancelSchema()
	if err != nil {
		log.Printf("dataplane postgres: %v", err)
		os.Exit(1)
	}
	// The persistence.Store over this pool is constructed in bind, together
	// with the catalog repositories it backs — this is the change that first
	// runs a query, the one the deferral here was waiting for. The usage-fact
	// reader rides the same pool, and answers from the usage_events table the
	// runtime storage schema creates; the projection applier (ADR 0007) rides
	// it too, as the first use case in this process that writes.

	// Every listener is bound before any of them serves. A process that
	// answered on the runtime port while its management port was already taken
	// would be a process an operator believes is fully deployed and which is
	// half of one.
	services, err := bind(cfg, pool)
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
// The pool arrives here rather than the adapters being built beside the open in
// main so that the whole composition — store, repositories, application,
// listeners — is one function a reader can read top to bottom as "what this
// process is made of".
//
// The application is constructed once and shared, because it is one process
// with one set of outbound ports; the two listeners are two surfaces over it.
// That is also why the runtime handler is built here and not inside the
// management branch: the surface that serves traffic must exist whether or not
// an administrative port was configured.
//
// The pool arrives as an argument rather than being reopened here because the
// adapters are built over it — one database, one pool, and the composition
// root is where the adapters that share it meet: the fact reader the usage
// feed is read from, and the projection applier whose mirror the management
// listener writes on the Control Plane's behalf (ADR 0007). The mirror is
// this process's one state, shared with everything else the request path
// will read.
func bind(cfg config.Config, pool *sql.DB) ([]service, error) {
	// One store over the pool, and the catalog's four repositories over that
	// store — every one of them the same postgres adapter, because the catalog
	// is this plane's own database and there is exactly one of it. The
	// repositories share the store rather than each opening their own so a
	// unit of work spans them: the alias aggregate is written as a row and its
	// candidates in one transaction, and that guarantee is made here, at
	// composition, not inside a use case that reaches for a global. The
	// usage-fact reader is the second adapter over the same pool.
	catalogStore := postgres.New(pool)
	catalog := application.NewCatalog(
		catalogStore,
		postgres.NewBackends(catalogStore),
		postgres.NewModelAliases(catalogStore),
		postgres.NewAliasGroupVersions(catalogStore),
	)
	app := application.New(version, postgres.NewUsageFacts(pool), catalog, postgres.NewProjectionApplier(pool))

	// The admission use cases over the same store: credential verification
	// and the admit-or-account-for-it decision behind the chat completion
	// route. Every port reads the plane's own database — the credential
	// mirror, the alias and price books, the runtime storage the migrations
	// created — so one unit of work spans them, the same guarantee the
	// catalog's repositories were given above. The horizons come from the
	// validated configuration; the lease owner is derived, because a lease
	// name a deployment could set twice is a lease name that means nothing.
	credentials := postgres.NewCredentials(catalogStore)
	admission := application.NewChatAdmission(
		catalogStore,
		credentials,
		postgres.NewModelAliases(catalogStore),
		postgres.NewPriceBook(catalogStore),
		postgres.NewRequestRepository(catalogStore),
		postgres.NewIntakeRepository(catalogStore),
		postgres.NewReservationRepository(catalogStore),
		postgres.NewQuotaProjectionRepository(catalogStore),
		postgres.NewFactRepository(catalogStore),
		application.AdmissionConfig{
			HoldWindow: cfg.ReservationHoldWindow,
			LeaseTTL:   cfg.ReservationLeaseTTL,
			LeaseOwner: leaseOwner(),
		},
	)

	runtimeListener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}

	services := []service{{
		name:     "runtime",
		server:   newServer(http.New(app, http.WithChatAuthenticator(application.NewCredentialAuthenticator(credentials)), http.WithChatCompletion(admission)), cfg),
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
		server:   newServer(management.New(app, cfg.ManagementToken), cfg),
		listener: managementListener,
	})
	return services, nil
}

// leaseOwner derives the name this process leases its reservations under:
// hostname:pid, truncated hard to the schema's 256-octet bound with the pid
// kept whole — the pid is the half that answers "is this lease mine", so the
// host name absorbs the cut. It is derived rather than configured on purpose:
// a lease name a deployment could set twice is two processes claiming one
// lease, which is the confusion the lease exists to prevent. A host name that
// cannot be learned is not a boot failure — the value only has to separate
// processes from each other, and the pid half of it does.
func leaseOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return leaseOwnerFrom(host, os.Getpid())
}

// leaseOwnerFrom is the derivation the owner above makes from its inputs, and
// the shape the tests exercise. The cut is rune-safe: a host name that stops
// mid-rune would put a torn UTF-8 sequence into a value the schema stores and
// the lease compares byte-for-byte, so the truncation steps back to the
// nearest valid boundary before the pid half is joined.
func leaseOwnerFrom(host string, pid int) string {
	suffix := ":" + strconv.Itoa(pid)
	if keep := maxLeaseOwnerOctets - len(suffix); len(host) > keep {
		host = host[:keep]
		for len(host) > 0 && !utf8.ValidString(host) {
			host = host[:len(host)-1]
		}
	}
	return host + suffix
}

// newServer builds one listener's http.Server with the transport posture both
// of this process's surfaces serve under: the configured header timeout, the
// read and idle bounds, and the header ceiling.
//
// WriteTimeout is deliberately absent, and its absence is the decision here.
// The runtime's inference contract is a Server-Sent Events stream — an answer
// that is supposed to keep being written for as long as the model keeps
// producing — and a write deadline would kill a long stream mid-answer: the
// one failure this process must not introduce itself. What a slow client can
// actually hold this process with is reading, and that side is bounded.
func newServer(handler stdhttp.Handler, cfg config.Config) *stdhttp.Server {
	return &stdhttp.Server{
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
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
// io.Closer because closing is the whole of what run does with it. A close
// that fails is reported rather than buried: it is joined onto whatever run
// is already returning, so the process exits non-zero even when the drain
// itself was clean — the sibling composition root's contract, kept the same
// here so the two copies read as one rule.
func run(ctx context.Context, stop context.CancelFunc, services []service, shutdownTimeout time.Duration, pool io.Closer) error {
	// Buffered for the same reason as main's channel: each goroutine records
	// its result and exits even if the select below never reads it.
	errCh := make(chan error, len(services))
	for _, s := range services {
		go func(s service) {
			errCh <- s.server.Serve(s.listener)
		}(s)
	}

	var serveErr error
	select {
	case err := <-errCh:
		// A listener failed on its own — a closed socket, a listener taken
		// away. ErrServerClosed belongs to the shutdown path (Shutdown closes
		// the listener and Serve reports it), and on this branch it cannot
		// occur because Shutdown has not run.
		if !errors.Is(err, stdhttp.ErrServerClosed) {
			serveErr = err
		}

	case <-ctx.Done():
		stop()
		serveErr = shutdown(services, shutdownTimeout)
	}

	if closeErr := pool.Close(); closeErr != nil {
		serveErr = errors.Join(serveErr, closeErr)
	}
	return serveErr
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
