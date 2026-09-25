// Package config loads the dataplane's small, typed runtime configuration.
//
// Bootstrap configuration is process-level: it is read once before the server
// starts, and a change requires a restart. The set is small enough to read in
// one sitting, so a hand-written loader keeps defaults, parsing and validation
// visible instead of buying a configuration framework to hide them.
//
// The environment prefix is the application's own. That is the point of the
// prefix: one deployment environment may carry the variables of all four
// applications at once, and a value meant for the Control Plane must be
// incapable of configuring the process that serves traffic.
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
	// DefaultAddr is the listener the dataplane uses when DATAPLANE_ADDR is
	// absent. The four applications bind different ports by default so a
	// development machine can run all of them at once: :8080 console-api,
	// :8081 this one, :8082 dataplane-api.
	DefaultAddr = ":8081"

	// DefaultShutdownTimeout bounds graceful draining. Ten seconds leaves room
	// inside a container orchestrator's usual 30-second termination grace for an
	// in-flight request to finish and the process to exit after it does.
	DefaultShutdownTimeout = 10 * time.Second

	// DefaultReadHeaderTimeout bounds a connected peer that sends no request
	// headers. Read and write body timeouts wait for real traffic with measured
	// sizes; inventing them before that is guessing with a straight face.
	DefaultReadHeaderTimeout = 5 * time.Second

	// DefaultReservationHoldWindow is how long an admitted request's
	// reservation may remain open before it becomes eligible for lease-expiry
	// recovery — the horizon the reaper sweeps against. Five minutes is the
	// trade-off between capacity wasted and capacity stuck: long enough to
	// cover a slow streaming response plus the settlement that follows it,
	// short enough that a process which dies holding a reservation has it
	// recovered in minutes rather than hours.
	DefaultReservationHoldWindow = 5 * time.Minute

	// DefaultReservationLeaseTTL is how long one worker's claim (lease) on an
	// open reservation stays valid. Thirty seconds is the trade-off between a
	// lease lost mid-work and a hold recovered late: long enough to survive a
	// garbage-collection pause or a scheduler stall, short enough that a
	// crashed worker's hold is recovered quickly rather than after the whole
	// hold window has run.
	DefaultReservationLeaseTTL = 30 * time.Second

	// MaxReservationHoldWindow is the ceiling a configured hold window may
	// reach. The real constraint is the request-duration ceiling: a hold only
	// has to outlive one admitted request plus its settlement, and no request
	// this runtime admits runs for thirty days. The bound is stated anyway,
	// far inside anything the arithmetic could reach, because the adapters
	// derive a reservation's expiry by adding the hold to the database's own
	// clock — transaction_timestamp() + hold — and that addition must never
	// overflow the timestamp the database stores. A conservative ceiling makes
	// that a load-time fact instead of a per-deployment hope.
	MaxReservationHoldWindow = 30 * 24 * time.Hour

	// ownedDatabase is the only database this application may be pointed at —
	// the plane binding of ADR 0006 §7, checked in validatePostgresDSN and
	// again by the persistence adapter (which carries a constant of the same
	// name). It is a constant and not a setting: which database is this
	// plane's is decided by the architecture, and a deployment that wants a
	// different answer is misconfigured, not retuned.
	ownedDatabase = "dataplane"

	// DefaultPostgresDSN is the connection string used when
	// DATAPLANE_POSTGRES_DSN is absent: the local development fixture from
	// deploy/postgres/compose.yaml — one TimescaleDB cluster on this machine,
	// the database `dataplane` this application owns (ADR 0006 §7), and the
	// fixture's own role and password. It is a development default and nothing
	// more: no production credential lives in this file, and a deployment that
	// means production states its own DSN through the variable.
	DefaultPostgresDSN = "postgres://gateway:gateway-dev-only@127.0.0.1:5432/dataplane?sslmode=disable"

	// DefaultPostgresMaxOpenConns bounds the connections the pool may hold. The
	// runtime is the request hot path, and its database is what makes intake,
	// reservations and usage durable — a latency-bound, connection-heavy
	// workload (ADR 0006 §2) — so the pool is sized for concurrency rather than
	// frugality. Twenty-five leaves headroom under PostgreSQL's default
	// max_connections of 100 even with the Control Plane's own pool beside it
	// on the one shared cluster.
	DefaultPostgresMaxOpenConns = 25

	// DefaultPostgresMaxIdleConns keeps five connections warm between bursts.
	// Idle connections cost the server almost nothing and save a fresh
	// handshake on the next burst; the value rides under the open-connections
	// bound rather than beside it.
	DefaultPostgresMaxIdleConns = 5

	// DefaultPostgresConnMaxLifetime bounds how long one connection may be
	// reused before it is retired and replaced. It spreads a connection's
	// lifetime out in time so no failure mode that develops with connection age
	// — a NAT or firewall entry gone stale, a server-side recycle — lands on
	// every connection at once.
	DefaultPostgresConnMaxLifetime = 30 * time.Minute

	// DefaultPostgresConnMaxIdleTime bounds how long an idle connection is kept
	// before it is closed. A runtime that goes quiet between traffic bursts
	// should not hold its whole pool open against a server that may have moved
	// on underneath it.
	DefaultPostgresConnMaxIdleTime = 5 * time.Minute
)

