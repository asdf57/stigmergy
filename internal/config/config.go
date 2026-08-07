package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr        string
	EtcdEndpoints   []string
	EtcdPrefix      string
	DialTimeout     time.Duration
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	LogLevel        slog.Level
}

func Load() (Config, error) {
	config := Config{
		HTTPAddr:        envOr("HTTP_ADDR", "127.0.0.1:8080"),
		EtcdEndpoints:   splitCSV(envOr("ETCD_ENDPOINTS", "http://127.0.0.1:2379")),
		EtcdPrefix:      envOr("ETCD_PREFIX", "/homelab/v1"),
		DialTimeout:     5 * time.Second,
		RequestTimeout:  10 * time.Second,
		ShutdownTimeout: 10 * time.Second,
		LogLevel:        slog.LevelInfo,
	}

	var err error
	if config.DialTimeout, err = durationEnv("ETCD_DIAL_TIMEOUT", config.DialTimeout); err != nil {
		return Config{}, err
	}
	if config.RequestTimeout, err = durationEnv("REQUEST_TIMEOUT", config.RequestTimeout); err != nil {
		return Config{}, err
	}
	if config.ShutdownTimeout, err = durationEnv("SHUTDOWN_TIMEOUT", config.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if err := config.LogLevel.UnmarshalText([]byte(envOr("LOG_LEVEL", "info"))); err != nil {
		return Config{}, fmt.Errorf("LOG_LEVEL: %w", err)
	}
	if len(config.EtcdEndpoints) == 0 {
		return Config{}, fmt.Errorf("ETCD_ENDPOINTS must contain at least one endpoint")
	}
	if strings.Trim(config.EtcdPrefix, "/ ") == "" {
		return Config{}, fmt.Errorf("ETCD_PREFIX must identify a non-root key prefix")
	}
	return config, nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return duration, nil
}
