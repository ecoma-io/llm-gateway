package main

import (
	"context"
	"errors"
	"io"
	"net"
	stdhttp "net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/config"
)

// The lifecycle tests below drive run() against a real listener, because the
// behaviour they pin — an in-flight request finishes, an overrun is reported,
// a dead listener is not swallowed — only exists once a socket is involved.
// Handler behaviour belongs to internal/adapters/inbound/http and is tested there through
// httptest, without a process.

// TestLeaseOwnerStaysInsideTheSchemaBound pins the derivation the reservation
// lease is claimed under: host name and pid, never a knob, and truncated —
// when truncation is needed at all — in the host name rather than the pid,
// because the pid is the half that answers "is this lease mine".
func TestLeaseOwnerStaysInsideTheSchemaBound(t *testing.T) {
	owner := leaseOwner()
	if owner == "" {
		t.Fatalf("the derived lease owner is empty")
	}
	if len(owner) > maxLeaseOwnerOctets {
		t.Fatalf("the derived lease owner is %d octets, past the bound the schema CHECKs", len(owner))
	}
	if !strings.HasPrefix(owner, "unknown-host") && !strings.Contains(owner, ":") {
		t.Fatalf("the derived lease owner carries no pid half: %d octets of host name only", len(owner))
	}
}

// TestLeaseOwnerTruncatesOnARuneBoundary: a host name is cut at the schema's
// octet bound, and a cut that lands inside a multi-byte character would put a
// torn UTF-8 sequence into a value the store keeps and the lease compares —
// so the derivation steps back to the last whole rune before the pid half is
// joined.
func TestLeaseOwnerTruncatesOnARuneBoundary(t *testing.T) {
	host := strings.Repeat("høst", 80) // 320 bytes of four-rune repeats, well past the bound
	owner := leaseOwnerFrom(host, 4242)

	if len(owner) > maxLeaseOwnerOctets {
		t.Fatalf("the derived lease owner is %d octets, past the bound the schema CHECKs", len(owner))
	}
	if !utf8.ValidString(owner) {
		t.Fatalf("the derived lease owner is not valid UTF-8: a cut landed inside a rune")
	}
	if !strings.HasSuffix(owner, ":4242") {
		t.Fatalf("the derived lease owner lost its pid half: %q", owner)
	}
	wantHost := host[:maxLeaseOwnerOctets-len(":4242")]
	for len(wantHost) > 0 && !utf8.ValidString(wantHost) {
		wantHost = wantHost[:len(wantHost)-1]
	}
	if !strings.HasPrefix(owner, wantHost) {
		t.Fatalf("the derived lease owner does not start with the rune-safe truncation of the host")
	}
}

// TestNewServerCarriesTheTransportPosture pins the fields both listeners are
// built with. The WriteTimeout assertion is the one that matters: the runtime's
// inference contract is a Server-Sent Events stream, and a write deadline would
// kill a long answer mid-flight — so its zero value is the decision itself, and
// an editor who adds one meets this red test instead of a silent regression.
func TestNewServerCarriesTheTransportPosture(t *testing.T) {
	cfg := config.Config{ReadHeaderTimeout: 7 * time.Second}
	server := newServer(stdhttp.HandlerFunc(func(_ stdhttp.ResponseWriter, _ *stdhttp.Request) {}), cfg)

	if server.Handler == nil {
		t.Error("newServer built a server without a handler")
	}
	if server.ReadHeaderTimeout != cfg.ReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %s, want the configured %s", server.ReadHeaderTimeout, cfg.ReadHeaderTimeout)
	}
	if server.ReadTimeout != readTimeout {
		t.Errorf("ReadTimeout = %s, want %s", server.ReadTimeout, readTimeout)
	}
	if server.IdleTimeout != idleTimeout {
		t.Errorf("IdleTimeout = %s, want %s", server.IdleTimeout, idleTimeout)
	}
	if server.MaxHeaderBytes != maxHeaderBytes {
		t.Errorf("MaxHeaderBytes = %d, want %d", server.MaxHeaderBytes, maxHeaderBytes)
	}
	if server.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %s, want unset — a write deadline would kill a streamed answer mid-flight", server.WriteTimeout)
	}
}

// recordingPool stands in for the database pool run closes on its way out, and
// records that the close happened — a process that drained its listeners and
// left its pool open would hold database connections past the point the
// process was told to stop. closeErr is what Close answers, so a test can
// make the close fail and hold run to its promise of reporting it.
type recordingPool struct {
	closed   bool
	closeErr error
}

func (p *recordingPool) Close() error {
	p.closed = true
	return p.closeErr
}

