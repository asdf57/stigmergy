package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("ETCD_ENDPOINTS", "")
	t.Setenv("ETCD_DIAL_TIMEOUT", "")
	t.Setenv("REQUEST_TIMEOUT", "")
	t.Setenv("SHUTDOWN_TIMEOUT", "")
	t.Setenv("LOG_LEVEL", "")

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.HTTPAddr != "127.0.0.1:8080" {
		t.Fatalf("HTTPAddr = %q", config.HTTPAddr)
	}
	if config.RequestTimeout != 10*time.Second {
		t.Fatalf("RequestTimeout = %s", config.RequestTimeout)
	}
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	t.Setenv("REQUEST_TIMEOUT", "eventually")
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want invalid duration")
	}
}
