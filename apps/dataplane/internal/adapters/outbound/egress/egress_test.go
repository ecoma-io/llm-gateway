package egress

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/egress"
)

// startEchoListener runs the "provider" of these tests: a TCP listener that
// answers every connection by echoing what it receives. The egress layer's
// wire tests do not need a real provider behind the tunnel — they need a far
// end that proves the payload survives the handshake untouched.
func startEchoListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}(conn)
		}
	}()
	return listener.Addr().String()
}

// roundTrip writes a marker into a dialled connection and expects it echoed,
// the proof that the returned connection carries payload and not handshake
// debris.
func roundTrip(t *testing.T, conn net.Conn) {
	t.Helper()
	const marker = "payload-marker"
	if _, err := conn.Write([]byte(marker)); err != nil {
		t.Fatalf("writing the marker: %v", err)
	}
	got := make([]byte, len(marker))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("reading the echo: %v", err)
	}
	if string(got) != marker {
		t.Errorf("connection echoed %q, want %q — the payload did not survive", got, marker)
	}
}

// TestDirectDialsOutAndCarriesThePayload is the control case: the plain
// dialer reaches a listener and the connection is the conversation's.
func TestDirectDialsOutAndCarriesThePayload(t *testing.T) {
	addr := startEchoListener(t)

	conn, err := Direct()(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("Direct() dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	roundTrip(t, conn)
}

// startConnectProxy runs a CONNECT proxy fake. It records the request lines
// it received — the test asserts the request names the caller's destination
// and nothing exotic — and then answers with the status it was given. A 2xx
// turns the connection into an echo; a refusal closes it.
func startConnectProxy(t *testing.T, status string) (addr string, requests <-chan []string) {
	t.Helper()
	seen := make(chan []string, 4)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				reader := bufio.NewReader(conn)
				var lines []string
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					trimmed := strings.TrimRight(line, "\r\n")
					if trimmed == "" {
						break
					}
					lines = append(lines, trimmed)
				}
				seen <- lines
				if _, err := conn.Write([]byte(status)); err != nil {
					return
				}
				_, _ = io.Copy(conn, conn)
			}(conn)
		}
	}()
	return listener.Addr().String(), seen
}

