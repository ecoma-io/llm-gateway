package config

import (
	"strings"
	"testing"
	"time"
)

// deploymentEnv is the environment every case starts from: the two values Load
// has no default for, because a deployment states where its Data Plane is and
// what credential it presents rather than inheriting a guess. A case that wants
// one of them absent removes it, which is the only way the loader can tell
// "unset" from "set to empty" — and that difference is the point of the two
// failure messages below.
func deploymentEnv() map[string]string {
	return map[string]string{
		"DATAPLANE_API_DATAPLANE_URL":      "http://dataplane.internal:8083",
		"DATAPLANE_API_SERVICE_CREDENTIAL": "a-deployment-secret",
	}
}

func TestLoadUsesExplicitDefaultsAndEnvironmentOverrides(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		unset   []string
		want    Config
		wantErr string
	}{
		{
			name: "uses explicit defaults for the values that have them",
			want: Config{
				Addr:              DefaultAddr,
				ShutdownTimeout:   DefaultShutdownTimeout,
				ReadHeaderTimeout: DefaultReadHeaderTimeout,
				DataPlaneURL:      "http://dataplane.internal:8083",
				ServiceCredential: "a-deployment-secret",
			},
		},
		{
			name: "uses supplied environment values",
			env: map[string]string{
				"DATAPLANE_API_ADDR":                "127.0.0.1:9090",
				"DATAPLANE_API_SHUTDOWN_TIMEOUT":    "15s",
				"DATAPLANE_API_READ_HEADER_TIMEOUT": "3s",
				"DATAPLANE_API_DATAPLANE_URL":       "https://dataplane.internal:9443",
				"DATAPLANE_API_SERVICE_CREDENTIAL":  "another-secret",
			},
			want: Config{
				Addr:              "127.0.0.1:9090",
				ShutdownTimeout:   15 * time.Second,
				ReadHeaderTimeout: 3 * time.Second,
				DataPlaneURL:      "https://dataplane.internal:9443",
				ServiceCredential: "another-secret",
			},
		},
		{
			// Absent and empty are different failures on purpose: an absent
			// variable is an incomplete deployment, an empty one is a deployment
			// template with a hole in it. Both stop the process, and the message
			// says which so the operator does not have to guess.
			name:    "rejects an absent Data Plane URL",
			unset:   []string{"DATAPLANE_API_DATAPLANE_URL"},
			wantErr: "DATAPLANE_API_DATAPLANE_URL must be set",
		},
		{
			name:    "rejects a Data Plane URL with no scheme",
			env:     map[string]string{"DATAPLANE_API_DATAPLANE_URL": "dataplane.internal:8083"},
			wantErr: "DATAPLANE_API_DATAPLANE_URL must use the http or https scheme",
		},
		{
			name:    "rejects a Data Plane URL on a scheme this process cannot speak",
			env:     map[string]string{"DATAPLANE_API_DATAPLANE_URL": "grpc://dataplane.internal:8083"},
			wantErr: "DATAPLANE_API_DATAPLANE_URL must use the http or https scheme",
		},
		{
			name:    "rejects a Data Plane URL with no host",
			env:     map[string]string{"DATAPLANE_API_DATAPLANE_URL": "http://"},
			wantErr: "DATAPLANE_API_DATAPLANE_URL must name a host",
		},
		{
			// A trailing slash is a path, and this is the case that catches it:
			// appending the operation's own path to a base URL ending in one
			// produces a doubled slash, which every listener in this repository
			// answers 404 because it refuses to redirect a non-canonical path.
			name:    "rejects a Data Plane URL with a trailing slash",
			env:     map[string]string{"DATAPLANE_API_DATAPLANE_URL": "http://dataplane.internal:8083/"},
			wantErr: "must be the listener's origin",
		},
		{
			name:    "rejects a Data Plane URL carrying a path",
			env:     map[string]string{"DATAPLANE_API_DATAPLANE_URL": "http://dataplane.internal:8083/management"},
			wantErr: "must be the listener's origin",
		},
		{
			name:    "rejects a Data Plane URL carrying a query",
			env:     map[string]string{"DATAPLANE_API_DATAPLANE_URL": "http://dataplane.internal:8083?tenant=1"},
			wantErr: "must be the listener's origin",
		},
		{
			name:    "rejects an absent service credential",
			unset:   []string{"DATAPLANE_API_SERVICE_CREDENTIAL"},
			wantErr: "DATAPLANE_API_SERVICE_CREDENTIAL must be set",
		},
		{
			// An empty credential would authenticate nobody, but it would do so
			// by accident — the deployment that forgot to mount the secret would
			// look configured. It is refused at startup instead.
			name:    "rejects an explicitly empty service credential",
			env:     map[string]string{"DATAPLANE_API_SERVICE_CREDENTIAL": ""},
			wantErr: "DATAPLANE_API_SERVICE_CREDENTIAL must not be empty",
		},
		{
			name: "rejects an explicitly empty address",
			env: map[string]string{
				"DATAPLANE_API_ADDR": "",
			},
			wantErr: "DATAPLANE_API_ADDR must not be empty",
		},
		{
			name: "rejects an address without a TCP port",
			env: map[string]string{
				"DATAPLANE_API_ADDR": "127.0.0.1",
			},
			wantErr: "DATAPLANE_API_ADDR must be a host:port address",
		},
		{
			name: "rejects a named TCP port",
			env: map[string]string{
				"DATAPLANE_API_ADDR": ":http",
			},
			wantErr: "DATAPLANE_API_ADDR must contain a numeric port",
		},
		{
			name: "rejects an empty shutdown timeout",
			env: map[string]string{
				"DATAPLANE_API_SHUTDOWN_TIMEOUT": "",
			},
			wantErr: "DATAPLANE_API_SHUTDOWN_TIMEOUT must not be empty",
		},
		{
			name: "rejects a zero shutdown timeout",
			env: map[string]string{
				"DATAPLANE_API_SHUTDOWN_TIMEOUT": "0s",
			},
			wantErr: "DATAPLANE_API_SHUTDOWN_TIMEOUT must be greater than zero",
		},
		{
			name: "rejects a negative read header timeout",
			env: map[string]string{
				"DATAPLANE_API_READ_HEADER_TIMEOUT": "-1s",
			},
			wantErr: "DATAPLANE_API_READ_HEADER_TIMEOUT must be greater than zero",
		},
		{
			name: "rejects a malformed duration",
			env: map[string]string{
				"DATAPLANE_API_READ_HEADER_TIMEOUT": "soon",
			},
			wantErr: "DATAPLANE_API_READ_HEADER_TIMEOUT must be a Go duration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := deploymentEnv()
			for _, name := range tt.unset {
				delete(values, name)
			}
			for name, value := range tt.env {
				values[name] = value
			}

			got, err := Load(lookup(values))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("Load() error = nil, want an error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Load() error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Load() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestLoadNeverEchoesARejectedValue pins the one property of these messages
// that is a security property rather than a usability one: a configuration
// failure reaches the process log at startup, and the value that caused it may
// be a secret. The variable's name is enough for an operator to find it.
func TestLoadNeverEchoesARejectedValue(t *testing.T) {
	secret := "a-credential-that-must-not-be-logged"

	tests := []struct {
		name  string
		env   map[string]string
		unset []string
	}{
		{
			name: "a credential the deployment left empty",
			env:  map[string]string{"DATAPLANE_API_SERVICE_CREDENTIAL": ""},
		},
		{
			name:  "a credential the deployment never supplied",
			unset: []string{"DATAPLANE_API_SERVICE_CREDENTIAL"},
		},
		{
			// A URL is the other value that could carry a secret inside it if an
			// operator pasted one in by mistake, so it is not echoed either.
			name: "a Data Plane URL the loader refuses",
			env:  map[string]string{"DATAPLANE_API_DATAPLANE_URL": "http://" + secret + "@["},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := deploymentEnv()
			for _, name := range tt.unset {
				delete(values, name)
			}
			for name, value := range tt.env {
				values[name] = value
			}

			_, err := Load(lookup(values))
			if err == nil {
				t.Fatal("Load() error = nil, want the value refused")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("Load() error = %q, want it not to echo the rejected value", err)
			}
		})
	}
}

func lookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
