package config

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// postgresDefaults is the Postgres half of the configuration the loader
// returns when none of the DATAPLANE_POSTGRES_* variables is set — the local
// development fixture's DSN and the pool sizes documented beside the default
// constants. Each table case states only the fields it is about.
func postgresDefaults() Postgres {
	return Postgres{
		DSN:             DefaultPostgresDSN,
		MaxOpenConns:    DefaultPostgresMaxOpenConns,
		MaxIdleConns:    DefaultPostgresMaxIdleConns,
		ConnMaxLifetime: DefaultPostgresConnMaxLifetime,
		ConnMaxIdleTime: DefaultPostgresConnMaxIdleTime,
	}
}

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
				Postgres:          postgresDefaults(),
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
				Postgres:          postgresDefaults(),
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
		{
			name: "serves no management surface unless one is configured",
			env: map[string]string{
				"DATAPLANE_ADDR": "127.0.0.1:9090",
			},
			want: Config{
				Addr:              "127.0.0.1:9090",
				ShutdownTimeout:   DefaultShutdownTimeout,
				ReadHeaderTimeout: DefaultReadHeaderTimeout,
				Postgres:          postgresDefaults(),
			},
		},
		{
			name: "configures the management listener and its credential together",
			env: map[string]string{
				"DATAPLANE_MANAGEMENT_ADDR":  "127.0.0.1:9091",
				"DATAPLANE_MANAGEMENT_TOKEN": "a-service-credential",
			},
			want: Config{
				Addr:              DefaultAddr,
				ShutdownTimeout:   DefaultShutdownTimeout,
				ReadHeaderTimeout: DefaultReadHeaderTimeout,
				ManagementAddr:    "127.0.0.1:9091",
				ManagementToken:   "a-service-credential",
				Postgres:          postgresDefaults(),
			},
		},
		{
			name: "uses a supplied Postgres DSN, including the postgresql scheme",
			env: map[string]string{
				"DATAPLANE_POSTGRES_DSN": "postgresql://gateway:not-the-fixture-password@db.internal:5433/dataplane?sslmode=require",
			},
			want: Config{
				Addr:              DefaultAddr,
				ShutdownTimeout:   DefaultShutdownTimeout,
				ReadHeaderTimeout: DefaultReadHeaderTimeout,
				Postgres: Postgres{
					DSN:             "postgresql://gateway:not-the-fixture-password@db.internal:5433/dataplane?sslmode=require",
					MaxOpenConns:    DefaultPostgresMaxOpenConns,
					MaxIdleConns:    DefaultPostgresMaxIdleConns,
					ConnMaxLifetime: DefaultPostgresConnMaxLifetime,
					ConnMaxIdleTime: DefaultPostgresConnMaxIdleTime,
				},
			},
		},
		{
			name: "uses supplied pool sizes and connection lifetimes",
			env: map[string]string{
				"DATAPLANE_POSTGRES_MAX_OPEN_CONNS":     "10",
				"DATAPLANE_POSTGRES_MAX_IDLE_CONNS":     "2",
				"DATAPLANE_POSTGRES_CONN_MAX_LIFETIME":  "1h",
				"DATAPLANE_POSTGRES_CONN_MAX_IDLE_TIME": "45s",
			},
			want: Config{
				Addr:              DefaultAddr,
				ShutdownTimeout:   DefaultShutdownTimeout,
				ReadHeaderTimeout: DefaultReadHeaderTimeout,
				Postgres: Postgres{
					DSN:             DefaultPostgresDSN,
					MaxOpenConns:    10,
					MaxIdleConns:    2,
					ConnMaxLifetime: time.Hour,
					ConnMaxIdleTime: 45 * time.Second,
				},
			},
		},
		{
			name: "rejects an explicitly empty Postgres DSN",
			env: map[string]string{
				"DATAPLANE_POSTGRES_DSN": "",
			},
			wantErr: "DATAPLANE_POSTGRES_DSN must not be empty",
		},
		{
			name: "rejects an explicitly empty max open conns",
			env: map[string]string{
				"DATAPLANE_POSTGRES_MAX_OPEN_CONNS": "",
			},
			wantErr: "DATAPLANE_POSTGRES_MAX_OPEN_CONNS must not be empty",
		},
		{
			name: "rejects an explicitly empty max idle conns",
			env: map[string]string{
				"DATAPLANE_POSTGRES_MAX_IDLE_CONNS": "",
			},
			wantErr: "DATAPLANE_POSTGRES_MAX_IDLE_CONNS must not be empty",
		},
		{
			name: "rejects an explicitly empty conn max lifetime",
			env: map[string]string{
				"DATAPLANE_POSTGRES_CONN_MAX_LIFETIME": "",
			},
			wantErr: "DATAPLANE_POSTGRES_CONN_MAX_LIFETIME must not be empty",
		},
		{
			name: "rejects an explicitly empty conn max idle time",
			env: map[string]string{
				"DATAPLANE_POSTGRES_CONN_MAX_IDLE_TIME": "",
			},
			wantErr: "DATAPLANE_POSTGRES_CONN_MAX_IDLE_TIME must not be empty",
		},
		{
			name: "rejects a max open conns that is not an integer",
			env: map[string]string{
				"DATAPLANE_POSTGRES_MAX_OPEN_CONNS": "plenty",
			},
			wantErr: "DATAPLANE_POSTGRES_MAX_OPEN_CONNS must be an integer",
		},
		{
			name: "rejects a zero max open conns",
			env: map[string]string{
				"DATAPLANE_POSTGRES_MAX_OPEN_CONNS": "0",
			},
			wantErr: "DATAPLANE_POSTGRES_MAX_OPEN_CONNS must be greater than zero",
		},
		{
			name: "rejects a negative max open conns",
			env: map[string]string{
				"DATAPLANE_POSTGRES_MAX_OPEN_CONNS": "-1",
			},
			wantErr: "DATAPLANE_POSTGRES_MAX_OPEN_CONNS must be greater than zero",
		},
		{
			name: "rejects a negative max idle conns",
			env: map[string]string{
				"DATAPLANE_POSTGRES_MAX_IDLE_CONNS": "-1",
			},
			wantErr: "DATAPLANE_POSTGRES_MAX_IDLE_CONNS must not be negative",
		},
		{
			name: "rejects a max idle conns greater than max open conns",
			env: map[string]string{
				"DATAPLANE_POSTGRES_MAX_OPEN_CONNS": "5",
				"DATAPLANE_POSTGRES_MAX_IDLE_CONNS": "10",
			},
			wantErr: "DATAPLANE_POSTGRES_MAX_IDLE_CONNS (10) must not be greater than DATAPLANE_POSTGRES_MAX_OPEN_CONNS (5)",
		},
		{
			name: "rejects a malformed conn max lifetime",
			env: map[string]string{
				"DATAPLANE_POSTGRES_CONN_MAX_LIFETIME": "forever",
			},
			wantErr: "DATAPLANE_POSTGRES_CONN_MAX_LIFETIME must be a Go duration",
		},
		{
			name: "rejects a zero conn max idle time",
			env: map[string]string{
				"DATAPLANE_POSTGRES_CONN_MAX_IDLE_TIME": "0s",
			},
			wantErr: "DATAPLANE_POSTGRES_CONN_MAX_IDLE_TIME must be greater than zero",
		},
		{
			name: "rejects a DSN that is not a URL",
			env: map[string]string{
				"DATAPLANE_POSTGRES_DSN": "postgres://%zz@127.0.0.1:5432/dataplane",
			},
			wantErr: "DATAPLANE_POSTGRES_DSN must be a parsable URL",
		},
		{
			name: "rejects a DSN with a foreign scheme",
			env: map[string]string{
				"DATAPLANE_POSTGRES_DSN": "mysql://gateway:not-the-fixture-password@127.0.0.1:5432/dataplane",
			},
			wantErr: "DATAPLANE_POSTGRES_DSN must use the postgres or postgresql scheme",
		},
		{
			name: "rejects a DSN with no database in its path",
			env: map[string]string{
				"DATAPLANE_POSTGRES_DSN": "postgres://gateway:not-the-fixture-password@127.0.0.1:5432/",
			},
			wantErr: "DATAPLANE_POSTGRES_DSN must name exactly one database in its path",
		},
		{
			name: "rejects a DSN pointed at another plane's database",
			env: map[string]string{
				"DATAPLANE_POSTGRES_DSN": "postgres://gateway:not-the-fixture-password@127.0.0.1:5432/control?sslmode=disable",
			},
			wantErr: `DATAPLANE_POSTGRES_DSN names the "control" database; this application owns only the dataplane database`,
		},
		{
			name: "rejects an explicitly empty management address",
			env: map[string]string{
				"DATAPLANE_MANAGEMENT_ADDR": "",
			},
			wantErr: "DATAPLANE_MANAGEMENT_ADDR must not be empty",
		},
		{
			name: "rejects a management address without a TCP port",
			env: map[string]string{
				"DATAPLANE_MANAGEMENT_ADDR":  "127.0.0.1",
				"DATAPLANE_MANAGEMENT_TOKEN": "a-service-credential",
			},
			wantErr: "DATAPLANE_MANAGEMENT_ADDR must be a host:port address",
		},
		{
			name: "refuses a management listener with no credential",
			env: map[string]string{
				"DATAPLANE_MANAGEMENT_ADDR": "127.0.0.1:9091",
			},
			wantErr: "DATAPLANE_MANAGEMENT_TOKEN is required",
		},
		{
			name: "refuses an explicitly empty management credential",
			env: map[string]string{
				"DATAPLANE_MANAGEMENT_ADDR":  "127.0.0.1:9091",
				"DATAPLANE_MANAGEMENT_TOKEN": "",
			},
			wantErr: "DATAPLANE_MANAGEMENT_TOKEN must not be empty",
		},
		{
			name: "refuses a credential with no listener to read it",
			env: map[string]string{
				"DATAPLANE_MANAGEMENT_TOKEN": "a-service-credential",
			},
			wantErr: "DATAPLANE_MANAGEMENT_TOKEN is set but DATAPLANE_MANAGEMENT_ADDR is not",
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
				// Every rejection in this table is a string an operator will
				// paste somewhere, and two of the rejected DSNs above carry a
				// password on purpose: no error may carry one back out.
				if strings.Contains(err.Error(), "not-the-fixture-password") {
					t.Errorf("Load() error = %q, want it to carry no DSN password", err)
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

func TestLoadErrorsNeverCarryThePostgresPassword(t *testing.T) {
	const password = "a-password-only-the-DSN-knows"
	dsns := []string{
		"", // explicitly empty
		"postgres://%zz@127.0.0.1:5432/dataplane",                    // not a parsable URL
		"mysql://gateway:" + password + "@127.0.0.1:5432/dataplane",  // wrong scheme
		"postgres://gateway:" + password + "@127.0.0.1:5432/",        // no database
		"postgres://gateway:" + password + "@127.0.0.1:5432/control", // another plane's database
	}

	for _, dsn := range dsns {
		_, err := Load(lookup(map[string]string{"DATAPLANE_POSTGRES_DSN": dsn}))
		if err == nil {
			t.Fatalf("Load() with DSN %q error = nil, want an error", dsn)
		}
		if strings.Contains(err.Error(), password) {
			t.Errorf("Load() error = %q, want it to carry no DSN password", err)
		}
	}
}

func TestPostgresLogValueRendersWhereThePoolPointsAndNeverThePassword(t *testing.T) {
	p := Postgres{
		DSN:             "postgres://gateway:a-quiet-password@10.0.0.1:5433/dataplane?sslmode=require",
		MaxOpenConns:    25,
		MaxIdleConns:    5,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
	}

	rendered := renderLogValue(p.LogValue())
	for _, want := range []string{"scheme=postgres", "host=10.0.0.1", "port=5433", "database=dataplane", "max_open_conns=25", "max_idle_conns=5"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("LogValue() = %q, want it to contain %q — the location and sizing are the operational facts a log line is for", rendered, want)
		}
	}
	for _, banned := range []string{"a-quiet-password", "postgres://"} {
		if strings.Contains(rendered, banned) {
			t.Errorf("LogValue() = %q, want it to carry none of the DSN's credential", rendered)
		}
	}
}

func TestPostgresLogValueRedactsADSNCannotBeDecomposed(t *testing.T) {
	p := Postgres{DSN: "postgres://%zz@gateway-host:5432/dataplane"}

	rendered := renderLogValue(p.LogValue())
	if !strings.Contains(rendered, "unparsable") {
		t.Errorf("LogValue() = %q, want the unparsable DSN flagged rather than echoed", rendered)
	}
	if strings.Contains(rendered, "gateway-host") {
		t.Errorf("LogValue() = %q, want it to carry nothing of a DSN it could not parse", rendered)
	}
}

// renderLogValue flattens one slog group value into key=value pairs, so an
// assertion can read a redacted log line the way a log would print it.
func renderLogValue(value slog.Value) string {
	parts := make([]string, 0, len(value.Group()))
	for _, attr := range value.Group() {
		parts = append(parts, fmt.Sprintf("%s=%s", attr.Key, attr.Value.String()))
	}
	return strings.Join(parts, " ")
}

func lookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
