// Command api is the producer: a REST service that accepts and tracks jobs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/mahalingam-dev-8/gotaskqueue/internal/api"
	"github.com/mahalingam-dev-8/gotaskqueue/internal/auth"
	"github.com/mahalingam-dev-8/gotaskqueue/internal/config"
	"github.com/mahalingam-dev-8/gotaskqueue/internal/db"
	"github.com/mahalingam-dev-8/gotaskqueue/internal/logging"
	"github.com/mahalingam-dev-8/gotaskqueue/internal/queue"
	"github.com/mahalingam-dev-8/gotaskqueue/migrations"
)

// main stays tiny and run() returns an error, so every resource below can be
// released with defer. os.Exit skips deferred calls, so it must happen in exactly
// one place — here.
func main() {
	if err := run(); err != nil {
		slog.Error("api exited with error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadAPI()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := logging.NewWithService(cfg.Log.Level, cfg.Log.Format, "api")

	// NotifyContext gives us a context that is cancelled on SIGINT/SIGTERM — ECS
	// sends SIGTERM, waits for stopTimeout, then SIGKILLs. This is the one signal
	// handler the process needs.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DB)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	log.Info("database connected", slog.Int("max_conns", cfg.DB.MaxConns))

	if cfg.DB.RunMigrations {
		if err := db.Migrate(ctx, pool, migrations.FS, log); err != nil {
			return fmt.Errorf("run migrations: %w", err)
		}
	}

	server := api.New(cfg, queue.New(pool), pool, auth.NewManager(cfg.JWT), log)

	// ListenAndServe blocks, so it runs on its own goroutine and reports back over a
	// buffered channel. Buffered (cap 1) so the goroutine can exit even if nobody
	// ever reads — an unbuffered send would leak it.
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Start()
	}()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
	case <-ctx.Done():
		log.Info("shutdown signal received", slog.Duration("timeout", cfg.HTTP.ShutdownTimeout))

		// Fresh context: ctx is already cancelled, and Shutdown needs a live
		// deadline to drain in-flight requests against.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
	}

	log.Info("api stopped")
	return nil
}
