package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// dsnPassword is the password every test DSN below embeds. It is the local
// fixture's development password (deploy/postgres/compose.yaml), and it
// appears in this file so the assertions can prove the one thing the DSN's
// secret-bearing nature demands: no error out of Load ever carries it.
const dsnPassword = "gateway-dev-only"

func TestLoadUsesExplicitDefaultsAndEnvironmentOverrides(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr string
	}{
		{
			name: "uses explicit defaults when no variables are set",
			env:  requiredDataPlaneEnv(),
			want: Config{
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
				DataPlane: DataPlane{
					URL:                testDataplaneURL,
					Credential:         testDataplaneCredential,
					ProjectionInterval: DefaultProjectionInterval,
					ProjectionTimeout:  DefaultProjectionTimeout,
				},
			},
		},
		{
			name: "uses supplied environment values",
			env: merge(requiredDataPlaneEnv(), map[string]string{
				"CONSOLE_API_ADDR":                "127.0.0.1:9090",
				"CONSOLE_API_SHUTDOWN_TIMEOUT":    "15s",
				"CONSOLE_API_READ_HEADER_TIMEOUT": "3s",
			}),
			want: Config{
				Addr:              "127.0.0.1:9090",
				ShutdownTimeout:   15 * time.Second,
				ReadHeaderTimeout: 3 * time.Second,
				Postgres: Postgres{
					DSN:             DefaultPostgresDSN,
					MaxOpenConns:    DefaultPostgresMaxOpenConns,
					MaxIdleConns:    DefaultPostgresMaxIdleConns,
					ConnMaxLifetime: DefaultPostgresConnMaxLifetime,
					ConnMaxIdleTime: DefaultPostgresConnMaxIdleTime,
				},
				DataPlane: DataPlane{
					URL:                testDataplaneURL,
					Credential:         testDataplaneCredential,
					ProjectionInterval: DefaultProjectionInterval,
					ProjectionTimeout:  DefaultProjectionTimeout,
				},
			},
		},
		{
			name: "uses supplied postgres values",
			env: merge(requiredDataPlaneEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_DSN":                "postgres://gateway:console-secret@db.internal:5433/control?sslmode=require",
				"CONSOLE_API_POSTGRES_MAX_OPEN_CONNS":     "25",
				"CONSOLE_API_POSTGRES_MAX_IDLE_CONNS":     "5",
				"CONSOLE_API_POSTGRES_CONN_MAX_LIFETIME":  "45m",
				"CONSOLE_API_POSTGRES_CONN_MAX_IDLE_TIME": "90s",
			}),
			want: Config{
				Addr:              DefaultAddr,
				ShutdownTimeout:   DefaultShutdownTimeout,
				ReadHeaderTimeout: DefaultReadHeaderTimeout,
				Postgres: Postgres{
					DSN:             "postgres://gateway:console-secret@db.internal:5433/control?sslmode=require",
					MaxOpenConns:    25,
					MaxIdleConns:    5,
					ConnMaxLifetime: 45 * time.Minute,
					ConnMaxIdleTime: 90 * time.Second,
				},
				DataPlane: DataPlane{
					URL:                testDataplaneURL,
					Credential:         testDataplaneCredential,
					ProjectionInterval: DefaultProjectionInterval,
					ProjectionTimeout:  DefaultProjectionTimeout,
				},
			},
		},
		{
			name: "uses supplied data plane values",
			env: merge(requiredDataPlaneEnv(), map[string]string{
				"CONSOLE_API_DATAPLANE_URL":        "https://mgmt.internal.example/gateway",
				"CONSOLE_API_DATAPLANE_CREDENTIAL": "production-management-credential",
				"CONSOLE_API_PROJECTION_INTERVAL":  "500ms",
				"CONSOLE_API_PROJECTION_TIMEOUT":   "1m",
			}),
			want: Config{
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
				DataPlane: DataPlane{
					URL:                "https://mgmt.internal.example/gateway",
					Credential:         "production-management-credential",
					ProjectionInterval: 500 * time.Millisecond,
					ProjectionTimeout:  time.Minute,
				},
			},
		},
		{
			name:    "rejects a missing data plane URL",
			env:     map[string]string{"CONSOLE_API_DATAPLANE_CREDENTIAL": testDataplaneCredential},
			wantErr: "CONSOLE_API_DATAPLANE_URL must be set",
		},
		{
			name:    "rejects a missing data plane credential",
			env:     map[string]string{"CONSOLE_API_DATAPLANE_URL": testDataplaneURL},
			wantErr: "CONSOLE_API_DATAPLANE_CREDENTIAL must be set",
		},
		{
			name: "rejects an explicitly empty data plane credential",
			env: map[string]string{
				"CONSOLE_API_DATAPLANE_URL":        testDataplaneURL,
				"CONSOLE_API_DATAPLANE_CREDENTIAL": "",
			},
			wantErr: "CONSOLE_API_DATAPLANE_CREDENTIAL must not be empty",
		},
		{
			name: "rejects a data plane URL of another scheme",
			env: map[string]string{
				"CONSOLE_API_DATAPLANE_URL":        "ftp://" + testDataplaneURL,
				"CONSOLE_API_DATAPLANE_CREDENTIAL": testDataplaneCredential,
			},
			wantErr: "CONSOLE_API_DATAPLANE_URL must use the http or https scheme",
		},
		{
			name: "rejects a data plane URL with no host",
			env: map[string]string{
				"CONSOLE_API_DATAPLANE_URL":        "http://",
				"CONSOLE_API_DATAPLANE_CREDENTIAL": testDataplaneCredential,
			},
			wantErr: "CONSOLE_API_DATAPLANE_URL must name a host",
		},
		{
			name: "rejects a data plane URL carrying a query",
			env: map[string]string{
				"CONSOLE_API_DATAPLANE_URL":        testDataplaneURL + "?route=management",
				"CONSOLE_API_DATAPLANE_CREDENTIAL": testDataplaneCredential,
			},
			wantErr: "CONSOLE_API_DATAPLANE_URL must carry no query or fragment",
		},
		{
			name: "rejects a zero projection interval",
			env: merge(requiredDataPlaneEnv(), map[string]string{
				"CONSOLE_API_PROJECTION_INTERVAL": "0s",
			}),
			wantErr: "CONSOLE_API_PROJECTION_INTERVAL must be greater than zero",
		},
		{
			name: "rejects a malformed projection timeout",
			env: merge(requiredDataPlaneEnv(), map[string]string{
				"CONSOLE_API_PROJECTION_TIMEOUT": "soon",
			}),
			wantErr: "CONSOLE_API_PROJECTION_TIMEOUT must be a Go duration",
		},
		{
			name: "rejects an explicitly empty address",
			env: merge(requiredDataPlaneEnv(), map[string]string{
				"CONSOLE_API_ADDR": "",
			}),
			wantErr: "CONSOLE_API_ADDR must not be empty",
		},
		{
			name: "rejects an address without a TCP port",
			env: merge(requiredDataPlaneEnv(), map[string]string{
				"CONSOLE_API_ADDR": "127.0.0.1",
			}),
			wantErr: "CONSOLE_API_ADDR must be a host:port address",
		},
		{
			name: "rejects a named TCP port",
			env: merge(requiredDataPlaneEnv(), map[string]string{
				"CONSOLE_API_ADDR": ":http",
			}),
			wantErr: "CONSOLE_API_ADDR must contain a numeric port",
		},
		{
			name: "rejects an empty shutdown timeout",
			env: map[string]string{
				"CONSOLE_API_SHUTDOWN_TIMEOUT": "",
			},
			wantErr: "CONSOLE_API_SHUTDOWN_TIMEOUT must not be empty",
		},
		{
			name: "rejects a zero shutdown timeout",
			env: map[string]string{
				"CONSOLE_API_SHUTDOWN_TIMEOUT": "0s",
			},
			wantErr: "CONSOLE_API_SHUTDOWN_TIMEOUT must be greater than zero",
		},
		{
			name: "rejects a negative read header timeout",
			env: map[string]string{
				"CONSOLE_API_READ_HEADER_TIMEOUT": "-1s",
			},
			wantErr: "CONSOLE_API_READ_HEADER_TIMEOUT must be greater than zero",
		},
		{
			name: "rejects a malformed duration",
			env: map[string]string{
				"CONSOLE_API_READ_HEADER_TIMEOUT": "soon",
			},
			wantErr: "CONSOLE_API_READ_HEADER_TIMEOUT must be a Go duration",
		},
		{
			name: "rejects an explicitly empty postgres DSN",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_DSN": "",
			},
			wantErr: "CONSOLE_API_POSTGRES_DSN must not be empty",
		},
		{
			name: "rejects an explicitly empty max open conns",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_MAX_OPEN_CONNS": "",
			},
			wantErr: "CONSOLE_API_POSTGRES_MAX_OPEN_CONNS must not be empty",
		},
		{
			name: "rejects a non-integer max open conns",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_MAX_OPEN_CONNS": "plenty",
			},
			wantErr: "CONSOLE_API_POSTGRES_MAX_OPEN_CONNS must be an integer",
		},
		{
			name: "rejects a zero max open conns",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_MAX_OPEN_CONNS": "0",
			},
			wantErr: "CONSOLE_API_POSTGRES_MAX_OPEN_CONNS must be greater than zero",
		},
		{
			name: "rejects a negative max open conns",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_MAX_OPEN_CONNS": "-1",
			},
			wantErr: "CONSOLE_API_POSTGRES_MAX_OPEN_CONNS must be greater than zero",
		},
		{
			name: "rejects a negative max idle conns",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_MAX_IDLE_CONNS": "-1",
			},
			wantErr: "CONSOLE_API_POSTGRES_MAX_IDLE_CONNS must not be negative",
		},
		{
			name: "rejects max idle conns above max open conns",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_MAX_OPEN_CONNS": "2",
				"CONSOLE_API_POSTGRES_MAX_IDLE_CONNS": "5",
			},
			wantErr: "CONSOLE_API_POSTGRES_MAX_IDLE_CONNS (5) must not exceed CONSOLE_API_POSTGRES_MAX_OPEN_CONNS (2)",
		},
		{
			name: "rejects an explicitly empty conn max lifetime",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_CONN_MAX_LIFETIME": "",
			},
			wantErr: "CONSOLE_API_POSTGRES_CONN_MAX_LIFETIME must not be empty",
		},
		{
			name: "rejects a zero conn max lifetime",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_CONN_MAX_LIFETIME": "0s",
			},
			wantErr: "CONSOLE_API_POSTGRES_CONN_MAX_LIFETIME must be greater than zero",
		},
		{
			name: "rejects a malformed conn max idle time",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_CONN_MAX_IDLE_TIME": "soon",
			},
			wantErr: "CONSOLE_API_POSTGRES_CONN_MAX_IDLE_TIME must be a Go duration",
		},
		{
			name: "rejects a postgres DSN that is not a URL",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_DSN": "postgres://gateway:" + dsnPassword + "@127.0.0.1:5432/contr%zzol",
			},
			wantErr: "CONSOLE_API_POSTGRES_DSN must be a URL",
		},
		{
			name: "rejects a postgres DSN of another scheme",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_DSN": "mysql://gateway:" + dsnPassword + "@127.0.0.1:3306/control",
			},
			wantErr: "CONSOLE_API_POSTGRES_DSN must be a postgres:// or postgresql:// URL",
		},
		{
			name: "rejects a postgres DSN with no database in its path",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_DSN": "postgres://gateway:" + dsnPassword + "@127.0.0.1:5432/?sslmode=disable",
			},
			wantErr: "CONSOLE_API_POSTGRES_DSN must name exactly one database in its path",
		},
		{
			name: "rejects a postgres DSN naming the other plane's database",
			env: map[string]string{
				"CONSOLE_API_POSTGRES_DSN": "postgres://gateway:" + dsnPassword + "@127.0.0.1:5432/dataplane?sslmode=disable",
			},
			wantErr: "CONSOLE_API_POSTGRES_DSN names database \"dataplane\", but this application owns only the control database",
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
				if strings.Contains(err.Error(), dsnPassword) {
					t.Errorf("Load() error = %q, must not carry the DSN's password", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got != tt.want {
				// Redacted, not %#v-raw: a dumped Config carries Postgres.DSN,
				// and a test failure is CI output the whole world can read.
				t.Errorf("Load() = %s, want %s", redactDSN(got), redactDSN(tt.want))
			}
		})
	}
}

