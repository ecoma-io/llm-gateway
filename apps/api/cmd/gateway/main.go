// The gateway binary: process wiring, and nothing else.
//
// Everything HTTP lives in internal/server so it can be tested through
// httptest without a process; what remains here is listening, signal
// handling, and the version stamp.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/api/internal/server"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/gateway
//
// release-please tags the repository root `v<version>` — one release unit for
// the whole monorepo — and the future release lane will pass that tag through
// this flag. Nothing consumes the value yet beyond the startup log, which is
// the structure the scaffold commits to and nothing more.
var version = "dev"

// shutdownTimeout bounds graceful shutdown. Ten seconds is the ceiling a
// container orchestrator's own termination grace period (30s by default)
// leaves room for: in-flight requests drain first, the process exits second.
const shutdownTimeout = 10 * time.Second

func main() {
	addr := flag.String("addr", ":8080", "listen address for the gateway HTTP server")
	flag.Parse()

	// log without timestamps: the scaffold has one startup line and one
	// shutdown line, and a caller that wants to know when they happened has
	// the process supervisor's clock for that. Revisit when there is real
	// traffic to speak of.
	log.SetFlags(0)

	srv := &http.Server{
		Addr:    *addr,
		Handler: server.New(),
		// ReadHeaderTimeout is set now because it is the one timeout that
		// guards against a peer that connects and says nothing — a
		// slowloris costs a goroutine forever without it. The read/write
		// body and idle timeouts wait until there is real traffic with
		// real sizes to measure them against; inventing numbers before
		// that is guessing with a straight face.
		ReadHeaderTimeout: 5 * time.Second,
	}

	// SIGINT and SIGTERM are the two signals that mean "stop": Ctrl-C sends
	// the former, every container orchestrator sends the latter. NotifyContext
	// cancels on the first one received, and a second signal reverts to the
	// default handler — an impatient operator's kill is answered by the
	// kernel, not by another graceful attempt.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Buffered so the serve goroutine can record its result and exit even if
	// main never reads it — no goroutine outliving the decision it reports.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	log.Printf("gateway %s listening on %s", version, *addr)

	select {
	case err := <-errCh:
		// The listener failed on its own — a port already in use, or a
		// listener taken away. ErrServerClosed belongs to the shutdown
		// path (Shutdown() closes the listener and Serve reports it), and
		// on this branch it cannot occur because Shutdown has not run.
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server error: %v", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		// A stop signal arrived: drain in-flight requests inside the
		// timeout, and treat an overrun as a defect worth a non-zero
		// exit rather than a green one.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown error: %v", err)
			os.Exit(1)
		}
		// Read the serve goroutine's result so it finishes before the
		// process does. Shutdown already closed the listener, so this
		// unblocks immediately with ErrServerClosed.
		<-errCh
	}

	log.Printf("gateway %s stopped", version)
}
