//go:build integration

package valkey

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/cache"
)

// Integration tests deliberately require an explicit server rather than
// starting Docker from go test: process tests that own a Docker socket are
// brittle (pull latency, daemon permissions, port races) and conceal which
// server they actually exercised. `deploy/redis/docker-compose.yml` provides
// the one pinned disposable instance; its README has the exact commands.
//
// Run:
//
//	VALKEY_ADDRESS=127.0.0.1:6379 go test -tags=integration ./internal/adapters/outbound/valkey
//
// VALKEY_USERNAME and VALKEY_PASSWORD are respected too, so a deployment test
// can prove ACL configuration without changing this file.
func integrationConfig(t *testing.T) Config {
	t.Helper()
	address := os.Getenv("VALKEY_ADDRESS")
	if address == "" {
		t.Fatal("VALKEY_ADDRESS is required for integration tests; start deploy/redis first")
	}
	return Config{
		Address:           address,
		Username:          os.Getenv("VALKEY_USERNAME"),
		Password:          os.Getenv("VALKEY_PASSWORD"),
		Database:          0,
		DialTimeout:       2 * time.Second,
		ConnTimeout:       2 * time.Second,
		PipelineMultiplex: 1,
		BlockingPoolSize:  1,
	}
}

func integrationClient(t *testing.T) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	client, err := Connect(ctx, integrationConfig(t))
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestIntegrationConnectAndHealth(t *testing.T) {
	client := integrationClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := client.Health(ctx); err != nil {
		t.Errorf("Health() error = %v", err)
	}
}

func TestIntegrationCloseStopsNewCommands(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := Connect(ctx, integrationConfig(t))
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	client.Close()

	if err := client.Health(ctx); err == nil {
		t.Error("Health() after Close() error = nil, want a closed-client error")
	}
}

func TestIntegrationCacheSetGetDelete(t *testing.T) {
	store := integrationClient(t).Cache()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	key := "integration:" + t.Name()
	value := []byte{0, 1, 2, 3, 255}

	if err := store.Set(ctx, key, value, 30*time.Second); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	got, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Errorf("Get() = %v, want %v", got, value)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	_, err = store.Get(ctx, key)
	if !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("Get() after Delete() error = %v, want cache.ErrNotFound", err)
	}
}

func TestIntegrationCacheRejectsNonPositiveTTL(t *testing.T) {
	store := integrationClient(t).Cache()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	for _, ttl := range []time.Duration{0, -time.Second} {
		if err := store.Set(ctx, "integration:ttl", []byte("value"), ttl); !errors.Is(err, cache.ErrInvalidTTL) {
			t.Errorf("Set(ttl %s) error = %v, want cache.ErrInvalidTTL", ttl, err)
		}
	}
}