// testDataplaneURL and testDataplaneCredential are the data plane settings
// every success case above needs: the URL and credential are required, so a
// test that wants Load to succeed always supplies both. The credential is a
// test value, not the fixture's — the assertion that no Load error carries a
// secret is about the DSN's password, and this one never appears in an error
// at all.
const (
	testDataplaneURL        = "http://dataplane-api.internal:8082"
	testDataplaneCredential = "dev-management-credential"
)

// requiredDataPlaneEnv is the smallest environment Load accepts: the two
// required data plane values, everything else defaulted.
func requiredDataPlaneEnv() map[string]string {
	return map[string]string{
		"CONSOLE_API_DATAPLANE_URL":        testDataplaneURL,
		"CONSOLE_API_DATAPLANE_CREDENTIAL": testDataplaneCredential,
	}
}

func merge(base, extra map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(extra))
	for name, value := range base {
		merged[name] = value
	}
	for name, value := range extra {
		merged[name] = value
	}
	return merged
}

func TestDataPlaneLogValueRedactsTheCredential(t *testing.T) {
	dataPlane := DataPlane{
		URL:                testDataplaneURL,
		Credential:         testDataplaneCredential,
		ProjectionInterval: DefaultProjectionInterval,
		ProjectionTimeout:  DefaultProjectionTimeout,
	}

	value := dataPlane.LogValue().String()
	if strings.Contains(value, testDataplaneCredential) {
		t.Errorf("LogValue() = %q, must not carry the management credential", value)
	}
	if !strings.Contains(value, "[redacted]") {
		t.Errorf("LogValue() = %q, want the redaction marker", value)
	}
	for _, want := range []string{testDataplaneURL, "projection_interval", "projection_timeout"} {
		if !strings.Contains(value, want) {
			t.Errorf("LogValue() = %q, want it to name %s", value, want)
		}
	}
}