// TestConnectEstablishesTheTunnelAndLeavesThePayloadAlone drives the whole
// wire shape the implementation promises: the request is CONNECT and the
// destination, the success answer is read only as far as its blank line, and
// the tunnel from there is the caller's payload, byte for byte.
func TestConnectEstablishesTheTunnelAndLeavesThePayloadAlone(t *testing.T) {
	addr, requests := startConnectProxy(t, "HTTP/1.1 200 Connection established\r\n\r\n")
	target := startEchoListener(t)

	conn, err := Connect(addr)(context.Background(), "tcp", target)
	if err != nil {
		t.Fatalf("Connect() dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	roundTrip(t, conn)

	select {
	case lines := <-requests:
		if len(lines) == 0 {
			t.Fatal("the proxy saw no request line")
		}
		if lines[0] != "CONNECT "+target+" HTTP/1.1" {
			t.Errorf("request line = %q, want %q", lines[0], "CONNECT "+target+" HTTP/1.1")
		}
		host := false
		for _, line := range lines[1:] {
			if strings.HasPrefix(line, "Host: ") {
				host = true
			}
		}
		if !host {
			t.Errorf("request lines = %v, want a Host header among them", lines)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the proxy never saw a CONNECT request")
	}
}

// TestConnectRefusalIsAnEstablishmentFailure: a proxy that says no produces
// a dial error carrying the refusal, and the payload never flows.
func TestConnectRefusalIsAnEstablishmentFailure(t *testing.T) {
	addr, _ := startConnectProxy(t, "HTTP/1.1 403 Forbidden\r\n\r\n")

	conn, err := Connect(addr)(context.Background(), "tcp", startEchoListener(t))
	if err == nil {
		_ = conn.Close()
		t.Fatal("Connect() against a refusing proxy error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("Connect() error = %v, want it to carry the refusal status", err)
	}
}

// socksRequest is what the SOCKS fake saw the client ask for, decoded.
type socksRequest struct {
	cmd    byte
	atyp   byte
	host   string
	port   int
	marker []byte // the first payload bytes after the success reply, once they arrive
}

// startSOCKSProxy runs a SOCKS5 (RFC 1928) fake: it greets method 0x00,
// records the decoded request, answers success with a zero bound address,
// and then echoes the payload — so a test can assert both the address form
// that travelled (domain for socks5h, packed IP for socks5) and that the
// tunnel carries bytes afterwards.
func startSOCKSProxy(t *testing.T) (addr string, requests <-chan socksRequest) {
	t.Helper()
	seen := make(chan socksRequest, 4)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				greeting := make([]byte, 3)
				if _, err := io.ReadFull(conn, greeting); err != nil {
					return
				}
				if greeting[0] != 0x05 {
					return
				}
				if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
					return
				}
				header := make([]byte, 4)
				if _, err := io.ReadFull(conn, header); err != nil {
					return
				}
				req := socksRequest{cmd: header[1], atyp: header[3]}
				switch header[3] {
				case 0x01:
					ip := make([]byte, 4)
					if _, err := io.ReadFull(conn, ip); err != nil {
						return
					}
					req.host = net.IP(ip).String()
				case 0x03:
					length := make([]byte, 1)
					if _, err := io.ReadFull(conn, length); err != nil {
						return
					}
					name := make([]byte, length[0])
					if _, err := io.ReadFull(conn, name); err != nil {
						return
					}
					req.host = string(name)
				case 0x04:
					ip := make([]byte, 16)
					if _, err := io.ReadFull(conn, ip); err != nil {
						return
					}
					req.host = net.IP(ip).String()
				default:
					return
				}
				portBytes := make([]byte, 2)
				if _, err := io.ReadFull(conn, portBytes); err != nil {
					return
				}
				req.port = int(binary.BigEndian.Uint16(portBytes))
				seen <- req
				// Success: BND.ADDR all zeroes, the far end the payload's judge.
				if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
					return
				}
				const marker = "payload-marker"
				buf := make([]byte, len(marker))
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				if _, err := conn.Write(buf[:n]); err != nil {
					return
				}
				req.marker = buf[:n]
				seen <- req
			}(conn)
		}
	}()
	return listener.Addr().String(), seen
}

func awaitRequest(t *testing.T, requests <-chan socksRequest) socksRequest {
	t.Helper()
	select {
	case req := <-requests:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("the SOCKS proxy never saw a request")
		return socksRequest{}
	}
}