func TestRunDrainsAnInFlightRequestBeforeReturning(t *testing.T) {
	listener := listenForTest(t)
	server, started, release := blockingServer(t)
	defer release()
	pool := &recordingPool{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := serveForTest(t, ctx, cancel, server, listener, time.Second, time.Second, pool)
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
	if !pool.closed {
		t.Error("run() left the pool open after returning — the pool must be closed on every path out")
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
	pool := &recordingPool{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, cancel, []service{
			{name: "runtime", server: runtimeServer, listener: runtimeListener},
			{name: "management", server: managementServer, listener: managementListener},
		}, time.Second, time.Second, pool)
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
	if !pool.closed {
		t.Error("run() left the pool open after returning — the pool must be closed on every path out")
	}
}

// TestRunExitsGreenAfterAnOverranDrain: the drain deadline ran out with a
// request still in flight. Shutdown does not force that connection closed —
// it reports the expiry and leaves it running — so run's policy is explicit:
// the overrun is named, the sockets are closed, the in-flight endings are
// given their bounded tail, and the process still exits green. The tail is
// real: run returns no earlier than it. The request the shutdown severed
// learns of it as the failed connection it now is — the caller was going to
// be answered by a process that has been asked to leave, and the ending that
// matters (the hold, the usage) runs detached from the socket.
func TestRunExitsGreenAfterAnOverranDrain(t *testing.T) {
	listener := listenForTest(t)
	server, started, release := blockingServer(t)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &recordingPool{}
	// The drain timeout is far shorter than the request's own lifetime, so
	// the shutdown is guaranteed to overrun it; the tail is long enough to
	// be measurable and far shorter than the test's patience.
	drainTail := 150 * time.Millisecond
	runErr := serveForTest(t, ctx, cancel, server, listener, 50*time.Millisecond, drainTail, pool)
	requestErr := requestForTest(t, listener)

	waitForStart(t, started)
	stoppedAt := time.Now()
	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run() error = %v, want nil — an overran drain is policy, not failure", err)
		}
		if elapsed := time.Since(stoppedAt); elapsed < drainTail {
			t.Fatalf("run() returned %s after the stop, want at least the %s tail the endings were granted", elapsed, drainTail)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() never returned after the shutdown overran its timeout")
	}
	if !pool.closed {
		t.Error("run() left the pool open after returning — an overrun drain is still a return")
	}

	// The request the force-close severed: its connection died with the
	// sockets, which is the policy — the caller of a process mid-exit does
	// not get an answer, and the walk's ending ran on without the socket.
	if err := <-requestErr; err == nil {
		t.Fatal("the severed request error = nil, want the connection's failure")
	}
}

func TestRunReportsAListenerThatFailsOnItsOwn(t *testing.T) {
	listener := listenForTest(t)
	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close() error = %v", err)
	}
	pool := &recordingPool{}

	server := &stdhttp.Server{Handler: stdhttp.HandlerFunc(func(_ stdhttp.ResponseWriter, _ *stdhttp.Request) {})}
	err := run(context.Background(), func() {}, []service{{name: "runtime", server: server, listener: listener}}, time.Second, time.Second, pool)

	if err == nil {
		t.Fatal("run() error = nil, want the listener failure")
	}
	if strings.Contains(err.Error(), "graceful shutdown") {
		t.Errorf("run() error = %q, want the listener failure rather than a drain report", err)
	}
	if !pool.closed {
		t.Error("run() left the pool open after returning — a dead listener is still a return")
	}
}

func TestRunReportsAPoolThatWillNotCloseCleanly(t *testing.T) {
	// A pool that fails to close after an otherwise clean drain is a defect
	// the process must not exit green through: run reports it even though
	// nothing else went wrong — the sibling composition root's contract, so
	// an operator never sees exit 0 from a process whose database
	// connections are still open.
	listener := listenForTest(t)
	pool := &recordingPool{closeErr: errors.New("close: still busy")}

	server := &stdhttp.Server{Handler: stdhttp.HandlerFunc(func(_ stdhttp.ResponseWriter, _ *stdhttp.Request) {})}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, stop, []service{{name: "runtime", server: server, listener: listener}}, time.Second, time.Second, pool)
	}()

	stop()
	if err := <-done; err == nil {
		t.Fatal("run() error = nil, want the pool-close failure carried out of a clean drain")
	} else if !strings.Contains(err.Error(), "close: still busy") {
		t.Errorf("run() error = %q, want it to carry the close failure", err)
	}
	if !pool.closed {
		t.Error("run() never closed the pool")
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
func serveForTest(t *testing.T, ctx context.Context, stop context.CancelFunc, server *stdhttp.Server, listener net.Listener, timeout, drainTail time.Duration, pool io.Closer) chan error {
	t.Helper()
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, stop, []service{{name: "runtime", server: server, listener: listener}}, timeout, drainTail, pool)
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
