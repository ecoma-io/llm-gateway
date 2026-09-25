package main

import (
	"context"
	"errors"
	"net"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The lifecycle tests below drive run() against a real listener, because the
// behaviour they pin — an in-flight request finishes, an overrun is reported,
// a dead listener is not swallowed — only exists once a socket is involved.
// Handler behaviour belongs to internal/adapters/inbound/http and is tested there through
// httptest, without a process.

// TestNewHandlerGatesReadinessOnTheStoreItWasGiven is the wiring test: the
// composition the process actually serves, driven through both answers of the
// store it is handed. A handler rebuilt without the store is the static
// /readyz this composition removed, and nothing else in this package would
// notice — the route package proves the probe gates, this proves the process
// gave it something to gate on.
func TestNewHandlerGatesReadinessOnTheStoreItWasGiven(t *testing.T) {
	tests := []struct {
		name       string
		pingErr    error
		wantStatus int
	}{
		{name: "a store that answers is ready", wantStatus: stdhttp.StatusOK},
		{name: "a store that does not answer is not ready", pingErr: errors.New("ping: connection refused"), wantStatus: stdhttp.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newHandler("v0.1.0", answeringStore{pingErr: tt.pingErr})

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(stdhttp.MethodGet, "/readyz", nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("GET /readyz: status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}

	// The same handler still serves the version stamped into the process: the
	// store is a dependency of the probe, not a replacement for the
	// application.
	rec := httptest.NewRecorder()
	newHandler("v0.1.0", answeringStore{}).ServeHTTP(rec, httptest.NewRequest(stdhttp.MethodGet, "/version", nil))
	if got := rec.Body.String(); got != "{\"version\":\"v0.1.0\"}\n" {
		t.Errorf("GET /version: body = %q, want the stamped version", got)
	}
}

// answeringStore stands in for the persistence.Store the process builds from
// its pool: the one member the readiness probe asks, and the answer a test
// wants.
type answeringStore struct {
	pingErr error
}

// Ping implements the readiness port the server constructor is handed.
func (s answeringStore) Ping(context.Context) error { return s.pingErr }

func TestRunDrainsAnInFlightRequestBeforeReturning(t *testing.T) {
	listener := listenForTest(t)
	server, started, release := blockingServer(t)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := serveForTest(t, ctx, cancel, server, listener, time.Second)
	requestErr := requestForTest(t, listener)

	waitForStart(t, started)

	// The stop signal arrives while the request is in flight.
	cancel()

	select {
	case err := <-runErr:
		t.Fatalf("run() returned %v with a request still in flight", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()

	if err := <-requestErr; err != nil {
		t.Fatalf("in-flight request error = %v", err)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("run() error = %v, want nil after an orderly drain", err)
	}
}

func TestRunReportsAShutdownThatOverrunsItsTimeout(t *testing.T) {
	listener := listenForTest(t)
	server, started, release := blockingServer(t)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The drain timeout is far shorter than the request's own lifetime, so the
	// shutdown is guaranteed to overrun it.
	runErr := serveForTest(t, ctx, cancel, server, listener, 50*time.Millisecond)
	requestErr := requestForTest(t, listener)

	waitForStart(t, started)
	cancel()

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("run() error = nil, want the overrun reported")
		}
		if !strings.Contains(err.Error(), "graceful shutdown") {
			t.Errorf("run() error = %q, want it to name the graceful shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() never returned after the shutdown overran its timeout")
	}

	// The request the shutdown gave up on still has to be let go, or the
	// connection it holds outlives the test.
	release()
	if err := <-requestErr; err != nil {
		t.Fatalf("overrun request error = %v", err)
	}
}

func TestRunReportsAListenerThatFailsOnItsOwn(t *testing.T) {
	listener := listenForTest(t)
	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close() error = %v", err)
	}

	server := &stdhttp.Server{Handler: stdhttp.HandlerFunc(func(_ stdhttp.ResponseWriter, _ *stdhttp.Request) {})}
	err := run(context.Background(), func() {}, server, listener, time.Second)

	if err == nil {
		t.Fatal("run() error = nil, want the listener failure")
	}
	if strings.Contains(err.Error(), "graceful shutdown") {
		t.Errorf("run() error = %q, want the listener failure rather than a drain report", err)
	}
}

// blockingServer returns a server whose only handler announces that it started
// and then waits to be released, together with an idempotent release function —
// so an assertion path and a cleanup path can both call it without racing to
// close a channel.
func blockingServer(t *testing.T) (*stdhttp.Server, <-chan struct{}, func()) {
	t.Helper()
	started := make(chan struct{})
	held := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(held) }) }
	t.Cleanup(release)

	return &stdhttp.Server{Handler: stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		close(started)
		<-held
		w.WriteHeader(stdhttp.StatusNoContent)
	})}, started, release
}

func listenForTest(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

// serveForTest starts run in the background and returns the channel its
// outcome arrives on.
func serveForTest(t *testing.T, ctx context.Context, stop context.CancelFunc, server *stdhttp.Server, listener net.Listener, timeout time.Duration) chan error {
	t.Helper()
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, stop, server, listener, timeout)
	}()
	return runErr
}

// requestForTest sends one request at the listener and reports it as soon as
// the server answers — which, against a blocking handler, is at release.
func requestForTest(t *testing.T, listener net.Listener) chan error {
	t.Helper()
	requestErr := make(chan error, 1)
	go func() {
		requestCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		request, err := stdhttp.NewRequestWithContext(requestCtx, stdhttp.MethodGet, "http://"+listener.Addr().String()+"/", nil)
		if err != nil {
			requestErr <- err
			return
		}
		response, err := stdhttp.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
		requestErr <- err
	}()
	return requestErr
}

func waitForStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the server")
	}
}
