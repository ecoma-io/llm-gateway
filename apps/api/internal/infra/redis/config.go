package redis

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"time"
)

// Defaults for every Config field. They are valkey-go's own defaults stated
// once, here, so a reader sees the behaviour they get without opening the
// library: the dial timeout is the library's DefaultDialTimeout, and the
// connection timeout is the deadline the library derives when none is set
// (TCP keepalive 1s × 10 — Linux's 9 keepalive probes plus one interval).
// Inventing tighter numbers now would be guessing with a straight face —
// the workload that would tune them does not exist yet.
const (
	defaultAddress           = "127.0.0.1:6379"
	defaultDialTimeout       = 5 * time.Second
	defaultConnTimeout       = 10 * time.Second
	defaultPipelineMultiplex = 2
	maxPipelineMultiplex     = 4
	defaultBlockingPoolSize  = 1
	maxBlockingPoolSize      = 16
)

// Config is the typed, validated description of one connection to the store.
//
// It is data, not policy: whether the gateway connects at boot or lazily, and
// what a connection failure then means, are decisions for the wiring that
// consumes this package, not encoded here.
type Config struct {
	// Address is the host:port of the Redis-compatible server.
	Address string

	// Username and Password authenticate the connection (ACL AUTH). Both
	// empty means no authentication — the shape of every local development
	// instance. They arrive from the environment and never appear in logs,
	// panics, or error messages; Config.LogValue is what keeps that true.
	Username string
	Password string

	// Database is the logical SELECT index, 0 by default. One index per
	// deployment keeps the cache keyspace policy in one place.
	Database int

	// DialTimeout bounds establishing a TCP connection.
	DialTimeout time.Duration

	// ConnTimeout bounds the read/write deadline of each connection. valkey-go
	// applies one deadline to both directions and exposes it as
	// ClientOption.ConnWriteTimeout, so the contract is a single positive value
	// mapped through unchanged: a read/write split would be collapsed by the
	// library anyway, and one name states what the deadline actually governs.
	ConnTimeout time.Duration

	// PipelineMultiplex bounds connections that multiplex ordinary commands:
	// the client opens 2^m pipes, 4 at the default of 2.
	PipelineMultiplex int

	// BlockingPoolSize bounds the separate pool the client reserves for future
	// blocking commands. The cache path does not use it, but setting it avoids
	// inheriting the library's 1024-connection default when a later consumer
	// does. A production workload may tune it at its wiring boundary.
	BlockingPoolSize int
}

// Validate reports whether every field holds a value the connection can be
// built from. It checks shape (an address with a port, positive timeouts), not
// reachability: whether the server answers is what Client.Health reports, on
// the wire.
func (c Config) Validate() error {
	if c.Address == "" {
		return errors.New("address is required")
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return fmt.Errorf("address %q must be host:port: %w", c.Address, err)
	}
	if c.Database < 0 {
		return fmt.Errorf("database %d must not be negative", c.Database)
	}
	if c.DialTimeout <= 0 {
		return fmt.Errorf("dial timeout %s must be positive", c.DialTimeout)
	}
	if c.ConnTimeout <= 0 {
		return fmt.Errorf("conn timeout %s must be positive", c.ConnTimeout)
	}
	if c.PipelineMultiplex < 0 || c.PipelineMultiplex > maxPipelineMultiplex {
		return fmt.Errorf("pipeline multiplex %d must be between 0 and %d", c.PipelineMultiplex, maxPipelineMultiplex)
	}
	if c.BlockingPoolSize < 1 || c.BlockingPoolSize > maxBlockingPoolSize {
		return fmt.Errorf("blocking pool size %d must be between 1 and %d", c.BlockingPoolSize, maxBlockingPoolSize)
	}
	return nil
}

// FromEnv returns the Config the environment describes, applying the defaults
// above for variables that are unset or empty. Credentials are read raw: an
// empty REDIS_PASSWORD is a legitimate "no password", not a fallback
// candidate.
//
// A variable that is set to something unparsable is an error naming the
// variable — a misconfiguration that silently becomes a default is worse than
// one that stops the process.
func FromEnv() (Config, error) {
	database, err := envInt("REDIS_DATABASE", 0)
	if err != nil {
		return Config{}, err
	}
	dialTimeout, err := envDuration("REDIS_DIAL_TIMEOUT", defaultDialTimeout)
	if err != nil {
		return Config{}, err
	}
	connTimeout, err := envDuration("REDIS_CONN_TIMEOUT", defaultConnTimeout)
	if err != nil {
		return Config{}, err
	}
	multiplex, err := envInt("REDIS_PIPELINE_MULTIPLEX", defaultPipelineMultiplex)
	if err != nil {
		return Config{}, err
	}
	blockingPoolSize, err := envInt("REDIS_BLOCKING_POOL_SIZE", defaultBlockingPoolSize)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Address:           envString("REDIS_ADDRESS", defaultAddress),
		Username:          os.Getenv("REDIS_USERNAME"),
		Password:          os.Getenv("REDIS_PASSWORD"),
		Database:          database,
		DialTimeout:       dialTimeout,
		ConnTimeout:       connTimeout,
		PipelineMultiplex: multiplex,
		BlockingPoolSize:  blockingPoolSize,
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("redis: environment configuration: %w", err)
	}
	return cfg, nil
}

// LogValue renders the config safe for logs: operational settings are visible;
// ACL credentials are not. Config satisfies slog.LogValuer, so a future
// `slog.Info("redis", "config", cfg)` redacts by construction — the requirement
// that credentials never reach logs is enforced by the type, not by every call
// site remembering to.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("address", c.Address),
		slog.String("username", "[redacted]"),
		slog.String("password", "[redacted]"),
		slog.Int("database", c.Database),
		slog.Duration("dial_timeout", c.DialTimeout),
		slog.Duration("conn_timeout", c.ConnTimeout),
		slog.Int("pipeline_multiplex", c.PipelineMultiplex),
		slog.Int("blocking_pool_size", c.BlockingPoolSize),
	)
}

// envString returns the named variable, or fallback when it is unset or empty.
func envString(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}

// envInt parses the named variable as an integer, or returns fallback when it
// is unset or empty.
func envInt(name string, fallback int) (int, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("redis: %s must be an integer, got %q", name, v)
	}
	return n, nil
}

// envDuration parses the named variable as a Go duration, or returns fallback
// when it is unset or empty.
func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("redis: %s must be a duration like 5s, got %q", name, v)
	}
	return d, nil
}