// Config is the complete bootstrap configuration of the dataplane process.
//
// It deliberately carries nothing about the Control Plane. The two management
// settings below are not an exception to that: DATAPLANE_MANAGEMENT_ADDR is an
// *inbound* listener of this process and DATAPLANE_MANAGEMENT_TOKEN is the
// credential a caller must present to it — neither is an address this process
// dials, and there is no setting here that would let it dial one. A runtime that
// can be misconfigured towards a management endpoint is one step from depending
// on it.
type Config struct {
	Addr              string
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration

	// ReservationHoldWindow is how long an admitted request's reservation may
	// remain open before it becomes eligible for lease-expiry recovery — the
	// horizon the reaper sweeps against. The admission use case reads it at
	// reservation creation, when it stamps the expiry the recovery sweep
	// compares against. It is an operational horizon, not a contract constant:
	// no response the runtime sends ever states it, and retuning it changes
	// only how quickly capacity comes back, never what a reservation means.
	ReservationHoldWindow time.Duration

	// ReservationLeaseTTL is how long one worker's claim (lease) on an open
	// reservation stays valid. Like ReservationHoldWindow it is consumed by
	// the admission use case at reservation creation and honoured by the
	// recovery sweep, and like that one it is an operational horizon, not a
	// contract constant. It must sit strictly inside the hold window — a lease
	// that outlives the hold it fences is a configuration the process refuses
	// to start with, in validateReservationHorizons.
	ReservationLeaseTTL time.Duration

	// ManagementAddr is the private listener the Data Plane's management
	// surface is served from. An empty value means this process serves no
	// management surface at all, which is the default: a runtime that has not
	// been told to open an administrative port does not open one.
	ManagementAddr string

	// ManagementToken is the shared secret a management caller presents. It is
	// a credential this process *accepts*, not one it uses to reach anyone, and
	// it is required whenever ManagementAddr is set — a listener with no
	// credential would authenticate nobody and answer 401 to every caller, which
	// is a deployment mistake worth failing the process over rather than
	// discovering from a log line.
	//
	// It is never logged, never echoed in an error and never serialized. The
	// loader below reads it into exactly this field and nothing else in this
	// process formats it.
	ManagementToken string

	// Postgres holds the connection settings for this process's own plane's
	// database — the one outbound address this configuration names, and the
	// only one it may name: the `dataplane` database of ADR 0006 §7, which the
	// runtime owns and nothing else does. A setting that pointed this process
	// at the Control Plane's listener would be a dependency on it; a setting
	// that points it at its own database is what makes its hot path work when
	// the Control Plane is unreachable.
	Postgres Postgres
}

