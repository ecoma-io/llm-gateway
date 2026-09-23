// Package config loads the dataplane-api's small, typed runtime configuration.
//
// Bootstrap configuration is process-level: it is read once before the server
// starts, and a change requires a restart. There are five values today, so a
// hand-written loader keeps defaults, parsing and validation visible instead of
// buying a configuration framework to hide them.
//
// The environment prefix is the application's own. That is the point of the
// prefix: one deployment environment may carry the variables of all four
// applications at once, and a value meant for the runtime must be incapable of
// configuring the management surface — or of changing what the runtime does by
// being read in the wrong process.
//
// Two of the five have no default and never will. The Data Plane's address and
// the service credential are facts about the deployment this process runs in:
// there is no convention to guess an address from, and a default credential is
// either a published secret or a silent hole. An absent one fails startup,
// which is the only moment at which the operator can still fix it.
package config

import (
	"fmt"
	"net"
	"net/url"
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
// neither, and an address it could dial directly is the seam it must not have.
// DataPlaneURL is the one outbound address here, and it is a management call
// the Data Plane can refuse rather than a store this process reads (ADR 0006
// §9, §11).
type Config struct {
	Addr              string
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration

	// DataPlaneURL is the origin of the Data Plane's private management
	// listener — the process this façade fronts for GET /internal/usage-events.
	// It is an origin rather than a base path because the operation's own path
	// is part of the contract and not something a deployment should be able to
	// relocate; nothing in this process is served at an address the Data Plane
	// decided.
	DataPlaneURL string

	// ServiceCredential is the shared secret a management caller presents, and
	// the one this process presents to the Data Plane in return. It is read
	// here, handed to the route table and to the outbound adapter, and written
	// nowhere: not into a log line, not into an error, not into a response.
	// Deployment supplies it — a mounted secret, not a literal in a manifest.
	ServiceCredential string
}

// Defaults returns the configuration used when no supported environment
// variable is set. It is a function rather than a shared mutable value so each
// caller owns its copy.
//
// The two deployment values above are absent from it on purpose: Load fills
// them from the environment or fails, and a default here would be a value that
// looks configured and is not.
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

	// The two required values, read together because they are one decision:
	// where the Data Plane is and what this process says to it. Presence rather
	// than a default is the whole check — an absent variable is `ok == false`
	// here and an empty one is caught below, and the two are distinguished
	// because an empty value in a deployment template is a mistake while an
	// absent one is an incomplete deployment. Both stop the process.
	//
	// The variable names stutter — DATAPLANE_API_DATAPLANE_URL prefixes the
	// process with the name of its peer — and the stutter is deliberate: the
	// prefix says which application reads the value, the suffix says what it
	// points at, and those are different things even when they share a word.
	// A name shortened to DATAPLANE_API_URL would be a variable nobody could
	// place at a glance in a shared environment.
	dataPlaneURL, ok := lookup("DATAPLANE_API_DATAPLANE_URL")
	if !ok {
		return Config{}, fmt.Errorf("DATAPLANE_API_DATAPLANE_URL must be set: this process has no default for where the Data Plane is")
	}
	if err := validateDataPlaneURL(dataPlaneURL); err != nil {
		return Config{}, err
	}
	cfg.DataPlaneURL = dataPlaneURL

	credential, ok := lookup("DATAPLANE_API_SERVICE_CREDENTIAL")
	if !ok {
		return Config{}, fmt.Errorf("DATAPLANE_API_SERVICE_CREDENTIAL must be set: the management surface authenticates callers and has nothing to authenticate them with otherwise")
	}
	if credential == "" {
		return Config{}, fmt.Errorf("DATAPLANE_API_SERVICE_CREDENTIAL must not be empty")
	}
	cfg.ServiceCredential = credential

	if err := validateAddr(cfg.Addr); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validateDataPlaneURL accepts the listener's origin and nothing beyond it: an
// http(s) scheme and a host, with no path, query, fragment or trailing slash.
//
// The refusals are not tidiness. An empty path is what lets the outbound
// adapter build a request by appending the operation's own path, so a base URL
// carrying a path would either lose a prefix the operator meant or produce a
// doubled slash — and a doubled slash is answered 404 by any listener in this
// repository, because each of them refuses a non-canonical path rather than
// redirecting it. Failing here means the deployment learns at startup instead
// of at the first poll.
//
// The value is never echoed in a failure. These messages reach the process log,
// and a URL is the one configuration value that could carry a credential inside
// it if an operator pasted one in by mistake.
func validateDataPlaneURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("DATAPLANE_API_DATAPLANE_URL must be an absolute http(s) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("DATAPLANE_API_DATAPLANE_URL must use the http or https scheme")
	}
	if parsed.Host == "" {
		return fmt.Errorf("DATAPLANE_API_DATAPLANE_URL must name a host")
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("DATAPLANE_API_DATAPLANE_URL must be the listener's origin — a scheme, a host and a port, with no path, query, fragment or trailing slash")
	}
	return nil
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
