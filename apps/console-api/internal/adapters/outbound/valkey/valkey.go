// Package valkey owns the console-api's connection lifecycle to a
// Redis-compatible store.
//
// The package is named for the server, which is Valkey today — the local
// deployment record (deploy/redis/README.md) explains the decision. The client
// speaks plain RESP either way, and the command set used here — PING, SELECT,
// AUTH, GET, SET, DEL — predates both forks. This is the single package under
// internal/ that imports a store client; everything else reaches the store
// through the interfaces it returns, named for what they do
// (internal/ports/outbound/cache).
//
// Nothing wires this into the Control Plane API yet, by design: the package is the
// foundation issue #7 asked for, and the pull request that first consumes it
// decides connect-at-boot versus lazy, and how Health feeds /readyz. Until
// then its integration tests are what keeps it honest.
//
// The runtime has its own copy of this adapter in its own module, with its own
// keyspace prefix: the two planes may share one server, and a key written by
// one must never be readable as the other's (ADR 0006 §7).
package valkey

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/valkey-io/valkey-go"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/cache"
)

// Client owns the connection to the store: one instance, one server, built
// from a Config and closed exactly once at shutdown. Its methods are safe for
// concurrent use — the client library multiplexes every caller's commands
// over the bounded set of connections described by Config.PipelineMultiplex.
type Client struct {
	client    valkey.Client
	closeOnce sync.Once
}

// Connect builds the client described by cfg and verifies it with one health
// probe bounded by ctx. Verification is part of connection establishment, not
// a separate step: a "connection" that cannot answer a PING is not one, and
// the caller learns that here rather than on its first real command.
//
// Connect failing does not by itself decide whether the process should stop —
// that policy belongs to the wiring, which may treat the error as fatal at
// boot or as a degraded mode to retry. On failure the underlying connections
// are released before the error is returned.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("valkey: invalid config: %w", err)
	}
	client, err := valkey.NewClient(cfg.clientOption())
	if err != nil {
		return nil, fmt.Errorf("valkey: connect %s: %w", cfg.Address, err)
	}
	c := &Client{client: client}
	if err := c.Health(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("valkey: connect %s: %w", cfg.Address, err)
	}
	return c, nil
}

// Health reports whether the store is usable right now, with one PING round
// trip bound to ctx. One probe, no retries, no cached verdict: a health check
// that answers from memory is a check of nothing. This is the signal a future
// /readyz wiring will consume — until that endpoint's owner picks it up, it is
// exercised by this package's tests.
func (c *Client) Health(ctx context.Context) error {
	if err := c.client.Do(ctx, c.client.B().Ping().Build()).Error(); err != nil {
		return fmt.Errorf("valkey: ping: %w", err)
	}
	return nil
}

// Close releases every connection the client holds. It is safe to call more
// than once; callers coordinate it with their own request-draining shutdown
// policy before invoking it.
func (c *Client) Close() {
	c.closeOnce.Do(c.client.Close)
}

// Cache returns the cache.Cache served by this connection. The returned value
// is namespaced under the keyspace policy in deploy/redis/README.md, which
// every application under apps/ prefixes with its own name; see cache.go in
// this package for the implementation.
func (c *Client) Cache() cache.Cache {
	return storeCache{client: c.client}
}

// clientOption maps Config onto the client library's option struct. Every
// field the library would default is set from Config so the mapping is total
// and visible; ConnTimeout passes through unchanged to ConnWriteTimeout, the
// library's single read/write deadline per connection, and DisableCache turns
// off the library's opt-in client-side response cache, which no code here or
// planned uses — off is the smaller memory footprint and one less thing to
// reason about under invalidations.
func (c Config) clientOption() valkey.ClientOption {
	return valkey.ClientOption{
		InitAddress:       []string{c.Address},
		Username:          c.Username,
		Password:          c.Password,
		SelectDB:          c.Database,
		Dialer:            net.Dialer{Timeout: c.DialTimeout},
		ConnWriteTimeout:  c.ConnTimeout,
		PipelineMultiplex: c.PipelineMultiplex,
		BlockingPoolSize:  c.BlockingPoolSize,
		DisableCache:      true,
	}
}