// Postgres holds the connection settings for this application's own plane's
// database — `dataplane` (ADR 0006 §7). The DSN is a secret-bearing value: it
// is redacted by LogValue and never appears in an error.
type Postgres struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// Defaults returns the configuration used when no supported environment
// variable is set. It is a function rather than a shared mutable value so each
// caller owns its copy.
func Defaults() Config {
	return Config{
		Addr:                  DefaultAddr,
		ShutdownTimeout:       DefaultShutdownTimeout,
		ReadHeaderTimeout:     DefaultReadHeaderTimeout,
		ReservationHoldWindow: DefaultReservationHoldWindow,
		ReservationLeaseTTL:   DefaultReservationLeaseTTL,
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

// Load reads the supported DATAPLANE_* bootstrap variables, applies explicit
// defaults, and validates the complete result before returning it.
func Load(lookup LookupEnv) (Config, error) {
	cfg := Defaults()

	if value, ok := lookup("DATAPLANE_ADDR"); ok {
		if value == "" {
			return Config{}, fmt.Errorf("DATAPLANE_ADDR must not be empty")
		}
		cfg.Addr = value
	}
	if value, ok := lookup("DATAPLANE_SHUTDOWN_TIMEOUT"); ok {
		duration, err := parsePositiveDuration("DATAPLANE_SHUTDOWN_TIMEOUT", value)
		if err != nil {
			return Config{}, err
		}
		cfg.ShutdownTimeout = duration
	}
	if value, ok := lookup("DATAPLANE_READ_HEADER_TIMEOUT"); ok {
		duration, err := parsePositiveDuration("DATAPLANE_READ_HEADER_TIMEOUT", value)
		if err != nil {
			return Config{}, err
		}
		cfg.ReadHeaderTimeout = duration
	}
	if value, ok := lookup("DATAPLANE_RESERVATION_HOLD_WINDOW"); ok {
		duration, err := parsePositiveDuration("DATAPLANE_RESERVATION_HOLD_WINDOW", value)
		if err != nil {
			return Config{}, err
		}
		cfg.ReservationHoldWindow = duration
	}
	if value, ok := lookup("DATAPLANE_RESERVATION_LEASE_TTL"); ok {
		duration, err := parsePositiveDuration("DATAPLANE_RESERVATION_LEASE_TTL", value)
		if err != nil {
			return Config{}, err
		}
		cfg.ReservationLeaseTTL = duration
	}

	if value, ok := lookup("DATAPLANE_MANAGEMENT_ADDR"); ok {
		if value == "" {
			return Config{}, fmt.Errorf("DATAPLANE_MANAGEMENT_ADDR must not be empty; omit the variable to serve no management surface")
		}
		if err := validateAddr("DATAPLANE_MANAGEMENT_ADDR", value); err != nil {
			return Config{}, err
		}
		cfg.ManagementAddr = value
	}
	if value, ok := lookup("DATAPLANE_MANAGEMENT_TOKEN"); ok {
		// The two management settings are decided together, and both halves of
		// the mistake are refused. A listener without a credential is an
		// administrative port that answers 401 to everyone — useless, and one
		// edit away from an administrative port that answers everyone. A
		// credential without a listener is a setting this process cannot act
		// on, which is the misleading kind: it looks like the fact feed is
		// protected when in fact it is not being served at all.
		if value == "" {
			return Config{}, fmt.Errorf("DATAPLANE_MANAGEMENT_TOKEN must not be empty")
		}
		if cfg.ManagementAddr == "" {
			return Config{}, fmt.Errorf("DATAPLANE_MANAGEMENT_TOKEN is set but DATAPLANE_MANAGEMENT_ADDR is not; a credential is only read by the listener that would present it")
		}
		cfg.ManagementToken = value
	}
	if cfg.ManagementAddr != "" && cfg.ManagementToken == "" {
		return Config{}, fmt.Errorf("DATAPLANE_MANAGEMENT_TOKEN is required when DATAPLANE_MANAGEMENT_ADDR is set")
	}

	if value, ok := lookup("DATAPLANE_POSTGRES_DSN"); ok {
		if value == "" {
			return Config{}, fmt.Errorf("DATAPLANE_POSTGRES_DSN must not be empty")
		}
		cfg.Postgres.DSN = value
	}
	if value, ok := lookup("DATAPLANE_POSTGRES_MAX_OPEN_CONNS"); ok {
		conns, err := parseInt("DATAPLANE_POSTGRES_MAX_OPEN_CONNS", value)
		if err != nil {
			return Config{}, err
		}
		cfg.Postgres.MaxOpenConns = conns
	}
	if value, ok := lookup("DATAPLANE_POSTGRES_MAX_IDLE_CONNS"); ok {
		conns, err := parseInt("DATAPLANE_POSTGRES_MAX_IDLE_CONNS", value)
		if err != nil {
			return Config{}, err
		}
		cfg.Postgres.MaxIdleConns = conns
	}
	if value, ok := lookup("DATAPLANE_POSTGRES_CONN_MAX_LIFETIME"); ok {
		duration, err := parsePositiveDuration("DATAPLANE_POSTGRES_CONN_MAX_LIFETIME", value)
		if err != nil {
			return Config{}, err
		}
		cfg.Postgres.ConnMaxLifetime = duration
	}
	if value, ok := lookup("DATAPLANE_POSTGRES_CONN_MAX_IDLE_TIME"); ok {
		duration, err := parsePositiveDuration("DATAPLANE_POSTGRES_CONN_MAX_IDLE_TIME", value)
		if err != nil {
			return Config{}, err
		}
		cfg.Postgres.ConnMaxIdleTime = duration
	}

	if err := validateAddr("DATAPLANE_ADDR", cfg.Addr); err != nil {
		return Config{}, err
	}
	if err := validateReservationHorizons(cfg.ReservationHoldWindow, cfg.ReservationLeaseTTL); err != nil {
		return Config{}, err
	}
	if err := validatePostgres(cfg.Postgres); err != nil {
		return Config{}, err
	}
	return cfg, nil
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

// parseInt parses one connection-count variable. The range each count must sit
// in is a relationship between two values — idle against open — so the range
// checks live in validatePostgres where both are in hand, and this helper only
// refuses what is not a number at all.
func parseInt(name, value string) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("%s must not be empty", name)
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %v", name, err)
	}
	return number, nil
}

// validatePostgres checks the pool settings as a set: each count against its
// own bound, the idle bound against the open bound, and the DSN against both
// its shape and the plane it may name. The variable names are spelled here
// rather than inferred because a message that named the wrong variable would
// send an operator to the wrong line of their deployment.
func validatePostgres(p Postgres) error {
	if p.MaxOpenConns <= 0 {
		return fmt.Errorf("DATAPLANE_POSTGRES_MAX_OPEN_CONNS must be greater than zero")
	}
	if p.MaxIdleConns < 0 {
		return fmt.Errorf("DATAPLANE_POSTGRES_MAX_IDLE_CONNS must not be negative")
	}
	if p.MaxIdleConns > p.MaxOpenConns {
		return fmt.Errorf(
			"DATAPLANE_POSTGRES_MAX_IDLE_CONNS (%d) must not be greater than DATAPLANE_POSTGRES_MAX_OPEN_CONNS (%d)",
			p.MaxIdleConns, p.MaxOpenConns)
	}
	return validatePostgresDSN("DATAPLANE_POSTGRES_DSN", p.DSN)
}

// validatePostgresDSN checks one connection string's shape and, past shape,
// the one rule that is an ownership boundary: this application may only be
// pointed at the database it owns. The default DSN names `dataplane`, and no
// override may name anything else — a DSN naming `control` would point this
// process at the Control Plane's database, which ADR 0006 §7 assigns to the
// other plane and which no ordinary query may span into. Configured against
// the wrong plane, the process refuses to start rather than starting half
// wrong.
//
// The failure messages never quote the DSN: its userinfo carries the
// password, and a validation error is a string an operator will paste into a
// ticket.
func validatePostgresDSN(name, dsn string) error {
	parsed, err := url.Parse(dsn)
	if err != nil {
		// url.Parse's own error quotes the URL it was handed — userinfo and
		// all — so the reason is dropped rather than wrapped. A value that is
		// not a URL at all has nothing diagnosable to preserve anyway: the fix
		// is the same whatever the parser choked on.
		return fmt.Errorf("%s must be a parsable URL", name)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return fmt.Errorf("%s must use the postgres or postgresql scheme", name)
	}
	database := strings.TrimPrefix(parsed.Path, "/")
	if database == "" || strings.Contains(database, "/") {
		return fmt.Errorf("%s must name exactly one database in its path", name)
	}
	if database != ownedDatabase {
		return fmt.Errorf(
			"%s names the %q database; this application owns only the dataplane database (ADR 0006 §7) and must not be configured against another plane's",
			name, database)
	}
	return nil
}

// validateAddr checks one listener address. The name is a parameter rather than
// a constant because there are two listeners now and a message that named the
// wrong variable would send an operator to the wrong line of their deployment.
func validateAddr(name, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return fmt.Errorf("%s must be a host:port address", name)
	}
	if strings.Contains(port, ":") {
		return fmt.Errorf("%s must contain a numeric port", name)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("%s must contain a numeric port", name)
	}
	return nil
}