func TestPostgresLogValueRendersTheTargetAndRedactsTheCredential(t *testing.T) {
	postgres := Postgres{
		DSN:             "postgres://gateway:" + dsnPassword + "@db.internal:5433/control?sslmode=require",
		MaxOpenConns:    10,
		MaxIdleConns:    2,
		ConnMaxLifetime: 30 * time.Minute,
		ConnMaxIdleTime: 5 * time.Minute,
	}

	value := postgres.LogValue().String()
	if strings.Contains(value, dsnPassword) {
		t.Errorf("LogValue() = %q, must not carry the DSN's password", value)
	}
	for _, want := range []string{"postgres", "db.internal", "5433", "control"} {
		if !strings.Contains(value, want) {
			t.Errorf("LogValue() = %q, want it to name the target's %s", value, want)
		}
	}
}

func TestPostgresLogValueRedactsADSNItCannotParseWhole(t *testing.T) {
	// Validation at load time makes an unparsable DSN unreachable here, but
	// LogValue must be safe on any Postgres value whatever built it: the one
	// field it refuses to summarise is redacted whole rather than echoed.
	postgres := Postgres{DSN: "postgres://gateway:" + dsnPassword + "@127.0.0.1:5432/contr%zzol"}

	value := postgres.LogValue().String()
	if strings.Contains(value, dsnPassword) {
		t.Errorf("LogValue() = %q, must not carry the DSN's password", value)
	}
	if !strings.Contains(value, "[redacted]") {
		t.Errorf("LogValue() = %q, want the redaction marker", value)
	}
}

func lookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

// redactDSN renders a Config for a test-failure message with the DSN struck
// out: a raw %#v of the struct would quote Postgres.DSN, and a test failure
// is CI output the whole world can read.
func redactDSN(c Config) string {
	return strings.ReplaceAll(fmt.Sprintf("%#v", c), c.Postgres.DSN, "<redacted dsn>")
}
