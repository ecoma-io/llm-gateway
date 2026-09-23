// Package config loads the dataplane-api's small, typed runtime configuration.
//
// Bootstrap configuration is process-level: it is read once before the server
// starts, and a change requires a restart. There are three values today, so a
// hand-written loader keeps defaults, parsing and validation visible instead of
// buying a configuration framework to hide them.
//
// The environment prefix is the application's own. That is the point of the
// prefix: one deployment environment may carry the variables of all four
// applications at once, and a value meant for the runtime must be incapable of
// configuring the management surface — or of changing what the runtime does by
// being read in the wrong process.
package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultAddr is the listener the dataplane-api uses when DATAPLANE_API_ADDR
	// is absent. The four applications bind different ports by default so a
	// development machine can run all of them at once: :8080 console-api,
	// :8081 dataplane, :8082 this one.
	DefaultAddr = ":8082"

	// DefaultShutdownTimeout bounds graceful draining. Ten seconds leaves room
	// inside a container orchestrator's usual 30-second termination grace for an
	// in-flight request to finish and the process to exit after it does.
	DefaultShutdownTimeout = 10 * time.Second

	// DefaultReadHeaderTimeout bounds a connected peer that sends no request
	// headers. Read and write body timeouts wait for real traffic with measured
	// sizes; inventing them before that is guessing with a straight face.
	DefaultReadHeaderTimeout = 5 * time.Second
)

// Config is the complete bootstrap configuration of the dataplane-api process.
// It deliberately carries no database or cache settings: this application owns
// neither, and an address it could dial directly is the seam it must not
// have.
type Config struct {
	Addr              string
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration
}

// Defaults returns the configuration used when no supported environment
// variable is set. It is a function rather than a shared mutable value so each
// caller owns its copy.
func Defaults() Config {
	return Config{
		Addr:              DefaultAddr,
		ShutdownTimeout:   DefaultShutdownTimeout,
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
	}
}

// LookupEnv is os.LookupEnv's shape. Injecting it keeps validation tests
// hermetic and distinguishes an absent variable from one explicitly set empty:
// absence uses a documented default, while an empty deployment template fails
// before the server has accepted traffic.
type LookupEnv func(string) (string, bool)

// Load reads the supported DATAPLANE_API_* bootstrap variables, applies explicit
// defaults, and validates the complete result before returning it.
func Load(lookup LookupEnv) (Config, error) {
	cfg := Defaults()

	if value, ok := lookup("DATAPLANE_API_ADDR"); ok {
		if value == "" {
			return Config{}, fmt.Errorf("DATAPLANE_API_ADDR must not be empty")
		}
		cfg.Addr = value
	}
	if value, ok := lookup("DATAPLANE_API_SHUTDOWN_TIMEOUT"); ok {
		duration, err := parsePositiveDuration("DATAPLANE_API_SHUTDOWN_TIMEOUT", value)
		if err != nil {
			return Config{}, err
		}
		cfg.ShutdownTimeout = duration
	}
	if value, ok := lookup("DATAPLANE_API_READ_HEADER_TIMEOUT"); ok {
		duration, err := parsePositiveDuration("DATAPLANE_API_READ_HEADER_TIMEOUT", value)
		if err != nil {
			return Config{}, err
		}
		cfg.ReadHeaderTimeout = duration
	}

	if err := validateAddr(cfg.Addr); err != nil {
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

func validateAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return fmt.Errorf("DATAPLANE_API_ADDR must be a host:port address")
	}
	if strings.Contains(port, ":") {
		return fmt.Errorf("DATAPLANE_API_ADDR must contain a numeric port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("DATAPLANE_API_ADDR must contain a numeric port")
	}
	return nil
}
