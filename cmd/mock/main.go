package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sutra/internal/db"
	"sutra/internal/mockaccounting"
)

func main() {
	if err := run(); err != nil {
		slog.Error("mock accounting stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := db.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	cfg := mockaccounting.ConfigFromEnv()
	mock, err := mockaccounting.New(pool, cfg, slog.Default())
	if err != nil {
		return err
	}
	addr := os.Getenv("MOCK_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	server := &http.Server{
		Addr: addr, Handler: mock.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("mock HTTP shutdown", "error", err)
		}
	}()
	slog.Info("mock accounting listening", "address", addr, "mode", cfg.Mode)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}
