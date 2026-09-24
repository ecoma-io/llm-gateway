// Package config loads the console-api's small, typed runtime configuration.
//
// Bootstrap configuration is process-level: it is read once before the server
// starts, and a change requires a restart. The roster is small enough that a
// hand-written loader keeps defaults, parsing and validation visible instead
// of buying a configuration framework to hide them — every variable is one
// named branch a reader can audit top to bottom.
//
// The environment prefix is the application's own. That is the point of the
// prefix: one deployment environment may carry the variables of all four
// applications at once, and a value meant for the runtime must be incapable of
// configuring the Control Plane API.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultAddr is the listener the console-api uses when CONSOLE_API_ADDR
	// is absent. The four applications bind different ports by default so a
	// development machine can run all of them at once: :8080 this one, :8081
	// dataplane, :8082 dataplane-api.
	DefaultAddr = ":8080"

	// DefaultShutdownTimeout bounds graceful draining. Ten seconds leaves room
	// inside a container orchestrator's usual 30-second termination grace for an
	// in-flight request to finish and the process to exit after it does.
	DefaultShutdownTimeout = 10 * time.Second

	// DefaultReadHeaderTimeout bounds a connected peer that sends no request
	// headers. Read and write body timeouts wait for real traffic with measured
	// sizes; inventing them before that is guessing with a straight face.
	DefaultReadHeaderTimeout = 5 * time.Second

	// DefaultPostgresDSN is this application's own plane's database as
	// deploy/postgres/compose.yaml runs it: the local fixture's `gateway` role
	// against the `control` database on the loopback cluster. Every piece of it
	// is a development default and none of it is a production credential — a
	// real deployment names its own DSN through the environment, with its own
	// role and its own TLS posture, and never inherits this one.
	DefaultPostgresDSN = "postgres://gateway:gateway-dev-only@127.0.0.1:5432/control?sslmode=disable"

	// DefaultPostgresMaxOpenConns bounds the pool at ten open connections. The
	// console's backend is human-paced — ADR 0006 §2 — so the number is a
	// ceiling against a pool that grows one connection per concurrent request
	// without limit, not a measured size: the workload that would justify a
	// measurement does not exist yet.
	DefaultPostgresMaxOpenConns = 10

	// DefaultPostgresMaxIdleConns keeps two connections warm, so a console
	// that pauses between interactions pays the dial on the first request
	// after the pause rather than on every request.
	DefaultPostgresMaxIdleConns = 2

	// DefaultPostgresConnMaxLifetime retires a connection after half an hour
	// of total service — long enough that a working pool is not dialing in a
	// loop, short enough that a database restart or a network device's idle
	// expiry is absorbed by ordinary churn instead of by a request.
	DefaultPostgresConnMaxLifetime = 30 * time.Minute

	// DefaultPostgresConnMaxIdleTime closes an idle connection after five
	// minutes, so a console nobody is using holds no connections the database
	// could otherwise hand to someone who is.
	DefaultPostgresConnMaxIdleTime = 5 * time.Minute
)

// ownedDatabase is the only database this application may open — the plane
// binding of ADR 0006 §7. It is a constant and not a setting: which database
// is this plane's is decided by the architecture, and a deployment that wants
// a different answer is misconfigured, not retuned.
const ownedDatabase = "control"

// Config is the complete bootstrap configuration of the console-api process.
// It deliberately carries nothing about the runtime or the Data Plane's
// databases: a setting this process cannot act on is a setting that can only
// mislead.
type Config struct {
	Addr              string
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration

	// Postgres is this process's connection settings for its own plane's
	// database. It is a struct and not five loose fields so its LogValue —
	// the thing that keeps the DSN's credential out of logs — travels with
	// the value it protects.
	Postgres Postgres
}

// Postgres holds the connection settings for this application's own plane's
// database — `control` (ADR 0006 §7). The DSN is a secret-bearing value: it
// is redacted by LogValue and never appears in an error.
type Postgres struct {
	// DSN is the connection string, in the URL form PostgreSQL documents.
	// Load validates it twice over: once as a URL of this driver's scheme
	// naming exactly one database, and once against the plane binding — a
	// DSN naming any database but `control` fails startup, so an application
	// pointed at the other plane's database refuses to start instead of
	// writing into it.
	DSN string

	// MaxOpenConns bounds the pool's open connections. It must be positive:
	// a pool that may not open a connection is not a pool.
	MaxOpenConns int

	// MaxIdleConns bounds the connections the pool keeps when idle. It may
	// not be negative, and it may not exceed MaxOpenConns — Load rejects a
	// value that does, naming both variables.
	MaxIdleConns int

	// ConnMaxLifetime bounds the total service of one pooled connection
	// before the pool retires it.
	ConnMaxLifetime time.Duration

	// ConnMaxIdleTime bounds how long an idle connection is kept before the
	// pool closes it.
	ConnMaxIdleTime time.Duration
}

