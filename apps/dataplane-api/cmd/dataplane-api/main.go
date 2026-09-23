// The dataplane-api binary: the Data Plane's management surface, and nothing
// else.
//
// Everything HTTP lives in internal/adapters/inbound/http so it can be tested
// through httptest without a process; what remains here is the version stamp,
// the listener, signal handling and the one place an adapter is chosen.
// Configuration is parsed and validated by internal/config before this package
// sees it, so an invalid value fails the process before it has accepted
// traffic.
//
// Exactly one outbound adapter is composed here, and the seam it crosses is the
// one this application exists for: the Data Plane. It is an HTTP call rather
// than a database or a cache connection, and that difference is the rule rather
// than a detail — this process owns no data, so what it answers with comes from
// the Data Plane over a management call the Data Plane can refuse (ADR 0006 §9,
// §11). ADR 0006 §9 records how this application reaches Data Plane state as
// the split's open question; this is the answer for the usage-fact feed — a
// network call through the port in internal/ports/outbound — and it leaves the
// separate question of a shared domain core where the ADR left it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	stdhttp "net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/adapters/inbound/http"
	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/adapters/outbound/dataplane"
	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane-api/internal/config"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/dataplane-api
//
// release-please tags the repository root `v<version>` — one release unit for
// the whole monorepo — and the release lane passes that tag through this flag.
// It remains the one version source: application.New receives the value and
// GET /version serves it, so no endpoint ever restates it.
var version = "dev"

func main() {
	// log without timestamps: the process has one startup line and one
	// shutdown line, and a caller that wants to know when they happened has
	// the process supervisor's clock for that.
	log.SetFlags(0)

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		log.Printf("dataplane-api configuration: %v", err)
		os.Exit(1)
	}

	// Binding before announcing or serving: a port already in use is a
	// startup failure with a clear line, not a race between two goroutines.
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Printf("dataplane-api listen: %v", err)
		os.Exit(1)
	}

	// SIGINT and SIGTERM are the two signals that mean "stop": Ctrl-C sends
	// the former, every container orchestrator sends the latter. NotifyContext
	// cancels on the first one received.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The outbound client is built here rather than inside the adapter, and it
	// carries no timeout of its own. The call it makes inherits the inbound
	// request's context, so its lifetime is already bounded by the caller's
	// deadline and by that caller going away; a second, invented deadline at
	// this layer would be a number chosen without any latency to measure it
	// against, and it would compound with the caller's rather than replace it.
	stdhttpClient := &stdhttp.Client{}
	usageFacts := dataplane.New(stdhttpClient, cfg.DataPlaneURL, cfg.ServiceCredential)

	// The credential reaches two places and no others: the adapter presents it
	// to the Data Plane, and the authenticator compares it against what callers
	// present here. Both are handed the same configured value, so there is no
	// second copy of a secret to drift, and neither logs it.
	authenticator := http.NewServiceAuthenticator(cfg.ServiceCredential)

	server := &stdhttp.Server{
		Handler: http.New(application.New(version, usageFacts), authenticator),
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

	log.Printf("dataplane-api %s listening on %s", version, listener.Addr())
	if err := <-errCh; err != nil {
		log.Printf("dataplane-api error: %v", err)
		os.Exit(1)
	}

	log.Printf("dataplane-api %s stopped", version)
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
