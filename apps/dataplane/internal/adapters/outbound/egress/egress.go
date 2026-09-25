// Package egress implements the outbound dial port: the ways a connection
// leaves this process on its way to a provider. The shapes are the four the
// configuration can name — direct, an HTTP CONNECT tunnel, SOCKS5 with local
// name resolution, SOCKS5 with resolution left to the proxy — and one policy
// wrapper that carries an ordered route list over any of them.
//
// The layer's law is establishment. A dial either produces a working
// connection or an error; failover between routes happens while the
// connection is being established and never after, because re-routing a
// conversation in progress would mean a second party silently finishing a
// sentence a third one started. What a route's repeated failures earn is not
// a retry but a cooldown: the policy prefers its healthy routes and lets a
// struggling one recover in the background, which is the whole of the
// rotation model this package carries over from the organisation's proxy
// gateway — ordered preference, establishment-only failover, per-route
// backoff — with none of the request-path machinery that would make egress
// visible above the adapter boundary.
package egress

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/egress"
)

// Direct returns the plain dialer: the connection goes out this process's
// own interfaces, through whatever routing the host already has. It is the
// policy every backend gets when its egress reference is empty, and the
// route every other policy falls back to when configuration says so.
func Direct() egress.DialFunc {
	var dialer net.Dialer
	return dialer.DialContext
}

// Connect returns a dialer that tunnels through an HTTP CONNECT proxy: it
// dials the proxy, asks it — with the CONNECT method the HTTP tunnelling
// convention defines — to extend the connection to the caller's destination,
// and returns the tunnel once the proxy answers 2xx. The handshake is
// written at the wire's own level rather than through an HTTP client,
// because the tunnel carries the executor's conversation untouched and no
// request semantics above the status line are this layer's business.
//
// A proxy refusal is an establishment failure: the connection is closed and
// the error carries the status the proxy gave, for the policy above to treat
// like any other route that would not carry the call.
func Connect(proxyAddr string) egress.DialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialProxy(ctx, proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("egress: connect proxy %s: %w", proxyAddr, err)
		}
		if err := establishConnect(ctx, conn, address); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("egress: connect proxy %s: %w", proxyAddr, err)
		}
		return conn, nil
	}
}

// SOCKS5 returns a dialer that connects through a SOCKS5 proxy (RFC 1928).
// The remoteDNS spelling is the configuration's, not the protocol library's:
// with it set, the destination hostname travels to the proxy unresolved and
// the proxy resolves it — the socks5h form, the right default when the
// resolver this process sees is not the one the route trusts; with it clear,
// the name is resolved here and only the address travels, the socks5 form.
//
// The proxy library's dialer must be used through its context-aware
// interface — the context-blind shape keeps dialling in a goroutine after
// the caller has gone, and an abandoned request must not leave a dial
// running behind it. The library's dialer has had the context form since it
// grew context support; if that ever stops being true, the constructor
// refuses here rather than wiring a dialer that leaks.
func SOCKS5(proxyAddr string, remoteDNS bool) egress.DialFunc {
	inner, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	if err != nil {
		// The library's constructor fails on an unusable proxy address, which
		// configuration validation has already refused. Reaching this is a
		// wiring mistake at the composition root, not a runtime condition.
		panic(fmt.Sprintf("egress: build the socks5 dialer for %s: %v", proxyAddr, err))
	}
	contextDialer, ok := inner.(proxy.ContextDialer)
	if !ok {
		panic(fmt.Sprintf("egress: the socks5 dialer for %s lost its context-aware interface; dialing without it would leak goroutines on cancellation", proxyAddr))
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		target := address
		if !remoteDNS {
			resolved, err := resolveLocally(ctx, address)
			if err != nil {
				return nil, fmt.Errorf("egress: socks5 proxy %s: %w", proxyAddr, err)
			}
			target = resolved
		}
		conn, err := contextDialer.DialContext(ctx, network, target)
		if err != nil {
			return nil, fmt.Errorf("egress: socks5 proxy %s: %w", proxyAddr, err)
		}
		return conn, nil
	}
}

// resolveLocally resolves a host:port address with this process's resolver,
// leaving IP literals alone — the socks5 spelling's whole difference is that
// the proxy sees an address, never a name.
func resolveLocally(ctx context.Context, address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", address, err)
	}
	if ip := net.ParseIP(host); ip != nil {
		return address, nil
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("resolve %q: no addresses", host)
	}
	return net.JoinHostPort(addrs[0], port), nil
}

// dialProxy opens the TCP connection to a proxy itself, the one hop every
// tunnelling route shares.
func dialProxy(ctx context.Context, proxyAddr string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	// The handshake must not outlive the dial's own patience: a proxy that
	// accepts and never answers would hold the request forever, so the
	// context's deadline — the execution budget, when the caller set one —
	// becomes the handshake's.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	return conn, nil
}

