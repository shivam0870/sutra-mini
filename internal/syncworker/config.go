package syncworker

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// ConfigFromEnv rejects malformed values instead of silently using defaults.
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()
	if value := os.Getenv("ACCOUNTING_URL"); value != "" {
		cfg.AccountingURL = value
	}
	for _, setting := range []struct {
		name string
		dest *int
	}{
		{"WORKER_CONCURRENCY", &cfg.Concurrency},
		{"WORKER_MAX_ATTEMPTS", &cfg.MaxAttempts},
	} {
		if value := os.Getenv(setting.name); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return cfg, fmt.Errorf("%s: %w", setting.name, err)
			}
			*setting.dest = parsed
		}
	}
	for _, setting := range []struct {
		name string
		dest *time.Duration
	}{
		{"WORKER_HTTP_TIMEOUT", &cfg.HTTPTimeout},
		{"WORKER_LEASE_DURATION", &cfg.LeaseDuration},
		{"WORKER_POLL_INTERVAL", &cfg.PollInterval},
		{"WORKER_BASE_BACKOFF", &cfg.BaseBackoff},
		{"WORKER_MAX_BACKOFF", &cfg.MaxBackoff},
	} {
		if value := os.Getenv(setting.name); value != "" {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return cfg, fmt.Errorf("%s: %w", setting.name, err)
			}
			*setting.dest = parsed
		}
	}
	return cfg, nil
}
