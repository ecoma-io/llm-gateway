// Package cache is the gateway's boundary for ephemeral, TTL-scoped state.
//
// It is what consuming code sees of shared short-lived data: an interface
// named for what it does, not for the store behind it. The Redis-compatible
// implementation lives in internal/infra/redis — the one auditable site of a
// store-client import — and nothing here knows which server, client library,
// or protocol version serves the bytes. The local Redis deployment record
// (deploy/redis/README.md) states what may live in that store and what may
// never.
//
// Deliberately absent: a lease/lock (its fencing and renewal semantics cannot
// be designed without the consumer that needs them), counter or add-if-absent
// operations (rate limiting is not cache-shaped), and any serialisation layer
// (values are bytes; encoding is the caller's concern). Each arrives, if ever,
// with the consumer that justifies it.
package cache

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound reports a key that is not present. It means a miss and only a
// miss: an unreachable store, a failed connection, or a timed-out round trip
// is never flattened into it. Keeping the two distinguishable is what lets a
// consumer make the one decision a cache forces — fail open (treat an outage
// as a miss) or fail closed — from the error it actually received.
var ErrNotFound = errors.New("cache: key not found")

// ErrInvalidTTL reports a non-positive time-to-live. An entry without an
// expiry is an immortal key inside a store that is wiped on every restart —
// a bug an unset configuration field could trigger silently — so the zero
// value of time.Duration is rejected rather than quietly meaning "forever".
// The day a consumer needs entries that outlive their process is the day
// this decision is revisited, with that consumer in the room.
var ErrInvalidTTL = errors.New("cache: ttl must be positive")

// Cache is a shared, TTL-scoped key/value store.
//
// Keys are logical names scoped to this abstraction: implementations may
// namespace them in the backing store (the keyspace policy in the ADR), so a
// key passed here is not promised to be the key stored there. Values are
// opaque bytes.
//
// Every method binds ctx to the underlying round trip, so a stalled store
// stalls the caller no longer than the context allows.
type Cache interface {
	// Set stores value under key for ttl, replacing any previous entry.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Get returns the value stored under key, or ErrNotFound when the key is
	// absent or expired. Any other error is a store failure, and is kept
	// distinguishable from a miss.
	Get(ctx context.Context, key string) ([]byte, error)

	// Delete removes key. Deleting an absent key is not an error: the state
	// after the call is exactly the state that was asked for.
	Delete(ctx context.Context, key string) error
}
