// Package egress names the one thing the runtime needs from the world below
// its provider adapters: a connection dialled out of this process. Which
// proxies that connection passes through, in which order, and what happens
// when one refuses to carry it are decisions of the egress layer — invisible
// to routing, translation and billing, exactly as ADR 0002 puts them below
// the adapter boundary.
//
// The port is a function type and nothing else, deliberately. A dial is the
// whole contract: callers hand in where they mean to reach and get back a
// connection or an error, and every policy — direct, a CONNECT proxy, a
// SOCKS5 pool with failover — is expressible as one. Anything richer would
// be transport vocabulary leaking upward: the executors above this port
// speak HTTP to their providers, and this port's job is to make the hops in
// between somebody else's problem.
package egress

import (
	"context"
	"net"
)

// DialFunc dials one connection out of this process. The network and address
// are the caller's destination — the provider endpoint an executor means to
// reach, spelled as the transport expects it ("tcp", "host:port") — and the
// connection returned is already established end to end: whatever proxies
// the implementation was configured to pass through have been negotiated
// before the call returns, and nothing of that negotiation is visible on
// the connection's use.
//
// Establishment is the port's whole promise. Once the connection is
// returned, its failures are the caller's — a cut mid-conversation is the
// provider conversation's own ending, and no egress policy retries or
// re-routes a conversation in progress. A DialFunc must also honour ctx:
// a cancelled context returns promptly, because an abandoned request must
// not keep paying for the dial it was abandoning.
//
// The implementations live in the outbound adapters; the composition root
// builds them from configuration and hands them, already resolved, to the
// executor adapters that use them.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)
