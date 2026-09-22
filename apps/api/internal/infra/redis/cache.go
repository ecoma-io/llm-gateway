package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/ecoma-io/llm-gateway/apps/api/internal/cache"
)

// keyPrefix is the keyspace namespace of this abstraction, per ADR 0001: keys
// this package writes are stored prefixed, so a future subsystem in the same
// database cannot collide with them and its keys cannot collide with this
// one. The prefix lives here, not in the caller's view of a key.
const keyPrefix = "cache:"

// storeCache implements cache.Cache over the shared client. It is served
// through Client.Cache() so that callers hold an interface named for what it
// does and never import the client library.
type storeCache struct {
	client valkey.Client
}

// Set stores value under the namespaced key with a PX expiry. The TTL is
// checked here rather than trusted to the wire: a non-positive duration would
// otherwise send SET without an expiry, silently creating an immortal key.
func (s storeCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return cache.ErrInvalidTTL
	}
	cmd := s.client.B().Set().Key(keyPrefix + key).Value(valkey.BinaryString(value)).Px(ttl).Build()
	if err := s.client.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("redis: set %q: %w", key, err)
	}
	return nil
}

// Get returns the stored value or cache.ErrNotFound. The nil-reply check runs
// on the error the library returned, before any wrapping: the library
// identifies a miss by identity, not by errors.Is, so wrapping first would
// turn misses into store failures.
func (s storeCache) Get(ctx context.Context, key string) ([]byte, error) {
	cmd := s.client.B().Get().Key(keyPrefix + key).Build()
	value, err := s.client.Do(ctx, cmd).AsBytes()
	if valkey.IsValkeyNil(err) {
		return nil, cache.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("redis: get %q: %w", key, err)
	}
	return value, nil
}

// Delete removes the namespaced key. DEL of an absent key reports zero
// deletions and no error, which is exactly the post-state Delete promises.
func (s storeCache) Delete(ctx context.Context, key string) error {
	cmd := s.client.B().Del().Key(keyPrefix + key).Build()
	if err := s.client.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("redis: delete %q: %w", key, err)
	}
	return nil
}