// Defaults returns the configuration used when no supported environment
// variable is set. It is a function rather than a shared mutable value so each
// caller owns its copy.
func Defaults() Config {
	return Config{
		Addr:              DefaultAddr,
		ShutdownTimeout:   DefaultShutdownTimeout,
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		Postgres: Postgres{
			DSN:             DefaultPostgresDSN,
			MaxOpenConns:    DefaultPostgresMaxOpenConns,
			MaxIdleConns:    DefaultPostgresMaxIdleConns,
			ConnMaxLifetime: DefaultPostgresConnMaxLifetime,
			ConnMaxIdleTime: DefaultPostgresConnMaxIdleTime,
		},
	}
}

// LookupEnv is os.LookupEnv's shape. Injecting it keeps validation tests
// hermetic and distinguishes an absent variable from one explicitly set empty:
// absence uses a documented default, while an empty deployment template fails
// before the server has accepted traffic.
type LookupEnv func(string) (string, bool)

// Load reads the supported CONSOLE_API_* bootstrap variables, applies explicit
// defaults, and validates the complete result before returning it.
func Load(lookup LookupEnv) (Config, error) {
	cfg := Defaults()

	if value, ok := lookup("CONSOLE_API_ADDR"); ok {
		if value == "" {
			return Config{}, fmt.Errorf("CONSOLE_API_ADDR must not be empty")
		}
		cfg.Addr = value
	}
	if value, ok := lookup("CONSOLE_API_SHUTDOWN_TIMEOUT"); ok {
		duration, err := parsePositiveDuration("CONSOLE_API_SHUTDOWN_TIMEOUT", value)
		if err != nil {
			return Config{}, err
		}
		cfg.ShutdownTimeout = duration
	}
	if value, ok := lookup("CONSOLE_API_READ_HEADER_TIMEOUT"); ok {
		duration, err := parsePositiveDuration("CONSOLE_API_READ_HEADER_TIMEOUT", value)
		if err != nil {
			return Config{}, err
		}
		cfg.ReadHeaderTimeout = duration
	}
	postgres, err := loadPostgres(lookup)
	if err != nil {
		return Config{}, err
	}
	cfg.Postgres = postgres

	if err := validateAddr(cfg.Addr); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadPostgres reads the CONSOLE_API_POSTGRES_* variables — this process's
// connection to its own plane's database — applying the defaults for absent
// variables and validating the whole result. Absence and emptiness stay
// distinct here as everywhere else in this package: an absent variable is a
// documented default, an empty one is a deployment template that failed to
// render, and only the first is survivable.
func loadPostgres(lookup LookupEnv) (Postgres, error) {
	cfg := Postgres{
		DSN:             DefaultPostgresDSN,
		MaxOpenConns:    DefaultPostgresMaxOpenConns,
		MaxIdleConns:    DefaultPostgresMaxIdleConns,
		ConnMaxLifetime: DefaultPostgresConnMaxLifetime,
		ConnMaxIdleTime: DefaultPostgresConnMaxIdleTime,
	}

	if value, ok := lookup("CONSOLE_API_POSTGRES_DSN"); ok {
		if value == "" {
			return Postgres{}, fmt.Errorf("CONSOLE_API_POSTGRES_DSN must not be empty")
		}
		cfg.DSN = value
	}
	if value, ok := lookup("CONSOLE_API_POSTGRES_MAX_OPEN_CONNS"); ok {
		conns, err := parsePositiveInt("CONSOLE_API_POSTGRES_MAX_OPEN_CONNS", value)
		if err != nil {
			return Postgres{}, err
		}
		cfg.MaxOpenConns = conns
	}
	if value, ok := lookup("CONSOLE_API_POSTGRES_MAX_IDLE_CONNS"); ok {
		conns, err := parseNonNegativeInt("CONSOLE_API_POSTGRES_MAX_IDLE_CONNS", value)
		if err != nil {
			return Postgres{}, err
		}
		cfg.MaxIdleConns = conns
	}
	if value, ok := lookup("CONSOLE_API_POSTGRES_CONN_MAX_LIFETIME"); ok {
		duration, err := parsePositiveDuration("CONSOLE_API_POSTGRES_CONN_MAX_LIFETIME", value)
		if err != nil {
			return Postgres{}, err
		}
		cfg.ConnMaxLifetime = duration
	}
	if value, ok := lookup("CONSOLE_API_POSTGRES_CONN_MAX_IDLE_TIME"); ok {
		duration, err := parsePositiveDuration("CONSOLE_API_POSTGRES_CONN_MAX_IDLE_TIME", value)
		if err != nil {
			return Postgres{}, err
		}
		cfg.ConnMaxIdleTime = duration
	}

	// The two bounds are validated as a pair, not as variables: either half
	// of a pool configured to keep more idle connections than it may open is
	// a configuration the pool would resolve arbitrarily, so the error names
	// both variables.
	if cfg.MaxIdleConns > cfg.MaxOpenConns {
		return Postgres{}, fmt.Errorf("CONSOLE_API_POSTGRES_MAX_IDLE_CONNS (%d) must not exceed CONSOLE_API_POSTGRES_MAX_OPEN_CONNS (%d)", cfg.MaxIdleConns, cfg.MaxOpenConns)
	}
	if err := validatePostgresDSN("CONSOLE_API_POSTGRES_DSN", cfg.DSN); err != nil {
		return Postgres{}, err
	}
	return cfg, nil
}

// validatePostgresDSN checks the shape of the DSN the environment named. The
// failure modes are the ones a misconfigured deployment actually hits: not a
// URL at all, a URL of another driver's scheme, no database in the path —
// and, the check that makes the plane boundary mechanical, a database this
// application does not own. ADR 0006 §7 gives the console-api the `control`
// database and nothing else, so a configuration naming the other plane's
// database is a process that refuses to start, not one that writes into
// another plane's state.
//
// Errors name the variable and, where it helps, the database — never the
// userinfo: the DSN carries the role's password, and an error message is a
// log line waiting to happen.
func validatePostgresDSN(name, dsn string) error {
	parsed, err := url.Parse(dsn)
	if err != nil {
		// url.Parse quotes the string it rejected, credentials included, so
		// its own text is deliberately not wrapped into this error.
		return fmt.Errorf("%s must be a URL", name)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return fmt.Errorf("%s must be a postgres:// or postgresql:// URL", name)
	}
	database := strings.TrimPrefix(parsed.Path, "/")
	if database == "" || strings.Contains(database, "/") {
		return fmt.Errorf("%s must name exactly one database in its path", name)
	}
	if database != ownedDatabase {
		return fmt.Errorf("%s names database %q, but this application owns only the %s database (ADR 0006 §7): the Control Plane API refuses to start against another plane's database", name, database, ownedDatabase)
	}
	return nil
}

// LogValue renders the settings safe for logs: the target — scheme, host,
// port, database — and the pool limits are visible; the DSN, which carries
// the role's password, is not. Postgres satisfies slog.LogValuer, so a future
// `slog.Info("postgres", "config", cfg)` redacts by construction — the same
// enforcement the valkey adapter's Config carries, and for the same reason:
// the requirement that the credential never reach a log is enforced by the
// type, not by every call site remembering to.
func (p Postgres) LogValue() slog.Value {
	attrs := make([]slog.Attr, 0, 7)
	// The DSN is parsed rather than string-sliced so each piece stays honest
	// about what it names. An unparsable DSN never reaches a log either: it
	// is redacted whole, since any piece of it could be the credential.
	if parsed, err := url.Parse(p.DSN); err != nil {
		attrs = append(attrs, slog.String("dsn", "[redacted]"))
	} else {
		attrs = append(attrs,
			slog.String("scheme", parsed.Scheme),
			slog.String("host", parsed.Hostname()),
			slog.String("port", parsed.Port()),
			slog.String("database", strings.TrimPrefix(parsed.Path, "/")),
		)
	}
	attrs = append(attrs,
		slog.Int("max_open_conns", p.MaxOpenConns),
		slog.Int("max_idle_conns", p.MaxIdleConns),
		slog.Duration("conn_max_lifetime", p.ConnMaxLifetime),
		slog.Duration("conn_max_idle_time", p.ConnMaxIdleTime),
	)
	return slog.GroupValue(attrs...)
}

func parsePositiveDuration(name, value string) (time.Duration, error) {
	if value == "" {
		return 0, fmt.Errorf("%s must not be empty", name)
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration: %w", name, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", name)
	}
	return duration, nil
}

// parsePositiveInt parses a pool bound whose floor is one connection: a pool
// that may open none can never answer.
func parsePositiveInt(name, value string) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("%s must not be empty", name)
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	if number < 1 {
		return 0, fmt.Errorf("%s must be greater than zero", name)
	}
	return number, nil
}

// parseNonNegativeInt parses a pool bound whose floor is zero: keeping no
// idle connection is a legitimate — if slow — choice.
func parseNonNegativeInt(name, value string) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("%s must not be empty", name)
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	if number < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}
	return number, nil
}

func validateAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return fmt.Errorf("CONSOLE_API_ADDR must be a host:port address")
	}
	if strings.Contains(port, ":") {
		return fmt.Errorf("CONSOLE_API_ADDR must contain a numeric port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("CONSOLE_API_ADDR must contain a numeric port")
	}
	return nil
}