// establishConnect speaks the CONNECT handshake over an open proxy
// connection: the request names the caller's destination, and the response
// is read only as far as the status line and its headers — the bytes after
// the blank line belong to the tunnel's payload, and this layer must not
// read one byte of them.
func establishConnect(ctx context.Context, conn net.Conn, address string) error {
	request := "CONNECT " + address + " HTTP/1.1\r\nHost: " + address + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		return fmt.Errorf("write the CONNECT request: %w", err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read the CONNECT response: %w", err)
	}
	code, ok := connectStatus(status)
	if !ok {
		return fmt.Errorf("unreadable CONNECT response status %q", strings.TrimRight(status, "\r\n"))
	}
	if code < 200 || code > 299 {
		return fmt.Errorf("the proxy refused with status %d", code)
	}
	// The headers between the status line and the blank line are drained,
	// never interpreted: whatever a proxy annotates its success with is its
	// own affair, and the tunnel starts at the blank line.
	for headers := 0; ; headers++ {
		if headers >= 128 {
			return fmt.Errorf("the CONNECT response headers do not end")
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read the CONNECT response headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			return nil
		}
	}
}

// connectStatus reads the one number the handshake needs out of a status
// line — "HTTP/1.1 200 Connection established" — and nothing else. A line
// that is not an HTTP status at all is unreadable, which is a refusal by
// another name.
func connectStatus(status string) (int, bool) {
	parts := strings.Split(status, " ")
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, false
	}
	return code, true
}

// Policy is an ordered route list over any dialers configuration resolved:
// the first healthy route carries the call, a route that fails to establish
// earns a cooldown that doubles with each consecutive failure up to a cap,
// and a route whose cooldown is running is passed over — unless every route
// is cooling, in which case the list is tried in plain order anyway, because
// a cooldown is a preference about where to try first, never a refusal to
// try at all.
//
// The policy's judgment ends at establishment. It returns a connection or
// an error; what happens on the connection afterwards belongs to the
// conversation that owns it, and no failure there is seen here.
type Policy struct {
	mu       sync.Mutex
	routes   []policyRoute
	cooldown time.Duration
	// now is the policy's clock, injectable so the cooldown arithmetic is a
	// test's to step through rather than a sleep's to guess at.
	now func() time.Time
}

// policyRoute is one route and what the policy remembers about it: the
// consecutive establishment failures since it last carried a call, and the
// instant its current cooldown runs out.
type policyRoute struct {
	dial      egress.DialFunc
	failures  int
	coolUntil time.Time
}

// maxCooldownFactor is how far one route's backoff may grow past the base
// cooldown — sixteen doublings' worth of room for a route that stays sick,
// and a hard ceiling so a flapping route's absence stays bounded.
const maxCooldownFactor = 16

// NewPolicy builds the policy over the routes in the order they are given.
// The cooldown is the base each route's backoff doubles from; it must be
// positive, and a non-positive one is a wiring mistake at the composition
// root rather than a runtime condition.
func NewPolicy(routes []egress.DialFunc, cooldown time.Duration) *Policy {
	if cooldown <= 0 {
		panic("egress: the policy cooldown must be greater than zero")
	}
	if len(routes) == 0 {
		panic("egress: the policy needs at least one route")
	}
	policy := &Policy{
		cooldown: cooldown,
		now:      time.Now,
	}
	for _, dial := range routes {
		policy.routes = append(policy.routes, policyRoute{dial: dial})
	}
	return policy
}

// Dial tries the policy's routes in preference order and returns the first
// connection that establishes. Each establishment failure cools its route
// and moves the attempt on; a context that finishes mid-list stops the list,
// because a caller that has gone is nobody to keep dialling for.
func (p *Policy) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	p.mu.Lock()
	order := p.preference()
	p.mu.Unlock()

	var firstErr error
	for _, index := range order {
		if err := ctx.Err(); err != nil {
			if firstErr == nil {
				return nil, fmt.Errorf("egress: every route failed to establish: %w", err)
			}
			return nil, fmt.Errorf("egress: %w (first route error: %v)", err, firstErr)
		}
		conn, err := p.routes[index].dial(ctx, network, address)
		if err == nil {
			p.established(index)
			return conn, nil
		}
		p.failed(index)
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, fmt.Errorf("egress: no route of %d established the connection (first error: %w)", len(p.routes), firstErr)
}

// preference is the order the routes are tried in: the ones not cooling, in
// list order, followed by the cooling ones in list order — so a policy whose
// every route is cooling still dials, and still prefers the head of its list.
// The caller holds the lock.
func (p *Policy) preference() []int {
	now := p.now()
	healthy := make([]int, 0, len(p.routes))
	cooling := make([]int, 0, len(p.routes))
	for index, route := range p.routes {
		if now.Before(route.coolUntil) {
			cooling = append(cooling, index)
			continue
		}
		healthy = append(healthy, index)
	}
	return append(healthy, cooling...)
}

// established records that a route carried a call: its failure count and
// its cooldown both go back to zero, whatever stricken history preceded it.
func (p *Policy) established(index int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes[index].failures = 0
	p.routes[index].coolUntil = time.Time{}
}

// failed records an establishment failure: the route's backoff doubles from
// the base cooldown with each consecutive failure and saturates at the cap,
// so a route that stays sick is retried at a pace that stops accelerating.
func (p *Policy) failed(index int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	route := &p.routes[index]
	route.failures++
	delay := p.cooldown
	for factor := 1; factor < route.failures && factor < maxCooldownFactor; factor++ {
		delay *= 2
	}
	if delay > p.cooldown*maxCooldownFactor {
		delay = p.cooldown * maxCooldownFactor
	}
	route.coolUntil = p.now().Add(delay)
}