// TestSOCKS5WithProxyResolutionSendsTheNameUnresolved is the socks5h
// spelling: the hostname travels as a domain entry, and the proxy — never
// this process — learns what it means.
func TestSOCKS5WithProxyResolutionSendsTheNameUnresolved(t *testing.T) {
	addr, requests := startSOCKSProxy(t)
	const name = "provider.example.invalid"
	const port = 993

	conn, err := SOCKS5(addr, true)(context.Background(), "tcp", name+":"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("SOCKS5() dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	roundTrip(t, conn)

	req := awaitRequest(t, requests)
	if req.cmd != 0x01 {
		t.Errorf("request cmd = %#x, want CONNECT (0x01)", req.cmd)
	}
	if req.atyp != 0x03 {
		t.Errorf("request ATYP = %#x, want a domain name (0x03)", req.atyp)
	}
	if req.host != name {
		t.Errorf("request domain = %q, want %q — the name must travel unresolved", req.host, name)
	}
	if req.port != port {
		t.Errorf("request port = %d, want %d", req.port, port)
	}
}

// TestSOCKS5WithLocalResolutionSendsThePackedAddress is the socks5 spelling
// over an IP literal: resolution is a no-op and the address travels packed,
// four bytes of IPv4.
func TestSOCKS5WithLocalResolutionSendsThePackedAddress(t *testing.T) {
	addr, requests := startSOCKSProxy(t)
	echo := startEchoListener(t)
	host, portText, err := net.SplitHostPort(echo)
	if err != nil {
		t.Fatalf("splitting the echo address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("reading the echo port: %v", err)
	}

	conn, err := SOCKS5(addr, false)(context.Background(), "tcp", net.JoinHostPort(host, portText))
	if err != nil {
		t.Fatalf("SOCKS5() dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	roundTrip(t, conn)

	req := awaitRequest(t, requests)
	if req.atyp != 0x01 {
		t.Errorf("request ATYP = %#x, want an IPv4 address (0x01)", req.atyp)
	}
	if req.host != host {
		t.Errorf("request address = %q, want %q", req.host, host)
	}
	if req.port != port {
		t.Errorf("request port = %d, want %d", req.port, port)
	}
}

// TestSOCKS5WithLocalResolutionResolvesTheNameHere drives the difference the
// two spellings exist for: with local resolution, "localhost" becomes an
// address before the wire — the proxy sees no name to resolve.
func TestSOCKS5WithLocalResolutionResolvesTheNameHere(t *testing.T) {
	addr, requests := startSOCKSProxy(t)
	echo := startEchoListener(t)
	_, portText, err := net.SplitHostPort(echo)
	if err != nil {
		t.Fatalf("splitting the echo address: %v", err)
	}

	conn, err := SOCKS5(addr, false)(context.Background(), "tcp", "localhost:"+portText)
	if err != nil {
		t.Fatalf("SOCKS5() dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	roundTrip(t, conn)

	req := awaitRequest(t, requests)
	if req.atyp != 0x01 {
		t.Errorf("request ATYP = %#x, want a resolved IPv4 address (0x01) — the name must not travel", req.atyp)
	}
	ip := net.ParseIP(req.host)
	if ip == nil || !ip.IsLoopback() {
		t.Errorf("request address = %q, want a loopback address", req.host)
	}
}

// dialScript is a route for the policy tests: it records each call in the
// test's shared log — so the interleaved order the policy tried them in
// survives — fails with err (nil = succeed), and keeps the far end of every
// pipe it handed out so a test can prove which route's connection came back.
type dialScript struct {
	name  string
	err   error
	log   *[]string
	conns []net.Conn
}

func (s *dialScript) dial(ctx context.Context, network, address string) (net.Conn, error) {
	*s.log = append(*s.log, s.name)
	if s.err != nil {
		return nil, s.err
	}
	near, far := net.Pipe()
	s.conns = append(s.conns, far)
	return near, nil
}

func (s *dialScript) fail(err error) { s.err = err }

// newScript builds one route over a test's shared call log.
func newScript(log *[]string, name string, err error) *dialScript {
	return &dialScript{name: name, err: err, log: log}
}

// TestPolicyTriesRoutesInOrderAndStopsAtTheFirstEstablished: the list order
// is the preference, and a success ends the walk — with the connection that
// comes back provably the route that made it.
func TestPolicyTriesRoutesInOrderAndStopsAtTheFirstEstablished(t *testing.T) {
	calls := []string{}
	a := newScript(&calls, "a", errors.New("a refused"))
	b := newScript(&calls, "b", nil)
	policy := NewPolicy([]egress.DialFunc{a.dial, b.dial}, time.Second)

	conn, err := policy.Dial(context.Background(), "tcp", "provider.example.invalid:443")
	if err != nil {
		t.Fatalf("Policy.Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if got := calls; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("route order = %v, want [a b]", got)
	}

	// The connection that came back is b's. The route's connection is an
	// in-memory pipe, whose write blocks until its far end reads — so the
	// write runs while the test reads on b's end.
	const marker = "which-route"
	written := make(chan error, 1)
	go func() { _, err := conn.Write([]byte(marker)); written <- err }()
	got := make([]byte, len(marker))
	if _, err := io.ReadFull(b.conns[0], got); err != nil {
		t.Fatalf("reading on the far end: %v", err)
	}
	if err := <-written; err != nil {
		t.Fatalf("writing: %v", err)
	}
	if string(got) != marker {
		t.Errorf("far end read %q, want %q — the connection is not b's", got, marker)
	}
}

// TestPolicyPassesOverACoolingRoute: a route that just failed is passed over
// while its cooldown runs, and tried first again once it has expired.
func TestPolicyPassesOverACoolingRoute(t *testing.T) {
	calls := []string{}
	a := newScript(&calls, "a", errors.New("a refused"))
	b := newScript(&calls, "b", nil)
	policy := NewPolicy([]egress.DialFunc{a.dial, b.dial}, time.Second)
	t0 := time.Unix(1_700_000_000, 0)
	policy.now = func() time.Time { return t0 }

	if _, err := policy.Dial(context.Background(), "tcp", "p:443"); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	if _, err := policy.Dial(context.Background(), "tcp", "p:443"); err != nil {
		t.Fatalf("second dial: %v", err)
	}
	if got := calls; len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "b" {
		t.Errorf("route order = %v, want [a b b] — the cooling route is passed over", got)
	}

	t0 = t0.Add(2 * time.Second) // past a's one-second cooldown
	if _, err := policy.Dial(context.Background(), "tcp", "p:443"); err != nil {
		t.Fatalf("third dial: %v", err)
	}
	if got := calls; len(got) != 5 || got[3] != "a" || got[4] != "b" {
		t.Errorf("route order = %v, want the expired route tried first again ([a b b a b])", got)
	}
}

// TestPolicyDialsCoolingRoutesWhenNothingIsHealthy: cooldown is a preference
// about where to try first, never a refusal to try at all.
func TestPolicyDialsCoolingRoutesWhenNothingIsHealthy(t *testing.T) {
	calls := []string{}
	a := newScript(&calls, "a", errors.New("a refused"))
	b := newScript(&calls, "b", errors.New("b refused"))
	policy := NewPolicy([]egress.DialFunc{a.dial, b.dial}, time.Second)
	t0 := time.Unix(1_700_000_000, 0)
	policy.now = func() time.Time { return t0 }

	if _, err := policy.Dial(context.Background(), "tcp", "p:443"); err == nil {
		t.Fatal("first dial error = nil, want every route failing")
	}
	if _, err := policy.Dial(context.Background(), "tcp", "p:443"); err == nil {
		t.Fatal("second dial error = nil, want the cooling routes dialled anyway")
	}
	if got := calls; len(got) != 4 || got[0] != "a" || got[1] != "b" || got[2] != "a" || got[3] != "b" {
		t.Errorf("route order = %v, want [a b a b] — all cooling, list order, still dialled", got)
	}
}

// TestPolicyBackoffDoublesAndSaturates walks the cooldown ladder: one second
// doubling to two, four, eight, and sixteen — and staying at sixteen.
func TestPolicyBackoffDoublesAndSaturates(t *testing.T) {
	calls := []string{}
	a := newScript(&calls, "a", errors.New("a refused"))
	b := newScript(&calls, "b", nil)
	policy := NewPolicy([]egress.DialFunc{a.dial, b.dial}, time.Second)
	t0 := time.Unix(1_700_000_000, 0)
	now := t0
	policy.now = func() time.Time { return now }

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 16 * time.Second}
	for step, delay := range want {
		if _, err := policy.Dial(context.Background(), "tcp", "p:443"); err != nil {
			t.Fatalf("step %d: %v", step+1, err)
		}
		coolUntil := policy.routes[0].coolUntil
		if got := coolUntil.Sub(now); got != delay {
			t.Errorf("after failure %d the cooldown is %v, want %v", step+1, got, delay)
		}
		now = coolUntil // exactly at expiry the route is healthy again and fails once more
	}
}

// TestPolicySuccessResetsTheBackoff: a route that carried a call has its
// stricken history forgiven — its next failure cools it by the base, not by
// the ladder it had climbed.
func TestPolicySuccessResetsTheBackoff(t *testing.T) {
	calls := []string{}
	a := newScript(&calls, "a", errors.New("a refused"))
	b := newScript(&calls, "b", nil)
	policy := NewPolicy([]egress.DialFunc{a.dial, b.dial}, time.Second)
	t0 := time.Unix(1_700_000_000, 0)
	now := t0
	policy.now = func() time.Time { return now }

	// Two failures put a two steps up the ladder.
	if _, err := policy.Dial(context.Background(), "tcp", "p:443"); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	now = t0.Add(time.Second)
	if _, err := policy.Dial(context.Background(), "tcp", "p:443"); err != nil {
		t.Fatalf("second dial: %v", err)
	}
	if got := policy.routes[0].coolUntil.Sub(now); got != 2*time.Second {
		t.Fatalf("after two failures the cooldown is %v, want 2s — the ladder did not climb", got)
	}

	// At the second failure's expiry a is tried again and succeeds — the
	// walk only reaches it once its cooldown has run out, so the success
	// happens exactly here.
	now = t0.Add(3 * time.Second)
	a.fail(nil)
	if _, err := policy.Dial(context.Background(), "tcp", "p:443"); err != nil {
		t.Fatalf("third dial: %v", err)
	}
	if got := calls; len(got) != 5 || got[4] != "a" {
		t.Fatalf("route order = %v, want the third dial to begin with a's success", got)
	}

	// The failure after the success starts from the base again.
	a.fail(errors.New("a refused again"))
	if _, err := policy.Dial(context.Background(), "tcp", "p:443"); err != nil {
		t.Fatalf("fourth dial: %v", err)
	}
	if got := policy.routes[0].coolUntil.Sub(now); got != time.Second {
		t.Errorf("after a success and one failure the cooldown is %v, want 1s — the success did not reset the ladder", got)
	}
}

// TestPolicyEveryRouteFailedCarriesTheFirstError: the error a caller sees
// when nothing establishes names the list's size and carries the head's
// failure, not whichever route happened to fail last.
func TestPolicyEveryRouteFailedCarriesTheFirstError(t *testing.T) {
	calls := []string{}
	a := newScript(&calls, "a", errors.New("a refused first"))
	b := newScript(&calls, "b", errors.New("b refused last"))
	policy := NewPolicy([]egress.DialFunc{a.dial, b.dial}, time.Second)

	_, err := policy.Dial(context.Background(), "tcp", "p:443")
	if err == nil {
		t.Fatal("Policy.Dial error = nil, want every route failing")
	}
	if !strings.Contains(err.Error(), "no route of 2") {
		t.Errorf("Policy.Dial error = %v, want it to name the route count", err)
	}
	if !strings.Contains(err.Error(), "a refused first") {
		t.Errorf("Policy.Dial error = %v, want it to carry the first route's failure", err)
	}
}

// TestPolicyStopsWhenTheContextIsDone: a caller that has gone is nobody to
// keep dialling for — the failing route cancels the context, and the walk
// must stop at the next route boundary with the context's own error.
func TestPolicyStopsWhenTheContextIsDone(t *testing.T) {
	calls := []string{}
	a := newScript(&calls, "a", errors.New("a refused"))
	b := newScript(&calls, "b", errors.New("b must not be dialled"))
	ctx, cancel := context.WithCancel(context.Background())
	cancelAfterA := func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := a.dial(ctx, network, address)
		cancel()
		return conn, err
	}
	neverB := func(ctx context.Context, network, address string) (net.Conn, error) {
		return b.dial(ctx, network, address)
	}
	policy := NewPolicy([]egress.DialFunc{cancelAfterA, neverB}, time.Second)

	_, err := policy.Dial(ctx, "tcp", "p:443")
	if err == nil {
		t.Fatal("Policy.Dial error = nil, want the context's")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Policy.Dial error = %v, want context.Canceled", err)
	}
	if got := calls; len(got) != 1 || got[0] != "a" {
		t.Errorf("routes dialled = %v, want only [a] — the walk must stop when the caller is gone", got)
	}
}

// TestPolicyRefusesBrokenWiring pins the constructor's two refusals: no
// routes to try and no base to back off from are wiring mistakes at the
// composition root, not runtime conditions.
func TestPolicyRefusesBrokenWiring(t *testing.T) {
	if !panics(func() { NewPolicy(nil, time.Second) }) {
		t.Error("NewPolicy(nil, ...) did not panic — a policy of no routes is a wiring mistake")
	}
	if !panics(func() {
		NewPolicy([]egress.DialFunc{func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("x") }}, 0)
	}) {
		t.Error("NewPolicy(routes, 0) did not panic — a zero cooldown is a wiring mistake")
	}
}

func panics(f func()) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	f()
	return false
}
