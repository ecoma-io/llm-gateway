package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadUsesExplicitDefaultsAndEnvironmentOverrides(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr string
	}{
		{
			name: "uses explicit defaults when no variables are set",
			want: Config{
				Addr:              DefaultAddr,
				ShutdownTimeout:   DefaultShutdownTimeout,
				ReadHeaderTimeout: DefaultReadHeaderTimeout,
			},
		},
		{
			name: "uses supplied environment values",
			env: map[string]string{
				"DATAPLANE_ADDR":                "127.0.0.1:9090",
				"DATAPLANE_SHUTDOWN_TIMEOUT":    "15s",
				"DATAPLANE_READ_HEADER_TIMEOUT": "3s",
			},
			want: Config{
				Addr:              "127.0.0.1:9090",
				ShutdownTimeout:   15 * time.Second,
				ReadHeaderTimeout: 3 * time.Second,
			},
		},
		{
			name: "rejects an explicitly empty address",
			env: map[string]string{
				"DATAPLANE_ADDR": "",
			},
			wantErr: "DATAPLANE_ADDR must not be empty",
		},
		{
			name: "rejects an address without a TCP port",
			env: map[string]string{
				"DATAPLANE_ADDR": "127.0.0.1",
			},
			wantErr: "DATAPLANE_ADDR must be a host:port address",
		},
		{
			name: "rejects a named TCP port",
			env: map[string]string{
				"DATAPLANE_ADDR": ":http",
			},
			wantErr: "DATAPLANE_ADDR must contain a numeric port",
		},
		{
			name: "rejects an empty shutdown timeout",
			env: map[string]string{
				"DATAPLANE_SHUTDOWN_TIMEOUT": "",
			},
			wantErr: "DATAPLANE_SHUTDOWN_TIMEOUT must not be empty",
		},
		{
			name: "rejects a zero shutdown timeout",
			env: map[string]string{
				"DATAPLANE_SHUTDOWN_TIMEOUT": "0s",
			},
			wantErr: "DATAPLANE_SHUTDOWN_TIMEOUT must be greater than zero",
		},
		{
			name: "rejects a negative read header timeout",
			env: map[string]string{
				"DATAPLANE_READ_HEADER_TIMEOUT": "-1s",
			},
			wantErr: "DATAPLANE_READ_HEADER_TIMEOUT must be greater than zero",
		},
		{
			name: "rejects a malformed duration",
			env: map[string]string{
				"DATAPLANE_READ_HEADER_TIMEOUT": "soon",
			},
			wantErr: "DATAPLANE_READ_HEADER_TIMEOUT must be a Go duration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(lookup(tt.env))
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

func lookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
