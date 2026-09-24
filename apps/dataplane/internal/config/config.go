// Package config loads the dataplane's small, typed runtime configuration.
//
// Bootstrap configuration is process-level: it is read once before the server
// starts, and a change requires a restart. There are five values today, so a
// hand-written loader keeps defaults, parsing and validation visible instead of
// buying a configuration framework to hide them.
//
// The environment prefix is the application's own. That is the point of the
// prefix: one deployment environment may carry the variables of all four
// applications at once, and a value meant for the Control Plane must be
// incapable of configuring the process that serves traffic.
package config

import (
	"fmt"
	"net"
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

	if err := validateAddr("DATAPLANE_ADDR", cfg.Addr); err != nil {
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
