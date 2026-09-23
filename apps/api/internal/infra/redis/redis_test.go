package redis

import (
	"testing"
	"time"
)

func TestConfigClientOption(t *testing.T) {
	cfg := Config{
		Address:           "cache.example.test:6379",
		Username:          "gateway",
		Password:          "not-for-logs",
		Database:          5,
		DialTimeout:       time.Second,
		ConnTimeout:       7 * time.Second,
		PipelineMultiplex: 2,
		BlockingPoolSize:  3,
	}

	option := cfg.clientOption()

	if len(option.InitAddress) != 1 || option.InitAddress[0] != cfg.Address {
		t.Errorf("InitAddress = %v, want [%s]", option.InitAddress, cfg.Address)
	}
	if option.Username != cfg.Username {
		t.Errorf("Username = %q, want %q", option.Username, cfg.Username)
	}
	if option.Password != cfg.Password {
		t.Errorf("Password = %q, want %q", option.Password, cfg.Password)
	}
	if option.SelectDB != cfg.Database {
		t.Errorf("SelectDB = %d, want %d", option.SelectDB, cfg.Database)
	}
	if option.Dialer.Timeout != cfg.DialTimeout {
		t.Errorf("Dialer.Timeout = %s, want %s", option.Dialer.Timeout, cfg.DialTimeout)
	}
	if option.ConnWriteTimeout != cfg.ConnTimeout {
		t.Errorf("ConnWriteTimeout = %s, want the ConnTimeout %s passed through unchanged", option.ConnWriteTimeout, cfg.ConnTimeout)
	}
	if option.PipelineMultiplex != cfg.PipelineMultiplex {
		t.Errorf("PipelineMultiplex = %d, want %d", option.PipelineMultiplex, cfg.PipelineMultiplex)
	}
	if option.BlockingPoolSize != cfg.BlockingPoolSize {
		t.Errorf("BlockingPoolSize = %d, want %d", option.BlockingPoolSize, cfg.BlockingPoolSize)
	}
	if !option.DisableCache {
		t.Error("DisableCache = false, want true: the client-side response cache stays off")
	}
}
