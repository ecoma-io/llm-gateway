package redis

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	valid := Config{
		Address:           "cache.example.test:6379",
		Database:          0,
		DialTimeout:       time.Second,
		ConnTimeout:       time.Second,
		PipelineMultiplex: 2,
		BlockingPoolSize:  1,
	}
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{
			name: "requires an address",
			edit: func(c *Config) { c.Address = "" },
			want: "address is required",
		},
		{
			name: "requires a host and port",
			edit: func(c *Config) { c.Address = "cache.example.test" },
			want: "must be host:port",
		},
		{
			name: "rejects a negative database",
			edit: func(c *Config) { c.Database = -1 },
			want: "must not be negative",
		},
		{
			name: "requires a positive dial timeout",
			edit: func(c *Config) { c.DialTimeout = 0 },
			want: "dial timeout 0s must be positive",
		},
		{
			name: "requires a positive connection timeout",
			edit: func(c *Config) { c.ConnTimeout = -time.Second },
			want: "conn timeout -1s must be positive",
		},
		{
			name: "rejects a zero connection timeout",
			edit: func(c *Config) { c.ConnTimeout = 0 },
			want: "conn timeout 0s must be positive",
		},
		{
			name: "rejects an out-of-range connection bound",
			edit: func(c *Config) { c.PipelineMultiplex = maxPipelineMultiplex + 1 },
			want: "must be between 0 and",
		},
		{
			name: "rejects an out-of-range blocking connection bound",
			edit: func(c *Config) { c.BlockingPoolSize = maxBlockingPoolSize + 1 },
			want: "blocking pool size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.edit(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("REDIS_ADDRESS", "redis.example.test:6380")
	t.Setenv("REDIS_USERNAME", "gateway")
	t.Setenv("REDIS_PASSWORD", "not-for-logs")
	t.Setenv("REDIS_DATABASE", "3")
	t.Setenv("REDIS_DIAL_TIMEOUT", "2s")
	t.Setenv("REDIS_CONN_TIMEOUT", "4s")
	t.Setenv("REDIS_PIPELINE_MULTIPLEX", "1")
	t.Setenv("REDIS_BLOCKING_POOL_SIZE", "2")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	want := Config{
		Address:           "redis.example.test:6380",
		Username:          "gateway",
		Password:          "not-for-logs",
		Database:          3,
		DialTimeout:       2 * time.Second,
		ConnTimeout:       4 * time.Second,
		PipelineMultiplex: 1,
		BlockingPoolSize:  2,
	}
	if cfg != want {
		t.Errorf("FromEnv() = %#v, want %#v", cfg, want)
	}
}

func TestFromEnvDefaults(t *testing.T) {
	for _, name := range []string{
		"REDIS_ADDRESS",
		"REDIS_USERNAME",
		"REDIS_PASSWORD",
		"REDIS_DATABASE",
		"REDIS_DIAL_TIMEOUT",
		"REDIS_CONN_TIMEOUT",
		"REDIS_PIPELINE_MULTIPLEX",
		"REDIS_BLOCKING_POOL_SIZE",
	} {
		t.Setenv(name, "")
	}

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	want := Config{
		Address:           defaultAddress,
		Database:          0,
		DialTimeout:       defaultDialTimeout,
		ConnTimeout:       defaultConnTimeout,
		PipelineMultiplex: defaultPipelineMultiplex,
		BlockingPoolSize:  defaultBlockingPoolSize,
	}
	if cfg != want {
		t.Errorf("FromEnv() = %#v, want %#v", cfg, want)
	}
}

func TestFromEnvRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{
			name:  "rejects a non-integer database",
			key:   "REDIS_DATABASE",
			value: "one",
			want:  "REDIS_DATABASE must be an integer",
		},
		{
			name:  "rejects a malformed dial timeout",
			key:   "REDIS_DIAL_TIMEOUT",
			value: "soon",
			want:  "REDIS_DIAL_TIMEOUT must be a duration",
		},
		{
			name:  "rejects a malformed connection timeout",
			key:   "REDIS_CONN_TIMEOUT",
			value: "later",
			want:  "REDIS_CONN_TIMEOUT must be a duration",
		},
		{
			name:  "rejects a non-integer connection bound",
			key:   "REDIS_PIPELINE_MULTIPLEX",
			value: "a few",
			want:  "REDIS_PIPELINE_MULTIPLEX must be an integer",
		},
		{
			name:  "rejects a non-integer blocking connection bound",
			key:   "REDIS_BLOCKING_POOL_SIZE",
			value: "some",
			want:  "REDIS_BLOCKING_POOL_SIZE must be an integer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			_, err := FromEnv()
			if err == nil {
				t.Fatal("FromEnv() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("FromEnv() error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestConfigLogValueRedactsPassword(t *testing.T) {
	cfg := Config{
		Address:  "redis.example.test:6379",
		Username: "gateway",
		Password: "super-secret",
	}
	value := cfg.LogValue().String()
	if strings.Contains(value, cfg.Password) {
		t.Errorf("LogValue() = %q, must not contain the password", value)
	}
	if strings.Contains(value, cfg.Username) {
		t.Errorf("LogValue() = %q, must not contain the username", value)
	}
	if !strings.Contains(value, "[redacted]") {
		t.Errorf("LogValue() = %q, want redaction marker", value)
	}
}

func TestConfigValidatePreservesUnderlyingAddressError(t *testing.T) {
	cfg := Config{
		Address:           "[::1",
		DialTimeout:       time.Second,
		ConnTimeout:       time.Second,
		PipelineMultiplex: 2,
		BlockingPoolSize:  1,
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
	var addrErr *net.AddrError
	if !errors.As(err, &addrErr) {
		t.Errorf("Validate() error = %T (%v), want wrapped *net.AddrError", err, err)
	}
}
