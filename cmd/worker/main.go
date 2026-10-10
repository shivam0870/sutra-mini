package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"sutra/internal/db"
	"sutra/internal/syncworker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := syncworker.ConfigFromEnv()
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	worker, err := syncworker.New(pool, cfg, slog.Default())
	if err != nil {
		return err
	}
	slog.Info("worker started", "concurrency", cfg.Concurrency, "http_timeout", cfg.HTTPTimeout, "lease_duration", cfg.LeaseDuration)
	return worker.Run(ctx)
}
