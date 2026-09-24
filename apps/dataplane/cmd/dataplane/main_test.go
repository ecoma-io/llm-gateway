package main

import (
	"context"
	"net"
	stdhttp "net/http"
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

// TestRunDrainsEveryListenerItWasGiven covers the two-listener process: the
// runtime's port and the private management port are served by one run, and a
// stop signal has to drain both. A shutdown that drained the runtime and left
// the management listener to the process exit would drop an administrative call
// that was already in flight — which, on the fact feed, is a read whose
// consumer would then retry from a position it had not advanced.
func TestRunDrainsEveryListenerItWasGiven(t *testing.T) {
	runtimeListener := listenForTest(t)
	managementListener := listenForTest(t)
	runtimeServer, runtimeStarted, releaseRuntime := blockingServer(t)
	managementServer, managementStarted, releaseManagement := blockingServer(t)
	defer releaseRuntime()
	defer releaseManagement()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, cancel, []service{
			{name: "runtime", server: runtimeServer, listener: runtimeListener},
			{name: "management", server: managementServer, listener: managementListener},
		}, time.Second)
	}()

	runtimeRequest := requestForTest(t, runtimeListener)
	managementRequest := requestForTest(t, managementListener)
	waitForStart(t, runtimeStarted)
	waitForStart(t, managementStarted)

	cancel()

	select {
	case err := <-runErr:
		t.Fatalf("run() returned %v with requests still in flight on both listeners", err)
	case <-time.After(50 * time.Millisecond):
	}

	releaseRuntime()
	releaseManagement()

	for name, request := range map[string]chan error{"runtime": runtimeRequest, "management": managementRequest} {
		if err := <-request; err != nil {
			t.Fatalf("the in-flight %s request error = %v", name, err)
		}
	}
	if err := <-runErr; err != nil {
		t.Fatalf("run() error = %v, want nil after an orderly drain of both listeners", err)
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
	err := run(context.Background(), func() {}, []service{{name: "runtime", server: server, listener: listener}}, time.Second)

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

// serveForTest starts run in the background against a single listener and
// returns the channel its outcome arrives on. The tests that need two drive
// run directly, because what they are checking is that one process serves both.
func serveForTest(t *testing.T, ctx context.Context, stop context.CancelFunc, server *stdhttp.Server, listener net.Listener, timeout time.Duration) chan error {
	t.Helper()
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, stop, []service{{name: "runtime", server: server, listener: listener}}, timeout)
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