// validateReservationHorizons checks the two admission horizons as a set. The
// per-variable rules — a Go duration at all, greater than zero — are applied
// where each variable is read, by parsePositiveDuration like every other
// duration this file reads; what needs both values in hand lives here: the
// ceiling that keeps the adapters' database-clock arithmetic safe, and the
// ordering that keeps a lease inside the hold it fences. The variable names
// are spelled rather than inferred because a message that named the wrong
// variable would send an operator to the wrong line of their deployment.
func validateReservationHorizons(hold, lease time.Duration) error {
	if hold > MaxReservationHoldWindow {
		return fmt.Errorf(
			"DATAPLANE_RESERVATION_HOLD_WINDOW (%s) must not be greater than %s",
			hold, MaxReservationHoldWindow)
	}
	if lease >= hold {
		return fmt.Errorf(
			"DATAPLANE_RESERVATION_LEASE_TTL (%s) must be strictly shorter than DATAPLANE_RESERVATION_HOLD_WINDOW (%s); a lease must never outlive the hold it fences",
			lease, hold)
	}
	return nil
}

// LogValue renders the connection settings safe for logs: where the pool
// points and how it is sized are operational facts, and the DSN's userinfo —
// its password — is not one. Postgres satisfies slog.LogValuer, so
// `slog.Any("postgres", cfg.Postgres)` redacts by construction: the
// requirement that the DSN never reach a log is enforced by the type rather
// than by every call site remembering to, which is the pattern the valkey
// adapter's Config already uses. The DSN is decomposed rather than truncated,
// because its components are the useful part of it and the whole is the
// dangerous one.
func (p Postgres) LogValue() slog.Value {
	if parsed, err := url.Parse(p.DSN); err == nil {
		return slog.GroupValue(
			slog.String("scheme", parsed.Scheme),
			slog.String("host", parsed.Hostname()),
			slog.String("port", parsed.Port()),
			slog.String("database", strings.Trim(parsed.Path, "/")),
			slog.Int("max_open_conns", p.MaxOpenConns),
			slog.Int("max_idle_conns", p.MaxIdleConns),
			slog.Duration("conn_max_lifetime", p.ConnMaxLifetime),
			slog.Duration("conn_max_idle_time", p.ConnMaxIdleTime),
		)
	}
	// An unparsable DSN cannot be decomposed, so none of it is shown.
	return slog.GroupValue(
		slog.String("dsn", "[unparsable, redacted]"),
		slog.Int("max_open_conns", p.MaxOpenConns),
		slog.Int("max_idle_conns", p.MaxIdleConns),
		slog.Duration("conn_max_lifetime", p.ConnMaxLifetime),
		slog.Duration("conn_max_idle_time", p.ConnMaxIdleTime),
	)
}
