package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// dsnPassword is the password every test DSN below embeds. It is the local
// fixture's development password (deploy/postgres/compose.yaml), and it
// appears in this file so the assertions can prove the one thing the DSN's
// secret-bearing nature demands: no error out of Load ever carries it.
const dsnPassword = "gateway-dev-only"

// testAnalyticsScope is the scope table every case below starts from: one
// credential naming one account. It is a secret-bearing test value in the same
// way the DSN password is, and the assertion that no error out of Load ever
// carries it is the same one.
const testAnalyticsScope = `{"console-test-token":"018f0000-0000-7000-8000-000000000001"}`

// testAnalytics is that same table once parsed, which is what every case below
// that succeeds expects. It is a function rather than a var because the map is
// shared: a case that wrote to it would change what every later case sees.
func testAnalytics() Analytics {
	return Analytics{Scope: map[string]string{
		"console-test-token": "018f0000-0000-7000-8000-000000000001",
	}}
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
			env:  requiredEnv(),
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
					IngestionInterval:  DefaultIngestionInterval,
					IngestionTimeout:   DefaultIngestionTimeout,
				},
				Reconciliation: defaultReconciliation(),
				Analytics:      testAnalytics(),
			},
		},
		{
			name: "uses supplied environment values",
			env: merge(requiredEnv(), map[string]string{
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
					IngestionInterval:  DefaultIngestionInterval,
					IngestionTimeout:   DefaultIngestionTimeout,
				},
				Reconciliation: defaultReconciliation(),
				Analytics:      testAnalytics(),
			},
		},
		{
			name: "uses supplied postgres values",
			env: merge(requiredEnv(), map[string]string{
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
					IngestionInterval:  DefaultIngestionInterval,
					IngestionTimeout:   DefaultIngestionTimeout,
				},
				Reconciliation: defaultReconciliation(),
				Analytics:      testAnalytics(),
			},
		},
		{
			name: "uses supplied data plane values",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_DATAPLANE_URL":        "https://mgmt.internal.example/gateway",
				"CONSOLE_API_DATAPLANE_CREDENTIAL": "production-management-credential",
				"CONSOLE_API_PROJECTION_INTERVAL":  "500ms",
				"CONSOLE_API_PROJECTION_TIMEOUT":   "1m",
				"CONSOLE_API_INGESTION_INTERVAL":   "2s",
				"CONSOLE_API_INGESTION_TIMEOUT":    "45s",
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
					IngestionInterval:  2 * time.Second,
					IngestionTimeout:   45 * time.Second,
				},
				Reconciliation: defaultReconciliation(),
				Analytics:      testAnalytics(),
			},
		},
		{
			name: "rejects a missing data plane URL",
			// WITHOUT the base: the whole point of this case is a variable that
			// is not set at all, so it cannot be merged over a table that sets
			// it. The scope table is present because the case is about the Data
			// Plane URL, and a case that failed on the missing scope instead
			// would not be about the thing it names.
			env:     map[string]string{"CONSOLE_API_ANALYTICS_SCOPE": testAnalyticsScope, "CONSOLE_API_DATAPLANE_CREDENTIAL": testDataplaneCredential},
			wantErr: "CONSOLE_API_DATAPLANE_URL must be set",
		},
		{
			name:    "rejects a missing data plane credential",
			env:     map[string]string{"CONSOLE_API_ANALYTICS_SCOPE": testAnalyticsScope, "CONSOLE_API_DATAPLANE_URL": testDataplaneURL},
			wantErr: "CONSOLE_API_DATAPLANE_CREDENTIAL must be set",
		},
		{
			name: "rejects an explicitly empty data plane credential",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_DATAPLANE_URL":        testDataplaneURL,
				"CONSOLE_API_DATAPLANE_CREDENTIAL": "",
			}),
			wantErr: "CONSOLE_API_DATAPLANE_CREDENTIAL must not be empty",
		},
		{
			name: "rejects a data plane URL of another scheme",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_DATAPLANE_URL":        "ftp://" + testDataplaneURL,
				"CONSOLE_API_DATAPLANE_CREDENTIAL": testDataplaneCredential,
			}),
			wantErr: "CONSOLE_API_DATAPLANE_URL must use the http or https scheme",
		},
		{
			name: "rejects a data plane URL with no host",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_DATAPLANE_URL":        "http://",
				"CONSOLE_API_DATAPLANE_CREDENTIAL": testDataplaneCredential,
			}),
			wantErr: "CONSOLE_API_DATAPLANE_URL must name a host",
		},
		{
			name: "rejects a data plane URL carrying a query",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_DATAPLANE_URL":        testDataplaneURL + "?route=management",
				"CONSOLE_API_DATAPLANE_CREDENTIAL": testDataplaneCredential,
			}),
			wantErr: "CONSOLE_API_DATAPLANE_URL must carry no query or fragment",
		},
		{
			// The scope is DELETED rather than set empty, because those are two
			// different refusals: an unset variable is a deployment that never
			// configured access control, and an empty one is a deployment that
			// configured it to nothing. The error text says which.
			name:    "rejects a missing analytics scope",
			env:     without(requiredEnv(), "CONSOLE_API_ANALYTICS_SCOPE"),
			wantErr: "CONSOLE_API_ANALYTICS_SCOPE must be set",
		},
		{
			name: "rejects an explicitly empty analytics scope",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_ANALYTICS_SCOPE": "",
			}),
			wantErr: "CONSOLE_API_ANALYTICS_SCOPE must not be empty",
		},
		{
			name: "rejects an analytics scope that is not an object",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_ANALYTICS_SCOPE": `"console-test-token"`,
			}),
			wantErr: "must be a JSON object of credential to account id",
		},
		{
			name: "rejects an analytics scope that is not JSON",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_ANALYTICS_SCOPE": `{"console-test-token":"018f0000-0000-7000-8000-000000000001"`,
			}),
			wantErr: "must be a JSON object of credential to account id",
		},
		{
			name: "rejects an analytics scope with no entries",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_ANALYTICS_SCOPE": "{}",
			}),
			wantErr: "must name at least one credential",
		},
		{
			name: "rejects an analytics scope with an empty credential",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_ANALYTICS_SCOPE": `{"":"018f0000-0000-7000-8000-000000000001"}`,
			}),
			wantErr: "carries an empty credential",
		},
		{
			name: "rejects an analytics scope with an empty account id",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_ANALYTICS_SCOPE": `{"console-test-token":""}`,
			}),
			wantErr: "maps a credential to an empty account id",
		},
		{
			name: "rejects a zero projection interval",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_PROJECTION_INTERVAL": "0s",
			}),
			wantErr: "CONSOLE_API_PROJECTION_INTERVAL must be greater than zero",
		},
		{
			name: "rejects a malformed projection timeout",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_PROJECTION_TIMEOUT": "soon",
			}),
			wantErr: "CONSOLE_API_PROJECTION_TIMEOUT must be a Go duration",
		},
		{
			name: "rejects a zero ingestion interval",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_INGESTION_INTERVAL": "0s",
			}),
			wantErr: "CONSOLE_API_INGESTION_INTERVAL must be greater than zero",
		},
		{
			name: "rejects a malformed ingestion timeout",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_INGESTION_TIMEOUT": "soon",
			}),
			wantErr: "CONSOLE_API_INGESTION_TIMEOUT must be a Go duration",
		},
		{
			name: "uses supplied reconciliation values",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_RECONCILIATION_INTERVAL": "90s",
				"CONSOLE_API_RECONCILIATION_TIMEOUT":  "10m",
				"CONSOLE_API_RECONCILIATION_BATCH":    "250",
				"CONSOLE_API_RECONCILIATION_LOOKBACK": "48h",
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
					URL:                testDataplaneURL,
					Credential:         testDataplaneCredential,
					ProjectionInterval: DefaultProjectionInterval,
					ProjectionTimeout:  DefaultProjectionTimeout,
					IngestionInterval:  DefaultIngestionInterval,
					IngestionTimeout:   DefaultIngestionTimeout,
				},
				Reconciliation: Reconciliation{
					Interval: 90 * time.Second,
					Timeout:  10 * time.Minute,
					Batch:    250,
					Lookback: 48 * time.Hour,
				},
				Analytics: testAnalytics(),
			},
		},
		{
			name: "rejects a zero reconciliation interval",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_RECONCILIATION_INTERVAL": "0s",
			}),
			wantErr: "CONSOLE_API_RECONCILIATION_INTERVAL must be greater than zero",
		},
		{
			name: "rejects a malformed reconciliation timeout",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_RECONCILIATION_TIMEOUT": "soon",
			}),
			wantErr: "CONSOLE_API_RECONCILIATION_TIMEOUT must be a Go duration",
		},
		{
			name: "rejects a zero reconciliation batch",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_RECONCILIATION_BATCH": "0",
			}),
			wantErr: "CONSOLE_API_RECONCILIATION_BATCH must be greater than zero",
		},
		{
			name: "rejects a zero reconciliation lookback",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_RECONCILIATION_LOOKBACK": "0s",
			}),
			wantErr: "CONSOLE_API_RECONCILIATION_LOOKBACK must be greater than zero",
		},
		{
			// EQUALITY is accepted, and the case says why. A pass that spends
			// its whole budget ends as the next one is due: a tight loop, not
			// two passes sweeping overlapping windows. The window claim is what
			// cannot catch the overlapping shape, and the reason it is named
			// here is that this is the boundary the old rule drew one step too
			// tight — the shipped defaults sat exactly on it, so a plan of
			// "timeout equal to interval" read as a refusal rather than as the
			// tight loop it is.
			name: "accepts a reconciliation timeout equal to the interval",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_RECONCILIATION_INTERVAL": "30s",
				"CONSOLE_API_RECONCILIATION_TIMEOUT":  "30s",
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
					URL:                testDataplaneURL,
					Credential:         testDataplaneCredential,
					ProjectionInterval: DefaultProjectionInterval,
					ProjectionTimeout:  DefaultProjectionTimeout,
					IngestionInterval:  DefaultIngestionInterval,
					IngestionTimeout:   DefaultIngestionTimeout,
				},
				Reconciliation: Reconciliation{
					Interval: 30 * time.Second,
					Timeout:  30 * time.Second,
					Batch:    DefaultReconciliationBatch,
					Lookback: DefaultReconciliationLookback,
				},
				Analytics: testAnalytics(),
			},
		},
		{
			name: "rejects a reconciliation timeout below the interval",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_RECONCILIATION_INTERVAL": "1m",
				"CONSOLE_API_RECONCILIATION_TIMEOUT":  "10s",
			}),
			wantErr: "CONSOLE_API_RECONCILIATION_TIMEOUT (10s) must be at least CONSOLE_API_RECONCILIATION_INTERVAL (" + time.Minute.String() + ")",
		},
		{
			// The ceiling is a shutdown budget, not a work budget: a pass
			// still running when the signal arrives is waited for on the tail
			// this process grants its in-flight work, and a budget past that
			// tail is a pass whose overrun only SIGKILL can end.
			name: "rejects a reconciliation timeout past the shutdown tail",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_RECONCILIATION_INTERVAL": "1m",
				"CONSOLE_API_RECONCILIATION_TIMEOUT":  "24h",
			}),
			wantErr: "CONSOLE_API_RECONCILIATION_TIMEOUT (24h0m0s) must be at most 15m0s",
		},
		{
			name: "rejects a reconciliation lookback narrower than the interval",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_RECONCILIATION_INTERVAL": "10m",
				"CONSOLE_API_RECONCILIATION_TIMEOUT":  "20m",
				"CONSOLE_API_RECONCILIATION_LOOKBACK": "5m",
			}),
			wantErr: "CONSOLE_API_RECONCILIATION_LOOKBACK (" + (5 * time.Minute).String() + ") must be at least CONSOLE_API_RECONCILIATION_INTERVAL (" + (10 * time.Minute).String() + ")",
		},
		{
			name: "rejects an explicitly empty address",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_ADDR": "",
			}),
			wantErr: "CONSOLE_API_ADDR must not be empty",
		},
		{
			name: "rejects an address without a TCP port",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_ADDR": "127.0.0.1",
			}),
			wantErr: "CONSOLE_API_ADDR must be a host:port address",
		},
		{
			name: "rejects a named TCP port",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_ADDR": ":http",
			}),
			wantErr: "CONSOLE_API_ADDR must contain a numeric port",
		},
		{
			name: "rejects an empty shutdown timeout",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_SHUTDOWN_TIMEOUT": "",
			}),
			wantErr: "CONSOLE_API_SHUTDOWN_TIMEOUT must not be empty",
		},
		{
			name: "rejects a zero shutdown timeout",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_SHUTDOWN_TIMEOUT": "0s",
			}),
			wantErr: "CONSOLE_API_SHUTDOWN_TIMEOUT must be greater than zero",
		},
		{
			name: "rejects a negative read header timeout",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_READ_HEADER_TIMEOUT": "-1s",
			}),
			wantErr: "CONSOLE_API_READ_HEADER_TIMEOUT must be greater than zero",
		},
		{
			name: "rejects a malformed duration",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_READ_HEADER_TIMEOUT": "soon",
			}),
			wantErr: "CONSOLE_API_READ_HEADER_TIMEOUT must be a Go duration",
		},
		{
			name: "rejects an explicitly empty postgres DSN",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_DSN": "",
			}),
			wantErr: "CONSOLE_API_POSTGRES_DSN must not be empty",
		},
		{
			name: "rejects an explicitly empty max open conns",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_MAX_OPEN_CONNS": "",
			}),
			wantErr: "CONSOLE_API_POSTGRES_MAX_OPEN_CONNS must not be empty",
		},
		{
			name: "rejects a non-integer max open conns",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_MAX_OPEN_CONNS": "plenty",
			}),
			wantErr: "CONSOLE_API_POSTGRES_MAX_OPEN_CONNS must be an integer",
		},
		{
			name: "rejects a zero max open conns",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_MAX_OPEN_CONNS": "0",
			}),
			wantErr: "CONSOLE_API_POSTGRES_MAX_OPEN_CONNS must be greater than zero",
		},
		{
			name: "rejects a negative max open conns",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_MAX_OPEN_CONNS": "-1",
			}),
			wantErr: "CONSOLE_API_POSTGRES_MAX_OPEN_CONNS must be greater than zero",
		},
		{
			name: "rejects a negative max idle conns",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_MAX_IDLE_CONNS": "-1",
			}),
			wantErr: "CONSOLE_API_POSTGRES_MAX_IDLE_CONNS must not be negative",
		},
		{
			name: "rejects max idle conns above max open conns",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_MAX_OPEN_CONNS": "2",
				"CONSOLE_API_POSTGRES_MAX_IDLE_CONNS": "5",
			}),
			wantErr: "CONSOLE_API_POSTGRES_MAX_IDLE_CONNS (5) must not exceed CONSOLE_API_POSTGRES_MAX_OPEN_CONNS (2)",
		},
		{
			name: "rejects an explicitly empty conn max lifetime",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_CONN_MAX_LIFETIME": "",
			}),
			wantErr: "CONSOLE_API_POSTGRES_CONN_MAX_LIFETIME must not be empty",
		},
		{
			name: "rejects a zero conn max lifetime",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_CONN_MAX_LIFETIME": "0s",
			}),
			wantErr: "CONSOLE_API_POSTGRES_CONN_MAX_LIFETIME must be greater than zero",
		},
		{
			name: "rejects a malformed conn max idle time",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_CONN_MAX_IDLE_TIME": "soon",
			}),
			wantErr: "CONSOLE_API_POSTGRES_CONN_MAX_IDLE_TIME must be a Go duration",
		},
		{
			name: "rejects a postgres DSN that is not a URL",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_DSN": "postgres://gateway:" + dsnPassword + "@127.0.0.1:5432/contr%zzol",
			}),
			wantErr: "CONSOLE_API_POSTGRES_DSN must be a URL",
		},
		{
			name: "rejects a postgres DSN of another scheme",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_DSN": "mysql://gateway:" + dsnPassword + "@127.0.0.1:3306/control",
			}),
			wantErr: "CONSOLE_API_POSTGRES_DSN must be a postgres:// or postgresql:// URL",
		},
		{
			name: "rejects a postgres DSN with no database in its path",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_DSN": "postgres://gateway:" + dsnPassword + "@127.0.0.1:5432/?sslmode=disable",
			}),
			wantErr: "CONSOLE_API_POSTGRES_DSN must name exactly one database in its path",
		},
		{
			name: "rejects a postgres DSN naming the other plane's database",
			env: merge(requiredEnv(), map[string]string{
				"CONSOLE_API_POSTGRES_DSN": "postgres://gateway:" + dsnPassword + "@127.0.0.1:5432/dataplane?sslmode=disable",
			}),
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
			// DeepEqual, not ==, and the change is what the payment group
			// brought: Config now carries the top-up catalogue, and a struct
			// holding a slice is not comparable — Go refuses `got != tt.want`
			// at compile time rather than answering wrongly, which is why this
			// is a one-line change and not a rewritten assertion.
			if !reflect.DeepEqual(got, tt.want) {
				// Redacted, not %#v-raw: a dumped Config carries Postgres.DSN,
				// and a test failure is CI output the whole world can read.
				t.Errorf("Load() = %s, want %s", redact(got), redact(tt.want))
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
// requiredEnv is every variable Load refuses to start without, so that a case
// written about something else does not have to repeat them — and so that a new
// required variable is one edit here rather than one per case.
//
// All three are the same kind of requirement: a process with no reachable Data
// Plane façade, no credential to reach it with, or no way to name the account a
// caller speaks for has nothing to serve, and none of the three is a value this
// process is entitled to invent. A default for the scope table in particular
// would be an access control that guesses an account, which is worse than none
// because the guess reads as an authorization.
//
// The scope table is a JSON object because a bearer token is not a value any
// other variable type carries without quoting rules a token does not obey, and
// because the alternative — a table spread over indexed variables — would let a
// deployment set the last entry and silently lose the first four.
func requiredEnv() map[string]string {
	return map[string]string{
		"CONSOLE_API_DATAPLANE_URL":        testDataplaneURL,
		"CONSOLE_API_DATAPLANE_CREDENTIAL": testDataplaneCredential,
		"CONSOLE_API_ANALYTICS_SCOPE":      testAnalyticsScope,
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

// without is merge's complement: the base with one variable REMOVED rather
// than overridden. A case about a variable that is not set at all cannot be
// written with merge — setting it to the empty string is a different refusal,
// and the two are exactly the pair the scope cases above distinguish.
func without(base map[string]string, name string) map[string]string {
	trimmed := make(map[string]string, len(base))
	for key, value := range base {
		if key == name {
			continue
		}
		trimmed[key] = value
	}
	return trimmed
}

func TestDataPlaneLogValueRedactsTheCredential(t *testing.T) {
	dataPlane := DataPlane{
		URL:                testDataplaneURL,
		Credential:         testDataplaneCredential,
		ProjectionInterval: DefaultProjectionInterval,
		ProjectionTimeout:  DefaultProjectionTimeout,
		IngestionInterval:  DefaultIngestionInterval,
		IngestionTimeout:   DefaultIngestionTimeout,
	}

	value := dataPlane.LogValue().String()
	if strings.Contains(value, testDataplaneCredential) {
		t.Errorf("LogValue() = %q, must not carry the management credential", value)
	}
	if !strings.Contains(value, "[redacted]") {
		t.Errorf("LogValue() = %q, want the redaction marker", value)
	}
	for _, want := range []string{testDataplaneURL, "projection_interval", "projection_timeout", "ingestion_interval", "ingestion_timeout"} {
		if !strings.Contains(value, want) {
			t.Errorf("LogValue() = %q, want it to name %s", value, want)
		}
	}
}

func TestAnalyticsLogValueRendersTheTableSizeAndRedactsEveryCredential(t *testing.T) {
	// Two entries, so the count is a real count rather than "there is a table".
	// The tokens here are shaped like the ones a deployment supplies and are
	// assembled from parts for the same reason the fixture credential is: a
	// literal that looks like a live token is a push-protection refusal.
	first := "console" + "-scope-" + "credential-one"
	second := "console" + "-scope-" + "credential-two"
	analytics := Analytics{Scope: map[string]string{
		first:  "018f0000-0000-7000-8000-000000000001",
		second: "018f0000-0000-7000-8000-000000000002",
	}}

	value := analytics.LogValue().String()
	for _, credential := range []string{first, second} {
		if strings.Contains(value, credential) {
			t.Errorf("LogValue() = %q, must not carry a scope credential; the table is the deployment's access control and a log line is not a place it may be read from", value)
		}
	}
	if !strings.Contains(value, "scopes=2") {
		t.Errorf("LogValue() = %q, want the table's size: an operator needs to know the surface has scopes at all, and the size is the whole of what may be said about it", value)
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

func TestTheShippedReconciliationDefaultsSatisfyTheRulesThatRefuseThem(t *testing.T) {
	// The loader refuses a schedule whose timeout is not longer than its
	// interval, and a schedule whose lookback is narrower than its interval.
	// Nothing forces Defaults() through that refusal, so the defaults are
	// checked against it here: a control plane that shipped defaults its own
	// validator rejects would fail to start on every deployment that set none
	// of the four dials — which is every deployment that trusted them.
	//
	// This is not a test that was added because the rule is right. It was
	// added because the rule was added after the defaults were, and the
	// defaults were exactly the violating shape: a one-minute pass bounded at
	// one minute, on a plane with no check that noticed.
	if err := validateReconciliation(defaultReconciliation()); err != nil {
		t.Fatalf("the shipped reconciliation defaults are rejected by their own rule: %v", err)
	}
}

func TestReconciliationLogValueNamesEveryDial(t *testing.T) {
	// The group is secret-free, and this is the only thing that keeps it from
	// being the one settings group a log line silently omits: a dial added to
	// the struct without a line here renders in no log at all.
	value := Reconciliation{
		Interval: DefaultReconciliationInterval,
		Timeout:  DefaultReconciliationTimeout,
		Batch:    DefaultReconciliationBatch,
		Lookback: DefaultReconciliationLookback,
	}.LogValue().String()

	for _, want := range []string{"interval", "timeout", "batch", "lookback"} {
		if !strings.Contains(value, want) {
			t.Errorf("LogValue() = %q, want it to name %s", value, want)
		}
	}
}

func lookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

// redact renders a Config for a test-failure message with every secret in it
// struck out. A raw %#v of the struct would quote the DSN, the management
// credential and every credential in the analytics scope — and a test failure
// is CI output the whole world can read.
//
// It walks the scope's KEYS rather than its whole rendering, because the keys
// ARE the credentials and the values are account ids that appear in the
// expectation beside them.
func redact(c Config) string {
	rendered := strings.ReplaceAll(fmt.Sprintf("%#v", c), c.Postgres.DSN, "<redacted dsn>")
	rendered = strings.ReplaceAll(rendered, c.DataPlane.Credential, "<redacted credential>")
	for credential := range c.Analytics.Scope {
		rendered = strings.ReplaceAll(rendered, credential, "<redacted scope credential>")
	}
	return rendered
}
